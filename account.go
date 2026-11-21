package main

// The account switch. A Claude Code account is nothing but a CLAUDE_CONFIG_DIR
// holding a login, and every project names one in limen.yaml. When an account's
// tokens run out mid-day, editing every descriptor is the wrong lever: the
// override is one machine-wide value that beats every project's claudeConfigDir
// until it is cleared. Everything that inherits the shell — terminals, officina
// strands, clavo sessions — picks it up from CLAUDE_CONFIG_DIR as before.
//
// A running session cannot change its config directory. `swap` does the next
// best thing: it copies the session transcript into the other account's project
// folder, switches, and prints the `claude --resume` that continues it there.
// limen prints the command; it never runs it.
//
// The override flows into `eval` in the user's shell on every cd, and swap
// copies files between accounts, so both are held to the findings of the
// red-team pass in docs/security/red-team-2026-11-21-account.md: values are
// checked where they are read, ids where they are picked, and no write follows
// a symlink.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// accountOverridePath sits next to the registry: state of this machine, not
// configuration anyone edits by hand.
func accountOverridePath() (string, error) {
	reg, err := registryPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(reg), "account"), nil
}

// readAccountOverride returns the file's value as written, unchecked, or "".
func readAccountOverride() string {
	path, err := accountOverridePath()
	if err != nil {
		return ""
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(body), "\n")
}

// loadAccountOverride returns the overriding config directory, or "" when none
// is set or the file holds something that is no account (F-9). It runs on every
// cd through the shell hook, so it is never an error.
func loadAccountOverride() string {
	v := readAccountOverride()
	if v == "" || checkAccountDir(v) != nil {
		return ""
	}
	return v
}

func saveAccountOverride(dir string) error {
	path, err := accountOverridePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// The registry may have created the directory 0755 earlier (F-12).
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, []byte(dir+"\n"), 0o600, time.Time{})
}

func clearAccountOverride() error {
	path, err := accountOverridePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// writeFileAtomic writes through a temp file with a random name in the target
// directory and renames it into place, so neither a reader sees half a file nor
// a symlink planted at a predictable temp name is written through (F-4).
func writeFileAtomic(path string, body []byte, perm fs.FileMode, mtime time.Time) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(tmp, mtime, mtime); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// applyAccountOverride makes the override the context's ClaudeDir and keeps the
// project's own value beside it, so show and json can say what was overridden.
func applyAccountOverride(c *Context, override string) {
	if c == nil {
		return
	}
	c.ProjectClaudeDir = c.ClaudeDir
	if override != "" {
		c.ClaudeDir = override
		c.AccountOverride = override
	}
}

// shellAccountLines are what `limen shell` adds for the switch. An active
// override is exported with a marker, so a later call can tell a value limen
// set from one the user set; once the override is cleared — in this shell or
// another — the marked value is unset again instead of lingering (F-7).
func shellAccountLines(w io.Writer, c *Context, override string) {
	if override != "" {
		if c == nil {
			fmt.Fprintf(w, "export CLAUDE_CONFIG_DIR=%s\n", shellQuote(override))
		}
		fmt.Fprintf(w, "export LIMEN_ACCOUNT_OVERRIDE=%s\n", shellQuote(override))
		return
	}
	prev := os.Getenv("LIMEN_ACCOUNT_OVERRIDE")
	if prev == "" {
		return
	}
	if (c == nil || c.ClaudeDir == "") && os.Getenv("CLAUDE_CONFIG_DIR") == prev {
		fmt.Fprintln(w, "unset CLAUDE_CONFIG_DIR")
	}
	fmt.Fprintln(w, "unset LIMEN_ACCOUNT_OVERRIDE")
}

var (
	accountNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	// Claude Code names transcripts by UUID. Anything else — "..", ".", a name
	// with a separator, a newline or an escape — is not a session (F-1, F-8).
	sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
)

// hasControl reports C0, DEL and C1 characters: what a terminal or a shell
// would act on rather than show.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return true
		}
	}
	return false
}

// printable replaces control characters so a hostile path cannot drive the
// terminal it is printed to.
func printable(s string) string {
	if !hasControl(s) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			b.WriteRune('?')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isConventionalAccount is ~/.claude or ~/.claude-<something>.
func isConventionalAccount(dir string) bool {
	home, err := os.UserHomeDir()
	if err != nil || filepath.Dir(dir) != home {
		return false
	}
	base := filepath.Base(dir)
	return base == ".claude" || (strings.HasPrefix(base, ".claude-") && len(base) > len(".claude-"))
}

// checkAccountDir is the one test every account directory passes, whether it
// comes from `use`, from the state file or as a swap source: absolute, clean,
// printable, an existing directory, and recognisably a Claude config — the
// conventional place, or one carrying Claude's own files (F-9, F-10).
func checkAccountDir(dir string) error {
	if hasControl(dir) || strings.Contains(dir, "%") {
		return fmt.Errorf("account path %q holds control or prompt characters", printable(dir))
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return fmt.Errorf("account path %s is not a clean absolute path", dir)
	}
	home, _ := os.UserHomeDir()
	if dir == "/" || dir == home {
		return fmt.Errorf("%s is not a Claude config directory", dir)
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("no Claude config directory %s — log in there first: CLAUDE_CONFIG_DIR=%s claude", dir, shellQuote(dir))
	}
	if isConventionalAccount(dir) {
		return nil
	}
	for _, marker := range []string{".claude.json", "settings.json", "projects"} {
		if _, err := os.Lstat(filepath.Join(dir, marker)); err == nil {
			return nil
		}
	}
	return fmt.Errorf("%s does not look like a Claude config directory (no .claude.json, settings.json or projects/)", dir)
}

// resolveAccount maps a name to a config directory: "default" is ~/.claude,
// a bare name n is ~/.claude-n, and anything starting with / or ~ is a path.
// The directory must pass checkAccountDir — a typo must not silently log a
// session into a fresh, empty account.
func resolveAccount(name string) (string, error) {
	if name == "" || name != strings.TrimSpace(name) {
		return "", fmt.Errorf("invalid account name %q (leading or trailing space)", printable(name))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	var dir string
	switch {
	case name == "default":
		dir = filepath.Join(home, ".claude")
	case strings.HasPrefix(name, "/") || strings.HasPrefix(name, "~"):
		dir = filepath.Clean(expandTilde(name))
	case accountNamePattern.MatchString(name):
		dir = filepath.Join(home, ".claude-"+name)
	default:
		return "", fmt.Errorf("invalid account name %q (a name like turbogruen-2, \"default\", or a path)", printable(name))
	}
	if err := checkAccountDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// accountName is the short name of a config directory, the inverse of
// resolveAccount for the conventional places, and the path otherwise.
func accountName(dir string) string {
	if dir == "" {
		return ""
	}
	if isConventionalAccount(dir) {
		base := filepath.Base(dir)
		if base == ".claude" {
			return "default"
		}
		return strings.TrimPrefix(base, ".claude-")
	}
	return dir
}

type accountEntry struct {
	Name   string `json:"name"`
	Dir    string `json:"dir"`
	Active bool   `json:"active"`
}

// listAccounts finds ~/.claude and every ~/.claude-* directory. Files with
// such names (backups, archives) are not accounts.
func listAccounts(active string) ([]accountEntry, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil, err
	}
	list := []accountEntry{}
	for _, e := range entries {
		n := e.Name()
		if n != ".claude" && !strings.HasPrefix(n, ".claude-") {
			continue
		}
		dir := filepath.Join(home, n)
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		list = append(list, accountEntry{Name: accountName(dir), Dir: dir, Active: dir == active})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

// claudeProjectSlug is how Claude Code names a working directory's folder
// under projects/: every character that is not a letter or digit becomes '-'.
func claudeProjectSlug(dir string) string {
	var b strings.Builder
	for _, r := range dir {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// CmdAccount dispatches `limen account …`. c is the context with the override
// already applied, or nil outside a project.
func CmdAccount(w io.Writer, c *Context, cwd string, args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "", "show", "status":
		return accountStatus(w, c)
	case "list":
		jsonOut := len(args) > 1 && args[1] == "--json"
		return accountList(w, c, jsonOut)
	case "use":
		if len(args) != 2 {
			return errors.New("usage: limen account use <name|default|path>")
		}
		dir, err := resolveAccount(args[1])
		if err != nil {
			return err
		}
		if err := saveAccountOverride(dir); err != nil {
			return err
		}
		fmt.Fprintf(w, "account: %s (%s) — overrides every project until `limen account clear`\n", accountName(dir), dir)
		fmt.Fprintln(w, "New sessions use it; a running one keeps its account (see `limen account swap`).")
		return nil
	case "clear":
		if err := clearAccountOverride(); err != nil {
			return err
		}
		fmt.Fprintln(w, "account override cleared — each project's claudeConfigDir applies again")
		return nil
	case "swap":
		return accountSwap(w, c, cwd, args[1:])
	default:
		return fmt.Errorf("unknown account command %q (show, list, use, clear, swap)", printable(sub))
	}
}

func effectiveDir(c *Context) string {
	if c != nil {
		return c.ClaudeDir
	}
	return loadAccountOverride()
}

func accountStatus(w io.Writer, c *Context) error {
	if raw := readAccountOverride(); raw != "" {
		if err := checkAccountDir(raw); err != nil {
			fmt.Fprintf(w, "override: invalid, ignored — %s\n", printable(err.Error()))
			fmt.Fprintln(w, "          `limen account clear` removes it")
		}
	}
	override := loadAccountOverride()
	dir, source := effectiveDir(c), ""
	switch {
	case override != "":
		source = "override"
		if c != nil && c.ProjectClaudeDir != "" && c.ProjectClaudeDir != override {
			source += fmt.Sprintf(" (the project names %s)", printable(accountName(c.ProjectClaudeDir)))
		}
	case dir != "":
		source = "project (.limen/limen.yaml)"
	default:
		fmt.Fprintln(w, "account: none set — Claude Code uses CLAUDE_CONFIG_DIR from the environment or ~/.claude")
		return nil
	}
	fmt.Fprintf(w, "account: %s (%s)\n", printable(accountName(dir)), printable(dir))
	fmt.Fprintf(w, "source:  %s\n", source)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		fmt.Fprintln(w, "warning: the directory does not exist")
	}
	return nil
}

func accountList(w io.Writer, c *Context, jsonOut bool) error {
	list, err := listAccounts(effectiveDir(c))
	if err != nil {
		return err
	}
	if jsonOut {
		b, err := json.Marshal(list)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	}
	for _, a := range list {
		mark := " "
		if a.Active {
			mark = "*"
		}
		fmt.Fprintf(w, "%s %-20s %s\n", mark, printable(a.Name), printable(a.Dir))
	}
	return nil
}

type swapOpts struct {
	target     string
	from       string
	session    string
	sessionSet bool
	dryRun     bool
	force      bool
}

func parseSwap(args []string) (swapOpts, error) {
	var o swapOpts
	value := func(i *int, flag string) (string, error) {
		if *i+1 >= len(args) {
			return "", fmt.Errorf("%s needs a value", flag)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		var err error
		switch {
		case a == "--dry-run" || a == "-n":
			o.dryRun = true
		case a == "--force":
			o.force = true
		case a == "--session":
			o.session, err = value(&i, a)
			o.sessionSet = true
		case strings.HasPrefix(a, "--session="):
			o.session, o.sessionSet = strings.TrimPrefix(a, "--session="), true
		case a == "--from":
			o.from, err = value(&i, a)
		case strings.HasPrefix(a, "--from="):
			o.from = strings.TrimPrefix(a, "--from=")
		case strings.HasPrefix(a, "-"):
			err = fmt.Errorf("unknown flag %q", printable(a))
		case o.target == "":
			o.target = a
		default:
			err = fmt.Errorf("unexpected argument %q", printable(a))
		}
		if err != nil {
			return o, err
		}
	}
	if o.target == "" {
		return o, errors.New("usage: limen account swap <name|default|path> [--from <account>] [--session <id>] [--dry-run] [--force]")
	}
	if o.sessionSet {
		o.session = strings.TrimSuffix(o.session, ".jsonl")
		if !sessionIDPattern.MatchString(o.session) {
			return o, fmt.Errorf("invalid session id %q", printable(o.session))
		}
	}
	return o, nil
}

// swapSource is the account the session ran under: --from when given, else the
// shell's CLAUDE_CONFIG_DIR, because that is what the session was started with,
// then the context, then Claude Code's default. Without --from the source must
// be a conventional account: a CLAUDE_CONFIG_DIR a cloned repository chose must
// not decide what lands in another account (F-2).
func swapSource(c *Context, from string) (string, error) {
	if from != "" {
		return resolveAccount(from)
	}
	dir := ""
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		dir = filepath.Clean(expandTilde(d))
	} else if d := effectiveDir(c); d != "" {
		dir = d
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".claude")
	}
	if !isConventionalAccount(dir) || checkAccountDir(dir) != nil {
		return "", fmt.Errorf("the session's account %s is not ~/.claude or ~/.claude-*; name the source with --from <account>", printable(dir))
	}
	return dir, nil
}

// swapCandidates are the directories a session of this place may have been
// started from: where we stand and the project root, each also with symlinks
// resolved, because Claude Code keys the folder by the path it saw.
func swapCandidates(c *Context, cwd string) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range []string{cwd, rootOf(c)} {
		add(p)
		if r, err := filepath.EvalSymlinks(p); err == nil {
			add(r)
		}
	}
	return out
}

func rootOf(c *Context) string {
	if c == nil {
		return ""
	}
	return c.Root
}

// copyStep is one regular file to carry over.
type copyStep struct {
	src, dst string
	info     fs.FileInfo
}

// noSymlink refuses a path that exists as a symlink: a link in the target
// account would carry the write somewhere else (F-5).
func noSymlink(path string) error {
	if st, err := os.Lstat(path); err == nil && st.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write through it", printable(path))
	}
	return nil
}

// planSwap lists every file the swap copies and what it leaves out, and checks
// every target folder before anything is written.
func planSwap(srcFile, srcFolder, dstFolder, id string) ([]copyStep, []string, error) {
	for _, p := range []string{filepath.Dir(dstFolder), dstFolder} {
		if err := noSymlink(p); err != nil {
			return nil, nil, err
		}
	}
	info, err := os.Lstat(srcFile)
	if err != nil {
		return nil, nil, err
	}
	steps := []copyStep{{src: srcFile, dst: filepath.Join(dstFolder, id+".jsonl"), info: info}}
	skipped := []string{}

	side := filepath.Join(srcFolder, id)
	st, err := os.Lstat(side)
	switch {
	case err != nil:
		return steps, skipped, nil
	case st.Mode()&fs.ModeSymlink != 0:
		return steps, append(skipped, id+"/ (a symlink)"), nil
	case !st.IsDir():
		return steps, skipped, nil
	}
	err = filepath.WalkDir(side, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcFolder, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstFolder, rel)
		switch {
		case d.IsDir():
			return noSymlink(target)
		case d.Type().IsRegular():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			steps = append(steps, copyStep{src: path, dst: target, info: fi})
		default:
			skipped = append(skipped, rel+" (not a regular file)")
		}
		return nil
	})
	return steps, skipped, err
}

func accountSwap(w io.Writer, c *Context, cwd string, args []string) error {
	o, err := parseSwap(args)
	if err != nil {
		return err
	}
	dst, err := resolveAccount(o.target)
	if err != nil {
		return err
	}
	src, err := swapSource(c, o.from)
	if err != nil {
		return err
	}
	if src == dst {
		return fmt.Errorf("the session already runs under %s", printable(accountName(dst)))
	}

	var workDir, srcFolder string
	tried := []string{}
	for _, cand := range swapCandidates(c, cwd) {
		folder := filepath.Join(src, "projects", claudeProjectSlug(cand))
		tried = append(tried, folder)
		if st, err := os.Lstat(folder); err == nil && st.IsDir() {
			workDir, srcFolder = cand, folder
			break
		}
	}
	if srcFolder == "" {
		return fmt.Errorf("no session of this directory under %s (looked in %s)", printable(accountName(src)), printable(strings.Join(tried, ", ")))
	}

	id, srcFile, err := pickSession(srcFolder, o.session)
	if err != nil {
		return err
	}
	dstFolder := filepath.Join(dst, "projects", claudeProjectSlug(workDir))
	steps, skipped, err := planSwap(srcFile, srcFolder, dstFolder, id)
	if err != nil {
		return err
	}
	for _, s := range steps {
		if di, err := os.Lstat(s.dst); err == nil && di.ModTime().After(s.info.ModTime()) && !o.force {
			rel, _ := filepath.Rel(dstFolder, s.dst)
			return fmt.Errorf("%s already holds a newer %s of session %s — it went on there; --force overwrites it", printable(accountName(dst)), printable(rel), id)
		}
	}

	fmt.Fprintf(w, "session %s (%s) of %s\n", id, steps[0].info.ModTime().Format("2006-01-02 15:04"), printable(workDir))
	fmt.Fprintf(w, "  from %s (%s)\n  to   %s (%s)\n", printable(accountName(src)), printable(src), printable(accountName(dst)), printable(dst))
	for _, s := range skipped {
		fmt.Fprintf(w, "  skipped %s\n", printable(s))
	}
	if o.dryRun {
		for _, s := range steps {
			rel, _ := filepath.Rel(dstFolder, s.dst)
			fmt.Fprintf(w, "  would copy %s\n", printable(rel))
		}
		fmt.Fprintln(w, "and switch the account — nothing changed (dry run)")
		return nil
	}

	for _, s := range steps {
		if err := os.MkdirAll(filepath.Dir(s.dst), 0o700); err != nil {
			return err
		}
		if err := copyTranscriptFile(s.src, s.dst, s.info); err != nil {
			return err
		}
	}
	if err := saveAccountOverride(dst); err != nil {
		return err
	}
	fmt.Fprintf(w, "copied %d file(s); account is now %s (override)\n\n", len(steps), printable(accountName(dst)))
	fmt.Fprintf(w, "resume:\n  cd %s && claude --resume %s\n", shellQuote(printable(workDir)), id)
	fmt.Fprintf(w, "  (a shell without the limen hook: CLAUDE_CONFIG_DIR=%s claude --resume %s)\n", shellQuote(printable(dst)), id)
	fmt.Fprintln(w, "The prompt cache does not cross accounts: the first turn reads the whole context again.")
	return nil
}

// pickSession returns the named session, or the most recently written one.
// Only regular files whose name is a session id count (F-1, F-3).
func pickSession(folder, id string) (string, string, error) {
	if id != "" {
		path := filepath.Join(folder, id+".jsonl")
		st, err := os.Lstat(path)
		if err != nil {
			return "", "", fmt.Errorf("no session %s in %s", id, printable(folder))
		}
		if !st.Mode().IsRegular() {
			return "", "", fmt.Errorf("session %s is not a regular file", id)
		}
		return id, path, nil
	}
	entries, err := os.ReadDir(folder)
	if err != nil {
		return "", "", err
	}
	var best string
	var bestTime time.Time
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".jsonl") || !sessionIDPattern.MatchString(strings.TrimSuffix(name, ".jsonl")) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if best == "" || info.ModTime().After(bestTime) {
			best, bestTime = name, info.ModTime()
		}
	}
	if best == "" {
		return "", "", fmt.Errorf("no session transcript in %s", printable(folder))
	}
	return strings.TrimSuffix(best, ".jsonl"), filepath.Join(folder, best), nil
}

// copyTranscriptFile copies one regular file with its mode and modification
// time, so the newer-copy check of a later swap compares like with like. The
// source is opened without following a symlink swapped in after planning (F-3).
func copyTranscriptFile(src, dst string, info fs.FileInfo) error {
	f, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, body, info.Mode().Perm(), info.ModTime())
}
