package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// mcpStub is a recording doctorRunOutput stub: each joined argv is looked up
// in answers (a missing entry errors), and every invocation is recorded.
type mcpStub struct {
	answers map[string]mcpAnswer
	calls   []string
}

type mcpAnswer struct {
	out string
	err error
}

func installMCPStub(t *testing.T, answers map[string]mcpAnswer) *mcpStub {
	t.Helper()
	s := &mcpStub{answers: answers}
	orig := doctorRunOutput
	doctorRunOutput = func(name string, arg ...string) (string, error) {
		key := strings.Join(append([]string{name}, arg...), " ")
		s.calls = append(s.calls, key)
		if a, ok := s.answers[key]; ok {
			return a.out, a.err
		}
		return "", fmt.Errorf("stubbed: %s unavailable", key)
	}
	t.Cleanup(func() { doctorRunOutput = orig })
	return s
}

func (s *mcpStub) called(prefix string) bool {
	for _, c := range s.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// pinGetwd makes registerMCP see root as the cwd.
func pinGetwd(t *testing.T, dir string) {
	t.Helper()
	orig := mcpGetwd
	mcpGetwd = func() (string, error) { return dir, nil }
	t.Cleanup(func() { mcpGetwd = orig })
}

const testBackend = "http://127.0.0.1:8080"
const wantClaudeCmd = "claude mcp add --transport http fishhawk-http http://127.0.0.1:8080/mcp"

var claudeVersion = map[string]mcpAnswer{"claude --version": {out: "2.1.0 (Claude Code)"}}

func withAnswers(extra map[string]mcpAnswer) map[string]mcpAnswer {
	out := map[string]mcpAnswer{}
	for k, v := range claudeVersion {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// TestRegisterMCP_NoCLIPrintsCommandAndSucceeds (m1): no supported CLI —
// init prints the exact command and exits 0.
func TestRegisterMCP_NoCLIPrintsCommandAndSucceeds(t *testing.T) {
	stubDoctorSeams(t)
	stubOrigin(t, "", fmt.Errorf("no origin"))
	stub := installMCPStub(t, nil)
	dir := newInitRepo(t)
	var stdout strings.Builder
	if got := run([]string{"init", "--working-dir", dir, "--backend-url", testBackend}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d\n%s", got, stdout.String())
	}
	if !strings.Contains(stdout.String(), "no supported agent CLI found on PATH (claude, codex); register by running:\n    "+wantClaudeCmd+"\n") {
		t.Errorf("stdout missing the exact command:\n%s", stdout.String())
	}
	if stub.called("claude mcp add") {
		t.Errorf("an add ran with no CLI detected: %v", stub.calls)
	}
}

// TestRegisterMCP_AddFailureWarnsAndInitExitsZero (m2).
func TestRegisterMCP_AddFailureWarnsAndInitExitsZero(t *testing.T) {
	stubDoctorSeams(t)
	stubOrigin(t, "", fmt.Errorf("no origin"))
	dir := newInitRepo(t)
	pinGetwd(t, dir)
	stub := installMCPStub(t, withAnswers(map[string]mcpAnswer{
		"claude mcp list": {out: "other: http://elsewhere/mcp (HTTP)"},
		wantClaudeCmd:     {err: fmt.Errorf("exit status 1")},
	}))
	var stdout strings.Builder
	if got := run([]string{"init", "--working-dir", dir, "--backend-url", testBackend}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK on a failed add\n%s", got, stdout.String())
	}
	if !stub.called(wantClaudeCmd) {
		t.Fatalf("the add was never attempted: %v", stub.calls)
	}
	want := "warn: `" + wantClaudeCmd + "` failed (exit status 1)"
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("stdout missing warn line %q:\n%s", want, stdout.String())
	}
}

// TestRegisterMCP_CodexPrintsCommandAndRunsNoAdd (m3 / approval condition
// 2): Codex detected, Claude Code not — print the CODEX command, never a
// claude one, and execute no add.
func TestRegisterMCP_CodexPrintsCommandAndRunsNoAdd(t *testing.T) {
	stub := installMCPStub(t, map[string]mcpAnswer{"codex --version": {out: "codex-cli 0.153.4"}})
	var buf strings.Builder
	registerMCP(&buf, t.TempDir(), testBackend, false)
	out := buf.String()
	var cmd string
	for _, line := range strings.Split(out, "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "codex ") || strings.HasPrefix(l, "claude ") {
			cmd = l
		}
	}
	if !strings.HasPrefix(cmd, "codex mcp") {
		t.Errorf("printed command = %q, want one starting with `codex mcp`:\n%s", cmd, out)
	}
	if cmd != "codex mcp add fishhawk-http --url http://127.0.0.1:8080/mcp" {
		t.Errorf("codex command = %q", cmd)
	}
	if strings.Contains(out, "claude mcp add") {
		t.Errorf("codex-only branch printed a claude command:\n%s", out)
	}
	if stub.called("codex mcp") || stub.called("claude mcp") {
		t.Errorf("codex-only branch executed an mcp subcommand: %v", stub.calls)
	}
}

// TestRegisterMCP_SkipsWhenAlreadyRegistered (C6): a matching `claude mcp
// list` line means no add is recorded.
func TestRegisterMCP_SkipsWhenAlreadyRegistered(t *testing.T) {
	dir := t.TempDir()
	pinGetwd(t, dir)
	stub := installMCPStub(t, withAnswers(map[string]mcpAnswer{
		"claude mcp list": {out: "fishhawk-http: http://127.0.0.1:8080/mcp (HTTP)"},
	}))
	var buf strings.Builder
	registerMCP(&buf, dir, testBackend, false)
	if stub.called("claude mcp add") {
		t.Errorf("an add ran over an existing registration: %v", stub.calls)
	}
	if !strings.Contains(buf.String(), "already registered: fishhawk-http (HTTP)") || !strings.Contains(buf.String(), wantClaudeCmd) {
		t.Errorf("output = %q", buf.String())
	}
}

// TestRegisterMCP_GetFallbackAlreadyRegistered: `claude mcp list` broken but
// `claude mcp get` resolves — no add.
func TestRegisterMCP_GetFallbackAlreadyRegistered(t *testing.T) {
	dir := t.TempDir()
	pinGetwd(t, dir)
	stub := installMCPStub(t, withAnswers(map[string]mcpAnswer{"claude mcp get fishhawk": {out: "ok"}}))
	var buf strings.Builder
	registerMCP(&buf, dir, testBackend, false)
	if stub.called("claude mcp add") {
		t.Errorf("an add ran over a get-fallback registration: %v", stub.calls)
	}
	if !strings.Contains(buf.String(), "already registered (fishhawk, via `claude mcp get`)") {
		t.Errorf("output = %q", buf.String())
	}
}

func TestRegisterMCP_RegistersWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	pinGetwd(t, dir)
	stub := installMCPStub(t, withAnswers(map[string]mcpAnswer{
		"claude mcp list": {out: ""},
		wantClaudeCmd:     {out: "Added HTTP MCP server fishhawk-http"},
	}))
	var buf strings.Builder
	registerMCP(&buf, dir, testBackend+"/", false)
	if !stub.called(wantClaudeCmd) {
		t.Errorf("add not run with the exact argv: %v", stub.calls)
	}
	if !strings.Contains(buf.String(), "registered fishhawk-http with Claude Code:\n    "+wantClaudeCmd) {
		t.Errorf("output = %q", buf.String())
	}
}

// TestRegisterMCP_CwdNotRootPrintsOnly: Claude Code's default scope is keyed
// to the cwd, so an add from another directory is printed, not run.
func TestRegisterMCP_CwdNotRootPrintsOnly(t *testing.T) {
	root := t.TempDir()
	pinGetwd(t, t.TempDir())
	stub := installMCPStub(t, withAnswers(map[string]mcpAnswer{"claude mcp list": {out: ""}}))
	var buf strings.Builder
	registerMCP(&buf, root, testBackend, false)
	if stub.called("claude mcp add") {
		t.Errorf("an add ran from a different directory: %v", stub.calls)
	}
	if !strings.Contains(buf.String(), "cd "+root+" && "+wantClaudeCmd) {
		t.Errorf("output = %q", buf.String())
	}
}

func TestRegisterMCP_SkipFlagRunsNothing(t *testing.T) {
	stubDoctorSeams(t)
	stubOrigin(t, "", fmt.Errorf("no origin"))
	stub := installMCPStub(t, claudeVersion)
	dir := newInitRepo(t)
	var stdout strings.Builder
	if got := run([]string{"init", "--working-dir", dir, "--backend-url", testBackend, "--skip-mcp-register"}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d", got)
	}
	if !strings.Contains(stdout.String(), "skipped (--skip-mcp-register); to register by hand run:\n    "+wantClaudeCmd) {
		t.Errorf("stdout = %s", stdout.String())
	}
	// The closing doctor preflight probes `claude mcp list`; registration
	// itself must not have run --version or add.
	if stub.called("claude --version") || stub.called("claude mcp add") {
		t.Errorf("--skip-mcp-register still probed/added: %v", stub.calls)
	}
}

// TestRegisterMCP_URLFollowsBackendURLFlagAndEnv (approval condition 3):
// init's --backend-url (a common flag, defaulting to $FISHHAWK_BACKEND_URL
// then http://localhost:8080 like every other command) sets the URL.
func TestRegisterMCP_URLFollowsBackendURLFlagAndEnv(t *testing.T) {
	for name, c := range map[string]struct {
		env  string
		args []string
		want string
	}{
		"default": {"", nil, "http://localhost:8080/mcp"},
		"env":     {"https://fh.example.test", nil, "https://fh.example.test/mcp"},
		"flag":    {"https://fh.example.test", []string{"--backend-url", "http://10.0.0.5:9000"}, "http://10.0.0.5:9000/mcp"},
	} {
		t.Run(name, func(t *testing.T) {
			stubDoctorSeams(t)
			stubOrigin(t, "", fmt.Errorf("no origin"))
			t.Setenv("FISHHAWK_BACKEND_URL", c.env)
			dir := newInitRepo(t)
			var stdout strings.Builder
			args := append([]string{"init", "--working-dir", dir, "--skip-mcp-register"}, c.args...)
			if got := run(args, &stdout, io.Discard); got != exitOK {
				t.Fatalf("status = %d", got)
			}
			if !strings.Contains(stdout.String(), "claude mcp add --transport http fishhawk-http "+c.want+"\n") {
				t.Errorf("registration command does not use %s:\n%s", c.want, stdout.String())
			}
		})
	}
}

func TestSameDir(t *testing.T) {
	dir := t.TempDir()
	if !sameDir(func() (string, error) { return dir, nil }, dir) {
		t.Error("sameDir(dir, dir) = false")
	}
	if sameDir(func() (string, error) { return "", fmt.Errorf("boom") }, dir) {
		t.Error("sameDir with a getwd error = true")
	}
	if sameDir(func() (string, error) { return dir, nil }, dir+string(os.PathSeparator)+"missing") {
		t.Error("sameDir with an unresolvable dir = true")
	}
}

// TestShellQuote pins the copy-pasteable-command contract: an ordinary word
// stays bare, and anything a shell would split or interpret is wrapped in
// single quotes that survive the paste as the literal value.
func TestShellQuote(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"claude", "claude"},
		{"--transport", "--transport"},
		{"http://127.0.0.1:8080/mcp", "http://127.0.0.1:8080/mcp"},
		{"/tmp/my repo", `'/tmp/my repo'`},
		{"/tmp/$(touch pwned)", `'/tmp/$(touch pwned)'`},
		{"/tmp/a;rm -rf /", `'/tmp/a;rm -rf /'`},
		{"/tmp/it's", `'/tmp/it'\''s'`},
		{"http://h/?a=1&b=2", `'http://h/?a=1&b=2'`},
		{"", "''"},
	} {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRegisterMCP_QuotesSpacesAndMetacharacters (concern: the documented
// --working-dir flow supports directory names with spaces, and the backend
// URL is operator-supplied too). The printed command must parse back into
// the INTENDED words, so the assertion runs it through a real shell parse
// (`printf %s\n` over the argv) rather than matching a substring.
func TestRegisterMCP_QuotesSpacesAndMetacharacters(t *testing.T) {
	root := filepath.Join(t.TempDir(), "my repo; touch pwned")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	pinGetwd(t, t.TempDir())
	installMCPStub(t, withAnswers(nil))
	backend := "http://127.0.0.1:8080/a b"
	var buf strings.Builder
	registerMCP(&buf, root, backend, false)

	line := ""
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "cd ") {
			line = strings.TrimSpace(l)
		}
	}
	if line == "" {
		t.Fatalf("no cd line printed:\n%s", buf.String())
	}
	// Ask a real shell what words the printed line expands to. A root
	// that is not one argument, or a metacharacter that executes, shows
	// up here and nowhere in a substring match.
	script := strings.Replace(line, "&& claude ", "&& set -- ", 1) + `; printf '%s\n' "$(pwd -P)" "$@"`
	sh := exec.Command("/bin/sh", "-c", script)
	// Run from a scratch dir: if the control regresses, the unquoted
	// metacharacter EXECUTES, and its side effect must land outside the
	// repository (this test observed it create a file in the package
	// directory before cmd.Dir was pinned).
	sh.Dir = t.TempDir()
	out, err := sh.CombinedOutput()
	if err != nil {
		t.Fatalf("printed command does not parse (%v):\n%s\n%s", err, line, out)
	}
	got := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	wantRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string{wantRoot}, claudeMCPAddArgs(backend)...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("printed command expands to %q, want %q\n%s", got, want, line)
	}
	if _, err := os.Stat(filepath.Join(root, "pwned")); err == nil {
		t.Errorf("the printed command executed the metacharacter in the root name")
	}
}

// TestRegisterMCP_CwdNotRootWithMatchingRegistrationDoesNotClaimRegistered
// (concern: registerMCP used to consult `claude mcp list` BEFORE checking
// the cwd, so a registration belonging to ANOTHER directory's scope was
// reported as the target repository being registered). With the cwd check
// first, the directory-scoped probe is not even consulted from the wrong
// directory, and the output says so.
func TestRegisterMCP_CwdNotRootWithMatchingRegistrationDoesNotClaimRegistered(t *testing.T) {
	root := t.TempDir()
	pinGetwd(t, t.TempDir())
	stub := installMCPStub(t, withAnswers(map[string]mcpAnswer{
		"claude mcp list":         {out: "fishhawk-http: http://127.0.0.1:8080/mcp (HTTP)"},
		"claude mcp get fishhawk": {out: "ok"},
	}))
	var buf strings.Builder
	registerMCP(&buf, root, testBackend, false)
	out := buf.String()
	if strings.Contains(out, "already registered") {
		t.Errorf("a registration in ANOTHER directory's scope was reported as the target repository's:\n%s", out)
	}
	if !strings.Contains(out, "cd "+root+" && "+wantClaudeCmd) {
		t.Errorf("output = %q", out)
	}
	if stub.called("claude mcp list") || stub.called("claude mcp get") {
		t.Errorf("a directory-scoped registration probe ran from the wrong directory: %v", stub.calls)
	}
	if stub.called("claude mcp add") {
		t.Errorf("an add ran from a different directory: %v", stub.calls)
	}
}
