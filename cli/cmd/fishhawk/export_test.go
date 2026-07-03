package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exportStub is one canned export page: its body plus the two
// continuation headers. cursor is the query cursor this page answers to
// (empty = first page).
type exportStub struct {
	cursor     string
	body       string
	complete   bool
	nextCursor string
	status     int // 0 => 200
}

// newExportServer serves the given pages from the given base path,
// keyed by the request's ?cursor. Any request whose cursor has no
// matching stub 404s (a test bug). It records how many times it was
// hit so tests can assert the loop stopped.
func newExportServer(t *testing.T, path string, pages []exportStub) (*httptest.Server, *int) {
	t.Helper()
	byCursor := map[string]exportStub{}
	for _, p := range pages {
		byCursor[p.cursor] = p
	}
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		hits++
		p, ok := byCursor[r.URL.Query().Get("cursor")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if p.status >= 400 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(p.status)
			_, _ = io.WriteString(w, p.body)
			return
		}
		w.Header().Set("X-Fishhawk-Export-Complete", boolStr(p.complete))
		if !p.complete {
			w.Header().Set("X-Fishhawk-Export-Next-Cursor", p.nextCursor)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, p.body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

const (
	runA      = "11111111-1111-1111-1111-111111111111"
	runB      = "22222222-2222-2222-2222-222222222222"
	globalKey = "00000000-0000-0000-0000-000000000000"
)

// TestExport_JSONTwoPageMerge (case a): two JSON pages merge into a body
// that round-trips to exactly {schema, exported_at, runs} with the union
// of run keys, and the first page's schema/exported_at are preserved.
func TestExport_JSONTwoPageMerge(t *testing.T) {
	page1 := `{"schema":"v1","exported_at":"2026-05-01T00:00:00Z","runs":{` +
		`"` + runA + `":{"audit_entries":[]},` +
		`"` + globalKey + `":{"audit_entries":[]}}}`
	page2 := `{"schema":"v1","exported_at":"2026-05-01T00:00:00Z","runs":{` +
		`"` + runB + `":{"audit_entries":[]}}}`
	srv, hits := newExportServer(t, "/v0/audit/export", []exportStub{
		{cursor: "", body: page1, complete: false, nextCursor: "c1"},
		{cursor: "c1", body: page2, complete: true},
	})

	var stdout strings.Builder
	got := run([]string{"export", "--backend-url", srv.URL, "--limit", "1"}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if *hits != 2 {
		t.Errorf("server hit %d times, want 2", *hits)
	}

	// Exactly the three Export v1 top-level fields, no assembly metadata.
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout.String()), &top); err != nil {
		t.Fatalf("assembled body not valid JSON: %v\n%s", err, stdout.String())
	}
	if len(top) != 3 {
		t.Errorf("assembled body has %d top-level fields, want 3 (schema, exported_at, runs): %v", len(top), keys(top))
	}
	for _, k := range []string{"schema", "exported_at", "runs"} {
		if _, ok := top[k]; !ok {
			t.Errorf("assembled body missing %q field", k)
		}
	}
	if string(top["schema"]) != `"v1"` {
		t.Errorf("schema = %s, want \"v1\"", top["schema"])
	}
	var runs map[string]json.RawMessage
	if err := json.Unmarshal(top["runs"], &runs); err != nil {
		t.Fatalf("runs not an object: %v", err)
	}
	for _, want := range []string{runA, runB, globalKey} {
		if _, ok := runs[want]; !ok {
			t.Errorf("runs union missing %q", want)
		}
	}
	if len(runs) != 3 {
		t.Errorf("runs has %d keys, want 3: %v", len(runs), keys(runs))
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestExport_IncompleteWithoutCursorErrors (case b): a page reporting
// complete=false with no continuation cursor fails loud instead of
// looping forever.
func TestExport_IncompleteWithoutCursorErrors(t *testing.T) {
	srv, hits := newExportServer(t, "/v0/audit/export", []exportStub{
		{cursor: "", body: `{"schema":"v1","exported_at":"2026-05-01T00:00:00Z","runs":{}}`, complete: false, nextCursor: ""},
	})

	var stderr strings.Builder
	got := run([]string{"export", "--backend-url", srv.URL}, io.Discard, &stderr)
	if got != exitFailure {
		t.Fatalf("status = %d, want exitFailure", got)
	}
	if *hits != 1 {
		t.Errorf("server hit %d times, want 1 (must not loop)", *hits)
	}
	if !strings.Contains(stderr.String(), "no continuation cursor") {
		t.Errorf("stderr missing loop-guard diagnostic: %s", stderr.String())
	}
}

// TestExport_DuplicateRunKeyErrors (case c): the same run key appearing
// on two pages is a hard error (the server contract is disjoint pages).
func TestExport_DuplicateRunKeyErrors(t *testing.T) {
	page := `{"schema":"v1","exported_at":"2026-05-01T00:00:00Z","runs":{"` + runA + `":{"audit_entries":[]}}}`
	srv, _ := newExportServer(t, "/v0/audit/export", []exportStub{
		{cursor: "", body: page, complete: false, nextCursor: "c1"},
		{cursor: "c1", body: page, complete: true}, // re-emits runA
	})

	var stderr strings.Builder
	got := run([]string{"export", "--backend-url", srv.URL}, io.Discard, &stderr)
	if got != exitFailure {
		t.Fatalf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "more than one export page") {
		t.Errorf("stderr missing duplicate-key diagnostic: %s", stderr.String())
	}
}

// TestExport_APIErrorSurfaced (case d): a server 400 (filter mutual
// exclusion) exits non-zero and prints the API message.
func TestExport_APIErrorSurfaced(t *testing.T) {
	srv, _ := newExportServer(t, "/v0/audit/export", []exportStub{
		{cursor: "", status: http.StatusBadRequest,
			body: `{"error":{"code":"validation_failed","message":"run_id cannot be combined with repo, from, or to","details":{"field":"run_id"}}}`},
	})

	var stderr strings.Builder
	got := run([]string{"export", "--backend-url", srv.URL, "--repo", "x/y", "--run", runA}, io.Discard, &stderr)
	if got != exitFailure {
		t.Fatalf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "validation_failed") {
		t.Errorf("stderr missing API code: %s", stderr.String())
	}
}

// TestExport_CSVTwoPageConcat (case e): two CSV pages concatenate with
// exactly one header row (the first page's), continuation rows appended.
func TestExport_CSVTwoPageConcat(t *testing.T) {
	page1 := "ts,run_id,entry_hash\n2026-05-01,r1,h1\n"
	page2 := "ts,run_id,entry_hash\n2026-05-02,r2,h2\n"
	srv, _ := newExportServer(t, "/v0/audit/export.csv", []exportStub{
		{cursor: "", body: page1, complete: false, nextCursor: "c1"},
		{cursor: "c1", body: page2, complete: true},
	})

	var stdout strings.Builder
	got := run([]string{"export", "--csv", "--backend-url", srv.URL, "--limit", "1"}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	out := stdout.String()
	if n := strings.Count(out, "ts,run_id,entry_hash"); n != 1 {
		t.Errorf("header row appears %d times, want exactly 1:\n%s", n, out)
	}
	for _, want := range []string{"2026-05-01,r1,h1", "2026-05-02,r2,h2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing data row %q:\n%s", want, out)
		}
	}
}

// TestExport_CSVHeaderMismatchErrors (case f): a later CSV page whose
// header differs from the first page's is a hard error.
func TestExport_CSVHeaderMismatchErrors(t *testing.T) {
	page1 := "ts,run_id,entry_hash\n2026-05-01,r1,h1\n"
	page2 := "ts,run_id,DIFFERENT\n2026-05-02,r2,h2\n"
	srv, _ := newExportServer(t, "/v0/audit/export.csv", []exportStub{
		{cursor: "", body: page1, complete: false, nextCursor: "c1"},
		{cursor: "c1", body: page2, complete: true},
	})

	var stderr strings.Builder
	got := run([]string{"export", "--csv", "--backend-url", srv.URL}, io.Discard, &stderr)
	if got != exitFailure {
		t.Fatalf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "does not match the first page header") {
		t.Errorf("stderr missing header-mismatch diagnostic: %s", stderr.String())
	}
}

// TestExport_OutWritesFile (case g, part 1): --out writes the assembled
// body to the file and prints nothing to stdout.
func TestExport_OutWritesFile(t *testing.T) {
	body := `{"schema":"v1","exported_at":"2026-05-01T00:00:00Z","runs":{"` + runA + `":{"audit_entries":[]}}}`
	srv, _ := newExportServer(t, "/v0/audit/export", []exportStub{
		{cursor: "", body: body, complete: true},
	})
	outPath := filepath.Join(t.TempDir(), "export.json")

	var stdout strings.Builder
	got := run([]string{"export", "--backend-url", srv.URL, "--out", outPath}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty when --out is set, got: %s", stdout.String())
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read --out file: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("--out file not valid JSON: %v", err)
	}
	if _, ok := top["runs"]; !ok {
		t.Errorf("--out file missing runs: %s", raw)
	}
}

// TestExport_NoOutWritesStdout (case g, part 2): without --out the body
// streams to stdout.
func TestExport_NoOutWritesStdout(t *testing.T) {
	body := `{"schema":"v1","exported_at":"2026-05-01T00:00:00Z","runs":{}}`
	srv, _ := newExportServer(t, "/v0/audit/export", []exportStub{
		{cursor: "", body: body, complete: true},
	})

	var stdout strings.Builder
	got := run([]string{"export", "--backend-url", srv.URL}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if !strings.Contains(stdout.String(), `"schema"`) {
		t.Errorf("stdout missing export body: %s", stdout.String())
	}
}

// TestExport_OutAtomicOnMidPaginationFailure (binding condition): a
// failure after the first page must NOT leave a partial file at --out.
// The whole export is assembled in memory before any write, so the
// destination path must be absent after the run fails.
func TestExport_OutAtomicOnMidPaginationFailure(t *testing.T) {
	page1 := `{"schema":"v1","exported_at":"2026-05-01T00:00:00Z","runs":{"` + runA + `":{"audit_entries":[]}}}`
	srv, _ := newExportServer(t, "/v0/audit/export", []exportStub{
		{cursor: "", body: page1, complete: false, nextCursor: "c1"},
		{cursor: "c1", status: http.StatusInternalServerError,
			body: `{"error":{"code":"internal_error","message":"boom"}}`},
	})
	outPath := filepath.Join(t.TempDir(), "export.json")

	var stderr strings.Builder
	got := run([]string{"export", "--backend-url", srv.URL, "--limit", "1", "--out", outPath}, io.Discard, &stderr)
	if got != exitFailure {
		t.Fatalf("status = %d, want exitFailure", got)
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Errorf("--out path exists after mid-pagination failure (stat err = %v); atomic write violated", err)
	}
	// No leftover temp files in the output directory either.
	entries, _ := os.ReadDir(filepath.Dir(outPath))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".fishhawk-export-") {
			t.Errorf("leftover temp file after failure: %s", e.Name())
		}
	}
}

// TestExport_FlagParseError (case h): an unknown flag exits with the
// usage code.
func TestExport_FlagParseError(t *testing.T) {
	got := run([]string{"export", "--no-such-flag"}, io.Discard, io.Discard)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
}

// TestExport_UnexpectedPositionalArg (case h, sibling): a positional
// argument (this verb is flags-only) exits with the usage code before
// any network call.
func TestExport_UnexpectedPositionalArg(t *testing.T) {
	var stderr strings.Builder
	got := run([]string{"export", "stray-arg"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "unexpected argument") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}
