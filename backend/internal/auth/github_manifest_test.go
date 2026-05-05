package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeManifestServer stands in for api.github.com's manifest
// endpoint. Tests configure responses via fields and assert on
// captured input.
type fakeManifestServer struct {
	respCode int
	respBody string
	gotCode  string
	calls    int
}

func newFakeManifestServer(t *testing.T) (*fakeManifestServer, ManifestURLs) {
	t.Helper()
	fs := &fakeManifestServer{respCode: http.StatusCreated}
	mux := http.NewServeMux()
	// {base}/{code}/conversions
	mux.HandleFunc("/app-manifests/", func(w http.ResponseWriter, r *http.Request) {
		fs.calls++
		// Path looks like /app-manifests/<code>/conversions
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/app-manifests/"), "/")
		if len(parts) >= 1 {
			fs.gotCode = parts[0]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fs.respCode)
		_, _ = w.Write([]byte(fs.respBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fs, ManifestURLs{ConversionsURL: srv.URL + "/app-manifests"}
}

func TestManifest_Convert_HappyPath(t *testing.T) {
	fs, urls := newFakeManifestServer(t)
	body, _ := json.Marshal(map[string]any{
		"id":             int64(123456),
		"slug":           "fishhawk-local",
		"name":           "Fishhawk (local)",
		"html_url":       "https://github.com/apps/fishhawk-local",
		"client_id":      "Iv1.abc",
		"client_secret":  "s3cret",
		"webhook_secret": "hookhook",
		"pem":            "-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----\n",
	})
	fs.respBody = string(body)
	m := NewGitHubManifest(urls)

	got, err := m.Convert(context.Background(), "code-1")
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if fs.gotCode != "code-1" {
		t.Errorf("path code = %q", fs.gotCode)
	}
	if got.ID != 123456 || got.Slug != "fishhawk-local" {
		t.Errorf("ID/Slug = %d/%q", got.ID, got.Slug)
	}
	if got.ClientID != "Iv1.abc" || got.ClientSecret != "s3cret" {
		t.Errorf("client_id/secret = %q/%q", got.ClientID, got.ClientSecret)
	}
	if got.WebhookSecret != "hookhook" {
		t.Errorf("webhook_secret = %q", got.WebhookSecret)
	}
	if !strings.HasPrefix(got.PEM, "-----BEGIN") {
		t.Errorf("PEM = %q", got.PEM)
	}
}

func TestManifest_Convert_RejectsEmptyCode(t *testing.T) {
	m := NewGitHubManifest(ManifestURLs{})
	_, err := m.Convert(context.Background(), "")
	if err == nil {
		t.Error("expected error on empty code")
	}
}

func TestManifest_Convert_HTTPNon201(t *testing.T) {
	fs, urls := newFakeManifestServer(t)
	fs.respCode = http.StatusNotFound
	fs.respBody = `{"message":"Not Found"}`
	m := NewGitHubManifest(urls)
	_, err := m.Convert(context.Background(), "stale-code")
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want 404 in message", err)
	}
}

func TestManifest_Convert_RejectsMissingFields(t *testing.T) {
	cases := map[string]string{
		"missing id":        `{"client_id":"x","pem":"-----BEGIN-----"}`,
		"missing client_id": `{"id":1,"pem":"-----BEGIN-----"}`,
		"missing pem":       `{"id":1,"client_id":"x"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			fs, urls := newFakeManifestServer(t)
			fs.respBody = body
			m := NewGitHubManifest(urls)
			_, err := m.Convert(context.Background(), "code")
			if err == nil {
				t.Errorf("expected error for %s", name)
			}
		})
	}
}

func TestManifest_Convert_DefaultsToProductionURL(t *testing.T) {
	m := NewGitHubManifest(ManifestURLs{})
	if m.urls.ConversionsURL != defaultManifestConversionsURL {
		t.Errorf("default URL = %q", m.urls.ConversionsURL)
	}
}
