package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// E50.15 / #2656 — the plan-approve advance is a compare-and-swap.
//
// advanceStage's approve leg used the non-CAS TransitionStage, and
// transitionStage's row-locked `from == to` short-circuit returns a SILENT
// SUCCESS. A second approval racing an approval that already advanced the stage
// was therefore told it succeeded and walked straight into finishApprovalAdvance's
// post-approval hooks (fileSplitProposalChildren, fileOrLinkLiveValidationWalk),
// duplicating filed work items. The approve leg now anchors the transition on
// the state the caller ALREADY OBSERVED via run.StageCASTransitioner, so the
// racing loser refuses with a typed run.StageStateChangedError and returns
// BEFORE either hook.
//
// The anchor is the OBSERVED state, not the literal awaiting_approval (operator
// binding condition 2): the CAS means "nothing changed since I looked", so every
// transition the endpoint admits today stays admissible and no live-surface 200
// becomes a 409.

// --- CAS-honest, concurrency-safe run repository double ---

// casTransitionCall records one transition entry so a test can assert WHICH
// primitive the advance used (the CAS sibling vs the plain one).
type casTransitionCall struct {
	StageID  uuid.UUID
	From, To run.StageState
}

// casRunRepo is the CAS-HONEST, concurrency-safe run.Repository double this
// file's concurrency assertions need. The package's promptRunRepo has no mutex
// and mutates the seeded *run.Stage IN PLACE (so a concurrent reader of the
// returned pointer is a data race), and prompt_test.go's promptCASRunRepo does
// not actually compare the from-state. casRunRepo therefore:
//
//   - keeps stage state in its OWN mutex-guarded map and returns COPIES, so no
//     caller ever shares a mutable *run.Stage with a concurrent writer;
//   - implements TransitionStageFrom as a real compare-and-swap, mirroring
//     postgresRepo.TransitionStageFrom — a drifted state returns
//     run.StageStateChangedError and mutates nothing;
//   - optionally rendezvouses every transition entry (arrive) so a two-goroutine
//     test is DETERMINISTIC: neither goroutine can mutate the state until both
//     have reached their transition call, and each therefore acted on the same
//     observed awaiting_approval.
//
// Everything else (GetRun, ListStagesForRun, artifact-free reads) is inherited
// from promptRunRepo and is read-only for these fixtures.
type casRunRepo struct {
	*promptRunRepo

	mu          sync.Mutex
	states      map[uuid.UUID]run.StageState
	completions map[uuid.UUID]*run.StageCompletion
	fromCalls   []casTransitionCall
	plainCalls  []casTransitionCall

	// arrive, when non-nil, is invoked at the top of EVERY transition entry,
	// OUTSIDE the state lock.
	arrive func()
	// beforeCompare, when non-nil, runs inside the state lock immediately
	// before TransitionStageFrom's compare. It seeds the raced-window state BY
	// CONSTRUCTION (a concurrent decision landing between the caller's load and
	// its CAS) without a second goroutine.
	beforeCompare func(r *casRunRepo, id uuid.UUID)
}

func newCASRunRepo(inner *promptRunRepo) *casRunRepo {
	r := &casRunRepo{
		promptRunRepo: inner,
		states:        map[uuid.UUID]run.StageState{},
		completions:   map[uuid.UUID]*run.StageCompletion{},
	}
	for id, st := range inner.getStages {
		r.states[id] = st.State
	}
	return r
}

// setStateLocked is the raced-window seeder beforeCompare uses.
func (r *casRunRepo) setStateLocked(id uuid.UUID, to run.StageState) { r.states[id] = to }

// stageCopyLocked returns a COPY of the seeded row carrying the live state.
func (r *casRunRepo) stageCopyLocked(id uuid.UUID) (*run.Stage, bool) {
	base, ok := r.getStages[id]
	if !ok {
		return nil, false
	}
	cp := *base
	cp.State = r.states[id]
	if c := r.completions[id]; c != nil {
		cp.FailureCategory = c.FailureCategory
		cp.FailureReason = c.FailureReason
	}
	return &cp, true
}

// applyLocked mirrors postgresRepo/orchestratorRepo: the transition table
// decides admissibility, and the completion is recorded on the row.
func (r *casRunRepo) applyLocked(id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	cur, ok := r.states[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	if cur != to && !run.ValidStageTransition(cur, to) {
		return nil, run.InvalidTransitionError{Kind: "stage", From: string(cur), To: string(to)}
	}
	r.states[id] = to
	if c != nil {
		r.completions[id] = c
	}
	st, _ := r.stageCopyLocked(id)
	return st, nil
}

func (r *casRunRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.stageCopyLocked(id); ok {
		return st, nil
	}
	return nil, run.ErrNotFound
}

func (r *casRunRepo) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if r.arrive != nil {
		r.arrive()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plainCalls = append(r.plainCalls, casTransitionCall{StageID: id, To: to})
	return r.applyLocked(id, to, c)
}

func (r *casRunRepo) TransitionStageFrom(_ context.Context, id uuid.UUID, from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if r.arrive != nil {
		r.arrive()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fromCalls = append(r.fromCalls, casTransitionCall{StageID: id, From: from, To: to})
	if r.beforeCompare != nil {
		r.beforeCompare(r, id)
	}
	cur, ok := r.states[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	// The compare runs BEFORE the same-state short-circuit, exactly as
	// postgresRepo.transitionStage does — that ordering is what makes an
	// already-flipped stage a typed refusal rather than a silent success.
	if cur != from {
		return nil, run.StageStateChangedError{StageID: id, Expected: from, Actual: cur}
	}
	return r.applyLocked(id, to, c)
}

func (r *casRunRepo) stageState(id uuid.UUID) run.StageState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.states[id]
}

func (r *casRunRepo) casCalls() []casTransitionCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]casTransitionCall(nil), r.fromCalls...)
}

// casBarrier releases every arriving goroutine once n have arrived, and is a
// no-op for every arrival after that. Both worlds — CAS present and CAS deleted
// — enter the transition exactly once per approval, so the barrier can never
// deadlock under the counterfactual either.
type casBarrier struct {
	mu      sync.Mutex
	n       int
	arrived int
	ch      chan struct{}
}

func newCASBarrier(n int) *casBarrier { return &casBarrier{n: n, ch: make(chan struct{})} }

func (b *casBarrier) arrive() {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.n {
		close(b.ch)
	}
	b.mu.Unlock()
	<-b.ch
}

// --- fixture ---

// approveCASHarness stands up the COMBINED fixture: one plan stage parked at
// awaiting_approval whose approved plan carries BOTH a split_proposal (3 phases)
// AND a requires_live_validation acceptance criterion, so ONE approve exercises
// BOTH post-approval hooks. Per operator binding condition 1 the assertions
// count exactly-once PER FILING PATH (splitFilings / walkFilings below), never a
// single global total — each filing path files its own item, so a combined
// fixture cannot have a meaningful single total.
type approveCASHarness struct {
	s         *Server
	rr        *casRunRepo
	au        *auditFake
	ar        *fakeApprovalRepo
	provider  *splitFileProvider
	runID     uuid.UUID
	stageID   uuid.UUID
	planStage *run.Stage
}

// approveCASPlanBytes is splitPlanBytes plus a requires_live_validation
// criterion, so the same approved plan drives both hooks.
func approveCASPlanBytes(t *testing.T) []byte {
	t.Helper()
	var p plan.Plan
	if err := json.Unmarshal(splitPlanBytes(t, true), &p); err != nil {
		t.Fatalf("unmarshal split plan: %v", err)
	}
	p.Verification.AcceptanceCriteria = append(p.Verification.AcceptanceCriteria,
		plan.AcceptanceCriterion{
			ID:                     "ac-live",
			Statement:              "the operator confirms the rename in the live dashboard",
			RequiresLiveValidation: true,
		})
	b, err := json.Marshal(&p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return b
}

func newApproveCASHarness(t *testing.T) *approveCASHarness {
	t.Helper()
	provider := &splitFileProvider{name: workmgmt.Default().Provider}
	workmgmt.Register(provider)

	au := newAuditFake()
	inner := newPromptRunRepo()
	art := newFakeArtifactRepo()
	ar := newFakeApprovalRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	trigger := "issue:" + strconv.Itoa(splitParentIssue)
	inner.getRuns[runID] = &run.Run{
		ID:           runID,
		Repo:         "o/r",
		WorkflowID:   "feature_change",
		TriggerRef:   &trigger,
		WorkflowSpec: specImplementPathConstraints,
	}
	planStage := &run.Stage{ID: planStageID, RunID: runID, Type: run.StageTypePlan, State: run.StageStateAwaitingApproval}
	inner.getStages[planStageID] = planStage
	inner.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {planStage}}

	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID: planStageID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: approveCASPlanBytes(t),
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}
	seedReachability(au, runID, 12)

	rr := newCASRunRepo(inner)
	s := New(Config{Addr: "127.0.0.1:0", ApprovalRepo: ar, RunRepo: rr, AuditRepo: au, ArtifactRepo: art})

	return &approveCASHarness{
		s: s, rr: rr, au: au, ar: ar, provider: provider,
		runID: runID, stageID: planStageID, planStage: planStage,
	}
}

// observedStage is the stage snapshot a caller passes to approveStageAs — the
// row it loaded, parked at awaiting_approval. Both racing callers are handed
// their own copy of the SAME observation, which is exactly the race: two
// approvals whose premise ("the gate is still open") was true when each looked.
func (h *approveCASHarness) observedStage() *run.Stage {
	cp := *h.planStage
	return &cp
}

// approverIdentity is campaignOperatorIdentity's scope set under a DISTINCT
// subject, so two approvals are two distinct approvers rather than the #986
// same-subject duplicate.
func approverIdentity(subject string) Identity {
	return Identity{Subject: subject, TokenID: "operator-agent-" + subject, Scopes: operatorrole.CampaignActorScopes()}
}

// splitFilings / walkFilings partition the work-management provider's File
// calls by FILING PATH. The live-validation walk files one chore titled
// "Operator live-validation walk for #N"; every other filing is a split child.
// Counting per path is operator binding condition 1: the combined fixture runs
// both hooks, so "exactly once" is a per-path claim (3 split children — one per
// declared phase, no duplicates — and 1 walk), never a single global total.
func (h *approveCASHarness) filings() (splitFilings, walkFilings []workmgmt.ProviderRequest) {
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	for _, r := range h.provider.reqs {
		if strings.Contains(r.Item.Title, "live-validation walk") {
			walkFilings = append(walkFilings, r)
		} else {
			splitFilings = append(splitFilings, r)
		}
	}
	return splitFilings, walkFilings
}

func (h *approveCASHarness) markerCount(category string) int {
	h.au.mu.Lock()
	defer h.au.mu.Unlock()
	n := 0
	for _, e := range h.au.appended {
		if e.Category == category && e.RunID == h.runID {
			n++
		}
	}
	return n
}

// assertHooksRanExactlyOncePerPath is the shared per-filing-path assertion.
func (h *approveCASHarness) assertHooksRanExactlyOncePerPath(t *testing.T) {
	t.Helper()
	splitReqs, walkReqs := h.filings()
	if len(splitReqs) != 3 {
		t.Errorf("split-child filings = %d, want 3 (one per declared phase, filed once); titles=%v",
			len(splitReqs), filingTitles(splitReqs))
	}
	if len(walkReqs) != 1 {
		t.Errorf("live-validation walk filings = %d, want 1; titles=%v", len(walkReqs), filingTitles(walkReqs))
	}
	if got := h.markerCount(splitChildrenFiledCategory); got != 1 {
		t.Errorf("split_children_filed markers = %d, want 1", got)
	}
	if got := h.markerCount(liveValidationWalkIntentKind); got != 1 {
		t.Errorf("live_validation_walk_intent markers = %d, want 1", got)
	}
	if got := h.markerCount(liveValidationWalkLinkedKind); got != 1 {
		t.Errorf("live_validation_walk_linked markers = %d, want 1", got)
	}
}

func filingTitles(reqs []workmgmt.ProviderRequest) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Item.Title)
	}
	return out
}

// --- 1. CONCURRENT: the race, closed ---

// TestApproveAdvanceCAS_ConcurrentApprovals_HooksRunExactlyOncePerFilingPath is
// the primary CROSS-BOUNDARY test and the designated counterfactual vehicle
// (operator binding condition 5). Two distinct approvers approve the SAME
// awaiting_approval plan stage concurrently, each acting on a snapshot it
// observed while the gate was still open. The transition rendezvous makes it
// deterministic: neither can mutate the state until both have reached their
// transition, so both act on the same observed awaiting_approval — exactly the
// race. With the CAS, one wins and the loser refuses with
// run.StageStateChangedError BEFORE finishApprovalAdvance's hook tail, so each
// filing path files exactly once.
func TestApproveAdvanceCAS_ConcurrentApprovals_HooksRunExactlyOncePerFilingPath(t *testing.T) {
	h := newApproveCASHarness(t)
	barrier := newCASBarrier(2)
	h.rr.arrive = barrier.arrive

	subjects := []string{"approver-one", "approver-two"}
	errs := make([]error, len(subjects))
	var wg sync.WaitGroup
	for i, subj := range subjects {
		wg.Add(1)
		go func(i int, subj string) {
			defer wg.Done()
			_, err := h.s.approveStageAs(context.Background(), approverIdentity(subj), approveActionParams{
				Stage:    h.observedStage(),
				Decision: approval.DecisionApprove,
			})
			errs[i] = err
		}(i, subj)
	}
	wg.Wait()

	// The per-filing-path counts come FIRST: they are the behavioral assertion
	// the counterfactual has to redden, so they must be reported even when the
	// error-shape assertions below also fail.
	h.assertHooksRanExactlyOncePerPath(t)

	var losers int
	for i, err := range errs {
		if err == nil {
			continue
		}
		losers++
		var sce run.StageStateChangedError
		if !errors.As(err, &sce) {
			t.Fatalf("approval %d error = %v, want run.StageStateChangedError via errors.As", i, err)
		}
		if sce.Expected != run.StageStateAwaitingApproval || sce.Actual != run.StageStateSucceeded {
			t.Errorf("StageStateChangedError = {expected:%q actual:%q}, want {awaiting_approval succeeded}",
				sce.Expected, sce.Actual)
		}
	}
	if losers != 1 {
		t.Errorf("losing approvals = %d, want exactly 1 (errs=%v)", losers, errs)
	}
	if got := h.rr.stageState(h.stageID); got != run.StageStateSucceeded {
		t.Errorf("stage state = %q, want succeeded", got)
	}
	// Both approvals are RECORDED — the loser keeps its approval row; only the
	// advance (and therefore the hook tail) is refused.
	if len(h.ar.all) != 2 {
		t.Errorf("recorded approvals = %d, want 2", len(h.ar.all))
	}
	// Both entered the CAS; the anchor is the OBSERVED state.
	calls := h.rr.casCalls()
	if len(calls) != 2 {
		t.Fatalf("TransitionStageFrom calls = %d, want 2: %+v", len(calls), calls)
	}
	for _, c := range calls {
		if c.From != run.StageStateAwaitingApproval || c.To != run.StageStateSucceeded {
			t.Errorf("CAS call = %+v, want from=awaiting_approval to=succeeded", c)
		}
	}
}

// --- 2. The loser's error identity AND its committed state ---

// TestApproveAdvanceCAS_LoserRefusedOnStaleObservation drives the same race
// without goroutines: two back-to-back approvals under distinct subjects, the
// second still holding the awaiting_approval snapshot it observed before the
// first advanced. It asserts BOTH the error identity (an *approveActionError at
// gateActionAdvance wrapping run.StageStateChangedError) AND the COMMITTED STATE
// read back after the call — zero additional filings on either path and no
// additional markers.
//
// NOTE: this test is deliberately NOT the counterfactual vehicle. Both hooks
// carry their OWN durable-marker dedup (fileSplitProposalChildren's
// split_children_filed completion marker; fileOrLinkLiveValidationWalk's
// intent marker plus its per-run mutex), so a SEQUENTIAL re-entry no-ops even
// with the CAS deleted and its filing counts do not move. Test 1 — genuinely
// concurrent — is the vehicle whose counts redden.
func TestApproveAdvanceCAS_LoserRefusedOnStaleObservation(t *testing.T) {
	h := newApproveCASHarness(t)

	if _, err := h.s.approveStageAs(context.Background(), approverIdentity("approver-one"), approveActionParams{
		Stage:    h.observedStage(),
		Decision: approval.DecisionApprove,
	}); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	h.assertHooksRanExactlyOncePerPath(t)
	splitBefore, walkBefore := h.filings()

	_, err := h.s.approveStageAs(context.Background(), approverIdentity("approver-two"), approveActionParams{
		Stage:    h.observedStage(), // the stale premise: still awaiting_approval
		Decision: approval.DecisionApprove,
	})
	if err == nil {
		t.Fatal("second approve on a stale observation returned nil error, want refusal")
	}
	var aerr *approveActionError
	if !errors.As(err, &aerr) {
		t.Fatalf("error = %v, want *approveActionError", err)
	}
	if aerr.failedAt != gateActionAdvance {
		t.Errorf("failedAt = %v, want gateActionAdvance", aerr.failedAt)
	}
	var sce run.StageStateChangedError
	if !errors.As(err, &sce) {
		t.Fatalf("error = %v, want it to wrap run.StageStateChangedError", err)
	}
	if sce.Expected != run.StageStateAwaitingApproval || sce.Actual != run.StageStateSucceeded {
		t.Errorf("StageStateChangedError = {expected:%q actual:%q}, want {awaiting_approval succeeded}",
			sce.Expected, sce.Actual)
	}

	// COMMITTED STATE after the refusal: nothing further was filed or marked.
	splitAfter, walkAfter := h.filings()
	if len(splitAfter) != len(splitBefore) {
		t.Errorf("split-child filings after the refused approve = %d, want %d", len(splitAfter), len(splitBefore))
	}
	if len(walkAfter) != len(walkBefore) {
		t.Errorf("walk filings after the refused approve = %d, want %d", len(walkAfter), len(walkBefore))
	}
	h.assertHooksRanExactlyOncePerPath(t)
	if got := h.rr.stageState(h.stageID); got != run.StageStateSucceeded {
		t.Errorf("stage state = %q, want succeeded", got)
	}
}

// --- 3. HTTP rendering of the refusal ---

// TestApproveAdvanceCAS_HTTPRacedApproveReturns409 pins the HTTP boundary: a
// concurrent decision landing between this request's stage load and its CAS
// (seeded BY CONSTRUCTION via beforeCompare, not by calling the control) renders
// as the endpoint's already-documented 409 invalid_state_transition carrying the
// observed from-state and the actual drifted state — never a 2xx, and with
// neither hook run.
func TestApproveAdvanceCAS_HTTPRacedApproveReturns409(t *testing.T) {
	h := newApproveCASHarness(t)
	var once sync.Once
	h.rr.beforeCompare = func(r *casRunRepo, id uuid.UUID) {
		// A concurrent approval already advanced the stage in the window
		// between this request's load and its compare-and-swap.
		once.Do(func() { r.setStateLocked(id, run.StageStateSucceeded) })
	}

	w := submitApproval(t, h.s, h.stageID, `{"decision":"approve"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	if body.Error.Code != "invalid_state_transition" {
		t.Errorf("error code = %q, want invalid_state_transition (body %s)", body.Error.Code, w.Body.String())
	}
	if got := body.Error.Details["state"]; got != string(run.StageStateSucceeded) {
		t.Errorf("details.state = %v, want succeeded (body %s)", got, w.Body.String())
	}
	if got := body.Error.Details["from"]; got != string(run.StageStateAwaitingApproval) {
		t.Errorf("details.from = %v, want awaiting_approval (body %s)", got, w.Body.String())
	}
	if got := body.Error.Details["stage_id"]; got != h.stageID.String() {
		t.Errorf("details.stage_id = %v, want %s", got, h.stageID)
	}
	splitReqs, walkReqs := h.filings()
	if len(splitReqs) != 0 || len(walkReqs) != 0 {
		t.Errorf("refused approve filed work items: split=%d walk=%d", len(splitReqs), len(walkReqs))
	}
	if got := h.markerCount(splitChildrenFiledCategory); got != 0 {
		t.Errorf("split_children_filed markers = %d, want 0", got)
	}
	if got := h.markerCount(liveValidationWalkIntentKind); got != 0 {
		t.Errorf("live_validation_walk_intent markers = %d, want 0", got)
	}
}

// --- 4. Capability degradation ---

// TestApproveAdvanceCAS_NonCASRepoStillAdvances is the capability-degradation
// branch (issue done-means 5, operator binding condition 6): a RunRepo that does
// NOT implement run.StageCASTransitioner still advances the approval to
// succeeded with no error and no panic — today's behavior, preserved. The plain
// promptRunRepo is exactly such a repo.
//
// Counterfactual for THIS branch is the inverse deletion: removing the
// `if cas, ok := ...` capability assert and calling TransitionStageFrom
// unconditionally panics here on the failed interface conversion.
func TestApproveAdvanceCAS_NonCASRepoStillAdvances(t *testing.T) {
	h := newApproveCASHarness(t)
	// Swap the CAS-capable repo out for the bare promptRunRepo underneath it.
	plain := h.rr.promptRunRepo
	if _, ok := any(plain).(run.StageCASTransitioner); ok {
		t.Fatal("promptRunRepo unexpectedly implements StageCASTransitioner; it is this test's no-capability vehicle")
	}
	h.s.cfg.RunRepo = plain

	res, err := h.s.approveStageAs(context.Background(), approverIdentity("approver-one"), approveActionParams{
		Stage:    h.observedStage(),
		Decision: approval.DecisionApprove,
	})
	if err != nil {
		t.Fatalf("approve against a non-CAS RunRepo: %v", err)
	}
	if res.Stage == nil || res.Stage.State != run.StageStateSucceeded {
		t.Fatalf("stage did not advance: %+v", res.Stage)
	}
	if plain.getStages[h.stageID].State != run.StageStateSucceeded {
		t.Errorf("persisted state = %q, want succeeded", plain.getStages[h.stageID].State)
	}
	if got := len(plain.transitionStageCalls); got != 1 {
		t.Errorf("plain TransitionStage calls = %d, want 1 (the fallback path)", got)
	}
	h.assertHooksRanExactlyOncePerPath(t)
}

// --- 5. Same-subject duplicate submission ---

// TestApproveAdvanceCAS_DuplicateSubmissionUnchanged is the #986 path (issue
// done-means 3, operator binding condition 6): a SAME-subject re-submission
// still returns 200 with duplicate_submission=true, records no second approval
// row, and runs NEITHER hook. It never reaches the advance, so the CAS is not
// involved — this test proves the CAS did not perturb it.
func TestApproveAdvanceCAS_DuplicateSubmissionUnchanged(t *testing.T) {
	h := newApproveCASHarness(t)

	if w := submitApproval(t, h.s, h.stageID, `{"decision":"approve"}`); w.Code != http.StatusOK {
		t.Fatalf("first approve status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	h.assertHooksRanExactlyOncePerPath(t)
	splitBefore, walkBefore := h.filings()
	approvalsBefore := len(h.ar.all)

	w := submitApproval(t, h.s, h.stageID, `{"decision":"approve"}`) // same subject
	if w.Code != http.StatusOK {
		t.Fatalf("duplicate approve status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode duplicate body %q: %v", w.Body.String(), err)
	}
	if body["duplicate_submission"] != true {
		t.Errorf("duplicate_submission = %v, want true (body %s)", body["duplicate_submission"], w.Body.String())
	}
	if len(h.ar.all) != approvalsBefore {
		t.Errorf("approval rows = %d, want %d (no second row)", len(h.ar.all), approvalsBefore)
	}
	splitAfter, walkAfter := h.filings()
	if len(splitAfter) != len(splitBefore) || len(walkAfter) != len(walkBefore) {
		t.Errorf("duplicate submission ran a hook: split %d→%d walk %d→%d",
			len(splitBefore), len(splitAfter), len(walkBefore), len(walkAfter))
	}
	h.assertHooksRanExactlyOncePerPath(t)
}

// --- 6. Deploy pre-execution leg ---

// TestApproveAdvanceCAS_DeployLegRefusesRacedSecondAdvance covers the sharper
// leg: the deploy pre-execution advance guards an EXTERNAL delegating-pipeline
// fire, so a silently-succeeding second advance means a duplicate release
// trigger. A concurrent decision landing in the CAS window (seeded BY
// CONSTRUCTION) refuses, and triggerDeploy is NOT fired — asserted on the
// dispatch seam's call count, which is COMMITTED STATE, not the error identity.
func TestApproveAdvanceCAS_DeployLegRefusesRacedSecondAdvance(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	stub, gh := newDeployTriggerGitHub(t)
	s.cfg.GitHub = gh
	stage, runRow := seedDeployRun(rr, "release", deploySpecNoConstraints)
	runRow.InstallationID = instID(99)

	racing := &deployRacingRunRepo{approvalRunRepo: rr}
	s.cfg.RunRepo = racing

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	// COMMITTED STATE first: the external pipeline must not have fired. This is
	// the assertion the counterfactual has to redden, so it is reported even
	// when the status assertion below also fails.
	if stub.dispatchHits != 0 {
		t.Errorf("deploy dispatch hits = %d, want 0 (the refused advance must not fire the pipeline)", stub.dispatchHits)
	}
	if cur, _ := rr.GetStage(context.Background(), stage.ID); cur.State != run.StageStateAwaitingDeployApproval {
		t.Errorf("stage state = %q, want awaiting_deploy_approval (the refused advance must not move the gate)", cur.State)
	}
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_state_transition") {
		t.Errorf("body should carry invalid_state_transition: %s", w.Body.String())
	}
	if len(racing.fromCalls) != 1 {
		t.Fatalf("TransitionStageFrom calls = %d, want 1: %+v", len(racing.fromCalls), racing.fromCalls)
	}
	if got := racing.fromCalls[0]; got.From != run.StageStateAwaitingDeployApproval || got.To != run.StageStateDispatched {
		t.Errorf("deploy CAS call = %+v, want from=awaiting_deploy_approval to=dispatched", got)
	}
}

// deployRacingRunRepo gives approvalRunRepo the StageCASTransitioner capability
// and models a concurrent decision landing between the handler's stage load and
// its compare-and-swap: the compare always sees the drifted state.
type deployRacingRunRepo struct {
	*approvalRunRepo
	mu        sync.Mutex
	fromCalls []casTransitionCall
}

func (r *deployRacingRunRepo) TransitionStageFrom(_ context.Context, id uuid.UUID, from, to run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	r.mu.Lock()
	r.fromCalls = append(r.fromCalls, casTransitionCall{StageID: id, From: from, To: to})
	r.mu.Unlock()
	// The winner already moved the stage on; this caller's premise is stale.
	return nil, run.StageStateChangedError{StageID: id, Expected: from, Actual: run.StageStateDispatched}
}

// --- 7. Reject leg deliberately unchanged ---

// TestApproveAdvanceCAS_RejectLegUnchanged pins that the reject leg is
// deliberately left on run.FailStage (operator binding condition 6): a reject
// still fails the stage category D, and a reject of an already-succeeded
// (terminal) stage is still refused by FailStage's own guards. No CAS anchored
// on awaiting_approval was introduced on this path.
func TestApproveAdvanceCAS_RejectLegUnchanged(t *testing.T) {
	h := newApproveCASHarness(t)

	rejected, err := h.s.advanceStage(context.Background(), h.observedStage(), approval.DecisionReject)
	if err != nil {
		t.Fatalf("reject advance: %v", err)
	}
	if rejected.State != run.StageStateFailed {
		t.Errorf("state = %q, want failed", rejected.State)
	}
	if rejected.FailureCategory == nil || *rejected.FailureCategory != run.FailureD {
		t.Errorf("failure category = %v, want D", rejected.FailureCategory)
	}
	// No filing hook runs on a reject.
	splitReqs, walkReqs := h.filings()
	if len(splitReqs) != 0 || len(walkReqs) != 0 {
		t.Errorf("reject filed work items: split=%d walk=%d", len(splitReqs), len(walkReqs))
	}

	// A reject of an already-terminal stage is still refused.
	h2 := newApproveCASHarness(t)
	if _, err := h2.s.approveStageAs(context.Background(), approverIdentity("approver-one"), approveActionParams{
		Stage: h2.observedStage(), Decision: approval.DecisionApprove,
	}); err != nil {
		t.Fatalf("seed approve: %v", err)
	}
	succeeded, err := h2.rr.GetStage(context.Background(), h2.stageID)
	if err != nil {
		t.Fatalf("reload stage: %v", err)
	}
	if _, err := h2.s.advanceStage(context.Background(), succeeded, approval.DecisionReject); err == nil {
		t.Fatal("reject of an already-succeeded stage returned nil error, want refusal")
	}
}
