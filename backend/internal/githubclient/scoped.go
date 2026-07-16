package githubclient

import (
	"context"
	"fmt"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubapp"
	"github.com/kuhlman-labs/fishhawk/backend/internal/securityscan"
)

// credentialTokens adapts a forge.CredentialProvider into a
// githubapp.TokenProvider by wrapping the int64 installation id into a
// forge.CredentialScope before delegating. This lets a Client
// constructed via NewWithCredentialProvider serve BOTH the int64
// methods and the Scoped variants through the one credential source.
type credentialTokens struct {
	provider forge.CredentialProvider
}

func (c *credentialTokens) Token(ctx context.Context, installationID int64) (string, error) {
	return c.provider.Token(ctx, forge.FromGitHubInstallationID(installationID))
}

// NewWithCredentialProvider constructs a Client whose token source is a
// forge-neutral forge.CredentialProvider rather than a
// githubapp.TokenProvider. It builds via the unchanged New and wires
// Tokens to a credentialTokens adapter.
//
// A nil p is passed through to New as a nil githubapp.TokenProvider
// rather than wrapped: wrapping it would install a non-nil Tokens whose
// provider field is nil, bypassing New's "client missing TokenProvider"
// nil check and panicking on first use instead.
func NewWithCredentialProvider(p forge.CredentialProvider) *Client {
	if p == nil {
		return New(nil)
	}
	return New(&credentialTokens{provider: p})
}

// installationIDForScope resolves scope to a GitHub installation id for
// the Scoped method variants. It rejects a zero scope and parses the ref
// via scope.GitHubInstallationID(), naming the offending ref in the
// error.
func installationIDForScope(scope forge.CredentialScope) (int64, error) {
	if scope.IsZero() {
		return 0, fmt.Errorf("githubclient: credential scope is empty")
	}
	id, err := scope.GitHubInstallationID()
	if err != nil {
		return 0, fmt.Errorf("githubclient: %w", err)
	}
	return id, nil
}

// compile-time assertion that credentialTokens implements
// githubapp.TokenProvider.
var _ githubapp.TokenProvider = (*credentialTokens)(nil)

// --- client.go variants ---

// GetFileScoped is the forge.CredentialScope variant of GetFile.
func (c *Client) GetFileScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, path, ref string) (*FileContent, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.GetFile(ctx, id, repo, path, ref)
}

// GetWorkflowSpecScoped is the forge.CredentialScope variant of GetWorkflowSpec.
func (c *Client) GetWorkflowSpecScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, ref string) (*FileContent, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.GetWorkflowSpec(ctx, id, repo, ref)
}

// ListDirectoryScoped is the forge.CredentialScope variant of ListDirectory.
func (c *Client) ListDirectoryScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, path, ref string) ([]DirEntry, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.ListDirectory(ctx, id, repo, path, ref)
}

// GetIssueScoped is the forge.CredentialScope variant of GetIssue.
func (c *Client) GetIssueScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, number int) (*Issue, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.GetIssue(ctx, id, repo, number)
}

// GetBranchProtectionScoped is the forge.CredentialScope variant of GetBranchProtection.
func (c *Client) GetBranchProtectionScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, branch string) (*BranchProtection, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.GetBranchProtection(ctx, id, repo, branch)
}

// ListRulesetRequiredChecksScoped is the forge.CredentialScope variant of ListRulesetRequiredChecks.
func (c *Client) ListRulesetRequiredChecksScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, branch string) ([]RulesetRequiredCheck, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.ListRulesetRequiredChecks(ctx, id, repo, branch)
}

// EnableAutoMergeScoped is the forge.CredentialScope variant of EnableAutoMerge.
func (c *Client) EnableAutoMergeScoped(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, prNumber int, method MergeMethod) error {
	id, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	return c.EnableAutoMerge(ctx, id, repo, prNumber, method)
}

// MergePullRequestScoped is the forge.CredentialScope variant of MergePullRequest.
func (c *Client) MergePullRequestScoped(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, prNumber int, method MergeMethod) error {
	id, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	return c.MergePullRequest(ctx, id, repo, prNumber, method)
}

// GetPullRequestScoped is the forge.CredentialScope variant of GetPullRequest.
func (c *Client) GetPullRequestScoped(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, number int) (*PullRequest, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.GetPullRequest(ctx, id, repo, number)
}

// EditPullRequestScoped is the forge.CredentialScope variant of EditPullRequest.
func (c *Client) EditPullRequestScoped(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, number int, body string) error {
	id, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	return c.EditPullRequest(ctx, id, repo, number, body)
}

// GetReleaseByTagScoped is the forge.CredentialScope variant of GetReleaseByTag.
func (c *Client) GetReleaseByTagScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, tag string) (*Release, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.GetReleaseByTag(ctx, id, repo, tag)
}

// UpdateReleaseBodyScoped is the forge.CredentialScope variant of UpdateReleaseBody.
func (c *Client) UpdateReleaseBodyScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, releaseID int64, body string) error {
	id, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	return c.UpdateReleaseBody(ctx, id, repo, releaseID, body)
}

// DeleteReleaseAssetScoped is the forge.CredentialScope variant of DeleteReleaseAsset.
func (c *Client) DeleteReleaseAssetScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, assetID int64) error {
	id, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	return c.DeleteReleaseAsset(ctx, id, repo, assetID)
}

// UploadReleaseAssetScoped is the forge.CredentialScope variant of UploadReleaseAsset.
func (c *Client) UploadReleaseAssetScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, releaseID int64, name, contentType string, data []byte) error {
	id, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	return c.UploadReleaseAsset(ctx, id, repo, releaseID, name, contentType, data)
}

// CompareCommitsScoped is the forge.CredentialScope variant of CompareCommits.
func (c *Client) CompareCommitsScoped(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, base, head string) ([]string, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.CompareCommits(ctx, id, repo, base, head)
}

// ListPullRequestsForCommitScoped is the forge.CredentialScope variant of ListPullRequestsForCommit.
func (c *Client) ListPullRequestsForCommitScoped(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, sha string) ([]PullRequestRef, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.ListPullRequestsForCommit(ctx, id, repo, sha)
}

// ComparePatchScoped is the forge.CredentialScope variant of ComparePatch.
func (c *Client) ComparePatchScoped(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, base, head string) (*ComparePatchResult, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.ComparePatch(ctx, id, repo, base, head)
}

// ForceUpdateRefScoped is the forge.CredentialScope variant of ForceUpdateRef.
func (c *Client) ForceUpdateRefScoped(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, branch, newSHA string) error {
	id, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	return c.ForceUpdateRef(ctx, id, repo, branch, newSHA)
}

// GetBranchSHAScoped is the forge.CredentialScope variant of GetBranchSHA.
func (c *Client) GetBranchSHAScoped(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, branch string) (string, bool, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return "", false, err
	}
	return c.GetBranchSHA(ctx, id, repo, branch)
}

// CreateRefScoped is the forge.CredentialScope variant of CreateRef.
func (c *Client) CreateRefScoped(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, branch, sha string) error {
	id, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	return c.CreateRef(ctx, id, repo, branch, sha)
}

// MergeBranchScoped is the forge.CredentialScope variant of MergeBranch.
func (c *Client) MergeBranchScoped(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, base, head, commitMessage string) (string, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return "", err
	}
	return c.MergeBranch(ctx, id, repo, base, head, commitMessage)
}

// CreatePullRequestScoped is the forge.CredentialScope variant of CreatePullRequest.
func (c *Client) CreatePullRequestScoped(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, head, base, title, body string) (*PullRequest, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.CreatePullRequest(ctx, id, repo, head, base, title, body)
}

// ClosePullRequestScoped is the forge.CredentialScope variant of ClosePullRequest.
func (c *Client) ClosePullRequestScoped(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, number int) error {
	id, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	return c.ClosePullRequest(ctx, id, repo, number)
}

// ListOpenPullRequestsByHeadScoped is the forge.CredentialScope variant of ListOpenPullRequestsByHead.
func (c *Client) ListOpenPullRequestsByHeadScoped(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, headBranch, base string) ([]PullRequest, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.ListOpenPullRequestsByHead(ctx, id, repo, headBranch, base)
}

// ListTeamMembersScoped is the forge.CredentialScope variant of ListTeamMembers.
func (c *Client) ListTeamMembersScoped(ctx context.Context, scope forge.CredentialScope, org, slug string) ([]TeamMember, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.ListTeamMembers(ctx, id, org, slug)
}

// ListIssueCommentsScoped is the forge.CredentialScope variant of ListIssueComments.
func (c *Client) ListIssueCommentsScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, number int) ([]FetchedIssueComment, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.ListIssueComments(ctx, id, repo, number)
}

// DispatchWorkflowScoped is the forge.CredentialScope variant of DispatchWorkflow.
func (c *Client) DispatchWorkflowScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, workflowFile, ref string, inputs DispatchInputs) error {
	id, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	return c.DispatchWorkflow(ctx, id, repo, workflowFile, ref, inputs)
}

// GetWorkflowRunScoped is the forge.CredentialScope variant of GetWorkflowRun.
func (c *Client) GetWorkflowRunScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, runID int64) (*WorkflowRun, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.GetWorkflowRun(ctx, id, repo, runID)
}

// ResolveDispatchedRunScoped is the forge.CredentialScope variant of ResolveDispatchedRun.
func (c *Client) ResolveDispatchedRunScoped(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, branch string, correlation map[string]string, createdAfter time.Time) (*WorkflowRun, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.ResolveDispatchedRun(ctx, id, repo, branch, correlation, createdAfter)
}

// CreateIssueCommentScoped is the forge.CredentialScope variant of CreateIssueComment.
func (c *Client) CreateIssueCommentScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, issueNumber int, body string) (*IssueComment, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.CreateIssueComment(ctx, id, repo, issueNumber, body)
}

// UpdateIssueCommentScoped is the forge.CredentialScope variant of UpdateIssueComment.
func (c *Client) UpdateIssueCommentScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, commentID int64, body string) (*IssueComment, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.UpdateIssueComment(ctx, id, repo, commentID, body)
}

// CreateCheckRunScoped is the forge.CredentialScope variant of CreateCheckRun.
func (c *Client) CreateCheckRunScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, p CreateCheckRunParams) (*CreateCheckRunResult, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.CreateCheckRun(ctx, id, repo, p)
}

// CreateReviewScoped is the forge.CredentialScope variant of CreateReview.
func (c *Client) CreateReviewScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, prNumber int, params CreateReviewParams) (*CreateReviewResult, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.CreateReview(ctx, id, repo, prNumber, params)
}

// ListIssueCommentReactionsScoped is the forge.CredentialScope variant of ListIssueCommentReactions.
func (c *Client) ListIssueCommentReactionsScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, commentID int64) ([]IssueCommentReaction, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.ListIssueCommentReactions(ctx, id, repo, commentID)
}

// --- gitdata.go variants ---

// GetRepositoryScoped is the forge.CredentialScope variant of GetRepository.
func (c *Client) GetRepositoryScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef) (*Repository, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.GetRepository(ctx, id, repo)
}

// GetCommitScoped is the forge.CredentialScope variant of GetCommit.
func (c *Client) GetCommitScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, sha string) (*GitCommit, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.GetCommit(ctx, id, repo, sha)
}

// CreateTreeScoped is the forge.CredentialScope variant of CreateTree.
func (c *Client) CreateTreeScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef,
	baseTree string, entries []TreeEntry) (string, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return "", err
	}
	return c.CreateTree(ctx, id, repo, baseTree, entries)
}

// CreateCommitScoped is the forge.CredentialScope variant of CreateCommit.
func (c *Client) CreateCommitScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef,
	message, treeSHA string, parents []string) (string, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return "", err
	}
	return c.CreateCommit(ctx, id, repo, message, treeSHA, parents)
}

// --- projects.go variants ---

// CreateIssueScoped is the forge.CredentialScope variant of CreateIssue.
func (c *Client) CreateIssueScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, p CreateIssueParams) (*CreatedIssue, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.CreateIssue(ctx, id, repo, p)
}

// SearchOpenIssuesScoped is the forge.CredentialScope variant of SearchOpenIssues.
func (c *Client) SearchOpenIssuesScoped(ctx context.Context, scope forge.CredentialScope, query string) ([]IssueSearchResult, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.SearchOpenIssues(ctx, id, query)
}

// SearchIssuesByTitleScoped is the forge.CredentialScope variant of SearchIssuesByTitle.
func (c *Client) SearchIssuesByTitleScoped(ctx context.Context, scope forge.CredentialScope, query string) ([]IssueTitleResult, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.SearchIssuesByTitle(ctx, id, query)
}

// IssueNodeIDScoped is the forge.CredentialScope variant of IssueNodeID.
func (c *Client) IssueNodeIDScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, number int) (string, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return "", err
	}
	return c.IssueNodeID(ctx, id, repo, number)
}

// ProjectFieldsScoped is the forge.CredentialScope variant of ProjectFields.
func (c *Client) ProjectFieldsScoped(ctx context.Context, scope forge.CredentialScope, coord ProjectCoord, fieldName string) (*ProjectMeta, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.ProjectFields(ctx, id, coord, fieldName)
}

// ProjectItemStatusScoped is the forge.CredentialScope variant of ProjectItemStatus.
func (c *Client) ProjectItemStatusScoped(ctx context.Context, scope forge.CredentialScope, issueNodeID, projectID, fieldName string) (*ProjectItemStatus, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.ProjectItemStatus(ctx, id, issueNodeID, projectID, fieldName)
}

// AddProjectItemScoped is the forge.CredentialScope variant of AddProjectItem.
func (c *Client) AddProjectItemScoped(ctx context.Context, scope forge.CredentialScope, projectID, contentID string) (string, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return "", err
	}
	return c.AddProjectItem(ctx, id, projectID, contentID)
}

// SetProjectItemSingleSelectScoped is the forge.CredentialScope variant of SetProjectItemSingleSelect.
func (c *Client) SetProjectItemSingleSelectScoped(ctx context.Context, scope forge.CredentialScope, projectID, itemID, fieldID, optionID string) error {
	id, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	return c.SetProjectItemSingleSelect(ctx, id, projectID, itemID, fieldID, optionID)
}

// AddSubIssueScoped is the forge.CredentialScope variant of AddSubIssue.
func (c *Client) AddSubIssueScoped(ctx context.Context, scope forge.CredentialScope, parentNodeID, childNodeID string) error {
	id, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	return c.AddSubIssue(ctx, id, parentNodeID, childNodeID)
}

// ListSubIssuesScoped is the forge.CredentialScope variant of ListSubIssues.
func (c *Client) ListSubIssuesScoped(ctx context.Context, scope forge.CredentialScope, parentNodeID string) ([]SubIssue, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.ListSubIssues(ctx, id, parentNodeID)
}

// --- codescanning.go variants ---

// ListCodeScanningAlertsScoped is the forge.CredentialScope variant of ListCodeScanningAlerts.
func (c *Client) ListCodeScanningAlertsScoped(ctx context.Context, scope forge.CredentialScope, repo RepoRef, ref string) ([]securityscan.Finding, error) {
	id, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	return c.ListCodeScanningAlerts(ctx, id, repo, ref)
}
