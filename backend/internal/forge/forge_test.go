package forge_test

import (
	"errors"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
)

// TestMovedSentinelErrorsPreserveIdentity is the load-bearing assertion
// for the alias step: githubclient re-declares each moved sentinel as
// `var Err… = forge.Err…`, so the two spellings MUST bind the same error
// value and errors.Is MUST hold across them. If the alias step ever
// regressed to a fresh errors.New on the githubclient side, the two
// values would diverge and a caller switching on the forge spelling
// would miss an error the client returned under the githubclient
// spelling (or vice versa). This test fails in that world.
func TestMovedSentinelErrorsPreserveIdentity(t *testing.T) {
	cases := []struct {
		name         string
		forgeErr     error
		githubclient error
	}{
		{"ErrNotFound", forge.ErrNotFound, githubclient.ErrNotFound},
		{"ErrForbidden", forge.ErrForbidden, githubclient.ErrForbidden},
		{"ErrValidation", forge.ErrValidation, githubclient.ErrValidation},
		{"ErrNotInstalled", forge.ErrNotInstalled, githubclient.ErrNotInstalled},
		{"ErrPullRequestExists", forge.ErrPullRequestExists, githubclient.ErrPullRequestExists},
		{"ErrMergeConflict", forge.ErrMergeConflict, githubclient.ErrMergeConflict},
		{"ErrPullRequestCleanStatus", forge.ErrPullRequestCleanStatus, githubclient.ErrPullRequestCleanStatus},
		{"ErrPullRequestNotMergeable", forge.ErrPullRequestNotMergeable, githubclient.ErrPullRequestNotMergeable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Same identity in both directions.
			if !errors.Is(tc.githubclient, tc.forgeErr) {
				t.Errorf("errors.Is(githubclient.%s, forge.%s) = false; the alias must bind the same value", tc.name, tc.name)
			}
			if !errors.Is(tc.forgeErr, tc.githubclient) {
				t.Errorf("errors.Is(forge.%s, githubclient.%s) = false; the alias must bind the same value", tc.name, tc.name)
			}
			// A wrapped githubclient error is still matchable by the forge
			// sentinel — the shape real callers use.
			wrapped := errors.Join(errors.New("context"), tc.githubclient)
			if !errors.Is(wrapped, tc.forgeErr) {
				t.Errorf("errors.Is(wrap(githubclient.%s), forge.%s) = false", tc.name, tc.name)
			}
		})
	}
}

// TestMovedSentinelsAreDistinctFromEachOther guards the reverse failure:
// a copy-paste slip that pointed two aliases at the same forge sentinel
// would make two distinct conditions collapse into one. Every sentinel
// must remain a distinct value.
func TestMovedSentinelsAreDistinctFromEachOther(t *testing.T) {
	all := []error{
		forge.ErrNotFound,
		forge.ErrForbidden,
		forge.ErrValidation,
		forge.ErrNotInstalled,
		forge.ErrPullRequestExists,
		forge.ErrMergeConflict,
		forge.ErrPullRequestCleanStatus,
		forge.ErrPullRequestNotMergeable,
	}
	for i := range all {
		for j := range all {
			if i == j {
				continue
			}
			if errors.Is(all[i], all[j]) {
				t.Errorf("sentinels at index %d and %d compare equal; they must be distinct values", i, j)
			}
		}
	}
}

// TestErrUnsupportedIdentity pins forge.ErrUnsupported — the fail-closed
// sentinel for a Forge operation a forge's API cannot express (the GitLab
// adapter's git-data trio + MergeBranch, ADR-058 / #1859). Unlike the moved
// sentinels above it is a NEW forge-neutral value (not an alias of a
// githubclient error), so its identity is pinned here rather than in the
// alias-preservation table: it must be non-nil, matchable through a wrap the
// way real callers use it, and distinct from every moved sentinel so a
// caller switching on ErrUnsupported never collides with a not-found/
// validation/etc. failure.
func TestErrUnsupportedIdentity(t *testing.T) {
	if forge.ErrUnsupported == nil {
		t.Fatal("forge.ErrUnsupported is nil")
	}
	// Matchable through a wrap — the shape the GitLab adapter returns it in
	// (errors.Join / fmt.Errorf %w around the sentinel).
	wrapped := errors.Join(errors.New("gitlab: GetCommit"), forge.ErrUnsupported)
	if !errors.Is(wrapped, forge.ErrUnsupported) {
		t.Error("errors.Is(wrap(ErrUnsupported), ErrUnsupported) = false; the wrap must stay matchable")
	}
	// Distinct from every moved sentinel.
	for _, other := range []error{
		forge.ErrNotFound,
		forge.ErrForbidden,
		forge.ErrValidation,
		forge.ErrNotInstalled,
		forge.ErrPullRequestExists,
		forge.ErrMergeConflict,
		forge.ErrPullRequestCleanStatus,
		forge.ErrPullRequestNotMergeable,
	} {
		if errors.Is(other, forge.ErrUnsupported) {
			t.Errorf("ErrUnsupported collides with %v; it must be a distinct value", other)
		}
	}
}

// TestMovedTypesAreIdenticalAcrossSpellings pins the alias posture at
// the type level: an alias is the SAME type, so a value built under one
// spelling must be assignable to a variable of the other with no
// conversion. If githubclient ever redefined these as distinct named
// types, these assignments would stop compiling — the test is a
// compile-time contract that also runs.
func TestMovedTypesAreIdenticalAcrossSpellings(t *testing.T) {
	// assignIdentical asserts, at compile time, that a value produced
	// under the githubclient spelling is assignable to the forge type T
	// with no conversion — which holds iff the two are the SAME type
	// (an alias), not merely convertible. It returns the value so
	// callers can also assert on it at runtime.
	repo := assignIdentical[forge.RepoRef](t, githubclient.RepoRef{Owner: "o", Name: "n"})
	if repo.String() != "o/n" {
		t.Errorf("RepoRef.String() = %q, want %q", repo.String(), "o/n")
	}

	pr := assignIdentical[forge.PullRequest](t, githubclient.PullRequest{Number: 7})
	if pr.Number != 7 {
		t.Errorf("PullRequest.Number = %d, want 7", pr.Number)
	}

	// Enum constants are the same value across spellings.
	method := assignIdentical[forge.MergeMethod](t, githubclient.MergeMethodSquash)
	if method != forge.MergeMethodSquash {
		t.Errorf("MergeMethodSquash mismatch across spellings")
	}
	status := assignIdentical[forge.CheckRunStatus](t, githubclient.CheckRunStatusCompleted)
	if status != forge.CheckRunStatusCompleted {
		t.Errorf("CheckRunStatusCompleted mismatch across spellings")
	}

	// The remaining moved vocabulary is assignable across spellings.
	assignIdentical[forge.Repository](t, githubclient.Repository{})
	assignIdentical[forge.GitCommit](t, githubclient.GitCommit{})
	assignIdentical[forge.TreeEntry](t, githubclient.TreeEntry{})
	assignIdentical[forge.PullRequestRef](t, githubclient.PullRequestRef{})
	assignIdentical[forge.BranchProtection](t, githubclient.BranchProtection{})
	assignIdentical[forge.RulesetRequiredCheck](t, githubclient.RulesetRequiredCheck{})
	assignIdentical[forge.ComparePatchFile](t, githubclient.ComparePatchFile{})
	assignIdentical[forge.ComparePatchResult](t, githubclient.ComparePatchResult{})
	assignIdentical[forge.CreateCheckRunParams](t, githubclient.CreateCheckRunParams{})
	assignIdentical[forge.CreateCheckRunResult](t, githubclient.CreateCheckRunResult{})
	assignIdentical[forge.CheckRunConclusion](t, githubclient.CheckRunConclusionSuccess)
}

// assignIdentical returns v typed as T. Because T is the forge spelling
// and v is the githubclient spelling, this compiles ONLY when the two
// name the same type — the alias contract. A distinct named type on
// either side would make the call a type error.
func assignIdentical[T any](t *testing.T, v T) T {
	t.Helper()
	return v
}
