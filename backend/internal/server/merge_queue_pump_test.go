package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// ADR-093 slice 2 (E87.5 / #4296): the pump side of merge-on-pass — the
// admission-epoch CAS against a stale decision, the continuation rule, the
// live-entry branch and the bounded readvance. The decision itself is pinned
// by merge_queue_merge_test.go; these drive pumpMergeCandidateQueue over the
// real Postgres store, the real D1 ledger and the mcq fake forge.

// mqpHeldIntent seeds r eligible at head (a clean advance onto head, passed
// there unless noVerdict) with a HELD entry carrying merge intent at the
// merge_queue_entered row's sequence, and returns that entry.
func (w *mqmWorld) mqpHeldIntent(r *mcqRun, head string, noVerdict bool) mergequeue.Entry {
	w.t.Helper()
	w.mqeRebased(r, "pre-"+head, head, head, false)
	if !noVerdict {
		w.mqePassed(r, head)
	}
	anchor := w.mqeIntent(r, head)
	res, err := w.store.Enqueue(context.Background(), mergequeue.EnqueueRequest{
		RunID: r.id, BaseRef: mcqBase, HeadSHA: head, MergeIntent: true, IntentSequence: anchor.Sequence})
	if err != nil {
		w.t.Fatalf("enqueue: %v", err)
	}
	return res.Entry
}

func (w *mqmWorld) storeEntry(r *mcqRun) mergequeue.Entry {
	w.t.Helper()
	var e mergequeue.Entry
	err := w.pool.QueryRow(context.Background(), `SELECT id, state, COALESCE(phase, ''), anchored_head_sha, admission_epoch
FROM merge_candidate_queue_entries WHERE run_id = $1 ORDER BY enqueued_at DESC, id DESC LIMIT 1`, r.id).
		Scan(&e.ID, &e.State, &e.Phase, &e.AnchoredHeadSHA, &e.AdmissionEpoch)
	if err != nil {
		w.t.Fatalf("read entry: %v", err)
	}
	return e
}

func (w *mqmWorld) claims(r *mcqRun) int {
	w.t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM merge_dispatch_claims WHERE run_id = $1`, r.id).Scan(&n); err != nil {
		w.t.Fatalf("count claims: %v", err)
	}
	return n
}

// TestMergeQueuePump_LiveAdmissionContinuesToMergeOnPass: a held entry whose
// head already passed is admitted (phase passed, merge_queue_verified
// already_passed), the pump CONTINUES to the live-entry branch, and
// merge-on-pass merges it in the same pump through the real D1 ledger.
func TestMergeQueuePump_LiveAdmissionContinuesToMergeOnPass(t *testing.T) {
	w := newMQMWorld(t)
	r := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
	w.mqpHeldIntent(r, "h1", false)

	w.pumpBase(r)

	if got := w.merger.calls(); len(got) != 1 || got[0] != "h1" {
		t.Fatalf("pinned merges = %v, want [h1] in the admitting pump", got)
	}
	w.requireState(r, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	if n := w.claims(r); n != 1 {
		t.Fatalf("merge_dispatch_claims rows = %d, want 1 (the queue claimed before the forge)", n)
	}
	verified := w.rows(r, CategoryMergeQueueVerified)
	if len(verified) != 1 || w.payload(verified[0])["source"] != mergeQueueVerifiedSourceAlreadyPassed {
		t.Fatalf("merge_queue_verified rows = %d, want one already_passed", len(verified))
	}
}

// TestMergeQueuePump_NotRequiredEntryMerges: rejection item 3 at the pump —
// a spec with no verify command reads not_required, which merges like passed.
func TestMergeQueuePump_NotRequiredEntryMerges(t *testing.T) {
	w := newMQMWorld(t)
	r := w.seed("nr", "h1", []byte(mcvNoVerifySpecYAML))
	w.mqpHeldIntent(r, "h1", true)

	w.pumpBase(r)

	w.requireMerges(1)
	w.requireState(r, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	verified := w.rows(r, CategoryMergeQueueVerified)
	if len(verified) != 1 || w.payload(verified[0])["source"] != mergeQueueVerifiedSourceNotRequired {
		t.Fatalf("merge_queue_verified rows = %d, want one not_required", len(verified))
	}
}

// TestMergeQueuePump_NoIntentEntryHoldsTheLine: a passed live entry WITHOUT
// merge intent is not merged by the pump (slice-1 behavior kept).
func TestMergeQueuePump_NoIntentEntryHoldsTheLine(t *testing.T) {
	w := newMQMWorld(t)
	r := w.seed("ni", "h1", []byte(mcvUndelegatedSpecYAML))
	w.mqeRebased(r, "pre-h1", "h1", "h1", false)
	w.mqePassed(r, "h1")
	w.enqueueDirect(r, "h1")

	w.pumpBase(r)

	w.requireMerges(0)
	if e := w.storeEntry(r); e.State != mergequeue.StateLive || e.Phase != mergequeue.PhasePassed {
		t.Fatalf("entry = %s/%s, want live/passed holding the line", e.State, e.Phase)
	}
}

// TestMergeQueuePump_TransientAdmissionFailureDoesNotLoop is rejection item 5:
// the behind probe answers 500, so the admission Unadmits; the pump STOPS
// (one AdmitNext, one Unadmit) instead of re-admitting the same entry until
// mergeCandidateQueueMaxAdmissions.
func TestMergeQueuePump_TransientAdmissionFailureDoesNotLoop(t *testing.T) {
	w := newMQMWorld(t)
	r := w.seed("t", "h1", []byte(mcvUndelegatedSpecYAML))
	w.mqpHeldIntent(r, "h1", false)
	w.gh.set(func() { w.gh.compareStatus["h1"] = http.StatusInternalServerError })
	fs := &mcqFaultStore{Store: w.store}
	w.srv.cfg.MergeQueue = fs

	w.pumpBase(r)

	if a, u := fs.called("AdmitNext"), fs.called("Unadmit"); a != 1 || u != 1 {
		t.Fatalf("AdmitNext = %d, Unadmit = %d; want 1 and 1 (an Unadmit stops the pump): %v", a, u, fs.names())
	}
	if e := w.storeEntry(r); e.State != mergequeue.StateHeld {
		t.Fatalf("entry = %s, want held at its place", e.State)
	}
	w.requireMerges(0)
}

// TestMergeQueuePump_StaleReadvanceUnadmitConflicts is rejection item 1: a
// live, passed, intent-bearing entry at h1, epoch N, whose base moved. Pump
// B decides first and parks in its readvance Unadmit(N); pump A then runs to
// completion (Unadmit(N), re-admission at N+1, the advance to h2, the pass
// start). Released, B's stale Unadmit(N) is a CAS conflict and B stops: the
// entry stays live, verifying at N+1, with ONE advance and ONE admission.
func TestMergeQueuePump_StaleReadvanceUnadmitConflicts(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("race", "h1", mcvUndelegatedSpecYAML)
	epoch := req.Entry.AdmissionEpoch
	w.behind(r, "h1", "h2")

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var bErr error
	fs := &mcqFaultStore{Store: w.store}
	fs.unadmitAt = func(ctx context.Context, id uuid.UUID, ep int64) (mergequeue.Entry, error) {
		first := false
		once.Do(func() { first = true })
		if first {
			close(entered)
			<-release
			e, err := w.store.Unadmit(ctx, id, ep)
			bErr = err
			return e, err
		}
		return w.store.Unadmit(ctx, id, ep)
	}
	w.srv.cfg.MergeQueue = fs

	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		w.srv.pumpMergeCandidateQueue(context.Background(), req.Entry.Base())
	}()
	select {
	case <-entered:
	case <-time.After(timescale.D(30 * time.Second)):
		t.Fatal("pump B never reached its readvance Unadmit")
	}

	w.srv.pumpMergeCandidateQueue(context.Background(), req.Entry.Base())
	close(release)
	select {
	case <-bDone:
	case <-time.After(timescale.D(30 * time.Second)):
		t.Fatal("pump B did not return after its stale Unadmit")
	}

	if !errors.Is(bErr, mergequeue.ErrStateConflict) {
		t.Fatalf("B's stale Unadmit(%d) err = %v, want ErrStateConflict", epoch, bErr)
	}
	e := w.storeEntry(r)
	if e.State != mergequeue.StateLive || e.Phase != mergequeue.PhaseVerifying || e.AdmissionEpoch != epoch+1 || e.AnchoredHeadSHA != "h2" {
		t.Fatalf("entry = %s/%s epoch %d head %s; want live/verifying at epoch %d on h2", e.State, e.Phase, e.AdmissionEpoch, e.AnchoredHeadSHA, epoch+1)
	}
	if n := len(w.gh.mergeCalls()); n != 1 {
		t.Fatalf("POST /merges = %d, want 1 (one re-admission advanced)", n)
	}
	if n := len(w.rows(r, CategoryMergeCandidateQueueAdmitted)); n != 1 {
		t.Fatalf("merge_candidate_queue_admitted rows = %d, want 1 (B never re-admitted)", n)
	}
	adv := w.rows(r, CategoryMergeQueueAdvanced)
	if len(adv) != 1 {
		t.Fatalf("merge_queue_advanced rows = %d, want 1", len(adv))
	}
	if p := w.payload(adv[0]); p["prior_head_sha"] != "h1" || p["new_head_sha"] != "h2" || p["clean"] != true ||
		int64(p["admission_epoch"].(float64)) != epoch+1 {
		t.Fatalf("merge_queue_advanced payload = %v", p)
	}
	w.requireMerges(0)
}

// TestMergeQueuePump_ReadvanceCapped: a base that moves past every head
// re-advances the entry at most mergeQueueMaxReadvancesPerPump times in one
// pump; the forge merge is never called.
func TestMergeQueuePump_ReadvanceCapped(t *testing.T) {
	w := newMQMWorld(t)
	r := w.seed("cap", "h1", []byte(mcvNoVerifySpecYAML))
	req := w.live(r, "h1")
	w.behind(r, "h1", "h2")
	w.gh.set(func() { w.gh.behind["h2"] = []string{"base-moving"} })
	fs := &mcqFaultStore{Store: w.store}
	w.srv.cfg.MergeQueue = fs

	w.srv.pumpMergeCandidateQueue(context.Background(), req.Entry.Base())

	if n := fs.called("Unadmit"); n != mergeQueueMaxReadvancesPerPump {
		t.Fatalf("readvance Unadmits = %d, want %d (the per-pump bound): %v", n, mergeQueueMaxReadvancesPerPump, fs.names())
	}
	if n := len(w.gh.mergeCalls()); n != mergeQueueMaxReadvancesPerPump {
		t.Fatalf("advances = %d, want %d", n, mergeQueueMaxReadvancesPerPump)
	}
	w.requireMerges(0)
	if e := w.storeEntry(r); e.State != mergequeue.StateLive || e.Phase != mergequeue.PhasePassed {
		t.Fatalf("entry = %s/%s, want live/passed (the next pump re-decides)", e.State, e.Phase)
	}
}

// TestMergeQueuePump_StaleDecisionSettleConflicts: a settle outcome decided
// against an earlier admission epoch is a SettleAt CAS conflict: the pump
// stops and the committed entry is untouched.
func TestMergeQueuePump_StaleDecisionSettleConflicts(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("stale", "h1", mcvUndelegatedSpecYAML)
	stale := req.Entry
	stale.AdmissionEpoch--
	readvances := 0
	cont := w.srv.applyMergeOnPassOutcome(context.Background(), stale, mergeOnPassOutcome{
		Kind: mergeOnPassSettle, State: mergequeue.StateDropped, Reason: mergeCandidateQueueReasonPRClosed}, &readvances)
	if cont {
		t.Fatal("a stale settle continued the pump")
	}
	if e := w.storeEntry(r); e.State != mergequeue.StateLive || e.AdmissionEpoch != req.Entry.AdmissionEpoch {
		t.Fatalf("entry = %s epoch %d, want untouched live at %d", e.State, e.AdmissionEpoch, req.Entry.AdmissionEpoch)
	}
	if !w.srv.applyMergeOnPassOutcome(context.Background(), req.Entry, mergeOnPassOutcome{
		Kind: mergeOnPassSettle, State: mergequeue.StateDropped, Reason: mergeCandidateQueueReasonPRClosed}, &readvances) {
		t.Fatal("a current-epoch settle did not continue the pump")
	}
	w.requireState(r, mergequeue.StateDropped, mergeCandidateQueueReasonPRClosed)
}

// TestMergeQueuePump_HeldOutcomeAndMergedSettle pins the other two outcome
// arms: held stops with nothing written; merged with Settled=false settles
// the entry merged here and continues.
func TestMergeQueuePump_HeldOutcomeAndMergedSettle(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("arms", "h1", mcvUndelegatedSpecYAML)
	readvances := 0
	if w.srv.applyMergeOnPassOutcome(context.Background(), req.Entry, mergeOnPassOutcome{Kind: mergeOnPassHeld}, &readvances) {
		t.Fatal("a held outcome continued the pump")
	}
	if e := w.storeEntry(r); e.State != mergequeue.StateLive {
		t.Fatalf("entry after held = %s, want live", e.State)
	}
	if !w.srv.applyMergeOnPassOutcome(context.Background(), req.Entry, mergeOnPassOutcome{Kind: mergeOnPassMerged}, &readvances) {
		t.Fatal("a merged outcome did not continue the pump")
	}
	w.requireState(r, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
}

// TestMergeQueuePump_VerdictHookPumpsDetached: a passed verdict for a live,
// verifying, intent-bearing entry sets phase passed, writes
// merge_queue_verified (source pass) and pumps the base DETACHED, which
// merges the entry.
func TestMergeQueuePump_VerdictHookPumpsDetached(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("hook", "h1", mcvUndelegatedSpecYAML)
	w.exec(`UPDATE merge_candidate_queue_entries SET phase = 'verifying' WHERE id = $1`, req.Entry.ID)

	w.srv.mergeCandidateQueueOnVerdict(context.Background(), r.id, "h1", mergeCandidateResultPassed)
	w.srv.bgMergeQueuePump.Wait()

	w.requireMerges(1)
	w.requireState(r, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	verified := w.rows(r, CategoryMergeQueueVerified)
	if len(verified) != 1 || w.payload(verified[0])["source"] != mergeQueueVerifiedSourcePass {
		t.Fatalf("merge_queue_verified rows = %d, want one pass", len(verified))
	}
}

// TestMergeQueueDispatchLedger_Adapter: the store's ErrAlreadyDispatched is
// claimed=false with a nil error; any other error is returned.
func TestMergeQueueDispatchLedger_Adapter(t *testing.T) {
	w := newMQMWorld(t)
	r := w.seed("led", "h1", []byte(mcvUndelegatedSpecYAML))
	l := w.srv.mergeDispatchLedger()
	ctx := context.Background()
	first, ok, err := l.ClaimMergeDispatch(ctx, r.id, mergeDispatchPathQueue, "h1")
	if !ok || err != nil {
		t.Fatalf("first claim = %v, %v; want claimed", ok, err)
	}
	if existing, ok, err := l.ClaimMergeDispatch(ctx, r.id, mergeDispatchPathQueue, "h1"); ok || err != nil || existing.Path != mergeDispatchPathQueue {
		t.Fatalf("second claim = %+v, %v, %v; want the existing claim, false, nil (already dispatched)", existing, ok, err)
	}
	if _, ok, err := l.ClaimMergeDispatch(ctx, r.id, "bogus", "h1"); ok || !errors.Is(err, mergequeue.ErrInvalidRequest) {
		t.Fatalf("bad-path claim = %v, %v; want ErrInvalidRequest", ok, err)
	}
	if err := l.MarkMergeDispatchFailed(ctx, r.id, "boom"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	// Ruling R1 test 6: a release is a CAS on the exact dispatched claim, so
	// it never deletes a dispatch_failed claim or another holder's claim.
	if err := l.ReleaseMergeDispatch(ctx, first); !errors.Is(err, mergequeue.ErrStateConflict) {
		t.Fatalf("release of a dispatch_failed claim = %v, want ErrStateConflict", err)
	}
	other := w.seed("led2", "h2", []byte(mcvUndelegatedSpecYAML))
	mine, _, err := l.ClaimMergeDispatch(ctx, other.id, mergequeue.DispatchPathInline, "h2")
	if err != nil {
		t.Fatalf("claim other: %v", err)
	}
	for name, stale := range map[string]mergequeue.DispatchClaim{
		"other head":       {RunID: other.id, State: mine.State, Path: mine.Path, HeadSHA: "h9", ClaimedAt: mine.ClaimedAt},
		"other path":       {RunID: other.id, State: mine.State, Path: mergeDispatchPathQueue, HeadSHA: mine.HeadSHA, ClaimedAt: mine.ClaimedAt},
		"other claimed_at": {RunID: other.id, State: mine.State, Path: mine.Path, HeadSHA: mine.HeadSHA, ClaimedAt: mine.ClaimedAt.Add(-time.Second)},
	} {
		if err := l.ReleaseMergeDispatch(ctx, stale); !errors.Is(err, mergequeue.ErrStateConflict) {
			t.Fatalf("release with %s = %v, want ErrStateConflict", name, err)
		}
	}
	if c, found, err := w.store.MergeDispatch(ctx, other.id); err != nil || !found || c.State != mergequeue.DispatchDispatched {
		t.Fatalf("other's claim after stale releases = %+v found=%v err=%v; want it intact", c, found, err)
	}
	if err := l.ReleaseMergeDispatch(ctx, mine); err != nil {
		t.Fatalf("release of the exact claim: %v", err)
	}
	if _, found, err := w.store.MergeDispatch(ctx, other.id); err != nil || found {
		t.Fatalf("claim after its exact release: found=%v err=%v; want deleted", found, err)
	}
	if (&Server{}).mergeDispatchLedger() != nil {
		t.Fatal("an unwired queue must yield a nil ledger")
	}
}

// TestMCQFaultStore_NilEmbeddedReturnsNamedError: a pure fake with no
// embedded store and no override answers errMCQNilStore, never panics.
func TestMCQFaultStore_NilEmbeddedReturnsNamedError(t *testing.T) {
	fs := &mcqFaultStore{}
	ctx := context.Background()
	id := uuid.New()
	b := mergequeue.Base{Repo: "x/y", BaseRef: mcqBase}
	for name, call := range map[string]func() error{
		"Enqueue":         func() error { _, err := fs.Enqueue(ctx, mergequeue.EnqueueRequest{}); return err },
		"AdmitNext":       func() error { _, err := fs.AdmitNext(ctx, b); return err },
		"Unadmit":         func() error { _, err := fs.Unadmit(ctx, id, 1); return err },
		"SetPhase":        func() error { _, err := fs.SetPhase(ctx, id, 1, mergequeue.PhasePassed); return err },
		"Reanchor":        func() error { _, err := fs.Reanchor(ctx, id, "h", ""); return err },
		"ReanchorAt":      func() error { _, err := fs.ReanchorAt(ctx, id, 1, "h", ""); return err },
		"Settle":          func() error { _, err := fs.Settle(ctx, id, mergequeue.StateMerged, ""); return err },
		"SettleAt":        func() error { _, err := fs.SettleAt(ctx, id, 1, mergequeue.StateMerged, ""); return err },
		"ActiveForRun":    func() error { _, _, err := fs.ActiveForRun(ctx, id); return err },
		"ActiveBases":     func() error { _, err := fs.ActiveBases(ctx); return err },
		"ListForBase":     func() error { _, err := fs.ListForBase(ctx, b); return err },
		"MarkMergeIntent": func() error { _, err := fs.MarkMergeIntent(ctx, id, nil); return err },
		"RecentLineTurns": func() error { _, err := fs.RecentLineTurns(ctx, b, 1); return err },
		"ClaimMergeDispatch": func() error {
			_, err := fs.ClaimMergeDispatch(ctx, id, mergeDispatchPathQueue, "h")
			return err
		},
		"MarkMergeDispatchFailed": func() error { return fs.MarkMergeDispatchFailed(ctx, id, "x") },
		"ReleaseMergeDispatch":    func() error { return fs.ReleaseMergeDispatch(ctx, mergequeue.DispatchClaim{RunID: id}) },
		"MergeDispatch":           func() error { _, _, err := fs.MergeDispatch(ctx, id); return err },
	} {
		if err := call(); !errors.Is(err, errMCQNilStore) {
			t.Errorf("%s err = %v, want errMCQNilStore", name, err)
		}
	}
}

// TestMCQGitHub_PinnedPutMergesAndMovesBase pins the mcq fake's head-pinned
// merge: a PUT at the head records sha and method, merges the PR and makes
// every other open PR read behind; a PUT at another sha is GitHub's 409; a
// configured status answers as is.
func TestMCQGitHub_PinnedPutMergesAndMovesBase(t *testing.T) {
	f := newMCQFixture(t)
	a := f.seed("a", "a1", []byte(mcvUndelegatedSpecYAML))
	f.seed("b", "b1", []byte(mcvUndelegatedSpecYAML))
	c := f.seed("c", "c1", []byte(mcvUndelegatedSpecYAML))
	gh := f.gh.client(t)
	ctx := context.Background()
	repo, err := parseRepoOwnerName("x/y")
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	scope := forge.FromGitHubInstallationID(99)
	if err := gh.MergePullRequestAtHead(ctx, scope, repo, a.pr, githubclient.MergeMethodSquash, "stale"); err == nil {
		t.Fatal("a PUT at a non-head sha merged; want the 409 head-mismatch")
	}
	f.gh.set(func() { f.gh.putStatus[c.pr] = http.StatusMethodNotAllowed })
	if err := gh.MergePullRequestAtHead(ctx, scope, repo, c.pr, githubclient.MergeMethodSquash, "c1"); err == nil {
		t.Fatal("a configured 405 merged")
	}
	if err := gh.MergePullRequestAtHead(ctx, scope, repo, a.pr, githubclient.MergeMethodSquash, "a1"); err != nil {
		t.Fatalf("pinned PUT at head: %v", err)
	}
	puts := f.gh.putCalls()
	if len(puts) != 3 || puts[2].SHA != "a1" || puts[2].Method != "squash" || puts[2].PR != a.pr {
		t.Fatalf("puts = %+v, want the third at a1/squash on PR %d", puts, a.pr)
	}
	f.gh.mu.Lock()
	merged, behindB := f.gh.prs[a.pr].merged, len(f.gh.behind["b1"])
	f.gh.mu.Unlock()
	if !merged || behindB != 1 {
		t.Fatalf("after the merge: a merged=%v, b behind=%d; want true, 1 (the base moved)", merged, behindB)
	}
}

// realLedger rewires the world onto the REAL postgres D1 ledger.
func (w *mqmWorld) realLedger(req mergeOnPassRequest) mergeOnPassRequest {
	req.Ledger = w.srv.mergeDispatchLedger()
	return req
}

func (w *mqmWorld) requireClaim(r *mcqRun, want mergequeue.DispatchState) {
	w.t.Helper()
	c, found, err := w.store.MergeDispatch(context.Background(), r.id)
	if err != nil {
		w.t.Fatalf("read claim: %v", err)
	}
	if want == "" {
		if found {
			w.t.Fatalf("claim = %+v, want none (released)", c)
		}
		return
	}
	if !found || c.State != want {
		w.t.Fatalf("claim = %+v found=%v, want state %s", c, found, want)
	}
}

func (w *mqmWorld) settledOutcomes(r *mcqRun) []any {
	var out []any
	for _, row := range w.rows(r, categoryMergeDispatchSettled) {
		out = append(out, w.payload(row)["outcome"])
	}
	return out
}

// Ruling R1 test 2: a pinned-merge 409 head mismatch releases the claim, so
// the re-advanced entry's next merge-on-pass claims again and merges.
func TestMergeQueueMergeOnPass_RealLedger_HeadMismatchThenMerges(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("rl409", "h1", mcvUndelegatedSpecYAML)
	req = w.realLedger(req)
	w.merger.err = fmt.Errorf("pinned: %w", forge.ErrMergeConflict)
	requireOutcome(t, w.decide(req), mergeOnPassReadvance, mergeOnPassReasonForgeHeadMismatch)
	w.requireClaim(r, "")
	if got := w.settledOutcomes(r); len(got) != 1 || got[0] != string(mergeDispatchRelease) {
		t.Fatalf("merge_dispatch_settled outcomes = %v, want [released]", got)
	}
	w.merger.err = nil
	w.exec(`UPDATE merge_candidate_queue_entries SET phase = 'passed' WHERE run_id = $1 AND state = 'live'`, r.id)
	req.Entry = w.active(r)
	requireOutcome(t, w.decide(req), mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
	w.requireMerges(2)
}

// Ruling R1 tests 4 and 5: a 5xx is uncertain (dispatch_failed); the next
// merge-on-pass holds already-dispatched while the PR is unmerged, then
// settles merged by reconciliation once the forge reports it merged.
func TestMergeQueueMergeOnPass_RealLedger_UncertainThenReconciles(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("rl5xx", "h1", mcvUndelegatedSpecYAML)
	req = w.realLedger(req)
	w.merger.err = errors.New("github: 502 bad gateway")
	requireOutcome(t, w.decide(req), mergeOnPassHeld, mergeOnPassReasonTransient)
	w.requireClaim(r, mergequeue.DispatchFailed)
	if got := w.settledOutcomes(r); len(got) != 1 || got[0] != string(mergeDispatchUncertain) {
		t.Fatalf("merge_dispatch_settled outcomes = %v, want [dispatch_failed]", got)
	}
	w.merger.err = nil
	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassHeld, mergeOnPassReasonAlreadyDispatched)
	if !strings.Contains(out.Detail, string(mergequeue.DispatchFailed)) {
		t.Fatalf("held detail %q does not surface the dispatch_failed claim", out.Detail)
	}
	w.requireMerges(1)
	w.requireClaim(r, mergequeue.DispatchFailed)
	n := parsePRNumberFromURL(w.runRow(r).PullRequestURL)
	w.gh.set(func() { w.gh.prs[n].merged, w.gh.prs[n].state = true, "closed" })
	requireOutcome(t, w.decide(req), mergeOnPassSettle, mergeCandidateQueueReasonPRMerged)
	w.requireMerges(1)
}

// Ruling R1 test 3: a dispatch whose context is cancelled mid-call records
// dispatch_failed on a non-cancellable context, never leaving 'dispatched'.
func TestMergeQueueMergeOnPass_RealLedger_CancelledCtxLeavesDispatchFailed(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("rlctx", "h1", mcvUndelegatedSpecYAML)
	req = w.realLedger(req)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.srv.cfg.GateMerger = mqmCancelMerger{w.merger, cancel}
	_ = w.srv.mergeQueueMergeOnPass(ctx, req)
	w.requireClaim(r, mergequeue.DispatchFailed)
}

// mqmCancelMerger cancels the caller's context, then fails with its error.
type mqmCancelMerger struct {
	*mqmMerger
	cancel context.CancelFunc
}

func (m mqmCancelMerger) MergePullRequestAtHead(ctx context.Context, _ *run.Run, _ string) error {
	m.cancel()
	<-ctx.Done()
	return ctx.Err()
}

// Ruling R1 test 7: the one classification of a failed post-claim dispatch.
func TestClassifyMergeDispatchOutcome(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want mergeDispatchLedgerOutcome
	}{
		{"checks pending (our pre-call refusal)", fmt.Errorf("x: %w", forge.ErrPullRequestUnstableStatus), mergeDispatchRelease},
		{"405 not mergeable", fmt.Errorf("x: %w", forge.ErrPullRequestNotMergeable), mergeDispatchRelease},
		{"409 head mismatch", fmt.Errorf("x: %w", forge.ErrMergeConflict), mergeDispatchRelease},
		{"422 validation", fmt.Errorf("x: %w", forge.ErrValidation), mergeDispatchRelease},
		{"403 may be a rate limit", fmt.Errorf("x: %w", forge.ErrForbidden), mergeDispatchUncertain},
		{"404", fmt.Errorf("x: %w", forge.ErrNotFound), mergeDispatchUncertain},
		{"5xx / transport", errors.New("502 bad gateway"), mergeDispatchUncertain},
		{"timeout", fmt.Errorf("x: %w", context.DeadlineExceeded), mergeDispatchUncertain},
		{"cancel", fmt.Errorf("x: %w", context.Canceled), mergeDispatchUncertain},
		{"cancel wrapping a 409", fmt.Errorf("%w: %w", context.Canceled, forge.ErrMergeConflict), mergeDispatchUncertain},
		{"nil", nil, mergeDispatchUncertain},
	} {
		if got := classifyMergeDispatchOutcome(tc.err); got != tc.want {
			t.Errorf("%s: classify = %s, want %s", tc.name, got, tc.want)
		}
	}
}
