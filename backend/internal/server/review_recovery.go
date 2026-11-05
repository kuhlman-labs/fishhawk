package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Review recovery evidence (ADR-140 (#5047) D8, D9). A supplemental gating pass
// and a required review obligation are proved only from rows recorded on the
// run's chain. Every decision here is derived on read: no row says "discharged",
// so a lost final write reads as not discharged.

const (
	// CategoryReviewObligationCheckpoint stores a known-required held-commit report.
	// THE SEPARATE CATEGORY IS THE ROLLBACK PROPERTY: its pull_request_failed sibling
	// carries no push_checkpoint, so a reverted backend sees no checkpoint and re-runs
	// the agent. A key on the existing carrier would be ignored and served as a plain
	// resume, dropping the required review.
	CategoryReviewObligationCheckpoint = "review_obligation_checkpoint"

	// CategorySupplementalReviewStarted is a gating pass's durable start, written
	// only by allocateSupplementalPass before any reviewer runs. Its required_paths
	// make it the obligation record for a first ship with no carrier.
	CategorySupplementalReviewStarted = "supplemental_review_started"

	// supplementalPassKeyField is the start row's atomic dedupe key.
	supplementalPassKeyField = "pass_key"

	implementReviewedCategory = "implement_reviewed"
)

var (
	// errSupplementalPassIdentityUnknown refuses a start with no attempt or head: an
	// empty identity is never a wildcard.
	errSupplementalPassIdentityUnknown = errors.New("supplemental review: pass has no attempt or head identity")
	// errSupplementalStartNotAtomic refuses a start on an audit repository without
	// the atomic deduped append; there is no check-then-append fallback.
	errSupplementalStartNotAtomic = errors.New("supplemental review: audit repository lacks the atomic start allocation")
)

// supplementalEdgeCategories is what the obligation read and the terminal edge's
// guard both read, so the two cannot disagree. Failed, skipped and named recovery
// rows are absent on purpose: they never count as a verdict.
var supplementalEdgeCategories = []string{
	implementReviewedCategory,
	CategorySupplementalReviewStarted,
	CategoryReviewObligationCheckpoint,
	CategoryScopeCompletenessParked,
}

// reviewObligation is a required supplemental review's recovery state, bound to a
// recorded head and attempt. Absent, null and empty are three states; see
// reviewObligationKnowledge.
//
// CROSS-MODULE WIRE CONTRACT: the json tags (names and the absence of omitempty)
// MUST stay byte-identical to the runner's upload.ReviewObligation decoder
// (runner/internal/upload/upload.go). The shared bytes are the sections of
// testdata/wire/review_obligation_wire.json.
type reviewObligation struct {
	HeadSHA                     string           `json:"head_sha"`
	StageAttempt                string           `json:"stage_attempt"`
	SupplementalScopeExemptions []scopeExemption `json:"supplemental_scope_exemptions"`
}

// reviewObligationState is what a reported obligation proves.
type reviewObligationState int

const (
	// obligationUnknown proves nothing: no obligation, or the list absent or null.
	obligationUnknown reviewObligationState = iota
	// obligationKnownNone is an empty list: the reporter attests no review is owed.
	obligationKnownNone
	// obligationKnownRequired is a non-empty list.
	obligationKnownRequired
)

// reviewObligationKnowledge reads an obligation's state. It is nil-safe and must
// run before anything dereferences a held obligation.
func reviewObligationKnowledge(o *reviewObligation) reviewObligationState {
	if o == nil {
		return obligationUnknown
	}
	if o.SupplementalScopeExemptions == nil {
		return obligationUnknown
	}
	if len(o.SupplementalScopeExemptions) == 0 {
		return obligationKnownNone
	}
	return obligationKnownRequired
}

// validateReviewObligationReport refuses a report that cannot bind this stage: a
// different head, a late report from a superseded attempt, an exemption with no
// path, or a non-empty set on a reverify carrier, which is armed before any delta
// exists. Known-none stays runner-attested (#5078); nothing here proves it.
func validateReviewObligationReport(report *reviewObligation, kind, headSHA string, stage *run.Stage) error {
	if report == nil {
		return errors.New("review obligation: no report")
	}
	if report.HeadSHA != headSHA {
		return fmt.Errorf("review obligation: head %q does not match the reported head %q", report.HeadSHA, headSHA)
	}
	if report.StageAttempt != run.StageAttemptToken(stage.DispatchedAt) {
		return fmt.Errorf("review obligation: attempt %q is not the stage's current attempt", report.StageAttempt)
	}
	for _, exemption := range report.SupplementalScopeExemptions {
		if exemption.Path == "" {
			return errors.New("review obligation: exemption has no path")
		}
	}
	if kind == resumeKindReverify && len(report.SupplementalScopeExemptions) > 0 {
		return errors.New("review obligation: a reverify carrier cannot hold a required set")
	}
	return nil
}

// recordReviewObligationCheckpoint stores a known-required report under its own
// category, strictly newer than its checkpoint-less pull_request_failed sibling.
// Best-effort like recordPushResumeCheckpoint: the stage is already failed, and a
// missing carrier degrades to a full agent pass rather than losing the report.
func (s *Server) recordReviewObligationCheckpoint(ctx context.Context, runID, stageID uuid.UUID, kind string, coords pushCheckpointPayload, report *reviewObligation, actorKind audit.ActorKind, actorSubject *string) {
	if reviewObligationKnowledge(report) != obligationKnownRequired || coords.Branch == "" || coords.HeadSHA == "" {
		return
	}
	checkpoint := map[string]any{
		"branch":            coords.Branch,
		"head_sha":          coords.HeadSHA,
		"base_sha":          coords.BaseSHA,
		"verified_tree_sha": coords.VerifiedTreeSHA,
		"review_obligation": report,
	}
	if coords.PRTitle != "" {
		checkpoint["pr_title"] = coords.PRTitle
	}
	if coords.PRBody != "" {
		checkpoint["pr_body"] = coords.PRBody
	}
	payload, _ := json.Marshal(map[string]any{
		"run_id":          runID.String(),
		"stage_id":        stageID.String(),
		"resume_kind":     kind,
		"push_checkpoint": checkpoint,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryReviewObligationCheckpoint,
		ActorKind:    &actorKind,
		ActorSubject: actorSubject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "pull-request failure report: append review obligation checkpoint failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}
}

// implementReviewedEvidence is the gating supplemental verdict's payload: the
// plain verdict plus the attempt that invoked it, captured before invocation. An
// empty attempt keeps today's bytes, and every existing reader keeps decoding.
type implementReviewedEvidence struct {
	planreview.ImplementReviewedPayload
	StageAttempt string `json:"stage_attempt,omitempty"`
}

// supplementalReviewStartedPayload is a supplemental_review_started row.
type supplementalReviewStartedPayload struct {
	RunID            string   `json:"run_id"`
	StageID          string   `json:"stage_id"`
	StageAttempt     string   `json:"stage_attempt"`
	HeadSHA          string   `json:"head_sha"`
	ConfiguredAgents int      `json:"configured_agents"`
	RequiredPaths    []string `json:"required_paths"`
	Origin           string   `json:"origin"`
	PassKey          string   `json:"pass_key"`
}

// supplementalPassStartKey is the start row's identity: one start per attempt and
// head on a stage.
func supplementalPassStartKey(attempt, headSHA string) string {
	return "supplemental-pass-v1|" + attempt + "|" + headSHA
}

// exemptionPaths lists a dispatcher's exemption set as a start row's required_paths;
// an empty set is `[]`, never null.
func exemptionPaths(exemptions []prompt.GateScopeExemption) []string {
	paths := make([]string, 0, len(exemptions))
	for _, exemption := range exemptions {
		paths = append(paths, exemption.Path)
	}
	return paths
}

// supplementalPassAllocation is allocateSupplementalPass's result. Only the winner
// may invoke reviewers. sequence names the surviving row on a lost allocation.
type supplementalPassAllocation struct {
	won      bool
	sequence int64
}

// allocateSupplementalPass atomically records one attempt's start before any
// reviewer runs, so two processes that both read no start cannot both invoke
// reviewers. It is the category's only writer.
func (s *Server) allocateSupplementalPass(ctx context.Context, runID, stageID uuid.UUID, headSHA, attempt string, configuredAgents int, requiredPaths []string) (supplementalPassAllocation, error) {
	if attempt == "" || headSHA == "" {
		return supplementalPassAllocation{}, errSupplementalPassIdentityUnknown
	}
	appender, ok := supplementalStartAppender(s.cfg.AuditRepo)
	if !ok {
		return supplementalPassAllocation{}, errSupplementalStartNotAtomic
	}
	payload, _ := json.Marshal(supplementalReviewStartedPayload{
		RunID:            runID.String(),
		StageID:          stageID.String(),
		StageAttempt:     attempt,
		HeadSHA:          headSHA,
		ConfiguredAgents: configuredAgents,
		RequiredPaths:    requiredPaths,
		Origin:           planreview.OriginBaseRebaseReinvoke,
		PassKey:          supplementalPassStartKey(attempt, headSHA),
	})
	systemKind := audit.ActorKind("system")
	params := audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategorySupplementalReviewStarted,
		ActorKind: &systemKind,
		Payload:   payload,
	}
	_, err := appender.AppendChainedDeduped(ctx, params, audit.DedupeSpec{StageID: &stageID, PayloadKey: supplementalPassKeyField, PayloadValue: supplementalPassStartKey(attempt, headSHA)})
	var dup *audit.DedupedDuplicateError
	if errors.As(err, &dup) {
		var survivor supplementalReviewStartedPayload
		if derr := json.Unmarshal(dup.Existing.Payload, &survivor); derr != nil {
			return supplementalPassAllocation{}, fmt.Errorf("decode surviving start: %w", derr)
		}
		if survivor.StageAttempt != attempt || survivor.HeadSHA != headSHA {
			return supplementalPassAllocation{}, fmt.Errorf("surviving start sequence %d names another attempt or head", dup.Existing.Sequence)
		}
		return supplementalPassAllocation{sequence: dup.Existing.Sequence}, nil
	}
	if err != nil {
		return supplementalPassAllocation{}, fmt.Errorf("record supplemental start: %w", err)
	}
	return supplementalPassAllocation{won: true}, nil
}

// nonGatingAuthority is true only for advisory and gateless verdicts, which have
// never blocked. A verdict with no recorded authority still binds.
func nonGatingAuthority(authority planreview.AuthorityMode) bool {
	return authority == planreview.AuthorityAdvisory || authority == planreview.AuthorityGateless
}

// supplementalVerdictAtHead is the ONE decode of a supplemental verdict: the
// round read and the edge guard both read a row through it.
func supplementalVerdictAtHead(row run.StageEdgeGuardRow, stageID uuid.UUID, headSHA string) (implementReviewedEvidence, bool, error) {
	if row.Category != implementReviewedCategory || row.StageID == nil || *row.StageID != stageID {
		return implementReviewedEvidence{}, false, nil
	}
	var payload implementReviewedEvidence
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		return implementReviewedEvidence{}, false, fmt.Errorf("decode %s sequence %d: %w", implementReviewedCategory, row.Sequence, err)
	}
	if payload.Origin != planreview.OriginBaseRebaseReinvoke || payload.HeadSHA != headSHA {
		return implementReviewedEvidence{}, false, nil
	}
	return payload, true, nil
}

// supplementalRoundSnapshot is what recorded rows prove about a pass at one head
// on one attempt.
type supplementalRoundSnapshot struct {
	// rejected is a binding reject at this head from ANY attempt, legacy rows included.
	rejected bool
	// currentVerdicts counts verdicts stamped with this attempt, whatever their time.
	currentVerdicts int
	// startedThisAttempt and startedAgents come from this attempt's start row.
	startedThisAttempt bool
	startedAgents      int
}

// supplementalPassDischarged is the only discharge: this attempt's start, its full
// verdict set stamped with this attempt's captured token, and no reject at this
// head from any attempt. A pull_request_opened row or a newer artifact proves nothing.
func supplementalPassDischarged(state supplementalRoundSnapshot) bool {
	return !state.rejected && state.startedThisAttempt && state.startedAgents > 0 && state.currentVerdicts >= state.startedAgents
}

// supplementalRoundFromRows reduces rows to the pass's state. A legacy verdict with
// no recorded attempt counts only on an unstamped stage; on an anchored stage it is
// unknown and settles nothing, while its reject still binds. An undecodable row is
// an error, never an empty state.
func supplementalRoundFromRows(rows []run.StageEdgeGuardRow, stageID uuid.UUID, headSHA, attempt string) (supplementalRoundSnapshot, error) {
	var state supplementalRoundSnapshot
	for _, row := range rows {
		switch row.Category {
		case implementReviewedCategory:
			payload, matched, err := supplementalVerdictAtHead(row, stageID, headSHA)
			if err != nil {
				return supplementalRoundSnapshot{}, err
			}
			if !matched {
				continue
			}
			if payload.Verdict == planreview.VerdictReject && !nonGatingAuthority(payload.Authority) {
				state.rejected = true
			}
			if payload.StageAttempt != attempt {
				continue
			}
			state.currentVerdicts++
		case CategorySupplementalReviewStarted:
			if row.StageID == nil || *row.StageID != stageID {
				continue
			}
			var started supplementalReviewStartedPayload
			if err := json.Unmarshal(row.Payload, &started); err != nil {
				return supplementalRoundSnapshot{}, fmt.Errorf("decode %s sequence %d: %w", CategorySupplementalReviewStarted, row.Sequence, err)
			}
			if started.PassKey != supplementalPassStartKey(started.StageAttempt, started.HeadSHA) || started.ConfiguredAgents < 1 {
				return supplementalRoundSnapshot{}, fmt.Errorf("%s sequence %d is not a valid start", CategorySupplementalReviewStarted, row.Sequence)
			}
			if started.StageAttempt == attempt && started.HeadSHA == headSHA {
				state.startedThisAttempt = true
				state.startedAgents = started.ConfiguredAgents
			}
		}
	}
	return state, nil
}

// listGuardRows reads categories in the row shape a guard's Decide reads, so an
// outside read and the edge reach the same reducer. A list error is returned,
// never read as no evidence.
func (s *Server) listGuardRows(ctx context.Context, runID uuid.UUID, categories []string) ([]run.StageEdgeGuardRow, error) {
	var rows []run.StageEdgeGuardRow
	for _, category := range categories {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, category)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", category, err)
		}
		for _, entry := range entries {
			rows = append(rows, run.StageEdgeGuardRow{Category: category, Sequence: entry.Sequence, StageID: entry.StageID, Payload: entry.Payload})
		}
	}
	return rows, nil
}

// supplementalRoundState decides dispatch only; the terminal edge re-decides under
// the run lock. Any list or decode error is returned.
func (s *Server) supplementalRoundState(ctx context.Context, runID, stageID uuid.UUID, headSHA, attempt string) (supplementalRoundSnapshot, error) {
	rows, err := s.listGuardRows(ctx, runID, []string{implementReviewedCategory, CategorySupplementalReviewStarted})
	if err != nil {
		return supplementalRoundSnapshot{}, err
	}
	return supplementalRoundFromRows(rows, stageID, headSHA, attempt)
}

// reviewObligationCheck is the one reducer's answer at a recorded head.
type reviewObligationCheck struct {
	// missing lists the newest recorded required set's paths the body does not carry.
	missing []string
	// discharged is the completion proof.
	discharged bool
	// startedIncomplete is a recorded start on this attempt without its full verdict
	// set: a lost pass, whatever paths the body carries.
	startedIncomplete bool
	// err is a decode fault. No other field may be read when it is set.
	err error
}

// requiredSetFromRow reads the obligation a carrier row records. A row carrying
// none returns the zero value. Only these three categories can carry one, and
// known-none on a parked row carries no required set.
func requiredSetFromRow(row run.StageEdgeGuardRow) (reviewObligation, error) {
	switch row.Category {
	case CategoryReviewObligationCheckpoint:
		var carrier struct {
			PushCheckpoint struct {
				ReviewObligation *reviewObligation `json:"review_obligation"`
			} `json:"push_checkpoint"`
		}
		if err := json.Unmarshal(row.Payload, &carrier); err != nil {
			return reviewObligation{}, fmt.Errorf("decode %s sequence %d: %w", row.Category, row.Sequence, err)
		}
		if carrier.PushCheckpoint.ReviewObligation != nil {
			return *carrier.PushCheckpoint.ReviewObligation, nil
		}
	case CategoryScopeCompletenessParked:
		var parked struct {
			ReviewObligation *reviewObligation `json:"review_obligation"`
		}
		if err := json.Unmarshal(row.Payload, &parked); err != nil {
			return reviewObligation{}, fmt.Errorf("decode %s sequence %d: %w", row.Category, row.Sequence, err)
		}
		if parked.ReviewObligation != nil {
			return *parked.ReviewObligation, nil
		}
	case CategorySupplementalReviewStarted:
		var started supplementalReviewStartedPayload
		if err := json.Unmarshal(row.Payload, &started); err != nil {
			return reviewObligation{}, fmt.Errorf("decode %s sequence %d: %w", row.Category, row.Sequence, err)
		}
		required := make([]scopeExemption, 0, len(started.RequiredPaths))
		for _, path := range started.RequiredPaths {
			required = append(required, scopeExemption{Path: path})
		}
		return reviewObligation{HeadSHA: started.HeadSHA, StageAttempt: started.StageAttempt, SupplementalScopeExemptions: required}, nil
	}
	return reviewObligation{}, nil
}

// obligationCheckFromRows is the ONE reducer behind requiredReviewObligationMissing
// and the terminal edge's guard. It is pure: the guard calls it under the run lock.
// The newest required set at this head wins; a recorded path the body does not
// carry is missing.
func obligationCheckFromRows(rows []run.StageEdgeGuardRow, stageID uuid.UUID, headSHA, attempt string, bodyExemptions []scopeExemption) reviewObligationCheck {
	var check reviewObligationCheck
	state, err := supplementalRoundFromRows(rows, stageID, headSHA, attempt)
	if err != nil {
		check.err = err
		return check
	}
	check.discharged = supplementalPassDischarged(state)
	check.startedIncomplete = state.startedThisAttempt && !check.discharged

	var required []scopeExemption
	var newest int64
	for _, row := range rows {
		if row.StageID == nil || *row.StageID != stageID {
			continue
		}
		obligation, err := requiredSetFromRow(row)
		if err != nil {
			check.err = err
			return check
		}
		if obligation.HeadSHA != headSHA {
			continue
		}
		if reviewObligationKnowledge(&obligation) != obligationKnownRequired {
			continue
		}
		if required == nil || row.Sequence >= newest {
			required, newest = obligation.SupplementalScopeExemptions, row.Sequence
		}
	}
	for _, exemption := range required {
		if !slices.ContainsFunc(bodyExemptions, func(carried scopeExemption) bool { return carried.Path == exemption.Path }) {
			check.missing = append(check.missing, exemption.Path)
		}
	}
	return check
}

// requiredReviewObligationMissing is the server-side refusal behind "never
// silently discard" (D9). It reads no pull_request_opened row and no artifact:
// neither is evidence a required review completed. A read or decode fault is an
// error, which changes nothing.
func (s *Server) requiredReviewObligationMissing(ctx context.Context, runID uuid.UUID, stage *run.Stage, headSHA string, bodyExemptions []scopeExemption) (reviewObligationCheck, error) {
	rows, err := s.listGuardRows(ctx, runID, supplementalEdgeCategories)
	if err != nil {
		return reviewObligationCheck{}, err
	}
	check := obligationCheckFromRows(rows, stage.ID, headSHA, run.StageAttemptToken(stage.DispatchedAt), bodyExemptions)
	return check, check.err
}

// supplementalPassLost reports whether a replayed gating upload must be refused:
// a required pass whose recorded rows hold a reject or no completion proof. An
// advisory or no-pass configuration is never lost. An unreadable requirement or
// round fails closed; the caller's refusal changes nothing.
func (s *Server) supplementalPassLost(ctx context.Context, runID uuid.UUID, stage *run.Stage, pr *pullRequestBody) bool {
	if stage.Type != run.StageTypeImplement || stage.State != run.StageStateRunning || len(pr.SupplementalScopeExemptions) == 0 {
		return false
	}
	warnLost := func(cause error) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "supplemental pass lost check: read failed; treating the pass as lost",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", cause.Error()))
	}
	req := s.resolveSupplementalPassRequirement(ctx, runID)
	if req.state == passRequirementUnknown {
		warnLost(req.err)
		return true
	}
	if req.state != passRequiredGating {
		return false
	}
	state, err := s.supplementalRoundState(ctx, runID, stage.ID, pr.HeadSHA, run.StageAttemptToken(stage.DispatchedAt))
	if err != nil {
		warnLost(err)
		return true
	}
	return !supplementalPassDischarged(state)
}

// supplementalHeadRejectedError is a binding reject recorded at the edge's head.
type supplementalHeadRejectedError struct {
	headSHA string
}

func (e *supplementalHeadRejectedError) Error() string {
	return "supplemental review: a binding reject is recorded at head " + e.headSHA
}

// reviewObligationUnmetError is a recorded required path a body does not carry,
// with no completion proof.
type reviewObligationUnmetError struct {
	missing []string
}

func (e *reviewObligationUnmetError) Error() string {
	return fmt.Sprintf("supplemental review: %d required paths have no completed review", len(e.missing))
}

// supplementalPassIncompleteError is a recorded start on this attempt without its
// full verdict set: D8's lost pass.
type supplementalPassIncompleteError struct {
	headSHA string
}

func (e *supplementalPassIncompleteError) Error() string {
	return "supplemental review: the recorded pass at head " + e.headSHA + " has no complete verdict set"
}

// recordedRejectAtHead is the head-reject predicate, for a reject from any attempt,
// legacy rows included. Other heads, stages and origins do not match.
func recordedRejectAtHead(rows []run.StageEdgeGuardRow, stageID uuid.UUID, headSHA string) error {
	for _, row := range rows {
		verdict, matched, err := supplementalVerdictAtHead(row, stageID, headSHA)
		if err != nil {
			return err
		}
		if !matched {
			continue
		}
		if verdict.Verdict == planreview.VerdictReject && !nonGatingAuthority(verdict.Authority) {
			return &supplementalHeadRejectedError{headSHA: headSHA}
		}
	}
	return nil
}

// supplementalHeadRejectGuard refuses an edge over a reject recorded at this head
// from any attempt. A state and attempt compare-and-swap cannot see a reject another
// attempt commits after this attempt's last read; this guard reads under the lock.
func supplementalHeadRejectGuard(stageID uuid.UUID, headSHA string) run.StageEdgeGuard {
	return run.StageEdgeGuard{
		Categories: []string{implementReviewedCategory},
		Decide: func(rows []run.StageEdgeGuardRow) error {
			return recordedRejectAtHead(rows, stageID, headSHA)
		},
	}
}

// decideSupplementalEdge applies the success edge's three predicates in order: a
// binding reject, an unmet obligation, a started pass without its verdict set.
func decideSupplementalEdge(rows []run.StageEdgeGuardRow, stageID uuid.UUID, headSHA, attempt string, bodyExemptions []scopeExemption) error {
	if err := recordedRejectAtHead(rows, stageID, headSHA); err != nil {
		return err
	}
	check := obligationCheckFromRows(rows, stageID, headSHA, attempt, bodyExemptions)
	if check.err != nil {
		return check.err
	}
	if len(check.missing) > 0 && !check.discharged {
		return &reviewObligationUnmetError{missing: check.missing}
	}
	if check.startedIncomplete {
		return &supplementalPassIncompleteError{headSHA: headSHA}
	}
	return nil
}

// supplementalEdgeGuard is the terminal edge's evidence check for every plain,
// claimed and healed upload. Its Decide runs under the run lock, which closes both
// the reject race and the admission race where a start records a required path
// after the upload passed its refusal. The caller sets LegacyIdentity.
func supplementalEdgeGuard(stageID uuid.UUID, headSHA, attempt string, bodyExemptions []scopeExemption) run.StageEdgeGuard {
	return run.StageEdgeGuard{
		Categories: supplementalEdgeCategories,
		Decide: func(rows []run.StageEdgeGuardRow) error {
			return decideSupplementalEdge(rows, stageID, headSHA, attempt, bodyExemptions)
		},
	}
}

// variantModeCategories are the rows that decide whether a runner-spelled outcome's
// mode is real. Each producer is server-owned; the runner's spelling proves nothing.
var variantModeCategories = []string{
	CategoryStageFixupTriggered,
	CategoryFixupPushed,
	CategoryFixupNoChanges,
	CategoryStageFixupRecovered,
	CategoryStageFixupSuperseded,
	CategoryStageConflictResolutionTriggered,
	CategoryStageConflictResolutionFailed,
	CategoryConflictResolutionPushed,
	CategoryStageMergeCandidateVerifyTriggered,
}

// variantModeProved reports whether server evidence proves outcome's mode. pushed
// needs a real child. The others need the stage's newest mode row to be their live
// trigger: a consumed trigger or a later re-open of another kind proves nothing,
// while a retried pass of a live trigger stays proved, as the prompt still serves it.
func variantModeProved(outcome string, rows []run.StageEdgeGuardRow, stageID uuid.UUID, isChild bool) (bool, error) {
	var wantTrigger string
	switch outcome {
	case "pushed":
		return isChild, nil
	case "fixup_pushed", "fixup_no_changes":
		wantTrigger = CategoryStageFixupTriggered
	case "conflict_resolution_pushed":
		wantTrigger = CategoryStageConflictResolutionTriggered
	default:
		return false, nil
	}
	var newest *run.StageEdgeGuardRow
	for i := range rows {
		row := &rows[i]
		if !slices.Contains(variantModeCategories, row.Category) || row.StageID == nil || *row.StageID != stageID {
			continue
		}
		if newest == nil || row.Sequence >= newest.Sequence {
			newest = row
		}
	}
	if newest == nil {
		return false, nil
	}
	if newest.Category != wantTrigger {
		return false, nil
	}
	if wantTrigger == CategoryStageConflictResolutionTriggered {
		var conflictTrigger conflictResolutionTrigger
		if err := json.Unmarshal(newest.Payload, &conflictTrigger); err != nil {
			return false, fmt.Errorf("decode %s sequence %d: %w", newest.Category, newest.Sequence, err)
		}
		return conflictTrigger.populated(), nil
	}
	var fixupTrigger struct {
		Concerns []planreview.Concern `json:"concerns"`
	}
	if err := json.Unmarshal(newest.Payload, &fixupTrigger); err != nil {
		return false, fmt.Errorf("decode %s sequence %d: %w", newest.Category, newest.Sequence, err)
	}
	if len(fixupTrigger.Concerns) == 0 {
		return false, nil
	}
	return true, nil
}

// recordedEvidenceHeads lists every head this stage's rows record supplemental
// evidence at, in a stable order. An unproved claim's own head is untrusted, so the
// reducer runs at each of these instead.
func recordedEvidenceHeads(rows []run.StageEdgeGuardRow, stageID uuid.UUID) ([]string, error) {
	var heads []string
	for _, row := range rows {
		if row.StageID == nil || *row.StageID != stageID {
			continue
		}
		var head string
		if row.Category == implementReviewedCategory {
			var verdict implementReviewedEvidence
			if err := json.Unmarshal(row.Payload, &verdict); err != nil {
				return nil, fmt.Errorf("decode %s sequence %d: %w", row.Category, row.Sequence, err)
			}
			head = verdict.HeadSHA
		} else {
			obligation, err := requiredSetFromRow(row)
			if err != nil {
				return nil, err
			}
			head = obligation.HeadSHA
		}
		if !slices.Contains(heads, head) {
			heads = append(heads, head)
		}
	}
	slices.Sort(heads)
	return heads, nil
}

// variantEdgeGuard is the terminal edge's evidence check for the four alternative
// outcomes. A proved mode applies the success predicates at its own head, so a real
// fix-up at a new head never fails on an old reject; a proved no-change report has
// no head and passes. An unproved claim's head is untrusted, so recorded binding
// evidence at any head refuses it, while nothing recorded proceeds as today: D9's
// runner-attested boundary, not a defence against a compromised runner (#5078).
func variantEdgeGuard(outcome string, stageID uuid.UUID, headSHA, attempt string, isChild bool) run.StageEdgeGuard {
	return run.StageEdgeGuard{
		Categories:     append(slices.Clone(supplementalEdgeCategories), variantModeCategories...),
		LegacyIdentity: attempt == "",
		Decide: func(rows []run.StageEdgeGuardRow) error {
			proved, proofErr := variantModeProved(outcome, rows, stageID, isChild)
			if proofErr != nil {
				return proofErr
			}
			if !proved {
				heads, err := recordedEvidenceHeads(rows, stageID)
				if err != nil {
					return err
				}
				for _, head := range heads {
					if err := decideSupplementalEdge(rows, stageID, head, attempt, nil); err != nil {
						return err
					}
				}
				return nil
			}
			if headSHA == "" {
				return nil
			}
			return decideSupplementalEdge(rows, stageID, headSHA, attempt, nil)
		},
	}
}
