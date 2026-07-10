package releaseevidence

import (
	"context"
	"fmt"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
)

// GitHubResolver is the production MergedPRResolver: it enumerates the
// commits in (base, head] via CompareCommits, then associates each
// landing commit with its merged PR via ListPullRequestsForCommit,
// de-duped by PR number. The pgtest assembly tests fake the resolver;
// this is the only wiring that touches GitHub.
type GitHubResolver struct {
	Client         *githubclient.Client
	InstallationID int64
}

// MergedPRsInRange resolves the merged PRs whose landing commits fall in
// (base, head]. A CompareCommits or ListPullRequestsForCommit failure is
// returned as an error — MergedPRsInRange fails CLOSED: the caller (the
// assembler) propagates the error and the release-notes endpoint returns a
// non-2xx rather than rendering notes from a partial commit walk. PRs are
// de-duped by number, so a PR whose merge commit plus follow-on commits both
// appear in range yields a single MergedPR.
func (g *GitHubResolver) MergedPRsInRange(ctx context.Context, repo, base, head string) ([]MergedPR, error) {
	ref, err := parseRepoRef(repo)
	if err != nil {
		return nil, err
	}
	shas, err := g.Client.CompareCommits(ctx, g.InstallationID, ref, base, head)
	if err != nil {
		return nil, err
	}
	seen := make(map[int]struct{})
	var out []MergedPR
	for _, sha := range shas {
		prs, err := g.Client.ListPullRequestsForCommit(ctx, g.InstallationID, ref, sha)
		if err != nil {
			return nil, err
		}
		for _, pr := range prs {
			if _, ok := seen[pr.Number]; ok {
				continue
			}
			seen[pr.Number] = struct{}{}
			out = append(out, MergedPR{
				URL:      pr.URL,
				Number:   pr.Number,
				Title:    pr.Title,
				MergeSHA: sha,
			})
		}
	}
	return out, nil
}

// parseRepoRef splits an "owner/name" repo string into a
// githubclient.RepoRef. It rejects a string that is not exactly two
// non-empty slash-separated segments.
func parseRepoRef(repo string) (githubclient.RepoRef, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return githubclient.RepoRef{}, fmt.Errorf("releaseevidence: repo must be owner/name, got %q", repo)
	}
	return githubclient.RepoRef{Owner: owner, Name: name}, nil
}
