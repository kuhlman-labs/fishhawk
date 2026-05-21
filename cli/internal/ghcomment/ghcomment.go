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
// Scope notes (v0, deliberate cuts from the #416 acceptance list):
//   - Posts are append-only — each state transition gets a new
//     comment rather than editing a sticky one. Edit-in-place
//     requires persisting the comment id somewhere the CLI can
//     read; deferred to a follow-up.
//   - Posts as the operator's own GitHub identity, not the
//     Fishhawk App. For local dev this is arguably more honest
//     (the operator IS the one triggering the run) but the
//     authorship pattern differs from the GHA flow's bot-authored
//     comments. Worth flagging in CLAUDE.md once we settle.
package ghcomment

import (
	"errors"
	"fmt"
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
