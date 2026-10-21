package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ForgeMerger is the forge-RESOLVED merge seam bound at every GitHubMerger site
// (E45.47 / #3464). It satisfies GitHubMerger and routes a run's merge by its
// forge FAMILY, using the SAME per-family ladder prStateReaderFor / issueOpsFor
// codify:
//
//   - a github-family run (observationForgeID(InstallationRef) == "github",
//     which includes the nil / bare-decimal pre-0076 shape) delegates
//     byte-for-byte to GitHub — serve.go's githubAutoMerger leaf. It NEVER
//     touches Resolver, so registry availability can never change a GitHub
//     outcome. The GitHub seam is an INTERFACE: serve.go assigns a nil interface
//     (not a githubAutoMerger wrapping a nil client) when no client is wired, so
//     the nil check below is a genuine "no merge client" refusal, not a panic.
//   - any other family resolves the target through resolveObservationTarget (so
//     a github-shaped URL on a gitlab ref, a malformed URL, or a repo/URL
//     mismatch refuses BEFORE any forge call and an unimplemented forge never
//     reaches a merge) and the forge through Resolver (defaulting to forge.Get,
//     isNilForge-guarded against a typed-nil), then queues EnableAutoMerge with
//     the squash method and the SAME ErrPullRequestCleanStatus -> MergePullRequest
//     fallback ladder githubAutoMerger uses.
//
// The clean-status fallback is uniform across families deliberately: the GitLab
// adapter never emits forge.ErrPullRequestCleanStatus (a merge-ready MR merges
// synchronously on the EnableAutoMerge call, EnableAutoMerge's doc-comment in
// forge/gitlab), so on GitLab the fallback arm is simply never taken; keeping
// one ladder avoids a family-specific merge path.
//
// ForgeMerger also implements the optional HeadPinnedMerger (ADR-093 D3, E87.5 /
// #4296) over the same family ladder and target resolution (resolveNonGitHub):
// the merge queue's merge-on-pass merges a verified head through it, pinned and
// synchronous, never through the auto-merge arm above.
//
// Both merge entry points — POST /v0/runs/{id}/merge (merge_run.go) and the
// delegated may_merge arm (dispatchAcceptanceGatedMerge, autodrive.go) — flow
// through this seam unchanged, so a GitLab run reaches the same merge the
// GitHub-only bindings previously refused it.
type ForgeMerger struct {
	// GitHub is the github-family merge leaf (serve.go's githubAutoMerger). A
	// nil interface here means "no github merge client wired": a github-family
	// merge then fails closed rather than dispatching a nil.
	GitHub GitHubMerger
	// Resolver resolves a NON-github forge by family id. Nil defaults to
	// forge.Get, so production needs no wiring — serve.go registers the forges
	// at startup and the lookup is late-bound per merge call.
	Resolver func(id string) (forge.Forge, error)
}

var _ GitHubMerger = ForgeMerger{}

// HeadPinnedMerger is the OPTIONAL server-side head-pinned merge seam the merge
// queue's merge-on-pass consumes (ADR-093 D3, E87.5 / #4296): merge the run's
// PR SYNCHRONOUSLY only if its head still equals headSHA, the head the queue
// verified. A consumer type-asserts its GateMerger to it; a GateMerger that
// does not implement it means "no pinned merge available", and merge-on-pass
// holds rather than falling back to an unpinned or queued (auto-)merge.
//
// Contract: an empty headSHA is an error before any forge call; a moved head
// surfaces as forge.ErrMergeConflict (GitHub REST 409 / GitLab 409); a PR not
// in a mergeable state as forge.ErrPullRequestNotMergeable. It NEVER queues
// auto-merge: a queued merge waits on checks while the base may move.
type HeadPinnedMerger interface {
	MergePullRequestAtHead(ctx context.Context, runRow *run.Run, headSHA string) error
}

var _ HeadPinnedMerger = ForgeMerger{}

// ErrHeadPinnedMergeUnsupported is returned by ForgeMerger.MergePullRequestAtHead
// when the run's resolved merge leaf (the github leaf, or the resolved forge)
// has no head-pinned merge. It is a refusal, never a fallback to an unpinned
// merge.
var ErrHeadPinnedMergeUnsupported = errors.New("forge merge: head-pinned merge not supported by this forge")

// headPinnedMergeCallTimeout bounds ONE pinned merge forge call. Merge-on-pass
// runs the call inside the per-run merge-intent lock fn (90s deadline), so the
// call is capped well under that deadline: a hung forge cannot consume the
// lock's whole budget (ADR-093 slice 2 approval condition (f)).
const headPinnedMergeCallTimeout = 30 * time.Second

// MergePullRequestAtHead routes a head-pinned merge by forge family with the
// same ladder MergePullRequest uses: the github family delegates to the github
// leaf when IT implements HeadPinnedMerger (serve.go's githubAutoMerger: a
// synchronous REST merge carrying sha), and every other family resolves the
// forge and requires forge.HeadPinnedMerger. Either missing capability is
// ErrHeadPinnedMergeUnsupported. The forge call runs under
// headPinnedMergeCallTimeout.
func (m ForgeMerger) MergePullRequestAtHead(ctx context.Context, runRow *run.Run, headSHA string) error {
	if strings.TrimSpace(headSHA) == "" {
		return fmt.Errorf("forge merge: run %s: head-pinned merge needs a head sha", runRow.ID)
	}
	family := observationForgeID(runRow.InstallationRef)
	if family == observationForgeGitHub {
		if m.GitHub == nil {
			return fmt.Errorf("forge merge: run %s: no github merge client configured", runRow.ID)
		}
		pinned, ok := m.GitHub.(HeadPinnedMerger)
		if !ok {
			return fmt.Errorf("%w: run %s: github merge leaf", ErrHeadPinnedMergeUnsupported, runRow.ID)
		}
		cctx, cancel := context.WithTimeout(ctx, headPinnedMergeCallTimeout)
		defer cancel()
		return pinned.MergePullRequestAtHead(cctx, runRow, headSHA)
	}

	f, scope, repo, number, err := m.resolveNonGitHub(runRow)
	if err != nil {
		return err
	}
	pinned, ok := f.(forge.HeadPinnedMerger)
	if !ok {
		return fmt.Errorf("%w: run %s: forge %q", ErrHeadPinnedMergeUnsupported, runRow.ID, f.Name())
	}
	cctx, cancel := context.WithTimeout(ctx, headPinnedMergeCallTimeout)
	defer cancel()
	return pinned.MergePullRequestAtHead(cctx, scope, repo, number, forge.MergeMethodSquash, headSHA)
}

// MergePullRequest routes the run's merge by forge family per the type doc.
func (m ForgeMerger) MergePullRequest(ctx context.Context, runRow *run.Run) error {
	family := observationForgeID(runRow.InstallationRef)
	if family == observationForgeGitHub {
		if m.GitHub == nil {
			return fmt.Errorf("forge merge: run %s: no github merge client configured", runRow.ID)
		}
		return m.GitHub.MergePullRequest(ctx, runRow)
	}

	f, scope, repo, number, err := m.resolveNonGitHub(runRow)
	if err != nil {
		return err
	}
	err = f.EnableAutoMerge(ctx, scope, repo, number, forge.MergeMethodSquash)
	if err == nil {
		return nil
	}
	// Uniform clean-status fallback (see the type doc): only ever taken on
	// GitHub, which the github family never reaches here — kept so the ladder is
	// one shape across families. The gitlab adapter never emits this sentinel.
	if errors.Is(err, forge.ErrPullRequestCleanStatus) {
		return f.MergePullRequest(ctx, scope, repo, number, forge.MergeMethodSquash)
	}
	return err
}

// resolveNonGitHub resolves a NON-github-family run's merge target and forge:
// the installation-ref guard, resolveObservationTarget (so a github-shaped URL
// on a gitlab ref, a malformed URL, or a repo/URL mismatch refuses BEFORE any
// forge call), then the resolver (forge.Get by default, isNilForge-guarded).
// Shared by MergePullRequest and MergePullRequestAtHead so the two merge arms
// can never diverge on target resolution.
func (m ForgeMerger) resolveNonGitHub(runRow *run.Run) (forge.Forge, forge.CredentialScope, forge.RepoRef, int, error) {
	// Non-github family. The ref is non-empty by construction (a non-github
	// family id only comes from a "<forge>:<id>" InstallationRef), but guard it
	// explicitly so a caller cannot reach the resolver with an empty scope.
	if runRow.InstallationRef == nil || *runRow.InstallationRef == "" {
		return nil, forge.CredentialScope{}, forge.RepoRef{}, 0, fmt.Errorf("forge merge: run %s has no installation ref", runRow.ID)
	}
	ref := *runRow.InstallationRef

	forgeID, repo, number, reason := resolveObservationTarget(runRow)
	switch reason {
	case obsTargetMalformed:
		return nil, forge.CredentialScope{}, forge.RepoRef{}, 0, fmt.Errorf("forge merge: run %s: could not resolve repository and pull request number from %q",
			runRow.ID, prURLOf(runRow))
	case obsTargetMismatch:
		return nil, forge.CredentialScope{}, forge.RepoRef{}, 0, fmt.Errorf("forge merge: run %s: recorded pull request url %q does not name this run's repository on forge family %q",
			runRow.ID, prURLOf(runRow), forgeID)
	}

	resolver := m.Resolver
	if resolver == nil {
		resolver = forge.Get
	}
	f, err := resolver(forgeID)
	if err != nil {
		return nil, forge.CredentialScope{}, forge.RepoRef{}, 0, fmt.Errorf("forge merge: run %s: resolve forge %q: %w", runRow.ID, forgeID, err)
	}
	if isNilForge(f) {
		return nil, forge.CredentialScope{}, forge.RepoRef{}, 0, fmt.Errorf("forge merge: run %s: forge %q not registered", runRow.ID, forgeID)
	}

	return f, forge.FromRef(ref), repo, number, nil
}

// prURLOf reads a run's recorded pull request URL for an error message, "" when
// unset.
func prURLOf(runRow *run.Run) string {
	if runRow.PullRequestURL == nil {
		return ""
	}
	return *runRow.PullRequestURL
}
