package bundle

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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

func TestExtractManifest_PushFixupRoundTrips(t *testing.T) {
	// #794 wire-flag lockstep: the read side must decode the exact
	// `push_fixup` json key the runner stamps. No schema-sync CI guards this
	// wire format, so this test pins the backend half of the contract.
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{
			"bundle_schema":"v1",
			"run_id":"run-1",
			"stage_id":"stage-1",
			"agent":"claude-code",
			"push_fixup":true
		}`)},
		{Seq: 2, Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
	got, err := ExtractManifest(packLines(t, lines))
	if err != nil {
		t.Fatalf("ExtractManifest: %v", err)
	}
	if !got.PushFixup {
		t.Error("PushFixup = false, want true")
	}
}

func TestExtractManifest_OlderBundleParsesPushFixupAsFalse(t *testing.T) {
	// An older bundle (and every non-fix-up stage) omits the field; the read
	// side must default to PushFixup=false so the prior trace-driven transition
	// is preserved.
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
	if got.PushFixup {
		t.Error("PushFixup = true on a bundle without the field")
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

func TestExtractDiff_RoundTripsPatch(t *testing.T) {
	// The git_diff event's unified-diff patch text round-trips through
	// ExtractDiff into policy.Diff.Patch for the implement-review prompt
	// (#585). ChangedFiles is unaffected.
	patch := "diff --git a/a.go b/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	payload, err := json.Marshal(gitDiffPayload{
		Kind:           "name_status",
		BaseRef:        "origin/main",
		Files:          []gitDiffEntry{{Path: "a.go", Status: "M"}},
		NumFiles:       1,
		Patch:          patch,
		PatchTruncated: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{"bundle_schema":"v1"}`)},
		{Seq: 2, Kind: EventKindGitDiff, Data: payload},
	}
	got, err := ExtractDiff(packLines(t, lines))
	if err != nil {
		t.Fatalf("ExtractDiff: %v", err)
	}
	if got.Patch != patch {
		t.Errorf("Patch = %q, want %q", got.Patch, patch)
	}
	if len(got.ChangedFiles) != 1 || got.ChangedFiles[0].Path != "a.go" {
		t.Errorf("ChangedFiles = %+v, want single a.go", got.ChangedFiles)
	}
}

func TestExtractDiff_OlderBundleDecodesEmptyPatch(t *testing.T) {
	// A bundle WITHOUT the patch field (older runner) decodes to an
	// empty Patch — backward-compatible additive field (#585).
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{"bundle_schema":"v1"}`)},
		makeDiffLine(t, "origin/main", [2]string{"a.go", "M"}),
	}
	got, err := ExtractDiff(packLines(t, lines))
	if err != nil {
		t.Fatalf("ExtractDiff: %v", err)
	}
	if got.Patch != "" {
		t.Errorf("Patch = %q, want empty for a bundle without the field", got.Patch)
	}
	if len(got.ChangedFiles) != 1 {
		t.Errorf("ChangedFiles = %+v, want one file", got.ChangedFiles)
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

func TestExtractDiff_LastDiffEventWins(t *testing.T) {
	// When two git_diff events appear, the LAST is authoritative (#870): the
	// runner re-emits a fresh scope-only diff after a verify-fix reinvoke, and
	// that reconciled diff is the committed tree the PR ships. Assert both the
	// file set AND the patch come from the last event, not the stale first.
	first, err := json.Marshal(gitDiffPayload{
		Kind: "name_status", BaseRef: "origin/main",
		Files:    []gitDiffEntry{{Path: "a.go", Status: "M"}},
		NumFiles: 1,
		Patch:    "diff --git a/a.go b/a.go\n@@ -1 +1 @@\n-old\n+stale\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	last, err := json.Marshal(gitDiffPayload{
		Kind: "name_status", BaseRef: "origin/main",
		Files:    []gitDiffEntry{{Path: "b.go", Status: "M"}, {Path: "c.go", Status: "A"}},
		NumFiles: 2,
		Patch:    "diff --git a/b.go b/b.go\n@@ -1 +1 @@\n-old\n+reconciled\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := []Line{
		{Seq: 1, Kind: EventKindGitDiff, Data: first},
		{Seq: 2, Kind: EventKindGitDiff, Data: last},
	}
	got, err := ExtractDiff(packLines(t, lines))
	if err != nil {
		t.Fatalf("ExtractDiff: %v", err)
	}
	if len(got.ChangedFiles) != 2 || got.ChangedFiles[0].Path != "b.go" || got.ChangedFiles[1].Path != "c.go" {
		t.Errorf("expected the LAST event's file set, got %+v", got.ChangedFiles)
	}
	if got.Patch != "diff --git a/b.go b/b.go\n@@ -1 +1 @@\n-old\n+reconciled\n" {
		t.Errorf("expected the LAST event's patch, got %q", got.Patch)
	}
}

func TestExtractDiff_SingleEventBackCompat(t *testing.T) {
	// Every bundle without a reconciling reinvoke carries exactly one git_diff,
	// so last == first — last-write-wins must be identical to the old behavior.
	lines := []Line{
		makeDiffLine(t, "origin/main", [2]string{"a.go", "M"}),
	}
	got, err := ExtractDiff(packLines(t, lines))
	if err != nil {
		t.Fatalf("ExtractDiff: %v", err)
	}
	if len(got.ChangedFiles) != 1 || got.ChangedFiles[0].Path != "a.go" {
		t.Errorf("single-event bundle = %+v, want one file a.go", got.ChangedFiles)
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

func TestExtractScopeDrift_HappyPath(t *testing.T) {
	// The runner's scope_drift policy_event carries the undeclared paths
	// the operator may stage; ExtractScopeDrift round-trips them (#695).
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{"bundle_schema":"v1"}`)},
		makeDiffLine(t, "origin/main", [2]string{"a.go", "M"}),
		{Seq: 3, Kind: EventKindPolicyEvent, Data: json.RawMessage(
			`{"check":"scope_drift","outcome":"excluded","undeclared":["a_test.go","docs/x.md"]}`)},
		{Seq: 4, Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
	got, err := ExtractScopeDrift(packLines(t, lines))
	if err != nil {
		t.Fatalf("ExtractScopeDrift: %v", err)
	}
	want := []string{"a_test.go", "docs/x.md"}
	if len(got) != len(want) {
		t.Fatalf("got %d paths, want %d (%v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("path %d = %q, want %q", i, got[i], w)
		}
	}
}

func TestExtractScopeDrift_NoEvent(t *testing.T) {
	// No scope_drift event is the ordinary no-drift case — (nil, nil),
	// not an error. A non-drift policy_event is skipped, not mistaken.
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{"bundle_schema":"v1"}`)},
		makeDiffLine(t, "origin/main", [2]string{"a.go", "M"}),
		{Seq: 3, Kind: EventKindPolicyEvent, Data: json.RawMessage(`{"check":"constraints","outcome":"valid"}`)},
		{Seq: 4, Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
	got, err := ExtractScopeDrift(packLines(t, lines))
	if err != nil {
		t.Fatalf("ExtractScopeDrift: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil for a bundle with no scope_drift event", got)
	}
}

func TestExtractScopeDrift_BadGzip(t *testing.T) {
	_, err := ExtractScopeDrift([]byte("not gzipped"))
	if !errors.Is(err, ErrBadGzip) {
		t.Errorf("err = %v, want ErrBadGzip", err)
	}
}

func TestExtractScopeDrift_BadPayload(t *testing.T) {
	// A scope_drift policy_event whose `undeclared` is a string, not an
	// array → json.Unmarshal into scopeDriftPayload fails. The trace
	// handler WARN-degrades to nil drift, but this branch must still
	// surface the error rather than silently returning empty (#695
	// implement-review finding — distinct from the bad-gzip ReadEvents path).
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{"bundle_schema":"v1"}`)},
		{Seq: 2, Kind: EventKindPolicyEvent, Data: json.RawMessage(
			`{"check":"scope_drift","outcome":"excluded","undeclared":"not-an-array"}`)},
	}
	_, err := ExtractScopeDrift(packLines(t, lines))
	if err == nil || !strings.Contains(err.Error(), "parse policy_event payload") {
		t.Errorf("err = %v, want a parse-payload error", err)
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

func TestExtractTiming_HappyPath(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(12 * time.Minute)
	lines := []Line{
		{Seq: 1, Kind: EventKindManifest, Timestamp: t0.Add(-1 * time.Second), Data: json.RawMessage(`{}`)},
		{Seq: 2, Kind: "agent_start", Timestamp: t0, Data: json.RawMessage(`{}`)},
		{Seq: 3, Kind: "agent_event", Timestamp: t0.Add(6 * time.Minute), Data: json.RawMessage(`{}`)},
		{Seq: 4, Kind: "agent_end", Timestamp: t1, Data: json.RawMessage(`{}`)},
		{Seq: 5, Kind: "trailer", Timestamp: t1.Add(time.Second), Data: json.RawMessage(`{}`)},
	}
	startedAt, endedAt, ok := ExtractTiming(packLines(t, lines))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if !startedAt.Equal(t0) {
		t.Errorf("startedAt = %v, want %v", startedAt, t0)
	}
	if !endedAt.Equal(t1) {
		t.Errorf("endedAt = %v, want %v", endedAt, t1)
	}
}

func TestExtractTiming_ManifestAndTrailerOnly_ReturnsFalse(t *testing.T) {
	lines := []Line{
		{Seq: 1, Kind: EventKindManifest, Data: json.RawMessage(`{}`)},
		{Seq: 2, Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
	_, _, ok := ExtractTiming(packLines(t, lines))
	if ok {
		t.Error("ok = true, want false for manifest+trailer-only bundle")
	}
}

func TestExtractTiming_SingleIntermediateEvent_ReturnsFalse(t *testing.T) {
	lines := []Line{
		{Seq: 1, Kind: EventKindManifest, Data: json.RawMessage(`{}`)},
		{Seq: 2, Kind: "agent_start", Data: json.RawMessage(`{}`)},
		{Seq: 3, Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
	_, _, ok := ExtractTiming(packLines(t, lines))
	if ok {
		t.Error("ok = true, want false for single intermediate event")
	}
}

func TestExtractTiming_BadBundle_ReturnsFalse(t *testing.T) {
	_, _, ok := ExtractTiming([]byte("not-gzip"))
	if ok {
		t.Error("ok = true, want false for bad bundle bytes")
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

// makeVerifyRunLine builds a verify_run event line carrying the given
// head_sha (#797). Mirrors the runner's verifyRunEvent payload shape.
func makeVerifyRunLine(t *testing.T, seq int, headSHA string) Line {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"command":   "go build ./...",
		"head_sha":  headSHA,
		"exit_code": 0,
		"output":    "",
		"outcome":   "passed",
	})
	if err != nil {
		t.Fatal(err)
	}
	return Line{Seq: seq, Kind: EventKindVerifyRun, Data: payload}
}

func TestExtractHeadSHA_HappyPath(t *testing.T) {
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{"bundle_schema":"v1"}`)},
		makeDiffLine(t, "origin/main", [2]string{"a.go", "M"}),
		makeVerifyRunLine(t, 3, "deadbeefcafe"),
	}
	got, err := ExtractHeadSHA(packLines(t, lines))
	if err != nil {
		t.Fatalf("ExtractHeadSHA: %v", err)
	}
	if got != "deadbeefcafe" {
		t.Errorf("head_sha = %q, want deadbeefcafe", got)
	}
}

func TestExtractHeadSHA_NoVerifyRun(t *testing.T) {
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{"bundle_schema":"v1"}`)},
		makeDiffLine(t, "origin/main", [2]string{"a.go", "M"}),
	}
	got, err := ExtractHeadSHA(packLines(t, lines))
	if !errors.Is(err, ErrNoHeadSHA) {
		t.Errorf("err = %v, want ErrNoHeadSHA", err)
	}
	if got != "" {
		t.Errorf("head_sha = %q, want empty", got)
	}
}

// A gate-skipped / infra-failure verify_run carries an empty head_sha; when
// it precedes a real verify_run, ExtractHeadSHA must skip the empty one and
// return the first NON-EMPTY SHA (binding approval refinement on step 1).
func TestExtractHeadSHA_SkipsEmptyReturnsFirstNonEmpty(t *testing.T) {
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{"bundle_schema":"v1"}`)},
		makeVerifyRunLine(t, 2, ""),        // gate-skipped: empty head_sha
		makeVerifyRunLine(t, 3, "realsha"), // later real verify
	}
	got, err := ExtractHeadSHA(packLines(t, lines))
	if err != nil {
		t.Fatalf("ExtractHeadSHA: %v", err)
	}
	if got != "realsha" {
		t.Errorf("head_sha = %q, want realsha", got)
	}
}

// A bundle whose only verify_run events carry empty head_shas is treated as
// head_sha-less → ErrNoHeadSHA (fail open to the variant gate).
func TestExtractHeadSHA_OnlyEmptyReturnsErrNoHeadSHA(t *testing.T) {
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{"bundle_schema":"v1"}`)},
		makeVerifyRunLine(t, 2, ""),
	}
	_, err := ExtractHeadSHA(packLines(t, lines))
	if !errors.Is(err, ErrNoHeadSHA) {
		t.Errorf("err = %v, want ErrNoHeadSHA", err)
	}
}

func TestExtractHeadSHA_BadGzip(t *testing.T) {
	_, err := ExtractHeadSHA([]byte("not gzipped"))
	if !errors.Is(err, ErrBadGzip) {
		t.Errorf("err = %v, want ErrBadGzip", err)
	}
}

func TestExtractHeadSHA_BadPayload(t *testing.T) {
	lines := []Line{
		{Seq: 1, Kind: EventKindVerifyRun, Data: json.RawMessage(`[1,2,3]`)},
	}
	_, err := ExtractHeadSHA(packLines(t, lines))
	if err == nil || !strings.Contains(err.Error(), "parse verify_run payload") {
		t.Errorf("err = %v, want parse-payload error", err)
	}
}

// The raw and redacted variants of one pack carry the IDENTICAL verify_run
// head_sha — the discrimination the (stage_id, head_sha) dedup key relies on
// (#797). Redaction strips secrets from event output, never the git SHA, so
// both variants of the same pack return the same value here.
func TestExtractHeadSHA_RawAndRedactedVariantsMatch(t *testing.T) {
	raw := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{"bundle_schema":"v1"}`)},
		makeVerifyRunLine(t, 2, "samesha123"),
	}
	// Redacted variant: same head_sha, output field redacted.
	redactedPayload, err := json.Marshal(map[string]any{
		"command":   "go build ./...",
		"head_sha":  "samesha123",
		"exit_code": 0,
		"output":    "[redacted]",
		"outcome":   "passed",
	})
	if err != nil {
		t.Fatal(err)
	}
	redacted := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{"bundle_schema":"v1"}`)},
		{Seq: 2, Kind: EventKindVerifyRun, Data: redactedPayload},
	}
	gotRaw, err := ExtractHeadSHA(packLines(t, raw))
	if err != nil {
		t.Fatalf("ExtractHeadSHA(raw): %v", err)
	}
	gotRedacted, err := ExtractHeadSHA(packLines(t, redacted))
	if err != nil {
		t.Fatalf("ExtractHeadSHA(redacted): %v", err)
	}
	if gotRaw != gotRedacted {
		t.Errorf("raw head_sha %q != redacted head_sha %q", gotRaw, gotRedacted)
	}
}

// gateEvidenceLine builds a runner-shaped gate_evidence event line (#963):
// the exact JSON the runner's composeGateEvidence packs, so these tests pin
// the lockstep wire contract field-by-field.
func gateEvidenceLine(t *testing.T, seq int) Line {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"verify_runs": []map[string]any{
			{
				"command":        "scripts/test",
				"head_sha":       "abc123",
				"tree_sha":       "def456",
				"exit_code":      2,
				"outcome":        "failed",
				"output_tail":    "FAIL\tgithub.com/kuhlman-labs/fishhawk/backend/internal/foo [build failed]",
				"tail_truncated": true,
			},
			{
				"command":   "scripts/test",
				"exit_code": -1,
				"outcome":   "skipped",
				// Skip paths carry the reason in the tail.
				"output_tail": "stage_scoped: worktree busy",
			},
		},
		"verify_summary": map[string]any{
			"outcome":        "failed",
			"iterations":     2,
			"max_iterations": 3,
			"detail":         "budget exhausted",
		},
		"flake_retries": 1,
		"scope_facts": map[string]any{
			"declared_files":   5,
			"staged_files":     4,
			"undeclared_paths": []string{"backend/internal/foo/foo_test.go"},
			"undeclared_categorized": []map[string]any{
				{"path": "backend/internal/foo/foo_test.go", "category": "A", "disposition": "excluded_from_commit"},
			},
		},
		"policy_violations": []map[string]any{
			{
				"check":      "constraints",
				"constraint": "forbidden_paths",
				"detail":     "path matches forbidden glob",
				"files":      []string{".github/workflows/ci.yml"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return Line{Seq: seq, Kind: EventKindGateEvidence, Data: payload}
}

func TestExtractGateEvidence_HappyPath(t *testing.T) {
	// Round-trip a runner-shaped gate_evidence payload (#963). Every json
	// tag asserted here is the lockstep runner↔backend wire contract —
	// a silent zero value below means the tags diverged from the composer.
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{"bundle_schema":"v1"}`)},
		makeDiffLine(t, "origin/main", [2]string{"a.go", "M"}),
		gateEvidenceLine(t, 3),
		{Seq: 4, Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
	got, err := ExtractGateEvidence(packLines(t, lines))
	if err != nil {
		t.Fatalf("ExtractGateEvidence: %v", err)
	}
	if len(got.VerifyRuns) != 2 {
		t.Fatalf("got %d verify runs, want 2", len(got.VerifyRuns))
	}
	vr := got.VerifyRuns[0]
	if vr.Command != "scripts/test" || vr.ExitCode != 2 || vr.Outcome != "failed" {
		t.Errorf("verify run 0 = %+v, want command=scripts/test exit=2 outcome=failed", vr)
	}
	if vr.HeadSHA != "abc123" || vr.TreeSHA != "def456" {
		t.Errorf("verify run 0 SHAs = %q/%q, want abc123/def456", vr.HeadSHA, vr.TreeSHA)
	}
	if !strings.Contains(vr.OutputTail, "[build failed]") {
		t.Errorf("verify run 0 tail %q missing [build failed]", vr.OutputTail)
	}
	if !vr.TailTruncated {
		t.Error("verify run 0 TailTruncated = false, want true")
	}
	if got.VerifyRuns[1].Outcome != "skipped" || got.VerifyRuns[1].OutputTail != "stage_scoped: worktree busy" {
		t.Errorf("verify run 1 = %+v, want skipped with skip reason in tail", got.VerifyRuns[1])
	}
	if got.VerifySummary == nil {
		t.Fatal("VerifySummary = nil, want populated")
	}
	if got.VerifySummary.Outcome != "failed" || got.VerifySummary.Iterations != 2 ||
		got.VerifySummary.MaxIterations != 3 || got.VerifySummary.Detail != "budget exhausted" {
		t.Errorf("VerifySummary = %+v, want failed 2/3 with detail", got.VerifySummary)
	}
	if got.FlakeRetries != 1 {
		t.Errorf("FlakeRetries = %d, want 1", got.FlakeRetries)
	}
	if got.ScopeFacts == nil {
		t.Fatal("ScopeFacts = nil, want populated")
	}
	if got.ScopeFacts.DeclaredFiles != 5 {
		t.Errorf("ScopeFacts.DeclaredFiles = %d, want 5", got.ScopeFacts.DeclaredFiles)
	}
	if got.ScopeFacts.StagedFiles == nil || *got.ScopeFacts.StagedFiles != 4 {
		t.Errorf("ScopeFacts.StagedFiles = %v, want pointer to 4", got.ScopeFacts.StagedFiles)
	}
	if len(got.ScopeFacts.UndeclaredPaths) != 1 || got.ScopeFacts.UndeclaredPaths[0] != "backend/internal/foo/foo_test.go" {
		t.Errorf("ScopeFacts.UndeclaredPaths = %v, want the drifted test path", got.ScopeFacts.UndeclaredPaths)
	}
	wantDP := DriftPathEvidence{
		Path: "backend/internal/foo/foo_test.go", Category: "A", Disposition: "excluded_from_commit",
	}
	if len(got.ScopeFacts.UndeclaredCategorized) != 1 || got.ScopeFacts.UndeclaredCategorized[0] != wantDP {
		t.Errorf("ScopeFacts.UndeclaredCategorized = %+v, want [%+v]", got.ScopeFacts.UndeclaredCategorized, wantDP)
	}
	if len(got.PolicyViolations) != 1 {
		t.Fatalf("got %d policy violations, want 1", len(got.PolicyViolations))
	}
	pv := got.PolicyViolations[0]
	if pv.Check != "constraints" || pv.Constraint != "forbidden_paths" ||
		pv.Detail != "path matches forbidden glob" ||
		len(pv.Files) != 1 || pv.Files[0] != ".github/workflows/ci.yml" {
		t.Errorf("PolicyViolations[0] = %+v, want the forbidden_paths entry", pv)
	}
}

func TestExtractGateEvidence_OlderBundleWithoutCategorizedDrift(t *testing.T) {
	// Tolerant-decode contract (#991): a bundle from an older runner
	// whose scope_facts has undeclared_paths but no undeclared_categorized
	// key decodes with a nil categorized slice — UndeclaredPaths stays
	// the authoritative list and downstream renders the uncategorized
	// lines exactly as before.
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{"bundle_schema":"v1"}`)},
		{Seq: 2, Kind: EventKindGateEvidence, Data: json.RawMessage(
			`{"scope_facts":{"declared_files":3,"staged_files":2,"undeclared_paths":["stray.go"]}}`)},
		{Seq: 3, Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
	got, err := ExtractGateEvidence(packLines(t, lines))
	if err != nil {
		t.Fatalf("ExtractGateEvidence: %v", err)
	}
	if got.ScopeFacts == nil {
		t.Fatal("ScopeFacts = nil, want populated")
	}
	if len(got.ScopeFacts.UndeclaredPaths) != 1 || got.ScopeFacts.UndeclaredPaths[0] != "stray.go" {
		t.Errorf("UndeclaredPaths = %v, want [stray.go]", got.ScopeFacts.UndeclaredPaths)
	}
	if got.ScopeFacts.UndeclaredCategorized != nil {
		t.Errorf("UndeclaredCategorized = %+v, want nil on an older bundle", got.ScopeFacts.UndeclaredCategorized)
	}
}

func TestExtractGateEvidence_NoEvent(t *testing.T) {
	// A pre-#963 fixture bundle — manifest, git_diff, a raw verify_run,
	// a scope_drift policy_event, but NO gate_evidence — is the ordinary
	// older-bundle / no-gate case: zero value + ErrNoGateEvidence, never
	// a hard error (fail-open, mirrors ErrNoHeadSHA).
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{"bundle_schema":"v1"}`)},
		makeDiffLine(t, "origin/main", [2]string{"a.go", "M"}),
		makeVerifyRunLine(t, 3, "abc123"),
		{Seq: 4, Kind: EventKindPolicyEvent, Data: json.RawMessage(
			`{"check":"scope_drift","outcome":"excluded","undeclared":["a_test.go"]}`)},
		{Seq: 5, Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
	_, err := ExtractGateEvidence(packLines(t, lines))
	if !errors.Is(err, ErrNoGateEvidence) {
		t.Errorf("err = %v, want ErrNoGateEvidence", err)
	}
}

func TestExtractGateEvidence_BadGzip(t *testing.T) {
	_, err := ExtractGateEvidence([]byte("not gzipped"))
	if !errors.Is(err, ErrBadGzip) {
		t.Errorf("err = %v, want ErrBadGzip", err)
	}
}

func TestExtractGateEvidence_BadPayload(t *testing.T) {
	// A gate_evidence payload whose verify_runs is a string, not an array
	// → unmarshal fails. The trace handler WARN-degrades to nil evidence,
	// but the extractor must surface the error rather than silently
	// returning an empty digest.
	lines := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{"bundle_schema":"v1"}`)},
		{Seq: 2, Kind: EventKindGateEvidence, Data: json.RawMessage(`{"verify_runs":"not-an-array"}`)},
	}
	_, err := ExtractGateEvidence(packLines(t, lines))
	if err == nil || !strings.Contains(err.Error(), "parse gate_evidence payload") {
		t.Errorf("err = %v, want a parse-payload error", err)
	}
}

func TestGateEvidenceEvent_DoesNotPerturbOtherExtractors(t *testing.T) {
	// Additive wire-format property (#963): every pre-change extractor
	// filters by Kind, so a bundle that ALSO carries a gate_evidence event
	// must return identical results from ExtractDiff / ExtractScopeDrift /
	// ExtractHeadSHA / ExtractManifest as the same bundle without it.
	base := []Line{
		{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{"bundle_schema":"v1"}`)},
		makeDiffLine(t, "origin/main", [2]string{"a.go", "M"}),
		makeVerifyRunLine(t, 3, "abc123"),
		{Seq: 4, Kind: EventKindPolicyEvent, Data: json.RawMessage(
			`{"check":"scope_drift","outcome":"excluded","undeclared":["a_test.go"]}`)},
		{Seq: 5, Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
	withEvidence := append(append([]Line{}, base[:4]...), gateEvidenceLine(t, 5), base[4])

	packedBase := packLines(t, base)
	packedEvidence := packLines(t, withEvidence)

	diffBase, err := ExtractDiff(packedBase)
	if err != nil {
		t.Fatalf("ExtractDiff(base): %v", err)
	}
	diffEv, err := ExtractDiff(packedEvidence)
	if err != nil {
		t.Fatalf("ExtractDiff(withEvidence): %v", err)
	}
	if len(diffEv.ChangedFiles) != len(diffBase.ChangedFiles) {
		t.Errorf("ExtractDiff perturbed: %d files vs %d", len(diffEv.ChangedFiles), len(diffBase.ChangedFiles))
	}

	driftBase, _ := ExtractScopeDrift(packedBase)
	driftEv, err := ExtractScopeDrift(packedEvidence)
	if err != nil {
		t.Fatalf("ExtractScopeDrift(withEvidence): %v", err)
	}
	if len(driftEv) != len(driftBase) || driftEv[0] != driftBase[0] {
		t.Errorf("ExtractScopeDrift perturbed: %v vs %v", driftEv, driftBase)
	}

	shaBase, _ := ExtractHeadSHA(packedBase)
	shaEv, err := ExtractHeadSHA(packedEvidence)
	if err != nil {
		t.Fatalf("ExtractHeadSHA(withEvidence): %v", err)
	}
	if shaEv != shaBase {
		t.Errorf("ExtractHeadSHA perturbed: %q vs %q", shaEv, shaBase)
	}

	mBase, _ := ExtractManifest(packedBase)
	mEv, err := ExtractManifest(packedEvidence)
	if err != nil {
		t.Fatalf("ExtractManifest(withEvidence): %v", err)
	}
	if mEv != mBase {
		t.Errorf("ExtractManifest perturbed: %+v vs %+v", mEv, mBase)
	}
}
