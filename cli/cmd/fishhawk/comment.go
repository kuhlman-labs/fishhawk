package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/ghcomment"
	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// maybePostLocalComment fires the CLI-side issue comment when the
// run is the local-issue shape that warrants one — `runner_kind=local`
// AND `issue_context` populated. All failures are best-effort: a
// warning to stderr, no change to the verb's exit code.
//
// Gating: backend's IssueNotifier handles webhook-dispatched runs
// (which carry an installation_id). For local runs the backend's
// notifier silently skips (the nil-installation_id branch in
// contextForStatus); this function fills the gap.
func maybePostLocalComment(stderr io.Writer, r *httpclient.Run, body string) {
	if r == nil || r.RunnerKind != "local" || r.IssueContext == nil || body == "" {
		return
	}
	if err := ghcomment.Post(r.Repo, r.IssueContext.Number, body); err != nil {
		if errors.Is(err, ghcomment.ErrGhNotInstalled) {
			// Don't double-warn — the kickoff path already
			// surfaced the missing gh; quieter on subsequent
			// transitions.
			return
		}
		_, _ = fmt.Fprintf(stderr,
			"fishhawk: comment on issue #%d failed: %v\n",
			r.IssueContext.Number, err)
	}
}

// toGhCommentRun adapts httpclient.Run to ghcomment.Run. Threads
// the backend URL through as ExternalURL so the comment's
// run-detail link points at the same surface the CLI is talking
// to (works correctly across dev, staging, prod).
func toGhCommentRun(r *httpclient.Run, externalURL string) ghcomment.Run {
	pr := ""
	if r.PullRequestURL != nil {
		pr = *r.PullRequestURL
	}
	return ghcomment.Run{
		ID:             r.ID,
		WorkflowID:     r.WorkflowID,
		State:          r.State,
		RunnerKind:     r.RunnerKind,
		PullRequestURL: pr,
		ExternalURL:    externalURL,
	}
}

// ghCurrentUserCommand is exposed as a var so tests can swap a
// fake. Production wires exec.Command("gh", "api", ...).
var ghCurrentUserCommand = exec.Command

// resolveGitHubHandle returns the operator's GitHub login by
// shelling to `gh api user --jq .login`. Returns empty when gh
// is missing or the call fails — the renderer drops the "by @user"
// clause cleanly on empty input.
func resolveGitHubHandle() string {
	if _, err := exec.LookPath("gh"); err != nil {
		return ""
	}
	cmd := ghCurrentUserCommand("gh", "api", "user", "--jq", ".login")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// fetchRunForComment loads the run by id. Returns nil + nil on
// any error (best-effort: comment failures don't propagate). The
// log noise tradeoff: silent vs. stderr warning. We pick silent
// here because the surrounding verb already reported success
// against the API; a follow-up "couldn't load run for the
// comment" would be a distracting tail when the operator
// already saw the canonical result. Aggregate failure paths
// land in the explicit ghcomment.Post warning instead.
func fetchRunForComment(ctx context.Context, client *httpclient.Client, runID uuid.UUID) *httpclient.Run {
	if client == nil {
		return nil
	}
	r, err := client.GetRun(ctx, runID)
	if err != nil {
		return nil
	}
	return r
}
