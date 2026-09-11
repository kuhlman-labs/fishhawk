// Package stub is the in-process stub forge for the acceptance preview
// (E72.3 / #3327). It promotes the stateful GitHub and GitLab test fakes
// that backend/internal/server/split_parent_close_test.go grew for the
// E50.6 parent-close watcher into a package the preview's own fishhawkd
// can mount under --dev-stub-forge, so a sandboxed acceptance agent that
// reaches only localhost:8090 can seed forge state, deliver a signed
// webhook, and read the committed result back — without a real forge and
// without a second port.
//
// The stub serves the GitHub and GitLab REST subsets the product actually
// calls (see GitHubHandler / GitLabHandler) from ONE mutex-guarded Forge
// value. State is held in each forge's NATIVE vocabulary — GitHub
// "open"/"closed" plus state_reason, GitLab "opened"/"closed" — so the real
// adapters' normalization is exercised, not bypassed. Every request is
// recorded in arrival ORDER (the watcher's comment-before-close invariant
// is an ordering property), and a per-operation fault (SetFault) makes one
// endpoint answer 500 until cleared, so each error branch of a consumer can
// be exercised in isolation and then proven transient.
//
// NEVER IN PRODUCTION. Nothing here is reachable unless fishhawkd is
// started with --dev-stub-forge, which refuses to coexist with a configured
// GitHub App or GitLab token; the credentials below are fixed, public dev
// constants. Long-form contract: README.md in this directory.
package stub

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Forge family discriminators, matching the values the webhook receivers
// and the split_children_filed audit payload use.
const (
	ForgeGitHub = "github"
	ForgeGitLab = "gitlab"
)

// Fixed dev credentials and hosts. The base URLs use the reserved
// `.invalid` TLD (RFC 2606) so a request that escapes the in-process
// transport can never resolve on the network.
const (
	// GitHubWebhookSecret is the HMAC secret the preview's GitHub receiver
	// is configured with when the operator supplies none.
	GitHubWebhookSecret = "fishhawk-stub-forge-github-webhook-secret"
	// GitLabWebhookToken is the X-Gitlab-Token the preview's GitLab receiver
	// is configured with when the operator supplies none.
	GitLabWebhookToken = "fishhawk-stub-forge-gitlab-webhook-token"
	// InstallationToken is the bearer/PRIVATE-TOKEN value StaticTokens hands
	// the clients and the access_tokens endpoint mints.
	InstallationToken = "fishhawk-stub-forge-installation-token"
	// GitHubBaseURL is the API root cfg.GitHub is pointed at.
	GitHubBaseURL = "http://stub-github.invalid"
	// GitLabBaseURL is the instance root the gitlab forge is pointed at; the
	// notes-pagination Link header is rooted here because the real adapter
	// follows a next link only on its own scheme+host.
	GitLabBaseURL = "http://stub-gitlab.invalid"
)

// Operation names: the keys SetFault takes and the first token of every
// request-log entry.
const (
	OpGitHubAccessToken  = "github.access_token"
	OpGitHubGetRepo      = "github.get_repo"
	OpGitHubGetIssue     = "github.get_issue"
	OpGitHubPatchIssue   = "github.patch_issue"
	OpGitHubListComments = "github.list_comments"
	OpGitHubPostComment  = "github.post_comment"
	OpGitHubGetPull      = "github.get_pull"

	OpGitLabGetProject      = "gitlab.get_project"
	OpGitLabGetIssue        = "gitlab.get_issue"
	OpGitLabPutIssue        = "gitlab.put_issue"
	OpGitLabListNotes       = "gitlab.list_notes"
	OpGitLabPostNote        = "gitlab.post_note"
	OpGitLabGetMergeRequest = "gitlab.get_merge_request"
)

// ErrInvalid is returned (wrapped) by SeedIssue / SeedPullRequest for a
// record the stub cannot hold: an unknown forge, a non-positive number, or
// a GitLab record without a project id.
var ErrInvalid = errors.New("stub: invalid seed")

// Issue is one issue in the stub, in its forge's NATIVE state vocabulary.
// Repo is the GitHub "owner/name" full name, or for GitLab the optional
// namespaced project path registered for path lookups; ProjectID is the
// GitLab numeric project id and is zero for GitHub. Comments are in
// arrival order.
type Issue struct {
	Forge       string   `json:"forge"`
	Repo        string   `json:"repo,omitempty"`
	ProjectID   int      `json:"project_id,omitempty"`
	Number      int      `json:"number"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	State       string   `json:"state"`
	StateReason string   `json:"state_reason,omitempty"`
	Comments    []string `json:"comments"`
}

// PullRequest is one pull request (GitHub) or merge request (GitLab). State
// is native: GitHub "open"/"closed" with Merged set separately; GitLab
// "opened"/"closed"/"merged".
type PullRequest struct {
	Forge          string     `json:"forge"`
	Repo           string     `json:"repo,omitempty"`
	ProjectID      int        `json:"project_id,omitempty"`
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	State          string     `json:"state"`
	Merged         bool       `json:"merged"`
	MergeCommitSHA string     `json:"merge_commit_sha,omitempty"`
	MergedAt       *time.Time `json:"merged_at,omitempty"`
	HeadSHA        string     `json:"head_sha,omitempty"`
	HeadRef        string     `json:"head_ref,omitempty"`
	BaseRef        string     `json:"base_ref,omitempty"`
}

// Snapshot is the JSON-shaped read of the whole stub GET /v0/dev/forge
// returns: every record per family, sorted by key, plus the request log.
type Snapshot struct {
	GitHub   FamilySnapshot `json:"github"`
	GitLab   FamilySnapshot `json:"gitlab"`
	Requests []string       `json:"requests"`
}

// FamilySnapshot is one forge family's records. Pulls carries GitHub pull
// requests; MergeRequests carries GitLab merge requests — the two are the
// same Go type under the family's own name.
type FamilySnapshot struct {
	Issues        []Issue       `json:"issues"`
	Pulls         []PullRequest `json:"pulls,omitempty"`
	MergeRequests []PullRequest `json:"merge_requests,omitempty"`
}

// Forge is the mutex-guarded stub state. The zero value is not usable;
// construct with New. It is safe for concurrent use — the receivers and
// the control API reach it from different goroutines.
type Forge struct {
	mu       sync.Mutex
	issues   map[string]*Issue
	pulls    map[string]*PullRequest
	projects map[string]int // GitLab namespaced path -> project id
	requests []string
	faults   map[string]bool
	// notesPageSize, when > 0, overrides the per_page the GitLab notes
	// list honours, so a test can force pagination with a small thread.
	notesPageSize int
}

// New returns an empty stub.
func New() *Forge {
	f := &Forge{}
	f.resetLocked()
	return f
}

func (f *Forge) resetLocked() {
	f.issues = map[string]*Issue{}
	f.pulls = map[string]*PullRequest{}
	f.projects = map[string]int{}
	f.requests = nil
	f.faults = map[string]bool{}
}

// Reset drops every record, the request log, every fault and the notes
// page-size override.
func (f *Forge) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetLocked()
	f.notesPageSize = 0
}

// recordKey addresses one issue or pull request: "github:<owner/name>#<n>"
// or "gitlab:<project-id>#<n>". Two repositories may hold the same number.
func recordKey(family, repo string, projectID, number int) string {
	if family == ForgeGitLab {
		return family + ":" + strconv.Itoa(projectID) + "#" + strconv.Itoa(number)
	}
	return family + ":" + repo + "#" + strconv.Itoa(number)
}

// validateSeed checks the family/addressing fields shared by issues and
// pull requests.
func validateSeed(family, repo string, projectID, number int) error {
	switch family {
	case ForgeGitHub:
		if repo == "" {
			return fmt.Errorf("%w: github record requires repo (owner/name)", ErrInvalid)
		}
	case ForgeGitLab:
		if projectID <= 0 {
			return fmt.Errorf("%w: gitlab record requires a positive project_id", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown forge %q (want %q or %q)", ErrInvalid, family, ForgeGitHub, ForgeGitLab)
	}
	if number <= 0 {
		return fmt.Errorf("%w: number must be positive, got %d", ErrInvalid, number)
	}
	return nil
}

// SeedIssue stores is, replacing any record at the same address, and
// returns the stored copy. An empty State defaults to the family's native
// open word ("open" for GitHub, "opened" for GitLab). A GitLab issue with
// a non-empty Repo registers that namespaced path for project lookups.
func (f *Forge) SeedIssue(is Issue) (Issue, error) {
	if err := validateSeed(is.Forge, is.Repo, is.ProjectID, is.Number); err != nil {
		return Issue{}, err
	}
	if is.State == "" {
		is.State = nativeOpen(is.Forge)
	}
	if is.Forge == ForgeGitHub {
		is.ProjectID = 0
	}
	is.Comments = append([]string{}, is.Comments...)
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := is
	f.issues[recordKey(is.Forge, is.Repo, is.ProjectID, is.Number)] = &stored
	if is.Forge == ForgeGitLab && is.Repo != "" {
		f.projects[is.Repo] = is.ProjectID
	}
	return copyIssue(&stored), nil
}

// GetIssue reads one issue by address; ok is false when none is seeded.
// For GitHub, repo addresses the record; for GitLab, projectID does.
func (f *Forge) GetIssue(family, repo string, projectID, number int) (Issue, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is, ok := f.issues[recordKey(family, repo, projectID, number)]
	if !ok {
		return Issue{}, false
	}
	return copyIssue(is), true
}

// SeedPullRequest stores pr, replacing any record at the same address, and
// returns the stored copy. An empty State defaults to the family's native
// open word; a GitLab record with Merged set is stored as state "merged"
// (GitLab's lifecycle word). A GitLab pull request with a non-empty Repo
// registers that namespaced path for project lookups.
func (f *Forge) SeedPullRequest(pr PullRequest) (PullRequest, error) {
	if err := validateSeed(pr.Forge, pr.Repo, pr.ProjectID, pr.Number); err != nil {
		return PullRequest{}, err
	}
	if pr.State == "" {
		pr.State = nativeOpen(pr.Forge)
	}
	if pr.Forge == ForgeGitHub {
		pr.ProjectID = 0
	} else if pr.Merged {
		pr.State = "merged"
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := pr
	f.pulls[recordKey(pr.Forge, pr.Repo, pr.ProjectID, pr.Number)] = &stored
	if pr.Forge == ForgeGitLab && pr.Repo != "" {
		f.projects[pr.Repo] = pr.ProjectID
	}
	return stored, nil
}

// GetPullRequest reads one pull/merge request by address.
func (f *Forge) GetPullRequest(family, repo string, projectID, number int) (PullRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, ok := f.pulls[recordKey(family, repo, projectID, number)]
	if !ok {
		return PullRequest{}, false
	}
	return *pr, true
}

// Requests returns every request served so far, in arrival order, as
// "<op> <key>" strings (e.g. "github.post_comment github:o/r#100").
func (f *Forge) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// SetFault makes op answer HTTP 500 (on=true) until cleared (on=false).
// op is one of the Op* constants; an unknown op is stored but never
// consulted.
func (f *Forge) SetFault(op string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if on {
		f.faults[op] = true
		return
	}
	delete(f.faults, op)
}

// SetNotesPageSize forces the GitLab notes list to page with n entries per
// page regardless of the per_page the client asks for; 0 restores the
// client's per_page. It exists so a test can drive the adapter's
// page-to-exhaustion walk with a short thread.
func (f *Forge) SetNotesPageSize(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notesPageSize = n
}

// Snapshot returns the whole stub, records sorted by key.
func (f *Forge) Snapshot() Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	var snap Snapshot
	for _, k := range sortedKeys(f.issues) {
		is := copyIssue(f.issues[k])
		if is.Forge == ForgeGitLab {
			snap.GitLab.Issues = append(snap.GitLab.Issues, is)
		} else {
			snap.GitHub.Issues = append(snap.GitHub.Issues, is)
		}
	}
	for _, k := range sortedKeys(f.pulls) {
		pr := *f.pulls[k]
		if pr.Forge == ForgeGitLab {
			snap.GitLab.MergeRequests = append(snap.GitLab.MergeRequests, pr)
		} else {
			snap.GitHub.Pulls = append(snap.GitHub.Pulls, pr)
		}
	}
	if snap.GitHub.Issues == nil {
		snap.GitHub.Issues = []Issue{}
	}
	if snap.GitLab.Issues == nil {
		snap.GitLab.Issues = []Issue{}
	}
	snap.Requests = append([]string{}, f.requests...)
	return snap
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func copyIssue(is *Issue) Issue {
	out := *is
	out.Comments = append([]string{}, is.Comments...)
	return out
}

func nativeOpen(family string) string {
	if family == ForgeGitLab {
		return "opened"
	}
	return "open"
}

// --- handler-side helpers (called with f.mu HELD) -------------------------

// record appends one request-log entry.
func (f *Forge) recordLocked(op, key string) {
	f.requests = append(f.requests, op+" "+key)
}

// faulted reports whether op is currently set to fail.
func (f *Forge) faultedLocked(op string) bool {
	return f.faults[op]
}
