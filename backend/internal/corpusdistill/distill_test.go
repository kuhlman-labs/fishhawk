package corpusdistill

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/agenteval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
)

// fixtureJSONL is a small but valid trace bundle: a manifest line, a
// read-class tool_use, a write-class tool_use, and a trailer. It has no
// git_diff and an unset agent_failed flag, so the scorer derives the
// no_diff outcome with evidence_before_edit=true.
const fixtureJSONL = `{"seq":1,"ts":"2026-06-01T14:00:00Z","kind":"manifest","data":{"bundle_schema":"v1","run_id":"run-distill-fixture","stage_id":"implement","agent":"claudecode","model":"claude-opus-4-8","generated_at":"2026-06-01T14:05:00Z"}}
{"seq":2,"ts":"2026-06-01T14:00:05Z","kind":"assistant","data":{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"backend/internal/domain/report.go"}}]}}}
{"seq":3,"ts":"2026-06-01T14:00:10Z","kind":"assistant","data":{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"backend/internal/domain/report.go"}}]}}}
{"seq":4,"ts":"2026-06-01T14:00:15Z","kind":"trailer","data":{}}
`

func gzipFixture(t *testing.T, plain string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(plain)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func readDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, name := range []string{"trace.jsonl", "expected.json", "case.md"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out[name] = b
	}
	return out
}

// TestDistill_GzipAndPlain_IdenticalOutput covers modes (1) and (2): a
// gzipped input and a plain .jsonl input must both write all three files
// and produce byte-identical output (the gzip auto-detect, both branches).
func TestDistill_GzipAndPlain_IdenticalOutput(t *testing.T) {
	gz := gzipFixture(t, fixtureJSONL)

	gzDir := t.TempDir()
	gzCase, err := Distill(bytes.NewReader(gz), Options{CaseName: "c", Issue: "#1290", OutDir: gzDir})
	if err != nil {
		t.Fatalf("Distill gzip: %v", err)
	}
	plainDir := t.TempDir()
	plainCase, err := Distill(strings.NewReader(fixtureJSONL), Options{CaseName: "c", Issue: "#1290", OutDir: plainDir})
	if err != nil {
		t.Fatalf("Distill plain: %v", err)
	}

	gzFiles := readDir(t, gzCase)
	plainFiles := readDir(t, plainCase)
	for _, name := range []string{"trace.jsonl", "expected.json", "case.md"} {
		if !bytes.Equal(gzFiles[name], plainFiles[name]) {
			t.Errorf("%s differs between gzip and plain inputs:\ngzip:  %q\nplain: %q", name, gzFiles[name], plainFiles[name])
		}
	}
}

// TestDistill_ExpectedAndTraceBytes covers mode (3): expected.json equals a
// fresh MarshalIndent of Score over the parsed lines (+newline), and
// trace.jsonl byte-equals the plain JSONL input.
func TestDistill_ExpectedAndTraceBytes(t *testing.T) {
	dir := t.TempDir()
	caseDir, err := Distill(strings.NewReader(fixtureJSONL), Options{CaseName: "c", Issue: "#1290", OutDir: dir})
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	files := readDir(t, caseDir)

	if !bytes.Equal(files["trace.jsonl"], []byte(fixtureJSONL)) {
		t.Errorf("trace.jsonl not byte-equal to plain input\ngot:  %q\nwant: %q", files["trace.jsonl"], fixtureJSONL)
	}

	lines, err := bundle.ReadEvents(gzipFixture(t, fixtureJSONL))
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	want, err := json.MarshalIndent(agenteval.Score(lines), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want = append(want, '\n')
	if !bytes.Equal(files["expected.json"], want) {
		t.Errorf("expected.json mismatch\ngot:  %q\nwant: %q", files["expected.json"], want)
	}
}

// TestDistill_CaseMD covers mode (4): case.md carries the PRODUCTION
// provenance marker, the --issue reference, and the redacted-source
// statement.
func TestDistill_CaseMD(t *testing.T) {
	dir := t.TempDir()
	caseDir, err := Distill(strings.NewReader(fixtureJSONL), Options{CaseName: "my-case", Issue: "#1290", OutDir: dir})
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	md := string(readDir(t, caseDir)["case.md"])
	for _, want := range []string{"Provenance: PRODUCTION", "#1290", "REDACTED", "no_diff"} {
		if !strings.Contains(md, want) {
			t.Errorf("case.md missing %q\n---\n%s", want, md)
		}
	}
}

// TestDistill_OverwriteGuard covers mode (5), both branches: a re-run
// without Force errors and leaves the dir untouched; with Force it
// succeeds.
func TestDistill_OverwriteGuard(t *testing.T) {
	dir := t.TempDir()
	opts := Options{CaseName: "c", Issue: "#1290", OutDir: dir}
	caseDir, err := Distill(strings.NewReader(fixtureJSONL), opts)
	if err != nil {
		t.Fatalf("first Distill: %v", err)
	}
	// Drop a sentinel so we can prove the no-Force path doesn't touch it.
	sentinel := filepath.Join(caseDir, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	if _, err := Distill(strings.NewReader(fixtureJSONL), opts); err == nil {
		t.Fatal("expected error re-distilling without Force, got nil")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("no-Force path disturbed existing dir: sentinel gone: %v", err)
	}

	forced := opts
	forced.Force = true
	if _, err := Distill(strings.NewReader(fixtureJSONL), forced); err != nil {
		t.Fatalf("Distill with Force: %v", err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Errorf("Force did not recreate the dir (sentinel still present): %v", err)
	}
}

// TestDistill_RoundTripReplay covers mode (6): the generated case loads and
// replays through Score exactly as agenteval TestScore does, and the result
// DeepEquals the parsed expected.json — proving the scaffold is replay-valid.
func TestDistill_RoundTripReplay(t *testing.T) {
	dir := t.TempDir()
	caseDir, err := Distill(strings.NewReader(fixtureJSONL), Options{CaseName: "c", Issue: "#1290", OutDir: dir})
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}

	lines := replayTraceLines(t, filepath.Join(caseDir, "trace.jsonl"))
	got := agenteval.Score(lines)

	var want agenteval.Scorecard
	expBytes, err := os.ReadFile(filepath.Join(caseDir, "expected.json"))
	if err != nil {
		t.Fatalf("read expected.json: %v", err)
	}
	if err := json.Unmarshal(expBytes, &want); err != nil {
		t.Fatalf("parse expected.json: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("replay mismatch\ngot:  %+v\nwant: %+v", got, want)
	}
}

// TestDistill_Errors covers the validation guards: empty input, and the
// required-field guards (CaseName, Issue, OutDir).
func TestDistill_Errors(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		r    string
		opts Options
	}{
		{"empty input", "", Options{CaseName: "c", Issue: "#1290", OutDir: dir}},
		{"missing case name", fixtureJSONL, Options{Issue: "#1290", OutDir: dir}},
		{"missing issue", fixtureJSONL, Options{CaseName: "c", OutDir: dir}},
		{"missing out dir", fixtureJSONL, Options{CaseName: "c", Issue: "#1290"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Distill(strings.NewReader(tc.r), tc.opts); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// replayTraceLines mirrors agenteval's TestScore loader: one bundle.Line
// per non-blank trace.jsonl line.
func replayTraceLines(t *testing.T, path string) []bundle.Line {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
	defer func() { _ = f.Close() }()
	var lines []bundle.Line
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var l bundle.Line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("parse trace line: %v", err)
		}
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return lines
}
