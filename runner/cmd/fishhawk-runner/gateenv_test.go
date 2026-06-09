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
