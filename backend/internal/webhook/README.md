# backend/internal/webhook

GitHub webhook receipt, event → run/stage matching and dispatch, and the shared budget-admission decision core both admission seams use. Also the GitLab receiver's parse + match primitives (E45.6 / #1860).

## GitLab receiver (E45.6 / #1860)

`backend/internal/webhook/gitlab.go` holds the GitLab parse/verify primitives; the HTTP handler + MR review-gate consumer live in `backend/internal/server/webhook_gitlab.go` (`POST /webhooks/gitlab`, secret from `FISHHAWKD_GITLAB_WEBHOOK_SECRET`).

- **Auth is a verbatim token, NOT an HMAC.** GitLab sends the configured secret token verbatim in `X-Gitlab-Token`; `VerifyGitLabToken` compares it against the configured secret. This is the load-bearing difference from the GitHub `X-Hub-Signature-256` HMAC path. The compare hashes both sides to fixed-length SHA-256 digests before `subtle.ConstantTimeCompare` — a bare compare over the raw bytes short-circuits on a length mismatch, leaking the secret's length via timing (the same fixed-length-digest posture `VerifySignature` uses for HMAC sums).
- **Delivery-id namespacing.** `ParseGitLabEvent` prefixes the `X-Gitlab-Event-UUID` with `gitlab:` before it enters the SAME `DeliveryStore` the GitHub path uses, so a GitLab UUID and a GitHub UUID can never collide and dedup one another. A missing `X-Gitlab-Event` or `X-Gitlab-Event-UUID` fails closed (400) — a delivery with no id can't be deduped.
- **Record-then-dispatch, unmark on 5xx.** The receiver `Mark`s the delivery (dedup) BEFORE dispatch, so a concurrent redelivery dedups instead of double-dispatching. The cost is that a dispatch failure would otherwise leave the delivery recorded, so GitLab's retry of the `5xx` would hit the record, dedup to `202`, and permanently drop an event whose processing failed. `dispatchGitLabDelivery` closes that by `Unmark`ing the delivery on a dispatch error (the new `DeliveryStore.Unmark`, implemented by both `MemoryStore` and `PostgresStore`) so the retry re-records and re-processes. The record-store `Mark` failure `5xx` needs no unmark — nothing was recorded. (The dispatch-error branch is currently unreachable for GitLab — no `Handle` path returns a transient error yet — but goes live with E45.8 run creation.)
- **Event mapping.** `Type` normalizes to `object_kind` (`merge_request` / `note` / `issue` / `pipeline` / `build`); `Action` from `object_attributes.action` (MR/issue), `object_attributes.status` (pipeline), or `build_status` (job); `Repo` from `project.path_with_namespace`; `Sender` from `user.username`; `CredentialRef` = `gitlab:<project.id>` (lockstep with `forge/gitlab`'s `scopeRefPrefix`); `Forge` = `gitlab`.
- **Shared matcher.** `MatchGitLabEvent` classifies into the SAME `Match` vocabulary + `Dispatcher.Handle` pipeline. Issue-label adds and `/fishhawk` notes trigger; the note/approve/reject/reply command classification is the shared `classifyCommentCommand` helper (extracted from `matchIssueComment` so the two forges cannot drift). A GitLab access-token bot username (`^(project|group)_\d+_bot`) is skipped to avoid feedback loops.
- **The E45.8 dispatch boundary.** A GitLab `MatchActionRun` parks fail-closed BEFORE the spec-fetch step: the `forge.Forge` interface has no workflow-spec read, comment-backs are GitHub-only, and no `gitlab_ci` runner_kind exists yet, so actual run CREATION from a GitLab trigger is the CI/dispatch phase's work (E45.8 / #1861). Pipeline/job events are recognized-and-skipped naming the same consumer. `merge_request` skips in the matcher because the merge/close lifecycle is consumed server-side (see below).
- **MR merge/close → review stage.** `handleGitLabMergeRequest` drives the ADR-018 `resolveReviewStageOnMerge` shared with the GitHub merge reconciler: action `merge` → review succeeded, `close` → cancelled. The run is resolved by the DURABLE identity both surfaces carry — project path + MR **iid** (`findRunByGitLabMR`) — NOT by exact `object_attributes.url` vs stored `pull_request_url` string equality, because GitLab's webhook/API URL representations are not byte-identical across versions (the `/-/` infix). A normalized-URL pass supplements but never replaces the iid key. **`handleGitLabMergeRequest` now participates in the unmark-on-`5xx` retry contract on a TRANSIENT `RunRepo` lookup failure (E45.21 / #2038):** a `ListRuns` error (either the project-scoped scan or the exact-URL supplement) propagates as a returned error, and the receiver `Unmark`s the already-recorded delivery (via the shared `unmarkGitLabDelivery` helper) and answers `5xx` so GitLab redelivers and re-drives the transition — because this receiver is GitLab's ONLY review-gate signal (the GitHub path has the merge-reconciler poll as a backstop; GitLab has none), so a swallowed lookup error would strand the review stage permanently. A parse failure, a non-terminal action, or a genuine no-match stay best-effort `202`. The harmful consequence is dormant until E45.8 wires GitLab run creation (#1861), the named hard blocker that must land before it.

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
