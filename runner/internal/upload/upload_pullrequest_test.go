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

// TestShipPullRequest_FailedBodyCarriesCheckpoint pins the PR-open checkpoint
// wire shape (#2169): a failed ship whose caller supplied the pushed
// coordinates POSTs a body carrying all four of them, so the backend can record
// a push_checkpoint and a later retry_stage can resume the PR open with no
// agent re-invocation.
func TestShipPullRequest_FailedBodyCarriesCheckpoint(t *testing.T) {
	pf, srv := newPRFakeBackend(t)
	priv := makePRKey(t)
	if _, err := quickPRClient(srv).ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID: "r", StageID: "s", PrivateKey: priv,
		Outcome: "failed", Category: "C", Reason: "open PR: 503 from the forge",
		Branch: "fishhawk/run-2169/stage-abc", HeadSHA: "headsha2169", BaseSHA: "basesha2169",
		TreeSHA: "treesha2169",
	}); err != nil {
		t.Fatalf("ShipPullRequest: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal(pf.receivedBody, &sent); err != nil {
		t.Fatalf("unmarshal failure body: %v", err)
	}
	for key, want := range map[string]string{
		"outcome":           "failed",
		"category":          "C",
		"branch":            "fishhawk/run-2169/stage-abc",
		"head_sha":          "headsha2169",
		"base_sha":          "basesha2169",
		"verified_tree_sha": "treesha2169",
	} {
		if got, _ := sent[key].(string); got != want {
			t.Errorf("failure body %s = %q, want %q (full body: %s)", key, got, want, pf.receivedBody)
		}
	}
}

// TestShipPullRequest_FailedBodyByteIdenticalWithoutCheckpoint is the
// omitempty control (#2169): a failed ship carrying NONE of the checkpoint
// coordinates — every PRE-push failure — must marshal the EXACT legacy
// {outcome,category,reason} bytes. Asserted on the bytes rather than on a
// decoded struct, because the signed digest covers the bytes: a stray
// `"branch":""` would change every pre-push failure report's signature and its
// stored payload for no reason.
func TestShipPullRequest_FailedBodyByteIdenticalWithoutCheckpoint(t *testing.T) {
	pf, srv := newPRFakeBackend(t)
	priv := makePRKey(t)
	if _, err := quickPRClient(srv).ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID: "r", StageID: "s", PrivateKey: priv,
		Outcome: "failed", Category: "B", Reason: "commit would not compile",
	}); err != nil {
		t.Fatalf("ShipPullRequest: %v", err)
	}
	const want = `{"outcome":"failed","category":"B","reason":"commit would not compile"}`
	if got := string(pf.receivedBody); got != want {
		t.Errorf("pre-push failure body must be byte-identical to the legacy shape\n got: %s\nwant: %s", got, want)
	}
}

// TestShipPullRequest_AggressiveRetryPreservesCheckpoint pins the SECOND
// marshal site (#2169): the bounded post-4xx aggressive re-marshal rebuilds the
// failure body from scratch, so it must re-carry the checkpoint. Dropping it
// there would make a 413-retried failure report silently un-resumable and fall
// the retry back to a full agent re-run. Also verifies the re-marshalled bytes'
// signature, since the retry re-signs.
func TestShipPullRequest_AggressiveRetryPreservesCheckpoint(t *testing.T) {
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

	if _, err := c.ShipPullRequest(context.Background(), ShipPullRequestArgs{
		RunID: "r", StageID: "s", PrivateKey: priv,
		Outcome: "failed", Category: "C", Reason: "open PR: " + strings.Repeat("x", 200*1024),
		Branch: "fishhawk/run-2169/stage-abc", HeadSHA: "headsha2169", BaseSHA: "basesha2169",
		TreeSHA: "treesha2169",
	}); err != nil {
		t.Fatalf("ShipPullRequest: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("calls = %d, want exactly 2 (413 then one aggressive retry)", len(bodies))
	}
	var sent pullRequestFailureBody
	if err := json.Unmarshal(bodies[1], &sent); err != nil {
		t.Fatalf("unmarshal aggressive body: %v", err)
	}
	if sent.Branch != "fishhawk/run-2169/stage-abc" || sent.HeadSHA != "headsha2169" {
		t.Errorf("aggressive retry dropped the checkpoint: branch=%q head_sha=%q", sent.Branch, sent.HeadSHA)
	}
	if sent.BaseSHA != "basesha2169" || sent.VerifiedTreeSHA != "treesha2169" {
		t.Errorf("aggressive retry dropped base/tree: base_sha=%q verified_tree_sha=%q", sent.BaseSHA, sent.VerifiedTreeSHA)
	}
}

// pushReportOutcomes is exactly the set that rides the request-local budget.
var pushReportOutcomes = []string{"fixup_pushed", "conflict_resolution_pushed", OutcomeAcceptanceScenariosPushed}

// pushReportStub answers the pull-request route from a status script (the last
// entry repeats) and the trace route with a 202, counting both. A hanging stub
// holds each pull-request answer until the request's context ends or the test
// cleans up.
type pushReportStub struct {
	mu         sync.Mutex
	statuses   []int
	prCalls    int
	traceCalls int
	hang       bool
	dropConn   int           // the first dropConn pull-request attempts drop the connection
	onPR       func(int)     // called with the attempt number, before the answer
	release    chan struct{} // closed at cleanup so a hanging handler returns
}

func newPushReportStub(t *testing.T, statuses ...int) (*pushReportStub, *Client) {
	t.Helper()
	st := &pushReportStub{statuses: statuses, release: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/pull-request", func(w http.ResponseWriter, r *http.Request) {
		// Reading the body to EOF is what lets net/http notice the client's
		// disconnect and end r.Context() while the handler is still holding.
		_, _ = io.Copy(io.Discard, r.Body)
		st.mu.Lock()
		st.prCalls++
		attempt := st.prCalls
		status := http.StatusOK
		if len(st.statuses) > 0 {
			i := attempt - 1
			if i >= len(st.statuses) {
				i = len(st.statuses) - 1
			}
			status = st.statuses[i]
		}
		hang, drop, onPR := st.hang, attempt <= st.dropConn, st.onPR
		st.mu.Unlock()
		if onPR != nil {
			onPR(attempt)
		}
		if drop {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		if hang {
			select {
			case <-r.Context().Done():
			case <-st.release:
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(ShipPullRequestResult{StageID: "s"})
		}
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/trace", func(w http.ResponseWriter, _ *http.Request) {
		st.mu.Lock()
		st.traceCalls++
		st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(ShipResult{RunID: "r", StageID: "s", Variant: "raw", ContentHash: "h"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(st.release) }) // runs first, so srv.Close cannot wait on a holder
	c := New(srv.URL)
	// The blip budget is four attempts here, deliberately fewer than the
	// push-report budget's six, so a report on the wrong budget is visible.
	c.MaxRetries = 3
	c.Backoff = time.Millisecond
	c.PushReportBackoff = time.Millisecond
	c.PushReportBackoffCap = time.Millisecond
	return st, c
}

func (s *pushReportStub) attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prCalls
}

func (s *pushReportStub) traces() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.traceCalls
}

func pushReportArgs(t *testing.T, outcome string) ShipPullRequestArgs {
	t.Helper()
	return ShipPullRequestArgs{
		RunID: "r", StageID: "s", PrivateKey: makePRKey(t), Outcome: outcome,
		Branch: "fishhawk/run-r", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40),
	}
}

func shipTraceArgs(t *testing.T) ShipArgs {
	t.Helper()
	return ShipArgs{RunID: "r", StageID: "s", Variant: "raw", Bundle: []byte("b"), PrivateKey: makePRKey(t)}
}

// isolationBudgets are the probe's values: a report runs at least 1.2s, longer
// than the 500ms settling window of the 1s terminal phase, so a report that
// armed that phase would leave a later ShipTrace born expired.
func isolationBudgets(c *Client) {
	c.TerminalEgressBudget = time.Second
	c.PushReportBackoff = 600 * time.Millisecond
	c.PushReportBackoffCap = 600 * time.Millisecond
	c.PushReportMaxRetries = 2
}

// TestShipPullRequest_ExhaustedPushReportThenShipTraceSucceeds is the
// client-level isolation probe for any later terminal-egress call (ADR-141 D4).
// It proves isolation and says nothing about run()'s order. Swapping the
// request-local context for the settling one arms the 1s phase, the report
// spends its 500ms settling window, and ShipTrace joins an expired phase and
// makes no request.
func TestShipPullRequest_ExhaustedPushReportThenShipTraceSucceeds(t *testing.T) {
	st, c := newPushReportStub(t, http.StatusServiceUnavailable)
	isolationBudgets(c)

	_, err := c.ShipPullRequest(context.Background(), pushReportArgs(t, "fixup_pushed"))
	if err == nil {
		t.Errorf("assert: report against a persistent 503 returned no error")
	}
	if got := st.attempts(); got != 3 {
		t.Errorf("assert: report attempts = %d, want 3", got)
	}
	if got := c.terminalEgressDeadline.Load(); got != 0 {
		t.Errorf("assert: terminal-egress deadline = %d after the report, want 0 (never armed)", got)
	}
	res, err := c.ShipTrace(context.Background(), shipTraceArgs(t))
	if err != nil {
		t.Errorf("assert: ShipTrace after an exhausted report: %v", err)
	} else if res.RunID != "r" {
		t.Errorf("assert: ShipTrace result = %+v, want the stub's", res)
	}
	if got := st.traces(); got != 1 {
		t.Errorf("assert: trace requests = %d, want 1", got)
	}
}

// TestShipPullRequest_PermanentFailedPushReportThenShipTraceSucceeds is the
// failed arm of the probe. A rejected report takes no time, so the pause runs
// the clock past the settling window a wrongly armed phase would have opened:
// without it the later ShipTrace would succeed either way.
func TestShipPullRequest_PermanentFailedPushReportThenShipTraceSucceeds(t *testing.T) {
	st, c := newPushReportStub(t, http.StatusConflict)
	isolationBudgets(c)

	if _, err := c.ShipPullRequest(context.Background(), pushReportArgs(t, "conflict_resolution_pushed")); err == nil {
		t.Errorf("assert: report answered 409 returned no error")
	}
	if got := st.attempts(); got != 1 {
		t.Errorf("assert: report attempts = %d, want 1", got)
	}
	if got := c.terminalEgressDeadline.Load(); got != 0 {
		t.Errorf("assert: terminal-egress deadline = %d after the report, want 0 (never armed)", got)
	}
	time.Sleep(600 * time.Millisecond)
	if _, err := c.ShipTrace(context.Background(), shipTraceArgs(t)); err != nil {
		t.Errorf("assert: ShipTrace after a rejected report: %v", err)
	}
	if got := st.traces(); got != 1 {
		t.Errorf("assert: trace requests = %d, want 1", got)
	}
}

// TestShipPullRequest_PushReports_RideLocalRetryBudget rides out more failures
// than the blip budget's four attempts, on the default six.
func TestShipPullRequest_PushReports_RideLocalRetryBudget(t *testing.T) {
	for _, outcome := range pushReportOutcomes {
		t.Run(outcome, func(t *testing.T) {
			u := http.StatusServiceUnavailable
			st, c := newPushReportStub(t, u, u, u, u, u, http.StatusOK)
			if _, err := c.ShipPullRequest(context.Background(), pushReportArgs(t, outcome)); err != nil {
				t.Errorf("assert: report after five 503s: %v", err)
			}
			if got := st.attempts(); got != 6 {
				t.Errorf("assert: attempts = %d, want 6", got)
			}
		})
	}
	t.Run("network_errors_retry", func(t *testing.T) {
		st, c := newPushReportStub(t, http.StatusOK)
		st.dropConn = 5
		if _, err := c.ShipPullRequest(context.Background(), pushReportArgs(t, "fixup_pushed")); err != nil {
			t.Errorf("assert: report after five dropped connections: %v", err)
		}
		if got := st.attempts(); got != 6 {
			t.Errorf("assert: attempts = %d, want 6", got)
		}
	})
}

func TestShipPullRequest_PushReports_SuccessOneAttempt(t *testing.T) {
	for _, outcome := range pushReportOutcomes {
		t.Run(outcome, func(t *testing.T) {
			st, c := newPushReportStub(t, http.StatusOK)
			res, err := c.ShipPullRequest(context.Background(), pushReportArgs(t, outcome))
			if err != nil {
				t.Fatalf("assert: ShipPullRequest: %v", err)
			}
			if res.StageID != "s" {
				t.Errorf("assert: result = %+v, want the stub's", res)
			}
			if got := st.attempts(); got != 1 {
				t.Errorf("assert: attempts = %d, want 1", got)
			}
		})
	}
}

// TestShipPullRequest_PushReports_PermanentFailureNoRetry: the new budget changes
// no status class. 409 is a mismatch refusal, so a retry could not change it.
func TestShipPullRequest_PushReports_PermanentFailureNoRetry(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{status: http.StatusConflict},
		{status: http.StatusBadRequest},
		{status: http.StatusUnauthorized, want: ErrSignatureRejected},
		{status: http.StatusNotFound, want: ErrNotFound},
	} {
		for _, outcome := range pushReportOutcomes {
			t.Run(http.StatusText(tc.status)+"/"+outcome, func(t *testing.T) {
				st, c := newPushReportStub(t, tc.status)
				_, err := c.ShipPullRequest(context.Background(), pushReportArgs(t, outcome))
				if err == nil {
					t.Fatalf("assert: status %d returned no error", tc.status)
				}
				if tc.want != nil && !errors.Is(err, tc.want) {
					t.Errorf("assert: err = %v, want %v", err, tc.want)
				}
				if got := st.attempts(); got != 1 {
					t.Errorf("assert: attempts = %d, want 1", got)
				}
			})
		}
	}
}

// TestShipPullRequest_PushReports_ExhaustionIsBounded pins N+1 attempts, the
// wrapped last error and the backoff cap: the uncapped series would sleep 3s
// where the capped one sleeps 800ms.
func TestShipPullRequest_PushReports_ExhaustionIsBounded(t *testing.T) {
	st, c := newPushReportStub(t, http.StatusServiceUnavailable)
	c.PushReportMaxRetries = 4
	c.PushReportBackoff = 200 * time.Millisecond
	c.PushReportBackoffCap = 200 * time.Millisecond

	start := time.Now()
	_, err := c.ShipPullRequest(context.Background(), pushReportArgs(t, "fixup_pushed"))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("assert: a persistent 503 returned no error")
	}
	if got := st.attempts(); got != 5 {
		t.Errorf("assert: attempts = %d, want 5", got)
	}
	if !strings.Contains(err.Error(), "exhausted retries") || !strings.Contains(err.Error(), "503") {
		t.Errorf("assert: err = %v, want exhausted retries wrapping the 503", err)
	}
	if elapsed > 2500*time.Millisecond {
		t.Errorf("assert: elapsed = %v, want the capped 800ms of sleep", elapsed)
	}
}

// TestShipPullRequest_PushReports_ContextCancelled cancels the caller during a
// backoff far longer than the test. Detaching the local context from the caller
// would hold that backoff until the 2s budget and answer DeadlineExceeded.
func TestShipPullRequest_PushReports_ContextCancelled(t *testing.T) {
	st, c := newPushReportStub(t, http.StatusServiceUnavailable)
	c.PushReportBackoff = time.Minute
	c.PushReportBackoffCap = time.Minute
	c.PushReportBudget = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st.onPR = func(int) { time.AfterFunc(50*time.Millisecond, cancel) }

	_, err := c.ShipPullRequest(ctx, pushReportArgs(t, "fixup_pushed"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("assert: err = %v, want context.Canceled", err)
	}
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("assert: err = %v, want the last 503 kept beside the cancellation", err)
	}
	if got := st.attempts(); got != 1 {
		t.Errorf("assert: attempts = %d, want 1", got)
	}
}

// TestShipPullRequest_PushReports_LocalDeadlineBounds: the budget itself stops a
// hanging request while the caller's context stays live.
func TestShipPullRequest_PushReports_LocalDeadlineBounds(t *testing.T) {
	st, c := newPushReportStub(t, http.StatusOK)
	st.hang = true
	c.PushReportBudget = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	args := pushReportArgs(t, "acceptance_scenarios_pushed")
	done := make(chan error, 1)
	go func() {
		_, err := c.ShipPullRequest(ctx, args)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("assert: err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(3 * time.Second):
		t.Errorf("assert: a 50ms budget did not bound a hanging report within 3s")
	}
	if err := ctx.Err(); err != nil {
		t.Errorf("assert: caller context = %v, want it still live", err)
	}
}

// TestShipPullRequest_PushReports_CallerEarlierDeadlineWins: a 100ms caller
// deadline beats the 45s budget, on this test's own 3s bound.
func TestShipPullRequest_PushReports_CallerEarlierDeadlineWins(t *testing.T) {
	st, c := newPushReportStub(t, http.StatusOK)
	st.hang = true
	c.PushReportBudget = 45 * time.Second
	c.PushReportMaxRetries = 1
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	args := pushReportArgs(t, "conflict_resolution_pushed")
	done := make(chan error, 1)
	go func() {
		_, err := c.ShipPullRequest(ctx, args)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("assert: err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(3 * time.Second):
		t.Errorf("assert: the caller's 100ms deadline did not bound the report within 3s")
	}
}

// TestShipPullRequest_PushReport_LeavesArmedPhaseUntouched: a landed push report
// neither closes nor moves a phase another terminal call opened.
func TestShipPullRequest_PushReport_LeavesArmedPhaseUntouched(t *testing.T) {
	_, c := newPushReportStub(t, http.StatusOK)
	armed := c.terminalEgressPhaseDeadline().UnixNano()
	if _, err := c.ShipPullRequest(context.Background(), pushReportArgs(t, "fixup_pushed")); err != nil {
		t.Fatalf("setup: report: %v", err)
	}
	if got := c.terminalEgressDeadline.Load(); got != armed {
		t.Errorf("assert: terminal-egress deadline = %d, want %d unchanged", got, armed)
	}
}

// TestShipPullRequest_OtherOutcomes_KeepShortBudget is the narrowness control.
// The push-report values are set so a widened budget changes both the attempt
// count (seven) and the sleep (a 10ms cap would give 30ms, not the blip's 70ms).
func TestShipPullRequest_OtherOutcomes_KeepShortBudget(t *testing.T) {
	for _, outcome := range []string{"", "pushed", "failed", "fixup_no_changes", OutcomeAcceptanceScenarioRetirementDropped} {
		t.Run("outcome="+outcome, func(t *testing.T) {
			st, c := newPushReportStub(t, http.StatusServiceUnavailable)
			c.Backoff = 10 * time.Millisecond
			c.PushReportMaxRetries = 6
			c.PushReportBackoff = 10 * time.Millisecond
			c.PushReportBackoffCap = 10 * time.Millisecond
			args := pushReportArgs(t, outcome)
			args.Body = []byte(`{"pr_number":1}`)

			start := time.Now()
			_, err := c.ShipPullRequest(context.Background(), args)
			elapsed := time.Since(start)
			if err == nil {
				t.Errorf("assert: a persistent 503 returned no error")
			}
			if got := st.attempts(); got != 4 {
				t.Errorf("assert: attempts = %d, want the blip budget's 4", got)
			}
			if elapsed < 70*time.Millisecond {
				t.Errorf("assert: elapsed = %v, want at least the doubling series' 70ms", elapsed)
			}
		})
	}
}

// TestPushReportBudget_DefaultSeriesFitsBudget is a sleep-free check on the
// shipped constants: 1,2,4,8,15 is 30s of sleep across 6 attempts, inside 45s,
// so a quiet change to any of them goes red here.
func TestPushReportBudget_DefaultSeriesFitsBudget(t *testing.T) {
	maxRetries, backoff, backoffCap := New("http://unused").pushReportRetryPolicy()
	if maxRetries != 5 || backoff != time.Second || backoffCap != 15*time.Second {
		t.Fatalf("assert: policy = %d/%v/%v, want 5/1s/15s", maxRetries, backoff, backoffCap)
	}
	var sleep time.Duration
	for i := 0; i < maxRetries; i++ {
		sleep += backoff
		backoff = capBackoff(backoff, backoffCap)
	}
	if sleep != 30*time.Second {
		t.Errorf("assert: total sleep = %v, want 30s", sleep)
	}
	if DefaultPushReportBudget != 45*time.Second {
		t.Errorf("assert: budget = %v, want 45s", DefaultPushReportBudget)
	}
	if sleep >= DefaultPushReportBudget {
		t.Errorf("assert: sleep %v leaves the attempts no time inside %v", sleep, DefaultPushReportBudget)
	}
}
