package run_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func TestFailureCategoryValid(t *testing.T) {
	cases := []struct {
		in   run.FailureCategory
		want bool
	}{
		{run.FailureA, true},
		{run.FailureB, true},
		{run.FailureC, true},
		{run.FailureD, true},
		{"", false},
		{"E", false},
		{"a", false}, // case-sensitive on purpose; canonical forms are uppercase
	}
	for _, tc := range cases {
		if got := tc.in.Valid(); got != tc.want {
			t.Errorf("(%q).Valid() = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestFailureCategoryDescription(t *testing.T) {
	// Each canonical category gets a distinct, non-empty description.
	// The frontend mirrors these labels; if the tests pass with
	// duplicates or empties the agreement breaks silently.
	descriptions := map[run.FailureCategory]string{}
	for _, c := range []run.FailureCategory{run.FailureA, run.FailureB, run.FailureC, run.FailureD} {
		got := c.Description()
		if got == "" {
			t.Errorf("(%q).Description() empty", c)
		}
		if strings.Contains(got, "<") || strings.Contains(got, ">") {
			t.Errorf("(%q).Description() = %q, looks templated", c, got)
		}
		descriptions[c] = got
	}
	if len(descriptions) != 4 {
		t.Fatalf("expected four distinct entries, got %v", descriptions)
	}
	seen := map[string]run.FailureCategory{}
	for c, d := range descriptions {
		if dup, ok := seen[d]; ok {
			t.Errorf("descriptions collide: %q for %q and %q", d, dup, c)
		}
		seen[d] = c
	}
}

func TestFailureCategoryDescriptionUnknownPassesThrough(t *testing.T) {
	if got := run.FailureCategory("Z").Description(); got != "Z" {
		t.Errorf("unknown.Description() = %q, want pass-through %q", got, "Z")
	}
}

// memRepo is a minimal in-memory Repository sufficient for FailStage's
// surface (GetStage + TransitionStage). The real postgres adapter
// has integration coverage in postgres_test.go; these tests focus on
// the helper's branching logic.
type memRepo struct {
	mu     sync.Mutex
	stages map[uuid.UUID]*run.Stage
	// transitionErr forces TransitionStage to fail for a specific stage
	// id, letting tests model a re-park failure mid-fix-up (#780).
	transitionErr map[uuid.UUID]error
}

func newMemRepo(s ...*run.Stage) *memRepo {
	m := &memRepo{stages: map[uuid.UUID]*run.Stage{}, transitionErr: map[uuid.UUID]error{}}
	for _, st := range s {
		m.stages[st.ID] = st
	}
	return m
}

// failTransition arms TransitionStage to return err for the given
// stage id — used to assert the fix-up re-park ordering leaves the
// implement stage untouched on a re-park failure (#780).
func (m *memRepo) failTransition(id uuid.UUID, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.transitionErr[id] = err
}

func (m *memRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.stages[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	cp := *s
	return &cp, nil
}

func (m *memRepo) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.transitionErr[id]; err != nil {
		return nil, err
	}
	s, ok := m.stages[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	// Mirror postgresRepo's same-state short-circuit: idempotent
	// re-application returns the current row unchanged. Without this
	// a same-state no-op would silently overwrite the original
	// FailureCategory / FailureReason.
	if s.State == to {
		cp := *s
		return &cp, nil
	}
	// Mirror postgresRepo: admit the fix-up override edges
	// (awaiting_approval → pending and succeeded → pending, #762/#780)
	// AND the fix-up RECOVERY edges (failed → succeeded/awaiting_approval,
	// review pending → awaiting_approval, #788) in addition to the normal
	// transitions so FixupStage / RestoreFixupStage can reuse
	// TransitionStage.
	if !run.ValidStageTransition(s.State, to) &&
		!run.ValidStageFixupTransition(s.State, to) &&
		!run.ValidStageFixupRecoveryTransition(s.State, to) {
		return nil, run.InvalidTransitionError{Kind: "stage", From: string(s.State), To: string(to)}
	}
	s.State = to
	// Mirror postgresRepo's UpdateStageState, which sets
	// failure_category/failure_reason directly (not COALESCE): a nil
	// completion clears the stale failure metadata to SQL NULL. This is
	// what lets a recovery transition (failed → succeeded, nil) un-fail
	// the stage AND drop its prior FailureCategory/FailureReason (#788).
	if c != nil {
		s.FailureCategory = c.FailureCategory
		s.FailureReason = c.FailureReason
	} else {
		s.FailureCategory = nil
		s.FailureReason = nil
	}
	// Mirror postgresRepo's ended_at handling: stamp it when entering a
	// terminal state, NULL it otherwise (a re-open to a non-terminal
	// target clears the terminal timestamp). Keeps the fixture's
	// succeeded → pending fix-up semantics aligned with Postgres (#780).
	if to.IsTerminal() {
		now := time.Now().UTC()
		s.EndedAt = &now
	} else {
		s.EndedAt = nil
	}
	cp := *s
	return &cp, nil
}

// Pad the rest of run.Repository so memRepo satisfies the interface.
func (m *memRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (m *memRepo) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (m *memRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}
func (m *memRepo) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("not used")
}
func (m *memRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (m *memRepo) RetryRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (m *memRepo) SetRunPullRequestURL(context.Context, uuid.UUID, string) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (m *memRepo) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}

// ListStagesForRun returns every stage sharing the queried RunID, so
// FixupStage's push_and_open_pr applicability check can locate the
// run's review stage (#780). Returns copies to mirror GetStage.
func (m *memRepo) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*run.Stage
	for _, s := range m.stages {
		if s.RunID == runID {
			cp := *s
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (m *memRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (m *memRepo) ListReviewStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (m *memRepo) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (m *memRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

// RetryStage validates the retry-only transition table and clears
// the stage's failure metadata, mirroring postgresRepo's behaviour.
// retry_test.go's RetryStage helper tests rely on this happening
// in-memory.
func (m *memRepo) RetryStage(_ context.Context, id uuid.UUID, to run.StageState) (*run.Stage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.stages[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	if !run.ValidStageRetryTransition(s.State, to) {
		return nil, run.InvalidTransitionError{Kind: "stage", From: string(s.State), To: string(to)}
	}
	s.State = to
	s.FailureCategory = nil
	s.FailureReason = nil
	s.EndedAt = nil
	cp := *s
	return &cp, nil
}

func newStage(state run.StageState) *run.Stage {
	now := time.Now().UTC()
	return &run.Stage{
		ID:           uuid.New(),
		RunID:        uuid.New(),
		Sequence:     0,
		Type:         run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "claude-code",
		State:        state,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

func TestFailStageFromAwaitingApproval(t *testing.T) {
	stage := newStage(run.StageStateAwaitingApproval)
	repo := newMemRepo(stage)

	got, err := run.FailStage(context.Background(), repo, stage.ID, run.FailureD, "rejected")
	if err != nil {
		t.Fatalf("FailStage: %v", err)
	}
	if got.State != run.StageStateFailed {
		t.Errorf("State = %q, want failed", got.State)
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureD {
		t.Errorf("FailureCategory = %v, want D", got.FailureCategory)
	}
	if got.FailureReason == nil || *got.FailureReason != "rejected" {
		t.Errorf("FailureReason = %v, want rejected", got.FailureReason)
	}
}

func TestFailStageFromRunning(t *testing.T) {
	stage := newStage(run.StageStateRunning)
	repo := newMemRepo(stage)

	got, err := run.FailStage(context.Background(), repo, stage.ID, run.FailureB, "policy")
	if err != nil {
		t.Fatalf("FailStage: %v", err)
	}
	if got.State != run.StageStateFailed {
		t.Errorf("State = %q, want failed", got.State)
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureB {
		t.Errorf("FailureCategory = %v, want B", got.FailureCategory)
	}
}

func TestFailStageFromDispatchedWalksThroughRunning(t *testing.T) {
	stage := newStage(run.StageStateDispatched)
	repo := newMemRepo(stage)

	got, err := run.FailStage(context.Background(), repo, stage.ID, run.FailureC, "infra")
	if err != nil {
		t.Fatalf("FailStage: %v", err)
	}
	if got.State != run.StageStateFailed {
		t.Errorf("State = %q, want failed", got.State)
	}
	// Confirm the intermediate transition actually happened by
	// checking that direct dispatched → failed (which the state
	// machine forbids) didn't get applied as a fallback.
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureC {
		t.Errorf("FailureCategory = %v, want C", got.FailureCategory)
	}
}

func TestFailStageRejectsInvalidCategory(t *testing.T) {
	stage := newStage(run.StageStateRunning)
	repo := newMemRepo(stage)

	_, err := run.FailStage(context.Background(), repo, stage.ID, run.FailureCategory("Z"), "nope")
	if err == nil {
		t.Fatal("FailStage with invalid category returned nil error")
	}
	if !strings.Contains(err.Error(), "invalid category") {
		t.Errorf("error = %v, want contains 'invalid category'", err)
	}
	// Stage state must not have changed.
	current, _ := repo.GetStage(context.Background(), stage.ID)
	if current.State != run.StageStateRunning {
		t.Errorf("stage state = %q, want unchanged (running)", current.State)
	}
}

// FailStage on an already-failed stage is idempotent — the original
// category and reason persist; the second call's category/reason
// are dropped. This matches postgresRepo's same-state short-circuit
// and protects the audit chain: the *first* failure cause stays the
// canonical one.
func TestFailStageOnAlreadyFailedIsIdempotent(t *testing.T) {
	originalCat := run.FailureB
	originalReason := "policy violation"
	stage := newStage(run.StageStateFailed)
	stage.FailureCategory = &originalCat
	stage.FailureReason = &originalReason
	repo := newMemRepo(stage)

	got, err := run.FailStage(context.Background(), repo, stage.ID, run.FailureD, "double-fail")
	if err != nil {
		t.Fatalf("FailStage idempotent re-apply: %v", err)
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureB {
		t.Errorf("FailureCategory = %v, want B (original preserved)", got.FailureCategory)
	}
	if got.FailureReason == nil || *got.FailureReason != "policy violation" {
		t.Errorf("FailureReason = %v, want original preserved", got.FailureReason)
	}
}

// FailStage on a non-failed terminal state (succeeded, cancelled)
// is a real conflict and must error. The state machine rejects
// these transitions at the repo layer; the helper surfaces the
// resulting InvalidTransitionError to the caller.
func TestFailStageOnSucceededIsRejected(t *testing.T) {
	stage := newStage(run.StageStateSucceeded)
	repo := newMemRepo(stage)

	_, err := run.FailStage(context.Background(), repo, stage.ID, run.FailureD, "too late")
	if err == nil {
		t.Fatal("FailStage on succeeded stage returned nil error")
	}
}

// Mode 1 (#1903): a stage already in awaiting_children at load time is a
// live decomposition fan-in park owned by its child slices. FailStage must
// REFUSE it up-front with the ErrStageParked sentinel and attempt no
// transition — leaving the park's state and (nil) failure metadata
// untouched — because failing it would take the legal awaiting_children →
// failed edge and destroy the park. This holds on the non-CAS memRepo, so
// the refusal is not merely the CAS path's doing.
func TestFailStageRefusesAwaitingChildrenPark(t *testing.T) {
	stage := newStage(run.StageStateAwaitingChildren)
	repo := newMemRepo(stage)

	_, err := run.FailStage(context.Background(), repo, stage.ID, run.FailureC, "doomed spawn")
	if err == nil {
		t.Fatal("FailStage on an awaiting_children park returned nil error")
	}
	if !errors.Is(err, run.ErrStageParked) {
		t.Errorf("error = %v, want wrapping ErrStageParked", err)
	}
	// The park is intact: state unchanged, no failure metadata stamped.
	cur, _ := repo.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateAwaitingChildren {
		t.Errorf("stage state = %q, want awaiting_children (park preserved)", cur.State)
	}
	if cur.FailureCategory != nil || cur.FailureReason != nil {
		t.Errorf("failure metadata stamped on a refused park: cat=%v reason=%v",
			cur.FailureCategory, cur.FailureReason)
	}
}

// casMemRepo augments memRepo with the StageCASTransitioner capability so
// FailStage exercises its compare-and-swap path (as it does against the
// production postgresRepo). beforeTransitionFrom, when set, runs just before
// each TransitionStageFrom evaluates its expected-vs-current check, letting a
// test flip the stage state to model a park landing mid-flight.
type casMemRepo struct {
	*memRepo
	beforeTransitionFrom func(id uuid.UUID)
}

func (m *casMemRepo) TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if m.beforeTransitionFrom != nil {
		m.beforeTransitionFrom(id)
	}
	m.mu.Lock()
	s, ok := m.stages[id]
	if !ok {
		m.mu.Unlock()
		return nil, run.ErrNotFound
	}
	// Compare-and-swap under the (mutex) lock: refuse atomically if the
	// current state drifted from the caller's expected from-state, mirroring
	// postgresRepo.TransitionStageFrom's row-locked check.
	if s.State != from {
		cur := s.State
		m.mu.Unlock()
		return nil, run.StageStateChangedError{StageID: id, Expected: from, Actual: cur}
	}
	m.mu.Unlock()
	// Delegate the actual mutation + completion/ended_at semantics to the
	// embedded memRepo's TransitionStage (which re-locks internally).
	return m.TransitionStage(ctx, id, to, c)
}

// Mode 2 (#1903): a park landing AFTER FailStage's load — the residual
// TOCTOU. The stage is loaded as pending; the CAS hook flips it to
// awaiting_children before the (pending → failed) TransitionStageFrom
// evaluates, so the compare-and-swap refuses with StageStateChangedError and
// the park survives instead of being destroyed.
func TestFailStageCASRefusesMidFlightPark(t *testing.T) {
	stage := newStage(run.StageStatePending)
	base := newMemRepo(stage)
	var flipped bool
	repo := &casMemRepo{memRepo: base, beforeTransitionFrom: func(id uuid.UUID) {
		if flipped || id != stage.ID {
			return
		}
		flipped = true
		base.mu.Lock()
		base.stages[id].State = run.StageStateAwaitingChildren
		base.mu.Unlock()
	}}

	_, err := run.FailStage(context.Background(), repo, stage.ID, run.FailureC, "raced by a fanout park")
	if err == nil {
		t.Fatal("FailStage returned nil error despite a mid-flight park")
	}
	var sce run.StageStateChangedError
	if !errors.As(err, &sce) {
		t.Fatalf("error = %v, want StageStateChangedError via errors.As", err)
	}
	if sce.Expected != run.StageStatePending || sce.Actual != run.StageStateAwaitingChildren {
		t.Errorf("StageStateChangedError = {expected:%q actual:%q}, want {pending awaiting_children}",
			sce.Expected, sce.Actual)
	}
	// The park survived and was never stamped failed.
	cur, _ := repo.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateAwaitingChildren {
		t.Errorf("stage state = %q, want awaiting_children (park preserved)", cur.State)
	}
	if cur.FailureCategory != nil {
		t.Errorf("failure metadata stamped on a refused park: %v", cur.FailureCategory)
	}
}

// Mode 3 (#1903): the CAS happy path, including the dispatched → running →
// failed walk. Proves failStageCAS re-anchors its expected from-state per
// step (dispatched for the first CAS, then the produced running state for the
// final CAS), landing failed with the right category — so the CAS path is not
// merely a refusal mechanism.
func TestFailStageCASHappyPathWalksDispatched(t *testing.T) {
	stage := newStage(run.StageStateDispatched)
	base := newMemRepo(stage)
	repo := &casMemRepo{memRepo: base}

	got, err := run.FailStage(context.Background(), repo, stage.ID, run.FailureC, "infra")
	if err != nil {
		t.Fatalf("FailStage: %v", err)
	}
	if got.State != run.StageStateFailed {
		t.Errorf("state = %q, want failed", got.State)
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureC {
		t.Errorf("failure category = %v, want C", got.FailureCategory)
	}
	if got.EndedAt == nil {
		t.Error("ended_at not stamped on terminal transition")
	}
}

// setState is a small helper to flip a seeded stage's state under the
// memRepo lock, used by the re-anchor-loop tests' CAS hooks.
func (m *memRepo) setState(id uuid.UUID, to run.StageState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stages[id].State = to
}

// #1907 mode (i): a benign concurrent ADVANCE from dispatched to running,
// landing before the first CAS. The re-anchor loop must ABSORB it — re-anchor
// at running and drive on to failed — rather than surfacing the refusal, so
// the stage lands failed with the caller's category/reason stamped exactly
// once (only the single successful terminal transition writes completion).
func TestFailStageCASAbsorbsDispatchedRunningFlip(t *testing.T) {
	stage := newStage(run.StageStateDispatched)
	base := newMemRepo(stage)
	var flipped bool
	repo := &casMemRepo{memRepo: base, beforeTransitionFrom: func(id uuid.UUID) {
		if flipped || id != stage.ID {
			return
		}
		flipped = true
		base.setState(id, run.StageStateRunning) // a concurrent writer advanced it
	}}

	got, err := run.FailStage(context.Background(), repo, stage.ID, run.FailureC, "infra")
	if err != nil {
		t.Fatalf("FailStage: %v", err)
	}
	if got.State != run.StageStateFailed {
		t.Errorf("state = %q, want failed (advance absorbed)", got.State)
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureC {
		t.Errorf("failure category = %v, want C stamped exactly once", got.FailureCategory)
	}
	if got.FailureReason == nil || *got.FailureReason != "infra" {
		t.Errorf("failure reason = %v, want the caller's reason", got.FailureReason)
	}
}

// #1907 mode (ii): a benign concurrent ADVANCE from running to
// awaiting_approval, landing before the final CAS. The re-anchor loop must
// absorb it via the legal awaiting_approval → failed edge.
func TestFailStageCASAbsorbsAwaitingApprovalFlip(t *testing.T) {
	stage := newStage(run.StageStateRunning)
	base := newMemRepo(stage)
	var flipped bool
	repo := &casMemRepo{memRepo: base, beforeTransitionFrom: func(id uuid.UUID) {
		if flipped || id != stage.ID {
			return
		}
		flipped = true
		base.setState(id, run.StageStateAwaitingApproval) // gate opened concurrently
	}}

	got, err := run.FailStage(context.Background(), repo, stage.ID, run.FailureD, "sla elapsed")
	if err != nil {
		t.Fatalf("FailStage: %v", err)
	}
	if got.State != run.StageStateFailed {
		t.Errorf("state = %q, want failed (advance absorbed via awaiting_approval → failed)", got.State)
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureD {
		t.Errorf("failure category = %v, want D", got.FailureCategory)
	}
}

// #1907 mode (iii): a concurrent writer settled the stage TERMINAL mid-flight.
// The re-anchor loop must NOT retry — it returns the typed
// StageStateChangedError (Actual=failed) unchanged, and the winner's
// completion metadata is left untouched.
func TestFailStageCASRefusesTerminalFlip(t *testing.T) {
	stage := newStage(run.StageStateRunning)
	base := newMemRepo(stage)
	winnerCat := run.FailureB
	winnerReason := "winner settled first"
	var flipped bool
	repo := &casMemRepo{memRepo: base, beforeTransitionFrom: func(id uuid.UUID) {
		if flipped || id != stage.ID {
			return
		}
		flipped = true
		base.mu.Lock()
		base.stages[id].State = run.StageStateFailed
		base.stages[id].FailureCategory = &winnerCat
		base.stages[id].FailureReason = &winnerReason
		base.mu.Unlock()
	}}

	_, err := run.FailStage(context.Background(), repo, stage.ID, run.FailureC, "loser")
	if err == nil {
		t.Fatal("FailStage returned nil error despite a terminal flip")
	}
	var sce run.StageStateChangedError
	if !errors.As(err, &sce) {
		t.Fatalf("error = %v, want StageStateChangedError via errors.As", err)
	}
	if sce.Actual != run.StageStateFailed {
		t.Errorf("StageStateChangedError.Actual = %q, want failed", sce.Actual)
	}
	// The winner's completion metadata is untouched by the loser.
	cur, _ := repo.GetStage(context.Background(), stage.ID)
	if cur.FailureCategory == nil || *cur.FailureCategory != run.FailureB {
		t.Errorf("failure category = %v, want B (winner's, untouched)", cur.FailureCategory)
	}
	if cur.FailureReason == nil || *cur.FailureReason != winnerReason {
		t.Errorf("failure reason = %v, want the winner's (untouched)", cur.FailureReason)
	}
}

// #1907 mode (iv): the park invariant must survive a RE-ANCHOR, not just the
// first attempt. The hook flips dispatched → running before the first CAS (a
// benign advance the loop absorbs by re-anchoring), then flips running →
// awaiting_children before the retry's CAS. FailStage must refuse the park
// (StageStateChangedError, Actual=awaiting_children) with no failure metadata
// stamped — proving a park landing on ANY attempt is never collapsed.
func TestFailStageCASRefusesParkAfterReanchor(t *testing.T) {
	stage := newStage(run.StageStateDispatched)
	base := newMemRepo(stage)
	var calls int
	repo := &casMemRepo{memRepo: base, beforeTransitionFrom: func(id uuid.UUID) {
		if id != stage.ID {
			return
		}
		calls++
		switch calls {
		case 1:
			base.setState(id, run.StageStateRunning) // benign advance, absorbed
		case 2:
			base.setState(id, run.StageStateAwaitingChildren) // park lands on the retry
		}
	}}

	_, err := run.FailStage(context.Background(), repo, stage.ID, run.FailureC, "raced by a fanout park after re-anchor")
	if err == nil {
		t.Fatal("FailStage returned nil error despite a park landing after re-anchor")
	}
	var sce run.StageStateChangedError
	if !errors.As(err, &sce) {
		t.Fatalf("error = %v, want StageStateChangedError via errors.As", err)
	}
	if sce.Actual != run.StageStateAwaitingChildren {
		t.Errorf("StageStateChangedError.Actual = %q, want awaiting_children", sce.Actual)
	}
	cur, _ := repo.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateAwaitingChildren {
		t.Errorf("stage state = %q, want awaiting_children (park preserved)", cur.State)
	}
	if cur.FailureCategory != nil || cur.FailureReason != nil {
		t.Errorf("failure metadata stamped on a refused park: cat=%v reason=%v",
			cur.FailureCategory, cur.FailureReason)
	}
}

// #1907 mode (v): pathological livelock. The hook alternates the stage between
// two live states before every CAS so no attempt ever succeeds. FailStage must
// return StageStateChangedError after exactly failStageCASMaxAttempts CAS
// calls — the asserted call count pins the bound so a future off-by-one or an
// unbounded loop regresses loudly.
func TestFailStageCASExhaustsRetries(t *testing.T) {
	stage := newStage(run.StageStateRunning)
	base := newMemRepo(stage)
	var calls int
	repo := &casMemRepo{memRepo: base, beforeTransitionFrom: func(id uuid.UUID) {
		if id != stage.ID {
			return
		}
		calls++
		base.mu.Lock()
		if base.stages[id].State == run.StageStateRunning {
			base.stages[id].State = run.StageStateAwaitingApproval
		} else {
			base.stages[id].State = run.StageStateRunning
		}
		base.mu.Unlock()
	}}

	_, err := run.FailStage(context.Background(), repo, stage.ID, run.FailureC, "livelock")
	if err == nil {
		t.Fatal("FailStage returned nil error despite perpetual livelock")
	}
	var sce run.StageStateChangedError
	if !errors.As(err, &sce) {
		t.Fatalf("error = %v, want StageStateChangedError via errors.As", err)
	}
	// The bound: one CAS call per attempt (no dispatched pre-step from a
	// running anchor), so the hook fires exactly maxAttempts times.
	if calls != 4 {
		t.Errorf("CAS attempts = %d, want 4 (failStageCASMaxAttempts)", calls)
	}
	// The stage is left in a live state — never failed under livelock.
	cur, _ := repo.GetStage(context.Background(), stage.ID)
	if cur.State == run.StageStateFailed {
		t.Error("stage failed despite exhausted retries; must be left live for a re-report")
	}
}
