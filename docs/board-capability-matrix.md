# Per-forge board capability matrix

The one referenceable answer to "can Fishhawk read or write this repo's board, and what happens when it cannot?" (E45.24 / #2231, required by ADR-064).

ADR-064's rule: board integration uses per-tenant capability **detection** and graceful degradation, never an assumed-available surface. A board is a per-tenant, per-forge, per-credential capability — it is present for one repo and absent for the next, with the same code path and the same configuration. So every degradation surfaces as a typed `*workmgmt.UnavailableError` carrying a machine-readable `Reason`, and **never** an empty page a caller could misread as an empty backlog. A permissions failure and an empty backlog are the same bytes if the answer is a bare empty list, and acting on that confusion drives exactly the wrong decision.

Read this document alongside `backend/internal/workmgmt/README.md` § "Work-item read/list capability", which carries the long-form contract for the reader seam itself.

## The matrix

| Case | Board write (filing / board sync) | Board read (`WorkItemReader`) | Degradation |
|---|---|---|---|
| **GitHub, user-owned Projects v2** (the Project #7 case) | Requires the `project`-scoped `FISHHAWKD_PROJECTS_TOKEN` PAT/UAT. A GitHub App installation token **cannot** reach it: the Projects permission is org-only, so there is no user-projects permission for an App to hold (#1114). `placeOnBoard` and the transition path route board GraphQL calls onto that static token when `project.owner_type` is `user`. | Same credential constraint. | Filing degrades to `boarded:false` with a logged `boarding_error` — best-effort, the issue is still filed conventions-complete (#1107). A board **transition** degrades to a best-effort skip carrying `SkipReason`, never an error, so the mandated `work_item_transitioned` audit is never dropped. A board **read** fails typed with `no_projects_token`. |
| **GitHub, org-owned Projects v2** | Reachable with the App installation token; no separate projects token needed. | Reachable on the installation token. | The credential-shaped reasons do not apply. `no_project_configured` still applies when the target carries no project connection, and `forbidden` / `no_installation` still apply. |
| **Any GitHub project with more than 100 items** | Unaffected — placement writes a single item, it does not enumerate. | Enumeration goes through `repository.issues` (the GraphQL issues connection, cursor-paginated to completion under a fail-closed page cap), **never** a ProjectV2 board item list: that list caps at 100 and truncates silently. Per-issue board membership is read from the issue's own bounded `projectItems` page. | A **full** `projectItems` page that does not carry the target project leaves "is it on this board?" undecidable, so the read fails closed with `board_state_undecidable` rather than reporting a false `OnBoard=false`. A bounded window (`MaxScanned`) reports `WorkItemPage.Truncated` so a caller can tell "not in the backlog" from "not in the part I paid to look at". |
| **GitLab** | Board placement rides the issue create as a **label**: GitLab group/project boards are label-driven rather than single-select-field driven, so applying the state's label at create time *is* board placement. No separate transition call exists; the board-sync hook type-asserts `Transitioner` and yields a no-op. The instance sits inside the E45 group-scoped OAuth app. | `WorkItemReader` is deliberately **not** implemented in v0. The GitLab shape was reviewed before the interface vocabulary was fixed; leaving it unimplemented is a decision, not an omission. | `not_implemented`, returned by the `ReaderFor` chokepoint — never a nil interface a caller could dispatch against. |
| **Jira** | `Provider.File` creates the issue via Jira Cloud REST v3, then best-effort transitions it to the board status. | File-only in v0; `WorkItemReader` is not implemented. | `not_implemented`. |
| **No board capability at all** (no project connection, no installation, forge refusal) | Filing still succeeds and degrades to `boarded:false` with a logged `boarding_error` (#1107). The issue is never lost because the board is unreachable. | Fails typed. | `no_project_configured` when nothing declares a board to read; `no_installation` when no credential scope can mint a token; `forbidden` when the forge refused on permissions. |

## Degradation vocabulary

Every value below is a `workmgmt.UnavailableReason` constant declared in `backend/internal/workmgmt/reader.go`. It is what a caller **switches on** — the message text is for humans, the reason is the contract. `UnavailableError.Cause` retains the underlying forge error and `Unwrap` exposes it, so on one value `errors.As` yields the typed reason and `errors.Is(err, githubclient.ErrForbidden)` still holds for existing forge-level handling.

| Reason | What produces it | Operator remedy |
|---|---|---|
| `not_implemented` | The resolved provider does not implement the capability at all — a File-only provider (`gitlab`, `jira`) in v0. | None available: the capability does not exist for this forge in v0. Use a `github_projects` provider, or wait for the forge's reader to land. |
| `no_project_configured` | Board state was requested but the target carries no project connection, so there is no board to read. | Declare a `project:` block in the repo's `.fishhawk/work-management.yaml` (or accept that this repo has no board). |
| `no_projects_token` | The board is **user-owned**, which a GitHub App installation token cannot reach (#1114), and no projects token is configured. | Configure `FISHHAWKD_PROJECTS_TOKEN` with a `project`-scoped PAT/UAT. |
| `forbidden` | The forge refused the read on permissions. | Widen the credential's scope, or grant the App access to the target repository/project. |
| `no_installation` | No installation or credential scope is available, so no token can be minted for the read. | Install the GitHub App on the target account, or configure the forge credential. |
| `board_state_undecidable` | The forge **can** be read but its answer is ambiguous: an issue's per-issue `projectItems` page came back **full** without carrying the target project, leaving board membership undecidable. | Nothing about the credential is wrong, so prompting for a token would be the wrong response. Reduce the issue's project memberships, or read board state from the project side for that item. The offending item number is recoverable via `errors.As` on `Cause` (`*githubclient.BoardMembershipUndecidableError`). |

Two pairs are distinct on purpose. `no_project_configured` vs `no_projects_token`: one means *no board is declared*, the other means *a board is declared but unreachable with the credentials at hand*. `board_state_undecidable` vs `forbidden`: the remedy is different — collapsing them would send an operator hunting for a permissions problem that does not exist.

### Coverage is machine-enforced

`TestBoardCapabilityMatrixDocumentsEveryDeclaredUnavailableReason` (`backend/internal/workmgmt/conventions_test.go`) AST-walks `backend/internal/workmgmt`'s non-test source, collects every constant declared with type `UnavailableReason`, and asserts each declared string value appears in this document. It is a **source-derived sweep, not a frozen list**: a seventh reason added to `reader.go` reddens the test on the day it is added, with nobody having to remember this file exists.

## Where this is enforced

| Surface | What it owns |
|---|---|
| `backend/internal/workmgmt/reader.go` | The `ReaderFor(id)` resolution chokepoint, the `UnavailableReason` closed set, and `UnavailableError`. Routing every consumer through one chokepoint is what makes "never an empty list on a permissions failure" enforceable rather than conventional. |
| `backend/internal/workmgmt/github/provider.go` | `placeOnBoard`'s token routing — the `owner_type: user` → projects-token opt-in (#1114) and the best-effort `boarded:false` / transition-skip degradations (#1107). |
| `backend/internal/workmgmt/gitlab/provider.go` | The label-driven board model and the deliberate non-implementation of `WorkItemReader`, pinned by `TestProvider_DoesNotImplementWorkItemReader` (which would not compile had the methods been folded into `Provider`). |
| `backend/internal/mcpserver/board_read_guard_test.go` | The ADR-064 invariant that **no board-read path reaches the MCP agent tool surface**. An agent acting on a stale, unaudited, rate-limited, truncation-prone board column would be acting on unreconciled run state. It is a source-level, tamper-evident check over `mcpserver`'s full in-repo dependency closure, not a runtime capability boundary. |

## Selection config

A tenant declares **which board view feeds work selection** with the optional `selection` block in `work-management-v0` — see [`docs/spec/work-management-v0.md` § Selection](spec/work-management-v0.md#selection).

That block is a **declaration only**: nothing reads it, and the board-view-as-selection-source feature is deferred post-alpha by ADR-064. It lives in the work-management conventions rather than as a `board:` block in the workflow spec (ADR-064 fork 1) because the provider, project ref, states and transitions already live there, and the workflow spec stays a pure governance surface.

The matrix above is the constraint reference a declared selection source must be read against: a `source_view` on a user-owned GitHub project is unreadable without `FISHHAWKD_PROJECTS_TOKEN`, and one on a GitLab or Jira project is unreadable at all in v0. Declaring the block does not change that — it records the operator's intent for the consumer that will eventually have to honour it.

## See also

- `docs/spec/work-management-v0.md` — the conventions spec, including the `selection` block.
- `backend/internal/workmgmt/README.md` § "Work-item read/list capability (#2230 / ADR-064)" — the long-form reader contract.
- ADR-064; triggering issue #2231; the credential constraint is #1114 and the best-effort filing posture is #1107.
