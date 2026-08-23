# backend/internal/campaign

Campaign domain model: object model + persistence (E25.2) and the pure assembly / next-eligible / state-derivation logic (E25.3). ADR-047 / #1437.

## Object model + persistence (ADR-047 / #1437, E25.2: the Track B keystone)

The package mirrors `backend/internal/run/`:

- Domain types in `campaign.go`: `Campaign` (id, repo, epic_ref, state) + `CampaignItem` (id, campaign_id, issue_ref, `depends_on []string`, `run_id *uuid.UUID`, state).
- Two state machines in `transition.go`, governed by transition tables:
  - campaign: `pending → running → {succeeded,failed,cancelled}`
  - item: `pending → blocked → running → {succeeded,failed,cancelled}` (`blocked` = depends_on edges unsatisfied)
  - Refused edges surface `InvalidTransitionError` (kind `campaign`/`campaign_item`).
- The `Repository` interface (`repository.go`) + postgres adapter (`postgres.go`, sqlc-generated `db/` from `queries.sql`) carries the same FOR-UPDATE transition atomicity as `run.Repository`; `fake.go` is the embeddable `BaseFake`.

**`SetCampaignItemAutonomy(itemID, autonomy)` (E25.20 / #2355)** overwrites an item's `autonomy` tier — routing metadata, NOT lifecycle state — so a relabelled child's fresh tier can be folded back in by the reconcile-on-read refresh instead of the assembly-time snapshot going stale. It is deliberately NOT a state transition: a single `UPDATE` with NO `FOR UPDATE` lock (autonomy participates in no state machine). It **normalizes the input first** (`normalizeAutonomy`) to the CHECK-permitted set (`""`, `low`, `medium`, `high`), so an out-of-set tier persists as `""` (the unknown/default) rather than tripping the migration-0049 column CHECK. Idempotent on an unchanged value; `ErrNotFound` for a missing item. Modelled on `SetCampaignItemRun`. The generated `db/queries.sql.go` method is hand-applied (per `AGENTS.md`, a local `sqlc generate` would churn out-of-scope packages) and pinned against the real DB by `TestSetCampaignItemAutonomy_*` in `postgres_test.go`.

**`ReopenCampaignForItemRestart(campaignID, itemID)` (E67.43 / #2681)** is the recovery primitive for a campaign whose LAST unsettled item failed. `DeriveState` returns `failed` for `anyFailed && allTerminal`, so such a campaign goes TERMINAL while that item is still deps-satisfied and restartable — and a terminal campaign refuses every recovery verb, which made the item unrecoverable inside the campaign. This method flips the campaign `failed → running` AND resets the item `{cancelled|failed} → pending` with its `run_id` cleared.

- **ONE transaction, BOTH rows locked.** The pair is applied inside a single `pgx.BeginFunc` holding `SELECT … FOR UPDATE` locks on the campaign row and THEN the item row. That order is the package-wide lock order (no other method in `postgres.go` locks an item before a campaign inside one transaction — `RestartCampaignItem`, `SettleCampaignItemOutOfBand` and `PauseCampaignItem` lock only the item; `TransitionCampaign` locks only the campaign), so it introduces no lock-order cycle. The single transaction is load-bearing, not cosmetic: `reconcileCampaignItemsOnRead` runs on EVERY status read and a `running` campaign whose every item is still terminal derives straight back to `failed`, so two separate writes would expose exactly that window.
- **It lives OUTSIDE the transition tables.** `transition.go` refuses every transition out of a terminal `from`, for the campaign AND the item, because a reopen-for-restart is an operator recovery rather than a lifecycle transition. So, like `RestartCampaignItem`, it enforces its own guards: missing campaign → `ErrCampaignNotFound`; campaign state ≠ `failed` → `InvalidTransitionError{Kind:"campaign"}` (a `paused`, `cancelled`, `succeeded`, `running` or `pending` campaign is NEVER reopened — cancellation and success are verdicts this verb must not undo, and a non-terminal campaign needs no reopen); missing item → `ErrCampaignItemNotFound`; item owned by another campaign → `ErrCampaignItemNotFound` (from THIS campaign's perspective it does not exist); item state outside `{cancelled, failed}` → `InvalidTransitionError{Kind:"campaign_item"}`.
- **On ANY guard failure NOTHING is written** — including the case where the campaign guard PASSED and the item guard then rejected, which is the assertion `TestReopenCampaignForItemRestart_RejectsNonRestartableItem` uses as the atomicity proof (a two-writes implementation would already have committed the campaign flip).
- **`ErrCampaignNotFound` / `ErrCampaignItemNotFound` refine `ErrNotFound`** (they wrap it, so every existing `errors.Is(err, ErrNotFound)` caller is unaffected). They exist so the handler can report a campaign-shaped miss under a campaign-shaped code instead of folding all three miss causes into an item-shaped 404.
- **Relationship to the siblings.** `RestartCampaignItem` stays the path for a still-pending/running campaign, where only the item needs resetting; this is its campaign-plus-item counterpart. `SettleCampaignItemOutOfBand` is the third member of the guard-bypassing family — it settles (rather than restarts) a delivered item and RETAINS the run link.

All five sqlc queries it composes (`LockCampaignForUpdate`, `UpdateCampaignState`, `LockCampaignItemForUpdate`, `SetCampaignItemRun`, `UpdateCampaignItemState`) already exist in `queries.sql`, so no `sqlc generate` was needed. Pinned against the real DB by `TestReopenCampaignForItemRestart_*` in `postgres_test.go`.

**`campaigns.working_dir` — the campaign-level checkout binding (E48.87 / [#2527](https://github.com/kuhlman-labs/fishhawk/issues/2527)).** `Campaign.WorkingDir` (migration `0071_campaigns_working_dir`, `TEXT NOT NULL DEFAULT ''` — the exact shape `runs.working_dir` took in 0065) records the absolute checkout every item run minted from the campaign inherits, so the operator binds it ONCE at `fishhawk_start_campaign` instead of re-typing an identical path on every `start_campaign_item_run` call. `NOT NULL DEFAULT ''` rather than nullable keeps the Go mapping a plain `string` and makes "no binding" exactly ONE value; every pre-#2527 row reads back unbound, which is the unchanged-behavior default (each item run then falls back to #2498's explicit per-item `working_dir`).

The column carries the value VERBATIM: this package neither validates nor normalizes it. Absolute-path validation is transport- and runner-kind-conditional, so it lives above the database — in `server.handleCreateCampaign` (a 400 at create) and again in `handleStartCampaignItemRun` (the inherited value is re-validated, so a binding written straight to the row cannot bypass the gate by arriving through a different door). There is deliberately no CHECK constraint for the same reason.

It threads `Assembly.WorkingDir` → `CreateCampaignParams.WorkingDir` → the column, and reads back through the ONE shared `rowToCampaign` mapper, so the binding is visible on every campaign read path. The generated `db/` code is hand-applied (per `AGENTS.md`, a local `sqlc generate` churns out-of-scope packages) across all SIX campaigns-table statements; `TestPostgres_CreateCampaign_WorkingDir_RoundTripsEveryReadPath` reads the value back through each of them — including `LockCampaignForUpdate` driven directly, so its own column list and `Scan` are load-bearingly covered — because a hand edit's failure mode is a column list updated in some statements but not all.

**The run ↔ campaign cross-boundary link is a nullable `campaign_items.run_id` FK to `runs` (ON DELETE SET NULL)** — a campaign's issue-runs are discoverable via the item rows without touching the hot `runs` table. Reverse discovery ("which campaign owns this run") is `ListCampaignItemsForRun` over the `campaign_items_run_idx` index. `SET NULL` (not `CASCADE`) preserves campaign history when a run is deleted.

Migration `0039_campaigns.{up,down}.sql` creates `campaigns` + `campaign_items`, reusing the shared `fishhawk_set_updated_at()` trigger.

No driving yet in the E25.2 keystone (that lands E25.3+): the keystone delivers persistence + the validated state machine only. A run-side `campaign_id` pointer is an additive follow-on if ever needed.

## Assembly + next-eligible engine + state derivation (ADR-047 / #1437, E25.3: the campaign brain)

`assembly.go` + `engine.go` are pure logic over the E25.1 epic-children result and the E25.2 item rows — no `Repository` dependency, so unit-testable without Postgres.

**Assembly** (`assembly.go::Assemble(epicRef, *workmgmt.EpicChildrenResult)`):

- Maps each child issue number to an ascending 0-based index.
- Builds a `plan.Decomposition` whose `SubPlanSummary[i].DependsOn` carries the indices of child `i`'s depends_on targets (edge `{From,To}` ⇒ item `From` depends on `To`).
- Reuses `plan.Waves` for the topological sort, and maps the `[][]int` waves back to `[][]string` `issue:N` refs — REUSING the wave engine rather than reimplementing Kahn.
- **Fails closed**: any `DroppedEdges` (a mis-targeted/dangling dependency the provider surfaced) yields `*DanglingDependencyError` — a typed error that **wraps** `ErrDanglingDependency` (so `errors.Is(err, ErrDanglingDependency)` still holds) and categorizes the blocking edges by cause: `NotChild` (an out-of-epic/typo'd target — `DropNotChild`, or an unclassified/zero-reason dropped edge, defensively) and `ExcludedIncomplete` (an included subset item depending on an excluded, not-yet-complete sibling — `DropExcludedIncomplete`, #2120). `Error()` renders one clause per non-empty category — the not_child clause keeps the "not a fellow child of the epic" wording, the excluded_incomplete clause names the include-in-items / omit-items remedy — and the handler `errors.As`es the typed form to enrich the `422 campaign_dangling_dependency` details map (`dangling_not_child` / `dangling_excluded_incomplete`). A cycle/out-of-range edge from `plan.Waves` yields `ErrCycle`.

**Subset filter** (`subset.go::FilterToSubset(*workmgmt.EpicChildrenResult, items)`, #2003): an OPTIONAL pre-assembly narrowing that lets an operator scope a campaign to a named subset of an epic's children in one `POST /v0/campaigns` call, instead of filing a shadow epic and re-parenting issues. Pure and fail-closed:

- `items` are issue refs (bare number or `issue:N`); every ref MUST resolve to a child in the result. The FIRST ref that is not a child (or is unparseable) yields `ErrItemNotChild`, which the handler maps to `422 campaign_item_not_child`.
- Children are narrowed to the requested set, preserving ascending order.
- Edges are re-partitioned against the included set: both-endpoints-included edges are kept; an included item whose `depends_on` targets an EXCLUDED item is resolved against that target's completion state (`EpicChild.Complete`): a **closed-and-completed** excluded target is a satisfied dependency, so the edge is dropped **silently** (the same result the full all-children sweep produces via closed-child auto-settle, #2120), while an **incomplete** excluded target is appended to `DroppedEdges` stamped `DropExcludedIncomplete` (a dangling dependency — `Assemble` fails it closed as `422 campaign_dangling_dependency`, the same guarantee a cross-epic edge gives). The completion lookup uses the all-children index, so it always resolves; an unexpectedly-missing target fails closed (treated as excluded-incomplete). An edge whose depending item (`From`) is excluded is dropped silently (that item is not in the campaign). A pre-existing `DroppedEdge` carries through only when its `From` is included; a dropped edge from an EXCLUDED item is dropped silently, so a cross-epic dependency on an excluded child no longer blocks the campaign's other children (#2087).
- Empty/nil `items` returns the result unchanged — the backward-compatible no-op that sweeps every child.

**No-epic campaign variant (`items` WITHOUT `epic_ref`, E48.36 / #2051).** A campaign can also be assembled over an EXPLICIT issue list that shares no epic parent. The `POST /v0/campaigns` handler branches: `epic_ref` present → the `EpicChildrenQuerier` sweep + optional `FilterToSubset` above (byte-identical); `epic_ref` ABSENT + `items` present → the optional `workmgmt.IssueSetDependencyResolver` capability resolves each named issue's `depends_on` marker directly (there is no epic sweep to derive the sibling edge set from), emitting the same `*workmgmt.EpicChildrenResult` `Assemble` consumes. Both branches feed the same `Assemble` → `Persist`. Two recorded decisions:

- **Empty-string `EpicRef` is the no-epic sentinel.** A no-epic campaign assembles with `Assemble("", result)` and PERSISTS `Campaign.EpicRef == ""` (`campaigns.epic_ref` is `TEXT NOT NULL` with no CHECK, so `""` is valid). A future surface that parses `Campaign.EpicRef` as an `issue:N` ref MUST treat the empty string as "no epic" (the items-only variant) rather than choking on it. The campaign status response renders `EpicRef` verbatim, so an empty `epic_ref` in the response is expected and degrades gracefully (no panic / mis-render).
- **The no-epic path fails-dangling for EVERY out-of-set target (deliberate #2120 difference).** The resolver stamps every `depends_on` target outside the named set as `DropNotChild`, so `Assemble` fails it closed as `422 campaign_dangling_dependency`. Unlike the epic path after #2120 — which treats an excluded-but-CLOSED+COMPLETED dependency as SATISFIED — the no-epic path does NOT apply the completion-satisfied refinement (there is no epic child set to derive an out-of-set target's completion state from). This is a recorded decision, revisitable as a follow-up, not an inconsistency.

`Persist(ctx, Repository, repo, *Assembly)` is a thin sequencing helper (CreateCampaign then CreateCampaignItem per item) so Track C / E25.4 can assemble-and-store.

**Engine** (`engine.go`):

- `NextEligible([]*Item) Eligibility` partitions items into eligible/blocked/running/done/failed from each item's `State`, `DependsOn`, and `RunID`. An item is eligible only when every dependency succeeded; an absent dep ref is treated as not-satisfied, defensively.
- `DeriveState([]*Item) State` reduces item states to the campaign state, emitting only `pending`/`running`/`succeeded`/`failed` — `cancelled` (and the proposal's `paused`) are operator-set overlays owned by Track C, never derived.

## The third campaign source: an approved grooming order (E54.6 / #2238)

`POST /v0/campaigns` accepts three mutually-exclusive sources: an `epic_ref` to
decompose, an explicit `items` list (#2051), and — new here — a
`grooming_source` naming an **approved grooming run** whose ratified priority
order becomes the campaign queue. The combination of `grooming_source` with
either of the others is a `400`, not a precedence rule: a silent winner among
three sources is how an operator gets a batch they did not ask for.

**The derivation is pure, and it lives here.** `groomingorder.go` holds
`OrderFromReport` (report → rank-ordered issue numbers) and `ReorderByPriority`
(permute a resolved `EpicChildrenResult` into that order) — no I/O, no provider,
no clock, the same two-layer split `grooming_apply.go` uses. The server-side
ladder (run/artifact/approval reads, tenancy, ratification, supersession) lives
in `backend/internal/server/campaign_grooming_source.go`.

`OrderFromReport` sorts ASCENDING by rank, and converts an entry only when its
`item_ref.type` is `github_issue` and its id parses as `<owner>/<repo>#<number>`
matching the target repo case-insensitively. Every other entry is EXCLUDED with
a NAMED reason (`not_github_issue`, `other_repo`, `unparseable_id`) carried on
the result — never dropped silently, because an invisibly-truncated batch is the
failure this surface exists to prevent. Two entries resolving to one issue number
fail closed. `limit` caps the CONVERTIBLE entries and records `OmittedByLimit`
separately from `Excluded`: a capped entry WAS convertible, so conflating the two
would make the omitted count underivable.

### Ratification is the stage approval gate, not a per-entry decision set

#2237 shipped `workmgmt.ApplyGrooming` with its seam deliberately unwired, so no
production path writes a `grooming_mutation_applied` row and a per-entry
disposition read would resolve empty for every report. The shipped ratification
mechanism is the `backlog_grooming` workflow's `groom` stage approval gate. An
APPROVED order is therefore: the `grooming_report` artifact of a plan stage
carrying at least one `approval.DecisionApprove` and ZERO `DecisionReject`. A
rejection alongside an approval is a CONTESTED gate, not a ratified one.

### Rank order becomes queue order, durably

This is the headline behaviour, and it needed a schema change to be true.

`campaign.Assemble` stamps each item's `Position` from its index in
`res.Children`, `Persist` threads it onto `campaign_items.queue_position`
(migration `0074`), and `ListCampaignItemsForCampaign` orders by that column
first. So permuting `Children` with `ReorderByPriority` before assembly is what
makes the ratified order the order items are CREATED in — and therefore the order
the engine's `Eligible` slice, the `#1816` `campaign_started` → Up Next board
sweep, and every later read see.

**Insertion sequence is not an ordering.** Before `0074` the listing ordered by
`(created_at, id)` while `Persist` inserted every item with a `now()`-defaulted
timestamp. `Persist` is not transactional, so those timestamps were not identical
— but they were only *usually* increasing: two inserts inside one clock tick share
a `created_at` and the order is then decided by a RANDOM UUID. A queue meant to
carry a ratified priority order cannot rest on that, so the order is written down.
The column is not unique and the `(created_at, id)` tiebreak is retained, so every
pre-`0074` row (which carries the `DEFAULT 0`) lists in exactly its prior order.
`campaign/postgres_test.go` proves this against REAL Postgres — including a case
whose queue positions CONTRADICT insertion order, so it cannot pass by timing luck.

Permuting `Children` does NOT perturb the DAG: `Assemble` builds its child-index
map by iterating the actual slice rather than assuming ascending numbers, so a
permutation changes only the creation order (`TestReorderByPriority_PreservesWaves`).

### Provenance is on the campaign row, not only on the audit chain

Every grooming-sourced campaign carries `campaigns.grooming_source` (JSONB,
migration `0074`): source run/stage/artifact ids, the report content hash, the
ordered refs, the named exclusions, the applied limit and any acknowledged
supersession. It is written by the campaign's OWN single-row INSERT, so there is
no window in which such a campaign exists unprovenanced.

That placement is deliberate. `Persist` is not transactional (one
`CreateCampaign`, then N `CreateCampaignItem`), and the
`campaign_grooming_source_resolved` audit emit is best-effort and runs AFTER
persistence — so an audit-only record could be lost while the campaign survived.
The audit entry still exists (an operator can await the category), but the column
is the system of record, and it is echoed on every read of the campaign, not just
on the create response.

### Supersession fails closed, in two distinguishable ways

An order that a NEWER approved grooming run of the same workflow has superseded
is REFUSED `422 grooming_order_superseded`, naming the newer run.
`allow_superseded` converts THAT refusal — and only that one — into an explicit,
RECORDED acknowledgement, because it names a run the operator can look at and
decide about.

The scan pages the workflow-scoped run list until it sees a SHORT page — positive
proof it reached the end — and only then reports absence. If it exhausts its page
budget first it reports `422 grooming_order_supersession_undetermined`, and that
refusal is UNCONDITIONAL: `allow_superseded` does not reach it. An incomplete
scan names nothing to acknowledge, so letting a caller-set request flag stand in
for it would turn the flag into a bypass of an authorization-shaped check rather
than an acknowledgement of a known state.

A read FAILURE is a third, distinct outcome: `502
grooming_order_supersession_unreadable`, which `allow_superseded` deliberately
does NOT bypass. An operator can knowingly accept a stale order; nobody can
acknowledge a read that did not happen.

This is the opposite posture to `groomingChurnBaseline`, and deliberately so:
that guard is a SUPPRESSOR, so an unreadable baseline makes it propose. This one
is authorization-shaped, so it refuses.

### The order is read exactly once

At assembly, and nowhere else. Nothing in the campaign engine or the campaign
driver re-reads a grooming report or a board afterwards — a mid-campaign re-read
would silently re-derive a queue the operator already ratified, with no gate and
no audit row. Two independent controls pin it: a counting-fake test in the server
package (exactly one artifact read and one approval list per create, zero across
a later status read and engine partition), and an AST guard over
`backend/internal/campaigndriver`'s non-test sources.
