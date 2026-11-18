package permdrift

import (
	"regexp"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/diffsecrets"
)

// allowlistKeyShape is `diff_secrets_allowlist.<list>.<id>.<16hex>`.
var allowlistKeyShape = regexp.MustCompile(`^diff_secrets_allowlist\.(entries|markers)\.[a-z0-9][a-z0-9-]*\.[0-9a-f]{16}$`)

func TestExtractDiffSecretsAllowlist(t *testing.T) {
	doc := "version: 1\nentries:\n" +
		"  - id: two-globs\n    paths: [\"a/**\", \"b/**\"]\n    patterns: [\"github-pat-classic\", \"aws-access-key-id\"]\n    reason: r\n" +
		"  - id: Rejected_ID\n    paths: [\"c/**\"]\n    patterns: [\"github-pat-classic\"]\n    reason: r\n" +
		"markers:\n  - id: fix\n    paths: [\"fix/**\"]\n    patterns: [\"github-pat-classic\"]\n    reason: r\n"
	g, err := ExtractDiffSecretsAllowlist([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, gl := range []string{"a/**", "b/**"} {
		for _, p := range []string{"github-pat-classic", "aws-access-key-id"} {
			want["diff_secrets_allowlist.entries.two-globs."+diffsecrets.RuleFingerprint(diffsecrets.ListEntries, gl, p)] = true
		}
	}
	want["diff_secrets_allowlist.markers.fix."+diffsecrets.RuleFingerprint(diffsecrets.ListMarkers, "fix/**", "github-pat-classic")] = true
	if len(g) != len(want) {
		t.Fatalf("grants = %d (%v), want %d: one per (list, glob, pattern) of ACCEPTED rules only", len(g), g, len(want))
	}
	for k, e := range g {
		if !want[k] || !allowlistKeyShape.MatchString(k) {
			t.Errorf("unexpected key %q", k)
		}
		if e.Value != Present || e.Polarity != Grant || e.Rank != PresenceRank {
			t.Errorf("%s: entry %+v, want a presence Grant", k, e)
		}
	}
	if g, err := ExtractDiffSecretsAllowlist(nil); err != nil || len(g) != 0 {
		t.Errorf("absent file: %v, %v", g, err)
	}
	if _, err := ExtractDiffSecretsAllowlist([]byte("version: 3\n")); err == nil {
		t.Error("unsupported version did not error")
	}
}

// TestExtractDiffSecretsAllowlist_NoGlobBytesInKeys (C1): no glob byte enters
// a key, a Value or a widening note.
//
// Arm (i) COUNTERFACTUAL: replace the fingerprint segment with
// keySegment(glob) — the sentinel glob then sits in the key, RED. Arm (ii)
// COUNTERFACTUAL: disable diffsecrets' redaction-hit check — the credential
// rule then loads and emits a Grant, so the zero-count assertion goes RED.
// With BOTH mutations ("copy the glob verbatim"), the credential bytes are in
// a key and the no-bytes assertion goes RED.
func TestExtractDiffSecretsAllowlist_NoGlobBytesInKeys(t *testing.T) {
	sentinel := "sentinel-" + strings.Repeat("q7", 6)
	cred := "ghp_" + strings.Repeat("A1b2C3d4E5", 4)[:36]
	rule := func(id, glob string) string {
		return "  - id: " + id + "\n    paths: [\"" + glob + "/**\"]\n    patterns: [\"github-pat-classic\"]\n    reason: r\n"
	}
	t.Run("sentinel glob loads but never enters a key, value or note", func(t *testing.T) {
		g, err := ExtractDiffSecretsAllowlist([]byte("version: 1\nentries:\n" + rule("sent", sentinel)))
		if err != nil || len(g) != 1 {
			t.Fatalf("grants = %v, %v; want 1", g, err)
		}
		s := MatchSurfaces(DefaultSurfaces(), diffsecrets.AllowlistPath)[0]
		for k, e := range g {
			note := Note(s, diffsecrets.AllowlistPath, Change{Key: k, Before: Absent, After: e.Value, Direction: Widened})
			for _, field := range []string{k, e.Value, e.Except, note} {
				if strings.Contains(field, sentinel) {
					t.Errorf("field carries the sentinel glob: %q", field)
				}
			}
		}
	})
	t.Run("credential glob is rejected and emits nothing", func(t *testing.T) {
		g, err := ExtractDiffSecretsAllowlist([]byte("version: 1\nentries:\n" + rule("cred", cred)))
		if err != nil {
			t.Fatal(err)
		}
		if len(g) != 0 {
			t.Errorf("grants = %v, want none for a redaction-hit rule", g)
		}
		for k := range g {
			if strings.Contains(k, cred) {
				t.Errorf("key carries the credential: %q", k)
			}
		}
	})
}
