package server

// ADR-093 D8 (E87.7 / #4298), slice 1: the restart resume's VERIFYING
// recovery and the helpers every resume step shares.
//
// A live head-of-line entry records its LAST RECORDED step in its phase (not
// a liveness signal). After a restart the pump resumes each live entry from
// that step; this file owns the verifying/unverified step and three shared
// pieces:
//
//   - merge_queue_resumed, the resume's record of what it did. Every
//     state-changing action appends its row BEFORE acting and FAILS CLOSED:
//     an append error means nothing is changed (appendMergeQueueResumed).
//     Only a hold is deduped, so a held entry re-decided every pump tick
//     writes one row; a state-changing action always writes its own.
//   - mergeQueueRunStageLive, the ADR-115 D4 / ruling R7 gate: no re-claimed
//     admission and no re-dispatch starts work while a stage of the run is
//     dispatched or running or a merge-candidate pass is triggered.
//   - mergeQueueResumeVerifying, the verifying recovery: it applies a
//     verdict whose queue hook (mergeCandidateQueueOnVerdict) a restart lost,
//     and re-dispatches a runner_failed pass at most once.
//
// It reads verdict ROWS (newestMergeCandidateVerdict), never the derived
// not_required state, so a live entry with no verdict row is never moved. The
// decision is made unlocked, then re-derived under the run's merge-intent lock
// (withRunMergeLock) against a re-read entry before anything is written. No
// settle that pumps runs inside that lock (the next entry's merge-on-pass
// would meet pglock.ErrNestedLock): the eject runs after the lock returns.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryMergeQueueResumed is the restart resume's record (actor system,
// INTERNAL: not an issue-comment surface). Its payload names only THIS run's
// own entry, claim and head, so it is run-bound-readable under ADR-106; the
// audit registry carries no ADR-106 flag yet, so none is set (E89 classifies
// it when the flag lands).
const CategoryMergeQueueResumed = "merge_queue_resumed"

// The closed merge_queue_resumed action vocabulary.
const (
	mergeQueueResumeActionReclaim          = "reclaim"
	mergeQueueResumeActionApplyVerdict     = "apply_verdict"
	mergeQueueResumeActionEject            = "eject"
	mergeQueueResumeActionRecordUnverified = "record_unverified"
	mergeQueueResumeActionRedispatchPass   = "redispatch_pass"
	mergeQueueResumeActionSettleMerged     = "settle_merged"
	mergeQueueResumeActionSettlePRMerged   = "settle_pr_merged"
	mergeQueueResumeActionDropPRClosed     = "drop_pr_closed"
	mergeQueueResumeActionRedecide         = "redecide"
	mergeQueueResumeActionHold             = "hold"
)

// mergeQueueResumeActions is the closed vocabulary as a set: an action outside
// it is refused before any row is written.
var mergeQueueResumeActions = map[string]bool{
	mergeQueueResumeActionReclaim:          true,
	mergeQueueResumeActionApplyVerdict:     true,
	mergeQueueResumeActionEject:            true,
	mergeQueueResumeActionRecordUnverified: true,
	mergeQueueResumeActionRedispatchPass:   true,
	mergeQueueResumeActionSettleMerged:     true,
	mergeQueueResumeActionSettlePRMerged:   true,
	mergeQueueResumeActionDropPRClosed:     true,
	mergeQueueResumeActionRedecide:         true,
	mergeQueueResumeActionHold:             true,
}

// The verifying recovery's merge_queue_resumed reasons.
const (
	// mergeQueueResumeReasonLostPassedHook: a passed verdict (recorded or
	// enforce-transferred) at the anchored head whose queue hook was lost.
	mergeQueueResumeReasonLostPassedHook = "lost_passed_hook"
	// mergeQueueResumeReasonLostFailedHook: a failed verdict at the anchored
	// head whose queue hook was lost.
	mergeQueueResumeReasonLostFailedHook = "lost_failed_hook"
	// mergeQueueResumeReasonLostNotExecutedHook: a not_executed verdict at the
	// anchored head left the entry in phase verifying.
	mergeQueueResumeReasonLostNotExecutedHook = "lost_not_executed_hook"
	// mergeQueueResumeReasonRunnerFailed: the head's ONE runner_failed
	// not_executed verdict is re-dispatched (at most once).
	mergeQueueResumeReasonRunnerFailed = "runner_failed"
)

// mergeCandidateRunnerFailedReasonPrefix leads the not_executed reason
// maybeRecoverMergeCandidateVerifyFailure records for a crashed pass.
const mergeCandidateRunnerFailedReasonPrefix = "runner_failed:"

// mergeQueueResumeLostHookRefusal is the eject's D6 refusal: the verdict's
// queue hook was lost, so no delegated fix-up route is RECORDED for it and the
// row fails closed to fixup_routed=false (ruling R8b).
const mergeQueueResumeLostHookRefusal = "the verdict's queue hook was lost to a restart, so no delegated fix-up route is recorded for it. " + mergeCandidateFixupRoute

// mergeQueueResumed is one merge_queue_resumed row. Step is the entry's
// phase the decision read; Action is from the closed vocabulary. Every other
// field is optional and omitted when empty. Extra carries further fields a
// later resume step records (e.g. its merged-tail references); a key that
// collides with a named field is ignored, so a caller cannot overwrite the
// row's identity.
type mergeQueueResumed struct {
	Step         string
	Action       string
	Reason       string
	NextStep     string
	Remedy       string
	ClaimState   string
	ClaimPath    string
	ClaimHeadSHA string
	Extra        map[string]any
}

// mergeQueueResumedParams builds the merge_queue_resumed append for entry e:
// the payload {entry_id, base_ref, step, action, reason?, head_sha,
// admission_epoch, next_step?, remedy?, claim_state?, claim_path?,
// claim_head_sha?} under the system actor on e's run. Every id in it is e's
// own. It is shared by appendMergeQueueResumed and the in-transaction reclaim
// record (audit.AppendChainedTx on the queue transaction).
func mergeQueueResumedParams(e mergequeue.Entry, r mergeQueueResumed) (audit.ChainAppendParams, error) {
	if !mergeQueueResumeActions[r.Action] {
		return audit.ChainAppendParams{}, fmt.Errorf("merge_queue_resumed: action %q is not in the closed vocabulary", r.Action)
	}
	if r.Step == "" {
		return audit.ChainAppendParams{}, errors.New("merge_queue_resumed: step is required")
	}
	fields := map[string]any{}
	for k, v := range r.Extra {
		fields[k] = v
	}
	fields["entry_id"] = e.ID.String()
	fields["base_ref"] = e.BaseRef
	fields["step"] = r.Step
	fields["action"] = r.Action
	fields["head_sha"] = e.AnchoredHeadSHA
	fields["admission_epoch"] = e.AdmissionEpoch
	for k, v := range map[string]string{
		"reason": r.Reason, "next_step": r.NextStep, "remedy": r.Remedy,
		"claim_state": r.ClaimState, "claim_path": r.ClaimPath, "claim_head_sha": r.ClaimHeadSHA,
	} {
		delete(fields, k)
		if v != "" {
			fields[k] = v
		}
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return audit.ChainAppendParams{}, fmt.Errorf("merge_queue_resumed: encode payload: %w", err)
	}
	kind, subject := audit.ActorSystem, mergeCandidateSystemSubject
	return audit.ChainAppendParams{
		RunID:        e.RunID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryMergeQueueResumed,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	}, nil
}

// appendMergeQueueResumed appends one merge_queue_resumed row for e and
// reports whether it appended. It is FAIL-CLOSED (ruling R8d): any error is
// returned, and every caller then holds with nothing changed. ONLY a hold is
// deduped (approval condition C7): it is skipped when the run's newest
// merge_queue_resumed row already carries the same entry_id, step, action and
// reason, so a held entry re-decided every pump tick writes one row. A
// state-changing action always appends its own row before it acts.
func (s *Server) appendMergeQueueResumed(ctx context.Context, e mergequeue.Entry, r mergeQueueResumed) (bool, error) {
	if s.cfg.AuditRepo == nil {
		return false, errors.New("merge_queue_resumed: no audit repository is wired")
	}
	p, err := mergeQueueResumedParams(e, r)
	if err != nil {
		return false, err
	}
	if r.Action == mergeQueueResumeActionHold {
		rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, e.RunID, CategoryMergeQueueResumed)
		if err != nil {
			return false, fmt.Errorf("merge_queue_resumed: read the newest row: %w", err)
		}
		if len(rows) > 0 && mergeQueueResumedSameHold(rows[len(rows)-1].Payload, e, r) {
			return false, nil
		}
	}
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, p); err != nil {
		return false, err
	}
	return true, nil
}

// mergeQueueResumedSameHold reports whether raw (the newest row's payload)
// is the same hold: entry_id, step, action and reason. An undecodable row is
// not the same.
func mergeQueueResumedSameHold(raw []byte, e mergequeue.Entry, r mergeQueueResumed) bool {
	var p struct {
		EntryID string `json:"entry_id"`
		Step    string `json:"step"`
		Action  string `json:"action"`
		Reason  string `json:"reason"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return false
	}
	return p.EntryID == e.ID.String() && p.Step == r.Step && p.Action == r.Action && p.Reason == r.Reason
}

// mergeQueueRunStageLive is the ADR-115 D4 / ruling R7 gate: the run is live
// when any of its stages is dispatched or running, OR its implement stage
// carries a LIVE merge-candidate verify trigger. The trigger leg counts a pass
// beginMergeCandidateVerifyPass just started while its stage is still pending
// or awaiting host dispatch. A read error counts as live (fail closed); why
// names the reason.
func (s *Server) mergeQueueRunStageLive(ctx context.Context, runID uuid.UUID) (bool, string) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return true, "no run or audit repository is wired, so the run's stages cannot be read"
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return true, "the run's stages could not be read: " + err.Error()
	}
	var impl *run.Stage
	for _, st := range stages {
		if st.State == run.StageStateDispatched || st.State == run.StageStateRunning {
			return true, "the " + string(st.Type) + " stage is " + string(st.State)
		}
		if impl == nil && st.Type == run.StageTypeImplement {
			impl = st
		}
	}
	if impl == nil {
		return false, ""
	}
	live, err := s.liveMergeCandidateTrigger(ctx, runID, impl.ID)
	if err != nil {
		return true, "the merge-candidate verify chain could not be read: " + err.Error()
	}
	if live != nil {
		return true, "a merge-candidate verify pass is triggered for " + live.payload.ExpectedHeadSHA
	}
	return false, ""
}

// mergeQueueVerifyingDecision is what the verifying recovery decided from one
// read of an entry's evidence. action "" holds with nothing written.
type mergeQueueVerifyingDecision struct {
	action string
	reason string
	// source is apply_verdict's merge_queue_verified source (pass/transfer).
	source string
	// redispatch: the not_executed arm met every re-dispatch precondition the
	// database decides (intent, one runner_failed verdict, no live stage or
	// trigger, implement stage at its review gate); the PR head is checked
	// in-lock.
	redispatch bool
	// why explains a hold (logs only).
	why string
}

// decideMergeQueueResumeVerifying reads the evidence for a live verifying or
// unverified entry at its anchored head and decides. It reads verdict ROWS
// only. A read error is returned (the caller holds).
func (s *Server) decideMergeQueueResumeVerifying(ctx context.Context, e mergequeue.Entry) (mergeQueueVerifyingDecision, error) {
	head := e.AnchoredHeadSHA
	verdict, err := s.newestMergeCandidateVerdict(ctx, e.RunID, head)
	if err != nil {
		return mergeQueueVerifyingDecision{}, err
	}
	if verdict != nil && verdict.Result == mergeCandidateResultPassed {
		source := mergeQueueVerifiedSourcePass
		if verdict.Source == mergeCandidateSourceTransferred {
			source = mergeQueueVerifiedSourceTransfer
		}
		return mergeQueueVerifyingDecision{action: mergeQueueResumeActionApplyVerdict,
			reason: mergeQueueResumeReasonLostPassedHook, source: source}, nil
	}
	if verdict != nil && verdict.Result == mergeCandidateResultFailed {
		return mergeQueueVerifyingDecision{action: mergeQueueResumeActionEject, reason: mergeQueueResumeReasonLostFailedHook}, nil
	}
	_, triggered, err := s.liveMergeCandidateTriggerForHead(ctx, e.RunID, head)
	if err != nil {
		return mergeQueueVerifyingDecision{}, err
	}
	if triggered {
		// The runner's upload (or the existing reap path) settles it.
		return mergeQueueVerifyingDecision{why: "a merge-candidate verify pass is in flight for the anchored head"}, nil
	}
	if verdict == nil || verdict.Result != mergeCandidateResultNotExecuted {
		return mergeQueueVerifyingDecision{why: "no merge-candidate verdict or trigger names the anchored head"}, nil
	}
	d := mergeQueueVerifyingDecision{why: "the anchored head is unverified; the operator's fishhawk_rebase_run_branch re-triggers it (ADR-092)"}
	if e.Phase == mergequeue.PhaseVerifying {
		d.action, d.reason = mergeQueueResumeActionRecordUnverified, mergeQueueResumeReasonLostNotExecutedHook
	}
	ok, why, err := s.mergeQueueRedispatchEligible(ctx, e, verdict)
	if err != nil {
		return mergeQueueVerifyingDecision{}, err
	}
	d.redispatch = ok
	if !ok {
		d.why = why
	}
	return d, nil
}

// mergeQueueRedispatchEligible decides the database-side re-dispatch
// preconditions for a not_executed newest verdict: the entry carries merge
// intent; the verdict's reason starts with runner_failed:; the head carries
// EXACTLY ONE such runner_failed not_executed verdict (the at-most-once
// bound: the re-dispatched pass failing the same way makes two); the run has
// no live stage or trigger (R7); and the implement stage is succeeded or
// awaiting_approval. why names the first unmet precondition.
func (s *Server) mergeQueueRedispatchEligible(ctx context.Context, e mergequeue.Entry,
	verdict *mergeCandidateVerifiedPayload) (bool, string, error) {
	if e.MergeIntentAt == nil {
		return false, "the entry carries no operator merge intent", nil
	}
	if !strings.HasPrefix(verdict.Reason, mergeCandidateRunnerFailedReasonPrefix) {
		return false, "the not_executed verdict is not a crashed runner's (" + mergeCandidateRunnerFailedReasonPrefix + ")", nil
	}
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, e.RunID, CategoryMergeCandidateVerified)
	if err != nil {
		return false, "", err
	}
	failed := 0
	for _, row := range rows {
		var p mergeCandidateVerifiedPayload
		if json.Unmarshal(row.Payload, &p) != nil || p.HeadSHA != e.AnchoredHeadSHA {
			continue
		}
		if p.Result == mergeCandidateResultNotExecuted && strings.HasPrefix(p.Reason, mergeCandidateRunnerFailedReasonPrefix) {
			failed++
		}
	}
	if failed != 1 {
		return false, fmt.Sprintf("the head carries %d runner_failed not_executed verdicts; the resume re-dispatches only the first", failed), nil
	}
	if live, why := s.mergeQueueRunStageLive(ctx, e.RunID); live {
		return false, why, nil
	}
	impl := s.findImplementStage(ctx, e.RunID)
	if impl == nil {
		return false, "the run has no implement stage", nil
	}
	if impl.State != run.StageStateSucceeded && impl.State != run.StageStateAwaitingApproval {
		return false, "the implement stage is " + string(impl.State) + ", not at its review gate", nil
	}
	return true, "", nil
}

// mergeQueueResumeForgeTarget resolves the run's forge target for a resume
// step's PR read.
func mergeQueueResumeForgeTarget(runRow *run.Run) (mergeOnPassTarget, error) {
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return mergeOnPassTarget{}, errors.New("the run has no installation to read its pull request")
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return mergeOnPassTarget{}, fmt.Errorf("the run repo is unparseable: %w", err)
	}
	number := parsePRNumberFromURL(runRow.PullRequestURL)
	if number <= 0 {
		return mergeOnPassTarget{}, errors.New("the run has no tracked pull request")
	}
	return mergeOnPassTarget{scope: forge.FromGitHubInstallationID(*runRow.InstallationID), repo: repo, number: number}, nil
}

// mergeQueueResumeLiveEntry re-reads the run's entry and reports it when it
// is still e: same id, live, same admission epoch, an allowed phase and the
// same anchored head.
func (s *Server) mergeQueueResumeLiveEntry(ctx context.Context, e mergequeue.Entry, phases ...mergequeue.Phase) (mergequeue.Entry, bool) {
	st, found, err := s.cfg.MergeQueue.ActiveForRun(ctx, e.RunID)
	if err != nil || !found {
		return mergequeue.Entry{}, false
	}
	cur := st.Entry
	if cur.ID != e.ID || cur.State != mergequeue.StateLive || cur.AdmissionEpoch != e.AdmissionEpoch ||
		cur.AnchoredHeadSHA != e.AnchoredHeadSHA {
		return mergequeue.Entry{}, false
	}
	for _, p := range phases {
		if cur.Phase == p {
			return cur, true
		}
	}
	return mergequeue.Entry{}, false
}

// mergeQueueResumeVerifying resumes a LIVE entry whose last recorded step is
// verifying or unverified, and reports whether the pump continues. Decision
// (unlocked, then re-derived in-lock against a re-read entry):
//
//   - passed (recorded or enforce-transferred) at the anchored head →
//     apply_verdict: the SetPhase(passed) CAS plus merge_queue_verified the
//     lost hook would have written; no pump; continue.
//   - failed → eject verify_failed AFTER the lock returns, recording
//     fixup_routed=false and next_step fishhawk_fixup_stage (fail closed,
//     ruling R8b); continue when settled.
//   - a live trigger names the head → hold, nothing written.
//   - not_executed → record_unverified when the phase is verifying, then
//     redispatch_pass ONLY when the entry carries merge intent, the head
//     carries exactly one runner_failed not_executed verdict, the run has no
//     live stage or trigger (R7), the implement stage is at its review gate
//     and the PR's head is the anchored head; otherwise hold for the
//     operator's fishhawk_rebase_run_branch, as ADR-092 does.
//   - no verdict and no trigger → hold, nothing written.
//
// Every state-changing action appends merge_queue_resumed BEFORE it acts; an
// append failure holds with nothing changed.
func (s *Server) mergeQueueResumeVerifying(ctx context.Context, e mergequeue.Entry) bool {
	log := func(level slog.Level, msg string, attrs ...slog.Attr) {
		s.cfg.Logger.LogAttrs(ctx, level, msg, append([]slog.Attr{
			slog.String("run_id", e.RunID.String()), slog.String("entry_id", e.ID.String()),
			slog.String("phase", string(e.Phase)), slog.String("head_sha", e.AnchoredHeadSHA)}, attrs...)...)
	}
	if s.cfg.MergeQueue == nil || s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		return false
	}
	if e.State != mergequeue.StateLive || (e.Phase != mergequeue.PhaseVerifying && e.Phase != mergequeue.PhaseUnverified) {
		return false
	}
	d, err := s.decideMergeQueueResumeVerifying(ctx, e)
	if err != nil {
		log(slog.LevelWarn, "merge queue resume: the verifying evidence could not be read; the entry holds", slog.String("error", err.Error()))
		return false
	}
	if d.action == "" && !d.redispatch {
		log(slog.LevelInfo, "merge queue resume: the verifying entry holds", slog.String("why", d.why))
		return false
	}

	var eject *mergequeue.Entry
	cont := false
	lockErr := s.withRunMergeLock(ctx, e.RunID, func(lctx context.Context) error {
		cur, ok := s.mergeQueueResumeLiveEntry(lctx, e, mergequeue.PhaseVerifying, mergequeue.PhaseUnverified)
		if !ok {
			log(slog.LevelInfo, "merge queue resume: the entry changed before the verifying recovery could act; it holds")
			return nil
		}
		d2, err := s.decideMergeQueueResumeVerifying(lctx, cur)
		if err != nil || d2.action != d.action || d2.reason != d.reason || d2.source != d.source || d2.redispatch != d.redispatch {
			log(slog.LevelInfo, "merge queue resume: the verifying evidence changed under the lock; the entry holds")
			return nil
		}
		step := string(cur.Phase)
		record := func(action, reason, next string) bool {
			if _, err := s.appendMergeQueueResumed(lctx, cur, mergeQueueResumed{
				Step: step, Action: action, Reason: reason, NextStep: next}); err != nil {
				log(slog.LevelWarn, "merge queue resume: merge_queue_resumed could not be appended; nothing is changed",
					slog.String("action", action), slog.String("error", err.Error()))
				return false
			}
			return true
		}
		switch d.action {
		case mergeQueueResumeActionApplyVerdict:
			if !record(d.action, d.reason, "") {
				return nil
			}
			cont = s.applyMergeCandidateStartToEntry(lctx, cur, mergeCandidateStatePassed, d.source) == admitLive
			return nil
		case mergeQueueResumeActionEject:
			if !record(d.action, d.reason, mergeQueueEjectNextFixupStage) {
				return nil
			}
			eject = &cur
			return nil
		case mergeQueueResumeActionRecordUnverified:
			if !record(d.action, d.reason, "") {
				return nil
			}
			if s.setMergeCandidateQueuePhaseAt(lctx, cur, mergequeue.PhaseUnverified) != admitLive {
				return nil
			}
			cur.Phase, step = mergequeue.PhaseUnverified, string(mergequeue.PhaseUnverified)
		}
		if d.redispatch {
			s.mergeQueueResumeRedispatch(lctx, cur, func() bool {
				return record(mergeQueueResumeActionRedispatchPass, mergeQueueResumeReasonRunnerFailed, "")
			})
		}
		return nil
	})
	if lockErr != nil {
		log(slog.LevelWarn, "merge queue resume: the run's merge-intent lock could not be taken; the entry holds",
			slog.String("error", lockErr.Error()))
		return false
	}
	if eject != nil {
		f := false
		return s.settleMergeCandidateQueueEntryAtWith(ctx, *eject, mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed,
			&mergeQueueEjection{FixupRouted: &f, Refusal: mergeQueueResumeLostHookRefusal}) == admitSettled
	}
	return cont
}

// mergeQueueResumeRedispatch re-dispatches the anchored head's crashed pass
// for the live unverified entry e, under the merge-intent lock. It reads the
// PR (its head must still be the anchored head, and its branch names the
// pass), resolves the cause from the verify state, records the row (record,
// fail closed), then starts the pass under the system actor and records phase
// verifying when it is in flight. Every refusal holds, logged.
func (s *Server) mergeQueueResumeRedispatch(ctx context.Context, e mergequeue.Entry, record func() bool) {
	hold := func(why string) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"merge queue resume: the crashed pass is not re-dispatched; the entry holds for fishhawk_rebase_run_branch",
			slog.String("run_id", e.RunID.String()), slog.String("head_sha", e.AnchoredHeadSHA), slog.String("why", why))
	}
	if s.cfg.GitHub == nil {
		hold("no GitHub client is wired to read the pull request")
		return
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, e.RunID)
	if err != nil {
		hold("the run could not be read: " + err.Error())
		return
	}
	target, err := mergeQueueResumeForgeTarget(runRow)
	if err != nil {
		hold(err.Error())
		return
	}
	pr, err := s.mergeOnPassReadPR(ctx, target)
	if err != nil {
		hold("reading the pull request failed: " + err.Error())
		return
	}
	if pr.Merged || pr.State == "closed" {
		hold("the pull request is " + pr.State + "; its resolution settles the entry")
		return
	}
	if pr.HeadSHA != e.AnchoredHeadSHA || pr.HeadRef == "" {
		hold("the pull request head " + pr.HeadSHA + " is not the anchored head")
		return
	}
	st, err := s.mergeCandidateVerifyState(ctx, runRow, e.AnchoredHeadSHA)
	if err != nil {
		hold("the merge-candidate verify state could not be read: " + err.Error())
		return
	}
	if st.State != mergeCandidateStateUnverified || st.Cause == "" {
		hold("the anchored head reads " + st.State + ", not unverified")
		return
	}
	if !record() {
		return
	}
	start, refusal := s.beginMergeCandidateVerifyPass(ctx, e.RunID, mergeCandidateVerifyParams{
		Branch: pr.HeadRef, BaseRef: e.BaseRef, HeadSHA: e.AnchoredHeadSHA, Cause: st.Cause,
	}, mergeCandidateSystemActor())
	if refusal != nil {
		hold("the pass could not be started: " + refusal.Reason)
		return
	}
	if start.State == mergeCandidateStateInFlight {
		s.setMergeCandidateQueuePhase(ctx, e, mergequeue.PhaseVerifying)
	}
}
