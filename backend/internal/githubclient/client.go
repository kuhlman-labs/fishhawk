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
	// ErrNotInstalled means the GitHub App is not installed on the
	// target repo (GET /repos/{owner}/{repo}/installation returned
	// 404). Distinct from ErrNotFound so callers can surface a
	// precise user-facing error instead of a generic "not found".
	ErrNotInstalled = errors.New("githubclient: app not installed on repo")
	// ErrPullRequestExists means CreatePullRequest hit a 422 whose
	// body indicates a PR already exists for the requested head/base
	// pair. The orchestrator's consolidated-PR path treats this as a
	// benign lost race (ADR-032 / #714) and recovers the existing PR
	// URL via ListOpenPullRequestsByHead rather than failing the
	// settle. Distinct from ErrValidation so the caller can switch on
	// it without re-parsing the 422 body.
	ErrPullRequestExists = errors.New("githubclient: pull request already exists for head/base")
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

	// AppJWT, when non-nil, returns a fresh App-level JWT for
	// endpoints that authenticate as the GitHub App itself rather
	// than as an installation (e.g. GetRepoInstallation). In
	// production this wraps githubapp.Signer.Sign(0); tests inject
	// a stub that returns a fixed string.
	AppJWT func() (string, error)
}

// New returns a Client with sensible defaults. tokens is required;
// without it every call returns an error before touching the wire.
//
// New leaves AppJWT nil, so App-level endpoints (GetRepoInstallation)
// fail the nil guard in buildAppJWTRequest. Production must construct
// via NewWithSigner so those endpoints authenticate; New is for
// installation-only callers and tests that wire AppJWT by hand.
func New(tokens githubapp.TokenProvider) *Client {
	return &Client{
		Tokens: tokens,
		HTTP:   &http.Client{Timeout: 30 * time.Second},
	}
}

// AppJWTSigner mints an App-level JWT. Satisfied by *githubapp.Signer
// (its Sign(ttl time.Duration) (string, error) method). Declared here
// so NewWithSigner depends on the minting capability, not the concrete
// signer type.
type AppJWTSigner interface {
	Sign(ttl time.Duration) (string, error)
}

// NewWithSigner is the production constructor: it builds a Client via
// New(tokens) and wires AppJWT from the App signer, so App-level
// endpoints (GetRepoInstallation) authenticate with a fresh App JWT
// instead of hitting the nil guard. This is the single wiring path
// production must use; serve.go constructs cfg.GitHub through it.
//
// signer.Sign(0) clamps to githubapp.DefaultJWTTTL (9m), safely under
// GitHub's 10-minute App-JWT cap.
func NewWithSigner(tokens githubapp.TokenProvider, signer AppJWTSigner) *Client {
	c := New(tokens)
	c.AppJWT = func() (string, error) { return signer.Sign(0) }
	return c
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

// BranchProtection is the slice of a branch-protection API response
// Fishhawk surfaces for the required-checks snapshot (#251 /
// ADR-017). Only RequiredStatusCheckContexts is consumed today;
// other fields land alongside as features need them.
type BranchProtection struct {
	// RequiredStatusCheckContexts is the closed list of context
	// names a PR must report green before GitHub allows merge.
	// Empty (nil or zero-length) means classic protection has no
	// required-status-checks rule for the branch — rulesets may
	// still contribute. The dispatcher derives the union per
	// ADR-017.
	RequiredStatusCheckContexts []string
}

// GetBranchProtection fetches classic branch protection for a
// branch.
//
//	GET /repos/{owner}/{repo}/branches/{branch}/protection
//
// Returns ErrNotFound when the branch has no protection configured
// (GitHub returns 404 for that case, not an empty document). The
// dispatcher treats ErrNotFound on this call as "no classic
// protection" and falls through to the rulesets check rather than
// surfacing the error — protection-via-ruleset-only is a normal
// shape on GitHub repos that have migrated.
//
// Requires the App to hold `administration: read` (#252 / ADR-017).
func (c *Client) GetBranchProtection(ctx context.Context, installationID int64, repo RepoRef, branch string) (*BranchProtection, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if branch == "" {
		return nil, errors.New("githubclient: branch is required")
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/branches/" + url.PathEscape(branch) +
		"/protection")

	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: get branch protection: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("get branch protection", resp); err != nil {
		return nil, err
	}

	// GitHub's response shape: required_status_checks is an object
	// with `contexts` (string[]) and `checks` (object[]). v0 reads
	// only `contexts` — `checks` is the newer per-check-with-app-id
	// shape that's a superset of `contexts` for our purposes (every
	// check contributes its `context` to the contexts list).
	var body struct {
		RequiredStatusChecks *struct {
			Contexts []string `json:"contexts"`
		} `json:"required_status_checks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode branch protection: %w", err)
	}
	out := &BranchProtection{}
	if body.RequiredStatusChecks != nil {
		out.RequiredStatusCheckContexts = body.RequiredStatusChecks.Contexts
	}
	return out, nil
}

// RulesetRequiredCheck is one required-status-check context
// contributed by a ruleset that targets the branch (#251).
type RulesetRequiredCheck struct {
	// RulesetID is the GitHub-side ruleset identifier — surfaced so
	// the snapshot's `sources` field can record exactly which
	// ruleset contributed.
	RulesetID int64
	// Contexts is the deduped list of context names the ruleset
	// requires. May be empty when the ruleset doesn't include a
	// `required_status_checks` rule.
	Contexts []string
}

// ListRulesetRequiredChecks walks the repo-level rulesets that
// target the given branch and returns their required-status-check
// contexts. Two-step: list rulesets, then fetch each by ID for the
// rule body — the list endpoint omits `parameters`.
//
//	GET /repos/{owner}/{repo}/rulesets
//	GET /repos/{owner}/{repo}/rulesets/{id}
//
// Filters to rulesets whose target is "branch" (org-level rulesets
// that target a different repo flow through the org endpoint, out of
// scope for v0) and whose enforcement is "active". Disabled and
// "evaluate-only" rulesets are skipped — they wouldn't block a
// merge in production and shouldn't shape Fishhawk's snapshot.
//
// Returns nil + nil when the repo has no matching rulesets.
//
// Requires the App to hold `administration: read` (#252 / ADR-017).
func (c *Client) ListRulesetRequiredChecks(ctx context.Context, installationID int64, repo RepoRef, branch string) ([]RulesetRequiredCheck, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if branch == "" {
		return nil, errors.New("githubclient: branch is required")
	}

	listEndpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/rulesets?includes_parents=true")

	listReq, err := c.buildRequest(ctx, http.MethodGet, listEndpoint, nil, installationID)
	if err != nil {
		return nil, err
	}
	listResp, err := c.HTTP.Do(listReq)
	if err != nil {
		return nil, fmt.Errorf("githubclient: list rulesets: %w", err)
	}
	defer func() { _ = listResp.Body.Close() }()

	if err := classifyStatus("list rulesets", listResp); err != nil {
		return nil, err
	}

	var summaries []struct {
		ID          int64  `json:"id"`
		Target      string `json:"target"`
		Enforcement string `json:"enforcement"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&summaries); err != nil {
		return nil, fmt.Errorf("githubclient: decode rulesets list: %w", err)
	}

	var out []RulesetRequiredCheck
	for _, s := range summaries {
		if s.Target != "branch" || s.Enforcement != "active" {
			continue
		}
		contexts, err := c.fetchRulesetContexts(ctx, installationID, repo, s.ID, branch)
		if err != nil {
			return nil, err
		}
		if len(contexts) == 0 {
			continue
		}
		out = append(out, RulesetRequiredCheck{RulesetID: s.ID, Contexts: contexts})
	}
	return out, nil
}

// fetchRulesetContexts pulls a single ruleset and returns the
// `required_status_checks` rule's contexts when it applies to
// `branch`. Returns an empty slice when the ruleset doesn't have a
// matching rule or excludes the branch.
//
// v0 doesn't try to evaluate the full conditions DSL — it only
// honors the common case (`include` containing `~ALL`, `~DEFAULT_BRANCH`,
// or the literal branch name; `exclude` ignored). Rulesets with
// complex match expressions land empty-handed; the operator's
// fallback is to add a classic-protection row, which v0 does read.
func (c *Client) fetchRulesetContexts(ctx context.Context, installationID int64, repo RepoRef, rulesetID int64, branch string) ([]string, error) {
	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/rulesets/" + url.PathEscape(fmt.Sprintf("%d", rulesetID)))
	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: get ruleset: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("get ruleset", resp); err != nil {
		return nil, err
	}

	var body struct {
		Conditions *struct {
			RefName *struct {
				Include []string `json:"include"`
				Exclude []string `json:"exclude"`
			} `json:"ref_name"`
		} `json:"conditions"`
		Rules []struct {
			Type       string `json:"type"`
			Parameters *struct {
				RequiredStatusChecks []struct {
					Context string `json:"context"`
				} `json:"required_status_checks"`
			} `json:"parameters"`
		} `json:"rules"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode ruleset: %w", err)
	}

	if !rulesetMatchesBranch(body.Conditions, branch) {
		return nil, nil
	}

	var contexts []string
	for _, r := range body.Rules {
		if r.Type != "required_status_checks" || r.Parameters == nil {
			continue
		}
		for _, c := range r.Parameters.RequiredStatusChecks {
			if c.Context == "" {
				continue
			}
			contexts = append(contexts, c.Context)
		}
	}
	return contexts, nil
}

// rulesetMatchesBranch is the v0 condition matcher: honors `~ALL`,
// `~DEFAULT_BRANCH` (only when branch is "main"; we don't have a
// way to know the configured default here, and this is the v0
// approximation), and exact branch-name matches against the
// `refs/heads/<branch>` form GitHub returns. Rulesets with a
// nil Conditions block are treated as "matches everything" —
// GitHub's UI maps "no condition" to that.
func rulesetMatchesBranch(conditions *struct {
	RefName *struct {
		Include []string `json:"include"`
		Exclude []string `json:"exclude"`
	} `json:"ref_name"`
}, branch string) bool {
	if conditions == nil || conditions.RefName == nil {
		return true
	}
	full := "refs/heads/" + branch
	for _, ex := range conditions.RefName.Exclude {
		if ex == full || ex == branch || ex == "~ALL" {
			return false
		}
	}
	if len(conditions.RefName.Include) == 0 {
		return true
	}
	for _, in := range conditions.RefName.Include {
		switch in {
		case "~ALL":
			return true
		case "~DEFAULT_BRANCH":
			if branch == "main" {
				return true
			}
		case full, branch:
			return true
		}
	}
	return false
}

// MergeMethod is the strategy GitHub uses when an auto-merge fires.
// Mirrors the GraphQL `PullRequestMergeMethod` enum on the wire so
// callers can pass the canonical names through.
type MergeMethod string

// Merge methods accepted by enablePullRequestAutoMerge.
const (
	MergeMethodSquash MergeMethod = "SQUASH"
	MergeMethodMerge  MergeMethod = "MERGE"
	MergeMethodRebase MergeMethod = "REBASE"
)

// EnableAutoMerge queues a PR for auto-merge once branch protection
// clears (#255 / ADR-017). Uses the GitHub GraphQL API because the
// REST surface does not expose auto-merge directly — only synchronous
// merge.
//
// Two-call sequence:
//  1. REST GET /repos/{owner}/{repo}/pulls/{number} to resolve the
//     PR's GraphQL node id.
//  2. GraphQL mutation `enablePullRequestAutoMerge` with the node id
//     and the requested merge method.
//
// Returns nil on success. ErrNotFound when the PR doesn't exist on
// the installation; ErrForbidden for auth issues; ErrValidation when
// GitHub rejects the auto-merge enable (e.g., branch protection
// already met and the PR auto-merged synchronously, repo doesn't
// allow auto-merge, or the merge method is disabled).
//
// Idempotent in practice: enabling auto-merge on a PR that already
// has it queued returns success rather than failing. The dispatcher
// can call this multiple times across retries without special-
// casing.
func (c *Client) EnableAutoMerge(ctx context.Context, installationID int64,
	repo RepoRef, prNumber int, method MergeMethod) error {
	if c.Tokens == nil {
		return errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return errors.New("githubclient: repo owner and name required")
	}
	if prNumber <= 0 {
		return errors.New("githubclient: pr number must be > 0")
	}
	if method == "" {
		method = MergeMethodSquash
	}

	pr, err := c.GetPullRequest(ctx, installationID, repo, prNumber)
	if err != nil {
		return err
	}
	nodeID := pr.NodeID

	mutation := `mutation EnableAutoMerge($id: ID!, $method: PullRequestMergeMethod!) {
  enablePullRequestAutoMerge(input: { pullRequestId: $id, mergeMethod: $method }) {
    pullRequest { number url state }
  }
}`
	body := map[string]any{
		"query": mutation,
		"variables": map[string]any{
			"id":     nodeID,
			"method": string(method),
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("githubclient: marshal auto-merge mutation: %w", err)
	}
	req, err := c.buildRequest(ctx, http.MethodPost, c.endpoint("/graphql"), bytes.NewReader(raw), installationID)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("githubclient: enable auto-merge: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("enable auto-merge", resp); err != nil {
		return err
	}
	// GraphQL returns 200 even for application-level errors. Inspect
	// the `errors` field and surface as ErrValidation so the
	// orchestrator can audit the rejection without retrying.
	var gqlResp struct {
		Errors []struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&gqlResp); err != nil {
		return fmt.Errorf("githubclient: decode auto-merge response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return fmt.Errorf("%w: enable auto-merge: %s", ErrValidation, gqlResp.Errors[0].Message)
	}
	return nil
}

// PullRequest is the slice of a pull-request API response Fishhawk
// surfaces. NodeID is the opaque base64-encoded GraphQL identifier
// (consumed by `EnableAutoMerge` per #255). HeadSHA + State are
// consumed by the foreign-commit audit rule (#282) — Fishhawk
// compares the live HEAD on GitHub against the head_shas it
// recorded in its own pull_request artifacts.
type PullRequest struct {
	NodeID  string
	HeadSHA string
	State   string // "open" | "closed"
	Merged  bool   // true when state=closed and the PR was merged
	// BaseRef is the PR's target branch name (the `base.ref` field).
	// It is the independently-trustworthy compare anchor for the run
	// branch lineage guard (ADR-035, #858): GitHub knows what the PR
	// targets, so a contaminated branch commit cannot launder it the
	// way a runner-reported base_sha can.
	BaseRef string
	// Number and HTMLURL are populated by CreatePullRequest and
	// ListOpenPullRequestsByHead (the consolidated-PR path, #714).
	// GetPullRequest leaves them zero/empty — its callers only need
	// NodeID/HeadSHA/State/Merged.
	Number  int
	HTMLURL string
}

// GetPullRequest fetches a single PR by number.
//
//	GET /repos/{owner}/{repo}/pulls/{number}
//
// Returns ErrNotFound when the PR doesn't exist on the installation,
// ErrForbidden on auth issues. Used by:
//
//   - `EnableAutoMerge` (#255) — for the node id (the GraphQL
//     mutation can't be addressed by number).
//   - `auditcomplete.Compute` rule 5 (#282) — for the live head_sha
//     comparison.
func (c *Client) GetPullRequest(ctx context.Context, installationID int64,
	repo RepoRef, number int) (*PullRequest, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if number <= 0 {
		return nil, errors.New("githubclient: pr number must be > 0")
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/pulls/" + url.PathEscape(fmt.Sprintf("%d", number)))
	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: get pr: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := classifyStatus("get pr", resp); err != nil {
		return nil, err
	}
	var body struct {
		NodeID string `json:"node_id"`
		State  string `json:"state"`
		Merged bool   `json:"merged"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode pr: %w", err)
	}
	if body.NodeID == "" {
		return nil, fmt.Errorf("githubclient: pr response missing node_id")
	}
	return &PullRequest{
		NodeID:  body.NodeID,
		HeadSHA: body.Head.SHA,
		State:   body.State,
		Merged:  body.Merged,
		BaseRef: body.Base.Ref,
	}, nil
}

// CompareCommits returns the SHAs of the commits on head since its
// merge-base with base, i.e. the commits the branch added relative to
// base — (merge-base, head].
//
//	GET /repos/{owner}/{repo}/compare/{base}...{head}
//
// The three-dot form anchors the comparison on the merge-base, so
// commits merged into base while the run was open are excluded (no
// false positive if the target branch advances mid-run). It is the
// branch-lineage guard's enumeration of every commit on the run
// branch (ADR-035, #858); each returned SHA is checked for membership
// in the run's own reported-head ledger.
//
// No pagination: the compare API returns up to 250 commits, far above
// any realistic run branch's commit count. A branch exceeding 250
// would under-return, and the guard fails open on that (no false
// positive). Returns a typed error (ErrNotFound / ErrValidation /
// ErrForbidden) on non-2xx so callers can fail open on a transient
// GitHub failure.
func (c *Client) CompareCommits(ctx context.Context, installationID int64,
	repo RepoRef, base, head string) ([]string, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if base == "" || head == "" {
		return nil, errors.New("githubclient: compare base and head required")
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/compare/" + escapePath(base) + "..." + escapePath(head))
	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: compare commits: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := classifyStatus("compare commits", resp); err != nil {
		return nil, err
	}
	var body struct {
		Commits []struct {
			SHA string `json:"sha"`
		} `json:"commits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode compare: %w", err)
	}
	shas := make([]string, 0, len(body.Commits))
	for _, cm := range body.Commits {
		shas = append(shas, cm.SHA)
	}
	return shas, nil
}

// CreatePullRequest opens a pull request from head into base (#714 /
// ADR-032). It is the single GitHub write surface for the consolidated
// decomposition PR: after all decomposed children push their commits
// onto the shared branch, the orchestrator opens ONE PR for the parent
// run via this method.
//
//	POST /repos/{owner}/{repo}/pulls
//
// head is a branch name in the same repo (no "owner:" prefix — same-repo
// PRs only for v0). base is the target branch (typically the repo's
// default ref). Returns the created PR's number + html_url decoded from
// the 201.
//
// GitHub returns 422 when a PR already exists for the same head/base
// pair. This method body-sniffs that 422 for the duplicate marker and
// returns ErrPullRequestExists BEFORE the body is consumed by the shared
// classifyStatus path (which would map every 422 to ErrValidation and
// exhaust the 256-byte brief body). Callers recover the existing PR via
// ListOpenPullRequestsByHead. ErrNotFound when the repo isn't visible to
// the installation, ErrForbidden when the App lacks `pull_requests:write`.
func (c *Client) CreatePullRequest(ctx context.Context, installationID int64,
	repo RepoRef, head, base, title, body string) (*PullRequest, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if head == "" || base == "" {
		return nil, errors.New("githubclient: head and base branches required")
	}
	if title == "" {
		return nil, errors.New("githubclient: pull request title required")
	}

	raw, err := json.Marshal(map[string]string{
		"title": title,
		"head":  head,
		"base":  base,
		"body":  body,
	})
	if err != nil {
		return nil, fmt.Errorf("githubclient: marshal create pr: %w", err)
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) + "/pulls")

	req, err := c.buildRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(raw), installationID)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: create pr: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Body-sniff the 422-duplicate case BEFORE classifyStatus consumes
	// the body. GitHub signals "PR already exists for this head/base"
	// with a 422 whose errors[].message reads "A pull request already
	// exists for ..." and/or a code of "custom"/"already_exists"; we
	// match on the human marker to avoid a guaranteed-shape assumption.
	if resp.StatusCode == http.StatusUnprocessableEntity {
		brief := readBriefBody(resp.Body)
		low := strings.ToLower(brief)
		if strings.Contains(low, "already exists") || strings.Contains(low, "already_exists") {
			return nil, fmt.Errorf("%w: %s", ErrPullRequestExists, brief)
		}
		return nil, fmt.Errorf("%w: create pr: %s", ErrValidation, brief)
	}
	if err := classifyStatus("create pr", resp); err != nil {
		return nil, err
	}

	var out struct {
		NodeID  string `json:"node_id"`
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
		Head    struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("githubclient: decode create pr: %w", err)
	}
	return &PullRequest{
		NodeID:  out.NodeID,
		HeadSHA: out.Head.SHA,
		State:   out.State,
		Number:  out.Number,
		HTMLURL: out.HTMLURL,
	}, nil
}

// ListOpenPullRequestsByHead returns the open PRs whose head matches the
// given branch and whose base matches the given base ref (#714). Used by
// the orchestrator's consolidated-PR path to recover the existing PR's
// URL when CreatePullRequest lost a double-open race and returned
// ErrPullRequestExists — the create-PR 422 body does not carry the
// existing PR's number/url in a guaranteed shape, so we look it up.
//
//	GET /repos/{owner}/{repo}/pulls?head={owner}:{branch}&base={base}&state=open
//
// head is a bare branch name; this method builds the "owner:branch" head
// filter GitHub's list endpoint expects. Returns ErrNotFound when the
// repo isn't visible to the installation, ErrForbidden on auth issues.
func (c *Client) ListOpenPullRequestsByHead(ctx context.Context, installationID int64,
	repo RepoRef, headBranch, base string) ([]PullRequest, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if headBranch == "" {
		return nil, errors.New("githubclient: head branch required")
	}

	q := url.Values{}
	q.Set("head", repo.Owner+":"+headBranch)
	if base != "" {
		q.Set("base", base)
	}
	q.Set("state", "open")
	endpoint := c.endpoint("/repos/"+url.PathEscape(repo.Owner)+
		"/"+url.PathEscape(repo.Name)+"/pulls") + "?" + q.Encode()

	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: list pulls by head: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("list pulls by head", resp); err != nil {
		return nil, err
	}

	var body []struct {
		NodeID  string `json:"node_id"`
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
		Head    struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode pulls by head: %w", err)
	}
	out := make([]PullRequest, 0, len(body))
	for _, p := range body {
		out = append(out, PullRequest{
			NodeID:  p.NodeID,
			HeadSHA: p.Head.SHA,
			State:   p.State,
			Number:  p.Number,
			HTMLURL: p.HTMLURL,
		})
	}
	return out, nil
}

// TeamMember is the slice of a team-membership API response we
// surface for role resolution. Login is the canonical handle used
// in audits and approvers comparisons; ID is preserved so future
// callers that need a stable-across-renames key have it.
type TeamMember struct {
	Login string
	ID    int64
}

// ListTeamMembers fetches the active members of a GitHub team.
//
//	GET /orgs/{org}/teams/{team_slug}/members?role=all
//
// Used by E4.4 role resolution to expand `@org/team` references
// in the workflow spec into a username allowlist for approvers.
//
// Pages until exhaustion via Link headers. Returns the union of
// "maintainers" and "members" (role=all is the documented
// equivalent on the team-members endpoint).
func (c *Client) ListTeamMembers(ctx context.Context, installationID int64, org, slug string) ([]TeamMember, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if org == "" || slug == "" {
		return nil, errors.New("githubclient: org and team slug required")
	}

	pagePath := "/orgs/" + url.PathEscape(org) + "/teams/" + url.PathEscape(slug) + "/members?role=all&per_page=100"
	endpoint := c.endpoint(pagePath)

	var out []TeamMember
	for endpoint != "" {
		req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
		if err != nil {
			return nil, err
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("githubclient: list team members: %w", err)
		}
		members, next, err := decodeTeamMembersPage(resp)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, members...)
		endpoint = next
	}
	return out, nil
}

// decodeTeamMembersPage handles one page of team members and
// returns the next-page URL if Link advertises one. Split out so
// the pagination loop above stays readable.
func decodeTeamMembersPage(resp *http.Response) ([]TeamMember, string, error) {
	if err := classifyStatus("list team members", resp); err != nil {
		return nil, "", err
	}
	var body []struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("githubclient: decode team members: %w", err)
	}
	out := make([]TeamMember, 0, len(body))
	for _, m := range body {
		out = append(out, TeamMember{Login: m.Login, ID: m.ID})
	}
	return out, nextPageURL(resp.Header.Get("Link")), nil
}

// nextPageURL parses GitHub's Link header for the rel="next" URL.
// Returns "" when no further pages remain.
func nextPageURL(link string) string {
	for _, part := range strings.Split(link, ",") {
		segs := strings.Split(strings.TrimSpace(part), ";")
		if len(segs) < 2 {
			continue
		}
		urlPart := strings.TrimSpace(segs[0])
		if !strings.HasPrefix(urlPart, "<") || !strings.HasSuffix(urlPart, ">") {
			continue
		}
		isNext := false
		for _, attr := range segs[1:] {
			if strings.TrimSpace(attr) == `rel="next"` {
				isNext = true
				break
			}
		}
		if isNext {
			return urlPart[1 : len(urlPart)-1]
		}
	}
	return ""
}

// FetchedIssueComment is the subset of a GitHub issue-comment Fishhawk
// reads when fetching an issue's comment thread (#621). It is distinct
// from IssueComment, which models the create/update *response* shape
// (id/body/html_url) those helpers decode. The fetch side surfaces the
// author login, body, and creation timestamp the plan-stage prompt
// renders.
type FetchedIssueComment struct {
	Author    string
	Body      string
	CreatedAt string
}

// ListIssueComments fetches the comment thread on an issue (or PR —
// GitHub treats PR conversations as issue threads).
//
//	GET /repos/{owner}/{repo}/issues/{number}/comments?per_page=100
//
// Used by the webhook/installation-token prompt path (branch 2 of
// fillIssueContext) to populate the plan-stage prompt with
// comment-borne refinements, matching the operator gh-fetch path
// (#618). Pages until exhaustion via the rel="next" Link header —
// the same mechanism ListTeamMembers relies on.
//
// Returns ErrNotFound when the issue or repo isn't visible to the
// installation, ErrForbidden on auth issues.
func (c *Client) ListIssueComments(ctx context.Context, installationID int64, repo RepoRef, number int) ([]FetchedIssueComment, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if number <= 0 {
		return nil, errors.New("githubclient: issue number must be > 0")
	}

	pagePath := "/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/issues/" + url.PathEscape(fmt.Sprintf("%d", number)) +
		"/comments?per_page=100"
	endpoint := c.endpoint(pagePath)

	var out []FetchedIssueComment
	for endpoint != "" {
		req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
		if err != nil {
			return nil, err
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("githubclient: list issue comments: %w", err)
		}
		comments, next, err := decodeIssueCommentsPage(resp)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, comments...)
		endpoint = next
	}
	return out, nil
}

// decodeIssueCommentsPage handles one page of issue comments and
// returns the next-page URL if Link advertises one. Mirrors
// decodeTeamMembersPage so the pagination loop above stays readable.
func decodeIssueCommentsPage(resp *http.Response) ([]FetchedIssueComment, string, error) {
	if err := classifyStatus("list issue comments", resp); err != nil {
		return nil, "", err
	}
	var body []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("githubclient: decode issue comments: %w", err)
	}
	out := make([]FetchedIssueComment, 0, len(body))
	for _, c := range body {
		out = append(out, FetchedIssueComment{
			Author:    c.User.Login,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		})
	}
	return out, nextPageURL(resp.Header.Get("Link")), nil
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

// WorkflowRun is the slice of an Actions workflow_run we surface
// for matching back to a Fishhawk stage (#243). The real GitHub
// payload is much wider; we only need enough to recover the
// `workflow_dispatch` inputs (run_id / stage_id) the dispatcher
// fired with.
type WorkflowRun struct {
	ID         int64
	HTMLURL    string
	Conclusion string
	Status     string
	Event      string
	HeadBranch string
	HeadSHA    string
	// Inputs is the `workflow_dispatch` inputs map echoed back by
	// GitHub. Empty for non-dispatch runs.
	Inputs map[string]string
}

// GetWorkflowRun fetches a single Actions run by ID, surfacing the
// fields Fishhawk needs to match it back to a dispatched stage
// (#243).
//
//	GET /repos/{owner}/{repo}/actions/runs/{run_id}
//
// The returned `inputs` map is the workflow_dispatch input set the
// dispatcher fired with, which carries `run_id` and `stage_id` for
// the Fishhawk-side row.
//
// Returns ErrNotFound if the run id doesn't exist, ErrForbidden on
// auth issues.
func (c *Client) GetWorkflowRun(ctx context.Context, installationID int64, repo RepoRef, runID int64) (*WorkflowRun, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if runID <= 0 {
		return nil, errors.New("githubclient: workflow run id must be > 0")
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/actions/runs/" + url.PathEscape(fmt.Sprintf("%d", runID)))

	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: get workflow run: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("get workflow run", resp); err != nil {
		return nil, err
	}

	var body struct {
		ID         int64             `json:"id"`
		HTMLURL    string            `json:"html_url"`
		Conclusion string            `json:"conclusion"`
		Status     string            `json:"status"`
		Event      string            `json:"event"`
		HeadBranch string            `json:"head_branch"`
		HeadSHA    string            `json:"head_sha"`
		Inputs     map[string]string `json:"inputs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode workflow run: %w", err)
	}
	return &WorkflowRun{
		ID:         body.ID,
		HTMLURL:    body.HTMLURL,
		Conclusion: body.Conclusion,
		Status:     body.Status,
		Event:      body.Event,
		HeadBranch: body.HeadBranch,
		HeadSHA:    body.HeadSHA,
		Inputs:     body.Inputs,
	}, nil
}

// CreateIssueComment posts a markdown comment to the given issue
// (or PR — GitHub treats PR conversations as issue threads).
//
//	POST /repos/{owner}/{repo}/issues/{number}/comments
//
// Returns the created IssueComment (id, body, html_url) so callers
// can record the id for later edits — required by the sticky-status-
// comment flow (E20 / #326) and the plan-as-issue-comment flow
// (E17 / #323), both of which need to call UpdateIssueComment on a
// previously-posted row.
//
// Returns ErrNotFound when the issue or repo isn't visible to the
// installation, ErrForbidden when the App lacks `issues:write`.
// Caller is responsible for any rate-limit / dedup logic — this
// helper is the thin wrapper around the wire call.
func (c *Client) CreateIssueComment(ctx context.Context, installationID int64, repo RepoRef, issueNumber int, body string) (*IssueComment, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if issueNumber <= 0 {
		return nil, errors.New("githubclient: issue number must be > 0")
	}
	if body == "" {
		return nil, errors.New("githubclient: comment body must be non-empty")
	}

	raw, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return nil, fmt.Errorf("githubclient: marshal issue comment: %w", err)
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/issues/" + url.PathEscape(fmt.Sprintf("%d", issueNumber)) +
		"/comments")

	req, err := c.buildRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(raw), installationID)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: create issue comment: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("create issue comment", resp); err != nil {
		return nil, err
	}
	var out IssueComment
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("githubclient: decode create issue comment: %w", err)
	}
	return &out, nil
}

// IssueComment is the subset of GitHub's issue-comment shape Fishhawk
// reads back from PATCH responses. The wire shape carries more fields
// (user, reactions, timing); we surface only what callers verify.
type IssueComment struct {
	ID      int64  `json:"id"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
}

// UpdateIssueComment edits an existing issue comment in place. ADR-019
// / #320 (Fishhawk as coordination layer) leans on this for the
// sticky status comment (E20 / #326) and `update_on_change` plan
// comments (E17.2 / #337) — both flows need to mutate a previously-
// posted comment instead of spamming new ones.
//
//	PATCH /repos/{owner}/{repo}/issues/comments/{comment_id}
//
// Returns ErrNotFound when the comment was deleted by the operator or
// isn't visible to the installation, ErrForbidden when the App lacks
// `issues:write`. Same permission scope as CreateIssueComment, so no
// new manifest entry needed.
//
// Caller is responsible for finding the comment id (typically via
// the run's audit log — the existing `issue_commented` rows record
// the id for sticky-comment lookups).
func (c *Client) UpdateIssueComment(ctx context.Context, installationID int64, repo RepoRef, commentID int64, body string) (*IssueComment, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if commentID <= 0 {
		return nil, errors.New("githubclient: comment id must be > 0")
	}
	if body == "" {
		return nil, errors.New("githubclient: comment body must be non-empty")
	}

	raw, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return nil, fmt.Errorf("githubclient: marshal issue comment: %w", err)
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/issues/comments/" + url.PathEscape(fmt.Sprintf("%d", commentID)))

	req, err := c.buildRequest(ctx, http.MethodPatch, endpoint, bytes.NewReader(raw), installationID)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: update issue comment: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("update issue comment", resp); err != nil {
		return nil, err
	}
	var out IssueComment
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("githubclient: decode update issue comment: %w", err)
	}
	return &out, nil
}

// CheckRunStatus is the GitHub Checks API `status` enum.
// Closed set; passing anything else returns ErrValidation.
type CheckRunStatus string

// Check-run status values. Documented at
// https://docs.github.com/en/rest/checks/runs.
const (
	CheckRunStatusQueued     CheckRunStatus = "queued"
	CheckRunStatusInProgress CheckRunStatus = "in_progress"
	CheckRunStatusCompleted  CheckRunStatus = "completed"
)

// CheckRunConclusion is the Checks API `conclusion` enum.
// Required when status=completed; must be empty otherwise.
type CheckRunConclusion string

// Check-run conclusion values.
const (
	CheckRunConclusionSuccess        CheckRunConclusion = "success"
	CheckRunConclusionFailure        CheckRunConclusion = "failure"
	CheckRunConclusionNeutral        CheckRunConclusion = "neutral"
	CheckRunConclusionCancelled      CheckRunConclusion = "cancelled"
	CheckRunConclusionTimedOut       CheckRunConclusion = "timed_out"
	CheckRunConclusionActionRequired CheckRunConclusion = "action_required"
	CheckRunConclusionSkipped        CheckRunConclusion = "skipped"
)

// CreateCheckRunParams is the typed wire body for
// POST /repos/{owner}/{repo}/check-runs. Only the fields Fishhawk
// uses today are surfaced; the GitHub schema is wider.
type CreateCheckRunParams struct {
	Name          string
	HeadSHA       string
	Status        CheckRunStatus
	Conclusion    CheckRunConclusion // required when Status==completed
	DetailsURL    string             // where the "Details" link on github.com points (typically a Fishhawk run URL)
	OutputTitle   string
	OutputSummary string
}

// CreateCheckRunResult carries the bits of GitHub's response we
// care about. ID lets a caller PATCH the same row later if a
// follow-up surface ever needs progressive updates; v0 callers
// typically POST a fresh row per state change and ignore it.
type CreateCheckRunResult struct {
	ID      int64
	HTMLURL string
}

// CreateCheckRun publishes a check run on a head commit (#231).
//
//	POST /repos/{owner}/{repo}/check-runs
//
// Each call creates a new row; GitHub displays the most recent
// per (head_sha, name) on the PR's checks panel and uses it for
// branch-protection evaluation. Re-POSTing identical state is
// safe but wasteful — callers should dedup at the application
// layer (the auditcomplete publisher does this).
//
// Returns ErrValidation when the params are malformed (missing
// required fields, conclusion without status=completed),
// ErrNotFound when the repo isn't visible to the installation,
// ErrForbidden when the installation token lacks `checks:write`.
func (c *Client) CreateCheckRun(ctx context.Context, installationID int64, repo RepoRef, p CreateCheckRunParams) (*CreateCheckRunResult, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if p.Name == "" {
		return nil, errors.New("githubclient: check-run name required")
	}
	if p.HeadSHA == "" {
		return nil, errors.New("githubclient: head_sha required")
	}
	if p.Status == "" {
		return nil, errors.New("githubclient: status required")
	}
	if p.Status == CheckRunStatusCompleted && p.Conclusion == "" {
		return nil, errors.New("githubclient: conclusion required when status=completed")
	}
	if p.Status != CheckRunStatusCompleted && p.Conclusion != "" {
		return nil, errors.New("githubclient: conclusion only allowed when status=completed")
	}

	body := map[string]any{
		"name":     p.Name,
		"head_sha": p.HeadSHA,
		"status":   string(p.Status),
	}
	if p.Conclusion != "" {
		body["conclusion"] = string(p.Conclusion)
	}
	if p.DetailsURL != "" {
		body["details_url"] = p.DetailsURL
	}
	if p.OutputTitle != "" || p.OutputSummary != "" {
		// GitHub requires both `title` and `summary` when `output`
		// is present. Default the title when only a summary is set
		// so callers can pass just the body without ceremony.
		title := p.OutputTitle
		if title == "" {
			title = p.Name
		}
		body["output"] = map[string]string{
			"title":   title,
			"summary": p.OutputSummary,
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("githubclient: marshal check-run body: %w", err)
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) + "/check-runs")

	req, err := c.buildRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(raw), installationID)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: create check run: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("create check run", resp); err != nil {
		return nil, err
	}

	var out struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("githubclient: decode check run: %w", err)
	}
	return &CreateCheckRunResult{ID: out.ID, HTMLURL: out.HTMLURL}, nil
}

// IssueCommentReaction is the subset of GitHub's reaction payload
// Fishhawk reads from
// `GET /repos/{owner}/{repo}/issues/comments/{comment_id}/reactions`.
// Used by the reaction-polling worker (#360) to catch
// 👍-as-approval without polling the PR or replying in text.
type IssueCommentReaction struct {
	ID      int64                 `json:"id"`
	Content IssueCommentReactKind `json:"content"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
}

// IssueCommentReactKind is the closed set GitHub uses for the
// `content` field on a reaction. Spelled "+1" / "-1" rather than
// "thumbs_up" because that's GitHub's wire format.
type IssueCommentReactKind string

// IssueCommentReactKind values.
const (
	ReactPlusOne  IssueCommentReactKind = "+1"
	ReactMinusOne IssueCommentReactKind = "-1"
	ReactLaugh    IssueCommentReactKind = "laugh"
	ReactConfused IssueCommentReactKind = "confused"
	ReactHeart    IssueCommentReactKind = "heart"
	ReactHooray   IssueCommentReactKind = "hooray"
	ReactRocket   IssueCommentReactKind = "rocket"
	ReactEyes     IssueCommentReactKind = "eyes"
)

// ListIssueCommentReactions returns every reaction on an issue
// comment (#360).
//
//	GET /repos/{owner}/{repo}/issues/comments/{comment_id}/reactions
//
// The endpoint is paginated (30 per page default). v0's polling
// worker walks pages until the response is short of a full page —
// reactions accumulate slowly on a plan comment so the all-pages
// fetch is cheap.
//
// Returns ErrNotFound when the comment was deleted, ErrForbidden
// when the installation lacks `issues:read` (covered by the
// existing `issues:write` scope; this is a defensive check).
func (c *Client) ListIssueCommentReactions(ctx context.Context, installationID int64, repo RepoRef, commentID int64) ([]IssueCommentReaction, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if commentID <= 0 {
		return nil, errors.New("githubclient: comment id must be > 0")
	}

	out := []IssueCommentReaction{}
	const perPage = 100
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf(
			"%s/repos/%s/%s/issues/comments/%d/reactions?per_page=%d&page=%d",
			c.endpoint(""), url.PathEscape(repo.Owner), url.PathEscape(repo.Name), commentID, perPage, page,
		)
		req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
		if err != nil {
			return nil, err
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("githubclient: list reactions: %w", err)
		}
		closeErr := classifyStatus("list issue comment reactions", resp)
		if closeErr != nil {
			_ = resp.Body.Close()
			return nil, closeErr
		}
		var batch []IssueCommentReaction
		if err := json.NewDecoder(resp.Body).Decode(&batch); err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("githubclient: decode reactions: %w", err)
		}
		_ = resp.Body.Close()
		out = append(out, batch...)
		if len(batch) < perPage {
			break
		}
	}
	return out, nil
}

// GetRepoInstallation returns the GitHub App's installation ID for
// the given repo. Requires the App JWT (not an installation token)
// because the endpoint is App-level, not installation-level.
//
//	GET /repos/{owner}/{repo}/installation
//
// Returns ErrNotInstalled when the App is not installed on the repo
// (GitHub returns 404 for that case). Returns a wrapped error with
// the HTTP status code for other non-2xx responses.
func (c *Client) GetRepoInstallation(ctx context.Context, repo RepoRef) (int64, error) {
	if repo.Owner == "" || repo.Name == "" {
		return 0, errors.New("githubclient: repo owner and name required")
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/installation")

	req, err := c.buildAppJWTRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("githubclient: get repo installation: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		body := readBriefBody(resp.Body)
		return 0, fmt.Errorf("%w: %s", ErrNotInstalled, body)
	}
	if err := classifyStatus("get repo installation", resp); err != nil {
		return 0, err
	}

	var body struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("githubclient: decode installation: %w", err)
	}
	if body.ID == 0 {
		return 0, errors.New("githubclient: installation response missing id")
	}
	return body.ID, nil
}

// App is the slice of the GitHub App's own metadata Fishhawk needs:
// the App's slug, which composes the bot account's git commit identity
// (`<slug>[bot]`). Other fields land here as features need them.
type App struct {
	Slug string
}

// GetApp returns the authenticated GitHub App's metadata. Requires the
// App JWT (not an installation token) because the endpoint authenticates
// as the App itself.
//
//	GET /app
//
// Used to resolve the App's slug for the bot commit identity (#722). A
// client built via New (no signer / dev) hits the AppJWT nil guard in
// buildAppJWTRequest and returns the configured error rather than a
// wrong identity. Returns ErrForbidden / ErrNotFound via classifyStatus
// for non-2xx responses.
func (c *Client) GetApp(ctx context.Context) (*App, error) {
	endpoint := c.endpoint("/app")

	req, err := c.buildAppJWTRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: get app: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("get app", resp); err != nil {
		return nil, err
	}

	var body struct {
		Slug string `json:"slug"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode app: %w", err)
	}
	return &App{Slug: body.Slug}, nil
}

// User is the slice of a GitHub user/account payload Fishhawk needs:
// the numeric account id, which composes the bot's no-reply commit
// email (`<id>+<login>@users.noreply.github.com`). Read via an
// unauthenticated public-user lookup (GetUser), not the App JWT.
type User struct {
	ID    int64
	Login string
}

// GetUser fetches a single account by login via an UNAUTHENTICATED
// public-user lookup. The bot user-id read here belongs to the App's
// own bot account, resolved alongside GetApp in the commit-identity
// flow (#722).
//
//	GET /users/{login}
//
// This endpoint is public and returns 200 without auth, so GetUser does
// NOT send the App JWT — and must not. The App JWT is only valid for
// /app* endpoints; routing this public call through it made GitHub 401
// with "Bad credentials", silently failing the commit-identity resolve
// and falling back to the hardcoded fishhawk-runner[bot] (#750). GetUser
// is therefore independent of the App JWT and resolves even when AppJWT
// is nil.
//
// Returns ErrNotFound when the login doesn't exist or isn't visible,
// ErrForbidden on auth issues.
func (c *Client) GetUser(ctx context.Context, login string) (*User, error) {
	if login == "" {
		return nil, errors.New("githubclient: login required")
	}

	endpoint := c.endpoint("/users/" + url.PathEscape(login))

	req, err := c.buildAnonymousRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: get user: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("get user", resp); err != nil {
		return nil, err
	}

	var body struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode user: %w", err)
	}
	return &User{ID: body.ID, Login: body.Login}, nil
}

// buildAppJWTRequest constructs an http.Request authenticated as the
// GitHub App itself (App JWT). Used for endpoints that require App-level
// auth rather than installation-level auth (e.g. GetRepoInstallation).
func (c *Client) buildAppJWTRequest(ctx context.Context, method, rawURL string, body io.Reader) (*http.Request, error) {
	if c.AppJWT == nil {
		return nil, errors.New("githubclient: AppJWT not configured for App-level requests")
	}
	jwt, err := c.AppJWT()
	if err != nil {
		return nil, fmt.Errorf("githubclient: get app jwt: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("githubclient: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return req, nil
}

// buildAnonymousRequest constructs an http.Request with the standard
// GitHub headers but NO Authorization header — for public endpoints
// (e.g. GET /users/{login}) that return 200 unauthenticated and that
// the App JWT must not touch (it is only valid for /app* endpoints; see
// GetUser / #750). Unauthenticated requests share GitHub's lower
// per-IP rate limit, acceptable here because the only caller resolves
// once per process (#722).
func (*Client) buildAnonymousRequest(ctx context.Context, method, rawURL string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("githubclient: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return req, nil
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
