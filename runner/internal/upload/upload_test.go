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

// TestFetchPrompt_DecodesScopeFiles confirms the client decodes the
// backend's scope_files response field into FetchedPrompt.ScopeFiles
// (#581), preserving path + operation order.
func TestFetchPrompt_DecodesScopeFiles(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	fb.promptBody = `{
		"stage_id": "stage-abc",
		"stage_type": "implement",
		"prompt": "p",
		"prompt_hash": "h",
		"scope_files": [
			{"path": "backend/internal/server/prompt.go", "operation": "modify"},
			{"path": "docs/api/v0.md", "operation": "modify"}
		]
	}`
	c := quickClient(srv)

	got, err := c.FetchPrompt(context.Background(), FetchPromptArgs{
		StageID:    "stage-abc",
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("FetchPrompt: %v", err)
	}
	if len(got.ScopeFiles) != 2 {
		t.Fatalf("ScopeFiles len = %d, want 2: %+v", len(got.ScopeFiles), got.ScopeFiles)
	}
	if got.ScopeFiles[0].Path != "backend/internal/server/prompt.go" || got.ScopeFiles[0].Operation != "modify" {
		t.Errorf("ScopeFiles[0] = %+v", got.ScopeFiles[0])
	}
	if got.ScopeFiles[1].Path != "docs/api/v0.md" {
		t.Errorf("ScopeFiles[1] = %+v", got.ScopeFiles[1])
	}
}

// TestFetchPrompt_DecodesSliceIndex confirms the client decodes the backend's
// slice_index response field into FetchedPrompt.SliceIndex (ADR-041 / #1141) —
// the decode half of the cross-module slice_index seam. The runner threads
// FetchedPrompt.SliceIndex into cfg.sliceIndex (fetchPromptToFile) and routes
// the child onto fishhawk/run-<parent>/slice-<n>; that field->branch-name half
// is asserted by TestRun_ImplementStage_DecomposedChild_SliceTwo in the runner
// command package.
func TestFetchPrompt_DecodesSliceIndex(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	fb.promptBody = `{
		"stage_id": "stage-abc",
		"stage_type": "implement",
		"prompt": "p",
		"prompt_hash": "h",
		"decomposed_from_run_id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"slice_index": 2
	}`
	c := quickClient(srv)

	got, err := c.FetchPrompt(context.Background(), FetchPromptArgs{
		StageID:    "stage-abc",
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("FetchPrompt: %v", err)
	}
	if got.SliceIndex != 2 {
		t.Errorf("SliceIndex = %d, want 2", got.SliceIndex)
	}
}

// TestFetchPrompt_SliceIndexOmittedWhenAbsent confirms SliceIndex decodes to 0
// when the backend omits the field (standalone run, or a slice-0 child where
// omitempty drops it) — the correct value the runner reads for slice 0.
func TestFetchPrompt_SliceIndexOmittedWhenAbsent(t *testing.T) {
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
	if got.SliceIndex != 0 {
		t.Errorf("SliceIndex = %d, want 0 when absent", got.SliceIndex)
	}
}

// TestFetchPrompt_DecodesCommitAuthor confirms the client decodes the
// backend's commit_author_name/commit_author_email response fields into
// FetchedPrompt so App-backed commits attribute to the App's bot (#722).
func TestFetchPrompt_DecodesCommitAuthor(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	fb.promptBody = `{
		"stage_id": "stage-abc",
		"stage_type": "implement",
		"prompt": "p",
		"prompt_hash": "h",
		"commit_author_name": "fishhawk[bot]",
		"commit_author_email": "41898282+fishhawk[bot]@users.noreply.github.com"
	}`
	c := quickClient(srv)

	got, err := c.FetchPrompt(context.Background(), FetchPromptArgs{
		StageID:    "stage-abc",
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("FetchPrompt: %v", err)
	}
	if got.CommitAuthorName != "fishhawk[bot]" {
		t.Errorf("CommitAuthorName = %q, want fishhawk[bot]", got.CommitAuthorName)
	}
	if got.CommitAuthorEmail != "41898282+fishhawk[bot]@users.noreply.github.com" {
		t.Errorf("CommitAuthorEmail = %q", got.CommitAuthorEmail)
	}
}

// TestFetchPrompt_CommitAuthorOmittedWhenAbsent confirms the commit
// author fields decode to empty when the backend omits them (no
// resolvable App), so the runner falls back to its default bot identity.
func TestFetchPrompt_CommitAuthorOmittedWhenAbsent(t *testing.T) {
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
	if got.CommitAuthorName != "" || got.CommitAuthorEmail != "" {
		t.Errorf("commit author = (%q,%q), want empty when absent",
			got.CommitAuthorName, got.CommitAuthorEmail)
	}
}

// TestFetchPrompt_ScopeFilesOmittedWhenAbsent confirms ScopeFiles
// decodes to nil when the backend omits the field (plan_missing or a
// non-implement stage), so the runner falls back to `git add -A`.
func TestFetchPrompt_ScopeFilesOmittedWhenAbsent(t *testing.T) {
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
	if got.ScopeFiles != nil {
		t.Errorf("ScopeFiles = %+v, want nil when absent", got.ScopeFiles)
	}
}

// TestFetchPrompt_DecodesBindingAssertions confirms the client decodes the
// backend's binding_assertions response field (#1171) into
// FetchedPrompt.BindingAssertions, preserving type/path/literal order.
func TestFetchPrompt_DecodesBindingAssertions(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	fb.promptBody = `{
		"stage_id": "stage-abc",
		"stage_type": "implement",
		"prompt": "p",
		"prompt_hash": "h",
		"binding_assertions": [
			{"type": "file_contains", "path": "internal/foo/layout.yaml", "literal": "pad: 3"},
			{"type": "test_asserts", "path": "internal/foo/foo_test.go", "literal": "TestPadWidth"}
		]
	}`
	c := quickClient(srv)

	got, err := c.FetchPrompt(context.Background(), FetchPromptArgs{
		StageID:    "stage-abc",
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("FetchPrompt: %v", err)
	}
	if len(got.BindingAssertions) != 2 {
		t.Fatalf("BindingAssertions len = %d, want 2: %+v", len(got.BindingAssertions), got.BindingAssertions)
	}
	if got.BindingAssertions[0] != (BindingAssertion{Type: "file_contains", Path: "internal/foo/layout.yaml", Literal: "pad: 3"}) {
		t.Errorf("BindingAssertions[0] = %+v", got.BindingAssertions[0])
	}
	if got.BindingAssertions[1] != (BindingAssertion{Type: "test_asserts", Path: "internal/foo/foo_test.go", Literal: "TestPadWidth"}) {
		t.Errorf("BindingAssertions[1] = %+v", got.BindingAssertions[1])
	}
}

// TestFetchPrompt_BindingAssertionsOmittedWhenAbsent confirms BindingAssertions
// decodes to nil when the backend omits the field (no declared assertions, or a
// non-implement stage), so the runner's binding-assertion gate is a no-op —
// byte-identical to behavior before #1171.
func TestFetchPrompt_BindingAssertionsOmittedWhenAbsent(t *testing.T) {
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
	if got.BindingAssertions != nil {
		t.Errorf("BindingAssertions = %+v, want nil when absent", got.BindingAssertions)
	}
}

// TestFetchPrompt_DecodesScopeExemptions confirms the client decodes the
// backend's scope_exemptions response field (#1229) into
// FetchedPrompt.ScopeExemptions, preserving path/reason order.
func TestFetchPrompt_DecodesScopeExemptions(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	fb.promptBody = `{
		"stage_id": "stage-abc",
		"stage_type": "implement",
		"prompt": "p",
		"prompt_hash": "h",
		"scope_exemptions": [
			{"path": "backend/internal/server/recover.go", "reason": "interface unchanged, no edit needed"},
			{"path": "docs/api/v0.md", "reason": "prose already current"}
		]
	}`
	c := quickClient(srv)

	got, err := c.FetchPrompt(context.Background(), FetchPromptArgs{
		StageID:    "stage-abc",
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("FetchPrompt: %v", err)
	}
	if len(got.ScopeExemptions) != 2 {
		t.Fatalf("ScopeExemptions len = %d, want 2: %+v", len(got.ScopeExemptions), got.ScopeExemptions)
	}
	if got.ScopeExemptions[0] != (ScopeExemption{Path: "backend/internal/server/recover.go", Reason: "interface unchanged, no edit needed"}) {
		t.Errorf("ScopeExemptions[0] = %+v", got.ScopeExemptions[0])
	}
	if got.ScopeExemptions[1] != (ScopeExemption{Path: "docs/api/v0.md", Reason: "prose already current"}) {
		t.Errorf("ScopeExemptions[1] = %+v", got.ScopeExemptions[1])
	}
}

// TestFetchPrompt_ScopeExemptionsOmittedWhenAbsent confirms ScopeExemptions
// decodes to nil when the backend omits the field (every non-recovery run), so
// the runner's scope-completeness gate keeps the strict #1151 default —
// byte-identical to behavior before #1229.
func TestFetchPrompt_ScopeExemptionsOmittedWhenAbsent(t *testing.T) {
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
	if got.ScopeExemptions != nil {
		t.Errorf("ScopeExemptions = %+v, want nil when absent", got.ScopeExemptions)
	}
}

// TestBindingAssertions_BackendEmitRunnerDecodeRoundTrip pins the backend
// prompt-response binding_assertions JSON serialization against this package's
// FetchedPrompt decoder (#1171 approval condition 1), the binding-assertion
// analogue of the gateevidence↔bundle wire-contract test. The backend's
// server.bindingAssertion struct serializes with the json tags
// type/path/literal; backendBindingAssertion below replicates that exact shape
// (a runner test cannot import the backend module without inverting the
// dependency, so the tags are mirrored here and pinned). Marshalling the
// backend shape and decoding it through FetchedPrompt must reproduce the
// declaration field-for-field; a silent zero value means a tag drifted across
// the module boundary that per-side unit tests cannot catch.
func TestBindingAssertions_BackendEmitRunnerDecodeRoundTrip(t *testing.T) {
	// Mirror of backend server.bindingAssertion (json: type/path/literal).
	type backendBindingAssertion struct {
		Type    string `json:"type"`
		Path    string `json:"path"`
		Literal string `json:"literal"`
	}
	// Mirror of the relevant subset of the backend's promptResponse: the
	// binding_assertions field carries the slice with omitempty, exactly as
	// the runner's FetchedPrompt expects it.
	type backendPromptResponse struct {
		StageID           string                    `json:"stage_id"`
		StageType         string                    `json:"stage_type"`
		Prompt            string                    `json:"prompt"`
		PromptHash        string                    `json:"prompt_hash"`
		BindingAssertions []backendBindingAssertion `json:"binding_assertions,omitempty"`
	}

	emitted := backendPromptResponse{
		StageID:    "stage-xyz",
		StageType:  "implement",
		Prompt:     "p",
		PromptHash: "h",
		BindingAssertions: []backendBindingAssertion{
			{Type: "file_contains", Path: "docs/api/v0.md", Literal: "binding_assertions"},
			{Type: "test_asserts", Path: "backend/internal/server/approvals_test.go", Literal: "TestApprove_RecordsBindingAssertions"},
		},
	}
	wire, err := json.Marshal(emitted)
	if err != nil {
		t.Fatalf("marshal backend prompt-response: %v", err)
	}

	var decoded FetchedPrompt
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("decode into FetchedPrompt: %v", err)
	}
	if len(decoded.BindingAssertions) != len(emitted.BindingAssertions) {
		t.Fatalf("decoded %d assertions, want %d (wire: %s)",
			len(decoded.BindingAssertions), len(emitted.BindingAssertions), wire)
	}
	for i, want := range emitted.BindingAssertions {
		got := decoded.BindingAssertions[i]
		if got.Type != want.Type || got.Path != want.Path || got.Literal != want.Literal {
			t.Errorf("assertion[%d] = %+v, want {%s %s %s} — wire tag drift",
				i, got, want.Type, want.Path, want.Literal)
		}
	}

	// And the reverse: the runner's BindingAssertion must marshal back to the
	// same wire keys, so the contract holds in both directions.
	reMarshaled, err := json.Marshal(decoded.BindingAssertions[0])
	if err != nil {
		t.Fatalf("marshal runner BindingAssertion: %v", err)
	}
	var backFromRunner backendBindingAssertion
	if err := json.Unmarshal(reMarshaled, &backFromRunner); err != nil {
		t.Fatalf("decode runner-marshaled assertion into backend shape: %v", err)
	}
	if backFromRunner != emitted.BindingAssertions[0] {
		t.Errorf("runner→backend re-decode = %+v, want %+v", backFromRunner, emitted.BindingAssertions[0])
	}
}

// TestFetchPrompt_DecodesFixup confirms the client decodes the backend's
// fixup/fixup_branch response fields (#762) so the runner routes a fix-up
// pass's commit onto the existing PR branch.
func TestFetchPrompt_DecodesFixup(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	fb.promptBody = `{
		"stage_id": "stage-abc",
		"stage_type": "implement",
		"prompt": "p",
		"prompt_hash": "h",
		"fixup": true,
		"fixup_branch": "fishhawk/run-aaaaaaaa/stage-bbbbbbbb",
		"fixup_expected_head_sha": "cafe000000000000000000000000000000000000"
	}`
	c := quickClient(srv)

	got, err := c.FetchPrompt(context.Background(), FetchPromptArgs{
		StageID:    "stage-abc",
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("FetchPrompt: %v", err)
	}
	if !got.Fixup {
		t.Error("Fixup = false, want true")
	}
	if got.FixupBranch != "fishhawk/run-aaaaaaaa/stage-bbbbbbbb" {
		t.Errorf("FixupBranch = %q", got.FixupBranch)
	}
	if got.FixupExpectedHeadSHA != "cafe000000000000000000000000000000000000" {
		t.Errorf("FixupExpectedHeadSHA = %q, want the advertised recorded head (#967)", got.FixupExpectedHeadSHA)
	}
}

// TestFetchPrompt_FixupOmittedWhenAbsent confirms the fix-up fields decode
// to their zero values when the backend omits them (a normal implement
// stage), so branch routing falls through to the per-stage branch.
func TestFetchPrompt_FixupOmittedWhenAbsent(t *testing.T) {
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
	if got.Fixup || got.FixupBranch != "" || got.FixupExpectedHeadSHA != "" {
		t.Errorf("fixup = (%t,%q,%q), want (false,\"\",\"\") when absent",
			got.Fixup, got.FixupBranch, got.FixupExpectedHeadSHA)
	}
}

// TestFetchPrompt_DecodesApplyPatches asserts the runner consumes the #1165
// fixup_apply_patches apply-list — the consume end of the server-serves ->
// runner-applies seam.
func TestFetchPrompt_DecodesApplyPatches(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	fb.promptBody = `{
		"stage_id": "stage-abc",
		"stage_type": "implement",
		"prompt": "p",
		"prompt_hash": "h",
		"fixup": true,
		"fixup_branch": "fishhawk/run-aaaaaaaa",
		"fixup_apply_patches": [{"patch": "diff-one"}, {"patch": "diff-two"}]
	}`
	c := quickClient(srv)

	got, err := c.FetchPrompt(context.Background(), FetchPromptArgs{
		StageID:    "stage-abc",
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("FetchPrompt: %v", err)
	}
	if len(got.FixupApplyPatches) != 2 {
		t.Fatalf("FixupApplyPatches len = %d, want 2", len(got.FixupApplyPatches))
	}
	if got.FixupApplyPatches[0].Patch != "diff-one" || got.FixupApplyPatches[1].Patch != "diff-two" {
		t.Errorf("FixupApplyPatches = %+v, want the two diffs verbatim", got.FixupApplyPatches)
	}
}

// TestFetchPrompt_ApplyPatchesOmittedWhenAbsent confirms a non-eligible fix-up
// (or older backend) leaves the apply-list empty, so the runner takes the agent
// path — byte-identical to pre-#1165.
func TestFetchPrompt_ApplyPatchesOmittedWhenAbsent(t *testing.T) {
	fb, srv := newFakeBackend(t)
	priv, _ := makeKey(t, fb)
	fb.promptBody = `{"stage_id":"stage-abc","stage_type":"implement","prompt":"p","prompt_hash":"h","fixup":true,"fixup_branch":"b"}`
	c := quickClient(srv)

	got, err := c.FetchPrompt(context.Background(), FetchPromptArgs{
		StageID: "stage-abc", PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("FetchPrompt: %v", err)
	}
	if len(got.FixupApplyPatches) != 0 {
		t.Errorf("FixupApplyPatches = %+v, want empty when absent", got.FixupApplyPatches)
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

// --- FetchMCPToken (E19.8 / #348) ---

func TestFetchMCPToken_HappyPath(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	var receivedSig string
	var receivedBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/mcp-token", func(w http.ResponseWriter, r *http.Request) {
		receivedSig = r.Header.Get("X-Fishhawk-Signature")
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(FetchMCPTokenResult{
			Token:     "fhm_serverissued",
			TokenID:   "tok-123",
			RunID:     r.PathValue("run_id"),
			ExpiresAt: time.Now().Add(time.Hour),
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}

	got, err := c.FetchMCPToken(context.Background(), FetchMCPTokenArgs{
		RunID:      "run-abc",
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("FetchMCPToken: %v", err)
	}
	if got.Token != "fhm_serverissued" {
		t.Errorf("Token = %q", got.Token)
	}
	if got.RunID != "run-abc" {
		t.Errorf("RunID = %q", got.RunID)
	}
	if receivedSig == "" {
		t.Error("server didn't see X-Fishhawk-Signature")
	}
	// Verify the signature against the body the server received.
	sigBytes, decErr := hex.DecodeString(receivedSig)
	if decErr != nil {
		t.Fatal(decErr)
	}
	digest := sha256.Sum256(receivedBody)
	if !ed25519.Verify(pub, digest[:], sigBytes) {
		t.Error("signature does not verify against received body digest")
	}
}

func TestFetchMCPToken_SignatureRejected(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/mcp-token", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"signature_invalid"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}

	_, err := c.FetchMCPToken(context.Background(), FetchMCPTokenArgs{
		RunID: "run-abc", PrivateKey: priv,
	})
	if !errors.Is(err, ErrSignatureRejected) {
		t.Errorf("err = %v, want ErrSignatureRejected", err)
	}
}

func TestFetchMCPToken_NotFound(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/mcp-token", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}

	_, err := c.FetchMCPToken(context.Background(), FetchMCPTokenArgs{
		RunID: "run-abc", PrivateKey: priv,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFetchMCPToken_RejectsEmptyRunID(t *testing.T) {
	c := New("http://nowhere")
	_, err := c.FetchMCPToken(context.Background(), FetchMCPTokenArgs{
		RunID: "", PrivateKey: make(ed25519.PrivateKey, ed25519.PrivateKeySize),
	})
	if err == nil || !strings.Contains(err.Error(), "run_id") {
		t.Errorf("err = %v, want run_id error", err)
	}
}

func TestFetchMCPToken_RejectsBadKey(t *testing.T) {
	c := New("http://nowhere")
	_, err := c.FetchMCPToken(context.Background(), FetchMCPTokenArgs{
		RunID:      "run-abc",
		PrivateKey: ed25519.PrivateKey{0x01},
	})
	if err == nil || !strings.Contains(err.Error(), "private key") {
		t.Errorf("err = %v, want private key length error", err)
	}
}

// canonicalScopeAmendmentsJSON is the wire fixture for GET
// /v0/runs/{run_id}/scope-amendments, matching the backend's
// scopeAmendmentListResponse shape (backend/internal/server/
// scope_amendment.go). The runner e2e in cmd/fishhawk-runner/
// main_test.go serves the same canonical shape so the seam is pinned
// from both sides (#618 cross-boundary test rule).
const canonicalScopeAmendmentsJSON = `{
  "items": [
    {
      "id": "0b54f9f3-0c83-4f6e-9c6e-1a54a3b1a001",
      "run_id": "run-abc",
      "stage_id": "0b54f9f3-0c83-4f6e-9c6e-1a54a3b1a002",
      "paths": [
        {"path": "pkg/extra.go", "operation": "modify"},
        {"path": "pkg/newfile.go", "operation": "create"}
      ],
      "reason": "the seam needs these",
      "status": "approved",
      "decision_reason": "ok",
      "decided_by": "github:operator",
      "requested_at": "2026-06-10T12:00:00Z",
      "decided_at": "2026-06-10T12:01:00Z"
    },
    {
      "id": "0b54f9f3-0c83-4f6e-9c6e-1a54a3b1a003",
      "run_id": "run-abc",
      "stage_id": "0b54f9f3-0c83-4f6e-9c6e-1a54a3b1a002",
      "paths": [{"path": "pkg/denied.go", "operation": "modify"}],
      "reason": "nope",
      "status": "denied",
      "decision_reason": "out of bounds",
      "decided_by": "github:operator",
      "requested_at": "2026-06-10T12:02:00Z",
      "decided_at": "2026-06-10T12:03:00Z"
    }
  ]
}`

func TestFetchScopeAmendments_HappyPath(t *testing.T) {
	var receivedAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}/scope-amendments", func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, canonicalScopeAmendmentsJSON)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}

	got, err := c.FetchScopeAmendments(context.Background(), FetchScopeAmendmentsArgs{
		RunID:    "run-abc",
		MCPToken: "fhm_runnerheld",
	})
	if err != nil {
		t.Fatalf("FetchScopeAmendments: %v", err)
	}
	if receivedAuth != "Bearer fhm_runnerheld" {
		t.Errorf("Authorization = %q, want the run-bound fhm_ bearer", receivedAuth)
	}
	if len(got) != 2 {
		t.Fatalf("items = %d, want 2", len(got))
	}
	if got[0].Status != "approved" || len(got[0].Paths) != 2 ||
		got[0].Paths[1].Operation != "create" || got[0].Paths[1].Path != "pkg/newfile.go" {
		t.Errorf("approved item decoded wrong: %+v", got[0])
	}
	if got[1].Status != "denied" || got[1].DecisionReason != "out of bounds" {
		t.Errorf("denied item decoded wrong: %+v", got[1])
	}
}

func TestFetchScopeAmendments_Non200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}/scope-amendments", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"cross_run_scope_amendment"}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}

	_, err := c.FetchScopeAmendments(context.Background(), FetchScopeAmendmentsArgs{
		RunID: "run-abc", MCPToken: "fhm_x",
	})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want status error carrying 403", err)
	}
}

func TestFetchScopeAmendments_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}/scope-amendments", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}

	_, err := c.FetchScopeAmendments(context.Background(), FetchScopeAmendmentsArgs{
		RunID: "run-abc", MCPToken: "fhm_x",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFetchScopeAmendments_RejectsMissingInputs(t *testing.T) {
	c := New("http://nowhere")
	if _, err := c.FetchScopeAmendments(context.Background(), FetchScopeAmendmentsArgs{MCPToken: "fhm_x"}); err == nil || !strings.Contains(err.Error(), "run_id") {
		t.Errorf("err = %v, want run_id error", err)
	}
	if _, err := c.FetchScopeAmendments(context.Background(), FetchScopeAmendmentsArgs{RunID: "run-abc"}); err == nil || !strings.Contains(err.Error(), "mcp token") {
		t.Errorf("err = %v, want mcp token error", err)
	}
}

// runLineageServer spins a httptest server whose GET /v0/runs/{run_id}
// returns body for any run id, and returns a Client pointed at it.
func runLineageServer(t *testing.T, status int, body string) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL)
}

func TestRunLineageComplete_True(t *testing.T) {
	c := runLineageServer(t, http.StatusOK, `{"id":"r","lineage_complete":true}`)
	ok, err := c.RunLineageComplete(context.Background(), "run-abc")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("complete = false, want true")
	}
}

func TestRunLineageComplete_False(t *testing.T) {
	c := runLineageServer(t, http.StatusOK, `{"id":"r","lineage_complete":false}`)
	ok, err := c.RunLineageComplete(context.Background(), "run-abc")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("complete = true, want false")
	}
}

// TestRunLineageComplete_FieldAbsent asserts an older backend that omits
// lineage_complete decodes to false (not reclaimable) rather than erroring.
func TestRunLineageComplete_FieldAbsent(t *testing.T) {
	c := runLineageServer(t, http.StatusOK, `{"id":"r","state":"running"}`)
	ok, err := c.RunLineageComplete(context.Background(), "run-abc")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("complete = true, want false when field absent")
	}
}

func TestRunLineageComplete_NotFound(t *testing.T) {
	c := runLineageServer(t, http.StatusNotFound, `{"error":"run_not_found"}`)
	if _, err := c.RunLineageComplete(context.Background(), "run-abc"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRunLineageComplete_RejectsEmptyRunID(t *testing.T) {
	c := New("http://unused")
	if _, err := c.RunLineageComplete(context.Background(), ""); err == nil {
		t.Errorf("want error on empty run id")
	}
}
