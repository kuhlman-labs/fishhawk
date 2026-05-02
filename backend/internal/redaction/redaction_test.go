package redaction_test

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/redaction"
)

// findHit returns the count for a named pattern in hits, or 0 if the
// pattern didn't fire. Test helper.
func findHit(hits []redaction.Hit, name string) int {
	for _, h := range hits {
		if h.Pattern == name {
			return h.Count
		}
	}
	return 0
}

// TestDefaultPatterns_PositiveCases asserts each default pattern
// matches a representative live-format example. If a regex regresses,
// this test fails on the specific pattern name.
func TestDefaultPatterns_PositiveCases(t *testing.T) {
	cases := []struct {
		pattern string
		sample  string
	}{
		{"github-pat-classic", "ghp_" + strings.Repeat("a", 36)},
		{"github-pat-fine-grained", "github_pat_" + strings.Repeat("a", 82)},
		{"github-app-token", "ghs_" + strings.Repeat("a", 36)},
		{"openai-api-key", "sk-" + strings.Repeat("A", 48)},
		{"openai-project-key", "sk-proj-" + strings.Repeat("A", 50)},
		{"anthropic-api-key", "sk-ant-api03-" + strings.Repeat("A", 50)},
		{"aws-access-key-id", "AKIAABCDEFGHIJKLMNOP"},
		{"authorization-bearer", "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig"},
		{"json-password-field", `"password": "swordfish"`},
		{"json-password-field", `"api_key": "abc123"`},
		{"json-password-field", `"access_token": "xyz789"`},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"/"+tc.sample[:min(20, len(tc.sample))], func(t *testing.T) {
			out, hits := redaction.RedactDefault([]byte(tc.sample))
			if findHit(hits, tc.pattern) == 0 {
				t.Errorf("expected pattern %q to fire on %q; hits = %+v", tc.pattern, tc.sample, hits)
			}
			if bytes.Contains(out, []byte(tc.sample)) {
				t.Errorf("redacted output still contains the original sample: %s", out)
			}
			if !bytes.Contains(out, []byte("[REDACTED:")) {
				t.Errorf("redacted output missing marker: %s", out)
			}
		})
	}
}

// TestDefaultPatterns_NegativeCases checks for false positives on
// strings that look secret-shaped but shouldn't match. If we
// accidentally redact ordinary text, that's a regression.
func TestDefaultPatterns_NegativeCases(t *testing.T) {
	cases := []string{
		"this is just normal prose with no secrets",
		"AKIA-but-not-a-key (only 4 trailing chars)",
		"sk-tooShort",                           // shorter than openai-api-key
		`Authorization: Basic dXNlcjpwYXNz`,     // Basic, not Bearer
		`"username": "alice"`,                   // not a redaction-tier field
		"github_pat_" + strings.Repeat("a", 80), // fine-grained PAT requires 82, not 80
		"akia0123456789abcdef",                  // lowercase doesn't match aws-access-key-id
	}
	for _, sample := range cases {
		t.Run(sample[:min(30, len(sample))], func(t *testing.T) {
			out, hits := redaction.RedactDefault([]byte(sample))
			if len(hits) != 0 {
				t.Errorf("expected no hits on %q; got %+v", sample, hits)
			}
			if !bytes.Equal(out, []byte(sample)) {
				t.Errorf("expected output unchanged on %q; got %s", sample, out)
			}
		})
	}
}

func TestRedact_MultipleMatchesInSameInput(t *testing.T) {
	input := []byte("first ghp_" + strings.Repeat("a", 36) + " then ghp_" + strings.Repeat("b", 36))
	out, hits := redaction.RedactDefault(input)
	if got := findHit(hits, "github-pat-classic"); got != 2 {
		t.Errorf("expected 2 hits, got %d", got)
	}
	if bytes.Contains(out, []byte("ghp_aaaa")) || bytes.Contains(out, []byte("ghp_bbbb")) {
		t.Errorf("redaction missed at least one match: %s", out)
	}
	// Both occurrences replaced with the same marker.
	if bytes.Count(out, []byte("[REDACTED:github-pat-classic]")) != 2 {
		t.Errorf("expected 2 markers, got: %s", out)
	}
}

func TestRedact_MixedPatterns(t *testing.T) {
	// A realistic-ish trace fragment with two distinct secret types.
	input := []byte(`{"prompt": "deploy with AKIA1234567890ABCDEF and ghp_` + strings.Repeat("c", 36) + `"}`)
	_, hits := redaction.RedactDefault(input)

	if findHit(hits, "aws-access-key-id") != 1 {
		t.Errorf("expected 1 aws-access-key-id hit; hits = %+v", hits)
	}
	if findHit(hits, "github-pat-classic") != 1 {
		t.Errorf("expected 1 github-pat-classic hit; hits = %+v", hits)
	}
	// Hits sorted alphabetically for determinism.
	for i := 1; i < len(hits); i++ {
		if hits[i].Pattern < hits[i-1].Pattern {
			t.Errorf("hits should be sorted: %+v", hits)
		}
	}
}

func TestRedact_EmptyInput(t *testing.T) {
	out, hits := redaction.RedactDefault(nil)
	if out != nil {
		t.Errorf("expected nil out, got %v", out)
	}
	if hits != nil {
		t.Errorf("expected nil hits, got %v", hits)
	}
}

func TestRedact_EmptyPatternList(t *testing.T) {
	in := []byte("ghp_" + strings.Repeat("a", 36))
	out, hits := redaction.Redact(in, nil)
	if !bytes.Equal(out, in) {
		t.Errorf("with no patterns, output should equal input")
	}
	if len(hits) != 0 {
		t.Errorf("expected no hits with no patterns; got %v", hits)
	}
}

func TestRedact_PreservesSurroundingBytes(t *testing.T) {
	// Bytes outside the matched region must pass through verbatim.
	prefix := []byte("BEGIN ")
	secret := []byte("ghp_" + strings.Repeat("z", 36))
	suffix := []byte(" END")
	input := bytes.Join([][]byte{prefix, secret, suffix}, nil)

	out, _ := redaction.RedactDefault(input)
	if !bytes.HasPrefix(out, prefix) {
		t.Errorf("prefix changed: %s", out)
	}
	if !bytes.HasSuffix(out, suffix) {
		t.Errorf("suffix changed: %s", out)
	}
	if bytes.Contains(out, secret) {
		t.Errorf("secret leaked through: %s", out)
	}
}

func TestRedact_CustomPatternWithCustomReplace(t *testing.T) {
	custom := []redaction.Pattern{
		{
			Name:    "test-pattern",
			Regex:   regexp.MustCompile(`SECRET-\d+`),
			Replace: "<masked>",
		},
	}
	out, hits := redaction.Redact([]byte("SECRET-123 and SECRET-456"), custom)
	if got := findHit(hits, "test-pattern"); got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
	if bytes.Count(out, []byte("<masked>")) != 2 {
		t.Errorf("expected 2 custom replacements: %s", out)
	}
	if bytes.Contains(out, []byte("SECRET-")) {
		t.Errorf("custom pattern missed a match: %s", out)
	}
}

func TestRedact_CustomPatternDefaultsReplace(t *testing.T) {
	// When Replace is empty, redactor falls back to
	// "[REDACTED:<name>]". This is the case most pattern definitions
	// use.
	custom := []redaction.Pattern{
		{
			Name:  "my-secret",
			Regex: regexp.MustCompile(`xyz-\d{3}`),
		},
	}
	out, _ := redaction.Redact([]byte("xyz-789"), custom)
	if !bytes.Equal(out, []byte("[REDACTED:my-secret]")) {
		t.Errorf("got %q, want default marker", out)
	}
}

// TestRedact_HitsAreDeterministic confirms multiple Redact calls on
// the same input produce the same hit slice (sorted by name). This
// matters because telemetry on hits should be reproducible — the
// same trace redacted twice should report identical patterns and
// counts.
func TestRedact_HitsAreDeterministic(t *testing.T) {
	input := []byte("AKIA1234567890ABCDEF\nghp_" + strings.Repeat("a", 36))
	_, h1 := redaction.RedactDefault(input)
	_, h2 := redaction.RedactDefault(input)
	if len(h1) != len(h2) {
		t.Fatalf("hit count differs: %d vs %d", len(h1), len(h2))
	}
	for i := range h1 {
		if h1[i] != h2[i] {
			t.Errorf("hit %d differs: %v vs %v", i, h1[i], h2[i])
		}
	}
}

// min is needed for substring slicing in test names; Go 1.25 has
// builtin min for ordered types.
func _testHelpersUnused() {} //nolint:unused
