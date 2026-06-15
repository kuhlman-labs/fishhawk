package diagnostics

import "testing"

func TestFingerprint_StableForEqualInputs(t *testing.T) {
	a := Fingerprint("B", "policy_evaluated", "v0.4")
	b := Fingerprint("B", "policy_evaluated", "v0.4")
	if a != b {
		t.Fatalf("fingerprint not stable: %q != %q", a, b)
	}
	if len(a) != fingerprintLength {
		t.Errorf("length = %d, want %d", len(a), fingerprintLength)
	}
}

func TestFingerprint_NormalizesComponents(t *testing.T) {
	if Fingerprint("B", "policy_evaluated", "v0.4") != Fingerprint("  b ", "Policy_Evaluated", "V0.4") {
		t.Error("fingerprint should be insensitive to case/whitespace in components")
	}
}

func TestFingerprint_VariesByComponent(t *testing.T) {
	base := Fingerprint("B", "policy_evaluated", "v0.4")
	cases := map[string]string{
		"error code":     Fingerprint("A", "policy_evaluated", "v0.4"),
		"surface":        Fingerprint("B", "agent_failed", "v0.4"),
		"version family": Fingerprint("B", "policy_evaluated", "v0.5"),
	}
	for name, got := range cases {
		if got == base {
			t.Errorf("fingerprint did not vary with %s: both %q", name, got)
		}
	}
}

func TestFingerprint_NoConcatenationCollision(t *testing.T) {
	// The NUL separator must keep ("a","bc") distinct from ("ab","c").
	if Fingerprint("a", "bc", "v0") == Fingerprint("ab", "c", "v0") {
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
