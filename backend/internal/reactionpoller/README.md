# backend/internal/reactionpoller

Reaction-polling worker (#360 / E17.3b): approval-shaped emoji reactions on plan comments.

## Ticker

`Ticker` polls Fishhawk-authored plan comments for approval-shaped reactions (👍 / ❤️ / 🎉 / 🚀) GitHub doesn't deliver via webhooks.

Polling because the GitHub webhook surface has no `reaction` event on issue comments — typed-reply approvals (E17.3 / #338) cover the `+1` / `lgtm` case at zero rate-limit cost; this worker is the catch-net for click-only thumbs-up.

- **Adaptive cadence**: fast tier (~30s) for plan comments younger than 10 min, slow tier (~5 min) once older.
- **Dedup** via the `plan_reaction_observed` audit category — every observed reaction id writes a row; existing rows mean the worker already saw the reaction and won't re-forward it.
- **Forwarding**: approval-shaped reactions land in the same `webhook.ApprovalCommandHandler` the reply-comment path uses (`Source = ApprovalSourceReactionEmoji`); the handler is "silent skip when no awaiting plan" by design, so a reaction on an unrelated comment is harmless.
- GitHub method: `githubclient.Client.ListIssueCommentReactions`; the App's `issues: write` permission already covers it.

## Configuration

Off by default; enable with `--enable-reaction-poller` plus `--reaction-poller-fast-interval` / `--reaction-poller-slow-interval` / `--reaction-poller-age-threshold` knobs (defaults: 30s / 5min / 10min).
