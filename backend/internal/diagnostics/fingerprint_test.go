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

// TestFingerprint_DelegatesToFingerprintOf pins that Fingerprint produces the
// exact byte value FingerprintOf does over the same components (the empty
// detail class collapses to the 3-component form, a set class to the
// 4-component form) — the backward-compatibility contract for every open
// deduped report. FingerprintOf is the primitive Fingerprint delegates to, so
// a change to the normalize/NUL-join order that broke compatibility would
// break this equality.
func TestFingerprint_DelegatesToFingerprintOf(t *testing.T) {
	// 3-component (empty detail class): matches the legacy 3-component digest.
	if got, want := Fingerprint("C", "fixup_base_checkout", "", "v0.4"),
		FingerprintOf("C", "fixup_base_checkout", "v0.4"); got != want {
		t.Errorf("3-component: Fingerprint = %q, FingerprintOf = %q", got, want)
	}
	if got, want := Fingerprint("C", "fixup_base_checkout", "", "v0.4"),
		legacyThreeComponentDigest("C", "fixup_base_checkout", "v0.4"); got != want {
		t.Errorf("3-component: Fingerprint = %q, legacy golden = %q", got, want)
	}
	// 4-component (set detail class): matches FingerprintOf over the 4 parts.
	if got, want := Fingerprint("C", "fixup_base_checkout", "auth-401", "v0.4"),
		FingerprintOf("C", "fixup_base_checkout", "auth-401", "v0.4"); got != want {
		t.Errorf("4-component: Fingerprint = %q, FingerprintOf = %q", got, want)
	}
}

// TestDescriptionDigest pins the whitespace/case invariance, the difference
// across distinct text, and the empty-on-whitespace contract (#3233).
func TestDescriptionDigest(t *testing.T) {
	a := DescriptionDigest("The planner mis-ordered my stages")
	b := DescriptionDigest("  the  PLANNER   mis-ordered\tmy\nstages ")
	if a != b {
		t.Errorf("case/whitespace variants must digest identically: %q != %q", a, b)
	}
	if a == "" {
		t.Error("a non-empty description must produce a non-empty digest")
	}
	if DescriptionDigest("a different description") == a {
		t.Error("distinct descriptions must digest differently")
	}
	for _, ws := range []string{"", "   ", "\t\n ", "   "} {
		if got := DescriptionDigest(ws); got != "" {
			t.Errorf("DescriptionDigest(%q) = %q, want empty", ws, got)
		}
	}
}

// healthyBundle builds a no-failing-stage bundle for the ReportFingerprint
// table.
func healthyBundle(runID, workflowID string) DiagnosticBundle {
	return DiagnosticBundle{
		RunID:      runID,
		WorkflowID: workflowID,
		RunState:   "running",
		Versions:   VersionFacts{Fishhawkd: Component{Version: "0.4.2"}},
	}
}

// TestReportFingerprint pins the three keyings, their Components lists, the
// DedupSearched flag, and the cross-workflow / cross-run / cross-description
// discrimination (#3233).
func TestReportFingerprint(t *testing.T) {
	// Failure keying: ignores the description, lists detail class only when set.
	failing := DiagnosticBundle{
		RunID:      "run-1",
		WorkflowID: "feature_change",
		RunState:   "failed",
		FailingStage: &FailingStage{
			Type:            "implement",
			FailureCategory: "B",
			FailureSurface:  "scope_violation",
		},
		Versions: VersionFacts{Fishhawkd: Component{Version: "0.4.2"}},
	}
	fpNoText, basisNoText := ReportFingerprint(failing, "")
	fpText, _ := ReportFingerprint(failing, "some operator prose")
	if fpNoText != fpText {
		t.Errorf("failure keying must ignore the description: %q != %q", fpNoText, fpText)
	}
	if basisNoText.Kind != FingerprintKindFailure || basisNoText.DedupSearched != true {
		t.Errorf("failure basis = %+v, want kind=failure dedup_searched=true", basisNoText)
	}
	if got, want := basisNoText.Components, []string{"failure_category", "failure_surface", "version_family"}; !slicesEqual(got, want) {
		t.Errorf("failure components (no detail class) = %v, want %v", got, want)
	}
	withClass := failing
	fs := *failing.FailingStage
	fs.FailureDetailClass = "auth-401"
	withClass.FailingStage = &fs
	_, basisClass := ReportFingerprint(withClass, "")
	if got, want := basisClass.Components, []string{"failure_category", "failure_surface", "failure_detail_class", "version_family"}; !slicesEqual(got, want) {
		t.Errorf("failure components (detail class) = %v, want %v", got, want)
	}

	// Healthy, different workflow -> different fingerprint under BOTH healthy
	// kinds. No description -> healthy_unique.
	uA, basisUA := ReportFingerprint(healthyBundle("run-x", "backlog_grooming"), "")
	uB, basisUB := ReportFingerprint(healthyBundle("run-x", "feature_change"), "")
	if uA == uB {
		t.Errorf("healthy_unique fingerprints must differ by workflow: both %q", uA)
	}
	if basisUA.Kind != FingerprintKindHealthyUnique || basisUA.DedupSearched != false {
		t.Errorf("healthy_unique basis = %+v, want kind=healthy_unique dedup_searched=false", basisUA)
	}
	if got, want := basisUA.Components, []string{"run_state", "workflow_id", "run_id", "version_family"}; !slicesEqual(got, want) {
		t.Errorf("healthy_unique components = %v, want %v", got, want)
	}
	_ = basisUB
	// healthy_unique differs across two run ids.
	uR1, _ := ReportFingerprint(healthyBundle("run-1", "feature_change"), "")
	uR2, _ := ReportFingerprint(healthyBundle("run-2", "feature_change"), "")
	if uR1 == uR2 {
		t.Errorf("healthy_unique fingerprints must differ by run id: both %q", uR1)
	}

	// With a description -> healthy_description, keyed on the digest. Different
	// workflows differ; same workflow + normalized-same description matches.
	dA, basisD := ReportFingerprint(healthyBundle("run-a", "backlog_grooming"), "grooming apply lost the tail")
	dB, _ := ReportFingerprint(healthyBundle("run-b", "feature_change"), "grooming apply lost the tail")
	if dA == dB {
		t.Errorf("healthy_description fingerprints must differ by workflow: both %q", dA)
	}
	if basisD.Kind != FingerprintKindHealthyDescription || basisD.DedupSearched != true {
		t.Errorf("healthy_description basis = %+v, want kind=healthy_description dedup_searched=true", basisD)
	}
	if got, want := basisD.Components, []string{"run_state", "workflow_id", "description_digest", "version_family"}; !slicesEqual(got, want) {
		t.Errorf("healthy_description components = %v, want %v", got, want)
	}
	same1, _ := ReportFingerprint(healthyBundle("run-p", "backlog_grooming"), "The Same Text")
	same2, _ := ReportFingerprint(healthyBundle("run-q", "backlog_grooming"), "  the   same    text ")
	if same1 != same2 {
		t.Errorf("same workflow + normalized-same description must match: %q != %q", same1, same2)
	}
	diff, _ := ReportFingerprint(healthyBundle("run-r", "backlog_grooming"), "a wholly different description")
	if diff == same1 {
		t.Errorf("same workflow + different description must differ: both %q", diff)
	}
}

// slicesEqual is a local string-slice equality helper (the package targets a
// go.mod pin without slices.Equal guaranteed in scope).
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
