package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// FeedbackProviderName is the feedback-registry id this provider
// registers under. It matches the work-item provider id so a single
// conventions `provider` value resolves both registries.
const FeedbackProviderName = "github_projects"

// markerPrefix opens the hidden fingerprint marker embedded in a filed
// report body. The dedup search filters on the full marker, so the
// writer and reader share this one constant — a drift between them is
// caught by the create-then-search test, independent of GitHub's live
// indexing behavior.
const markerPrefix = "<!-- fishhawk-fingerprint:"

// marker returns the hidden HTML-comment marker for a fingerprint. It is
// appended to a filed report body and is the exact substring the dedup
// search both queries for and re-verifies against a candidate's body.
func marker(fingerprint string) string {
	return markerPrefix + fingerprint + " -->"
}

// MatchedIssue is the minimal open-issue shape the dedup search needs:
// the number/URL to return and the body to re-verify the marker against.
type MatchedIssue struct {
	Number int
	URL    string
	Body   string
}

// FeedbackAPI is the slice of GitHub operations the feedback provider
// needs, declared consumer-side so the provider is unit-testable against
// a fake. *githubclient.Client satisfies CreateIssue and
// CreateIssueComment today; SearchOpenIssues is the dedup search the
// production client gains in a follow-up (the provider is wired
// run-scoped alongside the work-item provider, which is itself not yet
// registered at startup).
type FeedbackAPI interface {
	SearchOpenIssues(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, query string) ([]MatchedIssue, error)
	CreateIssue(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, p githubclient.CreateIssueParams) (*githubclient.CreatedIssue, error)
	CreateIssueComment(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, issueNumber int, body string) (*githubclient.IssueComment, error)
}

// FeedbackProvider is the GitHub product-feedback provider: it files
// fingerprint-marked product reports into the fixed upstream repo the
// Target names and dedups identical failures onto one report.
type FeedbackProvider struct {
	api FeedbackAPI
}

// NewFeedback returns a FeedbackProvider backed by api (in production a
// *githubclient.Client adapter).
func NewFeedback(api FeedbackAPI) *FeedbackProvider { return &FeedbackProvider{api: api} }

// Name implements workmgmt.FeedbackProvider.
func (*FeedbackProvider) Name() string { return FeedbackProviderName }

// SearchOpenByFingerprint searches the target repo's open issues for one
// already carrying the fingerprint marker. A search hit is re-verified
// against the candidate body so a fuzzy index match can never be mistaken
// for a real marker. Returns nil (no error) on a miss.
func (p *FeedbackProvider) SearchOpenByFingerprint(ctx context.Context, target workmgmt.Target, fingerprint string) (*workmgmt.ExistingReport, error) {
	repo, scope, err := p.resolve(target)
	if err != nil {
		return nil, err
	}
	mk := marker(fingerprint)
	query := fmt.Sprintf(`repo:%s/%s is:issue is:open in:body %q`, repo.Owner, repo.Name, mk)
	matches, err := p.api.SearchOpenIssues(ctx, scope, repo, query)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: search open reports: %w", err)
	}
	for _, m := range matches {
		// Re-verify: GitHub search is fuzzy, so only a body that actually
		// contains the exact marker counts as a dedup hit.
		if strings.Contains(m.Body, mk) {
			return &workmgmt.ExistingReport{Number: m.Number, URL: m.URL}, nil
		}
	}
	return nil, nil
}

// File creates a new product report with the fingerprint marker appended
// to the body so a later search can dedup against it.
func (p *FeedbackProvider) File(ctx context.Context, target workmgmt.Target, report workmgmt.FeedbackReport) (*workmgmt.CreatedItem, error) {
	repo, scope, err := p.resolve(target)
	if err != nil {
		return nil, err
	}
	body := strings.TrimRight(report.Body, "\n") + "\n\n" + marker(report.Fingerprint)
	issue, err := p.api.CreateIssue(ctx, scope, repo, githubclient.CreateIssueParams{
		Title:  report.Title,
		Body:   body,
		Labels: report.Labels,
	})
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: create product report: %w", err)
	}
	return &workmgmt.CreatedItem{
		Provider:      FeedbackProviderName,
		Number:        issue.Number,
		URL:           issue.HTMLURL,
		AppliedLabels: report.Labels,
	}, nil
}

// AppendOccurrence records another occurrence of the deduped failure as a
// comment on the existing report.
func (p *FeedbackProvider) AppendOccurrence(ctx context.Context, target workmgmt.Target, number int, note string) error {
	repo, scope, err := p.resolve(target)
	if err != nil {
		return err
	}
	if _, err := p.api.CreateIssueComment(ctx, scope, repo, number, note); err != nil {
		return fmt.Errorf("workmgmt/github: append occurrence to #%d: %w", number, err)
	}
	return nil
}

// resolve validates and unpacks the Target into the repo ref +
// installation id every call needs. Like the work-item provider it fails
// closed when no installation id is available: product-feedback egress is
// run-scoped in v0, so the source run must supply the installation.
func (p *FeedbackProvider) resolve(target workmgmt.Target) (githubclient.RepoRef, forge.CredentialScope, error) {
	if p.api == nil {
		return githubclient.RepoRef{}, forge.CredentialScope{}, errors.New("workmgmt/github: feedback provider missing API client")
	}
	if target.Repo.Owner == "" || target.Repo.Name == "" {
		return githubclient.RepoRef{}, forge.CredentialScope{}, errors.New("workmgmt/github: target repo owner and name required")
	}
	if target.Scope.IsZero() {
		return githubclient.RepoRef{}, forge.CredentialScope{}, errors.New("workmgmt/github: no installation id available; product-feedback egress is run-scoped in v0 — file from a run whose installation can act on the product repo")
	}
	return githubclient.RepoRef{Owner: target.Repo.Owner, Name: target.Repo.Name}, target.Scope, nil
}
