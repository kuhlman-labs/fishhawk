package server

import (
	"bytes"
	"compress/gzip"
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
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
	"github.com/kuhlman-labs/fishhawk/pricing"
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
	mu             sync.Mutex
	appended       []audit.ChainAppendParams
	globalAppended []audit.GlobalChainAppendParams
	appendErr      error
	// seeded is pre-existing history returned by ListAll alongside the
	// entries appended during the test. The spend-alert check (#649)
	// reads cost_recorded entries via ListAll to build its rolling
	// baseline, so tests seed prior-hour samples here.
	seeded []*audit.Entry
}

func newAuditFake() *auditFake { return &auditFake{} }

func (a *auditFake) Append(_ context.Context, _ audit.AppendParams) (*audit.Entry, error) {
	return nil, errors.New("auditFake: Append not used")
}

func (a *auditFake) ChainsByParent(_ context.Context, _ uuid.UUID, _ bool) ([]*audit.Entry, error) {
	return nil, nil
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

func (a *auditFake) AppendGlobalChained(_ context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error) {
	if a.appendErr != nil {
		return nil, a.appendErr
	}
	a.mu.Lock()
	a.globalAppended = append(a.globalAppended, p)
	a.mu.Unlock()
	return &audit.Entry{ID: uuid.New()}, nil
}

func (a *auditFake) ListGlobal(_ context.Context) ([]*audit.Entry, error) {
	return nil, errors.New("auditFake: ListGlobal not used")
}

// ListAll returns the seeded history plus any entries appended during
// the test, filtered by p.Category when set. This backs the
// spend-alert check's cost-history read (#649); the trace handler is
// the only caller, and it filters to cost_recorded.
func (a *auditFake) ListAll(_ context.Context, p audit.ListAllParams) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*audit.Entry
	for _, e := range a.seeded {
		if p.Category == nil || e.Category == *p.Category {
			out = append(out, e)
		}
	}
	for i := range a.appended {
		ap := a.appended[i]
		if p.Category != nil && ap.Category != *p.Category {
			continue
		}
		rid := ap.RunID
		out = append(out, &audit.Entry{
			RunID:     &rid,
			StageID:   ap.StageID,
			Timestamp: ap.Timestamp,
			Category:  ap.Category,
			Payload:   ap.Payload,
		})
	}
	return out, nil
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

// TestAdvanceStageAfterTrace_PlanStage_NoArtifact_StaysRunning pins the
// #603 gate: a gated plan stage whose ArtifactRepo holds no standard_v1
// plan artifact is left in running by the trace handler — it must NOT
// reach awaiting_approval on trace upload alone. The complementary
// sub-test pre-seeds a valid plan artifact so the trace handler DOES
// advance (the future plan-first ordering), proving the gate keys on the
// artifact rather than the stage type.
func TestAdvanceStageAfterTrace_PlanStage_NoArtifact_StaysRunning(t *testing.T) {
	t.Run("no artifact stays running", func(t *testing.T) {
		sf := newSigningFake()
		rr := newApprovalRunRepo()
		art := newFakeArtifactRepo()                    // empty: no plan artifact for the stage
		stage := rr.seedStage(run.StageStateDispatched) // plan-type, gated
		s := New(Config{
			Addr:         "127.0.0.1:0",
			SigningRepo:  sf,
			TraceStore:   newTraceStoreFake(),
			AuditRepo:    newAuditFake(),
			RunRepo:      rr,
			ArtifactRepo: art,
		})

		priv, _ := sf.issue(t, stage.RunID)
		w := shipRequest(t, s, stage.RunID, stage.ID, "raw", priv, []byte("b"), "")
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
		}
		if got := rr.stages[stage.ID].State; got != run.StageStateRunning {
			t.Errorf("stage state = %q, want running (no plan artifact → gate leaves it in running)", got)
		}
		for _, tr := range rr.transitions {
			if tr.To == run.StageStateAwaitingApproval {
				t.Errorf("stage transitioned to awaiting_approval with no plan artifact:\n%+v", rr.transitions)
			}
		}
	})

	t.Run("pre-seeded plan artifact advances", func(t *testing.T) {
		sf := newSigningFake()
		rr := newApprovalRunRepo()
		art := newFakeArtifactRepo()
		stage := rr.seedStage(run.StageStateDispatched) // plan-type, gated
		// Pre-seed a standard_v1 plan artifact for the stage so the gate
		// passes — modelling the future plan-first upload ordering.
		seedBudgetPlanArtifact(t, art, stage.ID, &plan.Plan{PlanVersion: "standard_v1"})
		s := New(Config{
			Addr:         "127.0.0.1:0",
			SigningRepo:  sf,
			TraceStore:   newTraceStoreFake(),
			AuditRepo:    newAuditFake(),
			RunRepo:      rr,
			ArtifactRepo: art,
		})

		priv, _ := sf.issue(t, stage.RunID)
		w := shipRequest(t, s, stage.RunID, stage.ID, "raw", priv, []byte("b"), "")
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
		}
		if got := rr.stages[stage.ID].State; got != run.StageStateAwaitingApproval {
			t.Errorf("stage state = %q, want awaiting_approval (plan artifact present → gate passes)", got)
		}
	})
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

// ── runtime_observed emission from trace upload ───────────────────────────────

// makeTimedBundle builds a minimal gzip JSONL bundle with a manifest
// (agent_failed=false), two intermediate events at t0 and t1, and a
// trailer. ExtractTiming will return (t0, t1, true) for this bundle.
func makeTimedBundle(t *testing.T, t0, t1 time.Time) []byte {
	t.Helper()
	type line struct {
		Seq  int             `json:"seq"`
		TS   time.Time       `json:"ts"`
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data,omitempty"`
	}
	lines := []line{
		{Seq: 1, TS: t0.Add(-time.Second), Kind: bundle.EventKindManifest,
			Data: json.RawMessage(`{"bundle_schema":"v1","agent_failed":false}`)},
		{Seq: 2, TS: t0, Kind: "agent_start", Data: json.RawMessage(`{}`)},
		{Seq: 3, TS: t1, Kind: "agent_end", Data: json.RawMessage(`{}`)},
		{Seq: 4, TS: t1.Add(time.Second), Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
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

// seedPlanArtifactForRun inserts a standard_v1 plan artifact with the
// given predicted_runtime_minutes into art, associated with planStageID.
func seedPlanArtifactForRun(t *testing.T, art *fakeArtifactRepo, planStageID uuid.UUID, predictedMinutes int) {
	t.Helper()
	p := &plan.Plan{
		PlanVersion:                "standard_v1",
		PredictedRuntimeMinutes:    predictedMinutes,
		PredictedRuntimeConfidence: plan.RuntimeConfidenceMedium,
	}
	seedBudgetPlanArtifact(t, art, planStageID, p)
}

// TestShipTrace_EmitRuntimeObserved_ImplementStage verifies that uploading
// a trace for an implement stage emits exactly one runtime_observed audit
// entry with stage_type=implement and actual_seconds present.
func TestShipTrace_EmitRuntimeObserved_ImplementStage(t *testing.T) {
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()

	runRow := rr.seedRun()

	// Plan stage (succeeded) with a plan artifact.
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateSucceeded)
	// planStage.Type is already StageTypePlan.
	seedPlanArtifactForRun(t, art, planStage.ID, 15)

	// Implement stage in dispatched state.
	implStage := rr.seedStage(runRow.ID, 1, run.StageStateDispatched)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = false

	priv, _ := sf.issue(t, runRow.ID)

	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(12 * time.Minute)
	bundleBytes := makeTimedBundle(t, t0, t1)

	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		TraceStore:   ts,
		AuditRepo:    au,
		RunRepo:      rr,
		ArtifactRepo: art,
	})

	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	// Find runtime_observed in the audit entries.
	au.mu.Lock()
	defer au.mu.Unlock()
	var ro *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == "runtime_observed" {
			cp := au.appended[i]
			ro = &cp
			break
		}
	}
	if ro == nil {
		t.Fatal("no runtime_observed audit entry emitted")
	}
	var payload map[string]any
	if err := json.Unmarshal(ro.Payload, &payload); err != nil {
		t.Fatalf("decode runtime_observed payload: %v", err)
	}
	if got, _ := payload["stage_type"].(string); got != "implement" {
		t.Errorf("stage_type = %q, want implement", got)
	}
	if _, ok := payload["actual_seconds"]; !ok {
		t.Error("payload missing actual_seconds")
	}
	if got, _ := payload["outcome"].(string); got != "succeeded" {
		t.Errorf("outcome = %q, want succeeded", got)
	}
}

// TestShipTrace_EmitRuntimeObserved_PlanStage verifies that uploading a
// trace for a plan stage does NOT emit a runtime_observed entry.
func TestShipTrace_EmitRuntimeObserved_PlanStage(t *testing.T) {
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()

	runRow := rr.seedRun()

	// Plan stage in dispatched state.
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateDispatched)
	// Type is already StageTypePlan; RequiresApproval=false for simplicity.
	planStage.RequiresApproval = false

	priv, _ := sf.issue(t, runRow.ID)

	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Minute)
	bundleBytes := makeTimedBundle(t, t0, t1)

	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		TraceStore:   ts,
		AuditRepo:    au,
		RunRepo:      rr,
		ArtifactRepo: art,
	})

	w := shipRequest(t, s, runRow.ID, planStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	au.mu.Lock()
	defer au.mu.Unlock()
	for _, e := range au.appended {
		if e.Category == "runtime_observed" {
			t.Errorf("unexpected runtime_observed entry emitted for plan stage")
		}
	}
}

// packManifestBundle builds a minimal gzipped JSONL trace bundle whose
// only line is a manifest carrying the given model + token split — the
// signed wire form the cost rollup reads. Used by the cost seam test.
func packManifestBundle(t *testing.T, m bundle.Manifest) []byte {
	t.Helper()
	mdata, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	line := bundle.Line{
		Seq:       0,
		Timestamp: time.Now().UTC(),
		Kind:      bundle.EventKindManifest,
		Data:      mdata,
	}
	lb, err := json.Marshal(line)
	if err != nil {
		t.Fatalf("marshal line: %v", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(append(lb, '\n')); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// TestShipTrace_RecordsCost is the cross-boundary seam test for the
// per-run cost rollup (#649). It POSTs a trace bundle whose manifest
// carries a model + input/output token split and asserts end-to-end
// that the manifest → handler → cost → audit/run-record path produces:
//
//	(a) a cost_recorded audit entry tied to the run, carrying the
//	    pricing-derived usd and the token split, and
//	(b) the same usd accumulated on the run record, with the resolved
//	    model id pinned.
//
// This exercises the boundaries together rather than only per-layer
// units (cf. #618): a regression in any of the manifest decode, the
// pricing call, the audit payload shape, or the run-record accumulator
// trips this test even when the layer-local unit tests still pass.
func TestShipTrace_RecordsCost(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	rr := newApprovalRunRepo()
	stage := rr.seedStage(run.StageStateDispatched)
	rr.seedRun(&run.Run{ID: stage.RunID, Repo: "kuhlman-labs/fishhawk"})
	s.cfg.RunRepo = rr

	const model = "claude-opus-4-8"
	const inTok, outTok = 1000, 2000
	wantUSD, ok := pricing.Cost(model, inTok, outTok)
	if !ok {
		t.Fatalf("pricing.Cost returned ok=false for %q — fixture model must be priced", model)
	}

	bundleBytes := packManifestBundle(t, bundle.Manifest{
		BundleSchema: "trace-bundle-v0",
		RunID:        stage.RunID.String(),
		StageID:      stage.ID.String(),
		Agent:        "claude-code",
		Model:        model,
		InputTokens:  inTok,
		OutputTokens: outTok,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
	})

	priv, _ := sf.issue(t, stage.RunID)
	w := shipRequest(t, s, stage.RunID, stage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	// (a) cost_recorded audit entry with the pricing-derived usd.
	au.mu.Lock()
	var costEntry *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == "cost_recorded" {
			costEntry = &au.appended[i]
			break
		}
	}
	au.mu.Unlock()
	if costEntry == nil {
		t.Fatal("no cost_recorded audit entry written")
	}
	if costEntry.RunID != stage.RunID {
		t.Errorf("cost_recorded RunID = %s, want %s", costEntry.RunID, stage.RunID)
	}
	if costEntry.StageID == nil || *costEntry.StageID != stage.ID {
		t.Errorf("cost_recorded StageID = %v, want %s", costEntry.StageID, stage.ID)
	}
	var payload struct {
		Model        string  `json:"model"`
		InputTokens  int     `json:"input_tokens"`
		OutputTokens int     `json:"output_tokens"`
		USD          float64 `json:"usd"`
		KnownModel   bool    `json:"known_model"`
		KnownUsage   bool    `json:"known_usage"`
		PricingAsOf  string  `json:"pricing_as_of"`
	}
	if err := json.Unmarshal(costEntry.Payload, &payload); err != nil {
		t.Fatalf("decode cost_recorded payload: %v", err)
	}
	if payload.Model != model || payload.InputTokens != inTok || payload.OutputTokens != outTok {
		t.Errorf("cost_recorded payload token/model mismatch: %+v", payload)
	}
	if payload.USD != wantUSD {
		t.Errorf("cost_recorded usd = %v, want %v (pricing.Cost)", payload.USD, wantUSD)
	}
	if !payload.KnownModel {
		t.Errorf("cost_recorded known_model = false, want true for a priced model")
	}
	if !payload.KnownUsage {
		t.Errorf("cost_recorded known_usage = false, want true for a non-zero token split")
	}
	if payload.PricingAsOf != pricing.AsOf {
		t.Errorf("cost_recorded pricing_as_of = %q, want %q", payload.PricingAsOf, pricing.AsOf)
	}

	// (b) per-run total accumulated on the run record + model pinned.
	got, err := rr.GetRun(t.Context(), stage.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.CostUSDTotal != wantUSD {
		t.Errorf("run.CostUSDTotal = %v, want %v", got.CostUSDTotal, wantUSD)
	}
	if got.ResolvedModel != model {
		t.Errorf("run.ResolvedModel = %q, want %q", got.ResolvedModel, model)
	}
}

// TestShipTrace_RecordsCostOncePerBundle is the regression test for the
// 2x double-count (#678). The runner POSTs each stage bundle twice — once
// as the raw variant, once as the redacted variant — with identical
// signed manifest token counts. Before the fix, recordCost ran on every
// variant upload, so the per-run cost and the cost_recorded ledger were
// doubled. This double-uploads both variants of the SAME bundle and
// asserts cost is recorded exactly once: one cost_recorded audit entry
// and an un-doubled CostUSDTotal. It fails if recordCost is not gated on
// the raw variant.
func TestShipTrace_RecordsCostOncePerBundle(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	rr := newApprovalRunRepo()
	stage := rr.seedStage(run.StageStateDispatched)
	rr.seedRun(&run.Run{ID: stage.RunID, Repo: "kuhlman-labs/fishhawk"})
	s.cfg.RunRepo = rr

	const model = "claude-opus-4-8"
	const inTok, outTok = 1000, 2000
	wantUSD, ok := pricing.Cost(model, inTok, outTok)
	if !ok {
		t.Fatalf("pricing.Cost returned ok=false for %q — fixture model must be priced", model)
	}

	bundleBytes := packManifestBundle(t, bundle.Manifest{
		BundleSchema: "trace-bundle-v0",
		RunID:        stage.RunID.String(),
		StageID:      stage.ID.String(),
		Agent:        "claude-code",
		Model:        model,
		InputTokens:  inTok,
		OutputTokens: outTok,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
	})

	priv, _ := sf.issue(t, stage.RunID)
	// Raw first (authoritative), then redacted — the runner's real
	// two-POST sequence for a single bundle.
	if w := shipRequest(t, s, stage.RunID, stage.ID, "raw", priv, bundleBytes, ""); w.Code != http.StatusAccepted {
		t.Fatalf("raw upload status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	if w := shipRequest(t, s, stage.RunID, stage.ID, "redacted", priv, bundleBytes, ""); w.Code != http.StatusAccepted {
		t.Fatalf("redacted upload status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	// Exactly one cost_recorded entry across both uploads — not two.
	au.mu.Lock()
	costEntries := 0
	for i := range au.appended {
		if au.appended[i].Category == "cost_recorded" {
			costEntries++
		}
	}
	au.mu.Unlock()
	if costEntries != 1 {
		t.Fatalf("cost_recorded entries = %d, want 1 (double-uploaded raw+redacted must record cost once)", costEntries)
	}

	// Per-run total is the single un-doubled pricing figure, not 2x.
	got, err := rr.GetRun(t.Context(), stage.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.CostUSDTotal != wantUSD {
		t.Errorf("run.CostUSDTotal = %v, want %v (not doubled to %v)", got.CostUSDTotal, wantUSD, 2*wantUSD)
	}
}

// TestShipTrace_RecordsCost_UnknownModelZero pins the unknown-model
// arm: an unpriced model id records a cost_recorded entry at usd=0 with
// known_model=false rather than being dropped or guessed, and the run
// total stays at 0.
func TestShipTrace_RecordsCost_UnknownModelZero(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	rr := newApprovalRunRepo()
	stage := rr.seedStage(run.StageStateDispatched)
	rr.seedRun(&run.Run{ID: stage.RunID})
	s.cfg.RunRepo = rr

	bundleBytes := packManifestBundle(t, bundle.Manifest{
		BundleSchema: "trace-bundle-v0",
		RunID:        stage.RunID.String(),
		StageID:      stage.ID.String(),
		Agent:        "claude-code",
		Model:        "gpt-some-future-model",
		InputTokens:  500,
		OutputTokens: 500,
	})

	priv, _ := sf.issue(t, stage.RunID)
	w := shipRequest(t, s, stage.RunID, stage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	au.mu.Lock()
	var found bool
	for i := range au.appended {
		if au.appended[i].Category == "cost_recorded" {
			found = true
			var p struct {
				USD        float64 `json:"usd"`
				KnownModel bool    `json:"known_model"`
			}
			if err := json.Unmarshal(au.appended[i].Payload, &p); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			if p.USD != 0 || p.KnownModel {
				t.Errorf("unknown model: usd=%v known_model=%v, want 0/false", p.USD, p.KnownModel)
			}
		}
	}
	au.mu.Unlock()
	if !found {
		t.Fatal("no cost_recorded entry for unknown model — must record at 0, not drop")
	}

	got, _ := rr.GetRun(t.Context(), stage.RunID)
	if got.CostUSDTotal != 0 {
		t.Errorf("run.CostUSDTotal = %v, want 0 for unknown model", got.CostUSDTotal)
	}
}

// TestShipTrace_RecordsCost_NoUsageKnownUsageFalse pins the no-usage arm
// (#682): a manifest carrying a KNOWN, priced model but a 0/0 token split
// (a backend that didn't report usage) records a cost_recorded entry at
// usd=0 with known_usage=false rather than a silent $0 indistinguishable
// from a real tiny run, and the run total stays at 0. Using a known model
// isolates the new known_usage signal from the existing known_model one.
func TestShipTrace_RecordsCost_NoUsageKnownUsageFalse(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	rr := newApprovalRunRepo()
	stage := rr.seedStage(run.StageStateDispatched)
	rr.seedRun(&run.Run{ID: stage.RunID, Repo: "kuhlman-labs/fishhawk"})
	s.cfg.RunRepo = rr

	// Same priced model the happy path uses, but with no reported usage.
	const model = "claude-opus-4-8"
	bundleBytes := packManifestBundle(t, bundle.Manifest{
		BundleSchema: "trace-bundle-v0",
		RunID:        stage.RunID.String(),
		StageID:      stage.ID.String(),
		Agent:        "claude-code",
		Model:        model,
		InputTokens:  0,
		OutputTokens: 0,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
	})

	priv, _ := sf.issue(t, stage.RunID)
	w := shipRequest(t, s, stage.RunID, stage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	au.mu.Lock()
	var found bool
	for i := range au.appended {
		if au.appended[i].Category == "cost_recorded" {
			found = true
			var p struct {
				USD        float64 `json:"usd"`
				KnownModel bool    `json:"known_model"`
				KnownUsage bool    `json:"known_usage"`
			}
			if err := json.Unmarshal(au.appended[i].Payload, &p); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			if p.USD != 0 {
				t.Errorf("no-usage: usd=%v, want 0", p.USD)
			}
			if p.KnownUsage {
				t.Errorf("no-usage: known_usage=true, want false for a 0/0 token split")
			}
			if !p.KnownModel {
				t.Errorf("no-usage: known_model=false, want true — the model is priced")
			}
		}
	}
	au.mu.Unlock()
	if !found {
		t.Fatal("no cost_recorded entry for no-usage bundle — must record at 0, not drop")
	}

	got, _ := rr.GetRun(t.Context(), stage.RunID)
	if got.CostUSDTotal != 0 {
		t.Errorf("run.CostUSDTotal = %v, want 0 for no-usage bundle", got.CostUSDTotal)
	}
}

// seedCostEntry builds a cost_recorded audit entry at the given time
// carrying a usd figure — the history the spend-alert check reads to
// build its rolling baseline.
func seedCostEntry(t *testing.T, ts time.Time, usd float64) *audit.Entry {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"usd": usd})
	if err != nil {
		t.Fatalf("marshal seed payload: %v", err)
	}
	return &audit.Entry{Timestamp: ts, Category: "cost_recorded", Payload: payload}
}

// TestShipTrace_SpendAlertTrips is the wiring test for the spend-anomaly
// detector (#649). It seeds three prior hours of low spend, then POSTs a
// bundle whose priced cost far exceeds 3x that baseline, and asserts a
// spend_alert audit entry is written tying the spike to the run. Warn-
// only: the upload still returns 202.
func TestShipTrace_SpendAlertTrips(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	rr := newApprovalRunRepo()
	stage := rr.seedStage(run.StageStateDispatched)
	rr.seedRun(&run.Run{ID: stage.RunID})
	s.cfg.RunRepo = rr

	const model = "claude-opus-4-8"
	const inTok, outTok = 100000, 200000
	wantUSD, ok := pricing.Cost(model, inTok, outTok)
	if !ok || wantUSD <= 0 {
		t.Fatalf("pricing.Cost(%q) ok=%v usd=%v — fixture model must be priced", model, ok, wantUSD)
	}

	now := time.Now().UTC()
	au.seeded = []*audit.Entry{
		seedCostEntry(t, now.Add(-3*time.Hour), 0.01),
		seedCostEntry(t, now.Add(-2*time.Hour), 0.01),
		seedCostEntry(t, now.Add(-1*time.Hour), 0.01),
	}

	bundleBytes := packManifestBundle(t, bundle.Manifest{
		BundleSchema: "trace-bundle-v0",
		RunID:        stage.RunID.String(),
		StageID:      stage.ID.String(),
		Agent:        "claude-code",
		Model:        model,
		InputTokens:  inTok,
		OutputTokens: outTok,
	})

	priv, _ := sf.issue(t, stage.RunID)
	w := shipRequest(t, s, stage.RunID, stage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (warn-only must not gate):\n%s", w.Code, w.Body.String())
	}

	au.mu.Lock()
	var alert *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == "spend_alert" {
			alert = &au.appended[i]
			break
		}
	}
	au.mu.Unlock()
	if alert == nil {
		t.Fatal("no spend_alert audit entry written for an anomalous hour")
	}
	if alert.RunID != stage.RunID {
		t.Errorf("spend_alert RunID = %s, want %s", alert.RunID, stage.RunID)
	}
	var ap struct {
		LatestHourUSD float64 `json:"latest_hour_usd"`
		RollingAvgUSD float64 `json:"rolling_avg_usd"`
		PriorHours    int     `json:"prior_hours"`
		Multiple      float64 `json:"multiple"`
		Model         string  `json:"triggering_model"`
	}
	if err := json.Unmarshal(alert.Payload, &ap); err != nil {
		t.Fatalf("decode spend_alert payload: %v", err)
	}
	if ap.PriorHours != 3 {
		t.Errorf("spend_alert prior_hours = %d, want 3", ap.PriorHours)
	}
	if ap.LatestHourUSD < wantUSD {
		t.Errorf("spend_alert latest_hour_usd = %v, want >= %v", ap.LatestHourUSD, wantUSD)
	}
	if ap.Model != model {
		t.Errorf("spend_alert triggering_model = %q, want %q", ap.Model, model)
	}
}

// TestShipTrace_NoSpendAlertUnderSteadySpend confirms the detector stays
// quiet when the current hour matches the recent baseline: seeding prior
// hours at the same magnitude as the incoming bundle's cost yields no
// spend_alert entry.
func TestShipTrace_NoSpendAlertUnderSteadySpend(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	rr := newApprovalRunRepo()
	stage := rr.seedStage(run.StageStateDispatched)
	rr.seedRun(&run.Run{ID: stage.RunID})
	s.cfg.RunRepo = rr

	const model = "claude-opus-4-8"
	const inTok, outTok = 1000, 2000
	wantUSD, ok := pricing.Cost(model, inTok, outTok)
	if !ok {
		t.Fatalf("pricing.Cost(%q) ok=false — fixture model must be priced", model)
	}

	now := time.Now().UTC()
	au.seeded = []*audit.Entry{
		seedCostEntry(t, now.Add(-3*time.Hour), wantUSD),
		seedCostEntry(t, now.Add(-2*time.Hour), wantUSD),
		seedCostEntry(t, now.Add(-1*time.Hour), wantUSD),
	}

	bundleBytes := packManifestBundle(t, bundle.Manifest{
		BundleSchema: "trace-bundle-v0",
		RunID:        stage.RunID.String(),
		StageID:      stage.ID.String(),
		Agent:        "claude-code",
		Model:        model,
		InputTokens:  inTok,
		OutputTokens: outTok,
	})

	priv, _ := sf.issue(t, stage.RunID)
	w := shipRequest(t, s, stage.RunID, stage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	au.mu.Lock()
	for i := range au.appended {
		if au.appended[i].Category == "spend_alert" {
			t.Errorf("unexpected spend_alert under steady spend: %s", au.appended[i].Payload)
		}
	}
	au.mu.Unlock()
}
