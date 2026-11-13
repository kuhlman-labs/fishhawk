# hostdispatcher

The ADR-104 host-dispatcher identity (E88.2 / #4526). Leaf package (standard
library only): the subject grammar, the ratified scope profile, the clamp, the route
allowlist and the record-act source tag. Server enforcement lives in
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
| `POST /v0/runs/{run_id}/auto-drive/acts` | the `run_auto_driven` record act |
| `POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch` | the host-dispatch marker |
| `POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure` | the category-C report for a failed spawn or a reaped runner |
| `POST /v0/stages/{stage_id}/acceptance-admission` | acceptance admission before an acceptance spawn |

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
the core makes is on the allowlist. Widening the list changes all three, and
this table, together.

Remaining reach (accepted): within the allowlist a stolen dispatcher token can
record dispatch acts, post the marker for any run in its account, report a
category-C stage failure, and run acceptance admission. It cannot approve,
merge, mint, recover, cancel or decide a gate.

## Runner credential custody (narrower than ADR-104 Q4)

The dispatcher never forwards its own bearer to a runner it spawns. The runner
environment has every `FISHHAWK_API_TOKEN` entry removed. The runner needs no
bearer: the signing-key route is OIDC-or-open, prompt fetch and uploads are
Ed25519-signed, and the agent's bearer is the run-bound `fhm_` token the runner
mints itself. This fits ADR-104 Q1, where the runner credential "may be the
forwarded token", and it narrows Q4's description of the dispatcher token
reaching the runner. **Narrower than ADR-104 Q4; proposed amendment for Captain
ratification.** The ADR file is not edited by this delivery.
