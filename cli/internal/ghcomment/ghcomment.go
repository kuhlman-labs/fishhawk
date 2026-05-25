// Package ghcomment renders + posts GitHub-issue comments for
// Fishhawk runs minted outside the webhook flow (#416).
//
// The backend's IssueNotifier is the canonical sticky-comment
// surface for webhook-dispatched runs — it uses a GitHub App
// installation token, edits its previous comment in place, and
// runs from the request-handling hot path. For local-runner runs
// (runner_kind=local) the backend has no installation token and
// silently skips the post; this package fills the gap by shelling
// to the operator's authed `gh` from the CLI side.
//
// v0 (post-#428): PostOrEditStatusComment fetches the rendered body
// from the backend, edits the sticky comment in place when a prior
// comment id is stored, and records the resulting id back to the
// backend — matching the GHA sticky-comment parity shape. Posts as
// the operator's own GitHub identity, not the Fishhawk App.
package ghcomment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// ErrGhNotInstalled signals the `gh` binary is missing on PATH.
// Callers warn and proceed; comment posting is best-effort, not
// a fail-the-verb condition.
var ErrGhNotInstalled = errors.New("gh CLI not found on PATH; install https://cli.github.com to enable issue comments")

// ghCommentCommand is the subprocess Post() shells to. Exposed as
// a var so tests can swap in a recording fake. Production wires
// exec.Command.
var ghCommentCommand = exec.Command

// ghLookPath is the PATH lookup used in tests via swap.
var ghLookPath = exec.LookPath

// Run carries the subset of httpclient.Run fields the renderer
// needs. Defined here so the package doesn't import the httpclient
// package — avoids a cycle if the httpclient ever wants to use
// the renderer for its own logging.
type Run struct {
	ID             uuid.UUID
	WorkflowID     string
	State          string
	RunnerKind     string
	PullRequestURL string // optional; surfaces in stage-complete after implement
	ExternalURL    string // backend's external URL prefix, e.g. http://localhost:8080
}

// shortID is the first 8 hex chars of the run id. Mirrors the
// backend's shortID in issuecomment/status_template.go so the
// references look consistent across surfaces.
func shortID(id uuid.UUID) string {
	s := id.String()
	if len(s) < 8 {
		return s
	}
	return s[:8]
}

// runURL is the canonical run-detail page URL the comments link
// to. Pre-computed once per render so the body builders stay
// readable.
func runURL(r Run) string {
	base := strings.TrimRight(r.ExternalURL, "/")
	return base + "/runs/" + r.ID.String()
}

// RenderKickoff is the comment posted after `fishhawk run start`
// successfully creates an issue-triggered local run. Names the
// run, the workflow, and links back to the run-detail page so a
// reviewer can follow along.
//
// Deprecated: use PostOrEditStatusComment.
func RenderKickoff(r Run) string {
	return fmt.Sprintf(
		"Fishhawk picked this up. Local run [`%s`](%s) on workflow `%s` — plan stage queued.",
		shortID(r.ID), runURL(r), r.WorkflowID,
	)
}

// RenderPlanApproved is posted by `fishhawk plan approve`.
// `approver` is the operator's GitHub handle when resolvable, or
// empty when not — the renderer drops the "by …" clause if blank
// so the comment still reads cleanly.
//
// Deprecated: use PostOrEditStatusComment.
func RenderPlanApproved(r Run, approver string) string {
	var by string
	if approver != "" {
		by = " by @" + approver
	}
	return fmt.Sprintf(
		"Plan approved%s. Run [`%s`](%s) — implement stage queued.",
		by, shortID(r.ID), runURL(r),
	)
}

// RenderPlanRejected is posted by `fishhawk plan reject`. The
// optional comment from the operator surfaces on its own line so
// the reviewer can read the rejection reason without clicking
// through.
//
// Deprecated: use PostOrEditStatusComment.
func RenderPlanRejected(r Run, approver, reason string) string {
	var by string
	if approver != "" {
		by = " by @" + approver
	}
	body := fmt.Sprintf(
		"Plan rejected%s. Run [`%s`](%s) — ended at plan stage.",
		by, shortID(r.ID), runURL(r),
	)
	if reason = strings.TrimSpace(reason); reason != "" {
		body += "\n\n> " + reason
	}
	return body
}

// RenderRunCancelled is posted by `fishhawk run cancel`.
//
// Deprecated: use PostOrEditStatusComment.
func RenderRunCancelled(r Run, canceller string) string {
	var by string
	if canceller != "" {
		by = " by @" + canceller
	}
	return fmt.Sprintf(
		"Run [`%s`](%s) cancelled%s.",
		shortID(r.ID), runURL(r), by,
	)
}

// RenderImplementPROpened is posted by `fishhawk runner start` after
// the auto-PR flow succeeds (implement stage, --no-pr absent). Links
// to the PR so the reviewer can click through immediately; also links
// the run for the audit trail.
//
// Deprecated: use PostOrEditStatusComment.
func RenderImplementPROpened(r Run, prURL string, prNumber int) string {
	return fmt.Sprintf(
		"✓ Fishhawk implement stage opened [PR #%d](%s).\n\nRun [`%s`](%s) — awaiting review.",
		prNumber, prURL, shortID(r.ID), runURL(r),
	)
}

// RenderStageComplete is posted by `fishhawk runner start` once
// the runner subprocess exits cleanly. `stageType` is the
// spec-level type (plan / implement / review); `stateAfter` is
// the run-side state the stage settled into (succeeded /
// awaiting_approval / failed). The PullRequestURL surfaces only
// when the implement stage produced one.
//
// Deprecated: use PostOrEditStatusComment.
func RenderStageComplete(r Run, stageType, stateAfter string) string {
	body := fmt.Sprintf(
		"`%s` stage complete on run [`%s`](%s) — %s.",
		stageType, shortID(r.ID), runURL(r), stateAfter,
	)
	if r.PullRequestURL != "" {
		body += "\n\nPR: " + r.PullRequestURL
	}
	return body
}

// StatusCommentResponse is the JSON shape GET /v0/runs/{id}/status-comment
// returns (#428). Used by FetchStatus to decode the backend response.
type StatusCommentResponse struct {
	Body            string `json:"body"`
	IssueNumber     int    `json:"issue_number"`
	Repo            string `json:"repo"`
	GithubCommentID int64  `json:"github_comment_id"`
}

// FetchStatus calls GET /v0/runs/{runID}/status-comment on the backend and
// returns the rendered sticky-comment body plus the most-recent stored
// github_comment_id. Returns an error when the backend is unreachable, the
// run is not found, or the response is not parseable.
func FetchStatus(backendURL, runID string) (StatusCommentResponse, error) {
	url := strings.TrimRight(backendURL, "/") + "/v0/runs/" + runID + "/status-comment"
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return StatusCommentResponse{}, fmt.Errorf("ghcomment: fetch status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return StatusCommentResponse{}, fmt.Errorf("ghcomment: fetch status: HTTP %d", resp.StatusCode)
	}
	var result StatusCommentResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return StatusCommentResponse{}, fmt.Errorf("ghcomment: fetch status: decode: %w", err)
	}
	return result, nil
}

// EditOrCreate edits the GitHub comment at githubCommentID in place when
// githubCommentID > 0, creating a new comment otherwise. On a 404 from the
// PATCH (comment deleted by a reviewer), falls back to creating a new one.
// Returns the canonical comment id after the operation.
func EditOrCreate(repo string, issueNumber int, githubCommentID int64, body string) (int64, error) {
	if _, err := ghLookPath("gh"); err != nil {
		return 0, ErrGhNotInstalled
	}
	if strings.TrimSpace(repo) == "" {
		return 0, fmt.Errorf("ghcomment: empty repo")
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return 0, fmt.Errorf("ghcomment: repo must be in owner/name format: %q", repo)
	}
	if issueNumber <= 0 {
		return 0, fmt.Errorf("ghcomment: invalid issue number %d", issueNumber)
	}
	if strings.TrimSpace(body) == "" {
		return 0, fmt.Errorf("ghcomment: empty body")
	}

	if githubCommentID > 0 {
		commentPath := fmt.Sprintf("/repos/%s/%s/issues/comments/%d", owner, name, githubCommentID)
		cmd := ghCommentCommand("gh", "api", commentPath, "-X", "PATCH",
			"-f", "body="+body, "--jq", ".id")
		out, err := cmd.CombinedOutput()
		if err != nil {
			trimmed := strings.TrimSpace(string(out))
			if strings.Contains(trimmed, "HTTP 404") {
				// Comment was deleted; fall through to create a fresh one.
				githubCommentID = 0
			} else {
				if trimmed != "" {
					return 0, fmt.Errorf("gh api PATCH: %s", trimmed)
				}
				return 0, fmt.Errorf("gh api PATCH: %w", err)
			}
		} else {
			id, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
			if perr != nil {
				return 0, fmt.Errorf("ghcomment: parse comment id from PATCH response: %w", perr)
			}
			return id, nil
		}
	}
	_ = githubCommentID // 0 at this point; fall through to create

	issuePath := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, name, issueNumber)
	cmd := ghCommentCommand("gh", "api", issuePath, "-X", "POST",
		"-f", "body="+body, "--jq", ".id")
	out, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed != "" {
			return 0, fmt.Errorf("gh api POST: %s", trimmed)
		}
		return 0, fmt.Errorf("gh api POST: %w", err)
	}
	id, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if perr != nil {
		return 0, fmt.Errorf("ghcomment: parse comment id from POST response: %w", perr)
	}
	return id, nil
}

// RecordCommentID calls POST /v0/runs/{runID}/status-comment to record the
// github_comment_id in the backend's audit log so the GHA-side Notifier can
// pick it up on subsequent transitions.
func RecordCommentID(backendURL, runID string, githubCommentID int64) error {
	url := strings.TrimRight(backendURL, "/") + "/v0/runs/" + runID + "/status-comment"
	payload, _ := json.Marshal(map[string]any{"github_comment_id": githubCommentID})
	resp, err := http.Post(url, "application/json", bytes.NewReader(payload)) //nolint:noctx
	if err != nil {
		return fmt.Errorf("ghcomment: record comment id: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("ghcomment: record comment id: HTTP %d", resp.StatusCode)
	}
	return nil
}

// PostOrEditStatusComment fetches the rendered sticky-comment body from the
// backend, creates or edits the GitHub comment in place, and records the
// resulting comment id back to the backend (#428). Best-effort for callers:
// errors are returned so the caller can warn and continue.
func PostOrEditStatusComment(backendURL, runID, repo string, issueNumber int) error {
	sc, err := FetchStatus(backendURL, runID)
	if err != nil {
		return fmt.Errorf("ghcomment: fetch rendered body: %w", err)
	}
	newID, err := EditOrCreate(repo, issueNumber, sc.GithubCommentID, sc.Body)
	if err != nil {
		return err
	}
	return RecordCommentID(backendURL, runID, newID)
}

// Post shells to `gh issue comment <number> --repo <owner/name>
// --body <body>` and returns once the subprocess exits. Returns
// ErrGhNotInstalled when the binary is missing — callers warn and
// proceed without failing the verb.
func Post(repo string, issueNumber int, body string) error {
	if _, err := ghLookPath("gh"); err != nil {
		return ErrGhNotInstalled
	}
	if strings.TrimSpace(repo) == "" {
		return fmt.Errorf("ghcomment: empty repo")
	}
	if issueNumber <= 0 {
		return fmt.Errorf("ghcomment: invalid issue number %d", issueNumber)
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("ghcomment: empty body")
	}
	cmd := ghCommentCommand("gh", "issue", "comment", strconv.Itoa(issueNumber),
		"--repo", repo, "--body", body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Surface gh's stderr verbatim so the operator can see
		// `not authorized` / `repo not found` / etc.
		trimmed := strings.TrimSpace(string(out))
		if trimmed != "" {
			return fmt.Errorf("gh issue comment: %s", trimmed)
		}
		return fmt.Errorf("gh issue comment: %w", err)
	}
	return nil
}
