package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ReportProductIssueInput is the fishhawk_report_product_issue tool's
// input schema (#1006, slice 3). run_id falls back to FISHHAWK_RUN_ID env
// when omitted (the in-runner case), mirroring fishhawk_file_issue. kind
// selects the report flavor. The CONSENT contract is explicit: description
// (operator free text) crosses the egress boundary ONLY when
// include_free_text is true, and is run through the backend's redaction
// machinery server-side first; without the flag the report carries
// product-level facts ONLY.
type ReportProductIssueInput struct {
	RunID           string `json:"run_id,omitempty" jsonschema:"the run whose product-facts bundle to attach; falls back to FISHHAWK_RUN_ID env when omitted (the in-runner case)"`
	Kind            string `json:"kind,omitempty" jsonschema:"report flavor: 'bug' (default — attaches the diagnostic bundle) or 'feature' (an enhancement request; lighter workflow context)"`
	Description     string `json:"description,omitempty" jsonschema:"OPTIONAL operator free text ('what was I trying to do'). It crosses the boundary ONLY when include_free_text is true, and is redacted server-side first. Ignored when include_free_text is false"`
	IncludeFreeText bool   `json:"include_free_text,omitempty" jsonschema:"EXPLICIT consent: when true, the description crosses the egress boundary AFTER server-side redaction. Default false — only product-level facts leave the boundary"`
}

// ReportProductIssueOutput wraps the egress outcome plus a transparency
// preview of the product facts that were attached. Report echoes what
// left the boundary (fingerprint, created-vs-occurrence, upstream
// number/url, destination). Diagnostics is the product-facts-only bundle
// the report carried — surfaced so the caller can see exactly what
// crossed (best-effort; omitted if the preview fetch failed).
// FreeTextIncluded reports whether operator free text was consented and
// redaction-passed across the boundary.
type ReportProductIssueOutput struct {
	Report           ProductReport     `json:"report"`
	Diagnostics      *DiagnosticBundle `json:"diagnostics,omitempty"`
	FreeTextIncluded bool              `json:"free_text_included"`
}

// registerReportProductIssue wires the fishhawk_report_product_issue tool
// (#1006): the operator/agent path to file an upstream Fishhawk product
// bug or feature request carrying an auto-collected, redacted,
// fingerprint-deduped diagnostic bundle.
//
// Auth: a write tool that drives an egress on the run's hash chain, so the
// backend requires the run's OWN run-bound agent token — an operator token
// or a foreign run's token is rejected (run_not_entitled). The destination
// is the FIXED upstream product repo; it is not caller-controlled.
func registerReportProductIssue(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_report_product_issue",
		Description: strings.TrimSpace(`
Use this to file an upstream Fishhawk PRODUCT bug or feature request when you
hit friction with Fishhawk itself (not the repo you're working in) — the
operator-feedback path (#1006). It wraps POST /v0/runs/{run_id}/product-reports:
the backend auto-collects the run's product-facts diagnostic bundle (run id,
stage states, the failing stage's failure category + surface, audit sequence
range, binary versions + git SHAs, workflow spec hash, runner kind),
fingerprints the failure, searches the FIXED upstream product repo for an open
report already carrying that fingerprint, and either appends an occurrence
comment (dedup hit — nothing new is created) or files a new fingerprint-marked
report (dedup miss). A source-side product_report_filed audit entry records
what left the boundary.

THE REDACTION BOUNDARY IS THE HARD CONTRACT. By default the report carries
product-level FACTS ONLY — no diffs, paths, prompts, or free text. Operator
free text (the 'description' input) crosses the boundary ONLY when you set
include_free_text=true, and even then it is run through the backend's
secret-redaction machinery first. Treat include_free_text as the operator's
explicit consent; default it off.

Inputs: run_id falls back to FISHHAWK_RUN_ID env when omitted (the in-runner
case). kind is 'bug' (default) or 'feature'. description + include_free_text
carry the consented, redacted free text.

Returns the egress outcome (report.action created|occurrence, fingerprint,
upstream number/url, destination), a transparency preview of the product
facts that were attached (diagnostics), and free_text_included. Tool errors:
validation_failed (400), authentication_required (401), run_not_entitled
(403 — only the run's own run-bound token may file), product_feedback_disabled
(403 — the repo's kill-switch), run_not_found (404), provider_unimplemented
(501), product_report_failed (502).
`),
	}, resolver.reportProductIssue)
}

// reportProductIssue is the tool handler. It resolves run_id from the env
// when omitted, files the report (the backend owns bundle collection,
// fingerprinting, dedup, redaction, and the source-side audit), then
// best-effort fetches the diagnostic bundle so the response shows exactly
// which product facts crossed the boundary.
func (r *runResolver) reportProductIssue(ctx context.Context, _ *mcp.CallToolRequest, in ReportProductIssueInput) (*mcp.CallToolResult, ReportProductIssueOutput, error) {
	runIDStr := strings.TrimSpace(in.RunID)
	if runIDStr == "" {
		runIDStr = strings.TrimSpace(r.getenv("FISHHAWK_RUN_ID"))
	}
	if runIDStr == "" {
		return nil, ReportProductIssueOutput{}, fmt.Errorf("run_id is required: pass the run UUID or set FISHHAWK_RUN_ID in the environment")
	}
	runID, err := uuid.Parse(runIDStr)
	if err != nil {
		return nil, ReportProductIssueOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", runIDStr, err)
	}

	kind := strings.TrimSpace(in.Kind)
	// Guard the consent contract locally too: free text only travels with
	// the explicit flag. (The backend enforces this authoritatively; this
	// keeps an un-consented description off the wire entirely.)
	description := ""
	if in.IncludeFreeText {
		description = in.Description
	}

	report, err := r.api.ReportProductIssue(ctx, runID, kind, description, in.IncludeFreeText)
	if err != nil {
		return nil, ReportProductIssueOutput{}, fmt.Errorf("report product issue: %w", err)
	}

	out := ReportProductIssueOutput{
		Report:           *report,
		FreeTextIncluded: in.IncludeFreeText && strings.TrimSpace(in.Description) != "",
	}
	// Transparency preview: surface the product facts that were attached.
	// Best-effort — the report already filed, so a preview-fetch failure
	// must not turn a successful egress into a tool error.
	if bundle, derr := r.api.GetDiagnostics(ctx, runID); derr == nil {
		out.Diagnostics = bundle
	}
	return nil, out, nil
}
