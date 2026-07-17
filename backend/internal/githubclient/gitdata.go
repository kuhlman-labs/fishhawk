package githubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// GetRepository fetches repository metadata.
//
//	GET /repos/{owner}/{repo}
//
// The onboarding scaffolder (E29.7) calls it to resolve the default
// branch, which is both the base ref for the scaffold PR and the ref
// whose HEAD commit seeds the create-tree base_tree. Returns ErrNotFound
// when the repo isn't visible to the installation, ErrForbidden on auth
// issues.
func (c *Client) GetRepository(ctx context.Context, scope forge.CredentialScope, repo RepoRef) (*Repository, error) {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name))
	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: get repository: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("get repository", resp); err != nil {
		return nil, err
	}
	var body struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode repository: %w", err)
	}
	if body.DefaultBranch == "" {
		return nil, fmt.Errorf("githubclient: repository response missing default_branch")
	}
	return &Repository{DefaultBranch: body.DefaultBranch}, nil
}

// GetCommit fetches a git commit object by SHA (the Git Data API, not the
// higher-level repo-commit endpoint).
//
//	GET /repos/{owner}/{repo}/git/commits/{sha}
//
// The onboarding scaffolder (E29.7) calls it to resolve the default
// branch HEAD commit's TREE sha — create-tree's base_tree must be a tree
// sha, not a commit sha, so the repo's existing files are preserved.
// Returns ErrNotFound when the repo/commit isn't visible, ErrForbidden on
// auth issues.
func (c *Client) GetCommit(ctx context.Context, scope forge.CredentialScope, repo RepoRef, sha string) (*GitCommit, error) {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if sha == "" {
		return nil, errors.New("githubclient: commit sha is required")
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/git/commits/" + url.PathEscape(sha))
	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: get commit: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("get commit", resp); err != nil {
		return nil, err
	}
	var body struct {
		SHA  string `json:"sha"`
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode commit: %w", err)
	}
	if body.Tree.SHA == "" {
		return nil, fmt.Errorf("githubclient: commit response missing tree.sha")
	}
	return &GitCommit{SHA: body.SHA, TreeSHA: body.Tree.SHA}, nil
}

// CreateTree creates a new git tree that layers entries on top of
// baseTree, preserving every existing path the base tree carried.
//
//	POST /repos/{owner}/{repo}/git/trees
//	{ "base_tree": "<sha>", "tree": [{path, mode:"100644", type:"blob", content}] }
//
// baseTree MUST be a TREE sha (resolve it via GetCommit on the branch
// HEAD), not a commit sha — passing a commit sha drops the repo's
// existing files. Each entry is a regular file (mode 100644). Returns the
// new tree's SHA. ErrNotFound when the repo isn't visible, ErrForbidden
// on auth issues, ErrValidation when GitHub rejects the tree (422 — e.g.
// a bad base_tree sha).
func (c *Client) CreateTree(ctx context.Context, scope forge.CredentialScope, repo RepoRef,
	baseTree string, entries []TreeEntry) (string, error) {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return "", err
	}
	if c.Tokens == nil {
		return "", errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return "", errors.New("githubclient: repo owner and name required")
	}
	if len(entries) == 0 {
		return "", errors.New("githubclient: at least one tree entry is required")
	}

	type wireEntry struct {
		Path    string `json:"path"`
		Mode    string `json:"mode"`
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	wire := struct {
		BaseTree string      `json:"base_tree,omitempty"`
		Tree     []wireEntry `json:"tree"`
	}{BaseTree: baseTree}
	for _, e := range entries {
		if e.Path == "" {
			return "", errors.New("githubclient: tree entry path is required")
		}
		wire.Tree = append(wire.Tree, wireEntry{
			Path:    e.Path,
			Mode:    "100644",
			Type:    "blob",
			Content: e.Content,
		})
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return "", fmt.Errorf("githubclient: marshal create tree: %w", err)
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) + "/git/trees")
	req, err := c.buildRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(raw), installationID)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("githubclient: create tree: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("create tree", resp); err != nil {
		return "", err
	}
	var body struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("githubclient: decode create tree: %w", err)
	}
	if body.SHA == "" {
		return "", fmt.Errorf("githubclient: create tree response missing sha")
	}
	return body.SHA, nil
}

// CreateCommit creates a git commit pointing at treeSHA with the given
// parents.
//
//	POST /repos/{owner}/{repo}/git/commits
//	{ "message": "<msg>", "tree": "<treeSHA>", "parents": ["<parentSHA>"] }
//
// The onboarding scaffolder passes the new tree from CreateTree and the
// branch HEAD as the single parent, producing a fast-forwardable commit
// the scaffold branch ref will point at. Returns the new commit's SHA.
// ErrNotFound when the repo isn't visible, ErrForbidden on auth issues,
// ErrValidation when GitHub rejects the commit (422 — e.g. a bad tree or
// parent sha).
func (c *Client) CreateCommit(ctx context.Context, scope forge.CredentialScope, repo RepoRef,
	message, treeSHA string, parents []string) (string, error) {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return "", err
	}
	if c.Tokens == nil {
		return "", errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return "", errors.New("githubclient: repo owner and name required")
	}
	if message == "" {
		return "", errors.New("githubclient: commit message is required")
	}
	if treeSHA == "" {
		return "", errors.New("githubclient: tree sha is required")
	}

	// parents may legitimately be empty for an initial commit, but the
	// onboarding path always builds on a branch HEAD; keep parents nil-safe
	// by defaulting to an empty slice so it serializes as [] not null.
	if parents == nil {
		parents = []string{}
	}
	raw, err := json.Marshal(map[string]any{
		"message": message,
		"tree":    treeSHA,
		"parents": parents,
	})
	if err != nil {
		return "", fmt.Errorf("githubclient: marshal create commit: %w", err)
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) + "/git/commits")
	req, err := c.buildRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(raw), installationID)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("githubclient: create commit: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("create commit", resp); err != nil {
		return "", err
	}
	var body struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("githubclient: decode create commit: %w", err)
	}
	if body.SHA == "" {
		return "", fmt.Errorf("githubclient: create commit response missing sha")
	}
	return body.SHA, nil
}
