package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergeoutcome"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// ADR-093 slice 1 (D1, E87.4 / #4295): the merge-queue ELIGIBILITY predicate.
// It decides QUEUE ADMISSION itself, not only the merge: POST
// /v0/runs/{id}/merge enqueues only an eligible run (mergeQueueEligibility),
// and the ADR-092 rebase-path enqueue and the pump's admission of a held entry
// consult the admission variant (mergeQueueAdmissionEligibility). Four legs,
// each reusing the existing single-source helper so the queue and the merge
// endpoint cannot drift:
//
//	(a) review gate approved — every configured agent implement reviewer has a
//	    TERMINAL entry (a pure count over
//	    auditcomplete.TerminalImplementReviewCategories; never
//	    checkImplementReviewSettled, which writes a backstop row), review
//	    coverage is complete (reviewCoverageRefusal), and, when the review
//	    stage declares an approval gate, a LIVE forge approval exists by an
//	    approvals.members identity (any live approval when members is empty).
//	    A successfully read spec whose workflow has no review stage, or whose
//	    review stage declares no approval gate, passes; an absent, unreadable
//	    or unparseable spec is UNDETERMINABLE (fail closed).
//	    KNOWN DEVIATION from ADR-093 D6: the approval must be LIVE on the
//	    forge; D6's approval transfer across a clean advance is slice 2's. In
//	    a repository with GitHub's dismiss-stale-approvals setting a rebase
//	    therefore answers not_queued until slice 2 lands.
//	(b) acceptance merge-eligible and not stale — acceptanceGateState +
//	    acceptanceGateAdmitsMerge, and acceptanceStaleRefusal == nil.
//	(c) required checks green at the head — the check-runs listing classified
//	    by mergeoutcome.ClassifyMergeCI against the run's required-checks
//	    snapshot (a nil snapshot judges the observed set). ZERO observed check
//	    runs is NOT green: it is undeterminable. The admission variant refuses
//	    only an observed RED required check (a freshly advanced head is
//	    legitimately pending, and the verify pass is the gate there), so it
//	    neither requires green nor treats an empty listing as a refusal.
//	(d) no open blocking concern — an open concern whose Provenance is
//	    concern.ProvenanceFirstOfficer ("first_officer") or whose Severity is
//	    planreview.SeverityHigh ("high"). Medium/low concerns stay advisory
//	    (ADR-027). No requirement-arbitration concern CATEGORY exists
//	    (requirement_arbitration is a page event, spec.PageEventRequirementArbitration,
//	    never a concern category), so leg (d) matches no category.
//
// The predicate NEVER writes. Every read error is undeterminable, which is
// NOT eligible: uncertainty never enqueues or admits.

// Merge-queue ineligibility reason codes.
const (
	mergeQueueReasonNotApproved              = "not_approved"
	mergeQueueReasonReviewPending            = "review_pending"
	mergeQueueReasonReviewCoverageIncomplete = "review_coverage_incomplete"
	mergeQueueReasonAcceptanceNotEligible    = "acceptance_not_eligible"
	mergeQueueReasonAcceptanceStale          = "acceptance_stale"
	mergeQueueReasonChecksNotGreen           = "checks_not_green"
	mergeQueueReasonChecksRed                = "checks_red"
	mergeQueueReasonBlockingConcernOpen      = "blocking_concern_open"
	mergeQueueReasonUndeterminable           = "undeterminable"
)

// mergeQueueIneligibility is one failed leg: a reason code and its detail.
type mergeQueueIneligibility struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// mergeQueueEligibilityResult is the predicate's verdict. Eligible is true
// only when no leg failed and every read succeeded; Undeterminable is true
// when any leg's read failed (its reason names the leg).
type mergeQueueEligibilityResult struct {
	Eligible       bool
	Undeterminable bool
	Reasons        []mergeQueueIneligibility
}

// codes returns the reason codes in leg order.
func (r mergeQueueEligibilityResult) codes() []string {
	out := make([]string, 0, len(r.Reasons))
	for _, x := range r.Reasons {
		out = append(out, x.Code)
	}
	return out
}

// mergeQueueEligibility is the queue-ENTRY predicate (merge_run's enqueue
// arm): leg (c) requires the required checks GREEN at headSHA.
func (s *Server) mergeQueueEligibility(ctx context.Context, runRow *run.Run, headSHA string) mergeQueueEligibilityResult {
	return s.evaluateMergeQueueEligibility(ctx, runRow, headSHA, false)
}

// mergeQueueAdmissionEligibility is the ADMISSION predicate (the ADR-092
// rebase-path enqueue and the pump's admission of a held entry): identical
// except leg (c) refuses only an observed RED required check.
func (s *Server) mergeQueueAdmissionEligibility(ctx context.Context, runRow *run.Run, headSHA string) mergeQueueEligibilityResult {
	return s.evaluateMergeQueueEligibility(ctx, runRow, headSHA, true)
}

func (s *Server) evaluateMergeQueueEligibility(ctx context.Context, runRow *run.Run, headSHA string, admission bool) mergeQueueEligibilityResult {
	var res mergeQueueEligibilityResult
	add := func(code, detail string) {
		res.Reasons = append(res.Reasons, mergeQueueIneligibility{Code: code, Detail: detail})
	}
	undeterminable := func(leg string, err error) {
		res.Undeterminable = true
		add(mergeQueueReasonUndeterminable, leg+": "+err.Error())
	}
	if runRow == nil {
		undeterminable("run", errors.New("no run row"))
		return res
	}

	// (a) review gate approved.
	if code, detail, err := s.mergeQueueReviewLeg(ctx, runRow); err != nil {
		undeterminable("review gate", err)
	} else if code != "" {
		add(code, detail)
	}

	// (b) acceptance merge-eligible and not stale.
	if code, detail, err := s.mergeQueueAcceptanceLeg(ctx, runRow); err != nil {
		undeterminable("acceptance", err)
	} else if code != "" {
		add(code, detail)
	}

	// (c) required checks at the head.
	if code, detail, err := s.mergeQueueChecksLeg(ctx, runRow, headSHA, admission); err != nil {
		undeterminable("required checks", err)
	} else if code != "" {
		add(code, detail)
	}

	// (d) no open blocking concern.
	if code, detail, err := s.mergeQueueConcernLeg(ctx, runRow); err != nil {
		undeterminable("concerns", err)
	} else if code != "" {
		add(code, detail)
	}

	res.Eligible = len(res.Reasons) == 0
	return res
}

// mergeQueueReviewLeg is leg (a). It returns a reason code ("" when the leg
// passes) or a read error (undeterminable).
func (s *Server) mergeQueueReviewLeg(ctx context.Context, runRow *run.Run) (string, string, error) {
	declared, members, err := reviewApprovalGateForRun(runRow)
	if err != nil {
		return "", "", err
	}

	if cfg := s.resolveStageReviewers(ctx, runRow, spec.StageTypeImplement); cfg != nil && cfg.AgentCount() > 0 {
		if s.cfg.AuditRepo == nil {
			return "", "", errors.New("no audit repository is wired")
		}
		terminal := 0
		for _, cat := range auditcomplete.TerminalImplementReviewCategories {
			entries, lerr := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, cat)
			if lerr != nil {
				return "", "", fmt.Errorf("list %s: %w", cat, lerr)
			}
			terminal += len(entries)
		}
		if terminal < cfg.AgentCount() {
			return mergeQueueReasonReviewPending,
				fmt.Sprintf("%d of %d configured agent implement reviews are terminal", terminal, cfg.AgentCount()), nil
		}
	}

	coverage, err := s.reviewCoverageRefusal(ctx, runRow)
	if err != nil {
		return "", "", err
	}
	if coverage != nil {
		return mergeQueueReasonReviewCoverageIncomplete, coverage.Message, nil
	}

	if !declared {
		return "", "", nil
	}
	reviews, err := s.listMergeSettledReviews(ctx, runRow)
	if err != nil {
		return "", "", err
	}
	approvers := approvingReviewers(reviews)
	if len(members) == 0 {
		if len(approvers) == 0 {
			return mergeQueueReasonNotApproved, "the pull request has no live forge approval", nil
		}
		return "", "", nil
	}
	forgeID := observationForgeID(runRow.InstallationRef)
	ids := make([]string, 0, len(approvers))
	for _, login := range approvers {
		ids = append(ids, identity.CanonicalMember(login, forgeID))
	}
	if listedIdentity(members, ids, forgeID) == "" {
		return mergeQueueReasonNotApproved,
			"no live forge approval is by an approvals.members identity (" + strings.Join(members, ", ") + ")", nil
	}
	return "", "", nil
}

// reviewApprovalGateForRun reads whether the run's review stage declares an
// approval gate and its approvals.members. A successfully read spec whose
// workflow has no review stage, or whose review stage declares no approval
// gate, reports declared=false. An absent, unparseable or workflow-less spec
// is an error (undeterminable, fail closed — approval condition C4).
func reviewApprovalGateForRun(runRow *run.Run) (declared bool, members []string, err error) {
	if len(runRow.WorkflowSpec) == 0 {
		return false, nil, errNoCachedWorkflowSpec
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		return false, nil, fmt.Errorf("parse workflow spec: %w", err)
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return false, nil, fmt.Errorf("workflow %q not in spec", runRow.WorkflowID)
	}
	for _, stg := range wf.Stages {
		if stg.Type != spec.StageTypeReview {
			continue
		}
		for _, gate := range stg.Gates {
			if gate.Type != spec.GateTypeApproval {
				continue
			}
			if gate.Approvals != nil {
				return true, gate.Approvals.Members, nil
			}
			return true, nil, nil
		}
		return false, nil, nil
	}
	return false, nil, nil
}

// mergeQueueAcceptanceLeg is leg (b).
func (s *Server) mergeQueueAcceptanceLeg(ctx context.Context, runRow *run.Run) (string, string, error) {
	if s.cfg.RunRepo == nil {
		return "", "", errors.New("no run repository is wired")
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runRow.ID)
	if err != nil {
		return "", "", err
	}
	state, err := s.acceptanceGateState(ctx, runRow, stages)
	if err != nil {
		return "", "", err
	}
	if !acceptanceGateAdmitsMerge(state) {
		return mergeQueueReasonAcceptanceNotEligible, "the acceptance gate state " + state + " does not admit a merge", nil
	}
	stale, err := s.acceptanceStaleRefusal(ctx, runRow, stages)
	if err != nil {
		return "", "", err
	}
	if stale != nil {
		return mergeQueueReasonAcceptanceStale, stale.Message, nil
	}
	return "", "", nil
}

// mergeQueueChecksLeg is leg (c).
func (s *Server) mergeQueueChecksLeg(ctx context.Context, runRow *run.Run, headSHA string, admission bool) (string, string, error) {
	if s.cfg.GitHub == nil {
		return "", "", errors.New("no GitHub client is wired")
	}
	if headSHA == "" {
		return "", "", errors.New("no head sha to read checks at")
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return "", "", err
	}
	checks, truncated, err := s.cfg.GitHub.ListCheckRunsForRef(ctx, mergeObservationScope(runRow), repo, headSHA)
	if err != nil {
		return "", "", err
	}
	if truncated {
		return "", "", errors.New("the check-runs listing was truncated")
	}
	snapshot := runRow.RequiredChecksSnapshot
	if snapshot == nil {
		// Judge the observed set (an authoritatively-empty required set).
		snapshot = &run.RequiredChecksSnapshot{}
	}
	v := mergeoutcome.ClassifyMergeCI(snapshot, checks, false, time.Now().UTC())
	if v.Conclusion == mergeoutcome.CIConclusionRed {
		return mergeQueueReasonChecksRed, "required check(s) failed at " + headSHA + ": " + strings.Join(v.FailedChecks, ", "), nil
	}
	if admission {
		return "", "", nil
	}
	if len(checks) == 0 {
		// Approval condition C5: zero observed check runs is never green.
		return "", "", errors.New("no check runs are reported at " + headSHA)
	}
	if v.Conclusion != mergeoutcome.CIConclusionGreen {
		return mergeQueueReasonChecksNotGreen,
			"required check(s) not green at " + headSHA + " (" + v.MissingReason + "): " + strings.Join(v.MissingChecks, ", "), nil
	}
	return "", "", nil
}

// mergeQueueConcernLeg is leg (d).
func (s *Server) mergeQueueConcernLeg(ctx context.Context, runRow *run.Run) (string, string, error) {
	if s.cfg.ConcernRepo == nil {
		return "", "", errors.New("no concern repository is wired")
	}
	open, err := s.cfg.ConcernRepo.ListOpenByRun(ctx, runRow.ID)
	if err != nil {
		return "", "", err
	}
	var ids []string
	for _, c := range open {
		if c == nil || !c.State.IsOpen() {
			continue
		}
		if c.Provenance == concern.ProvenanceFirstOfficer || c.Severity == string(planreview.SeverityHigh) {
			ids = append(ids, c.ID.String())
		}
	}
	if len(ids) > 0 {
		return mergeQueueReasonBlockingConcernOpen,
			"open blocking concern(s) (high severity or first_officer provenance): " + strings.Join(ids, ", "), nil
	}
	return "", "", nil
}
