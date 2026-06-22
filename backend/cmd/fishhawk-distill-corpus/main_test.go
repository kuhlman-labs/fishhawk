package main

import (
	"bytes"
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
