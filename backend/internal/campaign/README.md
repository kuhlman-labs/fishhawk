# backend/internal/campaign

Campaign domain model: object model + persistence (E25.2) and the pure assembly / next-eligible / state-derivation logic (E25.3). ADR-047 / #1437.

## Object model + persistence (ADR-047 / #1437, E25.2: the Track B keystone)

The package mirrors `backend/internal/run/`:

- Domain types in `campaign.go`: `Campaign` (id, repo, epic_ref, state) + `CampaignItem` (id, campaign_id, issue_ref, `depends_on []string`, `run_id *uuid.UUID`, state, and `Expedite` for a hotfix-added item — see "Expedited items and the dispatch hold") + `DispatchHold`.
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

**`campaigns.runner_kind` — the campaign-level runner kind ([#4485](https://github.com/kuhlman-labs/fishhawk/issues/4485)).** `Campaign.RunnerKind` (migration `0113_campaigns_runner_kind`, `TEXT NOT NULL DEFAULT ''`, the `working_dir` shape) records the runner kind every item run minted from the campaign inherits when the per-item call passes none. `''` means NOT RECORDED — exactly one no-value state — and is what every pre-#4485 row reads back; the item-run handler then resolves `local` for a campaign with a `working_dir` binding and `github_actions` otherwise. No CHECK constraint: validation against `run.ValidRunnerKinds` lives in the REST handler (a CHECK would need a migration per new runner kind), and this package carries the value verbatim through `Assembly.RunnerKind` → `CreateCampaignParams.RunnerKind` → both INSERTs (the plain and the grooming-guarded one). The sqlc output is hand-edited; `TestPostgres_CreateCampaign_RunnerKind_RoundTripsEveryReadPath` covers every query returning a campaigns row.

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
- **Fails closed**: any `DroppedEdges` (a mis-targeted/dangling dependency the provider surfaced) yields `*DanglingDependencyError` — a typed error that **wraps** `ErrDanglingDependency` (so `errors.Is(err, ErrDanglingDependency)` still holds) and categorizes the blocking edges by cause: `NotChild` (an out-of-epic/typo'd target — `DropNotChild`, or an unclassified/zero-reason dropped edge, defensively), `ExcludedIncomplete` (an included subset item depending on an excluded, not-yet-complete sibling — `DropExcludedIncomplete`, #2120), `ClosedIncomplete` (an out-of-set target CLOSED but not completed — `DropTargetClosedIncomplete`, #2953) and `StateUnreadable` (an out-of-set target whose state could not be read — `DropTargetStateUnreadable`, #2953). `Error()` renders one clause per non-empty category — the not_child clause keeps the "not a fellow child of the epic" wording, the excluded_incomplete clause names the include-in-items / omit-items remedy, the closed_incomplete clause says the target closed WITHOUT completing so widening cannot satisfy it (reopen/replace/drop), the state_unreadable clause says the state could not be read (retry) — and the handler `errors.As`es the typed form to enrich the `422 campaign_dangling_dependency` details map (`dangling_not_child` / `dangling_excluded_incomplete` / `dangling_closed_incomplete` / `dangling_state_unreadable`). A cycle/out-of-range edge from `plan.Waves` yields `ErrCycle`.
- **A closed-AND-completed out-of-set target is NOT dangling (#2953)**: the provider records it in `res.SatisfiedEdges` (never `DroppedEdges`), and `Assemble` carries it through to `Assembly.SatisfiedDependencies` as a PURE passthrough — no DAG edge or wave dependency is built from it. So a batch whose prerequisite already landed assembles instead of failing closed with unactionable advice. The server renders these into the create-response-only `satisfied_dependencies` block and a best-effort `campaign_dependency_elided` audit.
- **Admission screen is server-side and advisory (#3649)**: `Assemble` still screens dependencies only. The runnability / forbidden-path screen runs in the server (`backend/internal/server/campaign_admission_screen.go`) over the same candidate set AFTER `Assemble` succeeds; it never changes the `Assembly` (no exclusion, no DAG or wave change) and nothing about it is persisted by `Persist` — it rides the create response's `admission_screen` block and one best-effort `campaign_admission_screened` audit.
- **An unresolvable target is NAMED, not rendered `issue:0` (#2956)**: a `DropTargetStateUnreadable` edge from a cross-repo/unparseable `depends_on` token carries `To:0`, so `formatEdges` now renders the target side through the single `workmgmt.DependsEdge.TargetRef()` authority — an operator sees `issue:2032->unparsable:<digest>:"other/repo#12"` in the refusal message instead of the identity-less `issue:2032->issue:0`, and two DISTINCT cross-repo tokens on one item no longer collapse. `TargetRef()` renders `issue:<To>` unchanged for a resolvable edge.

**Subset filter** (`subset.go::FilterToSubset(*workmgmt.EpicChildrenResult, items)`, #2003): an OPTIONAL pre-assembly narrowing that lets an operator scope a campaign to a named subset of an epic's children in one `POST /v0/campaigns` call, instead of filing a shadow epic and re-parenting issues. Pure and fail-closed:

- `items` are issue refs (`N`, `#N`, or `issue:N`, #3314); every ref MUST parse AND resolve to a child in the result. The FIRST miss fails closed naming the ref, and the two causes are DISTINCT sentinels (#2176): a ref that does not PARSE — including a non-positive number and a doubled prefix like `issue:issue:101` — yields a wrapped `workmgmt.ErrInvalidItemRef` → `422 campaign_item_ref_invalid`; a ref that parses but is not among the children yields `ErrItemNotChild` → `422 campaign_item_not_child`. `ErrInvalidItemRef` is the SHARED sentinel the no-epic `IssueSetDependencyResolver` also wraps — both assembly paths parse through the SAME `workmgmt.ParseIssueRef`, so both answer one code for one cause — and a ref that names no issue no longer claims to be "not a child of the epic".
- Children are narrowed to the requested set, preserving ascending order.
- Edges are re-partitioned against the included set: both-endpoints-included edges are kept; an included item whose `depends_on` targets an EXCLUDED item is resolved against that target's completion state (`EpicChild.Complete`): a **closed-and-completed** excluded target is a satisfied dependency, so the edge is elided and **RECORDED as a `SatisfiedEdge`** (#2953 upgraded this from the earlier SILENT drop, so the same elision the full all-children sweep performs is now visible/auditable; `FilterToSubset` also carries any inbound `res.SatisfiedEdges` through and dedups so an edge appears in `SatisfiedEdges` at most once), while an **incomplete** excluded target is appended to `DroppedEdges` stamped `DropExcludedIncomplete` (a dangling dependency — `Assemble` fails it closed as `422 campaign_dangling_dependency`, the same guarantee a cross-epic edge gives). The completion lookup uses the all-children index, so it always resolves; an unexpectedly-missing target fails closed (treated as excluded-incomplete). An edge whose depending item (`From`) is excluded is dropped silently (that item is not in the campaign). A pre-existing `DroppedEdge` carries through only when its `From` is included; a dropped edge from an EXCLUDED item is dropped silently, so a cross-epic dependency on an excluded child no longer blocks the campaign's other children (#2087). **Edges keep their source (#4028).** Every `DependsEdge` / `SatisfiedEdge` carries a `Source` (`native` | `legacy_text`) set by the provider; `Assemble` and `FilterToSubset` never change it — kept and dropped edges pass the provider's edge through (a dropped edge only gains a `Reason`), and the `SatisfiedEdge` `FilterToSubset` creates for an excluded-complete sibling copies the source of the edge it elides.
- Empty/nil `items` returns the result unchanged — the backward-compatible no-op that sweeps every child.

**No-epic campaign variant (`items` WITHOUT `epic_ref`, E48.36 / #2051).** A campaign can also be assembled over an EXPLICIT issue list that shares no epic parent. The `POST /v0/campaigns` handler branches: `epic_ref` present → the `EpicChildrenQuerier` sweep + optional `FilterToSubset` above (byte-identical); `epic_ref` ABSENT + `items` present → the optional `workmgmt.IssueSetDependencyResolver` capability resolves each named issue's `depends_on` marker directly (there is no epic sweep to derive the sibling edge set from), emitting the same `*workmgmt.EpicChildrenResult` `Assemble` consumes. Both branches feed the same `Assemble` → `Persist`. Two recorded decisions:

- **Empty-string `EpicRef` is the no-epic sentinel.** A no-epic campaign assembles with `Assemble("", result)` and PERSISTS `Campaign.EpicRef == ""` (`campaigns.epic_ref` is `TEXT NOT NULL` with no CHECK, so `""` is valid). A future surface that parses `Campaign.EpicRef` as an `issue:N` ref MUST treat the empty string as "no epic" (the items-only variant) rather than choking on it. The campaign status response renders `EpicRef` verbatim, so an empty `epic_ref` in the response is expected and degrades gracefully (no panic / mis-render).
- **The no-epic and epic paths now classify every out-of-set target by reading its state (#2953 — SUPERSEDES the earlier #2120 no-epic difference).** The recorded #2120 decision was that the no-epic path fails-dangling for EVERY out-of-set target, applying no completion refinement. That is RETIRED: both source paths (`ResolveDependencies` and `EpicChildren`) now share one classifier (`workmgmt/github`'s pure `classifyFetchedTarget`, which `EpicChildren` reaches through its `classifyOutOfSetTarget` wrapper and `ResolveDependencies` applies to a bounded-concurrency target prefetch since #3113) that reads each out-of-set target's issue state once per DISTINCT target. A **closed-AND-completed** target is a SATISFIED dependency (`SatisfiedEdges`, elided from the DAG); an **open** target keeps `DropNotChild`; a **closed-but-not-completed** (not_planned/duplicate) target is `DropTargetClosedIncomplete`; an **unreadable** target (GetIssue error, nil issue, or a non-positive/unresolvable ref) is `DropTargetStateUnreadable`. This closes the unactionable-refusal defect where a batch whose prerequisite was already done refused with advice (raise/drop `grooming_order_limit`) that could never include a closed issue.

`Persist(ctx, Repository, repo, *Assembly)` is a thin sequencing helper (CreateCampaign then CreateCampaignItem per item) so Track C / E25.4 can assemble-and-store.

**Engine** (`engine.go`):

- `NextEligible([]*Item) Eligibility` partitions items into eligible/blocked/running/done/failed from each item's `State`, `DependsOn`, and `RunID`. An item is eligible only when every dependency succeeded; an absent dep ref is treated as not-satisfied, defensively.
- `DeriveState([]*Item) State` reduces item states to the campaign state, emitting `pending`/`running`/`awaiting_human`/`succeeded`/`failed` — `cancelled` and `paused` are operator/driver-set overlays owned by Track C, never derived.

### `awaiting_human`: the derived non-terminal state (E72.33 / #3660)

A campaign whose only remaining OPEN items are human-led (`autonomy:low`) had no state to report. `DeriveState` emitted `running` forever, because the auto-driver never dispatches an `autonomy:low` item and no item state implied a stop — so the campaign advertised the truthful `attend_human_led` under a state claiming work was in flight.

**The predicate, and it is an IFF holding at every derive.** `state == awaiting_human` **iff** the `NextEligible` partition has **no** `Eligible`, `Restartable`, `Running` or `Paused` item **and** `>= 1` `HumanLed` item. It is computed from the SAME partition the server distills `next_action` from (`baseCampaignNextAction`), which is what makes the derived state and the advertised action structurally unable to contradict each other — the class of contradiction #2681 exists to prevent.

Each exclusion is load-bearing: a `Running` or `Paused` item means the engine genuinely has work; an `Eligible` item is auto-dispatchable; and a `Restartable` item (deps-satisfied, non-human-led, cancelled-or-failed) has an operator forward path that `baseCampaignNextAction` outranks `HumanLed` with, so deriving `awaiting_human` there would advertise `start_run` under a state claiming only humans are owed work. `Blocked` is deliberately NOT excluded: with none of those four buckets populated, every blocked item is waiting (transitively) on the human-led work, so the campaign is genuinely parked on a human.

**The iff is what makes the state self-correcting rather than sticky**, and the transition table (`transition.go`) carries the reverse edges that let the persisted row FOLLOW the derivation — `deriveCampaignAfterChange` silently DROPS a derivation whose transition the table refuses:

| Edge | Why |
|---|---|
| `pending → awaiting_human` | an all-human-led campaign derives it before any dispatch |
| `running → awaiting_human` | the agent-drivable work finished; the #3660 headline case |
| `awaiting_human → running` | a human-led item was relabelled and STARTED, or a settle made a sibling eligible on a campaign that already HAS progress |
| `awaiting_human → pending` | **the relabel-WITHOUT-start corner.** On a NEVER-STARTED campaign no item has succeeded, run or failed, so after the relabel `DeriveState` falls through to its DEFAULT branch returning `pending`. Without this edge the row STICKS at `awaiting_human` while `next_action`, computed from the same refreshed partition, advertises `start_run`. The status-read path genuinely reaches it: an autonomy refresh alone makes `settleIssueClosedItems` report `refreshedAny`, so `reconcileCampaignItemsOnRead` re-lists and calls `deriveCampaignAfterChange` |
| `awaiting_human → succeeded` | the last human-led item closed-as-completed and settled run-less |
| `awaiting_human → failed` | the last open item settled failed/cancelled, so `anyFailed && allTerminal` holds |
| `awaiting_human → cancelled` | the operator cancel verb |

Deliberately ABSENT: `paused → awaiting_human` (a paused campaign stays sticky; the table refusing it backstops every caller outside `reconcileCampaignItemsOnRead`'s own sticky-paused guard) and `awaiting_human → paused` (the pause/page path only sweeps `running` campaigns, and an `awaiting_human` campaign has no running item to page).

**Why NOT a terminal `succeeded` with a human-led count** — the alternative the originating issue floated, whose rejection the operator ratified at the plan gate. Three reasons, each verifiable in this tree: (1) `computeCampaignNextAction`'s #2681 terminal post-filter rewrites the truthful `attend_human_led` into `closed`, whose detail tells the operator the campaign will not track the issue; (2) `handleStartCampaignItemRun`'s campaign-state gate admits only `pending`/`running`/`failed`/`awaiting_human`, so a terminal-`succeeded` campaign would refuse the start verb and an operator who relabelled the item could never drive it inside the campaign; (3) `ValidCampaignTransition` refuses every edge out of a terminal state, so a remaining human-led item that later settled failed or cancelled could never re-derive and the campaign would report `succeeded` over a failure.

**Honest residual.** An `awaiting_human` campaign leaves `campaigndriver`'s `running`-only sweep. That is BY DESIGN — the engine has nothing to dispatch — and what returns it is the status-read path: the reconcile pass's autonomy refresh plus the `awaiting_human → pending`/`running` edges above. An operator who never polls the status surface and never starts an item sees no movement, which is correct: only a human can move the work.

**Intended behavior change beyond the headline:** an all-human-led campaign that has NOT started now derives `awaiting_human` where it previously derived `pending`. That is the honest reading (the engine will never dispatch any of its items) and is fully reversible on a relabel.

**ROLLBACK, including the CODE-ONLY case.** Migration `0087` only WIDENS `campaigns_state_check`; its down migration rewrites any `awaiting_human` row to `running` BEFORE narrowing, so a full schema rollback strands nothing. `running` is the exact pre-0087 state for the headline case (a started campaign); the ONE exception is a NEVER-STARTED all-human-led campaign, which derived `pending` pre-0087 and lands in `running` — benign (the old driver sweeps it, finds nothing eligible and no-ops) but sticky, since the old table has no `running → pending` edge. **A CODE-ONLY revert — the Go reverted while migration 0087 is left applied — must FIRST normalise existing rows:**

```sql
UPDATE campaigns SET state = 'running' WHERE state = 'awaiting_human';
```

Without it an `awaiting_human` row is STRANDED under the old code: `campaigndriver` sweeps only `running` campaigns, and `handleStartCampaignItemRun`'s pre-#3660 gate admits only `pending`/`running`/`failed` — so the campaign would be neither swept nor startable. The widened CHECK itself is harmless to leave applied (it accepts every value the narrower one did, and nothing writes the new value once the Go is reverted).

## Issue-closed resolution: `resolved_by` and the dropped DAG gate (E72.24 / #3563)

A campaign item can be resolved OUTSIDE the run lifecycle — the issue merged and closed by a maintainer PR, or abandoned as `not_planned` — and the backend's reconcile-on-read pass (`backend/internal/server/campaigns.go::settleIssueClosedItems`) is what recognises that. Three things this package owns:

**The fail-closed closure enumeration.** `classifyClosedIssueOutcome(state, state_reason)` maps the forge's issue state onto the item's terminal state, and ONLY these three pairs settle anything:

| issue state | `state_reason` | item settles |
|---|---|---|
| `closed` | `completed` | `succeeded` — delivered out of band |
| `closed` | `not_planned` | `cancelled` — abandoned, never delivered |
| `closed` | `duplicate` | `cancelled` — likewise a non-delivery closure; the DELIVERING issue is the duplicate's target |

Everything else returns not-a-candidate and leaves the item exactly as it stands: an OPEN issue, a closed issue carrying `reopened`, a closed issue with an EMPTY reason, and any `state_reason` GitHub may add later. The default arm is a CONTROL, not a formality — it is what keeps an unrecognised closure from being read as a delivery, and it is why the change does not depend on GitHub's documented `state_reason` set being exhaustive. `duplicate` maps to `cancelled` rather than `succeeded` deliberately: folding it into `succeeded` would seed the engine's done-set and unblock dependents on work nobody did.

**`Item.ResolvedBy` (`campaign_items.resolved_by`, migration 0085).** The closed value set is `{"" , "issue_closed"}`, pinned by the column CHECK and by `normalizeResolvedBy`. `""` is the unchanged default carried by every pre-0085 row and every non-settle path — an operator cancellation, a run-linked settle, a restart. `"issue_closed"` is stamped by BOTH settle arms, ATOMICALLY with the state write: the not-yet-run arm goes through `SettleCampaignItemForClosedIssue` (one `UPDATE … SET state, resolved_by`), the out-of-band-terminal arm (#2029) through `SettleCampaignItemOutOfBand`, which stamps the same marker while retaining its run link. The single statement is load-bearing: a two-statement settle would leave a window in which the item is already `cancelled` and carries no marker, and in that window `NextEligible` offers it as Restartable.

**Why the marker exists — one control, two refusals.** `NextEligible`'s cancelled arm diverts a deps-satisfied, non-human-led cancelled item into `Restartable` so `next_action` surfaces `start_run`. An issue-closed cancellation must NOT be offered: the work was abandoned at the forge, so there is nothing to restart. Without a durable marker that cancellation is byte-indistinguishable from an operator cancellation, which legitimately IS restartable. The suppression is the conjunct `it.ResolvedBy != ResolvedByIssueClosed` — and because `handleStartCampaignItemRun`'s DAG gate is that SAME `NextEligible` call, the one predicate also makes the operator start verb refuse the item (409) without a second guard.

**The settle carries NO `depends_on` gate — this REPLACES #1558's wave-order rule.** #1558 refused to settle a closed item until every `depends_on` ref had succeeded, to "preserve wave order". That rationale does not hold and is retired: the campaign DAG orders DISPATCH — which item may mint a RUN next — and a settle mints no run. It only recognises an outcome the forge has ALREADY recorded. Holding the recognition back parked a closed item non-terminal for as long as its dependency stayed open, and FOREVER when that dependency was human-led-and-open, cancelled, or itself abandoned (the reported instance parked four days behind its dependency's own settle). So a closed issue settles on the poll that observes the closure, whatever its deps are doing. Two consequences, both intended:

- a dependent of a settled-SUCCEEDED item becomes `Eligible` on the SAME poll, one poll earlier than before. Correct: its declared dependency IS delivered, which is precisely what closed-as-completed records.
- a dependent of a settled-CANCELLED item stays `Blocked`, because a cancelled ref never enters `NextEligible`'s done-set. An abandoned dependency unblocks nothing.

With the gate gone the #1558 in-memory FIXPOINT loop is gone too: it existed solely to converge a closed-C-depends_on-closed-D chain within one read DESPITE the gate, and with no gate to converge against a single pass settles every candidate regardless of iteration order.

**Transition-table note.** `pending → cancelled` and `blocked → cancelled` were already admitted by `campaignItemTransitions` as MANUAL halts; the issue-closed settle is a second, NON-manual producer of those edges. No table change was needed — only the comments naming the new producer.

## Append and the monotone dependency refresh (E86.47 / #4358)

A campaign's DAG is no longer frozen at assembly. Two writers grow it, both through ONE repository transaction, `AppendCampaignItems` (`postgres.go`), which locks the campaign row, then each addition's item row (the package lock order), re-runs `ValidateAppend` under the lock, and calls the caller's `Record` callback before commit — an unrecordable change rolls back whole.

**Append** (`POST /v0/campaigns/{campaign_id}/items`, `fishhawk_append_campaign_items`; `backend/internal/server/campaign_append.go`). Only the NEW refs are resolved on the tracker; `PlanAppend` (`append.go`) maps the result. Refusals, each writing nothing:

- a duplicate member (`ErrDuplicateItem`), decided before the tracker read and again under the lock;
- a malformed ref (`ErrInvalidDependencyRef`: only canonical `issue:N` is ever persisted);
- a NEW item's dependency on a non-member, unless `allow_external_blockers` admits it (the edge is then persisted and HOLDS the item);
- a cycle **only when it passes through a NEW node**. A pre-existing member-only cycle (one a refresh held) is HELD, not refused, so an unrelated append never fails on it.

Existing members are never re-checked on append: their external edges are HELD by the refresh rather than refused on append (`CheckExternal` runs over new nodes only).

**Refresh** (`backend/internal/server/campaign_dependency_refresh.go`). Native blocked-by links and `Depends-on:` lines are re-read for members that have not started (pending/blocked, no run). It is MONOTONE: `MergeDependsOn` only ever adds, so a tracker edit can HOLD an item and never release one — removing a native link or a `Depends-on:` line changes nothing persisted. An edge onto a non-member is held and marked external; an unparsable tracker token is reported and never written; an item that started between the read and the lock is skipped (`SkippedStarted`) and never touched; a cycle the tracker closes among members is held (both items blocked) and named in `cycle_members`. When nothing new is observed there is NO repository call and NO audit row. Each write is recorded before commit as `campaign_dependencies_refreshed`.

| Trigger | Scope | Gate | Tracker read fails | Audit row / write fails |
|---|---|---|---|---|
| `GET …/status` | every unstarted member | caller's account owns the campaign (or untenanted) AND repo visible; single-flight per campaign, 30 s minimum interval (process-local) | read served with a warning; the interval still starts | transaction rolls back; the response still HOLDS the observed blocker (`committed:false`, a warning naming the missing row), and so does every later status read (interval-skipped or tracker-failed) until a refresh records it (process-local, like the gate). A committed edge whose re-list fails is held on the stale list |
| `POST …/runs` (start) | the target item only | same access predicate; not interval-gated | `502 campaign_dependency_refresh_failed`, nothing started | `503 campaign_dependency_audit_failed`, nothing written or started |

A caller failing the access predicate skips the refresh entirely (zero tracker reads, zero writes) and the read or start otherwise behaves as before. `Config.CampaignDependencyRefresh` (set in `serve.go`) is the kill switch: false makes no refresh read or write.

### Releasing a held edge (interim)

The refresh never removes an edge, and an admitted external blocker holds its item by design. The explicit, audited, operator-only edge-drop act (an operator flag on the append verb, pending/blocked items only, recorded before commit) is the remaining, ruling-derived scope of #4358 and is filed separately by the operator. Until it lands, release a held item this way:

1. A wrong edge onto a fellow MEMBER clears on its own once that member succeeds.
2. Otherwise drive the issue standalone with `fishhawk_start_run`, outside the campaign rollup (the campaign will not track it).
3. As a last resort the operator edits `campaign_items.depends_on` by SQL and records the edit by hand as a comment on the campaign's issue (no audit row is written for it):

```sql
UPDATE campaign_items SET depends_on = '["issue:11"]'::jsonb
WHERE campaign_id = '<campaign uuid>' AND issue_ref = 'issue:10' AND state IN ('pending','blocked') AND run_id IS NULL;
```

## Expedited items and the dispatch hold (E83.100 / #4234)

The persistence half of the campaign hotfix verb (`POST /v0/campaigns/{campaign_id}/hotfix`, `backend/internal/server/campaign_hotfix.go`). Pure core in `expedite.go`, hand-written pgx in `expedite_postgres.go`, schema in migration `0106`.

**Tables.** Two child tables, hand-written pgx rather than new `campaign_items` columns, so the sqlc `SELECT *` / `RETURNING *` scans of `campaign_items` stay untouched. Like `campaign_items` they carry no `account_id` and no RLS; tenancy is the parent campaign's, enforced by the caller. Both cascade with their campaign and item.

| Table | Key | Columns | Meaning |
|---|---|---|---|
| `campaign_item_expedites` | `item_id` | `campaign_id`, `kind` (CHECK `hotfix`), `workflow` (CHECK non-empty), `reason`, `created_at` | the item is EXPEDITED; a normal item has no row |
| `campaign_dispatch_holds` | `campaign_id` | `hotfix_item_id`, `held_at`, `expires_at` (CHECK `expires_at > held_at`) | the campaign's ONE active dispatch hold |

**Expedited append.** `AppendCampaignItemsParams.Expedite` (`ExpediteParams{Kind, Workflow, Reason, Hold}`) makes an append expedited. Inside the existing transaction, under the campaign row lock and before `Record`:

1. `ValidateExpedite` (called by `ValidateAppend`, so every fake enforces it too) refuses `ErrExpediteInvalid` unless the append carries exactly ONE new item, NO `depends_on` additions (an expedited item declares no reverse dependencies), kind `hotfix`, a non-blank workflow, and, when a hold is set, a non-zero `held_at < expires_at <= held_at + MaxDispatchHold` (2 h).
2. When a hold is asked, the campaign's hold row is read `FOR UPDATE`; an existing one refuses `ErrDispatchHoldActive` before the first write. A second expedite WITHOUT a hold is admitted during an active hold.
3. The item takes `ExpeditedHeadPosition` = `min(existing queue_position) - 1` (0 on an empty roster), so `ListCampaignItemsForCampaign` lists it first and `NextEligible` offers it first with no engine change.
4. Its `campaign_item_expedites` row is inserted, and the hold, naming it, with the CALLER's `held_at` / `expires_at`.
5. `Record` sees the created item with `Expedite` attached and `AppendOutcome.Hold`; its error rolls back the item, the expedite row and the hold together (`ErrAppendUnrecorded`).

**`queue_position` is no longer dense or non-negative.** An expedited head position is `-1` or lower on any campaign whose items start at 0, and a second expedite goes lower still. Migration `0074` put no CHECK on the column, so this is legal; readers must order by the value, never index by it. The dense 0-based invariant holds only for a campaign that has never had an expedited item.

**Reads.** `GetCampaignItem`, `ListCampaignItemsForCampaign` and `ListCampaignItemsForRun` attach `Item.Expedite` through one `item_id = ANY($1)` query; an expedite read error fails the whole item read, so a caller deciding the hold exemption never mistakes an unreadable expedite for a normal item. The single-row write returns (transitions, run link, restart, settle) do not attach it. `Expedite.Reason` is the operator's free text: untrusted-origin, rendered only as data, never copied into an agent prompt or a promote reason.

**Hold lifecycle.** `GetDispatchHold` is a plain read and returns a hold past its `expires_at` until something releases it (so is `ListDispatchHolds`, every campaign's hold oldest `held_at` first, which the host-dispatch guard reads only for a stage whose campaign ancestry is unresolvable, to hold it while ANY campaign still holds dispatches); the caller decides due-ness with `HoldReleaseReason(hold, hotfixTerminal, now)` against ITS OWN clock: a terminal hotfix wins (`hotfix_terminal`), else `now >= expires_at` is `timeout`. Every stamp is fishhawkd-clock, never Postgres `now()`, so the comparison stays in one clock domain (AGENTS.md trap #3048). `ReleaseDispatchHold` locks the campaign row, then the hold row (the package lock order), deletes the row and calls `Record` before commit:

- no campaign, no hold, or a hold naming a different `HotfixItemID` is an idempotent `(nil, nil)` no-op, so the loser of a concurrent release does nothing;
- a `Record` error rolls the delete back (`ErrHoldReleaseUnrecorded`) and the hold stays active, even past the 2 h cap;
- a nil `Record` (`ErrHoldReleaseUnrecorded`) or an unknown reason (`ErrExpediteInvalid`) is refused before any read.

**Unblocking during an audit outage.** A release that cannot be recorded keeps the hold, so while the audit chain is down the campaign's other implements stay refused, even past `expires_at`. The unblock path is to cancel the hotfix run. That makes the hotfix terminal, and the hold releases at the first evaluation after the audit chain recovers. Do not delete the hold row by hand, because that release would carry no audit row.

**Rollback.** `0106` is additive. Revert the binary first, then migrate down. The down migration drops both tables, an expedited item survives as an ordinary item at its (possibly negative) position, and a held campaign is simply un-held.

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
An expedited item (`0106` / #4234) is the one exception to the assembled order: it
takes `min - 1`, possibly negative, and heads the queue above the ratified ranks.
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

### The currency decision is atomic with the campaign INSERT (E54.17 / #2817)

The pre-flight supersession scan above runs BEFORE work-management provider
resolution — a forge round-trip bounded by a multi-second budget — and then
assembly and persistence. Deciding currency only in that scan leaves a
seconds-wide check-then-act window: a grooming run approved anywhere inside it
would yield a `201` campaign whose source order was already superseded. So the
currency decision is ALSO made INSIDE the campaign row's own INSERT.
`CreateCampaignGuardedByGroomingCurrency` (a SECOND query alongside an untouched
`CreateCampaign`) inserts the row only `WHERE NOT EXISTS` a strictly-newer
approved grooming run of the same `(repo, workflow, account, installation)` as
the source run; a zero-row result maps to `ErrGroomingOrderSuperseded`, which the
handler renders as the SAME `422 grooming_order_superseded` (with a best-effort
`superseded_by` label — an unreadable diagnostic omits the key and keeps the
`422`, never upgrading it to a `502`, because the atomic INSERT already decided).

**State the guarantee honestly.** The guard is evaluated under the INSERT
statement's Read Committed snapshot, so the contract is: any superseding approval
COMMITTED BEFORE THE GUARDED STATEMENT BEGAN prevents creation and leaves no
campaign row; an approval that commits AFTER the statement began is LATER than the
campaign, not a supersession of it. This collapses the window from seconds (a
forge round-trip) to the duration of one statement — it does NOT make the whole
HTTP request one serializable transaction, and it does NOT claim that no approval
can land between predicate evaluation and the row write. A reviewer expecting
whole-request serializability should read this as "the check-to-persist window is
closed", not "the request is a serializable transaction".

The predicate DUPLICATES the Go predicate in `newerApprovedGroomingRun` /
`groomingRunPrecedes` / `sameGroomingTenant` in SQL — the same strict
`(created_at, id)` total order (so the source never supersedes itself) and the
same exact-match tenancy, where `IS NOT DISTINCT FROM` makes an unset account or a
nil installation match only another unset one rather than every row. A future
change to one side must move the other. `allow_superseded` deliberately threads a
NIL guard: re-imposing the check at insert time would refuse exactly the order the
operator acknowledged, and by extension any OTHER newer run that becomes approved
during the request — the widened acknowledgement is the cost of the flag.

RESIDUAL (RLS): migration `0057` puts row-level-security policies on `runs`/`stages`
keyed to the per-transaction `app.account_id` GUC, and `Persist` does not run
inside a tenant transaction. Today the policies are INERT (the runtime role is a
superuser) and the guard scopes by account explicitly regardless; if the runtime
ever moves to the non-superuser role `0057` describes, the guard's subquery could
be filtered to NULL-account runs and would then fail OPEN (miss a superseding run)
rather than closed. This is a known coupling to the RLS rollout, not fixed here.

### The order is read exactly once

At assembly, and nowhere else. Nothing in the campaign engine or the campaign
driver re-reads a grooming report or a board afterwards — a mid-campaign re-read
would silently re-derive a queue the operator already ratified, with no gate and
no audit row. Two independent controls pin it: a counting-fake test in the server
package (exactly one artifact read and one approval list per create, zero across
a later status read and engine partition), and an AST guard over
`backend/internal/campaigndriver`'s non-test sources.
