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
	"strconv"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubapp"
)

// DefaultBaseURL is GitHub's REST API root. Tests override this
// via Client.BaseURL pointing at httptest fakes.
const DefaultBaseURL = "https://api.github.com"

// DefaultUploadBaseURL is GitHub's release-asset upload host. Release
// asset uploads go to a SEPARATE host from the REST API — POST
// https://uploads.github.com/repos/{owner}/{repo}/releases/{id}/assets
// (per the GitHub REST "Upload a release asset" docs). Tests override this
// via Client.UploadBaseURL pointing at an httptest fake, and the upload
// test asserts the request hit THIS host, not api.github.com.
const DefaultUploadBaseURL = "https://uploads.github.com"

// WorkflowSpecPath is the canonical location of the workflow spec
// in a customer's repo (per MVP_SPEC §4.1).
const WorkflowSpecPath = ".fishhawk/workflows.yaml"

// Errors callers may want to switch on.
//
// Each is an ASSIGNMENT to the canonical forge sentinel (ADR-058 /
// E45.4), not a fresh errors.New: both spellings therefore bind the SAME
// error value, so errors.Is holds across them and a caller may switch on
// either. forge/types.go carries the per-sentinel contract.
var (
	ErrNotFound                = forge.ErrNotFound
	ErrForbidden               = forge.ErrForbidden
	ErrValidation              = forge.ErrValidation
	ErrNotInstalled            = forge.ErrNotInstalled
	ErrPullRequestExists       = forge.ErrPullRequestExists
	ErrMergeConflict           = forge.ErrMergeConflict
	ErrPullRequestCleanStatus  = forge.ErrPullRequestCleanStatus
	ErrPullRequestNotMergeable = forge.ErrPullRequestNotMergeable
)

// The Forge-surface vocabulary lives canonically in the forge package
// now (ADR-058 / E45.4); githubclient re-declares each moved name as a
// type ALIAS. Every existing reference — in production code and in the
// many test fixtures that build &githubclient.Client{} literals and
// githubclient.PullRequest{} values — keeps compiling against the same
// type with zero behavior change: an alias is the same type, not a new
// named type, so method sets and assignability are preserved. The
// consumer-migration gate (forge/consumer_migration_gate_test.go)
// enforces that migrated packages reference forge.* directly rather than
// leaning on these aliases; the aliases exist for the unmigrated
// non-forge surfaces (issues/comments/reactions) that still spell
// RepoRef through githubclient.

// RepoRef aliases forge.RepoRef.
type RepoRef = forge.RepoRef

// Repository aliases forge.Repository.
type Repository = forge.Repository

// GitCommit aliases forge.GitCommit.
type GitCommit = forge.GitCommit

// TreeEntry aliases forge.TreeEntry.
type TreeEntry = forge.TreeEntry

// PullRequest aliases forge.PullRequest.
type PullRequest = forge.PullRequest

// PullRequestRef aliases forge.PullRequestRef.
type PullRequestRef = forge.PullRequestRef

// MergeMethod aliases forge.MergeMethod.
type MergeMethod = forge.MergeMethod

// BranchProtection aliases forge.BranchProtection.
type BranchProtection = forge.BranchProtection

// RulesetRequiredCheck aliases forge.RulesetRequiredCheck.
type RulesetRequiredCheck = forge.RulesetRequiredCheck

// ComparePatchFile aliases forge.ComparePatchFile.
type ComparePatchFile = forge.ComparePatchFile

// ComparePatchResult aliases forge.ComparePatchResult.
type ComparePatchResult = forge.ComparePatchResult

// CheckRunStatus aliases forge.CheckRunStatus.
type CheckRunStatus = forge.CheckRunStatus

// CheckRunConclusion aliases forge.CheckRunConclusion.
type CheckRunConclusion = forge.CheckRunConclusion

// CreateCheckRunParams aliases forge.CreateCheckRunParams.
type CreateCheckRunParams = forge.CreateCheckRunParams

// CreateCheckRunResult aliases forge.CreateCheckRunResult.
type CreateCheckRunResult = forge.CreateCheckRunResult

// Const aliases for the moved enum values. A const declared as
// `X = forge.X` is the same constant value, so switch arms and
// comparisons against either spelling are identical.
const (
	MergeMethodSquash = forge.MergeMethodSquash
	MergeMethodMerge  = forge.MergeMethodMerge
	MergeMethodRebase = forge.MergeMethodRebase

	CheckRunStatusQueued     = forge.CheckRunStatusQueued
	CheckRunStatusInProgress = forge.CheckRunStatusInProgress
	CheckRunStatusCompleted  = forge.CheckRunStatusCompleted

	CheckRunConclusionSuccess        = forge.CheckRunConclusionSuccess
	CheckRunConclusionFailure        = forge.CheckRunConclusionFailure
	CheckRunConclusionNeutral        = forge.CheckRunConclusionNeutral
	CheckRunConclusionCancelled      = forge.CheckRunConclusionCancelled
	CheckRunConclusionTimedOut       = forge.CheckRunConclusionTimedOut
	CheckRunConclusionActionRequired = forge.CheckRunConclusionActionRequired
	CheckRunConclusionSkipped        = forge.CheckRunConclusionSkipped
)

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

	// UploadBaseURL is the release-asset upload host. Empty →
	// DefaultUploadBaseURL. Release asset uploads (UploadReleaseAsset)
	// target this host, not BaseURL — GitHub serves asset uploads from
	// uploads.github.com rather than api.github.com. Test-overridable to
	// point at an httptest fake.
	UploadBaseURL string

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

	// ProjectsToken is an optional static user PAT/UAT carrying the
	// `project` scope. Empty → not configured. It is used ONLY for
	// GraphQL against USER-owned Projects (v2) boards, which App
	// installation tokens cannot reach — there is no user-projects
	// permission for GitHub Apps (#1114). doGraphQL swaps to this
	// token only when the request opts in via WithProjectsToken AND
	// this field is non-empty; otherwise the installation-token path
	// is unchanged. It is backend config, never run data — it MUST
	// NOT be logged, traced, or included in any error message.
	ProjectsToken string

	// ResolveBaseURL, when non-nil, resolves the per-installation REST API base
	// URL for an installation (Mode 2, data-resident installs on
	// <slug>.ghe.com), following the githubapp mint precedent (E44.2 / #1826)
	// promoted to the whole REST surface here (E44.16 / #2094). It is consulted
	// at the buildRequest choke point — the SOLE request-construction path for
	// every installation-scoped method (codescanning/gitdata/projects/... all
	// route through it) — passing the stringified installation id (the
	// forge-neutral installation_ref the account resolver keys on):
	//
	//   - a non-empty return OVERRIDES the deployment API base for that request:
	//     the request's scheme+host (and any base path) is swapped to the
	//     resolved base, path+query preserved. It is validated as a well-formed
	//     absolute https URL (account.ValidateResolvedBaseURL) and, when
	//     AllowedInstallationHosts is non-empty, pinned to the allowlist
	//     (account.HostAllowed) BEFORE the installation token ships — a bad
	//     scheme or a disallowed host FAILS CLOSED (the request is never issued).
	//   - an empty return keeps the deployment default (BaseURL/DefaultBaseURL):
	//     the intentional absence of an override (NULL column / unknown
	//     installation) is byte-identical to Mode 1.
	//   - a NON-NIL error FAILS CLOSED (the request is never issued): a real
	//     endpoint-resolution fault must never silently target the default host
	//     for a data-resident install.
	//
	// ONLY requests targeting the REST API base are rewritten. Release-asset
	// uploads (UploadBaseURL/DefaultUploadBaseURL) and the static-token
	// user-Projects GraphQL path (buildStaticTokenRequest) are NOT installation
	// scoped and are left byte-identical — the rewrite is gated on the request
	// URL's API-base prefix, not on which caller issued it, so installation
	// GraphQL via buildRequest IS rewritten while an upload is not (#2094
	// binding condition 3).
	//
	// Nil (the default) preserves the pre-#2094 behavior for every method.
	ResolveBaseURL func(ctx context.Context, installationRef string) (string, error)

	// AllowedInstallationHosts, when non-empty, restricts the resolved
	// per-installation base URL (see ResolveBaseURL) to an operator-configured
	// allowlist, enforced at buildRequest time BEFORE the installation token
	// ships. Entries and matching semantics are the shared forge-neutral
	// account.HostAllowed contract (exact host or leading-dot label-boundary
	// suffix, case- and port-insensitive). Empty/nil (the default) applies
	// scheme/parse validation only, per the #2093 operator arbitration.
	AllowedInstallationHosts []string
}

// New returns a Client with sensible defaults. tokens is required;
// without it every call returns an error before touching the wire.
//
// New is the GitHub-convenience constructor: it takes the int64-taking
// githubapp.TokenProvider directly. NewWithCredentialProvider is the
// forge-neutral entry point (#1855). Both yield the same scope-taking
// method surface — the constructor chosen only decides where tokens
// come from, never what callers pass.
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
func (c *Client) GetFile(ctx context.Context, scope forge.CredentialScope, repo RepoRef, path, ref string) (*FileContent, error) {
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
func (c *Client) GetWorkflowSpec(ctx context.Context, scope forge.CredentialScope, repo RepoRef, ref string) (*FileContent, error) {
	return c.GetFile(ctx, scope, repo, WorkflowSpecPath, ref)
}

// DirEntry is one entry in a Contents-API directory listing
// (ListDirectory): the entry's base name, repo-relative path, and type
// ("file" | "dir" | "symlink" | "submodule").
type DirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
}

// ListDirectory lists a repository directory's entries at the given ref.
// path is relative to the repo root (no leading slash); ref="" serves
// the repository's default branch.
//
//	GET /repos/{owner}/{repo}/contents/{path}?ref={ref}
//
// On a directory path the Contents API returns a JSON ARRAY of entries
// (name/path/type) rather than GetFile's single-object file shape, capped
// at 1000 entries per directory — far above any Go package directory's
// file count, so no pagination here. Returns ErrNotFound when the
// directory or repo isn't visible to the installation, ErrForbidden on
// auth issues, and a descriptive error (never a panic) when path names a
// file — GitHub answers with a JSON object instead of an array there.
func (c *Client) ListDirectory(ctx context.Context, scope forge.CredentialScope, repo RepoRef, path, ref string) ([]DirEntry, error) {
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
		return nil, fmt.Errorf("githubclient: list directory: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("list directory", resp); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("githubclient: read directory listing: %w", err)
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, fmt.Errorf("githubclient: %s is not a directory (contents response is not a listing array)", path)
	}
	var entries []DirEntry
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return nil, fmt.Errorf("githubclient: decode directory listing: %w", err)
	}
	return entries, nil
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
	// StateReason is GitHub's issue `state_reason`: "completed",
	// "not_planned", "reopened", or "" (empty when absent — e.g. an open
	// issue, or an older payload). The campaign run-less settle pass (#1558)
	// gates on State=="closed" AND StateReason=="completed" to distinguish a
	// real completion (issue closed by a merged PR) from a not_planned
	// abandonment. Additive — existing callers ignore it.
	StateReason string
	// Labels is the issue's label names. GitHub's REST issue payload
	// carries `labels` as an array whose entries are label objects
	// ({name, color, …}) or, for some responses, plain strings; GetIssue
	// decodes both forms into the bare names. Consumed by the area-label
	// derivation (#1616): a child's parent epic's area:* label is copied
	// onto the filing.
	Labels []string
}

// GetIssue fetches a single issue by number.
//
//	GET /repos/{owner}/{repo}/issues/{number}
//
// Used by the prompt-construction handler to build the
// agent-facing prompt from the originating issue. Returns
// ErrNotFound if the issue or repo isn't visible to the
// installation.
func (c *Client) GetIssue(ctx context.Context, scope forge.CredentialScope, repo RepoRef, number int) (*Issue, error) {
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
		Number      int    `json:"number"`
		Title       string `json:"title"`
		Body        string `json:"body"`
		State       string `json:"state"`
		StateReason string `json:"state_reason"`
		// labels entries are string-or-object per the REST Issues schema;
		// json.RawMessage defers the shape decision to decodeLabelNames.
		Labels []json.RawMessage `json:"labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode issue: %w", err)
	}
	return &Issue{
		Number:      body.Number,
		Title:       body.Title,
		Body:        body.Body,
		State:       body.State,
		StateReason: body.StateReason,
		Labels:      decodeLabelNames(body.Labels),
	}, nil
}

// decodeLabelNames tolerantly parses a GitHub issue payload's `labels`
// array, whose entries are label objects ({name, color, …}) or plain
// strings per the REST Issues schema. It returns the bare names, skipping
// any entry that is neither form or whose name is empty. Returns nil for an
// empty/absent array so a labelless issue carries a nil Labels slice.
func decodeLabelNames(raw []json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var names []string
	for _, entry := range raw {
		var obj struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(entry, &obj); err == nil && obj.Name != "" {
			names = append(names, obj.Name)
			continue
		}
		var s string
		if err := json.Unmarshal(entry, &s); err == nil && s != "" {
			names = append(names, s)
		}
	}
	return names
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
func (c *Client) GetBranchProtection(ctx context.Context, scope forge.CredentialScope, repo RepoRef, branch string) (*BranchProtection, error) {
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
func (c *Client) ListRulesetRequiredChecks(ctx context.Context, scope forge.CredentialScope, repo RepoRef, branch string) ([]RulesetRequiredCheck, error) {
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
func (c *Client) EnableAutoMerge(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, prNumber int, method MergeMethod) error {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
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

	pr, err := c.GetPullRequest(ctx, scope, repo, prNumber)
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
		msg := gqlResp.Errors[0].Message
		// A "clean status" rejection is the already-merge-ready case (#1954):
		// the PR could be merged synchronously right now, so GitHub refuses to
		// queue auto-merge. Wrap ErrPullRequestCleanStatus ALONGSIDE
		// ErrValidation (Go multi-%w) so the operator merge path can fall back
		// to a synchronous REST merge on this sentinel while every existing
		// errors.Is(err, ErrValidation) caller is unchanged.
		if strings.Contains(strings.ToLower(msg), "clean status") {
			return fmt.Errorf("%w: %w: enable auto-merge: %s", ErrValidation, ErrPullRequestCleanStatus, msg)
		}
		return fmt.Errorf("%w: enable auto-merge: %s", ErrValidation, msg)
	}
	return nil
}

// MergePullRequest synchronously merges a PR via the REST API (E48.7 / #1954):
//
//	PUT /repos/{owner}/{repo}/pulls/{number}/merge
//
// It is the fallback the operator merge path takes when EnableAutoMerge is
// rejected with ErrPullRequestCleanStatus — a PR that is already merge-ready
// (approved, required checks green) cannot be queued for auto-merge, so it is
// merged directly here instead. The wire `merge_method` is the lower-cased
// MergeMethod (REST spells the strategies `squash`/`merge`/`rebase`, distinct
// from the GraphQL enum EnableAutoMerge uses); an empty method defaults to
// squash to match the auto-merge default.
//
// Returns nil on success (200) and on 204 (rare idempotent no-content).
// ErrPullRequestNotMergeable on 405 (the PR is not in a mergeable state — the
// actionable, retryable case). ErrMergeConflict on 409 (head branch moved or a
// merge conflict). ErrNotFound (404) / ErrForbidden (401/403) / ErrValidation
// (422) are mapped like the sibling REST methods via classifyStatus.
func (c *Client) MergePullRequest(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, prNumber int, method MergeMethod) error {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
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

	body := map[string]string{"merge_method": strings.ToLower(string(method))}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("githubclient: marshal merge pull request: %w", err)
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) + "/pulls/" + url.PathEscape(fmt.Sprintf("%d", prNumber)) + "/merge")
	req, err := c.buildRequest(ctx, http.MethodPut, endpoint, bytes.NewReader(raw), installationID)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("githubclient: merge pull request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 405 = not mergeable (base moved, checks not settled, draft). Mapped to a
	// dedicated actionable/retryable sentinel BEFORE classifyStatus (which has
	// no 405 case and would fall to the opaque default).
	if resp.StatusCode == http.StatusMethodNotAllowed {
		brief := readBriefBody(resp.Body)
		return fmt.Errorf("%w: merge pr #%d: %s", ErrPullRequestNotMergeable, prNumber, brief)
	}
	// 409 = the head branch was modified / merge conflict — a retry after a
	// rebase can succeed. Reuse ErrMergeConflict so a caller that already
	// switches on it treats this uniformly.
	if resp.StatusCode == http.StatusConflict {
		brief := readBriefBody(resp.Body)
		return fmt.Errorf("%w: merge pr #%d: %s", ErrMergeConflict, prNumber, brief)
	}
	return classifyStatus("merge pull request", resp)
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
func (c *Client) GetPullRequest(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, number int) (*PullRequest, error) {
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
		Body   string `json:"body"`
		Head   struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
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
		HeadRef: body.Head.Ref,
		Body:    body.Body,
	}, nil
}

// EditPullRequest replaces a pull request's body (#1702). It is the write
// half of the merge-time economics stamp: resolveReviewStageOnMerge reads the
// current body via GetPullRequest, splices its delimited economics section in,
// and calls this to persist the result.
//
//	PATCH /repos/{owner}/{repo}/pulls/{number}
//	{ "body": "<body>" }
//
// This is the same endpoint GitHub documents for "Update a pull request";
// editing the body is permitted regardless of merge state, so a stamp on an
// already-merged PR succeeds. Mirrors the ClosePullRequest PATCH pattern.
// Returns ErrNotFound when the repo/PR isn't visible to the installation,
// ErrForbidden on auth issues, ErrValidation when GitHub rejects the update.
func (c *Client) EditPullRequest(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, number int, body string) error {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	if c.Tokens == nil {
		return errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return errors.New("githubclient: repo owner and name required")
	}
	if number <= 0 {
		return errors.New("githubclient: pr number must be > 0")
	}

	raw, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return fmt.Errorf("githubclient: marshal edit pr: %w", err)
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/pulls/" + url.PathEscape(fmt.Sprintf("%d", number)))
	req, err := c.buildRequest(ctx, http.MethodPatch, endpoint, bytes.NewReader(raw), installationID)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("githubclient: edit pr: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return classifyStatus("edit pr", resp)
}

// ReleaseAsset is one asset attached to a GitHub Release — the id (used to
// delete/replace the asset) and its file name (matched against the fixed
// release-notes asset name for idempotent replacement). Other fields
// (size, content_type, download URL) land here as callers need them.
type ReleaseAsset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Release is the slice of a GitHub Release payload the publish integration
// reads (E33.3 / #1588): the numeric id (addresses the body-PATCH + asset
// endpoints), the tag, the current body (hashed for content-hash idempotency),
// the html_url (recorded on the release_published audit entry), and the
// attached assets (scanned for the fixed release-notes asset to replace).
type Release struct {
	ID      int64          `json:"id"`
	TagName string         `json:"tag_name"`
	Body    string         `json:"body"`
	HTMLURL string         `json:"html_url"`
	Assets  []ReleaseAsset `json:"assets"`
}

// GetReleaseByTag fetches a published Release by its tag (E33.3 / #1588).
//
//	GET /repos/{owner}/{repo}/releases/tags/{tag}
//
// Returns ErrNotFound when no release exists for the tag (or the repo isn't
// visible to the installation), ErrForbidden on auth issues. The Releases
// endpoints are covered by the App's existing contents:write grant — no new
// permission (ADR-051 accepted-condition 3, confirmed on #1588).
func (c *Client) GetReleaseByTag(ctx context.Context, scope forge.CredentialScope, repo RepoRef, tag string) (*Release, error) {
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
	if tag == "" {
		return nil, errors.New("githubclient: tag is required")
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/releases/tags/" + escapePath(tag))
	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: get release by tag: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := classifyStatus("get release by tag", resp); err != nil {
		return nil, err
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("githubclient: decode release: %w", err)
	}
	return &rel, nil
}

// UpdateReleaseBody replaces a Release's body with the persisted release-notes
// markdown (E33.3 / #1588). Mirrors EditPullRequest's PATCH-with-body shape.
//
//	PATCH /repos/{owner}/{repo}/releases/{id}
//	{ "body": "<body>" }
//
// Returns ErrNotFound when the release/repo isn't visible to the installation,
// ErrForbidden on auth issues, ErrValidation when GitHub rejects the update.
func (c *Client) UpdateReleaseBody(ctx context.Context, scope forge.CredentialScope, repo RepoRef, releaseID int64, body string) error {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	if c.Tokens == nil {
		return errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return errors.New("githubclient: repo owner and name required")
	}
	if releaseID <= 0 {
		return errors.New("githubclient: release id must be > 0")
	}

	raw, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return fmt.Errorf("githubclient: marshal update release body: %w", err)
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/releases/" + url.PathEscape(fmt.Sprintf("%d", releaseID)))
	req, err := c.buildRequest(ctx, http.MethodPatch, endpoint, bytes.NewReader(raw), installationID)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("githubclient: update release body: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return classifyStatus("update release body", resp)
}

// DeleteReleaseAsset removes an existing release asset by id (E33.3 / #1588).
// The publish integration deletes a stale release-notes asset by name before
// re-uploading, so the attached asset can never diverge from the Release body
// (binding approval condition: content-hash idempotency for both surfaces).
//
//	DELETE /repos/{owner}/{repo}/releases/assets/{asset_id}
//
// A 204 (no content) is success. Returns ErrNotFound when the asset/repo isn't
// visible, ErrForbidden on auth issues.
func (c *Client) DeleteReleaseAsset(ctx context.Context, scope forge.CredentialScope, repo RepoRef, assetID int64) error {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	if c.Tokens == nil {
		return errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return errors.New("githubclient: repo owner and name required")
	}
	if assetID <= 0 {
		return errors.New("githubclient: asset id must be > 0")
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/releases/assets/" + url.PathEscape(fmt.Sprintf("%d", assetID)))
	req, err := c.buildRequest(ctx, http.MethodDelete, endpoint, nil, installationID)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("githubclient: delete release asset: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return classifyStatus("delete release asset", resp)
}

// UploadReleaseAsset attaches raw bytes to a Release as a named asset
// (E33.3 / #1588). It targets the SEPARATE upload host (UploadBaseURL /
// uploads.github.com), NOT the REST API host — GitHub serves asset uploads
// from a different origin.
//
//	POST {UploadBaseURL}/repos/{owner}/{repo}/releases/{id}/assets?name={name}
//	Content-Type: <contentType>
//	<raw bytes>
//
// Returns ErrNotFound when the release/repo isn't visible to the installation,
// ErrForbidden on auth issues, ErrValidation when GitHub rejects the upload
// (e.g. an asset with the same name already exists — the caller deletes the
// stale asset first).
func (c *Client) UploadReleaseAsset(ctx context.Context, scope forge.CredentialScope, repo RepoRef, releaseID int64, name, contentType string, data []byte) error {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	if c.Tokens == nil {
		return errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return errors.New("githubclient: repo owner and name required")
	}
	if releaseID <= 0 {
		return errors.New("githubclient: release id must be > 0")
	}
	if name == "" {
		return errors.New("githubclient: asset name is required")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	endpoint := c.uploadEndpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/releases/" + url.PathEscape(fmt.Sprintf("%d", releaseID)) +
		"/assets?name=" + url.QueryEscape(name))
	req, err := c.buildRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(data), installationID)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("githubclient: upload release asset: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return classifyStatus("upload release asset", resp)
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
func (c *Client) CompareCommits(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, base, head string) ([]string, error) {
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

// ListPullRequestsForCommit returns the MERGED pull requests associated
// with a commit — the squash/merge commit that landed a PR carries the
// PR in this list, which is how the release-evidence walk maps a commit
// in a compare range back to its merged PR.
//
//	GET /repos/{owner}/{repo}/commits/{sha}/pulls
//
// Each PR object carries merged_at (null until merged); this filters to
// the merged ones so an open PR that also touches the commit is
// excluded. Returns a typed error (ErrNotFound / ErrValidation /
// ErrForbidden) on non-2xx so callers can classify the failure and
// decide their own policy — the sole production caller
// (GitHubResolver.MergedPRsInRange) propagates it and fails the
// release-evidence assembly closed. Mirrors CompareCommits'
// TokenProvider guard, endpoint building, and classifyStatus error
// mapping.
//
// No pagination: the number of PRs associated with a single commit is
// tiny (in practice one — the PR the commit landed), far below the
// per-page default.
func (c *Client) ListPullRequestsForCommit(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, sha string) ([]PullRequestRef, error) {
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
		"/commits/" + escapePath(sha) + "/pulls")
	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: list pulls for commit: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := classifyStatus("list pulls for commit", resp); err != nil {
		return nil, err
	}
	var body []struct {
		Number   int     `json:"number"`
		HTMLURL  string  `json:"html_url"`
		Title    string  `json:"title"`
		MergedAt *string `json:"merged_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode pulls for commit: %w", err)
	}
	prs := make([]PullRequestRef, 0, len(body))
	for _, p := range body {
		// merged_at is null until the PR merges; skip any PR associated
		// with the commit that has not merged (an open PR touching it).
		if p.MergedAt == nil {
			continue
		}
		prs = append(prs, PullRequestRef{
			Number: p.Number,
			URL:    p.HTMLURL,
			Title:  p.Title,
		})
	}
	return prs, nil
}

// compareFilesCap is GitHub's documented per-response changed-file
// ceiling for the Compare API: a comparison touching more than 300 files
// returns only the first 300 in `files`. The consolidated decomposition
// fan-out (#1060) is exactly the large-diff case that trips it, so a hit
// at the cap is surfaced as a truncation rather than silently
// under-reviewed.
const compareFilesCap = 300

// ComparePatch returns the unified diff + changed-file list for base...head
// via the Compare API (#1060). It is the diff source for a decomposed
// parent's consolidated implement review: the parent has no runner bundle,
// so the diff that actually merges is sourced from GitHub here.
//
//	GET /repos/{owner}/{repo}/compare/{base}...{head}
//
// The three-dot form anchors on the merge-base (matching the PR's own
// diff), so commits merged into base while the run was open are excluded.
// The default JSON response is used rather than the raw-diff media type: it
// carries the per-file STATUS (needed to build policy.ChangedFile) and the
// truncation signals GitHub only exposes in the structured form — the
// 300-file cap and the per-file omitted-patch marker. The Patch is
// reconstructed by concatenating each file's hunks under a synthetic
// `diff --git` header.
//
// Returns a typed error (ErrNotFound / ErrValidation / ErrForbidden) on
// non-2xx. Truncation is NOT an error — the partial diff is returned with
// Truncated set so the caller can review what it has and surface the gap.
func (c *Client) ComparePatch(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, base, head string) (*ComparePatchResult, error) {
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
		return nil, fmt.Errorf("githubclient: compare patch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := classifyStatus("compare patch", resp); err != nil {
		return nil, err
	}

	var body struct {
		TotalCommits int `json:"total_commits"`
		Commits      []struct {
			SHA string `json:"sha"`
		} `json:"commits"`
		Files []struct {
			Filename string `json:"filename"`
			Status   string `json:"status"`
			Changes  int    `json:"changes"`
			Patch    string `json:"patch"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode compare patch: %w", err)
	}

	result := &ComparePatchResult{}
	if n := len(body.Commits); n > 0 {
		result.HeadSHA = body.Commits[n-1].SHA
	}

	var patch strings.Builder
	result.Files = make([]ComparePatchFile, 0, len(body.Files))
	for _, f := range body.Files {
		result.Files = append(result.Files, ComparePatchFile{Path: f.Filename, Status: f.Status})
		switch {
		case f.Patch != "":
			fmt.Fprintf(&patch, "diff --git a/%s b/%s\n%s\n", f.Filename, f.Filename, f.Patch)
		case f.Changes > 0:
			// A changed file whose patch body GitHub dropped (oversized) —
			// the review cannot see its content. A binary file legitimately
			// carries no patch but reports zero changes, so gating on
			// changes>0 isolates the genuine size-cap omission.
			result.Truncated = true
			result.TruncationReason = "one or more changed-file patch bodies omitted by GitHub (oversized diff)"
		}
	}
	result.Patch = patch.String()

	// The 300-file ceiling is the broader truncation (whole files dropped,
	// not just their patch bodies), so let it win the reason field.
	if len(body.Files) >= compareFilesCap {
		result.Truncated = true
		result.TruncationReason = fmt.Sprintf(
			"changed-file count reached GitHub's %d-file compare cap; files beyond the cap are not reviewed", compareFilesCap)
	}

	return result, nil
}

// ForceUpdateRef force-updates a branch ref to point at newSHA — the
// destructive remediation primitive for ADR-035 (#867). It rewinds the
// run/PR head branch back to its last run-authored commit, dropping a
// foreign commit pushed ON TOP of the run's own commits.
//
//	PATCH /repos/{owner}/{repo}/git/refs/heads/{branch}
//	{ "sha": "<newSHA>", "force": true }
//
// force:true is required because the move is a REWIND (newSHA is an
// ancestor of the current tip, not a fast-forward descendant); GitHub
// rejects a non-fast-forward ref update without it. The REST refs API
// exposes NO expected-sha precondition (no compare-and-swap / the
// --force-with-lease analog), so the lease check is the CALLER's
// responsibility: re-read the live head and confirm it still equals the
// classified SHA immediately before calling this, narrowing (never
// eliminating) the TOCTOU window against a concurrent push.
//
// Returns ErrNotFound when the repo/branch isn't visible to the
// installation, ErrForbidden on auth issues, ErrValidation when GitHub
// rejects the update (422 — e.g. the SHA doesn't exist on the repo). The
// dropped commit stays recoverable from the remote reflog / the foreign
// pusher's own branch.
func (c *Client) ForceUpdateRef(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, branch, newSHA string) error {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	if c.Tokens == nil {
		return errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return errors.New("githubclient: repo owner and name required")
	}
	if branch == "" {
		return errors.New("githubclient: branch is required")
	}
	if newSHA == "" {
		return errors.New("githubclient: newSHA is required")
	}

	raw, err := json.Marshal(map[string]any{"sha": newSHA, "force": true})
	if err != nil {
		return fmt.Errorf("githubclient: marshal force-update ref: %w", err)
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/git/refs/heads/" + escapePath(branch))
	req, err := c.buildRequest(ctx, http.MethodPatch, endpoint, bytes.NewReader(raw), installationID)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("githubclient: force-update ref: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return classifyStatus("force-update ref", resp)
}

// GetBranchSHA resolves a branch ref to its tip commit SHA (ADR-041 /
// #1142). It is the fan-in step's existence probe: the orchestrator reads
// the run's base ref to seed the consolidated branch, and probes the
// consolidated branch itself to decide whether to create it.
//
//	GET /repos/{owner}/{repo}/git/ref/heads/{branch}
//
// Returns (sha, true, nil) when the branch exists, ("", false, nil) on a
// 404 (the branch is absent — callers branch on absence rather than
// treating it as a hard error), and a typed error (ErrForbidden /
// ErrValidation) on other non-2xx responses.
func (c *Client) GetBranchSHA(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, branch string) (string, bool, error) {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return "", false, err
	}
	if c.Tokens == nil {
		return "", false, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return "", false, errors.New("githubclient: repo owner and name required")
	}
	if branch == "" {
		return "", false, errors.New("githubclient: branch is required")
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/git/ref/heads/" + escapePath(branch))
	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return "", false, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("githubclient: get branch sha: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("get branch sha", resp); err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}

	var body struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", false, fmt.Errorf("githubclient: decode branch ref: %w", err)
	}
	if body.Object.SHA == "" {
		return "", false, fmt.Errorf("githubclient: branch ref response missing object.sha")
	}
	return body.Object.SHA, true, nil
}

// CreateRef creates a new branch ref pointing at sha (ADR-041 / #1142).
// The fan-in step calls it to create the consolidated branch
// fishhawk/run-<parent> from the run's base ref when it does not yet
// exist (under E24.1 / #1141 NOBODY creates that branch — each child
// pushes only its own slice branch).
//
//	POST /repos/{owner}/{repo}/git/refs
//	{ "ref": "refs/heads/<branch>", "sha": "<sha>" }
//
// A 422 whose body indicates the reference already exists is treated as a
// benign idempotent no-op (returns nil): a re-entrant settle (the sweeper
// + event-driven race, or a retry after a non-conflict error) must not
// fail because a prior fan-in pass already created the branch. Returns
// ErrNotFound when the repo isn't visible, ErrForbidden on auth issues,
// ErrValidation for other 422s.
func (c *Client) CreateRef(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, branch, sha string) error {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	if c.Tokens == nil {
		return errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return errors.New("githubclient: repo owner and name required")
	}
	if branch == "" {
		return errors.New("githubclient: branch is required")
	}
	if sha == "" {
		return errors.New("githubclient: sha is required")
	}

	raw, err := json.Marshal(map[string]string{
		"ref": "refs/heads/" + branch,
		"sha": sha,
	})
	if err != nil {
		return fmt.Errorf("githubclient: marshal create ref: %w", err)
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) + "/git/refs")
	req, err := c.buildRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(raw), installationID)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("githubclient: create ref: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Body-sniff the 422 "Reference already exists" case BEFORE
	// classifyStatus consumes the body: a re-entrant fan-in pass that
	// finds the consolidated branch already created is a no-op, not a
	// failure.
	if resp.StatusCode == http.StatusUnprocessableEntity {
		brief := readBriefBody(resp.Body)
		if strings.Contains(strings.ToLower(brief), "already exists") {
			return nil
		}
		return fmt.Errorf("%w: create ref: %s", ErrValidation, brief)
	}
	return classifyStatus("create ref", resp)
}

// MergeBranch performs a server-side git merge of head into base
// (ADR-041 / #1142). It is the fan-in step's per-slice integration
// primitive: each succeeded slice branch is merged onto the consolidated
// branch in ascending slice-index order without a local working tree.
//
//	POST /repos/{owner}/{repo}/merges
//	{ "base": "<base>", "head": "<head>", "commit_message": "<msg>" }
//
// Status mapping (GitHub REST "Merge a branch"): 201 = merged — returns the
// resulting merge commit SHA decoded from the response body's `sha` field;
// 204 = nothing to merge / base already contains head ("", nil, idempotent);
// 409 = ErrMergeConflict, 404 = ErrNotFound (base or head missing),
// 422 = ErrValidation (each returns an empty SHA). The 204 case makes a
// re-entrant settle a clean no-op once a slice is already integrated.
//
// The returned merge commit SHA is recorded by the fan-in caller in the
// slices_integrated audit payload (#1459) so a later boundary's ADR-035
// lineage guard attributes the integration merges instead of flagging them
// foreign. Decode is defensive: an unparseable or absent `sha` in a 201 body
// returns ("", nil) so a body-shape quirk never wedges an otherwise clean
// fan-in — the merge still happened; only its SHA goes unrecorded.
func (c *Client) MergeBranch(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, base, head, commitMessage string) (string, error) {
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
	if base == "" || head == "" {
		return "", errors.New("githubclient: merge base and head required")
	}

	body := map[string]string{"base": base, "head": head}
	if commitMessage != "" {
		body["commit_message"] = commitMessage
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("githubclient: marshal merge branch: %w", err)
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) + "/merges")
	req, err := c.buildRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(raw), installationID)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("githubclient: merge branch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 409 is a genuine merge conflict — the fan-in caller switches on the
	// dedicated sentinel to fail the parent recoverable. Mapped here
	// before classifyStatus (which has no 409 case).
	if resp.StatusCode == http.StatusConflict {
		brief := readBriefBody(resp.Body)
		return "", fmt.Errorf("%w: merge %s into %s: %s", ErrMergeConflict, head, base, brief)
	}
	// 204 = base already contains head (nothing to merge) — idempotent
	// success for a re-entrant fan-in pass; no merge commit was created.
	if resp.StatusCode == http.StatusNoContent {
		return "", nil
	}
	if err := classifyStatus("merge branch", resp); err != nil {
		return "", err
	}
	// 201 Created — decode the merge commit SHA. Defensive: a decode error or
	// an absent sha returns ("", nil), never an error, so a body-shape quirk
	// cannot wedge a fan-in whose merge already succeeded.
	var mergeBody struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&mergeBody); err != nil {
		return "", nil
	}
	return mergeBody.SHA, nil
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
func (c *Client) CreatePullRequest(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, head, base, title, body string) (*PullRequest, error) {
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

// ClosePullRequest closes an open pull request (#877). It is the
// remediation surface for a gating implement-review reject: the review
// fails the implement stage category-B during the raw-trace upload, but
// the runner independently opens a PR before learning the stage failed,
// leaving a dangling PR for a change that will never merge. The
// /pull-request handler calls this to close it.
//
//	PATCH /repos/{owner}/{repo}/pulls/{number}
//	{ "state": "closed" }
//
// This is the same endpoint GitHub documents for updating a pull request
// ("Update a pull request"); setting state=closed closes it without
// merging. Mirrors the ForceUpdateRef PATCH pattern. Returns ErrNotFound
// when the repo/PR isn't visible to the installation, ErrForbidden on
// auth issues, ErrValidation when GitHub rejects the update. Closing a PR
// leaves its head branch intact.
func (c *Client) ClosePullRequest(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, number int) error {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	if c.Tokens == nil {
		return errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return errors.New("githubclient: repo owner and name required")
	}
	if number <= 0 {
		return errors.New("githubclient: pr number must be > 0")
	}

	raw, err := json.Marshal(map[string]string{"state": "closed"})
	if err != nil {
		return fmt.Errorf("githubclient: marshal close pr: %w", err)
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/pulls/" + url.PathEscape(fmt.Sprintf("%d", number)))
	req, err := c.buildRequest(ctx, http.MethodPatch, endpoint, bytes.NewReader(raw), installationID)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("githubclient: close pr: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return classifyStatus("close pr", resp)
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
func (c *Client) ListOpenPullRequestsByHead(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, headBranch, base string) ([]PullRequest, error) {
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
func (c *Client) ListTeamMembers(ctx context.Context, scope forge.CredentialScope, org, slug string) ([]TeamMember, error) {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
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
	// ID is the GitHub comment id, surfaced so the sticky-comment
	// orphan-rediscovery fallback (#1793) can match a hidden marker to the
	// comment that must be edited in place when the audit chain lost its id.
	ID        int64
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
func (c *Client) ListIssueComments(ctx context.Context, scope forge.CredentialScope, repo RepoRef, number int) ([]FetchedIssueComment, error) {
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
		ID   int64 `json:"id"`
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
			ID:        c.ID,
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
func (c *Client) DispatchWorkflow(ctx context.Context, scope forge.CredentialScope, repo RepoRef, workflowFile, ref string, inputs DispatchInputs) error {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
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
func (c *Client) GetWorkflowRun(ctx context.Context, scope forge.CredentialScope, repo RepoRef, runID int64) (*WorkflowRun, error) {
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

// ResolveDispatchedRun recovers the workflow_dispatch run a DispatchWorkflow
// just fired (#1386 / E23.6). GitHub's create-a-workflow-dispatch endpoint
// returns 204 with NO run id, so the run must be resolved after the fact by
// listing recent runs and matching the Fishhawk correlation token.
//
//	GET /repos/{owner}/{repo}/actions/runs?event=workflow_dispatch&branch={branch}&created=>={ts}
//
// correlation is the run_id+stage_id map the dispatch fired with. It is the
// PRIMARY, REQUIRED match (binding condition 1, #1386): a listed run whose
// echoed Inputs carry the exact correlation is an unambiguous hit. GitHub does
// NOT populate `inputs` on every run object, so when NO listed candidate echoes
// inputs the resolver FALLS BACK to the branch+created window — but returns a
// run ONLY when EXACTLY ONE candidate exists. Multiple concurrent
// workflow_dispatch runs on the same branch with no correlating inputs are
// AMBIGUOUS and resolve to (nil, nil): INDETERMINATE, never a guess, so a caller
// never associates a wrong external run with the deploy (and never records a
// wrong outcome). (nil, nil) is also returned when no run has appeared yet
// (GitHub's listing is eventually consistent) — the caller retries later.
//
// Returns a typed error (ErrNotFound / ErrForbidden / ErrValidation) only on a
// hard API failure; an empty/ambiguous result is (nil, nil), not an error.
func (c *Client) ResolveDispatchedRun(ctx context.Context, scope forge.CredentialScope,
	repo RepoRef, branch string, correlation map[string]string, createdAfter time.Time) (*WorkflowRun, error) {
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
	if len(correlation) == 0 {
		return nil, errors.New("githubclient: correlation token required")
	}

	q := url.Values{}
	q.Set("event", "workflow_dispatch")
	if branch != "" {
		q.Set("branch", branch)
	}
	if !createdAfter.IsZero() {
		// GitHub's `created` filter accepts a `>=ISO8601` value; url.Values
		// percent-encodes the `>=` prefix, which the API decodes back.
		q.Set("created", ">="+createdAfter.UTC().Format(time.RFC3339))
	}
	q.Set("per_page", "30")

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/actions/runs?" + q.Encode())

	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: resolve dispatched run: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("resolve dispatched run", resp); err != nil {
		return nil, err
	}

	var body struct {
		WorkflowRuns []struct {
			ID         int64             `json:"id"`
			HTMLURL    string            `json:"html_url"`
			Conclusion string            `json:"conclusion"`
			Status     string            `json:"status"`
			Event      string            `json:"event"`
			HeadBranch string            `json:"head_branch"`
			HeadSHA    string            `json:"head_sha"`
			Inputs     map[string]string `json:"inputs"`
		} `json:"workflow_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode workflow runs: %w", err)
	}

	var (
		correlated []*WorkflowRun
		candidates []*WorkflowRun
		anyInputs  bool
	)
	for i := range body.WorkflowRuns {
		raw := body.WorkflowRuns[i]
		// Defensive branch filter: the query already constrains by branch, but
		// a proxy/stub may echo the whole list. Skip runs on a different branch
		// (an empty head_branch is admitted — the API occasionally omits it).
		if branch != "" && raw.HeadBranch != "" && raw.HeadBranch != branch {
			continue
		}
		wr := &WorkflowRun{
			ID:         raw.ID,
			HTMLURL:    raw.HTMLURL,
			Conclusion: raw.Conclusion,
			Status:     raw.Status,
			Event:      raw.Event,
			HeadBranch: raw.HeadBranch,
			HeadSHA:    raw.HeadSHA,
			Inputs:     raw.Inputs,
		}
		candidates = append(candidates, wr)
		if len(raw.Inputs) > 0 {
			anyInputs = true
			if inputsMatchCorrelation(raw.Inputs, correlation) {
				correlated = append(correlated, wr)
			}
		}
	}

	// PRIMARY: an exact correlation-input match is unambiguous.
	if len(correlated) == 1 {
		return correlated[0], nil
	}
	if len(correlated) > 1 {
		// Two runs echoing the SAME run_id+stage_id should be impossible;
		// refuse to guess between them rather than associate a wrong run.
		return nil, nil
	}
	// No correlation hit. If ANY candidate carried inputs, this dispatch's run
	// is simply not among the listed runs yet — not found, retry later.
	if anyInputs {
		return nil, nil
	}
	// FALLBACK (echoed Inputs absent on every candidate): a SINGLE candidate on
	// the branch+created window is safe to associate. Multiple are AMBIGUOUS —
	// return INDETERMINATE rather than mis-associate (binding condition 1).
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	return nil, nil
}

// inputsMatchCorrelation reports whether every key/value in correlation is
// present and equal in inputs (the echoed workflow_dispatch inputs). Extra
// inputs keys are ignored — only the correlation token must match.
func inputsMatchCorrelation(inputs, correlation map[string]string) bool {
	for k, v := range correlation {
		if inputs[k] != v {
			return false
		}
	}
	return true
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
func (c *Client) CreateIssueComment(ctx context.Context, scope forge.CredentialScope, repo RepoRef, issueNumber int, body string) (*IssueComment, error) {
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
func (c *Client) UpdateIssueComment(ctx context.Context, scope forge.CredentialScope, repo RepoRef, commentID int64, body string) (*IssueComment, error) {
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
func (c *Client) CreateCheckRun(ctx context.Context, scope forge.CredentialScope, repo RepoRef, p CreateCheckRunParams) (*CreateCheckRunResult, error) {
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

// CreateReviewParams is the typed wire body for
// POST /repos/{owner}/{repo}/pulls/{pull_number}/reviews. Only the two
// fields Fishhawk sets are surfaced — Body (the review markdown) and Event
// (the review action). The GitHub schema is wider (commit_id, and a
// `comments` array for inline review comments); mapping file/line-located
// concerns to inline review comments is the deferred v2 (#1785).
type CreateReviewParams struct {
	Body  string
	Event string
}

// CreateReviewResult carries the bits of GitHub's create-review response
// Fishhawk records: the review id and its html_url.
type CreateReviewResult struct {
	ID      int64
	HTMLURL string
}

// CreateReview posts a pull-request review (E42.2 / #1785).
//
//	POST /repos/{owner}/{repo}/pulls/{pull_number}/reviews
//
// Fishhawk uses this ONLY to post advisory COMMENT-type reviews of an agent
// reviewer's verdict: a review with Event="COMMENT" and a body posts a
// non-blocking comment review. APPROVE / REQUEST_CHANGES are the
// branch-protection-relevant blocking events and are deliberately never sent
// from the agent-review surface (the COMMENT-type invariant is pinned by
// issuecomment.PRReviewEventComment and its test), so a bot review can never
// hard-block merge under branch protection.
//
// v2 deferral (#1785): mapping file/line-located concerns to inline review
// comments (this endpoint's `comments` array) is out of scope here — the body
// carries the full verdict as prose.
//
// Returns ErrValidation when GitHub rejects the request (422 — e.g. an
// invalid event), ErrNotFound when the repo/PR isn't visible to the
// installation, ErrForbidden when the installation token lacks
// `pull_requests:write`.
func (c *Client) CreateReview(ctx context.Context, scope forge.CredentialScope, repo RepoRef, prNumber int, params CreateReviewParams) (*CreateReviewResult, error) {
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
	if prNumber <= 0 {
		return nil, errors.New("githubclient: pr number must be > 0")
	}
	if params.Event == "" {
		return nil, errors.New("githubclient: review event required")
	}

	body := map[string]string{"event": params.Event}
	if params.Body != "" {
		body["body"] = params.Body
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("githubclient: marshal review body: %w", err)
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/pulls/" + url.PathEscape(fmt.Sprintf("%d", prNumber)) +
		"/reviews")

	req, err := c.buildRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(raw), installationID)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: create review: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("create review", resp); err != nil {
		return nil, err
	}

	var out struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("githubclient: decode review: %w", err)
	}
	return &CreateReviewResult{ID: out.ID, HTMLURL: out.HTMLURL}, nil
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
	// CreatedAt is the reaction's placement time, parsed from the
	// GitHub `created_at` field (RFC3339). The reaction-poller (#1054)
	// compares it against plan existence so a reaction placed before
	// the plan was generated is not admitted as a plan approval —
	// the anchor comment now exists from run creation (before any
	// plan), so the poller can no longer use the comment's posted-at
	// as the cutoff. Zero value when GitHub omits the field.
	CreatedAt time.Time `json:"created_at"`
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
func (c *Client) ListIssueCommentReactions(ctx context.Context, scope forge.CredentialScope, repo RepoRef, commentID int64) ([]IssueCommentReaction, error) {
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
//
// This is the SOLE request-construction path for every installation-scoped
// method, so it is also the single choke point where a per-installation REST
// API base override (Mode 2, E44.16 / #2094) is applied — see
// applyInstallationBaseURL. A resolver fault, a bad scheme, or a disallowed
// host FAILS CLOSED here, before the installation token is even minted, so no
// request ever ships to an unvalidated host.
func (c *Client) buildRequest(ctx context.Context, method, rawURL string, body io.Reader, installationID int64) (*http.Request, error) {
	// Resolve the per-installation base BEFORE minting the token so a
	// resolver/allowlist rejection fails closed without touching the token
	// provider or the wire.
	targetURL, err := c.applyInstallationBaseURL(ctx, rawURL, installationID)
	if err != nil {
		return nil, err
	}
	token, err := c.Tokens.Token(ctx, installationID)
	if err != nil {
		return nil, fmt.Errorf("githubclient: get token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, targetURL, body)
	if err != nil {
		return nil, fmt.Errorf("githubclient: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return req, nil
}

// applyInstallationBaseURL rewrites rawURL's scheme+host (and any base path) to
// the per-installation REST API base resolved by c.ResolveBaseURL, when one is
// configured and returns a non-empty override for installationID. It is the
// centralized Mode 2 hook (E44.16 / #2094): every installation-scoped method
// builds its URL via c.endpoint(...) and passes it here through buildRequest,
// so no per-method (codescanning/gitdata/projects) edit is needed.
//
// The rewrite is gated on rawURL being prefixed by the deployment REST API base
// (c.BaseURL, else DefaultBaseURL): release-asset uploads (uploadEndpoint →
// UploadBaseURL/DefaultUploadBaseURL) and any other-host URL are left
// byte-identical. Installation-scoped GraphQL issued via buildRequest targets
// the API base and IS rewritten; the static-token user-Projects GraphQL path
// uses buildStaticTokenRequest and never reaches here.
//
// Fail-closed contract (no request issued): a resolver error, an override that
// is not a well-formed absolute https URL (account.ValidateResolvedBaseURL), or
// — when AllowedInstallationHosts is non-empty — a host outside the allowlist
// (account.HostAllowed) each return an error. An empty override (nil resolver /
// NULL column / unknown installation) returns rawURL unchanged.
func (c *Client) applyInstallationBaseURL(ctx context.Context, rawURL string, installationID int64) (string, error) {
	if c.ResolveBaseURL == nil {
		return rawURL, nil
	}
	apiBase := c.BaseURL
	if apiBase == "" {
		apiBase = DefaultBaseURL
	}
	uploadBase := c.UploadBaseURL
	if uploadBase == "" {
		uploadBase = DefaultUploadBaseURL
	}
	// Only REST API-base requests are per-installation routable. An upload-host
	// request is excluded FIRST — so an upload base that shares a textual prefix
	// with the API base still wins the classification — and any other-host
	// request is left exactly as built. The match is host+path-boundary aware
	// (urlTargetsBase), NOT a bare strings.HasPrefix: a raw prefix test would
	// misclassify a look-alike host ("https://api.github.com.evil.example/…") or
	// a longer path segment as the API base and rewrite it to the resolved
	// installation host, shipping the installation token to a route this rewrite
	// is explicitly meant to leave untouched (#2094 fix-up, security).
	if urlTargetsBase(rawURL, uploadBase) || !urlTargetsBase(rawURL, apiBase) {
		return rawURL, nil
	}
	resolved, err := c.ResolveBaseURL(ctx, strconv.FormatInt(installationID, 10))
	if err != nil {
		return "", fmt.Errorf("githubclient: resolve installation base url: %w", err)
	}
	if resolved == "" {
		return rawURL, nil
	}
	// Validate BEFORE the token ships: an override that is not a well-formed
	// https URL fails closed rather than transmitting to an unvalidated host.
	if err := account.ValidateResolvedBaseURL(resolved); err != nil {
		return "", err
	}
	// Optional host allowlist: a configured allowlist pins the resolved host
	// before the token ships. Empty allowlist = scheme/parse validation only.
	if len(c.AllowedInstallationHosts) > 0 && !account.HostAllowed(resolved, c.AllowedInstallationHosts) {
		return "", fmt.Errorf("githubclient: installation base url host not in configured allowlist: %q", resolved)
	}
	// Swap the API base for the resolved base, preserving the path+query built
	// by endpoint(). A trailing slash on the resolved base is trimmed so a
	// resolved "https://acme.ghe.com/" and "https://acme.ghe.com" behave
	// identically; a resolved base carrying its own path prefix (GHES /api/v3)
	// is honored by appending the remainder.
	return strings.TrimSuffix(resolved, "/") + strings.TrimPrefix(rawURL, apiBase), nil
}

// urlTargetsBase reports whether rawURL targets base at a host+path-segment
// boundary: rawURL must equal base, or the character immediately after base
// must begin a path/query/fragment ('/', '?', or '#'). This is stricter than a
// bare strings.HasPrefix, which would treat a look-alike host that merely
// shares base as a textual prefix as targeting base — "https://api.github.com"
// is a string-prefix of "https://api.github.com.evil.example", but the latter
// is a DIFFERENT host and must never be rewritten to the resolved installation
// host. base is expected to carry no trailing slash (endpoint()/uploadEndpoint()
// build "<base><path>" with path starting at '/'), so the boundary check both
// pins the host and prevents a longer path segment from matching a shorter base
// path prefix.
func urlTargetsBase(rawURL, base string) bool {
	if !strings.HasPrefix(rawURL, base) {
		return false
	}
	rest := rawURL[len(base):]
	return rest == "" || rest[0] == '/' || rest[0] == '?' || rest[0] == '#'
}

// buildStaticTokenRequest constructs an http.Request authenticated with a
// caller-supplied static token (the projects PAT/UAT — Client.ProjectsToken)
// instead of resolving an installation token. Used only by doGraphQL for
// user-owned Projects v2 boards, which installation tokens cannot reach
// (#1114). Header shape is identical to buildRequest so only the token value
// differs. The token value is never logged or surfaced in errors.
func (*Client) buildStaticTokenRequest(ctx context.Context, method, url string, body io.Reader, token string) (*http.Request, error) {
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

// uploadEndpoint returns UploadBaseURL + path, defaulting to
// uploads.github.com. Release-asset uploads use this host, not endpoint's
// api.github.com root.
func (c *Client) uploadEndpoint(path string) string {
	base := c.UploadBaseURL
	if base == "" {
		base = DefaultUploadBaseURL
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
