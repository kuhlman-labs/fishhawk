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

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
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
	// listByCategoryErr, when set, makes ListForRunByCategory return an
	// error. The child-push idempotency guard (#776) reads the chain via
	// ListForRunByCategory and is fail-open on a read error (WARN + fall
	// through); this lets a test exercise that path.
	listByCategoryErr error
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

// ListForRun returns the seeded + appended entries for one run across all
// categories. Backs the issue-comment notifier's status-comment render
// (NotifyStatusUpdateForRun reads the full chain) when a test wires
// s.issueNotifier against this fake.
func (a *auditFake) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*audit.Entry
	for _, e := range a.seeded {
		if e.RunID != nil && *e.RunID == runID {
			out = append(out, e)
		}
	}
	for i := range a.appended {
		ap := a.appended[i]
		if ap.RunID != runID {
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
func (a *auditFake) LastForRun(_ context.Context, _ uuid.UUID) (*audit.Entry, error) {
	return nil, errors.New("auditFake: LastForRun not used")
}

// ListForRunByCategory returns the seeded + appended entries for one
// run filtered by category. Backs the issue-comment notifier's
// per-surface dedup (e.g. the advisory budget_alert per-period/per-tier
// guard, #688) when a test wires s.issueNotifier against this fake.
func (a *auditFake) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if a.listByCategoryErr != nil {
		return nil, a.listByCategoryErr
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*audit.Entry
	for _, e := range a.seeded {
		if e.RunID != nil && *e.RunID == runID && e.Category == category {
			out = append(out, e)
		}
	}
	for i := range a.appended {
		ap := a.appended[i]
		if ap.RunID != runID || ap.Category != category {
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

// runnerKindResolverFake embeds the package fakeRepo (so it satisfies the
// full run.Repository) and adds the optional ResolveRunnerKind capability
// (#1346 / ADR-045) the trace handler consumes via type assertion. It
// records the (runID, observed) it was called with and returns a canned
// resolution / error, so the handler-wiring tests assert that handleShipTrace
// extracts the manifest's runner_kind, calls the resolver, and emits the
// right reconciliation audit — without standing up Postgres (the real DB
// lock/mismatch semantics are covered exhaustively in run/postgres_test.go).
type runnerKindResolverFake struct {
	*fakeRepo
	called      int
	gotRunID    uuid.UUID
	gotObserved string
	result      run.RunnerKindResolution
	resolveErr  error
}

func (r *runnerKindResolverFake) ResolveRunnerKind(_ context.Context, runID uuid.UUID, observed string) (run.RunnerKindResolution, error) {
	r.called++
	r.gotRunID = runID
	r.gotObserved = observed
	if r.resolveErr != nil {
		return run.RunnerKindResolution{}, r.resolveErr
	}
	return r.result, nil
}

// makeRunnerKindBundle builds a minimal gzip JSONL bundle whose manifest
// carries the given runner_kind (empty omits the field, modelling a legacy
// bundle). It has no git_diff event, so the handler's policy re-eval takes
// the no-diff skip path and the stage advances normally.
func makeRunnerKindBundle(t *testing.T, runnerKind string) []byte {
	t.Helper()
	manifestData := `{"bundle_schema":"v1","agent_failed":false`
	if runnerKind != "" {
		manifestData += `,"runner_kind":"` + runnerKind + `"`
	}
	manifestData += `}`
	type line struct {
		Seq  int             `json:"seq"`
		TS   time.Time       `json:"ts"`
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data,omitempty"`
	}
	lines := []line{
		{Seq: 1, Kind: bundle.EventKindManifest, Data: json.RawMessage(manifestData)},
		{Seq: 2, Kind: "trailer", Data: json.RawMessage(`{}`)},
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

// findAppendedByCategory returns the single appended audit entry with the
// given category, failing if zero or more than one match.
func findAppendedByCategory(t *testing.T, au *auditFake, category string) audit.ChainAppendParams {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var matches []audit.ChainAppendParams
	for _, e := range au.appended {
		if e.Category == category {
			matches = append(matches, e)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("appended entries with category %q = %d, want 1", category, len(matches))
	}
	return matches[0]
}

func countAppendedByCategory(au *auditFake, category string) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for _, e := range au.appended {
		if e.Category == category {
			n++
		}
	}
	return n
}

// TestShipTrace_RunnerKind_ChangedLocksAndAudits asserts the load-bearing
// handler wiring for the #1344 fix: a bundle whose signed manifest reports
// runner_kind=local against a github_actions-default run drives
// ResolveRunnerKind with the observed value, the trace_uploaded audit is
// stamped with the LOCKED kind, and a runner_kind_resolved entry (from→to)
// is chained.
func TestShipTrace_RunnerKind_ChangedLocksAndAudits(t *testing.T) {
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()

	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr := &runnerKindResolverFake{
		fakeRepo: newFakeRepo(),
		result: run.RunnerKindResolution{
			Locked:   run.RunnerKindLocal,
			Changed:  true,
			Observed: run.RunnerKindLocal,
			Prior:    run.RunnerKindGitHubActions,
		},
	}
	rr.runs[runID] = &run.Run{ID: runID, Repo: "x/y", RunnerKind: run.RunnerKindGitHubActions, State: run.StatePending}

	s := New(Config{
		Addr:        "127.0.0.1:0",
		SigningRepo: sf,
		TraceStore:  ts,
		AuditRepo:   au,
		RunRepo:     rr,
	})

	body := makeRunnerKindBundle(t, run.RunnerKindLocal)
	w := shipRequest(t, s, runID, stageID, "raw", priv, body, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	// The resolver was driven with the manifest's observed channel.
	if rr.called != 1 || rr.gotRunID != runID || rr.gotObserved != run.RunnerKindLocal {
		t.Fatalf("ResolveRunnerKind called=%d runID=%s observed=%q, want 1/%s/local", rr.called, rr.gotRunID, rr.gotObserved, runID)
	}

	// trace_uploaded payload reflects the LOCKED kind, not the prior hint.
	traceEntry := findAppendedByCategory(t, au, "trace_uploaded")
	var tracePayload map[string]any
	if err := json.Unmarshal(traceEntry.Payload, &tracePayload); err != nil {
		t.Fatalf("decode trace_uploaded payload: %v", err)
	}
	if got, _ := tracePayload["runner_kind"].(string); got != run.RunnerKindLocal {
		t.Errorf("trace_uploaded payload.runner_kind = %q, want local", got)
	}

	// A runner_kind_resolved entry was chained with from→to.
	resEntry := findAppendedByCategory(t, au, "runner_kind_resolved")
	var resPayload map[string]any
	if err := json.Unmarshal(resEntry.Payload, &resPayload); err != nil {
		t.Fatalf("decode runner_kind_resolved payload: %v", err)
	}
	if from, _ := resPayload["from"].(string); from != run.RunnerKindGitHubActions {
		t.Errorf("runner_kind_resolved.from = %q, want github_actions", from)
	}
	if to, _ := resPayload["to"].(string); to != run.RunnerKindLocal {
		t.Errorf("runner_kind_resolved.to = %q, want local", to)
	}
	if n := countAppendedByCategory(au, "runner_kind_mismatch"); n != 0 {
		t.Errorf("runner_kind_mismatch entries = %d, want 0 on a Changed resolution", n)
	}
}

// TestShipTrace_RunnerKind_MismatchAudits asserts the post-execution
// guardrail wiring: when ResolveRunnerKind reports a Mismatch (a later report
// disagreeing with the already-locked kind), the handler emits a
// runner_kind_mismatch audit (declared/observed) and NO runner_kind_resolved.
func TestShipTrace_RunnerKind_MismatchAudits(t *testing.T) {
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()

	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr := &runnerKindResolverFake{
		fakeRepo: newFakeRepo(),
		result: run.RunnerKindResolution{
			Mismatch: true,
			Locked:   run.RunnerKindLocal,
			Observed: run.RunnerKindGitHubActions,
			Prior:    run.RunnerKindLocal,
		},
	}
	rr.runs[runID] = &run.Run{ID: runID, Repo: "x/y", RunnerKind: run.RunnerKindLocal, State: run.StatePending}

	s := New(Config{
		Addr:        "127.0.0.1:0",
		SigningRepo: sf,
		TraceStore:  ts,
		AuditRepo:   au,
		RunRepo:     rr,
	})

	body := makeRunnerKindBundle(t, run.RunnerKindGitHubActions)
	w := shipRequest(t, s, runID, stageID, "raw", priv, body, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	mismatch := findAppendedByCategory(t, au, "runner_kind_mismatch")
	var payload map[string]any
	if err := json.Unmarshal(mismatch.Payload, &payload); err != nil {
		t.Fatalf("decode runner_kind_mismatch payload: %v", err)
	}
	if declared, _ := payload["declared"].(string); declared != run.RunnerKindLocal {
		t.Errorf("runner_kind_mismatch.declared = %q, want local", declared)
	}
	if observed, _ := payload["observed"].(string); observed != run.RunnerKindGitHubActions {
		t.Errorf("runner_kind_mismatch.observed = %q, want github_actions", observed)
	}
	// The locked kind (not the rejected report) is stamped on trace_uploaded.
	traceEntry := findAppendedByCategory(t, au, "trace_uploaded")
	var tracePayload map[string]any
	if err := json.Unmarshal(traceEntry.Payload, &tracePayload); err != nil {
		t.Fatalf("decode trace_uploaded payload: %v", err)
	}
	if got, _ := tracePayload["runner_kind"].(string); got != run.RunnerKindLocal {
		t.Errorf("trace_uploaded payload.runner_kind = %q, want local (unchanged on mismatch)", got)
	}
	if n := countAppendedByCategory(au, "runner_kind_resolved"); n != 0 {
		t.Errorf("runner_kind_resolved entries = %d, want 0 on a Mismatch resolution", n)
	}
}

// TestShipTrace_RunnerKind_LegacyBundleSkipsReconcile asserts the back-compat
// path: a bundle whose manifest omits runner_kind (older runner) drives NO
// resolver call and emits neither reconciliation audit — the create-time hint
// stands and the trace_uploaded payload keeps the run's recorded kind.
func TestShipTrace_RunnerKind_LegacyBundleSkipsReconcile(t *testing.T) {
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()

	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr := &runnerKindResolverFake{fakeRepo: newFakeRepo()}
	rr.runs[runID] = &run.Run{ID: runID, Repo: "x/y", RunnerKind: run.RunnerKindGitHubActions, State: run.StatePending}

	s := New(Config{
		Addr:        "127.0.0.1:0",
		SigningRepo: sf,
		TraceStore:  ts,
		AuditRepo:   au,
		RunRepo:     rr,
	})

	body := makeRunnerKindBundle(t, "") // no runner_kind in manifest
	w := shipRequest(t, s, runID, stageID, "raw", priv, body, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	if rr.called != 0 {
		t.Errorf("ResolveRunnerKind called %d times on a legacy bundle, want 0", rr.called)
	}
	if n := countAppendedByCategory(au, "runner_kind_resolved") + countAppendedByCategory(au, "runner_kind_mismatch"); n != 0 {
		t.Errorf("reconciliation audit entries = %d on a legacy bundle, want 0", n)
	}
	// trace_uploaded keeps the run's recorded hint.
	traceEntry := findAppendedByCategory(t, au, "trace_uploaded")
	var tracePayload map[string]any
	if err := json.Unmarshal(traceEntry.Payload, &tracePayload); err != nil {
		t.Fatalf("decode trace_uploaded payload: %v", err)
	}
	if got, _ := tracePayload["runner_kind"].(string); got != run.RunnerKindGitHubActions {
		t.Errorf("trace_uploaded payload.runner_kind = %q, want github_actions (hint preserved)", got)
	}
}

// TestShipTrace_RunnerKind_ResolveErrorDegrades asserts the best-effort
// contract: when ResolveRunnerKind errors, the upload still succeeds (202),
// no reconciliation audit is emitted, and the trace_uploaded payload falls
// back to the run's recorded hint.
func TestShipTrace_RunnerKind_ResolveErrorDegrades(t *testing.T) {
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()

	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr := &runnerKindResolverFake{
		fakeRepo:   newFakeRepo(),
		resolveErr: errors.New("db down"),
	}
	rr.runs[runID] = &run.Run{ID: runID, Repo: "x/y", RunnerKind: run.RunnerKindGitHubActions, State: run.StatePending}

	s := New(Config{
		Addr:        "127.0.0.1:0",
		SigningRepo: sf,
		TraceStore:  ts,
		AuditRepo:   au,
		RunRepo:     rr,
	})

	body := makeRunnerKindBundle(t, run.RunnerKindLocal)
	w := shipRequest(t, s, runID, stageID, "raw", priv, body, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (best-effort never unwinds the upload):\n%s", w.Code, w.Body.String())
	}
	if n := countAppendedByCategory(au, "runner_kind_resolved") + countAppendedByCategory(au, "runner_kind_mismatch"); n != 0 {
		t.Errorf("reconciliation audit entries = %d on a resolver error, want 0", n)
	}
	traceEntry := findAppendedByCategory(t, au, "trace_uploaded")
	var tracePayload map[string]any
	if err := json.Unmarshal(traceEntry.Payload, &tracePayload); err != nil {
		t.Fatalf("decode trace_uploaded payload: %v", err)
	}
	if got, _ := tracePayload["runner_kind"].(string); got != run.RunnerKindGitHubActions {
		t.Errorf("trace_uploaded payload.runner_kind = %q, want github_actions (hint preserved on error)", got)
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

// TestShipTrace_StampsResolvedModel asserts the calibration-stamp side of the
// implement-model feature (#1013): the resolved model {value, source} is
// stamped onto the EXISTING runtime_observed AND cost_recorded kinds, and NO
// new audit kind (model_resolved) is emitted from this slice — the surface
// sweep stays clean. The resolved model here comes from the spec executor.model
// rung.
func TestShipTrace_StampsResolvedModel(t *testing.T) {
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()

	runRow := rr.seedRun()
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = []byte("workflows:\n" +
		"  feature_change:\n" +
		"    stages:\n" +
		"      - id: implement\n" +
		"        type: implement\n" +
		"        executor:\n" +
		"          model: claude-opus-4-8\n")

	planStage := rr.seedStage(runRow.ID, 0, run.StageStateSucceeded)
	seedPlanArtifactForRun(t, art, planStage.ID, 15)
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

	au.mu.Lock()
	defer au.mu.Unlock()
	var sawRO, sawCost bool
	for i := range au.appended {
		cat := au.appended[i].Category
		if cat == "model_resolved" {
			t.Fatalf("this slice must NOT emit the model_resolved kind (surface sweep must stay clean)")
		}
		var p map[string]any
		if err := json.Unmarshal(au.appended[i].Payload, &p); err != nil {
			continue
		}
		switch cat {
		case "runtime_observed":
			sawRO = true
			if p["resolved_model"] != "claude-opus-4-8" {
				t.Errorf("runtime_observed resolved_model = %v, want claude-opus-4-8", p["resolved_model"])
			}
			if p["resolved_model_source"] != "spec" {
				t.Errorf("runtime_observed resolved_model_source = %v, want spec", p["resolved_model_source"])
			}
		case "cost_recorded":
			sawCost = true
			if p["resolved_model"] != "claude-opus-4-8" {
				t.Errorf("cost_recorded resolved_model = %v, want claude-opus-4-8", p["resolved_model"])
			}
			if p["resolved_model_source"] != "spec" {
				t.Errorf("cost_recorded resolved_model_source = %v, want spec", p["resolved_model_source"])
			}
		}
	}
	if !sawRO {
		t.Fatal("no runtime_observed audit entry emitted")
	}
	if !sawCost {
		t.Fatal("no cost_recorded audit entry emitted")
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

// makePushPRBundle builds a gzip JSONL bundle whose manifest carries the
// given push_and_open_pr flag, a git_diff event with fileCount changed
// files (at t0), an agent_end event (at t1), and a trailer. The two
// intermediate events give ExtractTiming a (t0, t1) window for runtime
// calibration; the git_diff drives the trace handler's push-and-open-pr
// gate (#742), which only defers the terminal transition when the diff is
// non-empty.
func makePushPRBundle(t *testing.T, pushAndOpenPR bool, fileCount int, t0, t1 time.Time) []byte {
	t.Helper()
	type line struct {
		Seq  int             `json:"seq"`
		TS   time.Time       `json:"ts"`
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data,omitempty"`
	}
	mdata, err := json.Marshal(bundle.Manifest{BundleSchema: "v1", PushAndOpenPR: pushAndOpenPR})
	if err != nil {
		t.Fatal(err)
	}
	files := make([]map[string]string, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		files = append(files, map[string]string{"path": fmt.Sprintf("file%d.go", i), "status": "modified"})
	}
	diffData, err := json.Marshal(map[string]any{
		"kind": "git_diff", "base_ref": "main", "files": files, "num_files": fileCount,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := []line{
		{Seq: 1, TS: t0.Add(-time.Second), Kind: bundle.EventKindManifest, Data: mdata},
		{Seq: 2, TS: t0, Kind: bundle.EventKindGitDiff, Data: diffData},
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

// TestShipTrace_PushAndOpenPR_ImplementStaysRunning is the #742 forward
// gate: when an implement stage's bundle stamps push_and_open_pr AND
// carries a non-empty diff, the trace upload must leave the stage in
// `running` (the /pull-request upload drives the terminal transition) — it
// must NOT advance to awaiting_approval, or a later PR-open failure would
// strand the run at the review gate with a null PR. The runtime_observed
// calibration row must STILL fire (it can only be emitted at trace time,
// where the bundle timing lives) so the gate doesn't silently disable
// ADR-030 calibration.
func TestShipTrace_PushAndOpenPR_ImplementStaysRunning(t *testing.T) {
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()

	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateSucceeded)
	seedPlanArtifactForRun(t, art, planStage.ID, 15)

	implStage := rr.seedStage(runRow.ID, 1, run.StageStateDispatched)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	priv, _ := sf.issue(t, runRow.ID)
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(10 * time.Minute)
	bundleBytes := makePushPRBundle(t, true, 2, t0, t1)

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

	// The gate must leave the stage in running — NOT awaiting_approval.
	got, err := rr.GetStage(t.Context(), implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateRunning {
		t.Errorf("stage.State = %q, want %q (gate must defer the terminal transition to /pull-request)",
			got.State, run.StageStateRunning)
	}

	// runtime_observed must still fire (condition: the gate's early return
	// must not disable ADR-030 calibration).
	au.mu.Lock()
	defer au.mu.Unlock()
	var found bool
	for i := range au.appended {
		if au.appended[i].Category == "runtime_observed" {
			found = true
			var payload map[string]any
			if err := json.Unmarshal(au.appended[i].Payload, &payload); err != nil {
				t.Fatalf("decode runtime_observed payload: %v", err)
			}
			if got, _ := payload["outcome"].(string); got != "succeeded" {
				t.Errorf("runtime_observed outcome = %q, want succeeded", got)
			}
		}
	}
	if !found {
		t.Error("no runtime_observed audit entry emitted for gated push_and_open_pr implement stage")
	}
}

// TestShipTrace_PushAndOpenPR_EmptyDiffAdvances pins the no-changes carve-
// out: an implement bundle stamps push_and_open_pr but carries an EMPTY
// diff (the agent made no edits → no commit → no PR → the runner never
// POSTs /pull-request). Gating it would hang the stage in running, so the
// gate must NOT fire — the stage advances to awaiting_approval as before.
func TestShipTrace_PushAndOpenPR_EmptyDiffAdvances(t *testing.T) {
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()

	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateSucceeded)
	seedPlanArtifactForRun(t, art, planStage.ID, 15)

	implStage := rr.seedStage(runRow.ID, 1, run.StageStateDispatched)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	priv, _ := sf.issue(t, runRow.ID)
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(3 * time.Minute)
	bundleBytes := makePushPRBundle(t, true, 0, t0, t1) // 0 files → empty diff

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

	got, err := rr.GetStage(t.Context(), implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateAwaitingApproval {
		t.Errorf("stage.State = %q, want %q (empty-diff no-changes path must NOT be gated)",
			got.State, run.StageStateAwaitingApproval)
	}
}

// makeChildPushBundle builds a gzip JSONL bundle whose manifest carries the
// given push_to_shared_branch flag plus a git_diff with fileCount changed
// files — the decomposed-child analogue of makePushPRBundle (#771). It drives
// the trace handler's childPushGated check, which only defers the terminal
// transition when push_to_shared_branch is set AND the diff is non-empty.
func makeChildPushBundle(t *testing.T, pushToShared bool, fileCount int, t0, t1 time.Time) []byte {
	t.Helper()
	type line struct {
		Seq  int             `json:"seq"`
		TS   time.Time       `json:"ts"`
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data,omitempty"`
	}
	mdata, err := json.Marshal(bundle.Manifest{BundleSchema: "v1", PushToSharedBranch: pushToShared})
	if err != nil {
		t.Fatal(err)
	}
	files := make([]map[string]string, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		files = append(files, map[string]string{"path": fmt.Sprintf("file%d.go", i), "status": "modified"})
	}
	diffData, err := json.Marshal(map[string]any{
		"kind": "git_diff", "base_ref": "main", "files": files, "num_files": fileCount,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := []line{
		{Seq: 1, TS: t0.Add(-time.Second), Kind: bundle.EventKindManifest, Data: mdata},
		{Seq: 2, TS: t0, Kind: bundle.EventKindGitDiff, Data: diffData},
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

// TestChildPushGated is the true/false matrix for the #771 gate predicate.
func TestChildPushGated(t *testing.T) {
	s := &Server{}
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)

	if !s.childPushGated(makeChildPushBundle(t, true, 2, t0, t1)) {
		t.Error("flag set + non-empty diff → want gated (true)")
	}
	if s.childPushGated(makeChildPushBundle(t, true, 0, t0, t1)) {
		t.Error("flag set + empty diff → want NOT gated (false): no-changes child POSTs no report")
	}
	if s.childPushGated(makeChildPushBundle(t, false, 2, t0, t1)) {
		t.Error("flag unset + non-empty diff → want NOT gated (false): a standalone / older bundle")
	}
}

// TestShipTrace_ChildPush_ImplementStaysRunning is the #771 forward gate (the
// decomposition-child analogue of #742): a decomposed-child implement bundle
// stamps push_to_shared_branch AND carries a non-empty diff, so the trace
// upload must leave the stage in `running` (the /pull-request "pushed"/"failed"
// report drives the terminal transition) — it must NOT reach terminal
// succeeded, or a later push failure could not flip it to failed and the
// childcompletion sweeper would consolidate a child missing its code.
func TestShipTrace_ChildPush_ImplementStaysRunning(t *testing.T) {
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()

	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateSucceeded)
	seedPlanArtifactForRun(t, art, planStage.ID, 15)

	implStage := rr.seedStage(runRow.ID, 1, run.StageStateDispatched)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	priv, _ := sf.issue(t, runRow.ID)
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(10 * time.Minute)
	bundleBytes := makeChildPushBundle(t, true, 2, t0, t1)

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

	got, err := rr.GetStage(t.Context(), implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateRunning {
		t.Errorf("stage.State = %q, want %q (child-push gate must defer the terminal transition)",
			got.State, run.StageStateRunning)
	}
}

// TestShipTrace_ChildPush_EmptyDiffAdvances pins the no-changes carve-out for
// the child-push gate: an empty-diff decomposed child POSTs no /pull-request
// report, so gating it would hang the stage in running — the gate must NOT
// fire and the stage advances as before.
func TestShipTrace_ChildPush_EmptyDiffAdvances(t *testing.T) {
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()

	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateSucceeded)
	seedPlanArtifactForRun(t, art, planStage.ID, 15)

	implStage := rr.seedStage(runRow.ID, 1, run.StageStateDispatched)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	priv, _ := sf.issue(t, runRow.ID)
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(3 * time.Minute)
	bundleBytes := makeChildPushBundle(t, true, 0, t0, t1) // 0 files → empty diff

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

	got, err := rr.GetStage(t.Context(), implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateAwaitingApproval {
		t.Errorf("stage.State = %q, want %q (empty-diff child must NOT be gated)",
			got.State, run.StageStateAwaitingApproval)
	}
}

// makeFixupPushBundle builds a fix-up re-dispatch trace bundle: a manifest
// carrying push_fixup plus a git_diff event with fileCount files. It exercises
// the trace handler's fixupPushGated check (#794), which only defers the
// terminal transition when push_fixup is set AND the diff is non-empty.
func makeFixupPushBundle(t *testing.T, pushFixup bool, fileCount int, t0, t1 time.Time) []byte {
	t.Helper()
	type line struct {
		Seq  int             `json:"seq"`
		TS   time.Time       `json:"ts"`
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data,omitempty"`
	}
	mdata, err := json.Marshal(bundle.Manifest{BundleSchema: "v1", PushFixup: pushFixup})
	if err != nil {
		t.Fatal(err)
	}
	files := make([]map[string]string, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		files = append(files, map[string]string{"path": fmt.Sprintf("file%d.go", i), "status": "modified"})
	}
	diffData, err := json.Marshal(map[string]any{
		"kind": "git_diff", "base_ref": "main", "files": files, "num_files": fileCount,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := []line{
		{Seq: 1, TS: t0.Add(-time.Second), Kind: bundle.EventKindManifest, Data: mdata},
		{Seq: 2, TS: t0, Kind: bundle.EventKindGitDiff, Data: diffData},
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

// TestFixupPushGated is the true/false matrix for the #794 gate predicate.
func TestFixupPushGated(t *testing.T) {
	s := &Server{}
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)

	if !s.fixupPushGated(makeFixupPushBundle(t, true, 2, t0, t1)) {
		t.Error("flag set + non-empty diff → want gated (true)")
	}
	if s.fixupPushGated(makeFixupPushBundle(t, true, 0, t0, t1)) {
		t.Error("flag set + empty diff → want NOT gated (false): no-changes fix-up POSTs no report")
	}
	if s.fixupPushGated(makeFixupPushBundle(t, false, 2, t0, t1)) {
		t.Error("flag unset + non-empty diff → want NOT gated (false): a non-fix-up / older bundle")
	}
}

// TestShipTrace_FixupPush_ImplementStaysRunning is CONDITION 2 of #794: a
// fix-up re-dispatch implement bundle stamps push_fixup AND carries a non-empty
// diff, so the trace upload must leave the stage in `running` (the
// /pull-request "fixup_pushed"/"failed" report drives the terminal transition).
// It must NOT reach terminal succeeded immediately after the trace upload, or a
// later push failure could not flip it to failed and #788 recovery never fires,
// leaving the implement re-review to approve an unlanded diff.
func TestShipTrace_FixupPush_ImplementStaysRunning(t *testing.T) {
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()

	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateSucceeded)
	seedPlanArtifactForRun(t, art, planStage.ID, 15)

	implStage := rr.seedStage(runRow.ID, 1, run.StageStateDispatched)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	priv, _ := sf.issue(t, runRow.ID)
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(10 * time.Minute)
	bundleBytes := makeFixupPushBundle(t, true, 2, t0, t1)

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

	got, err := rr.GetStage(t.Context(), implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateRunning {
		t.Errorf("stage.State = %q, want %q (fix-up push gate must defer the terminal transition)",
			got.State, run.StageStateRunning)
	}
}

// TestShipTrace_FixupPush_EmptyDiffAdvances pins the no-changes carve-out for
// the fix-up gate: an empty-diff fix-up POSTs no /pull-request report, so gating
// it would hang the stage in running — the gate must NOT fire and the stage
// advances as before.
func TestShipTrace_FixupPush_EmptyDiffAdvances(t *testing.T) {
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()

	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateSucceeded)
	seedPlanArtifactForRun(t, art, planStage.ID, 15)

	implStage := rr.seedStage(runRow.ID, 1, run.StageStateDispatched)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	priv, _ := sf.issue(t, runRow.ID)
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(3 * time.Minute)
	bundleBytes := makeFixupPushBundle(t, true, 0, t0, t1) // 0 files → empty diff

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

	got, err := rr.GetStage(t.Context(), implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateAwaitingApproval {
		t.Errorf("stage.State = %q, want %q (empty-diff fix-up must NOT be gated)",
			got.State, run.StageStateAwaitingApproval)
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

// TestShipTrace_RecordsCacheAwareCost is the cross-boundary seam test for the
// agent-stage cache-aware cost rollup (ADR-044 / #1349): runner executor →
// signed manifest wire → cost.FromManifestWithCache → cost_recorded audit
// payload → sumRunTokens. It POSTs a trace bundle whose manifest carries the
// prompt-cache split (fresh input + cache read + cache write + output) and
// asserts end-to-end that:
//
//	(a) the cost_recorded usd is the cache-aware price — cache read at the
//	    family DISCOUNT and cache write at the PREMIUM, NOT the flat input
//	    rate (the falsifier),
//	(b) the payload carries cache_read_input_tokens / cache_write_input_tokens
//	    ADDITIVELY alongside the unchanged input_tokens / output_tokens, and
//	(c) sumRunTokens includes the cache buckets.
//
// A regression in the manifest decode, the CostWithCache call, the additive
// payload keys, or the sumRunTokens accumulation trips this even when the
// per-layer units still pass (cf. #618).
func TestShipTrace_RecordsCacheAwareCost(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	rr := newApprovalRunRepo()
	stage := rr.seedStage(run.StageStateDispatched)
	rr.seedRun(&run.Run{ID: stage.RunID, Repo: "kuhlman-labs/fishhawk"})
	s.cfg.RunRepo = rr

	const model = "claude-opus-4-8"
	const freshIn, cacheRead, cacheWrite, outTok = 1000, 4000, 2000, 2000
	wantUSD, ok := pricing.CostWithCache(model, freshIn, cacheRead, cacheWrite, outTok)
	if !ok {
		t.Fatalf("pricing.CostWithCache returned ok=false for %q", model)
	}
	// Falsifier baseline: pricing the cache portions at the flat input rate
	// would treat (fresh+read+write) all as input. The cache-aware total must
	// differ (read at discount, write at premium).
	flatUSD, _ := pricing.Cost(model, freshIn+cacheRead+cacheWrite, outTok)
	if wantUSD == flatUSD {
		t.Fatalf("test fixture is degenerate: cache-aware (%v) == flat-rate (%v)", wantUSD, flatUSD)
	}

	bundleBytes := packManifestBundle(t, bundle.Manifest{
		BundleSchema:          "trace-bundle-v0",
		RunID:                 stage.RunID.String(),
		StageID:               stage.ID.String(),
		Agent:                 "claude-code",
		Model:                 model,
		InputTokens:           freshIn,
		OutputTokens:          outTok,
		CacheReadInputTokens:  cacheRead,
		CacheWriteInputTokens: cacheWrite,
		GeneratedAt:           time.Now().UTC().Format(time.RFC3339),
	})

	priv, _ := sf.issue(t, stage.RunID)
	w := shipRequest(t, s, stage.RunID, stage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

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
	var payload struct {
		Model                 string  `json:"model"`
		InputTokens           int     `json:"input_tokens"`
		OutputTokens          int     `json:"output_tokens"`
		CacheReadInputTokens  int     `json:"cache_read_input_tokens"`
		CacheWriteInputTokens int     `json:"cache_write_input_tokens"`
		USD                   float64 `json:"usd"`
		KnownModel            bool    `json:"known_model"`
		KnownUsage            bool    `json:"known_usage"`
	}
	if err := json.Unmarshal(costEntry.Payload, &payload); err != nil {
		t.Fatalf("decode cost_recorded payload: %v", err)
	}
	// (a) cache-aware USD (read discount + write premium), not the flat rate.
	if payload.USD != wantUSD {
		t.Errorf("cost_recorded usd = %v, want %v (CostWithCache: read at discount, write at premium)", payload.USD, wantUSD)
	}
	if payload.USD == flatUSD {
		t.Errorf("cost_recorded usd = %v priced cache at the flat input rate; discount/premium not applied", payload.USD)
	}
	// (b) additive cache keys alongside the unchanged fresh-input/output keys.
	if payload.InputTokens != freshIn || payload.OutputTokens != outTok {
		t.Errorf("input/output = (%d,%d), want (%d,%d) — fresh input is cache-exclusive",
			payload.InputTokens, payload.OutputTokens, freshIn, outTok)
	}
	if payload.CacheReadInputTokens != cacheRead || payload.CacheWriteInputTokens != cacheWrite {
		t.Errorf("cache split = (read %d, write %d), want (%d, %d)",
			payload.CacheReadInputTokens, payload.CacheWriteInputTokens, cacheRead, cacheWrite)
	}
	if !payload.KnownUsage {
		t.Error("known_usage = false, want true for a non-zero cache-aware split")
	}

	// (c) sumRunTokens must include the cache buckets.
	gotTokens := s.sumRunTokens(t.Context(), stage.RunID)
	const wantTokens = freshIn + outTok + cacheRead + cacheWrite
	if gotTokens != wantTokens {
		t.Errorf("sumRunTokens = %d, want %d (fresh + output + cache read + cache write)", gotTokens, wantTokens)
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

// TestShipTrace_RecordsCost_CacheOnlyKnownUsageTrue pins the #1349 extension to
// the known_usage inference: a bundle with zero FRESH input and zero output but
// NON-zero cache tokens (a fully cache-served continuation) is real spend, so it
// must record known_usage=true at a non-zero cache-aware usd — NOT the
// known_usage=false / usd=0 the 0/0 path stamps. This is the defensive branch
// `cacheRead > 0 || cacheWrite > 0` added to the knownUsage guard.
func TestShipTrace_RecordsCost_CacheOnlyKnownUsageTrue(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	rr := newApprovalRunRepo()
	stage := rr.seedStage(run.StageStateDispatched)
	rr.seedRun(&run.Run{ID: stage.RunID, Repo: "kuhlman-labs/fishhawk"})
	s.cfg.RunRepo = rr

	const model = "claude-opus-4-8"
	const cacheRead = 5000
	wantUSD, ok := pricing.CostWithCache(model, 0, cacheRead, 0, 0)
	if !ok || wantUSD == 0 {
		t.Fatalf("fixture: CostWithCache(read only) ok=%v usd=%v, want ok=true and >0", ok, wantUSD)
	}
	bundleBytes := packManifestBundle(t, bundle.Manifest{
		BundleSchema:         "trace-bundle-v0",
		RunID:                stage.RunID.String(),
		StageID:              stage.ID.String(),
		Agent:                "claude-code",
		Model:                model,
		InputTokens:          0,
		OutputTokens:         0,
		CacheReadInputTokens: cacheRead,
		GeneratedAt:          time.Now().UTC().Format(time.RFC3339),
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
				KnownUsage bool    `json:"known_usage"`
			}
			if err := json.Unmarshal(au.appended[i].Payload, &p); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			if !p.KnownUsage {
				t.Error("cache-only: known_usage=false, want true — cache spend is real usage")
			}
			if p.USD != wantUSD {
				t.Errorf("cache-only: usd=%v, want %v (cache read priced, not zeroed)", p.USD, wantUSD)
			}
		}
	}
	au.mu.Unlock()
	if !found {
		t.Fatal("no cost_recorded entry for cache-only bundle")
	}
}

// spendTestNow is the fixed, wall-clock-independent instant the three
// rolling-hour spend tests pin s.nowFunc to. It is anchored mid-hour (:30) so
// the current-hour cluster (:30, :29, :28) and the exact -N-hour priors all
// truncate into clean, distinct hour buckets — never straddling a real hour
// edge regardless of when the suite runs. (A time.Time can't be a Go const, so
// this is a package-level var; it is the single shared instant reused by all
// three tests.)
var spendTestNow = time.Date(2026, 1, 2, 15, 30, 0, 0, time.UTC)

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

	// Pin the spend-alert clock to one fixed mid-hour instant so the seeded
	// priors and the recordCost-stamped current-hour sample share one
	// controlled now that never straddles a real hour edge.
	s.nowFunc = func() time.Time { return spendTestNow }
	now := spendTestNow
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

	// Pin the spend-alert clock to one fixed mid-hour instant so the seeded
	// priors and the recordCost-stamped current-hour sample share one
	// controlled now that never straddles a real hour edge.
	s.nowFunc = func() time.Time { return spendTestNow }
	now := spendTestNow
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

// TestShipTrace_RunBudgetTripwire_HaltsRun is the cross-boundary integration
// test for the per-run budget tripwire (ADR-030 / #653). Per the
// cross-boundary rule (#618/#624) it exercises config → trace handler →
// run-repo persistence → audit consumer together: it seeds a run whose
// accumulated cost sits just below a low configured per-run US$ ceiling,
// uploads a raw trace bundle whose manifest cost pushes the rolled total over
// the ceiling, and asserts:
//
//	(a) the run transitions to the cancelled terminal state (operator
//	    decision: a budget halt is a protective system stop, not a work
//	    failure — cancelled is non-retryable),
//	(b) a run_budget_exceeded audit entry is appended (system actor) naming
//	    the breached dimension (usd) and the figures, and
//	(c) NO further stage is dispatched — the stage stays in its pre-upload
//	    dispatched state because the handler short-circuits before
//	    advanceStageAfterTrace (the orchestrator-advance no-dispatch).
func TestShipTrace_RunBudgetTripwire_HaltsRun(t *testing.T) {
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()
	rr := newOrchestratorRepo()

	runRow := rr.seedRun() // StateRunning
	// Stage in dispatched — advanceStageAfterTrace would otherwise walk it to
	// running/awaiting_approval; the halt must prevent that.
	stage := rr.seedStage(runRow.ID, 0, run.StageStateDispatched)
	// A second pending stage so "no further stage dispatched" is observable:
	// it must still be pending after the halt.
	nextStage := rr.seedStage(runRow.ID, 1, run.StageStatePending)

	const model = "claude-opus-4-8"
	const inTok, outTok = 1000, 2000
	bundleUSD, ok := pricing.Cost(model, inTok, outTok)
	if !ok || bundleUSD <= 0 {
		t.Fatalf("pricing.Cost(%q) ok=%v usd=%v — fixture model must be priced", model, ok, bundleUSD)
	}
	// Seed cost just below the ceiling; the bundle's rolled cost pushes the
	// total to 1.5*bundleUSD, over the ceiling.
	runRow.CostUSDTotal = bundleUSD * 0.5
	ceiling := bundleUSD

	bundleBytes := packManifestBundle(t, bundle.Manifest{
		BundleSchema: "trace-bundle-v0",
		RunID:        runRow.ID.String(),
		StageID:      stage.ID.String(),
		Agent:        "claude-code",
		Model:        model,
		InputTokens:  inTok,
		OutputTokens: outTok,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
	})

	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		TraceStore:   ts,
		AuditRepo:    au,
		RunRepo:      rr,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
		MaxRunUSD:    ceiling,
	})

	priv, _ := sf.issue(t, runRow.ID)
	w := shipRequest(t, s, runRow.ID, stage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	// (a) run halted via the cancelled terminal state.
	got, err := rr.GetRun(t.Context(), runRow.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State != run.StateCancelled {
		t.Errorf("run.State = %q, want %q (budget halt is a cancel, not a fail)", got.State, run.StateCancelled)
	}

	// (b) run_budget_exceeded audit entry with the breached dimension + figures.
	au.mu.Lock()
	var be *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == "run_budget_exceeded" {
			be = &au.appended[i]
			break
		}
	}
	au.mu.Unlock()
	if be == nil {
		t.Fatal("no run_budget_exceeded audit entry written")
	}
	if be.RunID != runRow.ID {
		t.Errorf("run_budget_exceeded RunID = %s, want %s", be.RunID, runRow.ID)
	}
	if be.ActorKind == nil || *be.ActorKind != audit.ActorKind("system") {
		t.Errorf("run_budget_exceeded ActorKind = %v, want system", be.ActorKind)
	}
	var bp struct {
		Dimension     string  `json:"dimension"`
		CostUSDTotal  float64 `json:"cost_usd_total"`
		MaxRunUSD     float64 `json:"max_run_usd"`
		TerminalState string  `json:"terminal_state"`
	}
	if err := json.Unmarshal(be.Payload, &bp); err != nil {
		t.Fatalf("decode run_budget_exceeded payload: %v", err)
	}
	if bp.Dimension != "usd" {
		t.Errorf("dimension = %q, want usd", bp.Dimension)
	}
	if bp.MaxRunUSD != ceiling {
		t.Errorf("max_run_usd = %v, want %v", bp.MaxRunUSD, ceiling)
	}
	if bp.CostUSDTotal < ceiling {
		t.Errorf("cost_usd_total = %v, want >= ceiling %v", bp.CostUSDTotal, ceiling)
	}
	if bp.TerminalState != string(run.StateCancelled) {
		t.Errorf("terminal_state = %q, want cancelled", bp.TerminalState)
	}

	// (c) no further stage dispatched. The trace's stage stays dispatched
	// (handler short-circuited before advanceStageAfterTrace), and the next
	// stage stays pending (the orchestrator's Advance was never invoked).
	gotStage, err := rr.GetStage(t.Context(), stage.ID)
	if err != nil {
		t.Fatalf("GetStage(trace stage): %v", err)
	}
	if gotStage.State != run.StageStateDispatched {
		t.Errorf("trace stage.State = %q, want %q (no advance after halt)", gotStage.State, run.StageStateDispatched)
	}
	gotNext, err := rr.GetStage(t.Context(), nextStage.ID)
	if err != nil {
		t.Fatalf("GetStage(next stage): %v", err)
	}
	if gotNext.State != run.StageStatePending {
		t.Errorf("next stage.State = %q, want %q (no dispatch after halt)", gotNext.State, run.StageStatePending)
	}
}

// TestShipTrace_RunBudgetTripwire_UnderCeilingProceeds confirms the tripwire
// does NOT halt a run whose rolled cost stays under the ceiling: the stage
// advances normally and no run_budget_exceeded entry is written. This guards
// against a false-halt regression in the evaluator wiring.
func TestShipTrace_RunBudgetTripwire_UnderCeilingProceeds(t *testing.T) {
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()
	rr := newOrchestratorRepo()

	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStateDispatched)
	stage.RequiresApproval = true // plan-type gated; advances to awaiting_approval

	const model = "claude-opus-4-8"
	const inTok, outTok = 1000, 2000
	bundleUSD, ok := pricing.Cost(model, inTok, outTok)
	if !ok || bundleUSD <= 0 {
		t.Fatalf("pricing.Cost(%q) ok=%v usd=%v", model, ok, bundleUSD)
	}
	// Ceiling comfortably above the bundle's cost — no breach.
	ceiling := bundleUSD * 100

	bundleBytes := packManifestBundle(t, bundle.Manifest{
		BundleSchema: "trace-bundle-v0",
		RunID:        runRow.ID.String(),
		StageID:      stage.ID.String(),
		Agent:        "claude-code",
		Model:        model,
		InputTokens:  inTok,
		OutputTokens: outTok,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
	})

	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		TraceStore:   ts,
		AuditRepo:    au,
		RunRepo:      rr,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
		MaxRunUSD:    ceiling,
	})

	priv, _ := sf.issue(t, runRow.ID)
	w := shipRequest(t, s, runRow.ID, stage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	au.mu.Lock()
	for i := range au.appended {
		if au.appended[i].Category == "run_budget_exceeded" {
			t.Errorf("unexpected run_budget_exceeded under ceiling: %s", au.appended[i].Payload)
		}
	}
	au.mu.Unlock()

	got, err := rr.GetRun(t.Context(), runRow.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State == run.StateCancelled {
		t.Errorf("run cancelled under ceiling — tripwire false-halted")
	}
	gotStage, err := rr.GetStage(t.Context(), stage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if gotStage.State != run.StageStateAwaitingApproval {
		t.Errorf("stage.State = %q, want %q (normal advance under ceiling)", gotStage.State, run.StageStateAwaitingApproval)
	}
}

// implementBundleTwoGitDiffs builds an implement trace bundle carrying TWO
// git_diff events in emission order — a stale PRE-reconcile diff followed by the
// runner's reconciled scope-only re-emit (#870). It models the wire a verify-fix
// reinvoke produces, so the test can prove the backend reads the LAST event.
func implementBundleTwoGitDiffs(t *testing.T, staleFiles, reconciledFiles []map[string]string, stalePatch, reconciledPatch string) []byte {
	t.Helper()
	var raw bytes.Buffer
	writeLine := func(seq int, kind string, payload any) {
		data, _ := json.Marshal(payload)
		line, _ := json.Marshal(map[string]any{"seq": seq, "kind": kind, "data": json.RawMessage(data)})
		raw.Write(line)
		raw.WriteByte('\n')
	}
	writeLine(1, "manifest", map[string]any{"bundle_schema": "v1"})
	writeLine(2, "git_diff", map[string]any{
		"kind": "name_status", "base_ref": "origin/main",
		"files": staleFiles, "num_files": len(staleFiles), "patch": stalePatch,
	})
	writeLine(3, "git_diff", map[string]any{
		"kind": "name_status", "base_ref": "origin/main",
		"files": reconciledFiles, "num_files": len(reconciledFiles), "patch": reconciledPatch,
	})

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(raw.Bytes())
	_ = w.Close()
	return gz.Bytes()
}

// TestShipTrace_ImplementReview_ReconciledGitDiffWins is the #870 end-to-end
// seam test: a raw implement bundle carrying a PRE-reconcile git_diff followed
// by the runner's reconciled scope-only re-emit must drive the implement review
// off the LAST (reconciled) event. It closes the runner->bundle->ExtractDiff->
// review seam — proving last-write-wins ExtractDiff feeds the reviewer prompt
// the diff the PR actually ships, not the stale first one (#618: per-layer units
// miss this boundary).
func TestShipTrace_ImplementReview_ReconciledGitDiffWins(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, _, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	stalePatch := "diff --git a/backend/internal/foo/foo.go b/backend/internal/foo/foo.go\n" +
		"@@ -1,2 +1,2 @@\n-old impl\n+STALE pre-reconcile impl\n"
	reconciledPatch := "diff --git a/backend/internal/foo/foo.go b/backend/internal/foo/foo.go\n" +
		"@@ -1,2 +1,2 @@\n-old impl\n+RECONCILED scope-only impl\n"
	bundleBytes := implementBundleTwoGitDiffs(t,
		[]map[string]string{{"path": "backend/internal/foo/foo.go", "status": "M"}, {"path": "backend/internal/foo/stale.go", "status": "A"}},
		[]map[string]string{{"path": "backend/internal/foo/foo.go", "status": "M"}},
		stalePatch, reconciledPatch)

	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer invoked %d times, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	// The reconciled patch (last git_diff) must reach the reviewer; the stale
	// first patch and its drift-only file must NOT.
	if !strings.Contains(got, "+RECONCILED scope-only impl") {
		t.Errorf("reviewer prompt missing the reconciled patch (last git_diff):\n%s", got)
	}
	if strings.Contains(got, "STALE pre-reconcile impl") {
		t.Errorf("reviewer prompt carries the STALE first patch — last-write-wins broken:\n%s", got)
	}
	if strings.Contains(got, "backend/internal/foo/stale.go") {
		t.Errorf("reviewer prompt carries the stale file set, not the reconciled one:\n%s", got)
	}
}

// TestImplementReviewLoop_RepublishesAuditCompleteWhenReviewLands is the #947
// cross-boundary seam test spanning the auditcomplete domain rule, the server
// audit-repo + spec wiring (s.auditCompleteDeps), and the trace.go republish.
// It proves the seam end-to-end, not just the per-layer units:
//
//   - BEFORE: a configured agent implement-review (spec reviewers.agent=1) is
//     dispatched (implement_review_started) but no verdict has landed, so
//     auditcomplete.Compute returns StatePending with review_pending — the
//     pre-merge presence gate holds even though every OTHER rule passes.
//   - runImplementReviewLoop runs the reviewer, writes implement_reviewed, and
//     (the wiring under test) calls recomputeAndPublishAuditComplete.
//   - AFTER: Compute flips to StatePass, and the republished GitHub check run
//     carries a success conclusion — the required check clears with no operator
//     action once the advisory review lands.
func TestImplementReviewLoop_RepublishesAuditCompleteWhenReviewLands(t *testing.T) {
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	runRow.InstallationID = ptrInt64(99)
	runRow.Repo = "kuhlman-labs/example"
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = specImplementAdvisoryReviewers

	planStage := rr.seedStage(runRow.ID, 0, run.StageStateSucceeded)
	planStage.Type = run.StageTypePlan
	implStage := rr.seedStage(runRow.ID, 1, run.StageStateSucceeded)
	implStage.Type = run.StageTypeImplement
	rev := rr.seedStage(runRow.ID, 2, run.StageStateAwaitingApproval)
	rev.Type = run.StageTypeReview
	rev.Gate = &run.Gate{Kind: run.GateKindApproval}

	au := newAuditCompleteAuditFake()
	au.appendTrace(t, runRow.ID, planStage.ID, "raw")
	au.appendTrace(t, runRow.ID, planStage.ID, "redacted")
	au.appendTrace(t, runRow.ID, implStage.ID, "raw")
	au.appendTrace(t, runRow.ID, implStage.ID, "redacted")
	// Review dispatched, no terminal verdict yet — the only outstanding gap.
	// Use a recent timestamp so the dispatch is WITHIN the backstop bound (the
	// fixed-ts test helper would land it decades back and trip backstop-elapsed).
	if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runRow.ID,
		StageID:   &implStage.ID,
		Timestamp: time.Now().UTC(),
		Category:  "implement_review_started",
	}); err != nil {
		t.Fatalf("seed implement_review_started: %v", err)
	}

	arts := newFakeArtifactRepo()
	seedPlanArtifact(arts, planStage.ID)
	arts.all = append(arts.all, &artifact.Artifact{
		ID: uuid.New(), StageID: implStage.ID,
		Kind:    artifact.KindPullRequest,
		Content: pullRequestArtifactBody("abc12345"),
	})

	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	gh := newPublisherFakeGitHub()
	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr,
		AuditRepo: au, ArtifactRepo: arts,
		StageCheckRepo: newFakeStageCheckRepo(),
		PlanReviewer:   reviewer,
		ExternalURL:    "https://app.fishhawk.example.com",
	})
	s.auditCheckPublisher = auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, Runs: rr, Artifacts: arts,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	// BEFORE: the dispatched-but-unlanded review holds the audit pending.
	state, missing, err := auditcomplete.Compute(context.Background(), runRow.ID, s.auditCompleteDeps())
	if err != nil {
		t.Fatalf("Compute(before): %v", err)
	}
	if state != stagecheck.StatePending {
		t.Fatalf("before: state = %s want pending; missing=%+v", state, missing)
	}
	var hasReviewPending bool
	for _, m := range missing {
		if m.Kind == auditcomplete.MissingReviewPending {
			hasReviewPending = true
		}
	}
	if !hasReviewPending {
		t.Fatalf("before: missing did not include review_pending: %+v", missing)
	}

	// Exercise the review loop: writes implement_reviewed AND republishes.
	s.runImplementReviewLoop(context.Background(), runRow.ID, implStage.ID, 1,
		planreview.AuthorityAdvisory, "review prompt", "")

	// (a) an implement_reviewed terminal entry was written.
	reviewed, err := au.ListForRunByCategory(context.Background(), runRow.ID, "implement_reviewed")
	if err != nil {
		t.Fatalf("list implement_reviewed: %v", err)
	}
	if len(reviewed) != 1 {
		t.Fatalf("implement_reviewed entries = %d, want 1", len(reviewed))
	}

	// AFTER: the audit-complete state flips to pass once the review lands.
	state, missing, err = auditcomplete.Compute(context.Background(), runRow.ID, s.auditCompleteDeps())
	if err != nil {
		t.Fatalf("Compute(after): %v", err)
	}
	if state != stagecheck.StatePass {
		t.Fatalf("after: state = %s want pass; missing=%+v", state, missing)
	}

	// (b) recomputeAndPublishAuditComplete republished a PASSING check run.
	calls := gh.calls()
	if len(calls) == 0 {
		t.Fatal("expected a republished audit-complete check run after the review landed")
	}
	last := calls[len(calls)-1]
	if last.params.Conclusion != githubclient.CheckRunConclusionSuccess {
		t.Errorf("republished conclusion = %q want success", last.params.Conclusion)
	}
}

// TestFailStageCategoryC_DuplicateReport_DoesNotAdvanceRun reproduces the
// #968 incident sequence at the server layer: a fix-up re-dispatch failed,
// fix-up recovery restored the implement stage to succeeded and re-parked
// the review stage at awaiting_approval — then a DUPLICATE category-C
// failure report arrives for the same stage. FailStage rejects the
// transition (the stage is already terminal succeeded), and the handler
// must NOT fall through to advanceAfterFailure: with the review gate at
// awaiting_approval and nothing pending, the old fall-through routed the
// run to completeRun and stamped it succeeded while the human gate was
// still open (run 68e13183). The duplicate report must change nothing.
func TestFailStageCategoryC_DuplicateReport_DoesNotAdvanceRun(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditFake()

	runRow := rr.seedRun() // StateRunning
	implStage := rr.seedStage(runRow.ID, 1, run.StageStateSucceeded)
	reviewStage := rr.seedStage(runRow.ID, 2, run.StageStateAwaitingApproval)
	rr.mu.Lock()
	implStage.Type = run.StageTypeImplement
	reviewStage.Type = run.StageTypeReview
	rr.mu.Unlock()

	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})

	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runRow.ID.String()+"/trace", nil)
	s.failStageCategoryC(req, runRow.ID, implStage.ID,
		"no_diff_captured: duplicate report after fix-up recovery", nil)

	// The recovered implement stage stays succeeded.
	if cur, _ := rr.GetStage(context.Background(), implStage.ID); cur.State != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want unchanged (succeeded)", cur.State)
	}
	// The re-parked review gate stays open.
	if cur, _ := rr.GetStage(context.Background(), reviewStage.ID); cur.State != run.StageStateAwaitingApproval {
		t.Errorf("review state = %q, want unchanged (awaiting_approval)", cur.State)
	}
	// The run stays running at its gate — NOT stamped succeeded.
	got, err := rr.GetRun(context.Background(), runRow.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State != run.StateRunning {
		t.Errorf("run state = %q, want running (duplicate report must not complete the run)", got.State)
	}
}

// TestGateEvidenceForReview_MapsUndeclaredCategorized pins the
// bundle→prompt field mapping for the per-path drift categories (#991):
// every DriftPathEvidence crosses gateEvidenceForReview into a
// prompt.GateDriftPath verbatim, and a nil categorized slice stays nil
// (the older-bundle tolerance the render's byte-identity contract
// relies on).
// TestAmendedScopeFilesForReview_DoesNotSurfaceReasonProse is the #1225
// review-side regression guard. amendedScopeFilesForReview is the source for the
// implement-review prompt's "Scope amended at approval" section; it must derive
// the amended scope ONLY from the structured #824 add_scope_files fold, never
// from a repo-relative path scraped out of the operator's free-text approve
// comment (the removed #730 prose fold). This keeps the review side in lockstep
// with the stage side (#829): both now scope solely from the structured source,
// so a comment-named committed path is no longer flagged as scope drift by the
// reviewer while the stage no longer scopes it. The structured path IS still
// surfaced (proving only the prose source was removed), and the raw plan scope
// file is excluded (already rendered by writePlanForReview).
func TestAmendedScopeFilesForReview_DoesNotSurfaceReasonProse(t *testing.T) {
	runID := uuid.New()
	const plannedFile = "backend/internal/server/prompt.go"
	const structuredPath = "docs/api/v0.md"
	const reasonPath = "backend/go.mod"
	comment := "Approved. No edit to `" + reasonPath + "` is needed — it is already correct."

	s := New(Config{
		Addr: "127.0.0.1:0",
		AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
			runID: {makeApproveWithCommentAndScopeFilesEntry(runID, comment, []string{structuredPath})},
		}},
	})
	approvedPlan := &plan.Plan{
		Scope: plan.Scope{Files: []plan.ScopeFile{{Path: plannedFile, Operation: plan.FileOpModify}}},
	}

	got := s.amendedScopeFilesForReview(context.Background(), &run.Run{ID: runID}, approvedPlan)
	set := make(map[string]bool, len(got))
	for _, p := range got {
		set[p] = true
	}
	if !set[structuredPath] {
		t.Errorf("structured add_scope_files path %q must be surfaced as amended scope; got %#v", structuredPath, got)
	}
	if set[reasonPath] {
		t.Errorf("reason-prose path %q must NOT be surfaced as amended scope (#1225); got %#v", reasonPath, got)
	}
	if set[plannedFile] {
		t.Errorf("raw plan scope file %q must be excluded from amended scope; got %#v", plannedFile, got)
	}
}

func TestGateEvidenceForReview_MapsUndeclaredCategorized(t *testing.T) {
	staged := 2
	ev := bundle.GateEvidence{
		ScopeFacts: &bundle.ScopeFactsEvidence{
			DeclaredFiles:   3,
			StagedFiles:     &staged,
			UndeclaredPaths: []string{"stray/a.go", "stray/b.go"},
			UndeclaredCategorized: []bundle.DriftPathEvidence{
				{Path: "stray/a.go", Category: "A", Disposition: "excluded_from_commit"},
				{Path: "stray/b.go", Category: "B", Disposition: "would_fail_loud"},
			},
		},
	}
	got := gateEvidenceForReview(ev, nil)
	if got.ScopeFacts == nil {
		t.Fatal("ScopeFacts = nil, want populated")
	}
	want := []prompt.GateDriftPath{
		{Path: "stray/a.go", Category: "A", Disposition: "excluded_from_commit"},
		{Path: "stray/b.go", Category: "B", Disposition: "would_fail_loud"},
	}
	if len(got.ScopeFacts.UndeclaredCategorized) != len(want) {
		t.Fatalf("UndeclaredCategorized = %+v, want %+v", got.ScopeFacts.UndeclaredCategorized, want)
	}
	for i, w := range want {
		if got.ScopeFacts.UndeclaredCategorized[i] != w {
			t.Errorf("UndeclaredCategorized[%d] = %+v, want %+v", i, got.ScopeFacts.UndeclaredCategorized[i], w)
		}
	}

	ev.ScopeFacts.UndeclaredCategorized = nil
	if uncat := gateEvidenceForReview(ev, nil); uncat.ScopeFacts.UndeclaredCategorized != nil {
		t.Errorf("nil categorized input mapped to %+v, want nil",
			uncat.ScopeFacts.UndeclaredCategorized)
	}
}

func TestSubtractPaths(t *testing.T) {
	tests := []struct {
		name   string
		paths  []string
		remove []string
		want   []string
	}{
		{"nil remove returns input unchanged", []string{"a", "b"}, nil, []string{"a", "b"}},
		{"empty remove returns input unchanged", []string{"a", "b"}, []string{}, []string{"a", "b"}},
		{"nil paths yields nil", nil, []string{"a"}, nil},
		{"full overlap empties", []string{"a", "b"}, []string{"a", "b"}, []string{}},
		{"partial overlap drops only matched", []string{"a", "b", "c"}, []string{"b"}, []string{"a", "c"}},
		{"order preserved", []string{"z", "y", "x", "w"}, []string{"y"}, []string{"z", "x", "w"}},
		{"remove element absent from paths is a no-op", []string{"a"}, []string{"q"}, []string{"a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := subtractPaths(tc.paths, tc.remove)
			if len(got) != len(tc.want) {
				t.Fatalf("subtractPaths(%v, %v) = %v, want %v", tc.paths, tc.remove, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("index %d: got %q, want %q (full: %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

// TestGateEvidenceForReview_SubtractsFoldedPaths is the #1317 DONE-MEANS test
// for surface 2: a folded path is removed from BOTH ScopeFacts.UndeclaredPaths
// and ScopeFacts.UndeclaredCategorized, while a co-present NON-folded drift
// path is retained in both.
func TestGateEvidenceForReview_SubtractsFoldedPaths(t *testing.T) {
	ev := bundle.GateEvidence{
		ScopeFacts: &bundle.ScopeFactsEvidence{
			DeclaredFiles:   2,
			UndeclaredPaths: []string{"folded.go", "other.go"},
			UndeclaredCategorized: []bundle.DriftPathEvidence{
				{Path: "folded.go", Category: "A", Disposition: "excluded_from_commit"},
				{Path: "other.go", Category: "B", Disposition: "would_fail_loud"},
			},
		},
	}
	got := gateEvidenceForReview(ev, []string{"folded.go"})
	if got.ScopeFacts == nil {
		t.Fatal("ScopeFacts = nil, want populated")
	}
	if len(got.ScopeFacts.UndeclaredPaths) != 1 || got.ScopeFacts.UndeclaredPaths[0] != "other.go" {
		t.Errorf("UndeclaredPaths = %v, want [other.go] (folded.go subtracted)", got.ScopeFacts.UndeclaredPaths)
	}
	if len(got.ScopeFacts.UndeclaredCategorized) != 1 || got.ScopeFacts.UndeclaredCategorized[0].Path != "other.go" {
		t.Errorf("UndeclaredCategorized = %+v, want only other.go (folded.go subtracted)", got.ScopeFacts.UndeclaredCategorized)
	}
}

// buildDriftReconcileBundle packs a manifest, an optional scope_drift
// policy_event (undeclared paths), an optional scope_amendments_folded
// policy_event (added — pass a raw JSON string to model an unparseable
// payload), and an optional gate_evidence event whose ScopeFacts mirrors the
// drift, into the gzipped JSONL wire shape the trace handler reads. It models
// the runner's emission so the backend extraction+subtraction chain can be
// driven exactly as handleTraceUpload runs it (#1317).
func buildDriftReconcileBundle(t *testing.T, drift []string, foldedJSON string, gateScopeFacts map[string]any) []byte {
	t.Helper()
	type line struct {
		Seq  int             `json:"seq"`
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data,omitempty"`
	}
	mdata, err := json.Marshal(bundle.Manifest{BundleSchema: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	lines := []line{{Seq: 1, Kind: bundle.EventKindManifest, Data: mdata}}
	seq := 2
	if drift != nil {
		dp, derr := json.Marshal(map[string]any{"check": "scope_drift", "outcome": "excluded", "undeclared": drift})
		if derr != nil {
			t.Fatal(derr)
		}
		lines = append(lines, line{Seq: seq, Kind: bundle.EventKindPolicyEvent, Data: dp})
		seq++
	}
	if foldedJSON != "" {
		lines = append(lines, line{Seq: seq, Kind: bundle.EventKindPolicyEvent, Data: json.RawMessage(foldedJSON)})
		seq++
	}
	if gateScopeFacts != nil {
		gp, gerr := json.Marshal(map[string]any{"scope_facts": gateScopeFacts})
		if gerr != nil {
			t.Fatal(gerr)
		}
		lines = append(lines, line{Seq: seq, Kind: bundle.EventKindGateEvidence, Data: gp})
		seq++
	}
	lines = append(lines, line{Seq: seq, Kind: "trailer", Data: json.RawMessage(`{}`)})

	var raw bytes.Buffer
	for _, l := range lines {
		b, merr := json.Marshal(l)
		if merr != nil {
			t.Fatal(merr)
		}
		raw.Write(b)
		raw.WriteByte('\n')
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, werr := w.Write(raw.Bytes()); werr != nil {
		t.Fatal(werr)
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	return gz.Bytes()
}

// reconcileDriftSurfaces drives the exact extraction+subtraction chain the
// trace handler runs (#1317): ExtractScopeDrift / ExtractScopeAmendmentsFolded
// → subtractPaths over Trigger.ScopeDrift, and ExtractGateEvidence →
// gateEvidenceForReview over the gate-evidence ScopeFacts. It returns the
// reconciled drift list and the reconciled gate evidence so a test can assert
// BOTH review surfaces. On an ExtractScopeAmendmentsFolded error it mirrors the
// handler's WARN-degrade (folded = nil → no subtraction).
func reconcileDriftSurfaces(t *testing.T, bundleBytes []byte) ([]string, *prompt.GateEvidence) {
	t.Helper()
	scopeDrift, err := bundle.ExtractScopeDrift(bundleBytes)
	if err != nil {
		t.Fatalf("ExtractScopeDrift: %v", err)
	}
	folded, ferr := bundle.ExtractScopeAmendmentsFolded(bundleBytes)
	if ferr != nil {
		// Handler degrade: WARN + subtract nothing.
		folded = nil
	}
	scopeDrift = subtractPaths(scopeDrift, folded)
	var ge *prompt.GateEvidence
	if ev, geerr := bundle.ExtractGateEvidence(bundleBytes); geerr == nil {
		ge = gateEvidenceForReview(ev, folded)
	} else if !errors.Is(geerr, bundle.ErrNoGateEvidence) {
		t.Fatalf("ExtractGateEvidence: %v", geerr)
	}
	return scopeDrift, ge
}

func pathsContain(paths []string, p string) bool {
	for _, x := range paths {
		if x == p {
			return true
		}
	}
	return false
}

func categorizedContains(cat []prompt.GateDriftPath, p string) bool {
	for _, x := range cat {
		if x.Path == p {
			return true
		}
	}
	return false
}

// TestTraceUpload_FoldedPathSubtractedFromBothSurfaces is the #1317
// load-bearing cross-boundary test: it constructs a bundle in the runner's
// wire shape carrying a scope_amendments_folded event for path P alongside a
// scope_drift event {P, Q} and a gate_evidence event whose ScopeFacts also
// list {P, Q}, then drives the full backend extraction+subtraction chain and
// asserts P is removed from BOTH the #695 Trigger.ScopeDrift list and the
// gate-evidence ScopeFacts, while the co-present non-folded drift path Q is
// retained in both.
func TestTraceUpload_FoldedPathSubtractedFromBothSurfaces(t *testing.T) {
	bundleBytes := buildDriftReconcileBundle(t,
		[]string{"P.go", "Q.go"},
		`{"check":"scope_amendments_folded","added":["P.go"]}`,
		map[string]any{
			"declared_files":   2,
			"undeclared_paths": []string{"P.go", "Q.go"},
			"undeclared_categorized": []map[string]any{
				{"path": "P.go", "category": "A", "disposition": "excluded_from_commit"},
				{"path": "Q.go", "category": "B", "disposition": "would_fail_loud"},
			},
		},
	)
	scopeDrift, ge := reconcileDriftSurfaces(t, bundleBytes)

	// Surface 1: Trigger.ScopeDrift.
	if pathsContain(scopeDrift, "P.go") {
		t.Errorf("folded path P.go must be removed from Trigger.ScopeDrift; got %v", scopeDrift)
	}
	if !pathsContain(scopeDrift, "Q.go") {
		t.Errorf("non-folded drift path Q.go must be retained in Trigger.ScopeDrift; got %v", scopeDrift)
	}
	// Surface 2: gate-evidence ScopeFacts.
	if ge == nil || ge.ScopeFacts == nil {
		t.Fatal("gate evidence ScopeFacts = nil, want populated")
	}
	if pathsContain(ge.ScopeFacts.UndeclaredPaths, "P.go") {
		t.Errorf("folded P.go must be removed from ScopeFacts.UndeclaredPaths; got %v", ge.ScopeFacts.UndeclaredPaths)
	}
	if !pathsContain(ge.ScopeFacts.UndeclaredPaths, "Q.go") {
		t.Errorf("non-folded Q.go must be retained in ScopeFacts.UndeclaredPaths; got %v", ge.ScopeFacts.UndeclaredPaths)
	}
	if categorizedContains(ge.ScopeFacts.UndeclaredCategorized, "P.go") {
		t.Errorf("folded P.go must be removed from UndeclaredCategorized; got %+v", ge.ScopeFacts.UndeclaredCategorized)
	}
	if !categorizedContains(ge.ScopeFacts.UndeclaredCategorized, "Q.go") {
		t.Errorf("non-folded Q.go must be retained in UndeclaredCategorized; got %+v", ge.ScopeFacts.UndeclaredCategorized)
	}
}

// TestTraceUpload_ApprovedButUnfoldedPathPreserved is the #1317 binding
// correctness guard (operator condition): a path U that is approved/amended
// but NOT present in the scope_amendments_folded event's `added` set — because
// the runner never folded it (e.g. it was never edited/staged) — remains in
// BOTH drift surfaces. The subtract set is sourced ONLY from the per-commit
// fold record, so real drift is never hidden. Here F is the genuinely folded
// path and U is approved-but-unfolded; only F is subtracted.
func TestTraceUpload_ApprovedButUnfoldedPathPreserved(t *testing.T) {
	bundleBytes := buildDriftReconcileBundle(t,
		[]string{"U.go", "F.go"},
		`{"check":"scope_amendments_folded","added":["F.go"]}`,
		map[string]any{
			"declared_files":   2,
			"undeclared_paths": []string{"U.go", "F.go"},
			"undeclared_categorized": []map[string]any{
				{"path": "U.go", "category": "A", "disposition": "excluded_from_commit"},
				{"path": "F.go", "category": "A", "disposition": "excluded_from_commit"},
			},
		},
	)
	scopeDrift, ge := reconcileDriftSurfaces(t, bundleBytes)

	if !pathsContain(scopeDrift, "U.go") {
		t.Errorf("approved-but-unfolded U.go MUST remain real drift in Trigger.ScopeDrift; got %v", scopeDrift)
	}
	if pathsContain(scopeDrift, "F.go") {
		t.Errorf("folded F.go should be subtracted from Trigger.ScopeDrift; got %v", scopeDrift)
	}
	if ge == nil || ge.ScopeFacts == nil {
		t.Fatal("gate evidence ScopeFacts = nil, want populated")
	}
	if !pathsContain(ge.ScopeFacts.UndeclaredPaths, "U.go") {
		t.Errorf("approved-but-unfolded U.go MUST remain in ScopeFacts.UndeclaredPaths; got %v", ge.ScopeFacts.UndeclaredPaths)
	}
	if !categorizedContains(ge.ScopeFacts.UndeclaredCategorized, "U.go") {
		t.Errorf("approved-but-unfolded U.go MUST remain in UndeclaredCategorized; got %+v", ge.ScopeFacts.UndeclaredCategorized)
	}
	if pathsContain(ge.ScopeFacts.UndeclaredPaths, "F.go") {
		t.Errorf("folded F.go should be subtracted from ScopeFacts.UndeclaredPaths; got %v", ge.ScopeFacts.UndeclaredPaths)
	}
}

// TestTraceUpload_FoldedEventAbsentKeepsDrift covers the #1317 degrade branch
// where no scope_amendments_folded event is present (the ordinary
// no-amendment case): ExtractScopeAmendmentsFolded returns (nil, nil),
// subtractPaths is a no-op, and both surfaces keep the original drift — proving
// real drift is NOT hidden when there is no fold record.
func TestTraceUpload_FoldedEventAbsentKeepsDrift(t *testing.T) {
	bundleBytes := buildDriftReconcileBundle(t,
		[]string{"a.go", "b.go"},
		"", // no scope_amendments_folded event
		map[string]any{
			"declared_files":   1,
			"undeclared_paths": []string{"a.go", "b.go"},
		},
	)
	scopeDrift, ge := reconcileDriftSurfaces(t, bundleBytes)
	if !pathsContain(scopeDrift, "a.go") || !pathsContain(scopeDrift, "b.go") {
		t.Errorf("absent fold event must leave Trigger.ScopeDrift unchanged; got %v", scopeDrift)
	}
	if ge == nil || ge.ScopeFacts == nil {
		t.Fatal("gate evidence ScopeFacts = nil")
	}
	if !pathsContain(ge.ScopeFacts.UndeclaredPaths, "a.go") || !pathsContain(ge.ScopeFacts.UndeclaredPaths, "b.go") {
		t.Errorf("absent fold event must leave ScopeFacts.UndeclaredPaths unchanged; got %v", ge.ScopeFacts.UndeclaredPaths)
	}
}

// TestTraceUpload_FoldedEventUnparseableDegrades covers the #1317 degrade
// branch where the scope_amendments_folded payload is malformed: the extractor
// surfaces an error, the handler WARN-degrades to folded=nil → no subtraction,
// and the review still proceeds with the original drift intact (never blocks).
func TestTraceUpload_FoldedEventUnparseableDegrades(t *testing.T) {
	bundleBytes := buildDriftReconcileBundle(t,
		[]string{"a.go", "b.go"},
		`{"check":"scope_amendments_folded","added":"not-an-array"}`,
		map[string]any{
			"declared_files":   1,
			"undeclared_paths": []string{"a.go", "b.go"},
		},
	)
	// The extractor itself surfaces the parse error (the handler keys its
	// WARN-degrade off this).
	if _, err := bundle.ExtractScopeAmendmentsFolded(bundleBytes); err == nil {
		t.Fatal("ExtractScopeAmendmentsFolded: want a parse error on a malformed payload, got nil")
	}
	// The handler's degrade (folded=nil) leaves both surfaces unchanged and
	// never blocks the review.
	scopeDrift, ge := reconcileDriftSurfaces(t, bundleBytes)
	if !pathsContain(scopeDrift, "a.go") || !pathsContain(scopeDrift, "b.go") {
		t.Errorf("unparseable fold event must degrade to no subtraction on Trigger.ScopeDrift; got %v", scopeDrift)
	}
	if ge == nil || ge.ScopeFacts == nil {
		t.Fatal("gate evidence ScopeFacts = nil")
	}
	if !pathsContain(ge.ScopeFacts.UndeclaredPaths, "a.go") || !pathsContain(ge.ScopeFacts.UndeclaredPaths, "b.go") {
		t.Errorf("unparseable fold event must degrade to no subtraction on ScopeFacts; got %v", ge.ScopeFacts.UndeclaredPaths)
	}
}

// TestGateEvidenceForReview_PropagatesSuperseded pins the bundle→prompt
// mapping for the verify-run Superseded flag (#1205): the absorbed
// (non-terminal) run crosses gateEvidenceForReview into prompt.GateVerifyRun
// as Superseded=true and the terminal run as false, so the render can mark
// only the absorbed iteration and the reviewer never reads it as a
// committed-tree blocker.
func TestGateEvidenceForReview_PropagatesSuperseded(t *testing.T) {
	ev := bundle.GateEvidence{
		VerifyRuns: []bundle.VerifyRunEvidence{
			{Command: "scripts/test verify", ExitCode: 1, Outcome: "failed", Superseded: true},
			{Command: "scripts/test verify", ExitCode: 0, Outcome: "passed", Superseded: false},
		},
	}
	got := gateEvidenceForReview(ev, nil)
	if len(got.VerifyRuns) != 2 {
		t.Fatalf("VerifyRuns = %d, want 2", len(got.VerifyRuns))
	}
	if !got.VerifyRuns[0].Superseded {
		t.Error("VerifyRuns[0].Superseded = false, want true (absorbed run)")
	}
	if got.VerifyRuns[1].Superseded {
		t.Error("VerifyRuns[1].Superseded = true, want false (terminal run)")
	}
}

// TestGateEvidenceForReview_MapsScopeExemptions pins the bundle→prompt mapping
// for the scope self-exemptions (#1153): each validated path/reason crosses
// gateEvidenceForReview into prompt.GateScopeExemption so writeGateEvidence can
// render them for the reviewer.
func TestGateEvidenceForReview_MapsScopeExemptions(t *testing.T) {
	ev := bundle.GateEvidence{
		ScopeExemptions: []bundle.ScopeExemptionEvidence{
			{Path: "a.go", Reason: "already correct"},
			{Path: "b.go", Reason: "no change needed"},
		},
	}
	got := gateEvidenceForReview(ev, nil)
	want := []prompt.GateScopeExemption{
		{Path: "a.go", Reason: "already correct"},
		{Path: "b.go", Reason: "no change needed"},
	}
	if len(got.ScopeExemptions) != len(want) {
		t.Fatalf("ScopeExemptions = %+v, want %+v", got.ScopeExemptions, want)
	}
	for i, w := range want {
		if got.ScopeExemptions[i] != w {
			t.Errorf("ScopeExemptions[%d] = %+v, want %+v", i, got.ScopeExemptions[i], w)
		}
	}

	ev.ScopeExemptions = nil
	if none := gateEvidenceForReview(ev, nil); none.ScopeExemptions != nil {
		t.Errorf("nil exemptions input mapped to %+v, want nil", none.ScopeExemptions)
	}
}

// TestGateEvidenceForReview_MapsFixupSelfReportDivergence pins the bundle→prompt
// mapping for the advisory fix-up self-report divergence (#1210): the claimed/
// actual statuses cross gateEvidenceForReview into prompt.GateFixupSelfReportDivergence,
// and a nil input maps to nil out.
func TestGateEvidenceForReview_MapsFixupSelfReportDivergence(t *testing.T) {
	ev := bundle.GateEvidence{
		FixupSelfReportDivergence: &bundle.FixupSelfReportDivergenceEvidence{
			ClaimedVerifyStatus: "passed", ActualVerifyStatus: "failed",
		},
	}
	got := gateEvidenceForReview(ev, nil)
	if got.FixupSelfReportDivergence == nil {
		t.Fatal("FixupSelfReportDivergence mapped to nil, want populated")
	}
	want := prompt.GateFixupSelfReportDivergence{ClaimedVerifyStatus: "passed", ActualVerifyStatus: "failed"}
	if *got.FixupSelfReportDivergence != want {
		t.Errorf("FixupSelfReportDivergence = %+v, want %+v", *got.FixupSelfReportDivergence, want)
	}

	ev.FixupSelfReportDivergence = nil
	if none := gateEvidenceForReview(ev, nil); none.FixupSelfReportDivergence != nil {
		t.Errorf("nil divergence input mapped to %+v, want nil", none.FixupSelfReportDivergence)
	}
}

// TestFixupSelfReportDivergence_GateEvidence_EndToEnd is the #1210 cross-boundary
// integration test: a divergence flows across EVERY serialized seam — the runner's
// gate_evidence wire payload (the exact JSON composeGateEvidence emits, the
// fixup_selfreport_divergence tag and all) -> bundle.ExtractGateEvidence ->
// gateEvidenceForReview -> the writeGateEvidence-rendered implement-review text.
// A drift in any json tag or mapping along the chain breaks the rendered assertion.
func TestFixupSelfReportDivergence_GateEvidence_EndToEnd(t *testing.T) {
	gatePayload, err := json.Marshal(map[string]any{
		"scope_facts": map[string]any{"declared_files": 1},
		"fixup_selfreport_divergence": map[string]any{
			"claimed_verify_status": "passed",
			"actual_verify_status":  "failed",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mdata, err := json.Marshal(bundle.Manifest{BundleSchema: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	type line struct {
		Seq  int             `json:"seq"`
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data,omitempty"`
	}
	lines := []line{
		{Seq: 1, Kind: bundle.EventKindManifest, Data: mdata},
		{Seq: 2, Kind: bundle.EventKindGateEvidence, Data: gatePayload},
		{Seq: 3, Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
	var raw bytes.Buffer
	for _, l := range lines {
		b, merr := json.Marshal(l)
		if merr != nil {
			t.Fatal(merr)
		}
		raw.Write(b)
		raw.WriteByte('\n')
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, werr := w.Write(raw.Bytes()); werr != nil {
		t.Fatal(werr)
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	// Seam 1: runner wire JSON -> bundle.GateEvidence.
	ev, err := bundle.ExtractGateEvidence(gz.Bytes())
	if err != nil {
		t.Fatalf("ExtractGateEvidence: %v", err)
	}
	if ev.FixupSelfReportDivergence == nil || ev.FixupSelfReportDivergence.ClaimedVerifyStatus != "passed" {
		t.Fatalf("bundle.GateEvidence.FixupSelfReportDivergence = %+v", ev.FixupSelfReportDivergence)
	}
	// Seam 2: bundle.GateEvidence -> prompt.GateEvidence.
	pe := gateEvidenceForReview(ev, nil)
	// Seam 3: prompt.GateEvidence -> writeGateEvidence-rendered reviewer text.
	got, err := prompt.Build("implement_review", prompt.Trigger{
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: &plan.Plan{PlanVersion: "standard_v1", Summary: "selfreport-divergence e2e"},
		Diff:         "- M pkg/bar/bar.go\n",
		GateEvidence: pe,
	})
	if err != nil {
		t.Fatalf("prompt.Build: %v", err)
	}
	if !strings.Contains(got, "Fix-up self-report divergence") ||
		!strings.Contains(got, "CLAIMED the verify gate `passed`") ||
		!strings.Contains(got, "committed-tree verify gate `failed`") {
		t.Errorf("end-to-end divergence did not reach the rendered reviewer text:\n%s", got)
	}
}

// TestScopeExemption_GateEvidence_EndToEnd is the #1153 cross-boundary
// integration test (binding condition 3): a validated exemption set flows
// across EVERY serialized seam in one test — the runner's gate_evidence wire
// payload (the exact JSON composeGateEvidence emits, scope_exemptions tag and
// all) -> bundle.ExtractGateEvidence (bundle.GateEvidence) ->
// gateEvidenceForReview (prompt.GateEvidence) -> the writeGateEvidence-rendered
// implement-review text. A drift in any json tag or mapping along the chain
// breaks the rendered assertion, unlike the four independent per-layer tests.
func TestScopeExemption_GateEvidence_EndToEnd(t *testing.T) {
	// The runner's gate_evidence wire payload (composeGateEvidence output shape).
	gatePayload, err := json.Marshal(map[string]any{
		"scope_facts": map[string]any{"declared_files": 3},
		"scope_exemptions": []map[string]any{
			{"path": "pkg/foo/foo.go", "reason": "already correct after the helper change"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mdata, err := json.Marshal(bundle.Manifest{BundleSchema: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	type line struct {
		Seq  int             `json:"seq"`
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data,omitempty"`
	}
	lines := []line{
		{Seq: 1, Kind: bundle.EventKindManifest, Data: mdata},
		{Seq: 2, Kind: bundle.EventKindGateEvidence, Data: gatePayload},
		{Seq: 3, Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
	var raw bytes.Buffer
	for _, l := range lines {
		b, merr := json.Marshal(l)
		if merr != nil {
			t.Fatal(merr)
		}
		raw.Write(b)
		raw.WriteByte('\n')
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, werr := w.Write(raw.Bytes()); werr != nil {
		t.Fatal(werr)
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	// Seam 1: runner wire JSON -> bundle.GateEvidence.
	ev, err := bundle.ExtractGateEvidence(gz.Bytes())
	if err != nil {
		t.Fatalf("ExtractGateEvidence: %v", err)
	}
	if len(ev.ScopeExemptions) != 1 || ev.ScopeExemptions[0].Path != "pkg/foo/foo.go" {
		t.Fatalf("bundle.GateEvidence.ScopeExemptions = %+v", ev.ScopeExemptions)
	}
	// Seam 2: bundle.GateEvidence -> prompt.GateEvidence.
	pe := gateEvidenceForReview(ev, nil)
	// Seam 3: prompt.GateEvidence -> writeGateEvidence-rendered reviewer text.
	got, err := prompt.Build("implement_review", prompt.Trigger{
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: &plan.Plan{PlanVersion: "standard_v1", Summary: "scope-exempt e2e"},
		Diff:         "- M pkg/bar/bar.go\n",
		GateEvidence: pe,
	})
	if err != nil {
		t.Fatalf("prompt.Build: %v", err)
	}
	if !strings.Contains(got, "Self-exempted declared scope files") ||
		!strings.Contains(got, "pkg/foo/foo.go — already correct after the helper change") {
		t.Errorf("end-to-end exemption did not reach the rendered reviewer text:\n%s", got)
	}
}

// cannedComparePatchClient builds a githubclient.Client whose compare
// endpoint returns the given canned JSON, for the #1060 consolidated-review
// dispatch tests.
func cannedComparePatchClient(t *testing.T, body string) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

// seedConsolidatedParent wires a decomposed parent run with a succeeded
// plan stage (+ plan artifact), a succeeded implement stage, and a linked
// child run, plus the implement-review spec — the fixture the consolidated
// review dispatches against.
func seedConsolidatedParent(t *testing.T, rr *orchestratorRepo, art *fakeArtifactRepo, spec []byte) (*run.Run, *run.Stage) {
	t.Helper()
	runRow := rr.seedRun()
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = spec
	runRow.Repo = "kuhlman-labs/example"
	instID := int64(55)
	runRow.InstallationID = &instID

	planStage := rr.seedStage(runRow.ID, 0, run.StageStateSucceeded)
	seedBudgetPlanArtifact(t, art, planStage.ID, &plan.Plan{
		PlanVersion:                "standard_v1",
		Summary:                    "Decomposed parent",
		PredictedRuntimeMinutes:    10,
		PredictedRuntimeConfidence: plan.RuntimeConfidenceMedium,
		Scope:                      plan.Scope{Files: []plan.ScopeFile{{Path: "x.go", Operation: plan.FileOpModify}}},
	})

	implStage := rr.seedStage(runRow.ID, 1, run.StageStateSucceeded)
	implStage.Type = run.StageTypeImplement

	// Linked child so the parent passes the has-children gate.
	child := rr.seedRun()
	child.DecomposedFrom = &runRow.ID
	child.State = run.StateSucceeded

	return runRow, implStage
}

const cannedCompareOneFile = `{
	"total_commits": 1,
	"commits": [{"sha":"headsha1"}],
	"files": [{"filename":"x.go","status":"modified","changes":2,"patch":"@@ -1 +1 @@\n-a\n+b"}]
}`

func TestDispatchConsolidatedReview_AttachesConcernsToParentImplementStage(t *testing.T) {
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	au := newAuditFake()
	cr := newFakeConcernRepo()

	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict: planreview.VerdictReject,
			Concerns: []planreview.Concern{
				{Severity: planreview.SeverityHigh, Category: "correctness", Note: "child A introduced a nil deref the consolidated diff carries"},
			},
		},
		model: "claude-opus-4-8",
	}

	parent, implStage := seedConsolidatedParent(t, rr, art, specImplementGatingReviewers)

	s := New(Config{
		Addr:          "127.0.0.1:0",
		RunRepo:       rr,
		ArtifactRepo:  art,
		AuditRepo:     au,
		ConcernRepo:   cr,
		PlanReviewers: singleReviewerSet{reviewer},
		GitHub:        cannedComparePatchClient(t, cannedCompareOneFile),
	})

	s.DispatchConsolidatedReview(context.Background(), parent.ID, "main", "fishhawk/run-"+parent.ID.String()[:8])
	s.waitBackgroundReviews()

	// A round dispatched against the parent: started + reviewed entries.
	started, _ := au.ListForRunByCategory(context.Background(), parent.ID, "implement_review_started")
	if len(started) != 1 {
		t.Fatalf("implement_review_started entries = %d, want 1", len(started))
	}
	reviewed, _ := au.ListForRunByCategory(context.Background(), parent.ID, "implement_reviewed")
	if len(reviewed) != 1 {
		t.Fatalf("implement_reviewed entries = %d, want 1", len(reviewed))
	}

	// LOAD-BEARING: the concern attaches with StageID == the parent
	// implement stage that fixup_stage targets.
	rows, err := cr.ListByRun(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("persisted concerns = %d, want 1", len(rows))
	}
	if rows[0].StageID != implStage.ID {
		t.Errorf("concern StageID = %s, want parent implement stage %s", rows[0].StageID, implStage.ID)
	}
	if rows[0].StageKind != concern.StageKindImplement {
		t.Errorf("concern StageKind = %q, want implement", rows[0].StageKind)
	}
}

func TestDispatchConsolidatedReview_OrdinaryRun_NoDispatch(t *testing.T) {
	// A DecomposedFrom==nil run with NO children (an ordinary feature run)
	// must not get a consolidated review even when the orchestrator fires
	// the hook — its implement review already ran on the trace path.
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	au := newAuditFake()

	runRow := rr.seedRun()
	runRow.WorkflowSpec = specImplementGatingReviewers
	runRow.Repo = "kuhlman-labs/example"
	instID := int64(55)
	runRow.InstallationID = &instID
	impl := rr.seedStage(runRow.ID, 1, run.StageStateSucceeded)
	impl.Type = run.StageTypeImplement
	// No child runs seeded.

	s := New(Config{
		Addr:          "127.0.0.1:0",
		RunRepo:       rr,
		ArtifactRepo:  art,
		AuditRepo:     au,
		PlanReviewers: singleReviewerSet{&fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}}},
		GitHub:        cannedComparePatchClient(t, cannedCompareOneFile),
	})

	s.DispatchConsolidatedReview(context.Background(), runRow.ID, "main", "fishhawk/run-xxxx")
	s.waitBackgroundReviews()

	started, _ := au.ListForRunByCategory(context.Background(), runRow.ID, "implement_review_started")
	if len(started) != 0 {
		t.Errorf("implement_review_started entries = %d, want 0 for an ordinary run", len(started))
	}
}

func TestDispatchConsolidatedReview_TruncatedDiff_EmitsDegradationAndStillReviews(t *testing.T) {
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	au := newAuditFake()
	cr := newFakeConcernRepo()

	// A changed file whose patch body GitHub dropped → ComparePatch flags
	// truncation.
	truncatedBody := `{
		"total_commits": 1,
		"commits": [{"sha":"h"}],
		"files": [{"filename":"big.go","status":"modified","changes":99999,"patch":""}]
	}`

	parent, _ := seedConsolidatedParent(t, rr, art, specImplementGatingReviewers)

	s := New(Config{
		Addr:          "127.0.0.1:0",
		RunRepo:       rr,
		ArtifactRepo:  art,
		AuditRepo:     au,
		ConcernRepo:   cr,
		PlanReviewers: singleReviewerSet{&fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "m"}},
		GitHub:        cannedComparePatchClient(t, truncatedBody),
	})

	s.DispatchConsolidatedReview(context.Background(), parent.ID, "main", "fishhawk/run-"+parent.ID.String()[:8])
	s.waitBackgroundReviews()

	trunc, _ := au.ListForRunByCategory(context.Background(), parent.ID, consolidatedReviewTruncatedCategory)
	if len(trunc) != 1 {
		t.Fatalf("%s entries = %d, want 1", consolidatedReviewTruncatedCategory, len(trunc))
	}
	// The review still ran on the partial diff (degradation, not abort).
	started, _ := au.ListForRunByCategory(context.Background(), parent.ID, "implement_review_started")
	if len(started) != 1 {
		t.Errorf("implement_review_started entries = %d, want 1 (review still dispatched on partial diff)", len(started))
	}
}

// TestShipTrace_RunBudgetTripwire_FamilyAggregateHalts is the family-budget
// aggregation end-to-end (E24.6 / #1146): a decomposed CHILD whose own
// cost_usd_total stays under the per-run ceiling is still halted because the
// decomposition family (parent + child) aggregate is over. It drives the real
// trace-upload path so checkRunBudget runs over the summed family figure, not
// the child's own.
func TestShipTrace_RunBudgetTripwire_FamilyAggregateHalts(t *testing.T) {
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()
	rr := newOrchestratorRepo()

	// Decomposition family: a parent (root) and one child. The child is the
	// run uploading the trace.
	parent := rr.seedRun()
	child := rr.seedRun()
	child.DecomposedFrom = &parent.ID

	stage := rr.seedStage(child.ID, 0, run.StageStateDispatched)

	const model = "claude-opus-4-8"
	const inTok, outTok = 1000, 2000
	bundleUSD, ok := pricing.Cost(model, inTok, outTok)
	if !ok || bundleUSD <= 0 {
		t.Fatalf("pricing.Cost(%q) ok=%v usd=%v — fixture model must be priced", model, ok, bundleUSD)
	}
	// Ceiling sits ABOVE the child's own post-bundle cost (1x bundleUSD) but
	// BELOW the family aggregate (parent 2x + child 1x = 3x).
	ceiling := bundleUSD * 2.5
	parent.CostUSDTotal = bundleUSD * 2.0
	// child starts at 0; the bundle's rolled cost brings it to 1x bundleUSD —
	// still under the ceiling on its own, so only the family sum trips.

	bundleBytes := packManifestBundle(t, bundle.Manifest{
		BundleSchema: "trace-bundle-v0",
		RunID:        child.ID.String(),
		StageID:      stage.ID.String(),
		Agent:        "claude-code",
		Model:        model,
		InputTokens:  inTok,
		OutputTokens: outTok,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
	})

	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		TraceStore:   ts,
		AuditRepo:    au,
		RunRepo:      rr,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
		MaxRunUSD:    ceiling,
	})

	priv, _ := sf.issue(t, child.ID)
	w := shipRequest(t, s, child.ID, stage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	// Sanity: the child's OWN cost stayed under the ceiling — the halt is the
	// family aggregate, not the child alone.
	gotChild, err := rr.GetRun(t.Context(), child.ID)
	if err != nil {
		t.Fatalf("GetRun child: %v", err)
	}
	if gotChild.CostUSDTotal >= ceiling {
		t.Fatalf("child own cost = %v, want < ceiling %v (test must isolate family aggregation)", gotChild.CostUSDTotal, ceiling)
	}
	// The child run is halted (cancelled) by the family tripwire.
	if gotChild.State != run.StateCancelled {
		t.Errorf("child run.State = %q, want %q (family aggregate over ceiling halts)", gotChild.State, run.StateCancelled)
	}

	// run_budget_exceeded audit entry, with cost_usd_total reflecting the
	// summed family (>= ceiling), not the child's own figure.
	au.mu.Lock()
	var be *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == "run_budget_exceeded" {
			be = &au.appended[i]
			break
		}
	}
	au.mu.Unlock()
	if be == nil {
		t.Fatal("no run_budget_exceeded audit entry written for the family aggregate breach")
	}
	var bp struct {
		Dimension    string  `json:"dimension"`
		CostUSDTotal float64 `json:"cost_usd_total"`
	}
	if err := json.Unmarshal(be.Payload, &bp); err != nil {
		t.Fatalf("decode run_budget_exceeded payload: %v", err)
	}
	if bp.Dimension != "usd" {
		t.Errorf("dimension = %q, want usd", bp.Dimension)
	}
	if bp.CostUSDTotal < ceiling {
		t.Errorf("cost_usd_total = %v, want >= ceiling %v (must be the family sum)", bp.CostUSDTotal, ceiling)
	}
}

// TestShipTrace_RunBudgetTripwire_NonDecomposedFamilyIsSelf is the
// regression guard paired with the family-aggregate test (#1146): a
// NON-decomposed run with the SAME own cost as the halted child above
// (1x bundleUSD, under the 2.5x ceiling) must NOT trip, because its family
// is just itself — proving the aggregation reduces to the prior single-run
// figure for ordinary runs.
func TestShipTrace_RunBudgetTripwire_NonDecomposedFamilyIsSelf(t *testing.T) {
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()
	rr := newOrchestratorRepo()

	runRow := rr.seedRun() // DecomposedFrom nil, no children
	stage := rr.seedStage(runRow.ID, 0, run.StageStateDispatched)
	stage.RequiresApproval = true // advances to awaiting_approval on no-trip

	const model = "claude-opus-4-8"
	const inTok, outTok = 1000, 2000
	bundleUSD, ok := pricing.Cost(model, inTok, outTok)
	if !ok || bundleUSD <= 0 {
		t.Fatalf("pricing.Cost(%q) ok=%v usd=%v", model, ok, bundleUSD)
	}
	ceiling := bundleUSD * 2.5 // identical ceiling to the family test

	bundleBytes := packManifestBundle(t, bundle.Manifest{
		BundleSchema: "trace-bundle-v0",
		RunID:        runRow.ID.String(),
		StageID:      stage.ID.String(),
		Agent:        "claude-code",
		Model:        model,
		InputTokens:  inTok,
		OutputTokens: outTok,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
	})

	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		TraceStore:   ts,
		AuditRepo:    au,
		RunRepo:      rr,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
		MaxRunUSD:    ceiling,
	})

	priv, _ := sf.issue(t, runRow.ID)
	w := shipRequest(t, s, runRow.ID, stage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	got, err := rr.GetRun(t.Context(), runRow.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State == run.StateCancelled {
		t.Errorf("run.State = cancelled, want not-cancelled (family==self, own cost under ceiling must not trip)")
	}
	au.mu.Lock()
	for i := range au.appended {
		if au.appended[i].Category == "run_budget_exceeded" {
			t.Errorf("unexpected run_budget_exceeded entry for a single-run family under ceiling")
			break
		}
	}
	au.mu.Unlock()
}

// listErrRepo wraps orchestratorRepo to force a ListRuns error, exercising
// familyRuns' best-effort children-list degradation branch (#1146) without
// editing the shared approvals_test fixture.
type listErrRepo struct {
	*orchestratorRepo
	listErr error
}

func (r *listErrRepo) ListRuns(_ context.Context, _ run.ListRunsFilter) ([]*run.Run, error) {
	return nil, r.listErr
}

// TestFamilyRuns covers familyRuns' family assembly and its two best-effort
// degradation branches (#1146): a non-decomposed run is its own family; a
// parent's children are gathered with the root first; a parent-GetRun failure
// and a children-ListRuns failure both degrade to the single run.
func TestFamilyRuns(t *testing.T) {
	t.Run("non-decomposed run is its own family", func(t *testing.T) {
		rr := newOrchestratorRepo()
		runRow := rr.seedRun()
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})
		fam := s.familyRuns(t.Context(), runRow)
		if len(fam) != 1 || fam[0].ID != runRow.ID {
			t.Fatalf("family = %v, want exactly [self]", fam)
		}
	})

	t.Run("parent gathers root-first with children", func(t *testing.T) {
		rr := newOrchestratorRepo()
		parent := rr.seedRun()
		c1 := rr.seedRun()
		c1.DecomposedFrom = &parent.ID
		c2 := rr.seedRun()
		c2.DecomposedFrom = &parent.ID
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})

		fam := s.familyRuns(t.Context(), parent)
		if len(fam) != 3 {
			t.Fatalf("family size = %d, want 3 (parent + 2 children)", len(fam))
		}
		if fam[0].ID != parent.ID {
			t.Errorf("family[0] = %s, want root %s", fam[0].ID, parent.ID)
		}
		ids := map[uuid.UUID]bool{fam[1].ID: true, fam[2].ID: true}
		if !ids[c1.ID] || !ids[c2.ID] {
			t.Errorf("family children = %v, want {%s,%s}", ids, c1.ID, c2.ID)
		}
	})

	t.Run("child resolves the same family from a sibling", func(t *testing.T) {
		rr := newOrchestratorRepo()
		parent := rr.seedRun()
		child := rr.seedRun()
		child.DecomposedFrom = &parent.ID
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})

		fam := s.familyRuns(t.Context(), child)
		if len(fam) != 2 || fam[0].ID != parent.ID {
			t.Fatalf("family = %v, want [parent, child] with parent first", fam)
		}
	})

	t.Run("parent GetRun failure degrades to single run", func(t *testing.T) {
		rr := newOrchestratorRepo()
		child := rr.seedRun()
		missingParent := uuid.New() // never seeded → GetRun ErrNotFound
		child.DecomposedFrom = &missingParent
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})

		fam := s.familyRuns(t.Context(), child)
		if len(fam) != 1 || fam[0].ID != child.ID {
			t.Fatalf("family = %v, want [child] after parent lookup failure", fam)
		}
	})

	t.Run("children ListRuns failure degrades to single run", func(t *testing.T) {
		rr := newOrchestratorRepo()
		runRow := rr.seedRun()
		repo := &listErrRepo{orchestratorRepo: rr, listErr: errors.New("boom")}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})

		fam := s.familyRuns(t.Context(), runRow)
		if len(fam) != 1 || fam[0].ID != runRow.ID {
			t.Fatalf("family = %v, want [self] after children-list failure", fam)
		}
	})
}

// TestCheckSpendAlert_FamilyFanOutAggregates is the fan-out spend-alert
// assertion (#1146): cost_recorded entries spread across decomposition
// family members in the current hour aggregate into the detector's
// latest-hour sample, so a fan-out spike over the rolling baseline fires the
// spend_alert. checkSpendAlert reads the ledger across runs, so the whole
// family's spend is reflected without narrowing the cross-run baseline.
func TestCheckSpendAlert_FamilyFanOutAggregates(t *testing.T) {
	au := newAuditFake()
	rr := newOrchestratorRepo()

	parent := rr.seedRun()
	c1 := rr.seedRun()
	c1.DecomposedFrom = &parent.ID
	c2 := rr.seedRun()
	c2.DecomposedFrom = &parent.ID

	// Seed and evaluate against one fixed mid-hour instant (no real wall clock)
	// so the rolling-hour buckets are deterministic at any suite run time.
	now := spendTestNow
	// Three prior hours of low baseline spend.
	seeded := []*audit.Entry{
		seedCostEntry(t, now.Add(-3*time.Hour), 0.01),
		seedCostEntry(t, now.Add(-2*time.Hour), 0.01),
		seedCostEntry(t, now.Add(-1*time.Hour), 0.01),
	}
	// Current hour: spend spread across the three family members. Each member
	// is modest; the family SUM (0.15) is the spike the detector must see.
	for i, member := range []*run.Run{parent, c1, c2} {
		e := seedCostEntry(t, now.Add(-time.Duration(i)*time.Minute), 0.05)
		rid := member.ID
		e.RunID = &rid
		seeded = append(seeded, e)
	}
	au.seeded = seeded

	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})
	s.nowFunc = func() time.Time { return spendTestNow }

	s.checkSpendAlert(t.Context(), c1.ID, uuid.New(), "claude-opus-4-8")

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
		t.Fatal("no spend_alert for a family fan-out hour exceeding the baseline")
	}
	var ap struct {
		LatestHourUSD float64 `json:"latest_hour_usd"`
		PriorHours    int     `json:"prior_hours"`
	}
	if err := json.Unmarshal(alert.Payload, &ap); err != nil {
		t.Fatalf("decode spend_alert payload: %v", err)
	}
	// The latest-hour sample is the SUM across the family (0.15), not any one
	// member's 0.05 — proof the fan-out aggregates.
	if ap.LatestHourUSD < 0.15-1e-9 {
		t.Errorf("latest_hour_usd = %v, want >= 0.15 (sum across family members)", ap.LatestHourUSD)
	}
	if ap.PriorHours != 3 {
		t.Errorf("prior_hours = %d, want 3", ap.PriorHours)
	}
}

// --- Supplemental base-rebase re-invoke review (#1250) ---

// supplementalExemptions is the standard delta the #1250 tests dispatch with.
func supplementalExemptions() []prompt.GateScopeExemption {
	return []prompt.GateScopeExemption{
		{Path: "backend/internal/foo/foo.go", Reason: "already correct after the rebase"},
	}
}

// findSupplementalImplementReviewed returns the single implement_reviewed
// audit entry carrying Origin=base_rebase_reinvoke for the stage, or nil.
func findSupplementalImplementReviewed(t *testing.T, au *auditFake, stageID uuid.UUID) *planreview.ImplementReviewedPayload {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var got *planreview.ImplementReviewedPayload
	for i := range au.appended {
		ap := au.appended[i]
		if ap.Category != "implement_reviewed" || ap.StageID == nil || *ap.StageID != stageID {
			continue
		}
		var p planreview.ImplementReviewedPayload
		if err := json.Unmarshal(ap.Payload, &p); err != nil {
			t.Fatalf("decode implement_reviewed payload: %v", err)
		}
		if p.Origin != planreview.OriginBaseRebaseReinvoke {
			continue
		}
		if got != nil {
			t.Fatalf("more than one supplemental implement_reviewed entry for stage %s", stageID)
		}
		cp := p
		got = &cp
	}
	return got
}

// seedFirstReviewRound appends a first-review round (one implement_review_started
// + one origin-less implement_reviewed) for the stage so a test can assert the
// supplemental verdict counts ADDITIVELY without burying it (condition 2).
func seedFirstReviewRound(t *testing.T, au *auditFake, runID, stageID uuid.UUID) {
	t.Helper()
	system := audit.ActorKind("system")
	startedPayload, _ := json.Marshal(planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityAdvisory})
	if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: &stageID, Category: "implement_review_started", ActorKind: &system, Payload: startedPayload,
	}); err != nil {
		t.Fatalf("seed implement_review_started: %v", err)
	}
	reviewedPayload, _ := json.Marshal(planreview.ImplementReviewedPayload{
		ReviewerKind: "agent", Authority: planreview.AuthorityAdvisory, Verdict: planreview.VerdictApprove,
	})
	if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: &stageID, Category: "implement_reviewed", ActorKind: &system, Payload: reviewedPayload,
	}); err != nil {
		t.Fatalf("seed implement_reviewed: %v", err)
	}
}

// TestRunSupplementalReinvokeReview_AdvisoryAdditive_NoStarted is failure-mode
// (1) + binding condition 2: an advisory-authority supplemental review records
// exactly one ADDITIVE implement_reviewed (Origin=base_rebase_reinvoke + the
// re-landed head_sha), emits NO new implement_review_started (so the anchor
// floor stays at the first review and the first review's verdict is still
// counted), and returns false (advisory never gates).
func TestRunSupplementalReinvokeReview_AdvisoryAdditive_NoStarted(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, _, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)

	// A first review round already landed; the supplemental must not bury it.
	seedFirstReviewRound(t, au, runRow.ID, implStage.ID)

	const reHead = "abc123abc123abc123abc123abc123abc123abcd"
	reject := s.runSupplementalReinvokeReview(t.Context(), runRow.ID, implStage.ID, reHead, supplementalExemptions())
	if reject {
		t.Fatal("advisory supplemental review must return false (never gates)")
	}
	s.waitBackgroundReviews()

	// The floor is unchanged: still exactly one implement_review_started, so the
	// first review's verdict is still counted alongside the supplemental one.
	if n := countAuditCategory(au, "implement_review_started"); n != 1 {
		t.Errorf("implement_review_started = %d, want 1 (supplemental must NOT emit a fresh started — it would advance the anchor floor and bury the first review)", n)
	}
	// Two verdicts now: the first review's + the additive supplemental one.
	if n := countAuditCategory(au, "implement_reviewed"); n != 2 {
		t.Errorf("implement_reviewed = %d, want 2 (first review + additive supplemental)", n)
	}
	sup := findSupplementalImplementReviewed(t, au, implStage.ID)
	if sup == nil {
		t.Fatal("no supplemental implement_reviewed entry (Origin=base_rebase_reinvoke)")
	}
	if sup.HeadSHA != reHead {
		t.Errorf("supplemental HeadSHA = %q, want %q", sup.HeadSHA, reHead)
	}
}

// TestRunSupplementalReinvokeReview_GatingReject_ReturnsTrue is failure-mode
// (2): a gating-authority reject returns true (the caller fails the stage
// category-B), records the supplemental verdict with both provenance fields,
// and still emits NO implement_review_started.
func TestRunSupplementalReinvokeReview_GatingReject_ReturnsTrue(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictReject},
		model:   "gpt-5.5",
	}
	s, _, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)

	const reHead = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	reject := s.runSupplementalReinvokeReview(t.Context(), runRow.ID, implStage.ID, reHead, supplementalExemptions())
	if !reject {
		t.Fatal("gating supplemental reject must return true")
	}
	if n := countAuditCategory(au, "implement_review_started"); n != 0 {
		t.Errorf("implement_review_started = %d, want 0 (supplemental never emits started)", n)
	}
	sup := findSupplementalImplementReviewed(t, au, implStage.ID)
	if sup == nil {
		t.Fatal("no supplemental implement_reviewed entry")
	}
	if sup.Verdict != planreview.VerdictReject || sup.HeadSHA != reHead {
		t.Errorf("supplemental verdict=%q head=%q, want reject + %q", sup.Verdict, sup.HeadSHA, reHead)
	}
}

// TestRunSupplementalReinvokeReview_GatingApprove_ReturnsFalse is failure-mode
// (3): a gating-authority approve returns false (the caller advances the stage)
// and still records the additive supplemental verdict.
func TestRunSupplementalReinvokeReview_GatingApprove_ReturnsFalse(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "gpt-5.5",
	}
	s, _, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)

	if reject := s.runSupplementalReinvokeReview(t.Context(), runRow.ID, implStage.ID, "feed00dfeed00dfeed00dfeed00dfeed00dfeed0", supplementalExemptions()); reject {
		t.Fatal("gating supplemental approve must return false")
	}
	if findSupplementalImplementReviewed(t, au, implStage.ID) == nil {
		t.Fatal("no supplemental implement_reviewed entry recorded on approve")
	}
}

// TestRunSupplementalReinvokeReview_EmptyDelta_NoDispatch is failure-mode (4):
// an empty exemption delta dispatches NO review (the reviewer is never called
// and no implement_reviewed lands), returning false.
func TestRunSupplementalReinvokeReview_EmptyDelta_NoDispatch(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, _, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)

	if reject := s.runSupplementalReinvokeReview(t.Context(), runRow.ID, implStage.ID, "headsha", nil); reject {
		t.Fatal("empty delta must return false")
	}
	s.waitBackgroundReviews()
	reviewer.mu.Lock()
	calls := len(reviewer.calls)
	reviewer.mu.Unlock()
	if calls != 0 {
		t.Errorf("reviewer invoked %d times on an empty delta, want 0", calls)
	}
	if n := countAuditCategory(au, "implement_reviewed"); n != 0 {
		t.Errorf("implement_reviewed = %d on an empty delta, want 0", n)
	}
}

// TestRunSupplementalReinvokeReview_Idempotent_SerialRetry is failure-mode (5):
// a retried PR-upload with the SAME re-landed head_sha does NOT dispatch a
// second supplemental review — the (stage_id, Origin, head_sha) dedup finds the
// existing entry and skips. The dedup is best-effort for the detached advisory
// path, exercised here as the serial retry the runner actually performs (it
// drives a stage's PR-uploads serially), not a concurrency guarantee.
func TestRunSupplementalReinvokeReview_Idempotent_SerialRetry(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, _, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)

	const reHead = "1111111111111111111111111111111111111111"
	// First dispatch lands the supplemental verdict.
	s.runSupplementalReinvokeReview(t.Context(), runRow.ID, implStage.ID, reHead, supplementalExemptions())
	s.waitBackgroundReviews()
	if n := countAuditCategory(au, "implement_reviewed"); n != 1 {
		t.Fatalf("after first dispatch implement_reviewed = %d, want 1", n)
	}

	// Serial retry with the SAME head_sha must be a no-op.
	s.runSupplementalReinvokeReview(t.Context(), runRow.ID, implStage.ID, reHead, supplementalExemptions())
	s.waitBackgroundReviews()
	if n := countAuditCategory(au, "implement_reviewed"); n != 1 {
		t.Errorf("after serial retry implement_reviewed = %d, want 1 (dedup on stage_id+origin+head_sha)", n)
	}

	// A DIFFERENT re-landed head_sha is NOT deduped — it is a genuine new
	// re-invoke and dispatches its own supplemental review.
	s.runSupplementalReinvokeReview(t.Context(), runRow.ID, implStage.ID, "2222222222222222222222222222222222222222", supplementalExemptions())
	s.waitBackgroundReviews()
	if n := countAuditCategory(au, "implement_reviewed"); n != 2 {
		t.Errorf("after new-head dispatch implement_reviewed = %d, want 2", n)
	}
}

// TestRunSupplementalReinvokeReview_NoReviewerBackend_Skips covers the
// defaultPlanReviewer()==nil degradation guard: the spec declares an agent
// reviewer (so the AgentCount()>0 guard passes) but no reviewer backend is
// wired, so the dispatch is skipped quietly — no implement_reviewed lands and
// no review_skipped is double-recorded (the first-review path already emitted
// it). Returns false. This mirrors the first-review path's no-backend skip,
// which uses the same defaultPlanReviewer() helper.
func TestRunSupplementalReinvokeReview_NoReviewerBackend_Skips(t *testing.T) {
	// singleReviewerSet with a nil reviewer: Default() returns nil, so
	// defaultPlanReviewer() returns nil — the no-backend degradation.
	s, _, au, _, runRow, implStage := newImplementReviewServerWithSet(t, singleReviewerSet{nil}, specImplementAdvisoryReviewers)

	if reject := s.runSupplementalReinvokeReview(t.Context(), runRow.ID, implStage.ID, "3333333333333333333333333333333333333333", supplementalExemptions()); reject {
		t.Fatal("no reviewer backend must return false (skip, never gate)")
	}
	s.waitBackgroundReviews()
	if n := countAuditCategory(au, "implement_reviewed"); n != 0 {
		t.Errorf("implement_reviewed = %d with no reviewer backend, want 0 (dispatch skipped)", n)
	}
}
