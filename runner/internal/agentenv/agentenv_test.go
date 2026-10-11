package agentenv

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
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
	// The trailing GOFLAGS=-trimpath is the always-present overlay (#4180);
	// the assertion still pins that the malformed entries added nothing else.
	if !reflect.DeepEqual(env, []string{"PATH=/bin", "GOFLAGS=-trimpath"}) {
		t.Errorf("Env = %v, want exactly [PATH=/bin GOFLAGS=-trimpath] — a malformed entry must be dropped", env)
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
// caught. GOFLAGS is NOT among them: it is the one entry Env rewrites
// (#4180), covered by TestEnv_GOFLAGSTrimpathOverlay; the byte-identical claim
// is every OTHER entry, and the overlay's own appended entry is trimmed off
// before the comparison.
func TestEnv_KeepsAllowedEntriesByteIdentical(t *testing.T) {
	entries := []string{
		"PATH=/usr/bin:/bin",                 // allowExact
		"DOCKER_HOST=unix:///var/run/d.sock", // allowExact (docker client)
		"CI=true",                            // allowExact (timescale auto-5x)
		"GODEBUG=x509sha1=1",                 // allowGo, value contains '='
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
	want := append(append([]string(nil), entries...), "GOFLAGS=-trimpath")
	if !reflect.DeepEqual(env, want) {
		t.Errorf("Env = %#v,\nwant byte-identical %#v plus the GOFLAGS overlay", env, entries)
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
	if !reflect.DeepEqual(env, []string{"FOO=bar=baz", "GOFLAGS=-trimpath"}) {
		t.Errorf("Env = %v, want [FOO=bar=baz GOFLAGS=-trimpath]", env)
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
	if !reflect.DeepEqual(env, []string{"PATH=/bin", "GOFLAGS=-trimpath"}) {
		t.Errorf("Env = %v, want exactly [PATH=/bin GOFLAGS=-trimpath]", env)
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
// an all-dropped composition must still be a non-nil slice. Since #4180 it is
// exactly the GOFLAGS overlay and nothing the dropped entry carried.
func TestEnv_EmptyBaseYieldsNonNilSlice(t *testing.T) {
	env, refused := Env([]string{"GITHUB_TOKEN=x"})
	if env == nil {
		t.Fatal("Env returned a nil slice; a nil cmd.Env means inherit-parent-env — the opposite of default-deny")
	}
	if !reflect.DeepEqual(env, []string{"GOFLAGS=-trimpath"}) {
		t.Errorf("Env = %v, want exactly [GOFLAGS=-trimpath]", env)
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

// TestEnv_RunAgentMarkerNeverComposed (#3945) is a TEST-ONLY pin of existing
// behaviour: the run-agent marker is runner-stamped by the agent adapter, so
// agentenv must never compose one. An ambient FISHHAWK_RUN_AGENT (an
// operator's value, or one inherited by a runner spawned under an agent) is
// dropped by the FISHHAWK_ deny prefix, and an operator passthrough
// FISHHAWK_AGENT_ENV_FISHHAWK_RUN_AGENT is refused because Denied applies to
// the stripped name.
func TestEnv_RunAgentMarkerNeverComposed(t *testing.T) {
	name := agent.RunAgentEnvVar
	if !Denied(name) {
		t.Fatalf("Denied(%q) = false, want true (the FISHHAWK_ deny prefix must cover the marker)", name)
	}

	env, refused := Env([]string{name + "=stale-parent", "PATH=/bin"})
	if _, ok := envMap(t, env)[name]; ok {
		t.Errorf("ambient %s survived agentenv composition: %v", name, env)
	}
	if len(refused) != 0 {
		t.Errorf("refused = %v, want none (an ambient value is dropped by omission)", refused)
	}

	env, refused = Env([]string{PassthroughPrefix + name + "=spoof"})
	if _, ok := envMap(t, env)[name]; ok {
		t.Errorf("passthrough %s admitted onto the env: %v", name, env)
	}
	if !reflect.DeepEqual(refused, []string{name}) {
		t.Errorf("refused = %v, want [%s] — the passthrough must be refused, never silent", refused, name)
	}
}

// goflagsEntries returns every GOFLAGS value in env in order, so an overlay
// test asserts the COUNT (a duplicate entry is a defect) and the exact bytes.
func goflagsEntries(env []string) []string {
	var out []string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GOFLAGS="); ok {
			out = append(out, v)
		}
	}
	return out
}

// TestEnv_GOFLAGSTrimpathOverlay (#4180) pins every merge mode of the GOFLAGS
// overlay, one named subtest each, asserting the composed env's GOFLAGS
// entries as a count plus byte-exact values. The merge is NON-clobbering (an
// inherited value is a verbatim prefix), idempotent (a field already naming
// the flag is left alone, which is also how -trimpath=false opts out), and
// quote-aware (it splits GOFLAGS the way the go command does, so the TEXT
// -trimpath inside a quoted -ldflags value is not mistaken for the flag).
func TestEnv_GOFLAGSTrimpathOverlay(t *testing.T) {
	tests := []struct {
		name string
		base []string
		want []string
	}{
		{"absent appends", []string{"PATH=/bin"}, []string{"-trimpath"}},
		{"empty value", []string{"GOFLAGS="}, []string{"-trimpath"}},
		{"whitespace-only value", []string{"GOFLAGS=   \t"}, []string{"-trimpath"}},
		{"inherited flags kept as prefix", []string{"GOFLAGS=-mod=mod -tags=x"}, []string{"-mod=mod -tags=x -trimpath"}},
		{"already single dash", []string{"GOFLAGS=-trimpath"}, []string{"-trimpath"}},
		{"already double dash", []string{"GOFLAGS=--trimpath"}, []string{"--trimpath"}},
		{"already with =true", []string{"GOFLAGS=-trimpath=true"}, []string{"-trimpath=true"}},
		{"already among others", []string{"GOFLAGS=-mod=mod -trimpath -tags=x"}, []string{"-mod=mod -trimpath -tags=x"}},
		{"quoted standalone field", []string{"GOFLAGS='-trimpath'"}, []string{"'-trimpath'"}},
		{"explicit opt-out is honored", []string{"GOFLAGS=-trimpath=false"}, []string{"-trimpath=false"}},
		{"flag text inside a quoted value is not the flag", []string{"GOFLAGS='-ldflags=-s -trimpath -w'"}, []string{"'-ldflags=-s -trimpath -w' -trimpath"}},
		// A field that STARTS with a quote and never closes it is the go
		// command's parse error; the value must reach it unchanged.
		{"unterminated quote left untouched", []string{"GOFLAGS='-ldflags=-s -w"}, []string{"'-ldflags=-s -w"}},
		{"unterminated double quote left untouched", []string{`GOFLAGS="-tags=a`}, []string{`"-tags=a`}},
		// A quote past the field start is ordinary text to the go command (no
		// parse error), so this is NOT the unterminated case and is merged.
		{"quote past field start is plain text", []string{"GOFLAGS=-ldflags='-s"}, []string{"-ldflags='-s -trimpath"}},
		{
			"ambient and passthrough are both merged",
			[]string{"GOFLAGS=-mod=mod", PassthroughPrefix + "GOFLAGS=-tags=y"},
			[]string{"-mod=mod -trimpath", "-tags=y -trimpath"},
		},
		{"passthrough only", []string{PassthroughPrefix + "GOFLAGS=-tags=y"}, []string{"-tags=y -trimpath"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, refused := Env(tc.base)
			if len(refused) != 0 {
				t.Errorf("refused = %v, want none", refused)
			}
			if got := goflagsEntries(env); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("GOFLAGS entries = %q, want %q (env %v)", got, tc.want, env)
			}
		})
	}
}

// TestEnv_GOFLAGSOverlayKeepsPosition pins that an existing GOFLAGS entry is
// rewritten IN PLACE (neighbours and order untouched) rather than dropped and
// re-appended.
func TestEnv_GOFLAGSOverlayKeepsPosition(t *testing.T) {
	env, _ := Env([]string{"PATH=/bin", "GOFLAGS=-mod=mod", "HOME=/h"})
	want := []string{"PATH=/bin", "GOFLAGS=-mod=mod -trimpath", "HOME=/h"}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("Env = %v, want %v", env, want)
	}
}

// TestSplitGOFLAGS pins the go-command-compatible tokenization directly, so a
// drift from cmd/internal/quoted is named here rather than only surfacing as
// an overlay subtest.
func TestSplitGOFLAGS(t *testing.T) {
	tests := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"", nil, false},
		{" \t\r\n ", nil, false},
		{"-a  -b\t-c", []string{"-a", "-b", "-c"}, false},
		{`'-ldflags=-s -w' "-tags=a b"`, []string{"-ldflags=-s -w", "-tags=a b"}, false},
		{`-ldflags='-s -w'`, []string{`-ldflags='-s`, `-w'`}, false}, // a quote past the field start does not count
		{`'-s`, nil, true},
		{`"-s`, nil, true},
	}
	for _, tc := range tests {
		got, err := splitGOFLAGS(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("splitGOFLAGS(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitGOFLAGS(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
