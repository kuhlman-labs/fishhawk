// Package onboarding generates and opens the App-PR onboarding scaffold
// (ADR-048 / E29.7). When the Fishhawk GitHub App is installed on a repo
// — or repos are added to an existing installation — the backend opens a
// reviewable pull request that seeds the four files a repo needs to run
// through Fishhawk:
//
//   - .fishhawk/workflows.yaml — the workflow spec (default: the "medium"
//     autonomy preset).
//   - AGENTS.md — the canonical, cross-agent instruction file, carrying
//     Fishhawk's marker-delimited managed block.
//   - CLAUDE.md — a bridge file importing AGENTS.md so Claude Code picks up
//     the canonical instructions.
//   - .github/workflows/fishhawk.yml — the customer-side execution workflow
//     the backend fires workflow_dispatch against, pinning the PUBLISHED
//     runner + auth actions.
//
// The human is the author/reviewer of record: the scaffold lands as a PR
// they review and merge, never a direct push to the default branch
// (autonomy:low). The commit is authored server-side through the GitHub
// Git Data API (create-tree with inline content → create-commit →
// create-ref → open PR), so all four files land in one atomic commit with
// no working tree.
//
// The scaffold content reuses the same sources as `fishhawk init`
// (spec.PresetBytes + bridge.ManagedBlock/ImportLine), so a repo onboarded
// via the App and a repo onboarded via the CLI get byte-identical files.
package onboarding

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sort"

	"github.com/kuhlman-labs/fishhawk/backend/internal/bridge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// OnboardingBranch is the branch the scaffold commit lands on. The App
// opens a PR from this branch into the repo's default branch. Reused
// across attempts: a re-run force-updates it to the fresh scaffold commit
// (see OpenScaffoldPR) rather than creating a divergent branch.
const OnboardingBranch = "fishhawk/onboarding"

// Scaffold file paths (repo-relative).
const (
	specPath     = githubclient.WorkflowSpecPath // ".fishhawk/workflows.yaml"
	agentsPath   = "AGENTS.md"
	claudePath   = "CLAUDE.md"
	workflowPath = ".github/workflows/fishhawk.yml"
)

// commitMessage / prTitle / prBody are the scaffold PR's copy. Restrained,
// technical voice per BRAND_FOUNDATIONS §5.
const (
	commitMessage = "Add Fishhawk onboarding scaffold"
	prTitle       = "Add Fishhawk onboarding scaffold"
	prBody        = `Fishhawk opened this pull request when the app was installed on this repository.

It seeds the files a repository needs to run changes through Fishhawk:

- ` + "`.fishhawk/workflows.yaml`" + ` — the workflow spec (default: the medium autonomy preset). Change the tier before merging if you want more or less automation.
- ` + "`AGENTS.md`" + ` — the canonical, cross-agent instruction file. Fishhawk owns the marker-delimited block; edit the text outside the markers.
- ` + "`CLAUDE.md`" + ` — imports AGENTS.md so Claude Code picks up the same instructions.
- ` + "`.github/workflows/fishhawk.yml`" + ` — the GitHub Actions workflow Fishhawk dispatches to run a stage. It references the published ` + "`kuhlman-labs/fishhawk/runner`" + ` action.

Review the files, adjust the autonomy tier if needed, and merge to finish onboarding.
`
)

//go:embed templates/fishhawk.yml
var workflowTemplate []byte

// ScaffoldFiles assembles the scaffold file set for a preset. It is pure —
// no I/O — so it can be unit-tested for content correctness independently
// of the GitHub write path. The returned map is keyed by repo-relative
// path. Returns an error only for an unknown preset (from spec.PresetBytes).
func ScaffoldFiles(preset spec.Preset) (map[string][]byte, error) {
	specBytes, err := spec.PresetBytes(preset)
	if err != nil {
		return nil, fmt.Errorf("onboarding: preset bytes: %w", err)
	}
	claude := "# CLAUDE.md\n\n" + bridge.ImportLine + "\n"
	return map[string][]byte{
		specPath:     specBytes,
		agentsPath:   []byte(bridge.ManagedBlock()),
		claudePath:   []byte(claude),
		workflowPath: workflowTemplate,
	}, nil
}

// githubClient is the narrow slice of *githubclient.Client the scaffolder
// drives. Declaring it as an interface lets tests substitute a fake that
// records the call sequence without an httptest server.
type githubClient interface {
	GetFileScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, path, ref string) (*githubclient.FileContent, error)
	GetRepositoryScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef) (*githubclient.Repository, error)
	GetBranchSHAScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, branch string) (string, bool, error)
	GetCommitScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, sha string) (*githubclient.GitCommit, error)
	CreateTreeScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, baseTree string, entries []githubclient.TreeEntry) (string, error)
	CreateCommitScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, message, treeSHA string, parents []string) (string, error)
	CreateRefScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, branch, sha string) error
	ForceUpdateRefScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, branch, newSHA string) error
	CreatePullRequestScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, head, base, title, body string) (*githubclient.PullRequest, error)
}

// Scaffolder opens the onboarding scaffold PR through a githubClient. The
// zero value is not usable; construct via NewScaffolder.
type Scaffolder struct {
	client githubClient
	// Preset is the autonomy tier the scaffolded .fishhawk/workflows.yaml
	// seeds. Defaults to spec.PresetMedium (see NewScaffolder), matching
	// `fishhawk init`.
	Preset spec.Preset
}

// NewScaffolder returns a Scaffolder driving client (a *githubclient.Client
// in production). The default preset is medium — the operator reviews and
// can change the tier in the scaffold PR before merge.
func NewScaffolder(client githubClient) *Scaffolder {
	return &Scaffolder{client: client, Preset: spec.PresetMedium}
}

// Result reports the outcome of OpenScaffoldPR so the caller can log/audit
// what happened per repo.
type Result struct {
	// Skipped is true when the repo was already onboarded (a
	// .fishhawk/workflows.yaml already exists) and no PR was opened.
	Skipped bool
	// Reason explains a Skipped result.
	Reason string
	// PullRequestURL is the opened (or pre-existing) scaffold PR's URL.
	// Empty on a skip.
	PullRequestURL string
	// PRAlreadyExisted is true when a scaffold PR was already open for the
	// branch (CreatePullRequest returned ErrPullRequestExists) — an
	// idempotent success, not a new PR.
	PRAlreadyExisted bool
	// RefForceUpdated is true when the onboarding branch already existed
	// and was force-updated to the fresh scaffold commit rather than
	// created (the binding-condition path — a prior attempt left a stale
	// branch).
	RefForceUpdated bool
}

// OpenScaffoldPR opens (or refreshes) the onboarding scaffold PR for repo,
// idempotently.
//
// Sequence:
//
//  1. Idempotency: if .fishhawk/workflows.yaml already exists on the
//     default branch, the repo is onboarded — return skipped, no writes.
//  2. Resolve the default branch (the PR base) and its HEAD commit's tree.
//  3. Build the scaffold tree on top of that base tree, commit it.
//  4. Point the onboarding branch at the new commit: create it, OR — when a
//     prior attempt already created it — FORCE-UPDATE it to the fresh
//     commit so the PR reflects the freshly-generated scaffold, not stale
//     branch contents.
//  5. Open the PR. ErrPullRequestExists is an idempotent success.
//
// A pre-commit read failure (ErrForbidden / ErrNotFound on GetFile /
// GetRepository / GetBranchSHA) surfaces as an error with no partial
// branch created.
func (s *Scaffolder) OpenScaffoldPR(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef) (*Result, error) {
	// Step 1: already-onboarded skip. ref="" reads the default branch.
	existing, err := s.client.GetFileScoped(ctx, scope, repo, specPath, "")
	if err != nil && !errors.Is(err, githubclient.ErrNotFound) {
		return nil, fmt.Errorf("onboarding: probe existing spec: %w", err)
	}
	if err == nil && existing != nil {
		return &Result{Skipped: true, Reason: "already onboarded: " + specPath + " exists"}, nil
	}

	// Step 2: resolve default branch + its HEAD commit's tree.
	repoInfo, err := s.client.GetRepositoryScoped(ctx, scope, repo)
	if err != nil {
		return nil, fmt.Errorf("onboarding: get repository: %w", err)
	}
	base := repoInfo.DefaultBranch

	baseCommitSHA, exists, err := s.client.GetBranchSHAScoped(ctx, scope, repo, base)
	if err != nil {
		return nil, fmt.Errorf("onboarding: get base branch sha: %w", err)
	}
	if !exists {
		// An empty repo (no default-branch commit) has no tree to build on.
		// Skip rather than error — nothing to scaffold yet.
		return &Result{Skipped: true, Reason: "default branch " + base + " has no commit"}, nil
	}
	baseCommit, err := s.client.GetCommitScoped(ctx, scope, repo, baseCommitSHA)
	if err != nil {
		return nil, fmt.Errorf("onboarding: get base commit: %w", err)
	}

	// Step 3: build the scaffold tree + commit.
	files, err := ScaffoldFiles(s.presetOrDefault())
	if err != nil {
		return nil, fmt.Errorf("onboarding: scaffold files: %w", err)
	}
	entries := treeEntries(files)
	treeSHA, err := s.client.CreateTreeScoped(ctx, scope, repo, baseCommit.TreeSHA, entries)
	if err != nil {
		return nil, fmt.Errorf("onboarding: create tree: %w", err)
	}
	commitSHA, err := s.client.CreateCommitScoped(ctx, scope, repo, commitMessage, treeSHA, []string{baseCommitSHA})
	if err != nil {
		return nil, fmt.Errorf("onboarding: create commit: %w", err)
	}

	// Step 4: point the onboarding branch at the new commit. A prior
	// attempt may have created the branch but failed before/without
	// merging — force-update it to the fresh commit so the PR reflects the
	// freshly-generated scaffold rather than stale contents (binding
	// condition; ADR-048 / gpt-5.5 MEDIUM). The GetBranchSHA probe
	// distinguishes create-vs-update up front, which the underlying
	// CreateRef (already-exists → benign no-op) cannot.
	result := &Result{}
	_, branchExists, err := s.client.GetBranchSHAScoped(ctx, scope, repo, OnboardingBranch)
	if err != nil {
		return nil, fmt.Errorf("onboarding: probe onboarding branch: %w", err)
	}
	if branchExists {
		if err := s.client.ForceUpdateRefScoped(ctx, scope, repo, OnboardingBranch, commitSHA); err != nil {
			return nil, fmt.Errorf("onboarding: force-update onboarding ref: %w", err)
		}
		result.RefForceUpdated = true
	} else if err := s.client.CreateRefScoped(ctx, scope, repo, OnboardingBranch, commitSHA); err != nil {
		return nil, fmt.Errorf("onboarding: create onboarding ref: %w", err)
	}

	// Step 5: open the PR. A pre-existing PR for the same head/base is an
	// idempotent success (the branch we just refreshed already has an open
	// PR pointing at it).
	pr, err := s.client.CreatePullRequestScoped(ctx, scope, repo, OnboardingBranch, base, prTitle, prBody)
	if err != nil {
		if errors.Is(err, githubclient.ErrPullRequestExists) {
			result.PRAlreadyExisted = true
			return result, nil
		}
		return nil, fmt.Errorf("onboarding: create pull request: %w", err)
	}
	result.PullRequestURL = pr.HTMLURL
	return result, nil
}

// presetOrDefault returns the configured preset, falling back to medium
// when the zero value slipped through (e.g. a hand-constructed Scaffolder).
func (s *Scaffolder) presetOrDefault() spec.Preset {
	if s.Preset == "" {
		return spec.PresetMedium
	}
	return s.Preset
}

// treeEntries turns the scaffold file map into a path-sorted TreeEntry
// slice. Sorting makes the create-tree request deterministic (stable
// across runs and easy to assert in tests).
func treeEntries(files map[string][]byte) []githubclient.TreeEntry {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	entries := make([]githubclient.TreeEntry, 0, len(paths))
	for _, p := range paths {
		entries = append(entries, githubclient.TreeEntry{Path: p, Content: string(files[p])})
	}
	return entries
}
