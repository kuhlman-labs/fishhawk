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

// instTokenBackend mimics POST /v0/runs/{run_id}/installation-token.
type instTokenBackend struct {
	mu sync.Mutex

	status       int
	body         string
	respTok      string
	receivedSig  string
	receivedPath string
	calls        int
}

func newInstTokenBackend(t *testing.T) (*instTokenBackend, *httptest.Server) {
	t.Helper()
	b := &instTokenBackend{status: http.StatusCreated, respTok: "ghs_xyz"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/installation-token", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.calls++
		b.receivedSig = r.Header.Get("X-Fishhawk-Signature")
		b.receivedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(b.status)
		if b.status == http.StatusCreated && b.body == "" {
			_ = json.NewEncoder(w).Encode(FetchInstallationTokenResult{Token: b.respTok})
		} else if b.body != "" {
			_, _ = io.WriteString(w, b.body)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return b, srv
}

func quickInstClient(srv *httptest.Server) *Client {
	c := New(srv.URL)
	c.HTTP = &http.Client{Timeout: time.Second}
	return c
}

func makeInstKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func TestFetchInstallationToken_HappyPath(t *testing.T) {
	b, srv := newInstTokenBackend(t)
	c := quickInstClient(srv)
	priv := makeInstKey(t)

	res, err := c.FetchInstallationToken(context.Background(), FetchInstallationTokenArgs{
		RunID:      "run-aaa",
		StageID:    "stage-bbb",
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("FetchInstallationToken: %v", err)
	}
	if res.Token != "ghs_xyz" {
		t.Errorf("token = %q", res.Token)
	}
	if b.calls != 1 {
		t.Errorf("calls = %d, want 1", b.calls)
	}
	if b.receivedPath != "/v0/runs/run-aaa/installation-token" {
		t.Errorf("path = %q", b.receivedPath)
	}
	// Signature is over the empty body's sha256.
	digest := sha256.Sum256([]byte{})
	wantSig := hex.EncodeToString(ed25519.Sign(priv, digest[:]))
	if b.receivedSig != wantSig {
		t.Errorf("signature mismatch")
	}
}

func TestFetchInstallationToken_SignatureRejected_401(t *testing.T) {
	b, srv := newInstTokenBackend(t)
	b.status = http.StatusUnauthorized
	b.body = `{"code":"signature_invalid"}`
	c := quickInstClient(srv)
	priv := makeInstKey(t)

	_, err := c.FetchInstallationToken(context.Background(), FetchInstallationTokenArgs{
		RunID: "r", StageID: "s", PrivateKey: priv,
	})
	if !errors.Is(err, ErrSignatureRejected) {
		t.Errorf("err = %v, want ErrSignatureRejected", err)
	}
}

func TestFetchInstallationToken_NotFound_404(t *testing.T) {
	b, srv := newInstTokenBackend(t)
	b.status = http.StatusNotFound
	c := quickInstClient(srv)
	priv := makeInstKey(t)

	_, err := c.FetchInstallationToken(context.Background(), FetchInstallationTokenArgs{
		RunID: "r", StageID: "s", PrivateKey: priv,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFetchInstallationToken_BadGateway_502(t *testing.T) {
	b, srv := newInstTokenBackend(t)
	b.status = http.StatusBadGateway
	b.body = `{"code":"installation_token_issuance_failed","message":"gh JWT rejected"}`
	c := quickInstClient(srv)
	priv := makeInstKey(t)

	_, err := c.FetchInstallationToken(context.Background(), FetchInstallationTokenArgs{
		RunID: "r", StageID: "s", PrivateKey: priv,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "gh JWT rejected") {
		t.Errorf("err = %v, want backend message in error", err)
	}
}

func TestFetchInstallationToken_RejectsBadInputs(t *testing.T) {
	c := New("http://example.com")
	priv := makeInstKey(t)
	if _, err := c.FetchInstallationToken(context.Background(), FetchInstallationTokenArgs{
		RunID: "", StageID: "s", PrivateKey: priv,
	}); err == nil {
		t.Error("expected run_id-required error")
	}
	if _, err := c.FetchInstallationToken(context.Background(), FetchInstallationTokenArgs{
		RunID: "r", StageID: "s", PrivateKey: ed25519.PrivateKey{1, 2},
	}); err == nil {
		t.Error("expected key-length error")
	}
}
