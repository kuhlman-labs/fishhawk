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
  `implement_review_skipped` (degraded gate), and `plan_review_started` /
  `implement_review_started` (the #600 dispatch proxy emitted only when
  `reviewers.agent>0` AND a `PlanReviewer` is wired) — are **internal audit
  kinds, not issue-comment surfaces**. Nothing in `issuecomment` posts them
  to the issue thread. They are written by the plan/trace upload handlers
  and read back by the MCP surface (`fishhawk_get_run_status` /
  `fishhawk_get_plan` `review_status`, and `fishhawk_await_review`) to
  derive the none/pending/complete/skipped lifecycle. Listed here only so a
  future reader grepping for `*_reviewed` doesn't mistake them for a comment
  surface.
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
