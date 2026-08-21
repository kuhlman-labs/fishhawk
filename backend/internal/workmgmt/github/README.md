# backend/internal/workmgmt/github

GitHub work-item provider (`github_projects`): issue filing, board placement on Projects v2, board-state transitions, and the epic-children query (see `backend/internal/workmgmt/README.md` for the capability contracts).

## User-owned Projects v2 board placement (#1114)

Why a GitHub App installation token can't board Project #7:

- `FISHHAWKD_PROJECTS_TOKEN` (`--projects-token`, `serve.go` → `cfg.GitHub.ProjectsToken`) is an optional user PAT/UAT carrying the **`project`** scope. A GitHub App installation token has no user-projects permission, so it cannot reach a personal-account Projects v2 (the `Could not resolve to a ProjectV2 with the number 7` errors).
- The provider's `placeOnBoard` opts the three board-placement GraphQL calls (`ProjectFields` / `AddProjectItem` / `SetProjectItemSingleSelect`) into the projects token via `githubclient.WithProjectsToken(ctx)` **only when `proj.OwnerType=="user"`**.
- `doGraphQL` honors the flag only when `Client.ProjectsToken` is non-empty, otherwise it falls back to the installation token — so issue creation (REST) + epic sub-issue linking (`AddSubIssue`, repo-scoped) stay on the installation token, and an unset token preserves the #1107 best-effort `boarded:false` degradation unchanged.
- The token is a secret: never logged or traced (startup logs `projects_token_configured` presence only), never included in an error message.
- Code: `backend/internal/githubclient/{client.go,projects.go}` + `backend/internal/workmgmt/github/provider.go`.

## Work-item read/list (#2230 / ADR-064)

`reader.go` implements the optional `workmgmt.WorkItemReader` capability. It has **no production consumer** — #2230 reserves the board-as-input seam. The cross-package contract (typed degradation, the never-a-board-item-list rule, the post-filter honesty note, the MCP invariant) lives in `../README.md`; what follows is GitHub-specific.

- **Enumeration:** `githubclient.ListRepoIssues` — the GraphQL `repository.issues` connection, `first:100` pages ordered ascending by number, following `pageInfo.endCursor` until `hasNextPage:false`. It is modelled directly on `ListSubIssues` (#2102). A ProjectV2 board item list is NEVER used: it caps at 100 and truncates silently (the AGENTS.md Project #7 trap), and the search REST API caps at 1000 results (`searchByTitleMaxPages`).
- **Page cap:** `listRepoIssuesMaxPages = 100` (10 000 issues). Reaching it with more pages remaining is a fail-closed error naming the repo and the accumulated count — the caller gets the COMPLETE set or a hard error, never a partial slice it could mistake for complete.
- **Server-side filters:** `labels:` (forwarded verbatim) and `states:` (`OPEN` only unless `IncludeClosed`). Board state cannot be pushed down, so it is a client-side post-filter — see `../README.md`.
- **User-owned board token dependency (#1114):** Project #7 is USER-owned, which a GitHub App installation token cannot reach. When board state is requested against a user-owned project, the reader requires `ProjectsTokenConfigured()` and opts the board GraphQL calls into `githubclient.WithProjectsToken`; with no token it returns `*workmgmt.UnavailableError{Reason: ReasonNoProjectsToken}` naming `FISHHAWKD_PROJECTS_TOKEN`. This is the deliberate opposite of `Transition`, which degrades to a best-effort SKIP on the same condition — a skipped board move is a no-op, a silently board-less READ is a wrong answer.
- **Board state per item:** on the list path from the per-issue `projectItems` selection (whose truncation FAILS CLOSED — see `../README.md`); on the read path from `IssueNodeID` + `ProjectItemStatus`, the same calls `Transition` makes.
- **Completion:** `Complete = closed AND completed`. The list path matches the UPPERCASE GraphQL enums (as `EpicChildren` does); the read path matches case-insensitively because `GetIssue` returns GitHub's LOWERCASE REST `state`/`state_reason` (as `ResolveDependencies` does).
- **Refs:** `ReadWorkItem` accepts `#N`, `N`, and `issue:N` through the existing `parseIssueRef` helper — no second regex.
