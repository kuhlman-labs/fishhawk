# specialistjob

ADR-097 D2/D6 specialist jobs (E85.9 / #4300): bounded, typed evidence work a
seat requests and a runner executes in a fresh scratch clone (the `host.*`
inventory ids, which read no tree, excepted), recorded on the audit chain. Contract: `docs/spec/specialist-job-v1.md` (canonical schema
`docs/spec/specialist-job-v1.schema.json`, mirrored into `schemas/` by
`scripts/sync-schemas`).

## Files

| File | Owns |
|---|---|
| `specialistjob.go` | Kinds, verdicts, seats; the closed `InventoryCommands` set; `ParseRequest` / `ParseCompletion` (duplicate-member walk → schema → strict decode → Go semantic rules); `ValidateResult`; `DecodeEvidence`; the deterministic `RenderDraft`. |
| `store.go` | The pgx store over `specialist_jobs` (migrations 0103, 0104): `Create`, `CreateCompleted`, `Get`, `AdmitJob` (mints / refreshes / clears the attestation key), `Complete` (verifies a signed result, records provenance, consumes the key), `ListCompletedForRun`. |
| `attestation.go` | Result attestation (E85.15 / #4363): `AttestScope`, the provenance values, `AttestationTTL`, `ResultMessage`, `BlockingEligible`, `VerifyResultAttestation` and its refusal reasons. |
| `backend/internal/server/specialist_job.go` | The four `/v0/specialist-jobs` handlers, `attesterFor`, `requireSpecialistJobScope`. |
| `backend/internal/server/gateview.go` | The gate view's display-only `specialist_jobs` block. |
| `runner/internal/upload/upload.go` | The runner client (`FetchSpecialistJob`, `AdmitSpecialistJob`, `ShipSpecialistJobCompletion`). |

## HTTP surface

Not run-scoped routes: a job's subject may be a run, a campaign or only a head
SHA, so each handler scopes by the caller's account through the store
(`account_id IS NOT DISTINCT FROM`), and a foreign job reads as
`specialist_job_not_found`. A nil `Config.SpecialistJobs` answers every route
`503 specialist_jobs_unconfigured` (the partial rollback). OpenAPI:
`docs/api/v0.openapi.yaml`; companion: `docs/api/v0.md` § "Specialist jobs".

| Route | Scope | Notes |
|---|---|---|
| `POST /v0/specialist-jobs` | `write:stages`, or `request:specialist_jobs` (`RequestScope`) from a first-officer seat ONLY | 64 KiB cap (`413 request_too_large`). Run-bound token → `403 specialist_job_requester_not_a_seat`. A non-seat token holding only `request:specialist_jobs` → `403 insufficient_scope`. A seat's subject must lie inside its seat (`subject.campaign_id` = a campaign seat's own campaign, `subject.run_id` a run inside the seat; a subject naming neither) or `403 first_officer_subject_out_of_seat`, before any store call; a seat-membership read failure → `503`. Subject run / campaign outside the caller's account → `404 run_not_found` / `campaign_not_found`. `draft` → `201 completed`; else `201 queued`. A runner credential (`attest:specialist-jobs` only) can never request a job. |
| `GET /v0/specialist-jobs/{job_id}` | `read:runs` or `attest:specialist-jobs` | Never renders `execution_nonce`. Advisory `stale`. `provenance` (completed jobs only) and `blocking_eligible`. |
| `POST .../{job_id}/admission` | `write:stages` or `attest:specialist-jobs` | `{host, nonce}`; admit / heartbeat / stale takeover. `409 specialist_job_not_admissible` / `specialist_job_already_running`. An attester's admission that moves the job to running carries `attestation_key` + `attestation_expires_at`, once. |
| `POST .../{job_id}/result` | `write:stages` or `attest:specialist-jobs` | 256 KiB cap. Parsed against the STORED kind (`400`, `details.reason: kind_mismatch`). `409 specialist_job_not_running` / `specialist_job_nonce_mismatch`. Optional `X-Fishhawk-Signature`; a refused signature is `401 specialist_job_attestation_invalid` with `details.reason`. |

The three runner routes use `requireSpecialistJobScope`, which is
`requireWriteScope` with the attest scope as an accepted alternative and
nothing else changed: anonymous is `401 authentication_required`, a cookie
session (no `TokenID`) bypasses the scope check exactly as before, and a token
holding neither scope is `403 insufficient_scope`.

**Requester.** Server-derived from the authenticated identity, never read from
the body (the closed request def refuses a `requester` member):
`seat: first_officer` for a first-officer seat identity (a `first_officer/`
token subject, E85.6 / #4148; contract: `backend/internal/firstofficer/README.md`),
`seat: operator` for every other identity; `actor_kind` from the token subject
(`actorKindForSubject`: an operator-agent token or a first-officer seat is
`agent`, every other identity `user`), `subject` = the identity subject. A seat
is charged to its anchor's account (the seat identity hook stamps it). A seat
holds no `write:stages` and no attest scope, so admission and result refuse it
`403 insufficient_scope`.

**Draft** is a model-free template render at request time (`RenderDraft`); it
writes only its `specialist_job_requested` + `specialist_job_completed` rows
and never files a work item. A model-written draft would be a later kind
amendment.

## Admission

A runner-executed job is admitted through the `local-verify:<host>` concurrency
group (`concurrency.VerifyGroupKey`), never an implement slot, contending on the
SAME advisory lock key as verify stages. It is admitted only when stage holders
plus live job holders are below `concurrency.DefaultVerifyLimit`, no fresh
verify-stage waiter exists (a job yields to merge-candidate passes, which may
starve jobs under a stream of verifies — deliberate), and no older fresh queued
job of the same group and account is ahead. A running job holds only while its
runner re-POSTs `/admission` with its nonce (every 15s); past
`concurrency.JobHeartbeatTTL` (2m) it stops counting as a holder and a
different nonce TAKES IT OVER, appending `specialist_job_taken_over`.

## Audit payloads

| Category | Chain | Payload |
|---|---|---|
| `specialist_job_requested` | subject run's chain, else the account's global chain | `{job_id, kind, requester, subject, inputs}` — `inputs` is the request's raw member (semantic JSON; JSONB normalizes key order and whitespace). Actor = the requester. |
| `specialist_job_taken_over` | same | `{job_id, kind, admission_group, host, previous_host, previous_last_seen_at}`. |
| `specialist_job_completed` | same | `{job_id, kind, requester (from the job ROW), subject, verdict, reason, evidence, charged_to: {seat, subject}, duration_ms, requested_sequence, provenance: "bearer" \| "runner_attested", blocking_eligible, attested_by (omitted unless runner_attested)}`. |

Actor attribution: an attester's admission, takeover and completion record
`actor_kind: system` with the runner token's subject; every other caller keeps
`actorKindForSubject` (`agent` for an operator-agent token, else `user`).

## Result provenance and attestation (E85.15 / #4363)

Canonical contract: `docs/spec/specialist-job-v1.md` § "Result provenance and
attestation". Summary of the server half:

- **Attester.** `attesterFor(identity)` is the token subject when the caller
  is an explicit API token (`TokenID` set) holding `attest:specialist-jobs`
  whose subject is not `mcp:run:`-prefixed, and `""` otherwise. A cookie
  session is never an attester (it carries no scope list), a run-bound token
  never is, and a plain `write:stages` bearer never is. The scope is absent
  from the operator default set (the HTTP mint ceiling refuses it) and from
  the OAuth vocabulary, so it is minted only by `fishhawkd token issue
  --scopes`.
- **Key.** An attester's admission that moves the job to running mints a
  job-scoped Ed25519 key. The store keeps only the public half, bound to the
  job id, the admitting nonce and the minting subject, with a sliding
  `AttestationTTL` (5 min) expiry decided on the database clock. The private
  half is delivered ONCE as `attestation_key` (base64 std) and is never
  re-delivered on a heartbeat. Only the minting attester's same-nonce
  heartbeats slide the expiry. A non-attester admission (including a stale
  takeover) clears the columns, so a takeover always invalidates the previous
  runner's key.
- **Signed result.** The runner signs `ResultMessage(job_id, body)` over the
  EXACT request body and sends the hex signature as `X-Fishhawk-Signature`.
  The handler refuses, before any store call, a repeated / empty / non-hex
  header (`signature_malformed`) and a signature from a caller that is not an
  attester (`caller_not_attester`). The store then verifies on the locked row,
  after the state and nonce checks: `no_attestation_key`,
  `attestation_expired`, `subject_mismatch`, `signature_malformed`,
  `signature_invalid`. Every refusal is `401
  specialist_job_attestation_invalid` with `details.reason`, and nothing is
  written. A verified result records `runner_attested` and consumes the key
  (single use; a replay is `409 specialist_job_not_running`).
- **Unsigned result.** No header → `bearer`, accepted from any caller the
  route scope admits, exactly as before #4363. A pre-0104 completion and a
  `draft` read as `bearer`.
- **Blocking eligibility.** `BlockingEligible(provenance)` is the ONE
  predicate: true only for `runner_attested`. It is rendered as
  `blocking_eligible` on the read, the audit payload and the gate view.
- **Gate view.** `GET /v0/runs/{run_id}/gate-view` carries a display-only
  `specialist_jobs` block (`job_id, kind, verdict, provenance,
  blocking_eligible, completed_sequence`; enum and id members only, so no
  job-authored text reaches a run-bound caller) from `ListCompletedForRun`
  under the run row's account. It is attached by `handleGetRunGateView`, never
  `buildGateView` (the attention scan does not pay the read). Omitted when the
  store is unwired or the run has no completed job; a read failure or an
  unparseable run account omits it with a `specialist_jobs` `history_gaps`
  entry. It feeds no gate, merge or advance decision.

## Rollout

Attestation is opt-in and safe to ship without operator action: without a
runner token every result keeps recording `bearer`, as before.

1. Issue one runner token per host subject (CLI only; the HTTP mint ceiling
   refuses the scope):
   `fishhawkd token issue --subject specialist-runner/<host> --scopes attest:specialist-jobs`.
2. Export it as `FISHHAWK_RUNNER_ATTEST_TOKEN` in the environment of the
   process that spawns the runner: `fishhawk-mcp` (stdio MCP), or `fishhawkd`
   for HTTP MCP. The runner prefers it over `FISHHAWK_API_TOKEN` for the
   job's fetch, admission, heartbeat and result.
3. Revoke or unset it to fall back to `bearer`; a missing or revoked runner
   token degrades results to `bearer`, never to a failure.

**Attestation binds to the minting token's SUBJECT, not the token id.** The
key is stored against `attestation_subject`, and the result handler and the
store compare the posting identity's subject to it. Two tokens issued for the
same subject are therefore interchangeable attesters: a result signed with
the job's key verifies when posted under either, and either's same-nonce
heartbeat slides the key's expiry. Issue ONE token per host subject
(`specialist-runner/<host>`), and revoke a host's token before re-issuing it.

## Host inventory commands (#4330)

`inventory` accepts four read-only, argument-free host ids beside `git_log` /
`ls_files` / `go_list`: `host.memory`, `host.top_memory`, `host.load`,
`host.containers` (`InventoryHost*`). They are the diagnostics the first
officer's host-health duty requests; they observe and never remediate
(ADR-089 D9). The schema closes their `args` to the empty object, so a host id
carrying any member, and every id outside `InventoryCommands` (`host.shell`,
`sh`, `git`, a lookalike), is a `400` schema error at request time. The
completion and result `reason` enums gain `not_applicable` (the executing
runner's OS has no host table). `TestInventoryCommands_MatchSchemaEnums` pins
both command enums to `InventoryCommands` and both reason enums to each other;
`TestParseCompletion_HostInventoryAndNotApplicable` pins that a `host.memory`
evidence completion and a `not_applicable` one parse. The per-OS read set,
the no-clone routing and the bounding live runner-side (`runner/README.md` §
"Specialist jobs"; table: `docs/spec/specialist-job-v1.md` § "Host inventory
commands").

## Residuals

- **Host inventory evidence names host processes.** A `host.top_memory` or
  `host.containers` completion carries process / container names and pids of
  the executing host onto the job's `specialist_job_completed` row. The runner
  strips control and format characters, cuts each parsed name to 64 bytes and
  bounds every line (2000 lines of ≤1000 chars, redacted); the names are
  evidence data, not instructions. On a podman-only host `host.containers` is
  `inconclusive` (`execution_error`): only a safe local docker endpoint is
  read.

- **Host trust.** Attestation proves possession of a credential a plain
  `write:stages` bearer cannot mint over the API. It does NOT isolate the
  runner token from a same-host, same-OS-user process that can read the
  `fishhawk-mcp` (or `fishhawkd`) environment, and an operator agent with
  shell access on the host is such a process. Stronger isolation (a runner
  identity the operator host cannot read) is a follow-up against ADR-089.
- **Subject binding.** Same-subject tokens are interchangeable attesters
  (see Rollout).
- **Key expiry is 5 minutes after the last attester heartbeat.** A terminal
  result shipped later than that is refused `attestation_expired`, and the
  runner fails the job rather than re-shipping unsigned. The runner's
  terminal-egress budget (~90s) is well inside the window.
- **The "a job cannot mutate run state" guarantee** is enforced runner-side
  (the allow-listed job transport and the git guard) and server-side by the
  result handler, which writes only `specialist_jobs` and the audit chain;
  attestation adds result provenance, not a narrower write surface.
- **`stale` is judged on fishhawkd's clock** against the DB-stamped
  `last_seen_at` (a cross-clock-domain comparison), so it is an advisory read
  hint; admission and takeover decide staleness on the database clock.
- **A completion retry is ambiguous after a transport error**: if the first
  POST landed, the retry answers `409 specialist_job_not_running`; the runner
  re-reads the job to tell a landed result from a lost one.
- **Rollback order.** Revert the fishhawkd binary BEFORE migrating 0103 down:
  the concurrency store's job-holder count reads `specialist_jobs`, so a binary
  that still counts job holders against a dropped table errors every
  local-verify admission (`backend/internal/concurrency/README.md`).
  Migration 0104 likewise: revert the binary FIRST, then migrate 0104 down
  (it drops only the four attestation/provenance columns and their checks).
  A pre-0104 binary runs unchanged against the 0104 schema (every added column
  is nullable, and its completions leave `provenance` NULL, which reads as
  `bearer`), so a binary-only rollback needs no migration.
