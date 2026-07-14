# backend/internal/webhook

GitHub webhook receipt, event → run/stage matching and dispatch, and the shared budget-admission decision core both admission seams use.

## Event dispatcher (events → runs + stages)

`backend/internal/webhook/dispatcher.go` (`MatchEvent` pure + `Dispatcher.Handle` orchestrator); wired via `cfg.WebhookDispatcher`.

Sub-topics, with full detail in [`docs/architecture/webhook-dispatcher.md`](../../../docs/architecture/webhook-dispatcher.md):

- [Branch-protection snapshot](../../../docs/architecture/webhook-dispatcher.md#branch-protection-snapshot) (#251 / ADR-017)
- [CI policy re-evaluation](../../../docs/architecture/webhook-dispatcher.md#ci-policy-re-evaluation) (#300)
- [CI-failure retry trigger + handler](../../../docs/architecture/webhook-dispatcher.md#ci-failure-retry) (#278/#279/#280/#283 / E16)
- [Review-stage merge signal](../../../docs/architecture/webhook-dispatcher.md#review-stage-merge-signal) (ADR-018 / #312)
- [PR closed without merging](../../../docs/architecture/webhook-dispatcher.md#pr-closed-without-merging) (#316)
- [In-Fishhawk approval prune](../../../docs/architecture/webhook-dispatcher.md#in-fishhawk-approval-prune) (ADR-018 / #313)
- [SPA review-stage read-only summary](../../../docs/architecture/webhook-dispatcher.md#spa-review-stage-read-only-summary) (ADR-018 / #314)

## workflow_run.failure → fail-C (#243)

`webhook.matchWorkflowRun` recognizes `workflow_run.completed` events for `fishhawk.yml` (the configured `DefaultActionsWorkflowFile`) with a non-success conclusion (`failure`, `timed_out`, `cancelled`, `action_required`, `startup_failure`, `stale`). Tagged as `MatchActionRunnerActionFailed` carrying the workflow_run's id + conclusion.

- `Dispatcher.handleRunnerActionFailed` calls `githubclient.Client.GetWorkflowRun` to recover the original `workflow_dispatch.inputs` — specifically the `stage_id` we fired with — then transitions that stage to failed-C via `run.FailStage`.
- Idempotent: a redelivered webhook hits a stage that's already terminal, and `FailStage` is a same-state no-op.
- Faster path than the dispatch watchdog: the watchdog times out on a clock; this fires within seconds of the GitHub Actions run terminating.
- Best-effort throughout: lookup failures log + return (the watchdog still cleans up).
- New App permission: none — `workflow_run` events are already in the App's `default_events` (per `manifest.go`); the webhook receiver was just dropping them as unrecognized.
- SPA gets the failure-C `<FailureBanner>` Retry affordance for free since #173 covers category-C retry.

## Blocking periodic budget (admission gate, `run_rejected_budget` / `run_admitted_budget_override`)

Shared decision core: `backend/internal/webhook/budget_admission.go` (#688 / ADR-030) — `CostSummer` capability + `CheckBlockingBudget(ctx, summer, repo, workflowID, budgets, now, loc)`.
It iterates `Workflow.Budgets`, skips advisory/empty-enforcement entries (only `enforcement: blocking` gates), sums the current calendar period via `CostSummer.SumWorkflowCostInRange`, calls `budget.Evaluate`, and returns the FIRST blocking budget whose `Over` is true.
It lives in the **webhook** package because both admission seams need it and server already imports webhook (no import cycle); a sum error is fail-open (returns `blocked=false` + err so the caller logs and admits).

**Two admission seams gate a NEW run** — a workflow that crosses a blocking budget refuses the next run once the period's `cost_recorded` spend reaches `limit_usd`:

1. **HTTP handler** — `backend/internal/server/budget_admission.go::checkBlockingBudget`, called from `handleCreateRun` (`runs.go`) after the plan-reviewer guard and before `CreateRun`.
   On block it writes a `run_rejected_budget` global-chain audit and returns `402 budget_exhausted` (reason names the period, limit, spend, and how to clear it), unless the request carries `budget_override:true` (threaded from the CLI `--override-budget` flag / MCP `fishhawk_start_run budget_override`), which admits the run and records a `run_admitted_budget_override` audit instead.
2. **Webhook dispatcher** — `dispatcher.go::refusedByBlockingBudget`, called in the new-run dispatch flow after `resolveRequiredChecks` and before `d.Runs.CreateRun` (the dispatcher creates runs directly, bypassing the handler — a gate only at the handler would leak every webhook-dispatched run).
   On block it writes a `run_rejected_budget` audit and creates NO run row. **There is no override on the webhook path** (webhook triggers can't carry one).

- Both seams type-assert their run repo to `CostSummer` and ADMIT when it's absent (capability-absent skip, mirroring `checkBudgetAlerts`).
- **Never gated** (ADR-030: in-flight work finishes): in-flight runs, CI-retry re-dispatch, and decomposition-child creates — those continuation paths call `CreateRun` directly and skip the gate.
- Timezone reuses `server.Config.BudgetLocation` / `Dispatcher.BudgetLocation` (both fed from `FISHHAWKD_BUDGET_TIMEZONE` in `serve.go`).
- Both new audit kinds are admission-time global-chain entries (no Notifier method, no issue comment) — see `docs/issue-comment-surfaces.md`.
