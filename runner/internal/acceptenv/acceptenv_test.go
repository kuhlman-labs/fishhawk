package acceptenv_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/acceptenv"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

const proxy = "http://127.0.0.1:39999"

func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("malformed env entry %q", kv)
		}
		m[k] = v
	}
	return m
}

// TestEnv_PostureTable pins the full ADR-050 decision-#2 posture in one
// table: what survives, what is injected, and what can never appear. It runs
// with and without WithCredentialIsolation: the option changes only the
// passthrough refusal set, so the posture (and CLAUDE_CODE_OAUTH_TOKEN's
// admission, #3792) is identical under both.
func TestEnv_PostureTable(t *testing.T) {
	for _, iso := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_isolation", true: "credential_isolation"}[iso], func(t *testing.T) {
			var opts []acceptenv.EnvOption
			if iso {
				opts = append(opts, acceptenv.WithCredentialIsolation())
			}
			testEnvPosture(t, opts...)
		})
	}
}

func testEnvPosture(t *testing.T, opts ...acceptenv.EnvOption) {
	base := []string{
		"PATH=/usr/bin",
		"HOME=/home/runner",
		"LC_ALL=en_US.UTF-8",
		"ANTHROPIC_API_KEY=model-key",                  // model key: the one surviving secret class
		"CLAUDE_CODE_OAUTH_TOKEN=oauth-token",          // env-carried subscription credential (#3792)
		"OPENAI_API_KEY=other-model-key",               // second model provider
		"FISHHAWK_API_TOKEN=fhm-secret",                // MCP token: NEVER present (ADR-050: no token leg)
		"FISHHAWK_GITHUB_TOKEN=ghs-xxx",                // repo write: denied
		"FISHHAWK_GITLAB_TOKEN=glpat-xxx",              // repo write (gitlab): denied
		"GITHUB_TOKEN=ghs-yyy",                         // repo write: denied
		"GH_TOKEN=ghs-zzz",                             // repo write: denied
		"AWS_SECRET_ACCESS_KEY=aws-shh",                // arbitrary secret: dropped by default-deny
		"FISHHAWK_RUNNER_INTERNAL=x",                   // runner internals: dropped
		"FISHHAWK_ACCEPTANCE_ENV_APP_USER=tester",      // target cred passthrough
		"FISHHAWK_ACCEPTANCE_ENV_APP_PASSWORD=hunter2", // target cred passthrough
	}
	env, refused := acceptenv.Env(base, proxy, opts...)
	if len(refused) != 0 {
		t.Fatalf("refused = %v, want none", refused)
	}
	m := envMap(t, env)

	present := map[string]string{
		"PATH":                    "/usr/bin",
		"HOME":                    "/home/runner",
		"LC_ALL":                  "en_US.UTF-8",
		"ANTHROPIC_API_KEY":       "model-key",
		"CLAUDE_CODE_OAUTH_TOKEN": "oauth-token",
		"OPENAI_API_KEY":          "other-model-key",
		"APP_USER":                "tester",
		"APP_PASSWORD":            "hunter2",
		"HTTPS_PROXY":             proxy,
		"HTTP_PROXY":              proxy,
		"ALL_PROXY":               proxy,
		"https_proxy":             proxy,
		"http_proxy":              proxy,
		"all_proxy":               proxy,
		"NO_PROXY":                "",
		"no_proxy":                "",
		// E72.13 / #3500: the forge-writes deny every descendant runner
		// inherits, injected as a fixed value.
		acceptenv.ForgeWritesVar: acceptenv.ForgeWritesDeny,
	}
	for k, want := range present {
		if got, ok := m[k]; !ok || got != want {
			t.Errorf("%s = %q (present=%v), want %q", k, got, ok, want)
		}
	}
	for _, banned := range []string{
		"FISHHAWK_API_TOKEN", "FISHHAWK_GITHUB_TOKEN", "FISHHAWK_GITLAB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN",
		"AWS_SECRET_ACCESS_KEY", "FISHHAWK_RUNNER_INTERNAL",
		"FISHHAWK_ACCEPTANCE_ENV_APP_USER", // the prefixed form must not leak alongside the stripped one
	} {
		if _, ok := m[banned]; ok {
			t.Errorf("%s present on the acceptance invocation env, must never be", banned)
		}
	}
	if len(m) != len(present) {
		t.Errorf("env has %d entries, want exactly the %d pinned ones — extras: %v", len(m), len(present), env)
	}
}

// TestEnv_PassthroughCannotResurrectDeniedKeys proves the deny set outranks
// the operator passthrough: FISHHAWK_ACCEPTANCE_ENV_GITHUB_TOKEN is refused
// and reported, not honored.
func TestEnv_PassthroughCannotResurrectDeniedKeys(t *testing.T) {
	base := []string{
		"FISHHAWK_ACCEPTANCE_ENV_GITHUB_TOKEN=smuggled",
		"FISHHAWK_ACCEPTANCE_ENV_FISHHAWK_API_TOKEN=smuggled-too",
		"FISHHAWK_ACCEPTANCE_ENV_OK_VALUE=fine",
	}
	env, refused := acceptenv.Env(base, proxy)
	m := envMap(t, env)
	if _, ok := m["GITHUB_TOKEN"]; ok {
		t.Error("GITHUB_TOKEN resurrected via passthrough")
	}
	if _, ok := m["FISHHAWK_API_TOKEN"]; ok {
		t.Error("FISHHAWK_API_TOKEN resurrected via passthrough")
	}
	if got := m["OK_VALUE"]; got != "fine" {
		t.Errorf("OK_VALUE = %q, want %q", got, "fine")
	}
	for _, want := range []string{"FISHHAWK_API_TOKEN", "GITHUB_TOKEN"} {
		if !slices.Contains(refused, want) {
			t.Errorf("refused = %v, want it to name %s", refused, want)
		}
	}
}

// TestEnv_PassthroughCannotRepointProxy proves the containment vars are not
// passthrough-overridable: an attempt to set HTTPS_PROXY (any case) through
// the credential channel is refused and the proxy value stands.
func TestEnv_PassthroughCannotRepointProxy(t *testing.T) {
	base := []string{
		"FISHHAWK_ACCEPTANCE_ENV_HTTPS_PROXY=http://evil.example.test:1",
		"FISHHAWK_ACCEPTANCE_ENV_no_proxy=*",
	}
	env, refused := acceptenv.Env(base, proxy)
	m := envMap(t, env)
	if got := m["HTTPS_PROXY"]; got != proxy {
		t.Errorf("HTTPS_PROXY = %q, want the egress proxy %q", got, proxy)
	}
	if got := m["no_proxy"]; got != "" {
		t.Errorf("no_proxy = %q, want cleared", got)
	}
	if len(refused) != 2 {
		t.Errorf("refused = %v, want both proxy-var passthroughs named", refused)
	}
}

// TestEnv_InjectsForgeWritesDeny (E72.13 / #3500) pins that the acceptance
// env carries EXACTLY ONE FISHHAWK_FORGE_WRITES entry and that its value is
// "deny" — the fixed injection a descendant fishhawk-runner's pre-spawn
// gate reads. A bare base env (nothing to pass through) is the minimal
// vehicle: the entry comes from the composer, not from the base.
func TestEnv_InjectsForgeWritesDeny(t *testing.T) {
	env, refused := acceptenv.Env([]string{"PATH=/usr/bin"}, proxy)
	if len(refused) != 0 {
		t.Fatalf("refused = %v, want none", refused)
	}
	var got []string
	for _, kv := range env {
		if strings.HasPrefix(kv, acceptenv.ForgeWritesVar+"=") {
			got = append(got, kv)
		}
	}
	want := []string{acceptenv.ForgeWritesVar + "=" + acceptenv.ForgeWritesDeny}
	if !slices.Equal(got, want) {
		t.Fatalf("forge-writes entries = %v, want exactly %v", got, want)
	}
}

// TestEnv_RefusesForgeWritesPassthrough (E72.13 / #3500) proves the deny is
// not re-pointable through the credential channel: a passthrough named
// FISHHAWK_FORGE_WRITES (any case) is refused and reported, and the env
// still carries the single injected deny.
func TestEnv_RefusesForgeWritesPassthrough(t *testing.T) {
	base := []string{
		"FISHHAWK_ACCEPTANCE_ENV_FISHHAWK_FORGE_WRITES=allow",
		"FISHHAWK_ACCEPTANCE_ENV_fishhawk_forge_writes=allow",
	}
	env, refused := acceptenv.Env(base, proxy)
	if !slices.Contains(refused, "FISHHAWK_FORGE_WRITES") || !slices.Contains(refused, "fishhawk_forge_writes") {
		t.Errorf("refused = %v, want both forge-writes passthroughs named", refused)
	}
	m := envMap(t, env)
	if got := m[acceptenv.ForgeWritesVar]; got != acceptenv.ForgeWritesDeny {
		t.Errorf("%s = %q, want %q (passthrough must not re-point it)", acceptenv.ForgeWritesVar, got, acceptenv.ForgeWritesDeny)
	}
	if _, ok := m["fishhawk_forge_writes"]; ok {
		t.Error("lower-case forge-writes passthrough leaked onto the env")
	}
	count := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, acceptenv.ForgeWritesVar+"=") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("FISHHAWK_FORGE_WRITES entries = %d, want exactly 1", count)
	}
}

// TestEnv_DropsBaseForgeWritesValue (E72.13 / #3500) proves a base-env
// FISHHAWK_FORGE_WRITES=allow never survives: the allow-list drops it and
// the composer's fixed deny is the only entry under that name.
func TestEnv_DropsBaseForgeWritesValue(t *testing.T) {
	env, refused := acceptenv.Env([]string{acceptenv.ForgeWritesVar + "=allow"}, proxy)
	if len(refused) != 0 {
		t.Fatalf("refused = %v, want none (a base value is dropped by omission)", refused)
	}
	var got []string
	for _, kv := range env {
		if strings.HasPrefix(kv, acceptenv.ForgeWritesVar+"=") {
			got = append(got, kv)
		}
	}
	want := []string{acceptenv.ForgeWritesVar + "=" + acceptenv.ForgeWritesDeny}
	if !slices.Equal(got, want) {
		t.Fatalf("forge-writes entries = %v, want exactly %v (base allow must be dropped)", got, want)
	}
}

// TestEnv_RefusesRunAgentMarkerPassthrough (#3945) proves the runner-stamped
// run-agent marker cannot be supplied through the credential channel: a
// passthrough named FISHHAWK_RUN_AGENT (upper and mixed case) is refused and
// reported, and neither spelling reaches the composed env.
func TestEnv_RefusesRunAgentMarkerPassthrough(t *testing.T) {
	base := []string{
		"FISHHAWK_ACCEPTANCE_ENV_FISHHAWK_RUN_AGENT=spoof",
		"FISHHAWK_ACCEPTANCE_ENV_Fishhawk_Run_Agent=spoof",
	}
	env, refused := acceptenv.Env(base, proxy)
	if !slices.Contains(refused, "FISHHAWK_RUN_AGENT") || !slices.Contains(refused, "Fishhawk_Run_Agent") {
		t.Errorf("refused = %v, want both run-agent passthroughs named", refused)
	}
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); strings.EqualFold(k, agent.RunAgentEnvVar) {
			t.Errorf("run-agent passthrough admitted onto the env: %q (the marker is runner-stamped, never operator input)", kv)
		}
	}
}

// TestEnv_DropsBaseRunAgentMarker (#3945) proves an ambient FISHHAWK_RUN_AGENT
// on the runner's env (an operator's value, or one inherited by a runner
// spawned under an agent) never survives composition: the allow-list drops it
// by omission, and the adapter's last-applied stamp supplies the real value.
func TestEnv_DropsBaseRunAgentMarker(t *testing.T) {
	env, refused := acceptenv.Env([]string{agent.RunAgentEnvVar + "=stale-parent", "PATH=/usr/bin"}, proxy)
	if len(refused) != 0 {
		t.Fatalf("refused = %v, want none (a base value is dropped by omission)", refused)
	}
	if _, ok := envMap(t, env)[agent.RunAgentEnvVar]; ok {
		t.Errorf("ambient %s survived acceptenv composition: %v", agent.RunAgentEnvVar, env)
	}
}

// TestEnv_CredentialLocatorRefusalIsConditional (#3792, finding 3) pins that
// the credential-locator passthrough refusals bind ONLY under
// WithCredentialIsolation: with it each name is refused and absent; without
// it (FISHHAWK_ACCEPTANCE_CREDENTIAL_ISOLATION=off) each is emitted exactly as
// before. The base carries no HOME, so an emitted HOME can only be the
// passthrough's.
func TestEnv_CredentialLocatorRefusalIsConditional(t *testing.T) {
	names := []string{
		"HOME", "xdg_config_home", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR",
		"GH_CONFIG_DIR", "git_config_count", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM",
		"GIT_CONFIG_KEY_0", "GIT_CONFIG_PARAMETERS", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "SSH_AUTH_SOCK",
	}
	base := []string{"PATH=/usr/bin", "FISHHAWK_ACCEPTANCE_ENV_APP_USER=tester"}
	for _, n := range names {
		base = append(base, acceptenv.PassthroughPrefix+n+"=/x/"+n)
	}
	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			t.Run("with_isolation_refused", func(t *testing.T) {
				env, refused := acceptenv.Env(base, proxy, acceptenv.WithCredentialIsolation())
				if !slices.Contains(refused, n) {
					t.Errorf("refused = %v, want it to name %s under credential isolation", refused, n)
				}
				if _, ok := envMap(t, env)[n]; ok {
					t.Errorf("%s passed through under credential isolation: %v", n, env)
				}
				if got := envMap(t, env)["APP_USER"]; got != "tester" {
					t.Errorf("APP_USER = %q, want tester (only locator names are refused)", got)
				}
			})
			t.Run("without_isolation_passed_through", func(t *testing.T) {
				env, refused := acceptenv.Env(base, proxy)
				if slices.Contains(refused, n) {
					t.Errorf("refused = %v names %s without credential isolation; off must restore the passthrough", refused, n)
				}
				if !slices.Contains(env, n+"=/x/"+n) {
					t.Errorf("env lacks %s=/x/%s without credential isolation: %v", n, n, env)
				}
			})
		})
	}
}

// TestHasModelCredential_Table (#3792) pins which env-carried credential
// counts per agent: codex needs OPENAI_API_KEY; every other agent needs a
// non-empty ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN.
func TestHasModelCredential_Table(t *testing.T) {
	rows := []struct {
		name  string
		base  []string
		agent string
		want  bool
	}{
		{"claude-code with anthropic key", []string{"ANTHROPIC_API_KEY=k"}, "claude-code", true},
		{"claude-code with oauth token", []string{"CLAUDE_CODE_OAUTH_TOKEN=t"}, "claude-code", true},
		{"claude-code with neither key", []string{"PATH=/usr/bin", "OPENAI_API_KEY=o"}, "claude-code", false},
		{"claude-code with empty values", []string{"ANTHROPIC_API_KEY=", "CLAUDE_CODE_OAUTH_TOKEN="}, "claude-code", false},
		{"claude-code last duplicate wins (empty)", []string{"ANTHROPIC_API_KEY=k", "ANTHROPIC_API_KEY="}, "claude-code", false},
		{"codex with openai key", []string{"OPENAI_API_KEY=o"}, "codex", true},
		{"codex with only anthropic key", []string{"ANTHROPIC_API_KEY=k", "CLAUDE_CODE_OAUTH_TOKEN=t"}, "codex", false},
		{"codex with empty openai key", []string{"OPENAI_API_KEY="}, "codex", false},
	}
	for _, r := range rows {
		if got := acceptenv.HasModelCredential(r.base, r.agent); got != r.want {
			t.Errorf("%s: HasModelCredential = %v, want %v", r.name, got, r.want)
		}
	}
}

// TestIsolateCredentials_Composition (#3792) pins the env rewrite itself:
// every GIT_CONFIG* entry (any case) is stripped and the two pins appended
// exactly once, with or without a home; the home family is re-pointed only
// when a home is given.
func TestIsolateCredentials_Composition(t *testing.T) {
	in := []string{
		"PATH=/usr/bin", "HOME=/real", "home=/lower", "XDG_CONFIG_HOME=/real/.config", "GH_CONFIG_DIR=/real/gh",
		"GIT_CONFIG_GLOBAL=/real/.gitconfig", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper",
		"git_config_value_0=store", "ANTHROPIC_API_KEY=k",
	}
	t.Run("with_home", func(t *testing.T) {
		out := acceptenv.IsolateCredentials(in, "/syn")
		want := []string{
			"PATH=/usr/bin", "ANTHROPIC_API_KEY=k",
			"HOME=/syn", "XDG_CONFIG_HOME=/syn/.config", "XDG_CACHE_HOME=/syn/.cache",
			"XDG_DATA_HOME=/syn/.local/share", "XDG_STATE_HOME=/syn/.local/state", "GH_CONFIG_DIR=/syn/.config/gh",
			acceptenv.GitConfigGlobalPin, acceptenv.GitConfigNoSystemPin,
		}
		if !slices.Equal(out, want) {
			t.Fatalf("IsolateCredentials =\n%q\nwant\n%q", out, want)
		}
	})
	t.Run("without_home_pins_git_only", func(t *testing.T) {
		out := acceptenv.IsolateCredentials(in, "")
		want := []string{
			"PATH=/usr/bin", "HOME=/real", "home=/lower", "XDG_CONFIG_HOME=/real/.config", "GH_CONFIG_DIR=/real/gh",
			"ANTHROPIC_API_KEY=k", acceptenv.GitConfigGlobalPin, acceptenv.GitConfigNoSystemPin,
		}
		if !slices.Equal(out, want) {
			t.Fatalf("IsolateCredentials =\n%q\nwant\n%q", out, want)
		}
	})
}

// TestIsolateCredentials_FakeHomeCredentialsUnreachable (#3792, controls
// 1–3) runs a REAL child under the composed env against a fake operator
// home carrying a gh hosts file, a .git-credentials and a .gitconfig, each
// with a distinct sentinel. The child reads each via $HOME and the git
// sub-cases use NORMAL config loading (no --global/--system), the way a
// descendant's git would.
func TestIsolateCredentials_FakeHomeCredentialsUnreachable(t *testing.T) {
	const (
		ghSentinel     = "GHSENTINEL-hosts"
		credsSentinel  = "CREDSENTINEL-git-credentials"
		globalSentinel = "GLOBALSENTINEL"
		sysSentinel    = "SYSSENTINEL"
	)
	fake := t.TempDir()
	writeFile(t, filepath.Join(fake, ".config", "gh", "hosts.yml"), "github.com:\n  oauth_token: "+ghSentinel+"\n")
	writeFile(t, filepath.Join(fake, ".git-credentials"), "https://x:"+credsSentinel+"@github.com\n")
	writeFile(t, filepath.Join(fake, ".gitconfig"),
		"[user]\n\temail = "+globalSentinel+"\n[credential]\n\thelper = store --file="+filepath.Join(fake, ".git-credentials")+"\n")
	writeFile(t, filepath.Join(fake, "sysgitconfig"), "[user]\n\tname = "+sysSentinel+"\n")

	base := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + fake}
	env, refused := acceptenv.Env(base, proxy, acceptenv.WithCredentialIsolation())
	if len(refused) != 0 {
		t.Fatalf("refused = %v, want none", refused)
	}
	// Created exactly as main.go creates it: an empty 0700 temp dir.
	home, err := os.MkdirTemp("", "fishhawk-acceptance-home-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	env = acceptenv.IsolateCredentials(env, home)

	m := envMap(t, env)
	if m["HOME"] != home || m["HOME"] == fake {
		t.Fatalf("HOME = %q, want the synthetic home %q (never the operator home %q)", m["HOME"], home, fake)
	}
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "GH_CONFIG_DIR"} {
		if !strings.HasPrefix(m[k], home+string(filepath.Separator)) {
			t.Errorf("%s = %q, want under the synthetic home %q", k, m[k], home)
		}
	}
	fi, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("synthetic home mode = %o, want 0700", fi.Mode().Perm())
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Errorf("synthetic home has %d entries, want 0", len(ents))
	}

	run := func(t *testing.T, script string) string {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Env = env
		cmd.Dir = t.TempDir() // not inside any repository
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	noSentinel := func(t *testing.T, out string, sentinels ...string) {
		t.Helper()
		for _, s := range sentinels {
			if strings.Contains(out, s) {
				t.Errorf("child output carries %s — a host credential was reachable:\n%s", s, out)
			}
		}
	}

	t.Run("home_reads", func(t *testing.T) {
		out := run(t, `echo "HOME=$HOME"; cat "$HOME/.config/gh/hosts.yml" "$HOME/.git-credentials" "$HOME/.gitconfig" "$GH_CONFIG_DIR/hosts.yml" "$XDG_CONFIG_HOME/gh/hosts.yml" 2>&1`)
		if !strings.Contains(out, "HOME="+home+"\n") {
			t.Errorf("child HOME line missing the synthetic home %q:\n%s", home, out)
		}
		noSentinel(t, out, ghSentinel, credsSentinel, globalSentinel)
	})

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; the git sub-cases need it")
	}
	t.Run("git_global_pin_survives_home_repoint", func(t *testing.T) {
		// An agent that re-points HOME at the real home: only the
		// GIT_CONFIG_GLOBAL pin keeps git's normal load off its .gitconfig.
		out := run(t, `HOME='`+fake+`' git config --list --show-origin 2>&1`)
		if strings.Contains(out, "file:"+fake) {
			t.Errorf("git loaded config from the operator home:\n%s", out)
		}
		noSentinel(t, out, globalSentinel)
	})
	t.Run("git_global_list_empty", func(t *testing.T) {
		if out := run(t, `git config --global --list 2>&1`); strings.TrimSpace(out) != "" {
			t.Errorf("git config --global --list = %q, want empty", out)
		}
	})
	t.Run("git_nosystem_pin_ignores_planted_system_file", func(t *testing.T) {
		sys := filepath.Join(fake, "sysgitconfig")
		out := run(t, `GIT_CONFIG_SYSTEM='`+sys+`' git config --list --show-origin 2>&1`)
		if strings.Contains(out, sys) {
			t.Errorf("git loaded the planted system file %s:\n%s", sys, out)
		}
		noSentinel(t, out, sysSentinel)
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
