package diagnostics

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func ptrCat(c run.FailureCategory) *run.FailureCategory { return &c }
func ptrStr(s string) *string                           { return &s }

func sampleVersions() VersionFacts {
	return VersionFacts{
		Fishhawkd:        Component{Version: "v0.4.1", GitSHA: "abc1234"},
		MinRunnerVersion: "v0.3.0",
	}
}

func TestCollect_ProductFactsOnly(t *testing.T) {
	runID := uuid.New()
	failStageID := uuid.New()
	planStageID := uuid.New()

	r := &run.Run{
		ID:          runID,
		WorkflowID:  "feature_change",
		WorkflowSHA: "deadbeefspec",
		RunnerKind:  run.RunnerKindLocal,
		State:       run.StateFailed,
	}
	stages := []*run.Stage{
		{ID: planStageID, Sequence: 0, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{
			ID:              failStageID,
			Sequence:        1,
			Type:            run.StageTypeImplement,
			State:           run.StageStateFailed,
			FailureCategory: ptrCat(run.FailureB),
			// Free text that MUST NOT leak into the bundle. It carries a
			// classifiable git-stderr shape (auth-401) AND the leak-canary
			// strings, so the class flows through while the reason text is
			// still proven absent from the serialized bundle below.
			FailureReason: ptrStr("fatal: unable to access '...': The requested URL returned error: 401 " +
				"(agent edited forbidden path /etc/secret and printed PROMPT_LEAK)"),
		},
	}
	entries := []*audit.Entry{
		{Sequence: 10, Category: "stage_dispatched"},
		{Sequence: 11, StageID: &failStageID, Category: "policy_evaluated"},
		{Sequence: 12, StageID: &failStageID, Category: "stage_failed"},
	}

	b := Collect(r, stages, entries, sampleVersions())

	if b.RunID != runID.String() {
		t.Errorf("RunID = %q, want %q", b.RunID, runID.String())
	}
	if b.WorkflowID != "feature_change" {
		t.Errorf("WorkflowID = %q", b.WorkflowID)
	}
	if b.WorkflowSpecHash != "deadbeefspec" {
		t.Errorf("WorkflowSpecHash = %q", b.WorkflowSpecHash)
	}
	if b.RunnerKind != run.RunnerKindLocal {
		t.Errorf("RunnerKind = %q", b.RunnerKind)
	}
	if b.RunState != string(run.StateFailed) {
		t.Errorf("RunState = %q", b.RunState)
	}
	if len(b.Stages) != 2 {
		t.Fatalf("Stages len = %d, want 2", len(b.Stages))
	}
	if b.Stages[0].Type != "plan" || b.Stages[1].Type != "implement" {
		t.Errorf("stage ordering wrong: %+v", b.Stages)
	}
	if b.FailingStage == nil {
		t.Fatal("FailingStage = nil, want the implement stage")
	}
	if b.FailingStage.Sequence != 1 || b.FailingStage.Type != "implement" {
		t.Errorf("FailingStage = %+v", b.FailingStage)
	}
	if b.FailingStage.FailureCategory != "B" {
		t.Errorf("FailureCategory = %q, want B", b.FailingStage.FailureCategory)
	}
	// Most-recent audit category scoped to the failing stage.
	if b.FailingStage.FailureSurface != "stage_failed" {
		t.Errorf("FailureSurface = %q, want stage_failed", b.FailingStage.FailureSurface)
	}
	// The detail class is derived from the free-text reason.
	if b.FailingStage.FailureDetailClass != "auth-401" {
		t.Errorf("FailureDetailClass = %q, want auth-401", b.FailingStage.FailureDetailClass)
	}
	if b.AuditSequenceRange == nil || b.AuditSequenceRange.Min != 10 || b.AuditSequenceRange.Max != 12 {
		t.Errorf("AuditSequenceRange = %+v, want {10,12}", b.AuditSequenceRange)
	}
	if b.Versions.Fishhawkd.Version != "v0.4.1" || b.Versions.Fishhawkd.GitSHA != "abc1234" {
		t.Errorf("Versions.Fishhawkd = %+v", b.Versions.Fishhawkd)
	}
	if b.Versions.MinRunnerVersion != "v0.3.0" {
		t.Errorf("MinRunnerVersion = %q", b.Versions.MinRunnerVersion)
	}

	// The hard requirement: no free text crosses the boundary. Serialize
	// the whole bundle and assert the FailureReason text is absent.
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leak := range []string{"forbidden path", "/etc/secret", "PROMPT_LEAK", "FailureReason", "failure_reason"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("bundle leaked %q: %s", leak, raw)
		}
	}
}

// TestCollect_NilFailureReason_EmptyDetailClass pins the nil-guard branch
// in Collect: a failing stage with no recorded FailureReason leaves
// FailureDetailClass empty (omitempty), degrading to the pre-#1962
// fingerprint per the backward-compat contract, without ever calling
// ClassifyFailureDetail on a nil pointer.
func TestCollect_NilFailureReason_EmptyDetailClass(t *testing.T) {
	failStageID := uuid.New()
	r := &run.Run{ID: uuid.New(), WorkflowID: "feature_change", State: run.StateFailed}
	stages := []*run.Stage{
		{
			ID:              failStageID,
			Sequence:        0,
			Type:            run.StageTypeImplement,
			State:           run.StageStateFailed,
			FailureCategory: ptrCat(run.FailureB),
			FailureReason:   nil, // no reason recorded on the failed stage
		},
	}
	b := Collect(r, stages, nil, sampleVersions())
	if b.FailingStage == nil {
		t.Fatal("FailingStage = nil, want the implement stage")
	}
	if b.FailingStage.FailureDetailClass != "" {
		t.Errorf("FailureDetailClass = %q, want empty for a nil FailureReason", b.FailingStage.FailureDetailClass)
	}
}

func TestCollect_NoFailure(t *testing.T) {
	r := &run.Run{ID: uuid.New(), WorkflowID: "feature_change", State: run.StateSucceeded}
	stages := []*run.Stage{
		{ID: uuid.New(), Sequence: 0, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{ID: uuid.New(), Sequence: 1, Type: run.StageTypeImplement, State: run.StageStateSucceeded},
	}
	b := Collect(r, stages, nil, sampleVersions())
	if b.FailingStage != nil {
		t.Errorf("FailingStage = %+v, want nil for a succeeded run", b.FailingStage)
	}
	if b.AuditSequenceRange != nil {
		t.Errorf("AuditSequenceRange = %+v, want nil for no entries", b.AuditSequenceRange)
	}
}

func TestCollect_OrdersStagesBySequence(t *testing.T) {
	r := &run.Run{ID: uuid.New(), State: run.StateRunning}
	// Deliberately out of order.
	stages := []*run.Stage{
		{Sequence: 2, Type: run.StageTypeReview, State: run.StageStatePending},
		{Sequence: 0, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{Sequence: 1, Type: run.StageTypeImplement, State: run.StageStateRunning},
	}
	b := Collect(r, stages, nil, sampleVersions())
	want := []int{0, 1, 2}
	for i, s := range b.Stages {
		if s.Sequence != want[i] {
			t.Errorf("Stages[%d].Sequence = %d, want %d", i, s.Sequence, want[i])
		}
	}
}

func TestCollect_NilRunIsSafe(t *testing.T) {
	b := Collect(nil, nil, nil, sampleVersions())
	if b.RunID != "" || b.FailingStage != nil {
		t.Errorf("nil run should yield an empty-ish bundle, got %+v", b)
	}
	// Versions still carried.
	if b.Versions.Fishhawkd.Version != "v0.4.1" {
		t.Errorf("versions dropped on nil run")
	}
}

func TestSequenceRange_Unordered(t *testing.T) {
	entries := []*audit.Entry{
		{Sequence: 30},
		{Sequence: 5},
		{Sequence: 17},
	}
	rng := sequenceRange(entries)
	if rng == nil || rng.Min != 5 || rng.Max != 30 {
		t.Errorf("sequenceRange = %+v, want {5,30}", rng)
	}
}

// --- wedge context (#1737) ---

// TestCollect_NoWedgeArgument_BundleUnchanged is failure mode m9 AND the
// counterfactual vehicle for the nil gate in CollectWithWedge: the
// no-wedge Collect wrapper must produce the bundle it produced before
// #1737 even on a run whose audit chain carries the fan-in conflict
// signal, so every un-migrated caller is unaffected.
//
// Counterfactual: delete the `if wedge != nil` gate (assemble the block
// unconditionally) and this goes RED — Collect emits a wedge_context
// carrying the slice_integration_conflict marker.
func TestCollect_NoWedgeArgument_BundleUnchanged(t *testing.T) {
	r := &run.Run{ID: uuid.New(), WorkflowID: "feature_change", State: run.StateFailed}
	entries := []*audit.Entry{
		{Sequence: 1, Category: "stage_dispatched"},
		{Sequence: 2, Category: "slice_integration_conflict"},
	}

	b := Collect(r, nil, entries, sampleVersions())
	if b.WedgeContext != nil {
		t.Fatalf("Collect emitted wedge_context = %+v, want nil", b.WedgeContext)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "wedge_context") {
		t.Errorf("Collect bundle carries a wedge_context key: %s", raw)
	}
}

// TestCollectWithWedge_NoWedgeShape_OmitsBlock is failure mode m1: an
// opted-in run with nothing wedged carries no wedge_context key at all,
// rather than an empty object.
func TestCollectWithWedge_NoWedgeShape_OmitsBlock(t *testing.T) {
	r := &run.Run{ID: uuid.New(), WorkflowID: "feature_change", State: run.StateSucceeded}
	entries := []*audit.Entry{{Sequence: 7, Category: "stage_dispatched"}}

	b := CollectWithWedge(r, nil, entries, sampleVersions(), &WedgeFacts{})
	if b.WedgeContext != nil {
		t.Fatalf("wedge_context = %+v, want nil for a run with no wedge shape", b.WedgeContext)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "wedge_context") {
		t.Errorf("bundle carries a wedge_context key: %s", raw)
	}
	// And it is otherwise identical to the no-wedge wrapper's output.
	plain, err := json.Marshal(Collect(r, nil, entries, sampleVersions()))
	if err != nil {
		t.Fatalf("marshal plain: %v", err)
	}
	if string(raw) != string(plain) {
		t.Errorf("opted-in bundle differs from Collect:\n got %s\nwant %s", raw, plain)
	}
}

// TestCollectWithWedge_PopulatedBlock pins the assembled block: injected
// blocking checks and campaign facts plus the audit-derived fan-in
// marker.
func TestCollectWithWedge_PopulatedBlock(t *testing.T) {
	r := &run.Run{ID: uuid.New(), WorkflowID: "feature_change", State: run.StateFailed}
	entries := []*audit.Entry{
		{Sequence: 3, Category: "stage_dispatched"},
		{Sequence: 4, Category: "slice_integration_conflict"},
	}
	wedge := &WedgeFacts{
		// One empty name mixed in: dropped, not carried as "".
		BlockingChecks:    []string{"CI Pass", "", "CodeQL"},
		CampaignItemState: "failed",
		BlockedDependents: 3,
	}

	b := CollectWithWedge(r, nil, entries, sampleVersions(), wedge)
	if b.WedgeContext == nil {
		t.Fatal("wedge_context = nil, want populated")
	}
	wc := b.WedgeContext
	if got, want := strings.Join(wc.BlockingChecks, ","), "CI Pass,CodeQL"; got != want {
		t.Errorf("blocking_checks = %q, want %q", got, want)
	}
	if wc.CampaignItemState != "failed" {
		t.Errorf("campaign_item_state = %q, want failed", wc.CampaignItemState)
	}
	if wc.BlockedDependents != 3 {
		t.Errorf("blocked_dependents = %d, want 3", wc.BlockedDependents)
	}
	if wc.IntegrateWaveError != "slice_integration_conflict" {
		t.Errorf("integrate_wave_error = %q, want slice_integration_conflict", wc.IntegrateWaveError)
	}
}

// TestCollectWithWedge_NoConflictEntry_NoFanInMarker pins the negative
// half of the fan-in derivation: no slice_integration_conflict audit
// category means no marker, so the block never over-claims a fan-in
// failure.
func TestCollectWithWedge_NoConflictEntry_NoFanInMarker(t *testing.T) {
	r := &run.Run{ID: uuid.New(), State: run.StateFailed}
	entries := []*audit.Entry{{Sequence: 1, Category: "children_settled"}}

	b := CollectWithWedge(r, nil, entries, sampleVersions(), &WedgeFacts{BlockedDependents: 1})
	if b.WedgeContext == nil {
		t.Fatal("wedge_context = nil, want populated by blocked_dependents")
	}
	if b.WedgeContext.IntegrateWaveError != "" {
		t.Errorf("integrate_wave_error = %q, want empty", b.WedgeContext.IntegrateWaveError)
	}
}

// TestWedgeContext_NeverCarriesFreeText is the redaction counterfactual.
// The fixture seeds the sentinel BY CONSTRUCTION in three places free
// text could plausibly be picked up from — the drive advance audit
// payload, the failing stage's FailureReason, and the caller-injected
// campaign item state — and asserts none of it reaches the serialized
// wedge block.
//
// Counterfactual: delete normalizeCampaignItemState's table lookup
// (assign wedge.CampaignItemState straight through) and this goes RED on
// the campaign_item_state assertion.
func TestWedgeContext_NeverCarriesFreeText(t *testing.T) {
	const sentinel = "SENTINEL_WEDGE_FREE_TEXT"

	failStageID := uuid.New()
	r := &run.Run{ID: uuid.New(), WorkflowID: "feature_change", State: run.StateFailed}
	stages := []*run.Stage{{
		ID:              failStageID,
		Sequence:        0,
		Type:            run.StageTypeImplement,
		State:           run.StageStateFailed,
		FailureCategory: ptrCat(run.FailureB),
		FailureReason:   ptrStr("slice branch will not merge: " + sentinel),
	}}
	entries := []*audit.Entry{
		// A drive advance whose Event string embeds the sentinel...
		{Sequence: 1, Category: "run_auto_advanced", Payload: []byte(`{"event":"` + sentinel + `"}`)},
		// ...and the structured fan-in category, whose payload also does.
		{Sequence: 2, StageID: &failStageID, Category: "slice_integration_conflict",
			Payload: []byte(`{"detail":"` + sentinel + `"}`)},
	}
	wedge := &WedgeFacts{
		BlockingChecks: []string{"CI Pass"},
		// An unrecognized, free-text-bearing state: must be dropped.
		CampaignItemState: "failed because " + sentinel,
		BlockedDependents: 2,
	}

	b := CollectWithWedge(r, stages, entries, sampleVersions(), wedge)
	if b.WedgeContext == nil {
		t.Fatal("wedge_context = nil, want populated")
	}
	if b.WedgeContext.CampaignItemState != "" {
		t.Errorf("campaign_item_state = %q, want empty (unrecognized state dropped)",
			b.WedgeContext.CampaignItemState)
	}
	// The structured marker still flows — it is the package's own literal.
	if b.WedgeContext.IntegrateWaveError != "slice_integration_conflict" {
		t.Errorf("integrate_wave_error = %q", b.WedgeContext.IntegrateWaveError)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), sentinel) {
		t.Errorf("wedge bundle leaked free text: %s", raw)
	}
}

// TestNormalizeCampaignItemState_ClosedTable pins that every real
// campaign item state round-trips and everything else is dropped.
func TestNormalizeCampaignItemState_ClosedTable(t *testing.T) {
	for _, s := range []string{"pending", "blocked", "running", "paused", "succeeded", "failed", "cancelled"} {
		if got := normalizeCampaignItemState(s); got != s {
			t.Errorf("normalizeCampaignItemState(%q) = %q, want %q", s, got, s)
		}
	}
	for _, s := range []string{"", "FAILED", "failed ", "wedged", "failed: reason"} {
		if got := normalizeCampaignItemState(s); got != "" {
			t.Errorf("normalizeCampaignItemState(%q) = %q, want empty", s, got)
		}
	}
}
