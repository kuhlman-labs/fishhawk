package server

// On-approval comms apply (E81.5 / #3775, phase 7 #4017).
//
// The comms scan's last phase: when the captain DECIDES the plan stage that
// carries the run's recorded comms_report, this hook settles the report's
// disposition-capture window and, on a ratified approve only, files every
// captain-approved draft through the SAME work-item filing core every other
// auto-file path funnels through (applyAndFileWorkItem). It NEVER creates a
// run, and it never labels, edits, closes or comments on a SOURCE report: a
// draft becomes a NEW tracker issue and nothing else. Nothing in this file
// references a run-creation path, a grooming mutator, a transitioner or an
// issue-comment call.
//
// It mirrors applyApprovedUpkeep (upkeep_apply.go) on the shared
// proposal-report seam (report_seam.go): same placement in the TYPE-only plan
// block of approvals.go, same passed decision, same detach-at-entry, same
// prelaunch budget, same Shutdown-drained detached half.
//
// THE LADDER, in evaluation order:
//
//	E0 REPORT    (an early-out, not a control) no comms_report artifact on the
//	             decided stage, no comms_report_recorded row on the run, or the
//	             recorded row names an artifact that is NOT one of this stage's
//	             comms_report artifacts → return having written nothing.
//	C1 DECISION  a decision other than approve settles the window `rejected`
//	             BEFORE any report-body read and files NOTHING. On a settled
//	             window it records `rejected` suppressions: for the drafts the
//	             captain individually rejected, or — when the consumed set holds
//	             NO disposition for this artifact (a whole-gate reject) — for
//	             every draft's cited reports. A FAILED settlement leaves the
//	             consumed set UNKNOWN, not empty: it writes one degraded row
//	             (comms_apply_window_unsettled) and suppresses nothing. The
//	             cursor never moves on a reject.
//	C3 RATIFY    the report must still resolve through latestCommsReport to the
//	             row's artifact, and the gate must carry >= 1 grant and 0
//	             rejections (reportGateRatified). Either failure degrades with
//	             the window left OPEN.
//
// Then the window settles `approved`, and every later degrade is
// POST-RATIFICATION: the window stays CLOSED and the cursor is untouched.
//
// THE DETACHED LOOP takes a per-(account, repo) lock whose wait is bounded by
// the apply budget (acquireCommsApplyLock), reads the double-filing guard
// INSIDE it, then writes exactly one comms_draft_filed or comms_draft_skipped
// row per draft in REPORT order, first match wins:
//
//	undecided              no consumed disposition
//	rejected               the consumed verdict is rejected
//	already_filed          this run already filed this artifact's draft
//	filed_elsewhere        a cited (id, gathered hash) already carries a
//	                       `filed` suppression or appears in another
//	                       comms_draft_filed record
//	parent_epic_is_source  the effective parent epic is an issue a cited
//	                       report lives on
//	apply_budget_exhausted the detached budget expired before this draft
//	filing_failed          the recorded filing_body_digest is absent, the
//	                       re-rendered body drifted from it, the charter the
//	                       recorded body quoted is unreadable now, or the
//	                       filing core refused
//
// It ends with ONE comms_apply_completed row carrying the suppressions the
// gather's memory reads (loadCommsSuppressionMemory) and the cursor outcome.
// Long-form contract: backend/internal/server/README.md § "On-approval comms
// apply"; docs/spec/comms-report-v1.md § "Apply".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/userreport"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// CategoryCommsDraftSkipped is the per-draft skip row (registered by #4012).
// comms_draft_filed and comms_apply_completed are declared in comms_record.go,
// which reads them.
const CategoryCommsDraftSkipped = "comms_draft_skipped"

// The completion row's decision values.
const (
	commsApplyDecisionApprove = "approve"
	commsApplyDecisionReject  = "reject"
)

// The closed set of per-draft skip reasons (payload VALUES).
const (
	commsSkipUndecided          = "undecided"
	commsSkipRejected           = "rejected"
	commsSkipAlreadyFiled       = "already_filed"
	commsSkipFiledElsewhere     = "filed_elsewhere"
	commsSkipParentEpicIsSource = "parent_epic_is_source"
	commsSkipBudgetExhausted    = "apply_budget_exhausted"
	commsSkipFilingFailed       = "filing_failed"
)

// The filing_failed codes this file mints itself; every other code is the
// filing core's *workItemError code.
const (
	// commsFilingDigestAbsent: the recorded preview carries no
	// filing_body_digest for the draft, so there is nothing to prove the
	// filed bytes are the reviewed ones.
	commsFilingDigestAbsent = "filing_body_digest_absent"
	// commsFilingBodyDrift: the re-rendered body hashes differently from the
	// recorded digest (the charter changed since ingest, or the renderer did).
	commsFilingBodyDrift = "filing_body_drift"
	// commsFilingCharterUnavailable: the recorded body quoted the charter's
	// rubric text and the charter cannot be read NOW (a transient read
	// failure), so the reviewed body cannot be reproduced.
	commsFilingCharterUnavailable = "charter_unavailable"
)

// The closed set of degrade reasons carried on a degraded
// comms_apply_completed row.
const (
	// PRE-ratification (the window stays OPEN on approve):
	commsApplyReportUnreadable = "comms_apply_report_unreadable"
	commsApplyNotRatified      = "comms_apply_not_ratified"
	// commsApplyWindowUnsettled: the watermark did not land. On approve the
	// window stays open; on reject the consumed set is unknown, so nothing
	// is suppressed.
	commsApplyWindowUnsettled = "comms_apply_window_unsettled"
	// POST-ratification (the window stays CLOSED, the cursor untouched):
	commsApplyRunUnreadable          = "comms_apply_run_unreadable"
	commsApplyAccountUnparseable     = "comms_apply_account_unparseable"
	commsApplyRepoUnresolvable       = "comms_apply_repo_unresolvable"
	commsApplyConventionsUnavailable = "comms_apply_conventions_unavailable"
	commsApplyPriorFilingsUnreadable = "comms_apply_prior_filings_unreadable"
	// commsApplyGuardUnavailable: the double-filing guard (suppression memory
	// or draft-filed records) could not be read inside the lock.
	commsApplyGuardUnavailable = "comms_apply_guard_unavailable"
	// commsApplyLockWaitExhausted: the apply budget expired while queued for
	// the per-repository lock; every draft is recorded apply_budget_exhausted.
	commsApplyLockWaitExhausted = "comms_apply_lock_wait_exhausted"
	// commsApplyPrelaunchTimeout: the synchronous half did not finish inside
	// commsApplyPrelaunchBudget, whichever rung reported the failure.
	commsApplyPrelaunchTimeout = "comms_apply_prelaunch_timeout"
)

// The completion row's cursor.reason values: why the cursor did not advance.
const (
	commsCursorReasonDecisionReject = "decision_reject"
	commsCursorReasonApplyDegraded  = "apply_degraded"
	commsCursorReasonScanIncomplete = "scan_incomplete"
	commsCursorReasonStoreUnwired   = "cursor_store_unwired"
	commsCursorReasonAdvanceFailed  = "advance_failed"
)

// commsConflictBasisDraftFiled is a filed_elsewhere conflict resting on
// another artifact's comms_draft_filed record (the other basis is `filed`).
const commsConflictBasisDraftFiled = "draft_filed"

// commsApplyLockWaitExhaustedDetail is the message on each draft's row when
// the lock wait exhausted the budget.
const commsApplyLockWaitExhaustedDetail = "the apply budget expired while queued behind another apply for this repository"

// commsApplyPrelaunchBudget bounds the SYNCHRONOUS half of the hook (the part
// still on the captain's approve request) and each sink write. A var ONLY so
// a test can shrink it.
var commsApplyPrelaunchBudget = 30 * time.Second

// commsApplyBudget is the FLOOR of the detached loop's budget (lock wait
// included) and commsApplyPerDraftBudget its per-draft scale. Vars ONLY so a
// test can shrink them.
var (
	commsApplyBudget         = 3 * time.Minute
	commsApplyPerDraftBudget = 3 * time.Second
)

// commsApplyBudgetFor is the PURE budget for a loop over `drafts` entries: the
// floor, or the per-draft scale, whichever is larger.
func commsApplyBudgetFor(drafts int) time.Duration {
	scaled := time.Duration(drafts) * commsApplyPerDraftBudget
	if scaled > commsApplyBudget {
		return scaled
	}
	return commsApplyBudget
}

// Test-only hooks, nil in production. commsApplyBeforeLock runs on the
// detached goroutine just before it queues for the per-repository lock;
// commsApplyAfterLoop runs after the completion row is written. Tests that
// set them do not run in parallel.
var (
	commsApplyBeforeLock func(runID uuid.UUID)
	commsApplyAfterLoop  func(runID uuid.UUID)
)

// commsDraftFiledPayload is the comms_draft_filed row. Its first four keys are
// the READ contract commsDraftFiledRecord decodes (repo, issue_number,
// comment_id — 0, an issue — and reports with the GATHERED hashes); the rest
// is the audit record. AppliedLabels are the labels ACTUALLY filed.
type commsDraftFiledPayload struct {
	Repo        string                    `json:"repo"`
	IssueNumber int                       `json:"issue_number"`
	CommentID   int64                     `json:"comment_id"`
	Reports     []userreport.MarkedReport `json:"reports"`

	RunID                   string             `json:"run_id"`
	StageID                 string             `json:"stage_id"`
	ArtifactID              string             `json:"artifact_id"`
	DraftID                 string             `json:"draft_id"`
	IssueURL                string             `json:"issue_url,omitempty"`
	Provider                string             `json:"provider"`
	Title                   string             `json:"title"`
	ParentEpic              string             `json:"parent_epic,omitempty"`
	AppliedLabels           []string           `json:"applied_labels"`
	StrippedLabels          []string           `json:"stripped_labels"`
	SuppressedDefaultLabels []string           `json:"suppressed_default_labels"`
	FilingBodyDigest        string             `json:"filing_body_digest"`
	PriorSuppressions       []commsSuppression `json:"prior_suppressions"`
}

// commsConflictingReport is one cited report a filed_elsewhere skip rests on:
// basis `filed` (a prior apply's suppression) or `draft_filed` (another
// artifact's comms_draft_filed record, IssueNumber its filed item).
type commsConflictingReport struct {
	ID          string `json:"id"`
	ContentHash string `json:"content_hash"`
	Basis       string `json:"basis"`
	IssueNumber int    `json:"issue_number,omitempty"`
}

// commsDraftSkippedPayload is the comms_draft_skipped row; the optional fields
// are populated per skip reason.
type commsDraftSkippedPayload struct {
	RunID              string                   `json:"run_id"`
	StageID            string                   `json:"stage_id"`
	ArtifactID         string                   `json:"artifact_id"`
	DraftID            string                   `json:"draft_id"`
	SkipReason         string                   `json:"skip_reason"`
	Code               string                   `json:"code,omitempty"`
	Message            string                   `json:"message,omitempty"`
	PriorIssueNumber   int                      `json:"prior_issue_number,omitempty"`
	ConflictingReports []commsConflictingReport `json:"conflicting_reports,omitempty"`
}

// commsApplyCounts: on a non-degraded approve row filed + skipped + failed +
// budget_exhausted == drafts (skipped counts undecided / rejected /
// already_filed / filed_elsewhere / parent_epic_is_source; failed counts
// filing_failed).
type commsApplyCounts struct {
	Drafts          int `json:"drafts"`
	Filed           int `json:"filed"`
	Skipped         int `json:"skipped"`
	Failed          int `json:"failed"`
	BudgetExhausted int `json:"budget_exhausted"`
}

// commsApplySuppression is one written suppression. id / hash / basis are the
// keys loadCommsSuppressionMemory decodes (commsSuppression); entry_id names
// the draft or n_drift entry it came from.
type commsApplySuppression struct {
	ID      string `json:"id"`
	Hash    string `json:"hash"`
	Basis   string `json:"basis"`
	EntryID string `json:"entry_id"`
}

// commsApplyCursorOutcome is what the apply did to the user-report cursor.
// Advanced is true only when both Advance calls returned without error;
// Cursor / NoteCursor are the values COMMITTED (a later stored value wins).
type commsApplyCursorOutcome struct {
	Advanced      bool       `json:"advanced"`
	Cursor        *time.Time `json:"cursor,omitempty"`
	NoteCursor    *time.Time `json:"note_cursor,omitempty"`
	HeldReportIDs []string   `json:"held_report_ids"`
	Reason        string     `json:"reason,omitempty"`
	Error         string     `json:"error,omitempty"`
}

// commsApplyCompletedPayload is the ONE comms_apply_completed row per apply.
// Every row — degraded ones included — carries repo and a suppressions array
// (empty on a degraded row).
type commsApplyCompletedPayload struct {
	Repo           string                  `json:"repo"`
	ArtifactID     string                  `json:"artifact_id,omitempty"`
	Decision       string                  `json:"decision"`
	Counts         commsApplyCounts        `json:"counts"`
	Degraded       bool                    `json:"degraded"`
	DegradeReason  string                  `json:"degrade_reason,omitempty"`
	GuardTruncated bool                    `json:"guard_truncated,omitempty"`
	Suppressions   []commsApplySuppression `json:"suppressions"`
	Cursor         commsApplyCursorOutcome `json:"cursor"`
}

// commsConsumedDisposition is one collapsed captain verdict consumed from the
// closed window.
type commsConsumedDisposition struct {
	Verdict    string
	ParentEpic string
}

// commsApplySink writes the apply's rows onto the run's chain: actor system,
// the decided stage's id, payloads marshalled BARE, each on its OWN bounded
// context detached from the caller's (an expired budget never loses its own
// row). repo is stamped onto every completion row.
type commsApplySink struct {
	s       *Server
	runID   uuid.UUID
	stageID uuid.UUID
	repo    string
}

func (k *commsApplySink) append(ctx context.Context, category string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commsApplyPrelaunchBudget)
	defer cancel()
	systemKind := audit.ActorSystem
	stageID := k.stageID
	_, aerr := k.s.cfg.AuditRepo.AppendChained(wctx, audit.ChainAppendParams{
		RunID:     k.runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  category,
		ActorKind: &systemKind,
		Payload:   body,
	})
	return aerr
}

// record appends one row, logging (never aborting) on failure. A lost
// comms_draft_filed row is the at-least-once residual: a later apply of the
// same artifact could file the draft again, identifiable by its marker.
func (k *commsApplySink) record(ctx context.Context, category string, payload any) {
	if err := k.append(ctx, category, payload); err != nil {
		k.s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "comms apply: audit row not recorded",
			slog.String("run_id", k.runID.String()),
			slog.String("stage_id", k.stageID.String()),
			slog.String("category", category),
			slog.String("error", err.Error()))
	}
}

// commsFilingJob is everything the detached loop needs, resolved on the
// synchronous half so the goroutine never touches the request.
type commsFilingJob struct {
	runID, stageID uuid.UUID
	artifactID     string
	accountID      *uuid.UUID
	repo           string
	report         *plan.CommsReport
	gather         *commsScanGatheredPayload
	unaccounted    []string
	consumed       map[string]commsConsumedDisposition
	digests        map[string]string
	priorFiled     map[string]int
	rubricText     map[string]string
	// charterUnavailable: the recorded body quoted rubric text and the
	// charter is unreadable now, so a drifted digest is charter_unavailable.
	charterUnavailable bool
	env                commsPreviewEnv
	sink               *commsApplySink
}

// applyApprovedComms is the on-approval hook. See the file header for the
// ladder; it is best-effort and never unwinds the approval.
func (s *Server) applyApprovedComms(ctx context.Context, stage *run.Stage, decision approval.Decision) {
	if stage == nil || s.cfg.ArtifactRepo == nil || s.cfg.AuditRepo == nil {
		return
	}

	// DETACH AT ENTRY, then BOUND the synchronous half.
	base := context.WithoutCancel(ctx)
	ctx, prelaunchCancel := context.WithTimeout(base, commsApplyPrelaunchBudget)
	defer prelaunchCancel()

	// E0: does this stage carry a comms_report at all? An unreadable list is
	// silent: it must not degrade an ordinary plan approval.
	stageArts, lerr := s.cfg.ArtifactRepo.ListForStage(ctx, stage.ID)
	if lerr != nil {
		return
	}
	onStage := map[uuid.UUID]bool{}
	for _, a := range stageArts {
		if a != nil && a.Kind == artifact.KindCommsReport {
			onStage[a.ID] = true
		}
	}
	if len(onStage) == 0 {
		return
	}

	sink := &commsApplySink{s: s, runID: stage.RunID, stageID: stage.ID}
	// The run row, read once: its repo stamps every row, and an unreadable
	// run degrades only POST-ratification (below).
	var rn *run.Run
	rnErr := errors.New("no run repository is configured")
	if s.cfg.RunRepo != nil {
		rn, rnErr = s.cfg.RunRepo.GetRun(ctx, stage.RunID)
		if rnErr == nil && rn != nil {
			sink.repo = rn.Repo
		}
	}
	decisionName := commsApplyDecisionApprove
	if decision != approval.DecisionApprove {
		decisionName = commsApplyDecisionReject
	}

	boundID, found, berr := s.recordedReportArtifact(ctx, stage.RunID, CategoryCommsReportRecorded)
	if berr != nil {
		s.degradeCommsApply(ctx, sink, "", decisionName, commsApplyReportUnreadable, berr.Error())
		return
	}
	if !found || !onStage[boundID] {
		return
	}
	artifactID := boundID.String()

	// C1: a decision other than approve files NOTHING.
	if decision != approval.DecisionApprove {
		s.applyRejectedComms(ctx, sink, stage, boundID)
		return
	}

	b, rerr := s.latestCommsReport(ctx, stage.RunID)
	if rerr != nil {
		s.degradeCommsApply(ctx, sink, artifactID, decisionName, commsApplyReportUnreadable, rerr.Error())
		return
	}
	if b.art.ID != boundID {
		s.degradeCommsApply(ctx, sink, artifactID, decisionName, commsApplyReportUnreadable,
			fmt.Sprintf("the recorded comms_report moved from %s to %s during resolution", boundID, b.art.ID))
		return
	}
	if sink.repo == "" {
		sink.repo = b.gather.Repo
	}

	// C3: re-ratify from the stage's approval rows.
	switch outcome, aerr := s.reportGateRatified(ctx, stage.ID); outcome {
	case reportGateRatifiedOK:
	case reportGateNoRepository:
		s.degradeCommsApply(ctx, sink, artifactID, decisionName, commsApplyNotRatified, "no approval repository is configured")
		return
	case reportGateUnreadable:
		s.degradeCommsApply(ctx, sink, artifactID, decisionName, commsApplyNotRatified, aerr.Error())
		return
	default:
		s.degradeCommsApply(ctx, sink, artifactID, decisionName, commsApplyNotRatified,
			"the comms gate is contested or ungranted; nothing filed")
		return
	}

	// Ratified. SETTLE THE WINDOW; from here on every degrade leaves it CLOSED.
	consumedRows, serr := s.settleReportWindow(ctx, commsWindowFamily, stage, artifactID, "approved")
	if serr != nil {
		s.degradeCommsApply(ctx, sink, artifactID, decisionName, commsApplyWindowUnsettled, serr.Error())
		return
	}
	consumed := collapseCommsConsumed(consumedRows, artifactID)

	job, reason, detail := s.resolveCommsFilingJob(ctx, stage, b, rn, rnErr, consumed, sink)
	if reason != "" {
		s.degradeCommsApply(ctx, sink, artifactID, decisionName, reason, detail)
		return
	}

	s.startDetachedReportApply(base, commsApplyBudgetFor(len(b.report.Drafts)), func(applyCtx context.Context) {
		s.runCommsFilingLoop(applyCtx, job)
	})
}

// applyRejectedComms is C1: settle the window `rejected` FIRST (before any
// body read), then record the `rejected` suppressions the settled consumed set
// implies. It files nothing and never moves the cursor.
func (s *Server) applyRejectedComms(ctx context.Context, sink *commsApplySink, stage *run.Stage, boundID uuid.UUID) {
	artifactID := boundID.String()
	consumedRows, serr := s.settleReportWindow(ctx, commsWindowFamily, stage, artifactID, "rejected")
	if serr != nil {
		// The consumed set is UNKNOWN, not empty: treating it as empty would
		// read as a whole-gate reject and suppress drafts the captain may
		// have approved (approval condition 1).
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "comms apply: reject-path window settlement failed",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", serr.Error()))
		s.degradeCommsApply(ctx, sink, artifactID, commsApplyDecisionReject, commsApplyWindowUnsettled, serr.Error())
		return
	}
	consumed := collapseCommsConsumed(consumedRows, artifactID)

	b, rerr := s.latestCommsReport(ctx, stage.RunID)
	if rerr == nil && b.art.ID != boundID {
		rerr = fmt.Errorf("the recorded comms_report moved from %s to %s during resolution", boundID, b.art.ID)
	}
	if rerr != nil {
		s.degradeCommsApply(ctx, sink, artifactID, commsApplyDecisionReject, commsApplyReportUnreadable, rerr.Error())
		return
	}
	if sink.repo == "" {
		sink.repo = b.gather.Repo
	}

	hashes := commsShownHashes(b.gather)
	sups := []commsApplySuppression{}
	wholeGate := len(consumed) == 0
	for _, d := range b.report.Drafts {
		if !wholeGate && consumed[d.ID].Verdict != commsVerdictRejected {
			continue
		}
		sups = commsAppendSuppressions(sups, d.SourceReportIDs, hashes, commsSuppressionBasisRejected, d.ID)
	}
	sink.record(ctx, CategoryCommsApplyCompleted, commsApplyCompletedPayload{
		Repo: sink.repo, ArtifactID: artifactID, Decision: commsApplyDecisionReject,
		Counts:       commsApplyCounts{Drafts: len(b.report.Drafts)},
		Suppressions: sups,
		Cursor:       commsApplyCursorOutcome{HeldReportIDs: []string{}, Reason: commsCursorReasonDecisionReject},
	})
}

// resolveCommsFilingJob resolves every input of the detached loop, returning
// a named degrade reason (and detail) on the first rung that cannot produce
// one. All of these are POST-ratification: the window is already closed.
func (s *Server) resolveCommsFilingJob(ctx context.Context, stage *run.Stage, b *commsReportBinding, rn *run.Run, rnErr error,
	consumed map[string]commsConsumedDisposition, sink *commsApplySink) (*commsFilingJob, string, string) {
	artifactID := b.art.ID.String()
	if rnErr != nil || rn == nil {
		detail := "the run row is absent"
		if rnErr != nil {
			detail = rnErr.Error()
		}
		return nil, commsApplyRunUnreadable, detail
	}
	var accountID *uuid.UUID
	if rn.AccountID != "" {
		id, perr := uuid.Parse(rn.AccountID)
		if perr != nil {
			return nil, commsApplyAccountUnparseable, perr.Error()
		}
		accountID = &id
	}
	env, currentRubric, degrade, currentCharter := s.commsPreviewSetup(ctx, rn, b.gather)
	switch degrade {
	case "":
	case commsPreviewDegradeRepoMalformed:
		return nil, commsApplyRepoUnresolvable, rn.Repo
	default:
		return nil, commsApplyConventionsUnavailable, degrade
	}
	priorFiled, perr := s.commsPriorFilings(ctx, stage.RunID, artifactID)
	if perr != nil {
		return nil, commsApplyPriorFilingsUnreadable, perr.Error()
	}

	// Rubric text follows the RECORDED charter_text state: the bytes the
	// captain reviewed quoted the rubric only when the ingest rendered it,
	// and the current text is used only while the charter still has the
	// gathered content hash (commsPreviewSetup's `rendered`).
	var rubric map[string]string
	charterUnavailable := false
	if b.recorded.CharterText == commsCharterTextRendered {
		switch currentCharter {
		case commsCharterTextRendered:
			rubric = currentRubric
		case commsCharterTextUnavailable:
			charterUnavailable = true
		}
	}

	return &commsFilingJob{
		runID: stage.RunID, stageID: stage.ID, artifactID: artifactID,
		accountID: accountID, repo: rn.Repo,
		report: b.report, gather: b.gather, unaccounted: *b.recorded.UnaccountedReportIDs,
		consumed: consumed, digests: commsRecordedDigests(b.recorded.Previews), priorFiled: priorFiled,
		rubricText: rubric, charterUnavailable: charterUnavailable, env: env, sink: sink,
	}, "", ""
}

// commsApplyLockKey keys the per-(account, repo) apply lock. The server is
// part of the key so two Servers in one process (tests) never share a lock.
type commsApplyLockKey struct {
	srv     *Server
	account string
	repo    string
}

// commsApplyLocks holds one 1-slot channel per key. Keys are never deleted:
// the set is bounded by the repositories an apply ran for.
var commsApplyLocks = struct {
	mu sync.Mutex
	m  map[commsApplyLockKey]chan struct{}
}{m: map[commsApplyLockKey]chan struct{}{}}

// acquireCommsApplyLock takes the per-(account, repo) lock, waiting at most
// until ctx (the apply budget) is done — unlike keyedLocks, whose wait cannot
// be cancelled (approval condition 3). It returns the release func, or ctx's
// error when the budget expired first (an already-expired ctx never acquires).
func acquireCommsApplyLock(ctx context.Context, key commsApplyLockKey) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	commsApplyLocks.mu.Lock()
	ch, ok := commsApplyLocks.m[key]
	if !ok {
		ch = make(chan struct{}, 1)
		commsApplyLocks.m[key] = ch
	}
	commsApplyLocks.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// commsApplyGuard is the double-filing guard read inside the lock: the
// suppression memory and the (id, hash) → filed issue map of every
// comms_draft_filed record for the repository.
type commsApplyGuard struct {
	memory     commsSuppressionMemory
	draftFiled map[commsSuppressionKey]int
	truncated  bool
}

// runCommsFilingLoop is the detached half: lock, guard, one row per draft in
// report order, the cursor, then ONE comms_apply_completed row.
func (s *Server) runCommsFilingLoop(applyCtx context.Context, job *commsFilingJob) {
	defer func() {
		if commsApplyAfterLoop != nil {
			commsApplyAfterLoop(job.runID)
		}
	}()
	account := ""
	if job.accountID != nil {
		account = job.accountID.String()
	}
	if commsApplyBeforeLock != nil {
		commsApplyBeforeLock(job.runID)
	}
	release, lerr := acquireCommsApplyLock(applyCtx, commsApplyLockKey{srv: s, account: account, repo: job.repo})
	if lerr != nil {
		s.commsApplyLockWaitExhausted(applyCtx, job)
		return
	}
	defer release()

	guard, gerr := s.readCommsApplyGuard(applyCtx, job)
	if gerr != nil {
		s.degradeCommsApply(context.WithoutCancel(applyCtx), job.sink, job.artifactID, commsApplyDecisionApprove,
			commsApplyGuardUnavailable, gerr.Error())
		return
	}

	hashes := commsShownHashes(job.gather)
	issueOf := map[string]int{}
	for _, r := range job.gather.Shown {
		issueOf[r.ID] = r.IssueNumber
	}
	sum := commsApplyCompletedPayload{
		Repo: job.repo, ArtifactID: job.artifactID, Decision: commsApplyDecisionApprove,
		Counts: commsApplyCounts{Drafts: len(job.report.Drafts)}, GuardTruncated: guard.truncated,
	}
	sups := []commsApplySuppression{}
	held := map[string]bool{}
	for _, id := range job.unaccounted {
		held[id] = true
	}
	filedNow := map[string]int{}
	for _, d := range job.report.Drafts {
		skip := commsDraftSkippedPayload{
			RunID: job.runID.String(), StageID: job.stageID.String(), ArtifactID: job.artifactID, DraftID: d.ID,
		}
		disp, decided := job.consumed[d.ID]
		prior, wasFiled := job.priorFiled[d.ID]
		if n, ok := filedNow[d.ID]; ok {
			prior, wasFiled = n, true
		}
		holdSources := false
		switch {
		case !decided:
			skip.SkipReason = commsSkipUndecided
			sum.Counts.Skipped++
			holdSources = true
		case disp.Verdict != commsVerdictApproved:
			skip.SkipReason = commsSkipRejected
			sum.Counts.Skipped++
			sups = commsAppendSuppressions(sups, d.SourceReportIDs, hashes, commsSuppressionBasisRejected, d.ID)
		case wasFiled:
			skip.SkipReason = commsSkipAlreadyFiled
			skip.PriorIssueNumber = prior
			sum.Counts.Skipped++
			sups = commsAppendSuppressions(sups, d.SourceReportIDs, hashes, commsSuppressionBasisFiled, d.ID)
		default:
			if conflicts := guard.conflicts(d.SourceReportIDs, hashes); len(conflicts) > 0 {
				skip.SkipReason = commsSkipFiledElsewhere
				skip.ConflictingReports = conflicts
				sum.Counts.Skipped++
				holdSources = true
				break
			}
			epicNum, hasEpic := commsEffectiveParentEpic(d, disp)
			if hasEpic && commsCitesIssue(d.SourceReportIDs, issueOf, epicNum) {
				skip.SkipReason = commsSkipParentEpicIsSource
				skip.Message = fmt.Sprintf("parent epic #%d is an issue a cited report lives on", epicNum)
				sum.Counts.Skipped++
				holdSources = true
				break
			}
			if applyCtx.Err() != nil {
				skip.SkipReason = commsSkipBudgetExhausted
				sum.Counts.BudgetExhausted++
				holdSources = true
				break
			}
			filed, code, msg := s.fileCommsDraft(applyCtx, job, d, epicNum, hasEpic, hashes, guard)
			if filed == nil {
				skip.SkipReason = commsSkipFilingFailed
				skip.Code, skip.Message = code, msg
				sum.Counts.Failed++
				holdSources = true
				break
			}
			filedNow[d.ID] = filed.IssueNumber
			sum.Counts.Filed++
			sups = commsAppendSuppressions(sups, d.SourceReportIDs, hashes, commsSuppressionBasisFiled, d.ID)
			job.sink.record(applyCtx, CategoryCommsDraftFiled, filed)
			continue
		}
		if holdSources {
			for _, id := range d.SourceReportIDs {
				held[id] = true
			}
		}
		job.sink.record(applyCtx, CategoryCommsDraftSkipped, skip)
	}
	for _, n := range job.report.NDrift {
		sups = commsAppendSuppressions(sups, n.SourceReportIDs, hashes, commsSuppressionBasisNDrift, n.ID)
	}
	sum.Suppressions = sups
	sum.Cursor = s.advanceCommsCursor(applyCtx, job, commsSortedKeys(held))
	job.sink.record(applyCtx, CategoryCommsApplyCompleted, sum)
	s.cfg.Logger.LogAttrs(applyCtx, slog.LevelInfo, "comms apply completed",
		slog.String("run_id", job.runID.String()),
		slog.String("stage_id", job.stageID.String()),
		slog.Int("filed", sum.Counts.Filed),
		slog.Int("skipped", sum.Counts.Skipped),
		slog.Int("failed", sum.Counts.Failed),
		slog.Int("budget_exhausted", sum.Counts.BudgetExhausted),
		slog.Bool("cursor_advanced", sum.Cursor.Advanced))
}

// commsApplyLockWaitExhausted records an apply whose budget expired while it
// was queued for the per-repository lock: one apply_budget_exhausted row per
// draft (none was processed) and ONE degraded completion row with the cursor
// held — every draft's sources and the unaccounted reports are held, and
// nothing advances.
func (s *Server) commsApplyLockWaitExhausted(applyCtx context.Context, job *commsFilingJob) {
	held := map[string]bool{}
	for _, id := range job.unaccounted {
		held[id] = true
	}
	for _, d := range job.report.Drafts {
		job.sink.record(applyCtx, CategoryCommsDraftSkipped, commsDraftSkippedPayload{
			RunID: job.runID.String(), StageID: job.stageID.String(), ArtifactID: job.artifactID, DraftID: d.ID,
			SkipReason: commsSkipBudgetExhausted, Message: commsApplyLockWaitExhaustedDetail,
		})
		for _, id := range d.SourceReportIDs {
			held[id] = true
		}
	}
	s.cfg.Logger.LogAttrs(applyCtx, slog.LevelWarn, "comms apply degraded; nothing was filed",
		slog.String("run_id", job.runID.String()),
		slog.String("stage_id", job.stageID.String()),
		slog.String("degrade_reason", commsApplyLockWaitExhausted))
	job.sink.record(applyCtx, CategoryCommsApplyCompleted, commsApplyCompletedPayload{
		Repo: job.repo, ArtifactID: job.artifactID, Decision: commsApplyDecisionApprove,
		Counts:   commsApplyCounts{Drafts: len(job.report.Drafts), BudgetExhausted: len(job.report.Drafts)},
		Degraded: true, DegradeReason: commsApplyLockWaitExhausted,
		Suppressions: []commsApplySuppression{},
		Cursor:       commsApplyCursorOutcome{HeldReportIDs: commsSortedKeys(held), Reason: commsCursorReasonApplyDegraded},
	})
}

// readCommsApplyGuard reads the double-filing guard for the run's account and
// repository. An unavailable read is an error (file nothing); a truncated one
// proceeds on the newest rows and is recorded as guard_truncated.
func (s *Server) readCommsApplyGuard(ctx context.Context, job *commsFilingJob) (commsApplyGuard, error) {
	g := commsApplyGuard{draftFiled: map[commsSuppressionKey]int{}}
	var unavailable *commsMemoryUnavailableError
	var truncated *commsMemoryTruncatedError
	mem, merr := s.loadCommsSuppressionMemory(ctx, job.accountID, job.repo)
	if errors.As(merr, &unavailable) {
		return g, merr
	}
	recs, ferr := s.loadCommsDraftFiled(ctx, job.accountID, job.repo)
	if errors.As(ferr, &unavailable) {
		return g, ferr
	}
	g.truncated = errors.As(merr, &truncated) || errors.As(ferr, &truncated)
	g.memory = mem
	for _, rec := range recs {
		for _, r := range rec.Reports {
			g.draftFiled[commsSuppressionKey{ID: r.ID, Hash: r.ContentHash}] = rec.IssueNumber
		}
	}
	return g, nil
}

// conflicts returns the cited reports, at their GATHERED hashes, that already
// carry a `filed` suppression or appear in a comms_draft_filed record. The
// check is PER REPORT: one filed report makes the whole draft filed_elsewhere.
//
// A draft-filed record citing (id, hash) is necessarily ANOTHER artifact's
// filing: CheckCommsReportSemantics gives each shown report exactly one
// placement in a report, so no other draft of this artifact cites it, and
// this draft's own prior filing is caught first as already_filed (approval
// condition 5). The read contract (commsDraftFiledRecord) therefore needs no
// artifact_id.
func (g commsApplyGuard) conflicts(ids []string, hashes map[string]string) []commsConflictingReport {
	var out []commsConflictingReport
	for _, id := range ids {
		hash := hashes[id]
		if hash == "" {
			continue
		}
		if sup, ok := g.memory.lookup(id, hash); ok && sup.Basis == commsSuppressionBasisFiled {
			out = append(out, commsConflictingReport{ID: id, ContentHash: hash, Basis: commsSuppressionBasisFiled})
			continue
		}
		if n, ok := g.draftFiled[commsSuppressionKey{ID: id, Hash: hash}]; ok {
			out = append(out, commsConflictingReport{ID: id, ContentHash: hash, Basis: commsConflictBasisDraftFiled, IssueNumber: n})
		}
	}
	return out
}

// priorSuppressions returns the cited reports holding a `rejected` or
// `n_drift` suppression from another apply. Such a draft is FILED and
// flagged: that apply necessarily preceded this approval, so the captain's
// approval in this window is the later decision.
func (g commsApplyGuard) priorSuppressions(ids []string, hashes map[string]string) []commsSuppression {
	out := []commsSuppression{}
	for _, id := range ids {
		if sup, ok := g.memory.lookup(id, hashes[id]); ok &&
			(sup.Basis == commsSuppressionBasisRejected || sup.Basis == commsSuppressionBasisNDrift) {
			out = append(out, sup)
		}
	}
	return out
}

// fileCommsDraft re-renders one approved draft, proves its body is the one the
// captain reviewed (the recorded filing_body_digest), and files it. On failure
// it returns nil with the filing_failed code and message.
func (s *Server) fileCommsDraft(ctx context.Context, job *commsFilingJob, d plan.CommsDraft, epicNum int, hasEpic bool,
	hashes map[string]string, guard commsApplyGuard) (*commsDraftFiledPayload, string, string) {
	req := commsFilingRequest(commsFilingInput{Draft: d, Gathered: job.gather, RubricText: job.rubricText})
	digest := commsFilingBodyDigest(req.Body)
	recorded := job.digests[d.ID]
	switch {
	case recorded == "":
		return nil, commsFilingDigestAbsent, "the recorded preview carries no filing_body_digest for this draft"
	case digest != recorded && job.charterUnavailable:
		return nil, commsFilingCharterUnavailable,
			"the reviewed body quoted the charter's rubric text and the charter cannot be read now; nothing filed"
	case digest != recorded:
		return nil, commsFilingBodyDrift,
			"the re-rendered filing body does not match the recorded filing_body_digest; nothing the captain did not review is filed"
	}

	// NO IdempotencyKey: workmgmt.StampIdempotencyKey appends a marker to the
	// body, which would make the filed body differ from the reviewed one.
	req.Labels = commsStripAutonomy(commsFilingLabels(req.Labels))
	req.Relations.ParentEpic = ""
	if hasEpic {
		req.Relations.ParentEpic = "#" + strconv.Itoa(epicNum)
	}
	conv, suppressedDefaults := commsFilingConventions(job.env.conv, req.Type)
	item, created, werr := s.applyAndFileWorkItem(ctx, req, conv, job.env.target, job.env.owner, job.env.name)
	if werr != nil {
		return nil, werr.code, werr.msg
	}
	applied := []string{}
	title := req.Summary
	if item != nil {
		applied = append(applied, item.Classification.Labels...)
		title = item.Title
	}
	reports := []userreport.MarkedReport{}
	for _, id := range d.SourceReportIDs {
		if h := hashes[id]; h != "" {
			reports = append(reports, userreport.MarkedReport{ID: id, ContentHash: h})
		}
	}
	out := &commsDraftFiledPayload{
		Repo: job.repo, Reports: reports,
		RunID: job.runID.String(), StageID: job.stageID.String(), ArtifactID: job.artifactID, DraftID: d.ID,
		Title: title, ParentEpic: req.Relations.ParentEpic,
		AppliedLabels: applied, StrippedLabels: commsStrippedLabels(d.ProposedIssue.Labels, req.Labels),
		SuppressedDefaultLabels: suppressedDefaults, FilingBodyDigest: digest,
		PriorSuppressions: guard.priorSuppressions(d.SourceReportIDs, hashes),
	}
	if created != nil {
		out.IssueNumber, out.IssueURL, out.Provider = created.Number, created.URL, created.Provider
	}
	return out, "", ""
}

// advanceCommsCursor advances the note floor THEN the cursor to the bound
// gather's pending values held back to the earliest held report. The scan
// read notes up to the note floor, so the floor never runs ahead of a cursor
// that failed to move.
func (s *Server) advanceCommsCursor(ctx context.Context, job *commsFilingJob, held []string) commsApplyCursorOutcome {
	out := commsApplyCursorOutcome{HeldReportIDs: held}
	cursor, noteCursor, ok := commsCursorHoldBack(*job.gather, held)
	if !ok {
		out.Reason = commsCursorReasonScanIncomplete
		return out
	}
	if s.cfg.UserReportCursors == nil {
		out.Reason = commsCursorReasonStoreUnwired
		return out
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commsApplyPrelaunchBudget)
	defer cancel()
	key := userreport.Key{AccountID: job.accountID, Repo: job.repo, Source: userreport.SourceIssueNotes}
	committedNote, _, err := s.cfg.UserReportCursors.Advance(wctx, key, noteCursor)
	if err != nil {
		out.Reason, out.Error = commsCursorReasonAdvanceFailed, err.Error()
		return out
	}
	key.Source = userreport.SourceIssues
	committed, _, err := s.cfg.UserReportCursors.Advance(wctx, key, cursor)
	if err != nil {
		out.Reason, out.Error = commsCursorReasonAdvanceFailed, err.Error()
		out.NoteCursor = &committedNote
		return out
	}
	out.Advanced, out.Cursor, out.NoteCursor = true, &committed, &committedNote
	return out
}

// degradeCommsApply records ONE degraded comms_apply_completed row naming why
// the apply did not run, alongside a Warn log. When ctx has expired the step's
// own failure is a symptom and the reason is comms_apply_prelaunch_timeout.
// The row carries no suppressions and leaves the cursor untouched.
func (s *Server) degradeCommsApply(ctx context.Context, sink *commsApplySink, artifactID, decision, reason, detail string) {
	if ctx.Err() != nil {
		detail = reason + ": " + detail
		reason = commsApplyPrelaunchTimeout
	}
	cursorReason := commsCursorReasonApplyDegraded
	if decision == commsApplyDecisionReject {
		cursorReason = commsCursorReasonDecisionReject
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "comms apply degraded; nothing was filed",
		slog.String("run_id", sink.runID.String()),
		slog.String("stage_id", sink.stageID.String()),
		slog.String("degrade_reason", reason),
		slog.String("detail", detail))
	sink.record(ctx, CategoryCommsApplyCompleted, commsApplyCompletedPayload{
		Repo: sink.repo, ArtifactID: artifactID, Decision: decision,
		Degraded: true, DegradeReason: reason,
		Suppressions: []commsApplySuppression{},
		Cursor:       commsApplyCursorOutcome{HeldReportIDs: []string{}, Reason: cursorReason},
	})
}

// collapseCommsConsumed collapses the consumed comms_disposition_recorded rows
// of artifactID LAST-WINS per draft id (collapseConsumed, report_seam.go); an
// undecodable row is skipped.
func collapseCommsConsumed(entries []*audit.Entry, artifactID string) map[string]commsConsumedDisposition {
	return collapseConsumed(entries, artifactID, func(payload []byte) (string, string, commsConsumedDisposition, bool) {
		var p commsDispositionPayload
		if json.Unmarshal(payload, &p) != nil {
			return "", "", commsConsumedDisposition{}, false
		}
		return p.DraftID, p.ArtifactID, commsConsumedDisposition{Verdict: p.Verdict, ParentEpic: p.ParentEpic}, true
	})
}

// commsPriorFilings maps draft id → issue number for the run's
// comms_draft_filed rows of artifactID. A row it cannot attribute is
// UNREADABLE (fail closed: it might be the filing a re-apply would duplicate).
func (s *Server) commsPriorFilings(ctx context.Context, runID uuid.UUID, artifactID string) (map[string]int, error) {
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryCommsDraftFiled)
	if err != nil {
		return nil, fmt.Errorf("list %s rows: %w", CategoryCommsDraftFiled, err)
	}
	out := map[string]int{}
	for _, e := range rows {
		if e == nil {
			continue
		}
		var p commsDraftFiledPayload
		if jerr := json.Unmarshal(e.Payload, &p); jerr != nil || p.DraftID == "" {
			return nil, fmt.Errorf("%s row %d is not attributable", CategoryCommsDraftFiled, e.Sequence)
		}
		if p.ArtifactID == artifactID {
			out[p.DraftID] = p.IssueNumber
		}
	}
	return out, nil
}

// commsRecordedDigests decodes the recorded previews LENIENTLY into draft id
// → filing_body_digest: an element that does not decode, or names no draft,
// is skipped (that draft then files nothing: filing_body_digest_absent).
func commsRecordedDigests(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	var elems []json.RawMessage
	if json.Unmarshal(raw, &elems) != nil {
		return out
	}
	for _, el := range elems {
		var p struct {
			DraftID          string `json:"draft_id"`
			FilingBodyDigest string `json:"filing_body_digest"`
		}
		if json.Unmarshal(el, &p) != nil || p.DraftID == "" {
			continue
		}
		out[p.DraftID] = p.FilingBodyDigest
	}
	return out
}

// commsFilingConventions returns conv with item type typ's DEFAULTED autonomy
// tier removed — the `autonomy` label_defaults key and every autonomy:*
// default_labels entry — and the labels it removed (sorted, non-nil). A comms
// scan reads user reports, so neither the proposal NOR the conventions'
// default may set an issue's delegation tier at filing (the preview's
// defaulted autonomy label is therefore NOT filed). The shared loaded
// conventions are never mutated: the Types map and the changed type's
// LabelDefaults / DefaultLabels are fresh copies.
func commsFilingConventions(conv workmgmt.Conventions, typ string) (workmgmt.Conventions, []string) {
	suppressed := []string{}
	it, ok := conv.Types[typ]
	if !ok {
		return conv, suppressed
	}
	types := make(map[string]workmgmt.ItemType, len(conv.Types))
	for k, v := range conv.Types {
		types[k] = v
	}
	defaults := make(map[string]string, len(it.LabelDefaults))
	for ns, l := range it.LabelDefaults {
		if strings.EqualFold(strings.TrimSpace(ns), "autonomy") {
			suppressed = append(suppressed, l)
			continue
		}
		defaults[ns] = l
	}
	var labels []string
	for _, l := range it.DefaultLabels {
		if commsIsAutonomyLabel(l) {
			suppressed = append(suppressed, l)
			continue
		}
		labels = append(labels, l)
	}
	it.LabelDefaults, it.DefaultLabels = defaults, labels
	types[typ] = it
	conv.Types = types
	sort.Strings(suppressed)
	return conv, suppressed
}

// commsIsAutonomyLabel reports whether l is in the autonomy:* namespace.
func commsIsAutonomyLabel(l string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), "autonomy:")
}

// commsStripAutonomy drops every autonomy:* label AGAIN (commsFilingLabels
// already drops them; this is the apply's own guard). Always non-nil.
func commsStripAutonomy(labels []string) []string {
	out := []string{}
	for _, l := range labels {
		if !commsIsAutonomyLabel(l) {
			out = append(out, l)
		}
	}
	return out
}

// commsStrippedLabels returns the proposed labels absent from the filed
// request's labels (compared trimmed and case-insensitively). Always non-nil.
func commsStrippedLabels(proposed, kept []string) []string {
	keep := map[string]bool{}
	for _, l := range kept {
		keep[strings.ToLower(strings.TrimSpace(l))] = true
	}
	out := []string{}
	for _, l := range proposed {
		if !keep[strings.ToLower(strings.TrimSpace(l))] {
			out = append(out, l)
		}
	}
	return out
}

// commsEffectiveParentEpic is the parent epic a draft files under: the
// captain's override when it parses (plan.CommsParentEpicNumber), else the
// proposal's.
func commsEffectiveParentEpic(d plan.CommsDraft, disp commsConsumedDisposition) (int, bool) {
	if disp.ParentEpic != "" {
		if n, ok := plan.CommsParentEpicNumber(disp.ParentEpic); ok {
			return n, true
		}
	}
	if d.ProposedIssue.ParentEpic != nil {
		return plan.CommsParentEpicNumber(*d.ProposedIssue.ParentEpic)
	}
	return 0, false
}

// commsCitesIssue reports whether any cited report lives on issue n.
func commsCitesIssue(ids []string, issueOf map[string]int, n int) bool {
	for _, id := range ids {
		if v, ok := issueOf[id]; ok && v == n {
			return true
		}
	}
	return false
}

// commsShownHashes maps each shown report id to its GATHERED content hash.
func commsShownHashes(g *commsScanGatheredPayload) map[string]string {
	out := map[string]string{}
	if g == nil {
		return out
	}
	for _, r := range g.Shown {
		out[r.ID] = r.ContentHash
	}
	return out
}

// commsAppendSuppressions appends one suppression per cited id that the gather
// shows with a hash (an id it does not show could only ever match an
// unidentifiable report).
func commsAppendSuppressions(sups []commsApplySuppression, ids []string, hashes map[string]string, basis, entryID string) []commsApplySuppression {
	for _, id := range ids {
		if h := hashes[id]; h != "" {
			sups = append(sups, commsApplySuppression{ID: id, Hash: h, Basis: basis, EntryID: entryID})
		}
	}
	return sups
}

// commsSortedKeys returns the set's members sorted; always non-nil.
func commsSortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
