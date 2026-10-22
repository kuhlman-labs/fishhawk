package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergeoutcome"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// ADR-093 slice 1 (D1, E87.4 / #4295), extended by slice 2 (D3/D6, E87.5 /
// #4296): the merge-queue ELIGIBILITY predicate. It decides QUEUE ADMISSION
// itself, not only the merge: POST /v0/runs/{id}/merge enqueues only an
// eligible run (mergeQueueEligibility, the ENTRY variant), the ADR-092
// rebase-path enqueue and the pump's admission of a held entry consult the
// ADMISSION variant (mergeQueueAdmissionEligibility, or
// mergeQueueAdmissionEligibilityAt for an entry carrying merge intent), and
// merge-on-pass consults the MERGE variant (mergeQueueMergeEligibility) on
// every pump. Four legs, each reusing the existing single-source helper so
// the queue and the merge endpoint cannot drift:
//
//	(a) review gate approved — the LATEST implement-review round is settled
//	    (Server.implementReviewRound, read-only: when agent implement
//	    reviewers are configured, a round must have started and its
//	    terminal entries must reach its configured count; an unreadable
//	    round is undeterminable; a terminal entry of an EARLIER round never
//	    counts), review coverage is complete (reviewCoverageRefusal), and,
//	    when the review stage declares an approval gate, no reviewer's
//	    latest non-comment review is CHANGES_REQUESTED (changes_requested,
//	    every variant) and a LIVE forge approval exists by an
//	    approvals.members identity (any live approval when members is
//	    empty). A successfully read spec whose workflow has no review stage,
//	    or whose review stage declares no approval gate, passes; an absent,
//	    unreadable or unparseable spec is UNDETERMINABLE (fail closed).
//	    APPROVAL TRANSFER (ADR-093 D6): with no live approval, a variant
//	    given a merge-intent anchor (mergeQueueIntentAnchor) still passes
//	    when the review-gate approval CARRIES to the head. The carrying set
//	    is seeded by the anchored_head_sha of the merge_queue_entered row at
//	    the anchor's RECORDED sequence (legacy sequence 0: the newest row
//	    naming the anchor's base), then walks the branch_rebased and
//	    conflict_resolution_pushed rows after it in sequence order: a CLEAN
//	    advance (performed, its new head IS its merge commit) from a
//	    carrying head adds its new head; a conflict-resolution push never
//	    carries the approval and clears the set. Transfer holds only when
//	    the head was reached by at least one clean advance (the anchored
//	    head itself never transfers: an approval missing there was
//	    withdrawn, not dismissed by an advance), no reviewer requests
//	    changes. The result then names ApprovalBasis transferred (live
//	    otherwise). (The merge variant's in-lock fold applies the full
//	    ADR-112 CLEAN predicate instead; this walk only ADMITS.)
//	(b) acceptance merge-eligible and not stale — acceptanceGateState +
//	    acceptanceGateAdmitsMerge, and acceptanceStaleRefusal == nil.
//	(c) required checks at the head — the check-runs listing classified by
//	    mergeoutcome.ClassifyMergeCI against the run's required-checks
//	    snapshot (a nil snapshot judges the observed set). ZERO observed
//	    check runs is NOT green: it is undeterminable. The entry and merge
//	    variants require GREEN. The admission variant refuses only an
//	    observed RED required check (a freshly advanced head is
//	    legitimately pending, and the verify pass is the gate there), so it
//	    neither requires green nor treats an empty listing as a refusal.
//	    With a NIL snapshot an observed red may be an optional check, so it
//	    is NOT GREEN rather than red for the variants that act on a queued
//	    entry: the admission variant passes (never dropped), the merge
//	    variant answers checks_not_green (a hold); the entry variant keeps
//	    checks_red.
//	(d) no open blocking concern — an open concern whose Provenance is
//	    concern.ProvenanceFirstOfficer ("first_officer") or whose Severity is
//	    planreview.SeverityHigh ("high"). Medium/low concerns stay advisory
//	    (ADR-027). No requirement-arbitration concern CATEGORY exists
//	    (requirement_arbitration is a page event, spec.PageEventRequirementArbitration,
//	    never a concern category), so leg (d) matches no category.
//
// ADR-112 (#4619) restricts leg (a)'s FORGE sub-legs (changes_requested, the
// live approval, the D6 transfer) to the ENTRY and ADMISSION variants: the
// MERGE variant keeps the Fishhawk sub-legs (review_pending,
// review_coverage_incomplete) and the spec read, and makes no forge review
// read at all, because merge-on-pass judges approvals and vetoes through the
// in-lock forge-approval fold (forge_approval.go). Its ApprovalBasis is
// therefore always "" and the merge path takes the basis from the fold.
//
// The predicate NEVER writes. Every read error is undeterminable, which is
// NOT eligible: uncertainty never enqueues, admits or merges.

// Merge-queue ineligibility reason codes.
const (
	mergeQueueReasonNotApproved              = "not_approved"
	mergeQueueReasonChangesRequested         = "changes_requested"
	mergeQueueReasonReviewPending            = "review_pending"
	mergeQueueReasonReviewCoverageIncomplete = "review_coverage_incomplete"
	mergeQueueReasonAcceptanceNotEligible    = "acceptance_not_eligible"
	mergeQueueReasonAcceptanceStale          = "acceptance_stale"
	mergeQueueReasonChecksNotGreen           = "checks_not_green"
	mergeQueueReasonChecksRed                = "checks_red"
	mergeQueueReasonBlockingConcernOpen      = "blocking_concern_open"
	mergeQueueReasonUndeterminable           = "undeterminable"
)

// Approval bases mergeQueueEligibilityResult.ApprovalBasis reports when leg
// (a) passed on a declared approval gate: a LIVE forge approval, or the
// ADR-093 D6 approval TRANSFERRED across clean advances.
const (
	mergeQueueApprovalLive        = "live"
	mergeQueueApprovalTransferred = "transferred"
)

// mergeQueueEligibilityVariant selects the caller's leg-(c) and transfer
// rules.
type mergeQueueEligibilityVariant int

const (
	// mergeQueueVariantEntry is merge_run's enqueue arm: checks GREEN, an
	// observed red is checks_red.
	mergeQueueVariantEntry mergeQueueEligibilityVariant = iota
	// mergeQueueVariantAdmission is the pump's admission: only a red
	// required check refuses.
	mergeQueueVariantAdmission
	// mergeQueueVariantMerge is merge-on-pass: checks GREEN, and no forge
	// review read (the in-lock forge-approval fold judges approvals, ADR-112).
	mergeQueueVariantMerge
)

// mergeQueueIntentAnchor names the merge_queue_entered row that recorded a
// queue entry's merge intent: its chain Sequence (0 for an entry whose intent
// predates the recorded sequence) and the entry's BaseRef. The zero value is
// NO anchor: leg (a) never transfers an approval without one.
type mergeQueueIntentAnchor struct {
	Sequence int64
	BaseRef  string
}

func (a mergeQueueIntentAnchor) isZero() bool { return a.Sequence == 0 && a.BaseRef == "" }

// mergeQueueIneligibility is one failed leg: a reason code and its detail.
type mergeQueueIneligibility struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// mergeQueueEligibilityResult is the predicate's verdict. Eligible is true
// only when no leg failed and every read succeeded; Undeterminable is true
// when any leg's read failed (its reason names the leg). ApprovalBasis is
// mergeQueueApprovalLive or mergeQueueApprovalTransferred when leg (a) passed
// on a declared approval gate, "" otherwise (no gate declared, or leg (a)
// did not pass).
type mergeQueueEligibilityResult struct {
	Eligible       bool
	Undeterminable bool
	Reasons        []mergeQueueIneligibility
	ApprovalBasis  string
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
// arm): leg (c) requires the required checks GREEN at headSHA. It carries no
// intent anchor, so leg (a) needs a LIVE approval.
func (s *Server) mergeQueueEligibility(ctx context.Context, runRow *run.Run, headSHA string) mergeQueueEligibilityResult {
	return s.evaluateMergeQueueEligibility(ctx, runRow, headSHA, mergeQueueIntentAnchor{}, mergeQueueVariantEntry)
}

// mergeQueueAdmissionEligibility is the ADMISSION predicate with no intent
// anchor (the ADR-092 rebase-path enqueue, and an entry without merge
// intent): identical to the entry variant except leg (c) refuses only an
// observed RED required check.
func (s *Server) mergeQueueAdmissionEligibility(ctx context.Context, runRow *run.Run, headSHA string) mergeQueueEligibilityResult {
	return s.evaluateMergeQueueEligibility(ctx, runRow, headSHA, mergeQueueIntentAnchor{}, mergeQueueVariantAdmission)
}

// mergeQueueAdmissionEligibilityAt is the ADMISSION predicate for an entry
// whose merge intent anchor is anchor: leg (a) may carry the review-gate
// approval across clean advances since the intent (ADR-093 D6).
func (s *Server) mergeQueueAdmissionEligibilityAt(ctx context.Context, runRow *run.Run, headSHA string, anchor mergeQueueIntentAnchor) mergeQueueEligibilityResult {
	return s.evaluateMergeQueueEligibility(ctx, runRow, headSHA, anchor, mergeQueueVariantAdmission)
}

// mergeQueueMergeEligibility is merge-on-pass's MERGE predicate (ADR-093
// D3): leg (c) requires the required checks GREEN at headSHA, and leg (a)
// makes no forge review read (ADR-112: the in-lock fold does). anchor is
// unused by leg (a) on this variant and kept for the caller's signature.
func (s *Server) mergeQueueMergeEligibility(ctx context.Context, runRow *run.Run, headSHA string, anchor mergeQueueIntentAnchor) mergeQueueEligibilityResult {
	return s.evaluateMergeQueueEligibility(ctx, runRow, headSHA, anchor, mergeQueueVariantMerge)
}

func (s *Server) evaluateMergeQueueEligibility(ctx context.Context, runRow *run.Run, headSHA string,
	anchor mergeQueueIntentAnchor, variant mergeQueueEligibilityVariant) mergeQueueEligibilityResult {
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
	if code, detail, basis, err := s.mergeQueueReviewLeg(ctx, runRow, headSHA, anchor, variant); err != nil {
		undeterminable("review gate", err)
	} else if code != "" {
		add(code, detail)
	} else {
		res.ApprovalBasis = basis
	}

	// (b) acceptance merge-eligible and not stale.
	if code, detail, err := s.mergeQueueAcceptanceLeg(ctx, runRow); err != nil {
		undeterminable("acceptance", err)
	} else if code != "" {
		add(code, detail)
	}

	// (c) required checks at the head.
	if code, detail, err := s.mergeQueueChecksLeg(ctx, runRow, headSHA, variant); err != nil {
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
// passes), the approval basis when it passes on a declared approval gate, or
// a read error (undeterminable).
func (s *Server) mergeQueueReviewLeg(ctx context.Context, runRow *run.Run, headSHA string,
	anchor mergeQueueIntentAnchor, variant mergeQueueEligibilityVariant) (code, detail, basis string, err error) {
	declared, members, err := reviewApprovalGateForRun(runRow)
	if err != nil {
		return "", "", "", err
	}

	if cfg := s.resolveStageReviewers(ctx, runRow, spec.StageTypeImplement); cfg != nil && cfg.AgentCount() > 0 {
		if s.cfg.AuditRepo == nil {
			return "", "", "", errors.New("no audit repository is wired")
		}
		// Bound to the LATEST round (carried concern 78c08b62): a fix-up
		// re-review's fresh started row supersedes a settled earlier round,
		// so that round's terminal rows never satisfy this leg.
		round := s.implementReviewRound(ctx, runRow)
		if !round.ok {
			return "", "", "", errors.New("the latest implement-review round could not be read")
		}
		if round.configured > 0 && (!round.started || round.terminal < round.configured) {
			if !round.started {
				return mergeQueueReasonReviewPending,
					fmt.Sprintf("no implement-review round has started for the %d configured agent implement reviewer(s)", round.configured), "", nil
			}
			return mergeQueueReasonReviewPending,
				fmt.Sprintf("%d of %d configured agent implement reviews in the latest round are terminal", round.terminal, round.configured), "", nil
		}
	}

	coverage, err := s.reviewCoverageRefusal(ctx, runRow)
	if err != nil {
		return "", "", "", err
	}
	if coverage != nil {
		return mergeQueueReasonReviewCoverageIncomplete, coverage.Message, "", nil
	}

	if !declared {
		return "", "", "", nil
	}
	// ADR-112 variant restriction (#4619): merge-on-pass applies the ONE
	// forge-approval fold itself, in-lock (mergeOnPassLocked step 6b), so the
	// merge variant makes NO forge review read here: no REST listing, no
	// changes_requested, no approvers and no transfer walk. The entry and
	// admission variants keep these legs unchanged until #4632.
	if variant == mergeQueueVariantMerge {
		return "", "", "", nil
	}
	reviews, err := s.listMergeSettledReviews(ctx, runRow)
	if err != nil {
		return "", "", "", err
	}
	if requesters := changesRequestingReviewers(reviews); len(requesters) > 0 {
		return mergeQueueReasonChangesRequested,
			"reviewer(s) " + strings.Join(requesters, ", ") + " requested changes on the pull request", "", nil
	}

	approvers := approvingReviewers(reviews)
	var notApproved string
	if len(members) == 0 {
		if len(approvers) > 0 {
			return "", "", mergeQueueApprovalLive, nil
		}
		notApproved = "the pull request has no live forge approval"
	} else {
		forgeID := observationForgeID(runRow.InstallationRef)
		ids := make([]string, 0, len(approvers))
		for _, login := range approvers {
			ids = append(ids, identity.CanonicalMember(login, forgeID))
		}
		if listedIdentity(members, ids, forgeID) != "" {
			return "", "", mergeQueueApprovalLive, nil
		}
		notApproved = "no live forge approval is by an approvals.members identity (" + strings.Join(members, ", ") + ")"
	}

	if anchor.isZero() {
		return mergeQueueReasonNotApproved, notApproved, "", nil
	}
	carried, why, err := s.mergeQueueApprovalTransfers(ctx, runRow, headSHA, anchor)
	if err != nil {
		return "", "", "", err
	}
	if carried {
		return "", "", mergeQueueApprovalTransferred, nil
	}
	return mergeQueueReasonNotApproved, notApproved + "; the approval does not transfer (ADR-093 D6): " + why, "", nil
}

// changesRequestingReviewers folds a chronological reviews listing to each
// reviewer's LATEST non-comment review (approvingReviewers' fold) and returns
// the logins whose latest review is CHANGES_REQUESTED, in first-seen order.
func changesRequestingReviewers(reviews []githubclient.PullRequestReview) []string {
	latest := map[string]string{}
	var order []string
	for _, rv := range reviews {
		switch rv.State {
		case "COMMENTED", "PENDING":
			continue
		}
		if rv.UserLogin == "" {
			continue
		}
		if _, seen := latest[rv.UserLogin]; !seen {
			order = append(order, rv.UserLogin)
		}
		latest[rv.UserLogin] = rv.State
	}
	var out []string
	for _, login := range order {
		if latest[login] == "CHANGES_REQUESTED" {
			out = append(out, login)
		}
	}
	return out
}

// mergeQueueIntentRecord is the merge_queue_entered row that recorded a queue
// entry's merge intent, decoded.
type mergeQueueIntentRecord struct {
	Entry           *audit.Entry
	Sequence        int64
	BaseRef         string
	AnchoredHeadSHA string
	Verdict         string
}

// mergeQueueIntentRow reads the merge_queue_entered row anchor names: the row
// AT anchor.Sequence (that exact sequence, and its base_ref must equal
// anchor.BaseRef), or — for a legacy anchor with Sequence 0 — the NEWEST row
// naming anchor.BaseRef. found=false when no row matches; a read error is
// returned (fail closed).
func (s *Server) mergeQueueIntentRow(ctx context.Context, runID uuid.UUID, anchor mergeQueueIntentAnchor) (mergeQueueIntentRecord, bool, error) {
	if s.cfg.AuditRepo == nil {
		return mergeQueueIntentRecord{}, false, errors.New("no audit repository is wired")
	}
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryMergeQueueEntered)
	if err != nil {
		return mergeQueueIntentRecord{}, false, fmt.Errorf("list %s: %w", CategoryMergeQueueEntered, err)
	}
	var best mergeQueueIntentRecord
	found := false
	for _, e := range rows {
		if anchor.Sequence != 0 && e.Sequence != anchor.Sequence {
			continue
		}
		var p struct {
			BaseRef         string `json:"base_ref"`
			AnchoredHeadSHA string `json:"anchored_head_sha"`
			Verdict         string `json:"verdict"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || p.BaseRef != anchor.BaseRef {
			continue
		}
		if found && e.Sequence <= best.Sequence {
			continue
		}
		best = mergeQueueIntentRecord{Entry: e, Sequence: e.Sequence, BaseRef: p.BaseRef,
			AnchoredHeadSHA: p.AnchoredHeadSHA, Verdict: p.Verdict}
		found = true
	}
	return best, found, nil
}

// mergeQueueApprovalTransfers is ADR-093 D6: whether the review-gate approval
// recorded with the merge intent anchor names carries to headSHA. It returns
// the reason it does not ("" when it does) or a read error.
func (s *Server) mergeQueueApprovalTransfers(ctx context.Context, runRow *run.Run, headSHA string,
	anchor mergeQueueIntentAnchor) (bool, string, error) {
	intent, found, err := s.mergeQueueIntentRow(ctx, runRow.ID, anchor)
	if err != nil {
		return false, "", err
	}
	if !found {
		return false, "no merge_queue_entered row records the entry's merge intent", nil
	}
	if intent.AnchoredHeadSHA == "" || headSHA == intent.AnchoredHeadSHA {
		return false, "the head has not advanced since the merge intent, so no advance carries the approval", nil
	}

	type advanceRow struct {
		category string
		entry    *audit.Entry
	}
	var advances []advanceRow
	for _, cat := range []string{CategoryBranchRebased, CategoryConflictResolutionPushed} {
		rows, lerr := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, cat)
		if lerr != nil {
			return false, "", fmt.Errorf("list %s: %w", cat, lerr)
		}
		for _, e := range rows {
			if e.Sequence > intent.Sequence {
				advances = append(advances, advanceRow{category: cat, entry: e})
			}
		}
	}
	sort.Slice(advances, func(i, j int) bool { return advances[i].entry.Sequence < advances[j].entry.Sequence })

	carriers := map[string]bool{intent.AnchoredHeadSHA: true}
	for _, a := range advances {
		switch a.category {
		case CategoryBranchRebased:
			var p struct {
				PriorHeadSHA    string `json:"prior_head_sha"`
				NewHeadSHA      string `json:"new_head_sha"`
				MergeCommitSHA  string `json:"merge_commit_sha"`
				AlreadyUpToDate bool   `json:"already_up_to_date"`
			}
			if json.Unmarshal(a.entry.Payload, &p) != nil {
				// An undecodable advance carries nothing; a head reached
				// through it is not in the set (fail closed).
				continue
			}
			clean := !p.AlreadyUpToDate && p.MergeCommitSHA != "" && p.NewHeadSHA == p.MergeCommitSHA // ADR-093 D6 clean advance
			if clean && carriers[p.PriorHeadSHA] {
				carriers[p.NewHeadSHA] = true
			}
		case CategoryConflictResolutionPushed:
			var p struct {
				HeadSHA string `json:"head_sha"`
			}
			_ = json.Unmarshal(a.entry.Payload, &p)
			carriesApproval := false // ADR-093 D6: a conflict-resolution advance never carries the review-gate approval
			if carriesApproval {
				carriers[p.HeadSHA] = true
			} else {
				carriers = map[string]bool{}
			}
		}
	}
	if !carriers[headSHA] {
		return false, "the head " + headSHA + " was not reached from the intent's head " + intent.AnchoredHeadSHA +
			" by clean advances only", nil
	}
	return true, "", nil
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
func (s *Server) mergeQueueChecksLeg(ctx context.Context, runRow *run.Run, headSHA string, variant mergeQueueEligibilityVariant) (string, string, error) {
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
	nilSnapshot := snapshot == nil
	if nilSnapshot {
		// Judge the observed set (an authoritatively-empty required set).
		snapshot = &run.RequiredChecksSnapshot{}
	}
	v := mergeoutcome.ClassifyMergeCI(snapshot, checks, false, time.Now().UTC())
	if v.Conclusion == mergeoutcome.CIConclusionRed {
		if !nilSnapshot || variant == mergeQueueVariantEntry {
			return mergeQueueReasonChecksRed, "required check(s) failed at " + headSHA + ": " + strings.Join(v.FailedChecks, ", "), nil
		}
		// No required-checks snapshot (carried concern 1208e0e2): the red run
		// may be an optional check, so a queued entry is NOT GREEN, never red
		// — admission passes it, merge-on-pass holds it.
		if variant == mergeQueueVariantAdmission {
			return "", "", nil
		}
		return mergeQueueReasonChecksNotGreen,
			"check(s) failed at " + headSHA + " with no required-checks snapshot to say whether they are required: " +
				strings.Join(v.FailedChecks, ", "), nil
	}
	if variant == mergeQueueVariantAdmission {
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
