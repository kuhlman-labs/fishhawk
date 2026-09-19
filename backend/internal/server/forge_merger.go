package server

import (
	"context"
	"errors"
	"fmt"

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

// MergePullRequest routes the run's merge by forge family per the type doc.
func (m ForgeMerger) MergePullRequest(ctx context.Context, runRow *run.Run) error {
	family := observationForgeID(runRow.InstallationRef)
	if family == observationForgeGitHub {
		if m.GitHub == nil {
			return fmt.Errorf("forge merge: run %s: no github merge client configured", runRow.ID)
		}
		return m.GitHub.MergePullRequest(ctx, runRow)
	}

	// Non-github family. The ref is non-empty by construction (a non-github
	// family id only comes from a "<forge>:<id>" InstallationRef), but guard it
	// explicitly so a caller cannot reach the resolver with an empty scope.
	if runRow.InstallationRef == nil || *runRow.InstallationRef == "" {
		return fmt.Errorf("forge merge: run %s has no installation ref", runRow.ID)
	}
	ref := *runRow.InstallationRef

	forgeID, repo, number, reason := resolveObservationTarget(runRow)
	switch reason {
	case obsTargetMalformed:
		return fmt.Errorf("forge merge: run %s: could not resolve repository and pull request number from %q",
			runRow.ID, prURLOf(runRow))
	case obsTargetMismatch:
		return fmt.Errorf("forge merge: run %s: recorded pull request url %q does not name this run's repository on forge family %q",
			runRow.ID, prURLOf(runRow), forgeID)
	}

	resolver := m.Resolver
	if resolver == nil {
		resolver = forge.Get
	}
	f, err := resolver(forgeID)
	if err != nil {
		return fmt.Errorf("forge merge: run %s: resolve forge %q: %w", runRow.ID, forgeID, err)
	}
	if isNilForge(f) {
		return fmt.Errorf("forge merge: run %s: forge %q not registered", runRow.ID, forgeID)
	}

	scope := forge.FromRef(ref)
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

// prURLOf reads a run's recorded pull request URL for an error message, "" when
// unset.
func prURLOf(runRow *run.Run) string {
	if runRow.PullRequestURL == nil {
		return ""
	}
	return *runRow.PullRequestURL
}
