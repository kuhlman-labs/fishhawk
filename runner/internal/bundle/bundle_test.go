package bundle

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
