package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
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
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return workmgmt.Default(), nil }
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

// TestWebhook_PullRequestActionRouting_NotApplicable is the
// CROSS-BOUNDARY test for E64.43 / #3160. It POSTs a webhook request the
// test SIGNS ITSELF, in-process, to the REAL POST /webhooks/github route,
// so signature verification → webhook.ParseEvent → the dispatch condition
// → republishOnPullRequestEvent → auditcheckpublisher → the forge fake all
// execute. No live forge and no live delivery are involved.
//
// It is the layer-crossing seam the per-layer unit tests cannot cover:
// every one of them still passes with the dispatch condition narrowed
// back to `synchronize`, because they call the handler directly.
func TestWebhook_PullRequestActionRouting_NotApplicable(t *testing.T) {
	cases := []struct {
		action       string
		wantPublish  int
		wantRouted   bool
		descriptions string
	}{
		{action: "opened", wantPublish: 1, wantRouted: true,
			descriptions: "a Dependabot PR never pushed to after opening must still get the terminal check"},
		{action: "reopened", wantPublish: 1, wantRouted: true,
			descriptions: "reopening re-drives the publish"},
		{action: "synchronize", wantPublish: 1, wantRouted: true,
			descriptions: "the pre-#3160 head-moved trigger"},
		{action: "edited", wantPublish: 0, wantRouted: false,
			descriptions: "a title/body edit does not move the head and must NOT reach the handler"},
		{action: "labeled", wantPublish: 0, wantRouted: false,
			descriptions: "labelling does not move the head and must NOT reach the handler"},
	}
	for i, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			rr := &synchronizeRunRepo{listResult: nil}
			arts := &synchronizeArtifactRepo{}
			gh := newPublisherFakeGitHub()
			store := webhook.NewMemoryStore(0)
			s := New(Config{
				Addr:                "127.0.0.1:0",
				GitHubWebhookSecret: []byte(testSecret),
				WebhookDeliveries:   store,
				RunRepo:             rr,
				ArtifactRepo:        arts,
				AuditRepo:           &synchronizeAuditRepo{},
				ExternalURL:         "https://app.fishhawk.example.com",
			})
			s.appIdentityGetterOverride = &stubAppIdentityGetter{
				app:  &githubclient.App{Slug: "fishhawk-dev"},
				user: &githubclient.User{ID: 12345, Login: ourAppLogin},
			}
			s.auditCheckPublisher = auditcheckpublisher.New(auditcheckpublisher.Deps{
				GitHub: gh, Runs: rr, Artifacts: arts,
				ExternalURL: "https://app.fishhawk.example.com",
			})

			body, err := json.Marshal(map[string]any{
				"action":       tc.action,
				"repository":   map[string]any{"full_name": "x/y"},
				"sender":       map[string]any{"login": "dependabot[bot]", "type": "Bot"},
				"installation": map[string]any{"id": 99},
				"pull_request": map[string]any{
					"html_url": "https://github.com/x/y/pull/7",
					"number":   7,
					"head":     map[string]any{"sha": "cafebabe"},
					"user":     map[string]any{"login": "dependabot[bot]", "type": "Bot"},
				},
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			w := postWebhook(t, s, map[string]string{
				"X-GitHub-Event":      "pull_request",
				"X-GitHub-Delivery":   fmt.Sprintf("00000000-0000-0000-0000-0000000000%02d", i),
				"X-Hub-Signature-256": sign(body),
				"Content-Type":        "application/json",
			}, body)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202 (%s):\n%s", w.Code, tc.descriptions, w.Body.String())
			}

			if got := len(gh.calls()); got != tc.wantPublish {
				t.Fatalf("action %q published %d check run(s), want %d — %s",
					tc.action, got, tc.wantPublish, tc.descriptions)
			}
			rr.mu.Lock()
			routed := len(rr.listURLs) > 0
			rr.mu.Unlock()
			if routed != tc.wantRouted {
				t.Fatalf("action %q reached the handler = %v, want %v — %s",
					tc.action, routed, tc.wantRouted, tc.descriptions)
			}
			if tc.wantPublish > 0 {
				p := gh.calls()[0].params
				if p.Conclusion != githubclient.CheckRunConclusionNeutral {
					t.Errorf("conclusion = %q, want neutral", p.Conclusion)
				}
				if p.Status != githubclient.CheckRunStatusCompleted {
					t.Errorf("status = %q, want completed", p.Status)
				}
				if p.HeadSHA != "cafebabe" {
					t.Errorf("head_sha = %q, want cafebabe", p.HeadSHA)
				}
			}
		})
	}
}
