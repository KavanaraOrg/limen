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

// loadAccountOverride returns the overriding config directory, or "" when none
// is set. It runs on every cd through the shell hook, so it is one small read
// and never an error: an unreadable file means no override.
func loadAccountOverride() string {
	path, err := accountOverridePath()
	if err != nil {
		return ""
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

func saveAccountOverride(dir string) error {
	path, err := accountOverridePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Write and rename, so a shell reading on cd never sees half a path.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(dir+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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

var accountNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// resolveAccount maps a name to a config directory: "default" is ~/.claude,
// a bare name n is ~/.claude-n, and anything starting with / or ~ is a path.
// The directory must exist — a typo must not silently log a session into a
// fresh, empty account.
func resolveAccount(name string) (string, error) {
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
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("account path %q is not absolute", name)
		}
	case accountNamePattern.MatchString(name):
		dir = filepath.Join(home, ".claude-"+name)
	default:
		return "", fmt.Errorf("invalid account name %q (a name like turbogruen-2, \"default\", or a path)", name)
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("no Claude config directory %s for account %q — log in there first: CLAUDE_CONFIG_DIR=%s claude", dir, name, shellQuote(dir))
	}
	return dir, nil
}

// accountName is the short name of a config directory, the inverse of
// resolveAccount for the conventional places, and the path otherwise.
func accountName(dir string) string {
	if dir == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err == nil && filepath.Dir(dir) == home {
		base := filepath.Base(dir)
		if base == ".claude" {
			return "default"
		}
		if n, ok := strings.CutPrefix(base, ".claude-"); ok && n != "" {
			return n
		}
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
		return fmt.Errorf("unknown account command %q (show, list, use, clear, swap)", sub)
	}
}

func effectiveDir(c *Context) string {
	if c != nil {
		return c.ClaudeDir
	}
	return loadAccountOverride()
}

func accountStatus(w io.Writer, c *Context) error {
	override := loadAccountOverride()
	dir, source := effectiveDir(c), ""
	switch {
	case override != "":
		source = "override"
		if c != nil && c.ProjectClaudeDir != "" && c.ProjectClaudeDir != override {
			source += fmt.Sprintf(" (the project names %s)", accountName(c.ProjectClaudeDir))
		}
	case dir != "":
		source = "project (.limen/limen.yaml)"
	default:
		fmt.Fprintln(w, "account: none set — Claude Code uses CLAUDE_CONFIG_DIR from the environment or ~/.claude")
		return nil
	}
	fmt.Fprintf(w, "account: %s (%s)\n", accountName(dir), dir)
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
		fmt.Fprintf(w, "%s %-20s %s\n", mark, a.Name, a.Dir)
	}
	return nil
}

type swapOpts struct {
	target  string
	session string
	dryRun  bool
	force   bool
}

func parseSwap(args []string) (swapOpts, error) {
	var o swapOpts
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--dry-run" || a == "-n":
			o.dryRun = true
		case a == "--force":
			o.force = true
		case a == "--session":
			if i+1 >= len(args) {
				return o, errors.New("--session needs a session id")
			}
			i++
			o.session = args[i]
		case strings.HasPrefix(a, "--session="):
			o.session = strings.TrimPrefix(a, "--session=")
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown flag %q", a)
		case o.target == "":
			o.target = a
		default:
			return o, fmt.Errorf("unexpected argument %q", a)
		}
	}
	if o.target == "" {
		return o, errors.New("usage: limen account swap <name|default|path> [--session <id>] [--dry-run] [--force]")
	}
	o.session = strings.TrimSuffix(o.session, ".jsonl")
	return o, nil
}

// swapSource is the account the session ran under: the shell's
// CLAUDE_CONFIG_DIR first, because that is what the session was started with,
// then the context, then Claude Code's default.
func swapSource(c *Context) (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Clean(expandTilde(d)), nil
	}
	if d := effectiveDir(c); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
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

func accountSwap(w io.Writer, c *Context, cwd string, args []string) error {
	o, err := parseSwap(args)
	if err != nil {
		return err
	}
	dst, err := resolveAccount(o.target)
	if err != nil {
		return err
	}
	src, err := swapSource(c)
	if err != nil {
		return err
	}
	if src == dst {
		return fmt.Errorf("the session already runs under %s", accountName(dst))
	}

	var workDir, srcFolder string
	tried := []string{}
	for _, cand := range swapCandidates(c, cwd) {
		folder := filepath.Join(src, "projects", claudeProjectSlug(cand))
		tried = append(tried, folder)
		if st, err := os.Stat(folder); err == nil && st.IsDir() {
			workDir, srcFolder = cand, folder
			break
		}
	}
	if srcFolder == "" {
		return fmt.Errorf("no session of this directory under %s (looked in %s)", accountName(src), strings.Join(tried, ", "))
	}

	id, srcFile, err := pickSession(srcFolder, o.session)
	if err != nil {
		return err
	}
	srcInfo, err := os.Stat(srcFile)
	if err != nil {
		return err
	}
	dstFolder := filepath.Join(dst, "projects", claudeProjectSlug(workDir))
	dstFile := filepath.Join(dstFolder, id+".jsonl")
	if di, err := os.Stat(dstFile); err == nil && di.ModTime().After(srcInfo.ModTime()) && !o.force {
		return fmt.Errorf("%s already holds a newer copy of session %s — it went on there; --force overwrites it", accountName(dst), id)
	}
	sideDir := filepath.Join(srcFolder, id)
	hasSide := false
	if st, err := os.Stat(sideDir); err == nil && st.IsDir() {
		hasSide = true
	}

	fmt.Fprintf(w, "session %s (%s) of %s\n", id, srcInfo.ModTime().Format("2006-01-02 15:04"), workDir)
	fmt.Fprintf(w, "  from %s (%s)\n  to   %s (%s)\n", accountName(src), src, accountName(dst), dst)
	if o.dryRun {
		fmt.Fprintf(w, "would copy %s", filepath.Base(srcFile))
		if hasSide {
			fmt.Fprintf(w, " and %s/", id)
		}
		fmt.Fprintln(w, " and switch the account — nothing changed (dry run)")
		return nil
	}

	if err := copyTranscriptFile(srcFile, dstFile); err != nil {
		return err
	}
	if hasSide {
		if err := copyTranscriptTree(sideDir, filepath.Join(dstFolder, id)); err != nil {
			return err
		}
	}
	if err := saveAccountOverride(dst); err != nil {
		return err
	}
	fmt.Fprintf(w, "copied %s", filepath.Base(srcFile))
	if hasSide {
		fmt.Fprintf(w, " and %s/", id)
	}
	fmt.Fprintf(w, "; account is now %s (override)\n\n", accountName(dst))
	fmt.Fprintf(w, "resume from %s:\n  claude --resume %s\n", workDir, id)
	fmt.Fprintf(w, "  (a shell without the limen hook: CLAUDE_CONFIG_DIR=%s claude --resume %s)\n", shellQuote(dst), id)
	fmt.Fprintln(w, "The prompt cache does not cross accounts: the first turn reads the whole context again.")
	return nil
}

// pickSession returns the named session, or the most recently written one.
func pickSession(folder, id string) (string, string, error) {
	if id != "" {
		if strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
			return "", "", fmt.Errorf("invalid session id %q", id)
		}
		path := filepath.Join(folder, id+".jsonl")
		if _, err := os.Stat(path); err != nil {
			return "", "", fmt.Errorf("no session %s in %s", id, folder)
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
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if best == "" || info.ModTime().After(bestTime) {
			best, bestTime = e.Name(), info.ModTime()
		}
	}
	if best == "" {
		return "", "", fmt.Errorf("no session transcript in %s", folder)
	}
	return strings.TrimSuffix(best, ".jsonl"), filepath.Join(folder, best), nil
}

// copyTranscriptFile copies with the source's mode and modification time, so the
// newer-copy check of a later swap compares like with like.
func copyTranscriptFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := dst + ".limen-tmp"
	if err := os.WriteFile(tmp, body, info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// copyTranscriptTree copies regular files below src; symlinks are skipped rather than
// followed, so a link in a transcript folder cannot pull foreign files along.
func copyTranscriptTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type().IsRegular():
			return copyTranscriptFile(path, target)
		default:
			return nil
		}
	})
}
