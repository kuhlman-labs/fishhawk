package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// merge_blocking_concern_test.go pins #5045: the inline merge endpoint and
// the delegated may_merge arm evaluate the merge-eligibility blocking-concern
// leg (d), child-slice reconcile included, INSIDE the run's merge-intent lock.
// It also pins #5261: merge-on-pass re-judges leg (d) inside that lock too, at
// mergeOnPassLocked step 4b (the "merge-on-pass (#5261)" section).
//
// Counterfactual selectors: a pgtest skipped INSIDE a t.Run subtest reads
// GREEN to the counterfactual classifier (the parent passes), so the
// cross-layer test below, whose worlds are built inside subtests, is excluded
// from every selector. The #5261 merge-on-pass tests build their Postgres
// world in the TOP-LEVEL test body with no t.Run, so a run without a database
// skips the top-level test (no_tests_matched, an error, never GREEN); they are
// the counterfactual-selected tests.

// bcEvents is an ordered, goroutine-safe event log.
type bcEvents struct {
	mu  sync.Mutex
	log []string
}

func (e *bcEvents) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, s)
}

func (e *bcEvents) get() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.log...)
}

// bcLockProbeQueue is a merge queue whose merge-intent lock sets held around
// fn and logs "lock"; onLock (optional) runs just before fn. Every method the
// inline and delegated dispatch tails reach is stubbed (the ownership read,
// the D1 claim and its settles), so a counterfactual lands on its assertion,
// never on a nil embedded method.
type bcLockProbeQueue struct {
	mergequeue.Store
	held   *atomic.Bool
	events *bcEvents
	onLock func()
	// fnTimeout, when set, bounds fn's context like the real store's intent
	// fn deadline, and a lapse is reported after fn returns, as it does.
	fnTimeout time.Duration
}

func newBCLockProbe() bcLockProbeQueue {
	return bcLockProbeQueue{held: &atomic.Bool{}, events: &bcEvents{}}
}

func (q bcLockProbeQueue) WithMergeIntentLock(ctx context.Context, _ uuid.UUID, fn func(context.Context) error) error {
	q.events.add("lock")
	if q.onLock != nil {
		q.onLock()
	}
	q.held.Store(true)
	defer q.held.Store(false)
	if q.fnTimeout <= 0 {
		return fn(ctx)
	}
	fctx, cancel := context.WithTimeout(ctx, q.fnTimeout)
	defer cancel()
	if err := fn(fctx); err != nil {
		return err
	}
	return fctx.Err()
}

func (q bcLockProbeQueue) ActiveForRun(context.Context, uuid.UUID) (mergequeue.Status, bool, error) {
	return mergequeue.Status{}, false, nil
}

func (q bcLockProbeQueue) ClaimMergeDispatch(_ context.Context, runID uuid.UUID, path, headSHA string) (mergequeue.DispatchClaim, error) {
	q.events.add("claim")
	return mergequeue.DispatchClaim{RunID: runID, Path: path, HeadSHA: headSHA, State: mergequeue.DispatchDispatched}, nil
}

func (q bcLockProbeQueue) MarkMergeDispatchFailed(context.Context, uuid.UUID, string) error {
	return nil
}

func (q bcLockProbeQueue) ReleaseMergeDispatch(context.Context, mergequeue.DispatchClaim) error {
	return nil
}

func (q bcLockProbeQueue) MergeDispatch(context.Context, uuid.UUID) (mergequeue.DispatchClaim, bool, error) {
	return mergequeue.DispatchClaim{}, false, nil
}

// bcConcernRepo wraps the fake: ImportRolledUp logs "import" and whether the
// lock was held, and ListOpenByRun fails for failRun ONLY while held (so the
// pre-lock first-officer read succeeds and only the in-lock leg read fails).
type bcConcernRepo struct {
	*fakeConcernRepo
	held       *atomic.Bool
	events     *bcEvents
	mu         sync.Mutex
	importHeld []bool
	failRun    uuid.UUID
	// blockImport makes ImportRolledUp wait for its context to end (a
	// contended child-source row lock) and return that context's error.
	blockImport bool
}

func (c *bcConcernRepo) ImportRolledUp(ctx context.Context, p concern.ImportRolledUpParams) ([]*concern.Concern, error) {
	c.events.add("import")
	c.mu.Lock()
	c.importHeld = append(c.importHeld, c.held.Load())
	c.mu.Unlock()
	if c.blockImport {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return c.fakeConcernRepo.ImportRolledUp(ctx, p)
}

func (c *bcConcernRepo) ListOpenByRun(ctx context.Context, runID uuid.UUID) ([]*concern.Concern, error) {
	if c.failRun != uuid.Nil && runID == c.failRun && c.held.Load() {
		return nil, errors.New("concern ledger down")
	}
	return c.fakeConcernRepo.ListOpenByRun(ctx, runID)
}

func (c *bcConcernRepo) imports() []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bool(nil), c.importHeld...)
}

// bcErrCode decodes an error envelope.
func bcErrCode(t *testing.T, w *httptest.ResponseRecorder) (string, string, map[string]any) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %d: %v: %s", w.Code, err, w.Body.String())
	}
	return env.Error.Code, env.Error.Message, env.Error.Details
}

func bcRequireBlocking(t *testing.T, w *httptest.ResponseRecorder, wantIDs ...string) map[string]any {
	t.Helper()
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 blocking_concern_open:\n%s", w.Code, w.Body.String())
	}
	code, msg, details := bcErrCode(t, w)
	if code != mergeQueueReasonBlockingConcernOpen {
		t.Fatalf("code = %q, want %s: %s", code, mergeQueueReasonBlockingConcernOpen, w.Body.String())
	}
	if !strings.Contains(msg, "nothing new was recorded by this call") {
		t.Errorf("message = %q, want it to say nothing new was recorded by this call", msg)
	}
	ids, _ := details["concern_ids"].([]any)
	if len(wantIDs) > 0 {
		if len(ids) != len(wantIDs) {
			t.Fatalf("concern_ids = %v, want %v", ids, wantIDs)
		}
		for i, id := range wantIDs {
			if ids[i] != id {
				t.Errorf("concern_ids[%d] = %v, want %s", i, ids[i], id)
			}
		}
	}
	if details["next_step"] != blockingConcernHoldNextStep {
		t.Errorf("next_step = %v, want %q", details["next_step"], blockingConcernHoldNextStep)
	}
	return details
}

// bcInline is the inline fake-store world: a gate-admitted (nil spec) run.
type bcInline struct {
	s      *Server
	repo   *autoDriveRepo
	au     *auditFake
	cr     *fakeConcernRepo
	merger *fakeMerger
	runID  uuid.UUID
	stage  uuid.UUID
}

func newBCInline(t *testing.T) *bcInline {
	t.Helper()
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID, stageID := uuid.New(), uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, []*run.Stage{{ID: stageID, RunID: runID, Sequence: 2, Type: run.StageTypeImplement, State: run.StageStateSucceeded}})
	return &bcInline{s: s, repo: repo, au: au, cr: s.cfg.ConcernRepo.(*fakeConcernRepo), merger: merger, runID: runID, stage: stageID}
}

func (b *bcInline) raise(t *testing.T, severity, provenance string) *concern.Concern {
	t.Helper()
	rows, err := b.cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID: b.runID, StageID: b.stage, StageKind: concern.StageKindImplement, Provenance: provenance,
		ReviewerModel: "gpt-6-astra", OriginReviewSequence: 1,
		Concerns: []concern.RaisedConcern{{Severity: severity, Category: "correctness", Note: "n"}},
	})
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	return rows[0]
}

func (b *bcInline) post(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return postMergeRun(t, b.s, b.runID, mergeRunRequest{Verdict: "lgtm"}, withMergeOperator)
}

func (b *bcInline) requireNothingRecorded(t *testing.T) {
	t.Helper()
	if n := len(mergeVerdictRows(b.au)); n != 0 {
		t.Errorf("merge_verdict_recorded rows = %d, want 0", n)
	}
	if n := countAudit(b.au, CategoryMergeQueueAuthorized); n != 0 {
		t.Errorf("merge_queue_authorized rows = %d, want 0", n)
	}
	if b.merger.called != 0 {
		t.Errorf("merger called %d times, want 0", b.merger.called)
	}
}

func bcRequireMerged(t *testing.T, w *httptest.ResponseRecorder) mergeRunResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp mergeRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// (1) An open reviewer HIGH refuses inside the lock. firstOfficerHoldRefusal
// filters first_officer provenance only, so without the in-lock leg nothing
// else on this path reads a reviewer HIGH and the run merges.
func TestMergeRun_InlineBlockingConcern_OpenReviewerHighRefuses(t *testing.T) {
	b := newBCInline(t)
	high := b.raise(t, "high", "")
	bcRequireBlocking(t, b.post(t), high.ID.String())
	b.requireNothingRecorded(t)
}

// (2) The existing dispositions are the way past the refusal; no override.
func TestMergeRun_InlineBlockingConcern_DispositionClears(t *testing.T) {
	ctx := context.Background()
	for name, clear := range map[string]func(t *testing.T, b *bcInline, id uuid.UUID){
		"waive": func(t *testing.T, b *bcInline, id uuid.UUID) {
			if _, err := b.cr.ApplyHumanDisposition(ctx, id, concern.StateWaived, "accepted", concern.HumanDisposition{
				Outcome: concern.OutcomeWaived, Judgement: concern.JudgementNoDefect, ActorKind: "user"}); err != nil {
				t.Fatalf("waive: %v", err)
			}
		},
		"defer": func(t *testing.T, b *bcInline, id uuid.UUID) {
			if _, err := b.cr.ApplyHumanDisposition(ctx, id, concern.StateDeferred, "later", concern.HumanDisposition{
				Outcome: concern.OutcomeDeferred, Judgement: concern.JudgementDefect, ActorKind: "user"}); err != nil {
				t.Fatalf("defer: %v", err)
			}
		},
		"fix-up": func(t *testing.T, b *bcInline, id uuid.UUID) {
			if err := b.cr.MarkAddressedPending(ctx, []uuid.UUID{id}, "fix-up routed"); err != nil {
				t.Fatalf("mark addressed pending: %v", err)
			}
			if _, err := b.cr.ApplyResolution(ctx, id, concern.StateAddressed, "fixed"); err != nil {
				t.Fatalf("resolve addressed: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBCInline(t)
			high := b.raise(t, "high", "")
			bcRequireBlocking(t, b.post(t), high.ID.String())
			clear(t, b, high.ID)
			resp := bcRequireMerged(t, b.post(t))
			if !resp.MergeQueued {
				t.Errorf("merge_queued = false after the %s, want true", name)
			}
			if n := len(mergeVerdictRows(b.au)); n != 1 {
				t.Errorf("merge_verdict_recorded rows = %d, want 1", n)
			}
			if b.merger.called != 1 {
				t.Errorf("merger called %d times, want 1", b.merger.called)
			}
		})
	}
}

// (3) An open MEDIUM is advisory (ADR-027): the merge proceeds.
func TestMergeRun_InlineBlockingConcern_MediumDoesNotBlock(t *testing.T) {
	b := newBCInline(t)
	b.raise(t, "medium", "")
	if resp := bcRequireMerged(t, b.post(t)); !resp.MergeQueued {
		t.Error("merge_queued = false, want true for a medium concern")
	}
	if b.merger.called != 1 {
		t.Errorf("merger called %d times, want 1", b.merger.called)
	}
}

// (4) A first_officer concern keeps its own, earlier, more specific code.
func TestMergeRun_InlineBlockingConcern_FirstOfficerKeepsItsCode(t *testing.T) {
	b := newBCInline(t)
	b.raise(t, "low", concern.ProvenanceFirstOfficer)
	w := b.post(t)
	if code, _, _ := bcErrCode(t, w); w.Code != http.StatusConflict || code != codeFirstOfficerFindingOpen {
		t.Fatalf("answer = %d %q, want 409 %s: %s", w.Code, code, codeFirstOfficerFindingOpen, w.Body.String())
	}
	b.requireNothingRecorded(t)
}

// (5) A HIGH raised at lock time, after the pre-lock holds read the ledger,
// refuses: only an in-lock read can see it.
func TestMergeRun_InlineBlockingConcern_HighRaisedAtLockRefuses(t *testing.T) {
	b := newBCInline(t)
	q := newBCLockProbe()
	var raised *concern.Concern
	q.onLock = func() { raised = b.raise(t, "high", "") }
	b.s.cfg.MergeQueue = q
	w := b.post(t)
	if raised == nil {
		t.Fatal("the on-lock hook did not run")
	}
	bcRequireBlocking(t, w, raised.ID.String())
	b.requireNothingRecorded(t)
	if got := q.events.get(); strings.Contains(strings.Join(got, ","), "claim") {
		t.Errorf("events = %v, want no dispatch claim", got)
	}
}

// (6) A PR that already merged keeps the #3622 already_merged answer.
func TestMergeRun_InlineBlockingConcern_AlreadyMergedAnswersAlreadyMerged(t *testing.T) {
	b := newBCInline(t)
	b.raise(t, "high", "")
	seedReviewEntry(t, b.au, b.runID, 3, CategoryPRMerged, map[string]any{"pr_url": mergePR})
	resp := bcRequireMerged(t, b.post(t))
	if !resp.AlreadyMerged || resp.MergeQueued {
		t.Errorf("response = %+v, want already_merged:true merge_queued:false", resp)
	}
	if b.merger.called != 0 {
		t.Errorf("merger called %d times, want 0", b.merger.called)
	}
}

// C4: a re-POST after a dispatched merge, with a HIGH raised since, answers
// honestly (409, nothing new recorded) and retracts nothing.
func TestMergeRun_InlineBlockingConcern_ResumeAfterDispatchIsHonest(t *testing.T) {
	b := newBCInline(t)
	bcRequireMerged(t, b.post(t))
	if n := len(mergeVerdictRows(b.au)); n != 1 || b.merger.called != 1 {
		t.Fatalf("precondition: verdicts=%d dispatches=%d, want 1 each", n, b.merger.called)
	}
	b.au.mu.Lock()
	before := len(b.au.appended)
	b.au.mu.Unlock()
	high := b.raise(t, "high", "")
	_, msg, _ := bcErrCode(t, b.post(t))
	bcRequireBlocking(t, b.post(t), high.ID.String())
	if !strings.Contains(msg, "a merge an earlier call already dispatched is not retracted") {
		t.Errorf("message = %q, want it to say an earlier dispatch is not retracted", msg)
	}
	b.au.mu.Lock()
	after := len(b.au.appended)
	b.au.mu.Unlock()
	if after != before {
		t.Errorf("audit rows %d -> %d, want nothing appended by the held re-POSTs", before, after)
	}
	if n := len(mergeVerdictRows(b.au)); n != 1 {
		t.Errorf("merge_verdict_recorded rows = %d, want the original 1 (never retracted)", n)
	}
	if b.merger.called != 1 {
		t.Errorf("merger called %d times, want the original 1", b.merger.called)
	}
}

// (9) A ledger read failure inside the lock fails closed. The wrapper fails
// ListOpenByRun only while the lock is held, so the pre-lock first-officer
// read succeeds and only the in-lock leg read fails.
func TestMergeRun_InlineBlockingConcern_LedgerReadFailureFailsClosed(t *testing.T) {
	b := newBCInline(t)
	q := newBCLockProbe()
	b.s.cfg.MergeQueue = q
	b.s.cfg.ConcernRepo = &bcConcernRepo{fakeConcernRepo: b.cr, held: q.held, events: q.events, failRun: b.runID}
	w := b.post(t)
	if code, msg, _ := bcErrCode(t, w); w.Code != http.StatusInternalServerError || code != "internal_error" ||
		!strings.Contains(msg, "nothing new was recorded by this call") {
		t.Fatalf("answer = %d %q %q, want 500 internal_error saying nothing new was recorded: %s", w.Code, code, msg, w.Body.String())
	}
	b.requireNothingRecorded(t)
}

// C8: an unwired concern repository skips the inline leg with one WARN.
func TestMergeRun_InlineBlockingConcern_NilConcernRepoSkipsWithWarn(t *testing.T) {
	b := newBCInline(t)
	var logs bytes.Buffer
	b.s.cfg.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	b.s.cfg.ConcernRepo = nil
	bcRequireMerged(t, b.post(t))
	if n := strings.Count(logs.String(), "the blocking-concern hold is skipped"); n != 1 {
		t.Errorf("skip WARN lines = %d, want 1; logs:\n%s", n, logs.String())
	}
	if b.merger.called != 1 {
		t.Errorf("merger called %d times, want 1", b.merger.called)
	}
}

// --- child reconcile (newRollupFixture) -----------------------------------

func newBCRollup(t *testing.T) (*rollupFixture, *fakeMerger) {
	t.Helper()
	f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}})
	merger := &fakeMerger{}
	f.s.cfg.GateMerger = merger
	pr := "https://github.com/acme-widgets/storefront/pull/9"
	f.parent.PullRequestURL = &pr
	f.parent.State = run.StateRunning
	// No recorded spec: the forge-approval fold makes no read, so with the
	// leg absent the merge would dispatch (the counterfactual's observable).
	f.parent.WorkflowSHA = ""
	return f, merger
}

func bcRollupPost(t *testing.T, f *rollupFixture) *httptest.ResponseRecorder {
	t.Helper()
	return postMergeRun(t, f.s, f.parent.ID, mergeRunRequest{Verdict: "lgtm"}, withMergeOperator)
}

func bcRollupVerdicts(t *testing.T, f *rollupFixture) int {
	t.Helper()
	rows, err := f.au.ListForRunByCategory(context.Background(), f.parent.ID, CategoryMergeVerdictRecorded)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// (7) An uncopied succeeded-child HIGH refuses: the in-lock reconcile copies
// it onto the parent ledger, and the leg then blocks on the copy. With no
// reconcile the parent ledger is empty and the merge dispatches.
func TestMergeRun_InlineBlockingConcern_UncopiedChildHighRefuses(t *testing.T) {
	f, merger := newBCRollup(t)
	src := f.seedChildConcern(t, 0, "high", "uncopied child high")
	w := bcRollupPost(t, f)
	copies := f.parentCopies(t)
	if len(copies) != 1 || copyOf(copies, src) == nil || !copies[0].State.IsOpen() {
		t.Fatalf("parent copies = %v, want exactly one open copy of the child HIGH", copies)
	}
	bcRequireBlocking(t, w, copies[0].ID.String())
	rows := f.rolledUpRows(t)
	if len(rows) != 1 || !strings.Contains(string(rows[0].Payload), `"site":"merge_reconcile"`) {
		t.Errorf("concern_rolled_up rows = %d, want 1 with site merge_reconcile", len(rows))
	}
	if n := bcRollupVerdicts(t, f); n != 0 {
		t.Errorf("merge_verdict_recorded rows = %d, want 0", n)
	}
	if merger.called != 0 {
		t.Errorf("merger called %d times, want 0", merger.called)
	}
}

// (8) A reconcile failure fails closed. ImportRolledUp is called only by the
// reconcile, never by the first-officer or coverage holds.
func TestMergeRun_InlineBlockingConcern_ReconcileFailureFailsClosed(t *testing.T) {
	f, merger := newBCRollup(t)
	f.seedChildConcern(t, 0, "high", "uncopied child high")
	f.cr.importErr = errors.New("insert failed")
	w := bcRollupPost(t, f)
	if code, msg, _ := bcErrCode(t, w); w.Code != http.StatusInternalServerError || code != "internal_error" ||
		!strings.Contains(msg, "nothing new was recorded by this call") {
		t.Fatalf("answer = %d %q %q, want 500 internal_error: %s", w.Code, code, msg, w.Body.String())
	}
	if n := len(f.parentCopies(t)); n != 0 {
		t.Errorf("parent copies = %d, want 0", n)
	}
	if n := bcRollupVerdicts(t, f); n != 0 {
		t.Errorf("merge_verdict_recorded rows = %d, want 0", n)
	}
	if merger.called != 0 {
		t.Errorf("merger called %d times, want 0", merger.called)
	}
	lines := warnLinesWith(f.logs, "event=concern_late_import_failed")
	if len(lines) != 1 || !strings.Contains(lines[0], "site=merge_reconcile") {
		t.Errorf("want one WARN keyed concern_late_import_failed with site merge_reconcile; logs:\n%s", f.logs.String())
	}
}

// (10) The reconcile runs UNDER the merge-intent lock: held is true only
// inside fn, so a pre-lock call would record held=false.
func TestMergeRun_InlineBlockingConcern_ReconcileRunsUnderMergeIntentLock(t *testing.T) {
	f, merger := newBCRollup(t)
	f.seedChildConcern(t, 0, "high", "uncopied child high")
	q := newBCLockProbe()
	f.s.cfg.MergeQueue = q
	cr := &bcConcernRepo{fakeConcernRepo: f.cr, held: q.held, events: q.events}
	f.s.cfg.ConcernRepo = cr
	w := bcRollupPost(t, f)
	if got := cr.imports(); len(got) != 1 || !got[0] {
		t.Fatalf("ImportRolledUp held flags = %v, want exactly one call under the lock", got)
	}
	if got := strings.Join(q.events.get(), ","); got != "lock,import" {
		t.Errorf("events = %q, want lock,import", got)
	}
	bcRequireBlocking(t, w)
	if merger.called != 0 {
		t.Errorf("merger called %d times, want 0", merger.called)
	}
}

// --- delegated may_merge arm ----------------------------------------------

// bcDelegated is a may_merge-Met run (seedMergeReadyRun) plus one SUCCEEDED
// decomposition child holding an open HIGH with no parent copy, so the
// evaluator's own-ledger read sees nothing.
type bcDelegated struct {
	s      *Server
	repo   *autoDriveRepo
	au     *auditFake
	cr     *fakeConcernRepo
	merger *fakeMerger
	runID  uuid.UUID
	src    *concern.Concern
}

func newBCDelegated(t *testing.T, withChildHigh bool) *bcDelegated {
	t.Helper()
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := seedMergeReadyRun(t, s, repo, au)
	d := &bcDelegated{s: s, repo: repo, au: au, cr: s.cfg.ConcernRepo.(*fakeConcernRepo), merger: merger, runID: runID}
	childID, childStage := uuid.New(), uuid.New()
	idx := 0
	repo.mu.Lock()
	repo.runs[childID] = &run.Run{ID: childID, Repo: "x/y", WorkflowID: "feature_change", State: run.StateSucceeded, DecomposedFrom: &runID, SliceIndex: &idx}
	repo.stagesByRun[childID] = []*run.Stage{{ID: childStage, RunID: childID, Type: run.StageTypeImplement, State: run.StageStateSucceeded}}
	repo.mu.Unlock()
	if withChildHigh {
		rows, err := d.cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
			RunID: childID, StageID: childStage, StageKind: concern.StageKindImplement,
			ReviewerModel: "gpt-6-astra", ReviewerRole: concern.ReviewerRoleStandard, OriginReviewSequence: 100,
			Concerns: []concern.RaisedConcern{{Severity: "high", Category: "correctness", Note: "uncopied child high"}},
		})
		if err != nil {
			t.Fatalf("seed child concern: %v", err)
		}
		d.src = rows[0]
	}
	return d
}

func (d *bcDelegated) drive(t *testing.T) AutoDriveOutcome {
	t.Helper()
	out, err := d.s.AutoDriveRunGate(context.Background(), getRun(t, d.repo, d.runID), campaignOperatorIdentity(), d.merger, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	return out
}

func (d *bcDelegated) parentCopies(t *testing.T) []*concern.Concern {
	t.Helper()
	rows, err := d.cr.ListByRun(context.Background(), d.runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []*concern.Concern
	for _, r := range rows {
		if r.RolledUpFrom != nil {
			out = append(out, r)
		}
	}
	return out
}

func bcRequireObserveOnly(t *testing.T, out AutoDriveOutcome, noteHas string) {
	t.Helper()
	if out.Acted || out.DecisionRequired || out.Paged {
		t.Fatalf("outcome = %+v, want observe-only", out)
	}
	if !strings.Contains(out.Note, noteHas) {
		t.Fatalf("note = %q, want it to contain %q", out.Note, noteHas)
	}
}

// (11) The Q1 gap: an uncopied succeeded-child HIGH holds the delegated merge.
func TestAutoDriveMerge_BlockingConcern_UncopiedChildHighObserveOnly(t *testing.T) {
	d := newBCDelegated(t, true)
	out := d.drive(t)
	copies := d.parentCopies(t)
	if len(copies) != 1 || copyOf(copies, d.src) == nil {
		t.Fatalf("parent copies = %v, want exactly one copy of the child HIGH", copies)
	}
	bcRequireObserveOnly(t, out, "blocking concern open")
	if !strings.Contains(out.Note, copies[0].ID.String()) {
		t.Errorf("note = %q, want it to name the copy %s", out.Note, copies[0].ID)
	}
	if d.merger.called != 0 {
		t.Errorf("merger called %d times, want 0", d.merger.called)
	}
}

// Control: with no child HIGH the same fixture merges, so (11)'s hold is the
// child concern and nothing else.
func TestAutoDriveMerge_BlockingConcern_NoChildHighMerges(t *testing.T) {
	d := newBCDelegated(t, false)
	if out := d.drive(t); !out.Acted || out.Action != delegation.ActionMerge {
		t.Fatalf("outcome = %+v, want acted merge", out)
	}
	if d.merger.called != 1 {
		t.Errorf("merger called %d times, want 1", d.merger.called)
	}
}

// (12) A reconcile failure is observe-only (fail closed).
func TestAutoDriveMerge_BlockingConcern_LegErrorObserveOnly(t *testing.T) {
	d := newBCDelegated(t, true)
	d.cr.importErr = errors.New("insert failed")
	bcRequireObserveOnly(t, d.drive(t), "blocking concerns could not be read or reconciled (fail-closed)")
	if d.merger.called != 0 {
		t.Errorf("merger called %d times, want 0", d.merger.called)
	}
}

// (13) The delegated reconcile runs UNDER the merge-intent lock.
func TestAutoDriveMerge_BlockingConcern_ReconcileRunsUnderMergeIntentLock(t *testing.T) {
	d := newBCDelegated(t, true)
	q := newBCLockProbe()
	d.s.cfg.MergeQueue = q
	cr := &bcConcernRepo{fakeConcernRepo: d.cr, held: q.held, events: q.events}
	d.s.cfg.ConcernRepo = cr
	out := d.drive(t)
	if got := cr.imports(); len(got) != 1 || !got[0] {
		t.Fatalf("ImportRolledUp held flags = %v, want exactly one call under the lock", got)
	}
	if got := strings.Join(q.events.get(), ","); got != "lock,import" {
		t.Errorf("events = %q, want lock,import", got)
	}
	bcRequireObserveOnly(t, out, "blocking concern open")
	if d.merger.called != 0 {
		t.Errorf("merger called %d times, want 0", d.merger.called)
	}
}

// (14) A HIGH raised at lock time, after the evaluator read an empty ledger,
// holds the delegated merge (the TOCTOU the pre-lock condition read misses).
func TestAutoDriveMerge_BlockingConcern_HighRaisedAtLockHolds(t *testing.T) {
	d := newBCDelegated(t, false)
	stages := d.repo.stagesFor(d.runID)
	q := newBCLockProbe()
	q.onLock = func() {
		if _, err := d.cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
			RunID: d.runID, StageID: stages[1].ID, StageKind: concern.StageKindImplement, ReviewerModel: "gpt-6-astra",
			OriginReviewSequence: 1, Concerns: []concern.RaisedConcern{{Severity: "high", Category: "correctness", Note: "late"}},
		}); err != nil {
			t.Errorf("raise at lock: %v", err)
		}
	}
	d.s.cfg.MergeQueue = q
	bcRequireObserveOnly(t, d.drive(t), "blocking concern open")
	if d.merger.called != 0 {
		t.Errorf("merger called %d times, want 0", d.merger.called)
	}
	if got := strings.Join(q.events.get(), ","); strings.Contains(got, "claim") {
		t.Errorf("events = %q, want no dispatch claim", got)
	}
}

// C8: an unwired concern repository skips the delegated leg with one WARN.
// AutoDriveRunGate itself reads the ledger first, so the leg is driven
// through delegatedMergeLocked, the lock fn it runs in.
func TestAutoDriveMerge_BlockingConcern_NilConcernRepoSkipsWithWarn(t *testing.T) {
	d := newBCDelegated(t, false)
	var logs bytes.Buffer
	d.s.cfg.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d.s.cfg.ConcernRepo = nil
	out, err := d.s.delegatedMergeLocked(context.Background(), getRun(t, d.repo, d.runID), d.repo.stagesFor(d.runID), d.merger, mergeCandidateGateResult{})
	if err != nil || !out.Acted {
		t.Fatalf("outcome = %+v / %v, want acted merge", out, err)
	}
	if n := strings.Count(logs.String(), "the blocking-concern hold is skipped"); n != 1 {
		t.Errorf("skip WARN lines = %d, want 1; logs:\n%s", n, logs.String())
	}
	if d.merger.called != 1 {
		t.Errorf("merger called %d times, want 1", d.merger.called)
	}
}

// --- cross-layer (real Postgres) -------------------------------------------

// TestMergeRunBlockingConcernPG_CrossLayer crosses the HTTP handler, the leg,
// the real Postgres concern store and the real mergequeue.PostgresStore
// merge-intent lock. Deliberately excluded from every counterfactual
// selector: its worlds are built inside t.Run subtests, so a skipped pgtest
// reads GREEN (see the file comment).
func TestMergeRunBlockingConcernPG_CrossLayer(t *testing.T) {
	t.Run("uncopied child HIGH nests the import under the real lock", func(t *testing.T) {
		ctx := context.Background()
		w, m := newMQEWorld(t)
		r := w.verified("bcchild", "h1")
		idx := 0
		inst := int64(99)
		child, err := w.repo.CreateRun(ctx, run.CreateRunParams{
			Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
			InstallationID: &inst, DecomposedFrom: &r.id, SliceIndex: &idx,
		})
		if err != nil {
			t.Fatalf("create child: %v", err)
		}
		cst, err := w.repo.CreateStage(ctx, run.CreateStageParams{RunID: child.ID, Sequence: 1, Type: run.StageTypeImplement,
			ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code"})
		if err != nil {
			t.Fatalf("create child stage: %v", err)
		}
		w.setStage(cst.ID, run.StageStateSucceeded)
		w.exec(`UPDATE runs SET state = 'succeeded' WHERE id = $1`, child.ID)
		cr := concern.NewPostgresRepository(w.pool)
		if _, err := cr.InsertRaised(ctx, concern.InsertRaisedParams{
			RunID: child.ID, StageID: cst.ID, StageKind: concern.StageKindImplement, ReviewerModel: "gpt-6-astra",
			ReviewerRole: concern.ReviewerRoleStandard, OriginReviewSequence: 1,
			Concerns: []concern.RaisedConcern{{Severity: "high", Category: "correctness", Note: "uncopied child high"}},
		}); err != nil {
			t.Fatalf("seed child concern: %v", err)
		}

		resp := w.mergeRun(r)
		rows, err := cr.ListByRun(ctx, r.id)
		if err != nil {
			t.Fatal(err)
		}
		var copies []*concern.Concern
		for _, c := range rows {
			if c.RolledUpFrom != nil {
				copies = append(copies, c)
			}
		}
		if len(copies) != 1 {
			t.Fatalf("parent copies = %d, want exactly 1 (%s)", len(copies), resp.Body.String())
		}
		bcRequireBlocking(t, resp, copies[0].ID.String())
		w.requireNothingRecorded(r, m)
	})
	t.Run("direct HIGH refuses, waive clears", func(t *testing.T) {
		ctx := context.Background()
		w, m := newMQEWorld(t)
		r := w.verified("bcdirect", "h1")
		w.raiseConcern(r, "high", "")
		resp := w.mergeRun(r)
		bcRequireBlocking(t, resp)
		w.requireNothingRecorded(r, m)

		cr := concern.NewPostgresRepository(w.pool)
		open, err := cr.ListOpenByRun(ctx, r.id)
		if err != nil || len(open) != 1 {
			t.Fatalf("open concerns = %v / %v, want 1", open, err)
		}
		if _, err := cr.ApplyHumanDisposition(ctx, open[0].ID, concern.StateWaived, "accepted", concern.HumanDisposition{
			Outcome: concern.OutcomeWaived, Judgement: concern.JudgementNoDefect, ActorKind: string(audit.ActorUser)}); err != nil {
			t.Fatalf("waive: %v", err)
		}
		if resp := w.mergeRun(r); resp.Code != http.StatusOK {
			t.Fatalf("re-merge = %d %s, want 200", resp.Code, resp.Body.String())
		}
		if n := len(w.rows(r, CategoryMergeVerdictRecorded)); n != 1 {
			t.Errorf("merge_verdict_recorded rows = %d, want 1", n)
		}
		if m.inlineCalls() != 1 {
			t.Errorf("inline dispatches = %d, want 1", m.inlineCalls())
		}
	})
}

// --- fix-up pass: already-merged escape, lock deadline, no implement stage --

// The already-merged escape's LIVE-forge arm on a FIRST post (no chain row):
// the hold answers 200 already_merged from its own observe, so
// merge_observation_recorded is true exactly when this call appended the
// row, a failed append names record-merge-observation, and neither appends a
// verdict nor dispatches the merger.
func TestMergeRun_InlineBlockingConcern_AlreadyMergedLiveForge(t *testing.T) {
	setup := func(t *testing.T) *bcInline {
		t.Helper()
		b := newBCInline(t)
		b.s.cfg.PRStateReader = &fakePRStateReader{pr: mergedPR()}
		b.repo.mu.Lock()
		b.repo.runs[b.runID].Repo = observeMergeRepo
		b.repo.mu.Unlock()
		b.raise(t, "high", "")
		return b
	}
	t.Run("observation recorded by this call", func(t *testing.T) {
		b := setup(t)
		resp := decodeMergeResponse(t, b.post(t))
		if !resp.AlreadyMerged || resp.MergeQueued || !resp.MergeObservationRecorded {
			t.Fatalf("response = %+v, want already_merged, not queued, merge_observation_recorded:true", resp)
		}
		if !strings.Contains(resp.Message, "This call recorded the merge observation") {
			t.Errorf("message = %q, want it to say this call recorded the observation", resp.Message)
		}
		if n := len(mergeObservationRows(b.au)); n != 1 {
			t.Errorf("merge_observation_recorded rows = %d, want 1", n)
		}
		if n := len(mergeVerdictRows(b.au)); n != 0 || b.merger.called != 0 {
			t.Errorf("verdict rows = %d, dispatches = %d; want 0 and 0", n, b.merger.called)
		}
	})
	t.Run("observation append fails", func(t *testing.T) {
		b := setup(t)
		fa := &failCategoryAudit{auditFake: b.au, failCategory: CategoryMergeObservationRecorded}
		b.s.cfg.AuditRepo = fa
		w := b.post(t)
		resp := decodeMergeResponse(t, w)
		if !resp.AlreadyMerged || resp.MergeQueued || resp.MergeObservationRecorded {
			t.Fatalf("response = %+v, want already_merged, not queued, merge_observation_recorded:false", resp)
		}
		if !strings.Contains(resp.Message, "record-merge-observation") {
			t.Errorf("message = %q, want it to name record-merge-observation", resp.Message)
		}
		if n := len(mergeVerdictRows(b.au)); n != 0 || b.merger.called != 0 {
			t.Errorf("verdict rows = %d, dispatches = %d; want 0 and 0 (no dispatch with a blocking concern open)", n, b.merger.called)
		}
	})
}

// A PR that already merged keeps the already_merged answer on the hold's
// ERROR path too: a failed in-lock ledger read does not turn a resume into a
// retryable 500.
func TestMergeRun_InlineBlockingConcern_ErrorPathAlreadyMerged(t *testing.T) {
	b := newBCInline(t)
	q := newBCLockProbe()
	b.s.cfg.MergeQueue = q
	b.s.cfg.ConcernRepo = &bcConcernRepo{fakeConcernRepo: b.cr, held: q.held, events: q.events, failRun: b.runID}
	seedReviewEntry(t, b.au, b.runID, 3, CategoryPRMerged, map[string]any{"pr_url": mergePR})
	resp := bcRequireMerged(t, b.post(t))
	if !resp.AlreadyMerged || resp.MergeQueued {
		t.Errorf("response = %+v, want already_merged:true merge_queued:false", resp)
	}
	b.requireNothingRecorded(t)
}

// A contended import that outlives the lock fn's deadline fails closed on
// both paths: the reconcile returns the context error, the inline merge
// answers 500 with nothing recorded and the delegated arm is observe-only.
func TestMergeRun_InlineBlockingConcern_LockDeadlineFailsClosed(t *testing.T) {
	f, merger := newBCRollup(t)
	f.seedChildConcern(t, 0, "high", "uncopied child high")
	q := newBCLockProbe()
	q.fnTimeout = 50 * time.Millisecond
	f.s.cfg.MergeQueue = q
	f.s.cfg.ConcernRepo = &bcConcernRepo{fakeConcernRepo: f.cr, held: q.held, events: q.events, blockImport: true}
	w := bcRollupPost(t, f)
	if code, msg, _ := bcErrCode(t, w); w.Code != http.StatusInternalServerError || code != "internal_error" ||
		!strings.Contains(msg, "nothing new was recorded by this call") {
		t.Fatalf("answer = %d %q %q, want 500 internal_error: %s", w.Code, code, msg, w.Body.String())
	}
	if n := len(f.parentCopies(t)); n != 0 {
		t.Errorf("parent copies = %d, want 0", n)
	}
	if n := bcRollupVerdicts(t, f); n != 0 || merger.called != 0 {
		t.Errorf("verdict rows = %d, dispatches = %d; want 0 and 0", n, merger.called)
	}
}

func TestAutoDriveMerge_BlockingConcern_LockDeadlineObserveOnly(t *testing.T) {
	d := newBCDelegated(t, true)
	q := newBCLockProbe()
	q.fnTimeout = 50 * time.Millisecond
	d.s.cfg.MergeQueue = q
	d.s.cfg.ConcernRepo = &bcConcernRepo{fakeConcernRepo: d.cr, held: q.held, events: q.events, blockImport: true}
	bcRequireObserveOnly(t, d.drive(t), "blocking concerns could not be read or reconciled (fail-closed)")
	if d.merger.called != 0 {
		t.Errorf("merger called %d times, want 0", d.merger.called)
	}
}

// A decomposed parent with a SUCCEEDED child and no implement stage fails
// closed (500) even when no child holds a concern: the reconcile reads the
// parent stages before it looks for sources, and a parent that reached merge
// always has the stage the fan-in attached copies to. With the stage, the
// same fixture merges.
func TestMergeRun_InlineBlockingConcern_NoImplementStageFailsClosed(t *testing.T) {
	for name, withStage := range map[string]bool{"no implement stage": false, "implement stage control": true} {
		t.Run(name, func(t *testing.T) {
			b := newBCInline(t)
			childID, idx := uuid.New(), 0
			b.repo.mu.Lock()
			if !withStage {
				b.repo.stagesByRun[b.runID] = nil
			}
			b.repo.runs[childID] = &run.Run{ID: childID, Repo: "x/y", WorkflowID: "feature_change", State: run.StateSucceeded, DecomposedFrom: &b.runID, SliceIndex: &idx}
			b.repo.mu.Unlock()
			w := b.post(t)
			if withStage {
				bcRequireMerged(t, w)
				return
			}
			if code, _, _ := bcErrCode(t, w); w.Code != http.StatusInternalServerError || code != "internal_error" {
				t.Fatalf("answer = %d %q, want 500 internal_error: %s", w.Code, code, w.Body.String())
			}
			b.requireNothingRecorded(t)
		})
	}
}

// --- merge-on-pass (#5261) -------------------------------------------------
//
// Every test below runs on the real-Postgres mqmWorld (merge_queue_merge_test.go,
// used read-only) and builds that world in its TOP-LEVEL body, with no t.Run,
// so a run with no database skips the top-level test (no_tests_matched to the
// counterfactual classifier), never a passing parent. The concern, failure or
// phase change each test needs is made inside the lock wrapper's onLock hook,
// AFTER the pre-lock predicate (mergeQueueMergeOnPass step 4) read an empty,
// readable ledger, so only mergeOnPassLocked step 4b can see it.

// mqmConcernLockQueue runs onLock ONCE, only for runID, then takes the REAL
// store's merge-intent lock with fn wrapped to set held for fn's duration.
// Every other method is the embedded store.
type mqmConcernLockQueue struct {
	mergequeue.Store
	runID  uuid.UUID
	onLock func()
	held   *atomic.Bool
	once   *sync.Once
}

func (q mqmConcernLockQueue) WithMergeIntentLock(ctx context.Context, runID uuid.UUID, fn func(context.Context) error) error {
	if runID == q.runID && q.onLock != nil {
		q.once.Do(q.onLock)
	}
	return q.Store.WithMergeIntentLock(ctx, runID, func(lctx context.Context) error {
		q.held.Store(true)
		defer q.held.Store(false)
		return fn(lctx)
	})
}

// mqmFailingListConcerns fails ListOpenByRun only while fail is set.
type mqmFailingListConcerns struct {
	concern.Repository
	fail *atomic.Bool
}

func (c mqmFailingListConcerns) ListOpenByRun(ctx context.Context, runID uuid.UUID) ([]*concern.Concern, error) {
	if c.fail.Load() {
		return nil, errors.New("injected concern ledger read failure")
	}
	return c.Repository.ListOpenByRun(ctx, runID)
}

// mqmImportProbe records whether the merge-intent lock was held at each
// ImportRolledUp, then delegates.
type mqmImportProbe struct {
	concern.Repository
	held    *atomic.Bool
	mu      sync.Mutex
	sawHeld []bool
}

func (c *mqmImportProbe) ImportRolledUp(ctx context.Context, p concern.ImportRolledUpParams) ([]*concern.Concern, error) {
	c.mu.Lock()
	c.sawHeld = append(c.sawHeld, c.held.Load())
	c.mu.Unlock()
	return c.Repository.ImportRolledUp(ctx, p)
}

func (c *mqmImportProbe) imports() []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bool(nil), c.sawHeld...)
}

// mqmFailingGetRun fails GetRun only while fail is set.
type mqmFailingGetRun struct {
	run.Repository
	fail *atomic.Bool
}

func (r mqmFailingGetRun) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	if r.fail.Load() {
		return nil, errors.New("injected run read failure")
	}
	return r.Repository.GetRun(ctx, id)
}

// bcqLockAt installs mqmConcernLockQueue for r with onLock and returns its
// held flag.
func (w *mqmWorld) bcqLockAt(r *mcqRun, onLock func()) *atomic.Bool {
	held := &atomic.Bool{}
	w.srv.cfg.MergeQueue = mqmConcernLockQueue{Store: w.store, runID: r.id, onLock: onLock, held: held, once: &sync.Once{}}
	return held
}

// bcqOpenIDs reads r's open concern ids back from the real ledger.
func (w *mqmWorld) bcqOpenIDs(r *mcqRun) []string {
	w.t.Helper()
	open, err := concern.NewPostgresRepository(w.pool).ListOpenByRun(context.Background(), r.id)
	if err != nil {
		w.t.Fatalf("list open concerns: %v", err)
	}
	ids := make([]string, 0, len(open))
	for _, c := range open {
		ids = append(ids, c.ID.String())
	}
	return ids
}

// bcqBlockingDetail is the not_merge_eligible drop detail naming ids.
func bcqBlockingDetail(ids ...string) string {
	return mergeOnPassNotEligibleDetail([]mergeQueueIneligibility{{Code: mergeQueueReasonBlockingConcernOpen,
		Detail: "open blocking concern(s) (high severity or first_officer provenance): " + strings.Join(ids, ", ")}})
}

// bcqRequireNothingWritten asserts no verdict, no authorization row, no merged
// row, no D1 claim on either ledger, no pinned merge, and the entry still live
// in phase.
func (w *mqmWorld) bcqRequireNothingWritten(r *mcqRun, phase mergequeue.Phase) {
	w.t.Helper()
	w.requireNoMergeWrites(r)
	if _, claimed, err := w.srv.mergeDispatchLedger().MergeDispatch(context.Background(), r.id); err != nil || claimed {
		w.t.Fatalf("store dispatch claim present = %v (%v), want none", claimed, err)
	}
	if e := w.active(r); e.State != mergequeue.StateLive || e.Phase != phase {
		w.t.Fatalf("entry = %s/%s, want live/%s", e.State, e.Phase, phase)
	}
}

// TestMergeOnPass_BlockingConcern_RaisedAtLockDrops pins #5261: a reviewer
// HIGH, and a first_officer MEDIUM, raised after the pre-lock predicate read
// the ledger but before the lock drops the phase-passed entry
// not_merge_eligible at step 4b, with step 4's detail byte for byte, writing
// nothing. The HIGH row runs on the declared approval gate with a qualifying
// live forge approval, so a 4b placed after the step-6b fold would leave a
// merge_queue_authorized row. Each row builds its world in this top-level
// body (no t.Run).
func TestMergeOnPass_BlockingConcern_RaisedAtLockDrops(t *testing.T) {
	for _, row := range []struct {
		name, spec, severity, provenance string
		declared                         bool
	}{
		{name: "reviewer high on the declared approval gate", spec: mqeSpecYAML, severity: "high", declared: true},
		{name: "first_officer medium", spec: mcvUndelegatedSpecYAML, severity: "medium", provenance: concern.ProvenanceFirstOfficer},
	} {
		t.Logf("row: %s", row.name)
		w := newMQMWorld(t)
		r, req := w.ready("bcq-raise", "h1", row.spec)
		if row.declared {
			w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "APPROVED", "h1"))
			w.gh.set(func() { w.gh.reviewsStatus[r.pr] = 500 })
		}
		w.bcqLockAt(r, func() { w.raiseConcern(r, row.severity, row.provenance) })

		out := w.decide(req)
		requireOutcome(t, out, mergeOnPassSettle, mergeCandidateQueueReasonNotMergeEligible)
		ids := w.bcqOpenIDs(r)
		if len(ids) != 1 {
			t.Fatalf("[%s] open concerns = %v, want the one raised at the lock", row.name, ids)
		}
		if out.State != mergequeue.StateDropped || out.Detail != bcqBlockingDetail(ids[0]) || !strings.Contains(out.Detail, ids[0]) {
			t.Fatalf("[%s] outcome = %s %q, want dropped %q", row.name, out.State, out.Detail, bcqBlockingDetail(ids[0]))
		}
		w.bcqRequireNothingWritten(r, mergequeue.PhasePassed)

		// The same concern, now on the ledger before the lock, drops at the
		// pre-lock step 4 with the identical detail.
		pre := w.decide(req)
		requireOutcome(t, pre, mergeOnPassSettle, mergeCandidateQueueReasonNotMergeEligible)
		if pre.Detail != out.Detail {
			t.Fatalf("[%s] pre-lock drop detail %q != in-lock drop detail %q", row.name, pre.Detail, out.Detail)
		}
	}
}

// TestMergeOnPass_BlockingConcern_MediumAtLockMerges is the severity control:
// a reviewer MEDIUM raised at the lock stays advisory, so the same fixture
// merges (the drop rows above are caused by the concern).
func TestMergeOnPass_BlockingConcern_MediumAtLockMerges(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("bcq-med", "h1", mcvUndelegatedSpecYAML)
	w.bcqLockAt(r, func() { w.raiseConcern(r, "medium", "") })
	requireOutcome(t, w.decide(req), mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
	w.requireMerges(1)
	if ids := w.bcqOpenIDs(r); len(ids) != 1 {
		t.Fatalf("open concerns = %v, want the medium raised at the lock", ids)
	}
}

// TestMergeOnPass_BlockingConcern_UncopiedChildHighAtLockDrops: a succeeded
// decomposition child's HIGH with no parent copy, seeded at the lock, is
// imported by step 4b's reconcile and drops the entry; every ImportRolledUp
// ran with the merge-intent lock held (lock order: intent lock first, row
// locks second).
func TestMergeOnPass_BlockingConcern_UncopiedChildHighAtLockDrops(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("bcq-child", "h1", mcvUndelegatedSpecYAML)
	var src *concern.Concern
	held := w.bcqLockAt(r, func() { src = mqeSeedLostImport(t, w.mcqFixture, r) })
	probe := &mqmImportProbe{Repository: concern.NewPostgresRepository(w.pool), held: held}
	w.srv.cfg.ConcernRepo = probe

	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassSettle, mergeCandidateQueueReasonNotMergeEligible)
	var copies []*concern.Concern
	for _, c := range mqeParentConcerns(t, w.mcqFixture, r) {
		if c.RolledUpFrom != nil {
			copies = append(copies, c)
		}
	}
	if len(copies) != 1 || *copies[0].RolledUpFrom != src.ID {
		t.Fatalf("parent copies = %+v, want exactly one copy of %s", copies, src.ID)
	}
	if out.State != mergequeue.StateDropped || out.Detail != bcqBlockingDetail(copies[0].ID.String()) {
		t.Fatalf("outcome = %s %q, want dropped naming the copy %s", out.State, out.Detail, copies[0].ID)
	}
	w.bcqRequireNothingWritten(r, mergequeue.PhasePassed)
	saw := probe.imports()
	if len(saw) == 0 {
		t.Fatal("ImportRolledUp never ran: the in-lock reconcile did not import the child HIGH")
	}
	for i, h := range saw {
		if !h {
			t.Fatalf("ImportRolledUp #%d ran without the merge-intent lock held (%v)", i, saw)
		}
	}
}

// TestMergeOnPass_BlockingConcern_ReadFailureAtLockHolds: a ledger read that
// fails only under the lock holds not_mergeable_yet with step 4's
// undeterminable text and no expiry yield, writing nothing.
func TestMergeOnPass_BlockingConcern_ReadFailureAtLockHolds(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("bcq-readfail", "h1", mcvUndelegatedSpecYAML)
	fail := &atomic.Bool{}
	w.srv.cfg.ConcernRepo = mqmFailingListConcerns{Repository: concern.NewPostgresRepository(w.pool), fail: fail}
	w.bcqLockAt(r, func() { fail.Store(true) })

	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassHeld, mergeOnPassReasonNotMergeable)
	if !strings.HasPrefix(out.Detail, "undeterminable: concerns: ") || out.ExpiryYield != nil {
		t.Fatalf("hold detail %q expiry %+v, want the undeterminable concerns text and no expiry yield", out.Detail, out.ExpiryYield)
	}
	w.bcqRequireNothingWritten(r, mergequeue.PhasePassed)
}

// TestMergeOnPass_BlockingConcern_RunReadFailureAtLockHolds: step 4b re-reads
// the run under the lock; a failed re-read holds not_mergeable_yet (no expiry
// yield), writing nothing.
func TestMergeOnPass_BlockingConcern_RunReadFailureAtLockHolds(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("bcq-runfail", "h1", mcvUndelegatedSpecYAML)
	fail := &atomic.Bool{}
	w.srv.cfg.RunRepo = mqmFailingGetRun{Repository: w.repo, fail: fail}
	w.bcqLockAt(r, func() { fail.Store(true) })

	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassHeld, mergeOnPassReasonNotMergeable)
	if !strings.HasPrefix(out.Detail, "undeterminable: run: ") || out.ExpiryYield != nil {
		t.Fatalf("hold detail %q expiry %+v, want the undeterminable run text and no expiry yield", out.Detail, out.ExpiryYield)
	}
	w.bcqRequireNothingWritten(r, mergequeue.PhasePassed)
}

// TestMergeOnPass_BlockingConcern_TerminalRunAtLockHolds: a run that went
// terminal between the pre-lock read and the lock holds not_mergeable_yet at
// step 4b's in-lock re-read (no in-lock terminal settle exists there),
// writing nothing.
func TestMergeOnPass_BlockingConcern_TerminalRunAtLockHolds(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("bcq-terminal", "h1", mcvUndelegatedSpecYAML)
	w.bcqLockAt(r, func() { w.exec(`UPDATE runs SET state = 'cancelled' WHERE id = $1`, r.id) })

	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassHeld, mergeOnPassReasonNotMergeable)
	if !strings.Contains(out.Detail, "the run is cancelled at the in-lock re-read") {
		t.Fatalf("hold detail = %q, want the in-lock terminal re-read named", out.Detail)
	}
	w.bcqRequireNothingWritten(r, mergequeue.PhasePassed)
}

// TestMergeOnPass_BlockingConcern_MergingEntryAtLockHolds: a blocking concern
// on an entry step 1 accepted in phase MERGING holds not_mergeable_yet, never
// drops (the ADR-093 D8 resume owns any claim). Follow-through: the next pump
// routes the merging entry to mergeQueueResumeMerge, which with no claim
// returns it to passed, and the next decision drops it not_merge_eligible.
func TestMergeOnPass_BlockingConcern_MergingEntryAtLockHolds(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("bcq-merging", "h1", mcvUndelegatedSpecYAML)
	w.bcqLockAt(r, func() {
		w.exec(`UPDATE merge_candidate_queue_entries SET phase = 'merging' WHERE run_id = $1 AND state = 'live'`, r.id)
		w.raiseConcern(r, "high", "")
	})

	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassHeld, mergeOnPassReasonNotMergeable)
	if !strings.Contains(out.Detail, mergeQueueReasonBlockingConcernOpen) {
		t.Fatalf("hold detail = %q, want blocking_concern_open", out.Detail)
	}
	w.bcqRequireNothingWritten(r, mergequeue.PhaseMerging)

	w.pumpBase(r)
	w.requireState(r, mergequeue.StateDropped, mergeCandidateQueueReasonNotMergeEligible)
	w.requireMerges(0)
	if _, claimed, err := w.srv.mergeDispatchLedger().MergeDispatch(context.Background(), r.id); err != nil || claimed {
		t.Fatalf("store dispatch claim present = %v (%v), want none", claimed, err)
	}
}

// TestMergeOnPass_BlockingConcern_AlreadyMergedAtLockSettlesMerged: a PR
// that merged before the lock settles pr_merged at step 2 even with a HIGH
// raised at the lock; step 4b never refuses a merged PR.
func TestMergeOnPass_BlockingConcern_AlreadyMergedAtLockSettlesMerged(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("bcq-merged", "h1", mcvUndelegatedSpecYAML)
	w.bcqLockAt(r, func() {
		w.raiseConcern(r, "high", "")
		w.gh.set(func() { w.gh.prs[r.pr].merged, w.gh.prs[r.pr].state = true, "closed" })
	})
	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassSettle, mergeCandidateQueueReasonPRMerged)
	if out.State != mergequeue.StateMerged {
		t.Fatalf("state = %q, want merged", out.State)
	}
	w.requireMerges(0)
}

// TestMergeOnPass_BlockingConcern_PumpDropsAndAdmitsNext crosses pump ->
// decision -> the real Postgres merge-intent lock -> leg (d) -> reconcile ->
// the real concern store -> settle -> merge_candidate_queue_settled: a HIGH
// raised on A at the lock drops A not_merge_eligible, unmerged, and B is
// admitted (the line is not wedged).
func TestMergeOnPass_BlockingConcern_PumpDropsAndAdmitsNext(t *testing.T) {
	ctx := context.Background()
	w := newMQMWorld(t)
	a, _ := w.ready("bcq-pa", "h1", mcvUndelegatedSpecYAML)
	b := w.seed("bcq-pb", "b0", []byte(mcvUndelegatedSpecYAML))
	if _, err := w.store.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: b.id, BaseRef: mcqBase, HeadSHA: "b0"}); err != nil {
		t.Fatalf("enqueue B: %v", err)
	}
	w.bcqLockAt(a, func() { w.raiseConcern(a, "high", "") })

	w.pumpBase(a)
	w.requireState(a, mergequeue.StateDropped, mergeCandidateQueueReasonNotMergeEligible)
	settled := w.settledRows(a)
	if len(settled) != 1 || settled[0]["reason"] != mergeCandidateQueueReasonNotMergeEligible {
		t.Fatalf("A merge_candidate_queue_settled rows = %v, want one not_merge_eligible", settled)
	}
	w.requireMerges(0)
	if n := len(w.rows(a, CategoryMergeVerdictRecorded)); n != 0 {
		t.Fatalf("A merge_verdict_recorded rows = %d, want 0", n)
	}
	if _, claimed, err := w.srv.mergeDispatchLedger().MergeDispatch(ctx, a.id); err != nil || claimed {
		t.Fatalf("A dispatch claim present = %v (%v), want none", claimed, err)
	}
	if len(w.rows(b, CategoryMergeCandidateQueueAdmitted)) == 0 {
		t.Fatal("B was not admitted: the line is wedged")
	}
}
