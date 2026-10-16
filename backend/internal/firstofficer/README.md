# firstofficer

The ADR-089 first-officer seat identity (E85.6 / #4148). Leaf package (strings +
uuid only): the subject grammar, the fixed scope profile, and the clamp. The
server enforcement lives in `backend/internal/server/first_officer_identity.go`.

## Identity

A seat is an API token (`api_tokens` row, no new table) whose subject is:

| Subject | Anchor |
|---|---|
| `first_officer/campaign/<uuid>` | a campaign |
| `first_officer/run/<uuid>` | a run |

ADR-089 D2's `first_officer/<campaign-or-run-id>` with the anchor kind spelled
out, so the two UUID spaces never collide. The UUID is canonical lowercase
hyphenated, so `SubjectFor` and `ParseSubject` round-trip and one anchor has one
subject. `IsSeatSubject` is a PREFIX match: a malformed `first_officer/` subject
is still a seat, so it is clamped and refused, never treated as a human.

## Scope profile

`SeatScopes()` = `read:runs`, `read:audit`, `write:messages`,
`request:specialist_jobs` (a fresh slice per call). `ClampScopes(in)` returns
`in ∩ SeatScopes`. `request:specialist_jobs` equals `specialistjob.RequestScope`
(pinned by `seat_test.go`); it is NOT in the operator default scope set
(`oauthas.SupportedScopes`) and POST `/v0/specialist-jobs` honors it only from a
seat subject.

Withheld, and why: `write:approvals` (the seat never approves a gate),
`write:runs` / `write:stages` (never dispatches, merges, reaps, admits a job or
ships a result), `write:fixups` / dispositions (the captain disposes findings),
`attest:specialist-jobs` (only a runner credential attests). A seat cannot mint
itself a token: POST `/v0/tokens` refuses a seat subject 403
`first_officer_cannot_mint_tokens`. A seat cannot write tracker state: POST
`/v0/work-items` (and so `fishhawk_file_issue` over `/mcp`, which dials it under
the caller's bearer) checks no scope, so the route refuses a seat subject 403
`first_officer_tracker_write_not_permitted` before the handler runs.

## Enforcement (server)

`bearerAuth`'s api-token branch hands every authenticated seat-subject token to
`resolveFirstOfficerSeat`. Order, every arm fail-closed:

1. Clamp scopes (always first, so no later arm can leak a wider identity). A
   hand-minted seat token carrying `write:approvals` gets 403
   `insufficient_scope` on approve.
2. Unparseable subject → 401 `first_officer_seat_invalid`.
3. Unwired token / run / campaign repository → 503
   `first_officer_seat_unverifiable` (`details.reason: unconfigured`).
4. Anchor read error → 503 `first_officer_seat_unverifiable`
   (`anchor_unreadable`); nothing is revoked.
5. Terminal (`succeeded|failed|cancelled`) or missing anchor → RETIRE: every
   active token for the subject is revoked, `first_officer_seat_retired`
   `{seat_subject, reason: anchor_terminal|anchor_missing, anchor {kind, id},
   anchor_state, revoked_token_ids}` is appended (run seat on an existing run:
   the run's chain; otherwise the global chain under the anchor's account), and
   the request answers 401 `first_officer_seat_retired`. A failed list/revoke is
   logged and the request is still refused; the row names only tokens actually
   revoked.
6. Live anchor → the identity's `AccountID` is stamped from the anchor (the
   run's account; the campaign's account). Reads and writes are therefore
   account-scoped to the anchor's account: `enforceAccount`,
   `requireRunAccount` and `specialistJobCampaignVisible` accept the seat on
   that account and refuse it on any other. An untenanted anchor leaves the seat
   untenanted.

Classification: `isAgentSubject` and `actorKindForSubject` treat a seat subject
as an agent, so the captain verbs (`captainActorIsAgent`), the human-only
server-check clearing guard and the human-only resolve route refuse it, the
delegation shadow never stamps it, and seat-driven rows record `actor_kind:
agent`.

Refusal record: `writeError` appends `first_officer_action_refused` `{seat_subject,
method, path, code, required_scope?}` on the global chain (caller's account) for
every 403 served to a seat identity — a prohibited call is a permission error
plus a record, never a silent no-op. Best-effort: an append failure is logged and
never alters the response.

Specialist jobs: a seat files POST `/v0/specialist-jobs` with
`request:specialist_jobs`; the job records `requester.seat: first_officer`,
`actor_kind: agent`, and its subject must lie inside the seat
(`firstOfficerRunInSeat`: a run seat matches only its run; a campaign seat
matches a run any of its campaign items is linked to) or 403
`first_officer_subject_out_of_seat`. REST only: over `/mcp`,
`fishhawk_request_specialist_job` stays `write:stages`, because that tool
spawns the daemon-resolved runner on the daemon host with the daemon's
environment, executing repository code from a caller-chosen `working_dir` (a
caller-chosen `runner_binary` is refused over HTTP since #4476). A seat reaches no `/mcp` tool that spawns a host
program (403 `insufficient_scope` at the gate); the REST request only queues.

## Residuals

- Retirement is LAZY: checked on the seat's next request (one anchor read per
  request). Concurrent first requests after the anchor turns terminal may each
  append a `first_officer_seat_retired` row; revocation is idempotent. An eager
  sweep is remaining scope on #4148.
- A refusal that bypasses `writeError` (the `/mcp` JSON-RPC tool-scope
  precheck) is a permission error with no `first_officer_action_refused` row.
- No per-seat bound: every 403 to a seat appends one
  `first_officer_action_refused` row (no coalescing), and a seat's
  `POST /v0/specialist-jobs` is unbounded (non-draft kinds consume runner
  capacity), so a looping or compromised seat agent amplifies requests into
  chain writes and queued jobs. The ADR-089 D6 spend cap / a per-seat rate
  bound is remaining scope on #4148.
- Seat tokens are untenanted rows (`apitoken.Repository.Issue` binds no
  account); the account comes from the anchor at request time. Per-account
  token binding is remaining scope.
