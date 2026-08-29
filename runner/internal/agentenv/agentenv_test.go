package agentenv

import (
	"reflect"
	"strings"
	"testing"
)

// envMap indexes a composed env slice by key. It deliberately does NOT
// deduplicate: a duplicate key would be visible as a differing count via
// envCount below.
func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			t.Fatalf("composed env entry %q has no key", kv)
		}
		out[kv[:i]] = kv[i+1:]
	}
	return out
}

// TestEnv_DropsMalformedEntries covers the two shapes the leading guard
// rejects: an entry with no '=' at all, and one with an empty key. Both are
// paired with an otherwise-allowed key so the drop is attributable to the
// guard and not to the allow-list.
//
// Counterfactual honesty: deleting the guard outright turns this RED (a
// panic — kv[:eq] with eq == -1), so the no-'=' half is load-bearing.
// Weakening ONLY the empty-key half (eq <= 0 -> eq < 0) leaves this GREEN,
// because the empty key matches no allow rung and default-deny drops it
// anyway. That half is kept for byte-uniformity with the sibling
// gateenv/acceptenv guards, and is defense-in-depth rather than an
// independently discriminating control.
func TestEnv_DropsMalformedEntries(t *testing.T) {
	env, refused := Env([]string{
		"PATH",      // no '=' — not an assignment
		"=orphaned", // empty key
		"PATH=/bin", // the control: an allowed, well-formed entry
	})
	if len(refused) != 0 {
		t.Errorf("refused = %v, want none", refused)
	}
	if !reflect.DeepEqual(env, []string{"PATH=/bin"}) {
		t.Errorf("Env = %v, want exactly [PATH=/bin] — a malformed entry must be dropped", env)
	}
}

// TestEnv_DropsDeniedExactKeys asserts every denyExact name is absent from
// the composed env, one subtest per name so a single re-admitted key is
// named in the failure rather than hidden in an aggregate.
func TestEnv_DropsDeniedExactKeys(t *testing.T) {
	for key := range denyExact {
		t.Run(key, func(t *testing.T) {
			env, _ := Env([]string{key + "=secret-value", "PATH=/bin"})
			if got := envMap(t, env); got[key] != "" {
				t.Errorf("%s = %q, want absent (denyExact)", key, got[key])
			}
			if strings.Contains(strings.Join(env, "\n"), "secret-value") {
				t.Errorf("denied value leaked into the composed env: %v", env)
			}
		})
	}
}

// TestEnv_DropsDeniedPrefixFamilies covers each denied PREFIX family with a
// representative member that is not itself on denyExact, so the drop is
// attributable to the prefix rung.
func TestEnv_DropsDeniedPrefixFamilies(t *testing.T) {
	for _, key := range []string{
		"FISHHAWK_TEST_TIME_SCALE",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"AWS_SECRET_ACCESS_KEY",
		"AZURE_CLIENT_SECRET",
	} {
		t.Run(key, func(t *testing.T) {
			if !Denied(key) {
				t.Fatalf("Denied(%q) = false, want true", key)
			}
			env, _ := Env([]string{key + "=secret-value", "PATH=/bin"})
			if got := envMap(t, env); got[key] != "" {
				t.Errorf("%s = %q, want absent (denyPrefix)", key, got[key])
			}
		})
	}
}

// TestEnv_DropsUnlistedKey is the default-deny case: a key on NO allow rung
// and NO deny rung is dropped anyway, so a secret added to the runner later
// never leaks by omission.
func TestEnv_DropsUnlistedKey(t *testing.T) {
	env, _ := Env([]string{"SOME_FUTURE_VENDOR_SECRET=s3cr3t", "PATH=/bin"})
	got := envMap(t, env)
	if _, ok := got["SOME_FUTURE_VENDOR_SECRET"]; ok {
		t.Errorf("unlisted key survived: %v — the policy must be default-deny, not denylist-only", env)
	}
	if got["PATH"] != "/bin" {
		t.Errorf("PATH = %q, want /bin", got["PATH"])
	}
	if Allowed("SOME_FUTURE_VENDOR_SECRET") {
		t.Error("Allowed(SOME_FUTURE_VENDOR_SECRET) = true, want false")
	}
}

// TestEnv_KeepsAllowedEntriesByteIdentical walks one representative of each
// allow rung — an exact system essential, a Go toolchain name, and every
// allowPrefix family — and asserts the entry is reproduced verbatim. One case
// carries a value CONTAINING '=' so a naive split-and-rejoin regression is
// caught.
func TestEnv_KeepsAllowedEntriesByteIdentical(t *testing.T) {
	entries := []string{
		"PATH=/usr/bin:/bin",                 // allowExact
		"DOCKER_HOST=unix:///var/run/d.sock", // allowExact (docker client)
		"CI=true",                            // allowExact (timescale auto-5x)
		"GOFLAGS=-mod=mod",                   // allowGo, value contains '='
		"LC_ALL=en_US.UTF-8",
		"CGO_ENABLED=1",
		"XDG_CACHE_HOME=/home/u/.cache",
		"NODE_OPTIONS=--max-old-space-size=4096",
		"NPM_CONFIG_REGISTRY=https://registry.npmjs.org",
		"npm_config_cache=/home/u/.npm",
		"TESTCONTAINERS_RYUK_DISABLED=true",
		"SSL_CERT_FILE=/etc/ssl/cert.pem",
		"ANTHROPIC_BASE_URL=https://gw.example/v1",
		"OPENAI_BASE_URL=https://gw.example/v1",
		"CLAUDE_CODE_MAX_OUTPUT_TOKENS=64000",
	}
	env, refused := Env(entries)
	if len(refused) != 0 {
		t.Errorf("refused = %v, want none", refused)
	}
	if !reflect.DeepEqual(env, entries) {
		t.Errorf("Env = %#v,\nwant byte-identical %#v", env, entries)
	}
}

// TestEnv_PassthroughStripsPrefix pins the operator escape hatch: the
// FISHHAWK_AGENT_ENV_ prefix is stripped and the value passes verbatim, even
// though the FISHHAWK_ deny prefix would otherwise drop the raw key.
func TestEnv_PassthroughStripsPrefix(t *testing.T) {
	env, refused := Env([]string{PassthroughPrefix + "FOO=bar=baz"})
	if len(refused) != 0 {
		t.Errorf("refused = %v, want none", refused)
	}
	if !reflect.DeepEqual(env, []string{"FOO=bar=baz"}) {
		t.Errorf("Env = %v, want [FOO=bar=baz]", env)
	}
	if got := envMap(t, env); got[PassthroughPrefix+"FOO"] != "" {
		t.Error("the prefixed key must not also survive")
	}
}

// TestEnv_PassthroughEmptyNameDropped covers the bare-prefix guard: a
// FISHHAWK_AGENT_ENV_= entry strips to an empty name, which is not a usable
// assignment. It must be dropped silently, NOT emitted as "=value" (which the
// child's own env parser would treat as malformed) and NOT reported refused
// (nothing was denied).
func TestEnv_PassthroughEmptyNameDropped(t *testing.T) {
	env, refused := Env([]string{PassthroughPrefix + "=value", "PATH=/bin"})
	if len(refused) != 0 {
		t.Errorf("refused = %v, want none — an empty name is malformed, not denied", refused)
	}
	if !reflect.DeepEqual(env, []string{"PATH=/bin"}) {
		t.Errorf("Env = %v, want exactly [PATH=/bin]", env)
	}
}

// TestEnv_PassthroughDeniedCollisionRefused is the case where the denylist is
// load-bearing rather than belt-and-suspenders: the default-deny allow-list
// already drops these names from the BASE, so the passthrough branch is the
// only place a deny rung changes the outcome. An attempt to smuggle a denied
// name back in must be absent from the env AND reported in refused.
//
// SSH_AUTH_SOCK (denyExact) and AWS_BEARER_TOKEN_BEDROCK (denyPrefix) are the
// two names the README's "Honest residuals" paragraph cites: they are the
// cases where an operator might reasonably EXPECT the passthrough to be a
// recovery path, and it is not. Pinning them here keeps that paragraph
// consistent with the enforced refusal instead of merely asserting it.
func TestEnv_PassthroughDeniedCollisionRefused(t *testing.T) {
	for _, name := range []string{
		"GITHUB_TOKEN",
		"FISHHAWK_API_TOKEN",
		"ANTHROPIC_API_KEY",
		"AWS_SECRET_ACCESS_KEY",
		"SSH_AUTH_SOCK",
		"AWS_BEARER_TOKEN_BEDROCK",
	} {
		t.Run(name, func(t *testing.T) {
			env, refused := Env([]string{PassthroughPrefix + name + "=smuggled"})
			if got := envMap(t, env); got[name] != "" {
				t.Errorf("%s = %q, want absent — a denied name must not be honored via the passthrough", name, got[name])
			}
			if strings.Contains(strings.Join(env, "\n"), "smuggled") {
				t.Errorf("smuggled value present in env: %v", env)
			}
			if !reflect.DeepEqual(refused, []string{name}) {
				t.Errorf("refused = %v, want [%s] — a refusal must be reported, never silent", refused, name)
			}
		})
	}
}

// TestEnv_RefusedSortedDeterministically pins the ordering guarantee so the
// caller's agent_env_refused log line is stable across runs regardless of the
// order the runner's environment happens to enumerate.
func TestEnv_RefusedSortedDeterministically(t *testing.T) {
	_, refused := Env([]string{
		PassthroughPrefix + "GITHUB_TOKEN=a",
		PassthroughPrefix + "AWS_SECRET_ACCESS_KEY=b",
		PassthroughPrefix + "FISHHAWK_API_TOKEN=c",
	})
	want := []string{"AWS_SECRET_ACCESS_KEY", "FISHHAWK_API_TOKEN", "GITHUB_TOKEN"}
	if !reflect.DeepEqual(refused, want) {
		t.Errorf("refused = %v, want %v (sorted)", refused, want)
	}
}

// TestEnv_EmptyBaseYieldsNonNilSlice pins the default-deny corner the
// adapters depend on: os/exec treats a nil Cmd.Env as inherit-parent-env, so
// an all-dropped composition must still be a non-nil EMPTY slice.
func TestEnv_EmptyBaseYieldsNonNilSlice(t *testing.T) {
	env, refused := Env([]string{"GITHUB_TOKEN=x"})
	if env == nil {
		t.Fatal("Env returned a nil slice; a nil cmd.Env means inherit-parent-env — the opposite of default-deny")
	}
	if len(env) != 0 {
		t.Errorf("Env = %v, want empty", env)
	}
	if len(refused) != 0 {
		t.Errorf("refused = %v, want none", refused)
	}
}

// TestAllowedConsultsAllowRungsOnly pins the predicate split (binding
// approval condition 1). Allowed reads the allow rungs ONLY, mirroring
// gateEnvAllowed, so a future widened allow-rule reddens here on its own
// rather than being masked by the deny layer.
//
// FISHHAWK_API_TOKEN is the case that genuinely pins that: FISHHAWK_ is a
// deny PREFIX and matches no allow rung, so the assertion holds on the allow
// layer alone. ANTHROPIC_API_KEY / OPENAI_API_KEY are deliberately NOT
// asserted false here — the model-key family IS re-admitted by allowPrefix
// and the DENYLIST is what excludes the raw key from the base, which is what
// the Denied assertions below pin.
func TestAllowedConsultsAllowRungsOnly(t *testing.T) {
	if Allowed("FISHHAWK_API_TOKEN") {
		t.Error("Allowed(FISHHAWK_API_TOKEN) = true, want false — it matches no allow rung")
	}
	if !Allowed("ANTHROPIC_API_KEY") {
		t.Error("Allowed(ANTHROPIC_API_KEY) = false, want true — the ANTHROPIC_ family is on allowPrefix; the DENY layer is what strips the raw key")
	}
	if !Denied("ANTHROPIC_API_KEY") {
		t.Error("Denied(ANTHROPIC_API_KEY) = false, want true — the raw model key must not ride in from the base env")
	}
	if !Denied("OPENAI_API_KEY") {
		t.Error("Denied(OPENAI_API_KEY) = false, want true — the raw model key must not ride in from the base env")
	}
	// The composition of the two layers: the raw key is stripped while a
	// sibling ANTHROPIC_* configuration var survives.
	got := envMap(t, mustEnv(t, []string{"ANTHROPIC_API_KEY=sk-ambient", "ANTHROPIC_BASE_URL=https://gw"}))
	if _, ok := got["ANTHROPIC_API_KEY"]; ok {
		t.Error("ANTHROPIC_API_KEY survived the base composition; the adapter overlay must be its single injection point")
	}
	if got["ANTHROPIC_BASE_URL"] != "https://gw" {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want https://gw", got["ANTHROPIC_BASE_URL"])
	}
}

func mustEnv(t *testing.T, base []string) []string {
	t.Helper()
	env, refused := Env(base)
	if len(refused) != 0 {
		t.Fatalf("unexpected refusals: %v", refused)
	}
	return env
}
