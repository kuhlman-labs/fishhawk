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
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// validAcceptanceBytes returns a complete passed-verdict acceptanceBody payload
// with two passing criteria.
func validAcceptanceBytes(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(acceptanceBody{
		Verdict: "passed",
		Criteria: []acceptanceCriterionResult{
			{ID: "ac-create", Result: "passed", Observed: "201 returned"},
			{ID: "ac-list", Result: "passed"},
		},
		TargetURL:      "https://preview.example.test",
		EvidenceHashes: []string{"sha256:abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// newAcceptanceServer wires the acceptance handler against the shared fakes.
func newAcceptanceServer(t *testing.T, runID, stageID uuid.UUID) (*Server, *signingFake, *fakeArtifactRepo, *auditFake, *promptRunRepo) {
	t.Helper()
	sf := newSigningFake()
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	rr := newPromptRunRepo()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeAcceptance}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		ArtifactRepo: ar,
		AuditRepo:    au,
		RunRepo:      rr,
	})
	return s, sf, ar, au, rr
}

func shipAcceptanceRequest(t *testing.T, s *Server, runID, stageID uuid.UUID, priv ed25519.PrivateKey, body []byte, sigOverride string) *httptest.ResponseRecorder {
	t.Helper()
	url := fmt.Sprintf("/v0/runs/%s/acceptance?stage_id=%s", runID, stageID)
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

// TestShipAcceptance_HappyPath crosses the handler -> persistence -> audit seam:
// a signed acceptance record persists a KindAcceptance artifact and writes a
// single acceptance_outcome_recorded chained audit entry carrying the verdict +
// the render tally.
func TestShipAcceptance_HappyPath(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newAcceptanceServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validAcceptanceBytes(t)

	w := shipAcceptanceRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var resp acceptanceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Verdict != "passed" || resp.Idempotent {
		t.Errorf("response not populated: %+v", resp)
	}

	if len(ar.all) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(ar.all))
	}
	got, err := ar.Get(context.Background(), resp.ID)
	if err != nil {
		t.Fatalf("read back artifact: %v", err)
	}
	if got.Kind != artifact.KindAcceptance {
		t.Errorf("artifact Kind = %q, want acceptance", got.Kind)
	}
	if got.ContentHash != sha256Hex(body) {
		t.Errorf("content hash = %q, want %q", got.ContentHash, sha256Hex(body))
	}

	if n := countByCategory(au, CategoryAcceptanceOutcomeRecorded); n != 1 {
		t.Fatalf("acceptance_outcome_recorded entries = %d, want 1", n)
	}
	payload := string(au.appended[0].Payload)
	for _, want := range []string{
		`"verdict":"passed"`, `"outcome":"accepted"`, `"criteria_passed":2`,
		`"criteria_total":2`, `"auth_method":"ed25519"`,
	} {
		if !strings.Contains(payload, want) {
			t.Errorf("audit payload missing %s: %s", want, payload)
		}
	}
}

// TestShipAcceptance_FailureModeCarryThrough is the E31.8 done-means: a failed
// verdict persists the failure_mode verbatim in the audit payload for BOTH
// error and assertion_fail, and maps the render outcome to "rejected".
func TestShipAcceptance_FailureModeCarryThrough(t *testing.T) {
	for _, mode := range []string{"error", "assertion_fail"} {
		t.Run(mode, func(t *testing.T) {
			runID, stageID := uuid.New(), uuid.New()
			s, sf, _, au, _ := newAcceptanceServer(t, runID, stageID)
			priv, _ := sf.issue(t, runID)
			body, err := json.Marshal(acceptanceBody{
				Verdict:     "failed",
				FailureMode: mode,
				Criteria: []acceptanceCriterionResult{
					{ID: "ac-create", Result: "failed", Observed: "500"},
					{ID: "ac-list", Result: "skipped"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			w := shipAcceptanceRequest(t, s, runID, stageID, priv, body, "")
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
			}
			payload := string(au.appended[0].Payload)
			for _, want := range []string{
				fmt.Sprintf(`"failure_mode":%q`, mode),
				`"verdict":"failed"`, `"outcome":"rejected"`,
				`"criteria_passed":0`, `"criteria_failed":1`, `"criteria_skipped":1`,
			} {
				if !strings.Contains(payload, want) {
					t.Errorf("audit payload missing %s: %s", want, payload)
				}
			}
			var resp acceptanceResponse
			_ = json.NewDecoder(w.Body).Decode(&resp)
			if resp.FailureMode != mode {
				t.Errorf("response failure_mode = %q, want %q", resp.FailureMode, mode)
			}
		})
	}
}

// TestShipAcceptance_Idempotent_SecondUpload pins the (stage_id, content_hash)
// dedup: a re-delivery of the identical record returns the existing artifact
// (idempotent=true) and writes no second artifact or audit row.
func TestShipAcceptance_Idempotent_SecondUpload(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newAcceptanceServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validAcceptanceBytes(t)

	if w := shipAcceptanceRequest(t, s, runID, stageID, priv, body, ""); w.Code != http.StatusCreated {
		t.Fatalf("first upload status = %d", w.Code)
	}
	w2 := shipAcceptanceRequest(t, s, runID, stageID, priv, body, "")
	if w2.Code != http.StatusOK {
		t.Fatalf("second upload status = %d, want 200", w2.Code)
	}
	var resp acceptanceResponse
	_ = json.NewDecoder(w2.Body).Decode(&resp)
	if !resp.Idempotent {
		t.Error("second upload should be marked idempotent=true")
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1 (no duplicate row)", len(ar.all))
	}
	if n := countByCategory(au, CategoryAcceptanceOutcomeRecorded); n != 1 {
		t.Errorf("acceptance_outcome_recorded entries = %d, want 1", n)
	}
}

// TestShipAcceptance_RetryAfterAuditAppendFailure_Heals is the #1396 done-means:
// a partial write (artifact created, audit append fails → 500) followed by an
// identical retry ends with BOTH the artifact and its governance audit entry.
func TestShipAcceptance_RetryAfterAuditAppendFailure_Heals(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newAcceptanceServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validAcceptanceBytes(t)

	au.appendErr = errors.New("boom")
	w1 := shipAcceptanceRequest(t, s, runID, stageID, priv, body, "")
	if w1.Code != http.StatusInternalServerError {
		t.Fatalf("first ship status = %d, want 500:\n%s", w1.Code, w1.Body.String())
	}
	if len(ar.all) != 1 {
		t.Fatalf("artifacts after partial write = %d, want 1", len(ar.all))
	}
	if n := countByCategory(au, CategoryAcceptanceOutcomeRecorded); n != 0 {
		t.Fatalf("acceptance_outcome_recorded entries after partial write = %d, want 0", n)
	}

	au.appendErr = nil
	w2 := shipAcceptanceRequest(t, s, runID, stageID, priv, body, "")
	if w2.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200 (idempotent heal):\n%s", w2.Code, w2.Body.String())
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts after retry = %d, want 1 (no duplicate)", len(ar.all))
	}
	if n := countByCategory(au, CategoryAcceptanceOutcomeRecorded); n != 1 {
		t.Errorf("acceptance_outcome_recorded entries after retry = %d, want 1 (healed)", n)
	}
}

// TestShipAcceptance_Idempotent_AuditPresent_NoDuplicate pins that a clean first
// ship followed by an identical second ship leaves exactly one governance entry
// (the self-heal must not append a duplicate on the already-healthy path).
func TestShipAcceptance_Idempotent_AuditPresent_NoDuplicate(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newAcceptanceServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validAcceptanceBytes(t)

	if w := shipAcceptanceRequest(t, s, runID, stageID, priv, body, ""); w.Code != http.StatusCreated {
		t.Fatalf("first ship status = %d, want 201", w.Code)
	}
	if w := shipAcceptanceRequest(t, s, runID, stageID, priv, body, ""); w.Code != http.StatusOK {
		t.Fatalf("second ship status = %d, want 200", w.Code)
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1 (no duplicate)", len(ar.all))
	}
	if n := countByCategory(au, CategoryAcceptanceOutcomeRecorded); n != 1 {
		t.Errorf("acceptance_outcome_recorded entries = %d, want 1 (no duplicate heal)", n)
	}
}

// TestShipAcceptance_IdempotentHeal_ListError_500 pins the fail-closed read
// branch: an idempotent retry while ListForRunByCategory errors returns 500
// (governance integrity, not a gapped 200).
func TestShipAcceptance_IdempotentHeal_ListError_500(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newAcceptanceServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validAcceptanceBytes(t)

	if w := shipAcceptanceRequest(t, s, runID, stageID, priv, body, ""); w.Code != http.StatusCreated {
		t.Fatalf("first ship status = %d, want 201", w.Code)
	}
	if len(ar.all) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(ar.all))
	}
	au.listByCategoryErr = errors.New("audit read down")
	w2 := shipAcceptanceRequest(t, s, runID, stageID, priv, body, "")
	if w2.Code != http.StatusInternalServerError {
		t.Fatalf("retry status = %d, want 500 (fail closed):\n%s", w2.Code, w2.Body.String())
	}
}

// TestShipAcceptance_InvalidPayload_400 pins the per-field validation branches:
// each malformed field (and an unknown field) is a 400 acceptance_invalid with
// no artifact created.
func TestShipAcceptance_InvalidPayload_400(t *testing.T) {
	cases := map[string][]byte{
		"missing verdict":             []byte(`{"criteria":[]}`),
		"unknown verdict":             []byte(`{"verdict":"maybe"}`),
		"failed missing failure_mode": []byte(`{"verdict":"failed"}`),
		"failed invalid failure_mode": []byte(`{"verdict":"failed","failure_mode":"halfway"}`),
		"passed with failure_mode":    []byte(`{"verdict":"passed","failure_mode":"error"}`),
		"criterion empty id":          []byte(`{"verdict":"passed","criteria":[{"id":"","result":"passed"}]}`),
		"criterion invalid result":    []byte(`{"verdict":"passed","criteria":[{"id":"x","result":"maybe"}]}`),
		"non-http target_url":         []byte(`{"verdict":"passed","target_url":"ssh://x"}`),
		"unknown field":               []byte(`{"verdict":"passed","extra":true}`),
		"trailing object":             []byte(`{"verdict":"passed"}{"verdict":"failed","failure_mode":"error"}`),
		"trailing garbage":            []byte(`{"verdict":"passed"} garbage`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			runID, stageID := uuid.New(), uuid.New()
			s, sf, ar, _, _ := newAcceptanceServer(t, runID, stageID)
			priv, _ := sf.issue(t, runID)
			w := shipAcceptanceRequest(t, s, runID, stageID, priv, body, "")
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400:\n%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "acceptance_invalid") {
				t.Errorf("body missing acceptance_invalid:\n%s", w.Body.String())
			}
			if len(ar.all) != 0 {
				t.Errorf("artifacts = %d, want 0", len(ar.all))
			}
		})
	}
}

// TestShipAcceptance_NoAuth_401 pins the auth-required default arm.
func TestShipAcceptance_NoAuth_401(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, ar, _, _ := newAcceptanceServer(t, runID, stageID)
	url := fmt.Sprintf("/v0/runs/%s/acceptance?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(validAcceptanceBytes(t)))
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

// TestShipAcceptance_BearerInsufficientScope_401 pins that a bearer token
// WITHOUT write:runs is rejected via the default 401 arm (unlike deploy there
// is no separate scope-403 branch — acceptance adds no scope).
func TestShipAcceptance_BearerInsufficientScope_401(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, ar, _, _ := newAcceptanceServer(t, runID, stageID)
	url := fmt.Sprintf("/v0/runs/%s/acceptance?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(validAcceptanceBytes(t)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("run_id", runID.String())
	req.SetPathValue("stage_id", stageID.String())
	ctx := context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "operator:test",
		TokenID: "tok-abc",
		Scopes:  []string{"read:runs"},
	})
	w := httptest.NewRecorder()
	s.handleShipAcceptance(w, req.WithContext(ctx))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "signature_or_bearer_required") {
		t.Errorf("body missing signature_or_bearer_required:\n%s", w.Body.String())
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0", len(ar.all))
	}
}

// TestShipAcceptance_BearerHappyPath_201 pins the operator bearer path: a token
// holding write:runs (no write:deploy-style extra scope) records the artifact +
// audit with auth_method=bearer.
func TestShipAcceptance_BearerHappyPath_201(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, ar, au, _ := newAcceptanceServer(t, runID, stageID)
	url := fmt.Sprintf("/v0/runs/%s/acceptance?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(validAcceptanceBytes(t)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("run_id", runID.String())
	req.SetPathValue("stage_id", stageID.String())
	ctx := context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "operator:test",
		TokenID: "tok-abc",
		Scopes:  []string{"write:runs"},
	})
	w := httptest.NewRecorder()
	s.handleShipAcceptance(w, req.WithContext(ctx))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1", len(ar.all))
	}
	if n := countByCategory(au, CategoryAcceptanceOutcomeRecorded); n != 1 {
		t.Fatalf("acceptance_outcome_recorded entries = %d, want 1", n)
	}
	if !strings.Contains(string(au.appended[0].Payload), `"auth_method":"bearer"`) {
		t.Errorf("audit payload missing auth_method=bearer: %s", au.appended[0].Payload)
	}
}

// TestShipAcceptance_InvalidSignature_401 pins the signature-verify denial: a
// valid-hex signature that does not verify is a 401 signature_invalid.
func TestShipAcceptance_InvalidSignature_401(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, _ := newAcceptanceServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	sf.verifyErr = signing.ErrSignatureInvalid
	w := shipAcceptanceRequest(t, s, runID, stageID, priv, validAcceptanceBytes(t), "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "signature_invalid") {
		t.Errorf("body missing signature_invalid:\n%s", w.Body.String())
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0 on bad signature", len(ar.all))
	}
}

// TestShipAcceptance_SigningKeyNotFound_404 pins the signature branch's
// ErrNotFound mapping: no signing key issued for the run is a 404
// signing_key_not_found (a distinct branch copied from the deploy precedent,
// so guard against a future refactor swapping its status/code).
func TestShipAcceptance_SigningKeyNotFound_404(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, _ := newAcceptanceServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	sf.verifyErr = signing.ErrNotFound
	w := shipAcceptanceRequest(t, s, runID, stageID, priv, validAcceptanceBytes(t), "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "signing_key_not_found") {
		t.Errorf("body missing signing_key_not_found:\n%s", w.Body.String())
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0 on missing key", len(ar.all))
	}
}

// TestShipAcceptance_SigningKeyExpired_401 pins the signature branch's
// ErrExpired mapping: a lapsed signing-key TTL is a 401 signing_key_expired.
func TestShipAcceptance_SigningKeyExpired_401(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, _ := newAcceptanceServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	sf.verifyErr = signing.ErrExpired
	w := shipAcceptanceRequest(t, s, runID, stageID, priv, validAcceptanceBytes(t), "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "signing_key_expired") {
		t.Errorf("body missing signing_key_expired:\n%s", w.Body.String())
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0 on expired key", len(ar.all))
	}
}

// TestShipAcceptance_WrongStageType_400 pins the stage-type guard: an
// acceptance record may only attach to an acceptance stage, so a non-acceptance
// stage is rejected before any persistence.
func TestShipAcceptance_WrongStageType_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, rr := newAcceptanceServer(t, runID, stageID)
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}
	priv, _ := sf.issue(t, runID)
	w := shipAcceptanceRequest(t, s, runID, stageID, priv, validAcceptanceBytes(t), "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "acceptance stage") {
		t.Errorf("body missing stage-type detail:\n%s", w.Body.String())
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0 (rejected before persistence)", len(ar.all))
	}
	if n := countByCategory(au, CategoryAcceptanceOutcomeRecorded); n != 0 {
		t.Errorf("acceptance_outcome_recorded entries = %d, want 0", n)
	}
}

// TestShipAcceptance_StageMismatch_400 pins the stage-ownership guard.
func TestShipAcceptance_StageMismatch_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, rr := newAcceptanceServer(t, runID, stageID)
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: uuid.New(), Type: run.StageTypeAcceptance}
	priv, _ := sf.issue(t, runID)
	w := shipAcceptanceRequest(t, s, runID, stageID, priv, validAcceptanceBytes(t), "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0", len(ar.all))
	}
}

// TestShipAcceptance_StageNotFound_404 pins the missing-stage guard.
func TestShipAcceptance_StageNotFound_404(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, rr := newAcceptanceServer(t, runID, stageID)
	delete(rr.getStages, stageID)
	priv, _ := sf.issue(t, runID)
	w := shipAcceptanceRequest(t, s, runID, stageID, priv, validAcceptanceBytes(t), "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0", len(ar.all))
	}
}

// TestShipAcceptance_BodyTooLarge_413 pins the size cap.
func TestShipAcceptance_BodyTooLarge_413(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, _ := newAcceptanceServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := bytes.Repeat([]byte("x"), maxAcceptanceBundleBytes+1)
	w := shipAcceptanceRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

// TestShipAcceptance_Unconfigured_503 pins the dependency guard.
func TestShipAcceptance_Unconfigured_503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	url := fmt.Sprintf("/v0/runs/%s/acceptance?stage_id=%s", uuid.New(), uuid.New())
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestShipAcceptance_AuditAppendFails_500 pins the durable-record branch.
func TestShipAcceptance_AuditAppendFails_500(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, au, _ := newAcceptanceServer(t, runID, stageID)
	au.appendErr = errors.New("boom")
	priv, _ := sf.issue(t, runID)
	w := shipAcceptanceRequest(t, s, runID, stageID, priv, validAcceptanceBytes(t), "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
}

// TestShipAcceptance_CrossBoundary_WireToRender is the #618 cross-boundary
// integration test spanning wire -> persist -> audit -> render: GET the
// acceptance-stage prompt (criteria ids present, diff withheld), POST the signed
// evidence body, assert the persisted artifact + chained audit entry, and prove
// issuecomment's status template renders the acceptance outcome line from the
// emitted entries (the spec -> runner -> triage seam, capstone in E31.10).
func TestShipAcceptance_CrossBoundary_WireToRender(t *testing.T) {
	// Prompt seam: an acceptance stage exposes the approved plan's criteria and
	// withholds the diff.
	ps, runID, acceptanceStageID, priv, _ := newAcceptancePromptServer(t)
	pw := promptRequest(t, ps, runID, acceptanceStageID, priv, "")
	if pw.Code != http.StatusOK {
		t.Fatalf("prompt status = %d, want 200:\n%s", pw.Code, pw.Body.String())
	}
	var presp promptResponse
	if err := json.Unmarshal(pw.Body.Bytes(), &presp); err != nil {
		t.Fatalf("decode prompt: %v", err)
	}
	if !strings.Contains(presp.Prompt, "ac-create") || strings.Contains(presp.Prompt, "### Diff under review") {
		t.Errorf("prompt seam wrong (criteria present / diff absent):\n%s", presp.Prompt)
	}

	// Persist + audit seam: POST the signed evidence.
	as, sf, ar, au, _ := newAcceptanceServer(t, runID, acceptanceStageID)
	shipPriv, _ := sf.issue(t, runID)
	body, err := json.Marshal(acceptanceBody{
		Verdict: "passed",
		Criteria: []acceptanceCriterionResult{
			{ID: "ac-create", Result: "passed"},
			{ID: "ac-list", Result: "passed"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w := shipAcceptanceRequest(t, as, runID, acceptanceStageID, shipPriv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("ship status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if len(ar.all) != 1 || ar.all[0].Kind != artifact.KindAcceptance {
		t.Fatalf("artifact row wrong: %+v", ar.all)
	}
	entries, err := au.ListForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}

	// Render seam: issuecomment renders the acceptance outcome from the entries.
	runRow := &run.Run{ID: runID, Repo: "kuhlman-labs/fishhawk"}
	stages := []*run.Stage{{Type: run.StageTypeAcceptance, State: run.StageStateRunning}}
	rendered := issuecomment.RenderStatusBody(runRow, stages, entries, "https://x", time.Now())
	if !strings.Contains(rendered, "Acceptance recorded — accepted (2/2 criteria passed)") {
		t.Errorf("status body missing acceptance outcome line:\n%s", rendered)
	}
}
