# Audit-complete derivation

Per-area appendix for the `Audit-complete derivation (#229, #282)` row in [ARCHITECTURE.md](../ARCHITECTURE.md). Hand-extracted from that row for readability; content is verbatim, not a rewrite.

Implementation: `backend/internal/auditcomplete` derives the `fishhawk_audit_complete` blocking-check state on demand — no row in `stage_checks`, no hook on writes. `Compute(ctx, runID, deps) (state, missing, err)` walks five rules.

## The five rules

1. Every plan stage produced a `kind=plan, schema_version=standard_v1` artifact.
2. Every non-review stage that executed has `trace_uploaded` audit entries for both raw and redacted variants (E2.4 #220).
3. Every implement stage produced a `kind=pull_request` artifact.
4. The run's audit chain re-hashes consistently via `audit.ComputeEntryHash` over each entry's `HashInputs`.
5. **The PR's live HEAD on GitHub is one of the Fishhawk-recorded head_shas across the run + its `parent_run_id` chain** (#282 — closes the "foreign commit lands on PR" audit-integrity gap).

## Normalization (rule 4 specifics)

The function applies two normalizations so the canonical form is stable across the `time.Now()` → INSERT → SELECT round-trip:

- `Timestamp` to `UTC().Truncate(time.Microsecond)` (#302; PG `timestamptz` is microsecond + read-back in connection's TZ).
- `Payload` via parse + re-marshal with `json.Decoder.UseNumber()` (#308; the `payload` column is JSONB which doesn't preserve key order or whitespace, so write-time `json.Marshal` bytes and pgx-read bytes differ shape for any multi-key payload).

Both normalizations are mirrored in the verifier package per ADR-008 / #72.

## Rule 5 (live-HEAD check) details

Rule 5 is gated on a `PRHead` callback in `Deps` (production wires `githubclient.Client.GetPullRequest`; dev / CLI runs leave it nil to skip the rule cleanly). Drift → `foreign_commit` missing item with both observed + known shas in the detail; GitHub fetch failure → `head_fetch_failed` (pending-flavored — `onlyHeadFetchFailures` demotes the overall state to pending so a flapping signal doesn't trip branch protection).

## State output

- `pending` while any non-review stage is non-terminal OR only `head_fetch_failed` items are present.
- `fail` with a structured `missing []{kind, detail}` list when other rules trip.
- `pass` only when all five rules clear.

Compute-on-read per #229's recommendation; cheap on the write path.

## Integration points

`server/checks.go::handleListStageChecks` injects a synthetic row carrying `state` + `missing[]` so the SPA can render "fail because: plan missing, redacted trace missing on stage X" without a secondary call. (Pre-#253 `server/approvals.go::checkBlockingChecks` also special-cased the name to gate the approval API — that gate moved to GitHub branch protection per ADR-017 / #249.)

The publisher (`backend/internal/auditcheckpublisher`) mirrors the state to the PR as a Check Run (#231) so branch protection can enforce it.

## Republish on drift

`pull_request.synchronize` webhooks fire `server/pullrequest_synchronize.go::republishOnSynchronize`, which looks up the matching Fishhawk run via `runs.pull_request_url` (#216) and re-runs Compute + publish so branch protection sees the drift immediately rather than waiting for the next SPA visit. Falls open (returns pass) when `ArtifactRepo` or `AuditRepo` aren't wired — same posture as the other check-derivation paths.

## Verifier mirror

The verifier package (`/verifier/internal/audit`) ships an external mirror of rules 1–4; rule 5 is **backend-only** because the verifier doesn't have GitHub access.
