# backend/internal/run

Run/stage domain model: state machines and transition tables, failure taxonomy, gate persistence, and the operator recovery verbs (retry, fix-up, revise, category-B recover) plus the `runner_kind` resolution model.

## `runner_kind` resolution model (#1346 / ADR-045)

`runner_kind` (`github_actions` | `local`) is an OBSERVED fact, not a creation-time operator declaration. **This supersedes migration 0024's "the runner never self-declares" note for the channel tag** — a self-report carried INSIDE the SIGNED bundle manifest is attestable (Ed25519-tamper-evident), not falsifiable.

Detection — the runner observes its OWN environment (`runner/cmd/fishhawk-runner/runnerkind.go::detectRunnerKind`):

- `GITHUB_ACTIONS=true` OR a non-empty `GITHUB_RUN_ID` ⇒ `github_actions`; everything else ⇒ `local`.
- **`CI=true` is NOT a signal** — a local dev shell commonly exports it, and treating it as `github_actions` would re-create the #1344 phantom-Actions-runner wedge (the GITHUB_*-authoritative assumption is load-bearing).

The runner stamps the value into the manifest (`runner_kind` field, lockstep `runner/internal/bundle/bundle.go::ManifestData` ↔ `backend/internal/bundle/bundle.go::Manifest`) and surfaces it on `runner_started`.

Reconciliation: the trace handler reconciles via `trace.go::reconcileRunnerKind` → `run.(runnerKindResolver).ResolveRunnerKind` (an optional concrete-repo capability like `AddRunCost`):

- The FIRST report LOCKS `runner_kind` (`runs.runner_kind_resolved=true`, migration 0036), correcting the create-time hint.
- A later report disagreeing with the locked kind is FLAGGED (`runner_kind_mismatch` audit) and does NOT mutate the row (warn, never silently flip).
- Because the plan stage ships its trace (`--upload-trace`) BEFORE the operator approves the plan, `runner_kind` is locked to the truth by the time the drive's plan-approval gate (`recordDrivePlanApproved` → `drive.EvaluatePlanApproved`) decides auto-dispatch (`github_actions`) vs park (`local`).
- The mismatch guardrail is post-execution AUDIT only this slice; a pre-dispatch host-endpoint block (the issue's level-1) is a deferred follow-up.
- Best-effort throughout: any reconciliation error WARN-logs and never unwinds the stored trace.

## Host-dispatch park state (`awaiting_host_dispatch`, #1912)

`awaiting_host_dispatch` splits the old conflated local `dispatched` state into two explicit signals, resolving the #1905 no-spawn-evidence ambiguity at the source:

- **`awaiting_host_dispatch`** — the backend wants this agent stage executed but the runner is host-spawned per ADR-024 and NO spawn attempt exists yet. A parked judgment awaiting a host/operator spawn action; `IsSettled()` true (mirrors `awaiting_approval`), `IsTerminal()` false.
- **`dispatched`** — now unambiguously means a spawn attempt EXISTS: `workflow_dispatch` fired (github_actions) or a host spawn was marked (local). Stays in-flight (`IsSettled()` false).

State-machine edges (`transition.go`):

- `pending → awaiting_host_dispatch` — the local park, written in exactly one place: `orchestrator.dispatchStage` for a `runner_kind`-locked-local agent stage (a sibling slice). All local parks (plan-approved dispatch, retry/fixup re-open, revise replan, children dispatch) flow through `Advance → dispatchStage`, so one branch covers every drive rule.
- `awaiting_host_dispatch → dispatched` — the host spawn marker (`POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch`, a sibling slice) CAS-flips the park once a spawn attempt exists.
- `awaiting_host_dispatch → cancelled` — run cancel halts the parked stage.

`FailStage` (`failure.go`) walks a parked stage `awaiting_host_dispatch → dispatched → running → failed` (the base machine forbids a direct `→ failed` edge) in both the CAS and non-CAS arms, so a parked local spawn that is abandoned still lands `failed` through the canonical path. The up-front `awaiting_children` park refusal is unaffected — only that fan-in park is un-failable.

Migration `0053_stages_awaiting_host_dispatch` widens `stages_state_check` to admit the value (the 0035/0038 CHECK-widening precedent) and backfills existing parked local rows (`state='dispatched'` + `started_at IS NULL` on a non-terminal `runner_kind='local'` run) to the new state; a re-opened row carrying a prior attempt's `started_at` is conservatively skipped (tolerated read-side). The down reverses the backfill then narrows the CHECK.

This slice is purely additive — nothing writes `awaiting_host_dispatch` yet (the park writer, marker endpoint, and MCP consumers land in sibling slices), so the tree stays green standalone.

## Stage-row gate persistence (#213, #254)

The dispatcher captures the workflow-spec gate shape on each stage row at create time, alongside the existing `gate_sla` / `requires_approval` columns from #207.

- Migration 0014 added `gate_type` (`'approval' | 'check' | NULL`), `gate_blocking_checks TEXT[]`, and `gate_approvers JSONB`; migration 0018 (#254 / ADR-017) dropped `gate_blocking_checks` along with the spec field — required CI checks now live in branch protection (#251).
- `webhook.dispatcher::primaryGate` picks one gate per stage — first approval gate, else first check gate, else nil — mirroring the scoping of `gate_sla`/`requires_approval`.
- `run.Gate` (in `run.go`) is the in-memory shape; `run.postgresRepo.CreateStage` JSON-marshals approvers into the JSONB column and `rowToStage` reverses it.
- Wire shape is `Stage.gate` (omitted when nil) per `docs/api/v0.openapi.yaml::StageGate`.

## Failure taxonomy

`run.go`'s `FailureCategory` type carries the four MVP_SPEC §6 categories (A=agent, B=constraint/policy, C=infra, D=approval timeout/rejection) with `Valid()` + `Description()` methods.

`failure.go`'s `FailStage(ctx, repo, stageID, cat, reason)` helper is the single transition entry point — walks `dispatched → running → failed` when needed, idempotent on already-failed stages.

Emitters:

- `server.failStageCategoryB` (trace path, B)
- `server.advanceStage` (approvals reject, D)
- `sla.handleStage` (SLA elapse, D)
- `dispatchwatchdog.handleStage` (C)
- the trace handler's `agent_failed` branch (A — the trace-bundle category-A signal)

Frontend mirrors the descriptions in `frontend/src/api/types.ts` (`FAILURE_DESCRIPTIONS`, `describeFailure`); rendered by `<FailureBanner>` (`frontend/src/components/failure-banner.tsx`) above the stage detail and as a category badge next to failed stages on the run-detail list.

## Retry semantics

`retry.go`'s `RetryStage(ctx, repo, stageID, opts)` is the per-category decision tree. `transition.go` keeps a separate `stageRetryTransitions` table (`failed → awaiting_approval` for D-timeout, `failed → pending` for A/C) so the regular state machine invariant "terminal states are terminal under normal transitions" stays true.

- The repo-side `RetryStage(stageID, to)` clears `failure_category`, `failure_reason`, and `ended_at`; the `updated_at` trigger fires implicitly so the SLA ticker measures from the new value on its next pass.
- `POST /v0/stages/{id}/retry` (`backend/internal/server/retry.go`) maps the helper's outcomes: 200 + updated Stage on success, 422 `retry_not_applicable` for B and D-rejected.
- For A/C the handler hands off to `Orchestrator.Advance` after the state move so the orchestrator transitions pending → dispatched and fires workflow_dispatch (E8.6 #173); orchestrator failures are logged but don't fail the request — the audit row records the retry intent and an operator can re-fire Advance.
- An A/C/D-timeout retry on a failed run also reopens the run `failed → running` (via the same `RetryRun` primitive #698's `RedriveChild` uses) before the orchestrator handoff, because `Advance` no-ops on a terminal run — re-opening only the stage stranded the run with the re-run's work landed and the next gate never opening (#798). The reopen is best-effort (logged, not fatal) and gated on `State == failed` so it is inert when no run row is resolvable.

**Audited category-B override (#698)**: the optional request body `{override: true, reason: "..."}` sets `RetryOptions.OverrideB`, which admits a genuine category-B failure onto the A/C `failed → pending` path (`RetryDecision.Overridden = true`).

- The override re-runs the stage so the policy gate re-evaluates the new diff — it does NOT accept the B-violating diff or bypass the gate.
- `reason` is mandatory (`400 validation_failed` otherwise) and the success writes a distinct `stage_override_retried` audit entry (user actor + reason + prior category B), kept separable from the ordinary `stage_retried` receipt and from #692's automatic empty-diff → C reclassification.
- The default (no body / `override:false`) leaves B non-retryable.

Frontend's `<FailureBanner>` renders a Retry button on failed-A, failed-C, and failed-D-timeout stages with optimistic update + rollback (mirrors `<ApprovalPanel>`); the optimistic state is `awaiting_approval` for D-timeout and `pending` for A/C, replaced by the canonical post-orchestrator state on the server response.

## Implement-review fix-up (#762, E22.X)

The operator verb for routing one or more *advisory* implement-review concerns (ADR-027 `approve_with_concerns`) back to the implement agent for a bounded, operator-gated fix-up pass — replacing the manual PR-branch hand-edits operators drove on 2026-06-04.

`fixup.go`'s `FixupStage(ctx, repo, stageID, opts)` re-opens the implement stage parked at the review gate via a dedicated `stageFixupTransitions` table in `transition.go` (`awaiting_approval → pending`, kept separate from `stageRetryTransitions` because a fix-up is a distinct semantic — no failure to clear, re-opened from a *healthy* gate — and consulted by `TransitionStage` in addition to `ValidStageTransition`).

It refuses with `ErrFixupNotApplicable` (non-implement stage / wrong state / no recorded concerns → 422 `fixup_not_applicable`) or `ErrFixupBudgetExhausted` (the NORMAL bounded pass count is spent → 422 `fixup_budget_exhausted`).

**#860 bounded operator override**: the request's `force_additional_pass: bool` (threaded into `FixupOptions.ForceAdditionalPass`, with `HardCeiling: defaultFixupCeiling == 3` supplied by the handler) grants ONE pass beyond the normal budget — audited via a `forced` flag on the `stage_fixup_triggered` entry — hard-capped at 3 total passes. At the ceiling `FixupStage` returns the DISTINCT `ErrFixupCeilingReached` → 422 `fixup_ceiling_reached` (the handler arm is ordered before `fixup_budget_exhausted` so it is not masked).

The MCP `review_action_hint` (`backend/cmd/fishhawk-mcp/review_action_hint.go`) no longer suppresses on a spent budget when concerns remain: it surfaces the exhaustion plus `OverrideAvailable` and counts only the LATEST review round's concerns (scoped by the most-recent `stage_fixup_triggered` audit sequence).

### Handler + concern delivery

`POST /v0/stages/{stage_id}/fixup` (`backend/internal/server/fixup.go`):

- Resolves the addressable concern set from the stage's `implement_reviewed` audit entries and validates the request's `concerns` indices against it.
- **Bounds the pass by counting prior `stage_fixup_triggered` audit entries** (default 1 — no dedicated column), then re-opens and hands off to `Orchestrator.Advance` exactly like retry (pending → dispatched → workflow_dispatch).
- Writes a `stage_fixup_triggered` audit entry carrying the selected concern objects, which `server/prompt.go::resolveFixupConcerns` reads back to deliver the concerns to the agent as binding instructions (the #558 condition-delivery framing, rendered under `### Fix-up concerns` by `prompt.go`) and to fold any concern-named file into the effective `scope.files`.

**#1214**: on the non-empty narrowed path, `narrowFixupScope` ALSO auto-folds each narrowed source file's coupled `*_test.go` stem-sibling (`coupledTestSiblings` → `foldScopePaths`, source `fixup-coupled-test-sibling`, mirroring the #1083 decomposed-child fold) so a fix-up's fix+test pass lands the sibling test in the SAME commit instead of having it stripped as scope_drift.
This removes the dependency on the timing-fragile mid-stage scope amendment (#1189) for the overwhelmingly common case (a concern naming only `main.go` now also scopes `main_test.go` as `operation=modify`). The empty-narrow fail-safe is untouched.

The runner commits the fix-up onto the SAME PR branch via the rebase-from-remote shared-branch path and UPDATES the existing PR (distinct from `/retry`'s fresh diff); it stamps `push_fixup` in the trace manifest so the backend forward-gates the fix-up stage's terminal transition (#794; see `docs/ARCHITECTURE.md` §4.2.1) and reports push success via a `/pull-request` `{outcome:"fixup_pushed"}` report (or `{outcome:"failed"}` on a push/compile-gate failure).

**Auth**: `write:stages` OR `write:fixups`; a run-bound MCP token may fix up only its own run's stages (`403 cross_run_fixup`). MCP verb: `fishhawk_fixup_stage`.

### Failure recovery (#788)

A fix-up re-dispatch that FAILS must NOT destroy the intact original work (the PR is open and mergeable) — a fix-up is a best-effort optional pass on top of an already-succeeded implement + open PR.

- `run.RestoreFixupStage` (in `fixup.go`) restores the run to its pre-fix-up review gate via a dedicated `stageFixupRecoveryTransitions` table in `transition.go` — implement `failed → succeeded` (push_and_open_pr) / `failed → awaiting_approval` (commit-yourself) + review `pending → awaiting_approval` (re-park restore).
- That table is kept SEPARATE from `stageRetryTransitions`/`stageFixupTransitions` because admitting `failed → succeeded` must never leak into the ordinary path, where it would fake success; it is reachable only through `ValidStageFixupRecoveryTransition` (consulted by `TransitionStage`) and guarded by `RestoreFixupStage`.
- The server detector `server/fixup.go::maybeRecoverFixupFailure` keys off the durable `stage_fixup_triggered` audit entry (its `prior_state` + `reparked_review_stage_id`): since a fix-up re-opens from a HEALTHY gate, {entry present + implement stage failed} uniquely identifies a re-dispatch failure.
- It restores the gate, writes a `stage_fixup_recovered` audit entry (system actor; restored states + source failure category/reason), and returns true so the two implement-failure chokepoints — `trace.go::advanceAfterFailure` (cat-A agent-fail, cat-B implement-review reject, failStageCategoryB/C) and `pullrequest.go::failPullRequestStage` (the #742 PR-open / #794 fix-up push failure) — SKIP the run-failing `Orchestrator.Advance`, leaving the run `running` at its gate.

**#794 closed the gap that made this recovery unreachable for fix-ups**: before #794 a fix-up stamped no forward-gate flag, terminalized at trace upload, and so never reached `failPullRequestStage` in a `failed` state — `maybeRecoverFixupFailure` could not fire. The `push_fixup` gate (above) defers the terminal transition so a fix-up push/compile-gate failure genuinely lands the stage `failed` and triggers recovery.

With `defaultMaxFixupPasses == 1` a failed fix-up still consumes the budget, so the post-recovery operator path is "merge the original PR," not "re-fire the fix-up."

Cross-component coverage: `backend/internal/integration/mcp/fixup_test.go` drives concern → MCP-triggered fix-up → re-open → binding-instruction prompt render → bound-exhausted refusal end-to-end, plus the #794 forward-gate seam end-to-end:
a `push_fixup` trace upload leaves the fix-up stage `running` (terminal transition deferred), then the `/pull-request` `{outcome:"fixup_pushed"}` success report drives it terminal + writes a `fixup_pushed` audit entry, AND the `{outcome:"failed"}` re-dispatch-failure report restores the run to its review gate with a `stage_fixup_recovered` audit entry.

### Near-deterministic apply (#1165)

When a reviewer emits a `suggested_patch` (a unified diff) on EVERY routed concern, the fix-up can collapse to a `git apply` with no agent invocation.

- `server/fixup.go::resolveConcernsByID` carries the stored `suggested_patch` onto the routed concern set and `writeFixupAudit` records an `apply_eligible` boolean on the `stage_fixup_triggered` entry (true iff ≥1 concern and every routed concern carries a patch).
- `server/prompt.go::resolveFixupApplyPatches` reads that same entry back and, under the SAME all-or-nothing gate, serves the patches as `fixup_apply_patches[]` on the implement prompt-response (omitted when any concern lacks a patch → the runner takes the unchanged agent path).
- The runner (`runner/cmd/fishhawk-runner/main.go::attemptDeterministicFixup`, after the #967 fix-up base checkout and BEFORE the agent loop) `git apply --3way`s each patch onto the PR branch and runs the existing committed-tree verify gate (`runVerifyGateCommitted`); on a clean apply that passes the gate it skips the agent invocation entirely and the applied working tree flows into the unchanged `fixup_pushed` push path.
- On ANY failure — a patch does not apply, the verify gate fails, or the gate produced no verifiable tree — it `git reset --hard <tip> && git clean -fd`s the worktree (fail-safe: never a half-applied tree) and falls through to the agent.
- **Provenance** is two-part: the server records the `apply_eligible` boolean on the `stage_fixup_triggered` trigger audit, and the runner emits a `fixup_apply_path` trace-bundle event carrying the runtime discriminator `apply_path` (`applied` | `agent` | `apply_failed_fellback`) for every fix-up dispatch.
- The runtime `apply_path` is deliberately NOT sent on the `fixup_pushed` report — the `/pull-request` handler decodes with `DisallowUnknownFields`, so an unknown body field would 400 and break the report; persisting it onto the `fixup_pushed` audit entry (`succeedFixupPushStage`, `pullrequest.go`) is a follow-up.
- Requires a configured verify gate (`cfg.verifyCmd`) and the push path (`!cfg.noPR`); without a gate the runner conservatively re-derives with the agent.

### Operator-authored ad-hoc concern (#1311)

The request's optional free-text `operator_concern` resolves the ADR-031/ADR-035 tension for a REQUIRED external-check failure that has no Fishhawk review concern (a CodeQL/SAST alert on a clean approve-with-zero-concerns diff) — previously the operator's only route was a hand-commit on the run branch, which trips ADR-035 `foreign_commit_on_branch`.

- `server/fixup.go` converts the free text into a synthetic `[high/operator]` `planreview.Concern` folded into the same `selected` slice the existing machinery carries (capped at `maxOperatorConcernBytes == 4000` to match the renderer's per-section cap — a whitespace-only or over-length value fails LOUD with 400 rather than silently truncating a binding instruction).
- It therefore rides the unchanged budget/applicability/audit/prompt-render path and is delivered to the implement agent as a binding `### Fix-up concerns` instruction ON the run branch — a sole-writer commit through the runner, not a foreign hand-commit.
- It needs NO recorded `approve_with_concerns` verdict: `operator_concern` ALONE is admitted on a zero-concern gate-open stage (the `fixup_not_applicable` precondition is enforced only on the deprecated positional path), and at least one of `concern_ids`/`concerns`/`operator_concern` is required.
- The raw text is recorded on the `stage_fixup_triggered` entry as `operator_concern`. Option 2 (the ADR-035 vouch path) is out of scope.

### Ceiling-reached vouch surfacing (#1097)

Display-only, no gate/ceiling change: once the hard ceiling is reached, the `422 fixup_ceiling_reached` `details` map carries a `remediation` pointer and the MCP `review_action_hint` hard-ceiling arm advertises a `commit_and_vouch` action + Message.
Both name the already-shipped #1068/#1044 operator-vouched patch path (commit the late-CI/SAST fix on the run branch, then `fishhawk_vouch_commit` with the operator/operator-agent token, NOT a run-bound `fhm_` token which is rejected `run_token_forbidden`) as the sanctioned in-loop remedy, migrating operator folklore (#996 Theme 3 / ADR-040) into a server-suggested next action. It builds NO separate CI/SAST fix-up budget.

## Plan-gate revise verdict (#1099, E22.X)

The third plan-gate verdict alongside approve/reject — re-plan IN PLACE against a binding operator design constraint, instead of approving as-is or rejecting to a fresh-run replan. The plan-stage analogue of the implement-review fix-up (#762): a bounded, operator-gated re-open of a HEALTHY gate.

`revise.go`'s `RevisePlanStage(ctx, repo, stageID, opts)` re-opens the plan stage parked at `awaiting_approval` via a dedicated `stageReviseTransitions` table in `transition.go` (`awaiting_approval → pending`, kept SEPARATE from `stageFixupTransitions`/`stageRetryTransitions` because a revise re-opens a PLAN stage — no failure to clear, no review/implement stage touched — and consulted by `TransitionStage` in addition to `ValidStageTransition`).

Refusals:

- `ErrReviseNotApplicable` (non-plan stage / not `awaiting_approval` → 409 `revise_not_applicable`)
- `ErrReviseBudgetExhausted` (NORMAL pass count spent → 409 `revise_budget_exhausted`)
- `ErrReviseCeilingReached` (hard ceiling of 3 → 409 `revise_ceiling_reached`, ordered before budget so it is not masked)

The bound is enforced by **counting prior `plan_revised` audit entries** for the stage (default 1 — no dedicated column, exactly as fix-up counts `stage_fixup_triggered`); `force_additional_pass: true` grants ONE pass beyond the budget (audited via a `forced` flag), hard-capped at 3.

`POST /v0/stages/{stage_id}/revise` (`backend/internal/server/revise.go::handleRevisePlan`; `write:approvals` scope — the #558 gate-answer family; a run-bound MCP token may revise only its own run's stages → 403 `cross_run_revise`) writes a `plan_revised` audit entry whose `conditions` field carries the rendered operator constraint, then re-opens and hands off to `Orchestrator.Advance` (pending → dispatched → workflow_dispatch).

On the re-dispatch, `server/prompt.go::loadRevisionConstraint` reads the newest `plan_revised` entry's `conditions` into `prompt.Trigger.RevisionConstraint` and `loadRevisionBasePlan` serializes the prior plan into `RevisionBasePlan`; `buildPlan` renders a DEDICATED `### Revision constraint (binding ...)` section (NOT under the Clarification-answers heading — a dedicated field, the constraint added to the #558 `trustedMarkers` anti-injection list) carrying the base plan + the binding constraint. First-pass plan dispatch leaves both Trigger fields nil so normal plans are byte-unchanged.

MCP verb `fishhawk_revise_plan` (`backend/cmd/fishhawk-mcp/revise.go`, resolves the plan stage from a run id like approve/reject; the `next_actions` `plan_gate_parked` arm offers it between approve and reject); CLI `fishhawk plan revise <run-id> --constraint …`.

Cross-component done-means seam: `backend/internal/integration/mcp/revise_test.go` drives plan → revise-with-constraint → re-dispatched plan prompt carries the binding constraint AND the prior plan as base → re-enters the review→approve gate → approve succeeds.

## Category-B recovery (plan reuse, no replan) (#978, E22.X)

The operator recovery verb for a run whose implement stage failed category-B after its plan was approved — the gap between stage retry (refuses B) and a fresh run (replans).

`POST /v0/runs/{run_id}/recover` (`backend/internal/server/recover.go::handleRecoverRun`; MCP verb `fishhawk_resume_run`) mints a NEW plan-stage-less child run mirroring `handleCIFailureRetry`'s inheritance (repo / workflow / spec / snapshots / runner_kind / issue_context; stages via `webhook.CreateStagesFromSpec` over the now-exported `webhook.FilterOutPlanStages`).

- **`retry_attempt` is carried UNCHANGED** — operator recovery never consumes the `on_ci_failure` cap; `parent_run_id` is the provenance thread.
- Eligibility gate: parent plan stage `succeeded` AND implement stage `failed` category-B, else `409 recovery_not_eligible` with `{plan_state, implement_state, failure_category}`; a parent without a cached spec is `422 recovery_unsupported`.
- Operator-named `add_scope_files` land as a PRE-APPROVED #961 scope-amendment row on the child implement stage (Create + Decide(approved) in the handler — consumed unchanged by `mergeApprovedScopeAmendments` and the runner's pre-commit refresh; `operation:create` entries flow into the #818/#825 gates).
- Provenance audit: `plan_reused_from` on the child (`{parent_run_id, parent_failure_category, added_paths, source:"operator_recovery", reason}`; internal kind, not an issue-comment surface).
- Approval carryover: `prompt.go::resolveApprovalConditions` / `resolveApprovalAddScopeFiles` gain a single-level `ParentRunID` fallback (when the run's own lookup is empty and `DecomposedFrom` is nil) so the parent's binding conditions + #824 folded paths reach the recovery (and CI-retry) implement stage; the plan itself resolves via `loadApprovedPlanForRun`'s existing parent walk.
- `Idempotency-Key` shares the `(repo, key)` keyspace with `POST /v0/runs`. Auth: `write:runs`, same as run create.

### Decomposition-child in-place recovery (#1081)

When the recover target is itself a failed decomposition CHILD (`DecomposedFrom != nil`), `handleRecoverRun` branches BEFORE the new-child eligibility gate to `handleRecoverDecompositionChild`, which recovers the child IN PLACE rather than minting a new run — the SAME run id, re-opened via `run.RedriveChild` on the shared parent branch (`failed` implement → `pending`, `failed` run → `running`).

An in-place re-drive is deliberate: a second `DecomposedFrom` row would double-count in `childcompletion.resolveParent`'s consolidation counters, forcing supersession logic into them.

**Auth (stricter than the new-child path)**: because this branch re-opens a terminal run via the SAME `run.RedriveChild` action `POST /v0/runs/{run_id}/redrive` performs — an operator-only recovery — `handleRecoverDecompositionChild` ADDITIONALLY rejects agent (MCP subject-bound, `mcp:run:<uuid>`) tokens with `403 agent_token_forbidden`, keeping both paths to `RedriveChild` authz-consistent.
The enclosing `write:runs` gate alone is insufficient: it permits a non-agent caller but does not enforce the agent-token rejection redrive requires; runner-side `fhm_` tokens lack `write:runs` and can't clear it in practice, but the posture is enforced by construction, not by accident.

Eligibility: the child's OWN implement stage `failed` category-B AND its plan resolves via the `loadApprovedPlanForRun` parent walk, else `409 recovery_not_eligible` with `{implement_state, failure_category, plan_resolved}`.

Operator `add_scope_files` fold as a pre-approved amendment on the EXISTING implement stage (shared `createApprovedScopeAmendment` helper, created BEFORE the `Orchestrator.Advance` handoff so the prompt's `mergeApprovedScopeAmendments` fold — keyed by run + stage id — sees it on the re-opened stage, whose id `RetryStage` preserves in place).
The `plan_reused_from` entry carries `source:"decomposition_child_recovery"`; the parked parent then reconciles on the re-driven child's next terminal transition through the unchanged `maybeAdvanceDecomposedParent` path (park-on-recoverable — see `backend/internal/orchestrator/README.md`).

Discoverability: `next_actions` (`fishhawk-mcp/next_actions.go`) points a failed decomposition child (`decomposed_from != nil`) with a category-B implement failure at `fishhawk_resume_run` against that CHILD's own id (in-place, `consumes:none` — no new run), superseding the older "point resume at the parent" guidance, which would replan from scratch.
