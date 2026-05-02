package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
)

// signingFake is a richer fake than newFakeSigningRepo so the trace
// tests can drive Verify with controlled (key, message, signature)
// triples. We hold the raw bytes of the issued private key so a
// test can sign messages and feed them through the handler.
type signingFake struct {
	mu   sync.Mutex
	keys map[uuid.UUID]ed25519.PrivateKey

	// verifyErr forces Verify to return a chosen error regardless
	// of the supplied signature, useful for the expired / not-found
	// branches.
	verifyErr error
}

func newSigningFake() *signingFake {
	return &signingFake{keys: map[uuid.UUID]ed25519.PrivateKey{}}
}

func (f *signingFake) issue(t *testing.T, runID uuid.UUID) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.keys[runID] = priv
	f.mu.Unlock()
	return priv, pub
}

func (f *signingFake) Issue(_ context.Context, _ uuid.UUID, _ time.Duration) (*signing.IssuedKey, error) {
	return nil, errors.New("signingFake: Issue not used in trace tests")
}

func (f *signingFake) Get(_ context.Context, _ uuid.UUID) (*signing.Key, error) {
	return nil, errors.New("signingFake: Get not used in trace tests")
}

func (f *signingFake) Verify(_ context.Context, runID uuid.UUID, message, signature []byte) error {
	if f.verifyErr != nil {
		return f.verifyErr
	}
	f.mu.Lock()
	priv, ok := f.keys[runID]
	f.mu.Unlock()
	if !ok {
		return signing.ErrNotFound
	}
	if !ed25519.Verify(priv.Public().(ed25519.PublicKey), message, signature) {
		return signing.ErrSignatureInvalid
	}
	return nil
}

// traceStoreFake records the last Put so tests can assert what was
// stored without standing up MinIO. tracestore.Storage has more
// methods than we need here; the unused ones return errors so an
// accidental call is loud.
type traceStoreFake struct {
	mu     sync.Mutex
	last   *tracestore.BundleRef
	body   []byte
	putErr error
}

func newTraceStoreFake() *traceStoreFake { return &traceStoreFake{} }

func (s *traceStoreFake) Put(_ context.Context, ref tracestore.BundleRef, body io.Reader) error {
	if s.putErr != nil {
		return s.putErr
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rc := ref
	s.last = &rc
	s.body = b
	return nil
}

func (s *traceStoreFake) Get(_ context.Context, _ tracestore.BundleRef) (io.ReadCloser, error) {
	return nil, errors.New("traceStoreFake: Get not used")
}
func (s *traceStoreFake) Stat(_ context.Context, _ tracestore.BundleRef) (tracestore.Stat, error) {
	return tracestore.Stat{}, errors.New("traceStoreFake: Stat not used")
}
func (s *traceStoreFake) List(_ context.Context, _ uuid.UUID) ([]tracestore.BundleRef, error) {
	return nil, errors.New("traceStoreFake: List not used")
}

// auditFake captures appended entries so tests can assert what got
// logged. AppendChained is the only method exercised by the trace
// handler.
type auditFake struct {
	mu        sync.Mutex
	appended  []audit.ChainAppendParams
	appendErr error
}

func newAuditFake() *auditFake { return &auditFake{} }

func (a *auditFake) Append(_ context.Context, _ audit.AppendParams) (*audit.Entry, error) {
	return nil, errors.New("auditFake: Append not used")
}
func (a *auditFake) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if a.appendErr != nil {
		return nil, a.appendErr
	}
	a.mu.Lock()
	a.appended = append(a.appended, p)
	a.mu.Unlock()
	return &audit.Entry{ID: uuid.New(), RunID: p.RunID}, nil
}
func (a *auditFake) Get(_ context.Context, _ uuid.UUID) (*audit.Entry, error) {
	return nil, errors.New("auditFake: Get not used")
}
func (a *auditFake) ListForRun(_ context.Context, _ uuid.UUID) ([]*audit.Entry, error) {
	return nil, errors.New("auditFake: ListForRun not used")
}
func (a *auditFake) LastForRun(_ context.Context, _ uuid.UUID) (*audit.Entry, error) {
	return nil, errors.New("auditFake: LastForRun not used")
}
func (a *auditFake) ListForRunByCategory(_ context.Context, _ uuid.UUID, _ string) ([]*audit.Entry, error) {
	return nil, errors.New("auditFake: ListForRunByCategory not used")
}

// newTraceServer wires all three repos for the trace handler.
func newTraceServer(t *testing.T) (*Server, *signingFake, *traceStoreFake, *auditFake) {
	t.Helper()
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()
	s := New(Config{
		Addr:        "127.0.0.1:0",
		SigningRepo: sf,
		TraceStore:  ts,
		AuditRepo:   au,
	})
	return s, sf, ts, au
}

// shipRequest builds a POST /v0/runs/{id}/trace request signed by
// `priv`. Returns the recorded response.
func shipRequest(t *testing.T, s *Server, runID, stageID uuid.UUID, variant string, priv ed25519.PrivateKey, body []byte, sigOverride string) *httptest.ResponseRecorder {
	t.Helper()
	url := fmt.Sprintf("/v0/runs/%s/trace?stage_id=%s&variant=%s", runID, stageID, variant)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if sigOverride != "" {
		req.Header.Set("X-Fishhawk-Signature", sigOverride)
	} else if priv != nil {
		sig := ed25519.Sign(priv, signing.ComputeMessage(body))
		req.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(sig))
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestShipTrace_HappyPath(t *testing.T) {
	s, sf, ts, au := newTraceServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)
	bundle := []byte("fake-gzipped-bundle-bytes")

	w := shipRequest(t, s, runID, stageID, "raw", priv, bundle, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	var resp traceUploadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RunID != runID || resp.StageID != stageID || resp.Variant != "raw" {
		t.Errorf("response mismatch: %+v", resp)
	}
	if len(resp.ContentHash) != 64 {
		t.Errorf("ContentHash len = %d, want 64", len(resp.ContentHash))
	}

	// Tracestore: stored at the ref with matching content_hash.
	if ts.last == nil {
		t.Fatal("tracestore.Put was not called")
	}
	if ts.last.RunID != runID || ts.last.Variant != tracestore.VariantRaw || ts.last.ContentHash != resp.ContentHash {
		t.Errorf("ref mismatch: got %+v", ts.last)
	}
	if !bytes.Equal(ts.body, bundle) {
		t.Errorf("body bytes not stored verbatim")
	}

	// Audit: one trace_uploaded entry tied to the run.
	au.mu.Lock()
	defer au.mu.Unlock()
	if len(au.appended) != 1 {
		t.Fatalf("audit appended %d, want 1", len(au.appended))
	}
	ent := au.appended[0]
	if ent.RunID != runID {
		t.Errorf("audit RunID = %s", ent.RunID)
	}
	if ent.Category != "trace_uploaded" {
		t.Errorf("audit Category = %q", ent.Category)
	}
	if ent.StageID == nil || *ent.StageID != stageID {
		t.Errorf("audit StageID = %v", ent.StageID)
	}
	// Payload should mention the content_hash so the audit log can be
	// cross-referenced to the stored bundle.
	if !bytes.Contains(ent.Payload, []byte(resp.ContentHash)) {
		t.Errorf("audit payload missing content_hash: %s", ent.Payload)
	}
}

func TestShipTrace_BadUUID(t *testing.T) {
	s, _, _, _ := newTraceServer(t)
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/not-a-uuid/trace?stage_id="+uuid.New().String()+"&variant=raw",
		strings.NewReader(""))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestShipTrace_MissingStageID(t *testing.T) {
	s, _, _, _ := newTraceServer(t)
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/runs/%s/trace?variant=raw", uuid.New()),
		strings.NewReader(""))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for missing stage_id", w.Code)
	}
}

func TestShipTrace_BadVariant(t *testing.T) {
	s, _, _, _ := newTraceServer(t)
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/runs/%s/trace?stage_id=%s&variant=other", uuid.New(), uuid.New()),
		strings.NewReader(""))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for bad variant", w.Code)
	}
}

func TestShipTrace_MissingSignature(t *testing.T) {
	s, sf, _, _ := newTraceServer(t)
	runID := uuid.New()
	sf.issue(t, runID)
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/runs/%s/trace?stage_id=%s&variant=raw", runID, uuid.New()),
		strings.NewReader("body"))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"signature_missing"`) {
		t.Errorf("body missing signature_missing: %s", w.Body.String())
	}
}

func TestShipTrace_BadHexSignature(t *testing.T) {
	s, sf, _, _ := newTraceServer(t)
	runID := uuid.New()
	priv, _ := sf.issue(t, runID)
	w := shipRequest(t, s, runID, uuid.New(), "raw", priv, []byte("body"), "not-hex")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"signature_invalid"`) {
		t.Errorf("body missing signature_invalid: %s", w.Body.String())
	}
}

func TestShipTrace_WrongSignature(t *testing.T) {
	s, sf, _, _ := newTraceServer(t)
	runID := uuid.New()
	sf.issue(t, runID)

	// Sign with a DIFFERENT key (a totally separate keypair).
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	w := shipRequest(t, s, runID, uuid.New(), "raw", otherPriv, []byte("body"), "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestShipTrace_NoSigningKeyForRun(t *testing.T) {
	s, _, _, _ := newTraceServer(t)
	// No issue() called → key not in fake's map → ErrNotFound.
	body := []byte("body")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	w := shipRequest(t, s, uuid.New(), uuid.New(), "raw", priv, body, "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"signing_key_not_found"`) {
		t.Errorf("body missing signing_key_not_found: %s", w.Body.String())
	}
}

func TestShipTrace_ExpiredKey(t *testing.T) {
	s, sf, _, _ := newTraceServer(t)
	sf.verifyErr = signing.ErrExpired
	runID := uuid.New()
	body := []byte("b")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	w := shipRequest(t, s, runID, uuid.New(), "raw", priv, body, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for expired key", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"signing_key_expired"`) {
		t.Errorf("body missing signing_key_expired: %s", w.Body.String())
	}
}

func TestShipTrace_BodyTooLarge(t *testing.T) {
	s, sf, _, _ := newTraceServer(t)
	runID := uuid.New()
	priv, _ := sf.issue(t, runID)
	big := bytes.Repeat([]byte{0}, maxTraceBundleBytes+1)
	w := shipRequest(t, s, runID, uuid.New(), "raw", priv, big, "")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

func TestShipTrace_TraceStoreError(t *testing.T) {
	s, sf, ts, _ := newTraceServer(t)
	ts.putErr = errors.New("s3 down")
	runID := uuid.New()
	priv, _ := sf.issue(t, runID)
	w := shipRequest(t, s, runID, uuid.New(), "raw", priv, []byte("b"), "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestShipTrace_AuditAppendError(t *testing.T) {
	// The bundle has been stored already; failing here surfaces 500
	// so the runner retries.
	s, sf, _, au := newTraceServer(t)
	au.appendErr = errors.New("db down")
	runID := uuid.New()
	priv, _ := sf.issue(t, runID)
	w := shipRequest(t, s, runID, uuid.New(), "raw", priv, []byte("b"), "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestShipTrace_NilDepsConfigured(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing signing", Config{Addr: "127.0.0.1:0", TraceStore: newTraceStoreFake(), AuditRepo: newAuditFake()}},
		{"missing tracestore", Config{Addr: "127.0.0.1:0", SigningRepo: newSigningFake(), AuditRepo: newAuditFake()}},
		{"missing audit", Config{Addr: "127.0.0.1:0", SigningRepo: newSigningFake(), TraceStore: newTraceStoreFake()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(tc.cfg)
			req := httptest.NewRequest(http.MethodPost,
				fmt.Sprintf("/v0/runs/%s/trace?stage_id=%s&variant=raw", uuid.New(), uuid.New()),
				strings.NewReader(""))
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", w.Code)
			}
		})
	}
}

func TestShipTrace_RedactedVariant(t *testing.T) {
	s, sf, ts, _ := newTraceServer(t)
	runID := uuid.New()
	priv, _ := sf.issue(t, runID)
	w := shipRequest(t, s, runID, uuid.New(), "redacted", priv, []byte("b"), "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if ts.last == nil || ts.last.Variant != tracestore.VariantRedacted {
		t.Errorf("variant not preserved in BundleRef: %+v", ts.last)
	}
}
