package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalJSONL is a valid one-line-manifest + trailer bundle, enough for
// Distill to parse and score. (The core's exhaustive cases live in
// corpusdistill's tests; here we only prove run()'s wiring.)
const minimalJSONL = `{"seq":1,"ts":"2026-06-01T14:00:00Z","kind":"manifest","data":{"bundle_schema":"v1","run_id":"r","stage_id":"s","agent":"claudecode","generated_at":"2026-06-01T14:05:00Z"}}
{"seq":2,"ts":"2026-06-01T14:00:15Z","kind":"trailer","data":{}}
`

// withStdin temporarily replaces os.Stdin with a file containing content.
func withStdin(t *testing.T, content string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatalf("temp stdin: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("seek stdin: %v", err)
	}
	orig := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = orig
		_ = f.Close()
	})
}

// TestRun_StdinSource asserts run() wires stdin through to Distill and
// writes a case dir under an explicit --out-dir.
func TestRun_StdinSource(t *testing.T) {
	withStdin(t, minimalJSONL)
	out := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := run([]string{"--case-name", "c", "--issue", "#1290", "--out-dir", out}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit = %d, stderr=%s", code, stderr.String())
	}
	caseDir := filepath.Join(out, "c")
	if _, err := os.Stat(filepath.Join(caseDir, "trace.jsonl")); err != nil {
		t.Errorf("trace.jsonl not written: %v", err)
	}
	if strings.TrimSpace(stdout.String()) != caseDir {
		t.Errorf("stdout = %q, want case dir %q", stdout.String(), caseDir)
	}
}

// TestRun_InFileSource asserts the --in file path wires through to Distill.
func TestRun_InFileSource(t *testing.T) {
	in := filepath.Join(t.TempDir(), "bundle.jsonl")
	if err := os.WriteFile(in, []byte(minimalJSONL), 0o644); err != nil {
		t.Fatalf("write in: %v", err)
	}
	out := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := run([]string{"--in", in, "--case-name", "c", "--issue", "#1290", "--out-dir", out}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit = %d, stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(out, "c", "case.md")); err != nil {
		t.Errorf("case.md not written: %v", err)
	}
}

// TestRun_DefaultOutDirFailLoud covers the fail-loud default-OutDir branch:
// run from a cwd lacking the corpus parent dir returns the actionable error
// rather than silently writing.
func TestRun_DefaultOutDirFailLoud(t *testing.T) {
	withStdin(t, minimalJSONL)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	var stdout, stderr bytes.Buffer

	// No --out-dir, and the cwd lacks backend/internal/agenteval/testdata.
	code := run([]string{"--case-name", "c", "--issue", "#1290"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), corpusParentRel) {
		t.Errorf("error does not name the corpus parent dir: %s", stderr.String())
	}
}

// TestRun_LabelFlags asserts --signal/--narrative wire through to a labeled
// case.md (the operator's text lands; the TODO prompts do not).
func TestRun_LabelFlags(t *testing.T) {
	withStdin(t, minimalJSONL)
	out := t.TempDir()
	var stdout, stderr bytes.Buffer

	const signal = "scope_drift"
	const narrative = "Agent edited an out-of-scope file the runner then dropped."
	code := run([]string{
		"--case-name", "c", "--issue", "#1291", "--out-dir", out,
		"--signal", signal, "--narrative", narrative,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit = %d, stderr=%s", code, stderr.String())
	}
	md, err := os.ReadFile(filepath.Join(out, "c", "case.md"))
	if err != nil {
		t.Fatalf("read case.md: %v", err)
	}
	for _, want := range []string{signal, narrative} {
		if !strings.Contains(string(md), want) {
			t.Errorf("case.md missing %q\n---\n%s", want, md)
		}
	}
	if strings.Contains(string(md), "TODO(operator): state the distilled signal") {
		t.Errorf("labeled case.md still emits the distilled-signal TODO prompt\n---\n%s", md)
	}
}

// TestRun_DryRun asserts --dry-run exits 0, writes NO case dir under
// --out-dir, and prints the resolved case dir + derived outcome to stdout.
func TestRun_DryRun(t *testing.T) {
	withStdin(t, minimalJSONL)
	out := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := run([]string{"--case-name", "c", "--issue", "#1291", "--out-dir", out, "--dry-run"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dry-run exit = %d, stderr=%s", code, stderr.String())
	}
	// No files written: OutDir must be empty.
	if entries, err := os.ReadDir(out); err != nil {
		t.Fatalf("read out dir: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("--dry-run wrote entries under --out-dir: %v", entries)
	}
	got := stdout.String()
	for _, want := range []string{filepath.Join(out, "c"), "derived outcome:", "case.md"} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run stdout missing %q\n---\n%s", want, got)
		}
	}
}

// TestRun_DryRun_ErrorExitsNonZero pins the exit contract's error branch: a
// genuine error (empty stdin bundle) under --dry-run returns 1, not 0.
func TestRun_DryRun_ErrorExitsNonZero(t *testing.T) {
	withStdin(t, "")
	out := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := run([]string{"--case-name", "c", "--issue", "#1291", "--out-dir", out, "--dry-run"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("dry-run on empty bundle exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if entries, err := os.ReadDir(out); err != nil {
		t.Fatalf("read out dir: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("--dry-run error path wrote entries under --out-dir: %v", entries)
	}
}

// TestRun_MissingRequiredFlags covers the required-flag guards.
func TestRun_MissingRequiredFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"missing case-name", []string{"--issue", "#1290", "--out-dir", t.TempDir()}},
		{"missing issue", []string{"--case-name", "c", "--out-dir", t.TempDir()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code != 2 {
				t.Errorf("run exit = %d, want 2; stderr=%s", code, stderr.String())
			}
		})
	}
}

// minimalMissItems is a one-item audit items array carrying a class-3
// acceptance_triage_decided payload with a plan_review_miss record.
const minimalMissItems = `[{"sequence":5,"run_id":"11111111-2222-3333-4444-555555555555","ts":"2026-07-01T00:00:00Z","payload":{"class":"3","disposition":"paged","reason":"bad criterion","plan_review_miss":[{"criterion_id":"ac-x","source":"inferred","observed":"wrong"}]}}]`

// TestRun_PlanReviewMiss_FlagValidation covers the miss-mode flag-combination
// guards: --run-id without the mode, --stage-id with it, and the shared
// required-flag guards, each exit 2 with an actionable message.
func TestRun_PlanReviewMiss_FlagValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{
			"run-id without plan-review-miss",
			[]string{"--run-id", "r", "--case-name", "c", "--issue", "#1539"},
			"--plan-review-miss",
		},
		{
			"stage-id with plan-review-miss",
			[]string{"--plan-review-miss", "--stage-id", "s", "--case-name", "c", "--issue", "#1539"},
			"--run-id",
		},
		{
			"missing case-name",
			[]string{"--plan-review-miss", "--issue", "#1539"},
			"--case-name",
		},
		{
			"missing issue",
			[]string{"--plan-review-miss", "--case-name", "c"},
			"--issue",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code != 2 {
				t.Fatalf("run exit = %d, want 2; stderr=%s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantSub) {
				t.Errorf("error not actionable (missing %q): %s", tc.wantSub, stderr.String())
			}
		})
	}
}

// TestRun_PlanReviewMiss_StdinSource: miss-mode stdin items write a case dir
// with miss.json + case.md, and the operator-supplied provenance TODO (not
// the PRODUCTION assertion).
func TestRun_PlanReviewMiss_StdinSource(t *testing.T) {
	withStdin(t, minimalMissItems)
	out := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := run([]string{"--plan-review-miss", "--case-name", "m", "--issue", "#1539", "--out-dir", out}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit = %d, stderr=%s", code, stderr.String())
	}
	caseDir := filepath.Join(out, "m")
	if _, err := os.Stat(filepath.Join(caseDir, "miss.json")); err != nil {
		t.Errorf("miss.json not written: %v", err)
	}
	md, err := os.ReadFile(filepath.Join(caseDir, "case.md"))
	if err != nil {
		t.Fatalf("read case.md: %v", err)
	}
	if !strings.Contains(string(md), "Provenance: TODO(operator)") {
		t.Errorf("stdin-sourced case.md must carry the provenance TODO:\n%s", md)
	}
	if strings.TrimSpace(stdout.String()) != caseDir {
		t.Errorf("stdout = %q, want case dir %q", stdout.String(), caseDir)
	}
}

// TestRun_PlanReviewMiss_DryRun: miss-mode --dry-run exits 0, writes nothing,
// and prints the would-be case.
func TestRun_PlanReviewMiss_DryRun(t *testing.T) {
	withStdin(t, minimalMissItems)
	out := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := run([]string{"--plan-review-miss", "--case-name", "m", "--issue", "#1539", "--out-dir", out, "--dry-run"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dry-run exit = %d, stderr=%s", code, stderr.String())
	}
	if entries, err := os.ReadDir(out); err != nil {
		t.Fatalf("read out dir: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("--dry-run wrote entries under --out-dir: %v", entries)
	}
	for _, want := range []string{filepath.Join(out, "m"), "miss.json", "case.md"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("dry-run stdout missing %q\n---\n%s", want, stdout.String())
		}
	}
}

// TestRun_PlanReviewMiss_RunIDFetch: the --run-id path fetches the audit
// items from --backend-url and the resulting case.md asserts the PRODUCTION
// redacted-by-construction provenance (Fetched=true wiring).
func TestRun_PlanReviewMiss_RunIDFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":` + minimalMissItems + `,"next_cursor":""}`))
	}))
	defer srv.Close()
	out := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := run([]string{"--plan-review-miss", "--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", srv.URL, "--case-name", "m", "--issue", "run 11111111", "--out-dir", out}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit = %d, stderr=%s", code, stderr.String())
	}
	md, err := os.ReadFile(filepath.Join(out, "m", "case.md"))
	if err != nil {
		t.Fatalf("read case.md: %v", err)
	}
	for _, want := range []string{"Provenance: PRODUCTION", "redacted-by-construction"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("fetched case.md missing %q:\n%s", want, md)
		}
	}
}

// TestRun_PlanReviewMiss_ZeroClass3ExitsNonZero: zero class-3 entries is a
// loud exit-1 error, never an empty success.
func TestRun_PlanReviewMiss_ZeroClass3ExitsNonZero(t *testing.T) {
	withStdin(t, `[{"sequence":1,"payload":{"class":"1","disposition":"fixup_dispatched"}}]`)
	var stdout, stderr bytes.Buffer

	code := run([]string{"--plan-review-miss", "--case-name", "m", "--issue", "#1539", "--out-dir", t.TempDir()}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "class-3") {
		t.Errorf("error does not state the zero-class-3 cause: %s", stderr.String())
	}
}

// TestRun_PlanReviewMiss_BadItemsJSON: undecodable items input is an
// actionable exit-1 error naming the accepted shapes.
func TestRun_PlanReviewMiss_BadItemsJSON(t *testing.T) {
	withStdin(t, `not json`)
	var stdout, stderr bytes.Buffer

	code := run([]string{"--plan-review-miss", "--case-name", "m", "--issue", "#1539", "--out-dir", t.TempDir()}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "items") {
		t.Errorf("error not actionable: %s", stderr.String())
	}
}
