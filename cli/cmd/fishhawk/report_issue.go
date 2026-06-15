package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// productReportRequest / productReport mirror the
// POST /v0/runs/{run_id}/product-reports wire shapes
// (docs/api/v0.openapi.yaml, backend/internal/server/product_report.go).
// Repeated locally rather than added to cli/internal/httpclient because
// this verb is the only consumer. Description crosses the egress boundary
// ONLY when IncludeFreeText is true, and is redacted server-side first.
type productReportRequest struct {
	Kind            string `json:"kind,omitempty"`
	Description     string `json:"description,omitempty"`
	IncludeFreeText bool   `json:"include_free_text,omitempty"`
}

type productReport struct {
	Fingerprint string `json:"fingerprint"`
	Action      string `json:"action"`
	Number      int    `json:"number"`
	URL         string `json:"url"`
	Destination string `json:"destination"`
}

// reportIssueHTTPDo is the HTTP seam for the report-issue verb. Tests swap
// it for a stub; production uses a 60s-timeout client (matching httpclient).
var reportIssueHTTPDo = func(req *http.Request) (*http.Response, error) {
	return (&http.Client{Timeout: 60 * time.Second}).Do(req)
}

// runReportIssue implements `fishhawk report-issue <run-id>`: file a
// deduped, audited upstream Fishhawk product report carrying the run's
// auto-collected, redacted diagnostic bundle (#1006). By default the
// report carries product-level facts ONLY; operator free text
// (--description) crosses the boundary ONLY with the explicit
// --include-free-text consent, and is redacted server-side first.
func runReportIssue(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk report-issue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)

	kind := fs.String("kind", "bug", "report flavor: bug | feature")
	description := fs.String("description", "", "operator free text ('what was I trying to do'); crosses the boundary ONLY with --include-free-text, redacted server-side first")
	includeFreeText := fs.Bool("include-free-text", false, "EXPLICIT consent: send --description across the egress boundary (redacted server-side). Default off — product facts only")
	outputFmt := fs.String("output", "text", "output format: text | json")
	fs.StringVar(outputFmt, "o", "text", "output format: text | json (shorthand)")

	positionals, err := parseIntermixed(fs, args)
	if err != nil {
		return exitUsage
	}
	if *outputFmt != "text" && *outputFmt != "json" {
		_, _ = fmt.Fprintf(stderr, "fishhawk report-issue: invalid --output %q (want text|json)\n", *outputFmt)
		return exitUsage
	}
	switch *kind {
	case "bug", "feature":
	default:
		_, _ = fmt.Fprintf(stderr, "fishhawk report-issue: invalid --kind %q (want bug|feature)\n", *kind)
		return exitUsage
	}
	if len(positionals) != 1 {
		_, _ = fmt.Fprintln(stderr, "fishhawk report-issue: <run-id> required")
		return exitUsage
	}
	id, err := uuid.Parse(positionals[0])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk report-issue: %q is not a UUID: %v\n", positionals[0], err)
		return exitUsage
	}

	// Consent guard: an un-consented description never leaves the machine.
	// Warn rather than fail so the common no-free-text path is frictionless.
	req := productReportRequest{Kind: *kind}
	if *includeFreeText {
		req.IncludeFreeText = true
		req.Description = *description
	} else if strings.TrimSpace(*description) != "" {
		_, _ = fmt.Fprintln(stderr, "fishhawk report-issue: --description ignored without --include-free-text (consent required); filing product facts only")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()
	report, err := postProductReport(ctx, *cf.backendURL, *cf.token, id, req)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk report-issue: %v\n", err)
		return exitOnAPIError(err)
	}

	if *outputFmt == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk report-issue: encode: %v\n", err)
			return exitFailure
		}
		return exitOK
	}
	printProductReport(stdout, report)
	return exitOK
}

// postProductReport POSTs to /v0/runs/{run_id}/product-reports and decodes
// the outcome. On a non-2xx it decodes the OpenAPI error envelope into an
// *httpclient.APIError so exitOnAPIError handles it uniformly. The endpoint
// is not on the shared httpclient.Client surface — this verb is its only
// consumer.
func postProductReport(ctx context.Context, backendURL, token string, runID uuid.UUID, req productReportRequest) (*productReport, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	url := strings.TrimRight(backendURL, "/") + "/v0/runs/" + runID.String() + "/product-reports"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := reportIssueHTTPDo(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		apiErr := &httpclient.APIError{StatusCode: resp.StatusCode}
		var env struct {
			Error struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &env) == nil {
			apiErr.Code = env.Error.Code
			apiErr.Message = env.Error.Message
			apiErr.Details = env.Error.Details
		}
		return nil, apiErr
	}

	var out productReport
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &out, nil
}

// printProductReport renders the egress outcome for the text format.
func printProductReport(w io.Writer, r *productReport) {
	switch r.Action {
	case "occurrence":
		_, _ = fmt.Fprintf(w, "occurrence appended to existing report #%d\n", r.Number)
	default:
		_, _ = fmt.Fprintf(w, "filed product report #%d\n", r.Number)
	}
	_, _ = fmt.Fprintf(w, "  action:      %s\n", r.Action)
	_, _ = fmt.Fprintf(w, "  fingerprint: %s\n", r.Fingerprint)
	_, _ = fmt.Fprintf(w, "  destination: %s\n", r.Destination)
	_, _ = fmt.Fprintf(w, "  url:         %s\n", r.URL)
}
