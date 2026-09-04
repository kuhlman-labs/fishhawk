# backend/internal/orchestrator

Stage orchestrator: next-stage dispatch after approve. Called from the approval handler on approve; dispatches the next pending stage (or transitions the Run to terminal when all stages are done). Agent stages fire `workflow_dispatch`; human stages walk to `awaiting_approval` directly.

## Credential-scope resolution in `triggerParams` (E45.22 / #2043)

`triggerParams` maps a run + its next stage onto the forge-neutral `runnerbackend.TriggerParams`. Its `Scope` comes from `runCredentialScope`, which is a LADDER, not a switch:

1. A non-nil, NON-EMPTY `run.InstallationRef` wraps verbatim via `forge.FromRef`. This is the only arm that can produce a GitLab scope (`gitlab:<project_id>`); it also covers a BACKFILLED GitHub row, whose ref is the bare base-10 decimal and therefore yields a scope byte-equal to arm 2's.
2. A nil ref — or a ref recorded as the EMPTY STRING, a distinct persisted state that names no credential — falls back to `run.InstallationID` via `forge.FromGitHubInstallationID`. Every legacy pre-0076 GitHub row stays on exactly today's behaviour.
3. Neither present → the zero scope, which `runnerbackend`'s `Scope.IsZero()` guard (`gitlabci.go`, `githubactions.go`) reads as "unwired" and warn-skips. Unchanged.

**Why the ladder exists.** Before #2043 this read ONLY `InstallationID`, so a `gitlab_ci` run — which has no GitHub installation id — resolved the zero scope and every one of its stages warn-skipped without ever firing a pipeline (HIGH 1 of #2043). `TestTriggerParams_GitLabRefReachesCreatePipeline` drives the whole seam with NO scope substituted by the test: a run carrying `installation_ref` `gitlab:5` reaches `POST /api/v4/projects/5/pipeline`, and the same run stripped of both credentials issues no request at all. `TestTriggerParams_ResolvesCredentialScopeFromInstallationRef` covers one cell per branch, including the ref-wins-over-a-disagreeing-installation-id cell that makes the ORDERING an assertion rather than a coincidence.

`Ref` is unchanged: the run's ADR-035 sole-writer branch from `runBranchRef`, which the `gitlab_ci` backend creates its pipeline against and the `github_actions` backend ignores.

## Auto-merge stages (#255 / ADR-017)

Review stages with a check-only gate (`gate.Kind == 'check'`) take a third path — `dispatchAutoMergeStage` calls `githubclient.EnableAutoMerge` (REST GET `/pulls/{n}` for the node id, then GraphQL `enablePullRequestAutoMerge` mutation, default `SQUASH`) against the run's `pull_request_url` and walks the stage straight to `succeeded`.

Fishhawk's role is "queue and step out of the way"; GitHub's auto-merge machinery handles the actual merge once branch protection clears. Failure to enable (auto-merge disabled on the repo, etc.) leaves the stage in `dispatched` and surfaces the error so a fresh `Advance` call retries — same idempotency posture as `workflow_dispatch`.

## Decomposition fan-out (#455 / ADR-025 D4)

When the next pending stage is `implement` and the approved plan declares `decomposition.sub_plans`, the orchestrator mints one child run per sub-plan (carrying `parent_run_id = parent.id` + `decomposed_from = parent.id` + an issue_context derived from the sub-plan's `scope_hint` + the parent's `working_dir` binding, inherited at mint so the child's stage verbs resolve the same checkout the parent's branch lineage is anchored to — E48.100 / [#2547](https://github.com/kuhlman-labs/fishhawk/issues/2547)), parks the parent's implement stage in the `awaiting_children` state, and emits a `plan_decomposed` audit entry naming the child IDs.

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

### `dispatchStage` reports the park; the acceptance anchor keys on it (E64.53 / #3174)

`dispatchStage` returns `(Outcome, parked bool, error)`. `parked` is true ONLY on the `backend.HostDispatched()` branch above — the one that transitions a locked-local agent stage to `awaiting_host_dispatch` and returns `(OutcomeDispatched, nil)` with **no spawn having happened**. Every other return (auto-merge stage, human walk, agent `dispatched` + `TriggerStage`, and every error path) reports `parked=false`.

`Advance` reads it to gate the `acceptance_dispatched` emit: `err == nil && !parked && next.Type == acceptance`. Rationale — a parked stage was never spawned, so an anchor written there names a dispatch that may never happen and, once `server/host_dispatch.go`'s marker writes its own anchor at the moment of the spawn, would be a SECOND anchor for the same validation episode. The duplicate misreports the dispatch count on the living-anchor timeline and shifts the review→dispatch latency boundary (`internal/latency` keys on this category). The LOCAL anchor is therefore owned solely by the host-dispatch marker, which is what makes a fix-up re-open's RE-dispatch advance it (`reopenAcceptanceOnFixupPush` never calls `Advance`). Pinned by `TestAdvance_AcceptanceStage_LocalLocked_ParksAndEmitsNoAnchor` (zero entries on the park) alongside the unchanged `TestAdvance_AcceptanceStage_DispatchesAndEmits` (exactly one on the github_actions path).

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

## Park-on-child-scope-decision (E48.101 / #2548)

The SAME hook covers a second parked-parent cause, on a different branch: a decomposition child parked in `awaiting_scope_decision` for a build-required scope-drift shortfall. `awaiting_scope_decision` is in `StageState.IsSettled()` and excluded from `IsTerminal()`, so the child stage is not swept by the SLA reaper and the child RUN stays NON-terminal — it lands in `maybeAdvanceDecomposedParent`'s non-terminal branch, which tops up the dispatch and returns with the parent still in `awaiting_children`.

**The parent deliberately STAYS parked.** Resolving it to `failed-C` while a live child holds an UNDECIDED park would terminate the run out from under the very decision the park exists to offer, leaving a terminal parent with a live child. This is the #698/#1081 recoverable-park shape: parked parent plus one discoverable audit signal.

**What changed is that the wait is no longer silent.** `surfaceParkedChildren` emits `parent_awaiting_child_scope_decision` (system actor) on the PARENT for each such child, with the FULL payload `{parent_stage_id, child_run_id, child_stage_id, child_slice_index, shortfall_class, build_required_paths, owning_slices}` read from the child stage's persisted `ScopeCompletenessPark`. The attribution is the operator value: it names the sibling slice that owns each coupled file. The no-duplicate property — at most one entry per `(parent run, child stage)` — is enforced at the store layer by migration 0067's partial unique index across BOTH this emitter and the park-time one in `server/build_required_park.go` (0067 / #2594); the pre-read of `ListForRunByCategory` against the parent's existing entries is now a best-effort FAST PATH that avoids a doomed insert in the common case, not the correctness mechanism. On a genuine race the loser's `AppendChained` hits the index and is treated as the benign already-recorded outcome (INFO, marked recorded, continue to the next child) via `audit.IsParentAwaitingChildScopeDecisionDuplicate`. An INFO log names the parked child so the parked parent is diagnosable from logs too. The dispatch top-up and the return are unchanged.

**The emission is DUAL, and it has to be.** This hook is reached ONLY from `completeRun` on a child's TERMINAL transition. A parked child does not complete — so if the parked child is the LAST non-terminal sibling, this hook never fires again. `server/pullrequest.go::parkScopeCompletenessStage` therefore emits the same category at PARK TIME; an orchestrator-only fix would leave that case with nothing on the parent's chain.

**Once the operator decides `fail`**, the child stage fails category-B, the child run goes terminal, and the parent resolves through the existing unchanged path (`failed-C`, or the #1081 recoverable re-drive park). The parent never sits in `awaiting_children` past the decision.

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

## Between-wave fan-in (#2363)

`IntegrateCompletedWave(ctx, parentRunID) (integrated bool, conflict *SliceConflict, err error)` is the fan-in for a WAVE boundary rather than the settle: it merges the already-succeeded slice branches onto the consolidated branch **while the fan-out is still in flight**, so a dependent wave's children can spawn against a base carrying their predecessors' commits.

It exists because that re-base used to be the CLIENT's job — the blocking `fishhawk_run_children` driver POSTed `/integrate-wave` between waves — so a fan-out with no client alive never advanced past a wave boundary. The child-completion sweeper calls it from the NOT-all-terminal branch, immediately before its dispatch backstop (see `backend/internal/childcompletion/README.md`).

It **never** transitions a stage and **never** calls `Advance`. Settling a decomposed parent stays owned by the all-terminal paths.

Predicate — a merge is attempted only when ALL THREE hold:

1. at least one child is in run state `succeeded` (there is something to merge);
2. at least one NON-TERMINAL child's slice declares a non-empty `depends_on` whose EVERY dependency slice has a minted sibling in state `succeeded` (a dependent wave is genuinely unblocked); and
3. **the steady-state short-circuit** — that dependent's dependencies are NOT already covered by the parent's newest `slices_integrated` entry. Without (3) the predicate stays true for the whole time a dependent child runs and the sweeper would re-merge on EVERY tick for the rest of the fan-out. Idempotency makes that safe, but the remote git load is real and unnecessary.

(3) is evaluated with `backend/internal/wavecoverage.Covered` — the SAME shared predicate the host-dispatch marker admits on and the MCP await verb releases on, deliberately not a second reconstruction (the duplicated-reconstruction drift class the `SliceBranch`/`childSliceBranch` note above already names). The coverage set is `child_run_ids` off the NEWEST `slices_integrated` entry, which is the complete merged set at that moment because `integrateSlices` always merges every succeeded slice ascending on each pass.

Degrades to `(false, nil, nil)` with a WARN, never an error, on ABSENT input: a nil plan or a nil `Decomposition`. Malformed CHILD metadata — ANY child with a nil `SliceIndex`, or a `slice_index` out of range for `sub_plans` — is a **WHOLE-CALL fail-closed** degrade ([#2695](https://github.com/kuhlman-labs/fishhawk/issues/2695) item 3), NOT a per-child skip: the classification is hoisted into the first pass over EVERY child so it is total, and any malformed child makes the call integrate **nothing** this tick. Skipping a malformed child while a valid sibling still drove a merge would merge against a slice map provably missing a child; failing closed keeps the base honest (`wavecoverage.Covered` cannot be trusted on an incomplete map). It stays a no-op (never an error), so the sweeper keeps the parent parked and the next tick re-enters once the metadata is fixed. A plan-LOAD error or a child-listing error propagates as `err` (retryable). An AUDIT-READ failure degrades to NOT-covered, i.e. it INTEGRATES: the merge is idempotent, so a spurious extra pass costs one round trip, whereas a spuriously skipped integration would strand the next wave behind the host-dispatch `409 wave_not_integrated` until the following tick.

The merge itself is the existing `IntegrateSlices` primitive — the same one `/consolidate` and `/integrate-wave` use — so conflict provenance, pagination, incremental merge-SHA recording and idempotency are inherited rather than reimplemented.

## Startup run-completion recovery (#727)

`ReconcileStuckRuns(ctx)` is a one-shot self-heal called from `serve.go` at boot (gated only on `Orchestrator != nil && RunRepo != nil`, best-effort/non-fatal).

It pages `ListRuns(State=running)` and, for any run whose stages are ALL terminal (`StageState.IsTerminal()`) but is itself non-terminal, calls `Advance` → `completeRun` to resolve it to `succeeded`/`failed`/`cancelled`. Skips any run with a non-terminal stage so a genuinely in-flight run is never force-completed; idempotent (an already-terminal run is a `completeRun` no-op, a re-run finds nothing).

Reuses existing repo methods only (no new query) — the recovery for the `{all stages terminal, run non-terminal}` class the merge-resolution bug produced.

## Pre-spawn acceptance short-circuit verdict (#1728 / #1748, verdict corrected by #2347)

`tryShortCircuitAcceptanceCore` evaluates three disjoint approved-plan predicates before an acceptance stage ever spawns a runner: the out-of-scope skip (`verification.out_of_scope` with zero `acceptance_criteria`), empty-criteria (zero of both), and all-skip-with-basis (every criterion `skip_expected` with an `expectation_basis`). On a hit it walks the stage straight to `succeeded` with no runner, no preview, and no observation.

The out-of-scope predicate records a skip MARKER (`acceptance_skipped_out_of_scope`) and no verdict; it is untouched by #2347 and already reads as "no verdict by design". The other two record a real `acceptance_outcome_recorded` verdict via `emitAcceptanceOutcomeShortCircuit` — and that verdict is **`not_validated`, never `passed`**.

Both bases verified exactly ZERO criteria. Recording the same `passed`/`accepted` words a validator-shipped pass records made an ABSENCE of verification render as certification at every consumer downstream: the merge gate (ADR-049 decision #6 gates on that word), the operator's status comment, and release evidence (which passes the verdict string through verbatim). `plan.AcceptanceVerdictNotValidated` / `plan.AcceptanceOutcomeNotValidated` are defined in the **plan** package — imported by orchestrator, server, and auditcomplete, importing no project package itself — so a producer/consumer drift is a compile error rather than a silent runtime miss.

The payload additionally carries `criteria_live_validation` (`plan.LiveValidationCriteriaCount`), the count of criteria marked `requires_live_validation`, so a skip that carries a tracked operator-validation walk (#2338 / #2345) is distinguishable from one skipped on any other basis. `criteria_passed` / `criteria_failed` stay 0 and `criteria_skipped` / `criteria_total` are unchanged.

The outcome stays **merge-eligible** (`server.acceptanceGateNotValidated`): a change with no live target must not be stranded, and a merge block that text-matched operator prose would trade a dishonest pass for a wedge. The honesty is carried by the distinct verdict, gate state, `next_actions` state + reason, and status-comment row — a prompt to acknowledge, not an enforcement.

`auditcomplete`'s trace-required exemption keys on the payload **basis**, never on the verdict, so it is unaffected by the verdict change (pinned by a regression test there).
