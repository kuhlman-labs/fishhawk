package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// ADR-093 slice 2 (E87.5 / #4296): merge-on-pass DECISION tests. They call
// mergeQueueMergeOnPass directly over the real Postgres queue, run and audit
// repositories and the mcq fake GitHub. Fixture variant (option b, all inside
// this file): a reverse proxy over the mcq fake that injects a per-PR
// mergeable_state into GET /pulls/{n}, a recording HeadPinnedMerger
// GateMerger, and a recording D1 dispatch ledger. Entries are seeded through
// Enqueue, AdmitNext and SQL only; mcqFixture helpers are reused read-only.

// mqmMerger is a recording head-pinned GateMerger. A successful pinned merge
// marks the mcq PR merged and closed.
type mqmMerger struct {
	f      *mcqFixture
	mu     sync.Mutex
	pinned []string
	inline int
	err    error
	// park, when set, is received from before the merge returns.
	park chan struct{}
	// entered, when set, is signalled on every pinned call.
	entered chan struct{}
}

func (m *mqmMerger) MergePullRequest(context.Context, *run.Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inline++
	return nil
}

func (m *mqmMerger) MergePullRequestAtHead(ctx context.Context, runRow *run.Run, head string) error {
	m.mu.Lock()
	m.pinned = append(m.pinned, head)
	err, park, entered := m.err, m.park, m.entered
	m.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if park != nil {
		<-park
	}
	if err != nil {
		return err
	}
	n := parsePRNumberFromURL(runRow.PullRequestURL)
	m.f.gh.set(func() {
		if pr := m.f.gh.prs[n]; pr != nil {
			pr.merged, pr.state = true, "closed"
		}
	})
	return nil
}

func (m *mqmMerger) calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.pinned...)
}

// mqmUnpinned is a GateMerger with no pinned merge.
type mqmUnpinned struct{}

func (mqmUnpinned) MergePullRequest(context.Context, *run.Run) error { return nil }

// mqmLedger is a recording D1 dispatch ledger: one claim per run unless
// permissive (then every claim is granted, isolating the merge-intent lock).
type mqmLedger struct {
	mu         sync.Mutex
	permissive bool
	claimErr   error
	claimed    map[uuid.UUID]string
	claims     []string
	failed     []string
	released   []string
	heads      map[uuid.UUID]string
	failedRun  map[uuid.UUID]bool
}

func newMQMLedger() *mqmLedger {
	return &mqmLedger{claimed: map[uuid.UUID]string{}, heads: map[uuid.UUID]string{}, failedRun: map[uuid.UUID]bool{}}
}

func (l *mqmLedger) MergeDispatch(_ context.Context, runID uuid.UUID) (mergequeue.DispatchClaim, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.claimed[runID]
	if !ok {
		return mergequeue.DispatchClaim{}, false, nil
	}
	st := mergequeue.DispatchDispatched
	if l.failedRun[runID] {
		st = mergequeue.DispatchFailed
	}
	return mergequeue.DispatchClaim{RunID: runID, State: st, Path: p, HeadSHA: l.heads[runID]}, true, nil
}

func (l *mqmLedger) ClaimMergeDispatch(_ context.Context, runID uuid.UUID, path, head string) (mergequeue.DispatchClaim, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.claimErr != nil {
		return mergequeue.DispatchClaim{}, false, l.claimErr
	}
	l.claims = append(l.claims, path+"@"+head)
	if p, ok := l.claimed[runID]; ok && !l.permissive {
		return mergequeue.DispatchClaim{RunID: runID, State: mergequeue.DispatchDispatched, Path: p, HeadSHA: head}, false, nil
	}
	l.claimed[runID] = path
	l.heads[runID] = head
	return mergequeue.DispatchClaim{RunID: runID, State: mergequeue.DispatchDispatched, Path: path, HeadSHA: head, ClaimedAt: time.Now()}, true, nil
}

func (l *mqmLedger) MarkMergeDispatchFailed(_ context.Context, runID uuid.UUID, cause string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failed = append(l.failed, cause)
	l.failedRun[runID] = true
	return nil
}

func (l *mqmLedger) ReleaseMergeDispatch(_ context.Context, c mergequeue.DispatchClaim) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.released = append(l.released, c.Path+"@"+c.HeadSHA)
	delete(l.claimed, c.RunID)
	return nil
}

// mqmAudit wraps the real audit repository: failCategory makes AppendChained
// fail for that category, onAppend runs before each append.
type mqmAudit struct {
	audit.Repository
	failCategory string
	onAppend     func(category string)
}

func (a *mqmAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if a.onAppend != nil {
		a.onAppend(p.Category)
	}
	if p.Category == a.failCategory {
		return nil, errors.New("injected append failure")
	}
	return a.Repository.AppendChained(ctx, p)
}

// mqmErrQueue fails ActiveForRun; every other method is the embedded store.
type mqmErrQueue struct{ mergequeue.Store }

func (mqmErrQueue) ActiveForRun(context.Context, uuid.UUID) (mergequeue.Status, bool, error) {
	return mergequeue.Status{}, false, errors.New("injected queue read failure")
}

// mqmWorld is one merge-on-pass world.
type mqmWorld struct {
	*mcqFixture
	merger *mqmMerger
	ledger *mqmLedger
	audit  *mqmAudit
	mu     sync.Mutex
	states map[int]string
	// prReadFail, when set, answers every GET /pulls/{n} 500.
	prReadFail bool
}

var mqmPullPath = regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/(\d+)$`)

// mqmTokens is a stateless installation-token provider: the concurrent
// decision tests read the PR from two goroutines, and the shared
// fakeTokenProvider's unguarded call bookkeeping would race (#3226).
type mqmTokens struct{}

func (mqmTokens) Token(context.Context, int64) (string, error) { return "ghs_t", nil }

func newMQMWorld(t *testing.T) *mqmWorld {
	t.Helper()
	w := &mqmWorld{mcqFixture: newMCQFixture(t), ledger: newMQMLedger(), states: map[int]string{}}
	w.merger = &mqmMerger{f: w.mcqFixture}
	orig := w.srv.cfg.GitHub
	target, err := url.Parse(orig.BaseURL)
	if err != nil {
		t.Fatalf("parse mcq url: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		m := mqmPullPath.FindStringSubmatch(resp.Request.URL.Path)
		if resp.Request.Method != http.MethodGet || m == nil || resp.StatusCode != http.StatusOK {
			return nil
		}
		n, _ := strconv.Atoi(m[1])
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		w.mu.Lock()
		fail := w.prReadFail
		w.mu.Unlock()
		if fail {
			resp.StatusCode, resp.Status = http.StatusInternalServerError, "500 Internal Server Error"
			resp.Body = io.NopCloser(bytes.NewReader([]byte(`{"message":"injected"}`)))
			resp.ContentLength = -1
			resp.Header.Del("Content-Length")
			return nil
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			return err
		}
		w.mu.Lock()
		body["mergeable_state"] = w.states[n]
		w.mu.Unlock()
		out, _ := json.Marshal(body)
		resp.Body = io.NopCloser(bytes.NewReader(out))
		resp.ContentLength = int64(len(out))
		resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
		return nil
	}
	ps := httptest.NewServer(proxy)
	t.Cleanup(ps.Close)
	w.srv.cfg.GitHub = &githubclient.Client{BaseURL: ps.URL, Tokens: mqmTokens{}, HTTP: orig.HTTP, AppJWT: orig.AppJWT}
	w.srv.cfg.GateMerger = w.merger
	w.audit = &mqmAudit{Repository: w.mcqFixture.audit}
	w.srv.cfg.AuditRepo = w.audit
	return w
}

func (w *mqmWorld) mergeable(r *mcqRun, state string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.states[r.pr] = state
}

// live seeds r's entry live in phase passed at head carrying merge intent,
// with its merge_queue_entered row, and returns the request.
func (w *mqmWorld) live(r *mcqRun, head string) mergeOnPassRequest {
	w.t.Helper()
	ctx := context.Background()
	anchor := w.mqeIntent(r, head)
	res, err := w.store.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: r.id, BaseRef: mcqBase, HeadSHA: head, MergeIntent: true})
	if err != nil {
		w.t.Fatalf("enqueue: %v", err)
	}
	if adm, err := w.store.AdmitNext(ctx, res.Entry.Base()); err != nil || !adm.Admitted {
		w.t.Fatalf("admit: %+v %v", adm, err)
	}
	w.exec(`UPDATE merge_candidate_queue_entries SET phase = 'passed' WHERE id = $1`, res.Entry.ID)
	return mergeOnPassRequest{Entry: w.active(r), IntentSequence: anchor.Sequence, Ledger: w.ledger}
}

func (w *mqmWorld) active(r *mcqRun) mergequeue.Entry {
	w.t.Helper()
	st, found, err := w.store.ActiveForRun(context.Background(), r.id)
	if err != nil || !found {
		w.t.Fatalf("active entry: found=%v err=%v", found, err)
	}
	return st.Entry
}

// ready seeds a run on spec at head, a base-advance head (so it needs a
// merge-candidate verify) passed there, with a live intent entry.
func (w *mqmWorld) ready(name, head, spec string) (*mcqRun, mergeOnPassRequest) {
	w.t.Helper()
	r := w.seed(name, head, []byte(spec))
	w.mqeRebased(r, "pre-"+head, head, head, false)
	w.mqePassed(r, head)
	return r, w.live(r, head)
}

func (w *mqmWorld) decide(req mergeOnPassRequest) mergeOnPassOutcome {
	return w.srv.mergeQueueMergeOnPass(context.Background(), req)
}

func requireOutcome(t *testing.T, got mergeOnPassOutcome, kind mergeOnPassKind, reason string) {
	t.Helper()
	if got.Kind != kind || got.Reason != reason {
		t.Fatalf("outcome = %s/%q (%s), want %s/%q", got.Kind, got.Reason, got.Detail, kind, reason)
	}
}

func (w *mqmWorld) requireMerges(n int) {
	w.t.Helper()
	if got := w.merger.calls(); len(got) != n {
		w.t.Fatalf("pinned merges = %v, want %d", got, n)
	}
}

func (w *mqmWorld) payload(e *audit.Entry) map[string]any {
	w.t.Helper()
	var p map[string]any
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		w.t.Fatalf("decode: %v", err)
	}
	return p
}

func TestMergeQueueMergeOnPass_HappyPathMergesPinnedAtVerifiedHead(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("ok", "h1", mcvUndelegatedSpecYAML)
	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
	if got := w.merger.calls(); len(got) != 1 || got[0] != "h1" {
		t.Fatalf("pinned merges = %v, want [h1]", got)
	}
	if w.merger.inline != 0 {
		t.Fatalf("unpinned MergePullRequest called %d times", w.merger.inline)
	}
	verdicts := w.rows(r, CategoryMergeVerdictRecorded)
	if len(verdicts) != 1 {
		t.Fatalf("merge_verdict_recorded rows = %d, want 1", len(verdicts))
	}
	vp := w.payload(verdicts[0])
	if vp["via"] != mergeVerdictViaMergeQueue || vp["verdict"] != "ship it at h1" || vp["head_sha"] != "h1" ||
		int64(vp["intent_sequence"].(float64)) != req.IntentSequence {
		t.Fatalf("verdict payload = %v", vp)
	}
	merged := w.rows(r, CategoryMergeQueueMerged)
	if len(merged) != 1 {
		t.Fatalf("merge_queue_merged rows = %d, want 1", len(merged))
	}
	mp := w.payload(merged[0])
	if mp["head_sha"] != "h1" || mp["verdict_head_sha"] != "h1" || int64(mp["verdict_sequence"].(float64)) != verdicts[0].Sequence {
		t.Fatalf("merged payload = %v", mp)
	}
	// D3: settled merged inside the lock, before it was released.
	if !out.Settled {
		t.Fatal("outcome not settled inside the lock")
	}
	w.requireState(r, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	if len(w.ledger.claims) != 1 || w.ledger.claims[0] != "queue@h1" {
		t.Fatalf("ledger claims = %v, want [queue@h1]", w.ledger.claims)
	}
}

// TestMergeQueueMergeOnPass_GateHolds pins each pre-forge gate: every row
// would merge (the merger accepts) but for its one control.
func TestMergeQueueMergeOnPass_GateHolds(t *testing.T) {
	cases := []struct {
		name   string
		mut    func(w *mqmWorld, req *mergeOnPassRequest)
		reason string
	}{
		{"no intent", func(_ *mqmWorld, req *mergeOnPassRequest) { req.Entry.MergeIntentAt = nil }, mergeOnPassReasonNoIntent},
		{"not live", func(_ *mqmWorld, req *mergeOnPassRequest) { req.Entry.State = mergequeue.StateHeld }, mergeOnPassReasonNotLive},
		{"not passed", func(_ *mqmWorld, req *mergeOnPassRequest) { req.Entry.Phase = mergequeue.PhaseVerifying }, mergeOnPassReasonNotPassed},
		{"unpinned seam", func(w *mqmWorld, _ *mergeOnPassRequest) { w.srv.cfg.GateMerger = mqmUnpinned{} }, mergeOnPassReasonNoPinnedMerge},
		{"no ledger", func(_ *mqmWorld, req *mergeOnPassRequest) { req.Ledger = nil }, mergeOnPassReasonLedgerUnavailable},
		{"claim error", func(w *mqmWorld, _ *mergeOnPassRequest) { w.ledger.claimErr = errors.New("db down") }, mergeOnPassReasonTransient},
		{"queue read error", func(w *mqmWorld, _ *mergeOnPassRequest) { w.srv.cfg.MergeQueue = mqmErrQueue{w.store} }, mergeOnPassReasonTransient},
		{"intent row missing", func(_ *mqmWorld, req *mergeOnPassRequest) { req.IntentSequence = 999999 }, mergeOnPassReasonIntentRowMissing},
		{"verify not passed", func(w *mqmWorld, req *mergeOnPassRequest) {
			w.mqeRebased(&mcqRun{id: req.Entry.RunID}, "h1", "hx", "hx", false)
			w.exec(`UPDATE merge_candidate_queue_entries SET anchored_head_sha = 'hx' WHERE id = $1`, req.Entry.ID)
			w.gh.set(func() { w.gh.prs[1].head = "hx" })
			req.Entry.AnchoredHeadSHA = "hx"
		}, mergeOnPassReasonVerifyNotPassed},
		{"checks pending", func(w *mqmWorld, _ *mergeOnPassRequest) {
			w.gh.set(func() {
				w.gh.checks["h1"] = `[{"id":1,"name":"ci","status":"in_progress","started_at":"2026-01-01T00:00:00Z","check_suite":{"id":1},"app":{"slug":"github-actions"}}]`
			})
		}, mergeOnPassReasonNotMergeable},
		{"nil snapshot optional red", func(w *mqmWorld, _ *mergeOnPassRequest) {
			w.gh.set(func() {
				w.gh.checks["h1"] = `[{"id":1,"name":"lint","status":"completed","conclusion":"failure","started_at":"2026-01-01T00:00:00Z","completed_at":"2026-01-01T00:01:00Z","check_suite":{"id":1},"app":{"slug":"github-actions"}}]`
			})
		}, mergeOnPassReasonNotMergeable},
		{"pr read error", func(w *mqmWorld, _ *mergeOnPassRequest) { w.gh.set(func() { delete(w.gh.prs, 1) }) }, mergeOnPassReasonTransient},
		{"base probe error", func(w *mqmWorld, _ *mergeOnPassRequest) { w.gh.set(func() { w.gh.compareStatus["h1"] = 500 }) }, mergeOnPassReasonTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, req := w.ready("g", "h1", mcvUndelegatedSpecYAML)
			tc.mut(w, &req)
			requireOutcome(t, w.decide(req), mergeOnPassHeld, tc.reason)
			w.requireMerges(0)
			if n := len(w.rows(r, CategoryMergeQueueMerged)); n != 0 {
				t.Fatalf("merge_queue_merged rows = %d, want 0", n)
			}
		})
	}
}

func TestMergeQueueMergeOnPass_NotRequiredMergesLikePassed(t *testing.T) {
	w := newMQMWorld(t)
	// No verify command: the head reads not_required (no verdict seeded).
	r := w.seed("nr", "h1", []byte(mcvNoVerifySpecYAML))
	req := w.live(r, "h1")
	requireOutcome(t, w.decide(req), mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
	w.requireMerges(1)
}

// TestMergeQueueMergeOnPass_SettleOutcomes pins the settle/readvance rows:
// none reaches the forge.
func TestMergeQueueMergeOnPass_SettleOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		mut    func(w *mqmWorld, r *mcqRun)
		kind   mergeOnPassKind
		state  mergequeue.State
		reason string
	}{
		{"pr already merged", func(w *mqmWorld, r *mcqRun) {
			w.gh.set(func() { w.gh.prs[r.pr].merged, w.gh.prs[r.pr].state = true, "closed" })
		}, mergeOnPassSettle, mergequeue.StateMerged, mergeCandidateQueueReasonPRMerged},
		{"pr closed", func(w *mqmWorld, r *mcqRun) { w.gh.set(func() { w.gh.prs[r.pr].state = "closed" }) },
			mergeOnPassSettle, mergequeue.StateDropped, mergeCandidateQueueReasonPRClosed},
		{"run terminal", func(w *mqmWorld, r *mcqRun) { w.exec(`UPDATE runs SET state = 'cancelled' WHERE id = $1`, r.id) },
			mergeOnPassSettle, mergequeue.StateDropped, mergeCandidateQueueReasonRunTerminal},
		{"verify failed", func(w *mqmWorld, r *mcqRun) {
			w.mqeAppend(r, CategoryMergeCandidateVerified, map[string]any{"head_sha": "h1", "result": mergeCandidateResultFailed})
		}, mergeOnPassSettle, mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed},
		{"blocking concern drops", func(w *mqmWorld, r *mcqRun) { w.raiseConcern(r, "high", "") },
			mergeOnPassSettle, mergequeue.StateDropped, mergeCandidateQueueReasonNotMergeEligible},
		{"head moved", func(w *mqmWorld, r *mcqRun) { w.gh.set(func() { w.gh.prs[r.pr].head = "h2" }) },
			mergeOnPassReadvance, "", mergeOnPassReasonHeadMoved},
		{"base moved", func(w *mqmWorld, _ *mcqRun) { w.gh.set(func() { w.gh.behind["h1"] = []string{"b9"} }) },
			mergeOnPassReadvance, "", mergeOnPassReasonBaseMoved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, req := w.ready("s", "h1", mcvUndelegatedSpecYAML)
			tc.mut(w, r)
			out := w.decide(req)
			requireOutcome(t, out, tc.kind, tc.reason)
			if out.State != tc.state {
				t.Fatalf("state = %q, want %q", out.State, tc.state)
			}
			w.requireMerges(0)
			if len(w.ledger.claims) != 0 {
				t.Fatalf("ledger claimed %v on a non-merging outcome", w.ledger.claims)
			}
		})
	}
}

func TestMergeQueueMergeOnPass_ChangesRequestedDropsNotMergeEligible(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("cr", "h1", mqeSpecYAML)
	w.gh.set(func() {
		w.gh.reviews[r.pr] = `[{"id":1,"user":{"login":"alice"},"state":"APPROVED"},{"id":2,"user":{"login":"bob"},"state":"CHANGES_REQUESTED"}]`
	})
	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassSettle, mergeCandidateQueueReasonNotMergeEligible)
	if !strings.Contains(out.Detail, mergeQueueReasonChangesRequested) {
		t.Fatalf("detail %q does not name changes_requested", out.Detail)
	}
	w.requireMerges(0)
}

// transferred seeds the D6 transfer world: intent at h0, a clean advance to
// h1, h1 passed, and alice's approval dismissed (no live approval), so the
// run is eligible only through the transferred basis.
func (w *mqmWorld) transferred() (*mcqRun, mergeOnPassRequest) {
	w.t.Helper()
	r := w.seed("tr", "h1", []byte(mqeSpecYAML))
	anchor := w.mqeIntent(r, "h0")
	w.mqeRebased(r, "h0", "h1", "h1", false)
	w.mqePassed(r, "h1")
	w.gh.set(func() { w.gh.reviews[r.pr] = mqeDismissed })
	res, err := w.store.Enqueue(context.Background(), mergequeue.EnqueueRequest{RunID: r.id, BaseRef: mcqBase, HeadSHA: "h1", MergeIntent: true})
	if err != nil {
		w.t.Fatalf("enqueue: %v", err)
	}
	if adm, err := w.store.AdmitNext(context.Background(), res.Entry.Base()); err != nil || !adm.Admitted {
		w.t.Fatalf("admit: %+v %v", adm, err)
	}
	w.exec(`UPDATE merge_candidate_queue_entries SET phase = 'passed' WHERE id = $1`, res.Entry.ID)
	return r, mergeOnPassRequest{Entry: w.active(r), IntentSequence: anchor.Sequence, Ledger: w.ledger}
}

func TestMergeQueueMergeOnPass_ForgeApprovalDismissedEjectsNamed(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.transferred()
	w.mergeable(r, "blocked")
	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassSettle, mergeCandidateQueueReasonForgeApprovalDismissed)
	if out.State != mergequeue.StateEjected || !strings.Contains(out.Detail, "self-approval is prohibited") ||
		!strings.Contains(out.Detail, "fishhawk_merge_run") {
		t.Fatalf("outcome = %+v", out)
	}
	w.requireMerges(0)
}

func TestMergeQueueMergeOnPass_TransferredBasis(t *testing.T) {
	cases := []struct {
		name   string
		state  string
		mut    func(w *mqmWorld, r *mcqRun)
		kind   mergeOnPassKind
		reason string
	}{
		{"undetermined approval holds", "unknown", nil, mergeOnPassHeld, mergeOnPassReasonApprovalUndetermined},
		// R3: the transferred-basis approval check fails closed, so a clean
		// mergeable_state (the check is undetermined off "blocked") holds.
		{"clean mergeable_state does not shortcut", "clean", nil, mergeOnPassHeld, mergeOnPassReasonApprovalUndetermined},
		{"reviews read error holds", "blocked", func(w *mqmWorld, r *mcqRun) {
			w.gh.set(func() { w.gh.reviewsStatus[r.pr] = 500 })
		}, mergeOnPassHeld, mergeOnPassReasonNotMergeable},
		// Determined before the lock (blocked, no dismissed review), then the
		// in-lock approval read fails after the verdict is recorded.
		{"in-lock approval read failure holds", "blocked", func(w *mqmWorld, r *mcqRun) {
			w.gh.set(func() { w.gh.reviews[r.pr] = `[]` })
			w.audit.onAppend = func(category string) {
				if category == CategoryMergeVerdictRecorded {
					w.mu.Lock()
					w.prReadFail = true
					w.mu.Unlock()
				}
			}
		}, mergeOnPassHeld, mergeOnPassReasonApprovalUndetermined},
		{"determined approval merges", "blocked", func(w *mqmWorld, r *mcqRun) {
			w.gh.set(func() { w.gh.reviews[r.pr] = `[]` })
		}, mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, req := w.transferred()
			w.mergeable(r, tc.state)
			if tc.mut != nil {
				tc.mut(w, r)
			}
			out := w.decide(req)
			requireOutcome(t, out, tc.kind, tc.reason)
			if tc.kind == mergeOnPassMerged {
				w.requireMerges(1)
				if out.ApprovalBasis != mergeQueueApprovalTransferred {
					t.Fatalf("approval basis = %q, want transferred", out.ApprovalBasis)
				}
				return
			}
			w.requireMerges(0)
			if len(w.ledger.claims) != 0 {
				t.Fatalf("ledger claimed %v on a held transferred basis", w.ledger.claims)
			}
			// The pre-lock check holds before the verdict is recorded; only the
			// in-lock row reaches the locked re-check.
			if n := len(w.rows(r, CategoryMergeVerdictRecorded)); (n == 1) != (tc.name == "in-lock approval read failure holds") {
				t.Fatalf("merge_verdict_recorded rows = %d: the hold came from the wrong check", n)
			}
		})
	}
}

func TestMergeQueueMergeOnPass_LiveBasisUndeterminedApprovalProceeds(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("lv", "h1", mqeSpecYAML)
	w.gh.set(func() { w.gh.reviews[r.pr] = mqeApprovedBy("alice") })
	w.mergeable(r, "unknown")
	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
	if out.ApprovalBasis != mergeQueueApprovalLive {
		t.Fatalf("approval basis = %q, want live", out.ApprovalBasis)
	}
}

// TestMergeQueueMergeOnPass_InLockRecheck pins approval condition (d): a
// change landing after the pre-lock checks (here, while the verdict is
// appended inside the lock) holds the entry before the pinned merge.
func TestMergeQueueMergeOnPass_InLockRecheck(t *testing.T) {
	cases := []struct {
		name    string
		state   string
		reviews string
		reason  string
	}{
		{"changes requested", "", `[{"id":1,"user":{"login":"alice"},"state":"APPROVED"},{"id":2,"user":{"login":"bob"},"state":"CHANGES_REQUESTED"}]`, mergeOnPassReasonChangesRequested},
		{"approval dismissed", "blocked", mqeDismissed, mergeOnPassReasonApprovalDismissed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, req := w.ready("rc", "h1", mqeSpecYAML)
			w.gh.set(func() { w.gh.reviews[r.pr] = mqeApprovedBy("alice") })
			w.audit.onAppend = func(category string) {
				if category == CategoryMergeVerdictRecorded {
					w.gh.set(func() { w.gh.reviews[r.pr] = tc.reviews })
					w.mergeable(r, tc.state)
				}
			}
			requireOutcome(t, w.decide(req), mergeOnPassHeld, tc.reason)
			w.requireMerges(0)
			if len(w.ledger.claims) != 0 {
				t.Fatalf("ledger claimed %v before a held in-lock re-check", w.ledger.claims)
			}
		})
	}
}

func TestMergeQueueMergeOnPass_OverlapRefusalHoldsUnmerged(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("ov", "h1", mcvUndelegatedSpecYAML)
	// A second run of the repo with its implement stage running and no
	// resolvable scope: the guard refuses 409 overlap_undetermined.
	other := w.seed("other", "o1", []byte(mcvUndelegatedSpecYAML))
	w.setStage(other.impl.ID, run.StageStateRunning)
	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassHeld, mergeOnPassReasonOverlapRefused)
	w.requireMerges(0)
	rows := w.rows(r, CategoryMergeQueueOverlapRefused)
	if len(rows) != 1 {
		t.Fatalf("merge_queue_overlap_refused rows = %d, want 1", len(rows))
	}
	p := w.payload(rows[0])
	if strings.Contains(string(rows[0].Payload), other.id.String()) || p["refused_code"] == "" {
		t.Fatalf("overlap row = %s", rows[0].Payload)
	}
	// A repeat decision appends no second row.
	requireOutcome(t, w.decide(req), mergeOnPassHeld, mergeOnPassReasonOverlapRefused)
	if n := len(w.rows(r, CategoryMergeQueueOverlapRefused)); n != 1 {
		t.Fatalf("overlap rows after repeat = %d, want 1", n)
	}
}

func TestMergeQueueMergeOnPass_InlineVerdict(t *testing.T) {
	t.Run("after intent holds", func(t *testing.T) {
		w := newMQMWorld(t)
		r, req := w.ready("ia", "h1", mcvUndelegatedSpecYAML)
		w.mqeAppend(r, CategoryMergeVerdictRecorded, map[string]any{"verdict": "inline", "delegated": false})
		requireOutcome(t, w.decide(req), mergeOnPassHeld, mergeOnPassReasonInlineMergeDispatched)
		w.requireMerges(0)
	})
	t.Run("before intent resumes", func(t *testing.T) {
		w := newMQMWorld(t)
		r := w.seed("ib", "h1", []byte(mcvUndelegatedSpecYAML))
		w.mqePassed(r, "h1")
		prior := w.mqeAppend(r, CategoryMergeVerdictRecorded, map[string]any{"verdict": "inline", "delegated": false})
		req := w.live(r, "h1")
		requireOutcome(t, w.decide(req), mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
		if n := len(w.rows(r, CategoryMergeVerdictRecorded)); n != 1 {
			t.Fatalf("verdict rows = %d, want the prior row only", n)
		}
		mp := w.payload(w.rows(r, CategoryMergeQueueMerged)[0])
		if int64(mp["verdict_sequence"].(float64)) != prior.Sequence || mp["verdict_head_sha"] != "" {
			t.Fatalf("merged payload = %v, want the prior verdict (no head)", mp)
		}
	})
}

// TestRecordMergeOnPassVerdict_DuplicateOnAppendResumesWinner pins the 0062
// once-index arm: a verdict row the pre-read did not see (existing=nil) makes
// the append collide, and the re-read returns the winner's sequence and head.
func TestRecordMergeOnPassVerdict_DuplicateOnAppendResumesWinner(t *testing.T) {
	w := newMQMWorld(t)
	r, _ := w.ready("dup", "h1", mcvUndelegatedSpecYAML)
	winner := w.mqeAppend(r, CategoryMergeVerdictRecorded, map[string]any{"verdict": "raced", "delegated": false, "head_sha": "h0"})
	seq, head, err := w.srv.recordMergeOnPassVerdict(context.Background(), w.runRow(r), w.active(r),
		mergeQueueIntentRecord{Verdict: "ship it"}, "h1", nil)
	if err != nil || seq != winner.Sequence || head != "h0" {
		t.Fatalf("record = %d, %q, %v; want the winner %d at h0", seq, head, err, winner.Sequence)
	}
	if n := len(w.rows(r, CategoryMergeVerdictRecorded)); n != 1 {
		t.Fatalf("verdict rows = %d, want the winner only", n)
	}
}

func TestMergeQueueMergeOnPass_IntentRowSelectedByRecordedSequence(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("sq", "h1", mcvUndelegatedSpecYAML)
	// A newer orphan row naming another verdict is ignored.
	w.mqeAppend(r, CategoryMergeQueueEntered, map[string]any{"base_ref": mcqBase, "anchored_head_sha": "zz", "verdict": "orphan"})
	requireOutcome(t, w.decide(req), mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
	if v := w.payload(w.rows(r, CategoryMergeVerdictRecorded)[0])["verdict"]; v != "ship it at h1" {
		t.Fatalf("verdict prose = %v, want the recorded-sequence row's", v)
	}
}

func TestMergeQueueMergeOnPass_VerdictAppendFailureHoldsNothingMerged(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("vf", "h1", mcvUndelegatedSpecYAML)
	w.audit.failCategory = CategoryMergeVerdictRecorded
	requireOutcome(t, w.decide(req), mergeOnPassHeld, mergeOnPassReasonVerdictUnrecorded)
	w.requireMerges(0)
	if len(w.ledger.claims) != 0 {
		t.Fatalf("ledger claimed %v without a recorded verdict", w.ledger.claims)
	}
	if e := w.active(r); e.State != mergequeue.StateLive {
		t.Fatalf("entry = %s, want live", e.State)
	}
}

// TestMergeQueueMergeOnPass_MergedAppendFailureStillMerged pins approval
// condition (c).
func TestMergeQueueMergeOnPass_MergedAppendFailureStillMerged(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("mf", "h1", mcvUndelegatedSpecYAML)
	w.audit.failCategory = CategoryMergeQueueMerged
	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
	if !out.Settled {
		t.Fatal("not settled after the forge accepted the merge")
	}
	w.requireState(r, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
}

func TestMergeQueueMergeOnPass_AlreadyDispatchedNeverCallsForge(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("ad", "h1", mcvUndelegatedSpecYAML)
	w.ledger.claimed[r.id] = "inline"
	requireOutcome(t, w.decide(req), mergeOnPassHeld, mergeOnPassReasonAlreadyDispatched)
	w.requireMerges(0)
	if e := w.active(r); e.State != mergequeue.StateLive {
		t.Fatalf("entry = %s, want live (kept for settle/reconcile)", e.State)
	}
}

// TestMergeQueueMergeOnPass_ForgeErrors pins the forge-refusal ladder and
// ruling R1: a refusal that applied nothing (405/409/422) RELEASES the claim;
// an uncertain failure marks it dispatch_failed (never deletes it).
func TestMergeQueueMergeOnPass_ForgeErrors(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		reviews      string
		state        string
		observeMerge bool
		kind         mergeOnPassKind
		reason       string
		failed       int
		released     int
	}{
		{name: "409 head mismatch readvances", err: fmt.Errorf("pinned: %w", forge.ErrMergeConflict),
			kind: mergeOnPassReadvance, reason: mergeOnPassReasonForgeHeadMismatch, released: 1},
		{name: "405 ejects forge_merge_refused", err: fmt.Errorf("pinned: %w", forge.ErrPullRequestNotMergeable),
			kind: mergeOnPassSettle, reason: mergeCandidateQueueReasonForgeMergeRefused, released: 1},
		{name: "405 with dismissed approval ejects approval dismissed", err: fmt.Errorf("pinned: %w", forge.ErrPullRequestNotMergeable),
			reviews: mqeDismissed, state: "blocked", kind: mergeOnPassSettle, reason: mergeCandidateQueueReasonForgeApprovalDismissed, released: 1},
		{name: "403 is uncertain and holds", err: fmt.Errorf("pinned: %w", forge.ErrForbidden),
			kind: mergeOnPassHeld, reason: mergeOnPassReasonTransient, failed: 1},
		{name: "transient error holds", err: errors.New("connection reset"),
			kind: mergeOnPassHeld, reason: mergeOnPassReasonTransient, failed: 1},
		{name: "transient error but observed merged", err: errors.New("timeout"), observeMerge: true,
			kind: mergeOnPassMerged, reason: mergeCandidateQueueReasonMergedByQueue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, req := w.ready("fe", "h1", mcvUndelegatedSpecYAML)
			w.merger.err = tc.err
			if tc.observeMerge {
				w.merger.err = nil
				orig := w.merger
				w.srv.cfg.GateMerger = mqmFailAfterMerge{orig, tc.err}
			}
			if tc.reviews != "" {
				// Dismissed only after the pre-merge checks admitted.
				w.audit.onAppend = nil
				w.merger.entered = make(chan struct{}, 1)
				go func() {
					<-w.merger.entered
					w.gh.set(func() { w.gh.reviews[r.pr] = tc.reviews })
					w.mergeable(r, tc.state)
				}()
				w.merger.park = make(chan struct{})
				go func() { time.Sleep(timescale.D(200 * time.Millisecond)); close(w.merger.park) }()
			}
			out := w.decide(req)
			requireOutcome(t, out, tc.kind, tc.reason)
			if n := len(w.rows(r, CategoryMergeQueueMerged)); (n == 1) != (tc.kind == mergeOnPassMerged) {
				t.Fatalf("merge_queue_merged rows = %d for outcome %s", n, tc.kind)
			}
			if len(w.ledger.failed) != tc.failed || len(w.ledger.released) != tc.released {
				t.Fatalf("dispatch_failed marks = %v (want %d), releases = %v (want %d)",
					w.ledger.failed, tc.failed, w.ledger.released, tc.released)
			}
			if _, ok := w.ledger.claimed[r.id]; ok != (tc.released == 0) {
				t.Fatalf("claim present = %v, want %v", ok, tc.released == 0)
			}
		})
	}
}

// mqmFailAfterMerge merges, then reports an error (a merge the forge applied
// whose response was lost).
type mqmFailAfterMerge struct {
	*mqmMerger
	err error
}

func (m mqmFailAfterMerge) MergePullRequestAtHead(ctx context.Context, runRow *run.Run, head string) error {
	_ = m.mqmMerger.MergePullRequestAtHead(ctx, runRow, head)
	return m.err
}

// TestMergeQueueMergeOnPass_ConcurrentDecisionsMergeOnce pins the shared
// merge-intent lock ALONE: the ledger is permissive, so only the lock (and the
// in-lock entry re-read after D3's merged settle) stops a second dispatch.
func TestMergeQueueMergeOnPass_ConcurrentDecisionsMergeOnce(t *testing.T) {
	w := newMQMWorld(t)
	_, req := w.ready("cc", "h1", mcvUndelegatedSpecYAML)
	w.ledger.permissive = true
	w.merger.park = make(chan struct{})
	w.merger.entered = make(chan struct{}, 2)
	var wg sync.WaitGroup
	outs := make([]mergeOnPassOutcome, 2)
	for i := range outs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i] = w.decide(req)
		}(i)
	}
	<-w.merger.entered
	time.Sleep(timescale.D(300 * time.Millisecond))
	close(w.merger.park)
	wg.Wait()
	w.requireMerges(1)
	merged := 0
	for _, o := range outs {
		if o.Kind == mergeOnPassMerged {
			merged++
		}
	}
	if merged != 1 {
		t.Fatalf("outcomes = %+v, want exactly one merged", outs)
	}
}

// TestMergeQueueMergeOnPass_LedgerAloneMergesOnce pins D1 at the decision:
// with the merger never marking the PR merged and the entry re-read passing,
// two sequential decisions dispatch once, the second refused by the ledger.
func TestMergeQueueMergeOnPass_LedgerAloneMergesOnce(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("la", "h1", mcvUndelegatedSpecYAML)
	w.merger.err = errors.New("lost response")
	requireOutcome(t, w.decide(req), mergeOnPassHeld, mergeOnPassReasonTransient)
	w.merger.err = nil
	requireOutcome(t, w.decide(req), mergeOnPassHeld, mergeOnPassReasonAlreadyDispatched)
	w.requireMerges(1)
	if e := w.active(r); e.State != mergequeue.StateLive {
		t.Fatalf("entry = %s, want live", e.State)
	}
}

func TestMergeQueueOwnsMerge_Predicate(t *testing.T) {
	ctx := context.Background()
	w := newMQMWorld(t)
	live, _ := w.ready("own", "h1", mcvUndelegatedSpecYAML)
	held := w.seed("held", "k1", []byte(mcvUndelegatedSpecYAML))
	if _, err := w.store.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: held.id, BaseRef: mcqBase, HeadSHA: "k1", MergeIntent: true}); err != nil {
		t.Fatalf("enqueue held: %v", err)
	}
	noIntent := w.seed("plain", "p1", []byte(mcvUndelegatedSpecYAML))
	if _, err := w.store.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: noIntent.id, BaseRef: mcqBase, HeadSHA: "p1"}); err != nil {
		t.Fatalf("enqueue plain: %v", err)
	}
	none := w.seed("none", "n1", []byte(mcvUndelegatedSpecYAML))
	w.exec(`UPDATE merge_candidate_queue_entries SET phase = 'verifying' WHERE run_id = $1`, live.id)

	for _, tc := range []struct {
		name string
		r    *mcqRun
		want bool
	}{
		{"live with intent, phase verifying", live, true},
		{"held with intent (D2)", held, true},
		{"no intent", noIntent, false},
		{"no entry", none, false},
	} {
		got, err := w.srv.mergeQueueOwnsMerge(ctx, tc.r.id)
		if err != nil || got != tc.want {
			t.Fatalf("%s: owned=%v err=%v, want %v", tc.name, got, err, tc.want)
		}
	}
	w.srv.cfg.GateMerger = mqmUnpinned{}
	if got, err := w.srv.mergeQueueOwnsMerge(ctx, live.id); err != nil || got {
		t.Fatalf("unpinned: owned=%v err=%v, want false", got, err)
	}
	w.srv.cfg.GateMerger = w.merger
	w.srv.cfg.MergeQueue = mqmErrQueue{w.store}
	if _, err := w.srv.mergeQueueOwnsMerge(ctx, live.id); err == nil {
		t.Fatal("queue read error: want an error")
	}
}

func TestWithRunMergeLock_UnwiredQueueCallsFn(t *testing.T) {
	s := &Server{}
	called := false
	if err := s.withRunMergeLock(context.Background(), uuid.New(), func(context.Context) error { called = true; return nil }); err != nil || !called {
		t.Fatalf("called=%v err=%v", called, err)
	}
	if s.headPinnedMerger() != nil {
		t.Fatal("headPinnedMerger with no GateMerger: want nil")
	}
	if owned, err := s.mergeQueueOwnsMerge(context.Background(), uuid.New()); owned || err != nil {
		t.Fatalf("unwired: owned=%v err=%v", owned, err)
	}
}

// mqmLockQueue overrides the merge-intent lock and Settle; every other method
// is the embedded store.
type mqmLockQueue struct {
	mergequeue.Store
	lockErr   error
	runFn     bool
	settleErr error
}

func (q mqmLockQueue) WithMergeIntentLock(ctx context.Context, runID uuid.UUID, fn func(context.Context) error) error {
	if q.runFn {
		if err := q.Store.WithMergeIntentLock(ctx, runID, fn); err != nil {
			return err
		}
	}
	return q.lockErr
}

func (q mqmLockQueue) Settle(ctx context.Context, id uuid.UUID, state mergequeue.State, reason string) (mergequeue.SettleResult, error) {
	if q.settleErr != nil {
		return mergequeue.SettleResult{}, q.settleErr
	}
	return q.Store.Settle(ctx, id, state, reason)
}

// TestMergeQueueMergeOnPass_LockAndEntryBranches pins the lock and entry
// failure modes.
func TestMergeQueueMergeOnPass_LockAndEntryBranches(t *testing.T) {
	t.Run("lock not taken holds", func(t *testing.T) {
		w := newMQMWorld(t)
		_, req := w.ready("lk", "h1", mcvUndelegatedSpecYAML)
		w.srv.cfg.MergeQueue = mqmLockQueue{Store: w.store, lockErr: errors.New("no lock")}
		requireOutcome(t, w.decide(req), mergeOnPassHeld, mergeOnPassReasonLockUnavailable)
		w.requireMerges(0)
	})
	t.Run("lapsed lock keeps the decision", func(t *testing.T) {
		w := newMQMWorld(t)
		_, req := w.ready("lp", "h1", mcvUndelegatedSpecYAML)
		w.srv.cfg.MergeQueue = mqmLockQueue{Store: w.store, runFn: true, lockErr: mergequeue.ErrIntentLockLapsed}
		requireOutcome(t, w.decide(req), mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
		w.requireMerges(1)
	})
	t.Run("settle error leaves Settled false", func(t *testing.T) {
		w := newMQMWorld(t)
		r, req := w.ready("se", "h1", mcvUndelegatedSpecYAML)
		w.srv.cfg.MergeQueue = mqmLockQueue{Store: w.store, runFn: true, settleErr: errors.New("settle down")}
		out := w.decide(req)
		requireOutcome(t, out, mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
		if out.Settled {
			t.Fatal("Settled true after a failed settle")
		}
		if e := w.active(r); e.State != mergequeue.StateLive {
			t.Fatalf("entry = %s, want live for the pump to settle", e.State)
		}
	})
	t.Run("entry changed under the lock holds", func(t *testing.T) {
		w := newMQMWorld(t)
		_, req := w.ready("ec", "h1", mcvUndelegatedSpecYAML)
		req.Entry.ID = uuid.New()
		requireOutcome(t, w.decide(req), mergeOnPassHeld, mergeOnPassReasonEntryChanged)
		w.requireMerges(0)
	})
	t.Run("unknown run settles run_terminal", func(t *testing.T) {
		w := newMQMWorld(t)
		_, req := w.ready("ur", "h1", mcvUndelegatedSpecYAML)
		req.Entry.RunID = uuid.New()
		out := w.decide(req)
		requireOutcome(t, out, mergeOnPassSettle, mergeCandidateQueueReasonRunTerminal)
		w.requireMerges(0)
	})
}
