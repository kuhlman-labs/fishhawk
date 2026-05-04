package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/verifier/internal/audit"
)

// writeExport produces a JSON export file at a temp path and
// returns the path. Internal helper for the CLI tests.
func writeExport(t *testing.T, ex *audit.Export) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "export.json")
	body, err := json.Marshal(ex)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// validExport is a minimal happy-path export with a single entry.
func validExport(t *testing.T) *audit.Export {
	t.Helper()
	runID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	payload := json.RawMessage(`{"event":"start"}`)
	hash, err := audit.ComputeEntryHash(audit.HashInputs{
		RunID:     &runID,
		Timestamp: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		Category:  "start",
		Payload:   payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &audit.Export{
		Schema: audit.ExportSchemaV1,
		Runs: map[string]audit.RunData{
			runID.String(): {
				AuditEntries: []audit.Entry{
					{
						ID:        uuid.New(),
						Sequence:  1,
						RunID:     &runID,
						Timestamp: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
						Category:  "start",
						Payload:   payload,
						EntryHash: hash,
					},
				},
			},
		},
	}
}

func TestRun_HappyPath(t *testing.T) {
	path := writeExport(t, validExport(t))
	var stdout strings.Builder
	got := run([]string{"--export", path}, &stdout, io.Discard)
	if got != exitOK {
		t.Errorf("exit = %d, want %d", got, exitOK)
	}
	if !strings.Contains(stdout.String(), "PASS") {
		t.Errorf("stdout missing PASS:\n%s", stdout.String())
	}
}

func TestRun_DetectsTampering(t *testing.T) {
	ex := validExport(t)
	for k, run := range ex.Runs {
		// Tamper the entry's payload without updating EntryHash.
		run.AuditEntries[0].Payload = json.RawMessage(`{"event":"TAMPERED"}`)
		ex.Runs[k] = run
	}
	path := writeExport(t, ex)

	var stdout strings.Builder
	got := run([]string{"--export", path}, &stdout, io.Discard)
	if got != exitFailure {
		t.Errorf("exit = %d, want %d", got, exitFailure)
	}
	out := stdout.String()
	if !strings.Contains(out, "FAIL") {
		t.Errorf("stdout missing FAIL:\n%s", out)
	}
	if !strings.Contains(out, "hash_mismatch") {
		t.Errorf("stdout missing hash_mismatch:\n%s", out)
	}
}

func TestRun_MissingExportFlag(t *testing.T) {
	got := run([]string{}, io.Discard, io.Discard)
	if got != exitUsage {
		t.Errorf("exit = %d, want %d", got, exitUsage)
	}
}

func TestRun_BadFlag(t *testing.T) {
	got := run([]string{"--no-such-flag"}, io.Discard, io.Discard)
	if got != exitUsage {
		t.Errorf("exit = %d, want %d", got, exitUsage)
	}
}

func TestRun_FileDoesNotExist(t *testing.T) {
	got := run([]string{"--export", "/no/such/path.json"}, io.Discard, io.Discard)
	if got != exitUsage {
		t.Errorf("exit = %d, want %d", got, exitUsage)
	}
}

func TestRun_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := run([]string{"--export", path}, io.Discard, io.Discard)
	if got != exitUsage {
		t.Errorf("exit = %d, want %d", got, exitUsage)
	}
}

func TestPrintResult_OK(t *testing.T) {
	var w strings.Builder
	printResult(&w, audit.Result{RunsVerified: 2, EntriesChecked: 7})
	out := w.String()
	if !strings.Contains(out, "PASS") {
		t.Errorf("missing PASS: %s", out)
	}
	if !strings.Contains(out, "2 run(s)") || !strings.Contains(out, "7 audit entries") {
		t.Errorf("missing counts: %s", out)
	}
}

func TestPrintResult_WithIssues(t *testing.T) {
	var w strings.Builder
	printResult(&w, audit.Result{
		RunsVerified:   1,
		EntriesChecked: 3,
		Issues: []audit.Issue{
			{
				RunID:    uuid.MustParse("00000000-0000-0000-0000-000000000001"),
				Sequence: 2,
				Kind:     audit.IssueHashMismatch,
				Detail:   "recomputed != stored",
			},
		},
	})
	out := w.String()
	for _, want := range []string{"FAIL", "1 issue", "kind=hash_mismatch", "seq=2"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}
