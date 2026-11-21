# Red-team record: `limen account` (use, clear, swap, shell hook)

Date: 2026-11-21.
Run by: a separate Claude Code session on the owner's instruction, not the author of the feature (kavanara-secured, "Red-teaming", step 2).
Scope: `account.go`, the account parts of `main.go` (`limen shell` outside a context), `render.go` (`Prompt`, `RenderShell`, `shellQuote`), the hook text `zshHook`/`bashHook` in `commands.go`, README section "Switching the Claude account".
Code: branch `master`, commit `f3b210e` (merge of PR #4, `feat/account-switch`).
Method: the binary was built with `go build` and run with a throwaway `HOME` and `XDG_STATE_HOME` in a scratch directory; fake config dirs `~/.claude-a` and `~/.claude-b` held hand-made `projects/<slug>/<id>.jsonl` files. The hook was evaluated in `zsh -c` and `bash -c` with the scratch binary first on `PATH`. The real `~/.claude*` directories, the real state directory and the owner's shell config were not touched. The `claude` CLI was not run; no network. No source or test file was changed. Paths below are shortened to `$S` (the scratch directory) and `<slug>`.

## Threat model (STRIDE, short)

What is protected:

- The owner's shell: the hook `eval`s the output of `limen shell` on every directory change (bash: every prompt).
- Account separation: transcripts, logins and settings of different Claude accounts (different organisations and customers, e.g. turbogrün and KavanaraOrg) must not mix by accident or by attack.
- Integrity of transcripts in the target account. A transcript is later read by an agent with tool access on `claude --resume`, so a planted transcript is a prompt injection.
- Files outside the config directories, which the copy must neither read nor write.

Who attacks:

- A cloned repository the owner `cd`s into. Its committed `.limen/limen.yaml` is read by `Discover`, and its `claudeConfigDir` becomes `CLAUDE_CONFIG_DIR`.
- Something that writes the state file `$XDG_STATE_HOME/limen/account` or the `projects/` folders: another tool, an agent session running as the owner, a misconfigured shared `XDG_STATE_HOME`.
- Claude Code or an agent inside a session that controls transcript file names and the contents of its own `projects/<slug>/`.
- The owner by mistake: typos, a failed command, two shells.

Entry points: the arguments of `account use|swap` (name, path, `--session`); the content of the state file; the `CLAUDE_CONFIG_DIR` environment value (swap source); the `claudeConfigDir` of the nearest `.limen/limen.yaml`; file names, symlinks and folders under `<src>/projects/<slug>/` and `<dst>/projects/<slug>/`; the hook wrapper `limen()`.

| STRIDE | Threat considered |
|---|---|
| Spoofing | the state file or a repository descriptor points `CLAUDE_CONFIG_DIR` at a directory the owner did not choose (a different login, someone else's settings) |
| Tampering | swap overwrites transcripts in the target account; a symlink or temp name redirects a write outside the target |
| Repudiation | not relevant (local single-user tool) |
| Information disclosure | swap copies files that are not this session's transcript (other projects, foreign files via symlink) into another account |
| Denial of service | a failed command or stale environment drops the shell onto the wrong or the default account |
| Elevation of privilege | state file or descriptor content reaching `eval`; a printed command line that runs extra commands when pasted |

## Attacks tried

### State file and `limen shell` into `eval`

| # | Input | Observed | Verdict |
|---|---|---|---|
| A1 | `limen account use "$S/x'; touch $S/PWNED1; '"` (directory exists), then `eval "$(limen shell)"` outside a project | exported as `'…x'\''; touch …; '\'''`; no file created; `CLAUDE_CONFIG_DIR` holds the literal string | held |
| A2 | state file `$(touch PWNED2)'` + backtick `touch PWNED3` + `'; touch PWNED4 #` | single-quoted, no file created (zsh) | held |
| A3 | state file with embedded newlines and `'` (`/a'⏎b⏎touch PWNED5`) | stays inside one single-quoted word; no file created | held |
| A4 | state file `relative/dir` | exported as `CLAUDE_CONFIG_DIR='relative/dir'`; `limen account` warns "does not exist", exit 0 | note, see F-9 |
| C6 | state file `/tmp/ESC]0;X BEL %F{red}%n` inside a context | raw ESC/BEL and zsh `%` sequences land in `limen prompt`, `LIMEN_SEGMENT` and `limen account` output | note, see F-9 |
| C3 | `account.tmp` pre-planted as a symlink to `$S/outside/v2`, then `limen account use b` | `v2` overwritten with the path; afterwards `account` itself is that symlink | finding F-4 |
| C4 | file modes after `use` | dir `0755`, file `0644` | note, see F-12 |
| C5 | `limen account use "$S/home/acc "` (trailing space; the directory with the space exists) | accepted, saved, loaded with `TrimSpace` as `$S/home/acc`, which does not exist; exported anyway | note, see F-10 |

### Account names and paths (`use`, `swap` target)

| # | Input | Observed | Verdict |
|---|---|---|---|
| T1 | `../../etc`, `x/../y`, `.claude-a`, `b⏎`, `b ` | "invalid account name" | held |
| T2 | `-rf` | "unknown flag" | held |
| T3 | `~root` | "is not absolute" | held |
| T4 | `a..b` | maps to `~/.claude-a..b`, refused as missing | held |
| T5 | `/`, `~`, `~/../../..` with `use` and with `swap -n` | accepted: any existing directory becomes the account, including `/` and `$HOME` | note, see F-10 |
| T6 | `swap -- b` | "unknown flag --" | held (no `--` support, harmless) |

### Session ids

| # | Input | Observed | Verdict |
|---|---|---|---|
| S1 | `--session ..`, `.`, `../x`, `a/b`, `a\b`, `--session=..` | "invalid session id" | held |
| S2 | `--session -x`, `....`, `s⏎`, `s ` | "no session … in …" | held |
| S3 | `--session s.jsonl`, `--session .jsonl` | `.jsonl` stripped; `.jsonl` becomes the empty id and silently means "latest" | note, see F-11 |
| S4 | `--session ""` | silently means "latest" | note, see F-11 |
| B1 | no `--session`; the newest file in the source folder is `...jsonl` (id `..`); target holds a newer `-other-proj/o.jsonl` | id `..` accepted; `sideDir` becomes `<src>/projects`; **every project folder of account a was copied into account b, and b's newer `o.jsonl` was overwritten**, without `--force`; output "copied ...jsonl and ../" | **finding F-1** |
| B2 | no `--session`; the newest file is `..jsonl` (id `.`); target holds a newer `s1.jsonl` | **all transcripts of the source project folder copied into the target folder; the newer `s1.jsonl` overwritten** | **finding F-1** |
| C1 | no `--session`; newest file `x; touch PWNED<ESC>]0;HACKED<BEL>.jsonl` | printed `claude --resume x; touch PWNED…` unquoted, raw ESC to the terminal | finding F-8 |
| C2 | newest file name with a newline: `a⏎rm -rf ~.jsonl` | printed resume line splits into `claude --resume a` and a separate line `rm -rf ~` | finding F-8 |

### Symlinks and temp files in `projects/`

| # | Input | Observed | Verdict |
|---|---|---|---|
| B3 | `<src>/projects/<slug>/leak.jsonl` is a symlink to `$S/secret` | picked as the newest session; **the secret's content copied as a regular file into account b** | finding F-3 |
| B4 | side folder `<src>/…/s` is a symlink to a directory | nothing copied from it (WalkDir lstat's the root), but the output still says "copied s.jsonl and s/" | held; note F-11 |
| B5 | a symlink `s/link.txt` inside a real side folder | skipped, `real.txt` copied | held |
| B6 | `<dst>/projects/<slug>` is a symlink to `$S/outside` | transcript written to `$S/outside/s.jsonl` | finding F-5 |
| B7 | `<dst>/projects/<slug>/s.jsonl.limen-tmp` pre-planted as a symlink to `$S/outside/victim` | **`victim` overwritten with the transcript**; after the rename `s.jsonl` is the symlink | finding F-4 |
| B7b | `<dst>/…/s.jsonl` itself is a symlink to a newer file | refused "newer copy"; with `--force` the rename replaces the link, not the target (by reading the code) | held |

### Swap source and repository content

| # | Input | Observed | Verdict |
|---|---|---|---|
| R1 | cloned repo with a committed `.limen/limen.yaml`: `claudeConfigDir: ./.cfg` | `limen shell` exports `CLAUDE_CONFIG_DIR='./.cfg'`, a path relative to wherever the shell is | finding F-2 (pre-existing part, see below) |
| R2 | same repo; `.cfg/projects/<repo-slug>/...jsonl` and `.cfg/projects/-Users-victim-secretproj/v.jsonl` with `INJECTED`; account b holds a newer `-Users-victim-secretproj/v.jsonl`; owner runs `limen account swap b` in the repo | source taken as `./.cfg`; **b's transcript of an unrelated project replaced by the repository's `INJECTED` line**, no `--force` | **finding F-2** (via F-1) |
| R3 | same repo, but an override is set (`use a`) | override wins, `CLAUDE_CONFIG_DIR` is account a | held |

### Shell hook wrapper (zsh and bash, outside a project, `CLAUDE_CONFIG_DIR=~/.claude-a` exported by the user)

| # | Input | Observed | Verdict |
|---|---|---|---|
| H1 | `limen account use typo` (fails, rc 1) | `CLAUDE_CONFIG_DIR` unset; rc 1 returned | finding F-6 |
| H2 | `limen account swap b --dry-run` | `CLAUDE_CONFIG_DIR` unset although "nothing changed" | finding F-6 |
| H3 | `use b`, then `clear` | `clear` leaves `CLAUDE_CONFIG_DIR` unset (Claude Code falls back to `~/.claude`), not the user's earlier value | finding F-6 |
| H4 | `use b`; another shell removes the override; this shell `cd`s | `CLAUDE_CONFIG_DIR` stays `~/.claude-b` | finding F-7 |
| H5 | `type limen` | a shell function shadows the binary in interactive shells; `command limen` and scripts are unaffected; return code preserved | held |

## Findings

### F-1 (medium): an auto-picked session id is not validated; `.` and `..` turn the side-folder copy into a bulk copy that overwrites newer transcripts

`pickSession` validates an id given with `--session`, but not the id it derives from the newest `*.jsonl` name. A file named `...jsonl` gives id `..`, so `sideDir = <src>/projects/<slug>/..` = `<src>/projects`, and `copyTranscriptTree` copies every project of the source account into `<dst>/projects` (B1). `..jsonl` gives id `.` and copies the whole project folder (B2). `copyTranscriptTree` overwrites every existing file unconditionally, so the "newer copy, use --force" check protects only the one `<id>.jsonl`. Effect: transcripts of other projects cross accounts (customer separation), and newer work in the target is lost.

Reproduce: `echo x > ~/.claude-a/projects/<slug>/...jsonl; CLAUDE_CONFIG_DIR=~/.claude-a limen account swap b`.

Fix: validate every id, auto-picked or given, against one strict pattern (Claude Code session ids are UUIDs: `^[A-Za-z0-9][A-Za-z0-9_-]*$`), and skip non-matching files when picking the newest. In `copyTranscriptTree` refuse to overwrite an existing file that is newer unless `--force`, as for the main file. Regression tests: auto-pick with `...jsonl` and `..jsonl` present.

### F-2 (medium): the swap source can be a directory a cloned repository controls, which lets the repository write into the owner's real account

`swapSource` trusts `CLAUDE_CONFIG_DIR` and then the context, and neither is checked: a committed `.limen/limen.yaml` with `claudeConfigDir: ./.cfg` makes the hook export a relative, repository-controlled config directory (R1), and `swap` then copies from it into the owner's account. Combined with F-1 the repository replaced a newer transcript of an unrelated project in account b with its own content (R2). On `claude --resume` there, an agent with tools reads planted content (prompt injection).

Pre-existing part, outside this feature but on its path: a repository-controlled `CLAUDE_CONFIG_DIR` is itself dangerous, because Claude Code takes its user settings (including hooks) from that directory. This was not verified by running `claude`. It should be fixed with this finding.

Reproduce: see R2.

Fix: (1) In context loading, reject a `claudeConfigDir` that is relative after tilde expansion, and only honour one that lies outside the repository root. (2) In `swapSource`, run the source through the same checks as a target (absolute, an existing directory, ideally under `$HOME` as `.claude` or `.claude-*`, otherwise require an explicit `--from <path>`). Print the source before copying (it is printed, but only after the decision).

### F-3 (low): a symlinked `<id>.jsonl` in the source is followed

`pickSession` and `copyTranscriptFile` use `os.ReadFile`/`os.Stat`, which follow links. A link `leak.jsonl -> ~/secret` was picked and the secret's bytes copied into the other account as a regular file (B3). The doc comment of `copyTranscriptTree` promises that links cannot pull foreign files along; that holds for the side folder only.

Fix: `os.Lstat` the source and require `Mode().IsRegular()` for the transcript, skip non-regular files when picking the newest, and open with `O_NOFOLLOW`.

### F-4 (low): predictable temp names are written through symlinks

`<id>.jsonl.limen-tmp` (B7) and `account.tmp` (C3) are opened with `os.WriteFile`, which follows a pre-planted symlink and truncates its target; the rename then leaves the link in place of the real file. Precondition: write access to the target `projects/` folder or the state directory (same user, another tool or agent).

Fix: create the temp file with `os.CreateTemp(dir, ".limen-*")` (O_EXCL, random name), write, `Chmod`/`Chtimes`, then rename; remove it on every error path.

### F-5 (low): a symlinked target project folder redirects the write

`<dst>/projects/<slug>` as a symlink to `$S/outside` made the transcript land outside the target account (B6). `MkdirAll` and the writes follow links at every level of `dst`.

Fix: `Lstat` `<dst>/projects` and `<dst>/projects/<slug>` and refuse when either is a symlink (or resolve with `EvalSymlinks` and require the result to stay under the resolved `dst`).

### F-6 (medium): the hook wrapper drops the shell's account on failure and on dry runs

The wrapper runs `unset CLAUDE_CONFIG_DIR; _limen_apply` for `account use|clear|swap` regardless of the exit code or `--dry-run` (H1, H2). Outside a project with no override, nothing re-exports, so the shell silently falls back to `~/.claude`, the default account. Every `claude` started there afterwards runs under a different login than before, by a typo. `clear` has the same effect on a user-exported value (H3). This is the account mix-up the feature exists to prevent.

Fix: re-apply only when `rc == 0` and the command was not `--dry-run`/`-n`. Do not `unset` a value limen did not set: have `limen shell` export a marker such as `LIMEN_CLAUDE_CONFIG_DIR` with the value it set, and unset `CLAUDE_CONFIG_DIR` only when it still equals that marker. Regression test in `TestHookReappliesAfterAnAccountCommand` with a failing `use`.

### F-7 (low): a cleared override lingers in other shells

`_limen_apply` unsets the `LIMEN_*` variables but never `CLAUDE_CONFIG_DIR`, so once the override is cleared elsewhere, or a shell leaves the project, the old account stays in force (H4). zsh re-applies only on `chpwd`, so even a project's own value comes back only after a `cd`.

Fix: the same marker as F-6: when `limen shell` emits no `CLAUDE_CONFIG_DIR` but the marker is set, emit `unset CLAUDE_CONFIG_DIR LIMEN_CLAUDE_CONFIG_DIR`.

### F-8 (low): the printed resume commands are not quoted

`claude --resume %s` prints the id raw, in both lines, as does `resume from %s`. An id taken from a file name with `;`, a newline or terminal escapes becomes extra commands when the owner pastes the line (C1, C2), and raw ESC sequences reach the terminal.

Fix: F-1's id pattern removes the cause; additionally print `shellQuote(id)` and `shellQuote(workDir)`, and strip control characters from every path shown.

### F-9 (note): the override is not validated when it is loaded

`loadAccountOverride` accepts any content: relative paths, missing directories, control characters and zsh `%` sequences (A4, C6). Quoting into `eval` held in every attempt (A1 to A3), so this is no injection, but raw ESC/BEL reach the terminal through the prompt segment and `limen account`, and a relative value means a different directory in every working directory.

Fix: on load accept only an absolute, existing directory without control characters; otherwise ignore it in `limen shell` and warn in `limen account`. Escape `%` as `%%` in `LIMEN_SEGMENT`, or document that the segment is literal.

### F-10 (note): `use` accepts any directory and validates a different path than it stores

`/`, `$HOME` and `~/../../..` are accepted as accounts (T5); with `swap`, `/` would mean writing `/projects/…`. A path with trailing whitespace is checked as given and then saved and loaded trimmed, i.e. a different, missing directory (C5).

Fix: reject leading/trailing whitespace and control characters in a path; check what will be stored; warn (or refuse without `--force`) when the directory has no sign of being a Claude config directory (no `.claude.json`, `settings.json` or `projects/`).

### F-11 (note): silent fallbacks in swap

`--session ""` and `--session .jsonl` mean "latest" (S3, S4); a symlinked side folder is skipped while the output still says it was copied (B4).

Fix: an empty id after stripping is a usage error; report what was skipped.

### F-12 (note): state modes

The state directory is `0755` and the override file `0644` (C4): other local users can read which account is in use. Low value, but the transcript copies already use `0700` directories.

Fix: `0700`/`0600` for the state directory and file.

## Resolution (2026-11-21, branch `fix/account-red-team`)

All twelve findings were fixed by the author after this pass. Each has a regression test in `account_test.go` that failed before the fix (ADR-0002); none is accepted.

| Finding | Fix | Regression test |
|---|---|---|
| F-1 | Session ids must match `^[A-Za-z0-9][A-Za-z0-9_-]*$`, both for `--session` and when picking the newest; every file of the side folder is held to the newer-copy check before anything is written | `TestSwapIgnoresTranscriptsWithInvalidIds`, `TestSwapRefusesANewerSideFileWithoutForce` |
| F-2 | A relative or in-project `claudeConfigDir` is dropped and reported in `show` (`!!`), never exported. Without `--from`, the swap source must be `~/.claude` or `~/.claude-*`; anything else must be named with `--from <account>` | `TestContextRejectsARelativeOrInRepoClaudeDir`, `TestSwapRejectsAnUnconventionalSource` |
| F-3 | Only regular files are picked (`Lstat`); the copy opens the source with `O_NOFOLLOW` | `TestSwapDoesNotFollowASymlinkedTranscript` |
| F-4 | Every write goes through `os.CreateTemp` in the target directory plus rename | `TestWritesDoNotFollowAPlantedTempSymlink` |
| F-5 | `projects/`, the slug folder and every side-folder directory in the target are refused when they are symlinks, checked before the first write | `TestSwapRefusesASymlinkedTargetFolder` |
| F-6 | The hook wrapper re-applies only on exit 0 and no longer unsets `CLAUDE_CONFIG_DIR` itself | `TestHookWrapperKeepsTheAccountOnFailureAndDryRun` (runs the real bash hook) |
| F-7 | An active override is exported with the marker `LIMEN_ACCOUNT_OVERRIDE`; with no override, `limen shell` unsets the marker and, when it still holds the marked value and no project sets one, `CLAUDE_CONFIG_DIR` | `TestShellUnsetsAnOverrideThatWasCleared` |
| F-8 | The resume hint is `cd '<dir>' && claude --resume <id>` with the directory shell-quoted; every path printed by `account` passes through a control-character filter | `TestSwapPrintsAQuotedPrintableResume` |
| F-9 | The override is checked on load (absolute, clean, no control or `%` characters, an existing Claude config directory); an invalid one is ignored and reported by `limen account` | `TestABrokenOverrideIsIgnoredAndReported` |
| F-10 | Names with surrounding whitespace, `/`, `$HOME` and directories without `.claude.json`, `settings.json` or `projects/` outside the conventional places are refused; the checked path is the stored path | `TestUseRefusesDirectoriesThatAreNoAccount` |
| F-11 | An empty `--session` (also `--session=.jsonl`) is an error; skipped side-folder entries are listed in the output | `TestSwapRejectsAnEmptySessionId` |
| F-12 | Override file `0600`, state directory `0700` | `TestWritesDoNotFollowAPlantedTempSymlink` |

Still open from "Not tested": the race between planning and copying is narrowed (`O_NOFOLLOW`, temp files) but not closed for directories swapped in between; large transcripts are still read whole. Both need local write access as the same user.

## Not tested

- Running `claude` (whether a planted transcript or a repository-controlled config directory actually leads to code execution); judged from Claude Code's documented settings location only.
- Races between the stat and the copy (TOCTOU), and two swaps at once.
- Very large transcripts (`os.ReadFile` reads the whole file into memory).
- Linux, fish, Windows paths; a shared or world-writable `XDG_STATE_HOME`.
- `limen account list` with hostile directory names in `$HOME` (only by reading: names are printed raw, same class as F-8).

## Summary

12 findings: 0 high, 3 medium (F-1, F-2, F-6), 5 low (F-3, F-4, F-5, F-7, F-8), 4 notes (F-9 to F-12). The shell-quoting boundary of the state file into `eval` held against every injection tried. Each fix needs a regression test (ADR-0002) before the release.
