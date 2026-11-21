package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// accountHome lays out a fake home with the Claude config directories a, b and
// the default one, and returns it with the environment that points limen at it.
// XDG_STATE_HOME is shared across calls, because the override written by one
// call is what the next call must read.
func accountHome(t *testing.T) (home string, env []string) {
	t.Helper()
	home = tempDir(t)
	for _, d := range []string{".claude", ".claude-a", ".claude-b"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A file named like a config directory is a backup, not an account.
	write(t, filepath.Join(home, ".claude-old-backup.tar.gz"), "x")
	return home, []string{"HOME=" + home, "XDG_STATE_HOME=" + filepath.Join(home, "state")}
}

// accountProject is a project inside the fake home whose descriptor names
// account a.
func accountProject(t *testing.T, home string) string {
	t.Helper()
	root := filepath.Join(home, "work", "proj")
	write(t, filepath.Join(root, ".limen", "limen.yaml"), "label: proj\nclaudeConfigDir: ~/.claude-a\n")
	return root
}

func mustRun(t *testing.T, dir string, env []string, args ...string) result {
	t.Helper()
	r := runLimen(t, dir, env, args...)
	if r.code != 0 {
		t.Fatalf("limen %v: exit %d\nstdout: %s\nstderr: %s", args, r.code, r.stdout, r.stderr)
	}
	return r
}

func TestAccountUseOverridesTheProjectClaudeDir(t *testing.T) {
	home, env := accountHome(t)
	root := accountProject(t, home)
	b := filepath.Join(home, ".claude-b")

	mustRun(t, root, env, "account", "use", "b")

	if r := mustRun(t, root, env, "shell"); !strings.Contains(r.stdout, "export CLAUDE_CONFIG_DIR='"+b+"'") {
		t.Errorf("shell should export the override, got:\n%s", r.stdout)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(mustRun(t, root, env, "json").stdout), &v); err != nil {
		t.Fatal(err)
	}
	if v["claude_config_dir"] != b {
		t.Errorf("json claude_config_dir = %v, want %s", v["claude_config_dir"], b)
	}
	if v["account_override"] != b {
		t.Errorf("json account_override = %v, want %s", v["account_override"], b)
	}
	if v["project_claude_config_dir"] != filepath.Join(home, ".claude-a") {
		t.Errorf("json project_claude_config_dir = %v", v["project_claude_config_dir"])
	}
	show := mustRun(t, root, env, "show").stdout
	if !strings.Contains(show, b) || !strings.Contains(show, "override") {
		t.Errorf("show should name the override:\n%s", show)
	}
	if p := mustRun(t, root, env, "prompt").stdout; !strings.Contains(p, "acct:b") {
		t.Errorf("prompt should carry the overriding account, got %q", p)
	}
}

func TestAccountStatusNamesActiveAccountAndSource(t *testing.T) {
	home, env := accountHome(t)
	root := accountProject(t, home)

	r := mustRun(t, root, env, "account")
	if !strings.Contains(r.stdout, "a") || !strings.Contains(r.stdout, "project") {
		t.Errorf("without override the project's account should be active:\n%s", r.stdout)
	}
	mustRun(t, root, env, "account", "use", "b")
	r = mustRun(t, root, env, "account")
	if !strings.Contains(r.stdout, "b") || !strings.Contains(r.stdout, "override") {
		t.Errorf("after use b the override should be reported:\n%s", r.stdout)
	}
}

func TestAccountUseRefusesAnUnknownAccount(t *testing.T) {
	home, env := accountHome(t)
	root := accountProject(t, home)

	r := runLimen(t, root, env, "account", "use", "nope")
	if r.code == 0 {
		t.Fatal("an account without a config directory must be refused")
	}
	if !strings.Contains(r.stderr, ".claude-nope") {
		t.Errorf("the error should name the missing directory: %s", r.stderr)
	}
	if _, err := os.Stat(filepath.Join(home, "state", "limen", "account")); !os.IsNotExist(err) {
		t.Error("a refused use must not write the override")
	}
	if r := runLimen(t, root, env, "account", "use", "../x"); r.code == 0 {
		t.Error("a name with a path separator that is not a path must be refused")
	}
}

func TestAccountUseAcceptsDefaultAndAPath(t *testing.T) {
	home, env := accountHome(t)
	root := accountProject(t, home)

	mustRun(t, root, env, "account", "use", "default")
	if r := mustRun(t, root, env, "shell"); !strings.Contains(r.stdout, "CLAUDE_CONFIG_DIR='"+filepath.Join(home, ".claude")+"'") {
		t.Errorf("default should be ~/.claude:\n%s", r.stdout)
	}
	mustRun(t, root, env, "account", "use", "~/.claude-b")
	if r := mustRun(t, root, env, "shell"); !strings.Contains(r.stdout, "CLAUDE_CONFIG_DIR='"+filepath.Join(home, ".claude-b")+"'") {
		t.Errorf("a tilde path should resolve:\n%s", r.stdout)
	}
}

func TestAccountClearRestoresTheProjectValue(t *testing.T) {
	home, env := accountHome(t)
	root := accountProject(t, home)

	mustRun(t, root, env, "account", "use", "b")
	mustRun(t, root, env, "account", "clear")
	if r := mustRun(t, root, env, "shell"); !strings.Contains(r.stdout, "CLAUDE_CONFIG_DIR='"+filepath.Join(home, ".claude-a")+"'") {
		t.Errorf("after clear the project's account applies again:\n%s", r.stdout)
	}
	// Clearing twice is not an error: the state asked for is the state reached.
	mustRun(t, root, env, "account", "clear")
}

func TestAccountOverrideAppliesOutsideAContext(t *testing.T) {
	home, env := accountHome(t)
	dir := filepath.Join(home, "elsewhere")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if r := mustRun(t, dir, env, "shell"); r.stdout != "" {
		t.Fatalf("without override and context shell stays silent, got %q", r.stdout)
	}
	mustRun(t, dir, env, "account", "use", "b")
	r := mustRun(t, dir, env, "shell")
	want := "export CLAUDE_CONFIG_DIR='" + filepath.Join(home, ".claude-b") + "'\n"
	if r.stdout != want {
		t.Errorf("outside a context only the account is exported:\ngot  %q\nwant %q", r.stdout, want)
	}
}

func TestAccountListNamesEveryConfigDir(t *testing.T) {
	home, env := accountHome(t)
	root := accountProject(t, home)
	mustRun(t, root, env, "account", "use", "b")

	r := mustRun(t, root, env, "account", "list")
	for _, want := range []string{"default", "a", "b"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("list missing %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "backup") {
		t.Errorf("a file is not an account:\n%s", r.stdout)
	}

	var list []struct {
		Name   string `json:"name"`
		Dir    string `json:"dir"`
		Active bool   `json:"active"`
	}
	if err := json.Unmarshal([]byte(mustRun(t, root, env, "account", "list", "--json").stdout), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 accounts, got %+v", list)
	}
	for _, a := range list {
		if a.Active != (a.Name == "b") {
			t.Errorf("only b is active, got %+v", a)
		}
	}
}

// session writes a transcript into a config directory's project folder.
func session(t *testing.T, cfg, cwd, id, body string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(cfg, "projects", claudeProjectSlug(cwd), id+".jsonl")
	write(t, path, body)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAccountSwapMovesTheLatestSessionAndSwitches(t *testing.T) {
	home, env := accountHome(t)
	root := accountProject(t, home)
	a, b := filepath.Join(home, ".claude-a"), filepath.Join(home, ".claude-b")
	now := time.Now()
	session(t, a, root, "old", "old\n", now.Add(-time.Hour))
	session(t, a, root, "new", "new\n", now)
	write(t, filepath.Join(a, "projects", claudeProjectSlug(root), "new", "subagents", "s1.jsonl"), "sub\n")

	r := mustRun(t, root, append(env, "CLAUDE_CONFIG_DIR="+a), "account", "swap", "b")

	dst := filepath.Join(b, "projects", claudeProjectSlug(root))
	if body, err := os.ReadFile(filepath.Join(dst, "new.jsonl")); err != nil || string(body) != "new\n" {
		t.Errorf("the newest session should be copied: %q %v", body, err)
	}
	if body, err := os.ReadFile(filepath.Join(dst, "new", "subagents", "s1.jsonl")); err != nil || string(body) != "sub\n" {
		t.Errorf("the session's subagent transcripts travel with it: %q %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "old.jsonl")); !os.IsNotExist(err) {
		t.Error("only the one session moves")
	}
	if !strings.Contains(r.stdout, "claude --resume new") {
		t.Errorf("swap should print the resume command:\n%s", r.stdout)
	}
	if s := mustRun(t, root, env, "shell").stdout; !strings.Contains(s, "CLAUDE_CONFIG_DIR='"+b+"'") {
		t.Errorf("swap also switches the account:\n%s", s)
	}
}

func TestAccountSwapTakesANamedSession(t *testing.T) {
	home, env := accountHome(t)
	root := accountProject(t, home)
	a, b := filepath.Join(home, ".claude-a"), filepath.Join(home, ".claude-b")
	now := time.Now()
	session(t, a, root, "old", "old\n", now.Add(-time.Hour))
	session(t, a, root, "new", "new\n", now)

	// Without CLAUDE_CONFIG_DIR the source is the context's account, a.
	mustRun(t, root, env, "account", "swap", "b", "--session", "old")
	if _, err := os.Stat(filepath.Join(b, "projects", claudeProjectSlug(root), "old.jsonl")); err != nil {
		t.Errorf("the named session should move: %v", err)
	}
	if r := runLimen(t, root, env, "account", "swap", "a", "--session", "missing"); r.code == 0 {
		t.Error("an unknown session must be an error")
	}
}

func TestAccountSwapDryRunChangesNothing(t *testing.T) {
	home, env := accountHome(t)
	root := accountProject(t, home)
	a, b := filepath.Join(home, ".claude-a"), filepath.Join(home, ".claude-b")
	session(t, a, root, "s", "s\n", time.Now())

	r := mustRun(t, root, env, "account", "swap", "b", "--dry-run")
	if !strings.Contains(r.stdout, "s.jsonl") {
		t.Errorf("dry run should say what it would copy:\n%s", r.stdout)
	}
	if _, err := os.Stat(filepath.Join(b, "projects")); !os.IsNotExist(err) {
		t.Error("dry run must not copy")
	}
	if s := mustRun(t, root, env, "shell").stdout; !strings.Contains(s, "CLAUDE_CONFIG_DIR='"+a+"'") {
		t.Errorf("dry run must not switch:\n%s", s)
	}
}

func TestAccountSwapRefusesToOverwriteANewerCopy(t *testing.T) {
	home, env := accountHome(t)
	root := accountProject(t, home)
	a, b := filepath.Join(home, ".claude-a"), filepath.Join(home, ".claude-b")
	now := time.Now()
	session(t, a, root, "s", "stale\n", now.Add(-time.Hour))
	session(t, b, root, "s", "continued in b\n", now)

	r := runLimen(t, root, env, "account", "swap", "b")
	if r.code == 0 {
		t.Fatal("a newer copy in the target is a conversation that went on there; refuse")
	}
	if !strings.Contains(r.stderr, "--force") {
		t.Errorf("the refusal should name the way out: %s", r.stderr)
	}
	mustRun(t, root, env, "account", "swap", "b", "--force")
	if body, _ := os.ReadFile(filepath.Join(b, "projects", claudeProjectSlug(root), "s.jsonl")); string(body) != "stale\n" {
		t.Errorf("--force overwrites, got %q", body)
	}
}

func TestAccountSwapErrors(t *testing.T) {
	home, env := accountHome(t)
	root := accountProject(t, home)

	r := runLimen(t, root, env, "account", "swap", "b")
	if r.code == 0 || !strings.Contains(r.stderr, "no session") {
		t.Errorf("no transcript to move must be an error naming that: %d %s", r.code, r.stderr)
	}
	if r := runLimen(t, root, env, "account", "swap", "a"); r.code == 0 {
		t.Error("swapping to the account already in use must be refused")
	}
	if r := runLimen(t, root, env, "account", "swap"); r.code == 0 {
		t.Error("swap without a target must be an error")
	}
	if r := runLimen(t, root, env, "account", "frobnicate"); r.code == 0 {
		t.Error("an unknown subcommand must be an error")
	}
}

func TestClaudeProjectSlug(t *testing.T) {
	cases := map[string]string{
		"/Users/x/Documents/GitHub/nuncio": "-Users-x-Documents-GitHub-nuncio",
		"/var/folders/2w/a_b/T":            "-var-folders-2w-a-b-T",
		"/private/tmp/claude-502/-Users-x": "-private-tmp-claude-502--Users-x",
		"/a/.officina/wt":                  "-a--officina-wt",
	}
	for in, want := range cases {
		if got := claudeProjectSlug(in); got != want {
			t.Errorf("claudeProjectSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHookReappliesAfterAnAccountCommand(t *testing.T) {
	dir := tempDir(t)
	for _, sh := range []string{"zsh", "bash"} {
		r := mustRun(t, dir, nil, "hook", sh)
		if !strings.Contains(r.stdout, "limen()") || !strings.Contains(r.stdout, "command limen") {
			t.Errorf("%s hook should wrap limen so account changes reach the current shell:\n%s", sh, r.stdout)
		}
	}
}
