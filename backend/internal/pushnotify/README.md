# pushnotify

Outbound push notifications for decisions a run is parked on (#2292). This
package is the transport seam only: it does not decide WHEN an Event exists.
The caller (the issuecomment push channel) projects page-class events from the
audit chain, claims each one, and hands it to `Dispatcher.Enqueue`. External
payload contract and operator env table: `docs/notifications.md`.

## Pieces

| Symbol | Contract |
|---|---|
| `Event` | One-screen decision payload. `source_sequence` is the originating audit entry's sequence: the dedup key and half of the delivery id. `issue` is omitted for a non-issue-anchored run; each `links` entry is omitted when its base URL is unset. `MarshalEvent` normalizes nil `verdicts` / `gate_latency.gates` to `[]` and is the exact byte sequence a signing sink signs. |
| `Sink` | `Name()` is the sink KIND (`webhook`, `slack`, `email`) and the only identifier ever surfaced (logs, `/healthz`, audit rows). `Deliver` must bound its own I/O independently of ctx and return errors only via `SanitizeTransportError`. Optional `Destination.DestinationHost()` exposes the host for error rendering. |
| `Register` / `SinksFromEnv` | Self-registering registry (the `forge.Register` shape). Each sink file calls `Register(kind, factory)` from its own `init()`, so a new sink edits no shared file. A factory returns `(nil, nil)` when unconfigured and a NAMED error when half- or mis-configured. `SinksFromEnv` returns sinks in sorted kind order; on ANY configuration error it returns every error (joined) and NO sinks, so fishhawkd fails startup rather than running a partial set. Duplicate / empty kind or nil factory panics at init. |
| `SanitizeTransportError` | The single credential scrubber. Output is always `*DeliveryError{Sink, Host, Reason}` rendering `push sink "<name>": <host>: <reason>`. The host comes from `AtHost`, a `*url.Error`'s URL (parsed; userinfo, path and query discarded), a `*net.DNSError` name, or a `*net.OpError` address. The reason is classified: `timeout`, `canceled`, `connection refused`, `connection reset`, `dns`, `tls`, `http status N`, `sink panicked`, else `delivery failed`. It retains NO cause (no `Unwrap`), so nested text — which for `net/http` embeds the full URL, i.e. a Slack credential — cannot escape via `Error()`, `%+v` or unwrapping. An existing `*DeliveryError` passes through unchanged. |
| `Dispatcher` | Asynchronous delivery; see below. |
| `WebhookSink` | Generic outbound webhook (`webhook.go`). |
| `SlackSink` | Slack as a payload MODE of the same transport (`slack.go`). |

## Dispatcher

- `NewDispatcher(sinks, Options)` builds a fixed-size queue (default 256) and
  starts the worker pool (default 2) at construction, so a worker is parked on
  the queue before the first enqueue. No sinks → no workers, every method a
  no-op. A nil `*Dispatcher` is also a no-op.
- `Enqueue(Event) bool` is the ONLY request-path call: a non-blocking send.
  It returns false (and WARN-logs) when the queue is full or the dispatcher is
  closed; the caller records the drop. A no-op dispatcher returns true.
- Per sink, a worker derives a context from `context.Background()` (never a
  request context — `Enqueue` takes none) with the per-sink timeout (default
  10s), runs `Deliver` in ITS OWN goroutine and selects on the result vs
  `ctx.Done()`. On timeout the worker records a timeout outcome and moves on;
  the abandoned goroutine is counted (`Abandoned()`, and the
  `abandoned_deliveries` WARN field) and bounded by the sink's own transport
  deadline. This is what bounds a `Deliver` that ignores its context.
- Sinks are isolated: one sink's error, timeout or panic never suppresses a
  sibling. A panic is recovered as `sink panicked` (the panic value is not
  logged).
- Every per-sink result reaches `Options.OnOutcome(ctx, Outcome)` on the
  worker, with a fresh detached ctx bounded by `OutcomeTimeout` (default 10s).
  `Outcome.Err` is nil on success and otherwise always a `*DeliveryError` —
  the dispatcher re-sanitizes whatever a sink returned (defense in depth), so
  even a sink that forgot to sanitize cannot leak raw text into a log or an
  audit row. A panicking callback is recovered.
- `Drain(ctx)` blocks until every accepted Event is fully processed — the test
  seam that replaces sleeps. `Close(ctx)` stops accepting, lets workers finish
  the queue, and waits under ONE overall deadline; it never waits on an
  abandoned `Deliver`. Idempotent.

## Webhook sink

`FISHHAWKD_NOTIFY_WEBHOOK_URL` + `FISHHAWKD_NOTIFY_WEBHOOK_SECRET`. Both unset
→ no sink. Either one without the other → named startup error. There is no
unsigned mode. POSTs `MarshalEvent` output with `Content-Type:
application/json`, `User-Agent: fishhawk/<version>`, `X-Fishhawk-Event: <kind>`,
`X-Fishhawk-Delivery: <run_id>:<source_sequence>`, and
`X-Fishhawk-Signature-256: sha256=<lowercase hex HMAC-SHA256(secret, body)>` —
the construction `internal/webhook` verifies inbound.
Not to be confused with the runner's upload header `X-Fishhawk-Signature`
(Ed25519 over the trace bundle, `runner/internal/upload`): the `-256` suffix
and the HMAC scheme are specific to push deliveries.

## Slack sink

`FISHHAWKD_NOTIFY_SLACK_WEBHOOK_URL` (an incoming-webhook URL; the URL IS the
credential, so no HMAC header is sent). `RenderSlackMessage` is pure: a `text`
fallback (`<decision> on <repo>#<issue> - waiting 8m - <link>`) plus blocks for
the decision, stage/workflow/event, reviewer verdicts, accumulated wait and
links. Payload text is mrkdwn-escaped (`&`, `<`, `>`) so it cannot forge a link
or mention. Pinned by `TestSlackMessageGolden`.

## Shared HTTP transport rules (both sinks)

- URL validation: absolute `https`, or plain `http` only to a loopback host.
- Every configuration error names the ENV VAR and the problem, never the value
  (e.g. `FISHHAWKD_NOTIFY_SLACK_WEBHOOK_URL: must be https (plain http is
  accepted only for a loopback host)`). No URL is ever echoed, at config time
  or at delivery time.
- `http.Client` always has a `Timeout` (default 10s) and never follows
  redirects: a 3xx is reported as `http status 3xx`, so a signed body cannot
  be carried to an unconfigured host.
- A non-2xx response is reported by status code and host only; the response
  body is drained (capped) and never read into an error.

## Residuals

- Delivery is at-most-once per enqueue; there is no retry. A process crash
  after enqueue drops the Event (the caller's claim row already landed).
- An abandoned `Deliver` goroutine keeps running until the sink's own
  transport deadline; a sink with no transport deadline would leak it. Both
  shipped HTTP sinks bound it via `http.Client.Timeout`.
