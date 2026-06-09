# Audit-complete derivation

Per-area appendix for the `Audit-complete derivation (#229, #282)` row in [ARCHITECTURE.md](../ARCHITECTURE.md). Hand-extracted from that row for readability; content is verbatim, not a rewrite.

Implementation: `backend/internal/auditcomplete` derives the `fishhawk_audit_complete` blocking-check state on demand — no row in `stage_checks`, no hook on writes. `Compute(ctx, runID, deps) (state, missing, err)` walks six rules.

## The six rules

1. Every plan stage produced a `kind=plan, schema_version=standard_v1` artifact.
2. Every non-review stage that executed has `trace_uploaded` audit entries for both raw and redacted variants (E2.4 #220).
3. Every implement stage produced a `kind=pull_request` artifact.
4. The run's audit chain re-hashes consistently via `audit.ComputeEntryHash` over each entry's `HashInputs`.
5. **The PR's live HEAD on GitHub is one of the Fishhawk-recorded head_shas across the run + its `parent_run_id` chain** (#282 — closes the "foreign commit lands on PR" audit-integrity gap).
6. **A configured agent implement-review (ADR-027 `reviewers.agent` > 0) reached a terminal verdict** (#947 — the pre-merge **presence** gate). Drives `review_pending` while a dispatched review has not landed.

## Normalization (rule 4 specifics)

The function applies two normalizations so the canonical form is stable across the `time.Now()` → INSERT → SELECT round-trip:

- `Timestamp` to `UTC().Truncate(time.Microsecond)` (#302; PG `timestamptz` is microsecond + read-back in connection's TZ).
- `Payload` via parse + re-marshal with `json.Decoder.UseNumber()` (#308; the `payload` column is JSONB which doesn't preserve key order or whitespace, so write-time `json.Marshal` bytes and pgx-read bytes differ shape for any multi-key payload).

Both normalizations are mirrored in the verifier package per ADR-008 / #72.

## Rule 5 (live-HEAD check) details

Rule 5 is gated on a `PRHead` callback in `Deps` (production wires `githubclient.Client.GetPullRequest`; dev / CLI runs leave it nil to skip the rule cleanly). Drift → `foreign_commit` missing item with both observed + known shas in the detail; GitHub fetch failure → `head_fetch_failed` (pending-flavored — `onlyPendingFlavored` demotes the overall state to pending so a flapping signal doesn't trip branch protection).

## Rule 6 (review-presence gate, #947) details

Rule 6 is gated on the `ImplementReviewers` + `ReviewBackstop` closures in `Deps` (production wires `server.resolveStageReviewers` + `server.planReviewBackstop`; dev / CLI / unwired posture leaves them nil to skip the rule cleanly). When the implement stage's spec declares `reviewers.agent` > 0 and at least one `implement_review_started` entry exists but fewer than the configured count of TERMINAL verdicts (`implement_reviewed` / `implement_review_failed` / `implement_review_skipped`) have landed, Compute appends a `review_pending` item — **pending-flavored**, so a not-yet-landed review holds the required check at `pending` (wait), never `fail` (broken). It is the **presence** gate, NOT the advisory-verdict gate: ANY terminal kind clears it (ADR-027 keeps `approve_with_concerns` non-blocking). The decision is a single source of truth — `auditcomplete.ReviewPresent` — shared with the ADR-036 merge-resolution hold (`server.checkImplementReviewSettled`), so the audit-complete rule and the merge gate cannot diverge; the same backstop (`ReviewBudget.Cap` × configured agents, anchored on the earliest dispatch) clears a reviewer that died emitting no terminal entry. When the review lands, `runImplementReviewLoop` calls `server.recomputeAndPublishAuditComplete`, which re-derives + republishes the Check Run so branch protection re-evaluates and the merge gate flips green with no operator action. Distinct from ADR-036's merge-completion hold, which gates the *merge reconciliation*; rule 6 gates the *required check* itself, making the advisory review a pre-merge precondition rather than post-merge bookkeeping.

## State output

- `pending` while any non-review stage is non-terminal OR the only gaps are pending-flavored (`head_fetch_failed` and/or `review_pending`).
- `fail` with a structured `missing []{kind, detail}` list when other rules trip.
- `pass` only when all six rules clear.

Compute-on-read per #229's recommendation; cheap on the write path.

## Integration points

`server/checks.go::handleListStageChecks` injects a synthetic row carrying `state` + `missing[]` so the SPA can render "fail because: plan missing, redacted trace missing on stage X" without a secondary call. (Pre-#253 `server/approvals.go::checkBlockingChecks` also special-cased the name to gate the approval API — that gate moved to GitHub branch protection per ADR-017 / #249.)

The publisher (`backend/internal/auditcheckpublisher`) mirrors the state to the PR as a Check Run (#231) so branch protection can enforce it.

## Republish on drift

`pull_request.synchronize` webhooks fire `server/pullrequest_synchronize.go::republishOnSynchronize`, which looks up the matching Fishhawk run via `runs.pull_request_url` (#216) and re-runs Compute + publish so branch protection sees the drift immediately rather than waiting for the next SPA visit. Falls open (returns pass) when `ArtifactRepo` or `AuditRepo` aren't wired — same posture as the other check-derivation paths.

## Verifier mirror

The verifier package (`/verifier/internal/audit`) ships an external mirror of rules 1–4; rules 5 and 6 are **backend-only** — rule 5 needs GitHub access, rule 6 needs the live spec-reviewers + backstop closures, neither of which the verifier has.
