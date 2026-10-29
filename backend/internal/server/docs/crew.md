# backend/internal/server: Captain, crew, delegation and historian surfaces

Split out of `backend/internal/server/README.md` (the index) on 2026-10-29 so concurrent runs stop colliding on one file. Section text is moved verbatim.

## Delegation read (`delegation_view.go`, E76.1 / #3747)

`GET /v0/repos/{owner}/{name}/delegation` answers "what may the crew decide on
its own in this repository?" for every workflow the spec declares — from the
spec at a ref, WITHOUT a run. Read-only: it writes nothing, mints NO audit
entry, and grants no authority. The projection is pure
(`backend/internal/delegationview`, whose README holds the long-form contract);
this file only resolves the source, parses, and maps.
Since E76.5 / #3768 the body also carries, when `DelegationConfirmStore` is
wired, a per-workflow `confirmation` verdict and a view-level `confirmation`
summary (`captain`, `seat_sequence`, `unconfirmed_workflows`) — computed after
the `?workflow` filter and after every hash is stamped. A nil store OMITS both
(never a false confirmed); a chain-read failure reports
`confirmation.unavailable`. The read still writes nothing. See § "Delegation
confirmation".

**The gate is the shared one.** `repoDashPrelude(w, r, false, false)` — so the
run-repo 503 and the `requestRepoFilter` + `repoVisibleOr403` point-read DENY are
byte-for-byte the ones the four repo-dashboard rollups use. There is no second
visibility path to keep in sync.

**Two explicit spec sources, always echoed, never switched silently.**

| `source` | Resolution |
|---|---|
| `ref` (the default) | `GetRepoInstallation` → `GetWorkflowSpec(…, ref)`. An empty `ref` serves the repository's DEFAULT BRANCH head — `githubclient.GetFile`'s documented `ref=""` behaviour, the same one `handleGetOnboardingReadiness` and `campaign_admission_screen.go` rely on, and the endpoint's "base branch head" default |
| `run_cache` | the newest run's `WorkflowSpec` + `WorkflowSHA` — the `projectPosture` ladder, same account narrowing |

Two sources rather than forge-only because a forge-only surface is unreadable on
any deployment without a wired installation, which would leave the endpoint with
no drivable acceptance criterion. What keeps that from becoming a lie is the
refusal: an unconfigured forge under `source=ref` is 503 `github_unconfigured`
whose message names `run_cache`, never a fallback, and the resolved `source` is a
required field on every response.

**Invalid spec is fail-closed with a `return`, not a degradation.** A
`spec.ParseBytes` failure writes 422 `workflow_spec_invalid` and returns
IMMEDIATELY: the body carries the validation error and NO `workflows` array and
NO `content_hash`. That guard is the only thing standing between an invalid spec
and a partial matrix — a spec can DECODE into a workflow map and still fail
validation (`mode: auto` on the non-delegable `ordering` grooming class is the
fixture `TestRepoDelegation_InvalidSpecReturnsNamedErrorNotPartialMatrix` uses),
and `delegationview.Project` would happily emit a matrix for it. The declared
`version` is still read from the raw YAML independently of validation, exactly as
`projectPosture` reads it, so an invalid spec can still report what it claimed.

**Hash-before-filter.** `delegationview.Project` stamps each entry's
`content_hash`, THEN the optional `?workflow=` filter narrows the set, THEN the
view-level hash is recomputed over what was retained. So a filtered read returns
the same per-workflow hash an unfiltered one does, and a consumer binding a
confirmation to one workflow binds to that workflow's own hash.

**Seven refusals, one test each** (#1182): 400 `validation_failed` (unknown
`source`), 503 `github_unconfigured` (`source=ref`, no forge), 404
`workflow_spec_not_found` (three branches — no cached run under `run_cache`, a
forge 404 under `ref`, and a forge 200 whose spec content is EMPTY or
whitespace-only), 502 `forge_unavailable` (a NON-NotFound fault from either
`GetRepoInstallation` or `GetWorkflowSpec` — a forge fault is not "no
installation is visible", and the two are never collapsed), 422
`workflow_spec_invalid`, 404 `workflow_not_found` (unknown `?workflow=`), 403
`repo_forbidden` (the prelude's DENY). `delegation_view_test.go` asserts each
branch's status AND its code. `docs/api/v0.md` already documents the 502.

**Parity with the run-side block** is asserted, not assumed.
`TestDelegationView_MatchesRunDelegationBlockForEveryWorkflow` reads the
committed `.fishhawk/workflows.yaml` and, for EVERY workflow it declares,
compares the projection's `autonomy` / `matrix` / `must_page_human` against
`delegationPayloadFrom(delegation.NewEvaluator(…).Evaluate(…), major)` — the
exact pair `handleGetRun` uses. It iterates the parsed workflow map rather than a
hand-written list, so a workflow added to the spec is covered automatically.

## Precedent query (`precedent.go`, E75.3 / #3731, ADR-082 #3728 decision (b))

`GET /v0/precedent` returns prior decisions of ONE class from ONE repository,
ranked by `backend/internal/precedent`'s pure scorer. Read-only: it writes
nothing, mints NO audit entry, and grants no authority. Wire contract:
`docs/api/v0.md` § "Precedent query". Scoring contract:
`backend/internal/precedent/README.md`.

`Config.PrecedentIndex` is the one new field — a `List` + `GateContext`
interface `*decisionindex.Store` satisfies. Nil answers 503
`precedent_unconfigured` (the absent-not-disabled posture
`handleGetCalibration` uses), which makes dropping the `serve.go` wiring a
partial rollback that needs no revert.

The query body is `runPrecedentQuery`, shared with the gate-open precedent block
(§ "Precedent at the gate") — ONE derivation of the candidate window and the
account-scoped filter, so the two cannot disagree.

### Two input modes

- **Explicit context** — `repo` + `decision_class` required, plus `stage_kind`,
  repeatable `paths`, `concern_category` (run through
  `decisionindex.NormalizeConcernCategory`, so a raw reviewer spelling matches
  the canonical indexed value), `severity`, repeatable `escalation_keys`.
- **Gate reference** — `run_id` (+ optional `stage_id`). `PrecedentIndex.GateContext`
  derives the repository, stage kind, touched paths and fired escalation keys.
  `decision_class` stays REQUIRED: a gate does not determine which CLASS of
  decision the caller is asking about, and inferring one from the stage kind
  would be a guess the response could not explain.

An explicit field supplied alongside a gate reference OVERRIDES the derived
value, and `resolved_context` echoes what the ranking was performed against.

### Narrowing (binding condition 1 of the #3731 approval)

Three layers, and none of them substitutes for another.

**ACCOUNT, on the run read.** The gate-reference resolve is ACCOUNT-SCOPED at the
resolve, not afterwards: the caller's account travels into `GateRef`, so a run in
another account is INDISTINGUISHABLE from a nonexistent one — both, and a stage
that is not on the named run, answer the same 404 with NO `resolved_context`
echoed and the candidate window never queried.

**ACCOUNT, on the row set.** The candidate window is read with
`decisionindex.ListFilter.AccountScoped`, so an identity carrying NO workspace
account (an untenanted bearer token, the operator posture) matches only UNTENANTED rows —
the same stricter rule the gate resolve applies, rather than `ListFilter`'s
default nil-matches-all, which is the backfill's row-set predicate. Repository
visibility cannot stand in for this: two accounts can share a repository, so a
repo-only check would hand an account-less caller another tenant's decisions AND
their query-time reason prose. `TestPrecedent_AccountLessIdentityReadsOnlyUntenantedRows`
and `TestPrecedentPG_AccountLessIdentityReadsOnlyUntenantedRows` pin it with the
foreign row in the SAME repository and a distinct reason on the chain, so a leak
surfaces the prose and not merely the row's existence.

**REPOSITORY.** EVERY repository whose context reaches the response is subject to
the point-read repo-visibility DENY (`enforceRepoVisibility`, 403
`repo_forbidden`), because a precedent item carries a decision's reason prose.
That is the repository the ranking runs against AND, in gate-reference mode, the
repository the derived context came FROM — checked BEFORE a single derived key is
adopted. Checking only the former would let a caller pair a gate reference from a
repository they CANNOT read with an explicit `repo` they can, and receive the
source repository's derived touched paths and escalation keys inside
`resolved_context`; `TestPrecedent_GateReferenceSourceRepoMustBeVisible` denies
the derived repository while ALLOWING the explicit one, so the derived-repository
check is the only thing in that fixture's path.

`callerAccountUUID` FAILS CLOSED on a non-empty `Identity.AccountID` that is not
a UUID (500) rather than widening the read to every account: the field is written
from a sessions row, so an unparseable value is a corrupted invariant. Its nil
return (the untenanted posture) is safe only because every consumer pairs it with
`AccountScoped` or with `GateRef`, which is scoped by construction.

### Reason excerpts are read at query time

ADR-082 rule 1: the prose is never copied into the index. `attachReasonExcerpts`
reads each DISTINCT run's chain ONCE via `AuditRepo.ListForRun`, finds the entry
at `reason_sequence`, takes `payload[reason_key]` and caps it to 280 bytes with a
truncation marker. The read count is therefore bounded by `limit` (max 50, and an
over-max limit is REFUSED rather than clamped so the bound is real). Five failure
modes each yield NO excerpt plus a NAMED `degraded[]` reason — nil audit
repository, failed read, missing entry, missing key, non-string value — never a
fabricated excerpt and never a failed request. The degradation list is SORTED
because it is assembled from a map iteration, so two identical requests produce
byte-identical bodies.

### Bounds

The candidate window is the newest 500 matching rows (`Newest: true`); a FULL
window sets `truncated` and appends `window_truncated`. The echoed
`resolved_context` lists are capped at 20 entries each with their untruncated
totals, so the response size is a function of the ANSWER rather than of the
request (binding condition 2's server half). Note the PLATFORM ceiling behind
that: `net/url`'s own query-parameter limit is 10,000 and `r.URL.Query()`
swallows the resulting error, so a request carrying more parses to an EMPTY query
and the handler fails CLOSED on the now-missing `decision_class` (400).

## Precedent at the gate (`gate_precedent.go`, E75.4 / #3732, ADR-082 #3728 decision (c))

When a HUMAN gate is open, the gate view (`handleGetRunGateView`) and the
single-run read (`handleGetRun`) carry a bounded `precedent` block, both from the
ONE helper `gatePrecedentFor`, so the two surfaces cannot disagree. Wire
contract: `docs/api/v0.md` § "Precedent at the gate".

**Gate-open is read-time.** `awaiting_approval` / `awaiting_scope_decision` are
entered from a dozen sites, so there is no single transition seam to compute at;
the block is computed when a captain-facing surface READS a run whose gate is
open, and the fingerprint de-duplication makes "record once" hold across reads.

**The ladder** (`gatePrecedentClass`, pure, table-tested by
`TestGatePrecedentClass_Ladder`), first match wins:

1. any stage in `awaiting_scope_decision` → `scope_amendment` (hard stage-kind
   filter = that stage's type);
2. the plan stage in `awaiting_approval` → `plan_approval` (filter `plan`);
3. a review stage in `awaiting_approval` with ≥1 OPEN concern → `concern_waive`,
   ranked on the newest open concern's normalized category + severity, context
   derived from THAT CONCERN's stage (a waiver is recorded on the concern's
   stage, not the review stage), filter = the concern's stage kind;
4. a review stage in `awaiting_approval` with none → `merge_verdict`, with NO
   stage-kind filter (`merge_verdict_recorded` carries no stage, so its index
   rows have an empty stage kind);
5. otherwise no gate → no block.

When the concern read failed (run detail only), arms 3 and 4 are undecidable and
a review gate yields no block rather than a guessed class. `concern_defer` is the
acknowledged residual: one gate yields ONE class (Summarize's modal-outcome share
is defined over one class's outcome vocabulary), and `full_query` names
`alternate_decision_class: concern_defer` for the captain to re-run.

**Reuse, not duplication.** `runPrecedentQuery` (`precedent.go`) is THE query
body — account-scoped `ListFilter`, the newest-500 candidate window, `Rank`, the
window/no-rows degradations, and query-time reason excerpts — shared with
`GET /v0/precedent`. The gate context comes from `PrecedentIndex.GateContext`,
the same derivation the indexer used. The window is read in the RUN's own
account (`ListFilter.AccountScoped`), so an untenanted run reads only untenanted
rows (`TestGatePrecedent_AccountIsolated` seeds a tenanted and an untenanted
decision in the SAME repository and class).

Separately from the degrades below, the block is withheld by POLICY for a
run-bound `mcp:run:` caller (§ Isolation) — same nil, same no-audit-entry
outcome, before any read.

**Seven degrade modes, each nil + no audit entry** (each has its own test in
`gate_precedent_test.go`): `PrecedentIndex` unwired, the stage read fails, no
open gate, the run's account id is not a UUID, `GateContext` fails, the index
`List` fails, zero indexed rows. The response is then byte-identical to the
pre-E75.4 one (`TestPrecedentChangesNoGateOutcome` compares a wired and an
unwired server over the same gate).

**The chain write** (`recordPrecedentSurfaced`): ONE `precedent_surfaced` entry
(system actor, the gate's stage) carrying decision_class, stage_id, stage_kind,
`precedent.IndexVersion`, the fingerprint, `cited[]` (sequence, entry hash,
outcome, score components, matched keys), the summary and the degradation
reasons — NO reason prose (ADR-082 rule 1). De-duplicated by
`precedentAlreadySurfaced` on (fingerprint, stage), mirroring
`escalationAlreadyAudited` including its posture: the read-then-append is NOT
atomic (two concurrent first reads can both append — "exactly one" is a
SEQUENTIAL-read guarantee), and a de-dup READ failure emits anyway. An APPEND
failure is warn-logged; the block is still returned with a 200.

**Isolation.** Precedent is never a gate input: nothing in the approve/advance
path reads `gatePrecedentBlock` or `precedent_surfaced`.

It is never an agent input, and that guarantee is enforced at BOTH reachable
paths — not only at prompt render. (1) No prompt renders it
(`TestPrecedentNeverRendersIntoAnAgentPrompt`, the prompt-render absence pin).
(2) `gatePrecedentFor` returns nil for a run-bound `mcp:run:<uuid>` identity, so
NEITHER carrying surface hands an agent the block. That second half is the
point: the gate view authorizes a run-bound token by the cross-run subject guard
ALONE (see the gate-view auth bullet below — it may read its OWN run), and the
live block carries reason EXCERPTS from OTHER runs' human decisions, which is
exactly the cross-run prose keeping prose off the `precedent_surfaced` entry
(ADR-082 rule 1) already denies it. Under Fishhawk's code-execution threat model
an agent holding its run token could otherwise fetch its own gate view while its
gate is open and self-serve precedent. A run-bound read also records NO
`precedent_surfaced` entry — nothing was surfaced to a captain, so the read stays
pure — and it does not even reach the decision index. Pinned by
`TestGatePrecedent_RunBoundIdentityGetsNoBlock`, whose operator / cookie-session
/ anonymous arms are the discriminator: a blanket nil fails it.

`precedent_surfaced` is INTERNAL — not an issue-thread activity line.

## Divergence at the gate (`gate_divergence.go`, E75.5 / #3733, ADR-082 #3728 decision (d), rule 5)

After a captain's decision at an allow-listed gate is DURABLY recorded,
`noteGateDivergence` asks whether it went against clear precedent and, only if
so, appends ONE `precedent_divergence` entry — the record a captain-facing
surface turns into the optional "one-off, or a change of doctrine?" question.

**Shipped disabled.** `Config.DivergenceConfig` nil (the default) or
`Enabled: false` returns before anything is read: no index query, no entry, every
response byte-identical to before E75.5. fishhawkd builds it from
`FISHHAWKD_PRECEDENT_DIVERGENCE_{ENABLED,MIN_DECISIONS,MIN_AGREEMENT,WINDOW,CLASSES}`
(`divergenceConfigFromEnv`); every knob is parsed strictly and a malformed value
refuses startup rather than falling back.

**Call sites — each after the decision's own entry is on the chain:**

- `applyConcernWaive` (waive.go), after `ApplyResolution` — so the bulk waive
  inherits it through the shared body;
- `handleDeferConcern` (defer_concern.go), after the `concern_deferred` entry
  appends (a failed append skips the hook);
- `writeApprovalAudit` (approvals.go), after a successful `approval_submitted`
  append, for a REJECT on a PLAN stage only — the tail every reject path
  (legacy first-vote, quorum, escalation-unreadable) shares, so it can never ask
  about a decision that was not recorded.

**The evaluation.** Resolve the run's account and `GateContext` (repo, touched
paths, escalation keys, doctrine version = `runs.workflow_sha`), run the SAME
`runPrecedentQuery` the gate block and `GET /v0/precedent` use once per
`precedent.ComparisonClasses` member (waive and defer are compared against the
union of both), drop the decision's OWN index row (same run + source sequence),
and evaluate `precedent.Decide`. The pure rule is documented in
`backend/internal/precedent/README.md` § "Divergence threshold".

**The entry.** `precedent_divergence`, actor `system`, on the decision's stage:
decision class, stage, the decision's sequence, the captain's outcome, the modal
outcome, agreement ratio, human count, the plan reject's `reject_class` (recorded,
not ranked — `precedent.Context` carries no reject-class signal), the effective
threshold (N, X, window seconds, doctrine version), `precedent.IndexVersion`, and
up to 10 citations (`cited_total` reports the rest) by `source_sequence` +
`source_entry_hash` + score + matched keys ONLY — no reason prose (ADR-082 rule 1).

**Ask once per gate.** De-duplicated on (run, stage, decision class) by a
read-then-append over `ListForRunByCategory`. NOT atomic — two concurrent first
decisions can both append (the `recordPrecedentSurfaced` posture); "once per
gate" is a SEQUENTIAL guarantee. Unlike `recordPrecedentSurfaced`, a
de-duplication READ failure records nothing (fail toward quiet): a missed
question is benign, re-asking is the nagging the rule's brakes exist to prevent.

**Best-effort and total.** Every miss returns with no entry and leaves the
decision and its response untouched: disabled config, an allow-list miss, nil
`PrecedentIndex` / `AuditRepo` / `RunRepo`, a run read failure, a non-UUID
account id, a `GateContext` failure, an index `List` failure, zero candidates, a
below-threshold or agreed verdict, a de-duplication hit or read failure, and an
append failure. Pinned one test per mode in `gate_divergence_test.go`; the
cross-boundary proof (a real plan reject through `POST
/v0/stages/{id}/approvals` on Postgres, matched vs mismatched doctrine version,
and the default recording nothing) is `gate_divergence_pg_test.go`.

`precedent_divergence` and `precedent_divergence_answered` are INTERNAL —
chain-only, not issue-thread activity lines.

## Attention queue (`attention.go`, E40.1 / #1713)

`GET /v0/attention` (`handleListAttention`) is the cross-run "Needs You" read the SPA home page renders: one ranked list of every decision parked on a human. It is a pure PROJECTION — it writes nothing, mints no audit entry, persists nothing, and reuses the existing derivations rather than re-stating any gate rule.

- **Six kinds, one rank table.** `attentionPriority`: 1 `plan_gate` (plan stage in `awaiting_approval`), 2 `scope_amendment` (a `ScopeAmendmentRepo.ListByRun` row still `pending`), 3 `acceptance_disposition` (`acceptanceGateState == acceptanceGateTriage` — failed and unarbitrated), 4 `split_verdict` (a `buildGateView` open concern with `disputed=true`), 5 `paged_concern` (any other open concern), 6 `attend_human_led_campaign` (a campaign whose persisted state is `awaiting_human`). Sort is `(priority, since, id)` ASC — total and stable. `TestAttention_PriorityOrdering` pins it (a no-op touch of the table passes presence gates, not this test).
- **Concern items only at a parked review gate.** Open concerns contribute only when the run has an `implement`/`review` stage in `awaiting_approval`; `buildGateView` runs ONCE per such run and its `history_incomplete` becomes a `gate_view_history_incomplete` degraded entry (an unreadable history is never read as an empty one). A split carries the raiser's full `note` and the newest confirming re-review's note as `confirmation_note`.
- **Campaign signal is the persisted state.** `awaiting_human` is read from the row, not recomputed via `computeCampaignNextAction` per campaign — cheaper, and the conservative direction for a queue (a settled-but-not-yet-re-derived campaign shows once more; a parked one is never hidden). Human-led refs and the next-action detail come from `campaign.NextEligible` over the campaign's items; an items read error keeps the item and degrades `campaign_items_unreadable`.
- **Acceptance card carries the failing criteria.** An `acceptance_disposition` item names WHICH criteria failed and each one's decision-relevant explanation as `failed_criteria[]` — the criterion `id` plus the request (`method`, `path`, `status`) whose response the failing assertion evaluated — derived from the recorded outcome's `transcript` block (`acceptanceItem` filters `Outcome == "failed"`), so the disposition is understandable on the card without a detail fetch. It is empty when the outcome carries no agreeing transcript (a legacy verdict, or one whose transcript was suppressed for disagreeing with the verdict rows), and only failed criteria surface (passed/skipped are dropped).
- **Links.** `detail_path` is emitted server-side so the link rules live in one place: `/runs/{id}/stages/{stage}` for the four stage-anchored kinds, `/runs/{id}` for an acceptance disposition with no acceptance stage row, `/campaigns/{id}` for a campaign. **No kind anchors to an audit entry** — the issue's "or audit entry" phrase is intentionally not implemented (operator-ratified narrowing, #1713 approval condition 3).
- **Bounded, and completeness is visible for BOTH scans.** `attentionRunScanLimit` (100) caps the merged `pending` + `running` candidate set (two `ListRuns` calls — `ListRunsFilter.State` is a single string), oldest first; a bite sets `truncated`. `attentionCampaignScanLimit` (50) caps the `awaiting_human` campaign scan; a bite appends `campaign_scan_truncated` to `degraded[]`. `attentionGateViewBudget` (25) caps `buildGateView` calls; runs past it contribute no concern items and one `gate_view_budget_exhausted` entry. `?limit` cuts the ranked list and also sets `truncated`. A client must read `truncated || degraded.length > 0` as "incomplete" even with zero items.
- **Degradation, never silence.** Only `RunRepo` is required (nil → 503 `run_repo_unconfigured`; a `ListRuns` error → 500). Every other store is optional and every per-run read is independent: `concern_store_unconfigured`, `scope_amendment_store_unconfigured`, `campaign_store_unconfigured`, `stage_read_failed` (the run is skipped whole), `acceptance_state_unreadable`, `scope_amendment_read_failed`, `concern_read_failed`, `plan_summary_unavailable` / `plan_reviews_unreadable` (the plan-gate item is KEPT with thinner context), `campaign_read_failed`. The OpenAPI `AttentionDegraded.reason` enum mirrors the constants.
  - **A thinned contribution is degraded, never swallowed.** Two branches read a contribution AFTER a sibling gate already succeeded and would otherwise thin the item's context in silence: `planGateItem` cannot read the plan-review verdicts when `AuditRepo` is nil (→ `plan_reviews_unreadable`, `detail: audit store unconfigured`), and `acceptanceItem` re-reads the outcome that `acceptanceGateState` just read to build the card — a failure on that SECOND read leaves the card with no verdict/criteria/`failed_criteria` (→ `acceptance_state_unreadable`, naming the thinning). Both KEEP the item (the gate parked it; the operator must see it) and name the loss in `degraded[]`, so an incomplete card never reads as a complete one.
- **Narrowing mirrors `handleListRuns`.** `Identity.AccountID` bounds both the run and campaign scans; `requestRepoFilter` → `filter.allows` drops rows the caller cannot read; a filter error on ANY row — run or campaign — writes 503 `service_unavailable` for the whole request, never a partially-narrowed 200 (`TestAttention_RepoFilterErrorFailsClosed` seeds a row that WOULD be emitted before the erroring one).
- **Auth.** Items carry full reviewer prose, so a token needs `read:audit` (`scopeGateViewRead`, the gate-view posture); cookie sessions bypass scope checks per `requireWriteScope`. Registered as `GET /v0/attention` with the method prefix, so any other method is 405 (`TestAttention_RejectsNonGET`). No `requireRunAccount` wrapper — it is not run-scoped.
- **Wire golden.** `TestAttention_EndToEnd_AllSixKinds` drives the real mux and compares the indented body byte-for-byte against `testdata/wire/attention_list.json`, the same file the SPA's tests render; regenerate deliberately with `FISHHAWK_UPDATE_WIRE_GOLDEN=1` and update the frontend mirror in the same change.
- **Read cost.** Linear in candidates: one `ListStagesForRun`, one amendment list and (at a parked review gate) one `ListOpenByRun` + one `buildGateView` per run, plus the acceptance reads for runs whose workflow declares acceptance. A single-query aggregation would need new sqlc packages across four stores; the bounds above are the chosen alternative.

## Restart blockers (`restart_blockers.go`, E83.30 / #3974)

`GET /v0/restart-blockers` (`handleListRestartBlockers`) answers one question for `scripts/dev reload` / `post-merge`: would restarting THIS daemon orphan work right now? A pure read-only PROJECTION — it writes nothing, mints no audit entry, persists nothing. The shell guard that consumes it (`_refuse_on_restart_blockers`) is documented in `scripts/README.md` § "Restart-blocker guard".

- **Scan set.** The orphaned-review boot sweep's own `reconcileOrphanedReviewStates` (`pending` + `running`), one `ListRuns` per state with `restartBlockersRunScanLimit + 1` (500 + 1), merged oldest-first and cut at the cap; a bite sets `truncated` and the runs past the cap are NOT checked.
- **`undispatched_child`.** A run with `DecomposedFrom` set whose `implement` stage is `pending` or `awaiting_host_dispatch` AND whose decomposition PARENT run is not terminal (`run.State.IsTerminal()` false); carries `parent_run_id` + `stage_state`. A non-child's pending implement is not a blocker (`TestRestartBlockers_NonChildPendingImplement_NotBlocker`). A child of a `succeeded` / `failed` / `cancelled` parent is not a blocker either (#4184): it can never be dispatched by that parent's fan-out, so a restart strands nothing — reporting it wedged `scripts/dev post-merge` on 41 stale rows. This stays defence in depth after the #4186 cancel cascade (below): the cascade fires only on CANCEL sinks, so the undispatched children of a FAILED or SUCCEEDED parent still reach this skip, as do children orphaned before `fishhawkd reconcile-orphan-children --apply` runs. The parent is read through `restartBlockerParent`, a per-request memo (one `GetRun` per distinct parent, the error cached too). Tests: `TestRestartBlockers_TerminalParentChild_NotBlocker` (cancelled / failed / succeeded → no item; pending / running parents are the control arms), `TestRestartBlockers_IncidentShape_TerminalParents_NoItems` (the #4184 shape through the real mux), `TestRestartBlockers_ParentReadMemoized`.
- **Decomposition-child cancel cascade (#4186, `decomposition_cancel_cascade.go`).** Every post-admission run-cancel sink calls `cascadeCancelToDecomposedChildren` immediately after `recordAcceptanceRetirementsDroppedOnCancel`, with the same `cancel_source`: `handleCancelRun` (`operator_cancel`, synchronously BEFORE the 200 so a caller reading the children afterwards sees them cancelled, and on the idempotent already-cancelled re-entry too, so re-POSTing cancel converges a child a transient error left behind), `checkRunBudget` (`run_budget_exceeded`), `checkStageBudget`'s blocking arm (`stage_budget_exceeded`) and `OnRunCancelled` (`stage_cancelled`, `orchestrator.completeRun`, which covers the PR-closed-without-merge path). `applies_to.go`'s `abandonUnauditedOverrideRun` stays unwired: it cancels a run in the request that created it, which cannot have children yet. The helper re-reads the parent and calls `childcancel.CascadeFromParent` (contract: `backend/internal/childcancel/README.md`): each non-terminal child is cancelled through the state machine with ONE system-actor `decomposition_child_cancelled` row on its chain, deduped per `parent_run_id`; terminal children are skipped with no row; a child whose implement is `dispatched`/`running` is CANCELLED, not refused (its stage and runner are untouched, `live_stage: true`). Best-effort: a parent re-read or child-list failure WARN-logs and cancels nothing, a per-child transition or append failure WARN-logs that child, and nothing changes the caller's response; one INFO summary names `children` / `cancelled` / `skipped_terminal` / `failed` / `live_stage`. With every child of the cancelled parent terminal, the child-completion sweeper settles the parent's `awaiting_children` stage failed-C (see `backend/internal/childcompletion/README.md`). Tests: `decomposition_cancel_cascade_test.go` (every sink, the skip, the live-runner arm, re-entry, the restart-blockers `scanned_runs` drop, each failure mode, both dedup legs).
- **`review_in_flight` — a round a restart would LOSE (narrowed by E72.59 / #4077).** For each `orphanedReviewStages` kind: the latest `*_review_started` round (`latestReviewStarted`) with verdicts landed strictly after it (`countLandedReviewTerminals`) `< ConfiguredAgents`, and then EITHER (a) the round is in this process's pending-redispatch set (`reviewRedispatchPending`: an orphaned round this process's boot sweep handed to a re-dispatch goroutine that has not settled — checked BEFORE the boot-marker skip, because that orphaned round predates `s.processStart`; a restart kills the goroutine and the next boot finds the round already named by its `review_round_redispatched` entry, so it closes it failed), OR (b) the started entry is NOT before `s.processStart` AND `redispatchEligibility` says the next boot would NOT re-dispatch it (a gating round, no reviewer backend, the depth cap, an unknown implement round source, a superseded round, a plan stage no longer `awaiting_approval`, an implement stage `failed` / `cancelled` / `superseded` (`implement_stage_terminal`) or a later review stage already terminal (`advanced_past_review`), an unwired artifact/trace/forge source). An ELIGIBLE current-process round is not a blocker: the next boot re-dispatches it against the same plan or head. Both the round tally and the eligibility decision are the boot sweep's own helpers (§ "Boot re-dispatch of orphaned review rounds"), so the two surfaces cannot disagree about which rounds survive a restart. A round an EARLIER process dispatched and this process did not take over is not a blocker — its goroutine is already dead and this process's boot sweep already re-dispatched or closed it (`TestRestartBlockers_PriorProcessOrphan_NotBlocker`). A zero-reviewer round is settled at zero verdicts. Carries `configured_agents` + `landed` (pointers, so a real `landed: 0` is emitted). The eligibility checks are CHEAP (audit + stage reads and wiring, never input loading), so a round reported re-dispatchable can still fail its input rebuild at boot (a deleted artifact) and then close failed — the TOCTOU residual. Tests: `TestRestartBlockers_AdvisoryRound` (eligible → not a blocker; unwired → blocker; the fixture wires a reviewer, an `awaiting_approval` plan stage row and an `ArtifactRepo` and asserts `redispatchEligibility` true FIRST, approval condition C3), `TestRestartBlockers_IneligibleWiredRoundIsBlocker` (gating, depth cap, the under-cap control), `TestRestartBlockers_ImplementRoundOnTerminalStage` (an advisory implement round on a `failed` / `cancelled` implement stage is a blocker; the `succeeded` control, asserted eligible first, is not; #4174), `TestRestartBlockers_PendingRedispatchIsBlocker`.
- **`check_failed`.** A per-run `ListStagesForRun`, decomposition-parent `GetRun` (including a parent that is not found: `decomposed_from` is `ON DELETE SET NULL`, so that is reachable only across a concurrent delete — `TestRestartBlockers_CheckFailed/parent_read`, `/parent_not_found`) or audit read error — including any read `redispatchEligibility` needs (`TestRestartBlockers_CheckFailed/eligibility_read`) — yields an item naming the run and the stage whose check failed, never a silent skip — the daemon is reachable but cannot decide, and the shell guard REFUSES on it (fail-closed).
- **No free text.** Every item value is an id, an enum slug or a count, so `scripts/dev` parses it with zsh `=~` and no JSON decoder; sorted `(run_id, reason, stage)`.
- **Auth and narrowing mirror `handleListRuns`.** Anonymous accepted (no scope check; `scripts/dev` sends no token), `Identity.AccountID` bounds the scan, `requestRepoFilter` → `filter.allows` drops unreadable repos, a filter fault is 503 `service_unavailable` for the whole request. The exposure is a subset of what anonymous `GET /v0/runs` and `GET /v0/runs/{run_id}/stages` already serve. Nil `RunRepo` / `AuditRepo` → 503 `run_repo_unconfigured` / `audit_repo_unconfigured`; a `ListRuns` error → 500.
- **Wire golden.** `TestRestartBlockers_EndToEnd_Golden` drives the real mux with NO credential and compares the RAW body to `testdata/wire/restart_blockers.json`; `scripts/test-dev` RB-k feeds the same file to `_parse_restart_blockers`. Its review seed is a GATING round (#4077), so the item survives the narrowed predicate regardless of the fixture's reviewer wiring, and its decomposition parent (`attnID(2)`) is seeded LIVE (`running`) so the parent-state filter (#4184) keeps the `undispatched_child` item — the golden stayed byte-identical; the wire shape and both enums are unchanged. Regenerate with `FISHHAWK_UPDATE_WIRE_GOLDEN=1` and re-run `scripts/test-dev` in the same change.
- **Read cost.** Per run: up to one `ListStagesForRun` (children only), plus one memoized `GetRun` per DISTINCT decomposition parent of a child with a `pending` / `awaiting_host_dispatch` implement stage (#4184), plus, per review stage, one started read and three terminal reads, and for an unsettled current-process round the eligibility reads (one `review_round_redispatched` list, one superseding-category list, and a `GetStage` or `GetRun`). It runs once per `reload` / `post-merge`, and once per poll of `scripts/dev reload --when-quiet` (default every 15 s).
- **`merge_in_flight` (ADR-093 D8 / [#4298](https://github.com/kuhlman-labs/fishhawk/issues/4298)), stage `merge`.** Check (c): `s.mergeInFlight.has(run)` — THIS process has the run's head-of-line merge-queue merge in flight (marked after the `merging` stamp, cleared by the pump after the outcome is applied, § "Merge-on-pass"). A restart interrupts the forge merge call, so the next boot must reconcile it and may hold it for the operator re-arm. Process-local with no DB read, so it never yields `check_failed`. A `merging` entry a PRIOR process left is NOT a blocker: the next boot reconciles it and a restart loses nothing more (the review-round rule's shape). Tests: `TestRestartBlockers_MergeInFlight`, `TestRestartBlockers_LiveQueueMergeListed_EndToEnd` (real queue, the pinned merger probing the route mid-call).
- **Consumer contexts.** `scripts/dev` reads `undispatched_child` only in the `post-merge` context (its `git pull` is what strands the child); `reload` ignores it. The endpoint itself reports every item regardless of caller (`scripts/README.md` § "Restart-blocker guard").

## Repo dashboard rollups (`repodash.go`, E40.3 / #1714)

`GET /v0/repos/{owner}/{name}/{throughput,health,economics,posture}` feed the SPA's `/repos/:owner/:name` page. Read-only projections: nothing is written, no audit entry is minted. The folds are the pure `backend/internal/repodash` package; this file gathers rows, decodes audit payloads into `repodash.Event`/`CostEntry`, and maps results onto json-tagged response structs.

- **Prelude (`repoDashPrelude`).** Order: 503 `run_repo_unconfigured` (and, for the three rollups, `audit_repo_unconfigured`) → `requestRepoFilter` + `repoVisibleOr403` (the #2071 point-read DENY: 403 `repo_forbidden`, 503 `service_unavailable` on a store fault) → the bounded `weeks` parameter (1–52, default 12; anything else 400 `validation_failed` with `details.field: weeks`). `posture` takes no `weeks`. No new auth scope — same read posture as `GET /v0/calibration`. `repoDashPrelude` has a FIFTH caller outside this section: the delegation read (`delegation_view.go`, E76.1 / #3747) reuses it with `needAudit=false, needWeeks=false` so its visibility DENY is byte-for-byte the one here.
- **Window scan (`scanRepoRuns`).** Pages `RunRepo.ListRuns` (Repo + the caller's `AccountID`, newest first, `repoDashPageSize` 200) until a row is older than `window_start − repodash.LookBack` (14 days — keeps a run created before the window but merged inside it) or a short page ends the listing, under the `repodash.MaxRunsScanned` (1000) ceiling. When the ceiling stops the scan first, a one-row probe decides `truncated` (so exactly-ceiling runs is complete); `truncated` is carried on every rollup, including the economics empty body (`{"truncated":true}`).
- **Per-run reads.** One `AuditRepo.ListForRun` per gathered run (plus `ListStagesForRun` for `health`, which resolves `approval_submitted` stage types and failure categories) — the N+1-acceptable-for-v0 posture `filterRuntimeObservedSamples` documents, bounded by the ceiling. `wait_on_human` is folded from the already-loaded chain through the same `gateEventCategory` / `latestTerminalTimestamp` / `latency.AggregateGateLatency` path as `runLatencySummary`, without a second read; the key is ABSENT when no run resolved.
- **Economics.** Cost from `cost_recorded` payloads (`usd`, `model`, token split, `source`); `{}` when the window holds none (the `/cost` presence convention). Budget burn evaluates the FIRST periodic budget of each workflow in the newest run's cached spec through `effectiveBudgetLimit` + `evaluateWorkflowBudget` + `budget.Tier` — the `/budget` chain — and is omitted when the run repo is not a `runCostSummer`.
- **Posture.** Newest run's cached `WorkflowSpec` (`{}` for no run or empty bytes). `version` is read from the raw YAML independently of validation so an unsupported or invalid spec still reports it; `schema_supported`/`schema_hash` come from `embeddedWorkflowSchemaHashes` (the three accessors `/healthz` advertises — a new workflow MAJOR must add its row here too); `spec_valid`/`spec_error` from `spec.ParseBytes`. There is no hash-equality check against `/healthz`: both come from the same binary, so it could never fire (approval condition 4 of #1714).
- **Wire goldens.** `TestRepoDash_WireGoldens` compares each endpoint's indented body against `testdata/wire/repodash_{throughput,health,economics,posture}.json`, the files the SPA's panel tests render; regenerate with `FISHHAWK_UPDATE_WIRE_GOLDEN=1` and update the frontend in the same change. The posture golden embeds the real workflow-v0 schema hash, so a v0 schema edit regenerates it.

## "Since you last looked" digest (`digest.go`, E75.6 / [#3734](https://github.com/kuhlman-labs/fishhawk/issues/3734))

The REST surface over `backend/internal/digest` (ADR-082 #3728 rule 7). The computation, sections, gap kinds, the parking-entry and paired-`answered` mappings and the bound are that package's contract (`backend/internal/digest/README.md`); this section is the HANDLER contract.

### `GET /v0/digest?repo=&section=&from_sequence=&to_sequence=`

- **Auth.** `requireWriteScope(read:audit)` — the digest is a projection of the chain, so it takes the chain's read scope (the gate-view precedent, `scopeGateViewRead`); 401 anonymous, 403 `insufficient_scope` on a token without it; cookie sessions are not scope-checked. The watermark is keyed by a subject + repo + `identityAccountID(ctx)`, so two subjects' digests are independent (`TestWatermark_TwoCaptainsAreIndependent` asserts B's body is byte-identical across A's mark).
- **Whose watermark (E76.3 / #3766, ADR-083 rule 6).** `resolveDigestReadKey` keys GET on the repository's SEATED captain (via `currentCaptain`), falling back to `IdentityFrom(ctx).Subject` when the seat is vacant or the captain record is unwired/unreadable. An optional read-only `captain_subject` query parameter (non-empty, ≤ `digestCaptainSubjectMaxBytes` = 256, else 400 `validation_failed` naming the field) overrides the key. The response wraps `digest.Digest` with `captain_subject_basis`: `captain` | `caller` (override naming the caller) | `caller_vacant` | `caller_unavailable` | `explicit` (override naming neither). The handler bounds at `digest.DefaultByteBudget - digestBasisOverhead(basis)` so the WHOLE body stays inside the one budget (`TestDigestBasisOverhead_MatchesWireSize`). Tests: `TestDigestCaptain_*` in `digest_pg_test.go`, real `captain.Store`, one per basis.
- **Request shape, before any read.** `repo` required; `section` through `digest.ParseSection` (the four content sections + `gaps`); both sequences non-negative integers. Each refusal is 400 `validation_failed` with `details.field` naming the parameter — the field is what distinguishes the handler guard from `digest.Build`'s own validation, which answers 400 without one.
- **Repo visibility.** `enforceRepoVisibility` → 403 `repo_forbidden` / 503 `service_unavailable` (the #2071 point-read deny), after the shape checks and before any query.
- **Never writes.** `digest.Build` then `digest.Bound(d, digest.DefaultByteBudget)` — the ONE bound the `fishhawk_digest` MCP tool also applies. No watermark move and no audit entry, however many times it is called (`TestGetDigest_DoesNotAdvanceWatermark`).
- **Errors.** `*digest.BeyondChainHeadError` → 400 `to_sequence_beyond_chain_head` with `details.requested` + `details.chain_head`; `digest.ErrInvalidRequest` → 400 `validation_failed`; anything else (a read failure, `digest.ErrBudgetTooSmall`) → 500 `internal_error`.

### `POST /v0/digest/mark-read` `{"repo","to_sequence"}`

- **Auth.** `requireWriteScope(write:approvals)` — the EXISTING scope (no new one, the E34.2 / #1595 precedent), so the auth-change impact inventory is empty by construction. Pinned by `TestMarkRead_AuthenticatedWithoutScopeIs403` (a token whose scopes are exactly `[read:runs]`) and `TestMarkRead_AnonymousIs401`; neither writes a row or an entry.
- **Body.** Strict decode (`DisallowUnknownFields`, 4 KiB `MaxBytesReader`); `repo` required and `to_sequence` positive, each a 400 naming its field. There is deliberately NO `captain_subject`: the strict decoder refuses one with 400 (`TestDigestCaptain_MarkReadRefusesCaptainSubject`), so a write can never be aimed at another subject's key.
- **Whose watermark (E76.3 / #3766, maintainer decision).** `resolveDigestMarkKey` ALWAYS advances the CALLER's own watermark: only the seated captain advances the captain's (for them the two keys are the same row), and a non-captain's mark moves their own key, never the seat's (`TestDigestCaptain_NonCaptainMarkReadAdvancesOwnKey` reads both stored rows after the call). The response and the chain entry carry `captain_subject_basis` (`captain` when the caller is seated, else `caller` / `caller_vacant` / `caller_unavailable`) and the entry records `marked_by`.
- **Ordering is the package's.** `digest.Store.MarkRead` resolves the watermark (at-or-below → 200 `advanced: false`, NOTHING appended), refuses a `to_sequence` above the repo's chain head (400 naming it), appends through the injected appender, and upserts ONLY after the append succeeds. This file supplies the appender: `appendDigestMarkedRead` writes one `digest_marked_read` entry via `AuditRepo.AppendGlobalChained` — the GLOBAL chain, partitioned by the caller's account, actor `user` + the marking subject (`marked_by`), payload `repo` / `captain_subject` / `marked_by` / `captain_subject_basis` / `previous_sequence` / `had_previous` / `to_sequence`. An append failure answers 500 with the watermark unmoved (`TestMarkRead_AppendFailureLeavesWatermarkUnmoved` reads the stored row after the call).
- **Not repo-visibility filtered.** Writes are never decided by the non-authoritative forge-visibility mirror (#2071); the mark moves only the caller's own watermark.
- **Not an issue-comment surface.** `digest_marked_read` belongs to no run, so it is deliberately absent from `issuecomment`'s `activityCategories` and from `docs/issue-comment-surfaces.md`.

### Wiring and degradation

`Config.DigestStore` (`*digest.Store`) and `Config.DigestIndex` (`digest.IndexReader`, satisfied by `*decisionindex.Store`) are wired in `backend/cmd/fishhawkd/serve.go` on the pool, beside the audit repository whose decorator projects into the same `decision_index`. Either nil — a DB-less boot — degrades BOTH routes to 501 `digest_unconfigured`; mark-read additionally requires `AuditRepo`. The message names each missing collaborator (`digest_store`, `decision_index`, `audit_repository`) because `writeError` redacts non-allow-listed 5xx detail keys.

### Tests

`digest_test.go` (no DB): route registration, both 501s, anonymous GET, every request-shape guard, and `TestOpenAPI_DigestRoutesDocumented`. `digest_pg_test.go` (pgtest, the production audit wiring — the Postgres repository wrapped by `decisionindex.NewIndexingRepository`): `TestDigestEndToEnd_WatermarkRoundTrip` (a merged run + a waived concern surface citing real chain entries; mark-read lands `digest_marked_read` on the global chain; the re-read drops the read items), plus one test per handler-visible failure mode (beyond-head on both routes, at-or-below no-op by ENTRY COUNT, append failure by stored state, 401/403, captain independence, section selector + bound).

## Delegation confirmation (`delegation_confirm.go`, E76.5 / [#3768](https://github.com/kuhlman-labs/fishhawk/issues/3768))

ADR-083 #3751 rule 7: the incoming captain confirms or lowers each workflow's
delegation; nothing raises it. Three routes under the delegation read, all
behind `repoDashPrelude` and the same `source=ref|run_cache` contract
(`loadDelegationWorkflows` reuses `delegationSpecFromRef` /
`delegationSpecFromRunCache` and refuses an invalid spec 422):

| Route | Scope | Effect |
|---|---|---|
| `GET …/delegation/confirmation` | read (prelude only) | every workflow of the VIEW with its verdict; `unconfirmed_workflows` includes never-confirmed ones |
| `POST …/delegation/confirm` `{workflow, content_hash, source?, ref?}` | `write:approvals` | appends ONE `delegation_confirmed` binding the exact hash shown |
| `POST …/delegation/lower` `{workflow, proposed_tier?, proposed_escalation?{paths,max_autonomy}, reason, source?, ref?, parent_epic?, title_vars?, labels?}` | `write:approvals` | files ONE `autonomy:low` chore carrying the proposed `.fishhawk/workflows.yaml` edit, then appends ONE `delegation_lower_proposed` naming it |

**Verdict.** `delegationconfirm.Statuses` is CHAIN-ONLY (`confirmed`,
`unconfirmed`/`handover`, or `no_captain` when no seat was ever taken);
`ApplyCurrentHashes` adds `unconfirmed`/`hash_stale` when the recorded hash
differs from the current `delegationview` hash. Staleness appears only on these
delegation reads; `GET /v0/captain` stays chain-only. A lower proposal does not
confirm — the workflow stays unconfirmed until the human-authored edit lands and
is confirmed.

**Refusals, each its own code** (`delegationConfirmRefusals`, pinned distinct):
agent / `mcp:run:` / `delegated:true` → 403 `delegation_agent_identity_refused`
(`delegationconfirm.GuardActor`, the single site, before any spec read); not the
captain → 403 `delegation_not_captain`; vacant seat → 409
`delegation_no_captain`; unknown workflow → 404 `workflow_not_found`; hash no
longer current → 409 `delegation_hash_stale` (nothing appended); lower not
strictly lower → 400 `delegation_raise_refused`; lowers nothing → 400
`delegation_nothing_proposed`; empty reason / missing fields → 400
`validation_failed`; nil store → 501 `delegation_confirm_unconfigured`.

**Serialization.** `delegationconfirm.Store.Append` takes the CAPTAIN RECORD's
advisory lock (same key as `captain.Store.Apply`) and runs the captain check on
state read inside that transaction, so an outgoing captain's confirm cannot
commit after a handover. The fold additionally ignores any entry whose actor was
not the captain in force at its chain position. The lower path pre-checks the
captain BEFORE filing (a refused proposal files nothing), files through the
run-absent operator path (`conventionsLoader` → `resolveRepoScope` →
`applyAndFileWorkItem`), dials no repository write seam, strips caller
`autonomy:*` labels in favour of `autonomy:low`, and appends nothing when the
filing fails. A handover landing between the filing and the append refuses the
append and names the orphaned `filed_ref` in the error details.

## Captain record (`captain.go`, E76.2 / [#3765](https://github.com/kuhlman-labs/fishhawk/issues/3765))

The REST surface over `backend/internal/captain` (ADR-083 #3751 rules 2-5). The event model, the `Derive` fold, the five transitions with one sentinel per refusal mode, and `Store.Apply`'s advisory-locked read -> derive -> validate -> append transaction are that package's contract (`backend/internal/captain/README.md`); this section is the HANDLER contract.

- **Routes.** `GET /v0/captain?repo=` (`scopeCaptainRead` = `read:audit`, repo-visibility enforced, never writes) and `POST /v0/captain/{offer,withdraw,accept,relinquish,claim}` (`scopeCaptainWrite` = the EXISTING `write:approvals`, so no token loses access; writes are not visibility-filtered, per #2071). A nil `Config.CaptainStore` degrades all six to 501 `captain_unconfigured` (`TestCaptain_UnconfiguredReturns501`).
- **Request.** `{repo, successor?, delegated?}`, `DisallowUnknownFields`, 4 KiB bound. `successor` on any verb but `offer` is a 400 rather than silently ignored.
- **ONE authoritative refusal site per control.** The handler COMPUTES `ActorIsAgent` (`captainActorIsAgent`: the operator-agent token family via `actorKindForSubject`, or a run-bound `mcp:run:` subject) and `ActorIsDelegated` (the body's `delegated`) and hands them to the transition; only `captain.guardActor` refuses. The predicate refusal likewise lives only in `captain.Claim`. `TestCaptain_AgentSubjectRefusedOnEveryVerb` drives all five verbs through the real handlers in the one state per verb where the agent would otherwise succeed, and reads the chain + GET body back.
- **One code per refusal** (`captainRefusals`, pinned distinct by `TestCaptainErrorStatus_EachRefusalDistinct`): 401 `authentication_required` (empty actor); 403 `captain_agent_identity_refused` / `captain_not_captain` / `captain_not_offerer` / `captain_offer_successor_mismatch` / `captain_predicate_rejected`; 409 `captain_no_captain` / `captain_no_offer` / `captain_exists` / `captain_self_handover`; 400 `captain_successor_required` / `validation_failed`; 422 `captain_predicate_undeterminable`; anything else 500 `internal_error`. A claim refusal carries `details.predicate_basis`.
- **`classifyRepoPredicate` — a trichotomy that FAILS CLOSED.** Reads the spec cached on the repo's NEWEST run (`ListRuns` Limit 1, account-narrowed — the `handleGetRepoPosture` precedent; never a live forge fetch). UNDETERMINABLE (→ 422): no run repository, a list error, no run, no cached spec, an unparseable spec, a legacy v0/v1 `approvers`-only gate (a predicate this path cannot evaluate), or `resolvePredicates` answering unavailable/unconfigured. Otherwise `foldRepoApprovals` folds every approval gate of every workflow through `effectiveApprovals` (max count, strictest `min_permission`, union of `member_of`, sorted; the parsed spec is never mutated): neither dimension declared → TRIVIAL (`trivial:any-non-agent-token-holder`, `claim_verified: false`); otherwise `resolvePredicates` — the approval quorum's own forge seam — decides satisfied (`claim_verified: true`) or rejected. Residual: a repo whose `.fishhawk/workflows.yaml` changed since its last run is judged against the older cached spec; a repo that has never run cannot be claimed.
- **Read shape.** `claim_verified` is rendered from the record's OWN field, never from `identity_verified` (`TestCaptain_ReadRendersClaimVerifiedSeparately` uses the one state where they disagree). `history` is the newest `captainHistoryLimit` (50) entries, ascending, with `history_total`.
- **Offer records the handover brief (E76.4 / #3767).** `handleCaptainVerb`'s offer branch calls `composeOfferBrief` BEFORE `Store.Apply` (composition does I/O; Apply's decide callback runs under the advisory lock) and passes the result as `captain.Params.Brief`: the canonical brief's `brief_hash` + window, or — only when `handoverbrief.Compose` cannot ESTABLISH the brief (captain record / window read failure, or a nil digest store/index: `dependency_unconfigured`) — `brief_unavailable` + the machine-readable reason and NO hash. FAIL-OPEN WITH A MARKER: the offer still commits, because refusing a handover over a read-only projection strands the seat. A section read error is only a degradation and the offer still carries a hash (`TestCaptainOffer_DigestSectionErrorStillHashes`). Pinned end to end by `TestCaptainOffer_StampsCanonicalBriefHash` (committed payload hash == the canonical brief's, and a truncating `Bound` render reports the same hash) and `TestCaptainOffer_FailOpenMarksBriefUnavailable` (a failing chain-head read, asserted on the COMMITTED payload). TOCTOU, stated: the chain can advance between the compose and the append, so the hash describes what was composed at or just before the offer's own sequence.
- **Non-interference.** `TestCaptain_SeatedCaptainDoesNotAffectApproval` seats a captain on a real store and asserts an approval by a different subject yields the same status, stage state and `approval_submitted` payload keys as with no captain record; the approval/quorum test files are unedited.

## Crew messages (`crewmessage.go`, E77.3 / [#3737](https://github.com/kuhlman-labs/fishhawk/issues/3737))

The REST surface over `backend/internal/crewmessage`'s chain-authoritative `Mailbox` (ADR-081 #3727 D4, rules 3-4). The document contract, the directional rules, the chain-first send/dispose and the round bound are that package's contract (`backend/internal/crewmessage/README.md`); this is the HANDLER contract.

- **Routes + wiring.** `POST /v0/crew-messages` (send), `GET /v0/crew-messages` (list), `GET /v0/crew-messages/{sequence}` (get one, opt-in `?wait=`), `POST .../{sequence}/respond`, `POST .../{sequence}/escalation-decision`, `POST .../{sequence}/convert-to-concern` (E77.7, below). A top-level collection, not run-scoped: the anchor is a FIELD of the document (one of `run_id` / `issue_ref` / `decision_record_id`), and the two run-less anchors cannot be expressed under a run path. `Config.CrewMailbox` is wired in `serve.go`'s pool block; dropping that ONE line is the partial rollback — every route but respond answers 503 `crew_message_unconfigured` (`crewConfigured` also requires `RunRepo` + `AuditRepo`) and nothing else changes (`TestCrewMessageAPI_Unconfigured503`).
- **The scope.** `write:messages` is minted by `handleIssueMCPToken` through `stageTypeMayMessage` — a POSITIVE allow-list over plan and review, so implement (invariant #8), deploy, acceptance and any future stage type are denied by default (`TestHandleIssueMCPToken_WriteMessagesGrantTable`). It is RUN-BOUND only: nothing is added to the operator default scopes or the OAuth vocabulary, so no existing token gains or loses access.
- **Sender role — one rule.** Derived, never asserted: a run-bound token's executing stage via `resolveExecutingStageType` mapped by `crewSenderRoleForStage` (plan -> `planner`, review -> `reviewer`, anything else -> 403 `crew_sender_not_derivable`, so a token that outlived its stage cannot send as the next one), or `captain` for any other identity (which needs `write:stages`). `crewmessage.WithSenderRole` then refuses a body naming a DIFFERENT role (400 `sender_role_not_settable`, no chain entry — `TestCrewMessageAPI_SenderRoleMismatchRefusedNoChainEntry`) and injects it when absent, after walking the caller's RAW bytes for duplicate members.
- **Send binding.** A run-bound token needs `write:messages` and its own run as the PARSED `anchor.run_id` (read after `WithSenderRole` + `Parse`, so a duplicate anchor is refused before the check could see one interpretation); another run and every run-less anchor -> 403 `cross_run_crew_message`. An operator's run anchor goes through `enforceAccount(memberWrite)`; a run-less anchor chains on the caller's account partition. A committed-but-unprojected send is 201 with `projection_degraded: true` — the send HAPPENED; do not retry.
- **Read gate (condition 1 of the approval; identical on get, `?wait`, list).** `authorizeCrewRead`: a run-bound token must hold `write:messages` (so an implement token is 403 `insufficient_scope`, checked BEFORE any read on get and list) AND the row's `run_id` must be its own run — another run's message and every run-less one are 403 `cross_run_crew_message`, and a recipient listing (which spans anchors) is refused outright. Any other identity needs `read:audit` and `crewAccountVisible` (the audit list's NULL-allow window); another account's row is 404 `crew_message_not_found`. `TestCrewMessageAPI_ReadGate` reads a message on the implement token's OWN run, so the scope gate is the only refusal in its path.
- **Rendered form only.** No response carries the payload. GET renders through `prompt.RenderCrewMessages` — the ONE exported form a crew message's text leaves the prompt package in; this package never assembles an envelope. The text is resolved from the `crew_message_sent` CHAIN entry (the row holds none) whose `entry_hash` must equal the row's `sent_entry_hash`: `ListForRunByCategory` for a run anchor, `ListAll{Category}` for a run-less one. `crewMessageForPrompt` puts every payload field into `MessageText` (quarantined) and only contract-closed metadata elsewhere. `TestCrewMessageAPI_SendThenGetReturnsRenderedForm` carries a column-0 heading and a forged BEGIN delimiter and asserts one balanced envelope with the text only inside it.
- **`?wait=`.** `parseCrewMessageWaitSeconds` clamps to `maxCrewMessageWaitSeconds` (30); absent/non-positive is a single read. The loop re-reads every `crewMessageWaitPollInterval` until the row leaves `open` or `crewThreadReply` finds a later message in the thread (`answered: true`, `answer.rendered`); on expiry the unchanged read returns. A run-bound `response_required` consult is answered by the in-process responder (below), whose threaded reply completes the wait with `answered: true`; an expired consult leaves `state: expired` with `answered: false` — the agent's "no answer" signal, never a refusal.
- **Respond = refusal ladder.** 403 `agent_token_required` (run-bound) then 403 `responder_required` (everything else): no HTTP caller is a fishhawkd-invoked responder. `(*Server).RespondToCrewMessage` is the in-process entry point `runCrewConsult` (E77.5) calls — a threaded reply (sender derived from `ResponderRole` through the same `WithSenderRole`) then an `accepted` `Dispose`; the two entries are sequential, not one transaction. Widening to an HTTP responder identity happens at the `responder_required` rung only. **It REFUSES an escalation** (`ErrInvalidDisposition`, E77.6 / [#3740](https://github.com/kuhlman-labs/fishhawk/issues/3740)) — see the trust-boundary bullet below.
- **Synchronous consult (`crew_consult.go`, E77.5 / [#3739](https://github.com/kuhlman-labs/fishhawk/issues/3739)).** A run-bound token's `consult` with `response_required: true` takes a branch in `handleSendCrewMessage` strictly AFTER `WithSenderRole` + `Parse` + the anchor binding and strictly BEFORE `Mailbox.Send`, so both refusals append NOTHING: the executing stage row (`resolveExecutingStage`, the same active-or-next fallback) → 422 `crew_consult_budget_exhausted` `{max, used}` when `crewConsultBudgetUsed >= maxCrewConsultsPerStage` → 422 `crew_responder_unavailable` `{recipient_role}` when `Config.CrewResponders.Lookup` misses (an `implementer` recipient never gets here: `Parse` already refused it 422 `recipient_not_addressable`). The send then carries `SendParams.StageID`, and the 201 adds `consult_deadline` + `consult_budget_remaining` (both `omitempty`; every other response is byte-identical to E77.3's). An operator consult and a consult WITHOUT `response_required` take no branch — no cap, no stamp, no responder.
  - **The registry is CLOSED.** `NewCrewResponderRegistry` refuses an `implementer`, a non-crew role or a nil responder at wiring time (the second gate behind `crewmessage.CanReceive`) and copies the map. **`serve.go` wires exactly ONE responder** since E77.8 ([#3742](https://github.com/kuhlman-labs/fishhawk/issues/3742)): the deterministic historian under `crewmessage.RoleHistorian` (ADR-081 rule 8), built by the `crewResponderRegistry` helper so the SHIPPED wiring is pinned behaviourally by `TestCrewResponderRegistry_RegistersHistorian`. Every OTHER role still misses, and dropping that one helper call (restoring `NewCrewResponderRegistry(nil)`) restores the pre-#3742 behaviour exactly — every consult refused at send with `crew_responder_unavailable`. `CrewResponder` is a plain-data seam (`CrewConsultRequest` → `CrewConsultAnswer`), so a deterministic responder needs no model call.
  - **Per-stage cap.** `maxCrewConsultsPerStage` = 2, copying `maxScopeAmendmentsPerStage`: counted from the CHAIN (`crew_message_sent` entries stamped with the stage id whose document is a `response_required` consult thread ROOT — a threaded answer never consumes the asker's budget), so an expired or unanswered consult still counts; the cap bounds interruptions, not answers. **The check and the append are NOT serialized per run** (the scope-amendment cap's `CountByStage` then `Create` are not either): concurrent consults from one stage can each pass the check before either appends, so two concurrent consults can exceed the cap by one. The cap bounds interruptions, not safety. A stage RETRY opens a new stage row and therefore a fresh budget, exactly as the amendment cap does.
  - **Per-consult window.** `crewConsultWindowSeconds` = 900, mirroring `AmendmentPollWindowSeconds`; `crewConsultDeadline` = min(the document's `deadline`, now + window) — a sender may ask for LESS, never more.
  - **Dispatch.** `dispatchCrewConsult` runs on `context.WithoutCancel` + `WithDeadline`, tracked by `s.bgReviews` (the #584 detachment; drained at `Shutdown`), so the consult outlives the sender's connection (`TestCrewConsult_SurvivesSenderDisconnect`). The responder runs on its own INNER goroutine so one ignoring its context cannot hold the consult open past its deadline, and that inner goroutine is ALSO tracked by `s.bgReviews` (E77.14 / [#3876](https://github.com/kuhlman-labs/fishhawk/issues/3876)): when the deadline branch returns without the responder, `Shutdown`'s drain — bounded by `shutdownCtx`, the same bound that already protects it from a hung reviewer — WAITS for the abandoned goroutine instead of leaving it untracked. **Honouring `ctx` is a `CrewResponder` CONTRACT obligation**, not a courtesy; #3742's historian is the first implementation bound by it, and because it is DETERMINISTIC (no model call) what it honours is its decision-index reads, which it passes `ctx` to. Read the residual honestly: a responder that never returns is a goroutine the process reclaims only at exit — the drain bounds SHUTDOWN, it does not unwedge the responder. An answer becomes a `notice` from the responder role to the consult's sender on the same anchor, recorded through `RespondToCrewMessage` (threaded reply + `accepted`). A responder error, a blank answer, an answer the contract rejects, or the deadline disposes the consult `expired` with the cause as the disposition entry's `reason`, WARN-logged with run id, sequence and role. **The stage is never touched** — no transition, no failure edge, no stage-scoped audit entry (`TestCrewConsult_ExpiresAtDeadline` reads the stage row and the chain back).
  - **`crewConsultSink` — the fault-injectable seam (E77.14 / [#3876](https://github.com/kuhlman-labs/fishhawk/issues/3876)).** A two-method unexported interface (`Respond`, `Dispose`) over the only two mailbox interactions the dispatcher performs. Production is `serverCrewConsultSink`, a pass-through to `(*Server).RespondToCrewMessage` + `cfg.CrewMailbox.Dispose`, resolved by the lazy `(*Server).crewConsultSink` accessor — `crewConsultSinkOverride` is nil in production and `New` needs no change, mirroring `releasePublisherOverride`. It exists because three branches are unreachable through a real mailbox: the **answered-but-not-disposed** pairing (a RECORDED reply with a FAILING disposition, which no real mailbox yields on demand), `expireCrewConsult`'s tolerance ladder (a chosen `Dispose` error class — wrapped `ErrAlreadyDisposed`, `*ProjectionError`, an unrelated error, each its own row with an anti-vacuity control), and the goroutine-tracking counterfactual (a responder provably still inside `Respond` after the deadline branch returned).
  - **Plan stage only.** The consult instructions are rendered in the PLAN prompt only: the in-process `plan_review` / `implement_review` reviewers hold no run token (`FISHHAWK_API_TOKEN` is runner-side) and cannot reach this endpoint. The review-stage half is deferred to a follow-up the operator files; `write:messages` is still minted for review stages in anticipation of a runner-dispatched review.
  - **Historian responder (`crew_historian.go`, E77.8 / [#3742](https://github.com/kuhlman-labs/fishhawk/issues/3742), ADR-081 rule 8).** The FIRST and only registered responder, and a DETERMINISTIC one: it answers a planner's "has this been decided?" from the E75.3 precedent query over this repository's own decision index, with no model call, no prompt and no write. `NewHistorianResponder` REFUSES a nil index with `ErrHistorianIndexRequired` — a responder runs on a detached goroutine with no HTTP response to fail into, so a nil field would panic there rather than expire one consult (`TestNewHistorianResponder_RefusesNilIndex`).
    - **WHY a planner may see precedent at all.** ADR-082 [#3728](https://github.com/kuhlman-labs/fishhawk/issues/3728) rule 6 makes precedent captain-facing in alpha; decision (e) option 3 carves out exactly one exception — agents, later, only through ADR-081 consults, STRUCTURED FIELDS ONLY. The projection is the CONTROL for that exception, not a waiver of it, and it is structural in two layers: the responder holds ONLY a `PrecedentIndex` (no audit repository), so it is INCAPABLE of reading the chain where another run's reason prose lives; and every rendered field crosses through ONE function, `projectHistorianItem`, whose output type `historianItem` carries a CLOSED field set (class, outcome, reject class, decided date, delegated, actor kind, doctrine version, concern category, severity, ≤2 matched path prefixes, ≤1 matched escalation key, score total, source sequence, source entry-hash prefix — and nothing else). It deliberately does NOT reuse `handleGetPrecedent`, whose `attachReasonExcerpts` reads exactly that prose. `TestProjectHistorianItem_FieldSetIsClosed` enumerates the set so a new `precedent.Item` field is never auto-exposed; `TestHistorian_RenderOmitsReasonProse` and the pg end-to-end assert a sentinel planted in `ReasonExcerpt`/`ReasonKey` and in the cited CHAIN entry reaches nothing an agent sees.
    - **Ranking context, and why prose paths are the only path signal.** The crew-message contract has NO path field by design (ADR-081 rule 2 forbids a field that can express scope; `crewmessage.EvidenceKind` omits a file member and directs a caller to cite a file in a payload prose field, "which carries no authority"). The structured alternative, `decisionindex.GateContext`, derives touched paths from the run's LATEST PLAN ARTIFACT — which does not exist while the plan stage that is consulting is still running (`TestCrewConsult_HistorianEndToEnd_Pg` asserts that list is EMPTY at consult time, so the union below is load-bearing and not decorative). So the responder ranks against `gc.TouchedPaths` ∪ `historianProsePaths(question, what_i_can_infer, context)`: a closed extraction (split on whitespace and the punctuation that brackets a path in prose; keep tokens containing `/`, carrying no URL scheme, ≤200 bytes; sort, de-duplicate, cap at 20). Those tokens are a RANKING SIGNAL ONLY — they reach the parameterized `ListFilter`'s sibling ranking context and `precedent.Rank`'s set arithmetic, are echoed back capped, and `CrewConsultAnswer` has no scope/constraint/authority field for them to reach (`TestHistorian_ProsePathsRankOnly` asserts that structurally). The concern category resolves against `decisionindex`'s CLOSED canonical vocabulary, adopted only on EXACTLY ONE word-boundary match — zero and ambiguity both leave it empty (match-all) rather than guessing.
    - **Scope is part of the REQUEST.** `CrewConsultRequest.AccountID` carries the sender's workspace account, populated on the send path with `callerAccountUUID`'s fail-closed rule (a non-empty non-UUID refuses the send 500 rather than handing the responder a widening nil). `GateRef` carries it into the run read and `ListFilter{AccountID, AccountScoped: true}` into the row read, so a nil account matches ONLY untenanted rows. `TestHistorian_AccountScopedRead` reads the filter back; `TestHistorian_ForeignAccountRowsAbsent_Pg` seeds another account's row under the same repository and asserts it is absent.
    - **Query shape.** One `List` per class over the allow-list ADR-082 rule 5 names — `plan_approval`, `concern_waive`, `concern_defer` — newest-first, window 500, then `precedent.Rank` to 3 items. The hard filter's repo comes from the RUN, never from prose (`TestHistorian_HardFilterRepoFromRun`). `StageKind` is left EMPTY (match-all) on purpose: a waive or defer bearing on a plan approach may have been decided at an implement gate, and rule 3's required boundary is repository and class. The class narrowing, the unrestricted stage kind and a FULL candidate window are all STATED in the answer's resolved-context line, so silence is never read as absence of precedent.
    - **Byte budget.** The rendered `summary` + `detail` must fit `prompt.MaxCrewMessageBytes` (2000 sanitized bytes) or `writeUntrustedCrewMessages` would cut it with no signal. The per-class item cap and the per-item matched-key caps are what make it fit BY CONSTRUCTION: `TestHistorian_AnswerUnderCrewMessageCap` drives the pathological case (500 rows per class, each with 200 long matching paths) and asserts BOTH that the render is under the cap AND that the belt-and-braces marker is ABSENT — the marker firing means the caps stopped being sufficient. `boundHistorianDetail` keeps that final truncation as a last resort and MARKS it (`TestHistorian_FinalTruncationMarks` forces it). Evidence refs are capped at `prompt.MaxCrewMessageEvidenceRefs`, imported rather than mirrored.
    - **Expire, never fabricate.** An unresolvable run (`decisionindex.ErrRunMissing`, which a cross-account read is indistinguishable from) or a failed row read RETURNS an error, so `runCrewConsult` disposes the consult `expired` with a named reason (`TestHistorian_RunMissingReturnsError`, `TestCrewConsult_HistorianRunMissingExpires_Pg`). An empty result is NOT that case: it answers a non-blank "no prior decision of these classes for this repository" (`TestHistorian_NoPrecedentStillAnswers`) — a blank `Summary` is read by `runCrewConsult` as no answer and would expire every consult on a fresh repository.
    - **Determinism.** `TestHistorian_AnswerByteIdenticalAcrossCalls` (two calls over the same rows), `TestHistorian_GoldenAnswer` (one exact rendered string a model could not reproduce) and `TestHistorian_NoModelCall` (a `ReviewerSet` that fails the test if any adapter is resolved).
  - **Residual.** Expiry is owned by the dispatching goroutine: a fishhawkd restart mid-consult leaves the row `open` past its deadline. The stage is unharmed (its `?wait=` returns `answered: false` and it proceeds); the stale row needs a sweeper, which is follow-up work. **A wedged responder's accumulation ceiling is unchanged by the E77.14 tracking**: one goroutine per abandoned consult, bounded per stage by `maxCrewConsultsPerStage` = 2 but UNBOUNDED across the process lifetime. No outstanding-responder semaphore is added — tracking makes an abandoned responder ACCOUNTED for at shutdown, it does not cap how many can accumulate.
- **The gate view's `consults[]` block (E77.8 / [#3742](https://github.com/kuhlman-labs/fishhawk/issues/3742)).** `gateViewConsultsForRun` puts what a run ASKED beside the gate it shaped. Selection MIRRORS `crewConsultBudgetUsed` and `resolveAnsweredCrewConsults` exactly — a `crew_message_sent` chain entry stamped with a stage id, `thread_root_sequence` nil, type `consult`, `response_required` true — so the three surfaces cannot disagree about what a consult is; unlike the prompt fold, an OPEN or EXPIRED consult is INCLUDED, because "what did this run ask" includes the questions that went unanswered. Each row carries `sent_sequence`, `stage_id`, `stage_kind`, both roles, the derived `state`, `answered`, an EXCERPTED question and answer summary (`gateViewConsultTextCap`), the answer's `cited_entry_refs` and `asked_at`. The `stage_kind` filter matches the CONSULT's own stage, resolved from the run's stage rows. **Degradation is visible and WHOLESALE**: any read failure records the `crew_consults` gap, sets `history_incomplete` and returns an EMPTY array — never a partially built block, whose silence would read as "asked nothing" (`TestGateView_ConsultsDegradeVisibly`, with `TestGateView_ConsultsEmptyWithoutGap` as the quiet-run control). An unconfigured AUDIT REPOSITORY is NOT a failure — there is no chain to read consults from, so the pre-E77.8 response is returned byte-for-byte. An unconfigured MAILBOX is different, and only reachable once the chain already records consults: the questions exist and their state cannot be read, which IS a gap. The block is mirrored on the MCP wire (`mcpserver.gateViewConsult`, pinned by `TestGateViewConsults_WireShape`) and in `docs/api/v0.openapi.yaml`'s `GateViewConsult`.
- **Escalation decision.** 403 `self_decision` (run-bound) -> `write:stages` -> `{decision: accepted|rejected, reason?}` (`DisallowUnknownFields`) -> the row must be visible and an `escalation` (422 `crew_message_not_escalation`) -> `Dispose`. 409 `crew_message_already_disposed`, 422 `crew_round_bound_exhausted`, 404 `crew_message_not_found` (including a sequence naming another category's entry). `TestCrewMessageAPI_EscalationDecisionRoundTrip` reads the CHAIN after the refused second decision (exactly one `crew_message_disposed`), since a fire-and-rollback would return a byte-identical 409.
- **No new audit category (E77.3; E77.7 registers four — see the E77.7 subsection below).** Send writes `crew_message_sent` (a consult's stamped with its stage id), the responder a threaded `crew_message_sent` + `crew_message_disposed`, the decision `crew_message_disposed` (or `crew_message_escalated`) — all registered by E77.2, and all still WRITTEN by the mailbox rather than by this package. Two of them acquired an issue-comment surface in E77.6 (#3740): `crew_message_escalated` is now an `issuecomment` `activityCategories` member (the anchor timeline's "Crew disagreement escalated to the captain") and a page-class ping, and `crew_message_sent` yields a page-class PING ONLY (never a timeline line) for a root `escalation`. `crew_message_disposed` stays wholly internal. See the escalation-page bullet below and `docs/issue-comment-surfaces.md`.

- **Escalation page + notify sites (E77.6 / [#3740](https://github.com/kuhlman-labs/fishhawk/issues/3740)).** Before this, neither crew-message handler called a notifier at all, so every escalation chain entry was silent on the issue thread. Two notify sites, both best-effort and both AFTER the response-shaping decision, so a notifier outage can never turn a recorded send or a recorded escalation into a different HTTP result (`TestCrewMessageNotify_FailureDoesNotChangeResponse`):
  - `notifyRootEscalationPage` (`notifyPageClass`, source `crew_message_sent`) — called from `handleSendCrewMessage` and from `RespondToCrewMessage`'s reply send, ONE function so the two call sites cannot drift. Its condition MATCHES the ping projection: a type-`escalation` THREAD ROOT on a RUN anchor only. A threaded reply notifies nothing (the projection yields no page for one), a consult notifies nothing, and a run-less anchor notifies nothing — stated residual, since both projections are run-scoped.
  - `notifyOperatorVisible(…, crewmessage.CategoryEscalated)` — called from `handleDecideCrewEscalation` when `Dispose` returns `ErrRoundBoundExhausted`. `Mailbox.escalate` appends the `crew_message_escalated` entry INSIDE the transaction that then reports exhaustion, so the notify follows a DURABLE row; `TestDisposeCrewMessage_RoundBoundExhaustionNotifiesOperator` reads the chain at the moment the notify is observed and would fail if the append were rolled back. The 422 refusal to the caller is unchanged, which is exactly why that test asserts on the notifier RECORDER and not on the status code — the refusal bytes are identical with or without the notify.
  - **The gate refusal is TIER-INDEPENDENT.** `activePageEvent` checks `crewEscalationOpen` FIRST and OUTSIDE the `res.MustPageHuman` loop and returns the server-package `PageEventCrewEscalation` (`"crew_escalation"`), deliberately NOT a `spec.PageEvent*` constant — so it cannot appear in a `page_human_on` list and `expandTier(TierLow)`'s EMPTY page list cannot drop it. `crewEscalationOpen` reads the CHAIN (never the derived table, matching the round bound's count-from-the-chain discipline): an escalated entry whose thread ROOT holds no terminal disposition, or an open root `escalation`. `accepted`/`rejected` are terminal; `expired` is NOT (an escalation nobody answered still needs a human). **The ruled set is keyed by the ruled message's OWN sequence, never by its thread root** — the rejections that exhausted the bound are rounds of the disagreement, not rulings on it, so keying by thread root would let them mark the escalation settled and the page would never fire on the shape it exists for (`TestCrewEscalationOpen_Branches`, cases `reply rejections do NOT resolve the exhaustion` / `accepting a REPLY does not resolve the exhaustion`, both observed RED under that swap). An audit read error fails TOWARD paging; an unconfigured `AuditRepo` returns false (structural absence, not doubt). Contract: `docs/METHODOLOGY.md`.
  - **One terminal-disposition path, by construction.** Because an `accepted`/`rejected` root escalation becomes TRUSTED binding prompt text (next bullet), the write side is narrowed to the captain's endpoint alone. The full reachable set, traced: there is NO generic dispose route — the six `/v0/crew-messages` routes are send, list, get, respond, escalation-decision and convert-to-concern, and the last disposes ONLY a `finding` (422 `crew_message_not_finding` for every other type, escalations included); `handleRespondCrewMessage` refuses EVERY HTTP caller (`agent_token_required` / `responder_required`); `expireCrewConsult` writes only `expired`, which is NOT terminal for this fold; and `RespondToCrewMessage`, the one in-process path that disposes `accepted`, now REFUSES a row whose `MessageType` is `escalation` rather than resting on its sole caller's type-`consult` gate in `crew_consult.go`. So `handleDecideCrewEscalation` — 403 `self_decision` for a run-bound token, `write:stages`, account-visible, 422 `crew_message_not_escalation` — is the ONLY path to a binding ruling. `TestRespondToCrewMessage_RefusesEscalation` reads COMMITTED STATE (row still open, zero resolved rulings) rather than the error alone, with an answerable CONSULT as the discriminating control; with the guard deleted it produced a binding `accepted` ruling carrying an EMPTY reason.
  - **Binding only once answered.** `resolveDecidedCrewEscalations` (`prompt.go`) folds a root escalation into `prompt.Trigger.CrewEscalationRulings` on BOTH plan-stage prompt handlers (each wiring line pinned independently through its REAL route by `TestPlanPrompt_RouteCarriesCaptainRuling`, since the two handlers carry separate copies and a deleted one is invisible to the compiler) ONLY once its derived row is terminal, carrying the contract-closed disposition and the operator-authored reason from the `crew_message_disposed` entry and NEVER the agent-authored `summary` / `recommended_default` / `tradeoffs`. The disposition entry must also carry the user `ActorKind` the captain endpoint stamps — any other actor (system, agent, absent) is skipped and WARN-logged, so a non-captain `Mailbox.Dispose` of an escalation binds nothing (#3884, pinned by `TestResolveDecidedCrewEscalations_OnlyUserActorDispositionBinds`). An open escalation folds NOTHING. Best-effort exactly like `resolveAnsweredCrewConsults`. Contract: `backend/internal/prompt/README.md`.
- **Tests.** `crewmessage_test.go` (no database: every refusal fires before a nil-pool mailbox could be reached; plus the E77.6 send-time notify condition as a unit table and the AC2 escalation-payload refusals) and `crewmessage_pg_test.go` (pgtest end-to-end: HTTP -> mailbox -> chain + row -> render; the read gate; both `?wait` completions and the expiry; the escalation round trip; `RespondToCrewMessage`; and E77.6's notify wiring plus `TestRespondToCrewMessage_RefusesEscalation`, and `TestCrewEscalation_EndToEnd_LowTierRunPagesThenBinds`, which crosses all four layers on a `low`-tier run — send, exhaust, assert the notifier fired AND `issuecomment.PageClassEventKinds` over the run's REAL chain yields both crew page kinds, assert the plan prompt carries no ruling, decide over the real handler, assert it now does). `crew_consult_test.go` (registry construction, deadline resolution, and the HERMETIC E77.14 arms over the `crewConsultSink` seam — no database: inner-goroutine tracking, answered-but-not-disposed with its expiring negative arm, and the four-row expiry tolerance table) and `crew_consult_pg_test.go` (the consult round trip end to end with a fake deterministic responder, and one test per named failure mode: budget, unregistered role, implementer recipient, deadline expiry, responder error, blank and invalid answers, the empty production registry, sender disconnect, and the real-mailbox `ErrAlreadyDisposed` tolerance arm).

### Deferred delivery, the gate-view crew block, work requests and finding conversion (E77.7 / [#3741](https://github.com/kuhlman-labs/fishhawk/issues/3741))

ADR-081 #3727 D1 option 3 for `finding`/`notice` and rule 2 for `work_request`. Nothing here touches scope, constraints, delegation or a gate outcome. Four new INTERNAL audit categories (`crew_message_delivered`, `crew_work_request_filed`, `crew_finding_converted`, `crew_finding_convert_failed`) are registered in `backend/internal/audit/categories.go` and deliberately NOT in `issuecomment`'s `activityCategories` — crew text reaches a reader only through the quarantine envelope. Every path guards on `Config.CrewMailbox`, so dropping it (the E77.3 kill switch) leaves pre-E77.7 prompts and gate views byte-identical.

- **Deferred delivery (`crew_delivery.go`).** `resolveDeliverableCrewMessages(ctx, runID, stageID, readingStage)` keeps a run-anchored row only when it is a `finding`/`notice` (`crewDeliveryTypes`), addressed to the reading stage's role (`crewRecipientRoleForStage`, a POSITIVE allow-list: plan -> planner, review -> reviewer, nothing else), OPEN, a thread ROOT, and not recorded delivered to a DIFFERENT stage (a retry of the SAME stage re-renders it). At most `maxCrewDeliveriesPerPrompt` = 3, newest by sequence, rendered ascending through `crewMessageForPrompt`. The four trigger sites (both plan-prompt handlers, the `plan_review` trigger, both `implement_review` triggers) PREPEND the deliveries to `Trigger.CrewMessages` ahead of the E77.5 consult fold-in, because `writeUntrustedCrewMessages`' block cap drops from the FRONT: advisory findings (still open, still on the gate view) are elided before a consult answer the stage cannot re-obtain. Never-into-implement holds twice: no implement render calls `writeUntrustedCrewMessages`, and no implement trigger is assigned. `recordCrewMessagesDelivered` appends one `crew_message_delivered` entry (`sent_sequences`, `stage_type`, `recipient_role`, `render`) from the SIGNED prompt handler and the in-process review paths only — never the `prompt-render` preview. Best-effort throughout (nil repos, a list error or one unresolvable document WARN and skip; a failed record means the message is offered again). Residual: a delivery recorded as handed to the renderer can still be elided by the block cap without being re-offered; it stays on the gate view.
- **Gate-view crew block (`gateview.go` `gateViewCrewMessagesFor`).** `crew_messages[]` lists every OPEN `finding`/`notice` thread root on the run WHATEVER its recipient (architect/security/historian findings have no stage; the gate is their only surface), carrying contract-closed metadata plus `rendered` = `prompt.RenderCrewMessages` over that one message (the `renderCrewRow` rule). OMITTED for every run-bound identity: `handleGetRunGateView` authorizes an `mcp:run:` token by the cross-run subject guard alone, so an implement-stage token (never minted `write:messages`) could otherwise read crew text around the REST read gate (ARCHITECTURE.md §6 invariant #8). No mailbox -> no block, no gap; a read failure -> no block plus a `crew_messages` `history_gaps` entry with `history_incomplete` set, so a partial read is never rendered as absence. A finding is a `crew_messages` row, never a concern, so it is structurally absent from `ConcernRepo.ListOpenByRun` — the merge gate's source (`TestCrewDelivery_OpenFindingIsNotAnOpenConcern`, an invariant test).
- **Work request (`crew_work_request.go`).** After `Mailbox.Send` commits, a `work_request` files exactly ONE item through the shared `applyAndFileWorkItem` core (title format, `type:*` default, derived labels, `Status=Backlog`, boarding, epic link) with a body citing the message, its evidence, the anchor run and the sent sequence; it never calls `RunRepo.CreateRun` or a dispatch path. A run-anchored message files against the anchor run's repository (run-scoped); a run-bound token on the run-absent path is refused `run_scoped_filing_required` with nothing filed. ORDERING IS DELIBERATE — the inverse of defer-concern's file-first: the chain entry is the record and must survive, so a filing failure keeps the 201 and sets `work_item_filing_error` (`run_scoped_filing_required` / `crew_work_request_no_repository` / `work_item_filing_failed` / `internal_error`); a landed filing sets `work_item` and appends `crew_work_request_filed`.
- **Finding conversion (`crew_convert.go`, `POST /v0/crew-messages/{sequence}/convert-to-concern`).** The captain's explicit route from an advisory finding to a binding concern; after it the EXISTING `concern_ids` fix-up routing applies unchanged. Order: `crewConfigured` + a wired `ConcernRepo` (503 `crew_message_unconfigured`) -> 403 `self_decision` for a run-bound token BEFORE any read (a crew role must not promote its own advice into a merge-gate blocker) -> `write:stages` -> optional `{reason}` (`DisallowUnknownFields`) -> the row must be account-visible (404) and a run-anchored `finding` thread ROOT (422 `crew_message_not_finding`, `details.reason` `not_a_finding` / `run_less_anchor` / `not_a_thread_root`) and open (409 `crew_message_already_disposed`) -> `crewConcernStage` picks the run's NEWEST plan/implement stage (422 `crew_finding_no_concern_stage` when none — `review_concerns.stage_kind` admits only those two) -> the recorded document resolves via `crewSentDocument` (500 `internal_error`; nothing disposed yet) -> `Dispose(accepted)` FIRST, so the mailbox's refuse-before-append `ErrAlreadyDisposed` makes conversion exactly-once even against a racing converter whose row read was stale (`TestCrewConvert_DisposeFirstIsTheExactlyOnceGate`) -> THEN `InsertRaised` (severity from the finding, default `medium`; category `crew_finding`; `OriginReviewSequence` = the finding's sent sequence; a note quoting the summary and citing `crew_message:<sequence>`) -> `crew_finding_converted`. An insert failure (or an empty result) after the dispose appends the corrective `crew_finding_convert_failed` (the `concern_defer_failed` shape: `sent_sequence`, `disposition_sequence`, `actual_state`, `error`) and answers 500 `crew_finding_convert_failed` — never a success entry for a conversion that did not land. Disposing also drops the finding out of the delivery resolver and the gate-view block, so one converted finding never appears twice. Because `OriginReviewSequence` is the FINDING's sequence, `gateViewRound` derives an implement concern's round from when the finding was SENT, not converted (`TestCrewConvert_ConvertedConcernRoundIsDerivedFromFindingSequence` pins the value). The MCP wrapper is `fishhawk_convert_crew_finding` (`backend/internal/mcpserver/README.md`), gated `anyOf: [write:stages]` in `mcpscopes.go`.
- **Tests.** `crew_delivery_test.go`, `gateview_test.go` (`TestGateView_Crew*`), `crew_work_request_test.go` and `crew_convert_test.go` — the last carries the cross-boundary `TestCrewDelivery_EndToEnd_SendThenPlanPromptThenGateViewThenConvert` (HTTP send -> signed plan prompt -> gate view -> HTTP conversion -> both crew surfaces cleared and the concern open) and reads committed state (concern rows, the row's state, the chain) on every refusal.

## First-officer seat (E85.6 / [#4148](https://github.com/kuhlman-labs/fishhawk/issues/4148))

The ADR-089 read-only first-officer seat, PARTIAL delivery (the ADR-097 standing-watch trigger engine, on-request runtime, seat spawner, spend cap and approver-members wrapper are #4148's remaining scope; the lease-scoped seat is #4421). The subject grammar and scope profile are `backend/internal/firstofficer/README.md`; this section is the server contract across four files.

- **Identity (`first_officer_identity.go`).** A seat IS an api token whose subject is `first_officer/campaign/<uuid>` or `first_officer/run/<uuid>`. `bearerAuth` hands every seat-subject token to `resolveFirstOfficerSeat`, which ALWAYS clamps scopes to `firstofficer.SeatScopes` (`read:runs`, `read:audit`, `write:messages`, `request:specialist_jobs`) first, so a hand-minted seat token carrying `write:approvals` cannot approve; then refuses a malformed subject (401 `first_officer_seat_invalid`), unwired repos or an unreadable anchor (503 `first_officer_seat_unverifiable`, nothing revoked), and RETIRES the seat on a terminal or missing anchor (every token for the subject revoked, `first_officer_seat_retired` appended — the run's chain for a run seat, the global chain for a campaign seat — 401 `first_officer_seat_retired`). A live anchor stamps its account onto `Identity.AccountID` (approval condition 1): reads and writes are account-scoped to the anchor's account, and an untenanted anchor stays untenanted. Seat subjects classify as agents (`isAgentSubject`). POST `/v0/work-items` requires `write:runs` of every non-run-bound caller (#4481), which a seat's clamped scopes lack; `refuseFirstOfficerTrackerWrite` (wrapped at registration in `handlers.go`) stays the NAMED seat refusal ahead of that scope check, so over REST a seat subject is refused 403 `first_officer_tracker_write_not_permitted` before the handler and the refusal is recorded (`TestFirstOfficerSeat_CannotFileWorkItem`). Over `/mcp` the `fishhawk_file_issue` row now requires `write:runs` too, so a seat is refused earlier, at the gate, with 403 `insufficient_scope`, and that gate refusal records no row (the residual below). `writeError` records `first_officer_action_refused` on every 403 served to a seat; a refusal that bypasses `writeError` (the `/mcp` JSON-RPC tool-scope precheck) records nothing — the stated residual. That row is unbounded per seat (no coalescing), as is a seat's `POST /v0/specialist-jobs`: a looping seat agent amplifies refused requests into chain writes and requests into queued jobs. The ADR-089 D6 spend cap / a per-seat rate bound is remaining scope on #4148. Over `/mcp` a seat reaches no tool that spawns a host program: `request:specialist_jobs` is NOT an alternative on `fishhawk_request_specialist_job` (which spawns the daemon-resolved runner with the daemon's environment in a caller-chosen `working_dir`; a caller-chosen `runner_binary` is refused over HTTP since #4476), so a seat files jobs over REST, which only queues (`TestFirstOfficerSeat_MCPRefusesRunnerSpawningTools`).
- **Lifecycle routes (`first_officer_seat.go`).** `POST /v0/first-officer-seats` `{campaign_id | run_id}` is a captain act: `write:approvals`; an agent caller → 403 `first_officer_attach_requires_human` (before any anchor read, so an agent learns nothing about the anchor; a delegated operator-agent attach waits for an `operator_agent` grant); an anchor outside the caller's account → 404 (not disclosed); while `currentCaptain` reports a SITTING captain for the anchor's repository (read in the anchor's account partition) only that subject attaches → 403 `first_officer_attach_not_captain`, and a vacant or unavailable captain record admits any human `write:approvals` holder with `captain_basis` recorded; a terminal anchor → 409 `first_officer_anchor_terminal`; an active seat token → 409 `first_officer_seat_exists`. One seat per anchor has no DB unique index: after `Issue`, `convergeSeatTokens` re-lists the subject's active tokens, keeps the OLDEST (`created_at`, then id) and revokes the rest, so a losing concurrent attach answers 409 with its token revoked. The outcome reflects PERSISTED revocation: if any losing token's `Revoke` fails, there is no winner — the attach withdraws its own token and answers 500 naming the unrevoked token ids, never a 201 beside a live competitor or a 409 claiming a revocation that did not happen. A failed convergence read or attach-row append likewise withdraws the issued token and answers 500 (an unrecorded or unproven seat must not exist); every such 500 says whether that withdrawal persisted, and when it did not, names the token as possibly still active and points at the retire verb. `first_officer_seat_attached` `{seat_subject, token_id, anchor, scopes, attached_by, captain_basis}` never carries the plaintext; the 201 returns the token ONCE. `GET /v0/first-officer-seats?campaign_id=|run_id=` (`read:runs`) derives attach state from the active tokens. `POST /v0/first-officer-seats/retire` `{anchor, reason}` applies the same human/captain gate (a terminal anchor is allowed), requires a non-empty `reason` (1-1000), revokes every active token and appends `first_officer_seat_retired` `{reason: captain_detached, note, …}`; no active token → 404 `first_officer_seat_not_attached`. Any of the token, audit, run or campaign repositories unwired → 503 `first_officer_seats_unconfigured`.
- **Blocking findings and the hold (`first_officer_gate.go`).** A seat's crew finding citing a runner-attested FAILING specialist-job completion on its anchor run converts (insert-then-dispose) to an open concern with provenance `first_officer`; `firstOfficerHoldRefusal` is the ONE predicate deciding the hold, and an unwired `ConcernRepo` is no hold (the explicit fail-open for the unconfigured case only). Long-form: `backend/internal/crewmessage/README.md`, `docs/spec/crew-message-v1.md`.
- **Where the hold applies.** `handleSubmitApproval` (approve only; plan stage → plan-kind, review stage → implement-kind; 409 `first_officer_finding_open`, read error 500, no approval row), `handleMergeRun` (after `review_coverage_incomplete`, before the merge seam; no verdict row), the delegated auto-drive arms (`firstOfficerAutoDriveHold` in `autodrive.go`: may_approve before `approveStageAs` on the gated stage's kind, may_merge after the review-coverage hold and before the merge overlap guard (`mergeOverlapAutoDriveHold`, #4582); a hold is `decision_required` `first_officer_finding_open`, a read error observe-only), and the gate view's `merge_readiness` (a `first_officer_finding_open` blocker, or an `undetermined` "first-officer findings: …" line plus the `merge_readiness` history gap on a read error). The drive layer's `awaiting_merge` next-action stamp is NOT held, so `next_actions` may still suggest merge while `merge_run` refuses with the named code. The captain clears the hold through the existing concern surface: waive (reason required), defer, or route a fix-up.
- **Tests.** `first_officer_identity_test.go` (clamp, retirement arms, tenanted anchor), `first_officer_gate_test.go` (evidence resolution, conversion, approve/merge holds), `first_officer_seat_test.go` (attach/get/retire refusals, post-issue convergence both ways plus revoke-failure injection, auto-drive and gate-view holds — the latter isolate the hold's own concern read with `holdReadConcernRepo`, because the delegated conditions and the gate view's concern block react to an open concern first and would mask the hold), and `first_officer_pg_test.go` (the cross-boundary walk over real Postgres: attach → four prohibited verbs refused with `first_officer_action_refused` rows → seat job request → runner-attested failing completion by construction → blocking finding → the captain's `recipient_role: first_officer` notice listed by the seat (`GET /v0/crew-messages?recipient_role=first_officer`) → merge 409 + gate-view blocker → waive → retirement on cancel; plus the captain gate over the real captain store).
- **Rollback.** Revert the PR; there is no migration. Retire live seats first (`POST /v0/first-officer-seats/retire`, or revoke tokens whose subject starts with `first_officer/`): a reverted server no longer clamps them. Converted `first_officer` concerns stay ordinary open concerns.

## Handover brief (`handover_brief.go`, E76.4 / [#3767](https://github.com/kuhlman-labs/fishhawk/issues/3767))

`GET /v0/handover-brief?repo=&section=&from_sequence=&to_sequence=` over `backend/internal/handoverbrief` (composition, window, hash and Bound are that package's contract — `backend/internal/handoverbrief/README.md`); this is the HANDLER contract, copied posture-for-posture from `handleGetDigest`.

- **Posture.** `scopeHandoverBriefRead` = `scopeDigestRead` (`read:audit` — no new scope, so no token loses access) → `handoverBriefConfigured` (501 `handover_brief_unconfigured`, the message naming each missing REQUIRED collaborator: `digest_store`, `decision_index`, `captain_store` — `TestGetHandoverBrief_UnconfiguredIs501`) → `repo` required / `ParseSection` / non-negative sequences (400 `validation_failed` with `details.field`) → `enforceRepoVisibility` (403 `repo_forbidden`, `TestHandoverBriefRoute_ForeignRepoIsForbidden` over a NON-EMPTY brief) → `Compose` → `Select` → `Bound` at `handoverbrief.DefaultByteBudget` → 200. NEVER writes, mints no audit entry.
- **Collaborators — no Config field of its own.** `handoverBriefDeps` reuses `DigestStore` + `DigestIndex` + `CaptainStore` (required) and `CampaignRepo` + `RunRepo` (optional; nil degrades `in_flight` inside the brief). Every nil is checked explicitly so a nil interface never becomes a typed-nil lister.
- **Delegation view = the run cache.** `handoverBriefDelegation` projects `delegationview` from the spec cached on the repo's NEWEST run — the `source=run_cache` read of `GET /v0/repos/{owner}/{name}/delegation`, account-narrowed — never a forge fetch, because the brief composes on every offer and a forge round-trip there would couple a handover to forge availability. Each non-projectable case (no run repo, list error, no run / no cached spec, an invalid spec) returns nil with a NAMED reason that the brief records as `delegation_unavailable` (`TestHandoverBriefDelegation_EachUnavailableBranch`). Residual: a spec changed since the last run is shown as the older cached one; `standing_orders.source` says `run_cache`.
- **Errors.** `writeHandoverBriefError`: `handoverbrief.ErrInvalidRequest` (incl. a `to_sequence` above the chain head) → 400 `validation_failed`; `handoverbrief.ErrUnavailable` → 503 `handover_brief_unavailable`, the reason in the MESSAGE (5xx detail keys are redacted); anything else → 500.
- **One hash.** The route returns the hash `Compose` stamped on the canonical, unbounded brief; `Select` and `Bound` copy it and never re-hash (`TestHandoverBriefRoute_ComposesAndBounds` asserts the whole and the `?section=` render carry the same hash, equal to `handoverbrief.Hash` of the composition). The brief is a POINT-IN-TIME composition: an offer's recorded hash commits to what was composed at offer time, and live sections (parked gates, campaigns, runs) and the chain head may differ on a later read.

## Delegation shadow stamp (`delegation_shadow.go`, E82.1 / [#3778](https://github.com/kuhlman-labs/fishhawk/issues/3778))

ADR-085 rule 4: at every HUMAN action on a delegable class, record what delegation WOULD have done — record-only, blind, grants nothing, changes no decision. One `delegation_shadow_evaluated` row (actor `system`, registered in `audit.KnownCategories`, NOT an issue-comment surface) per fresh human decision.

**Who is stamped.** `captureDelegationShadow` returns nil (never stamped) when the request is `delegated:true`, the subject is an agent (`isAgentSubject`: the operator-agent token family or `mcp:run:<uuid>`), or no audit store is wired. Per the ADR-040 trust model an agent presenting a human operator's token is indistinguishable from that human and is stamped as one; `actor_subject` is recorded so the projection can stratify.

**When.** Captured IMMEDIATELY before the decision call, after every refusal gate, so it evaluates the PRE-decision state; recorded only after the decision call succeeded and wrote a FRESH decision row — never on a duplicate, a refusal, an error, merge's `alreadyRecorded` / lost-race path, or a bulk batch where nothing landed. Appended AFTER the decision row; the link is `stage_id` + `decision_category` + chain order (no exact sequence pointer — concurrent writers may interleave).

**What.** `delegation.Evaluator.Shadow` (the same `evalClassCondition` checkDelegation's `Evaluate` routes through, nil campaign override, escalation ceiling applied last) evaluates the class's condition REGARDLESS of its mode. The capture then applies the page_human_on override Shadow does not: a met/unmet verdict with an active `activePageEvent` (incl. the unconditional crew escalation) becomes `not_delegable` with `page_event`. Payload (`shadow_version` 1): `action`, `class`, `condition`, `verdict` (the closed `delegation.ShadowVerdicts()` set: met | unmet | not_delegable | unevaluable), `met`, `reason` (the evaluator's unmet reason verbatim — byte-equal to a delegated call's `delegation_condition_unmet.unmet_reason` on the same state), `page_event`, `mode` / `mode_source` (clamped), `anchored` (mode == report: report proposals are SHOWN, so agreement there is anchored), `tier`, `matrix_resolved`, `resolved_matrix_hash` (`delegationview.HashMatrix`, the E76.1 wire mirror), `workflow_sha`, the E75.1 escalation stratum (`escalation_fingerprint`, sorted `escalation_keys` = `escalation.RuleKey` of the fired set, `max_autonomy`), `human_decision`, `decision_category`, `actor_kind`, `actor_subject`, `stage_id`, `concern_ids`. Shadow and checkDelegation both take only (run, action) — no request-specific input exists to thread.

**Non-emitting.** The evaluator gets `shadowEscalationResolver`, which evaluates escalations like `resolveEscalations` but never writes `escalation_fired`; the stamp is the capture's only write. No `notifyStatusUpdate` / `notifyOperatorVisible`.

**Bounded, best-effort.** The capture runs on `context.WithoutCancel(r.Context())` under a 3s bound (`defaultDelegationShadowCaptureTimeout`) and a select that returns at the bound even if a repository ignores its context; a timeout yields `unevaluable` (`capture_timeout: …`) and never delays the human beyond it. Every failure is an `unevaluable` stamp naming its mode (`repositories_unconfigured`, `run_read_failed`, `no_cached_spec`, `spec_unparseable`, `workflow_missing`, `evaluator_unavailable`, `shadow_evaluation_failed`, `open_concern_read_failed`, `capture_timeout`) — never a skipped stamp, never a refused action. The append uses `context.WithoutCancel` so a client disconnect after the decision does not drop it; a marshal/append failure is warn-logged, never propagated. **Accepted residual:** a crash between the decision row and the stamp append leaves a decision with no stamp — a missing-not-at-random bias (crash-correlated) the E82 projection must account for, not treat as "no evidence".

**Write-side inventory (approval condition 2)** — every caller of each decision core, classed:

| Core | Caller | Class | Stamped |
|---|---|---|---|
| `approveStageAs` | `handleSubmitApproval` (approvals.go) | human (or `delegated:true` / agent token, filtered) | yes, in the handler — not inside `approveStageAs` |
| `approveStageAs` | `autodrive.go` (campaign / `fishhawk_drive_run` auto-driver) | non-human (delegated) | no |
| `ApprovalRepo.Submit` + `approval_submitted` | `HandleApprovalCommand` (issue_approval.go: `/fishhawk approve\|reject`, `+1` reply) | human | yes (scope amendment a3996d2a) |
| `fixupStageAs` | `handleFixupStage` (fixup.go) | human / delegated / agent | yes for human |
| `fixupStageAs` | `autodrive.go` auto-driver | non-human (delegated) | no |
| `fixupStageAs` | `acceptance.go` triage (`acceptanceTriageSystemSubject`) | non-human (system) | no |
| `run.FixupStage` | `conflictresolution_trigger.go` | non-human (system) | no |
| `applyConcernWaive` | `handleWaiveConcern` (waive.go), `handleBulkWaiveConcerns` (bulk_waive.go, one stamp per request) | human / delegated / agent | yes for human |
| `retryStageAs` | `handleRetryStage` (retry.go; `decision_category` = `stage_override_retried` only for an override of a category-B failure, derived pre-decision) | human / delegated / agent | yes for human |
| `retryStageAs` | `autodrive.go` auto-driver | non-human (delegated) | no |
| `run.ReopenAcceptanceStage` | `retryAcceptanceOutcomeUnknown` (retry.go acceptance-reopen arm) | human, but NOT the delegable retry class: checkDelegation is never consulted there and `infra_flake` (a failed stage) cannot describe a succeeded acceptance stage | no |
| `merge_verdict_recorded` append | `handleMergeRun` (merge_run.go; never delegated) | human (agent tokens refused / filtered) | yes, fresh-append path only |
| delegated merge | `autodrive.go` (`ActionMerge`) | non-human (delegated) | no |

**Deferral is intentionally unstamped (approval condition 3).** `defer_concern.go` is untouched: deferral is not a delegable action class (no operator_agent knob or matrix class governs it; `delegation.Shadow` answers `not_delegable` for it), so the issue's proposal item 3 is narrowed to the five classes and the operator accepted that narrowing.

**Read-side inventory (approval condition 1)** — every server caller of `AuditRepo.ListForRun`, classed for the blindness contract (the withholding filter itself is slice C's):

| Caller | Class | Why |
|---|---|---|
| `reads.go` `handleListRunAudit` (non-chain list) | withheld | the general per-run list feeds the frontend run view; the frontend reads this non-chain endpoint, so the reads filter covers it (`fishhawk_get_run_status` `recent_audit` reads `GET /v0/audit`, `ListAll`, withheld by the same slice) |
| `reads.go` `handleListRunAudit` `chain=true` | integrity-exempt | hash-chain verification needs every row; the positive control pins the stamp is present here |
| `reads.go` `handleGetStatusComment` | internal-non-rendering | renders through `issuecomment.RenderStatusBody`, whose activity timeline is the `activityCategories` ALLOW-list — the stamp is not on it and must never be added |
| `product_report.go` diagnostic bundle | withheld | the bundle is PUBLISHED into a forge issue, so it must not carry the category or its payload |
| `diagnostics.go` `GET /v0/runs/{id}/diagnostics` | integrity-exempt | operator integrity/debug surface; stays complete by design, not a gate surface |
| `audit_export.go` compliance export | integrity-exempt | the external verifier and `frontend/src/repo/chain-verification.ts` read the export; it must stay complete |
| `acceptance.go` | internal-non-rendering | scans for acceptance outcome entries; renders nothing |
| `codescanning.go` | internal-non-rendering | scans for code-scanning alert entries |
| `latency.go` | internal-non-rendering | computes stage latencies from known categories |
| `precedent.go` | internal-non-rendering | indexes payloads by sequence for KNOWN decision categories only |
| `quorum.go` (x2) | internal-non-rendering | counts approvals / resolves the change author |
| `pullrequest_review_events.go` | internal-non-rendering | economics stamp over cost rows |
| `release_publish.go` | internal-non-rendering | reads only `release_published` |
| `repodash.go` | internal-non-rendering (payload) | `dashEvent` copies Category + Timestamp for unknown categories and never reads their payload; RESIDUAL: the category NAME and timestamp of a stamp reach the repo-dashboard event stream (no verdict, reason or mode) — a follow-up should drop it there |
