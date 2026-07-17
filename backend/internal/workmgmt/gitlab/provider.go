// Package gitlab implements the work-management Provider (ADR-058 Phase 2,
// #1856) against GitLab issues: it resolves the target project, creates the
// issue with the conventions-resolved labels PLUS the board-status label
// (GitLab issue boards are label-driven, so applying the state's label at
// create time IS board placement), then best-effort links the item to its
// parent via the Free-tier issue-links API. The REST calls live in
// backend/internal/gitlabclient; this package is the orchestration that
// turns a resolved workmgmt.ProviderRequest into them.
//
// Only Provider.File is implemented in v0 — the Transitioner (#1012),
// NumberDiscoverer (#1269), and EpicChildrenQuerier (ADR-047) capabilities
// are deliberately NOT implemented, matching the jira sibling. Because
// board placement rides the create as a label, no separate transition call
// exists; the board-sync hook type-asserts Transitioner and yields a no-op.
//
// The mapping decisions (canonical states -> GitLab label names,
// parent_epic -> a Free-tier relates_to issue link rather than a Premium
// group epic) are documented in README.md and docs/spec/work-management-v0.md.
//
// The GitLab instance base URL and token are server-side env
// (FISHHAWKD_GITLAB_*), supplied to the *gitlabclient.Client at construction
// in serve.go; the per-repo conventions gitlab block carries only the
// non-secret optional project override.
package gitlab

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/gitlabclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// ProviderName is the conventions `provider` id this provider registers
// under and echoes into CreatedItem.Provider.
const ProviderName = "gitlab"

// API is the slice of gitlabclient.Client the provider needs, declared as a
// consumer-side interface so the provider can be unit-tested against a fake.
// *gitlabclient.Client satisfies it directly.
type API interface {
	GetProject(ctx context.Context, path string) (*gitlabclient.Project, error)
	CreateIssue(ctx context.Context, projectID int, p gitlabclient.CreateIssueParams) (*gitlabclient.CreatedIssue, error)
	LinkIssues(ctx context.Context, projectID, iid, targetIID int) error
}

// Provider is the GitLab issues work-management provider.
type Provider struct {
	api API
}

// New returns a Provider backed by api (in production *gitlabclient.Client).
func New(api API) *Provider { return &Provider{api: api} }

// Name implements workmgmt.Provider.
func (*Provider) Name() string { return ProviderName }

// File creates the issue and applies the conventions-resolved placement.
//
// The target project is resolved first: the conventions gitlab.project
// override wins, else the filing repo's owner/name path. A GetProject
// failure is fatal (nil item + error) — the numeric project id addresses
// every subsequent call, so the filing cannot proceed without it.
//
// The issue is then created with the conventions-resolved labels PLUS the
// board-status label when BoardPlacement.Status is set: GitLab issue boards
// are label-driven, so the state's label riding the create IS board
// placement (Boarded is true when the label rode the create, false with an
// empty BoardingError when no status was configured). CreateIssue is the
// only other fatal step — no issue exists if it fails.
//
// The parent/epic reference is finally linked best-effort (#1107) via the
// Free-tier issue-links API: a requested parent that fails to parse or link
// records the cause in CreatedItem.EpicLinkError and leaves EpicLinked false
// without failing the filing, while an empty parent leaves EpicLinked false
// with no error. Once CreateIssue succeeds File always returns the durable
// issue with a nil error (matching the jira/github #1107 posture).
func (p *Provider) File(ctx context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	if p.api == nil {
		return nil, errors.New("workmgmt/gitlab: provider missing API client")
	}
	conn := req.Target.GitLab
	if conn == nil {
		return nil, errors.New("workmgmt/gitlab: target gitlab connection required; the conventions must declare a gitlab block")
	}

	projectPath := resolveProjectPath(conn, req.Target.Repo)
	if projectPath == "" {
		return nil, errors.New("workmgmt/gitlab: no target project; set the gitlab.project override or supply a filing repo")
	}

	project, err := p.api.GetProject(ctx, projectPath)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/gitlab: resolve project %q: %w", projectPath, err)
	}

	// GitLab boards are label-driven: the board-status label rides the
	// create alongside the conventions-resolved labels, so applying it IS
	// board placement. Build a fresh slice so req's labels are never mutated.
	status := strings.TrimSpace(req.Item.BoardPlacement.Status)
	labels := appliedLabels(req.Item.Classification.Labels, status)

	issue, err := p.api.CreateIssue(ctx, project.ID, gitlabclient.CreateIssueParams{
		Title:       req.Item.Title,
		Description: req.Item.Body,
		Labels:      labels,
	})
	if err != nil {
		return nil, fmt.Errorf("workmgmt/gitlab: create issue: %w", err)
	}

	created := &workmgmt.CreatedItem{
		Provider:      ProviderName,
		Number:        issue.IID,
		URL:           issue.WebURL,
		AppliedLabels: labels,
		Status:        status,
		BoardColumn:   req.Item.BoardPlacement.BoardColumn,
		// The label rode the create, so a configured status is boarded the
		// moment CreateIssue succeeds; an empty status boards nothing and
		// leaves BoardingError empty (there was nothing to board).
		Boarded: status != "",
	}

	// Epic linking is best-effort (#1107) via a separate post-create call:
	// an empty parent means nothing to link (EpicLinked false, no error); a
	// parse or link failure records the cause in EpicLinkError and leaves
	// EpicLinked false, but the durable issue is still returned. GitLab group
	// epics are Premium-only, so the parent maps to a Free-tier relates_to
	// issue link rather than an epic membership.
	if parent := strings.TrimSpace(req.Item.Relations.ParentEpic); parent != "" {
		targetIID, perr := parseIssueRef(parent)
		if perr != nil {
			created.EpicLinkError = fmt.Sprintf("parse parent epic %q: %v", parent, perr)
		} else if lerr := p.api.LinkIssues(ctx, project.ID, issue.IID, targetIID); lerr != nil {
			created.EpicLinkError = lerr.Error()
		} else {
			created.EpicLinked = true
		}
	}

	return created, nil
}

// resolveProjectPath picks the target GitLab project path: the conventions
// gitlab.project override wins, else the filing repo's owner/name path. A
// zero repo with no override yields "" (the caller fails closed).
func resolveProjectPath(conn *workmgmt.GitLabConnection, repo workmgmt.Repo) string {
	if conn != nil {
		if override := strings.TrimSpace(conn.Project); override != "" {
			return override
		}
	}
	owner := strings.TrimSpace(repo.Owner)
	name := strings.TrimSpace(repo.Name)
	if owner == "" || name == "" {
		return ""
	}
	return owner + "/" + name
}

// appliedLabels returns the conventions-resolved labels with the board-status
// label appended when status is non-empty, without mutating the input slice.
// The appended state label is what makes the create a board placement.
func appliedLabels(base []string, status string) []string {
	if status == "" {
		if len(base) == 0 {
			return nil
		}
		out := make([]string, len(base))
		copy(out, base)
		return out
	}
	out := make([]string, 0, len(base)+1)
	out = append(out, base...)
	out = append(out, status)
	return out
}

// parseIssueRef parses "#123" or "123" into the issue iid. GitLab
// parent_epic references share the numeric-ref semantics of the github/jira
// siblings — a non-numeric or non-positive ref is a link failure the caller
// records best-effort (#1107).
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
