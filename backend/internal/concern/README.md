# backend/internal/concern

Durable concern lifecycle with stable IDs: every review verdict's `concerns[]` persisted so fix-up routing addresses concerns by ID.

## v1 slice — stable IDs and lifecycle (#964, E22.X)

Every `plan_reviewed` / `implement_reviewed` verdict's `concerns[]` is persisted with a server-minted stable UUID so fix-up routing addresses concerns by ID instead of a flattened positional index (ambiguous once multiple heterogeneous review entries exist per stage — the run-73456dc8 mis-route).

Storage: this package (domain `Concern`/`State`/`Transition` + `Repository` {InsertRaised, GetByIDs, ListByRun, ListOpenByRun, MarkAddressedPending, ApplyResolution}; sqlc `db/`; migration `0030_review_concerns` — severity/category/state are tolerant plain TEXT, validity enforced by the Go state machine only).

The lifecycle enum ships in full (`raised`, `addressed_pending`, `addressed`, `reopened`, `waived`, `superseded`, `deferred`) with **reopen-wins-over-confirm** encoded order-independently: `addressed → reopened` is a valid edge (a reopen applies even after a confirm landed first) while `reopened → addressed` is rejected with a loggable `InvalidTransitionError` (a late confirm never silently downgrades a reopen).

Persistence rides the review loops (`server/trace.go::persistReviewConcerns`, called from `runImplementReviewInvocations` + `plan.go::runPlanReviewLoop`): rows are inserted AFTER the audit append using the sequence `AppendChained` RETURNs (`origin_review_sequence` NOT NULL — the audit chain stays the sole sequence authority), best-effort/warn-only (the audit payload remains the authoritative record; the store is a derived index).

Surfaces:

- `GET /v0/runs/{run_id}` attaches a `concerns` block ({open, by_state, items:[{id, stage_kind, severity, category, state}]}, OPEN states only, note text elided) on the single-run read ONLY — `toRunResponse` stays pure and the list endpoint never gains a per-row concern query; `fishhawk_get_run_status` mirrors it via the MCP `Run.Concerns` type.
- `POST /v0/stages/{id}/fixup` accepts `concern_ids` as the PRIMARY addressing scheme (`server/fixup.go::resolveConcernsByID` — scoped to the stage; unknown/foreign/plan-stage/non-open IDs → 400 `validation_failed`, so a plan-stage concern can never route into an implement fix-up; needs the store, else 503 `fixup_unconfigured`), marks routed concerns `addressed_pending` after the `stage_fixup_triggered` append (which now also records the routed `concern_ids`), and keeps positional `concerns` indices as a DEPRECATED fallback (both at once → 400).

Wiring: `server.Config.ConcernRepo`, constructed in `serve.go`.

## v2 slice — waive + delta-verifying re-reviews (#984, E22.X)

The deferred #964 verbs, landed with no migration (the enum + tolerant columns were shaped for it):

1. **Operator waiver** — `POST /v0/concerns/{concern_id}/waive` (`server/waive.go::handleWaiveConcern`; MCP verb `fishhawk_waive_concern`) transitions any OPEN concern to terminal `waived` with a REQUIRED reason, under fix-up's exact auth shape (`write:stages`/`write:fixups` + the `mcp:run:<uuid>` cross-run guard → 403 `cross_run_waive`).
   Audit-before-mutation: the `concern_waived` entry appends FIRST (append failure → 500 `audit_append_failed`, NO mutation); a transition that then fails appends a corrective `concern_waive_failed` entry (warn-only) and returns 422 `concern_waive_conflict` — a mutation can never exist without a durable record, in every interleaving.
2. **Delta-verifying re-reviews** — `trace.go::priorConcernsForReview` threads the stage's open + waived implement concerns into `prompt.Trigger.PriorConcerns` (warn-and-proceed on a list error — a store outage never blocks dispatch).
   `buildImplementReview` renders a binding `### Prior concerns (delta verification)` section (every `addressed_pending` concern MUST get a `concern_resolutions` entry — confirmed/reopened/superseded; waived concerns are not-re-litigable context carrying the operator's audited reason; `concerns[]` never re-mints a listed concern) and extends the inline verdict schema with `concern_resolutions` — both only when the set is non-empty, so a first review's prompt stays byte-identical.
3. **Resolution processing** — `planreview.ReviewVerdict`/`ImplementReviewedPayload` gain tolerant-decode `ConcernResolutions` (omitempty; old reviewer output and old stored payloads stay valid — the audit payload remains the authoritative record).
   After each successful `implement_reviewed` append, `trace.go::applyConcernResolutions` maps `confirmed → addressed`, `reopened → reopened`, `superseded → superseded` through `ApplyResolution`, warn-and-skipping every malformed/unknown/foreign-stage/plan-stage/invalid-transition entry (valid siblings still apply) so a sloppy reviewer can never wedge the gate; reopen-wins-over-confirm needs no reconciliation pass — the state machine encodes it order-independently.

## Defer slice — operator defer (#1202, E22.X)

The third concern-resolution verb, also no migration (`deferred` rides the tolerant TEXT `state` column): **operator defer** — `POST /v0/concerns/{concern_id}/defer` (`server/defer_concern.go::handleDeferConcern`; MCP verb `fishhawk_defer_concern`) converts any OPEN concern into a conventions-complete, boarded, epic-linked follow-up work item AND transitions it to terminal `deferred` in one call (consuming NO fix-up budget), sitting between `/fixup` (route back to the agent) and `/waive` (resolve with no follow-up).

- Auth byte-identical to `/waive` (`write:stages`/`write:fixups` + the `mcp:run:<uuid>` cross-run guard → 403 `cross_run_defer`).
- The follow-up body is auto-drafted from the concern (note, severity, category, reviewer model, evidence run id, source PR link); the operator supplies only `parent_epic` + `n` (the non-derivable title coordinates), `type` defaulting to `bug` for a defect category else `chore`.
- The filing pipeline is the extracted `workitems.go::applyAndFileWorkItem` (the #1005 confused-deputy egress gates stay on the HTTP filing handler verbatim — defer resolves its already-authorized run's installation directly).
- **Orphan-issue safety + audit-ordering (inverse of waive):** an open-state PRE-CHECK runs BEFORE any filing (a non-open concern → 422 `concern_defer_conflict`, provider `File` never called); the issue is filed FIRST then the concern transitions; a filing failure leaves the concern OPEN (no transition, no audit); the success `concern_deferred` entry is appended ONLY AFTER the transition succeeds (a fact, not an attempt); a post-filing transition race emits ONLY a corrective `concern_defer_failed` entry (naming the actual state + the orphaned issue url) and returns 422.
- Surfaced in `next_actions`' `implement_concerns_open` arm between fix-up and merge-with-follow-up, in both budget branches.

## Suggested-patch data slice (#1165, E22.X)

An implement-review concern may optionally carry a `suggested_patch` (a unified diff that applies to the PR branch):

- `planreview.Concern.SuggestedPatch` (`json:"suggested_patch,omitempty"`) flows onto the `implement_reviewed`/`plan_reviewed` audit payloads via the embedded `[]Concern` with no payload-struct change, and old reviewer output decodes unchanged.
- Persisted on `review_concerns.suggested_patch` (migration `0033_concern_suggested_patch`, `TEXT NOT NULL DEFAULT ''` so pre-existing rows read back `''`) → decoded through `persistReviewConcerns` and mapped in `concern.RaisedConcern`/`Concern`.
- `buildImplementReview`'s verdict schema instructs reviewers to populate it ONLY for a mechanical concern whose fix is a small self-contained diff (leave absent otherwise); `buildPlanReview` is unchanged — plan-review concerns are about the plan artifact, not code.
- The `GET /v0/runs/{run_id}` concerns block surfaces a bounded `has_suggested_patch` boolean per item (the diff text stays elided like the note); it is the signal the near-deterministic fix-up apply path keys on (delivering and applying the patch is a separate behavior slice).
