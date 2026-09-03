# Local-dev webhook relay (E64.49 / #3169)

An **opt-in** smee.io relay: a smee client that receives GitHub App deliveries
from a stable per-developer channel and POSTs them verbatim to
`POST /webhooks/github` on the local `fishhawkd`, so the webhook-driven
behaviours in `backend/internal/server/webhook.go` (`handleWebhook`) work on the
local stack instead of accreting one poller each. It is **off by default** and
touches no Go code.

## Why a relay, not more pollers

`fishhawkd` on `http://127.0.0.1:8080` is reachable from your workstation, but
GitHub cannot reach *in* to deliver a webhook. Putting a reverse proxy in front
of localhost does not help: a proxy in front of localhost is still *on*
localhost — the gap is **inbound reachability**, not TLS or routing inside the
box. smee.io bridges that gap: the App delivers to a public smee channel, and a
local smee client long-polls that channel and replays each delivery to the local
endpoint.

The webhook-driven behaviours this reaches include the run-less
`fishhawk_audit_complete` publish (#3160) and the five `handleWebhook`
consumers. Without the relay, each of those is exercised locally only by a
poller (e.g. the merge reconciler) or not at all.

## Enable it

1. Create a channel: visit <https://smee.io/new> and copy the
   `https://smee.io/<token>` URL.
2. Set both vars in `.env`:

   ```sh
   FISHHAWK_DEV_WEBHOOK_RELAY=1                       # opt in (only the literal 1 enables)
   FISHHAWK_DEV_WEBHOOK_CHANNEL=https://smee.io/<token>
   # FISHHAWK_DEV_SMEE_BIN=/path/to/smee-client       # explicit client binary
   ```

3. Install the client:

   ```sh
   npm install -g smee-client
   ```

4. Point the App's **Webhook URL** at the same smee channel, then
   `scripts/dev up`.

`scripts/dev` composes the client invocation with the **short** flags the client
supports (`docs/github-app/README.md` ships this exact form):

```sh
smee-client -u https://smee.io/<token> -t http://127.0.0.1:8080/webhooks/github
```

The target port is derived from `_healthz_port` (the same accessor `up`'s
readiness gate uses), so it tracks `FISHHAWKD_ADDR` if you override the listen
address.

`reload`/`post-merge`/`down` inherit the leg; the flag and channel are read from
`.env`, so the switch survives the daily loop. `down` tears the relay down
**unconditionally** (guarded on the pid file, never on the flag), so disabling
the flag between `up` and `down` never orphans the client.

### The channel-URL check is a typo guard, not a reachability check

`scripts/dev` validates that `FISHHAWK_DEV_WEBHOOK_CHANNEL` is a syntactically
plausible `https://smee.io/<token>` URL (non-empty, `https` scheme, a non-empty
path segment). It does **not** probe the channel — a well-formed URL for a
channel that does not exist passes the check and fails later at the client,
surfacing per the troubleshooting section below.

## Degrade-not-abort

Departing from the TLS front end (`docs/local-tls.md`), which `up` gates its own
readiness on, **a relay precondition failure never fails `scripts/dev up`**. A
missing relay invalidates nothing `up` claims to have started, so every failure
path prints one actionable line, leaves no pid file, and `up` continues.

| Reason | Fix |
|---|---|
| `FISHHAWK_DEV_WEBHOOK_CHANNEL is unset` | Create a channel at <https://smee.io/new> and set it in `.env`. |
| `FISHHAWK_DEV_WEBHOOK_CHANNEL='…' is malformed` | Use an `https://smee.io/<token>` URL. |
| `no smee client found` | `npm install -g smee-client`, or set `FISHHAWK_DEV_SMEE_BIN` to its path. |
| `webhook relay client died immediately` | Read `logs/webhook-relay.log` — a rejected channel or an unsupported client flag. |
| `webhook relay already running (pid N)` | A relay is already up; not an error. Run `scripts/dev down` first to restart it. |

## Troubleshooting

- **A silently-dead relay presents as a stale pid file plus errors in
  `logs/webhook-relay.log`.** The startup liveness poll only catches a client
  that dies *immediately*. One that survives startup and dies later — smee
  rejecting the channel after the first long-poll, a dropped network — leaves a
  live-looking pid file and is not supervised until the next `down`. This
  posture is deliberate (see the TLS precedent): there is no relay supervisor.
  If deliveries stop arriving, check `logs/webhook-relay.log` for the client
  error and re-`up`.

- **Reverting this feature while an enabled relay is alive can strand the
  client.** The revert removes `_relay_down`, so `scripts/dev down` from the
  reverted revision has nothing to reap the running relay with. **Stop the relay
  BEFORE reverting** (`scripts/dev down` on this revision). If you have already
  reverted with a relay still alive, reap it by hand: `kill $(cat
  .fishhawk/webhook-relay.pid)`, then remove the pid file (`rm -f
  .fishhawk/webhook-relay.pid`).

## Settled decisions

Three questions the issue raised, answered as design decisions:

1. **Per-developer channel, no committed default.** The channel URL is strictly
   per-developer local `.env` config. This repo targets a solo/per-developer
   App, and **one App has one webhook URL**: pointing the App at your dev
   channel redirects its deliveries away from any other consumer, and two
   developers relaying at once fight over the single channel. The team-scale
   answer is a **per-developer App** — documented here, not engineered for.
2. **Coexists with the merge reconciler.** The relay does not replace the
   existing pollers. `FISHHAWKD_ENABLE_MERGE_RECONCILER` stays available and its
   heal is idempotent, and the audit-complete publisher dedups, so relay +
   reconciler are **belt-and-braces**, not an undocumented race.
3. **Channel in `.env`, not minted per `up`.** A stable channel is the whole
   reason smee was chosen over a quick tunnel — the App's webhook URL is set
   once and the channel outlives any single `up`.

## Alternative — cloudflared (manual)

If you are unwilling to route webhook payloads through a third party, use a
tunnel that gives you a real HTTPS hostname for its lifetime instead:

```sh
brew install cloudflared
cloudflared tunnel --url http://localhost:8080
```

Use the printed `https://*.trycloudflare.com` URL for the App's webhook URL.
This is **not** orchestrated by `scripts/dev` — it is a manual recipe; see
`docs/github-app/README.md` Mode C.

## Security posture

The channel URL is a bearer capability: anyone holding it can POST to the
channel, and the client replays whatever arrives to the local endpoint. What the
webhook handler's controls do and do **not** stop:

- **HMAC signature (`X-Hub-Signature-256`) stops FORGERY and MODIFICATION.**
  `backend/internal/server/webhook.go` calls
  `webhook.VerifySignature(secret, body, sig)` over the **body only**, so a
  third party cannot forge a new event or alter a captured one without the
  webhook secret.
- **Delivery-id dedup stops honest retries and naive identical replay.** The
  handler marks `X-GitHub-Delivery` via `WebhookDeliveries.Mark(deliveryID)` and
  answers `202` without reprocessing on `webhook.ErrDeliveryDuplicate`.
- **NEITHER stops a channel-URL holder replaying a captured body+signature under
  a FRESH delivery id.** The signature does not cover the delivery-id header, so
  changing that header defeats the dedup while the signature still verifies.

So the real question is what **re-processing a legitimate event does**. It is
not uniformly idempotent — per handler:

| Event (handler) | Effect of a replay |
|---|---|
| `pull_request` opened/reopened/synchronize (`republishOnPullRequestEvent`) | **Idempotent** — recomputes the audit-complete result from the run's chain and republishes; the publisher's per-`(forge, repo, head_sha)` dedup suppresses a repeat at the same head. Caveat: a replay of an OLD body republishes at that old `head_sha`, which branch protection (evaluating at the current head) does not act on. |
| `code_scanning_alert` (`recordSecurityScan`) | **Idempotent** — writes one idempotent `securityscan` entry. |
| `check_run` (`ingestCheckRun` → `StageCheckRepo.Append`) | **Idempotent in effect** — consumers read the latest per `(stage_id, check_name)` — but the rows are append-only, so a replay adds duplicate history. |
| `pull_request.closed` (`handlePullRequestClosed` → `resolveReviewStageOnMerge`) | Stage transition is idempotent (the state machine rejects a repeat), but `writePRMergedAudit` is an **unguarded** `AppendChained` — a replay writes a **duplicate `pr_merged` audit row**. |
| `pull_request_review.submitted` (`handlePullRequestReviewSubmitted`) | **Not idempotent** — an unconditional `AppendChained` with no dedup writes a **duplicate review audit row**. The clearest case. |

And one consequence the per-handler table omits: `WebhookDispatcher.Handle` runs
on **every** delivery ahead of these consumers and is the run-**triggering** and
approval-**command** path. A replayed trigger or approval event is the **sharpest
replay consequence on this route** — sharper than a duplicated audit row.

This is a local dev convenience. The exposure is acceptable when stated
accurately: the worst realistic outcome is duplicated audit history and a
spuriously re-triggered local run. Anyone unwilling to accept that should use the
cloudflared path above.

## Implementation

`scripts/dev` (helpers `_relay_*`, wired into `cmd_up`/`cmd_down`), pinned
against stubs by `scripts/test-dev`. Long-form contract: `scripts/README.md`.
