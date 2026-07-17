package acceptenv_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/acceptenv"
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
// table: what survives, what is injected, and what can never appear.
func TestEnv_PostureTable(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"HOME=/home/runner",
		"LC_ALL=en_US.UTF-8",
		"ANTHROPIC_API_KEY=model-key",                  // model key: the one surviving secret class
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
	env, refused := acceptenv.Env(base, proxy)
	if len(refused) != 0 {
		t.Fatalf("refused = %v, want none", refused)
	}
	m := envMap(t, env)

	present := map[string]string{
		"PATH":              "/usr/bin",
		"HOME":              "/home/runner",
		"LC_ALL":            "en_US.UTF-8",
		"ANTHROPIC_API_KEY": "model-key",
		"OPENAI_API_KEY":    "other-model-key",
		"APP_USER":          "tester",
		"APP_PASSWORD":      "hunter2",
		"HTTPS_PROXY":       proxy,
		"HTTP_PROXY":        proxy,
		"ALL_PROXY":         proxy,
		"https_proxy":       proxy,
		"http_proxy":        proxy,
		"all_proxy":         proxy,
		"NO_PROXY":          "",
		"no_proxy":          "",
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
