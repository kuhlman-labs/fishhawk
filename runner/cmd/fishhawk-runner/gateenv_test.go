package main

import (
	"os"
	"strings"
	"testing"
)

// TestSanitizeEnv_StripsSecretsKeepsToolchain feeds sanitizeEnv a base slice
// carrying each known runner secret alongside the allow-listed system/toolchain
// vars and asserts every secret is dropped while every allow-listed key
// survives with its value intact.
func TestSanitizeEnv_StripsSecretsKeepsToolchain(t *testing.T) {
	base := []string{
		// Secrets — must all be stripped.
		"FISHHAWK_GITHUB_TOKEN=ghs_secret",
		"GITHUB_TOKEN=gh_secret",
		"GH_TOKEN=gh_secret2",
		"ANTHROPIC_API_KEY=sk-ant-secret",
		"OPENAI_API_KEY=sk-oai-secret",
		"FISHHAWK_API_TOKEN=mcp-secret",
		// Allow-listed — must survive.
		"PATH=/usr/bin:/bin",
		"HOME=/home/runner",
		"GOCACHE=/tmp/gocache",
		"GOMODCACHE=/tmp/gomodcache",
	}
	got := sanitizeEnv(base)
	gotMap := envSliceToMap(t, got)

	for _, secret := range []string{
		"FISHHAWK_GITHUB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN",
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "FISHHAWK_API_TOKEN",
	} {
		if _, present := gotMap[secret]; present {
			t.Errorf("secret %s must be stripped, but it survived", secret)
		}
	}
	want := map[string]string{
		"PATH":       "/usr/bin:/bin",
		"HOME":       "/home/runner",
		"GOCACHE":    "/tmp/gocache",
		"GOMODCACHE": "/tmp/gomodcache",
	}
	for k, v := range want {
		if gotMap[k] != v {
			t.Errorf("allow-listed %s = %q, want %q", k, gotMap[k], v)
		}
	}
}

// TestSanitizeEnv_EdgeCases covers the parser/allow-list edge cases: a
// malformed entry with no '=', an empty key, an empty value, an LC_* prefix
// match, a GO-prefixed var (GOFLAGS), and a non-allow-listed innocuous var
// (AWS_REGION) that must be dropped despite carrying no secret.
func TestSanitizeEnv_EdgeCases(t *testing.T) {
	base := []string{
		"MALFORMED_NO_EQUALS",  // no '=' — dropped
		"=novalue",             // empty key — dropped
		"TZ=",                  // allow-listed key, empty value — kept
		"LC_ALL=en_US.UTF-8",   // LC_* prefix — kept
		"GOFLAGS=-mod=mod",     // GO* prefix, value contains '=' — kept whole
		"AWS_REGION=us-east-1", // not allow-listed, not secret — dropped
	}
	got := sanitizeEnv(base)
	gotMap := envSliceToMap(t, got)

	for _, dropped := range []string{"MALFORMED_NO_EQUALS", "", "AWS_REGION"} {
		if _, present := gotMap[dropped]; present {
			t.Errorf("%q should have been dropped, but survived", dropped)
		}
	}
	if v, present := gotMap["TZ"]; !present || v != "" {
		t.Errorf("TZ should survive with empty value, got present=%v value=%q", present, v)
	}
	if gotMap["LC_ALL"] != "en_US.UTF-8" {
		t.Errorf("LC_ALL = %q, want en_US.UTF-8", gotMap["LC_ALL"])
	}
	// GOFLAGS value contains a '=' — the parser must split on the FIRST '='
	// only, preserving the full value.
	if gotMap["GOFLAGS"] != "-mod=mod" {
		t.Errorf("GOFLAGS = %q, want -mod=mod (split on first '=' only)", gotMap["GOFLAGS"])
	}
}

// TestSanitizeEnv_RedactsGoproxyUserinfo asserts that embedded URL userinfo is
// stripped from credentialed GO* values (notably GOPROXY) before they reach
// gate code, while non-credentialed forms (off, direct, bare host, no-userinfo
// URL) and the proxy host/path survive byte-identical.
func TestSanitizeEnv_RedactsGoproxyUserinfo(t *testing.T) {
	base := []string{
		// Single credentialed proxy — userinfo redacted, host/path kept.
		"GOPROXY=https://user:tok@proxy.example.com",
		// Comma-separated list — each credentialed entry redacted, the
		// uncredentialed 'direct' fall-through untouched, order/separators kept.
		"GOSUMDB=https://u:p@sum.example.com,direct",
		// '|'-separated list — same, with the alternate separator preserved.
		"GONOSUMCHECK=https://a:b@one.example.com|https://two.example.com",
		// No userinfo — must be byte-identical.
		"GOPRIVATE=https://proxy.example.com/path",
		// Non-URL GO* forms — must be byte-identical.
		"GO111MODULE=on",
		"GOFLAGS=-mod=mod",
		// Allow-listed non-GO var — never run through the transform.
		"PATH=/usr/bin:/bin",
	}
	got := sanitizeEnv(base)
	gotMap := envSliceToMap(t, got)

	want := map[string]string{
		"GOPROXY":      "https://proxy.example.com",
		"GOSUMDB":      "https://sum.example.com,direct",
		"GONOSUMCHECK": "https://one.example.com|https://two.example.com",
		"GOPRIVATE":    "https://proxy.example.com/path",
		"GO111MODULE":  "on",
		"GOFLAGS":      "-mod=mod",
		"PATH":         "/usr/bin:/bin",
	}
	for k, v := range want {
		if gotMap[k] != v {
			t.Errorf("%s = %q, want %q", k, gotMap[k], v)
		}
	}
}

// TestSanitizeEnv_RedactsMixedSeparatorGoproxy exercises a GOPROXY list that
// mixes both fall-through separators (',' and '|') alongside a bare 'direct'
// entry: each credentialed entry must be redacted, the uncredentialed/'direct'
// entries untouched, and BOTH separators plus the entry order preserved exactly.
func TestSanitizeEnv_RedactsMixedSeparatorGoproxy(t *testing.T) {
	const in = "GOPROXY=https://u:p@a.example.com,https://b.example.com|direct"
	const want = "https://a.example.com,https://b.example.com|direct"

	got := sanitizeEnv([]string{in})
	gotMap := envSliceToMap(t, got)
	if gotMap["GOPROXY"] != want {
		t.Errorf("GOPROXY = %q, want %q", gotMap["GOPROXY"], want)
	}
}

// TestSanitizedGateEnv_StripsLiveSecret is a thin check that the public
// entrypoint reads the live process env and drops a planted secret while
// keeping PATH.
func TestSanitizedGateEnv_StripsLiveSecret(t *testing.T) {
	t.Setenv("FISHHAWK_GITHUB_TOKEN", "leak-canary")
	got := sanitizedGateEnv()
	for _, kv := range got {
		if strings.HasPrefix(kv, "FISHHAWK_GITHUB_TOKEN=") {
			t.Fatalf("sanitizedGateEnv leaked the secret: %q", kv)
		}
	}
	if os.Getenv("PATH") != "" {
		var sawPath bool
		for _, kv := range got {
			if strings.HasPrefix(kv, "PATH=") {
				sawPath = true
				break
			}
		}
		if !sawPath {
			t.Error("sanitizedGateEnv dropped PATH, which must be preserved")
		}
	}
}

// TestWithIsolatedLintCache pins the pure helper that forces
// GOLANGCI_LINT_CACHE to a per-invocation dir: a base env without the var gains
// exactly one entry equal to the override; an inherited GOLANGCI_LINT_CACHE is
// dropped and replaced (a single entry, never a duplicate the platform might
// resolve to the ambient value); unrelated entries are preserved untouched.
func TestWithIsolatedLintCache(t *testing.T) {
	const override = "/tmp/iso-lint-cache"

	// (a) No inherited GOLANGCI_LINT_CACHE — exactly one entry is appended.
	base := []string{"PATH=/usr/bin:/bin", "HOME=/home/runner"}
	got := withIsolatedLintCache(base, override)
	if n := countKey(got, "GOLANGCI_LINT_CACHE"); n != 1 {
		t.Errorf("expected exactly 1 GOLANGCI_LINT_CACHE entry, got %d in %v", n, got)
	}
	gotMap := envSliceToMap(t, got)
	if gotMap["GOLANGCI_LINT_CACHE"] != override {
		t.Errorf("GOLANGCI_LINT_CACHE = %q, want %q", gotMap["GOLANGCI_LINT_CACHE"], override)
	}

	// (b) Inherited GOLANGCI_LINT_CACHE=/shared — dropped and replaced by the
	// override, leaving a single entry (no ambient value can win).
	inherited := []string{"PATH=/usr/bin:/bin", "GOLANGCI_LINT_CACHE=/shared", "HOME=/home/runner"}
	got = withIsolatedLintCache(inherited, override)
	if n := countKey(got, "GOLANGCI_LINT_CACHE"); n != 1 {
		t.Errorf("expected exactly 1 GOLANGCI_LINT_CACHE entry after replacing inherited, got %d in %v", n, got)
	}
	gotMap = envSliceToMap(t, got)
	if gotMap["GOLANGCI_LINT_CACHE"] != override {
		t.Errorf("inherited GOLANGCI_LINT_CACHE not overridden: = %q, want %q", gotMap["GOLANGCI_LINT_CACHE"], override)
	}

	// (c) Unrelated entries survive with their values intact.
	for k, v := range map[string]string{"PATH": "/usr/bin:/bin", "HOME": "/home/runner"} {
		if gotMap[k] != v {
			t.Errorf("unrelated %s = %q, want %q", k, gotMap[k], v)
		}
	}
}

// countKey returns how many "KEY=..." entries in env have the given key.
func countKey(env []string, key string) int {
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			n++
		}
	}
	return n
}

func envSliceToMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	m := make(map[string]string, len(env))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			t.Errorf("sanitizeEnv emitted a malformed entry %q", kv)
			continue
		}
		m[kv[:eq]] = kv[eq+1:]
	}
	return m
}
