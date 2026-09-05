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
| `label_set` | `GetIssue` → `AddIssueLabels` (POST `.../labels`) | **Additive, not a union PATCH — see below.** The payload carries only the labels being added; GitHub merges server-side. A label already present is a provider-side skip, not a write. |
| `epic_link` | `IssueParent` → `GetIssue` → `IssueNodeID` ×2 → `AddSubIssue` → `UpdateIssue(body)` | Idempotence keys on the STRUCTURAL parent (`IssueParent`), NOT the body marker (#2952) — see below. A structural parent = the proposal is a skip; ≠ the proposal is a typed `*ParentEpicConflictError`; NO structural parent takes the write path (`AddSubIssue`, marker stamped only when absent). |
| `depends_on_add` | `GetIssue` → `appendDependsOnRef` → `UpdateIssue(body)` | **ADDITIVE (#2860) — see below.** Merges the ref into an existing `Depends on:` line; only a ref the body ALREADY records is a skip. The filing path's `ensureDependsOnMarker` is untouched and keeps its never-double-stamp contract. |
| `close_duplicate` | `UpdateIssue(state=closed, state_reason=duplicate)` | **Destructive.** |
| `close_not_planned` | `UpdateIssue(state=closed, state_reason=not_planned)` | **Destructive.** Never `completed`, which would misreport a descoped item as delivered work. |
| `board_place` | `ProjectFields` → `IssueNodeID` → `ProjectItemStatus` → `placeIssueOnBoard` | Empty expected-source set ⇒ proceeds only while the item is genuinely OFF-board. |
| `icebox` | the same board path, targeting the conventions' icebox column | **Destructive.** Routed through the SAME placement guard and idempotence check as `board_place` (approval condition I2) — an icebox move must not override a placement a human chose. |
| `field_set` | `ProjectFields("Estimate")` → `SetProjectItemSingleSelect` | The `missing_estimate` hygiene defect's fix. |
| `priority_set` | `ProjectFields("Priority")` → `SetProjectItemSingleSelect` | |
| `rank_set` | `ProjectFields("Rank")` → `SetProjectItemSingleSelect` | **A field write, not a queue reorder — see below.** |

### `label_set` is ADDITIVE because a union PATCH is a lost update

GitHub's issue PATCH replaces `labels` **wholesale**. An add-a-label caller built on it must read the current set and PATCH the union — a read-modify-write whose window two concurrent applies can both enter: each reads the same pre-state, each computes a union missing the other's label, and the later PATCH replaces the earlier one's label away. No local guard closes that, because the losing write is a perfectly well-formed request.

So the write goes through `githubclient.AddIssueLabels` (`POST /repos/{o}/{r}/issues/{n}/labels`), which merges **server-side**: only the added labels cross the wire, and no client ever transmits the full set. The race is removed structurally rather than guarded, and the additive endpoint is exactly the additive operation a hygiene label fix means. The surviving `GetIssue` pre-read is **not** a correctness dependency — it reports the pre-state as `Observed` and turns an already-present label into a skip; a stale read costs at most one redundant additive POST, which GitHub treats as a no-op.

Pinned by `TestApplyGroomingMutation_CompetingLabelAddsBothSurvive`, whose fake models **both** write shapes and forces both racers to read before either writes, so reverting to the union PATCH turns it red on the committed label set — not on an error value.

`AddIssueLabels` is declared as the **optional** `labelAdder` extension of this package's `API` interface, not as a member of it: only this one mutation kind needs the primitive, so promoting it would force every `API` implementation that exercises only the filing and board-sync paths to carry a stub for a capability it never reaches. `*githubclient.Client` satisfies both, so production dispatch is unaffected; an `API` that lacks it is refused before the pre-read with a typed `ReasonNotImplemented` `UnavailableError` rather than falling back to the union PATCH (`TestApplyGroomingMutation_LabelSetWithoutTheAdditivePrimitiveIsRefused`).

### `epic_link` idempotence keys on the STRUCTURAL parent, not the body marker (#2952)

The sub-issue graph is the real relationship, and it is now what the idempotence diff reads. `workmgmt.WorkItemRecord.ParentRef` carries the structural parent, resolved through the **optional** `issueParentReader` extension of this package's `API` interface — the `labelAdder` precedent, for its reason: only the epic-link path needs `IssueParent`, so promoting it into `API` would force every filing/board-sync fake in sibling packages to carry a stub for a primitive it never reaches. `*githubclient.Client` satisfies it (asserted at compile time by `var _ issueParentReader = (*githubclient.Client)(nil)`, so a client signature drift fails the BUILD rather than falling through in production); an `API` that lacks it is refused with a typed `ReasonNotImplemented` `UnavailableError`, **never degraded to the body marker** — the marker is a rendering of the relationship, not the relationship, and a fall-through to it is the exact #2952 defect (`TestApplyGroomingMutation_EpicLinkWithoutTheParentPrimitiveIsRefused`).

Why the change: keying on the marker reported a body-marked-but-structurally-unlinked item (`#819`/`#821`/`#930`) as "already present" and wrote nothing — auditing a genuine REFUSAL as success, and masking that E22 #389 sits at GitHub's hard 100 sub-issue cap. The marker is still WRITTEN (both writes on the link path), because the repository's bodies carry the `Parent epic: #N` convention; it just no longer decides the diff.

`groomingLinkEpic` runs a **five-branch ladder** keyed on the structural parent:

1. structural parent present, **≠** proposal → typed `*ParentEpicConflictError` naming the STRUCTURAL parent; zero writes.
2. structural parent present, **=** proposal → skipped; zero writes.
3. no structural parent, a marker naming a DIFFERENT epic → `*ParentEpicConflictError` (a **STATED RESIDUAL**: the issue's fixtures all carry a matching marker, and #2237's invariant forbids leaving the body claiming one parent while the graph holds another — so this is preserved deliberately, not widened).
4. no structural parent, a marker already naming the proposal → **TAKE THE WRITE PATH** (the `#819`/`#821`/`#930` case): `AddSubIssue` links the missing edge, and the body PATCH is SKIPPED because the marker is already correct.
5. no structural parent, no marker → both writes.

Refs are compared NORMALIZED (`renderParentEpicMarker`, `1437` ≡ `#1437`), so a suggested fix written either way diffs equal.

**Order is deliberate on the write path: link first, marker second.** A marker written before a failed link would claim a relationship that does not exist and would *suppress* the retry. This way a failure between the two is loud (an error, candidate recorded failed) and the next apply re-attempts; the residual is that the re-attempted `AddSubIssue` may be refused by GitHub (a duplicate edge, or the 100 sub-issue cap), which surfaces as a failed candidate rather than as a false "applied".

It **refuses rather than re-parents** in branches 1 and 3 because this provider has no primitive to re-parent with: `linkEpic`'s only edge write is `AddSubIssue`, which ADDS an edge, and the client carries no sub-issue removal and no replace-parent option.

Pinned by, per branch: `..._EpicLinkStructuralConflict` (1), `..._EpicLinkStructuralParentAlreadyLinkedSkips` (2), `..._EpicLinkRefusesADifferentExistingParent` (3), `..._EpicLinkBodyMarkerWithoutTheEdgeStillLinks` (4, the headline #2952 case: `AddSubIssue` called, zero body PATCH), `..._EpicLinkPersistsBothTheEdgeAndTheMarker` (5, with DISTINCT child/parent node ids so the `AddSubIssue` direction is asserted), plus `..._EpicLinkReapplyIsANoOp` (real-provider round trip: the first write's recorded structural edge makes the second call skip), `..._EpicLinkMatchesTheParentAcrossRefShapes` (normalization control), `..._EpicLinkWithoutTheParentPrimitiveIsRefused` (the `ReasonNotImplemented` fail-closed), and the cross-boundary `TestApplyGrooming_EpicLinkBodyMarkedButUnlinkedFailsOnTheCap` (real core → provider → `*githubclient.Client` → httptest forge: a marker-but-no-edge item attempts the write and fails on the forge's own cap message, audited FAILED with the divergent `before={scalar:"", list:["#389"]}`, never `already_applied`).

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

- **Verified locally:** the additive label POST's method, path and exact payload; the `duplicate` / `not_planned` state_reason values are the ones we *send*; the pointer-omission invariant; the epic-link structural-parent round trip (#2952); the board pre-check ordering; every typed refusal; and the end-to-end core → provider → transport seam (`TestApplyGrooming_EndToEndThroughGitHubProvider`), which asserts on the requests an httptest forge actually received.
- **Verified against the fake, not the wire:** the projects-token credential routing on a board write. `groomingMoveCard` passes the *unwrapped* `ctx` to `placeIssueOnBoard`, which re-applies `WithProjectsToken` itself for a user-owned board (#1114) — an asymmetry with `groomingSetField` that reads as a bug from a diff and has been raised as one twice. The httptest fixture accepts either credential and cannot discriminate, so the claim is asserted where it *is* observable: `TestApplyGroomingMutation_BoardWriteUsesTheProjectsToken` reads the context flag recorded by the fake at `AddProjectItem` / `SetProjectItemSingleSelect`, paired with an org-owned case proving the opt-in is scoped rather than unconditional. What remains unproven is that the *real* transport attaches the right bearer for that flag — a `githubclient` property, not a grooming one.
- **Requires live validation:** whether GitHub **honours** the proposed `state_reason` values on close; whether a real board carries `Rank` / `Priority` / `Estimate` as single-select fields with the proposed option names; and whether a re-attempted `AddSubIssue` on an already-linked child is refused or idempotent (the epic-link partial-failure residual above). None is decidable against a fixture, which accepts whatever it is handed.

The acceptance stage could not close that gap when this was written: no run could select the grooming workflow (its non-diff trigger form matched nothing v0 minted), so an acceptance pass on this slice short-circuited with every criterion skipped, as it did on #2234. **That is no longer true as of E54.22 / #2826:** `trigger_source: on_demand` maps to `spec.TriggerOnDemand`, so a grooming run can now be STARTED and GATED. It does not make the live-validation items above decidable against a fixture — they still need a real forge — and it does not close the grooming LOOP: `ApplyGrooming` still has no production caller (#2822), so an approved report applies nothing. This section remains the honest record for the live-validation items.

## `EpicChild.Body` / `EpicChild.URL` and the create-payload marker guarantee (E50.7 / #2064)

`EpicChildren` maps two additive fields off each sub-issue, and both are carried **verbatim**:

- **`Body`** — the child's raw issue body, from the `body` field the `ListSubIssues` GraphQL selection already returned. It is the surface the split-filing forge-state adoption lookup reads (`splitfiling.FindAdoptableChild` asks `workmgmt.BodyHasIdempotencyKey` of it).
- **`URL`** — the child's canonical absolute URL, from the `url` field #2064 **added** to that selection (`githubclient.SubIssue.URL`). It is **never composed** from owner/repo/number: the filed path records `issue.HTMLURL` as the forge returned it and `githubclient.Client.BaseURL` is configurable, so a literal `https://github.com/{owner}/{repo}/issues/{n}` would be wrong on a GitHub Enterprise Server host. The split-filing adoption path records this value, so a composed url would put a URL no operator can follow on the completion marker.

`EpicChildren` applies **no state filter** — a child CLOSED before a re-approval is still returned and therefore still adoptable, because it was FILED and re-filing it would duplicate it. `TestProvider_EpicChildren_PopulatesBodyAndURLVerbatim` asserts both fields and the closed-child case; introducing a `state == "CLOSED"` skip in the mapping loop reddens it (`children = [...], want 2`).

**The create-payload guarantee is OBSERVED on the wire, not inferred.** `TestProviderFile_CreateRequestCarriesIdempotencyMarker` drives the real `Provider.File` against the real `githubclient` against the httptest mux, with a body rendered by the real `workmgmt.Apply` from a `FilingRequest` carrying an `IdempotencyKey`, and asserts the marker is present in the **create request body the fake server received** — after `ensureDependsOnMarker`'s body rewrite (the fixture declares a `depends_on` so the rewrite genuinely runs) and the JSON request assembly, and exactly once. This is the seam a serialization bug would live in; every other case in #2064 watches either a body handed to a fake provider or an `EpicChildren` OUTPUT. Deleting the stamping call in `workmgmt.Apply` reddens it.

## Out-of-set target classification (#2953)

Both campaign source paths — `EpicChildren` (the epic sweep) and `ResolveDependencies` (the no-epic/grooming item set) — share ONE definition of the classification rules for every `depends_on` target OUTSIDE the assembled set, so "satisfied" means exactly one thing across the codebase. Since #3113 that definition is the PURE `classifyFetchedTarget(issue, err)`, which makes no forge call and consults no cache. The two paths reach it differently, because they fetch differently: `EpicChildren` calls the `classifyOutOfSetTarget(ctx, scope, repo, ref, cache)` wrapper, which keeps the no-forge-call guards, the memoizing `cache` and the inline `GetIssue`; `ResolveDependencies` fetches its targets in a bounded pool and then applies `classifyFetchedTarget` to the already-fetched result, reading it back through `lookupTargetState` (see "Three-phase issue-set resolution" below). The rules themselves are identical and fail closed by construction:

- an UNRESOLVABLE ref (a cross-repo `owner/repo#N` token, an owner-qualified ref, or garbage) and a non-positive `number` each stamp `DropTargetStateUnreadable` **without any `GetIssue` call** — calling `GetIssue` with the wrong target could read an unrelated issue's state and FALSELY satisfy the edge. Both guards exist on BOTH paths: `classifyOutOfSetTarget` for the epic sweep, and `needsTargetFetch` (which never enqueues such a ref for a read) plus `lookupTargetState` (which answers it without a cache entry) for the issue set. `TestNeedsTargetFetchRefusesUnresolvableRef` / `TestLookupTargetStateAnswersNoForgeCallRefsWithoutCache` assert them on a SYNTHETIC ref carrying `Resolvable=false` with a POSITIVE number — a shape `parseDependsOnMarker` cannot produce today (an unresolvable token always carries number 0), which is exactly why a through-the-parser test cannot serve as these guards' counterfactual vehicle;
- a `GetIssue` **error** → `DropTargetStateUnreadable` (never satisfied);
- a **nil** issue with no error → `DropTargetStateUnreadable` (a distinct guard from the error branch, so each is independently deletable);
- `closed` AND `completed` (case-insensitive; `GetIssue`'s REST payload is lowercase) → **satisfied**, appended to `EpicChildrenResult.SatisfiedEdges` carrying the observed state — the SAME closed-AND-completed rule `EpicChild.Complete` encodes;
- `closed` with any other `state_reason` (not_planned/duplicate) → `DropTargetClosedIncomplete`;
- anything else (open) → `DropNotChild`.

The `cache map[int]targetState` is created once per resolution call, so N references to one closed target cost ONE `GetIssue`, and a batch with no out-of-set edges makes ZERO extra calls (the pre-#2953 API-call profile for the common case is unchanged). That still holds after #3113 — the issue-set path fetches a DISTINCT, first-encounter-ordered target set rather than filling the cache opportunistically — and is now pinned by `TestResolveDependenciesTargetFetchedOncePerDistinctNumber` rather than only asserted here. `EpicChildren` classifies only targets ABSENT from `ListSubIssues` (true non-children); a closed-and-completed FELLOW child stays an in-set `Edge`, and the excluded-fellow-child elision is recorded by the campaign `FilterToSubset` layer instead — so the closed-complete fellow-child edge has exactly one owner (no double-recording in `SatisfiedEdges`). This SUPERSEDES the pre-#2953 contract (`DropNotChild` for every out-of-set target); it exists so a campaign whose prerequisite already landed assembles instead of refusing with advice that could never include a closed issue. Pinned by `TestResolveDependencies*`, `TestEpicChildrenOutOfEpicTargetClassification`, `TestClassifyOutOfSetTargetInvalidRefFailsClosed`, and the memoization/no-extra-read call-count tests in `provider_test.go`.

## Three-phase issue-set resolution (#3113)

`ResolveDependencies` is the no-epic campaign source, and a full ratified grooming order is sixty-plus items. Resolved serially it cost one round-trip per named issue plus one per distinct out-of-set `depends_on` target — ~60-100 sequential REST calls — which is why campaign assembly from a full order could not finish inside a caller's timeout. `EpicChildren` was never the cost: it reads its whole sibling set in ONE `ListSubIssues` call, so it is deliberately left serial and its output is byte-identical (pinned by `TestEpicChildrenByteIdenticalUnderJitter`).

The resolution runs in three phases:

| Phase | What it does | Concurrency |
|---|---|---|
| 0 | Parse each `Items` ref via `parseIssueRef`, dedup into `numbers` preserving REQUEST ORDER (ratified rank order on the grooming path) | serial |
| 1 | Fetch every named issue | bounded, `issueSetFetchConcurrency` (8) |
| 2 | Collect the DISTINCT out-of-set targets in first-encounter order (`outOfSetTargets`), fetch them, build `stateCache` | bounded fetch, SERIAL cache build |
| 3 | Classify in request order, emit `Children`/`Edges`/`DroppedEdges`/`SatisfiedEdges`, sort | serial |

**No shared mutable state crosses a goroutine boundary, and there is no mutex.** A worker's only output is an `issueFetch` value sent on a buffered channel; the parent places each into an ordinal-indexed slice after the pool drains, and every map and slice write in the whole function is the parent's. Go's runtime throws on concurrent map writes *regardless of key*, so distinct keys would not have made a shared cache safe — the design avoids the question rather than guarding it. This is what makes the emitted `*workmgmt.EpicChildrenResult` **byte-identical to the serial result regardless of completion order**, pinned by `TestResolveDependenciesByteIdenticalUnderJitter` (50 repetitions under a fake that jitters per-call ordering, each `json.Marshal`ed and compared to the first AND to a hand-written serial golden). The 8-way bound is well under GitHub's published no-more-than-100-concurrent-requests REST guidance, and `TestResolveDependenciesConcurrencyBounded` pins it STRUCTURALLY in both directions: a non-blocking send on a capacity-8 slot channel records any 9th concurrent call as a violation (at most 8), while an arrivals channel plus a release closed after 8 arrivals proves at least 8 was reached (a serial implementation never releases, and an abort channel unblocks it so the test FAILS rather than deadlocks).

### Deadlines and the typed timeout

Every phase checks `ctx.Err()` **before** returning any wrapped fetch error and returns `*workmgmt.IssueSetResolutionTimeout` instead. Without that precedence a deadline would surface as `workmgmt/github: get issue #N: context deadline exceeded` — a deadline presented as a provider fault, which is exactly the misdiagnosis this change closes.

The counts use ONE uniform accounting rule (`issueSetTimeout` / `itemFullyResolved`), evaluated at whichever phase the deadline hit:

- **`Resolved`** counts items whose resolution is COMPLETE — the item's own fetch succeeded AND every out-of-set target it names is classified. An item naming no out-of-set target is fully resolved as soon as its fetch completes. A partially-resolved item is never counted.
- **`SuggestedLimit`** is the length of the longest fully-resolved **PREFIX** of the request order, and `0` for no suggestion. A prefix is what provably would have fit, because `grooming_order_limit` takes the top-N by ratified rank and this resolver preserves that order. It is deliberately conservative and will often suggest less than the true maximum — a suggestion that provably fits, or none. `TestResolveDependenciesSuggestedLimitPrefixOnly` pins that a non-prefix resolved set yields `0`, not `2`.

### Two distinctions that are easy to conflate

- **A context-TERMINATED target fetch is not an ordinary error.** An aborted fetch is never cached and never counts its item resolved; an ordinary 404/permission error still caches `DropTargetStateUnreadable` exactly as the serial path did, so #2953's meaning of "unreadable" is NOT widened. The paired `TestResolveDependenciesAbortedTargetNotCachedNotResolved` / `TestResolveDependenciesOrdinaryTargetErrorStillClassifies` drive the SAME target through both, so the distinction is context-termination and not error-vs-success.
- **A phase-1 fetch error is returned; a phase-2 one is not.** A named item the forge refuses fails the whole resolution (there is no meaningful result without it), and the error returned is deterministically the one FIRST IN REQUEST ORDER — the parent walks the ordinal-indexed slice ascending, so no worker's finishing order decides it. `TestResolveDependenciesFetchErrorFirstInRequestOrder` uses a DESCENDING `[20, 10]` fixture on purpose: on ascending input first-in-request-order and lowest-numbered coincide and the claim is invisible. A phase-2 (target) fetch error is a classification input, not a failure.

A named item the forge returns neither an issue nor an error for fails closed naming the item (`get issue #N: no issue returned`). Before #3113 that case dereferenced a nil issue and panicked.

Phase 3 carries one defensive branch: a target that reaches classification UNCLASSIFIED (absent from the cache because its fetch was context-terminated) returns the typed timeout rather than guessing. Guessing would either invent a satisfied dependency or fabricate an unreadable one, and both are worse than an honest refusal.

## Unresolvable-token identity (#2956)

A `DropTargetStateUnreadable` edge from an UNRESOLVABLE `depends_on` token (a cross-repo `owner/repo#N`, an owner-qualified ref, or garbage) carries `To:0`, so before #2956 every such edge rendered `issue:From->issue:0` and the per-item dedup key `(From,0)` collapsed two DIFFERENT cross-repo tokens into one edge — WHICH token vanished. `parseDependsOnMarker` now gives every unresolvable token an IDENTITY, threaded onto `workmgmt.DependsEdge` as `ToRefDigest`/`ToRef`:

- **Identity relation.** A token's CANONICAL form is its whitespace-trimmed, PRE-sanitization text (`dependsOnTokenCanonical` = `strings.TrimSpace`). Two tokens sharing a canonical form ARE one token and correctly collapse to one edge — the #2953 condition-3 dedup, preserved on purpose. Distinct canonical forms are distinguished by `dependsOnTokenDigest`: the first 16 hex chars (64 bits) of SHA-256 over the canonical form. For k distinct canonical tokens on one item the birthday-bound collision probability is ~k²/2⁶⁵ (~2.7e-16 at k=100), so the identity claim is explicitly PROBABILISTIC, not absolute — the digest is an operator-facing DIAGNOSTIC, never an authorization or satisfaction control.
- **Digest BEFORE sanitize.** The digest is taken over the canonical form; `sanitizeDependsOnToken` (control-rune/`U+FEFF` stripping, whitespace collapse, 64-rune bound with an ellipsis, `""` when nothing printable survives) is a DISPLAY transform applied AFTER, never before, so it can never merge two distinct canonical tokens onto one digest. The sanitized form rides out as `ToRef` (the raw token is untrusted issue-body content and never reaches a message unsanitized).
- **Dedup key** is now `dependsEdgeKey{From, To, Digest}`: two DISTINCT cross-repo tokens on one item produce two edges; a repeated IDENTICAL canonical token still collapses; a resolvable numeric ref carries an empty digest so its key reduces to `(From, To)` as before. `sortEdges` gains a `ToRefDigest` tiebreak so a set of `To:0` edges with distinct digests has a fully-determined order.
- **`dependsOnRefRE` is UNCHANGED.** The anchored `^\s*#?([1-9]\d*)\s*$` is the control that keeps a cross-repo ref from being reduced to a local number and read locally (`TestResolveDependenciesCrossRepoRefFailsClosed` / `TestEpicChildrenCrossRepoRefFailsClosed` still pass). #2956 makes the token VISIBLE (named in the operator refusal via `workmgmt.DependsEdge.TargetRef`) without making it RESOLVABLE. Pinned by `TestParseDependsOnMarker_*`, `TestResolveDependencies_TwoDistinctCrossRepoTokens_ReportedSeparately`, `TestSortEdges_ThirteenUnresolvableEdges_DigestOrdered`, and the EpicChildren mirrors in `provider_test.go`.

## The depends_on amend path is additive, and CRLF-aware (#2860)

`ensureDependsOnMarker` returns ANY marker-bearing body unchanged. That is
CORRECT for the FILING path it was written for (E34.3 / #1594): filing re-sends
an item's whole declared `depends_on` set, so a body already carrying a marker
already carries them. It is WRONG for the grooming AMEND path, which adds ONE
new edge to an item that may already record others — every SECOND approved edge
out of an item was refused, and the refusal was reported as
`depends_on marker already present`, indistinguishable from an idempotent no-op.
A measured 0/8 apply rate survived three grooming walks that way.

The amend path is `appendDependsOnRef`, a SEPARATELY NAMED sibling — two named
helpers, not one helper with a mode flag, so neither path can silently acquire
the other's behaviour. It returns `(newBody, changed)` and `changed` is never
true for a body that did not change:

- A ref that is not a reference at all (empty, whitespace, a bare `#`) is a
  no-op returning `changed == false`. BOTH body shapes matter: with a marker the
  splice would append a meaningless `, #`; WITHOUT one the delegation below
  returns the body unchanged (`renderDependsOnMarker` skips the token) while
  still reporting a change — a body that did not change, audited as applied.
- A ref the body already records is the genuine idempotent no-op.
- A marker-free body delegates to `ensureDependsOnMarker`.
- Otherwise the ref is spliced into the FIRST marker line's captured value.

**The splice is trailing-CR aware.** Go's `(?m)$` anchors immediately before a
`\n` only — it does not treat `\r\n` as one terminator — and `.` matches every
byte but `\n`, including a carriage return. So on a CRLF body
`dependsOnMarkerRE`'s `(.+)` capture ENDS WITH the CR, and appending at the raw
capture end would emit `#1641<CR>, #2032` and corrupt the line. The CR is
trimmed before appending and re-emitted after, preserving the line ending byte
for byte. Nothing outside the capture group is rewritten, so surrounding prose,
other marker lines and the final newline are untouched.
`TestAppendDependsOnRef_MergesIntoExistingMarker` and
`..._PreservesCRLFLineEndings` assert the ENTIRE resulting body, byte for byte.

**One normalizer decides ref SHAPE, across both packages.** `renderDependsOnMarker`,
`dependsOnMarkerRefs` (the membership read, which walks EVERY marker line the way
the core's `groomingMarkerObserved` does) and `appendDependsOnRef` all route
through `workmgmt.NormalizeIssueRef`. #2860 is a defect of two layers disagreeing
about whether a ref is already recorded, so a second normalization function here
— even one believed to agree — would rebuild it one level down. The render path's
empty-after-strip guard (`dependsOnRefStripped`) is NOT a competing normalizer: it
decides whether a token is emitted at all, never what shape it takes.
`NormalizeIssueRef("#")` returns the trimmed original `"#"`, which is non-empty,
so that guard must stay or a bare `#` would start being emitted as a reference.

One consequence is deliberate: a NON-NUMERIC ref on the filing path now persists
as written (`other/repo#1639`) rather than hashed (`#other/repo#1639`), which is
what makes the write and the later membership read agree. `parseDependsOnMarker`
classifies both shapes identically as `dependsOnRef{Resolvable: false}`, so
#2953's out-of-set classifier is unaffected and previously-filed bodies stay
readable.

## `manual_placement_preserved` and `not on board` are REFUSALS, not skips

Both are requested writes this provider DECLINED — nothing changed and nothing
was already correct — so since #2860 they return `Refused: true` with a
`RefuseReason` rather than `Skipped`. `labels already present`,
`parent epic already linked`, `already at target column` and
`depends_on ref already present` stay SKIPS: those are states the provider
OBSERVED as already-satisfied. The core reports the same refusal for its own
pre-dispatch placement guard, so the audit reads the same whichever layer
noticed first.
