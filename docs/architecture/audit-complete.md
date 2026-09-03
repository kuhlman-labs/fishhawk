# Audit-complete derivation

Per-area appendix for the `Audit-complete derivation (#229, #282)` row in [ARCHITECTURE.md](../ARCHITECTURE.md). Hand-extracted from that row for readability; content is verbatim, not a rewrite.

Implementation: `backend/internal/auditcomplete` derives the `fishhawk_audit_complete` blocking-check state on demand — no row in `stage_checks`, no hook on writes. `ComputeResult(ctx, runID, deps) (Result, err)` walks six rules and returns `{state, missing[], resolved[]}`. `Compute(ctx, runID, deps) (state, missing, err)` is a signature-identical thin wrapper that drops `resolved` — kept so every pre-#3092 call site is unchanged.

## The six rules

1. Every plan stage produced a `kind=plan, schema_version=standard_v1` artifact.
2. Every non-review stage that executed has `trace_uploaded` audit entries for both raw and redacted variants (E2.4 #220). **Rule 2b (#3092): on a DECOMPOSED run the parent implement stage's gap is RESOLVED through the fan-out** — see below.
3. Every implement stage produced a `kind=pull_request` artifact.
4. The run's audit chain re-hashes consistently via `audit.ComputeEntryHash` over each entry's `HashInputs`.
5. **The PR's live HEAD on GitHub is one of the Fishhawk-recorded head_shas across the run + its `parent_run_id` chain** (#282 — closes the "foreign commit lands on PR" audit-integrity gap).
6. **A configured agent implement-review (ADR-027 `reviewers.agent` > 0) reached a terminal verdict** (#947 — the pre-merge **presence** gate). Drives `review_pending` while a dispatched review has not landed.

## Rule 2b (decomposition trace resolution, #3092) details

A decomposed run's **parent implement stage is the fan-out stage**: it parks `awaiting_children`, spawns no agent, and by construction can never ship a trace bundle. Rule 2 was therefore unsatisfiable for EVERY decomposed run — the required check could never go green and the run could never merge.

The fix RESOLVES the evidence through the fan-out rather than exempting the stage. When the parent implement stage has a trace gap AND the run has decomposition children (`run.ListRunsFilter.DecomposedFrom`), Compute reads each child run's own implement-stage `trace_uploaded` entries and satisfies the parent's requirement only when every executed child is trace-complete. **Resolution is never an exemption**: a child genuinely missing a trace still FAILS, naming the child run id and the child stage id.

Only the IMPLEMENT stage is eligible. The misses are partitioned by owning stage id through a parallel slice returned by `missingTraces` (never by string-matching `Detail`, and never by a new field on the JSON-serialized `MissingItem`), so a plan- or acceptance-stage trace gap passes through verbatim in every branch — children carry no evidence for it.

Five branches are fail-closed by construction, each pinned by its own test:

| Child set | Outcome |
|---|---|
| A child's implement stage is NON-terminal (`pending`, `running`, `awaiting_*`) | The parent's opaque `trace_missing` items are REPLACED by a single pending-flavored `children_pending` item. State `pending` — not-yet, never a pass and never a hard fail. |
| A child's OWN audit chain does not verify | The child contributes NOTHING regardless of how complete its trace evidence looks, and the parent's items are REPLACED by a `chain_invalid` (hash mismatch) / `chain_unrecoverable` (read or recomputation failure) item naming that child. State `fail` — neither kind is pending-flavored. |
| Children exist but NONE contributed evidence (every child cancelled, or carrying no implement stage) | NO `Resolution` at all. A `Resolution` is a POSITIVE claim that named child runs supplied the traces, so it is constructed only from a non-empty contributor set; the parent's own `trace_missing` items stand. |
| The child query hit its page ceiling without a short page proving exhaustion | OVERFLOW: no resolution, the parent's items stand. A truncated read can never become a pass. |
| The run has NO children (a flat run), or is non-decomposed | Unchanged — byte-identical to the pre-#3092 output. No silent exemption. |

Child enumeration is **complete, not a bounded prefix**: `ListRuns(DecomposedFrom)` is paginated to exhaustion (`childPageSize` = 100) because `plan-standard-v1.schema.json` declares no `maxItems` on `decomposition.sub_plans`, so there is no schema cap to lean on and a fixed `Limit` would let the resolution inspect a prefix and pass while an omitted child had no trace. `childPageCeiling` (100 pages) is a non-termination guard only, and its exhaustion is the OVERFLOW row above. Every read failure that decides WHICH evidence to look at (child query, child stages, child traces) is returned as a **transient error**, matching rule 2's I/O posture — never a silent pass. The child-chain read is the deliberate exception: like rule 4 on the parent's own chain, a failure to read or re-hash it is categorized as a `chain_unrecoverable` missing item rather than a retryable error, so an unreadable child chain gates the merge instead of looking like a blip.

**Child-chain integrity.** Rule 4 verifies the PARENT run's chain only — `verifyChain` is scoped to a single run's entries — and a child run need never pass through any check that verifies its own chain. Reading a child's `trace_uploaded` rows without verifying that chain would let a decomposed parent's REQUIRED merge check pass on evidence that does not hash, an audit-integrity hole that exists for decomposed runs alone. So the resolver verifies each candidate child's chain FIRST, before any of its entries are trusted: the `trace_uploaded` rows come out of that same chain and are only as trustworthy as it is. Pinned by `TestComputeResult_DecompositionChainInvalidChildRejected`, whose "with a healthy sibling" case discriminates a real rejection from a fixture accident — a resolution built over the untampered sibling alone would still pass.

**Child provenance.** A run row carrying `decomposed_from == parent` is accepted as a legitimate child without further proof. That is sound because `decomposed_from` is not caller-settable: `server.createRunRequest` has no such field, so no REST or MCP client can mint a run claiming this parentage, and the only writers are the fan-out paths themselves (the orchestrator's child mint, consolidate, the childcompletion sweeper), each of which already holds the parent run. Anyone able to write a run row directly already has backend/database access and could forge the parent's own evidence just as easily.

The resolution is surfaced as evidence, not hidden: `ComputeResult` returns an `auditcomplete.Resolution` naming the satisfying child run ids, `GET /v0/stages/{id}/checks` carries it as the audit-complete row's `resolved[]` array, and the published Check Run's pass summary names those child runs so an auditor can follow the chain rather than trust the resolution.

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

- `pending` while any non-review stage is non-terminal OR the only gaps are pending-flavored (`head_fetch_failed`, `review_pending`, `security_findings_unverified`, and/or `children_pending` — a decomposition child whose implement stage has not terminated, #3092).
- `fail` with a structured `missing []{kind, detail}` list when other rules trip.
- `pass` only when all six rules clear — possibly with rule 2's parent implement-stage gap RESOLVED through the decomposition children, in which case `ComputeResult` also returns a `resolved []{kind, stage_id, child_run_ids, detail}` list naming them (#3092).

Compute-on-read per #229's recommendation; cheap on the write path.

## Integration points

`server/checks.go::handleListStageChecks` injects a synthetic row carrying `state` + `missing[]` (+ `resolved[]`, #3092) so the SPA can render "fail because: plan missing, redacted trace missing on stage X" without a secondary call. (Pre-#253 `server/approvals.go::checkBlockingChecks` also special-cased the name to gate the approval API — that gate moved to GitHub branch protection per ADR-017 / #249.)

The publisher (`backend/internal/auditcheckpublisher`) mirrors the state to the PR as a Check Run (#231) so branch protection can enforce it.

## Republish on drift

`pull_request` webhooks with action `opened`, `reopened` or `synchronize` fire `server/pullrequest_synchronize.go::republishOnPullRequestEvent`, which looks up the matching Fishhawk run via `runs.pull_request_url` (#216) and re-runs Compute + publish so branch protection sees the drift immediately rather than waiting for the next SPA visit. Falls open (returns pass) when `ArtifactRepo` or `AuditRepo` aren't wired — same posture as the other check-derivation paths.

`opened` / `reopened` were added in E64.43 (#3160); before that only `synchronize` was routed, which meant a PR never pushed to after opening (the Dependabot shape) received no publish at all. Routing the extra actions is safe for the run-BEARING path because the publisher's per-`(forge, repo, head_sha)` dedup cache makes a repeat at the same head a no-op.

## Every PR class receives a terminal check (E64.43 / #3160)

Once `fishhawk_audit_complete` is marked a **Required** status check, a PR that never gets the context published is blocked forever. Since #3160 the contract is complete:

- A **run-bearing** PR goes through the existing `ComputeResult` + `publishAuditCheck` path and receives `in_progress` → `success` / `failure` exactly as before.
- A **run-less** PR — Dependabot, a human hotfix, an operator-authored docs PR — receives a terminal `completed` / `neutral` Check Run via `auditcheckpublisher.PublishNotApplicable`, whose summary states plainly that no Fishhawk run is associated with the PR and the audit gate does not apply. GitHub treats `success`, `neutral` and `skipped` as satisfying a required context, and `neutral` stays honest that nothing was verified.

**What keeps a Fishhawk-managed PR out of the run-less path** is the App-identity discriminator in `server/pullrequest_synchronize.go::authoredByFishhawkApp`. Zero runs on a PR is not by itself proof the PR is foreign: an `opened` webhook for a Fishhawk-managed PR can arrive before `runs.pull_request_url` is denormalized. So before publishing not-applicable the handler must positively establish the PR was neither opened nor pushed by Fishhawk's own App — matching the App's own `<app-slug>[bot]` login (resolved from `GET /app`, never a hardcoded literal) case-insensitively against BOTH the PR author and the event sender.

The guard is **fail-closed in both directions**: an unresolvable App identity publishes nothing and logs at WARN, and a `ListRuns` error publishes nothing (an error is not evidence of zero runs). The direction is deliberate — a missing publish leaves a PR blocked, which an admin merge recovers; a wrong publish greens a real audit gate silently, which nothing recovers. Long-form contract, including the stated residuals: `backend/internal/server/README.md` and `backend/internal/auditcheckpublisher/README.md`.

## Reconcile sweep

Every publish surface above is one-shot best-effort: a transient GitHub failure (the #971 401 during the 2026-06-10 incident) drops the Check Run, the publisher dedup cache stays unrecorded (a failed publish never poisons it — pinned by `TestPublish_GitHubError_Returned`), and nothing retries until the next SPA visit or webhook (#973). The merge reconciler (`backend/internal/mergereconciler/`, `--enable-merge-reconciler`) closes this: each tick calls `server.RepublishAuditCheck` (→ `recomputeAndPublishAuditComplete`) for every review stage parked in `awaiting_approval`, BEFORE the merge poll so a GitHub poll failure cannot also skip the heal. A dropped publish heals within one tick of GitHub recovering; an already-published state dedups to a no-op. Re-creating a Check Run that already exists is safe — GitHub evaluates the latest check run per `(name, head_sha)`.

**Persistent-failure surfacing (#993).** The publisher tracks consecutive `CreateCheckRun` failures per `(run_id, head_sha)` episode (only the publish attempt proper counts; read errors and skip paths don't). After `auditcheckpublisher.DefaultDegradedThreshold` (5) consecutive failures — ~5 minutes at the reconciler's one attempt per tick — the server appends a chained `audit_check_publish_degraded` run-scope audit entry (payload `{head_sha, attempts, last_error}`), exactly once per episode, so the desynced merge gate is visible from `fishhawk_get_run_status` and the SPA without a daemon-log grep. The eventual successful publish appends the paired `audit_check_publish_recovered` entry (`{head_sha, attempts}`); a dedup no-op closes an open episode the same way — when another run sharing the head commit published the identical state first, the observing run's next sweep appends its own recovered entry without re-POSTing. Pairing is restart-proof: the publisher invokes its recovered callback on every successful publish and every dedup hit, and the server consults the run's audit chain (an open episode = a degraded entry for that head_sha with no later recovered entry) — only the *threshold counter* is in-memory, so a daemon restart mid-outage can at worst emit one extra degraded entry per restart (same posture as the dedup cache), never orphan one. Threshold is a package const, not operator-configurable; an issue-comment mirror is a possible follow-up.

## Verifier mirror

The verifier package (`/verifier/internal/audit`) ships an external mirror of rules 1–4; rules 5 and 6 are **backend-only** — rule 5 needs GitHub access, rule 6 needs the live spec-reviewers + backstop closures, neither of which the verifier has. **Rule 2b (#3092) is backend-only for the same reason**: resolving a decomposed parent's trace through its children needs a live `ListRuns(DecomposedFrom)` query against the run repository, which an export-based verifier cannot issue — so the verifier's rule-2 mirror still reads a decomposed parent's fan-out implement stage as trace-less. That is a known, deliberate divergence, not a defect in either side.
