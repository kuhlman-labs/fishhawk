package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// FileIssueRelations mirrors the work-item relations sub-object on the
// tool input — the provider-neutral links the conventions layer resolves
// into provider link operations (epic roll-up, supersession, companion,
// evidence runs).
type FileIssueRelations struct {
	ParentEpic   string   `json:"parent_epic,omitempty" jsonschema:"epic this item rolls up to (e.g. '#1005'); resolved into the provider's epic-link operation"`
	Supersedes   []string `json:"supersedes,omitempty" jsonschema:"items this one supersedes"`
	CompanionTo  []string `json:"companion_to,omitempty" jsonschema:"items this one is a companion to"`
	EvidenceRuns []string `json:"evidence_runs,omitempty" jsonschema:"Fishhawk run ids that motivated this item, recorded as evidence"`
}

// FileIssueInput is the fishhawk_file_issue tool's input schema (#1005).
// Only type + summary are required; repo and run_id fall back to the
// GITHUB_REPOSITORY / FISHHAWK_RUN_ID env when omitted (the in-runner
// case), mirroring fishhawk_get_active_run's resolver. Everything else is
// conventions-resolved server-side.
type FileIssueInput struct {
	Type            string              `json:"type" jsonschema:"work-item type; must be a key in the repo's conventions (e.g. feature, bug, chore, adr)"`
	Summary         string              `json:"summary" jsonschema:"mandatory one-liner: fills the {summary} title placeholder and is the required Summary field"`
	Body            string              `json:"body,omitempty" jsonschema:"verbatim body; when omitted the body is assembled from the type's skeleton plus sections"`
	Repo            string              `json:"repo,omitempty" jsonschema:"target repo as owner/name; falls back to GITHUB_REPOSITORY env when omitted"`
	Sections        map[string]string   `json:"sections,omitempty" jsonschema:"per-skeleton-section content keyed by section name; used only when body is empty. Keys MUST match the type's body skeleton exactly — an off-skeleton key fails the filing with work_item_invalid (the content is never silently dropped)"`
	TitleVars       map[string]string   `json:"title_vars,omitempty" jsonschema:"title placeholders beyond {summary}/{number} (e.g. epic, n); an unresolved placeholder fails the filing. For a child type whose title_format is [E{epic}.{n}], the {epic} placeholder is auto-derived from the parent_epic relation, so you need only supply {n}"`
	Labels          []string            `json:"labels,omitempty" jsonschema:"labels merged on top of the type's default_labels"`
	Complexity      string              `json:"complexity,omitempty" jsonschema:"overrides the type's default complexity; must be a declared level (e.g. low, medium, high)"`
	Status          string              `json:"status,omitempty" jsonschema:"overrides the type's default board status/column"`
	Relations       *FileIssueRelations `json:"relations,omitempty" jsonschema:"provider-neutral relations: parent epic, supersedes, companion, evidence runs"`
	ExistingNumbers []int               `json:"existing_numbers,omitempty" jsonschema:"numbers already in use for a numbered type (e.g. adr), so the next sequential number can be allocated"`
	RunID           string              `json:"run_id,omitempty" jsonschema:"optional in-flight run UUID; when set and non-terminal a work_item_filed audit entry is appended to it. Falls back to FISHHAWK_RUN_ID env when omitted"`
}

// FileIssueOutput wraps the created item. Kept under an `item` key so the
// client indexes on the same shape the CLI verb prints.
type FileIssueOutput struct {
	Item FiledWorkItem `json:"item"`
}

// registerFileIssue wires the fishhawk_file_issue tool (#1005): the
// operator/agent path to file a work item through the repo's
// work-management conventions, consistently across repos and platforms.
//
// Auth: a write tool — the backend requires an authenticated caller and
// rejects anonymous requests. The same call shape works against a
// Jira-configured repo because only the per-repo conventions differ (the
// concrete Jira provider is interface-only in v0; an unimplemented provider
// id fails closed with provider_unimplemented).
func registerFileIssue(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_file_issue",
		Description: strings.TrimSpace(`
Use this when you need to file a work item (issue, bug, chore, ADR) into a
repository's tracker through its work-management conventions — the
operator-agent follow-up-filing path (ADR-040) and the consistent
cross-repo/cross-platform filing surface (#1005). It wraps
POST /v0/work-items: the backend loads the repo's conventions, renders the
title, assembles the body from the type's skeleton, merges default + explicit
labels, resolves board placement / complexity / ADR numbering, links
relations, and dispatches to the registered provider (GitHub Projects in v0).

Inputs: type + summary are required; repo and run_id fall back to
GITHUB_REPOSITORY / FISHHAWK_RUN_ID env when omitted (the in-runner case).
body is optional — when omitted the body is assembled from the type's
skeleton plus per-section content; sections keys must match the type's body
skeleton exactly (an off-skeleton key fails work_item_invalid rather than
being silently dropped). relations carries the parent epic, supersedes,
companion, and evidence-run links. For a child type whose title_format is
[E{epic}.{n}], the {epic} placeholder is auto-derived from the parent_epic
relation, so title_vars need only supply {n}; a 422 work_item_invalid lists
the still-missing placeholders in details.missing_placeholders.

When run_id names an in-flight, non-terminal run a best-effort
work_item_filed audit entry is appended to it; filing still succeeds with no
run in flight (the audited flag in the response reports whether an entry was
written).

Returns the created item: type, title, number, url, provider, the resolved
applied_labels / complexity / status / board_column, boarded / epic_linked
(whether the best-effort board placement and epic link landed; false with a
boarding_error / epic_link_error when they did not — the issue is still
filed), and audited. Board placement / epic linking are best-effort and no
longer fail the filing; work_item_filing_failed (502) is reserved for a
create-issue or installation-resolution failure (no durable issue exists),
and the provider cause is surfaced in the tool error. Tool errors:
validation_failed (400), authentication_required (401), work_item_invalid
(422 — the request violates the type's conventions), provider_unimplemented
(501 — the configured provider id is not registered, e.g. the interface-only
jira), work_item_filing_failed (502).
`),
	}, resolver.fileIssue)
}

// fileIssue is the tool handler. It validates type + summary locally (a
// fast fail before the HTTP hop), resolves repo / run_id from the env when
// omitted, and delegates conventions-application + provider dispatch + audit
// to the backend (server/workitems.go).
func (r *runResolver) fileIssue(ctx context.Context, _ *mcp.CallToolRequest, in FileIssueInput) (*mcp.CallToolResult, FileIssueOutput, error) {
	if strings.TrimSpace(in.Type) == "" {
		return nil, FileIssueOutput{}, fmt.Errorf("type is required: name the work-item type (a key in the repo's conventions, e.g. feature, bug, chore, adr)")
	}
	if strings.TrimSpace(in.Summary) == "" {
		return nil, FileIssueOutput{}, fmt.Errorf("summary is required: the one-line summary fills the title and is the required Summary field")
	}

	repo := in.Repo
	if repo == "" {
		repo = r.getenv("GITHUB_REPOSITORY")
	}
	if strings.TrimSpace(repo) == "" {
		return nil, FileIssueOutput{}, fmt.Errorf("repo is required: pass repo as owner/name or set GITHUB_REPOSITORY in the environment")
	}

	runID := in.RunID
	if runID == "" {
		runID = r.getenv("FISHHAWK_RUN_ID")
	}

	req := FileWorkItemRequest{
		Repo:            strings.TrimSpace(repo),
		Type:            strings.TrimSpace(in.Type),
		Summary:         in.Summary,
		Body:            in.Body,
		Sections:        in.Sections,
		TitleVars:       in.TitleVars,
		Labels:          in.Labels,
		Complexity:      in.Complexity,
		Status:          in.Status,
		ExistingNumbers: in.ExistingNumbers,
		RunID:           strings.TrimSpace(runID),
	}
	if in.Relations != nil {
		req.Relations = &WorkItemRelations{
			ParentEpic:   in.Relations.ParentEpic,
			Supersedes:   in.Relations.Supersedes,
			CompanionTo:  in.Relations.CompanionTo,
			EvidenceRuns: in.Relations.EvidenceRuns,
		}
	}

	item, err := r.api.FileWorkItem(ctx, req)
	if err != nil {
		// Surface the backend's details.error on the remaining genuine 502
		// paths (CreateIssue / installation-resolution failure) so the
		// operator gets the provider cause instead of a bare
		// "HTTP 502 (work_item_filing_failed)" (#1107). Mirrors the
		// Details-extraction precedent in resume_run.go / tools.go.
		var ae *apiError
		if errors.As(err, &ae) {
			if cause, ok := ae.Details["error"].(string); ok && cause != "" {
				return nil, FileIssueOutput{}, fmt.Errorf("file work item: %w: %s", err, cause)
			}
		}
		return nil, FileIssueOutput{}, fmt.Errorf("file work item: %w", err)
	}
	return nil, FileIssueOutput{Item: *item}, nil
}
