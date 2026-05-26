package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// validPlanBytes returns a minimal standard_v1 plan that satisfies
// the schema. The same fixture is used across happy / idempotency
// tests so the content_hash matches.
func validPlanBytes(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(planfixture.Valid())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newPlanServer wires SigningRepo + ArtifactRepo + AuditRepo + RunRepo
// for the plan handler. Stages are seeded into the repo so GetStage
// returns the right RunID.
func newPlanServer(t *testing.T, runID, stageID uuid.UUID) (*Server, *signingFake, *fakeArtifactRepo, *auditFake, *promptRunRepo) {
	t.Helper()
	sf := newSigningFake()
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	rr := newPromptRunRepo()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		ArtifactRepo: ar,
		AuditRepo:    au,
		RunRepo:      rr,
	})
	return s, sf, ar, au, rr
}

// shipPlanRequest builds a POST /v0/runs/{run_id}/plan request signed
// by `priv`. Returns the recorded response.
func shipPlanRequest(t *testing.T, s *Server, runID, stageID uuid.UUID, priv ed25519.PrivateKey, body []byte, sigOverride string) *httptest.ResponseRecorder {
	t.Helper()
	url := fmt.Sprintf("/v0/runs/%s/plan?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
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

func TestShipPlan_HappyPath(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newPlanServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	var resp planResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SchemaVersion != "standard_v1" {
		t.Errorf("schema_version = %q", resp.SchemaVersion)
	}
	if resp.StageID != stageID {
		t.Errorf("stage_id = %s, want %s", resp.StageID, stageID)
	}
	if resp.Idempotent {
		t.Error("first upload should not be marked idempotent")
	}

	// One artifact row.
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1", len(ar.all))
	}

	// One audit entry, category plan_generated.
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	if got := au.appended[0].Category; got != "plan_generated" {
		t.Errorf("audit category = %q, want plan_generated", got)
	}
}

func TestShipPlan_Idempotent_SecondUploadReturnsExisting(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newPlanServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)

	// First upload.
	w1 := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w1.Code != http.StatusCreated {
		t.Fatalf("first upload status = %d", w1.Code)
	}

	// Second upload of identical bytes.
	w2 := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w2.Code != http.StatusOK {
		t.Fatalf("second upload status = %d, want 200:\n%s", w2.Code, w2.Body.String())
	}
	var resp planResponse
	_ = json.NewDecoder(w2.Body).Decode(&resp)
	if !resp.Idempotent {
		t.Error("second upload should be marked idempotent=true")
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1 (no duplicate row)", len(ar.all))
	}
	// No second audit entry — plan_generated fires once per artifact.
	if len(au.appended) != 1 {
		t.Errorf("audit entries = %d, want 1 (no second plan_generated)", len(au.appended))
	}
}

func TestShipPlan_SchemaInvalid_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, rr := newPlanServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := []byte(`{"plan_version":"standard_v1"}`) // missing required fields

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "plan_invalid") {
		t.Errorf("body missing plan_invalid code:\n%s", w.Body.String())
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0 on schema fail", len(ar.all))
	}

	// #527: when the plan body fails standard_v1 validation, the
	// plan handler must transition the stage to failed-B so the run
	// doesn't get stuck in awaiting_approval with no valid plan
	// attached. The trace handler advances the stage to
	// awaiting_approval first (trace arrives before plan); this
	// handler corrects course on the failure path.
	if len(rr.transitionStageCalls) != 1 {
		t.Fatalf("transitionStage calls = %d, want 1:\n%+v",
			len(rr.transitionStageCalls), rr.transitionStageCalls)
	}
	call := rr.transitionStageCalls[0]
	if call.StageID != stageID {
		t.Errorf("transitioned stage = %s, want %s", call.StageID, stageID)
	}
	if call.To != run.StageStateFailed {
		t.Errorf("transition.To = %q, want failed", call.To)
	}
	if call.Completion == nil {
		t.Fatal("transition.Completion is nil; failed transitions require StageCompletion")
	}
	if call.Completion.FailureCategory == nil || *call.Completion.FailureCategory != run.FailureB {
		t.Errorf("FailureCategory = %v, want B", call.Completion.FailureCategory)
	}
	if call.Completion.FailureReason == nil || !strings.HasPrefix(*call.Completion.FailureReason, "plan_invalid:") {
		t.Errorf("FailureReason = %v, want prefix 'plan_invalid:'", call.Completion.FailureReason)
	}
}

func TestShipPlan_SignatureMissing_401(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, _, _, _ := newPlanServer(t, runID, stageID)
	body := validPlanBytes(t)

	url := fmt.Sprintf("/v0/runs/%s/plan?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestShipPlan_SignatureInvalid_401(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, _ := newPlanServer(t, runID, stageID)
	sf.issue(t, runID) // server has the key, we sign with a different one
	body := validPlanBytes(t)

	// Sign with a wrong key.
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	w := shipPlanRequest(t, s, runID, stageID, otherPriv, body, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
}

func TestShipPlan_StageMismatch_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, rr := newPlanServer(t, runID, stageID)

	// Re-seed the stage so it points at a *different* run.
	otherRun := uuid.New()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: otherRun}
	priv, _ := sf.issue(t, runID)

	w := shipPlanRequest(t, s, runID, stageID, priv, validPlanBytes(t), "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (stage doesn't belong to run)", w.Code)
	}
}

func TestShipPlan_StageNotFound_404(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, rr := newPlanServer(t, runID, stageID)
	delete(rr.getStages, stageID)
	priv, _ := sf.issue(t, runID)

	w := shipPlanRequest(t, s, runID, stageID, priv, validPlanBytes(t), "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
}

func TestShipPlan_BodyTooLarge_413(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, _ := newPlanServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)

	// 257KB body — exceeds the 256KB cap, can't be valid JSON of
	// course but we expect the size check to fail before the
	// signature is verified anyway.
	body := bytes.Repeat([]byte("x"), maxPlanBundleBytes+1)
	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

func TestShipPlan_Unconfigured_503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	url := fmt.Sprintf("/v0/runs/%s/plan?stage_id=%s", uuid.New(), uuid.New())
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}
