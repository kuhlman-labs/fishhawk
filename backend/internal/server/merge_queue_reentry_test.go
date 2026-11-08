package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/invariantmonitor"
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

// rentProductPush records a product-push row of category on top of base (the
// production base_sha/head_sha field names) and moves the PR head to head, as
// the acceptance scenario push and the conflict-resolution push report do.
func (w *mqmWorld) rentProductPush(r *mcqRun, category, base, head string) *audit.Entry {
	w.t.Helper()
	e := w.mqeAppend(r, category, map[string]any{"base_sha": base, "head_sha": head})
	w.gh.set(func() { w.gh.prs[r.pr].head = head })
	return e
}

// rentBaseMerge records a base merge prior -> merge through the REAL
// branch_rebased writer (the resolved new head IS the merge commit) with
// clean_verified = clean, and moves the PR head to merge.
func (w *mqmWorld) rentBaseMerge(r *mcqRun, prior, merge string, clean bool) {
	w.t.Helper()
	w.srv.writeBranchRebasedAudit(context.Background(), mergeCandidateSystemActor(), r.id, r.pr, r.branch, mcqBase,
		prior, merge, merge, false, "base advanced", "", nil, "base-tip", clean)
	w.gh.set(func() { w.gh.prs[r.pr].head = merge })
}

// requireReentryChain asserts the re-entry row's reentry_head_chain is want
// and its fixup_pushed_sequence names push.
func requireReentryChain(t *testing.T, p map[string]any, push *audit.Entry, want []string) {
	t.Helper()
	raw, _ := p["reentry_head_chain"].([]any)
	got := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		got = append(got, s)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") || len(raw) != len(want) {
		t.Fatalf("reentry_head_chain = %v, want %v", p["reentry_head_chain"], want)
	}
	if p["fixup_pushed_sequence"] != float64(push.Sequence) {
		t.Fatalf("fixup_pushed_sequence = %v, want %d", p["fixup_pushed_sequence"], push.Sequence)
	}
}

// rentReadyPushed is rentReady returning the fix-up's fixup_pushed row too.
func (w *mqmWorld) rentReadyPushed(name string) (*mcqRun, mergequeue.Entry, *audit.Entry) {
	w.t.Helper()
	r := w.seed(name, "h0", []byte(mcvDelegatedV0SpecYAML))
	e := w.rentSeed(r, true, 10*time.Minute)
	push := w.rentPush(r, "h1")
	w.rentBlock()
	return r, e, push
}

// #4703: a commit the PRODUCT authored on top of the fix-up's pushed head h1
// re-enters at that descendant, its re-entry row naming the qualifying chain.
// MECHANISMS: dropping CategoryConflictResolutionPushed from the walk's edge
// decode leaves "conflict-resolution merge" unre-entered (RED); the other
// edges and the chain are pinned the same way by their own case.
func TestReentry_ProductDescendantOfFixupReenters(t *testing.T) {
	cases := []struct {
		name  string
		push  func(w *mqmWorld, r *mcqRun)
		head  string
		chain []string
	}{
		{"acceptance scenario commit", func(w *mqmWorld, r *mcqRun) {
			w.rentProductPush(r, CategoryAcceptanceScenariosPushed, "h1", "a1")
		}, "a1", []string{"h1", "a1"}},
		{"conflict-resolution merge", func(w *mqmWorld, r *mcqRun) {
			w.rentProductPush(r, CategoryConflictResolutionPushed, "h1", "c1")
		}, "c1", []string{"h1", "c1"}},
		{"clean base merge", func(w *mqmWorld, r *mcqRun) {
			w.rentBaseMerge(r, "h1", "m1", true)
		}, "m1", []string{"h1", "m1"}},
		{"acceptance then clean base merge", func(w *mqmWorld, r *mcqRun) {
			w.rentProductPush(r, CategoryAcceptanceScenariosPushed, "h1", "a1")
			w.rentBaseMerge(r, "a1", "m2", true)
		}, "m2", []string{"h1", "a1", "m2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, ej, push := w.rentReadyPushed("a")
			tc.push(w, r)
			if n := w.sweep(0); n != 1 {
				t.Fatalf("re-entered = %d, want 1 at the product descendant %s", n, tc.head)
			}
			_, p := w.requireReentered(r, ej, tc.head)
			requireReentryChain(t, p, push, tc.chain)
		})
	}
}

// requireEjectedNewest asserts ej is still r's newest queue row (skipped,
// never dropped).
func (w *mqmWorld) requireEjectedNewest(r *mcqRun, ej mergequeue.Entry) {
	w.t.Helper()
	latest, found, err := w.store.LatestForRun(context.Background(), r.id)
	if err != nil || !found || latest.ID != ej.ID || latest.State != mergequeue.StateEjected {
		w.t.Fatalf("latest = %+v found=%v err=%v, want the ejected row %s untouched", latest, found, err, ej.ID)
	}
}

// #4703: a PR head that is neither the fix-up's pushed head nor a recorded
// product descendant of it is SKIPPED (left to the operator's
// fishhawk_merge_run), on an otherwise fully eligible candidate, and gate 5
// refuses it BEFORE any merge-intent lock (the in-lock re-check would refuse
// the same head, so the lock count isolates gate 5). MECHANISMS: deleting
// gate 5's `!qualifies` skip takes the lock (count RED, though the in-lock
// re-check still refuses);
// returning a chain for an absent head re-enters "foreign push" (RED);
// accepting an edge whose parent is not qualifying re-enters "product push on
// a foreign base" (RED); dropping isCleanAdvance from the base-merge edge
// re-enters "unverified base merge" (RED, the merge commit IS the new head so
// only clean_verified refuses it); dropping the after-the-push sequence filter
// re-enters "product push before the fix-up push" (RED).
func TestReentry_HeadNotQualifyingNoReentry(t *testing.T) {
	cases := []struct {
		name  string
		setup func(w *mqmWorld, r *mcqRun)
	}{
		{"foreign push", func(w *mqmWorld, r *mcqRun) {
			w.rentPush(r, "h1")
			w.gh.set(func() { w.gh.prs[r.pr].head = "f1" })
		}},
		{"product push on a foreign base", func(w *mqmWorld, r *mcqRun) {
			w.rentPush(r, "h1")
			w.gh.set(func() { w.gh.prs[r.pr].head = "f1" })
			w.rentProductPush(r, CategoryAcceptanceScenariosPushed, "f1", "a1")
		}},
		{"unverified base merge", func(w *mqmWorld, r *mcqRun) {
			w.rentPush(r, "h1")
			w.rentBaseMerge(r, "h1", "m1", false)
		}},
		{"product push before the fix-up push", func(w *mqmWorld, r *mcqRun) {
			w.mqeAppend(r, CategoryAcceptanceScenariosPushed, map[string]any{"base_sha": "h1", "head_sha": "a1"})
			w.rentPush(r, "h1")
			w.gh.set(func() { w.gh.prs[r.pr].head = "a1" })
		}},
		{"operator-vouched commit", func(w *mqmWorld, r *mcqRun) {
			w.rentPush(r, "h1")
			w.mqeAppend(r, CategoryOperatorCommitVouched, map[string]any{lineageVouchedSHAField: "v1"})
			w.gh.set(func() { w.gh.prs[r.pr].head = "v1" })
		}},
		{"fix-up push naming no head", func(w *mqmWorld, r *mcqRun) {
			w.mqeAppend(r, mergeQueueFixupPushedCategory, map[string]any{})
			w.gh.set(func() { w.gh.prs[r.pr].head = "h1" })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r := w.seed("a", "h0", []byte(mcvDelegatedV0SpecYAML))
			ej := w.rentSeed(r, true, 10*time.Minute)
			tc.setup(w, r)
			w.rentBlock()
			locks := w.countLocks()
			if n := w.sweep(0); n != 0 {
				t.Fatalf("re-entered = %d, want 0", n)
			}
			w.requireNotReentered(r)
			w.requireEjectedNewest(r, ej)
			if n := locks.Load(); n != 0 {
				t.Fatalf("merge-intent locks = %d, want 0 (gate 5 refuses before the locked half)", n)
			}
		})
	}
}

// rentListFailAudit fails ListForRunByCategory for one category.
type rentListFailAudit struct {
	*mqmAudit
	fail string
}

func (a *rentListFailAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == a.fail {
		return nil, errors.New("injected list failure")
	}
	return a.mqmAudit.ListForRunByCategory(ctx, runID, category)
}

// #4703: an unreadable product-push category is no re-entry (undeterminable),
// gate 5 skips it before any merge-intent lock, and the next sweep with the
// read restored re-enters, so the skip was the read and never a drop.
func TestReentry_ProductPushRowsUnreadableSkips(t *testing.T) {
	for _, cat := range mergeQueueReentryDescendantCategories {
		t.Run(cat, func(t *testing.T) {
			w := newMQMWorld(t)
			r, ej, _ := w.rentReadyPushed("a")
			w.rentProductPush(r, CategoryAcceptanceScenariosPushed, "h1", "a1")
			w.srv.cfg.AuditRepo = &rentListFailAudit{mqmAudit: w.audit, fail: cat}
			locks := w.countLocks()
			if n := w.sweep(0); n != 0 {
				t.Fatalf("re-entered = %d with %s unreadable, want 0", n, cat)
			}
			w.requireNotReentered(r)
			w.requireEjectedNewest(r, ej)
			if n := locks.Load(); n != 0 {
				t.Fatalf("merge-intent locks = %d, want 0 (gate 5 skips the unreadable read)", n)
			}
			w.srv.cfg.MergeQueue = w.store
			w.srv.cfg.AuditRepo = w.audit
			if n := w.sweep(0); n != 1 {
				t.Fatalf("re-entered = %d once the read recovers, want 1", n)
			}
			w.requireReentered(r, ej, "a1")
		})
	}
}

// rentLockHookStore is the real store whose WithMergeIntentLock counts each
// acquisition and runs hook (when set) INSIDE the lock, once, before fn: a
// push landing between gate 5 and the locked half.
type rentLockHookStore struct {
	*mergequeue.PostgresStore
	hook  func()
	once  *sync.Once
	locks *atomic.Int64
}

func (s rentLockHookStore) WithMergeIntentLock(ctx context.Context, runID uuid.UUID, fn func(context.Context) error) error {
	s.locks.Add(1)
	return s.PostgresStore.WithMergeIntentLock(ctx, runID, func(fctx context.Context) error {
		if s.hook != nil {
			s.once.Do(s.hook)
		}
		return fn(fctx)
	})
}

// countLocks binds a counting lock store (no hook) and returns its counter:
// a gate-5 refusal takes NO merge-intent lock, which isolates gate 5 from the
// in-lock re-check that would otherwise refuse the same head.
func (w *mqmWorld) countLocks() *atomic.Int64 {
	w.t.Helper()
	locks := &atomic.Int64{}
	w.srv.cfg.MergeQueue = rentLockHookStore{PostgresStore: w.store, once: &sync.Once{}, locks: locks}
	return locks
}

// #4703: the locked half re-reads the PR and re-derives the qualifying set.
// Each case changes state INSIDE the lock after gate 5 judged h1 qualifying;
// none writes a re-entry row or an entry, and the ejected row stays. "head
// moved to a qualifying descendant" isolates the head-equality check (a2 IS
// in the re-derived set); "newer fix-up push" isolates the re-derived set
// (the PR head is unchanged); "PR read fails" is undeterminable. The first
// and last re-enter on a follow-up sweep without the hook (skip, not drop).
// MECHANISMS: replacing `repr.HeadSHA != head` with `false` re-enters the
// first at the stale h1 (RED); replacing the re-derived-set condition with
// false re-enters the second (RED); treating the read error as an open PR at
// head re-enters the third (RED).
func TestReentry_UnderLockRecheckNoReentry(t *testing.T) {
	cases := []struct {
		name  string
		hook  func(w *mqmWorld, r *mcqRun)
		after string // the follow-up sweep's re-entry head ("" = none run)
	}{
		{"head moved to a qualifying descendant", func(w *mqmWorld, r *mcqRun) {
			w.rentProductPush(r, CategoryAcceptanceScenariosPushed, "h1", "a2")
		}, "a2"},
		{"newer fix-up push", func(w *mqmWorld, r *mcqRun) {
			w.mqeAppend(r, mergeQueueFixupPushedCategory, map[string]any{"head_sha": "h9"})
		}, ""},
		{"PR read fails", func(w *mqmWorld, r *mcqRun) {
			w.mu.Lock()
			w.prReadFail = true
			w.mu.Unlock()
		}, "h1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, ej, _ := w.rentReadyPushed("a")
			locks := &atomic.Int64{}
			w.srv.cfg.MergeQueue = rentLockHookStore{PostgresStore: w.store, hook: func() { tc.hook(w, r) }, once: &sync.Once{}, locks: locks}
			if n := w.sweep(0); n != 0 {
				t.Fatalf("re-entered = %d, want 0", n)
			}
			if n := locks.Load(); n != 1 {
				t.Fatalf("merge-intent locks = %d, want 1 (gate 5 passed, so the locked half refused)", n)
			}
			w.requireNotReentered(r)
			w.requireEjectedNewest(r, ej)
			if tc.after == "" {
				return
			}
			w.srv.cfg.MergeQueue = w.store
			w.mu.Lock()
			w.prReadFail = false
			w.mu.Unlock()
			if n := w.sweep(0); n != 1 {
				t.Fatalf("follow-up re-entered = %d, want 1 at %s", n, tc.after)
			}
			w.requireReentered(r, ej, tc.after)
		})
	}
}

// reentryQualifyingChain is pure: nil rows and rows of other categories are
// no edge, an undecodable row is no edge, and a child already qualifying is
// not re-parented (its first chain stands).
func TestReentryQualifyingChain_Pure(t *testing.T) {
	row := func(seq int64, cat, payload string) *audit.Entry {
		return &audit.Entry{Sequence: seq, Category: cat, Payload: []byte(payload)}
	}
	rows := []*audit.Entry{
		nil,
		row(9, CategoryAcceptanceScenariosPushed, `{"base_sha":"x1","head_sha":"h1"}`), // re-parenting the seed: ignored
		row(4, CategoryAcceptanceScenariosPushed, `{"base_sha":"h1","head_sha":"a1"}`),
		row(5, CategoryConflictResolutionPushed, `not json`),
		row(6, CategoryBranchRebased, `not json`),
		row(7, "fixup_pushed", `{"base_sha":"a1","head_sha":"z1"}`),
		row(8, CategoryAcceptanceScenariosPushed, `{"base_sha":"h1","head_sha":"a1"}`), // already qualifying
	}
	if chain, ok := reentryQualifyingChain(rows, 3, "h1", "a1"); !ok || strings.Join(chain, ",") != "h1,a1" {
		t.Fatalf("chain = %v %v, want [h1 a1]", chain, ok)
	}
	if chain, ok := reentryQualifyingChain(rows, 3, "h1", "h1"); !ok || strings.Join(chain, ",") != "h1" {
		t.Fatalf("seed chain = %v %v, want [h1]", chain, ok)
	}
	if _, ok := reentryQualifyingChain(rows, 3, "h1", "z1"); ok {
		t.Fatal("a non-product category qualified z1")
	}
	if _, ok := reentryQualifyingChain(rows, 3, "", ""); ok {
		t.Fatal("an empty seed qualified the empty head")
	}
}

// rentAcceptanceStage adds the settled acceptance stage a scenario report comes
// from, with the verdict the runner recorded before its push.
func (w *mqmWorld) rentAcceptanceStage(r *mcqRun) *run.Stage {
	w.t.Helper()
	ctx := context.Background()
	st, err := w.repo.CreateStage(ctx, run.CreateStageParams{
		RunID: r.id, Sequence: 4, Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		w.t.Fatalf("setup: create acceptance stage: %v", err)
	}
	w.setStage(st.ID, run.StageStateSucceeded)
	w.rentStageRow(r, st.ID, CategoryAcceptanceOutcomeRecorded, map[string]any{"outcome": "passed"})
	return st
}

// rentStageRow appends one chained row on a stage BY CONSTRUCTION.
func (w *mqmWorld) rentStageRow(r *mcqRun, stageID uuid.UUID, category string, payload any) {
	w.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		w.t.Fatalf("setup: marshal %s: %v", category, err)
	}
	kind, subj := audit.ActorSystem, "test"
	if _, err := w.audit.AppendChained(context.Background(), audit.ChainAppendParams{RunID: r.id, StageID: &stageID,
		Timestamp: time.Now().UTC(), Category: category, ActorKind: &kind, ActorSubject: &subj, Payload: raw}); err != nil {
		w.t.Fatalf("setup: seed %s: %v", category, err)
	}
}

// rentConflictPass reopens the implement stage on a live conflict-resolution
// trigger. The trigger names no re-parked review stage, so the review gate stays
// parked and the recovery has only the implement stage to restore. The recorded
// base commit lets an admitted report's permission-drift check read its surfaces,
// so it raises no unevaluable concern that eligibility would then refuse.
func (w *mqmWorld) rentConflictPass(r *mcqRun, expectedHead string) {
	w.t.Helper()
	w.exec(`UPDATE runs SET document_base_commit = $1 WHERE id = $2`, rhBaseSHA, r.id)
	w.rentStageRow(r, r.impl.ID, CategoryStageConflictResolutionTriggered, conflictResolutionTrigger{
		Branch: r.branch, BaseRef: mcqBase, ExpectedHeadSHA: expectedHead, Pass: 1, PriorState: string(run.StageStateSucceeded),
	})
	w.setStage(r.impl.ID, run.StageStateRunning)
}

// rentReport signs one push report and sends it through the real route.
func (w *mqmWorld) rentReport(r *mcqRun, stageID uuid.UUID, outcome, head, base string) *httptest.ResponseRecorder {
	w.t.Helper()
	body, err := json.Marshal(map[string]any{
		"outcome": outcome, "branch": r.branch, "head_sha": head, "base_sha": base, "files_changed_count": 1,
	})
	if err != nil {
		w.t.Fatalf("setup: marshal report: %v", err)
	}
	sf := newSigningFake()
	priv, _ := sf.issue(w.t, r.id)
	w.srv.cfg.SigningRepo = sf
	w.srv.cfg.ArtifactRepo = newFakeArtifactRepo()
	resp := shipPRRequest(w.t, w.srv, r.id, stageID, priv, body, "")
	w.srv.waitBackgroundReviews()
	return resp
}

// rentMismatchRows counts r's reported_head_mismatch evidence rows.
func (w *mqmWorld) rentMismatchRows(r *mcqRun) int {
	w.t.Helper()
	n := 0
	for _, e := range w.rows(r, invariantmonitor.CategoryInvariantViolation) {
		if w.payload(e)["kind"] == invariantmonitor.KindReportedHeadMismatch {
			n++
		}
	}
	return n
}

// TestMergeQueueReentry_ForgedEdge_HeadNotOnBranch_NeverQualifies: an acceptance
// report names a qualifying base and a head that is neither the PR head nor the
// branch tip. It is refused with no row, so when that head later reaches the PR the
// walk has no edge to it and the sweep re-enters nothing. The control then lets the
// forge confirm the same head and edge, and the same world re-enters.
//
// COUNTERFACTUAL (M41, the acceptance hook call replaced with false): the edge row
// exists, the moved head qualifies and the sweep re-enters one → RED.
func TestMergeQueueReentry_ForgedEdge_HeadNotOnBranch_NeverQualifies(t *testing.T) {
	w := newMQMWorld(t)
	r, ej, push := w.rentReadyPushed("a")
	accept := w.rentAcceptanceStage(r)

	resp := w.rentReport(r, accept.ID, outcomeAcceptanceScenariosPushed, rhH2, "h1")
	if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), `"reported_head_mismatch"`) {
		t.Errorf("assert: status = %d, want 409 reported_head_mismatch:\n%s", resp.Code, resp.Body.String())
	}
	if rows := w.rows(r, CategoryAcceptanceScenariosPushed); len(rows) != 0 {
		t.Errorf("assert: acceptance_scenarios_pushed rows = %d, want 0", len(rows))
	}
	if n := w.rentMismatchRows(r); n != 1 {
		t.Errorf("assert: reported_head_mismatch rows = %d, want 1", n)
	}

	w.gh.set(func() { w.gh.prs[r.pr].head = rhH2 })
	if n := w.sweep(0); n != 0 {
		t.Errorf("assert: re-entered = %d at the refused head, want 0", n)
	}
	if rows := w.reentryRows(r); len(rows) != 0 {
		t.Errorf("assert: re-entry rows = %d, want 0", len(rows))
	}
	if latest, found, err := w.store.LatestForRun(context.Background(), r.id); err != nil || !found || latest.ID != ej.ID {
		t.Fatalf("assert: latest = %+v found=%v err=%v, want the ejected row %s", latest, found, err, ej.ID)
	}

	// Control: only the refusal held it back. The forge now confirms this edge.
	w.gh.set(func() { w.gh.commitParents[rhH2] = []string{"h1"} })
	if resp := w.rentReport(r, accept.ID, outcomeAcceptanceScenariosPushed, rhH2, "h1"); resp.Code != http.StatusOK {
		t.Fatalf("assert: confirmed report = %d, want 200:\n%s", resp.Code, resp.Body.String())
	}
	if n := w.sweep(0); n != 1 {
		t.Fatalf("assert: re-entered = %d once the forge confirmed the edge, want 1", n)
	}
	_, p := w.requireReentered(r, ej, rhH2)
	requireReentryChain(t, p, push, []string{"h1", rhH2})
}

// TestMergeQueueReentry_ForgedEdge_BaseNotFirstParent_NeverQualifies: the reported
// head IS the PR head by construction and the qualifying base is only its second
// parent, so the first-parent check alone refuses the edge. The conflict pass runs
// on a live trigger, whose recovery restores the stage, so a live stage is not what
// holds the sweep back. The control lets the forge show the base as first parent.
//
// COUNTERFACTUAL (M42, the first-parent check neutered): the edge row is written and
// the sweep re-enters at the head → RED on zero-rows and on the sweep.
func TestMergeQueueReentry_ForgedEdge_BaseNotFirstParent_NeverQualifies(t *testing.T) {
	cases := []struct {
		name     string
		outcome  string
		category string
		stage    func(w *mqmWorld, r *mcqRun) uuid.UUID
		refused  func(t *testing.T, w *mqmWorld, r *mcqRun, resp *httptest.ResponseRecorder)
		confirm  func(w *mqmWorld, r *mcqRun)
	}{
		{
			name: "acceptance", outcome: outcomeAcceptanceScenariosPushed, category: CategoryAcceptanceScenariosPushed,
			stage: func(w *mqmWorld, r *mcqRun) uuid.UUID { return w.rentAcceptanceStage(r).ID },
			refused: func(t *testing.T, _ *mqmWorld, _ *mcqRun, resp *httptest.ResponseRecorder) {
				if resp.Code != http.StatusConflict {
					t.Errorf("assert: status = %d, want 409:\n%s", resp.Code, resp.Body.String())
				}
			},
			confirm: func(*mqmWorld, *mcqRun) {},
		},
		{
			name: "conflict_resolution", outcome: "conflict_resolution_pushed", category: CategoryConflictResolutionPushed,
			stage: func(w *mqmWorld, r *mcqRun) uuid.UUID {
				w.rentConflictPass(r, "h1")
				return r.impl.ID
			},
			refused: func(t *testing.T, w *mqmWorld, r *mcqRun, resp *httptest.ResponseRecorder) {
				var body pullRequestFailureResponse
				_ = json.Unmarshal(resp.Body.Bytes(), &body)
				if resp.Code != http.StatusOK || body.Outcome != "failed" {
					t.Errorf("assert: status = %d outcome = %q, want 200 failed:\n%s", resp.Code, body.Outcome, resp.Body.String())
				}
				if rows := w.rows(r, CategoryStageConflictResolutionFailed); len(rows) != 1 {
					t.Errorf("assert: stage_conflict_resolution_failed rows = %d, want 1 (the trigger is consumed)", len(rows))
				}
				st, err := w.repo.GetStage(context.Background(), r.impl.ID)
				if err != nil {
					t.Fatalf("setup: get stage: %v", err)
				}
				if st.State != run.StageStateSucceeded {
					t.Errorf("assert: implement = %s, want succeeded (restored, so gate 1 is not what holds the sweep)", st.State)
				}
			},
			confirm: func(w *mqmWorld, r *mcqRun) { w.rentConflictPass(r, "h1") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, ej, push := w.rentReadyPushed("a")
			stageID := tc.stage(w, r)
			w.gh.set(func() {
				w.gh.prs[r.pr].head = rhH2
				w.gh.commitParents[rhH2] = []string{rhOther, "h1"}
			})

			resp := w.rentReport(r, stageID, tc.outcome, rhH2, "h1")
			tc.refused(t, w, r, resp)
			if rows := w.rows(r, tc.category); len(rows) != 0 {
				t.Errorf("assert: %s rows = %d, want 0", tc.category, len(rows))
			}
			if n := w.rentMismatchRows(r); n != 1 {
				t.Errorf("assert: reported_head_mismatch rows = %d, want 1", n)
			}
			if n := w.sweep(0); n != 0 {
				t.Errorf("assert: re-entered = %d on the refused edge, want 0", n)
			}
			if rows := w.reentryRows(r); len(rows) != 0 {
				t.Errorf("assert: re-entry rows = %d, want 0", len(rows))
			}
			if latest, found, err := w.store.LatestForRun(context.Background(), r.id); err != nil || !found || latest.ID != ej.ID {
				t.Fatalf("assert: latest = %+v found=%v err=%v, want the ejected row %s", latest, found, err, ej.ID)
			}

			// Control: the same edge with the base as first parent re-enters.
			tc.confirm(w, r)
			w.gh.set(func() { w.gh.commitParents[rhH2] = []string{"h1", rhOther} })
			if resp := w.rentReport(r, stageID, tc.outcome, rhH2, "h1"); resp.Code != http.StatusOK {
				t.Fatalf("assert: confirmed report = %d, want 200:\n%s", resp.Code, resp.Body.String())
			}
			if n := w.sweep(0); n != 1 {
				t.Fatalf("assert: re-entered = %d once the forge confirmed the edge, want 1", n)
			}
			_, p := w.requireReentered(r, ej, rhH2)
			requireReentryChain(t, p, push, []string{"h1", rhH2})
		})
	}
}

// rhConsumerView is what each reader of the audit chain answers about a reported
// head after its report settled.
type rhConsumerView struct {
	InLedger         bool
	ChainReports     bool
	DismissingCause  string
	MergeCause       string
	LatestRunHead    string
	GateViewHead     string
	PriorApprovalOK  bool
	CarriedToHead    bool
	QualifiesReentry bool
	ReentrySeed      string
}

// rhReadConsumers reads the head through every consumer, using the fix-up row at
// pushSeq as the qualifying walk's seed. Each read is the production one.
func (w *mqmWorld) rhReadConsumers(r *mcqRun, kc rhKindCase, pushSeq int64, prior, head string) rhConsumerView {
	w.t.Helper()
	ctx := context.Background()
	runRow := w.runRow(r)
	var v rhConsumerView

	ledger, complete := w.srv.buildReportedHeadLedger(ctx, runRow, "")
	if !complete {
		w.t.Fatalf("setup: the ledger read was incomplete")
	}
	if _, ok := ledger[prior]; !ok {
		w.t.Fatalf("setup: the ledger misses the recorded prior head %s", prior)
	}
	_, v.InLedger = ledger[head]

	var err error
	if v.ChainReports, err = w.srv.chainReportsHead(ctx, r.id, kc.category, "head_sha", head); err != nil {
		w.t.Fatalf("setup: chainReportsHead: %v", err)
	}
	v.DismissingCause = w.srv.dismissingCause(ctx, runRow, head)
	if v.MergeCause, err = w.srv.classifyMergeCandidateHead(ctx, runRow, head); err != nil {
		w.t.Fatalf("setup: classifyMergeCandidateHead: %v", err)
	}
	if v.LatestRunHead, _, err = w.srv.latestRunHeadSHA(ctx, r.id); err != nil {
		w.t.Fatalf("setup: latestRunHeadSHA: %v", err)
	}
	if v.GateViewHead, err = w.srv.gateViewCurrentHead(ctx, r.id); err != nil {
		w.t.Fatalf("setup: gateViewCurrentHead: %v", err)
	}

	rows := append(w.rows(r, CategoryBranchRebased), w.rows(r, CategoryConflictResolutionPushed)...)
	v.PriorApprovalOK, _ = cleanChainReaches(rows, prior, prior)
	v.CarriedToHead, _ = cleanChainReaches(rows, prior, head)

	entry := mergequeue.Entry{RunID: r.id}
	if _, v.QualifiesReentry, err = w.srv.mergeQueueReentryHeadChain(ctx, entry, pushSeq, prior, head); err != nil {
		w.t.Fatalf("setup: mergeQueueReentryHeadChain: %v", err)
	}
	if _, v.ReentrySeed, err = w.srv.mergeQueueFixupPushedAfter(ctx, entry, pushSeq-1); err != nil {
		w.t.Fatalf("setup: mergeQueueFixupPushedAfter: %v", err)
	}
	return v
}

// TestReportedHead_RefusedHeadReachesNoConsumer refuses through the real route for
// each kind, then reads the lineage ledger, the merge-candidate classifier, the
// approval-dismissal cause, the run-head and gate-view heads, the approval carrier
// walk and the re-entry walk. None sees the refused head or edge, so a refusal needs
// no consumer change. The admitted subtests are the positive controls: the same
// reads do see a head the forge confirmed.
//
// The fixture records one integration commit, so a head no report names classifies
// as fan_in rather than as "needs no verify", which the fix-up and conflict rows
// would otherwise share with it.
//
// COUNTERFACTUAL (M43a/b/c, each hook call replaced with false): the row lands, so
// the zero-rows assertion and the view comparison go RED for that kind.
func TestReportedHead_RefusedHeadReachesNoConsumer(t *testing.T) {
	refusedView := rhConsumerView{
		DismissingCause: dismissingCausePush, MergeCause: mergeCandidateCauseFanIn,
		LatestRunHead: rhH1, GateViewHead: rhH1, PriorApprovalOK: true, ReentrySeed: rhH1,
	}
	arrange := func(t *testing.T) (*mqmWorld, *mcqRun, *audit.Entry) {
		t.Helper()
		w := newMQMWorld(t)
		r := w.seed("a", rhH1, []byte(mcvDelegatedV0SpecYAML))
		push := w.rentPush(r, rhH1)
		w.mqeAppend(r, auditcomplete.CategoryIntegrationCommitRecorded, map[string]any{"merge_sha": rhOther})
		w.gh.set(func() { w.gh.baseTip[r.branch] = rhH1 })
		return w, r, push
	}
	// reportingStage puts the stage in the state the kind reports from.
	reportingStage := func(w *mqmWorld, r *mcqRun, kc rhKindCase) uuid.UUID {
		if kc == rhAcceptanceKind {
			return w.rentAcceptanceStage(r).ID
		}
		w.setStage(r.impl.ID, run.StageStateRunning)
		return r.impl.ID
	}

	refusals := []struct {
		name    string
		kinds   []rhKindCase
		prepare func(g *mcqGitHub, r *mcqRun)
		reason  string
	}{
		{"refused_head", rhAllKinds, func(g *mcqGitHub, _ *mcqRun) {
			g.commitParents[rhH2] = []string{rhH1}
		}, reportedHeadReasonHead},
		{"refused_base_edge", rhEdgeKinds, func(g *mcqGitHub, r *mcqRun) {
			g.prs[r.pr].head = rhH2
			g.commitParents[rhH2] = []string{rhOther, rhH1}
		}, reportedHeadReasonBaseEdge},
	}
	for _, rf := range refusals {
		for _, kc := range rf.kinds {
			t.Run(rf.name+"/"+kc.name, func(t *testing.T) {
				w, r, push := arrange(t)
				stageID := reportingStage(w, r, kc)
				w.gh.set(func() { rf.prepare(w.gh, r) })

				resp := w.rentReport(r, stageID, kc.outcome, rhH2, rhH1)

				if kc == rhAcceptanceKind {
					if resp.Code != http.StatusConflict {
						t.Errorf("assert: status = %d, want 409:\n%s", resp.Code, resp.Body.String())
					}
				} else {
					var body pullRequestFailureResponse
					_ = json.Unmarshal(resp.Body.Bytes(), &body)
					if resp.Code != http.StatusOK || body.Outcome != "failed" || body.Category != "C" {
						t.Errorf("assert: status = %d outcome = %q category = %q, want 200 failed C:\n%s",
							resp.Code, body.Outcome, body.Category, resp.Body.String())
					}
				}
				wantRows := 0
				if kc == rhFixupKind {
					wantRows = 1 // the prior push the fixture recorded
				}
				if rows := w.rows(r, kc.category); len(rows) != wantRows {
					t.Errorf("assert: %s rows = %d, want %d", kc.category, len(rows), wantRows)
				}
				evidence := 0
				for _, e := range w.rows(r, invariantmonitor.CategoryInvariantViolation) {
					if p := w.payload(e); p["kind"] == invariantmonitor.KindReportedHeadMismatch && p["reason"] == rf.reason {
						evidence++
					}
				}
				if evidence != 1 {
					t.Errorf("assert: reported_head_mismatch rows with reason %s = %d, want 1", rf.reason, evidence)
				}
				if got := w.rhReadConsumers(r, kc, push.Sequence, rhH1, rhH2); got != refusedView {
					t.Errorf("assert: consumers read the refused report:\n got %+v\nwant %+v", got, refusedView)
				}
			})
		}
	}

	admitted := []struct {
		kc   rhKindCase
		want rhConsumerView
	}{
		{rhFixupKind, rhConsumerView{InLedger: true, ChainReports: true, DismissingCause: dismissingCauseFixupPush,
			LatestRunHead: rhH2, GateViewHead: rhH2, PriorApprovalOK: true, ReentrySeed: rhH2}},
		{rhConflictKind, rhConsumerView{InLedger: true, ChainReports: true, DismissingCause: dismissingCausePush,
			MergeCause: mergeCandidateCauseConflictResolution, LatestRunHead: rhH1, GateViewHead: rhH2,
			QualifiesReentry: true, ReentrySeed: rhH1}},
		{rhAcceptanceKind, rhConsumerView{InLedger: true, ChainReports: true, DismissingCause: dismissingCausePush,
			MergeCause: mergeCandidateCauseFanIn, LatestRunHead: rhH2, GateViewHead: rhH2, PriorApprovalOK: true,
			QualifiesReentry: true, ReentrySeed: rhH1}},
	}
	for _, tc := range admitted {
		t.Run("admitted_control/"+tc.kc.name, func(t *testing.T) {
			w, r, push := arrange(t)
			stageID := reportingStage(w, r, tc.kc)
			if tc.kc == rhConflictKind {
				// The live-trigger guard (#3806) admits only an authorized pass.
				w.rentConflictPass(r, rhH1)
			}
			w.gh.set(func() {
				w.gh.prs[r.pr].head = rhH2
				w.gh.commitParents[rhH2] = []string{rhH1}
			})

			if resp := w.rentReport(r, stageID, tc.kc.outcome, rhH2, rhH1); resp.Code != http.StatusOK {
				t.Fatalf("assert: status = %d, want 200:\n%s", resp.Code, resp.Body.String())
			}
			if got := w.rhReadConsumers(r, tc.kc, push.Sequence, rhH1, rhH2); got != tc.want {
				t.Errorf("assert: consumers read the admitted report:\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}
