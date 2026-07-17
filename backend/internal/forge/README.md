# backend/internal/forge

Forge-neutral seams (ADR-057/ADR-058): the `CredentialScope` credential seam (#2009, the #1855 split) and the `Forge` interface over a code host's operational surface (#1858 / E45.4). Both are GitHub-only today; both exist so a second forge (GitLab per ADR-058) has a place to land instead of a tree full of `*githubclient.Client` fields.

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

Detection parses each file and inspects declaration nodes; it does not match source text. This is what makes the enforcement repo-wide rather than spelling-wide: a grouped declaration (`installationID, other int64`), a comment between the identifier and its type, or a name list wrapped across lines is the same cross-forge seam, and each of them slips past a line-oriented pattern. Parsing means the gate sees the declaration the compiler sees. `TestInstallationIDDeclDetectionForms` and `TestExportedClientInt64MethodDetectionForms` pin that behavior against those forms directly, and a file that fails to parse fails the gate rather than being skipped — an unscanned file is a hole.

One sanctioned entry is a deferral rather than a permanent survivor: `runnerbackend.TriggerParams.InstallationID` flips in #1861, where the `gitlab_ci` backend — the field's second consumer — gives it its forge-neutral shape.

## Forge interface (#1858 / E45.4)

`Forge` (`forge.go`) is the forge-neutral surface Fishhawk's flows drive a code host through: ref creation + SHA reads, git-data commit authoring (`GetRepository`/`GetCommit`/`CreateTree`/`CreateCommit`), the pull-request lifecycle (`Create`/`Get`/`Edit`/`Close`/`ListOpenByHead`/`ListForCommit`/`EnableAutoMerge`/`MergePullRequest`), commit status via `CreateCheckRun`, branch protection + ruleset required checks, `CompareCommits`/`ComparePatch`, plus `Name()` and `ResolveRepoScope`. Every method takes a `CredentialScope` first, mirroring the concrete client's scope-first shape — except `ResolveRepoScope`, which PRODUCES a scope (App-JWT auth) and so cannot take one.

**Push credentials are deliberately NOT a `Forge` method.** The forge-neutral push-credential seam is the existing `CredentialProvider` (#1855): it resolves a `CredentialScope` to a bearer token, which is exactly what a git push needs. Duplicating it as a `Forge` method would give one capability two interfaces to drift between.

### Vocabulary (`types.go`)

The DTO types the interface speaks — `RepoRef`, `Repository`, `GitCommit`, `TreeEntry`, `PullRequest`, `PullRequestRef`, `MergeMethod`, `BranchProtection`, `RulesetRequiredCheck`, `ComparePatch{Result,File}`, `CreateCheckRun{Params,Result}`, the `CheckRunStatus`/`CheckRunConclusion` enums — plus the sentinel errors (`ErrNotFound`, `ErrForbidden`, `ErrValidation`, `ErrNotInstalled`, `ErrPullRequestExists`, `ErrMergeConflict`, `ErrPullRequestCleanStatus`, `ErrPullRequestNotMergeable`) live canonically here. They moved verbatim off `githubclient`, which now re-declares each moved name as a type ALIAS (and each moved error var as an assignment to the forge canonical). An alias is the SAME type, so the whole tree keeps compiling and every `errors.Is` across the two spellings holds — the move is behavior-preserving by construction, and `forge_test.go` (`TestMovedSentinelErrorsPreserveIdentity`, `TestMovedTypesAreIdenticalAcrossSpellings`) pins it. The types are GitHub-shaped today because GitHub is the only implementation; the shapes generalize and get their forge-neutral refinement when the second implementation lands, not from this pass guessing it.

The message prefixes on the moved sentinels stay `"githubclient:"` deliberately: they are the SAME error values the concrete client has always returned and reach operators through logs/audits, so a zero-behavior-change move must not re-word them. A forge-neutral re-wording is a follow-up.

### Registry (`registry.go`)

A package-global `map[string]Forge` under a `sync.RWMutex`, copied verbatim from the proven `workmgmt` provider registry rather than newly designed: `Register(f)` keys on `f.Name()`, `Get(id)` returns a fail-closed `*UnknownForgeError` naming the id and the sorted known set (never a nil dispatch), `Registered()` returns the sorted ids for startup logging. `serve.go` registers the `github` adapter at startup.

### github adapter (`github/`)

`github.Forge` (`github/github.go`) is the registered `"github"` implementation, mirroring `workmgmt/github`: it EMBEDS `*githubclient.Client` to promote the covered methods verbatim (their signatures already take the moved `forge.*` vocabulary), and adds only `Name()` and `ResolveRepoScope` (which wraps `GetRepoInstallation` and converts via `FromGitHubInstallationID`, holding no `installationID int64` local so the #1855 credential-scope gate stays green). A `var _ forge.Forge = (*Forge)(nil)` assertion is the compile-time surface check. `New(c *githubclient.Client)` shares the same concrete client `serve.go` wires for the non-forge surfaces (issues/comments, projects, releases, contents, workflow dispatch).

Consumers should prefer a NARROW local interface naming just the methods they call (Go's consumer-side convention); `*github.Forge` satisfies both that and the full `Forge`.
