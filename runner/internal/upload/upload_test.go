package upload

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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

// fakeBackend builds a httptest.Server with handlers that mimic the
// production endpoints' shape. Tests can drive each handler's
// behavior via fields on fakeBackend.
type fakeBackend struct {
	mu sync.Mutex

	// signing-key handler config
	issueStatus   int
	issueResponse signingKeyResponse
	issueErrCount int // forces N consecutive 500s before success

	// trace handler config
	shipStatus    int
	shipBody      string // optional response body override
	shipErrCount  int    // N consecutive 500s before success
	receivedBody  []byte
	receivedSig   string
	receivedQuery string
	calls         int

	// prompt handler config
	promptStatus       int
	promptBody         string // canned response body; if empty, default JSON is built
	promptErrCount     int
	promptReceivedSig  string
	promptReceivedPath string
	promptCalls        int
}

func newFakeBackend(t *testing.T) (*fakeBackend, *httptest.Server) {
	t.Helper()
	fb := &fakeBackend{
		issueStatus:  http.StatusCreated,
		shipStatus:   http.StatusAccepted,
		promptStatus: http.StatusOK,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/signing-key", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		if fb.issueErrCount > 0 {
			fb.issueErrCount--
			fb.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		s := fb.issueStatus
		resp := fb.issueResponse
		if resp.RunID == "" {
			resp.RunID = r.PathValue("run_id")
		}
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s)
		if s == http.StatusCreated {
			_ = json.NewEncoder(w).Encode(resp)
		}
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/trace", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.calls++
		if fb.shipErrCount > 0 {
			fb.shipErrCount--
			fb.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		s := fb.shipStatus
		body := fb.shipBody
		fb.mu.Unlock()

		// Capture the request for assertions.
		raw, _ := io.ReadAll(r.Body)
		fb.mu.Lock()
		fb.receivedBody = raw
		fb.receivedSig = r.Header.Get("X-Fishhawk-Signature")
		fb.receivedQuery = r.URL.RawQuery
		fb.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s)
		if s == http.StatusAccepted && body == "" {
			_ = json.NewEncoder(w).Encode(ShipResult{
				RunID:       r.PathValue("run_id"),
				StageID:     r.URL.Query().Get("stage_id"),
				Variant:     r.URL.Query().Get("variant"),
				ContentHash: hex.EncodeToString(func() []byte { d := sha256.Sum256(raw); return d[:] }()),
			})
		} else if body != "" {
			_, _ = io.WriteString(w, body)
		}
	})
	mux.HandleFunc("GET /v0/stages/{stage_id}/prompt", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.promptCalls++
		if fb.promptErrCount > 0 {
			fb.promptErrCount--
			fb.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		s := fb.promptStatus
		body := fb.promptBody
		fb.promptReceivedSig = r.Header.Get("X-Fishhawk-Signature")
		fb.promptReceivedPath = r.URL.Path
		fb.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s)
		if s == http.StatusOK && body == "" {
			stageID := r.PathValue("stage_id")
			_ = json.NewEncoder(w).Encode(FetchedPrompt{
				StageID:    stageID,
				StageType:  "implement",
				Prompt:     "test prompt body",
				PromptHash: hex.EncodeToString(func() []byte { d := sha256.Sum256([]byte("test prompt body")); return d[:] }()),
			})
		} else if body != "" {
			_, _ = io.WriteString(w, body)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

// quickClient returns a Client that retries fast so tests don't
// pay backoff time. The production defaults are unchanged.
func quickClient(srv *httptest.Server) *Client {
	c := New(srv.URL)
	c.MaxRetries = 3
	c.Backoff = time.Millisecond
	return c
}

// makeKey generates a fresh keypair and pre-loads it into the fake
// backend's issue response so IssueKey returns a usable pair.
func makeKey(t *testing.T, fb *fakeBackend) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	fb.issueResponse = signingKeyResponse{
		PublicKey:  base64.StdEncoding.EncodeToString(pub),
		PrivateKey: base64.StdEncoding.EncodeToString(priv),
		IssuedAt:   now,
		ExpiresAt:  now.Add(30 * time.Minute),
	}
	return priv, pub
}

func TestIssueKey_HappyPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	_, pub := makeKey(t, fb)
	c := quickClient(srv)

	got, err := c.IssueKey(context.Background(), "run-abc", 0)
	if err != nil {
		t.Fatalf("IssueKey: %v", err)
	}
	if got.RunID != "run-abc" {
		t.Errorf("RunID = %q", got.RunID)
	}
	if !bytes.Equal(got.PublicKey, pub) {
		t.Error("PublicKey did not round-trip")
	}
	if len(got.PrivateKey) != ed25519.PrivateKeySize {
		t.Errorf("PrivateKey len = %d", len(got.PrivateKey))
	}
	// Signing with the returned private should verify under the
	// returned public — round-trip the bytes through a sign / verify.
	msg := []byte("hello fishhawk")
	sig := ed25519.Sign(got.PrivateKey, msg)
	if !ed25519.Verify(got.PublicKey, msg, sig) {
		t.Error("returned (priv, pub) pair did not round-trip a signature")
	}
}

func TestIssueKey_AlreadyIssued(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.issueStatus = http.StatusConflict
	c := quickClient(srv)
	_, err := c.IssueKey(context.Background(), "run-x", 0)
	if !errors.Is(err, ErrAlreadyIssued) {
		t.Errorf("err = %v, want ErrAlreadyIssued", err)
	}
}

func TestIssueKey_NotFound(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.issueStatus = http.StatusNotFound
	c := quickClient(srv)
	_, err := c.IssueKey(context.Background(), "run-x", 0)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestIssueKey_TTLForwarded(t *testing.T) {
	fb, srv := newFakeBackend(t)
	makeKey(t, fb)
	c := quickClient(srv)
	mux := http.NewServeMux()
	got := make(chan int, 1)
	mux.HandleFunc("POST /v0/runs/{run_id}/signing-key", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			TTL int `json:"ttl_seconds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- body.TTL
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(fb.issueResponse)
	})
	customSrv := httptest.NewServer(mux)
	t.Cleanup(customSrv.Close)
	c.BaseURL = customSrv.URL

	if _, err := c.IssueKey(context.Background(), "run-y", 90*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-got:
		if v != 90 {
			t.Errorf("ttl_seconds = %d, want 90", v)
		}
	default:
		t.Fatal("server did not receive request")
	}
}

func TestShipTrace_HappyPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	c := quickClient(srv)

	bundle := []byte("gzip-bytes-pretend")
	res, err := c.ShipTrace(context.Background(), ShipArgs{
		RunID:      "11111111-2222-3333-4444-555555555555",
		StageID:    "22222222-3333-4444-5555-666666666666",
		Variant:    "raw",
		Bundle:     bundle,
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("ShipTrace: %v", err)
	}
	if res.Variant != "raw" {
		t.Errorf("Variant = %q", res.Variant)
	}
	// Backend echoed sha256 of body; compare against ours.
	expectHash := sha256.Sum256(bundle)
	if res.ContentHash != hex.EncodeToString(expectHash[:]) {
		t.Errorf("ContentHash = %q, want %x", res.ContentHash, expectHash)
	}
	// Sanity check: the captured signature decodes and verifies.
	sig, err := hex.DecodeString(fb.receivedSig)
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	digest := sha256.Sum256(bundle)
	if !ed25519.Verify(priv.Public().(ed25519.PublicKey), digest[:], sig) {
		t.Error("captured signature did not verify against priv.Public()")
	}
	// Query string carried stage_id + variant.
	if !strings.Contains(fb.receivedQuery, "stage_id=22222222-3333-4444-5555-666666666666") {
		t.Errorf("query missing stage_id: %s", fb.receivedQuery)
	}
	if !strings.Contains(fb.receivedQuery, "variant=raw") {
		t.Errorf("query missing variant: %s", fb.receivedQuery)
	}
}

func TestShipTrace_RetriesOn500(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	fb.shipErrCount = 2 // 500, 500, then 202
	c := quickClient(srv)

	_, err := c.ShipTrace(context.Background(), ShipArgs{
		RunID: "r", StageID: "s", Variant: "raw",
		Bundle: []byte("b"), PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("ShipTrace: %v", err)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if fb.calls != 3 {
		t.Errorf("calls = %d, want 3 (2 fails + 1 success)", fb.calls)
	}
}

func TestShipTrace_ExhaustsRetries(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	fb.shipErrCount = 100 // always 500
	c := quickClient(srv)

	_, err := c.ShipTrace(context.Background(), ShipArgs{
		RunID: "r", StageID: "s", Variant: "raw",
		Bundle: []byte("b"), PrivateKey: priv,
	})
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	want := c.MaxRetries + 1
	if fb.calls != want {
		t.Errorf("calls = %d, want %d", fb.calls, want)
	}
}

func TestShipTrace_401StopsImmediately(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	fb.shipStatus = http.StatusUnauthorized
	fb.shipBody = `{"error":{"code":"signature_invalid","message":"no"}}`
	c := quickClient(srv)

	_, err := c.ShipTrace(context.Background(), ShipArgs{
		RunID: "r", StageID: "s", Variant: "raw",
		Bundle: []byte("b"), PrivateKey: priv,
	})
	if !errors.Is(err, ErrSignatureRejected) {
		t.Errorf("err = %v, want ErrSignatureRejected", err)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if fb.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retries on 401)", fb.calls)
	}
}

func TestShipTrace_404(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	fb.shipStatus = http.StatusNotFound
	c := quickClient(srv)

	_, err := c.ShipTrace(context.Background(), ShipArgs{
		RunID: "r", StageID: "s", Variant: "raw",
		Bundle: []byte("b"), PrivateKey: priv,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestShipTrace_ContextCancellation(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	fb.shipErrCount = 100
	c := quickClient(srv)
	c.Backoff = 50 * time.Millisecond
	c.MaxRetries = 5

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done

	_, err := c.ShipTrace(ctx, ShipArgs{
		RunID: "r", StageID: "s", Variant: "raw",
		Bundle: []byte("b"), PrivateKey: priv,
	})
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestShipTrace_RejectsEmptyBundle(t *testing.T) {
	c := New("http://nowhere")
	_, err := c.ShipTrace(context.Background(), ShipArgs{
		RunID: "r", StageID: "s", Variant: "raw",
		Bundle: nil, PrivateKey: make(ed25519.PrivateKey, ed25519.PrivateKeySize),
	})
	if err == nil || !strings.Contains(err.Error(), "empty bundle") {
		t.Errorf("err = %v, want empty bundle", err)
	}
}

func TestShipTrace_RejectsBadKey(t *testing.T) {
	c := New("http://nowhere")
	_, err := c.ShipTrace(context.Background(), ShipArgs{
		RunID: "r", StageID: "s", Variant: "raw",
		Bundle: []byte("b"), PrivateKey: ed25519.PrivateKey{0x01},
	})
	if err == nil || !strings.Contains(err.Error(), "private key") {
		t.Errorf("err = %v, want private key length error", err)
	}
}

func TestFetchPrompt_HappyPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	c := quickClient(srv)

	got, err := c.FetchPrompt(context.Background(), FetchPromptArgs{
		StageID:    "stage-abc",
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("FetchPrompt: %v", err)
	}
	if got.StageID != "stage-abc" {
		t.Errorf("StageID = %q", got.StageID)
	}
	if got.StageType != "implement" {
		t.Errorf("StageType = %q", got.StageType)
	}
	if got.Prompt == "" {
		t.Error("Prompt empty")
	}
	if len(got.PromptHash) != 64 {
		t.Errorf("PromptHash len = %d", len(got.PromptHash))
	}

	// Path was stage-bound; signature was sent.
	if fb.promptReceivedPath != "/v0/stages/stage-abc/prompt" {
		t.Errorf("path = %q", fb.promptReceivedPath)
	}
	if fb.promptReceivedSig == "" {
		t.Error("X-Fishhawk-Signature missing")
	}

	// Signature should verify against the canonical message bytes.
	digest := sha256.Sum256([]byte("prompt:stage-abc"))
	sigBytes, decErr := hex.DecodeString(fb.promptReceivedSig)
	if decErr != nil {
		t.Fatal(decErr)
	}
	if !ed25519.Verify(priv.Public().(ed25519.PublicKey), digest[:], sigBytes) {
		t.Error("signature does not verify against canonical message")
	}
}

func TestFetchPrompt_SignatureRejected(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	fb.promptStatus = http.StatusUnauthorized
	fb.promptBody = `{"code":"signature_invalid"}`
	c := quickClient(srv)

	_, err := c.FetchPrompt(context.Background(), FetchPromptArgs{
		StageID: "s", PrivateKey: priv,
	})
	if !errors.Is(err, ErrSignatureRejected) {
		t.Errorf("err = %v, want ErrSignatureRejected", err)
	}
}

func TestFetchPrompt_NotFound(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	fb.promptStatus = http.StatusNotFound
	c := quickClient(srv)

	_, err := c.FetchPrompt(context.Background(), FetchPromptArgs{
		StageID: "s", PrivateKey: priv,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFetchPrompt_UnsupportedStage(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	fb.promptStatus = http.StatusNotImplemented
	c := quickClient(srv)

	_, err := c.FetchPrompt(context.Background(), FetchPromptArgs{
		StageID: "s", PrivateKey: priv,
	})
	if !errors.Is(err, ErrUnsupportedStage) {
		t.Errorf("err = %v, want ErrUnsupportedStage", err)
	}
}

func TestFetchPrompt_RetriesOn5xx(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	fb.promptErrCount = 2 // two 500s, then OK
	c := quickClient(srv)

	got, err := c.FetchPrompt(context.Background(), FetchPromptArgs{
		StageID: "s", PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("FetchPrompt: %v", err)
	}
	if got.Prompt == "" {
		t.Error("Prompt empty after retry success")
	}
	if fb.promptCalls != 3 {
		t.Errorf("expected 3 calls (2 retries + success), got %d", fb.promptCalls)
	}
}

func TestFetchPrompt_RejectsEmptyStageID(t *testing.T) {
	c := New("http://nowhere")
	_, err := c.FetchPrompt(context.Background(), FetchPromptArgs{
		StageID:    "",
		PrivateKey: make(ed25519.PrivateKey, ed25519.PrivateKeySize),
	})
	if err == nil || !strings.Contains(err.Error(), "stage id") {
		t.Errorf("err = %v, want stage id error", err)
	}
}

func TestFetchPrompt_RejectsBadKey(t *testing.T) {
	c := New("http://nowhere")
	_, err := c.FetchPrompt(context.Background(), FetchPromptArgs{
		StageID:    "s",
		PrivateKey: ed25519.PrivateKey{0x01},
	})
	if err == nil || !strings.Contains(err.Error(), "private key") {
		t.Errorf("err = %v, want private key length error", err)
	}
}
