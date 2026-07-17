# backend/internal/workmgmt/gitlab

GitLab issues work-item provider (`provider: gitlab`) — the concrete third provider (ADR-058 Phase 2, #1856), alongside `github_projects` and `jira`.

## Filing (`provider.go`)

- `File` maps a resolved `workmgmt.ProviderRequest` onto `backend/internal/gitlabclient` (GitLab REST v4, `PRIVATE-TOKEN` auth).
- It resolves the target project first — the conventions `gitlab.project` override wins, else the filing repo's `owner/name` path — via `GetProject`. A resolve failure is **fatal** (nil item + error): the numeric project id addresses every subsequent call.
- It then creates the issue with the conventions-resolved labels **plus the board-status label** (see below), the second and last fatal step — no issue exists if `CreateIssue` fails.
- It finally links `Relations.ParentEpic` **best-effort** (#1107) via a Free-tier issue link (see below): a parse or link failure records `EpicLinkError` and leaves `EpicLinked=false` rather than discarding the issue.
- `CreatedItem.Number` is the issue iid; `URL` is the issue web URL.

## Mapping decisions

### Board placement → label (not a transition)

GitLab issue boards are **label-driven** (<https://docs.gitlab.com/ee/user/project/issue_board.html>): a board column is a saved label filter, so a card lands in a column by carrying that column's label. The canonical-state map's values are therefore GitLab **label names**, and the provider applies the resolved `BoardPlacement.Status` label **at create time** — the label riding the create *is* the board placement. There is no separate move call, so `Boarded` is true the moment `CreateIssue` succeeds with a status configured (and false, with an empty `BoardingError`, when no status is set — there was nothing to board). This is why the board-state `Transitioner` capability (#1012) is **not** implemented for gitlab: placement is a filing-time label, not a post-create transition.

### `parent_epic` → Free-tier `relates_to` issue link (not a Premium group epic)

GitLab **group epics are a Premium feature** (<https://docs.gitlab.com/ee/user/group/epics/>). To keep the v0 provider usable on Free/self-managed without a Premium tier, a `parent_epic` reference maps to a Free-tier **`relates_to` issue link** (`POST /projects/:id/issues/:iid/links`, <https://docs.gitlab.com/ee/api/issue_links.html>) rather than an epic membership. The reference (`#N` or `N`) parses with the same numeric-ref semantics as the github/jira siblings; an unparseable ref is treated as a best-effort link failure.

## Configuration: server-side env, not repo config

The instance base URL + token come from `FISHHAWKD_GITLAB_BASE_URL` / `FISHHAWKD_GITLAB_TOKEN` (matching the `FISHHAWKD_JIRA_*` single-instance precedent; secrets cannot live in a checked-in repo config), constructed into a single `*gitlabclient.Client` in `serve.go`. The configurable base URL is what covers both GitLab.com SaaS and self-managed instances.

The per-repo `gitlab` conventions block carries only the non-secret optional `project` override (a namespaced project path). `Target.GitLab` (populated from `conv.GitLab` in `server/workitems.go`) carries the connection to the provider.

## Capability posture

- `File` only. `Transitioner` (#1012), `NumberDiscoverer` (#1269), and `EpicChildrenQuerier` (ADR-047) are **not** implemented in v0 — the capability-asserting hooks yield a no-op, matching the jira sibling.
- Auth deliberately bypasses `forge.CredentialScope` in v0 (`Target.Scope` stays zero for gitlab filings): the client authenticates with the env token like `jiraclient`. Rehoming it onto the credential-scope seam is deferred to the #1855 chain.
