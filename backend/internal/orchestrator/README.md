# backend/internal/orchestrator

Stage orchestrator: next-stage dispatch after approve. Called from the approval handler on approve; dispatches the next pending stage (or transitions the Run to terminal when all stages are done). Agent stages fire `workflow_dispatch`; human stages walk to `awaiting_approval` directly.

## Auto-merge stages (#255 / ADR-017)

Review stages with a check-only gate (`gate.Kind == 'check'`) take a third path — `dispatchAutoMergeStage` calls `githubclient.EnableAutoMerge` (REST GET `/pulls/{n}` for the node id, then GraphQL `enablePullRequestAutoMerge` mutation, default `SQUASH`) against the run's `pull_request_url` and walks the stage straight to `succeeded`.

Fishhawk's role is "queue and step out of the way"; GitHub's auto-merge machinery handles the actual merge once branch protection clears. Failure to enable (auto-merge disabled on the repo, etc.) leaves the stage in `dispatched` and surfaces the error so a fresh `Advance` call retries — same idempotency posture as `workflow_dispatch`.

## Decomposition fan-out (#455 / ADR-025 D4)

When the next pending stage is `implement` and the approved plan declares `decomposition.sub_plans`, the orchestrator mints one child run per sub-plan (carrying `parent_run_id = parent.id` + `decomposed_from = parent.id` + an issue_context derived from the sub-plan's `scope_hint`), parks the parent's implement stage in the `awaiting_children` state, and emits a `plan_decomposed` audit entry naming the child IDs.

Children themselves skip the fanout check (a non-nil `decomposed_from` short-circuits the path), so recursion is bounded at one level.

**Existing-children idempotency guard (#1063)**: before minting, `fanoutIfDecomposed` probes `ListRuns(DecomposedFrom==parent, Limit:1)`; if the parent already has children (a fix-up re-open or a sweeper double-advance), it skips the fan-out and returns `(false, nil)` so `Advance` re-dispatches the parent implement stage against the existing shared branch instead of re-minting — only the first fan-out (zero children) mints.

Requires both `Artifacts` and `Audit` dependencies on the Orchestrator struct; with either nil the fanout is silently disabled and the implement stage dispatches as today.

Decomposed children are routed onto a shared branch (`fishhawk/run-<shortParentID>`) by the runner and CLI via the `decomposed_from_run_id` field on the prompt response — see "Pull-request artifact upload chain" in `backend/internal/server/README.md` for the branch-sharing protocol.

## Concurrent child dispatch (E24.3 / ADR-041 / #1143)

`DispatchDecomposedChildren` dispatches a parked parent's pending children — instead of leaving them for serial operator drive — up to the resolved concurrency cap.

- It lists ALL children (`listAllDecomposedChildren`), partitions them pending / in-flight / terminal, resolves the cap (`resolveEffectiveMaxParallel`), consumes `budget.ParallelDecision(pending+in-flight, cap)`, and dispatches `Allowed - in-flight` headroom children in ascending `slice_index` order via the existing runner-kind-aware `Advance` (per-backend dispatch mechanics stay owned by E24.4 local / E24.5 Actions).
- Each dispatched child records a `children_dispatch` `run_auto_advanced` entry via the nil-safe `Drive` engine (see `backend/internal/drive/README.md`).
- It is wired at THREE points, all best-effort (a dispatch error never unwinds the parked parent): inline at the end of `fanoutIfDecomposed` (initial dispatch), event-driven in `maybeAdvanceDecomposedParent` (refill — as in-flight children settle, the next pending ones dispatch to hold the active count at the cap), and the `childcompletion` sweeper's `resolveParent` not-all-terminal branch (the fail-closed backstop, via the nil-safe `ChildDispatcher` interface so `childcompletion` stays orchestrator-free).
- Idempotent + soft-cap: in-flight children are counted from current state so re-entrant/overlapping calls bound to the cap and `Advance` same-state transitions no-op; a benign one-slot overshoot in a tight race never strands or double-runs a child.

### Local-child park contract (#1980)

A decomposed child of a **runner_kind-locked-local** parent must park its implement stage at `awaiting_host_dispatch` — NOT the legacy `dispatched` — so `fishhawk_run_children` (whose dispatchable predicate is `{pending, awaiting_host_dispatch}` and which treats `dispatched` as in-flight) can host-spawn it. The subtlety the fix addresses: `run.CreateRunParams` has **no `RunnerKindResolved` field**, so every child row is minted runner_kind-**UNRESOLVED** with `RunnerKind` copied from the parent. The `#1912` park branch keys on the RESOLVED lock (`RunnerKindResolved && RunnerKind == local`), which never holds for a fresh child — so pre-#1980 the child fell through to `dispatched` + a silent local no-op `fireDispatch`, deadlocking `run_children` by construction (run 780f1bb6).

Since E45.7 (ADR-058 / #1851) this lineage decision lives in [`runnerbackend.Resolver`](../runnerbackend/README.md): `dispatchStage` calls `o.backends().Resolve(ctx, r)` once and keys the park on the resolved backend's `HostDispatched()` (local → park; github_actions → fire via `TriggerStage`), replacing the former `runLockedLocal` + `fireDispatch` pair with no behavior change. For an unresolved decomposed child (`DecomposedFrom != nil`) whose inherited `RunnerKind` is `local`, the resolver consults the parent's lock via `GetRun(*r.DecomposedFrom)`:

- parent RESOLVED local → local backend → park (`awaiting_host_dispatch`);
- parent RESOLVED non-local → the inherited local hint was superseded → github_actions backend → `dispatched` + `TriggerStage`;
- parent read errors OR parent itself unresolved → **fail toward the recoverable state**: local backend (park) and WARN. `awaiting_host_dispatch` is CAS-recoverable with one host-dispatch verb (`server/host_dispatch.go` admits `{pending, awaiting_host_dispatch} → dispatched`), whereas a wrongly-fired `workflow_dispatch` is an unrecoverable external side effect (#1355).

A `github_actions` child (inherited kind not `local`) resolves to the github_actions backend and fires its `workflow_dispatch` byte-identically; a resolved top-level run keeps the exact `#1912`/`#1346` behavior. `Local.TriggerStage` is the residual defensive locked-local skip the old `fireDispatch` carried.

## Actions decomposed-child dispatch (E24.5 / #1145)

For the `github_actions` backend the concurrent dispatch above is realized through the [`runnerbackend.GitHubActions`](../runnerbackend/README.md) backend's `TriggerStage` (formerly `fireDispatch`) — each child auto-advances and fires its OWN `workflow_dispatch` carrying its own `run_id`/`stage_id` against the base ref (`o.DefaultRef`, fallback `main`), bounded by the same `DispatchDecomposedChildren` cap.

The runner — NOT the dispatch — derives the sole-writer slice branch `fishhawk/run-<parent>/slice-<idx>` by fetching `decomposed_from` + `slice_index` from the stage-details endpoint keyed by `run_id`; because each child's `run_id`/`stage_id` are distinct, the per-slice checkouts push to distinct slice-branch names that cannot collide.

NO new `workflow_dispatch` input is added — GitHub rejects inputs not declared in the customer-side `.github/workflows/fishhawk.yml` with a 422 "Unexpected inputs provided", and the existing `run_id`/`stage_id` inputs already suffice; the dispatch carries structured `slice_index`/`decomposed_from` log fields for observability only.

The customer-side `fishhawk.yml` `concurrency:` group (a `.github/workflows/**` runner-capacity guard that bounds the customer's Actions runner pool) is a separate, human-led (`autonomy:low`) change tracked as an operator-filed follow-up, OUT OF SCOPE here.

## Park-on-recoverable (#698 / #1081)

The event-driven parent-resolution hook `maybeAdvanceDecomposedParent` (fired from `completeRun` on every child terminal transition) classifies each failed child's implement-stage failure via `run.ImplementFailureRecoverable` (which wraps `run.RecoverableInDecomposition` = `RetryableFailure || category B`).

When children failed but EVERY failed child's failure is recoverable in decomposition (category A/C, a D SLA timeout, or category B via the in-place recover path — #1081 / `backend/internal/run/README.md`), it leaves the parent parked in `awaiting_children` instead of resolving it to `failed-C`, and emits a one-time `parent_awaiting_redrive` audit (system actor, payload `{parent_stage_id, retryable_child_run_ids}`) so an operator can re-drive the recoverable child without racing the resolution.

The auto-retry / `retry_stage` path is UNCHANGED — it still consults `run.RetryableFailure` directly and keeps refusing B; only this parent-park gate broadened to B (#1081).

A genuinely non-recoverable failed child — a D-rejection (approver reject), or a child whose stages can't be listed or whose implement stage carries no category — resolves the parent to `failed-C` (park only when every failure is positively confirmed recoverable, so an unclassifiable child resolves rather than parking indefinitely).

## Consolidated PR on settle (#714 / ADR-032)

In `Advance`, when the next pending stage is `review`, the run is a decomposed parent (`decomposed_from == nil` AND it has decomposed children), and `pull_request_url` is empty, `maybeOpenConsolidatedPR` opens the ONE consolidated PR for the whole decomposition BEFORE dispatching review.

- Head = the consolidated branch `fishhawk/run-<first8(parentID)>-consolidated` (the `consolidatedBranch` helper; a NON-NESTING sibling of the slice branches, renamed under #1243 — see "Fan-in integration" below for the D/F-conflict rationale).
- Base = `o.DefaultRef` (fallback `main`; NOT `TriggerRef`, which is an `issue:NNN` string); title/body from the run's `issue_context`.
- It stamps `pull_request_url` via `SetRunPullRequestURL` so the existing merge reconciler resolves the review on the consolidated PR's MERGE — ADR-031's verified-landing invariant holds (the parent reaches `succeeded` only on merge, never at PR-open).
- Idempotency is load-bearing because the periodic sweeper and the event-driven `maybeAdvanceDecomposedParent` both finish by calling `Advance`: an empty-URL re-read shrinks the double-open window, and a `githubclient.ErrPullRequestExists` (422-duplicate) recovers the already-open PR's URL via `ListOpenPullRequestsByHead` rather than failing the settle.
- Emits a best-effort `consolidated_pr_opened` audit (system actor).
- Graceful-skip (parent stays PR-less, same posture as the github_actions backend's `TriggerStage` skip on a nil client / unwired installation) when the run has zero children, `o.GitHub == nil`, or `installation_id` is nil — narrowing rather than regressing prior behavior.

The consolidated PR's head branch is PRODUCED by the fan-in step below (under E24.1/#1141 each child pushes only its own slice branch and nobody creates the consolidated branch).

## Fan-in integration (ADR-041 / E24.2 / #1142)

`integrateSlices` runs on the all-children-SUCCEEDED settle path — invoked from BOTH `maybeAdvanceDecomposedParent` (event-driven) and `childcompletion.resolveParent` (sweeper) BEFORE the `awaiting_children` stage is stamped succeeded — and sequentially merges each succeeded slice branch `fishhawk/run-<shortParent>/slice-<n>` (the `sliceBranch` helper, kept in sync with the runner's `childSliceBranch`) onto the consolidated branch `fishhawk/run-<shortParent>-consolidated` in ascending `slice_index` order via server-side git merges.

**The consolidated branch is the NON-NESTING `-consolidated` sibling of the slice branches, NOT `fishhawk/run-<shortParent>` (#1243)**: git stores refs as a filesystem-like hierarchy under `.git/refs/heads`, so a ref whose full path (`refs/heads/fishhawk/run-<short>`) is a strict prefix of an existing slice ref's path (`refs/heads/fishhawk/run-<short>/slice-0`) cannot be created — the directory/file (D/F) conflict that 422'd `CreateRef` (and thus fan-in) 100% in production.
`runBranchPrefix(id)` is the shared `fishhawk/run-<short>` namespace; `sliceBranch` nests under it (byte-identical to the runner, which is UNCHANGED) while `consolidatedBranch` appends `-consolidated` so the two never nest.

- Backed by three REST primitives — `githubclient.GetBranchSHA` / `CreateRef` / `MergeBranch` (GET `/git/ref/heads/{branch}`, POST `/git/refs`, POST `/merges`).
- Resolves the base sha from `o.DefaultRef` (fallback `main`), creates the consolidated branch from it when absent (`CreateRef`'s 422 "already exists" is a benign idempotent no-op), then merges each slice (a `204`/already-contained is an idempotent no-op so a re-entrant settle is clean).
- A `409` merge conflict (`githubclient.ErrMergeConflict`) returns a STRUCTURED `*SliceConflict` (conflicting slice index + child run id); the settle path then fails the parent implement (`awaiting_children`) stage **category-B RECOVERABLE** with a stable `slice integration conflict: …` reason prefix and emits a `slice_integration_conflict` audit whose payload carries `conflicting_slice_index` + `conflicting_child_run_id` — the machine resume target `next_actions` reads back (never parsed from the reason string).
- A clean fan-in emits `slices_integrated` (payload `{child_run_ids, consolidated_branch, slice_count, integration_commit_shas}`, consumed by E24.7) and falls through to the succeeded transition + `Advance`, which opens the consolidated PR off the now-integrated branch.
- The decomposed-children listing **paginates to completion** (`listAllDecomposedChildren`, `integrateSlicesPageSize`) so a fan-out exceeding one page can never silently integrate only the first page.
- A non-conflict GitHub error leaves the stage parked (the next tick/retry re-enters; merges are idempotent).
- Graceful-skip (same posture as `maybeOpenConsolidatedPR`) when `o.GitHub == nil`, `installation_id` is nil, or there are zero succeeded slices.
- `IntegrateSlices(ctx, parentRunID)` is the exported wrapper the sweeper's adapter calls.

### Incremental merge-SHA recording for ADR-035 lineage (#1459 / #1806)

Each `Integrate slice N` merge SHA is recorded the INSTANT the commit is created via a dedicated ledger-only `integration_commit_recorded` audit entry (payload `{merge_sha, slice_index, child_run_id, consolidated_branch}`, on the PARENT's own chain).

This is durable across BOTH terminal-only-emit gaps: a partial pass that merges some slices then bails early (a later slice's `*SliceConflict` or a non-conflict GitHub error — neither reaches the terminal `slices_integrated`), and a re-entrant pass that sees the already-created merges as `204` no-ops (empty SHA, skipped).

The server-side ADR-035 lineage guard (`backend/internal/server/lineage.go::buildReportedHeadLedger`) unions BOTH `integration_commit_recorded.merge_sha` AND `slices_integrated.integration_commit_shas` into the reported-head ledger, so a fix-up on the consolidated parent attributes the integration merges instead of flagging them foreign; `slices_integrated` stays the clean-integration signal (no downstream classifier disturbed) and fail-closed is preserved (a commit in neither category still flags).

The #1806 false positive predated #1775 (which only changed the consolidated PR title/body and added no commit) — the gap was the pre-existing terminal-only recording, not that regression.

## Startup run-completion recovery (#727)

`ReconcileStuckRuns(ctx)` is a one-shot self-heal called from `serve.go` at boot (gated only on `Orchestrator != nil && RunRepo != nil`, best-effort/non-fatal).

It pages `ListRuns(State=running)` and, for any run whose stages are ALL terminal (`StageState.IsTerminal()`) but is itself non-terminal, calls `Advance` → `completeRun` to resolve it to `succeeded`/`failed`/`cancelled`. Skips any run with a non-terminal stage so a genuinely in-flight run is never force-completed; idempotent (an already-terminal run is a `completeRun` no-op, a re-run finds nothing).

Reuses existing repo methods only (no new query) — the recovery for the `{all stages terminal, run non-terminal}` class the merge-resolution bug produced.
