# backend/internal/drive

Drive mode: the rule engine classifying a drive-enabled run's named transition points as mechanical (auto-advance) or judgment (park), with audited auto-advance markers. #1023 / #996 theme 1.

## Rule table

- **Mechanical** (auto-advance or auto-detect): `plan_approved_dispatch`, `plan_approved_human_gate` (#3014 — the plan-approved successor rule for a workflow declaring NO implement stage: it names the real `plan:approved` -> `review:awaiting_approval` edge, parks, and carries NO next action because the human-executor gate is approved via the `fishhawk approve-review-gate` CLI verb, not a host dispatch), `reviews_settled_gate`, `fixup_rereview_repark`, `checks_green_awaiting_merge`, `ci_failed`, `ci_recovered` (#3414), `children_dispatch`, `deploy_initialization`.
- **Judgment** (always park): `gate_approval`, `concern_routing`, `merge` — absent ADR-040 delegation.

The package also owns the `run_auto_advanced` audit emission (`Engine.Record`) and two idempotency reads:

- `Engine.Recorded` — per-(run, stage, rule) "was this rule EVER stamped for this stage". The dedup for the poll-driven mergereconciler tick and the re-checkable gates whose staleness does not affect the drive loop.
- `Engine.LatestRuleIs` — run-wide "is this rule the run's CURRENT derived status", i.e. does the run's highest-`Sequence` `run_auto_advanced` entry name this rule (mirroring `applyDriveSurfaces`' sort-by-`Sequence`-take-last, so the engine and `GET /v0/runs` `derived_status` agree). The **acceptance-gate presentation stamps** (`acceptance_pending` / `acceptance_settled_outcome_unknown` / `acceptance_triage`) dedup on THIS rather than `Recorded`, because a fix-up re-park stamps a LATER `fixup_rereview_repark` entry that supersedes them: under `Recorded` the per-stage guard suppresses a re-stamp so `derived_status` stays stale (no longer `acceptance_pending`) after the re-review re-settles, the #1961 drive-loop guards go inert, and `drive_run` parks `decision_required:review_gate_parked` at a state whose authoritative next act is a bare acceptance dispatch (#2122). Keying on `LatestRuleIs` re-asserts the current derived status. Both reads FAIL-OPEN identically (nil/err/empty → `false`), so a persistently failing audit read re-stamps once per observation tick (per-tick duplication) rather than suppressing the current derived status forever.

Deliberate scoping (#2122, amended #3414): `ci_failed` now dedups on `LatestRuleIs` (a persistent red still stamps once, but a NEW red after a `ci_recovered` / `checks_green_awaiting_merge` supersession re-parks — the park is re-evaluated against the live `stage_checks` rows every tick rather than latched at the first observation). `checks_green_awaiting_merge` stays on `Recorded` EXCEPT when the run's latest entry is a CI park (`ci_failed` or `ci_recovered`): a run that went green after a CI park must re-assert `awaiting_merge` because `delegation.evalGatesResolvedCIGreen` and `derived_status` both key on the LATEST entry, so a stale `checks_green` stamp buried under the park would leave the run wedged. Every OTHER supersession — in particular `fixup_rereview_repark` — keeps the `Recorded` scoping, so this carve-out (`ciParkIsLatest`) is deliberately narrow. The precursor `reviews_settled_gate` stays on `Recorded` too — re-stamping it would oscillate the latest entry.

## Hook points

The engine never performs a state transition — the hook points that stamp it live with the transitions they document:

- **Plan approval**: `backend/internal/server/approvals.go::recordDrivePlanApproved` — which rule it stamps depends on the plan gate's DECLARED successor, resolved from the run's stage rows (#3014). With an implement stage row: `plan_approved_dispatch`, where the orchestrator `Advance` handoff IS the dispatch for `runner_kind github_actions` and `local` parks with a `run_implement_stage` next action per ADR-024. With NO implement stage but a human-executor review row (`backlog_grooming`): `plan_approved_human_gate`, parked on the `review:awaiting_approval` edge with no next action, host-independent — the gate is approved by a human-held credential through the `fishhawk approve-review-gate` CLI verb, so there is no dispatch to park for. With neither: nothing is stamped. A `ListStagesForRun` error or an empty list fails open to the `plan_approved_dispatch` path.
- **Fix-up re-park**: `fixup.go::recordDriveFixupRepark` — stamps the #780 review re-park.
- **Deploy initialization**: `runs.go::recordDriveDeployInitialization` — the deploy-first creation park (E23.13 / #1429 / ADR-038). When a created run's FIRST stage is a `deploy` stage, `handleCreateRun` calls `orchestrator.Advance` to park it `pending → awaiting_deploy_approval` at its pre-execution gate — there is no agent/runner and thus no operator-driven `run_stage` entry to trigger it.
  Best-effort: an `Advance` error WARN-logs and never unwinds the 201. `drive.EvaluateDeployInitialization` carries a host-independent `fishhawk_approve_plan` next action since the deploy approval pages the human regardless of runner kind.
- **Merge reconciler open-PR tick**: the optional `DriveObserver` (defaulted from the `Resolver` via type assertion since production wires `*server.Server` there) calls `server.go::ObserveParkedReviewForDrive` — N-of-N implement-review settlement via `planreview.Settled` (rounds delimited by `implement_review_started` sequence so a settled first round never satisfies a fix-up re-review).
  It then stamps the **derived, presentation-only** `awaiting_merge` status when the review evidence is terminal AND every `RequiredChecksSnapshot` context has a green `stage_checks` row (conservative: any gap reads not-green).
  **Unresolved-checks wording (#2497):** when the run has NO protection snapshot (`RequiredChecksSnapshot == nil` — every local MCP run, which never captures one), `server.go::reviewChecksResolution` reports the checks as UNRESOLVED rather than green. The `checks_green_awaiting_merge` rule still fires (parking it would wedge `merge_run` for every local run — `delegation.evalGatesResolvedCIGreen` keys `may_merge` on this exact rule), but its `Event` and `next_action.Detail` — and those of the three acceptance parks (`acceptance_pending`, `acceptance_settled_outcome_unknown`, `acceptance_triage`) — carry non-asserting prose ("required checks were not resolved for this run … verify CI on the PR before merging") instead of a greenness claim. Read the rule NAME as a presentation state, NOT as an assertion that CI passed.

## `ci_failed` (#1045)

The negative mirror at the same review-evidence-complete gate point: when the review evidence is terminal but `reviewChecksFailed` finds a required `stage_checks` row carrying a RED VERDICT (`(*Check).RedVerdict()` — `StateFail` minus a superseded `cancelled`/`stale` conclusion, #3414; a `StatePending` in-flight check never trips it either), the observer stamps the derived `ci_failed` status with a `classify_ci_failure` next action naming the red check(s).

Detection only (ADR-040 bucket 1): it parks, never advances — remediation stays the operator's call.

`runs.go::applyDriveSurfaces` derives `derived_status: ci_failed` when that is the latest rule on an open PR (a later `checks_green`/`ci_recovered`/`fixup_rereview_repark` stamp supersedes it), and the MCP `next_actions` classifier gains the `ci_failed_routable` arm (open concerns → `fishhawk_fixup_stage`) and the `ci_failed_unroutable` arm (no concerns → `commit_and_vouch` operator-remediation #1044, then a checks re-run, then `page_human`).

### `ci_recovered` lifecycle (#3414)

`ci.yml`'s `concurrency: cancel-in-progress` concludes a superseded head's check runs `cancelled`, which `DeriveState` maps to `StateFail`. Before #3414 that tripped `ci_failed` and the park never lifted — the `ci_failed` stamp and the `checks_green_awaiting_merge` stamp were both guarded, so nothing superseded the park even once the newer head went green. Three coupled fixes make the park re-evaluable:

1. **`ci_failed` keys on `RedVerdict`, not `StateFail`** — a superseded `cancelled`/`stale` conclusion no longer parks (see the `stagecheck` README for the predicate).
2. **`ci_recovered`** — when the run's latest auto-advance is `ci_failed` but no required check carries a red verdict at the latest rows and the checks are not yet green (pending / superseded), the observer stamps `ci_recovered` (From `ci_failed`, To `review:awaiting_approval`, next action `await_checks` naming the outstanding contexts). It is a RULE value, not a `derived_status` — `applyDriveSurfaces` maps no `derived_status` for it, so the field goes EMPTY and `next_actions` leaves the `ci_failed_unroutable` dead end. Anti-oscillation: it stamps only when the latest entry is `ci_failed` (so a `ci_recovered`-latest tick appends no duplicate).
3. **The `ci_failed` / `checks_green_awaiting_merge` re-assert guards** above (`LatestRuleIs` + `ciParkIsLatest`), so a genuine red after a recovery re-parks and a run that goes green after a CI park re-asserts `awaiting_merge`.

Detection only, like `ci_failed`: `ci_recovered` never merges — the remaining required checks concluding is the real edge.

## `children_dispatch` (E24.3 / #1143)

Stamped by `orchestrator.go::DispatchDecomposedChildren` (via `recordChildDispatch` + `drive.EvaluateChildrenDispatch`) for each decomposed child it dispatches: `runner_kind github_actions` records an advance (the `Advance` handoff fires the child's `workflow_dispatch`), `local` records a park with a `run_implement_stage` next action (host-spawned runner, ADR-024) — anchored to the child's implement stage, deduped via `Engine.Recorded`.

## Opt-in + surfaces

`runs.drive` (migration 0031) opts in per run; the GET-status rendering of `next_action`/`auto_advanced` is the #1023 slice-3 surface.
