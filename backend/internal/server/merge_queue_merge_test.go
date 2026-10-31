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
	"sync/atomic"
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

func (m *mqmMerger) MergePullRequest(context.Context, *run.Run, string) error {
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

func (mqmUnpinned) MergePullRequest(context.Context, *run.Run, string) error { return nil }

// mqmLedger is a recording D1 dispatch ledger: one claim per run unless
// permissive (then every claim is granted, isolating the merge-intent lock).
type mqmLedger struct {
	mu         sync.Mutex
	permissive bool
	claimErr   error
	// readErr, when set, is MergeDispatch's error (the claim read fails).
	readErr   error
	claimed   map[uuid.UUID]string
	claims    []string
	failed    []string
	released  []string
	heads     map[uuid.UUID]string
	failedRun map[uuid.UUID]bool
}

func newMQMLedger() *mqmLedger {
	return &mqmLedger{claimed: map[uuid.UUID]string{}, heads: map[uuid.UUID]string{}, failedRun: map[uuid.UUID]bool{}}
}

func (l *mqmLedger) MergeDispatch(_ context.Context, runID uuid.UUID) (mergequeue.DispatchClaim, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.readErr != nil {
		return mergequeue.DispatchClaim{}, false, l.readErr
	}
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
	// onResponse, when set, runs on every proxied response (before any
	// rewrite) with its request path: the ADR-112 head-moved seam.
	onResponse func(path string)
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
		w.mu.Lock()
		hook := w.onResponse
		w.mu.Unlock()
		if hook != nil {
			hook(resp.Request.URL.Path)
		}
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

// mqmActorID is a stable non-zero GraphQL databaseId for login.
func mqmActorID(login string) int64 {
	var n int64 = 7
	for _, c := range login {
		n = n*31 + int64(c)
	}
	return n
}

// mqmReview is login's review in state at commit (a GraphQL User author).
func mqmReview(id int64, login, state, commit string) mcqReviewNode {
	return mcqReviewNode{ID: id, State: state, SubmittedAt: time.Date(2026, 1, 1, 0, 0, int(id), 0, time.UTC), Commit: commit,
		Author: mcqActor{Typename: "User", Login: login, ID: mqmActorID(login)}}
}

// forgeReviews plants r's GraphQL review snapshot and grants every reviewer
// in writers write permission.
func (w *mqmWorld) forgeReviews(r *mcqRun, writers []string, nodes ...mcqReviewNode) {
	w.t.Helper()
	w.gh.setReviewSnapshot(r.pr, mcqReviewSnapshot{Reviews: nodes, Author: mcqActor{Typename: "User", Login: "pr-opener", ID: 1}})
	for _, l := range writers {
		w.gh.setPermission(l, "write", "write")
	}
}

// requireNoMergeWrites asserts a non-merging decision wrote no verdict, no
// authorization row, claimed nothing and merged nothing.
func (w *mqmWorld) requireNoMergeWrites(r *mcqRun) {
	w.t.Helper()
	w.requireMerges(0)
	if len(w.ledger.claims) != 0 {
		w.t.Fatalf("ledger claimed %v", w.ledger.claims)
	}
	for _, c := range []string{CategoryMergeVerdictRecorded, CategoryMergeQueueAuthorized, CategoryMergeQueueMerged} {
		if n := len(w.rows(r, c)); n != 0 {
			w.t.Fatalf("%s rows = %d, want 0", c, n)
		}
	}
}

// TestMergeQueueMergeOnPass_ForgeApprovalFoldInterimTable (converted from
// #4619's interim drop/eject table to ADR-112 R3, #4632) pins every
// non-approved fold at the verified head: vetoed YIELDS immediately (row 2);
// review_required, dismissed, absent, undetermined and reviews_truncated
// AWAIT approval at the head (row 3). The decision writes no verdict, no
// authorization row and no claim; applying it (the pump's outside-the-lock
// half) leaves an immediate yield HELD and marked with no wait anchor, and a
// wait LIVE and unmarked with the anchor set. Counterfactual: mapping vetoed
// to a wait leaves the veto row live with approval_wait_started_at set, RED.
func TestMergeQueueMergeOnPass_ForgeApprovalFoldInterimTable(t *testing.T) {
	cases := []struct {
		name   string
		plant  func(w *mqmWorld, r *mcqRun)
		kind   mergeOnPassKind
		reason string
		next   string
	}{
		{"write veto yields immediately", func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, []string{"alice", "bob"}, mqmReview(1, "alice", "APPROVED", "h1"), mqmReview(2, "bob", "CHANGES_REQUESTED", "h1"))
		}, mergeOnPassYield, mergeCandidateQueueReasonForgeApprovalVetoed, forgeApprovalNextDismissReview},
		{"review required waits", func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "APPROVED", "h1"))
			fx := *w.gh.reviewSnapshot[r.pr]
			fx.ReviewDecision = "REVIEW_REQUIRED"
			w.gh.setReviewSnapshot(r.pr, fx)
		}, mergeOnPassAwaitApproval, mergeCandidateQueueReasonForgeApprovalReviewRequired, forgeApprovalNextApprovePR},
		{"dismissed waits", func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "DISMISSED", "h1"))
		}, mergeOnPassAwaitApproval, mergeCandidateQueueReasonForgeApprovalDismissed, forgeApprovalNextApprovePR},
		{"read-permission member waits absent", func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, nil, mqmReview(1, "alice", "APPROVED", "h1"))
			w.gh.setPermission("alice", "read", "read")
		}, mergeOnPassAwaitApproval, mergeCandidateQueueReasonForgeApprovalAbsent, forgeApprovalNextApprovePR},
		// The operator's #4619 pin: a CLEAN mergeStateStatus with no reviews is
		// no shortcut.
		{"clean merge state and no reviews waits absent", func(w *mqmWorld, r *mcqRun) {
			w.gh.setReviewSnapshot(r.pr, mcqReviewSnapshot{MergeStateStatus: "CLEAN", Author: mcqActor{Typename: "User", Login: "pr-opener", ID: 1}})
			w.mergeable(r, "clean")
		}, mergeOnPassAwaitApproval, mergeCandidateQueueReasonForgeApprovalAbsent, forgeApprovalNextApprovePR},
		{"permission 500 waits undetermined", func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, nil, mqmReview(1, "alice", "APPROVED", "h1"))
			w.gh.set(func() { w.gh.permStatus["alice"] = 500 })
		}, mergeOnPassAwaitApproval, mergeCandidateQueueReasonForgeApprovalUndetermined, forgeApprovalNextRetryAfter},
		{"truncated listing waits", func(w *mqmWorld, r *mcqRun) {
			w.gh.setReviewSnapshot(r.pr, mcqReviewSnapshot{Truncated: true, Author: mcqActor{Typename: "User", Login: "pr-opener", ID: 1}})
		}, mergeOnPassAwaitApproval, mergeCandidateQueueReasonForgeApprovalReviewsTruncated, forgeApprovalNextRetryAfter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, req := w.ready("ft", "h1", mqeSpecYAML)
			tc.plant(w, r)
			out := w.decide(req)
			requireOutcome(t, out, tc.kind, tc.reason)
			if out.Yield.Reason != tc.reason || out.Yield.NextStep != tc.next || mergeQueueYieldNextStep(tc.reason) != tc.next || len(out.Yield.Detail) > forgeApprovalDetailMaxBytes {
				t.Fatalf("outcome yield = %+v, want reason %s next_step %s", out.Yield, tc.reason, tc.next)
			}
			w.requireNoMergeWrites(r)

			readvances := 0
			cont := w.srv.applyMergeOnPassOutcome(context.Background(), req.Entry, out, &readvances)
			e := w.active(r)
			if tc.kind == mergeOnPassYield {
				if !cont || e.State != mergequeue.StateHeld || e.YieldReason != tc.reason || e.YieldedAt == nil || e.ApprovalWaitStartedAt != nil {
					t.Fatalf("immediate yield applied: continue=%v entry=%+v, want held, marked %s, no wait anchor", cont, e, tc.reason)
				}
				if n := len(w.rows(r, CategoryMergeQueueYielded)); n != 1 {
					t.Fatalf("merge_queue_yielded rows = %d, want 1", n)
				}
			} else {
				if cont || e.State != mergequeue.StateLive || e.YieldedAt != nil || e.ApprovalWaitStartedAt == nil {
					t.Fatalf("wait applied: continue=%v entry=%+v, want live, unmarked, wait anchor set", cont, e)
				}
				if n := len(w.rows(r, CategoryMergeQueueYielded)); n != 0 {
					t.Fatalf("merge_queue_yielded rows = %d, want 0 before expiry", n)
				}
			}
			w.requireNoMergeWrites(r)
		})
	}
}

// transferred seeds the D6 transfer world: intent at h0, a parent-verified
// clean advance to h1, h1 passed, and alice's approval at h0 only, so the
// fold approves only through the transferred basis.
func (w *mqmWorld) transferred(advance func(r *mcqRun)) (*mcqRun, mergeOnPassRequest) {
	w.t.Helper()
	r := w.seed("tr", "h1", []byte(mqeSpecYAML))
	anchor := w.mqeIntent(r, "h0")
	advance(r)
	w.mqePassed(r, "h1")
	w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "APPROVED", "h0"))
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

func TestMergeQueueMergeOnPass_TransferredBasis(t *testing.T) {
	t.Run("parent-verified advance carries the approval", func(t *testing.T) {
		w := newMQMWorld(t)
		r, req := w.transferred(func(r *mcqRun) { w.mqeRebased(r, "h0", "h1", "h1", false) })
		out := w.decide(req)
		requireOutcome(t, out, mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
		if out.ApprovalBasis != forgeApprovalBasisTransferred {
			t.Fatalf("approval basis = %q, want transferred", out.ApprovalBasis)
		}
		auth := w.rows(r, CategoryMergeQueueAuthorized)
		if len(auth) != 1 {
			t.Fatalf("merge_queue_authorized rows = %d, want 1", len(auth))
		}
		if p := w.payload(auth[0]); p["basis"] != forgeApprovalBasisTransferred || p["path"] != mergeDispatchPathQueue {
			t.Fatalf("authorized payload = %v", p)
		}
	})
	t.Run("unverified advance carries nothing", func(t *testing.T) {
		w := newMQMWorld(t)
		r, req := w.transferred(func(r *mcqRun) { w.mqeUnverifiedAdvance(r, "h0", "h1") })
		requireOutcome(t, w.decide(req), mergeOnPassAwaitApproval, mergeCandidateQueueReasonForgeApprovalAbsent)
		w.requireNoMergeWrites(r)
	})
}

// TestMergeQueueMergeOnPass_LiveApprovalMergesWithEvidence pins the approved
// path: one in-lock snapshot, ZERO REST review listings (the listing answers
// 500, so any read would hold), and merge_queue_authorized < verdict <
// merge_queue_merged, the merged row naming the authorization and reviewer.
func TestMergeQueueMergeOnPass_LiveApprovalMergesWithEvidence(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("lv", "h1", mqeSpecYAML)
	w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "APPROVED", "h1"))
	w.gh.set(func() { w.gh.reviewsStatus[r.pr] = 500 })
	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
	if out.ApprovalBasis != forgeApprovalBasisLive {
		t.Fatalf("approval basis = %q, want live", out.ApprovalBasis)
	}
	if n := w.gh.snapshotCalls(); n != 1 {
		t.Fatalf("snapshot calls = %d, want 1", n)
	}
	auth, verdicts, merged := w.rows(r, CategoryMergeQueueAuthorized), w.rows(r, CategoryMergeVerdictRecorded), w.rows(r, CategoryMergeQueueMerged)
	if len(auth) != 1 || len(verdicts) != 1 || len(merged) != 1 {
		t.Fatalf("rows authorized=%d verdict=%d merged=%d, want 1 each", len(auth), len(verdicts), len(merged))
	}
	if auth[0].Sequence >= verdicts[0].Sequence || verdicts[0].Sequence >= merged[0].Sequence {
		t.Fatalf("sequences authorized=%d verdict=%d merged=%d out of order", auth[0].Sequence, verdicts[0].Sequence, merged[0].Sequence)
	}
	mp := w.payload(merged[0])
	q, _ := mp["qualifying_reviewer"].(map[string]any)
	if int64(mp["authorization_sequence"].(float64)) != auth[0].Sequence || q["login"] != "alice" || q["permission"] != "write" {
		t.Fatalf("merged payload = %v", mp)
	}
}

// TestMergeQueueMergeOnPass_FoldFailureModes pins the non-settle fold
// outcomes: a snapshot transport failure is an UNDETERMINED approval result
// (#4748) that WAITS at the head (await_approval, forge_approval_undetermined)
// writing nothing, and the healed next decision merges (CF4: back to a
// transient hold, the kind is held, RED); a failed merge_queue_authorized
// append HOLDS authorization_unrecorded with no verdict and no claim; and
// #4748 M1: a PR the forge reports merged whose dispatch claim cannot be read
// holds merged_claim_unread with NO ExpiryYield, so the expired-wait
// enforcement never yields it. MECHANISM (CF9): with the transient reason the
// held closure attaches the generic ExpiryYield, and the enforcement, whose
// claim re-read then succeeds with no claim, yields an already-merged entry,
// which the re-probe never clears: the no-yield assertion turns RED.
func TestMergeQueueMergeOnPass_FoldFailureModes(t *testing.T) {
	t.Run("snapshot failure waits, then merges", func(t *testing.T) {
		w := newMQMWorld(t)
		r, req := w.ready("sf", "h1", mqeSpecYAML)
		w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "APPROVED", "h1"))
		w.gh.set(func() { w.gh.graphQLFail[r.pr] = 3 })
		out := w.decide(req)
		requireOutcome(t, out, mergeOnPassAwaitApproval, mergeCandidateQueueReasonForgeApprovalUndetermined)
		if out.Yield.Reason != mergeCandidateQueueReasonForgeApprovalUndetermined || out.Yield.NextStep != forgeApprovalNextRetryAfter ||
			out.Yield.Row != mergeQueueYieldRowApprovalFold || out.ExpiryYield != nil {
			t.Fatalf("snapshot-failure yield = %+v expiry=%v, want forge_approval_undetermined retry_after row 3, no ExpiryYield", out.Yield, out.ExpiryYield)
		}
		w.requireNoMergeWrites(r)
		w.gh.set(func() { w.gh.graphQLFail[r.pr] = 0 })
		requireOutcome(t, w.decide(req), mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
	})
	t.Run("merged with an unreadable claim never yields", func(t *testing.T) {
		w := newMQMWorld(t)
		r, req := w.ready("mu", "h1", mqeSpecYAML)
		req = w.mqmExpiredWait(r, req)
		w.gh.set(func() { w.gh.prs[r.pr].merged, w.gh.prs[r.pr].state = true, "closed" })
		w.ledger.readErr = errors.New("injected claim read failure")
		out := w.decide(req)
		// The claim read heals before the enforcement runs (CF9's mechanism).
		w.ledger.readErr = nil
		enforced := w.srv.mergeOnPassEnforceExpiredWait(context.Background(), req, out)
		if n := len(w.rows(r, CategoryMergeQueueYielded)); n != 0 || w.active(r).Yielded() {
			t.Fatalf("merge_queue_yielded rows = %d, entry yielded = %v: an expired wait yielded a merged PR", n, w.active(r).Yielded())
		}
		requireOutcome(t, out, mergeOnPassHeld, mergeOnPassReasonMergedClaimUnread)
		if out.ExpiryYield != nil || enforced.Kind != mergeOnPassHeld || enforced.Reason != mergeOnPassReasonMergedClaimUnread {
			t.Fatalf("outcome expiry=%v enforced=%s/%s, want no ExpiryYield and the hold unchanged", out.ExpiryYield, enforced.Kind, enforced.Reason)
		}
		w.requireNoMergeWrites(r)
	})
	t.Run("authorization append failure holds", func(t *testing.T) {
		w := newMQMWorld(t)
		r, req := w.ready("af", "h1", mqeSpecYAML)
		w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "APPROVED", "h1"))
		w.audit.failCategory = CategoryMergeQueueAuthorized
		requireOutcome(t, w.decide(req), mergeOnPassHeld, mergeOnPassReasonAuthorizationUnrecorded)
		w.requireNoMergeWrites(r)
	})
}

// mqmExpiredWait stamps r's live entry's approval-wait anchor through the
// real store, backdates it past the default window and returns req with the
// re-read entry (an ESTABLISHED, expired episode).
func (w *mqmWorld) mqmExpiredWait(r *mcqRun, req mergeOnPassRequest) mergeOnPassRequest {
	w.t.Helper()
	if _, err := w.store.ApprovalWait(context.Background(), req.Entry.ID, req.Entry.AdmissionEpoch, time.Hour); err != nil {
		w.t.Fatalf("stamp the approval wait: %v", err)
	}
	w.backdateApprovalWait(r)
	req.Entry = w.active(r)
	return req
}

// TestMergeQueueMergeOnPass_BaseMovedWithEstablishedWait (#4748, CF5's
// decision vehicle) pins step 6's base-moved arm. The decision writes nothing
// (w.decide), so each row asserts the readvance and the ExpiryYield the pump's
// enforcement would apply. An entry whose wait is ESTABLISHED runs the fold at
// the verified head before re-advancing: approved → no ExpiryYield; dismissed
// → the fold's yield; fold error → forge_approval_undetermined; merged →
// settle merged; head moved → readvance head_moved. With NO anchor the
// readvance carries the generic ExpiryYield and makes no fold read.
//
// The R4 choice: the base-move path yields ONLY on expiry. An anchored VETOED
// entry readvances like an unanchored one, carrying the veto's yield
// (forge_approval_vetoed, row 2) as its ExpiryYield; it meets the veto's
// immediate yield at step 6b on its next pass, and an expired anchored one
// yields with the veto's reason. Reinstating an immediate yield there turns
// the vetoed row RED. Deleting the fold (CF5) turns the dismissed, vetoed,
// approved, merged and head-moved rows RED.
func TestMergeQueueMergeOnPass_BaseMovedWithEstablishedWait(t *testing.T) {
	cases := []struct {
		name      string
		noAnchor  bool
		plant     func(w *mqmWorld, r *mcqRun)
		kind      mergeOnPassKind
		reason    string
		expiry    string // "" = nil ExpiryYield
		expiryRow int
	}{
		{"approved fold re-advances with no expiry yield", false, func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "APPROVED", "h1"))
		}, mergeOnPassReadvance, mergeOnPassReasonBaseMoved, "", 0},
		{"dismissed carries the fold's yield", false, func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "DISMISSED", "h1"))
		}, mergeOnPassReadvance, mergeOnPassReasonBaseMoved, mergeCandidateQueueReasonForgeApprovalDismissed, mergeQueueYieldRowApprovalFold},
		{"vetoed re-advances carrying the veto's yield", false, func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, []string{"alice", "bob"}, mqmReview(1, "alice", "APPROVED", "h1"), mqmReview(2, "bob", "CHANGES_REQUESTED", "h1"))
		}, mergeOnPassReadvance, mergeOnPassReasonBaseMoved, mergeCandidateQueueReasonForgeApprovalVetoed, mergeQueueYieldRowVeto},
		{"fold error carries an undetermined yield", false, func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "APPROVED", "h1"))
			w.gh.set(func() { w.gh.graphQLFail[r.pr] = 100 })
		}, mergeOnPassReadvance, mergeOnPassReasonBaseMoved, mergeCandidateQueueReasonForgeApprovalUndetermined, mergeQueueYieldRowApprovalFold},
		{"fold merged settles merged", false, func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "DISMISSED", "h1"))
			w.onResponse = func(path string) {
				if strings.Contains(path, "/compare/") {
					w.gh.set(func() { w.gh.prs[r.pr].merged, w.gh.prs[r.pr].state = true, "closed" })
				}
			}
		}, mergeOnPassSettle, mergeCandidateQueueReasonPRMerged, "", 0},
		{"fold head moved re-advances head_moved", false, func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "DISMISSED", "h1"))
			w.onResponse = func(path string) {
				if strings.Contains(path, "/compare/") {
					w.gh.set(func() { w.gh.prs[r.pr].head = "h2" })
				}
			}
		}, mergeOnPassReadvance, mergeOnPassReasonHeadMoved, mergeCandidateQueueReasonForgeApprovalUndetermined, mergeQueueYieldRowApprovalFold},
		{"no anchor re-advances with the generic yield and no fold read", true, func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "DISMISSED", "h1"))
		}, mergeOnPassReadvance, mergeOnPassReasonBaseMoved, mergeCandidateQueueReasonForgeApprovalUndetermined, mergeQueueYieldRowApprovalFold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, req := w.ready("bm", "h1", mqeSpecYAML)
			if !tc.noAnchor {
				if _, err := w.store.ApprovalWait(context.Background(), req.Entry.ID, req.Entry.AdmissionEpoch, time.Hour); err != nil {
					t.Fatalf("stamp the approval wait: %v", err)
				}
				req.Entry = w.active(r)
			}
			tc.plant(w, r)
			w.gh.set(func() { w.gh.behind["h1"] = []string{"base-x"} })
			reads := w.gh.snapshotCalls()
			out := w.decide(req)
			requireOutcome(t, out, tc.kind, tc.reason)
			switch {
			case tc.expiry == "" && out.ExpiryYield != nil:
				t.Fatalf("ExpiryYield = %+v, want nil", *out.ExpiryYield)
			case tc.expiry != "" && (out.ExpiryYield == nil || out.ExpiryYield.Reason != tc.expiry || out.ExpiryYield.Row != tc.expiryRow ||
				out.ExpiryYield.NextStep != mergeQueueYieldNextStep(tc.expiry)):
				t.Fatalf("ExpiryYield = %+v, want reason %s row %d", out.ExpiryYield, tc.expiry, tc.expiryRow)
			}
			if out.Yield != (mergeQueueYield{}) {
				t.Fatalf("immediate yield = %+v, want none on the base-move path (R4)", out.Yield)
			}
			if tc.noAnchor {
				if n := w.gh.snapshotCalls() - reads; n != 0 {
					t.Fatalf("snapshot reads = %d, want 0 (no established wait: no fold)", n)
				}
				if e := w.active(r); e.ApprovalWaitStartedAt != nil {
					t.Fatalf("approval_wait_started_at = %v, want NULL (the decision writes nothing)", e.ApprovalWaitStartedAt)
				}
			}
			w.requireNoMergeWrites(r)
			if n := len(w.rows(r, CategoryMergeQueueYielded)); n != 0 {
				t.Fatalf("merge_queue_yielded rows = %d, want 0 (the decision writes nothing)", n)
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
		decision     string
		mergeState   string
		snapshotFail bool
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
		// ADR-112 R3 rows 4/5: only REVIEW_REQUIRED, CHANGES_REQUESTED or
		// BLOCKED select row 4 — YIELD forge_review_required, next_step
		// approve_pr, the claim released; the default null decision (row
		// above) and a snapshot read error are row 5, eject forge_merge_refused.
		// Counterfactual: treating a null reviewDecision as review-required
		// yields the "null decision and clean state" row, RED.
		{name: "405 review required yields forge_review_required", err: fmt.Errorf("pinned: %w", forge.ErrPullRequestNotMergeable),
			decision: "REVIEW_REQUIRED", kind: mergeOnPassYield, reason: mergeQueueYieldForgeReviewRequired, released: 1},
		{name: "422 changes requested yields forge_review_required", err: fmt.Errorf("pinned: %w", forge.ErrMergeUnprocessable),
			decision: "CHANGES_REQUESTED", kind: mergeOnPassYield, reason: mergeQueueYieldForgeReviewRequired, released: 1},
		{name: "405 blocked yields forge_review_required", err: fmt.Errorf("pinned: %w", forge.ErrPullRequestNotMergeable),
			mergeState: "BLOCKED", kind: mergeOnPassYield, reason: mergeQueueYieldForgeReviewRequired, released: 1},
		{name: "405 null decision and clean state ejects forge_merge_refused", err: fmt.Errorf("pinned: %w", forge.ErrPullRequestNotMergeable),
			mergeState: "CLEAN", kind: mergeOnPassSettle, reason: mergeCandidateQueueReasonForgeMergeRefused, released: 1},
		{name: "405 snapshot read error ejects forge_merge_refused", err: fmt.Errorf("pinned: %w", forge.ErrPullRequestNotMergeable),
			decision: "REVIEW_REQUIRED", snapshotFail: true, kind: mergeOnPassSettle, reason: mergeCandidateQueueReasonForgeMergeRefused, released: 1},
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
			// The gate-less spec makes no pre-merge fold read, so the only
			// snapshot read is the post-refusal one.
			w.gh.setReviewSnapshot(r.pr, mcqReviewSnapshot{ReviewDecision: tc.decision, MergeStateStatus: tc.mergeState})
			if tc.snapshotFail {
				w.gh.set(func() { w.gh.graphQLFail[r.pr] = 5 })
			}
			out := w.decide(req)
			requireOutcome(t, out, tc.kind, tc.reason)
			if tc.kind == mergeOnPassYield && (out.Yield.NextStep != forgeApprovalNextApprovePR || out.Yield.Row != mergeQueueYieldRowForgeRefusal ||
				!strings.Contains(out.Yield.Detail, "reviewDecision")) {
				t.Fatalf("row-4 yield = %+v, want next_step approve_pr, precedence row 4", out.Yield)
			}
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

// mqmOnLockQueue runs onLock each time the run's merge-intent lock is taken,
// then takes the real lock; every other method is the embedded store.
type mqmOnLockQueue struct {
	mergequeue.Store
	onLock func()
}

func (q mqmOnLockQueue) WithMergeIntentLock(ctx context.Context, runID uuid.UUID, fn func(context.Context) error) error {
	q.onLock()
	return q.Store.WithMergeIntentLock(ctx, runID, fn)
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

// TestMergeOnPass_ForgeRefusalEjectsExactlyOnce pins ADR-093 D4 / ADR-112 R3
// rows 4 and 5 for every TYPED forge refusal of the pinned merge PUT, the
// unstable-status one included (first-officer ruling C-R4), through the
// PUMP: each case runs two pumps and the periodic sweep.
//   - Row 5 (null decision, clean state): EJECT forge_merge_refused naming the
//     forge's code, one merge_queue_ejected row with a sanitised forge_detail,
//     the claim released, the next entry admitted, and the merger called
//     EXACTLY once.
//   - Row 4 (REVIEW_REQUIRED, or mergeStateStatus BLOCKED): YIELD
//     forge_review_required (next_step approve_pr), no ejection row, merger
//     called once.
//
// MECHANISMS: the merger refuses on EVERY call and the merge-variant
// eligibility passes, so without the eject arm the released claim lets the
// next pump claim and dispatch again — replacing the eject return with
// held(transient), or deleting ErrPullRequestUnstableStatus from the arm, makes
// the unstable rows' merger count 2 (RED). Skipping the row-4 snapshot on the
// unstable arm turns its yield row RED (it ejects instead).
func TestMergeOnPass_ForgeRefusalEjectsExactlyOnce(t *testing.T) {
	// Built at runtime so no source line matches a credential pattern.
	token := "gh" + "s_" + strings.Repeat("S3", 18)
	cases := []struct {
		name     string
		err      error
		decision string
		state    string
		yield    bool
		code     string
	}{
		{name: "unstable status ejects checks_not_settled", err: fmt.Errorf("pinned merge (token %s): %w", token, forge.ErrPullRequestUnstableStatus),
			state: "CLEAN", code: mergeQueueForgeReasonChecksNotSettled},
		{name: "unstable status review required yields", err: fmt.Errorf("pinned: %w", forge.ErrPullRequestUnstableStatus),
			decision: "REVIEW_REQUIRED", yield: true},
		{name: "unstable status blocked yields", err: fmt.Errorf("pinned: %w", forge.ErrPullRequestUnstableStatus),
			state: "BLOCKED", yield: true},
		{name: "405 ejects not_mergeable", err: fmt.Errorf("pinned merge (token %s): %w", token, forge.ErrPullRequestNotMergeable),
			code: mergeQueueForgeReasonNotMergeable},
		{name: "405 review required yields", err: fmt.Errorf("pinned: %w", forge.ErrPullRequestNotMergeable),
			decision: "REVIEW_REQUIRED", yield: true},
		{name: "422 ejects unprocessable", err: fmt.Errorf("pinned merge (token %s): %w: %w", token, forge.ErrValidation, forge.ErrMergeUnprocessable),
			code: mergeQueueForgeReasonUnprocessable},
		{name: "422 changes requested yields", err: fmt.Errorf("pinned: %w: %w", forge.ErrValidation, forge.ErrMergeUnprocessable),
			decision: "CHANGES_REQUESTED", yield: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			ctx := context.Background()
			r, _ := w.ready("fr", "h1", mcvUndelegatedSpecYAML)
			b := w.seed("frb", "b0", []byte(mcvUndelegatedSpecYAML))
			if _, err := w.store.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: b.id, BaseRef: mcqBase, HeadSHA: "b0"}); err != nil {
				t.Fatalf("enqueue B: %v", err)
			}
			w.merger.err = tc.err
			w.gh.setReviewSnapshot(r.pr, mcqReviewSnapshot{ReviewDecision: tc.decision, MergeStateStatus: tc.state})
			w.pumpBase(r)
			w.pumpBase(r)
			w.srv.PumpMergeCandidateQueues(ctx)

			w.requireMerges(1)
			if len(w.rows(b, CategoryMergeCandidateQueueAdmitted)) == 0 {
				t.Fatal("the next entry was not admitted: the line is wedged")
			}
			if _, claimed, err := w.srv.mergeDispatchLedger().MergeDispatch(ctx, r.id); err != nil || claimed {
				t.Fatalf("dispatch claim present = %v (%v), want released", claimed, err)
			}
			if tc.yield {
				if e := w.active(r); e.State != mergequeue.StateHeld || e.YieldReason != mergeQueueYieldForgeReviewRequired {
					t.Fatalf("A = %s yielded %q, want held and yielded forge_review_required (row 4)", e.State, e.YieldReason)
				}
				if rows := w.ejectedRows(r); len(rows) != 0 {
					t.Fatalf("merge_queue_ejected rows = %v, want none on a row-4 yield", rows)
				}
				return
			}
			w.requireState(r, mergequeue.StateEjected, mergeCandidateQueueReasonForgeMergeRefused)
			p := w.oneEjection(r, mergeCandidateQueueReasonForgeMergeRefused, mergeQueueEjectNextMergeRun)
			detail := fmt.Sprint(p["forge_detail"])
			if p["forge_reason"] != tc.code || strings.Contains(detail, token) || !strings.Contains(detail, "[redacted]") {
				t.Fatalf("ejection forge_reason %v forge_detail %q, want %s with the token redacted", p["forge_reason"], detail, tc.code)
			}
			if tc.code == mergeQueueForgeReasonChecksNotSettled && (!strings.HasPrefix(detail, "the checks were not settled at the merge attempt") ||
				!strings.Contains(fmt.Sprint(p["remedy"]), "pending checks hold before dispatch")) {
				t.Fatalf("checks_not_settled detail %q / remedy %v, want the hold/eject asymmetry stated", detail, p["remedy"])
			}
			if _, ok := p["fixup_routed"]; ok {
				t.Fatalf("a forge refusal carries fixup_routed: %v", p)
			}
		})
	}
}

// TestMergeOnPass_ChecksNotSettledKeepsRealisticForgeText (#4705): a
// checks_not_settled forge_detail is the short fixed lead plus the forge's
// text sanitised BEFORE the lead is added, so a realistic unstable-status
// refusal (built the way githubclient builds it) survives the
// forgeApprovalDetailMaxBytes bound WHOLE at the outcome, the
// merge_queue_ejected write sink and the read projection. The lead is
// pinned here as a literal, independently of mergeQueueChecksNotSettledDetail,
// so the fixture preconditions hold whatever the production lead is: the
// sanitised forge text is at least 150 bytes (more than the old 165-byte
// lead left room for) and lead + text fit the bound. MECHANISMS: restoring
// the old 165-byte lead cuts the forge text at both sinks (RED on the
// outcome and on both sink equalities); composing with the raw
// mergeErr.Error() puts the token in the outcome's ForgeDetail (RED in
// "outcome" — the sinks re-sanitise, so only the outcome observes that
// control).
func TestMergeOnPass_ChecksNotSettledKeepsRealisticForgeText(t *testing.T) {
	// Built at runtime so no source line matches a credential pattern.
	token := "gh" + "s_" + strings.Repeat("S3", 18)
	refusal := fmt.Errorf("%w: %w: enable auto-merge: Pull request Pull request is in unstable status (installation token %s)",
		forge.ErrValidation, forge.ErrPullRequestUnstableStatus, token)
	const lead = "the checks were not settled at the merge attempt: "
	forgeText := mergeQueueYieldDetail(refusal.Error())
	if len(forgeText) < 150 || len(lead)+len(forgeText) > forgeApprovalDetailMaxBytes {
		t.Fatalf("fixture: sanitised forge text %d bytes, lead + text %d bytes; want >= 150 and <= %d",
			len(forgeText), len(lead)+len(forgeText), forgeApprovalDetailMaxBytes)
	}
	want := lead + forgeText
	t.Run("outcome", func(t *testing.T) {
		w := newMQMWorld(t)
		r, req := w.ready("ns", "h1", mcvUndelegatedSpecYAML)
		w.merger.err = refusal
		w.gh.setReviewSnapshot(r.pr, mcqReviewSnapshot{MergeStateStatus: "CLEAN"})
		out := w.decide(req)
		requireOutcome(t, out, mergeOnPassSettle, mergeCandidateQueueReasonForgeMergeRefused)
		if out.State != mergequeue.StateEjected || out.ForgeReason != mergeQueueForgeReasonChecksNotSettled {
			t.Fatalf("outcome state %s forge_reason %q, want ejected %s", out.State, out.ForgeReason, mergeQueueForgeReasonChecksNotSettled)
		}
		if !strings.HasPrefix(out.ForgeDetail, lead) || strings.Contains(out.ForgeDetail, token) || out.ForgeDetail != want {
			t.Fatalf("outcome forge_detail = %q, want %q (the lead, then the whole sanitised forge text)", out.ForgeDetail, want)
		}
	})
	t.Run("sinks", func(t *testing.T) {
		w := newMQMWorld(t)
		ctx := context.Background()
		r, _ := w.ready("ns", "h1", mcvUndelegatedSpecYAML)
		b := w.seed("nsb", "b0", []byte(mcvUndelegatedSpecYAML))
		if _, err := w.store.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: b.id, BaseRef: mcqBase, HeadSHA: "b0"}); err != nil {
			t.Fatalf("enqueue B: %v", err)
		}
		w.merger.err = refusal
		w.gh.setReviewSnapshot(r.pr, mcqReviewSnapshot{MergeStateStatus: "CLEAN"})
		w.pumpBase(r)

		w.requireMerges(1)
		if len(w.rows(b, CategoryMergeCandidateQueueAdmitted)) == 0 {
			t.Fatal("the next entry was not admitted: the line is wedged")
		}
		w.requireState(r, mergequeue.StateEjected, mergeCandidateQueueReasonForgeMergeRefused)
		p := w.oneEjection(r, mergeCandidateQueueReasonForgeMergeRefused, mergeQueueEjectNextMergeRun)
		if p["forge_reason"] != mergeQueueForgeReasonChecksNotSettled {
			t.Fatalf("merge_queue_ejected forge_reason = %v, want %s", p["forge_reason"], mergeQueueForgeReasonChecksNotSettled)
		}
		ej, found, err := w.store.LatestForRun(ctx, r.id)
		if err != nil || !found || ej.State != mergequeue.StateEjected {
			t.Fatalf("latest entry = %+v found %v err %v, want the ejected entry", ej, found, err)
		}
		written, read := fmt.Sprint(p["forge_detail"]), w.srv.mergeQueueEjectionProjectionFor(ctx, ej).ForgeDetail
		if written != want || read != want {
			t.Fatalf("forge_detail: write sink %q, read sink %q; want both %q (the lead, then the whole sanitised forge text)", written, read, want)
		}
	})
}

// TestMergeQueueOnPass_ReviewRequiredRefusalStillYields is the ADR-112 R3
// row-4 regression guard (first-officer ruling C-R4): a 405 refusal with a
// REVIEW_REQUIRED snapshot still YIELDS forge_review_required with next_step
// approve_pr at the decision seam and writes no merge_queue_ejected row.
func TestMergeQueueOnPass_ReviewRequiredRefusalStillYields(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("pinned: %w", forge.ErrPullRequestNotMergeable),
		fmt.Errorf("pinned: %w", forge.ErrPullRequestUnstableStatus),
	} {
		w := newMQMWorld(t)
		r, req := w.ready("rr", "h1", mcvUndelegatedSpecYAML)
		w.merger.err = err
		w.gh.setReviewSnapshot(r.pr, mcqReviewSnapshot{ReviewDecision: "REVIEW_REQUIRED"})
		out := w.decide(req)
		requireOutcome(t, out, mergeOnPassYield, mergeQueueYieldForgeReviewRequired)
		if out.Yield.NextStep != forgeApprovalNextApprovePR || out.ForgeReason != "" {
			t.Fatalf("yield = %+v forge_reason %q, want approve_pr and no forge reason", out.Yield, out.ForgeReason)
		}
		n := 0
		w.srv.applyMergeOnPassOutcome(context.Background(), req.Entry, out, &n)
		if rows := w.ejectedRows(r); len(rows) != 0 {
			t.Fatalf("merge_queue_ejected rows = %v, want none", rows)
		}
	}
}

// mqmProbeMerger runs probe inside the pinned merge call, then delegates.
type mqmProbeMerger struct {
	*mqmMerger
	probe func()
}

func (m mqmProbeMerger) MergePullRequestAtHead(ctx context.Context, runRow *run.Run, head string) error {
	m.probe()
	return m.mqmMerger.MergePullRequestAtHead(ctx, runRow, head)
}

// ADR-093 D8 (counterfactual B3): merge-on-pass records phase merging and
// marks the in-flight registry BEFORE the forge call (the claim is then
// dispatched), and settles merged after it.
func TestMergeOnPass_StampsMergingBeforeForgeCall(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("stamp", "h1", mcvUndelegatedSpecYAML)
	req = w.realLedger(req)
	var phase mergequeue.Phase
	var claimState mergequeue.DispatchState
	var inFlight bool
	w.srv.cfg.GateMerger = mqmProbeMerger{mqmMerger: w.merger, probe: func() {
		phase = w.storeEntry(r).Phase
		c, _, _ := w.store.MergeDispatch(context.Background(), r.id)
		claimState = c.State
		inFlight = w.srv.mergeInFlight.has(r.id)
	}}
	requireOutcome(t, w.decide(req), mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
	if phase != mergequeue.PhaseMerging || claimState != mergequeue.DispatchDispatched || !inFlight {
		t.Fatalf("during the forge call: phase %q claim %q in-flight %v, want merging, dispatched, true", phase, claimState, inFlight)
	}
	w.requireState(r, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
}

// Check-run listings the A3 step-4 tests plant (docs/adr/112 A3.1).
const (
	mqmPendingCI = `[{"id":1,"name":"ci","status":"in_progress","started_at":"2026-01-01T00:00:00Z","check_suite":{"id":1},"app":{"slug":"github-actions"}}]`
	mqmRedCI     = `[{"id":1,"name":"ci","status":"completed","conclusion":"failure","started_at":"2026-01-01T00:00:00Z","completed_at":"2026-01-01T00:01:00Z","check_suite":{"id":1},"app":{"slug":"github-actions"}}]`
	mqmRedLint   = `[{"id":1,"name":"lint","status":"completed","conclusion":"failure","started_at":"2026-01-01T00:00:00Z","completed_at":"2026-01-01T00:01:00Z","check_suite":{"id":1},"app":{"slug":"github-actions"}}]`
)

// mqmRequireCI names "ci" a required check of r (a RequiredChecksSnapshot),
// so a red "ci" is A3.1(a) checks_red rather than nil-snapshot not-green.
func (w *mqmWorld) mqmRequireCI(r *mcqRun) {
	w.t.Helper()
	w.exec(`UPDATE runs SET required_checks_snapshot = '{"contexts":["ci"]}' WHERE id = $1`, r.id)
}

// TestMergeOnPass_A32_Step4 pins docs/adr/112 A3.2's step-4 classification
// (#4780): a red required check is await_gate IMMEDIATE merge_checks_red /
// retrigger_ci; pending checks and a settling review round are await_gate
// (not immediate) merge_checks_pending / merge_review_pending (retry_after);
// pending beside review is merge_checks_pending (A3.5 tier 2); red beside a
// blocking concern settles dropped not_merge_eligible (a non-retryable leg);
// a non-check undeterminable leg holds not_mergeable_yet. The decision
// writes nothing: no settle, no yield, intent kept. CF22: restoring the drop
// for checks_red turns the red row's settle RED.
func TestMergeOnPass_A32_Step4(t *testing.T) {
	cases := []struct {
		name      string
		spec      string
		plant     func(w *mqmWorld, r *mcqRun)
		kind      mergeOnPassKind
		reason    string
		next      string
		immediate bool
	}{
		{"red yields at once", mcvUndelegatedSpecYAML, func(w *mqmWorld, r *mcqRun) {
			w.mqmRequireCI(r)
			w.gh.set(func() { w.gh.checks["h1"] = mqmRedCI })
		}, mergeOnPassAwaitGate, mergeQueueYieldMergeChecksRed, mergeQueueYieldNextRetriggerCI, true},
		{"pending awaits the gate", mcvUndelegatedSpecYAML, func(w *mqmWorld, _ *mcqRun) {
			w.gh.set(func() { w.gh.checks["h1"] = mqmPendingCI })
		}, mergeOnPassAwaitGate, mergeQueueYieldMergeChecksPending, forgeApprovalNextRetryAfter, false},
		{"nil snapshot optional red is pending", mcvUndelegatedSpecYAML, func(w *mqmWorld, _ *mcqRun) {
			w.gh.set(func() { w.gh.checks["h1"] = mqmRedLint })
		}, mergeOnPassAwaitGate, mergeQueueYieldMergeChecksPending, forgeApprovalNextRetryAfter, false},
		{"zero check runs is pending", mcvUndelegatedSpecYAML, func(w *mqmWorld, _ *mcqRun) {
			w.gh.set(func() { w.gh.checks["h1"] = `[]` })
		}, mergeOnPassAwaitGate, mergeQueueYieldMergeChecksPending, forgeApprovalNextRetryAfter, false},
		{"settling review awaits the gate", mqeGatedReviewersSpec, func(w *mqmWorld, r *mcqRun) {
			w.mqeAppend(r, "implement_review_started", map[string]any{"configured_agents": 1})
		}, mergeOnPassAwaitGate, mergeQueueYieldMergeReviewPending, forgeApprovalNextRetryAfter, false},
		{"pending beside review is merge_checks_pending", mqeGatedReviewersSpec, func(w *mqmWorld, r *mcqRun) {
			w.mqeAppend(r, "implement_review_started", map[string]any{"configured_agents": 1})
			w.gh.set(func() { w.gh.checks["h1"] = mqmPendingCI })
		}, mergeOnPassAwaitGate, mergeQueueYieldMergeChecksPending, forgeApprovalNextRetryAfter, false},
		{"red beside a blocking concern drops", mcvUndelegatedSpecYAML, func(w *mqmWorld, r *mcqRun) {
			w.mqmRequireCI(r)
			w.gh.set(func() { w.gh.checks["h1"] = mqmRedCI })
			w.raiseConcern(r, "high", "")
		}, mergeOnPassSettle, mergeCandidateQueueReasonNotMergeEligible, "", false},
		{"non-check undeterminable holds", mcvUndelegatedSpecYAML, func(w *mqmWorld, r *mcqRun) {
			// No concern repository: leg (d) is undeterminable, a
			// non-check leg, beside a pending (nil-snapshot red) check.
			w.gh.set(func() { w.gh.checks["h1"] = mqmRedLint })
			w.srv.cfg.ConcernRepo = nil
		}, mergeOnPassHeld, mergeOnPassReasonNotMergeable, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, req := w.ready("s4", "h1", tc.spec)
			tc.plant(w, r)
			out := w.decide(req)
			requireOutcome(t, out, tc.kind, tc.reason)
			if out.Immediate != tc.immediate {
				t.Fatalf("Immediate = %v, want %v", out.Immediate, tc.immediate)
			}
			if tc.kind == mergeOnPassAwaitGate {
				if out.Yield.Reason != tc.reason || out.Yield.NextStep != tc.next || out.Yield.Row != mergeQueueYieldRowEligibility {
					t.Fatalf("Yield = %+v, want reason %s next_step %s row %d", out.Yield, tc.reason, tc.next, mergeQueueYieldRowEligibility)
				}
				if out.ExpiryYield != nil {
					t.Fatalf("ExpiryYield = %+v, want nil (a merge-gate wait is not an approval wait)", *out.ExpiryYield)
				}
			}
			if tc.kind == mergeOnPassSettle && out.State != mergequeue.StateDropped {
				t.Fatalf("state = %q, want dropped", out.State)
			}
			e := w.active(r)
			if e.State != mergequeue.StateLive || e.MergeIntentAt == nil || e.YieldedAt != nil {
				t.Fatalf("entry = %s intent=%v yielded_at=%v, want live, intent kept, unmarked (the decision writes nothing)", e.State, e.MergeIntentAt, e.YieldedAt)
			}
			w.requireMerges(0)
			if len(w.ledger.claims) != 0 {
				t.Fatalf("ledger claimed %v before an A3 step-4 outcome", w.ledger.claims)
			}
		})
	}
}

// mqmGateWaitErrQueue fails every GateWait write.
type mqmGateWaitErrQueue struct{ mergequeue.Store }

func (mqmGateWaitErrQueue) GateWait(context.Context, uuid.UUID, int64, time.Duration) (mergequeue.GateWaitResult, error) {
	return mergequeue.GateWaitResult{}, errors.New("gate wait write failed")
}

// mqmGateDecision seeds a ready entry at h1 with listing planted at h1,
// decides it and returns the request and the step-4 outcome.
func (w *mqmWorld) mqmGateDecision(listing string, red bool) (*mcqRun, mergeOnPassRequest, mergeOnPassOutcome) {
	w.t.Helper()
	r, req := w.ready("gw", "h1", mcvUndelegatedSpecYAML)
	if red {
		w.mqmRequireCI(r)
	}
	w.gh.set(func() { w.gh.checks["h1"] = listing })
	out := w.decide(req)
	if out.Kind != mergeOnPassAwaitGate {
		w.t.Fatalf("decision = %s/%s, want await_gate", out.Kind, out.Reason)
	}
	return r, req, out
}

// TestMergeOnPassEnforceGateWait pins docs/adr/112 A3.2's merge-gate wait
// enforcement, one subtest per mode: an open window holds (await_gate, the
// pump stops, no marker); an expired window yields the step-4 reason; an
// empty hold budget yields at once; a red (immediate) yield is refused by a
// claimed D1 dispatch, the in-flight registry and a changed entry (each
// held, no yield); a GateWait write error holds. CF23: deleting the
// mergeOnPassPreYieldRefusal call turns the claimed-dispatch, in-flight and
// changed-entry subtests RED (each yields).
func TestMergeOnPassEnforceGateWait(t *testing.T) {
	ctx := context.Background()
	requireNoYield := func(w *mqmWorld, r *mcqRun) {
		w.t.Helper()
		if e := w.active(r); e.State != mergequeue.StateLive || e.YieldedAt != nil {
			w.t.Fatalf("entry = %s yielded_at=%v, want live and unmarked", e.State, e.YieldedAt)
		}
		if n := len(w.rows(r, CategoryMergeQueueYielded)); n != 0 {
			w.t.Fatalf("merge_queue_yielded rows = %d, want 0", n)
		}
	}
	t.Run("open window holds", func(t *testing.T) {
		w := newMQMWorld(t)
		r, req, out := w.mqmGateDecision(mqmPendingCI, false)
		got := w.srv.mergeOnPassEnforceGateWait(ctx, req, out)
		requireOutcome(t, got, mergeOnPassAwaitGate, mergeQueueYieldMergeChecksPending)
		if got.GateRemaining <= 0 {
			t.Fatalf("GateRemaining = %s, want the open window's remaining time", got.GateRemaining)
		}
		requireNoYield(w, r)
		e := w.active(r)
		if e.GateWaitStartedAt == nil || e.GateWaitEpoch == nil || *e.GateWaitEpoch != req.Entry.AdmissionEpoch || e.GateHoldTokens != mergequeue.GateHoldBudget-1 {
			t.Fatalf("gate window = %v epoch %v tokens %d, want opened at epoch %d with one token spent", e.GateWaitStartedAt, e.GateWaitEpoch, e.GateHoldTokens, req.Entry.AdmissionEpoch)
		}
		if e.ApprovalWaitStartedAt != nil {
			t.Fatalf("approval_wait_started_at = %v, want NULL (a merge-gate hold never touches it)", e.ApprovalWaitStartedAt)
		}
		readvances := 0
		if w.srv.applyMergeOnPassOutcome(ctx, req.Entry, got, &readvances) {
			t.Fatal("the pump continued past an open merge-gate window, want it to stop")
		}
		requireNoYield(w, r)
	})
	t.Run("expired window yields the step-4 reason", func(t *testing.T) {
		w := newMQMWorld(t)
		r, req, out := w.mqmGateDecision(mqmPendingCI, false)
		if got := w.srv.mergeOnPassEnforceGateWait(ctx, req, out); got.Kind != mergeOnPassAwaitGate {
			t.Fatalf("first hold = %s, want await_gate", got.Kind)
		}
		w.exec(`UPDATE merge_candidate_queue_entries SET gate_wait_started_at = gate_wait_started_at - interval '2 hours' WHERE id = $1`, req.Entry.ID)
		got := w.srv.mergeOnPassEnforceGateWait(ctx, req, out)
		requireOutcome(t, got, mergeOnPassExpiredYielded, mergeQueueYieldMergeChecksPending)
		e := w.requireYielded(r, mergeQueueYieldMergeChecksPending, forgeApprovalNextRetryAfter, mergeQueueYieldSiteMergeOnPass)
		if e.MergeIntentAt == nil || e.ApprovalWaitStartedAt != nil {
			t.Fatalf("yielded entry intent=%v approval anchor=%v, want intent kept and no approval anchor", e.MergeIntentAt, e.ApprovalWaitStartedAt)
		}
	})
	t.Run("no hold token yields at once", func(t *testing.T) {
		w := newMQMWorld(t)
		r, req, out := w.mqmGateDecision(mqmPendingCI, false)
		w.exec(`UPDATE merge_candidate_queue_entries SET gate_hold_tokens = 0, gate_tokens_refilled_at = clock_timestamp() WHERE id = $1`, req.Entry.ID)
		got := w.srv.mergeOnPassEnforceGateWait(ctx, req, out)
		requireOutcome(t, got, mergeOnPassExpiredYielded, mergeQueueYieldMergeChecksPending)
		w.requireYielded(r, mergeQueueYieldMergeChecksPending, forgeApprovalNextRetryAfter, mergeQueueYieldSiteMergeOnPass)
	})
	t.Run("red yields at once with no window", func(t *testing.T) {
		w := newMQMWorld(t)
		r, req, out := w.mqmGateDecision(mqmRedCI, true)
		got := w.srv.mergeOnPassEnforceGateWait(ctx, req, out)
		requireOutcome(t, got, mergeOnPassExpiredYielded, mergeQueueYieldMergeChecksRed)
		e := w.requireYielded(r, mergeQueueYieldMergeChecksRed, mergeQueueYieldNextRetriggerCI, mergeQueueYieldSiteMergeOnPass)
		if e.GateWaitStartedAt != nil || e.GateHoldTokens != mergequeue.GateHoldBudget {
			t.Fatalf("gate window = %v tokens %d, want none opened and the budget untouched", e.GateWaitStartedAt, e.GateHoldTokens)
		}
		p := w.payload(w.rows(r, CategoryMergeQueueYielded)[0])
		if int(p["precedence_row"].(float64)) != mergeQueueYieldRowEligibility {
			t.Fatalf("precedence_row = %v, want %d", p["precedence_row"], mergeQueueYieldRowEligibility)
		}
	})
	refusals := []struct {
		name   string
		mut    func(w *mqmWorld, r *mcqRun, req *mergeOnPassRequest)
		reason string
	}{
		{"claimed dispatch holds", func(w *mqmWorld, r *mcqRun, _ *mergeOnPassRequest) {
			if _, claimed, err := w.ledger.ClaimMergeDispatch(ctx, r.id, "inline", "h1"); err != nil || !claimed {
				t.Fatalf("seed claim: claimed=%v err=%v", claimed, err)
			}
		}, mergeOnPassReasonAlreadyDispatched},
		{"in-flight registry holds", func(w *mqmWorld, r *mcqRun, _ *mergeOnPassRequest) {
			w.srv.mergeInFlight.mark(r.id)
		}, mergeOnPassReasonAlreadyDispatched},
		{"changed entry holds", func(w *mqmWorld, _ *mcqRun, req *mergeOnPassRequest) {
			w.exec(`UPDATE merge_candidate_queue_entries SET admission_epoch = admission_epoch + 1 WHERE id = $1`, req.Entry.ID)
		}, mergeOnPassReasonEntryChanged},
		{"no ledger holds", func(_ *mqmWorld, _ *mcqRun, req *mergeOnPassRequest) { req.Ledger = nil }, mergeOnPassReasonLedgerUnavailable},
	}
	for _, rc := range refusals {
		t.Run(rc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, req, out := w.mqmGateDecision(mqmRedCI, true)
			rc.mut(w, r, &req)
			got := w.srv.mergeOnPassEnforceGateWait(ctx, req, out)
			requireOutcome(t, got, mergeOnPassHeld, rc.reason)
			if got.ExpiryYield != nil {
				t.Fatalf("refusal carries ExpiryYield %+v, want none", *got.ExpiryYield)
			}
			if e := w.storeEntry(r); e.State != mergequeue.StateLive {
				t.Fatalf("entry = %s, want live (refused, not yielded)", e.State)
			}
			if n := len(w.rows(r, CategoryMergeQueueYielded)); n != 0 {
				t.Fatalf("merge_queue_yielded rows = %d, want 0", n)
			}
		})
	}
	t.Run("gate wait write error holds", func(t *testing.T) {
		w := newMQMWorld(t)
		r, req, out := w.mqmGateDecision(mqmPendingCI, false)
		w.srv.cfg.MergeQueue = mqmGateWaitErrQueue{w.store}
		got := w.srv.mergeOnPassEnforceGateWait(ctx, req, out)
		requireOutcome(t, got, mergeOnPassHeld, mergeOnPassReasonEntryChanged)
		w.srv.cfg.MergeQueue = w.store
		requireNoYield(w, r)
	})
	t.Run("lock unavailable holds", func(t *testing.T) {
		w := newMQMWorld(t)
		r, req, out := w.mqmGateDecision(mqmRedCI, true)
		w.srv.cfg.MergeQueue = mqmLockQueue{Store: w.store, lockErr: errors.New("lock busy")}
		got := w.srv.mergeOnPassEnforceGateWait(ctx, req, out)
		w.srv.cfg.MergeQueue = w.store
		requireOutcome(t, got, mergeOnPassHeld, mergeOnPassReasonLockUnavailable)
		requireNoYield(w, r)
	})
	t.Run("other kinds pass through", func(t *testing.T) {
		w := newMQMWorld(t)
		o := mergeOnPassOutcome{Kind: mergeOnPassHeld, Reason: mergeOnPassReasonTransient}
		if got := w.srv.mergeOnPassEnforceGateWait(ctx, mergeOnPassRequest{}, o); got.Kind != o.Kind || got.Reason != o.Reason {
			t.Fatalf("got %s/%s, want the outcome unchanged", got.Kind, got.Reason)
		}
	})
}

// TestMergeOnPass_GateHoldNeverSetsApprovalAnchor (docs/adr/112 A3.2/A3.7):
// a (b) hold and its expiry yield, driven through the pump's real path
// (decide, both enforcements, apply), leave approval_wait_started_at NULL.
// CF24: calling ApprovalWait in the await_gate arm stamps it, RED.
func TestMergeOnPass_GateHoldNeverSetsApprovalAnchor(t *testing.T) {
	ctx := context.Background()
	w := newMQMWorld(t)
	r, req := w.ready("ga", "h1", mcvUndelegatedSpecYAML)
	w.gh.set(func() { w.gh.checks["h1"] = mqmPendingCI })
	step := func() bool {
		o := w.srv.mergeOnPassEnforceExpiredWait(ctx, req, w.decide(req))
		o = w.srv.mergeOnPassEnforceGateWait(ctx, req, o)
		readvances := 0
		return w.srv.applyMergeOnPassOutcome(ctx, req.Entry, o, &readvances)
	}
	if step() {
		t.Fatal("the pump continued past the open window")
	}
	if e := w.active(r); e.ApprovalWaitStartedAt != nil || e.GateWaitStartedAt == nil {
		t.Fatalf("after the hold: approval anchor %v gate window %v, want NULL and set", e.ApprovalWaitStartedAt, e.GateWaitStartedAt)
	}
	w.exec(`UPDATE merge_candidate_queue_entries SET gate_wait_started_at = gate_wait_started_at - interval '2 hours' WHERE id = $1`, req.Entry.ID)
	if !step() {
		t.Fatal("the pump stopped after the expired merge-gate wait yielded")
	}
	e := w.requireYielded(r, mergeQueueYieldMergeChecksPending, forgeApprovalNextRetryAfter, mergeQueueYieldSiteMergeOnPass)
	if e.ApprovalWaitStartedAt != nil || e.ApprovalWaitOpen {
		t.Fatalf("after the yield: approval anchor %v open=%v, want NULL and closed", e.ApprovalWaitStartedAt, e.ApprovalWaitOpen)
	}
}

// TestMergeOnPass_GateWindowStartsAtFirstHoldNotAdmission (docs/adr/112
// A3.2): with admitted_at backdated far past the window, the first (b) hold
// opens the window NOW (await_gate, not a yield); CI turns green inside it
// and the next decision merges. CF25: a window measured from admitted_at
// yields on the first hold, RED.
func TestMergeOnPass_GateWindowStartsAtFirstHoldNotAdmission(t *testing.T) {
	ctx := context.Background()
	w := newMQMWorld(t)
	r, req := w.ready("gs", "h1", mcvUndelegatedSpecYAML)
	w.exec(`UPDATE merge_candidate_queue_entries SET admitted_at = clock_timestamp() - interval '3 hours' WHERE id = $1`, req.Entry.ID)
	req.Entry = w.active(r)
	w.gh.set(func() { w.gh.checks["h1"] = mqmPendingCI })
	got := w.srv.mergeOnPassEnforceGateWait(ctx, req, w.decide(req))
	requireOutcome(t, got, mergeOnPassAwaitGate, mergeQueueYieldMergeChecksPending)
	w.gh.set(func() { w.gh.checks["h1"] = mcqGreenChecks })
	requireOutcome(t, w.srv.mergeOnPassEnforceGateWait(ctx, req, w.decide(req)), mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
	w.requireMerges(1)
}

// TestMergeQueueGateWait_DefaultsToApprovalWait pins the knob's fallback
// (docs/adr/112 A3.2): zero answers the approval wait (itself defaulted), a
// positive value answers itself.
func TestMergeQueueGateWait_DefaultsToApprovalWait(t *testing.T) {
	s := &Server{}
	if got := s.mergeQueueGateWait(); got != DefaultMergeQueueApprovalWait {
		t.Fatalf("zero config = %s, want the approval wait default %s", got, DefaultMergeQueueApprovalWait)
	}
	s.cfg.MergeQueueApprovalWait = 7 * time.Minute
	if got := s.mergeQueueGateWait(); got != 7*time.Minute {
		t.Fatalf("zero gate wait = %s, want the approval wait 7m", got)
	}
	s.cfg.MergeQueueGateWait = 3 * time.Minute
	if got := s.mergeQueueGateWait(); got != 3*time.Minute {
		t.Fatalf("gate wait = %s, want 3m", got)
	}
}

// ADR-109 Amendment A1 (#4711): merge-on-pass step 8 releases an orphaned
// QUEUE claim. Each test seeds the crash-left state with mqrCrashLeft (a real
// 502 decision, the claim put back to 'dispatched' with claimed_at backdated
// against clock_timestamp(), #3048), returns the entry to phase passed in SQL
// and decides DIRECTLY over the real ledger: routing through the resume would
// let its own young / non-queue holds mask the step-8 guards.

// mqmOrphan seeds run a on spec with alice's approval at h1, crash-left at
// claimedAt and back in phase passed, and returns the real-ledger request.
func (w *mqmWorld) mqmOrphan(spec, claimedAt string) (*mcqRun, mergeOnPassRequest) {
	w.t.Helper()
	a := w.seed("a", "h1", []byte(spec))
	w.mqeRebased(a, "pre-h1", "h1", "h1", false)
	w.mqePassed(a, "h1")
	w.forgeReviews(a, []string{"alice"}, mqmReview(1, "alice", "APPROVED", "h1"))
	w.mqrCrashLeft(a, claimedAt)
	w.exec(`UPDATE merge_candidate_queue_entries SET phase = 'passed' WHERE run_id = $1 AND state = 'live'`, a.id)
	e := w.active(a)
	return a, w.realLedger(mergeOnPassRequest{Entry: e, IntentSequence: e.MergeIntentSequence})
}

// mqmReleasedRows counts a's merge_dispatch_settled rows with outcome
// released.
func (w *mqmWorld) mqmReleasedRows(a *mcqRun) int {
	n := 0
	for _, row := range w.rows(a, categoryMergeDispatchSettled) {
		if w.payload(row)["outcome"] == string(mergeDispatchRelease) {
			n++
		}
	}
	return n
}

// mqmRequireClaimUnchanged asserts a's stored claim still matches want.
func (w *mqmWorld) mqmRequireClaimUnchanged(a *mcqRun, want mergequeue.DispatchClaim) {
	w.t.Helper()
	got := w.mqrClaim(a)
	if got.State != want.State || got.Path != want.Path || got.HeadSHA != want.HeadSHA ||
		got.FailureCause != want.FailureCause || !got.ClaimedAt.Equal(want.ClaimedAt) {
		w.t.Fatalf("claim = %+v, want unchanged %+v", got, want)
	}
}

// DONE-MEANS (A1): an aged queue claim still 'dispatched' whose PR reads open
// at h1 is released in step 8 (one merge_dispatch_settled {released,
// released_orphaned_claim} row AFTER this decision's fresh R4 row), claimed
// afresh and merged ONCE through the ordinary pinned merge. MECHANISM: the
// fixture's claim still stands, so without the release ClaimMergeDispatch
// answers claimed=false and the decision holds merge_already_dispatched with
// zero merges.
func TestMergeQueueMergeOnPass_OrphanedQueueClaimReleasedThenMerges(t *testing.T) {
	w := newMQMWorld(t)
	a, req := w.mqmOrphan(mqeSpecYAML, mqrAged)
	seed := w.mqrClaim(a)
	authBefore, settledBefore := w.rows(a, CategoryMergeQueueAuthorized), w.rows(a, categoryMergeDispatchSettled)
	before := w.mqrCount("h1")
	requireOutcome(t, w.decide(req), mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
	if got := w.mqrCount("h1"); got != before+1 {
		t.Fatalf("pinned merges at h1 = %d, want %d (one ordinary merge)", got, before+1)
	}
	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	settled := w.rows(a, categoryMergeDispatchSettled)
	if len(settled) != len(settledBefore)+1 {
		t.Fatalf("merge_dispatch_settled rows = %d, want %d", len(settled), len(settledBefore)+1)
	}
	released := settled[len(settled)-1]
	sp := w.payload(released)
	if sp["outcome"] != string(mergeDispatchRelease) || sp["reason"] != mergeDispatchReasonReleasedOrphanedClaim ||
		sp["path"] != mergeDispatchPathQueue || sp["head_sha"] != "h1" {
		t.Fatalf("merge_dispatch_settled = %v, want released/released_orphaned_claim for queue@h1", sp)
	}
	auth := w.rows(a, CategoryMergeQueueAuthorized)
	if len(auth) != len(authBefore)+1 {
		t.Fatalf("merge_queue_authorized rows = %d, want %d (a fresh R4 row)", len(auth), len(authBefore)+1)
	}
	fresh := auth[len(auth)-1]
	for _, row := range append(append([]*audit.Entry{}, authBefore...), settledBefore...) {
		if fresh.Sequence <= row.Sequence {
			t.Fatalf("fresh merge_queue_authorized sequence %d is not after the seeding decision's row %d", fresh.Sequence, row.Sequence)
		}
	}
	if released.Sequence <= fresh.Sequence {
		t.Fatalf("released row %d precedes this decision's R4 row %d; A1 releases only after steps 1-7", released.Sequence, fresh.Sequence)
	}
	if c := w.mqrClaim(a); c.State != mergequeue.DispatchDispatched || c.Path != mergeDispatchPathQueue || c.HeadSHA != "h1" ||
		!c.ClaimedAt.After(seed.ClaimedAt) {
		t.Fatalf("claim = %+v, want a FRESH dispatched queue claim at h1 claimed after %v", c, seed.ClaimedAt)
	}
}

// TestMergeQueueMergeOnPass_OrphanReleaseGuards: one row per A1 guard. Every
// row is the released fixture but for its one condition, and holds
// merge_already_dispatched (or, unapproved, awaits approval before step 8)
// with zero merges, zero released rows and the claim read back unchanged.
// (a) young (130s, inside the 150s window): CF force the crash-left check
// true → released + merged. (b)/(c) inline/delegated: CF delete the path
// condition → released + merged. (d) dispatch_failed queue claim: the real
// CAS (state = 'dispatched') would refuse a release, so the row asserts zero
// released ROWS: with the state condition deleted the row-first ordering
// writes one. (e) a zero claimed_at (the fake ledger's claim): CF delete
// mergeQueueClaimCrashLeft's IsZero return → the fake releases and merges.
// (f) an unapproved fold returns await_approval at step 6b, so step 8 never
// runs: the claim holds the head until approval arrives (ADR-109 A1 point 2).
func TestMergeQueueMergeOnPass_OrphanReleaseGuards(t *testing.T) {
	cases := []struct {
		name      string
		spec, age string
		setup     func(w *mqmWorld, a *mcqRun)
		kind      mergeOnPassKind
		reason    string
	}{
		{name: "young queue claim", spec: mcvUndelegatedSpecYAML, age: mqrYoung,
			kind: mergeOnPassHeld, reason: mergeOnPassReasonAlreadyDispatched},
		{name: "inline claim", spec: mcvUndelegatedSpecYAML, age: mqrAged, setup: func(w *mqmWorld, a *mcqRun) {
			w.exec(`UPDATE merge_dispatch_claims SET path = 'inline' WHERE run_id = $1`, a.id)
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonAlreadyDispatched},
		{name: "delegated claim", spec: mcvUndelegatedSpecYAML, age: mqrAged, setup: func(w *mqmWorld, a *mcqRun) {
			w.exec(`UPDATE merge_dispatch_claims SET path = 'delegated' WHERE run_id = $1`, a.id)
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonAlreadyDispatched},
		{name: "dispatch_failed queue claim", spec: mcvUndelegatedSpecYAML, age: mqrAged, setup: func(w *mqmWorld, a *mcqRun) {
			w.exec(`UPDATE merge_dispatch_claims SET state = 'dispatch_failed', failed_at = clock_timestamp(), failure_cause = $2 WHERE run_id = $1`,
				a.id, mergeQueueResumeReasonOrphanedByRestart)
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonAlreadyDispatched},
		{name: "fold not approved", spec: mqeSpecYAML, age: mqrAged, setup: func(w *mqmWorld, a *mcqRun) {
			w.forgeReviews(a, nil)
		}, kind: mergeOnPassAwaitApproval, reason: mergeCandidateQueueReasonForgeApprovalAbsent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			a, req := w.mqmOrphan(tc.spec, tc.age)
			if tc.setup != nil {
				tc.setup(w, a)
			}
			want := w.mqrClaim(a)
			releasedBefore, before := w.mqmReleasedRows(a), w.mqrCount("h1")
			requireOutcome(t, w.decide(req), tc.kind, tc.reason)
			if got := w.mqrCount("h1"); got != before {
				t.Fatalf("pinned merges = %d, want %d", got, before)
			}
			if n := w.mqmReleasedRows(a); n != releasedBefore {
				t.Fatalf("released merge_dispatch_settled rows = %d, want %d", n, releasedBefore)
			}
			w.mqmRequireClaimUnchanged(a, want)
		})
	}
	t.Run("zero claimed_at", func(t *testing.T) {
		w := newMQMWorld(t)
		r, req := w.ready("zc", "h1", mcvUndelegatedSpecYAML)
		w.ledger.claimed[r.id], w.ledger.heads[r.id] = mergeDispatchPathQueue, "h1"
		requireOutcome(t, w.decide(req), mergeOnPassHeld, mergeOnPassReasonAlreadyDispatched)
		w.requireMerges(0)
		if len(w.ledger.released) != 0 || w.mqmReleasedRows(r) != 0 {
			t.Fatalf("released = %v rows %d, want none (a zero claimed_at is not crash-left)", w.ledger.released, w.mqmReleasedRows(r))
		}
	})
}

// mqmReleaseErrQueue fails ReleaseMergeDispatch; mqmReclaimErrQueue fails
// every ClaimMergeDispatch after the first.
type mqmReleaseErrQueue struct{ *mergequeue.PostgresStore }

func (mqmReleaseErrQueue) ReleaseMergeDispatch(context.Context, mergequeue.DispatchClaim) error {
	return errors.New("injected release failure")
}

type mqmReclaimErrQueue struct {
	*mergequeue.PostgresStore
	calls *atomic.Int32
}

func (q mqmReclaimErrQueue) ClaimMergeDispatch(ctx context.Context, runID uuid.UUID, path, head string) (mergequeue.DispatchClaim, error) {
	if q.calls.Add(1) > 1 {
		return mergequeue.DispatchClaim{}, errors.New("injected re-claim failure")
	}
	return q.PostgresStore.ClaimMergeDispatch(ctx, runID, path, head)
}

// TestMergeQueueMergeOnPass_OrphanReleaseFailsClosed: (a) the strict
// merge_dispatch_settled append fails → hold orphaned_claim_release_failed,
// zero merges and the COMMITTED claim unchanged (CF: ignore the append error
// → release + merge); (b) the release CAS fails → the same named hold, after
// the row (record before action; CF: ignore the release error → the re-claim
// conflicts and the decision holds merge_already_dispatched instead, RED on
// the REASON only); (c) the fresh claim after a release fails → transient,
// no claim row, and the next decision claims and merges (coverage of the
// re-claim error path). (a) and (b) then drive TWO pump ticks: each tick's
// resume hands the merging entry back (ONE merge_queue_resumed redecide row
// per tick) and its merge-on-pass fails the release again, leaving phase
// merging (approval condition 4).
func TestMergeQueueMergeOnPass_OrphanReleaseFailsClosed(t *testing.T) {
	ticks := func(t *testing.T, w *mqmWorld, a *mcqRun, want mergequeue.DispatchClaim, releasedPerTick int) {
		t.Helper()
		w.srv.mergeInFlight.clear(a.id) // the pump clears the decision's own mark
		released := w.mqmReleasedRows(a)
		before := w.mqrCount("h1")
		for tick := 1; tick <= 2; tick++ {
			w.srv.PumpMergeCandidateQueues(context.Background())
			if e := w.storeEntry(a); e.State != mergequeue.StateLive || e.Phase != mergequeue.PhaseMerging {
				t.Fatalf("tick %d: A = %s/%s, want live merging", tick, e.State, e.Phase)
			}
			res := w.mqrResumed(a)
			if len(res) != tick {
				t.Fatalf("tick %d: merge_queue_resumed rows = %d, want exactly %d (one redecide per tick)", tick, len(res), tick)
			}
			if p := res[tick-1]; p["action"] != mergeQueueResumeActionRedecide || p["reason"] != mergeQueueResumeReasonOrphanedClaimOpen {
				t.Fatalf("tick %d: merge_queue_resumed = %v, want redecide/orphaned_claim_open", tick, p)
			}
			if n := w.mqmReleasedRows(a); n != released+tick*releasedPerTick {
				t.Fatalf("tick %d: released rows = %d, want %d", tick, n, released+tick*releasedPerTick)
			}
			if got := w.mqrCount("h1"); got != before {
				t.Fatalf("tick %d: pinned merges = %d, want %d", tick, got, before)
			}
			w.mqmRequireClaimUnchanged(a, want)
		}
	}
	t.Run("settled row append fails", func(t *testing.T) {
		w := newMQMWorld(t)
		a, req := w.mqmOrphan(mcvUndelegatedSpecYAML, mqrAged)
		want := w.mqrClaim(a)
		before := w.mqrCount("h1")
		w.audit.failCategory = categoryMergeDispatchSettled
		requireOutcome(t, w.decide(req), mergeOnPassHeld, mergeOnPassReasonOrphanReleaseFailed)
		if got := w.mqrCount("h1"); got != before {
			t.Fatalf("pinned merges = %d, want %d", got, before)
		}
		w.mqmRequireClaimUnchanged(a, want)
		ticks(t, w, a, want, 0)
	})
	t.Run("release fails", func(t *testing.T) {
		w := newMQMWorld(t)
		a, _ := w.mqmOrphan(mcvUndelegatedSpecYAML, mqrAged)
		want := w.mqrClaim(a)
		released, before := w.mqmReleasedRows(a), w.mqrCount("h1")
		w.srv.cfg.MergeQueue = mqmReleaseErrQueue{w.store}
		e := w.active(a)
		req := w.realLedger(mergeOnPassRequest{Entry: e, IntentSequence: e.MergeIntentSequence})
		requireOutcome(t, w.decide(req), mergeOnPassHeld, mergeOnPassReasonOrphanReleaseFailed)
		if got := w.mqrCount("h1"); got != before {
			t.Fatalf("pinned merges = %d, want %d", got, before)
		}
		if n := w.mqmReleasedRows(a); n != released+1 {
			t.Fatalf("released rows = %d, want %d (recorded before the failed CAS)", n, released+1)
		}
		w.mqmRequireClaimUnchanged(a, want)
		ticks(t, w, a, want, 1)
	})
	t.Run("re-claim fails after the release", func(t *testing.T) {
		w := newMQMWorld(t)
		a, _ := w.mqmOrphan(mcvUndelegatedSpecYAML, mqrAged)
		before := w.mqrCount("h1")
		w.srv.cfg.MergeQueue = mqmReclaimErrQueue{PostgresStore: w.store, calls: new(atomic.Int32)}
		e := w.active(a)
		req := w.realLedger(mergeOnPassRequest{Entry: e, IntentSequence: e.MergeIntentSequence})
		requireOutcome(t, w.decide(req), mergeOnPassHeld, mergeOnPassReasonTransient)
		if got := w.mqrCount("h1"); got != before {
			t.Fatalf("pinned merges = %d, want %d", got, before)
		}
		w.requireClaim(a, "")
		w.srv.cfg.MergeQueue = w.store
		requireOutcome(t, w.decide(w.realLedger(req)), mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
		if got := w.mqrCount("h1"); got != before+1 {
			t.Fatalf("pinned merges after recovery = %d, want %d", got, before+1)
		}
	})
}
