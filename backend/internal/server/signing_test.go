package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// fakeSigningRepo is the in-memory signing.Repository for handler
// tests. Issue stores the public half and a stable issued/expires
// pair so assertions can compare timestamps deterministically.
type fakeSigningRepo struct {
	mu         sync.Mutex
	keys       map[uuid.UUID]*signing.Key
	issueErr   error
	now        func() time.Time
	defaultErr error
}

func newFakeSigningRepo() *fakeSigningRepo {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	return &fakeSigningRepo{
		keys: map[uuid.UUID]*signing.Key{},
		now:  func() time.Time { return t0 },
	}
}

func (f *fakeSigningRepo) Issue(_ context.Context, runID uuid.UUID, ttl time.Duration) (*signing.IssuedKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	if _, ok := f.keys[runID]; ok {
		return nil, signing.ErrAlreadyIssued
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	now := f.now()
	f.keys[runID] = &signing.Key{
		RunID:     runID,
		PublicKey: pub,
		IssuedAt:  now,
		ExpiresAt: now.Add(ttl),
	}
	return &signing.IssuedKey{
		RunID:      runID,
		PublicKey:  pub,
		PrivateKey: priv,
		IssuedAt:   now,
		ExpiresAt:  now.Add(ttl),
	}, nil
}

func (f *fakeSigningRepo) Get(_ context.Context, runID uuid.UUID) (*signing.Key, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.defaultErr != nil {
		return nil, f.defaultErr
	}
	k, ok := f.keys[runID]
	if !ok {
		return nil, signing.ErrNotFound
	}
	return k, nil
}

func (f *fakeSigningRepo) Verify(_ context.Context, _ uuid.UUID, _ []byte, _ []byte) error {
	return errors.New("fakeSigningRepo: Verify not implemented")
}

func newSigningServer(t *testing.T, repo signing.Repository) *Server {
	t.Helper()
	return New(Config{Addr: "127.0.0.1:0", SigningRepo: repo})
}

func issueRequest(t *testing.T, s *Server, runID uuid.UUID, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = strings.NewReader(string(raw))
	}
	url := fmt.Sprintf("/v0/runs/%s/signing-key", runID)
	var req *http.Request
	if rdr != nil {
		req = httptest.NewRequest(http.MethodPost, url, rdr)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(http.MethodPost, url, nil)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestIssueSigningKey_HappyPath_DefaultTTL(t *testing.T) {
	repo := newFakeSigningRepo()
	s := newSigningServer(t, repo)
	runID := uuid.New()

	w := issueRequest(t, s, runID, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	var got signingKeyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RunID != runID {
		t.Errorf("RunID = %s, want %s", got.RunID, runID)
	}
	pub, err := base64.StdEncoding.DecodeString(got.PublicKey)
	if err != nil {
		t.Errorf("PublicKey not valid base64: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Errorf("PublicKey len = %d, want %d", len(pub), ed25519.PublicKeySize)
	}
	priv, err := base64.StdEncoding.DecodeString(got.PrivateKey)
	if err != nil {
		t.Errorf("PrivateKey not valid base64: %v", err)
	}
	if len(priv) != ed25519.PrivateKeySize {
		t.Errorf("PrivateKey len = %d, want %d", len(priv), ed25519.PrivateKeySize)
	}
	// Default TTL = 30 minutes per signing.DefaultTTL.
	if got.ExpiresAt.Sub(got.IssuedAt) != signing.DefaultTTL {
		t.Errorf("expiry window = %v, want %v", got.ExpiresAt.Sub(got.IssuedAt), signing.DefaultTTL)
	}
}

func TestIssueSigningKey_CustomTTL(t *testing.T) {
	repo := newFakeSigningRepo()
	s := newSigningServer(t, repo)
	runID := uuid.New()

	w := issueRequest(t, s, runID, map[string]int{"ttl_seconds": 600})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var got signingKeyResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.ExpiresAt.Sub(got.IssuedAt) != 10*time.Minute {
		t.Errorf("expiry window = %v, want 10m", got.ExpiresAt.Sub(got.IssuedAt))
	}
}

func TestIssueSigningKey_TTLOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		ttl  int
	}{
		{"under min", minTTLSeconds - 1},
		{"over max", maxTTLSeconds + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeSigningRepo()
			s := newSigningServer(t, repo)
			w := issueRequest(t, s, uuid.New(), map[string]int{"ttl_seconds": tc.ttl})
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
			if !strings.Contains(w.Body.String(), `"validation_failed"`) {
				t.Errorf("body missing validation_failed: %s", w.Body.String())
			}
		})
	}
}

func TestIssueSigningKey_BadUUID(t *testing.T) {
	s := newSigningServer(t, newFakeSigningRepo())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/not-a-uuid/signing-key", nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestIssueSigningKey_BadJSONBody(t *testing.T) {
	s := newSigningServer(t, newFakeSigningRepo())
	url := fmt.Sprintf("/v0/runs/%s/signing-key", uuid.New())
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestIssueSigningKey_UnknownField(t *testing.T) {
	s := newSigningServer(t, newFakeSigningRepo())
	w := issueRequest(t, s, uuid.New(), map[string]any{"ttl_seconds": 600, "extra": true})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 on unknown field", w.Code)
	}
}

func TestIssueSigningKey_AlreadyIssued(t *testing.T) {
	repo := newFakeSigningRepo()
	s := newSigningServer(t, repo)
	runID := uuid.New()

	if w := issueRequest(t, s, runID, nil); w.Code != http.StatusCreated {
		t.Fatalf("first issue: status = %d", w.Code)
	}
	w := issueRequest(t, s, runID, nil)
	if w.Code != http.StatusConflict {
		t.Errorf("second issue: status = %d, want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"signing_key_already_issued"`) {
		t.Errorf("body missing code: %s", w.Body.String())
	}
}

func TestIssueSigningKey_RepoError(t *testing.T) {
	repo := newFakeSigningRepo()
	repo.issueErr = errors.New("disk full")
	s := newSigningServer(t, repo)
	w := issueRequest(t, s, uuid.New(), nil)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"internal_error"`) {
		t.Errorf("body missing internal_error: %s", w.Body.String())
	}
}

func TestIssueSigningKey_NilRepoConfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	w := issueRequest(t, s, uuid.New(), nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "signing_repo_unconfigured") {
		t.Errorf("body missing code: %s", w.Body.String())
	}
}

func TestIssueSigningKey_PrivateKeySigsVerifyAgainstPublic(t *testing.T) {
	// End-to-end: a caller can take the returned (public, private)
	// pair and verify a signature with each half. Catches a class
	// of bugs where, e.g., we accidentally swapped halves in the
	// response.
	repo := newFakeSigningRepo()
	s := newSigningServer(t, repo)
	runID := uuid.New()

	w := issueRequest(t, s, runID, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d", w.Code)
	}
	var resp signingKeyResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	pub, _ := base64.StdEncoding.DecodeString(resp.PublicKey)
	priv, _ := base64.StdEncoding.DecodeString(resp.PrivateKey)

	msg := []byte("hello fishhawk")
	sig := ed25519.Sign(priv, msg)
	if !ed25519.Verify(pub, msg, sig) {
		t.Error("signature did not verify against returned public key")
	}
}
