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
	return fn(ctx)
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
}

func (c *bcConcernRepo) ImportRolledUp(ctx context.Context, p concern.ImportRolledUpParams) ([]*concern.Concern, error) {
	c.events.add("import")
	c.mu.Lock()
	c.importHeld = append(c.importHeld, c.held.Load())
	c.mu.Unlock()
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
// selector: a skipped pgtest reads GREEN.
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
