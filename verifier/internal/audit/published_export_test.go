package audit_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/verifier/internal/audit"
)

// pkgSrcDir is this package's SOURCE directory, captured once at package
// initialization (#4179): `go test` runs the test binary from within the
// package's source directory (go help testflag), and package-level variables
// initialize before any test runs, so this holds even if a test later changes
// the process cwd. Fixtures anchor here, never on runtime.Caller, whose file
// name is module-relative under -trimpath and so misses every fixture.
var pkgSrcDir = mustGetwd()

// mustGetwd returns os.Getwd() and panics on an error, so an unresolvable
// package dir fails the test binary closed at init.
func mustGetwd() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(fmt.Sprintf("pkgSrcDir: os.Getwd: %v", err))
	}
	return dir
}

// repoRoot resolves the repository root from this package's init-captured
// source dir: verifier/internal/audit is three directories below the repo
// root. This is valid in the committed tree — the only place this test runs
// (go.work registers ./verifier and scripts/test executes it from the
// checkout).
func repoRoot(t *testing.T) string {
	t.Helper()
	// pkgSrcDir = verifier/internal/audit -> ../../.. = repo root.
	return filepath.Join(pkgSrcDir, "..", "..", "..")
}

// TestPublishedExportVerifies is the committed-tree done-means test for the
// E9 capstone (#1609): it re-verifies the COMMITTED compliance export via the
// real external-verifier library and asserts the committed human-readable
// report is complete. It is the cross-boundary end-to-end check — producer
// endpoint -> CLI continuation assembly -> committed artifact -> external
// verifier library — and it fails on a corrupted, tampered, partial, or
// missing published artifact where a mere scope-presence gate would pass,
// keeping the §13 "first compliance report export works end-to-end"
// criterion continuously demonstrated in CI.
//
// That the verifier CAN detect tampering is itself pinned elsewhere (the
// #1607 round-trip tamper case in
// backend/internal/server/audit_export_roundtrip_test.go); this test does
// not re-prove that — it pins that the published artifact verifies clean.
func TestPublishedExportVerifies(t *testing.T) {
	root := repoRoot(t)
	exportPath := filepath.Join(root, "docs", "compliance", "fishhawk-dev-audit-export.json")

	// A missing artifact is a FAILURE, not a skip: it is committed
	// alongside this test.
	f, err := os.Open(exportPath)
	if err != nil {
		t.Fatalf("open published export %s: %v (the artifact must be committed with this test)", exportPath, err)
	}
	defer f.Close()

	// DisallowUnknownFields inside ParseExport catches shape drift.
	ex, err := audit.ParseExport(f)
	if err != nil {
		t.Fatalf("parse published export: %v", err)
	}

	res := audit.VerifyExport(ex)
	if !res.OK() {
		for _, iss := range res.Issues {
			t.Errorf("verify issue: run=%s seq=%d kind=%s detail=%s", iss.RunID, iss.Sequence, iss.Kind, iss.Detail)
		}
		t.Fatalf("published export failed verification: %d issue(s)", len(res.Issues))
	}
	if res.RunsVerified == 0 {
		t.Error("published export verified zero runs; expected a non-empty audit trail")
	}
	if res.EntriesChecked == 0 {
		t.Error("published export verified zero audit entries; expected a non-empty audit trail")
	}
}

// TestPublishedReportIsComplete pins the committed human-readable
// agent-changes report's completeness markers: a partial report (the
// default-page "PARTIAL REPORT" banner), a report missing its totals line,
// or an evidence-poor window (no acceptance verdicts) each fails a distinct
// assertion, so a silently-truncated or wrong-window regeneration cannot be
// committed unnoticed.
func TestPublishedReportIsComplete(t *testing.T) {
	root := repoRoot(t)
	reportPath := filepath.Join(root, "docs", "compliance", "fishhawk-dev-agent-changes.md")

	b, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read published report %s: %v (the artifact must be committed with this test)", reportPath, err)
	}
	report := string(b)
	if strings.TrimSpace(report) == "" {
		t.Fatal("published report is empty")
	}

	if strings.Contains(report, "PARTIAL REPORT") {
		t.Error("published report carries the 'PARTIAL REPORT' banner — it was generated with a page limit below the window's run count")
	}
	if !strings.Contains(report, "Totals:") {
		t.Error("published report is missing its 'Totals:' line")
	}
	if !strings.Contains(report, "Acceptance: verdict=") {
		t.Error("published report carries no 'Acceptance: verdict=' evidence line — the window should include acceptance verdicts")
	}
}
