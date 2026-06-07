package agenteval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
)

// TestScore replays every seed corpus case through Score and asserts the
// full Scorecard against the case's expected.json. This is both the CI
// green gate (it runs green on the healthy + 618 seed set) AND the
// discrimination proof: the 618-wire-regression case's expected.json
// asserts the regression signals (evidence_before_edit=false +
// non-empty scope_drift_paths), so a scorer that failed to surface them
// would fail this assertion — the deterministic stand-in for the issue's
// "known-bad edit caught by the suite" acceptance test.
func TestScore(t *testing.T) {
	const corpusDir = "testdata/corpus"
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatalf("read corpus dir: %v", err)
	}
	cases := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cases++
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(corpusDir, name)
			lines := readTraceLines(t, filepath.Join(dir, "trace.jsonl"))

			var want Scorecard
			expBytes, err := os.ReadFile(filepath.Join(dir, "expected.json"))
			if err != nil {
				t.Fatalf("read expected.json: %v", err)
			}
			if err := json.Unmarshal(expBytes, &want); err != nil {
				t.Fatalf("parse expected.json: %v", err)
			}

			got := Score(lines)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("scorecard mismatch\n got: %s\nwant: %s", mustJSON(t, got), mustJSON(t, want))
			}
		})
	}
	if cases == 0 {
		t.Fatal("no corpus cases found under " + corpusDir)
	}
}

// TestToolNamesFailOpen pins the extractor's fail-open contract: a
// non-assistant line, a malformed payload, or a missing content array
// yields no tool names rather than a panic, so stream-json schema drift
// degrades to no-signal (mirroring toolCallSignatures).
func TestToolNamesFailOpen(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"non-json", `not json at all`},
		{"raw-text-fallback", `{"text":"some raw line"}`},
		{"wrong-type", `{"type":"user","message":{"content":[{"type":"tool_use","name":"Read"}]}}`},
		{"no-message", `{"type":"assistant"}`},
		{"empty-content", `{"type":"assistant","message":{"content":[]}}`},
		{"non-tooluse-block", `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolNames(json.RawMessage(tc.data)); len(got) != 0 {
				t.Errorf("toolNames(%s) = %v, want none", tc.data, got)
			}
		})
	}
}

// TestEvidenceBeforeEdit covers the read-before-first-write logic
// directly, including the blind-edit and read-only edges.
func TestEvidenceBeforeEdit(t *testing.T) {
	tests := []struct {
		name string
		seq  []string
		want bool
	}{
		{"read-then-edit", []string{"Read", "Edit"}, true},
		{"edit-then-read", []string{"Edit", "Read"}, false},
		{"blind-edit", []string{"Edit"}, false},
		{"read-only", []string{"Grep", "Read"}, true},
		{"empty", nil, false},
		{"bash-then-edit", []string{"Bash", "Edit"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := evidenceBeforeEdit(tc.seq); got != tc.want {
				t.Errorf("evidenceBeforeEdit(%v) = %v, want %v", tc.seq, got, tc.want)
			}
		})
	}
}

// readTraceLines parses a plain (non-gzip) .jsonl fixture into
// []bundle.Line, one Line per non-blank line.
func readTraceLines(t *testing.T, path string) []bundle.Line {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
	defer func() { _ = f.Close() }()

	var lines []bundle.Line
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var l bundle.Line
		if err := json.Unmarshal(scanner.Bytes(), &l); err != nil {
			t.Fatalf("parse trace line in %s: %v", path, err)
		}
		lines = append(lines, l)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan trace %s: %v", path, err)
	}
	return lines
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
