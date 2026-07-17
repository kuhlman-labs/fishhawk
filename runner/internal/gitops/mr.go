package gitops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OpenMRClient creates merge requests via GitLab's REST v4 API. It is the
// GitLab-forge parallel of OpenPRClient: the runner selects it in
// openPRAndShipArtifact when --forge=gitlab. Unlike the GitHub client there is
// NO default host — GitLab is deployed self-managed as often as on gitlab.com,
// so BaseURL is required and OpenMR fails closed when it is empty rather than
// silently targeting gitlab.com.
type OpenMRClient struct {
	// HTTP defaults to a 30s-timeout client when nil. Tests override.
	HTTP *http.Client

	// BaseURL is the GitLab instance root (e.g. https://gitlab.com or a
	// self-managed https://gitlab.example.com). Required — there is no
	// default. Tests point at httptest.
	BaseURL string

	// Token is the GitLab access token used as the PRIVATE-TOKEN header
	// (a group/project access token with api scope in v0). Required.
	Token string
}

// OpenMRArgs collects the inputs for a single merge-request creation.
type OpenMRArgs struct {
	// ProjectPath is the namespaced project path ("group/subgroup/project").
	// It rides RepoRef.Owner+"/"+Name from the runner and may carry nested
	// groups; OpenMR percent-encodes it into a single path segment.
	ProjectPath  string
	SourceBranch string // "fishhawk/run-aaa/stage-bbb"
	TargetBranch string // "main"
	Title        string
	Description  string
}

// OpenMR posts a merge-request creation request and returns the resulting
// (iid, web_url) mapped onto the shared OpenPRResult shape the runner's
// change-request artifact upload consumes (PRNumber carries the MR iid).
// Authenticates with GitLab's `PRIVATE-TOKEN` header. Single-attempt, matching
// OpenPR: MR creation is not idempotent on the GitLab side, so retrying a
// transient failure could create a duplicate. The caller surfaces any error to
// the audit log.
func (c *OpenMRClient) OpenMR(ctx context.Context, args OpenMRArgs) (*OpenPRResult, error) {
	switch {
	case c.Token == "":
		return nil, errors.New("gitops: token required")
	case c.BaseURL == "":
		return nil, errors.New("gitops: gitlab base URL required")
	case args.ProjectPath == "":
		return nil, errors.New("gitops: project path required")
	case args.SourceBranch == "" || args.TargetBranch == "":
		return nil, errors.New("gitops: source and target branch required")
	case args.Title == "":
		return nil, errors.New("gitops: title required")
	}

	base := strings.TrimRight(c.BaseURL, "/")

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	// url.PathEscape encodes '/' as %2F, collapsing a nested namespace
	// (group/subgroup/project) into the single path segment GitLab's
	// namespaced-path routing requires.
	endpoint := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests", base, url.PathEscape(args.ProjectPath))
	payload := map[string]any{
		"source_branch": args.SourceBranch,
		"target_branch": args.TargetBranch,
		"title":         args.Title,
		"description":   args.Description,
	}
	raw, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("gitops: build merge_requests request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitops: open MR: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		brief := readBriefBody(resp.Body, 512)
		return nil, fmt.Errorf("gitops: open MR: %d: %s", resp.StatusCode, brief)
	}

	var out struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitops: decode MR response: %w", err)
	}
	if out.IID == 0 || out.WebURL == "" {
		return nil, errors.New("gitops: MR response missing iid or web_url")
	}
	return &OpenPRResult{
		PRNumber: out.IID,
		PRURL:    out.WebURL,
	}, nil
}
