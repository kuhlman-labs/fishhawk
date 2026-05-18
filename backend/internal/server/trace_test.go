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
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
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
	rid := p.RunID
	return &audit.Entry{ID: uuid.New(), RunID: &rid}, nil
}

func (a *auditFake) AppendGlobalChained(_ context.Context, _ audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, errors.New("auditFake: AppendGlobalChained not used")
}

func (a *auditFake) ListGlobal(_ context.Context) ([]*audit.Entry, error) {
	return nil, errors.New("auditFake: ListGlobal not used")
}
func (a *auditFake) ListAll(_ context.Context, _ audit.ListAllParams) ([]*audit.Entry, error) {
	return nil, errors.New("auditFake: ListAll not used")
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

func TestShipTrace_TransitionsStageToAwaitingApproval(t *testing.T) {
	// Wire a RunRepo seeded with a stage in dispatched, ship a
	// trace, and confirm the stage advanced to awaiting_approval
	// so the approval handler can act on it next.
	s, sf, _, _ := newTraceServer(t)
	rr := newApprovalRunRepo()
	stage := rr.seedStage(run.StageStateDispatched)
	s.cfg.RunRepo = rr // inject after construction; New is the only setup we needed

	priv, _ := sf.issue(t, stage.RunID)
	w := shipRequest(t, s, stage.RunID, stage.ID, "raw", priv, []byte("b"), "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if rr.stages[stage.ID].State != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval",
			rr.stages[stage.ID].State)
	}
	// Two-step walk: dispatched → running → awaiting_approval.
	if len(rr.transitions) != 2 {
		t.Fatalf("transitions = %d, want 2:\n%+v", len(rr.transitions), rr.transitions)
	}
	if rr.transitions[0].To != run.StageStateRunning ||
		rr.transitions[1].To != run.StageStateAwaitingApproval {
		t.Errorf("transitions = %+v, want [running, awaiting_approval]", rr.transitions)
	}
}

// TestShipTrace_PendingStage_WalksThroughDispatched is the
// local-runner counterpart to the GHA flow (#416-followup): the
// GHA dispatcher transitions pending → dispatched after firing
// workflow_dispatch, but the local-runner path skips that step
// (there's no workflow_dispatch fire). Without this branch the
// trace handler's pending → running transition would be rejected
// by the state machine and the stage would stay in pending
// forever. The handler walks pending → dispatched first when it
// finds the stage in pending, then continues the normal chain.
func TestShipTrace_PendingStage_WalksThroughDispatched(t *testing.T) {
	s, sf, _, _ := newTraceServer(t)
	rr := newApprovalRunRepo()
	stage := rr.seedStage(run.StageStatePending) // the local-runner shape
	s.cfg.RunRepo = rr

	priv, _ := sf.issue(t, stage.RunID)
	w := shipRequest(t, s, stage.RunID, stage.ID, "raw", priv, []byte("b"), "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if rr.stages[stage.ID].State != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval (pending start should still reach the terminal)",
			rr.stages[stage.ID].State)
	}
	// Three-step walk: pending → dispatched → running → awaiting_approval.
	if len(rr.transitions) != 3 {
		t.Fatalf("transitions = %d, want 3 (the extra step is pending → dispatched):\n%+v",
			len(rr.transitions), rr.transitions)
	}
	if rr.transitions[0].To != run.StageStateDispatched {
		t.Errorf("transitions[0] = %q, want dispatched (the new step)", rr.transitions[0].To)
	}
	if rr.transitions[1].To != run.StageStateRunning {
		t.Errorf("transitions[1] = %q, want running", rr.transitions[1].To)
	}
	if rr.transitions[2].To != run.StageStateAwaitingApproval {
		t.Errorf("transitions[2] = %q, want awaiting_approval", rr.transitions[2].To)
	}
}

// TestShipTrace_DispatchedStage_SkipsExtraStep guards the
// regression direction: when the stage IS already in dispatched
// (the GHA happy path), we don't accidentally walk it through
// dispatched again — same-state is a no-op but the audit chain
// shouldn't grow extra rows.
func TestShipTrace_DispatchedStage_SkipsExtraStep(t *testing.T) {
	s, sf, _, _ := newTraceServer(t)
	rr := newApprovalRunRepo()
	stage := rr.seedStage(run.StageStateDispatched)
	s.cfg.RunRepo = rr

	priv, _ := sf.issue(t, stage.RunID)
	w := shipRequest(t, s, stage.RunID, stage.ID, "raw", priv, []byte("b"), "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	// Still the original two-step walk for the GHA shape.
	if len(rr.transitions) != 2 {
		t.Errorf("transitions = %d, want 2 (GHA path is unchanged):\n%+v",
			len(rr.transitions), rr.transitions)
	}
}

func TestShipTrace_GatelessStage_TransitionsStraightToSucceeded(t *testing.T) {
	// Implement stages have no approval gate per workflows.yaml.
	// The trace upload handler must walk dispatched → running →
	// succeeded directly (skipping awaiting_approval) and trigger
	// the orchestrator so the next stage gets dispatched. Without
	// this branch the stage hangs at awaiting_approval waiting for
	// a human action the workflow author never specified. (#207.)
	s, sf, _, _ := newTraceServer(t)
	rr := newApprovalRunRepo()
	stage := rr.seedGatelessStage(run.StageStateDispatched)
	s.cfg.RunRepo = rr
	// Wire a real orchestrator (no GitHub client; dispatch is a
	// no-op for human stages, which is fine for this assertion).
	s.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: rr}

	priv, _ := sf.issue(t, stage.RunID)
	w := shipRequest(t, s, stage.RunID, stage.ID, "raw", priv, []byte("b"), "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if rr.stages[stage.ID].State != run.StageStateSucceeded {
		t.Errorf("gateless stage state = %q, want succeeded", rr.stages[stage.ID].State)
	}
	// Two-step walk: dispatched → running → succeeded. (No
	// awaiting_approval — that's the bug we're fixing.)
	if len(rr.transitions) < 2 {
		t.Fatalf("transitions = %d, want at least 2:\n%+v", len(rr.transitions), rr.transitions)
	}
	if rr.transitions[0].To != run.StageStateRunning {
		t.Errorf("transitions[0] = %q, want running", rr.transitions[0].To)
	}
	if rr.transitions[1].To != run.StageStateSucceeded {
		t.Errorf("transitions[1] = %q, want succeeded (NOT awaiting_approval)", rr.transitions[1].To)
	}
}

func TestShipTrace_GatelessStage_NoOrchestrator_StillTransitionsToSucceeded(t *testing.T) {
	// Without an orchestrator wired the trace handler still
	// transitions the stage to succeeded — the orchestrator just
	// can't dispatch the next stage. Confirms the orchestrator
	// invocation is best-effort, not load-bearing for the
	// transition itself.
	s, sf, _, _ := newTraceServer(t)
	rr := newApprovalRunRepo()
	stage := rr.seedGatelessStage(run.StageStateDispatched)
	s.cfg.RunRepo = rr
	// No orchestrator wired.

	priv, _ := sf.issue(t, stage.RunID)
	w := shipRequest(t, s, stage.RunID, stage.ID, "raw", priv, []byte("b"), "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if rr.stages[stage.ID].State != run.StageStateSucceeded {
		t.Errorf("stage state = %q, want succeeded even without orchestrator", rr.stages[stage.ID].State)
	}
}

func TestShipTrace_TransitionFailureDoesntUnwindUpload(t *testing.T) {
	// If the post-upload transition errors (e.g., stage already
	// terminal because of a concurrent path), we log + return 202
	// — the trace itself is already stored and audited. A stuck
	// stage is surface-able via GET /v0/runs/{id}/stages.
	s, sf, _, _ := newTraceServer(t)
	rr := newApprovalRunRepo()
	rr.transitionErr = errors.New("state machine refusal")
	stage := rr.seedStage(run.StageStateDispatched)
	s.cfg.RunRepo = rr

	priv, _ := sf.issue(t, stage.RunID)
	w := shipRequest(t, s, stage.RunID, stage.ID, "raw", priv, []byte("b"), "")
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202 (transition failure must not unwind)", w.Code)
	}
}

func TestShipTrace_NoRunRepo_StillAccepts(t *testing.T) {
	// A backend deployed without a Postgres run repository should
	// still accept trace uploads; only the post-upload transition
	// gets skipped. This keeps the trace endpoint useful for
	// minimal smoke deployments before run-state is wired.
	s, sf, _, _ := newTraceServer(t)
	// Don't wire RunRepo.
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)
	w := shipRequest(t, s, runID, stageID, "raw", priv, []byte("b"), "")
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", w.Code)
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

// TestShipTrace_StampsRunnerKindInAuditPayload pins the E22.7 / #404
// invariant: when a RunRepo is wired and the run has a runner_kind,
// the trace_uploaded audit payload carries that field. The backend
// is the source of truth on provenance — the runner never declares it.
func TestShipTrace_StampsRunnerKindInAuditPayload(t *testing.T) {
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()

	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr := newFakeRepo()
	// Seed a run with runner_kind=local — the local-runner mode
	// (Phase C) would create runs tagged this way. Confirms the
	// audit payload reflects what's actually on the run row.
	rr.runs[runID] = &run.Run{
		ID:         runID,
		Repo:       "x/y",
		RunnerKind: run.RunnerKindLocal,
		State:      run.StatePending,
	}

	s := New(Config{
		Addr:        "127.0.0.1:0",
		SigningRepo: sf,
		TraceStore:  ts,
		AuditRepo:   au,
		RunRepo:     rr,
	})

	w := shipRequest(t, s, runID, stageID, "raw", priv, []byte("body"), "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	au.mu.Lock()
	defer au.mu.Unlock()
	if len(au.appended) != 1 {
		t.Fatalf("audit appended %d, want 1", len(au.appended))
	}
	var payload map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got, _ := payload["runner_kind"].(string); got != run.RunnerKindLocal {
		t.Errorf("payload.runner_kind = %q, want local", got)
	}
}

// TestShipTrace_OmitsRunnerKindWhenRunRepoNil pins the back-compat
// path: when the trace handler runs in a minimal config without a
// RunRepo (legacy dev backends), the audit payload omits the field
// rather than stamping a guessed default. Readers treat missing as
// legacy / github_actions per ADR-022's back-compat semantics.
func TestShipTrace_OmitsRunnerKindWhenRunRepoNil(t *testing.T) {
	s, sf, _, au := newTraceServer(t) // no RunRepo wired
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	w := shipRequest(t, s, runID, stageID, "raw", priv, []byte("body"), "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}

	au.mu.Lock()
	defer au.mu.Unlock()
	var payload map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if _, present := payload["runner_kind"]; present {
		t.Errorf("payload should omit runner_kind when no RunRepo; got %v", payload["runner_kind"])
	}
}
