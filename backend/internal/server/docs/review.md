# backend/internal/server: Plan and implement review, concerns and plan gates

Split out of `backend/internal/server/README.md` (the index) on 2026-10-29 so concurrent runs stop colliding on one file. Section text is moved verbatim.

## Charter injection consumer (`charter_injection.go`, E54.2 / #2234)

The SECOND consumer of the repo-document injection mechanism `backend/internal/repodoc`
shipped inert (E55.1 / #2242). It adds **no** resolution, fetch, hashing, capping,
delimiter-neutralization or audit code of its own — all of that is repodoc's, reached
through the untouched `resolveInjectedDocuments`. It supplies exactly three things:
the **declaration site** (`charter.path` in `.fishhawk/work-management.yaml`, read
through the process-wide `conventionsLoader` seam), the **framing** (what a charter is,
plus the instruction to cite rubric lines BY ID — the uppercase `V*/R*/U*/S*` ids the
charter's rubric tables carry, matching the citation contract #2235 validates), and the
**fail-closed policy**.

**Stage discriminator — a PERSISTED RUN-LEVEL FACT (E54.13 / #2806).** `stageRequiresCharter`
is true for a `plan`-typed (PROPOSE, ADR-067 §2) run stage whose run carries
`requires_charter = true`. That column (migration 0082, `backend/internal/run/README.md`) is
stamped at every root mint seam from the same pure structural predicate the #2236 admission
gate evaluates at that exact moment — `spec.WorkflowRequiresCharter` over the workflow the
run was minted from, when the spec was known-parseable — and inherited verbatim by
`run.ChildParamsFrom`. The prompt-serve path READS the fact; it does not re-derive it from
the cached `WorkflowSpec`, which may since have been corrupted in storage. Not the
workflow's NAME (renaming would evade it) and not a `kind:` field. The stage-type early
return precedes the fact: a persisted-grooming row's implement stage is not a propose stage.

**Two fail-closed layers.**

| Layer | Where | Refuses when |
|---|---|---|
| L1 `charterDeclarations` | `Config.DocumentDeclarations` | the determination is undecidable on a legacy row (`grooming_workflow_spec_unreadable`, below), conventions unreadable (`conventions_unavailable`), no `charter:` block (`charter_absent`), empty `path:` (`charter_path_empty`), base ref unresolvable (`charter_base_ref_unresolved`) |
| L2 `assertCharterInjected` | `handleGetStagePrompt`, after `resolveInjectedDocuments` | the same undecidable-determination refusal (L2 calls `stageRequiresCharter` independently of L1), then: the injected set carries no document resolved from the declared charter path (`charter_not_injected`) |

L2 is what makes the guarantee unbypassable by a deployment that simply leaves the seam
unwired — the one configuration in which L1 cannot run. **L2 verifies charter IDENTITY,
not "some document":** it re-resolves the declared path and requires an injected document
AT THAT PATH, so a grooming prompt carrying an unrelated injected document and no charter
is refused. Both layers surface as `document_injection_failed` (no new error code) with
the machine-readable `reason` above; the actionable message reaches the operator through
the log record, because a 5xx body's details are redacted to the default-deny allow-list
above.

**Base ref — what is actually guaranteed.** `Config.DocumentBaseRef` (wired in
`cmd/fishhawkd` to `forge.Forge.GetRepository`) supplies the repo's **default branch**,
which repodoc pins to a 40-hex commit before any fetch. So: resolution pins to a specific
commit, never a mutable ref; the commit and content hash are recorded so a ranking is
attributable to an exact charter revision; and the commit is the default-branch head AT
SERVE TIME, which for a non-diff workflow is the defined base (a grooming run produces no
diff and owns no branch). Consequence, stated rather than hidden: a charter amendment
landing between run creation and prompt serve changes which revision constrains that run
— acceptable for grooming, decidable after the fact from the audit hash, and **not
reusable** by E55's review-conventions consumer. That consumer declares
`repodoc.BaseSourceRunAdmission` and `resolveDeclaredDocuments` resolves it ONLY at
`runs.document_base_commit`, the commit recorded once at run admission and inherited by
every child run; a run with no recorded commit has those declarations WITHHELD (E55.7 /
#3746, `backend/internal/repodoc/README.md`). The charter keeps its serve-time semantics
until E71.2 #3242, which should reuse that column.

**Review prompts consult the same seam, with the REVIEWED stage (#2797).** The three
in-process review builds — `runPlanReviews` (`plan_review`), `runImplementReviewsForTree`
(`implement_review`: the trace-time review, the fix-up re-review backstop and the decomposed
parent's consolidated review) and `runSupplementalReinvokeReview` — never pass through the
signed `/prompt` endpoint, so each resolves, attributes and renders through the shared core
`resolveDocumentSet` (`document_injection.go`) — the plan and implement sites via
`resolveReviewDocuments`, which also selects review conventions (see "Review-convention
consumer" below), the supplemental re-invoke via its no-convention wrapper
`resolveReviewInjectedDocuments`. The declaration seam is consulted with the stage under review
(the plan stage for `plan_review`, the implement stage for `implement_review`), loaded LAZILY —
only when the seam is configured, since the seam is its only consumer (#2797 item 3). The inert
signal is now TWO-PART on the review paths: `Config.DocumentDeclarations == nil` AND no selected
review convention returns before any read, the reviewed-stage load included; on the `/prompt`
endpoints, which never select a convention, it is still `DocumentDeclarations == nil` alone.
The review resolve phase — the seam call, the credential scope and every forge `Resolve` — is
bounded by `reviewDocumentResolveTimeout` (30s, #2797 item 1); the audit appends run on the
caller's context, and the `/prompt` endpoints get no deadline. The charter consumer stays out
of review prompts by construction: `stageRequiresCharter` is false for every implement stage and for a non-grooming
plan stage, and a grooming propose stage ships a `grooming_report`, which `handleShipPlan`
routes away BEFORE `runPlanReviews` runs. In `runImplementReviewsForTree` the resolution sits
AFTER the #797 idempotency guard (inside `reviewDispatchMu`), so a duplicate dispatch performs
no forge read, no attribution append and has no new failure path. **Fail-closed, and how to
read it.** A review whose declared documents cannot be resolved or attributed runs NO reviewer
and emits no `*_review_started`; it records `plan_review_failed` / `implement_review_failed`
with a reason prefixed `document_injection_failed`. Under ADVISORY authority the human gate
stays authoritative with that failed review visible. Under GATING authority the stage fails
category-B: the plan stage with the truthful reason `plan_review_document_injection_failed: …`,
but an implement stage through its callers' existing reject branch, so its `failure_reason`
reads `implement_review_rejected: agent review verdict reject under gating authority` **even
though no reviewer ran**. Operators must read the stage's `implement_review_failed` audit entry
— reason prefix `document_injection_failed` — to tell a document-injection failure from a real
reject verdict (a real reject has `implement_reviewed` entries and no such failed entry).
Tests: `TestShipPlan_PlanReview_CarriesInjectedDocument`,
`TestShipTrace_ImplementReview_CarriesInjectedDocument`,
`TestRunSupplementalReinvokeReview_CarriesInjectedDocument`,
`TestRunImplementReviews_DuplicateDispatch_NoDocumentResolution`, and the fail-closed set in
`plan_test.go` / `trace_test.go` / `document_injection_test.go`.

**Non-grooming prompts are byte-identical.** The declarations func returns zero
declarations for every other stage (and never touches the conventions loader), and
`resolveInjectedDocuments` short-circuits on an empty set. Because the fact decides, a
persisted non-grooming row is served byte-identically whatever its cached spec bytes
contain — including a syntactically corrupt spec carrying `grooming_report` in an inline
comment and an unrelated scalar (the #2805 residual (a), pinned by
`TestCharterDetermination_PersistedNonGrooming_CorruptSpecWithIncidentalToken_Served`) —
and a persisted grooming row is still refused without its charter even when a corruption
destroyed the token or the spec now parses as decidably non-grooming (residual (b),
`TestCharterDetermination_PersistedGrooming_SpecTokenDestroyed_StillAnchored`). The earlier
attributed byte-scanning fallback (`specCouldBeGrooming` and its seven helpers) existed only
to answer a question the prompt-serve path no longer asks, and is deleted; the corrupt-spec
corner in which H4 and AC5 contradicted each other does not exist for any row carrying a
determination.

**The legacy branch, and its bounded residual.** A row whose `requires_charter` is NULL
carries NO persisted determination: a row minted before migration 0082, or a child minted
via `ChildParamsFrom` from such a parent (children inherit the value verbatim, including
nil, because a child's spec IS the parent's spec). That population is bounded and
non-growing except by descent from legacy parents. For it the determination is derived
from the cached spec exactly as the parsed path always was — a spec that parses and names
the run's workflow decides via `WorkflowRequiresCharter` — and an UNDECIDABLE cached spec
is REFUSED with `grooming_workflow_spec_unreadable` rather than falling open: **empty**
(approval condition 1 — no fact and no bytes; a legacy grooming row whose spec was wiped
must not be served unanchored), **unparseable**, or **naming no such workflow**. Refusal is
the recoverable direction (repair the row, or start a new run, whose determination is
recorded at creation) and the class is reachable only by storage corruption of a row that
carries no fact. Stated honestly: for that narrow class this changes non-grooming prompt
behaviour (the pre-#2806 posture fell a corrupt token-free spec open); the reason VALUE is
unchanged, so no client contract moves.
`TestCharterDetermination_LegacyRow_ParseableOrdinarySpec_Served` is what falsifies a
refusal that ever widens beyond the undecidable case. A rollback of the column restores
spec-derived behaviour for every row and LOSES the corruption protection the fact adds.

**Preview convergence (E54.12 / #2804).** BOTH prompt handlers now run L1 and L2.
`handleGetStagePromptRender` resolves through `previewInjectedDocuments` — the same
resolve/render core as the served path, with attribution suppressed — and then calls the
same `assertCharterInjected`, using the same error code, message and details builder. So
the preview and the served prompt for the same stage carry IDENTICAL bytes and an
identical `prompt_hash`, and refuse identically (same HTTP status, same `error.code`, same
`error.details.reason`, and for a `repodoc.ResolveError` the same `path` /
`declaration_site` on the error the log record carries). `TestGroomingPrompt_PreviewMatchesServed`
compares the two RESPONSES rather than asserting each in isolation — AND every row declares
its own expected outcome (`wantStatus`, plus `wantCharter` on a 200 row) with the
success-vs-refusal branch selected by that declared outcome, never by the status the served
endpoint returned. Comparison alone is vacuous against a SHARED regression: two endpoints
that both stopped refusing agree with each other, and would also agree on the then-absent
charter block.

The ONE ratified divergence is ATTRIBUTION: the preview writes NO `document_injected` /
`document_truncated` / `document_injection_degraded` entries, while rendering the same
`WithheldNotice` the served prompt carries for withheld run-admission declarations (E55.7 /
#3746). A `document_injected` entry claims a revision CONSTRAINED AN
AGENT; a preview constrains none, and `/prompt-render` is an unsigned read-access GET the
SPA re-fetches on every session view, so attributing it would both falsify the claim and
let a pure read surface append unbounded chained rows to an append-only log. **Residual:**
the audit log answers *"which revision constrained the run"*, never *"which revision did
the operator preview"* — a preview is not reconstructible after the fact. Pinned by
`TestPreviewInjectedDocuments_WritesNoAttribution` (preview writes zero) paired with
`TestResolveInjectedDocuments_StillAttributes` (served still writes exactly one per
document).

Two costs the convergence buys, stated plainly: a grooming preview on a deployment whose
seam is unwired or whose charter is missing now returns 500 instead of a 200 carrying an
unanchored plan prompt (that IS the point), and a grooming preview now performs forge
calls — a default-branch resolution plus a file fetch at the pinned commit — on an
unsigned endpoint, so a repeatedly-refreshed session view consumes forge quota it
previously did not. No caching layer is added.

**Operator walk (live validation).** The three `requires_live_validation` criteria are not
provable from the test suite; run this against a deployment whose grooming workflow
declares a charter, with `$B` the backend base URL, `$S` a backlog-grooming PLAN stage id
and `$R` its run id.

1. `curl -s "$B/v0/stages/$S/prompt-render" > /tmp/preview.json` — **expect** HTTP 200 and
   a body whose `prompt` contains the heading `### Product charter — the prioritization
   anchor for this grooming run`.
2. `curl -s -H "X-Fishhawk-Signature: <sig>" "$B/v0/stages/$S/prompt" > /tmp/served.json` —
   **expect** HTTP 200 and the same charter heading.
3. `diff <(jq -r .prompt /tmp/preview.json) <(jq -r .prompt /tmp/served.json)` — **expect**
   NO output: the two prompt texts are byte-identical.
4. `jq -r .prompt_hash /tmp/preview.json /tmp/served.json` — **expect** two identical hex
   digests.
5. Rename or remove the charter at its declared path ON THE BASE REF (a commit to the
   default branch; the read is pinned to that branch's head commit), then re-issue steps 1
   and 2 — **expect** BOTH to return HTTP 500 with `error.code` =
   `document_injection_failed` and the SAME `error.details.reason` (`charter_absent` when
   the `charter.path` declaration is removed, or a `repodoc` missing-document refusal when
   only the file is gone). Compare the two `reason` values directly; they must be equal.
6. `curl -s "$B/v0/runs/$R/audit?limit=500" | jq -r '.items[] | select(.category=="document_injected") | .id'`
   — **expect** exactly as many `document_injected` entries as SIGNED `/prompt` fetches you
   made (one, from step 2), and none attributable to the step-1 preview. Re-issue step 1
   five more times and re-read: **expect** the count to be UNCHANGED, which is the
   attribution-suppression claim.

**Wiring.** `cmd/fishhawkd`'s `wireDocumentInjection` installs the four `Config` members
(resolver, scope, base ref, declarations) TOGETHER or leaves all four nil, because
`resolveInjectedDocuments` treats a configured declaration seam with a nil resolver as a
wiring defect and fails EVERY prompt request. Forge selection is github-then-gitlab, and
selection ALONE does not make a non-selected-forge repo fail: `forge.RepoRef` carries only
owner/name, so a gitlab-hosted `acme/widgets` whose owner/name also names an accessible
github repository would resolve, and the prompt would carry THAT repository's charter — a
cross-repository provenance failure, not the refusal the mixed-forge guarantee claims.
`documentForgeOwnershipGuard` enforces it, wrapping `DocumentScope` (through which the
credential scope, the default-branch lookup and every file fetch all pass, so a refusal
costs ZERO forge calls). It consults the same `accounts.provider` discriminator the
conventions loader and the repo-visibility cross-forge deny use: a resolved provider that
is not the selected forge REFUSES; a resolver error REFUSES; an AMBIGUOUS repo (no
resolver, or `found=false`) refuses only on a MIXED deployment, where a second forge could
own it — a single-forge deployment keeps the pre-guard posture rather than switching
document injection off wherever no accounts table exists.
`Server.CharterDocumentDeclarations` is the exported entry point the wiring needs
because `server.New` copies its `Config` by value, so the seam must be installed before
construction while the implementation needs the constructed Server.

Tests: `charter_injection_test.go` (cross-boundary end-to-end plus one behavioural test
per refusal branch, each asserting the reason IDENTITY so a deleted branch reddens rather
than passing on a neighbouring control; the `TestCharterDetermination_*` set — one test per
branch of `stageRequiresCharter`: persisted false + corrupt spec with an incidental token
SERVED, persisted true + token destroyed / unparseable spec still ANCHORED and refused for
the CHARTER reason not the spec, legacy NULL + empty / unparseable / unknown-workflow spec
REFUSED, legacy NULL + parseable spec decided as before, non-plan stage never grooming,
plus `TestCharterDetermination_DirectTable` over the decision order;
`TestCharterFixtures_HaveTheShapeTheirTestsNeed`, which pins that each corrupt fixture is
genuinely parser-rejected and the token-destroyed fixture parses as decidably
non-grooming; and `TestGroomingPrompt_L2Divergence_FailsClosed`, which drives a
conventions loader that answers differently within one request — L2 re-resolves
independently of L1 by design, so it must fail closed on a divergence),
`charter_determination_pg_test.go` (the layer-crossing seam against REAL Postgres: `POST
/v0/runs` → the persisted column → `GET /v0/runs/{id}` → a direct UPDATE corrupting the
stored spec → the prompt-serve verdict, for a grooming and an ordinary run),
`grooming_prompt_agreement_test.go` (every row carries an explicit persisted determination
or is a declared legacy row, and the served prompt's grooming-ness must equal
`stageRequiresCharter` on each), `cmd/fishhawkd/serve_test.go` (forge preference, the
all-four-or-none invariant, the base-ref adapter, and the cross-forge collision: ONE
owner/name registered on both forges, refused through the non-owning one with zero forge
calls). The counterfactual RED observations for each control are recorded at the top of
`charter_injection_test.go`.

## Review-convention consumer (`review_conventions.go`, E55.3 / #2244)

The THIRD consumer of `backend/internal/repodoc`, after the charter (above) and the persona remit
(below). It renders the workflow-selected `review_conventions` (ADR-068; grammar in
`docs/spec/workflow-v2.md` § Review conventions) into the plan-review and implement-review prompts
as a fenced, explicitly SUBORDINATE supplemental section, and enforces the declaration
SERVER-SIDE — a severity clamp at ingest, a once-per-round synthesized concern, a fix-up refusal —
rather than by model compliance. Split by file: `review_conventions.go` (selection and every pure
helper — no I/O, no audit write), `document_injection.go` (resolution through the shared core),
`plan.go` / `trace.go` (ingest), `fixup.go` (refusal). The prompt half is
`backend/internal/prompt/README.md` § "Repository review conventions"; the verdict half (concern
categories, `ClampConventionSeverities`, `AddSynthesizedConventionsFileModifiedConcern`, payload
fields) is `backend/internal/planreview/README.md` § "Review-convention verdict contract".

**Selection — ONE admission change at BOTH sites.** `reviewConventionChange(runRow, paths)` is
`runAdmissionChange(runRow)` — the SAME trigger-form + issue-label change the plan gate's
`applies_to` evaluation uses (`applies_to_plan_gate.go`, read-only reuse; labels come from the
run's `IssueContext` snapshot, a nil context is an empty label set) — with only `Paths` set per
site:

| Site | Paths | Selected from |
|---|---|---|
| plan review (`runPlanReviews`) | `planGateScopePaths(plan)` — the `scope.files` union incl. decomposition phase scopes | the run's FIRST `plan` stage |
| implement review (`runImplementReviewsForTree`) | `implementReviewPaths(diff)` — every changed `Path` plus each rename/copy `OldPath`, slash-normalized, deduped, sorted; the FULL diff, not the #1725 fix-up delta | the run's FIRST `implement` stage |

So a convention whose `applies_to` names only a label or only a trigger form selects at both sites
or at neither; a path-only `applies_to` selects at whichever site's paths match. A file's PRE-move
name still matches, so a rename does not evade a convention. `selectReviewConventions` reads the
run's workflow-spec SNAPSHOT (the bytes admission parsed). "Nothing declared" (no snapshot, the
workflow absent from it, no stage of the type) is `(zero, nil)`; an unparseable snapshot, a
selection naming an undeclared entry, a stage type that may not select, or an `applies_to` Match
error FAILS CLOSED — a governance input is never read as "no convention". **Residual, stated:** the
stage is located FIRST-OF-TYPE, as `resolveStageReviewers` locates the standard reviewers' config —
not by the reviewed stage's type ordinal as personas are (`specStageForRunStage`) — so a workflow
with two same-type stages selecting different conventions applies the first stage's selection to
both. Tests: `TestReviewConventionSelection_LabelOrTriggerOnlySelectsAtBothSites`,
`TestReviewConventionSelection_PathOnly`, `TestImplementReviewPaths_IncludesOldPathDeduped`,
`TestSelectReviewConventions_Degrades`.

**Review-only routing.** Conventions are selected ONLY by `resolveReviewDocuments` (plan and
implement review) and come back in `reviewDocuments.Conventions` → `Trigger.ReviewConventions`,
NEVER in `Injected`. The `/prompt` and `/prompt-render` endpoints select none, so no author prompt
(plan, implement, fix-up, acceptance) can carry one — `TestGetStagePrompt_ConventionSelectingSpec_AuthorPromptCarriesNoConventions`;
the supplemental re-invoke resolves through the no-convention `resolveReviewInjectedDocuments` and
renders none. An attached reviewer persona's prompt is a VALUE COPY of the round's trigger, so it
carries the same conventions section and its verdict passes through the same ingest clamp and is an
eligible synthesis carrier.

**Resolution — required, optional, withheld.** Each selected convention becomes a
`repodoc.Declaration` (`conventionDeclaration`) with `Base: BaseSourceRunAdmission` and declaration
site `review_conventions.<name> in .fishhawk/workflows.yaml`, appended AFTER the seam's
declarations, so it rides the same base-source partition and the same
resolve-everything-before-any-attribution loop (`resolveDocumentSet`):

| Case | Outcome | Audit | Test |
|---|---|---|---|
| resolved at the admission commit | rendered in the supplemental section; its cap enters the round's clamp caps | `document_injected` naming `review_conventions.<name>`, `base_source: run_admission` | `TestResolveReviewDocuments_ConventionResolvedReviewOnly` |
| run recorded NO admission commit | WITHHELD, nothing fetched, `WithheldNotice` in `Injected`; the review proceeds — "required fails" applies only when a base exists (E55.7) | one `document_injection_degraded`, `run_base_commit_unrecorded` | `TestResolveReviewDocuments_NoAdmissionCommit_Withheld` |
| REQUIRED (`required` absent or `true`) and absent | the whole set fails: `review_convention_missing` naming name, path, site and commit; no reviewer runs | zero `document_injected`, even for an already-resolved seam document; `*_review_failed` reason `document_injection_failed: review_convention_missing: …` | `TestResolveReviewDocuments_RequiredConventionMissing_NoClaim`, `TestRunPlanReviews_RequiredConventionMissing_GatingFailsStage`, `TestRunImplementReviews_RequiredConventionMissing_FailsLoudly` |
| OPTIONAL (`required: false`) and absent | skipped, renders nothing, excluded from the caps | `document_injection_degraded`, reason `optional_document_missing`, written BEFORE any `document_injected` | `TestResolveReviewDocuments_ConventionResolvedReviewOnly` |
| selected with a nil `DocumentResolver` — even with no declaration seam | fails closed as a misconfiguration | none | `TestResolveReviewDocuments_SelectionWithNilResolver_FailsClosed` |
| any other resolve error, incl. the resolve deadline | fails closed | none | `TestResolveReviewDocuments_ResolveDeadlineExceeded_FailsClosed` |

A failure takes the existing review-side fail-closed branch above: gating plan review fails the
stage category-B (`plan_review_document_injection_failed`); gating implement review fails it through
the reject branch (read the `implement_review_failed` entry to tell it from a real reject); advisory
leaves the human gate authoritative. **Production wiring consequence, intended:**
`cmd/fishhawkd/serve.go` wires `DocumentResolver` only when a file-capable forge exists, so on a
deployment without one a repository that SELECTS conventions has its reviews fail closed instead of
running without them — partial configuration is a failure (the #2242 rule).

**Lazy stage load, bounded resolve phase, and the dispatch-lock hold bound (#2797 items 1–3).**
`resolveDocumentSet`'s inert check — no declaration seam AND no selected convention — runs FIRST and
reads nothing, the reviewed stage included; the stage is loaded only when the seam is configured
(the seam is its only consumer; attribution keys on `stageID`), and still fails closed when it
cannot be loaded (`TestResolveReviewDocuments_NilSeamNoSelection_UnloadableStage_Inert`,
`TestRunPlanReviews_ReviewedStageUnloadable_FailsClosed`). The seam call, `DocumentScope` and every
`Resolve` run under `const reviewDocumentResolveTimeout = 30 * time.Second`; `RecordWithheld` /
`Attribute` run on the CALLER's context, so an expiring deadline cannot fail a half-written
attribution set (`TestResolveReviewDocuments_ResolvePhaseBounded`, which first asserts its caller
context carries no deadline, and `TestRunImplementReviews_DocumentResolutionBounded` at the trace
site). At the implement site the resolution sits INSIDE `reviewDispatchMu`, AFTER the #797
`(stage_id, head_sha)` guard, so a duplicate dispatch selects, fetches and attributes nothing
(`TestRunImplementReviews_DuplicateDispatch_NoConventionResolution`). The section's worst-case hold
for this work is the resolve bound plus the attribution appends plus the prompt build; every exit —
duplicate return, resolution failure, prompt-build failure (through the `buildImplementReviewPrompt`
test seam), started emit — unlocks (`TestRunImplementReviews_PromptBuildFailure_ReleasesDispatchLock`).
Only runs that declare documents or select conventions pay the bound. **Residuals, stated:** (a) a
hung forge still holds `reviewDispatchMu` for up to the bound per dispatch; (b) an attached
persona's remit `Resolve` (`buildPersonaPrompts`) ALSO runs inside the section and is NOT under
`reviewDocumentResolveTimeout`, so a run attaching a persona can hold the lock past that bound — the
bound covers the declared-document and convention resolve only.

**Ingest clamp.** Both review loops — `runPlanReviewLoopWithConventions` and
`runImplementReviewInvocationsWithConventions` — pass every successful verdict through
`planreview.ClampConventionSeverities` (the implement loop: `ClampConventionSeveritiesForRound`) against the round's caps (`conventionCapsFor`: the RENDERED
conventions only — a withheld or optional-missing convention was never shown to a reviewer)
IMMEDIATELY after the reviewer returns and BEFORE the payload, the gating decision or
`persistReviewConcerns` reads it. A `repo_convention` concern above its convention's cap is lowered
and carries `severity_clamped_from`; an empty or unrendered `convention` name fails closed to the
most restrictive rendered cap; a `reject` resting only on clamped highs is recorded as
`approve_with_concerns` with `verdict_clamped_from: reject` and does not gate. **With ZERO
conventions rendered** (nil caps — the zero `reviewConventionRound` every no-conventions round and
both legacy loop entry points pass), every `repo_convention`, `conventions_override_attempt` and
`conventions_file_modified` concern a reviewer emits anyway is clamped to `low`, never left
unclamped — EXCEPT a `conventions_file_modified` concern on an implement round whose diff modifies a
declared conventions file (`Round.ModifiedFiles` non-empty): the server observed that modification
itself, so the reviewer's concern keeps its weight (E55.10 / #3755, the #2244 seam;
`clampImplementReviewVerdict` passes `len(conv.ModifiedFiles) > 0`). Every lowered concern is WARN-logged by `logConventionClamps` with `run_id`, `stage_id`,
`reviewer_index`, `category`, `convention`, `from`, `to` and `no_conventions_rendered`. Standard
categories are untouched, so a reject resting on a standard high still gates. Tests:
`TestRunPlanReviews_ReviewConventions_EndToEnd`, `TestRunPlanReviews_ReviewConventions_UncappedRejectStillGates`,
`TestPlanReviewLoop_NoConventionsRendered_ClampsConventionCategoriesToLow`,
`TestRunImplementReviews_ReviewConventions_EndToEnd`,
`TestRunImplementReviews_NoConventionsRendered_ConventionConcernClampedToLow`.

**Once-per-round `conventions_file_modified` synthesis and the approve upgrade (implement review
only).** `Round.ModifiedFiles` is the path of EVERY declared `review_conventions` entry — selected
this round or not — that `implementReviewPaths(diff)` touches (an `OldPath` counts, so moving the
file away is detected). Non-empty, it renders a machine-verified notice after
`ImplementReviewSplitMarker` and switches the ingest from streaming to HELD: every invocation is
invoked, then every verdict clamped, then synthesis is decided ONCE —
`conventionsFileModifiedCarrier` picks the FIRST successful verdict when no verdict of the round
already carries a `conventions_file_modified` concern AND the stage holds none in an open, waived or
deferred state from an earlier round (`conventionsFileModifiedAlreadyRaised` over
`ConcernRepo.ListByRun`; an addressed or superseded row does not suppress a re-raise; a nil repo or
a read error falls OPEN to synthesizing — a duplicate is noise, a missed flag is silent).
`AddSynthesizedConventionsFileModifiedConcern` appends ONE medium concern naming the paths to that
verdict alone; a plain `approve` is raised to `approve_with_concerns` (`verdict_raised_from:
approve`) and the carrying payload sets `conventions_file_modified_synthesized: true`. Synthesis runs
AFTER the clamp, so the zero-caps rule never lowers the server's own concern. Then every held result
is ingested in invocation order: audit row ORDER is unchanged, only the moment the first verdict
becomes visible moves. An all-failed round has no carrier and synthesizes nothing; the next round
re-evaluates. An `approve_with_concerns` does not gate, so the stage is not failed — the concern is
an open obligation on the human gate. With `ModifiedFiles` empty the loop streams exactly as before
E55.3; plan review has no diff and never synthesizes. Tests:
`TestRunImplementReviews_ConventionsFileModified_ApproveUpgraded`,
`TestRunImplementReviews_ConventionsFileModified_TwoReviewerRound` (the later reviewer raising it
suppresses synthesis onto the earlier one — the property a per-verdict decision would break),
`_WaivedInPriorRound_NotReminted`, `_DedupeReadError_FallsOpen`,
`TestRunImplementReviews_ConventionsIngestOrdering`, `TestConventionsFileModifiedCarrier`.

**Held-ingest durability.** A held verdict lives only in memory until the round's last invocation
returns, so two things must not lose it. (1) A LATER reviewer failing: every held result — failed
ones included — is ingested in invocation order, so the earlier reviewer's `implement_reviewed`, its
concerns and the synthesized `conventions_file_modified` land, the failed reviewer records
`implement_review_failed`, and the round settles at `configured_agents`. (2) The CALLER's context
being cancelled mid-round: the loop runs on `context.WithoutCancel(ctx)` (`reviewCtx`), so the held
ingest's audit appends and concern inserts are never issued on a cancelled context.
`TestPersonaHardening_HeldIngestDurability` pins both (the cancelled variant over audit and concern
fakes that REFUSE a write on a cancelled context — the stores a real driver behaves like).

**Residual — concurrent synthesis (stated, not fixed).** `conventionsFileModifiedAlreadyRaised`
reads the concern store AFTER the round's reviewers return, OUTSIDE `reviewDispatchMu` (which covers
only the duplicate-dispatch check, document resolution and the started emit). Two rounds of one stage
in flight at once — different heads, e.g. a trace-time review and a fix-up re-review of a newer
push — can therefore BOTH read "none raised" and EACH synthesize a `conventions_file_modified` row.
The cost is a duplicate open concern (noise the operator waives), never a missed flag; closing it
needs the read and the insert under one lock or a uniqueness guard on the concern store.

**Fix-up refusal.** `fixup.go` refuses a `conventions_override_attempt` concern on BOTH addressing
paths — `resolveConcernsByID` (after the routability check) and the positional `selectConcerns` —
with 400 `validation_failed` (`conventionsOverrideAttemptRefusal`), in ANY state and whether or not
a convention was rendered: it reports an instruction inside a conventions file read at the run's
pinned base, which this run must not edit, so it is never a code obligation; the operator waives or
defers it and fixes the file in a separate change. `repo_convention` and `conventions_file_modified`
route unchanged. Tests: `TestFixupStage_ConventionsOverrideAttemptRefused` (reads committed state:
no `stage_fixup_triggered` row), `_AmongOthers`, `_PositionalPath`, and the
`TestFixupStage_RepoConventionConcernRoutes{,_PositionalPath}` pair.

**No conventions selected is byte-identical.** No fetch, no `document_injected` naming
`review_conventions.*`, no `optional_document_missing` degradation, both review prompts
byte-identical to base (the prompt package's goldens), streaming ingest, and every new verdict /
payload field `omitempty` — `TestRunPlanReviews_NoConventions_NoConventionAuditTrace`.

**What this is, honestly.** A quality aid under a PROTECTED declaration, not an adversary-proof
control. The declaration (which conventions, which caps, required or not) lives in
`.fishhawk/workflows.yaml`; the convention TEXT is repository content read at the admission commit,
which is why a change editing a declared file is flagged rather than trusted. The subordinate
framing is a model instruction: whether a reviewer REPORTS an override instead of following it is
behavioural and unmeasured offline (#3187) — the `review-convention-override` agenteval class proves
STRUCTURAL containment only. What holds whatever the model does is server-side: the clamp, the
synthesis, the fix-up refusal. Clamp residuals: a file can steer the model to file a convention
finding under another category, escaping the clamp; a concern naming a different, uncapped rendered
convention is uncapped. Plan-review placement sits OUTSIDE the plan-review cached prefix (after
`PlanReviewSplitMarker`, because every plan-review criterion follows it) — the stated AC5 deviation
in `backend/internal/prompt/README.md`.

## Reviewer personas (`reviewer_persona.go`, ADR-084 #3752 / E55.8 #3753)

ADR-084 binding rule 1: "A named persona declares a model configuration and a remit document." The grammar
(`reviewer_personas` + per-stage `reviewers.personas`) and its validation live in
`backend/internal/spec/reviewer_personas.go`; this package runs them. Each attached persona is ONE EXTRA
invocation of the existing in-process loops — `runPlanReviews` / `runPlanReviewLoop` (`plan.go`) and
`runImplementReviewsForTree` / `runImplementReviewInvocations` (`trace.go`) — carried on `reviewerInvocation.persona`,
so the loop signatures are unchanged.

The PURE half — the remit framing, the persona prompt render (`personaprompt.Build`), the quote-document set, the
change-path union and the quote-verification + severity-cap ingest controls (`personaprompt.ApplyConcernControls`)
— lives in `backend/internal/personaprompt` (#4313; contract: its `README.md`), shared with the agenteval persona
catch arm. This package keeps the forge reads, the build-then-attribute order, every degrade detail, the WARN logs
(driven from the returned results), the ingest-marker scrub and the `reviewer_role` stamp; the wrappers
`personaRemitFraming`, `reviewChangePaths` and `personaPromptDocuments` delegate with their original signatures.

- **Per-invocation prompt; standard prompt byte-identical.** The standard prompt is built exactly as before. AFTER
  it, `buildPersonaPrompts` builds each persona's OWN prompt from a VALUE COPY of the round's `prompt.Trigger` with
  the remit appended to a CLONED `InjectedDocuments` slice, so nothing it writes can reach the standard prompt.
  `promptFor` hands each invocation its own prompt and review tree; the size-aware budget is computed per
  invocation (identical for a standard reviewer). Golden tests: `TestPlanReview_Persona_GoldenStandardPromptAndOwnPersonaPrompt`,
  `TestImplementReview_Persona_GoldenStandardPromptAndOwnPersonaPrompt`. Because the copy KEEPS the round's
  `InjectedDocuments` (the standard set, then the remit) and `ReviewConventions`, a persona prompt carries every
  document the standard reviewers see — the declared stage-injected documents and the selected review conventions
  — before its remit: `TestPersonaHardening_{Plan,Implement}_PersonaPromptKeepsStandardDocuments`.
- **Stage lookup.** Personas resolve from the stage ACTUALLY under review: the reviewed `run.Stage` is loaded by
  stage id (as `resolveReviewInjectedDocuments` does) and mapped to its spec stage with `specStageForRunStage` (type
  ordinal — run rows carry no spec stage id). Never first-of-type. The STANDARD reviewers' config is still read
  first-of-type by `resolveStageReviewers`, unchanged. A spec declaring no `reviewer_personas` short-circuits
  before any repo read, and the stage is located only when some stage of the workflow attaches a persona
  statically. `TestPersona_ResolvesFromReviewedStageNotFirstOfType`.
- **Escalation-attached personas (`escalation_persona.go`, ADR-084 D2(c) / E55.9 #3754).** The round's persona
  SET is the reviewed stage's static `reviewers.personas` UNION the personas named by the `require.reviewers` of
  every escalation that FIRED for the change, de-duplicated (static first in attachment order, then escalated by
  name; a persona in both runs ONCE). Plan review matches the scope union of the plan UNDER REVIEW
  (`path_source: plan_scope`); implement review matches the APPROVED plan's scope union UNION
  `diffReviewPaths(diff, evalDiff)` — the pass diff and the stage-cumulative evaluation diff, rename/copy
  `OldPath`s included (`path_source: approved_plan_scope_and_diff`) — so scope drift into a sensitive path still
  attaches. Labels/trigger are `escalationAdmissionChange` (the approval/delegation seams' snapshot). A workflow
  whose escalations declare no `require.reviewers` evaluates nothing; a non-firing escalation attaches nothing.
  Each attaching round writes ONE best-effort `escalation_persona_attached` entry on the reviewed stage:
  `{review_kind, stage_id, path_source, head_sha?, attachments: [{persona, fired, fired_keys, via_diff_only,
  also_static?}]}` (`fired`/`fired_keys` index-aligned like `escalation_fired`'s; `via_diff_only` = the plan scope
  alone fires no rule naming it). `also_static` has THREE readings (E55.10 / #3755): `true` — the reviewed stage
  also attaches the persona statically (it still runs once); `false` — the static set resolved and does not
  attach it; key ABSENT — the static source degraded (`persona_stage_unresolvable`), so it is unknown and the
  payload does not claim `false`. Decide on the raw JSON: absent and `false` are distinct states
  (`TestEscalationPersonaAttached_AlsoStaticAbsentWhenStaticDegraded`). INTERNAL — not an issue-thread activity line. Tests: `escalation_persona_test.go`.
- **Unresolvable attachment set (carried from #3753).** When the spec declares `reviewer_personas` but a source
  cannot be resolved, the round no longer silently runs no persona: ONE pseudo invocation per failed source
  records a terminal `*_review_skipped` with `reason: persona_attachment_unresolvable` (no `persona`) and
  `detail: persona_stage_unresolvable` (workflow absent, reviewed stage unloadable / stages unlistable / stage
  absent or unmappable, `SelectReviewerPersonas` refusal) or `escalation_unevaluable` (a `Match` error, a
  `SelectNamedReviewerPersonas` refusal). It is COUNTED in `configured_agents` and emitted after `*_review_started`,
  so `planreview.Settled` still waits for every standard reviewer; `hasRejection` is untouched; the other source's
  personas still run. The `escalation_unevaluable` skip is escalation-attached and, under GATING authority, fails
  the stage at the dispatch site (#3913, next bullet); the `persona_stage_unresolvable` skip never does.
  `TestResolveStageReviewerPersonas_FailurePaths`, `TestResolveEscalationPersonas_MatchErrorDegrades`;
  through the implement invocation loop, including the two mixed degrades (static degraded while the escalated
  persona runs, and the reverse): `TestPersonaHardening_EscalationUnevaluable_ThroughImplementLoop`,
  `TestPersonaHardening_Mixed_*`. `escalation_unevaluable` is unreachable from a stored spec — `ParseBytes` rejects
  both of its producers (a malformed glob, an undeclared `require.reviewers` name) — so those cases build the
  degraded spec by construction and hand it to the same `resolveParsedReviewPersonaInvocations` the dispatch site
  calls; it remains a defence for a spec that skipped validation.
- **Escalation-attached persona blocks a gating round ([#3913](https://github.com/kuhlman-labs/fishhawk/issues/3913)).**
  A persona an escalation attached exists because the change touches a path the workflow marked sensitive, so
  under GATING authority one that cannot run fails the reviewed stage category-B instead of letting the round
  settle on the standard reviewers alone. "Cannot run" is `reviewerInvocation.cannotRun()` — `resolveErr != nil`
  (the persona's provider is not runnable: `reviewer_unavailable`; `agent.optional: true` does NOT exempt it,
  because an escalation may only raise) or a persona with no prompt (`persona_remit_unavailable`, any remit or
  `decision_record` detail, or the `escalation_unevaluable` pseudo invocation) — the ONE predicate both loops skip
  on. "Escalation-attached" (`personaInvocation.escalation`) is set by MEMBERSHIP in the escalation resolution's
  selected names, not by position, so a persona attached both statically and by a fired escalation (de-duplicated
  into the static slot) is still tagged; the `escalation_unevaluable` pseudo invocation is tagged too.
  `persona_stage_unresolvable` stays non-blocking because it is static-source by construction (the reviewed stage
  could not be located, so the static attachment set is unknown), while an escalation-selected persona still
  joins the union and is tagged by membership — a deliberate reading of the operator decision the operator may
  overrule. The block is decided at the TWO dispatch sites that already fail a stage on a gating reject —
  `runPlanReviews` (via `planReviewGatingFailureReason`) and the trace-upload caller of
  `runImplementReviewsForTree` (which now RETURNS the failure reason, `""` = not blocked; `runImplementReviews`
  keeps its bool) — from `escalationPersonaGateBlock`, so the loops' `hasRejection` keeps its verdict-only meaning.
  Every reviewer still runs first, so the round settles at `configured_agents`. Reason:
  `plan_review_rejected: escalation_persona_unavailable: persona "<name>" (<reason>: <detail>), ... could not run
  under gating authority; ...` (implement: the `implement_review_rejected` prefix; the pseudo invocation renders
  `escalation source (persona_attachment_unresolvable: escalation_unevaluable)`). The `*_rejected` prefix is kept
  although no reviewer rejected because `handleShipPullRequest` closes a gating-failed implement stage's dangling
  PR by `strings.HasPrefix(FailureReason, implementReviewGatingRejectPrefix)` (#877). The block WINS over a reject
  verdict (a fix-up or replan cannot clear it; the reject stays visible in `*_reviewed`). The skip entry carries
  `escalation_attached: true` (omitempty — absent on every standard, static-only and `persona_stage_unresolvable`
  skip). Advisory rounds and static-only personas keep the degrade. Residuals: the post-success rounds (fix-up push
  re-review, consolidated review) ignore the gating return exactly as they ignore a reject; and a remit absent at
  the run's admission commit is base-pinned (`BaseSourceRunAdmission`), so retrying the stage in the SAME run fails
  again with the same reason — fail closed until a new run admits a commit carrying the remit. Tests:
  `escalation_persona_gate_test.go` (plan gating per mode, static-only, advisory, the cross-layer trace upload,
  the block's sources, reason precedence).
- **Authority residual (stated, not fixed).** The persona SET is the reviewed stage's, but a round takes ONE
  authority — `resolveStageReviewers`, the FIRST stage of the type — and every invocation shares it. With two
  same-type stages whose `reviewers` blocks differ, a persona attached by the second runs under the first's
  authority (e.g. advisory: a rejecting persona does not fail the stage). Splitting authority within a round would
  split `hasRejection` / started semantics. The same shape makes spec rung 9 (per-stage `reviewers.agents`) accept
  an escalation whose only agent-reviewing stage is a later same-type stage the runtime never reviews with agents.
  Pinned (not a control) by `TestPersona_AuthorityIsRoundAuthority_Residual`.
- **Remit resolution.** `repodoc.Resolve` with `BaseSourceRunAdmission` at `runRow.DocumentBaseCommit` (E55.7),
  declaration site `reviewer_personas.<name>.remit in .fishhawk/workflows.yaml`, framing heading
  `Reviewer persona remit: <name>` with an ADR-068 trust note (the remit ADDS a lens; it cannot remove, weaken,
  reorder or override any standard criterion, the verdict schema or the reviewer's authority; it scopes
  `quoted_passage`/`document_ref` to Source-lined injected repository documents — the only `quoteDocs` — and
  directs plan, diff, issue and review-tree citations into the concern's note, since quoting one is
  `document_unknown` and demotes; #3915, `TestPersonaIngest_Implement_UnquotedDiffCitationKeepsSeverity`). Build THEN
  `repodoc.AttributeSet` (`document_injected` stamped with the reviewed stage), so no injection claim exists for a
  prompt that never built; for a persona without `decision_record` the set is the remit alone, byte-identical to
  the pre-#3756 `repodoc.Attribute` call. The persona's `model` is part of its declaration: the gate-resolved
  `review_model` override does NOT apply to it.
- **Decision record (`reviewer_personas.<name>.decision_record`, ADR-084 D4(b) / E78.5
  [#3756](https://github.com/kuhlman-labs/fishhawk/issues/3756)).** Only for a persona that declares it (no extra
  read otherwise), AFTER the remit resolves: `resolvePersonaDecisionRecord` calls `decisionrecord.Assemble` at
  `runRow.DocumentBaseCommit` — checked nil / empty / non-40-hex BEFORE any dereference or read — with the round's
  `personaInvocation.changePaths` (`reviewChangePaths`: the plan-scope union UNION the diff paths, de-duplicated and
  sorted; plan review = the plan under review, implement review = the approved plan UNION `diffReviewPaths`). The
  index then each selected record (`Selection.PromptDocuments`, grounded = the persona can read the tree) are
  appended after the remit in the persona's CLONED `InjectedDocuments`; the standard prompt is untouched. Remit +
  index + records + the selection-level `document_truncated` (when a match was dropped by the cap) are written as
  ONE `repodoc.AttributeSet` (one `injection_set_id`; every `document_truncated` before any `document_injected`);
  a failure is `remit_unattributed` and the prompt is discarded. The selection's `cap_bytes` is the record budget
  (the resolver's cap, counting only the shown records' rendered bytes, whole bodies and Decision-section excerpts
  (#4489) — the index is shaped under its own cap, #4399); the remit and the fixed framing are outside it (contract:
  `backend/internal/decisionrecord/README.md`). The records join `quoteDocs`, so E55.10 quote verification covers
  them. Tests: `decision_record_persona_test.go` (the index-near-cap case,
  `TestDecisionRecordPersona_IndexNearCap_RecordsStillShown`, pins that the index cannot starve the records).
- **Degrade = this persona only.** A persona whose remit cannot be used never runs and records a terminal
  `*_review_skipped` with `reason: persona_remit_unavailable`, `persona`, `provider` and a `detail`:
  `document_resolver_unconfigured`, `run_base_commit_unrecorded`, `remit_missing`, `remit_unresolvable`,
  `persona_prompt_build_failed`, `remit_unattributed`, and for a `decision_record` persona
  `decision_record_index_missing` (`decisionrecord.ErrIndexMissing`), `decision_record_invalid`
  (`ErrInvalidIndex`) or `decision_record_unresolvable` (a listed record absent at the commit, any other assembly
  error, or an unusable admission commit — reachable only by a direct call, since the remit's own
  `run_base_commit_unrecorded` / `remit_unresolvable` fire first in the loops). The standard reviewers still run; `hasRejection` is
  untouched. A persona whose PROVIDER is unavailable takes the ordinary `reviewer_unavailable` skip stamped with
  `persona`, and its remit is never read. Exception: an ESCALATION-attached persona that cannot run fails a gating
  round (#3913, the bullet above); a static-only one never does. A persona reviewer that errors records `*_review_failed` with a reason
  prefixed `persona <name>: `.
- **Accounting.** `*_review_started.configured_agents` = standard count + persona count, and `personas` lists
  them. Terminal entries are matched by COUNT (`planreview.Settled`), never by provider or provider+persona, so a
  persona sharing the standard reviewer's provider (even the same adapter) settles at configured_agents with no
  dedupe and no double-count. Persona verdicts stamp `persona` (omitempty: standard payloads byte-identical), join
  the round's concern persistence and delta-verification buffer, and inherit stage authority (ADR-084 rule 3): a
  persona reject under gating fails the stage category-B. Residual: surfaces that fall back to the spec's
  `AgentCount()` before a round has started under-count personas until the started entry exists.
- **Grounding.** `allInvocationsGrounded` is evaluated over the STANDARD invocations only, before personas join,
  so a persona's capability can never change the standard prompt. A persona that cannot ground (or a round with
  no exported tree) gets the diff-only clause and no tree. `TestPlanReview_Persona_GroundingIsolation`.
  All three call sites (`runPlanReviews` in `plan.go`; `runImplementReviews` and `runSupplementalReinvokeReview`
  in `trace.go`, the last since #4160) decide grounding through ONE helper, `groundReview`
  (`review_grounding.go`), which stamps `Trigger.ReviewUngroundedReason` on every degrade (#4066), so the
  ungrounded prompt renders one of two variants: switch-off (`disabled`, naming `FISHHAWKD_REVIEW_GROUNDING`) or
  enabled-but-unavailable with the named reason (`no_working_dir`, `no_ref`, `reviewer_cannot_ground`,
  `ref_unavailable`, `export_failed`). A non-grounding persona on a GROUNDED round is stamped
  `reviewer_cannot_ground`; on an ungrounded round the round's reason carries over
  (`TestPersona_NonGroundingPersonaNamesReviewerCannotGround`). The supplemental base-rebase re-invoke pass is
  grounded against the pushed re-landed head (`pr.HeadSHA`), taken after every early exit, with the
  `runImplementReviews` cleanup ownership (goroutine defer, synchronous defer, build-error cleanup); its render
  carries REPOSITORY ACCESS (`buildImplementReview` writes it before the supplemental early return) plus one
  grounded-only framing sentence (#4160; `TestSupplementalReinvokeReview_*` in `review_grounding_test.go`,
  `TestBuild_ImplementReview_SupplementalReinvoke_RendersGroundingPosture`).
- **Implement-path ordering.** Persona resolution sits inside `reviewDispatchMu`, AFTER the #797 duplicate-dispatch
  guard and the standard build, BEFORE `implement_review_started` — a duplicate dispatch reads no remit and writes
  no attribution (`TestImplementReview_Persona_DuplicateDispatchReadsNothing`). `runSupplementalReinvokeReview`
  runs standard reviewers only — escalation-attached personas inherit that gap.
- **Persona concern ingest (`persona_concern_ingest.go`, ADR-084 D3 / rule 3, E55.10 #3755).** After the convention
  clamp, `applyPersonaConcernControls` runs on EVERY successful verdict of both loops, before its payload, the gating
  decision or concern persistence reads it, in a load-bearing order:
  1. **Scrub.** `planreview.ClearPersonaIngestMarkers` zeroes the four server-internal concern fields
     (`quote_unverified`, `quote_verified_content_hash`, `persona_severity_cap`, `reviewer_role`) on every verdict,
     standard included — a reviewer on the unconstrained decode path cannot pre-set them.
  2. **Quote verification (persona only).** A concern with a non-blank `quoted_passage` is checked against the
     EXACT text injected into THAT persona's prompt (`personaInvocation.quoteDocs`: the standard injected documents,
     the remit, for a `decision_record` persona the index and each selected record, then each rendered review
     convention; text via `repodoc.InjectedContent`, i.e. the
     delimiter-bracketed neutralized body the reviewer saw, never the server's framing). Match = whitespace-collapsed,
     case-sensitive substring of the document `document_ref` names (leading `./` stripped). Verified → stamps the
     document's `content_hash` as `quote_verified_content_hash` (the same hash its `document_injected` row
     recorded). Unverified → `quote_unverified`, severity lowered to `low` with `severity_clamped_from`; the failure
     mode is WARN-logged: `document_ref_missing`, `document_unknown` (a document injected into a DIFFERENT invocation,
     or withheld, is unknown by design), `document_text_unavailable`, `passage_not_found`.
  3. **Cap (persona only).** `planreview.ClampPersonaSeverities` lowers a concern above the remit's `severity_cap`
     to the cap (`severity_clamped_from`, `persona_severity_cap`); `""` = uncapped.
  4. **Reject downgrade.** A persona `reject` resting only on lowered highs becomes `approve_with_concerns`
     (`verdict_clamped_from: reject`; first non-empty of the convention and persona clamps wins) and does not gate.
  5. **Attribution.** Every concern is stamped with the invocation's `reviewer_role` — the persona name or
     `standard` — AFTER the scrub, so the `*_reviewed` payload and the persisted row (`persistReviewConcernsAs`)
     agree; a synthesized `conventions_file_modified` concern takes its carrier's role, including a persona carrier
     when the standard reviewer failed (payload and row, #3915:
     `TestPersonaHardening_HeldRoundPersonaCarriesSynthesizedConcern`). A row's `reviewer_role` is `''` for an
     unattributed legacy or operator-authored row.

  A standard verdict gets only the scrub and the `standard` stamp, so a compliant standard verdict is unchanged.
  No new authority mode (ADR-084 rule 4): persona concerns enter the existing concern state machine. **Veto
  identity** for implement-round delta verification is `reviewerIdentity{model, role}` with the role normalized
  (`''` reads as `standard`): a persona on the standard reviewer's model can no longer confirm-retire the standard
  reviewer's concern while that reviewer rejects the round; `concern_resolution_vetoed` carries
  `raising_reviewer_role` / `confirming_reviewer_role`. Operator surfaces (gate view, run status, MCP mirrors, the
  `stage_fixup_triggered` concern set and the fix-up prompt line `[severity/category · persona <name>] note`, plus
  `· quote unverified`) show the attribution. **Residual:** a very short quote (one common word) verifies
  trivially — the control bounds FABRICATED passages, not trivially-true ones. Tests: `persona_concern_ingest_test.go`,
  `persona_concern_surfaces_test.go`.
- **E63 obligation (ADR-084 rule 6).** Personas run in-process here today. When reviewers move to the runner
  (E63 #2307), runner-hosted reviewers MUST carry personas with the same contract — own prompt, standard prompt
  byte-identical, per-persona fail-closed degrade, configured_agents accounting — or the move silently drops a
  declared control.

## Plan-gate scope/constraint pre-check (#658)

`scope_precheck.go::runScopePrecheck` — called from `handleShipPlan` (`plan.go`) right after the `plan_generated` audit append and before `runPlanReviews`.

Evaluates the uploaded plan's `scope.files` against the run's implement-stage path constraints using the **same `backend/internal/policy` matcher as the post-implement gate**, so the plan-time verdict equals the verdict the implement stage would produce.

- `resolveImplementConstraints` mirrors `resolveStageReviewers`' spec read (parses `runs.workflow_spec`, finds the first implement stage) and flattens its `[]spec.Constraint` into a single `policy.Constraints` — keeping ONLY the scope-knowable constraints `forbidden_paths` / `allowed_paths` / `max_files_changed`, plus the built-in `agent_instruction_paths` (E80.12 / #3942), which `strictestAgentInstructionPolicy` folds STRICTEST across EVERY implement stage's `AgentInstructionPathsRule` (#4311), so a plan scoping an agent-instruction path is flagged here whenever some implement stage's post-implement gate would flag it (and not at all when every implement stage opts out). The three path kinds stay read from the first implement stage (#4345).
- `required_outcomes` is deliberately dropped (`tests_added_or_updated` would false-flag any plan not enumerating a `_test.go`, and `ci_green` has no pre-implement signal).
- Writes a `plan_scope_precheck` audit entry (payload `ScopePrecheckPayload{workflow_id, implement_stage_id, violations, scanned_files}`) **even on a clean scope** (empty `violations`) so a reader distinguishes "checked and clean" from "never checked".
- Advisory + fail-open throughout: a missing/unparseable spec or a workflow with no implement stage writes no entry and never blocks/unwinds the upload (matching `runPlanReviews`' degradation contract).
- The `plan FileOperation`→`policy.Status` mapping (create→A / modify→M / delete→D) is fidelity-only — policy path checks match on `Path` only, ignoring `Status`.
- MCP surface: `fishhawk_get_plan` adds `scope_precheck` (`ScopePrecheck{violations[], scanned_files}`) decoded from the **newest** `plan_scope_precheck` entry (`loadScopePrecheck` in `tools.go`; a schema-retry re-upload writes a second entry and the latest is authoritative), so the operator sees "scope hits forbidden_paths — wrong workflow?" before approving.
- Audit-kind note in `docs/issue-comment-surfaces.md`.
- The optional hard category-D plan-stage fail on an unambiguous forbidden match is deferred — this slice delivers the advisory surface only.

## Plan-gate test sweep (#942)

`test_sweep.go::runTestSweep` — called from `handleShipPlan` (`plan.go`) immediately after `runSurfaceSweep` and before `runPlanReviews`.

Generalizes the surface sweep's static registry to the class #942 names: a plan changing behavior whose tests live in an EXISTING `*_test.go` not listed in `scope.files`, which the runner then scope_drift-excludes (silently dropping the test edit, #885) or reconciles late (#862/#876).

fishhawkd has no local checkout, so it consults the repository tree at plan time via the Contents API — `githubclient.Client.ListDirectory` (`GET /repos/{owner}/{repo}/contents/{path}`, directory-listing array shape, default-branch HEAD via empty ref; `run.Run` carries no base tree ref, so a just-advanced main yields at worst one stale advisory).

Candidate generation is **data-driven per-repo** (#1004): `evaluateTestSweep(scopeFiles, dirListings, conventions)` takes effective `[]testConvention` = built-in `defaultTestConventions` (the Go `**/*.go` → `{dir}/{name}_test.go` rule plus colocated TS) **++ the run's declared `test_conventions`** (a top-level workflow-spec array of `{match: <doublestar glob>, candidates: [<path templates>]}` with vars `{dir}`/`{name}`/`{ext}`/`{relpath}`, parsed from `runRow.WorkflowSpec` via `spec.ParseBytes`; empty/unparseable spec fails open to the defaults only).
Declared entries APPEND to the defaults (never replace), so a no-`test_conventions` spec stays byte-identical to #1003's Go+TS behavior while a repo declaring only Python/Ruby keeps Go covered.

Four deterministic rules in the pure matcher (rules 3 and 4 are two rule ids on ONE table):

1. *Stem-sibling* — a scoped production file matching a convention's `match` (and not itself a recognized test file) whose expanded candidate test exists on the base ref and is absent from scope (rule id stays `stem_sibling`).
2. *New-test-in-tested-package* — a scoped CREATE whose basename is a recognized test file, in a directory that already has other recognized test files, reporting them sorted and capped at 10 names with `omitted_count` carrying the remainder.
3. The *path-trigger rule table* (`testSweepPathTriggerRules`, #1031) — curated rows of trigger glob → required paths evaluated against the scope set only (no Contents API consultation), so a row fires even when EVERY directory listing failed open. Rule id `migration_walk`: any scoped `backend/internal/postgres/migrations/*.sql` requires `backend/internal/postgres/postgres_test.go` (a new migration needs its own reversal test there; planners missed it on 0029/0030/0031). `RequiredPaths` is a slice so one row can require multiple paths per trigger — which rule 4 uses.
4. *Generated surface* (`generated_surface`, #3203) — the same table, one row per CANONICAL SOURCE whose DERIVED files a generator rewrites, so an OpenAPI or schema edit does not cost a mid-implement scope amendment. Two generators, named on the finding's `generator` field: **`scripts/gen-site-reference`** (the four byte-exact site Reference regions rendered by `cli/internal/docgen` — `docs/api/v0.openapi.yaml` → `reference/api.md`; the three `docs/spec/workflow-v{0,1,2}.schema.json` and the two `docs/spec/examples/workflow-v2-*.yaml` → `reference/workflow-spec.md`; `docs/spec/plan-standard-v1.schema.json` → `reference/plan-schema.md`; `cli/internal/cmdinfo/cmdinfo.go` → `reference/cli.md`) and **`scripts/sync-schemas`** (every canonical→mirror route of the script: the plan/clarification/grooming/crew-message/workflow/operator-role/work-management schemas, the two shipped default YAMLs, the three workflow presets and the three shared fixture corpora). Every trigger is a LITERAL path, so the required set is exact rather than glob-derived, and the finding names the derived files plus the command to run.

Overlapping declared+default conventions are deduped (candidate-set per production file + findings by `(rule, trigger_path, sub_plan_title, generator)`), so an overlap yields exactly one finding. The `generator` is part of that key because one canonical source legitimately feeds BOTH generators — `docs/spec/workflow-v2.schema.json` is rendered into the site workflow-spec region AND mirrored into two embedded schema copies — so a key without it would silently drop the second finding and hide half the gap. Adding it is inert for the three test-file rules, whose `generator` is empty.

**Residual, stated rather than papered over:** the `generated_surface` rows are a HAND-MAINTAINED MIRROR of two generators this package cannot import (`cli/internal/docgen` lives in the cli module; `scripts/sync-schemas` is a shell script). `TestGeneratedSurfaceRowsExistOnDisk` `os.Stat`s every trigger and every required path, so a RENAME on either side breaks loudly — but a NEW canonical source added to a generator with no matching row here is NOT caught, and the table under-reports until updated. A canonical schema scoped without its mirrors now draws BOTH a surface-sweep missing-sibling finding and a `generated_surface` finding; that duplication is deliberate — only the test-sweep finding names the generator to run.

**NOT call-graph/behavior-coverage analysis** — a plan changing package A whose tests live in package B is out of reach by design (#942 defers that).

Bounds and degradation:

- Bounded at `testSweepMaxDirs` (20) distinct directories per upload, counted AFTER candidate expansion so parallel-tree candidate directories (`tests/`, `spec/`) are included (the rest WARN-skipped).
- Each listing failure fails open per-call, and an all-listings-failed sweep writes NO entry (never-checked, not falsely clean).
- Writes a `plan_test_sweep` audit entry (payload `TestSweepPayload{findings, scanned_files, listed_dirs}`, `findings` an empty array not null) even on a clean sweep, and additionally fails open with no entry when `cfg.GitHub` is nil or the run's `installation_id` is nil (non-GitHub triggers / unwired deployments).

The returned payload threads into the plan-review prompt's gate-evidence section as a reviewer-judged ADVISORY (not an automatic high-severity concern: judge whether the changed behavior's tests or shared harness live in the flagged files — if so the plan must scope them or the runner will scope_drift-exclude the edits).

MCP surface: `fishhawk_get_plan` adds `test_sweep` (`TestSweep{findings[], scanned_files, listed_dirs}`) decoded from the **newest** `plan_test_sweep` entry (`loadTestSweep` in `tools.go`). Each finding carries the optional `generator` field (#3203). The persisted payload bytes are pinned by `testdata/plan_test_sweep_wire.json`, which `TestShipPlan_TestSweep_GeneratedSurface_EndToEnd` asserts the server wrote and `backend/internal/mcpserver/tools_test.go` reads back through the real MCP wire types — one file, two readers, so a json-tag rename on either side reddens the pair.

## Routed-concern NOT-ATTEMPTED pre-review signal (#2896)

`trace.go::runImplementReviews` — immediately after the `#2737` block, same allocate-if-nil `gateEvidence` pattern. A fix-up pass reports `succeeded` with no mechanical check that each routed concern was even ATTEMPTED (the verify gate certifies only the tree; this review is diff-only), so a silently dropped concern is indistinguishable from an addressed one — run `925addab` / PR #2895 routed two concerns, the pass never opened the medium's file, and the stage reported clean.

- `resolveRoutedFixupConcerns` selects the `stage_fixup_triggered` entry for the routing round that produced THE DELTA UNDER REVIEW, and captures each concern's 1-based ROUTING POSITION there. The pin is the audit log's own ordering: the reviewed delta IS its head SHA, `fixupPushedSequenceForHead` finds the `fixup_pushed` entry for `(stage, headSHA)`, and the round is the newest stage-bound trigger with `Sequence <` that push. Newest-wins would classify pass N's diff against pass N+1's concerns on any multi-pass stage — the normal shape, since re-routing a dropped concern in a further pass is the standard workaround — and write a durable warning naming the WRONG routing round. When the delta is unpinnable (empty `headSHA`, no `fixup_pushed` entry for that head, or a list error) the fallback is decided by AMBIGUITY not recency: ONE candidate trigger on the stage is used, TWO OR MORE suppress the signal (WARN). An UNDECODABLE governing trigger is the SAME rule, not a second one: inside the pinned window a malformed entry suppresses the signal rather than falling through to an older round — the concerns that round routed are unreadable, so any attribution would come from a round this delta was never written against, which is the wrong-round defect the pin exists to prevent reached by another path. A decodable entry routing ZERO concerns is not a round at all (`countStageFixupTriggers` reads it the same way) and the scan continues past it. This is a SEPARATE `ListForRunByCategory` call, deliberately NOT a hoist of the `#2737` anchor-pinned resolution: that anchor exists only when the served prompt detected a reporting obligation, so keying off it would make this signal inert on the ordinary fix-up pass. `concern_ids[i]` pairs with `concerns[i]` only when the lengths match; otherwise the finding is id-less and identified by position.
- `fixupAttemptCandidates` is the anchor set: the approved plan's `scope.files` UNION the committed paths. Only an EMPTY union suppresses the check — an empty `scope.files` with a committed diff still runs it (its candidates are then all touched by definition, so it can only produce a coverage-gap record).
- `backend/internal/fixupattempt` resolves each concern's implicated files from its note, and the SHARED routed text (`reason` + `operator_concern`) separately: a path from shared text cannot be attributed to one of several routed concerns without guessing, so untouched ones are reported UNATTRIBUTED. This is what makes the check non-inert — in the #2896 incident neither concern NOTE named a path while the operator's reason named both files.
- A path mentioned ONLY under an explicit negative instruction ("Do not touch onboarding.go") is dropped by `Implicated`: the agent leaving it untouched is OBEDIENCE, and reporting it would accuse the agent of a drop exactly when it followed instructions. Separating a CITATION from a requirement is not lexically tractable, so the shared-text half CLAIMS LESS instead of guessing — the payload member is `mentioned_untouched_files` and the prompt says "MENTIONED … and NOT touched", never "evidence of an unattempted concern".
- `touched` comes from `committedPathSet`, folding rename SOURCES in (#2398) so a rename-realized fix is never a false untouched signal; a diff whose R rows carry no source path is rename-INDETERMINATE and suppresses the signal entirely.
- Renders a distinct high-priority block (`prompt.GateEvidence.FixupUnattemptedConcerns` / `FixupMentionedUntouchedFiles`) framing a PER-CONCERN untouched file as evidence the concern was NOT ATTEMPTED and explicitly NOT proof it was not ADDRESSED, the shared-text line as a MENTION only, with the undeterminable count as a coverage caveat. Only the concern's severity/category and file PATHS are rendered — never the note text, so this block adds no new untrusted-text channel. The fix-up prompt states the same obligation up front (`writeTrustedFixupConcerns`).
- Appends ONE advisory `fixup_concern_unattempted` audit entry (payload `{routed_count, unattempted_count, undeterminable_count, concerns:[{id, position, severity, category, implicated_files}], mentioned_untouched_files}`) whenever the pass routed concerns AND there is a finding, an untouched named file, or a NON-ZERO undeterminable count — the last so a total coverage gap cannot read as silence. A pure coverage-gap record is operator-facing only and leaves the reviewer prompt byte-identical.
- EVIDENCE ONLY: it never fails, re-opens, or re-budgets the pass; pinned in both directions (no signal when every routed concern was attempted; identical review result / stage status / fix-up budget when it fires).
- Best-effort: nil `AuditRepo`, a list error, a malformed trigger payload, a rename-indeterminate diff, an empty candidate union, or an unpinnable delta across two or more routing rounds each contribute nothing; an append failure WARNs and the review proceeds.
- Long-form contract (resolution grammar, the boundary/ambiguity refusals, the three deliberate trades, and the real-incident coverage proof): `backend/internal/fixupattempt/README.md`. Audit-kind note in `docs/issue-comment-surfaces.md`.

## Routed-reporting-obligation undelivered pre-review signal (#2737)

`trace.go::runImplementReviews` — adjacent to the `#1407` block and using the same allocate-if-nil `gateEvidence` pattern. A fix-up pass can be routed an instruction that asks the agent to RECORD something on a report surface ("record the per-deletion counterfactual results in the PR body's `## Notes`"), and the slim fix-up prompt renders NO PR-description block, so before this the instruction could be declined with nothing marking the omission.

- `prompt.go::resolveFixupReportObligations` classifies the routed instructions (`concerns` notes, `operator_concern`, `reason` — all already on the `stage_fixup_triggered` audit payload) via `backend/internal/fixupobligation`, assigning stable `ob-N` ids; an obligation is PR-body `unsatisfiable` (#2782) only when its router declared the `pr_body` target (`instruction_targets` on the fix-up, recorded as `concern_targets` / `operator_concern_target` / `reason_target`, absent = `code`; E83.37 / #4022), never from its prose. The prompt SERVE resolves the newest stage-bound entry and then pins it by audit `Sequence` in a `fixup_report_obligations_declared` anchor (`recordFixupReportObligationsDeclared`); this review-time re-derivation reads that anchor (`resolveFixupReportObligationAnchor`) and resolves that EXACT entry, so a second fix-up triggered between the serve and the review cannot make the review report ids the agent never saw. No anchor → no signal. The SPA `prompt-render` preview renders the block but writes no anchor.
- The agent records each obligation in the existing `#1210` fix-up self-report sidecar's new `obligations` array; the runner validates each entry fail-closed and carries the survivors on `gate_evidence.fixup_reporting_obligations` → `bundle.FixupReportingObligations` → `prompt.GateEvidence.FixupObligationReports` (a CARRIER field, never rendered).
- A non-empty remainder after subtracting the `met` reports renders a distinct high-priority block (`prompt.GateEvidence.FixupReportingObligations`), deliberately worded so it cannot be read as a diff-only-unverifiable finding, AND appends ONE advisory `fixup_reporting_obligation_undelivered` audit entry (payload `{declared_count, undelivered_count, obligations:[{id, source, status, text_excerpt}]}`, status `unreported` | `declined`) BEFORE any reviewer verdict.
- No agent-authored free text crosses the upload boundary for obligation reports (the one deliberate exception is #3400's commit-body `approval_condition_responses`: text the agent ALSO commits on the branch, bounded to 4 KiB, pre-redacted, and rendered only inside a column-0 untrusted envelope — see the approval-condition responses signal below). The `record`/`reason` the agent writes is validated on the RUNNER (`met` needs a record, `declined` needs a reason) and discarded there; only `{id, status}` is transmitted, with the id constrained to the `ob-N` shape. Quarantining that text at the render site would have bounded what it could impersonate but not what it could carry — an arbitrary egress channel from a command-running agent to the reviewer, around the committed diff — so the channel is removed rather than hardened. The reviewer prompt and the audit payload carry only the OPERATOR's `text_excerpt`.
- EVIDENCE ONLY: it never fails, re-opens, or re-budgets the pass. Both directions are pinned — no signal when every obligation is met (the anti-noise control), and identical review result / stage status / fix-up budget when the signal DOES fire.
- Best-effort: nil `AuditRepo`, a list error, or a malformed trigger payload contributes nothing; an append failure WARNs and the review proceeds.
- Long-form contract (classifier grammar, id ordering, fail-closed drop table, the fabricated-`met` weakness): `backend/internal/fixupobligation/README.md`. Audit-kind note in `docs/issue-comment-surfaces.md`.

## Operator-scope-undelivered pre-review signal (#1407)

`trace.go::runImplementReviews` — before building the implement-review prompt, unions the run's two operator-add provenance channels (the approval-time `add_scope_files` folds via `amendedScopeFilesForReview`, and approved mid-stage scope amendments via `approvedAmendmentScopePaths` → `ScopeAmendmentRepo.ListByRun`) and computes the subset UNTOUCHED by the committed diff (`operatorScopeUndelivered`, untouched-only: absent from `diff.ChangedFiles`; directory-prefix / non-repo-relative tokens skipped like `MissingScopeFiles`).

- A non-empty set renders a high-priority `operator_scope_path_undelivered` warning in the prompt's gate-evidence section (`prompt.GateEvidence.OperatorScopeUndelivered`, allocate-if-nil) AND appends one deterministic advisory `operator_scope_path_undelivered` audit entry (payload `{undelivered_paths, undelivered_count, operator_added_count, indeterminate, evaluated_against, history_complete, incomplete_reason, cumulative_base_sha, head_sha}`) BEFORE any reviewer verdict — so a dropped operator-required edit (E23.9/E23.10) is visible pre-review instead of only at the reject→fixup round-trip.
- **Approval-condition responses signal (#3400).** `runImplementReviews` decides at ENTRY whether the round's gate evidence is BUNDLE-DERIVED (`isBundleDerivedGateEvidence`: non-nil and not the #3042 placeholder carrying only `VerifyEvidenceUnavailableReason` — keyed on the placeholder's exact shape, not on `reason != ""`, because the PARTIAL resolver path returns a real bundle with a reason stamped on it), before any allocate-if-nil block can conjure a non-nil evidence no bundle produced. When the trigger carries `ApprovalConditions` AND the evidence is bundle-derived it sets `prompt.GateEvidence.ApprovalConditionsAttached` (backend-only, never bundle-carried); when additionally `ApprovalConditionResponses` (mapped by `gateEvidenceForReview` from `bundle.GateEvidence.ApprovalConditionResponses`) is nil, it appends ONE advisory `approval_conditions_unrecorded` audit row (system actor, payload `{head_sha, source_pass: implement|fixup}` — `fixup` when the round carries a routed `addressed_pending` concern) BEFORE any verdict, best-effort WARN on failure, nil `AuditRepo` skips. The placeholder round and the nil-evidence consolidated/decomposed-parent path set neither: they never saw the runner's sidecar peek, so absence there means nothing. The reviewer-facing text says the responses came from the agent's PROPOSED commit message, read from the sidecar at pack time — the commit is made after the review dispatches, so branch durability is NOT asserted (the runner's `approval_condition_responses_committed` log line is where that is known). EVIDENCE ONLY: pinned by `TestRunImplementReviews_ApprovalConditionsUnrecorded_SignalDoesNotAlterOutcome`; the cross-boundary chain (runner-shaped bundle bytes → `bundle.ExtractGateEvidence` → `gateEvidenceForReview` → prompt render → audit row) by `TestRunImplementReviews_ApprovalConditionsUnrecorded_AuditRow` / `_ApprovalConditionResponses_Rendered`, the placeholder guard by `TestRunImplementReviews_FixupBackstopPlaceholder_NoUnrecordedRow`.
- **Indeterminate carry-through (#2398 fixup).** `operatorScopeUndelivered` returns the `committedPathSet` `indeterminate` flag alongside the untouched set; when the diff carries a rename row with no source path an absent operator-added path cannot be distinguished from a rename source, so the flag is threaded onto `GateEvidence.OperatorScopeUndeliveredIndeterminate` (and `indeterminate:true` on the audit payload) and `writeGateEvidence` HEDGES the warning as NOT DETERMINABLE rather than asserting it as fact. This keeps the second evidence surface from contradicting the `scopeProvenanceForReview` decomposition, which hedges the same path under the same diff mode.
- **Evaluated against the stage's CUMULATIVE committed state, not the pass delta (#3029).** Since #2884 a post-fix-up re-review is dispatched from `maybeBackstopFixupReReview`, whose diff is `ComparePatch(fixupBase, head)` — the fix-up DELTA — and `runImplementReviews` fed that delta straight into `committedPathSet`. Both this signal and `scopeProvenanceForReview` therefore asked "did THIS PASS touch the path" while their evidence text asserted "absent from the committed file set", so every operator-granted path delivered by an EARLIER pass of the same stage was reported undelivered (six occurrences, three reject verdicts on correct work). `resolveStageCumulativeEval` (`trace.go`) resolves the true state with a SECOND `ComparePatch(stageBase, head)` — `stageBase` being the `base_sha` of the OLDEST stage-scoped push-ledger entry (`lineageLedgerCategories`) — and BOTH surfaces classify against that one set, so they can never contradict each other. The reviewed DIFF is untouched: the #1725 delta framing still shows the reviewer the fix-up delta; only the evaluation set moved. Cost is one extra forge compare per implement review round, deliberately accepted.
- **A per-pass UNION is deliberately rejected.** A union of historical touched paths is an EVER-TOUCHED set: a path delivered in pass 1 and reverted in pass 2 has NO row in `stageBase..head` but DOES have an `M` row in the pass-2 delta, so no fold of per-pass deltas can detect the revert. That trades a false positive (a noisy gate) for a false NEGATIVE (a silent gate), which is strictly worse. There is no forge-free route to the true cumulative state, which is why the round-trip is taken.
- **Completeness is FAIL-CLOSED, with one machine reason per degrade.** The `stage-cumulative` label is asserted ONLY when the compare actually succeeded over a resolved stage base. Every other outcome returns the CURRENT PASS diff — the signal keeps working — with `history_complete:false`, `evaluated_against:"this-pass"`, a named `incomplete_reason`, and a reviewer block that drops the machine-verified framing and instructs verification against the PR's cumulative diff:

  | `incomplete_reason` | Cause |
  |---|---|
  | `push_ledger_unreadable` | nil `AuditRepo`, or a `ListForRunByCategory` error over the push ledger |
  | `stage_push_ledger_empty` | the read succeeded but carries NO stage-scoped entry. Deliberately INCOMPLETE, never "this pass is the whole stage": an absent row and a row whose `StageID` does not match are indistinguishable to this read, so the empty result does not establish that the pass diff spans the stage base |
  | `stage_base_sha_unavailable` | the oldest stage-scoped entry carries no decodable, non-empty `base_sha` |
  | `stage_base_equals_head` | the resolved base IS the current head, so the compare would return an EMPTY inventory and report every grant undelivered |
  | `forge_compare_unavailable` | no GitHub client, no installation, no current head, or an unparseable repo (the CLI/dev posture) |
  | `cumulative_compare_failed` | `ComparePatch` returned an error |
  | `cumulative_compare_truncated` | the forge capped the compare's file inventory (GitHub's 300-file cap, or an omitted oversized patch body), so a delivered path can be MISSING from the response — which would reproduce the exact false positive this closes |

- **STAGE-scoped, and the residual is named.** The ledger is filtered to THIS stage, so a grant delivered in a DIFFERENT implement stage of the same run — a decomposition child, or a re-run stage — WILL still be reported undelivered. That is the operator-ratified boundary (all six recorded occurrences were same-stage and the confirmed mechanism is per-stage), not an oversight; a seventh occurrence of that shape should be diagnosed against this sentence rather than re-filed as #3029.
- **The #2896 fixup-unattempted-concern check deliberately keeps the PER-PASS diff.** "Was this routed concern attempted in THIS pass" is genuinely a per-pass question — a concern attempted two passes ago and not attempted in this one is exactly what that signal reports — so widening it to the cumulative set would break it.
- Advisory + best-effort: a nil `ScopeAmendmentRepo` or `ListByRun` error contributes nothing and never blocks the review; an all-delivered commit keeps the prompt byte-identical and emits no entry.
- The complementary BLOCKING gate for a FULLY-untouched concrete DECLARED scope path is the runner's #1151/#1231/#2501 scope-completeness park (`gitops.MissingScopeFiles`; the scope-completeness invariant in `docs/ARCHITECTURE.md`); this is the advisory pre-review surface for the partial / operator-added case.
- Audit-kind note in `docs/issue-comment-surfaces.md`.

### Forge-resolved implement-review compare (`forge_compare.go`, E45.47 / #3464)

The four implement-review diff sites in `trace.go` — `DispatchConsolidatedReview` (the decomposed-parent consolidated review), `maybeBackstopFixupReReview` (the post-fix-up re-review, the #2884 primary path), `resolveFixupDeltaDiff` (the fix-up delta framing), and `resolveStageCumulativeEval` (the cumulative evaluation) — read their diff through `Server.forgeCompareFor(runRow)`, which returns a narrow `patchComparer` (`ComparePatch(base, head)`, satisfied by both `*githubclient.Client` and `forge.Forge`) plus the credential scope and repo, resolved by forge FAMILY: github via `cfg.GitHub` (never the resolver), any other family via `cfg.ForgeResolver` / `forge.Get` (`isNilForge`-guarded). Before #3464 these were GitHub-App-installation-gated, so a GitLab run's post-fix-up re-review and consolidated review never dispatched and the merge gate parked on a pending review; the fix-up-delta and cumulative sites degraded to full-diff / this-pass. A github-family run is byte-identical (the existing `NoInstallationID` / `ParseRepoError` / `GitHubNotWired` tests stay green). The nested-path residual noted for the merge seam does NOT apply here — `forgeCompareFor` splits the repo on the last slash and the GitLab adapter ignores `RepoRef`. Tested in `forge_compare_test.go` (the rung table) and `trace_test.go` (each site dispatches for a gitlab-family run; a typed-nil forge fails closed; github never consults the resolver).

### Implement-review diff-truncation signal (#2875)

`runImplementReviews` derives, from the SAME diff the round reviews (`reviewedDiff` — the fix-up delta on a delta re-review, else the full diff, so the prompt and the audit can never describe a different diff), whether the reviewer saw only a PREFIX of the change. The runner cuts the unified patch at its 256 KiB cap (`policy.Diff.PatchTruncated`, carrying only the boolean over the wire) or the forge caps an oversized compare (`consolidatedReviewDiff` now maps `cmp.Truncated`/`cmp.TruncationReason` onto `PatchTruncated`/`PatchTruncationReason`, which is what fires the notice on the fix-up-delta and consolidated fan-out paths that previously discarded it).

- **Reason is fail-closed and never blank (condition 1).** `deriveReviewDiffTruncation` fills `runner_patch_cap` when `PatchTruncated` is true and no forge reason is present (the runner has no producer for a reason string), and carries the forge reason verbatim otherwise. A non-empty `PatchTruncationReason` therefore always denotes a FORGE truncation, marked `BestEffort` because the compare API's file inventory is itself capped and the omitted set may be incomplete (condition 4).
- **Omitted-file derivation is fail-closed and over-reporting (`truncatedPatchOmittedFiles`).** It classifies each `ChangedFiles` path (the uncapped `--name-status` inventory) against the surviving patch: VISIBLE only when a `diff --git ... b/<path>` header line is present AND not the LAST surviving header (the last file is where the cap landed — `may be cut`); no header → `no hunks shown`. The match is ANCHORED on the full ` b/<path>` line-suffix via `gitDiffHeaderIndex` so a prefix/suffix path collision (`foo.go` vs `bar/foo.go`) can never mark an omitted file visible (condition 5, the under-report direction), and a git-C-quoted path fails closed to OMITTED (condition 6 conservative wording: `may be cut`, never asserting the tail IS gone).
- **Prompt bounded, gate view + audit complete (condition 3).** The prompt list is capped at `reviewDiffPromptOmittedCap` (200) with a `+N more` residual; the `implement_review_diff_truncated` audit payload and the gate-view distillation carry the COMPLETE list. `reviewDiffTruncatedForRun` distils the newest entry onto `gateViewResponse.ReviewDiffTruncated` (`review_diff_truncated`), mirroring `liveValidationForRun`'s best-effort posture (nil repo / list error / no entry / undecodable payload → nil, never a failed gate view).
- Advisory + best-effort: `emitReviewDiffTruncated` WARN-logs an append failure and a nil `AuditRepo` skips; the review is never blocked. Audit-kind note in `docs/issue-comment-surfaces.md`.

### Mid-stage amendment rollup (#2874)

The implement-review prompt is built from the RAW approved plan, so a path the operator APPROVED mid-stage — via `fishhawk_decide_scope_amendment`, folded by the runner into the stage's ENFORCED scope — reached the reviewer with no provenance and read as scope drift. On run bff9a242 both reviewers independently flagged `backend/internal/audit/categories.go` forty-eight minutes after the operator approved amendment `6c8a2006` for exactly that path (review sequences 67694 / 67705), costing two waivers and one spurious medium. `stageApprovedAmendmentScopePaths` (`stage_scope_amendment.go`) closes it: `trace.go::runImplementReviews` calls it once and threads the per-path records — path, authorizing amendment id, the operator's decision reason — onto `prompt.Trigger.MidStageAmendedScopeFiles`, which `buildImplementReview` renders as its own `### Scope amended mid-stage` section. The reviewer now evaluates against the stage's EFFECTIVE scope rather than its declared scope.

- **STAGE-SCOPED, APPROVED-only.** The resolver filters `a.StageID == stageID` — the SAME filter the enforced fold `resolveApprovedScopeAmendments` applies — so the rendered set is provably a SUBSET of what the runner permitted for this stage and can never assert in-scope a path the scope gate would have rejected. A sibling stage's approved amendment never leaks in (pinned at the resolver AND at the review-context level by `TestRunImplementReviews_MidStageAmendment_NoCrossStageLeak`). DENIED and PENDING rows confer nothing, so the fix cannot over-correct a refused request into a grant (`TestRunImplementReviews_OmitsDeniedMidStageAmendment`) — that would be a worse defect than the false drift signal being repaired.
- **Reverses the `shownScopeFilesForPrompt` "deliberately separate" contract.** That doc comment used to record a deliberate decision that the REVIEW surface must fold only `resolveApprovalAddScopeFiles` and NEVER approved mid-stage amendments, because "widening the reviewer's drift baseline would be wrong". Field evidence overturned it; the comment now records the reversal rather than contradicting the code. The two helpers stay separate because they feed different `Trigger` fields and different sections, not because the review side is forbidden the paths.
- **Cross-channel dedupe: deliberately NOT suppressed.** A path also surfaced by the approval-time `add_scope_files` fold renders in BOTH the "Scope amended at approval" and "Scope amended mid-stage" sections. The two lists have different PROVENANCE — authorization granted at the gate versus a mid-stage request the operator answered — and each section is the authority for its own channel; duplication is harmless to a reviewer (both say in-scope, NOT drift), whereas suppressing one would silently hide that the agent also asked mid-stage.
- **Fail-open, best-effort.** A nil `ScopeAmendmentRepo` or a `ListByRun` error WARN-logs and contributes nothing, matching `approvedAmendmentScopePaths` and `childApprovedAmendmentScopePaths`: a provenance lookup must never block a review, and the degraded behaviour is exactly the pre-#2874 behaviour (a flag-only criterion, never a reject ground). Paths already in the plan's raw `scope.files` are excluded (`writeApprovedPlan` renders those) and duplicates dedupe first-wins, preserving `ListByRun`'s oldest-first order so repeated builds are stable.
- **Two deliberate NON-changes.** The #1407 operator-scope-undelivered union and `scopeProvenanceForReview` already union `approvedAmendmentScopePaths`, which covers this same channel — re-folding here would double-count. Only the narrative section and criterion 4 were missing, which is precisely what both reviewers quoted.
- **Criterion 4's wording changed unconditionally.** The standing scope-adherence criterion now names all three operator-authorized lists; leaving it naming only the approval-time list would have left the exact rule both reviewers quoted still binding. It renders on every implement review, so implement-review prompt hashes DIFFER from prior runs — a one-time, stated break. See `prompt/README.md` for what section absence does and does not guarantee.
- **The acceptance stage does NOT share this gap** (the issue's open question, checked not assumed). `buildAcceptance` renders no `scope.files` at all: it withholds the diff by design and judges the running instance against `acceptance_criteria`, so it has no drift baseline to widen and nothing to fix. No follow-up filed.

### Decomposed-parent child-amendment rollup (#2820)

A decomposed PARENT's implement review resolves its declared-scope provenance from the PARENT run's own records only, so a fan-out CHILD's APPROVED mid-stage scope amendment — which lives under the CHILD run id — reaches the parent's consolidated diff with no provenance on the parent: it renders as an unexplained residual and the reviewer flags scope drift (#2237, #2239, the second of which produced a durable audit record falsely asserting a skipped process step). `childApprovedAmendmentScopePaths` (`child_scope_amendment.go`) closes that gap. `trace.go::runImplementReviews` calls it once and threads the result onto `prompt.Trigger.ChildAmendedScopeFiles`; it is consumed BOTH by the review prompt's "Scope authorized by child slice amendments" section and by `scopeProvenanceForReview`, which folds it as the new **`child-scope-amendment`** channel (each fold carrying a per-path `GateScopeFold.Detail` naming the authorizing amendment id, slice index, and child run id) so the machine `UnexplainedCount` arithmetic accounts for the path instead of reporting a residual.

- **Children discriminated by `DecomposedFrom`, not `ParentRunID`.** The resolver reuses `listAllDecomposedChildren` (`consolidate.go`, `ListRuns{DecomposedFrom}`) so paging past the 100-row cap is inherited, not reimplemented. `ParentRunID` is set for recovery children too (the E48.102/#2549 recovery-lineage walk), so it would wrongly sweep them in; `DecomposedFrom` names only fan-out slices.
- **Fail-open, per-child.** A nil `RunRepo`/`ScopeAmendmentRepo`, a `listAllDecomposedChildren` error, or a per-child `ListByRun` error all WARN-log and contribute nothing — the last ONLY for the failing child, whose siblings still resolve. A provenance lookup must never block a review; the end-to-end degrade (a failing amendment lookup still yields a DISPATCHED review with the section simply absent) is pinned in `implement_review_test.go`. Children are sorted deterministically (`SliceIndex` nil-last, then `CreatedAt`, then id) and paths deduped first-wins so the rendered prompt is stable.
- **Deliberately OMITTED from the #1407 operator-scope-undelivered union** (comment recorded in `runImplementReviews` at the `operatorAdded` build): the child-amendment set is a PERMISSION granted on a child slice, not a work-order the operator placed on THIS parent. Folding an approved-but-unused child path into the undelivered union would surface it as an operator-scope-undelivered MISS on the parent — trading the #2820 false drift signal for a false miss signal. It is folded only into the review-facing provenance.
- **Runner-side enforcement is unchanged.** The runner's scope gate on a decomposed-parent fix-up pass is out of scope; this change is review-facing provenance only. The fix-up's enforced scope reads the same walk through `childApprovedAmendmentEntries` since #4338 (§ "Decomposed-parent fix-up scope union (#4338)").

### Decomposed-parent per-slice verify evidence (#3132)

A decomposed PARENT's implement review was dispatched with a **nil** `GateEvidence` (`trace.go::DispatchConsolidatedReview`), because the parent's implement stage is a FAN-OUT: it spawns no agent, writes no commit and uploads no trace bundle, so `bundle.ExtractGateEvidence` has nothing to read on the parent's own chain. The consequence is that reviewers judged the consolidated diff — the LARGEST diffs the loop produces — with the tree's compile and test state entirely unknown, and both reviewers on run `0aad7486` / PR #3182 independently flagged missing verify evidence that the operator then had to answer by hand. The per-slice verify DID run: each child uploads its own redacted trace carrying a `gate_evidence` event, and the existing `resolveStageGateEvidence(runID, stageID)` seam already reads exactly that, per run and per stage. `childSliceVerifyEvidence` (`child_verify_evidence.go`) propagates it, modelled file-for-file on the `childApprovedAmendmentScopePaths` rollup above: it pages the parent's decomposition children via `listAllDecomposedChildren`, sorts them with the same deterministic comparator, resolves each child's implement-stage gate evidence, and returns `[]prompt.GateSliceVerify` plus an omitted count. `runImplementReviews` threads it onto `prompt.GateEvidence.SliceVerify` next to the `#2820` rollup — placement there rather than at the `DispatchConsolidatedReview` call site is what makes it cover BOTH the consolidated first review and the parent fix-up re-review dispatched from `maybeBackstopFixupReReview`.

- **Named-absence invariant.** Every per-child degrade produces a ROW carrying a machine reason, NEVER a silently missing slice — a dropped slice reads to the reviewer as "this slice does not exist", the silent-absence class the issue exists to close. This resolver owns `child_stage_list_failed`, `child_has_no_implement_stage` and the defensive `child_gate_evidence_unresolved`; every literal `resolveStageGateEvidence` itself returns (its seven load degrades and two partial states) is passed through verbatim onto the row.
- **Fail-open, best-effort, per child.** A nil `RunRepo`/`AuditRepo`/`TraceStore` or a `listAllDecomposedChildren` error returns `(nil, 0)`; a per-child `ListStagesForRun` error emits that child's named row and its SIBLINGS still resolve — never an all-or-nothing early return. An ordinary non-decomposed run has no children, so it allocates nothing and its prompt stays byte-identical.
- **Distinct structural literal.** When the parent carries no verify evidence of its own and nothing already named a reason, `VerifyEvidenceUnavailableReason` is set to `decomposed_parent_no_parent_level_verify` rather than reusing one of `resolveStageGateEvidence`'s transport-gap literals: on a fan-out parent the parent-level gate is absent BY CONSTRUCTION, and a literal saying "transport gap" would name the wrong cause. Correspondingly, `writeGateEvidence`'s two `#3042` NOT-ATTACHED blocks are SUPPRESSED whenever `SliceVerify` is populated — their "compile/test state is UNVERIFIED / this is a transport gap" framing is false for a fan-out parent.
- **Bounding, and what the bound may drop.** At most `prompt.MaxSliceVerifyRunsPerSlice` (6) non-superseded runs per row (keeping the LAST N, so the terminal run survives), with the output tail blanked on any `passed` run. `prompt.MaxSliceVerifyEntries` (20) bounds the **PASSING rows ONLY**: the resolver PARTITIONS on `sliceVerifyNonPassing` (an unresolved slice, a `failed` summary, or a non-`passed` terminal run) and retains every non-passing row **however many there are**, spending what is left of the bound on passing rows. So the bound can **only ever drop PASSING rows** — under a plain index ordering a failed slice at position 21 would be invisible, and that is precisely the row whose absence endangers a merge; and a non-passing-first ordering followed by an unconditional truncation would still eat non-passing rows once they outnumber the bound, while the rendered block asserts that every failed or unresolved slice is present — i.e. the prompt would lie about exactly those rows. A fan-out with more non-passing slices than the bound therefore renders MORE than 20 rows and omits zero: the bound exists for a wide GREEN fan-out and is deliberately yielded when the alternative is a hidden failure. The rendered truncation line states that the omitted slices are all passing. Pinned by `TestChildSliceVerifyEvidence_CapNeverDropsANonPassingSlice` (non-passing rows BEYOND the bound in index order) and `TestChildSliceVerifyEvidence_MoreNonPassingThanTheCapAreAllRetained` (more non-passing rows than the bound itself).
- **`VerifiedHeadSHA`, and the residual it makes visible.** Each row names the commit the row's own AUTHORITATIVE evidence ran against: the LAST (terminal-most) non-empty `head_sha` among the **NON-SUPERSEDED** runs — the same runs whose outcomes the row renders — falling back to an earlier authoritative run only when the later ones recorded none (a gate-skipped or infra-failure run emits an empty `head_sha`). A SUPERSEDED run's SHA is never borrowed: after a verify-fix iteration the absorbed run ran against an OLDER commit, so reading the raw list would render `verified head: A` next to a terminal `passed` on head B, misattributing which commit the gate certified and pointing a future cross-check at the wrong one. When the authoritative runs carry no `head_sha` the field is EMPTY and the row renders `(not recorded)` rather than a stale SHA (`TestChildSliceVerifyEvidence_HeadSHAIsTheTerminalRuns`, `..._HeadSHAFallsBackWithinAuthoritativeRuns`, `..._SupersededRunsDropped`). What remains is the child's PUSHED head at gate time, which is normally but not necessarily the commit the fan-in integrated; nothing here cross-checks the two against the `integration_commit_recorded` ledger. The residual is rendered in the block in the reviewer's own terms rather than left in a plan artifact, and carrying the SHA plumbs the value a future cross-check would need.
- **Issue proposal 2 deliberately NOT taken.** The issue also proposed a durable `verify_*` audit row on the child. This change reads the evidence that already exists rather than adding a new persisted surface; the audit-row proposal stays open.

### Rename-aware touched set (#2398)

Both this `#1407` signal and the `#1914` `scopeProvenanceForReview` decompose "was this declared path touched?" against a shared `committedPathSet(diff)` helper. It inserts every row's destination `Path` AND, for `Status == StatusRenamed` rows carrying a non-empty `OldPath`, the rename **SOURCE** — because porcelain rename detection collapses a declared delete+create into a single `R` row that records only the destination, so the source path never appears as its own row and would otherwise read as untouched (manufacturing a spurious high-severity `evidence_conflict` on any file move — run 933cd6ee / #2397). A **COPY** (`StatusCopied`) source is deliberately NOT inserted: a copy leaves its source byte-unchanged, so treating it as touched would replace a false negative with a false positive. When the diff carries any RENAME row with an EMPTY `OldPath` (a pre-field bundle, or the forge-compare consolidated-review diff), the helper reports `indeterminate` — an absent declared path cannot be distinguished from a rename source, so the provenance marks itself indeterminate and the prompt hedges the untouched label rather than asserting it. Copy rows with an empty source do NOT set indeterminate (a copy source is never counted as touched, so its absence cannot flip an untouched verdict).

## Stale-review detection: `review_head_mismatch` (#3655, comparison rewritten by #3665)

A base-rebase re-invoke seals and ships its trace bundle BEFORE the agent is re-invoked (#742 forward gating), so the implement-review round dispatched from that bundle can judge the FIRST attempt's change while the PR carries the re-invoked one (run 30fdadd8 / PR #3654). This DETECTS that and records it; it does NOT supersede or re-dispatch the stale round.

- **CHANGES, not trees, and not commits.** The round's `head_sha` is the throwaway committed-tree WIP-commit SHA (`bundle.ExtractHeadSHA`) the runner soft-resets away before the real push, so the pushed COMMIT differs on every ship — a commit comparison would fire on every ship (failed run a054cbdd's first plan). #3655 therefore compared TREES; #3665 found that wrong too. A routine commit-time base advance re-stages the SAME change onto a different base, so the two trees differ on every such ship while nothing the reviewer judged was superseded — the detector fired on the ordinary case (run 3ddb0f56). Both sides now carry a `git patch-id --stable` sum over the diff their commit introduces against its OWN parent. patch-id ignores line numbers, and the commit's own parent is the symmetric base across a fresh pass (parent == recorded base), a `FreshFetchBase` restage (parent == the re-fetched base) and a fix-up (parent == the run's own previous push) — where `base_sha` would mean something different on the `RebaseFromRemote` path and false-positive on every fix-up ship. So a change re-staged onto an advanced base yields an IDENTICAL id (no row) while a genuinely different re-landed change yields a different one (row still fires).
- **Reviewed side.** `trace.go` calls `bundle.ExtractVerifyIdentity`, which returns the `tree_sha` AND the `change_id` of the bundle's authoritative (LAST non-empty-tree) verify_run — read from THAT SAME event, never by an independent last-non-empty scan over `change_id`, which could pair iteration 3's tree with iteration 1's change id and manufacture a verdict from two different commits. Both are threaded through `runImplementReviewsForTree` → `emitReviewStarted(…, headSHA, treeSHA, changeID)` onto `planreview.ReviewStartedPayload` (`tree_sha,omitempty` / `change_id,omitempty`). Only the trace-time bundle path knows them; `runImplementReviews` (every other caller) and the plan path pass `""`, keeping those payloads byte-identical. An extraction error WARN-logs and passes `""` for BOTH — it never suppresses the review, only disables the check for that round.
- **Pushed side.** The runner's `openPRAndShipArtifact` ships `verified_change_id` on the SUCCESS artifact (omitted when it cannot be computed), the patch id of the REAL pushed commit — so unlike `verified_tree_sha` it needs none of the #969 reverify-pass rebind bookkeeping. `verified_tree_sha` is still shipped and still decoded, now as a human coordinate.
- **Check.** `recordReviewHeadMismatch` (`pullrequest.go`), on the create path of an implement-stage success ship, after the supplemental scope row and BEFORE the terminal advance: the newest same-stage `implement_review_started` round's `change_id` vs `pr.VerifiedChangeID`. Only a non-empty ≠ non-empty pair appends `review_head_mismatch` `{run_id, stage_id, reviewed_tree_sha, pushed_tree_sha, review_round_sequence, reviewed_head_sha, pushed_head_sha, reviewed_change_id, pushed_change_id}` — the change ids are the evidence, the trees and heads human coordinates (which is what keeps the gate-view / run-status / next_actions distillations reading unchanged), `review_round_sequence` the #3593 recorded round identity.
- **Fail-closed to silence, WIDENED.** Either CHANGE ID empty (an older runner shipping none, a no-verify stage, a held-commit resume, a legacy or pre-#3665 started row, an extraction or git degrade) is undecidable → no row. Equal change ids (the normal ship, and the base-advance restage) → no row. The TREE comparison is deliberately NOT kept as a fallback: keeping it would keep the reported false positive on exactly the runs the issue says it fires on most, and recording nothing beats recording something wrong. This does disable the detector for one deploy's worth of older runners — bounded, since the runner is respawned from `bin/` on every stage dispatch. Nil `AuditRepo`, list error, no started row, an undecodable NEWEST payload (older rounds are not consulted in its place), or an append error → WARN/silent, no row, never alters the response. The idempotent replay path does not re-run the check.
- **Remaining residual.** Context lines ARE part of the hashed diff body, so a base advance that modifies a line INSIDE a changed hunk's context window genuinely changes the patch id and records a row. This replaces #3655's much broader "known over-report" (every base advance), which is what this change closes. The row stays literally true — the reviewed and pushed diffs are not the same bytes — and advisory, and the failure direction is over-report, never a false silence.
- **Not an issue-comment surface** — deliberately absent from `activityCategories`; see `docs/issue-comment-surfaces.md`. The gate-view / run-status / next_actions surfacing is a separate slice.
- Tests: `pullrequest_test.go` — `TestRecordReviewHeadMismatch_BaseAdvanceRestage_RecordsNoRow` (the #3665 done-means negative: differing trees AND heads, identical change ids, zero rows, read back out of Postgres) and `_GenuineChangeDifference_RecordsOneRow` (the positive, payload pinned field-for-field) run against pgtest; `_EmptyChangeID_RecordsNothing` tables the three undecidable inputs, each paired with DIFFERING trees so the guard's absence is observable; F3–F6 + list-error on the fake seam; `_BundleToShipEndToEnd` drives a real bundle through the trace-upload handler and then a real PR ship. `trace_test.go` pins the change id reaching `implement_review_started` through the real trace upload, and the older-runner degrade. Runner-side: `TestGitPatchIDForCommit_SameChangeOnAdvancedBase_EqualIDs` is the real-git proof of the patch-id property, `_DifferentChange_DifferentIDs` preserves the run a1321dbb counter-example (five identical files plus an amendment's sixth still fires).

## Retry supersedes the prior review round (#3593)

An implement-stage retry that succeeds opens a FRESH review round, but before this change the prior attempt's round leaked onto the retry's PR two ways (run bed3f1d8, PR #3592): (1) the `pr_review_posted` relay posted EVERY unposted `implement_reviewed` entry, so attempt-1's rejects — including one that landed AFTER `stage_retried` — were posted onto the retry's PR; and (2) attempt-1's concerns stayed `raised` because the retry round's `confirmed` resolutions map to the forbidden `raised → addressed` edge. The fix records the review ROUND IDENTITY rather than inferring it, and keys supersession on that recorded round.

- **`review_round_sequence` is the recorded round key.** `emitReviewStarted` (`plan.go`) now returns `(seq int64, ok bool)` — the `implement_review_started` row's audit sequence, `ok=false` on a nil `AuditRepo` or append failure. `runImplementReviews` captures it as `roundSeq` and threads it into `runImplementReviewInvocations`, which stamps it on EVERY `implement_reviewed` payload's `ReviewRoundSequence` (int64, omitempty). `roundSeq == 0` (the supplemental re-invoke pass, which emits no started row, or an emit failure) records no key and marks nothing; consumers then fall back to the legacy derivation.
- **Three controls, pairing to cover every retry-landing instant:**
  1. **The retry-time sweep** — `retry.go::retryStageAs` calls `supersedeOpenImplementConcerns` (matching ALL open implement concerns of the stage) after the retry audit write and before the run un-terminal, gated on an implement stage re-opened to `pending` (a D-timeout `awaiting_approval` re-open supersedes nothing). Reason names the retry ordinal (`stage retry` / `stage override retry`).
  2. **The verdict-build-time flag** — `runImplementReviewInvocations` reads the latest same-stage retry sequence AT VERDICT TIME and sets `SupersededByRetry = ok && roundSeq > 0 && latest > roundSeq`.
  3. **The post-persist re-check** — after `persistReviewConcerns`, the loop RE-READS the retry sequence and supersedes ONLY the just-minted rows (`OriginReviewSequence == entry.Sequence`) when it exceeds `roundSeq`. A verdict superseded at either read has its `concern_resolutions` DROPPED (they describe the discarded tree) and does not witness condition claims.
- **The accepted window, and why it is safe.** A retry landing BETWEEN the payload write and the post-persist re-check leaves that payload UNMARKED (`SupersededByRetry` false). That is acceptable because (a) the post-persist re-check (or the retry-time sweep, whichever runs second) still supersedes the concerns, and (b) the PR relay compares the retry rows against the recorded `review_round_sequence`, not the flag alone, so an unmarked-but-superseded verdict is still never posted. `TestImplementReview_RetryBetweenPersistAndSweep_ConcernsSuperseded` pins exactly this window.
- **`latestStageRetrySequence` contract.** `(0, true)` when no matching row exists — "no retry yet" is a SUCCESSFUL read that yields `superseded=false`; `(0, false)` ONLY on a list read error or nil `AuditRepo`, and the predicate gates on `ok` so a bookkeeping outage never over-suppresses. It reads BOTH `stage_retried` and `stage_override_retried`, matching same-stage rows only.
- **The relay's exact rule** lives in `issuecomment.Notifier.supersededReviewSequences` — see `docs/issue-comment-surfaces.md`. For a row carrying `review_round_sequence` it NEVER uses the legacy "newest `implement_review_started` below the verdict" derivation (that derivation misreads the bed3f1d8 late reject at 98339, which sits above the new round's start); that derivation is the legacy-only fallback for a row lacking the recorded key. Fails OPEN (post) on any read error or undecodable payload.
- All supersession is best-effort/warn-only, mirroring `supersedeReplanConcerns` — a stuck concern is exactly today's operator-waive behavior, never worse.
- **A superseded implement concern stays RECOVERABLE (E45.83 / #3618).** The supersede above is correct — the concerns describe a tree that no longer exists — but the discard was reported as a SETTLE: `open_implement` drops to 0, the MCP classifier returned `implement_gate_settled` with the merge ritual, and `resolveConcernsByID`'s blanket `!State.IsOpen()` refusal made the discarded ids unroutable, leaving free-text `operator_concern` as the only recovery (which destroys the reviewer provenance). Two narrow changes close that WITHOUT touching the supersede: (1) `resolveConcernsByID` now admits an explicit set — the three open states PLUS `concern.StateSuperseded` — via `concernRoutable`, so a superseded implement-stage concern of the TARGET stage can be named in `concern_ids` and is re-opened by the existing `MarkAddressedPending` call through `concern`'s single `superseded → addressed_pending` edge, keeping its reviewer/round/severity and then tracked to closure by the ordinary delta-verification machinery; `addressed`/`waived`/`deferred`/`addressed_by_condition` stay refused with a message naming the admitted set. The stage-kind and run/stage-ownership refusals stay ORDERED AHEAD of the state check, so a plan-stage or cross-run superseded id is still refused for its own reason (pinned by `fixup_test.go::TestFixupStage_ConcernIDs_SupersededPlanStageRejected` / `_SupersededForeignStageRejected`). (2) The run-status concerns block reports `superseded_implement` — see the run-status paragraph above. The cross-layer seam over a real Postgres is `fixup_test.go::TestFixupStage_SupersededConcern_CrossLayer_PG`, which reads the COMMITTED concern state back rather than trusting the fix-up's 200 (the `MarkAddressedPending` call is warn-only, so the response is byte-identical either way).

## Re-review convergence: settled ledger + re-litigation guard (#1913)

`trace.go` makes implement re-review rounds converge by threading settled history forward and turning operator arbitrations into a machine-binding suppression guard (issue #1913; measured churn on runs a04d5cbf / 98704b0c).

- **Settled-ledger threading.** `settledConcernsForReview` (sibling of the OPEN-only `priorConcernsForReview`) gathers the stage's `waived`/`deferred` + `addressed`/`superseded` concerns into `prompt.Trigger.SettledConcerns`, threaded into every post-fixup round so a round-N reviewer has the full settled history (deferred arbitrations, invisible before, now reach the reviewer). Waived concerns MOVED out of `priorConcernsForReview` into this set; `hasFixupRoutedConcern` still gates the #1725 delta on `addressed_pending`, unaffected.
- **`concern_relitigation_suppressed` audit-category contract.** An internal, advisory, best-effort audit kind (system actor, payload `{settled_ref, settled_state, severity, category, note, reviewer_model, origin_review_sequence}`) written by `persistReviewConcerns` → `suppressRelitigation`/`appendRelitigationSuppressed` when a verdict concern's `settled_ref` resolves to a **same-run/same-stage/same-stageKind** `waived`/`deferred` concern AND its `new_evidence` is empty — the guard excludes that concern from the durable open-row insert and records this entry instead (so the suppression is visible, never silent). It posts NO issue comment and adds no Notifier method, so it is NOT an issue-comment surface (it is registered in `audit.KnownCategories` for `fishhawk_await_audit`). Fail-open on every other case — unparsable/unknown ref, cross-stage ref, non-waived/deferred state, non-empty `new_evidence`, and any lookup/append error (WARN) all fall through to the normal insert, so a sloppy tag never suppresses a genuine finding and a store outage never wedges the loop. A re-raise against an `addressed`/`superseded` concern is deliberately insertable (a genuine regression must reach the operator).

## Per-resolution reopen veto + the `reject_without_concern` advisory (`trace.go`, E68.41 / #3319)

**MECHANISM FIRST, so a later reader does not mis-state what this closes.** `concern.StateReopened` is written in exactly ONE place — `applyConcernResolutions`, inside the loop over `concern_resolutions[]` that parses `res.ID` as a UUID before setting the state — and no other `ApplyResolution` caller writes it. A bare `reject` with no resolutions therefore reopens NOTHING today, so #3319's rule as LITERALLY written was already satisfied on the reported run 35897f17 and would not have prevented it. What ships here is the rule's INTENT — a reopen must be SUBSTANTIATED for the concern it names — plus a regression pin on the literal rule (`TestApplyConcernResolutions_BareRejectWithNoResolutionsReopensNothing`, which passes on unchanged code).

**THE PREDICATE SPLIT is the load-bearing design decision, and it must not be re-unified.** Two deliberately different questions live in `planreview`:

- `RejectSubstantiatesResolution(v, res)` — the **PER-RESOLUTION** question the VETO asks. False (refuse) when `v.Verdict == reject` AND `len(v.Concerns) == 0` AND `strings.TrimSpace(res.Note) == ""`. It reads ONLY this resolution's own note.
- `RejectNamesNoConcern(v)` — the **VERDICT-LEVEL** question the DISPLAY ADVISORY asks. True when the verdict is `reject`, raises no concern, and NO resolution carries a non-blank note. It is NEVER consulted by the veto.

They disagree on exactly the case that matters. Given `verdict=reject, concerns[]=empty, A=confirmed note "looks good", B=reopened note ""`: `RejectNamesNoConcern` is FALSE (A carries a note), so a veto keyed on it would apply B's unsubstantiated reopen **using concern A's evidence** — the defect this issue exists to close. `RejectSubstantiatesResolution(v, B)` is false and refuses B while A's confirm applies normally. Pinned by `TestApplyConcernResolutions_M8_MixedResolutionVetoesOnlyTheBlankReopen`, `TestRejectPredicates_DisagreeOnMixedResolutionVerdict`, and the cross-boundary `TestE2E_RejectWithoutConcern_PredicateSplit`, which drives BOTH shapes and asserts the difference.

- **`free_form` substantiates nothing in either predicate.** Free-form prose is unmatched to any concern id — the exact shape #3319 reports.
- **The arm is deliberately narrow, in both directions.** A one-word note ("still broken") is NOT refused: a broader semantic judgement is not machine-decidable and would silently discard genuine findings. A `reject` that raises a NEW concern is an engaged review, so its blank-note reopens still apply. Both narrowings carry their own test (`M2`/`M3`).
- **A refused reopen mutates NOTHING** — no `ApplyResolution` call, so the row keeps its state AND its `state_reason`. That untouched ledger is the issue's done-means, so every test asserts COMMITTED STATE (the row re-read after the call), never an error identity.
- **`roundReviewVerdict` gained a PURELY ADDITIVE `review planreview.ReviewVerdict` field** carrying the full decoded verdict (the pre-existing `verdict`/`resolutions` projections cannot answer the predicate, which needs `Concerns` too). Out-of-scope keyed literals in `implement_review_test.go` construct this struct, so do NOT rename or remove a field. A legacy literal leaves `review` zero-valued (`Verdict ""`), so the predicate returns true and the arm is inert for it — which is why the four existing confirm-veto tests stay green unedited.
- **`ImplementReviewedPayload.RejectWithoutConcern`** (`reject_without_concern,omitempty`) is stamped from the VERDICT-LEVEL predicate. Additive within an existing audit category: `omitempty` keeps every pre-#3319 entry byte-identical and an old stored payload decodes false. No new audit category is minted, so `audit.KnownCategories`, `docs/issue-comment-surfaces.md` and the completeness scanner are untouched. The MCP layer decodes it onto `PlanReview` and folds a display-only `review_reject_without_concern` next-action; it gates nothing.
- **The review prompt names the refusal** (`prompt.go`'s prior-concerns section, inside the `len(t.PriorConcerns) > 0` guard) so a reviewer learns the rule before tripping it.

## Round-level concern-resolution veto (`trace.go`, E48.103 / #2551)

A routed concern used to auto-resolve on whichever reviewer confirmed it. `runImplementReviewInvocations` applied each verdict's `concern_resolutions` INLINE, per reviewer, with no knowledge of what the other reviewers in the SAME round said or of what the fix-up pass actually did — so a diff-only peer's `confirmed` retired a high-severity concern while the reviewer that RAISED it was still returning `reject` (run 9bba554d, concern 3b012c1f).

- **Buffered round application.** The loop now buffers each verdict as a `roundReviewVerdict{model, verdict, resolutions, reviewSequence}` under the UNCHANGED append-gated posture (buffered only when the `implement_reviewed` append returned a sequence), and `applyRoundConcernResolutions` replays them IN LOOP ORDER after the loop and BEFORE `recomputeAndPublishAuditComplete` (so the republished check still reflects post-resolution state). REOPEN-WINS is unaffected — `concern.validTransitions` encodes it order-independently, and both orders are pinned by test. The only mid-loop concern-row reader is `persistReviewConcerns` → `suppressRelitigation`, which keys strictly on `waived`/`deferred` — states no delta-verification resolution can produce — so buffering cannot change what it observes; `resolveConditionClaimedPlanConcerns` reads PLAN-stage rows (this path is implement-only) and the `hasRejection`/`pagedRejectAppended` bookkeeping is in-memory.
- **Four deterministic ROUND-LEVEL veto arms, evaluated in fixed order against a `confirmed` resolution ONLY** (`resolutionVetoContext.vetoReason`; since E80.3 / #3760 a fifth arm, `server_check_requires_human`, is evaluated AHEAD of these four and is the one arm that also refuses a reviewer `superseded` — only on a server-synthesized concern, see § "Diff secrets check"; `superseded` on any other concern is never vetoed, and since #3319 `reopened` has ONE arm of its own — see the next section, which is decided per resolution rather than from this round-level context): (1) `raiser_rejected_same_round` — a DIFFERENT reviewer confirms a concern whose RAISING reviewer (`concern.ReviewerModel`, matched by model string) returned `reject` in this round; a reviewer confirming its OWN concern is never vetoed, since that reviewer IS the authority the veto respects; (2) `operator_evidence_routed` — the concern was routed by a same-stage `stage_fixup_triggered` carrying a non-empty `operator_evidence` (see below); (3) `fixup_pass_no_changes` — the concern's NEWEST same-stage routing trigger is followed by `fixup_no_changes` with no earlier `fixup_pushed`; arm 3 fires on POSITIVE evidence of a no-change pass, so a newest trigger with NO following outcome entry at all is NOT vetoed — a re-review round runs only after the fix-up pass terminalizes, so the only real-round way to observe that is a failed best-effort outcome append (an audit-durability defect), and vetoing there would refuse confirms for a pass that DID push with no later entry to clear the veto; (4) `evidence_lookup_failed` — the audit reads behind arms 2/3 failed.
- **The evidence reads FAIL CLOSED** (a deliberate departure from this loop's prevailing fail-open posture): a nil `AuditRepo`, a `ListForRunByCategory` error, or a malformed trigger payload vetoes every `confirmed` in the round rather than applying it on unknown evidence. Asymmetry of harm — a wrongly-vetoed concern stays open and costs the operator one waive/defer (both remain available, so the gate cannot wedge), while a wrongly-applied confirm is the silent false-GREEN this issue reports.
- **A vetoed resolution mutates NOTHING.** `ApplyResolution` is not called: the concern keeps its current open state AND its `state_reason`. The refusal is WARN-logged and recorded as a `concern_resolution_vetoed` audit entry.
- **`concern_resolution_vetoed` audit-category contract.** Internal, advisory, BEST-EFFORT (system actor, payload `{concern_id, resolution, veto_reason, confirming_reviewer_model, raising_reviewer_model, concern_severity, concern_category, note, review_sequence, origin_review_sequence}`), registered in `audit.KnownCategories` so `fishhawk_await_audit` accepts it. `veto_reason` is one of the four round-level confirm reasons, `reopen_without_named_concern` (#3319) or `server_check_requires_human` (E80.3 / #3760), and `resolution` distinguishes them (`confirmed`, `reopened`, `superseded` — only ever with `server_check_requires_human` — or `auto_close` for a refused clean-round close; empty on a pre-#3319 entry). It posts NO issue comment and adds no Notifier method, so it is NOT an issue-comment surface (same posture as `concern_relitigation_suppressed`) — `docs/issue-comment-surfaces.md` is deliberately untouched. An append failure WARN-logs and **the veto still stands**: refusing to veto because bookkeeping failed would retire a disputed concern, which is strictly worse. That is safe only because the gate view derives the dispute from durable evidence rather than from this entry (next section).
- **`operator_evidence` (`fixup.go`).** An optional field on `POST /v0/stages/{stage_id}/fixup` declaring that the OPERATOR executed a reproduction. Its meaning is AUTHORITY, not prose delivery — the reproduction text still reaches the agent via `reason`/`operator_concern`. Validated beside `operator_concern` (whitespace-only → 400 `field: operator_evidence`; > `maxOperatorConcernBytes` → 400 naming the byte count and cap) and NOT a selection input (it alone does not satisfy the at-least-one-of rule). Recorded on the `stage_fixup_triggered` payload under `operator_evidence` ONLY when non-empty, so pre-#2551 payloads stay byte-identical. The exemption it grants is PERMANENT for those concerns: no later REVIEWER confirmation retires them — only a waive, a defer, a human operator's resolve-with-evidence (`POST /v0/runs/{run_id}/concerns/resolve`, #4086 — see "Merge readiness + operator-evidence resolve" below), or a genuine fix.

## Clean-round auto-close of a routed concern (`trace.go`, E45.84 / #3619)

Until this change the ONLY writer of `concern.StateAddressed` was `applyConcernResolutions`, which needs a reviewer to emit an explicit `confirmed` entry in `concern_resolutions[]`. A reviewer that approves the post-fix-up tree WITHOUT emitting one left the routed concern in `addressed_pending` forever — the run f199dcf1 shape: five reviewer concerns genuinely fixed across four fix-up passes, still raised at the gate, while the fix-up's OWN minted operator concern closed because the agent's obligation path is different wiring. `autoCloseUnjudgedRoutedConcerns` adds ONE pass that closes a routed concern the round re-judged and said nothing about. It is called from `runImplementReviewInvocations` immediately AFTER `applyRoundConcernResolutions` and BEFORE `recomputeAndPublishAuditComplete` — ordering that is load-bearing in both directions: after the explicit resolutions so a reviewer reopen/confirm has already written its state and this pass sees the FINAL per-row state, and before the audit-complete republish so the merge-gate check reflects the post-close open count in the same pass (pinned by `TestAutoCloseCallSiteOrderedBetweenResolutionsAndAuditComplete`).

- **What it closes.** A row in `addressed_pending` EXACTLY — the routed marker — owned by this run + stage + `StageKindImplement`, whose `OriginReviewSequence` is strictly BELOW the round's minimum `reviewSequence`. A `raised` row was never routed to the agent and a `reopened` row was routed and then re-raised; a clean round is evidence neither was fixed, so neither is eligible. The eligibility test is on audit SEQUENCE, never a timestamp: concern rows are stamped by Postgres `now()` while the review loop runs in the Go process, and the skew between those clock domains is unbounded (AGENTS.md), so a timestamp-based "was this routed before the round" test would be a cross-clock-domain comparison no scaling factor can fix.
- **Two round-level guards, each closing NOTHING and WARN-logging its reason.** (1) ROUND INCOMPLETE — `len(round) != dispatchedInvocations`. `round` gains exactly one entry per invocation, and the loop skips the append on a reviewer error, on an `implement_reviewed` append failure (no sequence) and on a retry-superseded verdict — the three cases where the round is not a complete judgment of the new tree. (2) ROUND NOT CLEAN — any buffered verdict is `reject`.
- **An EXPLICIT reviewer judgment always wins over the implicit close.** `judgedIDs` records every `res.ID` VERBATIM and BEFORE any UUID parse, so a malformed, unknown or VETOED resolution still excludes its concern. This is what makes the #2551 veto unbypassable by this path: a `confirmed` the veto refused can never be laundered into a close here. `reRaisedIDs` does the same for any fresh concern in the round whose `settled_ref` names the routed id — the round re-raising a concern is the opposite of saying nothing about it.
- **The three evidence vetoes refuse an auto-close exactly as they refuse a reviewer `confirmed`** — and so does the `server_check_requires_human` arm (E80.3 / #3760), evaluated first, which refuses the auto-close of a server-synthesized concern outright. The pass reuses the EXISTING `buildResolutionVetoContext` (never a fork of its derivation) and calls `vc.vetoReason(row, "")`: `operator_evidence_routed` (the operator executed a reproduction, so only the operator may retire it), `fixup_pass_no_changes` (the pass pushed nothing, so the tree the round judged is the tree that already carried the defect) and `evidence_lookup_failed` (fail-closed on unknown evidence, which includes a nil `AuditRepo`). The confirming-model argument is deliberately `""` — there is no confirming reviewer on this path, and the `raiser_rejected_same_round` arm is already subsumed by the round-not-clean guard (a clean round populates no `rejectedBy` entries). Each refusal appends a `concern_resolution_vetoed` entry with `resolution: "auto_close"`, so a refused auto-close is as visible in the ledger as a refused reviewer confirm.
- **`concern_auto_closed` audit-category contract.** Internal, advisory, BEST-EFFORT (system actor, payload `{concern_id, severity, category, raising_reviewer_model, origin_review_sequence, closing_review_sequences, closing_reviewer_models, reviewed_head_sha, basis}`), registered in `audit.KnownCategories` so `fishhawk_await_audit` accepts it. It posts NO issue comment and adds NO Notifier method, so it is **not** an issue-comment surface and `docs/issue-comment-surfaces.md` needs no entry — the same classification `concern_resolution_vetoed` and `concern_relitigation_suppressed` carry. `basis` is `clean_re_review_round`, the single value today: the row records WHY a concern reached `addressed` with no reviewer resolution behind it, so an operator reading the settled ledger can tell an auto-close from a reviewer confirm without a hand diff. A failed append WARN-logs and the closure STANDS (the durable concern row is the authority on its own state).
- **No new state, no new lifecycle edge, no migration.** `addressed_pending -> addressed` already exists in `concern.validTransitions`, so `backend/internal/concern` is untouched. The `state_reason` names the basis, the closing round's reviewer models and review sequences, and — only when the round carried one — the byte-identical ` [verified at <sha>]` suffix `applyConcernResolutions` appends for a confirm (#2884), so the two closing paths speak ONE provenance vocabulary. Per-row failures (including an `InvalidTransitionError` from a row that changed state under us) WARN-log and continue, mirroring `applyConcernResolutions`; a `ListByRun` error closes nothing, the same best-effort posture as `priorConcernsForReview`.
- **The ACCEPTED TRADE, stated plainly.** A round in which every reviewer approves a tree that was NOT actually fixed will close the routed concerns. That is the trade the issue asks for — the next review round judging the new tree IS the evidence — and it is bounded by the three veto arms above. It is NOT bounded by anything else: a silently-wrong unanimous approval is exactly the case this closes on purpose. Falsifiable by `TestAutoCloseUnjudgedRoutedConcerns_FixupNoChanges_NotClosed` and `_OperatorEvidenceRouted_NotClosed`, which fail if either bound is absent.
- **Behavioural coupling worth naming.** Closing routed concerns makes `hasFixupRoutedConcern` false for the NEXT round, so the #1725 delta-verification prompt section correctly stops rendering rows that are no longer open — a later round will not be asked to re-judge a concern this pass closed, which is why the guards are deliberately conservative.
- **Awaiting-re-review needed no new wire field.** The pre-round state IS literally `addressed_pending`, already broken out in the run-status concerns block's `by_state` map and carried with its routing reason in the gate view. After this change that state is TRANSIENT rather than permanent.
- Tests: `trace_test.go`'s `TestAutoCloseUnjudgedRoutedConcerns_*` (one per guard and per veto arm, each asserting COMMITTED STATE read back through the repository — the pass returns nothing, so an error-identity assertion would not discriminate) and `implement_review_test.go`'s `TestImplementReviewRound_CleanRoundAutoClosesRoutedConcern` / `_CleanFirstReviewAutoClosesNothing` (the cross-layer pins that the pass is REACHED from the production loop and that `buildRunConcernsPayload`'s `open_implement` sees the closure).

## Diff secrets check (`diff_secrets.go`, E80.3 / #3760, ADR-084 D5 / rule 5)

A deterministic, model-free check that turns a credential-shaped ADDITION into a server-synthesized implement concern only a human can clear. The scanner is `backend/internal/diffsecrets` (parse rules and the de-duplication trade: its README); this section owns the server wiring.

- **Placement.** `runImplementReviewsForTree` calls `raiseDiffSecretConcerns(ctx, runID, stageID, diff, headSHA)` immediately after `GetRun` succeeds and BEFORE the reviewer-configuration early return — so the check runs when no model reviewer is configured, and before persona resolution (the seam E80.5 / [#3762](https://github.com/kuhlman-labs/fishhawk/issues/3762) consumes; the function returns the round's groups for it). All three implement-review rounds reach that function: the first review (the raw bundle's diff), every fix-up push (`maybeBackstopFixupReReview`, whose diff is `ComparePatch(fixupBase, head)` — the PASS DELTA; the trace-time hook skips a push-fixup bundle since #2884), and a decomposed parent's consolidated review. Scanning each pass's delta covers every push, and because nothing auto-clears a raised concern, a later delta that omits an earlier hit cannot falsely clear it (pinned by `TestDiffSecrets_FixupBackstopDeltaRaisesAndNormalRoundKeeps`).
- **What is raised.** `redaction.DefaultPatterns` over the ADDED lines only. Each (file, pattern class) group becomes ONE `review_concerns` row: stage kind implement, severity `high`, category `security`, empty reviewer model and role, provenance `server_check`, `check_key` `diff_secrets|<pattern>|<path>`, and a location-only note (`diffsecrets.Note`).
- **Origin entry.** ONE `diff_secrets_detected` audit entry per round that raises anything (system actor, stage-anchored; payload `{check, head_sha, patch_truncated, hit_count, groups:[{path, lines, pattern, check_key}]}`); its sequence is the concerns' `origin_review_sequence`. INTERNAL — registered in `audit.KnownCategories`, no `issuecomment` activity line, so it is not an issue-comment surface and `docs/issue-comment-surfaces.md` is untouched.
- **De-duplication.** Under the package-level `diffSecretsRaiseMu`, a group is dropped when a `server_check` row with its `check_key` exists on THIS stage in an open, `waived` or `deferred` state. A `superseded` or `addressed` row does not suppress a re-raise, and a row on another stage never does. A round whose every group is already on record appends nothing.
- **Failure modes, one per branch (`TestDiffSecrets_FailureModes`).** A nil `ConcernRepo` or `AuditRepo` is a no-op. An empty `Patch` with changed files WARN-logs `added lines not scanned` and raises nothing (an empty diff is silent). A `ListByRun` error fails OPEN to raising — a duplicate is noise, a missed secret is silent. An audit-append failure WARN-logs and inserts NOTHING (no concern without its origin record). An `InsertRaised` failure WARN-logs and leaves the audit entry as the authoritative record.
- **Nothing carries the matched bytes.** Not the concern row, not the audit payload, not a log line (run/stage ids, counts, paths, lines and pattern names only), and not a reviewer prompt: immediately before `buildImplementReviewPrompt`, after the #1725 delta substitution, `trig.DiffPatch` passes through `redaction.RedactDefault` (`redactReviewPatch`), so the full-diff round, the delta re-review and every persona prompt built from a copy of `trig` see `[REDACTED:<pattern>]`; a diff with no credential-shaped string stays byte-identical, and only per-pattern hit counts are logged. Pinned end to end by `TestDiffSecrets_TraceUploadToHumanWaive` (bundle upload → concern store → prompt → reviewer adapter → waive handler, with the key bytes asserted absent from every concern field, every audit payload, every captured prompt and the log buffer).
- **Human-only clearing.** Waive, bulk waive and defer refuse an agent token (`operator-agent/` or `mcp:run:` subject) or a `delegated:true` request with 403 `concern_requires_human` (`concern_human_only.go`; contract in `backend/internal/concern/README.md`). On the reviewer side, `server_check_requires_human` is the FIRST arm of `resolutionVetoContext.vetoReason`, so a reviewer `confirmed` and the clean-round auto-close are refused and recorded as `concern_resolution_vetoed`; `applyConcernResolutions` additionally routes a reviewer `superseded` of a server_check row through the same arm (a tightening over the plan, which named only confirm and auto-close: a supersede would otherwise close it with no human decision). `priorConcernsForReview` skips server_check rows, so the delta-verification section never asks a reviewer to resolve what it cannot; the settled ledger still shows a waived or deferred one (its note carries no bytes). Pinned by `TestDiffSecrets_ReviewerCannotRetireServerCheck` and `TestDiffSecrets_PriorConcernsExcludeServerCheck`.
- **Residuals (stated, not fixed).**
  - **Retry supersession re-raises; a normal fix-up round never supersedes.** The only paths that supersede open implement concerns are the stage-retry path (`retry.go`, every open implement concern of the re-opened stage) and the post-persist retry race in `trace.go::ingestImplementReview` (via `supersedeOpenImplementConcerns`, matching only rows whose `origin_review_sequence` is that verdict's own sequence — never a `diff_secrets_detected` sequence, so in practice it does not reach a server_check row). Both are retry-scoped, and a retried implement's first review re-scans its FULL bundle diff, so a credential still present is re-raised (dedupe ignores superseded rows). A fix-up re-review round whose delta does not contain the key leaves the server_check row raised.
  - **The dedupe mutex is per process.** `diffSecretsRaiseMu` serializes read-check-insert within ONE fishhawkd; two replicas reviewing the same stage concurrently can each mint a row for the same key (a duplicate, never a miss).
  - **A truncated patch** (runner 256 KiB cap, forge compare cap) is scanned only up to the cut; `patch_truncated` is recorded on the entry.
  - **An audit-append failure skips that round's raise**; a later pass re-detects the hit only if its delta touches the file.
  - **A decomposed parent's consolidated review re-raises its children's hits on the PARENT run**, so a human waives each run's concern.
  - **An empty patch is WARN-logged, not audited.**
  - **Group-level dedupe**: once a human waives a pattern class in a file, a different key of the same class in the same file is not re-raised (the `diffsecrets` README states the trade).
  - **False positives are human-cleared**: `json-password-field` matches JSON-shaped key/value pairs in test code; each is a high concern, and open concerns block delegated merge.
  - Attaching the security persona on a hit is E80.5 / #3762; forge secret-scanning alerts are out of scope (#3760 item 5).

## Permission-drift check (`permission_drift.go`, E80.4 / #3761, ADR-084 D5 / rule 5)

A deterministic, model-free check that turns a change WIDENING a declared permission surface into a server-synthesized implement concern only a human can clear. The surface list, extractors, keys, polarity and the extension format live in `backend/internal/permdrift` (its README); this section owns the server wiring.

- **Placement — only once the head is on the forge.** The check reads whole files at the base and head commits through the forge (`forgeCompareFor` for the changed-file list, `fileFetcherFor` — `forge_compare.go` — for contents), so it cannot run at the FIRST implement review: that review fires from the trace upload BEFORE the runner pushes (#2884: the trace-time head is a throwaway verify sha). Call sites: the PR-opened `/pull-request` report (create path, implement stage, `base_sha → head_sha`; NOT the `GetByHash` idempotent replay — de-duplication would cover a repeat anyway), `succeedFixupPushStage` (the pass delta: previous branch head → pushed head), `succeedConflictResolutionPushStage` (see below) and `DispatchConsolidatedReview` (a decomposed parent's base → consolidated head; the parent is the run that merges, so a child's `pushed` report is deliberately not hooked). Each runs on a detached, `bgReviews`-tracked goroutine (`context.WithoutCancel`), so the runner's report latency is unchanged; the consolidated path runs it first INSIDE its review goroutine rather than on a second one (that goroutine's body is `runConsolidatedReviewRound`, which the re-run verb's first-round path also runs, synchronously — § "Operator re-run", #4282).
- **Conflict-resolution pushes ARE checked** (no agent-authored push bypasses it). The pass's merge commit carries the base branch's own changes, and the forge has no merge-base primitive, so for each changed surface file the check evaluates BOTH previous-head → pushed-head AND base-branch-tip → pushed-head (the base branch named on the stage's `stage_conflict_resolution_triggered` entry, fetched by branch name) and raises only widenings present in BOTH, keyed by check key AND direction (`intersectDrift` matches first's widenings against second's widenings only, narrowings against narrowings — a narrowing sharing the check key never keeps a widening alive, `TestIntersectDrift`): a widening the base branch introduced is absent from the second comparison and dropped; one the resolution introduced survives. When the base branch cannot be named the check runs the single comparison and records `intersect_unresolved` (a superset — noise, never a miss). A file ABSENT at the base branch (`forge.ErrNotFound` — a file the run added that the base branch never had, or a deleted/renamed base branch, since the contents API answers 404 for a missing ref too) keeps the single previous-head → pushed-head comparison as a superset instead of intersecting it away (`intersectSide`, #3939 F8): absent → head reports no change for a restriction the resolution removed, so the intersection would drop it. Any other read failure on the base-branch side fails the pair closed (`fetch_failed`). Pinned by `TestPermissionDrift_ConflictResolutionIntersects` and `TestPermissionDrift_ConflictIntersectAbsentBaseBranchSide`.
- **What is raised.** One `review_concerns` row per widened key: stage kind implement, severity from the surface (high everywhere, medium for GitHub App events — ratified at the plan gate), category `security`, provenance `server_check`, `check_key` `permission_drift|<surface>|<path>|<key>|<after>` (`permdrift.CheckKey` — escaped, control-free, an over-256-byte field digested), and a note naming surface, path, key, before and after (`permdrift.Note`, each value through `permdrift.Display`). What cannot be evaluated raises one `unevaluable` row instead (`permdrift.UnevaluableKey(surface, path, head)`, note from `permdrift.NoteUnevaluable` — the reason class, never file bytes); exactly which conditions do so is the fail-closed table below.
- **Narrowings are notices, never concerns.** All narrowings of one check go to ONE `permission_narrowing_noticed` entry (`{check, surfaces_version, trigger, base_sha, head_sha, narrowings:[{surface, path, key, before, after}]}`). A reorder-only change appends nothing at all.
- **Origin entry.** ONE `permission_drift_detected` entry per check that raises anything (system actor, stage-anchored; payload `{check, surfaces_version, trigger, base_sha, head_sha, intersect_ref, intersect_unresolved, surfaces_ref, compare_truncated, widenings:[{surface, path, key, before, after, severity, check_key}], unevaluable:[{surface, path, reason, check_key}], extension_rejected, extension_error}`), appended FIRST. The file-derived display fields (widening/narrowing `path`/`key`/`before`/`after`, unevaluable `path`, rejected-entry `id`) are `permdrift.Display` renderings (Cc/Cf/Zl/Zp → U+FFFD, ≤200 bytes); `check_key` is the escaped, digest-bounded key of the RAW values (`TestPermissionDrift_PayloadAndNoteAreSanitized`); `surfaces_ref` (omitted when empty) is the ref the repository extension was read at — absent when it was not read (`commit_missing`, `surfaces_ref_unresolved`); its sequence is the concerns' `origin_review_sequence`. All three categories (with `permission_drift_raise_failed`) are INTERNAL — registered in `audit.KnownCategories`, no `issuecomment` activity line, so `docs/issue-comment-surfaces.md` is untouched.
- **Repository extension, read at the RUN's base on every pass.** `.fishhawk/permission-surfaces.yaml` is fetched at the run's recorded base commit, never at an agent-authored pass base, so a change cannot delete its own surface — not even by removing a declaration in one pass and widening the surface in a later fix-up (#3939 F3; `TestPermissionDrift_ExtensionReadAtBase`, `_FixupReadsExtensionAtRunBase`, `_FixupHandlerReadsExtensionAtRunBase`); it may only ADD surfaces, and is itself the `permission-surface-declarations` surface. The ref (`permissionDriftSurfacesRef`, recorded as `surfaces_ref`; one arm per mode in `TestPermissionDrift_SurfacesRefResolution`):

  | Trigger / run kind | Ref the extension is read at |
  |---|---|
  | PR-opened, consolidated review | the request's `base_sha` (already the run's base) |
  | fix-up or conflict-resolution pass, non-child run | the `base_sha` of the stage's newest `pull_request_opened` entry; else `run.DocumentBaseCommit`; else unresolved |
  | fix-up or conflict-resolution pass, decomposed CHILD (`DecomposedFrom` set) | the `base_sha` of the stage's newest `child_pushed` entry (the slice cut point the runner reports); else unresolved — NO `DocumentBaseCommit` fallback |

  An unreadable ledger, an undecodable payload or an empty `base_sha` is no entry (never "read at the pass base"). An UNRESOLVED ref reads NO extension: the check records ONE `surfaces_ref_unresolved` unevaluable FIRST (so it is the one reason that lands for that check key), sets `extension_error`, and evaluates the product surfaces only. What it does NOT suppress: the `permission-surface-declarations` SURFACE comparison of `.fishhawk/permission-surfaces.yaml` itself, scheduled when the diff changes that file (or the compare is truncated or carries a rename), still runs on the diff's own base → head sides, because it is a separate surface — a declaration's removal there still raises (approval condition 1, the "unresolved ref with RepoSurfacesPath changed" arm).
- **Changed files.** From the forge compare. A `renamed` file's SOURCE (`forge.ComparePatchFile.PreviousPath`) is evaluated against every surface it matches, glob or exact, so a governed file moved out of a surface reads as its removal there (`TestPermissionDrift_RenameSource`). A rename (like truncation) also probes every EXACT-path surface at both refs.
- **De-duplication.** Under the package-level `permissionDriftRaiseMu`, a key is dropped when a `server_check` row with it exists on THIS stage in an open, `waived` or `deferred` state; `superseded`/`addressed` do not suppress. Because `<after>` is in a widening key, the same key moving to a NEW value later raises again. An unevaluable key carries the resolved HEAD COMMIT (`permissionDriftKeyHead`), so a waived/deferred unevaluable row suppresses only a repeat at that commit, never a result at a new one; a push report's head is the reported SHA, and the consolidated trigger's head (the consolidated BRANCH name, never keyed on) resolves to the compare's head commit — and the check then reads every surface file AT that commit, so the key names the commit actually evaluated even if the branch moves mid-check. A compare whose commit listing is capped (`CommitsTruncated`: GitHub lists 250 commits, so its last listed commit is not the tip) or lists none is an UNRESOLVED head (files are read at the branch name); only a FAILED compare falls back to the newest `integration_commit_recorded` `merge_sha`. When no SHA resolves (an empty head; a capped or empty consolidated listing; a consolidated compare failure with no ledger entry) the key carries `@` and only an OPEN row suppresses it (`TestPermissionDrift_UnevaluablePerHead`). A check whose every finding is on record appends nothing.
- **Fail-closed table (`TestPermissionDrift_FailureModes`, `_RenameSource`, `_UnevaluablePerHead`).** Exactly these conditions raise an `unevaluable` concern (reason class in parentheses):

  | Condition | Reason | Surface / path in the key |
  |---|---|---|
  | base or head commit missing on the request (raised after the forge resolves, before any forge call) | `commit_missing` | `all-surfaces` / `base..head` |
  | the forge compare returns an error | `compare_failed` | `all-surfaces` / `base..head` |
  | the compare is truncated | `compare_truncated` | each GLOB surface / `""` (exact-path surfaces are probed instead) |
  | a `renamed` file with no reported source path | `rename_source_unknown` | each GLOB surface / the destination path |
  | a surface file read fails with anything but `forge.ErrNotFound` at base, head or the conflict-resolution base branch — including a file too large for the contents API (`GetFile` rejects a non-base64 encoding) | `fetch_failed` | the surface / the file |
  | a fix-up / conflict-resolution pass whose run-base ref does not resolve (recorded FIRST; the extension is then NOT read) | `surfaces_ref_unresolved` | `permission-surface-declarations` / `.fishhawk/permission-surfaces.yaml` |
  | the run-base extension read fails with anything but `forge.ErrNotFound` | `fetch_failed` | `permission-surface-declarations` / `.fishhawk/permission-surfaces.yaml` |
  | the run-base extension does not parse | `extension_parse_error` | `permission-surface-declarations` / `.fishhawk/permission-surfaces.yaml` |
  | a surface file does not parse at base or head | `parse_error` | the surface / the file |
  | a Go-source surface loses its recognized shape | `shape_unrecognized` | the surface / the file |

  For all three extension rows the product surfaces are still evaluated and `extension_error` is recorded (`surfaces_ref_unresolved`, `fetch_failed` or `parse_error` respectively); rejected extension entries are named in `extension_rejected` (not a concern). A 404 is an absent side, never a failure (an added file's grants all widen; a deleted spec's restrictions all widen).

  These do NOT raise anything (each is a skip, by design): a nil `ConcernRepo`, `AuditRepo` or `RunRepo` (no-op, no forge call — nowhere to record); a `GetRun` failure (WARN-skip); a forge compare or file fetcher that is unavailable — no client, no installation, unresolved forge, a forge that cannot fetch files (INFO-skip, the CLI/dev posture, as the consolidated review). Recording failures: a `ListByRun` error fails OPEN to raising; an audit-append failure raises NOTHING (no concern without its origin record); an `InsertRaised` failure is retried ONCE, and a second failure appends `permission_drift_raise_failed` (`{check, origin_sequence, check_keys, error}`) and the detected entry stands as the record. Logs carry ids, counts, surfaces, paths and check keys only.
- **`DispatchConsolidatedReview` guard audit (item 7, #3935).** Every return before its drift hook is a non-parent identity check (`RunRepo` nil, `DecomposedFrom` set, no children), a store error resolving the parent, its children or its implement stage (or no implement stage — nothing to attach a concern to; the review round is skipped with it), or forge compare not wired (the check's own skip posture). None depends on reviewers, so the hook runs for a mergeable parent with no reviewer declared or wired (`TestPermissionDrift_ConsolidatedReviewNoReviewers`).
- **Human-only clearing.** Identical to the diff secrets check: `concern_human_only.go` refuses an agent token or a delegated request on waive, bulk waive and defer (its message is check-neutral since #3761), and the reviewer-side `server_check_requires_human` veto refuses confirm / supersede / auto-close. Pinned end to end by `TestPermissionDrift_PullRequestReportToHumanWaive` (PR-opened report → background check → forge → concern store → audit log → waive handler: an agent waive is 403 and the row reads back open; a human waive with a reason succeeds).
- **Residuals (stated, not fixed).**
  - **Async window.** The check runs after the report returns, so a merge decided in the seconds before the concern lands is not blocked by it; review settlement and acceptance take minutes.
  - **Per-pass deltas.** Nothing auto-clears a raised concern: a widening in pass N stays raised even if pass N+1 omits it, and a widening undone in a later pass leaves a concern a human clears.
  - **The `DocumentBaseCommit` fallback is not proven to be the run's base (#3902).** A non-child run with no readable `pull_request_opened` entry on the stage (notably a decomposed PARENT, whose `consolidated_pr_opened` payload carries only a URL) reads the extension at `run.DocumentBaseCommit`, the default-branch head pinned at admission: a run based on a non-default branch, or a default branch that moved between admission and the lineage checkout, reads it at another commit, so a surface declared only at the real base can be missed. Rows minted before migration 0091 have a nil `DocumentBaseCommit` and fail closed with `surfaces_ref_unresolved` (noise, never a miss).
  - **A child's slice cut point includes producer-wave changes.** For a wave-0 slice the `child_pushed` `base_sha` is the parent base; for a later wave it is the integrated producer-wave merge, which carries producer slices' agent-authored changes. The backstop for a declaration a producer slice removed is the consolidated trigger's cumulative parent base → consolidated head check.
  - **Run-base lookups are stage-scoped** (`newestStageEntry`): an entry recorded on a different stage row of the same run is not consulted — a non-child run then falls back to `DocumentBaseCommit`, a child fails closed.
  - **The extension ref is pinned, not tracked.** A surface the base branch declares AFTER the run's base (e.g. after the PR opened) is not checked on later passes; the consolidated trigger still reads at its request base.
  - **A Restriction-polarity file deleted on the base branch raises on the conflict-resolution pass (known noise, approval condition 2).** A governed file present at the previous head but absent at both the pushed head and the base branch keeps the superset comparison, so every restriction it carried raises as a widening for a human to clear (`TestPermissionDrift_ConflictIntersectAbsentBaseBranchSide`) — the fail-closed direction.
  - **Conflict resolution reads the base branch's CURRENT tip**, which may have moved past the commit the pass merged: a widening the resolution introduced is dropped from the intersection if the base branch has SINCE gained the identical grant (same key and value) — the base branch then carries it independently of this run.
  - **The dedupe mutex is per process** (as the diff secrets check): two replicas can each mint a row for one key — a duplicate, never a miss.
  - **A rename whose source the forge does not report** raises `rename_source_unknown` on EVERY glob surface (noise, never a miss); a rename inside one Restriction-polarity glob surface reads as a widening at the source.
  - **A consolidated branch more than 250 commits ahead of base keys unevaluable rows by `@`** (the capped listing names no provable tip), so a waived unevaluable row there never suppresses a repeat — noise for a human, never a miss.
  - **The compare-failure ledger fallback** (prescribed by the #3935 approval conditions) keys a `compare_failed` row by the newest `integration_commit_recorded` `merge_sha`, which is not proven to be the branch tip: a commit pushed to the consolidated branch after that integration (with no new ledger entry) shares the key, so a waived `compare_failed` row there suppresses a second compare failure on a later consolidated check.
  - **Unevaluable key format change (#3935):** rows minted before it carry `…|unevaluable` without a head, which no longer suppresses `…|unevaluable@<head>`, so a still-unevaluable file raises one duplicate on its next check.
  - **First-review ordering.** The security persona (E80.5 / #3762) cannot attach to the FIRST implement review on a drift hit, which predates the check; the persisted concerns and the detected entry are the seam it consumes.

## Runner-observed counterfactuals (`counterfactual_observed.go`, E85.3 / #4143)

The server half of plan-declared counterfactuals. The runner executes each entry of the approved plan's `counterfactuals` array against a scratch clone of the verified head and folds one record per entry into the signed bundle's `gate_evidence.counterfactual_observations` (contract: `runner/README.md`); this section owns what the backend does with them.

- **Placement.** `trace.go` calls `recordCounterfactualObservations` inside the raw-variant guard, right after `recordGateIsolation` and BEFORE the budget short-circuits and the agent-failed branch, so a recorded observation is on the chain before any stage failure. A redacted upload records nothing.
- **Row shape.** One `counterfactual_observed` audit row per ADMITTED observation (see Admission): system actor, stage-anchored, payload `{stage_id, content_hash, source:"runner", index, control_path, test_run, test_package, head_sha, tree_sha, baseline, observed, reason, duration_ms, timeout_seconds, mutation_sha256}` (the flat `bundle.CounterfactualObservationEvidence` embedded at the top level). It is derived from the runner-signed bundle, not from an agent-claimed sidecar — the authority difference from the #2929 self-report (`fixup_counterfactuals`). INTERNAL: registered in `audit.KnownCategories`, no `issuecomment` activity line, so `docs/issue-comment-surfaces.md` is untouched.
- **Admission (`admitCounterfactualObservations`).** Before any row, each observation must pass, else it is WARN-logged with a NAMED error and gets no row: at most `plan.MaxCounterfactuals` (8) per bundle (the tail is `errCounterfactualTooMany`); `index` must be a position in the run's approved plan's `counterfactuals` (`errCounterfactualIndexUndeclared`, read through the same `loadApprovedPlanForRun` the implement prompt served the specs from — a wired run with no approved plan declares none, a failed plan read records nothing, and only a server with no artifact/run store skips membership and checks the index sign); `observed`, `baseline` and a non-empty `reason` must each be in its CLOSED vocabulary (`errCounterfactualObservedUnknown` / `errCounterfactualBaselineUnknown` / `errCounterfactualReasonUnknown`). The vocabulary is pinned from BOTH modules against `testdata/wire/counterfactual_outcomes.json`. A rejection error quotes the offending value cut to 1 KiB plus a `…[truncated]` marker; an admitted value is an enum member, so nothing stored is truncated. **A failed approved-plan read is a DELIBERATE fail-closed** (#4360): index membership is undecidable, so that POST records no `counterfactual_observed` row and raises no `control_unpinned` concern, even for a GREEN. The operator-visible symptom is the WARN `counterfactuals: approved-plan read failed; recording no counterfactual_observed (index membership undecidable)`. The runner ships the raw bundle once, so recovery is a stage retry or a fix-up pass, which re-executes the counterfactuals. Contrast the dedup read below, which fails OPEN: a duplicate row is harmless, while admitting an index the plan may not declare is not.
- **Dedup.** An observation whose (`stage_id`, `content_hash`, `index`) row already exists is skipped, so a re-POST of the same raw bundle appends nothing and raises nothing; a bundle repeating an index records it once. A failed dedup read WARN-logs and appends every admitted observation (a duplicate row is harmless; a lost record is not).
- **Concern rule — GREEN only.** An observation whose `observed` is exactly `green` (mutating the control left the named tests passing) raises ONE implement concern: provenance `server_check`, severity `medium`, concern category `control_unpinned` (a review-concern category on the tolerant `review_concerns.category` column, NOT an audit category), `check_key` `control_unpinned|<index>|<control_path>`, `origin_review_sequence` = that observation's `counterfactual_observed` row, and a note naming the index, control path, selector, package and 12-char head SHA. `red` passes silently; `timeout` and `error` are recorded and raise nothing.
- **Concern dedup.** Under the package-level `counterfactualRaiseMu`, a GREEN is skipped when a `server_check` row with the same `check_key` is on THIS stage in an open, `waived` or `deferred` state (`addressed`/`superseded` do not suppress), so a later bundle with a different content hash re-observing the same GREEN raises nothing new. A `ListByRun` error fails OPEN to raising. A failed append raises no concern (no concern without its origin row); a failed `InsertRaised` WARN-logs and leaves the row as the record. Same single-process residual as the diff secrets check.
- **What it holds.** Being `server_check`, the concern is gate-visible on the gate view's `open[]` and only a human can waive or defer it (`refuseNonHumanServerCheckClear`, 403 `concern_requires_human` for an agent token or a delegated request). No new merge hold exists: an OPEN concern already fails the delegated `may_approve` / `may_merge` conditions (`delegation.evalGatesResolvedCIGreen` requires zero open concerns), so `AutoDriveRunGate` cannot settle the run while it is undispositioned. The human `fishhawk_merge_run` verb does not consult open concerns (pre-existing), so only DELEGATED settlement is held.
- **Tests.** `counterfactual_observed_test.go`: the pgtest cross-boundary arm (handler → stored rows → gate view → human-only waive), the `AutoDriveRunGate` hold, and one arm per failure mode above.

## Gate decision view (`gateview.go`, E48.13 / #1960)

`GET /v0/runs/{run_id}/gate-view` (`handleGetRunGateView`) answers "what is still open at this gate and why" in ONE read, replacing the `getRun` + `listRunAudit` stitch an operator otherwise runs at a review/fix-up gate. The run-status concerns block (`runs.go::buildRunConcernsPayload`) carries only a BOUNDED note-derived `short_summary` label per concern (at most 100 bytes, one line; #2488) — enough to recognise a defect, not the full prose; this surface returns the untruncated `concern.Concern.Note` intact.

- **Run-status concerns block presence + open-set equality (#3043).** `handleGetRun` emits the `concerns` block on any SUCCESSFUL `ConcernRepo.ListByRun` read (it was `ListOpenByRun` before E45.83 / #3618 — see below), INCLUDING a zero-open run (`open:0`, `by_state:{}`, `open_implement:0`, `superseded_implement:0`, `items:[]`) — `buildRunConcernsPayload` no longer returns nil on an empty set. The field is left ABSENT only on the UNAVAILABLE paths (`ConcernRepo` unwired, or the read errored — both warn-logged), so PRESENCE is authoritative: present == a store read, absent == unavailable, never "zero". The block's open set is BY CONSTRUCTION the same rows the gate view's `open[]` shows for the same run — the run-status/gate-view open-set equality invariant (pinned by `runs_get_test.go::TestGetRun_ConcernsMatchGateViewOpenSet`). The two now reach that set by different routes: the gate view still calls `ListOpenByRun`, while the run-status block derives it in Go from `ListByRun` with `concern.State.IsOpen()` — the SAME predicate the query's `WHERE` clause duplicates, over the same `ORDER BY`, which is why the equality survives the #3618 read swap (and is separately pinned by `TestGetRun_DerivedOpenSetEqualsListOpenByRun`). `buildRunConcernsPayload` also computes `open_implement` — the AUTHORITATIVE count of open `implement`-stage concerns over the FULL open set — which the MCP `review_action_hint` transports and reads rather than re-deriving a count from the bounded `items` list.
  **Since E45.83 / #3618 the read is ONE `ListByRun`, not `ListOpenByRun`**, and `buildRunConcernsPayload` derives the open set itself with `concern.State.IsOpen()` — the same predicate `ListOpenReviewConcernsByRun`'s `WHERE` clause duplicates, over the same `ORDER BY (origin_review_sequence, created_at, id)` — so `open`/`by_state`/`open_implement`/`items` are membership- AND order-identical to the pre-#3618 payload (pinned by `runs_get_test.go::TestGetRun_DerivedOpenSetEqualsListOpenByRun`, which compares the derived set against `ListOpenByRun`'s own result for a mixed-state run). The same single pass counts `superseded_implement`: the AUTHORITATIVE number of `implement`-stage concerns a RETRY DISCARDED (state `superseded`), which the open set structurally cannot express — a retry supersedes the prior attempt's open implement concerns, so `open_implement` drops to 0 and a discard reads as a settle. A plan-stage supersede (the #2065 revise) is EXCLUDED: no implement fix-up can route one. The count is not scoped to the latest attempt (the fail-safe direction: a discard from any attempt is one the merge candidate never answered), and it is always present on a successful read, so `0` is authoritative rather than an absent key. Cost: the single-run read now pays every concern row for the run rather than the open ones — still ONE query, bounded by reviewer output on one run; the list endpoint is untouched and still pays no concern read at all. The open-plan-concern gate around `loadApprovalConcernClaims` keeps its OWN `IsOpen()` filter so a run whose only plan concerns are SETTLED still skips the audit read.

- **Response shape.** Each OPEN concern (`raised`/`addressed_pending`/`reopened`) carries its FULL `note`, `severity`, `category`, `reviewer_model`, `origin_review_sequence`, a derived `round` (implement-only), `state_reason`, `has_suggested_patch`, the reviewer's `new_evidence` and the re-raise lineage tag `settled_ref` (#2353, both `omitempty` — a no-evidence concern leaves the payload byte-identical, and rows minted before migration 0069 carry neither), plus `fixups[]` and `resolutions[]`. The settled ledger (`waived`/`deferred`/`addressed`/`superseded`, each with `state_reason` and the same two evidence fields — the ledger is what an operator re-reads when judging whether a re-raise is legitimate) and the run's `concern_relitigation_suppressed` entries ride along. `suggested_patch` diff text stays elided as `has_suggested_patch` (token-dominant, not decision prose) — the response is sized by SCOPING (the optional `stage_kind=plan|implement` filter), not truncation.
- **History is reconstructed from the immutable audit payloads**, because `concern.StateReason` is OVERWRITTEN on every transition (`MarkAddressedPending` writes the routing reason, then `applyConcernResolutions` overwrites it with the re-review note) — there is no stored per-round history. `fixups[]` join each `stage_fixup_triggered` whose `concern_ids` names the concern (contributing `{sequence, reason}`) to the outcome (`apply_path`/`head_sha`) of the earliest following `fixup_pushed`/`fixup_no_changes` (`pending` when none yet). `resolutions[]` join each `implement_reviewed`/`plan_reviewed` payload's `concern_resolutions` entries keyed by concern ID. `round` = `1 +` the count of same-stage `stage_fixup_triggered` sequences below the review sequence (the `review_action_hint.go::latestRoundConcerns` convention); the handler sorts fetched audit entries by `Sequence` defensively rather than relying on repo order.
- **Disputes (E48.103 / #2551).** Each open concern carries `disputed` plus `disputes[]`. `disputed` is DERIVED FROM DURABLE EVIDENCE — this row's own open state plus a `confirmed` entry in the authoritative `implement_reviewed`/`plan_reviewed` `concern_resolutions` join — so a confirmation that did not settle the concern is always visible, INDEPENDENT of the best-effort `concern_resolution_vetoed` append. `disputes[]` is enrichment decoded from that category (added to `gateViewHistoryCategories`, so an unreadable read degrades via `history_incomplete` + a named gap) carrying `{sequence, round, veto_reason, resolution, confirming_reviewer_model, raising_reviewer_model, note}`; it can be EMPTY on a disputed concern when the veto append failed. A concern confirmed and later reopened also reads as disputed — the same operator-visible fact (a confirmation is on record that did not settle it). The settled ledger is untouched: a vetoed concern is by construction still open. **`disputed` is SCOPED to refused CONFIRMS (#3319):** a refused `reopened` appends its `disputes[]` row (so the refusal is visible as detail) but does NOT set `disputed` — the concern is still `addressed_pending`, so reporting it as disputed would tell the operator a confirmation failed to settle it when the opposite happened. A legacy veto payload carries no `resolution` string and is treated as a confirm, so no pre-#3319 gate view changes shape (`TestGateView_LegacyVetoPayload_EmptyResolution_StillDisputed`).
- **Degradation is visible, never silent.** `AuditRepo` nil, or any per-category `ListForRunByCategory` error, returns 200 with the concerns intact, `history_incomplete=true`, and `history_gaps` naming each failed category; a single malformed payload entry is skipped warn-only while its siblings still join. `ConcernRepo` unconfigured → 503 `gate_view_unconfigured` (mirrors `fixup_unconfigured`); `RunRepo` unconfigured → 503 `run_repo_unconfigured`; unknown run → 404; bad `stage_kind` → 400; a `ConcernRepo.ListByRun` error → 500 `internal_error`. **Auth mirrors `handleListRunAudit`'s read posture** (full reviewer prose must not be anonymously readable, #1960 authz): a run-bound `mcp:run:<uuid>` token is authorized by the cross-run subject guard alone — it may read only its own run (403 `cross_run_gate_view`, mirroring the fix-up handler; a malformed `mcp:run:` subject → 401 `authentication_required`) — while every other caller must clear the `read:audit` scope (anonymous → 401 `authentication_required`, a token missing the scope → 403 `insufficient_scope`, cookie-session operators bypass per `requireWriteScope`).
- **Captain block (E76.3 / #3766, ADR-083 rule 4).** `gateViewCaptainFor` runs in the HANDLER only (after `buildGateView`, so `attention.go`'s builds are untouched) and adds an `omitempty` `captain` block: the repository's current captain resolved through `currentCaptain` in the RUN's account (`runs.account_id`, never the request identity), `vacant`, `non_captain_approvers` (each HUMAN subject with an `approve` on the run's `approval_submitted` rows that is not the captain — agent-kind subjects and rejects excluded, de-duplicated in chain order) and `note` (`approved by X; captain is Y`, only when that list is non-empty). It is INFORMATIONAL: eligibility, counting and the advance decision never read it (ADR-083 rule 1). Degrades per the trichotomy: `CaptainStore` unwired → no block and NO gap (byte-identical to pre-E76.3); vacant → `{vacant: true}` with no approval read; captain read failed or the run account is not a UUID → no block + `history_gaps` `captain_record`; approval read or decode failed → no block + `approval_submitted`. Tests: `gateview_captain_pg_test.go` (real handler over pgtest, one per basis and gap) and `gateview_test.go` (unwired and non-UUID-account).

- **Precedent block (E75.4 / #3732).** At an open human gate the view also carries `precedent` (§ "Precedent at the gate"). It makes this READ handler append one best-effort, fingerprint-deduped `precedent_surfaced` entry the first time a given block is shown — the only write the gate view (and `handleGetRun`) performs; a failed append never changes the 200. **It is omitted for the run-bound `mcp:run:<uuid>` identity the auth bullet above admits.** That token clears this surface on the cross-run subject guard alone, and the block carries reason EXCERPTS from OTHER runs' decisions, so handing it over would breach ADR-082 rule 6 ("never an agent input") at the API even though no prompt renders it. A run-bound read therefore gets the pre-E75.4 response byte-for-byte and appends nothing (§ "Precedent at the gate" → Isolation).
- **Gate isolation block (E51.2 / #2135, `gate_isolation.go`).** The trace handler appends one stage-scoped `gate_isolation_recorded` row per RAW upload whose `gate_evidence` carries a `gate_isolation` member — inside the raw-variant guard and BEFORE the budget short-circuits and the agent-failed branch, so a refused stage's isolation path is on the chain before the stage fails — deduplicated on (`stage_id`, `content_hash`) so a re-POST of the same raw bundle appends nothing. `gateIsolationForRun` adds an `omitempty` `gate_isolation` block: the NEWEST row's fields plus `worst_class` / `worst_stage_id` / `worst_sequence`, the most severe class recorded on ANY stage (refused > fallback > container, newest on a tie), so an earlier fallback or refusal is never masked by a later container stage. Run-level (no `stage_kind` filter). A list error or ANY undecodable row degrades WHOLESALE — no block, a `gate_isolation_recorded` `history_gaps` entry and `history_incomplete` — because a worst class computed around an unreadable row could hide a refusal.
- **Absorbed verify flake row (#4229, `verify_flake_absorbed.go`).** The trace handler appends one stage-scoped, system-actor `verify_flake_absorbed` row per RAW upload whose `gate_evidence` carries a `flake_absorbed` member (the runner absorbed an outside-only verify failure at the verify-fix loop's terminal iteration: the verified tree passed ONE scoped re-run of the untouched failing packages, not the full test loop). Payload `{stage_id, content_hash, iteration, tree_sha, packages, package_dirs, tests, failed_modules, patch_coverage}` (the flat `bundle.FlakeAbsorbedEvidence` embedded at the top level, pre-redacted by the runner; ModeExact wire pair, golden `testdata/wire/verify_flake_absorbed_evidence.json`). Called inside the raw-variant guard immediately after `recordGateIsolation`, on its contract: deduplicated on (`stage_id`, `content_hash`), a failed dedup read appends anyway, and no `AuditRepo` / no evidence / no member appends nothing (an unparsable `gate_evidence` or a failed append WARN-logs). INTERNAL: registered in `audit.KnownCategories`, no `issuecomment` activity line and no gate-view block. Tests: `verify_flake_absorbed_test.go`.
- **Merge-candidate verdict block (E87.2 / #4293).** `merge_candidate_verdict` is computed in the handler only (`gateViewMergeCandidateVerdictFor`) and is always present; a read failure omits it with a `merge_candidate_verdict` `history_gaps` entry. Contract: § "Merge-candidate verdict transfer".
- **Fan-in conflict block (#4343).** `fan_in_conflict` (`fanInConflictForRun`, handler only; run-level, so `stage_kind` does not filter it) is a decomposed parent's UNRESOLVED fan-in conflict: `{kind, sequence, conflicting_slice_index?, conflicting_child_run_id?, base_ref?, base_sha?, base_attribution?, conflicting_base_paths (always an array), compare_truncated, remedy}`, read from the run's `slices_integrated` and `slice_integration_conflict` rows. **Selection.** (1) A `slice_integration_conflict` newer than the newest `slices_integrated` (or with none) → `slice_conflict` from its payload; the remedy says the conflicting child SUCCEEDED, so `fishhawk_resume_run` refuses it (`recovery_not_eligible`), and names the restart (`fishhawk_start_campaign_item_run` / `fishhawk_start_run`). A `slices_integrated` row is written only by a clean pass, so a newer one means every slice merged. (2) Otherwise the newest `slices_integrated` with `base_fold_outcome` `conflict` → `base_fold_conflict` from its `base_fold_conflict` member plus `base_ref` / `base_sha`; the remedy says the head is clean but behind `base_ref` and that `fishhawk_rebase_run_branch` authorizes the bounded #3202 / #4304 conflict-resolution pass at merge. It stays observable until it is ACTUALLY superseded (approval condition C1): by a newer `slices_integrated` whose fold landed (`merged` or `created_at_tip`), a newer `branch_rebased` or `conflict_resolution_pushed`, or the run's PR merging (`runPRObservablyMerged`). A newer `not_attempted` or `deferred_children_in_flight` row does NOT supersede it — a repeated consolidate or `/integrate-wave` (no stage-state guard) writes `not_attempted` without folding. An `already_contained` row does not supersede it either (C1's list is closed); reaching it after a conflict needs the base merged in by a rebase or resolution push, which supersedes on its own. (3) Otherwise omitted. No `AuditRepo` → omitted, no gap; a list error or an undecodable payload (any `slices_integrated` row, since the conflict row and every newer superseder are read) → omitted with a `fan_in` `history_gaps` entry. Tests: `gateview_test.go` (`TestGateView_FanInConflict_*`, one per mode) and the cross-boundary `fanin_gateview_test.go` (real fan-in through consolidate, then a no-op `/integrate-wave`, then the real gate-view handler).
- **Merge-candidate staleness block (E85.4 / #4147, `merge_candidate_staleness.go`).** `merge_candidate_staleness` (`gateViewMergeCandidateStalenessFor` in the handler only, never `buildGateView`; also on `GET /v0/runs/{run_id}` via `mergeCandidateStalenessFor`, single-run read only) reports whether an open decomposed parent's consolidated PR head contains the CURRENT tip of its base: `{state fresh|stale|undetermined, head_sha?, base_ref?, base_sha?, behind_by?, behind_by_lower_bound, reason?, next_step?, checked_at, note}`. **Scope (the Captain's 2026-10-15 ruling, option b).** Read-time forge observation on the run read and the gate view is a bounded, READ-ONLY extension of the ADR-090 merge-candidate surface: three GETs (`GetPullRequest`, `GetBranchSHA`, `CompareCommits` of the live head against the RESOLVED base sha, the merge gate's D1 call order), no forge write, no audit append, no stage change, no background sweep, no verify command. The rebase stays the operator's move (`next_step` `fishhawk_rebase_run_branch` on stale); auto-advance is reserved for a separate ADR-090 amendment. **Eligibility.** `nil, nil` for a nil / terminal run, no PR URL, no `RunRepo` / `AuditRepo`, or no decomposition child (`ListRuns{DecomposedFrom, Limit: 1}`, the `DispatchConsolidatedReview` predicate, which also excludes every child run); a PR the forge reports merged OR closed omits it — the closed half is a deliberate addition beyond `mergeCandidateGate`, which checks only `Merged`. **Fail-closed.** The D7 anchors (no client, no / zero installation, unparseable repo or PR number) → `forge_not_probeable`; a PR read error or empty head / base → `pr_read_failed`; a base-ref error or 404 → `base_tip_unresolved`; a compare error → `compare_failed`; any read past the deadline → `read_timeout`. Never `fresh` on a failed read; `reason` is an enum with no error text. A children-list or chain-head read error is returned: the gate view records a `merge_candidate_staleness` `history_gaps` entry, the run read warn-logs and omits. **Cost bound.** An in-process cache keyed on (run, `gateViewCurrentHead`) with a 60s TTL (`s.nowFunc`), each computation under a 5s deadline (`mergeCandidateStalenessReadTimeout` seam) on a context detached from the caller's cancellation; the mutex guards map access only and expired entries are pruned on store. The bound holds per cache entry ONCE STORED: concurrent first misses for the same run may each issue the reads (no singleflight), so it is not a hard rate limit. A head change Fishhawk records busts the key at once; a base advance or an unrecorded push shows within one TTL (`checked_at`). Tests: `merge_candidate_staleness_test.go` (`_Modes` one subtest per branch through both handlers, `_ReadErrors`, `_CacheBounded`, `_UnwiredRepos`, `_ReadOnly` with its go/parser guard and `_ReadOnlyGuardIsNotVacuous`, and the cross-boundary `_EndToEnd` over `newBaseTipHarness`: real fan-in, both handlers, then the real rebase verb busting the cache). The MCP `next_actions` fold that names the rebase lives in `backend/internal/mcpserver`.

## Run concern listing (`run_concerns.go`, #4101)

`GET /v0/runs/{run_id}/concerns` (`handleListRunConcerns`) lists a run's review concerns and, with `include_children=true`, its decomposition children's — the one call an operator makes to collect concern ids across a fan-out for bulk-waive / defer, instead of one `gate-view` read per child. It is a pure read: no audit row, no new store method, no migration.

- **Query.** `state=open` (default; `concern.State.IsOpen()` — `raised`/`addressed_pending`/`reopened`) or `state=all` (every row). `include_children` is parsed with `strconv.ParseBool` (absent = false). Anything else → 400 `validation_failed` with `details.field` (`run_id` / `state` / `include_children`).
- **Children are `DecomposedFrom`, never `ParentRunID`.** They are paged through `listAllDecomposedChildren` (past the 100-row cap); `run.ChildParamsFrom` sets `parent_run_id` for every child kind, so a `ParentRunID` walk would sweep in recovery children. A recovery child's concerns are never listed.
- **Ordering.** The parent's rows in `ListByRun` order (origin sequence), then each child's rows in `ListByRun` order, children ordered by `sortDecomposedChildren` (SliceIndex ascending with nil last, then CreatedAt, then ID) — the SAME helper `childApprovedAmendmentScopePaths` uses, so both surfaces order children identically.
- **Response.** `run_id`, `state`, `include_children`, `count`, `items[]` (never null) and — only when `include_children=true` — `children[]` (`run_id`, `slice_index`, run `state`, `item_count`; a non-nil `[]` on a non-decomposed run, so presence means the children were read). Each item: `id`, `run_id` (the OWNING run), `child_run_id` + `slice_index` (child rows only), `stage_id`, `stage_kind`, `severity`, `category`, `state`, `reviewer_model`, `reviewer_role`, `short_summary` (`concernShortSummary(DisplayNote())`, the bounded label — never the full note, which stays on the gate view), `origin_review_sequence`, `has_suggested_patch`, `provenance`.
- **Fail-closed reads.** `GetRun` (non-NotFound), the parent's `ListByRun`, `listAllDecomposedChildren` and EACH child's `ListByRun` error → 500 `internal_error`; never a partial list, because a missing child would read as "no open concerns there" to an operator collecting ids. The failing child's id is in the 500 MESSAGE: the 5xx detail redactor (`errors.go`) keeps only allow-listed keys, so `details.child_run_id` reaches only the server log (same `error_ref`).
- **Auth mirrors the gate view.** `requireRunAccount` authorizes the PARENT's account (children are minted from the parent, so they share it). Then a run-bound `mcp:run:<uuid>` token may list only its own run (403 `cross_run_concerns`) and may NOT pass `include_children=true` (403 `cross_run_concerns` — children are other runs); a malformed `mcp:run:` subject → 401. Every other caller must clear `read:audit` (`scopeGateViewRead`): anonymous → 401, a token missing it → 403 `insufficient_scope`, cookie sessions bypass. 503 `concern_store_unconfigured` when either repository is unwired; 404 `run_not_found` for an unknown run.
- **Tests.** `run_concerns_test.go` (package fakes, one test per branch) and `run_concerns_pg_test.go` (real Postgres through the REGISTERED route via `s.Handler()` with a real bearer token: parent + decomposition child + recovery child + a waived row).

## Plan-gate surface sweep (#763)

`surface_sweep.go::runSurfaceSweep` — called from `handleShipPlan` (`plan.go`) immediately after `runScopePrecheck` and before `runPlanReviews`. Flags sibling surfaces a plan must move in lockstep with: when `scope.files` touches one path of a known multi-surface pattern but omits a required sibling, the missing sibling is recorded.

- Uses a **static pattern registry** (`var surfacePatterns`), NOT call-graph analysis — broadening to call-graph is explicitly deferred. Registry entries, keyed to cited production misses:
  - *Actor @-mention render surfaces* — `status_template.go` ⇄ `notifier.go` (#751/#755, the wrong-user-ping class), each a trigger AND sibling of the other so touching one flags the missing peer.
  - *Audit kind requires surfaces doc* — triggers `notifier.go` + `server/pullrequest.go` (audit-kind emitters), sibling `docs/issue-comment-surfaces.md` (#742/#748, per the CLAUDE.md mandate).
  - *Work-management schema requires every mirror* — `docs/spec/work-management-v0.schema.json` ⇄ `backend/internal/workmgmt/schemas/work-management-v0.schema.json` (the only two mirror copies per `scripts/sync-schemas`), added to catch the #1101/#1006-case-2 kill-switch field-add.
- `evaluateSurfaceSweep(scopeFiles, patterns)` is the pure matcher: exact slash-normalized (`filepath.ToSlash`) path equality routed through a glob-ready `pathMatches` helper, reporting only siblings ABSENT from scope (so a self-referential pattern never flags a present sibling) with `MissingSiblings` sorted deterministic; `notifier.go` alone fires BOTH the mention and doc patterns (two findings).
- Writes a `plan_surface_sweep` audit entry (payload `SurfaceSweepPayload{findings, scanned_files}`, `findings` an empty array not null) **even on a clean sweep** so a reader distinguishes "checked and clean" from "never checked".
- **Guards `RunRepo` + `AuditRepo`**, advisory + fail-open: it resolves the run first (so it never records an orphan advisory entry against a run that does not exist) and any failure — run not resolvable, unparseable plan body, audit-append failure — WARN-logs and returns without unwinding the upload. The run row is load-bearing since #3620: the forbidden-criterion rule below reads the run's implement-stage `forbidden_paths` off it via the fail-open `resolveImplementConstraints`, so the sweep no longer depends on `scope.files` alone.
- A `surface_sweep_test.go` test `os.Stat`s every registry trigger/sibling path so a future rename breaks loudly rather than silently disabling the sweep.

### Audit-category rule (#3410)

`audit_category_sweep.go::evaluateAuditCategoryRule(parsedPlan, isKnown, exemptions)` — pure (no receiver, no I/O), called from `runSurfaceSweep` after the per-sub-plan loop and carried in the SAME `plan_surface_sweep` entry (no new audit category, so the rule does not itself trip what it teaches). Unlike every `surfacePatterns` entry it is **PROSE-triggered, not path-triggered**: run 49f642b8 introduced a new audit category with `backend/internal/audit/categories.go` out of scope and discovered the obligation only when the verify gate's `TestKnownCategoriesCoversEmittedCategories` failed.

- **Mentions.** `planAuditCategoryMentions` walks the plan's own text in fixed order — `summary`, each `approach[].description`, `verification.test_strategy`, each acceptance criterion's `statement` then `verify_hint`, `risks_and_assumptions[]`, each decomposition sub-plan's `scope_hint` — and `extractAuditCategoryMentions` runs three bounded regex shapes per field: (A) keyword-first `audit <category|kind|entry|row|event> "tok"` (token MUST be quoted/backticked, so `audit entry precedes plan_reviewed` captures nothing); (B) token-first `"tok" audit <category|entry|row|event|marker>` (quotes optional); (C) field form `category: "tok"`. Every shape anchors the token at a non-word left boundary (`Mixed_new_cat audit entry` yields NO token, not `ixed_new_cat`); Go regexp has no lookahead, so the lowercase snake_case + at-least-one-underscore + not-`fishhawk_*` rule is a second filter step (`isAuditCategoryToken`). Mentions are deduped by token, first location wins.
- **Evaluation.** Union scope = top-level `scope.files` ∪ every declared sub-plan scope (slash-normalized). Returns nothing unless some scoped path has the `backend/` prefix (the rule is fishhawk-backend-specific, like every registry pattern's literal paths — a plan for another repository never draws it); nothing when `backend/internal/audit/categories.go` is in the union (obligation satisfied); nothing when every token passes `audit.IsKnownCategory` (the exactly-decidable half). Otherwise one `SurfaceSweepFinding` per distinct unknown token (sorted, capped at 10): `Pattern: "new audit category requires registry"`, `TriggerPath: audit category "tok" named at <location>`, `MissingSiblings: [backend/internal/audit/categories.go]`, and the new **`Category`** field (`json:"category,omitempty"` — set ONLY by this rule, so every path-triggered payload stays byte-identical). No `SubPlanTitle`: the mention is plan-level.
- **Exemption.** A plan-declared `surface_sweep_exemptions` entry `{pattern: "new audit category requires registry", sibling: "backend/internal/audit/categories.go"}` suppresses every finding of the rule and is recorded as ONE `AppliedExemption` carrying the reason — the reviewer-challengeable escape hatch for a heuristic false positive (a same-shaped non-category token named as an audit entry).
- **Render.** `planGateEvidence` copies `Category` onto `prompt.SurfaceSweepFindingEvidence`; the plan-review renderer branches on it to a distinct `NEW AUDIT CATEGORY (<pattern>)` line naming the token, its location, the registry file, `TestKnownCategoriesCoversEmittedCategories` and the exemption key. `Category == ""` renders the unchanged `MISSING SIBLINGS` line. `fishhawk_get_plan`'s `surface_sweep` block shows the finding through the existing `pattern`/`trigger_path`/`missing_siblings` fields (the token is embedded in `trigger_path`); its `SurfaceSweepFinding` mirror in `backend/internal/mcpserver` ignores the unknown `category` key under `encoding/json` defaults.
- **Residuals, stated honestly.** Prose-only detection: a category the plan never names is not caught — the runner-side `OUT-OF-SCOPE PATHS` hint in the verify-fix prompt (`runner/cmd/fishhawk-runner/verifyfixscope.go`, #3410) covers that residual once the gate names the file. The `isKnown` filter runs against the SERVER binary's registry, so a stale dev server may flag a category registered on main after it was built; advisory, and the exemption clears it. It is NOT added to `surfacePatterns` / `surfaceCouplingPatternsForPrompt` (prose-triggered, and `TestSurfacePatternsExistOnDisk` stats registry paths), so the plan-prompt sibling map is unchanged; `AGENTS.md`'s "Audit-category change checklist" carries the first-shot rule to the planner instead.
- Tests: `audit_category_sweep_test.go` (`TestExtractAuditCategoryMentions`, `TestPlanAuditCategoryMentions_WalksEveryField`, `TestEvaluateAuditCategoryRule`), `surface_sweep_test.go::TestRunSurfaceSweep_AuditCategoryFinding` (the done-means on the real wiring + persisted-payload key presence/absence), `plan_test.go::TestPlanGateEvidence_AuditCategorySeam` (body → sweep → evidence → rendered prompt), `prompt_test.go::TestBuild_PlanReview_GateEvidence_AuditCategoryRenders`.

### Forbidden-criterion rule (#3620)

`forbidden_criterion_sweep.go::evaluateForbiddenCriterionRule(parsedPlan, forbiddenPaths, exemptions)` — pure (no receiver, no I/O), called from `runSurfaceSweep` after the audit-category rule and carried in the SAME `plan_surface_sweep` entry (no new audit category, no new issue-comment surface). PROSE-triggered like the audit-category rule, not path-triggered. It closes the class where an acceptance criterion demands a change to a path the implement stage's `forbidden_paths` structurally forbids the agent from touching: `forbidden_paths` is correct and stays — an implement agent must not rewrite the governance document that constrains it — so such a criterion is unsatisfiable from inside the loop, undetectable at authoring time, and rediscovered by paid reviewers every round (run f199dcf1: flagged six times across three rounds, unfixable every time).

- **What it reads.** `planCriterionPathMentions` walks `verification.acceptance_criteria` in declared order and, per criterion, `extractCriterionPathTokens` over the `statement` then the `verify_hint`. Two bounded shapes: (A) a token inside a backtick or a straight/curly single/double quote, taken verbatim; (B) a BARE token at a non-word left boundary that carries a forward slash, with ONE trailing sentence-final period stripped (`see docs/spec/workflow-v2.md.` names the file). A bare unquoted WORD is deliberately never captured — that is what stops prose mentioning the word `LICENSE` or `NOTICE` from matching the same-named forbidden glob. Both shapes anchor at a non-word left boundary so a longer identifier yields the WHOLE token, never a matching suffix (`foo.fishhawk/bar` never yields `.fishhawk/bar`). A path named in BOTH fields of one criterion draws one finding, attributed to the statement; the same path in two criteria draws one finding each, because each criterion is separately unenforceable.
- **Why it reuses `backend/internal/policy` rather than a second matcher.** `forbiddenGlobForPaths` builds a synthetic `policy.Diff` of the extracted tokens (`Status` modified) and calls `policy.Evaluate` with the run's resolved `ForbiddenPaths` — the SAME matcher the post-implement gate runs, so the plan-time verdict is IDENTICAL to the verdict the implement stage would produce for the same path. This is the identity property `runScopePrecheck` already documents, and it is sound because `policy`'s `checkForbidden` delegates to `matchAny`, which calls `doublestar.PathMatch(pattern, f.Path)` and never reads `f.Status` (the property `mapFileOperation` in `scope_precheck.go` also relies on). A second glob matcher here could drift from the gate and produce a plan-time verdict the implement stage contradicts. `forbiddenGlobFromDetail` recovers WHICH declared glob matched by testing each declared pattern for containment in the quoted form `policy` emits, rather than parsing the violation's `Detail` text as a format.
- **Findings.** One `SurfaceSweepFinding` per distinct (criterion, token) pair, sorted by criterion id then token and capped at 10 (a prose blizzard cannot flood the gate evidence): `Pattern: "acceptance criterion requires a forbidden path"`, `TriggerPath: path "<tok>" named at acceptance_criteria[<id>].statement|verify_hint`, `MissingSiblings` **nil** (nothing is missing from scope — the path is structurally unreachable), and the two new fields **`ForbiddenPath`** / **`ForbiddenPattern`** (`json:"forbidden_path,omitempty"` / `"forbidden_pattern,omitempty"` — set ONLY by this rule, so every path-triggered and audit-category payload stays byte-identical). No `SubPlanTitle`: the criteria are plan-level.
- **Exemption.** A plan-declared `surface_sweep_exemptions` entry `{pattern: "acceptance criterion requires a forbidden path", sibling: "<the path>"}` suppresses EVERY finding for that token and is recorded as ONE `AppliedExemption` carrying the reason — the reviewer-challengeable escape for the false-positive class (a criterion that MENTIONS a forbidden path, e.g. asserting a rendered prompt NAMES `.fishhawk/workflows.yaml`, without requiring an edit to it). Applied exemptions are emitted in first-mention order for determinism.
- **Fail-open contract.** `runSurfaceSweep` now KEEPS the `GetRun` row (it was discarded) and calls the existing `s.resolveImplementConstraints` — the same fail-open reader `runScopePrecheck` uses on this request path — invoking the rule only when it returns `ok` AND `ForbiddenPaths` is non-empty. An absent, unparseable or implement-stage-less spec, or one configuring no `forbidden_paths`, skips the rule and the payload is byte-identical to before it existed. That is a SECOND parse of a value already parsed once per plan upload: a bounded in-memory cost, taken deliberately so the two advisory evaluators stay independent rather than threading state between them. A malformed glob in the spec yields a `policy` violation carrying an `invalid pattern` Detail and NO `Files`, so it contributes no mapping and cannot crash the rule.
- **Render.** `planGateEvidence` copies both fields onto `prompt.SurfaceSweepFindingEvidence`; the plan-review renderer branches on `ForbiddenPattern` BEFORE the `Category` branch to a distinct `UNENFORCEABLE CRITERION (<pattern>)` line naming the criterion location and token, the glob that forbids it, the consequence (`forbidden_paths` is enforced against the real diff, so every review round will re-raise it), the OPERATOR ESCAPE (land the in-repository half in this merge request and edit the forbidden governance file by hand in a second one — the same escape `docs/METHODOLOGY.md` § "Changes that span a forbidden governance file" states), and the exemption key. An empty `ForbiddenPattern` renders the unchanged `MISSING SIBLINGS` / `NEW AUDIT CATEGORY` lines. The planner-facing Unenforceable-criteria rule in `buildPlan` warns about the defect at AUTHORING time, not only at review time.
- **Residuals, stated honestly.** It reads acceptance-criteria prose ONLY: a criterion phrased "the verify gate enforces the new guard" requires a forbidden edit without naming any path and is NOT caught — the honest claim is that the rule closes the common, statically-detectable case, not the class. `summary` / `approach` / `risks_and_assumptions` prose is deliberately NOT scanned, because a plan legitimately discussing the `forbidden_paths` constraint would self-flag. Advisory throughout: it never refuses a plan. `fishhawk_get_plan`'s `surface_sweep` block shows the finding through the existing `pattern`/`trigger_path` fields (the token and location are embedded in `trigger_path`); the `SurfaceSweepFinding` mirror in `backend/internal/mcpserver` ignores the unknown `forbidden_path`/`forbidden_pattern` keys under `encoding/json` defaults, exactly as it does the #3410 `category` key.
- Tests: `forbidden_criterion_sweep_test.go` (`TestExtractCriterionPathTokens`, `TestEvaluateForbiddenCriterionRule` — one case per named failure mode, plus the determinism and cap cases), `surface_sweep_test.go::TestRunSurfaceSweep_ForbiddenCriterionFinding` (the done-means on the real wiring + persisted-payload key presence/absence + both fail-open branches), `plan_test.go::TestPlanGateEvidence_ForbiddenCriterionSeam` (body → sweep → evidence → rendered prompt), `prompt_test.go::TestBuild_PlanReview_GateEvidence_ForbiddenCriterionRenders` (the FULL rendered line) and `TestBuild_Plan_UnenforceableCriteriaGuidance` (the authoring guidance in the PLAN prompt).

### Cross-slice coupling pass (#1102)

`evaluateCrossSliceCoupling(parsedPlan, patterns)` (pure, no `Server` receiver / I/O) runs after the per-sub-plan sweep when `parsedPlan.Decomposition != nil`.

- It is the **INVERSE** of the #1062 same-file ownership gate (`plan/validate.go::checkCrossSliceSharedFiles`, which FORBIDS two slices declaring the same path): here a registered lockstep pattern's member files (`Triggers ∪ Siblings`, slash-normalized, deduped) are partitioned across **2+ DISTINCT decomposition slices**, so completing the seam would force a later slice to modify an earlier slice's file via a runtime scope amendment that can time out (#1035) and ship the seam broken.
- It partitions over ONLY sub-plans that DECLARE a scope (an undeclared scope inherits the parent's full `scope.files` — same invariant as #1062/#1077); a single slice listing the same member twice collapses to one claimant.
- Each split pattern emits one `CrossSliceCouplingFinding{pattern, slices[]}` naming each involved slice and the member files it owns (slices sorted by title, files sorted). The fix is **consolidation** (one slice owns the whole seam, or the shared file goes to the integrating slice), not dual declaration.
- Carried in the same `plan_surface_sweep` payload field `cross_slice_findings` (empty array not null on a clean sweep).
- The un-registerable request-type/client coupling class (#1006 case 1, `product_report.go`) — which a static file-pair registry cannot express — is addressed instead by a decomposer-prompt cross-slice-seam rule (keep an end-to-end contract's files in one slice or assign the shared file to the integrating slice). Advisory + fail-open like the rest of the sweep.
- MCP surface: `fishhawk_get_plan` adds `surface_sweep` (`SurfaceSweep{findings[], scanned_files, cross_slice_findings[]}`) decoded from the **newest** `plan_surface_sweep` entry (`loadSurfaceSweep` in `tools.go`; a schema-retry re-upload writes a second entry and the latest is authoritative).

## Plan-approval pre-Submit gate: add_scope_files fan-in (#2103)

`approvals.go::checkDecomposedAddScopeFiles`, wired into `handleSubmitApproval`'s `decision==approve && stage.Type==plan` block AFTER the `remove_scope_files` normalization and BEFORE `checkPlanScopeCap`. It refuses a plan-stage approve that supplies `add_scope_files` on a **decomposed** plan (one whose loaded artifact carries `Decomposition != nil`), because `add_scope_files` is persisted as a flat `[]string` and folded into the effective scope by `resolveApprovalAddScopeFiles`, which returns the SAME parent-approval paths to EVERY decomposition child with no per-slice filtering — so an added path lands in every slice's scope, violating the single-owner-file rule `plan/validate.go::checkCrossSliceSharedFiles` already enforces for PLANNED files and producing a guaranteed add/add fan-in conflict (run bc47d2c4).

- **No override, categorical.** Unlike the override-able `checkPlanScopeCap` (`--override-scope-cap`) / `checkPlanBudget` (`--override-budget`) upper-bound heuristics, fanning an added path into every slice is never correct, so there is NO override flag and NO per-slice add channel. The 422 (`plan_add_scope_files_fans_into_slices`) `details` name every inheriting slice — `stage_id`, `add_scope_files`, `slice_count`, and `slices` (an ordered `{index, title}` list of every sub-plan) — and the refusal appends a `plan_add_scope_files_fans_into_slices` audit entry (system actor). Remediation: re-plan the decomposition so each added file is declared in exactly one slice's `scope.files`.
- **Fails CLOSED on an indeterminate plan (binding, diverges from the sibling gates).** When `add_scope_files` is non-empty the gate PASSES only when the plan positively loads AND is confirmed non-decomposed (`Decomposition == nil`). A load error or a nil/indeterminate plan is REJECTED with the same 422 (`details.reason = plan_indeterminate`), not let through — an `add_scope_files` approve must never be recorded without positive confirmation the plan is flat. This deliberately diverges from `checkPlanBudget`'s fail-OPEN-on-load-failure posture: that gate is an override-able heuristic; this one is categorical, so a transient blip must not admit the offending approval. `loadApprovedPlanForRun` returns a non-nil `*plan.Plan` for every real flat plan (a nil/error return means no plan found / a read failed, never a valid flat state), so failing closed on nil does not over-block a legitimate flat-plan approve.
- **No config-absence carve-out — the fail-closed guarantee is universal (#2103 fixup).** There is NO `ArtifactRepo`/`RunRepo`-nil fail-OPEN exception. When the plan-artifact subsystem is unconfigured `loadApprovedPlanForRun` returns `(nil, nil)`, which the gate treats as indeterminate and REJECTS (`plan_indeterminate`), since an unconfigured subsystem does not POSITIVELY confirm the plan is flat. This diverges from the sibling gates' `ArtifactRepo==nil` fail-open on purpose: they are override-able heuristics, this one is categorical. Production always wires both repos, so the config-absent branch is unreachable there; the tightening only affects a test/misconfiguration path and closes the fail-open bypass (binding condition 2 / the authz-bypass review).
- **Pre-Submit (ADR-036).** Like its siblings the gate runs BEFORE `Submit`, so a refused approve records no approval row and a corrected retry after re-planning flows normally. Only `add_scope_files` is gated; a subtractive `remove_scope_files` fan-out is harmless (removing a path absent from a slice's scope no-ops).

### The per-slice add channel (#2515)

`add_scope_files_to_slice` is the decomposed-plan counterpart of the flat add the gate above refuses: `{"<sub-plan title or 0-based index>": ["path", …]}` on `POST /v0/stages/{id}/approvals`, surfaced on `fishhawk_approve_plan`. It restores ONE dropped file into ONE slice without a full revise pass. `approvals.go::checkSliceAddScopeFiles` runs immediately AFTER `checkDecomposedAddScopeFiles` and BEFORE `checkPlanScopeCap` (categorical refusals precede the override-able cap error), and the canonical map it returns is threaded back onto the request so every downstream consumer sees one form.

- **Key resolution — title-first, then index.** An exact (trimmed) `decomposition.sub_plans[].title` match wins; otherwise the key must parse as a 0-based decimal index in range. Explicit intent beats positional coincidence, so a plan whose sub-plan title is literally `"0"` resolves a `"0"` key by TITLE. A title shared by two sub-plans is AMBIGUOUS and refused (the schema declares titles unique — this is the defensive backstop).
- **ONE canonical form.** Keys become `strconv.Itoa(index)`; each path list is trimmed, deduped, then **SORTED**. Sorting (not input order) is what makes a title-keyed request and the equivalent index-keyed request record BYTE-IDENTICAL `add_scope_files_to_slice` payloads, which is what prompt-hash replay stability rests on. The index — not the title — is the durable join key: it matches the `runs.slice_index` column the fan-out children carry.
- **Ownership is CONTAINMENT, not string equality (`scopePathsOverlap`).** A path on this channel may be a DIRECTORY (trailing slash), so slice A owning `pkg/foo/` while slice B is handed `pkg/foo/inner.go` passes every equality check yet stages one file in two slices. Overlap therefore means identical OR ancestor/descendant, compared on path SEGMENT boundaries after trailing-slash normalisation — so `pkg/foo` does not spuriously conflict with the sibling `pkg/foobar`. The same comparison is applied in BOTH directions: across keys within one request, and against the plan's own per-slice `scope.files`.
- **Enumerated refusals, all pre-Submit (no approval row, no audit entry).** `422 plan_slice_add_scope_files_requires_decomposed_plan` — the ONE new error code — for a positively FLAT plan (`details.reason` `plan_not_decomposed`, pointing at plain `add_scope_files`) and, FAIL-CLOSED, for a plan whose decomposition status cannot be positively confirmed (`plan_indeterminate`: a load error, a nil plan, or an unwired artifact subsystem — the same universal no-carve-out posture as `checkDecomposedAddScopeFiles`, and the same deliberate divergence from the fail-open sibling gates). Everything else reuses `400 validation_failed` with `details.field = add_scope_files_to_slice`: an unresolvable key, an ambiguous key, two keys resolving to one slice, a path under two keys, a path overlapping another slice's declared scope, a non-repo-relative path, and an empty path list. Error details list slices in DECLARED index order (never Go map order), so a message is byte-stable across runs.
- **It ADDS; it does NOT MOVE — and says so.** The motivating incident (#2515) was a file already in the plan's total scope that needed to sit in a DIFFERENT slice (`README.md` owned by slice 1, wanted alongside `tools.go` in slice 2). That case is still REFUSED, deliberately: a move has different semantics (the owning slice's plan would reference a file it no longer owns) and is tracked separately. The refusal is made actionable rather than left to be discovered at a gate — the message and `details` name the owning slice (`owning_slice`, `owning_title`, `owning_path`), the semantic (`channel_semantic: add_not_move`), the overlap kind, and the remedy (re-plan the decomposition so the path is declared in the slice that needs it).
- **Recordable from the PLAN STAGE alone.** `checkSliceAddScopeFiles` runs only inside the `approve && stage.Type == plan` branch, so the canonical map it returns is carried in a LOCAL declared outside that branch and assigned only inside it — never written back onto the decoded request (which is what `remove_scope_files` does). The request outlives the branch and feeds `approveActionParams` unconditionally, so a write-back would let a direct HTTP approve of a NON-plan stage (implement, review, deploy) on the same run record a RAW, un-canonicalised, un-validated map — including the non-repo-relative paths `isRepoRelativePath` refuses at the gate — on an `approval_submitted` row. `loadApprovalSliceAddScopeFiles` scans by run + category with NO stage-type filter, so numeric-keyed entries from such a row would fold into a decomposed child's implement scope, bypassing every refusal above. The field is IGNORED (not refused) off the plan stage: that approve still returns 200, it just records nothing on this channel.
- **Cap arithmetic.** The flattened union of the canonical per-slice paths rides into `checkPlanScopeCap` alongside `add_scope_files`, and `effectiveScopePathSet` folds a PRIOR approval's recorded per-slice paths, so the number the gate reports stays equal to the scope the prompt builder assembles. A run with no per-slice map produces a byte-identical set.
- **Fold — one selection, one slice.** `prompt.go::resolveApprovalSliceAddScopeFiles` returns paths only when `DecomposedFrom != nil && SliceIndex != nil`, reading the run's own rows then the parent's, and selects solely `m[strconv.Itoa(*SliceIndex)]`. That single selection IS the single-owner-file guarantee: folding the whole map would reproduce the exact add/add fan-in the #2103 refusal names. It is unioned into `resolveApprovalAddScopeFiles` — the documented SOLE fold source — rather than at a new call site, so the enforced implement scope, the agent-facing shown set, the reviewer drift baseline, and trace.go's provenance sets all inherit the channel with no site to miss. A nil `SliceIndex` (a plan-stage-less recovery child of a decomposed parent, #2027 case 1) folds NOTHING: folding the union would widen its scope beyond what any single slice was granted. An out-of-range persisted index likewise folds nothing rather than falling back to another slice. The decomposed PARENT's fan-out resolver (`decomposedParentScopeChannels`, via `parentSliceGrantRecords`) is the one reader of the WHOLE map, read by both the parent fix-up serve (#4338) and the parent implement-review baseline (#4425); that is safe because the parent is the integrating branch every slice's grant already landed on, not a slice.

### The per-slice move channel (#2596)

`move_scope_files_to_slice` is the slice-boundary MOVE the add channel above deliberately refuses (`path_owned_by_another_slice`): the narrower cut that relocates an ALREADY-declared file from one slice to another. Same wire shape as the add — `{"<destination sub-plan title or 0-based index>": ["path", …]}` on `POST /v0/stages/{id}/approvals`, surfaced on `fishhawk_approve_plan` — but the key names the DESTINATION and the SOURCE is DERIVED from ownership. `approvals.go::checkSliceMoveScopeFiles` runs immediately AFTER `checkSliceAddScopeFiles` (so the add's canonical map is available for the cross-channel disjointness check) and BEFORE `checkPlanScopeCap`.

- **Single-owner-file preserved BY CONSTRUCTION, not by validation.** A file has exactly one owner before the move and exactly one after, so a move can never create the add/add fan-in the add channel's `path_owned_by_another_slice` refusal exists to prevent — which is the whole reason a move is safer than the add it complements. The source slice is never declared; it is located by scanning every sub-plan's `scope.files` for the exact owned path.
- **Shared key resolution + path hygiene.** `resolveSliceKeys` and `canonicalizeSlicePaths` are factored out of the add validator and reused here (parameterised on the field name), so title-first/index-fallback resolution, the ambiguous/unresolvable/duplicate-key refusals, and the trimmed/deduped/**SORTED** canonical form are byte-for-byte identical to the add channel. A title-keyed request and the equivalent index-keyed one record BYTE-IDENTICAL `move_scope_files_to_slice` payloads (index-keyed), the prompt-hash replay-stability property.
- **EXACT ownership identity, not containment.** Unlike the add's `scopePathsOverlap`, a move locates its owner by `normalizeOwnershipPath` EQUALITY. A path that only overlaps a directory-valued declared entry by containment is refused `move_requires_exact_owned_path` — neither side of a move can be expressed by splitting a directory entry, so that case must be re-planned. Ownership is located by NORMALIZED equality but then required to be BYTE-EXACT: a normalized-equal request that differs from the declared spelling only by a trailing slash (a directory alias — `pkg/dir` for a declared `pkg/dir/`, or vice versa) is ALSO refused `move_requires_exact_owned_path` (`details.declared_path` names the exact spelling to retry with). The accepted spelling is folded VERBATIM into the destination slice, so admitting the alias would drop the trailing slash the scope format uses to mark a directory and narrow the destination to a FILE path while the source directory is removed — so a directory move must name the owned path byte-for-byte (#2596 fixup).
- **Enumerated refusals, all pre-Submit (no approval row, no audit entry).** TWO new codes: `422 plan_slice_move_scope_files_requires_decomposed_plan` (a positively FLAT plan → `plan_not_decomposed`; a plan whose decomposition cannot be positively confirmed → `plan_indeterminate`, FAIL-CLOSED with the same universal no-carve-out posture as the add) and `409 plan_slice_move_after_dispatch` (a SOURCE or DESTINATION fan-out child past run state `pending` → `slice_already_started`; a `ListRuns` failure → `dispatch_state_indeterminate`, fail-closed — re-scoping a slice whose work has begun is the harm). Everything else reuses `400 validation_failed` with `details.field = move_scope_files_to_slice`: the shared key/path shape refusals, plus `path_under_two_slices`, `path_in_both_scope_channels` (the field composes with `add_scope_files_to_slice` in ONE approve but over DISJOINT paths), `path_not_in_declared_scope` (nothing to move — the message NAMES `add_scope_files_to_slice` as the channel that adds a net-new path), `move_requires_exact_owned_path`, `path_already_owned_by_destination` (a no-op move), and `move_would_empty_source_slice` (the last declared path of a source slice — an empty per-slice scope 409s at dispatch).
- **No cap headroom.** A move changes neither the effective-scope union nor its count, so it deliberately does NOT ride in `unionScopeAdds` and consumes zero `max_files_changed` headroom — an at-cap plan can still take a move.
- **Two-sided fold.** The DESTINATION side (`gained`) reaches BOTH surfaces the acceptance criterion names. The enforced implement scope, shown set, reviewer drift baseline, and trace provenance inherit it via `resolveApprovalAddScopeFiles` — the documented SOLE fold source — with no new call site. The agent-facing `ScopeConstraint.ScopeFiles`, which `resolveApprovalAddScopeFiles` does NOT reach (it folds only the enforced `scopeFiles`, and `trigger.ScopeConstraint` is set from `requireDecomposedScope` before that fold), gains `gained` directly in `resolveDecomposedScopeConstraint` — so the moved-in file is not enforced-in-scope yet absent from the narrowing the agent reads (#2596 fixup). The SOURCE side (`lost`) is the genuinely new machinery: `requireDecomposedScope` resolves BOTH `gained` and `lost` once and threads them into the two halves it returns — `resolveDecomposedScopeFiles` subtracts `lost` (before the coupled-`_test.go`-sibling fold, so a moved-away `foo.go` cannot drag `foo_test.go` back) and `resolveDecomposedScopeConstraint` subtracts `lost` AND adds `gained`. `resolveApprovalSliceMoves` returns `lost` as every path under every non-own key without re-reading the plan — subtracting a path a slice never owned is a harmless no-op.
- **Provenance conflation (accepted, documented).** Because the destination side routes through `resolveApprovalAddScopeFiles`, `trace.go` and the gate view label a MOVED-IN path IDENTICALLY to a genuinely ADDED one. This is deliberate (one fold source, no site to miss); the audit's `move_scope_files_resolved` `[{path, from_slice, to_slice}]` list is the ONLY place the true move provenance survives. An operator reading trace output alone sees a move as an add — read `move_scope_files_resolved` to disambiguate.
- **Reviewer baseline is whole-plan (no source-side review change needed).** The implement-review drift baseline for a fan-out child is the WHOLE-plan `scope.files` (the union of all slices): `trace.go::amendedScopeFilesForReview` keys on `approvedPlan.Scope.Files` and `writePlanForReview` renders it. A moved path is in that union both before and after the move, so the move is a NO-OP on the review/trace surface — the source-side subtraction lives only in the per-slice enforced scope (`requireDecomposedScope`), which the review side does not consult.

### Plan-stage-only recording, all four channels (#2598)

The "recordable from the PLAN STAGE alone" property above is now held UNIFORMLY by the four approve-time scope channels — `add_scope_files` (#824), `remove_scope_files` (#1726), `add_scope_files_to_slice` (#2515), and `move_scope_files_to_slice` (#2596) — plus a loader-side second wall.

- **Recording side (the control), absolute by construction.** All three values are carried in LOCALS (`addScopeFiles`, `removeScopeFiles`, `sliceAddScopeFiles`) declared outside the `decision == approve && stage.Type == plan` block and assigned ONLY inside it. Nothing is written back onto the decoded request: `req` outlives the block and feeds `approveActionParams` UNCONDITIONALLY, so the previous threading (`req.AddScopeFiles` verbatim; `req.RemoveScopeFiles = trimmedRemove` written back) let a direct HTTP approve of a NON-plan stage on the same run record raw, un-trimmed, un-validated lists on its `approval_submitted` row — no gate validates either flat channel outside this block, and `isRepoRelativePath` is never applied to the flat add at any stage. The removal channel is the sharper exposure: `validateRemoveScopeFiles`' would-empty-a-non-empty-scope refusal is plan-block-only, so a non-plan approve naming every scope path emptied the effective scope and re-enabled the runner's `git add -A` fallback, disabling scope enforcement. `req.AddScopeFiles` / `req.RemoveScopeFiles` survive only as the INPUT to the in-block gates. All three fields stay IGNORED (not refused) off the plan stage — that approve still returns 200, it just records nothing on these channels.
- **Loader side (defence in depth), best-effort but decisive for every server-written row.** `prompt.go::approvalEntryStageIsPlan` decodes the payload's `stage_id`, resolves it via `RunRepo.GetStage`, and is called from `loadApprovalAddScopeFiles` / `loadApprovalRemoveScopeFiles` / `loadApprovalSliceAddScopeFiles` AFTER each loop's `decision == approve && len(field) > 0` candidate test — so a lookup is paid only for a candidate row, and a dropped row lets the scan continue to an older legitimate plan-stage row. It exists to neutralise rows a PRE-fix non-plan approve may already have written. Filtering inside the LOADERS (not at the `resolve*` layer) is load-bearing: `scope_headroom.go::effectiveScopePathSet` calls two of them directly, so the number the cap gate reports stays equal to the scope the prompt builder assembles.
- **The filter FAILS OPEN, deliberately.** It drops an entry only on POSITIVE confirmation of a non-plan recording stage (GetStage succeeded, the resolved type is non-empty and != `plan`). Five enumerated branches fold as before with a WARN: nil `RunRepo`, no `stage_id` key, an unparseable `stage_id`, a `GetStage` error, and a resolved stage with an EMPTY type. Fail-closed would drop legitimate folds for every hand-built or legacy audit row that omits `stage_id`. In practice those branches are reachable only by synthetic rows: `writeApprovalAudit` (and its slash-command sibling `writeSlashApprovalAudit`) stamp `stage_id` unconditionally as the first key of every `approval_submitted` payload, so for a real approve the filter is decisive.

## Human-executor review-gate admission (`review_gate_admission.go`, E54.53 / [#3041](https://github.com/kuhlman-labs/fishhawk/issues/3041))

ADR-018 (#311, #313) moved review-stage approval onto GitHub, and `handleSubmitApproval` implemented that by refusing EVERY `type: review` stage with `409 review_stage_managed_by_github`. That is correct for `feature_change` and `routine_change`, whose review stages ARE the PR merge gate. It is wrong for `backlog_grooming`'s `confirm` stage — `executor: human`, `approvals: {count: 1, not: [agent]}`, no `pull_request` input — which had no approvable surface ANYWHERE, so a grooming run could be started but never finished and parked in `running` indefinitely (run `1499bdb0`).

### The three legs

`resolveReviewGateAdmission` is the ONE predicate every caller reaches. All three legs must hold; the order is fixed so the most specific reason is reported.

| Leg | Condition | Why it is the right key |
|---|---|---|
| 1 | the PERSISTED stage row's `executor_kind` is `human` | written at stage-create by `webhook.CreateStagesFromSpec` / `mapExecutor` directly from THAT spec stage's own executor, so this leg needs no resolver and cannot be confused by a sibling stage |
| 2 | the workflow's SOLE review spec stage declares `executor.human` | a row/spec disagreement is an anomaly, not a tie to break — it fails closed |
| 3 | that spec stage declares NO `pull_request` input | the discriminator that keeps the ADR-018 merge gate closed. Keyed on the SPEC's declaration, not on a stamped `pull_request_url`: the declaration is a stable property of the workflow, while the URL is mutable run state, so an implement stage that failed before opening a PR would leave a PR-managed gate spuriously admissible |

Leg 3 recognizes BOTH spellings that ship in this repository — `feature_change`'s `artifact: pull_request, from_stage: implement` and `routine_change`'s `source: pull_request, required: true`. Checking only one would admit the other workflow's merge gate; `TestSpecStageDeclaresPullRequestInput` and the two-case `TestSubmitApproval_PullRequestManagedReviewGate_StillRefused` pin each spelling independently (deleting either arm reddens exactly its own case).

### Resolution is all-or-nothing, not ordinal

The deploy side resolves its spec stage by DEPLOY ORDINAL (`deployStageForRunStage`, E23.19 / #2642), because a workflow may legally declare several deploy stages. The schema permits several review stages too — but the sibling review-side readers `fetchApprovalsForStage` and `fetchGateForStage` still resolve FIRST-MATCH-BY-TYPE. An ordinal resolver here would let admission read the k-th review stage while quorum read the first, splitting ONE authorization decision across TWO spec stages.

So `soleReviewSpecStage` refuses a workflow declaring more than one review stage outright (`multiple_review_spec_stages`). No workflow in this repository declares two, so nothing is lost today, and admission and quorum cannot disagree by construction. Genuine multi-review support must converge all three call sites at once — tracked as a follow-up, deliberately NOT done here.

There is no persisted spec-stage identifier to prefer instead: the `stages` table carries `stage_type` / `executor_kind` / `sequence` but no spec stage id or name, and `run.Stage` has no such field.

### Every non-OK reason is today's 409

The fail-closed direction is PRESERVING current behavior — an unresolvable spec can never open a merge gate. `rejectReviewStageApproval` keeps its code, message and `pull_request_url` detail byte-identical and adds `details.admission_reason`, a stable snake_case string, so a refused caller learns WHICH leg failed.

| `admission_reason` | Meaning | Test |
|---|---|---|
| `not_human_executor_row` | leg 1: persisted `executor_kind` is not `human` (or a nil stage) | `TestSubmitApproval_ReviewGateAdmission_FailClosedReasons/agent-executor_row` |
| `run_unavailable` | the run row could not be read (or `RunRepo` is unwired) | `..._FailClosedReasons/unreadable_run_row`, `TestResolveReviewGateAdmission_NilRunRepo_FailsClosed` |
| `no_workflow_spec` | the run carries no cached spec | `..._FailClosedReasons/empty_cached_spec` |
| `workflow_spec_unparseable` | the cached spec does not parse | `..._FailClosedReasons/unparseable_cached_spec` |
| `workflow_missing` | the run's workflow id is absent from the parsed spec | `..._FailClosedReasons/workflow_absent_from_spec` |
| `no_review_spec_stage` | the workflow declares no review stage | `..._FailClosedReasons/no_review_spec_stage` |
| `multiple_review_spec_stages` | more than one review stage — refused outright, see above | `..._FailClosedReasons/two_review_spec_stages` |
| `spec_stage_not_human_executor` | leg 2: human ROW, agent SPEC stage | `..._FailClosedReasons/spec_stage_is_agent-executor` |
| `pull_request_managed` | leg 3: ADR-018 owns this gate | `TestSubmitApproval_PullRequestManagedReviewGate_StillRefused` (both spellings) |

### The surface enforces `not: [agent]`, and it does so BEFORE the scope check

The gate declares `not: [agent]` today, but that was honored only because no surface existed — which is not the same as enforced. `refuseAgentOnHumanReviewGate` adds the two rungs, mirroring `grooming_dispositions.go`'s G2/G3:

- **`403 self_decision`** — a run-bound `mcp:run:` agent token, even for its OWN run.
- **`403 operator_agent_forbidden`** — a DELEGATED operator-agent token, keyed on `operatorrole.IsTokenSubject`, the same predicate `actor.go` uses to classify a delegated writer as `actor_kind=agent`. This is a reuse of the existing notion of agent identity, not a new one.

**The placement ahead of `requireWriteScope` is load-bearing, not stylistic.** A real `fhm_` run token does NOT carry `write:approvals`, so behind the scope check the message an operator actually sees in production is `insufficient_scope`, which names nothing about the executor constraint — only an artificially minted token would ever reach the named refusal. `TestSubmitApproval_HumanReviewGate_RunBoundToken_SelfDecision` tables BOTH shapes; moving the call after `requireWriteScope` reddens the scope-less (realistic) case with `code = "insufficient_scope", want self_decision`, verified empirically.

The guard returns immediately for every non-agent identity, so the common operator path pays no extra repository read, and an unresolvable stage or a non-admitted gate falls through to the ordinary 400 / 404 / 409 ladder rather than shadowing it. Both fall-throughs are pinned for BOTH agent identities: `TestSubmitApproval_AgentToken_PullRequestManagedGate_Still409` drives the run-bound AND the delegated operator-agent token against `feature_change`'s PR-managed review stage and requires the 409 `review_stage_managed_by_github` with `admission_reason: pull_request_managed` (a guard answering its 403 before consulting admission reddens both subtests with `status = 403, want 409`), and `TestSubmitApproval_AgentToken_MalformedStageID_FallsThroughToLadder` drives both identities at a non-UUID `{stage_id}` — where `lookupStageForPath` returns nil — and requires the handler's own `400 validation_failed`. Tabling both identities is what stops a future reordering answering 403 for one and 409 for the other on the same gate.

**Consequence, stated plainly: after this change an agent still cannot approve the grooming confirm gate. A human must.** That is the gate working as declared, not a gap — which is also why no MCP verb was added: an MCP tool is invoked under exactly the delegated operator-agent identity the second rung refuses, so the verb would be refused by its own handler on every real call. The human path is documented in `docs/GROOMING_RUNBOOK.md` §5: since E54.55 / #3051 it leads with the CLI verb `fishhawk approve-review-gate <run-id> --attest "..."` (which resolves the parked review stage from the run id and, being a CLI verb run under a human-held credential, is refused by neither rung), with the exact `curl` kept as the documented fallback. Neither is an MCP tool, so the paragraph above is unchanged by it.

### What the plan-gate approval stamps on a run with no implement stage (E54.52 / #3014)

`recordDrivePlanApproved` used to call `drive.EvaluatePlanApproved` unconditionally, so EVERY plan-gate approval on a drive run synthesized an implement-stage transition — including on `backlog_grooming`, which declares no implement stage. Two observed grooming runs (1499bdb0, 7bd6c6d3) were left pointing at a stage their workflow never declares. It now resolves the plan gate's DECLARED successor first, via the pure `planApprovedSuccessor(stages []*run.Stage)`:

| Resolved successor | Condition | What is stamped |
|---|---|---|
| `planSuccessorImplement` | an implement-typed row exists ANYWHERE in the list, in any state | `plan_approved_dispatch`, byte-identical to pre-#3014: `implement:dispatched` on `github_actions`, parked `implement:awaiting_host_dispatch` + a `run_implement_stage` next action on `local` (ADR-024) |
| `planSuccessorHumanReviewGate` | no implement row, and a review row whose `executor_kind` is `human` | `plan_approved_human_gate`: `plan:approved` -> `review:awaiting_approval`, parked, **no next action** — the gate is approved by a human-held credential through `fishhawk approve-review-gate`, and the MCP classifier owns the operator-facing entry (`human_review_gate_parked`) |
| `planSuccessorUnknown` | neither | NOTHING is recorded, plus a WARN. A rule that cannot name a real edge stays inert rather than synthesizing a transition to a stage that does not exist |

The implement row winning UNCONDITIONALLY is what makes the founder's regression guard structural rather than incidental — `TestRecordDrivePlanApproved_FeatureChangeShape_Unchanged` drives both runner kinds against a fixture that carries the human review row TOO, so it proves precedence and not merely absence.

The successor is read from the run's stage ROWS, never by re-parsing the cached spec: `webhook.CreateStagesFromSpec` materializes one row per declared spec stage at run-create and `mapExecutor` writes `executor_kind` straight from that stage's own `executor`, so the row IS the cached spec's declaration — the same leg `reviewGateAdmitNotHumanRow` trusts.

**The fail-open is deliberately narrow.** `run.Repository.ListStagesForRun` returns `(nil, error)` on a read failure (the Postgres adapter wraps the query error and returns a nil slice; it never substitutes an empty list for one), and a non-nil EMPTY slice only for a run with genuinely zero rows. So ONLY `err != nil` or `len(stages) == 0` falls back to today's `plan_approved_dispatch` stamp — this surface is display-only attribution and must not go silent because a read broke. A successfully read, NON-EMPTY list is trusted and decides the rule; the fail-open is not widened past those two cases. `TestRecordDrivePlanApproved_StageListError_FailsOpen` pins the error branch and `TestRecordDrivePlanApproved_EmptyStageList_FailsOpen` the length branch — each with a fixture that WOULD resolve to the human gate if the list were visible, so a pass cannot be explained by the fixture. The empty leg is defensive (a created run never has zero rows), so its fake is given an explicit knob rather than the test contorting the fixture; without it that leg's DIRECTION would be untested.

### The read side: a next_action naming an undeclared stage (E54.52 / #3014)

The emission fix cannot repair an entry already written, so `nextActionStale` (`runs.go`) tightened its no-matching-row fall-through, and the two branches point OPPOSITE ways on purpose:

- **NON-EMPTY list, no stage of the named type → SUPPRESS.** The list is positive evidence that the workflow declares no such stage, so a `run_implement_stage` action names a dispatch that will never become legal. This is what makes the two already-stamped grooming runs read correctly.
- **NIL or EMPTY list → still fail OPEN.** That is not evidence about the workflow's shape; it is the caller's documented read-error degrade (`applyDriveSurfaces` and `stageNextAction` both pass nil when `ListStagesForRun` errors), so a degraded read never hides a dispatch the operator genuinely owes.

`TestGetRun_NextAction_HostDispatchNamesUndeclaredStage_Suppressed` and `TestGetRun_NextAction_EmptyStageList_FailsOpen` pin the pair; the former REPLACES the pre-#3014 `..._NoMatchingStage_Surfaces`, whose assertion was the opposite. The `{pending, awaiting_host_dispatch}` per-state branch (#1961) and every non-dispatch action string are untouched.

The whole seam is pinned end to end by the shared wire fixture `backend/internal/server/testdata/human_review_gate_wire.json`: `TestGetRun_HumanReviewGate_MatchesWireFixture` asserts the file IS the server's own canonicalized `GET /v0/runs/{id}` + `GET /v0/runs/{id}/stages` bodies, and `backend/internal/mcpserver`'s classifier tests read that same file and decode it through the real MCP wire types, so a json-tag drift on either side reddens the pair.

### The attestation is required on an approve

`requireReviewGateAttestation` refuses an `approve` carrying an empty or whitespace-only comment with `400 attestation_required`. The confirm gate exists because an audit row saying `applied` is not the same claim as the tracker CARRYING the change (walk #2844 / #2847) — the attestation IS what the gate records. The guard is DECISION-SCOPED: a `reject` is itself the judgment and needs no attestation, and plan-gate / deploy-gate approve comments are untouched. `TestRequireReviewGateAttestation_DecisionScoped` and the paired `TestSubmitApproval_HumanReviewGate_RejectNeedsNoAttestation` pin both directions — deleting the decision scoping reddens the reject case, neutralizing the emptiness check reddens the approve cases.

### Downstream is reused unchanged

An admitted approve flows through the EXISTING path with no new state machine: `checkApproverAuthorization` → the #986 duplicate pre-check → quorum/predicates (whose `eligibleApprover` agent floor remains the independent COUNTING control) → `ApprovalRepo.Submit` → `writeApprovalAudit` → `advanceStage` → `Orchestrator.Advance` → `completeRun`. `TestHumanReviewGate_BacklogGrooming_EndToEnd_PgBacked` is the cross-boundary proof: it reads the SHIPPED `docs/spec/examples/workflow-v2-backlog-grooming.yaml`, persists stage rows through `webhook.CreateStagesFromSpec` (so `executor_kind` comes from the production mapping, never hand-set), lets the REAL orchestrator park the human stage at `awaiting_approval`, approves over HTTP, and asserts BOTH the confirm stage succeeded AND the RUN row reached terminal `succeeded` — the state run `1499bdb0` cannot reach today.

## Trace-time policy re-evaluation (`trace.go::reEvaluatePolicy`)

The backend's source-of-truth constraint evaluation on trace upload (E3.13): it loads constraints from the run row's cached workflow spec (#283), extracts the bundle's diff, and calls `policy.EmitEvaluation`, which writes the chained `policy_evaluated` audit entry the SPA renders.

- **Verification-signal derivation (#1886 / ADR-059).** Before evaluating, `reEvaluatePolicy` sets `constraints.Verification` from `verificationSignalFromBundle`, which reads the SAME bundle's single pre-redacted `gate_evidence` event (#963) — the `verify_summary` when present, otherwise the last **non-superseded** `verify_run` (#1205, the only one reflecting the pushed tree). `Commands` carries `{command, exit_code, outcome}` per non-superseded run, no output tails. No new runner emission is involved; this only threads existing evidence into policy evaluation.
- **nil is a violation, not a pass.** The helper returns nil on `bundle.ErrNoGateEvidence`, on any extract error, and when the evidence carries neither a summary nor a verify run — and the `verification_reported` required outcome treats nil as a violation (fail-closed). Contrast `ci_green`, whose nil signal *defers* to branch protection.
- **Diff-coverage signal derivation (#1888 / ADR-059).** `reEvaluatePolicy` also sets `constraints.DiffCoverageSignal` from `diffCoverageSignalFromBundle`, which reads the `diff_coverage` record out of that SAME `gate_evidence` event, while `mergeConstraints` supplies `constraints.DiffCoverage` (the DECLARATION) from the cached spec. Both are nil-safe: a stage that did not declare the constraint never enters the evaluation branch. nil is likewise a violation, not a pass — the runner emits a measured-with-zero signal rather than nothing when there is nothing to measure, so absence unambiguously means the runner never ran.
- **`resolveDiffCoverageConfig` (prompt.go)** serves the declaration to the runner as `promptResponse.diff_coverage`, at BOTH construction sites. It mirrors `resolveVerifyConfig`'s parse + stage-lookup shape and fails open to nil on every degradation (nil spec, parse failure, missing workflow, missing stage). That is safe because it is SYMMETRIC: the same spec lookup drives the backend's own constraint load, so a spec the backend cannot read yields neither a measurement request nor a gate. When a stage declares the constraint twice the most RESTRICTIVE threshold wins, matching `mergeConstraints`, so the runner measures against the same threshold the backend enforces. The stage-type surfaces cannot diverge either: only the implement runner measures, so the spec validator rejects `diff_coverage` on every other stage type (#1888) — otherwise a declaration on, say, an acceptance stage would reach evaluation with no signal, and a nil signal is a violation, i.e. a guaranteed false RED.
- **Built-in `agent_instruction_paths` (E80.12 / #3942).** On its success path `loadStageConstraintsFromCache` calls `applyAgentInstructionPolicy` (`scope_precheck.go`, the ONE spec→policy mapping the plan-gate precheck shares), so every implement stage is evaluated against the built-in agent-instruction-path constraint unless its v2 spec opts out — even a stage declaring no constraints at all. The built-in is resolved from the cached spec, so it applies ONLY when that spec yields the workflow: every `emitPolicySkipped` path (`spec_unavailable`, `spec_unparseable`, `workflow_not_in_spec`, a NON-implement `stage_not_in_spec`, `no_diff_in_bundle`) evaluates nothing, the built-in included. Cross-layer pin: `TestShipTrace_PolicyReEval_AgentInstructionPaths`; post-CI round trip: `TestReevaluateCIPolicy_PreservesAgentInstructionPaths`.
- **Per evaluated stage, fail-closed (#4311).** `agentInstructionStage` resolves the rule from the EVALUATED run stage's own spec stage via `specStageForRunStage` (type ordinal), so a stage-1 `enforce: false` or broad `writable` never covers a second implement stage. Unlike that helper's other callers it FAILS CLOSED for an implement stage: a `ListStagesForRun` error, a stage absent from the rows, an ordinal past the spec, or a workflow with no implement stage yields the built-in enforced with no `writable` list, and an implement stage is evaluated instead of skipped as `stage_not_in_spec` (so `passed` can be false where a skip row once read `passed=true`). A non-implement stage short-circuits without the list read. The precheck (`resolveImplementConstraints`) folds the rule STRICTEST across implement stages via `strictestAgentInstructionPolicy`: enforced if any stage enforces, `writable` the intersection of the enforcing stages' lists. Every other post-hoc constraint (`mergeConstraints`, `flattenPathConstraints`, `resolveImplementRequiredOutcomes`) stays FIRST-OF-TYPE, a documented limitation (#4345). Pins: `TestShipTrace_PolicyReEval_AgentInstructionPaths_PerImplementStage`, `TestLoadStageConstraintsFromCache_AgentInstructionPaths_FailClosed`, `TestScopePrecheck_AgentInstructionPaths_StrictestAcrossImplementStages`, `TestStrictestAgentInstructionPolicy`.
- The `bundle` import stays on this side of the seam: `backend/internal/policy` never imports it. Outcome semantics and the audit round-trip invariant live in `backend/internal/policy/README.md`.

## Ship-time plan gates

### Plan-gate warnings advisory (`plan_warnings.go`, #1684)

`backend/internal/server/plan_warnings.go::runPlanWarnings` — called from `handleShipPlan` (`plan.go`) immediately after `runTestSweep` and before `runScopeRegression`/`runPlanReviews`.
Gives `backend/internal/plan/validate.go::Warnings(p *Plan) []string` (unit-tested but previously uncalled in production) its first production caller, so the decomposition safety net it computes actually reaches the operator.

- `plan.Warnings()` returns soft advisory strings: notably a multi-slice (`len(sub_plans) >= 2`) decomposition where EVERY sub-plan omits `depends_on` (the shape that wedged #1551's first attempt — with no declared edges every slice runs in wave 0 and a producer->consumer chain can fail typecheck against a not-yet-integrated symbol, #1679/#1680).
  Plus two pre-existing advisories: a sub-plan `predicted_runtime_minutes` sum less than the parent's (possible scope compression), and an expensive `test_strategy` gate (`-count>=50` or full-repo `-race`) paired with an under-budgeted `predicted_runtime_minutes`.
  Since E68.28 (#3057) it also returns `counterfactual_mechanism_missing` (const `plan.RuleCounterfactualMechanismMissing`, appended LAST so every pre-existing warning keeps its position): one warning per `test_strategy` sentence that names a control DELETION and a RED outcome while stating no MECHANISM in that sentence or the immediately following one, quoting a ≤160-rune rune-safe excerpt and naming the SETUP remedy (state the fixture state that makes the deletion observable). Capped at 5 plus one `(N more … suppressed)` tail so the payload stays bounded. LEXICAL and therefore evadable — the plan prompt's Counterfactual attainability rule clause (5) is the prevention; rule contract in `docs/spec/plan-standard-v1.md`.
- **Guards only `AuditRepo`** — unlike the sibling gates it needs no `RunRepo`/workflow spec/GitHub client, since `Warnings()` depends only on the parsed plan itself.
- **Write-only-when-non-empty** (the one divergence from the sibling gates' always-write "checked and clean" convention): a `plan_warnings` audit entry (payload `PlanWarningsPayload{warnings}`) is appended ONLY when `Warnings()` returns at least one string.
  A warning-free plan gets NO entry, which is what keeps `TestShipPlan_HappyPath`'s `len(au.appended)==1` assertion green (the sibling gates are no-ops in that test only because their external guards — `RunRepo`/spec/GitHub — are unmet, whereas this gate needs only `AuditRepo`, which IS wired there).
- Advisory + fail-open: a `plan.Parse` failure or an audit-append failure WARN-logs and returns nil/continues; it never transitions or fails the plan stage.
- The returned payload is NOT YET threaded into the plan-review prompt's gate-evidence section (deferred; the operator-facing surface for this slice is `fishhawk_get_plan`).
- MCP surface: `fishhawk_get_plan` adds `plan_warnings` (`[]string`) decoded from the **newest** `plan_warnings` entry (`loadPlanWarnings` in `tools.go`); absent/omitted when no entry exists (warning-free plan or an older run predating this pass).
- **Over-cap advisory (#2053)**: `runPlanWarnings` also appends a deterministic, count-derived over-cap advisory when the resolved implement-stage `max_files_changed` cap is `> 0` and `len(scope.files) > cap`, via `overCapWarning`. The decode is `json.Unmarshal` (NOT `plan.Parse`) so the advisory stays independent of `semanticCheck` — a `plan.Parse` failure (a decomposition/split structural error) would otherwise suppress the count-derived advisory for a plan the operator still needs to see flagged.
- **Near-cap advisory (#2492, E50.14)**: `nearCapWarning` (appended immediately after `overCapWarning`, before `phaseCapWarnings`/`irreducibleWarning`) makes the SHARED whole-plan budget visible at the plan gate. It fires when the plan is NOT over cap but leaves `capLimit - count <= nearCapMargin` files of headroom (`nearCapMargin = 3`, a package const pinned by `TestRunPlanWarnings_NearCap_ThresholdBoundary` — fires at headroom 0..3, silent at 4+; the headroom-0 row is seeded `count == capLimit`, an at-cap plan that is admissible but has zero headroom, so it discriminates near-cap from over-cap). The advisory names the count, cap and remaining headroom and states that once the headroom is spent the plan-approval scope-cap gate (`checkPlanScopeCap`) and the mid-stage scope-amendment headroom check (`amendmentHeadroom`/`effectiveScopeHeadroom`) refuse any further file — so a correct mid-stage fix needing an un-scoped file has no path in without a re-plan or a governed cap raise.
  - **Decomposition-aware**: when the plan carries a decomposition of `>= 2` sub-plans a second sentence names the slice count and states plainly that all N slices draw against this ONE whole-plan budget (the 'more prominently for a decomposed plan' half of the done-means). A single-slice (schema-forbidden: `sub_plans` `minItems=2`) or absent decomposition renders only the base sentence.
  - **Mutual exclusion is structural.** `nearCapWarning` and `overCapWarning` route through the SAME shared `overCapByCount` cap resolution, and `nearCapWarning` returns `""` the moment `over` is true (the over-cap advisory owns that case), so the near-cap and over-cap advisories can never both fire for one plan and the append is a no-op for every over-cap plan (the existing over-cap/irreducible/phase-cap warnings are byte-identical).
  - **Fail-open on four legs** (all via the shared `overCapByCount` `!ok`): nil `RunRepo`, `GetRun` error, no implement stage, and an implement stage declaring no `max_files_changed` — each skips the advisory and never blocks the settle. Pinned one-per-leg by `TestRunPlanWarnings_NearCap_FailOpenLegs`; the write-only-when-non-empty contract is pinned by `TestRunPlanWarnings_NearCap_AbsentWhenAmpleHeadroom`.
- **Unlandable advisory (#2415)**: `capUnlandableWarning` (appended immediately after `nearCapWarning`, before `phaseCapWarnings`, so the #2053 over-cap-first ordering is untouched) fires when the cap resolves AND the plan's minimum PHYSICAL changed-file count (`planMinChangedFiles`) exceeds it — the state in which `--override-scope-cap` is refused at approval. It states plainly that the plan CANNOT land in this run even with the override (the implement stage re-checks the real diff against a cap fixed for the run) and names the two levers: `remove_scope_files`, or a governed `max_files_changed` raise plus a fresh run. It reads the PHYSICAL count, so a rename-shaped plan whose declared count is over cap but whose physical count fits stays silent. Fail-open on the same shared `overCapByCount` `!ok` legs.
- **New-architectural-decision advisory (E78.4 / #3748)**: `newArchitecturalDecisionWarning` (a pure function, appended LAST after `irreducibleWarning`, so the #2053 over-cap-first ordering is untouched) surfaces a planner's optional `new_architectural_decision` declaration as exactly ONE advisory naming the `decision_summary`, the `rationale` and the `related_adrs` (`none cited` when empty), and says the captain decides whether an ADR is needed: approve (optionally filing one with `fishhawk_file_issue` type `adr`) or reject. It gates nothing and reads no `RunRepo`/cap, so it fires even when the cap is unresolvable; it rides the existing `plan_warnings` entry (no new audit category). The guard is `plan.NewArchitecturalDecision.Declared()`, kept defensive because this pass decodes with `json.Unmarshal` and never runs `semanticCheck`, so a whitespace-only rationale/summary/ADR id reaches it unrejected and must not warn. Pinned by `TestRunPlanWarnings_NewArchitecturalDecision_*` and, across the ship seam, `TestShipPlan_NewArchitecturalDecision_OnePlanWarning`.
  - **Revise carry-through.** `loadRevisionBasePlan` re-marshals the typed `plan.Plan`, so the field rides a whole-mode revision base verbatim (`RelatedADRs` carries no `omitempty`, so `[]` survives); an over-cap base renders it in the `prompt` package's step-complete digest. Pinned end to end by `TestRevisePlan_CrossLayer_NewArchitecturalDecisionSurvivesRevise`. `backend/internal/mcpserver`'s `revise.go` needs NO change: `fishhawk_revise_plan` only resolves the plan stage and POSTs the constraint to `/v0/stages/{id}/revise` (`SubmitRevise`); the revision base is assembled server-side here.

- **Partial-delivery advisory (E83.52 / #4085)**: `partialDeliveryWarning` (a pure function, appended after `newArchitecturalDecisionWarning`, so the #2053 over-cap-first ordering is untouched) fires only when `plan.(*Plan).IsPartialDelivery` reports the plan declares `delivery: partial`, as exactly ONE `PARTIAL DELIVERY` advisory naming the `remaining_scope` (`not stated` when blank) and asking the approver to confirm the issue should stay open or reject the plan. It gates nothing and rides the existing `plan_warnings` entry. The rest of the partial-delivery surface is in [Partial delivery](#partial-delivery-e8352--4085) below. Pinned by `TestRunPlanWarnings_PartialDelivery_*`.

### Mid-stage scope-amendment deadline observability (`scope_amendment.go`, #2540)

A scope amendment filed near the agent wall clock is **undecidable by construction**: the implement agent's ~15-minute wait-poll window can outlive the stage's remaining agent runtime, so the stage is SIGKILLed while the agent is still polling and a correctly-approved amendment is lost with the whole pass. `handleRequestScopeAmendment` derives the executing implement stage's **remaining agent wall clock** — the spec-resolved `agent_timeout_seconds` (`resolveAgentTimeout`, the same budget the runner enforces) minus the stage's elapsed time — via the pure `amendmentDeadlineRemaining`, and when it can, surfaces it OBSERVABILITY-ONLY on the created row's REQUEST response + `scope_amendment_requested` audit as `stage_deadline_seconds_remaining` + `amendment_poll_window_seconds` (`AmendmentPollWindowSeconds`, 900), so an operator can **see** whether a race against the deadline is already lost. The row is always **created** and `pending` — a late decision plus `fishhawk_retry_stage` still folds the paths, and the runner's `detectUndecidedScopeAmendments` still names them on the failure path.

- **There is DELIBERATELY no refusal (#2540 approval condition 1).** An earlier design emitted an `undecidable_before_deadline` flag that told the agent to skip its wait-poll; it was struck because the remainder is uncertain in the **pessimistic** direction (`started_at` precedes the real agent clock, and an operator retry can leave a cumulative `started_at`), so a computed "too short" can be a false positive — and refusing a WINNABLE amendment, killing a stage that would have succeeded, is strictly worse than the bug it guarded against (which merely loses an already-unwinnable amendment). The numbers are **displayed, never acted on**; no wire/audit/prompt signal tells the agent to abandon a wait.
- **`amendmentDeadlineRemaining` still gates whether the numbers are shown, failing open in four named modes** (a misfire now costs at most a hidden number, never a killed amendment): **(a)** `budgetSeconds <= 0` (unresolvable spec budget — `resolveAgentTimeout` is fail-open); **(b)** `startedAt == nil` (never-started stage); **(c)** `remaining <= 0` (elapsed exceeds the whole budget — a live agent posting proves the deadline has not literally passed, so this is a bad derivation, not a passed deadline); **(d)** `selfRetryCount > 0` (`stages.started_at` is `COALESCE`'d and never overwritten, so a re-spawned stage's elapsed is cumulative and its remainder understated; an operator `fishhawk_retry_stage` that does not bump `SelfRetryCount` leaves the same cumulative `started_at`, indistinguishable from a first run — precisely why no refusal is built on the remainder). Each mode is pinned by a row of `TestRequestScopeAmendment_DeadlineFailOpen` asserting both numbers absent (struct AND raw-body key) and the audit key set exactly `{amendment_id, paths, reason, remaining_budget}`; `TestRequestScopeAmendment_TightRemainderNeverRefuses` pins that even a tight remainder surfaces the numbers without any refusal signal.
- **`AmendmentPollWindowSeconds` (900) is one of four copies of the window** (this constant, the prompt's ~15-minute text, `mcpserver`'s `amendmentPollWindowMinutes`, and the `mcpserver`/`scopeamendment` READMEs). It is now an OBSERVABILITY figure on every surface. The server↔mcpserver copies are pinned equal by a cross-package test (`backend/internal/mcpserver/stage_wait_test.go`), so a desync reddens rather than silently showing an operator two different windows.

### Scope-cap override is conditional within a run (#2415)

- **The refusal.** `checkPlanScopeCap` (`approvals.go`) still refuses an over-**declared**-cap approve `422 plan_violates_scope_cap`, but `--override-scope-cap` is no longer an unconditional escape. When the override is present AND the effective scope's minimum physical changed-file count (`minPhysicalFileCount`, computed over `effectiveScopePathSetWithOps`'s ops map) exceeds the cap, the approve is REFUSED `422 plan_scope_cap_override_unavailable` (pre-insert — no approval row, stage untouched) with a `plan_scope_cap_override_refused` audit entry (system actor). The message names `constraints.max_files_changed`, both counts (`scoped_files` and `min_changed_files`) against the cap, and the two levers (`remove_scope_files`; or a governed cap raise plus a fresh run). When `min_changed_files` fits the cap the override still succeeds, and the `plan_scope_cap_override_acknowledged` payload now carries `min_changed_files` plus a `note` that it covers the declared-scope pre-check ONLY. The no-override `plan_violates_scope_cap` details also carry `min_changed_files`, and its message appends the "override will not help" sentence when `min_changed_files` already exceeds the cap.
- **Why categorical is safe within a run.** The cap is IMMUTABLE for an in-flight run: `checkPlanScopeCap` resolves it via `resolveImplementConstraints` (`scope_precheck.go`) and the post-implement gate via `loadStageConstraintsFromCache` (`trace.go`), and BOTH parse the same `runs.workflow_spec` snapshot (cached at run creation) with the same min-wins merge. Raising `.fishhawk/workflows.yaml` cannot change this run's cap, so an override that cannot fit today can never fit later in this run — refusing at the plan gate costs nothing, while granting it costs a full run before the implement stage re-check rejects the real diff category-B (the #2415 defect).
- **The estimate.** `minPhysicalFileCount` is deliberately GENEROUS (a lower bound on the physical count): it exempts generated/vendored paths exactly as `policy.CountedFileCount` does, then returns `others + max(creates, deletes)` because git rename detection collapses at most `min(creates, deletes)` declared delete+create pairs into single `R` rows. It is minimal only over DECLARED operations — an undeclared create git would pair with a declared delete as a rename makes the true physical count lower, so the refusal can only over-estimate the count (never admit an over-cap landing) but MAY refuse a rename-heavy plan whose real diff would fit; `remove_scope_files` at the gate is the in-loop escape. The schema has no `rename` operation, so a rename-shaped plan (declared over cap, physical at/under cap) keeps the override — it has no other way to declare itself.

### max_files_changed stays a whole-plan cap (#2492 decision)

**Decision.** `max_files_changed` remains a WHOLE-PLAN cap in v0, unchanged. The near-cap advisory above is the whole of #2492's implementation; the cap itself is NOT re-shaped into a per-slice sub-budget, and the issue's item 3 (a split-time reserved-headroom mechanism) is deliberately not implemented here.

**Rationale.** Cap ENFORCEMENT is already per-slice: the runner (`runner/internal/constraint`) counts each stage's OWN diff, so a decomposition child is evaluated against its own file diff and a slice cannot fail on a sibling's files. What is genuinely SHARED is (a) the plan-time budget and (b) the amendment headroom that `effectiveScopePathSet` computes over the PARENT plan's `scope.files` — which is exactly what #2492 reported blocking three correct mid-stage fixes. A derived per-slice sub-budget would need a plan-schema field, a per-slice headroom computation, and a cap-resolution path that distinguishes parent from child — a schema + spec change, not a local fix. This change instead makes the shared budget VISIBLE at the one gate (plan approval) where the operator can act on it, which sharpens the issue's own framing rather than expanding scope.

**Reopen conditions.** Revisit a per-slice sub-budget (or the reserved-headroom proposal) when EITHER (a) repeated near-cap DECOMPOSED plans show the shared whole-plan budget is the recurring blocker in practice (the advisory's decomposition clause is the signal to watch), OR (b) the #2052 split-proposal work reaches a point where reserving per-phase headroom at split time is the natural home for the mechanism. Cross-references: #2052 (split reachability/headroom), #2363, #2415 (governed cap raise is the current escape hatch), #2412 (irreducible declaration + per-phase cap refusal). This subsection is written to lift into an ADR body verbatim should the operator choose to promote it to a numbered ADR.

### Cross-run scope overlap (`scope_overlap.go`, #4118)

E83.60 / #4118. `computeScopeOverlap` answers "which OTHER in-flight runs share files with this one" — the merge-order signal for overlapping work (merging while an overlapping run is mid-implement can strand it in a category-B dead end). **Advisory on these surfaces** (endpoint, gate view, plan-gate row): nothing here refuses on it, every read is fail-open. The one REFUSING consumer is the fail-closed merge overlap guard, run by `POST /v0/runs/{run_id}/merge` and by the delegated `may_merge` arm (#4582); it reuses `overlapCandidates` and `resolveOverlapScope` (§ "Merge overlap guard").

- **Effective scope** (`resolveOverlapScope`, exact paths only, no glob/directory semantics): the newest standard_v1 plan via `loadApprovedPlanForRun` (parent-walk) — a decomposition child narrows to its own `sub_plan` slice when `matchDecomposedSubPlan` links one with declared files; any other run takes `scopeFilesFromApprovedPlanFull` (top-level `scope.files` UNION every sub_plan) — plus approval `add_scope_files`, plus APPROVED scope amendments only (`approvedAmendmentPaths`; pending/denied/lapsed never count), minus approval `remove_scope_files`. No plan, a plan read error, or an amendment-list error → unresolved.
- **Candidates** (`overlapCandidates`, shared with the merge overlap guard so both judge one set): `ListRuns` per state in `scopeOverlapLiveStates` (`pending`, `running` — terminal runs are never listed), same `Repo`, `AccountID` filter, `Limit: scopeOverlapListLimit` (200). `len(page) >= scopeOverlapListLimit` sets `truncated`.
- **Exclusions**: `isOverlapLineage` — self, parent/child (`DecomposedFrom`), sibling slices (equal `DecomposedFrom`; the within-plan `checkCrossSliceSharedFiles` rule governs them and they integrate into ONE consolidated PR), and a `ParentRunID` recovery/retry pair. **One `ParentRunID` hop** (`overlapRecoveredSlice`): a run with no `DecomposedFrom` whose parent IS a slice (a slice's recovery child, which resolves the parent's full union) is judged against that slice too, so it never reports its decomposed parent or sibling slices, and they never report it. A failed parent read infers no lineage. `overlapSameTenant`: an untenanted candidate stays visible (the `ListRunsFilter.AccountID` contract); a tenanted run never sees another tenant's run; an untenanted subject sees only untenanted runs.
- **Payload** `scopeOverlapPayload{run_id, self_scope_resolved, overlaps[], scanned_runs, unscoped_runs, truncated}`; each `scopeOverlapRun{run_id, issue_ref (TriggerRef — a child carries its parent's), decomposed_from, stage, stage_state (currentOverlapStage: lowest-sequence non-terminal, else last), paths (sorted)}`; `overlaps` is `[]` never null, sorted by `issue_ref` then `run_id`. A decomposed parent and its in-flight child are BOTH reported to an unrelated run (deliberate; `decomposed_from` lets a consumer group them). Self unresolved → `self_scope_resolved:false`, empty, NO listing. A candidate whose scope does not resolve counts in `unscoped_runs`. A stage-list failure leaves `stage`/`stage_state` empty and keeps the entry. The ONE returned error is a `ListRuns` failure.
- **Surfaces**:
  - `GET /v0/runs/{run_id}/scope-overlap` (`handleGetRunScopeOverlap`, `requireRunAccount(readAccess)`): 503 `run_repo_unconfigured` → 400 `validation_failed` → **403 `run_token_forbidden` for ANY run-bound `mcp:run:` subject, own run included** (the payload names other runs' ids, issue refs and paths — approval condition 1) → `read:audit` via `requireWriteScope` (401 anonymous, 403 `insufficient_scope`, cookie operators pass) → 404 `run_not_found` / 500 → 500 `internal_error` on a listing failure → 200.
  - Gate view `scope_overlap` (`gateViewScopeOverlapFor`), computed in `handleGetRunGateView` only — never `buildGateView`, so the attention scan's per-run gate-view fan-out does not pay the listing. OMITTED for a run-bound caller and on a listing failure (WARN; the view still answers 200).
  - Plan gate: `recordPlanScopeOverlap`, called from `handleShipPlan` right after `runPlanWarnings` (so the row precedes `plan_reviewed`), appends ONE `plan_scope_overlap` row (system actor, `StageID` set, payload = the overlap payload + `stage_id`) ONLY when the self scope resolved and an overlap exists (the `plan_warnings` write-only-when-non-empty convention — a reader cannot tell "checked and clean" from the audit log; the live endpoint answers that). Fail-open on nil `AuditRepo`/`RunRepo`, `GetRun` error, compute error and append error (WARN). Not an issue-thread activity category (`docs/issue-comment-surfaces.md`).
  - **Audit-log withhold for run-bound subjects** (`reads.go`): the `plan_scope_overlap` row (and, since #4119, the `merge_overlap_overridden` row) names other runs, so `withholdScopeOverlapFromRunBound` drops it for any `mcp:run:` subject (prefix match, malformed suffix included) on `handleListRunAudit`'s non-chain reads — the read that NAMES the category included, since this is access control by subject, not a render preference like `withholdDelegationShadow` — and on `handleListGlobalAudit`, both BEFORE `pageOffset` so `limit=N` keeps N visible rows. A run-bound `chain=true` read is refused **403 `run_token_forbidden`** before any repository read, because the chain read stays complete for hash verification and cannot be filtered; operators are unchanged. Reader inventory: `fishhawk_list_audit` / `fishhawk_await_audit` (`ListRunAudit` → per-run feed) and `fishhawk_get_run_status` `recent_audit` (`ListRecentRunAudit` → `GET /v0/audit`) are covered by those two handlers; `GET /v0/audit/export(.csv)` needs `read:audit-export`, which a run-bound token never holds (`mcptoken.go`); the diagnostics read and `diagnostics.Collect`/`CollectWithWedge` (the product-report bundle) never serialize entry payloads — a stage-tagged category NAME can surface as a bundle's `failing_surface`, never another run's id or path. **Accepted consequence**: `fishhawk_verify_run` pages the non-chain feed and recomputes `prev_hash` links, so under a run-bound subject it reports `chain_broken` on a run carrying a `plan_scope_overlap` row; `delegation_shadow_evaluated` already has the same latent effect. Operators verify on the complete feed.
- **Live vs snapshot**: the audit row is what the plan saw when it settled, so the FIRST of two overlapping runs to settle has no row naming the later one; the endpoint and gate view recompute on every read, which is what makes "each reports the other" hold.
- **Cost**: two `ListRuns` (≤200 each) plus ~5 reads per candidate; a per-repo cache is the follow-up if it proves slow.
- **Counterfactual anchors** (`scope_overlap_test.go`): the `approvedAmendmentPaths` append and status filter (`TestScopeOverlap_ApprovedAmendmentOnlyOverlap`, `TestScopeOverlap_PendingAndDeniedAmendmentsIgnored`); the `scopeOverlapLiveStates` literal (`TestScopeOverlap_TerminalRunsNeverReported` — the fake honours the State filter and there is deliberately no second Go-side terminal check); the `isOverlapLineage` call (`TestScopeOverlap_DecompositionLineageExcluded`) and the one-hop calls (`TestScopeOverlap_SliceRecoveryChildLineageExcluded`); `overlapSameTenant` (`TestScopeOverlap_OtherTenantNeverReported` — the fake ignores `AccountID`); the slice-narrowing branch (`TestScopeOverlap_DecomposedChildSliceAttributed`); the `plan.go` call (`TestShipPlan_RecordsScopeOverlap`); the run-bound refusals (`TestHandleGetRunScopeOverlap`, `TestGateView_CarriesScopeOverlap`); `stagePlanForOverlap`'s schema filter and unmarshal check (`TestRecordPlanScopeOverlap_UnreadableStagePlan` — the non-standard_v1 artifact carries a VALID overlapping plan); and, in `reads_test.go`, the audit withhold and its before-pagination placement (`TestListRunAudit_RunBoundWithholdsScopeOverlap`, `TestListGlobalAudit_RunBoundWithholdsScopeOverlap` — the overlap row is sequenced first) and the chain refusal (`TestListRunAudit_RunBoundChainRefused`).

### Campaign-wide pending amendments (`campaign_pending_amendments.go`, E86.85 / #4486)

A driver blocked in an await verb on ONE run of a campaign could not see a scope amendment filed by a DIFFERENT run of the same campaign, so the agent's ~15-minute poll window lapsed undecided (the #4082/#4077 miss). `campaignPendingAmendments` is the server half of the fix: one read that lists every pending amendment on every run of a campaign. The MCP await verbs consume it (`backend/internal/mcpserver/README.md`).

- **Run set** (`campaignPendingAmendments` + `campaignRunChildren`): every campaign item's linked run seeds the walk **whatever the item state** — cancelling a campaign does not cancel its linked runs, and an item can go succeeded through out-of-band delivery while its run is still live, so item state never excludes a run; the amendment's own run and stage lapse rules (#4127) decide liveness. Descendants are found breadth-first through BOTH lineage links — one `ListRuns` with `ParentRunID` and one with `DecomposedFrom` per node, unioned and de-duplicated — so a recovery child carrying only `parent_run_id` and a slice are both found. Depth is bounded by `maxCampaignAncestorDepth`; the total by `campaignPendingAmendmentsMaxRuns` (64). Reaching the cap with another run still to add stops enumeration and sets `truncated` (never silent). Every `ListRuns` carries an explicit `Limit: campaignPendingAmendmentsMaxRuns+1` (the Postgres adapter refuses `Limit <= 0`). Each enumerated run inherits its seed item's `issue_ref` and `item_run_id`.
- **Rows**: `buildScopeAmendmentItems` per run (so pending rows carry the #983 headroom), filtered to `status == pending`; each row's stage is read with `GetStage` and the entry carries `stage_state` and `decision_live = !StageState.IsSettled()` — true while an agent may still be waiting on the decision. A settled (e.g. failed) stage's still-pending row is LISTED with `decision_live:false`; the await verbs release only on a live entry, so such a row never wedges a wait. Sorted by `requested_at` ascending, then amendment id. **Any read error returns the error** — there is no partial result.
- **Campaign isolation is structural**: the run set derives only from the resolved campaign's own items, so another campaign's amendment cannot appear. There is no single deletable guard; `TestCampaignPendingAmendments_OtherCampaignExcluded` pins it as a regression test.
- **Route** `GET /v0/runs/{run_id}/campaign-pending-amendments` (`handleGetRunCampaignPendingAmendments`, `requireRunAccount(readAccess)`): 503 `run_repo_unconfigured` / `campaign_repo_unconfigured` / `scope_amendment_unconfigured` → 400 `validation_failed` → **403 `run_token_forbidden` for ANY run-bound `mcp:run:` subject, own run included** (the payload names other runs' paths and reasons) → `read:audit` via `requireWriteScope` (401 anonymous, 403 `insufficient_scope`, cookie operators pass) → 404 `run_not_found` / 500 → `resolveCampaignAncestry` (500 on error; unresolved or no items → 200 with `campaign_id: null`, `items: []`, `truncated: false`) → the first linked item's `CampaignID` → `ListCampaignItemsForCampaign` (500) → the helper (500) → 200 `{run_id, campaign_id, items, truncated}`.
- **Status block**: `handleGetCampaignStatus` sets `pending_amendments` from `campaignPendingAmendmentsForStatus` over the FINAL item slice (after reconcile-on-read and the dependency refresh). `campaignAmendmentsReadable` gives the status block the SAME outcome the route gives a caller: run-bound → no, anonymous → no, token → needs `read:audit`, cookie session → yes. **Absent key = not computed** (scope-amendment or run repository unwired, caller not permitted, or the read failed — WARN-logged, the status read never fails on it); **present with `items: []` = computed, none pending**. The two are different claims.
- **Cost**: the ancestry walk plus at most 64 runs × (two descendant lists + one amendment list) and one stage read per pending row; pending rows are rare.
- **Counterfactual anchors** (`campaign_pending_amendments_test.go`): the route's run-bound refusal (`TestCampaignPendingAmendments_RunBoundTokenForbidden` — the identity is bound to the anchor run and carries `read:audit`, so the scope check cannot mask it); the status block's run-bound omission (`TestCampaignStatus_PendingAmendmentsOmittedForRunBoundCaller`); the `decision_live` computation (`TestCampaignPendingAmendments_FailedStageListedNotLive`). The test-local run-repo wrapper honours `ParentRunID` (the shared `orchestratorRepo` fake ignores it) and refuses `Limit <= 0` like the Postgres adapter.

### Plan-gate over-cap split reject (`plan_warnings.go` / `plan.go`, #2055, E50.3)

`backend/internal/server/plan_warnings.go::overCapSplitRejection` — called from `handleShipPlan` (`plan.go`) right before `runPlanReviews`. This is the **SERVER-AUTHORITATIVE, count-derived HARD reject** for an over-cap monolith — the E50 keystone, distinct from the advisory above.

- **Count-derived, flag-independent.** `overCapByCount` factors the #2053 count determination (`resolveImplementConstraints` → `len(scope.files)` vs `MaxFilesChanged`) into a shared helper that `overCapWarning` (advisory) and `overCapSplitRejection` (reject) both call; it **never reads `over_cap`**. `overCapSplitRejection` returns a reject reason when the plan is over cap **by count** AND carries no `split_proposal`, and `""` otherwise — so an over-cap-by-count monolith without a split is rejected whether `over_cap` is omitted, `false`, or `true`; an over-cap plan carrying a valid `split_proposal` is accepted; an under-cap plan is unaffected.
- **Decodes with `json.Unmarshal`, not `plan.Parse`** (in `handleShipPlan`): decoding without `semanticCheck` keeps the gate flag- AND parse-independent, so it fires on the server-derived count alone for `over_cap` `{omitted, false, true}` alike and no in-artifact semantic error can preempt the authoritative count reject.
- On a non-empty reason: emit a terminal `plan_review_failed` audit entry (`emitReviewFailed`), fail the plan stage category-B (`run.FailStage` + `advanceAfterFailure`, the same terminal path the plan-invalid/decomposition reject uses), and set `gatingRejected` so `advancePlanStageTerminal`/`notifyPlanReadyIfReady` are suppressed — the rejected plan never advances, and the artifact is still stored so the operator can inspect it via `fishhawk_get_plan`.
- **Fail-open** on every leg exactly like the advisory (nil `RunRepo`, `GetRun` error, no spec/implement stage, unresolved/zero cap → `overCapByCount` returns `ok=false` → `""`), so an unresolved cap can never spuriously block a plan.
- There is deliberately **no** `over_cap ⇒ split_proposal` coupling in `semanticCheck` (`backend/internal/plan/validate.go`). Because that check had no view of the resolved cap it was count-blind, and rejected an **under-cap** plan that merely set the `over_cap` hint — turning the advisory into a server rejection across every `plan.Parse` caller (the plan reviewers plus the fail-open scope/surface/test gates) and breaking the under-cap-unaffected guarantee. It was removed (#2055 fixup); `overCapSplitRejection` is the sole authoritative over-cap reject, and `semanticCheck` only validates the **structure** of a `split_proposal` that is present (`checkSplitProposal`).

### Irreducible declaration + per-phase cap refusal (`plan_warnings.go` / `split_filing.go`, #2412, E32.44)

Two additions close the three defects #2412 reports: a planner facing a compile-atomic over-cap change can honestly DECLINE a split, and split-filing refuses to emit a lead phase that is itself over cap.

- **Irreducible is the ONE plan-artifact field an enforcement path reads — but only to WIDEN.** `overCapSplitRejection` gains a `if parsedPlan.Irreducible.Declared() { return "" }` **after** the count decision and the `split_proposal` short-circuit, so a well-formed `irreducible` (a non-blank `rationale`, checked by the shared `plan.Irreducible.Declared()`) with no `split_proposal` converts the count-derived HARD reject into an operator-decidable advisory. The guarantee is **positional, not merely intended**: the `!ok || !over` return precedes everything, so an under-cap plan returns before the field is read — no under-cap plan can change behaviour, reconciling this with the #2055 rule that a read field must never make the gate *reject* a plan the count says is fine. `plan.semanticCheck::checkIrreducible` rejects `irreducible`+`split_proposal` (contradictory) and a whitespace-only rationale (a bare unjustified flag).
- **Irreducible never suppresses the count advisory.** `runPlanWarnings` appends the count-derived `overCapWarning` FIRST, then `phaseCapWarnings` (one advisory per phase whose own `scope.files` count exceeds the cap) and `irreducibleWarning` (surfaces the rationale as a *challengeable* claim and states plainly the declaration does NOT make the change landable — implement re-checks `max_files_changed` against the real diff, so it still needs a governed cap raise). Ordering is the observable proof of non-suppression; both #2412 legs fail open on an unresolved cap.
- **Split-filing refusal (`fileSplitProposalChildren`).** Immediately after resolving the cap and BEFORE building/classifying/filing any child, the hook calls `splitfiling.PhaseCapViolations`; when any phase is over cap it files **zero** children, writes ONE `split_filing_refused` marker (registered in `audit.KnownCategories`, surfaced via `fishhawk_get_plan` `split_filing.refused`) naming every offending phase, and posts a parent refusal comment — so #2410 (a 160-file lead phase against a cap of 45) is never emitted. The marker append is a **hard prerequisite** (on failure it logs and returns WITHOUT the comment, mirroring `writeSplitChildFiledMarker`), and it **dedups on a prior `split_filing_refused`** so a re-approval of a still-over-cap proposal no-ops. `capFiles == 0` (unresolved cap) yields no violations, so filing proceeds unchanged. Split-filing still does **not** verify independent landability (path-keyed couplings): every child body carries `splitfiling.LandabilityCaveat` and the non-contract done-means drops the unsatisfiable "compiles as an at-or-under-cap intermediate" claim.

### Forge-state (query-before-file) idempotency for split-child filing (`split_filing.go`, E50.7 / #2064)

`fileSplitProposalChildren` dedups on the **audit log** — a per-ordinal `work_item_filed` marker written immediately AFTER `provider.File`, with a hard abort when that append fails. The residual is the interleaving where **File SUCCEEDS and the marker append then FAILS**: the ordinal has no durable record, so a re-approval re-files a DUPLICATE child. #2064 closes it with FORGE STATE.

- **Stamp, then query before filing.** Every filed child carries a deterministic hidden idempotency marker (`splitfiling.SplitChildKey(runID, phaseIndex)`, routed through `workmgmt.FilingRequest.IdempotencyKey` so `Apply` renders it into the body and the provider carries it onto the create request). Before filing any un-recorded ordinal, `loadAdoptableSplitChildren` queries the parent's children **exactly once** through the existing `workmgmt.EpicChildrenQuerier` seam; a match is **ADOPTED** — its number and the forge's own url fill the in-memory `filed` map and the missing durable marker is written — instead of re-filed.
- **The adopted url comes from the forge (`EpicChild.URL`), never composed.** `githubclient.Client.BaseURL` is configurable, so a literal `https://github.com/{owner}/{repo}/issues/{n}` would be wrong on a GitHub Enterprise Server host. `TestSplitFiling_MarkerAppendFails_ReapprovalAdoptsInsteadOfRefiling` asserts the adoption-pass url is **byte-identical** to the url the creation pass recorded for that same child.
- **The child path fails CLOSED; the comment path fails OPEN. The asymmetry is chosen.** A children-query ERROR aborts the pass before ANY creation (no completion marker, run resumable) — filing blind on an unreadable child set is precisely what creates the duplicate this change removes, so an unknown forge state must never be read as an empty one. A parent-thread LISTING error, by contrast, posts the comment anyway: a MISSING operator-facing acceptance-carrier or refusal comment is worse than a duplicated one, whereas a duplicate child ISSUE is the bug being fixed. Both directions are documented at the call sites and pinned by `TestSplitFiling_QueryErrorAbortsBeforeAnyCreation` and `TestSplitFiling_CommentListErrorPostsAnyway`.
- **Duplicate-key tie-break: LOWEST NUMBER WINS — an intentional choice, not iteration order.** Two children can bear the same key only after a prior CONCURRENT re-approval. The lowest-numbered child is the earliest-created, so every later pass converges on the same one regardless of the order the forge returns them in; a first-match-wins tie-break would let two passes adopt different children and record different numbers for one ordinal. **The second child is NOT reconciled** — nothing closes or links it, and an operator resolves the duplicate. `TestSplitFiling_DuplicateKeyTieBreakIsDeterministic` pins it.
- **Parent comments are STAMPED on the way out, and that has its own counterfactual.** Both `postSplitParentComment` (acceptance carrier) and `postSplitRefusalComment` (#2412) stamp their outbound body and skip the post when the thread already carries that key. The read half can only ever find a marker the write half put there, so an unstamped outbound comment would leave the dedup **silently inert in production** while every reader-side test (which SEEDS a keyed comment) still passed. `TestSplitFiling_ParentCommentCarriesIdempotencyMarker` / `..._RefusalCommentCarriesIdempotencyMarker` assert on the body the fake server RECEIVED; deleting either stamp reddens them.
- **The comment dedup reads the WHOLE thread.** `TestSplitFiling_CommentDedupReadsWholeThread` seeds page 1 with only unrelated comments and puts the keyed comment on **page 2** behind a `rel="next"` Link header, so the zero-new-posts assertion can hold only for a reader that follows the link. Deleting the pagination follow in `githubclient.ListIssueComments` reddens it with a duplicate post. `TestSplitFiling_CommentDedupToleratesCRLF` covers the `\r\n` round-trip (its fixture appends a trailing newline **before** converting line endings — otherwise the trailing marker line carries no `\r` and the case cannot discriminate).

**Three honestly-narrowed claims — read these rather than the headline.**

- **At-most-once is SEQUENTIAL-only.** `EpicChildren → File` and `ListIssueComments → PostComment` are not atomic, so two CONCURRENT re-approvals can both observe nothing and both create: a named residual of **at most one duplicate**, deliberately not serialized (a leaked distributed-lock session wedging the filing path entirely is a worse failure). `TestSplitFiling_SequentialReapprovalFilesAtMostOnce` claims only what the code holds.
- **The degrade guarantees apply to QUERIER-CAPABLE providers only.** `jira` does not implement `EpicChildrenQuerier`, so for it the window stays open exactly as today (`gitlab` implements it since #3658, so a gitlab split filing now takes the querier-capable path) — fail-open by design, pinned by `TestSplitFiling_CapabilityLessProviderKeepsTodaysPath`. Note the stamping is NOT gated on the capability: a capability-less provider still files keyed bodies, so a later capability gain finds the markers already in place.
- **A failed sub-issue LINK is an uncovered residual.** A child whose create succeeded but whose link to the parent then failed is not in the parent's sub-issue graph, so the query cannot return it and it is re-filed. Narrower than the pre-change window; closing it needs a repo-scoped search rather than a sub-issue sweep. There is no test — this is stated as a residual, not a claim.

**Failure-mode coverage.** m1 query error → abort before any creation; m2 empty result → normal path; m3 non-matching child → normal path; m4 near-miss key (both directions) → not adopted; m5 CLOSED child bearing the key → adopted; m6 capability-less provider → no query, today's path; m7 comment listing error → posted anyway; m8 marker append failure on the ADOPTION path → abort, no completion marker; m9 duplicate keys → deterministic tie-break.

### Close-parent-when-contract-child-lands watcher (`split_parent_close.go`, E50.6 / #2062)

`fileSplitProposalChildren` files a split proposal's phased children and posts a parent comment asking the operator to close the parent by hand once the contract-phase child lands (#2057). `handleContractChildClosed` retires that manual step: it is a second issue-closed consumer — on GitHub a SIBLING of the #1817 board-sync reconciler on the same `issues.closed` event (that one moves a card, this one closes a parent); on GitLab the ONLY consumer of the `issue`/`close` event — best-effort and never influencing the 202. **Forge-neutral since E50.17 / #2900**: every forge call dispatches through the standalone `forge.IssueOperations` capability (`FetchIssue` / `FetchIssueComments` / `PostIssueComment` / `SetIssueState`, `backend/internal/forge/issues.go`), implemented by both registered adapters, so the watcher runs on GitHub and GitLab from ONE function.

**The forge is the SOLE AUTHORITY for every decision with a side effect.** "Is the parent already closed?" is answered by `FetchIssue` (every adapter normalizes its native state word onto `open`/`closed`, so GitLab's `opened` cannot slip past the comparison). "Has the linking comment already been posted?" is answered by re-reading the parent thread for a `splitfiling.ParentCloseCommentKey` marker. Consequently there is **no lock, no cross-run fold, and no `split_parent_closed` idempotency record** — the audit entry this file writes is a pure OBSERVATION that nothing reads to gate behavior, so an append failure changes nothing (`TestContractChildClosed_ObservationAppendFailure_DoesNotUnwind`).

**Linkage is a PURE PAYLOAD READ, and that is what removes a whole defect class.** #2062 widened `split_children_filed` additively with `parent_repo` + `parent_issue`, and #2900 added `parent_forge`, so `resolveSplitParentLinkage` answers "which parent does this closed issue belong to?" from `AuditRepo.ListAll` alone — no run resolution, no forge call. The credential identity comes off the `webhook.Event` itself — `InstallationID` (an `int64` that is ZERO when the event isn't installation-scoped) for GitHub, `CredentialRef` (`gitlab:<project-id>`, EMPTY when the payload carried no project id) for GitLab — derived per family by `splitParentCredentialScope` into ONE `forge.CredentialScope`, so the `*run.InstallationID` nil-deref the run-resolving design carried is **structurally absent** here rather than merely guarded.

**A linkage is bound to the forge FAMILY it was filed under (`parent_forge`, #2900).** Repo path + issue number are NOT a cross-forge-unique key: a GitHub repo and a GitLab project can share an identical `path` AND issue number, so without a forge discriminator a GitHub-filed split linkage would match a colliding GitLab child close (or vice versa) and close an UNRELATED parent on the other forge. `writeSplitChildrenFiledAudit` stamps the run's forge family onto the marker (`splitParentForgeFamilyFromRef` maps `runRow.InstallationRef`: a `gitlab:<id>` ref → `gitlab`, everything else → `github`), and `resolveSplitParentLinkage` requires `normalizeSplitParentForge(parent_forge)` to equal the delivering forge (`splitParentForgeID(ev)`) before a candidate survives. An ABSENT `parent_forge` on a pre-#2900 marker reads as `github` — every historical marker was GitHub, so existing linkages keep matching (operator-confirmed backward-compat rule). `TestSplitParentClose_ForgeFamilyBinding` is the counterfactual: a linkage filed under ONE family closes only that family's parent and leaves the colliding parent on the OTHER family untouched; deleting the read-side forge filter reddens the "stays open" assertion on a real committed-state change. `TestSplitParentForgeFamily` pins the two mappers in isolation; `TestFileSplitProposalChildren_StampsParentForge` (#3306) pins the PRODUCER instead — the `parent_forge` value `writeSplitChildrenFiledAudit` actually persists on the real `fileSplitProposalChildren` hook's emitted `split_children_filed` payload for a `gitlab:` ref versus every other ref, closing the gap where every prior assertion either hand-seeded `ParentForge` or exercised the mappers alone.

**Ordering is load-bearing, twice.**

- **Linkage BEFORE every linkage-dependent outcome.** The credential-scope and `state_reason` gates run AFTER linkage, never before. An unrelated issue in the repo closed as `not_planned`, or any close lacking credential data, must write ZERO audit entries — an observation about a split it has nothing to do with would be a false record. Nothing forces the other order: the scope is needed only for the forge calls, which come last. `TestUnrelatedIssueClosed_NotPlanned_WritesNothing` / `..._ZeroInstallation_WritesNothing` (GitHub) and `TestSplitParentClose_GitLab_UnrelatedIssue_NoCredentialRef_WritesNothing` (GitLab) assert zero audit entries and zero forge calls; hoisting the scope gate ahead of linkage reddens them (run empirically in #2900's slice 2: a false `no_installation` observation appears).
- **COMMENT FIRST, THEN CLOSE.** The close is what stops future deliveries reaching the forge (a closed parent short-circuits at `already_closed`), so closing first would make a transient comment failure PERMANENT. With comment-first, a torn delivery leaves the parent OPEN with the comment present, and the next redelivery finds the marker, skips the post, and closes. `TestContractChildClosed_ClosesParentWithLinkingComment` (GitHub) and `TestSplitParentClose_GitLab_ClosesParent` (GitLab: `POST …/notes` strictly before `PUT …/issues/:iid`) assert on the fake's recorded CALL ORDER (counts alone cannot pin an ordering property); `..._CommentPostFails_ParentStaysOpen` / `TestSplitParentClose_GitLab_PostNoteFailure_DoesNotClose` assert ZERO `PATCH`/`PUT` calls after a failed comment.

**Every defined skip.** `AuditRepo` nil; an EMPTY `ev.Repo` (stated explicitly so the legacy no-op holds as a rule rather than depending on an empty-vs-empty comparison failing — without it, an empty-repo event MATCHES a legacy entry and the `state_reason` gate writes a false observation); an undecodable payload or non-positive issue number; a `ListAll` error (WARN, return, change nothing, **no** observation — an unreadable linkage is not a fact about any split); no linkage match (silent, zero writes — this is also the path a non-contract child close takes); AMBIGUOUS linkage; a ZERO credential scope (GitHub `InstallationID == 0`, GitLab empty `CredentialRef` — recorded as `no_installation`, the pre-parity name kept so chain readers keep matching); a nil `forge.IssueOperations` from the per-family ladder below (a server misconfiguration, not a fact about the split, so INFO log and no observation); an unsplittable repo full name; a forge family whose payload shape the watcher does not know (neither `github` nor `gitlab`: silent, zero writes); and each of the four forge errors.

**Per-family forge resolution is UNAMBIGUOUS (`issueOpsFor`, shared with the prompt handler's forge-neutral issue fetch since E45.42 / #3347), mirroring `prStateReaderFor` (E64.40 / #3151).** A github-family delivery (`ev.Forge` empty or `github`) resolves ONLY through `cfg.GitHub`, wrapped by `forgegithub.New`; it NEVER consults `cfg.ForgeResolver` or the process registry, so registry availability can never change a GitHub outcome (`TestSplitParentClose_ForgeFamilyBinding/github_delivery_never_consults_the_resolver` installs a resolver that fails the test if reached). Any other family resolves through `cfg.ForgeResolver` (nil → `forge.Get`) plus a type assertion to `forge.IssueOperations`; a resolver error, a nil forge, a TYPED-nil forge inside a non-nil interface (`isNilForge`), or a forge lacking the capability is a nil result — each pinned as a zero-call, zero-observation skip by `TestSplitParentClose_GitLab_ResolverNilRungs` (deleting the `isNilForge` guard or the assertion's `ok` check turns the rung into a recovered panic → 500). The `github delivery never consults the resolver` subtest of `TestSplitParentClose_ForgeFamilyBinding` installs a resolver that fails the test if reached, pinning that registry availability can never change a GitHub outcome. (That same test's colliding-identity subtests pin the ORTHOGONAL read-side `parent_forge` binding — a linkage owned by one forge must not close the colliding parent on the other — described under the linkage-read section above; adapter resolution and linkage ownership are distinct guarantees the one test exercises with both fakes live.)

**GitLab routing shape.** A GitLab issue close arrives at `POST /webhooks/gitlab` as object_kind `issue` (SINGULAR — `ev.Type` is the payload's object_kind) with `object_attributes.action` `close` (GitLab's bare imperative, not GitHub's `closed`), the issue number at `object_attributes.iid`, and the credential scope in `ev.CredentialRef` (`gitlab:<project.id>`, stamped by `webhook.ParseGitLabEvent`) — the literal `issues`/`closed` comparison could never fire for it. `isIssueClosedDelivery` (`webhook.go`) is the shared predicate both receivers route through; `TestIsIssueClosedDelivery_ForgeVocabulary` pins both positives and the negatives (GitLab `open`/`update`/`reopen`, GitHub `reopened`, each vocabulary under the other forge's id, an unknown forge), and `TestSplitParentClose_GitLab_NonCloseActions_NotRouted` / `TestSplitParentClose_GitHub_ReopenedNotRouted` drive the negatives end to end. The #1817 board-sync reconciler keeps its own GitHub-only routing — widening it is not #2900.

**Nested-group repo paths split on the LAST slash (`splitParentRepoRef`).** GitLab paths may nest (`group/subgroup/project`), which the shared `splitRepoFullName` rejects (it forbids a slash in the name half, correctly for its GitHub-shaped callers), so the watcher carries a local last-slash split and leaves the shared helper untouched. The GitLab adapter addresses projects by the scope's numeric id and ignores the `RepoRef`, so on that path the split is only cosmetic today; `TestSplitParentClose_GitLab_NestedGroupPath` asserts every call reached `/api/v4/projects/<pid>/…` and the parent closed, and `TestSplitParentRepoRef` pins the split table.

**Named GitLab residual: the `state_reason` gate is INERT there.** GitLab's issue object has no `state_reason` field ([Issues API](https://docs.gitlab.com/ee/api/issues.html) documents `state` as `opened`/`closed` only), so a GitLab contract-child close always records `state_reason: ""` and PROCEEDS — exactly the rule the GitHub path already applies to the missing/null form GitHub sends for a plain close. No GitLab equivalent is fabricated (a "closed as duplicate" there is a label or quick action, not a lifecycle field). `TestSplitParentClose_GitLab_ClosedChildWithNoStateReason_ClosesParent` asserts the fixture carries no `state_reason` and the parent still closes; the GitHub gate remains live under `TestContractChildClosed_NotLandedStateReasons_LeaveParentOpen`. The close itself is sent as `state_event: "close"` (GitLab refuses a direct `state` write) and the `completed` reason is dropped per the capability's best-effort contract.

**AMBIGUOUS LINKAGE is a defined skip, not an arbitrary pick.** Issue-number uniqueness proves the CHILD is unique; it does NOT prove two payloads agree on the PARENT. When two surviving same-repo matches name the same contract child but DIFFERENT `parent_issue` values, taking the newest could close the WRONG parent — an unrecoverable, operator-visible error. The watcher touches no forge and records `outcome: "ambiguous_linkage"` naming both candidates (`TestContractChildClosed_ConflictingLinkage_SkipsAndAudits`).

**The `state_reason` decision, and its justification.** A contract child closed as `not_planned` or `duplicate` did NOT land, so closing the parent would falsely assert completion — the same disposition the sibling #1817 reconciler takes on the same event. The parent is left open, the operator's manual close remains correct and available, and the skip is audited as `child_not_landed`. Every other value — `completed`, and the missing/null form GitHub sends routinely for a plain close — PROCEEDS (`TestContractChildClosed_NullStateReason_ClosesParent` covers the absent-key, explicit-`null` and `"completed"` forms).

**`FetchIssueComments` fails CLOSED here — deliberately opposite to `splitParentThreadHasComment` in `split_filing.go`.** There, a missing operator-facing comment was worse than a duplicate. Here that read IS the entire idempotency record, so posting blind would duplicate the comment on every redelivery. The parent stays open and the next delivery retries (`TestContractChildClosed_ListCommentsError_DoesNotPostBlind`, `TestSplitParentClose_GitLab_ListNotesFailure_PostsNothing`). Do not "fix" one to match the other. The marker itself is unchanged for GitLab — the `splitfiling.ParentCloseCommentKey` HTML comment is invisible in GitLab Flavored Markdown too, and the GitLab client pages the notes list to exhaustion so a marker on page 2 is still found (`TestSplitParentClose_GitLab_MarkerOnSecondPage_SkipsComment`). `TestSplitParentClose_GitLab_RedeliverySkipsComment` is the marker counterfactual vehicle: it seeds the parent OPEN with the stamped note ALREADY present (the state a failed `PUT` leaves behind) — the only shape that reaches `ThreadHasComment` past the already-closed short-circuit — and deleting the check produces a second note.

**Honestly-narrowed claims.**

- **Exactly-once is SEQUENTIAL-only.** Two GENUINELY CONCURRENT deliveries for the same contract child can interleave between the thread read and the comment write, both see no marker, and both post — a DUPLICATE LINKING COMMENT. The close does not duplicate (the second `UpdateIssue` is a no-op close on an already-closed issue). This window is deliberately left open: a duplicate comment is cosmetic, and a distributed lock's failure mode — a leaked lock wedging every later delivery — is strictly worse. `TestContractChildClosed_DuplicateDeliveryIsIdempotent` claims only the sequential property the code holds.
- **Splits filed before #2062 still need a manual parent close.** A legacy `split_children_filed` entry carries neither `parent_repo` nor `parent_issue`, so it matches nothing. That is the intended fail-quiet direction, asserted by `TestContractChildClosed_LegacyPayloadWithoutParentRepo_NoOp` rather than left to inference.
- **Cross-repo child filing would break the repo comparison.** `fileSplitProposalChildren` builds the children's `workmgmt.Target.Repo` from `runRow.Repo`, the same value stored as `parent_repo`, so the watcher compares the closed issue's repo against `parent_repo` directly. If cross-repo filing is ever added, the payload needs a separate `child_repo` and this comparison must move to it.

**Live operator walk — GitLab round trip (E50.17 / #2900; needs the #2032 public-bind setup).** The parity suite proves the seam against a stateful fake; this walk proves it against a real GitLab. Run it against the E45 GitLab instance (`docs/deploy/gitlab.md` for the `FISHHAWKD_GITLAB_BASE_URL` / `FISHHAWKD_GITLAB_TOKEN` / `FISHHAWKD_GITLAB_WEBHOOK_SECRET` trio; the receiver needs a publicly reachable bind, the same prerequisite the #2032 walk records). Every step below is checkable from the shell; `$GL`, `$TOKEN`, `$PID` are the instance URL, a project-scoped token, and the numeric project id.

1. **Webhook.** In the project: Settings → Webhooks → URL `<fishhawkd external URL>/webhooks/gitlab`, Secret token = the value of `FISHHAWKD_GITLAB_WEBHOOK_SECRET`, trigger **Issues events** only. Send the test event; fishhawkd must log `webhook received forge=gitlab event=issue` and answer 202.
2. **File a split with a contract child.** Drive a run on a project issue whose plan is a `split_proposal` with a contract phase and approve it; `fileSplitProposalChildren` files the children and writes `split_children_filed`. Confirm the linkage the watcher will read: `GET /v0/audit?category=split_children_filed` (bearer token) must show an entry whose `parent_repo` equals the project's `path_with_namespace`, whose `contract_child_number` is the child's iid, and whose `parent_forge` is `"gitlab"` (the #2900 forge binding — a GitLab run stamps it so the close delivery, which arrives with the gitlab forge family, matches) — note them as `$PARENT` and `$CHILD`. `parent_repo` is the SAME string GitLab puts in `project.path_with_namespace` on the webhook; if the two ever differ the watcher fails QUIET (no linkage match). A GitHub project sharing this path would file its own marker with `parent_forge: "github"`, and this GitLab close would NOT match it.
3. **Close the child.** `curl -sS -X PUT -H "PRIVATE-TOKEN: $TOKEN" "$GL/api/v4/projects/$PID/issues/$CHILD?state_event=close"` (or close it in the UI).
4. **Expect, in this order.** (a) fishhawkd logs `webhook received forge=gitlab event=issue action=close`. (b) The parent carries exactly ONE new note: `curl -sS -H "PRIVATE-TOKEN: $TOKEN" "$GL/api/v4/projects/$PID/issues/$PARENT/notes" | jq '[.[] | select(.system == false)] | length'` grew by one, its raw `body` names `#$CHILD` and ends with the `<!-- … -->` idempotency marker, and in the parent's RENDERED view the marker is INVISIBLE (only the linking prose shows). (c) The parent is closed: `curl -sS -H "PRIVATE-TOKEN: $TOKEN" "$GL/api/v4/projects/$PID/issues/$PARENT" | jq -r .state` prints `closed`. (d) `GET /v0/audit?category=split_parent_closed` shows one entry with `outcome: "closed"`, `commented: true`, `state_reason: ""`, `parent_repo` = the project path.
5. **Redeliver.** Settings → Webhooks → Recent events → Resend on the close delivery. Expect NO second note, the parent still closed, and a second observation with `outcome: "already_closed"`. To exercise the marker path itself, reopen the parent (`state_event=reopen`), leave the note in place, and resend: still ONE note, parent closed again, observation `closed` with `commented: false`.

**The producer-to-consumer seam has ONE test, and it is not optional.** `TestSplitFiling_ProducerToConsumer_ParentCloseEndToEnd` runs the real `fileSplitProposalChildren`, then delivers a signed `issues.closed` for the contract child the PROVIDER actually filed (the number comes from the provider's recorded creation, never from the audit payload under test) into a second Server sharing the same audit store. A producer assertion plus a hand-seeded consumer fixture cannot cover this: both halves would agree on a wrong value. Changing the producer to store the bare repo NAME instead of `runRow.Repo` reddens it. Note the honest limit — because both halves decode through ONE Go struct, a json-TAG rename is symmetric and this test does not (and cannot) discriminate against it.

### Charter admission gate for backlog-grooming workflows (`charter_gate.go`, ADR-065 / E54.4 / #2236)

A backlog-grooming run ranks the backlog against the repo's **charter** — every ranking it proposes cites a rubric line by id — so a grooming run in a repo with no charter has nothing to anchor on. ADR-065-as-amended says plainly that there is no unanchored-grooming mode, and `checkCharterDeclared` is where that becomes a refusal: `422 charter_required`, no run row.

**WHERE THE RULE IS ENFORCED.** **Run admission is the load-bearing enforcement point** — on every seam that mints a run, which today means `POST /v0/runs` and the campaign item-run start (`StartRunForCampaignIssue`). It is what makes *starting* an unanchored grooming run impossible, it fails closed on a loader error, and it is **unchanged** by the static half (AC5) apart from the message single-ownership extraction below. The rule is **also** checked statically now, by `fishhawk validate` (E54.11 / #2801): the CLI reruns the same `grooming_report` structural discriminator over the spec and reads the repo's **working-tree** `.fishhawk/work-management.yaml` for a `charter:` block, rendering the shared `MsgFmtCharterRequired` template when a grooming workflow has none.

**The two are an earlier warning plus the load-bearing gate, not equivalents.** The CLI decides the charter question from the **local working tree**; run admission decides it from the **forge-fetched** conventions at the run's ref (through the TTL-cached loader that also consults the provider discriminator and the `FISHHAWKD_WORKMGMT_CONVENTIONS` break-glass override). On a dirty or diverged checkout, or under the override, the two can legitimately disagree. **The static check can be LESS strict than admission (a missed early warning), never more** (a false refusal of a spec the product accepts) — the CLI validates the conventions file against the canonical `work-management-v0` schema it now mirrors, but does **not** reproduce the backend's cross-field *semantic* checks (the mandatory-field trio, provider-connection requirements, ADR numbering), which live in Go and not the schema, so a conventions file that violates **only** a semantic rule is admitted by the CLI though run admission refuses it. That residual direction is deliberate; see `cli/README.md`. The message template and the three reason values are held byte-identical across the two modules (which cannot share a package) by `TestCharterMessageParityAcrossModules`. `WorkflowRequiresCharter` stays **pure** (no receiver, no I/O); the CLI reruns an independent structural twin over its raw YAML tree rather than importing it, held to this copy by that parity test.

- **The discriminator is STRUCTURAL: a stage declaring `produces: [{artifact: grooming_report}]`.** Not the workflow's name (`backlog_grooming`), which is trivially evaded by renaming it, and not a `kind:` field, which the epic's AC1 forbids — the grooming workflow is built on the standard plan/implement/review stage types and is recognised by what it PRODUCES. `TestWorkflowRequiresCharter_StructuralDiscriminator` pins all three legs, including a workflow *named* `backlog_grooming` that produces no report and therefore does not trip the gate.
- **The gate is NARROW, and that is a tested property, not an intention.** An ordinary code-change workflow returns at the `WorkflowRequiresCharter` early return and never reaches the conventions loader, so it cannot be refused by a rule that has no business applying to it. `TestCharterGate_NonGroomingWorkflowWithoutCharter_Admits` is the pin: deleting that early return reddens it with a 422.
- **Three branches share the 422, so `details.reason` is what distinguishes them** — `charter_absent` (conventions loaded, no `charter` block), `charter_path_empty` (a block whose `path` is empty or whitespace, which anchors nothing), `conventions_unavailable` (the load itself failed). Every refusal test asserts the reason as well as the status; without that, deleting one branch would leave the other two branches' tests green.
- **A conventions-load ERROR fails CLOSED.** Admitting on a transient forge fault would let a grooming run start unanchored — the one outcome ADR-065 rules out — and it matches `RepoConventionsLoader`'s own documented posture (a fetch/parse failure other than `ErrNotFound` fails closed). The cost is that a forge outage blocks grooming run *creation*, which is the correct direction for a governance gate. Note the interaction with the default: `workmgmt.Default()` DOES declare `charter: {path: .fishhawk/charter.md}`, so the fallback posture admits and only a repo whose own `.fishhawk/work-management.yaml` omits the block is refused.
- **The refusal audit is BEST-EFFORT — the deliberate asymmetry `grantAppliesToOverride` documents (#2361).** A REFUSAL is the safe outcome, so failing to record it must not convert an audit outage into a governance outage; a GRANT records an exception being made, so its append is a precondition. This gate issues no grants, so it has only the best-effort half. `TestCharterGate_AuditAppendFailure_RefusalStands` pins it: the 422 stands when every append fails.
- **Placement: immediately after `checkAppliesTo`, before `CreateRunForTrigger`.** Same seam and the same two properties: **pre-insert** (a refusal leaves no run row — `TestCharterGate_MissingCharterBlock_Refuses` reads the run repo *after* the call, because a control that fired and rolled back would return a byte-identical envelope) and **post-replay** (an `Idempotency-Key` resolving to an existing run short-circuits above, so a replay re-evaluates no configuration decision and appends no second entry — `TestCharterGate_ReplayShortCircuitsBeforeGate` removes the charter *between* the two deliveries of the same request and still expects the 200).
- **`conventionsLoader` is a process-wide package var**, so every test here swaps it with a `t.Cleanup` restore and none calls `t.Parallel` — the established pattern in this package.
- **The `haveStageDefs` guard at the call site is a NARROWING, not the safety property.** The gate sits inside the existing `haveStageDefs` admission region beside `checkAppliesTo`, so it is fair to ask whether a grooming run could reach `CreateRunForTrigger` with that flag false and bypass it. It cannot: **both** spec-resolution branches in `handleCreateRun` (inline `workflow_spec` and the GitHub fetch) set `haveStageDefs = true` in the same statement that assigns `workflowDef`, and each rejects a zero-stage workflow *before* reaching it — so `haveStageDefs == false` means no workflow definition was resolved **at all**, `workflowDef` is the zero `spec.Workflow`, no stage rows are created, and `WorkflowRequiresCharter` (which iterates `wf.Stages`) is false **by construction**. A grooming-capable workflow necessarily declares a `grooming_report`-producing stage, so it can never sit behind that branch. Deleting the guard and re-running the suite leaves it green, which is the empirical form of the same statement: the **structural early return**, not the guard, is the load-bearing control. `TestCharterGate_NoStageDefsPathCarriesNoWorkflow` pins the observable half — the no-spec path creates a run with zero stages, with the loader stubbed to the worst case (no charter at all), so a future path that began carrying a grooming-capable workflow there would turn that 201 into a 422.
- **EVERY `CreateRunForTrigger` caller is gated, and that is a source-level pin rather than a claim.** The mint seam has two callers: `handleCreateRun` and `StartRunForCampaignIssue` (reached by the campaign item-run endpoint and by the campaign driver). The campaign seam is **not** structurally barred from carrying a grooming workflow the way the `haveStageDefs == false` branch is — it fetches the repo's spec from the forge and resolves an **operator-named** `workflow_id` out of it, so a repo whose spec declares a `grooming_report`-producing workflow can name it there. Documenting that seam as unreachable would therefore be false, so it is **gated**: the decision core is factored into `evaluateCharterAdmission` (discriminator → loader → reason → best-effort audit) and consumed by two arms that differ only in how they REPORT the verdict, never in the verdict — `checkCharterDeclared` writes the 422, and `ensureCharterDeclared` returns an `errCharterRequired`-wrapped error carrying the same reason and message for callers holding no `ResponseWriter`. Both fire **pre-mint**, so "refused before any run row exists" holds on both seams; the campaign arm's error surfaces through the item-run endpoint's existing `502 campaign_run_start_failed` mapping and leaves a driver-started item un-started for the next tick. `TestCharterGate_CampaignSeam_*` pins the seam behaviourally (refusal + fail-closed + both non-vacuity controls, reading the run repo and the audit fake after the call), and `TestCharterGate_EveryCreateRunForTriggerCallerIsGated` is the AST source scan — modelled on `run/childparams_gate_test.go` — that a **third** caller cannot be added ungated. Deleting the `ensureCharterDeclared` call reddens all three at once; so does returning `""` from `charterAdmissionReason`'s `charter_absent` arm, which reddens the HTTP and campaign refusal tests together and is the empirical form of "one core, two arms".
- **AC7's report-mode behaviour is pinned at the RUNTIME consumer, in `charter_gate_test.go`, not only in the spec package.** The spec-package `TestShippedGroomingExample_ReportModeDerivesNoAuthorityOrGate` asserts the *static* shape of the shipped declaration (empty `may_*` knobs, the declared gate inventory, an empty page list) — necessary but **not sufficient**, because a runtime consumer could begin treating a `mode: report` class as gated, or park the run on it, without touching either the workflow's `Gates` slices or `ResolveAutonomy`'s `PageHumanOn`. `TestShippedGrooming_ReportModeNeitherGatesNorParksAtTheConsumer` closes that: it drives the shipped declaration (read from disk) through the real `delegation.Evaluate` + `AutoDriveRunGate` report arm at a **live** approval gate and asserts the consumer derives no decision, surfaces no proposal, dispatches nothing, pages nothing, moves no stage, does not change the run state, and appends no `run_auto_driven` row — every assertion read as committed state *after* the call. `TestShippedGrooming_ReportArmIsLiveInThisHarness` is its paired control (a report entry on a backend-known class in the same harness DOES surface a proposal), so those zeros are the consumer answering rather than the consumer never running. Note the helper that creates the run, `startShippedGroomingRun`, no longer carries an `applies_to_override` (E54.22 / #2826): it starts the run with `trigger_source: on_demand`, which `appliesto.TriggerFormForSource` maps to `spec.TriggerOnDemand`, so the shipped `trigger: [scheduled, on_demand]` declaration admits it on its own terms. That converts a standing workaround into a **regression test** — if the mapping breaks, every `TestShippedGrooming*` case fails at admission with a 422 against the declaration read from disk.

### `applies_to` workflow routing, fail-closed in two phases (`applies_to.go` / `applies_to_plan_gate.go`, E53.3 / #2226, ADR-066 fork 4)

A workflow's optional `applies_to` is a `$defs/predicate` (E53.1 / #2224) declaring **which changes may be routed through that workflow**. Enforcement is **fail-closed** and split across two points, because the predicate's criteria do not all have a producer at the same moment (ADR-066 fork 4 ratified enforcement "at `start_run`"; the operator's 2026-07-30 ruling §1 refined that to "at the earliest point each criterion has a producer").

The **pure evaluation core** — the two-phase split, the satisfying-workflow enumeration, the ONE message renderer, and the trigger/label `Change` builders — lives in **`backend/internal/appliesto`** (see its `README.md`), extracted verbatim (E53.10 / #2361) so it can be shared with the webhook dispatcher without an import cycle (`server` already imports `webhook`). This file is the HTTP/audit-shaped ADAPTER for `POST /v0/runs`; `applies_to_plan_gate.go` is the plan-gate adapter; the webhook admission gate (`webhook/applies_to_admission.go`) is the third seam. All three delegate to `appliesto.*` and pass a `Seam` discriminator so the trailing ways-forward sentence differs while the binding message shape does not.

| Criterion | Enforced at | Against | Code |
|---|---|---|---|
| `labels` | run admission, `POST /v0/runs` | the run's `issue_context.labels` snapshot | `applies_to.go::checkAppliesTo` |
| `labels` | webhook admission (`issues.labeled`, `/fishhawk run`) | the forge-authoritative `issue.labels[]` on the event | `webhook/applies_to_admission.go::refusedByAppliesTo` |
| `trigger` | run admission (both admission seams) | the run's `trigger_source` → `spec.TriggerForm` | `checkAppliesTo` / `refusedByAppliesTo` |
| `paths` | the **plan gate** (`handleShipPlan`) | the approved plan's `scope.files` union | `applies_to_plan_gate.go::appliesToPlanGateRejection` |

- **The `paths` deferral is what makes the design sound, not what weakens it.** At admission a code change has proposed no diff, so evaluating `paths` there could only match against zero paths — which the AND-across-types rule turns into a blanket refusal of every run. The first authoritative path set is `scope.files`, which is **binding rather than descriptive**: the scope pre-check plus the runner's post-hoc constraint check confine the implement stage to it. A run admitted under `paths: ["docs/**"]` and cleared at the plan gate is therefore *confined* to docs-only, not merely claimed to be. **Both** rejection points fire before any implement work, so a refusal costs a re-run and never half-applied work.
- **The `paths` quantifier is UNIVERSAL, and that is a deliberate divergence from `Predicate.Match`'s default.** `spec.Predicate.Match`'s `paths` rule is *existential* — any change path matching any glob satisfies it — which is right for its other consumers (`escalations`, ADR-068 conventions) because they ask "does this change *touch* such a file?". It is wrong for a confinement control: under existential semantics a plan scoping `[docs/x.md, backend/everything.go]` would satisfy `paths: ["docs/**"]` and the confinement guarantee above would be false. `planGateUnmatchedPaths` therefore applies the ∀ quantifier **over the ratified matcher used verbatim** — each path is handed to `Predicate.Match` one at a time and every one must be accepted. No second matcher is written and `predicate.go` is untouched (operator ruling §3). `TestAppliesToPlanGate_PartiallyMatchingScope_Rejects` is the pin: it is the only test that fails if this is "simplified" back to one `Match(union)` call.
- **The evaluated path set is the UNION, not the top level**: `scope.files` ∪ every `decomposition.sub_plans[].scope.files` ∪ every `split_proposal.phases[].scope.files` (`planGateScopePaths`, slash-normalized / de-duplicated / sorted). A decomposition fan-out child runs bounded to its **slice** scope, so checking only the top level would let a slice escape the declaration entirely — the same structural reason `scopedPaths` in `scope_regression.go` had to cover sub-plan scopes (#1257).
- **One renderer, three seams.** Every rejection point builds `appliesto.Rejection` and calls `appliesto.RenderRejection`, so all carry the same shape: the workflow, the criterion that failed, the value observed, and **which workflows would accept this change**. An operator refused at the plan gate is further into a run than one refused at admission and needs more help, not less. Only the trailing ways-forward sentence keys off the `Seam` field: `SeamStartRun` and `SeamPlanGate` advertise `applies_to_override`; `SeamWebhook` does NOT (a webhook trigger carries no operator request, so there is nowhere to pass one) and names re-starting through `POST /v0/runs` / the CLI instead. `TestRenderRejection_AllThreeSeamsCarryTheSameShape` (in `appliesto`) pins the shape parity and the tail divergence.
- **The alternatives list is filtered at BOTH phases, and at the plan gate that means the run's labels and trigger too.** The operator's next move after a plan-gate refusal is to re-start under a named alternative, and that re-start hits **admission** first — so a candidate filtered on `paths` alone lists every labels-only or trigger-only workflow unconditionally (they do not constrain the plan gate at all) and walks the operator from one fail-closed refusal into another. The run's labels (`issue_context.labels`) and trigger are immutable for its lifetime, so `resolveRunWorkflowDef` rebuilds the admission-phase `spec.Change` from the same run row (`runAdmissionChange`) and `planGateSatisfyingWorkflows` runs each candidate through `evaluateAppliesTo` at admission **before** the ∀ paths check. Pinned by `TestPlanGateSatisfyingWorkflows_AlsoFiltersOnAdmissionCriteria` (the combined label-incompatible-but-path-compatible edge) and `TestAppliesToPlanGate_RejectionNamesOnlyUsableAlternatives` (the same invariant through the real gate and the real run row).
- **The phase split is exhaustive over the predicate grammar, and a TRIPWIRE — not prose — is what keeps it that way.** `appliesto.PhasePredicate` is a hand-maintained field-by-field copy of `spec.Predicate` whose `constrains` check enumerates today's four criteria by name, so a *fifth* criterion added by a sibling slice (#2227 `escalations`, #2211 review conventions) would be picked up by the `applies_to` `$ref` automatically and copied into **neither** half — declared and silently never enforced, the failure mode the function's own comment names for `change_kind`. `TestAppliesTo_PhaseSplit_IsExhaustive` (now in the `appliesto` package) therefore asserts against the struct via `reflect`: a field-count check (with the actionable message: route it into a phase half **and** `constrains`, extend `appliesto.FirstFailingCriterion` — naming all THREE consumers) plus a per-field check that every criterion survives into at least one half. A new criterion fails the test rather than shipping unenforced.
- **Terminal path adds no audit category.** On a non-empty reason the gate emits `plan_review_failed` (`emitReviewFailed`), fails the stage category-B (`run.FailStage` + `advanceAfterFailure`) and sets `gatingRejected` — byte-for-byte the path `overCapSplitRejection` uses. It runs **after** the artifact is stored and audited, so the operator can read the refused plan via `fishhawk_get_plan`, and it is skipped when the over-cap reject already failed the stage (a stage fails once).

**The override's source of truth is the audit entry, which means the entry is a PRECONDITION of the bypass and not a record written after it.** The audited exception is one concept with one name: `applies_to_override` + a **required** reason on `POST /v0/runs`. It is granted in two writes:

- **The grant** (`grantAppliesToOverride`) appends `run_admitted_applies_to_override` to the **global** chain *before* the run is admitted — the run-scoped entry cannot be written here because there is no run id until the insert. An append failure, or an absent audit repository, **refuses the run** with `503 audit_unavailable` and creates no run row. Warn-logging and admitting anyway would be an unaudited governance bypass, and for an **admission-only** violation nothing downstream would ever catch it: `paths` is re-evaluated at the plan gate, so a lost override there costs a re-run, but a `labels` or `trigger` bypass has no second evaluation point. This is the deliberate inverse of the **refusal** audit, whose append failure is correctly best-effort (`TestAppliesTo_RefusalAuditFailure_StillRefuses`) — there the decision already went the safe way, so an audit problem must not soften it; here it goes the unsafe way, so the audit gates it. Pinned by `TestAppliesTo_M11c_OverrideGrantAuditFailure_RefusesTheRun` and `TestAppliesTo_OverrideWithNoAuditRepo_Refuses`.
- **The carry-forward** (`recordAppliesToOverride`) appends the **run-scoped** entry post-insert. The plan gate suppresses its own refusal by **looking that entry up** (`runHasAppliesToOverride`), never by re-reading request state — audit-entry lookup, no run column, no migration. **This append is a precondition too**: on failure `abandonUnauditedOverrideRun` CANCELS the just-created run and returns `503 audit_unavailable` (`details.run_id` names the cancelled run; the cancel itself is best-effort, the 503 is not conditional on it). Warn-logging and proceeding was the earlier posture, justified by "the plan gate re-rejects a deferred `paths` violation anyway" — true for `paths` and false for the violation the override most often forces past. An **admission-only** (`labels`/`trigger`) violation has no second evaluation point, so that run would complete on a bypass its **own** audit chain does not carry: a global-chain entry no run-scoped query returns is not the run's source of truth. No entry, no run — the same answer both writes give. `TestAppliesToPlanGate_OverrideAbsent_Rejects` and `TestAppliesTo_M11b_OverrideLookup_IsAuditEntryNotRequest` pin it (the latter asserts the 503, the cancelled row, and that the lookup still reports no override).

**The override is recorded independently of whether ADMISSION refuses, which is what makes it reachable for the deferred `paths` criterion.** Deriving the record from the refusal — the obvious reading, since admission is where the refusal happens — yields an override that exists only when admission itself refuses, and so is unavailable in the **common** case: a workflow whose `labels`/`trigger` the run satisfies (or which declares neither, the paths-only declaration) and whose `paths` the plan will not. That operator's only recourse would be widening the declaration permanently, which is the behaviour the override exists to avoid. `TestAppliesTo_M11d_Override_CarriesForwardWhenAdmissionPasses` covers both shapes; `TestAppliesTo_AdmittedRun_RecordsNoOverrideEntry` is the control that an ordinary run — one that asked for no override — still acquires none.

**The empty-label-set rejection has two sentences because it has two causes.** An issue-**less** run (`trigger_source` `cli`/`ui`, no issue context) and an **unlabelled** issue (issue context present, zero labels) share the branch and have different fixes, so telling the operator of an issue-triggered run that it "carries NO issue context" sends them hunting a trigger problem they do not have (`appliesToRejection.IssueContextPresent`). Both sentences still report the **observed value** (`[]`): the message shape is binding on every rejection path, and the empty-set branch is exactly where dropping it as uninteresting is tempting (`TestAppliesTo_M4b_UnlabelledIssue_ReportsTheObservedValue`).

**Degrade posture is deliberately ASYMMETRIC to this handler's advisory neighbours, and the line is drawn at a specific place rather than applied as a mood:**

- **Once a declaration is in hand, an evaluation failure REJECTS.** A `Predicate.Match` error (empty predicate, malformed glob) and an override-lookup error are both refusals — an override that cannot be *confirmed* is not an override. `runScopePrecheck`, `runSurfaceSweep`, `runTestSweep` and `overCapSplitRejection` all correctly fail **open** on their error legs because they are advisory sweeps whose worst case is a missing hint; this is a governance control whose worst case is an unenforced routing declaration. Writing `if err != nil { return "" }` here by analogy with the four neighbours sitting in the same handler is the specific bug this posture exists to prevent (`TestPlanGateUnmatchedPaths_MalformedGlob_FailsClosed`, `TestAppliesToPlanGate_OverrideLookupError_Rejects`).
- **Where NO declaration EXISTS to resolve, the gate fails open** (nil `RunRepo`, run not found, nil workflow spec, unparseable spec, workflow absent from the spec — each warn-logged). There is nothing to enforce, and refusing every plan in a deployment that cannot resolve a declaration would be a denial of service rather than a control. The leg is narrow by construction: the bytes parsed here are the **snapshot admission already parsed successfully**, so a parse failure at plan time is an internal inconsistency, not a reachable bypass.
- **Where the declaration could not be READ, the gate fails closed.** A `GetRun` error that is *not* `run.ErrNotFound` — a transient database failure, a timeout, a reset connection — is an **absence of knowledge**, not the fact "there is no declaration", and only the second is a safe reason to admit. `resolveRunWorkflowDef` returns the two separately (`ok bool, err error`) precisely so a caller cannot collapse them by reading one flag. Collapsing them would leave a governance control that any repository hiccup silently switches off, and switches off exactly when it matters: a plan that *satisfies* the declaration is admitted either way, so the only plan a blind gate changes the verdict on is one that would have been refused. The refusal is retry-shaped — the failure is transient, so re-shipping the plan recovers, whereas a silent admission never does (`TestAppliesToPlanGate_TransientReadFailure_FailsClosed`, `TestResolveRunWorkflowDef_SeparatesAbsenceFromUnreadability`).
- **A plan with an EMPTY scope is refused, not admitted on a vacuous ∀.** The zero-path case delegates to the predicate's own ratified answer (`(false, nil)` against zero paths) rather than to "every one of zero paths matched", which would be the one hole in the posture.
- **The gate's call site sits OUTSIDE `handleShipPlan`'s `json.Unmarshal(body, &parsedForCap)` guard, and an undecodable body is a zero-path plan rather than a skip.** That decode failure is a documented fail-**open** for the *advisory* over-cap gate it shares the block with; nesting this call inside it would hand a fail-closed control a fail-open leg that is **not** among the enumerated ones above — which is precisely the "narrow by construction and enumerated" claim this posture rests on. So the gate runs either way and is passed a `nil` plan when the decode failed (the partially decoded struct is deliberately not reused: a half-populated scope would let a truncated body attest a confinement it never demonstrated). A `nil` plan refuses under a `paths` declaration and still clears a workflow that declares none, so the leg cannot become a blanket plan-ship outage. Like the malformed-glob branch it is **not reachable through a stored body today** — `body` has already passed `plan.Validate` against the closed `standard_v1` schema — so it is pinned where the contract lives (`TestAppliesToPlanGate_NilPlan_Rejects`) rather than through a faked end-to-end path.
- The `Match`-error branch is **defense in depth and not reachable through a stored spec today**: slice 0's `validateWorkflow` calls `Predicate.Validate` at parse time, so a malformed glob fails `spec.ParseBytes` at admission before a run row exists. It is asserted directly against the pure function that owns the posture rather than through a faked end-to-end path.

**Trust boundary (documented, not implied).** On `POST /v0/runs` labels are fetched by the *caller's* `gh` and shipped inline on the create request, so a caller determined to route a change through the wrong workflow can attest whatever labels it likes. `applies_to` prevents **misrouting**, not a determined authorized caller; that is an accepted boundary for a routing control whose sanctioned exception is an audited override, and server-side label fetching is the named hardening follow-up. The **webhook** seam (E53.10 / #2361) is stronger on both counts: it evaluates against the forge's own `issue.labels[]` rather than a caller attestation, and it carries NO override (the event carries no operator request) — so its refusal is unconditional and the refusal comment names amending the declaration or re-starting through `POST /v0/runs` / the CLI. Overlap is benign by construction: `applies_to` *filters* an operator-named `workflow_id`, it never *selects* one, so two matching workflows is not a coin flip.

### Clarification answers are refused over-cap, never truncated (`clarification_answer.go` / `prompt.go`, #3063)

`handleAnswerClarification`'s answers are BINDING in exactly the #558 sense — the planner declared the issue NOT plannable without them, so they arrive in the resumed plan prompt as authoritative non-derivable facts. A silent cut therefore does not merely lose detail: it converts a question the operator RESOLVED back into a guess. So this channel takes the same **refuse-on-binding** posture `validateApprovalComment` (#2583) and `validateReviseConstraint` (#2871) established.

- **`validateClarificationAnswers`** refuses `len(rendered) > prompt.MaxClarificationAnswerBytes` (12000) with `400 validation_failed`, naming the byte count, the cap and the overflow, plus `details{field:"answers", bytes, max_bytes, overflow_bytes}`. It measures the **RENDERED** blob — the value actually persisted as the `clarification_answered` payload's `conditions` key and actually rendered into the prompt — not the raw per-answer sum, which excludes the `Q<id> (<question>): ` framing `renderClarificationAnswers` adds. A refusal measured on the raw sum would admit a payload the renderer then still cuts.
- **Ordering is the whole guarantee.** The check sits immediately after `renderClarificationAnswers` returns and BEFORE every stateful step: the `chainParams` build, all three resume tiers (`clarificationAnswerCommitter` / `clarificationResumer` / plain `TransitionStage`) and the audit append. So a refused call leaves the stage at `plan`/`awaiting_input`, writes NO `clarification_answered` entry, and the operator can simply re-answer. An error identity alone cannot distinguish that from "appended then refused", which is why `TestAnswerClarification_OverCapAnswersRefused_StageStillParkedNoAudit` reads the committed state after the call and then drives a successful under-cap re-answer.
- **What the old behaviour cost.** The blob was stored WHOLE and the call returned 200; the loss happened downstream, TWICE, at 4000 bytes each and silently at both layers — `loadClarificationAnswers`' `CapText` (the binding one in production) and each prompt renderer's own local `const maxAnswerBytes = 4000` blind, non-rune-safe byte prefix. The LAST answer died first while the earlier ones still looked complete. Observed live loss: run `7984a5a8`, 8256 bytes across three answers plus the trailing comment.
- **One owner for the cap.** `loadClarificationAnswers` and `prompt.writeClarificationAnswers` now apply the SAME constant, so the #2871 two-caps-lower-one-wins-silently shape cannot recur. With the gate in place the renderer's `CapTextWithRetrieval` elision is reachable only for a blob persisted BEFORE the gate shipped; it renders the ADR-077 marker plus a `risks_and_assumptions` declaration instruction written ABOVE the blob. Contract: `backend/internal/prompt/README.md`.

#### `clarification_answers_truncated` — the residual truncation is audited (migration 0086)

`CategoryClarificationAnswersTruncated = "clarification_answers_truncated"` (registered in `audit.KnownCategories` per the #3410 obligation) is emitted by `appendClarificationAnswersTruncatedAudit`, called from `loadClarificationAnswers` when and only when its `CapText` actually cut. Payload: `{original_bytes, cap_bytes, dropped_bytes, source:"clarification_answered", source_entry_id}`. Best-effort — a WARN-logged append failure never blocks prompt construction.

`source_entry_id` is the id of the `clarification_answered` entry whose blob was cut, and it is the key of **migration 0086**'s partial unique index `audit_entries_clarification_answers_truncated_once_idx` — `(run_id, (payload->>'source_entry_id'))` `WHERE category = 'clarification_answers_truncated'`. **Why that key and not `run_id` alone:** prompt construction for a stage repeats (retries, prompt-render fetches), so an unkeyed append would report ONE truncation as five; but a run can also carry two genuinely DISTINCT over-cap blobs across a re-park-and-re-answer cycle, and each must still report once. A `run_id`-only key would suppress the second — the same class of audit lie in the other direction. The partial-on-category predicate leaves every other category unconstrained, and NULL-distinct semantics (`NULLS NOT DISTINCT` is opt-in and not used) mean a key-less row cannot collide, so the `CREATE` cannot fail loud on legacy data. A collision is the benign already-recorded outcome, recognized by `audit.IsClarificationAnswersTruncatedDuplicate` — scoped to THAT index only, so an unrelated 23505 (entry-hash, `(run_id, sequence)`) stays a hard error — and logged at INFO, not WARN. Exactly mirrors `approval_conditions_truncated` / migration 0068 (#2583/#2622).

### Plan-gate revise: the binding constraint is refused over-cap, never truncated (`revise.go`, #2871)

`handleRevisePlan`'s `constraint` is BINDING in exactly the sense an approve-with-conditions comment is (#558: it AMENDS the plan and wins on conflict), so it takes the same **refuse-on-binding** posture that `approvals.go::validateApprovalComment` established for #2583, not the advisory warn-on-reject one.

- **`validateReviseConstraint`** refuses `len(trimmed) > prompt.MaxRevisionConstraintBytes` (12000) with `400 validation_failed`, naming the actual byte count, the cap and the overflow, plus `details{field, bytes, max_bytes, overflow_bytes}`. BYTES, not characters — matching the renderer's byte-based cut.
- **Ordering is the whole guarantee.** The check sits immediately after the empty-constraint 400 and BEFORE every stateful step: the `GetStage` read, the subject-binding guard, `countRevisePasses` / `countRegressedRevisePasses`, and above all `run.RevisePlanStage` with its `OnAdmit` `plan_revised` append. So a refused call consumes NO revise pass, writes NO audit entry, and leaves the stage at `awaiting_approval`. An error identity alone cannot distinguish that from "appended then refused", which is why `TestRevisePlan_OverCapConstraint_ConsumesNoPass` reads the committed state after the call and then drives a successful follow-up revise on the still-unspent budget.
- **What the old behaviour cost.** `revise.go` previously did a bare `constraint = constraint[:4000]` — a rune-unaware byte slice — and returned 200. Because `writeReviseAudit` then stored the ALREADY-TRUNCATED text, the hash-chained `plan_revised` record corroborated the shortened version, so unlike #2583 (where the audit record stayed intact and the loss was detectable afterwards) a later reader could not detect the loss at all. Two observed live losses: run `bff9a242` dropped conditions C5-C7 of seven, and the #2062 drive lost a BINDING item for four consecutive rounds and it was misattributed to agent non-compliance.
- **One owner for the cap.** `prompt.go::loadRevisionConstraint` no longer calls `CapText`: it returns the stored blob RAW (a second cap would cut at the LOWER of two, silently shortening a constraint the gate accepted) and WARN-logs — not INFO — an over-cap legacy read. The renderer (`prompt.writeOperatorConstraint`) is the sole eliding site, and with the gate in place its `CapTextWithRetrieval` path is reachable only for a constraint persisted BEFORE the gate shipped. This mirrors the loader/renderer split `loadPriorRejectionFeedback` already uses.
- The rendered constraint is terminated by `prompt.RevisionConstraintEndMarker`; the expectation that it will be is written into the prompt scaffolding BEFORE the constraint, so a cut removing the terminator cannot also remove the instruction to notice it. Contract: `backend/internal/prompt/README.md`.

#### The revision-base elision is reported at revise time (`revise.go`, #3442)

The prior plan rides into the re-dispatched prompt as the revision base, whole under `prompt.MaxRevisionBasePlanBytes` (60000) and as a step-complete digest above it (#3087). Nothing told the OPERATOR that the digest fired — run `8b911565` learned of a 63761-byte digest only from the revised plan's `risks_and_assumptions`. Now:

- **Measurement placement.** `handleRevisePlan` calls `s.assessRevisionBase(ctx, stage)` AFTER the subject-binding guard and BEFORE the pass counting and `run.RevisePlanStage`. It measures `s.loadRevisionBasePlan` — the `json.MarshalIndent` of the typed plan, the identical string the prompt path renders — through `prompt.AssessRevisionBase`, so the response, the audit row and the prompt's digest header are ONE measurement (`TestRevisePlan_CrossLayer_OverCapBase_PromptAndSurfaceAgree` fails if either side ever diverges). Read-only and best-effort: `loadRevisionBasePlan` returns nil on a missing/unreadable artifact and the revise proceeds with the key absent.
- **Surfaces.** The 200 body is `reviseResponse` (`stageResponse` embedded + `revision_base,omitempty`, mirroring `fixupResponse`) — byte-identical to the pre-#3442 Stage when no base is loadable (`TestRevisePlan_NoBase_OmitsRevisionBase`). `writeReviseAudit` adds the same object to the `plan_revised` payload under `revision_base` when non-nil, inside the `OnAdmit` hook, so the durable record carries the fact. An elided base WARN-logs (`revise: <prompt.RevisionBaseElisionSummary>` with `run_id`, `stage_id`, `original_bytes`, `cap_bytes`, `mode`, `rendered_bytes`, `elided_bytes`); a whole base INFO-logs.
- **Wording agrees with mode.** The WARN sentence claims a "step-complete digest" only for `mode` `digest`/`digest_shrunk`; a `cut` base (an artifact that did not decode as a plan carrying approach steps) says "byte-cut prefix … NOT a digest" — `TestRevisePlan_CutModeBase_WordingSaysCutNotDigest` pins it on a zero-step base.
- No refusal, no cap change: the operator cannot shrink a prior plan, so a refuse-then-acknowledge dance would be ceremony on every large-plan revise. Should refusal be wanted later, the assessment is already computed ahead of every stateful step, so a 409 gated on a request flag slots in before `RevisePlanStage`.

### Plan-gate scope-regression sweep (`scope_regression.go`, #1257)

`backend/internal/server/scope_regression.go::runScopeRegression` — called from `handleShipPlan` (`plan.go`) immediately after `runTestSweep` and before `runPlanReviews`, but ONLY on a **revise pass**.
`fishhawk_revise_plan` regenerates the WHOLE plan artifact, so a narrowly-scoped revision constraint can silently DROP files the immediately-prior (revision-base) plan scoped — even one that says "keep everything else" — and the runner then scope_drift-excludes the agent's edits to them (#1257).

- The gate's run-guard is a prior `plan_revised` audit entry (`countRevisePasses > 0`, the same durable revise-pass record the bound is counted against); the revision **base** is captured via `loadApprovedPlanForRun` **BEFORE** `ArtifactRepo.Create` (an after-Create capture would diff the just-shipped plan against itself and report no regression).
- `scopedPaths(*plan.Plan)` is the pure helper: the slash-normalized (`filepath.ToSlash`), sorted, de-duplicated UNION of `plan.Scope.Files[].Path`, every `plan.Decomposition.SubPlans[].Scope.Files[].Path`, AND every `plan.SplitProposal.Phases[].Scope.Files[].Path`.
  The observed regression dropped files living in decomposition sub-plan slice scopes, so the diff MUST cover sub-plan scopes, not only the flat top-level list. **Split-phase scopes joined the union with the refusal (#2516)**: under an advisory, reporting a file *moved into* a split phase as dropped was a tolerable false positive; under a refusal it would refuse a revise that merely rearranged the same file set. This now matches the sibling `planGateScopePaths`, which already unioned all three.
- `runScopeRegression` computes `removed = base-scoped − new-scoped` (every dropped path) and `added = new − base`, then splits `removed` by the new plan's top-level `scope_removals` declarations into `declared_removals` and `undeclared_removals` (paths slash-normalized on both sides; an entry naming a path that was NOT dropped is a harmless no-op, exactly as an unmatched `surface_sweep_exemption` is). It writes a `plan_scope_regression` audit entry (payload `ScopeRegressionPayload{removed_files, declared_removals, undeclared_removals, added_files, scanned_files, regressed}`, all four lists empty arrays not null on a clean diff).
- **`Regressed` is `len(undeclared_removals) > 0`, NOT `len(removed_files) > 0` (#2516).** `removed_files` keeps its prior meaning, so no existing reader breaks, but the bit the refusal AND the budget refund key on now excludes a declared drop: a declared narrowing neither refuses nor refunds, because it is not a mistake. (This is a behaviour change to the existing refund mechanism, not purely additive.)
- Advisory + fail-open: `base==nil` (non-revise ship), a nil `AuditRepo`, a parse failure, OR an audit-append failure WARN-logs and returns nil — never blocks or unwinds the ship. **The REFUSAL built on top of it lives in the caller** (`plan.go::tryScopeRetry`), documented below.
- The returned payload threads into the plan-review prompt's gate-evidence section as a HIGH-severity signal (`prompt.ScopeRegressionEvidence`, rendered only when `RemovedFiles` is non-empty) so BOTH reviewers and the operator see the drop before approving.
- **Budget refund seam**: `backend/internal/server/revise.go::countRegressedRevisePasses` counts the stage's `plan_scope_regression` entries with `regressed==true`.
  `handleRevisePlan` sets `run.ReviseOptions.BudgetPassCount = max(0, priorPasses − regressedPasses)` so a regressing revise pass refunds the NORMAL revise budget (the operator gets a free recovery pass), while `PriorPassCount` keeps governing the HARD CEILING (`defaultReviseCeiling` = 3) so the refund cannot create an unbounded revise loop — total work stays bounded.
  `RevisePlanStage` compares `BudgetPassCount` against `MaxPasses` for `ErrReviseBudgetExhausted` and derives `remaining`/`forced` from it, leaving the ceiling check on `PriorPassCount`.

#### Undeclared-narrowing REFUSAL (`plan.go::tryScopeRetry`, #2516)

The sweep above is advisory: it records the drop, but the narrowed plan still walks to the gate. That spends a **reviewer** pass and an **operator revise** pass on a plan whose only remedy is another revise that can drop again — the pain #2516 describes. `handleShipPlan` therefore calls `tryScopeRetry` immediately after `runScopeRegression` and **before** the over-cap gate, when `regression != nil && regression.Regressed`, and on `true` sets the existing `gatingRejected` flag (reusing the advancement suppression, so the refused stage never walks to `awaiting_approval` and no plan-ready ping fires). The artifact is stored and audited before every gate, so the operator can still inspect the refused plan via `fishhawk_get_plan` — matching the over-cap reject's documented behaviour.

- **Budget**: `maxPlanScopeRetries = 1`, enforced by the shared `consumeRetryBudget` helper against the run's `plan_scope_retry` audit entries. It **fails closed**: on budget exhaustion OR any check-and-consume error it declines the refusal (an unreadable/unwritable budget is an *unbounded* one — a persistent audit failure would otherwise grant a refusal on every corrective ship and the one-shot would never exhaust), and the plan falls through to park-with-evidence with the refunded revise pass. The check-and-consume is now **atomic under concurrency (#2518)**: under the production repo it counts under a `SELECT … FOR UPDATE` row lock and appends in ONE transaction (`audit.AppendChainedUnderBudget`; lock-then-count-then-append, READ COMMITTED), so two racing ships cannot both read `0` and both refuse — the one-shot is strictly one-shot. This supersedes the earlier over-consume-but-`>=`-bounds argument and the "would need a unique constraint on `(run_id, category)`" note: no index over the append-only, hash-chained table is used, because its race-produced duplicates cannot be de-duplicated without breaking the chain the verifier depends on. The reachability invariant justifying this — that concurrent same-stage ships are genuinely reachable — is the "Plan-ship concurrency" note below.
- **The `FailStage` reason is capped** at `maxSchemaValidationErrorBytes` (4000), like the sibling schema-retry path's `validation_error`: nothing bounds how many paths a revise can drop, and a stage failure reason is surfaced back to agents and operators.
- **Payload keys** on the `plan_scope_retry` entry: `{run_id, stage_id, attempt, undeclared_removals, required_scope_files}`. `required_scope_files` is the base-scoped union **MINUS** the declared removals — derived SERVER-SIDE from the revision base, never asserted by the planner — i.e. the exact enumerated set the corrected plan must cover. The entry is BOTH the budget counter and the prompt's feedback source.
- **Ordering is consume-first**, mirroring `trySchemaRetry` and `handleRetryStage` so the retry intent is durable even if a later step fails: `consumeRetryBudget` (which commits the `plan_scope_retry` entry atomically) → `FailStage(FailureA)` → `RetryStage(pending)` → `Orchestrator.Advance`. The transient category A never leaks into the run's terminal state. A racer that finds the stage already re-opened to `pending` is absorbed by `failStageCAS`'s bounded RE-ANCHOR loop (`run/failure.go`), which re-fails from any live, legally-failable state — so the budget primitive, NOT the stage CAS, is what bounds the refusal to one (an earlier comment claiming "at most one racer's re-open wins" overstated the CAS's role and was corrected).
- **Return contract, per leg** (each leaves a *different* observable state, and each has its own behavioural test):

  | Leg | Returns | Committed state after |
  |---|---|---|
  | nil `Orchestrator` / nil `AuditRepo` / budget exhausted / atomic consume error / fallback read error | `false` | Nothing NEW mutated; caller's fall-through parks the stage at `awaiting_approval` with the evidence |
  | `FailStage` error | `false` | The entry IS committed and the budget IS consumed, but the stage never left its state → fall-through parks it at `awaiting_approval` |
  | `RetryStage` error | `false` | The entry is committed AND the stage is left at `failed` (transient category A, operator-recoverable via `retry_stage`). It does NOT reach `awaiting_approval` |
  | `Advance` error | **`true`** (log-and-continue) | The stage is already re-opened to `pending`; returning `false` would fall through to normal advancement holding a pending stage — the stranded state. The refusal stands; only the auto-dispatch is missing, which on the local runner is the normal case anyway (ADR-024) |

- **Exhaustion degrades, it never fails terminally.** With the budget spent the gate falls through UNCHANGED to today's behaviour: the plan proceeds to review carrying the regression evidence, and `revise.go`'s refund gives the operator their pass back. Contrast the **over-cap reject**, which IS terminal (`plan_review_failed` + category-B). A refusal must never become a new way to lose a plan.
- The over-cap gate is skipped when the refusal already fired: a stage is failed once, and layering a terminal category-B reject onto a stage already re-opened to `pending` would destroy the recoverable refusal.
- **Carry-forward prompt channel** (`prompt.go::loadScopeCarryForward`, populated at BOTH plan-prompt build sites): resolution order is load-bearing — the newest `plan_scope_retry` entry **for the stage** wins (its `required_scope_files` is the authoritative carry-forward set, its `undeclared_removals` the refusal notice); only when none exists does it fall back to deriving the set from the newest plan artifact through `scopedPaths`. On a corrective re-dispatch the newest plan artifact is the REFUSED narrowed plan, so deriving carry-forward from it would cement the very drop the retry exists to undo. Best-effort/warn-only: an audit read error, an undecodable payload, or a missing artifact yields `(nil, nil)` and the prompt renders as it does today. The first two legs return `(nil, nil)` *without* trying the artifact fallback — both mean "a refusal may be recorded and I cannot read it", and falling back would render the REFUSED narrowed plan's scope under the authoritative heading, cementing the drop; rendering nothing is strictly better. The artifact fallback itself does not test for a revise, so the "first-pass plan prompts stay byte-unchanged" guarantee lives at the RENDERER (`buildPlan` writes the list only inside its `RevisionConstraint != nil` block, and a first-pass dispatch records no `plan_revised`) — pinned by `TestGetStagePrompt_Plan_NonRevise_NoCarryForwardSection`.
- **Path rendering is sanitized** (`prompt.go::sanitizeScopePath`): the paths are server-derived from the machine diff, but their CONTENT is planner-authored and lands inside Fishhawk's own binding prompt sections. Every line terminator and control character is escaped to a visible two-character form and fences are broken, so a crafted path cannot end its `- ` list item and land attacker-chosen text at column 0 where it could impersonate a trusted banner. An ordinary path passes through byte-unchanged.
- The renderer (`prompt.Trigger.RevisionBaseScopeFiles` / `ScopeRestoration`) enumerates the carry-forward set inside the **Revision constraint** section, right after the base-plan blob, and states plainly that the list — not the blob — is authoritative. **That claim no longer rests on the blob being truncated ([#3087](https://github.com/kuhlman-labs/fishhawk/issues/3087)).** When #2516 shipped, the base was cut at 4000 bytes with a bare marker, so on a 40+ file plan a planner told to "not discard the parts the constraint does not touch" could not see the scope it was asked to preserve. The base now rides WHOLE under `prompt.MaxRevisionBasePlanBytes` (60000 — above realistic plan size, so a real revise inlines the entire prior plan) and, above that cap, as a step-complete digest with an elision manifest; the lead sentence of this block is correspondingly CONDITIONAL on whether the blob was actually elided, so the prompt no longer asserts a truncation that did not happen. The enumeration remains authoritative for an INDEPENDENT reason: it is resolved from the newest `plan_scope_retry` entry, which on a corrective re-dispatch is NOT the newest plan artifact, so it supersedes the blob whether or not the blob is complete.

#### Plan-ship concurrency (why the budget window needs its own atomicity, #2518)

`handleShipPlan` is a plain authenticated HTTP handler with NO per-stage serialization ahead of the budget window: the only per-run lock in the path is `AppendChainedTx`'s `SELECT … FOR UPDATE`, taken INSIDE the append, i.e. AFTER the count. So two concurrent executions of the same `(run, stage)` can both enter the count→append window. This is reachable, narrowly: the runner's `upload.Client` is built with `http.Client{Timeout: 120s}` (`runner/internal/upload/upload.go`), and `ShipPlan` treats any HTTP `Do` error — including that client-side timeout — as transient and RE-POSTs the identical signed body, so a server-side stall past 120s puts two in-flight executions of the same invalid body into the window.

What DOES serialize same-stage ships is the stage-state CAS (`advancePlanStageTerminal` / the re-open transitions), but it sits AFTER the budget window, so it cannot bound a count taken before it. What does NOT serialize them is the runner-side lineage lock (`runner/cmd/fishhawk-runner/lockholder.go`, `worktree.go`; surfaced as `runner_failed/lineage_lock`): it is a FILE lock over the local checkout's lineage root that serializes runner PROCESSES on one local runner only — it is absent on the `github_actions` path and blind to a single runner's retried in-flight HTTP request, and nothing in `plan.go` consults it. Therefore the budget window needs its own atomicity, which is `audit.AppendChainedUnderBudget`'s lock-then-count-then-append (above). The cross-layer proof is `TestE2E_Revise_ConcurrentScopeRetryShips_GrantsExactlyOneRefusal` (two concurrent signed ships → exactly one `plan_scope_retry` entry, at most one re-dispatch).

#### Generated-surface REFUSAL (`generated_surface_gate.go::tryGeneratedSurfaceRetry`, #3437)

The `plan_test_sweep` sweep already flags a `generated_surface` miss — a canonical source (a `docs/spec/*.schema.json`, `docs/api/v0.openapi.yaml`, `cli/internal/cmdinfo/cmdinfo.go`) scoped without every DERIVED file it generates (a byte-exact site Reference region, an embedded schema/preset/fixture mirror) — but as an ADVISORY, so the plan walked to the gate and **both plan reviewers rejected/flagged it every time** (#3437, the deterministic dual-reject class the #2516 scope refusal above closes for undeclared narrowing). This gate REFUSES it instead, mirroring `tryScopeRetry` leg for leg. `handleShipPlan` decodes the plan ONCE (the `parsedForCap` decode, hoisted above this gate) and, when `capDecoded && !gatingRejected`, calls `evaluateGeneratedSurfaceGate`; on a non-empty result it calls `tryGeneratedSurfaceRetry`, whose `true` sets `gatingRejected` (reusing the advancement suppression, so the refused stage never reaches `awaiting_approval` and no plan-ready ping fires). Placed BETWEEN the #2516 scope refusal and the over-cap reject; skipped when the scope refusal already re-opened the stage (a stage is failed once). Zero reviewer passes and zero operator `add_scope_files` grants are spent on the deterministic miss.

- **Deterministic for the unexempted, budget-available case; every fall-through leg emits a NAMED log line so an admitted-with-finding plan is diagnosable.** The three admitted shapes are the criterion's stated exceptions: **(1) exempted** — a `surface_sweep_exemptions` entry whose `(pattern, sibling)` equals `(generator, derived path)` is subtracted inside `evaluateGeneratedSurfaceGate` (the escape hatch for an edit that provably leaves the derivative byte-identical; scoping a file the implement pass never touches would otherwise park the scope-completeness gate), so a fully-exempted finding never reaches `tryGeneratedSurfaceRetry`; **(2) budget exhausted** — `consumeRetryBudget` declines (logged by the helper); **(3) dependency nil** — a nil `Orchestrator`/`AuditRepo` declines with its own `Info` log. A partial exemption keeps the remaining derivatives.
- **Independent of `runTestSweep`.** `evaluateGeneratedSurfaceGate` re-derives the miss from the SAME `testSweepPathTriggerRules` table via the pure `evaluatePathTriggerRules`, over the flat scope AND every decomposition sub-plan scope, consulting NO `dirListings` and NO GitHub client — so the refusal fires even when `runTestSweep` failed open (no GitHub client, no installation, failed listings), which the advisory could not.
- **Budget**: `maxPlanGeneratedSurfaceRetries = 1`, via the shared `consumeRetryBudget` against the run's `plan_generated_surface_retry` audit entries (its own category — a separate one-shot from the schema and scope budgets). Fails closed on exhaustion or any consume error exactly like `tryScopeRetry`, and exhaustion degrades to today's advisory review path (the plan carries the `plan_test_sweep` evidence), never a terminal fail.
- **Payload keys** on the `plan_generated_surface_retry` entry: `{run_id, stage_id, attempt, findings:[{trigger_path, missing_tests, generator, sub_plan_title}], required_scope_files}` — `required_scope_files` the sorted union of every missing derivative. The entry is BOTH the budget counter and the feedback source `loadGeneratedSurfaceRestoration` reads back into the corrective re-dispatch's binding `### Generated-surface scope restoration` prompt section, which also renders the REFUSED plan (via `loadApprovedPlanForRun`, whose newest artifact on the re-dispatch IS the refused plan) so the planner re-emits it with the derivatives added.
- **Ordering is consume-first** and the per-leg return contract is identical to `tryScopeRetry`'s table above (consume decline / `FailStage` error / `RetryStage` error → `false`; `Advance` error → **`true`**). The reason is capped at `maxSchemaValidationErrorBytes`.
- **Residual (parity with the test sweep):** `split_proposal` phase scopes are NOT evaluated — only the flat scope and decomposition sub-plans.

## Scope-completeness exempt emission gate (#2501)

`prompt.go::resolveHeldCommitExemption` decides whether an implement-stage prompt response carries the four zero-re-run fields — `open_pr_from_held_commit`, `held_commit_sha`, `held_commit_branch`, `held_commit_base_sha` — that make the runner skip the agent, the committed-tree gates, and the commit/push entirely and open the PR from the commit a scope-completeness park already held on the run branch.

- **Newest-entry-wins over a DECISION-plus-INVALIDATOR set.** It resolves the NEWEST audit entry for the stage across `scopeCompletenessDecisionInvalidatorCategories`, comparing by `Entry.Sequence` and filtering on `entry.StageID == stage.ID`. That set is the three `scope_completeness_*` decision carriers (`_parked` / `_exempted` / `_failed`) PLUS the three lineage INVALIDATORS `pull_request_opened` / `fixup_pushed` / `child_pushed`, mirroring `pushCheckpointCategories`. Only an `exempted` newest entry emits. A stage that RE-parked after an earlier exempt emits nothing (the later `parked` entry wins), and — because the invalidators are in the walk (#2630) — a stage whose exempt was SUPERSEDED by an opened PR, a landed fix-up push, or a child push also emits nothing, so a later dispatch takes the ordinary agent path instead of re-entering the completed PR-open short-circuit (the #2630 sticky re-entry). The `newestParked` walk stays restricted to `scope_completeness_parked`, so widening the decision set does NOT change which entry supplies the #2563 base-SHA fallback.
- **Fix-up refusal (#2630).** A fix-up dispatch (`fixup == true`, passed from both call sites) returns `ok=false` before the audit walk, mirroring the sibling `resolvePushCheckpointResume`. A fix-up carries a prompt the runner MUST run; emitting the held-commit fields would send it to its pre-agent `openHeldCommitPR` short-circuit and discard the fix — the exact trigger that stranded implement stage 266b8cf8 in `running` with no verb short of `cancel_run`.
- **Base-SHA source order (#2563).** The fourth field is the base commit the held commit was built on — the value the runner ships as the PR artifact's `base_sha`, which the backend's success-arm `validate()` REQUIRES (without it the exempt resume opens the PR and then always fails category-B, orphaning it — the #2562 bug). It resolves from (a) `park.BaseSHA` (persisted on every park since #2563), else (b) the `base_sha` of the NEWEST `scope_completeness_parked` audit payload for the stage — read from the same category walk the gate already performs. The fallback is what makes an ALREADY-parked legacy row (e.g. #2169's, whose park predates the field) resumable instead of re-failing identically after a `retry_stage`.
- **Fail-closed.** This gate is the only thing standing between the widened `awaiting_scope_decision → pending` transition edge (which any caller can drive) and a PR opened from a commit no operator accepted. So every uncertain branch WARN-logs and emits nothing: a fix-up dispatch (above), a nil `AuditRepo`, any `ListForRunByCategory` error, no entry for the stage, a newest entry that is not `exempted` (a re-park OR a superseding invalidator), a non-implement stage, a stage with no `ScopeCompletenessPark`, a park whose `HeldCommitSHA`/`RunBranch` is empty, an undecodable parked-audit payload, AND a park for which no non-empty base SHA resolves from either source. The four exempt fields are emitted together or all withheld — a missing base SHA withholds the held-commit fields too, so the runner never opens a PR it cannot ship.
- **Both handlers.** Wired into `handleGetStagePrompt` (the runner-facing dispatch path) AND `handleGetStagePromptRender` (the SPA preview), per that handler's byte-consistency convention.
- **The decision handler writes the proof before it opens the door.** Because the gate reads the audit chain, `scope_completeness.go::exemptScopeCompleteness` appends the `scope_completeness_exempted` entry BEFORE transitioning the stage out of `awaiting_scope_decision` (and therefore before the `Orchestrator.Advance` that can spawn a runner): a runner whose prompt fetch raced the append would read the older `parked` entry, get no held-commit fields, and re-invoke the agent — the full re-run the park exists to avoid. For the same reason the append is BLOCKING there: a failure returns 500 `exemption_unrecorded` and leaves the stage parked (re-POST the decision) rather than dispatching a stage whose exemption cannot be proven. `failScopeCompleteness` keeps its best-effort append — nothing reads the `failed` entry as authorization and its transition is terminal.
- **An `amend` DEMOTES a stale `exempted` (#2591).** `CategoryScopeCompletenessAmended` is in `scopeCompletenessDecisionInvalidatorCategories` for the same reason the three lineage invalidators are: an amend means "do NOT ship this tree, re-run against a wider scope", so an amend following an earlier exempt on the SAME stage must not leave `exempted` newest. Without it the gate re-emits all four held-commit fields on the amend's re-dispatch, the runner takes its pre-agent `openHeldCommitPR` short-circuit, and the widened re-run is discarded — the #2630 sticky re-entry shape, on a new trigger.
- The four json tags are the byte-identical cross-module wire contract with `runner/internal/upload/upload.go`'s `FetchedPrompt` decoder; a SINGLE shared fixture, `testdata/wire/exempt_prompt_fields.json`, read by BOTH modules' tests (via the `goldenExemptPromptJSON(t)` helpers) is the prompt-response seam — one source of truth, so a backend tag change forces a fixture edit that then reddens the runner decode, the load-bearing property the two prior duplicated consts claimed and lacked (#2558). The struct-level tag parity is additionally pinned by `backend/internal/wirecontract` (`TestCrossModuleWireParity`). The PR artifact the runner ships from the held commit has its OWN cross-module seam: `testdata/wire/held_commit_pr_artifact.json` is ONE file the runner test asserts its output equals and the backend test POSTs through the real handler — so a runner field the backend rejects fails at test time. #2558's remaining half — the shared wire MODULE that would make these compile-enforced — is deferred (see `backend/internal/wirecontract/README.md`). Both seams exist because the runner and the backend are separate Go modules and no single-process end-to-end can span them.

## Build-required scope-drift park — the third shortfall class (E48.101 / #2548)

A scope-completeness park carries exactly ONE of three shortfall classes. The first two — missing declared scope files (#1151) and unsatisfied binding assertions (#1171/#2501) — are standalone-open-PR-push only and EXEMPT-ELIGIBLE. The third is not.

- **What it is.** A decomposition slice whose scope-only committed tree COMPILES but whose tests are red because a build-required file is owned by a SIBLING slice — the boundary cut a coupling (#2548's live case: slice 0 needed `backend/internal/server/prompt.go`, owned by slice 1). Before this, that failed category-B at commit+push, after the agent had exited, and the whole pass was discarded. Now the gate-verified commit is pushed to the child's own sole-writer slice branch, the child implement stage parks in `awaiting_scope_decision`, and the park names the build-required paths AND the owning sibling slice for each (`owning_slices`, resolved from the parent plan's `decomposition.sub_plans[i].scope.files` — the same authority `resolveSliceDependencies` reads).
- **Scoped to fan-out children.** The runner sets `ParkOnBuildRequiredDrift = isDecomposed && !isFixup`. A standalone run's build-required failure keeps today's category-B abort and its `fishhawk_resume_run --add_scope_files` in-band recovery (which a child has no equivalent of — its coupled file belongs to a sibling); a fix-up pass is byte-for-byte unchanged. A COMPILE failure and a test failure with EMPTY drift stay non-parkable on every path.
- **What it delivers: PARK-AND-PRESERVE WITH ATTRIBUTION.** The slice's work survives on `fishhawk/run-<parent>/slice-<n>` and the operator is told which sibling slice owns the file that broke the boundary. **Since #2591 the admissible decisions are `amend` and `fail`** (see the amend section below) — `amend` widens THIS child's stage scope with the coupled paths and resumes it, guarded so the single-owner-file invariant is only relaxed where it can be shown safe; `fail` keeps the original "correct the decomposition and re-run" recovery.
- **`exempt` is REFUSED (409 `exempt_refused_build_required`).** `scope_completeness.go::exemptScopeCompleteness` returns before it does anything — before the load-bearing `scope_completeness_exempted` append, before the stage leaves `awaiting_scope_decision`, before any `Advance`. Exempt resumes the stage and drives a PR-open from the held commit, whose committed tree is RED BY CONSTRUCTION for this class (that redness IS the shortfall), and opening a PR from it is the one outcome this design says must never happen. The refusal leaves the stage parked and appends no exempted entry, so the operator can still resolve it with `fail`.
- **Defence in depth.** `prompt.go::resolveHeldCommitExemption` independently withholds all four held-commit fields for any park carrying non-empty `BuildRequiredPaths`, even when an `exempted` entry is somehow newest for the stage. The endpoint refusal is the primary control; this gate is the last thing between a hand-written, legacy, or otherwise anomalous exempted entry and a PR opened from a red tree.
- **The parent side.** A parked child is settled but NOT terminal, so the decomposition parent stays in `awaiting_children`. That wait is correct but must not be silent: a `parent_awaiting_child_scope_decision` entry is emitted on the PARENT, from BOTH `emitParentAwaitingChildScopeDecision` at park time and `orchestrator.maybeAdvanceDecomposedParent` when a sibling settles. At most one entry exists per `(parent run, child stage)`, enforced at the store layer by migration 0067's partial unique index on `(run_id, payload->>'child_stage_id')` — not by a best-effort read in one emitter; the race-loser's `AppendChained` hits the index and both emitters treat that specific collision as the benign already-recorded outcome (INFO, park untouched) via `audit.IsParentAwaitingChildScopeDecisionDuplicate`. See `backend/internal/orchestrator/README.md` for why the emission has to be dual, and `backend/internal/audit/README.md` (0067 / #2594) for the index and the #2591 no-episode-component caveat.

> **ROLLBACK PRECONDITION — resolve every outstanding build-required park with `fail` BEFORE reverting #2548.** A revert removes the `build_required_paths` field AND both safeguards (the endpoint refusal and the prompt withholding gate). An already-parked build-required row would then decode with the key dropped, look like an ordinary park, and become EXEMPTIBLE — an ordinary operator `exempt` would set the held-commit emission fields and the runner would open a PR from a commit whose committed tree is red, which is precisely the outcome this design forbids. This is a narrow revert-window hazard and the resolution is operational, not structural: drain the parks first. Skipping this step does not corrupt data or break the revert; it leaves a live, reachable path to a red-tree PR for as long as any such park remains parked. To enumerate them, look for implement stages in `awaiting_scope_decision` whose `scope_completeness_park` JSONB carries a non-empty `build_required_paths`.

## Amend-and-resume a scope-completeness park (`scope_completeness_amend.go`, E67.18 / [#2591](https://github.com/kuhlman-labs/fishhawk/issues/2591))

`POST /v0/runs/{run_id}/scope-completeness/decision` takes a THIRD decision, `amend`: the operator names the paths the parked implement stage actually needed, they are folded into that stage's effective scope, and the stage RESUMES so the agent re-runs against the wider scope. It exists because the build-required class (#2548) had only `fail` — its `exempt` is refused since its held tree is red by construction — so a boundary miscut cost a whole pass and a re-run of the entire run.

**The four design questions the issue deferred, and how they are settled here.**

- **Whose scope widens.** The parked CHILD's own implement stage, never the parent's approved decomposition — an approved plan artifact is immutable. The widening is a stage-scoped, pre-approved #961 scope-amendment row, so it lasts for the remainder of this child's implement pass and nothing else.
- **The held commit.** REUSED, never discarded: `amendScopeCompleteness` does not reset, delete or rewrite `stage.ScopeCompletenessPark`, so the slice's work stays on its sole-writer run branch (ADR-035). The park row also remains as the durable record of what was parked; the prompt-gate invalidator above (not deletion of the park) is what stops it being read as a live exemption.
- **Re-run, not re-verify.** `gitops.StageScoped` strips the out-of-scope edits from the commit BEFORE the committed-tree gate runs — that stripping IS what produces the build-required shortfall — so the coupled file's content survives in no durable artifact the backend could re-verify. Making re-verify possible needs the runner to persist the stripped drift as a patch at park time; left to a follow-up.
- **Relationship to #961.** `amend` reuses that channel outright: the same `scopeamendment.ValidatePaths`, the same `{path, operation}` vocabulary, the same create-then-auto-approve row `recover.go::createApprovedScopeAmendment` mints, and therefore the same `prompt.go` fold (`mergeApprovedScopeAmendments` → `resolveApprovedScopeAmendmentEntries` → `foldScopeEntries`), which filters on `Status == approved` with no origin discriminator.

**The single-owner-file invariant is relaxed only where that can be SHOWN safe.** An amend hands a second slice permission to edit a file a sibling owns. The guard resolves each submitted path against the park's persisted `owning_slices` attribution (#2548) and inspects the owning sibling's implement stage:

| Owner state | Outcome |
|---|---|
| Run is not a decomposition child (`DecomposedFrom == nil`) | Resolved BY CONSTRUCTION — no sibling slice exists, so none can hold a divergent edit. No acknowledgement needed. |
| Path owned by THIS run's own slice | Resolved — a slice cannot conflict with itself. |
| Owner resolved, its implement stage NOT started | Permitted; the owning slice is named on the response and the audit entry so the operator knows to drop the file from it or expect a merge. |
| Owner resolved, its implement stage STARTED (`StartedAt != nil`) or succeeded | **409 `amend_refused_owner_slice_active`** — from that moment two branches can carry divergent edits to one file. |
| Ownership UNRESOLVED (no attribution entry, nil slice index, sibling lookup failed, sibling run/implement stage absent, sibling stages unreadable) | **409 `amend_owner_attribution_unresolved` — FAILS CLOSED.** Unknown ownership is exactly the case in which a sibling might already hold divergent edits, so recording the risk is not preventing it. `acknowledge_owner_unresolved: true` re-admits it and the acknowledgement plus the unresolved path list land on BOTH the response and the `scope_completeness_amended` audit entry — an audited operator decision, never a silent default. It does NOT relax the started-owner refusal. The flag is `amend`-ONLY: supplied with `exempt` or `fail` it is refused 400 `validation_failed` at BOTH seams (the handler and the `fishhawk_decide_scope_completeness` tool's pre-HTTP check), because only the amend arm runs the guard — forwarding it elsewhere would leave the operator believing an unrecorded risk decision had been audited. |

`Stage.StartedAt` is the sound proxy because `stages.started_at` is written under `COALESCE(started_at, $3)` and never overwritten (see `scope_amendment.go::amendmentDeadlineRemaining`); its documented cumulative-on-respawn caveat can only make the guard MORE conservative.

**Ordering is load-bearing** — `validate paths → coverage → in-flight row lookup → amend cap → owner guard → row → audit entry → resume → advance`. Everything up to the owner guard is a no-side-effect refusal (409/400/500: stage parked, no row, no audit entry) — the in-flight lookup is a READ. Coverage refuses an amend that omits any `build_required_path` (409 `amend_incomplete_coverage`), because a re-run under a scope that still excludes one re-parks identically and burns a full agent pass. The audit append is BLOCKING for the same reason as the exempt arm: that entry is what demotes a stale `exempted`, so a stage whose widening the chain never recorded must never become dispatchable. The resume is to `pending`, not `running` — `host_dispatch.go`'s admission switch accepts only `{pending, awaiting_host_dispatch}` (#2501).

**RETRY-SAFE, not atomic.** The row, the audit entry and the transition are three writes with no shared transaction, so a failure at either of the last two leaves an orphaned APPROVED row. That orphan is inert for the effective scope (`foldScopeEntries` dedupes by path) but NOT for the budget, which counts ROWS via `ScopeAmendmentRepo.CountByStage`. So the amend is IDEMPOTENT BY REUSE: `findOperatorAmendRow` recognizes its own orphan by `(stage, marker reason, identical path set)` — exactly what a re-POST of the same decision presents — and `resolveOrCreateOperatorAmendRow` reuses it, completing a PENDING one (`Create` landed, the auto-approve `Decide` did not) rather than duplicating. `ListByRun` failing REFUSES the amend (500 `amend_unrecorded`) rather than guessing, because guessing wrong drains the agent's budget silently. Correspondingly the per-stage amend cap counts DISTINCT `amendment_id`s across the stage's `scope_completeness_amended` entries, not raw entries, so a retry does not spend a second slot.

Two boundary conditions make that reuse safe rather than a cap bypass:

- **The reuse key carries a PARK-EPISODE component** (`amendEpisodeKey` — the park's `held_commit_sha`, falling back to `verified_tree_sha`, embedded in the marker reason). A stage that is amended, re-runs and parks AGAIN presents a byte-identical `(stage, reason, path set)` on its second episode; without the episode key it would adopt the first episode's row, keep one `amendment_id`, and amend without limit — the cap would never bite. An unidentifiable episode (empty key) refuses reuse entirely and mints a row per attempt: that over-counts against the cap (fail-CLOSED on the control) and only over-spends the agent's #961 budget, which `agent_amendment_budget_remaining` reports honestly.
- **The cap admits a retry of an amendment it has ALREADY counted.** A resume failure on the LAST permitted amend has already appended that amendment's audit entry, so a plain `used >= max` check would refuse the retry `amend_budget_exhausted` and strand the stage parked with no admissible decision left. `scopeCompletenessAmendUsage` therefore returns the counted `amendment_id`s alongside the count, and the cap refuses only a request that does NOT resolve to one of them — a retry adds no new amendment, so it stays admissible at the boundary while a genuinely new amend is capped exactly as before.

**Retry-safe is NOT concurrency-safe (accepted residual, stated rather than implied).** Retry-safety orders a SEQUENTIAL re-POST against its own orphan; it does not serialize two SIMULTANEOUS ones. Every read the amend decides on is a non-transactional read-then-write, leaving two TOCTOU windows on concurrent POSTs against the same parked stage. (1) **The cap and the reuse key:** `scopeCompletenessAmendUsage` and `findOperatorAmendRow` both read before either writes, so two amends can each pass the cap, and two decisions differing in paths or reason miss each other's reuse key and mint two rows — the same budget drain the retry path closes, arriving by a different route. (2) **The owner guard:** the owning sibling's `StartedAt` is inspected before the resume, so a sibling implement stage that STARTS inside that window admits exactly the divergent-edits state `amend_refused_owner_slice_active` exists to prevent. Neither window is guarded or tested. Closing them needs the three writes plus their preceding reads under one transaction with a per-stage lock, which is a repository-interface change (`ScopeAmendmentRepo` and `RunRepo` are separate seams with no shared `Tx`), not a local fix. The exposure is bounded by who can reach it: the decision is operator-only (a run-bound agent token is refused `run_token_forbidden`), so it takes two operators — or one operator double-submitting — racing on one parked stage.

**The true cost to the re-run agent, stated honestly.** An operator amend consumes ONE of the stage's TWO `maxScopeAmendmentsPerStage` slots, because that cap counts rows and the amend row hangs off the same stage — a re-run agent that would have had two #961 requests has one. Excluding operator-originated rows from that count would need an origin column and a migration. The response's `agent_amendment_budget_remaining` reports what is actually left. The residual: a retry that CHANGES the paths or the reason is a different amendment and correctly mints its own row, so the orphan from the earlier attempt does stay and does consume a slot; that is visible in the same field rather than hidden.

**Declared residual (not fixed here).** After an amend the re-run can park AGAIN, and migration 0067's partial unique index on `parent_awaiting_child_scope_decision` is keyed `(run_id, child_stage_id)` with no episode component (see `build_required_park.go`). The second park's PARENT-side signal is therefore deduped away and the parent chain keeps the FIRST park's `build_required_paths`. The child's own `scope_completeness_parked` entry is fresh, so the shortfall itself is never lost — only the parent-side attribution goes stale.

## Acceptance-criteria amendment at the plan gate (`acceptance_amendments.go`, E67.11 / #2581)

Plan-approval conditions reshape the design but never touch the plan's
acceptance criteria, so the acceptance stage validates the shipped behaviour
against the PRE-approval contract and fails a correct implementation (#2581, the
observed `healthz-reports-server-budget` failure in run 66c938d1). Two halves fix
that, and a deliberately-rejected third is documented below.

**1. The explicit channel.** `POST /v0/stages/{id}/approvals` accepts
`amend_acceptance_criteria`: a list of `{id, action: retire|restate|add|retire_scenario, reason,
statement?}` (`add` in 1b below). `retire` drops a criterion out of the live contract; `restate`
replaces its statement and leaves it LIVE (restatement is NOT a silencing
channel — a restated criterion still fails if it genuinely fails). The reason is
REQUIRED per criterion; the amendment is recorded on the SAME
`approval_submitted` payload as the `comment` / `add_scope_files` /
`remove_scope_files` that motivated it, so each retirement's id, reason and
source are reconstructable from the chain alone.

**1b. The widening action — `add` (#3181).** `retire` narrows and `restate`
rewords, but nothing widened the contract: an operator who spotted a missing
drivable criterion at the gate could only write a reason condition, which is
structurally unsatisfiable (criteria live in the approved plan artifact the
implement stage cannot write — run 0aad7486). `{id, action: add, reason,
statement}` APPENDS an operator-authored criterion in the same call. `id` is a
FRESH `^[a-z0-9][a-z0-9-]*$` slug; statement and reason are required. The seam
materializes it as an explicit criterion with `source_ref: operator_approval`,
its reason as `rationale`, `skip_expected: false` (always drivable) and
`blocking: false`, appended to `Live` AFTER every plan criterion, to `AllIDs`
(so `acceptance_criteria_ids` — now served from the seam — admits a verdict row
for it) and to `Added`. The acceptance prompt names it under an
"Operator-authored at approval" block: it did NOT pass plan review, it must be
validated, and a failure on it alone does not condemn the change. It is
ADVISORY by construction, not only by the `blocking` flag (which
`aggregateAcceptanceResults` never reads): a failed verdict whose failures name
ONLY operator-added criteria is neutralized at ingest under downgrade basis
`added_criteria_only`, and the orchestrator's acceptance short-circuit evaluates
the EFFECTIVE verification (via the nil-safe `EffectiveAcceptance` hook), so an
add on an all-skip-with-basis plan is actually driven instead of settling
`not_validated`.

**2. Contested context in the acceptance prompt.** The acceptance prompt renders
the binding approval conditions AND the paths the operator dropped via
`remove_scope_files` as CONTESTED CONTEXT, with a skip-not-fail instruction:
where an observed behaviour conflicts with a criterion because a condition
changed the design or dropped its surface, the validator reports that criterion
`result`=`skipped` with the conflict in its `expectation_basis`, never
`result`=`failed`. This half is a PROMPT INSTRUCTION to an LLM validator — the
tests prove the instruction RENDERS, not that a validator obeys it. The failure
direction is safe: a non-compliant validator yields at worst today's spurious
failure, never a silent pass.

**Automatic DERIVED retirement is deliberately NOT provided.** Retiring a
criterion because its "subject" is a file dropped by `remove_scope_files` is a
natural-language judgement no token rule decides, and a rule that silently
retires a live criterion converts a loud acceptance failure into silence — the
exact inversion of the defect this fixes. So the ONLY retirement source is the
operator (`acceptanceRetirementSourceOperator`), and the dropped paths reach the
validator as context to judge in situ rather than as an inference.

### The single seam and its call sites

`resolveEffectiveAcceptanceCriteria` is the ONE place the effective set is
computed. No caller may union, filter, or recompute any part of it; a consumer
needing a different projection changes that signature. It returns `Live` (plan
order, restatements applied), `Retired` (plan order, with reason + source +
recording audit sequence), `Restated` (the ids a restatement replaced, plan
order), `Added` (operator-added ids, add order, #3181), and `AllIDs` — ALWAYS the
full plan id list plus every added id.

`Restated` exists because `Live` alone cannot signal that an amendment applied:
a restate-only history leaves `Live` the same LENGTH as the plan set with only a
statement differing. `amended()` (`len(Retired) > 0 || len(Restated) > 0 ||
len(Added) > 0`) is the
single predicate a consumer uses to choose the effective set over the plan set —
`resolveAcceptancePromptCriteria` keying that decision off `Retired` alone
silently dropped a restate-only amendment on BOTH prompt paths, so the validator
judged the change against the very statement the operator replaced at the gate.
On a restate-only history the live set renders and the retired block does not
(nothing was retired). Pinned by
`TestGetStagePrompt_Acceptance_RestateOnly_RendersReplacement` and its
`TestRenderStagePrompt_…` twin.

It is consumed at exactly FOUR call sites:

1. `checkAmendAcceptanceCriteria` (the approve gate) — **validation-only**: it
   resolves the PRIOR effective set to evaluate the refusals below and records
   nothing derived from the result.
2. `handleGetPrompt`'s acceptance branch (the signed dispatch prompt).
3. `handleRenderPrompt`'s acceptance branch (the render/preview prompt).
4. `handleShipAcceptance` (verdict ingest), where the recorded retired-id set is
   the strict key for the downgrade.

### Anti-silencing gate — named refusals, all PRE-Submit

Every refusal inserts NO approval row, so a corrected retry flows normally
(the ADR-036 placement its sibling gates use). `details.rule` names each one:

| # | Refusal | Status |
|---|---|---|
| R1 | `unknown_criterion_id` — not in the approved plan | 400 `validation_failed` |
| R2 | `reason_required` — blank/whitespace reason | 400 |
| R3 | `statement_required` — `restate` with no statement | 400 |
| R4 | `duplicate_id` — the same id twice in one request | 400 |
| R5 | `amendment_not_approve_plan_stage` — a reject, or a non-plan stage | 400 |
| R6 | every criterion retired in ONE call | 422 `acceptance_criteria_all_retired` |
| R7 | every criterion retired CUMULATIVELY (prior approvals retired the rest) | 422 `acceptance_criteria_all_retired` |
| R8 | `already_retired` — an id a PRIOR approval retired | 400 |
| R9 | plan unloadable / zero criteria / prior amendments unreadable — applies to `add` too | 422 `acceptance_criteria_unavailable` |
| R10 | `criterion_id_exists` — an `add` id already in the plan, added by a PRIOR approval, or carrying `scenario:` | 400 |
| R11 | `invalid_criterion_id` — an `add` id not matching `^[a-z0-9][a-z0-9-]*$` | 400 |
| R12 | no REVIEWED criterion would stay live while any `add` is present (one call or cumulatively) | 422 `acceptance_criteria_all_operator_authored` |

R6 and R7 are ONE control evaluated on the deduplicated union of prior and
in-flight retirements: the channel cannot empty a plan's acceptance contract, in
one call or across many. R12 is its anti-SUBSTITUTION mirror, read off the SAME
union so the two cannot disagree: when that union would leave no reviewed
criterion live and an add is present (in this request or recorded earlier), the
refusal is `all_operator_authored` — an advisory criterion may never become the
whole contract. R9 applying to `add` is the same rule seen from a criteria-less
plan. An action outside `{retire, restate, add, retire_scenario}` is refused under
`unknown_action`; an oversized reason/statement is CAPPED (`prompt.CapText`),
not refused. Unlike the scope channels — which IGNORE a non-plan-stage value
(#2598) — this channel REFUSES it, because a silently dropped amendment diverges
what the operator believes they retired from what the chain records.

### Downgrade at verdict ingest — four CONJUNCTIVE preconditions

`acceptanceDowngrade` neutralizes a FAILED verdict to `passed` only when ALL
hold:

- **D1** `verdict=failed` AND `failure_mode=assertion_fail`. An `error` mode (a
  crash / 500) is never downgraded — a crashing target is not a superseded
  expectation.
- **D2** EVERY failed criterion result's id is in the RECORDED retired-id set.
  Strict keying on the chain, never a prompt-text or heuristic match.
- **D3** the surviving partition is non-empty (at least one reported result whose
  id is NOT retired) — a verdict reporting only retired criteria evidences
  nothing about the live contract.
- **D4** no surviving BLOCKING un-evaluated criterion: no non-retired criterion
  REPORTED `skipped` **or `undecidable`** whose plan `blocking` value (nil → true)
  is true. D4 keys on what the verdict REPORTS, not on the live set: a blocking
  live criterion the verdict OMITS entirely does not block the downgrade (the
  same trust model that already lets a validator omit criteria from a `passed`
  verdict). `undecidable` (#2512) joins the arm with identical semantics — without
  it, a retirement plus a surviving blocking criterion the validator could not
  decide silently produces `passed`, a green light over unevaluated evidence.
  Pinned in both directions by `TestDowngrade_SurvivingBlockingSkip_NotDowngraded`,
  `TestDowngrade_SurvivingBlockingUndecidable_NotDowngraded` and
  `TestDowngrade_OmittedBlockingLiveCriterion_StillDowngrades`.

On a downgrade the `acceptance_outcome_recorded` payload records
`verdict=passed` plus `verdict_reported`, `downgrade_basis`, and
`retired_criterion_ids`; `failure_mode` and the raw criteria tallies stay what
the agent reported (evidence, not verdict), the STORED ARTIFACT BYTES are never
rewritten, and triage is skipped (no failure left to route). The merge gate reads
the audit payload, so `acceptanceGateState` returns the merge-eligible
`acceptance_passed` with no gate change.

### The undecidable acceptance outcome (#2512, E48.78 layer 4)

A validator that attempted a criterion and genuinely could not DECIDE it used to
have only two words for it: `failed` (which flattens honest uncertainty into a
defect signal, lands the run in acceptance triage, and can be discharged only by
the #2474 arbitration verb) or `passed` (a green light over an unevaluated
criterion). `undecidable` is the third: a per-criterion `result` value carrying a
REQUIRED, non-whitespace `undecidable_reason`, from which the server derives a
run-level disposition.

**The partition, settled by construction rather than by convention.** Three names
share one contract — merge-eligible, never a pass, distinct state string,
operator acknowledgement asked for in the merge verdict — and are partitioned by
a single total question: *was there evidence, and what did it say?*

| | decided | evidence | outcome |
|---|---|---|---|
| `not_validated` (#2347, #3397) | PRE-SPAWN from the plan (no rows), OR POST-RUN when every non-retired row is `skipped` / no rows were itemized / every itemized row was retired | verified nothing — pre-spawn: no runner/preview (bases `empty-criteria` / `all-skip-with-basis`); post-run: the stage RAN but observed nothing (bases `all-skip-observed` / `no-rows-observed` / `all-retired-observed`) | merge-eligible |
| `undecidable` (#2512) | POST-RUN, from the agent's own rows | the stage ran and drove the preview; at least one row could not be decided | merge-eligible |
| `failed` → `acceptance_triage` (#2474) | POST-RUN, from the agent's own rows | a criterion genuinely failed | blocked until arbitrated |

They stay MUTUALLY EXCLUSIVE by construction because ONE total severity ladder
decides between them (`passed < not_validated < undecidable < failed`): `failed`
strictly above `undecidable` strictly above the all-skip/no-rows `not_validated`
case, so one failed row keeps the run in triage and one undecidable row keeps it
undecidable — even though `not_validated` can now carry all-skipped rows. (The
`not_validated` recorded here is server-derived; auditcomplete's trace exemption
still honors ONLY the two pre-spawn bases, so a POST-RUN all-skip/no-rows verdict
still owes its trace — a runner spawned.) `undecidable` routes NO `acceptance_triage_decided`
disposition and never enters the class-1..5 triage classifier — an undecidable row
is not a defect, so there is nothing to fix up or retry. That is the #2474
wedge-surface reduction: the run lands merge-eligible with no arbitration.
Disjointness is pinned by
`TestAcceptanceAdmission_ShortCircuitIsDisjointFromUndecidable`.

**The aggregation is a TOTAL, EXPLICIT precedence ladder** over the per-criterion
rows (`aggregateAcceptanceResults`): any `failed` row → `failed`; else any
`undecidable` row → `undecidable`, **including when EVERY row is undecidable and
nothing was verified**; else if every row is `skipped` (a non-empty set with no
passed/failed/undecidable row) → `not_validated` (#3397 — a validator that
skipped every criterion verified nothing); else `passed`. Read those clauses
literally — an all-undecidable or all-skip set is a run that verified nothing at
all, exactly the case an operator most needs described correctly. A row set
containing an undecidable row can NEVER be recorded as `passed`, and an all-skip
set can never be `passed` either: a green light over an unevaluated criterion is
the dangerous direction, because nobody looks behind it. The seam layers the
empty-row-set door on top (binding condition 1, #3397): an empty non-retired set
is laddered against `not_validated` directly, so a shipped `passed` that itemized
no rows records `not_validated` while a shipped `failed` with no rows stays
`failed`. The empty non-retired set has TWO distinct origins, recorded under
distinct bases so they stay tellable apart: `no-rows-observed` (the validator
itemized nothing) and `all-retired-observed` (rows WERE itemized but the operator
retired every one). The `all-retired-observed` origin is reachable only from a
shipped `passed` — the #2581 downgrade's D3 precondition requires a surviving
non-retired row, so an all-retired shipped `failed` never downgrades and the
severity ladder leaves it `failed`.

The shipped-verdict/derived-verdict mismatch resolves SEVERITY-MONOTONE as a
lower bound on the total order `passed < not_validated < undecidable < failed`:
`recorded = acceptanceVerdictAtLeast(shipped, derived)`. Nothing is ever softened
below what either source claims. **Compatibility note (not purely additive):** an
already-valid body shipping top-level `passed` with a `failed` criterion row
recorded `passed` before #2512 and records `failed` now. That is a behaviour
change on a pre-existing wire shape carrying no undecidable data, and it is
deliberate — recording a pass over a row the agent itself reported failed was the
dishonest direction. Pinned by
`TestAcceptanceSeam_ShippedPassed_FailedRow_NowRecordsFailed`.

`aggregateAcceptanceResults` is TOTAL and answers an EMPTY row set with `passed`.
That answer is never taken as-is: its single call site guards on `len(rows) > 0`
and, on an empty non-retired set, ladders the shipped verdict against
`not_validated` DIRECTLY (#3397, binding condition 1) rather than calling the
function — so a shipped `passed` with no rows records `not_validated`
(`no-rows-observed`), a shipped `passed` whose every itemized row was retired
records `not_validated` (`all-retired-observed`), and a shipped `failed` with no
rows (or all rows retired) stays `failed`. The
function's empty-set answer is pinned openly by
`TestAggregateAcceptanceResults/EMPTY-row-set-answers-passed-the-documented-hazard`;
the shipped-`failed`-with-no-rows anti-softening property by
`TestAcceptanceSeam_NoCriteriaRows_ShippedVerdictRecordedUnchanged` and
`TestShipAcceptance_NoRows_ShippedFailed_StaysFailed`; the shipped-`passed`
neighbouring-door by `TestShipAcceptance_NoRows_RecordsNotValidated`; and the
all-retired origin (both directions) by
`TestShipAcceptance_AllRowsRetired_ShippedPassed_RecordsAllRetiredBasis` and
`TestShipAcceptance_AllRowsRetired_ShippedFailed_StaysFailed`.

**Ordering at the ingest seam is load-bearing.** The #2581 retired-criterion
downgrade runs FIRST over the retired-id set; the ladder then runs over the
NON-RETIRED rows only. If the ladder ran over all rows, a retired criterion's
undecidable row would pin the run to `undecidable` forever and the operator's
retirement would have no effect
(`TestDowngrade_RetiredCriterionUndecidableRow_DoesNotPinTheRun`, with its paired
positive `TestDowngrade_SurvivingUndecidableOutsideRetirement_RecordsUndecidable`).
`verdict_reported` is the ONE "what the agent said vs what was recorded" field
and is present whenever the two differ, for either reason; `downgrade_basis` and
`retired_criterion_ids` stay gated on an actual retirement.

### Head binding: the consolidated fan-in fallback and the unbound-head clamp (#3091)

An acceptance verdict is only meaningful if it names the tree it validated. Two
holes let a `passed` be recorded with `head_sha: ""`, and both are closed here.

**Why a decomposed parent resolved no head at all.** `resolveNewestReportedHeadSHA`
(and therefore `resolveAcceptanceExpectedHeadSHA`) reads the run's OWN
reported-head ledger — `pull_request_opened` / `child_pushed` / `fixup_pushed`.
A DECOMPOSED PARENT writes NONE of them on its own chain: each child's push
lands on the CHILD's chain (see the decomposition-awareness note on
`buildReportedHeadLedger`), and the consolidated PR is opened by
`orchestrator.maybeOpenConsolidatedPR`, whose `consolidated_pr_opened` payload
carries a `pull_request_url` and no SHA. So the ledger answered `""`, the
acceptance-admission endpoint shipped an empty `expected_head_sha`, and the
dispatch gates warned-and-proceeded.

`resolveConsolidatedFanInHeadSHA` supplies the missing head from the parent's own
`integration_commit_recorded` entries (`lineageIntegrationCommitCategory`, #1806):
the fan-in emits one per "Integrate slice N" merge the instant it is created, plus
one `kind: base_fold` row for the base-tip fold merge (ADR-096 D4 / #4294) that a
pass with a pending slice writes into a pre-existing consolidated branch BEFORE
its slice merges, and each such merge commit becomes the consolidated branch tip,
so the highest-SEQUENCE `merge_sha` IS the consolidated head (the fold row is
appended before the pass's slice rows, so the last slice merge stays newest). Ordering is by sequence,
not timestamp — the fan-in writes these within one pass, and the dispatch-anchored
caller compares against a sequence anchor.

Three properties are deliberate:

- **Strictly SUBORDINATE.** `acceptanceHeadForRun` returns the reported head
  verbatim whenever one resolves; the fallback fires only on `""`. A
  non-decomposed run's answer is byte-identical to before
  (`TestResolveAcceptanceExpectedHeadSHA_ReportedHeadWins`).
- **ACCEPTANCE-ONLY.** `lineageLedgerCategories` and
  `auditcomplete.HeadReportCategoriesByPrecedence` are NOT widened — they are
  shared with the branch-lineage guard and the audit-check publisher, and
  widening them would silently change head resolution for surfaces this has
  nothing to do with. `resolveFixupExpectedHeadSHA` is unaffected
  (`TestResolveFixupExpectedHeadSHA_UnaffectedByIntegrationEntry`).
- **DISPATCH-SEQUENCE-BOUNDED on the validated-head path.**
  `acceptanceValidatedHeadSHA` applies the fallback with `bounded=true` against
  the same `dispatchSeq` anchor its reported-head walk uses, so a fan-in merge
  recorded AFTER the acceptance dispatch can never re-bind the verdict to a tree
  the stage did not validate
  (`TestShipAcceptance_PostDispatchIntegrationCommitDoesNotBind`).

**Anchor provenance and the retained sequence bound (E64.53 / #3174).** The
`acceptance_dispatched` anchor is written by `Advance` for a backend-triggered
dispatch and by the host-dispatch marker for a local host spawn (see "Dispatch
emit" above), so a re-opened stage's RE-dispatch advances it. The
`e.Sequence <= dispatchSeq` bound in `acceptanceValidatedHeadSHA` is UNCHANGED by
that fix and must NOT be loosened: it is what excludes a post-dispatch head from
a verdict the stage never validated. Two anchorless residuals fall through to the
clamp below — a local ship that never went through the marker, and a
FIRST-dispatch anchor-append failure at the marker
(`TestHostDispatchAnchorAppendFailure_ShipClampsUndecidable`).

**Episode-restart staleness guard (E64.54 / #3176).** A RE-dispatch whose anchor
append fails is NOT an anchorless residual — the PREVIOUS episode's anchor is
still on the chain, so `acceptanceValidatedHeadSHA` would resolve it and bind the
verdict to the PRE-fix-up tree, a confident wrong answer where every sibling path
is fail-closed (#3174's residual). `acceptanceValidatedHeadSHA` closes it at
RESOLUTION time: it resolves the newest `acceptance_reopened` marker for the stage
(`latestAcceptanceEpisodeRestartSeq`) and treats an anchor at or below it as
STALE — a prior episode's anchor — returning `("", false)` exactly like an absent
anchor, which the clamp below records as `undecidable`/`head_unresolved`. Two
design constraints, both load-bearing and non-obvious:

- **No new audit write on the failing path.** Option 1 (invalidate the stale
  anchor at the marker) was rejected because the invalidation would itself be a
  DB write on the exact path where DB writes are failing — inert in the fault it
  exists to handle. The guard adds only reads.
- **A pure within-chain SEQUENCE comparison, never a timestamp.** A
  DB-stamped-vs-Go-side comparison here would be the #3048 cross-clock trap, and
  its near-tie direction would clamp EVERY acceptance verdict to undecidable,
  since the anchor is appended microseconds after the transition. Both compared
  values are `Sequence` on the SAME monotone append chain.

The guard is TOTAL over episode restarts ONLY while every re-open path writes an
`acceptance_reopened` entry scoped to the acceptance stage. There are exactly two
such non-test writers — `reopenAcceptanceOnFixupPush` (the #1682 fix-up
invalidation) and `writeAcceptanceReopenAudit` in `retry.go` (the #1567 operator
re-open) — and `TestAcceptanceReopenedWriters_AreExactlyTheKnownSites` is the
source-level AST gate that FAILS IN BOTH DIRECTIONS (a new writer or a removed
one) so a third re-open path cannot silently make the guard inert. An unreadable
restart ledger fails CLOSED (undecidable), never open. Pinned by
`TestShipAcceptance_StaleAnchorAfterReopen_ClampsUndecidable` (the m-stale /
m-fresh / m-none / m-readerr table) and end to end by
`TestAcceptanceSeam_LocalRedispatchAnchorAppendFailure_ClampsUndecidable`.

Remaining residual, strictly NARROWER than #3174's (which needed only the second
failure): if the SAME DB fault fails BOTH the `acceptance_reopened` append at the
re-open AND the `acceptance_dispatched` append at the re-dispatch, no restart
marker exists and the stale anchor still answers. It cannot be closed by any
write-based approach — a write is precisely what is failing. Likewise, an episode
restart that writes NO `acceptance_reopened` entry at all (a hypothetical future
third re-open path) is outside the guard's reach; the source-level gate above is
what keeps that hypothetical from landing unnoticed.

**The unbound-head clamp.** Resolution can still legitimately answer `""` — a
bare operator ship with no `acceptance_dispatched` anchor, an unreadable ledger,
a fan-in that recorded no merge. `handleShipAcceptance` therefore runs one final
rewrite after the #2581 retirement downgrade and the #2512 row aggregation: when
`validatedHead == ""` the recorded verdict is raised through the SAME
severity-monotone `acceptanceVerdictAtLeast` ladder to `undecidable`, and
`undecidable_basis: head_unresolved` is attached to the
`acceptance_outcome_recorded` payload.

- **Ladder position and DIRECTION.** `passed (0) < undecidable (1) < failed (2)`.
  Because the clamp is a `max` against `undecidable`, a shipped `failed` can
  never be softened — that is a property of the operation, not of a branch that
  could be inverted. Pinned in both directions by
  `TestShipAcceptance_EmptyHead_PassClampedToUndecidable` and
  `TestShipAcceptance_EmptyHead_FailedNotSoftened`.
- **It runs LAST** because an unbound head invalidates whatever the row
  aggregation concluded about the tree.
- **It applies to EVERY unbound ship, a #2581 retirement neutralization
  INCLUDED (#3124).** Retirement and the unbound-head clamp are orthogonal:
  retirement decides WHICH CRITERIA COUNT, an unresolvable validated head means
  no tree is named AT ALL — so a neutralized pass on an unbound head is no more
  anchored to a validated commit than any other pass, and is raised to
  `undecidable` too. The direction stays safe by construction: the `max` ladder
  can only lift the neutralized `passed (0)` to `undecidable (1)`, which is
  strictly LESS soft than the `passed (0)` retirement itself produced from the
  shipped `failed (2)`, so nothing is softened. The operator's decision stays
  fully visible on the clamped payload via `downgrade_basis` /
  `retired_criterion_ids` / `verdict_reported`, and triage routing is unchanged —
  it stays keyed on the AGENT's shipped `failed` claim plus the downgrade, so a
  neutralized-then-clamped outcome still routes no triage. Pinned by
  `TestShipAcceptance_EmptyHead_RetirementNeutralizedPassClamped` (the clamped
  arm) and its resolvable-head sibling
  `TestShipAcceptance_RetiredOnlyFailure_RecordedPassed` (which proves an
  ordinary retirement WITH a bound head still records the neutralized `passed`
  exactly as #2581 requires). `TestE2E_AmendAcceptanceCriteria_RetiredAtApproval_...`
  in `backend/internal/integration/mcp` pins the resolvable-head retirement arm
  end to end (it seeds a validated head + dispatch anchor precisely so the clamp
  does not fire there).
- **The basis is attached only when the clamp rewrote the verdict,** so a shipped
  `failed` on an unbound head carries no `undecidable_basis` it did not earn.
- **`undecidable_basis` is its own key,** distinct from the #2581
  `downgrade_basis`, which keeps its retirement meaning; both are omitted when
  they did not fire, so an outcome with a resolvable head marshals
  byte-identically to before. `effectiveVerdict` is recomputed AFTER the clamp,
  so `verdict_reported` carries the agent's shipped verdict on a clamped outcome.
- **No new downstream state.** `undecidable` already maps to the
  `undecidable` render vocabulary, routes no triage (#2512), and is
  merge-eligible-with-acknowledgement — so the operator-visible change is that an
  unbound run must be acknowledged at the merge gate instead of reading
  "accepted".

**`undecidable_causes` names WHY rows were left undecided (E72.63 / #4095).**
`handleShipAcceptance` classifies each UNDECIDABLE criterion row's
`undecidable_reason` with `plan.ClassifyUndecidableReason`
(`backend/internal/plan/acceptance_cause.go`, helper
`acceptanceUndecidableCauses`) and records the classified row ids per cause on
the `acceptance_outcome_recorded` payload as `undecidable_causes`, e.g.
`{"sandbox_denied_db": ["a", "b"]}`, in row order.

- **Descriptive only.** It never changes the recorded verdict, the severity
  ladder, the clamp, `verdict_reported`, the tallies or triage routing.
- **Emitted only when non-empty,** so an outcome with no classified row
  marshals byte-identically to before; older entries simply lack it and no
  reader requires it. Presence (not emptiness) is pinned by
  `TestShipAcceptance_UndecidableCauses_OmittedWhenUnclassified`.
- **Undecidable rows only** — the same rows `criteria_undecidable` tallies,
  retired rows included. A passed, failed or skipped row is never classified,
  even when its `expectation_basis` names a database
  (`TestShipAcceptance_UndecidableCauses_SandboxDeniedDB`).
- **One cause today, `sandbox_denied_db`:** the check deciding the criterion
  needs a database or container daemon the acceptance network sandbox cannot
  reach. It matches the explicit `sandbox_denied_db` token the acceptance
  prompt asks the agent to lead the reason with, or — as a heuristic fallback
  over agent prose — a database/container marker together with a sandbox
  denial (the `operation not permitted` EPERM text, or the word `sandbox`
  beside a refusal verb). A reason using `sandbox` only as a LOCATION ("the
  sandbox checkout lacks the database migration file") does not classify.
  The accepted false-positive boundary is a reason that also carries a refusal
  verb for an unrelated cause; that is acceptable because the key is
  descriptive. Pinned by `TestClassifyUndecidableReason`, whose positive rows
  are the four verbatim #4094 reasons.

**Both dispatch gates now fail CLOSED on an unresolvable expectation.** The
verb-side gate (`backend/internal/mcpserver/acceptance_target.go`) refuses with
`acceptance_expected_head_unresolved` BEFORE its `FISHHAWK_ACCEPTANCE_PREVIEW_CMD`
proceed branch (a provision command cannot provision an unknown head), and the
runner's `acceptanceTargetGate` fails the stage category-C with the same reason
before provisioning. A refused acceptance is recoverable in seconds; an
unfalsifiable `passed` is not recoverable at all.

**`undecidable` is SERVER-DERIVED and UNFORGEABLE.** The ship endpoint's
top-level verdict enum still admits only `passed`/`failed` — that switch is
deliberately INCOMPLETE with respect to the `acceptanceVerdict*` constant set, and
`undecidable` joins `not_validated` in never being admissible on the wire. A
producer expresses undecidability on a criterion ROW, never at the top level. Do
not "complete" the enum: the counterfactual for that control is COMPLETION, not
deletion (`TestAcceptanceBody_TopLevelUndecidableVerdictRejected`).

`acceptanceGateState` resolves a recorded `undecidable` verdict to
`acceptance_undecidable`, handled BESIDE the `not_validated` arm rather than
falling through to the settled-outcome-unknown hole (which would wedge every
undecidable run at a 409), and `acceptanceGateAdmitsMerge` admits it — one
predicate, so all three merge consumers (#2474) admit it at once.

**`undecidable_reason` is decided on field PRESENCE from the RAW BYTES, never on
emptiness or on a nil pointer (#2787, the #2699 `validateReapExpectedState`
pattern).** It is decoded as a `json.RawMessage` on both validators and read
through one byte-identical helper, `decodeUndecidableReason`: absent (nil raw) is
the ONLY admitted shape on a passed/failed/skipped row; ANY present value — a
string, the empty string, an explicit JSON `null`, a number, an object — is
rejected there. On an undecidable row the raw bytes must decode to a JSON STRING
with non-whitespace content: absent, `null`, `""`, whitespace-only and every
non-string all reject (`null` and non-string draw a distinct `must be a JSON
string` message; the four bytes `null` are never read as the text `null`). Absent,
`null` and empty are THREE states. The previous `*string` decode collapsed an
explicit `null` onto nil and admitted `{"result":"passed","undecidable_reason":null}`
as if the key were absent — the bypass #2787 closed; a plain `string` would
likewise admit `""`, since Go's `encoding/json` makes an ABSENT field
indistinguishable from a PRESENT empty one.

**Corpus agreement, not byte-carrying.** The wire shape is validated by two
hand-maintained twins that cannot import each other:
`validateAcceptanceVerdict` in package `main` of `runner/cmd/fishhawk-runner` (the
runner module does not require the backend module) and `acceptanceBody.validate`
here. `docs/spec/acceptance-verdict-fixtures.json`, mirrored into both `testdata/`
dirs by `scripts/sync-schemas`, is the shared proof surface: each side runs the
SAME rows and must produce the SAME admit/reject partition, and CI's schema-sync
gate red-lines a mirror that drifts. The claim established is corpus agreement —
NOT that any test carries bytes returned by one validator into the other, which is
unimplementable across the module boundary. The sharpest rows are a WHITESPACE-ONLY
`undecidable_reason` (the shape one side would admit and the other reject if
either compared against `""` instead of trimming) and, since #2787, the explicit
`null` rows — `undecidable-reason-on-{passed,failed,skipped}-row-null`,
`undecidable-row-reason-null` — plus the non-string rows
`undecidable-row-reason-non-string` and `undecidable-reason-on-passed-row-non-string`:
the shapes one side would admit if it still decoded into a `*string` or trimmed
the raw bytes instead of string-decoding them. Any such divergence would strand a
completed acceptance stage while both suites stayed green.

The backend half proves the partition at the WIRE, not only at the decoder:
EVERY row — both halves — is POSTed VERBATIM to the real ship endpoint, an
admitted body asserted `201` and a rejected body asserted `400
acceptance_invalid`. POSTing only the admitted half would leave a handler whose
decode path had become laxer than the test's own strict decoder admitting a
corpus-rejected shape while the test stayed green; the `top-level-unknown-field`
row is the one that detects exactly that drift (deleting the handler's top-level
`DisallowUnknownFields` admits it `201`).

### Deployed-target identity: the post-deploy release arm (E35.2 / #1599)

A post-deploy acceptance stage (one whose spec stage declares a `deployment`
input, ADR-053) validates the DEPLOYED build, not a PR candidate — and a release
run writes no reported-head ledger entry at all, so without a separate source
its expectation resolved `""` (refused pre-spawn) and every passed verdict
clamped to `undecidable`/`head_unresolved`. `acceptance_deployed_identity.go::resolvePostDeployExpectedSHA`
is that source. It returns `(sha, applies)`:

- **`applies=false` only when the stage is POSITIVELY KNOWN not to consume a
  deployment** — nil `RunRepo`, `GetRun` answering `run.ErrNotFound`, no
  workflow-spec snapshot on the run, a workflow declaring no `deployment` input
  on any stage (decided from the spec alone, so a stage-read blip cannot move a
  feature_change run off its reported head), or a mapped spec stage declaring
  none. These are facts, never absences of knowledge (the split
  `resolveRunWorkflowDef` draws). Only then do callers take the reported-head
  path, byte-identical to before.
- **`applies=true` on every other leg**, with `sha==""` wherever no deployed SHA
  can be produced — INCLUDING the legs where applicability itself cannot be
  determined: any other `GetRun` error, a `ListStagesForRun` error, an
  unparseable spec, a workflow absent from it, a stage absent from the run's
  rows, a stage with no spec counterpart at its type ordinal (operator approval
  condition 2 on #1599: an undeterminable identity never falls back to the
  reported head). Every such leg WARNs with a stable `reason`.

Derivation, once applicable: more than one `deployment` input → `""`
(ambiguous target). The input's `from_stage` maps to its run row by
`runStageForSpecStage`, the INVERSE type-ordinal of `specStageForRunStage` (the
k-th spec stage of a type is the k-th row of that type, rows sorted by Sequence
then ID — the rule that survives a plan-filtered child's renumbering). Then
`deployedSHAFromArtifacts` over that row's deployment artifacts
(`ListForStage`, `created_at` ascending): the NEWEST must decode as `succeeded`
(a later `rolled_back`/`failed`/`partial` means the host no longer serves the
older build); each succeeded record contributes `sha` if it is a full 40- or
64-hex commit SHA (`isDeployCommitSHA`), else `ref` if it is one, lowercased; zero
contributions → `""` (symbolic `main`, or an abbreviated SHA — the runner's probe
prefix-matches the target's `git_sha` against the expectation, so it needs the
full SHA); more than one DISTINCT SHA → `""`. The disagreement rule, not a
timestamp bound, is what stops a different succeeded record posted after
dispatch from re-binding a verdict (no cross-clock comparison, #3048).

Three call sites, one answer:

- `resolveAcceptanceExpectedHeadSHA` (both prompt handlers): arm first; when it
  applies its value is returned verbatim, `""` included.
- `resolveAcceptanceExpectedHeadSHAWalkingParents` (acceptance admission):
  short-circuits on the arm BEFORE the `ParentRunID` walk — an ancestor's
  reported head names a PR candidate, never the deployed build.
- `acceptanceValidatedHeadSHA` (the verdict binding): the arm runs AFTER the
  dispatch-anchor check and the episode-restart staleness guard (order is
  load-bearing — an anchorless or stale-anchored ship still clamps), and a
  resolved deployed SHA binds a passed verdict to it.

Pinned by `acceptance_deployed_identity_test.go` (per-leg table, the caller
tables with a decoy reported head seeded by construction, the walk
short-circuit, the validated-head ordering, and
`TestShipAcceptance_PostDeploy_PassBindsDeployedSHA` through the real ship
handler), `TestGetStagePrompt_PostDeployAcceptance_ServesDeployedSHA` /
`TestGetStagePrompt_Acceptance_FeatureChangeSpec_KeepsReportedHead`, and the
Postgres seam in `release_acceptance_integration_test.go` (reconciler
`head_sha` → stored artifact `sha` → served expectation → verdict binding, plus
the symbolic-ref fail-closed leg). The wire field is unchanged: the runner
decodes it on `FetchedPrompt.AcceptanceExpectedHeadSHA` under the existing
`prompt_response` pair in `backend/internal/wirecontract`'s `SeedManifest`.

### Invariants

- **`acceptance_criteria_ids` stays a SUPERSET.** The ids served on the
  acceptance prompt response are every PLAN criterion id, retired ones included,
  so a verdict reporting a retired id can never fail the stage closed on the
  runner's join-key validation.
- **Stored artifact bytes are never rewritten.** Only the governance payload
  carries the effective verdict.
- **Byte-identical when unused.** With no amendment recorded, the
  `approval_submitted` payload, the `acceptance_outcome_recorded` payload, the
  acceptance prompt, and the ship response are byte-for-byte what they were
  before this change (every new key is `omitempty`/conditional). Asserted against
  FROZEN PRE-CHANGE goldens, not against heading/key absence: the
  `approval_submitted` payload against `wantPreChangeAmendlessApprovalPayload`
  (`approvals_test.go`) and the acceptance prompt's criteria span against
  `wantPreChangeAcceptanceCriteriaSpan` (`prompt_test.go`, captured by rendering
  the package at the pre-#2581 commit) — so a same-keys-different-bytes
  regression fails.
- **A replay echoes the CHAIN, not a recomputation.** `handleShipAcceptance`
  computes the downgrade from the CURRENT approval chain, so a retirement
  recorded AFTER the original ship would make a replay of the same verdict bytes
  answer differently from the governance entry the merge gate reads. The
  idempotent branch therefore reads the recorded outcome entry
  (`recordedAcceptanceEffectiveVerdict`) and echoes ITS effective verdict, falling
  back to the fresh computation only when no entry is readable (in which case the
  #1396 heal just appended exactly that value). Pinned by
  `TestShipAcceptance_IdempotentReplay_EchoesRecordedEffectiveVerdict`.
- **Every degrade fails toward MORE validation.** An unreadable chain at prompt
  build renders the FULL plan criteria set; an unreadable chain, an unreadable
  plan, or NO approved plan at all at ingest performs NO downgrade; an unreadable
  chain at the approve gate REFUSES the amendment; an absent `AuditRepo` or an
  undecodable `approval_submitted` payload leaves the criterion LIVE. None of them
  can silence a criterion.

## Bulk concern waive + the condition-claim shorthand (`bulk_waive.go`, `condition_claims.go`, E64.77 / #3318)

Three surfaces closing the merge-gate concern-waiver toil a 2026-09 campaign surfaced, built on the existing #1956 condition-claim machinery rather than beside it.

### `reject_class` — the structured plan-rejection class (E75.1 / [#3729](https://github.com/kuhlman-labs/fishhawk/issues/3729))

`POST /v0/stages/{stage_id}/approvals` accepts an OPTIONAL `reject_class` naming the CLOSED set **`scope` | `approach` | `verification` | `other`**, so a decision index can aggregate *why* plans get rejected instead of parsing free-text reasons.

- **Validated as a PURE input check, ahead of the stage fetch.** `validateRejectClass` reads no state and has no side effects, and `handleSubmitApproval` calls it beside `validateApprovalComment` — so a refusal inserts NO approval row, advances nothing and emits no audit entry. Two refusals, both `400 validation_failed`: a value outside the set (details `{field, got, allowed}`, and the message NAMES every allowed value), and a non-empty value paired with `decision != reject` (details `rule: reject_class_requires_reject`).
- **Recorded verbatim, only on a reject that declared one.** `writeApprovalAudit` writes `reject_class` on the `approval_submitted` payload under `decision == reject && rejectClass != ""`. Absent AND explicitly-empty both record NO key, so every existing row and every existing caller's body stay byte-identical and an unclassified rejection indexes as unclassified.
- **`decompose_required` is deliberately NOT in the set.** Decomposition is requested with the existing `--decompose` comment marker, which writes `reject_reason: decompose_required` — the key `prompt.go` reads back to inject the decomposed-replan hint. A `reject_class` of the same name would record the intent WITHOUT triggering the replan, so it is refused like any other unknown value and the refusal points at the marker. The two keys are orthogonal by design: `reject_reason` is the replan TRIGGER, `reject_class` is a recording dimension no prompt consumes. The existing `reject_reason` behaviour is unchanged.
- **Mirrored on the MCP side.** `fishhawk_reject_plan` takes a matching optional `reject_class`, threaded through the hand-maintained `approvalRequest` mirror in `mcpserver/client.go` — a json-tag typo there passes both unit halves and fails only the cross-boundary integration case (`backend/internal/integration/mcp/reject_without_concern_test.go`).

### `claims_all_open_plan_concerns` — the approve-time expansion

`POST /v0/stages/{id}/approvals` accepts `claims_all_open_plan_concerns: true` as the shorthand for `claims_concern_ids`. `expandAllOpenPlanConcernClaims` resolves it PRE-Submit into the run's full open PLAN-stage concern id set (`ConcernRepo.ListOpenByRun`, filtered to `StageKindPlan`, in origin-sequence order) and the handler threads THOSE ids through the existing `claims_concern_ids` channel — so `writeApprovalAudit` records them under the same audit key, and the whole downstream path (`loadApprovalConcernClaims` → `resolveConditionClaimedPlanConcerns` → `concern_addressed_by_condition`) runs byte-identically with no change at all.

The semantics are a **SNAPSHOT taken during the approve request**, not a continuously-evaluated set. That is the load-bearing choice: expanding at approve time is what lets the resolution path stay unchanged and makes the operator's claim reconstructable from the `approval_submitted` payload alone. The consequence, stated rather than hidden: a concern raised AFTER the approval is NOT claimed and still needs its own decision.

Two additive intent keys ride the same payload — `claims_all_open_plan_concerns: true` and `claims_all_open_plan_concerns_expanded: <count>` — so a shorthand approve is distinguishable from one that hand-listed the same ids, and an expansion that legitimately resolved to ZERO concerns still leaves a trace of the declared intent. Both are omitted otherwise, so a non-shorthand approve marshals byte-identically to pre-#3318.

Named refusals, all pre-Submit (a malformed shorthand inserts NO approval row, so a corrected retry flows normally — the `binding_assertions` / `claims_concern_ids` posture):

| Condition | Response |
|---|---|
| `decision != approve` | 400 `validation_failed`, `details.field` |
| non-plan stage | 400 `validation_failed`, `details.stage_type` |
| `claims_concern_ids` also non-empty | 400 `validation_failed`, `details.rule` `claims_all_and_explicit_ids` |
| `ConcernRepo` unwired | 503 `concern_store_unconfigured` |
| `ListOpenByRun` failed | 500 `internal_error` |

The two channels are **mutually exclusive** because the expansion is always a superset of any valid explicit list: accepting both would make the recorded audit claim ambiguous about operator intent. A store outage is a 500 rather than a 400 for the same reason `validateClaimsConcernIDs` distinguishes them — a transient failure is retryable, not an operator-corrected input, and collapsing it into a 400 would point the operator at re-reading ids.

An EMPTY expansion (no open plan concerns at approval time) is legal: nothing to claim is not an error.

### `claimed_by_approval` — the run-status marker

`GET /v0/runs/{run_id}`'s `concerns.items[]` gain `claimed_by_approval` (omitempty), true when the run's newest claiming approval covers that concern. It means: this concern will self-settle to `addressed_by_condition` at the first confirming implement review and needs no hand waive at the merge gate.

It is only useful while the concern is still OPEN — a settled one has left the list — so the marker is what tells an operator, AT the gate, which of the open concerns they can leave alone. `handleGetRun` loads the claims only when the open set contains at least one plan-stage concern; that gate is a pure PERFORMANCE guard with no behavioural signature (an approval's claim set contains only plan-stage ids, so a run with no open plan concerns yields an empty claimed set either way) and is deliberately not claimed as a control. The load is best-effort exactly like the surrounding Drive block: `loadApprovalConcernClaims` warn-logs and returns nil on any failure, so the MARKERS are simply absent. The concerns block itself must never become absent because of this — presence stays the authoritative store-read signal the MCP review-action hint reads (#3043).

### `POST /v0/runs/{run_id}/concerns/waive` — the bulk verb

Waives a LIST of one run's open concerns under ONE audited reason, recording one `concern_waived` audit row per concern (each carrying an additive `bulk_waive: true` marker; the single path passes `bulk=false` and its payload is byte-identical to before).

Auth mirrors `handleWaiveConcern`: authenticated required, `write:stages` OR `write:fixups`, and the `mcp:run:` subject-binding guard — compared here against the **PATH** run id, which is what makes the same-run invariant structural on this route and lets the guard decide without a concern read.

**Pre-validation is ALL-OR-NOTHING and mutates NOTHING.** First violation wins, each naming the offending id: blank reason, empty `concern_ids`, more than `bulkWaiveMaxConcerns` (50) ids, a non-UUID id, a duplicate id (all 400 `validation_failed`; ids are parsed BEFORE the duplicate check and deduped on the PARSED `uuid.UUID`, not the raw string, so two spellings of one id — `uuid.Parse` accepts case-insensitive hex, the `urn:uuid:` prefix, braces and the dash-less 32-hex form — collide rather than slipping through to append two `concern_waived` rows for one concern; the refusal carries `concern_id` (the raw spelling sent), `canonical_concern_id` and `first_spelling`); an unwired store (503); an unknown id (404 `concern_not_found`); an id whose `RunID` is not the path run (400 `validation_failed` with `details.rule` `concern_run_mismatch` — the top-level code stays the house-convention `validation_failed` and the specific rule travels in `details`, matching the acceptance-amendment refusals); an id not in an OPEN state (422 `concern_waive_conflict` naming the id and the from/to pair). A `delegated: true` batch evaluates `checkDelegation` ONCE for the run before any intent entry is appended — every batched concern shares the run, so a per-item re-evaluation would read the same state N times.

**The apply loop is PER-ITEM, and the batch is deliberately NOT a database transaction.** Each concern carries its own chained audit row; wrapping N audit appends plus N transitions in one transaction would either serialize the chain append or leave the chain and the concern store inconsistent on rollback. So a concurrent transition landing between validation and apply fails ONE item while the rest still apply — reported honestly as per-item `results` rather than a single misleading status.

The 200 body is `{run_id, reason, waived, failed, results[]}`. **The counting rule:** `waived` counts results with `applied == true` and `failed` counts `applied == false`, so a three-item batch whose SECOND item fails reports `waived: 2` / `failed: 1` with `results` carrying `[applied=true, applied=false, applied=true]` in REQUEST order. `error_code` reuses the single path's vocabulary (`concern_waive_conflict` / `audit_append_failed` / `internal_error`) so an operator reads one code set across both verbs.

The 50-id cap is an arbitrary but STATED bound on the audit-append fan-out and the response size; the campaign that motivated this issue waived roughly 90 concerns across many runs, so a per-RUN batch of 50 sits comfortably above the observed per-run need. It is a named 400 refusal, never a silent truncation.

### The shared `applyConcernWaive` helper

`waive.go` now carries the durable-record-first body as one unexported helper both verbs call, so the ordering invariant cannot drift between them: append the `concern_waived` intent entry FIRST (failure returns `concernWaiveAuditAppendError` with NO mutation and NO corrective entry — there is no intent on the chain to correct), then `ApplyResolution`, and on ITS failure append the corrective `concern_waive_failed` entry before returning the transition error unwrapped so the caller can map `InvalidTransitionError` → 422 and `ErrNotFound` → 404. The typed append error is what lets the bulk path report the same per-item `error_code` vocabulary the single path uses.

## Merge readiness + operator-evidence resolve (`merge_readiness.go`, `resolve_concerns.go`, `gateview.go`, E83.53 / [#4086](https://github.com/kuhlman-labs/fishhawk/issues/4086))

Two gate states that used to be HIDDEN — a merge that queued and then timed out, and a routed concern only a waiver could clear — are now named.

### Merge-readiness refusals (`POST /v0/runs/{run_id}/merge`)

All three are 409s returned BEFORE the `merge_verdict_recorded` append, so none records a verdict nor dispatches a merge. `mergeReadinessRefusal{Code, Message, Details}` never writes HTTP: `handleMergeRun` renders it (adding `run_id` / `pr_url`) and the gate view reuses the same value, so the two surfaces cannot drift.

- **`acceptance_stale` — FAIL-CLOSED, audit chain only.** `acceptanceStaleRefusal` refuses when the run's acceptance stage carries a stage-scoped `acceptance_reopened` entry NEWER than the newest `acceptance_outcome_recorded` — the state `reopenAcceptanceOnFixupPush` (and the #1567 operator re-open) leaves, where `acceptanceGateState` still reads the stale `passed` outcome and admits. It keys on the chain's own invalidation record, NEVER a head compare: a vouch commit or an ADR-090 rebase moves the head without re-opening acceptance, by design, and a head compare would block merges no verb could clear. Details: `verified_head_sha` (the outcome's `head_sha`, "" when unrecorded), `current_head_sha` (the re-open's), both sequences, the stage id/state, and `next_step` (`fishhawk_dispatch_stage` pending / `fishhawk_await_stage` dispatched or running / `fishhawk_retry_stage` settled without a newer verdict). An audit read error is returned, and the endpoint maps it to `500 internal_error` with nothing recorded — the evidence lives on Fishhawk's own chain, like `acceptanceGateState`. Placed after the acceptance gate and BEFORE the `GateMerger` nil check, so it needs no merge seam.
- **`approval_dismissed` — GATE VIEW ONLY, advisory, BEST-EFFORT, FAIL-OPEN, forge.** Since ADR-112 (#4619) the merge endpoint no longer runs `approvalDismissedCheck`: its approval refusal is the forge-approval fold's 409 `forge_approval_required` (§ "ADR-112 forge-approval fold"), so the gate view's `merge_readiness` and the merge endpoint deliberately differ on this leg. The gate view still reports it: it flags only when GitHub reports `mergeable_state=blocked`, no reviewer's LATEST review (COMMENTED / PENDING ignored) is `APPROVED`, and at least one is `DISMISSED`. Every uncertainty is reported as undetermined, never a blocker — no GitHub client, no installation, an unparseable repo, no PR number, a PR read error, a merged PR, any other `mergeable_state`, a reviews read error, a truncated listing. A dismissed review loses its earlier state, so a dismissed CHANGES_REQUESTED looks like a dismissed approval; re-approving is the remedy either way. `dismissing_commit` is the live head; `dismissing_cause` comes from the chain: `vouch_commit`, `fixup_push`, `push`, or `unknown` on an audit read error.

- **`review_coverage_incomplete` — FAIL-CLOSED, audit chain only (#4219).** `reviewCoverageRefusal` (`review_coverage.go`) holds a decomposed parent until every SUCCEEDED slice child is covered by a SUCCESSFUL agent review: the parent's latest implement-review round carries an `implement_reviewed` verdict (a consolidated or fix-up round compares the whole parent diff, so it covers every slice), or each slice's own latest round does. A round whose reviewers all failed or were skipped covers nothing — the d3b1d915 hole; a reject verdict counts. Not applicable to a non-parent, a child run, zero configured agent reviewers, or no wired reviewer backend. Details: `uncovered_slices[]` (`child_run_id`, `slice_index`, `round_sequence`, `reason` `no_review_round` | `review_round_in_flight` | `review_round_no_verdict`), `parent_round`, and `next_step` (`fishhawk_rerun_review` when the parent round settled with no verdict, or when the parent has NO round and its consolidated PR is open — the verb then starts the first consolidated round, #4282; `fishhawk_await_review` while a round is in flight, before the PR opens, and for a GATING parent with no round, whose message names `fishhawk_fixup_stage` because the verb refuses it `gating_authority`). Placed right after `acceptance_stale` and BEFORE the `GateMerger` nil check; a read error is `500 internal_error`. The same predicate feeds the gate view, the drive observer (`drive.RuleReviewCoverageIncomplete`, stamped instead of `checks_green_awaiting_merge`) and the delegated `may_merge` arm (observe-only on the hold), so the four cannot drift. The recovery is the re-run verb (§ "Operator re-run"), including for a parent whose consolidated dispatch never started a round; no merge-time override ships.

`fishhawk_merge_run` maps all three to IMMEDIATE statuses carrying a `next_action` instead of timing out (`backend/internal/mcpserver`).

### Gate-view `merge_readiness` block

`gateViewMergeReadinessFor` builds `{blockers[], undetermined[]}` from the helpers above. It runs ONLY in `handleGetRunGateView`, never in `buildGateView`, because `buildGateView` is also the attention scan's per-run builder and the approval half costs forge reads (`TestGateView_MergeReadiness` pins that `buildGateView` makes none). Omitted when the run has no PR url or is failed/cancelled (the merge endpoint's `run_not_mergeable` cases). The two halves keep the endpoint's split: the approval half's fail-open reasons land in `undetermined` (`approval dismissal: …`); the acceptance half cannot fail the request here, so a stages or acceptance-history read failure adds `undetermined` (`acceptance staleness: …`), and a review-coverage read failure `undetermined` (`review coverage: …`, #4219), each PLUS the single `merge_readiness` `history_gaps` entry and `history_incomplete` — never a silent empty `blockers`. Both arrays are always present when the block is.

### `POST /v0/runs/{run_id}/concerns/resolve` — the operator-evidence resolve

The counterpart to the permanent `operator_evidence_routed` veto: a fix-up carrying `operator_evidence` makes the concerns it routes immune to reviewer-confirmation auto-resolve, and before this verb only a waiver cleared them, misrecording a resolved concern as a non-blocking one. A HUMAN resolves routed (`addressed_pending`) concerns as `addressed` — the state a reviewer confirmation produces, so a later review can still reopen them — with ONE evidence note for the batch. No state-machine change: `addressed_pending -> addressed` was already an edge.

- **Auth.** Wrapped in `requireRunAccount(memberWrite, …)` (403 `account_forbidden` cross-account). `write:stages` OR `write:fixups` (the waive predicate). HUMAN-ONLY: ANY agent subject (`isAgentSubject` — `operator-agent/…`, or a run-bound `mcp:run:` token even on its own run) is refused 403 `resolve_requires_human` before the body is read. Operator evidence is a human authority claim, so there is no delegated path.
- **Pre-validation is all-or-nothing and mutates nothing** (the bulk waive's shape): blank / whitespace / >4000-byte evidence, empty or >50 ids, a non-UUID or duplicate (parsed) id, an id from another run → 400; unwired store → 503; unknown id → 404; any concern not `addressed_pending` → 409 `concern_resolve_conflict` naming its state and the recovery (route a raised/reopened concern first; a closed one has nothing to resolve).
- **Apply is per-item and durable-record-first** (`applyConcernResolve`): `concern_resolved_with_evidence` `{concern_id, prior_state, evidence, stage_kind, severity, category, provenance?}` is appended FIRST (failure → `audit_append_failed`, no mutation), then `ApplyResolution(addressed, "operator evidence: <evidence>")`; a transition failure appends the warn-only corrective `concern_resolve_failed` and fails that item.
- **Issue thread.** A batch that resolved ≥1 concern calls `notifyOperatorVisible(…, concern_resolved_with_evidence)` AFTER the apply loop, gated on the RESOLVED count (an all-failed or refused batch refreshes nothing). The category is an `issuecomment` `activityCategories` member rendering "Concern `<id8>` resolved with operator evidence: `<evidence>`"; `concern_resolve_failed` is deliberately not.
- **Gate view `resolution_basis`.** A settled row carries `resolution_basis: operator_evidence` only when it is `addressed`, its CURRENT `state_reason` carries the `operator evidence: ` prefix (so the last transition was the resolve, not a later review re-confirmation that overwrote the reason), AND the chain holds a `concern_resolved_with_evidence` entry for it (the intent entry alone is not enough — it precedes the transition, so a failed or overtaken resolve still has one). `concern_resolved_with_evidence` is in `gateViewHistoryCategories`, so a read failure is a `history_gaps` entry and the basis is withheld rather than guessed.
- **Known gap — the historian / precedent index does not see these resolutions.** `backend/internal/decisionindex`'s `decisionBearing` map is unchanged by #4086, so `concern_resolved_with_evidence` is NOT a decision-bearing category: an operator-evidence resolution is invisible to `fishhawk_precedent`, the gate-view precedent block and historian consults, which still see only waives / defers / plan approvals. Indexing it needs its own decision class and is a tracked operator follow-up, not part of this change.

## Escape records (`escape.go`, E94.3 / [#4694](https://github.com/kuhlman-labs/fishhawk/issues/4694))

`POST /v0/runs/{run_id}/escapes` records and closes **escapes** — defects that got past the live reviews — under ADR-117 D3 and its Amendment A1, **RATIFIED** (2026-10-27), with A1.2 as amended by erratum E1. An escape is stored as append-only `escape_recorded` rows on the run's own audit chain, one row per state change (A1.1): a **birth**, and at most ONE **closure** successor. There is no table and nothing is edited in place. The category is INTERNAL (no issue-comment activity line, no status-comment refresh).

### Request shapes

A body carrying `supersedes_sequence` is a CLOSE; any other body is a RECORD. Both decode STRICTLY (`DisallowUnknownFields`, one document, no trailing content; an unknown key is 400 `validation_failed` naming it in `details.field`).

- **RECORD:** `source` (`later_reviewer` | `follow_up_bug` | `operator_battery` | `human_finding` | `ci_red_on_main`); `severity` (`high` | `medium` | `low`); `category` (CLOSED: `correctness` | `security` | `reliability` | `concurrency` | `performance` | `data_integrity` | `test_coverage` | `documentation` | `other`); `description` (≤ 4000 chars, required unless security); `files` (1..200 clean repo-relative paths, required unless security); `head_sha` (optional, 40/64 lowercase hex); `introducing_commits` (optional, ≤ 20 shas, recorded with `introducing_commits_source: operator_supplied`); `concern_id` (UUID, required iff `later_reviewer`); `idempotency_key` (`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`, required for every non-`later_reviewer` source, refused on `later_reviewer`); `security_class` + `advisory_id` (required iff `category=security`, refused otherwise); `delegated` (bool).
- **CLOSE:** `supersedes_sequence` (integer > 0), `attestation {judgement: defect | no_defect, note?: ≤ 2000 chars}`, `delegated`. Any other key is 400.

### Refusal ladder (every refusal appends NOTHING)

1. 401 `authentication_required` — an anonymous identity. Registered as `s.escapeRequireAuthenticated(s.requireRunAccount(memberWrite, s.handleRecordEscape))`, so the 401 precedes the ownership wrapper's 403 `account_forbidden`; the handler repeats it.
2. 403 `run_token_forbidden` — any `mcp:run:` subject (a PREFIX check, so a malformed run-bound subject is refused too), before the scope check. The agent inside a run never records or closes an escape (#4694 AC5).
3. 403 `insufficient_scope` — a token without `write:runs` (cookie sessions carry no scope list).
4. 503 `escape_unconfigured` — no run or audit repository; for `later_reviewer`, no concern repository.
5. 400 `validation_failed` (`details.field`) — a bad `run_id`, the strict decode, any field rule above. A security RECORD carrying `description`, `files` or `introducing_commits` (even empty) is instead 422 `escape_security_free_text_refused` naming the field (A1.5).
6. CLOSE only — the A1.3 attestation refusals below.
7. 404 `run_not_found`. Run state never gates a record or a close (A1.2): a terminal, revived or reaped run neither resets nor blocks closure.
8. The RECORD or CLOSE branch's own refusals; 500 `internal_error` on any read, lookup or append failure.

### Birth (A1.2 / A1.4)

`escapeBirthState`, arms in order: a `later_reviewer` source is born `pending_judgement` (`pending_reason: review_finding_source`); an **agent-recorded** escape — an agent-kind subject (operator-agent, first-officer seat) OR a request with `delegated: true` — is `pending_judgement` (`agent_recorded`); a recorder whose credential kind (`approvalCredentialKind`, the server's one adapter over #4633's `identity.CredentialKind`) is not forge-verified (`session` | `oauth` | `oauth_as`) — e.g. a `static` or `unrecorded` api token — is `pending_judgement` (`credential_not_forge_verified`); only then is a birth `counted` with `judgement_basis: not_required`. A pending birth's `judgement_basis` is `awaiting_attestation`.

The subject is `{pull_request_url (from the run row), pull_request_merged (runPRObservablyMerged), head_sha (from the body)}`; a body with no `head_sha` on a run whose PR is not observably merged is 422 `escape_subject_unresolved`.

**Birth keys** (`source_event_key`, deduped per run with `StageID=nil`; a duplicate is 409 `escape_already_recorded` with `existing_sequence`):

- `later_reviewer`: `concern:<concern_uuid>` — #4690 ADR-117 A1.2 as amended by erratum E1 (re-transcription PR #4784). Never the concern's `OriginReviewSequence`: every concern of one verdict shares that sequence, so it would refuse a verdict's second finding.
- every other source: `idempotency_key:<key>`. For `category=security` on these sources the key must ALSO be a canonical lowercase UUID (R9b as narrowed by approval condition C1); a security `later_reviewer` escape carries no key and dedupes on `concern:<uuid>`.

**Concern lookup** (`later_reviewer`, fail closed): `ConcernRepo.GetByIDs` returning the repository's wrapped `concern.ErrNotFound` (`errors.Is`), a concern whose run is missing, or a concern in ANOTHER account are all 422 `escape_concern_not_found`, answered identically so the verb is not a cross-account existence oracle; any other error is 500. A concern on another run of the SAME account is accepted. The escape binds `concern {id, run_id, stage_kind, origin_review_sequence, review_round_sequence?, reviewer_role, reviewer_model, provenance}`.

**`reviewed_head` on a `later_reviewer` escape (R3).** `escapeConcernRoundHead` resolves it over the CONCERN's run chain: the `implement_reviewed` verdict whose sequence is the concern's `origin_review_sequence` → its `review_round_sequence` (#3593) → the `implement_review_started` row at that sequence → its `reviewed_head` key, copied JSON-equal. It is ABSENT (no key) for a plan-stage concern, a missing verdict, a zero round sequence, a missing started row, or a pre-A1.7 started row with no `reviewed_head`; absence means "the reviewed head is not known", never "the round saw nothing". A read error is 500: the record is immutable, so it fails closed rather than recording a head-less escape that a retry would have bound. On a `category=security` escape the copy is projected onto the scalar identity fields ONLY (`status`, `kind`, `sha`, `tree_sha`, `change_id`, `origin`) — the `files`, `partial_files` and `omitted_files` path sets are dropped (approval condition C3); a non-security escape keeps the full object. reviewed_head on non-later_reviewer escapes: deferred to E94.4.

**`implement_review_started.reviewed_head`** (ADR-117 A1.7, recorded by every implement-review round since #4694's slice 0) is the round's reviewed identity: `status` (`recorded` | `head_unknown` + `unknown_reason`), `kind` (`wip_tree` | `pushed_commit` | `compare_head`), `origin`, `sha`, `tree_sha`, `change_id`, `base_sha`, `diff_mode`, `patch_rendered`, `truncated`, `inventory_best_effort`, `changed_file_count`, and the three pairwise-disjoint plain-path sets `files` / `partial_files` / `omitted_files` whose union is the reviewed diff's changed files. Partial and omitted files count as NOT seen. It is additive (`omitempty`), never enters the review prompt, leaves the `head_sha` #797 dedup key unchanged, and a pre-change row reads as head unknown. Contract: `backend/internal/planreview/README.md`.

### Security escapes (A1.5)

A `category=security` escape carries ONLY `source`, `severity`, `category`, `security {class, advisory_id}`, `subject`, its source reference (`concern` or `idempotency_key`) and, for `later_reviewer`, the C3-projected `reviewed_head`. `security_class` matches `^[a-z0-9][a-z0-9_]{0,63}$` with no enum: an interim vocabulary that closes when #4774 defines the class list (#4783). `advisory_id` is validated by the run's forge (`runForge`): a GitHub run requires a GHSA id (`^GHSA(-[0-9a-z]{4}){3}$`), any other forge an opaque bounded token (`^[A-Za-z0-9][A-Za-z0-9._:#/-]{0,127}$`). D7 security_finding_id deferred to the ADR-126 record; the record carries the provider advisory id (E96 / #4774) — `security_finding_id` is not a field, so strict decoding refuses it 400 and no row carries it. A CLOSE of a security target carrying an `attestation.note` is refused in-transaction, 422 `escape_security_free_text_refused` (`field: attestation.note`).

### Closure (A1.2 / A1.3)

**Attestation predicate.** Three INDEPENDENT refusals on the close path, each 403: an agent-kind subject (`isAgentSubject`: operator-agent, run-bound, first-officer seat) → `escape_attestation_requires_human` (`refused_actor: agent_token`); a `delegated: true` request → `escape_attestation_requires_human` (`refused_actor: delegated`); a credential kind outside `identity.ForgeVerifiedKinds` → `escape_attestation_requires_forge_credential` (`credential_kind`, `forge_verified_kinds`). It shares #4633's derivation with the approval gate, so the two cannot drift. Recording stays open to agent and delegated callers (A1.4); only attestation is human-only.

**State machine and atomicity.** `supersedes_sequence` is the server's canonical base-10 rendering of the target sequence. The close appends through `audit.DedupedChainAppender` with `DedupeSpec{PayloadKey: "supersedes_sequence", PayloadValue: <seq>, BeforeAppend: hook}`: under the run-row lock, in ONE transaction, a prior successor naming the same target is 409 `escape_already_closed` (`successor_sequence`); otherwise the hook reads the target from the same-transaction scan of this run's `escape_recorded` rows — absent (unknown or another run's sequence) → 422 `escape_target_not_found`; not `pending_judgement` (a counted birth, or a closure row) → 422 `escape_target_not_pending` — and builds the successor, copying the target's `escape` object JSON-equal (`reviewed_head` included), so a closure cannot restate a different defect. `defect` closes `counted`, `no_defect` closes `dismissed`; `judgement_basis: operator_attestation`; `attestation {judgement, note?, judgement_id (server UUID), actor_subject, credential_kind}`. The single-successor guarantee holds on the production Postgres repository; an in-memory fake is list + `audit.CheckDedupeSpec` + append (A1.2 allows it).

**Payloads** (`escape_version: 1`, `StageID` nil, `actor_kind` from `actorKindForSubject`): birth `{escape_version, record_kind: birth, run_id, state, judgement_basis, pending_reason?, agent_recorded, recorder {subject, actor_kind, credential_kind, delegated}, source_event_key, escape {...}}`; closure `{escape_version, record_kind: closure, run_id, state, judgement_basis, supersedes_sequence, escape (copied), attestation}`. 201 `{run_id, audit_sequence, record_kind, state, judgement_basis, pending_reason?, source_event_key?, supersedes_sequence?}`.

**Reader contract.** A birth with no successor is in its birth state; a birth with a successor takes the single closure row's `state` as its effective state.

### Visibility and its interim deviation from ADR-106

`escape_recorded` is withheld from run-bound audit reads (A1.6) on the per-run read and the global feed — and through them every MCP audit read, which proxies those routes — by the entry in `reads.go`'s `withholdScopeOverlapFromRunBound`. That deny-list entry is a **deliberate interim deviation from ADR-106**, owned by E89 [#4537](https://github.com/kuhlman-labs/fishhawk/issues/4537) and guarded by `TestWithholdScopeOverlapFromRunBound_EscapeRecordedWithheld`: it MUST migrate into ADR-106's per-category registry flag when that lands, and the guard test must move with it. Residuals beside that pointer (R9a): the diagnostics bundle and the product-report body render no audit payload and never name `escape_recorded` (escape rows are stage-less, and the failing-surface pick skips stage-less rows); the only trace a run-bound reader sees is the `audit_sequence_range` span, which counts escape rows among the sequences. Audit export needs `read:audit-export`, which a run-bound token never carries.

**Rollback (approval condition C2).** A plain revert is NOT safe: it would drop the `reads.go` withhold and make every pre-revert `escape_recorded` row readable by run-bound tokens on the per-run read and the global feed. Revert everything EXCEPT the `escape_recorded` entry in `withholdScopeOverlapFromRunBound` and its `audit/categories.go` registration, or forward-fix.

### Residuals

- **`delegated` is caller-asserted (R9b, ADR-113).** Only the credential KIND is server-verified; a process holding an admitted human credential that omits `delegated` is indistinguishable from the human, so attestation proves a non-agent-marked forge-verified credential, not presence (A1.3).
- **Caller-chosen strings are a channel (R9b).** `idempotency_key`, `security_class` and `advisory_id` are stored verbatim; the security UUID key, the slug charset and the advisory format narrow what a security row can carry.
- **Mislabelling (A1.5).** A caller can record a security defect under another category; the tool description directs `category=security` for any vulnerability.
- **Agent-recorded births are pending, never counted** (A1.4 supersedes the earlier "counted" reading) and are distinguishable by `agent_recorded: true` / `actor_kind: agent`.
- **Dedupe is per run chain (A1.1).** The same concern recorded against two different runs yields two births; cross-run dedupe by `source_event_key` is E94.4's.
- **A run-less escape is refused by construction** (the route is run-scoped; A1.1).

Tests: `escape_test.go` (every refusal asserts status, code and zero new rows; the C1 trio `TestRecordEscape_SecurityLaterReviewerBornWithReviewedHead` / `_SecurityLaterReviewerReplayDeduplicated` / `_SecurityNonLaterReviewerRequiresUUIDKey`; C3's projection), `escape_pg_test.go` (production repositories: capability wired, re-closure refused, 8-way concurrent closes and births each commit exactly one row, in-transaction target checks, the real repository's wrapped `ErrNotFound` → 422), `audit/deduped_test.go` (the `BeforeAppend` hook).
