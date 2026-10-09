package server

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
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ADR-090 (E83.33 / #4018): Fishhawk verifies the MERGE CANDIDATE, not the
// branch. This file is the backend core every producer and consumer shares:
//
//   - startMergeCandidateVerifyPass records the durable trigger and re-opens
//     the implement stage for a runner VERIFY-ONLY pass (D3), with its own
//     per-head idempotency and never touching the fix-up budget;
//   - resolveMergeCandidateVerifyTrigger serves the LIVE trigger to the
//     runner, consumed by a later merge_candidate_verified row;
//   - recordMergeCandidateVerified writes the head-bound result;
//   - maybeRecoverMergeCandidateVerifyFailure keeps a crashed pass from
//     wedging its head as permanently in flight;
//   - mergeCandidateVerifyState is the ONE predicate the merge gate, the
//     rebase producer and the decomposed-parent hold read (D2, D4);
//   - routeMergeCandidateFailure applies D6 to a red result.

// CategoryStageMergeCandidateVerifyTriggered is the durable trigger that
// re-opens the implement stage for a verify-only merge-candidate pass (ADR-090
// D3). It is BOTH the runner's instruction (served on the prompt response) and
// the per-head in-flight marker. It is a distinct category from
// stage_fixup_triggered and stage_conflict_resolution_triggered, so the pass
// can neither read nor spend either budget. INTERNAL: not an issue-comment
// surface.
const CategoryStageMergeCandidateVerifyTriggered = "stage_merge_candidate_verify_triggered"

// CategoryMergeCandidateVerified records the result of one pass, bound to
// EXACTLY one head SHA (D2). A same-stage entry at or after a trigger CONSUMES
// it, whatever the result. INTERNAL: not an issue-comment surface.
const CategoryMergeCandidateVerified = "merge_candidate_verified"

// Merge-candidate pass causes: which Fishhawk write produced the head (D2).
const (
	mergeCandidateCauseBaseAdvance        = "base_advance"
	mergeCandidateCauseConflictResolution = "conflict_resolution"
	mergeCandidateCauseFanIn              = "fan_in"
)

// Recorded pass results (D3). not_executed means the pass did not reach a
// verdict (head moved, infrastructure, a crashed runner); the head may be
// re-triggered.
const (
	mergeCandidateResultPassed      = "passed"
	mergeCandidateResultFailed      = "failed"
	mergeCandidateResultNotExecuted = "not_executed"
)

// Verify states mergeCandidateVerifyState reports for a head.
const (
	mergeCandidateStateNotRequired = "not_required"
	mergeCandidateStateUnverified  = "unverified"
	mergeCandidateStateInFlight    = "in_flight"
	mergeCandidateStatePassed      = "passed"
	mergeCandidateStateFailed      = "failed"
)

// mergeCandidateOutputTailMax bounds the stored output tail. The runner
// already redacts and bounds it; the server re-bounds so a misbehaving
// reporter cannot grow the audit chain.
const mergeCandidateOutputTailMax = 4096

// mergeCandidateSystemSubject is the in-process identity a non-request write
// (the fan-in hold's trigger) and the D6 routed fix-up act under.
const mergeCandidateSystemSubject = "system:merge-candidate-verify"

// mergeCandidateDelegatedRule labels the routed fix-up's stage_fixup_triggered
// row. It is deliberately NOT the knob's convergent_concerns condition: D6
// routes on the delegation being DECLARED, and that condition was never
// evaluated for this route.
const mergeCandidateDelegatedRule = "merge_candidate_verify_failed"

// mergeCandidateFixupRoute is named by every D6 refusal, so an operator reading
// any arm learns the same next step.
const mergeCandidateFixupRoute = "Route a fix-up with fishhawk_fixup_stage on the implement stage, then re-verify the new head."

var (
	errMergeCandidateInvalidResult  = errors.New("merge_candidate_result must be one of passed, failed, not_executed")
	errMergeCandidateNoLiveTrigger  = errors.New("no live merge-candidate verify trigger for this stage")
	errMergeCandidateTriggerPayload = errors.New("the live merge-candidate verify trigger payload is malformed or half-populated")
)

// mergeCandidateVerifyTrigger is the stage_merge_candidate_verify_triggered
// payload: everything the runner needs (branch, base ref, expected head, the
// declared command) plus the pre-pass gate state the failure recovery
// restores. PriorState and ReparkedReviewStageID mirror the conflict-
// resolution trigger's restore anchors so run.RestoreFixupStage applies.
type mergeCandidateVerifyTrigger struct {
	Branch                string `json:"branch"`
	BaseRef               string `json:"base_ref"`
	ExpectedHeadSHA       string `json:"expected_head_sha"`
	Cause                 string `json:"cause"`
	VerifyCommand         string `json:"verify_command"`
	PriorState            string `json:"prior_state,omitempty"`
	ReparkedReviewStageID string `json:"reparked_review_stage_id,omitempty"`
}

// populated reports whether the trigger carries every anchor the runner needs.
// A half-populated trigger is treated as NO pass: a runner handed an empty
// expected head or command would report a verdict about nothing.
func (t *mergeCandidateVerifyTrigger) populated() bool {
	return t != nil && t.Branch != "" && t.BaseRef != "" && t.ExpectedHeadSHA != "" &&
		validMergeCandidateCause(t.Cause) && t.VerifyCommand != ""
}

func validMergeCandidateCause(c string) bool {
	switch c {
	case mergeCandidateCauseBaseAdvance, mergeCandidateCauseConflictResolution, mergeCandidateCauseFanIn:
		return true
	}
	return false
}

func validMergeCandidateResult(r string) bool {
	switch r {
	case mergeCandidateResultPassed, mergeCandidateResultFailed, mergeCandidateResultNotExecuted:
		return true
	}
	return false
}

// mergeCandidateVerifiedPayload is the merge_candidate_verified row. The
// output tail is runner-redacted and server-bounded, and it is UNTRUSTED:
// output_untrusted is always true so every reader frames it as data.
type mergeCandidateVerifiedPayload struct {
	HeadSHA         string `json:"head_sha"`
	Cause           string `json:"cause"`
	VerifyCommand   string `json:"verify_command"`
	Result          string `json:"result"`
	Reason          string `json:"reason,omitempty"`
	TriggerSequence int64  `json:"trigger_sequence"`
	OutputTail      string `json:"output_tail,omitempty"`
	OutputUntrusted bool   `json:"output_untrusted"`
}

// mergeCandidateActor is the audit actor a trigger records. Request-bound
// producers pass the operator; non-request producers pass the system kind.
type mergeCandidateActor struct {
	Kind    audit.ActorKind
	Subject string
}

// mergeCandidateSystemActor is the actor for a non-request producer.
func mergeCandidateSystemActor() mergeCandidateActor {
	return mergeCandidateActor{Kind: audit.ActorSystem, Subject: mergeCandidateSystemSubject}
}

// mergeCandidateVerifyParams anchors a pass to one head.
type mergeCandidateVerifyParams struct {
	Branch  string
	BaseRef string
	HeadSHA string
	Cause   string
}

// mergeCandidateVerifyStart is a successful start answer. State is in_flight
// when a pass is live for the head (Triggered says whether THIS call appended
// it), passed/failed when a verdict is already recorded for the head (nothing
// started), or not_required when no verify command is declared (D4).
type mergeCandidateVerifyStart struct {
	State     string
	StageID   uuid.UUID
	Triggered bool
}

// mergeCandidateVerifyRefusal is the negative answer: no pass could be
// started, and why. Reason is always populated.
type mergeCandidateVerifyRefusal struct {
	Reason string
}

// startMergeCandidateVerifyPass authorizes a verify-only pass for one head
// (ADR-090 D3), mirroring startConflictResolutionPass with three differences:
// it is context-based with an EXPLICIT actor (the fan-in hold has no request),
// its idempotency is PER HEAD rather than a ceiling, and it short-circuits to
// not_required when the workflow declares no verify command (D4).
//
// ORDERING IS APPEND-THEN-REOPEN for the reason the conflict pass documents: a
// failed append must leave the implement stage untouched, because a re-opened
// stage with no trigger is dispatched as an ORDINARY implement re-run. A failed
// re-open after a successful append is settled by a not_executed row so the
// head is not wedged as in flight with nothing running.
//
// The re-open hands run.FixupStage the pass's OWN counts (0 prior, max 1,
// ceiling 1), so the fix-up counter is never read or spent.
func (s *Server) startMergeCandidateVerifyPass(ctx context.Context, runID uuid.UUID,
	p mergeCandidateVerifyParams, actor mergeCandidateActor) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return nil, &mergeCandidateVerifyRefusal{Reason: "no run/audit repository is wired, so no merge-candidate verify pass can be authorized"}
	}
	if p.Branch == "" || p.BaseRef == "" || p.HeadSHA == "" || !validMergeCandidateCause(p.Cause) {
		return nil, &mergeCandidateVerifyRefusal{Reason: "the run branch, base ref, head sha or cause could not be resolved, so no merge-candidate verify pass can be anchored"}
	}
	if actor.Subject == "" {
		return nil, &mergeCandidateVerifyRefusal{Reason: "no actor was supplied for the merge-candidate verify trigger"}
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		return nil, &mergeCandidateVerifyRefusal{Reason: "the run could not be read (" + err.Error() + ")"}
	}

	// D4: no declared verify command means nothing is required and nothing is
	// started.
	command, _, _ := s.resolveVerifyConfig(ctx, runRow, run.StageTypeImplement)
	if command == "" {
		return &mergeCandidateVerifyStart{State: mergeCandidateStateNotRequired}, nil
	}

	impl := s.findImplementStage(ctx, runID)
	if impl == nil {
		return nil, &mergeCandidateVerifyRefusal{Reason: "the run has no implement stage to re-open, so no merge-candidate verify pass can be started"}
	}

	// PER-HEAD IDEMPOTENCE. A live pass for this head is returned as-is; a
	// recorded passed/failed verdict starts nothing; not_executed (or nothing)
	// falls through to a fresh trigger.
	live, err := s.liveMergeCandidateTrigger(ctx, runID, impl.ID)
	if err != nil {
		return nil, &mergeCandidateVerifyRefusal{Reason: "the merge-candidate verify chain could not be read (" + err.Error() + "); refusing to authorize a pass on an uncertain state"}
	}
	if live != nil && live.payload.ExpectedHeadSHA == p.HeadSHA {
		return &mergeCandidateVerifyStart{State: mergeCandidateStateInFlight, StageID: impl.ID}, nil
	}
	verdict, err := s.newestMergeCandidateVerdict(ctx, runID, p.HeadSHA)
	if err != nil {
		return nil, &mergeCandidateVerifyRefusal{Reason: "the merge-candidate verify chain could not be read (" + err.Error() + "); refusing to authorize a pass on an uncertain state"}
	}
	if verdict != nil && (verdict.Result == mergeCandidateResultPassed || verdict.Result == mergeCandidateResultFailed) {
		return &mergeCandidateVerifyStart{State: verdict.Result, StageID: impl.ID}, nil
	}

	priorState := impl.State
	if priorState != run.StageStateSucceeded && priorState != run.StageStateAwaitingApproval {
		return nil, &mergeCandidateVerifyRefusal{Reason: fmt.Sprintf(
			"the implement stage is in state %q; only a stage parked at its review gate (succeeded or awaiting_approval) can be re-opened for a merge-candidate verify pass", priorState)}
	}
	reviewStageID := ""
	if priorState == run.StageStateSucceeded {
		if review := s.openReviewStageFor(ctx, runID); review != nil {
			reviewStageID = review.ID.String()
		}
	}

	trigger := mergeCandidateVerifyTrigger{
		Branch:                p.Branch,
		BaseRef:               p.BaseRef,
		ExpectedHeadSHA:       p.HeadSHA,
		Cause:                 p.Cause,
		VerifyCommand:         command,
		PriorState:            string(priorState),
		ReparkedReviewStageID: reviewStageID,
	}
	if err := s.appendMergeCandidateTrigger(ctx, runID, impl.ID, trigger, actor); err != nil {
		return nil, &mergeCandidateVerifyRefusal{Reason: "recording the merge-candidate verify trigger failed (" + err.Error() + "), so no pass was authorized and the implement stage was not re-opened"}
	}

	if _, err := run.FixupStage(ctx, s.cfg.RunRepo, impl.ID, run.FixupOptions{
		PriorPassCount: 0,
		MaxPasses:      1,
		HardCeiling:    1,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate verify: re-opening the implement stage failed; settling the trigger as not_executed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", impl.ID.String()),
			slog.String("error", err.Error()))
		// Consume the trigger so the head is re-triggerable rather than read
		// as in flight with nothing running. Best-effort: a failed append
		// leaves the live trigger, which the next start for this head answers
		// as in_flight and the reap path recovers (D8).
		if _, rerr := s.recordMergeCandidateVerified(ctx, runID, impl.ID, mergeCandidateResultNotExecuted,
			"reopen_failed: "+err.Error(), "", audit.ActorSystem, mergeCandidateSystemSubject); rerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"merge-candidate verify: settling the un-reopened trigger failed",
				slog.String("run_id", runID.String()),
				slog.String("error", rerr.Error()))
		}
		return nil, &mergeCandidateVerifyRefusal{Reason: "re-opening the implement stage for the merge-candidate verify pass failed (" + err.Error() + ")"}
	}

	s.notifyStatusUpdate(ctx, runID, "merge_candidate_verify_start")
	return &mergeCandidateVerifyStart{State: mergeCandidateStateInFlight, StageID: impl.ID, Triggered: true}, nil
}

// appendMergeCandidateTrigger appends the durable trigger. NOT best-effort: the
// entry is the runner's instruction, so the caller refuses on an error.
func (s *Server) appendMergeCandidateTrigger(ctx context.Context, runID, stageID uuid.UUID,
	trigger mergeCandidateVerifyTrigger, actor mergeCandidateActor) error {
	payload, err := json.Marshal(trigger)
	if err != nil {
		return err
	}
	kind, subject := actor.Kind, actor.Subject
	_, err = s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryStageMergeCandidateVerifyTriggered,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	})
	return err
}

// liveTrigger is a decoded live trigger plus its chain sequence.
type liveTrigger struct {
	payload  mergeCandidateVerifyTrigger
	sequence int64
}

// stageEntries lists a category's entries for one stage, returning the read
// error rather than reading it as "none".
func (s *Server) stageEntriesStrict(ctx context.Context, runID, stageID uuid.UUID, category string) ([]*audit.Entry, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, category)
	if err != nil {
		return nil, err
	}
	var out []*audit.Entry
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			out = append(out, e)
		}
	}
	return out, nil
}

// liveMergeCandidateTrigger returns the stage's newest trigger when no
// same-stage merge_candidate_verified row has consumed it, or nil. A read
// error is returned; a malformed or half-populated newest trigger is an error
// too, so no caller serves or skips a pass it cannot justify.
func (s *Server) liveMergeCandidateTrigger(ctx context.Context, runID, stageID uuid.UUID) (*liveTrigger, error) {
	triggers, err := s.stageEntriesStrict(ctx, runID, stageID, CategoryStageMergeCandidateVerifyTriggered)
	if err != nil {
		return nil, err
	}
	if len(triggers) == 0 {
		return nil, nil
	}
	newest := triggers[len(triggers)-1]
	verified, err := s.stageEntriesStrict(ctx, runID, stageID, CategoryMergeCandidateVerified)
	if err != nil {
		return nil, err
	}
	if len(verified) > 0 && verified[len(verified)-1].Sequence >= newest.Sequence {
		return nil, nil
	}
	var payload mergeCandidateVerifyTrigger
	if err := json.Unmarshal(newest.Payload, &payload); err != nil || !payload.populated() {
		return nil, errMergeCandidateTriggerPayload
	}
	return &liveTrigger{payload: payload, sequence: newest.Sequence}, nil
}

// resolveMergeCandidateVerifyTrigger returns the LIVE trigger to serve to the
// runner for a stage, or nil. Fails closed (nil) on a read error and on a
// malformed or half-populated payload — the shape of
// resolveConflictResolutionTrigger: serving a pass the backend cannot justify
// is worse than serving none.
func (s *Server) resolveMergeCandidateVerifyTrigger(ctx context.Context, runID, stageID uuid.UUID) *mergeCandidateVerifyTrigger {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	live, err := s.liveMergeCandidateTrigger(ctx, runID, stageID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate verify: trigger unresolvable — serving no pass",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		return nil
	}
	if live == nil {
		return nil
	}
	return &live.payload
}

// mergeCandidateVerifiedRecord is what recordMergeCandidateVerified reports:
// the trigger the result settled, the result in force, and whether this call
// was an idempotent replay that appended nothing.
type mergeCandidateVerifiedRecord struct {
	Trigger   mergeCandidateVerifyTrigger
	Result    string
	Duplicate bool
}

// recordMergeCandidateVerified appends the merge_candidate_verified row for the
// stage's live trigger, consuming it. It refuses an out-of-set result and a
// stage with no live trigger, and it is idempotent per (stage, trigger
// sequence): a runner retry after a 5xx finds the trigger already consumed by
// a row naming the same trigger sequence and appends nothing.
func (s *Server) recordMergeCandidateVerified(ctx context.Context, runID, stageID uuid.UUID,
	result, reason, outputTail string, actorKind audit.ActorKind, actorSubject string) (*mergeCandidateVerifiedRecord, error) {
	if !validMergeCandidateResult(result) {
		return nil, errMergeCandidateInvalidResult
	}
	if s.cfg.AuditRepo == nil {
		return nil, errors.New("no audit repository is wired")
	}
	triggers, err := s.stageEntriesStrict(ctx, runID, stageID, CategoryStageMergeCandidateVerifyTriggered)
	if err != nil {
		return nil, err
	}
	if len(triggers) == 0 {
		return nil, errMergeCandidateNoLiveTrigger
	}
	newest := triggers[len(triggers)-1]
	var trigger mergeCandidateVerifyTrigger
	if err := json.Unmarshal(newest.Payload, &trigger); err != nil || !trigger.populated() {
		return nil, errMergeCandidateTriggerPayload
	}
	verified, err := s.stageEntriesStrict(ctx, runID, stageID, CategoryMergeCandidateVerified)
	if err != nil {
		return nil, err
	}
	if len(verified) > 0 && verified[len(verified)-1].Sequence >= newest.Sequence {
		var prior mergeCandidateVerifiedPayload
		if err := json.Unmarshal(verified[len(verified)-1].Payload, &prior); err == nil &&
			prior.TriggerSequence == newest.Sequence {
			return &mergeCandidateVerifiedRecord{Trigger: trigger, Result: prior.Result, Duplicate: true}, nil
		}
		return nil, errMergeCandidateNoLiveTrigger
	}

	payload, err := json.Marshal(mergeCandidateVerifiedPayload{
		HeadSHA:         trigger.ExpectedHeadSHA,
		Cause:           trigger.Cause,
		VerifyCommand:   trigger.VerifyCommand,
		Result:          result,
		Reason:          reason,
		TriggerSequence: newest.Sequence,
		OutputTail:      boundMergeCandidateTail(outputTail),
		OutputUntrusted: true,
	})
	if err != nil {
		return nil, err
	}
	kind := actorKind
	subject := actorSubject
	if subject == "" {
		subject = "anonymous"
	}
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryMergeCandidateVerified,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		return nil, err
	}
	s.notifyStatusUpdate(ctx, runID, "merge_candidate_verify_settle")
	return &mergeCandidateVerifiedRecord{Trigger: trigger, Result: result}, nil
}

// boundMergeCandidateTail keeps the LAST mergeCandidateOutputTailMax bytes of
// the tail (the end of verify output names the failure), re-validated as
// UTF-8 after the cut.
func boundMergeCandidateTail(tail string) string {
	if len(tail) <= mergeCandidateOutputTailMax {
		return tail
	}
	return strings.ToValidUTF8(tail[len(tail)-mergeCandidateOutputTailMax:], "")
}

// maybeRecoverMergeCandidateVerifyFailure handles a runner `failed` report for
// a stage carrying a live merge-candidate trigger: it writes
// merge_candidate_verified{not_executed, reason} FIRST (consuming the trigger,
// so the head is re-triggerable rather than permanently in flight) and then
// restores the pre-pass gate via run.RestoreFixupStage. Returns true so the
// caller skips the ordinary failure path. Keyed on a live trigger for that
// stage, so a decomposition child's pull_request_failed (#4079), which never
// carries one, takes today's path unchanged.
func (s *Server) maybeRecoverMergeCandidateVerifyFailure(ctx context.Context, runID, stageID uuid.UUID, reason string) bool {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return false
	}
	stage, err := s.cfg.RunRepo.GetStage(ctx, stageID)
	if err != nil || stage.Type != run.StageTypeImplement {
		return false
	}
	trigger := s.resolveMergeCandidateVerifyTrigger(ctx, runID, stageID)
	if trigger == nil {
		return false
	}
	var reviewStageID *uuid.UUID
	if trigger.ReparkedReviewStageID != "" {
		rid, perr := uuid.Parse(trigger.ReparkedReviewStageID)
		if perr != nil {
			return false
		}
		reviewStageID = &rid
	}
	if _, err := s.recordMergeCandidateVerified(ctx, runID, stageID, mergeCandidateResultNotExecuted,
		"runner_failed: "+reason, "", audit.ActorSystem, mergeCandidateSystemSubject); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate verify recovery: consumption row not persisted — leaving failure path in force",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		return false
	}
	if _, err := run.RestoreFixupStage(ctx, s.cfg.RunRepo, stageID,
		run.StageState(trigger.PriorState), reviewStageID); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate verify recovery: restore failed — leaving failure path in force",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		return false
	}
	s.notifyStatusUpdate(ctx, runID, "merge_candidate_verify_recovered")
	return true
}

// mergeCandidateState is mergeCandidateVerifyState's answer. Cause is set for a
// classified head; StageID names the implement stage carrying a live trigger
// when State is in_flight.
type mergeCandidateState struct {
	State   string
	Cause   string
	StageID uuid.UUID
}

// mergeCandidateRunnerGatedCategories carry head_sha reports of heads the
// runner already gated on the committed tree (implement PR open, fix-up push,
// decomposition child push): they need nothing more (D2).
var mergeCandidateRunnerGatedCategories = []string{"pull_request_opened", "fixup_pushed", "child_pushed"}

// mergeCandidateVerifyState is the single predicate the merge gate, the rebase
// producer and the decomposed-parent hold read (ADR-090 D2/D4/D8). It returns
// not_required when no verify command is declared. Otherwise it classifies
// headSHA by the Fishhawk write that produced it, in precedence order:
//
//  1. branch_rebased with a performed merge whose new_head_sha or
//     merge_commit_sha is the head → base_advance;
//  2. conflict_resolution_pushed at the head → conflict_resolution;
//  3. a runner-gated head report at the head → not_required;
//  4. an operator-vouched head → not_required (D8);
//  5. a decomposed parent (fan-in integration rows on its chain) → fan_in;
//  6. anything else → not_required.
//
// The rebase verb attributes its own merge commit with an
// operator_commit_vouched row, which is why 1 and 2 precede 4: a vouch must
// not exempt a base-advance head. A classified head is passed or failed only
// by a verdict for EXACTLY that SHA; otherwise in_flight while a live trigger
// names it, else unverified. Every audit read error is returned, never read as
// "no requirement".
func (s *Server) mergeCandidateVerifyState(ctx context.Context, runRow *run.Run, headSHA string) (mergeCandidateState, error) {
	command, _, _ := s.resolveVerifyConfig(ctx, runRow, run.StageTypeImplement)
	if command == "" {
		return mergeCandidateState{State: mergeCandidateStateNotRequired}, nil
	}
	if s.cfg.AuditRepo == nil {
		return mergeCandidateState{}, errors.New("no audit repository is wired, so the merge-candidate verify state cannot be read")
	}
	if headSHA == "" {
		return mergeCandidateState{}, errors.New("the live head sha is empty, so the merge-candidate verify state cannot be decided")
	}
	cause, err := s.classifyMergeCandidateHead(ctx, runRow.ID, headSHA)
	if err != nil {
		return mergeCandidateState{}, err
	}
	if cause == "" {
		return mergeCandidateState{State: mergeCandidateStateNotRequired}, nil
	}

	verdict, err := s.newestMergeCandidateVerdict(ctx, runRow.ID, headSHA)
	if err != nil {
		return mergeCandidateState{}, err
	}
	if verdict != nil && (verdict.Result == mergeCandidateResultPassed || verdict.Result == mergeCandidateResultFailed) {
		return mergeCandidateState{State: verdict.Result, Cause: cause}, nil
	}
	stageID, live, err := s.liveMergeCandidateTriggerForHead(ctx, runRow.ID, headSHA)
	if err != nil {
		return mergeCandidateState{}, err
	}
	if live {
		return mergeCandidateState{State: mergeCandidateStateInFlight, Cause: cause, StageID: stageID}, nil
	}
	return mergeCandidateState{State: mergeCandidateStateUnverified, Cause: cause}, nil
}

// classifyMergeCandidateHead returns the cause for a head that needs a
// merge-candidate verify, or "" when it needs none.
func (s *Server) classifyMergeCandidateHead(ctx context.Context, runID uuid.UUID, headSHA string) (string, error) {
	rebased, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryBranchRebased)
	if err != nil {
		return "", err
	}
	for _, e := range rebased {
		var p struct {
			NewHeadSHA      string `json:"new_head_sha"`
			MergeCommitSHA  string `json:"merge_commit_sha"`
			AlreadyUpToDate bool   `json:"already_up_to_date"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || p.AlreadyUpToDate {
			continue
		}
		if p.NewHeadSHA == headSHA || p.MergeCommitSHA == headSHA {
			return mergeCandidateCauseBaseAdvance, nil
		}
	}

	if hit, err := s.chainReportsHead(ctx, runID, CategoryConflictResolutionPushed, "head_sha", headSHA); err != nil {
		return "", err
	} else if hit {
		return mergeCandidateCauseConflictResolution, nil
	}

	for _, category := range mergeCandidateRunnerGatedCategories {
		hit, err := s.chainReportsHead(ctx, runID, category, "head_sha", headSHA)
		if err != nil {
			return "", err
		}
		if hit {
			return "", nil
		}
	}

	if hit, err := s.chainReportsHead(ctx, runID, CategoryOperatorCommitVouched, "vouched_sha", headSHA); err != nil {
		return "", err
	} else if hit {
		return "", nil
	}

	for _, category := range []string{auditcomplete.CategoryIntegrationCommitRecorded, auditcomplete.CategorySlicesIntegrated} {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, category)
		if err != nil {
			return "", err
		}
		if len(entries) > 0 {
			return mergeCandidateCauseFanIn, nil
		}
	}
	return "", nil
}

// chainReportsHead reports whether any of the run's entries in category carry
// headSHA in the named string payload field.
func (s *Server) chainReportsHead(ctx context.Context, runID uuid.UUID, category, field, headSHA string) (bool, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, category)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		var p map[string]any
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		if v, ok := p[field].(string); ok && v == headSHA {
			return true, nil
		}
	}
	return false, nil
}

// newestMergeCandidateVerdict returns the newest merge_candidate_verified row
// for EXACTLY headSHA (any stage), or nil.
func (s *Server) newestMergeCandidateVerdict(ctx context.Context, runID uuid.UUID, headSHA string) (*mergeCandidateVerifiedPayload, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryMergeCandidateVerified)
	if err != nil {
		return nil, err
	}
	var newest *mergeCandidateVerifiedPayload
	for _, e := range entries {
		var p mergeCandidateVerifiedPayload
		if json.Unmarshal(e.Payload, &p) != nil || p.HeadSHA != headSHA {
			continue
		}
		newest = &p
	}
	return newest, nil
}

// liveMergeCandidateTriggerForHead reports whether a live (unconsumed) trigger
// names headSHA, and the stage it re-opened.
func (s *Server) liveMergeCandidateTriggerForHead(ctx context.Context, runID uuid.UUID, headSHA string) (uuid.UUID, bool, error) {
	triggers, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageMergeCandidateVerifyTriggered)
	if err != nil {
		return uuid.Nil, false, err
	}
	seen := map[uuid.UUID]bool{}
	for i := len(triggers) - 1; i >= 0; i-- {
		e := triggers[i]
		if e.StageID == nil || seen[*e.StageID] {
			continue
		}
		seen[*e.StageID] = true
		live, err := s.liveMergeCandidateTrigger(ctx, runID, *e.StageID)
		if err != nil {
			return uuid.Nil, false, err
		}
		if live != nil && live.payload.ExpectedHeadSHA == headSHA {
			return *e.StageID, true, nil
		}
	}
	return uuid.Nil, false, nil
}

// routeMergeCandidateFailure applies ADR-090 D6 to a `failed` pass. When the
// run's effective delegation declares fix-up routing (v2 actions.fixup or
// v0/v1 may_route_fixup, after the escalation clamp the evaluator applies), it
// routes ONE bounded fix-up through fixupStageAs under an in-process system
// identity carrying write:fixups, with the existing budget/ceiling arithmetic.
// The routed concern carries ONLY trusted fields — head SHA, cause and the
// declared command — never the untrusted verify output. Not delegated, an
// unevaluable delegation, or a refused route returns routed=false with a
// refusal naming fishhawk_fixup_stage.
func (s *Server) routeMergeCandidateFailure(ctx context.Context, runRow *run.Run, stageID uuid.UUID,
	trigger *mergeCandidateVerifyTrigger) (bool, string) {
	if runRow == nil || trigger == nil || !trigger.populated() {
		return false, "no complete merge-candidate verify trigger accompanies the failed result, so no fix-up was routed. " + mergeCandidateFixupRoute
	}
	res, _, ok := s.evaluateRunDelegation(ctx, runRow, nil)
	if !ok {
		return false, "the run's fix-up delegation could not be evaluated, so no fix-up was routed. " + mergeCandidateFixupRoute
	}
	if _, delegated := res.Decision(delegation.ActionRouteFixup); !delegated {
		return false, "the workflow does not delegate fix-up routing, so no fix-up was routed (ADR-090 D6). " + mergeCandidateFixupRoute
	}

	priorPasses, err := s.countFixupPasses(ctx, runRow.ID, stageID)
	if err != nil {
		return false, "the fix-up budget could not be read (" + err.Error() + "), so no fix-up was routed. " + mergeCandidateFixupRoute
	}
	refunded, _, err := s.fixupRefundedPasses(ctx, runRow.ID, stageID)
	if err != nil {
		return false, "the fix-up refund ledger could not be read (" + err.Error() + "), so no fix-up was routed. " + mergeCandidateFixupRoute
	}
	if refunded > priorPasses {
		refunded = priorPasses
	}
	ceilingCredits := refunded
	if ceilingCredits > maxCeilingRefundCredits {
		ceilingCredits = maxCeilingRefundCredits
	}

	selected := []planreview.Concern{{
		Severity: planreview.SeverityHigh,
		Category: "merge_candidate",
		Note:     mergeCandidateConcernNote(trigger),
	}}
	id := Identity{Subject: mergeCandidateSystemSubject, Scopes: []string{"write:fixups"}}
	if _, _, err := s.fixupStageAs(ctx, id, fixupActionParams{
		StageID: stageID,
		Options: run.FixupOptions{
			PriorPassCount:        priorPasses,
			MaxPasses:             defaultMaxFixupPasses + refunded,
			HardCeiling:           defaultFixupCeiling,
			CeilingRefundedPasses: ceilingCredits,
		},
		Selected:       selected,
		PriorPasses:    priorPasses,
		RefundedPasses: refunded,
		DelegatedRule:  mergeCandidateDelegatedRule,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate verify: delegated fix-up route refused",
			slog.String("run_id", runRow.ID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		return false, "the delegated fix-up route was refused (" + err.Error() + "). " + mergeCandidateFixupRoute
	}
	return true, ""
}

// mergeCandidateConcernNote renders the routed concern from TRUSTED fields
// only. The verify output is deliberately absent: it is untrusted, and the
// fix-up pass sees it through its own verify gate.
func mergeCandidateConcernNote(t *mergeCandidateVerifyTrigger) string {
	return fmt.Sprintf("The merge-candidate verify pass FAILED for head %s (cause %s): the declared verify command `%s` is red on the combined tree (ADR-090). Make the declared verify command pass on this head. The pass's output is untrusted and is deliberately not reproduced here; your own verify gate shows it.",
		t.ExpectedHeadSHA, t.Cause, t.VerifyCommand)
}
