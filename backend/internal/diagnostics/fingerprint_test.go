package diagnostics

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestFingerprint_StableForEqualInputs(t *testing.T) {
	a := Fingerprint("B", "policy_evaluated", "", "v0.4")
	b := Fingerprint("B", "policy_evaluated", "", "v0.4")
	if a != b {
		t.Fatalf("fingerprint not stable: %q != %q", a, b)
	}
	if len(a) != fingerprintLength {
		t.Errorf("length = %d, want %d", len(a), fingerprintLength)
	}
}

func TestFingerprint_NormalizesComponents(t *testing.T) {
	if Fingerprint("B", "policy_evaluated", "auth-401", "v0.4") != Fingerprint("  b ", "Policy_Evaluated", "Auth-401", "V0.4") {
		t.Error("fingerprint should be insensitive to case/whitespace in components")
	}
}

func TestFingerprint_VariesByComponent(t *testing.T) {
	base := Fingerprint("B", "policy_evaluated", "", "v0.4")
	cases := map[string]string{
		"error code":     Fingerprint("A", "policy_evaluated", "", "v0.4"),
		"surface":        Fingerprint("B", "agent_failed", "", "v0.4"),
		"detail class":   Fingerprint("B", "policy_evaluated", "auth-401", "v0.4"),
		"version family": Fingerprint("B", "policy_evaluated", "", "v0.5"),
	}
	for name, got := range cases {
		if got == base {
			t.Errorf("fingerprint did not vary with %s: both %q", name, got)
		}
	}
}

// TestFingerprint_VariesByDetailClass pins the #1962 behavior: two
// failures sharing error code + surface + version family but differing
// only in detail class (auth-401 vs bad-object-ref) fingerprint
// differently — the conflated-surface case now files separately.
func TestFingerprint_VariesByDetailClass(t *testing.T) {
	auth := Fingerprint("C", "fixup_base_checkout", "auth-401", "v0.4")
	badRef := Fingerprint("C", "fixup_base_checkout", "bad-object-ref", "v0.4")
	if auth == badRef {
		t.Errorf("distinct detail classes must fingerprint differently: both %q", auth)
	}
}

// TestFingerprint_EmptyDetailClass_BackwardCompatible is the done-means
// backward-compatibility test: an empty detail class reproduces the exact
// pre-change 3-component digest, so every currently-unclassified open
// report keeps deduping. The expected value is independently recomputed
// over the 3-component NUL-joined string (NOT via Fingerprint), so this
// fails if the empty-class path ever changes the hash input shape.
func TestFingerprint_EmptyDetailClass_BackwardCompatible(t *testing.T) {
	errorCode, surface, versionFamily := "C", "fixup_base_checkout", "v0.4"
	want := legacyThreeComponentDigest(errorCode, surface, versionFamily)
	if got := Fingerprint(errorCode, surface, "", versionFamily); got != want {
		t.Errorf("empty-class fingerprint = %q, want pre-change 3-component digest %q", got, want)
	}
}

// legacyThreeComponentDigest recomputes the pre-#1962 fingerprint over the
// 3-component NUL-joined normalized string, independent of Fingerprint.
func legacyThreeComponentDigest(errorCode, failingSurface, versionFamily string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		strings.ToLower(strings.TrimSpace(errorCode)),
		strings.ToLower(strings.TrimSpace(failingSurface)),
		strings.ToLower(strings.TrimSpace(versionFamily)),
	}, "\x00")))
	return hex.EncodeToString(sum[:])[:fingerprintLength]
}

func TestFingerprint_NoConcatenationCollision(t *testing.T) {
	// The NUL separator must keep ("a","bc") distinct from ("ab","c").
	if Fingerprint("a", "bc", "", "v0") == Fingerprint("ab", "c", "", "v0") {
		t.Error("concatenation collision: components must be unambiguously separated")
	}
}

func TestVersionFamily(t *testing.T) {
	cases := map[string]string{
		"v0.4.2":   "v0.4",
		"v0.4":     "v0.4",
		"1.2.3-rc": "1.2",
		"dev":      "dev",
		"unknown":  "unknown",
		"":         "dev",
	}
	for in, want := range cases {
		if got := VersionFamily(in); got != want {
			t.Errorf("VersionFamily(%q) = %q, want %q", in, got, want)
		}
	}
}
