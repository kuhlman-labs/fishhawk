package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// #4676: the head-pinned readiness pre-read (pinnedMergeReadiness), the
// inline not_merge_ready / post-verdict merge_conflicting answers and the
// delegated observe-only notes, over the mqmWorld harness (real Postgres
// audit chain, queue and D1 ledger; the mcq GitHub fake behind a proxy that
// serves the per-PR mergeable_state).

// nmrRunRow reads r's run row.
func (w *mqmWorld) nmrRunRow(r *mcqRun) *run.Run {
	w.t.Helper()
	row, err := w.repo.GetRun(context.Background(), r.id)
	if err != nil {
		w.t.Fatalf("GetRun: %v", err)
	}
	return row
}

// nmrErr decodes a writeError body, requiring code.
func nmrErr(t *testing.T, body []byte, code string) (string, map[string]any) {
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
	if env.Error.Code != code {
		t.Fatalf("code = %q, want %s: %s", env.Error.Code, code, body)
	}
	return env.Error.Message, env.Error.Details
}

// nmrSeedClaim seeds a D1 claim at h1 through the real ledger, optionally
// moved to dispatch_failed.
func (w *mqmWorld) nmrSeedClaim(r *mcqRun, path string, state mergequeue.DispatchState) {
	w.t.Helper()
	ctx := context.Background()
	if _, err := w.store.ClaimMergeDispatch(ctx, r.id, path, "h1"); err != nil {
		w.t.Fatalf("seed claim: %v", err)
	}
	if state == mergequeue.DispatchFailed {
		if err := w.store.MarkMergeDispatchFailed(ctx, r.id, "seeded"); err != nil {
			w.t.Fatalf("mark failed: %v", err)
		}
	}
}

// TestPinnedMergeReadiness_States pins the pre-read's classification at the
// gate head h1: conflict first (Z1), unstable (CC5), blocked / unknown
// resumable, behind / draft not, and clean / has_hooks / empty / an
// unrecognised value fall through. A head mismatch, an empty gate head (the
// pinned-only control, C5) and a closed unmerged PR (CC6) fall through too.
func TestPinnedMergeReadiness_States(t *testing.T) {
	w := newMQMWorld(t)
	r := w.faeInline("nmr-states", mmrSpec(""))
	row := w.nmrRunRow(r)
	for _, tc := range []struct {
		name, state, gateHead string
		closed                bool
		refuse, resumable     bool
		conflicting, unstable bool
	}{
		{name: "dirty", state: "dirty", gateHead: "h1", refuse: true, conflicting: true},
		{name: "unstable", state: "unstable", gateHead: "h1", refuse: true, resumable: true, unstable: true},
		{name: "blocked", state: "blocked", gateHead: "h1", refuse: true, resumable: true},
		{name: "unknown", state: "unknown", gateHead: "h1", refuse: true, resumable: true},
		{name: "behind", state: "behind", gateHead: "h1", refuse: true},
		{name: "draft", state: "draft", gateHead: "h1", refuse: true},
		{name: "clean", state: "clean", gateHead: "h1"},
		{name: "has_hooks", state: "has_hooks", gateHead: "h1"},
		{name: "empty", state: "", gateHead: "h1"},
		{name: "unrecognised", state: "sideways", gateHead: "h1"},
		{name: "head_mismatch", state: "blocked", gateHead: "h0"},
		{name: "unpinned", state: "blocked", gateHead: ""},
		{name: "closed_unmerged", state: "blocked", gateHead: "h1", closed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w.mergeable(r, tc.state)
			prState := "open"
			if tc.closed {
				prState = "closed"
			}
			w.gh.set(func() { w.gh.prs[r.pr].state = prState })
			got := w.srv.pinnedMergeReadiness(context.Background(), row, tc.gateHead, nil)
			if (got.Refusal != nil) != tc.refuse {
				t.Fatalf("refusal = %v, want refuse=%v", got.Refusal, tc.refuse)
			}
			if !tc.refuse {
				return
			}
			if got.Resumable != tc.resumable {
				t.Errorf("resumable = %v, want %v", got.Resumable, tc.resumable)
			}
			if isNotMergeReadyConflict(got.Refusal) != tc.conflicting {
				t.Errorf("conflicting = %v, want %v (%v)", isNotMergeReadyConflict(got.Refusal), tc.conflicting, got.Refusal)
			}
			if errors.Is(got.Refusal, forge.ErrPullRequestUnstableStatus) != tc.unstable {
				t.Errorf("unstable identity = %v, want %v", !tc.unstable, tc.unstable)
			}
		})
	}
	w.gh.set(func() { w.gh.prs[r.pr].state = "open" })
}

// TestPinnedMergeReadiness_BlockedReviewDecision (Z6, C16): a blocked PR whose
// reviewDecision is REVIEW_REQUIRED or CHANGES_REQUESTED is non-resumable with
// blocked_by; APPROVED, null and a snapshot read error stay resumable.
func TestPinnedMergeReadiness_BlockedReviewDecision(t *testing.T) {
	w := newMQMWorld(t)
	r := w.faeInline("nmr-review", mmrSpec(""))
	row := w.nmrRunRow(r)
	w.mergeable(r, "blocked")
	for _, tc := range []struct {
		name, decision, blockedBy string
		fail                      bool
		resumable                 bool
	}{
		{name: "review_required", decision: "REVIEW_REQUIRED", blockedBy: notMergeReadyBlockedByReviewRequired},
		{name: "changes_requested", decision: "CHANGES_REQUESTED", blockedBy: notMergeReadyBlockedByChangesRequested},
		{name: "approved", decision: "APPROVED", resumable: true},
		{name: "null", decision: "", resumable: true},
		{name: "snapshot_error", decision: "REVIEW_REQUIRED", fail: true, resumable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w.gh.setReviewSnapshot(r.pr, mcqReviewSnapshot{ReviewDecision: tc.decision, MergeStateStatus: "BLOCKED"})
			if tc.fail {
				w.gh.set(func() { w.gh.graphQLFail[r.pr] = 5 })
				defer w.gh.set(func() { w.gh.graphQLFail[r.pr] = 0 })
			}
			got := w.srv.pinnedMergeReadiness(context.Background(), row, "h1", nil)
			if got.Refusal == nil {
				t.Fatal("blocked did not refuse")
			}
			if got.Resumable != tc.resumable || got.BlockedBy != tc.blockedBy {
				t.Fatalf("resumable=%v blocked_by=%q, want %v %q", got.Resumable, got.BlockedBy, tc.resumable, tc.blockedBy)
			}
			_, res, by, _ := notMergeReadyOf(got.Refusal)
			if res != tc.resumable || by != tc.blockedBy {
				t.Fatalf("refusal carries resumable=%v blocked_by=%q", res, by)
			}
		})
	}
}

// TestPinnedMergeReadiness_HungReadFallsThrough (Z9, CC4, C17): the pre-read
// runs under its own mergePinnedPreReadTimeout bound; a GET that hangs until
// its request context ends falls through within the bound.
func TestPinnedMergeReadiness_HungReadFallsThrough(t *testing.T) {
	if mergePinnedPreReadTimeout != 5*time.Second {
		t.Fatalf("mergePinnedPreReadTimeout = %v, want 5s (CC4)", mergePinnedPreReadTimeout)
	}
	w := newMQMWorld(t)
	r := w.faeInline("nmr-hung", mmrSpec(""))
	row := w.nmrRunRow(r)
	w.mergeable(r, "blocked")
	orig := mergePinnedPreReadTimeout
	mergePinnedPreReadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { mergePinnedPreReadTimeout = orig })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	w.mu.Lock()
	w.onResponse = func(string) { <-release }
	w.mu.Unlock()
	done := make(chan pinnedReadiness, 1)
	go func() { done <- w.srv.pinnedMergeReadiness(context.Background(), row, "h1", nil) }()
	select {
	case got := <-done:
		if got.Refusal != nil {
			t.Fatalf("hung pre-read refused %v, want fall-through", got.Refusal)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pinned pre-read did not return within 5s: it is not bounded")
	}
}

// TestMergeRunNotMergeReady_PreReadWritesNoClaim (C4): blocked at the gate
// head answers 409 merge_checks_pending reason not_merge_ready BEFORE the
// claim, though the inline merger would succeed: zero dispatches, no claim, no
// settle row, a durable verdict row.
func TestMergeRunNotMergeReady_PreReadWritesNoClaim(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.faeInline("nmr-noclaim", mmrSpec(""))
	w.mergeable(r, "blocked")
	resp := w.mergeRun(r)
	if resp.Code != http.StatusConflict {
		t.Fatalf("merge = %d %s, want 409", resp.Code, resp.Body.String())
	}
	msg, d := nmrErr(t, resp.Body.Bytes(), "merge_checks_pending")
	if d["reason"] != "not_merge_ready" || d["mergeable_state"] != "blocked" || d["resumable"] != true || d["head_sha"] != "h1" {
		t.Fatalf("details = %v", d)
	}
	if !strings.Contains(msg, "never queues an auto-merge") || strings.Contains(msg, "retry the merge") {
		t.Fatalf("message = %q", msg)
	}
	if m.inlineCalls() != 0 || w.claims(r) != 0 || len(w.rows(r, categoryMergeDispatchSettled)) != 0 {
		t.Fatalf("dispatches=%d claims=%d settled=%d, want 0/0/0", m.inlineCalls(), w.claims(r), len(w.rows(r, categoryMergeDispatchSettled)))
	}
	if n := len(w.rows(r, CategoryMergeVerdictRecorded)); n != 1 {
		t.Fatalf("verdict rows = %d, want 1", n)
	}
}

// TestMergeRunNotMergeReady_InlineStates pins the inline answer per state:
// unstable keeps checks_pending (CC5); behind / draft are non-resumable with
// the remedy; clean / empty fall through to the merger (one dispatch).
func TestMergeRunNotMergeReady_InlineStates(t *testing.T) {
	for _, tc := range []struct {
		state, reason string
		resumable     bool
		dispatched    bool
		remedy        string
	}{
		{state: "unstable", reason: "checks_pending"},
		{state: "unknown", reason: "not_merge_ready", resumable: true},
		{state: "behind", reason: "not_merge_ready", remedy: "fishhawk_rebase_run_branch"},
		{state: "draft", reason: "not_merge_ready", remedy: "gh pr ready"},
		{state: "clean", dispatched: true},
		{state: "", dispatched: true},
	} {
		t.Run("state_"+tc.state, func(t *testing.T) {
			w, m := newMQEWorld(t)
			r := w.faeInline("nmr-st", mmrSpec(""))
			w.mergeable(r, tc.state)
			resp := w.mergeRun(r)
			if tc.dispatched {
				if resp.Code != http.StatusOK || m.inlineCalls() != 1 {
					t.Fatalf("merge = %d (%d dispatches) %s, want 200 with one dispatch", resp.Code, m.inlineCalls(), resp.Body.String())
				}
				return
			}
			msg, d := nmrErr(t, resp.Body.Bytes(), "merge_checks_pending")
			if d["reason"] != tc.reason {
				t.Fatalf("reason = %v, want %s", d["reason"], tc.reason)
			}
			if tc.reason == "not_merge_ready" && d["resumable"] != tc.resumable {
				t.Fatalf("resumable = %v, want %v", d["resumable"], tc.resumable)
			}
			if tc.remedy != "" && (!strings.Contains(msg, tc.remedy) || !strings.Contains(msg, "waiting will not clear it")) {
				t.Fatalf("message lacks remedy %q: %s", tc.remedy, msg)
			}
			if m.inlineCalls() != 0 || w.claims(r) != 0 {
				t.Fatalf("dispatches=%d claims=%d, want 0/0", m.inlineCalls(), w.claims(r))
			}
		})
	}
}

// TestMergeRunNotMergeReady_DirtyIsConflictGuardFirst (Z1): dirty at h1 is
// answered by the #3109 guard BEFORE the verdict: 409 merge_conflicting, no
// verdict row, no dispatch, never not_merge_ready.
func TestMergeRunNotMergeReady_DirtyIsConflictGuardFirst(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.faeInline("nmr-dirty", mmrSpec(""))
	w.mergeable(r, "dirty")
	resp := w.mergeRun(r)
	_, d := nmrErr(t, resp.Body.Bytes(), "merge_conflicting")
	if d["verdict_recorded"] != nil || strings.Contains(resp.Body.String(), "not_merge_ready") {
		t.Fatalf("guard answer carries post-verdict keys: %s", resp.Body.String())
	}
	if n := len(w.rows(r, CategoryMergeVerdictRecorded)); n != 0 || m.inlineCalls() != 0 {
		t.Fatalf("verdict rows=%d dispatches=%d, want 0/0", n, m.inlineCalls())
	}
}

// TestMergeRunNotMergeReady_ExistingClaimAnswersAlreadyDispatched (Z2, C14):
// an existing claim, dispatched or dispatch_failed, skips the pre-read even on
// a not-ready PR, so the claim path answers merge_already_dispatched, never
// not_merge_ready, and the claim row is unchanged.
func TestMergeRunNotMergeReady_ExistingClaimAnswersAlreadyDispatched(t *testing.T) {
	for _, tc := range []struct {
		claim mergequeue.DispatchState
		state string
	}{
		{mergequeue.DispatchDispatched, "blocked"},
		{mergequeue.DispatchFailed, "draft"},
	} {
		t.Run(string(tc.claim), func(t *testing.T) {
			w, m := newMQEWorld(t)
			r := w.faeInline("nmr-claim", mmrSpec(""))
			w.mergeable(r, tc.state)
			w.nmrSeedClaim(r, mergequeue.DispatchPathDelegated, tc.claim)
			resp := w.mergeRun(r)
			if strings.Contains(resp.Body.String(), "not_merge_ready") {
				t.Fatalf("body reports not_merge_ready over a claim: %s", resp.Body.String())
			}
			_, d := nmrErr(t, resp.Body.Bytes(), "merge_already_dispatched")
			if d["claim_state"] != string(tc.claim) {
				t.Fatalf("claim_state = %v, want %s", d["claim_state"], tc.claim)
			}
			if m.inlineCalls() != 0 {
				t.Fatalf("dispatches = %d, want 0", m.inlineCalls())
			}
			w.requireClaim(r, tc.claim)
		})
	}
}

// TestMergeRunNotMergeReady_LeafRefusalReleasesClaim (C3): an empty state
// lets the pre-read fall through; the leaf refuses NotMergeReadyError after
// the claim, which RELEASES it (one released settle row), so a second POST
// reaches the leaf again.
func TestMergeRunNotMergeReady_LeafRefusalReleasesClaim(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.faeInline("nmr-leaf", mmrSpec(""))
	m.mu.Lock()
	m.inlineErr = fmt.Errorf("pinned: %w", &forge.NotMergeReadyError{MergeableState: "unknown", ExpectedHeadSHA: "h1"})
	m.mu.Unlock()
	resp := w.mergeRun(r)
	_, d := nmrErr(t, resp.Body.Bytes(), "merge_checks_pending")
	if d["reason"] != "not_merge_ready" || d["resumable"] != true {
		t.Fatalf("details = %v", d)
	}
	w.requireClaim(r, "")
	if got := w.settledOutcomes(r); len(got) != 1 || got[0] != string(mergeDispatchRelease) {
		t.Fatalf("settled outcomes = %v, want one released", got)
	}
	_ = w.mergeRun(r)
	if m.inlineCalls() != 2 {
		t.Fatalf("dispatches = %d, want 2 (the released claim re-dispatches)", m.inlineCalls())
	}
}

// TestHandleMergeRun_LeafConflictAfterGuardAnswersConflicting (Z1, C12): a
// leaf conflict (NotMergeReadyError Conflicting) seen after the guard answers
// 409 merge_conflicting with verdict_recorded true, never not_merge_ready.
func TestHandleMergeRun_LeafConflictAfterGuardAnswersConflicting(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.faeInline("nmr-leafconf", mmrSpec(""))
	m.mu.Lock()
	m.inlineErr = &forge.NotMergeReadyError{MergeableState: "dirty", ExpectedHeadSHA: "h1", Conflicting: true}
	m.mu.Unlock()
	resp := w.mergeRun(r)
	msg, d := nmrErr(t, resp.Body.Bytes(), "merge_conflicting")
	if d["verdict_recorded"] != true || d["verdict_sequence"] == nil || d["mergeable_state"] != "dirty" {
		t.Fatalf("details = %v", d)
	}
	if !strings.Contains(msg, "became conflicting after the conflict guard read it") || strings.Contains(resp.Body.String(), "not_merge_ready") {
		t.Fatalf("message = %q", msg)
	}
	if n := len(w.rows(r, CategoryMergeVerdictRecorded)); n != 1 {
		t.Fatalf("verdict rows = %d, want 1", n)
	}
	w.requireClaim(r, "")
}

// TestMergeRunNotMergeReady_PreReadConflictAnswersConflicting (Z1, C12): a
// conflict only the pre-read sees (the guard's and the gate's reads answered
// clean) answers merge_conflicting with the verdict recorded and no dispatch.
func TestMergeRunNotMergeReady_PreReadConflictAnswersConflicting(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.faeInline("nmr-preconf", mmrSpec(""))
	// Each proxied pull GET bumps the counter; the guard and the gate read
	// first, so flipping to dirty once two reads were served lands it on the
	// pre-read only.
	var gets int
	w.mu.Lock()
	w.onResponse = func(path string) {
		if !mqmPullPath.MatchString(path) {
			return
		}
		gets++
		if gets == 2 {
			w.states[r.pr] = "dirty"
		}
	}
	w.mu.Unlock()
	resp := w.mergeRun(r)
	_, d := nmrErr(t, resp.Body.Bytes(), "merge_conflicting")
	if d["verdict_recorded"] != true {
		t.Fatalf("details = %v, want verdict_recorded true (the pre-read saw the conflict)", d)
	}
	if m.inlineCalls() != 0 || w.claims(r) != 0 {
		t.Fatalf("dispatches=%d claims=%d, want 0/0", m.inlineCalls(), w.claims(r))
	}
}

// TestMergeRun_PreReadIsPinnedOnlyAndUnattributed405Keeps502 (X1): a leaf
// ErrPullRequestNotMergeable on a pinned dispatch keeps the 502 and never
// answers not_merge_ready.
func TestMergeRun_PinnedUnattributed405Keeps502(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.faeInline("nmr-405", mmrSpec(""))
	m.mu.Lock()
	m.inlineErr = fmt.Errorf("put: %w", forge.ErrPullRequestNotMergeable)
	m.mu.Unlock()
	resp := w.mergeRun(r)
	if resp.Code != http.StatusBadGateway || strings.Contains(resp.Body.String(), "not_merge_ready") {
		t.Fatalf("merge = %d %s, want 502 without not_merge_ready", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "synchronous head-pinned dispatch") {
		t.Fatalf("502 still claims a queued merge: %s", resp.Body.String())
	}
}

// TestAutoDrive_NotMergeReady_PreClaim (C9): blocked at liveHead is
// observe-only BEFORE the claim: nothing claimed, the merger never called,
// MergeNotReady pre_claim resumable.
func TestAutoDrive_NotMergeReady_PreClaim(t *testing.T) {
	w := newMQMWorld(t)
	r := w.admqReady("nmr-pre", "h1")
	w.mergeable(r, "blocked")
	m := &admqMerger{}
	out := w.admqDrive(r, m)
	if out.Acted || m.calls() != 0 || w.claims(r) != 0 {
		t.Fatalf("out=%+v merger=%d claims=%d, want observe-only, 0, 0", out, m.calls(), w.claims(r))
	}
	nr := out.MergeNotReady
	if nr == nil || nr.ClaimOutcome != autoDriveClaimOutcomePreClaim || nr.MergeableState != "blocked" || !nr.Resumable || nr.Conflicting {
		t.Fatalf("MergeNotReady = %+v", nr)
	}
	if !strings.Contains(out.Note, "nothing was merged, armed or claimed") {
		t.Fatalf("note = %q", out.Note)
	}
}

// TestAutoDrive_NotMergeReady_NonResumablePreClaim: draft is non-resumable
// and the note names the remedy.
func TestAutoDrive_NotMergeReady_NonResumablePreClaim(t *testing.T) {
	w := newMQMWorld(t)
	r := w.admqReady("nmr-draft", "h1")
	w.mergeable(r, "draft")
	out := w.admqDrive(r, &admqMerger{})
	if out.MergeNotReady == nil || out.MergeNotReady.Resumable || !strings.Contains(out.Note, "gh pr ready") ||
		!strings.Contains(out.Note, "waiting will not clear it") {
		t.Fatalf("out = %+v", out)
	}
}

// TestAutoDrive_NotMergeReady_Released (C8): a leaf refusal after the claim
// releases it and is observe-only with a nil error, naming the release.
func TestAutoDrive_NotMergeReady_Released(t *testing.T) {
	w := newMQMWorld(t)
	r := w.admqReady("nmr-rel", "h1")
	m := &admqMerger{err: &forge.NotMergeReadyError{MergeableState: "unknown", ExpectedHeadSHA: "h1"}}
	out, err := w.admqDriveErr(r, m)
	if err != nil || out.Acted {
		t.Fatalf("out=%+v err=%v, want observe-only nil error", out, err)
	}
	if out.MergeNotReady == nil || out.MergeNotReady.ClaimOutcome != autoDriveClaimOutcomeReleased || !out.MergeNotReady.Resumable {
		t.Fatalf("MergeNotReady = %+v", out.MergeNotReady)
	}
	if !strings.Contains(out.Note, "claim was released") || m.calls() != 1 || w.claims(r) != 0 {
		t.Fatalf("note=%q merger=%d claims=%d", out.Note, m.calls(), w.claims(r))
	}
}

// TestAutoDrive_NotMergeReady_ExistingClaimIsAlreadyClaimed (Z2, C15): an
// existing claim skips the pre-read, so the arm answers already-claimed and
// MergeNotReady stays nil, though the PR is blocked at liveHead.
func TestAutoDrive_NotMergeReady_ExistingClaimIsAlreadyClaimed(t *testing.T) {
	for _, st := range []mergequeue.DispatchState{mergequeue.DispatchDispatched, mergequeue.DispatchFailed} {
		t.Run(string(st), func(t *testing.T) {
			w := newMQMWorld(t)
			r := w.admqReady("nmr-ac", "h1")
			w.mergeable(r, "blocked")
			w.nmrSeedClaim(r, mergequeue.DispatchPathInline, st)
			m := &admqMerger{}
			out := w.admqDrive(r, m)
			if out.Note != autoDriveNoteAlreadyClaimed || out.MergeNotReady != nil || m.calls() != 0 {
				t.Fatalf("out=%+v merger=%d, want the already-claimed note", out, m.calls())
			}
		})
	}
}

// TestAutoDrive_PinnedConflictNamesConflict (Z1, C13): dirty at liveHead is
// observe-only naming the conflict and its remedy, never "not merge-ready".
func TestAutoDrive_PinnedConflictNamesConflict(t *testing.T) {
	w := newMQMWorld(t)
	r := w.admqReady("nmr-conf", "h1")
	w.mergeable(r, "dirty")
	m := &admqMerger{}
	out := w.admqDrive(r, m)
	if out.MergeNotReady == nil || !out.MergeNotReady.Conflicting || out.MergeNotReady.Resumable {
		t.Fatalf("MergeNotReady = %+v", out.MergeNotReady)
	}
	if !strings.Contains(out.Note, "merge conflict") || !strings.Contains(out.Note, "fishhawk_vouch_commit") ||
		strings.Contains(out.Note, "merge-ready") || m.calls() != 0 || w.claims(r) != 0 {
		t.Fatalf("note=%q merger=%d claims=%d", out.Note, m.calls(), w.claims(r))
	}
}

// TestAutoDrive_NotMergeReady_UnstableKeepsError (CC5): unstable at liveHead
// keeps today's delegated error (no observe-only), with no claim taken.
func TestAutoDrive_NotMergeReady_UnstableKeepsError(t *testing.T) {
	w := newMQMWorld(t)
	r := w.admqReady("nmr-unst", "h1")
	w.mergeable(r, "unstable")
	m := &admqMerger{}
	out, err := w.admqDriveErr(r, m)
	if err == nil || !errors.Is(err, forge.ErrPullRequestUnstableStatus) || out.MergeNotReady != nil {
		t.Fatalf("out=%+v err=%v, want the unstable error", out, err)
	}
	if m.calls() != 0 || w.claims(r) != 0 {
		t.Fatalf("merger=%d claims=%d, want 0/0", m.calls(), w.claims(r))
	}
}

// TestAutoDriveMergeNotReadyNote_FollowsClaimOutcome (X6, C10): the note names
// each claim outcome; dispatch_failed names the re-arm.
func TestAutoDriveMergeNotReadyNote_FollowsClaimOutcome(t *testing.T) {
	for outcome, want := range map[string]string{
		autoDriveClaimOutcomePreClaim:       "nothing was merged, armed or claimed",
		autoDriveClaimOutcomeReleased:       "claim was released",
		autoDriveClaimOutcomeDispatchFailed: "operator re-arm",
		autoDriveClaimOutcomeUnclaimed:      "no merge-dispatch ledger is wired",
	} {
		if note := autoDriveMergeNotReadyNote(outcome, "blocked", "", false, true); !strings.Contains(note, want) {
			t.Errorf("%s note = %q, want %q", outcome, note, want)
		}
	}
	if got := autoDriveNotMergeReadyClaimOutcome(mergeDispatchUncertain); got != autoDriveClaimOutcomeDispatchFailed {
		t.Errorf("uncertain settle = %q, want dispatch_failed", got)
	}
	if note := autoDriveMergeNotReadyNote(autoDriveClaimOutcomePreClaim, "sideways", "", false, false); !strings.Contains(note, "unrecognized") {
		t.Errorf("unrecognised state note = %q", note)
	}
}
