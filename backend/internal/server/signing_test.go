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

	"github.com/kuhlman-labs/fishhawk/backend/internal/githuboidc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
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
	// Multi-call per migration 0012: every Issue gets a fresh key;
	// any prior key for the same run is superseded but Verify still
	// uses the latest, so the runner doesn't see a 409.
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

func TestIssueSigningKey_RotatesOnSecondCall(t *testing.T) {
	// Multi-stage runs (per migration 0012) require each stage's
	// fresh runner process to issue its own key. The second Issue
	// must succeed and yield a key whose public half differs from
	// the first; older keys remain in the table for history.
	repo := newFakeSigningRepo()
	s := newSigningServer(t, repo)
	runID := uuid.New()

	first := issueRequest(t, s, runID, nil)
	if first.Code != http.StatusCreated {
		t.Fatalf("first issue: status = %d", first.Code)
	}
	var firstResp map[string]string
	_ = json.NewDecoder(first.Body).Decode(&firstResp)

	second := issueRequest(t, s, runID, nil)
	if second.Code != http.StatusCreated {
		t.Errorf("second issue: status = %d, want 201", second.Code)
	}
	var secondResp map[string]string
	_ = json.NewDecoder(second.Body).Decode(&secondResp)
	if firstResp["public_key"] == secondResp["public_key"] {
		t.Error("second issue should yield a new public_key, got the same")
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

// stubOIDCVerifier is the test seam for githuboidc.Verifier. Tests
// drive the verdict (claims or error) through fields rather than
// running real RSA verification — that's covered exhaustively in
// the githuboidc package tests.
type stubOIDCVerifier struct {
	claims    *githuboidc.Claims
	err       error
	gotToken  string
	gotExp    githuboidc.Expectations
	callCount int
}

func (s *stubOIDCVerifier) Verify(_ context.Context, token string, exp githuboidc.Expectations) (*githuboidc.Claims, error) {
	s.callCount++
	s.gotToken = token
	s.gotExp = exp
	if s.err != nil {
		return nil, s.err
	}
	return s.claims, nil
}

// fakeOIDCRunRepo provides GetRun for OIDC claim binding and
// ListStagesForRun for TTL resolution. Other run.Repository methods
// return errors so accidental calls are loud.
type fakeOIDCRunRepo struct {
	runRow *run.Run
	getErr error
	stages []*run.Stage
}

func (r *fakeOIDCRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	if r.runRow != nil && r.runRow.ID == id {
		return r.runRow, nil
	}
	return nil, run.ErrNotFound
}
func (r *fakeOIDCRunRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *fakeOIDCRunRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}
func (r *fakeOIDCRunRepo) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *fakeOIDCRunRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *fakeOIDCRunRepo) RetryRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *fakeOIDCRunRepo) SetRunPullRequestURL(context.Context, uuid.UUID, string) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *fakeOIDCRunRepo) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *fakeOIDCRunRepo) GetStage(context.Context, uuid.UUID) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *fakeOIDCRunRepo) ListStagesForRun(_ context.Context, _ uuid.UUID) ([]*run.Stage, error) {
	return r.stages, nil
}
func (r *fakeOIDCRunRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *fakeOIDCRunRepo) ListReviewStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (r *fakeOIDCRunRepo) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (r *fakeOIDCRunRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (r *fakeOIDCRunRepo) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *fakeOIDCRunRepo) TransitionStage(context.Context, uuid.UUID, run.StageState, *run.StageCompletion) (*run.Stage, error) {
	return nil, errors.New("not used")
}

func newOIDCSigningServer(t *testing.T, verifier githuboidc.Verifier, runRepo run.Repository) *Server {
	t.Helper()
	return New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  newFakeSigningRepo(),
		RunRepo:      runRepo,
		OIDCVerifier: verifier,
		OIDCAudience: "https://fishhawk.example.com",
	})
}

func issueRequestWithAuth(t *testing.T, s *Server, runID uuid.UUID, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	url := fmt.Sprintf("/v0/runs/%s/signing-key", runID)
	req := httptest.NewRequest(http.MethodPost, url, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestIssueSigningKey_OIDC_HappyPath(t *testing.T) {
	runID := uuid.New()
	runRepo := &fakeOIDCRunRepo{
		runRow: &run.Run{ID: runID, Repo: "kuhlman-labs/example", WorkflowID: "feature_change"},
	}
	verifier := &stubOIDCVerifier{
		claims: &githuboidc.Claims{Repository: "kuhlman-labs/example", Workflow: "feature_change"},
	}
	s := newOIDCSigningServer(t, verifier, runRepo)

	w := issueRequestWithAuth(t, s, runID, "Bearer eyJhbGciOiJSUzI1NiJ9.fake.signature")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if verifier.callCount != 1 {
		t.Errorf("Verify called %d times, want 1", verifier.callCount)
	}
	if verifier.gotExp.Repository != "kuhlman-labs/example" {
		t.Errorf("Expectations.Repository = %q", verifier.gotExp.Repository)
	}
	if verifier.gotExp.Workflow != "feature_change" {
		t.Errorf("Expectations.Workflow = %q", verifier.gotExp.Workflow)
	}
	if verifier.gotExp.Audience != "https://fishhawk.example.com" {
		t.Errorf("Expectations.Audience = %q", verifier.gotExp.Audience)
	}
	if verifier.gotToken != "eyJhbGciOiJSUzI1NiJ9.fake.signature" {
		t.Errorf("token = %q", verifier.gotToken)
	}
}

func TestIssueSigningKey_OIDC_MissingHeader(t *testing.T) {
	runID := uuid.New()
	runRepo := &fakeOIDCRunRepo{runRow: &run.Run{ID: runID, Repo: "x/y", WorkflowID: "w"}}
	s := newOIDCSigningServer(t, &stubOIDCVerifier{}, runRepo)

	w := issueRequestWithAuth(t, s, runID, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"oidc_missing"`) {
		t.Errorf("body missing oidc_missing code: %s", w.Body.String())
	}
}

func TestIssueSigningKey_OIDC_NonBearerScheme(t *testing.T) {
	runID := uuid.New()
	runRepo := &fakeOIDCRunRepo{runRow: &run.Run{ID: runID, Repo: "x/y", WorkflowID: "w"}}
	s := newOIDCSigningServer(t, &stubOIDCVerifier{}, runRepo)

	w := issueRequestWithAuth(t, s, runID, "Basic dXNlcjpwYXNz")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"oidc_invalid"`) {
		t.Errorf("body missing oidc_invalid: %s", w.Body.String())
	}
}

func TestIssueSigningKey_OIDC_InvalidToken(t *testing.T) {
	runID := uuid.New()
	runRepo := &fakeOIDCRunRepo{runRow: &run.Run{ID: runID, Repo: "x/y", WorkflowID: "w"}}
	verifier := &stubOIDCVerifier{err: githuboidc.ErrInvalidToken}
	s := newOIDCSigningServer(t, verifier, runRepo)

	w := issueRequestWithAuth(t, s, runID, "Bearer bogus")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"oidc_invalid"`) {
		t.Errorf("body missing oidc_invalid: %s", w.Body.String())
	}
}

func TestIssueSigningKey_OIDC_TokenExpired(t *testing.T) {
	runID := uuid.New()
	runRepo := &fakeOIDCRunRepo{runRow: &run.Run{ID: runID, Repo: "x/y", WorkflowID: "w"}}
	verifier := &stubOIDCVerifier{err: githuboidc.ErrTokenExpired}
	s := newOIDCSigningServer(t, verifier, runRepo)

	w := issueRequestWithAuth(t, s, runID, "Bearer expired")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "expired") {
		t.Errorf("body missing 'expired': %s", w.Body.String())
	}
}

func TestIssueSigningKey_OIDC_ClaimMismatch(t *testing.T) {
	runID := uuid.New()
	runRepo := &fakeOIDCRunRepo{runRow: &run.Run{ID: runID, Repo: "x/y", WorkflowID: "w"}}
	verifier := &stubOIDCVerifier{err: githuboidc.ErrClaimMismatch}
	s := newOIDCSigningServer(t, verifier, runRepo)

	w := issueRequestWithAuth(t, s, runID, "Bearer mismatched")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "bind") {
		t.Errorf("body missing 'bind': %s", w.Body.String())
	}
}

func TestIssueSigningKey_OIDC_UnknownKID(t *testing.T) {
	runID := uuid.New()
	runRepo := &fakeOIDCRunRepo{runRow: &run.Run{ID: runID, Repo: "x/y", WorkflowID: "w"}}
	verifier := &stubOIDCVerifier{err: githuboidc.ErrUnknownKID}
	s := newOIDCSigningServer(t, verifier, runRepo)

	w := issueRequestWithAuth(t, s, runID, "Bearer rotated")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "unknown key") {
		t.Errorf("body missing 'unknown key': %s", w.Body.String())
	}
}

func TestIssueSigningKey_OIDC_RunNotFound(t *testing.T) {
	runRepo := &fakeOIDCRunRepo{} // no runRow
	verifier := &stubOIDCVerifier{}
	s := newOIDCSigningServer(t, verifier, runRepo)

	w := issueRequestWithAuth(t, s, uuid.New(), "Bearer something")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if verifier.callCount != 0 {
		t.Errorf("Verifier shouldn't have been called when run lookup fails")
	}
}

func TestIssueSigningKey_OIDC_NoVerifier_FallsBackOpen(t *testing.T) {
	// Without a configured verifier the endpoint is unauthenticated
	// (v0 self-execution posture). Useful for the demo and for dev,
	// the operator opts into OIDC by wiring it.
	repo := newFakeSigningRepo()
	s := New(Config{Addr: "127.0.0.1:0", SigningRepo: repo})
	w := issueRequest(t, s, uuid.New(), nil)
	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201 (no verifier → unauthenticated)", w.Code)
	}
}

func TestIssueSigningKey_OIDC_VerifierWithoutAudience(t *testing.T) {
	runRepo := &fakeOIDCRunRepo{runRow: &run.Run{ID: uuid.New(), Repo: "x/y", WorkflowID: "w"}}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  newFakeSigningRepo(),
		RunRepo:      runRepo,
		OIDCVerifier: &stubOIDCVerifier{},
		// OIDCAudience deliberately empty — config error
	})
	w := issueRequestWithAuth(t, s, runRepo.runRow.ID, "Bearer x")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestIssueSigningKey_OIDC_VerifierWithoutRunRepo(t *testing.T) {
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  newFakeSigningRepo(),
		OIDCVerifier: &stubOIDCVerifier{},
		OIDCAudience: "https://fishhawk.example.com",
		// RunRepo deliberately nil — config error
	})
	w := issueRequestWithAuth(t, s, uuid.New(), "Bearer x")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestIssueSigningKey_TTLTracksStagePolicy(t *testing.T) {
	// policy.max_stage_runtime=60m on an active plan stage → TTL = 65m.
	spec60m := []byte(`version: "0.3"
workflows:
  feature_change:
    policy:
      max_stage_runtime: "60m"
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`)
	runID := uuid.New()
	runRepo := &fakeOIDCRunRepo{
		runRow: &run.Run{
			ID:           runID,
			Repo:         "kuhlman-labs/example",
			WorkflowID:   "feature_change",
			WorkflowSpec: spec60m,
		},
		stages: []*run.Stage{
			{
				ID:    uuid.New(),
				RunID: runID,
				Type:  run.StageTypePlan,
				State: run.StageStateDispatched,
			},
		},
	}
	s := New(Config{
		Addr:        "127.0.0.1:0",
		SigningRepo: newFakeSigningRepo(),
		RunRepo:     runRepo,
	})

	w := issueRequest(t, s, runID, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var got signingKeyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := 65 * time.Minute
	if ttl := got.ExpiresAt.Sub(got.IssuedAt); ttl != want {
		t.Errorf("TTL = %v, want %v", ttl, want)
	}
}

func TestIssueSigningKey_TTLNeverShrinksBelowDefault(t *testing.T) {
	// executor.timeout=5m on the plan stage → candidate = 10m < DefaultTTL → TTL = 30m.
	spec5m := []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
          timeout: "5m"
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`)
	runID := uuid.New()
	runRepo := &fakeOIDCRunRepo{
		runRow: &run.Run{
			ID:           runID,
			Repo:         "kuhlman-labs/example",
			WorkflowID:   "feature_change",
			WorkflowSpec: spec5m,
		},
		stages: []*run.Stage{
			{
				ID:    uuid.New(),
				RunID: runID,
				Type:  run.StageTypePlan,
				State: run.StageStateDispatched,
			},
		},
	}
	s := New(Config{
		Addr:        "127.0.0.1:0",
		SigningRepo: newFakeSigningRepo(),
		RunRepo:     runRepo,
	})

	w := issueRequest(t, s, runID, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var got signingKeyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ttl := got.ExpiresAt.Sub(got.IssuedAt); ttl != signing.DefaultTTL {
		t.Errorf("TTL = %v, want %v (DefaultTTL)", ttl, signing.DefaultTTL)
	}
}

// signingTTLSpec60m resolves a 60m budget for every stage via
// policy.max_stage_runtime, so budget + buffer = 65m > DefaultTTL.
var signingTTLSpec60m = []byte(`version: "0.3"
workflows:
  feature_change:
    policy:
      max_stage_runtime: "60m"
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`)

// issueAndDecodeTTL issues a signing key for runID against a server
// wired to runRepo and returns the response's ExpiresAt - IssuedAt.
func issueAndDecodeTTL(t *testing.T, runRepo *fakeOIDCRunRepo, runID uuid.UUID) time.Duration {
	t.Helper()
	s := New(Config{
		Addr:        "127.0.0.1:0",
		SigningRepo: newFakeSigningRepo(),
		RunRepo:     runRepo,
	})
	w := issueRequest(t, s, runID, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var got signingKeyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got.ExpiresAt.Sub(got.IssuedAt)
}

func TestIssueSigningKey_TTLPendingFirstImplement(t *testing.T) {
	// Decomposition-child shape (#1033, run d816e58a): the run's ONLY
	// stage is a still-pending implement stage — local-runner stages
	// stay `pending` for their whole execution, so the pre-fix
	// dispatched/running-only loop resolved no stage and issued
	// DefaultTTL (30m), expiring the key under a 31m implement stage.
	// The #1030 active-or-next fallback resolves the pending stage:
	// TTL = 60m budget + 5m buffer.
	runID := uuid.New()
	runRepo := &fakeOIDCRunRepo{
		runRow: &run.Run{
			ID:           runID,
			Repo:         "kuhlman-labs/example",
			WorkflowID:   "feature_change",
			WorkflowSpec: signingTTLSpec60m,
		},
		stages: []*run.Stage{
			{
				ID:       uuid.New(),
				RunID:    runID,
				Sequence: 1,
				Type:     run.StageTypeImplement,
				State:    run.StageStatePending,
			},
		},
	}
	want := 65 * time.Minute
	if ttl := issueAndDecodeTTL(t, runRepo, runID); ttl != want {
		t.Errorf("TTL = %v, want %v (budget + buffer, not DefaultTTL %v)", ttl, want, signing.DefaultTTL)
	}
}

func TestIssueSigningKey_TTLSucceededPlanPendingImplement(t *testing.T) {
	// Post-approval local gap: plan succeeded, implement still pending
	// (no orchestrator dispatch under a local runner) — terminal stages
	// are skipped and the implement stage's budget resolves.
	runID := uuid.New()
	runRepo := &fakeOIDCRunRepo{
		runRow: &run.Run{
			ID:           runID,
			Repo:         "kuhlman-labs/example",
			WorkflowID:   "feature_change",
			WorkflowSpec: signingTTLSpec60m,
		},
		stages: []*run.Stage{
			{
				ID:       uuid.New(),
				RunID:    runID,
				Sequence: 1,
				Type:     run.StageTypePlan,
				State:    run.StageStateSucceeded,
			},
			{
				ID:       uuid.New(),
				RunID:    runID,
				Sequence: 2,
				Type:     run.StageTypeImplement,
				State:    run.StageStatePending,
			},
		},
	}
	want := 65 * time.Minute
	if ttl := issueAndDecodeTTL(t, runRepo, runID); ttl != want {
		t.Errorf("TTL = %v, want %v (implement budget + buffer)", ttl, want)
	}
}

func TestIssueSigningKey_TTLAllTerminalFallsBackToDefault(t *testing.T) {
	// Every stage terminal → activeOrNextStage returns nil and the
	// resolver still degrades to DefaultTTL.
	runID := uuid.New()
	runRepo := &fakeOIDCRunRepo{
		runRow: &run.Run{
			ID:           runID,
			Repo:         "kuhlman-labs/example",
			WorkflowID:   "feature_change",
			WorkflowSpec: signingTTLSpec60m,
		},
		stages: []*run.Stage{
			{
				ID:       uuid.New(),
				RunID:    runID,
				Sequence: 1,
				Type:     run.StageTypePlan,
				State:    run.StageStateSucceeded,
			},
			{
				ID:       uuid.New(),
				RunID:    runID,
				Sequence: 2,
				Type:     run.StageTypeImplement,
				State:    run.StageStateSucceeded,
			},
		},
	}
	if ttl := issueAndDecodeTTL(t, runRepo, runID); ttl != signing.DefaultTTL {
		t.Errorf("TTL = %v, want %v (DefaultTTL)", ttl, signing.DefaultTTL)
	}
}

func TestIssueSigningKey_NoSpecFallsBackToDefault(t *testing.T) {
	// WorkflowSpec=nil → DefaultTTL regardless of stage state.
	runID := uuid.New()
	runRepo := &fakeOIDCRunRepo{
		runRow: &run.Run{
			ID:           runID,
			Repo:         "kuhlman-labs/example",
			WorkflowID:   "feature_change",
			WorkflowSpec: nil,
		},
	}
	s := New(Config{
		Addr:        "127.0.0.1:0",
		SigningRepo: newFakeSigningRepo(),
		RunRepo:     runRepo,
	})

	w := issueRequest(t, s, runID, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var got signingKeyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ttl := got.ExpiresAt.Sub(got.IssuedAt); ttl != signing.DefaultTTL {
		t.Errorf("TTL = %v, want %v (DefaultTTL)", ttl, signing.DefaultTTL)
	}
}

// GetRunAccountID satisfies the REQUIRED run.AccountGetter portion of
// run.Repository (E44.11 / #2074). Untenanted: this fake's runs carry no
// tenant account, matching its pre-promotion effective behavior.
func (*fakeOIDCRunRepo) GetRunAccountID(_ context.Context, _ uuid.UUID) (string, error) {
	return "", nil
}
