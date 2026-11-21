package main

// The account registry. A Claude account can be authenticated by a long-lived
// OAuth token from `claude setup-token`; Claude Code reads it from
// CLAUDE_CODE_OAUTH_TOKEN, and it beats the login stored in CLAUDE_CONFIG_DIR.
// The registry says, per account, where that token comes from. It never holds
// the token itself: a source is `bws:<secret-uuid>` (Bitwarden Secrets Manager)
// or `env:<VAR>`.
//
// The token is resolved when it is needed and goes to exactly two places: the
// stdout of `limen account env`, meant for eval, and the environment of the
// process `limen account exec` replaces itself with. Not into json, list,
// shell, a file or an error message.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const (
	tokenEnv       = "CLAUDE_CODE_OAUTH_TOKEN"
	tokenMarkerEnv = "LIMEN_ACCOUNT_TOKEN_FOR"
)

type claudeAccount struct {
	Name      string
	Label     string
	ConfigDir string // as written, ~ allowed; "" means ~/.claude-<name>
	Token     string // the source: bws:<uuid> or env:<VAR>
}

var (
	uuidPattern    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	envVarPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	tokenPattern   = regexp.MustCompile(`^sk-ant-[A-Za-z0-9_-]+$`)
	tokenLikeInStr = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]*`)
)

func (a claudeAccount) sourceKind() string {
	kind, _, _ := strings.Cut(a.Token, ":")
	return kind
}

func (a claudeAccount) sourceRef() string {
	_, ref, _ := strings.Cut(a.Token, ":")
	return ref
}

// dir is the account's CLAUDE_CONFIG_DIR.
func (a claudeAccount) dir() (string, error) {
	if a.ConfigDir != "" {
		return filepath.Clean(expandTilde(a.ConfigDir)), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude-"+a.Name), nil
}

func claudeAccountsPath() (string, error) {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "limen", "claude-accounts.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "limen", "claude-accounts.yaml"), nil
}

const claudeAccountsTemplate = `# Claude accounts of this machine, for limen account use | env | exec.
# Machine-local: never commit this file. It holds no token, only where one is:
#   token: bws:<secret-uuid>   Bitwarden Secrets Manager (bws CLI; BWS_ACCESS_TOKEN
#                              from the environment, else the login keychain)
#   token: env:<VAR>           an environment variable holding the sk-ant- token
# configDir is optional and defaults to ~/.claude-<name>.
accounts:
#  - name: work
#    label: Claude-Work
#    token: bws:00000000-0000-0000-0000-000000000000
#    configDir: ~/.claude-work
`

// ensureClaudeAccounts creates the registry template when there is none, 0600
// in a 0700 directory. Only the account commands call it, never the shell hook.
func ensureClaudeAccounts() error {
	path, err := claudeAccountsPath()
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, []byte(claudeAccountsTemplate), 0o600, time.Time{})
}

// loadClaudeAccounts reads the registry; a missing file is an empty one.
func loadClaudeAccounts() ([]claudeAccount, error) {
	path, err := claudeAccountsPath()
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	list, err := parseClaudeAccounts(string(body))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return list, nil
}

// parseClaudeAccounts reads the one shape the registry has: a list under
// accounts:, each item a few key: value lines. Errors never quote a token
// field, in case a token was pasted there.
func parseClaudeAccounts(body string) ([]claudeAccount, error) {
	var list []claudeAccount
	var cur *claudeAccount
	sc := bufio.NewScanner(strings.NewReader(body))
	n := 0
	for sc.Scan() {
		n++
		raw := sc.Text()
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || trimmed == "accounts:" || trimmed == "---" {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			list = append(list, claudeAccount{})
			cur = &list[len(list)-1]
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			if trimmed == "" {
				continue
			}
		} else if cur == nil {
			return nil, fmt.Errorf("line %d: a key outside an account entry (entries start with \"- name: …\")", n)
		}
		key, val, ok := parseLine(trimmed)
		if !ok {
			return nil, fmt.Errorf("line %d: not a key: value line", n)
		}
		switch key {
		case "name":
			cur.Name = val
		case "label":
			cur.Label = val
		case "configdir":
			cur.ConfigDir = val
		case "token":
			cur.Token = val
		default:
			return nil, fmt.Errorf("line %d: unknown key (name, label, token, configDir)", n)
		}
	}
	seen := map[string]bool{}
	for i, a := range list {
		if err := checkClaudeAccount(a); err != nil {
			return nil, fmt.Errorf("account %d: %w", i+1, err)
		}
		if seen[a.Name] {
			return nil, fmt.Errorf("account %s is listed twice", a.Name)
		}
		seen[a.Name] = true
	}
	return list, nil
}

func checkClaudeAccount(a claudeAccount) error {
	if !accountNamePattern.MatchString(a.Name) || a.Name == "default" {
		return fmt.Errorf("invalid name %q (letters, digits, . _ -; not \"default\")", printable(a.Name))
	}
	if hasControl(a.Label) {
		return fmt.Errorf("%s: label holds control characters", a.Name)
	}
	if strings.HasPrefix(a.Token, "sk-ant-") {
		return fmt.Errorf("%s: token holds a token itself; the registry keeps only its source (bws:<id> or env:<VAR>)", a.Name)
	}
	switch a.sourceKind() {
	case "bws":
		if !uuidPattern.MatchString(a.sourceRef()) {
			return fmt.Errorf("%s: bws: needs a secret UUID", a.Name)
		}
	case "env":
		if !envVarPattern.MatchString(a.sourceRef()) {
			return fmt.Errorf("%s: env: needs a variable name", a.Name)
		}
	default:
		return fmt.Errorf("%s: token must be bws:<secret-uuid> or env:<VAR>", a.Name)
	}
	if a.ConfigDir != "" {
		d := expandTilde(a.ConfigDir)
		if hasControl(d) || !filepath.IsAbs(d) {
			return fmt.Errorf("%s: configDir must be an absolute path or start with ~", a.Name)
		}
	}
	return nil
}

func findClaudeAccount(list []claudeAccount, name string) *claudeAccount {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

// claudeAccountForDir maps a config directory back to its registry entry.
func claudeAccountForDir(list []claudeAccount, dir string) *claudeAccount {
	if dir == "" {
		return nil
	}
	for i := range list {
		if d, err := list[i].dir(); err == nil && d == dir {
			return &list[i]
		}
	}
	return nil
}

// registryAccountDir resolves a registry name to its config directory and
// creates a conventional one that does not exist yet: with a token the account
// needs no stored login, only a place for its settings. ok is false when the
// name is not in the registry.
func registryAccountDir(name string) (string, bool, error) {
	list, err := loadClaudeAccounts()
	if err != nil {
		return "", false, err
	}
	a := findClaudeAccount(list, name)
	if a == nil {
		return "", false, nil
	}
	dir, err := a.dir()
	if err != nil {
		return "", true, err
	}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) && isConventionalAccount(dir) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return "", true, err
		}
	}
	return dir, true, checkAccountDir(dir)
}

func validToken(v string) bool { return tokenPattern.MatchString(v) }

// accountTokenEnv is the variable a token for this account may already lie in.
func accountTokenEnv(name string) string { return "LIMEN_CLAUDE_TOKEN_" + envSuffix(name) }

// explicitEnvToken is CLAUDE_CODE_OAUTH_TOKEN when it counts as a choice: set
// by hand (no marker), or set by limen for this very account. A token limen
// exported for another account is a leftover of a switch, not a choice.
func explicitEnvToken(a *claudeAccount, getenv func(string) string) (string, bool) {
	t := getenv(tokenEnv)
	if !validToken(t) {
		return "", false
	}
	marker := getenv(tokenMarkerEnv)
	if marker == "" || (a != nil && marker == a.Name) {
		return t, true
	}
	return "", false
}

// tokenSourceKind is what json reports: where the token would come from,
// without fetching it.
func tokenSourceKind(a *claudeAccount, getenv func(string) string) string {
	if _, ok := explicitEnvToken(a, getenv); ok {
		return "env"
	}
	if a == nil {
		return "none"
	}
	if validToken(getenv(accountTokenEnv(a.Name))) {
		return "env"
	}
	return a.sourceKind()
}

// resolveClaudeToken applies the precedence: an explicit token in the
// environment, then the account's own variable or env: source, then bws, then
// none. Errors never carry a value.
func resolveClaudeToken(a *claudeAccount, getenv func(string) string, bws func(id string) (string, error)) (string, string, error) {
	if t, ok := explicitEnvToken(a, getenv); ok {
		return t, "env", nil
	}
	if a == nil {
		return "", "none", nil
	}
	if t := getenv(accountTokenEnv(a.Name)); validToken(t) {
		return t, "env", nil
	}
	switch a.sourceKind() {
	case "env":
		t := getenv(a.sourceRef())
		if !validToken(t) {
			return "", "", fmt.Errorf("%s: $%s is empty or holds no sk-ant- token", a.Name, a.sourceRef())
		}
		return t, "env", nil
	case "bws":
		if bws == nil {
			return "", "", fmt.Errorf("%s: no bws available", a.Name)
		}
		t, err := bws(a.sourceRef())
		if err != nil {
			return "", "", fmt.Errorf("%s: %w", a.Name, err)
		}
		t = strings.TrimSpace(t)
		if !validToken(t) {
			return "", "", fmt.Errorf("%s: Bitwarden secret %s holds no sk-ant- token", a.Name, a.sourceRef())
		}
		return t, "bws", nil
	}
	return "", "", fmt.Errorf("%s: unknown token source", a.Name)
}

// keychainLookup is the seam for the BWS_ACCESS_TOKEN keychain fallback.
var keychainLookup = securityLookup

// bwsAccessToken: the environment first, then the login keychain (service
// BWS_ACCESS_TOKEN, account $USER) — launchd services and GUI-started
// processes never read a shell startup file, the keychain is the one global
// place.
func bwsAccessToken(getenv func(string) string, lookup func(service, account string) (string, bool)) (string, error) {
	if v := strings.TrimSpace(getenv("BWS_ACCESS_TOKEN")); v != "" {
		return v, nil
	}
	user := getenv("USER")
	if user != "" && lookup != nil {
		if v, ok := lookup("BWS_ACCESS_TOKEN", user); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), nil
		}
	}
	return "", errors.New("no BWS_ACCESS_TOKEN: not in the environment and not in the login keychain (security add-generic-password -a \"$USER\" -s BWS_ACCESS_TOKEN -w)")
}

func bwsBinary() string {
	if b := os.Getenv("LIMEN_BWS_BIN"); b != "" {
		return b
	}
	if _, err := os.Stat("/usr/local/bin/bws"); err == nil {
		return "/usr/local/bin/bws"
	}
	return "bws"
}

// bwsErrorText is what a failed bws call may say: its exit and the first line
// of stderr, with anything shaped like a token and the access token removed.
func bwsErrorText(err error, stderr, access string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(stderr), "\n")
	if access != "" {
		line = strings.ReplaceAll(line, access, "[redacted]")
	}
	line = tokenLikeInStr.ReplaceAllString(line, "[redacted]")
	if len(line) > 200 {
		line = line[:200]
	}
	msg := err.Error()
	if line != "" {
		msg += ": " + printable(line)
	}
	return msg
}

// bwsGet reads one secret's value through the bws CLI. A package variable, so
// the tests never reach the real one.
var bwsGet = func(id string) (string, error) {
	access, err := bwsAccessToken(os.Getenv, keychainLookup)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bwsBinary(), "secret", "get", id, "--output", "json", "--color", "no")
	cmd.Env = append(os.Environ(), "BWS_ACCESS_TOKEN="+access)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("bws secret get %s failed (%s)", id, bwsErrorText(err, stderr.String(), access))
	}
	var secret struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(stdout.String()), &secret); err != nil {
		return "", fmt.Errorf("bws secret get %s: unreadable answer", id)
	}
	return secret.Value, nil
}

// accountTarget is the account a command acts on: the named one (a registry
// name, or anything resolveAccount takes), else the one in effect.
func accountTarget(c *Context, name string) (dir string, entry *claudeAccount, err error) {
	list, err := loadClaudeAccounts()
	if err != nil {
		return "", nil, err
	}
	if name != "" {
		dir, err = resolveAccount(name)
		if err != nil {
			return "", nil, err
		}
	} else {
		dir = effectiveDir(c)
	}
	return dir, claudeAccountForDir(list, dir), nil
}

type envOpts struct {
	name         string
	shell        bool
	requireToken bool
	cmd          []string
}

func parseEnvArgs(args []string, exec bool) (envOpts, error) {
	var o envOpts
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--" && exec:
			o.cmd = args[i+1:]
			return o, nil
		case a == "--shell" && !exec:
			o.shell = true
		case a == "--require-token" && exec:
			o.requireToken = true
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown flag %q", printable(a))
		case o.name == "":
			o.name = a
		default:
			return o, fmt.Errorf("unexpected argument %q", printable(a))
		}
	}
	if exec {
		return o, errors.New("usage: limen account exec [<name>] [--require-token] -- <command> [args…]")
	}
	return o, nil
}

// accountEnv prints what a new Claude Code session needs. The one place a
// token is printed: stdout, for eval or a launcher.
func accountEnv(w io.Writer, c *Context, args []string) error {
	o, err := parseEnvArgs(args, false)
	if err != nil {
		return err
	}
	if err := ensureClaudeAccounts(); err != nil {
		return err
	}
	dir, entry, err := accountTarget(c, o.name)
	if err != nil {
		return err
	}
	token, _, err := resolveClaudeToken(entry, os.Getenv, bwsGet)
	if err != nil {
		return err
	}
	emit := func(k, v string) {
		if o.shell {
			fmt.Fprintf(w, "export %s=%s\n", k, shellQuote(v))
		} else {
			fmt.Fprintf(w, "%s=%s\n", k, v)
		}
	}
	if dir != "" {
		emit("CLAUDE_CONFIG_DIR", dir)
	}
	if token != "" {
		emit(tokenEnv, token)
		if entry != nil && !isHandSetToken(token) {
			emit(tokenMarkerEnv, entry.Name)
		}
	} else if o.shell && os.Getenv(tokenMarkerEnv) != "" {
		fmt.Fprintf(w, "unset %s\nunset %s\n", tokenEnv, tokenMarkerEnv)
	}
	return nil
}

// isHandSetToken: the token is CLAUDE_CODE_OAUTH_TOKEN as the user set it,
// which limen must not claim as its own.
func isHandSetToken(token string) bool {
	return os.Getenv(tokenEnv) == token && os.Getenv(tokenMarkerEnv) == ""
}

// accountExec replaces limen with the command, in the account's environment.
// A token that does not resolve is reported on stderr and the command still
// runs — a terminal must open even when Bitwarden is unreachable — unless
// --require-token says otherwise.
func accountExec(c *Context, args []string) error {
	o, err := parseEnvArgs(args, true)
	if err != nil {
		return err
	}
	if len(o.cmd) == 0 {
		return errors.New("usage: limen account exec [<name>] [--require-token] -- <command> [args…]")
	}
	if err := ensureClaudeAccounts(); err != nil {
		return err
	}
	dir, entry, err := accountTarget(c, o.name)
	if err != nil {
		return err
	}
	token, _, terr := resolveClaudeToken(entry, os.Getenv, bwsGet)
	if terr != nil {
		if o.requireToken {
			return terr
		}
		fmt.Fprintf(os.Stderr, "limen: no token, running without one — %s\n", printable(terr.Error()))
	}
	path, err := exec.LookPath(o.cmd[0])
	if err != nil {
		return err
	}
	set := map[string]string{}
	drop := map[string]bool{}
	if dir != "" {
		set["CLAUDE_CONFIG_DIR"] = dir
	}
	switch {
	case token != "" && entry != nil && !isHandSetToken(token):
		set[tokenEnv] = token
		set[tokenMarkerEnv] = entry.Name
	case token != "":
		set[tokenEnv] = token
	case os.Getenv(tokenMarkerEnv) != "":
		drop[tokenEnv], drop[tokenMarkerEnv] = true, true
	}
	env := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := set[k]; ok || drop[k] {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range set {
		env = append(env, k+"="+v)
	}
	return syscall.Exec(path, o.cmd, env)
}

// shellTokenLines unsets a token limen exported for an account that is no
// longer in effect. Without a marker there is nothing of limen's to clean up,
// and the registry is not even read: this runs on every cd.
func shellTokenLines(w io.Writer, c *Context, override string) {
	marker := os.Getenv(tokenMarkerEnv)
	if marker == "" {
		return
	}
	dir := override
	if c != nil {
		dir = c.ClaudeDir
	}
	list, _ := loadClaudeAccounts()
	if a := claudeAccountForDir(list, dir); a != nil && a.Name == marker {
		return
	}
	fmt.Fprintf(w, "unset %s\nunset %s\n", tokenEnv, tokenMarkerEnv)
}
