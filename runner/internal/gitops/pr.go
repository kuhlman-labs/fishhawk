package gitops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultGitHubAPIBase is the public api.github.com URL. Override
// via OpenPRClient.APIBase in tests pointing at an httptest.Server.
const defaultGitHubAPIBase = "https://api.github.com"

// OpenPRClient creates pull requests via GitHub's REST API. Caller
// constructs once with the auth token; OpenPR is safe to call
// repeatedly across stages (each PR is independent).
type OpenPRClient struct {
	// HTTP defaults to a 30s-timeout client when nil. Tests
	// override.
	HTTP *http.Client

	// APIBase is the GitHub API root. Empty defaults to
	// https://api.github.com. Tests point at httptest.
	APIBase string

	// Token is the GitHub token (workflow GITHUB_TOKEN or App
	// installation token). Required.
	Token string
}

// OpenPRArgs collects the inputs for a single PR creation.
type OpenPRArgs struct {
	Owner string // "kuhlman-labs"
	Repo  string // "fishhawk"
	Head  string // "fishhawk/run-aaa/stage-bbb"
	Base  string // "main"
	Title string
	Body  string
}

// OpenPRResult mirrors the shape the runner's pull-request artifact
// upload needs.
type OpenPRResult struct {
	PRNumber int
	PRURL    string
}

// OpenPR posts a pull-request creation request and returns the
// resulting (number, html_url). Authenticates with `Authorization:
// Bearer <token>` per GitHub's REST guidance. Single-attempt; PR
// creation is not idempotent on the GitHub side, so retrying a
// transient failure could create a duplicate. The caller is
// responsible for surfacing any error to the audit log.
func (c *OpenPRClient) OpenPR(ctx context.Context, args OpenPRArgs) (*OpenPRResult, error) {
	switch {
	case c.Token == "":
		return nil, errors.New("gitops: token required")
	case args.Owner == "" || args.Repo == "":
		return nil, errors.New("gitops: owner and repo required")
	case args.Head == "" || args.Base == "":
		return nil, errors.New("gitops: head and base required")
	case args.Title == "":
		return nil, errors.New("gitops: title required")
	}

	apiBase := c.APIBase
	if apiBase == "" {
		apiBase = defaultGitHubAPIBase
	}
	apiBase = strings.TrimRight(apiBase, "/")

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls", apiBase, args.Owner, args.Repo)
	payload := map[string]any{
		"title": args.Title,
		"head":  args.Head,
		"base":  args.Base,
		"body":  args.Body,
	}
	raw, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("gitops: build pulls request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitops: open PR: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		brief := readBriefBody(resp.Body, 512)
		return nil, fmt.Errorf("gitops: open PR: %d: %s", resp.StatusCode, brief)
	}

	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitops: decode PR response: %w", err)
	}
	if out.Number == 0 || out.HTMLURL == "" {
		return nil, errors.New("gitops: PR response missing number or html_url")
	}
	return &OpenPRResult{
		PRNumber: out.Number,
		PRURL:    out.HTMLURL,
	}, nil
}

// readBriefBody reads up to limit bytes for inclusion in error
// messages. Larger bodies get truncated; tests pass small limits to
// pin assertions.
func readBriefBody(r io.Reader, limit int64) string {
	limited := io.LimitReader(r, limit)
	b, _ := io.ReadAll(limited)
	return strings.TrimSpace(string(b))
}
