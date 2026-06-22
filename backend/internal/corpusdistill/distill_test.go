package corpusdistill

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/agenteval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
)

// readSample reads a testdata fixture or fails the test.
func readSample(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// scoreSample parses the plain JSONL fixture line-by-line (mirroring
// agenteval.TestScore's readTraceLines) and scores it — the
// independently-derived expectation the generated case is checked
// against.
func scoreSample(t *testing.T, plain []byte) agenteval.Scorecard {
	t.Helper()
	var lines []bundle.Line
	sc := bufio.NewScanner(bytes.NewReader(plain))
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
		t.Fatalf("scan plain sample: %v", err)
	}
	return agenteval.Score(lines)
}

// TestDistillPlainInput exercises the plain-JSONL detect branch: Distill
// on testdata/sample.jsonl writes all three files, and their bytes match
// the independently-derived expectation.
func TestDistillPlainInput(t *testing.T) {
	plain := readSample(t, "sample.jsonl")
	outDir := t.TempDir()
	if err := Distill(plain, Options{CaseName: "plain-case", Issue: "#1290", OutDir: outDir}); err != nil {
		t.Fatalf("Distill plain: %v", err)
	}
	caseDir := filepath.Join(outDir, "plain-case")

	// trace.jsonl is the plain trajectory verbatim.
	gotTrace := readFile(t, filepath.Join(caseDir, "trace.jsonl"))
	if !bytes.Equal(gotTrace, plain) {
		t.Errorf("trace.jsonl mismatch:\n got %q\nwant %q", gotTrace, plain)
	}

	// expected.json byte-matches a fresh MarshalIndent of the scorecard
	// derived from the same lines.
	want := scoreSample(t, plain)
	wantBytes, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	wantBytes = append(wantBytes, '\n')
	gotExpected := readFile(t, filepath.Join(caseDir, "expected.json"))
	if !bytes.Equal(gotExpected, wantBytes) {
		t.Errorf("expected.json mismatch:\n got %s\nwant %s", gotExpected, wantBytes)
	}

	// case.md carries the provenance marker and the issue reference.
	gotMD := string(readFile(t, filepath.Join(caseDir, "case.md")))
	if !strings.Contains(gotMD, "Provenance: PRODUCTION") {
		t.Errorf("case.md missing Provenance: PRODUCTION marker:\n%s", gotMD)
	}
	if !strings.Contains(gotMD, "#1290") {
		t.Errorf("case.md missing issue reference #1290:\n%s", gotMD)
	}
	if !strings.Contains(gotMD, want.Outcome) {
		t.Errorf("case.md missing derived outcome %q:\n%s", want.Outcome, gotMD)
	}
}

// TestDistillGzipInput exercises the gzip detect branch: Distill on
// testdata/sample.jsonl.gz produces output IDENTICAL to the plain path,
// proving the auto-detect normalizes both encodings to the same case.
func TestDistillGzipInput(t *testing.T) {
	gz := readSample(t, "sample.jsonl.gz")
	plain := readSample(t, "sample.jsonl")

	outDir := t.TempDir()
	if err := Distill(gz, Options{CaseName: "gz-case", Issue: "#1290", OutDir: outDir}); err != nil {
		t.Fatalf("Distill gzip: %v", err)
	}
	caseDir := filepath.Join(outDir, "gz-case")

	// trace.jsonl is the gunzipped plain trajectory — byte-equal to the
	// plain fixture.
	gotTrace := readFile(t, filepath.Join(caseDir, "trace.jsonl"))
	if !bytes.Equal(gotTrace, plain) {
		t.Errorf("gz trace.jsonl mismatch:\n got %q\nwant %q", gotTrace, plain)
	}

	// expected.json matches the plain-derived scorecard.
	want := scoreSample(t, plain)
	wantBytes, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	wantBytes = append(wantBytes, '\n')
	gotExpected := readFile(t, filepath.Join(caseDir, "expected.json"))
	if !bytes.Equal(gotExpected, wantBytes) {
		t.Errorf("gz expected.json mismatch:\n got %s\nwant %s", gotExpected, wantBytes)
	}
}

// TestDistillRoundTripReplayable is the cross-seam test: distill a
// fixture, then replay the generated case the way agenteval.TestScore
// does (parse trace.jsonl line-by-line, unmarshal expected.json,
// re-score, reflect.DeepEqual). A distilled case must be replay-valid.
func TestDistillRoundTripReplayable(t *testing.T) {
	plain := readSample(t, "sample.jsonl")
	outDir := t.TempDir()
	if err := Distill(plain, Options{CaseName: "rt-case", Issue: "#1290", OutDir: outDir}); err != nil {
		t.Fatalf("Distill: %v", err)
	}
	caseDir := filepath.Join(outDir, "rt-case")

	// Replay: parse the generated trace.jsonl exactly as the corpus
	// replay test does.
	got := scoreSample(t, readFile(t, filepath.Join(caseDir, "trace.jsonl")))

	var want agenteval.Scorecard
	if err := json.Unmarshal(readFile(t, filepath.Join(caseDir, "expected.json")), &want); err != nil {
		t.Fatalf("unmarshal generated expected.json: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip replay mismatch:\n got %+v\nwant %+v", got, want)
	}
}

// TestDistillOverwriteGuard asserts BOTH branches of the overwrite
// guard: without Force an existing case directory is an error and is
// left untouched; with Force it overwrites.
func TestDistillOverwriteGuard(t *testing.T) {
	plain := readSample(t, "sample.jsonl")
	outDir := t.TempDir()
	caseName := "guarded"
	caseDir := filepath.Join(outDir, caseName)

	// Pre-populate the case directory with a sentinel file so we can
	// prove the non-force path leaves it untouched.
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatalf("pre-create case dir: %v", err)
	}
	sentinel := filepath.Join(caseDir, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	// Without Force: error, directory untouched (no trace.jsonl written,
	// sentinel intact).
	err := Distill(plain, Options{CaseName: caseName, Issue: "#1290", OutDir: outDir})
	if err == nil {
		t.Fatal("Distill into existing dir without Force: want error, got nil")
	}
	if _, statErr := os.Stat(filepath.Join(caseDir, "trace.jsonl")); !os.IsNotExist(statErr) {
		t.Errorf("non-force path wrote trace.jsonl into existing dir (want untouched): %v", statErr)
	}
	if b := readFile(t, sentinel); string(b) != "keep me" {
		t.Errorf("non-force path disturbed sentinel: %q", b)
	}

	// With Force: overwrites successfully, three files now present.
	if err := Distill(plain, Options{CaseName: caseName, Issue: "#1290", OutDir: outDir, Force: true}); err != nil {
		t.Fatalf("Distill with Force: %v", err)
	}
	for _, name := range []string{"trace.jsonl", "expected.json", "case.md"} {
		if _, statErr := os.Stat(filepath.Join(caseDir, name)); statErr != nil {
			t.Errorf("force path missing %s: %v", name, statErr)
		}
	}
}

// TestDistillBadGzip asserts the parse-error branch: a truncated gzip
// member (valid magic bytes, corrupt frame) surfaces a wrapped error
// rather than masking it.
func TestDistillBadGzip(t *testing.T) {
	// Valid gzip magic so isGzip routes it down the gunzip branch, then
	// garbage so the frame is corrupt.
	corrupt := append([]byte{0x1f, 0x8b}, []byte("not a real gzip frame")...)
	outDir := t.TempDir()
	err := Distill(corrupt, Options{CaseName: "bad", Issue: "#1290", OutDir: outDir})
	if err == nil {
		t.Fatal("Distill on corrupt gzip: want error, got nil")
	}
	if !strings.Contains(err.Error(), "gunzip") {
		t.Errorf("error should name the gunzip failure, got: %v", err)
	}
}

// TestDistillRequiredOptions asserts the input-validation guards for an
// empty case name and an empty out dir.
func TestDistillRequiredOptions(t *testing.T) {
	plain := readSample(t, "sample.jsonl")
	if err := Distill(plain, Options{CaseName: "", OutDir: t.TempDir()}); err == nil {
		t.Error("empty CaseName: want error, got nil")
	}
	if err := Distill(plain, Options{CaseName: "x", OutDir: ""}); err == nil {
		t.Error("empty OutDir: want error, got nil")
	}
}

// TestDistillReader covers the io.Reader wrapper used by the command's
// stdin and HTTP-body paths.
func TestDistillReader(t *testing.T) {
	plain := readSample(t, "sample.jsonl")
	outDir := t.TempDir()
	if err := DistillReader(bytes.NewReader(plain), Options{CaseName: "reader-case", OutDir: outDir}); err != nil {
		t.Fatalf("DistillReader: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "reader-case", "trace.jsonl")); err != nil {
		t.Errorf("DistillReader did not write trace.jsonl: %v", err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
