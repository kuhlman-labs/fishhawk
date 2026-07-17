# backend/internal/githubclient

GitHub REST operations (read workflow spec, fire workflow_dispatch, PR surfaces); consumes `githubapp.TokenProvider`.

## Consolidated-PR surface (#714 / ADR-032)

`CreatePullRequest(scope, repo, head, base, title, body)` POSTs `/repos/{o}/{r}/pulls` — the one GitHub write surface for the decomposition's single PR.

- It body-sniffs its own 422 for the duplicate marker and returns the typed `ErrPullRequestExists` BEFORE `classifyStatus` consumes the body (which maps all 422 → `ErrValidation`).
- `ListOpenPullRequestsByHead(scope, repo, headBranch, base)` GETs `/pulls?head={owner}:{branch}&base&state=open` to recover the existing PR's `html_url` on that lost-race path (the 422 body carries no guaranteed PR number).

## Consolidated-diff surface (#1060)

`ComparePatch(scope, repo, base, head)` GETs `/repos/{o}/{r}/compare/{base}...{head}` and returns a `ComparePatchResult{HeadSHA, Patch, Files[], Truncated, TruncationReason}` — the diff source for a decomposed parent's consolidated implement review (the parent has no runner trace bundle).

- It uses the structured JSON response (not the raw-diff media type) so the per-file `status` is available for `policy.ChangedFile` and GitHub's truncation signals are observable: `Truncated` is set when the file list hits the documented 300-file cap (`compareFilesCap`) or a changed file's patch body is omitted (oversized), so the consolidated-review dispatch surfaces the under-review loudly rather than silently.
- `Patch` reconstructs a unified diff by prefixing each file's hunks with a synthetic `diff --git` header.

## Forge credential-scope surface (#2009 / ADR-058)

Every exported `*Client` method takes a `forge.CredentialScope` as its first post-`ctx` argument. There is ONE surface: the `Scoped`-suffixed variants that phase 1 (EXPAND, #2009) added alongside the `int64` originals are gone, and the original names now carry the scope. This is the contract state — phase 5/5 of the #1855 forge-credential split (#2013).

- **One surface, resolved once at the boundary.** Each exported method resolves its scope to a GitHub installation id exactly once on entry via `installationIDForScope`, then hands the `int64` to the unexported plumbing (`buildRequest`, `doGraphQL`, `fetchRulesetContexts`). That plumbing stays `int64` by design: it is below the forge boundary and speaks GitHub's REST/GraphQL wire format.
- **Zero-scope fail-closed rule.** Every method rejects a zero (`IsZero() == true`) or unparseable-ref scope BEFORE issuing any request — no outbound HTTP, no panic, an error naming the offending ref for a non-numeric ref. See `backend/internal/forge/README.md` for the scope contract.
- **Constructors.** `NewWithCredentialProvider(forge.CredentialProvider)` is the forge-neutral entry point. `New(githubapp.TokenProvider)` and `NewWithSigner` are kept as the GitHub-convenience constructors (`fishhawkd`'s `serve.go` builds via `NewWithSigner`); both feed the same scope-taking surface through the unexported `credentialTokens` adapter, which wraps the int64-taking `githubapp.TokenProvider`. The choice of constructor does not change the method surface.
- **The gate.** `backend/internal/forge/credential_scope_gate_test.go` walks all three modules' non-test Go source and fails, naming `file:line`, if an `installationID int64` declaration reappears outside its sanctioned survivor allowlist (GitHub App token internals, this package's unexported plumbing, webhook ingest, run persistence, and the onboarding payload mirrors). A second assertion pins this package specifically: no exported `*Client` method may take a bare `int64` installation id.

## Forge vocabulary aliases (#1858 / E45.4)

The forge-surface DTO vocabulary this package used to define — `RepoRef`, `Repository`, `GitCommit`, `TreeEntry`, `PullRequest`, `PullRequestRef`, `MergeMethod` (+ consts), `BranchProtection`, `RulesetRequiredCheck`, `ComparePatch{Result,File}`, `CreateCheckRun{Params,Result}`, `CheckRunStatus`/`CheckRunConclusion` (+ consts) — plus the sentinel errors now live canonically in `backend/internal/forge` (`types.go`). This package re-declares each as a type ALIAS (`type RepoRef = forge.RepoRef`) and each error as an assignment (`var ErrNotFound = forge.ErrNotFound`), in the alias block near the top of `client.go`.

- **An alias is the same type, not a new named type.** Every existing reference — in production code and in the many test fixtures that build `&githubclient.Client{}` literals and `githubclient.PullRequest{}` values — keeps compiling against the same type with zero behavior change; method sets and assignability are preserved. Each aliased error `var` binds the SAME value as its forge canonical, so `errors.Is` holds across both spellings.
- **The aliases are for the UNMIGRATED non-forge surfaces.** Issues/comments/reactions still spell `RepoRef` through `githubclient`; they keep working via the alias. Migrated packages reference `forge.*` directly, enforced by `backend/internal/forge/consumer_migration_gate_test.go` (a sibling migration, #1858) so an alias-compatible no-op touch cannot silently pass for a real migration.
- The exported `*Client` methods (`CreateRef`, `MergeBranch`, `CreatePullRequest`, `ComparePatch`, …) are unchanged: their signatures already spoke this vocabulary, which is now the moved `forge.*` types via the aliases. `forge/github` embeds `*Client` to promote them onto `forge.Forge`.
