package github

// This file implements the optional workmgmt.WorkItemReader capability
// (#2230 / ADR-064) on the GitHub Projects provider: read one work item by
// reference, or enumerate a query-scoped set.
//
// Enumeration goes through githubclient.ListRepoIssues — the GraphQL
// repository.issues connection, paginated to completion — and NEVER through a
// ProjectV2 board item list, which caps and truncates silently (the AGENTS.md
// Project #7 pagination trap). Board state is a per-issue projectItems read
// layered on top, not the enumeration source.
//
// Every degradation is a TYPED *workmgmt.UnavailableError with a nil result:
// no installation scope, no project configured, a user-owned board with no
// projects token (#1114), or a forge permission refusal. A read must fail
// typed where Transition degrades to a best-effort skip — a silent empty
// answer here is indistinguishable from an empty backlog, which is exactly the
// wrong-decision class the capability exists to prevent.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// unavailable builds the typed capability-unavailable error for this provider.
func unavailable(reason workmgmt.UnavailableReason, detail string, cause error) *workmgmt.UnavailableError {
	return &workmgmt.UnavailableError{
		Provider:   ProviderName,
		Capability: workmgmt.ReaderCapability,
		Reason:     reason,
		Detail:     detail,
		Cause:      cause,
	}
}

// boardContext is the resolved board-read precondition set: whether board
// state was requested at all, the project node id to match items against, and
// the (possibly projects-token-opted-in) context the board calls must use.
type boardContext struct {
	want      bool
	projectID string
	ctx       context.Context
}

// resolveBoard enforces the board-read preconditions shared by both read
// paths and resolves the project node id. It FAILS CLOSED with a typed
// *workmgmt.UnavailableError on each degradation rather than returning a
// board-less result the caller would read as "this item has no board state":
//
//   - target project absent          -> ReasonNoProjectConfigured
//   - user-owned board, no PAT       -> ReasonNoProjectsToken (#1114)
//   - forge refuses the field read   -> ReasonForbidden (cause retained)
//
// When want is false it is a no-op returning the untouched context, so a
// caller that never asked for board state is never failed on a board it does
// not need.
func (p *Provider) resolveBoard(ctx context.Context, want bool, target workmgmt.Target) (boardContext, error) {
	if !want {
		return boardContext{ctx: ctx}, nil
	}
	proj := target.Project
	if proj == nil {
		return boardContext{}, unavailable(workmgmt.ReasonNoProjectConfigured,
			"board state was requested but the target carries no project connection; configure work_management.project in the repo conventions or request the read without board state", nil)
	}
	// User-owned Projects v2 (the Project #7 case) cannot be reached with the
	// App installation token (#1114). Transition degrades to a best-effort skip
	// there; a READ must not — an item silently reported as off-board is a
	// wrong answer, so fail typed and let the caller name the remedy.
	if proj.OwnerType == "user" {
		if !p.api.ProjectsTokenConfigured() {
			return boardContext{}, unavailable(workmgmt.ReasonNoProjectsToken,
				"the project board is user-owned, which a GitHub App installation token cannot reach; set FISHHAWKD_PROJECTS_TOKEN to a project-scoped token", nil)
		}
		ctx = githubclient.WithProjectsToken(ctx)
	}
	coord := githubclient.ProjectCoord{Owner: proj.Owner, OwnerType: proj.OwnerType, Number: proj.Number}
	meta, err := p.api.ProjectFields(ctx, coordScope(target), coord, statusFieldName)
	if err != nil {
		if errors.Is(err, forge.ErrForbidden) {
			return boardContext{}, unavailable(workmgmt.ReasonForbidden,
				"the forge refused the project-fields read; the token needs read access to the project board", err)
		}
		return boardContext{}, fmt.Errorf("workmgmt/github: resolve project fields: %w", err)
	}
	return boardContext{want: true, projectID: meta.ProjectID, ctx: ctx}, nil
}

// coordScope is a tiny readability shim: the credential scope every board call
// authenticates with.
func coordScope(target workmgmt.Target) forge.CredentialScope { return target.Scope }

// preflight validates the request preconditions shared by both read methods.
// The api/repo checks are programming errors (plain errors, File's actionable
// style); a missing installation scope is a TYPED ReasonNoInstallation, so a
// caller distinguishes "we could not authenticate" from "the backlog is empty".
func (p *Provider) preflight(target workmgmt.Target) (forge.RepoRef, error) {
	if p.api == nil {
		return forge.RepoRef{}, errors.New("workmgmt/github: provider missing API client")
	}
	if target.Repo.Owner == "" || target.Repo.Name == "" {
		return forge.RepoRef{}, errors.New("workmgmt/github: target repo owner and name required")
	}
	if target.Scope.IsZero() {
		return forge.RepoRef{}, unavailable(workmgmt.ReasonNoInstallation,
			"no installation id available; work-item reads are run-scoped in v0 — read with a run whose run carries an installation", nil)
	}
	return forge.RepoRef{Owner: target.Repo.Owner, Name: target.Repo.Name}, nil
}

// ListWorkItems implements workmgmt.WorkItemReader: enumerate the repository's
// issues matching the request's filters.
//
// The label filter is forwarded to the forge (GitHub AND-s the `labels:`
// connection argument). The board-state filter CANNOT be pushed down — neither
// the search API nor the repository.issues connection accepts a ProjectV2
// single-select predicate — so it is an honest client-side post-filter over
// the enumerated set, applied after each item's board column is reverse-mapped
// to a canonical state through req.States. An item whose column maps to no
// canonical state is EXCLUDED by a board-state filter and reports an empty
// BoardState, never a guessed one. Limit is applied last, after filtering.
func (p *Provider) ListWorkItems(ctx context.Context, req workmgmt.ListWorkItemsRequest) (*workmgmt.WorkItemPage, error) {
	repo, err := p.preflight(req.Target)
	if err != nil {
		return nil, err
	}
	wantBoard := req.ResolveBoardState || len(req.BoardStates) > 0
	board, err := p.resolveBoard(ctx, wantBoard, req.Target)
	if err != nil {
		return nil, err
	}
	ctx = board.ctx

	var states []string
	if !req.IncludeClosed {
		states = []string{"OPEN"}
	}
	issues, err := p.api.ListRepoIssues(ctx, req.Target.Scope, repo, githubclient.ListRepoIssuesOptions{
		Labels:          req.Labels,
		States:          states,
		ProjectID:       board.projectID,
		WantBoardStatus: board.want,
	})
	if err != nil {
		if errors.Is(err, forge.ErrForbidden) {
			return nil, unavailable(workmgmt.ReasonForbidden,
				"the forge refused the issue enumeration; the token needs read access to the repository's issues", err)
		}
		return nil, fmt.Errorf("workmgmt/github: list issues for %s/%s: %w", repo.Owner, repo.Name, err)
	}

	keep := canonicalStateSet(req.BoardStates)
	items := make([]workmgmt.WorkItemRecord, 0, len(issues))
	for _, iss := range issues {
		rec := workmgmt.WorkItemRecord{
			Number:      iss.Number,
			Title:       iss.Title,
			URL:         iss.URL,
			Body:        iss.Body,
			Labels:      iss.Labels,
			State:       iss.State,
			StateReason: iss.StateReason,
			// ListRepoIssues carries the UPPERCASE GraphQL IssueState /
			// IssueStateReason enums, matching ListSubIssues. Complete =
			// closed-AND-completed; a not_planned/duplicate close is NOT complete.
			Complete:    iss.State == "CLOSED" && iss.StateReason == "COMPLETED",
			OnBoard:     iss.OnBoard,
			BoardColumn: iss.BoardStatus,
		}
		if board.want && iss.OnBoard {
			rec.BoardState = workmgmt.CanonicalStateForOption(req.States, iss.BoardStatus)
		}
		if keep != nil && !keep[rec.BoardState] {
			continue
		}
		items = append(items, rec)
		if req.Limit > 0 && len(items) >= req.Limit {
			break
		}
	}
	return &workmgmt.WorkItemPage{Items: items, BoardStateResolved: board.want}, nil
}

// canonicalStateSet builds the board-state filter set, or nil when no filter
// was requested. An empty canonical state is never a member, so an off-board
// or unmapped-column item can never satisfy a filter.
func canonicalStateSet(states []string) map[string]bool {
	if len(states) == 0 {
		return nil
	}
	set := make(map[string]bool, len(states))
	for _, s := range states {
		if s = strings.TrimSpace(s); s != "" {
			set[s] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// ReadWorkItem implements workmgmt.WorkItemReader: resolve ONE work item by
// reference. Ref accepts `#N`, `N`, or `issue:N` — the repo's existing ref
// conventions, parsed by the SAME parseIssueRef helper ResolveDependencies
// uses (no second regex).
//
// It reaches the same degradations ListWorkItems does — no installation scope,
// and (only when ResolveBoardState is set) no project configured, a user-owned
// board with no projects token, or a forge refusal — each returning a NIL
// record with a typed *workmgmt.UnavailableError, never a zero-valued record a
// caller would read as a real item.
func (p *Provider) ReadWorkItem(ctx context.Context, req workmgmt.ReadWorkItemRequest) (*workmgmt.WorkItemRecord, error) {
	repo, err := p.preflight(req.Target)
	if err != nil {
		return nil, err
	}
	number, err := parseIssueRef(strings.TrimPrefix(strings.TrimSpace(req.Ref), "issue:"))
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: work item %q: %w", req.Ref, err)
	}
	board, err := p.resolveBoard(ctx, req.ResolveBoardState, req.Target)
	if err != nil {
		return nil, err
	}
	ctx = board.ctx

	issue, err := p.api.GetIssue(ctx, req.Target.Scope, repo, number)
	if err != nil {
		if errors.Is(err, forge.ErrForbidden) {
			return nil, unavailable(workmgmt.ReasonForbidden,
				"the forge refused the issue read; the token needs read access to the repository's issues", err)
		}
		return nil, fmt.Errorf("workmgmt/github: get issue #%d: %w", number, err)
	}
	rec := &workmgmt.WorkItemRecord{
		Number:      issue.Number,
		Title:       issue.Title,
		Body:        issue.Body,
		Labels:      issue.Labels,
		State:       issue.State,
		StateReason: issue.StateReason,
		// GetIssue is the REST payload, whose state/state_reason are LOWERCASE
		// ("closed"/"completed") — unlike the GraphQL enums the list path carries
		// — so match case-insensitively, exactly as ResolveDependencies does.
		Complete: strings.EqualFold(issue.State, "closed") && strings.EqualFold(issue.StateReason, "completed"),
	}
	if !board.want {
		return rec, nil
	}
	nodeID, err := p.api.IssueNodeID(ctx, req.Target.Scope, repo, number)
	if err != nil {
		if errors.Is(err, forge.ErrForbidden) {
			return nil, unavailable(workmgmt.ReasonForbidden,
				"the forge refused the issue node-id read; the token needs read access to the repository's issues", err)
		}
		return nil, fmt.Errorf("workmgmt/github: resolve issue #%d: %w", number, err)
	}
	item, err := p.api.ProjectItemStatus(ctx, req.Target.Scope, nodeID, board.projectID, statusFieldName)
	if err != nil {
		if errors.Is(err, forge.ErrForbidden) {
			return nil, unavailable(workmgmt.ReasonForbidden,
				"the forge refused the project-item read; the token needs read access to the project board", err)
		}
		return nil, fmt.Errorf("workmgmt/github: read project item status for #%d: %w", number, err)
	}
	rec.OnBoard = item.OnBoard
	rec.BoardColumn = item.Status
	if item.OnBoard {
		rec.BoardState = workmgmt.CanonicalStateForOption(req.States, item.Status)
	}
	return rec, nil
}
