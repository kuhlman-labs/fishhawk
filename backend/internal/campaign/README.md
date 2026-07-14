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
- **Fails closed**: any `DroppedEdges` (a mis-targeted/dangling dependency the provider surfaced) yields `ErrDanglingDependency` (the body-authoritative "a missing dependency fails assembly closed" choice, reconciling the E25.1 forward-note); a cycle/out-of-range edge from `plan.Waves` yields `ErrCycle`.

`Persist(ctx, Repository, repo, *Assembly)` is a thin sequencing helper (CreateCampaign then CreateCampaignItem per item) so Track C / E25.4 can assemble-and-store.

**Engine** (`engine.go`):

- `NextEligible([]*Item) Eligibility` partitions items into eligible/blocked/running/done/failed from each item's `State`, `DependsOn`, and `RunID`. An item is eligible only when every dependency succeeded; an absent dep ref is treated as not-satisfied, defensively.
- `DeriveState([]*Item) State` reduces item states to the campaign state, emitting only `pending`/`running`/`succeeded`/`failed` — `cancelled` (and the proposal's `paused`) are operator-set overlays owned by Track C, never derived.
