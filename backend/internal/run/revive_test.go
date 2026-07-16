package run_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// reviveRepo is an in-memory run.Repository storing one run plus its
// stages so ReviveRun's run-level validation, per-stage re-park, and
// run reopen can be exercised end to end. It embeds BaseFake so only the
// methods ReviveRun (and the run.RetryStage it reuses) touch need real
// behaviour. RetryStage mutates in place and bumps SelfRetryCount,
// mirroring the postgres repo so the per-stage retry-budget consumption
// is observable.
type reviveRepo struct {
	run.BaseFake
	run *run.Run
	// stages are stored as live pointers; mutations land in place and
	// GetStage / ListStagesForRun hand back copies (the postgres
	// isolation posture) so the domain code never shares a live pointer.
	stages []*run.Stage
	// hasChildren makes ListRuns report a decomposition child for this
	// run, so run.RetryStage's decomposed-parent restore (#1891) targets
	// awaiting_children instead of pending.
	hasChildren bool
	// retryStageErr injects a RetryStage failure keyed by stage ID: when
	// set for a stage, RetryStage returns the error BEFORE mutating that
	// stage, modelling a mid-batch re-park failure (#1942).
	retryStageErr map[uuid.UUID]error
	// retryRunErr injects a RetryRun failure: when non-nil, RetryRun returns
	// it without mutating, modelling a tail run-reopen failure (#1942).
	retryRunErr error
}

func (r *reviveRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if r.run == nil || r.run.ID != id {
		return nil, run.ErrNotFound
	}
	cp := *r.run
	return &cp, nil
}

func (r *reviveRepo) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	out := make([]*run.Stage, 0, len(r.stages))
	for _, s := range r.stages {
		if s.RunID == runID {
			cp := *s
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *reviveRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	for _, s := range r.stages {
		if s.ID == id {
			cp := *s
			return &cp, nil
		}
	}
	return nil, run.ErrNotFound
}

func (r *reviveRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	if r.hasChildren && f.DecomposedFrom != nil && *f.DecomposedFrom == r.run.ID {
		// A single sentinel child is enough — run.RetryStage only checks
		// len(children) > 0.
		return []*run.Run{{ID: uuid.New(), DecomposedFrom: &r.run.ID}}, nil
	}
	return nil, nil
}

func (r *reviveRepo) RetryStage(_ context.Context, id uuid.UUID, to run.StageState) (*run.Stage, error) {
	if err, ok := r.retryStageErr[id]; ok && err != nil {
		return nil, err
	}
	for _, s := range r.stages {
		if s.ID != id {
			continue
		}
		if !run.ValidStageRetryTransition(s.State, to) {
			return nil, run.InvalidTransitionError{Kind: "stage", From: string(s.State), To: string(to)}
		}
		s.State = to
		s.FailureCategory = nil
		s.FailureReason = nil
		s.EndedAt = nil
		s.SelfRetryCount++
		cp := *s
		return &cp, nil
	}
	return nil, run.ErrNotFound
}

func (r *reviveRepo) RetryRun(_ context.Context, id uuid.UUID, to run.State) (*run.Run, error) {
	if r.retryRunErr != nil {
		return nil, r.retryRunErr
	}
	if r.run == nil || r.run.ID != id {
		return nil, run.ErrNotFound
	}
	if !run.ValidRunRetryTransition(r.run.State, to) {
		return nil, run.InvalidTransitionError{Kind: "run", From: string(r.run.State), To: string(to)}
	}
	r.run.State = to
	cp := *r.run
	return &cp, nil
}

// reviveStage builds one failed stage of the given type + category.
func reviveStage(runID uuid.UUID, seq int, typ run.StageType, cat run.FailureCategory, reason string) *run.Stage {
	now := time.Now().UTC()
	catCopy := cat
	reasonCopy := reason
	return &run.Stage{
		ID:              uuid.New(),
		RunID:           runID,
		Sequence:        seq,
		Type:            typ,
		State:           run.StageStateFailed,
		FailureCategory: &catCopy,
		FailureReason:   &reasonCopy,
		EndedAt:         &now,
	}
}

// reviveFailedRun builds a failed run with the given stages attached.
func reviveFailedRun(stages ...*run.Stage) *reviveRepo {
	runID := uuid.New()
	for _, s := range stages {
		s.RunID = runID
	}
	return &reviveRepo{
		run: &run.Run{
			ID:         runID,
			Repo:       "kuhlman-labs/fishhawk",
			WorkflowID: "feature_change",
			State:      run.StateFailed,
		},
		stages: stages,
	}
}

// m8: a run that is not in state failed refuses — runRetryTransitions
// admits only failed → running, so revive on cancelled/succeeded/running
// has no defined meaning and must refuse before any mutation.
func TestReviveRun_RejectsNonFailedRun(t *testing.T) {
	for _, st := range []run.State{run.StateRunning, run.StateSucceeded, run.StateCancelled, run.StatePending} {
		repo := reviveFailedRun(reviveStage(uuid.Nil, 0, run.StageTypeImplement, run.FailureA, "agent crashed"))
		repo.run.State = st

		_, err := run.ReviveRun(context.Background(), repo, repo.run.ID)
		if !errors.Is(err, run.ErrReviveNotApplicable) {
			t.Errorf("state %q: err = %v, want ErrReviveNotApplicable", st, err)
		}
		// No stage was re-parked.
		if repo.stages[0].State != run.StageStateFailed {
			t.Errorf("state %q: stage mutated to %q despite refusal", st, repo.stages[0].State)
		}
	}
}

// m9: a failed run with zero failed stages refuses (nothing to re-park).
func TestReviveRun_RejectsNoFailedStages(t *testing.T) {
	repo := reviveFailedRun(reviveStage(uuid.Nil, 0, run.StageTypeImplement, run.FailureA, "agent crashed"))
	repo.stages[0].State = run.StageStateSucceeded
	repo.stages[0].FailureCategory = nil
	repo.stages[0].FailureReason = nil

	_, err := run.ReviveRun(context.Background(), repo, repo.run.ID)
	if !errors.Is(err, run.ErrReviveNotApplicable) {
		t.Fatalf("err = %v, want ErrReviveNotApplicable", err)
	}
}

// m10: a category-B failed stage refuses the WHOLE revive with NO partial
// mutation — a sibling failed-A stage is still failed afterward, and the
// run is still failed.
func TestReviveRun_RejectsCategoryBNoPartialMutation(t *testing.T) {
	stageA := reviveStage(uuid.Nil, 0, run.StageTypeImplement, run.FailureA, "agent crashed")
	stageB := reviveStage(uuid.Nil, 1, run.StageTypeReview, run.FailureB, "constraint violation")
	repo := reviveFailedRun(stageA, stageB)

	_, err := run.ReviveRun(context.Background(), repo, repo.run.ID)
	if !errors.Is(err, run.ErrReviveNotApplicable) {
		t.Fatalf("err = %v, want ErrReviveNotApplicable", err)
	}
	// No partial mutation: the retryable sibling was NOT re-parked.
	if stageA.State != run.StageStateFailed {
		t.Errorf("sibling A stage = %q, want failed (revive must not partially re-park)", stageA.State)
	}
	if stageA.SelfRetryCount != 0 {
		t.Errorf("sibling A stage SelfRetryCount = %d, want 0 (no budget consumed on refusal)", stageA.SelfRetryCount)
	}
	if repo.run.State != run.StateFailed {
		t.Errorf("run = %q, want failed (revive must not reopen on refusal)", repo.run.State)
	}
}

// m10 variant: a failed stage with NO recorded category refuses (its
// retryability cannot be confirmed).
func TestReviveRun_RejectsUncategorizedFailedStage(t *testing.T) {
	stage := reviveStage(uuid.Nil, 0, run.StageTypeImplement, run.FailureA, "agent crashed")
	stage.FailureCategory = nil
	repo := reviveFailedRun(stage)

	_, err := run.ReviveRun(context.Background(), repo, repo.run.ID)
	if !errors.Is(err, run.ErrReviveNotApplicable) {
		t.Fatalf("err = %v, want ErrReviveNotApplicable", err)
	}
}

// m11: a D-rejected (approver said no) failed stage refuses — only the D
// SLA-timeout sub-reason is retryable.
func TestReviveRun_RejectsDRejected(t *testing.T) {
	repo := reviveFailedRun(reviveStage(uuid.Nil, 0, run.StageTypeReview, run.FailureD, "gate rejected by approver"))

	_, err := run.ReviveRun(context.Background(), repo, repo.run.ID)
	if !errors.Is(err, run.ErrReviveNotApplicable) {
		t.Fatalf("err = %v, want ErrReviveNotApplicable", err)
	}
	if repo.stages[0].State != run.StageStateFailed {
		t.Errorf("stage = %q, want failed (revive must not re-park a D-rejected stage)", repo.stages[0].State)
	}
}

// m12: an A/C failed stage restores to pending, and (m15) the run flips
// failed → running.
func TestReviveRun_RestoresACToPending(t *testing.T) {
	for _, cat := range []run.FailureCategory{run.FailureA, run.FailureC} {
		repo := reviveFailedRun(reviveStage(uuid.Nil, 0, run.StageTypeImplement, cat, "boom"))

		dec, err := run.ReviveRun(context.Background(), repo, repo.run.ID)
		if err != nil {
			t.Fatalf("cat %s: ReviveRun: %v", cat, err)
		}
		if len(dec.Stages) != 1 {
			t.Fatalf("cat %s: restored %d stages, want 1", cat, len(dec.Stages))
		}
		if dec.Stages[0].RestoredState != run.StageStatePending {
			t.Errorf("cat %s: restored state = %q, want pending", cat, dec.Stages[0].RestoredState)
		}
		if dec.Stages[0].PriorCategory != cat {
			t.Errorf("cat %s: prior category = %q", cat, dec.Stages[0].PriorCategory)
		}
		// m15: the run flips failed → running.
		if dec.Run.State != run.StateRunning {
			t.Errorf("cat %s: run = %q, want running", cat, dec.Run.State)
		}
		// The stage's failure metadata is cleared and retry budget consumed.
		if repo.stages[0].FailureCategory != nil {
			t.Errorf("cat %s: stage still carries failure metadata", cat)
		}
		if repo.stages[0].SelfRetryCount != 1 {
			t.Errorf("cat %s: SelfRetryCount = %d, want 1 (revive consumes per-stage retry budget)", cat, repo.stages[0].SelfRetryCount)
		}
	}
}

// m13: a D SLA-timeout failed stage restores to awaiting_approval (the gate
// re-opens without a re-dispatch).
func TestReviveRun_RestoresDTimeoutToAwaitingApproval(t *testing.T) {
	repo := reviveFailedRun(reviveStage(uuid.Nil, 0, run.StageTypeReview, run.FailureD, "sla_timeout: 5h elapsed (deadline 4h)"))

	dec, err := run.ReviveRun(context.Background(), repo, repo.run.ID)
	if err != nil {
		t.Fatalf("ReviveRun: %v", err)
	}
	if dec.Stages[0].RestoredState != run.StageStateAwaitingApproval {
		t.Errorf("restored state = %q, want awaiting_approval", dec.Stages[0].RestoredState)
	}
	if dec.Run.State != run.StateRunning {
		t.Errorf("run = %q, want running", dec.Run.State)
	}
}

// m14: a failed implement stage on a decomposition PARENT restores to
// awaiting_children (#1891) — revive must NOT re-open it to pending and
// spawn a doomed runner.
func TestReviveRun_RestoresDecomposedParentToAwaitingChildren(t *testing.T) {
	repo := reviveFailedRun(reviveStage(uuid.Nil, 0, run.StageTypeImplement, run.FailureA, "child fan-out failed"))
	repo.hasChildren = true // ListRuns reports a child → decomposed parent

	dec, err := run.ReviveRun(context.Background(), repo, repo.run.ID)
	if err != nil {
		t.Fatalf("ReviveRun: %v", err)
	}
	if dec.Stages[0].RestoredState != run.StageStateAwaitingChildren {
		t.Errorf("restored state = %q, want awaiting_children (decomposed-parent restore, #1891)", dec.Stages[0].RestoredState)
	}
	if dec.Run.State != run.StateRunning {
		t.Errorf("run = %q, want running", dec.Run.State)
	}
}

// A multi-stage revive re-parks every failed stage, ordered by sequence,
// and leaves non-failed stages untouched.
func TestReviveRun_ReparksEveryFailedStageOrdered(t *testing.T) {
	implement := reviveStage(uuid.Nil, 1, run.StageTypeImplement, run.FailureC, "runner OOM")
	review := reviveStage(uuid.Nil, 2, run.StageTypeReview, run.FailureD, "sla_timeout: elapsed")
	repo := reviveFailedRun(implement, review)
	// A succeeded plan stage that must be left alone.
	plan := reviveStage(repo.run.ID, 0, run.StageTypePlan, run.FailureA, "n/a")
	plan.State = run.StageStateSucceeded
	plan.FailureCategory = nil
	plan.FailureReason = nil
	repo.stages = append([]*run.Stage{plan}, repo.stages...)

	dec, err := run.ReviveRun(context.Background(), repo, repo.run.ID)
	if err != nil {
		t.Fatalf("ReviveRun: %v", err)
	}
	if len(dec.Stages) != 2 {
		t.Fatalf("restored %d stages, want 2 (plan is succeeded, untouched)", len(dec.Stages))
	}
	// Ordered by sequence: implement (seq 1) then review (seq 2).
	if dec.Stages[0].StageType != run.StageTypeImplement || dec.Stages[1].StageType != run.StageTypeReview {
		t.Errorf("restore order = [%s, %s], want [implement, review]", dec.Stages[0].StageType, dec.Stages[1].StageType)
	}
	if dec.Stages[0].RestoredState != run.StageStatePending {
		t.Errorf("implement restored to %q, want pending", dec.Stages[0].RestoredState)
	}
	if dec.Stages[1].RestoredState != run.StageStateAwaitingApproval {
		t.Errorf("review restored to %q, want awaiting_approval", dec.Stages[1].RestoredState)
	}
}

// TestReviveRun_MidBatchRetryStageFailureIsResumable pins the mid-batch
// RetryStage-failure branch (#1942): the first stage re-parks, the second
// stage's RetryStage fails, so the run is left partially re-parked. The
// error carries the resume hint, and a SECOND ReviveRun re-parks only the
// remaining failed stage without double-consuming the first stage's retry
// budget.
func TestReviveRun_MidBatchRetryStageFailureIsResumable(t *testing.T) {
	implement := reviveStage(uuid.Nil, 1, run.StageTypeImplement, run.FailureA, "agent crashed")
	review := reviveStage(uuid.Nil, 2, run.StageTypeReview, run.FailureD, "sla_timeout: 5h elapsed")
	repo := reviveFailedRun(implement, review)

	injected := errors.New("concurrent transition on review stage")
	repo.retryStageErr = map[uuid.UUID]error{review.ID: injected}

	_, err := run.ReviveRun(context.Background(), repo, repo.run.ID)
	if !errors.Is(err, injected) {
		t.Fatalf("err = %v, want wrapped injected error", err)
	}
	if !strings.Contains(err.Error(), "second revive resumes") {
		t.Errorf("err = %q, want the partial-re-park resume hint", err)
	}
	// Partial state: the first stage is re-parked (pending, budget consumed);
	// the second stage is still failed; the run is still failed.
	if implement.State != run.StageStatePending {
		t.Errorf("implement stage = %q, want pending (first stage re-parked)", implement.State)
	}
	if implement.SelfRetryCount != 1 {
		t.Errorf("implement SelfRetryCount = %d, want 1", implement.SelfRetryCount)
	}
	if review.State != run.StageStateFailed {
		t.Errorf("review stage = %q, want failed (its re-park failed)", review.State)
	}
	if repo.run.State != run.StateFailed {
		t.Errorf("run = %q, want failed (reopen not reached)", repo.run.State)
	}

	// A second revive clears the injection and resumes: only the remaining
	// failed review stage is re-parked, the run flips to running, and the
	// already-re-parked implement stage's budget is NOT bumped again.
	repo.retryStageErr = nil
	dec, err := run.ReviveRun(context.Background(), repo, repo.run.ID)
	if err != nil {
		t.Fatalf("second ReviveRun: %v", err)
	}
	if dec.Resumed {
		t.Errorf("Resumed = true, want false (this call performed a fresh re-park)")
	}
	if len(dec.Stages) != 1 {
		t.Fatalf("restored %d stages, want 1 (only the remaining failed review)", len(dec.Stages))
	}
	if dec.Stages[0].StageType != run.StageTypeReview {
		t.Errorf("restored stage = %s, want review", dec.Stages[0].StageType)
	}
	if dec.Stages[0].RestoredState != run.StageStateAwaitingApproval {
		t.Errorf("review restored to %q, want awaiting_approval", dec.Stages[0].RestoredState)
	}
	if dec.Run.State != run.StateRunning {
		t.Errorf("run = %q, want running", dec.Run.State)
	}
	if implement.SelfRetryCount != 1 {
		t.Errorf("implement SelfRetryCount = %d, want STILL 1 (resume must not re-consume an already-re-parked stage's budget)", implement.SelfRetryCount)
	}
}

// TestReviveRun_RetryRunTailFailureThenResume pins the tail RetryRun-failure
// branch and its resume (#1942): every failed stage re-parks but the closing
// RetryRun fails, leaving the run failed with zero failed stages. A second
// ReviveRun takes the interrupted-revive resume branch — Resumed true, empty
// Stages, run running — without bumping any stage's retry budget again.
func TestReviveRun_RetryRunTailFailureThenResume(t *testing.T) {
	implement := reviveStage(uuid.Nil, 1, run.StageTypeImplement, run.FailureA, "agent crashed")
	review := reviveStage(uuid.Nil, 2, run.StageTypeReview, run.FailureD, "sla_timeout: elapsed")
	repo := reviveFailedRun(implement, review)

	injected := errors.New("run reopen lost the row lock")
	repo.retryRunErr = injected

	_, err := run.ReviveRun(context.Background(), repo, repo.run.ID)
	if !errors.Is(err, injected) {
		t.Fatalf("err = %v, want wrapped injected error", err)
	}
	if !strings.Contains(err.Error(), "second revive completes the reopen") {
		t.Errorf("err = %q, want the tail-failure completion hint", err)
	}
	// Every failed stage is re-parked, but the run is still failed.
	if implement.State != run.StageStatePending {
		t.Errorf("implement stage = %q, want pending", implement.State)
	}
	if review.State != run.StageStateAwaitingApproval {
		t.Errorf("review stage = %q, want awaiting_approval", review.State)
	}
	if repo.run.State != run.StateFailed {
		t.Errorf("run = %q, want failed (reopen failed)", repo.run.State)
	}

	// Resume: zero failed stages remain, but a stage sits in a pre-dispatch
	// park state, so the resume branch completes the reopen.
	repo.retryRunErr = nil
	dec, err := run.ReviveRun(context.Background(), repo, repo.run.ID)
	if err != nil {
		t.Fatalf("resume ReviveRun: %v", err)
	}
	if !dec.Resumed {
		t.Errorf("Resumed = false, want true (this call completed an interrupted revive)")
	}
	if len(dec.Stages) != 0 {
		t.Errorf("restored %d stages, want 0 (resume re-parks nothing)", len(dec.Stages))
	}
	if dec.Run.State != run.StateRunning {
		t.Errorf("run = %q, want running", dec.Run.State)
	}
	if implement.SelfRetryCount != 1 || review.SelfRetryCount != 1 {
		t.Errorf("SelfRetryCount = (%d, %d), want (1, 1) — resume must not bump budget a second time",
			implement.SelfRetryCount, review.SelfRetryCount)
	}
}

// TestReviveRun_ResumeRefusesWithoutPreDispatchParkedStage proves the resume
// branch cannot reopen an ARBITRARY inconsistent run: a failed run with zero
// failed stages and NO stage in a pre-dispatch park state still refuses with
// ErrReviveNotApplicable (#1942).
func TestReviveRun_ResumeRefusesWithoutPreDispatchParkedStage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state run.StageState
	}{
		{"all succeeded", run.StageStateSucceeded},
		{"a running stage", run.StageStateRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage := reviveStage(uuid.Nil, 0, run.StageTypeImplement, run.FailureA, "n/a")
			stage.State = tc.state
			stage.FailureCategory = nil
			stage.FailureReason = nil
			repo := reviveFailedRun(stage)

			_, err := run.ReviveRun(context.Background(), repo, repo.run.ID)
			if !errors.Is(err, run.ErrReviveNotApplicable) {
				t.Fatalf("err = %v, want ErrReviveNotApplicable", err)
			}
			// The run must not have been reopened.
			if repo.run.State != run.StateFailed {
				t.Errorf("run = %q, want failed (resume branch must not reopen)", repo.run.State)
			}
		})
	}
}

// A missing run surfaces ErrNotFound (not ErrReviveNotApplicable).
func TestReviveRun_NotFound(t *testing.T) {
	repo := reviveFailedRun(reviveStage(uuid.Nil, 0, run.StageTypeImplement, run.FailureA, "boom"))

	_, err := run.ReviveRun(context.Background(), repo, uuid.New())
	if !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
