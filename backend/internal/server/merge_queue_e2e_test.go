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
// (auto-merge, pinned to the gate head since #4641) dispatch is countable and
// can fail.

// mqeMerger wraps the recording pinned merger with a countable, failable
// inline MergePullRequest (it models auto-merge: it never merges the PR).
type mqeMerger struct {
	*mqmMerger
	mu          sync.Mutex
	inlineErr   error
	inlineN     int
	inlineHeads []string
	inlineHook  func(ctx context.Context) error
}

func (m *mqeMerger) MergePullRequest(ctx context.Context, _ *run.Run, head string) error {
	m.mu.Lock()
	m.inlineN++
	m.inlineHeads = append(m.inlineHeads, head)
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

// inlinePins returns the head each inline dispatch was pinned to (#4641).
func (m *mqeMerger) inlinePins() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.inlineHeads...)
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

// mqeErrorDetails decodes an error body's code, message and details.
func mqeErrorDetails(t *testing.T, body []byte) (code, msg string, details map[string]any) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	return env.Error.Code, env.Error.Message, env.Error.Details
}

// TestMergeQueueE2E_InlineDispatchPinsGateHead pins (#4641) that the inline
// dispatch is PINNED to gate.HeadSHA, the head the merge-candidate gate
// admitted and the claim records. Counterfactual: passing "" at the
// merge_run.go call site records an empty pin, RED.
func TestMergeQueueE2E_InlineDispatchPinsGateHead(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.verified("pin", "h1")
	if resp := w.mergeRun(r); resp.Code != http.StatusOK {
		t.Fatalf("merge = %d %s, want 200", resp.Code, resp.Body.String())
	}
	if got := m.inlinePins(); len(got) != 1 || got[0] != "h1" {
		t.Fatalf("inline pins = %q, want [h1]", got)
	}
}

// TestMergeQueueE2E_InlineHeadMovedRefusalReleasesClaim pins (#4641) the
// inline head-moved mapping on the real ledger: a pinned dispatch the forge
// refuses for a head mismatch answers 409 merge_head_moved naming the pinned
// head and its refusal_basis. On a basis the forge guarantees applied nothing
// (precheck, rest_sha) and on a bare REST 409 naming no basis it RELEASES the
// claim (no claim row, a merge_dispatch_settled row outcome released at h1)
// and the next re-invoke claims again. On graphql_reread (#4749) the claim is
// LEFT dispatch_failed, the settled row records the head-movement detail, the
// message names the held claim with the uncertain-basis clause, and the
// re-invoke answers merge_already_dispatched without a second dispatch. A
// precheck refusal whose release does not land (approval condition C1) is
// reported from the SETTLE outcome: dispatch_failed, without the
// uncertain-basis clause. The run is gate-less, so the message names only the
// admitted head, never an evaluated approval (ruling C2). Only the precheck /
// rest_sha bases claim "nothing merged"; graphql_reread is hedged (ruling C3).
// Counterfactuals: deleting the merge_head_moved branch answers 502
// merge_dispatch_failed; neutralising mergeDispatchHeadMismatchUncertain
// releases the graphql_reread claim (re-invoke 200), RED.
func TestMergeQueueE2E_InlineHeadMovedRefusalReleasesClaim(t *testing.T) {
	const uncertainClause = "does not prove the forge applied nothing"
	for _, tc := range []struct {
		name         string
		basis        string
		guaranteed   bool
		released     bool // hard-coded expectation, never derived from the production predicate
		releaseFails bool
	}{
		{"basis precheck", githubclient.HeadMismatchPrecheck, true, true, false},
		{"basis rest_sha", githubclient.HeadMismatchRESTSha, true, true, false},
		{"basis graphql_reread", githubclient.HeadMismatchGraphQLReread, false, false, false},
		{"basis ", "", false, true, false}, // a leaf that names no basis (the bare REST 409)
		{"basis precheck release fails", githubclient.HeadMismatchPrecheck, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, m := newMQEWorld(t)
			if tc.releaseFails {
				w.srv.cfg.MergeQueue = mqeReleaseFailQueue{Store: w.store}
			}
			r := w.verified("hm", "h1")
			cause := fmt.Errorf("pinned enable: %w", forge.ErrMergeConflict)
			if tc.basis != "" {
				cause = fmt.Errorf("%w: %w", forge.ErrMergeConflict,
					&githubclient.HeadMismatchError{Basis: tc.basis, ExpectedHeadSHA: "h1", ObservedHeadSHA: "h2"})
			}
			m.mu.Lock()
			m.inlineErr = cause
			m.mu.Unlock()
			resp := w.mergeRun(r)
			code, msg, d := mqeErrorDetails(t, resp.Body.Bytes())
			if resp.Code != http.StatusConflict || code != mergeCodeHeadMoved {
				t.Fatalf("merge = %d %s, want 409 %s", resp.Code, resp.Body.String(), mergeCodeHeadMoved)
			}
			wantBasis := tc.basis
			if wantBasis == "" {
				wantBasis = mergeHeadMovedBasisUnknown
			}
			if d["head_sha"] != "h1" || d["refusal_basis"] != wantBasis || d["next_step"] != "fishhawk_merge_run" || d["run_id"] != r.id.String() {
				t.Fatalf("details = %v, want head_sha h1, refusal_basis %s, next_step fishhawk_merge_run", d, wantBasis)
			}
			if !strings.Contains(msg, "admitted (h1)") || strings.Contains(msg, "approval was evaluated") {
				t.Fatalf("message = %q, want the admitted head and no evaluated-approval claim (gate-less run)", msg)
			}
			if got := strings.Contains(msg, "Nothing merged"); got != tc.guaranteed {
				t.Fatalf("message claims nothing merged = %v, want %v: %q", got, tc.guaranteed, msg)
			}
			settled := w.rows(r, categoryMergeDispatchSettled)
			if len(settled) != 1 {
				t.Fatalf("merge_dispatch_settled rows = %d, want 1", len(settled))
			}
			sp := w.payload(settled[0])
			if tc.basis != "" && (sp["refusal_basis"] != tc.basis || sp["expected_head_sha"] != "h1" || sp["observed_head_sha"] != "h2") {
				t.Fatalf("settled payload = %v, want refusal_basis %s expected h1 observed h2", sp, tc.basis)
			}
			if tc.released {
				if !strings.Contains(msg, "claim was released") || strings.Contains(msg, "dispatch_failed") {
					t.Fatalf("message = %q, want the released claim named and no dispatch_failed", msg)
				}
				if st, _ := w.claim(r); st != "" {
					t.Fatalf("claim = %q, want released (no row)", st)
				}
				if sp["outcome"] != string(mergeDispatchRelease) || sp["head_sha"] != "h1" {
					t.Fatalf("settled payload = %v, want released at h1", sp)
				}
				m.mu.Lock()
				m.inlineErr = nil
				m.mu.Unlock()
				if resp := w.mergeRun(r); resp.Code != http.StatusOK {
					t.Fatalf("re-invoke = %d %s, want 200 (the released claim is claimable)", resp.Code, resp.Body.String())
				}
				return
			}
			if strings.Contains(msg, "claim was released") || !strings.Contains(msg, "dispatch_failed") {
				t.Fatalf("message = %q, want the held dispatch_failed claim and no released claim", msg)
			}
			if got, want := strings.Contains(msg, uncertainClause), !tc.releaseFails; got != want {
				t.Fatalf("message uncertain-basis clause = %v, want %v: %q", got, want, msg)
			}
			if tc.releaseFails && !strings.Contains(msg, "claim release did not land, so it is dispatch_failed") {
				t.Fatalf("message = %q, want the release that did not land named", msg)
			}
			if st, path := w.claim(r); st != string(mergequeue.DispatchFailed) || path != mergequeue.DispatchPathInline {
				t.Fatalf("claim = %q/%q, want dispatch_failed/inline", st, path)
			}
			if sp["outcome"] != string(mergeDispatchUncertain) || sp["head_sha"] != "h1" {
				t.Fatalf("settled payload = %v, want dispatch_failed at h1", sp)
			}
			m.mu.Lock()
			m.inlineErr = nil
			m.mu.Unlock()
			again := w.mergeRun(r)
			if again.Code != http.StatusConflict || mqeCode(t, again.Body.Bytes()) != "merge_already_dispatched" {
				t.Fatalf("re-invoke = %d %s, want 409 merge_already_dispatched (the claim is held)", again.Code, again.Body.String())
			}
			if n := m.inlineCalls(); n != 1 {
				t.Fatalf("inline dispatches = %d, want 1 (the held claim admits no second dispatch)", n)
			}
		})
	}
}

// mqeReleaseFailQueue fails ReleaseMergeDispatch, so a releasable refusal's
// settle leaves the claim dispatch_failed (approval condition C1).
type mqeReleaseFailQueue struct{ mergequeue.Store }

func (mqeReleaseFailQueue) ReleaseMergeDispatch(context.Context, mergequeue.DispatchClaim) error {
	return errors.New("injected release failure")
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

func (mqeForgePUTMerger) MergePullRequest(context.Context, *run.Run, string) error {
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
// PUT. #4748 (CF2's vehicle): the entry never waited, so after the readvance
// approval_wait_started_at is still NULL. Without the enforcement's
// no-episode pre-filter, its in-lock ApprovalWait (COALESCE) STAMPS a fresh
// anchor on an entry that never waited, RED.
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
	if e := w.active(a); e.ApprovalWaitStartedAt != nil {
		t.Fatalf("approval_wait_started_at after the readvance = %v, want NULL (the entry never waited)", e.ApprovalWaitStartedAt)
	}

	w.passAt(a, "a2")

	w.requirePuts("a2")
	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
}

// requireYielded asserts r's active entry is HELD and yielded with reason, its
// merge_queue_yielded row naming next and site, and returns the entry.
func (w *mqmWorld) requireYielded(r *mcqRun, reason, next, site string) mergequeue.Entry {
	w.t.Helper()
	e := w.active(r)
	if e.State != mergequeue.StateHeld || e.YieldedAt == nil || e.YieldReason != reason {
		w.t.Fatalf("run %s entry = %s yielded_at=%v reason=%q, want held and yielded %s", r.id, e.State, e.YieldedAt, e.YieldReason, reason)
	}
	rows := w.rows(r, CategoryMergeQueueYielded)
	if len(rows) != 1 {
		w.t.Fatalf("merge_queue_yielded rows = %d, want 1", len(rows))
	}
	if p := w.payload(rows[0]); p["yield_reason"] != reason || p["next_step"] != next || p["site"] != site || p["entry_id"] != e.ID.String() {
		w.t.Fatalf("merge_queue_yielded payload = %v, want reason %s next_step %s site %s", p, reason, next, site)
	}
	return e
}

// backdateApprovalWait moves r's active entry's approval_wait_started_at two
// hours into the past (the database clock's own value, so no cross-clock
// seed): past the default 30-minute window.
func (w *mqmWorld) backdateApprovalWait(r *mcqRun) {
	w.t.Helper()
	w.exec(`UPDATE merge_candidate_queue_entries SET approval_wait_started_at = approval_wait_started_at - interval '2 hours'
WHERE run_id = $1 AND state IN ('held', 'live') AND approval_wait_started_at IS NOT NULL`, r.id)
	if e := w.active(r); e.ApprovalWaitStartedAt == nil {
		w.t.Fatalf("run %s has no approval wait anchor to backdate", r.id)
	}
}

// TestMergeQueueE2E_ForgeApprovalDismissedEjectsAndLineContinues (converted
// to ADR-112 R3, #4632): under a review approval gate the head of line's
// in-lock fold reads its only review DISMISSED, so merge-on-pass WAITS at the
// head: A stays live with its approval-wait anchor, nothing else is admitted
// and nothing is PUT. Once the anchor is past the window, the next pump
// YIELDS A (held, marked forge_approval_dismissed, next_step approve_pr; no
// claim, no verdict, no authorization) and CONTINUES in the SAME pump: B,
// approved by a write-level member, is admitted and merges. Counterfactual
// (CF4): the pump stopping after a yield leaves B held behind A with no b1
// PUT, RED — only the same pump's continuation can admit B here.
func TestMergeQueueE2E_ForgeApprovalDismissedEjectsAndLineContinues(t *testing.T) {
	w := newMQELineWorld(t)
	a := w.seed("a", "a1", []byte(mqeSpecYAML))
	b := w.seed("b", "b1", []byte(mqeSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	w.mqpHeldIntent(b, "b1", false)
	w.forgeReviews(a, []string{"alice"}, mqmReview(1, "alice", "DISMISSED", "a1"))
	w.forgeReviews(b, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "b1"))

	w.pumpBase(a)

	if e := w.active(a); e.State != mergequeue.StateLive || e.YieldedAt != nil || e.ApprovalWaitStartedAt == nil {
		t.Fatalf("A after the first pump = %s yielded_at=%v wait=%v, want live, waiting at the head", e.State, e.YieldedAt, e.ApprovalWaitStartedAt)
	}
	if e := w.storeEntry(b); e.State != mergequeue.StateHeld {
		t.Fatalf("B while A waits = %s, want held (no re-admission during the wait)", e.State)
	}
	w.requirePuts()

	w.backdateApprovalWait(a)
	w.pumpBase(a)

	w.requireYielded(a, mergeCandidateQueueReasonForgeApprovalDismissed, forgeApprovalNextApprovePR, mergeQueueYieldSiteMergeOnPass)
	if n := w.claims(a); n != 0 {
		t.Fatalf("A claims = %d, want 0 (yielded before any dispatch)", n)
	}
	if n := len(w.rows(a, CategoryMergeVerdictRecorded)) + len(w.rows(a, CategoryMergeQueueAuthorized)); n != 0 {
		t.Fatalf("A verdict/authorization rows = %d, want 0", n)
	}
	w.requirePuts("b1")
	w.requireState(b, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
}

// TestMergeQueueE2E_GatelessBlockedRefusalEjectsReviewRequired (converted to
// ADR-112 R3 row 4, #4632) is the gate-less twin: no fold read, so A reaches
// the pinned merge; the forge refuses it (405, a typed RELEASE outcome), the
// claim is released, the post-refusal snapshot reads BLOCKED, and A YIELDS
// forge_review_required with next_step approve_pr (never ejects) while B
// still merges in the same pump.
func TestMergeQueueE2E_GatelessBlockedRefusalEjectsReviewRequired(t *testing.T) {
	w := newMQELineWorld(t)
	a := w.seed("a", "a1", []byte(mcvUndelegatedSpecYAML))
	b := w.seed("b", "b1", []byte(mcvUndelegatedSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	w.mqpHeldIntent(b, "b1", false)
	w.gh.set(func() { w.gh.putStatus[a.pr] = http.StatusMethodNotAllowed })
	w.gh.setReviewSnapshot(a.pr, mcqReviewSnapshot{MergeStateStatus: "BLOCKED"})

	w.pumpBase(a)

	w.requireYielded(a, mergeQueueYieldForgeReviewRequired, forgeApprovalNextApprovePR, mergeQueueYieldSiteMergeOnPass)
	if n := w.claims(a); n != 0 {
		t.Fatalf("A claims = %d, want 0 (the typed refusal released it)", n)
	}
	w.requirePuts("a1", "b1")
	w.requireState(b, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
}

// TestMergeQueueE2E_GatelessRowFourYieldIsNotReprobedIntoALoop is the
// row-4 yield's re-probe twin of the test above: A alone on its base, so a
// cleared row re-admits at an unmoved base and re-attempts the pinned PUT.
// Two due sweeps and one targeted (webhook-shaped, unforced) re-probe run on
// an UNCHANGED forge state (405, BLOCKED). The gate-less fold is always
// approved, so only the fresh-snapshot clear predicate keeps the row: exactly
// ONE PUT in total, A still yielded, yielded_at unchanged. Counterfactual:
// clearing on the approved fold alone re-admits A on the first sweep and
// re-PUTs a1 on every probe, RED.
func TestMergeQueueE2E_GatelessRowFourYieldIsNotReprobedIntoALoop(t *testing.T) {
	w, a, y := newRowFourYieldWorld(t)
	ctx := context.Background()

	for i := range 2 {
		if i > 0 {
			w.srv.yieldSweptAgo(time.Hour) // rewind every ADR-112 A2.1 service mark one (hour-long) tick
		}
		if n := w.srv.sweepMergeQueueYields(ctx, time.Hour); n != 1 {
			t.Fatalf("sweep %d re-probed %d rows, want 1 (the backdated row is due)", i+1, n)
		}
		w.srv.bgMergeQueuePump.Wait()
	}
	if w.srv.ReprobeMergeQueueYield(ctx, a.id, false) {
		t.Fatal("the targeted re-probe cleared the row on an unchanged forge state")
	}
	w.srv.bgMergeQueuePump.Wait()

	w.requirePuts("a1")
	w.requireRowFourKept(a, y)
}

// TestMergeQueueE2E_ApprovalWaitAnchorSurvivesReadvance (reworked for
// #4748): A waits at the head with no forge approval; the base moves BEFORE
// the window expires, so the next pump READVANCES A (Unadmit) and the
// re-admission advances it onto the moved base (a2). The read-back anchor
// EQUALS the first one, which pins Unadmit and the re-admission keeping it
// (an anchor reset by either starts a fresh wait, RED). Then the anchor is
// backdated past the window, and the runner's pass at a2 brings A back to
// merge-on-pass, which yields forge_approval_absent at the preserved anchor.
// (An anchor already expired at the readvance now yields at the readvance by
// design: TestMergeQueueE2E_ExpiredApprovalWaitYieldsOnRepeatedBaseMovement.)
func TestMergeQueueE2E_ApprovalWaitAnchorSurvivesReadvance(t *testing.T) {
	w := newMQELineWorld(t)
	a := w.seed("a", "a1", []byte(mqeSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	w.forgeReviews(a, nil)
	w.gh.set(func() { w.gh.mergeSHA[a.branch] = "a2" })

	w.pumpBase(a)
	first := w.active(a)
	if first.State != mergequeue.StateLive || first.ApprovalWaitStartedAt == nil {
		t.Fatalf("A after the first pump = %+v, want live and waiting", first)
	}

	w.gh.set(func() { w.gh.behind["a1"] = []string{"base-x"} })
	w.pumpBase(a)
	e := w.active(a)
	if e.State != mergequeue.StateLive || e.AnchoredHeadSHA != "a2" || e.YieldedAt != nil {
		t.Fatalf("A after the readvance = %s at %s yielded_at=%v, want live at a2, unmarked", e.State, e.AnchoredHeadSHA, e.YieldedAt)
	}
	if e.ApprovalWaitStartedAt == nil || !e.ApprovalWaitStartedAt.Equal(*first.ApprovalWaitStartedAt) {
		t.Fatalf("A's wait anchor after the readvance = %v, want the first one %v (kept by Unadmit and the re-admission)", e.ApprovalWaitStartedAt, first.ApprovalWaitStartedAt)
	}

	w.backdateApprovalWait(a)
	w.passAt(a, "a2")

	y := w.requireYielded(a, mergeCandidateQueueReasonForgeApprovalAbsent, forgeApprovalNextApprovePR, mergeQueueYieldSiteMergeOnPass)
	if y.ApprovalWaitStartedAt == nil || !y.ApprovalWaitStartedAt.Before(first.ApprovalWaitStartedAt.Add(-time.Hour)) {
		t.Fatalf("A's wait anchor = %v, want the backdated original (first %v minus 2h)", y.ApprovalWaitStartedAt, first.ApprovalWaitStartedAt)
	}
	w.requirePuts()
}

// requireNoDispatchEvidence asserts r claimed nothing and recorded no merge
// verdict and no authorization row.
func (w *mqmWorld) requireNoDispatchEvidence(r *mcqRun) {
	w.t.Helper()
	if n := w.claims(r); n != 0 {
		w.t.Fatalf("claims = %d, want 0", n)
	}
	if n := len(w.rows(r, CategoryMergeVerdictRecorded)) + len(w.rows(r, CategoryMergeQueueAuthorized)); n != 0 {
		w.t.Fatalf("verdict/authorization rows = %d, want 0", n)
	}
}

// requireWaiting asserts r's entry is live, unmarked and waiting (its
// approval-wait anchor set) at head.
func (w *mqmWorld) requireWaiting(r *mcqRun, head string) mergequeue.Entry {
	w.t.Helper()
	e := w.active(r)
	if e.State != mergequeue.StateLive || e.YieldedAt != nil || e.ApprovalWaitStartedAt == nil || e.AnchoredHeadSHA != head {
		w.t.Fatalf("run %s entry = %s at %s yielded_at=%v wait=%v, want live, unmarked, waiting at %s",
			r.id, e.State, e.AnchoredHeadSHA, e.YieldedAt, e.ApprovalWaitStartedAt, head)
	}
	return e
}

// requireHeldBehind asserts b is still held behind the head of line, with no
// PUT at all.
func (w *mqmWorld) requireHeldBehind(b *mcqRun) {
	w.t.Helper()
	if e := w.storeEntry(b); e.State != mergequeue.StateHeld {
		w.t.Fatalf("B = %s, want held behind the head of line", e.State)
	}
	w.requirePuts()
}

// TestMergeQueueE2E_ExpiredApprovalWaitYieldsOnRepeatedBaseMovement (#4748;
// CF1 and CF5 vehicle): A (review gate, its only review DISMISSED) and B
// (approved) are queued. A waits at the head; then the base moves under every
// pass, so every later decision returns at the base re-probe as a readvance
// and never reaches await_approval. Before expiry A re-advances (a2, a3) and B
// stays held. Once A's anchor is past the window, the next base-moved
// readvance YIELDS A with the FOLD's reason (the base-move fold at the new
// head; next_step approve_pr; site merge_on_pass), with no claim, verdict or
// authorization row, and the SAME pump admits B, which merges. MECHANISM:
// without the enforcement A readvances forever and B is never admitted
// (CF1); without the base-move fold the expiry yield is the generic
// forge_approval_undetermined and the reason assertion fails (CF5).
func TestMergeQueueE2E_ExpiredApprovalWaitYieldsOnRepeatedBaseMovement(t *testing.T) {
	w := newMQELineWorld(t)
	a := w.seed("a", "a1", []byte(mqeSpecYAML))
	b := w.seed("b", "b1", []byte(mqeSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	w.mqpHeldIntent(b, "b1", false)
	w.forgeReviews(a, []string{"alice"}, mqmReview(1, "alice", "DISMISSED", "a1"))
	w.forgeReviews(b, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "b1"))

	w.pumpBase(a)
	w.requireWaiting(a, "a1")
	w.requireHeldBehind(b)

	w.behind(a, "a1", "a2")
	w.pumpBase(a)
	if e := w.active(a); e.State != mergequeue.StateLive || e.AnchoredHeadSHA != "a2" || e.YieldedAt != nil {
		t.Fatalf("A after the first base move = %s at %s yielded_at=%v, want live at a2, unmarked", e.State, e.AnchoredHeadSHA, e.YieldedAt)
	}
	w.requireHeldBehind(b)

	w.behind(a, "a2", "a3")
	w.passAt(a, "a2")
	if e := w.active(a); e.State != mergequeue.StateLive || e.AnchoredHeadSHA != "a3" || e.YieldedAt != nil {
		t.Fatalf("A after the second base move = %s at %s yielded_at=%v, want live at a3, unmarked", e.State, e.AnchoredHeadSHA, e.YieldedAt)
	}
	w.requireHeldBehind(b)

	w.backdateApprovalWait(a)
	w.behind(a, "a3", "a4")
	w.passAt(a, "a3")

	e := w.requireYielded(a, mergeCandidateQueueReasonForgeApprovalDismissed, forgeApprovalNextApprovePR, mergeQueueYieldSiteMergeOnPass)
	if e.AnchoredHeadSHA != "a3" {
		t.Fatalf("A yielded at %s, want a3 (the head the expired readvance decided at)", e.AnchoredHeadSHA)
	}
	w.requireNoDispatchEvidence(a)
	w.requirePuts("b1")
	w.requireState(b, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
}

// TestMergeQueueE2E_ExpiredApprovalWaitYieldsOnRepeatedTransientHold (#4748;
// CF1 vehicle): A waits (dismissed); then every base re-probe fails (compare
// 500), so A holds transient and B stays held. Once A's anchor is past the
// window, the next pump YIELDS A forge_approval_undetermined (next_step
// retry_after) and B merges. MECHANISM: a transient hold returns before the
// fold, so without the enforcement no expiry judgement ever runs and A holds
// the line forever.
func TestMergeQueueE2E_ExpiredApprovalWaitYieldsOnRepeatedTransientHold(t *testing.T) {
	w := newMQELineWorld(t)
	a := w.seed("a", "a1", []byte(mqeSpecYAML))
	b := w.seed("b", "b1", []byte(mqeSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	w.mqpHeldIntent(b, "b1", false)
	w.forgeReviews(a, []string{"alice"}, mqmReview(1, "alice", "DISMISSED", "a1"))
	w.forgeReviews(b, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "b1"))

	w.pumpBase(a)
	w.requireWaiting(a, "a1")

	w.gh.set(func() { w.gh.compareStatus["a1"] = http.StatusInternalServerError })
	w.pumpBase(a)
	w.requireWaiting(a, "a1")
	w.requireHeldBehind(b)

	w.backdateApprovalWait(a)
	w.pumpBase(a)

	w.requireYielded(a, mergeCandidateQueueReasonForgeApprovalUndetermined, forgeApprovalNextRetryAfter, mergeQueueYieldSiteMergeOnPass)
	w.requireNoDispatchEvidence(a)
	w.requirePuts("b1")
	w.requireState(b, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
}

// TestMergeQueueE2E_SnapshotFailureWaitsThenYields (#4748; CF4 vehicle):
// every review snapshot of A fails from its FIRST merge-on-pass decision (the
// admission's veto read precedes the failure). After pump 1, before any
// backdate, A is live WITH an approval-wait anchor and B is held. After the
// backdate the pump yields A forge_approval_undetermined (retry_after) and B
// merges. Once the forge heals and A is approved, a targeted re-probe clears
// A. MECHANISM: as a transient hold the failure never calls ApprovalWait, so A
// has no anchor after pump 1, the no-episode pre-filter skips the
// enforcement, and A holds the line forever.
func TestMergeQueueE2E_SnapshotFailureWaitsThenYields(t *testing.T) {
	w := newMQELineWorld(t)
	a := w.seed("a", "a1", []byte(mqeSpecYAML))
	b := w.seed("b", "b1", []byte(mqeSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	w.mqpHeldIntent(b, "b1", false)
	w.forgeReviews(a, []string{"alice"}, mqmReview(1, "alice", "APPROVED", "a1"))
	w.forgeReviews(b, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "b1"))
	armed := false
	w.audit.onAppend = func(category string) {
		// A's admission records its pass; every later snapshot of A fails.
		if category == CategoryMergeQueueVerified && !armed {
			armed = true
			w.gh.set(func() { w.gh.graphQLFail[a.pr] = 1000 })
		}
	}

	w.pumpBase(a)
	if !armed {
		t.Fatal("A's admission recorded no merge_queue_verified row")
	}
	w.requireWaiting(a, "a1")
	w.requireHeldBehind(b)

	w.backdateApprovalWait(a)
	w.pumpBase(a)

	w.requireYielded(a, mergeCandidateQueueReasonForgeApprovalUndetermined, forgeApprovalNextRetryAfter, mergeQueueYieldSiteMergeOnPass)
	w.requireNoDispatchEvidence(a)
	w.requirePuts("b1")
	w.requireState(b, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)

	w.gh.set(func() {
		w.gh.graphQLFail[a.pr] = 0
		w.gh.mergeSHA[a.branch] = "a2"
	})
	if !w.srv.ReprobeMergeQueueYield(context.Background(), a.id, false) {
		t.Fatal("the targeted re-probe did not clear A once the forge healed and A is approved")
	}
	w.srv.bgMergeQueuePump.Wait()
	if n := len(w.rows(a, CategoryMergeQueueYieldCleared)); n != 1 {
		t.Fatalf("A merge_queue_yield_cleared rows = %d, want 1", n)
	}
}

// TestMergeQueueE2E_ApprovedFoldOnBaseMoveReadvancesPastAnExpiredWait (#4748;
// CF6 vehicle): A waited once (dismissed) and is then APPROVED; its anchor is
// backdated past the window and the base moves. The base-move fold approves,
// so A READVANCES (it does not yield), re-advances onto a2, passes there and
// merges, with no merge_queue_yielded row. MECHANISM: without the approved
// arm's ExpiryYield clear the readvance keeps the generic ExpiryYield and the
// expired wait yields an approved entry.
func TestMergeQueueE2E_ApprovedFoldOnBaseMoveReadvancesPastAnExpiredWait(t *testing.T) {
	w := newMQELineWorld(t)
	a := w.seed("a", "a1", []byte(mqeSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	w.forgeReviews(a, []string{"alice"}, mqmReview(1, "alice", "DISMISSED", "a1"))

	w.pumpBase(a)
	w.requireWaiting(a, "a1")

	w.forgeReviews(a, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "a1"))
	w.backdateApprovalWait(a)
	w.behind(a, "a1", "a2")
	w.pumpBase(a)

	if e := w.active(a); e.State != mergequeue.StateLive || e.AnchoredHeadSHA != "a2" || e.YieldedAt != nil {
		t.Fatalf("A after the base move = %s at %s yielded_at=%v, want live at a2, unmarked (re-advanced, not yielded)", e.State, e.AnchoredHeadSHA, e.YieldedAt)
	}
	w.forgeReviews(a, []string{"alice"}, mqmReview(3, "alice", "APPROVED", "a2"))
	w.passAt(a, "a2")

	w.requirePuts("a2")
	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	if n := len(w.rows(a, CategoryMergeQueueYielded)); n != 0 {
		t.Fatalf("merge_queue_yielded rows = %d, want 0", n)
	}
}

// TestMergeQueueE2E_HeadMovedReadvancePastExpiryYieldsThenReanchorsAndClears
// (#4748 R5; CF1 vehicle): A (review gate, its only review DISMISSED) waits at
// a1; its anchor is backdated past the window and the PR head moves to a1b.
// The next decision is a head_moved readvance carrying the generic
// ExpiryYield, and the enforcement YIELDS A forge_approval_undetermined
// (retry_after, site merge_on_pass) anchored at the OLD head a1, with no
// claim, verdict or authorization row. A targeted re-probe with no approval
// at a1b takes the reanchor arm (ADR-112 A1.1): A is still yielded with the
// SAME yielded_at, anchored at a1b. After an APPROVED review at a1b a second
// targeted re-probe clears A at a1b, and the detached pump re-admits A
// unmarked at a1b and merges it pinned there. MECHANISM: without the
// enforcement the readvance re-advances A (no merge_queue_yielded row; A live
// at a1b).
func TestMergeQueueE2E_HeadMovedReadvancePastExpiryYieldsThenReanchorsAndClears(t *testing.T) {
	w := newMQELineWorld(t)
	ctx := context.Background()
	a := w.seed("a", "a1", []byte(mqeSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	w.forgeReviews(a, []string{"alice"}, mqmReview(1, "alice", "DISMISSED", "a1"))

	w.pumpBase(a)
	w.requireWaiting(a, "a1")

	w.backdateApprovalWait(a)
	w.gh.set(func() { w.gh.prs[a.pr].head = "a1b" })
	w.pumpBase(a)

	y := w.requireYielded(a, mergeCandidateQueueReasonForgeApprovalUndetermined, forgeApprovalNextRetryAfter, mergeQueueYieldSiteMergeOnPass)
	if y.AnchoredHeadSHA != "a1" {
		t.Fatalf("A yielded anchored at %s, want the old head a1", y.AnchoredHeadSHA)
	}
	w.requireNoDispatchEvidence(a)

	if w.srv.ReprobeMergeQueueYield(ctx, a.id, false) {
		t.Fatal("the targeted re-probe cleared A with no approval at a1b")
	}
	re := w.active(a)
	if re.State != mergequeue.StateHeld || re.YieldedAt == nil || !re.YieldedAt.Equal(*y.YieldedAt) || re.AnchoredHeadSHA != "a1b" {
		t.Fatalf("A after the reanchor re-probe = %s yielded_at=%v (was %v) at %s, want held, same yielded_at, anchored a1b",
			re.State, re.YieldedAt, y.YieldedAt, re.AnchoredHeadSHA)
	}

	w.forgeReviews(a, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "a1b"))
	if !w.srv.ReprobeMergeQueueYield(ctx, a.id, false) {
		t.Fatal("the targeted re-probe did not clear A approved at a1b")
	}
	w.srv.bgMergeQueuePump.Wait()
	cleared := w.rows(a, CategoryMergeQueueYieldCleared)
	if len(cleared) != 1 || w.payload(cleared[0])["head_sha"] != "a1b" {
		t.Fatalf("merge_queue_yield_cleared rows = %d, want 1 at a1b", len(cleared))
	}
	// The re-admission at a1b (unmarked) needs no merge-candidate pass
	// there, so the same detached pump merges A pinned to a1b.
	w.requirePuts("a1b")
	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
}

// TestMergeQueueE2E_AdmissionVetoYieldsAndLineContinues pins ADR-112 R3's
// admission precedence (approval condition C5): an intent-bearing entry whose
// ONLY ineligibility is a write-level CHANGES_REQUESTED (row 2) YIELDS at its
// admission (held, marked forge_approval_vetoed, next_step dismiss_review,
// site admission) and the pump continues to B, which merges; the SAME veto
// beside a determined non-approval reason (row 1: a high concern) drops
// not_merge_eligible as before. Counterfactuals: returning no veto from the
// admission check admits A (no yield row), RED; yielding on any reason set
// containing the veto yields the co-occurrence case instead of dropping, RED.
func TestMergeQueueE2E_AdmissionVetoYieldsAndLineContinues(t *testing.T) {
	t.Run("veto alone yields", func(t *testing.T) {
		w := newMQELineWorld(t)
		a := w.seed("a", "a1", []byte(mqeSpecYAML))
		b := w.seed("b", "b1", []byte(mqeSpecYAML))
		w.mqpHeldIntent(a, "a1", false)
		w.mqpHeldIntent(b, "b1", false)
		w.forgeReviews(a, []string{"alice", "bob"}, mqmReview(1, "alice", "APPROVED", "a1"), mqmReview(2, "bob", "CHANGES_REQUESTED", "a1"))
		w.forgeReviews(b, []string{"alice"}, mqmReview(3, "alice", "APPROVED", "b1"))

		w.pumpBase(a)

		e := w.requireYielded(a, mergeCandidateQueueReasonForgeApprovalVetoed, forgeApprovalNextDismissReview, mergeQueueYieldSiteAdmission)
		if !strings.Contains(e.YieldDetail, "bob") {
			t.Fatalf("A yield detail = %q, want the vetoing reviewer named", e.YieldDetail)
		}
		w.requirePuts("b1")
		w.requireState(b, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	})
	t.Run("veto beside a blocking concern drops", func(t *testing.T) {
		w := newMQELineWorld(t)
		a := w.seed("a", "a1", []byte(mqeSpecYAML))
		w.mqpHeldIntent(a, "a1", false)
		w.forgeReviews(a, []string{"alice", "bob"}, mqmReview(1, "alice", "APPROVED", "a1"), mqmReview(2, "bob", "CHANGES_REQUESTED", "a1"))
		w.raiseConcern(a, "high", "")

		w.pumpBase(a)

		w.requireState(a, mergequeue.StateDropped, mergeCandidateQueueReasonNotMergeEligible)
		if n := len(w.rows(a, CategoryMergeQueueYielded)); n != 0 {
			t.Fatalf("merge_queue_yielded rows = %d, want 0 (row 1 drops before row 2)", n)
		}
		w.requirePuts()
	})
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
// auto-merge ladder (QueueMerge, #4641), pinned to the head it is handed.
type mqeAutoMerger struct{ gh *githubclient.Client }

func (m mqeAutoMerger) MergePullRequest(ctx context.Context, runRow *run.Run, head string) error {
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return err
	}
	return m.gh.QueueMerge(ctx, forge.FromGitHubInstallationID(*runRow.InstallationID), repo,
		parsePRNumberFromURL(runRow.PullRequestURL), forge.MergeMethodSquash, head)
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

// TestMergeQueueE2E_RedPassDelegatedFixupReentersAtTail is ADR-093 slice 3's
// cross-layer walk (D4, E87.6 / #4297): the merge-candidate verdict upload
// (succeedMergeCandidateVerifiedStage) for a LIVE intent-bearing entry A
// ahead of held B and C, the real Postgres queue and audit chain, the ADR-090
// D6 route under a delegated workflow, the merge_queue_ejected row, a
// fixup_pushed row with a moved PR head on the forge fake, the re-entry sweep,
// and the per-base queue read over HTTP. A is ejected verify_failed; B is
// admitted live in the same pump (the line is not wedged); A re-enters with
// merge intent at the TAIL behind C, under the SYSTEM actor (security c),
// its entry naming the re-entry row's sequence.
func TestMergeQueueE2E_RedPassDelegatedFixupReentersAtTail(t *testing.T) {
	w := newMQMWorld(t)
	ctx := context.Background()
	a, b := w.waveSpec(mcvDelegatedV0SpecYAML)
	intent := w.mqeIntent(a, "a1")
	if _, err := w.store.MarkMergeIntent(ctx, w.entry(a).ID, &mergequeue.MergeIntentRequest{IntentSequence: intent.Sequence}); err != nil {
		t.Fatalf("mark intent: %v", err)
	}
	c := w.seed("c", "c0", []byte(mcvDelegatedV0SpecYAML))
	w.enqueueDirect(c, "c0")

	resp := w.shipVerdict(a, "a1", mergeCandidateResultFailed)
	if !resp.FixupRouted {
		t.Fatalf("routed = false (refusal %q), want the D6 fix-up routed", resp.FixupRefusal)
	}
	ej := w.requireState(a, mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed)
	w.requireState(b, mergequeue.StateLive, "")
	if p := w.oneEjection(a, mergeCandidateQueueReasonVerifyFailed, mergeQueueEjectNextAwaitDelegatedFixup); p["merge_intent"] != true {
		t.Fatalf("ejection = %v, want merge intent", p)
	}
	if n := len(w.rows(a, CategoryStageFixupTriggered)); n != 1 {
		t.Fatalf("stage_fixup_triggered rows = %d, want 1", n)
	}
	ejected, found, err := w.store.LatestForRun(ctx, a.id)
	if err != nil || !found || ejected.ID != ej.ID || ejected.MergeIntentSequence != intent.Sequence {
		t.Fatalf("latest = %+v %v %v, want the ejected intent row", ejected, found, err)
	}

	// Before the fix-up pushes, the sweep leaves A out.
	w.exec(`UPDATE merge_candidate_queue_entries SET settled_at = settled_at - interval '10 minutes' WHERE id = $1`, ej.ID)
	w.setStage(a.impl.ID, run.StageStateSucceeded)
	if n := w.sweep(0); n != 0 {
		t.Fatalf("re-entered = %d before the fix-up pushed, want 0", n)
	}
	// The delegated fix-up pushes a2 and its stage settles.
	w.rentPush(a, "a2")
	if n := w.sweep(0); n != 1 {
		t.Fatalf("re-entered = %d after the fix-up pushed, want 1", n)
	}
	st, _ := w.requireReentered(a, ejected, "a2")

	q := decodeMergeQueue(t, getMergeQueue(t, w.srv, "base_ref="+mcqBase, mqrOperator("")))
	if len(q.Bases) != 1 {
		t.Fatalf("bases = %+v, want one", q.Bases)
	}
	es := q.Bases[0].Entries
	if len(es) != 3 || es[0].RunID != b.id.String() || es[0].State != string(mergequeue.StateLive) ||
		es[1].RunID != c.id.String() || es[2].RunID != a.id.String() {
		t.Fatalf("queue = %+v, want B live, then C, then the re-entered A at the tail", es)
	}
	if es[2].EntryID != st.Entry.ID.String() || !es[2].MergeIntent || es[2].Position != 2 || es[2].AnchoredHeadSHA != "a2" {
		t.Fatalf("re-entered A = %+v, want the fresh entry with merge intent at position 2 behind C, anchored a2", es[2])
	}
}
