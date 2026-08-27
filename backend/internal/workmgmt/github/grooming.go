package github

// This file implements the optional workmgmt.GroomingMutator capability
// (E54.5 / #2237) on the GitHub Projects provider: execute ONE cleared
// grooming mutation against the tracker.
//
// It is the WRITE counterpart to reader.go. Every containment decision — the
// join, the operator verdict, the action-class mode, the destructive
// authorization, the idempotence diff, the manual-placement courtesy — has
// already been made by workmgmt.ApplyGrooming BEFORE a request reaches here,
// so this file does NOT re-litigate authorization. It re-checks exactly one
// thing as defence in depth, the expected-source set on a board move, because
// Transition already does and because that guard protects a human's manual
// placement (approval condition I2).
//
// NOTHING IN PRODUCTION CALLS THIS YET (see grooming_mutator.go): no HTTP
// route, MCP tool, CLI verb, env var or config field resolves a mutator.
//
// WHAT THE TESTS PROVE, AND WHAT THEY DO NOT (approval condition I4). The
// httptest fixtures in grooming_test.go and githubclient/client_test.go pin
// the REQUEST SHAPE this provider emits — method, path, exact serialized body,
// call count, and which calls are NOT made. They do NOT and cannot prove that
// GitHub ACCEPTS that shape: whether the forge honours the `duplicate` /
// `not_planned` state_reason values on a close is a server-side question no
// local fixture decides. That residual needs live validation against a real
// repository and is recorded as such in README.md rather than implied away.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// UnsupportedGroomingKindError is the TYPED refusal for a mutation kind this
// provider cannot execute. It exists because a silent no-op is the one outcome
// that must not be possible: an apply that reported "applied" while the
// tracker never changed would put a fabricated row in the audit trail. A kind
// added to workmgmt's closed vocabulary without a branch here therefore FAILS
// LOUD on the first dispatch rather than quietly succeeding.
type UnsupportedGroomingKindError struct {
	Kind   workmgmt.GroomingMutationKind
	Detail string
}

func (e *UnsupportedGroomingKindError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("workmgmt/github: cannot execute grooming mutation kind %q: %s", e.Kind, e.Detail)
	}
	return fmt.Sprintf("workmgmt/github: cannot execute grooming mutation kind %q", e.Kind)
}

// ParentEpicConflictError is the TYPED refusal for an epic link whose item
// already records a DIFFERENT parent epic (#2237 review).
//
// It exists because the alternative is the worst outcome available. The apply
// layer dispatches an epic link ONLY when its pre-dispatch read saw a parent
// marker that does not match the proposal — i.e. exactly when a CORRECTION is
// being requested — and reporting that correction as "already present" would
// tell the operator the tracker agrees with the report when it does not, and
// would keep telling them on every re-apply.
//
// It refuses rather than re-parents because this provider has no primitive to
// re-parent with: linkEpic's only edge write is AddSubIssue, which ADDS an
// edge, and there is no RemoveSubIssue and no replace-parent option on the
// client. Overwriting the marker while leaving the old sub-issue edge in place
// would make the body claim one parent while the graph holds another — a
// worse lie than the one this replaces. So the conflict is surfaced as a
// FAILED candidate naming both parents, and a human decides which is right.
type ParentEpicConflictError struct {
	Number   int
	Current  string
	Proposed string
}

func (e *ParentEpicConflictError) Error() string {
	return fmt.Sprintf(
		"workmgmt/github: #%d already records parent epic %s but %s was proposed; "+
			"re-parenting is not available through this provider (no sub-issue removal primitive) — resolve the parent by hand",
		e.Number, e.Current, e.Proposed)
}

// groomingFieldNames maps each VALUE-SET kind onto the project board field it
// writes. These are FIELD WRITES, not positional reordering (approval
// condition I3): this provider implements no positional primitive, so a
// rank_set writes the Rank field's value and the board's own item order does
// NOT move. E54.6 / #2238's campaign feed reads the Rank field back and sorts
// on it — see README.md.
var groomingFieldNames = map[workmgmt.GroomingMutationKind]string{
	// The missing_estimate hygiene defect is the only defect that maps to
	// field_set, so the field it writes is the Estimate field.
	workmgmt.GroomingKindFieldSet:    "Estimate",
	workmgmt.GroomingKindPrioritySet: "Priority",
	workmgmt.GroomingKindRankSet:     "Rank",
}

// closeStateReasons maps each destructive CLOSE kind onto the GitHub issue
// `state_reason` it closes with. Icebox is deliberately absent: it is a board
// move, not a close.
var closeStateReasons = map[workmgmt.GroomingMutationKind]string{
	workmgmt.GroomingKindCloseDuplicate:  "duplicate",
	workmgmt.GroomingKindCloseNotPlanned: "not_planned",
}

// labelAdder is the ADDITIVE label primitive, declared as an OPTIONAL
// extension of API rather than a member of it.
//
// Only this one mutation kind needs it. Promoting it into API would make it a
// compile-time obligation for EVERY API implementation — including the filing
// and board-sync fakes that never add a label to an existing issue — so each
// would have to grow a stub for a capability it cannot reach. The optional
// shape keeps the requirement where the requirement is.
//
// *githubclient.Client satisfies it (the signature matches
// Client.AddIssueLabels exactly), so the production provider takes the write
// path; an API that does not is refused with a typed
// ReasonNotImplemented UnavailableError rather than silently degrading to a
// wholesale UpdateIssue PATCH, which is the lost-update shape groomingSetLabels
// exists to avoid.
type labelAdder interface {
	AddIssueLabels(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int, labels []string) ([]string, error)
}

// groomingUnavailable builds the typed capability-unavailable error for the
// MUTATE capability, so a caller classifies a grooming degradation through the
// same errors.As chokepoint the read capability uses.
func groomingUnavailable(reason workmgmt.UnavailableReason, detail string, cause error) *workmgmt.UnavailableError {
	return &workmgmt.UnavailableError{
		Provider:   ProviderName,
		Capability: workmgmt.GroomingCapability,
		Reason:     reason,
		Detail:     detail,
		Cause:      cause,
	}
}

// ApplyGroomingMutation implements workmgmt.GroomingMutator: execute exactly
// one cleared mutation and report what it did.
//
// The kind switch is exhaustive over workmgmt's closed vocabulary and its
// default arm returns *UnsupportedGroomingKindError — never a nil-error
// no-op.
func (p *Provider) ApplyGroomingMutation(ctx context.Context, req workmgmt.GroomingMutationRequest) (*workmgmt.GroomingMutationResult, error) {
	repo, number, err := p.groomingPreflight(req)
	if err != nil {
		return nil, err
	}

	switch req.Kind {
	case workmgmt.GroomingKindLabelSet:
		return p.groomingSetLabels(ctx, req, repo, number)
	case workmgmt.GroomingKindEpicLink:
		return p.groomingLinkEpic(ctx, req, repo, number)
	case workmgmt.GroomingKindDependsOnAdd:
		return p.groomingAddDependsOn(ctx, req, repo, number)
	case workmgmt.GroomingKindCloseDuplicate, workmgmt.GroomingKindCloseNotPlanned:
		return p.groomingCloseIssue(ctx, req, repo, number)
	case workmgmt.GroomingKindBoardPlace, workmgmt.GroomingKindIcebox:
		return p.groomingMoveCard(ctx, req, repo, number)
	case workmgmt.GroomingKindFieldSet, workmgmt.GroomingKindPrioritySet, workmgmt.GroomingKindRankSet:
		return p.groomingSetField(ctx, req, repo, number)
	default:
		return nil, &UnsupportedGroomingKindError{Kind: req.Kind,
			Detail: "no GitHub primitive is wired for this kind; wire one or remove the kind from the closed vocabulary"}
	}
}

// groomingPreflight validates the shared preconditions and resolves the issue
// number. The api/repo checks are programming errors (File's actionable
// style); a missing installation scope is a TYPED ReasonNoInstallation so a
// caller can tell "we could not authenticate" from "the mutation was refused".
func (p *Provider) groomingPreflight(req workmgmt.GroomingMutationRequest) (forge.RepoRef, int, error) {
	if p.api == nil {
		return forge.RepoRef{}, 0, errors.New("workmgmt/github: provider missing API client")
	}
	if req.Target.Repo.Owner == "" || req.Target.Repo.Name == "" {
		return forge.RepoRef{}, 0, errors.New("workmgmt/github: target repo owner and name required")
	}
	if req.Target.Scope.IsZero() {
		return forge.RepoRef{}, 0, groomingUnavailable(workmgmt.ReasonNoInstallation,
			"no installation id available; grooming mutations are run-scoped in v0", nil)
	}
	number, err := parseIssueRef(strings.TrimPrefix(strings.TrimSpace(req.ItemRef), "issue:"))
	if err != nil {
		return forge.RepoRef{}, 0, fmt.Errorf("workmgmt/github: grooming target %q: %w", req.ItemRef, err)
	}
	return forge.RepoRef{Owner: req.Target.Repo.Owner, Name: req.Target.Repo.Name}, number, nil
}

// groomingSetLabels adds the proposed labels to the item's set.
//
// IT DOES NOT SEND A UNION THROUGH UpdateIssue, and that is the point
// (#2237 review). GitHub's PATCH replaces `labels` WHOLESALE, so an
// add-a-label caller built on it must read the current set and PATCH the
// union — a read-modify-write whose window two concurrent applies can both
// enter: each reads the same pre-state, each computes a union missing the
// other's label, and the later PATCH replaces the earlier one's label away.
// No amount of local guarding closes that, because the losing write is a
// perfectly well-formed request.
//
// So the write goes through AddIssueLabels (POST .../labels), which merges
// SERVER-SIDE: the payload carries ONLY the labels being added, GitHub unions
// them into whatever the issue currently holds, and a concurrent add cannot be
// clobbered because no client ever transmits the full set. The race is removed
// structurally rather than guarded — the additive endpoint is also exactly the
// additive operation a hygiene label fix means.
//
// The pre-read survives for two NON-load-bearing reasons: it reports the
// pre-state as Observed, and it turns an already-present label into a
// provider-side SKIP rather than a wasted round trip. Neither is a correctness
// dependency — a stale read costs at most one redundant additive POST, which
// GitHub treats as a no-op.
func (p *Provider) groomingSetLabels(ctx context.Context, req workmgmt.GroomingMutationRequest,
	repo forge.RepoRef, number int) (*workmgmt.GroomingMutationResult, error) {
	// Checked BEFORE the pre-read: an API without the additive primitive can
	// never complete this mutation, so refusing up front costs no round trip
	// and cannot report a partial observation for a write that never happens.
	adder, ok := p.api.(labelAdder)
	if !ok {
		return nil, groomingUnavailable(workmgmt.ReasonNotImplemented,
			"api client does not implement the additive AddIssueLabels primitive; a label mutation must not fall back to a wholesale UpdateIssue PATCH", nil)
	}
	issue, err := p.api.GetIssue(ctx, req.Target.Scope, repo, number)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: read labels of #%d: %w", number, err)
	}
	have := map[string]struct{}{}
	for _, l := range issue.Labels {
		have[l] = struct{}{}
	}
	var add []string
	for _, l := range req.After.List {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if _, ok := have[l]; ok {
			continue
		}
		have[l] = struct{}{}
		add = append(add, l)
	}
	observed := workmgmt.GroomingValue{List: append([]string(nil), issue.Labels...)}
	if len(add) == 0 {
		return &workmgmt.GroomingMutationResult{Skipped: true, SkipReason: "labels already present",
			ProviderResponse: "no label added; the issue already carries every proposed label",
			Observed:         observed}, nil
	}
	if _, err := adder.AddIssueLabels(ctx, req.Target.Scope, repo, number, add); err != nil {
		return nil, fmt.Errorf("workmgmt/github: add labels to #%d: %w", number, err)
	}
	return &workmgmt.GroomingMutationResult{Applied: true, Observed: observed,
		ProviderResponse: fmt.Sprintf("added labels %s to #%d", strings.Join(add, ", "), number)}, nil
}

// groomingLinkEpic links the item as a sub-issue of the proposed parent epic
// AND stamps the `Parent epic: #N` body marker.
//
// BOTH writes, because the write and the pre-dispatch read must observe the
// SAME persisted relationship (#2237 review). The sub-issue graph is the real
// relationship, but it is not what the idempotence diff can see:
// workmgmt.WorkItemRecord exposes labels, body, state and board column — no
// parent edge — so groomingObserved reads the body marker. A write that
// persisted the link ONLY through AddSubIssue would therefore be invisible to
// the next apply's read, and a second apply would re-dispatch a link that
// already exists — AC7 broken for this kind, with the audit row claiming a
// mutation that changed nothing.
//
// So the marker is not decoration: it is the projection of the relationship
// onto the surface the reader can observe, exactly as depends_on already does
// (ADR-047 / #1437), and it matches the `Parent epic: #N` body convention this
// repository's issue bodies already carry.
//
// ORDER IS DELIBERATE: link first, marker second. A marker written before a
// failed link would claim a relationship that does not exist and would
// SUPPRESS the retry — the silent-wrong-state outcome. This way a failure
// between the two is loud (an error, candidate recorded failed) and the next
// apply re-attempts; the residual is that the re-attempted AddSubIssue may be
// refused by GitHub as a duplicate edge, which surfaces as a failed candidate
// rather than as a false "applied".
func (p *Provider) groomingLinkEpic(ctx context.Context, req workmgmt.GroomingMutationRequest,
	repo forge.RepoRef, number int) (*workmgmt.GroomingMutationResult, error) {
	parent := strings.TrimSpace(req.After.Scalar)
	want := renderParentEpicMarker(parent)
	if want == "" {
		return nil, &UnsupportedGroomingKindError{Kind: req.Kind, Detail: "no parent epic reference was proposed"}
	}
	issue, err := p.api.GetIssue(ctx, req.Target.Scope, repo, number)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: read body of #%d: %w", number, err)
	}
	observed := workmgmt.GroomingValue{Scalar: parseParentEpicMarker(issue.Body)}
	// THE ALREADY-PRESENT TEST IS PER-PARENT, NOT PER-MARKER (#2237 review).
	// ensureParentEpicMarker is idempotent on the MARKER — it returns any
	// marker-bearing body unchanged — so reading its unchanged return as
	// "the requested relationship exists" conflates two different bodies: one
	// naming the proposed parent, and one naming a DIFFERENT parent. The
	// second is precisely the case the apply layer dispatches on (its
	// pre-dispatch read diffs the marker value), so treating it as a skip
	// reports the requested correction as already done.
	//
	// Every marker line is compared, not just the first, mirroring the read
	// side's multi-marker observation; each is normalized through
	// renderParentEpicMarker so `1437` and `#1437` are one parent, not two.
	if existing := parentEpicMarkerValues(issue.Body); len(existing) > 0 {
		for _, cur := range existing {
			if renderParentEpicMarker(cur) == want {
				// The relationship is already recorded on the surface the next
				// read observes, so re-linking would be a duplicate dispatch.
				return &workmgmt.GroomingMutationResult{Skipped: true, SkipReason: "parent epic marker already present",
					ProviderResponse: fmt.Sprintf("#%d already records parent epic %s", number, cur),
					Observed:         observed}, nil
			}
		}
		return nil, &ParentEpicConflictError{Number: number,
			Current: strings.Join(existing, ", "), Proposed: strings.TrimPrefix(want, "Parent epic: ")}
	}
	updated := ensureParentEpicMarker(issue.Body, parent)
	childNodeID, err := p.api.IssueNodeID(ctx, req.Target.Scope, repo, number)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: resolve issue #%d: %w", number, err)
	}
	if err := p.linkEpic(ctx, req.Target.Scope, repo, parent, childNodeID); err != nil {
		return nil, err
	}
	if _, err := p.api.UpdateIssue(ctx, req.Target.Scope, repo, number,
		githubclient.UpdateIssueParams{Body: &updated}); err != nil {
		return nil, fmt.Errorf("workmgmt/github: record parent epic on #%d: %w", number, err)
	}
	return &workmgmt.GroomingMutationResult{Applied: true, Observed: observed,
		ProviderResponse: fmt.Sprintf("linked #%d as a sub-issue of %s and recorded the parent-epic marker", number, parent)}, nil
}

// parentEpicMarkerRE matches the `Parent epic:` body marker line and captures
// the reference. Line-anchored and case-insensitive, like
// dependsOnMarkerRE, so the marker is found wherever it sits in the body. It
// is the single source of truth for the marker shape, paired with
// renderParentEpicMarker so the write and the read cannot drift.
var parentEpicMarkerRE = regexp.MustCompile(`(?im)^Parent epic:\s*(.+)$`)

// renderParentEpicMarker renders the marker line for ref as
// `Parent epic: #N`, normalizing a bare `N` to `#N` so a suggested fix written
// either way persists in ONE shape. Returns "" for an empty ref.
func renderParentEpicMarker(ref string) string {
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ref), "#"))
	if s == "" {
		return ""
	}
	return "Parent epic: #" + s
}

// ensureParentEpicMarker appends the marker line to body when body does not
// already carry one. Idempotent on the MARKER: a body that already has a
// `Parent epic:` line is returned UNCHANGED, so re-applying never
// double-stamps it.
//
// Its unchanged return is NOT an "already linked to this parent" signal and
// must not be read as one — it is marker-shaped, not parent-valued, so a body
// naming a DIFFERENT parent also comes back unchanged. groomingLinkEpic
// therefore discriminates on the marker VALUES (parentEpicMarkerValues) before
// calling this, and only reaches it once no marker exists.
func ensureParentEpicMarker(body, ref string) string {
	marker := renderParentEpicMarker(ref)
	if marker == "" {
		return body
	}
	if parentEpicMarkerRE.MatchString(body) {
		return body
	}
	if strings.TrimSpace(body) == "" {
		return marker
	}
	return strings.TrimRight(body, "\n") + "\n\n" + marker
}

// parseParentEpicMarker returns the reference on the FIRST `Parent epic:` body
// line, or "" when the body carries none. Paired with renderParentEpicMarker
// as the single source of truth for the marker round trip.
func parseParentEpicMarker(body string) string {
	m := parentEpicMarkerRE.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// parentEpicMarkerValues returns the reference on EVERY `Parent epic:` body
// line, in body order, or nil when the body carries none. groomingLinkEpic
// needs all of them, not just the first: a body carrying two marker lines is
// malformed but real, and an already-linked test that looked only at the first
// would refuse a link the body already records further down.
func parentEpicMarkerValues(body string) []string {
	var out []string
	for _, m := range parentEpicMarkerRE.FindAllStringSubmatch(body, -1) {
		if v := strings.TrimSpace(m[1]); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// groomingAddDependsOn records a depends_on edge on the item's body, MERGING it
// into any marker line the body already carries (#2860). It calls the amend
// path's appendDependsOnRef, NOT the filing path's ensureDependsOnMarker, whose
// coarser PRESENCE check refused every SECOND edge out of an item and reported
// that refusal as an ordinary no-op — the 0/8 apply rate this issue removes.
// The question is MEMBERSHIP (is THIS ref already recorded?), not presence (is
// there a marker at all?), which is what the layer above already asks.
//
// The `depends_on ref already present` skip below is reachable only when the
// CORE and the PROVIDER DISAGREE — ApplyGrooming's groomingSatisfied asks the
// same membership question BEFORE dispatch and short-circuits to
// `already_applied`, so a body would have to be mutated between that read and
// this write to reach here. It is defence in depth, and is pinned by a unit
// test on appendDependsOnRef rather than end to end through ApplyGrooming.
func (p *Provider) groomingAddDependsOn(ctx context.Context, req workmgmt.GroomingMutationRequest,
	repo forge.RepoRef, number int) (*workmgmt.GroomingMutationResult, error) {
	dep := strings.TrimSpace(req.After.Scalar)
	if dep == "" {
		return nil, &UnsupportedGroomingKindError{Kind: req.Kind, Detail: "no depends_on reference was proposed"}
	}
	issue, err := p.api.GetIssue(ctx, req.Target.Scope, repo, number)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: read body of #%d: %w", number, err)
	}
	updated, changed := appendDependsOnRef(issue.Body, dep)
	observed := workmgmt.GroomingValue{Scalar: dep}
	if !changed {
		return &workmgmt.GroomingMutationResult{Skipped: true, SkipReason: "depends_on ref already present",
			ProviderResponse: fmt.Sprintf("#%d already records depends_on %s", number, dep),
			Observed:         observed}, nil
	}
	if _, err := p.api.UpdateIssue(ctx, req.Target.Scope, repo, number,
		githubclient.UpdateIssueParams{Body: &updated}); err != nil {
		return nil, fmt.Errorf("workmgmt/github: record depends_on on #%d: %w", number, err)
	}
	return &workmgmt.GroomingMutationResult{Applied: true, Observed: observed,
		ProviderResponse: fmt.Sprintf("recorded depends_on %s on #%d", dep, number)}, nil
}

// groomingCloseIssue closes the item with the kind's state_reason. The reason
// vocabulary is a map lookup rather than an inline conditional so a close kind
// added without a reason fails loud here instead of closing as `completed`,
// which would misreport a de-duplicated issue as delivered work.
func (p *Provider) groomingCloseIssue(ctx context.Context, req workmgmt.GroomingMutationRequest,
	repo forge.RepoRef, number int) (*workmgmt.GroomingMutationResult, error) {
	reason, ok := closeStateReasons[req.Kind]
	if !ok {
		return nil, &UnsupportedGroomingKindError{Kind: req.Kind, Detail: "no close state_reason is mapped for this kind"}
	}
	state := "closed"
	if _, err := p.api.UpdateIssue(ctx, req.Target.Scope, repo, number,
		githubclient.UpdateIssueParams{State: &state, StateReason: &reason}); err != nil {
		return nil, fmt.Errorf("workmgmt/github: close #%d as %s: %w", number, reason, err)
	}
	return &workmgmt.GroomingMutationResult{Applied: true,
		Observed:         workmgmt.GroomingValue{Scalar: "closed"},
		ProviderResponse: fmt.Sprintf("closed #%d as %s", number, reason)}, nil
}

// groomingMoveCard executes a board move (board_place and icebox alike),
// re-checking the expected-source set PROVIDER-SIDE as defence in depth before
// writing — the same never-fight-the-human courtesy Transition applies, and
// the reason icebox is routed through this method rather than a bespoke one
// (approval condition I2).
//
// The card write itself reuses placeIssueOnBoard, the shared add-item +
// set-single-select routine File uses. That resolves the project fields a
// second time (this method already resolved them for the pre-check); the
// duplicate round trip is accepted deliberately in exchange for having ONE
// board-write routine rather than a second copy that could drift from it.
func (p *Provider) groomingMoveCard(ctx context.Context, req workmgmt.GroomingMutationRequest,
	repo forge.RepoRef, number int) (*workmgmt.GroomingMutationResult, error) {
	column := strings.TrimSpace(req.After.Scalar)
	if column == "" {
		// For icebox this is the no-icebox-column case ApplyGrooming already
		// refuses upstream (GroomingSkipIceboxColumnUnavailable). Reaching it
		// here means a caller bypassed that, so it is a typed refusal, never a
		// misroute to some other column (approval condition I5).
		return nil, &UnsupportedGroomingKindError{Kind: req.Kind,
			Detail: "no target board column was resolved; an icebox move needs an icebox column configured in the conventions"}
	}
	proj := req.Target.Project
	if proj == nil {
		return nil, groomingUnavailable(workmgmt.ReasonNoProjectConfigured,
			"a board move was requested but the target carries no project connection; configure work_management.project in the repo conventions", nil)
	}
	boardCtx := ctx
	if proj.OwnerType == "user" {
		// User-owned Projects v2 cannot be written with an App installation
		// token (#1114). Transition degrades to a best-effort skip there; a
		// MUTATION must not silently do nothing, so fail typed.
		if !p.api.ProjectsTokenConfigured() {
			return nil, groomingUnavailable(workmgmt.ReasonNoProjectsToken,
				"the project board is user-owned, which a GitHub App installation token cannot reach; set FISHHAWKD_PROJECTS_TOKEN to a project-scoped token", nil)
		}
		boardCtx = githubclient.WithProjectsToken(ctx)
	}

	coord := githubclient.ProjectCoord{Owner: proj.Owner, OwnerType: proj.OwnerType, Number: proj.Number}
	meta, err := p.api.ProjectFields(boardCtx, req.Target.Scope, coord, statusFieldName)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: resolve project fields: %w", err)
	}
	if _, ok := meta.StatusOptions[column]; !ok {
		return nil, &UnsupportedGroomingKindError{Kind: req.Kind,
			Detail: fmt.Sprintf("target column %q is not a %s option on the project; available: %s",
				column, statusFieldName, strings.Join(sortedKeys(meta.StatusOptions), ", "))}
	}
	nodeID, err := p.api.IssueNodeID(boardCtx, req.Target.Scope, repo, number)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: resolve issue #%d: %w", number, err)
	}
	item, err := p.api.ProjectItemStatus(boardCtx, req.Target.Scope, nodeID, meta.ProjectID, statusFieldName)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: read project item status for #%d: %w", number, err)
	}
	observed := workmgmt.GroomingValue{Scalar: item.Status}
	if !groomingSourceAllows(req.ExpectedFrom, item, req.States) {
		// REFUSED, not skipped (#2860): a board move the provider DECLINED to
		// perform because a human placed the card elsewhere. That is not an
		// idempotent no-op and must not read as one in the audit.
		return &workmgmt.GroomingMutationResult{Refused: true, RefuseReason: "manual_placement_preserved",
			ProviderResponse: fmt.Sprintf("#%d sits at %q, outside the expected source set", number, labelOrUnset(item.Status)),
			Observed:         observed}, nil
	}
	if item.OnBoard && item.Status == column {
		return &workmgmt.GroomingMutationResult{Skipped: true, SkipReason: "already at target column",
			ProviderResponse: fmt.Sprintf("#%d is already at %q", number, column), Observed: observed}, nil
	}
	// The ORIGINAL ctx, not boardCtx, and that is correct: placeIssueOnBoard
	// OWNS the projects-token routing for its own calls — it re-applies
	// githubclient.WithProjectsToken itself for a user-owned board, because a
	// GitHub App installation token cannot write user-owned Projects v2
	// (#1114). Wrapping here would be redundant, not safer. Recorded at the
	// call site because the asymmetry with groomingSetField (which threads its
	// own wrapped ctx into its own write) reads as a bug from a diff, and has
	// been raised as one. TestApplyGroomingMutation_BoardWriteUsesTheProjectsToken
	// asserts the write actually receives the opt-in, so this is a covered
	// claim rather than a comment asserting its own correctness.
	if err := placeIssueOnBoard(ctx, p.api, req.Target.Scope, proj, column,
		&githubclient.CreatedIssue{Number: number, NodeID: nodeID}); err != nil {
		return nil, err
	}
	return &workmgmt.GroomingMutationResult{Applied: true, Observed: observed,
		ProviderResponse: fmt.Sprintf("moved #%d to %q", number, column)}, nil
}

// groomingSourceAllows is the provider-side expected-source re-check, applied
// to EVERY board-placement kind including icebox. It mirrors
// workmgmt.groomingPlacementAllowed exactly: an EMPTY expected-source set is a
// placement of an OFF-BOARD item, which proceeds only while the item is
// genuinely off-board; a non-empty set requires the card's current column to
// reverse-map to a canonical state in the set.
//
// It is deliberately NOT sourceAllows (Transition's helper), which treats an
// unset status as Backlog so a fresh card still advances. A grooming apply
// must not infer a state that was never set: an icebox move is destructive,
// and inferring Backlog for an unset column would park a card whose real state
// nobody has declared.
func groomingSourceAllows(expectedFrom []string, item *githubclient.ProjectItemStatus, states map[string]string) bool {
	if item == nil {
		return false
	}
	if len(expectedFrom) == 0 {
		return !item.OnBoard
	}
	if !item.OnBoard {
		return false
	}
	current := workmgmt.CanonicalStateForOption(states, item.Status)
	for _, s := range expectedFrom {
		if s == current {
			return true
		}
	}
	return false
}

// groomingSetField writes a board FIELD VALUE for the three value-set kinds.
//
// It is a FIELD WRITE, not a positional reorder (approval condition I3): this
// provider exposes no positional primitive, so a rank_set changes the Rank
// field's value and leaves the board's own item order untouched. The value
// must be one of the field's configured single-select options; anything else
// is a typed refusal rather than a guess at the nearest option.
func (p *Provider) groomingSetField(ctx context.Context, req workmgmt.GroomingMutationRequest,
	repo forge.RepoRef, number int) (*workmgmt.GroomingMutationResult, error) {
	fieldName, ok := groomingFieldNames[req.Kind]
	if !ok {
		return nil, &UnsupportedGroomingKindError{Kind: req.Kind, Detail: "no board field is mapped for this kind"}
	}
	value := strings.TrimSpace(req.After.Scalar)
	if value == "" {
		return nil, &UnsupportedGroomingKindError{Kind: req.Kind,
			Detail: fmt.Sprintf("no %s value was proposed", fieldName)}
	}
	proj := req.Target.Project
	if proj == nil {
		return nil, groomingUnavailable(workmgmt.ReasonNoProjectConfigured,
			"a board field write was requested but the target carries no project connection; configure work_management.project in the repo conventions", nil)
	}
	if proj.OwnerType == "user" {
		if !p.api.ProjectsTokenConfigured() {
			return nil, groomingUnavailable(workmgmt.ReasonNoProjectsToken,
				"the project board is user-owned, which a GitHub App installation token cannot reach; set FISHHAWKD_PROJECTS_TOKEN to a project-scoped token", nil)
		}
		ctx = githubclient.WithProjectsToken(ctx)
	}
	coord := githubclient.ProjectCoord{Owner: proj.Owner, OwnerType: proj.OwnerType, Number: proj.Number}
	meta, err := p.api.ProjectFields(ctx, req.Target.Scope, coord, fieldName)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: resolve %s field: %w", fieldName, err)
	}
	optionID, ok := meta.StatusOptions[value]
	if !ok {
		return nil, &UnsupportedGroomingKindError{Kind: req.Kind,
			Detail: fmt.Sprintf("value %q is not a %s option on the project; available: %s",
				value, fieldName, strings.Join(sortedKeys(meta.StatusOptions), ", "))}
	}
	nodeID, err := p.api.IssueNodeID(ctx, req.Target.Scope, repo, number)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: resolve issue #%d: %w", number, err)
	}
	item, err := p.api.ProjectItemStatus(ctx, req.Target.Scope, nodeID, meta.ProjectID, fieldName)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: read project item for #%d: %w", number, err)
	}
	if !item.OnBoard {
		// REFUSED, not skipped (#2860): the requested field write did not
		// happen and the value was not already correct — there is no card to
		// write it to. An operator reading `skipped` would read that as
		// already-satisfied.
		return &workmgmt.GroomingMutationResult{Refused: true, RefuseReason: "not on board",
			ProviderResponse: fmt.Sprintf("#%d carries no card on the project, so there is no %s field to write", number, fieldName)}, nil
	}
	if err := p.api.SetProjectItemSingleSelect(ctx, req.Target.Scope, meta.ProjectID, item.ItemID, meta.FieldID, optionID); err != nil {
		return nil, fmt.Errorf("workmgmt/github: set %s field on #%d: %w", fieldName, number, err)
	}
	return &workmgmt.GroomingMutationResult{Applied: true,
		Observed:         workmgmt.GroomingValue{Scalar: item.Status},
		ProviderResponse: fmt.Sprintf("set %s on #%d to %q", fieldName, number, value)}, nil
}
