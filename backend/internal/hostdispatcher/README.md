# hostdispatcher

The ADR-104 host-dispatcher identity (E88.2 / #4526). Leaf package (standard
library only): the subject grammar, the ratified scope profile, the clamp, the route
allowlist, the record-act source tag and the server heartbeat wire type (#5489). Server enforcement lives in
`backend/internal/server/middleware.go` (`bearerAuth`), `actor.go` and
`autodrive_http.go`; minting in `backend/cmd/fishhawkd/token.go`; the migration
exclusion in `backend/internal/tokenmigrate`. The dispatcher loop itself is
`fishhawk-mcp host-dispatch` (backend/internal/mcpserver; wired into scripts/dev
by E88.3 #4527).

## Identity

A host dispatcher is an API token (`api_tokens` row) whose subject is
`host-dispatcher/<host>`. `<host>` is 1–64 characters of `[A-Za-z0-9._-]` (no
slash, so one host has one subject). `IsSubject` is a PREFIX match: a malformed
`host-dispatcher/` subject is still a dispatcher, so it is classified as an
agent and refused, never treated as a human.

## Scope profile (ADR-104 Q4)

`Scopes()` = `read:runs`, `read:audit`, `write:runs`, `write:stages` (a fresh
slice per call). `ClampScopes(in)` returns `in ∩ Scopes()`.

Withheld: `write:approvals` (the dispatcher never acts at a gate),
`write:campaigns`, `write:fixups`, `write:retries`, `write:deploy`,
`read:audit-export`.

## Enforcement (server)

1. **Clamp.** `bearerAuth`'s api-token branch clamps every dispatcher-subject
   token to the profile, so a hand-minted dispatcher token holding
   `write:approvals` does not carry it to any handler.
2. **Malformed subject.** A token-store record whose subject has the prefix but
   an invalid label is refused 401 `invalid_token_subject`.
3. **Token store only.** After every credential branch has run, an identity
   carrying a dispatcher subject that did NOT come from the api-token branch (an
   AS-issued `fho_` token, a session, a run-bound `fhm_` token) resets to
   anonymous. Only the api-token branch runs the clamp. Today only the OAuth
   branch can produce such a subject (fhm_ is always `mcp:run:<id>`, and a
   session subject is `<provider>:<login>`). The guard covers every branch, so
   a future branch cannot grant the profile either.
4. **Agent classification.** `actorKindForSubject` records `actor_kind agent`,
   and `isAgentSubject` makes every human-only consumer (captain verbs,
   server-check clearing, re-run refusal) refuse a dispatcher.
5. **Record act.** POST `/v0/runs/{id}/auto-drive/acts` admits a dispatcher
   subject under `write:runs` instead of `write:approvals`. The subject must
   send `source: host_dispatcher` (`SourceTag`), or it gets 400
   `validation_failed`. Every other subject keeps the `write:approvals` rule.
6. **Route allowlist.** After the token-store guard, `bearerAuth` refuses a
   dispatcher subject every route outside `Routes()` with 403
   `host_dispatcher_route_forbidden`, before the handler and before any
   repository-configured check (see below).

## Minting

```sh
fishhawkd token issue --subject host-dispatcher/<host>
```

Without `--scopes` the token gets exactly the profile, never the operator
default. A requested scope outside the profile is refused with a usage exit
that names it. A malformed label is also a usage exit.

## Token migrate exclusion

The profile is a strict subset of the operator default, so the subset rule in
`fishhawkd token migrate` would promote a dispatcher token to the full operator
set, `write:approvals` included. `MigrateScopes` checks for a dispatcher
subject first. It counts the row as Skipped and prints
`skip  subject=host-dispatcher/<host> id=<id> (host-dispatcher profile)`, so
the dry-run inventory names it.

## Route allowlist: the profile's reach (ADR-104 Q4)

`write:runs` and `write:stages` are coarse operator scopes: many routes check
one of them and nothing more. ADR-104 Q4 accepted that reach as a residual; the
FO's fix-up ruling on this delivery closes it with ONE server-side allowlist
instead of per-handler refusals. A dispatcher subject may call only the routes
`fishhawk-mcp host-dispatch` uses (`Routes()`):

| Route | Used for |
|---|---|
| `GET /v0/runs` | candidate list (`runner_kind=local`, `pending` / `running`) |
| `GET /v0/runs/{run_id}` | drive view, forge target, parent read, `?include=campaign_link` (gate and reap) |
| `GET /v0/runs/{run_id}/stages` | stage selection, in-flight refresh, the runner reaper's stage-state probe |
| `GET /v0/runs/{run_id}/audit` | the implement review round read before an acceptance spawn (#5298) |
| `POST /v0/runs/{run_id}/auto-drive/acts` | the `run_auto_driven` record act |
| `POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch` | the host-dispatch marker |
| `POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure` | the category-C report for a failed spawn or a reaped runner |
| `POST /v0/stages/{stage_id}/acceptance-admission` | acceptance admission before an acceptance spawn |
| `POST /v0/host-dispatchers/heartbeat` | the per-tick server heartbeat (#5489; see below) |

Every other route answers 403 `host_dispatcher_route_forbidden`, including
the ones a dispatcher token's scopes would otherwise pass: run minting
(`POST /v0/runs`, `POST /v0/runs/{run_id}/recover`, so a dispatcher cannot
mint an inherited `auto_dispatch` child of a marked run), gate decisions
(scope-amendment, crew-escalation and scope-completeness decisions),
`cancel`, crew convert-to-concern, specialist admission, concurrency promote,
stage progress, and every read outside the table. `handleCreateRun`'s own
403 `host_dispatcher_cannot_start_runs` stays as defense in depth.
`RouteAllowed` matches segments as `http.ServeMux` does and fails closed on a
trailing slash, a dot or empty segment, an escaped slash or another method
(HEAD is not GET).

Pins: `TestHostDispatcherProfile_RouteReach`
(`backend/internal/server/host_dispatcher_identity_test.go`) drives every
route registered in `handlers.go` with a dispatcher token hand-minted with the
full operator set and asserts each one: an allowlisted route passes
`bearerAuth` and its scope gate, every other route is 403
`host_dispatcher_route_forbidden`. There is no unasserted bucket.
`TestHostDispatcherAllowlist_RefusesBeforeHandler` reads committed state (a
refused dispatcher `cancel` leaves the run running). On the client side,
`TestHostDispatcher_RequestsStayOnServerAllowlist`
(`backend/internal/mcpserver/host_dispatcher_test.go`) asserts every request
the core makes is on the allowlist, and
`TestHostDispatchLoop_PublishesHeartbeatEachTick`
(`backend/internal/mcpserver/host_dispatcher_loop_test.go`) asserts the same
for the loop's heartbeat publish, which the core never makes. Widening the list
changes these pins, and this table, together.

The audit row is justified by the scope grant, not by a route ruling:
`read:audit` is already in `Scopes()` (ADR-104 Q4, "read runs and their audit
chain"), and the row is the one route that implements it. It is a read and adds
no write reach.

Remaining reach (accepted): within the allowlist a stolen dispatcher token can
record dispatch acts, post the marker for any run in its account, report a
category-C stage failure, and run acceptance admission. It can also read ANY
run's audit chain in its account through `GET /v0/runs/{run_id}/audit`, every
category and `?chain=true` included (a dispatcher is not a run-bound subject,
so the run-bound withholding does not apply). That is within the `read:audit`
scope it already held. Audit payloads carry no plaintext credential by
construction: token rows record a token id or a SHA-256 of the token, and
`diff_secrets_detected` records locations, pattern classes and truncated
fingerprints, never matched bytes. Agent and operator free text (reasons,
review summaries) is credential-scanned best effort only, the same posture as
every other `read:audit` reader. It cannot approve, merge, mint, recover,
cancel or decide a gate.

## Server heartbeat (#5489, ADR-104 Amendment A2)

Each tick, the dispatcher POSTs a `Heartbeat` (`heartbeat.go`) to
`POST /v0/host-dispatchers/heartbeat` (`HeartbeatRoute`). The server
(`backend/internal/server/host_dispatcher_heartbeat.go`) upserts one row per
(token account, subject host) into `host_dispatcher_heartbeats` (migration
0121, `backend/internal/dispatcherheartbeat`), stamped with fishhawkd's clock.
The body carries no host and no account: the host comes from the
`host-dispatcher/<host>` subject and the account from the token row, and a
`host` or `account_id` key is refused as an unknown field.

The route needs `write:runs` (in the profile) and a valid dispatcher subject.
It is the dispatcher's only server write that is not run-scoped: it rewrites
the dispatcher's own heartbeat row and adds no dispatch reach. It writes no
audit row, because a heartbeat is state, not an event.

Fields: `interval_seconds`, `started_at`, `last_tick_at`,
`last_complete_tick_at` (omitted when unknown), `paused`, `tick_failure_code`,
`daily_cap`, `spawns_today`, `spawns_day` (UTC `YYYY-MM-DD`), `in_flight` and
`run_refusals`. Each refusal carries `run_id`, `stage_id`, `code`, `detail`,
`disposition`, `budget_source`, `first_at` and `last_at`.

- `disposition` is the dispatcher's reading of the refusal, a closed set:
  `waiting`, `in_flight`, `held`, `budget_blocked`, `paused`, `capped`,
  `refusing`.
- `budget_source` is `live_spec`, `admission_spec` or
  `admission_spec_fallback`, and is legal only on a `budget_blocked` entry.

Ingest rules, applied in this order:

1. **Structural errors are a 400** (`Validate`): malformed JSON, an unknown
   key, a missing or non-positive `interval_seconds`, a missing `started_at` or
   `last_tick_at`, a negative count, an unparseable `spawns_day`, more than
   `MaxRunRefusals` (256) entries, a `run_id` or non-empty `stage_id` that is
   not a lowercase 8-4-4-4-12 UUID, or a duplicate `run_id`. A body over
   256 KiB is a 400 with field `body`.
2. **One bad entry never blanks a dispatcher** (`DropInvalidRefusals`): an
   entry whose `code` is not 1–64 bytes of `[a-z0-9_:.-]`, whose
   `disposition` is outside its set, or whose `budget_source` is outside its
   set or sits on a non-`budget_blocked` entry is dropped. The rest is stored,
   and the 200 response body reports `{"dropped_refusals": <n>}`.
3. **Free text is bounded, not refused** (`BoundFields`, `BoundText`): each
   `detail` and the `tick_failure_code` lose invalid UTF-8 and control runes and
   are cut at 300 bytes on a rune boundary, the dispatcher's own
   `boundHostDispatchText` rule.

Readers judge freshness on the server-stamped `received_at`, never on
`last_tick_at`. A heartbeat is fresh for `StaleAfter(interval_seconds)`, which
is three intervals with the interval clamped to [1, 300] s, or 30 s when it is
absent or not positive. A `received_at` more than `FutureSkew` (60 s) ahead of
the reader's clock reads stale. These are the status.json reader's constants.

## Runner credential custody (narrower than ADR-104 Q4)

The dispatcher never forwards its own bearer to a runner it spawns. The runner
environment has every `FISHHAWK_API_TOKEN` entry removed. The runner needs no
bearer: the signing-key route is OIDC-or-open, prompt fetch and uploads are
Ed25519-signed, and the agent's bearer is the run-bound `fhm_` token the runner
mints itself. This fits ADR-104 Q1, where the runner credential "may be the
forwarded token", and it narrows Q4's description of the dispatcher token
reaching the runner. **Narrower than ADR-104 Q4; proposed amendment for Captain
ratification.** The ADR file is not edited by this delivery.

## A child the dispatcher never spawned (#5497)

A dispatchable decomposition child that no dispatcher spawned (the dispatcher
is down, refusing, or was never started) pages `child_dispatch_stalled` after
fishhawkd's `--stall-watch-threshold` (default 30m), unless the watch withholds
it: for example, a live fan-out holds it behind its concurrency cap, or its
parent is terminal. An `auto_dispatch` child is not exempt: a stalled
dispatcher is exactly the case. The remedy is `fishhawk_run_children` on the
parent. See `backend/internal/stallwatch/README.md`.
