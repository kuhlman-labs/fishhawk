package main

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleBundle is a minimal valid plain-JSONL trajectory (manifest +
// assistant tool_use + git_diff + trailer) used by the cmd tests.
const sampleBundle = `{"seq":1,"ts":"2026-06-10T09:00:00Z","kind":"manifest","data":{"bundle_schema":"v1","run_id":"r","stage_id":"implement","agent":"claudecode","model":"claude-opus-4-8","generated_at":"2026-06-10T09:05:00Z"}}
{"seq":2,"ts":"2026-06-10T09:00:05Z","kind":"assistant","data":{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read"}]}}}
{"seq":3,"ts":"2026-06-10T09:00:12Z","kind":"git_diff","data":{"kind":"git_diff","base_ref":"main","files":[{"path":"a.go","status":"M"}],"num_files":1}}
{"seq":4,"ts":"2026-06-10T09:00:20Z","kind":"trailer","data":{"event_count":3}}
`

func gzipString(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// noEnv is an env func that returns no values.
func noEnv(string) string { return "" }

// envMap builds a getenv func from a literal map.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestRunFromFile covers the --in file input source: a plain bundle on
// disk produces a populated case directory.
func TestRunFromFile(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "bundle.jsonl")
	if err := os.WriteFile(inPath, []byte(sampleBundle), 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	outDir := t.TempDir()
	err := run([]string{"--in", inPath, "--case-name", "file-case", "--out-dir", outDir, "--issue", "#1290"},
		noEnv, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertCaseDir(t, filepath.Join(outDir, "file-case"))
}

// TestRunFromStdin covers the stdin input source (absent --in).
func TestRunFromStdin(t *testing.T) {
	outDir := t.TempDir()
	err := run([]string{"--case-name", "stdin-case", "--out-dir", outDir},
		noEnv, strings.NewReader(sampleBundle))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertCaseDir(t, filepath.Join(outDir, "stdin-case"))
}

// TestRunMissingCaseName asserts the required-flag guard.
func TestRunMissingCaseName(t *testing.T) {
	err := run([]string{"--out-dir", t.TempDir()}, noEnv, strings.NewReader(sampleBundle))
	if err == nil || !strings.Contains(err.Error(), "case-name") {
		t.Fatalf("want missing --case-name error, got %v", err)
	}
}

// TestRunStageIDAndInConflict asserts --stage-id and --in are mutually
// exclusive.
func TestRunStageIDAndInConflict(t *testing.T) {
	err := run([]string{"--stage-id", "abc", "--in", "x", "--case-name", "c", "--out-dir", t.TempDir()},
		envMap(map[string]string{"FISHHAWK_API_TOKEN": "fhk_x"}), nil)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want mutual-exclusion error, got %v", err)
	}
}

// TestRunStageIDMissingToken asserts the fetch path requires
// FISHHAWK_API_TOKEN.
func TestRunStageIDMissingToken(t *testing.T) {
	err := run([]string{"--stage-id", "abc", "--case-name", "c", "--out-dir", t.TempDir()}, noEnv, nil)
	if err == nil || !strings.Contains(err.Error(), "FISHHAWK_API_TOKEN") {
		t.Fatalf("want missing-token error, got %v", err)
	}
}

// TestResolveOutDirDefaultMissing asserts the fail-loud default-OutDir
// resolution (condition 2): from a cwd where the default corpus dir does
// not exist, the tool errors actionably instead of scaffolding into a
// stray path.
func TestResolveOutDirDefaultMissing(t *testing.T) {
	// t.TempDir has no backend/internal/agenteval/testdata/corpus.
	t.Chdir(t.TempDir())
	_, err := resolveOutDir("")
	if err == nil || !strings.Contains(err.Error(), "default corpus dir") {
		t.Fatalf("want fail-loud default-dir error, got %v", err)
	}
}

// TestResolveOutDirExplicit asserts an explicit --out-dir bypasses the
// default existence check.
func TestResolveOutDirExplicit(t *testing.T) {
	got, err := resolveOutDir("/some/explicit/dir")
	if err != nil {
		t.Fatalf("explicit out-dir: %v", err)
	}
	if got != "/some/explicit/dir" {
		t.Fatalf("got %q, want explicit path", got)
	}
}

// TestFetchStageTraceSuccess stands up an in-process httptest.Server and
// asserts the fetch seam (condition 1): the request path is
// /v0/stages/{id}/trace, the bearer credential header is sent, and a
// gzipped body is piped into the core to produce a valid case dir.
func TestFetchStageTraceSuccess(t *testing.T) {
	const stageID = "11111111-1111-1111-1111-111111111111"
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gzipString(t, sampleBundle))
	}))
	defer srv.Close()

	outDir := t.TempDir()
	err := run([]string{"--stage-id", stageID, "--case-name", "fetched", "--out-dir", outDir, "--issue", "#1290"},
		envMap(map[string]string{
			"FISHHAWK_BACKEND_URL": srv.URL,
			"FISHHAWK_API_TOKEN":   "fhk_secret",
		}), nil)
	if err != nil {
		t.Fatalf("run via --stage-id: %v", err)
	}

	if gotPath != "/v0/stages/"+stageID+"/trace" {
		t.Errorf("request path = %q, want /v0/stages/%s/trace", gotPath, stageID)
	}
	if gotAuth != "Bearer fhk_secret" {
		t.Errorf("auth header = %q, want Bearer fhk_secret", gotAuth)
	}
	assertCaseDir(t, filepath.Join(outDir, "fetched"))
}

// TestFetchStageTraceNon200 asserts a non-200 response yields a clear,
// status-bearing error.
func TestFetchStageTraceNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"trace_not_found"}`))
	}))
	defer srv.Close()

	_, err := fetchStageTrace(srv.Client(), srv.URL, "fhk_x", "22222222-2222-2222-2222-222222222222")
	if err == nil {
		t.Fatal("want error on non-200, got nil")
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "trace_not_found") {
		t.Errorf("error should name the status + body, got: %v", err)
	}
}

// assertCaseDir checks the three case files exist and are non-empty.
func assertCaseDir(t *testing.T, caseDir string) {
	t.Helper()
	for _, name := range []string{"trace.jsonl", "expected.json", "case.md"} {
		info, err := os.Stat(filepath.Join(caseDir, name))
		if err != nil {
			t.Errorf("missing %s: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", name)
		}
	}
}
