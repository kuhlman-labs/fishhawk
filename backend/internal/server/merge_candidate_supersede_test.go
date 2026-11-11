package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// #4811: the stale-undispatched-pass supersede core. Fake-backed over
// newMCVFixture; the Postgres end-to-end path through the real rebase handler
// lives in merge_candidate_queue_test.go (slice 1).

const (
	supHeadOld = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	supHeadNew = "a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2"
)

// mcvStageListErrRepo fails ListStagesForRun (R2), counting the calls. It
// wraps mcvRepo (server_test.go's stageListErrRepo wraps approvalRunRepo).
type mcvStageListErrRepo struct {
	*mcvRepo
	err   error
	mu    sync.Mutex
	calls int
}

func (r *mcvStageListErrRepo) ListStagesForRun(_ context.Context, _ uuid.UUID) ([]*run.Stage, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return nil, r.err
}

// nthListErrAudit fails ONLY the failOn-th ListForRunByCategory call for
// category (approval condition D3): an earlier read of the same category (the
// detection) succeeds and the settle-time read fails.
type nthListErrAudit struct {
	*auditFake
	category string
	failOn   int
	mu       sync.Mutex
	calls    int
}

func (a *nthListErrAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == a.category {
		a.mu.Lock()
		a.calls++
		n := a.calls
		a.mu.Unlock()
		if n == a.failOn {
			return nil, errors.New("nthListErrAudit: injected read error on call " + category)
		}
	}
	return a.auditFake.ListForRunByCategory(ctx, runID, category)
}

// syncBuffer (crew_consult_test.go) is the goroutine-safe log sink.

// supFixture is newMCVFixture with the implement stage at state, a pending
// review stage the trigger re-parked, and trigger A for supHeadOld (prior
// state succeeded). It returns A's chain sequence.
type supFixture struct {
	*mcvFixture
	review *run.Stage
	seqA   int64
	logs   *syncBuffer
}

func newSupFixture(t *testing.T, state run.StageState) *supFixture {
	t.Helper()
	f := newMCVFixture(t, mcvUndelegatedSpecYAML)
	logs := &syncBuffer{}
	f.s.cfg.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	review := &run.Stage{ID: uuid.New(), RunID: f.runID, Type: run.StageTypeReview,
		State: run.StageStatePending, Sequence: 3}
	f.repo.mu.Lock()
	f.repo.stagesByRun[f.runID] = append(f.repo.stagesByRun[f.runID], review)
	f.repo.mu.Unlock()
	f.impl.State = state
	sf := &supFixture{mcvFixture: f, review: review, logs: logs}
	sf.seqA = sf.seedTriggerFor(supHeadOld)
	return sf
}

// seedTriggerFor appends a populated trigger for head (prior succeeded,
// re-parking the fixture's review) and returns its sequence.
func (f *supFixture) seedTriggerFor(head string) int64 {
	id := f.impl.ID
	f.appendRow(&id, CategoryStageMergeCandidateVerifyTriggered, mergeCandidateVerifyTrigger{
		Branch: mcvBranch, BaseRef: mcvBaseRef, ExpectedHeadSHA: head,
		Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: mcvCommand,
		PriorState: string(run.StageStateSucceeded), ReparkedReviewStageID: f.review.ID.String(),
	})
	return int64(chainLen(f.au))
}

func (f *supFixture) headRow(category, field, head string) {
	f.appendRow(nil, category, map[string]any{field: head})
}

func (f *supFixture) state(t *testing.T, id uuid.UUID) run.StageState {
	t.Helper()
	st, err := f.repo.GetStage(context.Background(), id)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	return st.State
}

func (f *supFixture) live(t *testing.T) *liveTrigger {
	t.Helper()
	live, err := f.s.liveMergeCandidateTrigger(context.Background(), f.runID, f.impl.ID)
	if err != nil {
		t.Fatalf("liveMergeCandidateTrigger: %v", err)
	}
	return live
}

func (f *supFixture) verdicts(t *testing.T) []mergeCandidateVerifiedPayload {
	t.Helper()
	var out []mergeCandidateVerifiedPayload
	for _, row := range auditEntries(f.au, CategoryMergeCandidateVerified) {
		var p mergeCandidateVerifiedPayload
		if err := json.Unmarshal(row.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func (f *supFixture) supersede(t *testing.T, ctx context.Context, producerHead string) (mergeCandidateSupersession, error) {
	t.Helper()
	return f.s.supersedeStaleMergeCandidatePass(ctx, f.runID, f.impl.ID, producerHead)
}

// assertSettled is a settled supersession's committed state.
func (f *supFixture) assertSettled(t *testing.T, sup mergeCandidateSupersession) {
	t.Helper()
	if !sup.Superseded || !sup.Settled || sup.Step != mergeCandidateSettled {
		t.Fatalf("supersession = %+v, want settled", sup)
	}
	v := f.verdicts(t)
	if len(v) != 1 {
		t.Fatalf("merge_candidate_verified rows = %d, want 1", len(v))
	}
	if v[0].Result != mergeCandidateResultNotExecuted || !strings.HasPrefix(v[0].Reason, mergeCandidateReasonSuperseded+":") ||
		v[0].TriggerSequence != f.seqA || v[0].HeadSHA != supHeadOld {
		t.Errorf("verdict = %+v, want not_executed superseded: for trigger %d at %s", v[0], f.seqA, supHeadOld)
	}
	if st := f.state(t, f.impl.ID); st != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want the trigger's prior_state succeeded", st)
	}
	if st := f.state(t, f.review.ID); st != run.StageStateAwaitingApproval {
		t.Errorf("review state = %q, want re-parked awaiting_approval", st)
	}
	if live := f.live(t); live != nil {
		t.Errorf("live trigger = %+v, want consumed", live)
	}
}

// assertUntouched is an untouched answer's committed state.
func (f *supFixture) assertUntouched(t *testing.T, sup mergeCandidateSupersession, before int, state run.StageState) {
	t.Helper()
	if sup.Superseded || sup.Settled {
		t.Errorf("supersession = %+v, want none", sup)
	}
	if n := len(chainDelta(f.au, before)); n != 0 {
		t.Errorf("rows appended = %d, want 0", n)
	}
	if st := f.state(t, f.impl.ID); st != state {
		t.Errorf("implement state = %q, want unchanged %q", st, state)
	}
}

func TestSupersedeStaleMergeCandidatePass_Modes(t *testing.T) {
	type mode struct {
		name     string
		state    run.StageState
		noSeedA  bool
		setup    func(f *supFixture)
		producer string
		settled  bool
	}
	var modes []mode
	for _, src := range gateViewCurrentHeadSources {
		src := src
		modes = append(modes, mode{name: "post-trigger " + src.category + " names a new head", settled: true,
			setup: func(f *supFixture) { f.headRow(src.category, src.field, supHeadNew) }})
	}
	modes = append(modes,
		mode{name: "merge_commit_sha-only branch_rebased", settled: true,
			setup: func(f *supFixture) { f.headRow(CategoryBranchRebased, "merge_commit_sha", supHeadNew) }},
		mode{name: "producer head differs with no rows", settled: true, producer: supHeadNew},
		mode{name: "pending stage", state: run.StageStatePending, settled: true,
			setup: func(f *supFixture) { f.headRow("fixup_pushed", "head_sha", supHeadNew) }},

		mode{name: "pre-trigger row names a new head", noSeedA: true, setup: func(f *supFixture) {
			f.headRow("fixup_pushed", "head_sha", supHeadNew)
			f.seqA = f.seedTriggerFor(supHeadOld)
		}},
		mode{name: "post-trigger row names the expected head",
			setup: func(f *supFixture) { f.headRow("fixup_pushed", "head_sha", supHeadOld) }},
		mode{name: "already_up_to_date rebase", setup: func(f *supFixture) {
			f.appendRow(nil, CategoryBranchRebased, map[string]any{"new_head_sha": supHeadNew, "already_up_to_date": true})
		}},
		mode{name: "newest post-trigger head is the expected head", setup: func(f *supFixture) {
			f.headRow("fixup_pushed", "head_sha", supHeadNew)
			f.headRow(CategoryOperatorCommitVouched, "vouched_sha", supHeadOld)
		}},
		mode{name: "dispatched stage", state: run.StageStateDispatched,
			setup: func(f *supFixture) { f.headRow("fixup_pushed", "head_sha", supHeadNew) }},
		mode{name: "running stage", state: run.StageStateRunning,
			setup: func(f *supFixture) { f.headRow("fixup_pushed", "head_sha", supHeadNew) }},
		mode{name: "no trigger", noSeedA: true,
			setup: func(f *supFixture) { f.headRow("fixup_pushed", "head_sha", supHeadNew) }},
		mode{name: "malformed newest trigger reads as no pass (R6)", setup: func(f *supFixture) {
			id := f.impl.ID
			f.appendRow(&id, CategoryStageMergeCandidateVerifyTriggered, mergeCandidateVerifyTrigger{
				Branch: mcvBranch, BaseRef: mcvBaseRef, ExpectedHeadSHA: supHeadOld,
				Cause: mergeCandidateCauseBaseAdvance, PriorState: string(run.StageStateSucceeded),
			})
			f.headRow("fixup_pushed", "head_sha", supHeadNew)
		}},
	)
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			state := m.state
			if state == "" {
				state = run.StageStateAwaitingHostDispatch
			}
			f := newMCVFixture(t, mcvUndelegatedSpecYAML)
			review := &run.Stage{ID: uuid.New(), RunID: f.runID, Type: run.StageTypeReview, State: run.StageStatePending, Sequence: 3}
			f.repo.mu.Lock()
			f.repo.stagesByRun[f.runID] = append(f.repo.stagesByRun[f.runID], review)
			f.repo.mu.Unlock()
			f.impl.State = state
			sf := &supFixture{mcvFixture: f, review: review, logs: &syncBuffer{}}
			if !m.noSeedA {
				sf.seqA = sf.seedTriggerFor(supHeadOld)
			}
			if m.setup != nil {
				m.setup(sf)
			}
			before := chainLen(f.au)
			sup, err := sf.supersede(t, context.Background(), m.producer)
			if err != nil {
				t.Fatalf("supersede error = %v, want nil", err)
			}
			if m.settled {
				sf.assertSettled(t, sup)
				return
			}
			sf.assertUntouched(t, sup, before, state)
		})
	}
}

func TestSupersedeStaleMergeCandidatePass_StageMismatchShortCircuits(t *testing.T) {
	// D5: the caller's already-loaded stage ID is not the run's implement
	// stage, so nothing is read past the stage list and nothing is written.
	f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
	f.headRow("fixup_pushed", "head_sha", supHeadNew)
	before := chainLen(f.au)
	sup, err := f.s.supersedeStaleMergeCandidatePass(context.Background(), f.runID, uuid.New(), "")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	f.assertUntouched(t, sup, before, run.StageStateAwaitingHostDispatch)
	if f.live(t) == nil {
		t.Error("trigger A was consumed, want it live")
	}
}

func TestSupersedeStaleMergeCandidatePass_SettleFailures(t *testing.T) {
	t.Run("record append refused", func(t *testing.T) {
		f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
		f.headRow("fixup_pushed", "head_sha", supHeadNew)
		f.au.appendErr = errors.New("injected append failure")
		before := chainLen(f.au)
		sup, err := f.supersede(t, context.Background(), "")
		if err != nil {
			t.Fatalf("err = %v, want nil (record_failed is a step, not an error)", err)
		}
		if sup.Step != mergeCandidateSettleRecordFailed || sup.Settled {
			t.Fatalf("supersession = %+v, want record_failed", sup)
		}
		if n := len(chainDelta(f.au, before)); n != 0 {
			t.Errorf("rows appended = %d, want 0", n)
		}
		if st := f.state(t, f.impl.ID); st != run.StageStateAwaitingHostDispatch {
			t.Errorf("state = %q, want untouched awaiting_host_dispatch", st)
		}
		if live := f.live(t); live == nil || live.sequence != f.seqA {
			t.Errorf("live = %+v, want trigger A still live", live)
		}
	})
	walks := []struct {
		name     string
		from     run.StageState
		refuseTo run.StageState
		left     run.StageState
		warn     string
	}{
		{"awaiting_host_dispatch, the last FailStage step fails", run.StageStateAwaitingHostDispatch, run.StageStateFailed, run.StageStateRunning, "fishhawk_reap_stage"},
		{"awaiting_host_dispatch, dispatched to running fails", run.StageStateAwaitingHostDispatch, run.StageStateRunning, run.StageStateDispatched, "dispatch watchdog"},
		{"pending, FailStage fails", run.StageStatePending, run.StageStateFailed, run.StageStatePending, "fishhawk_reap_stage"},
	}
	for _, w := range walks {
		t.Run(w.name, func(t *testing.T) {
			f := newSupFixture(t, w.from)
			f.headRow("fixup_pushed", "head_sha", supHeadNew)
			f.repo.refuseTo = w.refuseTo
			sup, err := f.supersede(t, context.Background(), "")
			f.repo.refuseTo = ""
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if sup.Step != mergeCandidateSettleBeforeRestoreFailed || sup.LeftState != w.left {
				t.Fatalf("supersession = %+v, want before_restore_failed leaving %q", sup, w.left)
			}
			if st := f.state(t, f.impl.ID); st != w.left {
				t.Errorf("persisted state = %q, want EXACTLY %q", st, w.left)
			}
			if v := f.verdicts(t); len(v) != 1 || v[0].TriggerSequence != f.seqA {
				t.Errorf("verdicts = %+v, want one row consuming trigger A", v)
			}
			if live := f.live(t); live != nil {
				t.Errorf("live = %+v, want trigger consumed", live)
			}
			logs := f.logs.String()
			if !strings.Contains(logs, w.warn) || !strings.Contains(logs, "left "+string(w.left)) {
				t.Errorf("WARN does not name %q and the left state %q:\n%s", w.warn, w.left, logs)
			}
		})
	}
	t.Run("restore refused", func(t *testing.T) {
		f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
		f.headRow("fixup_pushed", "head_sha", supHeadNew)
		f.repo.refuseTo = run.StageStateAwaitingApproval
		sup, err := f.supersede(t, context.Background(), "")
		f.repo.refuseTo = ""
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if sup.Step != mergeCandidateSettleRestoreFailed {
			t.Fatalf("supersession = %+v, want restore_failed", sup)
		}
		if st := f.state(t, f.impl.ID); st != run.StageStateFailed {
			t.Errorf("state = %q, want failed", st)
		}
	})
}

func TestSupersedeStaleMergeCandidatePass_ReadErrorReturned(t *testing.T) {
	f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
	f.headRow("fixup_pushed", "head_sha", supHeadNew)
	f.au.listByCategoryErrCategory = "fixup_pushed"
	before := chainLen(f.au)
	sup, err := f.supersede(t, context.Background(), "")
	if err == nil {
		t.Fatalf("err = nil (supersession %+v), want the head-source read error", sup)
	}
	f.au.listByCategoryErrCategory = ""
	f.assertUntouched(t, sup, before, run.StageStateAwaitingHostDispatch)
}

func TestSupersedeStaleMergeCandidatePass_StageListErrorReturned(t *testing.T) {
	f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
	f.headRow("fixup_pushed", "head_sha", supHeadNew)
	wrapper := &mcvStageListErrRepo{mcvRepo: f.repo, err: errors.New("injected stage list failure")}
	f.s.cfg.RunRepo = wrapper
	before := chainLen(f.au)
	sup, err := f.supersede(t, context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "injected stage list failure") {
		t.Fatalf("err = %v (supersession %+v), want the stage-list error, not absence", err, sup)
	}
	f.s.cfg.RunRepo = f.repo
	f.assertUntouched(t, sup, before, run.StageStateAwaitingHostDispatch)
}

// TestSupersedeStaleMergeCandidatePass_SettleTimeReadErrorReturned is approval
// condition D3: detection reads the trigger chain fine, then the settle's own
// read fails. That is an error (the marker's 500), never no_live_trigger.
func TestSupersedeStaleMergeCandidatePass_SettleTimeReadErrorReturned(t *testing.T) {
	for _, tc := range []struct {
		name   string
		failOn int
	}{
		{"the settle's own trigger resolve", 2},
		{"the pinned record's trigger read", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
			f.headRow("fixup_pushed", "head_sha", supHeadNew)
			wrap := &nthListErrAudit{auditFake: f.au, category: CategoryStageMergeCandidateVerifyTriggered, failOn: tc.failOn}
			f.s.cfg.AuditRepo = wrap
			before := chainLen(f.au)
			sup, err := f.supersede(t, context.Background(), "")
			f.s.cfg.AuditRepo = f.au
			if err == nil {
				t.Fatalf("err = nil (supersession %+v), want the settle-time read error", sup)
			}
			if sup.Step != mergeCandidateSettleReadFailed || !sup.Superseded || sup.Settled {
				t.Errorf("supersession = %+v, want detected then read_failed", sup)
			}
			if wrap.calls < tc.failOn {
				t.Errorf("trigger reads = %d, want the failure injected on read %d", wrap.calls, tc.failOn)
			}
			if n := len(chainDelta(f.au, before)); n != 0 {
				t.Errorf("rows appended = %d, want 0", n)
			}
			if st := f.state(t, f.impl.ID); st != run.StageStateAwaitingHostDispatch {
				t.Errorf("state = %q, want untouched", st)
			}
		})
	}
}

// TestSettleStaleMergeCandidatePass_ConcurrentReplacementStaysLive is R1:
// detection pins A, then the REAL begin appends replacement B before the
// settle. The pinned record refuses, so B stays live and dispatchable.
func TestSettleStaleMergeCandidatePass_ConcurrentReplacementStaysLive(t *testing.T) {
	f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
	f.headRow("fixup_pushed", "head_sha", supHeadNew)
	ctx := context.Background()
	stale, err := f.s.detectStaleMergeCandidatePass(ctx, f.runID, f.impl.ID, "")
	if err != nil || stale == nil || stale.TriggerSequence != f.seqA {
		t.Fatalf("detect = %+v, %v; want stale pinned to A (%d)", stale, err, f.seqA)
	}
	start, refusal := f.s.beginMergeCandidateVerifyPass(ctx, f.runID, mergeCandidateVerifyParams{
		Branch: mcvBranch, BaseRef: mcvBaseRef, HeadSHA: supHeadNew, Cause: mergeCandidateCauseFanIn,
	}, mergeCandidateSystemActor())
	if refusal != nil || !start.Triggered {
		t.Fatalf("begin = %+v, %+v; want replacement B appended", start, refusal)
	}
	seqB := int64(chainLen(f.au))

	step, err := f.s.settleStaleMergeCandidatePass(ctx, f.runID, stale)
	if err != nil || step != mergeCandidateSettleTriggerReplaced {
		t.Fatalf("settle = %v, %v; want trigger_replaced", mergeCandidateSettleStepName(step), err)
	}
	if v := f.verdicts(t); len(v) != 0 {
		t.Errorf("merge_candidate_verified rows = %+v, want zero", v)
	}
	live := f.live(t)
	if live == nil || live.sequence != seqB || live.payload.ExpectedHeadSHA != supHeadNew {
		t.Errorf("live = %+v, want B (sequence %d) for %s", live, seqB, supHeadNew)
	}
	if st := f.state(t, f.impl.ID); st != run.StageStateAwaitingHostDispatch {
		t.Errorf("state = %q, want awaiting_host_dispatch", st)
	}
	again, err := f.s.detectStaleMergeCandidatePass(ctx, f.runID, f.impl.ID, "")
	if err != nil || again != nil {
		t.Errorf("second detect = %+v, %v; want nil (the a2 row precedes B)", again, err)
	}
}

// TestBeginMergeCandidateVerifyPass_ReplacementWaitsForAdmissionFence is the
// R1 fence and approval condition D1: a replacement never appends while the
// implement stage's fence is held, and its bounded miss is Transient.
func TestBeginMergeCandidateVerifyPass_ReplacementWaitsForAdmissionFence(t *testing.T) {
	params := mergeCandidateVerifyParams{Branch: mcvBranch, BaseRef: mcvBaseRef, HeadSHA: supHeadNew, Cause: mergeCandidateCauseFanIn}
	triggersFor := func(f *supFixture, head string) int {
		n := 0
		for _, row := range mcvTriggerRows(f.au) {
			var p mergeCandidateVerifyTrigger
			if json.Unmarshal(row.Payload, &p) == nil && p.ExpectedHeadSHA == head {
				n++
			}
		}
		return n
	}

	t.Run("held fence refuses at its bound, then appends after unlock", func(t *testing.T) {
		f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
		unlock := f.s.cfg.Orchestrator.LockStageAdmission(f.impl.ID)
		start, refusal := f.s.beginMergeCandidateVerifyPass(context.Background(), f.runID, params, mergeCandidateSystemActor())
		if refusal == nil {
			unlock()
			t.Fatalf("start = %+v, want a fence refusal", start)
		}
		if !refusal.Transient || !strings.Contains(refusal.Reason, "admission fence is busy") {
			t.Errorf("refusal = %+v, want a Transient fence refusal", refusal)
		}
		if n := triggersFor(f, supHeadNew); n != 0 {
			t.Errorf("a2 triggers = %d, want 0 while the fence is held", n)
		}
		if live := f.live(t); live == nil || live.sequence != f.seqA {
			t.Errorf("live = %+v, want A", live)
		}
		unlock()
		start, refusal = f.s.beginMergeCandidateVerifyPass(context.Background(), f.runID, params, mergeCandidateSystemActor())
		if refusal != nil || !start.Triggered || triggersFor(f, supHeadNew) != 1 {
			t.Errorf("after unlock: start=%+v refusal=%+v, want B appended", start, refusal)
		}
	})
	t.Run("re-entrant context appends without waiting", func(t *testing.T) {
		f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
		ctx, unlock := f.s.holdStageAdmission(context.Background(), f.impl.ID)
		defer unlock()
		began := time.Now()
		start, refusal := f.s.beginMergeCandidateVerifyPass(ctx, f.runID, params, mergeCandidateSystemActor())
		if refusal != nil || !start.Triggered || triggersFor(f, supHeadNew) != 1 {
			t.Fatalf("start=%+v refusal=%+v, want B appended", start, refusal)
		}
		if waited := time.Since(began); waited >= stageAdmissionTryWait {
			t.Errorf("waited %v, want no wait on the caller's own fence", waited)
		}
	})
}

// advanceRecorder swaps mergeCandidateSupersedeAdvance (never t.Parallel) for
// a recorder that probes the implement stage's fence with a bounded wait.
type advanceRecorder struct {
	mu     sync.Mutex
	calls  int
	probes []bool
}

func swapSupersedeAdvance(t *testing.T, implID uuid.UUID) *advanceRecorder {
	t.Helper()
	rec := &advanceRecorder{}
	prev := mergeCandidateSupersedeAdvance
	t.Cleanup(func() { mergeCandidateSupersedeAdvance = prev })
	mergeCandidateSupersedeAdvance = func(_ context.Context, s *Server, _ uuid.UUID) error {
		_, unlock, ok := s.tryHoldStageAdmission(context.Background(), implID, 200*time.Millisecond)
		unlock()
		rec.mu.Lock()
		rec.calls++
		rec.probes = append(rec.probes, ok)
		rec.mu.Unlock()
		return nil
	}
	return rec
}

func (r *advanceRecorder) snapshot() (int, []bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, append([]bool(nil), r.probes...)
}

func TestSupersedeStalePassOnQueueAnswer_AdvancesOutsideFence(t *testing.T) {
	f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
	rec := swapSupersedeAdvance(t, f.impl.ID)
	if err := f.s.supersedeStalePassOnQueueAnswer(context.Background(), f.runID, supHeadNew, mergeCandidateStateHeld); err != nil {
		t.Fatalf("err = %v", err)
	}
	f.assertSettled(t, mergeCandidateSupersession{Superseded: true, Settled: true, Step: mergeCandidateSettled})
	calls, probes := rec.snapshot()
	if calls != 1 || len(probes) != 1 || !probes[0] {
		t.Errorf("advance calls = %d probes = %v, want 1 call with the fence acquirable", calls, probes)
	}
}

func TestSupersedeStalePassOnQueueAnswer_SkipsAdvanceWhenCallerHoldsFence(t *testing.T) {
	f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
	rec := swapSupersedeAdvance(t, f.impl.ID)
	ctx, unlock := f.s.holdStageAdmission(context.Background(), f.impl.ID)
	err := f.s.supersedeStalePassOnQueueAnswer(ctx, f.runID, supHeadNew, mergeCandidateStateHeld)
	unlock()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	f.assertSettled(t, mergeCandidateSupersession{Superseded: true, Settled: true, Step: mergeCandidateSettled})
	if calls, _ := rec.snapshot(); calls != 0 {
		t.Errorf("advance calls = %d, want 0 under the caller's fence", calls)
	}
}

func TestSupersedeStalePassOnQueueAnswer_SkipsWhenFenceBusy(t *testing.T) {
	f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
	rec := swapSupersedeAdvance(t, f.impl.ID)
	unlock := f.s.cfg.Orchestrator.LockStageAdmission(f.impl.ID)
	before := chainLen(f.au)
	began := time.Now()
	err := f.s.supersedeStalePassOnQueueAnswer(context.Background(), f.runID, supHeadNew, mergeCandidateStateNotQueued)
	waited := time.Since(began)
	unlock()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if waited < stageAdmissionTryWait/2 {
		t.Errorf("returned after %v, want about stageAdmissionTryWait", waited)
	}
	if calls, _ := rec.snapshot(); calls != 0 {
		t.Errorf("advance calls = %d, want 0", calls)
	}
	if v := f.verdicts(t); len(v) != 0 {
		t.Errorf("verdicts = %+v, want none: no settle without the fence", v)
	}
	if n := len(chainDelta(f.au, before)); n != 0 {
		t.Errorf("rows appended = %d, want 0", n)
	}
	if live := f.live(t); live == nil || live.sequence != f.seqA {
		t.Errorf("live = %+v, want A", live)
	}
	if st := f.state(t, f.impl.ID); st != run.StageStateAwaitingHostDispatch {
		t.Errorf("state = %q, want unchanged", st)
	}
}

// TestSupersedeStalePassOnQueueAnswer_AdvanceCondition is approval condition
// D4 for the producer arm: restore_failed advances (the failure path stays in
// force), before_restore_failed and record_failed do not.
func TestSupersedeStalePassOnQueueAnswer_AdvanceCondition(t *testing.T) {
	for _, tc := range []struct {
		name      string
		refuseTo  run.StageState
		appendErr bool
		step      mergeCandidateSettleStep
		calls     int
	}{
		{"restore_failed advances", run.StageStateSucceeded, false, mergeCandidateSettleRestoreFailed, 1},
		{"before_restore_failed does not", run.StageStateFailed, false, mergeCandidateSettleBeforeRestoreFailed, 0},
		{"record_failed does not", "", true, mergeCandidateSettleRecordFailed, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
			rec := swapSupersedeAdvance(t, f.impl.ID)
			f.repo.refuseTo = tc.refuseTo
			if tc.appendErr {
				f.au.appendErr = errors.New("injected append failure")
			}
			err := f.s.supersedeStalePassOnQueueAnswer(context.Background(), f.runID, supHeadNew, mergeCandidateStateHeld)
			f.repo.refuseTo, f.au.appendErr = "", nil
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !strings.Contains(f.logs.String(), "settle_step="+mergeCandidateSettleStepName(tc.step)) {
				t.Fatalf("logs do not name step %s:\n%s", mergeCandidateSettleStepName(tc.step), f.logs.String())
			}
			if calls, _ := rec.snapshot(); calls != tc.calls {
				t.Errorf("advance calls = %d, want %d", calls, tc.calls)
			}
		})
	}
}

// TestSupersedeStalePassOnQueueAnswer_NoOrchestrator is approval condition D2:
// with no Orchestrator wired the pass settles, the call site skips Advance, and
// the seam itself is nil-safe.
func TestSupersedeStalePassOnQueueAnswer_NoOrchestrator(t *testing.T) {
	t.Run("call site skips Advance", func(t *testing.T) {
		f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
		rec := swapSupersedeAdvance(t, f.impl.ID)
		f.s.cfg.Orchestrator = nil
		if err := f.s.supersedeStalePassOnQueueAnswer(context.Background(), f.runID, supHeadNew, mergeCandidateStateHeld); err != nil {
			t.Fatalf("err = %v", err)
		}
		f.assertSettled(t, mergeCandidateSupersession{Superseded: true, Settled: true, Step: mergeCandidateSettled})
		if calls, _ := rec.snapshot(); calls != 0 {
			t.Errorf("advance calls = %d, want 0 with no Orchestrator", calls)
		}
	})
	t.Run("the seam is nil-safe", func(t *testing.T) {
		f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
		f.s.cfg.Orchestrator = nil
		if err := mergeCandidateSupersedeAdvance(context.Background(), f.s, f.runID); err != nil {
			t.Errorf("seam err = %v, want nil", err)
		}
	})
}

func TestAnswerQueuedWithoutPass_StageListErrorRefuses(t *testing.T) {
	f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
	f.s.cfg.RunRepo = &mcvStageListErrRepo{mcvRepo: f.repo, err: errors.New("injected stage list failure")}
	before := chainLen(f.au)
	start, refusal := f.s.answerQueuedWithoutPass(context.Background(), f.runID, supHeadNew,
		&mergeCandidateVerifyStart{State: mergeCandidateStateHeld})
	f.s.cfg.RunRepo = f.repo
	if start != nil || refusal == nil || !strings.Contains(refusal.Reason, "could not be read") ||
		!strings.Contains(refusal.Reason, "injected stage list failure") || !strings.Contains(refusal.Reason, "fishhawk_rebase_run_branch") {
		t.Fatalf("start=%+v refusal=%+v, want a refusal naming the read failure", start, refusal)
	}
	if n := len(chainDelta(f.au, before)); n != 0 {
		t.Errorf("rows appended = %d, want 0", n)
	}
}

func TestAnswerQueuedWithoutPass_ReturnsTheAnswer(t *testing.T) {
	f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
	swapSupersedeAdvance(t, f.impl.ID)
	want := &mergeCandidateVerifyStart{State: mergeCandidateStateNotQueued}
	start, refusal := f.s.answerQueuedWithoutPass(context.Background(), f.runID, supHeadNew, want)
	if refusal != nil || start != want {
		t.Fatalf("start=%+v refusal=%+v, want the queue's answer returned", start, refusal)
	}
	f.assertSettled(t, mergeCandidateSupersession{Superseded: true, Settled: true, Step: mergeCandidateSettled})
}

// TestSettleStaleMergeCandidatePass_MalformedNewestTriggerWritesNothing is R6
// on the pinned settle: a malformed trigger appended after detection reads as
// no live trigger (nothing written, no error), not as a read failure.
func TestSettleStaleMergeCandidatePass_MalformedNewestTriggerWritesNothing(t *testing.T) {
	f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
	f.headRow("fixup_pushed", "head_sha", supHeadNew)
	ctx := context.Background()
	stale, err := f.s.detectStaleMergeCandidatePass(ctx, f.runID, f.impl.ID, "")
	if err != nil || stale == nil {
		t.Fatalf("detect = %+v, %v; want stale", stale, err)
	}
	id := f.impl.ID
	f.appendRow(&id, CategoryStageMergeCandidateVerifyTriggered, map[string]any{"expected_head_sha": supHeadNew})
	before := chainLen(f.au)
	step, err := f.s.settleStaleMergeCandidatePass(ctx, f.runID, stale)
	if err != nil || step != mergeCandidateSettleNoLiveTrigger {
		t.Fatalf("settle = %s, %v; want no_live_trigger with a nil error", mergeCandidateSettleStepName(step), err)
	}
	if n := len(chainDelta(f.au, before)); n != 0 {
		t.Errorf("rows appended = %d, want 0", n)
	}
	if st := f.state(t, f.impl.ID); st != run.StageStateAwaitingHostDispatch {
		t.Errorf("state = %q, want untouched", st)
	}
}
