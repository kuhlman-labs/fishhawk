package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// PreviewIssueInput is the fishhawk_preview_issue tool's input schema
// (#3774): fishhawk_file_issue's inputs minus run_id. A preview writes no
// audit and is not run-scoped, so there is no run to name, and the tool never
// reads FISHHAWK_RUN_ID. repo falls back to GITHUB_REPOSITORY when omitted.
type PreviewIssueInput struct {
	Type            string              `json:"type" jsonschema:"work-item type; must be a key in the repo's conventions (e.g. feature, bug, chore, adr, epic)"`
	Summary         string              `json:"summary" jsonschema:"mandatory one-liner: fills the {summary} title placeholder and is the required Summary field"`
	Body            string              `json:"body,omitempty" jsonschema:"verbatim body; when omitted the body is assembled from the type's skeleton plus sections"`
	Repo            string              `json:"repo,omitempty" jsonschema:"target repo as owner/name; falls back to GITHUB_REPOSITORY env when omitted"`
	Sections        map[string]string   `json:"sections,omitempty" jsonschema:"per-skeleton-section content keyed by section name; used only when body is empty. Keys MUST match the type's body skeleton exactly"`
	TitleVars       map[string]string   `json:"title_vars,omitempty" jsonschema:"title placeholders beyond {summary}/{number} (e.g. epic, n); for a child type {epic} and {n} are auto-derived from relations.parent_epic exactly as a filing derives them"`
	Labels          []string            `json:"labels,omitempty" jsonschema:"labels merged on top of the type's default_labels"`
	Complexity      string              `json:"complexity,omitempty" jsonschema:"overrides the type's default complexity; must be a declared level (e.g. low, medium, high)"`
	Status          string              `json:"status,omitempty" jsonschema:"overrides the type's default board status/column"`
	Relations       *FileIssueRelations `json:"relations,omitempty" jsonschema:"provider-neutral relations: parent epic, supersedes, companion, evidence runs, depends_on"`
	ExistingNumbers []int               `json:"existing_numbers,omitempty" jsonschema:"OPTIONAL for a numbered type: override/hint server-side number discovery, exactly as on fishhawk_file_issue"`
	SourceRefs      []string            `json:"source_refs,omitempty" jsonschema:"same-repo refs ('#N' or 'N') to the existing items this draft derives from; they are EXCLUDED from the intake duplicate candidates and reported as intake.derives_from instead. A malformed ref (including owner/repo#N) is refused 400 validation_failed"`
}

// PreviewIssueOutput wraps the preview under a `preview` key.
type PreviewIssueOutput struct {
	Preview WorkItemPreview `json:"preview"`
}

// registerPreviewIssue wires the fishhawk_preview_issue tool (#3774): the
// operator's look at exactly what a fishhawk_file_issue call would file —
// conventions applied, intake hook run — without filing it.
//
// Auth: the REST route refuses a run-bound token (403 preview_operator_only),
// because a preview reads tracker titles without filing anything and ADR-064
// keeps that read off the agent surface. The MCP scope table admits the
// bearer (mcpScopeAuthenticatedOnly, fishhawk_file_issue's posture) and the
// handler does the refusing.
func registerPreviewIssue(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_preview_issue",
		Description: strings.TrimSpace(`
Use this when you want to show the captain exactly what a draft work item
would look like when filed, with its intake signals, BEFORE filing anything
(#3774). It wraps POST /v0/work-items/preview: the backend runs the same
conventions and intake hook fishhawk_file_issue runs and returns the rendered
title, the body the tracker would receive (advisory intake section included),
the merged labels, the number a numbered type would be allocated, and the
intake object. It creates NOTHING: no issue, no label, no board placement, no
epic link, and it writes no audit entry.

Eligibility: an operator caller. A run-bound (in-run agent) token is refused
403 preview_operator_only, because a preview reads tracker titles without
filing and ADR-064 keeps that read off the agent surface. A session that
cannot see the repo is refused 403 repo_forbidden.

Inputs: fishhawk_file_issue's inputs minus run_id (a preview is not
run-scoped and the tool never sends one, even when FISHHAWK_RUN_ID is set);
type + summary are required and repo falls back to GITHUB_REPOSITORY.
source_refs ('#N' or 'N', same repo) names the items this draft derives from:
they are excluded from the duplicate candidates and reported as
intake.derives_from, while an unrelated near duplicate is still flagged.

The result is a point-in-time view and reserves nothing: a concurrent filing
can take the previewed number or change the duplicate window before you file.
Duplicates and the epic suggestion are ADVISORY and LEXICAL, never a
decision; intake with degraded:true and a degrade_reason is normal. To file
the draft, call fishhawk_file_issue with the same inputs.

Tool errors: validation_failed (400 — repo/type/summary, a malformed
source_refs entry, or a run_id), authentication_required (401),
preview_operator_only / repo_forbidden (403), work_item_invalid (422 — the
draft violates the type's conventions), provider_unimplemented (501),
work_item_filing_failed (502 — the repo's installation could not be resolved;
nothing was filed either way).
`),
	}, resolver.previewIssue)
}

// previewIssue is the tool handler. It validates type + summary locally,
// resolves repo from the env when omitted, and delegates to the backend. It
// deliberately does NOT read FISHHAWK_RUN_ID.
func (r *runResolver) previewIssue(ctx context.Context, _ *mcp.CallToolRequest, in PreviewIssueInput) (*mcp.CallToolResult, PreviewIssueOutput, error) {
	if strings.TrimSpace(in.Type) == "" {
		return nil, PreviewIssueOutput{}, fmt.Errorf("type is required: name the work-item type (a key in the repo's conventions, e.g. feature, bug, chore, adr, epic)")
	}
	if strings.TrimSpace(in.Summary) == "" {
		return nil, PreviewIssueOutput{}, fmt.Errorf("summary is required: the one-line summary fills the title and is the required Summary field")
	}
	repo := in.Repo
	if repo == "" {
		repo = r.getenv("GITHUB_REPOSITORY")
	}
	if strings.TrimSpace(repo) == "" {
		return nil, PreviewIssueOutput{}, fmt.Errorf("repo is required: pass repo as owner/name or set GITHUB_REPOSITORY in the environment")
	}

	pv, err := r.api.PreviewWorkItem(ctx, FileWorkItemRequest{
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
		SourceRefs:      in.SourceRefs,
		Relations:       fileIssueRelations(in.Relations),
	})
	if err != nil {
		return nil, PreviewIssueOutput{}, fmt.Errorf("preview work item: %w", err)
	}
	return nil, PreviewIssueOutput{Preview: *pv}, nil
}
