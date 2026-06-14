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
	AddProjectItem(ctx context.Context, installationID int64, projectID, contentID string) (string, error)
	SetProjectItemSingleSelect(ctx context.Context, installationID int64, projectID, itemID, fieldID, optionID string) error
	AddSubIssue(ctx context.Context, installationID int64, parentNodeID, childNodeID string) error
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
// and relations. The issue is created first (the durable result); a
// later placement/link failure returns an error naming the step, leaving
// the created issue's URL in the error for recovery. The created issue is
// returned only when every requested step succeeds.
func (p *Provider) File(ctx context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	if p.api == nil {
		return nil, errors.New("workmgmt/github: provider missing API client")
	}
	if req.Target.Repo.Owner == "" || req.Target.Repo.Name == "" {
		return nil, errors.New("workmgmt/github: target repo owner and name required")
	}
	repo := githubclient.RepoRef{Owner: req.Target.Repo.Owner, Name: req.Target.Repo.Name}
	inst := req.Target.InstallationID

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

	if err := p.placeOnBoard(ctx, inst, req, issue); err != nil {
		return nil, fmt.Errorf("%w (issue created: %s)", err, issue.HTMLURL)
	}

	if epic := strings.TrimSpace(req.Item.Relations.ParentEpic); epic != "" {
		if err := p.linkEpic(ctx, inst, repo, epic, issue.NodeID); err != nil {
			return nil, fmt.Errorf("%w (issue created: %s)", err, issue.HTMLURL)
		}
	}

	return created, nil
}

// placeOnBoard adds the created issue to the configured project and sets
// its Status field. No-op when the conventions declare no project.
func (p *Provider) placeOnBoard(ctx context.Context, inst int64, req workmgmt.ProviderRequest, issue *githubclient.CreatedIssue) error {
	proj := req.Target.Project
	if proj == nil {
		return nil
	}
	coord := githubclient.ProjectCoord{Owner: proj.Owner, OwnerType: proj.OwnerType, Number: proj.Number}
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
