# backend/internal/server: Acceptance stage and live validation

Split out of `backend/internal/server/README.md` (the index) on 2026-10-29 so concurrent runs stop colliding on one file. Section text is moved verbatim.

## Acceptance stage seam (E31, ADR-049 / ADR-050)

The acceptance surface spans spec → plan pre-check → dispatch →
runner-shipped signed verdict → outcome ingest → living-anchor render →
deterministic triage.

- **Spec**: the `acceptance` stage type / artifact / `egress` allowance
  (`docs/spec/workflow-v1.schema.json` v1.1–v1.3, semantic bindings in
  `backend/internal/spec/validate.go`). Full runnable example at
  `docs/spec/examples/workflow-v1-acceptance.yaml` — also the
  operator's `.fishhawk/workflows.yaml` companion-commit stanza, since
  the implement agent cannot touch `.fishhawk/**`.
- **Plan gate**: `acceptance_precheck.go::runAcceptancePrecheck`
  (stage-conditional; writes `plan_acceptance_precheck` with a
  `no_blocking_criterion` finding for a criteria-less behavioral plan).
- **Seeded fixtures (dev-only, E72.2 / #3326)**: `devfixtures.go` registers
  `GET /v0/dev/fixtures` (the catalog), `POST /v0/dev/fixtures {scenario}`
  (materializes a named scenario through the run / artifact / audit /
  approval repositories and answers 201 with the FRESH run and stage ids;
  body bounded by `http.MaxBytesReader` at `devFixturesMaxBodyBytes`, 4 KiB,
  so an over-cap body fails the decode with the same 400 `validation_failed`
  shape — `TestDevFixtures_BodyTooLarge400` pins over-cap → 400 and at-cap →
  applier reached, matching the sibling sign route's 32 MiB bound)
  and `POST /v0/dev/sign` (signs a raw trace bundle with the base64
  `private_key` from `POST /v0/runs/{id}/signing-key`, header
  `X-Fishhawk-Dev-Private-Key`, persisting nothing and never logging the
  key) ONLY when `Config.DevFixtures` is non-nil (`fishhawkd
  --dev-fixtures` / `FISHHAWKD_DEV_FIXTURES=1` with a database); a nil
  applier leaves the paths unregistered (router 404, never a "disabled"
  503). Every route is wrapped by `devLoopbackOnly` (403
  `dev_surface_loopback_only` for a non-loopback or unparseable peer). No
  `csrfExemptPath` entry: the csrf middleware passes an identity with no
  SessionID and the routes are called credential-less. The acceptance
  prompt's `### Seeded fixtures` section (`prompt.go::
  writeAcceptanceSeededFixtures`) tells the validator how to drive them and
  that a 404 on the GET is a provisioning fact ⇒ Posture A skip, never a
  failure; the plan-gate classifier's seeded-scenario extension (below,
  under the #3163 entry) reads the same catalog. Contract:
  `backend/internal/devfixtures/README.md`; operator view:
  `docs/acceptance-preview.md` § "Seeded fixtures".
- **Stub forge control surface (dev-only, E72.3 / #3327)**: `devforge.go`
  registers six routes ONLY when `Config.DevStubForge` (a
  `*stub.Forge`, `backend/internal/forge/stub`) is non-nil — `fishhawkd
  --dev-stub-forge` / `FISHHAWKD_DEV_STUB_FORGE=1`, which ALSO points
  `cfg.GitHub` and the registered `gitlab` forge at the same stub through
  its socket-free round-tripper, defaults both webhook secrets to the stub
  constants, and refuses to coexist with a configured GitHub App or GitLab
  token (`serve.go::resolveDevStubForge`, pinned by
  `TestResolveDevStubForge` + the `TestServe_DevStubForge*` boot tests). A
  nil stub leaves the paths unregistered (router 404, never a "disabled"
  503 — `TestDevForge_RoutesAbsentWhenUnconfigured`); every route is
  wrapped by the same `devLoopbackOnly` as the fixture surface
  (`TestDevForge_NonLoopbackRefused` asserts the 403 AND that the stub saw
  zero requests) and needs no `csrfExemptPath` entry. Routes: `GET
  /v0/dev/forge` (`stub.Snapshot`: every record per family in NATIVE state
  words plus the arrival-ordered request log), `DELETE /v0/dev/forge`
  (`Reset` → 204), `POST /v0/dev/forge/issues` (seed, replacing the
  address; 201 the stored copy), `GET /v0/dev/forge/issues?forge=&repo=&
  project_id=&number=` (200 the issue + comments in arrival order; 404
  `stub_issue_not_found`), `POST /v0/dev/forge/pulls` (seed a pull /
  merge request with its merge facts; 201), and `POST
  /v0/dev/forge/deliveries` `{forge, event, delivery_id?, payload}` — the
  load-bearing one: it sends the `payload` bytes VERBATIM (object-ness is
  proven by decoding the top level into `map[string]json.RawMessage`, never
  a `map[string]any` re-marshal, which rounds integers above 2^53 through
  float64 and would deliver a different identifier than the one seeded),
  signs them exactly as the real receiver verifies them (`X-Hub-Signature-256` via
  `stub.SignGitHubDelivery(cfg.GitHubWebhookSecret, body)`;
  `X-Gitlab-Token` = `cfg.GitLabWebhookSecret` verbatim), mints a UUID
  delivery id when absent, and dispatches the request IN-PROCESS through
  `s.Handler()` into a minimal recorder (no `httptest` import in
  production code), answering 200 `{delivery_id, status, body}` with the
  RECEIVER's verdict reported, never mapped. Because both receivers run
  their consumers synchronously, the parent close the E50.6 watcher
  performs is committed before the 200 returns. Every POST body is bounded
  by `http.MaxBytesReader` at `devForgeMaxBodyBytes` (256 KiB) and refuses
  unknown fields; the address is validated ONCE (`devForgeAddress.validate`
  mirrors `stub.validateSeed`) so a bad `forge` / non-positive `number` /
  `github` without `repo` / `gitlab` without `project_id` answers 400
  `validation_failed` naming `details.field` BEFORE the stub sees it
  (`TestDevForge_Validation`, `TestDevForge_Deliver_Validation` — one row
  per branch); an empty receiver secret answers 503
  `stub_forge_webhook_unconfigured` per family
  (`TestDevForge_Deliver_WebhookUnconfigured`). The END-TO-END done-means
  is `TestDevForge_GitHubIssueClosed_ClosesParentThroughRealReceiver` /
  `…GitLabIssueClosed…` / `TestDevForge_Redelivery_LeavesOneComment`: a
  `New(cfg)` server whose ONLY forges are the stub (`cfg.GitHub` over
  `stub.Transport` with `stub.StaticTokens{Value: stub.InstallationToken}`;
  `cfg.ForgeResolver` → a real `*forgegitlab.Forge` over the same
  transport), audit pre-seeded with the `split_children_filed` linkage
  rows, driven ONLY through the control API, asserting the parent's
  COMMITTED state (closed / `completed`, exactly one comment whose last
  line is the `splitfiling.ParentCloseCommentKey` marker, comment-before-
  close on the request log). The `split_parent_closed` observation is read
  back through `GET /v0/audit?category=split_parent_closed` with NO
  `Authorization` header — the credential-free acceptance agent's exact
  observation path — against an audit fake whose `AppendGlobalChained` rows
  land in the list `ListAll` serves and which honours
  `ListAllParams.AccountID` with the documented NULL-allow semantics; the
  watcher writes with a nil `AccountID` (untenanted partition), and
  `handleListGlobalAudit` passes the anonymous identity's empty
  `AccountID` (unconstrained), so the row IS globally visible — an
  established fact, not an assumption. Contract:
  `backend/internal/forge/stub/README.md`; operator view:
  `docs/acceptance-preview.md` § "Stub forge".
- **Ingest + triage**: `acceptance.go::handleShipAcceptance` verifies
  the Ed25519 signature (or an operator bearer), persists an
  `artifact.KindAcceptance` row + an `acceptance_outcome_recorded`
  audit entry, and on a failed verdict runs `triageAcceptanceFailure`
  inline (`classifyAcceptanceFailure` → class 1 fix-up / class 2
  acceptance re-open / class 3-4 paged; bounded; exactly one
  `acceptance_triage_decided` entry).
- **Render**: `issuecomment.RenderStatusBody`
  (`status_template.go::renderAcceptanceOutcomeLine` /
  `renderAcceptanceTriageLine`).
- **Triage-miss corpus + stats**: `acceptance_stats.go`,
  `GET /v0/acceptance-triage/stats`.

**Cross-boundary seam test** (#618 pattern, the E31.10 capstone):
`acceptance_integration_test.go` runs `spec.Parse()` on the committed
example and drives the whole seam over one shared audit store — welding
the example's schema-validity to the suite. Per-slice unit coverage:
`acceptance_test.go`, `acceptance_precheck_test.go`,
`acceptance_stats_test.go`. Runner-side schema↔validator lockstep:
`TestAcceptanceVerdictSchema_LockstepWithValidator`.

## Replayable scenario corpus — backend half (E72.4 / #3328)

The acceptance runner persists each passing drivable criterion as a scenario
YAML under `acceptance/scenarios/` and replays prior scenarios against every
new preview head (`runner/internal/scenario`). The backend half is four seams:

- **Prompt fields** (`prompt.go::fillAcceptanceReplayFields`, acceptance
  stages only, all omitempty): `acceptance_run_branch` +
  `acceptance_pull_request_number` from the reported-head ledger
  (`resolveAcceptanceRunBranchAndPR` — the newest `lineageLedgerCategories`
  entry's `branch`, the newest `pull_request_opened` entry's `pr_number`; 0 =
  unresolvable, NEVER the issue number), `acceptance_criteria` (the EFFECTIVE
  set through `resolveEffectiveAcceptanceCriteria`, fail-open to the plan set)
  and `acceptance_retired_scenarios` (FULL `retiredScenarioEntry` rows with
  `pr` filled from the ledger). The element types are wirecontract-pinned
  ModeExact against `runner/internal/scenario.RetiredEntry` and
  `upload.AcceptanceCriterionEntry`.
- **Retirement request channel** (`acceptance_amendments.go`,
  `checkRetireScenarioAmendments`): `amend_acceptance_criteria` action
  `retire_scenario` (scenario: prefix required, reason required, no
  statement, duplicate / already-retired refused 400, never counted toward
  `acceptance_criteria_all_retired`, permitted with zero criteria). The FULL
  entry is recorded on `approval_submitted` as `retired_scenarios`
  (`retiredScenarioEntriesFor`, `pr` 0 at approval). **Unpersistable
  refusal**: `acceptanceRunnerNeverSpawns` refuses 400
  `acceptance_scenario_retirement_unpersistable` when the plan's shape
  (`DeclaresNoAcceptanceSurface` / `AcceptanceSkippableOutOfScope` /
  `AcceptanceSkippableEmptyCriteria` / `AcceptanceSkippableAllSkipWithBasis`)
  settles acceptance server-side with no runner spawn — the SAME predicate set
  `orchestrator.tryShortCircuitAcceptanceCore` and
  `omitAcceptanceStageForSurfaceNone` branch on;
  `TestAcceptanceRetirementRefusal_PredicateSetEqualsShortCircuitSet` scans all
  three call sites with `go/ast` and fails on divergence, so a later
  short-circuit predicate cannot silently reopen the drop window.
- **Verdict ingest** (`acceptance.go`): `acceptanceBody.Replay` is the
  runner-injected `replay` object (twin of `scenario.ReplaySet`, ModeExact).
  `validate()` PARTITIONS `scenario:`-prefixed rows out of
  `normalizedCriteria` into `scenarioRows`, so every criterion consumer (tally,
  #2581 downgrade, #2512 precedence ladder, triage, concern synthesis) reads
  criterion rows only; `acceptance_outcome_recorded.replay` copies the cap
  header VERBATIM (`acceptanceReplayPayload`; absent → `replay: null`) plus
  scenario tallies and `served_mismatch`; each FAILED scenario row appends
  `acceptance_scenario_regression` attributed ONLY from `replay.scenarios`
  (`recordAcceptanceScenarioRegressions`: no entry or `origin_pr` 0 →
  `origin_pr` absent + `origin_unresolved: true`); `classifyAcceptanceFailure`
  routes a failed scenario row class 1 and never lets a skipped/undecidable
  scenario row reach the class-2/5 skip partition or clamp the run.
- **Reports** (`pullrequest.go`): `acceptance_scenarios_pushed` (acceptance
  stage only; ONE audit entry idempotent on `stage_id`+`head_sha`; status
  comment; NO transition, NO acceptance reopen — the verdict stays bound to
  the dispatch-anchored pre-scenario head while the commit attributes as run
  lineage via `auditcomplete.HeadReportCategoriesByPrecedence` (first) and
  `lineageLedgerCategories`) and `acceptance_scenario_retirement_dropped`
  (audit entry idempotent per `stage_id`+`reason`, enforced under the run-row
  lock via `appendRetirementDropOnce` — see below — + a status comment refresh
  that renders the drop as a LIVE anchor/status-comment surface —
  `issuecomment`'s `activityCategories` admits the kind and
  `renderAcceptanceRetirementDroppedLine` names every dropped scenario id and
  reason unconditionally, see `docs/issue-comment-surfaces.md`).
  `acceptance_scenario_retirement_dropped` has a SECOND writer (#3396): the
  dispatch prompt path (`prompt.go` `fillAcceptanceReplayFields(…,
  recordDrop=true)` → `recordAcceptanceRetirementsUnserved`) appends it with
  reason `approval_chain_unreadable` and `retired: []` when the
  approval-chain read behind the replay corpus fails — the criteria still
  fail OPEN to the plan's full set (#2581 direction unchanged) but the
  approved retirements that read would have named are NOT served, and this
  row is the alertable record of that. Idempotent per stage+reason via the
  same `appendRetirementDropOnce`, actor `system`, best-effort (an append
  error is WARN-logged, never a non-200),
  NO status refresh, and NEVER from `handleGetStagePromptRender`
  (`recordDrop=false`). Both paths WARN-log the named event
  `acceptance_retirements_unserved {run_id, stage_id, error}` — the floor of
  the surface when the store itself is degraded. Pinned by
  `TestPromptResponse_AcceptanceReplay_DegradedChainReadRecordsDrop`
  (`prompt_test.go`), which reads the row back through
  `GET /v0/runs/{id}/audit?category=acceptance_scenario_retirement_dropped`.

  `acceptance_scenario_retirement_dropped` has a THIRD writer (#3389,
  `acceptance_retirement_cancel.go` `recordAcceptanceRetirementsDroppedOnCancel`):
  the run-CANCEL sinks. A run carrying approved retire_scenario entries whose
  acceptance stage never spawned a runner has nobody to report the drop — the
  runner-reported reasons and `approval_chain_unreadable` both start at the
  prompt fetch — so every run-cancel sink reachable AFTER plan approval calls
  ONE helper immediately after its successful cancel transition:
  `handleCancelRun` (`cancel_source: operator_cancel`, the REST verb behind
  `fishhawk_cancel_run`; the idempotent already-cancelled 200 re-enters it and
  appends nothing new), the run-budget tripwire (`trace.go` `checkRunBudget`,
  `run_budget_exceeded`), the BLOCKING stage-budget tripwire
  (`checkStageBudget`, `stage_budget_exceeded`; an advisory breach cancels
  nothing and records nothing), and `orchestrator.completeRun` resolving a
  run to `cancelled` (`stage_cancelled`, the PR-closed-without-merge path)
  through the nil-safe `orchestrator.RunCancelledObserver` seam
  (`Server.OnRunCancelled`) that `server.New` wires exactly like
  `ConsolidatedReview`. The helper reads the run's approved retirements
  through `approvedScenarioRetirements` (the scenario-only projection of the
  `resolveEffectiveAcceptanceCriteria` seam — one shared flatten,
  `scenarioRetirementsFromRecorded`); a run with no retirements costs one
  `approval_submitted` read and appends nothing. **Spawn gate:** it records
  when the acceptance stage is `pending` / `awaiting_host_dispatch` /
  `dispatched` and SKIPS (DEBUG log, no row) when it is `running` or terminal,
  because the signed prompt fetch flips dispatched→running
  (`markStageRunningOnPromptFetch`) in the same handler that serves the
  retirements and the runner arms its deferred reporter the moment
  `FetchPrompt` returns — `running`+ proves the wire delivered them and the
  runner's #3328 window owns the report. `dispatched` is a spawn attempt whose
  fetch may not have landed (the flip is best-effort), so a double report with
  a DIFFERENT reason on that race is the fail-safe direction. The row lands on
  the acceptance stage — or, with no acceptance stage, on the PLAN stage that
  recorded the approval with `acceptance_stage_absent: true` — with payload
  `{run_id, stage_id, retired: [full entries], scenario_ids, reason:
  run_cancelled_before_acceptance, cancel_source, acceptance_stage_state}`,
  actor `system`, idempotent per stage+reason via the same
  `appendRetirementDropOnce`, and UNLIKE
  #3396 is followed by a `notifyOperatorVisible` refresh (#3406, below) so the
  #3392 renderer surfaces every dropped id on the anchor now. On an approval-chain read error
  it mirrors #3396: WARN the named event
  `acceptance_retirements_cancel_chain_unreadable {run_id, cancel_source,
  error}` and append the row with `retired: []` plus an `error` field. An
  append failure is WARN-logged and never unwinds the caller. Pinned by
  `acceptance_retirement_cancel_test.go` (the spawn-gate table over every
  stage state, no-retirement / idempotent / chain-read-error / append-error /
  no-acceptance-stage / nil-repo branches, and
  `TestCancelRun_DroppedRetirement_RendersOnStatusComment` over the real
  routes), `TestCancelRun_RecordsDroppedScenarioRetirements`, the two
  `RecordsDroppedRetirements` tripwire tests in `trace_test.go`, and
  `TestResolveReviewFromPollState_ClosedUnmerged_RecordsDroppedRetirements`
  through the production `server.New` wiring.

  **All three writers route through ONE helper, `appendRetirementDropOnce`
  (`acceptance_retirement_drop.go`, #3439), and the (stage_id, reason)
  idempotency key is enforced UNDER THE RUN-ROW LOCK.** Before #3439 each
  writer listed the category via `ListForRunByCategory` and matched
  `retirementDropAlreadyRecorded` OUTSIDE the append's transaction — a
  check-then-act — so two cancel sinks racing on one run (operator cancel + a
  budget tripwire, or the orchestrator's `stage_cancelled` resolution) could
  each pass the scan and each append, over-reporting one drop as two. The
  helper type-asserts the OPTIONAL `audit.DedupedChainAppender` capability
  (the production Postgres repo; `audit/postgres.go` carries the compile-time
  assertion) and drives `AppendChainedDeduped` with `DedupeSpec{StageID,
  PayloadKey: "reason", PayloadValue: reason}` — `LockRunForUpdate` FIRST,
  then the same-transaction scan, then `AppendChainedTx` — mapping
  `*audit.DedupedDuplicateError` to `(false, nil)` (DEBUG line naming the
  surviving sequence; the writer skips its refresh) and any other error to
  `(false, err)` for the writer's own handling (the cancel and prompt writers
  WARN, the PR route 500s `internal_error` "append audit entry failed",
  unchanged). The key is deliberately (stage_id, reason) and NOT
  `cancel_source`: two sinks racing are ONE drop, while the dispatched-race
  second row with a DIFFERENT reason stays legal. A non-capable repo (the
  in-memory fakes only) takes the prior non-atomic list-then-append leg
  byte-for-byte — a list error is still WARN "proceeding without idempotency
  guard" + proceed — behind a DEBUG "falling back" line; the residual is that
  atomicity holds only for the Postgres repository. No backstop unique index
  is added (rationale in `backend/internal/audit/README.md`). Pinned by
  `acceptance_retirement_drop_test.go` (the `dedupedAuditFake`, the ONLY
  server fake carrying the capability, so every pre-existing test keeps
  exercising the fallback; per-branch helper cases for both legs; the cancel
  writer over three cancel sources → 1 row / 1 refresh; the unserved writer's
  duplicate → no row / no refresh),
  `TestShipPullRequest_AcceptanceScenarioRetirementDropped_CapabilityPath`
  (wire contract unchanged on the capability path), and — the atomicity
  proof on REAL Postgres — `acceptance_retirement_drop_pg_test.go`
  (`_PG_ConcurrentSinksExactlyOne`: 8 racing sinks → exactly one committed
  row; `_PG_CapabilityWired` guards that the production repo carries the
  capability so the race test cannot silently run the fallback), alongside
  `audit.TestPostgres_AppendChainedDeduped_*` on the primitive.

Residual, stated: an operator-invoked `fishhawk_retry_stage` on the settled
acceptance stage AFTER a scenario push sees recorded-head ≠ current-head
(`retry.go` Option C) and is admitted as a stale-head reopen — deliberate
operator action, not an automatic reopen. Terminal-FAILED runs are
deliberately NOT hooked by the cancel writer: `failed` is not absorbing
(`runRetryTransitions` failed→running via revive/redrive), so a drop row at
failure would be a false report once the run is revived and its acceptance
stage persists the retirement; `cancelled` has no retry edge out of it, and
its ONLY exit — the PR-reopen revive (#4082, § "PR-reopen revive" below) —
refuses (`acceptance_retirements_dropped`, guard (g)) whenever this writer
recorded a `stage_cancelled` row at or after the PR close that cancelled the
run, which is what keeps the cancel row safe to write. `applies_to.go`'s `abandonUnauditedOverrideRun`
is not hooked either — it fires at run creation before any approval can carry
a retirement, and its audit store is the thing that just failed.

## Pre-spawn acceptance-dispatch admission (E31.23 / #1928)

`acceptance_admission.go::handleAcceptanceAdmission` —
`POST /v0/stages/{stage_id}/acceptance-admission`, the pre-spawn admission step
a local host dispatch (`fishhawk_dispatch_stage` / `fishhawk_run_stage` /
`fishhawk_drive_run`) calls for an acceptance stage BEFORE it spawns a runner. It
closes the parity gap where the acceptance all-skip / empty-criteria /
out-of-scope short-circuit fired only on the `orchestrator.Advance` retry path,
not at initial host dispatch — so a run whose every acceptance criterion is
`skip_expected`-with-basis spawned a runner that needed a preview and failed
category-C `acceptance_target_unreachable`, a failure the server already knew was
unnecessary.

- **Orchestrator delegate:** the handler calls
  `orchestrator.TryShortCircuitAcceptance(runID, stageID)` — the exported entry
  point that shares the exact predicate/walk/emit core the inline `Advance` arm
  delegates to (so retry-path behavior stays byte-identical). The target must be
  an acceptance stage in a dispatch-admissible state — post-#1936 that set is
  exactly `{pending, awaiting_host_dispatch}`. `dispatched` is deliberately NOT
  admissible: post-#1912 a `dispatched` acceptance stage means the host-dispatch
  marker already stamped a spawn attempt, so short-circuiting it under a live
  runner is the double-drive #1936 closes; a migration-missed legacy `dispatched`
  park degrades to the normal operator-dispatched spawn path (pre-#1928 behavior,
  safe) instead. On a hit the stage is walked straight to `succeeded`, the
  matching audit lands (skip marker for out-of-scope, an
  `acceptance_outcome_recorded` **not-validated** verdict for the other two —
  see below), and `Advance` is re-entered so the run rolls forward.
- **The short-circuit records `not_validated`, never `passed` (#2347).** Both
  verdict-recording predicates settle a stage that verified exactly ZERO criteria
  — no runner, no preview, no observation. Emitting the same `passed`/`accepted`
  words a validator-shipped pass emits made an ABSENCE of verification render as
  certification at every consumer: the merge gate (ADR-049 decision #6), the
  operator's status comment, release evidence. The short-circuit now emits
  `plan.AcceptanceVerdictNotValidated` / `plan.AcceptanceOutcomeNotValidated`
  plus a `criteria_live_validation` count (how many criteria are marked
  `requires_live_validation`, so a skip carrying a tracked operator-validation
  walk — #2338 / #2345 — is distinguishable from one skipped on any other basis).
  `acceptanceGateState` resolves that verdict to the merge-ELIGIBLE
  `acceptance_not_validated` state: eligibility is deliberate, because a change
  with no live target must not be stranded, and a text-matching merge block would
  trade a dishonest pass for a wedge. The distinction is carried by the state
  string, the MCP `next_actions` reason (which asks the operator to acknowledge
  the non-validation in their merge verdict), and a distinct status-comment row —
  a PROMPT, not an enforcement. The verdict is **server-internal only**:
  `acceptanceBody.validate` still rejects any wire verdict other than
  `passed`/`failed`, so a validator cannot forge it, and an already-recorded
  `passed` keeps its exact prior meaning (no migration, no in-flight breakage).
- **Admission ↔ host-dispatch fence (#1936):** the whole read → admissibility-check
  → walk in `TryShortCircuitAcceptance` runs under a per-stage in-process mutex
  (`orchestrator.LockStageAdmission`) that `host_dispatch.go::handleHostDispatchStage`
  also takes across its stage-load → eligibility → CAS. This closes the mid-walk
  race where a client that timed out on the admission POST re-read `pending`/
  `dispatched`, then the host-dispatch marker observed the walk-intermediate
  `dispatched` and returned the idempotent `{transitioned:false}` proceed while
  the walk continued to `succeeded` — a double-drive. Serialized, the marker
  either waits for the walk (then 409s on the settled stage — the MCP verb's
  fail-closed marker handling spawns nothing) or wins the CAS first (then the late
  admission re-reads `dispatched` under the lock and no-ops per the narrowed
  admissible set). The lock map **never evicts** (one mutex per admission-touched
  stage per process lifetime, negligible at v0 volume; eviction under a concurrent
  `LoadOrStore` is a correctness hazard, not a bug). The fence is **single-process
  only** — the multi-replica upgrade is a DB-transactional walk (out of scope; v0
  deploys a single replica).
- **Bounded detached walk (binding condition 1, #1936):** the handler invokes
  `TryShortCircuitAcceptance` under `context.WithTimeout(context.WithoutCancel(r.Context()), acceptanceAdmissionWalkTimeout)`.
  The timeout bounds ONLY the context-cancellable part of the pre-mutation phase
  (the `GetRun`/`ListStagesForRun` admissibility reads). It does **not** bound the
  per-stage `LockStageAdmission` acquisition that precedes those reads — that blocks
  on a plain, non-context-aware `sync.Mutex.Lock()`, so a goroutine parked behind a
  long-held lock waits past the deadline. This degrades safely: once the lock is
  acquired the first admissibility read fails fast on the by-then-expired context,
  so nothing mutates, and the lock hold is itself bounded by the holder's own
  DB/statement timeouts. `TryShortCircuitAcceptance` re-detaches onto
  `context.WithoutCancel` with NO deadline at its **point of no return** (the first
  state transition), so a client disconnect or the handler timeout can no longer
  abort the walk mid-flight. An admission that begins its state walk therefore
  always runs to completion (settle + audit + `Advance`) — nothing changed, or fully
  settled. Individual repo calls stay bounded by their own DB/statement timeouts, the
  honest liveness backstop.
- **Auth mirrors `handleRetryStage`:** authenticated identity required (401
  anonymous), `write:stages` scope gates a token identity (403
  `insufficient_scope`), and an `mcp:run:<uuid>` subject may only admit stages
  within its own run (403 `cross_run_admission`). The endpoint reuses
  `write:stages` and adds NO new scope or audit kind, so the Auth-change impact
  inventory is empty.
- **Fail-open by design (the reconciliation binding condition):** a
  non-admissible stage state (already settled, mixed criteria, an unconfigured
  orchestrator) returns `200 {short_circuited:false}` with NO warning — the
  normal no-op path; the caller records spawn evidence and spawns a runner
  exactly as today. A hit returns `200 {short_circuited:true, kind, basis,
  criteria_total, stage}`. A non-acceptance stage is `422 validation_failed`; an
  unknown stage is `404`.
- **MCP callers fail OPEN only on a TRANSPORT error** (network/5xx → warning +
  spawn as today); `short_circuited:false` never adds a warning. A **4xx
  admission REJECTION** (401 / 403 `cross_run_admission` / 404 / 422) is NOT
  fail-open — the verb HALTS with a tool error and spawns nothing, so a runner
  never executes after the run-subject authorization boundary rejected the
  request. On the 5xx fail-open path the verb ALSO re-checks the target stage
  before spawning: a mid-walk 500 can leave the acceptance stage `running`, and
  an observed non-dispatchable state halts rather than double-driving it. Tests:
  `acceptance_admission_test.go` (endpoint) + the orchestrator's
  `TestTryShortCircuitAcceptance` + the MCP `*_Acceptance*FailsClosed` /
  `*_PostFetchFailure` cases.

## Plan-gate acceptance pre-check (#1533, ADR-049 decision #4)

`acceptance_precheck.go::runAcceptancePrecheck` — called from `handleShipPlan` (`plan.go`) alongside the sibling gates (after `runScopeRegression`, before `runPlanReviews`). The acceptance-criteria sibling of `runScopePrecheck`, shifting an acceptance-quality gap left to the plan gate.

- **Stage-conditional:** `resolveAcceptanceStage` mirrors `resolveImplementConstraints`' spec read (parses `runs.workflow_spec`, finds the first stage with `type: acceptance`) and returns `ok=false` when the spec is absent/unparseable, the workflow is missing, or it has **no acceptance stage** — so a run whose workflow does not configure acceptance produces NO entry and NO block, ever (the issue's off-switch).
- Decodes `verification.acceptance_criteria` from the **RAW plan body** with `json.Unmarshal` — deliberately NOT `plan.Parse`, whose `semanticCheck` rejects duplicate ids (`plan/validate.go`), which would fail-open a duplicate-id plan out of the pre-check before the `duplicate_id` rule could flag it.
- Deterministic rules → `AcceptanceFinding{rule, criterion_id, detail}`: `no_blocking_criterion` (no effectively-blocking criterion — `Blocking == nil || *Blocking` applying the schema default — AND empty `verification.out_of_scope`, the justified-absence escape hatch), `missing_source_ref` (explicit criterion, empty `source_ref`), `missing_rationale` (inferred criterion, empty `rationale` — defense-in-depth; the schema conditional normally rejects this upstream), `empty_id`, `duplicate_id`.
  The rule set is the ONE exported `plan.EvaluateAcceptanceCriteria` (`backend/internal/plan/acceptance_check.go`; `AcceptanceFinding` is a type alias) shared with the intake criteria pre-check — see `backend/internal/refinement/README.md`.
- **`undecidable_criterion` verify_hint exemption (#3163)** — the `undecidable_criterion` rule classified from the criterion STATEMENT alone, so a hermetic in-process test whose statement happened to carry a corpus phrase (the #3163 evidence run: a criterion naming a "signed pull_request webhook delivery" driving the in-process `POST /webhooks/github` route) drew a finding whose remedy told the author to mark a sandbox-decidable check `skip_expected` — the exact anti-pattern the rule set exists to refuse. The fix is a narrow CONJUNCTIVE suppression: a corpus-phrase match is suppressed only when **(H)** the criterion's `verify_hint` — `verify_hint` ALONE, never `expectation_basis`, never the statement (`verifyHintDeclaresInRepo`) — names an in-repository / repository-local harness, AND **(L)** the matched capability is NOT a `liveTarget` corpus entry.
  - **Conjunct L is what preserves #2845.** No in-repository harness can stand up a live forge, deployed environment, or external instance, so those `liveTarget` classes keep firing regardless of the hint; only the external-TRIGGER classes (a live MCP client, a real operator session, a real webhook delivery), which a hermetic in-process test genuinely can fabricate, become suppressible. A liveTarget phrase in the same statement still fires — the suppression `continue`s (does NOT `break`), so a later liveTarget capability in one statement is reached and emitted.
  - **Conjunct H reuses the acceptance executor's OWN doctrine.** `buildAcceptance`'s Posture B (`prompt.go`) sanctions bounded repository-local validation exactly "when the criterion's `verify_hint` names an in-repository / repository-local check", so a hint naming such a harness is positive evidence the sandboxed executor CAN decide the criterion.
  - **The #3016 conjunct-S predicate stays UNSHARED with `verifyHintDeclaresInRepo` — but it is no longer `hasInRepoVerification` alone, and the polarity post-filter it belongs to is now shared across BOTH rules (E72.32 / #3614).** The two predicates still answer different questions, which is why they must not be merged: conjunct S asks whether a polarity-negated live-target assertion's stated verification needs no live target (a both-fields question — either `verify_hint` OR `expectation_basis`), while `verifyHintDeclaresInRepo` asks whether a sandbox-decidable trigger's harness is NAMED in the hint (a hint-only question, because reading `expectation_basis` there would let an unmarked, genuinely-undecidable criterion escape the finding by citing a test). What CHANGED in #3614 is the other half of the separation: conjunct S is now `hasNoLiveTargetVerification`, which ORs `hasInRepoVerification` with a SEPARATE `sandboxLocalVerificationMarkers` list, so `inRepoVerificationMarkers` itself is still untouched and the #3163 / #3326 / #3327 suppression surfaces do not grow. And the post-filter as a whole is now reached from `undecidable_criterion` too (see its own polarity bullet below), via the one shared `polaritySuppressesLiveTarget` helper — sharing the HELPER is what stops the two rules drifting apart on what counts as a negated live target, which is exactly the drift #3614 reported.
  - **Advisory, and the finding Detail is now conditional**: a NON-liveTarget finding that was NOT suppressed appends guidance to name the in-repository harness in `verify_hint` rather than marking it `skip_expected`; the liveTarget Detail is left BYTE-IDENTICAL to the pre-#3163 text so a live-target plan's audit payload bytes do not move.
  - **Honest residual**: the exemption is author-controllable — an author can suppress a genuinely-undecidable non-live-target criterion by writing harness words into `verify_hint` with no such harness existing (the rule is advisory and never refuses a plan, so the fail direction is a missed advisory, the same direction every other residual in this rule set accepts). And a criterion whose STATEMENT quotes a live-target phrase still draws the advisory findings BY DESIGN — the b-shaped case (`TestUnevaluableCriteria_VerifyHintExemption` case b): conjunct L blocks suppression for a liveTarget capability regardless of how in-repository the `verify_hint` reads, so #2845's live-target contract is preserved rather than narrowed. Pinned by the tests above rather than papered over.
  - **Seeded-scenario extension (E72.2 / #3326).** Conjunct H has a second, OR-ed form under the SAME conjunct L: `verifyHintNamesSeedScenario` returns true when some whole token of `verify_hint` (the shared `acceptanceTokens` split — surrounding punctuation trimmed, interior hyphens kept, lowercased first) is a member of `seedScenarioNames` in `backend/internal/plan/acceptance_check.go`, the closed set of names the preview's dev-only `POST /v0/dev/fixtures` route can materialize (`grooming-confirm-gate`, `plan-gate-parked`, `trace-upload-target`, `split-parent-linked`). A hint that names one is positive evidence the sandbox can PRODUCE the state an external-trigger criterion needs, so the finding is suppressed exactly as a named in-repository harness suppresses it — and for the same non-`liveTarget` classes only, so #2845 is untouched (`TestUnevaluableCriteria_SeedHintDoesNotSuppressLiveTarget`). The membership test IS `seedScenarioNames` membership — there is no generic "seed" branch and no route-substring branch, so `seed the run`, `seeded run`, `seedling`, a route mention with an unknown name, `plan-gate-parked-v2` / `xgrooming-confirm-gate`, and a real name in the STATEMENT only all earn nothing (`TestVerifyHintNamesSeedScenario` NO rows; `TestUnevaluableCriteria_UnknownSeedNameDoesNotSuppress`, whose counterfactual mutates the `if seedScenarioNames[tok]` test to `if seedScenarioNames[tok] || true` and lands on the executed line). The set is carried inline so the plan package keeps importing nothing from the repo and cannot cycle with the packages that import it; `TestSeedScenarioNamesMatchCatalog` (a TEST-ONLY import of the name-only `devfixtures/catalog` leaf) binds it to `catalog.Names()` in both directions, so a scenario added to the catalog without the classifier — or vice versa — fails in-loop rather than drifting silently. The audit payload bytes of a plan whose hints name no scenario do not move.
  - **POLARITY AWARENESS for `liveTarget` matches (E72.32 / #3614).** The #3163 suppression above is gated on `!uc.liveTarget`, so before #3614 this rule had NO polarity awareness AT ALL: an absence assertion whose statement says "No live forge, network egress, or deployed environment is required" matched the deployed-environment corpus entry, and because that entry IS a `liveTarget` the verify-hint suppression was structurally skipped — the finding fired and its remedy told the author to mark a sandbox-verifiable criterion `skip_expected`, the very anti-pattern #3163 exists to refuse. The fix reuses the sibling rule's post-filter rather than writing a second one: `if uc.liveTarget && polaritySuppressesLiveTarget(c, statement) { continue }`, the SAME helper `missing_live_validation_marker` calls, so the two rules cannot disagree about what a negated live target is. It uses **`continue`, never `break`** — mirroring the #3163 suppression — so a LATER capability in the same statement still fires (`TestUnevaluableCriteria_PolarityDoesNotSuppressLaterLiveCapability` drives a statement that negates a live forge, corpus entry 4, while naming a webhook delivery, corpus entry 5, and asserts the webhook finding is the one emitted). NON-`liveTarget` capabilities keep their verify-hint / seeded-scenario / stub-forge suppression untouched, and a `liveTarget` match whose statement does NOT negate every anchor keeps firing exactly as before (`_SeedHintDoesNotSuppressLiveTarget`, `_StubForgeHintDoesNotSuppressLiveTarget`). End-to-end at the server seam: `TestShipPlan_AbsenceAssertionCriterionNotFlagged` ships the verbatim run-b58cc883 two-criterion plan through the REAL `handleShipPlan` and asserts the PERSISTED `plan_acceptance_precheck` payload attributes exactly one finding per rule, to the genuinely-live sibling only.
- **`missing_live_validation_marker` (#2845, E54.31)** — the second advisory rule on the same shared evaluator, and NOT a duplicate of `undecidable_criterion`. It flags a criterion whose statement names a LIVE forge/deploy/external **target** but which is not marked `requires_live_validation`. Two matchers, either sufficient: **M1** reuses the shared `unevaluableCapabilities` corpus, restricted to entries flagged `liveTarget` (the live-forge-round-trip and running-external-instance/deployed-environment entries) — corpus REUSE, so no second phrase list can drift, and no phrase string moved, leaving `undecidable_criterion`'s output byte-identical; **M2** is a three-conjunct proximity matcher for named-system prose no fixed phrase list anticipates — a liveness qualifier within 4 tokens of a live-ACTION noun, AND `against` within 4 tokens of an external-target noun, AND no sandbox marker in that against-phrase window.
  - **Exemption is `requires_live_validation` ALONE**, unlike `undecidable_criterion`'s either-declaration exemption. That difference IS the fix: only the marker auto-files the tracked operator-validation walk on plan approval, so a live-target criterion marked `skip_expected`-with-basis alone was exempt from `undecidable_criterion`, drew no finding at all, and silently lost its walk — observed across four runs in #2845.
  - **Deliberate non-coverage** of the MCP-client / operator-session / webhook-delivery capabilities (`liveTarget: false`). The plan artifact schema scopes `requires_live_validation` to a live forge/deploy/external target, "not merely an external trigger event, which `skip_expected` covers" — for those three, `skip_expected` with a basis is the doctrinally complete marking and no walk is owed, so demanding the marker would fire on correctly-authored criteria. Widening is a one-line `liveTarget` flip; the decision is recorded by a control test.
  - **No cross-rule suppression**: a wholly-unmarked live-target criterion draws exactly one finding from EACH rule (one per criterion per rule). Neither rule reads the other's findings; since #3614 both consult the SAME polarity post-filter independently, which is why an unmarked ABSENCE assertion now draws nothing from either — that is each rule applying one shared filter, not one rule suppressing the other. Complementary, not redundant — `undecidable_criterion` says "declare it (either marking)", this rule says "the weaker marking will not suffice here" — and it avoids a two-step in which an author applies the weaker remedy and only then learns it was insufficient.
  - **M1's sandbox-marker negation is scoped to ONE CLAUSE**, not to the whole statement. A whole-statement negation is itself a false-NEGATIVE hole — any stray `fake`/`mock`/`preview` anywhere in a sentence disabled M1 even when the sentence named a genuine live target, recreating the defect the rule closes. So the statement is split at clause punctuation (`,` `;` `:` newline em/en-dash — `.` deliberately excluded, since splitting on it would cut the corpus phrase `against github.com` in half) and a clause carrying a liveTarget phrase and no stand-in **of its own** fires, however many stand-ins its neighbours mention.
  - **POLARITY POST-FILTER (#3016, E32.48; widened and SHARED by E72.32 / #3614)** — a criterion that fired is SUPPRESSED only when BOTH conjuncts hold, and the whole filter now lives in ONE helper, `polaritySuppressesLiveTarget`, called from this rule AND from `undecidable_criterion`'s `liveTarget` branch. The filter runs AFTER a matcher fired, so M1's clause-scoped negation and M2's three conjuncts are untouched, and it runs on a SECOND token stream, `polarityTokens` — `acceptanceTokens` plus two standalone punctuation sentinels (a list separator for a trailing `,`/`;`, a sentence boundary for a trailing `.`/`!`/`?`). `acceptanceTokens` is left BYTE-IDENTICAL, so both matchers still run on the stream they always did.
    - **(P)** EVERY *anchor* in the statement is negated, which as of #3614 means EITHER **(P1)** a negation token within the four-token `livenessProximityWindow` BEFORE it — the original rule, unchanged and evaluated FIRST — OR **(P2)** the anchor falls inside some negator's FORWARD scope: a walk of the SAME four-token budget that RESETS at each list-separator sentinel and TERMINATES at a sentence-boundary sentinel or on exhaustion. **P2 exists because English distributes one negator over a coordinated list**: in "No live forge, network egress, or deployed environment is required" a single `no` governs all three nouns, yet the last sits SIX tokens behind it, so P1 alone judged it un-negated and the whole absence assertion kept drawing findings (#3614). The constant is still REUSED, not redefined — no second bound is introduced — and the change is strictly ADDITIVE: P1 is evaluated unchanged, and a sentinel never SPENDS window budget in either direction, so no window that held before can shrink. Sentinels are zero-cost in `qualifierActionIndices`' conjunct-1 window too, so a comma between a qualifier and its action noun cannot DROP an M2 anchor (a smaller anchor set would make the EVERY-anchor conjunct easier to satisfy — the suppression-widening direction). In multi-token PHRASE ADJACENCY, by contrast, a sentinel is a non-matching token that BLOCKS the phrase: `"live forge,"` still yields its anchor, `"live, forge"` yields none — which keeps the anchor set aligned with `liveTargetCorpusMatch`, whose own `strings.Contains` gate rejects the straddling form too (`TestAcceptancePolarity_AnchorBigramStraddlingComma` pins both directions plus the invariant that a punctuation-free statement yields an IDENTICAL anchor set on either stream).
    - **(S)** the criterion's stated verification method — `verify_hint` and `expectation_basis` JOINED, never the statement — names a verification procedure that needs NO LIVE TARGET (`hasNoLiveTargetVerification`). That is EITHER an in-repository harness (`inRepoVerificationMarkers`: `_test.go`, `pgtest`, `httptest`, `go test`, `fake`, `stub`, …) OR, since #3614, a sandbox-local procedure (`sandboxLocalVerificationMarkers`: `localhost`, `loopback`, `127.0.0.1`, `preview`, `in-sandbox`, `/v0/dev/`, `stub forge`, `no forge` / `no egress` …), carried as a SEPARATE list so `inRepoVerificationMarkers` does not grow and the #3163 / #3326 / #3327 surfaces that also read it are unmoved. **This deliberately reverses #3016's original reasoning FOR THIS CONJUNCT ONLY**, and the reversal is what the argument turns on: #3016 declined to count "validated against the localhost preview" because that is evidence about the acceptance executor's TARGET rather than about an in-REPOSITORY harness — correct for the question "is this verified in-repository?", but that is not the question S asks. S is evaluated ONLY in conjunction with P, and once P holds the STATEMENT itself asserts that no live target is involved, so what remains to decide is "does the stated verification need a LIVE target?" — and a loopback/localhost-preview procedure does not. (`sandboxMarkers` is still NOT reused: it is a matcher-side negation list, and the two must not be conflated.) The motivating #3614 criterion is exactly this shape: its statement says "No live forge, network egress, or deployed environment is required" and its `verify_hint` prescribes a localhost/loopback sandbox check, which the narrower list scored false.
    - **An ANCHOR is the LIVENESS-BEARING token** (`liveTargetAnchors`): for M1, the head token of every `liveTarget` corpus-phrase occurrence, scanned over the WHOLE statement including clauses M1's own marker filter discarded; for M2, every liveness qualifier satisfying conjunct 1. **M2's external-target noun is deliberately NOT an anchor** — in "no real grooming run against this repo's backlog" the liveness claim is carried by `real … run`, while `repo`/`backlog` are bystander objects; requiring a negator adjacent to the object noun would demand phrasing no criterion writes, and the rule would suppress nothing.
    - **#2845 is preserved BY THE CONJUNCTION, not by leaving the rule alone — and that argument is what bounds the #3614 widening.** Conjunct S alone can NEVER suppress, whichever of its two disjuncts is satisfied: a genuinely live-dependent criterion citing an in-repository basis ("validated by the httptest fake forge in `client_test.go`") OR a sandbox-local one ("verified on the localhost preview") has S true and P false and still fires (`TestMissingLiveValidationMarker_InRepoBasisAloneNeverSuppresses` and `TestAcceptancePolarity_SandboxLocalAloneNeverSuppresses` pin the two halves; if the widened list ever became sufficient alone, both go red). Symmetrically, P alone can never suppress: the flagship absence assertion stripped of both fields still fires (`TestAcceptancePolarity_NoStatedVerificationStillFires`). A single un-negated anchor makes P false, so a statement negating one live target while asserting another still fires (`TestMissingLiveValidationMarker_PartiallyNegatedStatementFires` pins EVERY-anchor, not ANY-anchor, semantics). An empty anchor set returns false — the fail direction is toward FIRING, but that claim is exact only for the FULLY-EMPTY set; see the mixed-case residual below.
    - **The finding Detail is now conditional**: it keeps the #2845 remedy verbatim for a genuinely live-dependent criterion and APPENDS the counter-instruction that an in-sandbox ABSENCE assertion must NOT be marked, because marking it skips a criterion the sandbox can verify.
    - **ACCEPTED COMMA-SPLICE RESIDUAL (E72.32 / #3614), pinned not merely disclosed**: P2's budget reset fires on a comma whether the following span is another LIST ITEM or a spliced CLAUSE, so "no live forge, the deployed environment serves the endpoint" suppresses where a parser would not. Telling a coordinate list from a comma splice needs parsing this deterministic word-list matcher deliberately does not do, and the failure direction is an advisory MISS. `TestAcceptancePolarity_AcceptedCommaSpliceExtendsScope` asserts the current behaviour on exactly that phrasing, so a later change to negation scoping is a visible test edit rather than silent drift.
    - **ACCEPTED MISFIRE, pinned not merely disclosed**: a negator inside the window but scoping a different constituent — "the resolver does **not** fail when the **live** forge lister is registered", where `not` sits at exactly distance 4 from the anchor — wrongly suppresses when an in-repository basis is also present. Distinguishing constituent scope needs parsing this word-list matcher deliberately does not do, and the rule is advisory, so the residual fails in the advisory-MISS direction. `TestMissingLiveValidationMarker_AcceptedNegationScopeMisfire` asserts the current behaviour on exactly that phrasing, so a later change to negation scoping is a visible test edit rather than silent drift.
    - **ACCEPTED MIXED-CASE RESIDUAL, pinned not merely disclosed**: `liveTargetCorpusMatch` is a SUBSTRING test while `phraseHeadIndices` requires exact token-sequence equality, so a corpus phrase matching only inside a longer token ("github api" within "github **apis**") contributes NO anchor. When that is the ONLY live-target occurrence the anchor set is empty and the finding fires. But when it coexists with a NEGATED M2 qualifier-action anchor ("**no real dispatch** is attempted while the loader still calls the github apis") the anchor set is non-empty, every COUNTED anchor is negated, conjunct P holds, and an in-repository basis suppresses — despite the un-negated live-target occurrence. Closing it would mean anchoring on substring fragments inside unrelated words or dropping the substring matcher M1 has used since #2845, so the residual is accepted in the advisory-MISS direction and pinned by `TestMissingLiveValidationMarker_AcceptedSubstringAnchorMixedCase`, whose first assertion proves M1 really fires on the statement.
  - **Known residual**, stated rather than papered over: the clause-scoped negation rescues prose whose clause names its own stand-in ("the `github api` client retries in the **fake** transport test") but not a liveTarget phrase in sandbox-validatable prose with no marker at all ("the **deployed environment** config template is rendered"). The only narrowing that would suppress it also drops the true positive "the deployed environment serves the new endpoint", so the residual is pinned by a test instead. A corpus phrase straddling a clause boundary would also be missed; no corpus phrase has that shape.
- **`all_criteria_skip_expected` (#3026, E32.50)** — the THIRD advisory rule on the same shared evaluator, and the only PLAN-LEVEL one. It fires when the plan declares at least one acceptance criterion and EVERY one is `skip_expected` with a non-whitespace `expectation_basis` — the shape under which the orchestrator short-circuits the acceptance stage straight to a `not_validated` verdict with no runner spawn and no preview, verifying ZERO criteria. Because the plan is already known AT APPROVAL TIME to skip acceptance entirely, the finding surfaces on the plan-review gate evidence where it can still be fixed, alongside `undecidable_criterion` and `missing_live_validation_marker`.
  - **ADVISORY — it never refuses a plan.** A change may genuinely have no in-sandbox observable (#2894 is the issue's own honest counter-example), so the rule exists to make the approver ask the question, not to ban the shape. The non-blocking outcome is asserted per refusing consumer: `TestShipPlan_AllCriteriaSkipExpected_IsAdvisoryNotARefusal` (the plan gate admits the plan, the stage reaches `awaiting_approval`, no failure category, the run is alive) and `refinement.TestEvaluateDraftCriteria_AllSkipShapedDraft_NotBlocked` (intake never marks the draft `needs_attention`).
  - **It REUSES `plan.AcceptanceSkippableAllSkipWithBasis`** — the very predicate the orchestrator short-circuits on — rather than re-deriving the condition, so the gate advisory and the runtime short-circuit are the SAME boolean by construction and cannot drift, including on the whitespace-only-`expectation_basis` edge where both agree the plan is NOT all-skip and acceptance dispatches normally.
  - **One finding per PLAN, never one per criterion**, with an empty `criterion_id` — the shape `no_blocking_criterion` already uses for a presence-level condition.
  - **No cross-rule suppression**: an all-skip plan whose criteria name a live target still draws its `missing_live_validation_marker` findings, one per criterion, unchanged.
  - **Where the headline surfaces:** `AcceptancePrecheckPayload.all_skip_short_circuit` (read back off the finding the one evaluator already returned — never a second `AcceptanceSkippableAllSkipWithBasis` call in the server) → `prompt.AcceptancePrecheckEvidence.AllSkipShortCircuit` → one `- ADVISORY all_criteria_skip_expected:` line in the plan-review prompt's Acceptance pre-check block (collapsed from the former CONSEQUENCE-plus-FINDING pair by #3317 — see `backend/internal/prompt/README.md` "All-skip acceptance advisory line"), printed after the `out_of_scope` count, carrying a HANDLING clause that tells the reviewer to acknowledge it in `free_form` rather than record it as a concern, and rendered NOTHING when false so every other plan's gate-evidence bytes are unchanged. The duplicate `- FINDING all_criteria_skip_expected` line the generic findings loop would otherwise also render is suppressed when the flag is true, so the two never both appear; the `plan_acceptance_precheck` audit payload below and the merge-verdict path's "NOT a validated pass" warning (#2347) are both unchanged by this rendering-only collapse. `refinement` intake is reached BY CONSTRUCTION through the shared evaluator with no production edit, but the rule is structurally INERT there: `EvaluateDraftCriteria` maps a child's `[]string` criteria setting only `ID` and `Statement`, never the skip markers, so `AcceptanceSkippableAllSkipWithBasis` is false for every possible draft.
- Writes a `plan_acceptance_precheck` audit entry (payload `AcceptancePrecheckPayload{workflow_id, acceptance_stage_id, findings, criteria_count, blocking_count, out_of_scope_count, undecidable_count, live_validation_marker_count, all_skip_short_circuit, restates_test_count, acceptance_surface_none}`, `findings` an empty array not null) **even when clean** so a reader distinguishes "checked and clean" from "never checked". `undecidable_count` and `live_validation_marker_count` are independent per-criterion headline counts read back off the findings the one evaluator already returned — no second evaluation — and an unmarked live-target criterion increments BOTH. `all_skip_short_circuit` is a plan-level BOOL and its json tag carries **NO `omitempty` deliberately**: this is an additive AUDIT-WIRE change that adds the key to EVERY payload, false ones included, so a machine reader can distinguish "checked, not all-skip" (present-and-false) from "written by a pre-#3026 binary" (key absent).

- **`criterion_restates_test` and `no_observable_criterion` (E72.1 / #3325)** — the fourth and fifth advisory rules on the same shared evaluator (`plan.TestOnlyCriteria` / `plan.EvaluateAcceptanceCriteria`; rule contract in `docs/spec/plan-standard-v1.md`). `criterion_restates_test` fires per criterion whose `verify_hint` names ONLY in-repository Go-test evidence and no operator-observable surface (HTTP route/response/status code, MCP tool result / `fishhawk_*` tool, CLI exit code/stderr/stdout, rendered prompt, persisted audit row); `no_observable_criterion` fires once per plan when at least one criterion exists, the plan is not all-skip, `acceptance_surface` is not `none`, and every criterion is a declared skip or a test-restating one. Both are exempt for `skip_expected`-with-basis / `requires_live_validation` criteria and silent on an empty `verify_hint`. Since E72.22 (#3559 / #3510) the test-only match is POLARITY-AWARE — a marker inside a negated disclaimer ("a rendered file in the checkout, not a Go test") is a denial, not evidence, measured as a negator within 4 tokens BEFORE the marker head so a negation after it still fires — and a THIRD suppressor (not a sixth observable surface) clears the finding when the hint names a concrete non-test rendered file (`.md`/`.mdx`/`.yaml`/`.yml`/`.json`/`.txt`/`.tmpl`, excluding a `testdata` path segment and a `*_test.*` basename, and excluding `.go` so a Go source path never clears it). ADVISORY — neither refuses a plan.
  - **Headlines:** `AcceptancePrecheckPayload.restates_test_count` (one per criterion, read back off the ONE evaluator call — never a second `TestOnlyCriteria` call) and `acceptance_surface_none` (`plan.DeclaresNoAcceptanceSurface` on the raw-decoded verification, so the server never re-spells the enum). Both json tags carry **NO `omitempty`** (the #3026 present-and-false posture: a machine reader distinguishes "checked, zero/false" from "pre-#3325 binary"). Both are a count and a bool — no plan-derived text — so no redaction path is involved (the #3338 hold: a future plan-derived text field routes through the established redaction path, never a second one).
  - **Rendering:** `planGateEvidence` maps both onto `prompt.AcceptancePrecheckEvidence{RestatesTestCount, AcceptanceSurfaceNone}`; the plan-review prompt renders a finding whose Rule is either new constant as an `- ADVISORY <rule>` line with a HANDLING clause (keyed on the Rule CONSTANT, never Detail text — #3317), a `- criteria whose verify_hint names only a Go test (criterion_restates_test): N` count line, and the `- acceptance_surface: none —` headline (`backend/internal/prompt/README.md`). The #1533 seam test `TestShipPlan_ReviewAgents_AcceptancePrecheckReachesReviewPrompt` threads both facts audit-persist → `planGateEvidence` → prompt-render.
- **`blocking_criterion_undecided` (E68.28 / #3057)** — the sixth advisory rule on the same shared evaluator (`plan.BlockingUndecidedCriteria`, wired into `plan.EvaluateAcceptanceCriteria`; alias const `acceptanceRuleBlockingCriterionUndecided`; rule contract in `docs/spec/plan-standard-v1.md`). Fires per criterion that is effectively BLOCKING, carries a non-empty `source`, and whose `verify_hint` is EMPTY or whitespace-only — it names no deciding test and no observable surface at all. It is the deliberate COMPLEMENT of `criterion_restates_test`, which fires on POSITIVE test-only evidence and is silent on an empty hint: the absence case was unguarded by every other rule in the set. Exempt for a non-blocking criterion, for `skip_expected`-with-basis / `requires_live_validation` (the shared `criterionDeclaresUnevaluable` predicate), and for an EMPTY `source` — the last making it structurally INTAKE-INERT, the way the provenance rules are, since `refinement.EvaluateDraftCriteria` never sets a source or a hint. ADVISORY — it never refuses a plan.
  - **Rendering: DELIBERATELY none.** No payload field and no per-rule prompt wiring were added — the finding rides the existing `Findings` array into both the `plan_acceptance_precheck` audit payload and `prompt.AcceptancePrecheckEvidence`, where `writePlanGateEvidence` renders an unrecognised Rule as a generic `- FINDING <rule> (criterion: <id>): <detail>` line. `TestAcceptancePrecheck_BlockingCriterionUndecided_ReachesAuditAndReviewPrompt` (+ its negative twin) is the cross-layer seam test threading plan artifact → shared evaluator → audit persistence → prompt render.
  - **`all_criteria_skip_expected` is SUPPRESSED under `acceptance_surface: none`** (the evaluator gates it on `!DeclaresNoAcceptanceSurface`), so `all_skip_short_circuit` reads false for a `none` plan even though `AcceptanceSkippableAllSkipWithBasis` may be true. The #3026 "same boolean as the orchestrator short-circuit" invariant is deliberately narrowed to "fires when the short-circuit WOULD run": under `none` the stage is omitted at approval (next section), so the short-circuit never runs on the parent. A run that still carries the stage (a decomposition child, or the two partial states below) short-circuits as before — runtime behaviour is unchanged; only the advisory stops nagging about a ceremony the parent will not perform.
- **`criterion_requires_database` (E72.63 / #4095)** — the seventh advisory rule on the same shared evaluator (`plan.DatabaseBackedCriteria`, wired into `plan.EvaluateAcceptanceCriteria`; alias const `acceptanceRuleCriterionRequiresDatabase`; rule contract in `docs/spec/plan-standard-v1.md`). Fires per criterion whose `verify_hint` (alone) names a database- or container-backed check — `pgtest`, `testcontainers`, `_pg_test.go`, `FISHHAWK_TEST_PG_URL`, `db-backed`/`database-backed`/`postgres-backed`, `psql`, `docker run`/`exec`/`compose` — and no operator-observable surface. The acceptance network sandbox admits only the preview target's ports, so such a check fails `connect: operation not permitted` and the criterion lands undecidable as `sandbox_denied_db` (the #4094 shape). The detail names both remedies: name the preview surface that decides it, or declare `skip_expected: true` with an `expectation_basis` citing the committed-tree verify gate. Three suppressors: the shared `criterionDeclaresUnevaluable` declaration, a named observable surface (with `FISHHAWK_TEST_PG_URL` masked first, since it contains the `fishhawk_` surface substring and would otherwise clear itself), and a NEGATED marker (polarity-aware via `anyUnnegatedOccurrence`, like `criterion_restates_test`). No cross-rule suppression: a `pgtest`-only hint draws both rules. INTAKE-INERT by construction — drafts carry no `verify_hint`. Residual: a hint naming a plain `foo_test.go` that happens to be pgtest-backed is not detected at plan time; the acceptance prompt's sandbox-limits section covers it at runtime. ADVISORY — it never refuses a plan.
  - **Rendering: DELIBERATELY none,** exactly like `blocking_criterion_undecided`: no payload field and no per-rule prompt wiring — the finding rides `Findings` into the `plan_acceptance_precheck` payload and renders as a generic `- FINDING criterion_requires_database (criterion: <id>): <detail>` line. `TestAcceptancePrecheck_CriterionRequiresDatabase` (+ its declared-skip twin) is the cross-layer seam test (plan artifact → shared evaluator → audit persistence → prompt render).

## Acceptance-stage omission at plan approval (`acceptance_omission.go`, E72.1 / #3325)

`omitAcceptanceStageForSurfaceNone` — called from `finishApprovalAdvance` (`approvals.go`) on an **approve of a plan stage**, immediately after `advanceForDecision` and **BEFORE the `Orchestrator.Advance` block**, so the orchestrator never observes (and never dispatches or short-circuits) a stage the plan declared moot. When the approved plan declares `verification.acceptance_surface: none`, the run's PENDING acceptance stage is physically DELETED rather than minted only to short-circuit to `not_validated` — the run is genuinely "minted without" the stage, which every stage-ROW consumer (auditcomplete, status comment, `next_actions`, `specStageForRunStage`'s type-ordinal lookup) already handles because a workflow without an acceptance stage is a supported shape. The ONE consumer that reads the workflow SPEC rather than rows is `acceptanceGateState`, hence the gate disposition below.

- **Trigger and pre-conditions, in order (each an early return with ZERO side effects):** nil `RunRepo`/`AuditRepo`; `loadApprovedPlanForRun` error (WARN) or nil plan; `!plan.DeclaresNoAcceptanceSurface` — so for every non-`none` plan the hook performs **no audit read at all** (pinned by `TestApprovePlan_DrivableCriterion_RetainsAcceptanceStage`, which counts `ListForRunByCategory` calls); `RunRepo` not implementing the OPTIONAL `acceptanceStageOmitter` capability (`DeletePendingAcceptanceStage(ctx, id) (bool, error)`, implemented by `run.(*postgresRepo)`; deliberately NOT on `run.Repository`, so the many fakes need no stub) → WARN `repo lacks DeletePendingAcceptanceStage; stage retained`; `ListStagesForRun` error (WARN); no stage with `Type == acceptance && State == pending` (already omitted, already dispatched, or never declared).
- **WRITE ORDER IS DURABLE-RECORD-FIRST, and the ordering is the control.** (1) `AppendChained` the `acceptance_stage_omitted` marker — scoped to the **PLAN stage id**, never the acceptance id, because `audit_entries.stage_id` is `ON DELETE RESTRICT` and the row must outlive the stage; `ActorKind: system`; payload `{omitted_stage_id, omitted_sequence, basis: "acceptance_surface_none", criteria_total, out_of_scope_count}` (ids, counts and a fixed constant — no plan prose, #3338 hold). (2) Only after the marker landed, `DeletePendingAcceptanceStage`. The two reachable partial states are therefore both safe and recoverable: **(i) no marker + stage present** (append failed, or the process died before it) — the run keeps its stage, which short-circuits as today (`out_of_scope` / all-skip); **(ii) marker + stage present** (delete failed — e.g. a future writer scoped a RESTRICT row to the pending stage — or the process died between the writes) — the next approval finds the marker, skips the append and retries the idempotent delete. The stage-less-and-marker-less state (a run the merge gate would wedge at `acceptance_pending` forever) is unreachable BY CONSTRUCTION. `TestApprovePlan_AcceptanceSurfaceNone_AppendFails_StageRetainedNoMarker` is the directional counterfactual: swap the hook to delete-then-append and it reddens (stage gone, no marker) while the happy-path test stays green.
- **Idempotency is anchored on the MARKER, never on the stage's absence:** the marker set is read once per call (`ListForRunByCategory(run, acceptance_stage_omitted)`, decoded on `omitted_stage_id`; an undecodable payload names no stage and is skipped) and matched per pending stage. A re-approval after a fully successful pass finds no pending acceptance stage and writes nothing; a re-approval in state (ii) completes the delete with no second append (`_DeleteFails_MarkerPresent_ReapprovalCompletesWithoutDoubleAppend`, `_Idempotent`). **No lock wraps the list-then-append** — the caller runs behind the approve-advance CAS, which admits exactly one approval into `finishApprovalAdvance` per gate (the loser is refused at `gateActionAdvance` with `run.StageStateChangedError`; `TestApproveAdvanceCAS_ConcurrentApprovals_HooksRunExactlyOncePerFilingPath`), so the append inherits that exclusion. Do not add a second lock here: a redundant lock over a CAS-protected section is how the next reader concludes the CAS is insufficient.
- **Every fail-open branch, each with its own test:** missing capability (`_RepoWithoutOmitter_FailsOpen`), marker-read error → nothing written, stage retained (`_MarkerReadError_FailsOpen`), append error → state (i) (`_AppendFails_StageRetainedNoMarker`), delete error → state (ii) (`_DeleteFails_…`), non-pending acceptance stage untouched (`_NonPendingAcceptance_Untouched`), reject decision never reaches the hook (`TestApprovePlan_Reject_DoesNotOmit`). Best-effort like the sibling approval hooks: the hook never unwinds the approval; every failure WARN-logs under the `acceptance omission:` prefix.
- **Persistence:** `DeletePendingAcceptanceStage` (`run/queries.sql`, `:execrows`, `WHERE id = $1 AND state = 'pending' AND stage_type = 'acceptance'`) — the predicates ARE the refusal (a non-pending or non-acceptance row matches zero rows; `false`, no error), and a RESTRICT-referencing `audit_entries` / `approvals` row makes the DELETE error, returned wrapped. The `db/queries.sql.go` block is HAND-MIRRORED (local `sqlc generate` aborts and regenerates every package; see the preserve-on-regeneration note there) and pinned behaviourally by `TestPostgres_DeletePendingAcceptanceStage_{DeletesPending,RefusesNonPending,RefusesNonAcceptance,RefusedByReferencingAuditRow}` (pgtest).
- **Unshipped acceptance verdict — the reap-failure marker and the NON-merge-admitting `acceptance_verdict_unshipped` gate state (E72.11 / #3447).** Run 8b911565's shape: the runner's trace upload settled the acceptance stage `succeeded`, then the verdict ship 413'd, the runner exited non-zero, and the detached reaper's `POST …/reap-failure` hit the handler's already-terminal no-op — a PASSED verdict silently stranded as settled-outcome-unknown. `reap_failure.go::recordAcceptanceVerdictUnshipped` converts that into a NAMED failure on the UNCONDITIONAL already-terminal path only (the conditional 409 and the mid-flight race branches are untouched): when `stage.Type == acceptance && State == succeeded`, no stage-scoped `acceptance_outcome_recorded` entry is newer than the stage's latest anchor (`max(acceptance_dispatched, acceptance_reopened)` seq), and no LIVE marker exists, it `AppendChained`s ONE stage-scoped `acceptance_verdict_unshipped` entry (payload = the `dispatch_reaper_failed` key set), `notifyStatusUpdate`s, and answers `200 {transitioned:false, acceptance_verdict_unshipped_recorded:true}` (`omitempty`, so every other body is byte-identical). No transition, no Advance, no `dispatch_reaper_failed` row. Every audit read on the path is FAIL-CLOSED: a read error is `500 acceptance_verdict_unshipped_record_failed` so the reaper's bounded retry re-attempts, never the pre-#3447 silent no-op. **Marker liveness is decided by SEQUENCE** (`acceptance.go::acceptanceVerdictUnshippedLive`): with M the newest stage-scoped marker, A the anchor and O the newest stage-scoped outcome, live iff `M > A && M > O` — so a retry re-open (`acceptance_reopened`) + re-dispatch (`acceptance_dispatched`) retires it by construction, as does a verdict that ships after it. `acceptanceGateState` consults it on a TERMINAL acceptance stage BEFORE the verdict switch and returns `acceptanceGateVerdictUnshipped` (= `string(drive.RuleAcceptanceVerdictUnshipped)` = the audit category) when the marker is live AND (no outcome is recorded OR the newest recorded outcome is OLDER than the marker) — so a stale first-attempt `passed` outcome cannot admit a merge over an unshipped re-run. `acceptanceGateAdmitsMerge` is NOT widened (merge_run / delegated merge refuse `409 acceptance_gate_not_passed` with `acceptance_gate_state: acceptance_verdict_unshipped`); `retryAcceptanceOutcomeUnknown` is untouched (the marker never blocks the re-open — that IS the recovery). Drive: `ObserveParkedReviewForDrive` stamps `drive.RuleAcceptanceVerdictUnshipped` / `read_acceptance_audit` idempotently on `LatestRuleIs` and RETURNS (the default arm would stamp `checks_green_awaiting_merge`); `runs.go` surfaces it as `derived_status`. Tests, each with its named deletion counterfactual: `TestReapStageFailure_AcceptanceSucceededNoOutcome_RecordsUnshipped` (delete the branch call → RED), `_AcceptanceSucceededWithOutcomeAfterAnchor_NoMarker` (delete the `outcomeSeq > anchor` predicate → RED), `_AcceptanceUnshipped_DoubleReport_OneMarker` (delete the live-marker idempotence check → RED), `_AcceptanceFailed_NoMarker` + the plan-typed `_AlreadyTerminalNoOp` (delete the type/state gate → RED), `_AcceptanceUnshipped_ReadError_500`; `TestAcceptanceGateState_VerdictUnshipped` table (delete the `markerSeq <= anchor` anchor comparison → the "marker older than a later dispatch anchor" row goes RED), `_VerdictUnshipped_NotMergeAdmitting`, `_VerdictUnshippedMarkerReadError_FailsClosed`; `TestMergeRun_AcceptanceVerdictUnshipped_409`; `TestObserveParkedReview_AcceptanceVerdictUnshipped_ParksNoMerge` (delete the `server.go` case → RED); and the cross-boundary `TestAcceptanceVerdictUnshipped_EndToEnd` (reap → marker → merge 409 → retry re-open → fresh anchor → gate pending → re-run settles with the marker RETIRED → shipped verdict → merge proceeds, through the real handlers on a sequence-stamping audit repo). **Serialization (E72.12 / #3458).** `recordAcceptanceVerdictUnshipped`'s check-then-append (anchor / outcome / live-marker reads + the `AppendChained`) runs under the per-stage `orchestrator.LockStageAdmission(stage.ID)` fence — the SAME single-process mutex `host_dispatch.go` and `TryShortCircuitAcceptance` take (#1936); no lock when `Orchestrator` is nil, as in host_dispatch — and the ship handler's BOTH `acceptance_outcome_recorded` appends (the fresh-create path and the #1396 heal closure) go through `acceptance.go::appendAcceptanceOutcomeSerialized`, which takes the same lock ONLY around the `AppendChained` (a NARROW hold, never across triage / fixup dispatch / `Advance`, which can re-enter the admission walk for the same stage and would deadlock on the non-reentrant mutex). So a concurrent second reap report sees the live marker and appends nothing, and an outcome cannot land between the marker path's read and its append — the interleaving that would leave a LIVE marker over a shipped verdict. Residual: the fence is process-local, so the multi-replica hazard is the one `orchestrator.go` already states. The marker payload is built by the SAME `reapFailureAuditPayload` builder as the `dispatch_reaper_failed` row (`auth_method` from the single `reapFailureAuthMethod` const), so the two key sets cannot drift. Tests: `TestReapStageFailure_AcceptanceUnshipped_ConcurrentReports_OneMarker` (two concurrent reports through a two-party barrier fired AFTER each report's live-marker read on a `hookAuditRepo` over `seqAuditRepo`; delete the reap-path lock → TWO markers, observed `entries = 2, want 1`), `_AcceptanceUnshipped_OutcomeLandingMidWalk_NeverLiveOverShippedVerdict` (a verdict ships through the real `appendAcceptanceOutcomeSerialized` inside the read→append window; delete EITHER lock → `acceptanceVerdictUnshippedLive` reports a live marker over the shipped verdict → RED; each race counterfactual re-run without `-race` per #2586), and `_UnshippedMarkerPayload_SharedWithReaperFailed` (marker key set == the pinned unconditional `dispatch_reaper_failed` key set and `auth_method == reapFailureAuthMethod`; re-inline a divergent map → RED).
- **Merge gate — the FIFTH merge-eligible disposition `acceptance_stage_omitted` (`acceptance.go` `acceptanceGateOmitted`, value = the audit category so the two cannot diverge).** `acceptanceGateState` consults the marker **ONLY when no acceptance stage ROW exists** (the spec still declares one, so the off-switch passed; without this arm every `none` run would wedge at `acceptance_pending` — "not yet materialized"): ≥1 marker → `acceptance_stage_omitted`; none → the existing pending fall-through, unchanged for a run whose stage is genuinely not yet materialized; a marker-read error is PROPAGATED (fail-closed, same posture as the verdict and skip-marker reads). A marker beside a LIVE stage row (state ii) takes the ordinary stage path — the gate never admits a merge on the marker alone while a stage row exists. `acceptanceGateAdmitsMerge` admits it; it is deliberately NOT `acceptance_passed` (nothing was verified — the stage never existed) and NOT `acceptance_not_validated` (decided by a stage that DID exist). The drive presentation's default arm in `server.go` (whose membership is defined as "the set `acceptanceGateAdmitsMerge` admits") falls through for it by construction. Tests: `TestAcceptanceGateState_NoStage_{OmittedMarker_MergeEligible,NoMarker_StillPending,OmittedMarkerReadError_FailsClosed}`, `_MarkerWithStagePresent_UsesStagePath`, `TestAcceptanceGateAdmitsMerge_Omitted`, and the cross-boundary `TestAcceptanceSeam_SurfaceNone_OmittedRunClearsMergeGate` (approval handler → marker + deletion → gate read on the same stores).
- **Residuals, stated:** (a) a decomposition CHILD or recovery child resolves its parent's `none` plan via the parent walk and, carrying no plan stage of its own, never approves — so a child that materializes an acceptance stage keeps it and short-circuits as today; (b) after deletion the review stage keeps its sequence while no acceptance sequence row exists — benign, because orchestrator advancement selects the earliest pending stage and spec resolution is type-ordinal; (c) the MCP `next_actions` classifier reads stage ROWS and reports a stage-less run as not acceptance-gated, so it agrees with the gate without reading the marker; (d) revert `acceptance_omission.go` only after any in-flight `none` run has merged or been cancelled — such a run keeps merging only while the gate arm exists.

### Ship-path acceptance_surface refusal (E72.6 / #3381)

The E72.1 guard — `none` is mutually exclusive with a drivable criterion — lives in `plan.checkAcceptanceSurfaceNone`, called from `semanticCheck`, which only `plan.Parse` reaches. **`plan.Parse` is unreachable on the ship path**: `handleShipPlan` validates through `plan.Validate` (the schema plus only the E83.52 `delivery` rule, never this guard), and `runPlanReviews` reaches `Parse` only with agent plan reviewers AND a reviewer backend, where a semantic failure records `plan_review_failed`, never a 4xx. So a plan declaring `none` beside a drivable criterion shipped 201, was stored, reached the gate, and — because `omitAcceptanceStageForSurfaceNone` keys solely on `DeclaresNoAcceptanceSurface` — approval deleted the acceptance stage that would have verified the plan's own criterion.

- **Control:** `handleShipPlan` calls the narrow exported `plan.CheckAcceptanceSurface(body)` (decodes ONLY `verification`, delegates to the same `checkAcceptanceSurfaceNone` `Parse` runs — the two paths cannot disagree) immediately after `plan.Validate`/coercion succeeds and BEFORE `contentHash`/`GetByHash`/`ArtifactRepo.Create` and every precheck. A non-nil result routes through the shared `refusePlanInvalid` tail the schema-failure path uses: one bounded #646 schema-retry with the guard message fed back as `validation_error` (so the re-plan prompt carries it), then `FailStage(FailureB)` + `advanceAfterFailure` + `400 plan_invalid`. BOTH branches' bodies carry the guard message in `details.error`, which names the offending criterion id — the operator-visible half the REST acceptance criterion depends on.
- **Unconditional:** not gated on plan reviewers or a reviewer backend.
- **Deliberately NOT a `plan.Parse` swap:** the over-cap gate decodes with `json.Unmarshal` so the `semanticCheck` over_cap ⇒ split_proposal coupling cannot pre-empt the count-derived reject (see the cap-gate comment in `plan.go`); that reasoning stands, and no other `semanticCheck` rule is dragged onto the ship path.
- **Committed-state guarantees (placement is the control):** a refused plan leaves NO artifact row, NO `plan_acceptance_precheck` entry, NO `acceptance_stage_omitted` marker, and the acceptance stage row retained and pending; the plan stage never reaches `awaiting_approval`. Because the guard precedes `GetByHash`, a byte-identical re-upload of an artifact stored BEFORE the guard existed is refused rather than returned 200 from the content-hash cache — documented in `docs/api/v0.md`.
- **Fail-closed decode:** `CheckAcceptanceSurface` returns a wrapped non-`*SemanticError` error when the bytes do not decode — impossible for schema-valid bytes, so the branch is defensive; it is treated as a refusal, the direction that keeps the stage-deletion capability gated (unlike the over-cap gate's documented fail-open).
- **Tests (`acceptance_surface_refusal_test.go`, driven through `s.Handler()`):** `_RefusedFailB` (budget exhausted → 400, id in body, stage failed-B, zero side effects), `_RefusedRetryScheduled` (fresh budget → 400 `retry_scheduled:true`, id in BOTH the body and the `plan_schema_retry` `validation_error`, stage re-opened, zero side effects), `_DrivableAfterSkipped_Refused` (the scan does not stop at the first exempt entry), and the controls `_AllSkip_Admitted` / `TestShipPlan_NoAcceptanceSurface_DrivableCriterion_Admitted` (201; the check is inert for every non-none plan). Plan-package layer: `TestCheckAcceptanceSurface_Bytes` including the non-JSON fail-closed row. **Counterfactual pairing:** deleting the `plan.CheckAcceptanceSurface` call reddens the three refusal tests (201, artifact stored, precheck entry present) while `TestParse_AcceptanceSurfaceNone_RejectsDrivableCriterion` stays green — the parser-level pin structurally cannot fail on this defect; stubbing `CheckAcceptanceSurface` to `return nil` reddens the plan-package table AND the handler refusal tests.

### #3026 investigation finding — which surfaces were already discriminable

The issue asked whether a zero-verification acceptance short-circuit could be mistaken for a validated pass. Recording the answer, because two of the four surfaces named needed no change:

- **The audit record was ALREADY discriminable post-#2347.** The recorded verdict/outcome is `not_validated`, never `accepted`, and it carries an `all-skip-with-basis` basis field no validator-shipped verdict ever sets. A reader keying on outcome or basis could already tell the two apart.
- **The PR acceptance HEADLINE was ALREADY discriminable post-#2347.** `renderPRAcceptanceHeadline` gives `not_validated` its own sentence under a neutral ❓ icon ("not validated — 0 criteria verified"), deliberately not the generic `<icon> <outcome> (P/N criteria passed)` tally.
- **The genuinely undistinguished surfaces were the PR MERGE-DECISION sentence and the PLAN GATE** — which is what this change fixes. `renderPRWhatNow` short-circuited on `run.State == succeeded` and printed "run complete — the PR is ready to merge", pre-empting the acceptance-derived arms entirely, so the one sentence a human reads before clicking merge said the PR was ready while zero criteria had been verified; and the plan gate said nothing at all about a shape that was already decided at approval time.
- **The UPSTREAM mechanism pushing plans toward the all-skip shape WAS deferred here and is now FIXED in #3016 (E32.48).** That mechanism is the `missing_live_validation_marker` remedy prescribed for a criterion asserting the ABSENCE of a forge call — see the polarity post-filter above. #2845 is preserved not by leaving the rule alone but by the P-AND-S conjunction: an in-repository basis alone never suppresses, and one un-negated anchor still fires.
- Advisory + fail-open throughout: nil repos, a `GetRun` error, no acceptance stage, or an unmarshal error each returns without blocking/unwinding the upload; an audit-append failure still returns the computed payload.
- The returned payload threads into the plan-review prompt's `### Gate evidence` block (`planGateEvidence` → `prompt.AcceptancePrecheckEvidence`), where a finding inherits the machine-verified "recorded as a high-severity concern, named FIRST" contract. The plan artifact's criteria themselves also render in `writePlanForReview`, and five semantic checklist items (coverage, warrant-of-inferred, testability, independence, falsifiability) are appended to the `### Review criteria` block.
- Audit-kind note in `docs/issue-comment-surfaces.md`.

## Acceptance failure triage (E31.8 / #1536, ADR-049 decision #2)

`acceptance.go::triageAcceptanceFailure` — called from `handleShipAcceptance` ONLY on the fresh-create path (never the idempotent replay, so a re-delivered verdict cannot double-route) and only when `verdict==failed`. **Best-effort relative to the ship:** every internal error WARN-logs and never unwinds the `201`/artifact/`acceptance_outcome_recorded` audit.

**Pure classifier** `classifyAcceptanceFailure(acc, criteria)` → `(class, criterion_ids, reason)`:

- `failure_mode==error` → class 1.
- `assertion_fail` with a non-empty failed set where every failed id resolves to an `explicit`-source plan criterion → class 1.
- Any failed id `inferred`-source or unresolvable against the plan → class 3 (criterion_ids = those ids, the E31.11 per-criterion key).
- No failed but ≥1 skipped where at least one skip LACKS `expectation_basis` → class 2 (ambiguous env/flake).
- No failed but ≥1 skipped where EVERY skip carries a non-empty `expectation_basis` → class 5 (posture-A externally-unvalidatable can't-exhibit, #1671).
- No failed + no skips, or the plan carries no `acceptance_criteria` → class 4.
- Provenance is grounded against `loadApprovedPlanForRun` (nil-tolerant → class 4).

**Routing:**

- Class 1 synthesizes one `[high/acceptance]` `planreview.Concern` per failed criterion from the behavioral evidence (`observed`/`expected`/`steps_taken`/`expectation_basis`/`repro_handle` + the plan statement), or a single envelope concern when the verdict itemized nothing, and routes via the existing `fixupStageAs` under a token-less `Identity{Subject:"system:acceptance-triage"}` (passes `identityHasGateScope`) with `run.FixupOptions.AcceptanceStageID` set.
  The acceptance-driven mode on `run.FixupStage` re-parks the review stage tolerantly and re-opens the settled acceptance stage so the re-dispatched implement → review → acceptance chain re-runs against a fresh preview; disposition `fixup_dispatched`.
- **Class-1 deferral for an in-flight implement review (E72.58 / #4075; `acceptance_fixup_deferral.go`).** `routeAcceptanceClass1` defers ONLY when the pass would be admitted (`countFixupPasses < defaultMaxFixupPasses` — a spent budget still pages `fixup_unavailable_paged` immediately), `implementReviewRoundInFlight` reports the run's latest `implement_review_started` round names this implement stage, is unsettled (`landed < ConfiguredAgents`) and is alive in THIS process (pending boot re-dispatch, or started at/after `processStart` — the restart-blockers reading; a prior-process orphan belongs to the boot sweep), AND the run delegates `may_route_fixup` (`res.Decision(delegation.ActionRouteFixup)` found, nil campaign override, as the merge-candidate route reads it). It then records the non-paging, uncounted `fixup_deferred_review_in_flight` with `fixup_deferral {implement_stage_id, review_round_sequence, concerns}` and routes nothing; an in-flight read error or an undelegated run routes immediately, acceptance-only, as before. **Release** (`releaseDeferredAcceptanceFixup`, under a package `sync.Mutex` — single-process, like review rounds themselves): called at the end of every implement round in `runImplementReviewInvocationsWithConventions` (after concern resolutions/auto-close, BEFORE `recomputeAndPublishAuditComplete`), once by triage right after it writes a deferral (closes the settle-during-write race), and by the BOOT review sweep only (the on-demand reconcile verb stays terminate-only). It acts only when the NEWEST triage entry is the deferral; a terminal run writes nothing; a still-in-flight round no-ops; a `stage_fixup_triggered` on the implement stage newer than the deferral records `fixup_unavailable_paged` + `superseded_by_fixup_sequence` and pages (never a second pass). Otherwise it routes ONE pass through the shared `routeAcceptanceFixupPass` (the same `fixupStageAs` call as the immediate route): the deferred acceptance concerns (provenance `acceptance`, quarantined) plus — only when `may_route_fixup`'s `convergent_concerns` is MET at release — the implement stage's `raised`/`reopened` review concerns as `ConcernIDs` (→ `addressed_pending`), holding `requirement` and `conventions_override_attempt` rows as `held_review_concern_ids`; unmet records `review_concern_fold_unmet`, a concern-list error records `review_concerns_unavailable`, and both still route the acceptance obligation alone. The release writes `fixup_dispatched` (or `fixup_unavailable_paged` on refusal) with `released_deferral_sequence` and `routed_review_concern_ids`. `autoFixup` skips a run whose newest triage disposition is the deferral. Rollback drain: before reverting, let pending deferrals release or cancel their runs. Tests: `acceptance_fixup_deferral_test.go`.
- Class 2 calls `run.ReopenAcceptanceStage` (succeeded → pending, the class-2 verb in `run/acceptance.go` — deliberately NOT `RetryStage`, which operates on FAILED stages, but a valid failed VERDICT leaves the STAGE `succeeded`) then orchestrator `Advance` + `notifyStatusUpdate`; disposition `retry_dispatched`.
- Class 3 / class 4 take NO transition; disposition `paged`.
- Class 5 (#1671) ALSO takes NO transition — disposition `externally_unvalidatable_paged`, a terminal page that keeps the acceptance stage `succeeded` so `fishhawk_audit_complete` clears rather than looping the deterministically-futile class-2 re-run; because it never re-opens the stage it never contributes to the auto-routed count.
- **Post-deploy rollback offer (E35.3 / #1600, ADR-053; `acceptance_rollback_offer.go`).** `decidePostDeployRollback` runs AFTER the unsettled check and BEFORE the re-run budget (the offer is not an auto-route, so the budget must not suppress it). For class 1 or class 4 only, it lists the run's stages; `postDeployRollbackTarget` resolves the VERIFIED deploy (the nearest deploy sequenced before the acceptance stage), the RUN-SCOPED deploy (the lowest-sequence deploy — what `deployStageForRun` resolves for `POST /v0/runs/{run_id}/deployment/rollback`) and whether the run has ANY `implement` stage. It falls through (`handled=false`, existing routing unchanged) on a stage-list error (WARN), with no earlier deploy (`feature_change`), or when an implement stage exists (approval condition 1 on #1600: a mixed `[implement, deploy, acceptance]` workflow keeps the class-1 fix-up route; the offer fires only on the release shape, where class 1 would degrade to `fixup_unavailable_paged`). A verified ≠ run-scoped deploy (#2642 multi-deploy) records `paged`. Otherwise it reads `storedRollbackHandleFor(verified)` and records `rollback_offered` with `rollback_offer: {deploy_stage_id, deployment_artifact_id, rollback_handle}`; a handle read error still offers (empty handle, the reason names it) because the offer has no side effect and the rollback endpoint re-reads and fails closed itself. It NEVER calls `dispatchRollback`, the orchestrator or any transition — the operator fires the rollback or arbitrates (`rollback_offered` is in `acceptanceDispositionPages`, so it pages and is arbitrable). Tests: `acceptance_rollback_offer_test.go` (C7–C12 with their counterfactuals) and the Postgres seam `TestReleaseAcceptance_FailedVerdictOffersRollbackWithStoredHandle_PgBacked` (triage → audit row → no dispatch → the real rollback handler carries the handle).

**Bounds:** `countAcceptanceTriageRoutes` counts prior `acceptance_triage_decided` entries whose disposition auto-routed (`fixup_dispatched`/`retry_dispatched`) — the durable mirror of `countFixupPasses` — with `defaultMaxAcceptanceReruns` = 2. At the cap, or on ANY routing refusal (fixup budget/ceiling exhausted → `fixup_unavailable_paged`, reopen refusal → `retry_unavailable_paged`) or a defensive settle miss (acceptance stage not yet `succeeded` → `unsettled_paged`), the disposition degrades to a paged variant rather than acting, so non-convergence always lands on the human.

**Audit:** ONE `acceptance_triage_decided` chained entry per triage, written AFTER acting (payload `{run_id, stage_id, artifact_id, class, disposition, criterion_ids, failure_mode, prior_routed_passes, reason}`, plus `plan_review_miss` on class 3 and `rollback_offer` on `rollback_offered` — both omitted otherwise, so every other payload is byte-identical; the class/disposition/criterion_ids matching the E31.3 `renderAcceptanceTriageLine` render contract — no `status_template.go` change).

**Paging:** `issuecomment/ping.go::acceptanceTriageNeedsHuman` fires a page-class ping ONLY for the human-needed dispositions (the paged variants, `rollback_offered` included — the ping names class + disposition and never renders the pipeline-supplied `rollback_handle`); the auto-routed ones stay edit-only (the fixup/retry surfaces already render). Category `CategoryAcceptanceTriageDecided`; disposition vocabulary + ping in `docs/issue-comment-surfaces.md`.

## Live-validation walk auto-filing (`live_validation_filing.go`, E48.35 / #2045)

`fileOrLinkLiveValidationWalk`, a best-effort side effect of `finishApprovalAdvance` (fired on a plan approval, same posture as `fileSplitProposalChildren`): when the run's EFFECTIVE acceptance criteria (the approved plan as amended by this approval — see *Effective-criteria fold* below) carry any `requires_live_validation` criterion, it auto-files an operator-validation walk `chore` work item and records a durable audit marker so the pending live check is surfaced on run status / gate view rather than shipped silently unvalidated. Every forge / work-item / audit error logs and returns — it NEVER unwinds the approval. A run with no marked EFFECTIVE criterion — a plan that never carried one, or one whose approval retired every one it did — no-ops with zero side effects.

- **Intent-marker-before-file idempotency + per-run lock.** A durable `live_validation_walk_intent` marker is appended BEFORE the forge call; on entry, if ANY intent marker is already present the hook no-ops (never a second walk). The list-then-append guard is non-atomic, so `lockLiveValWalk(runID)` serializes the intent-check → intent-append → file → linked-append section per run. **That lock is no longer what prevents production double-filing** — since #2656 the approve CAS refuses a raced second approval before either hook is reached, and the lock is retained as a subordinate backstop that still bites only where the CAS is bypassed (a direct caller). See *Approval-hook self-protection: one CAS, one subordinate lock* above for the decision, the measured evidence, and the residual. In-process only (single-daemon v0); a multi-instance deployment needs a Postgres advisory lock, mirroring `childNumberLocks` in `workitems.go`.
- **MANUAL RECOVERY — stranded intent marker (binding condition A(2), #2045).** The idempotency guard leaves one unhandled terminal state: if the process dies (or the forge call is never reached) BETWEEN the intent-marker append and the filing, the newest marker is a bare intent marker with no linked marker following it. The hook deliberately does NOT re-file it — a bare intent marker cannot distinguish crashed-before-file from filed-but-linked-write-failed without a forge query this hook avoids, and re-filing would reopen the double-file window. This rare case degrades to the pre-#2045 status quo: `liveValidationForRun` renders the stranded intent as the file-manually variant (`filing_failed=true`, `filing_incomplete=true` — never a healthy `walk: #X` and never a malformed empty ref), and the **operator files the walk by hand**. A filing-failure linked marker (`filing_failed=true`, empty ref) renders identically.
- **Two arms, one filing — parent under the TRUE epic (#2179).** `resolveWalkParentEpic` resolves the triggering child's sub-issue PARENT via `githubclient.Client.IssueParent` (the parent-direction query added for this, see `../githubclient/README.md`). When the parent's title matches the **CLOSED** form of the conventions' own `epic` type `title_format` — `workmgmt.EpicTitleNumber(conv, title, true)`, derived since ADR-110 guard (i) / #4626 (for this repository's `[E{number}] {summary}` it is exactly the retired `^\s*\[E(\d+)\]`), deliberately DISTINCT from the OPEN form `deriveEpicTitleVar` in `workitems.go` uses, which stops at the digit run and so would ACCEPT a child title like `[E48.35]` and re-introduce the exact mis-parenting this fixes; the closed form requires the format's literal after `{number}` (today's `]`) — the provider can discover a child number, and the epic is not full, the walk is filed with `Relations.ParentEpic = "#<epic>"` and `TitleVars{epic,n}` — `{n}` **allocated by `resolveWalkParentEpic` under a HELD per-epic lock** and passed EXPLICITLY, so `deriveChildNumberTitleVar` short-circuits and does not re-take the lock. It renders `[E<epic>.<n>]` as a genuine child of the epic.
- **Cap decision under the allocation lock (high/concurrency TOCTOU, #2179 fix-up).** The capacity decision and the `{n}` allocation are BOTH made under the SAME per-epic `childNumberLock` `deriveChildNumberTitleVar` takes, and the lock stays HELD across the caller's `File` (`fileLiveValidationChore` releases it with `defer unlockEpic()`). The unlocked pre-count is only a fast reject of an obviously-full epic; the AUTHORITATIVE cap check is a second `EpicChildren` read UNDER the lock. Without this a concurrent filer could add the cap-th child between an unlocked count and the `File`, after which this walk would take the epic arm and fail attachment instead of using the mandatory companion fallback. Pinned by `TestFileOrLinkLiveValidationWalk_FullEpicRace` (pre-count sees 99, the locked read sees 100 → one companion filing).
- **Companion fallback modes.** On ANY resolution failure the walk falls back to the pre-#2179 companion filing byte-for-byte (`{epic}=<issue>`/`{n}=1` → `[E<issue>.1]`, `CompanionTo` the triggering issue), which neither mis-parents nor collides with the real epic's child numbering. The modes: (1) no GitHub client; (2) a zero credential scope; (3) `IssueParent` errors; (4) `IssueParent` returns no parent; (5) the parent title does not match the closed form of the conventions' epic `title_format` (e.g. another child `[E48.35]`, or a conventions set declaring no `epic` type); (6) the provider is **unregistered** (`workmgmt.Get` errors), does not implement `EpicChildrenQuerier`, or the `chore` type carries no `{n}` placeholder, so `{n}` could never be discovered; (7) **the epic is resolvable but UNATTACHABLE** — it already holds the per-parent sub-issue cap (`githubSubIssueParentCap = 100`), its child count could not be read, or its children carry no numbered `[E<epic>.<n>]` form so `{n}` cannot be allocated (#2101). GitHub REJECTS the 101st `addSubIssue` with a GraphQL error (surfaced through `doGraphQL` as `ErrValidation` — that is the forge error that indicates the cap); the mode-7 check degrades to companion BEFORE filing rather than catching that error after the issue already exists (which would leave an unparented `[E<epic>.<n>]` walk). E22 #389, E48 #1940 and E67 #2561 are each at the cap today, so this is a live condition, not a hypothetical (binding condition 4).
- **Single deterministic filing — arm decided BEFORE the one `File` call.** The arm is chosen once, then a single `applyAndFileWorkItem` runs, so the no-double-file invariant is untouched: the earlier two-attempt design opened a same-approval double-file window (a provider 502 AFTER the first `File` created the issue would still trigger the second attempt; the intent marker only dedupes ACROSS approvals). Any error now routes to the `filing_failed` linked marker, never to a second differently-shaped walk.
- **RESIDUAL — two distinct cases (corrected, #2179 fix-up).** The earlier text here overclaimed; the accurate residual has TWO parts. **(1) A provider `CreateIssue` failure (a 502)** is the ONLY fatal `File` step; it routes to the `filing_failed` linked marker (the operator files by hand), NOT to a companion retry — a second attempt within one approval would reopen the double-file window. **(2) The cross-process cap window is NARROWED, not closed.** Allocating `{n}` under the held lock before `File` resolves the resolvable-but-unattachable cases (full, unreadable, non-numbered children) to companion **within this daemon**, so the cap decision no longer fails at file time *for filers this process serializes*. It does **not** close the window against writers this process cannot see — **another `fishhawkd` instance, a human adding a sub-issue in the GitHub UI, or the grooming apply hook** — reaching the cap between our locked read and our `File`. In that window `File` still CREATES the `[E<epic>.<n>]` issue and the epic attach (`Provider.File`'s `linkEpic`/`AddSubIssue`) is **non-fatal**: GitHub rejects the 101st `addSubIssue` as `ErrValidation`, captured as `created.EpicLinkError` (a WARN, not a `File` error), leaving a filed-but-**UNPARENTED** walk — NOT a `filing_failed` and NOT a companion, so binding condition 4's companion-degrade is **not** honored in this residual window. Making it degrade to companion requires **create-then-attach as separate `workmgmt.Provider` steps** (a cap refusal becomes a re-LINK, not a re-FILE that would break the #2045 single-filing invariant); that is a provider-seam change out of #2179's scope and is **DECLINED** here rather than silently substituting the operator-pre-rejected lock-tighter-only design. A filed unparented walk is still a better outcome than a hook that errors, and the residual is rare (an external writer inside the narrow read→`File` window) and visible via the `EpicLinkError` WARN. The in-process lock covers a single daemon; a multi-instance deployment sharing one tracker would need a Postgres advisory lock (mirrors `childNumberLocks`) to close case (2).

### Effective-criteria fold (E72.20 / [#3554](https://github.com/kuhlman-labs/fishhawk/issues/3554))

The walk renders the run's **EFFECTIVE** acceptance criteria, not the plan artifact's text. Before #3554 the hook called `plan.LiveValidationCriteria(approvedPlan.Verification)` directly, so a criterion the SAME approval RESTATED was rendered in its pre-restatement form and one it RETIRED still appeared as a live checkbox — the operator was handed a checklist the approval had already superseded. The orchestrator's acceptance short-circuit already folded the effective set (`EffectiveAcceptanceResolver`, #3181); this hook was the one straggler.

- **One seam, no recomputation.** `effectiveLiveValidationWalkCriteria` makes ONE call to `resolveEffectiveAcceptanceCriteria` (registered as **call site 5** on that seam's enumerated call-site list — a binding condition of the seam) and selects the `requires_live_validation` subset by handing the seam's `Live` set back through `plan.LiveValidationCriteria`, the shared selector, rather than re-implementing the filter.
- **Ordering is what makes this work.** `writeApprovalAudit` persists the `approval_submitted` row carrying `amend_acceptance_criteria` BEFORE `finishApprovalAdvance` invokes this hook (`approvals.go`), so a resolve with `pending=nil` already sees THIS approval's own amendments.
- **RESTATE** → the checkbox carries the restated statement, plus one indented continuation line `Restated by operator approval (audit sequence <N>).` naming the amending approval. `<N>` comes from the additive `effectiveAcceptanceCriteria.RestatedBy` projection (criterion id → the audit `Sequence` of the approval that LAST restated it); the parenthetical is omitted for sequence `0`, a pending amendment that has no recorded row to name.
- **RETIRE** → NO checkbox. The criterion is rendered once, after the live list, as a NON-tickable struck provenance line under a `Retired at approval:` label: ``- ~~`id` — statement~~ — retired by operator approval (audit sequence N): <reason>``. Visible and auditable, but nothing for the operator to tick.
- **Markers follow the effective set.** Both the intent and linked markers' `pending_criteria_count` / `criterion_ids` are derived from the effective live set, so run-status and `gate_view` stop counting a retired criterion as pending.
- **ALL-RETIRED FILES NO WALK** (operator-endorsed behavior change, binding condition 1). When the approval retires EVERY marked criterion the effective live set is empty and the hook takes its `len(wc.Live) == 0` early return: no forge call, no intent marker, no linked marker, and `live_validation` omitted from the run surface. A walk whose every item is struck has nothing for an operator to do. Pinned by `TestFileOrLinkLiveValidationWalk_AllMarkedCriteriaRetired_NoWalk`.
- **FAIL DIRECTION on an audit read error** — the resolve fails, the hook WARNs and renders the plan's FULL live-validation set with no retired/restated annotation (the status-quo pre-#3554 body). Toward MORE validation, never a dropped walk and never a silently narrowed checklist — the same direction the acceptance prompt builder takes on the same error. Pinned by `TestFileOrLinkLiveValidationWalk_AmendmentResolveErrorRendersPlanCriteria`.
- **An UNAMENDED run renders byte-identically** to the pre-#3554 body — no `Restated by` line, no `Retired at approval:` label, not one extra newline. Pinned by `TestFileOrLinkLiveValidationWalk_UnamendedBodyByteIdentical` alongside the existing `TestLiveValidationWalkBody_CompanionByteIdentity`.
- **RESIDUAL — a LATER approval does not rewrite an existing walk.** The fold applies to the filing that actually HAPPENS. Once any intent marker exists the hook is a no-op by design (the #2045 double-file window), so an approval that retires a criterion AFTER a walk was already filed leaves that walk's checkbox standing; the operator resolves it on the issue. Closing this would need the hook to EDIT a previously filed walk, which reopens the idempotency question the intent marker settles.
- **RESIDUAL — a retired criterion's struck line shows its PLAN statement.** `retiredCriterion` carries `{ID, Reason, Source, ApprovalSequence}` and no statement, so the text is joined from the plan artifact by id. A criterion retired after an EARLIER approval restated it therefore renders its ORIGINAL plan statement, not the intermediate restatement. Deliberate: the struck line is provenance and the plan text is the stable reference.

### Rolling walk per epic (#3323)

The epic arm no longer files one walk PER RUN; it batches every run under an epic into ONE rolling checklist issue. On the epic arm `fileOrLinkLiveValidationWalk` looks for an OPEN walk among the epic's children carrying a stable per-`(repo, epic)` idempotency marker (`liveValidationRollingKey` = `workmgmt.MintIdempotencyKey("live_validation_walk_rolling", repo, epicRef)`), and:

- **ADOPTION (append).** When a candidate exists it APPENDS this run's `### Run <run-id>` checklist section (run id, criterion bullets, each criterion's `verify_hint`, the triggering issue ref) to that issue's body via `githubclient.Client.UpdateIssue`. No `File`, no `addSubIssue`, no `{n}`.
- **First filing.** When none exists it FILES a new walk titled `[E<n>.<m>] Operator live-validation walk (rolling)`, body stamped with the rolling key, so the next run under the same epic adopts it.

**Marker-keyed discovery, NOT title matching (ratified deviation).** Discovery asks `workmgmt.BodyHasIdempotencyKey` — a WHOLE-LINE body-marker match, the same helper the split-filing adoption uses — rather than matching the rendered title. The originating issue proposed a title match; the helper's own doc comment warns a substring/prefix title match adopts the wrong child, and an operator retitling the walk would silently orphan it. The SHIPPED title is still pinned by test, so the operator-visible contract holds independently of the discovery mechanism.

**Zero extra forge reads.** The candidate scan reuses the AUTHORITATIVE `EpicChildren` result `resolveWalkParentEpic` already fetches UNDER the held per-epic allocation lock; the whole discover → fresh-read → append critical section runs under that same lock, so two approvals under one epic serialize.

**ADOPTION separated from ALLOCATION (binding condition).** Under the held lock the OPEN candidate scan runs FIRST. A candidate is appendable REGARDLESS of the sub-issue cap and REGARDLESS of whether `{n}` is allocatable — appending needs no new child. Only when there is NO candidate do the cap (`githubSubIssueParentCap`) and `NextChildNumber` failures degrade to the companion arm, exactly as before. This is what stops the feature going inert on the long-lived epics at the cap today (E22 #389, E48 #1940, E67 #2561), where per-run walk sprawl is worst. `walkEpicResolution.ChildN` is legitimately empty on the append path (a resolution with a candidate and no allocatable `{n}` is a legal state); the caller never attempts allocation there and never invents a placeholder child number.

**OPEN-state requirement / empty-State fail direction.** A candidate is adopted only when `EpicChild.State == "OPEN"` (the additive, uppercase-NORMALIZED field). An empty State (any provider that does not populate it — gitlab/jira) is UNKNOWN and NEVER adopts, so the fail direction is the pre-#3323 file-a-new-walk status quo.

**Append arms & UpdateIssue-error routing.** `appendRollingWalkSection` (a) does a FRESH `GetIssue` (the `EpicChildren` snapshot is stale by construction); (b) if the fresh state is not open, or the GetIssue fails, degrades to filing a NEW rolling walk (never a lost walk — at most one extra issue) **when `{n}` is allocatable**, else falls through to the companion arm; (c) if the body already carries this run's section key, writes NOTHING (idempotent re-entry); (d) otherwise `UpdateIssue`s the appended body. An `UpdateIssue` FAILURE on an existing walk routes to the SAME `filing_failed` linked marker as a `File` error and NEVER re-files — a second walk would reopen the #2045 double-file window.

**The append-degrade file-new is cap-gated too (high/untested_edge_path, medium/untested-path fix-up).** The degrade in (b) files a new epic child, so it needs the SAME cap gate as the no-candidate file-new path — otherwise a capped epic whose adopted candidate turns out unusable at the fresh read would file a doomed, filed-but-UNPARENTED `[E<epic>.<n>]` walk (the #2179 residual), where the pre-#3323 behaviour would have filed an attachable companion. `resolveWalkParentEpic` therefore allocates the best-effort append-fallback `ChildN` **only when the epic is under `githubSubIssueParentCap`**; at the cap it leaves `ChildN=""`, so a retryable degrade falls through to the companion arm rather than a doomed new child. The append path ITSELF is never gated by the cap (appending needs no new child) — only its file-new fallback is.

**Cross-process lost-update residual (narrowed, NOT closed).** The append is a read-modify-write (`UpdateIssue` is a whole-body PATCH; GitHub offers no append-to-body endpoint). The per-epic in-process lock serializes it within one daemon; a second `fishhawkd` instance, or a human editing the body concurrently, could still lose an update — the same shape as the #2179 cross-process cap residual above, not claimed closed.

**Surfaces.** The healthy linked marker and `runLiveValidationPayload` gain `checklist_anchor` (`run-<run_id>`); `fishhawk_get_run_status`'s `operator_live_validation` action names both the walk (`walk_ref`) and the section (`checklist_anchor`). The mcpserver mirror `RunLiveValidation.ChecklistAnchor` is pinned by `client_test.go`'s `TestRunLiveValidation_WireShape` (the #371-class hand-maintained-mirror trap).
