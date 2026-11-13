# hostdispatcher

The ADR-104 host-dispatcher identity (E88.2 / #4526). Leaf package (strings
only): the subject grammar, the ratified scope profile, the clamp and the
record-act source tag. Server enforcement lives in
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
   token to the profile. A hand-minted dispatcher token holding
   `write:approvals` gets 403 `insufficient_scope` on approve.
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

## Accepted residual: the profile's reach (ADR-104 Q4)

`write:runs` and `write:stages` are coarse operator scopes. Many routes check
one of them and nothing more, so a dispatcher token passes their scope gate.
The dispatcher's own code calls none of them except the record act and the
host-dispatch marker. ADR-104 Q4 accepts this residual. There is no
per-route dispatcher scope.

`TestHostDispatcherProfile_RouteReach`
(`backend/internal/server/host_dispatcher_identity_test.go`) sends a request
to every mutating route registered in `handlers.go` twice: once with a
read-only control token, once with a dispatcher token hand-minted with the
full operator set. It pins which routes let the dispatcher past the scope gate
(`hdAdmittedRoutes`) and which refuse it (`hdRefusedRoutes`, for example
approvals, merge, `auto-drive` gate acts, campaigns, captain verbs, concern
resolve and deploy rollback). A route that changes class turns the test RED.
When that happens, update the pinned list and this section together.

Some admitted routes are reachable only past the scope gate. Their handlers may
still refuse later, on run state, a subject check or validation. The table pins
scope reach, not end-to-end success. `POST /v0/runs` is classified there but not
pinned, because a sibling slice adds a 403 `host_dispatcher_cannot_start_runs`
refusal with its own test.

The admitted set at this delivery has 34 routes:
concern defer/waive, release cut/notes/publish, run `auto-drive/acts`,
`cancel`, `concerns/waive`, `consolidate`, divergence answer, `escapes`,
`integrate-wave`, merge-queue promote/withdraw, `product-reports`,
`rebase-branch`, `reconcile-merge`, `record-merge-observation`, `recover`,
`redrive`, `reset-branch`, `retrigger-ci`, reviews reconcile/rerun, `revive`,
scope-completeness decision, stage `host-dispatch` and `reap-failure`,
`status-comment`, `vouch-commit`, stage `acceptance-admission`, `fixup` and
`retry`, and `POST /v0/work-items`. The test's `hdAdmittedRoutes` is the
authoritative list.

## Runner credential custody (narrower than ADR-104 Q4)

The dispatcher never forwards its own bearer to a runner it spawns. The runner
environment has every `FISHHAWK_API_TOKEN` entry removed. The runner needs no
bearer: the signing-key route is OIDC-or-open, prompt fetch and uploads are
Ed25519-signed, and the agent's bearer is the run-bound `fhm_` token the runner
mints itself. This fits ADR-104 Q1, where the runner credential "may be the
forwarded token", and it narrows Q4's description of the dispatcher token
reaching the runner. **Narrower than ADR-104 Q4; proposed amendment for Captain
ratification.** The ADR file is not edited by this delivery.
