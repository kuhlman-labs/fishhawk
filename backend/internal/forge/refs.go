package forge

import "context"

// RefDeleter is the forge-neutral branch-delete capability the run-branch
// sweep consumes (E68.67 / #3562): delete one Fishhawk-owned branch from
// the forge once the run that created it is terminal.
//
// It is deliberately a STANDALONE capability interface rather than a new
// method on Forge, for the same reason FileFetcher (#2022) and
// IssueOperations (#2900) are: widening Forge would churn both registered
// implementations, the stub forge, and every test fake for a capability
// ONE consumer uses. Both registered adapters carry it off-interface with
// a compile-time assertion (`var _ forge.RefDeleter = (*Forge)(nil)` in
// forge/github and forge/gitlab); a consumer resolves a registered
// forge.Forge to it with a type assertion, and a forge that does not
// implement it is a fail-closed nil, never a fabricated no-op.
//
// Contract:
//   - branch is the short branch name (no "refs/heads/" prefix) and MAY
//     contain slashes (fishhawk/run-<short>/slice-<n>); implementations
//     address it as a hierarchical ref, never as one escaped segment.
//   - An ALREADY-ABSENT branch is NOT an error: each implementation maps
//     its forge's missing-ref status to nil (GitHub answers 422 "Reference
//     does not exist" — and 404 is tolerated too; GitLab answers 404), so
//     a re-observed merge or a redelivered cancel is an idempotent no-op.
//   - Every other failure surfaces. Notably ErrForbidden (a protected ref
//     or ruleset refusing the delete) is returned, not swallowed, so the
//     caller can record it rather than claim a deletion that did not
//     happen.
type RefDeleter interface {
	// DeleteRef deletes branch from repo. nil when the branch was deleted
	// or was already absent.
	DeleteRef(ctx context.Context, scope CredentialScope, repo RepoRef, branch string) error
}
