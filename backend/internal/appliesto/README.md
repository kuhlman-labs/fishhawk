# `appliesto` — shared `applies_to` routing evaluation core

The pure evaluation core behind a workflow's `applies_to` routing declaration
(E53.3 / #2224–#2226, E53.10 / #2361). No I/O, no HTTP shapes, no persistence —
only the two-phase predicate split, the satisfying-workflow enumeration, the ONE
operator-facing rejection renderer, and the trigger/label `Change` builders.

## Why a package

`applies_to` is enforced at **three** seams:

| Seam | Consumer | Phase | Labels source |
|---|---|---|---|
| `SeamStartRun` | `server/applies_to.go::checkAppliesTo` (`POST /v0/runs`) | admission (`labels`/`trigger`) | caller-attested `issue_context.labels` |
| `SeamWebhook` | `webhook/applies_to_admission.go::refusedByAppliesTo` (`Dispatcher.Handle`) | admission (`labels`/`trigger`) | forge-authoritative `issue.labels[]` |
| `SeamPlanGate` | `server/applies_to_plan_gate.go::appliesToPlanGateRejection` (`handleShipPlan`) | plan gate (`paths`) | the approved plan's `scope.files` union |

The core was extracted verbatim out of `server/applies_to.go` (E53.10) because
the webhook dispatcher needed the SAME phase split and message renderer, and
`server` already imports `webhook` — so the dispatcher cannot import `server`.
Re-implementing the ~250-line core (including an operator-facing governance
renderer both existing seams depend on) a package away is exactly the drift
E53.10 exists to prevent. Parking it in `webhook` (the `CheckBlockingBudget`
precedent) would leave a governance renderer undiscoverable to the next reader
of `server/applies_to.go`. So it is its own package, imported symmetrically.

**Dependency direction is verified by compilation:** this package imports only
`spec` and `run`, and neither imports it — a cycle fails `go build`.

## Surface

- `Phase` (`PhaseAdmission` / `PhasePlanGate`) — which criteria are in play.
- `Seam` (`SeamStartRun` / `SeamWebhook` / `SeamPlanGate`) — ORTHOGONAL to
  `Phase`; selects ONLY the trailing ways-forward sentence of the message.
- `PhasePredicate(p, phase)` — splits a predicate into the phase's sub-predicate
  + a `constrains` bool (false ⇒ admit without calling `Match`).
- `Evaluate(wf, change, phase)` — `(matched, evaluated, err)`; a Match error is
  RETURNED, never swallowed, so callers fail closed.
- `SatisfyingWorkflows(spec, change, phase)` — name-ordered "where can I take
  this instead?" list; a workflow whose predicate errors is EXCLUDED.
- `Rejection` + `RenderRejection(rj)` — the ONE renderer. The binding body shape
  (workflow, failed criterion, observed value, satisfying workflows) is
  identical across seams; only the tail differs. `SeamWebhook` carries NO
  `applies_to_override` (a webhook trigger has no way to pass one).
- `FirstFailingCriterion(sub, change)` — names the specific criterion for the
  message.
- `AdmissionChange(triggerSource, labels)` / `TriggerFormForSource(src)` — build
  the `spec.Change` evaluated at admission. A nil/empty label set is an EMPTY
  label set (fail-closed against a `labels` criterion), never match-all.

## Load-bearing invariants

- **Fail-closed is uniform and asymmetric to the host packages' advisory
  sweeps.** `Predicate.Match` returns `(false, error)` for an empty predicate or
  a malformed glob; every seam treats that as a REJECTION. Writing
  `if err != nil { admit }` by analogy with a package's fail-open advisory sweeps
  is the tempting bug the header comment names.
- **The phase split is exhaustive over the predicate grammar.** `PhasePredicate`
  is a hand-maintained field-by-field copy of `spec.Predicate` whose `constrains`
  check enumerates the four criteria by name. A fifth criterion added by a
  sibling slice (#2227 escalations, #2211 conventions) would be picked up by the
  `applies_to` `$ref` automatically and copied into NEITHER half — declared and
  silently never enforced. `TestAppliesTo_PhaseSplit_IsExhaustive` is a
  `reflect`-based tripwire against the struct that fails on exactly that edit,
  naming all THREE consumers.
- **The admission phase cannot produce a Match error.** The admission
  sub-predicate never carries `paths` (the only error-capable criterion besides
  the empty predicate, which `constrains` filters), so the admission seams'
  Match-error branches are defense in depth, pinned by structural tripwires
  (`server.TestAppliesTo_AdmissionPhase_CannotProduceAMatchError`,
  `webhook.TestAppliesToWebhook_UnevaluablePredicate_FailsClosed`).

## What lives OUTSIDE this package

The HTTP responses, the audit appends (`run_rejected_applies_to`,
`run_admitted_applies_to_override`), the override grant/carry-forward, and the
issue-comment refusal surfaces stay in the adapter files at each seam — this
package decides, the adapters record and respond. See
`backend/internal/server/README.md` and `backend/internal/webhook/README.md`.
