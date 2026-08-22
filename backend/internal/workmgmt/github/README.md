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
- **Undecidable board membership is TYPED.** `ListWorkItems` matches `githubclient`'s `*BoardMembershipUndecidableError` (the truncated-`projectItems` refusal) with `errors.As` and re-surfaces it as `*workmgmt.UnavailableError{Reason: ReasonBoardStateUndecidable}`, retaining the githubclient error in `Cause`. So a caller classifies it through the SAME `errors.As` chokepoint every other degradation uses, and can unwrap for the offending issue number; it never has to match on message text. `TestListWorkItems_UndecidableBoardMembershipIsTyped` drives it end to end over `httptest`.
- **`WorkItemRecord.URL` is LIST-PATH-ONLY.** The list path populates it from the GraphQL issue node's `url`; `ReadWorkItem` leaves it EMPTY, because `GetIssue` decodes `githubclient.Issue` — the REST single-issue payload — which carries no URL field at all. Widening that shared REST struct is deferred to the first consumer that needs it (several unrelated callers consume it). A caller must not read an empty URL from the read path as "this item has no URL": reconstruct it from `Number` plus the target repo, or read through `ListWorkItems`. Pinned by the URL assertion in `TestProvider_ReadWorkItem_AcceptsEveryRefForm` so the asymmetry cannot drift unnoticed.
- **Completion:** `Complete = closed AND completed`. The list path matches the UPPERCASE GraphQL enums (as `EpicChildren` does); the read path matches case-insensitively because `GetIssue` returns GitHub's LOWERCASE REST `state`/`state_reason` (as `ResolveDependencies` does).
- **Refs:** `ReadWorkItem` accepts `#N`, `N`, and `issue:N` through the existing `parseIssueRef` helper — no second regex.

## Grooming apply — the mutation half (E54.5 / #2237)

`grooming.go` implements the optional `workmgmt.GroomingMutator` capability. Like the reader it has **no production consumer** — #2237 reserves the board-WRITE seam. The provider-neutral contract (the closed vocabulary, the seven containment rules, the continue-and-report executor) lives in `../README.md`; what follows is GitHub-specific.

**Authorization is NOT re-litigated here.** Every containment decision — join, operator verdict, action-class mode, destructive authorization, idempotence diff, manual-placement courtesy — is made by `workmgmt.ApplyGrooming` before a request reaches this package. The one thing the provider re-checks is the expected-source set on a board move, as defence in depth, exactly as `Transition` already does.

### Kind → GitHub primitive

| Kind | Primitive | Notes |
|---|---|---|
| `label_set` | `GetIssue` → `UpdateIssue(labels)` | Sends the **union**. GitHub's PATCH replaces `labels` wholesale, so an add-one-label mutation must read the current set first. A label already present is a provider-side skip, not a write. |
| `epic_link` | `IssueNodeID` ×2 → `AddSubIssue` | The same sub-issues path `File`'s `linkEpic` uses. Never a PATCH. |
| `depends_on_add` | `GetIssue` → `ensureDependsOnMarker` → `UpdateIssue(body)` | Reuses the existing idempotent body-marker helper; a body already carrying a marker is a skip. |
| `close_duplicate` | `UpdateIssue(state=closed, state_reason=duplicate)` | **Destructive.** |
| `close_not_planned` | `UpdateIssue(state=closed, state_reason=not_planned)` | **Destructive.** Never `completed`, which would misreport a descoped item as delivered work. |
| `board_place` | `ProjectFields` → `IssueNodeID` → `ProjectItemStatus` → `placeIssueOnBoard` | Empty expected-source set ⇒ proceeds only while the item is genuinely OFF-board. |
| `icebox` | the same board path, targeting the conventions' icebox column | **Destructive.** Routed through the SAME placement guard and idempotence check as `board_place` (approval condition I2) — an icebox move must not override a placement a human chose. |
| `field_set` | `ProjectFields("Estimate")` → `SetProjectItemSingleSelect` | The `missing_estimate` hygiene defect's fix. |
| `priority_set` | `ProjectFields("Priority")` → `SetProjectItemSingleSelect` | |
| `rank_set` | `ProjectFields("Rank")` → `SetProjectItemSingleSelect` | **A field write, not a queue reorder — see below.** |

### `rank_set` and `priority_set` are FIELD writes, not positional reordering (I3)

This repository's provider implements **no positional primitive**, and #2237 adds none. `rank_set` writes the board's `Rank` field value and `priority_set` writes `Priority`; the board's own item order does **not** move. That is a deliberate narrowing of the "queue order" language in the issue, not an omission: what E54.6 / #2238's campaign feed will actually read is the **Rank field value**, sorting on it to recover the proposed order. A consumer must not expect the board's visual ordering to reflect an applied `rank_set`.

The value written must be one of the field's configured single-select options. Anything else is `*UnsupportedGroomingKindError` naming the value and the available options — never a guess at the nearest option.

### The missing-icebox-column fail path (I5)

Two distinct cases, both **typed refusals**, never a silent no-op and never a misroute to another column:

- **No icebox column configured** — `workmgmt.ApplyGrooming` refuses upstream with the audited skip `icebox_column_unavailable` (`GroomingApplyRequest.IceboxColumn` is empty; work-management-v0's `states` map declares no icebox canonical state, so there is nothing to resolve it from). A caller that reaches the provider anyway gets `*UnsupportedGroomingKindError`. Pinned by `TestApplyGroomingMutation_IceboxWithNoColumnIsTypedNotSilent`.
- **Configured but not a board option** — `*UnsupportedGroomingKindError` naming the column and the available options, raised BEFORE any board write. Pinned by `TestApplyGroomingMutation_IceboxColumnNotOnBoardIsTypedNotSilent`.

### `githubclient.UpdateIssue`: pointer params are the safety property

`UpdateIssueParams` fields are all pointers and the marshalled body carries **only the non-nil ones**. This is not style. GitHub's PATCH replaces `labels` wholesale, so a value-typed `Labels []string` left at its zero value would serialize as `"labels":null` and silently strip every label off an issue an unrelated body-only update touched. With a pointer, *not set* and *set to empty* are distinct states, and only the caller's explicit choice reaches the wire. `TestUpdateIssue_RequestBodies` asserts the exact serialized body per case, including the body-only row that carries no `labels` key at all. A params set with no field is refused locally rather than sent.

### What the tests prove, and what needs live validation (I4)

The `httptest` fixtures pin the **request shape this provider emits** — method, path, exact serialized body, call count, and which calls are *not* made. They do **not** prove the forge accepts that shape. Specifically:

- **Verified locally:** the label union; the `duplicate` / `not_planned` state_reason values are the ones we *send*; the pointer-omission invariant; the board pre-check ordering; every typed refusal; and the end-to-end core → provider → transport seam (`TestApplyGrooming_EndToEndThroughGitHubProvider`), which asserts on the requests an httptest forge actually received.
- **Requires live validation:** whether GitHub **honours** the proposed `state_reason` values on close, and whether a real board carries `Rank` / `Priority` / `Estimate` as single-select fields with the proposed option names. Neither is decidable against a fixture, which accepts whatever it is handed.

The acceptance stage cannot close that gap either: no run can select the grooming workflow (its non-diff trigger form matches nothing v0 mints), so an acceptance pass on this slice short-circuits with every criterion skipped, as it did on #2234. This section is the honest record in place of coverage that will not exist.
