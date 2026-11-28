package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// #3641: a FIX-UP dispatch that meets a resumable checkpoint declines it — a
// fix-up must re-invoke the agent (#2630) — and that decline throws the
// recorded verified tree away. Every assertion below reads the appended audit
// rows back after the call returns: the resolver's return values are identical
// whether or not the verified_tree_discarded row is written, so the committed
// audit state is the only observable effect of the control.

// discardRows returns the verified_tree_discarded rows appended for stageID.
func discardRows(appended []audit.ChainAppendParams, stageID uuid.UUID) []map[string]any {
	var out []map[string]any
	for _, p := range appended {
		if p.Category != "verified_tree_discarded" || p.StageID == nil || *p.StageID != stageID {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal(p.Payload, &m)
		out = append(out, m)
	}
	return out
}

// assertOneFixupDiscard asserts exactly one verified_tree_discarded row for the
// stage, with reason fixup_dispatch and the given coordinates.
func assertOneFixupDiscard(t *testing.T, rows []map[string]any, tree, head, branch string) {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("verified_tree_discarded rows = %d, want exactly 1 (reason %s): %v", len(rows), declineReasonFixupDispatch, rows)
	}
	for key, want := range map[string]string{
		"reason":            declineReasonFixupDispatch,
		"verified_tree_sha": tree,
		"head_sha":          head,
		"branch":            branch,
	} {
		if got, _ := rows[0][key].(string); got != want {
			t.Errorf("discard row %s = %q, want %q", key, got, want)
		}
	}
}

func TestDeclineReasonFixupDispatchWireValue(t *testing.T) {
	if declineReasonFixupDispatch != "fixup_dispatch" {
		t.Errorf("declineReasonFixupDispatch = %q, want %q", declineReasonFixupDispatch, "fixup_dispatch")
	}
}

// TestResolvePushCheckpointResume_FixupDeclineAuditsDiscard is the direct
// resolver table. Every resumable fix-up row is paired with a NON-fix-up
// control on the IDENTICAL entries, which is SERVED and writes no row — so the
// fix-up arm, not an unresumable fixture, is what declines and writes.
func TestResolvePushCheckpointResume_FixupDeclineAuditsDiscard(t *testing.T) {
	fixupTriggered := func(seq int64) *audit.Entry {
		return scopeDecisionEntry(uuid.Nil, CategoryStageFixupTriggered, seq)
	}
	fixupRecovered := func(seq int64) *audit.Entry {
		return scopeDecisionEntryPayload(uuid.Nil, CategoryStageFixupRecovered, seq, `{"source_failure_category":"C"}`)
	}
	for _, tc := range []struct {
		name       string
		entries    func() []*audit.Entry
		recordDrop bool
		// wantRow: the fix-up writes exactly one fixup_dispatch row.
		wantRow bool
		// controlKind: the non-fix-up control is served this kind ("" = the
		// control is not resumable either, so no control is asserted).
		controlKind string
	}{
		{"push", func() []*audit.Entry { return []*audit.Entry{prePushFailedEntry(7), pushCheckpointEntry(8)} }, true, true, resumeKindPush},
		{"pr_open", func() []*audit.Entry { return []*audit.Entry{checkpointFailedEntry(7)} }, true, true, resumeKindPROpen},
		{"push_with_newer_fixup_trigger_and_recovery", func() []*audit.Entry {
			return []*audit.Entry{prePushFailedEntry(7), pushCheckpointEntry(8), fixupTriggered(9), fixupRecovered(10)}
		}, true, true, resumeKindPush},
		{"reverify", func() []*audit.Entry { return []*audit.Entry{prePushFailedEntry(7), reverifyCheckpointEntry(8, false)} }, true, false, resumeKindReverify},
		{"superseded", func() []*audit.Entry {
			return []*audit.Entry{prePushFailedEntry(7), pushCheckpointEntry(8), scopeDecisionEntry(uuid.Nil, "fixup_pushed", 9)}
		}, true, false, ""},
		{"preview", func() []*audit.Entry { return []*audit.Entry{prePushFailedEntry(7), pushCheckpointEntry(8)} }, false, false, resumeKindPush},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.controlKind != "" {
				s, runRow, stage := checkpointGateFixture(t, tc.entries())
				held, ok := s.resolvePushCheckpointResume(context.Background(), runRow, stage, false, true, true, true, tc.recordDrop)
				if !ok || held.resumeKind != tc.controlKind {
					t.Fatalf("control: served = %t kind %q, want served %q on the identical state", ok, held.resumeKind, tc.controlKind)
				}
				au := s.cfg.AuditRepo.(*exemptAuditFake)
				if rows := discardRows(au.appended, stage.ID); len(rows) != 0 {
					t.Errorf("control: a served resume must write no discard row, got %v", rows)
				}
			}

			s, runRow, stage := checkpointGateFixture(t, tc.entries())
			if tc.name == "superseded" {
				if _, verdict := s.newestPushCheckpoint(context.Background(), runRow.ID, stage.ID); verdict != checkpointSuperseded {
					t.Fatalf("fixture verdict = %q, want %q", verdict, checkpointSuperseded)
				}
			}
			if tc.name == "push_with_newer_fixup_trigger_and_recovery" {
				if _, verdict := s.newestPushCheckpoint(context.Background(), runRow.ID, stage.ID); verdict != checkpointResumable {
					t.Fatalf("verdict = %q, want %q: neither stage_fixup_triggered nor stage_fixup_recovered may supersede a checkpoint", verdict, checkpointResumable)
				}
			}
			held, ok := s.resolvePushCheckpointResume(context.Background(), runRow, stage, true, true, true, true, tc.recordDrop)
			if ok {
				t.Fatalf("fix-up: served %+v, want declined (a fix-up must re-invoke the agent)", held)
			}
			au := s.cfg.AuditRepo.(*exemptAuditFake)
			rows := discardRows(au.appended, stage.ID)
			if tc.wantRow {
				assertOneFixupDiscard(t, rows, checkpointVerifiedTree, checkpointHeadSHA, checkpointBranch)
			} else if len(rows) != 0 {
				t.Errorf("fix-up: want NO discard row, got %v", rows)
			}
		})
	}
}

// fixupConcernTriggerEntry is a stage_fixup_triggered entry carrying one routed
// concern, which is what makes the /prompt handler take its fixup=true branch.
func fixupConcernTriggerEntry(seq int64) *audit.Entry {
	return scopeDecisionEntryPayload(uuid.Nil, CategoryStageFixupTriggered, seq,
		`{"prior_state":"succeeded","concerns":[{"severity":"medium","category":"scope","note":"drift"}],"concern_ids":["`+uuid.NewString()+`"],"pass_ordinal":1}`)
}

// TestPrompt_FixupDispatchAuditsPreservedCheckpointDiscard drives the REAL
// signed /prompt handler: a fix-up fetch over a newer push checkpoint is served
// fixup:true with no held-commit fields and writes exactly one fixup_dispatch
// row; /prompt-render on the same state writes none; the same state WITHOUT the
// trigger is served the push kind and writes none.
func TestPrompt_FixupDispatchAuditsPreservedCheckpointDiscard(t *testing.T) {
	build := func(withTrigger bool) (*Server, uuid.UUID, *exemptAuditFake, ed25519.PrivateKey) {
		var au *exemptAuditFake
		s, _, stageID, priv := exemptPromptFixture(t, nil, func(sid uuid.UUID) audit.Repository {
			au = &exemptAuditFake{}
			entries := []*audit.Entry{prePushFailedEntry(7), pushCheckpointEntry(8)}
			if withTrigger {
				entries = append([]*audit.Entry{fixupConcernTriggerEntry(5)}, entries...)
			}
			for _, e := range entries {
				id := sid
				e.StageID = &id
				au.entries = append(au.entries, e)
			}
			return au
		})
		return s, stageID, au, priv
	}

	t.Run("fixup_declined_and_audited", func(t *testing.T) {
		s, stageID, au, priv := build(true)
		keys := exemptBodyKeys(t, promptRequestWithCapabilities(t, s, stageID, priv, capableRunnerCapabilities), "/prompt")
		if string(keys["fixup"]) != "true" {
			t.Fatalf("fixup = %s, want true (the seeded trigger must route the fix-up branch)", keys["fixup"])
		}
		assertNoResumeEmission(t, keys, "a fix-up dispatch")
		if raw, ok := keys["held_commit_verified_tree_sha"]; ok {
			t.Errorf("held_commit_verified_tree_sha must be absent on a fix-up, got %s", raw)
		}
		assertOneFixupDiscard(t, discardRows(au.appended, stageID), checkpointVerifiedTree, checkpointHeadSHA, checkpointBranch)

		_ = exemptBodyKeys(t, promptRenderRequest(t, s, stageID), "/prompt-render")
		if rows := discardRows(au.appended, stageID); len(rows) != 1 {
			t.Errorf("after /prompt-render: discard rows = %d, want still 1 (preview writes nothing)", len(rows))
		}
	})

	t.Run("render_only_writes_nothing", func(t *testing.T) {
		s, stageID, au, _ := build(true)
		keys := exemptBodyKeys(t, promptRenderRequest(t, s, stageID), "/prompt-render")
		assertNoResumeEmission(t, keys, "a fix-up render")
		if rows := discardRows(au.appended, stageID); len(rows) != 0 {
			t.Errorf("/prompt-render: discard rows = %d, want 0", len(rows))
		}
	})

	t.Run("no_trigger_control_served", func(t *testing.T) {
		s, stageID, au, priv := build(false)
		keys := exemptBodyKeys(t, promptRequestWithCapabilities(t, s, stageID, priv, capableRunnerCapabilities), "/prompt")
		if _, ok := keys["fixup"]; ok {
			t.Fatalf("control: fixup = %s, want absent", keys["fixup"])
		}
		if string(keys["held_commit_resume_kind"]) != `"push"` {
			t.Fatalf("control: held_commit_resume_kind = %s, want \"push\"", keys["held_commit_resume_kind"])
		}
		if rows := discardRows(au.appended, stageID); len(rows) != 0 {
			t.Errorf("control: a served resume must write no discard row, got %v", rows)
		}
	})
}

// fixupDiscardLifecycleRepo is fixupRecoveryRepo plus the orchestratorRepo's
// dispatch-attempt stamp on the move to dispatched, which fixupRecoveryRepo's
// TransitionStage override drops. File-local so no shared fixture changes.
type fixupDiscardLifecycleRepo struct {
	*fixupRecoveryRepo
}

func (r *fixupDiscardLifecycleRepo) TransitionStage(ctx context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	st, err := r.fixupRecoveryRepo.TransitionStage(ctx, id, to, c)
	if err == nil && to == run.StageStateDispatched {
		r.mu.Lock()
		r.stampDispatchedLocked(st)
		r.mu.Unlock()
	}
	return st, err
}

// TestFixupRecoveredThenSecondFixup_AuditsVerifiedTreeDiscard is the #3641
// cross-layer lifecycle on the real handlers: a fix-up pass's push fails, the
// /pull-request failure report recovers the stage to its review gate and
// records a push checkpoint, the retry handler refuses the recovered stage, a
// SECOND fix-up is admitted on the delivered-nothing refund, and its signed
// /prompt fetch declines the still-newest checkpoint with exactly one audited
// discard. Each intermediate state is read back, so a wrong premise fails at
// setup rather than at the final assertion.
func TestFixupRecoveredThenSecondFixup_AuditsVerifiedTreeDiscard(t *testing.T) {
	rr := &fixupDiscardLifecycleRepo{&fixupRecoveryRepo{newOrchestratorRepo()}}
	art := newFakeArtifactRepo()
	sf := newSigningFake()
	au := &auditFake{stampSequence: true}
	cr := newFakeConcernRepo()

	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateSucceeded)
	seedPlanArtifactForRun(t, art, planStage.ID, 15)
	impl := rr.seedStage(runRow.ID, 1, run.StageStateRunning)
	review := rr.seedStage(runRow.ID, 2, run.StageStatePending)
	rr.mu.Lock()
	impl.Type = run.StageTypeImplement
	review.Type = run.StageTypeReview
	rr.mu.Unlock()

	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		AuditRepo:    au,
		RunRepo:      rr,
		ArtifactRepo: art,
		ConcernRepo:  cr,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})
	priv, _ := sf.issue(t, runRow.ID)

	// SEED BY CONSTRUCTION the first fix-up's production-shaped trigger, the
	// lowest sequence on the chain.
	firstConcern := planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "edited an out-of-scope file"}
	triggerPayload, err := json.Marshal(map[string]any{
		"stage_id":                 impl.ID.String(),
		"prior_state":              string(run.StageStateSucceeded),
		"reparked_review_stage_id": review.ID.String(),
		"concerns":                 []planreview.Concern{firstConcern},
		"concern_ids":              []string{uuid.NewString()},
		"pass_ordinal":             1,
	})
	if err != nil {
		t.Fatal(err)
	}
	implID := impl.ID
	if _, err := au.AppendChained(t.Context(), audit.ChainAppendParams{
		RunID: runRow.ID, StageID: &implID, Category: CategoryStageFixupTriggered, Payload: triggerPayload,
	}); err != nil {
		t.Fatalf("seed stage_fixup_triggered: %v", err)
	}

	snapshot := func() []audit.ChainAppendParams {
		au.mu.Lock()
		defer au.mu.Unlock()
		return append([]audit.ChainAppendParams(nil), au.appended...)
	}

	// (1) The first fix-up's push fails: the real signed /pull-request report.
	const (
		branch  = "fishhawk/run-3641/stage-implement"
		headSHA = "head3641"
		baseSHA = "base3641"
		treeSHA = "tree3641"
	)
	failBody, err := json.Marshal(map[string]any{
		"outcome": "failed", "category": "C", "reason": "commit+push: gitops: push: push failed",
		"branch": branch, "head_sha": headSHA, "base_sha": baseSHA,
		"verified_tree_sha": treeSHA, "resume_kind": "push",
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := shipPRRequest(t, s, runRow.ID, impl.ID, priv, failBody, ""); w.Code != http.StatusOK {
		t.Fatalf("failed report status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	gotImpl, err := rr.GetStage(t.Context(), impl.ID)
	if err != nil {
		t.Fatalf("GetStage implement: %v", err)
	}
	if gotImpl.State != run.StageStateSucceeded || gotImpl.FailureCategory != nil || gotImpl.FailureReason != nil {
		t.Fatalf("after failed report: implement state=%q category=%v reason=%v, want RECOVERED (succeeded, no failure)",
			gotImpl.State, gotImpl.FailureCategory, gotImpl.FailureReason)
	}
	gotReview, err := rr.GetStage(t.Context(), review.ID)
	if err != nil {
		t.Fatalf("GetStage review: %v", err)
	}
	if gotReview.State != run.StageStateAwaitingApproval {
		t.Fatalf("after failed report: review state=%q, want awaiting_approval (re-parked gate restored)", gotReview.State)
	}
	entries := snapshot()
	recovered := entriesByCategory(entries, CategoryStageFixupRecovered)
	if len(recovered) != 1 {
		t.Fatalf("stage_fixup_recovered entries = %d, want 1", len(recovered))
	}
	var recPayload map[string]any
	_ = json.Unmarshal(recovered[0].Payload, &recPayload)
	if recPayload["source_failure_category"] != string(run.FailureC) {
		t.Errorf("stage_fixup_recovered source_failure_category = %v, want C", recPayload["source_failure_category"])
	}
	if n := len(entriesByCategory(entries, CategoryPushResumeCheckpoint)); n != 1 {
		t.Fatalf("%s entries = %d, want 1", CategoryPushResumeCheckpoint, n)
	}
	if rows := discardRows(entries, impl.ID); len(rows) != 0 {
		t.Fatalf("after failed report: discard rows = %d, want 0 (the report records a checkpoint, not a discard)", len(rows))
	}
	if _, verdict := s.newestPushCheckpoint(t.Context(), runRow.ID, impl.ID); verdict != checkpointResumable {
		t.Fatalf("after recovery: checkpoint verdict = %q, want %q", verdict, checkpointResumable)
	}

	// (2) R1 cross-check: retry is NOT the path — the recovered stage is not
	// failed, so the real retry handler refuses it.
	if w := postRetry(t, s, impl.ID); w.Code >= 200 && w.Code < 300 {
		t.Fatalf("retry status = %d, want a refusal on a recovered (non-failed) stage:\n%s", w.Code, w.Body.String())
	}
	if gotImpl, err = rr.GetStage(t.Context(), impl.ID); err != nil || gotImpl.State != run.StageStateSucceeded {
		t.Fatalf("after refused retry: implement state = %v (err %v), want still succeeded", gotImpl, err)
	}

	// (3) The SECOND fix-up through the real handler. The binding text rides
	// operator_concern; reason is an audit note only (#5561).
	if w := postFixup(t, s, impl.ID, fixupRequest{
		OperatorConcern: "Re-apply the scope fix and keep edits inside the declared files.",
		Reason:          "audit: second pass after the recovered push failure",
	}); w.Code != http.StatusOK {
		t.Fatalf("second fixup status = %d, want 200 (the recovered pass refunds):\n%s", w.Code, w.Body.String())
	}
	entries = snapshot()
	triggers := entriesByCategory(entries, CategoryStageFixupTriggered)
	if len(triggers) != 2 {
		t.Fatalf("stage_fixup_triggered entries = %d, want 2", len(triggers))
	}
	var trig map[string]any
	_ = json.Unmarshal(triggers[len(triggers)-1].Payload, &trig)
	if trig["refunded_passes"] != float64(1) {
		t.Errorf("second trigger refunded_passes = %v, want 1", trig["refunded_passes"])
	}
	if _, verdict := s.newestPushCheckpoint(t.Context(), runRow.ID, impl.ID); verdict != checkpointResumable {
		t.Fatalf("after second fix-up: checkpoint verdict = %q, want still %q", verdict, checkpointResumable)
	}

	// (4) Walk to running, skipping states the handler's orchestrator took.
	if gotImpl, err = rr.GetStage(t.Context(), impl.ID); err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	walk := []run.StageState{run.StageStateDispatched, run.StageStateRunning}
	switch gotImpl.State {
	case run.StageStatePending:
	case run.StageStateDispatched:
		walk = walk[1:]
	case run.StageStateRunning:
		walk = nil
	default:
		t.Fatalf("after second fix-up: implement state = %q, want pending/dispatched/running", gotImpl.State)
	}
	for _, to := range walk {
		if _, err := rr.TransitionStage(t.Context(), impl.ID, to, nil); err != nil {
			t.Fatalf("TransitionStage %s: %v", to, err)
		}
	}

	// (5) The second fix-up's real signed /prompt fetch.
	pw := promptRequestWithCapabilities(t, s, impl.ID, priv, capableRunnerCapabilities)
	keys := exemptBodyKeys(t, pw, "/prompt")
	if string(keys["fixup"]) != "true" {
		t.Fatalf("fixup = %s, want true", keys["fixup"])
	}
	assertNoResumeEmission(t, keys, "the second fix-up's dispatch")
	for _, k := range []string{"held_commit_verified_tree_sha", "held_commit_pr_title", "held_commit_pr_body", "held_commit_review_obligation"} {
		if raw, ok := keys[k]; ok {
			t.Errorf("%s must be absent on a fix-up, got %s", k, raw)
		}
	}
	assertOneFixupDiscard(t, discardRows(snapshot(), impl.ID), treeSHA, headSHA, branch)

	// (6) A preview render on the same state adds no row.
	_ = exemptBodyKeys(t, promptRenderRequest(t, s, impl.ID), "/prompt-render")
	if rows := discardRows(snapshot(), impl.ID); len(rows) != 1 {
		t.Errorf("after /prompt-render: discard rows = %d, want still 1", len(rows))
	}
}

// TestFixupDispatchDiscard_Documented pins the fixup_dispatch reason into both
// API documents. The word boundary keeps the unrelated acceptance-triage token
// fixup_dispatched from satisfying it.
func TestFixupDispatchDiscard_Documented(t *testing.T) {
	re := regexp.MustCompile(`fixup_dispatch\b`)
	for _, rel := range []string{"docs/api/v0.openapi.yaml", "docs/api/v0.md"} {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !re.Match(b) {
			t.Errorf("%s must document the verified_tree_discarded reason fixup_dispatch (#3641)", rel)
		}
	}
}
