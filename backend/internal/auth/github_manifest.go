package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// GitHub's published manifest-conversion endpoint. Override via
// NewGitHubManifest's urls argument in tests.
const defaultManifestConversionsURL = "https://api.github.com/app-manifests"

// ManifestURLs lets tests substitute an httptest.Server URL for the
// real api.github.com manifest-conversion endpoint.
type ManifestURLs struct {
	// ConversionsURL is the base URL the path `/{code}/conversions`
	// is appended to. Defaults to https://api.github.com/app-manifests.
	ConversionsURL string
}

// GitHubManifest exchanges the one-shot `code` GitHub returns from
// the manifest-flow redirect for a freshly-minted App's
// credentials. See: docs.github.com/en/apps/sharing-github-apps/
// registering-a-github-app-from-a-manifest.
//
// The code is single-use and good for ten minutes. Convert is
// the only operation; everything else (App ID, OAuth client
// secret, webhook secret, PEM private key) flows from the
// conversion response.
type GitHubManifest struct {
	urls ManifestURLs
	http *http.Client
}

// NewGitHubManifest returns a configured client. urls.ConversionsURL
// is empty in production (defaults applied); tests pass a stub.
func NewGitHubManifest(urls ManifestURLs) *GitHubManifest {
	if urls.ConversionsURL == "" {
		urls.ConversionsURL = defaultManifestConversionsURL
	}
	return &GitHubManifest{
		urls: urls,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// ManifestCredentials is the GitHub App created by the manifest
// flow. Lives in memory only — the Convert call is the single
// place where the secrets exist as plaintext; the operator (or
// the secrets backend, in the hosted path) immediately persists
// them and discards the struct.
type ManifestCredentials struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	HTMLURL       string `json:"html_url"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PEM           string `json:"pem"`
}

// Convert posts code to /app-manifests/{code}/conversions and
// returns the freshly-minted App's credentials. Errors carry the
// HTTP status when GitHub rejects the conversion so callers can
// surface "the code is one-shot — kick off the flow again".
func (g *GitHubManifest) Convert(ctx context.Context, code string) (*ManifestCredentials, error) {
	if code == "" {
		return nil, errors.New("auth: empty manifest code")
	}
	endpoint := strings.TrimRight(g.urls.ConversionsURL, "/") + "/" + code + "/conversions"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: build manifest conversion request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := g.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: manifest conversion: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		brief := readBriefBody(resp.Body)
		return nil, fmt.Errorf("auth: manifest conversion returned %d: %s", resp.StatusCode, brief)
	}

	var out ManifestCredentials
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("auth: decode manifest conversion: %w", err)
	}
	if out.ID == 0 || out.ClientID == "" || out.PEM == "" {
		return nil, errors.New("auth: manifest conversion response missing required fields")
	}
	return &out, nil
}
