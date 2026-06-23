// Package jira implements the work-management Provider (#1094, deferred
// from #1005) against Jira Cloud: it creates the issue (labels applied at
// creation), best-effort links it to a parent/epic via a post-create edit
// on the conventions-configured field, and best-effort moves it to the
// conventions' board status via a workflow transition. The REST calls live
// in backend/internal/jiraclient; this package is the orchestration that
// turns a resolved workmgmt.ProviderRequest into them.
//
// Only Provider.File is implemented in v0. The board-state Transitioner
// capability (#1012) is intentionally NOT implemented for jira — the
// board-sync hook type-asserts it and yields a no-op move for jira.
//
// The Jira instance base URL and credentials are server-side env
// (FISHHAWKD_JIRA_*), supplied to the *jiraclient.Client at construction
// in serve.go; the per-repo conventions block carries only the non-secret
// project selection (project_key + optional issue-type map).
package jira

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/jiraclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// ProviderName is the conventions `provider` id this provider registers
// under and echoes into CreatedItem.Provider.
const ProviderName = "jira"

// API is the slice of jiraclient.Client the provider needs, declared as a
// consumer-side interface so the provider can be unit-tested against a
// fake. *jiraclient.Client satisfies it directly.
type API interface {
	CreateIssue(ctx context.Context, p jiraclient.CreateIssueParams) (*jiraclient.CreatedIssue, error)
	LinkParent(ctx context.Context, issueKey, fieldName, epicKey string) error
	Transition(ctx context.Context, key, targetStatusName string) error
}

// Provider is the Jira Cloud work-management provider.
type Provider struct {
	api API
}

// New returns a Provider backed by api (in production *jiraclient.Client).
func New(api API) *Provider { return &Provider{api: api} }

// Name implements workmgmt.Provider.
func (*Provider) Name() string { return ProviderName }

// File creates the issue and applies the conventions-resolved placement.
// The issue is created first — it is the durable result and the only fatal
// step: a CreateIssue failure (or a failed pre-create guard) returns a nil
// item and an error, because no issue exists. The parent/epic reference is
// then linked best-effort (#1107) via a separate post-create LinkParent
// edit on the conventions-configured field (the default `parent` reference
// for team-managed projects, or a classic epic-link custom field id): a
// requested parent that fails to link records the cause in
// CreatedItem.EpicLinkError and leaves EpicLinked false without failing the
// filing, while an empty parent leaves EpicLinked false with no error.
// Board placement is a best-effort workflow transition (#1107): a created
// issue lands in the project's default status, and reaching the
// conventions' status requires a separate transition call — once the issue
// exists File always returns it with a nil error, recording whether the
// transition landed in CreatedItem.Boarded and the cause in BoardingError
// when it did not (matching the github provider's #1107 posture).
func (p *Provider) File(ctx context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	if p.api == nil {
		return nil, errors.New("workmgmt/jira: provider missing API client")
	}
	conn := req.Target.Jira
	if conn == nil {
		return nil, errors.New("workmgmt/jira: target jira connection required; the conventions must declare a jira block")
	}
	if strings.TrimSpace(conn.ProjectKey) == "" {
		return nil, errors.New("workmgmt/jira: jira connection missing project_key")
	}

	parentKey := strings.TrimSpace(req.Item.Relations.ParentEpic)
	issue, err := p.api.CreateIssue(ctx, jiraclient.CreateIssueParams{
		ProjectKey:  conn.ProjectKey,
		IssueType:   issueTypeFor(conn, req.Item.Type),
		Summary:     req.Item.Title,
		Description: req.Item.Body,
		Labels:      req.Item.Classification.Labels,
	})
	if err != nil {
		return nil, fmt.Errorf("workmgmt/jira: create issue: %w", err)
	}

	created := &workmgmt.CreatedItem{
		Provider:      ProviderName,
		Number:        numberFromKey(issue.Key),
		URL:           issue.URL,
		AppliedLabels: req.Item.Classification.Labels,
		Status:        req.Item.BoardPlacement.Status,
		BoardColumn:   req.Item.BoardPlacement.BoardColumn,
	}

	// Epic linking is best-effort (#1107) via a separate post-create edit: an
	// empty parent means nothing to link (EpicLinked false, no error); a link
	// failure records the cause in EpicLinkError and leaves EpicLinked false,
	// but the durable issue is still returned. The field defaults to the
	// team-managed `parent` reference; a classic project configures its
	// epic-link custom field id via conn.ParentField.
	if parentKey != "" {
		parentField := strings.TrimSpace(conn.ParentField)
		if parentField == "" {
			parentField = "parent"
		}
		if lerr := p.api.LinkParent(ctx, issue.Key, parentField, parentKey); lerr != nil {
			created.EpicLinkError = lerr.Error()
		} else {
			created.EpicLinked = true
		}
	}

	// Board placement is best-effort (#1107): no configured status means
	// nothing to move (leave Boarded false with no error); a transition
	// failure records the cause and leaves the durable issue intact.
	status := strings.TrimSpace(req.Item.BoardPlacement.Status)
	if status == "" {
		created.Boarded = false
	} else if terr := p.api.Transition(ctx, issue.Key, status); terr != nil {
		created.BoardingError = terr.Error()
	} else {
		created.Boarded = true
	}

	return created, nil
}

// issueTypeFor maps a canonical work-item type to the Jira issue-type name.
// An explicit issue_types entry wins; otherwise the canonical type is
// title-cased (bug -> Bug), matching Jira's default issue-type naming.
func issueTypeFor(conn *workmgmt.JiraConnection, canonicalType string) string {
	if conn != nil {
		if name, ok := conn.IssueTypes[canonicalType]; ok && strings.TrimSpace(name) != "" {
			return name
		}
	}
	return titleCase(canonicalType)
}

// titleCase upper-cases the first rune and lower-cases the rest, the
// default issue-type fallback. Empty input is returned unchanged.
func titleCase(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + strings.ToLower(string(r[1:]))
}

// numberFromKey extracts the numeric suffix of a Jira issue key
// (ENG-1234 -> 1234) for CreatedItem.Number, which is provider-agnostic
// and integer-typed. The full key is always preserved in CreatedItem.URL
// (the browse URL), so a non-numeric or malformed key simply yields 0
// rather than failing the filing.
func numberFromKey(key string) int {
	idx := strings.LastIndex(key, "-")
	if idx < 0 || idx == len(key)-1 {
		return 0
	}
	n, err := strconv.Atoi(key[idx+1:])
	if err != nil {
		return 0
	}
	return n
}
