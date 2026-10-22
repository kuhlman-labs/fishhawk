package server

// ADR-112 (#4622, ratified 2026-10-22, v4.1) R1, R2, R4 and R6: ONE
// forge-approval predicate — the FOLD — for every basis (live and
// transferred) and every merge path (queue merge-on-pass, inline merge_run,
// delegated may_merge). This file is the predicate, its provider seam, its
// evidence writer (merge_queue_authorized) and its wire contract
// (forge_approval_required / merge_authorization_unrecorded); the merge paths
// call it in-lock (slice 2 of #4619).
//
// A forge review QUALIFIES only if:
//
//	(a) it is the reviewer's latest opinionated APPROVED review at the
//	    verified head, or at a head reaching it only through
//	    Fishhawk-recorded CLEAN advances (cleanChainReaches, seeded at the
//	    review's commit);
//	(b) its author satisfies the review gate's approvals predicate with the
//	    fired escalations (members, member_of), the `not: author`
//	    exclusions and exactly R1(b)'s single-member voucher exception;
//	(c) its author's permission, read FRESH with the run's installation
//	    token, is at least max(min_permission, write);
//	(d) its author is a real user (GraphQL User, non-zero databaseId, no
//	    `[bot]` suffix, not `ghost`).
//
// A CHANGES_REQUESTED vetoes only from an identity-valid author at or above
// the same floor (R2); an unresolvable veto is undetermined. Every Detail
// leaves the fold already sanitised (sanitizeForgeReadDetail).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/escalation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// CategoryMergeQueueAuthorized is the ADR-112 R4 evidence row: the fold's
// qualifying approvals and the policy they were judged under, appended
// (chained) in-lock BEFORE any merge verdict row or dispatch claim. INTERNAL:
// not an issue-comment surface.
const CategoryMergeQueueAuthorized = "merge_queue_authorized"

// The ADR-112 wire contract on POST /v0/runs/{run_id}/merge.
const (
	// mergeCodeForgeApprovalRequired (409): the fold did not answer approved
	// at the verified head. details.fold / reason / next_step say why and
	// what clears it. Nothing is recorded.
	mergeCodeForgeApprovalRequired = "forge_approval_required"
	// mergeCodeAuthorizationUnrecorded (503, retryable): the fold approved
	// but the merge_queue_authorized append failed, so nothing else was
	// recorded and the merge was not dispatched.
	mergeCodeAuthorizationUnrecorded = "merge_authorization_unrecorded"
)

// Fold bounds (ADR-112).
const (
	// forgeApprovalMaxPermissionReads caps the distinct reviewers whose
	// permission one decision reads; needing more is undetermined.
	forgeApprovalMaxPermissionReads = 10
	// forgeApprovalFoldTimeout is the ONE sub-deadline the snapshot and every
	// permission and membership read share.
	forgeApprovalFoldTimeout = 15 * time.Second
	// forgeApprovalDetailMaxBytes bounds every forge-read detail the fold
	// emits (held reasons, logs, audit rows, API bodies).
	forgeApprovalDetailMaxBytes = 300
)

// Fold results.
const (
	forgeApprovalApproved            = "approved"
	forgeApprovalVetoed              = "vetoed"
	forgeApprovalReviewRequired      = "review_required"
	forgeApprovalDismissed           = "dismissed"
	forgeApprovalAbsent              = "absent"
	forgeApprovalUndetermined        = "undetermined"
	forgeApprovalReviewsTruncated    = "reviews_truncated"
	forgeApprovalProviderUnsupported = "provider_unsupported"
)

// Approval bases.
const (
	forgeApprovalBasisLive        = "live"
	forgeApprovalBasisTransferred = "transferred"
	forgeApprovalBasisNotRequired = "not_required"
)

// Next steps a non-approved decision names.
const (
	forgeApprovalNextApprovePR                 = "approve_pr"
	forgeApprovalNextDismissReview             = "dismiss_review"
	forgeApprovalNextRetryAfter                = "retry_after"
	forgeApprovalNextConfigureIdentityProvider = "configure_identity_provider"
)

// Machine reasons. head_moved is the ADR-112 H1 head binding: the in-lock
// snapshot's headRefOid is not the head the fold evaluates (merge-on-pass
// readvances on it; inline and delegated refuse).
const (
	forgeApprovalReasonHeadMoved           = "head_moved"
	forgeApprovalReasonHeadUnknown         = "head_unknown"
	forgeApprovalReasonMerged              = "pull_request_merged"
	forgeApprovalReasonSpecUndeterminable  = "spec_undeterminable"
	forgeApprovalReasonForgeUnsupported    = "forge_unsupported"
	forgeApprovalReasonNoIdentityProvider  = "member_of_no_identity_provider"
	forgeApprovalReasonEscalations         = "escalations_unresolvable"
	forgeApprovalReasonRunUnaddressable    = "run_unaddressable"
	forgeApprovalReasonAuditUnreadable     = "audit_unreadable"
	forgeApprovalReasonTruncated           = "reviews_truncated"
	forgeApprovalReasonVeto                = "changes_requested"
	forgeApprovalReasonVetoUndetermined    = "veto_undetermined"
	forgeApprovalReasonApprovalUndetermine = "approval_undetermined"
	forgeApprovalReasonPermissionCap       = "permission_read_cap"
	forgeApprovalReasonReviewRequired      = "review_required"
	forgeApprovalReasonDismissed           = "approval_dismissed"
	forgeApprovalReasonNoQualifying        = "no_qualifying_approval"
	forgeApprovalReasonQualified           = "qualified"
	forgeApprovalReasonNotRequired         = "no_gate_declared"
)

// forgeApprovalProviderGitHub is the provider every counted approval names.
const forgeApprovalProviderGitHub = "github"

// countedApproval is one qualifying approval as merge_queue_authorized
// records it (R4).
type countedApproval struct {
	ReviewID    int64     `json:"review_id"`
	Commit      string    `json:"commit"`
	SubmittedAt time.Time `json:"submitted_at"`
	Provider    string    `json:"provider"`
	ActorID     int64     `json:"actor_id"`
	Login       string    `json:"login"`
	Typename    string    `json:"typename"`
	Permission  string    `json:"permission"`
}

// forgeApprovalDecision is the fold's answer for one verified head.
type forgeApprovalDecision struct {
	// Declared is false when the run's review stage declares no approval
	// gate (R6): no forge approval is needed and no evidence row is written.
	Declared bool
	Result   string
	Reason   string
	// Detail is human prose, ALREADY sanitised and bounded.
	Detail   string
	NextStep string
	Basis    string
	Counted  []countedApproval
	// ChainHeads is the clean-advance chain a transferred approval rode
	// (review commit first, verified head last); nil for a live basis.
	ChainHeads       []string
	VoucherException bool
	ReviewDecision   string
	MergeStateStatus string
	// VerifiedHead is the head the fold evaluated; ObservedHead is the
	// in-lock snapshot's headRefOid.
	VerifiedHead string
	ObservedHead string
	ObservedAt   time.Time
	// Merged is true when the snapshot reported the pull request merged; the
	// fold then answers undetermined and a caller routes to its observe path.
	Merged bool
	// VetoReviewer names the write-or-higher CHANGES_REQUESTED author on a
	// vetoed decision (the dismiss_review next step names them).
	VetoReviewer string

	// Policy the decision was judged under (merge_queue_authorized).
	// escalated is any raise; raisedCountOrGroups is the narrower R1(b)
	// condition that withdraws the voucher exception.
	approvals           escalatedApprovals
	escalated           bool
	raisedCountOrGroups bool
	firedRuleKeys       []string
	specSHA256          string
}

// approved reports whether the decision admits a merge: a declared gate whose
// fold answered approved, or no gate at all.
func (d forgeApprovalDecision) approved() bool {
	return !d.Declared || d.Result == forgeApprovalApproved
}

// forgeApprovalReader is the provider-neutral seam the fold reads through.
// Exactly one adapter exists (GitHub); any other forge answers
// provider_unsupported BY NAME and is never a fallback (ADR-110 rule 3(c),
// ADR-093 D9, ADR-112 R6).
type forgeApprovalReader interface {
	ReviewSnapshot(ctx context.Context) (*githubclient.ReviewSnapshot, error)
	Permission(ctx context.Context, login string) (githubclient.RepoPermission, error)
}

// githubForgeApprovalReader is the GitHub adapter: the run's installation
// token, one repository, one pull request.
type githubForgeApprovalReader struct {
	client *githubclient.Client
	scope  forge.CredentialScope
	repo   forge.RepoRef
	number int
}

func (g githubForgeApprovalReader) ReviewSnapshot(ctx context.Context) (*githubclient.ReviewSnapshot, error) {
	return g.client.PullRequestReviewSnapshot(ctx, g.scope, g.repo, g.number)
}

func (g githubForgeApprovalReader) Permission(ctx context.Context, login string) (githubclient.RepoPermission, error) {
	return g.client.CollaboratorPermission(ctx, g.scope, g.repo, login)
}

// forgeApprovalGate is a resolved review approval gate.
type forgeApprovalGate struct {
	approvals *spec.Approvals
	workflow  *spec.Workflow
}

// errForgeApprovalSpecEmpty is a run that RECORDED a spec whose cached bytes
// carry no document (whitespace only).
var errForgeApprovalSpecEmpty = errors.New("the run's recorded workflow spec is empty")

// forgeApprovalGateFor resolves whether the run's review stage declares an
// approval gate (ADR-112 R6, approval condition C4):
//
//   - the run NEVER recorded a spec (no cached bytes: a legacy or ad-hoc
//     run) → not declared, no forge approval needed;
//   - a spec was recorded but its cached bytes read back EMPTY, do not parse,
//     or do not carry the run's workflow → an error, which every path treats
//     as undetermined (refuse inline/delegated; eject on the queue);
//   - a parsed workflow with no review stage, or a review stage with no
//     approval gate → not declared.
//
// This deliberately DIVERGES from the queue's entry and admission variants
// (reviewApprovalGateForRun), which hold on a missing cached spec: the fold
// admits a run that never recorded one.
func forgeApprovalGateFor(runRow *run.Run) (declared bool, gate forgeApprovalGate, err error) {
	if runRow == nil || len(runRow.WorkflowSpec) == 0 {
		return false, forgeApprovalGate{}, nil
	}
	if strings.TrimSpace(string(runRow.WorkflowSpec)) == "" {
		return false, forgeApprovalGate{}, errForgeApprovalSpecEmpty
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		return false, forgeApprovalGate{}, fmt.Errorf("parse workflow spec: %w", err)
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return false, forgeApprovalGate{}, fmt.Errorf("workflow %q not in spec", runRow.WorkflowID)
	}
	for _, stg := range wf.Stages {
		if stg.Type != spec.StageTypeReview {
			continue
		}
		for _, g := range stg.Gates {
			if g.Type == spec.GateTypeApproval {
				return true, forgeApprovalGate{approvals: g.Approvals, workflow: &wf}, nil
			}
		}
		return false, forgeApprovalGate{}, nil
	}
	return false, forgeApprovalGate{}, nil
}

// forgeApprovalReaderFor selects the provider adapter for the run. ok=false
// with unsupported=true is a non-GitHub forge (provider_unsupported by name);
// ok=false with a reason is an unaddressable GitHub run (undetermined).
func (s *Server) forgeApprovalReaderFor(runRow *run.Run) (r forgeApprovalReader, unsupported bool, why string) {
	if runForge(runRow) != forgeApprovalProviderGitHub {
		return nil, true, ""
	}
	if s.cfg.GitHub == nil {
		return nil, false, "no GitHub client is wired"
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return nil, false, "the run carries no GitHub installation"
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return nil, false, "the run repo is unparseable"
	}
	n := parsePRNumberFromURL(runRow.PullRequestURL)
	if n <= 0 {
		return nil, false, "the run carries no parseable pull request number"
	}
	return githubForgeApprovalReader{client: s.cfg.GitHub, scope: forge.FromGitHubInstallationID(*runRow.InstallationID),
		repo: repo, number: n}, false, ""
}

// forgeApprovalFold is THE ADR-112 predicate: ONE review snapshot plus at
// most forgeApprovalMaxPermissionReads permission reads under one
// forgeApprovalFoldTimeout, judged at verifiedHead. The error is ONLY a
// snapshot transport failure (a caller holds or refuses on it); every other
// failure is a decision (undetermined). A run whose review stage declares no
// approval gate answers {Declared:false, Basis:not_required} with no forge
// read.
func (s *Server) forgeApprovalFold(ctx context.Context, runRow *run.Run, verifiedHead string) (forgeApprovalDecision, error) {
	d := forgeApprovalDecision{VerifiedHead: verifiedHead, Basis: forgeApprovalBasisNotRequired}
	declared, gate, err := forgeApprovalGateFor(runRow)
	if err != nil {
		d.Declared = true
		return d.undetermined(forgeApprovalReasonSpecUndeterminable, "the run's recorded workflow spec could not be read", err), nil
	}
	if !declared {
		d.Result, d.Reason = forgeApprovalApproved, forgeApprovalReasonNotRequired
		return d, nil
	}
	d.Declared = true
	sum := sha256.Sum256(runRow.WorkflowSpec)
	d.specSHA256 = hex.EncodeToString(sum[:])

	reader, unsupported, why := s.forgeApprovalReaderFor(runRow)
	if unsupported {
		d.Result, d.Reason = forgeApprovalProviderUnsupported, forgeApprovalReasonForgeUnsupported
		d.Detail = sanitizeForgeReadDetail("provider_unsupported", "the "+runForge(runRow)+
			" forge has no approval-to-revision binding, so a qualifying approval at the verified head cannot be determined (ADR-112 R6)")
		return d, nil
	}

	// Escalations and the effective requirement (R1(b)), before any forge read.
	var escRes escalation.Result
	if len(gate.workflow.Escalations) > 0 {
		stage, serr := s.forgeApprovalReviewStage(ctx, runRow.ID)
		if serr != nil {
			return d.undetermined(forgeApprovalReasonEscalations, "the review stage row could not be resolved", serr), nil
		}
		escRes, err = s.resolveEscalations(ctx, runRow, gate.workflow, stage.ID)
		if err != nil {
			return d.undetermined(forgeApprovalReasonEscalations, "the workflow's escalations could not be resolved", err), nil
		}
	}
	d.approvals = effectiveApprovals(gate.approvals, escRes.Requirements)
	baseCount, baseGroups := 1, 0
	if gate.approvals != nil {
		if gate.approvals.Count != nil {
			baseCount = *gate.approvals.Count
		}
		if gate.approvals.MemberOf != "" {
			baseGroups = 1
		}
	}
	d.raisedCountOrGroups = d.approvals.count != baseCount || len(d.approvals.memberOf) != baseGroups
	d.escalated = d.raisedCountOrGroups || d.approvals.minPermission != approvalsMinPermission(gate.approvals)
	for _, f := range escRes.Fired {
		d.firedRuleKeys = append(d.firedRuleKeys, escalation.RuleKey(f.Escalation))
	}
	var idp identity.IdentityProvider
	if len(d.approvals.memberOf) > 0 {
		p, ok := s.predicateIdentityProvider(forgeApprovalProviderGitHub)
		if !ok {
			d.Result, d.Reason, d.NextStep = forgeApprovalProviderUnsupported, forgeApprovalReasonNoIdentityProvider,
				forgeApprovalNextConfigureIdentityProvider
			d.Detail = sanitizeForgeReadDetail("provider_unsupported",
				"the review gate requires member_of but no github identity provider is configured to resolve membership")
			return d, nil
		}
		idp = p
	}

	if verifiedHead == "" {
		return d.undetermined(forgeApprovalReasonHeadUnknown, "no verified head was supplied to judge approvals at", nil), nil
	}
	if reader == nil {
		return d.undetermined(forgeApprovalReasonRunUnaddressable, why, nil), nil
	}

	foldCtx, cancel := context.WithTimeout(ctx, forgeApprovalFoldTimeout)
	defer cancel()
	snap, err := reader.ReviewSnapshot(foldCtx)
	if err != nil {
		return d, fmt.Errorf("forge approval: review snapshot: %w", err)
	}
	d.ObservedAt = time.Now().UTC()
	d.ObservedHead, d.ReviewDecision, d.MergeStateStatus = snap.HeadOID, snap.ReviewDecision, snap.MergeStateStatus
	if snap.Merged {
		d.Merged = true
		return d.undetermined(forgeApprovalReasonMerged, "the pull request is already merged", nil), nil
	}
	if snap.HeadOID != verifiedHead {
		return d.undetermined(forgeApprovalReasonHeadMoved, fmt.Sprintf("the pull request head is %s, not the verified head %s",
			shaOrUnrecorded(snap.HeadOID), verifiedHead), nil), nil
	}
	if snap.Truncated {
		d.Result, d.Reason, d.NextStep = forgeApprovalReviewsTruncated, forgeApprovalReasonTruncated, forgeApprovalNextRetryAfter
		d.Detail = sanitizeForgeReadDetail("reviews_truncated", "the reviewer-latest review listing exceeded the snapshot page cap")
		return d, nil
	}
	return s.foldSnapshot(foldCtx, runRow, d, snap, reader, idp, gate.approvals)
}

// approvalsMinPermission is the gate's declared min_permission ("" for none).
func approvalsMinPermission(a *spec.Approvals) string {
	if a == nil {
		return ""
	}
	return a.MinPermission
}

// forgeApprovalReviewStage returns the run's review stage row; a missing row
// or a read error is an error (undetermined).
func (s *Server) forgeApprovalReviewStage(ctx context.Context, runID uuid.UUID) (*run.Stage, error) {
	if s.cfg.RunRepo == nil {
		return nil, errors.New("no run repository is wired")
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	var out *run.Stage
	for _, st := range stages {
		if st.Type == run.StageTypeReview && (out == nil || st.Sequence > out.Sequence) {
			out = st
		}
	}
	if out == nil {
		return nil, errors.New("the run has no review stage row")
	}
	return out, nil
}

// latestOpinion is one reviewer's re-folded latest opinionated review.
type latestOpinion struct {
	key    string
	review githubclient.SnapshotReview
}

// refoldLatestOpinions re-folds the snapshot to each reviewer's latest
// opinionated review, keyed by the immutable author databaseId (a non-User or
// null author is keyed by login or review id, and can never qualify), ordered
// by submittedAt then review databaseId. COMMENTED and PENDING are ignored.
func refoldLatestOpinions(reviews []githubclient.SnapshotReview) []latestOpinion {
	latest := map[string]githubclient.SnapshotReview{}
	for _, rv := range reviews {
		switch rv.State {
		case "COMMENTED", "PENDING":
			continue
		}
		var key string
		switch {
		case rv.Author.Present && rv.Author.Typename == "User" && rv.Author.DatabaseID != 0:
			key = "id:" + strconv.FormatInt(rv.Author.DatabaseID, 10)
		case rv.Author.Present && rv.Author.Login != "":
			key = "login:" + rv.Author.Login
		default:
			key = "review:" + strconv.FormatInt(rv.DatabaseID, 10)
		}
		prev, seen := latest[key]
		if !seen || rv.SubmittedAt.After(prev.SubmittedAt) ||
			(rv.SubmittedAt.Equal(prev.SubmittedAt) && rv.DatabaseID > prev.DatabaseID) {
			latest[key] = rv
		}
	}
	out := make([]latestOpinion, 0, len(latest))
	for k, rv := range latest {
		out = append(out, latestOpinion{key: k, review: rv})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// forgeReviewIdentityValid is R1(d): only a GraphQL User with a non-empty
// login, a non-zero databaseId, no `[bot]` suffix and a login other than
// `ghost`. A null author never qualifies.
func forgeReviewIdentityValid(a githubclient.ReviewActor) bool {
	return a.Present && a.Typename == "User" && a.Login != "" && a.DatabaseID != 0 &&
		!strings.HasSuffix(strings.ToLower(a.Login), "[bot]") && !strings.EqualFold(a.Login, "ghost")
}

// forgePermissionVerdict classifies one permission read against the floor.
type forgePermissionVerdict int

const (
	forgePermissionBelow forgePermissionVerdict = iota
	forgePermissionMeets
	forgePermissionUndetermined
)

// forgeApprovalFloor is R1(c): max(declared min_permission, write).
func forgeApprovalFloor(minPermission string) identity.Permission {
	if permissionAtLeast(minPermission, string(identity.PermissionWrite)) {
		return identity.Permission(minPermission)
	}
	return identity.PermissionWrite
}

// classifyForgePermission is R1(c) over one permission read: a read error
// (404 included) is undetermined, as is `none` on a private repository and an
// unrecognised role; `none` on a public repository, read and triage are below
// the write floor.
func classifyForgePermission(perm githubclient.RepoPermission, err error, isPrivate bool, floor identity.Permission) (forgePermissionVerdict, string) {
	if err != nil {
		return forgePermissionUndetermined, ""
	}
	role := perm.EffectiveRole()
	p, ok := identity.ParsePermission(role)
	if !ok {
		return forgePermissionUndetermined, role
	}
	if p == identity.PermissionNone && isPrivate {
		return forgePermissionUndetermined, role
	}
	if p.AtLeast(floor) {
		return forgePermissionMeets, role
	}
	return forgePermissionBelow, role
}

// forgeApprovalExclusions is the `not: author` exclusion set (canonical
// subjects) and the recorded vouchers.
type forgeApprovalExclusions struct {
	excludeAuthor bool
	opener        string
	vouchers      map[string]bool
	changeAuthor  string
}

// excluded reports whether subject is excluded, and whether ONLY the
// voucher/change-author legs (both derived from operator_commit_vouched rows)
// exclude it — the legs R1(b)'s voucher exception may lift.
func (x forgeApprovalExclusions) excluded(subject string) (excluded, voucherOnly bool) {
	if !x.excludeAuthor {
		return false, false
	}
	if x.opener != "" && subject == x.opener {
		return true, false
	}
	if x.vouchers[subject] || (x.changeAuthor != "" && subject == x.changeAuthor) {
		return true, true
	}
	return false, false
}

// forgeApprovalExclusionsFor builds the exclusion set from the snapshot's PR
// opener and the run's operator_commit_vouched rows (one read).
func (s *Server) forgeApprovalExclusionsFor(ctx context.Context, runRow *run.Run, snap *githubclient.ReviewSnapshot, a *spec.Approvals) (forgeApprovalExclusions, error) {
	x := forgeApprovalExclusions{excludeAuthor: approvalsExcludeAuthor(a), vouchers: map[string]bool{}}
	if snap.Author.Present && snap.Author.Typename == "User" && snap.Author.Login != "" {
		x.opener = identity.CanonicalMember(snap.Author.Login, forgeApprovalProviderGitHub)
	}
	if s.cfg.AuditRepo == nil {
		return x, errors.New("no audit repository is wired")
	}
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, CategoryOperatorCommitVouched)
	if err != nil {
		return x, fmt.Errorf("list %s: %w", CategoryOperatorCommitVouched, err)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Sequence < rows[j].Sequence })
	for _, e := range rows {
		if e.ActorKind == nil || *e.ActorKind != audit.ActorUser || e.ActorSubject == nil || *e.ActorSubject == "" {
			continue
		}
		subj := identity.CanonicalMember(*e.ActorSubject, forgeApprovalProviderGitHub)
		x.vouchers[subj] = true
		if x.changeAuthor == "" {
			x.changeAuthor = subj
		}
	}
	return x, nil
}

// voucherExceptionApplies is exactly R1(b)'s single-member voucher
// exception: the effective members canonicalise to exactly {subject}, subject
// is a recorded voucher, the effective count is 1 and no escalation raised
// the requirement. Never for a gate without members.
func voucherExceptionApplies(eff escalatedApprovals, raisedCountOrGroups bool, x forgeApprovalExclusions, subject string) bool {
	if raisedCountOrGroups || eff.count != 1 || len(eff.members) == 0 || !x.vouchers[subject] {
		return false
	}
	for _, m := range eff.members {
		if identity.CanonicalMember(m, forgeApprovalProviderGitHub) != subject {
			return false
		}
	}
	return true
}

// foldSnapshot judges one snapshot (already at the verified head, complete).
func (s *Server) foldSnapshot(ctx context.Context, runRow *run.Run, d forgeApprovalDecision, snap *githubclient.ReviewSnapshot,
	reader forgeApprovalReader, idp identity.IdentityProvider, gateApprovals *spec.Approvals) (forgeApprovalDecision, error) {
	head := d.VerifiedHead
	opinions := refoldLatestOpinions(snap.Reviews)

	var chainRows []*audit.Entry
	chainLoaded := false
	loadChain := func() error {
		if chainLoaded {
			return nil
		}
		if s.cfg.AuditRepo == nil {
			return errors.New("no audit repository is wired")
		}
		for _, cat := range []string{CategoryBranchRebased, CategoryConflictResolutionPushed} {
			rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, cat)
			if err != nil {
				return fmt.Errorf("list %s: %w", cat, err)
			}
			chainRows = append(chainRows, rows...)
		}
		chainLoaded = true
		return nil
	}

	type candidate struct {
		op     latestOpinion
		chain  []string
		isVeto bool
	}
	var candidates []candidate
	sawDismissed := false
	for _, op := range opinions {
		rv := op.review
		switch rv.State {
		case "DISMISSED":
			sawDismissed = true
		case "CHANGES_REQUESTED":
			if forgeReviewIdentityValid(rv.Author) {
				candidates = append(candidates, candidate{op: op, isVeto: true})
			}
		case "APPROVED":
			if !forgeReviewIdentityValid(rv.Author) || rv.CommitOID == "" {
				continue
			}
			if rv.CommitOID == head {
				candidates = append(candidates, candidate{op: op, chain: []string{head}})
				continue
			}
			if err := loadChain(); err != nil {
				return d.undetermined(forgeApprovalReasonAuditUnreadable, "the run's advance rows could not be read", err), nil
			}
			if ok, heads := cleanChainReaches(chainRows, rv.CommitOID, head); ok {
				candidates = append(candidates, candidate{op: op, chain: heads})
			}
		}
	}
	if len(candidates) > forgeApprovalMaxPermissionReads {
		d.NextStep = forgeApprovalNextRetryAfter
		return d.undetermined(forgeApprovalReasonPermissionCap, fmt.Sprintf("%d candidate reviewers exceed the %d permission-read cap",
			len(candidates), forgeApprovalMaxPermissionReads), nil), nil
	}

	floor := forgeApprovalFloor(d.approvals.minPermission)
	x, err := s.forgeApprovalExclusionsFor(ctx, runRow, snap, gateApprovals)
	if err != nil {
		return d.undetermined(forgeApprovalReasonAuditUnreadable, "the run's vouch rows could not be read", err), nil
	}

	var (
		vetoer, vetoUndetermined, approvalUndetermined string
		counted                                        []countedApproval
		chains                                         [][]string
	)
	for _, c := range candidates {
		rv := c.op.review
		perm, perr := reader.Permission(ctx, rv.Author.Login)
		verdict, role := classifyForgePermission(perm, perr, snap.IsPrivate, floor)
		why := ""
		if perr != nil {
			why = perr.Error()
		}
		if c.isVeto {
			switch verdict {
			case forgePermissionMeets:
				if vetoer == "" {
					vetoer = rv.Author.Login
				}
			case forgePermissionUndetermined:
				if vetoUndetermined == "" {
					vetoUndetermined = rv.Author.Login + ": " + nonEmpty(why, "permission "+nonEmpty(role, "unresolved"))
				}
			}
			continue
		}
		switch verdict {
		case forgePermissionUndetermined:
			if approvalUndetermined == "" {
				approvalUndetermined = rv.Author.Login + ": " + nonEmpty(why, "permission "+nonEmpty(role, "unresolved"))
			}
			continue
		case forgePermissionBelow:
			continue
		}
		subject := identity.CanonicalMember(rv.Author.Login, forgeApprovalProviderGitHub)
		exception := false
		if excluded, voucherOnly := x.excluded(subject); excluded {
			if !voucherOnly || !voucherExceptionApplies(d.approvals, d.raisedCountOrGroups, x, subject) {
				continue
			}
			exception = true
		}
		if len(d.approvals.members) > 0 && !identity.SubjectListed(d.approvals.members, subject, forgeApprovalProviderGitHub) {
			continue
		}
		member, merr := forgeApprovalMemberOf(ctx, idp, d.approvals.memberOf, subject)
		if merr != nil {
			if approvalUndetermined == "" {
				approvalUndetermined = rv.Author.Login + ": membership: " + merr.Error()
			}
			continue
		}
		if !member {
			continue
		}
		if exception {
			d.VoucherException = true
		}
		counted = append(counted, countedApproval{ReviewID: rv.DatabaseID, Commit: rv.CommitOID, SubmittedAt: rv.SubmittedAt,
			Provider: forgeApprovalProviderGitHub, ActorID: rv.Author.DatabaseID, Login: rv.Author.Login,
			Typename: rv.Author.Typename, Permission: role})
		chains = append(chains, c.chain)
	}

	switch {
	case vetoer != "":
		d.Result, d.Reason, d.NextStep, d.VetoReviewer = forgeApprovalVetoed, forgeApprovalReasonVeto, forgeApprovalNextDismissReview, vetoer
		d.Detail = sanitizeForgeReadDetail("vetoed", "reviewer "+vetoer+" (write or higher) requested changes on the pull request")
		return d, nil
	case vetoUndetermined != "":
		d.NextStep = forgeApprovalNextRetryAfter
		return d.undetermined(forgeApprovalReasonVetoUndetermined, "a CHANGES_REQUESTED author's permission could not be resolved ("+vetoUndetermined+")", nil), nil
	case len(counted) > 0:
		d.Counted = counted
		d.Basis = forgeApprovalBasisTransferred
		for i, ch := range chains {
			if counted[i].Commit == head {
				d.Basis, d.ChainHeads = forgeApprovalBasisLive, nil
				break
			}
			if d.ChainHeads == nil {
				d.ChainHeads = ch
			}
		}
		if snap.ReviewDecision == "REVIEW_REQUIRED" {
			d.Result, d.Reason, d.NextStep = forgeApprovalReviewRequired, forgeApprovalReasonReviewRequired, forgeApprovalNextApprovePR
			d.Detail = sanitizeForgeReadDetail("review_required", "GitHub reports reviewDecision REVIEW_REQUIRED despite a qualifying approval")
			return d, nil
		}
		d.Result, d.Reason = forgeApprovalApproved, forgeApprovalReasonQualified
		return d, nil
	case approvalUndetermined != "":
		d.NextStep = forgeApprovalNextRetryAfter
		return d.undetermined(forgeApprovalReasonApprovalUndetermine, "an approval candidate could not be judged ("+approvalUndetermined+")", nil), nil
	case sawDismissed:
		d.Result, d.Reason, d.NextStep = forgeApprovalDismissed, forgeApprovalReasonDismissed, forgeApprovalNextApprovePR
		d.Detail = sanitizeForgeReadDetail("dismissed", "no qualifying approval is live and a prior review was dismissed")
		return d, nil
	default:
		d.Result, d.Reason, d.NextStep = forgeApprovalAbsent, forgeApprovalReasonNoQualifying, forgeApprovalNextApprovePR
		d.Detail = sanitizeForgeReadDetail("absent", "no qualifying forge approval (a write-or-higher human reviewer satisfying the review gate) covers the verified head")
		return d, nil
	}
}

// forgeApprovalMemberOf resolves the member_of CONJUNCTION for subject: every
// group must hold. No groups is trivially true.
func forgeApprovalMemberOf(ctx context.Context, idp identity.IdentityProvider, groups []string, subject string) (bool, error) {
	for _, g := range groups {
		if idp == nil {
			return false, errors.New("no identity provider")
		}
		ok, err := idp.ResolveMembership(ctx, g, subject)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// undetermined stamps an undetermined result with a sanitised detail. A
// next step already set (retry_after on the cap) is kept; otherwise
// retry_after.
func (d forgeApprovalDecision) undetermined(reason, msg string, cause error) forgeApprovalDecision {
	d.Result, d.Reason = forgeApprovalUndetermined, reason
	if d.NextStep == "" {
		d.NextStep = forgeApprovalNextRetryAfter
	}
	if cause != nil {
		msg += ": " + cause.Error()
	}
	d.Detail = sanitizeForgeReadDetail(reason, msg)
	return d
}

func nonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// Token shapes sanitizeForgeReadDetail redacts.
var (
	forgeDetailAuthHeader = regexp.MustCompile(`(?i)authorization\s*:\s*(bearer\s+|token\s+|basic\s+)?\S+`)
	forgeDetailBearer     = regexp.MustCompile(`(?i)\bbearer\s+\S+`)
	forgeDetailToken      = regexp.MustCompile(`\b(gh[spour]_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+)`)
)

// sanitizeForgeReadDetail bounds a forge-read detail for every surface it
// reaches (held reasons, settle logs, audit rows, API bodies): `kind: msg`,
// Authorization/Bearer values and ghs_/ghp_/gho_/ghu_/ghr_/github_pat_
// tokens redacted, first line only, control characters stripped, capped at
// forgeApprovalDetailMaxBytes on a rune boundary.
func sanitizeForgeReadDetail(kind, msg string) string {
	out := forgeDetailAuthHeader.ReplaceAllString(msg, "Authorization: [redacted]")
	out = forgeDetailBearer.ReplaceAllString(out, "Bearer [redacted]")
	out = forgeDetailToken.ReplaceAllString(out, "[redacted]")
	if i := strings.IndexAny(out, "\r\n"); i >= 0 {
		out = out[:i]
	}
	out = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, out)
	if kind != "" {
		out = kind + ": " + out
	}
	if len(out) > forgeApprovalDetailMaxBytes {
		cut := forgeApprovalDetailMaxBytes
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut]
	}
	return out
}

// forgeApprovalRefusal builds the 409 forge_approval_required body for a
// non-approved decision on the operator merge endpoint.
func forgeApprovalRefusal(d forgeApprovalDecision, runRow *run.Run) *mergeReadinessRefusal {
	prURL := ""
	if runRow != nil && runRow.PullRequestURL != nil {
		prURL = *runRow.PullRequestURL
	}
	runID := ""
	if runRow != nil {
		runID = runRow.ID.String()
	}
	msg := "no qualifying forge approval covers the verified head " + shaOrUnrecorded(d.VerifiedHead) +
		" (ADR-112 fold: " + d.Result + ", " + d.Reason + "). " + forgeApprovalNextStepProse(d) + " No merge verdict was recorded."
	ref := &mergeReadinessRefusal{
		Code:    mergeCodeForgeApprovalRequired,
		Message: msg,
		Details: map[string]any{
			"run_id":    runID,
			"pr_url":    prURL,
			"head_sha":  d.VerifiedHead,
			"fold":      d.Result,
			"reason":    d.Reason,
			"detail":    d.Detail,
			"next_step": d.NextStep,
		},
	}
	if d.VetoReviewer != "" {
		ref.Details["veto_reviewer"] = d.VetoReviewer
	}
	return ref
}

// forgeApprovalNextStepProse renders the next step a refusal names.
func forgeApprovalNextStepProse(d forgeApprovalDecision) string {
	switch d.NextStep {
	case forgeApprovalNextApprovePR:
		return "Approve the pull request at the current head under a write-or-higher GitHub identity that satisfies the review gate (gh pr review --approve), then re-invoke the merge."
	case forgeApprovalNextDismissReview:
		return "Reviewer " + d.VetoReviewer + " requested changes: resolve them and have the review dismissed or re-approved, then re-invoke the merge."
	case forgeApprovalNextRetryAfter:
		return "The approval could not be determined; re-invoke the merge after the forge recovers or the head settles."
	case forgeApprovalNextConfigureIdentityProvider:
		return "Configure a github identity provider so member_of can be resolved."
	}
	return "The forge has no approval-to-revision binding, so a declared review approval gate cannot be satisfied there."
}

// forgeApprovalAuthorization names the path writing a merge_queue_authorized
// row and its provenance.
type forgeApprovalAuthorization struct {
	// Path is queue, inline or delegated.
	Path string
	// IntentSequence is the merge_queue_entered sequence (queue path only).
	IntentSequence int64
	ActorKind      audit.ActorKind
	ActorSubject   string
}

// writeMergeQueueAuthorized appends the chained ADR-112 R4 evidence row for
// an approved decision on a declared gate and RETURNS its sequence and the
// append (or evidence-read) error: a caller holds (queue, delegated) or
// refuses 503 (inline) on an error, with no verdict row and no claim. It
// refuses an undeclared gate or a non-approved decision without writing.
func (s *Server) writeMergeQueueAuthorized(ctx context.Context, runRow *run.Run, d forgeApprovalDecision, ref forgeApprovalAuthorization) (int64, error) {
	if !d.Declared || d.Result != forgeApprovalApproved {
		return 0, fmt.Errorf("merge_queue_authorized: decision %q on a declared=%v gate is not an authorization", d.Result, d.Declared)
	}
	if s.cfg.AuditRepo == nil {
		return 0, errors.New("no audit repository is wired")
	}
	gateSeq, err := s.reviewGateApprovalSequence(ctx, runRow.ID)
	if err != nil {
		return 0, err
	}
	verifyRef, err := s.verifyPassReference(ctx, runRow.ID, d.VerifiedHead)
	if err != nil {
		return 0, err
	}
	members := d.approvals.members
	if members == nil {
		members = []string{}
	}
	escMemberOf := []string{}
	if len(d.approvals.memberOf) > 0 {
		escMemberOf = d.approvals.memberOf
	}
	snapshot := map[string]any{
		"result":              "satisfied",
		"count_required":      d.approvals.count,
		"identity":            map[string]any{"provider": forgeApprovalProviderGitHub, "subjects": countedSubjects(d.Counted)},
		"submitter_class":     "human",
		"auth_method":         "artifact",
		"channel":             "forge_review",
		"min_permission":      string(forgeApprovalFloor(d.approvals.minPermission)),
		"member_of":           renderMemberOf(d.approvals.memberOf),
		"members":             members,
		"escalated":           d.escalated,
		"escalated_member_of": escMemberOf,
	}
	fired := d.firedRuleKeys
	if fired == nil {
		fired = []string{}
	}
	chain := d.ChainHeads
	if chain == nil {
		chain = []string{}
	}
	var intent any = "none"
	if ref.Path == "queue" {
		intent = ref.IntentSequence
	}
	payload, err := json.Marshal(map[string]any{
		"run_id":                     runRow.ID.String(),
		"path":                       ref.Path,
		"head_sha":                   d.VerifiedHead,
		"predicate_snapshot":         snapshot,
		"counted_approvals":          d.Counted,
		"observed_at":                d.ObservedAt,
		"chain_heads":                chain,
		"policy_version":             map[string]any{"workflow_spec_sha256": d.specSHA256, "fired_escalations": fired},
		"basis":                      d.Basis,
		"review_gate_approval_seq":   gateSeq,
		"intent_sequence":            intent,
		"verify_pass":                verifyRef,
		"voucher_exception":          d.VoucherException,
		"review_decision":            d.ReviewDecision,
		"merge_state_status":         d.MergeStateStatus,
		"observed_head_sha":          d.ObservedHead,
		"permission_floor_effective": string(forgeApprovalFloor(d.approvals.minPermission)),
	})
	if err != nil {
		return 0, err
	}
	kind, subject := ref.ActorKind, ref.ActorSubject
	if kind == "" {
		kind = audit.ActorSystem
	}
	if subject == "" {
		subject = "fishhawk"
	}
	e, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runRow.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryMergeQueueAuthorized,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	})
	if err != nil {
		return 0, fmt.Errorf("append %s: %w", CategoryMergeQueueAuthorized, err)
	}
	if e == nil {
		return 0, nil
	}
	return e.Sequence, nil
}

// countedSubjects is the provider-qualified subjects of the counted approvals.
func countedSubjects(c []countedApproval) []string {
	out := make([]string, 0, len(c))
	for _, a := range c {
		out = append(out, identity.CanonicalMember(a.Login, forgeApprovalProviderGitHub))
	}
	return out
}

// reviewGateApprovalSequence is the newest approval_submitted row on the
// run's review stage (0 when none).
func (s *Server) reviewGateApprovalSequence(ctx context.Context, runID uuid.UUID) (int64, error) {
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "approval_submitted")
	if err != nil {
		return 0, fmt.Errorf("list approval_submitted: %w", err)
	}
	stage, err := s.forgeApprovalReviewStage(ctx, runID)
	if err != nil {
		return 0, nil //nolint:nilerr // a run with no review stage row has no review-gate approval to reference
	}
	var seq int64
	for _, e := range rows {
		if e.StageID != nil && *e.StageID == stage.ID && e.Sequence > seq {
			seq = e.Sequence
		}
	}
	return seq, nil
}

// verifyPassReference is the newest passed merge_candidate_verified sequence
// at head, or "not_required" when none exists.
func (s *Server) verifyPassReference(ctx context.Context, runID uuid.UUID, head string) (any, error) {
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryMergeCandidateVerified)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", CategoryMergeCandidateVerified, err)
	}
	var seq int64
	for _, e := range rows {
		var p mergeCandidateVerifiedPayload
		if json.Unmarshal(e.Payload, &p) != nil || p.HeadSHA != head || p.Result != mergeCandidateResultPassed {
			continue
		}
		if e.Sequence > seq {
			seq = e.Sequence
		}
	}
	if seq == 0 {
		return forgeApprovalBasisNotRequired, nil
	}
	return map[string]any{"merge_candidate_verified_sequence": seq}, nil
}
