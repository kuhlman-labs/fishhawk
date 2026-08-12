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
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
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
	// getRunCalls counts run-row reads. Both the quorum gate evaluation
	// (fetchApprovalsForStage, quorum.go) and both post-approval hooks read
	// the run row, so it is the countable "did the gate machinery run again"
	// seam the duplicate-submission test asserts on.
	getRunCalls int

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

func (r *casRunRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	r.mu.Lock()
	r.getRunCalls++
	r.mu.Unlock()
	return r.promptRunRepo.GetRun(ctx, id)
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

// transitionCalls is the total number of transition ATTEMPTS on either
// primitive — the countable "was the advance re-entered" seam.
func (r *casRunRepo) transitionCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.fromCalls) + len(r.plainCalls)
}

func (r *casRunRepo) runReads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getRunCalls
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

// casHookGate is the SECOND rendezvous, and it is what makes the counterfactual
// deterministic rather than interleaving-dependent (implement-review
// high/verification). casBarrier alone synchronizes only the TRANSITION entry:
// with the CAS deleted both approvals pass the silent-success transition and
// then race UNSYNCHRONIZED into the hook bodies, so a scheduling in which the
// first completes fileSplitProposalChildren — including writing its
// split_children_filed completion marker — before the second performs its
// marker check is valid, and in it the second no-ops and the counterfactual
// comes back GREEN.
//
// casHookGate closes that window by rendezvousing the approvals INSIDE
// fileSplitProposalChildren's list-then-append idempotency guard: the gated
// audit repo below performs the marker read and then blocks, so neither
// approval can append its completion marker until every approval that will
// reach the guard has already READ it. That is precisely the unlocked
// list-before-append the CAS exists to close, and with the CAS deleted both
// approvals read an empty marker list and both file.
//
// It cannot deadlock in the CAS-PRESENT world, where only ONE approval ever
// reaches the guard: departures count toward the release condition, so the
// refused loser returning from approveStageAs releases the winner. No wall
// clock, no timeout, no polling — the release is fully determined by how many
// approvals have arrived plus how many have finished.
type casHookGate struct {
	mu       sync.Mutex
	cond     *sync.Cond
	parties  int
	arrived  int
	departed int
}

func newCASHookGate(parties int) *casHookGate {
	g := &casHookGate{parties: parties}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// arrive blocks until every party has either reached the guard or finished.
func (g *casHookGate) arrive() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.arrived++
	g.cond.Broadcast()
	for g.arrived+g.departed < g.parties {
		g.cond.Wait()
	}
}

// depart records that one approval has returned (whether or not it reached the
// guard), so a refused loser can never leave a waiting winner wedged.
func (g *casHookGate) depart() {
	g.mu.Lock()
	g.departed++
	g.cond.Broadcast()
	g.mu.Unlock()
}

// casGatedAudit decorates auditFake with (a) a per-category read counter, which
// is the POSITIVE evidence that a hook was re-entered at all (a dedup test that
// cannot tell "deduplicated" from "never ran" is vacuous), and (b) the optional
// casHookGate rendezvous on ONE category's read.
//
// arrive() is called AFTER the inner read returns, i.e. OUTSIDE auditFake's own
// mutex — a goroutine parked in the gate while holding that mutex would wedge
// every other audit reader, including the loser whose departure releases it.
type casGatedAudit struct {
	*auditFake

	mu    sync.Mutex
	reads map[string]int

	// gateCategory / gate, when set, rendezvous every read of that category.
	gateCategory string
	gate         *casHookGate
}

func newCASGatedAudit(inner *auditFake) *casGatedAudit {
	return &casGatedAudit{auditFake: inner, reads: map[string]int{}}
}

func (a *casGatedAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	out, err := a.auditFake.ListForRunByCategory(ctx, runID, category)
	a.mu.Lock()
	a.reads[category]++
	gate := a.gate
	gated := a.gateCategory == category
	a.mu.Unlock()
	if gate != nil && gated {
		gate.arrive()
	}
	return out, err
}

func (a *casGatedAudit) readCount(category string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reads[category]
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
	s  *Server
	rr *casRunRepo
	au *auditFake
	// gaud is the audit repo the server actually holds: auditFake plus the
	// per-category read counter and the optional hook-window rendezvous.
	gaud      *casGatedAudit
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
	gaud := newCASGatedAudit(au)
	s := New(Config{Addr: "127.0.0.1:0", ApprovalRepo: ar, RunRepo: rr, AuditRepo: gaud, ArtifactRepo: art})

	return &approveCASHarness{
		s: s, rr: rr, au: au, gaud: gaud, ar: ar, provider: provider,
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
// observed while the gate was still open.
//
// TWO rendezvous make it deterministic in BOTH worlds:
//
//	(1) casBarrier, at the TRANSITION entry — neither approval can mutate the
//	    state until both have reached their transition, so both act on the same
//	    observed awaiting_approval. That is exactly the race.
//	(2) casHookGate, INSIDE fileSplitProposalChildren's list-then-append
//	    idempotency guard — neither approval that reaches the guard can append
//	    its split_children_filed completion marker until every approval that
//	    will reach the guard has already READ the marker list.
//
// Rendezvous (1) alone would leave the counterfactual interleaving-dependent
// (implement-review high/verification): with the CAS deleted both approvals
// clear the silent-success transition and then race unsynchronized into the
// hooks, and a scheduling in which the first writes its completion marker
// before the second reads it makes the second no-op and the deletion come back
// GREEN. Rendezvous (2) removes that scheduling: with the CAS deleted both
// approvals read an EMPTY marker list and both file, so the per-path counts
// redden every run. With the CAS present only one approval ever reaches the
// guard, and the loser's departure — not a timeout — releases it.
func TestApproveAdvanceCAS_ConcurrentApprovals_HooksRunExactlyOncePerFilingPath(t *testing.T) {
	h := newApproveCASHarness(t)
	barrier := newCASBarrier(2)
	h.rr.arrive = barrier.arrive
	gate := newCASHookGate(2)
	h.gaud.gateCategory = splitChildrenFiledCategory
	h.gaud.gate = gate

	subjects := []string{"approver-one", "approver-two"}
	errs := make([]error, len(subjects))
	var wg sync.WaitGroup
	for i, subj := range subjects {
		wg.Add(1)
		go func(i int, subj string) {
			defer wg.Done()
			// depart() before the goroutine ends so a REFUSED approval that
			// never reaches the hook guard still releases the winner parked in
			// it — the CAS-present world's release condition.
			defer gate.depart()
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
	// Exactly ONE approval reached the hook guard at all — the CAS kept the
	// loser out of finishApprovalAdvance's tail rather than letting it in and
	// relying on the marker dedup to absorb it. Under the deleted-CAS
	// counterfactual this reads 2 (both approvals rendezvous in the guard).
	if got := h.gaud.readCount(splitChildrenFiledCategory); got != 1 {
		t.Errorf("approvals reaching the split-filing idempotency guard = %d, want 1", got)
	}

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

// --- 2b. The sequential FRESH-LOAD re-approval the anchor deliberately admits ---

// TestApproveAdvanceCAS_SequentialFreshLoadReapprove_MarkerDedupFilesNothing
// pins the claim the README makes about what the observed-state anchor
// deliberately does NOT close (implement-review low/untested-path). A second
// approver whose request loads the stage AFTER the first advance sees
// `succeeded`, so its CAS compares EQUAL, hits the from == to short-circuit,
// and RE-ENTERS the post-approval hook tail. That is required behavior, not a
// gap: refusing it would be exactly the 200 → 409 narrowing operator binding
// condition 2 forbids. What keeps it harmless is each hook's own durable-marker
// dedup, and until now that guarantee was pinned only by prose.
//
// The test is non-vacuous in both directions: it asserts the hook was RE-ENTERED
// (the split-filing idempotency guard is read a SECOND time — a test that could
// not tell "deduplicated" from "never ran" would prove nothing) AND that the
// re-entry filed nothing on either path.
func TestApproveAdvanceCAS_SequentialFreshLoadReapprove_MarkerDedupFilesNothing(t *testing.T) {
	h := newApproveCASHarness(t)

	if _, err := h.s.approveStageAs(context.Background(), approverIdentity("approver-one"), approveActionParams{
		Stage:    h.observedStage(),
		Decision: approval.DecisionApprove,
	}); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	h.assertHooksRanExactlyOncePerPath(t)
	splitBefore, walkBefore := h.filings()
	guardReadsBefore := h.gaud.readCount(splitChildrenFiledCategory)

	// A genuinely FRESH load — this approver's request read the stage after the
	// first advance committed, so its premise is `succeeded` and true.
	fresh, err := h.rr.GetStage(context.Background(), h.stageID)
	if err != nil {
		t.Fatalf("fresh load: %v", err)
	}
	if fresh.State != run.StageStateSucceeded {
		t.Fatalf("fresh load state = %q, want succeeded", fresh.State)
	}

	if _, err := h.s.approveStageAs(context.Background(), approverIdentity("approver-two"), approveActionParams{
		Stage:    fresh,
		Decision: approval.DecisionApprove,
	}); err != nil {
		t.Fatalf("sequential fresh-load re-approve returned %v; it must still be admitted (no 200→409 narrowing)", err)
	}

	// It really did re-enter the hook tail — this is the assertion that makes
	// the zero-additional-filings claim below about DEDUP rather than about the
	// hook never having run.
	if got := h.gaud.readCount(splitChildrenFiledCategory); got != guardReadsBefore+1 {
		t.Errorf("split-filing idempotency-guard reads = %d, want %d (the re-approval must RE-ENTER the hook)",
			got, guardReadsBefore+1)
	}
	// The CAS was anchored on the freshly observed state and compared equal.
	calls := h.rr.casCalls()
	if len(calls) != 2 {
		t.Fatalf("TransitionStageFrom calls = %d, want 2: %+v", len(calls), calls)
	}
	if got := calls[1]; got.From != run.StageStateSucceeded || got.To != run.StageStateSucceeded {
		t.Errorf("second CAS call = %+v, want from=succeeded to=succeeded (the observed-state anchor)", got)
	}

	// …and the marker dedup absorbed it: nothing further filed on either path.
	splitAfter, walkAfter := h.filings()
	if len(splitAfter) != len(splitBefore) {
		t.Errorf("split-child filings after the fresh-load re-approve = %d, want %d", len(splitAfter), len(splitBefore))
	}
	if len(walkAfter) != len(walkBefore) {
		t.Errorf("walk filings after the fresh-load re-approve = %d, want %d", len(walkAfter), len(walkBefore))
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
// row, RE-RUNS NO GATE, and runs NEITHER hook. It never reaches the advance, so
// the CAS is not involved — this test proves the CAS did not perturb it.
//
// "Re-runs no gate" is asserted on three countable seams rather than inferred
// (implement-review high/test_vacuity): no further transition ATTEMPT on either
// primitive (the advance gate), no further run-row read (the quorum gate
// evaluation, fetchApprovalsForStage → RunRepo.GetRun, and both hooks), and no
// second approval_submitted audit row (the gate-evaluation record every
// evaluating path writes).
func TestApproveAdvanceCAS_DuplicateSubmissionUnchanged(t *testing.T) {
	h := newApproveCASHarness(t)

	if w := submitApproval(t, h.s, h.stageID, `{"decision":"approve"}`); w.Code != http.StatusOK {
		t.Fatalf("first approve status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	h.assertHooksRanExactlyOncePerPath(t)
	splitBefore, walkBefore := h.filings()
	approvalsBefore := len(h.ar.all)
	transitionsBefore := h.rr.transitionCalls()
	runReadsBefore := h.rr.runReads()
	auditsBefore := h.markerCount("approval_submitted")
	if transitionsBefore == 0 || runReadsBefore == 0 || auditsBefore == 0 {
		t.Fatalf("gate-seam baseline is empty (transitions=%d run_reads=%d approval_submitted=%d); the deltas below would be vacuous",
			transitionsBefore, runReadsBefore, auditsBefore)
	}

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

	// No gate was re-evaluated on the duplicate request.
	if got := h.rr.transitionCalls(); got != transitionsBefore {
		t.Errorf("transition attempts = %d, want %d (a duplicate must not re-enter the advance)", got, transitionsBefore)
	}
	if got := h.rr.runReads(); got != runReadsBefore {
		t.Errorf("run-row reads = %d, want %d (a duplicate must not re-evaluate the quorum gate or the hooks)",
			got, runReadsBefore)
	}
	if got := h.markerCount("approval_submitted"); got != auditsBefore {
		t.Errorf("approval_submitted audit rows = %d, want %d (a duplicate must not re-emit a gate-evaluation record)",
			got, auditsBefore)
	}
}

// --- 6. Deploy pre-execution leg ---

// TestApproveAdvanceCAS_DeployLeg_DispatchesOnceThenRefusesRacedSecondAdvance
// covers the sharper leg: the deploy pre-execution advance guards an EXTERNAL
// delegating-pipeline fire, so a silently-succeeding second advance means a
// duplicate release trigger.
//
// It drives the full exactly-once sequence against an HONEST compare-and-swap
// (implement-review high/test_vacuity): a FIRST deploy approval that succeeds
// and fires the pipeline, THEN a raced second approval whose premise is the
// awaiting_deploy_approval snapshot it observed before the first advance. Both
// halves are load-bearing — the first is what distinguishes this
// implementation from one that simply refuses every deploy approval, and the
// second is the refusal. The decisive assertion is the dispatch seam's call
// count, which is COMMITTED STATE, not the error identity.
func TestApproveAdvanceCAS_DeployLeg_DispatchesOnceThenRefusesRacedSecondAdvance(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	stub, gh := newDeployTriggerGitHub(t)
	s.cfg.GitHub = gh
	stage, runRow := seedDeployRun(rr, "release", deploySpecNoConstraints)
	runRow.InstallationID = instID(99)
	// The snapshot the racing second approver loaded while the gate was still
	// open, captured BEFORE the first approval mutates the row.
	observed := *stage

	cas := &deployCASRunRepo{approvalRunRepo: rr}
	s.cfg.RunRepo = cas

	// (a) The FIRST deploy approval succeeds and fires the pipeline exactly once.
	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("first deploy approve status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if got := stub.dispatchHits; got != 1 {
		t.Fatalf("deploy dispatch hits after the first approve = %d, want 1", got)
	}
	cur, err := rr.GetStage(context.Background(), stage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if cur.State != run.StageStateAwaitingDeployment {
		t.Fatalf("stage state after the first approve = %q, want awaiting_deployment", cur.State)
	}

	// (b) The RACED second advance, on the stale premise, is refused.
	_, err = s.advanceForDecision(context.Background(), &observed, approval.DecisionApprove)
	// COMMITTED STATE first: the external pipeline must not have fired again.
	// This is the assertion the counterfactual has to redden, so it is reported
	// even when the error-shape assertions below also fail.
	if got := stub.dispatchHits; got != 1 {
		t.Errorf("deploy dispatch hits = %d, want 1 (the refused advance must not re-fire the pipeline)", got)
	}
	if cur, _ := rr.GetStage(context.Background(), stage.ID); cur.State != run.StageStateAwaitingDeployment {
		t.Errorf("stage state = %q, want awaiting_deployment (the refused advance must not move the stage)", cur.State)
	}
	if err == nil {
		t.Fatal("raced second deploy advance returned nil error, want refusal")
	}
	var sce run.StageStateChangedError
	if !errors.As(err, &sce) {
		t.Fatalf("error = %v, want run.StageStateChangedError", err)
	}
	if sce.Expected != run.StageStateAwaitingDeployApproval || sce.Actual != run.StageStateAwaitingDeployment {
		t.Errorf("StageStateChangedError = {expected:%q actual:%q}, want {awaiting_deploy_approval awaiting_deployment}",
			sce.Expected, sce.Actual)
	}
	calls := cas.casCalls()
	if len(calls) != 2 {
		t.Fatalf("TransitionStageFrom calls = %d, want 2: %+v", len(calls), calls)
	}
	for _, c := range calls {
		if c.From != run.StageStateAwaitingDeployApproval || c.To != run.StageStateDispatched {
			t.Errorf("deploy CAS call = %+v, want from=awaiting_deploy_approval to=dispatched", c)
		}
	}
}

// TestApproveAdvanceCAS_DeployLegRacedApproveReturns409 pins the HTTP rendering
// of that refusal on the deploy leg: a concurrent decision landing between this
// request's stage load and its compare-and-swap (seeded BY CONSTRUCTION via
// raceBefore, not by calling the control) returns 409 invalid_state_transition
// and never fires the pipeline.
func TestApproveAdvanceCAS_DeployLegRacedApproveReturns409(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	stub, gh := newDeployTriggerGitHub(t)
	s.cfg.GitHub = gh
	stage, runRow := seedDeployRun(rr, "release", deploySpecNoConstraints)
	runRow.InstallationID = instID(99)

	var once sync.Once
	cas := &deployCASRunRepo{approvalRunRepo: rr, raceBefore: func(r *deployCASRunRepo, id uuid.UUID) {
		// A concurrent approval already advanced the stage in the window
		// between this request's load and its compare-and-swap.
		once.Do(func() { r.setState(id, run.StageStateDispatched) })
	}}
	s.cfg.RunRepo = cas

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if got := stub.dispatchHits; got != 0 {
		t.Errorf("deploy dispatch hits = %d, want 0 (the refused advance must not fire the pipeline)", got)
	}
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_state_transition") {
		t.Errorf("body should carry invalid_state_transition: %s", w.Body.String())
	}
}

// deployCASRunRepo gives approvalRunRepo the run.StageCASTransitioner
// capability with an HONEST compare, mirroring postgresRepo.TransitionStageFrom:
// a matching observed state advances for real, a drifted one returns
// run.StageStateChangedError and mutates nothing. raceBefore, when set, seeds a
// concurrent decision landing inside the CAS window.
type deployCASRunRepo struct {
	*approvalRunRepo
	callMu     sync.Mutex
	fromCalls  []casTransitionCall
	raceBefore func(r *deployCASRunRepo, id uuid.UUID)
}

func (r *deployCASRunRepo) TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	r.callMu.Lock()
	r.fromCalls = append(r.fromCalls, casTransitionCall{StageID: id, From: from, To: to})
	r.callMu.Unlock()
	if r.raceBefore != nil {
		r.raceBefore(r, id)
	}
	cur, err := r.GetStage(ctx, id)
	if err != nil {
		return nil, err
	}
	if cur.State != from {
		return nil, run.StageStateChangedError{StageID: id, Expected: from, Actual: cur.State}
	}
	return r.TransitionStage(ctx, id, to, c)
}

func (r *deployCASRunRepo) setState(id uuid.UUID, to run.StageState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.stages[id]; ok {
		st.State = to
	}
}

func (r *deployCASRunRepo) casCalls() []casTransitionCall {
	r.callMu.Lock()
	defer r.callMu.Unlock()
	return append([]casTransitionCall(nil), r.fromCalls...)
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
