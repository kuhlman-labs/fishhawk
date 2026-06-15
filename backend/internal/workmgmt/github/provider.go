// Package github implements the work-management Provider (#1005) against
// GitHub Projects: it creates the issue (labels applied at creation),
// adds it to the configured project board, sets the single-select Status
// field, and links the parent epic as a sub-issue. The GraphQL/REST calls
// live in backend/internal/githubclient (projects.go); this package is
// the orchestration that turns a resolved workmgmt.ProviderRequest into
// those calls.
package github

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// ProviderName is the conventions `provider` id this provider registers
// under and echoes into CreatedItem.Provider.
const ProviderName = "github_projects"

// statusFieldName is the conventional single-select board field the
// provider sets from BoardPlacement.Status.
const statusFieldName = "Status"

// API is the slice of githubclient.Client the provider needs, declared as
// a consumer-side interface so the provider can be unit-tested against a
// fake. *githubclient.Client satisfies it.
type API interface {
	CreateIssue(ctx context.Context, installationID int64, repo githubclient.RepoRef, p githubclient.CreateIssueParams) (*githubclient.CreatedIssue, error)
	IssueNodeID(ctx context.Context, installationID int64, repo githubclient.RepoRef, number int) (string, error)
	ProjectFields(ctx context.Context, installationID int64, coord githubclient.ProjectCoord, fieldName string) (*githubclient.ProjectMeta, error)
	ProjectItemStatus(ctx context.Context, installationID int64, issueNodeID, projectID, fieldName string) (*githubclient.ProjectItemStatus, error)
	AddProjectItem(ctx context.Context, installationID int64, projectID, contentID string) (string, error)
	SetProjectItemSingleSelect(ctx context.Context, installationID int64, projectID, itemID, fieldID, optionID string) error
	AddSubIssue(ctx context.Context, installationID int64, parentNodeID, childNodeID string) error
	ProjectsTokenConfigured() bool
}

// Provider is the GitHub Projects work-management provider.
type Provider struct {
	api API
}

// New returns a Provider backed by api (in production *githubclient.Client).
func New(api API) *Provider { return &Provider{api: api} }

// Name implements workmgmt.Provider.
func (*Provider) Name() string { return ProviderName }

// File creates the issue and applies the conventions-resolved placement
// and relations. The issue is created first — it is the durable result and
// the only fatal step: a CreateIssue failure (or a failed pre-create
// guard) returns a nil item and an error, because no issue exists. Board
// placement and epic linking are best-effort (#1107): once the issue
// exists File always returns it with a nil error, recording whether the
// enrichment landed in CreatedItem.Boarded / EpicLinked and the cause in
// BoardingError / EpicLinkError when it did not. The server logs those
// causes and echoes them in the response so a real misconfiguration stays
// diagnosable while a placement failure no longer orphans a created issue.
func (p *Provider) File(ctx context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	if p.api == nil {
		return nil, errors.New("workmgmt/github: provider missing API client")
	}
	if req.Target.Repo.Owner == "" || req.Target.Repo.Name == "" {
		return nil, errors.New("workmgmt/github: target repo owner and name required")
	}
	repo := githubclient.RepoRef{Owner: req.Target.Repo.Owner, Name: req.Target.Repo.Name}
	inst := req.Target.InstallationID
	// Fail closed when no installation id is available (#1005 concern-2).
	// On the run-absent filing path Target.InstallationID stays 0, so the
	// client cannot mint an installation token; proceeding would fail
	// opaquely deep inside the first REST call. GitHub Projects filing is
	// run-scoped in v0 — name the missing context and the constraint here
	// instead. A run-absent installation source is a follow-up.
	if inst == 0 {
		return nil, errors.New("workmgmt/github: no installation id available; GitHub Projects filing is run-scoped in v0 — file with a run_id whose run carries an installation, or use a provider that needs no installation token")
	}

	issue, err := p.api.CreateIssue(ctx, inst, repo, githubclient.CreateIssueParams{
		Title:  req.Item.Title,
		Body:   req.Item.Body,
		Labels: req.Item.Classification.Labels,
	})
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: create issue: %w", err)
	}

	created := &workmgmt.CreatedItem{
		Provider:      ProviderName,
		Number:        issue.Number,
		URL:           issue.HTMLURL,
		AppliedLabels: req.Item.Classification.Labels,
		Status:        req.Item.BoardPlacement.Status,
		BoardColumn:   req.Item.BoardPlacement.BoardColumn,
	}

	// Board placement is best-effort (#1107): the issue is the durable
	// result, so a placement failure records the cause and leaves Boarded
	// false rather than discarding the created issue. No project configured
	// means nothing to board — leave Boarded false with no error.
	if req.Target.Project == nil {
		created.Boarded = false
	} else if err := p.placeOnBoard(ctx, inst, req, issue); err != nil {
		created.BoardingError = err.Error()
	} else {
		created.Boarded = true
	}

	// Epic linking is best-effort too; an empty parent epic means nothing
	// to link (leave EpicLinked false with no error).
	if epic := strings.TrimSpace(req.Item.Relations.ParentEpic); epic != "" {
		if err := p.linkEpic(ctx, inst, repo, epic, issue.NodeID); err != nil {
			created.EpicLinkError = err.Error()
		} else {
			created.EpicLinked = true
		}
	}

	return created, nil
}

// Transition moves an already-filed issue's board Status along a
// run-lifecycle edge (#1012). It resolves the issue node id, the project's
// Status field + options, and the issue's current project item, then:
//   - SKIPS (no mutation) when no project is configured, the issue is not on
//     the board, the target canonical state has no configured/board option,
//     or — the never-fight-the-human guard — the card's current status is
//     not in the request's expected source set. An unset status counts as
//     Backlog so a fresh card still advances on run_started.
//   - otherwise sets the Status single-select to the target option and
//     reports Moved with from->to.
//
// Genuine provider failures (issue/field resolution, the status read, the
// set mutation) return an error; the lifecycle hook logs it best-effort and
// never unwinds the run. Only the Status column is touched — never labels,
// fields, or epic links (the #1005 scope split).
func (p *Provider) Transition(ctx context.Context, req workmgmt.TransitionRequest) (*workmgmt.TransitionResult, error) {
	if p.api == nil {
		return nil, errors.New("workmgmt/github: provider missing API client")
	}
	if req.Target.Repo.Owner == "" || req.Target.Repo.Name == "" {
		return nil, errors.New("workmgmt/github: target repo owner and name required")
	}
	proj := req.Target.Project
	if proj == nil {
		return &workmgmt.TransitionResult{Skipped: true, SkipReason: "no project configured"}, nil
	}
	if req.IssueNumber <= 0 {
		return nil, errors.New("workmgmt/github: transition requires a positive issue number")
	}
	inst := req.Target.InstallationID
	if inst == 0 {
		return nil, errors.New("workmgmt/github: no installation id available; board transitions are run-scoped in v0")
	}

	// Resolve the target board option from the canonical state via the
	// conventions' states map. An unmapped canonical state is a no-op skip,
	// not an error — the config simply doesn't bind that state to a column.
	toOption := strings.TrimSpace(req.States[req.CanonicalState])
	if toOption == "" {
		return &workmgmt.TransitionResult{Skipped: true,
			SkipReason: fmt.Sprintf("canonical state %q has no configured provider option", req.CanonicalState)}, nil
	}

	coord := githubclient.ProjectCoord{Owner: proj.Owner, OwnerType: proj.OwnerType, Number: proj.Number}
	// User-owned Projects v2 (the Project #7 case) cannot be reached with the
	// App installation token (#1114). With no projects token configured the
	// installation-token fallback would error on every board GraphQL call, and
	// that error would drop the mandated work_item_transitioned audit — so
	// degrade to a best-effort SKIP (the #1107/#1114 posture: never an error)
	// before dispatching anything. With a projects token configured, opt the
	// board GraphQL calls into it.
	if proj.OwnerType == "user" {
		if !p.api.ProjectsTokenConfigured() {
			return &workmgmt.TransitionResult{Skipped: true, To: toOption,
				SkipReason: "user-owned project board unreachable: no projects token configured"}, nil
		}
		ctx = githubclient.WithProjectsToken(ctx)
	}
	repo := githubclient.RepoRef{Owner: req.Target.Repo.Owner, Name: req.Target.Repo.Name}

	issueNodeID, err := p.api.IssueNodeID(ctx, inst, repo, req.IssueNumber)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: resolve issue #%d: %w", req.IssueNumber, err)
	}
	meta, err := p.api.ProjectFields(ctx, inst, coord, statusFieldName)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: resolve project fields: %w", err)
	}
	optionID, ok := meta.StatusOptions[toOption]
	if !ok {
		return &workmgmt.TransitionResult{Skipped: true,
			SkipReason: fmt.Sprintf("target status %q is not a %s option on the project", toOption, statusFieldName)}, nil
	}

	item, err := p.api.ProjectItemStatus(ctx, inst, issueNodeID, meta.ProjectID, statusFieldName)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: read project item status: %w", err)
	}
	if !item.OnBoard {
		return &workmgmt.TransitionResult{Skipped: true, To: toOption,
			SkipReason: "issue is not on the project board"}, nil
	}
	current := item.Status
	// never-fight-the-human: only advance from an expected source status. A
	// card a human parked elsewhere (e.g. Blocked) is left untouched.
	if !sourceAllows(current, req) {
		return &workmgmt.TransitionResult{Skipped: true, From: current, To: toOption,
			SkipReason: fmt.Sprintf("current status %q is not in the expected source set", labelOrUnset(current))}, nil
	}
	if current == toOption {
		return &workmgmt.TransitionResult{Skipped: true, From: current, To: toOption,
			SkipReason: "card already at target status"}, nil
	}
	if err := p.api.SetProjectItemSingleSelect(ctx, inst, meta.ProjectID, item.ItemID, meta.FieldID, optionID); err != nil {
		return nil, fmt.Errorf("workmgmt/github: set status field: %w", err)
	}
	return &workmgmt.TransitionResult{Moved: true, From: current, To: toOption}, nil
}

// sourceAllows reports whether the card's current board status is an
// expected source for the move. The expected source canonical states are
// resolved to board options through the request's states map; an unset
// current status (a fresh/un-triaged card) counts as Backlog so it still
// advances when backlog is an expected source (run_started's unset/Backlog).
func sourceAllows(current string, req workmgmt.TransitionRequest) bool {
	for _, s := range req.ExpectedSourceStates {
		if current == "" && s == workmgmt.CanonicalStateBacklog {
			return true
		}
		if opt := strings.TrimSpace(req.States[s]); opt != "" && current == opt {
			return true
		}
	}
	return false
}

// labelOrUnset renders an empty status as "(unset)" for skip-reason text.
func labelOrUnset(status string) string {
	if status == "" {
		return "(unset)"
	}
	return status
}

// placeOnBoard adds the created issue to the configured project and sets
// its Status field. No-op when the conventions declare no project.
func (p *Provider) placeOnBoard(ctx context.Context, inst int64, req workmgmt.ProviderRequest, issue *githubclient.CreatedIssue) error {
	proj := req.Target.Project
	if proj == nil {
		return nil
	}
	coord := githubclient.ProjectCoord{Owner: proj.Owner, OwnerType: proj.OwnerType, Number: proj.Number}
	// User-owned Projects v2 boards (the Project #7 case) cannot be written
	// with the App installation token — there is no user-projects permission
	// for GitHub Apps (#1114). Opt the three board-placement GraphQL calls
	// into the static projects token via the request-scoped flag; the client
	// honors it only when a projects token is configured, so this stays the
	// #1107 best-effort boarded:false path when it is not. Org-owned projects
	// and the repo-scoped epic link (AddSubIssue) stay on the installation
	// token.
	if proj.OwnerType == "user" {
		ctx = githubclient.WithProjectsToken(ctx)
	}
	meta, err := p.api.ProjectFields(ctx, inst, coord, statusFieldName)
	if err != nil {
		return fmt.Errorf("workmgmt/github: resolve project fields: %w", err)
	}
	itemID, err := p.api.AddProjectItem(ctx, inst, meta.ProjectID, issue.NodeID)
	if err != nil {
		return fmt.Errorf("workmgmt/github: add project item: %w", err)
	}
	status := strings.TrimSpace(req.Item.BoardPlacement.Status)
	if status == "" {
		return nil
	}
	optionID, ok := meta.StatusOptions[status]
	if !ok {
		return fmt.Errorf("workmgmt/github: status %q is not a %s option on the project; available: %s",
			status, statusFieldName, strings.Join(sortedKeys(meta.StatusOptions), ", "))
	}
	if err := p.api.SetProjectItemSingleSelect(ctx, inst, meta.ProjectID, itemID, meta.FieldID, optionID); err != nil {
		return fmt.Errorf("workmgmt/github: set status field: %w", err)
	}
	return nil
}

// linkEpic resolves the parent-epic reference (#N or N) to its node id
// and links the new issue as its sub-issue.
func (p *Provider) linkEpic(ctx context.Context, inst int64, repo githubclient.RepoRef, epicRef, childNodeID string) error {
	number, err := parseIssueRef(epicRef)
	if err != nil {
		return fmt.Errorf("workmgmt/github: parent epic %q: %w", epicRef, err)
	}
	parentNodeID, err := p.api.IssueNodeID(ctx, inst, repo, number)
	if err != nil {
		return fmt.Errorf("workmgmt/github: resolve parent epic #%d: %w", number, err)
	}
	if err := p.api.AddSubIssue(ctx, inst, parentNodeID, childNodeID); err != nil {
		return fmt.Errorf("workmgmt/github: link parent epic #%d: %w", number, err)
	}
	return nil
}

// parseIssueRef parses "#123" or "123" into the issue number.
func parseIssueRef(ref string) (int, error) {
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ref), "#"))
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("not a numeric issue reference")
	}
	if n <= 0 {
		return 0, fmt.Errorf("issue number must be > 0")
	}
	return n, nil
}

// sortedKeys returns the sorted keys of a string-keyed map, for stable
// error messages.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
