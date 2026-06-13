# Issue-comment surfaces

Inventory of every comment Fishhawk posts to a triggering GitHub issue. The
`backend/internal/issuecomment` package owns all of these; this doc is the
quick map of *what's live* so future work doesn't have to grep-reconstruct
it.

## Active surfaces

| Surface | Audit category | Audit kind | Caller (production) | First posted | Edits in place? |
|---|---|---|---|---|---|
| Living anchor | `status_comment_posted` | `status_update` | `Dispatcher.Handle` (run create); `Server.notifyStatusUpdate` (every stage transition); `Server.notifyPlanReady` (plan-stage terminal) | run dispatch | Yes — one comment per run, every transition rebuilds + edits the same comment id |
| Page-class ping | `anchor_ping_posted` | _(payload `event`)_ | `Notifier.firePings` from `NotifyStatusUpdateForRun` | first crossing of a page-class event (plan gate awaiting human approval, reviewer reject, must_page_human, CI failure) | No — a one-line NEW comment per source event (deduped on the source audit `Sequence`) linking back to the anchor |
| CI-failure retry | `issue_commented` | `ci_retry` | `Dispatcher.handleCIFailureRetry` (#279) | retry dispatch | No (per-attempt dedup; new attempts post new comments) |
| Budget alert (advisory) | `issue_commented` | `budget_alert` | `Server.checkBudgetAlerts` → `NotifyBudgetAlert` (#688) | warn_at / 100% crossing of an advisory periodic budget | No (per-`(period_start, tier)` dedup; the warn comment and the 100% comment each post once per calendar period) |
| Slash-command reply | _(none — no dedup row)_ | _(none)_ | `Server.HandleApprovalCommand` via `replyApproval` | each `/fishhawk approve` or `/fishhawk reject` command | No (every command gets its own reply) |
| Run rejected (misconfigured) | _(none at notifier; global-chain `run_rejected_misconfigured` on the dispatcher)_ | _(none)_ | `Dispatcher.Handle` reviewer-misconfigured guard (#599) | dispatch refusal (agent-gated plan stage, no reviewer wired) | No (each refusal posts its own comment) |

Notes:
- **The living anchor (#1054) subsumes the old plan-on-issue full/summary
  comments.** There is now ONE comment per run (the `status_comment_posted`
  surface), rebuilt from the run's audit chain on every transition by
  `RenderAnchorBody` and edited in place. It projects: a distilled header +
  a next_actions-style "what now" line; the stage list; a collapsed
  `<details>` timeline of interesting audit rows; surfaced reviewer verdicts
  (severity-tagged concern counts inline, free_form in an expandable
  `<details>`); the current plan as a collapsed `<details>` with its summary
  visible; and any superseded plans kept collapsed with the rejection reason
  that retired them. `NotifyPlanReady` no
  longer posts its own comment — it routes to the same anchor rebuild. The
  deleted paths (`notifyFullPlan` / `renderFullPlanBody` / `renderPlanBody`
  and the `plan` / `plan_full` / `plan_updated` posting) are gone;
  `KindPlanFull` / `KindPlanUpdated` are retained only as recognized
  historical kinds the reaction poller may still read on legacy runs.
- **Plan content lives in the artifact store, not the audit chain.** The
  anchor loads the current + superseded plans via the optional
  `Deps.Artifacts` (`PlanArtifactLister`) — the latest plan artifact (by
  `CreatedAt`) across the run's plan stages is current, earlier ones are
  superseded. When the lister is not wired the anchor degrades gracefully and
  omits the plan sections, rendering everything else. Each superseded plan is
  annotated with its rejection reason, derived by aligning the run's plan-gate
  reject decisions (`approval_submitted` with decision `reject`, ascending by
  `Sequence`) to the superseded plan artifacts oldest-first.
- **Reviewer-verdict isolation (binding condition 1).** The anchor counts
  only the verdicts of the MOST-RECENT review dispatch per stage: it floors
  verdict counting at the latest `*_review_started` audit `Sequence` (the
  dispatch boundary, mirroring `decodeReviewVerdicts`' `sinceSeq` floor in
  `fishhawk-mcp/review.go`), so a stale prior-round verdict never reads as the
  current round's state.
- **Body cap.** The anchor body is capped at `MaxIssueCommentBodyBytes`
  (65,536) by a degradation ladder that drops the timeline first, then
  superseded plans, always preserving the header, the current plan summary,
  and the dashboard deep-link.
- **Page-class pings (`anchor_ping_posted`).** GitHub does not notify on
  comment EDITS, so a state change that needs a human is announced by a
  one-line NEW comment linking to the anchor. Page-class events are derived
  from the audit chain:
  - **Plan gate awaiting human approval** — keyed to the LATEST
    `plan_generated`, but emitted ONLY when a plan stage is actually parked at
    `awaiting_approval`. A gateless / routine plan stage never parks, so it
    produces no spurious "awaiting your review" ping.
  - **Reviewer reject** — `plan_reviewed` / `implement_reviewed` with verdict
    `reject`.
  - **must_page_human (ADR-040)** — the concrete must_page_human EVENTS in the
    closed v0 set (`spec.PageEvent*`) are audit categories even though the
    request-time `may_*` delegation knobs are not. Today this surfaces the
    scope-amendment request (`scope_amendment_requested`, an internal audit
    kind that otherwise has NO issue-comment surface and would be silent on
    edits); other closed-set events join here as their categories are wired.
  - **CI failure** — `ci_failure_retry_dispatched` / `ci_retry_exhausted`.

  Each ping records its source audit `Sequence` so a re-render never
  double-pings.
- The reaction-polling worker (#360) is a *read-side* concern that reads the
  anchor comment rather than writing new surfaces; it records observed
  reactions under the separate `plan_reaction_observed` audit category and
  forwards approval-shaped reactions through the same handler the typed-reply
  path uses. It resolves the comment to poll from the anchor's
  `status_comment_posted` id (not the deleted `plan_full`/`plan_updated`
  rows), and — because the anchor exists from run creation, before any plan —
  it gates the approval cutoff on plan EXISTENCE (the first `plan_generated`
  timestamp), dropping any reaction placed before the plan as a non-approval
  (binding condition 2). Not a surface in its own right.
- `PlanStatusFooterForAuditPayload` and its approver-identity rendering
  (#751 / #1053) survive as a shared helper the server's approval seam still
  asserts against; the three-form identity convention (valid GitHub login →
  `@<login>`; operator-agent token subject → "the operator agent
  (`<subject>`, delegated: `<rule>`)"; any other non-login subject → verbatim
  in a sanitized code span; "an approver" only for empty/"anonymous") is
  unchanged.
- The living anchor is the *only* surface that follows a run end-to-end;
  everything else is event-scoped. A plan rejection that spawns a new run
  gets its own anchor on the new run.
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
- The plan-approval completion-gate backstop audit kind —
  `plan_review_backstop_elapsed` (ADR-036 / #875), written by the approval
  handler (`server/approvals.go::checkPlanReviewSettled`) when a plan-stage
  approve is allowed because the hard backstop (`ReviewBudget.Cap` ×
  configured agents, measured from the earliest `plan_review_started`)
  elapsed before the configured agent reviews all reached a terminal state —
  is an **internal, degrade-record audit kind, not an issue-comment
  surface**. Nothing in `issuecomment` posts it to the issue thread. Payload:
  `{stage_id, configured_agents, landed_terminal, started_at,
  elapsed_seconds}`. It exists so a reviewer that dies emitting no terminal
  verdict can never strand the gate, and so the degrade is auditable.
- The implement-review / merge completion-gate backstop audit kind —
  `implement_review_backstop_elapsed` (ADR-036 / #876), written by the
  merge-resolution path
  (`server/pullrequest_review_events.go::checkImplementReviewSettled`) when a
  merge is allowed to resolve the review stage to `succeeded` because the hard
  backstop (`ReviewBudget.Cap` × configured agents, measured from the earliest
  `implement_review_started`) elapsed before the configured agent implement
  reviews all reached a terminal state — is an **internal, degrade-record audit
  kind, not an issue-comment surface**. Nothing in `issuecomment` posts it to
  the issue thread. Payload: `{stage_id, configured_agents, landed_terminal,
  started_at, elapsed_seconds}`. It is the merge-gate twin of
  `plan_review_backstop_elapsed`: it exists so a reviewer that dies emitting no
  terminal verdict can never strand a merge resolution, and so the degrade is
  auditable.
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
- The plan-gate test-sweep audit kind — `plan_test_sweep` (#942), written by
  the plan upload handler (`server/test_sweep.go::runTestSweep`) immediately
  after `plan_surface_sweep` and before plan review — is an **internal,
  advisory audit kind, not an issue-comment surface**. Nothing in
  `issuecomment` posts it to the issue thread. It consults the repository
  tree via the GitHub Contents API (default-branch HEAD) and flags EXISTING
  test files the plan omitted — a stem-sibling test of a scoped production
  `.go` file (`stem_sibling`), existing tests in a package where the plan
  creates a new test file (`new_test_in_tested_package`, capped at 10 names
  + `omitted_count`), or a path-trigger rule's pinned test
  (`migration_walk`: a scoped `migrations/*.sql` requires
  `backend/internal/postgres/postgres_test.go`, scope-set only, #1031) —
  with payload `{findings, scanned_files, listed_dirs}`.
  Advisory + fail-open (nil GitHub client, nil installation, every listing
  failing → no entry, never a block) and written even on a clean sweep
  (empty `findings`) so a reader can distinguish "checked and clean" from
  "never checked". Read back by the MCP surface (`fishhawk_get_plan`
  `test_sweep`, newest entry wins) and rendered into the plan-review
  prompt's gate-evidence section as a reviewer-judged advisory. Listed here
  only so a future reader grepping the audit categories doesn't mistake it
  for a comment surface.
- The cost-accounting audit kind — `cost_recorded` (#649), written by the
  trace upload handler (`trace.go::recordCost`) once per bundle receipt with
  payload `{model, input_tokens, output_tokens, usd, known_model,
  known_usage, pricing_as_of, estimated}`, and by
  `trace.go::recordReviewerCost` once per advisory reviewer invocation with
  the same shape plus `{cached_input_tokens, total_input_tokens, turns,
  source}` (#681/#995/#1010; `input_tokens` is the fresh cache-exclusive
  count, `total_input_tokens` = fresh + cached) — is an **internal audit
  kind, not an issue-comment surface**. Nothing in `issuecomment` posts it
  to the issue thread. It is the canonical per-invocation cost ledger (the
  per-run rollup on `runs.cost_usd_total` is a denormalized read of it);
  listed here only so a future reader grepping the audit categories doesn't
  mistake it for a comment surface.
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
  writes it once on a successful override, with the acting token's subject and
  a kind selected from it (`user` for a human token, `agent` for an
  `operator-agent/<role-spec-version>` token — ADR-040 D4, #1027) and payload
  `{stage_id, prior_category (always B), prior_reason,
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
  with the acting token's subject and a kind selected from it (`user` for a
  human token, `agent` for an `operator-agent/<role-spec-version>` token —
  ADR-040 D4, #1027) and payload `{stage_id,
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

- The concern-waiver audit kinds — `concern_waived` and its corrective
  companion `concern_waive_failed` (#984) — are **internal audit kinds, not
  issue-comment surfaces**. Nothing in `issuecomment` posts them; they have no
  Notifier method. The waive handler (`server/waive.go::handleWaiveConcern`,
  `POST /v0/concerns/{concern_id}/waive`) writes `concern_waived` with the
  acting token's subject and a kind selected from it (`user` for a human
  token, `agent` for an `operator-agent/<role-spec-version>` token —
  ADR-040 D4, #1027) and payload `{concern_id, prior_state,
  reason, stage_kind, severity, category}` BEFORE the state transition — the
  durable-record-first contract: append failure fails the request (500
  `audit_append_failed`, no mutation), so a waive mutation can never exist
  without this entry. When the transition then fails after the append (a
  concurrent transition raced it), the handler appends the `system`-actor
  `concern_waive_failed` corrective entry `{concern_id, intended_state,
  actual_state, error}` (best-effort) so the chain shows intent + outcome in
  every interleaving. The recorded `reason` is read back into later
  implement-review prompts as the waived concern's not-re-litigable context.
  Listed here only so a future reader grepping the audit categories doesn't
  mistake them for comment surfaces.
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
- The fix-up no-changes audit kind — `fixup_no_changes` (#856) — is an **internal,
  system-actor audit kind, not an issue-comment surface**. Nothing in `issuecomment`
  posts it; it has no Notifier method.
  `server/pullrequest.go::succeedFixupNoChangesStage` writes it once
  (idempotency-guarded **stage-keyed**, since no commit landed and there is no
  `head_sha` to dedup on) when a fix-up re-dispatch reports
  `{outcome:"fixup_no_changes"}` after producing no changes on the EXISTING PR
  branch, with a `system` (or operator, on the bearer path) actor and payload
  `{run_id, stage_id, branch, base_sha, files_changed_count, auth_method}` (no
  `head_sha` — the branch tip is unchanged). It drives the fix-up stage's terminal
  transition and re-parks the review gate but posts nothing to the issue thread (the
  existing PR's sticky status comment is refreshed via the separate
  `notifyStatusUpdate` hook, not this audit kind). Mirrors the sibling `fixup_pushed`
  (#794) minus the new commit. Listed here only so a future reader grepping the audit
  categories doesn't mistake it for a comment surface.
- The mid-stage scope-amendment audit kinds — `scope_amendment_requested` /
  `scope_amendment_decided` (#961) — are **internal audit kinds, not issue-comment
  surfaces**. Nothing in `issuecomment` posts them; they have no Notifier methods.
  `server/scope_amendment.go` writes the requested entry (agent actor, payload
  `{amendment_id, paths, reason, remaining_budget}`) when the implement agent files
  a mid-stage scope amendment request, and the decided entry (user actor, payload
  `{amendment_id, decision, reason, decided_by}`) when the operator approves or
  denies it. The requested entry doubles as the operator's `fishhawk_await_audit`
  anchor (#977); delivery to the agent is poll-based (the agent polls the GET
  endpoint), so no comment surface is involved. Listed here only so a future reader
  grepping the audit categories doesn't mistake them for comment surfaces.
- The audit-check publish-health audit kinds — `audit_check_publish_degraded` /
  `audit_check_publish_recovered` (#993) — are **internal, system-actor audit
  kinds, not issue-comment surfaces**. Nothing in `issuecomment` posts them; they
  have no Notifier methods. `server/checks.go::auditCheckPublishDegraded` writes
  the degraded entry (system actor, payload `{head_sha, attempts, last_error}`)
  exactly once per failure episode when the `fishhawk_audit_complete` Check Run
  publish has failed `auditcheckpublisher.DefaultDegradedThreshold` consecutive
  times for a `(run_id, head_sha)`, and `auditCheckPublishRecovered` writes the
  paired recovered entry (payload `{head_sha, attempts}`) on the successful
  publish that closes the open episode. Run-surface visibility comes from
  `fishhawk_get_run_status` / the SPA reading run-chain entries generically; an
  issue-comment mirror would be a separate follow-up surface. Listed here only so
  a future reader grepping the audit categories doesn't mistake them for comment
  surfaces.
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
- The pull-request open-failure audit kind — `pull_request_failed` (#742) — is an
  **internal, audit-only kind, not an issue-comment surface**. Nothing in
  `issuecomment` posts it; it has no Notifier method.
  `server/pullrequest.go::failPullRequestStage` writes it via
  `AuditRepo.AppendChained` with category `pull_request_failed` when the runner
  reports `{outcome:"failed"}` to `POST /v0/runs/{run_id}/pull-request` — the
  commit/push/PR-open step failed after the trace gate left the implement stage in
  `running`. The actor is the request's `actorKind`/`actorSubject` (`system`, or
  operator on the bearer path) and the payload is `{run_id, stage_id, category,
  reason, auth_method}`. It pins the runner's failure category (C retryable via
  `failed → pending`, B parks for re-scope) and reason into the chain so the run
  never reaches `review:awaiting_approval` with a null PR. Listed here only so a
  future reader grepping the audit categories doesn't mistake it for a comment
  surface.
- The gating-reject PR-close audit kind — `pull_request_closed_after_review_reject`
  (#877) — is an **audit kind, not a triggering-issue comment surface**, but it
  DOES post a best-effort comment to the closed PR thread (not via the
  `issuecomment` package — directly via `githubclient.CreateIssueComment`, which
  GitHub routes through the issues endpoint a PR shares). A gating agent
  implement-review (human==0) reject fails the implement stage category-B
  synchronously during the raw-trace upload, BEFORE the runner — which has no view
  of that verdict — opens its PR and POSTs to
  `POST /v0/runs/{run_id}/pull-request`. By then the stage is terminally failed,
  so the PR artifact + `pull_request_opened` audit stay honestly recorded but the
  change will never merge, leaving a dangling open PR.
  `server/pullrequest.go::closePRAfterGatingReject` detects that exact state
  (implement + `failed` + category-B + the `implement_review_rejected` reason
  prefix, the same const the trace failure site stamps), posts the short
  explanatory PR comment, closes the PR via `githubclient.ClosePullRequest`
  (`PATCH .../pulls/{number}` state=closed), then writes this audit entry with a
  `system` actor and payload `{run_id, stage_id, artifact_id, pr_number, pr_url,
  failure_reason}`. The whole step is fail-open: GitHub unconfigured, a nil
  installation id, an unparseable repo, or a close error WARNs and skips (the
  stage is already failed — a failed close must never 500 the handler), and the
  audit entry is written only after a successful close. Closing a PR leaves its
  head branch intact (branch cleanup is out of scope). Listed here so a future
  reader grepping the audit categories understands both its close-comment side
  effect and that it is NOT a triggering-issue surface.
- The run-branch lineage violation kind — `foreign_commit_on_branch` (ADR-035,
  #858) — is an **internal, audit-only kind, not an issue-comment surface**.
  Nothing in `issuecomment` posts it; it has no Notifier method. It is written
  under category `invariant_violation` (shared with the invariant monitor) by a
  `system` actor: `server/lineage.go::recordForeignCommitViolation` calls
  `AuditRepo.AppendChained` when the branch-lineage guard detects a commit on
  the run branch that is not attributable to any of the run's own reported head
  SHAs. The payload is `{kind, run_id, stage_id, offending_sha, head_sha}`. The
  guard also fails the implement stage category-B and fires the sticky status
  comment (`notifyStatusUpdate`), but the audit kind itself posts nothing.
  Listed here only so a future reader grepping the audit categories doesn't
  mistake it for a comment surface.
- The run-branch reset kind — `branch_reset` (ADR-035, #867) — is an
  **internal, audit-only kind, not its own issue-comment surface**. Nothing in
  `issuecomment` posts it; it has no Notifier method. It is written under its
  own category `branch_reset` by an **operator** actor:
  `server/reset_branch.go::handleResetRunBranch` calls `AuditRepo.AppendChained`
  after it force-rewinds a run/PR branch back to its last run-authored HEAD,
  dropping a foreign commit pushed on top. The payload is `{run_id, pr_number,
  branch, dropped_offending_sha, reset_to_sha, prior_head_sha, reason,
  recovery_note}` (plus `reparked_review_stage_id` when a review stage was
  re-parked). The handler DOES refresh the sticky status comment afterward
  (`notifyStatusUpdate(runID, "branch_reset")` → the `status_update` surface in
  the table above, same comment id, edits in place), but the `branch_reset`
  audit kind itself posts nothing. Listed here so a future reader grepping the
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
