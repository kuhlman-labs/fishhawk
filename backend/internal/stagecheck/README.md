# backend/internal/stagecheck

Stage blocking-check ingestion (#228): typed surface over the `stage_checks` table.

## Data model and API

The `stage_checks` table (migration 0015) records every observed state of every blocking check declared on a stage's gate. Append-only; the latest row per `(stage_id, check_name)` is what consumers see.

This package exposes the typed surface: `Append`, `LatestForStage`, `LatestForStageAndName`, `FindMatchingStages` (the (PR, head_sha, check_name) → stage_id lookup the ingester uses).

State derivation lives in `DeriveState(status, conclusion)`:

- pass on `success`/`neutral`/`skipped`
- fail on `failure`/`timed_out`/`cancelled`/`action_required`/`stale`/`startup_failure`
- pending on anything still in progress or carrying a conclusion we haven't catalogued

## Ingest and read paths

- Ingest: `backend/internal/server/checkrun.go::ingestCheckRun` parses the `check_run` event, walks `pull_requests[]`, asks the repo for matching stages via `(pr_number, head_sha, check_name)`, and appends a row per match.
- Read: `GET /v0/stages/{id}/checks` returns the gate's declared list + the latest observed state per name.

**The approval handler does NOT gate on this data as of #253 / ADR-017** — reviewers approve based on plan + diff; GitHub branch protection blocks the merge until the required checks (including `fishhawk_audit_complete`, published as a Check Run per #231) report green. The `stage_checks` table still feeds the review-page panel as informational live state.

The `fishhawk_audit_complete` self-derivation is the unfilled half of the same data model — see `backend/internal/auditcomplete/README.md` and `docs/architecture/audit-complete.md`.
