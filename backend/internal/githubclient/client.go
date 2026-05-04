// Package githubclient is the backend's typed wrapper around the
// GitHub REST API for the small set of operations Fishhawk needs:
// fetching `.fishhawk/workflows.yaml` from a customer's repo at a
// given ref, and firing `workflow_dispatch` to start a runner job.
//
// Auth comes from a githubapp.TokenProvider passed at construction:
// every call resolves an installation token first, then uses it as
// an Authorization: Bearer <token> header. Token caching lives in
// the provider, not here.
//
// What's NOT in scope: a comprehensive GitHub SDK. We only cover
// the methods Fishhawk's flows demand. New methods land here as
// the dispatcher (#109), the PR-opening flow (E5.6 follow-on), and
// the audit-log render path need them.
package githubclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubapp"
)

// DefaultBaseURL is GitHub's REST API root. Tests override this
// via Client.BaseURL pointing at httptest fakes.
const DefaultBaseURL = "https://api.github.com"

// WorkflowSpecPath is the canonical location of the workflow spec
// in a customer's repo (per MVP_SPEC §4.1).
const WorkflowSpecPath = ".fishhawk/workflows.yaml"

// Errors callers may want to switch on.
var (
	// ErrNotFound means the resource (repo, file, workflow)
	// doesn't exist OR the App's installation lacks access. GitHub
	// returns 404 for both cases by design — we can't distinguish.
	ErrNotFound = errors.New("githubclient: not found")
	// ErrForbidden means the installation token was rejected (401)
	// or the App lacks permission for the request (403).
	ErrForbidden = errors.New("githubclient: forbidden")
	// ErrValidation means GitHub rejected the request as malformed
	// (422). Typical: bad ref name, missing required input.
	ErrValidation = errors.New("githubclient: validation failed")
)

// RepoRef identifies a GitHub repository by owner + name.
type RepoRef struct {
	Owner string
	Name  string
}

// String returns "owner/name" for use in log lines and URLs.
func (r RepoRef) String() string { return r.Owner + "/" + r.Name }

// FileContent is the decoded result of GetFile.
type FileContent struct {
	Path    string
	Content []byte
	// SHA is GitHub's blob SHA for the file's content at the ref.
	// Stable per content, so two refs pointing to the same content
	// produce the same SHA — the dedup we want for workflow_sha.
	SHA string
}

// Client wraps a TokenProvider and net/http.Client with the small
// set of GitHub operations Fishhawk needs. Concurrent use is safe.
type Client struct {
	// BaseURL is the API root. Empty → DefaultBaseURL.
	BaseURL string

	// Tokens issues installation-scoped Authorization tokens.
	Tokens githubapp.TokenProvider

	// HTTP is the underlying client. Defaults applied by New.
	HTTP *http.Client
}

// New returns a Client with sensible defaults. tokens is required;
// without it every call returns an error before touching the wire.
func New(tokens githubapp.TokenProvider) *Client {
	return &Client{
		Tokens: tokens,
		HTTP:   &http.Client{Timeout: 30 * time.Second},
	}
}

// GetFile fetches a single file from a repo at the given ref.
// path is relative to the repo root (no leading slash).
//
//	GET /repos/{owner}/{repo}/contents/{path}?ref={ref}
//
// The response carries content base64-encoded; we decode here so
// callers see []byte. Returns ErrNotFound if the file or repo
// isn't visible to the installation, ErrForbidden on auth issues.
func (c *Client) GetFile(ctx context.Context, installationID int64, repo RepoRef, path, ref string) (*FileContent, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if path == "" {
		return nil, errors.New("githubclient: path required")
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/contents/" + escapePath(path))
	if ref != "" {
		endpoint = endpoint + "?ref=" + url.QueryEscape(ref)
	}

	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: get file: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("get file", resp); err != nil {
		return nil, err
	}

	var body struct {
		Path     string `json:"path"`
		SHA      string `json:"sha"`
		Content  string `json:"content"`  // base64
		Encoding string `json:"encoding"` // "base64"
		Type     string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode file: %w", err)
	}
	if body.Type != "" && body.Type != "file" {
		return nil, fmt.Errorf("githubclient: %s is a %q, not a file", path, body.Type)
	}
	if body.Encoding != "base64" {
		return nil, fmt.Errorf("githubclient: unexpected content encoding %q", body.Encoding)
	}
	// GitHub wraps base64 at 60 chars; the standard decoder requires
	// the input to be unwrapped first (or padded properly).
	clean := strings.ReplaceAll(body.Content, "\n", "")
	decoded, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("githubclient: decode content: %w", err)
	}
	return &FileContent{
		Path:    body.Path,
		Content: decoded,
		SHA:     body.SHA,
	}, nil
}

// GetWorkflowSpec is the canonical wrapper around GetFile for
// `.fishhawk/workflows.yaml`. Callers want the content + the
// blob SHA (used as workflow_sha on Run records); having a single
// helper eliminates the per-call risk of a path typo.
func (c *Client) GetWorkflowSpec(ctx context.Context, installationID int64, repo RepoRef, ref string) (*FileContent, error) {
	return c.GetFile(ctx, installationID, repo, WorkflowSpecPath, ref)
}

// Issue is the slice of an issue payload Fishhawk surfaces for
// prompt construction. We deliberately don't expose the full
// GitHub Issue type — adding fields here is opt-in as new prompt
// templates need them.
type Issue struct {
	Number int
	Title  string
	Body   string
	State  string
}

// GetIssue fetches a single issue by number.
//
//	GET /repos/{owner}/{repo}/issues/{number}
//
// Used by the prompt-construction handler to build the
// agent-facing prompt from the originating issue. Returns
// ErrNotFound if the issue or repo isn't visible to the
// installation.
func (c *Client) GetIssue(ctx context.Context, installationID int64, repo RepoRef, number int) (*Issue, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if number <= 0 {
		return nil, errors.New("githubclient: issue number must be > 0")
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/issues/" + url.PathEscape(fmt.Sprintf("%d", number)))

	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: get issue: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("get issue", resp); err != nil {
		return nil, err
	}

	var body struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		State  string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode issue: %w", err)
	}
	return &Issue{
		Number: body.Number,
		Title:  body.Title,
		Body:   body.Body,
		State:  body.State,
	}, nil
}

// DispatchInputs is the JSON body of a workflow_dispatch event.
// Per GitHub's contract, inputs is a flat map[string]string —
// non-string values must be JSON-encoded by the caller.
type DispatchInputs map[string]string

// DispatchWorkflow fires a `workflow_dispatch` event for the given
// workflow file at ref.
//
//	POST /repos/{owner}/{repo}/actions/workflows/{workflow_file}/dispatches
//
// workflowFile is the YAML file name inside .github/workflows/
// (e.g. "fishhawk.yml"). ref is a branch / tag / commit SHA.
//
// On success returns nil; the customer's GitHub Actions runner
// picks up the event and starts the job. Returns ErrValidation
// for bad refs / unrecognized inputs (422), ErrNotFound for an
// unknown workflow file (404).
func (c *Client) DispatchWorkflow(ctx context.Context, installationID int64, repo RepoRef, workflowFile, ref string, inputs DispatchInputs) error {
	if c.Tokens == nil {
		return errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return errors.New("githubclient: repo owner and name required")
	}
	if workflowFile == "" {
		return errors.New("githubclient: workflowFile required")
	}
	if ref == "" {
		return errors.New("githubclient: ref required")
	}

	body := map[string]any{"ref": ref}
	if len(inputs) > 0 {
		body["inputs"] = inputs
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("githubclient: marshal dispatch body: %w", err)
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/actions/workflows/" + url.PathEscape(workflowFile) +
		"/dispatches")

	req, err := c.buildRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(raw), installationID)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("githubclient: dispatch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return classifyStatus("dispatch", resp)
}

// buildRequest constructs an http.Request with the standard
// GitHub headers (auth, accept, version). Centralized so every
// call site uses the same shape.
func (c *Client) buildRequest(ctx context.Context, method, url string, body io.Reader, installationID int64) (*http.Request, error) {
	token, err := c.Tokens.Token(ctx, installationID)
	if err != nil {
		return nil, fmt.Errorf("githubclient: get token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("githubclient: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return req, nil
}

// endpoint returns BaseURL + path, defaulting to api.github.com.
func (c *Client) endpoint(path string) string {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return base + path
}

// classifyStatus turns a non-2xx response into a typed error.
// 401/403 → ErrForbidden, 404 → ErrNotFound, 422 → ErrValidation,
// everything else gets a numeric prefix + body excerpt for
// observability.
func classifyStatus(op string, resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body := readBriefBody(resp.Body)
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s: %d: %s", ErrForbidden, op, resp.StatusCode, body)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s: %s", ErrNotFound, op, body)
	case http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %s: %s", ErrValidation, op, body)
	default:
		return fmt.Errorf("githubclient: %s: %d: %s", op, resp.StatusCode, body)
	}
}

// readBriefBody returns up to 256 bytes of the response body for
// inclusion in error messages. Caller closes the body.
func readBriefBody(r io.Reader) string {
	limited := io.LimitReader(r, 256)
	b, _ := io.ReadAll(limited)
	return strings.TrimSpace(string(b))
}

// escapePath URL-escapes a multi-segment path while preserving
// the slashes between segments. url.PathEscape escapes slashes,
// which would break GitHub's contents-API path matching.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return strings.Join(parts, "/")
}
