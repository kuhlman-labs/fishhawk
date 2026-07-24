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

`Persist(ctx, Repository, repo, *Assembly)` is a thin sequencing helper (CreateCampaign then CreateCampaignItem per item) so Track C / E25.4 can assemble-and-store.

**Engine** (`engine.go`):

- `NextEligible([]*Item) Eligibility` partitions items into eligible/blocked/running/done/failed from each item's `State`, `DependsOn`, and `RunID`. An item is eligible only when every dependency succeeded; an absent dep ref is treated as not-satisfied, defensively.
- `DeriveState([]*Item) State` reduces item states to the campaign state, emitting only `pending`/`running`/`succeeded`/`failed` — `cancelled` (and the proposal's `paused`) are operator-set overlays owned by Track C, never derived.
