package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// ADR-093 slice 3 (D4, E87.6 / #4297): the automatic re-entry sweep. Each
// fixture is otherwise FULLY eligible (an ejected verify_failed entry with
// merge intent, a routed fix-up, a fixup_pushed row after the ejection, the
// PR head moved, eligible at the new head, settled past the first schedule
// point), so the gate under test is the only refusal.

// rentInterval is the sweep interval the tests judge yieldReprobeDue on.
const rentInterval = time.Minute

// rentSeed seeds r on spec at h0 with a merge-intent entry admitted live,
// ejects it verify_failed with fixup_routed = routed (its merge_queue_ejected
// row written by the production writer), and backdates the settle by age.
// It returns the ejected entry.
func (w *mqmWorld) rentSeed(r *mcqRun, routed bool, age time.Duration) mergequeue.Entry {
	w.t.Helper()
	ctx := context.Background()
	anchor := w.mqeIntent(r, "h0")
	res, err := w.store.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: r.id, BaseRef: mcqBase, HeadSHA: "h0",
		MergeIntent: true, IntentSequence: anchor.Sequence})
	if err != nil {
		w.t.Fatalf("enqueue: %v", err)
	}
	if adm, err := w.store.AdmitNext(ctx, res.Entry.Base()); err != nil || !adm.Admitted || adm.Entry.RunID != r.id {
		w.t.Fatalf("admit: %+v %v", adm, err)
	}
	st, err := w.store.Settle(ctx, res.Entry.ID, mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed)
	if err != nil || !st.Settled {
		w.t.Fatalf("settle: %+v %v", st, err)
	}
	w.srv.writeMergeQueueEjected(ctx, st.Entry, mergeQueueEjection{FixupRouted: &routed})
	w.exec(`UPDATE merge_candidate_queue_entries SET settled_at = settled_at - make_interval(secs => $1) WHERE id = $2`,
		age.Seconds(), res.Entry.ID)
	return st.Entry
}

// rentPush records a fixup_pushed row (after the ejection) and moves the PR
// head to head, as a delegated fix-up's push does.
func (w *mqmWorld) rentPush(r *mcqRun, head string) *audit.Entry {
	w.t.Helper()
	e := w.mqeAppend(r, mergeQueueFixupPushedCategory, map[string]any{"head_sha": head})
	w.gh.set(func() { w.gh.prs[r.pr].head = head })
	return e
}

// rentBlock admits a blocker run LIVE on the base, so the detached pump a
// re-entry starts leaves the re-entered row held (one live entry per base).
func (w *mqmWorld) rentBlock() {
	w.t.Helper()
	z := w.seed("z", "z0", []byte(mcvUndelegatedSpecYAML))
	w.enqueueDirect(z, "z0")
	if adm, err := w.store.AdmitNext(context.Background(), mergequeue.Base{Repo: "x/y", BaseRef: mcqBase}); err != nil || !adm.Admitted || adm.Entry.RunID != z.id {
		w.t.Fatalf("admit blocker: %+v %v", adm, err)
	}
}

// rentReady seeds the fully eligible candidate on spec: ejected routed, past
// the first schedule point, the fix-up pushed at h1, behind a live blocker.
func (w *mqmWorld) rentReady(name, spec string) (*mcqRun, mergequeue.Entry) {
	w.t.Helper()
	r := w.seed(name, "h0", []byte(spec))
	e := w.rentSeed(r, true, 10*time.Minute)
	w.rentPush(r, "h1")
	w.rentBlock()
	return r, e
}

// sweepAged ages ej's settle by d (time passing) and sweeps with since = d.
func (w *mqmWorld) sweepAged(ej mergequeue.Entry, d time.Duration) {
	w.t.Helper()
	w.exec(`UPDATE merge_candidate_queue_entries SET settled_at = settled_at - make_interval(secs => $1) WHERE id = $2`,
		d.Seconds(), ej.ID)
	w.sweep(d)
}

// sweep runs one re-entry sweep with no keyset pager (the first page) and
// waits for any detached pump it started.
func (w *mqmWorld) sweep(since time.Duration) int {
	w.t.Helper()
	return w.sweepPage(since, nil)
}

// sweepPage runs one re-entry sweep carrying page between ticks and waits for
// any detached pump it started.
func (w *mqmWorld) sweepPage(since time.Duration, page *mergeQueueReentryPager) int {
	w.t.Helper()
	n := w.srv.sweepMergeQueueReentries(context.Background(), rentInterval, since, page)
	w.srv.bgMergeQueuePump.Wait()
	return n
}

// reentryRows returns r's merge_queue_entered rows that are re-entries.
func (w *mqmWorld) reentryRows(r *mcqRun) []*audit.Entry {
	w.t.Helper()
	var out []*audit.Entry
	for _, e := range w.rows(r, CategoryMergeQueueEntered) {
		if w.payload(e)["reentry"] == true {
			out = append(out, e)
		}
	}
	return out
}

// requireNotReentered asserts no re-entry row and no active entry for r.
func (w *mqmWorld) requireNotReentered(r *mcqRun) {
	w.t.Helper()
	if rows := w.reentryRows(r); len(rows) != 0 {
		w.t.Fatalf("re-entry merge_queue_entered rows = %d, want 0", len(rows))
	}
	if _, found, err := w.store.ActiveForRun(context.Background(), r.id); err != nil || found {
		w.t.Fatalf("active entry found=%v err=%v, want none", found, err)
	}
}

// requireReentered asserts exactly one re-entry row, actor SYSTEM, naming the
// ejected entry and the original intent, and an active held entry at head
// whose IntentSequence is that row's.
func (w *mqmWorld) requireReentered(r *mcqRun, ej mergequeue.Entry, head string) (mergequeue.Status, map[string]any) {
	w.t.Helper()
	rows := w.reentryRows(r)
	if len(rows) != 1 {
		w.t.Fatalf("re-entry merge_queue_entered rows = %d, want exactly 1", len(rows))
	}
	row := rows[0]
	if row.ActorKind == nil || *row.ActorKind != audit.ActorSystem || row.ActorSubject == nil || *row.ActorSubject != mergeCandidateSystemSubject {
		w.t.Fatalf("re-entry actor = %v/%v, want system %s", row.ActorKind, row.ActorSubject, mergeCandidateSystemSubject)
	}
	verdict := ""
	for _, e := range w.rows(r, CategoryMergeQueueEntered) {
		if e.Sequence == ej.MergeIntentSequence {
			verdict, _ = w.payload(e)["verdict"].(string)
		}
	}
	p := w.payload(row)
	if verdict == "" || p["reentry_basis"] != mergeQueueReentryBasisDelegatedFixup || p["reentry_of_entry_id"] != ej.ID.String() ||
		p["original_intent_sequence"] != float64(ej.MergeIntentSequence) || p["verdict"] != verdict ||
		p["anchored_head_sha"] != head || p["base_ref"] != mcqBase {
		w.t.Fatalf("re-entry row = %v", p)
	}
	st, found, err := w.store.ActiveForRun(context.Background(), r.id)
	if err != nil || !found {
		w.t.Fatalf("active entry found=%v err=%v, want the re-entered row", found, err)
	}
	if st.Entry.ID == ej.ID || st.Entry.MergeIntentAt == nil || st.Entry.MergeIntentSequence != row.Sequence ||
		st.Entry.AnchoredHeadSHA != head || st.Entry.OrderKey != nil {
		w.t.Fatalf("re-entered entry = %+v, want a fresh intent row at %s naming sequence %d", st.Entry, head, row.Sequence)
	}
	return st, p
}

// Happy path: a routed, pushed, eligible candidate re-enters with merge intent
// at the TAIL, behind every entry already held, under the SYSTEM actor.
func TestReentry_DelegatedFixupPassedReentersAtTail(t *testing.T) {
	w := newMQMWorld(t)
	r, ej := w.rentReady("a", mcvDelegatedV0SpecYAML)
	b := w.seed("b", "b0", []byte(mcvDelegatedV0SpecYAML))
	c := w.seed("c", "c0", []byte(mcvDelegatedV0SpecYAML))
	w.enqueueDirect(b, "b0")
	w.enqueueDirect(c, "c0")
	if n := w.sweep(0); n != 1 {
		t.Fatalf("re-entered = %d, want 1", n)
	}
	st, _ := w.requireReentered(r, ej, "h1")
	es, err := w.store.ListForBase(context.Background(), st.Entry.Base())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(es) != 4 || es[2].RunID != c.id || es[3].RunID != r.id {
		t.Fatalf("queue order = %v, want the re-entered run last, behind b and c", es)
	}
	if st.Position != 3 {
		t.Fatalf("position = %d, want 3 (behind held b and c)", st.Position)
	}
	if held := w.rows(r, CategoryMergeCandidateQueueHeld); len(held) == 0 {
		t.Fatal("no merge_candidate_queue_held row for the re-entry")
	}
}

// Gate (a): the ejection routed no fix-up. MECHANISM: delete the fixup_routed
// check and the run re-enters (RED).
func TestReentry_NotDelegatedNoReentry(t *testing.T) {
	w := newMQMWorld(t)
	r := w.seed("a", "h0", []byte(mcvDelegatedV0SpecYAML))
	w.rentSeed(r, false, 10*time.Minute)
	w.rentPush(r, "h1")
	if n := w.sweep(0); n != 0 {
		t.Fatalf("re-entered = %d, want 0", n)
	}
	w.requireNotReentered(r)
}

// Gate (b): the only fixup_pushed row precedes the ejection (an EARLIER
// fix-up). MECHANISM: delete the sequence comparison and it re-enters (RED).
func TestReentry_FixupNotPushedNoReentry(t *testing.T) {
	w := newMQMWorld(t)
	r := w.seed("a", "h0", []byte(mcvDelegatedV0SpecYAML))
	w.mqeAppend(r, mergeQueueFixupPushedCategory, map[string]any{"head_sha": "h0"})
	w.rentSeed(r, true, 10*time.Minute)
	w.gh.set(func() { w.gh.prs[r.pr].head = "h1" })
	if n := w.sweep(0); n != 0 {
		t.Fatalf("re-entered = %d, want 0", n)
	}
	w.requireNotReentered(r)
}

// Gate (c): ineligible at the new head (a blocking concern). MECHANISM:
// delete the eligibility call and it re-enters (RED).
func TestReentry_IneligibleNoReentry(t *testing.T) {
	w := newMQMWorld(t)
	r, _ := w.rentReady("a", mcvDelegatedV0SpecYAML)
	w.raiseConcern(r, "high", "")
	if n := w.sweep(0); n != 0 {
		t.Fatalf("re-entered = %d, want 0", n)
	}
	w.requireNotReentered(r)
}

// Security (a): a gated run whose fix-up push DISMISSED the forge approval is
// not re-entered (the entry variant reads a LIVE approval at the new head);
// the same fixture with a live approval re-enters.
func TestReentry_DismissedApprovalNoReentry(t *testing.T) {
	w := newMQMWorld(t)
	r, ej := w.rentReady("a", mqeSpecYAML)
	w.gh.set(func() { w.gh.reviews[r.pr] = mqeDismissed })
	if n := w.sweep(0); n != 0 {
		t.Fatalf("re-entered = %d with the approval dismissed, want 0", n)
	}
	w.requireNotReentered(r)
	w.gh.set(func() { w.gh.reviews[r.pr] = mqeApprovedBy("alice") })
	if n := w.sweep(0); n != 1 {
		t.Fatalf("re-entered = %d with a live approval, want 1", n)
	}
	w.requireReentered(r, ej, "h1")
}

// Gate (d): undeterminable (a PR read failure) skips, and the candidate is
// retried — never dropped — on a later due tick.
func TestReentry_UndeterminableNoReentry(t *testing.T) {
	w := newMQMWorld(t)
	r, ej := w.rentReady("a", mcvDelegatedV0SpecYAML)
	w.mu.Lock()
	w.prReadFail = true
	w.mu.Unlock()
	if n := w.sweep(0); n != 0 {
		t.Fatalf("re-entered = %d on a PR read failure, want 0", n)
	}
	w.requireNotReentered(r)
	w.mu.Lock()
	w.prReadFail = false
	w.mu.Unlock()
	if n := w.sweep(0); n != 1 {
		t.Fatalf("re-entered = %d once the read recovers, want 1", n)
	}
	w.requireReentered(r, ej, "h1")
}

// Gate (head): a head the fix-up did not move is not re-entered.
// MECHANISM: delete the moved-head comparison and it re-enters at h0 (RED).
func TestReentry_HeadNotMovedNoReentry(t *testing.T) {
	w := newMQMWorld(t)
	r := w.seed("a", "h0", []byte(mcvDelegatedV0SpecYAML))
	w.rentSeed(r, true, 10*time.Minute)
	w.mqeAppend(r, mergeQueueFixupPushedCategory, map[string]any{"head_sha": "h0"})
	if n := w.sweep(0); n != 0 {
		t.Fatalf("re-entered = %d, want 0", n)
	}
	w.requireNotReentered(r)
}

// C-D4: a run with a stage running is not re-entered, and re-enters once the
// stage settles. MECHANISM: delete the live-stage skip and the first sweep
// re-enters (RED).
func TestReentry_LiveStageSkipsUntilSettled(t *testing.T) {
	for _, live := range []run.StageState{run.StageStateRunning, run.StageStateDispatched} {
		t.Run(string(live), func(t *testing.T) {
			w := newMQMWorld(t)
			r, ej := w.rentReady("a", mcvDelegatedV0SpecYAML)
			w.setStage(r.impl.ID, live)
			if n := w.sweep(0); n != 0 {
				t.Fatalf("re-entered = %d with a %s stage, want 0", n, live)
			}
			w.requireNotReentered(r)
			w.setStage(r.impl.ID, run.StageStateSucceeded)
			if n := w.sweep(0); n != 1 {
				t.Fatalf("re-entered = %d once the stage settled, want 1", n)
			}
			w.requireReentered(r, ej, "h1")
		})
	}
}

// C-BO: an approval-only candidate (eligibility fails ONLY on the approval
// leg) is SKIPPED, never enqueued yielded, and re-polled on the
// yieldReprobeDue schedule over its settled age, not on every tick: the PR
// read (the first forge read, taken only for an evaluated candidate) is
// counted across ticks, each tick aging the row by its since. MECHANISM:
// delete the schedule check and every tick evaluates it (11 reads, RED).
func TestReentry_ApprovalOnlyCandidateRepolledOnSchedule(t *testing.T) {
	w := newMQMWorld(t)
	r, ej := w.rentReady("a", mqeSpecYAML)
	if elig := w.srv.mergeQueueEligibility(context.Background(), w.runRow(r), "h1"); len(elig.Reasons) != 1 || elig.Reasons[0].Code != mergeQueueReasonNotApproved {
		t.Fatalf("fixture eligibility = %v, want ONLY not_approved", elig.codes())
	}
	var mu sync.Mutex
	reads := 0
	w.mu.Lock()
	w.onResponse = func(path string) {
		if m := mqmPullPath.FindStringSubmatch(path); m != nil {
			mu.Lock()
			reads++
			mu.Unlock()
		}
	}
	w.mu.Unlock()
	count := func() int { mu.Lock(); defer mu.Unlock(); return reads }

	w.sweep(0) // restart window (0, ~10m]: due
	for i := 0; i < 5; i++ {
		w.sweepAged(ej, rentInterval) // ages ~11m..~15m: no schedule point in any window
	}
	if n := count(); n != 1 {
		t.Fatalf("PR reads over 6 ticks = %d, want 1 (evaluated only when due)", n)
	}
	for i := 0; i < 5; i++ {
		w.sweepAged(ej, rentInterval) // ~16m holds the 16m point; ~17m..~20m none
	}
	if n := count(); n != 2 {
		t.Fatalf("PR reads over 11 ticks = %d, want 2 (re-polled at the 16m point only)", n)
	}
	w.requireNotReentered(r)
	if es := w.entries(r); len(es) != 1 || es[0].State != mergequeue.StateEjected {
		t.Fatalf("entries = %+v, want only the ejected row (never enqueued yielded)", es)
	}
}

// Gate 5, one arm per fixture on an otherwise FULLY eligible candidate: a
// closed PR, a merged PR (state left open, so only the merged check refuses
// it), a PR retargeted to another base, and a terminal run (the candidate
// list captured BEFORE the run went terminal, so the store's terminal
// predicate cannot mask the sweep's own run-state check). MECHANISM: delete
// the arm's check and that fixture re-enters (RED).
func TestReentry_Gate5PRStateAndTerminalRunNoReentry(t *testing.T) {
	cases := []struct {
		name string
		mut  func(w *mqmWorld, r *mcqRun)
	}{
		{"pr closed", func(w *mqmWorld, r *mcqRun) { w.gh.set(func() { w.gh.prs[r.pr].state = "closed" }) }},
		{"pr merged", func(w *mqmWorld, r *mcqRun) { w.gh.set(func() { w.gh.prs[r.pr].merged = true }) }},
		{"pr retargeted", func(w *mqmWorld, r *mcqRun) { w.gh.set(func() { w.gh.prs[r.pr].base = "release" }) }},
		{"run terminal", func(w *mqmWorld, r *mcqRun) {
			cands, err := w.store.ReentryCandidates(context.Background(), nil, 10)
			if err != nil || len(cands) != 1 || cands[0].RunID != r.id {
				w.t.Fatalf("candidates = %v %v, want the one fixture", cands, err)
			}
			w.exec(`UPDATE runs SET state = 'cancelled' WHERE id = $1`, r.id)
			w.srv.cfg.MergeQueue = rentStaleStore{PostgresStore: w.store, cands: cands}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, _ := w.rentReady("a", mcvDelegatedV0SpecYAML)
			tc.mut(w, r)
			if n := w.sweep(0); n != 0 {
				t.Fatalf("re-entered = %d, want 0", n)
			}
			w.requireNotReentered(r)
		})
	}
}

// Gate 5's PR-read refusals (#4705): a run with no installation (NULL or 0),
// a repo that is not owner/name, or no tracked pull request (no URL, or a URL
// naming no /pull/N) is skipped WITHOUT reading the forge. Each arm starts
// from the fully eligible fixture, so the refusal is the only one; gates 1-4
// make no forge call, so any proxied forge response during the sweep means
// the PR read was attempted. The "control" arm proves the counter observes a
// PR read. MECHANISMS: narrowing the installation guard to `== nil` lets the
// zero-installation arm read the PR (counter RED, and the direct call no
// longer refuses); a PR-number guard of `< 0` makes both URL arms GET
// /pulls/0 (counter RED, error text RED); ignoring the parseRepoOwnerName
// error sends an empty RepoRef, so the direct-call error no longer names
// "is not owner/name" (RED); deleting the nil-installation guard
// dereferences nil (panic, RED).
func TestReentry_ReadPRRefusalsSkipWithoutAForgeRead(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		args func(r *mcqRun) []any
		want string
	}{
		{name: "nil installation", sql: `UPDATE runs SET installation_id = NULL WHERE id = $1`, want: "no installation"},
		{name: "zero installation", sql: `UPDATE runs SET installation_id = 0 WHERE id = $1`, want: "no installation"},
		{name: "unparsable repo", sql: `UPDATE runs SET repo = 'x-y' WHERE id = $1`, want: "is not owner/name"},
		{name: "no pull request url", sql: `UPDATE runs SET pull_request_url = NULL WHERE id = $1`, want: "no tracked pull request"},
		{name: "non-pull-request url", sql: `UPDATE runs SET pull_request_url = $2 WHERE id = $1`,
			args: func(r *mcqRun) []any { return []any{fmt.Sprintf("https://github.com/x/y/issues/%d", r.pr)} },
			want: "no tracked pull request"},
	}
	countForgeReads := func(w *mqmWorld) *atomic.Int64 {
		var n atomic.Int64
		w.mu.Lock()
		w.onResponse = func(string) { n.Add(1) }
		w.mu.Unlock()
		return &n
	}
	t.Run("control: an unrefused read reaches the forge", func(t *testing.T) {
		w := newMQMWorld(t)
		r, _ := w.rentReady("a", mcvDelegatedV0SpecYAML)
		reads := countForgeReads(w)
		runRow, err := w.srv.cfg.RunRepo.GetRun(context.Background(), r.id)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if _, err := w.srv.mergeQueueReentryReadPR(context.Background(), runRow); err != nil {
			t.Fatalf("read PR on the eligible fixture: %v", err)
		}
		if reads.Load() == 0 {
			t.Fatal("forge reads = 0 after a PR read: the counter observes nothing")
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, _ := w.rentReady("a", mcvDelegatedV0SpecYAML)
			args := []any{r.id}
			if tc.args != nil {
				args = append(args, tc.args(r)...)
			}
			w.exec(tc.sql, args...)
			reads := countForgeReads(w)
			runRow, err := w.srv.cfg.RunRepo.GetRun(context.Background(), r.id)
			if err != nil {
				t.Fatalf("get run: %v", err)
			}
			if _, err := w.srv.mergeQueueReentryReadPR(context.Background(), runRow); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("mergeQueueReentryReadPR err = %v, want one naming %q", err, tc.want)
			}
			if n := w.sweep(0); n != 0 {
				t.Fatalf("re-entered = %d, want 0", n)
			}
			if n := reads.Load(); n != 0 {
				t.Fatalf("forge reads = %d, want 0 (the refusal precedes any forge request)", n)
			}
			w.requireNotReentered(r)
		})
	}
}

// reentryCommitted reports whether r has at least one re-entry row AND a
// committed active queue entry other than the ejected ej: the re-entry
// appends its merge_queue_entered row BEFORE the Enqueue, so the row alone
// does not prove the entry landed.
func (w *mqmWorld) reentryCommitted(r *mcqRun, ej mergequeue.Entry) bool {
	w.t.Helper()
	if len(w.reentryRows(r)) == 0 {
		return false
	}
	st, found, err := w.store.ActiveForRun(context.Background(), r.id)
	if err != nil {
		w.t.Fatalf("active entry: %v", err)
	}
	return found && st.Entry.ID != ej.ID
}

// TestRunMergeQueueYieldReprobe_ReentersThroughTheTicker drives the
// production loop end to end: its ticker, its process-local clock (the first
// tick's since is 0, the restart window) and its keyset pager re-enter the
// fully eligible candidate with no hand-driven sweep, and it re-enters
// EXACTLY once although the ticker keeps ticking. The poll waits until the
// re-entered entry is COMMITTED (not just its row), because cancelling the
// loop between the row append and the Enqueue would orphan the row.
// MECHANISM: removing the sweepMergeQueueReentries call from the loop leaves
// no re-entry: RED at the scaled deadline.
func TestRunMergeQueueYieldReprobe_ReentersThroughTheTicker(t *testing.T) {
	w := newMQMWorld(t)
	r, ej := w.rentReady("a", mcvDelegatedV0SpecYAML)
	ctx, cancel := context.WithCancel(context.Background())
	go w.srv.RunMergeQueueYieldReprobe(ctx, 5*time.Millisecond)
	deadline := time.Now().Add(timescale.D(10 * time.Second))
	for !w.reentryCommitted(r, ej) {
		if time.Now().After(deadline) {
			cancel()
			w.srv.bgMergeQueuePump.Wait()
			t.Fatal("the reprobe loop re-entered nothing before the deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	w.srv.bgMergeQueuePump.Wait()
	w.requireReentered(r, ej, "h1")
}

// Starvation (#4297): mergeQueueReentryLimit+1 OLDER candidates that never
// pass a gate (no routed fix-up) sit ahead of one routed, pushed, eligible
// candidate. The keyset pager reads past the first full page, so the eligible
// one re-enters within two ticks, and the next short page wraps the cursor.
// MECHANISM: ignore the cursor (read the first page every tick) and the
// eligible candidate is never read (RED).
func TestReentry_PagerReadsPastNeverEligibleCandidates(t *testing.T) {
	w := newMQMWorld(t)
	stale := make([]*mcqRun, 0, mergeQueueReentryLimit+1)
	for i := 0; i <= mergeQueueReentryLimit; i++ {
		o := w.seed(fmt.Sprintf("old%d", i), "o0", []byte(mcvDelegatedV0SpecYAML))
		w.rentSeed(o, false, 20*time.Minute)
		stale = append(stale, o)
	}
	r, ej := w.rentReady("a", mcvDelegatedV0SpecYAML)
	var page mergeQueueReentryPager
	n := w.sweepPage(0, &page)
	if page.after == nil {
		t.Fatal("a full first page left no cursor")
	}
	n += w.sweepPage(rentInterval, &page)
	if n != 1 {
		t.Fatalf("re-entered = %d over two ticks, want 1 (the eligible candidate behind a full page)", n)
	}
	w.requireReentered(r, ej, "h1")
	if page.after != nil {
		t.Fatalf("cursor after a short page = %+v, want nil (wrapped)", page.after)
	}
	for _, o := range stale {
		w.requireNotReentered(o)
	}
}

// The pager's due window: with one page it is exactly the time since the
// previous sweep (C-BO unchanged); across pages it reaches back to the start
// of the previous cycle, so no schedule point falls between two reads of a
// page; a page never read in this process gets the restart window 0. A
// failed read rotates nothing. MECHANISM: return since as every window and
// the multi-page rows are RED.
func TestMergeQueueReentryPager_Window(t *testing.T) {
	settled := time.Unix(1000, 0)
	row := mergequeue.Entry{SettledAt: &settled}
	full, short := []mergequeue.Entry{row, row}, []mergequeue.Entry{row}
	type step struct {
		since  time.Duration
		page   []mergequeue.Entry // nil: the read failed
		window time.Duration
		cursor bool // the page read after a cursor
	}
	for name, steps := range map[string][]step{
		"one page": {
			{0, short, 0, false},
			{60 * time.Second, short, 60 * time.Second, false},
			{45 * time.Second, short, 45 * time.Second, false},
		},
		"two pages": {
			{0, full, 0, false},                                 // cycle 0, page 0
			{60 * time.Second, short, 0, true},                  // page 1 never read: restart window
			{60 * time.Second, full, 120 * time.Second, false},  // cycle 1 page 0: back to cycle 0's start
			{60 * time.Second, short, 180 * time.Second, true},  // page 1: back to cycle 0's start
			{60 * time.Second, nil, 120 * time.Second, false},   // failed read: cycle 2 does not begin
			{60 * time.Second, short, 180 * time.Second, false}, // retried: still back to cycle 1's start
		},
	} {
		t.Run(name, func(t *testing.T) {
			var p mergeQueueReentryPager
			for i, st := range steps {
				after, window := p.tick(st.since)
				if window != st.window || (after != nil) != st.cursor {
					t.Fatalf("step %d: window %v cursor %v, want %v cursor %v", i, window, after != nil, st.window, st.cursor)
				}
				if st.page != nil {
					p.read(st.page, 2)
				}
			}
		})
	}
}

// A candidate younger than one interval is not due (the ejection just ran).
func TestReentry_FreshEjectionNotDue(t *testing.T) {
	w := newMQMWorld(t)
	r := w.seed("a", "h0", []byte(mcvDelegatedV0SpecYAML))
	w.rentSeed(r, true, 0)
	w.rentPush(r, "h1")
	if n := w.sweep(0); n != 0 {
		t.Fatalf("re-entered = %d, want 0 before the first schedule point", n)
	}
	w.requireNotReentered(r)
}

// rentStaleStore is the real store whose ReentryCandidates answers a
// captured (stale) list.
type rentStaleStore struct {
	*mergequeue.PostgresStore
	cands []mergequeue.Entry
}

func (s rentStaleStore) ReentryCandidates(context.Context, *mergequeue.ReentryCursor, int) ([]mergequeue.Entry, error) {
	return s.cands, nil
}

// Gate (e): the candidate read is stale by the time the lock is held — the
// operator's merge_run re-entered first (a newer active row), or a newer row
// already settled. Exactly the operator's row exists, no re-entry row and no
// second write. MECHANISM: delete the in-lock LatestForRun recheck and a
// re-entry row plus a write appear (RED).
func TestReentry_StaleCandidateRecheckedUnderLock(t *testing.T) {
	for _, settleNewer := range []bool{false, true} {
		name := "operator re-entered (active)"
		if settleNewer {
			name = "newer row already settled"
		}
		t.Run(name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, _ := w.rentReady("a", mcvDelegatedV0SpecYAML)
			cands, err := w.store.ReentryCandidates(context.Background(), nil, 10)
			if err != nil || len(cands) != 1 {
				t.Fatalf("candidates = %v %v", cands, err)
			}
			op := w.mqeIntent(r, "h1")
			res, err := w.store.Enqueue(context.Background(), mergequeue.EnqueueRequest{RunID: r.id, BaseRef: mcqBase, HeadSHA: "h1",
				MergeIntent: true, IntentSequence: op.Sequence})
			if err != nil {
				t.Fatalf("operator enqueue: %v", err)
			}
			if settleNewer {
				if _, err := w.store.Settle(context.Background(), res.Entry.ID, mergequeue.StateDropped, mergeCandidateQueueReasonRunTerminal); err != nil {
					t.Fatalf("settle: %v", err)
				}
			}
			w.srv.cfg.MergeQueue = rentStaleStore{PostgresStore: w.store, cands: cands}
			if n := w.sweep(0); n != 0 {
				t.Fatalf("re-entered = %d on a stale candidate, want 0", n)
			}
			if rows := w.reentryRows(r); len(rows) != 0 {
				t.Fatalf("re-entry rows = %d, want 0", len(rows))
			}
			if es := w.entries(r); len(es) != 2 {
				t.Fatalf("entries = %d, want the ejected row and the operator's only", len(es))
			}
		})
	}
}

// Gate (f): two sweeps re-enter once.
func TestReentry_TwoSweepsReenterOnce(t *testing.T) {
	w := newMQMWorld(t)
	r, ej := w.rentReady("a", mcvDelegatedV0SpecYAML)
	if n := w.sweep(0) + w.sweep(0); n != 1 {
		t.Fatalf("re-entered = %d over two sweeps, want 1", n)
	}
	w.requireReentered(r, ej, "h1")
	if es := w.entries(r); len(es) != 2 {
		t.Fatalf("entries = %d, want the ejected row plus one re-entry", len(es))
	}
}

// Gate (g): the merge_queue_entered append fails — record before commit
// writes NO entry (read after the call: committed state). MECHANISM: delete
// the append-error return and Enqueue commits an entry (RED).
func TestReentry_AppendFailureWritesNoEntry(t *testing.T) {
	w := newMQMWorld(t)
	r, _ := w.rentReady("a", mcvDelegatedV0SpecYAML)
	w.audit.failCategory = CategoryMergeQueueEntered
	if n := w.sweep(0); n != 0 {
		t.Fatalf("re-entered = %d, want 0", n)
	}
	w.requireNotReentered(r)
}

// rentFailEnqueue is the real store whose Enqueue fails.
type rentFailEnqueue struct{ *mergequeue.PostgresStore }

func (rentFailEnqueue) Enqueue(context.Context, mergequeue.EnqueueRequest) (mergequeue.EnqueueResult, error) {
	return mergequeue.EnqueueResult{}, errors.New("injected enqueue failure")
}

// Security (d): the append succeeds and the Enqueue fails. No entry exists,
// so nothing can merge from the orphan row; the next sweep re-enters with a
// NEW row, and the entry's IntentSequence names that row, never the orphan.
func TestReentry_EnqueueFailureLeavesOrphanNoEntryNamesIt(t *testing.T) {
	w := newMQMWorld(t)
	r, ej := w.rentReady("a", mcvDelegatedV0SpecYAML)
	w.srv.cfg.MergeQueue = rentFailEnqueue{w.store}
	if n := w.sweep(0); n != 0 {
		t.Fatalf("re-entered = %d, want 0", n)
	}
	orphans := w.reentryRows(r)
	if len(orphans) != 1 {
		t.Fatalf("orphan rows = %d, want 1", len(orphans))
	}
	if _, found, _ := w.store.ActiveForRun(context.Background(), r.id); found {
		t.Fatal("an entry exists after the failed Enqueue")
	}
	w.requireMerges(0)
	w.srv.cfg.MergeQueue = w.store
	if n := w.sweep(0); n != 1 {
		t.Fatalf("re-entered = %d on the retry, want 1", n)
	}
	st, found, err := w.store.ActiveForRun(context.Background(), r.id)
	if err != nil || !found || st.Entry.MergeIntentSequence == orphans[0].Sequence || st.Entry.MergeIntentSequence == 0 {
		t.Fatalf("entry = %+v (found %v, err %v), want an intent sequence naming the NEW row, not orphan %d",
			st.Entry, found, err, orphans[0].Sequence)
	}
	if st.Entry.ID == ej.ID {
		t.Fatal("the ejected row was revived instead of a fresh tail row")
	}
}

// M1: a store without the ReentryReader capability (a nil-embedded fake)
// degrades to nothing, never a panic.
func TestReentry_StoreWithoutCapabilityDegrades(t *testing.T) {
	w := newMQMWorld(t)
	w.srv.cfg.MergeQueue = &mcqFaultStore{}
	if n := w.srv.sweepMergeQueueReentries(context.Background(), rentInterval, 0, nil); n != 0 {
		t.Fatalf("re-entered = %d, want 0", n)
	}
	s := New(Config{Addr: "127.0.0.1:0"})
	if n := s.sweepMergeQueueReentries(context.Background(), rentInterval, 0, nil); n != 0 {
		t.Fatalf("unwired re-entered = %d", n)
	}
}

// The re-entry clock is process-local: 0 before the first sweep, then the
// time since the previous one.
func TestMergeQueueReentryClock(t *testing.T) {
	var c mergeQueueReentryClock
	t0 := time.Unix(1000, 0)
	if d := c.since(t0); d != 0 {
		t.Fatalf("first since = %v, want 0", d)
	}
	if d := c.since(t0.Add(3 * time.Second)); d != 3*time.Second {
		t.Fatalf("second since = %v, want 3s", d)
	}
}

// Gate (intent): the intent is bound to the entry's stored
// MergeIntentSequence — no stored sequence, or none naming a
// merge_queue_entered row, is not re-entered (never the newest row instead).
// MECHANISM: delete the found check and it re-enters with no verdict (RED).
func TestReentry_IntentRowBoundToStoredSequence(t *testing.T) {
	for name, set := range map[string]string{
		"no stored sequence":         `UPDATE merge_candidate_queue_entries SET merge_intent_sequence = NULL WHERE id = $1`,
		"sequence names no such row": `UPDATE merge_candidate_queue_entries SET merge_intent_sequence = 999999 WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, ej := w.rentReady("a", mcvDelegatedV0SpecYAML)
			w.exec(set, ej.ID)
			if n := w.sweep(0); n != 0 {
				t.Fatalf("re-entered = %d, want 0", n)
			}
			w.requireNotReentered(r)
		})
	}
}
