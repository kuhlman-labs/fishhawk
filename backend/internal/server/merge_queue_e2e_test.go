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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// ADR-093 slice 2 (E87.5 / #4296), the merge_run slice: HTTP-level tests over
// POST /v0/runs/{id}/merge, the real Postgres queue, the real D1 merge-dispatch
// ledger (merge_dispatch_claims), the mcq fake GitHub and merge-on-pass. They
// reuse the mqmWorld fixture read-only and wrap its merger so the inline
// (unpinned) dispatch is countable and can fail.

// mqeMerger wraps the recording pinned merger with a countable, failable
// inline MergePullRequest (it models auto-merge: it never merges the PR).
type mqeMerger struct {
	*mqmMerger
	mu         sync.Mutex
	inlineErr  error
	inlineN    int
	inlineHook func(ctx context.Context) error
}

func (m *mqeMerger) MergePullRequest(ctx context.Context, _ *run.Run) error {
	m.mu.Lock()
	m.inlineN++
	err, hook := m.inlineErr, m.inlineHook
	m.mu.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	return err
}

func (m *mqeMerger) inlineCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inlineN
}

// newMQEWorld is an mqmWorld whose GateMerger is an mqeMerger.
func newMQEWorld(t *testing.T) (*mqmWorld, *mqeMerger) {
	t.Helper()
	w := newMQMWorld(t)
	m := &mqeMerger{mqmMerger: w.merger}
	w.srv.cfg.GateMerger = m
	// A merged PR reads with a merge commit and merged_at, as GitHub
	// answers, so the merge_run observe ladder can confirm the merge.
	orig := w.srv.cfg.GitHub
	target, err := url.Parse(orig.BaseURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.Request.Method != http.MethodGet || !mqmPullPath.MatchString(resp.Request.URL.Path) || resp.StatusCode != http.StatusOK {
			return nil
		}
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			return err
		}
		if merged, _ := body["merged"].(bool); merged {
			body["merge_commit_sha"] = "merge-" + fmt.Sprint(body["number"])
			body["merged_at"] = "2026-10-21T00:00:00Z"
		}
		out, _ := json.Marshal(body)
		resp.Body = io.NopCloser(bytes.NewReader(out))
		resp.ContentLength = int64(len(out))
		resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
		return nil
	}
	ps := httptest.NewServer(proxy)
	t.Cleanup(ps.Close)
	w.srv.cfg.GitHub = &githubclient.Client{BaseURL: ps.URL, Tokens: mqmTokens{}, HTTP: orig.HTTP, AppJWT: orig.AppJWT}
	return w, m
}

// verified seeds a run at head, advanced cleanly onto it and passed there,
// up to date with the base: the merge-candidate gate admits it inline.
func (w *mqmWorld) verified(name, head string) *mcqRun {
	w.t.Helper()
	r := w.seed(name, head, []byte(mcvUndelegatedSpecYAML))
	w.mqeRebased(r, "pre-"+head, head, head, false)
	w.mqePassed(r, head)
	return r
}

// intentEntry enqueues r live at head with merge intent stamped at its
// merge_queue_entered row's sequence, in phase, and returns the sequence.
func (w *mqmWorld) intentEntry(r *mcqRun, head string, phase mergequeue.Phase) int64 {
	w.t.Helper()
	ctx := context.Background()
	anchor := w.mqeIntent(r, head)
	res, err := w.store.Enqueue(ctx, mergequeue.EnqueueRequest{
		RunID: r.id, BaseRef: mcqBase, HeadSHA: head, MergeIntent: true, IntentSequence: anchor.Sequence})
	if err != nil {
		w.t.Fatalf("enqueue: %v", err)
	}
	if adm, err := w.store.AdmitNext(ctx, res.Entry.Base()); err != nil || !adm.Admitted {
		w.t.Fatalf("admit: %+v %v", adm, err)
	}
	w.exec(`UPDATE merge_candidate_queue_entries SET phase = $1 WHERE id = $2`, string(phase), res.Entry.ID)
	return anchor.Sequence
}

// claim reads r's merge_dispatch_claims row ("" state when none).
func (w *mqmWorld) claim(r *mcqRun) (state, path string) {
	w.t.Helper()
	err := w.pool.QueryRow(context.Background(),
		`SELECT state, path FROM merge_dispatch_claims WHERE run_id = $1`, r.id).Scan(&state, &path)
	if err != nil && !strings.Contains(err.Error(), "no rows") {
		w.t.Fatalf("read claim: %v", err)
	}
	return state, path
}

func mqeCode(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	return env.Error.Code
}

// TestMergeQueueE2E_H1InlineRetryDuringVerifyingDispatchesOnce is approval
// condition D1 race (i), the H1 interleaving exactly: an inline merge records
// its 0062 verdict and its dispatch FAILS (the claim moves to
// dispatch_failed); the run is then enqueued with merge intent; an inline
// retry lands while the entry is mid-pass (phase verifying, the head already
// passed, so the merge-candidate gate admits); then merge-on-pass runs.
// Exactly ONE forge dispatch: the retry is refused 409 merge_queue_owns_merge
// (D2) and merge-on-pass meets the run's existing claim and never calls the
// forge (D1). Counterfactual (dropping the UNIQUE constraint): merge-on-pass
// claims again and merges pinned, two dispatches, RED.
func TestMergeQueueE2E_H1InlineRetryDuringVerifyingDispatchesOnce(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.verified("h1", "h1")
	m.inlineErr = errors.New("forge 500")

	first := w.mergeRun(r)
	if first.Code != http.StatusBadGateway || mqeCode(t, first.Body.Bytes()) != "merge_dispatch_failed" ||
		!strings.Contains(first.Body.String(), "dispatch_failed") {
		t.Fatalf("first merge = %d %s, want 502 merge_dispatch_failed naming the dispatch_failed claim", first.Code, first.Body.String())
	}
	if st, path := w.claim(r); st != string(mergequeue.DispatchFailed) || path != mergequeue.DispatchPathInline {
		t.Fatalf("claim = %s/%s, want dispatch_failed/inline", st, path)
	}
	if n := len(w.rows(r, CategoryMergeVerdictRecorded)); n != 1 {
		t.Fatalf("merge_verdict_recorded rows = %d, want the 0062 verdict", n)
	}

	m.inlineErr = nil
	seq := w.intentEntry(r, "h1", mergequeue.PhaseVerifying)
	retry := w.mergeRun(r)
	if retry.Code != http.StatusConflict || mqeCode(t, retry.Body.Bytes()) != "merge_queue_owns_merge" {
		t.Fatalf("retry during verifying = %d %s, want 409 merge_queue_owns_merge", retry.Code, retry.Body.String())
	}

	w.exec(`UPDATE merge_candidate_queue_entries SET phase = 'passed' WHERE run_id = $1`, r.id)
	out := w.srv.mergeQueueMergeOnPass(context.Background(),
		mergeOnPassRequest{Entry: w.active(r), IntentSequence: seq, Ledger: w.srv.mergeDispatchLedger()})
	requireOutcome(t, out, mergeOnPassHeld, mergeOnPassReasonAlreadyDispatched)

	if total := m.inlineCalls() + len(w.merger.calls()); total != 1 {
		t.Fatalf("forge dispatches = %d (inline %d, pinned %v), want exactly 1", total, m.inlineCalls(), w.merger.calls())
	}
	if n := len(w.rows(r, CategoryMergeVerdictRecorded)); n != 1 {
		t.Fatalf("merge_verdict_recorded rows = %d, want still 1", n)
	}
}

// TestMergeQueueE2E_InlineVsMergeOnPassMergeOnce pins the three orders of an
// inline merge_run against merge-on-pass. (a) The entry already passed: the
// inline call gets the named 409 and the pump's merge-on-pass merges pinned.
// (b) The entry is held (not live) with intent: ownership holds regardless of
// phase or state (D2), so the inline call still refuses and records nothing.
// (c) Merge-on-pass merges first: the entry settles merged inside the lock
// (D3), so a later inline call answers 200 already_merged, never 409, with no
// dispatch. Every order ends with exactly one forge dispatch at most.
func TestMergeQueueE2E_InlineVsMergeOnPassMergeOnce(t *testing.T) {
	t.Run("a entry passed: named refusal, pump merges", func(t *testing.T) {
		w, m := newMQEWorld(t)
		r := w.verified("a", "h1")
		w.intentEntry(r, "h1", mergequeue.PhasePassed)
		resp := w.mergeRun(r)
		if resp.Code != http.StatusConflict || mqeCode(t, resp.Body.Bytes()) != "merge_queue_owns_merge" {
			t.Fatalf("inline = %d %s, want 409 merge_queue_owns_merge", resp.Code, resp.Body.String())
		}
		if n := len(w.rows(r, CategoryMergeVerdictRecorded)); n != 0 {
			t.Fatalf("merge_verdict_recorded rows after the refusal = %d, want 0 (nothing recorded)", n)
		}
		w.pumpBase(r)
		if got := w.merger.calls(); len(got) != 1 || got[0] != "h1" {
			t.Fatalf("pinned merges = %v, want [h1]", got)
		}
		if m.inlineCalls() != 0 {
			t.Fatalf("inline dispatches = %d, want 0", m.inlineCalls())
		}
		w.requireState(r, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
		if st, path := w.claim(r); st != string(mergequeue.DispatchDispatched) || path != mergequeue.DispatchPathQueue {
			t.Fatalf("claim = %s/%s, want dispatched/queue", st, path)
		}
	})
	t.Run("b held with intent still owns", func(t *testing.T) {
		w, m := newMQEWorld(t)
		r := w.verified("b", "h1")
		w.intentEntry(r, "h1", mergequeue.PhaseVerifying)
		e := w.active(r)
		if _, err := w.store.Unadmit(context.Background(), e.ID, e.AdmissionEpoch); err != nil {
			t.Fatalf("unadmit: %v", err)
		}
		if got := w.active(r); got.State != mergequeue.StateHeld || got.MergeIntentAt == nil {
			t.Fatalf("entry = %s intent %v, want held with intent", got.State, got.MergeIntentAt)
		}
		owned, err := w.srv.mergeQueueOwnsMerge(context.Background(), r.id)
		if err != nil || !owned {
			t.Fatalf("mergeQueueOwnsMerge = %v, %v; want owned (D2: held with intent)", owned, err)
		}
		resp := w.mergeRun(r)
		if resp.Code == http.StatusOK || m.inlineCalls() != 0 || len(w.merger.calls()) != 0 {
			t.Fatalf("inline on a held intent entry = %d %s, inline %d pinned %v; want no dispatch",
				resp.Code, resp.Body.String(), m.inlineCalls(), w.merger.calls())
		}
		if n := len(w.rows(r, CategoryMergeVerdictRecorded)); n != 0 {
			t.Fatalf("merge_verdict_recorded rows = %d, want 0", n)
		}
	})
	t.Run("c merge-on-pass first: already_merged never 409", func(t *testing.T) {
		w, m := newMQEWorld(t)
		r := w.verified("c", "h1")
		seq := w.intentEntry(r, "h1", mergequeue.PhasePassed)
		out := w.srv.mergeQueueMergeOnPass(context.Background(),
			mergeOnPassRequest{Entry: w.active(r), IntentSequence: seq, Ledger: w.srv.mergeDispatchLedger()})
		requireOutcome(t, out, mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
		w.requireState(r, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
		resp := w.mergeRun(r)
		if resp.Code != http.StatusOK {
			t.Fatalf("inline after the queue merged = %d %s, want 200 already_merged", resp.Code, resp.Body.String())
		}
		var body mergeRunResponse
		_ = json.Unmarshal(resp.Body.Bytes(), &body)
		if !body.AlreadyMerged || body.MergeQueued {
			t.Fatalf("response = %+v, want already_merged with no merge queued", body)
		}
		if m.inlineCalls() != 0 || len(w.merger.calls()) != 1 {
			t.Fatalf("dispatches: inline %d pinned %v, want 0 and exactly 1", m.inlineCalls(), w.merger.calls())
		}
	})
}

// TestMergeQueueE2E_InlineDispatchClaimsAndRefusesSecond: an inline merge
// with no queue entry claims the run's ONE dispatch (path inline) before the
// forge; a re-invoke whose PR has not merged meets the claim, answers 409
// merge_already_dispatched and never calls the forge again (D1).
func TestMergeQueueE2E_InlineDispatchClaimsAndRefusesSecond(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.verified("i", "h1")
	if resp := w.mergeRun(r); resp.Code != http.StatusOK {
		t.Fatalf("first merge = %d %s, want 200", resp.Code, resp.Body.String())
	}
	if st, path := w.claim(r); st != string(mergequeue.DispatchDispatched) || path != mergequeue.DispatchPathInline {
		t.Fatalf("claim = %s/%s, want dispatched/inline", st, path)
	}
	resp := w.mergeRun(r)
	if resp.Code != http.StatusConflict || mqeCode(t, resp.Body.Bytes()) != "merge_already_dispatched" {
		t.Fatalf("re-invoke = %d %s, want 409 merge_already_dispatched", resp.Code, resp.Body.String())
	}
	if m.inlineCalls() != 1 {
		t.Fatalf("inline dispatches = %d, want exactly 1", m.inlineCalls())
	}
}

// TestMergeQueueE2E_ExistingClaimButMergedAnswersAlreadyMerged: an existing
// claim whose PR has landed answers already_merged, not 409 (D1).
func TestMergeQueueE2E_ExistingClaimButMergedAnswersAlreadyMerged(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.verified("l", "h1")
	if _, err := w.store.ClaimMergeDispatch(context.Background(), r.id, mergequeue.DispatchPathDelegated, "h1"); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	// The PR lands after the gate's read: the claim path's live re-observe
	// reads it merged.
	w.audit.onAppend = func(category string) {
		if category == CategoryMergeVerdictRecorded {
			w.gh.set(func() { w.gh.prs[r.pr].merged, w.gh.prs[r.pr].state = true, "closed" })
		}
	}
	resp := w.mergeRun(r)
	var body mergeRunResponse
	_ = json.Unmarshal(resp.Body.Bytes(), &body)
	if resp.Code != http.StatusOK || !body.AlreadyMerged {
		t.Fatalf("merge = %d %s, want 200 already_merged", resp.Code, resp.Body.String())
	}
	if m.inlineCalls() != 0 {
		t.Fatalf("inline dispatches = %d, want 0", m.inlineCalls())
	}
}

// Ruling R1 test 1 (concern c76a8132): a merge_checks_pending refusal applied
// nothing, so it RELEASES the inline claim and the re-invoke, once the checks
// pass, claims again and dispatches. Against the real postgres ledger.
func TestMergeQueueE2E_InlineChecksPendingThenRetryMerges(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.verified("cp", "h1")
	m.mu.Lock()
	m.inlineErr = fmt.Errorf("enable auto-merge: %w", forge.ErrPullRequestUnstableStatus)
	m.mu.Unlock()
	resp := w.mergeRun(r)
	if resp.Code != http.StatusConflict || mqeCode(t, resp.Body.Bytes()) != "merge_checks_pending" {
		t.Fatalf("first merge = %d %s, want 409 merge_checks_pending", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "dispatch_failed") {
		t.Fatalf("a checks_pending refusal must not name a dispatch_failed claim: %s", resp.Body.String())
	}
	if st, _ := w.claim(r); st != "" {
		t.Fatalf("claim after checks_pending = %q, want released", st)
	}
	m.mu.Lock()
	m.inlineErr = nil
	m.mu.Unlock()
	if resp := w.mergeRun(r); resp.Code != http.StatusOK {
		t.Fatalf("retry merge = %d %s, want 200", resp.Code, resp.Body.String())
	}
	if m.inlineCalls() != 2 {
		t.Fatalf("inline dispatches = %d, want 2 (refused, then dispatched)", m.inlineCalls())
	}
	if st, path := w.claim(r); st != string(mergequeue.DispatchDispatched) || path != mergequeue.DispatchPathInline {
		t.Fatalf("claim after retry = %s/%s, want dispatched/inline", st, path)
	}
}

// Ruling R1 test 3 (concern db4a28ac): a request cancelled mid-dispatch still
// records dispatch_failed (the write runs on a non-cancellable context), so the
// claim is never stranded as 'dispatched'; a re-invoke is refused (no
// automatic redispatch) and the operator re-arm DELETE then admits one.
func TestMergeQueueE2E_InlineCancelledRequestLeavesDispatchFailed(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.verified("cx", "h1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.mu.Lock()
	m.inlineHook = func(c context.Context) error {
		cancel()
		<-c.Done()
		return c.Err()
	}
	m.mu.Unlock()
	raw, _ := json.Marshal(mergeRunRequest{Verdict: "ship it"})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+r.id.String()+"/merge", bytes.NewReader(raw)).WithContext(ctx)
	req.SetPathValue("run_id", r.id.String())
	w.srv.handleMergeRun(httptest.NewRecorder(), withMergeOperator(req))
	if st, path := w.claim(r); st != string(mergequeue.DispatchFailed) || path != mergequeue.DispatchPathInline {
		t.Fatalf("claim after a cancelled dispatch = %q/%q, want dispatch_failed/inline", st, path)
	}
	m.mu.Lock()
	m.inlineHook = nil
	m.mu.Unlock()
	resp := w.mergeRun(r)
	if resp.Code != http.StatusConflict || mqeCode(t, resp.Body.Bytes()) != "merge_already_dispatched" {
		t.Fatalf("re-invoke = %d %s, want 409 merge_already_dispatched", resp.Code, resp.Body.String())
	}
	if m.inlineCalls() != 1 {
		t.Fatalf("inline dispatches = %d, want 1 (no automatic redispatch)", m.inlineCalls())
	}
	// The documented operator re-arm (mergequeue README).
	w.exec(`DELETE FROM merge_dispatch_claims WHERE run_id = $1 AND state = 'dispatch_failed'`, r.id)
	if resp := w.mergeRun(r); resp.Code != http.StatusOK {
		t.Fatalf("merge after re-arm = %d %s, want 200", resp.Code, resp.Body.String())
	}
}

// mqeForgePUTMerger is the head-pinned merge over the production githubclient
// PUT against the mcq fake, which moves the base (every other open PR reads
// behind) on a merge.
type mqeForgePUTMerger struct{ gh *githubclient.Client }

func (mqeForgePUTMerger) MergePullRequest(context.Context, *run.Run) error {
	return errors.New("the line e2e drives only the head-pinned merge")
}

func (m mqeForgePUTMerger) MergePullRequestAtHead(ctx context.Context, runRow *run.Run, head string) error {
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return err
	}
	return m.gh.MergePullRequestAtHead(ctx, forge.FromGitHubInstallationID(*runRow.InstallationID), repo,
		parsePRNumberFromURL(runRow.PullRequestURL), forge.MergeMethodSquash, head)
}

// newMQELineWorld is an mqmWorld whose merge-on-pass merges through the real
// PUT, so a merge moves the base under the rest of the line.
func newMQELineWorld(t *testing.T) *mqmWorld {
	t.Helper()
	w := newMQMWorld(t)
	w.srv.cfg.GateMerger = mqeForgePUTMerger{gh: w.srv.cfg.GitHub}
	return w
}

// passAt records the runner's passed verdict at head for r's live verifying
// entry and waits for the detached pump it triggers (no human act).
func (w *mqmWorld) passAt(r *mcqRun, head string) {
	w.t.Helper()
	w.mqePassed(r, head)
	w.srv.mergeCandidateQueueOnVerdict(context.Background(), r.id, head, mergeCandidateResultPassed)
	w.srv.bgMergeQueuePump.Wait()
}

func (w *mqmWorld) requirePuts(want ...string) {
	w.t.Helper()
	var got []string
	for _, p := range w.gh.putCalls() {
		got = append(got, p.SHA)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		w.t.Fatalf("pinned PUTs = %v, want %v", got, want)
	}
}

// TestMergeQueueE2E_TwoPRsMergeBackToBack: two intent entries on one base.
// The first merges pinned to its verified head, which moves the base; the
// second advances onto that base, passes there and merges pinned to the new
// head, with no human act in between (the pass is the runner's verdict).
func TestMergeQueueE2E_TwoPRsMergeBackToBack(t *testing.T) {
	w := newMQELineWorld(t)
	a := w.seed("a", "a1", []byte(mcvUndelegatedSpecYAML))
	b := w.seed("b", "b1", []byte(mcvUndelegatedSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	w.mqpHeldIntent(b, "b1", false)
	w.gh.set(func() { w.gh.mergeSHA[b.branch] = "b2" })

	w.pumpBase(a)

	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	if e := w.storeEntry(b); e.State != mergequeue.StateLive || e.AnchoredHeadSHA != "b2" {
		t.Fatalf("B after A merged = %s at %s, want live at the advanced head b2", e.State, e.AnchoredHeadSHA)
	}
	w.requirePuts("a1")

	w.passAt(b, "b2")

	w.requirePuts("a1", "b2")
	w.requireState(b, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	if n := len(w.rows(b, CategoryMergeQueueAdvanced)); n != 1 {
		t.Fatalf("B merge_queue_advanced rows = %d, want 1 (onto the base A's merge moved)", n)
	}
}

// TestMergeQueueE2E_BaseMovesBetweenPassAndMergeReadvances: the base moves
// after the entry's pass and before its merge, so merge-on-pass re-advances
// it at its place; the re-admission advances onto the new base, the runner's
// pass there merges it pinned to the new head, and the stale head is never
// PUT.
func TestMergeQueueE2E_BaseMovesBetweenPassAndMergeReadvances(t *testing.T) {
	w := newMQELineWorld(t)
	a := w.seed("a", "a1", []byte(mcvUndelegatedSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	w.gh.set(func() { w.gh.mergeSHA[a.branch] = "a2" })
	moved := false
	w.audit.onAppend = func(category string) {
		// The admission records the pass; the base moves right after it.
		if category == CategoryMergeQueueVerified && !moved {
			moved = true
			w.gh.set(func() { w.gh.behind["a1"] = []string{"base-x"} })
		}
	}

	w.pumpBase(a)

	if !moved {
		t.Fatal("the admission recorded no merge_queue_verified row")
	}
	w.requirePuts()
	if e := w.storeEntry(a); e.State != mergequeue.StateLive || e.AnchoredHeadSHA != "a2" {
		t.Fatalf("entry after the readvance = %s at %s, want live at a2 (re-advanced in place)", e.State, e.AnchoredHeadSHA)
	}

	w.passAt(a, "a2")

	w.requirePuts("a2")
	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
}

// TestMergeQueueE2E_ForgeApprovalDismissedEjectsAndLineContinues: the head
// of line's forge approval was dismissed, so merge-on-pass ejects it
// forge_approval_dismissed (no claim, no PUT) and the line continues: the
// next entry merges in the same pump.
func TestMergeQueueE2E_ForgeApprovalDismissedEjectsAndLineContinues(t *testing.T) {
	w := newMQELineWorld(t)
	a := w.seed("a", "a1", []byte(mcvUndelegatedSpecYAML))
	b := w.seed("b", "b1", []byte(mcvUndelegatedSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	w.mqpHeldIntent(b, "b1", false)
	w.gh.set(func() { w.gh.reviews[a.pr] = mqeDismissed })
	w.mergeable(a, "blocked")

	w.pumpBase(a)

	w.requireState(a, mergequeue.StateEjected, mergeCandidateQueueReasonForgeApprovalDismissed)
	if n := w.claims(a); n != 0 {
		t.Fatalf("A claims = %d, want 0 (ejected before any dispatch)", n)
	}
	w.requirePuts("b1")
	w.requireState(b, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
}

// mqeEnqueueOnLock enqueues a HELD intent entry the first time the run's
// merge-intent lock is taken: the inline gate (before the lock) saw no entry,
// so merge_candidate_held cannot refuse first and only the in-lock D2
// ownership check refuses.
type mqeEnqueueOnLock struct {
	mergequeue.Store
	once  *sync.Once
	onRun func()
}

func (q mqeEnqueueOnLock) WithMergeIntentLock(ctx context.Context, runID uuid.UUID, fn func(context.Context) error) error {
	q.once.Do(q.onRun)
	return q.Store.WithMergeIntentLock(ctx, runID, fn)
}

// TestMergeQueueE2E_InlineRefusedByHeldIntentEntry pins D2's held arm on the
// inline surface: 409 merge_queue_owns_merge, the forge never called and no
// claim taken.
func TestMergeQueueE2E_InlineRefusedByHeldIntentEntry(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.verified("heldown", "h1")
	w.srv.cfg.MergeQueue = mqeEnqueueOnLock{Store: w.store, once: &sync.Once{}, onRun: func() {
		if _, err := w.store.Enqueue(context.Background(), mergequeue.EnqueueRequest{
			RunID: r.id, BaseRef: mcqBase, HeadSHA: "h1", MergeIntent: true}); err != nil {
			t.Errorf("enqueue held intent entry: %v", err)
		}
	}}
	resp := w.mergeRun(r)
	if resp.Code != http.StatusConflict || mqeCode(t, resp.Body.Bytes()) != "merge_queue_owns_merge" {
		t.Fatalf("merge = %d %s, want 409 merge_queue_owns_merge", resp.Code, resp.Body.String())
	}
	if e := w.storeEntry(r); e.State != mergequeue.StateHeld {
		t.Fatalf("entry = %s, want held (the D2 held arm)", e.State)
	}
	if m.inlineCalls() != 0 {
		t.Fatalf("inline dispatches = %d, want 0", m.inlineCalls())
	}
	if st, _ := w.claim(r); st != "" {
		t.Fatalf("claim = %q, want none", st)
	}
}

// TestMergeQueueE2E_InlineHoldsLockMergeOnPassWaits is the barrier-forced
// inline-vs-merge-on-pass race under the REAL merge-intent lock: the inline
// merge holds the lock parked inside its dispatch while an intent entry
// passes and merge-on-pass decides; merge-on-pass waits on the lock, then
// meets the inline dispatch and holds. Exactly one forge dispatch.
func TestMergeQueueE2E_InlineHoldsLockMergeOnPassWaits(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.verified("race", "h1")
	entered, release := make(chan struct{}), make(chan struct{})
	m.mu.Lock()
	m.inlineHook = func(context.Context) error {
		close(entered)
		<-release
		return nil
	}
	m.mu.Unlock()
	inlineDone := make(chan int, 1)
	go func() { inlineDone <- w.mergeRun(r).Code }()
	<-entered // the inline path holds the run's lock, inside its dispatch

	seq := w.intentEntry(r, "h1", mergequeue.PhasePassed)
	decided := make(chan mergeOnPassOutcome, 1)
	go func() {
		decided <- w.decide(w.realLedger(mergeOnPassRequest{Entry: w.active(r), IntentSequence: seq}))
	}()
	select {
	case out := <-decided:
		close(release) // unpark the inline merge so a failure ends the test, never hangs it
		<-inlineDone
		t.Fatalf("merge-on-pass decided %+v while the inline merge held the lock", out)
	case <-time.After(timescale.D(300 * time.Millisecond)):
	}
	close(release)
	if code := <-inlineDone; code != http.StatusOK {
		t.Fatalf("inline merge = %d, want 200", code)
	}
	out := <-decided
	if out.Kind != mergeOnPassHeld {
		t.Fatalf("merge-on-pass = %+v, want held behind the inline dispatch", out)
	}
	if m.inlineCalls() != 1 || len(w.merger.calls()) != 0 {
		t.Fatalf("dispatches: inline %d pinned %v, want exactly the one inline", m.inlineCalls(), w.merger.calls())
	}
}

// TestMergeQueueE2E_InlineGenericGraphQLErrorLeavesDispatchFailed (ruling R1
// tightening): the production githubclient auto-merge mutation answering an
// HTTP-200 GraphQL INTERNAL error envelope is an UNCERTAIN outcome, so the
// inline claim is dispatch_failed (never released) on the real ledger.
func TestMergeQueueE2E_InlineGenericGraphQLErrorLeavesDispatchFailed(t *testing.T) {
	w, _ := newMQEWorld(t)
	r := w.verified("gql", "h1")
	orig := w.srv.cfg.GitHub
	target, err := url.Parse(orig.BaseURL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	gql := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/graphql" {
			rw.Header().Set("Content-Type", "application/json")
			_, _ = rw.Write([]byte(`{"data":null,"errors":[{"type":"INTERNAL","message":"Something went wrong while executing your query."}]}`))
			return
		}
		rp.ServeHTTP(rw, req)
	}))
	t.Cleanup(gql.Close)
	c := *orig
	c.BaseURL = gql.URL
	w.srv.cfg.GateMerger = mqeAutoMerger{gh: &c}

	resp := w.mergeRun(r)
	if resp.Code != http.StatusBadGateway || mqeCode(t, resp.Body.Bytes()) != "merge_dispatch_failed" {
		t.Fatalf("merge = %d %s, want 502 merge_dispatch_failed", resp.Code, resp.Body.String())
	}
	if st, path := w.claim(r); st != string(mergequeue.DispatchFailed) || path != mergequeue.DispatchPathInline {
		t.Fatalf("claim = %q/%q, want dispatch_failed/inline (a generic GraphQL error is uncertain)", st, path)
	}
}

// mqeAutoMerger is the inline GateMerger over the production githubclient
// auto-merge mutation.
type mqeAutoMerger struct{ gh *githubclient.Client }

func (m mqeAutoMerger) MergePullRequest(ctx context.Context, runRow *run.Run) error {
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return err
	}
	return m.gh.EnableAutoMerge(ctx, forge.FromGitHubInstallationID(*runRow.InstallationID), repo,
		parsePRNumberFromURL(runRow.PullRequestURL), forge.MergeMethodSquash)
}

// TestMergeQueueE2E_InlineUncertainErrorButMergedKeepsClaimDispatched
// (#3320f2eb): an uncertain dispatch error whose merge in fact landed is
// re-observed BEFORE the ledger is settled, so it answers already_merged,
// the claim stays 'dispatched' and no merge_dispatch_settled row is written.
func TestMergeQueueE2E_InlineUncertainErrorButMergedKeepsClaimDispatched(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.verified("lm", "h1")
	m.mu.Lock()
	m.inlineHook = func(context.Context) error {
		w.gh.set(func() { w.gh.prs[r.pr].merged, w.gh.prs[r.pr].state = true, "closed" })
		return errors.New("github: 502 bad gateway")
	}
	m.mu.Unlock()
	resp := w.mergeRun(r)
	var body mergeRunResponse
	_ = json.Unmarshal(resp.Body.Bytes(), &body)
	if resp.Code != http.StatusOK || !body.AlreadyMerged {
		t.Fatalf("merge = %d %s, want 200 already_merged", resp.Code, resp.Body.String())
	}
	if st, _ := w.claim(r); st != string(mergequeue.DispatchDispatched) {
		t.Fatalf("claim = %q, want dispatched (the merge landed)", st)
	}
	if n := len(w.rows(r, categoryMergeDispatchSettled)); n != 0 {
		t.Fatalf("merge_dispatch_settled rows = %d, want 0", n)
	}
}
