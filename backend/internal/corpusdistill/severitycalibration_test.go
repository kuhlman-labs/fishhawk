package corpusdistill

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/agenteval"
)

// ---------------------------------------------------------------------------
// Fixture builders. Defined HERE rather than by extending an existing test
// file's fakes, so this slice adds no unscoped test surface.
// ---------------------------------------------------------------------------

func reviewItem(t *testing.T, seq int64, runID, model string, concerns ...[3]string) CalibrationAuditItem {
	t.Helper()
	type c struct {
		Severity string `json:"severity"`
		Category string `json:"category"`
		Note     string `json:"note"`
	}
	body := struct {
		ReviewerModel string `json:"reviewer_model"`
		Verdict       string `json:"verdict"`
		Concerns      []c    `json:"concerns"`
	}{ReviewerModel: model, Verdict: "reject"}
	for _, cc := range concerns {
		body.Concerns = append(body.Concerns, c{Severity: cc[0], Category: cc[1], Note: cc[2]})
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal review payload: %v", err)
	}
	return CalibrationAuditItem{Sequence: seq, RunID: runID, Category: categoryImplementReviewed, Payload: raw}
}

func dispositionItem(t *testing.T, seq int64, runID, category, concernID, severity, cat, reason string) CalibrationAuditItem {
	t.Helper()
	fields := map[string]any{
		"concern_id":  concernID,
		"prior_state": "open",
		"reason":      reason,
		"stage_kind":  "implement",
	}
	if severity != "" {
		fields["severity"] = severity
	}
	if cat != "" {
		fields["category"] = cat
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal disposition payload: %v", err)
	}
	return CalibrationAuditItem{Sequence: seq, RunID: runID, Category: category, Payload: raw}
}

func calibrationOpts(t *testing.T) CalibrationOptions {
	t.Helper()
	return CalibrationOptions{
		CaseName: "cal-case",
		Issue:    "#3309",
		OutDir:   t.TempDir(),
		Diff:     "--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-a\n+b\n",
	}
}

// happyItems is the shared two-concern, two-disposition fixture.
func happyItems(t *testing.T) []CalibrationAuditItem {
	t.Helper()
	return []CalibrationAuditItem{
		reviewItem(t, 10, "run-1", "claude-fable-5",
			[3]string{"high", "correctness", "unbounded read in the mirror path"},
			[3]string{"medium", "test-coverage", "no counterfactual for the new guard"}),
		dispositionItem(t, 20, "run-1", categoryConcernWaived, "c-1", "high", "correctness", "sibling code already does this"),
		dispositionItem(t, 21, "run-1", categoryConcernDeferred, "c-2", "medium", "test-coverage", "filed as #9999"),
	}
}

// assertNoCaseDirWritten reads the output directory AFTER the call returned.
// Error identity alone would not distinguish a refusal from a write-then-error
// (trap (a) in the counterfactual rules).
func assertNoCaseDirWritten(t *testing.T, outDir string) {
	t.Helper()
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read out dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected NO case directory written, found %d entries", len(entries))
	}
}

// ---------------------------------------------------------------------------
// CROSS-BOUNDARY end-to-end: writer -> operator labelling -> agenteval loader.
// ---------------------------------------------------------------------------

// TestDistillSeverityCalibration_RoundTripsThroughLoader drives the WHOLE
// corpusdistill -> agenteval seam in one test: the distiller writes a real
// case directory, the operator-labelling step is simulated by stamping
// operator_severity, and agenteval.LoadSeverityCalibrationCorpus reads that
// same directory back. A tag rename or field drift on EITHER side of the
// writer/loader seam fails HERE, where two per-layer unit tests would both
// stay green.
func TestDistillSeverityCalibration_RoundTripsThroughLoader(t *testing.T) {
	opts := calibrationOpts(t)
	dir, err := DistillSeverityCalibration(happyItems(t), opts)
	if err != nil {
		t.Fatalf("distill: %v", err)
	}

	// The operator labels the candidate.
	raw, err := os.ReadFile(filepath.Join(dir, "case.json"))
	if err != nil {
		t.Fatalf("read case.json: %v", err)
	}
	var c agenteval.SeverityCalibrationCase
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse written case.json: %v", err)
	}
	if len(c.Concerns) != 2 {
		t.Fatalf("want 2 joined concerns, got %d", len(c.Concerns))
	}
	c.Concerns[0].OperatorSeverity = "low"
	c.Concerns[1].OperatorSeverity = "medium"
	labelled, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatalf("marshal labelled case: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "case.json"), labelled, 0o644); err != nil {
		t.Fatalf("write labelled case.json: %v", err)
	}

	loaded, err := agenteval.LoadSeverityCalibrationCorpus(opts.OutDir)
	if err != nil {
		t.Fatalf("load labelled corpus: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("want 1 loaded case, got %d", len(loaded))
	}
	got := loaded[0].Case

	// Field-for-field against the INPUT audit payloads, both directions of
	// the seam.
	want := []agenteval.LabelledConcern{{
		ConcernID: "c-1", ReviewerModel: "claude-fable-5", Severity: "high", Category: "correctness",
		Note: "unbounded read in the mirror path", Disposition: "waived",
		DispositionReason: "sibling code already does this", OperatorSeverity: "low",
	}, {
		ConcernID: "c-2", ReviewerModel: "claude-fable-5", Severity: "medium", Category: "test-coverage",
		Note: "no counterfactual for the new guard", Disposition: "deferred",
		DispositionReason: "filed as #9999", OperatorSeverity: "medium",
	}}
	if len(got.Concerns) != len(want) {
		t.Fatalf("want %d concerns, got %d", len(want), len(got.Concerns))
	}
	for i := range want {
		if got.Concerns[i] != want[i] {
			t.Errorf("concern %d round-trip mismatch:\n got %+v\nwant %+v", i, got.Concerns[i], want[i])
		}
	}
	if got.RunID != "run-1" {
		t.Errorf("run_id = %q, want run-1", got.RunID)
	}
	if got.Synthetic {
		t.Error("a DISTILLED case must not be marked synthetic: true")
	}
}

// TestDistillSeverityCalibration_UnlabelledOutputIsRefusedByLoader is the
// property that makes "labelled" mean something: the tool's UNEDITED output
// must not load. It is the end-to-end half of agenteval loader mode (g).
func TestDistillSeverityCalibration_UnlabelledOutputIsRefusedByLoader(t *testing.T) {
	opts := calibrationOpts(t)
	if _, err := DistillSeverityCalibration(happyItems(t), opts); err != nil {
		t.Fatalf("distill: %v", err)
	}
	_, err := agenteval.LoadSeverityCalibrationCorpus(opts.OutDir)
	if err == nil {
		t.Fatal("expected the loader to REFUSE the tool's unlabelled output, got nil error")
	}
	if !strings.Contains(err.Error(), "operator_severity") {
		t.Errorf("refusal should name operator_severity, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Shipped rendered output.
// ---------------------------------------------------------------------------

// TestDistillSeverityCalibration_CaseMarkdownWarnsAboutFreeText asserts the
// MEDIUM fix as SHIPPED BEHAVIOR, reading the WRITTEN case.md rather than a
// return value: the point-of-use free-text warning is present and the
// phrase 'redacted-by-construction' appears NOWHERE in the written output.
func TestDistillSeverityCalibration_CaseMarkdownWarnsAboutFreeText(t *testing.T) {
	opts := calibrationOpts(t)
	dir, err := DistillSeverityCalibration(happyItems(t), opts)
	if err != nil {
		t.Fatalf("distill: %v", err)
	}
	md, err := os.ReadFile(filepath.Join(dir, "case.md"))
	if err != nil {
		t.Fatalf("read case.md: %v", err)
	}
	got := string(md)
	for _, want := range []string{
		"TODO(operator): REVIEW FREE TEXT BEFORE COMMITTING",
		"disposition_reason",
		"FREE TEXT",
		"COMMITTED TO THE REPOSITORY",
		"READ every note",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("case.md is missing the free-text warning fragment %q", want)
		}
	}
	js, err := os.ReadFile(filepath.Join(dir, "case.json"))
	if err != nil {
		t.Fatalf("read case.json: %v", err)
	}
	for name, body := range map[string]string{"case.md": got, "case.json": string(js)} {
		if strings.Contains(strings.ToLower(body), "redacted-by-construction") {
			t.Errorf("%s claims 'redacted-by-construction'; this surface carries FREE TEXT and makes no such claim", name)
		}
	}
}

// TestDistillSeverityCalibration_LeavesOperatorSeverityEmpty pins the
// labelling TODO: the tool cannot know the operator's severity, so it must
// not invent one.
func TestDistillSeverityCalibration_LeavesOperatorSeverityEmpty(t *testing.T) {
	res, err := PreviewSeverityCalibration(happyItems(t), calibrationOpts(t))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	for _, c := range res.Case.Concerns {
		if c.OperatorSeverity != "" {
			t.Errorf("concern %s: operator_severity = %q, want empty", c.ConcernID, c.OperatorSeverity)
		}
	}
	if !strings.Contains(res.CaseMD, "TODO(operator): label every concern") {
		t.Error("case.md must carry the labelling TODO")
	}
}

// TestPreviewSeverityCalibration_WritesNothing pins the --dry-run contract.
func TestPreviewSeverityCalibration_WritesNothing(t *testing.T) {
	opts := calibrationOpts(t)
	if _, err := PreviewSeverityCalibration(happyItems(t), opts); err != nil {
		t.Fatalf("preview: %v", err)
	}
	assertNoCaseDirWritten(t, opts.OutDir)
}

// TestDistillSeverityCalibration_MissingDiffPromptsOperator: no audit
// payload carries the reviewed diff, so a diff-less candidate must SAY so.
func TestDistillSeverityCalibration_MissingDiffPromptsOperator(t *testing.T) {
	opts := calibrationOpts(t)
	opts.Diff = ""
	res, err := PreviewSeverityCalibration(happyItems(t), opts)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if !strings.Contains(res.CaseMD, "TODO(operator): supply the diff") {
		t.Error("a diff-less candidate must carry the supply-the-diff TODO")
	}
}

// ---------------------------------------------------------------------------
// Fail-loud modes. One behavioral test each.
// ---------------------------------------------------------------------------

// TestDistillSeverityCalibration_UndecodablePayloadRefused — mode 1: an
// undecodable payload errors NAMING the item's sequence, and writes nothing.
func TestDistillSeverityCalibration_UndecodablePayloadRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		item CalibrationAuditItem
	}{
		{"implement_reviewed", CalibrationAuditItem{Sequence: 77, RunID: "run-1", Category: categoryImplementReviewed, Payload: json.RawMessage(`{"concerns":`)}},
		{"concern_waived", CalibrationAuditItem{Sequence: 88, RunID: "run-1", Category: categoryConcernWaived, Payload: json.RawMessage(`{"concern_id":`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := calibrationOpts(t)
			items := append(happyItems(t), tc.item)
			_, err := DistillSeverityCalibration(items, opts)
			if err == nil {
				t.Fatal("expected an error on an undecodable payload, got nil")
			}
			if !strings.Contains(err.Error(), "sequence") {
				t.Errorf("error should name the item sequence, got: %v", err)
			}
			assertNoCaseDirWritten(t, opts.OutDir)
		})
	}
}

// TestDistillSeverityCalibration_OrphanDispositionRefused — mode 2: a
// disposition naming a concern no implement_reviewed verdict in the input
// carries is an error NAMING the concern_id. The assertion reads the OUTPUT
// DIRECTORY after the call returns, because this control's effect is
// COMMITTED STATE — error identity alone would not distinguish a refusal
// from a write-then-error.
func TestDistillSeverityCalibration_OrphanDispositionRefused(t *testing.T) {
	opts := calibrationOpts(t)
	items := append(happyItems(t),
		dispositionItem(t, 30, "run-1", categoryConcernWaived, "c-orphan", "low", "style", "not worth it"))
	_, err := DistillSeverityCalibration(items, opts)
	if err == nil {
		t.Fatal("expected an error on a disposition with no matching review concern, got nil")
	}
	if !strings.Contains(err.Error(), "c-orphan") {
		t.Errorf("error should name the orphan concern_id, got: %v", err)
	}
	assertNoCaseDirWritten(t, opts.OutDir)
}

// TestDistillSeverityCalibration_AmbiguousJoinRefused — the ambiguity mode.
// The audit chain carries NO concern id on a review verdict, so when two
// unconsumed review concerns share (severity, category) nothing can say
// which reviewer note belongs to this concern_id. Attributing the wrong
// prose would silently corrupt a LABELLED corpus, so this fails loud rather
// than guessing.
func TestDistillSeverityCalibration_AmbiguousJoinRefused(t *testing.T) {
	opts := calibrationOpts(t)
	items := []CalibrationAuditItem{
		reviewItem(t, 10, "run-1", "claude-fable-5",
			[3]string{"high", "correctness", "first unbounded read"},
			[3]string{"high", "correctness", "second, different unbounded read"}),
		dispositionItem(t, 20, "run-1", categoryConcernWaived, "c-ambiguous", "high", "correctness", "waived"),
	}
	_, err := DistillSeverityCalibration(items, opts)
	if err == nil {
		t.Fatal("expected an error on an ambiguous (severity, category) join, got nil")
	}
	if !strings.Contains(err.Error(), "c-ambiguous") || !strings.Contains(err.Error(), "matches 2") {
		t.Errorf("error should name the concern_id and the match count, got: %v", err)
	}
	assertNoCaseDirWritten(t, opts.OutDir)
}

// TestDistillSeverityCalibration_ZeroJoinedConcernsRefused — mode 3: zero
// joined concerns is an ERROR, never an empty success. Both branches:
// no dispositions at all, and dispositions that were all unjoinable.
func TestDistillSeverityCalibration_ZeroJoinedConcernsRefused(t *testing.T) {
	t.Run("no dispositions", func(t *testing.T) {
		opts := calibrationOpts(t)
		items := []CalibrationAuditItem{
			reviewItem(t, 10, "run-1", "m", [3]string{"high", "correctness", "note"}),
		}
		_, err := DistillSeverityCalibration(items, opts)
		if err == nil {
			t.Fatal("expected an error when no disposition entries are present, got nil")
		}
		if !strings.Contains(err.Error(), "nothing to scaffold") {
			t.Errorf("unexpected error: %v", err)
		}
		assertNoCaseDirWritten(t, opts.OutDir)
	})
	t.Run("all unjoinable", func(t *testing.T) {
		opts := calibrationOpts(t)
		// concern_addressed_by_condition carries NEITHER severity NOR
		// category (server/condition_claims.go), so it has no join key.
		items := []CalibrationAuditItem{
			reviewItem(t, 10, "run-1", "m", [3]string{"high", "correctness", "note"}),
			dispositionItem(t, 20, "run-1", categoryConcernAddressedByCondition, "c-cond", "", "", ""),
		}
		_, err := DistillSeverityCalibration(items, opts)
		if err == nil {
			t.Fatal("expected an error when every disposition was unjoinable, got nil")
		}
		if !strings.Contains(err.Error(), "c-cond") || !strings.Contains(err.Error(), "unjoinable") {
			t.Errorf("error should name the unjoinable concern ids, got: %v", err)
		}
		assertNoCaseDirWritten(t, opts.OutDir)
	})
}

// TestDistillSeverityCalibration_UnjoinableDispositionReported: an
// addressed_by_condition entry ALONGSIDE joinable ones is neither dropped
// nor guessed — it is listed in case.md by concern_id.
func TestDistillSeverityCalibration_UnjoinableDispositionReported(t *testing.T) {
	items := append(happyItems(t),
		dispositionItem(t, 30, "run-1", categoryConcernAddressedByCondition, "c-cond", "", "", ""))
	res, err := PreviewSeverityCalibration(items, calibrationOpts(t))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(res.UnjoinableConcernIDs) != 1 || res.UnjoinableConcernIDs[0] != "c-cond" {
		t.Fatalf("unjoinable = %v, want [c-cond]", res.UnjoinableConcernIDs)
	}
	if !strings.Contains(res.CaseMD, "c-cond") || !strings.Contains(res.CaseMD, "unjoinable disposition") {
		t.Error("case.md must list the unjoinable disposition by concern_id")
	}
	for _, c := range res.Case.Concerns {
		if c.ConcernID == "c-cond" {
			t.Error("an unjoinable disposition must NOT be guessed into a labelled concern")
		}
	}
}

// TestDistillSeverityCalibration_BlankConcernIDRefused: a disposition with
// no concern_id cannot be attributed at all.
func TestDistillSeverityCalibration_BlankConcernIDRefused(t *testing.T) {
	opts := calibrationOpts(t)
	items := append(happyItems(t),
		dispositionItem(t, 30, "run-1", categoryConcernWaived, "  ", "low", "style", "r"))
	_, err := DistillSeverityCalibration(items, opts)
	if err == nil {
		t.Fatal("expected an error on a disposition carrying no concern_id, got nil")
	}
	if !strings.Contains(err.Error(), "no concern_id") {
		t.Errorf("unexpected error: %v", err)
	}
	assertNoCaseDirWritten(t, opts.OutDir)
}

// TestDistillSeverityCalibration_RequiredOptions covers the three
// option-validation refusals.
func TestDistillSeverityCalibration_RequiredOptions(t *testing.T) {
	base := calibrationOpts(t)
	for name, mutate := range map[string]func(*CalibrationOptions){
		"CaseName":        func(o *CalibrationOptions) { o.CaseName = "" },
		"Issue":           func(o *CalibrationOptions) { o.Issue = "" },
		"OutDir":          func(o *CalibrationOptions) { o.OutDir = "" },
		"unsafe CaseName": func(o *CalibrationOptions) { o.CaseName = "../escape" },
	} {
		t.Run(name, func(t *testing.T) {
			opts := base
			mutate(&opts)
			if _, err := DistillSeverityCalibration(happyItems(t), opts); err == nil {
				t.Fatalf("expected an error for %s, got nil", name)
			}
		})
	}
}

// TestDistillSeverityCalibration_ExistingDirNeedsForce pins the overwrite
// guard in both directions.
func TestDistillSeverityCalibration_ExistingDirNeedsForce(t *testing.T) {
	opts := calibrationOpts(t)
	if _, err := DistillSeverityCalibration(happyItems(t), opts); err != nil {
		t.Fatalf("first distill: %v", err)
	}
	if _, err := DistillSeverityCalibration(happyItems(t), opts); err == nil {
		t.Fatal("expected a refusal on an existing case dir without --force")
	}
	opts.Force = true
	if _, err := DistillSeverityCalibration(happyItems(t), opts); err != nil {
		t.Fatalf("--force distill: %v", err)
	}
}

// TestDistillSeverityCalibration_JoinIsSequenceOrdered: the join consumes
// the note catalogue in ascending sequence order regardless of the caller's
// slice order, so a --in/stdin file listing dispositions first still joins.
func TestDistillSeverityCalibration_JoinIsSequenceOrdered(t *testing.T) {
	in := happyItems(t)
	shuffled := []CalibrationAuditItem{in[2], in[1], in[0]}
	res, err := PreviewSeverityCalibration(shuffled, calibrationOpts(t))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(res.Case.Concerns) != 2 {
		t.Fatalf("want 2 concerns, got %d", len(res.Case.Concerns))
	}
	if res.Case.Concerns[0].ConcernID != "c-1" || res.Case.Concerns[1].ConcernID != "c-2" {
		t.Errorf("join is not sequence-ordered: %+v", res.Case.Concerns)
	}
}

// TestDistillSeverityCalibration_ProvenanceDoesNotClaimRedaction: NEITHER
// source path may claim redaction, because both carry free-text prose.
func TestDistillSeverityCalibration_ProvenanceDoesNotClaimRedaction(t *testing.T) {
	for _, fetched := range []bool{true, false} {
		opts := calibrationOpts(t)
		opts.Fetched = fetched
		res, err := PreviewSeverityCalibration(happyItems(t), opts)
		if err != nil {
			t.Fatalf("preview (fetched=%v): %v", fetched, err)
		}
		if strings.Contains(strings.ToLower(res.CaseMD), "redact") &&
			!strings.Contains(res.CaseMD, "redact by hand") {
			t.Errorf("fetched=%v: case.md makes a redaction claim: %s", fetched, res.CaseMD)
		}
		if !strings.Contains(res.CaseMD, "free text") && !strings.Contains(res.CaseMD, "FREE TEXT") {
			t.Errorf("fetched=%v: case.md must state the free-text posture", fetched)
		}
	}
}
