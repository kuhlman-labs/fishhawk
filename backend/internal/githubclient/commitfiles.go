package githubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// commitFilesCap is GitHub's per-response changed-file ceiling for the
// "Get a commit" endpoint: a commit touching more than 300 files returns the
// first 300 and paginates the rest via Link headers.
const commitFilesCap = 300

// CommitFile is one changed file of a commit as the REST commit endpoint
// reports it.
type CommitFile struct {
	Path         string
	PreviousPath string // rename source; empty unless Status is "renamed"
	Status       string
	Additions    int
	Deletions    int
}

// CommitFiles is a commit's file-level diff. Truncated is set when the list
// is not known to be complete — GitHub paginated it (a Link rel="next", or a
// page at the 300-file cap) or a file entry omitted its additions/deletions
// counts — and TruncationReason names which. A caller comparing diffs must
// treat a truncated list as unknown, never as complete.
type CommitFiles struct {
	SHA              string
	Files            []CommitFile
	Truncated        bool
	TruncationReason string
}

// GetCommitFiles fetches a commit's changed-file list with per-file
// additions/deletions (E82.2 / #3779 — the revert attestation's forge diff).
//
//	GET /repos/{owner}/{repo}/commits/{sha}
//
// Only the first page is read. Returns ErrNotFound when the repo/commit isn't
// visible, ErrForbidden on auth issues, ErrValidation on a 422; any other
// non-2xx is an untyped status error via classifyStatus.
func (c *Client) GetCommitFiles(ctx context.Context, scope forge.CredentialScope, repo RepoRef, sha string) (*CommitFiles, error) {
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
		return nil, errors.New("githubclient: commit sha required")
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/commits/" + escapePath(sha))
	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: get commit files: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := classifyStatus("get commit files", resp); err != nil {
		return nil, err
	}
	var body struct {
		SHA   string `json:"sha"`
		Files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
			Status           string `json:"status"`
			// Pointers: an absent count must stay distinguishable from 0.
			Additions *int `json:"additions"`
			Deletions *int `json:"deletions"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode commit files: %w", err)
	}

	out := &CommitFiles{SHA: body.SHA, Files: make([]CommitFile, 0, len(body.Files))}
	for _, f := range body.Files {
		if f.Additions == nil || f.Deletions == nil {
			out.Truncated = true
			out.TruncationReason = "a changed-file entry omitted its additions/deletions counts"
			continue
		}
		out.Files = append(out.Files, CommitFile{
			Path:         f.Filename,
			PreviousPath: f.PreviousFilename,
			Status:       f.Status,
			Additions:    *f.Additions,
			Deletions:    *f.Deletions,
		})
	}
	// Pagination wins the reason: whole files are missing, not just counts.
	if strings.Contains(resp.Header.Get("Link"), `rel="next"`) || len(body.Files) >= commitFilesCap {
		out.Truncated = true
		out.TruncationReason = fmt.Sprintf("changed-file list paginated at GitHub's %d-file page; later pages not read", commitFilesCap)
	}
	return out, nil
}
