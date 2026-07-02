package corpusdistill

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/agenteval"
)

// class3Item builds an AuditItem whose payload is an acceptance_triage_decided
// class-3 entry built via the SAME agenteval.PlanReviewMiss type the server
// marshals (the server → tool seam).
func class3Item(t *testing.T, seq int64, misses []agenteval.PlanReviewMiss) AuditItem {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"run_id":           "11111111-2222-3333-4444-555555555555",
		"artifact_id":      "art-1",
		"class":            "3",
		"disposition":      "paged",
		"reason":           "bad criterion",
		"plan_review_miss": misses,
	})
	if err != nil {
		t.Fatal(err)
	}
	return AuditItem{Sequence: seq, RunID: "11111111-2222-3333-4444-555555555555",
		Timestamp: "2026-07-01T12:00:00Z", Payload: payload}
}

func otherClassItem(t *testing.T, seq int64, class string) AuditItem {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"class": class, "disposition": "fixup_dispatched"})
	if err != nil {
		t.Fatal(err)
	}
	return AuditItem{Sequence: seq, Payload: payload}
}

func sampleMisses() []agenteval.PlanReviewMiss {
	return []agenteval.PlanReviewMiss{{
		CriterionID: "ac-list", Statement: "GET /widgets lists widgets",
		Source: "inferred", Rationale: "listing implied",
		Observed: "unpaginated array", Expected: "a widget list",
		StepsTaken: "GET /widgets", ExpectationBasis: "criterion ac-list",
		ReproHandle: "curl $TARGET/widgets", Result: "failed",
	}}
}

func missOpts(outDir string) MissOptions {
	return MissOptions{CaseName: "miss-case", Issue: "#1539", OutDir: outDir}
}

// TestDistillPlanReviewMiss_SeamRoundTrip is the E31.11 cross-boundary seam
// test: a payload built from agenteval.PlanReviewMiss flows through
// DistillPlanReviewMiss into a case dir, loads back with
// agenteval.LoadPlanReviewMissCorpus, and every provenance + observed field
// round-trips losslessly. Non-class-3 items are filtered out.
func TestDistillPlanReviewMiss_SeamRoundTrip(t *testing.T) {
	out := t.TempDir()
	items := []AuditItem{
		otherClassItem(t, 1, "1"),
		class3Item(t, 2, sampleMisses()),
		otherClassItem(t, 3, "2"),
	}

	dirs, err := DistillPlanReviewMiss(items, missOpts(out))
	if err != nil {
		t.Fatalf("DistillPlanReviewMiss: %v", err)
	}
	if len(dirs) != 1 {
		t.Fatalf("case dirs = %d, want 1 (class-3 only)", len(dirs))
	}

	cases, err := agenteval.LoadPlanReviewMissCorpus(out)
	if err != nil {
		t.Fatalf("LoadPlanReviewMissCorpus over distilled output: %v", err)
	}
	if len(cases) != 1 {
		t.Fatalf("loaded cases = %d, want 1", len(cases))
	}
	c := cases[0].Case
	if c.RunID != "11111111-2222-3333-4444-555555555555" || c.ArtifactID != "art-1" ||
		c.TriageSequence != 2 || c.Class != "3" || c.Disposition != "paged" ||
		c.Reason != "bad criterion" || c.DecidedAt != "2026-07-01T12:00:00Z" {
		t.Errorf("triage envelope not carried: %+v", c)
	}
	if c.Synthetic {
		t.Error("distilled case must be Synthetic=false")
	}
	want := sampleMisses()[0]
	if len(c.Misses) != 1 || c.Misses[0] != want {
		t.Errorf("miss record did not round-trip:\ngot  %+v\nwant %+v", c.Misses, want)
	}
	// case.md exists alongside.
	if _, err := os.Stat(filepath.Join(dirs[0], "case.md")); err != nil {
		t.Errorf("case.md not written: %v", err)
	}
}

// TestDistillPlanReviewMiss_MultipleCasesSuffixed: several class-3 decisions
// in one run get -2, -3 suffixed case dirs.
func TestDistillPlanReviewMiss_MultipleCasesSuffixed(t *testing.T) {
	out := t.TempDir()
	items := []AuditItem{class3Item(t, 1, sampleMisses()), class3Item(t, 2, sampleMisses())}
	dirs, err := DistillPlanReviewMiss(items, missOpts(out))
	if err != nil {
		t.Fatalf("DistillPlanReviewMiss: %v", err)
	}
	if len(dirs) != 2 {
		t.Fatalf("case dirs = %d, want 2", len(dirs))
	}
	if filepath.Base(dirs[0]) != "miss-case" || filepath.Base(dirs[1]) != "miss-case-2" {
		t.Errorf("case dir names = %v, want [miss-case miss-case-2]", dirs)
	}
}

// TestDistillPlanReviewMiss_ZeroClass3_Errors pins the fail-loud contract:
// no class-3 entries is an error, never an empty success.
func TestDistillPlanReviewMiss_ZeroClass3_Errors(t *testing.T) {
	_, err := DistillPlanReviewMiss([]AuditItem{otherClassItem(t, 1, "1")}, missOpts(t.TempDir()))
	if err == nil {
		t.Fatal("expected error on zero class-3 entries, got nil")
	}
	if !strings.Contains(err.Error(), "no class-3") {
		t.Errorf("error does not state the zero-class-3 cause: %v", err)
	}
}

// TestDistillPlanReviewMiss_Class3WithoutMisses_Errors: class-3 entries that
// carry no plan_review_miss (a pre-#1539 backend) also fail loud with a
// distinct message.
func TestDistillPlanReviewMiss_Class3WithoutMisses_Errors(t *testing.T) {
	_, err := DistillPlanReviewMiss([]AuditItem{class3Item(t, 1, nil)}, missOpts(t.TempDir()))
	if err == nil {
		t.Fatal("expected error on class-3 without plan_review_miss, got nil")
	}
	if !strings.Contains(err.Error(), "plan_review_miss") {
		t.Errorf("error does not name the missing field: %v", err)
	}
}

// TestDistillPlanReviewMiss_UndecodablePayload_NamesSequence: a payload that
// fails to decode is an error naming the item's sequence.
func TestDistillPlanReviewMiss_UndecodablePayload_NamesSequence(t *testing.T) {
	items := []AuditItem{{Sequence: 77, Payload: json.RawMessage(`{"class":`)}}
	_, err := DistillPlanReviewMiss(items, missOpts(t.TempDir()))
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "77") {
		t.Errorf("error does not name the sequence: %v", err)
	}
}

// TestDistillPlanReviewMiss_OverwriteGuard: an existing case dir errors
// without Force and is overwritten with it.
func TestDistillPlanReviewMiss_OverwriteGuard(t *testing.T) {
	out := t.TempDir()
	items := []AuditItem{class3Item(t, 1, sampleMisses())}
	if _, err := DistillPlanReviewMiss(items, missOpts(out)); err != nil {
		t.Fatalf("first distill: %v", err)
	}
	if _, err := DistillPlanReviewMiss(items, missOpts(out)); err == nil {
		t.Fatal("expected already-exists error without Force, got nil")
	} else if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error does not point at --force: %v", err)
	}
	opts := missOpts(out)
	opts.Force = true
	if _, err := DistillPlanReviewMiss(items, opts); err != nil {
		t.Fatalf("distill with Force: %v", err)
	}
}

// TestDistillPlanReviewMiss_RequiredFieldsAndCaseName covers the option
// guards shared with the trace mode: missing CaseName/Issue/OutDir and an
// unsafe CaseName each error before any filesystem effect.
func TestDistillPlanReviewMiss_RequiredFieldsAndCaseName(t *testing.T) {
	items := []AuditItem{class3Item(t, 1, sampleMisses())}
	tests := []struct {
		name string
		opts MissOptions
	}{
		{"missing CaseName", MissOptions{Issue: "#1539", OutDir: "x"}},
		{"missing Issue", MissOptions{CaseName: "c", OutDir: "x"}},
		{"missing OutDir", MissOptions{CaseName: "c", Issue: "#1539"}},
		{"unsafe CaseName", MissOptions{CaseName: "../escape", Issue: "#1539", OutDir: "x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DistillPlanReviewMiss(items, tc.opts); err == nil {
				t.Error("expected option-validation error, got nil")
			}
		})
	}
}

// TestPreviewPlanReviewMiss_WritesNothing pins the --dry-run contract: the
// preview returns the would-be cases and touches no filesystem.
func TestPreviewPlanReviewMiss_WritesNothing(t *testing.T) {
	out := t.TempDir()
	items := []AuditItem{class3Item(t, 1, sampleMisses())}
	results, err := PreviewPlanReviewMiss(items, missOpts(out))
	if err != nil {
		t.Fatalf("PreviewPlanReviewMiss: %v", err)
	}
	if len(results) != 1 || len(results[0].MissJSON) == 0 || results[0].CaseMD == "" {
		t.Fatalf("preview results incomplete: %+v", results)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("preview wrote entries under OutDir: %v", entries)
	}
}

// TestRenderMissCaseMD_Provenance pins the two provenance branches: the
// fetched path asserts PRODUCTION + redacted-by-construction (ADR-049 #5);
// the operator-supplied path emits the TODO(operator) prompt. The narrative
// flag pre-fills the distilled-signal section.
func TestRenderMissCaseMD_Provenance(t *testing.T) {
	items := []AuditItem{class3Item(t, 1, sampleMisses())}

	fetched := missOpts(t.TempDir())
	fetched.Fetched = true
	res, err := PreviewPlanReviewMiss(items, fetched)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Provenance: PRODUCTION", "redacted-by-construction", "ADR-049 #5"} {
		if !strings.Contains(res[0].CaseMD, want) {
			t.Errorf("fetched case.md missing %q:\n%s", want, res[0].CaseMD)
		}
	}

	supplied := missOpts(t.TempDir())
	supplied.Narrative = "The reviewer should have demanded a source_ref."
	res, err = PreviewPlanReviewMiss(items, supplied)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res[0].CaseMD, "Provenance: TODO(operator)") {
		t.Errorf("operator-supplied case.md missing the provenance TODO:\n%s", res[0].CaseMD)
	}
	if !strings.Contains(res[0].CaseMD, supplied.Narrative) {
		t.Errorf("case.md missing the narrative:\n%s", res[0].CaseMD)
	}
	if strings.Contains(res[0].CaseMD, "TODO(operator): describe why") {
		t.Errorf("narrative-labeled case.md still emits the distilled-signal TODO:\n%s", res[0].CaseMD)
	}
}
