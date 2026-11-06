package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/invariantmonitor"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ADR-141 (#5053, #4870): the fix-up, conflict-resolution and
// acceptance-scenario push reports feed the lineage ledger, merge-candidate
// verification and merge-queue re-entry. Before one of them writes its row, the
// reported head must EXACTLY equal the forge's PR head or the tip of a branch
// the server derived. A commit that is merely reachable is refused. On the
// conflict and acceptance kinds the reported base must also be that head's first
// parent. A refusal can be a benign head-movement race and is never by itself
// proof of misconduct.
//
// Skips are explicit residuals rather than a claim of universal forge
// verification: a server with no forge client and a run with no installation
// (which includes every GitLab run) admit the report unchecked.

// reportedHeadKind names the push report being checked.
type reportedHeadKind int

const (
	reportedHeadFixupPush reportedHeadKind = iota
	reportedHeadConflictResolutionPush
	reportedHeadAcceptanceScenariosPush
)

// reportedHeadVerdict is the outcome of one check.
type reportedHeadVerdict int

const (
	reportedHeadVerified reportedHeadVerdict = iota
	reportedHeadMismatch
	reportedHeadUndeterminable
)

// Refusal reasons recorded on the evidence row and in the refusal text.
const (
	reportedHeadReasonHead     = "head"
	reportedHeadReasonBaseEdge = "base_edge"
)

// Diagnostic bounds (approval condition C3). Runner strings stay raw for the
// exact comparisons; every NEW diagnostic sink receives only the formatted form.
const (
	reportedHeadMaxObjectIDLen   = 64
	reportedHeadMaxBranchLen     = 255
	reportedHeadMaxParents       = 8
	reportedHeadObjectIDFallback = "(invalid-or-absent-id)"
	reportedHeadBranchFallback   = "(invalid-or-absent-branch)"
)

// reportedHeadForgeTimeout is the ONE deadline every forge read of a check
// shares, so a hung forge cannot hold the report handler past it. A variable so
// a test can shrink it; nothing else writes it.
var reportedHeadForgeTimeout = 15 * time.Second

// reportedHeadEvidence is what a check learned. Reasons apply to a mismatch;
// cause and err describe an undeterminable verdict.
type reportedHeadEvidence struct {
	verdict reportedHeadVerdict
	prHead  string
	branch  string
	tip     string
	parents []string
	reason  string
	cause   string
	err     error
}

// undeterminable records why confirmation is unknown. cause is a fixed class;
// no runner-supplied value can reach err, because a reported head reaches the
// forge only after exactly matching a forge value and the reported base and
// branch never reach it.
func (ev reportedHeadEvidence) undeterminable(cause string, err error) reportedHeadEvidence {
	ev.verdict = reportedHeadUndeterminable
	ev.cause = cause
	ev.err = err
	return ev
}

// reportedHeadInstallationID answers 0 for a run with no installation.
func reportedHeadInstallationID(runRow *run.Run) int64 {
	if runRow.InstallationID == nil {
		return 0
	}
	return *runRow.InstallationID
}

// verifyReportedHead runs the side-effect-free check. It never reads the
// reported branch, never calls CompareCommits and never consults reachability.
// A read failure is undeterminable, so it can never become a mismatch.
func (s *Server) verifyReportedHead(ctx context.Context, runID uuid.UUID, stage *run.Stage,
	pr *pullRequestBody, kind reportedHeadKind) reportedHeadEvidence {
	ev := reportedHeadEvidence{verdict: reportedHeadVerified}
	if s.cfg.GitHub == nil {
		return ev
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		return ev.undeterminable("run_read", err)
	}
	installationID := reportedHeadInstallationID(runRow)
	if installationID == 0 {
		return ev
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return ev.undeterminable("repository_unparseable", err)
	}
	scope := forge.FromGitHubInstallationID(installationID)

	fctx, cancel := context.WithTimeout(ctx, reportedHeadForgeTimeout)
	defer cancel()

	head := pr.HeadSHA
	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)
	if prNumber > 0 {
		forgePR, err := s.cfg.GitHub.GetPullRequest(fctx, scope, repo, prNumber)
		if err != nil {
			return ev.undeterminable("pull_request_read", err)
		}
		ev.prHead = forgePR.HeadSHA
		ev.branch = forgePR.HeadRef
	} else {
		branch, err := s.reportedHeadDerivedBranch(ctx, runRow, stage)
		if err != nil {
			return ev.undeterminable("branch_resolution", err)
		}
		ev.branch = branch
	}

	if head != ev.prHead {
		if ev.branch == "" {
			return ev.undeterminable("pull_request_head_ref_empty", nil)
		}
		tip, _, err := s.cfg.GitHub.GetBranchSHA(fctx, scope, repo, ev.branch)
		if err != nil {
			return ev.undeterminable("branch_ref_read", err)
		}
		ev.tip = tip
		if head != ev.tip {
			ev.verdict = reportedHeadMismatch
			ev.reason = reportedHeadReasonHead
			return ev
		}
	}

	if kind != reportedHeadFixupPush {
		// The forge has just named this head, so even a not-found here is an
		// unknown rather than a mismatch.
		commit, err := s.cfg.GitHub.GetCommit(fctx, scope, repo, head)
		if err != nil {
			return ev.undeterminable("commit_read", err)
		}
		ev.parents = commit.Parents
		if len(ev.parents) == 0 || ev.parents[0] != pr.BaseSHA {
			ev.verdict = reportedHeadMismatch
			ev.reason = reportedHeadReasonBaseEdge
			return ev
		}
	}
	return ev
}

// reportedHeadDerivedBranch names the branch a run with no tracked PR pushes to.
// It deliberately reuses neither fixupBranchForRun, whose probe swallows a
// repository error, nor resolveAcceptanceRunBranchAndPR, which reads
// runner-reported branch fields: a repository failure must stay an error here so
// it can never become a mismatch or a failure category.
func (s *Server) reportedHeadDerivedBranch(ctx context.Context, runRow *run.Run, stage *run.Stage) (string, error) {
	if runRow.DecomposedFrom != nil {
		return fixupBranchFor(runRow, stage), nil
	}
	children, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{DecomposedFrom: &runRow.ID, Limit: 1})
	if err != nil {
		return "", fmt.Errorf("reported head: probe decomposed children: %w", err)
	}
	if len(children) > 0 {
		return orchestrator.ConsolidatedBranch(runRow.ID), nil
	}
	if stage.Type == run.StageTypeImplement {
		return fixupBranchFor(runRow, stage), nil
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runRow.ID)
	if err != nil {
		return "", fmt.Errorf("reported head: list stages: %w", err)
	}
	var implement *run.Stage
	for _, st := range stages {
		if st.Type == run.StageTypeImplement && (implement == nil || st.Sequence > implement.Sequence) {
			implement = st
		}
	}
	if implement == nil {
		return "", fmt.Errorf("reported head: run %s has no implement stage", runRow.ID)
	}
	return fixupBranchFor(runRow, implement), nil
}

// reportedHeadRecoveryState performs every read the refusal's recovery depends
// on BEFORE any transition, so a failed read answers 503 rather than selecting a
// category or recovery from a partial view. The conflict and verify reads choose
// which recovery fires; only the fix-up state selects the category.
func (s *Server) reportedHeadRecoveryState(ctx context.Context, runID, stageID uuid.UUID) (bool, error) {
	for _, category := range []string{
		CategoryStageConflictResolutionTriggered,
		CategoryStageConflictResolutionFailed,
		CategoryConflictResolutionPushed,
	} {
		if _, err := s.stageEntriesStrict(ctx, runID, stageID, category); err != nil {
			return false, fmt.Errorf("read %s: %w", category, err)
		}
	}
	if _, err := s.liveMergeCandidateTrigger(ctx, runID, stageID); err != nil {
		return false, fmt.Errorf("read merge-candidate trigger: %w", err)
	}
	return s.fixupTriggerActive(ctx, runID, stageID)
}

// fixupTriggerActive reports whether the stage's newest fix-up trigger is still
// live. A settled pass (pushed, no changes or recovered after the trigger) and a
// confirmed supersession have spent it. dispatch_reaper_failed spends nothing,
// because a retry re-runs that pass on the same trigger, and neither does an
// inert supersession marker. A malformed newest trigger is indeterminate.
func (s *Server) fixupTriggerActive(ctx context.Context, runID, stageID uuid.UUID) (bool, error) {
	triggers, err := s.stageEntriesStrict(ctx, runID, stageID, CategoryStageFixupTriggered)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", CategoryStageFixupTriggered, err)
	}
	if len(triggers) == 0 {
		return false, nil
	}
	newest := triggers[len(triggers)-1]
	var anchor struct {
		PriorState string `json:"prior_state"`
	}
	if err := json.Unmarshal(newest.Payload, &anchor); err != nil {
		return false, fmt.Errorf("decode newest %s: %w", CategoryStageFixupTriggered, err)
	}
	for _, category := range []string{CategoryFixupPushed, CategoryFixupNoChanges, CategoryStageFixupRecovered} {
		settled, err := s.stageEntriesStrict(ctx, runID, stageID, category)
		if err != nil {
			return false, fmt.Errorf("read %s: %w", category, err)
		}
		if len(settled) > 0 && settled[len(settled)-1].Sequence > newest.Sequence {
			return false, nil
		}
	}
	markers, err := s.stageEntriesStrict(ctx, runID, stageID, CategoryStageFixupSuperseded)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", CategoryStageFixupSuperseded, err)
	}
	if len(markers) == 0 {
		return true, nil
	}
	conversions, err := s.stageEntriesStrict(ctx, runID, stageID, CategoryStageMergeCandidateVerifyTriggered)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", CategoryStageMergeCandidateVerifyTriggered, err)
	}
	if _, superseded := confirmedFixupSupersessions(markers, conversions)[fixupSupersessionKey{StageID: stageID, TriggerSequence: newest.Sequence}]; superseded {
		return false, nil
	}
	return true, nil
}

// reportedHeadFailureCategory holds the Captain's ruling (ADR-141 D3): an active
// fix-up trigger keeps category-B recovery and its pass budget; with none the
// stage fails category C so the same stage can be retried.
func reportedHeadFailureCategory(fixupTriggerActive bool) string {
	if fixupTriggerActive {
		return string(run.FailureB)
	}
	return string(run.FailureC)
}

// reportedHeadDedupCategory is the audit category a report's replay guard reads.
func reportedHeadDedupCategory(kind reportedHeadKind) string {
	switch kind {
	case reportedHeadConflictResolutionPush:
		return CategoryConflictResolutionPushed
	case reportedHeadAcceptanceScenariosPush:
		return CategoryAcceptanceScenariosPushed
	default:
		return CategoryFixupPushed
	}
}

// reportedHeadReplayUnknown re-reads the report's dedup rows on a putative
// mismatch. The handler's dedup fails open on a read error, so a failed read
// cannot tell a false report from a replay after the tip moved; a row now visible
// is a replay that raced the first read. Either way confirmation is unknown.
func (s *Server) reportedHeadReplayUnknown(ctx context.Context, runID, stageID uuid.UUID, headSHA string, kind reportedHeadKind) bool {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, reportedHeadDedupCategory(kind))
	if err != nil {
		return true
	}
	if kind == reportedHeadConflictResolutionPush {
		return auditEntryForStage(entries, stageID)
	}
	return childPushAlreadyRecorded(entries, stageID, headSHA)
}

// formatReportedHeadObjectID bounds a commit id for a diagnostic sink. Anything
// other than 1 to 64 hexadecimal characters becomes the fixed placeholder.
func formatReportedHeadObjectID(id string) string {
	if id == "" || len(id) > reportedHeadMaxObjectIDLen {
		return reportedHeadObjectIDFallback
	}
	for i := 0; i < len(id); i++ {
		switch c := id[i]; {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return reportedHeadObjectIDFallback
		}
	}
	return id
}

// formatReportedHeadBranch bounds a branch name for a diagnostic sink. A name
// the shared line-structure escaper would alter is invalid rather than escaped,
// and so is an overlong one.
func formatReportedHeadBranch(branch string) string {
	if branch == "" || len(branch) > reportedHeadMaxBranchLen || prompt.SanitizeScopePath(branch) != branch {
		return reportedHeadBranchFallback
	}
	return branch
}

// reportedHeadDiagnostics holds the only forms of the reported and forge values
// any refusal sink may carry.
type reportedHeadDiagnostics struct {
	reportedHead   string
	reportedBase   string
	reportedBranch string
	forgePRHead    string
	forgeBranch    string
	forgeTip       string
	forgeParents   []string
}

func newReportedHeadDiagnostics(pr *pullRequestBody, res reportedHeadEvidence) reportedHeadDiagnostics {
	parents := res.parents
	if len(parents) > reportedHeadMaxParents {
		parents = parents[:reportedHeadMaxParents]
	}
	forgeParents := make([]string, 0, len(parents))
	for _, parent := range parents {
		forgeParents = append(forgeParents, formatReportedHeadObjectID(parent))
	}
	return reportedHeadDiagnostics{
		reportedHead:   formatReportedHeadObjectID(pr.HeadSHA),
		reportedBase:   formatReportedHeadObjectID(pr.BaseSHA),
		reportedBranch: formatReportedHeadBranch(pr.Branch),
		forgePRHead:    formatReportedHeadObjectID(res.prHead),
		forgeBranch:    formatReportedHeadBranch(res.branch),
		forgeTip:       formatReportedHeadObjectID(res.tip),
		forgeParents:   forgeParents,
	}
}

// refusalReason is the neutral text every refusal surface shares: it names both
// heads and states the disagreement, never calling the commit or the report false.
func (d reportedHeadDiagnostics) refusalReason(reason string) string {
	return fmt.Sprintf("reported head %s (base %s) disagreed with forge state on %s: forge PR head %s, forge branch tip %s, forge parents [%s]; this can be a benign head-movement race",
		d.reportedHead, d.reportedBase, reason, d.forgePRHead, d.forgeTip, strings.Join(d.forgeParents, " "))
}

// attrs is the evidence every refusal WARN carries.
func (d reportedHeadDiagnostics) attrs(runID, stageID uuid.UUID) []slog.Attr {
	return []slog.Attr{
		slog.String("run_id", runID.String()),
		slog.String("stage_id", stageID.String()),
		slog.String("reported_head", d.reportedHead),
		slog.String("reported_base", d.reportedBase),
		slog.String("reported_branch", d.reportedBranch),
		slog.String("forge_pr_head", d.forgePRHead),
		slog.String("forge_branch", d.forgeBranch),
		slog.String("forge_tip", d.forgeTip),
		slog.String("forge_parents", strings.Join(d.forgeParents, " ")),
	}
}

// details is the evidence a 409 body and the invariant row carry.
func (d reportedHeadDiagnostics) details() map[string]any {
	return map[string]any{
		"reported_head":   d.reportedHead,
		"reported_base":   d.reportedBase,
		"reported_branch": d.reportedBranch,
		"forge_pr_head":   d.forgePRHead,
		"forge_branch":    d.forgeBranch,
		"forge_tip":       d.forgeTip,
		"forge_parents":   d.forgeParents,
	}
}

// refuseUnverifiedReportedHead gates one push report, returning true when it has
// written the response. It runs AFTER the handler's dedup, so a replay stays an
// idempotent 200, and BEFORE the row, stage advance and every later effect.
//
// Verified: the forge-resolved branch replaces the reported one and the handler
// continues. Undeterminable: 503 with nothing recorded, safe to re-post. Confirmed
// mismatch: best-effort evidence, then either a failed-stage 200 on a running
// implement stage or a 409 with no transition.
func (s *Server) refuseUnverifiedReportedHead(w http.ResponseWriter, r *http.Request, runID uuid.UUID, stage *run.Stage,
	pr *pullRequestBody, authMethod string, actorKind audit.ActorKind, actorSubject *string, kind reportedHeadKind) bool {
	res := s.verifyReportedHead(r.Context(), runID, stage, pr, kind)
	if res.verdict == reportedHeadVerified {
		if res.branch != "" {
			pr.Branch = res.branch
		}
		return false
	}

	if res.verdict == reportedHeadMismatch && s.reportedHeadReplayUnknown(r.Context(), runID, stage.ID, pr.HeadSHA, kind) {
		res = res.undeterminable("replay_unknown", nil)
	}
	failStage := kind != reportedHeadAcceptanceScenariosPush && stage.Type == run.StageTypeImplement && stage.State == run.StageStateRunning
	fixupTriggerActive := false
	if res.verdict == reportedHeadMismatch && failStage {
		active, trigErr := s.reportedHeadRecoveryState(r.Context(), runID, stage.ID)
		if trigErr != nil {
			res = res.undeterminable("trigger_state", trigErr)
		}
		fixupTriggerActive = active
	}
	if res.verdict == reportedHeadUndeterminable {
		s.writeReportedHeadUnverifiable(w, r, runID, stage.ID, res)
		return true
	}

	diag := newReportedHeadDiagnostics(pr, res)
	refusalText := diag.refusalReason(res.reason)
	evidenceAttrs := diag.attrs(runID, stage.ID)
	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "reported head: report refused", evidenceAttrs...)

	fields := diag.details()
	fields["kind"] = invariantmonitor.KindReportedHeadMismatch
	fields["run_id"] = runID.String()
	fields["stage_id"] = stage.ID.String()
	fields["outcome"] = pr.Outcome
	fields["reason"] = res.reason
	payload, _ := json.Marshal(fields)
	systemKind := audit.ActorSystem
	stageID := stage.ID
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  invariantmonitor.CategoryInvariantViolation,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "reported head: evidence append failed", evidenceAttrs...)
	}
	s.notifyStatusUpdate(r.Context(), runID, "lineage_violation")

	if failStage {
		synth := pullRequestBody{Outcome: "failed",
			Category: reportedHeadFailureCategory(fixupTriggerActive),
			Reason:   refusalText,
		}
		s.failPullRequestStageWith(w, r, runID, stage, &synth, authMethod, actorKind, actorSubject, failPullRequestOpts{
			skipFixupRecovery: !fixupTriggerActive,

			appendFailureAttrs: evidenceAttrs,
		})
		return true
	}

	details := diag.details()
	details["run_id"] = runID.String()
	details["stage_id"] = stage.ID.String()
	details["reason"] = res.reason
	s.writeError(w, r, http.StatusConflict, "reported_head_mismatch", refusalText, details)
	return true
}

// writeReportedHeadUnverifiable answers the retryable 503. The body names only the
// run and stage. The cause, bounded and structure-neutralized, reaches the
// operator through writeError's non-client channel, joined to the error_ref.
func (s *Server) writeReportedHeadUnverifiable(w http.ResponseWriter, r *http.Request, runID, stageID uuid.UUID, res reportedHeadEvidence) {
	cause := res.cause
	if res.err != nil {
		cause += ": " + truncate(neutralizeUntrustedFixupText(res.err.Error()), 300)
	}
	s.writeError(w, r, http.StatusServiceUnavailable, "reported_head_unverifiable",
		"could not confirm the reported head against forge state; nothing was recorded, so re-post the same report",
		map[string]any{"run_id": runID.String(), "stage_id": stageID.String(), internalCauseKey: cause})
}
