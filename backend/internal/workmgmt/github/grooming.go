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
// GitHub's PATCH replaces `labels` WHOLESALE, so this reads the current set
// and sends the UNION — never the proposed labels alone, which would strip
// every other label off the issue. A union identical to the current set is a
// provider-side SKIP rather than a wasted write.
func (p *Provider) groomingSetLabels(ctx context.Context, req workmgmt.GroomingMutationRequest,
	repo forge.RepoRef, number int) (*workmgmt.GroomingMutationResult, error) {
	issue, err := p.api.GetIssue(ctx, req.Target.Scope, repo, number)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: read labels of #%d: %w", number, err)
	}
	have := map[string]struct{}{}
	union := append([]string(nil), issue.Labels...)
	for _, l := range issue.Labels {
		have[l] = struct{}{}
	}
	added := 0
	for _, l := range req.After.List {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if _, ok := have[l]; ok {
			continue
		}
		have[l] = struct{}{}
		union = append(union, l)
		added++
	}
	observed := workmgmt.GroomingValue{List: append([]string(nil), issue.Labels...)}
	if added == 0 {
		return &workmgmt.GroomingMutationResult{Skipped: true, SkipReason: "labels already present",
			ProviderResponse: "no label added; the issue already carries every proposed label",
			Observed:         observed}, nil
	}
	if _, err := p.api.UpdateIssue(ctx, req.Target.Scope, repo, number,
		githubclient.UpdateIssueParams{Labels: &union}); err != nil {
		return nil, fmt.Errorf("workmgmt/github: set labels on #%d: %w", number, err)
	}
	return &workmgmt.GroomingMutationResult{Applied: true, Observed: observed,
		ProviderResponse: fmt.Sprintf("set labels on #%d to %s", number, strings.Join(union, ", "))}, nil
}

// groomingLinkEpic links the item as a sub-issue of the proposed parent epic,
// reusing the same IssueNodeID + AddSubIssue path File's linkEpic uses.
func (p *Provider) groomingLinkEpic(ctx context.Context, req workmgmt.GroomingMutationRequest,
	repo forge.RepoRef, number int) (*workmgmt.GroomingMutationResult, error) {
	parent := strings.TrimSpace(req.After.Scalar)
	if parent == "" {
		return nil, &UnsupportedGroomingKindError{Kind: req.Kind, Detail: "no parent epic reference was proposed"}
	}
	childNodeID, err := p.api.IssueNodeID(ctx, req.Target.Scope, repo, number)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: resolve issue #%d: %w", number, err)
	}
	if err := p.linkEpic(ctx, req.Target.Scope, repo, parent, childNodeID); err != nil {
		return nil, err
	}
	return &workmgmt.GroomingMutationResult{Applied: true,
		ProviderResponse: fmt.Sprintf("linked #%d as a sub-issue of %s", number, parent)}, nil
}

// groomingAddDependsOn records a depends_on edge on the item's body, reusing
// the existing idempotent ensureDependsOnMarker. A body already carrying a
// marker is returned unchanged by that helper, which this reports as a
// provider-side SKIP rather than writing an identical body.
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
	updated := ensureDependsOnMarker(issue.Body, []string{dep})
	observed := workmgmt.GroomingValue{Scalar: dep}
	if updated == issue.Body {
		return &workmgmt.GroomingMutationResult{Skipped: true, SkipReason: "depends_on marker already present",
			ProviderResponse: fmt.Sprintf("#%d already carries a depends_on marker", number),
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
		return &workmgmt.GroomingMutationResult{Skipped: true, SkipReason: "manual_placement_preserved",
			ProviderResponse: fmt.Sprintf("#%d sits at %q, outside the expected source set", number, labelOrUnset(item.Status)),
			Observed:         observed}, nil
	}
	if item.OnBoard && item.Status == column {
		return &workmgmt.GroomingMutationResult{Skipped: true, SkipReason: "already at target column",
			ProviderResponse: fmt.Sprintf("#%d is already at %q", number, column), Observed: observed}, nil
	}
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
		return &workmgmt.GroomingMutationResult{Skipped: true, SkipReason: "not on board",
			ProviderResponse: fmt.Sprintf("#%d carries no card on the project, so there is no %s field to write", number, fieldName)}, nil
	}
	if err := p.api.SetProjectItemSingleSelect(ctx, req.Target.Scope, meta.ProjectID, item.ItemID, meta.FieldID, optionID); err != nil {
		return nil, fmt.Errorf("workmgmt/github: set %s field on #%d: %w", fieldName, number, err)
	}
	return &workmgmt.GroomingMutationResult{Applied: true,
		Observed:         workmgmt.GroomingValue{Scalar: item.Status},
		ProviderResponse: fmt.Sprintf("set %s on #%d to %q", fieldName, number, value)}, nil
}
