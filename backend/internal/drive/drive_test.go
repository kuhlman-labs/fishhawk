package drive

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// auditStub implements the two audit.Repository methods the engine
// uses; the embedded nil interface panics on anything else, pinning
// the engine's read/write surface.
type auditStub struct {
	audit.Repository
	appended []audit.ChainAppendParams
	entries  []*audit.Entry
	listErr  error
	writeErr error
}

func (a *auditStub) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if a.writeErr != nil {
		return nil, a.writeErr
	}
	a.appended = append(a.appended, p)
	rid := p.RunID
	entry := &audit.Entry{ID: uuid.New(), RunID: &rid, StageID: p.StageID,
		Category: p.Category, Payload: p.Payload}
	a.entries = append(a.entries, entry)
	return entry, nil
}

func (a *auditStub) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if a.listErr != nil {
		return nil, a.listErr
	}
	var out []*audit.Entry
	for _, e := range a.entries {
		if e.RunID != nil && *e.RunID == runID && e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}

func TestMechanical_RuleTable(t *testing.T) {
	for _, rule := range []Rule{
		RulePlanApprovedDispatch,
		RuleReviewsSettledGate,
		RuleFixupRereviewRepark,
		RuleChecksGreenAwaitingMerge,
	} {
		if !Mechanical(rule) {
			t.Errorf("Mechanical(%q) = false, want true", rule)
		}
	}
	for _, rule := range []Rule{
		RuleGateApproval,
		RuleConcernRouting,
		RuleMerge,
		Rule("unknown_future_rule"),
	} {
		if Mechanical(rule) {
			t.Errorf("Mechanical(%q) = true, want false (judgment points always park)", rule)
		}
	}
}

func TestEvaluatePlanApproved_GitHubActions_Advances(t *testing.T) {
	out := EvaluatePlanApproved(run.RunnerKindGitHubActions)
	if !out.Advance {
		t.Fatal("Advance = false, want true for runner_kind github_actions")
	}
	if out.NextAction != nil {
		t.Errorf("NextAction = %+v, want nil (nothing for the operator to do)", out.NextAction)
	}
}

func TestEvaluatePlanApproved_Local_ParksWithNextAction(t *testing.T) {
	out := EvaluatePlanApproved(run.RunnerKindLocal)
	if out.Advance {
		t.Fatal("Advance = true, want false: the backend cannot spawn the host-side runner (ADR-024)")
	}
	if out.NextAction == nil || out.NextAction.Action != "run_implement_stage" {
		t.Fatalf("NextAction = %+v, want action run_implement_stage", out.NextAction)
	}
}

func TestEngineRecord_AppendsEntry(t *testing.T) {
	au := &auditStub{}
	e := &Engine{Audit: au}
	runID := uuid.New()
	stageID := uuid.New()

	e.Record(context.Background(), runID, &stageID, Advance{
		Rule:       RuleChecksGreenAwaitingMerge,
		From:       "review:awaiting_approval",
		To:         "awaiting_merge",
		Event:      "required checks green",
		NextAction: &NextAction{Action: "merge_pr", PRURL: "https://github.com/x/y/pull/7"},
	})

	if len(au.appended) != 1 {
		t.Fatalf("appended = %d entries, want 1", len(au.appended))
	}
	got := au.appended[0]
	if got.Category != Category {
		t.Errorf("Category = %q, want %q", got.Category, Category)
	}
	if got.RunID != runID || got.StageID == nil || *got.StageID != stageID {
		t.Errorf("entry keyed to (%v, %v), want (%v, %v)", got.RunID, got.StageID, runID, stageID)
	}
	if got.ActorKind == nil || *got.ActorKind != audit.ActorSystem {
		t.Errorf("ActorKind = %v, want system", got.ActorKind)
	}
	var p Advance
	if err := json.Unmarshal(got.Payload, &p); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if p.Rule != RuleChecksGreenAwaitingMerge || p.From != "review:awaiting_approval" || p.To != "awaiting_merge" {
		t.Errorf("payload = %+v", p)
	}
	if p.NextAction == nil || p.NextAction.Action != "merge_pr" || p.NextAction.PRURL != "https://github.com/x/y/pull/7" {
		t.Errorf("payload next_action = %+v", p.NextAction)
	}
}

func TestEngineRecord_AppendErrorIsBestEffort(t *testing.T) {
	au := &auditStub{writeErr: errors.New("db down")}
	e := &Engine{Audit: au}
	// Must not panic; the transition the entry documents already happened.
	e.Record(context.Background(), uuid.New(), nil, Advance{Rule: RulePlanApprovedDispatch})
}

func TestEngineRecorded_DedupsByRuleAndStage(t *testing.T) {
	au := &auditStub{}
	e := &Engine{Audit: au}
	ctx := context.Background()
	runID := uuid.New()
	stageID := uuid.New()
	otherStage := uuid.New()

	if e.Recorded(ctx, runID, &stageID, RuleReviewsSettledGate) {
		t.Fatal("Recorded = true on an empty trail")
	}
	e.Record(ctx, runID, &stageID, Advance{Rule: RuleReviewsSettledGate})

	if !e.Recorded(ctx, runID, &stageID, RuleReviewsSettledGate) {
		t.Error("Recorded = false after Record for the same (stage, rule)")
	}
	if e.Recorded(ctx, runID, &stageID, RuleChecksGreenAwaitingMerge) {
		t.Error("Recorded = true for a different rule")
	}
	if e.Recorded(ctx, runID, &otherStage, RuleReviewsSettledGate) {
		t.Error("Recorded = true for a different stage")
	}
}

func TestEngineRecorded_ListErrorFailsOpen(t *testing.T) {
	au := &auditStub{listErr: errors.New("db down")}
	e := &Engine{Audit: au}
	if e.Recorded(context.Background(), uuid.New(), nil, RuleReviewsSettledGate) {
		t.Fatal("Recorded = true on a read error; want false (a duplicate beats a suppressed trail)")
	}
}

func TestEngine_NilSafe(t *testing.T) {
	var e *Engine
	e.Record(context.Background(), uuid.New(), nil, Advance{Rule: RuleMerge})
	if e.Recorded(context.Background(), uuid.New(), nil, RuleMerge) {
		t.Fatal("nil engine Recorded = true")
	}
}
