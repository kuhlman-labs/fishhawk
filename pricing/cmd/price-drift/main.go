// Command price-drift runs the pricing table drift-check against the LiteLLM
// price dataset and prints a Markdown report (#1335, ADR-044 decision 1).
//
// It is the network/clock shell around pricing.CheckDrift (which is pure): it
// fetches the dataset at a PINNED immutable commit (AGENTS.md pin-tools rule),
// stamps the time, renders the report to stdout, and — when run under GitHub
// Actions (GITHUB_OUTPUT set) — emits `high_severity` and `has_findings`
// outputs so the daily scheduled job can open/update an issue on drift.
//
// Per ADR-044 the check WARNS and NEVER fails the build: this command exits 0
// even on high-severity drift. The daily job acts on the report; PR CI runs it
// as a non-blocking advisory. (The internal completeness invariant — every live
// model id priced — stays a hard CI FAIL in pricing_test.go, separately.)
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/kuhlman-labs/fishhawk/pricing"
)

// litellmPinnedSHA pins the LiteLLM model_prices_and_context_window.json to an
// immutable commit so a community-dataset change can never alter this check's
// result without a reviewed bump here (AGENTS.md pin-tools-in-CI rule). Bump
// deliberately; a floating main would couple our CI to upstream churn.
const litellmPinnedSHA = "bd2a1653bd92aeef272b29feaa750706db094975" // 2026-06-25

func datasetURL(sha string) string {
	return "https://raw.githubusercontent.com/BerriAI/litellm/" + sha +
		"/model_prices_and_context_window.json"
}

func main() {
	if err := run(); err != nil {
		// A fetch/parse failure is an infra problem, not drift: report it and
		// exit non-zero so the operator notices the alarm could not run — but
		// this is the FETCH path, distinct from a drift result (which is exit 0).
		fmt.Fprintf(os.Stderr, "price-drift: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	body, err := fetch(ctx, datasetURL(litellmPinnedSHA))
	if err != nil {
		return err
	}

	report, err := pricing.CheckDrift(body, litellmPinnedSHA, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}

	fmt.Println(report.Markdown())

	// Machine-readable outputs for the scheduled job (no-op locally).
	if out := os.Getenv("GITHUB_OUTPUT"); out != "" {
		line := fmt.Sprintf("high_severity=%t\nhas_drift=%t\nhas_findings=%t\n",
			report.HighSeverity(), report.HasDrift(), len(report.Findings) > 0)
		if err := appendFile(out, line); err != nil {
			return fmt.Errorf("write GITHUB_OUTPUT: %w", err)
		}
	}
	return nil
}

func appendFile(path, content string) (err error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	_, err = f.WriteString(content)
	return err
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
