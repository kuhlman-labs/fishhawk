package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// E86.54 / #4391: the operator's verify-only re-run of a FAILED merge-candidate
// head whose implement stage carries a QUEUED, never-dispatched ADR-090 D6
// fix-up. The re-run converts that fix-up IN PLACE into a verify-only pass:
// no stage state transition, no new transition edge, no run-package change.
//
//   - rerunQueuedMergeCandidateFixup is the verb arm: under the stage's
//     admission fence it proves eligibility, appends ONE stage_fixup_superseded
//     marker, voids the failed verdict and starts the pass;
//   - beginSupersededFixupPass is beginMergeCandidateVerifyPass's
//     SUPERSEDED-FIX-UP arm: it writes the conversion trigger (a
//     stage_merge_candidate_verify_triggered row carrying
//     supersedes_fixup_trigger_sequence) for whichever producer starts the pass;
//   - confirmedFixupSupersessions is the ONE reader rule: a marker counts only
//     when a conversion trigger naming its fix-up trigger follows it.

// CategoryStageFixupSuperseded is the operator's in-place supersession of a
// queued, never-dispatched ADR-090 D6 fix-up by a verify-only re-run. It is
// INERT unless a stage_merge_candidate_verify_triggered row naming its fix-up
// trigger (supersedes_fixup_trigger_sequence) follows it: a marker followed
// instead by a dispatch, push or settle is never a supersession. INTERNAL:
// not an issue-comment surface.
const CategoryStageFixupSuperseded = "stage_fixup_superseded"

// fixupSupersededByVerifyRerun is the marker's superseded_by value.
const fixupSupersededByVerifyRerun = "merge_candidate_verify_rerun"

// categoryInstallationTokenIssued is the runner-minted installation token row
// (installationtoken.go writes the literal): the only dispatch mark a stage
// carries when no concurrency slot store is wired.
const categoryInstallationTokenIssued = "installation_token_issued"

// stageAdmissionTryWait bounds beginSupersededFixupPass's acquisition of a
// stage admission fence its context does not already hold (approval condition
// C1): the pump that reaches it may run on a goroutine holding ANOTHER stage's
// fence, so it never blocks unboundedly.
const stageAdmissionTryWait = 2 * time.Second

// stageFixupSupersededPayload is the stage_fixup_superseded row.
type stageFixupSupersededPayload struct {
	// TriggerSequence is the superseded stage_fixup_triggered row's sequence.
	TriggerSequence int64 `json:"trigger_sequence"`
	// SupersededBy is always merge_candidate_verify_rerun.
	SupersededBy string `json:"superseded_by"`
	// FailedVerdictSequence is the failed merge_candidate_verified row the
	// re-run voids.
	FailedVerdictSequence int64 `json:"failed_verdict_sequence"`
	// Reason is "operator_rerun: <the operator's reason>".
	Reason string `json:"reason"`
}

// queuedFixupDisqualifyingCategories is the set of stage-bound rows that,
// sequenced after the D6 fix-up trigger, mean the fix-up is no longer queued
// and never-dispatched: a fix-up settle, a retry, a dispatch mark, a slot-queue
// row, or a later re-open owning the stage.
var queuedFixupDisqualifyingCategories = []string{
	// fix-up settles
	CategoryFixupPushed, CategoryFixupNoChanges, CategoryStageFixupRecovered,
	CategoryStageFixupSuperseded, CategoryDispatchReaperFailed,
	// retries
	CategoryStageRetried, CategoryStageOverrideRetried,
	// dispatch marks
	CategoryStageConcurrencyAdmitted, categoryInstallationTokenIssued,
	// slot queue
	CategoryStageConcurrencyQueued,
	// later re-opens
	CategoryStageMergeCandidateVerifyTriggered, CategoryStageConflictResolutionTriggered,
}

// --- the admission fence, re-entrant per request ---

// stageAdmissionHeld is the context marker naming every stage admission fence
// the request already holds. It is a chain so a nested acquisition never
// shadows an outer one.
type stageAdmissionHeld struct {
	stageID uuid.UUID
	outer   *stageAdmissionHeld
}

// stageAdmissionHeldKey keys stageAdmissionHeld on a context.
type stageAdmissionHeldKey struct{}

// stageAdmissionHeldBy reports whether ctx carries the marker for stageID.
func stageAdmissionHeldBy(ctx context.Context, stageID uuid.UUID) bool {
	h, _ := ctx.Value(stageAdmissionHeldKey{}).(*stageAdmissionHeld)
	for ; h != nil; h = h.outer {
		if h.stageID == stageID {
			return true
		}
	}
	return false
}

// withStageAdmissionHeld returns ctx marked as holding stageID's fence.
func withStageAdmissionHeld(ctx context.Context, stageID uuid.UUID) context.Context {
	outer, _ := ctx.Value(stageAdmissionHeldKey{}).(*stageAdmissionHeld)
	return context.WithValue(ctx, stageAdmissionHeldKey{}, &stageAdmissionHeld{stageID: stageID, outer: outer})
}

// holdStageAdmission takes the per-stage orchestrator.LockStageAdmission fence
// host dispatch holds (#1936) and returns a context carrying the marker plus
// the unlock. It is RE-ENTRANT per request: when ctx already carries the
// marker for stageID it takes nothing and returns a no-op unlock. That is what
// keeps the rerun's own queue admission from deadlocking: serialize ->
// pumpMergeCandidateQueue -> admitMergeCandidateEntry ->
// beginMergeCandidateVerifyPass runs on the SAME goroutine, and
// mergeCandidateAdmissionContext's context.WithoutCancel keeps the marker.
//
// With no Orchestrator wired it takes nothing (host_dispatch.go takes no lock
// in that case either; a documented residual).
//
// LOCK ORDER. The holder blocks only here, before it holds any other fence.
// While it holds stage S's fence the queue pump on its goroutine can admit
// ANOTHER run's entry (the dropped-base pump in serializeMergeCandidateVerifyPass
// admits whatever heads that base's line), so a stage-S holder CAN reach
// begin's arm for a different stage T. That arm therefore never blocks
// unboundedly: it takes T's fence through tryHoldStageAdmission, bounded by
// stageAdmissionTryWait, and answers a transient refusal (the admission
// returns the entry to held) when it is not acquired. A future path that
// re-enters the arm for the SAME stage on a context NOT derived from the
// holder's while the holder waits on it synchronously would still deadlock
// past that bound's refusal; no such path exists.
func (s *Server) holdStageAdmission(ctx context.Context, stageID uuid.UUID) (context.Context, func()) {
	if s.cfg.Orchestrator == nil || stageAdmissionHeldBy(ctx, stageID) {
		return ctx, func() {}
	}
	unlock := s.cfg.Orchestrator.LockStageAdmission(stageID)
	return withStageAdmissionHeld(ctx, stageID), unlock
}

// tryHoldStageAdmission is holdStageAdmission with a BOUNDED wait (approval
// condition C1). It reports false when the fence was not acquired within wait
// (or ctx ended first); the abandoned acquisition releases the fence the
// moment it gets it, so nothing leaks past the current holder.
func (s *Server) tryHoldStageAdmission(ctx context.Context, stageID uuid.UUID, wait time.Duration) (context.Context, func(), bool) {
	if s.cfg.Orchestrator == nil || stageAdmissionHeldBy(ctx, stageID) {
		return ctx, func() {}, true
	}
	got := make(chan func(), 1)
	orch := s.cfg.Orchestrator
	go func() { got <- orch.LockStageAdmission(stageID) }()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case unlock := <-got:
		return withStageAdmissionHeld(ctx, stageID), unlock, true
	case <-timer.C:
	case <-ctx.Done():
	}
	go func() { (<-got)() }()
	return ctx, func() {}, false
}

// --- the eligibility predicate ---

// queuedMergeCandidateFixup is a queued, never-dispatched D6 fix-up the
// re-run may supersede.
type queuedMergeCandidateFixup struct {
	// Trigger is the stage's newest stage_fixup_triggered row.
	Trigger *audit.Entry
	// FailedSequence is the failed merge_candidate_verified row's sequence.
	FailedSequence int64
	// PriorState and ReparkedReviewStageID are the trigger's restore anchors.
	PriorState            string
	ReparkedReviewStageID string
}

// fixupTriggerAnchors decodes the stage_fixup_triggered fields the predicate
// reads; writeFixupAudit writes the route label at fields["delegated"].
type fixupTriggerAnchors struct {
	Delegated             string `json:"delegated"`
	PriorState            string `json:"prior_state"`
	ReparkedReviewStageID string `json:"reparked_review_stage_id"`
}

// decodeD6FixupTrigger decodes a fix-up trigger and reports a refusal when it
// is not a restorable D6 route. "" means it is one.
func decodeD6FixupTrigger(t *audit.Entry) (fixupTriggerAnchors, string) {
	var a fixupTriggerAnchors
	if err := json.Unmarshal(t.Payload, &a); err != nil {
		return a, fmt.Sprintf("the queued fix-up's trigger (sequence %d) is undecodable", t.Sequence)
	}
	if a.Delegated != mergeCandidateDelegatedRule {
		return a, fmt.Sprintf("the queued fix-up (sequence %d) is not the ADR-090 D6 merge-candidate route (delegated %q): an operator-routed fix-up is never superseded; dispatch it with fishhawk_dispatch_stage", t.Sequence, a.Delegated)
	}
	if a.PriorState != string(run.StageStateSucceeded) && a.PriorState != string(run.StageStateAwaitingApproval) {
		return a, fmt.Sprintf("the queued fix-up's restore anchor prior_state %q (sequence %d) is not a review-gate state, so the pass could not restore the gate", a.PriorState, t.Sequence)
	}
	if a.ReparkedReviewStageID != "" {
		if _, err := uuid.Parse(a.ReparkedReviewStageID); err != nil {
			return a, fmt.Sprintf("the queued fix-up's reparked_review_stage_id %q (sequence %d) is unparseable, so the pass could not restore the gate", a.ReparkedReviewStageID, t.Sequence)
		}
	}
	return a, ""
}

// newestRecordedVerdict returns the newest merge_candidate_verified row for
// EXACTLY head (the scan voidFailedMergeCandidateVerdict makes), or nil.
func (s *Server) newestRecordedVerdict(ctx context.Context, runID uuid.UUID, head string) (*audit.Entry, mergeCandidateVerifiedPayload, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryMergeCandidateVerified)
	if err != nil {
		return nil, mergeCandidateVerifiedPayload{}, err
	}
	var newest *audit.Entry
	var payload mergeCandidateVerifiedPayload
	for _, e := range entries {
		var p mergeCandidateVerifiedPayload
		if json.Unmarshal(e.Payload, &p) != nil || p.HeadSHA != head {
			continue
		}
		newest, payload = e, p
	}
	return newest, payload, nil
}

// firstStageRowAfter returns the earliest stage-bound row in categories
// sequenced after seq, skipping the row whose sequence is skip (0 = none).
func (s *Server) firstStageRowAfter(ctx context.Context, runID, stageID uuid.UUID, seq, skip int64, categories []string) (*audit.Entry, error) {
	var first *audit.Entry
	for _, c := range categories {
		rows, err := s.stageEntriesStrict(ctx, runID, stageID, c)
		if err != nil {
			return nil, err
		}
		for _, e := range rows {
			if e.Sequence > seq && e.Sequence != skip && (first == nil || e.Sequence < first.Sequence) {
				first = e
			}
		}
	}
	return first, nil
}

// slotRefusal reads the authoritative concurrency slot store for the stage and
// answers a refusal when it holds a queued or held row. A nil store means host
// dispatch runs the bare CAS with no slot queue; nothing to refuse.
func (s *Server) slotRefusal(ctx context.Context, stageID uuid.UUID) (string, error) {
	if s.cfg.Concurrency == nil {
		return "", nil
	}
	st, err := s.cfg.Concurrency.StatusForStages(ctx, []uuid.UUID{stageID})
	if err != nil {
		return "", fmt.Errorf("the concurrency slot store could not be read: %w", err)
	}
	row, ok := st[stageID]
	if !ok {
		return "", nil
	}
	if row.State == concurrency.SlotHeld {
		return fmt.Sprintf("the stage holds a concurrency slot in group %s, so it is already dispatched", row.GroupKey), nil
	}
	return fmt.Sprintf("a host is waiting for a slot in group %s at position %d (waiter live %v), so the fix-up is already being dispatched", row.GroupKey, row.Position, row.WaiterLive), nil
}

// queuedMergeCandidateFixupFor is the SINGLE eligibility predicate the verb
// (rerunQueuedMergeCandidateFixup) and the run-read advertisement call. impl
// must be re-read by the caller (under the fence, for the verb). It answers
// the fix-up, or a refusal naming the specific reason, or a read error (fail
// closed). Eligible means:
//
//	(a) the stage is pending or awaiting_host_dispatch;
//	(b) the newest merge_candidate_verified row for EXACTLY p.HeadSHA is failed
//	    (sequence F);
//	(c) the stage's newest stage_fixup_triggered T is the D6 route
//	    (delegated == merge_candidate_verify_failed), sequenced after F, with a
//	    review-gate prior_state and a parseable reparked_review_stage_id;
//	(d) no queuedFixupDisqualifyingCategories row on the stage follows T;
//	(e) the concurrency slot store holds no queued or held row for the stage;
//	(f) the ADR-092 queue would not hold the pass behind another run.
func (s *Server) queuedMergeCandidateFixupFor(ctx context.Context, runRow *run.Run, impl *run.Stage,
	p mergeCandidateVerifyParams, actor mergeCandidateActor) (*queuedMergeCandidateFixup, string, error) {
	if s.cfg.AuditRepo == nil {
		return nil, "no audit repository is wired", nil
	}
	if impl.State != run.StageStatePending && impl.State != run.StageStateAwaitingHostDispatch {
		return nil, fmt.Sprintf("the implement stage is in state %q, not a queued fix-up (pending or awaiting_host_dispatch)", impl.State), nil
	}
	failed, verdict, err := s.newestRecordedVerdict(ctx, runRow.ID, p.HeadSHA)
	if err != nil {
		return nil, "", err
	}
	if failed == nil || verdict.Result != mergeCandidateResultFailed {
		if failed != nil && verdict.VoidsSequence > 0 {
			return nil, fmt.Sprintf("the failed verdict was already voided (sequence %d): a verify-only pass is already started or startable for this head", failed.Sequence), nil
		}
		return nil, "the head's newest recorded merge-candidate verdict is not failed", nil
	}
	triggers, err := s.stageEntriesStrict(ctx, runRow.ID, impl.ID, CategoryStageFixupTriggered)
	if err != nil {
		return nil, "", err
	}
	if len(triggers) == 0 {
		return nil, "no fix-up is queued on the stage", nil
	}
	t := triggers[len(triggers)-1]
	anchors, refusal := decodeD6FixupTrigger(t)
	if refusal != "" {
		return nil, refusal, nil
	}
	if t.Sequence <= failed.Sequence {
		return nil, fmt.Sprintf("the queued D6 fix-up (sequence %d) is older than the failed verdict (sequence %d), so it was not routed for this failure", t.Sequence, failed.Sequence), nil
	}
	after, err := s.firstStageRowAfter(ctx, runRow.ID, impl.ID, t.Sequence, 0, queuedFixupDisqualifyingCategories)
	if err != nil {
		return nil, "", err
	}
	if after != nil {
		return nil, fmt.Sprintf("a %s row (sequence %d) follows the fix-up trigger (sequence %d): the fix-up was already dispatched, settled, retried or superseded, or another pass re-opened the stage", after.Category, after.Sequence, t.Sequence), nil
	}
	if refusal, err := s.slotRefusal(ctx, impl.ID); err != nil || refusal != "" {
		return nil, refusal, err
	}
	if holder, held, err := s.mergeCandidateRerunWouldHold(ctx, runRow, p, actor); err != nil {
		return nil, "", err
	} else if held {
		return nil, "the ADR-092 merge-candidate queue would hold the pass behind run " + holder.String() + "'s active entry on " + p.BaseRef + "; re-run once that run's pass settles", nil
	}
	return &queuedMergeCandidateFixup{Trigger: t, FailedSequence: failed.Sequence,
		PriorState: anchors.PriorState, ReparkedReviewStageID: anchors.ReparkedReviewStageID}, "", nil
}

// mergeCandidateQueueRoutes reports whether startMergeCandidateVerifyPass
// routes p through the ADR-092 held-pass queue rather than a direct start.
func (s *Server) mergeCandidateQueueRoutes(p mergeCandidateVerifyParams, actor mergeCandidateActor) bool {
	return s.cfg.MergeQueue != nil && s.cfg.GitHub != nil && s.cfg.RunRepo != nil && s.cfg.AuditRepo != nil &&
		mergeCandidateQueueSerialized(p.Cause) && p.Branch != "" && p.BaseRef != "" && p.HeadSHA != "" && actor.Subject != ""
}

// mergeCandidateRerunWouldHold is the held-pass pre-check: when the queue
// routes p, it reports held when ANY active entry on the base belongs to
// ANOTHER run (a live one holds the line; a held one is ahead of a fresh tail
// insert). A read error is returned (fail closed). The check is TOCTOU: a run
// going live after it can still hold the pass (the README's held-coexistence
// decision).
func (s *Server) mergeCandidateRerunWouldHold(ctx context.Context, runRow *run.Run, p mergeCandidateVerifyParams,
	actor mergeCandidateActor) (uuid.UUID, bool, error) {
	if !s.mergeCandidateQueueRoutes(p, actor) {
		return uuid.Nil, false, nil
	}
	entries, err := s.cfg.MergeQueue.ListForBase(ctx, mergequeue.Base{AccountID: runRow.AccountID, Repo: runRow.Repo, BaseRef: p.BaseRef})
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("the merge-candidate queue could not be read: %w", err)
	}
	for _, e := range entries {
		if e.RunID != runRow.ID && e.State.Active() {
			return e.RunID, true, nil
		}
	}
	return uuid.Nil, false, nil
}

// --- the verb arm ---

// rerunQueuedMergeCandidateFixup is rerun_failed_verify on an implement stage
// that is pending or awaiting_host_dispatch behind a queued D6 fix-up. In
// order, all under the stage's admission fence (held until the start
// returns):
//
//  1. re-read the stage and run queuedMergeCandidateFixupFor; a refusal or
//     read error answers with NOTHING written;
//  2. append ONE stage_fixup_superseded marker;
//  3. voidFailedMergeCandidateVerdict (unchanged);
//  4. startMergeCandidateVerifyPass (unchanged), whose begin reaches the
//     SUPERSEDED-FIX-UP arm and writes the conversion trigger.
//
// Each partial failure says exactly what landed (README § "Operator
// verify-only re-run of a failed merge-candidate head (#4391)").
func (s *Server) rerunQueuedMergeCandidateFixup(ctx context.Context, runRow *run.Run, impl *run.Stage,
	branch, baseRef, head, cause, reason string) rebaseMergeCandidateFields {
	ctx, unlock := s.holdStageAdmission(ctx, impl.ID)
	defer unlock()
	refuse := func(state, why string) rebaseMergeCandidateFields {
		return rebaseMergeCandidateFields{State: mergeCandidateStateFailed, Refusal: fmt.Sprintf(
			"rerun_failed_verify refused: implement stage %s (state %q): %s; NOTHING was written — the failed merge-candidate verdict for %s was NOT voided and no pass was started",
			impl.ID, state, why, head)}
	}
	stage, err := s.cfg.RunRepo.GetStage(ctx, impl.ID)
	if err != nil {
		return refuse(string(impl.State), "the stage could not be re-read ("+err.Error()+")")
	}
	actor := rebaseOperatorActor(ctx)
	p := mergeCandidateVerifyParams{Branch: branch, BaseRef: baseRef, HeadSHA: head, Cause: cause}
	q, why, err := s.queuedMergeCandidateFixupFor(ctx, runRow, stage, p, actor)
	if err != nil {
		return refuse(string(stage.State), "the queued fix-up's eligibility could not be read ("+err.Error()+")")
	}
	if why != "" {
		return refuse(string(stage.State), why)
	}

	markerSeq, err := s.appendFixupSupersededMarker(ctx, runRow.ID, stage.ID, q, actor, reason)
	if err != nil {
		return refuse(string(stage.State), "recording the stage_fixup_superseded marker failed ("+err.Error()+"); the fix-up is still queued and dispatchable")
	}
	if err := s.voidFailedMergeCandidateVerdict(ctx, runRow.ID, head, actor, reason); err != nil {
		return rebaseMergeCandidateFields{State: mergeCandidateStateFailed, Refusal: fmt.Sprintf(
			"rerun_failed_verify: the supersede marker landed (sequence %d) but the failed verdict for %s was NOT voided (%s): the fix-up is still queued and dispatchable as a fix-up (dispatch it with fishhawk_dispatch_stage); a re-run is refused on this fix-up from now on because its marker is spent",
			markerSeq, head, err.Error())}
	}
	start, refusal := s.startMergeCandidateVerifyPass(ctx, runRow.ID, p, actor)
	switch {
	case refusal == nil && start.State == mergeCandidateStateInFlight && start.SupersededFixupSequence == q.Trigger.Sequence:
		return rebaseMergeCandidateFields{State: mergeCandidateStateInFlight, Triggered: start.Triggered,
			StageID: start.StageID.String(), Rerun: true, FixupSuperseded: true,
			Note: fmt.Sprintf("The queued merge-candidate fix-up (sequence %d) was superseded in place by a verify-only pass for %s: the implement stage keeps its place (%s), its next dispatch serves the verify-only pass in the local-verify group, and the superseded fix-up pass is refunded. ",
				q.Trigger.Sequence, head, stage.State) + mergeCandidateVerifyTriggeredNote}
	case refusal == nil && start.State == mergeCandidateStateHeld:
		holder := "another run"
		if start.LiveRunID != nil {
			holder = "run " + start.LiveRunID.String()
		}
		return rebaseMergeCandidateFields{State: mergeCandidateStateHeld, Rerun: true, Note: fmt.Sprintf(
			"The supersede marker and the void landed, and the verify-only pass is HELD behind %s on the merge-candidate queue (position %d). The fix-up is NOT yet superseded and is still dispatchable as a fix-up; dispatching it abandons the supersession. When the queue admits this run, its admission converts the fix-up in place.",
			holder, start.Position)}
	}
	startReason := "the pass answered state " + mergeCandidateStateUnverified
	if refusal != nil {
		startReason = refusal.Reason
	} else if start != nil {
		startReason = "the pass answered state " + start.State
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
		"branch rebase: queued fix-up superseded and verdict voided, but no conversion trigger was recorded",
		slog.String("run_id", runRow.ID.String()),
		slog.String("head_sha", head),
		slog.String("reason", startReason))
	return rebaseMergeCandidateFields{State: mergeCandidateStateUnverified, Rerun: true, Refusal: startReason +
		"; the marker and the void landed but no conversion trigger: the stage is still " + string(stage.State) +
		" and the fix-up is still dispatchable as a fix-up - re-invoke fishhawk_rebase_run_branch (no flag needed) to convert it"}
}

// appendFixupSupersededMarker appends the stage_fixup_superseded row and
// returns its chain sequence. NOT best-effort: the caller refuses on an error.
func (s *Server) appendFixupSupersededMarker(ctx context.Context, runID, stageID uuid.UUID,
	q *queuedMergeCandidateFixup, actor mergeCandidateActor, reason string) (int64, error) {
	raw, err := json.Marshal(stageFixupSupersededPayload{
		TriggerSequence:       q.Trigger.Sequence,
		SupersededBy:          fixupSupersededByVerifyRerun,
		FailedVerdictSequence: q.FailedSequence,
		Reason:                mergeCandidateRerunReasonPrefix + reason,
	})
	if err != nil {
		return 0, err
	}
	kind, subject := actor.Kind, actor.Subject
	e, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryStageFixupSuperseded,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      raw,
	})
	if err != nil {
		return 0, err
	}
	if e != nil && e.Sequence > 0 {
		return e.Sequence, nil
	}
	// A repository that does not report the appended sequence: read the newest
	// marker back (best effort; 0 when unreadable, used only in prose).
	if rows, rerr := s.stageEntriesStrict(ctx, runID, stageID, CategoryStageFixupSuperseded); rerr == nil && len(rows) > 0 {
		return rows[len(rows)-1].Sequence, nil
	}
	return 0, nil
}

// --- begin's SUPERSEDED-FIX-UP arm ---

// supersededFixupAwaitingConversion is the unlocked pre-read: the stage's
// newest stage_fixup_triggered T is the D6 route and a stage_fixup_superseded
// marker naming T follows it. It returns T and the newest such marker, or nil.
func (s *Server) supersededFixupAwaitingConversion(ctx context.Context, runID, stageID uuid.UUID) (*audit.Entry, *audit.Entry, error) {
	triggers, err := s.stageEntriesStrict(ctx, runID, stageID, CategoryStageFixupTriggered)
	if err != nil || len(triggers) == 0 {
		return nil, nil, err
	}
	t := triggers[len(triggers)-1]
	var a fixupTriggerAnchors
	if json.Unmarshal(t.Payload, &a) != nil || a.Delegated != mergeCandidateDelegatedRule {
		return nil, nil, nil
	}
	markers, err := s.stageEntriesStrict(ctx, runID, stageID, CategoryStageFixupSuperseded)
	if err != nil {
		return nil, nil, err
	}
	var marker *audit.Entry
	for _, m := range markers {
		var mp stageFixupSupersededPayload
		if m.Sequence > t.Sequence && json.Unmarshal(m.Payload, &mp) == nil && mp.TriggerSequence == t.Sequence {
			marker = m
		}
	}
	if marker == nil {
		return nil, nil, nil
	}
	return t, marker, nil
}

// beginSupersededFixupPass is beginMergeCandidateVerifyPass's
// SUPERSEDED-FIX-UP arm, reached with no live trigger on a pending or
// awaiting_host_dispatch stage. It is handled only when the stage's newest
// stage_fixup_triggered T is the D6 route and a stage_fixup_superseded marker
// naming T follows it; otherwise begin falls through to its prior-state
// refusal unchanged. Handled, it takes the stage's admission fence (a no-op
// inside the rerun; BOUNDED otherwise, approval condition C1, answering a
// TRANSIENT refusal when not acquired), re-reads, and refuses with nothing
// written when the stage left pending/awaiting_host_dispatch, a conversion
// trigger already follows the marker, any other disqualifying row follows T
// (the marker is then INERT), or the slot store holds a row. Otherwise it
// appends ONE stage_merge_candidate_verify_triggered carrying T's restore
// anchors and supersedes_fixup_trigger_sequence = T. It does NOT call
// run.FixupStage: the stage is already re-opened and keeps its place.
func (s *Server) beginSupersededFixupPass(ctx context.Context, runID uuid.UUID, impl *run.Stage,
	p mergeCandidateVerifyParams, command string, actor mergeCandidateActor) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal, bool) {
	t, _, err := s.supersededFixupAwaitingConversion(ctx, runID, impl.ID)
	if err != nil {
		return nil, &mergeCandidateVerifyRefusal{Reason: "the superseded fix-up chain could not be read (" + err.Error() + "); refusing to authorize a pass on an uncertain state"}, true
	}
	if t == nil {
		return nil, nil, false
	}
	ctx, unlock, ok := s.tryHoldStageAdmission(ctx, impl.ID, stageAdmissionTryWait)
	defer unlock()
	if !ok {
		return nil, &mergeCandidateVerifyRefusal{Transient: true, Reason: fmt.Sprintf(
			"the implement stage %s's admission fence is held by another request (a host dispatch, or another stage's admission on this goroutine), so the superseded fix-up was not converted yet; nothing was written and the conversion is retried on the next start for this head", impl.ID)}, true
	}
	refuse := func(why string) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal, bool) {
		return nil, &mergeCandidateVerifyRefusal{Reason: "the superseded merge-candidate fix-up was not converted: " + why + "; no conversion trigger was recorded"}, true
	}
	stage, err := s.cfg.RunRepo.GetStage(ctx, impl.ID)
	if err != nil {
		return refuse("the stage could not be re-read (" + err.Error() + ")")
	}
	if stage.State != run.StageStatePending && stage.State != run.StageStateAwaitingHostDispatch {
		return refuse(fmt.Sprintf("the implement stage left its queued state (now %q)", stage.State))
	}
	t, marker, err := s.supersededFixupAwaitingConversion(ctx, runID, impl.ID)
	if err != nil {
		return refuse("the superseded fix-up chain could not be read (" + err.Error() + ")")
	}
	if t == nil {
		return refuse("the supersede marker no longer names the stage's newest D6 fix-up")
	}
	anchors, why := decodeD6FixupTrigger(t)
	if why != "" {
		return refuse(why)
	}
	converted, err := s.firstStageRowAfter(ctx, runID, impl.ID, marker.Sequence, 0, []string{CategoryStageMergeCandidateVerifyTriggered})
	if err != nil {
		return refuse("the conversion chain could not be read (" + err.Error() + ")")
	}
	if converted != nil {
		return refuse(fmt.Sprintf("a stage_merge_candidate_verify_triggered row (sequence %d) already follows the marker (sequence %d): the fix-up was already converted", converted.Sequence, marker.Sequence))
	}
	after, err := s.firstStageRowAfter(ctx, runID, impl.ID, t.Sequence, marker.Sequence, queuedFixupDisqualifyingCategories)
	if err != nil {
		return refuse("the queued fix-up's chain could not be read (" + err.Error() + ")")
	}
	if after != nil {
		return refuse(fmt.Sprintf("a %s row (sequence %d) follows the fix-up trigger (sequence %d), so the marker (sequence %d) is INERT: the fix-up was dispatched, settled, retried or re-opened",
			after.Category, after.Sequence, t.Sequence, marker.Sequence))
	}
	if why, err := s.slotRefusal(ctx, impl.ID); err != nil {
		return refuse(err.Error())
	} else if why != "" {
		return refuse(why)
	}
	trigger := mergeCandidateVerifyTrigger{
		Branch:                         p.Branch,
		BaseRef:                        p.BaseRef,
		ExpectedHeadSHA:                p.HeadSHA,
		Cause:                          p.Cause,
		VerifyCommand:                  command,
		PriorState:                     anchors.PriorState,
		ReparkedReviewStageID:          anchors.ReparkedReviewStageID,
		SupersedesFixupTriggerSequence: t.Sequence,
	}
	if err := s.appendMergeCandidateTrigger(ctx, runID, impl.ID, trigger, actor); err != nil {
		return refuse("recording the conversion trigger failed (" + err.Error() + "); the stage is unchanged")
	}
	s.notifyStatusUpdate(ctx, runID, "merge_candidate_verify_start")
	return &mergeCandidateVerifyStart{State: mergeCandidateStateInFlight, StageID: impl.ID, Triggered: true,
		SupersededFixupSequence: t.Sequence}, nil, true
}

// --- the reader rule ---

// fixupSupersessionKey names one superseded fix-up trigger on one stage.
type fixupSupersessionKey struct {
	StageID         uuid.UUID
	TriggerSequence int64
}

// confirmedFixupSupersessions is the ONE reader rule for stage_fixup_superseded
// markers. A marker M confirms iff it decodes a positive trigger_sequence T
// and a stage_merge_candidate_verify_triggered row V on the SAME stage decodes
// supersedes_fixup_trigger_sequence == T with V.Sequence > M.Sequence. The
// value is M's sequence. Undecodable rows are skipped.
//
// The verify-only pass's own dispatch follows the marker, so "no later
// dispatch" cannot be the reader rule. A V carrying the field is written ONLY
// by beginSupersededFixupPass after it proved, under the fence, that nothing
// disqualifying follows the marker; so an inert marker (no V names it) is never
// a supersession, and rows after V belong to the verify pass.
func confirmedFixupSupersessions(markers, verifyTriggers []*audit.Entry) map[fixupSupersessionKey]int64 {
	out := map[fixupSupersessionKey]int64{}
	for _, m := range markers {
		if m == nil || m.StageID == nil {
			continue
		}
		var mp stageFixupSupersededPayload
		if json.Unmarshal(m.Payload, &mp) != nil || mp.TriggerSequence <= 0 {
			continue
		}
		for _, v := range verifyTriggers {
			if v == nil || v.StageID == nil || *v.StageID != *m.StageID || v.Sequence <= m.Sequence {
				continue
			}
			var vp mergeCandidateVerifyTrigger
			if json.Unmarshal(v.Payload, &vp) != nil || vp.SupersedesFixupTriggerSequence != mp.TriggerSequence {
				continue
			}
			out[fixupSupersessionKey{StageID: *m.StageID, TriggerSequence: mp.TriggerSequence}] = m.Sequence
			break
		}
	}
	return out
}
