# backend/internal/forge

Forge-neutral credential-scope seam (ADR-057/ADR-058, #2009). Phase 1 of 5 of the #1855 forge-credential split: purely additive, zero behavior change.

## What a credential scope is

`CredentialScope` names "which installation to authenticate as" without committing to a specific forge. Its canonical wire form is the `installations.installation_ref` TEXT column (ADR-057/ADR-058): today that's the stringified GitHub App installation id (e.g. `"4242"`, per `docs/ARCHITECTURE.md`'s installations-table contract and `backend/internal/postgres/postgres_test.go`'s `"4242"` round-trip). A future GitLab implementation (ADR-058) stores a different shape of ref in the same column — the type itself carries no forge assumption.

## Construction never validates

`FromRef(ref string) CredentialScope` is the forge-neutral constructor: it wraps an arbitrary `installation_ref` verbatim, with no validation at construction. This is deliberate — construction must not pre-judge which forge a ref belongs to. Non-GitHub credential implementations, and cross-package tests exercising non-GitHub-shaped refs (e.g. `githubapp.scoped_test.go`, `githubclient.scoped_test.go`), construct their scopes through `FromRef`.

`FromGitHubInstallationID(id int64) CredentialScope` is the GitHub convenience wrapper: `FromRef(strconv.FormatInt(id, 10))`. `id == 0` — the codebase's unresolved-installation sentinel — maps to the zero scope (empty ref).

## Zero-scope sentinel

The zero value of `CredentialScope` (empty ref) is the unresolved-installation sentinel, preserving the pre-scope `installationID == 0` semantics after the type change. `IsZero() bool` reports it. `Ref() string` returns the canonical ref, empty for the zero scope.

## Fail-closed parse rule

`GitHubInstallationID() (int64, error)` parses the ref with `strconv.ParseInt(ref, 10, 64)`. It NEVER returns a silent `0` with a nil error — an empty ref or a non-numeric ref (e.g. a GitLab-shaped ref) returns a non-nil error naming the offending ref. Validation lives here, at the point of USE, not at construction.

## CredentialProvider

```go
type CredentialProvider interface {
	Token(ctx context.Context, scope CredentialScope) (string, error)
}
```

The forge-neutral analogue of `githubapp.TokenProvider`. `githubapp.ScopedProvider` (`backend/internal/githubapp/scoped.go`) adapts the existing `githubapp.TokenProvider` to this interface by resolving `scope.GitHubInstallationID()` and delegating. `githubclient.NewWithCredentialProvider` (`backend/internal/githubclient/scoped.go`) constructs a `*githubclient.Client` directly from a `CredentialProvider`.

## Contract state (#1855 phase 5/5, #2013)

The staged split is complete. The `githubclient` `Scoped`-suffixed method variants are gone: their names collapsed back onto the originals, so every exported `*githubclient.Client` method now takes a `CredentialScope` and resolves it to an installation id exactly once at entry. `githubapp.ScopedProvider` and `githubclient.NewWithCredentialProvider` remain the forge-neutral adapters.

`credential_scope_gate_test.go` in this package is the enforcement: it walks every non-test `.go` file in the `backend`/`runner`/`cli` modules (skipping `*/db/` sqlc-generated packages, per the AGENTS.md coverage-gate convention) and fails, naming `file:line`, on any `installationID`/`InstallationID` `int64` declaration outside its sanctioned survivor allowlist — the GitHub App token internals, `githubclient`'s unexported wire plumbing, webhook ingest payloads, run-row persistence, and the onboarding payload mirrors. Each allowlist entry carries the reason it is forge-specific; a new cross-forge `int64` seam has to argue its way in.

One sanctioned entry is a deferral rather than a permanent survivor: `runnerbackend.TriggerParams.InstallationID` flips in #1861, where the `gitlab_ci` backend — the field's second consumer — gives it its forge-neutral shape.
