package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

const testSecret = "shhh-its-a-secret"

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newWebhookServer(t *testing.T) (*Server, *webhook.MemoryStore) {
	t.Helper()
	store := webhook.NewMemoryStore(0)
	s := New(Config{
		Addr:                "127.0.0.1:0",
		GitHubWebhookSecret: []byte(testSecret),
		WebhookDeliveries:   store,
	})
	return s, store
}

func postWebhook(t *testing.T, s *Server, headers map[string]string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestWebhook_HappyPath(t *testing.T) {
	s, _ := newWebhookServer(t)
	body := []byte(`{
		"action": "labeled",
		"repository": {"full_name": "kuhlman-labs/fishhawk"},
		"sender": {"login": "kuhlman-labs"}
	}`)
	w := postWebhook(t, s, map[string]string{
		"X-GitHub-Event":      "issues",
		"X-GitHub-Delivery":   "11111111-2222-3333-4444-555555555555",
		"X-Hub-Signature-256": sign(body),
		"Content-Type":        "application/json",
	}, body)
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
}

func TestWebhook_CodeScanningAlertRouted(t *testing.T) {
	// A signed code_scanning_alert delivery is accepted (202) and routed
	// to the ingest, observable here by the PR-URL run lookup the ingest
	// performs while matching the alert to a run (#1096).
	store := webhook.NewMemoryStore(0)
	rr := &codeScanRunRepo{listResult: nil} // no managed run; ingest no-ops after lookup
	s := New(Config{
		Addr:                "127.0.0.1:0",
		GitHubWebhookSecret: []byte(testSecret),
		WebhookDeliveries:   store,
		RunRepo:             rr,
		AuditRepo:           &codeScanAuditRepo{},
	})
	body := codeScanPayload(42, "deadbeef")
	w := postWebhook(t, s, map[string]string{
		"X-GitHub-Event":      "code_scanning_alert",
		"X-GitHub-Delivery":   "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"X-Hub-Signature-256": sign(body),
		"Content-Type":        "application/json",
	}, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	if rr.listCallCount() != 1 || rr.listURLs[0] != "https://github.com/octo/app/pull/42" {
		t.Errorf("ingest run lookup = %+v, want one PR-url lookup (routing reached ingest?)", rr.listURLs)
	}
}

func TestWebhook_BadSignature(t *testing.T) {
	s, _ := newWebhookServer(t)
	body := []byte(`{}`)
	w := postWebhook(t, s, map[string]string{
		"X-GitHub-Event":      "ping",
		"X-GitHub-Delivery":   "deliv",
		"X-Hub-Signature-256": "sha256=" + strings.Repeat("00", 32),
	}, body)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"webhook_signature_invalid"`) {
		t.Errorf("body missing code: %s", w.Body.String())
	}
}

func TestWebhook_MissingSignature(t *testing.T) {
	s, _ := newWebhookServer(t)
	body := []byte(`{}`)
	w := postWebhook(t, s, map[string]string{
		"X-GitHub-Event":    "ping",
		"X-GitHub-Delivery": "deliv",
		// No X-Hub-Signature-256.
	}, body)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for missing sig", w.Code)
	}
}

func TestWebhook_MissingEventHeader(t *testing.T) {
	s, _ := newWebhookServer(t)
	body := []byte(`{}`)
	w := postWebhook(t, s, map[string]string{
		"X-GitHub-Delivery":   "deliv",
		"X-Hub-Signature-256": sign(body),
		// No X-GitHub-Event.
	}, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestWebhook_MissingDeliveryHeader(t *testing.T) {
	s, _ := newWebhookServer(t)
	body := []byte(`{}`)
	w := postWebhook(t, s, map[string]string{
		"X-GitHub-Event":      "ping",
		"X-Hub-Signature-256": sign(body),
		// No X-GitHub-Delivery.
	}, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestWebhook_MalformedBody(t *testing.T) {
	s, _ := newWebhookServer(t)
	body := []byte("{not json")
	w := postWebhook(t, s, map[string]string{
		"X-GitHub-Event":      "issues",
		"X-GitHub-Delivery":   "deliv",
		"X-Hub-Signature-256": sign(body),
	}, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestWebhook_DuplicateDeliveryAcknowledged(t *testing.T) {
	s, _ := newWebhookServer(t)
	body := []byte(`{}`)
	headers := map[string]string{
		"X-GitHub-Event":      "ping",
		"X-GitHub-Delivery":   "deliv-dup",
		"X-Hub-Signature-256": sign(body),
	}
	if w := postWebhook(t, s, headers, body); w.Code != http.StatusAccepted {
		t.Fatalf("first delivery: status = %d, want 202", w.Code)
	}
	// Second delivery with the same ID — must still respond 202
	// because GitHub retries any non-2xx. Refuse-with-error would
	// mean retry storms.
	if w := postWebhook(t, s, headers, body); w.Code != http.StatusAccepted {
		t.Errorf("second delivery: status = %d, want 202", w.Code)
	}
}

func TestWebhook_NoSecretConfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", WebhookDeliveries: webhook.NewMemoryStore(0)})
	w := postWebhook(t, s, map[string]string{
		"X-GitHub-Event":      "ping",
		"X-GitHub-Delivery":   "deliv",
		"X-Hub-Signature-256": "sha256=00",
	}, []byte(`{}`))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "webhook_secret_unconfigured") {
		t.Errorf("body missing code: %s", w.Body.String())
	}
}

func TestWebhook_NoDeliveryStore(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", GitHubWebhookSecret: []byte(testSecret)})
	w := postWebhook(t, s, map[string]string{
		"X-GitHub-Event":      "ping",
		"X-GitHub-Delivery":   "deliv",
		"X-Hub-Signature-256": sign([]byte(`{}`)),
	}, []byte(`{}`))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "webhook_store_unconfigured") {
		t.Errorf("body missing code: %s", w.Body.String())
	}
}

func TestWebhook_BodyTooLarge(t *testing.T) {
	s, _ := newWebhookServer(t)
	body := bytes.Repeat([]byte("a"), maxWebhookBody+1)
	w := postWebhook(t, s, map[string]string{
		"X-GitHub-Event":      "issues",
		"X-GitHub-Delivery":   "deliv",
		"X-Hub-Signature-256": sign(body),
	}, body)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

// TestWebhook_IssueClosedBoardSync drives the #1817 issue-lifecycle reconciler
// end-to-end across the wire -> handleWebhook -> payload decode -> conventions
// -> fake Transitioner -> global work_item_transitioned audit seam: an
// HMAC-signed issues.closed delivery (state_reason completed, an installation
// block) is accepted 202, the provider is dispatched issue_closed with the
// payload's installation id on the Target, and a global-chain audit row records
// the move with the repo + issue number + state_reason. A per-layer-only suite
// would pass while this routing/decode/audit seam silently no-ops (#618).
func TestWebhook_IssueClosedBoardSync(t *testing.T) {
	prev := conventionsLoader
	conventionsLoader = func(string) (workmgmt.Conventions, error) { return workmgmt.Default(), nil }
	t.Cleanup(func() { conventionsLoader = prev })

	fp := &fakeTransitionProvider{result: &workmgmt.TransitionResult{Moved: true, From: "In Progress", To: "Done"}}
	registerTransitionProvider(t, fp)

	au := &campaignAuditRecorder{}
	s := New(Config{
		Addr:                "127.0.0.1:0",
		GitHubWebhookSecret: []byte(testSecret),
		WebhookDeliveries:   webhook.NewMemoryStore(0),
		AuditRepo:           au,
	})

	body := []byte(`{
		"action": "closed",
		"repository": {"full_name": "kuhlman-labs/fishhawk"},
		"sender": {"login": "kuhlman-labs"},
		"installation": {"id": 4242},
		"issue": {"number": 1817, "state_reason": "completed"}
	}`)
	w := postWebhook(t, s, map[string]string{
		"X-GitHub-Event":      "issues",
		"X-GitHub-Delivery":   "cccccccc-dddd-eeee-ffff-000000000000",
		"X-Hub-Signature-256": sign(body),
		"Content-Type":        "application/json",
	}, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	if len(fp.calls) != 1 {
		t.Fatalf("Transition calls = %d, want 1 (routing reached the reconciler?)", len(fp.calls))
	}
	got := fp.calls[0]
	if got.Trigger != lifecycleIssueClosed {
		t.Errorf("trigger = %q, want %q", got.Trigger, lifecycleIssueClosed)
	}
	if got.CanonicalState != workmgmt.CanonicalStateDone {
		t.Errorf("canonical state = %q, want %q", got.CanonicalState, workmgmt.CanonicalStateDone)
	}
	if got.IssueNumber != 1817 {
		t.Errorf("issue number = %d, want 1817", got.IssueNumber)
	}
	if got.Target.Scope != forge.FromGitHubInstallationID(4242) {
		t.Errorf("scope = %q, want scope for installation 4242 (from the payload)", got.Target.Scope.Ref())
	}

	audits := campaignTransitionAudits(au)
	if len(audits) != 1 {
		t.Fatalf("global work_item_transitioned audits = %d, want 1", len(audits))
	}
	a := audits[0]
	if a["moved"] != true || a["trigger"] != lifecycleIssueClosed {
		t.Errorf("audit = %v, want moved=true trigger=issue_closed", a)
	}
	if a["repo"] != "kuhlman-labs/fishhawk" || a["issue_number"] != float64(1817) || a["state_reason"] != "completed" {
		t.Errorf("audit = %v, want repo/issue_number/state_reason from the payload", a)
	}
}
