package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// diagnosticBundle is the CLI-side projection of the backend's
// product-facts-only diagnostic bundle (GET /v0/runs/{id}/diagnostics,
// #1006). Field names + types match the wire shape verbatim; the
// bundle carries structured facts only (no free text), so printing it
// raw is safe.
type diagnosticBundle struct {
	RunID            string `json:"run_id"`
	WorkflowID       string `json:"workflow_id"`
	WorkflowSpecHash string `json:"workflow_spec_hash"`
	RunnerKind       string `json:"runner_kind"`
	RunState         string `json:"run_state"`
	Stages           []struct {
		Sequence int    `json:"sequence"`
		Type     string `json:"type"`
		State    string `json:"state"`
	} `json:"stages"`
	FailingStage *struct {
		Sequence        int    `json:"sequence"`
		Type            string `json:"type"`
		FailureCategory string `json:"failure_category"`
		FailureSurface  string `json:"failure_surface"`
	} `json:"failing_stage"`
	AuditSequenceRange *struct {
		Min int64 `json:"min"`
		Max int64 `json:"max"`
	} `json:"audit_sequence_range"`
	Versions struct {
		Fishhawkd struct {
			Version string `json:"version"`
			GitSHA  string `json:"git_sha"`
		} `json:"fishhawkd"`
		MinRunnerVersion string `json:"min_runner_version"`
	} `json:"versions"`
}

// runDiagnose implements `fishhawk diagnose <run-id> [--output text|json]`.
// It fetches the product-facts diagnostic bundle for a run and renders
// it as a human summary (default) or raw JSON. Read-only; the bundle
// never leaves the operator's machine from this verb.
func runDiagnose(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk diagnose", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	outputFmt := fs.String("output", "text", "output format: text | json")
	fs.StringVar(outputFmt, "o", "text", "output format: text | json (shorthand)")
	positionals, err := parseIntermixed(fs, args)
	if err != nil {
		return exitUsage
	}
	switch *outputFmt {
	case "text", "json":
	default:
		_, _ = fmt.Fprintf(stderr, "fishhawk diagnose: invalid --output %q (want text|json)\n", *outputFmt)
		return exitUsage
	}
	if len(positionals) != 1 {
		_, _ = fmt.Fprintln(stderr, "fishhawk diagnose: <run-id> required")
		return exitUsage
	}
	id, err := uuid.Parse(positionals[0])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk diagnose: %q is not a UUID: %v\n", positionals[0], err)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()
	b, err := fetchDiagnostics(ctx, newClient(cf), id)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk diagnose: %v\n", err)
		return exitOnAPIError(err)
	}

	switch *outputFmt {
	case "json":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(b); err != nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk diagnose: encode: %v\n", err)
			return exitFailure
		}
	default:
		printDiagnostics(stdout, b)
	}
	return exitOK
}

// fetchDiagnostics issues GET /v0/runs/{id}/diagnostics using the
// shared client's configured transport + bearer token. The CLI's
// typed httpclient lives in a separate package whose request helper
// is unexported; this verb is part of the read-surface slice and does
// its own decode here rather than reaching across the slice boundary
// into the API-client package. Non-2xx responses decode into the
// shared *httpclient.APIError so exitOnAPIError handles them uniformly.
func fetchDiagnostics(ctx context.Context, c *httpclient.Client, id uuid.UUID) (*diagnosticBundle, error) {
	url := c.BaseURL + "/v0/runs/" + id.String() + "/diagnostics"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var env struct {
			Error struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&env)
		return nil, &httpclient.APIError{
			StatusCode: resp.StatusCode,
			Code:       env.Error.Code,
			Message:    env.Error.Message,
			Details:    env.Error.Details,
		}
	}

	var b diagnosticBundle
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return nil, fmt.Errorf("decode diagnostics: %w", err)
	}
	return &b, nil
}

// printDiagnostics renders the bundle as a compact human summary.
func printDiagnostics(w io.Writer, b *diagnosticBundle) {
	_, _ = fmt.Fprintf(w, "Run %s\n", b.RunID)
	_, _ = fmt.Fprintf(w, "  workflow:   %s\n", b.WorkflowID)
	_, _ = fmt.Fprintf(w, "  spec hash:  %s\n", b.WorkflowSpecHash)
	_, _ = fmt.Fprintf(w, "  runner:     %s\n", b.RunnerKind)
	_, _ = fmt.Fprintf(w, "  state:      %s\n", b.RunState)
	_, _ = fmt.Fprintf(w, "  fishhawkd:  %s (%s)\n", b.Versions.Fishhawkd.Version, b.Versions.Fishhawkd.GitSHA)
	_, _ = fmt.Fprintf(w, "  min runner: %s\n", b.Versions.MinRunnerVersion)
	if b.AuditSequenceRange != nil {
		_, _ = fmt.Fprintf(w, "  audit seq:  %d..%d\n", b.AuditSequenceRange.Min, b.AuditSequenceRange.Max)
	}
	_, _ = fmt.Fprintln(w, "  stages:")
	for _, st := range b.Stages {
		_, _ = fmt.Fprintf(w, "    %d %-10s %s\n", st.Sequence, st.Type, st.State)
	}
	if b.FailingStage != nil {
		_, _ = fmt.Fprintf(w, "  failing stage: #%d %s (category %s",
			b.FailingStage.Sequence, b.FailingStage.Type, b.FailingStage.FailureCategory)
		if b.FailingStage.FailureSurface != "" {
			_, _ = fmt.Fprintf(w, ", surface %s", b.FailingStage.FailureSurface)
		}
		_, _ = fmt.Fprintln(w, ")")
	}
}
