# Push notifications

Outbound push when a run parks at a decision (#2292). Optional, operator-configured, advisory. No `FISHHAWKD_NOTIFY_*` variable set → no sink, no dispatcher, no push channel; behaviour is identical to a build without the feature.

Code: `backend/internal/pushnotify` (payload, sinks, dispatcher — long-form contract in its `README.md`), `backend/internal/issuecomment/push.go` (trigger, claim, outcome rows), wiring in `backend/cmd/fishhawkd/serve.go` + `backend/internal/server/server.go`.

## Trigger

- The trigger is the SAME audit-chain page-class projection as the issue-comment pings (`issuecomment.pageClassEvents`): `plan_awaiting_approval`, `plan_review_rejected`, `implement_review_rejected`, `scope_amendment`, `clarification_request`, `campaign_gate_paged`, `acceptance_triage`, `ci_failure`, `ci_retry_exhausted`.
- Fires on the Router's `NotifyStatusUpdateForRun` and `NotifyPageClassForRun` (the #1786 immediate hook), for EVERY run — CLI- and PR-triggered runs too, not only issue-anchored ones. The `issue` block is omitted for a non-issue-anchored run.
- A reviewer reject the operator has already arbitrated is claimed but not sent (same `pageEventResolved` rule as pings).
- Gate latency is context, not the trigger: folded from `issuecomment.BuildRunEconomics` (`latency.AggregateGateLatency`). No latency-threshold escalation in v0 (#2293).
- Bounded volume: one push per (run, source_sequence) per sink set. Budget alerts do not push.

## Payload (`schema_version` 1)

Webhook body, and the data every sink renders. Canonical example (pinned against `pushnotify.Event` by `TestDocumentedPayloadMatchesEventStruct`):

<!-- BEGIN canonical-payload -->
```json
{
  "schema_version": 1,
  "event": "plan_awaiting_approval",
  "source_sequence": 1187,
  "run_id": "c1c9cd3e-b44c-4dfe-9062-8530aaeff8df",
  "run_short_id": "c1c9cd3e",
  "repo": "acme/widgets",
  "issue": {"number": 42, "url": "https://github.com/acme/widgets/issues/42"},
  "workflow_id": "feature_change",
  "stage": {"type": "plan", "state": "awaiting_approval"},
  "decision": "A plan is ready and awaiting your review",
  "verdicts": [{"reviewer_model": "model-a", "verdict": "approve"}],
  "gate_latency": {"total_wait_on_human_seconds": 480, "gates": [{"gate": "plan_approval", "wait_seconds": 480}]},
  "links": {"run": "https://fishhawk.example.com/runs/c1c9cd3e-b44c-4dfe-9062-8530aaeff8df", "issue": "https://github.com/acme/widgets/issues/42", "pull_request": "https://github.com/acme/widgets/pull/43"},
  "occurred_at": "2026-09-28T12:10:00Z"
}
```
<!-- END canonical-payload -->

| Field | Notes |
|---|---|
| `schema_version` | `1`. Additive fields keep the version; a removed or re-typed field bumps it. Consumers must ignore unknown fields. |
| `event` | Page-class kind token (list above). |
| `source_sequence` | Originating audit entry's sequence. Dedup key; with `run_id` forms the delivery id. |
| `run_id` / `run_short_id` | Full UUID / first 8 chars. |
| `repo` | `owner/name`. |
| `issue` | Omitted for a non-issue-anchored run. `url` set for GitHub-family runs only. |
| `workflow_id` | Omitted when empty. |
| `stage` | The stage the source audit entry names; omitted when it names none. |
| `decision` | One-line human-readable ask. |
| `verdicts` | Reviewer verdicts of the event's review round; `[]` for non-review events. |
| `gate_latency` | Whole seconds. `gates` is `[]` when no gate interval exists. |
| `links` | `run` omitted when `FISHHAWKD_EXTERNAL_URL` is unset (#1787); `issue` / `pull_request` omitted when absent. |
| `occurred_at` | RFC 3339 UTC timestamp of the source audit entry. |

## Webhook wire contract

`POST` with headers:

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `User-Agent` | `fishhawk/<version>` |
| `X-Fishhawk-Event` | `event` |
| `X-Fishhawk-Delivery` | `<run_id>:<source_sequence>` — stable across retries of the same event; use it to dedup on the receiver. |
| `X-Fishhawk-Signature-256` | `sha256=<lowercase hex HMAC-SHA256(secret, exact request body)>` |

Verify: recompute HMAC-SHA256 over the raw body bytes with the shared secret, hex-encode, prefix `sha256=`, compare in constant time — the same construction `backend/internal/webhook/webhook.go` verifies for inbound GitHub deliveries. Any 2xx is success; 3xx is not followed (reported as failure).

Slack receives a rendered incoming-webhook message instead (text fallback `<decision> on <repo>#<issue> - waiting 8m - <link>` plus blocks), unsigned — the incoming-webhook URL is the credential. Email is plain text (`pushnotify.RenderEmail`).

## Operator configuration

| Variable | Sink | Notes |
|---|---|---|
| `FISHHAWKD_NOTIFY_WEBHOOK_URL` | webhook | `https`, or `http` to a loopback host only. |
| `FISHHAWKD_NOTIFY_WEBHOOK_SECRET` | webhook | REQUIRED with the URL. There is no unsigned mode. |
| `FISHHAWKD_NOTIFY_SLACK_WEBHOOK_URL` | slack | Incoming-webhook URL (a bearer credential). Same scheme rule. |
| `FISHHAWKD_NOTIFY_EMAIL_SMTP_ADDR` | email | `host:port`. |
| `FISHHAWKD_NOTIFY_EMAIL_FROM` | email | Required when any email var is set. |
| `FISHHAWKD_NOTIFY_EMAIL_TO` | email | Comma-separated; required when any email var is set. |
| `FISHHAWKD_NOTIFY_EMAIL_USERNAME` / `_PASSWORD` | email | Optional; both or neither. AUTH is refused over an unencrypted connection. |
| `FISHHAWKD_NOTIFY_EMAIL_STARTTLS` | email | Default on. |

- A half- or mis-configured sink (e.g. a webhook URL without a secret) FAILS fishhawkd startup. Every configuration error names the ENV VAR and the problem, never the value.
- Startup logs one INFO naming the configured sink KINDS only. `/healthz` `push_sinks` lists the kinds (`[]` when none) — never a URL, address, recipient or secret.

## Delivery model (asynchronous)

- Request path: under a per-run in-process lock, read the chain → drop claimed sequences → append the `push_notification_sent` CLAIM row → `Enqueue` (non-blocking). No network I/O on the request path; the notifier always returns nil.
- A failed claim append sends nothing (fail toward silence, WARN).
- Dispatcher: fixed queue (256), worker pool (2) started at boot. Each sink `Deliver` runs in its own goroutine under a detached context with a 10s per-sink timeout; a sink ignoring its context is abandoned at the timeout (counted, bounded by the sink's own transport deadline). Sinks are isolated.
- Queue full → the event is dropped with a WARN and a `push_notification_failed` row (`sink: "*"`, `error: "queue_full"`).
- Shutdown drains the queue under one overall deadline and never waits on an abandoned delivery.
- No retry. At-most-once.

## Audit categories

| Category | Written | Payload |
|---|---|---|
| `push_notification_sent` | Request path, BEFORE enqueue (the claim) | `source_sequence`, `event`, `sinks` (target kinds); `suppressed: "resolved"` for a stale reject claimed without sending |
| `push_notification_failed` | Worker, per failed sink; or request path on queue-full | `source_sequence`, `event`, `sink`, `error` (sanitized: sink, host, classified reason), `timed_out` |

Neither is anchor-timeline activity. Delivery errors never carry a URL path, query, userinfo, secret or SMTP password — only `push sink "<kind>": <host>: <reason>` (`timeout`, `connection refused`, `dns`, `tls`, `http status N`, …).

## Residuals

- Dedup is the in-process claim lock plus the durable claim row. Two fishhawkd replicas on one database could each claim the same sequence (follow-up: a unique constraint on `(run_id, source_sequence)`).
- A crash between the claim append and delivery drops that event; the claim is committed, so a restart does not retry it. Deliberate: a notification storm is worse than a missed push.
- No latency SLO is asserted; delivery is enqueued at the page-class append itself with workers already parked on the queue.
