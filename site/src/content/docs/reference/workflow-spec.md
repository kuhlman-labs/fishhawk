---
title: Workflow spec
description: .fishhawk/workflows.yaml — the live major, and what an author of a frozen-major document needs to know.
---

`.fishhawk/workflows.yaml` declares the workflows for the repository it sits in.
This page covers all three live majors on purpose: `workflow-v0`, `workflow-v1`
and `workflow-v2` are simultaneously supported off a single `main`, so a
per-major page set would be three pages describing one product. The [support
table](/fishhawk/reference/#workflow-spec-major-support) is the status of each;
[versioning](/fishhawk/reference/versioning/) records why it is laid out this
way.

**Write new specs at `workflow-v2`.** The rest of this page describes v2 unless
it says otherwise; the [frozen majors](#the-frozen-majors-v0-and-v1) section
below is what a `0.x` or `1.x` author needs.

## The shape of a document

```yaml
version: "2"            # required — the single token "2"

defaults:               # optional — file-level executor / reviewers / budget
  executor: {...}

workflows:              # required — at least one
  feature_change:
    description: Default workflow for feature work.
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvals:
              count: 1
              not: [author, agent]
```

`version` is required and decides which schema validates the document. At v2 it
is the bare token `"2"`: **v2 has no minor chain.** v0 carried an additive
`0.3`–`0.7` chain and v1 a `1.0`–`1.6` chain, and both gated field acceptance on
the declared minor. At v2 a field is accepted because the schema declares it.

## What a stage declares

| Key | What |
|---|---|
| `id`, `type` | The stage's name and kind (`plan`, `implement`, `review`, …). |
| `executor` | `agent:` for an agent stage, `human: true` for a human one. `verify:` names the test command run before the pull request opens. |
| `inputs` | A `github_issue`, or an `artifact` `from_stage:` an earlier stage. |
| `produces` | The artifact this stage must emit — `plan`, `pull_request`, … |
| `constraints` | Rules checked against the real diff. See [constraint](/fishhawk/concepts/constraint/). |
| `gates` | Approvals that must be satisfied before the run advances. |
| `reviewers` | Advisory or gating review, by agents and/or humans. |
| `budget` | Token and runtime limits, advisory or enforced. |

## v2-only grammar

Three things exist only at v2, and are the reason to prefer it:

- **Same-document reuse.** File-level and workflow-level `defaults`, plus
  workflow `extends`, resolved before schema validation — so a stage can inherit
  its executor rather than repeating it.
- **Shape normalization.** `constraints` as an object, `auto_advance`, and the
  `needs:` shorthand, rewritten at parse time into the older representation.
  Nothing downstream changed; the document just reads better.
- **Autonomy grammar.** An `autonomy:` tier shorthand plus a per-action-class
  matrix (`report` / `gated` / `auto`), which replaced v1's `operator_agent`
  block.

v2 also *removes* several v0/v1 back-compat forms. Each removed or renamed form
is rejected with a message naming its replacement, so a v1 document pasted into
a v2 file fails loudly rather than silently meaning something else.

## The frozen majors: v0 and v1

If you maintain a document that declares `version: "0.x"` or `"1.x"`, this is
what applies to you:

- **It keeps working, with no action.** Both schemas stay embedded, compiled,
  routed and validated. That is pinned by a test, not by a convention.
- **Nothing rejects a new `0.x` or `1.x` document either.** It is accepted; it
  is discouraged, because the reuse, shape and autonomy grammar above exists
  only at v2 and `fishhawk init` emits v2 presets.
- **No removal date is set, and none is planned.**
- **To move, run `fishhawk migrate-spec`.** It translates an existing v1
  document and prints an approval-eligibility report rather than rewriting
  blind. The field-by-field migration path is in
  [`docs/spec/workflow-migration.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/spec/workflow-migration.md).

A document declaring a major above 2 fails closed — it is not routed to the
newest schema on the assumption that newer is compatible.

## Validating

```sh
fishhawk validate ./.fishhawk/workflows.yaml
```

Validate with the CLI or the backend rather than with a bare JSON Schema
runner: a v2 document using `defaults` or `extends` needs same-document reuse
resolved before the schema sees it, and a bare validator rejects a spec the
product accepts.

## The canonical reference

Every field of every major is documented in
[`docs/spec/`](https://github.com/kuhlman-labs/fishhawk/tree/main/docs/spec),
alongside the JSON Schemas themselves. That is the source of truth; the
orientation above is hand-written; the field reference below is generated from
those schemas and drift-gated.

## Generated field reference

<!-- BEGIN GENERATED workflow-spec -->

_Generated from the canonical sources by `scripts/gen-site-reference`; do not edit between the markers. Description-only edits to a source are not diffed — the delta tables below compare shape (type, requiredness, enum members, default)._

> **See also:** [Driving a run](/fishhawk/operating/driving-a-run/) — the operator loop these reference surfaces serve. This page is the field reference, not a restatement of that guide.

## Field reference

### `workflow-v2`

The live major — write new specs here.

#### workflow-v2 — top-level fields

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `version` | string | required | enum: `2` | Spec syntax version. workflow-v2 advertises the single token "2" and has NO minor chain (unlike v0's additive 0.3…0.7 and v1's 1.0…1.6 minor chains). Declare exactly "2": the validator routes a spec to this schema when its version major is 2, and the dotted minor forms ("2.0", "2.1", …) are REJECTED by this enum. Field acceptance under v2 is by schema declaration, not by a declared minor. Old specs in the audit log remain readable forever via their historical schemas; this enum gates *new* v2 specs only. |
| `workflows` | object | required |  | Named workflows. Keys are snake_case identifiers (e.g. feature_change). At least one workflow must be defined. |
| `defaults` | object | optional |  | FILE-LEVEL reuse defaults (E52.4 / #2216): the lowest rung of the same-document resolution ladder — file defaults -> extends base -> workflow defaults -> the stage's own declaration, later winning. Applied to every stage of every workflow in this document before schema validation, so an inherited executor satisfies $defs/stage's required list with no schema relaxation. MERGE SEMANTICS: `executor` and `budget` merge KEY-WISE (the receiving side wins per key) because they carry only execution parameters; `reviewers` is taken WHOLE from exactly one rung and is NEVER blended, because it determines review AUTHORITY (ADR-027) and a supplemented `human` key would silently convert a gating stage into an advisory one. Arrays REPLACE wholesale everywhere — a governance file never accumulates an approver or reviewer its author did not write. Cross-FILE inclusion (`include:`) is deliberately out of scope (ADR-067). This block is INLINED here and on $defs/workflow rather than factored into a shared $defs entry. That was originally FORCED: the interim v1->v2 copy-fidelity allow-list capped licensed divergent paths at ~15, and a shared $def would have spent a fourth. That check and its cap are RETIRED (#2320), so the duplication is now a free CHOICE rather than a constraint — it is kept because changing it would be churn, not a fix. Either way, the two inline bodies MUST be kept in sync: an edit to one is an edit to both. |
| `test_conventions` | array of `test_convention` | optional |  | Optional per-repo test-location conventions that generalize the plan-gate test sweep (#1004) beyond the built-in Go (name.go -> name_test.go) and colocated-TypeScript defaults. Each entry maps production files matching a glob to candidate test-file path templates. Declared entries are ADDITIVE to the built-in defaults (Go + colocated TS stay covered regardless), so a repo typically declares only its Python / Ruby / parallel-tree conventions. Advisory-only and fail-open: the sweep never blocks a plan. Declared by workflow-v2; accepted whenever present (v2 has no minor chain — a field is accepted because the schema declares it, not because the document declared a high enough minor). |

#### workflow-v2 — definitions

##### `test_convention`

One test-location convention: a production file whose repo-relative path matches `match` is expected to have a test at one of `candidates`. The plan-gate test sweep (#1004) flags an existing candidate test that is absent from the plan's scope.files.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `match` | string | required | minLength: `1` | Doublestar glob (bmatcuk/doublestar/v4 semantics; `**` crosses directory separators) matched against the repo-relative production-file path, e.g. `**/*.go`, `**/*.{ts,tsx}`, `src/**/*.py`. A scoped file whose basename also matches a candidate's test-file shape is treated as a test, not a production file. |
| `candidates` | array of string | required | minItems: `1` | Candidate test-file path templates for a matched production file. Template variables: `{dir}` (the production file's directory), `{name}` (basename without its final extension), `{ext}` (final extension without the leading dot), `{relpath}` (full repo-relative path without its final extension). Examples: `{dir}/{name}_test.go`, `tests/test_{name}.py`, `spec/{relpath}_spec.rb`. The sweep reports a candidate that exists on the base ref but is missing from scope.files. |

##### `workflow`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `stages` | array of `stage` | required | minItems: `1` |  |
| `description` | string | optional |  |  |
| `applies_to` | `predicate` | optional |  | Optional routing declaration (E53.3 / #2226): the changes this workflow may be used for. It is a $defs/predicate (E53.1 / #2224) — the SAME match rule `escalations` and the review-conventions consume, deliberately not a second matcher. A workflow declaring no `applies_to` accepts any change, which is what keeps every pre-#2226 document unaffected. Enforcement is fail-closed and runs in TWO PHASES, each at the earliest point its criterion has a PRODUCER: `labels` and `trigger` are evaluated at run admission (POST /v0/runs), `paths` at the PLAN GATE against the approved plan's scope.files. `change_kind` is REJECTED inside `applies_to` by the backend and by `fishhawk validate` — no producer emits a change kind, so a workflow declaring it could never be selected, which is indistinguishable from the feature being broken; $defs/predicate itself keeps the criterion for its other consumers. `applies_to` FILTERS an operator-named workflow_id, it never selects one, so two workflows with overlapping predicates is benign rather than a coin flip. BOTH ENFORCEMENT POINTS ARE LIVE: a run whose issue labels or trigger do not satisfy the declaration is refused at POST /v0/runs, and a plan whose scope.files reaches outside a declared `paths` is refused at the plan gate. The sanctioned exception is the audited `applies_to_override` (a reason is REQUIRED), which carries forward to the plan gate from its run-scoped audit entry. See docs/spec/workflow-v2.md § 'Workflow routing (applies_to)'. |
| `on_ci_failure` | `on_ci_failure` | optional |  |  |
| `policy` | `policy` | optional |  |  |
| `auto_advance` | boolean | optional |  | Opt-in auto-advance mode (#1023 / #996 theme 1; renamed from v0/v1's `drive` by E52.6 / #2218): when true, fishhawkd auto-advances the run's mechanical transitions (plan-approved dispatch, review-verdict settlement, fixup re-park, checks-green awaiting_merge) and records a run_auto_advanced audit entry per advance. Judgment points (gate approvals, concern routing, merge) always park. Default false preserves operator-driven advancement. Overridable per-run via POST /v0/runs `drive`. This is a SPEC-SURFACE rename only — the semantics are byte-identical to v0/v1's `drive`, which spelling v0 and v1 keep unchanged; the parser rewrites `auto_advance` to the `drive` key before the typed decode, so the Go field, the runs.drive column, the per-run API override and every read site are untouched. A v2 document declaring `drive` is rejected with a message naming `auto_advance`. |
| `budgets` | array of `periodic_budget` | optional |  | Periodic per-workflow cost ceilings (ADR-030 / #688). Each entry caps total USD spend across all runs of this workflow within a calendar period, resetting at the period boundary. Distinct from the per-stage `budget` ($defs/budget: token/runtime caps on one stage execution). Declared by workflow-v2; accepted whenever present (v2 has no minor chain — a field is accepted because the schema declares it, not because the document declared a high enough minor). |
| `autonomy` | `autonomy_tier` | optional |  |  |
| `actions` | `action_matrix` | optional |  |  |
| `decomposition` | `decomposition` | optional |  |  |
| `escalations` | array of `escalation` | optional | minItems: `1` | Per-path escalation rules (E53.4 / #2227): each entry RAISES the requirements that apply to a change matching its predicate. An escalation may only ever raise — a declared value that does not exceed the workflow's baseline, or that composes to the baseline unchanged, is a VALIDATION ERROR rather than a silently accepted no-op, so an operator reading the block can trust that every entry does something. When several entries match one change the composition is the STRICTEST per dimension and therefore ORDER-INDEPENDENT (max count, sorted de-duplicated UNION of member_of as a conjunction, strictest min_permission, lowest max_autonomy) — never last-match-wins. `max_autonomy` is a CEILING on AGENT autonomy (equivalently a floor on human involvement), applied LAST over the fully resolved action matrix, after the workflow tier and after every explicit `actions` override, so an explicit `actions: {merge: {mode: auto}}` cannot re-widen past it. NOT inherited through `extends` (which folds stages only), matching `applies_to`. See docs/spec/workflow-v2.md § 'Escalations'. |
| `extends` | string | optional | pattern: `^[a-z][a-z0-9_]*$` | SAME-DOCUMENT inheritance (E52.4 / #2216): names another workflow key in this document as this workflow's base. The base's resolved stages are inherited in their declared ORDER; a stage this workflow declares with a matching `id` merges onto the base stage IN THE BASE'S POSITION, and a stage with a new id is appended in declaration order (reordering is deliberately not expressible). Chains resolve transitively. An `extends` naming a workflow this document does not define, and an `extends` cycle (including a self-reference), are both rejected before schema validation with a message naming the offender. Resolution runs BEFORE schema validation, so a deriving workflow may omit `stages` entirely and still satisfy this definition's required list. Cross-FILE inclusion (`include:`) is deliberately out of scope (ADR-067). |
| `defaults` | object | optional |  | WORKFLOW-LEVEL reuse defaults (E52.4 / #2216): the rung ABOVE the extends base and BELOW this workflow's own stage declarations — file defaults -> extends base -> workflow defaults -> the stage's own declaration. A workflow-level default therefore OVERRIDES a value an inherited base stage declared explicitly, which is what makes "extend the base but swap the agent everywhere" expressible; a stage declared on THIS workflow still wins over it. Same merge semantics as the file-level block: `executor` and `budget` merge KEY-WISE, `reviewers` is taken WHOLE from exactly one rung and never blended (it determines review AUTHORITY), and arrays REPLACE wholesale. |

##### `decomposition`

Per-workflow decomposition controls (E24.6 / #1146). Declared by workflow-v2; accepted whenever present (v2 has no minor chain — a field is accepted because the schema declares it, not because the document declared a high enough minor).

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `max_parallel` | integer | optional | min: `0` | Maximum number of decomposed child runs that may dispatch concurrently for a run of this workflow. 0 = unlimited. Per-workflow override of the global FISHHAWKD_MAX_PARALLEL_CHILDREN; when > 0 it wins, otherwise the global default applies. This declares the cap; concurrency throttling that consumes it lands in E24.3 (#1143). |

##### `policy`

Per-workflow execution policy.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `max_stage_runtime` | string | optional | pattern: `^([0-9]+(ns\|us\|ms\|s\|m\|h))+$` | Default wall-clock cap for every agent stage in the workflow. Overridable per-stage via executor.timeout. Parsed by time.ParseDuration. Resolved by spec.ResolveStageTimeout on the backend and delivered to the runner via agent_timeout_seconds on the prompt-fetch response. |

##### `on_ci_failure`

Auto-retry policy when a required CI check fails on the implement stage's PR (#276 / E16). The dispatcher fires a fresh implement workflow_dispatch up to `max_retries` times, threading the new run via `parent_run_id` (#216). Only the closed set of failing conclusions in stagecheck.DeriveState triggers a retry; `fishhawk_audit_complete` failures are explicitly excluded — retrying won't fix Fishhawk's own audit gaps.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `max_retries` | integer | optional | min: `0`; max: `5` | Bound on the retry chain length. 1 (the default when the field is absent) means "on CI failure, dispatch one more run, total agent-touch count 2." 0 disables auto-retries — useful for low-autonomy workflows that prefer a human re-trigger. Capped at 5 to keep runaway loops contained in v0. |

##### `stage`

One workflow stage. The `required: [id, type, executor]` list is enforced on the RESOLVED document, NOT on the raw author bytes: a stage MAY omit `executor` and inherit one from a file- or workflow-level `defaults` block or an `extends` base (E52.4 / #2216). Same-document reuse resolution runs BEFORE schema validation, so an inherited executor satisfies this required list with no schema relaxation. CONSEQUENCE for tooling: a bare `check-jsonschema --schemafile` run resolves no reuse, so it REJECTS a reuse-bearing document the product ACCEPTS — for such a document `fishhawk validate` (or the backend) is the authority, because it resolves first. Readers that must reason about resolved stages resolve reuse before checking (that is what `fishhawk doctor`'s execution-path rung does, #2340).

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `id` | string | required | pattern: `^[a-z][a-z0-9_]*$` | Unique within the parent workflow. Referenced by inputs.from_stage. |
| `type` | string | required | enum: `plan`, `implement`, `review`, `deploy`, `acceptance` | Closed set, read in BOTH senses (E52.7 / #2219). The names are inherited from v0 and are RETAINED deliberately (ADR-067 §2 — do not rename), but they are GENERAL: plan is PROPOSE (emit a proposal — a code plan, or a grooming report), implement is APPLY (apply the proposed change — source edits, or tracker mutations), review is GATE (a decision point on the result). A workflow that produces no code diff at all uses these same three types; see examples/workflow-v2-backlog-grooming.yaml. deploy is the v1 delegating release stage (ADR-038 / #925): it holds no deploy logic or credentials in Fishhawk — it delegates execution to an external pipeline via a delegating executor (executor.delegate) and gates on pre-flight constraints. acceptance (ADR-049 / #1519) is a runner-hosted advisory acceptance stage: it uses an ordinary agent/human executor (never executor.delegate) and carries neither the pre-flight deploy constraints nor the deployment artifact — the validator rejects those on an acceptance stage exactly as on any non-deploy stage. Because the type names are general, POST-HOC DIFF CONSTRAINT VALIDITY BINDS TO WHAT A STAGE PRODUCES, NOT TO ITS TYPE NAME: at v2 a stage carrying one must declare the pull_request artifact. The type<->executor binding and both constraint bindings are enforced semantically by the validator. Custom stage types remain disallowed. |
| `executor` | `executor` | required |  |  |
| `inputs` | array of `input` | optional |  |  |
| `produces` | array of `produces` | optional |  |  |
| `needs` | array of string | optional | minItems: `1` | Shorthand for the common artifact-wiring case (E52.6 / #2218): each entry names an EARLIER stage in the same workflow whose default artifact this stage consumes, and expands at parse time to the equivalent `inputs` entry {artifact: <derived>, from_stage: <id>}. The artifact is derived from the REFERENCED stage's type — a `plan` stage yields the `plan` artifact and an `implement` stage yields `pull_request`, the only two members of $defs/input's artifact enum. A referent of any other type (review / deploy / acceptance) has no default input artifact and is REJECTED, naming longhand `inputs:` as the way to express it. `needs` MAY be combined with longhand `inputs:` on the same stage: the declared inputs come first and the derived entries follow in `needs` order, and a derived entry whose (artifact, from_stage) pair already appears verbatim among the declared inputs is dropped, so the resolved input set is identical however the author spelled it. The referent must exist and must be EARLIER in the workflow; both are enforced by the same from_stage graph-shape rules the longhand form goes through, with their existing messages. |
| `constraints` | `constraint` | optional |  |  |
| `budget` | `budget` | optional |  |  |
| `gates` | array of `gate` | optional |  |  |
| `reviewers` | `reviewers_config` | optional |  |  |
| `egress` | `stage_egress` | optional |  |  |
| `permissions` | `stage_permissions` | optional |  |  |

##### `stage_permissions`

Declared per-stage permissions (E53.5 / #2228): a DECLARATION-ONLY record of the network destinations, write paths and shell posture a stage's agent is expected to use. This declaration is validated, audited and surfaced but is NOT enforced until E51 (#2133); do not rely on it as containment. `permissions.network` is a SPELLING of the stage `egress` allowance — it is normalized into the same field after decode, so an acceptance stage declaring it lands on the one enforced path exactly as `egress` does; declaring BOTH `egress` and `permissions.network` on one stage is a validation error, never a precedence rule. minProperties 1: an empty `permissions: {}` is an authoring error, not a default.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `network` | `stage_egress` | optional |  | The network destinations the stage's agent is expected to reach, as the SAME host / host:port grammar as `egress` — this is a SPELLING of `egress`, normalized into it after decode (declaring both on one stage is a validation error). This declaration is validated, audited and surfaced but is NOT enforced until E51 (#2133); do not rely on it as containment. On an ACCEPTANCE stage the resolved allowance IS enforced today by the runner's default-deny egress proxy, because it normalizes into `egress`; on any other stage type it is a declaration only. |
| `write` | array of string | optional | minItems: `1` | Doublestar globs (bmatcuk/doublestar/v4 semantics; `**` crosses `/`) naming the paths the stage's agent is expected to write, validated and matched through the SAME shared predicate as `applies_to` and `escalations` so the write dialect cannot drift. This declaration is validated, audited and surfaced but is NOT enforced until E51 (#2133); do not rely on it as containment. |
| `shell` | string | optional | enum: `none`, `restricted`, `unrestricted` | The stage's declared shell posture: `none`, `restricted` or `unrestricted`. This declaration is validated, audited and surfaced but is NOT enforced until E51 (#2133); do not rely on it as containment. |

##### `stage_egress`

Egress allowance (ADR-050 / #1532; generalized E53.5 / #2228): the declared target-instance host(s) a stage's agent is expected to reach. On an ACCEPTANCE stage the allow-list IS enforced today by the runner's default-deny egress proxy — these entries are the single customer-controlled slot, with the model API endpoint and the Fishhawk backend added by the runner and not declarable here. On any OTHER stage type the declaration is surfaced and audited but NOT enforced until E51 (#2133). At workflow-v2 an `egress` block (equivalently the `permissions.network` spelling) is valid on any agent-executor stage and rejected on a human or delegate executor; below major 2 it stays acceptance-stage-only. Keep the grammar minimal: hosts, not URLs — scheme and path are not egress-relevant.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `target_hosts` | array of string | required | minItems: `1` | The target-instance host(s) the acceptance agent validates against. Default-deny: any destination not listed here (beyond the runner-added model endpoint and Fishhawk backend) is blocked by the egress proxy. |

##### `executor`

Exactly one of agent (string), human (true), or delegate (an external pipeline). The delegate branch is the v1 delegating executor (ADR-038 / #925) and is valid ONLY on a deploy stage; agent/human are valid only on non-deploy stages. The type<->executor binding is enforced semantically by the validator.

Type: object. 

##### `input`

Type: object. 

##### `produces`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `artifact` | string | required | enum: `plan`, `pull_request`, `deployment`, `acceptance`, `grooming_report` | Closed set. plan/pull_request are inherited from v0; deployment is the v1 runtime artifact a deploy stage emits (ADR-038 / #925) capturing the delegated release outcome {environment, ref/sha, external_run_url, outcome, rollback_handle}, valid ONLY on a deploy stage; acceptance is the artifact an acceptance stage emits (ADR-049 / #1531) capturing the acceptance-evidence record {verdict, per-criterion results, content_hash refs to evidence blobs}, valid ONLY on an acceptance stage; grooming_report is the v2-only backlog-grooming proposal a PROPOSE stage emits instead of a plan (ADR-065 §3 / E54.3 #2235) capturing the rubric-cited ordering, duplicate candidates, hygiene defects, suggested depends_on edges, vision-drift flags and decomposition suggestions, valid ONLY on a `plan`-typed stage (ADR-067 §2 reads `plan` as PROPOSE) and REQUIRING schema: grooming_report_v1, and never declared alongside the plan artifact on the same stage. v0 and v1 are frozen majors and do not admit grooming_report. All three stage-type bindings are enforced semantically by the validator. Custom artifacts remain deferred. |
| `schema` | string | optional |  | Artifact schema version (e.g. standard_v1 for plan, grooming_report_v1 for grooming_report). REQUIRED by the validator for both of those artifacts; MVP_SPEC §4.3 — a proposal artifact is schema-versioned for forward compatibility. |
| `persistence` | array of `persistence` | optional |  |  |

##### `persistence`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `target` | string | required | enum: `originating_issue`, `fishhawk_audit_log` |  |
| `mode` | string | required | enum: `rendered_comment`, `canonical` | rendered_comment: posted as a formatted comment on the target. canonical: stored as the authoritative copy in the audit log. |
| `update_on_change` | boolean | optional |  | Re-publish to the target if the artifact is regenerated. |

##### `constraint`

A stage's constraints, as ONE object keyed by constraint kind (E52.6 / #2218). v0 and v1 spell this as a LIST of single-kind objects, each pinned to exactly one key by maxProperties:1; v2 drops maxProperties because an object's keys are naturally unique, so one object states every kind the stage declares and the exactly-one-kind-per-entry rule is gone. minProperties:1 stays, so `constraints: {}` is rejected as a no-op. The v2 object normalizes at parse time to a ONE-ELEMENT list of the legacy representation — it denotes exactly one constraint value — so no constraint consumer changed and v0/v1 documents are never re-resolved. Two families: the post-hoc diff constraints (max_files_changed, forbidden_paths, allowed_paths, required_outcomes, diff_coverage) evaluated against a stage's produced diff — closed set per MVP_SPEC §4.1, valid on a non-deploy stage that ACTUALLY PRODUCES A DIFF — and the PRE-FLIGHT deploy constraints (allowed_environments, change_freeze, required_upstream; ADR-038 / #925) evaluated BEFORE a delegating deploy stage executes. ADR-038 makes the post-hoc diff constraints meaningless for a delegating deploy and the pre-flight kinds meaningless off a deploy stage; the type<->constraint binding is still enforced semantically by the validator, which reports a v2 violation at /constraints/<kind> rather than at a list index that names nothing the author wrote. AT V2 THE POST-HOC FAMILY ALSO BINDS TO THE PRODUCED-ARTIFACT SET, NOT THE STAGE TYPE NAME (E52.7 / #2219): a stage carrying one of these kinds must declare produces: [{artifact: pull_request}], because the stage types are general (plan/implement/review read as propose/apply/gate, ADR-067 §2) and a workflow on those types may produce no diff at all — a diff constraint there can never be evaluated. An ABSENT or empty produces list reads as 'produces no diff', so omitting produces does not exempt a stage. CONTRAST WITH v0/v1: there the binding stays type-keyed (valid on any non-deploy stage), because v0/v1 documents legitimately declare these constraints on a stage with no produces list; the generalization is licensed only from v2 forward. The violation names the constraint kind and both fixes: declare the artifact, or remove the constraint.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `max_files_changed` | integer | optional | min: `1` |  |
| `forbidden_paths` | array of string | optional | minItems: `1` | Glob patterns (gitignore-style) that the stage's diff must not touch. |
| `allowed_paths` | array of string | optional | minItems: `1` | Glob patterns; the stage's diff must touch only these paths. |
| `required_outcomes` | array of string | optional | minItems: `1` | Outcomes the stage must satisfy. `tests_added_or_updated` is filename-shape-aware (a test-named file in the diff satisfies it). `verification_reported` (ADR-059 / #1886) is substance-aware: it reads the runner's machine-verified committed-tree verify evidence and is satisfied ONLY when that gate reported `passed` — an absent signal, a failed gate, and a skipped gate each violate. Opt-in per workflow; a workflow declaring it must configure `executor.verify` or it can never be satisfied. |
| `diff_coverage` | object | optional |  | POST-HOC diff constraint (ADR-059 / #1888). Opt-in new-line coverage gate: the runner executes `command` against the stage's committed tree, reads the coverage report the command writes to `report_path`, intersects its per-line coverage with the lines this stage ADDED relative to `base_ref`, and reports the measurement as gate evidence. The BACKEND is authoritative for the verdict — it fails the stage category-B when new-line coverage is below `min_new_line_coverage`. A missing measurement is a VIOLATION, not a pass; a diff with zero new coverable lines is a vacuous PASS. Valid on an IMPLEMENT stage only: the implement runner is the layer that measures, and because an absent signal on a declared constraint is a violation, declaring it on any other stage type would be a guaranteed false failure — so the semantic validator rejects it there rather than at evaluation time. A workflow that does not declare it behaves byte-identically to today. |
| `allowed_environments` | array of string | optional | minItems: `1` | PRE-FLIGHT deploy constraint (evaluated BEFORE execution; ADR-038 / #925). The deploy stage may target only these environments. Distinct from the post-hoc diff constraints, which are meaningless for a delegating deploy; valid only on a deploy stage. |
| `change_freeze` | boolean | optional |  | PRE-FLIGHT deploy constraint (evaluated BEFORE execution; ADR-038 / #925). When true the deploy stage is blocked while a change freeze is active. The freeze-signal source is out of scope for the spec (it belongs to the run lifecycle / runner that consume the spec); this declares the gate. Valid only on a deploy stage. |
| `required_upstream` | array of string | optional | minItems: `1` | PRE-FLIGHT deploy constraint (evaluated BEFORE execution; ADR-038 / #925). Upstream conditions that must hold before the deploy stage is allowed to run. Valid only on a deploy stage. |

##### `budget`

Per-stage resource ceiling (token/runtime/cost caps on ONE stage execution; distinct from the workflow-level periodic_budget). AC-4 decision (E52.5 / #2217): limit_usd is the PRIMARY cost lever but is NOT required — a budget declaring only max_tokens, only limit_usd, only max_runtime, any combination, or nothing at all is valid. ADR-067 ratified unifying the units, not mandating a cost model, so the choice lives here in the schema text rather than in a required-field rule. Fields are DECODED ONLY in this slice: no reader of a stage budget exists in the repo, and wiring stage-budget enforcement is #2328's (E48.55) work.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `max_tokens` | integer | optional | min: `1` | OPTIONAL SECONDARY lever (E52.5 / #2217): a per-stage token ceiling, retained for operators who want a token cap in addition to (or instead of) the primary limit_usd USD ceiling. Not required — see this budget's own description for the AC-4 primary-not-required decision. |
| `max_runtime` | string | optional | pattern: `^([0-9]+(ns\|us\|ms\|s\|m\|h))+$` | Per-stage wall-clock cap as a Go duration string parsed by time.ParseDuration (E52.5 / #2217). This REPLACES v0/v1's bare-integer max_runtime_minutes: the v0/v1 value 15 becomes 15m, so a stage budget now spells its runtime cap in the SAME form as policy.max_stage_runtime, executor.timeout and executor.verify.timeout. The pattern is a strict SUBSET of what time.ParseDuration accepts (it also parses fractional / signed / micro-sign forms the pattern rejects), matching the convention of those three existing v2 duration fields byte-for-byte. |
| `limit_usd` | number | optional |  | The stage's PRIMARY cost ceiling in USD (E52.5 / #2217), in the same unit as the workflow-level budgets[].limit_usd. Primary but NOT required (AC-4) — see this budget's own description. Enforcement of this ceiling is #2328's (E48.55) work; declared here it is decoded only. |
| `enforcement` | string | optional | enum: `advisory`, `blocking` | v0 ships advisory only; blocking arrives in v0.x with the Fishhawk-issued ephemeral key path. |

##### `periodic_budget`

A workflow-level recurring cost ceiling (ADR-030 / #688). Caps aggregate USD spend across all runs of the workflow within a calendar period. Distinct from the per-stage `budget` def, which caps token/runtime on a single stage execution.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `period` | string | required | enum: `weekly`, `monthly` | Calendar reset cadence. weekly resets at the start of the ISO week; monthly resets on the first of the month. Boundaries are timezone-aware. |
| `limit_usd` | number | required |  | Cost ceiling in USD for the period, summed from runs.cost_usd_total across the workflow's runs created within the current period. |
| `enforcement` | string | optional | enum: `advisory`, `blocking` | advisory (default): emit a budget_alert audit entry + issue comment on warn_at and 100% crossings; never blocks. blocking: refuse a NEW run at admission once the period spend exhausts limit_usd; in-flight runs are untouched and an operator can override. |
| `warn_at` | number | optional | min: `0`; max: `1` | Optional fraction in [0,1] (e.g. 0.8 for 80%) at which an advisory warning fires ahead of the 100% crossing. Absent means only the 100% threshold is surfaced. |

##### `gate`

Type: object. 

##### `autonomy_tier`

Autonomy TIER SHORTHAND (ADR-066 / E52.10 / #2222) — one word that expands to a full action matrix, replacing v0/v1's five may_* knobs. low = every action class at `mode: gated`: nothing is delegated, every judgment pages the human, and no page_human_on list is implied (there is nothing to except). medium = approve, fixup and retry at `mode: auto` under their named conditions, waive and merge gated. high = medium plus waive and merge at `mode: auto`. medium and high both expand to the same seven-event page_human_on list (gating_reviewer_reject, plan_rejection, scope_amendment, budget_override, policy_override, exception_request, requirement_arbitration). The three tiers are METHODOLOGY.md's low/medium/high and reproduce the shipped workflow-preset-<tier>.yaml delegation blocks exactly, with the v2 `gating_reviewer_reject` spelling in place of the bare `reviewer_reject` v2 removed. An explicit `actions` entry OVERRIDES the tier for THAT CLASS ONLY — unlisted classes keep the tier's value, and a class no tier names resolves to the fail-closed `mode: gated`. Declarable at workflow level (default for every gate) and on an approval gate; a gate declaring EITHER `autonomy` or `actions` supplies the WHOLE block and inherits nothing from the workflow level. Declared by workflow-v2; accepted whenever present (v2 has no minor chain — a field is accepted because the schema declares it, not because the document declared a high enough minor).

Type: string. enum: `low`, `medium`, `high`

##### `action_entry`

One action class's delegation setting (ADR-066 / E52.10 / #2222). `mode` says WHO acts: `gated` — the human acts (the fail-closed default, and what an absent entry resolves to); `auto` — the operator agent may act without paging; `report` — the operator agent surfaces a PROPOSAL for the human and does NOT act. Only `auto` widens agent authority, and it is the only mode that requires a condition: `mode: auto` REQUIRES a `when` naming that class's single backend-evaluable condition, because the backend must be able to answer the predicate from run state. An auto entry with no `when`, an auto entry naming another class's condition, and an auto entry on an EXTENSION class (a class name this schema does not enumerate, which therefore has no backend-evaluable condition at all) are each REJECTED by the backend with a message naming the class and its legal condition. That per-class binding is deliberately NOT expressed as JSON Schema: the class-name set is OPEN (see action_matrix), so no keyword can bind a condition to a class that is not enumerated, and a raw check-jsonschema run therefore accepts a document the backend rejects. `mode: report` fires when the gate is LIVE and, when a `when` is declared, additionally requires that condition to be met — a bare `report` entry surfaces whenever the gate is reachable. A `when` declared on `mode: report` is subject to the SAME per-class binding as `mode: auto`: it must name that class's own condition, and a foreign or extension-class `when` is REJECTED (it would name a predicate the report arm cannot evaluate against this gate and would otherwise never surface). BACKLOG-GROOMING CLASS REGISTRY (ADR-065 / E54.4 / #2236; milestone added by E54.9 / #2309): `hygiene` is a KNOWN class bound to when: objective_reversible and is the ONLY auto-eligible grooming class; `ordering`, `dedup`, `scoping` and `milestone` are KNOWN grooming classes that carry NO condition, so `mode: auto` on any of them is REFUSED BY CONSTRUCTION by the backend, with a message naming the class and why it can never be delegated (ordering re-ranks the backlog, dedup closes items as duplicates, scoping decomposes/iceboxes/closes items, milestone scopes a release milestone against a human-supplied release definition — none of those effects is objectively reversible). That binding stays BACKEND-enforced rather than schema-enforced for the same reason the five run-driving ones do: the class-name set is OPEN, so no keyword can bind a condition to a class name, and a raw check-jsonschema run therefore accepts `scoping: {mode: auto}` where the backend rejects it.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `mode` | string | required | enum: `report`, `gated`, `auto` | report = surface a proposal, never act. gated = the human acts; this is what knob-absence meant in v0/v1 and what an unlisted class resolves to. auto = the operator agent may act, and `when` is then REQUIRED. |
| `when` | string | optional | enum: `clean_dual_approval`, `convergent_concerns`, `solo_low`, `infra_flake`, `gates_resolved_ci_green`, `objective_reversible` | The backend-evaluable condition under which the class's mode applies. Each is legal for exactly ONE class: clean_dual_approval→approve, convergent_concerns→fixup, solo_low→waive, infra_flake→retry, gates_resolved_ci_green→merge (the five v0 delegation conditions, ADR-040 / #1026), and objective_reversible→hygiene (the BACKLOG-GROOMING condition, ADR-065 / E54.4 / #2236: every mutation the grooming report proposes is an objective, reversible hygiene defect fix and none is destructive). The other backlog-grooming classes — `ordering`, `dedup`, `scoping` and `milestone` (E54.9 / #2309) — carry NO condition and so can never declare a `when`; `mode: auto` on them is unwritable. That per-class binding is enforced by the backend (which names the class and its legal condition), not here, because the class-name set is open. REQUIRED for `mode: auto`; optional for `mode: report` (absent means fire on gate-live) but, when present on `mode: report`, must name that class's own condition — a foreign or extension-class `when` on a report entry is REJECTED by the backend, same as at `mode: auto`; carries no meaning for `mode: gated`. |
| `min_severity` | string | optional | enum: `low`, `medium`, `high` | Accepted on the `fixup` class ONLY (backend-enforced — the open class-name set puts this out of schema's reach). Minimum open-concern severity that satisfies convergent_concerns when EVERY implement-review verdict is approve-class (approve / approve_with_concerns; #1964). Absent defaults to medium: a dual-approve round whose only open concern is low-severity parks for the operator instead of auto-routing a full fix-up pass. Set to low to restore route-on-any-concern. A reject verdict BYPASSES the threshold. This is v0/v1's operator_agent.route_fixup_min_severity, moved onto the class it governs. |

##### `action_matrix`

The ACTION MATRIX (ADR-066 / E52.10 / #2222) — workflow-v2's replacement for the v0/v1 `operator_agent` block, which v2 REMOVED. Every property other than the two RESERVED keys below is an ACTION CLASS mapped to an action_entry: `approve`, `fixup`, `waive`, `retry` and `merge` are the five classes the backend knows how to evaluate (they carry v0/v1's may_approve, may_route_fixup + route_fixup_min_severity, may_waive, may_retry and may_merge respectively). The class-name set is deliberately OPEN and per-workflow-type extensible (ADR-065): a workflow type may declare its own class, e.g. `promote` or `rollback`. An unknown class is SAFE BY CONSTRUCTION — it is accepted at `mode: gated` and `mode: report`, where it delegates nothing, and REJECTED at `mode: auto`, where it would need a backend-evaluable condition that does not exist. Absence is fail-closed at every level: an absent matrix, an absent class entry and `mode: gated` all mean the human acts, exactly as an absent may_* knob did. Declarable at workflow level (default for every gate) and on an approval gate; a gate declaring EITHER `autonomy` or `actions` supplies the WHOLE block — entries are NEVER merged across levels, matching the wholesale override semantics the operator_agent block had. Authority semantics (ADR-027) are unchanged: delegation changes who may act, not what gates exist. Declared by workflow-v2; accepted whenever present (v2 has no minor chain — a field is accepted because the schema declares it, not because the document declared a high enough minor). BACKLOG-GROOMING CLASSES (ADR-065 / E54.4 / #2236; milestone added by E54.9 / #2309) are a registered extension of this open set: `hygiene` (auto-eligible under when: objective_reversible), plus `ordering`, `dedup`, `scoping` and `milestone`, which are known but NON-DELEGABLE — accepted at `mode: gated` and `mode: report`, and REJECTED at `mode: auto` by the backend because no backend-evaluable condition exists or may be added for them. None of the five belongs to the resolved matrix's default set: an undeclared grooming class emits no entry, so an ordinary code-change workflow's delegation block is unchanged.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `page_human_on` | array of string | optional |  | RESERVED KEY (not an action class). Events that always page the human regardless of any class's mode — v0/v1's operator_agent.must_page_human, renamed onto the matrix. Closed set; an event listed here is never absorbed by a delegation. The reviewer-reject taxonomy carries exactly TWO tokens in v2: advisory_reviewer_reject (an agent reject under advisory review authority — arbitrable / auto-routed) and gating_reviewer_reject (an agent reject under gating authority — pages the human). The legacy bare reviewer_reject token, which v0/v1 accept and resolve to the gating class, is REMOVED in v2 (E52.3 / #2215): declare gating_reviewer_reject for the same effect. When the block declares no page_human_on, the `autonomy` tier's list applies (medium and high expand to the seven-event list; low expands to none). |
| `model_policy` | object | optional |  | RESERVED KEY (not an action class). Scenario-A operator-agent model-selection contract (#1421), moved here from the removed operator_agent block: declares how the operator agent picks each stage's model, spec-declared and per-repo configurable rather than left to ad-hoc per-gate overrides. Declarative only — the operator agent reads the resolved policy from the run-status delegation block and applies it through the existing per-stage model override channels (#1416), bounded by — never widening — the deployment per-adapter allow-list; this contract adds no new backend resolution code. Part of the matrix, so inherited under the SAME wholesale-override semantics as the class entries (a gate-level block replaces the workflow-level one entirely — model_policy is never merged across levels). All sub-fields optional; an absent model_policy is byte-identical to today. |

##### `approvals`

Forge-neutral approval predicate for an approval gate (E39.2 / #1707). At workflow-v2 it is the SOLE approval predicate (E52.2 / #2214): the legacy GitHub-handle `approvers` role allow-list was removed, so the gate approval branch lists `approvals` in its `required` set — an approval gate always declares exactly this block and is never a no-op. (v0 and v1 still accept both forms and enforce mutual exclusion via an inner oneOf.) `count` is REQUIRED (matching ADR-055's ratified preset default `approvals: {count: 1, not: [author, agent]}`, where count is always explicit) so an empty `approvals: {}` is a no-op and fails validation. The predicate fields carry no repo-specific @-handle, keeping the block forge-neutral; `min_permission` and `member_of` are annotated `x-intended-required` (optional now, intended to become required in a future major). Accepted whenever present under workflow-v2.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `count` | integer | required | min: `1` | REQUIRED. The number of distinct approvals that must be collected before the gate clears. Always explicit (ADR-055); an integer >= 1, so `approvals: {}` (a no-op empty predicate) is rejected. |
| `not` | array of string | optional |  | Relationships excluded from satisfying the gate. `author` bars the change's own author; `agent` bars any automated agent identity. Forge-neutral: these are relationship classes, not handles. |
| `min_permission` | string | optional | enum: `read`, `triage`, `write`, `maintain`, `admin` | Minimum forge-neutral repository permission tier an approver must hold, mirroring backend/internal/identity.Permission (the `none` tier is omitted — a min_permission of none is meaningless). Optional now; annotated x-intended-required for promotion to required in a future major. |
| `member_of` | string | optional | minLength: `1` | A forge-neutral group (org or org/team) an approver must belong to. Optional now; annotated x-intended-required for promotion to required in a future major. |
| `members` | array of string | optional |  | Explicit approver subjects as plain forge-neutral strings (plain identifiers, NOT GitHub-specific @-prefixed handles), keeping the approvals block forge-neutral. |

##### `reviewers_config`

Plan-review reviewers for plan stages (ADR-027). The `authority` property DECLARES whether agent reviewers can block; when it is ABSENT the count-derived DEFAULT governs, read on len(agents): len(agents)>0 && human==0 → gating (agent rejections block stage advancement); len(agents)>0 && human>0 → advisory (agent verdicts surfaced, cannot block human approval); len(agents)==0 → gateless. An explicit `authority` WINS over the counts. The bare `agent` integer count that v0/v1 accept is REMOVED in v2 (E52.3 / #2215) — `agents` is the sole agent-reviewer declaration and the effective agent count is len(agents). When the reviewers block is absent entirely NO reviewers are configured: Stage.Reviewers stays nil, the effective agent count is 0, and the stage resolves gateless. There is no {human:1} default — an absent block configures no reviewer authority, and a human approval requirement is declared by a stage gate of type approval, not by reviewers.human (E52.12 / #2322).

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `authority` | string | optional | enum: `advisory`, `gating` | Explicit review-authority declaration (E53.2 / #2225): states whether this stage's AGENT reviewers can BLOCK stage advancement, instead of leaving it inferred from the reviewer counts. `gating` — an agent reject blocks advancement (no human approver overrides). `advisory` — agent verdicts are surfaced but cannot block; the human gate is authoritative. ABSENT reproduces the ADR-027 count-derived rule EXACTLY: len(agents)>0 && human==0 → gating; len(agents)>0 && human>0 → advisory; len(agents)==0 → gateless — so every existing spec keeps byte-identical gating behavior. An explicit value WINS over the counts: `gating` alongside `human: 1` gates, and `advisory` alongside `human: 0` stays advisory. `gateless` is NOT declarable — it is the zero-agent OUTCOME, not a policy; a declaration (either value) with no agent reviewers (an absent/empty `agents` list) is REJECTED at semantic validation in both the backend and the CLI with a message naming the stage and the fix. Declaring `gating` engages the run-creation reviewer-availability check, so a stage naming an unconfigured reviewer provider fails run creation up front. Declared by workflow-v2; accepted whenever present (v2 has no minor chain — a field is accepted because the schema declares it, not because the document declared a high enough minor). |
| `agents` | array of object | optional | minItems: `1` | Heterogeneous agent reviewers (#955): one entry per reviewer invocation, each naming its provider and optionally its model. The effective agent count for the ADR-027 authority table is len(agents). Authority semantics are unchanged: heterogeneity changes WHO reviews, not gating semantics. |
| `human` | integer | optional | min: `0` | Number of human approvers required. 0 means no human approval gate for this plan stage. |
| `review_timeout` | string | optional | pattern: `^([0-9]+(ns\|us\|ms\|s\|m\|h))+$` | Per-stage review-budget floor (#1494). The stage's review_timeout OVERRIDES the FISHHAWKD_PLAN_REVIEW_TIMEOUT deployment default — it sets the Floor rung of the size-aware review-wait budget (Floor + PerKB*ceil(promptKB), clamped to [Floor,Cap]) for this stage's agent reviews; the deployment-level PerKB and Cap are unchanged. Parsed by time.ParseDuration; an unparseable or absent value falls back to the FISHHAWKD_PLAN_REVIEW_TIMEOUT deployment default. Resolved by spec.ResolveReviewTimeout on the backend. Declared by workflow-v2; accepted whenever present (v2 has no minor chain — a field is accepted because the schema declares it, not because the document declared a high enough minor). |

##### `escalation`

One escalation rule (E53.4 / #2227): a `match` predicate plus the `require` block it RAISES for a change satisfying it. The `match:` WRAPPER IS STRUCTURALLY FORCED, not stylistic — $defs/predicate is additionalProperties:false, and JSON Schema Draft 2020-12 evaluates additionalProperties against the WHOLE instance object, so an escalation cannot $ref the predicate and carry a sibling `require` key. Hoisting `paths:` to the escalation level would mean re-declaring the predicate's properties inline, which is exactly the second-matcher drift E53.1 exists to prevent on a predicate that now backs applies_to across three seams. The predicate is used verbatim with ONE consumer-side refusal: `change_kind` is rejected inside `escalations` for the same reason applies_to rejects it — nothing produces a change kind, so the escalation could never fire, which is indistinguishable from the control being broken.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `match` | `predicate` | required |  | The change this escalation applies to, as a $defs/predicate (E53.1 / #2224) — the SAME match rule `applies_to` and the review-conventions consume, deliberately not a second matcher. `paths` is evaluated against the UNION of the plan's top-level scope.files and every decomposition sub_plan / split_proposal phase scope, so a fan-out slice cannot reach an escalated path without escalating. |
| `require` | `escalation_requirements` | required |  | The requirements this escalation RAISES for a matching change. At least one dimension must be declared (minProperties 1) — an escalation that requires nothing is refused structurally rather than accepted as a no-op. |

##### `escalation_requirements`

The raised requirements of one escalation (E53.4 / #2227). Every dimension is optional individually but the block may not be EMPTY (minProperties 1), so `require: {}` is a schema error. Composition across several matching escalations is the strictest per dimension and therefore order-independent. Each dimension is checked at validation time to prove it actually RAISES something: `count` and `min_permission` must exceed the workflow's least-restrictive baseline (the max count and the strictest tier over every approval gate, because a workflow-level escalation applies to every gate); `member_of` is refused when EVERY applicable approval gate already names the group (the conjunction de-duplicates it, so the composed set is identical to the baseline); and `max_autonomy` is refused when clamping the resolved matrix leaves it identical.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `approvals` | object | optional |  | Raised approval-gate requirements. The NESTED minProperties 1 is load-bearing: without it `require: {approvals: {}}` would satisfy the outer minProperties while requiring nothing, so the claim that a no-op escalation is refused structurally would be only mostly true. A `require.approvals` block on a workflow declaring NO approval gate is refused by the validator — it raises nothing anywhere. |
| `max_autonomy` | `autonomy_tier` | optional |  | A CEILING on AGENT autonomy for a matching change — equivalently a floor on human involvement. It is applied LAST, over the FULLY RESOLVED action matrix: after the workflow (or gate) tier expansion and after every explicit `actions` override, so an explicit `actions: {merge: {mode: auto}}` cannot re-widen past the ceiling. Every class at `mode: auto` that the ceiling tier does not also hold at `auto` is downgraded to `gated` (its condition dropped, provenance recorded as `escalation`); `gated` and `report` are untouched, and an extension class clamps to `gated`, fail-closed. Composes as the LOWEST tier. Because clamping only ever downgrades, a ceiling is monotone-decreasing BY CONSTRUCTION and cannot lower any requirement — so it carries a NO-OP check (a ceiling whose clamp leaves the resolved matrix identical is refused) rather than a raise-check. |

##### `predicate`

One declarative path predicate (E53.1 / #2224): the SINGLE match rule that E53.3's applies_to routing (#2226), E53.4's escalations (#2227) and ADR-068's review-conventions (#2211) each $ref rather than re-declaring a shape. Match semantics are RATIFIED (ADR-066 / #2209): AND across criteria TYPES (every declared type must be satisfied) and OR WITHIN a list (any one entry satisfies its type); an undeclared type does not constrain. An empty predicate is an ERROR, never match-all — enforced here by minProperties 1 and again by spec.Predicate.Validate. Matching is byte-safe and fail-closed (the Go matcher decodes git's C-quoted paths and names an undecodable path rather than silently not matching). Its FIRST consumer is a workflow's `applies_to` (E53.3 / #2226), which $refs it unchanged; #2227 and #2211 wire the other two. This definition is deliberately UNTOUCHED by its consumers — a consumer that needs a narrower grammar rejects the criterion at its own declaration site (as `applies_to` does for `change_kind`) rather than editing the shared shape. See docs/spec/workflow-v2.md § 'Path predicate'.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `paths` | array of string | optional | minItems: `1` | doublestar path globs matched against a change's paths, `**` crossing `/`. Satisfied when ANY change path matches ANY glob (spec.Predicate uses the same doublestar.Match call the plan-gate test sweep makes). Repo-relative, slash-separated. |
| `labels` | array of string | optional | minItems: `1` | Issue/PR label names. Satisfied when the change carries ANY of them. |
| `change_kind` | array of string | optional | minItems: `1` | Change-kind tokens. Satisfied when the change's kind is ANY of them. |
| `trigger` | array of string | optional | minItems: `1` | Trigger forms this predicate accepts. `diff` is a code-diff run; `scheduled` and `on_demand` are the non-diff forms (ADR-067 decoupled stage types from producing a diff, so a scheduled groomer or on-demand incident intake is routable by the same mechanism). A change with no trigger normalizes to `diff`. Satisfied when the change's trigger is ANY listed form. |


### `workflow-v1`

Frozen. Accepted and validated forever; migrate with `fishhawk migrate-spec`.

#### workflow-v1 — top-level fields

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `version` | string | required | enum: `1.0`, `1.1`, `1.2`, `1.3`, `1.4`, `1.5`, `1.6` | Spec syntax version. workflow-v1 advertises 1.0 (the initial grammar plus the E23.2 deploy surface), 1.1 (adds the `acceptance` stage type, ADR-049 / #1519), 1.2 (adds the `acceptance` produces artifact, ADR-049 / #1531, E31.3), 1.3 (adds the acceptance-stage `egress` allowance, ADR-050 / #1532, E31.4), 1.4 (adds the `agent_version` compatibility range on the executor's agent branch and per reviewer in reviewers.agents[], E32.13 / #1743), 1.5 (adds the `verification_reported` required outcome, ADR-059 / #1886), and 1.6 (adds the `diff_coverage` post-hoc constraint kind, ADR-059 / #1888). Each is an additive minor — every lower-minor spec remains valid — mirroring v0's additive 0.3…0.7 minor chain. The validator routes a spec to this schema when its version major is 1 (minor is not routing-significant). Old specs in the audit log remain readable forever via their historical schemas; this enum gates *new* v1 specs only. |
| `workflows` | object | required |  | Named workflows. Keys are snake_case identifiers (e.g. feature_change). At least one workflow must be defined. |
| `roles` | object | optional |  | Named roles referenced by gate approvers. Keys are snake_case identifiers; values list GitHub user/team refs. |
| `test_conventions` | array of `test_convention` | optional |  | Optional per-repo test-location conventions that generalize the plan-gate test sweep (#1004) beyond the built-in Go (name.go -> name_test.go) and colocated-TypeScript defaults. Each entry maps production files matching a glob to candidate test-file path templates. Declared entries are ADDITIVE to the built-in defaults (Go + colocated TS stay covered regardless), so a repo typically declares only its Python / Ruby / parallel-tree conventions. Advisory-only and fail-open: the sweep never blocks a plan. Additive optional field within workflow-v0.x — accepted at every advertised version. |

#### workflow-v1 — definitions

##### `test_convention`

One test-location convention: a production file whose repo-relative path matches `match` is expected to have a test at one of `candidates`. The plan-gate test sweep (#1004) flags an existing candidate test that is absent from the plan's scope.files.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `match` | string | required | minLength: `1` | Doublestar glob (bmatcuk/doublestar/v4 semantics; `**` crosses directory separators) matched against the repo-relative production-file path, e.g. `**/*.go`, `**/*.{ts,tsx}`, `src/**/*.py`. A scoped file whose basename also matches a candidate's test-file shape is treated as a test, not a production file. |
| `candidates` | array of string | required | minItems: `1` | Candidate test-file path templates for a matched production file. Template variables: `{dir}` (the production file's directory), `{name}` (basename without its final extension), `{ext}` (final extension without the leading dot), `{relpath}` (full repo-relative path without its final extension). Examples: `{dir}/{name}_test.go`, `tests/test_{name}.py`, `spec/{relpath}_spec.rb`. The sweep reports a candidate that exists on the base ref but is missing from scope.files. |

##### `role`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `members` | array of `member-ref` | required | minItems: `1` |  |

##### `member-ref`

GitHub user (@user) or team (@org/team) reference. Resolved against the GitHub App installation at run time.

Type: string. pattern: `^@[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)?$`

##### `workflow`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `stages` | array of `stage` | required | minItems: `1` |  |
| `description` | string | optional |  |  |
| `on_ci_failure` | `on_ci_failure` | optional |  |  |
| `policy` | `policy` | optional |  |  |
| `drive` | boolean | optional |  | Opt-in drive mode (#1023 / #996 theme 1): when true, fishhawkd auto-advances the run's mechanical transitions (plan-approved dispatch, review-verdict settlement, fixup re-park, checks-green awaiting_merge) and records a run_auto_advanced audit entry per advance. Judgment points (gate approvals, concern routing, merge) always park. Default false preserves operator-driven advancement. Overridable per-run via POST /v0/runs `drive`. Additive within workflow-v0.x. |
| `budgets` | array of `periodic_budget` | optional |  | Periodic per-workflow cost ceilings (ADR-030 / #688). Each entry caps total USD spend across all runs of this workflow within a calendar period, resetting at the period boundary. Distinct from the per-stage `budget` ($defs/budget: token/runtime caps on one stage execution). Requires version 0.4+. |
| `operator_agent` | `operator_agent` | optional |  |  |
| `decomposition` | `decomposition` | optional |  |  |

##### `decomposition`

Per-workflow decomposition controls (E24.6 / #1146). Requires version 0.6+.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `max_parallel` | integer | optional | min: `0` | Maximum number of decomposed child runs that may dispatch concurrently for a run of this workflow. 0 = unlimited. Per-workflow override of the global FISHHAWKD_MAX_PARALLEL_CHILDREN; when > 0 it wins, otherwise the global default applies. This declares the cap; concurrency throttling that consumes it lands in E24.3 (#1143). |

##### `policy`

Per-workflow execution policy.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `max_stage_runtime` | string | optional | pattern: `^([0-9]+(ns\|us\|ms\|s\|m\|h))+$` | Default wall-clock cap for every agent stage in the workflow. Overridable per-stage via executor.timeout. Parsed by time.ParseDuration. Resolved by spec.ResolveStageTimeout on the backend and delivered to the runner via agent_timeout_seconds on the prompt-fetch response. |

##### `on_ci_failure`

Auto-retry policy when a required CI check fails on the implement stage's PR (#276 / E16). The dispatcher fires a fresh implement workflow_dispatch up to `max_retries` times, threading the new run via `parent_run_id` (#216). Only the closed set of failing conclusions in stagecheck.DeriveState triggers a retry; `fishhawk_audit_complete` failures are explicitly excluded — retrying won't fix Fishhawk's own audit gaps.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `max_retries` | integer | optional | min: `0`; max: `5` | Bound on the retry chain length. 1 (the default when the field is absent) means "on CI failure, dispatch one more run, total agent-touch count 2." 0 disables auto-retries — useful for low-autonomy workflows that prefer a human re-trigger. Capped at 5 to keep runaway loops contained in v0. |

##### `stage`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `id` | string | required | pattern: `^[a-z][a-z0-9_]*$` | Unique within the parent workflow. Referenced by inputs.from_stage. |
| `type` | string | required | enum: `plan`, `implement`, `review`, `deploy`, `acceptance` | Closed set. plan/implement/review are inherited from v0; deploy is the v1 delegating release stage (ADR-038 / #925). A deploy stage holds no deploy logic or credentials in Fishhawk — it delegates execution to an external pipeline via a delegating executor (executor.delegate) and gates on pre-flight constraints. acceptance (v1.1, ADR-049 / #1519) is a runner-hosted advisory acceptance stage: it uses an ordinary agent/human executor (never executor.delegate) and carries neither the pre-flight deploy constraints nor the deployment artifact — the validator rejects those on an acceptance stage exactly as on any non-deploy stage. The type<->executor<->constraint binding is enforced semantically by the validator. Custom stage types remain disallowed. |
| `executor` | `executor` | required |  |  |
| `inputs` | array of `input` | optional |  |  |
| `produces` | array of `produces` | optional |  |  |
| `constraints` | array of `constraint` | optional |  |  |
| `budget` | `budget` | optional |  |  |
| `gates` | array of `gate` | optional |  |  |
| `reviewers` | `reviewers_config` | optional |  |  |
| `egress` | `stage_egress` | optional |  |  |

##### `stage_egress`

Egress allowance for an acceptance stage (v1.3, ADR-050 / #1532): the declared target-instance host(s) the acceptance agent may reach. Valid ONLY on an acceptance stage — the validator rejects it on any other type. These entries are the single customer-controlled slot in the runner's default-deny egress allow-list; the model API endpoint and the Fishhawk backend are added by the runner and are not declarable here. Keep the grammar minimal: hosts, not URLs — scheme and path are not egress-relevant.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `target_hosts` | array of string | required | minItems: `1` | The target-instance host(s) the acceptance agent validates against. Default-deny: any destination not listed here (beyond the runner-added model endpoint and Fishhawk backend) is blocked by the egress proxy. |

##### `executor`

Exactly one of agent (string), human (true), or delegate (an external pipeline). The delegate branch is the v1 delegating executor (ADR-038 / #925) and is valid ONLY on a deploy stage; agent/human are valid only on non-deploy stages. The type<->executor binding is enforced semantically by the validator.

Type: object. 

##### `input`

Type: object. 

##### `produces`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `artifact` | string | required | enum: `plan`, `pull_request`, `deployment`, `acceptance` | Closed set. plan/pull_request are inherited from v0; deployment is the v1 runtime artifact a deploy stage emits (ADR-038 / #925) capturing the delegated release outcome {environment, ref/sha, external_run_url, outcome, rollback_handle}, valid ONLY on a deploy stage; acceptance is the v1.2 artifact an acceptance stage emits (ADR-049 / #1531) capturing the acceptance-evidence record {verdict, per-criterion results, content_hash refs to evidence blobs}, valid ONLY on an acceptance stage. Both stage-type bindings are enforced semantically by the validator. Custom artifacts remain deferred. |
| `schema` | string | optional |  | Artifact schema version (e.g. standard_v1 for plan). |
| `persistence` | array of `persistence` | optional |  |  |

##### `persistence`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `target` | string | required | enum: `originating_issue`, `fishhawk_audit_log` |  |
| `mode` | string | required | enum: `rendered_comment`, `canonical` | rendered_comment: posted as a formatted comment on the target. canonical: stored as the authoritative copy in the audit log. |
| `update_on_change` | boolean | optional |  | Re-publish to the target if the artifact is regenerated. |

##### `constraint`

Exactly one constraint kind per object. Two families: the post-hoc diff constraints (max_files_changed, forbidden_paths, allowed_paths, required_outcomes, diff_coverage) evaluated against a stage's produced diff — closed set per MVP_SPEC §4.1, valid on non-deploy stages — and the v1 PRE-FLIGHT deploy constraints (allowed_environments, change_freeze, required_upstream; ADR-038 / #925) evaluated BEFORE a delegating deploy stage executes. ADR-038 makes the post-hoc diff constraints meaningless for a delegating deploy and the pre-flight kinds meaningless off a deploy stage; the type<->constraint binding is enforced semantically by the validator.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `max_files_changed` | integer | optional | min: `1` |  |
| `forbidden_paths` | array of string | optional | minItems: `1` | Glob patterns (gitignore-style) that the stage's diff must not touch. |
| `allowed_paths` | array of string | optional | minItems: `1` | Glob patterns; the stage's diff must touch only these paths. |
| `required_outcomes` | array of string | optional | minItems: `1` | Outcomes the stage must satisfy. `tests_added_or_updated` is filename-shape-aware (a test-named file in the diff satisfies it). `verification_reported` (v1.5, ADR-059 / #1886) is substance-aware: it reads the runner's machine-verified committed-tree verify evidence and is satisfied ONLY when that gate reported `passed` — an absent signal, a failed gate, and a skipped gate each violate. Opt-in per workflow; a workflow declaring it must configure `executor.verify` or it can never be satisfied. |
| `diff_coverage` | object | optional |  | POST-HOC diff constraint (v1.6, ADR-059 / #1888). Opt-in new-line coverage gate: the runner executes `command` against the stage's committed tree, reads the coverage report the command writes to `report_path`, intersects its per-line coverage with the lines this stage ADDED relative to `base_ref`, and reports the measurement as gate evidence. The BACKEND is authoritative for the verdict — it fails the stage category-B when new-line coverage is below `min_new_line_coverage`. A missing measurement is a VIOLATION, not a pass; a diff with zero new coverable lines is a vacuous PASS. Valid on an IMPLEMENT stage only: the implement runner is the layer that measures, and because an absent signal on a declared constraint is a violation, declaring it on any other stage type would be a guaranteed false failure — so the semantic validator rejects it there rather than at evaluation time. A workflow that does not declare it behaves byte-identically to today. |
| `allowed_environments` | array of string | optional | minItems: `1` | PRE-FLIGHT deploy constraint (evaluated BEFORE execution; ADR-038 / #925). The deploy stage may target only these environments. Distinct from the post-hoc diff constraints, which are meaningless for a delegating deploy; valid only on a deploy stage. |
| `change_freeze` | boolean | optional |  | PRE-FLIGHT deploy constraint (evaluated BEFORE execution; ADR-038 / #925). When true the deploy stage is blocked while a change freeze is active. The freeze-signal source is out of scope for the spec (it belongs to the run lifecycle / runner that consume the spec); this declares the gate. Valid only on a deploy stage. |
| `required_upstream` | array of string | optional | minItems: `1` | PRE-FLIGHT deploy constraint (evaluated BEFORE execution; ADR-038 / #925). Upstream conditions that must hold before the deploy stage is allowed to run. Valid only on a deploy stage. |

##### `budget`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `max_tokens` | integer | optional | min: `1` |  |
| `max_runtime_minutes` | integer | optional | min: `1` |  |
| `enforcement` | string | optional | enum: `advisory`, `blocking` | v0 ships advisory only; blocking arrives in v0.x with the Fishhawk-issued ephemeral key path. |

##### `periodic_budget`

A workflow-level recurring cost ceiling (ADR-030 / #688). Caps aggregate USD spend across all runs of the workflow within a calendar period. Distinct from the per-stage `budget` def, which caps token/runtime on a single stage execution.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `period` | string | required | enum: `weekly`, `monthly` | Calendar reset cadence. weekly resets at the start of the ISO week; monthly resets on the first of the month. Boundaries are timezone-aware. |
| `limit_usd` | number | required |  | Cost ceiling in USD for the period, summed from runs.cost_usd_total across the workflow's runs created within the current period. |
| `enforcement` | string | optional | enum: `advisory`, `blocking` | advisory (default): emit a budget_alert audit entry + issue comment on warn_at and 100% crossings; never blocks. blocking: refuse a NEW run at admission once the period spend exhausts limit_usd; in-flight runs are untouched and an operator can override. |
| `warn_at` | number | optional | min: `0`; max: `1` | Optional fraction in [0,1] (e.g. 0.8 for 80%) at which an advisory warning fires ahead of the 100% crossing. Absent means only the 100% threshold is surfaced. |

##### `gate`

Type: object. 

##### `operator_agent`

Delegation knobs for the operator agent (ADR-040 / #1026). Each may_* knob names the single backend-evaluable condition under which the operator agent may take that action without paging the human; the per-knob enums are a closed set — the backend must be able to answer every condition from run state. Absent block (and absent knob) = fail-closed: nothing is delegated, every judgment pages the human. Declarable at workflow level (default for all gates) and on an approval gate (per-gate override; a gate-level block wins WHOLESALE over the workflow block — knobs are not merged). Authority semantics (ADR-027) are unchanged: delegation changes who may act, not what gates exist. Requires version 0.5+.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `may_approve` | string | optional | enum: `clean_dual_approval` | The operator agent may approve a gate when every configured reviewer for the gated stage returned an approve verdict and zero concerns are open. |
| `may_route_fixup` | string | optional | enum: `convergent_concerns` | The operator agent may route review concerns to a fixup when all reviewer verdicts are in, at least one concern is open, and no reviewer rejected. When every verdict is approve-class the auto-route additionally requires an open concern at or above route_fixup_min_severity (default medium); see that field. |
| `route_fixup_min_severity` | string | optional | enum: `low`, `medium`, `high` | Minimum open-concern severity that satisfies may_route_fixup's convergent_concerns condition when EVERY implement-review verdict is approve-class (approve / approve_with_concerns). Absent defaults to medium: a dual-approve round whose only open concern is low-severity parks for the operator instead of auto-routing a full fix-up pass (#1964). Set to low to restore the legacy route-on-any-concern behavior. A reject verdict BYPASSES the threshold — advisory arbitration and the gating-reject page are unchanged. Additive optional field within workflow-v1.x — accepted at every advertised version (the agent_version / executor.model precedent; no version enum bump). |
| `may_waive` | string | optional | enum: `solo_low` | The operator agent may waive a concern when it is the only open concern and its severity is low. |
| `may_retry` | string | optional | enum: `infra_flake` | The operator agent may retry a failed stage when the latest stage failure is classified as an infrastructure flake. |
| `may_merge` | string | optional | enum: `gates_resolved_ci_green` | The operator agent may merge when no gate approvals are pending, zero concerns are open, the PR is open, and required checks are green. Evaluated and surfaced in v0; merge itself happens on GitHub, so backend enforcement attaches once a merge action surface exists. |
| `must_page_human` | array of string | optional |  | Events that always page the human regardless of the may_* knobs. Closed v0 set; an event listed here is never absorbed by a delegation. The reviewer-reject taxonomy carries three tokens: the explicit advisory_reviewer_reject (an agent reject under advisory review authority — arbitrable / auto-routed) and gating_reviewer_reject (an agent reject under gating authority — pages the human), plus the legacy bare reviewer_reject, which is preserved and maps to the gating class for back-compat (requires version 0.7+ for the explicit tokens). |
| `model_policy` | object | optional |  | Scenario-A operator-agent model-selection contract (#1421): declares how the operator agent picks each stage's model, spec-declared and per-repo configurable rather than left to ad-hoc per-gate overrides. Declarative only — the operator agent reads the resolved policy from the run-status delegation block and applies it through the existing per-stage model override channels (#1416), bounded by — never widening — the deployment per-adapter allow-list; this contract adds no new backend resolution code. Part of the operator_agent block, so inherited under the SAME wholesale-override semantics as the may_* knobs (a gate-level operator_agent block replaces the workflow-level one entirely — model_policy is never merged across levels). All sub-fields optional; an absent model_policy is byte-identical to today. Requires version 0.5+. |

##### `approvers`

Either any_of (one approver suffices) or all_of (every named role must approve).

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `any_of` | array of string | optional | minItems: `1` |  |
| `all_of` | array of string | optional | minItems: `1` |  |

##### `approvals`

Forge-neutral approval predicate for an approval gate (E39.2 / #1707). An additive, optional alternative to the legacy GitHub-handle `approvers` allow-list: a gate declares EXACTLY ONE of the two (the gate's inner oneOf enforces the mutual exclusion), so every existing approvers-only gate stays valid. `count` is REQUIRED (matching ADR-055's ratified preset default `approvals: {count: 1, not: [author, agent]}`, where count is always explicit) so an empty `approvals: {}` is a no-op and fails validation. The predicate fields carry no repo-specific @-handle, keeping the block forge-neutral; `min_permission` and `member_of` are annotated `x-intended-required` (optional now, intended to become required in a future major). Accepted at every advertised v1 version.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `count` | integer | required | min: `1` | REQUIRED. The number of distinct approvals that must be collected before the gate clears. Always explicit (ADR-055); an integer >= 1, so `approvals: {}` (a no-op empty predicate) is rejected. |
| `not` | array of string | optional |  | Relationships excluded from satisfying the gate. `author` bars the change's own author; `agent` bars any automated agent identity. Forge-neutral: these are relationship classes, not handles. |
| `min_permission` | string | optional | enum: `read`, `triage`, `write`, `maintain`, `admin` | Minimum forge-neutral repository permission tier an approver must hold, mirroring backend/internal/identity.Permission (the `none` tier is omitted — a min_permission of none is meaningless). Optional now; annotated x-intended-required for promotion to required in a future major. |
| `member_of` | string | optional | minLength: `1` | A forge-neutral group (org or org/team) an approver must belong to. Optional now; annotated x-intended-required for promotion to required in a future major. |
| `members` | array of string | optional |  | Explicit approver subjects as plain forge-neutral strings (NOT the GitHub-specific @-prefixed member-ref used by the legacy roles.members path), keeping the approvals block forge-neutral. |

##### `reviewers_config`

Plan-review reviewer counts for plan stages (ADR-027). Authority table: agent>0 && human==0 → gating (agent rejections block stage advancement); agent>0 && human>0 → advisory (agent verdicts surfaced, cannot block human approval); agent==0 → gateless. The effective agent count is len(agents) when the `agents` list is present and non-empty, else the bare `agent` integer. When the reviewers block is absent entirely NO reviewers are configured: Stage.Reviewers stays nil, the effective agent count is 0, and the stage resolves gateless. There is no {human:1} default — an absent block configures no reviewer authority, and a human approval requirement is declared by a stage gate of type approval, not by reviewers.human (E52.12 / #2322).

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `agent` | integer | optional | min: `0` | Number of agent reviewers to invoke after the plan artifact is validated. 0 (default) means no agent review. Superseded by `agents` when that list is present and non-empty; each invocation then uses the deployment's precedence-selected default adapter. |
| `agents` | array of object | optional | minItems: `1` | Heterogeneous agent reviewers (#955): one entry per reviewer invocation, each naming its provider and optionally its model. When present and non-empty this list supersedes the bare `agent` integer count — the effective agent count for the ADR-027 authority table is len(agents). Authority semantics are unchanged: heterogeneity changes WHO reviews, not gating semantics. |
| `human` | integer | optional | min: `0` | Number of human approvers required. 0 means no human approval gate for this plan stage. |
| `review_timeout` | string | optional | pattern: `^([0-9]+(ns\|us\|ms\|s\|m\|h))+$` | Per-stage review-budget floor (#1494). The stage's review_timeout OVERRIDES the FISHHAWKD_PLAN_REVIEW_TIMEOUT deployment default — it sets the Floor rung of the size-aware review-wait budget (Floor + PerKB*ceil(promptKB), clamped to [Floor,Cap]) for this stage's agent reviews; the deployment-level PerKB and Cap are unchanged. Parsed by time.ParseDuration; an unparseable or absent value falls back to the FISHHAWKD_PLAN_REVIEW_TIMEOUT deployment default. Resolved by spec.ResolveReviewTimeout on the backend. Additive optional field within workflow-v1.x — accepted at every advertised version. |


### `workflow-v0`

Frozen. Accepted and validated forever.

#### workflow-v0 — top-level fields

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `version` | string | required | enum: `0.3`, `0.4`, `0.5`, `0.6`, `0.7` | Spec syntax version. 0.7 adds the explicit advisory_reviewer_reject / gating_reviewer_reject delegation page-event classes (#1378), additive within workflow-v0.x; the bare reviewer_reject is preserved and resolves to the gating sense. 0.6 adds workflow.decomposition.max_parallel — a per-workflow override of FISHHAWKD_MAX_PARALLEL_CHILDREN bounding how many decomposed child runs dispatch concurrently (E24.6 / #1146), additive within workflow-v0.x. 0.5 adds operator_agent — delegation knobs for the operator agent, declarable at workflow level and per approval gate (ADR-040 / #1026), additive within workflow-v0.x. 0.4 adds workflow.budgets — periodic per-workflow cost ceilings (ADR-030 / #688), additive within workflow-v0.x. 0.3 adds workflow.on_ci_failure.max_retries for the auto-retry epic (#276 / #277), workflow.policy.max_stage_runtime and stage.executor.timeout for spec-governed agent timeouts (#452). 0.2 dropped gate.blocking_checks (ADR-017 / #249); 0.1 is rejected. Old specs in the audit log remain readable forever via the historical schemas; this enum gates *new* specs only. |
| `workflows` | object | required |  | Named workflows. Keys are snake_case identifiers (e.g. feature_change). At least one workflow must be defined. |
| `roles` | object | optional |  | Named roles referenced by gate approvers. Keys are snake_case identifiers; values list GitHub user/team refs. |
| `test_conventions` | array of `test_convention` | optional |  | Optional per-repo test-location conventions that generalize the plan-gate test sweep (#1004) beyond the built-in Go (name.go -> name_test.go) and colocated-TypeScript defaults. Each entry maps production files matching a glob to candidate test-file path templates. Declared entries are ADDITIVE to the built-in defaults (Go + colocated TS stay covered regardless), so a repo typically declares only its Python / Ruby / parallel-tree conventions. Advisory-only and fail-open: the sweep never blocks a plan. Additive optional field within workflow-v0.x — accepted at every advertised version. |

#### workflow-v0 — definitions

##### `test_convention`

One test-location convention: a production file whose repo-relative path matches `match` is expected to have a test at one of `candidates`. The plan-gate test sweep (#1004) flags an existing candidate test that is absent from the plan's scope.files.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `match` | string | required | minLength: `1` | Doublestar glob (bmatcuk/doublestar/v4 semantics; `**` crosses directory separators) matched against the repo-relative production-file path, e.g. `**/*.go`, `**/*.{ts,tsx}`, `src/**/*.py`. A scoped file whose basename also matches a candidate's test-file shape is treated as a test, not a production file. |
| `candidates` | array of string | required | minItems: `1` | Candidate test-file path templates for a matched production file. Template variables: `{dir}` (the production file's directory), `{name}` (basename without its final extension), `{ext}` (final extension without the leading dot), `{relpath}` (full repo-relative path without its final extension). Examples: `{dir}/{name}_test.go`, `tests/test_{name}.py`, `spec/{relpath}_spec.rb`. The sweep reports a candidate that exists on the base ref but is missing from scope.files. |

##### `role`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `members` | array of `member-ref` | required | minItems: `1` |  |

##### `member-ref`

GitHub user (@user) or team (@org/team) reference. Resolved against the GitHub App installation at run time.

Type: string. pattern: `^@[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)?$`

##### `workflow`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `stages` | array of `stage` | required | minItems: `1` |  |
| `description` | string | optional |  |  |
| `on_ci_failure` | `on_ci_failure` | optional |  |  |
| `policy` | `policy` | optional |  |  |
| `drive` | boolean | optional |  | Opt-in drive mode (#1023 / #996 theme 1): when true, fishhawkd auto-advances the run's mechanical transitions (plan-approved dispatch, review-verdict settlement, fixup re-park, checks-green awaiting_merge) and records a run_auto_advanced audit entry per advance. Judgment points (gate approvals, concern routing, merge) always park. Default false preserves operator-driven advancement. Overridable per-run via POST /v0/runs `drive`. Additive within workflow-v0.x. |
| `budgets` | array of `periodic_budget` | optional |  | Periodic per-workflow cost ceilings (ADR-030 / #688). Each entry caps total USD spend across all runs of this workflow within a calendar period, resetting at the period boundary. Distinct from the per-stage `budget` ($defs/budget: token/runtime caps on one stage execution). Requires version 0.4+. |
| `operator_agent` | `operator_agent` | optional |  |  |
| `decomposition` | `decomposition` | optional |  |  |

##### `decomposition`

Per-workflow decomposition controls (E24.6 / #1146). Requires version 0.6+.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `max_parallel` | integer | optional | min: `0` | Maximum number of decomposed child runs that may dispatch concurrently for a run of this workflow. 0 = unlimited. Per-workflow override of the global FISHHAWKD_MAX_PARALLEL_CHILDREN; when > 0 it wins, otherwise the global default applies. This declares the cap; concurrency throttling that consumes it lands in E24.3 (#1143). |

##### `policy`

Per-workflow execution policy.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `max_stage_runtime` | string | optional | pattern: `^([0-9]+(ns\|us\|ms\|s\|m\|h))+$` | Default wall-clock cap for every agent stage in the workflow. Overridable per-stage via executor.timeout. Parsed by time.ParseDuration. Resolved by spec.ResolveStageTimeout on the backend and delivered to the runner via agent_timeout_seconds on the prompt-fetch response. |

##### `on_ci_failure`

Auto-retry policy when a required CI check fails on the implement stage's PR (#276 / E16). The dispatcher fires a fresh implement workflow_dispatch up to `max_retries` times, threading the new run via `parent_run_id` (#216). Only the closed set of failing conclusions in stagecheck.DeriveState triggers a retry; `fishhawk_audit_complete` failures are explicitly excluded — retrying won't fix Fishhawk's own audit gaps.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `max_retries` | integer | optional | min: `0`; max: `5` | Bound on the retry chain length. 1 (the default when the field is absent) means "on CI failure, dispatch one more run, total agent-touch count 2." 0 disables auto-retries — useful for low-autonomy workflows that prefer a human re-trigger. Capped at 5 to keep runaway loops contained in v0. |

##### `stage`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `id` | string | required | pattern: `^[a-z][a-z0-9_]*$` | Unique within the parent workflow. Referenced by inputs.from_stage. |
| `type` | string | required | enum: `plan`, `implement`, `review` | v0 closed set. Custom stage types are deliberately disallowed. |
| `executor` | `executor` | required |  |  |
| `inputs` | array of `input` | optional |  |  |
| `produces` | array of `produces` | optional |  |  |
| `constraints` | array of `constraint` | optional |  |  |
| `budget` | `budget` | optional |  |  |
| `gates` | array of `gate` | optional |  |  |
| `reviewers` | `reviewers_config` | optional |  |  |

##### `executor`

Exactly one of agent (string) or human (true).

Type: object. 

##### `input`

Type: object. 

##### `produces`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `artifact` | string | required | enum: `plan`, `pull_request` | v0 closed set. Custom artifacts deferred. |
| `schema` | string | optional |  | Artifact schema version (e.g. standard_v1 for plan). |
| `persistence` | array of `persistence` | optional |  |  |

##### `persistence`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `target` | string | required | enum: `originating_issue`, `fishhawk_audit_log` |  |
| `mode` | string | required | enum: `rendered_comment`, `canonical` | rendered_comment: posted as a formatted comment on the target. canonical: stored as the authoritative copy in the audit log. |
| `update_on_change` | boolean | optional |  | Re-publish to the target if the artifact is regenerated. |

##### `constraint`

Exactly one constraint kind per object. Closed set per MVP_SPEC §4.1.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `max_files_changed` | integer | optional | min: `1` |  |
| `forbidden_paths` | array of string | optional | minItems: `1` | Glob patterns (gitignore-style) that the stage's diff must not touch. |
| `allowed_paths` | array of string | optional | minItems: `1` | Glob patterns; the stage's diff must touch only these paths. |
| `required_outcomes` | array of string | optional | minItems: `1` |  |

##### `budget`

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `max_tokens` | integer | optional | min: `1` |  |
| `max_runtime_minutes` | integer | optional | min: `1` |  |
| `enforcement` | string | optional | enum: `advisory`, `blocking` | v0 ships advisory only; blocking arrives in v0.x with the Fishhawk-issued ephemeral key path. |

##### `periodic_budget`

A workflow-level recurring cost ceiling (ADR-030 / #688). Caps aggregate USD spend across all runs of the workflow within a calendar period. Distinct from the per-stage `budget` def, which caps token/runtime on a single stage execution.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `period` | string | required | enum: `weekly`, `monthly` | Calendar reset cadence. weekly resets at the start of the ISO week; monthly resets on the first of the month. Boundaries are timezone-aware. |
| `limit_usd` | number | required |  | Cost ceiling in USD for the period, summed from runs.cost_usd_total across the workflow's runs created within the current period. |
| `enforcement` | string | optional | enum: `advisory`, `blocking` | advisory (default): emit a budget_alert audit entry + issue comment on warn_at and 100% crossings; never blocks. blocking: refuse a NEW run at admission once the period spend exhausts limit_usd; in-flight runs are untouched and an operator can override. |
| `warn_at` | number | optional | min: `0`; max: `1` | Optional fraction in [0,1] (e.g. 0.8 for 80%) at which an advisory warning fires ahead of the 100% crossing. Absent means only the 100% threshold is surfaced. |

##### `gate`

Type: object. 

##### `operator_agent`

Delegation knobs for the operator agent (ADR-040 / #1026). Each may_* knob names the single backend-evaluable condition under which the operator agent may take that action without paging the human; the per-knob enums are a closed set — the backend must be able to answer every condition from run state. Absent block (and absent knob) = fail-closed: nothing is delegated, every judgment pages the human. Declarable at workflow level (default for all gates) and on an approval gate (per-gate override; a gate-level block wins WHOLESALE over the workflow block — knobs are not merged). Authority semantics (ADR-027) are unchanged: delegation changes who may act, not what gates exist. Requires version 0.5+.

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `may_approve` | string | optional | enum: `clean_dual_approval` | The operator agent may approve a gate when every configured reviewer for the gated stage returned an approve verdict and zero concerns are open. |
| `may_route_fixup` | string | optional | enum: `convergent_concerns` | The operator agent may route review concerns to a fixup when all reviewer verdicts are in, at least one concern is open, and no reviewer rejected. When every verdict is approve-class the auto-route additionally requires an open concern at or above route_fixup_min_severity (default medium); see that field. |
| `route_fixup_min_severity` | string | optional | enum: `low`, `medium`, `high` | Minimum open-concern severity that satisfies may_route_fixup's convergent_concerns condition when EVERY implement-review verdict is approve-class (approve / approve_with_concerns). Absent defaults to medium: a dual-approve round whose only open concern is low-severity parks for the operator instead of auto-routing a full fix-up pass (#1964). Set to low to restore the legacy route-on-any-concern behavior. A reject verdict BYPASSES the threshold — advisory arbitration and the gating-reject page are unchanged. Additive optional field within workflow-v0.x — accepted at every advertised version (the agent_version / executor.model precedent; no version enum bump). |
| `may_waive` | string | optional | enum: `solo_low` | The operator agent may waive a concern when it is the only open concern and its severity is low. |
| `may_retry` | string | optional | enum: `infra_flake` | The operator agent may retry a failed stage when the latest stage failure is classified as an infrastructure flake. |
| `may_merge` | string | optional | enum: `gates_resolved_ci_green` | The operator agent may merge when no gate approvals are pending, zero concerns are open, the PR is open, and required checks are green. Evaluated and surfaced in v0; merge itself happens on GitHub, so backend enforcement attaches once a merge action surface exists. |
| `must_page_human` | array of string | optional |  | Events that always page the human regardless of the may_* knobs. Closed v0 set; an event listed here is never absorbed by a delegation. The reviewer-reject taxonomy carries three tokens: the explicit advisory_reviewer_reject (an agent reject under advisory review authority — arbitrable / auto-routed) and gating_reviewer_reject (an agent reject under gating authority — pages the human), plus the legacy bare reviewer_reject, which is preserved and maps to the gating class for back-compat (requires version 0.7+ for the explicit tokens). |
| `model_policy` | object | optional |  | Scenario-A operator-agent model-selection contract (#1421): declares how the operator agent picks each stage's model, spec-declared and per-repo configurable rather than left to ad-hoc per-gate overrides. Declarative only — the operator agent reads the resolved policy from the run-status delegation block and applies it through the existing per-stage model override channels (#1416), bounded by — never widening — the deployment per-adapter allow-list; this contract adds no new backend resolution code. Part of the operator_agent block, so inherited under the SAME wholesale-override semantics as the may_* knobs (a gate-level operator_agent block replaces the workflow-level one entirely — model_policy is never merged across levels). All sub-fields optional; an absent model_policy is byte-identical to today. Requires version 0.5+. |

##### `approvers`

Either any_of (one approver suffices) or all_of (every named role must approve).

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `any_of` | array of string | optional | minItems: `1` |  |
| `all_of` | array of string | optional | minItems: `1` |  |

##### `reviewers_config`

Plan-review reviewer counts for plan stages (ADR-027). Authority table: agent>0 && human==0 → gating (agent rejections block stage advancement); agent>0 && human>0 → advisory (agent verdicts surfaced, cannot block human approval); agent==0 → gateless. The effective agent count is len(agents) when the `agents` list is present and non-empty, else the bare `agent` integer. When the reviewers block is absent entirely NO reviewers are configured: Stage.Reviewers stays nil, the effective agent count is 0, and the stage resolves gateless. There is no {human:1} default — an absent block configures no reviewer authority, and a human approval requirement is declared by a stage gate of type approval, not by reviewers.human (E52.12 / #2322).

| Field | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `agent` | integer | optional | min: `0` | Number of agent reviewers to invoke after the plan artifact is validated. 0 (default) means no agent review. Superseded by `agents` when that list is present and non-empty; each invocation then uses the deployment's precedence-selected default adapter. |
| `agents` | array of object | optional | minItems: `1` | Heterogeneous agent reviewers (#955): one entry per reviewer invocation, each naming its provider and optionally its model. When present and non-empty this list supersedes the bare `agent` integer count — the effective agent count for the ADR-027 authority table is len(agents). Authority semantics are unchanged: heterogeneity changes WHO reviews, not gating semantics. |
| `human` | integer | optional | min: `0` | Number of human approvers required. 0 means no human approval gate for this plan stage. |


## What changed between majors

Shape deltas only — a field whose description changed but whose type, requiredness, enum members and default did not is NOT listed here.

### workflow-v1 → workflow-v2

| Field | Change | Detail |
|---|---|---|
| `/$defs/action_entry` | added | new at the newer major |
| `/$defs/action_entry/properties/min_severity` | added | new at the newer major |
| `/$defs/action_entry/properties/mode` | added | new at the newer major |
| `/$defs/action_entry/properties/when` | added | new at the newer major |
| `/$defs/action_matrix` | added | new at the newer major |
| `/$defs/action_matrix/properties/model_policy` | added | new at the newer major |
| `/$defs/action_matrix/properties/page_human_on` | added | new at the newer major |
| `/$defs/approvers` | removed | present in the older major, absent at the newer |
| `/$defs/approvers/properties/all_of` | removed | present in the older major, absent at the newer |
| `/$defs/approvers/properties/any_of` | removed | present in the older major, absent at the newer |
| `/$defs/autonomy_tier` | added | new at the newer major |
| `/$defs/budget/properties/limit_usd` | added | new at the newer major |
| `/$defs/budget/properties/max_runtime` | added | new at the newer major |
| `/$defs/budget/properties/max_runtime_minutes` | removed | present in the older major, absent at the newer |
| `/$defs/escalation` | added | new at the newer major |
| `/$defs/escalation/properties/match` | added | new at the newer major |
| `/$defs/escalation/properties/require` | added | new at the newer major |
| `/$defs/escalation_requirements` | added | new at the newer major |
| `/$defs/escalation_requirements/properties/approvals` | added | new at the newer major |
| `/$defs/escalation_requirements/properties/max_autonomy` | added | new at the newer major |
| `/$defs/member-ref` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent/properties/may_approve` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent/properties/may_merge` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent/properties/may_retry` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent/properties/may_route_fixup` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent/properties/may_waive` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent/properties/model_policy` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent/properties/must_page_human` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent/properties/route_fixup_min_severity` | removed | present in the older major, absent at the newer |
| `/$defs/predicate` | added | new at the newer major |
| `/$defs/predicate/properties/change_kind` | added | new at the newer major |
| `/$defs/predicate/properties/labels` | added | new at the newer major |
| `/$defs/predicate/properties/paths` | added | new at the newer major |
| `/$defs/predicate/properties/trigger` | added | new at the newer major |
| `/$defs/produces/properties/artifact` | changed | enum members differ |
| `/$defs/reviewers_config/properties/agent` | removed | present in the older major, absent at the newer |
| `/$defs/reviewers_config/properties/authority` | added | new at the newer major |
| `/$defs/role` | removed | present in the older major, absent at the newer |
| `/$defs/role/properties/members` | removed | present in the older major, absent at the newer |
| `/$defs/stage/properties/constraints` | changed | type "array of `constraint`"→"`constraint`" |
| `/$defs/stage/properties/needs` | added | new at the newer major |
| `/$defs/stage/properties/permissions` | added | new at the newer major |
| `/$defs/stage_permissions` | added | new at the newer major |
| `/$defs/stage_permissions/properties/network` | added | new at the newer major |
| `/$defs/stage_permissions/properties/shell` | added | new at the newer major |
| `/$defs/stage_permissions/properties/write` | added | new at the newer major |
| `/$defs/workflow/properties/actions` | added | new at the newer major |
| `/$defs/workflow/properties/applies_to` | added | new at the newer major |
| `/$defs/workflow/properties/auto_advance` | added | new at the newer major |
| `/$defs/workflow/properties/autonomy` | added | new at the newer major |
| `/$defs/workflow/properties/defaults` | added | new at the newer major |
| `/$defs/workflow/properties/drive` | removed | present in the older major, absent at the newer |
| `/$defs/workflow/properties/escalations` | added | new at the newer major |
| `/$defs/workflow/properties/extends` | added | new at the newer major |
| `/$defs/workflow/properties/operator_agent` | removed | present in the older major, absent at the newer |
| `/properties/defaults` | added | new at the newer major |
| `/properties/roles` | removed | present in the older major, absent at the newer |
| `/properties/version` | changed | enum members differ |

### workflow-v0 → workflow-v2

| Field | Change | Detail |
|---|---|---|
| `/$defs/action_entry` | added | new at the newer major |
| `/$defs/action_entry/properties/min_severity` | added | new at the newer major |
| `/$defs/action_entry/properties/mode` | added | new at the newer major |
| `/$defs/action_entry/properties/when` | added | new at the newer major |
| `/$defs/action_matrix` | added | new at the newer major |
| `/$defs/action_matrix/properties/model_policy` | added | new at the newer major |
| `/$defs/action_matrix/properties/page_human_on` | added | new at the newer major |
| `/$defs/approvals` | added | new at the newer major |
| `/$defs/approvals/properties/count` | added | new at the newer major |
| `/$defs/approvals/properties/member_of` | added | new at the newer major |
| `/$defs/approvals/properties/members` | added | new at the newer major |
| `/$defs/approvals/properties/min_permission` | added | new at the newer major |
| `/$defs/approvals/properties/not` | added | new at the newer major |
| `/$defs/approvers` | removed | present in the older major, absent at the newer |
| `/$defs/approvers/properties/all_of` | removed | present in the older major, absent at the newer |
| `/$defs/approvers/properties/any_of` | removed | present in the older major, absent at the newer |
| `/$defs/autonomy_tier` | added | new at the newer major |
| `/$defs/budget/properties/limit_usd` | added | new at the newer major |
| `/$defs/budget/properties/max_runtime` | added | new at the newer major |
| `/$defs/budget/properties/max_runtime_minutes` | removed | present in the older major, absent at the newer |
| `/$defs/constraint/properties/allowed_environments` | added | new at the newer major |
| `/$defs/constraint/properties/change_freeze` | added | new at the newer major |
| `/$defs/constraint/properties/diff_coverage` | added | new at the newer major |
| `/$defs/constraint/properties/required_upstream` | added | new at the newer major |
| `/$defs/escalation` | added | new at the newer major |
| `/$defs/escalation/properties/match` | added | new at the newer major |
| `/$defs/escalation/properties/require` | added | new at the newer major |
| `/$defs/escalation_requirements` | added | new at the newer major |
| `/$defs/escalation_requirements/properties/approvals` | added | new at the newer major |
| `/$defs/escalation_requirements/properties/max_autonomy` | added | new at the newer major |
| `/$defs/member-ref` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent/properties/may_approve` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent/properties/may_merge` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent/properties/may_retry` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent/properties/may_route_fixup` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent/properties/may_waive` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent/properties/model_policy` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent/properties/must_page_human` | removed | present in the older major, absent at the newer |
| `/$defs/operator_agent/properties/route_fixup_min_severity` | removed | present in the older major, absent at the newer |
| `/$defs/predicate` | added | new at the newer major |
| `/$defs/predicate/properties/change_kind` | added | new at the newer major |
| `/$defs/predicate/properties/labels` | added | new at the newer major |
| `/$defs/predicate/properties/paths` | added | new at the newer major |
| `/$defs/predicate/properties/trigger` | added | new at the newer major |
| `/$defs/produces/properties/artifact` | changed | enum members differ |
| `/$defs/reviewers_config/properties/agent` | removed | present in the older major, absent at the newer |
| `/$defs/reviewers_config/properties/authority` | added | new at the newer major |
| `/$defs/reviewers_config/properties/review_timeout` | added | new at the newer major |
| `/$defs/role` | removed | present in the older major, absent at the newer |
| `/$defs/role/properties/members` | removed | present in the older major, absent at the newer |
| `/$defs/stage/properties/constraints` | changed | type "array of `constraint`"→"`constraint`" |
| `/$defs/stage/properties/egress` | added | new at the newer major |
| `/$defs/stage/properties/needs` | added | new at the newer major |
| `/$defs/stage/properties/permissions` | added | new at the newer major |
| `/$defs/stage/properties/type` | changed | enum members differ |
| `/$defs/stage_egress` | added | new at the newer major |
| `/$defs/stage_egress/properties/target_hosts` | added | new at the newer major |
| `/$defs/stage_permissions` | added | new at the newer major |
| `/$defs/stage_permissions/properties/network` | added | new at the newer major |
| `/$defs/stage_permissions/properties/shell` | added | new at the newer major |
| `/$defs/stage_permissions/properties/write` | added | new at the newer major |
| `/$defs/workflow/properties/actions` | added | new at the newer major |
| `/$defs/workflow/properties/applies_to` | added | new at the newer major |
| `/$defs/workflow/properties/auto_advance` | added | new at the newer major |
| `/$defs/workflow/properties/autonomy` | added | new at the newer major |
| `/$defs/workflow/properties/defaults` | added | new at the newer major |
| `/$defs/workflow/properties/drive` | removed | present in the older major, absent at the newer |
| `/$defs/workflow/properties/escalations` | added | new at the newer major |
| `/$defs/workflow/properties/extends` | added | new at the newer major |
| `/$defs/workflow/properties/operator_agent` | removed | present in the older major, absent at the newer |
| `/properties/defaults` | added | new at the newer major |
| `/properties/roles` | removed | present in the older major, absent at the newer |
| `/properties/version` | changed | enum members differ |

## The six primitives

MVP_SPEC §4.1 declares six workflow primitives, no more. Each is defined in the schema and demonstrated in a shipped example:

| Primitive | Schema anchor | Example key | Demonstrated in |
|---|---|---|---|
| workflow | `workflow` | `workflows:` | same-document reuse example |
| stage | `stage` | `stages:` | same-document reuse example |
| gate | `gate` | `gates:` | backlog grooming example |
| constraint | `constraint` | `constraints:` | same-document reuse example |
| approver | `approvals` | `approvals:` | backlog grooming example |
| artifact | `produces` | `produces:` | same-document reuse example |

### Example — same-document reuse

```yaml
# Workflow-v2 example — same-document reuse: `defaults` and `extends`
# (E52.4 / #2216).
#
# Validates against ../workflow-v2.schema.json. This file is the SHARED
# CROSS-VALIDATOR FIXTURE: it is read over the module wall by
#
#   TestV2Reuse_SharedFixtureResolvesIdenticallyAcrossValidators
#     in backend/internal/spec  AND  in cli/internal/spec
#
# Each module runs its OWN resolution over this document and asserts the
# result canonicalizes byte-for-byte to ../examples/workflow-v2-reuse.resolved.json,
# the golden resolved document beside it. The golden is a THIRD PARTY to that
# comparison — the two modules are never compared to each other — so a
# backend-only or CLI-only change to the algorithm fails the other module's
# test instead of silently diverging. The backend additionally drives this
# file through the FULL chain (schema validation, v2shape normalization, the
# typed decode and the semantic validator).
#
# THE `defaults:` AND `extends:` KEYS ARE DELIBERATELY PRESENT IN THE GOLDEN.
# The golden captures the document immediately AFTER resolution and BEFORE any
# v2shape normalization — the one point in the pipeline both modules reach
# identically. The reuse keys are stripped later (backend only, before its
# typed decode), so they still appear in the golden by design, not by omission.
#
# WHAT THIS DOCUMENT EXERCISES, rung by rung and rule by rule:
#
#   - file-level defaults.executor + defaults.reviewers + defaults.budget
#   - a base workflow (`base_change`) inheriting all three
#   - a deriving workflow (`feature_change`) with `extends` PLUS its own
#     `defaults`, overriding a file-level key everywhere
#   - a stage overridden BY ID (`apply`), keeping the base's position
#   - a stage declaring its own reviewers (defaults do NOT override it)
#   - a deriving stage declaring ONE agent reviewer where the base declared
#     TWO — the array-replace proof (never concatenate)
#   - a stage whose inputs[].from_stage references a stage existing ONLY in
#     the base (`propose`)
#   - a `human: true` gate stage under a file-level defaults.executor.agent —
#     the executor BRANCH RULE (the agent default is dropped wholesale)
#   - a DELEGATING deploy stage under that same agent default — the branch
#     rule on the highest-consequence branch, where a grafted `agent` key
#     would both break the oneOf and misrepresent who executes a deploy
#   - a base stage carrying the v2 `constraints` OBJECT and the `needs:`
#     shorthand, inherited verbatim and normalized afterwards in its resolved
#     position (the ordering pin: resolution precedes normalization)
#   - a third workflow (`hotfix_change`) extending the deriving one — the
#     TRANSITIVE chain

version: "2"

# THE LOWEST RUNG. Applied to every stage of every workflow below.
defaults:
  executor:
    agent: claude-code
    timeout: 30m
  reviewers:
    human: 1
    agents:
      - provider: claudecode
      - provider: codex
  budget:
    max_runtime: 45m

workflows:
  # ---------------------------------------------------------------- base ---
  base_change:
    description: >-
      The base workflow. Every stage here inherits its executor, reviewers and
      budget from the file-level defaults unless it says otherwise.

    stages:
      # Inherits executor {agent: claude-code, timeout: 30m} wholesale — it
      # declares NO executor of its own, and still satisfies $defs/stage's
      # required [id, type, executor] because resolution runs before schema
      # validation.
      - id: propose
        type: plan
        inputs:
          - source: github_issue
            required: true

      # Declares its own executor KEY-WISE: the file default's agent survives,
      # this stage's longer timeout wins. Carries the v2 constraints OBJECT
      # and the needs: shorthand, both inherited verbatim by every deriving
      # workflow and normalized afterwards in their resolved positions.
      - id: apply
        type: implement
        executor:
          timeout: 60m
        produces:
          - artifact: pull_request
        needs: [propose]
        constraints:
          max_files_changed: 40

      # Declares its own reviewers block. The file default declares BOTH human
      # and agents; this stage declares ONLY human, and the block is taken
      # WHOLE — no agents are supplemented from a rung the author did not
      # write on this stage.
      - id: gate
        type: review
        executor:
          human: true
        reviewers:
          human: 1

  # ------------------------------------------------------------ deriving ---
  feature_change:
    description: >-
      Extends base_change. Its own defaults swap the agent for every inherited
      stage; its own stage declarations still win over that.
    extends: base_change

    # THE MIDDLE RUNG. Above the base's stages, below this workflow's own
    # stage declarations.
    defaults:
      executor:
        agent: codex

    stages:
      # Overrides the base `apply` stage BY ID: it keeps the base's POSITION
      # (second, after the inherited `propose`), the base's constraints
      # object, its produces list and its needs: shorthand, while this
      # declaration's budget wins key-wise over the file default.
      #
      # Its reviewers block declares ONE agent where the file default declares
      # TWO — the array-replace proof. The resolved stage has exactly one
      # agent reviewer, and no `human` key supplemented from the file default.
      - id: apply
        type: implement
        budget:
          max_runtime: 90m
        reviewers:
          agents:
            - provider: codex

      # A NEW stage appended in declaration order. Its from_stage references
      # `propose`, which exists ONLY in the base — the graph-shape check runs
      # post-resolution, so it resolves.
      - id: verify
        type: review
        executor:
          human: true
        inputs:
          - artifact: pull_request
            from_stage: apply

      # THE BRANCH RULE, delegate side. A deploy stage delegating to an
      # external pipeline, under a file-level defaults.executor.agent AND a
      # workflow-level defaults.executor.agent. Both are dropped WHOLESALE —
      # the branchless `timeout` goes with them — so the resolved executor
      # carries the delegate branch alone, with no `agent` key grafted on. It
      # has to be wholesale: $defs/executor's delegate and human branches set
      # unevaluatedProperties: false, so even a surviving stray `timeout`
      # would fail the oneOf on a document whose author wrote nothing wrong.
      - id: ship
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        inputs:
          - artifact: pull_request
            from_stage: apply

  # ---------------------------------------------------------- transitive ---
  hotfix_change:
    description: >-
      Extends the DERIVING workflow, so its stages resolve through the whole
      chain: file defaults -> base_change -> feature_change -> here.
    extends: feature_change

    stages:
      # Overrides the inherited `gate` stage by id. Its `human: true` executor
      # survives every agent default on every rung — the branch rule, human
      # side.
      - id: gate
        type: review
        executor:
          human: true
        budget:
          max_runtime: 10m
```

### Example — backlog grooming (gates and approvals)

```yaml
# Workflow-v2 example — a NON-CODE-CHANGE workflow on the standard stage types
# (E52.7 / #2219, ADR-067 §2).
#
# Validates against ../workflow-v2.schema.json. Parsed end to end by
# TestParseV2_BacklogGroomingExample_ValidatesAndProducesNoDiff in
# backend/internal/spec, so a schema-invalid or drifted example fails that test
# rather than only a manual check-jsonschema run.
#
# WHY THIS FILE EXISTS. The v0 stage-type names read as if every workflow ships
# code. They do not: plan / implement / review are conceptually PROPOSE / APPLY
# / GATE, and ADR-067 §2 settled that the names are RETAINED rather than
# renamed. This workflow is the worked example — it proposes a grooming report,
# gates it on a human approval, and applies the approved mutations. Nothing here
# touches a line of source, and no stage declares the pull_request artifact.
#
# WHAT THAT NOW IMPLIES. At workflow-v2 the post-hoc diff constraints
# (max_files_changed, forbidden_paths, allowed_paths, required_outcomes,
# diff_coverage) bind to what a stage PRODUCES, not to its type name. Adding any
# of them to any stage of this file would be REJECTED, because no stage here
# declares `produces: [{artifact: pull_request}]` — an absent or empty produces
# list reads as "produces no diff" at major >= 2. That is the point: a diff
# constraint on a workflow with no diff can never be evaluated, and v2 says so
# at parse time instead of at run time.
#
# The grooming_report artifact kind is ADR-065 §3's decision and lands HERE
# (E54.3 / #2235): the propose stage below declares it, which is what makes this
# example ALSO the proof that the artifact is declarable on a real propose stage.
# Declaring grooming_report does not make this a code-change workflow — no stage
# declares the pull_request artifact, so the post-hoc diff constraints stay
# unavailable here exactly as described above.
#
# THIS FILE IS THE SHIPPED BACKLOG-GROOMING DECLARATION (E54.4 / #2236), and it
# is the declaration UNDER TEST: the TestShippedGroomingExample_* family in
# backend/internal/spec reads THESE bytes from disk and asserts the resolved
# matrix, the trigger routing, the approval gates and the stage types. A drift
# in this file reddens those tests rather than passing unnoticed.
#
# THE AUTONOMY DECLARATION. Two blocks that look like they overlap and do not:
#
#   autonomy: low   governs the five RUN-DRIVING classes (approve, fixup, waive,
#                   retry, merge). Low delegates none of them.
#   actions: {...}  governs the four BACKLOG-GROOMING classes. Low's expansion
#                   never names a grooming class, so the two sets are DISJOINT
#                   and declaring both is not an explicit-overrides-tier
#                   conflict: hygiene legitimately resolves to auto here.
#
# (An escalation `max_autonomy: low` CEILING is a different operation and DOES
# clamp hygiene to gated — ClampResolvedMatrix downgrades every auto class the
# ceiling tier's expansion does not name.)
#
# WHY EACH GROOMING CLASS SITS WHERE IT DOES:
#
#   hygiene: auto     objective, reversible defect fixes (a missing label, an
#                     absent parent link) — the ONE auto-eligible grooming class,
#                     and the only one for which a backend-evaluable condition
#                     (objective_reversible) exists.
#   ordering: gated   re-ranks the backlog. Not objectively reversible.
#   dedup: gated      closes items as duplicates. Not objectively reversible.
#   scoping: report   decomposes, iceboxes or closes items — the most destructive
#                     class, so it only ever SURFACES a proposal and never acts.
#                     `mode: auto` on any of these three is refused at PARSE time
#                     by the backend, not merely defaulted away.
#
# THE TRIGGER FORM. `applies_to: {trigger: [scheduled, on_demand]}` is the
# non-diff routing declaration (ADR-067): a grooming run arrives periodically or
# on operator demand and carries no diff, so it must not be routed as one.
# `on_demand` HAS a producer as of E54.22 / #2826 — a run started with
# `trigger_source: on_demand`, which appliesto.TriggerFormForSource maps to
# spec.TriggerOnDemand — so this declaration is ENFORCED against real runs
# today, not merely declarable. `scheduled` still has none (no scheduler
# exists), so the declaration is satisfied only through the on-demand form.

version: "2"

workflows:
  backlog_grooming:
    description: >-
      Propose a backlog grooming report, gate it on a human approval, then apply
      the approved mutations. Produces no code diff at any stage.
    auto_advance: true

    # Non-diff routing (ADR-067): a grooming run is periodic or operator-
    # initiated and carries no paths. Satisfied today by a run started with
    # trigger_source: on_demand (E54.22 / #2826); see the header.
    applies_to:
      trigger: [scheduled, on_demand]

    # The five run-driving classes: nothing delegated.
    autonomy: low

    # The four backlog-grooming classes. Disjoint from the tier above.
    actions:
      hygiene:
        mode: auto
        when: objective_reversible
      ordering:
        mode: gated
      dedup:
        mode: gated
      scoping:
        mode: report

    stages:
      # PROPOSE. A `plan`-typed stage need not emit a plan artifact — it emits a
      # proposal, and here the proposal is a grooming report. The artifact binds
      # to the stage TYPE (plan = PROPOSE per ADR-067 §2), not to the workflow
      # name, and a grooming_report-producing stage must declare its schema for
      # the same forward-compatibility reason a plan declares standard_v1.
      - id: groom
        type: plan
        executor:
          agent: claude-code
          timeout: 20m
        inputs:
          - source: github_issue
            required: true
        produces:
          - artifact: grooming_report
            schema: grooming_report_v1
        gates:
          - type: approval
            sla: 24_hours
            # Forge-neutral approval predicate (E39.2 / #1707). At v2 `approvals`
            # is the ONLY approval form: the legacy GitHub-handle `approvers`
            # allow-list and top-level `roles` map were removed (E52.2 / #2214).
            approvals:
              count: 1
              not: [author, agent]

      # APPLY. An `implement`-typed stage applies the approved mutations — here
      # tracker updates, not source edits. It carries no diff constraints and
      # produces no pull_request, which v2 accepts and evaluates accordingly.
      - id: apply
        type: implement
        executor:
          agent: claude-code
          timeout: 30m
        needs: [groom]

      # GATE. A `review`-typed stage is a gate on the applied result. Review
      # stages emit no artifact in any major.
      - id: confirm
        type: review
        executor:
          human: true
        gates:
          - type: approval
            sla: 24_hours
            approvals:
              count: 1
              not: [agent]
```

<!-- END GENERATED workflow-spec -->
