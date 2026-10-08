package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/netsandbox"
)

// TestResolveAcceptanceCredentialIsolation_Rows: one row per branch of
// resolveAcceptanceCredentialIsolation (#3792) — the category-C fail reason
// where the branch fails, and otherwise the decision's active / homeIsolated
// / skipReason and the deny flags handed to the profile.
func TestResolveAcceptanceCredentialIsolation_Rows(t *testing.T) {
	homes := []string{"/Users/fh-a", "/Users/fh-b"}
	const sock = "/private/tmp/fh-agent.sock"
	rows := []struct {
		name      string
		mode      string
		environ   []string
		agentID   string
		wantFail  string
		wantLog   []string
		want      acceptanceCredentialIsolation
		wantHomes bool
	}{
		{name: "invalid mode fails config", mode: "strict", agentID: "claude-code",
			environ: []string{"ANTHROPIC_API_KEY=k"}, wantFail: "acceptance_credential_isolation_config",
			wantLog: []string{`"event":"acceptance_credential_isolation_config"`, `"var":"FISHHAWK_ACCEPTANCE_CREDENTIAL_ISOLATION"`}},
		{name: "off is the zero decision", mode: "off", agentID: "claude-code",
			environ: []string{"ANTHROPIC_API_KEY=k"},
			want:    acceptanceCredentialIsolation{mode: netsandbox.ModeOff}},
		{name: "auto with credential isolates the home and denies the keychain", mode: "auto", agentID: "claude-code",
			environ: []string{"CLAUDE_CODE_OAUTH_TOKEN=t"}, wantHomes: true,
			want: acceptanceCredentialIsolation{mode: netsandbox.ModeAuto, active: true, homeIsolated: true,
				deny: netsandbox.CredentialDeny{Files: true, Keychain: true, SSHAgent: true}}},
		{name: "empty mode is auto", mode: "", agentID: "claude-code",
			environ: []string{"ANTHROPIC_API_KEY=k"}, wantHomes: true,
			want: acceptanceCredentialIsolation{mode: netsandbox.ModeAuto, active: true, homeIsolated: true,
				deny: netsandbox.CredentialDeny{Files: true, Keychain: true, SSHAgent: true}}},
		{name: "auto without credential keeps the real home loudly", mode: "auto", agentID: "claude-code",
			environ: []string{"ANTHROPIC_API_KEY="}, wantHomes: true,
			want: acceptanceCredentialIsolation{mode: netsandbox.ModeAuto, active: true,
				skipReason: skipReasonModelCredentialNotEnvCarried,
				deny:       netsandbox.CredentialDeny{Files: true, SSHAgent: true}}},
		{name: "codex with only an anthropic key has no credential", mode: "auto", agentID: "codex",
			environ: []string{"ANTHROPIC_API_KEY=k"}, wantHomes: true,
			want: acceptanceCredentialIsolation{mode: netsandbox.ModeAuto, active: true,
				skipReason: skipReasonModelCredentialNotEnvCarried,
				deny:       netsandbox.CredentialDeny{Files: true, SSHAgent: true}}},
		{name: "require without credential fails naming setup-token", mode: "require", agentID: "claude-code",
			wantFail: "acceptance_credential_isolation_required",
			wantLog:  []string{`"event":"acceptance_credential_isolation_required"`, "claude setup-token", "CLAUDE_CODE_OAUTH_TOKEN"}},
		{name: "require with credential isolates", mode: "require", agentID: "codex",
			environ: []string{"OPENAI_API_KEY=k"}, wantHomes: true,
			want: acceptanceCredentialIsolation{mode: netsandbox.ModeRequire, active: true, homeIsolated: true,
				deny: netsandbox.CredentialDeny{Files: true, Keychain: true, SSHAgent: true}}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			homesCalled := false
			var log strings.Builder
			iso, failReason, failDetail := resolveAcceptanceCredentialIsolation(
				fakeEnv(map[string]string{acceptanceCredentialIsolationEnvVar: r.mode, "SSH_AUTH_SOCK": sock}),
				r.environ, r.agentID,
				func() []string { homesCalled = true; return homes },
				&log)
			for _, want := range r.wantLog {
				if !strings.Contains(log.String(), want) && !strings.Contains(failDetail, want) {
					t.Errorf("log/detail missing %s: log=%s detail=%s", want, log.String(), failDetail)
				}
			}
			if failReason != r.wantFail {
				t.Fatalf("failReason = %q, want %q (detail %q)", failReason, r.wantFail, failDetail)
			}
			if r.wantFail != "" {
				if failDetail == "" {
					t.Error("a failing branch must carry a detail")
				}
				return
			}
			if r.wantHomes {
				r.want.deny.Homes = homes
				r.want.deny.AgentSockets = []string{sock}
			} else if homesCalled {
				t.Error("homes must not be resolved when isolation is inactive")
			}
			if !reflect.DeepEqual(iso, r.want) {
				t.Errorf("decision = %+v\nwant       %+v", iso, r.want)
			}
		})
	}
}

// TestRealHomeDirs_ResolvesSymlinks: $HOME pointing through a symlink is
// reported as its realpath, never as the symlink — Seatbelt matches the
// vnode's own (resolved) path.
func TestRealHomeDirs_ResolvesSymlinks(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "real-home")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "home-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", link)
	got := realHomeDirs()
	if !slices.Contains(got, resolved) {
		t.Errorf("realHomeDirs() = %q, want it to carry the resolved home %q", got, resolved)
	}
	if slices.Contains(got, link) {
		t.Errorf("realHomeDirs() = %q carries the unresolved symlink %q", got, link)
	}
	for _, h := range got {
		if !filepath.IsAbs(h) || filepath.Clean(h) != h {
			t.Errorf("home %q is not canonical", h)
		}
	}

	// An unresolvable $HOME is skipped, not rendered.
	t.Setenv("HOME", filepath.Join(base, "missing"))
	if got := realHomeDirs(); slices.Contains(got, filepath.Join(base, "missing")) {
		t.Errorf("realHomeDirs() = %q carries an unresolvable home", got)
	}

	// A relative $HOME that resolves (it exists under the cwd) is still not
	// canonical: skipped, never handed to the profile.
	t.Chdir(base)
	t.Setenv("HOME", "real-home")
	for _, h := range realHomeDirs() {
		if !filepath.IsAbs(h) {
			t.Errorf("realHomeDirs() carries the relative home %q", h)
		}
	}
}

// TestResolveAgentSocket: SSH_AUTH_SOCK is symlink-resolved, a stale
// (missing) socket falls back to the cleaned path, and unset/relative give
// none.
func TestResolveAgentSocket(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "agent-dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sockFile := filepath.Join(dir, "agent.sock")
	if err := os.WriteFile(sockFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "agent-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(sockFile)
	if err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		name, sock string
		want       []string
	}{
		{"unset", "", nil},
		{"relative", "agent.sock", nil},
		{"through a symlinked dir", filepath.Join(link, "agent.sock"), []string{resolved}},
		{"stale socket falls back to the clean path", base + "/gone//agent.sock", []string{filepath.Join(base, "gone", "agent.sock")}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got := resolveAgentSocket(fakeEnv(map[string]string{"SSH_AUTH_SOCK": r.sock}))
			if !reflect.DeepEqual(got, r.want) {
				t.Errorf("resolveAgentSocket(%q) = %q, want %q", r.sock, got, r.want)
			}
		})
	}
}

// readOnlyTree builds dir/sub/file with sub set 0555, the shape of a Go
// module cache under a synthetic $HOME.
func readOnlyTree(t *testing.T, dir string) {
	t.Helper()
	sub := filepath.Join(dir, "go", "pkg", "mod")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "file"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
}

// TestRemoveAllForce_ReadOnlySubdir: a plain os.RemoveAll cannot remove a
// tree holding a 0555 directory (precondition, on a twin fixture);
// removeAllForce does.
func TestRemoveAllForce_ReadOnlySubdir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions; the precondition cannot hold")
	}
	base := t.TempDir()
	twin := filepath.Join(base, "twin")
	readOnlyTree(t, twin)
	if err := os.RemoveAll(twin); err == nil {
		t.Fatal("precondition: plain os.RemoveAll removed a 0555 tree; the fixture does not exercise the retry")
	}
	dir := filepath.Join(base, "home")
	readOnlyTree(t, dir)
	if err := removeAllForce(dir); err != nil {
		t.Fatalf("removeAllForce: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir survived removeAllForce (stat err %v)", err)
	}
}

// TestCleanupAcceptanceDir_LogsFailure: a removal that cannot succeed (the
// parent is read-only) logs the named event with the dir and never panics;
// a removable dir logs nothing.
func TestCleanupAcceptanceDir_LogsFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	parent := filepath.Join(t.TempDir(), "parent")
	dir := filepath.Join(parent, "fishhawk-acceptance-home-x")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	var log strings.Builder
	cleanupAcceptanceDir(dir, "acceptance_home_cleanup_failed", &log)
	if !strings.Contains(log.String(), `"event":"acceptance_home_cleanup_failed"`) ||
		!strings.Contains(log.String(), dir) {
		t.Errorf("missing acceptance_home_cleanup_failed naming %s: %s", dir, log.String())
	}

	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	log.Reset()
	cleanupAcceptanceDir(dir, "acceptance_home_cleanup_failed", &log)
	if log.Len() != 0 {
		t.Errorf("a successful cleanup logged: %s", log.String())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir survived cleanup (stat err %v)", err)
	}
}
