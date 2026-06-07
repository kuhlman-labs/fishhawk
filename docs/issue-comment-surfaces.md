# Issue-comment surfaces

Inventory of every comment Fishhawk posts to a triggering GitHub issue. The
`backend/internal/issuecomment` package owns all of these; this doc is the
quick map of *what's live* so future work doesn't have to grep-reconstruct
it.

## Active surfaces

| Surface | Audit category | Audit kind | Caller (production) | First posted | Edits in place? |
|---|---|---|---|---|---|
| Sticky status | `status_comment_posted` | `status_update` | `Dispatcher.Handle` (run create); `Server.notifyStatusUpdate` (every stage transition) | run dispatch | Yes — every transition edits the same comment id |
| Plan-on-issue (full) | `issue_commented` | `plan_full` first, `plan_updated` on edits | `Server.notifyPlanReady` from trace + plan handlers; re-fired from approval handlers (#377) | plan-stage terminal | Yes — when the spec's `produces.persistence` has `update_on_change: true` |
| Plan-on-issue (summary) | `issue_commented` | `plan` | `Server.notifyPlanReady` | plan-stage terminal | No (legacy path; chosen when the spec opts out of full-plan rendering) |
| CI-failure retry | `issue_commented` | `ci_retry` | `Dispatcher.handleCIFailureRetry` (#279) | retry dispatch | No (per-attempt dedup; new attempts post new comments) |
| Budget alert (advisory) | `issue_commented` | `budget_alert` | `Server.checkBudgetAlerts` → `NotifyBudgetAlert` (#688) | warn_at / 100% crossing of an advisory periodic budget | No (per-`(period_start, tier)` dedup; the warn comment and the 100% comment each post once per calendar period) |
| Slash-command reply | _(none — no dedup row)_ | _(none)_ | `Server.HandleApprovalCommand` via `replyApproval` | each `/fishhawk approve` or `/fishhawk reject` command | No (every command gets its own reply) |
| Run rejected (misconfigured) | _(none at notifier; global-chain `run_rejected_misconfigured` on the dispatcher)_ | _(none)_ | `Dispatcher.Handle` reviewer-misconfigured guard (#599) | dispatch refusal (agent-gated plan stage, no reviewer wired) | No (each refusal posts its own comment) |

Notes:
- The reaction-polling worker (#360) is a *read-side* concern that reads
  Fishhawk-posted plan comments rather than writing new surfaces; it records
  observed reactions under the separate `plan_reaction_observed` audit
  category and forwards approval-shaped reactions through the same handler
  the typed-reply path uses. Not a surface in its own right.
- The plan-on-issue full surface chooses between create + edit by reading the
  most-recent `plan_full` or `plan_updated` row in the run's audit log
  (`findPlanCommentID`). 404 on edit (operator deleted the comment) falls
  back to a fresh create.
- The plan-on-issue full surface's `_Status:_` footer is driven by the
  latest `approval_submitted` audit row for the plan stage
  (`latestPlanApproval`); pre-approval the footer is omitted.
- The sticky status comment is the *only* surface that follows a run
  end-to-end; everything else is event-scoped.
- The run-rejected surface (`NotifyRunRejected`, #599) is *runless*: the
  dispatcher's plan-review wiring guard (#577) fires before `CreateRun`, so
  there is no run row to scope a comment to. Like the slash-command reply it
  takes explicit issue coordinates (repo, installation, issue number) +
  the offending workflow_id / stage rather than a run UUID, and writes no
  notifier-level audit row — the canonical machine record is the dispatcher's
  `run_rejected_misconfigured` global-chain entry.
- The typed-reply approval path (`+1` / `lgtm` per E17.4) does NOT post its
  own slash reply (`silent=true`) — the user's typed reply *is* the
  acknowledgment. The plan-on-issue comment edit covers the broadcast.
- The review-lifecycle audit categories — `plan_reviewed` /
  `implement_reviewed` (terminal verdicts), `plan_review_skipped` /
  `implement_review_skipped` (degraded gate), `plan_review_started` /
  `implement_review_started` (the #600 dispatch proxy emitted only when
  `reviewers.agent>0` AND a `PlanReviewer` is wired), and `plan_review_failed` /
  `implement_review_failed` (the #664 terminal entry written when a wired
  reviewer errors or times out) — are **internal audit
  kinds, not issue-comment surfaces**. Nothing in `issuecomment` posts them
  to the issue thread. They are written by the plan/trace upload handlers
  and read back by the MCP surface (`fishhawk_get_run_status` /
  `fishhawk_get_plan` `review_status`, and `fishhawk_await_review`) to
  derive the none/pending/complete/skipped/**failed** lifecycle (the
  `*_review_failed` categories feed `fishhawk_await_review`'s terminal
  `failed` resolution). Listed here only so a
  future reader grepping for `*_reviewed` doesn't mistake them for a comment
  surface.
- The plan-gate scope pre-check audit kind — `plan_scope_precheck` (#658),
  written by the plan upload handler (`server/scope_precheck.go::runScopePrecheck`)
  immediately after `plan_generated` and before plan review — is an **internal,
  advisory audit kind, not an issue-comment surface**. Nothing in `issuecomment`
  posts it to the issue thread. It evaluates the uploaded plan's `scope.files`
  against the run's implement-stage path constraints (`forbidden_paths` /
  `allowed_paths` / `max_files_changed`; `required_outcomes` is deliberately
  excluded — see the handler) using the same `backend/internal/policy` matcher as
  the post-implement gate, with payload `{workflow_id, implement_stage_id,
  violations, scanned_files}`. It is advisory + fail-open — a missing/unparseable
  spec or a workflow with no implement stage writes no entry and never blocks the
  upload — and is written even on a clean scope (empty `violations`) so a reader
  can distinguish "checked and clean" from "never checked". Read back by the MCP
  surface (`fishhawk_get_plan` `scope_precheck`, newest entry wins) so an operator
  sees a "scope hits forbidden_paths — wrong workflow?" advisory before approving.
  Listed here only so a future reader grepping the audit categories doesn't
  mistake it for a comment surface.
- The cost-accounting audit kind — `cost_recorded` (#649), written by the
  trace upload handler (`trace.go::recordCost`) once per bundle receipt with
  payload `{model, input_tokens, output_tokens, usd, known_model,
  pricing_as_of, estimated}` — is an **internal audit kind, not an
  issue-comment surface**. Nothing in `issuecomment` posts it to the issue
  thread. It is the canonical per-invocation cost ledger (the per-run rollup
  on `runs.cost_usd_total` is a denormalized read of it); listed here only so
  a future reader grepping the audit categories doesn't mistake it for a
  comment surface.
- The spend-anomaly audit kind — `spend_alert` (#649), written by the trace
  upload handler (`trace.go::checkSpendAlert`) when the current hour's
  estimated model spend exceeds `FISHHAWKD_SPEND_ALERT_MULTIPLE` (default 3x)
  of the rolling average of prior hours, with payload `{latest_hour_usd,
  rolling_avg_usd, ratio, multiple, prior_hours, latest_hour_start,
  triggering_model}` — is an **internal, warn-only audit kind, not an
  issue-comment surface**. It never gates or fails a run; nothing in
  `issuecomment` posts it to the issue thread. The detector
  (`backend/internal/spendalert`) suppresses alerts until a baseline of prior
  hours with spend exists, so a fresh deployment stays quiet. Listed here only
  so a future reader grepping the audit categories doesn't mistake it for a
  comment surface.
- The advisory periodic-budget surface — `budget_alert` (#688 / ADR-030),
  written by the trace upload handler (`trace.go::checkBudgetAlerts` →
  `emitBudgetAlert`) after a `cost_recorded` entry accumulates into
  `runs.cost_usd_total` — IS a comment surface (the table row above), unlike
  the warn-only audit-only `spend_alert`. For each workflow budget with
  `enforcement: advisory`, the handler sums the workflow's spend over the
  current calendar period (timezone-aware in `FISHHAWKD_BUDGET_TIMEZONE`) and,
  on a `warn_at` or 100% crossing, both appends a `budget_alert` audit entry
  (category `budget_alert`, payload `{workflow_id, repo, period, period_start,
  spent, limit, fraction, warn_at, tier, enforcement}`) AND posts the issue
  comment via `NotifyBudgetAlert`. Both are deduped on `(workflow_id,
  period_start, tier)` so each tier fires once per period. It is warn-only and
  best-effort — it never gates, fails, or blocks a run; blocking enforcement
  (admission-time refusal) is a separate scope item. The comment body carries
  the same estimate caveat as the cost ledger: period spend undercounts
  invocations a backend reported no tokens for (`known_usage=false`, #685), so
  actual spend is a lower bound.
- The advisory budget comment-dedup marker — `budget_alert_sent` (#758) — is an
  **internal, system-actor audit kind, NOT an issue-comment surface**. Nothing
  in `issuecomment` posts it to the issue thread; it has no Notifier method. The
  trace upload handler (`trace.go::emitBudgetAlert`) writes it once per
  `(workflow_id, period_start, tier)`, with a `system` actor and payload
  `{workflow_id, period_start, tier}`, ONLY when `NotifyBudgetAlert` actually
  posted the advisory comment. It exists to dedup the visible `budget_alert`
  comment ACROSS runs, decoupled from the `budget_alert` crossing record above:
  the crossing record is written even when the comment is suppressed (non-issue
  run, nil installation), so gating the comment on the crossing record poisoned
  the dedup for the whole period whenever the first over-threshold run could not
  comment (the #758 bug). Because the marker is written only on a real post, a
  comment-less first emission leaves no marker and the next capable run still
  surfaces the comment. Read back only by `budgetAlertCommentSent`. Listed here
  only so a future reader grepping the audit categories doesn't mistake it for a
  comment surface.
- The per-run budget tripwire audit kind — `run_budget_exceeded` (#653 /
  ADR-030) — is an **internal, system-actor audit kind, not an issue-comment
  surface**. Nothing in `issuecomment` posts it to the issue thread; it has no
  Notifier method. The trace upload handler (`trace.go::checkRunBudget`, after
  `recordCost` rolls the bundle's cost into `runs.cost_usd_total`) writes it
  once, with a `system` actor and payload `{dimension (usd|tokens),
  cost_usd_total, max_run_usd, tokens_total, max_run_tokens, terminal_state}`,
  when a run's cumulative spend reaches an operator-configured per-run ceiling
  (`Config.MaxRunUSD` / `Config.MaxRunTokens`, default 0 = disabled). It is the
  third axis of ADR-030's budget story — a global per-run safety rail distinct
  from the per-workflow periodic budgets (`budget_alert` / `run_rejected_budget`)
  and the rate-anomaly `spend_alert`. On breach the handler HALTS the run via
  the cancel transition (terminal state `cancelled`, non-retryable: a budget
  tripwire is a protective system stop, not a work failure, so a runaway run is
  deliberately not auto-redriven) and short-circuits before stage advancement,
  so no further stage is dispatched. The US$ ceiling is enforced against the
  rolled `cost_usd_total`; the token ceiling against the input+output tokens
  summed from the run's `cost_recorded` ledger. Listed here only so a future
  reader grepping the audit categories doesn't mistake it for a comment surface.
- The blocking periodic-budget admission audit kinds — `run_rejected_budget`
  and `run_admitted_budget_override` (#688 / ADR-030) — are **internal,
  admission-time global-chain audit entries, NOT issue-comment surfaces**.
  Nothing in `issuecomment` posts them to the issue thread; they have no
  Notifier method. They follow the `run_rejected_misconfigured` precedent: the
  refusal/override happens before (or in place of) `CreateRun`, so there is no
  run row to scope the entry to — both are written via `AppendGlobalChained`
  with a `system` actor. `run_rejected_budget` (payload `{reason:
  budget_exhausted, workflow_id, repo, period, limit_usd, spent}`; the
  dispatcher path adds `workflow_sha, delivery_id, event`) records a NEW run
  refused because a workflow's current calendar-period spend crossed an
  `enforcement: blocking` budget — written by BOTH admission seams: the HTTP
  handler (`server/budget_admission.go::checkBlockingBudget`, alongside a `402
  budget_exhausted` response) and the webhook dispatcher
  (`dispatcher.go::refusedByBlockingBudget`, no response, no run row).
  `run_admitted_budget_override` (payload `{workflow_id, repo, period,
  limit_usd, spent}`) records an operator forcing a blocked run past via
  `budget_override` — written ONLY on the HTTP-handler seam (the webhook path
  carries no override). In-flight runs and CI-retry / decomposition-child
  continuations are never gated, so neither kind is emitted for them. Listed
  here only so a future reader grepping the audit categories doesn't mistake
  them for comment surfaces.
- The fan-out re-drive parking audit kind — `parent_awaiting_redrive` (#698) —
  is an **internal, system-actor audit kind, not an issue-comment surface**.
  Nothing in `issuecomment` posts it to the issue thread; it has no Notifier
  method. The event-driven parent-resolution path
  (`orchestrator.go::maybeAdvanceDecomposedParent`) writes it once, with a
  `system` actor and payload `{parent_stage_id, retryable_child_run_ids}`, when
  a decomposition parent is left parked in `awaiting_children` because EVERY
  failed child's implement failure is retryable (category A/C or a D SLA
  timeout — see `run.RetryableFailure`). It is the one-time, operator-
  discoverable signal that the parent needs an operator re-drive rather than
  having been resolved to failed-C; the parked state is otherwise silent. The
  interval sweeper (`childcompletion.resolveParent`) parks identically but does
  NOT emit this entry (nor any per-tick log above debug), so an indefinitely-
  parked parent does not spam the chain — discoverability rests on this single
  orchestrator-path entry. Listed here only so a future reader grepping the
  audit categories doesn't mistake it for a comment surface.
- The consolidated-PR audit kind — `consolidated_pr_opened` (#714 / ADR-032) —
  is an **internal, system-actor audit kind, not an issue-comment surface**.
  Nothing in `issuecomment` posts it to the issue thread; it has no Notifier
  method. The orchestrator's consolidated-PR path
  (`orchestrator.go::maybeOpenConsolidatedPR`) writes it once, with a `system`
  actor and payload `{pull_request_url}`, when a decomposed parent reaching its
  review gate opens the single consolidated PR for the whole decomposition. It
  is the audit trail for "the parent — not a child — owns the decomposition's
  one PR." Best-effort: a nil `Audit` or an append failure logs at WARN and never
  unwinds the settle. Listed here only so a future reader grepping the audit
  categories doesn't mistake it for a comment surface.
- The fan-out re-drive action audit kind — `child_redriven` (#698) — is an
  **internal, user-actor audit kind, not an issue-comment surface**. Nothing in
  `issuecomment` posts it to the issue thread; it has no Notifier method. The
  re-drive handler (`server/redrive.go::handleRedriveChild`,
  `POST /v0/runs/{run_id}/redrive`) writes it once on a successful re-drive,
  with the operator's `user` actor + subject and payload
  `{run_id, stage_id, prior_category, prior_reason, prior_failure_class, via}`,
  recording who re-opened the failed child and which implement-stage failure was
  re-driven. The action re-opens the failed child run (`failed` → `running`) and
  its failed implement stage (`failed` → `pending`) so the orchestrator can
  re-dispatch; it is the operator counterpart to the system-emitted
  `parent_awaiting_redrive` park signal above. Listed here only so a future
  reader grepping the audit categories doesn't mistake it for a comment surface.
- The audited category-B override audit kind — `stage_override_retried` (#698) —
  is an **internal, user-actor audit kind, not an issue-comment surface**.
  Nothing in `issuecomment` posts it to the issue thread; it has no Notifier
  method. The retry handler (`server/retry.go::handleRetryStage`,
  `POST /v0/stages/{stage_id}/retry` with `{override: true, reason: "..."}`)
  writes it once on a successful override, with the operator's `user` actor +
  subject and payload `{stage_id, prior_category (always B), prior_reason,
  prior_failure_class, override_reason, retry_ordinal, override_effect,
  admissibility_reason}`. It is kept DISTINCT from the ordinary `stage_retried`
  receipt so the explicit operator escape hatch (who/why) stays separable in
  audit analysis from both a normal retry and #692's automatic empty-diff → C
  reclassification. The override re-opens the category-B stage to `pending`: the
  stage re-runs and the policy gate re-evaluates the new diff — it does NOT
  accept the B-violating diff or bypass the gate (the `override_effect` field
  records this framing). Listed here only so a future reader grepping the audit
  categories doesn't mistake it for a comment surface.
- The implement-review fix-up audit kind — `stage_fixup_triggered` (#762) — is an
  **internal, user-actor audit kind, not an issue-comment surface**. Nothing in
  `issuecomment` posts it to the issue thread; it has no Notifier method. The
  fix-up handler (`server/fixup.go::handleFixupStage`,
  `POST /v0/stages/{stage_id}/fixup`) writes it once on a successful re-open,
  with the operator's `user` actor + subject and payload `{stage_id,
  prior_state, selected_indices, concerns, reason, pass_ordinal, max_passes,
  remaining_budget, admissibility_reason}`. It serves double duty: the canonical
  receipt of who routed which advisory implement-review concerns back to the
  agent, AND **the durable fix-up-pass counter** — the bound (default 1) is
  enforced by counting prior `stage_fixup_triggered` entries for the stage, so
  there is no dedicated column. The `concerns` field (the resolved
  `[]planreview.Concern` the operator selected) is read back by the prompt
  renderer (`server/prompt.go::resolveFixupConcerns`) to deliver the concerns to
  the implement agent as binding instructions (the #558 condition-delivery
  framing). Distinct from the failure-driven `stage_override_retried` / `stage_retried`
  receipts: a fix-up re-opens a HEALTHY review gate (`awaiting_approval → pending`)
  and commits onto the same PR branch rather than regenerating a fresh diff.
  Listed here only so a future reader grepping the audit categories doesn't
  mistake it for a comment surface.

- The fix-up failure-recovery audit kind — `stage_fixup_recovered` (#788) — is an
  **internal, system-actor audit kind, not an issue-comment surface**. Nothing in
  `issuecomment` posts it; it has no Notifier method. `server/fixup.go::maybeRecoverFixupFailure`
  writes it once when a FAILED fix-up re-dispatch is recovered back to the run's
  pre-fix-up review gate (the implement stage restored `failed → succeeded`/`awaiting_approval`
  and the re-parked review stage restored `pending → awaiting_approval`), with a
  `system` actor and payload `{stage_id, restored_state, restored_review_stage_id,
  restored_review_state, source_failure_category, source_failure_reason}`. It is the
  durable record that a fix-up failure was absorbed without making the run a failed
  casualty. Listed here only so a future reader grepping the audit categories doesn't
  mistake it for a comment surface.
- The fix-up push-success audit kind — `fixup_pushed` (#794) — is an **internal,
  system-actor audit kind, not an issue-comment surface**. Nothing in `issuecomment`
  posts it; it has no Notifier method. `server/pullrequest.go::succeedFixupPushStage`
  writes it once (idempotency-guarded on `(stage_id, head_sha)`) when a fix-up
  re-dispatch reports `{outcome:"fixup_pushed"}` after committing onto the EXISTING
  PR branch, with a `system` (or operator, on the bearer path) actor and payload
  `{run_id, stage_id, branch, head_sha, base_sha, files_changed_count, auth_method}`.
  It is the durable record of which commit the fix-up landed onto the open PR; it
  drives the fix-up stage's terminal transition but posts nothing to the issue
  thread (the existing PR's sticky status comment is refreshed via the separate
  `notifyStatusUpdate` hook, not this audit kind). Mirrors the sibling `child_pushed`
  (#771). Listed here only so a future reader grepping the audit categories doesn't
  mistake it for a comment surface.
- The self-consistency invariant kind — `invariant_violation` (#764) — is an
  **internal, system-actor audit kind, not an issue-comment surface**. Nothing in
  `issuecomment` posts it; it has no Notifier method.
  `invariantmonitor.Ticker.checkReviewPRInvariant` writes it from the periodic
  invariant monitor when a run is parked at its review gate (`awaiting_approval`)
  with a null/empty `pull_request_url` despite its workflow intending to open a PR
  (the #742 shape) — a `system` actor and payload `{kind, run_id, reconciled:false}`.
  It is the durable, indexable record (paired with a structured WARN log) that a
  loop-state inconsistency was detected and surfaced for operator action; the
  monitor mutates nothing on this class. Matches the `dispatch_watchdog_elapsed` /
  `children_settled` precedent. Listed here only so a future reader grepping the
  audit categories doesn't mistake it for a comment surface.

## Routing

All surfaces above only fire when the run's `TriggerSource = github_issue`.
PR-triggered and CLI-triggered runs are out of scope for this package —
they have different surfaces and a different conversation locus.

The `Notifier`'s `contextFor` / `contextForStatus` helpers gate the skip:
missing `installation_id`, unparseable `trigger_ref`, or non-issue
`trigger_source` short-circuits before any GitHub call.

## Local-runner runs (#416, #428)

For runs minted with `runner_kind=local`, the backend's `IssueNotifier` is a
no-op by design: the run carries no `installation_id` (the operator's local
flow doesn't go through a GitHub App webhook), so `contextForStatus` returns
early. Comment posting moves to the CLI side, where the operator's authed
`gh` is available.

**Edit-in-place sticky comment (#428).** Every CLI verb that changes run or
stage state calls `ghcomment.PostOrEditStatusComment`, which:

1. `GET /v0/runs/{run_id}/status-comment` — fetches the rendered body (server
   calls `issuecomment.RenderStatusBody`) and the stored `github_comment_id`.
2. `EditOrCreate(repo, issueNumber, githubCommentID, body)` — shells to
   `gh api` to edit the existing comment (if `github_comment_id > 0`) or
   create a new one. Falls back to create on HTTP 404 (deleted comment).
3. `POST /v0/runs/{run_id}/status-comment` — records the returned comment ID
   in the run's audit log (`status_comment_posted` category) so the next call
   can edit in place.

| CLI verb | Sticky comment updated when |
|---|---|
| `fishhawk run start --issue N` | run-create succeeds |
| `fishhawk plan approve <run-id>` | approval submitted |
| `fishhawk plan reject <run-id>` | rejection submitted |
| `fishhawk run cancel <run-id>` | cancellation accepted |
| `fishhawk runner start --run-id … --stage plan` | runner subprocess exits cleanly |
| `fishhawk runner start --run-id … --stage implement` | auto-PR opened OR runner exits (two idempotent calls) |

The CI-failure retry path (`handleCIFailureRetry`) branches on `runner_kind`:
for `local` runs it mints the child run and leaves it in `pending` without
firing `workflow_dispatch`. The discovery signal for the agent is a
`ci_failure_retry_dispatched` audit entry whose payload contains
`"runner_kind":"local"` — the agent polls for this entry, then posts a
retry-minted comment via `gh issue comment` and drives the child run forward.
(This separate comment is agent-authored and append-only; it is not the
sticky-comment surface.)

Missing or unauthed `gh` warns to stderr and proceeds — the run still
records, the issue thread just stays quiet.

Authorship side note: local-run comments are authored by the operator's
GitHub identity (whoever ran `gh auth login`), not by the Fishhawk App. For
local dev this is arguably more honest — the operator IS the one running
the workflow — but the authorship pattern differs from the GHA flow's
bot-authored comments. Reviewers consuming both kinds of runs should keep
this in mind when filtering by author.

## Updating this doc

If you add, remove, or rename a Notifier method (the public surface) or its
audit kind, update the table above as part of the same PR. CI doesn't check
this — the convention does.
