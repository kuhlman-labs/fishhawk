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

// serverBodyCap mirrors the backend's maxPullRequestBundleBytes /
// maxReapFailureBodyBytes (backend/internal/server/{pullrequest,reap_failure}.go)
// — the exact 32*1024 limit the production endpoints enforce (#1791).
const serverBodyCap = 32 * 1024

// verifyPRSig fails the test if sigHex is not a valid Ed25519 signature over
// sha256(raw) under pub — proving ShipPullRequest re-signed over the exact bytes
// it posted (load-bearing for the aggressive-retry re-sign, #1791).
func verifyPRSig(t *testing.T, pub ed25519.PublicKey, raw []byte, sigHex string) {
	t.Helper()
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	d := sha256.Sum256(raw)
	if !ed25519.Verify(pub, d[:], sig) {
		t.Errorf("signature does not cover the posted body bytes")
	}
}

// TestTruncateReason locks the head+tail elision contract (#1791): under cap the
// input is byte-identical; over cap the result fits max, keeps the head and tail,
// and carries a non-zero elided-bytes marker.
func TestTruncateReason(t *testing.T) {
	t.Run("under cap is byte-identical", func(t *testing.T) {
		s := "category-B: short reason"
		if got := TruncateReason(s, 1024); got != s {
			t.Errorf("TruncateReason(%q) = %q, want byte-identical", s, got)
		}
		exact := strings.Repeat("x", 100)
		if got := TruncateReason(exact, 100); got != exact {
			t.Errorf("at-cap input was mutated: len %d", len(got))
		}
	})
	t.Run("over cap elides middle, keeps head+tail, fits max", func(t *testing.T) {
		head := strings.Repeat("H", 500)
		tail := strings.Repeat("T", 500)
		s := head + strings.Repeat("M", 100*1024) + tail
		const max = 4096
		got := TruncateReason(s, max)
		if len(got) > max {
			t.Fatalf("len(got) = %d, want <= %d", len(got), max)
		}
		if !strings.Contains(got, "truncated") {
			t.Errorf("missing truncation marker: %q", got[:80])
		}
		if strings.Contains(got, "truncated 0 bytes") {
			t.Errorf("marker reports zero elided bytes: %q", got)
		}
		if !strings.HasPrefix(got, "HHHHH") {
			t.Errorf("head not preserved: %q", got[:10])
		}
		if !strings.HasSuffix(got, "TTTTT") {
			t.Errorf("tail not preserved: %q", got[len(got)-10:])
		}
	})
	t.Run("degenerate tiny cap hard-truncates to max", func(t *testing.T) {
		got := TruncateReason(strings.Repeat("x", 100), 5)
		if len(got) != 5 {
			t.Errorf("len(got) = %d, want 5", len(got))
		}
	})
	t.Run("non-positive cap returns empty", func(t *testing.T) {
		if got := TruncateReason("abc", 0); got != "" {
			t.Errorf("got %q, want empty for max=0", got)
		}
	})
}

// TestShipPullRequest_FailedOutcome_OversizedReason_FitsCap is proposal (a): a
// "failed" report whose reason far exceeds 32KB (the #1791 multi-module verify
// dump) is truncated so the POST body fits the cap on the FIRST attempt — no
// 413, no retry — and the shipped reason keeps its head classification + marker.
func TestShipPullRequest_FailedOutcome_OversizedReason_FitsCap(t *testing.T) {
	priv := makePRKey(t)
	pub := priv.Public().(ed25519.PublicKey)
	var calls int
	var gotBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/pull-request", func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		gotBody = raw
		verifyPRSig(t, pub, raw, r.Header.Get("X-Fishhawk-Signature"))
		if len(raw) > serverBodyCap {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = io.WriteString(w, `{"error":{"code":"body_too_large"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ShipPullRequestResult{StageID: r.URL.Query().Get("stage_id")})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New(srv.URL)
	c.MaxRetries = 3
	c.Backoff = time.Millisecond

	reason := "category-B: verify failed\n" + strings.Repeat("x", 200*1024) + "\nFAIL summary tail"
	if _, err := c.ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID: "run-aaa", StageID: "stage-bbb", PrivateKey: priv,
		Outcome: "failed", Category: "B", Reason: reason,
	}); err != nil {
		t.Fatalf("ShipPullRequest: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (truncated body fits on first POST)", calls)
	}
	if len(gotBody) > serverBodyCap {
		t.Fatalf("posted body %d bytes exceeds cap %d", len(gotBody), serverBodyCap)
	}
	var sent pullRequestFailureBody
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("unmarshal failure body: %v", err)
	}
	if sent.Outcome != "failed" || sent.Category != "B" {
		t.Errorf("sent = %+v, want outcome=failed category=B", sent)
	}
	if !strings.Contains(sent.Reason, "truncated") {
		t.Errorf("shipped reason lost the truncation marker (len %d)", len(sent.Reason))
	}
	if !strings.HasPrefix(sent.Reason, "category-B: verify failed") {
		t.Errorf("head classification lost: %q", sent.Reason[:40])
	}
}

// TestShipPullRequest_FailedOutcome_413ThenAggressiveRetry is proposal (b): a
// first POST that 413s drives EXACTLY ONE aggressive-cap retry whose body is
// strictly smaller, still under the cap, re-signed over its own bytes, and
// succeeds.
func TestShipPullRequest_FailedOutcome_413ThenAggressiveRetry(t *testing.T) {
	priv := makePRKey(t)
	pub := priv.Public().(ed25519.PublicKey)
	var bodies [][]byte
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/pull-request", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		verifyPRSig(t, pub, raw, r.Header.Get("X-Fishhawk-Signature"))
		bodies = append(bodies, raw)
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = io.WriteString(w, `{"error":{"code":"body_too_large"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ShipPullRequestResult{StageID: r.URL.Query().Get("stage_id")})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New(srv.URL)
	c.MaxRetries = 3
	c.Backoff = time.Millisecond

	reason := "category-B\n" + strings.Repeat("x", 200*1024)
	if _, err := c.ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID: "r", StageID: "s", PrivateKey: priv,
		Outcome: "failed", Category: "B", Reason: reason,
	}); err != nil {
		t.Fatalf("ShipPullRequest: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("calls = %d, want exactly 2 (413 then one aggressive retry)", len(bodies))
	}
	if len(bodies[1]) >= len(bodies[0]) {
		t.Errorf("aggressive body %d not smaller than first %d", len(bodies[1]), len(bodies[0]))
	}
	if len(bodies[1]) > serverBodyCap {
		t.Errorf("aggressive body %d exceeds cap %d", len(bodies[1]), serverBodyCap)
	}
	var sent pullRequestFailureBody
	if err := json.Unmarshal(bodies[1], &sent); err != nil {
		t.Fatalf("unmarshal aggressive body: %v", err)
	}
	if !strings.Contains(sent.Reason, "truncated") {
		t.Errorf("aggressive reason missing marker: len %d", len(sent.Reason))
	}
}

// TestShipPullRequest_413NonFailedBody_NoRetry asserts the aggressive retry is
// scoped to the "failed" outcome: a 413 on a real PR artifact (empty Outcome)
// fails fast with NO retry — its body is not a truncatable reason.
func TestShipPullRequest_413NonFailedBody_NoRetry(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/pull-request", func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, `{"error":{"code":"body_too_large"}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New(srv.URL)
	c.MaxRetries = 3
	c.Backoff = time.Millisecond
	priv := makePRKey(t)

	if _, err := c.ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID: "r", StageID: "s", Body: []byte(`{"pr_number":1}`), PrivateKey: priv,
	}); err == nil {
		t.Fatal("expected an error for a 413 on a success body")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no aggressive retry for a non-failed body)", calls)
	}
}

// TestShipPullRequest_FailedOutcome_Persistent413_NoLoop asserts the retry is
// bounded: when even the aggressive body 413s, ShipPullRequest surfaces the
// error after EXACTLY ONE retry (initial + one aggressive attempt) — no loop.
func TestShipPullRequest_FailedOutcome_Persistent413_NoLoop(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/pull-request", func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, `{"error":{"code":"body_too_large"}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New(srv.URL)
	c.MaxRetries = 3
	c.Backoff = time.Millisecond
	priv := makePRKey(t)

	if _, err := c.ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID: "r", StageID: "s", PrivateKey: priv,
		Outcome: "failed", Category: "B", Reason: strings.Repeat("x", 200*1024),
	}); err == nil {
		t.Fatal("expected an error after a persistent 413")
	}
	if calls != 2 {
		t.Errorf("calls = %d, want exactly 2 (initial + one aggressive retry, no loop)", calls)
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
