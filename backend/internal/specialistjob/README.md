# specialistjob

ADR-097 D2/D6 specialist jobs (E85.9 / #4300): bounded, typed evidence work a
seat requests and a runner executes in a fresh scratch clone, recorded on the
audit chain. Contract: `docs/spec/specialist-job-v1.md` (canonical schema
`docs/spec/specialist-job-v1.schema.json`, mirrored into `schemas/` by
`scripts/sync-schemas`).

## Files

| File | Owns |
|---|---|
| `specialistjob.go` | Kinds, verdicts, seats; `ParseRequest` / `ParseCompletion` (duplicate-member walk → schema → strict decode → Go semantic rules); `ValidateResult`; `DecodeEvidence`; the deterministic `RenderDraft`. |
| `store.go` | The pgx store over `specialist_jobs` (migration 0103): `Create`, `CreateCompleted`, `Get`, `AdmitJob`, `Complete`. |
| `backend/internal/server/specialist_job.go` | The four `/v0/specialist-jobs` handlers. |
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
| `POST /v0/specialist-jobs` | `write:stages` | 64 KiB cap (`413 request_too_large`). Run-bound token → `403 specialist_job_requester_not_a_seat`. Subject run / campaign outside the caller's account → `404 run_not_found` / `campaign_not_found`. `draft` → `201 completed`; else `201 queued`. |
| `GET /v0/specialist-jobs/{job_id}` | `read:runs` | Never renders `execution_nonce`. Advisory `stale`. |
| `POST .../{job_id}/admission` | `write:stages` | `{host, nonce}`; admit / heartbeat / stale takeover. `409 specialist_job_not_admissible` / `specialist_job_already_running`. |
| `POST .../{job_id}/result` | `write:stages` | 256 KiB cap. Parsed against the STORED kind (`400`, `details.reason: kind_mismatch`). `409 specialist_job_not_running` / `specialist_job_nonce_mismatch`. |

**Requester.** Server-derived from the authenticated identity, never read from
the body (the closed request def refuses a `requester` member):
`seat: operator` (no first-officer identity exists yet; E85.6 adds the
derivation branch, request scope tracked in #4148), `actor_kind` from the token
subject (`actorKindForSubject`: an operator-agent token is `agent`, every other
identity `user`), `subject` = the identity subject.

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
| `specialist_job_completed` | same | `{job_id, kind, requester (from the job ROW), subject, verdict, reason, evidence, charged_to: {seat, subject}, duration_ms, requested_sequence, provenance: "bearer"}`. |

**Result provenance is `bearer`, not runner-signed.** The runner ships the
completion under the operator bearer fishhawk-mcp forwards
(`FISHHAWK_API_TOKEN`), so a specialist result is NOT ADR-089 D4 blocking-grade
evidence until runner-signed results land (#4363).

## Residuals

- **Job authentication is the operator bearer, not a job-scoped token.** The
  "a job cannot mutate run state" guarantee is enforced runner-side (the
  allow-listed job transport and the git guard) and server-side by the result
  handler, which writes only `specialist_jobs` and the audit chain. A
  job-scoped token with server-enforced scopes is a stronger follow-up.
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
