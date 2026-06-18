package upload

import (
	"context"
	"crypto/ed25519"
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

// prFakeBackend mimics POST /v0/runs/{run_id}/pull-request with a
// configurable response. Separate from the trace/plan fake backends
// to keep per-test plumbing focused.
type prFakeBackend struct {
	mu sync.Mutex

	status     int
	body       string
	errCount   int
	idempotent bool

	receivedBody []byte
	receivedSig  string
	receivedPath string
	calls        int
}

func newPRFakeBackend(t *testing.T) (*prFakeBackend, *httptest.Server) {
	t.Helper()
	pf := &prFakeBackend{status: http.StatusCreated}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/pull-request", func(w http.ResponseWriter, r *http.Request) {
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
			_ = json.NewEncoder(w).Encode(ShipPullRequestResult{
				ID:          "00000000-0000-0000-0000-000000000bbb",
				StageID:     r.URL.Query().Get("stage_id"),
				ContentHash: hex.EncodeToString(func() []byte { d := sha256.Sum256(raw); return d[:] }()),
				PRNumber:    42,
				PRURL:       "https://github.com/x/y/pull/42",
				HeadSHA:     "abc",
				Idempotent:  idem,
			})
		} else if body != "" {
			_, _ = io.WriteString(w, body)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return pf, srv
}

func quickPRClient(srv *httptest.Server) *Client {
	c := New(srv.URL)
	c.MaxRetries = 3
	c.Backoff = time.Millisecond
	return c
}

func makePRKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func TestShipPullRequest_HappyPath_Created(t *testing.T) {
	pf, srv := newPRFakeBackend(t)
	c := quickPRClient(srv)
	priv := makePRKey(t)
	body := []byte(`{"pr_number":42,"pr_url":"https://x/p/42"}`)

	res, err := c.ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID:      "run-aaa",
		StageID:    "stage-bbb",
		Body:       body,
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("ShipPullRequest: %v", err)
	}
	if res.PRNumber != 42 {
		t.Errorf("PRNumber = %d", res.PRNumber)
	}
	if res.Idempotent {
		t.Error("expected Idempotent=false on 201")
	}
	if pf.calls != 1 {
		t.Errorf("calls = %d, want 1", pf.calls)
	}
	if pf.receivedPath != "/v0/runs/run-aaa/pull-request" {
		t.Errorf("path = %q", pf.receivedPath)
	}
	digest := sha256.Sum256(body)
	wantSig := hex.EncodeToString(ed25519.Sign(priv, digest[:]))
	if pf.receivedSig != wantSig {
		t.Error("signature mismatch")
	}
}

func TestShipPullRequest_Idempotent_200(t *testing.T) {
	pf, srv := newPRFakeBackend(t)
	pf.status = http.StatusOK
	pf.idempotent = true
	c := quickPRClient(srv)
	priv := makePRKey(t)

	res, err := c.ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID:      "r",
		StageID:    "s",
		Body:       []byte(`{"pr_number":1}`),
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("ShipPullRequest: %v", err)
	}
	if !res.Idempotent {
		t.Error("expected Idempotent=true on 200")
	}
}

func TestShipPullRequest_RetriesOn5xx(t *testing.T) {
	pf, srv := newPRFakeBackend(t)
	pf.errCount = 2
	c := quickPRClient(srv)
	priv := makePRKey(t)

	if _, err := c.ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID: "r", StageID: "s",
		Body:       []byte(`{"pr_number":1}`),
		PrivateKey: priv,
	}); err != nil {
		t.Fatalf("ShipPullRequest: %v", err)
	}
	if pf.calls != 3 {
		t.Errorf("calls = %d, want 3 (2 retries + success)", pf.calls)
	}
}

func TestShipPullRequest_PRInvalid_400(t *testing.T) {
	pf, srv := newPRFakeBackend(t)
	pf.status = http.StatusBadRequest
	pf.body = `{"code":"pull_request_invalid","message":"missing pr_number"}`
	c := quickPRClient(srv)
	priv := makePRKey(t)

	_, err := c.ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID: "r", StageID: "s",
		Body:       []byte(`{"foo":"bar"}`),
		PrivateKey: priv,
	})
	if !errors.Is(err, ErrPullRequestInvalid) {
		t.Errorf("err = %v, want ErrPullRequestInvalid", err)
	}
	if pf.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 400)", pf.calls)
	}
}

func TestShipPullRequest_SignatureRejected_401(t *testing.T) {
	pf, srv := newPRFakeBackend(t)
	pf.status = http.StatusUnauthorized
	c := quickPRClient(srv)
	priv := makePRKey(t)

	_, err := c.ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID: "r", StageID: "s",
		Body:       []byte(`{}`),
		PrivateKey: priv,
	})
	if !errors.Is(err, ErrSignatureRejected) {
		t.Errorf("err = %v, want ErrSignatureRejected", err)
	}
}

func TestShipPullRequest_NotFound_404(t *testing.T) {
	pf, srv := newPRFakeBackend(t)
	pf.status = http.StatusNotFound
	c := quickPRClient(srv)
	priv := makePRKey(t)

	_, err := c.ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID: "r", StageID: "s",
		Body:       []byte(`{}`),
		PrivateKey: priv,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestShipPullRequest_RejectsBadInputs(t *testing.T) {
	c := New("http://example.com")
	priv := makePRKey(t)

	if _, err := c.ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID: "r", StageID: "s", Body: nil, PrivateKey: priv,
	}); err == nil || !strings.Contains(err.Error(), "empty pull-request") {
		t.Errorf("expected empty-body error, got %v", err)
	}
	if _, err := c.ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID: "r", StageID: "s",
		Body:       []byte(`{}`),
		PrivateKey: ed25519.PrivateKey{1, 2, 3},
	}); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Errorf("expected key-length error, got %v", err)
	}
}

// TestShipPullRequest_ChildPushOutcome_MarshalsPushBody confirms the #771
// child-push success report: Outcome=="pushed" causes ShipPullRequest to
// build the {outcome:"pushed", branch, head_sha, base_sha, files_changed_count}
// body from the args (ignoring the absent success Body) and sign over those
// bytes — no PR artifact.
func TestShipPullRequest_ChildPushOutcome_MarshalsPushBody(t *testing.T) {
	pf, srv := newPRFakeBackend(t)
	pf.status = http.StatusOK
	pf.body = `{"stage_id":"stage-bbb","outcome":"pushed","branch":"fishhawk/run-aaa","head_sha":"head-abc"}`
	c := quickPRClient(srv)
	priv := makePRKey(t)

	if _, err := c.ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID:             "run-aaa",
		StageID:           "stage-bbb",
		PrivateKey:        priv,
		Outcome:           "pushed",
		Branch:            "fishhawk/run-aaa",
		HeadSHA:           "head-abc",
		BaseSHA:           "base-def",
		FilesChangedCount: 3,
	}); err != nil {
		t.Fatalf("ShipPullRequest: %v", err)
	}

	var sent pullRequestChildPushBody
	if err := json.Unmarshal(pf.receivedBody, &sent); err != nil {
		t.Fatalf("unmarshal received body: %v", err)
	}
	want := pullRequestChildPushBody{
		Outcome: "pushed", Branch: "fishhawk/run-aaa",
		HeadSHA: "head-abc", BaseSHA: "base-def", FilesChangedCount: 3,
	}
	if sent != want {
		t.Errorf("sent body = %+v, want %+v", sent, want)
	}
	// Signature must cover the marshalled push body, not the empty success Body.
	digest := sha256.Sum256(pf.receivedBody)
	wantSig := hex.EncodeToString(ed25519.Sign(priv, digest[:]))
	if pf.receivedSig != wantSig {
		t.Error("signature must cover the child-push body bytes")
	}
}

// TestShipPullRequest_FixupPushedOutcome_MarshalsApplyPath asserts the #1165/#1213
// apply provenance wire field: a fixup_pushed report with ApplyPath set carries
// apply_path in the marshalled body, while a "pushed" child-push that shares the
// same pullRequestChildPushBody but leaves ApplyPath unset omits the key entirely
// (omitempty) — keeping the child-push body byte-identical to its pre-#1213 shape.
func TestShipPullRequest_FixupPushedOutcome_MarshalsApplyPath(t *testing.T) {
	priv := makePRKey(t)

	// fixup_pushed with ApplyPath set: the key is present and carries the value.
	pf, srv := newPRFakeBackend(t)
	pf.status = http.StatusOK
	pf.body = `{"stage_id":"stage-bbb","outcome":"fixup_pushed","branch":"fishhawk/run-aaa","head_sha":"head-abc"}`
	c := quickPRClient(srv)
	if _, err := c.ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID:             "run-aaa",
		StageID:           "stage-bbb",
		PrivateKey:        priv,
		Outcome:           "fixup_pushed",
		Branch:            "fishhawk/run-aaa",
		HeadSHA:           "head-abc",
		BaseSHA:           "base-def",
		FilesChangedCount: 2,
		ApplyPath:         "applied",
	}); err != nil {
		t.Fatalf("ShipPullRequest (fixup_pushed): %v", err)
	}
	if !strings.Contains(string(pf.receivedBody), `"apply_path":"applied"`) {
		t.Errorf("fixup_pushed body must carry apply_path: %s", pf.receivedBody)
	}
	var sent pullRequestChildPushBody
	if err := json.Unmarshal(pf.receivedBody, &sent); err != nil {
		t.Fatalf("unmarshal fixup_pushed body: %v", err)
	}
	if sent.ApplyPath != "applied" {
		t.Errorf("decoded ApplyPath = %q, want applied", sent.ApplyPath)
	}

	// A "pushed" child-push with no ApplyPath: omitempty drops the key, so the
	// body stays byte-identical to the pre-#1213 child-push shape.
	pf2, srv2 := newPRFakeBackend(t)
	pf2.status = http.StatusOK
	pf2.body = `{"stage_id":"stage-bbb","outcome":"pushed","branch":"fishhawk/run-aaa","head_sha":"head-abc"}`
	c2 := quickPRClient(srv2)
	if _, err := c2.ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID:             "run-aaa",
		StageID:           "stage-bbb",
		PrivateKey:        priv,
		Outcome:           "pushed",
		Branch:            "fishhawk/run-aaa",
		HeadSHA:           "head-abc",
		BaseSHA:           "base-def",
		FilesChangedCount: 3,
	}); err != nil {
		t.Fatalf("ShipPullRequest (pushed): %v", err)
	}
	if strings.Contains(string(pf2.receivedBody), "apply_path") {
		t.Errorf("child-push body must omit apply_path when unset (omitempty): %s", pf2.receivedBody)
	}
}
