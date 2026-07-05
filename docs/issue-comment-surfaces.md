# Issue-comment surfaces

Inventory of every comment Fishhawk posts to a triggering GitHub issue. The
`backend/internal/issuecomment` package owns all of these; this doc is the
quick map of *what's live* so future work doesn't have to grep-reconstruct
it.

## Active surfaces

| Surface | Audit category | Audit kind | Caller (production) | First posted | Edits in place? |
|---|---|---|---|---|---|
| Living anchor | `status_comment_posted` | `status_update` | `Dispatcher.Handle` (run create); `Server.notifyStatusUpdate` (every stage transition); `Server.notifyPlanReady` (plan-stage terminal) | run dispatch | Yes — one comment per run, every transition rebuilds + edits the same comment id |
| Page-class ping | `anchor_ping_posted` | _(payload `event`)_ | `Notifier.firePings` from `NotifyStatusUpdateForRun` | first crossing of a page-class event (plan gate awaiting human approval, advisory reviewer reject, advisory-reject arbitrated, must_page_human, clarification request / awaiting_input park, CI failure, acceptance triage paged, campaign gate hand-off) | No — a one-line NEW comment per source event (deduped on the source audit `Sequence`) linking back to the anchor |
| CI-failure retry | `issue_commented` | `ci_retry` | `Dispatcher.handleCIFailureRetry` (#279) | retry dispatch | No (per-attempt dedup; new attempts post new comments) |
| Budget alert (advisory) | `issue_commented` | `budget_alert` | `Server.checkBudgetAlerts` → `NotifyBudgetAlert` (#688, #1371) | crossing of an advisory periodic-budget ladder rung — `warn` / `over` / `ack_required` (≥2x) / `page` (≥3x) | No (per-`(period_start, tier)` dedup; each tier posts once per calendar period) |
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
- **Base-rebase re-invoke supplemental verdict (ADR-042 / #1250).** On a
  base-rebase re-invoke ship the backend dispatches an ADDITIONAL, bounded
  `implement_reviewed` verdict (`origin=base_rebase_reinvoke`) judging the
  extra scope exemptions the re-invoke honored after the first review. It is
  ADDITIVE, not a replacement: `runSupplementalReinvokeReview` deliberately
  does NOT emit a fresh `implement_review_started`, so the verdict-counting
  floor above stays at the first review's dispatch and the supplemental verdict
  counts ABOVE it — the anchor surfaces the first review's N verdict(s) PLUS
  the supplemental one(s) in the same round (an honest higher count the
  additive floor logic already handles, no anchor code change). This is a
  VERDICT surface (an additional `implement_reviewed` row, rendered by the
  existing anchor/`get_run_status` machinery), NOT a new issue-comment kind;
  the `origin`/`head_sha` provenance fields are retained for idempotency dedup,
  not for a new rendering surface in this slice.
- **Gate-decision timeline projection (#1070).** The anchor timeline renders
  each `approval_submitted` row as a first-class gate-decision entry instead
  of a bare activity line: the approver identity (#1053), a precise decision
  phrase distinguishing approve / approve-with-conditions / reject, an
  explicit "(over N advisory reject(s))" marker when the operator approved
  over reviewer reject verdicts in the same round (bounded to the arbitrated
  round via `advisoryRejectCountBefore`, mirroring the reviewer-verdict
  isolation above), and — for an approve carrying binding conditions — the
  verbatim conditions text (`approval_submitted` payload `comment`) in a
  nested collapsed `<details>`. Reject decisions carry no override marker.
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
  - **Reviewer reject (advisory)** — `plan_reviewed` / `implement_reviewed`
    with verdict `reject`. A reviewer reject is ADVISORY — the operator
    arbitrates the gate — so the ping is worded "🚫 `<model>` flagged a
    blocking concern on the `<stage>` (advisory reject) — awaiting operator
    arbitration." (naming the `reviewer_model`, falling back to "A reviewer")
    and never reads as a gate rejection. The kind token stays
    `<stage>_review_rejected` for dedup parity.
  - **Advisory-reject arbitrated (resolution, #1070)** — `advisory_reject_arbitrated`,
    fired on an `approval_submitted` approve that follows >=1 current-round
    reviewer reject (`advisoryRejectCountBefore('plan', …) > 0`), deduped on
    the approval `Sequence`. It posts "✅ The operator approved the plan over N
    advisory reject(s) — implementing now." so the thread's most-recent
    comment reflects the real gate outcome instead of leaving a stale advisory
    reject as the last word. A clean approve (no preceding advisory reject)
    produces no ping — it stays edit-only on the anchor.
  - **must_page_human (ADR-040)** — the concrete must_page_human EVENTS in the
    closed v0 set (`spec.PageEvent*`) are audit categories even though the
    request-time `may_*` delegation knobs are not. Today this surfaces the
    scope-amendment request (`scope_amendment_requested`, an internal audit
    kind that otherwise has NO issue-comment surface and would be silent on
    edits); other closed-set events join here as their categories are wired.
  - **Clarification request / awaiting_input park (#1057)** —
    `clarification_requested` (`spec.PageEventClarificationRequest`). The
    planner parked the plan stage at `awaiting_input` because the issue is not
    yet plannable, so the operator must answer the parked questions before
    planning resumes. The ping reads "❓ The planner parked this issue for
    direction — N question(s) need your answer before planning resumes." (the
    count is read from the parked document on the `clarification_requested`
    payload; a malformed payload degrades to a count-free phrase). Deduped on
    the `clarification_requested` `Sequence`.
  - **CI failure** — `ci_failure_retry_dispatched` / `ci_retry_exhausted`.
  - **Acceptance triage decision (E31.8, #1536)** — `acceptance_triage_decided`.
    A failed acceptance verdict was triaged (`server/acceptance.go::triageAcceptanceFailure`)
    and the disposition needs a human — the paged variants only (`paged`,
    `rerun_budget_exhausted`, `fixup_unavailable_paged`, `retry_unavailable_paged`,
    `unsettled_paged`, `externally_unvalidatable_paged` — the class-5 all-skip
    externally-unvalidatable terminal page, #1671). The auto-routed dispositions (`fixup_dispatched`,
    `retry_dispatched`) stay **edit-only** — the fixup/retry surfaces already
    render, so a ping there would double-notify. The paged variants are
    otherwise silent on anchor edits, so they get a page-class ping: "🔎
    Acceptance triage — class-`<class>`: `<disposition>` — your decision is
    needed." (`ping.go::acceptanceTriageNeedsHuman` gates the ping on the
    disposition; a malformed/empty payload fires none). Deduped on the
    `acceptance_triage_decided` `Sequence`.
  - **Campaign gate hand-off (E25.7, #1446)** — `campaign_gate_paged`. The
    campaign auto-driver REFUSED a must_page_human gate (reviewer_reject /
    requirement_arbitration), paused the affected issue, and handed the gate to
    a human. The run-chained `campaign_gate_paged` entry is otherwise silent on
    anchor edits, so it gets a page-class ping: "🛑 The campaign auto-driver
    paused this issue and needs you: <gate/decision>." (the gate phrase is read
    from the `page_event` field; an unknown/absent value degrades to "a gate
    decision"). Deduped on the `campaign_gate_paged` `Sequence`.

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
- The reviewer-capability audit kinds (#1495) — `reviewer_capability_unavailable`
  (global-chain, written by `handleCreateRun` when a spec-declared reviewer's
  provider is unavailable on the deployment but a backend IS wired, so the run
  degrades and proceeds instead of hard-failing) and the capability-framed
  `plan_review_skipped` / `implement_review_skipped` **reason** `reviewer_unavailable`
  (the runtime degradation point, carrying `provider` + `optional`) — are
  **internal, degrade-record audit kinds, not issue-comment surfaces**. Nothing
  in `issuecomment` posts them to the issue thread. The coarse no-backend-at-all
  gate still emits `run_rejected_misconfigured` + a customer comment via
  `NotifyRunRejected` (below); the finer per-reviewer capability gap does not
  reject and posts no comment. Listed here so a reader grepping for reviewer
  audit kinds sees the full non-comment set.
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
- The plan-gate acceptance pre-check audit kind — `plan_acceptance_precheck`
  (#1533, ADR-049 decision #4), written by the plan upload handler
  (`server/acceptance_precheck.go::runAcceptancePrecheck`) between
  `plan_scope_regression` and plan review — is an **internal, advisory audit
  kind, not an issue-comment surface**. Nothing in `issuecomment` posts it to
  the issue thread. It runs **only when the run's workflow configures an
  acceptance stage** (stage-conditional off-switch), decodes
  `verification.acceptance_criteria` from the raw plan body (NOT `plan.Parse`,
  so a duplicate-id plan is flagged rather than fail-open), and evaluates the
  deterministic rules `no_blocking_criterion` (no effectively-blocking
  criterion and no `verification.out_of_scope` justification),
  `missing_source_ref` (explicit criterion without `source_ref`),
  `missing_rationale` (inferred criterion without `rationale`), `empty_id`, and
  `duplicate_id`, with payload `{workflow_id, acceptance_stage_id, findings,
  criteria_count, blocking_count, out_of_scope_count}`. Advisory + fail-open —
  nil repos, a missing/unparseable spec, no acceptance stage, or an unmarshal
  error writes no entry and never blocks the upload — and written even when
  clean (empty `findings`) so a reader can distinguish "checked and clean" from
  "never checked". Threaded into the plan-review prompt's gate-evidence section
  as a machine-verified finding. Listed here only so a future reader grepping
  the audit categories doesn't mistake it for a comment surface.
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
- The operator-scope-undelivered audit kind — `operator_scope_path_undelivered`
  (#1407), written by the implement-review assembly path
  (`trace.go::runImplementReviews`) before any reviewer verdict — is an
  **internal, advisory audit kind, not an issue-comment surface**. Nothing in
  `issuecomment` posts it to the issue thread. It fires when an implement commit
  leaves an OPERATOR-DELIBERATELY-added scope path (an `add_scope_files` path
  folded at plan approval, or an approved mid-stage scope amendment) UNTOUCHED —
  the deterministic detection is untouched-only: a path ABSENT from the
  committed diff's file set. (A path the commit DID touch but with the wrong
  content is undecidable deterministically and stays a review concern — e.g. the
  E23.9 never-created case is caught here, the E23.10 wrong-content case is a
  review catch.) Payload `{undelivered_paths, undelivered_count,
  operator_added_count}`. Advisory + best-effort — a nil `ScopeAmendmentRepo` or
  a `ListByRun` error contributes nothing and never blocks the review, and the
  entry is written only when the undelivered set is non-empty (an all-delivered
  commit keeps the prompt byte-identical and emits no entry). The same set is
  rendered into the implement-review prompt's gate-evidence section as a
  high-priority `operator_scope_path_undelivered` warning. The complementary
  BLOCKING gate for a FULLY-untouched concrete DECLARED scope path is the
  runner's #1151/#1231 scope-completeness park; this signal is the advisory
  pre-review surface for the partial / operator-added case. Listed here only so a
  future reader grepping the audit categories doesn't mistake it for a comment
  surface.
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
  on a crossing of the escalating ladder rung (#1371) — `warn` (warn_at),
  `over` (100%), `ack_required` (the configured ack multiple of the effective
  limit, default 2x), or `page` (the page multiple, default 3x) — both appends
  a `budget_alert` audit entry (category `budget_alert`, payload `{workflow_id,
  repo, period, period_start, spent, limit, fraction, warn_at, tier,
  enforcement}`, where `limit`/`fraction` are against the effective limit —
  `FISHHAWKD_BUDGET_LIMIT_OVERRIDE_USD` when set, else the spec `limit_usd`)
  AND posts the issue comment via `NotifyBudgetAlert`. Both are deduped on
  `(workflow_id, period_start, tier)` so each tier fires once per period. It is
  warn-only and best-effort — it never gates, fails, or blocks a run; blocking
  enforcement (admission-time refusal) is a separate scope item. The comment
  body carries
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
- The escalating periodic-budget plan-gate audit kinds —
  `plan_violates_periodic_budget` and `plan_periodic_budget_tier_acknowledged`
  (#1371) — are **internal, system-actor audit kinds, NOT issue-comment
  surfaces**. Nothing in `issuecomment` posts them; they have no Notifier
  method. The approval handler (`approvals.go::checkPeriodicBudgetTier`) writes
  one once per plan-stage approve whose advisory periodic budget has escalated
  to the `ack_required`/`page` tier: `plan_violates_periodic_budget` alongside
  the `422 periodic_budget_requires_ack` refusal when the comment lacks
  `--ack-budget`, or `plan_periodic_budget_tier_acknowledged` when it carries
  it. Both use a `system` actor and payload `{stage_id, workflow_id, period,
  spent, limit, fraction, tier, ack_multiple}`. They mirror the
  `plan_violates_budget` / `plan_budget_override_acknowledged` runtime-budget
  pattern. Listed here only so a future reader grepping the audit categories
  doesn't mistake them for comment surfaces.
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
- The slice-integration audit kinds — `slices_integrated` and
  `slice_integration_conflict` (ADR-041 / #1142) — are **system-actor audit
  kinds with no dedicated Notifier method**, but as of E24.7 (#1147) both ALSO
  appear in the living-anchor timeline (the `status_comment_posted` surface).
  They are not posted by a dedicated Notifier method or `issuecomment` call;
  instead they render **data-drivenly** through the `activityCategories` set in
  `status_template.go` (rendered as "Slices integrated" / "Slice integration
  conflict"), which both `RenderStatusBody` and `renderAnchorTimeline` consume —
  so the fan-in outcome surfaces in the living anchor with no per-kind Notifier
  code. The fan-in step (`orchestrator.go::integrateSlices`, invoked from
  BOTH settle paths — `maybeAdvanceDecomposedParent` and
  `childcompletion.resolveParent`) writes one of them when a decomposed
  parent's children have all succeeded and their per-slice branches are
  merged onto the consolidated branch in ascending slice-index order.
  `slices_integrated` (payload `{child_run_ids, consolidated_branch,
  slice_count}`) records a clean fan-in — every slice merged — and is
  consumed by E24.7. `slice_integration_conflict` (payload
  `{parent_stage_id, conflicting_slice_index, conflicting_child_run_id}`)
  records a merge conflict (HTTP 409) that failed the parent implement
  (awaiting_children) stage category-B RECOVERABLE; its STRUCTURED payload
  is the machine resume target the `next_actions`
  `slices_integration_conflict` arm reads back (the
  `conflicting_child_run_id`), never parsing the stage's free-form failure
  reason. Both follow the `children_settled` / `consolidated_pr_opened`
  precedent: best-effort (a nil `Audit` or append failure logs at WARN and
  never unwinds the settle). The MCP `fishhawk_get_run_status`
  `children_status` block (#1147) also reads these kinds to classify the
  decomposed parent's integration phase (`integrated` /
  `integration_conflict`) and surface `consolidated_branch` /
  `conflicting_child_run_id`.
- The implement-model resolution audit kind — `model_resolved` (#1013) — is
  **INTRODUCED by the operator-gate slice** as the source-tagged record of the
  implement model the plan gate resolved, and it ALSO appears in the
  living-anchor / status-comment surfaces. The approval handler
  (`server/approvals.go::handleSubmitApproval` → `writeModelResolvedAudit`) is
  the SOLE writer: on a valid plan-stage approve it resolves the ladder
  (deployment default < spec `executor.model` < plan `model_recommendation` <
  the `implement_model` operator override), validates the resolved value against
  the deployment allow-list (rejecting `422 plan_invalid_model` pre-Submit on an
  unknown model), and writes ONE entry with payload `{model, model_source}`
  (`ResolvedModel`'s json tags; `model_source` ∈ default|spec|plan|operator, or
  empty for the deliberate default spawn). The slice-1 reader
  (`modelpolicy.go::gateResolvedModel`) consumes the most-recent entry to route
  the runner spawn's `--model`; the trace/calibration path deliberately never
  emits it (trace surface-sweep guard). On the issue thread it renders
  **data-drivenly** through the `activityCategories` set in `status_template.go`
  ("Implement model resolved: `<model>` (source: `<rung>`)", or "adapter
  default" for an empty resolution), which both `RenderStatusBody` and
  `renderAnchorTimeline` consume; the anchor additionally renders a dedicated
  "**Implement model**" block (`anchor_template.go::renderResolvedModel`) and the
  plan's `model_recommendation` (implement_model + rationale) under the plan
  section. Best-effort: a nil `Audit` or append failure logs and never unwinds
  the approval the gate already recorded.
- The deploy-stage governance audit kinds — `deployment_dispatched`,
  `deployment_outcome_recorded`, `deployment_rollback_initiated`, and
  `deployment_rollback_completed` (E23.5 / #1385, ADR-038) — are **system-actor
  audit kinds with NO dedicated Notifier method**, but (like
  `slices_integrated` / `model_resolved`) they ALSO appear in the living-anchor
  timeline (the `status_comment_posted` surface). Nothing in `issuecomment`
  posts them via a dedicated method; instead they render **data-drivenly**
  through the `activityCategories` set in `status_template.go` (rendered as
  "Deploy dispatched to `<env>`" / "Deployed to `<env>` — <outcome>" / "Deploy
  rollback initiated to `<env>`" / "Deploy rollback completed to `<env>`"),
  which both `RenderStatusBody` and `renderAnchorTimeline` consume — so the
  deploy dispatch, settled outcome, and any rollback sub-action surface on the
  living anchor with no per-kind Notifier code (and `notifier.go`'s actor
  @-mention render surface is genuinely uninvolved: these are fixed
  system-actor verb phrases that never `@`-mention an approver). The ship
  handler (`server/deployment.go::handleShipDeployment`,
  `POST /v0/runs/{run_id}/deployment`) is the SOLE writer of
  `deployment_outcome_recorded` (payload `{run_id, stage_id, artifact_id,
  content_hash, environment, ref, external_run_url, outcome, rollback_handle,
  auth_method}`, written on every persisted `deployment` artifact) and — when
  the body carries a `rollback_action` — the matching
  `deployment_rollback_initiated` / `deployment_rollback_completed` entry
  (payload `{run_id, stage_id, artifact_id, environment, rollback_handle,
  rollback_action, auth_method}`, best-effort: an append failure WARN-logs and
  never unwinds the recorded artifact + outcome). Delivery guarantee (#1396): the
  primary `deployment_outcome_recorded` entry is NOT best-effort — a failed
  append 500s the request, and because the artifact persist and the audit append
  are two non-atomic steps, the handler's idempotent-retry path (GetByHash hit)
  self-heals: it verifies an outcome entry exists for the persisted artifact and
  appends it if missing, so a retry after a partial Create-succeeded /
  AppendChained-failed 500 ends with BOTH the artifact and its governance entry
  present (the rollback sub-action entries stay best-effort and are NOT healed).
  `deployment_dispatched` is
  EMITTED by the E23.4 deploy stage machine (the pre-execution-gated dispatch),
  not by this handler; it is introduced as a constant + surfaced on the timeline
  here so dispatch and outcome render consistently. The handler refreshes the
  sticky living-anchor comment via the separate `notifyStatusUpdate` hook (like
  the PR-upload handler), not by posting a dedicated comment.
- The acceptance-stage evidence audit kinds — `acceptance_dispatched`,
  `acceptance_outcome_recorded`, and `acceptance_triage_decided` (E31.3 / #1531,
  ADR-049) — are **system-actor audit kinds with NO dedicated Notifier
  method**, mirroring the deploy governance kinds above (and
  `slices_integrated` / `model_resolved`): nothing in `issuecomment` posts them
  via a dedicated method. Instead they render **data-drivenly** through the
  `activityCategories` set in `status_template.go` (rendered as "Acceptance
  dispatched" / "Acceptance recorded — `<outcome>` (`<passed>`/`<total>`
  criteria passed)" / "Acceptance triage — class-`<class>`: `<disposition>`",
  each degrading field-by-field to its bare verb when a payload field is
  absent/undecodable), which both `RenderStatusBody` and `renderAnchorTimeline`
  consume — so the acceptance dispatch, recorded outcome, and triage
  disposition surface on the living anchor with no per-kind Notifier code (and
  `notifier.go`'s actor `@`-mention render surface is genuinely uninvolved:
  these are fixed system-actor verb phrases that never `@`-mention an
  approver). They follow the same deployment-precedent exemption from the
  notifier surface-sweep: adding a system-actor render-only audit kind adds no
  Notifier method (the change that added the deploy kinds, PR #1395 / commit
  f227dbb, likewise never touched the notifier source). The payload tags
  (`{outcome, criteria_passed, criteria_total, class, disposition}`) DEFINE the
  contract the writers emit to: the E31.6 acceptance-outcome handler writes
  `acceptance_dispatched` / `acceptance_outcome_recorded`, and the E31.8 triage
  (`server/acceptance.go::triageAcceptanceFailure`, #1536) writes
  `acceptance_triage_decided`. **`acceptance_triage_decided` is now WRITTEN by
  E31.8** — one chained entry per triaged failed verdict, payload
  `{run_id, stage_id, artifact_id, class, disposition, criterion_ids,
  failure_mode, prior_routed_passes, reason}`. The **disposition vocabulary**
  is a closed set: the auto-routed `fixup_dispatched` (class-1 → bounded
  implement fix-up) / `retry_dispatched` (class-2 → acceptance-stage reopen);
  and the human-paged `paged` (class-3/class-4) / `rerun_budget_exhausted` (the
  per-run re-run cap of 2 hit) / `fixup_unavailable_paged` (a class-1 fix-up
  route refused — budget/ceiling/not-applicable) / `retry_unavailable_paged` (a
  class-2 reopen refused) / `unsettled_paged` (the acceptance stage was not yet
  settled `succeeded` at ship time). The auto-routed dispositions stay
  render-only edit surfaces; the paged variants ALSO fire the page-class ping
  registered above. The class-3 entry keyed by `criterion_ids` is the durable
  per-criterion disposition record E31.11 consumes.
- The operator-gated acceptance re-open audit kind — `acceptance_reopened`
  (E31.16 / #1567) — is a **system-/user-actor audit kind with NO dedicated
  Notifier method and NO dedicated timeline render**, following the same
  posture as the deploy governance kinds and the acceptance triage kinds above.
  It is WRITTEN by TWO sites, both re-opening a settled acceptance stage:
  (1) the retry handler's acceptance-reopen branch
  (`server/retry.go::retryAcceptanceOutcomeUnknown`) when an operator re-opens
  an acceptance stage that settled `succeeded` with no recorded verdict; and
  (2) the fix-up push handler
  (`server/acceptance.go::reopenAcceptanceOnFixupPush`, #1682) when a fix-up
  push lands a NEW head AFTER a verdict-ful acceptance stage settled — the prior
  verdict is now bound to a stale commit, so the stage is re-opened to
  re-validate the final commit (payload `{stage_id, prior_state, head_sha,
  reason}`). This kind is REUSED for the #1682 invalidation rather than adding a
  new issue-comment surface. Both sites have no page-class ping of their own —
  the status refresh rides the `notifyStatusUpdate` hook (like the PR-upload
  handler), not a dedicated comment. Listed here so a future reader grepping the
  acceptance audit categories doesn't mistake it for a comment surface.
- The bounded-retry give-up audit kind — `slice_integration_failed` (#1243) —
  is a **system-actor audit kind with no dedicated Notifier method and is NOT
  an issue-comment surface**. The child-completion sweeper
  (`childcompletion.resolveParent`) retries a non-conflict `IntegrateSlices`
  error every tick; to avoid log-spamming a deterministically-failing
  integration forever, it counts CONSECUTIVE non-conflict errors per parent
  and, on the `maxIntegrationAttempts`-th (5th) failing tick, fails the parent
  implement (awaiting_children) stage category-B RECOVERABLE and writes this
  entry. It is the bounded-retry give-up TERMINAL event, distinct from
  `slice_integration_conflict` (a 409 merge conflict, which fails on the first
  tick). Payload `{parent_stage_id, attempts, error}` records the attempt count
  and the persistent error string. Best-effort: a nil `Audit` or append failure
  logs at WARN and never unwinds the give-up. Listed here only so a future
  reader grepping the audit categories doesn't mistake it for a comment surface.
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

- The plan-gate revise audit kind — `plan_revised` (#1099) — is an
  **internal, user-actor audit kind, not an issue-comment surface**. Nothing in
  `issuecomment` posts it to the issue thread; it has no Notifier method. The
  revise handler (`server/revise.go::handleRevisePlan`,
  `POST /v0/stages/{stage_id}/revise`) writes it once on a successful re-open,
  with the acting token's subject and a kind selected from it, and payload
  `{stage_id, prior_state, conditions, pass_ordinal, max_passes, hard_ceiling,
  remaining_budget, forced, actor}`. It serves double duty: the canonical
  receipt of who re-planned the plan stage in place against which binding
  operator constraint, AND **the durable revise-pass counter** — the bound
  (default 1) is enforced by counting prior `plan_revised` entries for the
  stage, so there is no dedicated column (exactly as `stage_fixup_triggered`
  bounds fix-up). The `conditions` field (the rendered operator constraint) is
  read back by the prompt renderer (`server/prompt.go::loadRevisionConstraint`)
  to deliver the constraint to the planner as a binding "Revision constraint"
  prompt section (the #558 condition-delivery framing), with the prior plan
  carried as the revision base. The plan-stage analogue of
  `stage_fixup_triggered`. Listed here only so a future reader grepping the
  audit categories doesn't mistake it for a comment surface.

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

- The concern-defer audit kinds — `concern_deferred` and its corrective
  companion `concern_defer_failed` (#1202) — are **internal audit kinds, not
  issue-comment surfaces**. Nothing in `issuecomment` posts them; they have no
  Notifier method. The defer handler (`server/defer_concern.go::handleDeferConcern`,
  `POST /v0/concerns/{concern_id}/defer`) converts an open concern into a
  follow-up work item and transitions it to terminal `deferred`. Unlike the
  waiver's audit-before-mutation contract, defer is **audit-AFTER-transition**
  (a GitHub issue is a durable side effect, so the issue is filed first, the
  concern transitions, and only THEN is the success fact recorded): the handler
  writes `concern_deferred` with the acting token's subject + a kind selected
  from it (`user`/`agent`, as for waive) and payload `{concern_id, prior_state,
  reason, stage_kind, severity, category, issue_number, issue_url, issue_type,
  issue_title, issue_provider}` ONLY AFTER the state transition succeeds — so the
  entry is a fact, never an attempt. When the transition fails after a successful
  filing (a concurrent writer raced it), the handler appends ONLY the
  `system`-actor `concern_defer_failed` corrective entry `{concern_id,
  intended_state, actual_state, issue_number, issue_url, error}` (best-effort,
  naming the orphaned issue) and returns 422 — never a success `concern_deferred`
  entry for a transition that did not happen. Listed here only so a future reader
  grepping the audit categories doesn't mistake them for comment surfaces.
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
  Since #1165/#1213 the payload additionally carries an optional `apply_path` field
  — one of `applied` | `agent` | `apply_failed_fellback` | `apply_failed_reset_failed`
  — present **only on the `fixup_pushed` variant** and only when the runner reports a
  recognized value (an absent or unknown value omits the key). It records whether the
  fix-up collapsed to a deterministic git-apply of the routed concerns' suggested
  patches or fell back to the agent.
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
- The scope-completeness park/decision audit kinds — `scope_completeness_parked`,
  `scope_completeness_exempted`, and `scope_completeness_failed` (#1231) — are
  **internal audit kinds, not issue-comment surfaces**. Nothing in `issuecomment`
  posts them; they have no Notifier methods.
  `server/pullrequest.go::parkScopeCompletenessStage` writes the parked entry
  (system actor, or operator on the bearer path; payload `{run_id, stage_id,
  branch, head_sha, base_sha, verified_tree_sha, missing_paths, auth_method}`)
  when the runner reports `{outcome:"scope_park"}` — the implement stage's ONLY
  committed-tree gate failure was the missing-declared-scope-file check and it
  pushed the verified commit to the run branch (no PR), parking in
  `awaiting_scope_decision`. `server/scope_completeness.go::handleDecideScopeCompleteness`
  writes the exempted entry (user actor; payload `{run_id, stage_id, decision,
  reason, decided_by, held_commit_sha, run_branch, verified_tree_sha,
  missing_paths, gate_evidence}`) when an operator accepts the already-committed
  tree — the held commit's PR is then opened with no agent re-run — and the failed
  entry (user actor; payload `{run_id, stage_id, decision, reason, decided_by,
  missing_paths}`) when an operator rejects the exemption and the stage drops to
  category-B. The `gate_evidence` field on the exempted entry reuses the #1153
  channel so a downstream implement-review gate reads the shortfall as
  operator-exempted rather than re-failing on it. Delivery to the operator is
  poll-based via `fishhawk_get_run_status` / `next_actions` (sibling slice); no
  comment surface is involved. Listed here only so a future reader grepping the
  audit categories doesn't mistake them for comment surfaces.
- The supplemental scope-exemption audit kind — `scope_files_exempted` (#1218) —
  is an **internal, audit-only kind, not an issue-comment surface**. Nothing in
  `issuecomment` posts it; it has no Notifier method. It is the **supplemental
  post-seal audit row** (NOT an issue comment) the success pull-request handler
  (`server/pullrequest.go::handleShipPullRequest`) writes via
  `AuditRepo.AppendChained` — best-effort (a nil `AuditRepo` or append failure
  WARNs and never unwinds the upload) — ONLY when a success ship carries
  `supplemental_scope_exemptions`, i.e. after a base-rebase re-invoke. On that
  path the runner reloads the re-invoked agent's freshly-validated scope
  self-exemptions AFTER the trace bundle (which folds the FIRST attempt's
  exemptions into its own bundle-sealed `scope_files_exempted` gate_evidence
  event) already sealed and shipped under #742 forward gating, so this row
  re-surfaces the re-invoke exemption delta the sealed bundle could not carry.
  The actor is the request's `actorKind`/`actorSubject` (`system`, or operator on
  the bearer path) and the payload is `{run_id, stage_id, exemptions:[{path,
  reason}], origin:"base_rebase_reinvoke", auth_method}` — the `origin` marker
  distinguishes this re-invoke re-emission from the bundle-sealed first-attempt
  event. The implement-review gate_evidence does NOT also receive this delta: the
  review is dispatched at trace-upload time, strictly BEFORE the re-invoke, so
  re-feeding it would require re-dispatching the review or re-sealing the bundle —
  both precluded by #742 forward gating (the gate_evidence half is deferred to
  #1250). Listed here only so a future reader grepping the audit categories
  doesn't mistake it for a comment surface.
- The operator-vouch audit kind — `operator_commit_vouched` (ADR-035, #1044) — is an
  **internal, user-actor audit kind, NOT an issue-comment surface**. Nothing in
  `issuecomment` posts it; it has no Notifier method. `server/vouch.go::handleVouchCommit`
  writes it (user actor, payload `{run_id, vouched_sha, reason}`) when an operator
  declares a foreign commit on a run branch to be run-authored lineage, un-wedging the
  merge reconciler. Unlike `branch_reset` (below), the vouch does **not** even refresh
  the sticky status comment — it is a pure ledger declaration. The #1067 living anchor
  comment projects the entry via the audit chain like any other category. Listed here
  only so a future reader grepping the audit categories doesn't mistake it for a comment
  surface.
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
- The work-item filing kind — `work_item_filed` (#1005) — is an **internal,
  audit-only category, not an issue-comment surface**. Nothing in `issuecomment`
  posts it; it has no Notifier method. It is written under its own category
  `work_item_filed` by the acting caller (`actor_kind` resolved from the token
  subject per ADR-040 D4 — `agent` for the operator-agent role instance, `user`
  otherwise): `server/workitems.go::auditWorkItemFiling` calls
  `AuditRepo.AppendChained` after `POST /v0/work-items` files a work item, but
  ONLY when the request's `run_id` names a run that is in flight (non-terminal).
  The payload is `{type, title, provider, created_url, created_number,
  applied_labels, board_column, status, defaulted_labels,
  missing_label_namespaces}` — the last two carrying the #1616 label-completeness
  report (system-added labels the caller did not supply, and any required
  namespace still absent). The write is best-effort — filing
  succeeds with no `run_id` (the operator-agent follow-up filing path), and an
  append failure never fails the response since the item is already filed. No
  sticky status comment is refreshed. Listed here so a future reader grepping
  the audit categories doesn't mistake it for a comment surface.
- The refinement-gate decision + edit kinds — `refinement_draft_approved`,
  `refinement_draft_rejected`, and `refinement_draft_edited` (#1593, ADR-052
  option A) — are **internal, global-chain audit-only categories, not
  issue-comment surfaces**. Nothing in `issuecomment` posts them; they have no
  Notifier method. They are written by the acting caller (`actor_kind` = `user`)
  in `server/refinement.go`'s E34.2 preview + approval gate handlers via
  `AuditRepo.AppendGlobalChained` (a refinement session is not a run, so the
  entry rides the global chain with `run_id` NULL). The `_approved` / `_rejected`
  entry carries `{session_id, draft_id, revision, content_hash, reason}`; the
  `_edited` entry carries `{session_id, draft_id, revision, origin,
  content_hash}`. Unlike `work_item_filed`, the write is NOT best-effort: the
  audit entry IS the gate's record, so it is appended BEFORE the decision/edit is
  persisted (durable-before-state-change) and an append failure fails the request
  `500` with nothing persisted — no gate action is ever unaudited. Listed here so
  a future reader grepping the audit categories doesn't mistake them for comment
  surfaces.
- The refinement filing-executor kind — `refinement_filing_completed` (#1594,
  ADR-052 filing half, E34.3) — is likewise an **internal, global-chain
  audit-only category, not an issue-comment surface**. Nothing in `issuecomment`
  posts it; it has no Notifier method. It is written by the acting caller
  (`actor_kind` = `user`) in `server/refinement_file.go`'s filing handler via
  `AuditRepo.AppendGlobalChained` once the approved draft's epic + children have
  all filed and the epic passed the epic-children round-trip. The entry carries
  `{session_id, draft_id, content_hash, repo, epic_number, child_numbers,
  verified}`. Like the decision/edit kinds the write is NOT best-effort: it is
  appended BEFORE the filing session's `completed_at` is flipped
  (durable-before-state-change), so an append failure fails the request `500`
  with the session left open for a retry — the session close is never unaudited.
  Listed here so a future reader grepping the audit categories doesn't mistake it
  for a comment surface.
- The board-state-sync kind — `work_item_transitioned` (#1012) — is an
  **internal, audit-only category, not an issue-comment surface**. Nothing in
  `issuecomment` posts it; it has no Notifier method. It is written under its
  own category `work_item_transitioned` by the `system` actor in
  `server/boardsync.go::auditBoardTransition`, which the best-effort
  `notifyBoardTransition` hook calls after attempting a run-lifecycle board
  move. The hook fires from four lifecycle points — run created
  (`run_started`, via the webhook dispatcher's `BoardSyncer` AND, for
  local-runner / API-created runs, `handleCreateRun` in `server/runs.go` so the
  edge is no longer webhook-exclusive; #1123), PR opened
  (`pr_opened`), run failed (`run_failed`), and PR merged (`run_merged`) — and
  moves ONLY the project board Status column (the #1005 scope split: labels,
  fields, and epic links belong to filing). An entry is written for BOTH a
  landed move AND a deliberate skip (the never-fight-the-human guard: a card a
  human parked outside the expected source status is left untouched), so the
  payload — `{trigger, issue_number, canonical_state, from, to, moved, skipped,
  skip_reason}` — records what happened either way. Unlike `work_item_filed`,
  this entry is NOT gated on the run being non-terminal: `run_merged` and
  `run_failed` fire as the run reaches a terminal state. The write is
  best-effort (the board move, if any, already happened) and never unwinds the
  run. Listed here so a future reader grepping the audit categories doesn't
  mistake it for a comment surface.
- The product-feedback egress kind — `product_report_filed` (#1006) — is an
  **internal, source-side audit-only category, not a run-thread comment
  surface**. Nothing in `issuecomment` posts it; it has no Notifier method. It
  is written under its own category `product_report_filed` by the acting caller
  (`actor_kind` resolved from the token subject) in
  `server/product_report.go::auditProductReport` after `POST
  /v0/runs/{run_id}/product-reports` files or dedups a report. Since the
  entitlement was widened (#1274), the acting caller may be the run's own
  run-bound agent token OR a non-run-bound operator/operator-agent bearer (with
  `write:runs`) OR a cookie-session operator — so the entry's `actor_kind`,
  resolved from the subject, may now name an operator/operator-agent
  (`actor_kind=agent` for the `operator-agent/` prefix) in addition to the
  run-bound agent. No new surface or audit category is added. It names ONLY
  what left the boundary — `{fingerprint, destination, action
  (created|occurrence), upstream_url, upstream_num}` — and carries no diffs,
  paths, prompts, free text, or audit payload bodies. The write is best-effort
  (the egress already happened). Listed here so a future reader grepping the
  audit categories doesn't mistake it for a comment surface.
- The product-feedback **occurrence comment** (#1006) IS an egress comment
  surface, but a distinct one: on a fingerprint dedup hit the GitHub
  `FeedbackProvider` (`workmgmt/github/feedback.go`) posts a product-facts-only
  occurrence comment to the matched issue **in the FIXED upstream product
  repo** (`kuhlman-labs/fishhawk`), NOT to the run's own issue thread, and via
  the provider, NOT a Notifier method. It is not the sticky-comment surface and
  does not touch the run thread; it collapses repeat occurrences of the same
  failure onto one upstream report instead of filing duplicates.

- The runner-kind reconciliation audit kinds — `runner_kind_resolved` and
  `runner_kind_mismatch` (#1346 / ADR-045) — are **internal, system-actor
  audit kinds, not issue-comment surfaces**. Nothing in `issuecomment` posts
  them; they have no Notifier methods. The trace upload handler
  (`trace.go::reconcileRunnerKind` → `run.(runnerKindResolver).ResolveRunnerKind`,
  then `trace.go::emitRunnerKindReconcileAudit`) reconciles the runner's
  self-observed execution channel (carried inside the SIGNED bundle manifest's
  `runner_kind` field) against the run's create-time hint. The FIRST report
  LOCKS `runner_kind` (`runs.runner_kind_resolved=true`, migration 0036): when
  the locked value differs from the prior hint the handler writes
  `runner_kind_resolved` (payload `{run_id, stage_id, from, to}`) — the #1344
  fix that corrects an omitted `runner_kind:local` defaulted to
  `github_actions`. A LATER report disagreeing with the already-locked kind
  writes `runner_kind_mismatch` (payload `{run_id, stage_id, declared,
  observed}`) and does NOT mutate the row (warn, never silently flip) — the
  post-execution guardrail. A re-affirmation (locked value == report), a
  legacy bundle with no `runner_kind`, an unrecognized report, or a resolver
  error emits NEITHER. Both are chained AFTER the `trace_uploaded` entry, which
  itself is stamped with the reconciled (locked) kind. Best-effort throughout:
  the trace is already stored, so any reconciliation failure WARN-logs and
  never unwinds the upload. Listed here only so a future reader grepping the
  audit categories doesn't mistake them for comment surfaces.

- The post-merge lifecycle audit kind — `post_merge_observed` (#1370) — is an
  **internal, system-actor audit kind, not an issue-comment surface**. Nothing
  in `issuecomment` posts it; it has no Notifier method. It is written
  best-effort by `server/pullrequest_review_events.go::resolveReviewStageOnMerge`
  (via `writePostMergeObservedAudit`) once per ACTUALLY-resolved merge —
  alongside the `pr_merged` row and the `run_merged` board move — from BOTH the
  `pull_request.closed` webhook handler and the merge-reconciler poll
  (`ResolveReviewFromPollState`), which share that resolution method. It fires
  for the review-gated and no-review (implement-only) merge paths alike, and
  NEVER for a merge held by the implement-review gate (the run stays parked) or a
  closed-without-merge resolution. Payload shape: `{pr_url, merger, head_sha,
  base_sha}` — the same fields `pr_merged` serializes. The write is best-effort
  and logged-never-rolled-back (it mirrors the `run_merged` board move's
  never-unwind contract). The `fishhawk-mcp` `next_actions` classifier consumes
  it (off the recent-audit slice `get_run_status` already fetches) to surface the
  `succeeded_merged` lifecycle state — a merged run's tail state owned and
  observable in `get_run_status` rather than implicit in whether the operator ran
  `scripts/dev post-merge`. Listed here only so a future reader grepping the
  audit categories doesn't mistake it for a comment surface.
- The campaign-driver audit kinds — `campaign_issue_started`,
  `campaign_issue_settled`, and `campaign_advanced` (E25.5 / #1444, ADR-047
  Track C) — are **system-actor audit kinds with no dedicated Notifier method
  and are NOT issue-comment surfaces**. Nothing in `issuecomment` posts them;
  they have no Notifier method. They ride the GLOBAL audit chain
  (`AppendGlobalChained`), not a per-run chain, because a campaign is not a run
  — the run linkage travels in the payload's `run_id` field. The
  campaign-driver ticker (`campaigndriver.Ticker.Tick`) is the SOLE writer,
  best-effort (a marshal or append failure WARN-logs and never unwinds the
  transition it records). `campaign_issue_started` (payload `{campaign_id,
  issue_ref, run_id}`) records starting a run for a dependency-eligible
  campaign issue; `campaign_issue_settled` (payload `{campaign_id, issue_ref,
  run_id, outcome}`) records settling a campaign item to its mapped terminal
  state once its linked run reached terminal; `campaign_advanced` (payload
  `{campaign_id, from, to}`) records the campaign state re-derivation
  (`campaign.DeriveState`) transition. **As of E26.2 / #1481 the
  campaign-driver ticker is no longer the SOLE writer of these three kinds**:
  the operator-driven campaign-linked start (`POST
  /v0/campaigns/{id}/runs` → `handleStartCampaignItemRun`) emits
  `campaign_issue_started` (and `campaign_advanced` on a pending→running
  derivation), and the reconcile-on-read path (`GET
  /v0/campaigns/{id}/status` → `reconcileCampaignItemsOnRead`) emits
  `campaign_issue_settled` (and `campaign_advanced` as items settle) —
  identical payload shapes + system actor, on the same GLOBAL chain, so the
  campaign rollup advances when the operator-agent drives the loop locally
  with no auto-driver. **As of #1558 `campaign_issue_settled` has a
  run-less variant**: the reconcile-on-read `settleIssueClosedItems` arm
  settles a run-less, deps-satisfied item whose GitHub issue is
  closed-as-completed and emits `campaign_issue_settled` with payload
  `{campaign_id, issue_ref, outcome:"succeeded", settled_via:"issue_closed",
  state_reason:"completed"}` — it carries `settled_via`/`state_reason` and
  OMITS `run_id` (there is no run), distinguishing it from the run-linked
  settle whose payload carries `run_id` + `outcome` and no `settled_via`.
  Same GLOBAL chain, same system actor, still best-effort. They remain
  best-effort and are still NOT issue-comment surfaces. Listed here only so a future reader grepping the
  audit categories doesn't mistake them for a comment surface.
- The campaign **pause** marker — `campaign_paused` (E25.7 / #1446, ADR-047
  Track C) — is also a **system-actor GLOBAL-chain audit kind, NOT an
  issue-comment surface**. The campaign-driver ticker
  (`campaigndriver.Ticker.pageGate`) is the SOLE writer: when the `GateActor`
  REFUSES a `must_page_human` gate (`out.Paged`), the driver pauses the affected
  item (`PauseCampaignItem`, recording the `PauseReason`) and — unless the
  campaign's `pause_policy` is `pause_item` (continue-others) — pauses the whole
  campaign, then records `campaign_paused` (payload `{campaign_id, issue_ref,
  run_id, page_event, policy}`). The human page itself is fired separately
  through the `Notifier` seam (see `campaign_gate_paged` below), not by this
  marker. Best-effort like the other driver kinds.
- The campaign auto-driver audit kinds — `campaign_gate_acted` and
  `campaign_gate_paged` (E25.6 / #1445, ADR-047 Track C) — are **audit-only
  kinds with no dedicated Notifier method and are NOT issue-comment surfaces**.
  Nothing in `issuecomment` posts them. They record the backend auto-acting on
  a run gate under the operator_agent contract:
  - `campaign_gate_acted` (payload `{campaign_id, issue_ref, run_id, action}`)
    is the **campaign-level marker** the campaign-driver
    (`campaigndriver.Ticker.driveGate`) writes on the **GLOBAL** chain
    (`AppendGlobalChained`, `system` actor) when the `GateActor` took a
    delegated gate action; `action` names the delegation verb
    (`approve`/`route_fixup`/`retry`/`merge`). The run-level audit of the action
    itself (`approval_submitted` / `stage_fixup_triggered` / `stage_retried` /
    `pr_merged`) is written separately on the **run** chain, stamped
    `audit.ActorAgent` under the `operator-agent/campaign` subject.
  - `campaign_gate_paged` (payload `{page_event, run_id, reason}`) is written on
    the **run** chain (`AppendChained`, `audit.ActorAgent` /
    `operator-agent/campaign`) by the gate actor
    (`server.Server.emitCampaignGatePaged`) when it REFUSES a `must_page_human`
    condition (`reviewer_reject`, `requirement_arbitration`): it takes no gate
    action and emits this hand-off. Both are best-effort (an append failure
    WARN/ERROR-logs and never unwinds the gate decision). As of E25.7 (#1446),
    `campaign_gate_paged` is the consumed *trigger* for the campaign pause/page:
    the campaign-driver (`campaigndriver.Ticker.pageGate`) pauses on the hand-off
    and calls the `Notifier` seam (`NotifyStatusUpdateForRun`), which posts a
    page-class `anchor_ping_posted` ping for this event (see the page-class ping
    list above). The `campaign_gate_paged` entry itself stays audit-only —
    `campaign_gate_acted` has no comment surface at all.

## Routing

All surfaces above only fire when the run's `TriggerSource = github_issue`.
PR-triggered and CLI-triggered runs are out of scope for this package —
they have different surfaces and a different conversation locus.

The `Notifier`'s `contextFor` / `contextForStatus` helpers gate the skip:
missing `installation_id`, unparseable `trigger_ref`, or non-issue
`trigger_source` short-circuits before any GitHub call.

### Channel routing (ADR-015 #79 option B)

Every surface above is delivered through the `Channel` interface
(`channel.go`). In v0 there is exactly ONE channel — the GitHub-comment
channel, `*Notifier` — and `Router` (`NewRouter(channels…)`) is the
notification core that fans each `Notify*` call out to its registered
channels. With a single channel the fan-out is a pass-through, so the
routing structure changed but the delivered output did not.

The **(audit category, kind) taxonomy** in the table above is the routing
key: a channel decides what to deliver and how to dedup from that taxonomy.
A future Slack adapter (v0.x) is a new `Channel` appended to the Router —
no change to the core, the call sites, or this GitHub channel. Two v0
semantics to carry forward when that adapter lands:

- `Router.NotifyBudgetAlert` returns `posted = OR` across channels, and the
  cross-run `budget_alert_sent` dedup marker (#758) keys off "any channel
  posted". For v0's single channel this is exactly the channel's own value;
  per-channel dedup (so a Slack post doesn't suppress a GitHub post) is a
  deferred v0.x design.
- The Router is nil-safe (nil receiver / nil channel entries skipped),
  matching the existing nil-safe `Notifier` posture, so call sites need no
  nil checks.

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
