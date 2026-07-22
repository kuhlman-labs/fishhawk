package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
)

/*
 * Tests for handleGetStageTrace (#218). Uses focused fakes —
 * traceStoreFake / auditFake from trace_test.go are stub-only on
 * the read path, so we'd have to extend them to return canned data;
 * cleaner to keep the read-side fakes scoped to this file.
 */

// traceReadStageRepo is a minimal run.Repository fake — just enough
// to resolve stage_id → run_id for the trace read handler.
type traceReadStageRepo struct {
	mu       sync.Mutex
	stages   map[uuid.UUID]*run.Stage
	getErr   error
	notFound bool
}

func newTraceReadStageRepo() *traceReadStageRepo {
	return &traceReadStageRepo{stages: map[uuid.UUID]*run.Stage{}}
}

func (r *traceReadStageRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return nil, r.getErr
	}
	if r.notFound {
		return nil, run.ErrNotFound
	}
	if st, ok := r.stages[id]; ok {
		return st, nil
	}
	return nil, run.ErrNotFound
}

// Stub the rest of run.Repository — the trace read handler only
// needs GetStage. Each unused method panics on call to make
// accidental wiring loud.
func (r *traceReadStageRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *traceReadStageRepo) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *traceReadStageRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}
func (r *traceReadStageRepo) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *traceReadStageRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *traceReadStageRepo) RetryRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *traceReadStageRepo) SetRunPullRequestURL(context.Context, uuid.UUID, string) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *traceReadStageRepo) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *traceReadStageRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *traceReadStageRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *traceReadStageRepo) ListReviewStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (r *traceReadStageRepo) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *traceReadStageRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}
func (r *traceReadStageRepo) TransitionStage(context.Context, uuid.UUID, run.StageState, *run.StageCompletion) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *traceReadStageRepo) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	return nil, errors.New("not used")
}

// traceReadAuditRepo returns canned trace_uploaded entries for the
// stage filter to walk. Other audit methods stay panic-stubs.
type traceReadAuditRepo struct {
	mu      sync.Mutex
	entries []*audit.Entry
	listErr error
}

func newTraceReadAuditRepo() *traceReadAuditRepo { return &traceReadAuditRepo{} }

func (a *traceReadAuditRepo) ListForRunByCategory(_ context.Context, _ uuid.UUID, _ string) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listErr != nil {
		return nil, a.listErr
	}
	// Caller's contract: sequence-ascending. Tests seed in order.
	return a.entries, nil
}

func (a *traceReadAuditRepo) Append(context.Context, audit.AppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}

func (a *traceReadAuditRepo) ChainsByParent(_ context.Context, _ uuid.UUID, _ bool) ([]*audit.Entry, error) {
	return nil, nil
}
func (a *traceReadAuditRepo) AppendChained(context.Context, audit.ChainAppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *traceReadAuditRepo) AppendGlobalChained(context.Context, audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *traceReadAuditRepo) ListGlobal(context.Context) ([]*audit.Entry, error) {
	return nil, errors.New("not used")
}

func (a *traceReadAuditRepo) ListGlobalByAccount(context.Context, *uuid.UUID) ([]*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *traceReadAuditRepo) ListAll(context.Context, audit.ListAllParams) ([]*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *traceReadAuditRepo) Get(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *traceReadAuditRepo) ListForRun(context.Context, uuid.UUID) ([]*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *traceReadAuditRepo) LastForRun(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, errors.New("not used")
}

// traceReadStore returns canned bytes for the (run, variant, hash)
// the handler asks for. Tracks the requested ref so tests can
// assert the right variant was selected.
type traceReadStore struct {
	mu  sync.Mutex
	got tracestore.BundleRef
	// bytesByHash maps content_hash → bundle bytes. Returns
	// ErrNotFound when the requested hash is absent.
	bytesByHash map[string][]byte
	getErr      error
}

func newTraceReadStore() *traceReadStore { return &traceReadStore{bytesByHash: map[string][]byte{}} }

func (s *traceReadStore) Get(_ context.Context, ref tracestore.BundleRef) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = ref
	if s.getErr != nil {
		return nil, s.getErr
	}
	if b, ok := s.bytesByHash[ref.ContentHash]; ok {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	return nil, tracestore.ErrNotFound
}

func (s *traceReadStore) Put(context.Context, tracestore.BundleRef, io.Reader) error {
	return errors.New("not used")
}
func (s *traceReadStore) Stat(context.Context, tracestore.BundleRef) (tracestore.Stat, error) {
	return tracestore.Stat{}, errors.New("not used")
}
func (s *traceReadStore) List(context.Context, uuid.UUID) ([]tracestore.BundleRef, error) {
	return nil, errors.New("not used")
}

// hash64 builds a fake but valid-shaped sha256 hex string. Tests
// don't care that it actually matches the body's sha256 — they care
// that the right hash is selected from the audit log.
func hash64(seed string) string {
	out := []byte(seed)
	for len(out) < 64 {
		out = append(out, 'a')
	}
	return string(out[:64])
}

func makeTraceUploadedEntry(t *testing.T, seq int64, runID, stageID uuid.UUID, variant, hash string) *audit.Entry {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"run_id":       runID.String(),
		"stage_id":     stageID.String(),
		"variant":      variant,
		"content_hash": hash,
	})
	if err != nil {
		t.Fatal(err)
	}
	rid := runID
	sid := stageID
	return &audit.Entry{
		ID:        uuid.New(),
		Sequence:  seq,
		RunID:     &rid,
		StageID:   &sid,
		Timestamp: time.Date(2026, 5, 7, 12, 0, int(seq), 0, time.UTC),
		Category:  "trace_uploaded",
		Payload:   payload,
		EntryHash: fmt.Sprintf("hash-%d", seq),
	}
}

func newTraceReadServer(t *testing.T) (*Server, *traceReadStageRepo, *traceReadAuditRepo, *traceReadStore) {
	t.Helper()
	rr := newTraceReadStageRepo()
	ar := newTraceReadAuditRepo()
	ts := newTraceReadStore()
	s := New(Config{
		Addr:       "127.0.0.1:0",
		RunRepo:    rr,
		AuditRepo:  ar,
		TraceStore: ts,
	})
	return s, rr, ar, ts
}

func TestGetStageTrace_HappyPath_StreamsRedactedBytes(t *testing.T) {
	s, rr, ar, ts := newTraceReadServer(t)
	runID, stageID := uuid.New(), uuid.New()
	rr.stages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	hashRaw := hash64("raw")
	hashRedacted := hash64("redacted")
	ar.entries = []*audit.Entry{
		makeTraceUploadedEntry(t, 1, runID, stageID, "raw", hashRaw),
		makeTraceUploadedEntry(t, 2, runID, stageID, "redacted", hashRedacted),
	}

	bundleBytes := []byte(`{"seq":1,"kind":"manifest","data":{}}` + "\n")
	ts.bytesByHash[hashRedacted] = bundleBytes

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/stages/%s/trace", stageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", got)
	}
	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
	if got := w.Header().Get("X-Fishhawk-Content-Hash"); got != hashRedacted {
		t.Errorf("X-Fishhawk-Content-Hash = %q, want %q", got, hashRedacted)
	}
	if !bytes.Equal(w.Body.Bytes(), bundleBytes) {
		t.Errorf("body bytes mismatch:\ngot %q\nwant %q", w.Body.Bytes(), bundleBytes)
	}
	if ts.got.Variant != tracestore.VariantRedacted {
		t.Errorf("requested variant = %q, want redacted", ts.got.Variant)
	}
	if ts.got.ContentHash != hashRedacted {
		t.Errorf("requested hash = %q, want redacted hash", ts.got.ContentHash)
	}
}

func TestGetStageTrace_PicksMostRecentRedacted(t *testing.T) {
	// Two redacted bundles for the same stage (e.g. retried stage)
	// — handler must pick the higher-sequence entry.
	s, rr, ar, ts := newTraceReadServer(t)
	runID, stageID := uuid.New(), uuid.New()
	rr.stages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	hashOlder := hash64("older")
	hashNewer := hash64("newer")
	ar.entries = []*audit.Entry{
		makeTraceUploadedEntry(t, 1, runID, stageID, "redacted", hashOlder),
		makeTraceUploadedEntry(t, 2, runID, stageID, "redacted", hashNewer),
	}
	ts.bytesByHash[hashOlder] = []byte("older")
	ts.bytesByHash[hashNewer] = []byte("newer")

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/stages/%s/trace", stageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !bytes.Equal(w.Body.Bytes(), []byte("newer")) {
		t.Errorf("served older bundle when newer was available; got %q", w.Body.String())
	}
}

func TestGetStageTrace_FiltersOutSiblingStages(t *testing.T) {
	// Audit log carries trace_uploaded entries for two stages on
	// the same run; handler must scope to the requested stage.
	s, rr, ar, ts := newTraceReadServer(t)
	runID, stageID := uuid.New(), uuid.New()
	otherStage := uuid.New()
	rr.stages[stageID] = &run.Stage{ID: stageID, RunID: runID}

	hashOurs := hash64("ours")
	hashSibling := hash64("sibling")
	ar.entries = []*audit.Entry{
		makeTraceUploadedEntry(t, 1, runID, otherStage, "redacted", hashSibling),
		makeTraceUploadedEntry(t, 2, runID, stageID, "redacted", hashOurs),
	}
	ts.bytesByHash[hashOurs] = []byte("ours")
	ts.bytesByHash[hashSibling] = []byte("sibling")

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/stages/%s/trace", stageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !bytes.Equal(w.Body.Bytes(), []byte("ours")) {
		t.Errorf("served sibling stage's bundle: %q", w.Body.String())
	}
}

func TestGetStageTrace_404_WhenOnlyRawExists(t *testing.T) {
	// Raw isn't surfaced via this endpoint; if no redacted has been
	// uploaded, the response is 404 trace_not_found.
	s, rr, ar, _ := newTraceReadServer(t)
	runID, stageID := uuid.New(), uuid.New()
	rr.stages[stageID] = &run.Stage{ID: stageID, RunID: runID}

	ar.entries = []*audit.Entry{
		makeTraceUploadedEntry(t, 1, runID, stageID, "raw", hash64("raw")),
	}

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/stages/%s/trace", stageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (raw-only stage)", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("trace_not_found")) {
		t.Errorf("body should reference trace_not_found code:\n%s", w.Body.String())
	}
}

func TestGetStageTrace_404_StageMissing(t *testing.T) {
	s, rr, _, _ := newTraceReadServer(t)
	rr.notFound = true
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/stages/%s/trace", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGetStageTrace_410_WhenAuditPointsAtMissingStorage(t *testing.T) {
	// Audit row says we have a redacted bundle but the storage
	// returns ErrNotFound — surface 410 so callers don't hammer.
	s, rr, ar, _ := newTraceReadServer(t)
	runID, stageID := uuid.New(), uuid.New()
	rr.stages[stageID] = &run.Stage{ID: stageID, RunID: runID}
	ar.entries = []*audit.Entry{
		makeTraceUploadedEntry(t, 1, runID, stageID, "redacted", hash64("ghost")),
	}
	// ts.bytesByHash intentionally empty — the store returns ErrNotFound.

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/stages/%s/trace", stageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusGone {
		t.Errorf("status = %d, want 410", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("trace_storage_missing")) {
		t.Errorf("body should reference trace_storage_missing:\n%s", w.Body.String())
	}
}

func TestGetStageTrace_400_BadStageUUID(t *testing.T) {
	s, _, _, _ := newTraceReadServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v0/stages/not-a-uuid/trace", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGetStageTrace_503_Unconfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/stages/%s/trace", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestGetStageTrace_500_AuditError(t *testing.T) {
	s, rr, ar, _ := newTraceReadServer(t)
	runID, stageID := uuid.New(), uuid.New()
	rr.stages[stageID] = &run.Stage{ID: stageID, RunID: runID}
	ar.listErr = errors.New("db down")
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/stages/%s/trace", stageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// TestGetStageTrace_StreamsValidGzippedJSONL is a sanity check: the
// happy-path body should pipe through gzip + JSONL parsing without
// the handler touching content. Acts as a regression guard against
// accidental decompression / re-encoding in the handler.
func TestGetStageTrace_StreamsValidGzippedJSONL(t *testing.T) {
	s, rr, ar, ts := newTraceReadServer(t)
	runID, stageID := uuid.New(), uuid.New()
	rr.stages[stageID] = &run.Stage{ID: stageID, RunID: runID}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte(`{"seq":1,"kind":"manifest","data":{"run_id":"abc"}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.Write([]byte(`{"seq":2,"kind":"raw","data":{"text":"hello"}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	hashOK := hash64("ok")
	ar.entries = []*audit.Entry{makeTraceUploadedEntry(t, 1, runID, stageID, "redacted", hashOK)}
	ts.bytesByHash[hashOK] = buf.Bytes()

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/stages/%s/trace", stageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	gr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("body is not valid gzip: %v", err)
	}
	defer gr.Close()
	out, err := io.ReadAll(gr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"manifest"`)) || !bytes.Contains(out, []byte(`"hello"`)) {
		t.Errorf("decompressed body missing expected events:\n%s", out)
	}
}
