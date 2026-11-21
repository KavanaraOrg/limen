package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A fake token. Real ones never appear in tests; this one exists so a test can
// prove the value does not leak where it must not.
const fakeToken = "sk-ant-oat01-FAKEFAKEFAKE_test-token"

const sampleRegistry = `# comment
accounts:
  - name: work
    label: Claude-Work
    token: bws:22bc1c26-dc65-407d-b057-b509011ac1c5
    configDir: ~/.claude-a
  - name: side
    label: "Claude-Side"
    token: env:SIDE_TOKEN
`

func TestRegistryParsesNameLabelTokenAndConfigDir(t *testing.T) {
	list, err := parseClaudeAccounts(sampleRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 accounts, got %+v", list)
	}
	w := list[0]
	if w.Name != "work" || w.Label != "Claude-Work" || w.Token != "bws:22bc1c26-dc65-407d-b057-b509011ac1c5" || w.ConfigDir != "~/.claude-a" {
		t.Errorf("first entry parsed wrong: %+v", w)
	}
	if w.sourceKind() != "bws" || list[1].sourceKind() != "env" {
		t.Errorf("source kinds: %s, %s", w.sourceKind(), list[1].sourceKind())
	}
	if list[1].Label != "Claude-Side" || list[1].ConfigDir != "" {
		t.Errorf("second entry parsed wrong: %+v", list[1])
	}
}

func TestRegistryRejectsBadEntries(t *testing.T) {
	cases := map[string]string{
		"no token":        "accounts:\n  - name: a\n",
		"no name":         "accounts:\n  - token: env:X\n",
		"bad source":      "accounts:\n  - name: a\n    token: file:/x\n",
		"bad uuid":        "accounts:\n  - name: a\n    token: bws:not-a-uuid\n",
		"bad env var":     "accounts:\n  - name: a\n    token: env:1-x\n",
		"duplicate":       "accounts:\n  - name: a\n    token: env:X\n  - name: a\n    token: env:Y\n",
		"reserved name":   "accounts:\n  - name: default\n    token: env:X\n",
		"relative dir":    "accounts:\n  - name: a\n    token: env:X\n    configDir: rel/dir\n",
		"unknown key":     "accounts:\n  - name: a\n    token: env:X\n    colour: red\n",
		"key outside one": "accounts:\n    name: a\n",
	}
	for name, body := range cases {
		if _, err := parseClaudeAccounts(body); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// Someone pastes the token itself into the registry. The registry must refuse,
// and the refusal must not repeat the value into a terminal or a log.
func TestRegistryRefusesARawTokenWithoutEchoingIt(t *testing.T) {
	_, err := parseClaudeAccounts("accounts:\n  - name: a\n    token: " + fakeToken + "\n")
	if err == nil {
		t.Fatal("a raw token in the registry must be refused")
	}
	if strings.Contains(err.Error(), "FAKEFAKE") {
		t.Errorf("the error repeats the token: %v", err)
	}
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func bwsReturning(v string, calls *int) func(string) (string, error) {
	return func(id string) (string, error) {
		*calls++
		return v, nil
	}
}

func TestTokenPrecedenceExplicitEnvBeatsBws(t *testing.T) {
	work := claudeAccount{Name: "work", Token: "bws:22bc1c26-dc65-407d-b057-b509011ac1c5"}
	calls := 0
	tok, src, err := resolveClaudeToken(&work, envOf(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": fakeToken}), bwsReturning("sk-ant-oat01-other", &calls))
	if err != nil || tok != fakeToken || src != "env" || calls != 0 {
		t.Errorf("explicit env token must win without calling bws: %q %q %v calls=%d", tok, src, err, calls)
	}
}

func TestTokenPrecedenceBwsWhenNoEnvToken(t *testing.T) {
	work := claudeAccount{Name: "work", Token: "bws:22bc1c26-dc65-407d-b057-b509011ac1c5"}
	calls := 0
	tok, src, err := resolveClaudeToken(&work, envOf(nil), bwsReturning(fakeToken, &calls))
	if err != nil || tok != fakeToken || src != "bws" || calls != 1 {
		t.Errorf("bws must answer: %q %q %v calls=%d", tok, src, err, calls)
	}
}

func TestTokenPrecedenceNoneWithoutAnAccount(t *testing.T) {
	calls := 0
	tok, src, err := resolveClaudeToken(nil, envOf(nil), bwsReturning(fakeToken, &calls))
	if err != nil || tok != "" || src != "none" || calls != 0 {
		t.Errorf("no account, no token: %q %q %v calls=%d", tok, src, err, calls)
	}
}

// A token limen exported for another account is not an explicit choice: after
// a switch it would run the new config directory under the old account.
func TestATokenLimenSetForAnotherAccountIsNotExplicit(t *testing.T) {
	work := claudeAccount{Name: "work", Token: "bws:22bc1c26-dc65-407d-b057-b509011ac1c5"}
	calls := 0
	env := envOf(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-old", "LIMEN_ACCOUNT_TOKEN_FOR": "side"})
	tok, src, _ := resolveClaudeToken(&work, env, bwsReturning(fakeToken, &calls))
	if tok != fakeToken || src != "bws" {
		t.Errorf("a token marked for side must not serve work: %q %q", tok, src)
	}
	// Marked for this very account, it is reused and bws stays idle.
	calls = 0
	env = envOf(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": fakeToken, "LIMEN_ACCOUNT_TOKEN_FOR": "work"})
	if tok, _, _ := resolveClaudeToken(&work, env, bwsReturning("x", &calls)); tok != fakeToken || calls != 0 {
		t.Errorf("a token marked for work is reused: %q calls=%d", tok, calls)
	}
}

func TestTokenFromTheEntrysEnvVarAndTheAccountsOwnVar(t *testing.T) {
	side := claudeAccount{Name: "side", Token: "env:SIDE_TOKEN"}
	if tok, src, err := resolveClaudeToken(&side, envOf(map[string]string{"SIDE_TOKEN": fakeToken}), nil); err != nil || tok != fakeToken || src != "env" {
		t.Errorf("env:SIDE_TOKEN: %q %q %v", tok, src, err)
	}
	if _, _, err := resolveClaudeToken(&side, envOf(nil), nil); err == nil {
		t.Error("an empty env source is an error")
	}
	// A bws account whose token already lies in LIMEN_CLAUDE_TOKEN_<NAME>
	// needs no Bitwarden round trip.
	work := claudeAccount{Name: "my-work", Token: "bws:22bc1c26-dc65-407d-b057-b509011ac1c5"}
	calls := 0
	tok, src, _ := resolveClaudeToken(&work, envOf(map[string]string{"LIMEN_CLAUDE_TOKEN_MY_WORK": fakeToken}), bwsReturning("x", &calls))
	if tok != fakeToken || src != "env" || calls != 0 {
		t.Errorf("LIMEN_CLAUDE_TOKEN_MY_WORK: %q %q calls=%d", tok, src, calls)
	}
}

func TestATokenOfTheWrongShapeIsRefusedWithoutEchoingIt(t *testing.T) {
	work := claudeAccount{Name: "work", Token: "bws:22bc1c26-dc65-407d-b057-b509011ac1c5"}
	calls := 0
	_, _, err := resolveClaudeToken(&work, envOf(nil), bwsReturning("hunter2 'secret", &calls))
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("a value that is no sk-ant- token must be refused without echo: %v", err)
	}
}

func TestBwsAccessTokenComesFromEnvThenKeychain(t *testing.T) {
	var asked [2]string
	lookup := func(service, account string) (string, bool) {
		asked = [2]string{service, account}
		return "from-keychain", true
	}
	if v, err := bwsAccessToken(envOf(map[string]string{"BWS_ACCESS_TOKEN": "from-env", "USER": "me"}), lookup); err != nil || v != "from-env" {
		t.Errorf("env first: %q %v", v, err)
	}
	if v, err := bwsAccessToken(envOf(map[string]string{"USER": "me"}), lookup); err != nil || v != "from-keychain" || asked != [2]string{"BWS_ACCESS_TOKEN", "me"} {
		t.Errorf("keychain second: %q %v asked %v", v, err, asked)
	}
	none := func(string, string) (string, bool) { return "", false }
	_, err := bwsAccessToken(envOf(map[string]string{"USER": "me"}), none)
	if err == nil || !strings.Contains(err.Error(), "BWS_ACCESS_TOKEN") || !strings.Contains(err.Error(), "keychain") {
		t.Errorf("the error must name both places: %v", err)
	}
}

func TestBwsErrorsCarryNoSecret(t *testing.T) {
	msg := bwsErrorText(errors.New("exit status 1"), "boom: "+fakeToken+" and bwsaccess", "bwsaccess")
	if strings.Contains(msg, "FAKEFAKE") || strings.Contains(msg, "bwsaccess") {
		t.Errorf("bws error leaks a secret: %s", msg)
	}
}

// --- CLI: the token reaches stdout of `account env` and the exec'd process,
// and nowhere else.

// fakeBws writes a stand-in for the bws CLI that answers one secret id with the
// fake token, and refuses without an access token as the real one does.
func fakeBws(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "bws")
	script := `#!/bin/sh
[ -n "$BWS_ACCESS_TOKEN" ] || { echo "missing access token" >&2; exit 1; }
[ "$1 $2" = "secret get" ] || exit 2
case "$3" in
22bc1c26-dc65-407d-b057-b509011ac1c5) printf '{"id":"%s","key":"k","value":"` + fakeToken + `"}\n' "$3" ;;
*) echo "leaky failure ` + fakeToken + `" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// tokenHome is accountHome plus a registry and a fake bws.
func tokenHome(t *testing.T) (home string, env []string) {
	home, env = accountHome(t)
	reg := `accounts:
  - name: work
    label: Claude-Work
    token: bws:22bc1c26-dc65-407d-b057-b509011ac1c5
    configDir: ~/.claude-a
  - name: broken
    label: Claude-Broken
    token: bws:d53b0712-a922-48cf-9a9f-b509011a9f99
`
	write(t, filepath.Join(home, ".config", "limen", "claude-accounts.yaml"), reg)
	env = append(env, "LIMEN_BWS_BIN="+fakeBws(t, home), "BWS_ACCESS_TOKEN=test-access")
	return home, env
}

func TestAccountEnvPrintsConfigDirAndTokenForTheAccountInEffect(t *testing.T) {
	home, env := tokenHome(t)
	root := accountProject(t, home) // names ~/.claude-a, which is "work"
	a := filepath.Join(home, ".claude-a")

	r := mustRun(t, root, env, "account", "env", "--shell")
	for _, want := range []string{
		"export CLAUDE_CONFIG_DIR=" + shellQuote(a) + "\n",
		"export CLAUDE_CODE_OAUTH_TOKEN=" + shellQuote(fakeToken) + "\n",
		"export LIMEN_ACCOUNT_TOKEN_FOR='work'\n",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("missing %q in:\n%s", want, r.stdout)
		}
	}
	plain := mustRun(t, root, env, "account", "env").stdout
	if !strings.Contains(plain, "CLAUDE_CODE_OAUTH_TOKEN="+fakeToken+"\n") || strings.Contains(plain, "export") {
		t.Errorf("without --shell the lines are KEY=value:\n%s", plain)
	}
}

func TestAccountUseTakesARegistryName(t *testing.T) {
	home, env := tokenHome(t)
	dir := filepath.Join(home, "elsewhere")
	os.MkdirAll(dir, 0o755)

	mustRun(t, dir, env, "account", "use", "broken")
	want := filepath.Join(home, ".claude-broken")
	if st, err := os.Stat(want); err != nil || !st.IsDir() {
		t.Fatalf("use of a registry account creates its config dir: %v", err)
	}
	r := mustRun(t, dir, env, "account")
	if !strings.Contains(r.stdout, want) || !strings.Contains(r.stdout, "override") {
		t.Errorf("the override is the registry account's dir:\n%s", r.stdout)
	}
}

func TestATokenFailureNamesNoSecret(t *testing.T) {
	home, env := tokenHome(t)
	dir := filepath.Join(home, "elsewhere")
	os.MkdirAll(dir, 0o755)
	r := runLimen(t, dir, env, "account", "env", "broken")
	if r.code == 0 {
		t.Fatalf("a failing bws must fail account env:\n%s", r.stdout)
	}
	if strings.Contains(r.stderr+r.stdout, "FAKEFAKE") || strings.Contains(r.stderr, "test-access") {
		t.Errorf("an error leaks a secret:\n%s\n%s", r.stdout, r.stderr)
	}
}

func TestTheTokenNeverAppearsInJsonListOrShell(t *testing.T) {
	home, env := tokenHome(t)
	root := accountProject(t, home)
	env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+fakeToken)

	j := mustRun(t, root, env, "json").stdout
	l := mustRun(t, root, env, "account", "list").stdout
	lj := mustRun(t, root, env, "account", "list", "--json").stdout
	sh := mustRun(t, root, env, "shell").stdout
	st := mustRun(t, root, env, "account").stdout
	for name, out := range map[string]string{"json": j, "list": l, "list --json": lj, "shell": sh, "account": st} {
		if strings.Contains(out, "FAKEFAKE") {
			t.Errorf("%s leaks the token:\n%s", name, out)
		}
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(j), &v); err != nil {
		t.Fatal(err)
	}
	if v["token_source"] != "env" || v["claude_account"] != "work" {
		t.Errorf("json names the source and the account, not the token: %v %v", v["token_source"], v["claude_account"])
	}
	for _, want := range []string{"work", "Claude-Work", "bws", "broken"} {
		if !strings.Contains(l, want) {
			t.Errorf("list misses %q:\n%s", want, l)
		}
	}
}

func TestJsonTokenSourceIsBwsWithoutCallingIt(t *testing.T) {
	home, env := tokenHome(t)
	root := accountProject(t, home)
	// A bws that would fail: json must not call it at all.
	env = append(env, "LIMEN_BWS_BIN=/nonexistent/bws")
	var v map[string]any
	if err := json.Unmarshal([]byte(mustRun(t, root, env, "json").stdout), &v); err != nil {
		t.Fatal(err)
	}
	if v["token_source"] != "bws" {
		t.Errorf("token_source = %v, want bws", v["token_source"])
	}
}

func TestAccountExecSetsTheEnvironmentAndRunsTheCommand(t *testing.T) {
	home, env := tokenHome(t)
	root := accountProject(t, home)
	r := mustRun(t, root, env, "account", "exec", "--", "/usr/bin/env")
	for _, want := range []string{
		"CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude-a") + "\n",
		"CLAUDE_CODE_OAUTH_TOKEN=" + fakeToken + "\n",
		"LIMEN_ACCOUNT_TOKEN_FOR=work\n",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("exec'd env misses %q:\n%s", want, r.stdout)
		}
	}
	// A failing token source does not keep a terminal from opening: the
	// command still runs, without a token, and the reason goes to stderr.
	r = mustRun(t, root, env, "account", "exec", "broken", "--", "/usr/bin/env")
	if strings.Contains(r.stdout, "CLAUDE_CODE_OAUTH_TOKEN") || !strings.Contains(r.stderr, "no token") {
		t.Errorf("lenient exec:\nstdout %s\nstderr %s", r.stdout, r.stderr)
	}
	if r := runLimen(t, root, env, "account", "exec", "broken", "--require-token", "--", "/usr/bin/env"); r.code == 0 {
		t.Error("--require-token must fail when no token resolves")
	}
}

// After a switch the hook must not leave the old account's token in the shell.
func TestShellDropsATokenLimenSetForAnotherAccount(t *testing.T) {
	home, env := tokenHome(t)
	root := accountProject(t, home)
	env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+fakeToken, "LIMEN_ACCOUNT_TOKEN_FOR=broken")
	r := mustRun(t, root, env, "shell")
	if !strings.Contains(r.stdout, "unset CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("a token marked for another account must be unset:\n%s", r.stdout)
	}
	env[len(env)-1] = "LIMEN_ACCOUNT_TOKEN_FOR=work"
	if r := mustRun(t, root, env, "shell"); strings.Contains(r.stdout, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("the token of the account in effect stays:\n%s", r.stdout)
	}
}

func TestHookExportsTheNewAccountsTokenAfterUse(t *testing.T) {
	home, env := tokenHome(t)
	dir := filepath.Join(home, "elsewhere")
	os.MkdirAll(dir, 0o755)
	script := `eval "$(limen hook bash)"
limen account use work >/dev/null; echo "tok=${CLAUDE_CODE_OAUTH_TOKEN:+set} for=$LIMEN_ACCOUNT_TOKEN_FOR"
limen account clear >/dev/null; echo "after-clear=${CLAUDE_CODE_OAUTH_TOKEN:+set}|"
`
	cmd := exec.Command("bash", "--noprofile", "--norc", "-c", script)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + filepath.Dir(binPath) + ":" + os.Getenv("PATH")}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"tok=set for=work", "after-clear=|"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestAccountCommandsCreateAPrivateRegistryTemplate(t *testing.T) {
	home, env := accountHome(t)
	mustRun(t, home, env, "account", "list")
	path := filepath.Join(home, ".config", "limen", "claude-accounts.yaml")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("account list creates the registry: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("registry mode %v, want 0600", st.Mode().Perm())
	}
	body, _ := os.ReadFile(path)
	if list, err := parseClaudeAccounts(string(body)); err != nil || len(list) != 0 {
		t.Errorf("the template parses to no accounts: %v %v", list, err)
	}
}
