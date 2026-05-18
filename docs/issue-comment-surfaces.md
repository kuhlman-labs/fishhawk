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
- The typed-reply approval path (`+1` / `lgtm` per E17.4) does NOT post its
  own slash reply (`silent=true`) — the user's typed reply *is* the
  acknowledgment. The plan-on-issue comment edit covers the broadcast.

## Routing

All surfaces above only fire when the run's `TriggerSource = github_issue`.
PR-triggered and CLI-triggered runs are out of scope for this package —
they have different surfaces and a different conversation locus.

The `Notifier`'s `contextFor` / `contextForStatus` helpers gate the skip:
missing `installation_id`, unparseable `trigger_ref`, or non-issue
`trigger_source` short-circuits before any GitHub call.

## Local-runner runs (#416)

For runs minted with `runner_kind=local`, the backend's `IssueNotifier` is a
no-op by design: the run carries no `installation_id` (the operator's local
flow doesn't go through a GitHub App webhook), so `contextForStatus` returns
early. Comment posting moves to the CLI side, where the operator's authed
`gh` is available:

| CLI verb | Renderer | Posted when |
|---|---|---|
| `fishhawk run start --issue N` | `ghcomment.RenderKickoff` | run-create succeeds |
| `fishhawk plan approve <run-id>` | `ghcomment.RenderPlanApproved` | approval submitted |
| `fishhawk plan reject <run-id>` | `ghcomment.RenderPlanRejected` | rejection submitted |
| `fishhawk run cancel <run-id>` | `ghcomment.RenderRunCancelled` | cancellation accepted |
| `fishhawk runner start --run-id …` | `ghcomment.RenderStageComplete` | runner subprocess exits cleanly |

Renderers live in `cli/internal/ghcomment`; the post step shells to
`gh issue comment <N> --repo <owner/name> --body …`. v0 scope is append-only
(each transition gets a new comment); edit-in-place against a sticky
comment-id is deferred to a follow-up. Missing or unauthed `gh` warns to
stderr and proceeds — the run still records, the issue thread just stays
quiet.

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
