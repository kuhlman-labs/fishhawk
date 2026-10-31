package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// ADR-093 slice 1 (D1, E87.4 / #4295), extended by slice 2 (D3, E87.5 /
// #4296) and ADR-112 R3 (#4632): the merge-queue ELIGIBILITY predicate. It
// decides QUEUE ADMISSION itself, not only the merge: POST
// /v0/runs/{id}/merge enqueues only an eligible run (mergeQueueEligibility,
// the ENTRY variant), the ADR-092
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
//	    when the review stage declares an approval gate, the variant's FORGE
//	    sub-legs hold (see below). A successfully read spec whose workflow
//	    has no review stage, or whose review stage declares no approval
//	    gate, passes; an absent, unreadable or unparseable spec is
//	    UNDETERMINABLE (fail closed).
//	(b) acceptance merge-eligible and not stale — acceptanceGateState +
//	    acceptanceGateAdmitsMerge, and acceptanceStaleRefusal == nil.
//	(c) required checks at the head, the PR-head MERGE-GATE leg
//	    (docs/adr/112 A3.1, #4780) — the check-runs listing classified by
//	    mergeoutcome.ClassifyMergeGateCI against the run's required-checks
//	    snapshot (a nil snapshot judges the observed set): per check suite
//	    the LATEST attempt decides, completed or not; a completed latest
//	    attempt in the fail set or CANCELLED is red (checks_red, A3.1(a));
//	    any failing suite fails the name (no superseded- or cancelled-run
//	    tolerance); anything else that is not a pass is not green
//	    (checks_not_green, A3.1(b)). ZERO observed check runs is never green
//	    (approval condition C5). The entry and merge variants require GREEN.
//	    The MERGE variant answers EVERY checks-read failure (no GitHub client,
//	    no head, an unparseable repo, a listing error, a truncated listing)
//	    and zero runs as the determined checks_not_green, with a detail
//	    naming the cause, never undeterminable: A3.2 bounds them with the
//	    merge-gate window. The ENTRY variant keeps read failures and zero
//	    runs undeterminable (merge_run refuses them, A3.3 "Unchanged"). The
//	    admission variant refuses only an observed RED required check (a
//	    freshly advanced head is legitimately pending, and the verify pass
//	    is the gate there), so it neither requires green nor treats an empty
//	    listing as a refusal; its read failures stay undeterminable (an A3
//	    residual). With a NIL snapshot an observed red may be an optional
//	    check, so it is NOT GREEN rather than red for the variants that act
//	    on a queued entry: the admission variant passes (never dropped), the
//	    merge variant answers checks_not_green (A3.1(b)); the entry variant
//	    keeps checks_red.
//	(d) no open blocking concern — an open concern whose Provenance is
//	    concern.ProvenanceFirstOfficer ("first_officer") or whose Severity is
//	    planreview.SeverityHigh ("high"). Medium/low concerns stay advisory
//	    (ADR-027). No requirement-arbitration concern CATEGORY exists
//	    (requirement_arbitration is a page event, spec.PageEventRequirementArbitration,
//	    never a concern category), so leg (d) matches no category.
//
// Leg (a)'s FORGE sub-legs differ by variant (ADR-112, #4619 / #4632):
//
//   - ENTRY, and ADMISSION with NO intent anchor (the ADR-092 rebase-path
//     enqueue, an entry without merge intent): no reviewer's latest
//     non-comment review is CHANGES_REQUESTED (changes_requested) and a LIVE
//     forge approval exists by an approvals.members identity (any live
//     approval when members is empty), else not_approved. ApprovalBasis is
//     live when it passes.
//   - ADMISSION WITH an intent anchor (mergeQueueAdmissionEligibilityAt):
//     NO approval is evaluated — merge-on-pass's in-lock fold judges it and
//     waits, then yields, on its absence (R3). Only the R2 VETO is checked,
//     through the ONE forge-approval fold (forge_approval.go) at the PR head:
//     vetoed (a write-or-higher CHANGES_REQUESTED) is forge_approval_vetoed
//     naming the reviewer; a veto_undetermined decision or a snapshot
//     transport failure is undeterminable; every other fold result passes.
//     ApprovalBasis is "". (The ADR-093 D6 approval-transfer walk this
//     variant used to run is gone: R1's clean-advance chain inside the fold
//     is the surviving transfer rule.)
//   - MERGE: no forge review read at all, because merge-on-pass judges
//     approvals and vetoes through the in-lock fold. ApprovalBasis is "" and
//     the merge path takes the basis from the fold.
//
// The predicate NEVER writes. Every read error is undeterminable, which is
// NOT eligible: uncertainty never enqueues, admits or merges. The one
// exception is leg (c) on the merge variant, whose checks-read failures are
// the determined checks_not_green (still not eligible), so merge-on-pass
// never reports that leg undeterminable (docs/adr/112 A3.2).
//
// mergeGateLegsOf is the ONE mapping of a result's reason codes onto A3.1's
// retryable merge-gate legs; merge-on-pass, admission and the yield re-probe
// read it.

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
	// mergeQueueReasonForgeApprovalVetoed is the intent-bearing admission's R2
	// veto (ADR-112, #4632): the fold at the PR head answered vetoed. It is the
	// queue's forge_approval_vetoed reason, so an admission that fails on it
	// ALONE yields under that name (R3 row 2).
	mergeQueueReasonForgeApprovalVetoed = mergeCandidateQueueReasonForgeApprovalVetoed
)

// mergeQueueApprovalLive is the approval basis
// mergeQueueEligibilityResult.ApprovalBasis reports when leg (a) passed on a
// declared approval gate through a LIVE forge approval (the entry and
// anchor-less admission variants; the others evaluate no approval).
const mergeQueueApprovalLive = "live"

// mergeQueueEligibilityVariant selects the caller's leg-(a) forge sub-legs
// and leg-(c) rules.
type mergeQueueEligibilityVariant int

const (
	// mergeQueueVariantEntry is merge_run's enqueue arm: checks GREEN, an
	// observed red is checks_red.
	mergeQueueVariantEntry mergeQueueEligibilityVariant = iota
	// mergeQueueVariantAdmission is the pump's admission: only a red
	// required check refuses.
	mergeQueueVariantAdmission
	// mergeQueueVariantMerge is merge-on-pass: checks GREEN (a checks-read
	// failure or zero runs is checks_not_green, docs/adr/112 A3.2), and no
	// forge review read (the in-lock forge-approval fold judges approvals,
	// ADR-112).
	mergeQueueVariantMerge
)

// mergeQueueIntentAnchor names the merge_queue_entered row that recorded a
// queue entry's merge intent: its chain Sequence (0 for an entry whose intent
// predates the recorded sequence) and the entry's BaseRef. The zero value is
// NO anchor: the admission variant then keeps its live-approval requirement.
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
// mergeQueueApprovalLive when leg (a) passed on a declared approval gate
// through a live approval, "" otherwise (no gate declared, leg (a) did not
// pass, or the variant evaluates no approval).
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

// mergeGateLegs is a result's reason codes mapped onto docs/adr/112 A3.1's
// merge-gate legs. Red is checks_red (A3.1(a)); Pending is checks_not_green
// (A3.1(b)); Review is review_pending or review_coverage_incomplete
// (A3.1(c)); Veto is forge_approval_vetoed (ADR-112 R2); Other is any other
// determined reason code (never retryable: a blocking concern, a stale or
// ineligible acceptance, not_approved, changes_requested); Undeterminable is
// the result's (a leg's read failed).
type mergeGateLegs struct {
	Red            bool
	Pending        bool
	Review         bool
	Veto           bool
	Other          bool
	Undeterminable bool
}

// mergeGateLegsOf is the ONE place that maps reason codes onto A3.1's
// retryable set (docs/adr/112 A3.1); merge-on-pass, admission and the yield
// re-probe all read it.
func mergeGateLegsOf(r mergeQueueEligibilityResult) mergeGateLegs {
	l := mergeGateLegs{Undeterminable: r.Undeterminable}
	for _, x := range r.Reasons {
		switch x.Code {
		case mergeQueueReasonChecksRed:
			l.Red = true
		case mergeQueueReasonChecksNotGreen:
			l.Pending = true
		case mergeQueueReasonReviewPending, mergeQueueReasonReviewCoverageIncomplete:
			l.Review = true
		case mergeQueueReasonForgeApprovalVetoed:
			l.Veto = true
		case mergeQueueReasonUndeterminable:
			l.Undeterminable = true
		default:
			l.Other = true
		}
	}
	return l
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
// whose merge intent anchor is anchor (mergeQueueAnchorFor). With a non-zero
// anchor leg (a) evaluates NO approval and checks only the ADR-112 R2 veto
// through the fold at headSHA (forge_approval_vetoed; veto_undetermined is
// undeterminable); a zero anchor is the anchor-less admission variant.
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
	switch {
	case variant == mergeQueueVariantMerge:
		// ADR-112 variant restriction (#4619): merge-on-pass applies the ONE
		// forge-approval fold itself, in-lock (mergeOnPassLocked step 6b), so
		// the merge variant makes NO forge review read here.
		return "", "", "", nil
	case variant == mergeQueueVariantAdmission && !anchor.isZero():
		// ADR-112 R3 (#4632): approval is judged at merge-on-pass, which waits
		// then yields; intent-bearing admission checks only the R2 veto.
		code, detail, err := s.mergeQueueAdmissionVetoLeg(ctx, runRow, headSHA)
		return code, detail, "", err
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
	if len(members) == 0 {
		if len(approvers) > 0 {
			return "", "", mergeQueueApprovalLive, nil
		}
		return mergeQueueReasonNotApproved, "the pull request has no live forge approval", "", nil
	}
	forgeID := observationForgeID(runRow.InstallationRef)
	ids := make([]string, 0, len(approvers))
	for _, login := range approvers {
		ids = append(ids, identity.CanonicalMember(login, forgeID))
	}
	if listedIdentity(members, ids, forgeID) != "" {
		return "", "", mergeQueueApprovalLive, nil
	}
	return mergeQueueReasonNotApproved,
		"no live forge approval is by an approvals.members identity (" + strings.Join(members, ", ") + ")", "", nil
}

// mergeQueueAdmissionVetoLeg is leg (a)'s forge sub-leg for an intent-bearing
// admission (ADR-112 R2/R3, #4632): the ONE fold at headSHA, consulted for its
// veto only. vetoed answers forge_approval_vetoed with the fold's sanitised
// detail (it names the reviewer); a veto_undetermined decision or a snapshot
// transport failure is an error (undeterminable: the entry stays at its
// place); every other result — approved, absent, dismissed, review_required,
// any other undetermined reason — passes, because merge-on-pass re-judges the
// approval in-lock.
func (s *Server) mergeQueueAdmissionVetoLeg(ctx context.Context, runRow *run.Run, headSHA string) (code, detail string, err error) {
	d, ferr := s.forgeApprovalFold(ctx, runRow, headSHA)
	if ferr != nil {
		d = d.undetermined(forgeApprovalReasonSnapshotUnreadable, "the forge review snapshot could not be read", ferr)
		return "", "", errors.New(d.Detail)
	}
	switch {
	case d.Result == forgeApprovalVetoed:
		return mergeQueueReasonForgeApprovalVetoed, d.Detail, nil
	case d.Result == forgeApprovalUndetermined && d.Reason == forgeApprovalReasonVetoUndetermined:
		return "", "", errors.New(d.Detail)
	}
	return "", "", nil
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
// entry's merge intent, decoded (merge-on-pass reads its verdict).
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

// mergeQueueChecksLeg is leg (c), the PR-head merge-gate leg (docs/adr/112
// A3.1): ClassifyMergeGateCI for every variant. On the MERGE variant every
// checks-read failure and zero runs answer the determined checks_not_green
// (A3.2's bound), never an error; the entry and admission variants keep them
// undeterminable (an error), and admission passes zero runs.
func (s *Server) mergeQueueChecksLeg(ctx context.Context, runRow *run.Run, headSHA string, variant mergeQueueEligibilityVariant) (string, string, error) {
	checks, err := s.listMergeGateChecks(ctx, runRow, headSHA)
	if err != nil {
		if variant == mergeQueueVariantMerge {
			return mergeQueueReasonChecksNotGreen, "required checks could not be read at " + headSHA + ": " + err.Error(), nil
		}
		return "", "", err
	}
	snapshot := runRow.RequiredChecksSnapshot
	nilSnapshot := snapshot == nil
	if nilSnapshot {
		// Judge the observed set (an authoritatively-empty required set).
		snapshot = &run.RequiredChecksSnapshot{}
	}
	v := mergeoutcome.ClassifyMergeGateCI(snapshot, checks, false, time.Now().UTC())
	if v.Conclusion == mergeoutcome.CIConclusionRed {
		if !nilSnapshot || variant == mergeQueueVariantEntry {
			return mergeQueueReasonChecksRed, "required check(s) failed or were cancelled at " + headSHA + ": " + strings.Join(v.FailedChecks, ", "), nil
		}
		// No required-checks snapshot (carried concern 1208e0e2): the red run
		// may be an optional check, so a queued entry is NOT GREEN, never red
		// — admission passes it, merge-on-pass holds it.
		if variant == mergeQueueVariantAdmission {
			return "", "", nil
		}
		return mergeQueueReasonChecksNotGreen,
			"check(s) failed or were cancelled at " + headSHA + " with no required-checks snapshot to say whether they are required: " +
				strings.Join(v.FailedChecks, ", "), nil
	}
	if variant == mergeQueueVariantAdmission {
		return "", "", nil
	}
	if len(checks) == 0 {
		// Approval condition C5: zero observed check runs is never green. The
		// merge variant holds it as pending (A3.1(b)); the entry variant
		// refuses it as undeterminable.
		if variant == mergeQueueVariantMerge {
			return mergeQueueReasonChecksNotGreen, "no check runs are reported at " + headSHA + " yet", nil
		}
		return "", "", errors.New("no check runs are reported at " + headSHA)
	}
	if v.Conclusion != mergeoutcome.CIConclusionGreen {
		return mergeQueueReasonChecksNotGreen,
			"required check(s) not green at " + headSHA + " (" + v.MissingReason + "): " + strings.Join(v.MissingChecks, ", "), nil
	}
	return "", "", nil
}

// listMergeGateChecks reads the check-runs listing at headSHA. A missing
// client, head or parseable repo, a listing error and a truncated listing are
// each an error naming the cause.
func (s *Server) listMergeGateChecks(ctx context.Context, runRow *run.Run, headSHA string) ([]githubclient.CheckRunSummary, error) {
	if s.cfg.GitHub == nil {
		return nil, errors.New("no GitHub client is wired")
	}
	if headSHA == "" {
		return nil, errors.New("no head sha to read checks at")
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return nil, err
	}
	checks, truncated, err := s.cfg.GitHub.ListCheckRunsForRef(ctx, mergeObservationScope(runRow), repo, headSHA)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, errors.New("the check-runs listing was truncated")
	}
	return checks, nil
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
