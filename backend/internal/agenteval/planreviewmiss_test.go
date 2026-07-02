package agenteval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadPlanReviewMissCorpus_CommittedSeed loads the committed synthetic
// seed case and asserts its shape carried through — the corpus loader's
// happy path against real committed fixture bytes.
func TestLoadPlanReviewMissCorpus_CommittedSeed(t *testing.T) {
	cases, err := LoadPlanReviewMissCorpus("testdata/planreview-miss-corpus")
	if err != nil {
		t.Fatalf("LoadPlanReviewMissCorpus: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("committed corpus loaded zero cases; the seed case is missing")
	}
	var seed *NamedPlanReviewMissCase
	for i := range cases {
		if cases[i].Name == "seed-synthetic-inferred-criterion" {
			seed = &cases[i]
			break
		}
	}
	if seed == nil {
		t.Fatal("seed-synthetic-inferred-criterion not found in loaded corpus")
	}
	if !seed.Case.Synthetic {
		t.Error("seed case must be marked synthetic")
	}
	if seed.Case.Class != "3" {
		t.Errorf("seed class = %q, want %q", seed.Case.Class, "3")
	}
	if len(seed.Case.Misses) != 1 {
		t.Fatalf("seed misses = %d, want 1", len(seed.Case.Misses))
	}
	m := seed.Case.Misses[0]
	if m.CriterionID != "ac-list-pagination" || m.Source != "inferred" ||
		m.Observed == "" || m.Rationale == "" {
		t.Errorf("seed miss fields not carried: %+v", m)
	}
}

// TestLoadPlanReviewMissCorpus_AbsentDir asserts the absent-corpus branch:
// empty slice, nil error (the corpus starts empty in most checkouts).
func TestLoadPlanReviewMissCorpus_AbsentDir(t *testing.T) {
	cases, err := LoadPlanReviewMissCorpus(filepath.Join(t.TempDir(), "no-such-corpus"))
	if err != nil {
		t.Fatalf("absent dir must not error, got: %v", err)
	}
	if len(cases) != 0 {
		t.Errorf("absent dir cases = %d, want 0", len(cases))
	}
}

// writeMissCase writes dir/<name>/miss.json with the given content.
func writeMissCase(t *testing.T, dir, name, content string) {
	t.Helper()
	caseDir := filepath.Join(dir, name)
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caseDir, "miss.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLoadPlanReviewMissCorpus_FailClosed covers each fail-closed loader
// branch: malformed JSON, a case dir with no miss.json, an empty misses
// list, and a miss with an empty criterion_id — each an error naming the
// case.
func TestLoadPlanReviewMissCorpus_FailClosed(t *testing.T) {
	valid := `{"run_id":"r","class":"3","misses":[{"criterion_id":"ac-1"}],"synthetic":true}`
	tests := []struct {
		name    string
		setup   func(t *testing.T, dir string)
		wantSub string
	}{
		{
			name: "malformed JSON",
			setup: func(t *testing.T, dir string) {
				writeMissCase(t, dir, "bad-json", `{"run_id":`)
			},
			wantSub: "bad-json",
		},
		{
			name: "case dir with no miss.json",
			setup: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "no-miss-json"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "no-miss-json",
		},
		{
			name: "empty misses",
			setup: func(t *testing.T, dir string) {
				writeMissCase(t, dir, "empty-misses", `{"run_id":"r","class":"3","misses":[],"synthetic":true}`)
			},
			wantSub: "empty-misses",
		},
		{
			name: "empty criterion id",
			setup: func(t *testing.T, dir string) {
				writeMissCase(t, dir, "empty-id", `{"run_id":"r","class":"3","misses":[{"criterion_id":""}],"synthetic":true}`)
			},
			wantSub: "empty-id",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// A sibling valid case proves the error is about the broken one,
			// not a general load failure.
			writeMissCase(t, dir, "a-valid-case", valid)
			tc.setup(t, dir)
			_, err := LoadPlanReviewMissCorpus(dir)
			if err == nil {
				t.Fatal("expected fail-closed error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error does not name the case %q: %v", tc.wantSub, err)
			}
		})
	}
}

// TestLoadPlanReviewMissCorpus_ValidRoundTrip pins that a well-formed case
// written by a producer loads back losslessly (the loader half of the
// server → tool → loader seam).
func TestLoadPlanReviewMissCorpus_ValidRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeMissCase(t, dir, "roundtrip", `{
		"run_id":"11111111-2222-3333-4444-555555555555",
		"artifact_id":"art-1",
		"triage_sequence":7,
		"class":"3",
		"disposition":"paged",
		"reason":"bad criterion",
		"decided_at":"2026-07-01T00:00:00Z",
		"misses":[{"criterion_id":"ac-x","statement":"s","source":"inferred","source_ref":"ref","rationale":"why","observed":"o","expected":"e","steps_taken":"st","expectation_basis":"eb","repro_handle":"rh","result":"failed"}],
		"synthetic":false
	}`)
	cases, err := LoadPlanReviewMissCorpus(dir)
	if err != nil {
		t.Fatalf("LoadPlanReviewMissCorpus: %v", err)
	}
	if len(cases) != 1 {
		t.Fatalf("cases = %d, want 1", len(cases))
	}
	c := cases[0].Case
	if c.TriageSequence != 7 || c.Disposition != "paged" || c.DecidedAt != "2026-07-01T00:00:00Z" {
		t.Errorf("envelope fields not carried: %+v", c)
	}
	m := c.Misses[0]
	for field, got := range map[string]string{
		"statement": m.Statement, "source": m.Source, "source_ref": m.SourceRef,
		"rationale": m.Rationale, "observed": m.Observed, "expected": m.Expected,
		"steps_taken": m.StepsTaken, "expectation_basis": m.ExpectationBasis,
		"repro_handle": m.ReproHandle, "result": m.Result,
	} {
		if got == "" {
			t.Errorf("miss field %s dropped in round-trip", field)
		}
	}
}
