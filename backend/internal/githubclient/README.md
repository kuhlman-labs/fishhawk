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
