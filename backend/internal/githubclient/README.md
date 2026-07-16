# backend/internal/githubclient

GitHub REST operations (read workflow spec, fire workflow_dispatch, PR surfaces); consumes `githubapp.TokenProvider`.

## Consolidated-PR surface (#714 / ADR-032)

`CreatePullRequest(installationID, repo, head, base, title, body)` POSTs `/repos/{o}/{r}/pulls` — the one GitHub write surface for the decomposition's single PR.

- It body-sniffs its own 422 for the duplicate marker and returns the typed `ErrPullRequestExists` BEFORE `classifyStatus` consumes the body (which maps all 422 → `ErrValidation`).
- `ListOpenPullRequestsByHead(installationID, repo, headBranch, base)` GETs `/pulls?head={owner}:{branch}&base&state=open` to recover the existing PR's `html_url` on that lost-race path (the 422 body carries no guaranteed PR number).

## Consolidated-diff surface (#1060)

`ComparePatch(installationID, repo, base, head)` GETs `/repos/{o}/{r}/compare/{base}...{head}` and returns a `ComparePatchResult{HeadSHA, Patch, Files[], Truncated, TruncationReason}` — the diff source for a decomposed parent's consolidated implement review (the parent has no runner trace bundle).

- It uses the structured JSON response (not the raw-diff media type) so the per-file `status` is available for `policy.ChangedFile` and GitHub's truncation signals are observable: `Truncated` is set when the file list hits the documented 300-file cap (`compareFilesCap`) or a changed file's patch body is omitted (oversized), so the consolidated-review dispatch surfaces the under-review loudly rather than silently.
- `Patch` reconstructs a unified diff by prefixing each file's hunks with a synthetic `diff --git` header.

## Forge credential-scope surface (#2009 / ADR-058)

`backend/internal/githubclient/scoped.go` adds a `forge.CredentialScope`-taking `Scoped`-suffixed variant of every exported `int64`-installation-taking method (49 at introduction), plus `NewWithCredentialProvider(forge.CredentialProvider) *Client`. This is Phase 1 (EXPAND) of the #1855 forge-credential split: purely additive.

- **Dual surface, one client type.** A `Client` built via the classic `New(githubapp.TokenProvider)` serves both the `int64` originals AND the `Scoped` variants (the variant resolves the scope to an `int64` and delegates to the original). A `Client` built via `NewWithCredentialProvider(forge.CredentialProvider)` also serves both surfaces — the unexported `credentialTokens` adapter wraps the provider as a `githubapp.TokenProvider` so the `int64` originals keep working unchanged. The two constructors and two surfaces coexist through the migrate phases (2-4 of #1855); nothing here forces callers to move yet.
- **Zero-scope fail-closed rule.** Every `Scoped` variant rejects a zero (`IsZero() == true`) or unparseable-ref scope via `installationIDForScope` BEFORE issuing any request — no outbound HTTP, no panic, an error naming the offending ref for a non-numeric ref. See `backend/internal/forge/README.md` for the scope contract.
- **`int64` originals stay canonical through phase 4.** They remain the single tested wire implementation; `Scoped` variants delegate to them rather than duplicating request-building. They are removed only in the contract phase (5/5).
