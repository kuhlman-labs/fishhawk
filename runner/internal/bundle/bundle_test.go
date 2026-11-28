package bundle

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// frozenNow returns a deterministic clock so manifest /
// per-line timestamps are stable across runs.
func frozenNow() func() time.Time {
	t := time.Date(2026, 5, 2, 9, 30, 0, 0, time.UTC)
	return func() time.Time {
		t = t.Add(time.Second)
		return t
	}
}

func sampleEvents() []agent.Event {
	return []agent.Event{
		{
			Kind:      "system.init",
			Timestamp: time.Date(2026, 5, 2, 9, 31, 0, 0, time.UTC),
			Payload:   agent.MakePayload(map[string]string{"agent": "claude-code"}),
		},
		{
			Kind:      "tool_call",
			Timestamp: time.Date(2026, 5, 2, 9, 31, 5, 0, time.UTC),
			Payload:   agent.MakePayload(map[string]string{"name": "bash"}),
		},
		{
			Kind:      "result",
			Timestamp: time.Date(2026, 5, 2, 9, 31, 10, 0, time.UTC),
			Payload:   agent.MakePayload(map[string]int{"input_tokens": 50, "output_tokens": 75}),
		},
	}
}

func TestPack_RoundTrip(t *testing.T) {
	in := PackInputs{
		RunID:        "11111111-2222-3333-4444-555555555555",
		StageID:      "22222222-3333-4444-5555-666666666666",
		Agent:        "claude-code",
		Model:        "claude-opus-4-7",
		InputTokens:  200,
		OutputTokens: 80,
		Now:          frozenNow(),
	}
	events := sampleEvents()

	data, hash, err := PackBytes(in, events)
	if err != nil {
		t.Fatalf("PackBytes: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("PackBytes returned empty data")
	}
	if len(hash) != 64 {
		t.Errorf("storage hash len = %d, want 64 (hex sha256)", len(hash))
	}

	manifest, gotEvents, trailer, err := Open(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if manifest.BundleSchema != SchemaV1 {
		t.Errorf("manifest schema = %q, want %q", manifest.BundleSchema, SchemaV1)
	}
	if manifest.RunID != in.RunID {
		t.Errorf("manifest RunID = %q, want %q", manifest.RunID, in.RunID)
	}
	if manifest.StageID != in.StageID {
		t.Errorf("manifest StageID = %q, want %q", manifest.StageID, in.StageID)
	}
	if manifest.Agent != "claude-code" {
		t.Errorf("manifest Agent = %q", manifest.Agent)
	}
	if manifest.Model != "claude-opus-4-7" {
		t.Errorf("manifest Model = %q", manifest.Model)
	}
	if manifest.InputTokens != 200 || manifest.OutputTokens != 80 {
		t.Errorf("manifest token split = (%d,%d), want (200,80)", manifest.InputTokens, manifest.OutputTokens)
	}
	if len(gotEvents) != len(events) {
		t.Fatalf("got %d events, want %d", len(gotEvents), len(events))
	}
	for i, ev := range gotEvents {
		if ev.Kind != events[i].Kind {
			t.Errorf("events[%d].Kind = %q, want %q", i, ev.Kind, events[i].Kind)
		}
		if ev.Seq != i+2 { // manifest is seq 1
			t.Errorf("events[%d].Seq = %d, want %d", i, ev.Seq, i+2)
		}
	}
	if trailer.EventCount != len(events) {
		t.Errorf("trailer EventCount = %d, want %d", trailer.EventCount, len(events))
	}
	if len(trailer.ContentHash) != 64 {
		t.Errorf("trailer content_hash len = %d, want 64", len(trailer.ContentHash))
	}
}

// ADR-044 / #1349: the runner carries the prompt-cache split
// (cache-read / cache-write input tokens) through the manifest so the
// backend prices cache reads at the discount and writes at the premium.
// Round-trip both buckets through Pack + Open.
func TestPack_CacheTokensRoundTrip(t *testing.T) {
	in := PackInputs{
		RunID:                 "11111111-2222-3333-4444-555555555555",
		StageID:               "22222222-3333-4444-5555-666666666666",
		Agent:                 "claude-code",
		Model:                 "claude-opus-4-8",
		InputTokens:           200,
		OutputTokens:          80,
		CacheReadInputTokens:  400,
		CacheWriteInputTokens: 150,
		Now:                   frozenNow(),
	}
	data, _, err := PackBytes(in, sampleEvents())
	if err != nil {
		t.Fatalf("PackBytes: %v", err)
	}
	manifest, _, _, err := Open(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if manifest.CacheReadInputTokens != 400 || manifest.CacheWriteInputTokens != 150 {
		t.Errorf("manifest cache split = (read %d, write %d), want (400, 150)",
			manifest.CacheReadInputTokens, manifest.CacheWriteInputTokens)
	}
	// InputTokens stays the fresh (cache-exclusive) input, unchanged.
	if manifest.InputTokens != 200 || manifest.OutputTokens != 80 {
		t.Errorf("manifest token split = (%d,%d), want (200,80)",
			manifest.InputTokens, manifest.OutputTokens)
	}
}

// TestPack_CacheTokensDefaultZeroAndOmitField pins the back-compat
// contract: a bundle packed without the cache buckets omits both fields
// on the wire (omitempty) and decodes to 0 — so an older bundle without
// the fields is priced as no cache. (#1349)
func TestPack_CacheTokensDefaultZeroAndOmitField(t *testing.T) {
	in := PackInputs{
		RunID:        "11111111-2222-3333-4444-555555555555",
		StageID:      "22222222-3333-4444-5555-666666666666",
		Agent:        "claude-code",
		InputTokens:  200,
		OutputTokens: 80,
		Now:          frozenNow(),
	}
	data, _, err := PackBytes(in, sampleEvents())
	if err != nil {
		t.Fatalf("PackBytes: %v", err)
	}
	manifest, _, _, err := Open(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if manifest.CacheReadInputTokens != 0 || manifest.CacheWriteInputTokens != 0 {
		t.Errorf("cache split on a no-cache bundle = (read %d, write %d), want (0, 0)",
			manifest.CacheReadInputTokens, manifest.CacheWriteInputTokens)
	}
	// omitempty must drop both keys from the wire when zero.
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if bytes.Contains(raw, []byte("cache_read_input_tokens")) {
		t.Errorf("omitempty failed: cache_read_input_tokens present on a no-cache bundle:\n%s", raw)
	}
	if bytes.Contains(raw, []byte("cache_write_input_tokens")) {
		t.Errorf("omitempty failed: cache_write_input_tokens present on a no-cache bundle:\n%s", raw)
	}
}

// E8.5 (#163): the runner stamps category-A failures into the
// bundle manifest so the backend's trace handler can route to
// FailStage(FailureA, …) without re-running the agent. Round-trip
// the new fields through Pack + Open.
func TestPack_AgentFailedFlagRoundTrips(t *testing.T) {
	in := PackInputs{
		RunID:              "11111111-2222-3333-4444-555555555555",
		StageID:            "22222222-3333-4444-5555-666666666666",
		Agent:              "claude-code",
		AgentFailed:        true,
		AgentFailureReason: "agent process exited with status 137 (OOM)",
		Now:                frozenNow(),
	}
	data, _, err := PackBytes(in, sampleEvents())
	if err != nil {
		t.Fatalf("PackBytes: %v", err)
	}
	manifest, _, _, err := Open(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !manifest.AgentFailed {
		t.Error("manifest.AgentFailed = false, want true")
	}
	if manifest.AgentFailureReason != in.AgentFailureReason {
		t.Errorf("manifest.AgentFailureReason = %q, want %q",
			manifest.AgentFailureReason, in.AgentFailureReason)
	}
}

func TestPack_AgentFailedDefaultsFalseAndOmitsField(t *testing.T) {
	// omitempty keeps older bundles (without the field) parsing as
	// AgentFailed=false. Lock that in by checking the on-the-wire
	// JSON of a bundle packed without AgentFailed set.
	in := PackInputs{
		RunID:   "11111111-2222-3333-4444-555555555555",
		StageID: "22222222-3333-4444-5555-666666666666",
		Agent:   "claude-code",
		Now:     frozenNow(),
	}
	data, _, err := PackBytes(in, sampleEvents())
	if err != nil {
		t.Fatalf("PackBytes: %v", err)
	}
	manifest, _, _, err := Open(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if manifest.AgentFailed {
		t.Error("manifest.AgentFailed = true on a no-failure bundle")
	}
}

func TestPack_PushFixupFlagRoundTrips(t *testing.T) {
	// #794 wire-flag lockstep: the push_fixup field must marshal under the
	// exact json key and round-trip through Open. There is no schema-sync CI
	// for this wire format, so this test plus the backend reader's decode test
	// are what keep the two modules in lockstep.
	in := PackInputs{
		RunID:     "11111111-2222-3333-4444-555555555555",
		StageID:   "22222222-3333-4444-5555-666666666666",
		Agent:     "claude-code",
		PushFixup: true,
		Now:       frozenNow(),
	}
	data, _, err := PackBytes(in, sampleEvents())
	if err != nil {
		t.Fatalf("PackBytes: %v", err)
	}
	manifest, _, _, err := Open(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !manifest.PushFixup {
		t.Error("manifest.PushFixup = false, want true")
	}

	// Assert the exact on-the-wire json key so a rename can't silently drift
	// from the backend reader's `json:"push_fixup"` tag.
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"push_fixup":true`)) {
		t.Errorf("bundle manifest missing exact key %q; got:\n%s", `"push_fixup":true`, raw)
	}
}

func TestPack_PushFixupDefaultsFalseAndOmitsField(t *testing.T) {
	// omitempty back-compat: an older bundle (and every non-fix-up stage) packs
	// without the field and decodes to PushFixup=false. Lock that in by
	// asserting the key is absent from the wire bytes and decodes false.
	in := PackInputs{
		RunID:   "11111111-2222-3333-4444-555555555555",
		StageID: "22222222-3333-4444-5555-666666666666",
		Agent:   "claude-code",
		Now:     frozenNow(),
	}
	data, _, err := PackBytes(in, sampleEvents())
	if err != nil {
		t.Fatalf("PackBytes: %v", err)
	}
	manifest, _, _, err := Open(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if manifest.PushFixup {
		t.Error("manifest.PushFixup = true on a non-fix-up bundle")
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if bytes.Contains(raw, []byte("push_fixup")) {
		t.Errorf("omitempty failed: push_fixup present on a non-fix-up bundle:\n%s", raw)
	}
}

func TestPack_RunnerKindRoundTrips(t *testing.T) {
	// #1346/ADR-045 wire-flag lockstep: runner_kind must marshal under the
	// exact json key and round-trip through Open. There is no schema-sync CI
	// for this wire format, so this test plus the backend reader's decode test
	// are what keep the two modules in lockstep.
	in := PackInputs{
		RunID:      "11111111-2222-3333-4444-555555555555",
		StageID:    "22222222-3333-4444-5555-666666666666",
		Agent:      "claude-code",
		RunnerKind: "local",
		Now:        frozenNow(),
	}
	data, _, err := PackBytes(in, sampleEvents())
	if err != nil {
		t.Fatalf("PackBytes: %v", err)
	}
	manifest, _, _, err := Open(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if manifest.RunnerKind != "local" {
		t.Errorf("manifest.RunnerKind = %q, want %q", manifest.RunnerKind, "local")
	}

	// Assert the exact on-the-wire json key so a rename can't silently drift
	// from the backend reader's `json:"runner_kind"` tag.
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"runner_kind":"local"`)) {
		t.Errorf("bundle manifest missing exact key %q; got:\n%s", `"runner_kind":"local"`, raw)
	}
}

func TestPack_RunnerKindDefaultsEmptyAndOmitsField(t *testing.T) {
	// omitempty back-compat: an older / channel-less bundle packs without the
	// field and decodes to RunnerKind="" (the backend treats it as no
	// self-report and skips reconciliation). Lock that in by asserting the key
	// is absent from the wire bytes and decodes empty.
	in := PackInputs{
		RunID:   "11111111-2222-3333-4444-555555555555",
		StageID: "22222222-3333-4444-5555-666666666666",
		Agent:   "claude-code",
		Now:     frozenNow(),
	}
	data, _, err := PackBytes(in, sampleEvents())
	if err != nil {
		t.Fatalf("PackBytes: %v", err)
	}
	manifest, _, _, err := Open(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if manifest.RunnerKind != "" {
		t.Errorf("manifest.RunnerKind = %q, want empty on a channel-less bundle", manifest.RunnerKind)
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if bytes.Contains(raw, []byte("runner_kind")) {
		t.Errorf("omitempty failed: runner_kind present on a channel-less bundle:\n%s", raw)
	}
}

func TestPack_StorageHashIsDeterministic(t *testing.T) {
	// Same inputs + same Now produce byte-identical output and
	// thus the same storage hash. Required for content-addressed
	// dedup at the backend (ARCHITECTURE.md §5.2).
	in := PackInputs{
		RunID: "r", StageID: "s", Agent: "claude-code", Now: frozenNow(),
	}
	events := sampleEvents()

	d1, h1, err := PackBytes(in, events)
	if err != nil {
		t.Fatal(err)
	}
	d2, h2, err := PackBytes(PackInputs{
		RunID: "r", StageID: "s", Agent: "claude-code", Now: frozenNow(),
	}, events)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(d1, d2) {
		t.Error("identical inputs produced different bytes")
	}
	if h1 != h2 {
		t.Errorf("storage hash mismatch: %q vs %q", h1, h2)
	}
}

func TestPack_DetectsTampering(t *testing.T) {
	in := PackInputs{
		RunID: "r", StageID: "s", Agent: "claude-code", Now: frozenNow(),
	}
	data, _, err := PackBytes(in, sampleEvents())
	if err != nil {
		t.Fatal(err)
	}

	// Decompress, mutate one event line in the middle, recompress
	// (so gzip integrity stays intact), then Open should reject.
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := readAll(t, zr)
	tampered := bytes.Replace(raw, []byte(`"name":"bash"`), []byte(`"name":"DROP"`), 1)
	if bytes.Equal(raw, tampered) {
		t.Fatal("test setup: expected substring not found in raw bundle")
	}

	var rebuilt bytes.Buffer
	zw := gzip.NewWriter(&rebuilt)
	if _, err := zw.Write(tampered); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	_, _, _, err = Open(&rebuilt)
	if !errors.Is(err, ErrHashMismatch) {
		t.Errorf("Open after tamper: err = %v, want ErrHashMismatch", err)
	}
}

func TestPack_EmptyEvents(t *testing.T) {
	// A bundle with zero captured events still pairs manifest +
	// trailer; trailer.EventCount = 0 and the content hash covers
	// just the manifest line.
	in := PackInputs{
		RunID: "r", StageID: "s", Agent: "claude-code", Now: frozenNow(),
	}
	data, _, err := PackBytes(in, nil)
	if err != nil {
		t.Fatalf("PackBytes: %v", err)
	}
	manifest, events, trailer, err := Open(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if manifest.BundleSchema != SchemaV1 {
		t.Errorf("schema = %q", manifest.BundleSchema)
	}
	if len(events) != 0 {
		t.Errorf("got %d events, want 0", len(events))
	}
	if trailer.EventCount != 0 {
		t.Errorf("trailer EventCount = %d, want 0", trailer.EventCount)
	}
}

func TestPack_RequiredFields(t *testing.T) {
	cases := []struct {
		name string
		in   PackInputs
		want error
	}{
		{"empty run id", PackInputs{StageID: "s", Agent: "a"}, ErrEmptyRunID},
		{"empty stage id", PackInputs{RunID: "r", Agent: "a"}, ErrEmptyStageID},
		{"empty agent", PackInputs{RunID: "r", StageID: "s"}, ErrEmptyAgent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := PackBytes(tc.in, nil)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPack_DefaultsKindToRaw(t *testing.T) {
	in := PackInputs{
		RunID: "r", StageID: "s", Agent: "claude-code", Now: frozenNow(),
	}
	events := []agent.Event{{Kind: "", Payload: agent.MakePayload(map[string]string{"x": "y"})}}
	data, _, err := PackBytes(in, events)
	if err != nil {
		t.Fatal(err)
	}
	_, got, _, err := Open(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != "raw" {
		t.Errorf("expected one event with kind=raw, got %+v", got)
	}
}

func TestOpen_BadGzip(t *testing.T) {
	_, _, _, err := Open(strings.NewReader("not gzip"))
	if err == nil {
		t.Fatal("expected error on non-gzip input")
	}
	if !strings.Contains(err.Error(), "gzip") {
		t.Errorf("err = %v, want gzip error", err)
	}
}

func TestOpen_TooShort(t *testing.T) {
	// A bundle with only one line cannot have both manifest and
	// trailer; Open should reject.
	var raw bytes.Buffer
	if err := writeLine(&raw, Line{Seq: 1, Kind: "manifest", Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(raw.Bytes())
	_ = zw.Close()

	_, _, _, err := Open(&gz)
	if !errors.Is(err, ErrBadTrailer) {
		t.Errorf("err = %v, want ErrBadTrailer", err)
	}
}

func TestOpen_UnknownSchema(t *testing.T) {
	// Manually construct a bundle whose manifest has a future
	// schema version; Open must reject explicitly.
	manifestPayload, _ := json.Marshal(ManifestData{
		BundleSchema: "v2",
		RunID:        "r", StageID: "s", Agent: "x",
		GeneratedAt: time.Now().UTC(),
	})
	var raw bytes.Buffer
	_ = writeLine(&raw, Line{Seq: 1, Kind: "manifest", Data: manifestPayload})
	hash := sha256.Sum256(raw.Bytes())
	trailerPayload, _ := json.Marshal(TrailerData{EventCount: 0, ContentHash: hex.EncodeToString(hash[:])})
	_ = writeLine(&raw, Line{Seq: 2, Kind: "trailer", Data: trailerPayload})

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(raw.Bytes())
	_ = zw.Close()

	_, _, _, err := Open(&gz)
	if !errors.Is(err, ErrSchemaUnknown) {
		t.Errorf("err = %v, want ErrSchemaUnknown", err)
	}
}

func TestPack_HonorsGeneratedAt(t *testing.T) {
	// The Now closure runs many times during Pack; the manifest's
	// GeneratedAt must be the FIRST tick (it's emitted first).
	t0 := time.Date(2026, 5, 2, 9, 0, 0, 0, time.UTC)
	step := time.Second
	cur := t0
	now := func() time.Time {
		out := cur
		cur = cur.Add(step)
		return out
	}
	in := PackInputs{
		RunID: "r", StageID: "s", Agent: "claude-code", Now: now,
	}
	data, _, err := PackBytes(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, _, err := Open(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.GeneratedAt.Equal(t0) {
		t.Errorf("GeneratedAt = %v, want %v", manifest.GeneratedAt, t0)
	}
}

// failingWriter returns an error after a fixed byte budget so we
// can drive Pack's write-error branch deterministically.
type failingWriter struct {
	budget int
	wrote  int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.wrote >= w.budget {
		return 0, errors.New("disk full")
	}
	n := len(p)
	if w.wrote+n > w.budget {
		n = w.budget - w.wrote
	}
	w.wrote += n
	return n, errors.New("disk full")
}

func TestPack_WriteError(t *testing.T) {
	in := PackInputs{RunID: "r", StageID: "s", Agent: "x", Now: frozenNow()}
	_, err := Pack(&failingWriter{budget: 0}, in, nil)
	if err == nil {
		t.Fatal("expected write error")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("err = %v, missing underlying disk full", err)
	}
}

func TestOpen_BadJSONLine(t *testing.T) {
	// Build a gzipped stream where one line is not JSON. Open
	// must fail with a parse error; never silently skip.
	var raw bytes.Buffer
	manifestPayload, _ := json.Marshal(ManifestData{
		BundleSchema: SchemaV1,
		RunID:        "r", StageID: "s", Agent: "x",
		GeneratedAt: time.Now().UTC(),
	})
	_ = writeLine(&raw, Line{Seq: 1, Kind: "manifest", Data: manifestPayload})
	raw.WriteString("not json\n")
	hash := sha256.Sum256(raw.Bytes())
	trailerPayload, _ := json.Marshal(TrailerData{EventCount: 1, ContentHash: hex.EncodeToString(hash[:])})
	_ = writeLine(&raw, Line{Seq: 3, Kind: "trailer", Data: trailerPayload})

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(raw.Bytes())
	_ = zw.Close()

	_, _, _, err := Open(&gz)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "parse line") {
		t.Errorf("err = %v, want parse line error", err)
	}
}

func TestOpen_FirstLineNotManifest(t *testing.T) {
	var raw bytes.Buffer
	_ = writeLine(&raw, Line{Seq: 1, Kind: "result", Data: json.RawMessage(`{}`)})
	hash := sha256.Sum256(raw.Bytes())
	trailerPayload, _ := json.Marshal(TrailerData{EventCount: 0, ContentHash: hex.EncodeToString(hash[:])})
	_ = writeLine(&raw, Line{Seq: 2, Kind: "trailer", Data: trailerPayload})

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(raw.Bytes())
	_ = zw.Close()

	_, _, _, err := Open(&gz)
	if !errors.Is(err, ErrBadTrailer) {
		t.Errorf("err = %v, want ErrBadTrailer for missing manifest", err)
	}
}

func TestOpen_LastLineNotTrailer(t *testing.T) {
	var raw bytes.Buffer
	manifestPayload, _ := json.Marshal(ManifestData{
		BundleSchema: SchemaV1, RunID: "r", StageID: "s", Agent: "x",
		GeneratedAt: time.Now().UTC(),
	})
	_ = writeLine(&raw, Line{Seq: 1, Kind: "manifest", Data: manifestPayload})
	_ = writeLine(&raw, Line{Seq: 2, Kind: "tool_call", Data: json.RawMessage(`{"x":1}`)})

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(raw.Bytes())
	_ = zw.Close()

	_, _, _, err := Open(&gz)
	if !errors.Is(err, ErrBadTrailer) {
		t.Errorf("err = %v, want ErrBadTrailer for missing trailer", err)
	}
}

func TestOpen_TrailerEventCountMismatch(t *testing.T) {
	// Build a bundle where the trailer's event_count is wrong.
	// Open must reject — guards against silent truncation that
	// somehow preserves the content hash (e.g. hash-collision
	// future or buggy producer).
	var raw bytes.Buffer
	manifestPayload, _ := json.Marshal(ManifestData{
		BundleSchema: SchemaV1, RunID: "r", StageID: "s", Agent: "x",
		GeneratedAt: time.Now().UTC(),
	})
	_ = writeLine(&raw, Line{Seq: 1, Kind: "manifest", Data: manifestPayload})
	_ = writeLine(&raw, Line{Seq: 2, Kind: "tool_call", Data: json.RawMessage(`{"x":1}`)})
	hash := sha256.Sum256(raw.Bytes())
	// Lie about the count.
	trailerPayload, _ := json.Marshal(TrailerData{EventCount: 99, ContentHash: hex.EncodeToString(hash[:])})
	_ = writeLine(&raw, Line{Seq: 3, Kind: "trailer", Data: trailerPayload})

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(raw.Bytes())
	_ = zw.Close()

	_, _, _, err := Open(&gz)
	if !errors.Is(err, ErrBadTrailer) {
		t.Errorf("err = %v, want ErrBadTrailer on count mismatch", err)
	}
}

func TestSchemaConstant(t *testing.T) {
	// Pinning the constant prevents an accidental rename from
	// silently invalidating every bundle in production.
	if SchemaV1 != "v1" {
		t.Errorf("SchemaV1 = %q, want v1", SchemaV1)
	}
}

// readAll is a tiny io.ReadAll wrapper that fails the test on error.
func readAll(t *testing.T, r interface {
	Read(p []byte) (n int, err error)
}) ([]byte, error) {
	t.Helper()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(readerFunc(r.Read)); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), nil
}

type readerFunc func(p []byte) (n int, err error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// --- #5535 bounds ---

func boundsInputs(log io.Writer) PackInputs {
	return PackInputs{
		RunID:   "11111111-2222-3333-4444-555555555555",
		StageID: "22222222-3333-4444-5555-666666666666",
		Agent:   "claude-code",
		Now:     frozenNow(),
		Log:     log,
	}
}

func testLimits(maxLine int, maxTotal int64) packLimits {
	return packLimits{maxLine: maxLine, maxTotal: maxTotal, excerptBytes: 64, verifyOutputKeepBytes: 128}
}

// packAt packs with injected bounds and Opens the result with the same
// bounds, so every fixture also proves the bundle reads back.
func packAt(t *testing.T, in PackInputs, events []agent.Event, lim packLimits) (ManifestData, []Line, TrailerData, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if _, err := pack(&buf, in, events, lim); err != nil {
		t.Fatalf("pack: %v", err)
	}
	m, lines, tr, err := open(bytes.NewReader(buf.Bytes()), lim.maxLine, lim.maxTotal)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return m, lines, tr, buf.Bytes()
}

func lineLen(t *testing.T, ln Line) int {
	t.Helper()
	b, err := json.Marshal(ln)
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

func blob(kind string, n int, seed byte) agent.Event {
	return agent.Event{
		Kind:      kind,
		Timestamp: time.Date(2026, 5, 2, 9, 31, 0, 0, time.UTC),
		Payload:   agent.MakePayload(map[string]string{"text": strings.Repeat(string(rune('a'+seed%26)), n)}),
	}
}

func gzipLines(t *testing.T, lines ...[]byte) []byte {
	t.Helper()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	for _, l := range lines {
		_, _ = zw.Write(l)
		_, _ = zw.Write([]byte{'\n'})
	}
	_ = zw.Close()
	return gz.Bytes()
}

// TestOpen_Bounds: Open accepts a 5 MiB line at the production bound and
// refuses one line over the line bound and a stream over the total bound.
func TestOpen_Bounds(t *testing.T) {
	big := blob("assistant", 5<<20, 0)
	data, _, err := PackBytes(boundsInputs(io.Discard), []agent.Event{big})
	if err != nil {
		t.Fatalf("PackBytes: %v", err)
	}
	_, lines, _, err := Open(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Open of a 5 MiB line: %v", err)
	}
	if n := lineLen(t, lines[0]); n <= 5<<20 || lines[0].Kind != "assistant" {
		t.Fatalf("line kind/len = %s/%d, want assistant over 5 MiB", lines[0].Kind, n)
	}

	line := bytes.Repeat([]byte("x"), 101)
	if _, _, _, err := open(bytes.NewReader(gzipLines(t, line)), 100, 1<<20); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("open(line 101 > 100) err = %v, want ErrLineTooLong", err)
	}
	// Exactly at the line bound is not ErrLineTooLong (it fails later as
	// unparseable JSON, which is a different error).
	if _, _, _, err := open(bytes.NewReader(gzipLines(t, line[:100])), 100, 1<<20); errors.Is(err, ErrLineTooLong) {
		t.Fatalf("open(line == bound) err = %v, want not ErrLineTooLong", err)
	}
	stream := gzipLines(t, bytes.Repeat([]byte("y"), 50), bytes.Repeat([]byte("y"), 50))
	if _, _, _, err := open(bytes.NewReader(stream), 100, 101); !errors.Is(err, ErrDecompressedTooLarge) {
		t.Fatalf("open(102 bytes > 101) err = %v, want ErrDecompressedTooLarge", err)
	}
	if _, _, _, err := open(bytes.NewReader(stream), 100, 102); errors.Is(err, ErrDecompressedTooLarge) {
		t.Fatalf("open(102 bytes == bound) err = %v, want not ErrDecompressedTooLarge", err)
	}
}

// TestPack_PerLine_ElidesOrdinaryEvent: an over-bound ordinary event
// becomes an event_elided placeholder carrying the original seq/ts, the
// sha256 of the original line and a bounded excerpt, and the elision is
// logged with kind, original_bytes and sha256 (Y5).
func TestPack_PerLine_ElidesOrdinaryEvent(t *testing.T) {
	events := []agent.Event{blob("small", 10, 1), blob("assistant", 4000, 2), blob("small", 10, 3)}
	// The original line exactly as Pack measures it (json.Marshal, HTML
	// escaping included).
	orig, _ := json.Marshal(Line{Seq: 3, Timestamp: events[1].Timestamp, Kind: "assistant", Data: events[1].Payload})
	sum := sha256.Sum256(orig)
	wantSum := hex.EncodeToString(sum[:])

	var log bytes.Buffer
	_, lines, tr, _ := packAt(t, boundsInputs(&log), events, testLimits(1024, 1<<20))
	if tr.EventCount != 3 || len(lines) != 3 {
		t.Fatalf("event_count/lines = %d/%d, want 3/3", tr.EventCount, len(lines))
	}
	if lines[0].Kind != "small" || lines[2].Kind != "small" {
		t.Fatalf("neighbours changed: %s, %s", lines[0].Kind, lines[2].Kind)
	}
	ph := lines[1]
	if ph.Kind != KindEventElided {
		t.Fatalf("kind = %q, want %q", ph.Kind, KindEventElided)
	}
	if ph.Seq != 3 || !ph.Timestamp.Equal(events[1].Timestamp) {
		t.Fatalf("seq/ts = %d/%v, want 3/%v", ph.Seq, ph.Timestamp, events[1].Timestamp)
	}
	var d ElidedEventData
	if err := json.Unmarshal(ph.Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.ElidedKind != "assistant" || d.OriginalBytes != len(orig) || d.Bound != "line" || d.MaxLineBytes != 1024 {
		t.Fatalf("placeholder = %+v", d)
	}
	if d.SHA256 != wantSum {
		t.Fatalf("sha256 = %s, want %s", d.SHA256, wantSum)
	}
	if d.ExcerptHead != string(orig[:64]) || d.ExcerptTail != string(orig[len(orig)-64:]) {
		t.Fatalf("excerpt head/tail = %q / %q", d.ExcerptHead, d.ExcerptTail)
	}
	want := `"event":"bundle_event_elided"`
	for _, frag := range []string{want, `"kind":"assistant"`, fmt.Sprintf(`"original_bytes":%d`, len(orig)), `"sha256":"` + wantSum + `"`} {
		if !strings.Contains(log.String(), frag) {
			t.Fatalf("elision log missing %s: %s", frag, log.String())
		}
	}
}

// TestPack_PerLine_ExcerptIsFromRedactedInput: Pack's excerpt and hash come
// from the bytes it is given, so packing redacted events (the redacted
// variant) never carries the pre-redaction text.
func TestPack_PerLine_ExcerptIsFromRedactedInput(t *testing.T) {
	secret := "ghp_" + strings.Repeat("S", 36)
	raw := agent.Event{Kind: "assistant", Payload: agent.MakePayload(map[string]string{"t": secret + strings.Repeat("z", 3000) + secret})}
	redacted := agent.Event{Kind: "assistant", Payload: json.RawMessage(strings.ReplaceAll(string(raw.Payload), secret, "[REDACTED]"))}
	_, lines, _, gz := packAt(t, boundsInputs(io.Discard), []agent.Event{redacted}, testLimits(1024, 1<<20))
	if lines[0].Kind != KindEventElided {
		t.Fatalf("kind = %q", lines[0].Kind)
	}
	zr, _ := gzip.NewReader(bytes.NewReader(gz))
	all, _ := io.ReadAll(zr)
	if bytes.Contains(all, []byte("SSSSSSSS")) {
		t.Fatal("redacted bundle carries a fragment of the pre-redaction secret")
	}
	if !strings.Contains(string(lines[0].Data), "REDACTED") {
		t.Fatalf("excerpt does not carry the redacted text: %s", lines[0].Data)
	}
}

// TestPack_PerLine_TrimsVerifyRunOutputOnly (W8): an over-bound verify_run
// keeps head_sha, tree_sha, change_id, command, exit_code and outcome
// byte-identical; only output is trimmed and output_elided_bytes is set.
func TestPack_PerLine_TrimsVerifyRunOutputOnly(t *testing.T) {
	output := strings.Repeat("o", 5000)
	payload := agent.MakePayload(map[string]any{
		"command": "scripts/verify-change", "head_sha": strings.Repeat("a", 40), "tree_sha": strings.Repeat("b", 40),
		"change_id": strings.Repeat("c", 40), "exit_code": 1, "outcome": "failed", "output": output,
	})
	var log bytes.Buffer
	_, lines, _, _ := packAt(t, boundsInputs(&log), []agent.Event{{Kind: "verify_run", Payload: payload}}, testLimits(1024, 1<<20))
	if lines[0].Kind != "verify_run" {
		t.Fatalf("kind = %q, want verify_run (never elided wholesale)", lines[0].Kind)
	}
	if n := lineLen(t, lines[0]); n > 1024 {
		t.Fatalf("trimmed line = %d bytes > 1024", n)
	}
	var before, after map[string]json.RawMessage
	_ = json.Unmarshal(payload, &before)
	_ = json.Unmarshal(lines[0].Data, &after)
	for _, k := range []string{"head_sha", "tree_sha", "change_id", "command", "exit_code", "outcome"} {
		if string(after[k]) != string(before[k]) {
			t.Fatalf("%s = %s, want %s", k, after[k], before[k])
		}
	}
	var out string
	_ = json.Unmarshal(after["output"], &out)
	if !strings.HasPrefix(out, output[:128]) || !strings.HasSuffix(out, output[:128]) || !strings.Contains(out, "bytes of verify output elided") {
		t.Fatalf("trimmed output = %q", out)
	}
	if string(after["output_elided_bytes"]) != fmt.Sprint(5000-256) {
		t.Fatalf("output_elided_bytes = %s, want %d", after["output_elided_bytes"], 5000-256)
	}
	if !strings.Contains(log.String(), `"event":"bundle_verify_output_trimmed"`) {
		t.Fatalf("trim not logged: %s", log.String())
	}

	// Non-output fields alone over the bound: the output is trimmable, but
	// the line is still over the bound after trimming, so Pack fails closed.
	huge := agent.MakePayload(map[string]any{"head_sha": strings.Repeat("a", 40), "command": strings.Repeat("c", 2000), "output": strings.Repeat("x", 5000)})
	if _, err := pack(io.Discard, boundsInputs(io.Discard), []agent.Event{{Kind: "verify_run", Payload: huge}}, testLimits(1024, 1<<20)); !errors.Is(err, ErrLineTooLarge) {
		t.Fatalf("verify_run with over-bound non-output fields: err = %v, want ErrLineTooLarge", err)
	}
	// No string output to trim: fail closed.
	noOut := agent.MakePayload(map[string]any{"head_sha": strings.Repeat("a", 40), "command": strings.Repeat("c", 2000)})
	if _, err := pack(io.Discard, boundsInputs(io.Discard), []agent.Event{{Kind: "verify_run", Payload: noOut}}, testLimits(1024, 1<<20)); !errors.Is(err, ErrLineTooLarge) {
		t.Fatalf("verify_run without output: err = %v, want ErrLineTooLarge", err)
	}
}

// TestPack_PerLine_LoadBearingFailsClosed: an over-bound manifest,
// git_diff, gate_evidence or policy_event line is refused.
func TestPack_PerLine_LoadBearingFailsClosed(t *testing.T) {
	lim := testLimits(1024, 1<<20)
	for _, kind := range []string{"git_diff", "gate_evidence", "policy_event"} {
		_, err := pack(io.Discard, boundsInputs(io.Discard), []agent.Event{blob(kind, 4000, 0)}, lim)
		if !errors.Is(err, ErrLineTooLarge) {
			t.Errorf("%s: err = %v, want ErrLineTooLarge", kind, err)
		}
	}
	in := boundsInputs(io.Discard)
	in.AgentFailureReason = strings.Repeat("r", 4000)
	if _, err := pack(io.Discard, in, nil, lim); !errors.Is(err, ErrLineTooLarge) {
		t.Errorf("manifest: err = %v, want ErrLineTooLarge", err)
	}
}

// TestPack_Total_ElidesLargestFirst (W5): twenty individually admissible
// lines of distinct sizes, the largest placed LAST, over a 64 KiB total.
// Pack elides exactly the largest until the total fits and keeps the
// smaller ones verbatim.
func TestPack_Total_ElidesLargestFirst(t *testing.T) {
	const maxLine, maxTotal = 16 << 10, 64 << 10
	var events []agent.Event
	for i := 0; i < 20; i++ {
		events = append(events, blob("assistant", 1000+i*500, byte(i))) // ascending: largest last
	}
	var log bytes.Buffer
	_, lines, tr, gz := packAt(t, boundsInputs(&log), events, testLimits(maxLine, maxTotal))
	if tr.EventCount != 20 {
		t.Fatalf("event_count = %d", tr.EventCount)
	}
	zr, _ := gzip.NewReader(bytes.NewReader(gz))
	all, _ := io.ReadAll(zr)
	if len(all) > maxTotal {
		t.Fatalf("total = %d > %d", len(all), maxTotal)
	}
	firstElided := -1
	for i, ln := range lines {
		if ln.Kind == KindEventElided {
			if firstElided < 0 {
				firstElided = i
			}
			var d ElidedEventData
			_ = json.Unmarshal(ln.Data, &d)
			if d.Bound != "total" || d.MaxDecompressedBytes != maxTotal || d.SHA256 == "" {
				t.Fatalf("placeholder %d = %+v", i, d)
			}
			continue
		}
		if firstElided >= 0 {
			t.Fatalf("line %d kept after an elided smaller-index line %d: elision is not largest-first", i, firstElided)
		}
		if string(ln.Data) != string(events[i].Payload) {
			t.Fatalf("kept line %d not verbatim", i)
		}
	}
	if firstElided < 0 || firstElided == 0 {
		t.Fatalf("first elided = %d, want some but not all lines elided", firstElided)
	}
	// Minimality: restoring the smallest elided line would exceed the bound.
	restored := lineLen(t, Line{Seq: firstElided + 2, Timestamp: events[firstElided].Timestamp, Kind: "assistant", Data: events[firstElided].Payload})
	if int64(len(all)-lineLen(t, lines[firstElided])+restored) <= maxTotal {
		t.Fatalf("line %d was elided although the bundle fits with it", firstElided)
	}
	if got := strings.Count(log.String(), `"bound":"total"`); got != 20-firstElided {
		t.Fatalf("logged %d total-bound elisions, want %d", got, 20-firstElided)
	}
}

// TestPack_Total_TrimsVerifyRunAfterEliding: when eliding every ordinary
// event is not enough, verify_run outputs are trimmed.
func TestPack_Total_TrimsVerifyRunAfterEliding(t *testing.T) {
	const maxLine, maxTotal = 16 << 10, 16 << 10
	vr := func(n int) agent.Event {
		return agent.Event{Kind: "verify_run", Payload: agent.MakePayload(map[string]any{
			"head_sha": strings.Repeat("a", 40), "tree_sha": strings.Repeat("b", 40), "output": strings.Repeat("v", n)})}
	}
	events := []agent.Event{blob("assistant", 8000, 0), vr(12000), vr(11000)}
	_, lines, _, gz := packAt(t, boundsInputs(io.Discard), events, testLimits(maxLine, maxTotal))
	zr, _ := gzip.NewReader(bytes.NewReader(gz))
	all, _ := io.ReadAll(zr)
	if len(all) > maxTotal {
		t.Fatalf("total = %d > %d", len(all), maxTotal)
	}
	if lines[0].Kind != KindEventElided {
		t.Fatalf("ordinary event not elided first: %s", lines[0].Kind)
	}
	var f map[string]json.RawMessage
	_ = json.Unmarshal(lines[1].Data, &f)
	if lines[1].Kind != "verify_run" || f["output_elided_bytes"] == nil || string(f["head_sha"]) != `"`+strings.Repeat("a", 40)+`"` {
		t.Fatalf("largest verify_run not trimmed with identity kept (kind %s, %d bytes)", lines[1].Kind, len(lines[1].Data))
	}
	var g map[string]json.RawMessage
	_ = json.Unmarshal(lines[2].Data, &g)
	if g["output_elided_bytes"] != nil {
		t.Fatal("smaller verify_run trimmed although the bundle already fit")
	}
}

// TestPack_PerLine_VerifyRunUntrimmable: an over-bound verify_run whose
// payload is not a JSON object, or whose output already fits the excerpt,
// cannot be trimmed, so Pack fails closed.
func TestPack_PerLine_VerifyRunUntrimmable(t *testing.T) {
	lim := testLimits(1024, 1<<20)
	notObject := json.RawMessage(`"` + strings.Repeat("s", 2000) + `"`)
	if _, err := pack(io.Discard, boundsInputs(io.Discard), []agent.Event{{Kind: "verify_run", Payload: notObject}}, lim); !errors.Is(err, ErrLineTooLarge) {
		t.Fatalf("verify_run with a non-object payload: err = %v, want ErrLineTooLarge", err)
	}
	// 200 bytes of output fit the 2*128-byte excerpt: nothing to elide.
	shortOut := agent.MakePayload(map[string]any{"command": strings.Repeat("c", 2000), "output": strings.Repeat("o", 200)})
	if _, err := pack(io.Discard, boundsInputs(io.Discard), []agent.Event{{Kind: "verify_run", Payload: shortOut}}, lim); !errors.Is(err, ErrLineTooLarge) {
		t.Fatalf("verify_run with output within the excerpt: err = %v, want ErrLineTooLarge", err)
	}
}

// TestPack_Total_SkipsUnshrinkableLines: in the total pass, equal-size
// ordinary events are elided higher seq first; an event smaller than its
// placeholder is kept verbatim; verify_runs with no output, or output within
// the excerpt, are skipped; and a smaller trimmable verify_run still brings
// the bundle under the bound.
func TestPack_Total_SkipsUnshrinkableLines(t *testing.T) {
	const maxLine, maxTotal = 64 << 10, 60000
	vr := func(fields map[string]any) agent.Event {
		fields["head_sha"] = strings.Repeat("a", 40)
		return agent.Event{Kind: "verify_run", Payload: agent.MakePayload(fields)}
	}
	events := []agent.Event{
		blob("small", 10, 1),                                      // seq 2: smaller than its placeholder
		blob("assistant", 3000, 2),                                // seq 3
		blob("assistant", 3000, 3),                                // seq 4: same size, elided first
		vr(map[string]any{"command": strings.Repeat("c", 30000)}), // seq 5: no output
		vr(map[string]any{"command": strings.Repeat("c", 25000), "output": strings.Repeat("o", 200)}), // seq 6: output within the excerpt
		vr(map[string]any{"output": strings.Repeat("v", 20000)}),                                      // seq 7: trimmable
	}
	var log bytes.Buffer
	_, lines, _, gz := packAt(t, boundsInputs(&log), events, testLimits(maxLine, maxTotal))
	zr, _ := gzip.NewReader(bytes.NewReader(gz))
	all, _ := io.ReadAll(zr)
	if len(all) > maxTotal {
		t.Fatalf("total = %d > %d", len(all), maxTotal)
	}
	if lines[0].Kind != "small" || string(lines[0].Data) != string(events[0].Payload) {
		t.Fatalf("event smaller than its placeholder not kept verbatim: %s %s", lines[0].Kind, lines[0].Data)
	}
	if lines[1].Kind != KindEventElided || lines[2].Kind != KindEventElided {
		t.Fatalf("equal-size events not elided: %s, %s", lines[1].Kind, lines[2].Kind)
	}
	if i3, i4 := strings.Index(log.String(), `"seq":3`), strings.Index(log.String(), `"seq":4`); i4 < 0 || i3 < 0 || i4 > i3 {
		t.Fatalf("equal-size tie not broken higher seq first: %s", log.String())
	}
	for i := 3; i <= 4; i++ {
		if lines[i].Kind != "verify_run" || string(lines[i].Data) != string(events[i].Payload) {
			t.Fatalf("untrimmable verify_run %d changed", i)
		}
	}
	var f map[string]json.RawMessage
	_ = json.Unmarshal(lines[5].Data, &f)
	if lines[5].Kind != "verify_run" || f["output_elided_bytes"] == nil {
		t.Fatalf("trimmable verify_run not trimmed: %s", lines[5].Data)
	}
}

// TestExcerpt cuts on UTF-8 rune boundaries and returns short input whole.
func TestExcerpt(t *testing.T) {
	if h, tl := excerpt([]byte("short"), 4); string(h) != "short" || tl != nil {
		t.Fatalf("short excerpt = %q / %q, want whole head and no tail", h, tl)
	}
	// Byte 3 and byte len-3 fall inside a two-byte é: the head backs off to
	// byte 2 and the tail moves forward past the continuation byte.
	b := []byte("abé" + strings.Repeat("x", 10) + "éde")
	h, tl := excerpt(b, 3)
	if string(h) != "ab" || string(tl) != "de" {
		t.Fatalf("excerpt = %q / %q, want %q / %q", h, tl, "ab", "de")
	}
}

// TestPack_Total_FailsClosedOnLoadBearing: load-bearing lines alone over the
// total bound fail with ErrDecompressedTooLarge.
func TestPack_Total_FailsClosedOnLoadBearing(t *testing.T) {
	events := []agent.Event{blob("policy_event", 10000, 0), blob("git_diff", 10000, 1), blob("assistant", 5000, 2)}
	if _, err := pack(io.Discard, boundsInputs(io.Discard), events, testLimits(16<<10, 16<<10)); !errors.Is(err, ErrDecompressedTooLarge) {
		t.Fatalf("err = %v, want ErrDecompressedTooLarge", err)
	}
}

// goldenManifestInputs is the fixed PackInputs the shared golden
// testdata/wire/bundle_manifest_verify_pin.json is written from.
func goldenManifestInputs() PackInputs {
	return PackInputs{
		RunID:            "11111111-2222-3333-4444-555555555555",
		StageID:          "22222222-3333-4444-5555-666666666666",
		Agent:            "claude-code",
		Model:            "claude-opus-4-7",
		RunnerKind:       "local",
		PushAndOpenPR:    true,
		VerifyPinHeadSHA: "0123456789abcdef0123456789abcdef01234567",
		Now:              func() time.Time { return time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC) },
		Log:              io.Discard,
	}
}

// TestPack_ManifestVerifyPinGolden pins the verify_pin_head_sha wire tag
// against the shared golden the backend bundle test decodes (#5535). With
// the field empty the manifest is byte-identical to the pre-field shape.
func TestPack_ManifestVerifyPinGolden(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "wire", "bundle_manifest_verify_pin.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	data, _, err := PackBytes(goldenManifestInputs(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m, _, _, err := Open(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(m)
	var gotV, wantV any
	_ = json.Unmarshal(got, &gotV)
	if err := json.Unmarshal(golden, &wantV); err != nil {
		t.Fatalf("golden: %v", err)
	}
	if !reflect.DeepEqual(gotV, wantV) {
		t.Fatalf("manifest data = %s\nwant golden %s", got, golden)
	}

	// Empty claim: the manifest line equals the one Pack wrote before the
	// field existed (the same struct without the tag).
	in := goldenManifestInputs()
	in.VerifyPinHeadSHA = ""
	data, _, _ = PackBytes(in, nil)
	zr, _ := gzip.NewReader(bytes.NewReader(data))
	all, _ := io.ReadAll(zr)
	first := all[:bytes.IndexByte(all, '\n')]
	if bytes.Contains(first, []byte("verify_pin_head_sha")) {
		t.Fatalf("empty claim serialized: %s", first)
	}
	want := `{"seq":1,"ts":"2026-11-27T12:00:00Z","kind":"manifest","data":{"bundle_schema":"v1","run_id":"11111111-2222-3333-4444-555555555555","stage_id":"22222222-3333-4444-5555-666666666666","agent":"claude-code","model":"claude-opus-4-7","generated_at":"2026-11-27T12:00:00Z","runner_kind":"local","push_and_open_pr":true}}`
	if string(first) != want {
		t.Fatalf("manifest line = %s\nwant %s", first, want)
	}
}
