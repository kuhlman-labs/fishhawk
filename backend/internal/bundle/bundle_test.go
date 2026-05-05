package bundle

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
)

// packLines builds a bundle-shaped *.jsonl.gz from the given lines.
// Tests use this rather than importing the runner's Pack to keep
// the read-side parser exercised end-to-end without crossing
// module boundaries.
func packLines(t *testing.T, lines []Line) []byte {
	t.Helper()
	var raw bytes.Buffer
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(b)
		raw.WriteByte('\n')
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

func makeDiffLine(t *testing.T, baseRef string, entries ...[2]string) Line {
	t.Helper()
	files := make([]gitDiffEntry, 0, len(entries))
	for _, e := range entries {
		files = append(files, gitDiffEntry{Path: e[0], Status: e[1]})
	}
	payload, err := json.Marshal(gitDiffPayload{
		Kind:     "name_status",
		BaseRef:  baseRef,
		Files:    files,
		NumFiles: len(files),
	})
	if err != nil {
		t.Fatal(err)
	}
	return Line{Seq: 2, Kind: EventKindGitDiff, Data: payload}
}

// E8.5 (#163): the manifest carries the runner's category-A
// signal. ExtractManifest is the read-side surface the trace
// handler uses to branch on it.

func TestExtractManifest_HappyPath(t *testing.T) {
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{
			"bundle_schema":"v1",
			"run_id":"run-1",
			"stage_id":"stage-1",
			"agent":"claude-code",
			"agent_failed":true,
			"agent_failure_reason":"agent process exited 137 (OOM)"
		}`)},
		{Seq: 2, Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
	got, err := ExtractManifest(packLines(t, lines))
	if err != nil {
		t.Fatalf("ExtractManifest: %v", err)
	}
	if got.RunID != "run-1" {
		t.Errorf("RunID = %q", got.RunID)
	}
	if !got.AgentFailed {
		t.Error("AgentFailed = false, want true")
	}
	if got.AgentFailureReason != "agent process exited 137 (OOM)" {
		t.Errorf("AgentFailureReason = %q", got.AgentFailureReason)
	}
}

func TestExtractManifest_OlderBundleParsesAgentFailedAsFalse(t *testing.T) {
	// Bundles packed before E8.5 don't carry the field at all.
	// omitempty on the runner side keeps them on-the-wire-clean;
	// the read side must default to AgentFailed=false.
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{
			"bundle_schema":"v1",
			"run_id":"run-1",
			"stage_id":"stage-1",
			"agent":"claude-code"
		}`)},
		{Seq: 2, Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
	got, err := ExtractManifest(packLines(t, lines))
	if err != nil {
		t.Fatalf("ExtractManifest: %v", err)
	}
	if got.AgentFailed {
		t.Error("AgentFailed = true on a bundle without the field")
	}
}

func TestExtractManifest_EmptyBundle(t *testing.T) {
	_, err := ExtractManifest(packLines(t, nil))
	if !errors.Is(err, ErrNoManifest) {
		t.Errorf("err = %v, want ErrNoManifest", err)
	}
}

func TestExtractManifest_FirstLineWrongKind(t *testing.T) {
	// First line was somehow not the manifest. Refuse cleanly so the
	// trace handler doesn't read garbage as the agent-failed flag.
	lines := []Line{
		{Seq: 1, Kind: "raw", Data: json.RawMessage(`{}`)},
		{Seq: 2, Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
	_, err := ExtractManifest(packLines(t, lines))
	if !errors.Is(err, ErrNoManifest) {
		t.Errorf("err = %v, want ErrNoManifest", err)
	}
}

func TestExtractManifest_BadJSON(t *testing.T) {
	// Hand-craft a bundle whose manifest line has an envelope that
	// parses but a `data` payload that doesn't. Going through
	// json.RawMessage on the test side won't work — Marshal
	// validates the bytes — so build the JSONL stream manually.
	var raw bytes.Buffer
	raw.WriteString(`{"seq":1,"kind":"manifest","data":{"bundle_schema": invalid}}`)
	raw.WriteByte('\n')
	raw.WriteString(`{"seq":2,"kind":"trailer","data":{}}`)
	raw.WriteByte('\n')

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := ExtractManifest(gz.Bytes())
	if err == nil {
		t.Fatal("ExtractManifest returned nil error on bad-JSON manifest")
	}
	if errors.Is(err, ErrNoManifest) || errors.Is(err, ErrBadGzip) {
		t.Errorf("err = %v, want a JSON parse error (not ErrNoManifest / ErrBadGzip)", err)
	}
}

func TestExtractDiff_HappyPath(t *testing.T) {
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{"bundle_schema":"v1"}`)},
		makeDiffLine(t, "origin/main",
			[2]string{"backend/main.go", "M"},
			[2]string{"backend/handlers.go", "A"},
		),
		{Seq: 3, Kind: "policy_event", Data: json.RawMessage(`{"check":"constraints","outcome":"valid"}`)},
		{Seq: 4, Kind: "trailer", Data: json.RawMessage(`{"event_count":3,"content_hash":"abc"}`)},
	}
	bytes := packLines(t, lines)

	got, err := ExtractDiff(bytes)
	if err != nil {
		t.Fatalf("ExtractDiff: %v", err)
	}
	if len(got.ChangedFiles) != 2 {
		t.Fatalf("got %d files, want 2", len(got.ChangedFiles))
	}
	if got.ChangedFiles[0].Path != "backend/main.go" || got.ChangedFiles[0].Status != policy.StatusModified {
		t.Errorf("first file = %+v", got.ChangedFiles[0])
	}
	if got.ChangedFiles[1].Path != "backend/handlers.go" || got.ChangedFiles[1].Status != policy.StatusAdded {
		t.Errorf("second file = %+v", got.ChangedFiles[1])
	}
}

func TestExtractDiff_NoDiffEvent(t *testing.T) {
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{}`)},
		{Seq: 2, Kind: "policy_event", Data: json.RawMessage(`{}`)},
	}
	_, err := ExtractDiff(packLines(t, lines))
	if !errors.Is(err, ErrNoDiffEvent) {
		t.Errorf("err = %v, want ErrNoDiffEvent", err)
	}
}

func TestExtractDiff_EmptyDiff(t *testing.T) {
	lines := []Line{
		makeDiffLine(t, "origin/main"),
	}
	got, err := ExtractDiff(packLines(t, lines))
	if err != nil {
		t.Fatalf("ExtractDiff: %v", err)
	}
	if len(got.ChangedFiles) != 0 {
		t.Errorf("expected empty diff, got %+v", got)
	}
}

func TestExtractDiff_FirstDiffEventWins(t *testing.T) {
	// If for some reason multiple git_diff events appear (shouldn't,
	// but defense in depth), the first one is what the backend uses.
	lines := []Line{
		makeDiffLine(t, "origin/main", [2]string{"a.go", "M"}),
		makeDiffLine(t, "origin/main", [2]string{"b.go", "M"}),
	}
	got, err := ExtractDiff(packLines(t, lines))
	if err != nil {
		t.Fatalf("ExtractDiff: %v", err)
	}
	if len(got.ChangedFiles) != 1 || got.ChangedFiles[0].Path != "a.go" {
		t.Errorf("expected first event to win, got %+v", got)
	}
}

func TestExtractDiff_BadGzip(t *testing.T) {
	_, err := ExtractDiff([]byte("not gzipped"))
	if !errors.Is(err, ErrBadGzip) {
		t.Errorf("err = %v, want ErrBadGzip", err)
	}
}

func TestExtractDiff_BadJSONLine(t *testing.T) {
	var raw bytes.Buffer
	raw.WriteString("not a json line\n")
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(raw.Bytes())
	_ = w.Close()

	_, err := ExtractDiff(gz.Bytes())
	if err == nil || !strings.Contains(err.Error(), "parse line") {
		t.Errorf("err = %v, want parse-line error", err)
	}
}

func TestExtractDiff_BadDiffPayload(t *testing.T) {
	// Payload is a JSON array, not the expected object shape →
	// json.Unmarshal into gitDiffPayload fails.
	lines := []Line{
		{Seq: 1, Kind: EventKindGitDiff, Data: json.RawMessage(`[1,2,3]`)},
	}
	_, err := ExtractDiff(packLines(t, lines))
	if err == nil || !strings.Contains(err.Error(), "parse git_diff payload") {
		t.Errorf("err = %v, want parse-payload error", err)
	}
}

func TestReadEvents_AllLinesReturned(t *testing.T) {
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{}`)},
		{Seq: 2, Kind: "git_diff", Data: json.RawMessage(`{"kind":"name_status","files":[]}`)},
		{Seq: 3, Kind: "policy_event", Data: json.RawMessage(`{}`)},
		{Seq: 4, Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
	got, err := ReadEvents(packLines(t, lines))
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(got) != 4 {
		t.Errorf("len = %d, want 4", len(got))
	}
	for i, want := range []string{"manifest", "git_diff", "policy_event", "trailer"} {
		if got[i].Kind != want {
			t.Errorf("got[%d].Kind = %q, want %q", i, got[i].Kind, want)
		}
	}
}

func TestExtractDiff_StatusValuesRoundTrip(t *testing.T) {
	lines := []Line{
		makeDiffLine(t, "origin/main",
			[2]string{"a.go", "A"},
			[2]string{"b.go", "M"},
			[2]string{"c.go", "D"},
			[2]string{"d.go", "R"},
		),
	}
	got, err := ExtractDiff(packLines(t, lines))
	if err != nil {
		t.Fatalf("ExtractDiff: %v", err)
	}
	want := []policy.Status{
		policy.StatusAdded,
		policy.StatusModified,
		policy.StatusDeleted,
		policy.StatusRenamed,
	}
	if len(got.ChangedFiles) != len(want) {
		t.Fatalf("got %d files, want %d", len(got.ChangedFiles), len(want))
	}
	for i, w := range want {
		if got.ChangedFiles[i].Status != w {
			t.Errorf("file %d status = %q, want %q", i, got.ChangedFiles[i].Status, w)
		}
	}
}
