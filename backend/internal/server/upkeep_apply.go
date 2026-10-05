package server

// On-approval upkeep apply (#3924, E79 / #3726).
//
// The upkeep scan's last phase: when the captain DECIDES the plan stage that
// carries the run's recorded upkeep_report, this hook settles the report's
// disposition-capture window and, on approve only, files every captain-approved
// finding that is not already covered — through the SAME work-item filing core
// every other auto-file path funnels through (applyAndFileWorkItem). It NEVER
// creates a run: an upkeep finding becomes a tracker issue a human (or a later
// campaign) picks up, not a dispatched change. Nothing in this file references
// a run-creation path.
//
// It mirrors applyApprovedGrooming (grooming_apply.go) on purpose — same
// placement in the TYPE-only plan block of approvals.go, same passed decision,
// same detach-at-entry, same prelaunch budget, same Shutdown-drained detached
// half — so the two on-approval hooks agree on what "ratified" and "settled"
// mean by construction.
//
// THE LADDER, in evaluation order:
//
//	E0 REPORT      (an early-out, not a control) no upkeep_report artifact on
//	               the decided stage, no upkeep_report_recorded row on the run,
//	               or the recorded row names an artifact that is NOT one of this
//	               stage's upkeep_report artifacts → return having written
//	               nothing. An ordinary plan approval or reject settles nothing.
//	               It reads ONLY the artifact LIST and the recorded ROW — never a
//	               report body — so a nil or minimal repo never degrades an
//	               ordinary approval.
//	C1 DECISION    a decision other than approve settles the window `rejected`
//	               from the recorded row's artifact id and files NOTHING. It runs
//	               BEFORE any report-body read, so a reject closes the window even
//	               when the report body is unreadable.
//	C3 RATIFY      re-read the stage's approval rows: >= 1 grant and 0
//	               rejections (approvedGroomingReport's predicate). A contested
//	               or ungranted gate files nothing and does NOT close the window.
//
// Then the window settles `approved` (the consumed dispositions are read in the
// same transaction on the production repository), and every later degrade is
// POST-RATIFICATION and leaves the window CLOSED.
//
// THE PER-FINDING LOOP runs detached, in REPORT ORDER, and records exactly one
// row per finding. The skip rules are checked in this order:
//
//	not_approved            no consumed disposition, or a `rejected` one
//	duplicate_of_open_issue the ingest marked the finding as covered by an
//	                        OPEN issue (marker or similarity) — the row carries
//	                        the issue number, url and basis
//	covered_by_dependabot_pr the ingest marked the advisory finding as fixed by
//	                        open Dependabot pull requests (#3750) — the row
//	                        carries their numbers and urls
//	already_filed           an upkeep_finding_filed row for this artifact and
//	                        finding already exists (a re-apply)
//	apply_budget_exhausted  the detached budget expired before this finding
//	filing_failed           the filing core returned a *workItemError
//
// A filed finding carries the proposed body PLUS upkeep.FindingMarker(id) — the
// marker the next scan's ingest dedupe matches — and a stable idempotency key
// minted from (run, artifact, finding). An ADVISORY finding (#3750) is the
// exception: its title and body are SERVER-RENDERED from the structured
// advisory fields (upkeep.RenderAdvisoryTitle / RenderAdvisoryFacts) and the
// agent-authored proposed title and body are ignored, so no agent prose — and
// no call-path frame naming the repository's own code — reaches the tracker
// through them. Its labels are narrowed to the area:, type: and phase:
// namespaces (upkeepAdvisoryLabels); the suffix inside a kept namespace is
// still agent-chosen until the filing-label allow-list (#3956). The
// proposal's autonomy:* labels are
// STRIPPED unless the captain set authorize_delegation_tier (an advisory
// filing strips them regardless, by the namespace narrowing); note that the
// conventions' label_defaults may still add their DEFAULT autonomy tier (the
// conventions' choice, not the scan's), so "stripped" means "the PROPOSED tier
// is never applied unaided". The parent epic is the disposition's override,
// else the proposal's, normalized to `#N`.
//
// Every outcome is auditable: upkeep_finding_filed / upkeep_finding_skipped per
// finding, and ONE upkeep_apply_completed summary row — degraded:true with a
// named degrade_reason when the apply did not run. Long-form contract:
// backend/internal/server/README.md § "On-approval upkeep apply".

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgithub "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/github"
)

// The upkeep apply's audit categories. upkeep_finding_filed /
// upkeep_finding_skipped were registered by #3921; upkeep_apply_completed is
// registered by #3924 (backend/internal/audit/categories.go).
const (
	CategoryUpkeepFindingFiled   = "upkeep_finding_filed"
	CategoryUpkeepFindingSkipped = "upkeep_finding_skipped"
	CategoryUpkeepApplyCompleted = "upkeep_apply_completed"
)

// upkeepIdempotencyNamespace namespaces the minted per-finding filing key.
const upkeepIdempotencyNamespace = "upkeep_finding"

// The closed set of per-finding skip reasons (payload VALUES, not categories).
const (
	upkeepSkipNotApproved     = "not_approved"
	upkeepSkipDuplicate       = "duplicate_of_open_issue"
	upkeepSkipCovered         = "covered_by_dependabot_pr"
	upkeepSkipAlreadyFiled    = "already_filed"
	upkeepSkipBudgetExhausted = "apply_budget_exhausted"
	upkeepSkipFilingFailed    = "filing_failed"
)

// The closed set of degrade reasons, each naming ONE rung that could not
// produce an input. Carried on the upkeep_apply_completed row.
const (
	// upkeepApplyReportUnreadable: the recorded row or the report it names
	// could not be read or parsed. PRE-ratification: the window is NOT closed.
	upkeepApplyReportUnreadable = "upkeep_apply_report_unreadable"
	// upkeepApplyNotRatified: the gate is contested, ungranted, or its rows are
	// unreadable. PRE-ratification: the window is NOT closed.
	upkeepApplyNotRatified = "upkeep_apply_not_ratified"
	// upkeepApplyWindowUnsettled: the `approved` watermark did not land, so
	// nothing was consumed and the window is still open.
	upkeepApplyWindowUnsettled = "upkeep_apply_window_unsettled"
	// The remaining reasons are POST-ratification: the window stays CLOSED.
	upkeepApplyDuplicatesUnreadable = "upkeep_apply_duplicates_unreadable"
	// upkeepApplyCoverageUnreadable: the recorded row's `covered` key is
	// present but null or not an array (#3750). An ABSENT key is a pre-#3750
	// row and reads as no marks.
	upkeepApplyCoverageUnreadable     = "upkeep_apply_coverage_unreadable"
	upkeepApplyPriorFilingsUnreadable = "upkeep_apply_prior_filings_unreadable"
	upkeepApplyRunUnreadable          = "upkeep_apply_run_unreadable"
	upkeepApplyRepoUnresolvable       = "upkeep_apply_repo_unresolvable"
	upkeepApplyConventionsUnavailable = "upkeep_apply_conventions_unavailable"
	// upkeepApplyPrelaunchTimeout: the synchronous half did not finish inside
	// upkeepApplyPrelaunchBudget, whichever rung reported the failure.
	upkeepApplyPrelaunchTimeout = "upkeep_apply_prelaunch_timeout"
)

// upkeepApplyPrelaunchBudget bounds the SYNCHRONOUS half of the hook — the part
// still on the captain's approve request. A var ONLY so a test can shrink it.
var upkeepApplyPrelaunchBudget = 30 * time.Second

// upkeepApplyBudget is the FLOOR of the detached loop's budget and
// upkeepApplyPerFindingBudget its per-finding scale (the grooming apply's
// observed ~1 write/s with a 3x margin). Vars ONLY so a test can zero them.
var (
	upkeepApplyBudget           = 3 * time.Minute
	upkeepApplyPerFindingBudget = 3 * time.Second
)

// upkeepApplyBudgetFor is the PURE budget for a loop over `findings` entries:
// the floor, or the per-finding scale, whichever is larger.
func upkeepApplyBudgetFor(findings int) time.Duration {
	scaled := time.Duration(findings) * upkeepApplyPerFindingBudget
	if scaled > upkeepApplyBudget {
		return scaled
	}
	return upkeepApplyBudget
}

// upkeepFindingFiledPayload is the upkeep_finding_filed row. AppliedLabels are
// the labels ACTUALLY filed (the post-Apply item), not the request.
type upkeepFindingFiledPayload struct {
	RunID          string   `json:"run_id"`
	StageID        string   `json:"stage_id"`
	ArtifactID     string   `json:"artifact_id"`
	FindingID      string   `json:"finding_id"`
	Source         string   `json:"source"`
	IssueNumber    int      `json:"issue_number"`
	IssueURL       string   `json:"issue_url,omitempty"`
	Provider       string   `json:"provider"`
	Title          string   `json:"title"`
	ParentEpic     string   `json:"parent_epic,omitempty"`
	AppliedLabels  []string `json:"applied_labels"`
	StrippedLabels []string `json:"stripped_labels"`
	IdempotencyKey string   `json:"idempotency_key"`
	// ServerRendered is true when the filed title and body were rendered
	// from the advisory fields and the agent's proposed prose was ignored
	// (every advisory finding, #3750).
	ServerRendered bool `json:"server_rendered,omitempty"`
}

// upkeepFindingSkippedPayload is the upkeep_finding_skipped row. The optional
// fields are populated per skip reason.
type upkeepFindingSkippedPayload struct {
	RunID                string `json:"run_id"`
	StageID              string `json:"stage_id"`
	ArtifactID           string `json:"artifact_id"`
	FindingID            string `json:"finding_id"`
	Source               string `json:"source"`
	SkipReason           string `json:"skip_reason"`
	DuplicateIssueNumber int    `json:"duplicate_issue_number,omitempty"`
	DuplicateIssueURL    string `json:"duplicate_issue_url,omitempty"`
	DuplicateBasis       string `json:"duplicate_basis,omitempty"`
	PriorIssueNumber     int    `json:"prior_issue_number,omitempty"`
	// CoveringPRNumbers / CoveringPRURLs name the Dependabot pull requests a
	// covered_by_dependabot_pr skip rests on, one per cited manifest
	// directory (#3750).
	CoveringPRNumbers []int    `json:"covering_pr_numbers,omitempty"`
	CoveringPRURLs    []string `json:"covering_pr_urls,omitempty"`
	Code              string   `json:"code,omitempty"`
	Message           string   `json:"message,omitempty"`
}

// upkeepApplyCompletedPayload is the ONE upkeep_apply_completed row per apply.
// filed + skipped + failed + budget_exhausted == findings on a non-degraded
// row: skipped counts not_approved / duplicate_of_open_issue /
// covered_by_dependabot_pr / already_filed, failed counts filing_failed. A degraded row carries zero counts.
type upkeepApplyCompletedPayload struct {
	ArtifactID      string `json:"artifact_id,omitempty"`
	Findings        int    `json:"findings"`
	Filed           int    `json:"filed"`
	Skipped         int    `json:"skipped"`
	Failed          int    `json:"failed"`
	BudgetExhausted int    `json:"budget_exhausted"`
	Degraded        bool   `json:"degraded"`
	DegradeReason   string `json:"degrade_reason,omitempty"`
}

// upkeepConsumedDisposition is one collapsed captain verdict consumed from the
// closed window.
type upkeepConsumedDisposition struct {
	Verdict                 string
	AuthorizeDelegationTier bool
	ParentEpic              string
}

// upkeepApplySink writes the apply's rows onto the run's chain: actor system,
// the decided stage's id, payloads marshalled BARE. Every write runs on its OWN
// bounded context detached from the caller's, so a row recording a budget
// expiry is not lost to that same expiry.
type upkeepApplySink struct {
	s       *Server
	runID   uuid.UUID
	stageID uuid.UUID
}

func (k *upkeepApplySink) append(ctx context.Context, category string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upkeepApplyPrelaunchBudget)
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

// upkeepFilingJob is everything the detached loop needs, resolved on the
// synchronous half so the goroutine never touches the request.
type upkeepFilingJob struct {
	runID, stageID uuid.UUID
	artifactID     string
	report         *plan.UpkeepReport
	consumed       map[string]upkeepConsumedDisposition
	duplicates     map[string]upkeep.Duplicate
	covered        map[string]upkeep.Covered
	priorFiled     map[string]int
	conv           workmgmt.Conventions
	target         workmgmt.Target
	owner, name    string
	sink           *upkeepApplySink
}

// applyApprovedUpkeep is the on-approval hook. See the file header for the
// ladder and the best-effort contract.
func (s *Server) applyApprovedUpkeep(ctx context.Context, stage *run.Stage, decision approval.Decision) {
	// E0 nil safety: an ordinary deployment or test harness without the
	// repositories this hook reads is an ordinary plan approval.
	if stage == nil || s.cfg.ArtifactRepo == nil || s.cfg.AuditRepo == nil {
		return
	}

	// DETACH AT ENTRY, then BOUND the synchronous half (grooming's #3232
	// shape): a client disconnect strands neither the settlement nor the
	// filing, and a stalled store cannot hold the approve request open.
	base := context.WithoutCancel(ctx)
	ctx, prelaunchCancel := context.WithTimeout(base, upkeepApplyPrelaunchBudget)
	defer prelaunchCancel()

	// E0 (early-out): does this stage carry an upkeep_report at all? An
	// unreadable list is silent — indistinguishable from "no report" for a
	// stage that never had one, and this hook must not degrade an ordinary
	// plan approval.
	stageArts, lerr := s.cfg.ArtifactRepo.ListForStage(ctx, stage.ID)
	if lerr != nil {
		return
	}
	onStage := map[uuid.UUID]bool{}
	for _, a := range stageArts {
		if a != nil && a.Kind == artifact.KindUpkeepReport {
			onStage[a.ID] = true
		}
	}
	if len(onStage) == 0 {
		return
	}

	sink := &upkeepApplySink{s: s, runID: stage.RunID, stageID: stage.ID}

	// The recorded binding: the artifact named by the HIGHEST-sequence
	// upkeep_report_recorded row (latestUpkeepReport's rule), read from the
	// ROW only so C1 below never depends on the report body.
	boundID, found, berr := s.upkeepRecordedArtifact(ctx, stage.RunID)
	if berr != nil {
		s.degradeUpkeepApplyPrelaunch(ctx, sink, "", upkeepApplyReportUnreadable, berr.Error())
		return
	}
	if !found || !onStage[boundID] {
		// No recorded row (an orphan artifact whose row never landed), or the
		// run's current report belongs to another stage: nothing to settle here.
		return
	}
	artifactID := boundID.String()

	// C1: a decision other than approve files NOTHING and settles the window
	// `rejected` — before any report-body read, so an unreadable body cannot
	// keep a rejected window open.
	if decision != approval.DecisionApprove {
		if _, err := s.settleUpkeepWindow(ctx, stage, artifactID, "rejected"); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "upkeep apply: reject-path window settlement failed",
				slog.String("run_id", stage.RunID.String()),
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", err.Error()))
		}
		return
	}

	// Resolve the report through latestUpkeepReport — the function the capture
	// binds through — and require it to name the same artifact the row did.
	b, rerr := s.latestUpkeepReport(ctx, stage.RunID)
	if rerr != nil {
		s.degradeUpkeepApplyPrelaunch(ctx, sink, artifactID, upkeepApplyReportUnreadable, rerr.Error())
		return
	}
	if b.art.ID != boundID {
		s.degradeUpkeepApplyPrelaunch(ctx, sink, artifactID, upkeepApplyReportUnreadable,
			fmt.Sprintf("the recorded upkeep_report moved from %s to %s during resolution", boundID, b.art.ID))
		return
	}

	// C3: re-ratify from the stage's approval rows. The submission is not the
	// gate: a gate contested by a rejection files nothing even when the latest
	// submission is a grant.
	if !s.upkeepGateRatified(ctx, sink, stage, artifactID) {
		return
	}

	// Ratified. SETTLE THE WINDOW before any provider resolution; from here on
	// every degrade leaves it CLOSED.
	consumed, serr := s.settleUpkeepWindow(ctx, stage, artifactID, "approved")
	if serr != nil {
		s.degradeUpkeepApplyPrelaunch(ctx, sink, artifactID, upkeepApplyWindowUnsettled, serr.Error())
		return
	}

	job, reason, detail := s.resolveUpkeepFilingJob(ctx, stage, b, consumed, sink)
	if reason != "" {
		s.degradeUpkeepApplyPrelaunch(ctx, sink, artifactID, reason, detail)
		return
	}

	// DETACHED and BOUNDED, on a goroutine Shutdown drains. The loop's context
	// derives from `base` — neither the request's cancellation nor the
	// prelaunch deadline.
	budget := upkeepApplyBudgetFor(len(b.report.Findings))
	s.bgUpkeepApply.Add(1)
	go func() {
		defer s.bgUpkeepApply.Done()
		applyCtx, cancel := context.WithTimeout(base, budget)
		defer cancel()
		s.runUpkeepFilingLoop(applyCtx, job)
	}()
}

// upkeepGateRatified is C3. It degrades (window NOT closed) and returns false
// when the gate's rows are unreadable, contested or ungranted.
func (s *Server) upkeepGateRatified(ctx context.Context, sink *upkeepApplySink, stage *run.Stage, artifactID string) bool {
	if s.cfg.ApprovalRepo == nil {
		s.degradeUpkeepApplyPrelaunch(ctx, sink, artifactID, upkeepApplyNotRatified, "no approval repository is configured")
		return false
	}
	approvals, aerr := s.cfg.ApprovalRepo.ListForStage(ctx, stage.ID)
	if aerr != nil {
		s.degradeUpkeepApplyPrelaunch(ctx, sink, artifactID, upkeepApplyNotRatified, aerr.Error())
		return false
	}
	grants, rejections := 0, 0
	for _, ap := range approvals {
		if ap == nil {
			continue
		}
		switch ap.Decision {
		case approval.DecisionApprove:
			grants++
		case approval.DecisionReject:
			rejections++
		}
	}
	if grants == 0 || rejections > 0 {
		s.degradeUpkeepApplyPrelaunch(ctx, sink, artifactID, upkeepApplyNotRatified,
			"the upkeep gate is contested or ungranted; nothing filed")
		return false
	}
	return true
}

// resolveUpkeepFilingJob resolves every input of the detached loop. It returns
// a named degrade reason (and detail) on the first rung that cannot produce
// one. All of these are POST-ratification: the window is already closed.
func (s *Server) resolveUpkeepFilingJob(ctx context.Context, stage *run.Stage, b *upkeepReportBinding,
	consumed map[string]upkeepConsumedDisposition, sink *upkeepApplySink) (*upkeepFilingJob, string, string) {
	artifactID := b.art.ID.String()

	recorded, rerr := s.upkeepRecordedRow(ctx, stage.RunID, artifactID)
	if rerr != nil {
		return nil, upkeepApplyDuplicatesUnreadable, rerr.Error()
	}
	duplicates, derr := upkeepRecordedDuplicates(recorded)
	if derr != nil {
		return nil, upkeepApplyDuplicatesUnreadable, derr.Error()
	}
	// The SAME row the duplicates came from.
	covered, cerr := upkeepRecordedCoverage(recorded)
	if cerr != nil {
		return nil, upkeepApplyCoverageUnreadable, cerr.Error()
	}
	priorFiled, perr := s.upkeepPriorFilings(ctx, stage.RunID, artifactID)
	if perr != nil {
		return nil, upkeepApplyPriorFilingsUnreadable, perr.Error()
	}
	if s.cfg.RunRepo == nil {
		return nil, upkeepApplyRunUnreadable, "no run repository is configured"
	}
	rn, gerr := s.cfg.RunRepo.GetRun(ctx, stage.RunID)
	if gerr != nil {
		return nil, upkeepApplyRunUnreadable, gerr.Error()
	}
	owner, name, ok := splitRepoFullName(rn.Repo)
	if !ok {
		return nil, upkeepApplyRepoUnresolvable, rn.Repo
	}
	conv, cerr := conventionsLoader(ctx, rn.Repo)
	if cerr != nil {
		return nil, upkeepApplyConventionsUnavailable, cerr.Error()
	}

	// The run-scoped target, built exactly as upkeepDuplicates builds it:
	// coordinates from the run, provider connections from the conventions, the
	// credential scope from the run's installation (else resolved for the
	// GitHub provider). A failed scope lookup is NOT fatal: the provider fails
	// closed per finding and the finding is recorded filing_failed.
	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
		Jira:    conv.Jira,
		GitLab:  conv.GitLab,
	}
	if rn.InstallationID != nil {
		target.Scope = forge.FromGitHubInstallationID(*rn.InstallationID)
	}
	if target.Scope.IsZero() && s.cfg.GitHub != nil && conv.Provider == workmgmtgithub.ProviderName {
		if scope, serr := s.resolveRepoScope(ctx, owner, name); serr == nil {
			target.Scope = scope
		}
	}

	return &upkeepFilingJob{
		runID: stage.RunID, stageID: stage.ID, artifactID: artifactID,
		report: b.report, consumed: consumed, duplicates: duplicates, covered: covered, priorFiled: priorFiled,
		conv: conv, target: target, owner: owner, name: name, sink: sink,
	}, "", ""
}

// runUpkeepFilingLoop is the detached half: one row per finding in report
// order, then ONE upkeep_apply_completed row.
func (s *Server) runUpkeepFilingLoop(applyCtx context.Context, job *upkeepFilingJob) {
	sum := upkeepApplyCompletedPayload{ArtifactID: job.artifactID, Findings: len(job.report.Findings)}
	filedNow := map[string]int{}
	for _, f := range job.report.Findings {
		skip := upkeepFindingSkippedPayload{
			RunID: job.runID.String(), StageID: job.stageID.String(),
			ArtifactID: job.artifactID, FindingID: f.ID, Source: f.Source,
		}
		disp, approved := job.consumed[f.ID]
		dup, isDup := job.duplicates[f.ID]
		cov, isCovered := job.covered[f.ID]
		prior, wasFiled := job.priorFiled[f.ID]
		if n, ok := filedNow[f.ID]; ok {
			prior, wasFiled = n, true
		}
		switch {
		case !approved || disp.Verdict != upkeepVerdictApproved:
			skip.SkipReason = upkeepSkipNotApproved
			sum.Skipped++
		case isDup:
			skip.SkipReason = upkeepSkipDuplicate
			skip.DuplicateIssueNumber = dup.IssueNumber
			skip.DuplicateIssueURL = dup.IssueURL
			skip.DuplicateBasis = string(dup.Basis)
			sum.Skipped++
		case isCovered:
			skip.SkipReason = upkeepSkipCovered
			for _, p := range cov.Pulls {
				skip.CoveringPRNumbers = append(skip.CoveringPRNumbers, p.Number)
				if p.URL != "" {
					skip.CoveringPRURLs = append(skip.CoveringPRURLs, p.URL)
				}
			}
			sum.Skipped++
		case wasFiled:
			skip.SkipReason = upkeepSkipAlreadyFiled
			skip.PriorIssueNumber = prior
			sum.Skipped++
		case applyCtx.Err() != nil:
			skip.SkipReason = upkeepSkipBudgetExhausted
			sum.BudgetExhausted++
		default:
			filed, werr := s.fileUpkeepFinding(applyCtx, job, f, disp)
			if werr != nil {
				skip.SkipReason = upkeepSkipFilingFailed
				skip.Code = werr.code
				skip.Message = werr.msg
				sum.Failed++
				break
			}
			filedNow[f.ID] = filed.IssueNumber
			sum.Filed++
			s.recordUpkeepApplyRow(applyCtx, job, CategoryUpkeepFindingFiled, filed)
			continue
		}
		s.recordUpkeepApplyRow(applyCtx, job, CategoryUpkeepFindingSkipped, skip)
	}
	s.recordUpkeepApplyRow(applyCtx, job, CategoryUpkeepApplyCompleted, sum)
	s.cfg.Logger.LogAttrs(applyCtx, slog.LevelInfo, "upkeep apply completed",
		slog.String("run_id", job.runID.String()),
		slog.String("stage_id", job.stageID.String()),
		slog.Int("filed", sum.Filed),
		slog.Int("skipped", sum.Skipped),
		slog.Int("failed", sum.Failed),
		slog.Int("budget_exhausted", sum.BudgetExhausted))
}

// recordUpkeepApplyRow appends one row, logging (never aborting) on failure.
// A lost upkeep_finding_filed row is the at-least-once residual: a re-apply
// could file the finding again, identifiable by its key and marker.
func (s *Server) recordUpkeepApplyRow(ctx context.Context, job *upkeepFilingJob, category string, payload any) {
	if err := job.sink.append(ctx, category, payload); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "upkeep apply: audit row not recorded",
			slog.String("run_id", job.runID.String()),
			slog.String("stage_id", job.stageID.String()),
			slog.String("category", category),
			slog.String("error", err.Error()))
	}
}

// fileUpkeepFinding files ONE approved finding through the shared work-item
// core and returns its upkeep_finding_filed payload.
func (s *Server) fileUpkeepFinding(ctx context.Context, job *upkeepFilingJob, f plan.UpkeepFinding,
	disp upkeepConsumedDisposition) (*upkeepFindingFiledPayload, *workItemError) {
	labels, stripped := upkeepFilingLabels(f.ProposedIssue.Labels, disp.AuthorizeDelegationTier)
	if f.Source == plan.UpkeepSourceAdvisory {
		labels, stripped = upkeepAdvisoryLabels(labels, stripped)
	}
	parentEpic := disp.ParentEpic
	if parentEpic == "" && f.ProposedIssue.ParentEpic != nil {
		parentEpic = *f.ProposedIssue.ParentEpic
	}
	parentEpic = normalizeUpkeepEpicRef(parentEpic)
	key := workmgmt.MintIdempotencyKey(upkeepIdempotencyNamespace, job.runID.String(), job.artifactID, f.ID)

	summary, body, serverRendered := upkeepFilingProse(f)
	filing := workmgmt.FilingRequest{
		Type:           f.ProposedIssue.Type,
		Summary:        summary,
		Body:           upkeepFilingBody(body, f.ID),
		Labels:         labels,
		Relations:      workmgmt.Relations{ParentEpic: parentEpic},
		IdempotencyKey: key,
	}
	item, created, werr := s.applyAndFileWorkItem(ctx, filing, job.conv, job.target, job.owner, job.name)
	if werr != nil {
		return nil, werr
	}
	applied := []string{}
	title := summary
	if item != nil {
		applied = append(applied, item.Classification.Labels...)
		title = item.Title
	}
	out := &upkeepFindingFiledPayload{
		RunID: job.runID.String(), StageID: job.stageID.String(), ArtifactID: job.artifactID,
		FindingID: f.ID, Source: f.Source, Title: title, ParentEpic: parentEpic,
		AppliedLabels: applied, StrippedLabels: stripped, IdempotencyKey: key,
		ServerRendered: serverRendered,
	}
	if created != nil {
		out.IssueNumber, out.IssueURL, out.Provider = created.Number, created.URL, created.Provider
	}
	return out, nil
}

// upkeepFilingProse returns the title and body to file for f, and whether
// they were server-rendered. An advisory finding (#3750) files ONLY what the
// server renders from its structured advisory fields — the primary id,
// package, in-use version and severity in the title; every advisory id, the
// ecosystem, package, versions (or "no fix published"), reachability,
// severity and the cited manifest paths in the body — and the agent's
// proposed title and body are ignored, so agent prose (which could quote the
// call path through the repository's own code) never reaches the tracker.
// It keys on the SOURCE alone: an advisory finding missing its advisory
// object (refused by rule (l), so unreachable past validation) still files
// only rendered fields, never the agent prose. Every other finding files its
// proposed title and body unchanged.
func upkeepFilingProse(f plan.UpkeepFinding) (title, body string, serverRendered bool) {
	if f.Source != plan.UpkeepSourceAdvisory {
		return f.ProposedIssue.Title, f.ProposedIssue.Body, false
	}
	facts := upkeepAdvisoryFacts(&f)
	return upkeep.RenderAdvisoryTitle(facts), upkeep.RenderAdvisoryFacts(facts), true
}

// upkeepAdvisoryFacts adapts an advisory finding to the renderers' input:
// structured fields only (no proposed prose, no call path), with the cited
// manifest paths in evidence order. A missing advisory object renders every
// field as withheld.
func upkeepAdvisoryFacts(f *plan.UpkeepFinding) upkeep.AdvisoryFacts {
	a := f.Advisory
	if a == nil {
		return upkeep.AdvisoryFacts{Manifests: upkeepAdvisoryManifestPaths(f)}
	}
	facts := upkeep.AdvisoryFacts{
		IDs:          append([]string(nil), a.AdvisoryIDs...),
		Ecosystem:    a.Ecosystem,
		Package:      a.Package,
		Version:      a.Version,
		Reachability: a.Reachability,
		Severity:     a.Severity,
		Manifests:    upkeepAdvisoryManifestPaths(f),
	}
	if a.FixedVersion != nil {
		facts.FixedVersion = *a.FixedVersion
	}
	return facts
}

// upkeepAdvisoryManifestPaths returns the distinct file-ref paths that name a
// manifest — basename in plan.UpkeepManifestBasenames, repository-relative,
// not escaping the root (plan.UpkeepAdvisoryManifestDirs' predicate) — in
// evidence order. A call-site source file cited as evidence is never listed.
func upkeepAdvisoryManifestPaths(f *plan.UpkeepFinding) []string {
	manifests := map[string]bool{}
	for _, b := range plan.UpkeepManifestBasenames() {
		manifests[b] = true
	}
	out := []string{}
	seen := map[string]bool{}
	for _, ev := range f.Evidence {
		c := path.Clean(ev.Path)
		if ev.Kind != plan.UpkeepEvidenceKindFile || ev.Path == "" || seen[ev.Path] || !manifests[path.Base(c)] ||
			path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
			continue
		}
		seen[ev.Path] = true
		out = append(out, ev.Path)
	}
	return out
}

// upkeepFilingLabels returns the proposal's labels with every autonomy:* label
// removed unless authorized, and the removed labels. Both are non-nil.
func upkeepFilingLabels(proposed []string, authorized bool) ([]string, []string) {
	kept, stripped := []string{}, []string{}
	for _, l := range proposed {
		if !authorized && strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), "autonomy:") {
			stripped = append(stripped, l)
			continue
		}
		kept = append(kept, l)
	}
	return kept, stripped
}

// upkeepAdvisoryLabelNamespaces are the only label namespaces an ADVISORY
// filing carries (#3750). An agent-proposed label is a whitespace-free token
// of up to 50 runes, so one such as `internal/server/serve.go` or
// `server.serveH2` would carry a call-path frame to the tracker past the
// server-rendered title and body.
var upkeepAdvisoryLabelNamespaces = []string{"area:", "type:", "phase:"}

// upkeepAdvisoryLabels narrows an advisory finding's labels to
// upkeepAdvisoryLabelNamespaces, appending every other label (an authorized
// autonomy:* label included) to stripped. Both results are non-nil.
func upkeepAdvisoryLabels(labels, stripped []string) ([]string, []string) {
	kept := []string{}
	for _, l := range labels {
		norm := strings.ToLower(strings.TrimSpace(l))
		allowed := false
		for _, ns := range upkeepAdvisoryLabelNamespaces {
			if strings.HasPrefix(norm, ns) {
				allowed = true
				break
			}
		}
		if allowed {
			kept = append(kept, l)
		} else {
			stripped = append(stripped, l)
		}
	}
	return kept, stripped
}

// upkeepFilingBody appends the finding's hidden marker on its own line — the
// marker the next scan's ingest dedupe matches against open issues.
func upkeepFilingBody(body, findingID string) string {
	marker := upkeep.FindingMarker(findingID)
	if strings.Contains(body, marker) {
		return body
	}
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return marker
	}
	return body + "\n\n" + marker
}

// normalizeUpkeepEpicRef renders a bare (389) or #-prefixed (#389) epic ref
// as `#389`; empty stays empty.
func normalizeUpkeepEpicRef(ref string) string {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "#")
	if ref == "" {
		return ""
	}
	return "#" + ref
}

// upkeepRecordedArtifact reads the artifact id named by the HIGHEST-sequence
// upkeep_report_recorded row — latestUpkeepReport's selection rule, without the
// body read. found=false with a nil error means no row.
func (s *Server) upkeepRecordedArtifact(ctx context.Context, runID uuid.UUID) (uuid.UUID, bool, error) {
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, audit.UpkeepReportRecordedCategory)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("list upkeep_report_recorded rows: %w", err)
	}
	var newest *audit.Entry
	for _, e := range rows {
		if e != nil && (newest == nil || e.Sequence >= newest.Sequence) {
			newest = e
		}
	}
	if newest == nil {
		return uuid.Nil, false, nil
	}
	var p struct {
		ArtifactID string `json:"artifact_id"`
	}
	if jerr := json.Unmarshal(newest.Payload, &p); jerr != nil {
		return uuid.Nil, false, fmt.Errorf("decode upkeep_report_recorded row %d: %w", newest.Sequence, jerr)
	}
	id, perr := uuid.Parse(p.ArtifactID)
	if perr != nil {
		return uuid.Nil, false, fmt.Errorf("upkeep_report_recorded row %d names artifact %q: %w", newest.Sequence, p.ArtifactID, perr)
	}
	return id, true, nil
}

// upkeepRecordedRow returns the highest-sequence upkeep_report_recorded row
// naming artifactID: the ONE row both the duplicates and the coverage marks
// are read from. No such row is an error (fail closed).
func (s *Server) upkeepRecordedRow(ctx context.Context, runID uuid.UUID, artifactID string) (*audit.Entry, error) {
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, audit.UpkeepReportRecordedCategory)
	if err != nil {
		return nil, fmt.Errorf("list upkeep_report_recorded rows: %w", err)
	}
	var best *audit.Entry
	for _, e := range rows {
		if e == nil {
			continue
		}
		var probe struct {
			ArtifactID string `json:"artifact_id"`
		}
		if json.Unmarshal(e.Payload, &probe) != nil || probe.ArtifactID != artifactID {
			continue
		}
		if best == nil || e.Sequence >= best.Sequence {
			best = e
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no upkeep_report_recorded row names artifact %s", artifactID)
	}
	return best, nil
}

// upkeepRecordedDuplicates reads the ingest's dedupe verdict from the recorded
// row. An absent, null or non-array duplicates key is UNREADABLE (fail closed:
// the apply cannot tell which findings an open issue already covers).
func upkeepRecordedDuplicates(e *audit.Entry) (map[string]upkeep.Duplicate, error) {
	var rec struct {
		Duplicates *[]upkeep.Duplicate `json:"duplicates"`
	}
	if jerr := json.Unmarshal(e.Payload, &rec); jerr != nil {
		return nil, fmt.Errorf("decode duplicates on upkeep_report_recorded row %d: %w", e.Sequence, jerr)
	}
	if rec.Duplicates == nil {
		return nil, fmt.Errorf("upkeep_report_recorded row %d carries no duplicates array", e.Sequence)
	}
	out := make(map[string]upkeep.Duplicate, len(*rec.Duplicates))
	for _, d := range *rec.Duplicates {
		if d.FindingID != "" {
			out[d.FindingID] = d
		}
	}
	return out, nil
}

// upkeepRecordedCoverage reads the ingest's Dependabot coverage marks (#3750)
// from the recorded row, deciding on the RAW JSON because the three states
// differ:
//
//   - key ABSENT: no marks. A row recorded before #3750 carries no `covered`
//     key, and such a report cannot carry an advisory finding.
//   - key present but null, or not an array: UNREADABLE (fail closed: the
//     ingest always records an array, so anything else is a corrupt row and
//     the apply cannot tell which findings are covered).
//   - an array: marks by finding id.
func upkeepRecordedCoverage(e *audit.Entry) (map[string]upkeep.Covered, error) {
	var fields map[string]json.RawMessage
	if jerr := json.Unmarshal(e.Payload, &fields); jerr != nil {
		return nil, fmt.Errorf("decode upkeep_report_recorded row %d: %w", e.Sequence, jerr)
	}
	raw, present := fields["covered"]
	if !present {
		return map[string]upkeep.Covered{}, nil
	}
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		return nil, fmt.Errorf("upkeep_report_recorded row %d carries a covered value that is not an array: %s", e.Sequence, upkeepTruncateRaw(raw))
	}
	var marks []upkeep.Covered
	if jerr := json.Unmarshal(raw, &marks); jerr != nil {
		return nil, fmt.Errorf("decode covered on upkeep_report_recorded row %d: %w", e.Sequence, jerr)
	}
	out := make(map[string]upkeep.Covered, len(marks))
	for _, c := range marks {
		if c.FindingID != "" {
			out[c.FindingID] = c
		}
	}
	return out, nil
}

// upkeepTruncateRaw bounds a raw JSON value echoed into a degrade detail.
func upkeepTruncateRaw(raw json.RawMessage) string {
	const max = 64
	if len(raw) <= max {
		return string(raw)
	}
	return string(raw[:max]) + "...[truncated]"
}

// upkeepPriorFilings maps finding id → issue number for the run's
// upkeep_finding_filed rows of artifactID. A row it cannot attribute is
// UNREADABLE (fail closed: it might be the filing a re-apply would duplicate).
func (s *Server) upkeepPriorFilings(ctx context.Context, runID uuid.UUID, artifactID string) (map[string]int, error) {
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryUpkeepFindingFiled)
	if err != nil {
		return nil, fmt.Errorf("list upkeep_finding_filed rows: %w", err)
	}
	out := map[string]int{}
	for _, e := range rows {
		if e == nil {
			continue
		}
		var p upkeepFindingFiledPayload
		if jerr := json.Unmarshal(e.Payload, &p); jerr != nil || p.FindingID == "" {
			return nil, fmt.Errorf("upkeep_finding_filed row %d is not attributable", e.Sequence)
		}
		if p.ArtifactID == artifactID {
			out[p.FindingID] = p.IssueNumber
		}
	}
	return out, nil
}

// settleUpkeepWindow closes artifactID's upkeep capture window with the given
// settlement, returning the consumed dispositions collapsed last-wins per
// finding id. It drives audit.UpkeepWindowAppender when present (production)
// and a non-atomic, permanence-aware read-then-append fallback otherwise
// (settleGroomingWindow's shape over the upkeep categories).
func (s *Server) settleUpkeepWindow(ctx context.Context, stage *run.Stage, artifactID, settlement string) (map[string]upkeepConsumedDisposition, error) {
	now := time.Now().UTC()
	payload, err := json.Marshal(groomingWindowPayload{
		RunID: stage.RunID.String(), StageID: stage.ID.String(),
		ArtifactID: artifactID, Settlement: settlement,
		ClosedAt: now.Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, err
	}
	systemKind := audit.ActorSystem
	stageID := stage.ID
	params := audit.ChainAppendParams{
		RunID: stage.RunID, StageID: &stageID, Timestamp: now,
		Category: audit.UpkeepApplyWindowClosedCategory, ActorKind: &systemKind, Payload: payload,
	}

	if appender, ok := s.cfg.AuditRepo.(audit.UpkeepWindowAppender); ok {
		_, consumed, aerr := appender.AppendChainedUpkeepWindowClose(ctx, params, artifactID)
		if aerr != nil {
			return nil, aerr
		}
		return collapseUpkeepConsumed(consumed, artifactID), nil
	}

	// FALLBACK (in-memory repos): permanence-aware read-then-append.
	existing, err := s.windowSettlementFor(ctx, stage.RunID, audit.UpkeepApplyWindowClosedCategory, artifactID)
	if err != nil {
		return nil, err
	}
	var belowSeq int64
	if existing != nil {
		belowSeq = existing.AuditSequence // PERMANENCE: never extend the bound.
	} else {
		wm, aerr := s.cfg.AuditRepo.AppendChained(ctx, params)
		if aerr != nil {
			return nil, aerr
		}
		belowSeq = wm.Sequence
	}
	disp, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, stage.RunID, CategoryUpkeepDispositionRecorded)
	if err != nil {
		return nil, err
	}
	scoped := make([]*audit.Entry, 0, len(disp))
	for _, e := range disp {
		if e != nil && e.Sequence < belowSeq {
			scoped = append(scoped, e)
		}
	}
	return collapseUpkeepConsumed(scoped, artifactID), nil
}

// collapseUpkeepConsumed collapses the consumed upkeep_disposition_recorded
// rows of artifactID LAST-WINS per finding id; an undecodable row is skipped
// (a junk row must not manufacture a verdict).
func collapseUpkeepConsumed(entries []*audit.Entry, artifactID string) map[string]upkeepConsumedDisposition {
	type ranked struct {
		d upkeepConsumedDisposition
		s int64
	}
	latest := map[string]ranked{}
	for _, e := range entries {
		if e == nil {
			continue
		}
		var p upkeepDispositionPayload
		if json.Unmarshal(e.Payload, &p) != nil || p.FindingID == "" || p.ArtifactID != artifactID {
			continue
		}
		if cur, ok := latest[p.FindingID]; ok && cur.s > e.Sequence {
			continue
		}
		latest[p.FindingID] = ranked{upkeepConsumedDisposition{
			Verdict: p.Verdict, AuthorizeDelegationTier: p.AuthorizeDelegationTier, ParentEpic: p.ParentEpic,
		}, e.Sequence}
	}
	out := make(map[string]upkeepConsumedDisposition, len(latest))
	for id, r := range latest {
		out[id] = r.d
	}
	return out
}

// degradeUpkeepApplyPrelaunch records ONE degraded upkeep_apply_completed row
// naming why the apply did not run, alongside a Warn log. When the prelaunch
// context has expired, the step's own failure is a symptom and the reason is
// upkeep_apply_prelaunch_timeout. The row is written on a fresh context (the
// sink's), so an expired prelaunch context does not lose it.
func (s *Server) degradeUpkeepApplyPrelaunch(ctx context.Context, sink *upkeepApplySink, artifactID, reason, detail string) {
	if ctx.Err() != nil {
		detail = reason + ": " + detail
		reason = upkeepApplyPrelaunchTimeout
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "upkeep apply degraded; nothing was filed",
		slog.String("run_id", sink.runID.String()),
		slog.String("stage_id", sink.stageID.String()),
		slog.String("degrade_reason", reason),
		slog.String("detail", detail))
	if err := sink.append(ctx, CategoryUpkeepApplyCompleted, upkeepApplyCompletedPayload{
		ArtifactID: artifactID, Degraded: true, DegradeReason: reason,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "upkeep apply: degrade marker not recorded",
			slog.String("run_id", sink.runID.String()),
			slog.String("stage_id", sink.stageID.String()),
			slog.String("error", err.Error()))
	}
}
