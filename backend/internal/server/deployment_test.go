package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
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

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/deployreconciler"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// validDeploymentBytes returns a complete deploymentBody payload (ADR-038
// fields) that satisfies the handler's structural validation.
func validDeploymentBytes(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(deploymentBody{
		Environment:    "production",
		Ref:            "1111111111111111111111111111111111111111",
		ExternalRunURL: "https://github.com/kuhlman-labs/fishhawk/actions/runs/42",
		Outcome:        "succeeded",
		RollbackHandle: "deploy-42",
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// newDeploymentServer wires the deployment handler against the same fakes the
// pull-request handler tests use (the package compiles all *_test.go together,
// so these helpers are shared without per-test redefinition).
func newDeploymentServer(t *testing.T, runID, stageID uuid.UUID) (*Server, *signingFake, *fakeArtifactRepo, *auditFake, *promptRunRepo) {
	t.Helper()
	sf := newSigningFake()
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	rr := newPromptRunRepo()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeDeploy}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		ArtifactRepo: ar,
		AuditRepo:    au,
		RunRepo:      rr,
	})
	return s, sf, ar, au, rr
}

func shipDeploymentRequest(t *testing.T, s *Server, runID, stageID uuid.UUID, priv ed25519.PrivateKey, body []byte, sigOverride string) *httptest.ResponseRecorder {
	t.Helper()
	url := fmt.Sprintf("/v0/runs/%s/deployment?stage_id=%s", runID, stageID)
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

func countByCategory(au *auditFake, category string) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	var n int
	for _, e := range au.appended {
		if e.Category == category {
			n++
		}
	}
	return n
}

// TestShipDeployment_HappyPath crosses the handler -> persistence -> audit
// seam: a signed deployment record persists a KindDeployment artifact (readable
// back through the artifact repo with the content hash matching), and writes a
// single deployment_outcome_recorded chained audit entry carrying the ADR-038
// fields.
func TestShipDeployment_HappyPath(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newDeploymentServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validDeploymentBytes(t)

	w := shipDeploymentRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	var resp deploymentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Environment != "production" || resp.Outcome != "succeeded" {
		t.Errorf("response not populated: %+v", resp)
	}
	if resp.Idempotent {
		t.Error("first upload should not be marked idempotent")
	}

	// Artifact persisted with the deployment kind and readable back via the repo.
	if len(ar.all) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(ar.all))
	}
	got, err := ar.Get(context.Background(), resp.ID)
	if err != nil {
		t.Fatalf("read back artifact: %v", err)
	}
	if got.Kind != artifact.KindDeployment {
		t.Errorf("artifact Kind = %q, want deployment", got.Kind)
	}
	if got.ContentHash != sha256Hex(body) {
		t.Errorf("content hash = %q, want %q", got.ContentHash, sha256Hex(body))
	}

	// Exactly one deployment_outcome_recorded audit entry, with the ADR-038
	// fields pinned into the payload.
	if n := countByCategory(au, "deployment_outcome_recorded"); n != 1 {
		t.Fatalf("deployment_outcome_recorded entries = %d, want 1", n)
	}
	payload := string(au.appended[0].Payload)
	for _, want := range []string{`"environment":"production"`, `"outcome":"succeeded"`, `"auth_method":"ed25519"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("audit payload missing %s: %s", want, payload)
		}
	}
	// No rollback entry on a plain outcome report.
	if n := countByCategory(au, "deployment_rollback_initiated") + countByCategory(au, "deployment_rollback_completed"); n != 0 {
		t.Errorf("unexpected rollback audit entries = %d, want 0", n)
	}
}

// TestShipDeployment_Idempotent_SecondUpload pins the (stage_id, content_hash)
// dedup: a re-delivery of the identical record returns the existing artifact
// (idempotent=true) and writes no second artifact or audit row.
func TestShipDeployment_Idempotent_SecondUpload(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newDeploymentServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validDeploymentBytes(t)

	if w := shipDeploymentRequest(t, s, runID, stageID, priv, body, ""); w.Code != http.StatusCreated {
		t.Fatalf("first upload status = %d", w.Code)
	}
	w2 := shipDeploymentRequest(t, s, runID, stageID, priv, body, "")
	if w2.Code != http.StatusOK {
		t.Fatalf("second upload status = %d, want 200", w2.Code)
	}
	var resp deploymentResponse
	_ = json.NewDecoder(w2.Body).Decode(&resp)
	if !resp.Idempotent {
		t.Error("second upload should be marked idempotent=true")
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1 (no duplicate row)", len(ar.all))
	}
	if n := countByCategory(au, "deployment_outcome_recorded"); n != 1 {
		t.Errorf("deployment_outcome_recorded entries = %d, want 1 (no second record)", n)
	}
}

// TestShipDeployment_InvalidPayload_400 pins the per-field validation branches:
// each missing/malformed required field (and an unknown field) is a 400
// deployment_invalid with no artifact created.
func TestShipDeployment_InvalidPayload_400(t *testing.T) {
	cases := map[string][]byte{
		"missing environment":      []byte(`{"ref":"abc","external_run_url":"https://x/1","outcome":"succeeded"}`),
		"missing ref":              []byte(`{"environment":"prod","external_run_url":"https://x/1","outcome":"succeeded"}`),
		"missing external_run_url": []byte(`{"environment":"prod","ref":"abc","outcome":"succeeded"}`),
		"non-http url":             []byte(`{"environment":"prod","ref":"abc","external_run_url":"ssh://x","outcome":"succeeded"}`),
		"missing outcome":          []byte(`{"environment":"prod","ref":"abc","external_run_url":"https://x/1"}`),
		"invalid outcome":          []byte(`{"environment":"prod","ref":"abc","external_run_url":"https://x/1","outcome":"shipped"}`),
		"invalid rollback_action":  []byte(`{"environment":"prod","ref":"abc","external_run_url":"https://x/1","outcome":"rolled_back","rollback_action":"halfway"}`),
		"unknown field":            []byte(`{"environment":"prod","ref":"abc","external_run_url":"https://x/1","outcome":"succeeded","extra":true}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			runID, stageID := uuid.New(), uuid.New()
			s, sf, ar, _, _ := newDeploymentServer(t, runID, stageID)
			priv, _ := sf.issue(t, runID)
			w := shipDeploymentRequest(t, s, runID, stageID, priv, body, "")
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400:\n%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "deployment_invalid") {
				t.Errorf("body missing deployment_invalid:\n%s", w.Body.String())
			}
			if len(ar.all) != 0 {
				t.Errorf("artifacts = %d, want 0", len(ar.all))
			}
		})
	}
}

// TestShipDeployment_RollbackVariants pins the rollback sub-action branches: a
// body carrying rollback_action writes the matching rollback audit category IN
// ADDITION to the always-written deployment_outcome_recorded entry.
func TestShipDeployment_RollbackVariants(t *testing.T) {
	cases := []struct {
		action   string
		wantCat  string
		otherCat string
	}{
		{"initiated", "deployment_rollback_initiated", "deployment_rollback_completed"},
		{"completed", "deployment_rollback_completed", "deployment_rollback_initiated"},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			runID, stageID := uuid.New(), uuid.New()
			s, sf, _, au, _ := newDeploymentServer(t, runID, stageID)
			priv, _ := sf.issue(t, runID)
			body, err := json.Marshal(map[string]any{
				"environment":      "production",
				"ref":              "abc",
				"external_run_url": "https://x/1",
				"outcome":          "rolled_back",
				"rollback_handle":  "deploy-42",
				"rollback_action":  tc.action,
			})
			if err != nil {
				t.Fatal(err)
			}
			w := shipDeploymentRequest(t, s, runID, stageID, priv, body, "")
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
			}
			if n := countByCategory(au, "deployment_outcome_recorded"); n != 1 {
				t.Errorf("deployment_outcome_recorded entries = %d, want 1", n)
			}
			if n := countByCategory(au, tc.wantCat); n != 1 {
				t.Errorf("%s entries = %d, want 1", tc.wantCat, n)
			}
			if n := countByCategory(au, tc.otherCat); n != 0 {
				t.Errorf("%s entries = %d, want 0 (only the reported action)", tc.otherCat, n)
			}
		})
	}
}

// auditFailOnCategory wraps an auditFake and forces AppendChained to fail for
// exactly one category, delegating every other append (and the rest of the
// audit.Repository surface) to the embedded fake. A global au.appendErr makes
// the FIRST (deployment_outcome_recorded) append fail and short-circuits with a
// 500 before the rollback append is reached, so it cannot exercise the
// best-effort rollback branch — this wrapper lets the outcome append succeed
// and fails only the second (rollback) append.
type auditFailOnCategory struct {
	*auditFake
	failCategory string
}

func (a *auditFailOnCategory) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if p.Category == a.failCategory {
		return nil, errors.New("rollback append boom")
	}
	return a.auditFake.AppendChained(ctx, p)
}

// TestShipDeployment_RollbackAppendFails_StillCreated pins the best-effort
// rollback-append branch: when the always-written deployment_outcome_recorded
// append succeeds but the additive rollback append fails, the handler WARN-logs
// and still returns 201 — it does NOT unwind the already-persisted artifact or
// the durable outcome entry. This is the one error path the global-appendErr
// test can't reach (that one fails the first append and 500s first).
func TestShipDeployment_RollbackAppendFails_StillCreated(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	sf := newSigningFake()
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	rr := newPromptRunRepo()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeDeploy}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		ArtifactRepo: ar,
		AuditRepo:    &auditFailOnCategory{auditFake: au, failCategory: CategoryDeploymentRollbackInitiated},
		RunRepo:      rr,
	})
	priv, _ := sf.issue(t, runID)
	body, err := json.Marshal(map[string]any{
		"environment":      "production",
		"ref":              "abc",
		"external_run_url": "https://x/1",
		"outcome":          "rolled_back",
		"rollback_handle":  "deploy-42",
		"rollback_action":  "initiated",
	})
	if err != nil {
		t.Fatal(err)
	}

	w := shipDeploymentRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (rollback append is best-effort, non-fatal):\n%s", w.Code, w.Body.String())
	}
	// Artifact persisted and NOT unwound by the failed rollback append.
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1 (artifact kept despite rollback-append failure)", len(ar.all))
	}
	// The durable outcome entry succeeded and is kept.
	if n := countByCategory(au, "deployment_outcome_recorded"); n != 1 {
		t.Errorf("deployment_outcome_recorded entries = %d, want 1 (durable record kept)", n)
	}
	// The rollback entry append failed, so none is recorded.
	if n := countByCategory(au, "deployment_rollback_initiated"); n != 0 {
		t.Errorf("deployment_rollback_initiated entries = %d, want 0 (append failed)", n)
	}
}

// TestShipDeployment_NoAuth_401 pins the auth-rejection branch: a request with
// neither a signature nor a bearer token is refused 401.
func TestShipDeployment_NoAuth_401(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, ar, _, _ := newDeploymentServer(t, runID, stageID)
	url := fmt.Sprintf("/v0/runs/%s/deployment?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(validDeploymentBytes(t)))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "signature_or_bearer_required") {
		t.Errorf("body missing signature_or_bearer_required:\n%s", w.Body.String())
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0 on auth failure", len(ar.all))
	}
}

// TestShipDeployment_BearerInsufficientScope_401 pins that a bearer token
// without write:runs is rejected (the second half of the auth-rejection
// branch).
func TestShipDeployment_BearerInsufficientScope_401(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, _, _, _ := newDeploymentServer(t, runID, stageID)
	url := fmt.Sprintf("/v0/runs/%s/deployment?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(validDeploymentBytes(t)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("run_id", runID.String())
	req.SetPathValue("stage_id", stageID.String())
	ctx := context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "operator:test",
		TokenID: "tok-abc",
		Scopes:  []string{"read:runs"},
	})
	w := httptest.NewRecorder()
	s.handleShipDeployment(w, req.WithContext(ctx))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "signature_or_bearer_required") {
		t.Errorf("body missing signature_or_bearer_required:\n%s", w.Body.String())
	}
}

// TestShipDeployment_BearerHappyPath_201 pins the operator bearer path: a
// write:runs token records the artifact + audit with auth_method=bearer and the
// actor kind resolved from the subject (ADR-040 D4).
func TestShipDeployment_BearerHappyPath_201(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, ar, au, _ := newDeploymentServer(t, runID, stageID)

	url := fmt.Sprintf("/v0/runs/%s/deployment?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(validDeploymentBytes(t)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("run_id", runID.String())
	req.SetPathValue("stage_id", stageID.String())
	ctx := context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "operator:test",
		TokenID: "tok-abc",
		Scopes:  []string{"write:runs"},
	})
	w := httptest.NewRecorder()
	s.handleShipDeployment(w, req.WithContext(ctx))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1", len(ar.all))
	}
	if n := countByCategory(au, "deployment_outcome_recorded"); n != 1 {
		t.Fatalf("deployment_outcome_recorded entries = %d, want 1", n)
	}
	entry := au.appended[0]
	if entry.ActorKind == nil || *entry.ActorKind != audit.ActorUser {
		t.Errorf("ActorKind = %v, want user (plain operator subject)", entry.ActorKind)
	}
	if !strings.Contains(string(entry.Payload), `"auth_method":"bearer"`) {
		t.Errorf("audit payload missing auth_method=bearer: %s", entry.Payload)
	}
}

// TestShipDeployment_StageMismatch_400 pins the stage-ownership guard: a stage
// that belongs to a different run is rejected before any persistence.
func TestShipDeployment_StageMismatch_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, rr := newDeploymentServer(t, runID, stageID)
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: uuid.New()}
	priv, _ := sf.issue(t, runID)
	w := shipDeploymentRequest(t, s, runID, stageID, priv, validDeploymentBytes(t), "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (stage doesn't belong to run)", w.Code)
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0", len(ar.all))
	}
}

// TestShipDeployment_WrongStageType_400 pins the stage-type guard: a stage
// that belongs to the run but is NOT a deploy stage (e.g. implement) is
// rejected before any persistence. A deployment governance artifact may only
// be attached to a deploy stage (ADR-038), so a valid run signer or write:runs
// bearer cannot pin a signed deploy record + deploy audit chain onto a
// plan/implement/review stage.
func TestShipDeployment_WrongStageType_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, rr := newDeploymentServer(t, runID, stageID)
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}
	priv, _ := sf.issue(t, runID)
	w := shipDeploymentRequest(t, s, runID, stageID, priv, validDeploymentBytes(t), "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (stage is not a deploy stage):\n%s", w.Code, w.Body.String())
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0 (rejected before persistence)", len(ar.all))
	}
	if n := countByCategory(au, "deployment_outcome_recorded"); n != 0 {
		t.Errorf("deployment_outcome_recorded entries = %d, want 0 (rejected before persistence)", n)
	}
}

// TestShipDeployment_StageNotFound_404 pins the missing-stage guard: a
// stage_id the run repo doesn't know is a 404 before any persistence.
func TestShipDeployment_StageNotFound_404(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, rr := newDeploymentServer(t, runID, stageID)
	delete(rr.getStages, stageID) // un-seed the stage the helper added
	priv, _ := sf.issue(t, runID)
	w := shipDeploymentRequest(t, s, runID, stageID, priv, validDeploymentBytes(t), "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0", len(ar.all))
	}
}

// TestShipDeployment_AuditAppendFails_500 pins the durable-record branch: when
// the deployment_outcome_recorded append fails, the handler returns 500 rather
// than reporting a persisted-but-unaudited deploy.
func TestShipDeployment_AuditAppendFails_500(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, au, _ := newDeploymentServer(t, runID, stageID)
	au.appendErr = errors.New("boom")
	priv, _ := sf.issue(t, runID)
	w := shipDeploymentRequest(t, s, runID, stageID, priv, validDeploymentBytes(t), "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
}

// TestShipDeployment_Unconfigured_503 pins the dependency guard: with no repos
// wired the handler is unavailable.
func TestShipDeployment_Unconfigured_503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	url := fmt.Sprintf("/v0/runs/%s/deployment?stage_id=%s", uuid.New(), uuid.New())
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestShipDeployment_BodyTooLarge_413 pins the size cap.
func TestShipDeployment_BodyTooLarge_413(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, _ := newDeploymentServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := bytes.Repeat([]byte("x"), maxDeploymentBundleBytes+1)
	w := shipDeploymentRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

// ---- slice 2 (#1386 / E23.6): deploy reconciler resolver + webhook terminal ----

// deployStageRepo is a stateful run.Repository for the deploy-executor tests:
// it serves the parked deploy stage, records transitions, and lists
// awaiting-deployment deploy stages for the reconciler drive. Embeds BaseFake
// for the rest of the interface.
type deployStageRepo struct {
	run.BaseFake
	mu          sync.Mutex
	stages      map[uuid.UUID]*run.Stage
	runs        map[uuid.UUID]*run.Run
	transitions []run.StageState
}

func newDeployStageRepo() *deployStageRepo {
	return &deployStageRepo{stages: map[uuid.UUID]*run.Stage{}, runs: map[uuid.UUID]*run.Run{}}
}

func (r *deployStageRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.stages[id]; ok {
		cp := *s
		return &cp, nil
	}
	return nil, run.ErrNotFound
}

func (r *deployStageRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rn, ok := r.runs[id]; ok {
		return rn, nil
	}
	return nil, run.ErrNotFound
}

func (r *deployStageRepo) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.stages[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	s.State = to
	r.transitions = append(r.transitions, to)
	cp := *s
	return &cp, nil
}

func (r *deployStageRepo) ListDeployStagesAwaitingDeployment(_ context.Context) ([]*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*run.Stage
	for _, s := range r.stages {
		if s.Type == run.StageTypeDeploy && s.State == run.StageStateAwaitingDeployment {
			cp := *s
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *deployStageRepo) stageState(id uuid.UUID) run.StageState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stages[id].State
}

// newResolverServer wires a server whose RunRepo is the stateful deploy repo,
// with the deploy stage seeded at awaiting_deployment.
func newResolverServer(t *testing.T) (*Server, *deployStageRepo, *fakeArtifactRepo, *auditFake, uuid.UUID, uuid.UUID) {
	t.Helper()
	runID, stageID := uuid.New(), uuid.New()
	rr := newDeployStageRepo()
	rr.stages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeDeploy, State: run.StageStateAwaitingDeployment}
	rr.runs[runID] = &run.Run{ID: runID, Repo: "octo/repo", WorkflowID: "deploy"}
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  newSigningFake(),
		ArtifactRepo: ar,
		AuditRepo:    au,
		RunRepo:      rr,
	})
	return s, rr, ar, au, runID, stageID
}

func TestResolveDeploymentFromPollState_Success(t *testing.T) {
	s, rr, ar, au, runID, stageID := newResolverServer(t)
	wr := &githubclient.WorkflowRun{ID: 555, HTMLURL: "https://gh/run/555", Status: "completed", Conclusion: "success"}

	if err := s.ResolveDeploymentFromPollState(context.Background(), runID, stageID, run.DeployOutcomeSucceeded, "main", wr); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := rr.stageState(stageID); got != run.StageStateSucceeded {
		t.Errorf("stage state = %q, want succeeded", got)
	}
	if len(ar.all) != 1 || ar.all[0].Kind != artifact.KindDeployment {
		t.Fatalf("want 1 deployment artifact, got %d", len(ar.all))
	}
	if !strings.Contains(string(ar.all[0].Content), `"outcome":"succeeded"`) {
		t.Errorf("artifact content missing outcome: %s", ar.all[0].Content)
	}
	if n := countByCategory(au, CategoryDeploymentOutcomeRecorded); n != 1 {
		t.Errorf("deployment_outcome_recorded entries = %d, want 1", n)
	}
	if n := countByCategory(au, CategoryDeployRun); n != 1 {
		t.Errorf("deploy_run trace events = %d, want 1", n)
	}
}

func TestResolveDeploymentFromPollState_FailedAndPartial(t *testing.T) {
	cases := []struct {
		name    string
		outcome run.DeployOutcome
	}{
		{"failed", run.DeployOutcomeFailed},
		{"partial", run.DeployOutcomePartial},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, rr, ar, _, runID, stageID := newResolverServer(t)
			wr := &githubclient.WorkflowRun{ID: 1, Status: "completed", Conclusion: "failure"}
			if err := s.ResolveDeploymentFromPollState(context.Background(), runID, stageID, c.outcome, "main", wr); err != nil {
				t.Fatalf("resolve: %v", err)
			}
			// Both failed and partial land the stage in the failed STATE; the
			// disposition rides the artifact's outcome field.
			if got := rr.stageState(stageID); got != run.StageStateFailed {
				t.Errorf("stage state = %q, want failed", got)
			}
			want := fmt.Sprintf(`"outcome":%q`, c.outcome)
			if !strings.Contains(string(ar.all[0].Content), want) {
				t.Errorf("artifact missing %s: %s", want, ar.all[0].Content)
			}
		})
	}
}

// A repeat resolve (the webhook callback or an earlier tick already moved the
// stage out of awaiting_deployment) is an idempotent no-op: no second artifact,
// no second audit, no transition.
func TestResolveDeploymentFromPollState_NotParked_NoOp(t *testing.T) {
	s, rr, ar, au, runID, stageID := newResolverServer(t)
	rr.stages[stageID].State = run.StageStateSucceeded // already resolved

	if err := s.ResolveDeploymentFromPollState(context.Background(), runID, stageID, run.DeployOutcomeSucceeded, "main",
		&githubclient.WorkflowRun{ID: 1, Status: "completed", Conclusion: "success"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0 (no-op on settled stage)", len(ar.all))
	}
	if n := countByCategory(au, CategoryDeploymentOutcomeRecorded); n != 0 {
		t.Errorf("audit entries = %d, want 0 (no-op)", n)
	}
}

func TestResolveDeploymentFromPollState_InvalidOutcome_Errors(t *testing.T) {
	s, _, _, _, runID, stageID := newResolverServer(t)
	if err := s.ResolveDeploymentFromPollState(context.Background(), runID, stageID, run.DeployOutcome("bogus"), "main", nil); err == nil {
		t.Fatal("want error on invalid outcome, got nil")
	}
}

// Webhook-target callback: the external pipeline POSTs its terminal outcome to
// POST /v0/runs/{run_id}/deployment, and the handler advances the parked deploy
// stage to the mapped terminal state (#1386 slice-2 / item 7).
func TestShipDeployment_WebhookCallback_TransitionsTerminal(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	rr := newDeployStageRepo()
	rr.stages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeDeploy, State: run.StageStateAwaitingDeployment}
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	sf := newSigningFake()
	s := New(Config{Addr: "127.0.0.1:0", SigningRepo: sf, ArtifactRepo: ar, AuditRepo: au, RunRepo: rr})
	priv, _ := sf.issue(t, runID)

	if w := shipDeploymentRequest(t, s, runID, stageID, priv, validDeploymentBytes(t), ""); w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if got := rr.stageState(stageID); got != run.StageStateSucceeded {
		t.Errorf("webhook callback left stage at %q, want succeeded", got)
	}
}

// A callback whose stage is NOT parked at awaiting_deployment must not
// re-transition (github_actions stages reach terminal via the reconciler; a
// re-delivery to a settled stage is a no-op).
func TestShipDeployment_WebhookCallback_NotParked_NoTransition(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	rr := newDeployStageRepo()
	rr.stages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeDeploy, State: run.StageStateRunning}
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	sf := newSigningFake()
	s := New(Config{Addr: "127.0.0.1:0", SigningRepo: sf, ArtifactRepo: ar, AuditRepo: au, RunRepo: rr})
	priv, _ := sf.issue(t, runID)

	if w := shipDeploymentRequest(t, s, runID, stageID, priv, validDeploymentBytes(t), ""); w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	if got := rr.stageState(stageID); got != run.StageStateRunning {
		t.Errorf("stage transitioned to %q, want unchanged running (not parked)", got)
	}
	if len(rr.transitions) != 0 {
		t.Errorf("transitions = %v, want none on a non-parked stage", rr.transitions)
	}
}

// Cross-boundary integration (required by the plan): drive a deploy stage
// end-to-end through the real deployreconciler.Ticker against a fakeGitHub
// poller, with the server as the Resolver. Asserts the trigger→poll→persist
// seam: the reconciler reads the dispatched handle, polls the run to a terminal
// success, and the server resolve persists the artifact + audits + deploy_run
// trace event AND transitions the stage to the mapped terminal state.
func TestDeployReconciler_EndToEnd(t *testing.T) {
	s, rr, ar, au, runID, stageID := newResolverServer(t)

	// Seed slice-1's deployment_dispatched handle the reconciler reads back.
	handle, _ := json.Marshal(map[string]any{
		"target":        "github_actions",
		"gha_run_id":    int64(909),
		"git_ref":       "main",
		"dispatched_at": time.Now().UTC().Format(time.RFC3339),
	})
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &runID, StageID: &stageID, Category: CategoryDeploymentDispatched, Payload: handle,
	})
	// The run needs an installation id for the reconciler to poll.
	rr.runs[runID].InstallationID = ptrInt64(99)

	poller := &fakeDeployPoller{get: &githubclient.WorkflowRun{ID: 909, HTMLURL: "https://gh/run/909", Status: "completed", Conclusion: "success"}}
	ticker := &deployreconciler.Ticker{Runs: rr, GH: poller, Audit: au, Resolver: s}
	ticker.Tick(context.Background())

	if got := rr.stageState(stageID); got != run.StageStateSucceeded {
		t.Fatalf("end-to-end stage state = %q, want succeeded", got)
	}
	if len(ar.all) != 1 || ar.all[0].Kind != artifact.KindDeployment {
		t.Fatalf("want 1 deployment artifact persisted, got %d", len(ar.all))
	}
	if n := countByCategory(au, CategoryDeploymentOutcomeRecorded); n != 1 {
		t.Errorf("deployment_outcome_recorded entries = %d, want 1", n)
	}
	if n := countByCategory(au, CategoryDeployRun); n != 1 {
		t.Errorf("deploy_run trace events = %d, want 1", n)
	}
	if poller.getCalls != 1 {
		t.Errorf("GetWorkflowRun calls = %d, want 1", poller.getCalls)
	}
}

// fakeDeployPoller is the deployreconciler.WorkflowRunPoller seam for the
// cross-boundary drive.
type fakeDeployPoller struct {
	get      *githubclient.WorkflowRun
	getCalls int
}

func (f *fakeDeployPoller) GetWorkflowRun(_ context.Context, _ int64, _ githubclient.RepoRef, _ int64) (*githubclient.WorkflowRun, error) {
	f.getCalls++
	return f.get, nil
}

func (f *fakeDeployPoller) ResolveDispatchedRun(_ context.Context, _ int64, _ githubclient.RepoRef, _ string, _ map[string]string, _ time.Time) (*githubclient.WorkflowRun, error) {
	return nil, nil
}
