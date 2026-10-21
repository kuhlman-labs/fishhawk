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

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
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
	mu        sync.Mutex
	inlineErr error
	inlineN   int
}

func (m *mqeMerger) MergePullRequest(context.Context, *run.Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inlineN++
	return m.inlineErr
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
