package opsmonitor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
)

// Notifier delivers one failing result to the operator.
type Notifier interface {
	Notify(ctx context.Context, r Result, deployment Deployment) error
}

// IssueCommentClient is the narrow GitHub App client pair the issue-comment
// notifier needs; *githubclient.Client satisfies it.
type IssueCommentClient interface {
	GetRepoInstallation(ctx context.Context, repo forge.RepoRef) (int64, error)
	CreateIssueComment(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, issueNumber int, body string) (*githubclient.IssueComment, error)
}

// IssueCommentNotifier posts each failing condition as a comment on ONE
// operator-configured issue. It deliberately bypasses the run-anchored
// issuecomment.Channel/Router: a host condition has no run. The target issue
// must be access-controlled — the comment names pids, run ids and host
// metrics (it renders nothing from Result.Detail).
type IssueCommentNotifier struct {
	Client      IssueCommentClient
	Repo        forge.RepoRef
	Issue       int
	QuietWindow time.Duration // rendered in the comment; default DefaultQuietWindow
}

// Notify resolves the repository's installation scope (mirroring
// server.resolveRepoScope: forge.ErrNotInstalled → the zero scope, any other
// error returned without posting) and posts RenderComment.
func (n IssueCommentNotifier) Notify(ctx context.Context, r Result, deployment Deployment) error {
	if n.Client == nil {
		return errors.New("opsmonitor: issue-comment notifier has no GitHub client")
	}
	var scope forge.CredentialScope
	instID, err := n.Client.GetRepoInstallation(ctx, n.Repo)
	switch {
	case err == nil:
		scope = forge.FromGitHubInstallationID(instID)
	case errors.Is(err, forge.ErrNotInstalled):
	default:
		return fmt.Errorf("opsmonitor: resolve installation for %s/%s: %w", n.Repo.Owner, n.Repo.Name, err)
	}
	quiet := n.QuietWindow
	if quiet <= 0 {
		quiet = DefaultQuietWindow
	}
	if _, err := n.Client.CreateIssueComment(ctx, scope, n.Repo, n.Issue, RenderComment(r, deployment, quiet)); err != nil {
		return fmt.Errorf("opsmonitor: post comment on %s/%s#%d: %w", n.Repo.Owner, n.Repo.Name, n.Issue, err)
	}
	return nil
}

// ParseIssueRef parses the strict `owner/name#N` notify-issue form: exactly
// one '/', exactly one '#', non-empty owner and name, positive N.
func ParseIssueRef(raw string) (forge.RepoRef, int, error) {
	const shape = "want owner/name#N, e.g. kuhlman-labs/fishhawk#123"
	if strings.Count(raw, "/") != 1 || strings.Count(raw, "#") != 1 {
		return forge.RepoRef{}, 0, fmt.Errorf("opsmonitor: notify issue %q: %s", raw, shape)
	}
	repoPart, numPart, _ := strings.Cut(raw, "#")
	owner, name, _ := strings.Cut(repoPart, "/")
	if strings.TrimSpace(owner) != owner || strings.TrimSpace(name) != name || owner == "" || name == "" {
		return forge.RepoRef{}, 0, fmt.Errorf("opsmonitor: notify issue %q: empty or padded owner/name (%s)", raw, shape)
	}
	n, err := strconv.Atoi(numPart)
	if err != nil || n <= 0 {
		return forge.RepoRef{}, 0, fmt.Errorf("opsmonitor: notify issue %q: issue number must be a positive integer (%s)", raw, shape)
	}
	return forge.RepoRef{Owner: owner, Name: name}, n, nil
}

// RenderComment renders the operator comment. It carries a hidden marker, the
// condition, probe, observed vs threshold, deployment and remedy — and
// deliberately NOTHING from Detail (host paths, process argv), because the
// target issue may be more widely readable than the host.
func RenderComment(r Result, deployment Deployment, quiet time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- fishhawk-ops-monitor:v1 condition=%s -->\n", r.Condition)
	fmt.Fprintf(&b, "### Fishhawk ops monitor: %s\n\n", r.Condition)
	fmt.Fprintf(&b, "- **Probe:** %s\n", r.Probe)
	if r.Subject != "" {
		fmt.Fprintf(&b, "- **Subject:** %s\n", r.Subject)
	}
	fmt.Fprintf(&b, "- **Observed:** %s\n", r.Observed)
	fmt.Fprintf(&b, "- **Threshold:** %s\n", r.Threshold)
	fmt.Fprintf(&b, "- **Deployment:** %s\n\n", deployment)
	fmt.Fprintf(&b, "**Remedy:** %s\n\n", r.Remedy)
	fmt.Fprintf(&b, "Repeats of this condition for the same subject are suppressed for %s.\n", quiet)
	return b.String()
}
