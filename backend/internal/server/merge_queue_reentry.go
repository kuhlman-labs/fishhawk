package server

// ADR-093 slice 3 (D4, E87.6 / #4297): AUTOMATIC RE-ENTRY. A red pass whose
// failed verdict ADR-090 D6 routed to a delegated fix-up (merge_queue_ejected
// fixup_routed true) rejoins the merge queue at the TAIL (ADR-093 Q2) once
// that fix-up has pushed and the run is merge-queue eligible again, with no
// operator action. sweepMergeQueueReentries runs on the yield re-probe ticker
// (RunMergeQueueYieldReprobe), after sweepMergeQueueYields.
//
// Each mergequeue.ReentryReader candidate (an ejected verify_failed row with
// merge intent, still its run's newest row, on a non-terminal run) is
// evaluated only when it is DUE on the yield re-probe schedule
// (yieldReprobeDue) over the row's database-clock settled age, judged over
// the time since this process's previous re-entry sweep (process-local,
// ADR-112 A1.4: after a restart the window is (0, age]). A due candidate
// re-enters only when EVERY gate holds, and a refused or undeterminable gate
// skips it until its next due tick (never dropped, never enqueued yielded):
//
//  1. no stage of the run is dispatched or running (ADR-115 D4 compliance:
//     a live agent pass may still move the head);
//  2. the entry's merge_queue_ejected row says fixup_routed true;
//  3. a fixup_pushed row's CHAIN SEQUENCE follows that ejected row's (never a
//     cross-clock timestamp comparison);
//  4. the merge_queue_entered row AT the entry's stored MergeIntentSequence
//     exists (the intent is bound to that sequence, never to the newest row);
//  5. the PR is open, on the entry's base, and its head moved off the ejected
//     anchored head;
//  6. the ENTRY-variant eligibility (mergeQueueEligibility) passes at the NEW
//     head, with a LIVE forge approval read there when the review stage
//     declares an approval gate: an approval the fix-up push dismissed means
//     no re-entry.
//
// Then, under the run's merge-intent lock, LatestForRun must still be that
// ejected row, which also proves the run has no active entry (an operator
// fishhawk_merge_run that re-entered first wins). The merge_queue_entered row
// is appended BEFORE the queue write (record before commit, as merge_run):
// actor SYSTEM (never the operator), reentry true, the original intent row's
// verdict prose copied so merge-on-pass records a verdict; the merge still
// binds to the verified head, never to a SHA in that prose. Enqueue then
// inserts a FRESH row (no order_key) at the tail carrying the new row's
// sequence, merge_candidate_queue_held is written, and the base is pumped
// detached. A failed append writes no entry; a failed Enqueue after the append
// leaves an orphan row no queue entry names, and a later sweep re-enters with
// a new row. Contract: backend/internal/server/README.md.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// mergeQueueReentryLimit bounds one sweep's candidate read.
const mergeQueueReentryLimit = 50

// mergeQueueReentryBasisDelegatedFixup is the merge_queue_entered
// reentry_basis of an automatic re-entry.
const mergeQueueReentryBasisDelegatedFixup = "delegated_fixup_passed"

// mergeQueueFixupPushedCategory is the fix-up push report's audit row
// (pullrequest.go succeedFixupPushStage).
const mergeQueueFixupPushedCategory = "fixup_pushed"

// mergeQueueReentryOutcome is what one candidate's evaluation did.
type mergeQueueReentryOutcome string

const (
	reentryNotDue    mergeQueueReentryOutcome = "not_due"
	reentrySkipped   mergeQueueReentryOutcome = "skipped"
	reentryReentered mergeQueueReentryOutcome = "reentered"
)

// mergeQueueReentryClock is the re-entry sweep's process-local previous-sweep
// instant (ADR-112 A1.4); the zero value has none.
type mergeQueueReentryClock struct{ last time.Time }

// since records now and returns the time since the previous sweep (0 before
// the first).
func (c *mergeQueueReentryClock) since(now time.Time) time.Duration {
	var d time.Duration
	if !c.last.IsZero() {
		d = now.Sub(c.last)
	}
	c.last = now
	return d
}

// sweepMergeQueueReentries evaluates every DUE re-entry candidate (see the
// file comment) and returns how many re-entered. A store without the
// ReentryReader and EjectionReader capabilities, or a candidate read error,
// does nothing; the next tick retries.
func (s *Server) sweepMergeQueueReentries(ctx context.Context, interval, since time.Duration) int {
	if s.cfg.MergeQueue == nil || s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil || s.cfg.GitHub == nil {
		return 0
	}
	reader, ok := s.cfg.MergeQueue.(mergequeue.ReentryReader)
	if !ok {
		return 0
	}
	if _, ok := s.cfg.MergeQueue.(mergequeue.EjectionReader); !ok {
		return 0
	}
	cands, err := reader.ReentryCandidates(ctx, mergeQueueReentryLimit)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge queue: listing re-entry candidates failed; the next tick retries",
			slog.String("error", err.Error()))
		return 0
	}
	n := 0
	for _, e := range cands {
		if !yieldReprobeDue(e.SettledAge, since, interval) {
			continue
		}
		if s.reenterMergeQueueEntry(ctx, e) == reentryReentered {
			n++
		}
	}
	return n
}

// reenterMergeQueueEntry judges one due candidate e (an ejected entry) and
// re-enters its run at the tail when every gate holds.
func (s *Server) reenterMergeQueueEntry(ctx context.Context, e mergequeue.Entry) mergeQueueReentryOutcome {
	skip := func(why string, err error) mergeQueueReentryOutcome {
		attrs := []slog.Attr{slog.String("run_id", e.RunID.String()), slog.String("entry_id", e.ID.String()), slog.String("why", why)}
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug, "merge queue: re-entry candidate skipped; a later due tick re-evaluates it", attrs...)
		return reentrySkipped
	}
	if e.State != mergequeue.StateEjected || e.MergeIntentAt == nil {
		return skip("not an ejected intent-bearing entry", nil)
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, e.RunID)
	if err != nil {
		return skip("run unreadable", err)
	}
	if runRow.State.IsTerminal() {
		return skip("run terminal", nil)
	}
	// Gate 1 (ADR-115 D4 compliance): no live agent pass.
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, e.RunID)
	if err != nil {
		return skip("stages unreadable", err)
	}
	for _, st := range stages {
		if st.State == run.StageStateDispatched || st.State == run.StageStateRunning {
			return skip("a stage is "+string(st.State), nil)
		}
	}
	// Gate 2: the ejection routed a delegated fix-up.
	ejSeq, routed, err := s.mergeQueueEjectionRouted(ctx, e)
	if err != nil {
		return skip("merge_queue_ejected unreadable", err)
	}
	if !routed {
		return skip("no delegated fix-up was routed for the ejection", nil)
	}
	// Gate 3: that fix-up pushed AFTER the ejection, by chain sequence.
	pushSeq, err := s.mergeQueueFixupPushedAfter(ctx, e, ejSeq)
	if err != nil {
		return skip("fixup_pushed unreadable", err)
	}
	if pushSeq == 0 {
		return skip("no fix-up has pushed since the ejection", nil)
	}
	// Gate 4: the original intent row, bound to the entry's stored sequence.
	if e.MergeIntentSequence <= 0 {
		return skip("the entry records no intent sequence", nil)
	}
	intent, found, err := s.mergeQueueIntentRow(ctx, e.RunID, mergeQueueIntentAnchor{Sequence: e.MergeIntentSequence, BaseRef: e.BaseRef})
	if err != nil {
		return skip("merge_queue_entered unreadable", err)
	}
	if !found {
		return skip("no merge_queue_entered row at the entry's intent sequence", nil)
	}
	// Gate 5: the PR is open on the entry's base and its head moved.
	pr, err := s.mergeQueueReentryReadPR(ctx, runRow)
	if err != nil {
		return skip("pull request unreadable", err)
	}
	if pr.Merged || pr.State != "open" || pr.BaseRef != e.BaseRef || pr.HeadSHA == "" || pr.HeadSHA == e.AnchoredHeadSHA {
		return skip("the pull request is not open on the base with a moved head", nil)
	}
	// Gate 6: entry-variant eligibility at the NEW head (live approval).
	if elig := s.mergeQueueEligibility(ctx, runRow, pr.HeadSHA); !elig.Eligible {
		return skip("not merge-queue eligible at the new head", errors.New(strings.Join(elig.codes(), ",")))
	}

	var entered *mergequeue.Entry
	lerr := s.cfg.MergeQueue.WithMergeIntentLock(ctx, e.RunID, func(fctx context.Context) error {
		got, err := s.reenterMergeQueueLocked(fctx, e, runRow, pr.HeadSHA, intent, ejSeq, pushSeq)
		entered = got
		return err
	})
	if lerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge queue: automatic re-entry did not land; the next due tick retries",
			slog.String("run_id", e.RunID.String()), slog.String("entry_id", e.ID.String()), slog.String("error", lerr.Error()))
	}
	if entered == nil {
		return reentrySkipped
	}
	s.pumpMergeCandidateQueueDetached(ctx, entered.Base())
	return reentryReentered
}

// errReentryStale is the in-lock recheck's refusal: the candidate is no
// longer the run's newest ejected row.
var errReentryStale = errors.New("the re-entry candidate is stale under the merge-intent lock")

// reenterMergeQueueLocked is the locked half of reenterMergeQueueEntry: the
// in-lock recheck, the record-before-commit merge_queue_entered append, then
// the Enqueue at the tail. It returns the new entry, or nil with the reason.
func (s *Server) reenterMergeQueueLocked(ctx context.Context, e mergequeue.Entry, runRow *run.Run, head string,
	intent mergeQueueIntentRecord, ejSeq, pushSeq int64) (*mergequeue.Entry, error) {
	// The run's newest row still being THIS ejected row is the whole
	// recheck: a run's active entry is always its newest row, so it also
	// proves the run has no active entry (an operator merge_run that
	// re-entered first, or a concurrent sweep, made a newer row).
	er, ok := s.cfg.MergeQueue.(mergequeue.EjectionReader)
	if !ok {
		return nil, errors.New("the queue store cannot read a run's newest row")
	}
	latest, found, err := er.LatestForRun(ctx, e.RunID)
	if err != nil {
		return nil, err
	}
	if !found || latest.ID != e.ID || latest.State != mergequeue.StateEjected {
		return nil, errReentryStale
	}
	prURL := ""
	if runRow.PullRequestURL != nil {
		prURL = *runRow.PullRequestURL
	}
	payload, _ := json.Marshal(map[string]any{
		"run_id":                   e.RunID.String(),
		"repo":                     runRow.Repo,
		"base_ref":                 e.BaseRef,
		"anchored_head_sha":        head,
		"verdict":                  intent.Verdict,
		"pr_url":                   prURL,
		"reentry":                  true,
		"reentry_basis":            mergeQueueReentryBasisDelegatedFixup,
		"reentry_of_entry_id":      e.ID.String(),
		"original_intent_sequence": e.MergeIntentSequence,
		"ejected_sequence":         ejSeq,
		"fixup_pushed_sequence":    pushSeq,
	})
	kind, subject := audit.ActorSystem, mergeCandidateSystemSubject
	row, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: e.RunID, Timestamp: time.Now().UTC(), Category: CategoryMergeQueueEntered,
		ActorKind: &kind, ActorSubject: &subject, Payload: payload,
	})
	if err != nil {
		return nil, err // record before commit: no row, no entry
	}
	res, err := s.cfg.MergeQueue.Enqueue(ctx, mergequeue.EnqueueRequest{
		RunID: e.RunID, BaseRef: e.BaseRef, HeadSHA: head, MergeIntent: true, IntentSequence: row.Sequence,
	})
	if err != nil {
		return nil, err // the row is an orphan no entry names; a later sweep records again
	}
	s.writeMergeCandidateQueueAudit(ctx, CategoryMergeCandidateQueueHeld, res.Entry, nil)
	return &res.Entry, nil
}

// mergeQueueEjectionRouted reads e's merge_queue_ejected row (matched by
// entry_id) and reports its chain sequence and fixup_routed. No row is not
// routed (a lost best-effort row costs the automatic re-entry: fail closed).
func (s *Server) mergeQueueEjectionRouted(ctx context.Context, e mergequeue.Entry) (int64, bool, error) {
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, e.RunID, CategoryMergeQueueEjected)
	if err != nil {
		return 0, false, err
	}
	for i := len(rows) - 1; i >= 0; i-- {
		var p struct {
			EntryID     string `json:"entry_id"`
			FixupRouted bool   `json:"fixup_routed"`
		}
		if json.Unmarshal(rows[i].Payload, &p) == nil && p.EntryID == e.ID.String() {
			return rows[i].Sequence, p.FixupRouted, nil
		}
	}
	return 0, false, nil
}

// mergeQueueFixupPushedAfter returns the newest fixup_pushed row's chain
// sequence when it follows after (0 when none does).
func (s *Server) mergeQueueFixupPushedAfter(ctx context.Context, e mergequeue.Entry, after int64) (int64, error) {
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, e.RunID, mergeQueueFixupPushedCategory)
	if err != nil {
		return 0, err
	}
	var best int64
	for _, r := range rows {
		if r.Sequence > after && r.Sequence > best {
			best = r.Sequence
		}
	}
	return best, nil
}

// mergeQueueReentryReadPR reads the run's tracked PR, as merge-on-pass does.
func (s *Server) mergeQueueReentryReadPR(ctx context.Context, runRow *run.Run) (*forge.PullRequest, error) {
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return nil, errors.New("the run has no installation to read its pull request")
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return nil, err
	}
	number := parsePRNumberFromURL(runRow.PullRequestURL)
	if number <= 0 {
		return nil, errors.New("the run has no tracked pull request")
	}
	return s.mergeOnPassReadPR(ctx, mergeOnPassTarget{scope: forge.FromGitHubInstallationID(*runRow.InstallationID), repo: repo, number: number})
}
