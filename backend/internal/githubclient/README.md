# backend/internal/githubclient

GitHub REST operations (read workflow spec, fire workflow_dispatch, PR surfaces); consumes `githubapp.TokenProvider`.

## Consolidated-PR surface (#714 / ADR-032)

`CreatePullRequest(scope, repo, head, base, title, body)` POSTs `/repos/{o}/{r}/pulls` — the one GitHub write surface for the decomposition's single PR.

- It body-sniffs its own 422 for the duplicate marker and returns the typed `ErrPullRequestExists` BEFORE `classifyStatus` consumes the body (which maps all 422 → `ErrValidation`).
- `ListOpenPullRequestsByHead(scope, repo, headBranch, base)` GETs `/pulls?head={owner}:{branch}&base&state=open` to recover the existing PR's `html_url` on that lost-race path (the 422 body carries no guaranteed PR number).

## Transient-retry transport (#2167 / E48.45)

`New()` installs a bounded-retry `http.RoundTripper` (`retryTransport`) on the default `Client.HTTP` (30s Timeout unchanged), so an isolated GitHub 5xx / secondary-rate-limit / primary-rate-limit blip is absorbed in-process instead of failing the caller (the observed `gitops: open PR: 500` outage that re-ran the whole implement stage). The runner's `gitops.OpenPRClient` carries the same contract in its own explicit loop.

- **Retry-safety is method-gated, not body-presence-gated.** A `req.GetBody`-present POST is NOT automatically retry-safe: a 5xx that arrives AFTER the server applied the mutation would duplicate it. The transport retries only (a) idempotent reads (GET/HEAD) and (b) mutations whose call site EXPLICITLY opts in via `withRetryableMutation(ctx)` AND that carry a replayable body. The ONLY opt-in call sites are `CreatePullRequest` and `CreateRef`, each guarded server-side by an already-exists 422 sniff, so a re-applied create collapses to a benign no-op. Issue-comment / GraphQL-mutation / create-issue / label / release-asset-upload POSTs never opt in and are never retried.
- **Retryable status set (`retryableStatus`).** ALL 5xx (`>= 500`), 429, and a 403 carrying a rate-limit signal (`Retry-After` header, or `x-ratelimit-remaining:0`). A plain permission 403 (no rate-limit headers) is NOT retried — it returns promptly. Retrying a rare permanent 5xx is fail-safe: bounded attempts then a return.
- **Backoff (`retryDelay`).** Honors `Retry-After` (integer seconds OR HTTP-date per RFC 7231 §7.1.3), else `X-RateLimit-Reset` (unix epoch seconds) when `x-ratelimit-remaining:0`, else exponential `base * 2^attempt` with full jitter. Every wait is capped to `maxDelay` AND to the REMAINING request-context budget: a server-provided delay that meets OR exceeds the remaining budget (`delay >= remaining`, the boundary) gives up PROMPTLY (returns the classified response) rather than sleeping the whole deadline only to issue a doomed retry. Every sleep is context-aware, so a caller cancellation / client Timeout stops retries cleanly mid-sleep. A malformed header falls through to backoff (fail-safe, never a hang).
- **Bounds.** Up to `maxRetries` additional attempts (defaults resolved in `newRetryTransport`); an exhausted-retries request returns the classified error.
- **Idempotent PR-open is already satisfied** (no `CreatePullRequest` semantic change): the duplicate 422 → `ErrPullRequestExists` → `ListOpenPullRequestsByHead` adopt-by-head path recovers a lost create race. The 422 sniff is a 4xx, so the transport never retries it. This closes issue criterion 3 for retry-ABSORBED PR-open failures (the common transient / rate-limit case); the sustained-outage checkpoint-resume (retry_stage skipping the agent re-run on an already-pushed branch) is a separate follow-up.

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

## Per-installation REST base-URL routing (Mode 2, #2094 / E44.16)

`*Client` carries two additive, backward-compatible fields — `ResolveBaseURL func(ctx, installationRef string) (string, error)` and `AllowedInstallationHosts []string` — that route installation-scoped requests to a per-installation forge endpoint (data-resident installs on `<slug>.ghe.com`, ADR-057 Amendment A1). This extends the githubapp mint precedent (#1826) to the whole REST surface.

- **One choke point.** The override is applied ONLY in `buildRequest` (`applyInstallationBaseURL`), which is the sole request-construction path for every installation-scoped method. `codescanning.go`, `gitdata.go`, and `projects.go` build their URL via `endpoint(...)` and pass it here — so per-installation routing needed NO edit to any of those files. `scoped.go` constructs no requests of its own.
- **Fail-closed, before the token ships.** `applyInstallationBaseURL` runs BEFORE the token is minted. A resolver error, an override that is not a well-formed absolute https URL (`account.ValidateResolvedBaseURL`), or — when `AllowedInstallationHosts` is non-empty — a host outside the allowlist (`account.HostAllowed`) each return an error and issue NO request. The validation + allowlist contract is the shared forge-neutral one in `backend/internal/account/hostpolicy.go` (promoted verbatim from githubapp so it cannot drift).
- **Backward-compatible.** A nil resolver, an empty resolved base (NULL column / unknown installation), or an empty allowlist leaves the request byte-identical to the deployment default (`BaseURL`, else `DefaultBaseURL`).
- **Rewrite gate is URL-prefix, not caller.** Only requests prefixed by the REST API base are rewritten (scheme+host+base-path swapped, path+query preserved). Release-asset uploads (`UploadBaseURL`/`DefaultUploadBaseURL`) and the static-token user-Projects GraphQL path (`buildStaticTokenRequest`, not installation-scoped) are left untouched; installation-scoped GraphQL issued via `buildRequest` IS rewritten because it targets the API base.

## Forge vocabulary aliases (#1858 / E45.4)

The forge-surface DTO vocabulary this package used to define — `RepoRef`, `Repository`, `GitCommit`, `TreeEntry`, `PullRequest`, `PullRequestRef`, `MergeMethod` (+ consts), `BranchProtection`, `RulesetRequiredCheck`, `ComparePatch{Result,File}`, `CreateCheckRun{Params,Result}`, `CheckRunStatus`/`CheckRunConclusion` (+ consts) — plus the sentinel errors now live canonically in `backend/internal/forge` (`types.go`). This package re-declares each as a type ALIAS (`type RepoRef = forge.RepoRef`) and each error as an assignment (`var ErrNotFound = forge.ErrNotFound`), in the alias block near the top of `client.go`.

- **An alias is the same type, not a new named type.** Every existing reference — in production code and in the many test fixtures that build `&githubclient.Client{}` literals and `githubclient.PullRequest{}` values — keeps compiling against the same type with zero behavior change; method sets and assignability are preserved. Each aliased error `var` binds the SAME value as its forge canonical, so `errors.Is` holds across both spellings.
- **The aliases are for the UNMIGRATED non-forge surfaces.** Issues/comments/reactions still spell `RepoRef` through `githubclient`; they keep working via the alias. Migrated packages reference `forge.*` directly, enforced by `backend/internal/forge/consumer_migration_gate_test.go` (a sibling migration, #1858) so an alias-compatible no-op touch cannot silently pass for a real migration.
- The exported `*Client` methods (`CreateRef`, `MergeBranch`, `CreatePullRequest`, `ComparePatch`, …) are unchanged: their signatures already spoke this vocabulary, which is now the moved `forge.*` types via the aliases. `forge/github` embeds `*Client` to promote them onto `forge.Forge`.

## Auto-merge enable sentinels (`EnableAutoMerge`)

`EnableAutoMerge` issues GitHub's `enablePullRequestAutoMerge` GraphQL mutation, which returns HTTP 200 with an application-level `errors` array on a refusal. The refusal message is substring-classified into `ErrValidation` plus, for two known precondition classes, an additional wrapped sentinel (Go multi-`%w`, so every existing `errors.Is(err, ErrValidation)` caller is unaffected):

- **`ErrPullRequestCleanStatus`** (message contains `"clean status"`, #1954) — the PR is ALREADY merge-ready, so GitHub refuses to queue auto-merge on something it could merge synchronously right now. The operator merge path (`serve.go` `githubAutoMerger`) **falls back to a synchronous REST squash merge** on this sentinel.
- **`ErrPullRequestUnstableStatus`** (message contains `"unstable status"`, E67.56 / #2717) — the PR is in GitHub's UNSTABLE `mergeStateStatus`: its required checks have NOT all passed. GitHub defines UNSTABLE as "mergeable with a non-passing commit status", which covers BOTH a still-pending required context AND one that has already COMPLETED and FAILED — so this sentinel is **not** proof the checks are merely unfinished, and a failed non-required check leaves a PR UNSTABLE forever. This class **takes NO REST fallback**, deliberately: a synchronous REST squash merge on an UNSTABLE PR would SUCCEED and land the change before its pending checks report. The server maps it to a bounded wait (`409 merge_checks_pending`), not a merge.

The two substrings (`"clean status"` vs `"unstable status"`) are disjoint, so neither branch can capture the other's message — pinned by paired positive/negative `errors.Is` assertions across the clean, unstable, and unrelated messages in `client_test.go`. A future GitHub rewording of either message degrades to the plain `ErrValidation` (the status-quo generic path), never a wrong merge.
