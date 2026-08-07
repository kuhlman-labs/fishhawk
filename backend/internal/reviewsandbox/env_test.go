package reviewsandbox

import (
	"slices"
	"strings"
	"testing"
)

// names extracts the variable NAME (before '=') of each "NAME=value" entry.
func names(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			out = append(out, kv[:i])
		}
	}
	return out
}

// TestEnv_DropsSecretsKeepsAllowed pins the core scrub: sentinel secrets are
// dropped, allow-listed names survive, a passthrough name is admitted, and a
// name that merely shares a PREFIX with an allowed name is NOT admitted (no
// prefix matching).
func TestEnv_DropsSecretsKeepsAllowed(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"HOME=/home/x",
		"FISHHAWKD_DATABASE_URL=postgres://secret",
		"GITHUB_TOKEN=ghs_secret",
		"OPENAI_API_KEY=sk-secret", // not in ClaudeAllow — proves per-adapter scoping
		"ANTHROPIC_API_KEY=sk-ant",
		"PATH_EXTRA=nope", // prefix of PATH but a distinct name
		"BEDROCK_REGION=us-east-1",
	}
	got := names(Env(parent, ClaudeAllow, []string{"BEDROCK_REGION"}))

	for _, want := range []string{"PATH", "HOME", "ANTHROPIC_API_KEY", "BEDROCK_REGION"} {
		if !slices.Contains(got, want) {
			t.Errorf("Env dropped allowed/passthrough var %q; got %v", want, got)
		}
	}
	for _, forbidden := range []string{"FISHHAWKD_DATABASE_URL", "GITHUB_TOKEN", "OPENAI_API_KEY", "PATH_EXTRA"} {
		if slices.Contains(got, forbidden) {
			t.Errorf("Env leaked %q; got %v", forbidden, got)
		}
	}
}

// TestEnv_CodexAllowScoping proves the per-adapter allow-lists differ: the codex
// list admits OPENAI_API_KEY / CODEX_HOME and drops the Anthropic vars.
func TestEnv_CodexAllowScoping(t *testing.T) {
	parent := []string{
		"OPENAI_API_KEY=sk-openai",
		"CODEX_HOME=/home/x/.codex",
		"ANTHROPIC_API_KEY=sk-ant",
	}
	got := names(Env(parent, CodexAllow, nil))
	if !slices.Contains(got, "OPENAI_API_KEY") || !slices.Contains(got, "CODEX_HOME") {
		t.Errorf("CodexAllow dropped an expected var; got %v", got)
	}
	if slices.Contains(got, "ANTHROPIC_API_KEY") {
		t.Errorf("CodexAllow admitted an Anthropic var; got %v", got)
	}
}

// TestEnv_EmptyPassthroughNameIgnored proves an empty passthrough entry does not
// admit malformed environ lines (a "=value" with no name).
func TestEnv_EmptyPassthroughNameIgnored(t *testing.T) {
	parent := []string{"=orphan", "PATH=/usr/bin"}
	got := Env(parent, BaseAllow, []string{""})
	if len(got) != 1 || got[0] != "PATH=/usr/bin" {
		t.Errorf("Env admitted a malformed entry via empty passthrough; got %v", got)
	}
}

// TestEnv_LastWinsOrderPreserved proves duplicate NAME entries keep input order
// so last-wins shadowing for the child's os.Getenv is preserved.
func TestEnv_LastWinsOrderPreserved(t *testing.T) {
	parent := []string{"PATH=/a", "HOME=/h", "PATH=/b"}
	got := Env(parent, BaseAllow, nil)
	want := []string{"PATH=/a", "HOME=/h", "PATH=/b"}
	if !slices.Equal(got, want) {
		t.Errorf("Env = %v, want order-preserving %v", got, want)
	}
}
