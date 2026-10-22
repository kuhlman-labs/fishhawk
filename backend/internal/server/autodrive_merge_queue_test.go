package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pglock"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// ADR-093 slice 2 (E87.5 / #4296), the delegated arm: AutoDriveRunGate's
// may_merge arm runs its dispatch tail under the run's merge-intent lock,
// observes merge-queue ownership first (approval condition D2: an active
// entry, live or held, any phase, carrying merge intent) and claims the run's
// ONE merge dispatch on the D1 ledger before the forge. The tests drive the
// real AutoDriveRunGate over the mqmWorld fixture (real Postgres queue, run
// and audit repositories, the real merge_dispatch_claims ledger, the mcq fake
// GitHub) read-only, with a countable delegated merger. Every test reads
// committed state: merger calls and the claim row, not only the outcome.

// admqSpecYAML delegates may_merge and declares a verify command, so the
// merge-candidate gate admits a head with a passing verdict.
const admqSpecYAML = `version: "0.5"
workflows:
  feature_change:
    operator_agent:
      may_merge: gates_resolved_ci_green
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
          verify:
            command: "scripts/test verify"
            timeout: "5m"
        produces:
          - artifact: pull_request
`

// admqMerger is a countable delegated GitHubMerger (it models auto-merge: it
// never merges the PR).
type admqMerger struct {
	mu  sync.Mutex
	n   int
	err error
}

func (m *admqMerger) MergePullRequest(context.Context, *run.Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	return m.err
}

func (m *admqMerger) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n
}

// admqReady seeds a may_merge-ready run at head: review stage settled, a
// clean advance onto head passed there (the merge-candidate gate admits), and
// a checks_green_awaiting_merge drive advance (the delegation is Met).
func (w *mqmWorld) admqReady(name, head string) *mcqRun {
	w.t.Helper()
	r := w.seed(name, head, []byte(admqSpecYAML))
	w.setStage(r.review.ID, run.StageStateSucceeded)
	w.mqeRebased(r, "pre-"+head, head, head, false)
	w.mqePassed(r, head)
	w.mqeAppend(r, drive.Category, map[string]any{"rule": string(drive.RuleChecksGreenAwaitingMerge)})
	return r
}

// admqDrive runs the delegated gate for r with merger m.
func (w *mqmWorld) admqDrive(r *mcqRun, m GitHubMerger) AutoDriveOutcome {
	w.t.Helper()
	runRow, err := w.repo.GetRun(context.Background(), r.id)
	if err != nil {
		w.t.Fatalf("GetRun: %v", err)
	}
	out, err := w.srv.AutoDriveRunGate(context.Background(), runRow, campaignOperatorIdentity(), m, nil)
	if err != nil {
		w.t.Fatalf("AutoDriveRunGate: %v", err)
	}
	return out
}

func requireObserveOnly(t *testing.T, out AutoDriveOutcome, note string) {
	t.Helper()
	if out.Acted || out.Paged || out.DecisionRequired || out.Note != note {
		t.Fatalf("outcome = %+v, want observe-only %q", out, note)
	}
}

// TestAutoDrive_MayMergeNoEntryClaimsAndDispatches is the positive control:
// with no queue entry the arm claims the run's dispatch (path delegated, at
// the admitted head) BEFORE the merger and dispatches once; a second tick
// meets the claim and never calls the merger again (D1).
func TestAutoDrive_MayMergeNoEntryClaimsAndDispatches(t *testing.T) {
	w := newMQMWorld(t)
	r := w.admqReady("free", "h1")
	m := &admqMerger{}
	out := w.admqDrive(r, m)
	if !out.Acted || out.Action != delegation.ActionMerge || m.calls() != 1 {
		t.Fatalf("outcome = %+v merger calls %d, want acted merge with 1 call", out, m.calls())
	}
	var head string
	st, path := w.claimRow(r, &head)
	if st != string(mergequeue.DispatchDispatched) || path != mergequeue.DispatchPathDelegated || head != "h1" {
		t.Fatalf("claim = %s/%s@%s, want dispatched/delegated@h1", st, path, head)
	}
	requireObserveOnly(t, w.admqDrive(r, m), autoDriveNoteAlreadyClaimed)
	if m.calls() != 1 {
		t.Fatalf("merger calls after the second tick = %d, want still 1", m.calls())
	}
}

// claimRow reads r's claim state, path and head ("" state when none).
func (w *mqmWorld) claimRow(r *mcqRun, head *string) (state, path string) {
	w.t.Helper()
	err := w.pool.QueryRow(context.Background(),
		`SELECT state, path, head_sha FROM merge_dispatch_claims WHERE run_id = $1`, r.id).Scan(&state, &path, head)
	if err != nil && err.Error() != "no rows in result set" {
		w.t.Fatalf("read claim: %v", err)
	}
	return state, path
}

// TestAutoDrive_MayMergeObservesQueueOwnership pins D2 on the delegated arm:
// an active entry carrying merge intent, live in verifying or passed, or
// held, owns the merge, so the arm is observe-only, claims nothing and never
// calls the merger. Counterfactual: the owned branch made a no-op lets the
// arm claim and dispatch on the live rows, merger calls 1, RED. A HELD entry
// is refused earlier by the merge-candidate gate (merge_candidate_held), so
// D2 is a backstop there and that row asserts only the committed outcome.
func TestAutoDrive_MayMergeObservesQueueOwnership(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase mergequeue.Phase
		held  bool
		note  string
	}{
		{"live verifying", mergequeue.PhaseVerifying, false, autoDriveNoteQueueOwnsMerge},
		{"live passed", mergequeue.PhasePassed, false, autoDriveNoteQueueOwnsMerge},
		{"held", mergequeue.PhaseVerifying, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r := w.admqReady("own", "h1")
			w.intentEntry(r, "h1", tc.phase)
			if tc.held {
				e := w.active(r)
				if _, err := w.store.Unadmit(context.Background(), e.ID, e.AdmissionEpoch); err != nil {
					t.Fatalf("unadmit: %v", err)
				}
			}
			m := &admqMerger{}
			out := w.admqDrive(r, m)
			if tc.note != "" {
				requireObserveOnly(t, out, tc.note)
			} else if out.Acted || out.Paged || out.DecisionRequired {
				t.Fatalf("outcome = %+v, want observe-only", out)
			}
			if m.calls() != 0 || len(w.merger.calls()) != 0 {
				t.Fatalf("dispatches: delegated %d pinned %v, want none", m.calls(), w.merger.calls())
			}
			var head string
			if st, _ := w.claimRow(r, &head); st != "" {
				t.Fatalf("claim state = %q, want no claim (the owned arm claims nothing)", st)
			}
		})
	}
}

// TestAutoDrive_MayMergeOwnershipNeedsPinnedSeam: without a head-pinned
// GateMerger the queue can never merge, so it owns nothing and the arm
// dispatches as before (the operational stop).
func TestAutoDrive_MayMergeOwnershipNeedsPinnedSeam(t *testing.T) {
	w := newMQMWorld(t)
	w.srv.cfg.GateMerger = mqmUnpinned{}
	r := w.admqReady("unpinned", "h1")
	w.intentEntry(r, "h1", mergequeue.PhasePassed)
	m := &admqMerger{}
	if out := w.admqDrive(r, m); !out.Acted || m.calls() != 1 {
		t.Fatalf("outcome = %+v merger calls %d, want acted with 1 call", out, m.calls())
	}
}

// admqLockReadErrQueue fails ActiveForRun only inside a pglock fn, isolating
// the arm's in-lock ownership read from every earlier queue read.
type admqLockReadErrQueue struct{ mergequeue.Store }

func (q admqLockReadErrQueue) ActiveForRun(ctx context.Context, runID uuid.UUID) (mergequeue.Status, bool, error) {
	if pglock.InLock(ctx) {
		return mergequeue.Status{}, false, errors.New("injected queue read failure")
	}
	return q.Store.ActiveForRun(ctx, runID)
}

// TestAutoDrive_MayMergeQueueReadErrorObserveOnly: an unreadable ownership
// check fails closed, observe-only, no claim and no dispatch. Counterfactual:
// treating the read error as not owned lets the arm claim and dispatch, RED.
func TestAutoDrive_MayMergeQueueReadErrorObserveOnly(t *testing.T) {
	w := newMQMWorld(t)
	w.srv.cfg.MergeQueue = admqLockReadErrQueue{Store: w.store}
	r := w.admqReady("readerr", "h1")
	m := &admqMerger{}
	requireObserveOnly(t, w.admqDrive(r, m), autoDriveNoteQueueUnreadable)
	var head string
	if st, _ := w.claimRow(r, &head); m.calls() != 0 || st != "" {
		t.Fatalf("merger calls %d claim %q, want 0 and no claim", m.calls(), st)
	}
}

// TestAutoDrive_MayMergeLockHeldElsewhereObserveOnly: while another dispatch
// path holds the run's merge-intent lock, the arm's bounded acquire wait
// runs out and it is observe-only with nothing claimed or dispatched.
func TestAutoDrive_MayMergeLockHeldElsewhereObserveOnly(t *testing.T) {
	w := newMQMWorld(t)
	r := w.admqReady("locked", "h1")
	orig := autoDriveMergeLockAcquireTimeout
	autoDriveMergeLockAcquireTimeout = timescale.D(300 * time.Millisecond)
	t.Cleanup(func() { autoDriveMergeLockAcquireTimeout = orig })

	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- w.store.WithMergeIntentLock(context.Background(), r.id, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	m := &admqMerger{}
	out := w.admqDrive(r, m)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("holder: %v", err)
	}
	requireObserveOnly(t, out, autoDriveNoteLockUnavailable)
	var head string
	if st, _ := w.claimRow(r, &head); m.calls() != 0 || st != "" {
		t.Fatalf("merger calls %d claim %q, want 0 and no claim", m.calls(), st)
	}
}

// admqLapseQueue runs fn under the real lock, then reports a lapse.
type admqLapseQueue struct{ mergequeue.Store }

func (q admqLapseQueue) WithMergeIntentLock(ctx context.Context, runID uuid.UUID, fn func(context.Context) error) error {
	if err := q.Store.WithMergeIntentLock(ctx, runID, fn); err != nil {
		return err
	}
	return fmt.Errorf("%w: %w", mergequeue.ErrIntentLockLapsed, pglock.ErrLockLapsed)
}

// TestAutoDrive_MayMergeLockLapseAfterFnKeepsOutcome: a lock that lapsed after
// fn ran leaves fn's outcome standing (a WARN only): the dispatch happened.
func TestAutoDrive_MayMergeLockLapseAfterFnKeepsOutcome(t *testing.T) {
	w := newMQMWorld(t)
	w.srv.cfg.MergeQueue = admqLapseQueue{Store: w.store}
	r := w.admqReady("lapse", "h1")
	m := &admqMerger{}
	if out := w.admqDrive(r, m); !out.Acted || m.calls() != 1 {
		t.Fatalf("outcome = %+v merger calls %d, want acted with 1 call", out, m.calls())
	}
}

// TestAutoDrive_MayMergeExistingClaimNeverCallsForge pins the delegated D1
// claim in isolation: with no queue entry (so ownership is false) but an
// existing claim from another path, the arm is observe-only and the merger is
// never called. Counterfactual: the !claimed branch made a no-op dispatches,
// merger calls 1, RED.
func TestAutoDrive_MayMergeExistingClaimNeverCallsForge(t *testing.T) {
	for _, state := range []mergequeue.DispatchState{mergequeue.DispatchDispatched, mergequeue.DispatchFailed} {
		t.Run(string(state), func(t *testing.T) {
			w := newMQMWorld(t)
			r := w.admqReady("claimed", "h1")
			if _, err := w.store.ClaimMergeDispatch(context.Background(), r.id, mergequeue.DispatchPathInline, "h1"); err != nil {
				t.Fatalf("seed claim: %v", err)
			}
			if state == mergequeue.DispatchFailed {
				if err := w.store.MarkMergeDispatchFailed(context.Background(), r.id, "forge 500"); err != nil {
					t.Fatalf("mark failed: %v", err)
				}
			}
			m := &admqMerger{}
			requireObserveOnly(t, w.admqDrive(r, m), autoDriveNoteAlreadyClaimed)
			if m.calls() != 0 {
				t.Fatalf("merger calls = %d, want 0", m.calls())
			}
		})
	}
}

// admqClaimErrQueue fails ClaimMergeDispatch.
type admqClaimErrQueue struct{ mergequeue.Store }

func (admqClaimErrQueue) ClaimMergeDispatch(context.Context, uuid.UUID, string, string) (mergequeue.DispatchClaim, error) {
	return mergequeue.DispatchClaim{}, errors.New("injected claim failure")
}

// TestAutoDrive_MayMergeClaimErrorObserveOnly: a claim that fails is
// observe-only with the merger never called (fail closed).
func TestAutoDrive_MayMergeClaimErrorObserveOnly(t *testing.T) {
	w := newMQMWorld(t)
	w.srv.cfg.MergeQueue = admqClaimErrQueue{Store: w.store}
	r := w.admqReady("claimerr", "h1")
	m := &admqMerger{}
	requireObserveOnly(t, w.admqDrive(r, m), autoDriveNoteDispatchUnclaimed)
	if m.calls() != 0 {
		t.Fatalf("merger calls = %d, want 0", m.calls())
	}
}

// TestAutoDrive_MayMergeDispatchFailureMarksClaimFailed: a merger error after
// the claim moves the claim to dispatch_failed (never deleted) and surfaces
// the error; the next tick meets the claim and never calls the merger again
// until the operator re-arms it.
func TestAutoDrive_MayMergeDispatchFailureMarksClaimFailed(t *testing.T) {
	w := newMQMWorld(t)
	r := w.admqReady("fail", "h1")
	m := &admqMerger{err: errors.New("forge 500")}
	runRow, err := w.repo.GetRun(context.Background(), r.id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	out, err := w.srv.AutoDriveRunGate(context.Background(), runRow, campaignOperatorIdentity(), m, nil)
	if err == nil || out.Acted || out.Note != "merge dispatch failed" {
		t.Fatalf("outcome = %+v err %v, want the dispatch failure surfaced", out, err)
	}
	var head string
	if st, path := w.claimRow(r, &head); st != string(mergequeue.DispatchFailed) || path != mergequeue.DispatchPathDelegated {
		t.Fatalf("claim = %s/%s, want dispatch_failed/delegated", st, path)
	}
	m.err = nil
	requireObserveOnly(t, w.admqDrive(r, m), autoDriveNoteAlreadyClaimed)
	if m.calls() != 1 {
		t.Fatalf("merger calls = %d, want still 1 (no re-dispatch before the re-arm)", m.calls())
	}
}

// TestAutoDrive_MayMergeH2DelegatedDuringVerifyingThenMergeOnPassDispatchesOnce
// is approval condition D1 race (ii), the H2 interleaving exactly: the
// delegated arm runs while the intent entry is mid-pass (phase verifying, the
// head already passed, so every delegated gate admits), then merge-on-pass
// runs. Exactly ONE forge dispatch: the arm observes the queue's ownership
// (D2) and merge-on-pass merges pinned. With the ownership branch deleted the
// arm claims and dispatches, and merge-on-pass then meets the delegated claim
// and holds (D1): still exactly one. Only with both deleted does it double.
func TestAutoDrive_MayMergeH2DelegatedDuringVerifyingThenMergeOnPassDispatchesOnce(t *testing.T) {
	w := newMQMWorld(t)
	r := w.admqReady("h2", "h1")
	seq := w.intentEntry(r, "h1", mergequeue.PhaseVerifying)
	m := &admqMerger{}
	w.admqDrive(r, m)

	w.exec(`UPDATE merge_candidate_queue_entries SET phase = 'passed' WHERE run_id = $1`, r.id)
	out := w.srv.mergeQueueMergeOnPass(context.Background(),
		mergeOnPassRequest{Entry: w.active(r), IntentSequence: seq, Ledger: w.srv.mergeDispatchLedger()})

	if total := m.calls() + len(w.merger.calls()); total != 1 {
		t.Fatalf("forge dispatches = %d (delegated %d, pinned %v; merge-on-pass %+v), want exactly 1",
			total, m.calls(), w.merger.calls(), out)
	}
}

// TestAutoDrive_MayMergeDelegatedDispatchThenMergeOnPassDispatchesOnce pins
// the D1 ledger standing on its own across the delegated and queue paths: the
// delegated arm dispatches BEFORE the run has any queue entry (so ownership is
// never in play), the run is then enqueued with intent and passes, and
// merge-on-pass meets the delegated claim and never calls the forge.
// Counterfactual (dropping the UNIQUE run_id constraint): merge-on-pass claims
// again and merges pinned, two dispatches, RED.
func TestAutoDrive_MayMergeDelegatedDispatchThenMergeOnPassDispatchesOnce(t *testing.T) {
	w := newMQMWorld(t)
	r := w.admqReady("d1", "h1")
	m := &admqMerger{}
	if out := w.admqDrive(r, m); !out.Acted || m.calls() != 1 {
		t.Fatalf("delegated outcome = %+v calls %d, want one acted dispatch", out, m.calls())
	}
	seq := w.intentEntry(r, "h1", mergequeue.PhasePassed)
	out := w.srv.mergeQueueMergeOnPass(context.Background(),
		mergeOnPassRequest{Entry: w.active(r), IntentSequence: seq, Ledger: w.srv.mergeDispatchLedger()})
	requireOutcome(t, out, mergeOnPassHeld, mergeOnPassReasonAlreadyDispatched)
	if total := m.calls() + len(w.merger.calls()); total != 1 {
		t.Fatalf("forge dispatches = %d (delegated %d, pinned %v), want exactly 1", total, m.calls(), w.merger.calls())
	}
}
