---
title: Plan schema
description: The standard_v1 plan artifact, generated field-by-field from the canonical JSON Schema.
---

A [plan](/fishhawk/concepts/plan/) is a structured artifact validated against a
published JSON Schema. The current version is `standard_v1`: a summary, a file
scope, an ordered approach, a verification strategy, risks and assumptions, and
a rollback plan.

The canonical schema and its reference are
[`docs/spec/plan-standard-v1.schema.json`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/spec/plan-standard-v1.schema.json)
and
[`docs/spec/plan-standard-v1.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/spec/plan-standard-v1.md).

## Generated field reference

<!-- BEGIN GENERATED plan-schema -->

_Generated from the canonical sources by `scripts/gen-site-reference`; do not edit between the markers. Description-only edits to a source are not diffed — the delta tables below compare shape (type, requiredness, enum members, default)._

> **See also:** [Driving a run](/fishhawk/operating/driving-a-run/) — the operator loop these reference surfaces serve. This page is the field reference, not a restatement of that guide.

## Field reference — `standard_v1`

#### standard_v1 — top-level fields

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `plan_version` | string | required | enum: `standard_v1` | Schema version the document was authored against. |
| `ticket_reference` | `ticket-reference` | required |  |  |
| `generated_by` | `generated-by` | required |  |  |
| `summary` | string | required | minLength: `1` | Human-readable description of the change. Rendered as the lead paragraph in the issue comment. |
| `scope` | `scope` | required |  |  |
| `approach` | array of `approach-step` | required | minItems: `1` | Ordered steps the agent intends to take. |
| `verification` | `verification` | required |  |  |
| `predicted_runtime_minutes` | integer | required | min: `1` | Agent's estimate of how long the implement stage will take, in minutes. Used to surface scope problems early and to populate decomposition.sub_plans when the estimate exceeds the implement-stage budget. |
| `predicted_runtime_confidence` | string | required | enum: `low`, `medium`, `high` | Agent's confidence in predicted_runtime_minutes. low = rough guess; medium = reasonably grounded; high = well-understood scope. |
| `risks_and_assumptions` | array of string | optional |  | Optional. Things the agent flagged as uncertain or assumed; surfaces in the review UI. |
| `raw_predicted_runtime_minutes` | integer | optional | min: `1` | Optional (#2862). The planner's PRE-calibration runtime estimate in minutes — the number it arrived at BEFORE multiplying by the fleet calibration factor the plan-stage calibration hint supplied. predicted_runtime_minutes keeps its existing meaning (the CALIBRATED value, so the dynamic implement kill cap and the runtime_observed calibration series are unperturbed); this field carries the raw one alongside it. The implement-budget gate evaluates max(predicted_runtime_minutes, raw_predicted_runtime_minutes), so a sub-1.0 calibration factor can never pull a plan under the budget and dissolve a required decomposition, while a factor above 1.0 can still push one over — calibration applies only in the structure-ADDING direction. Populate it whenever the prompt rendered a calibration hint; omitting it is legal (additive-optional within standard_v1) and leaves the gate reading predicted_runtime_minutes exactly as before, but a hint-bearing plan that omits it draws a plan_warnings advisory because the gate then cannot verify that calibration did not clear the budget. Equal to predicted_runtime_minutes is legitimate (factor 1.0, or no hint rendered). |
| `decomposition` | object | optional |  | Optional. Populated when predicted_runtime_minutes exceeds the implement-stage budget. Stored in the audit log but not acted upon until D3/D4. |
| `model_recommendation` | `model-recommendation` | optional |  |  |
| `surface_sweep_exemptions` | array of `surface-sweep-exemption` | optional |  | Optional (#1544). Machine-readable declarations that a surface-sweep lockstep pattern's named sibling correctly needs no change in this plan — the structured form of the prose 'justify why a sibling needs no change' escape hatch. The plan-gate surface sweep honors a matching (pattern, sibling) entry to suppress the would-be missing-sibling finding, and records the applied exemption in the audit payload + plan-review evidence so a bogus reason stays reviewer-challengeable (never silent). A non-matching or non-firing entry is a harmless no-op. |
| `scope_removals` | array of `scope-removal` | optional |  | Optional (#2516). Machine-readable declarations that a path present in the REVISION BASE plan is deliberately dropped by this revision — the structured form of 'this file is genuinely obsoleted by the constraint'. The plan-gate scope-regression sweep REFUSES a revision that narrows scope without declaring the drop (bounded in-run re-dispatch); a matching entry suppresses the refusal for exactly that path, with the reason surfaced to reviewers as a challengeable justification (never silent). An entry naming a path that was NOT dropped is a harmless no-op. |
| `over_cap` | boolean | optional |  | Optional (#2053). A planner SELF-DECLARATION HINT that scope.files exceeds the resolved implement-stage max_files_changed cap. Advisory only: the plan-gate over-cap advisory is derived SERVER-SIDE from len(scope.files) vs the resolved cap and does NOT depend on this flag — no enforcement or detection path may branch on over_cap to decide whether a plan is over-cap. Set it true as a courtesy when you must emit a single plan whose scope genuinely exceeds the cap; omitting it (or setting it false) never suppresses the server's count-derived advisory. |
| `split_proposal` | object | optional |  | Optional (#2055, E50.3). The ordered-phase split a plan carries when scope.files exceeds the resolved implement-stage max_files_changed cap BY COUNT. A plan over the cap by count MUST carry split_proposal or it is REJECTED server-side (plan_review_failed / category-B) REGARDLESS of the over_cap hint — the AUTHORITATIVE over-cap signal is the server-derived len(scope.files) vs the resolved cap, never over_cap (which remains an advisory courtesy). Each phase carries its own scope.files (intended at or under the cap) and depends_on edges, shaped expand->migrate->contract for compile-atomic changes. Additive-optional within standard_v1 (no major bump). |
| `irreducible` | object | optional |  | Optional (#2412). The structured way for a planner facing a COMPILE-ATOMIC change to DECLINE proposing a split_proposal instead of fabricating one it knows is invalid — PRESENCE of this object (with a non-empty rationale) is the declaration, the schema-idiomatic form of the issue's `irreducible: true`. Three load-bearing semantics: (a) it is the honest DECLINE of a split for a change that cannot be phased into independently-landable at-or-under-cap commits (e.g. a Go method whose receiver base type must live in the method's own package); (b) UNLIKE over_cap (hint-only — no enforcement path may read it) this field IS read by the plan gate: a well-formed irreducible with NO split_proposal converts the count-derived over-cap HARD REJECT into a surfaced, operator-decidable advisory, and it NEVER suppresses the count-derived over-cap advisory itself (that advisory still fires; a SECOND advisory surfaces this rationale as a challengeable claim); (c) it does NOT make the change landable — the implement stage re-checks max_files_changed against the REAL diff, so an approved irreducible over-cap plan still needs a governed spec raise. Mutually exclusive with split_proposal: a plan both declining and proposing a split is contradictory and is rejected by plan.Parse. |

#### standard_v1 — definitions

##### `ticket-reference`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `type` | string | required | enum: `github_issue` | v0 closed set; v0.x adds linear_ticket and jira_ticket. |
| `url` | string | required | format: uri | Canonical URL of the originating ticket. |
| `id` | string | required | minLength: `1` | Stable identifier — e.g. "kuhlman-labs/fishhawk#1247". |

##### `generated-by`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `agent` | string | required | minLength: `1` | Agent identifier matching the workflow spec's executor.agent. |
| `model` | string | required | minLength: `1` | Specific model used (e.g. claude-opus-4-7). |
| `timestamp` | string | required | format: date-time | RFC 3339 timestamp of generation. The runner wall-clock at agent invocation. |
| `version` | string | optional |  | Optional. Model version or build SHA when agent reports it. |

##### `scope`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `files` | array of `scope-file` | required | minItems: `1` | Files the agent intends to create, modify, or delete. |
| `estimated_lines_changed` | integer | optional | min: `0` | Rough estimate. Used by reviewers to gauge change size; not enforced. |

##### `scope-file`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `path` | string | required | minLength: `1` | Repo-relative path. |
| `operation` | string | required | enum: `create`, `modify`, `delete` |  |

##### `approach-step`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `step` | integer | required | min: `1` |  |
| `description` | string | required | minLength: `1` |  |

##### `verification`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `test_strategy` | string | required | minLength: `1` | How the change will be tested. Free-form prose; reviewers expect concrete tests, not just "add tests". |
| `rollback_plan` | string | required | minLength: `1` | How a regression would be undone. "Revert PR" is a valid answer for purely-additive changes; data migrations need more. |
| `acceptance_criteria` | array of `acceptance-criterion` | optional |  | Optional (ADR-049 #3, E31 wave 0). The structured, provenance-tagged acceptance-criteria contract. Each criterion's id is the join key threaded across plan → acceptance execution → evidence → triage → feedback. Annotated x-intended-required: a future standard version promotes this to required after an E31 soak; additive-optional today. |
| `out_of_scope` | array of string | optional |  | Optional. Statements of what this change deliberately does NOT cover, so reviewers and downstream acceptance don't treat an omission as a gap. |

##### `acceptance-criterion`

One acceptance criterion within verification.acceptance_criteria. The id is the unique join key across plan → acceptance execution → evidence → triage → feedback.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `id` | string | required | pattern: `^[a-z0-9][a-z0-9-]*$` | Slug identifier, unique within acceptance_criteria. The join key that ties this criterion to its downstream execution, evidence, triage, and feedback records. |
| `statement` | string | required | minLength: `1` | The acceptance criterion itself — what must hold for the change to be accepted. |
| `source` | string | required | enum: `explicit`, `inferred` | Provenance: explicit = stated in the ticket/spec; inferred = the agent derived it. An inferred criterion must carry a rationale. |
| `source_ref` | string | optional |  | Optional. Pointer to where an explicit criterion came from (e.g. an issue anchor or spec section). |
| `rationale` | string | optional |  | Why the agent inferred this criterion. Required when source = inferred (enforced by the if/then conditional); optional otherwise. |
| `blocking` | boolean | optional | default: `true` | Whether failing this criterion blocks acceptance. Defaults to true; downstream consumers apply the default when omitted. |
| `verify_hint` | string | optional |  | Optional. A hint to the acceptance executor on how to verify this criterion. |
| `preconditions` | array of string | optional |  | Optional. Preconditions that must hold before this criterion can be verified. |
| `skip_expected` | boolean | optional |  | Optional (#1748). Marks a criterion the acceptance agent cannot validate against the localhost preview — its trigger needs an external event the default-deny egress sandbox cannot produce. When true, expectation_basis is required (enforced by the presence-aware if/then conditional). A plan whose EVERY criterion is so marked short-circuits acceptance dispatch to a not_validated verdict (basis all-skip-with-basis), NEVER a passed one (#2347): the short-circuit verified ZERO criteria, so the recorded verdict says so — merge-eligible, but not certified. Omitted on a legacy criterion never triggers the then-branch. |
| `expectation_basis` | string | optional | minLength: `1` | Required when skip_expected is true (#1748). Cites where the criterion's expectation is actually validated — e.g. the integration / end-to-end test with a fake that exercises the externally-triggered behavior. |
| `requires_live_validation` | boolean | optional |  | Optional (#2045). Marks a criterion whose TRUE verification needs a LIVE forge/deploy/external target the default-deny sandbox lacks (not merely an external trigger event, which skip_expected covers). When true, the criterion should ALSO be marked skip_expected with an expectation_basis so acceptance short-circuits via all-skip-with-basis — recording a not_validated verdict, never a passed one (#2347) — instead of dispatching an unvalidatable live-target criterion to the acceptance agent. That short-circuit's acceptance_outcome_recorded payload carries criteria_live_validation, the count of criteria marked here, so a skip with a tracked operator-validation walk is distinguishable from one skipped on any other basis. On plan approval the system auto-files-or-links an operator-validation walk so the live check is tracked rather than shipped silently unvalidated. Strictly additive: omitted on a legacy criterion changes nothing. |

##### `sub-plan-summary`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `title` | string | required | minLength: `1`; maxLength: `200` | Short title for this sub-plan. Must be unique within decomposition.sub_plans. |
| `scope_hint` | string | required |  | Brief description of what this sub-plan covers. Helps the reviewer understand the split. |
| `predicted_runtime_minutes` | integer | required | min: `1` | Agent's estimate of how long this sub-plan's implement stage will take. |
| `predicted_runtime_confidence` | string | required | enum: `low`, `medium`, `high` | Agent's confidence in this sub-plan's predicted_runtime_minutes. |
| `scope` | `scope` | optional |  | Optional. The files THIS sub-plan's slice will touch. When present, the decomposition fan-out child for this sub-plan narrows its run scope (scope_handoff commit bounding and scope-drift detection) to these files instead of inheriting the parent plan's full scope.files. Omitted means the child inherits the parent's full scope. |
| `depends_on` | array of integer | optional |  | 0-based indices of sibling sub_plans this slice depends on; run_children dispatches in topological waves (omitted/empty = no dependency, first wave). |
| `model_recommendation` | `model-recommendation` | optional |  | Optional. The model recommendation for THIS sub-plan's decomposition child. Resolved through the same chokepoint as the top-level model_recommendation, so a decomposed child ratifies its own slice's recommendation at its plan gate. Omitted means the child has no per-slice recommendation. |

##### `split-phase`

One phase within split_proposal.phases (#2055). Carries its own scope.files (intended at or under the resolved implement-stage max_files_changed cap so each phase ships as its own within-cap plan) and depends_on edges; phases are shaped expand->migrate->contract for compile-atomic changes.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `title` | string | required | minLength: `1`; maxLength: `200` | Short title for this phase. Must be unique within split_proposal.phases. |
| `scope` | `scope` | required |  | The files THIS phase will touch. Intended at or under the resolved implement-stage max_files_changed cap so each phase ships as its own within-cap plan. |
| `scope_hint` | string | optional |  | Brief description of what this phase covers. Helps the reviewer understand the split. |
| `depends_on` | array of integer | optional |  | 0-based indices of sibling phases this phase depends on; mirrors decomposition.sub_plans depends_on. Shaped expand(0) <- migrate(1) <- contract(2) for the canonical compile-atomic split. |

##### `model-recommendation`

Optional. The agent's complexity-informed recommendation for which model should execute the implement stage (#1013). Advisory: the operator ratifies or overrides it at the plan-approval gate, and the resolved model is validated against the deployment's per-adapter allowed-model set. Omitted means no recommendation — the implement-model resolver falls through to the workflow spec's executor.model, then the deployment default.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `implement_model` | string | required | minLength: `1` | Model identifier the agent recommends for the implement stage (e.g. claude-opus-4-8). Routed to the runner adapter as the model override when this rung wins the resolution ladder (deployment default < spec executor.model < plan model_recommendation < operator gate decision). |
| `rationale` | string | required | minLength: `1` | Why this model fits the assessed complexity. Rendered alongside the recommendation in the plan-review surface. |
| `complexity_assessed` | string | required | enum: `low`, `medium`, `high` | The agent's assessment of the change's complexity, informing the recommendation. Stamped onto calibration history so per-model-per-complexity outcomes accumulate. |

##### `surface-sweep-exemption`

One machine-readable surface-sweep exemption (#1544): a declaration that the named lockstep pattern's named sibling correctly needs no change in this plan. The plan-gate surface sweep suppresses the missing-sibling finding only for a sibling actually absent from scope.files whose (pattern, sibling) matches; the applied exemption is surfaced to reviewers as a challengeable justification.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `pattern` | string | required | minLength: `1` | The surface-sweep pattern's name, exactly as surfaced in the plan-gate surface-coupling sibling map (e.g. "actor @-mention render surfaces"). |
| `sibling` | string | required | minLength: `1` | Repo-relative path of the pattern sibling that correctly needs no change in this plan. |
| `reason` | string | required | minLength: `1` | Why the sibling correctly needs no change. Rendered to plan reviewers as a challengeable justification, so a bogus reason is not silent. |

##### `scope-removal`

One machine-readable scope removal (#2516): a declaration that the named path, present in the revision base plan, is deliberately dropped by this revision. The plan-gate scope-regression sweep suppresses the undeclared-narrowing refusal for exactly this path; every OTHER dropped path still refuses. The reason is surfaced to reviewers in the plan artifact as a challengeable justification.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `path` | string | required | minLength: `1` | Repo-relative path, present in the revision base plan's scope, that this revision deliberately drops. |
| `reason` | string | required | minLength: `1` | Why the revision constraint genuinely obsoletes this path. Rendered to plan reviewers as a challengeable justification, so a bogus reason is not silent. |

<!-- END GENERATED plan-schema -->
