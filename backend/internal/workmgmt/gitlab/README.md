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

## Campaign sources (`campaign.go`, #3658)

The provider implements BOTH campaign-source capabilities, so `server.campaignSourcesSupported` reports `["epic_ref","items"]` for gitlab (compile-time assertions in `campaign.go` make a signature drift a build failure, not a silent 501):

- `IssueSetDependencyResolver.ResolveDependencies` — items / grooming-order mode over an explicitly-named set. Refs parse through the shared `workmgmt.ParseIssueRef`; a bad ref wraps `workmgmt.ErrInvalidItemRef` (→ 422 `campaign_item_ref_invalid`).
- `EpicChildrenQuerier.EpicChildren` — epic mode over an ordinary issue acting as the epic (Free tier). Premium group-epic refs never reach the provider: the server refuses them 422 `campaign_epic_ref_group_unsupported` first.

Both reuse the github sibling's three-phase bounded-concurrency shape (#3113): PHASE 1 reads every named issue + its links with at most 8 in flight, PHASE 2 reads the DISTINCT out-of-set targets with the same pool, PHASE 3 classifies serially in request order. Workers share no mutable state, so the result is byte-identical regardless of completion order. `ResolveDependencies` honours the `*workmgmt.IssueSetResolutionTimeout` deadline contract (typed timeout in preference to a wrapped fetch error; `Resolved` / `Total` / longest-resolved-prefix `SuggestedLimit`; `Phase` = `fetch_items` / `classify_targets` / `build_result`).

### Mapping decisions and their residuals

- **Epic children = the epic issue's `relates_to` links, confirmed by the `Parent epic:` body marker.** `relates_to` is the reciprocal of the link `File` writes from a child to its `parent_epic`, but it is a GENERIC link, so each candidate's body is checked: no marker → child; a marker naming THIS epic (`#N`, `N`, `issue:N`, trailing period tolerated) → child; markers that name only another epic (or do not parse) → excluded (`ExcludedCandidates{Reason: foreign_parent_marker}`). A CROSS-PROJECT `relates_to` link is excluded WITHOUT being read (`cross_project`) — its iid is scoped to another project and is never reduced to a local number. Excluded candidates are auditable on `EpicChildrenResult.ExcludedCandidates` and deliberately kept OUT of `DroppedEdges` (which assembly fails closed on). **Residual:** a hand-added `relates_to` link to an unrelated issue carrying no marker IS swept in as a child; requiring the marker would exclude every child filed outside Fishhawk. The campaign admission screen (#3649) and the operator gate are the backstop.
- **depends_on = the item's own `is_blocked_by` links.** An in-set target is an edge; an out-of-set target is classified (open → `DropNotChild`, closed → `SatisfiedEdge`, fetch error / nil issue / cross-project → `DropTargetStateUnreadable`, the last without any read). `relates_to` and `blocks` are not edges; duplicate links collapse and a self-link is dropped. **Residual:** `blocks` / `is_blocked_by` are a GitLab **Premium** link type (<https://docs.gitlab.com/ee/user/project/issues/related_issues.html>). On a Free-tier project every link is `relates_to`, so every campaign assembles EDGELESS (all items in wave 0) — a working campaign without dependency ordering, not a failure. The forge-agnostic `Depends on: #N` body marker is not read here (and `File` does not write it).
- **Closed means complete.** GitLab issues carry no `state_reason` (only `state: opened|closed`), so a closed child is `Complete` and a closed out-of-set target is a `SatisfiedEdge` with an EMPTY `StateReason` — there is no `not_planned` close to distinguish. `EpicChild.State` is normalized to `OPEN` / `CLOSED` (an unknown state is `""`, UNKNOWN).

## Capability posture

- `File`, `EpicChildrenQuerier` and `IssueSetDependencyResolver` (above). `Transitioner` (#1012) and `NumberDiscoverer` (#1269) are **not** implemented — the capability-asserting hooks yield a no-op, matching the jira sibling. Because `EpicChildrenQuerier` is now served, the child-number `{n}` allocation for a filing with a `parent_epic` resolves through `EpicChildren`.
- Auth deliberately bypasses `forge.CredentialScope` in v0 (`Target.Scope` stays zero for gitlab filings): the client authenticates with the env token like `jiraclient`. Rehoming it onto the credential-scope seam is deferred to the #1855 chain.

## Work-item read/list: reviewed, not implemented in v0 (#2230 / ADR-064)

The optional `workmgmt.WorkItemReader` capability (read one work item by reference / list a query-scoped set) is **deliberately not implemented here**, matching the jira sibling and this provider's existing posture on `Transitioner` (#1012) and `NumberDiscoverer` (#1269). `workmgmt.ReaderFor("gitlab")` resolves to a typed `*workmgmt.UnavailableError{Reason: ReasonNotImplemented}` — never a nil interface a caller could dispatch against, and never an empty page it could misread as an empty backlog. `TestProvider_DoesNotImplementWorkItemReader` and `TestReaderFor_GitLabResolvesTypedUnavailable` pin both halves.

**The GitLab shape was reviewed before the deferral, and the finding is what pins the interface vocabulary.** GitLab issue boards are LABEL-driven — the board-status label rides the create (see "Board placement → label" above) — so a GitLab reader would derive a work item's board state from its state LABEL, not from a single-select project field as GitHub Projects does. That is precisely why:

- `workmgmt.WorkItemRecord.BoardState` is a CANONICAL state with the PROVIDER owning the mapping, rather than the interface exposing a GitHub "Status field" concept; and
- the canonical → provider-option map travels ON THE REQUEST (`States`, exactly as `TransitionRequest` already carries it) rather than being read from conventions inside a provider.

A GitLab implementation therefore needs **no interface change** — it is deferred purely because v0 has no consumer for it. That is the acceptance point of #2230's fourth criterion: the second implementation is not forced into a GitHub-shaped contract.
