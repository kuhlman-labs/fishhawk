package upload

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// planFakeBackend mounts a /v0/runs/{run_id}/plan handler with
// configurable response shape. Separate from fakeBackend so the
// per-test plumbing stays focused.
type planFakeBackend struct {
	mu sync.Mutex

	// Status drives the response code on the next call.
	status int
	// Body drives the response body. When empty + status==201 or
	// status==200, we synthesize a plausible ShipPlanResult.
	body string
	// errCount forces N consecutive 500s before falling through to
	// `status` — for testing retry behavior.
	errCount int
	// idempotent is set on the synthesized body when status==200.
	idempotent bool

	receivedBody []byte
	receivedSig  string
	receivedPath string
	calls        int
}

func newPlanFakeBackend(t *testing.T) (*planFakeBackend, *httptest.Server) {
	t.Helper()
	pf := &planFakeBackend{status: http.StatusCreated}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/plan", func(w http.ResponseWriter, r *http.Request) {
		pf.mu.Lock()
		pf.calls++
		if pf.errCount > 0 {
			pf.errCount--
			pf.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		s := pf.status
		body := pf.body
		idem := pf.idempotent
		raw, _ := io.ReadAll(r.Body)
		pf.receivedBody = raw
		pf.receivedSig = r.Header.Get("X-Fishhawk-Signature")
		pf.receivedPath = r.URL.Path
		pf.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s)
		if (s == http.StatusCreated || s == http.StatusOK) && body == "" {
			_ = json.NewEncoder(w).Encode(ShipPlanResult{
				ID:            "00000000-0000-0000-0000-000000000aaa",
				StageID:       r.URL.Query().Get("stage_id"),
				ContentHash:   hex.EncodeToString(func() []byte { d := sha256.Sum256(raw); return d[:] }()),
				SchemaVersion: "standard_v1",
				Idempotent:    idem,
			})
		} else if body != "" {
			_, _ = io.WriteString(w, body)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return pf, srv
}

func makePlanKey(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

func quickPlanClient(srv *httptest.Server) *Client {
	c := New(srv.URL)
	c.MaxRetries = 3
	c.Backoff = time.Millisecond
	return c
}

func TestShipPlan_HappyPath_Created(t *testing.T) {
	pf, srv := newPlanFakeBackend(t)
	c := quickPlanClient(srv)
	priv, _ := makePlanKey(t)
	plan := []byte(`{"plan_version":"standard_v1"}`)

	res, err := c.ShipPlan(context.Background(), ShipPlanArgs{
		RunID:      "run-abc",
		StageID:    "stage-xyz",
		Plan:       plan,
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("ShipPlan: %v", err)
	}
	if res.SchemaVersion != "standard_v1" {
		t.Errorf("schema_version = %q", res.SchemaVersion)
	}
	if res.Idempotent {
		t.Error("expected Idempotent=false on 201")
	}
	if pf.calls != 1 {
		t.Errorf("calls = %d, want 1", pf.calls)
	}
	if pf.receivedPath != "/v0/runs/run-abc/plan" {
		t.Errorf("path = %q", pf.receivedPath)
	}
	if pf.receivedSig == "" {
		t.Error("missing signature header")
	}
	// Verify the signature matches what we'd compute over the body.
	digest := sha256.Sum256(plan)
	wantSig := hex.EncodeToString(ed25519.Sign(priv, digest[:]))
	if pf.receivedSig != wantSig {
		t.Errorf("signature mismatch")
	}
}

func TestShipPlan_HappyPath_Idempotent200(t *testing.T) {
	pf, srv := newPlanFakeBackend(t)
	pf.status = http.StatusOK
	pf.idempotent = true
	c := quickPlanClient(srv)
	priv, _ := makePlanKey(t)

	res, err := c.ShipPlan(context.Background(), ShipPlanArgs{
		RunID:      "r",
		StageID:    "s",
		Plan:       []byte(`{"plan_version":"standard_v1"}`),
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("ShipPlan: %v", err)
	}
	if !res.Idempotent {
		t.Error("expected Idempotent=true on 200")
	}
}

func TestShipPlan_RetriesOn5xx(t *testing.T) {
	pf, srv := newPlanFakeBackend(t)
	pf.errCount = 2 // 500, 500, then succeeds
	c := quickPlanClient(srv)
	priv, _ := makePlanKey(t)

	res, err := c.ShipPlan(context.Background(), ShipPlanArgs{
		RunID:      "r",
		StageID:    "s",
		Plan:       []byte(`{"plan_version":"standard_v1"}`),
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("ShipPlan: %v", err)
	}
	if pf.calls != 3 {
		t.Errorf("calls = %d, want 3 (2 retries + success)", pf.calls)
	}
	if res == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestShipPlan_PlanInvalid_400(t *testing.T) {
	pf, srv := newPlanFakeBackend(t)
	pf.status = http.StatusBadRequest
	pf.body = `{"code":"plan_invalid","message":"missing required field"}`
	c := quickPlanClient(srv)
	priv, _ := makePlanKey(t)

	_, err := c.ShipPlan(context.Background(), ShipPlanArgs{
		RunID:      "r",
		StageID:    "s",
		Plan:       []byte(`{"plan_version":"standard_v1"}`),
		PrivateKey: priv,
	})
	if !errors.Is(err, ErrPlanInvalid) {
		t.Errorf("err = %v, want ErrPlanInvalid", err)
	}
	if pf.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 400)", pf.calls)
	}
}

// TestShipPlan_AgentOutputInvalid_400 is the #2833 widening: every backend
// error code that means "the AGENT's output is bad" maps to ErrPlanInvalid
// (runner category-B), and every other 400 stays a generic error
// (category-C). All four codes arrive on this one endpoint because POST
// /v0/runs/{run_id}/plan routes by the artifact's "kind" discriminator.
//
// The control rows are the point:
//   - "unrelated code" proves the widening is a NAMED list, not a blanket;
//   - "message mentions a listed code" proves the classification EXACT-MATCHES
//     error.code and does not substring-scan the free-text message. That row
//     goes green under a substring implementation only by misclassifying a
//     retryable failure as permanent-B, which is exactly the hazard the
//     envelope decode exists to close.
func TestShipPlan_AgentOutputInvalid_400(t *testing.T) {
	// envelope renders the backend's real error shape
	// (backend/internal/server/errors.go errorEnvelope).
	envelope := func(code, message string) string {
		b, err := json.Marshal(map[string]any{
			"error": map[string]any{"code": code, "message": message},
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	cases := []struct {
		name       string
		body       string
		wantB      bool
		wantReason string
	}{
		{
			name:       "plan_invalid",
			body:       envelope("plan_invalid", "plan does not validate against standard_v1"),
			wantB:      true,
			wantReason: "the pre-#2833 code, unchanged",
		},
		{
			name:       "grooming_report_invalid",
			body:       envelope("grooming_report_invalid", "grooming_report does not validate against grooming-report-v1"),
			wantB:      true,
			wantReason: "backend failGroomingStage already transitioned the stage to failed-B",
		},
		{
			name:       "grooming_report_stage_invalid",
			body:       envelope("grooming_report_stage_invalid", "grooming_report may only be shipped from a plan stage"),
			wantB:      true,
			wantReason: "the stage type is a property of the run — re-shipping cannot help",
		},
		{
			name:       "clarification_request_invalid",
			body:       envelope("clarification_request_invalid", "clarification_request does not validate against clarification-request-v1"),
			wantB:      true,
			wantReason: "backend plan.go fails that stage category-B too",
		},
		{
			name:       "unrelated code",
			body:       envelope("validation_failed", "stage_id is not a uuid"),
			wantB:      false,
			wantReason: "a request-shape 400 is NOT the agent's output — stays category-C",
		},
		{
			name:       "message mentions a listed code",
			body:       envelope("validation_failed", "stage_id is not a uuid (this is not a plan_invalid or grooming_report_invalid rejection)"),
			wantB:      false,
			wantReason: "free-text message must not drive classification — error.code is exact-matched",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pf, srv := newPlanFakeBackend(t)
			pf.status = http.StatusBadRequest
			pf.body = tc.body
			c := quickPlanClient(srv)
			priv, _ := makePlanKey(t)

			_, err := c.ShipPlan(context.Background(), ShipPlanArgs{
				RunID:      "r",
				StageID:    "s",
				Plan:       []byte(`{"kind":"grooming_report"}`),
				PrivateKey: priv,
			})
			if err == nil {
				t.Fatal("ShipPlan = nil error, want a 400 failure")
			}
			if got := errors.Is(err, ErrPlanInvalid); got != tc.wantB {
				t.Errorf("errors.Is(err, ErrPlanInvalid) = %t, want %t (%s)\n  err: %v",
					got, tc.wantB, tc.wantReason, err)
			}
			if pf.calls != 1 {
				t.Errorf("calls = %d, want 1 (no retry on 400)", pf.calls)
			}
		})
	}
}

// TestShipPlan_AgentOutputInvalid_UndecodableBodyFallsBackToSubstring pins the
// documented degrade: when the body is NOT the backend error envelope (a
// truncated body, a proxy error page, or the flat {"code":…} shape older
// fixtures use), classification falls back to the historical substring check
// rather than losing category-B on a genuinely-invalid artifact.
func TestShipPlan_AgentOutputInvalid_UndecodableBodyFallsBackToSubstring(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		wantB bool
	}{
		{name: "flat code shape", body: `{"code":"grooming_report_invalid","message":"bad"}`, wantB: true},
		{name: "not json at all", body: `502 Bad Gateway: plan_invalid`, wantB: true},
		{name: "no listed code", body: `502 Bad Gateway from the proxy`, wantB: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pf, srv := newPlanFakeBackend(t)
			pf.status = http.StatusBadRequest
			pf.body = tc.body
			c := quickPlanClient(srv)
			priv, _ := makePlanKey(t)

			_, err := c.ShipPlan(context.Background(), ShipPlanArgs{
				RunID: "r", StageID: "s",
				Plan: []byte(`{"plan_version":"standard_v1"}`), PrivateKey: priv,
			})
			if err == nil {
				t.Fatal("ShipPlan = nil error, want a 400 failure")
			}
			if got := errors.Is(err, ErrPlanInvalid); got != tc.wantB {
				t.Errorf("errors.Is(err, ErrPlanInvalid) = %t, want %t\n  err: %v", got, tc.wantB, err)
			}
		})
	}
}

// TestShipPlan_AgentOutputInvalid_LongMessageStillExactMatched guards the
// classifyBodyLimit split. The envelope is longer than the 256-byte excerpt
// surfaced in the error string, so a classifier reading only that excerpt
// would fail the JSON decode mid-body and fall back to the substring scan —
// and the message here MENTIONS plan_invalid inside the first 256 bytes while
// error.code is validation_failed, so that fallback returns the WRONG answer.
// Shrinking classifyBodyLimit to briefBodyLimit reddens this test.
func TestShipPlan_AgentOutputInvalid_LongMessageStillExactMatched(t *testing.T) {
	long, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":    "validation_failed",
			"message": "stage_id is not a uuid; this is not a plan_invalid rejection " + strings.Repeat("x", 2000),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	pf, srv := newPlanFakeBackend(t)
	pf.status = http.StatusBadRequest
	pf.body = string(long)
	c := quickPlanClient(srv)
	priv, _ := makePlanKey(t)

	_, shipErr := c.ShipPlan(context.Background(), ShipPlanArgs{
		RunID: "r", StageID: "s",
		Plan: []byte(`{"plan_version":"standard_v1"}`), PrivateKey: priv,
	})
	if shipErr == nil {
		t.Fatal("ShipPlan = nil error, want a 400 failure")
	}
	if errors.Is(shipErr, ErrPlanInvalid) {
		t.Errorf("validation_failed misclassified as ErrPlanInvalid: %v", shipErr)
	}
	if len(shipErr.Error()) > 1024 {
		t.Errorf("error message not bounded by the brief excerpt: %d bytes", len(shipErr.Error()))
	}
}

func TestShipPlan_SignatureRejected_401(t *testing.T) {
	pf, srv := newPlanFakeBackend(t)
	pf.status = http.StatusUnauthorized
	pf.body = `{"code":"signature_invalid"}`
	c := quickPlanClient(srv)
	priv, _ := makePlanKey(t)

	_, err := c.ShipPlan(context.Background(), ShipPlanArgs{
		RunID:      "r",
		StageID:    "s",
		Plan:       []byte(`{"plan_version":"standard_v1"}`),
		PrivateKey: priv,
	})
	if !errors.Is(err, ErrSignatureRejected) {
		t.Errorf("err = %v, want ErrSignatureRejected", err)
	}
}

func TestShipPlan_NotFound_404(t *testing.T) {
	pf, srv := newPlanFakeBackend(t)
	pf.status = http.StatusNotFound
	c := quickPlanClient(srv)
	priv, _ := makePlanKey(t)

	_, err := c.ShipPlan(context.Background(), ShipPlanArgs{
		RunID: "r", StageID: "s",
		Plan: []byte(`{}`), PrivateKey: priv,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestShipPlan_RejectsEmptyAndBadKey(t *testing.T) {
	c := New("http://example.com")
	priv, _ := makePlanKey(t)

	_, err := c.ShipPlan(context.Background(), ShipPlanArgs{
		RunID: "r", StageID: "s", Plan: nil, PrivateKey: priv,
	})
	if err == nil || !strings.Contains(err.Error(), "empty plan") {
		t.Errorf("expected empty-plan error, got %v", err)
	}

	_, err = c.ShipPlan(context.Background(), ShipPlanArgs{
		RunID: "r", StageID: "s",
		Plan:       []byte(`{"plan_version":"standard_v1"}`),
		PrivateKey: ed25519.PrivateKey{0x01, 0x02},
	})
	if err == nil || !strings.Contains(err.Error(), "private key") {
		t.Errorf("expected key-length error, got %v", err)
	}
}
