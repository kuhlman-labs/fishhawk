# Workflow spec v2

Reference for `.fishhawk/workflows.yaml` at major version 2. The canonical schema is [`workflow-v2.schema.json`](workflow-v2.schema.json) (JSON Schema Draft 2020-12). Examples live in [`examples/`](examples/).

> **This page is the complete reference for major 2.** Every member a v2 document may declare is documented here. [`workflow-v0.md`](workflow-v0.md) and [`workflow-v1.md`](workflow-v1.md) are the two **frozen** earlier majors: read them to understand a spec that still declares `version: "0.x"` or `"1.x"`, not to look up a v2 field. The per-child record of how v2 diverged from v1 is retained as history in the [appendix](#appendix-what-changed-from-v0v1).

## What a v2 document is

A single YAML file declaring one or more **workflows**. A workflow is an ordered list of **stages**; a stage names who executes it, what it consumes, what it produces, what it may not do, and what must be approved before or after it runs. Fishhawk orchestrates and gates; it holds no build, test, or deploy logic of its own.

```yaml
version: "2"
```

`version` is required and is the single token `"2"`. **v2 has no minor chain.** v0 carried an additive `0.3…0.7` chain and v1 a `1.0…1.6` chain, and both gated field acceptance on the declared minor. v2 does not: **a field is accepted because this schema declares it** (ADR-067 scope item 4). The dotted forms (`"2.0"`, `"2.1"`, …) route to this schema by major but are rejected by the collapsed enum. See [Version routing](#version-routing) for how a document reaches this schema at all.

## Top-level shape

```yaml
version: "2"            # required
defaults:               # optional; file-level executor / reviewers / budget defaults
  executor: {...}
  reviewers: {...}
  budget: {...}
test_conventions: [...] # optional; per-repo test-location rules for the plan-gate test sweep
workflows:              # required; at least one workflow
  <workflow_id>:
    description: "..."
    stages: [...]
```

The document root is closed (`additionalProperties: false`) over exactly these four keys: `version`, `defaults`, `test_conventions`, and `workflows`. There is **no top-level `roles` map** at major 2 — approval membership lives on the gate's own `approvals` block.

`<workflow_id>` and every stage `id` are `snake_case` — `^[a-z][a-z0-9_]*$`.

## Reuse: `defaults` and `extends`

v2 has two SAME-DOCUMENT reuse primitives:

- **`defaults:`** — declarable at **file** level and at **workflow** level, scoped to the `executor`, `reviewers` and `budget` blocks.
- **`extends:`** — on a workflow, naming another workflow key **in the same document** as its base.

Cross-**file** inclusion (`include:`) is deliberately **out of scope** — see ADR-067 (and ADR-057 for the multi-tenant context in which a cross-file surface would have to be designed). Everything below resolves within one `.fishhawk/workflows.yaml`.

Worked example: [`examples/workflow-v2-reuse.yaml`](examples/workflow-v2-reuse.yaml), with its resolved form beside it at [`examples/workflow-v2-reuse.resolved.json`](examples/workflow-v2-reuse.resolved.json).

### Resolution order

For every defaultable block on every stage, the effective value folds in exactly this order, **later winning**:

1. the **file**-level `defaults`
2. the value the **`extends` base stage** declared
3. the **workflow**-level `defaults`
4. the **stage's own declaration**

Rung 3 sits above rung 2 deliberately: a workflow-level default **overrides a value the inherited base stage declared explicitly**, which is what makes *"extend the base but swap the agent everywhere"* expressible. A stage declared on the deriving workflow still wins over all of it.

Chains resolve transitively (`c` extends `b` extends `a`), and a deriving workflow may omit `stages` entirely — it inherits the base's set whole.

### Merge semantics

| Value | Rule |
|---|---|
| object | merges **key-wise**, recursively; the winning rung takes each key |
| array | **REPLACES wholesale** — never concatenated |
| `stages` | the ONE keyed array: merged **by stage `id`** |
| `reviewers` | taken **WHOLE** from exactly one rung — never blended |
| `executor` | key-wise, **except** the branch rule below |

**Arrays replace, they never concatenate.** This is a governance rule, not a convenience: a `reviewers.agents` or `approvals` list that accumulated entries by inheritance would give a stage an approver or a reviewer **its author did not write**. Declaring one agent reviewer where the base declared two resolves to exactly one — the deriving one.

**`stages` is the keyed exception.** Base stages keep their order and positions; a deriving stage whose `id` matches merges onto the base stage **in the base's position**; a deriving stage with a new id is **appended** in declaration order. Reordering is deliberately not expressible. Declaring the same `id` twice in one workflow is **rejected**, because two entries targeting one base stage have no defined merge order.

### `reviewers` is taken whole; `executor` and `budget` merge key-wise

The asymmetry is deliberate. **A block that determines review AUTHORITY is taken as authored or not at all; blocks that carry only execution parameters merge key-wise.**

Consider file defaults `reviewers: {human: 1, agents: [a, b]}` and a stage declaring `reviewers: {agents: [c]}`. A key-wise merge resolves to `{human: 1, agents: [c]}` — the stage's agents correctly replace the default's, but `human: 1` is **supplemented from a block the author never wrote on that stage**. That is not cosmetic: the ADR-027 authority table reads `len(agents) > 0 && human == 0` as **gating** and `len(agents) > 0 && human > 0` as **advisory**, so the supplemented human converts a gating review into an arbitrable one, and nothing in the document says so.

So a stage (or workflow) that **declares** `reviewers` gets that block exactly as authored, with no merge from any defaults rung. Declared or inherited — never blended. A block-level `reviewers.authority` (the explicit authority declaration) travels **with** the block for the same reason: it is authority, so it is taken whole from exactly one rung and is never supplemented onto a stage that declared its own `reviewers`.

`executor` and `budget` keep the key-wise merge: they carry no authority semantics, and block-level replacement would make a file-level `defaults.executor.timeout` useless for every stage that declares an executor, which is nearly all of them.

### The executor branch rule

`$defs/executor` is a `oneOf` over three mutually exclusive branches — `agent`, `human`, `delegate`. The rule has two halves, and both drop the incoming fragment **WHOLESALE** for that stage, branchless keys such as `timeout` included:

1. **Different branch.** A defaults (or base) executor selecting a different branch than the stage's own executor is dropped.
2. **Closed branch.** A defaults (or base) executor of **any** shape — including a branchless `{timeout: 30m}` — is dropped for a stage on the `human` or `delegate` branch. Those two `oneOf` arms declare no property beyond their own branch key.

```yaml
defaults:
  executor:
    agent: claude-code # applies to every agent-executed stage below
    timeout: 30m

workflows:
  feature_change:
    stages:
      - id: apply
        type: implement # inherits {agent: claude-code, timeout: 30m} whole

      - id: gate
        type: review
        executor:
          human: true # resolves to {human: true} — NO agent, NO timeout

      - id: ship
        type: deploy
        executor:
          delegate: # resolves to the delegate block alone
            target: github_actions
            workflow_ref: deploy.yml
```

Without the first half the file-level `agent` default would graft an `agent` key — and every agent-branch-only key — onto each `human: true` gate stage and each delegating deploy stage, and the schema's `oneOf` would then reject a document whose author wrote nothing wrong. The drop is *wholesale* rather than branch-key-only because the `human` and `delegate` branches set `unevaluatedProperties: false`, so even a stray surviving `timeout` fails the branch.

The second half exists because that same rejection returns one case over for a **branchless** default, which the first half cannot see. A file-level `defaults: {executor: {timeout: 30m}}` declares no branch, so it would merge key-wise into `{human: true}` and produce `{human: true, timeout: 30m}` — which the human arm rejects exactly as it rejects a grafted `agent`. Essentially every real workflow carries a human gate stage, so without the closed-branch half the bare-`timeout` default would reject nearly every realistic document. Both halves fail the same way — the default is dropped, never reinterpreted — so a bare `{timeout: 30m}` reaches every agent stage and silently skips every human gate and delegating deploy stage.

### A bare key is an error — not "inherit" and not "blank"

A block written with **no value** — `reviewers:`, `executor:`, `budget:`, or a stage's `type:` with nothing after the colon — is an **error**, reported at the stage's **own path**. It is neither a way to inherit a default nor a way to blank a block:

- To **inherit** the file- or workflow-level default, **OMIT the key** entirely.
- To **state a value**, write it explicitly.

A bare `reviewers:` means neither "no reviewers" nor "take the file default" — it is a null, and `$defs/reviewers_config` (like `$defs/executor` and `$defs/budget`) is `type: object`, so the schema rejects it. Crucially, the rejection does **not** depend on whether a `defaults` block exists elsewhere: the null is preserved as authored and fails at its own path either way, with no action at a distance.

```yaml
defaults:
  reviewers:
    human: 1

workflows:
  feature_change:
    stages:
      - id: apply
        type: implement
        # reviewers OMITTED -> inherits {human: 1} from the file defaults

      - id: gate
        type: review
        reviewers:      # ERROR: a bare key is null, rejected at
                        # /workflows/feature_change/stages/1/reviewers —
                        # NOT silently replaced by the file default
```

The same holds for a `defaults` block whose own key is bare (`defaults: {reviewers:}`): nothing is applied to any stage and the null is rejected at the defaults path, rather than being fabricated onto every stage.

### Rejections

Two authoring errors are rejected before schema validation, with messages naming the offender:

```
extends names workflow "nope", which this document does not define; defined workflows: alpha, zebra
extends forms a cycle: alpha -> beta -> alpha; a workflow cannot inherit from itself, directly or transitively
```

A self-reference (`solo` extending `solo`) reports as the same cycle. A duplicate stage id inside one workflow's own `stages` list is rejected the same way, naming the id and both positions.

### Where errors point

**Resolution runs BEFORE schema validation**, so the schema (and every semantic rule after it) validates the **resolved** document. That is what lets a stage inherit its `executor` and still satisfy `$defs/stage`'s `required: [id, type, executor]` with no schema relaxation, and lets `inputs[].from_stage` reference a stage that exists only in the inherited base.

The cost is the same tradeoff `needs:` expansion accepts: a reported error **path** points at a position in the resolved document, which for a deriving workflow may not be a position its author wrote. Read the path against the resolved form, not the source.

Post-resolution enforcement has one operator-facing consequence: a **bare `check-jsonschema` run resolves no reuse**, so it rejects a valid reuse-bearing document — a stage that inherits its `executor` trips `$defs/stage`'s `required` list when the raw bytes are validated directly. Use `fishhawk validate` (or let the backend validate), which resolves first. This is the same asymmetry the action-class rules note the other way round (a raw run *accepts* a document the backend *rejects*): the bare schema is neither a superset nor a subset of what the product accepts, so it is not the authority for a reuse-bearing spec.

The `defaults` and `extends` keys are stripped after validation, before the typed decode. No Go struct, database column, API field or consumer read site sees either key.

## Workflow members

```yaml
workflows:
  feature_change:
    description: "..."      # optional; free-form
    applies_to:             # optional; the changes this workflow may be used for
      labels: [dependencies]
    extends: base_change    # optional; another workflow key in this document
    autonomy: medium        # optional; tier shorthand (low | medium | high)
    actions: {...}          # optional; per-action-class delegation matrix
    auto_advance: false     # optional; auto-advance mechanical transitions
    policy:                 # optional
      max_stage_runtime: 30m
    on_ci_failure:          # optional
      max_retries: 1
    budgets: [...]          # optional; periodic USD ceilings across runs
    decomposition:          # optional
      max_parallel: 3
    defaults: {...}         # optional; workflow-level reuse defaults
    stages: [...]           # required (or inherited whole via extends)
```

| Member | Meaning |
|---|---|
| `description` | Free-form prose. Carried, never interpreted. |
| `applies_to` | A [path predicate](#path-predicate) declaring which changes may use this workflow — see [Workflow routing](#workflow-routing-applies_to). Absent means *any* change. |
| `extends` | Names another workflow in this document as this one's base — see [Reuse](#reuse-defaults-and-extends). |
| `autonomy` | Tier shorthand expanding to a full action matrix — see [Autonomy](#autonomy-tier-shorthand-and-action-matrix). |
| `actions` | The per-action-class delegation matrix, and the home of the two reserved keys `page_human_on` and `model_policy`. |
| `auto_advance` | Opt-in auto-advancement of mechanical run transitions. |
| `policy` | Workflow-level execution envelope; its one member is `max_stage_runtime`. |
| `on_ci_failure` | Auto-retry policy for a failed required check on the implement stage's PR. |
| `budgets` | Recurring USD ceilings across **all** runs of the workflow. |
| `decomposition` | Decomposition controls; its one member is `max_parallel`. |
| `defaults` | Workflow-level `executor` / `reviewers` / `budget` defaults — rung 3 of the resolution ladder. |
| `stages` | The ordered stage list. Required, unless inherited whole through `extends`. |

### `policy.max_stage_runtime`

The workflow's wall-clock envelope for agent stages, as a Go duration string (`"30m"`, `"1h"`, `"90s"`). It is the middle rung of a three-level precedence, highest first:

1. `stage.executor.timeout` — the exception; use when one stage's SLO differs from the rest.
2. `policy.max_stage_runtime` — the spirit; the workflow's overall envelope.
3. The backend default (15 minutes) — when neither is set.

The backend resolves this at prompt-fetch time and delivers the resolved value to the runner; the runner applies its own 15-minute fallback when the delivered value is zero.

### `on_ci_failure.max_retries`

```yaml
on_ci_failure:
  max_retries: 1 # integer 0..5; default 1 when the block is absent
```

When a **required** CI check fails on the implement stage's PR, the dispatcher fires a fresh implement dispatch up to `max_retries` times. A chain of length N means the agent is dispatched N+1 times total. `0` disables auto-retry — useful for a low-autonomy workflow where a human prefers to re-trigger after inspecting the failure.

Only the closed set of failing conclusions (`failure`, `timed_out`, `cancelled`, `action_required`, `stale`, `startup_failure`) fires a retry; `success` / `neutral` / `skipped` are no-ops. `fishhawk_audit_complete` failures are **excluded** — retrying does not fill Fishhawk's own audit gaps. Only failures of checks in the run's branch-protection snapshot count, so a failing third-party non-required check triggers nothing.

### `auto_advance`

```yaml
auto_advance: true # boolean; default false
```

Opt-in auto-advancement of the run transitions that carry no judgment content — plan-approved → implement dispatch, review verdicts settling a gate, fix-up-pushed re-review re-park, and all-gates-resolved + checks-green parking at a derived `awaiting_merge`. Each advance records a `run_auto_advanced` audit entry naming the transition rule. Judgment points (gate approvals, concern routing, merge) always park for the operator.

The workflow-level value is the default for every run of the workflow; `POST /v0/runs` accepts a per-run override that wins over it. The resolved flag is persisted on the run row at create time, so a spec edit mid-run does not change an in-flight run's behaviour.

### Periodic budgets (`budgets`)

A workflow-level list of recurring cost ceilings (ADR-030). Each entry caps total USD spend across **all runs** of the workflow within a calendar period, resetting at the period boundary. This is distinct from the per-stage [`budget`](#budget-per-stage), which governs one stage execution.

```yaml
budgets:
  - period: weekly            # weekly | monthly
    limit_usd: 50             # ceiling in USD for the period (> 0)
    enforcement: advisory     # advisory (default) | blocking
    warn_at: 0.8              # optional fraction [0,1]
```

- **`period`** — `weekly` resets at the start of the ISO week; `monthly` on the first of the month. Boundaries are timezone-aware, computed in the backend's `FISHHAWKD_BUDGET_TIMEZONE` (default UTC).
- **`limit_usd`** — the ceiling, summed from each run's recorded cost total across the workflow's runs created within the current period.
- **`enforcement`** — `advisory` (default) appends a `budget_alert` audit entry and posts an advisory comment on the triggering issue when period spend crosses `warn_at` and again at 100%, each deduped so either tier fires at most once per period; runs never block. `blocking` refuses a **new** run at admission once the period spend exhausts `limit_usd`, never touching an in-flight run — admission-time refusal is not yet implemented, so a `blocking` budget currently records spend and produces no automatic refusal.
- **`warn_at`** — the fraction at which the advisory warning fires ahead of the 100% crossing. Absent means only the 100% threshold is surfaced.

> Cost honesty caveat: bundles that report no known usage undercount spend, so a ceiling may be crossed later than true spend would imply. Period spend is a lower bound on actual spend, and the advisory comment repeats the caveat so a reader does not treat the figure as exact.

### `decomposition.max_parallel`

```yaml
decomposition:
  max_parallel: 3 # integer >= 0; 0 = unlimited
```

The maximum number of decomposed child runs that may dispatch **concurrently** for a run of this workflow. `0` — and an absent block — means unlimited. It is a per-workflow override of the global `FISHHAWKD_MAX_PARALLEL_CHILDREN` operator default: when `max_parallel > 0` the knob wins, otherwise the global default applies.

### Workflow routing (`applies_to`)

```yaml
applies_to: # optional; a $defs/predicate — see "Path predicate" below
  labels: [dependencies, documentation]
  paths: ["docs/**"]
  trigger: [diff]
```

`applies_to` declares **which changes this workflow may be used for**. It is a [path predicate](#path-predicate) — the same match rule `escalations` and the review-conventions consume, used verbatim rather than reimplemented — so the criteria and their combination rules are the predicate's: AND across declared criteria types, OR within a list, and an undeclared type does not constrain. The one documented divergence is the **quantifier applied to `paths`**, described under "Enforcement" below: the predicate's `paths` rule is existential (*any* change path matching *any* glob), and a confinement control needs the universal reading (*every* path accepted). The quantifier is applied around the ratified matcher, not inside a second one.

A workflow declaring **no** `applies_to` accepts any change. That is what leaves every document written before this property existed unaffected, and it is why the absent case is *not* an empty predicate (an empty predicate is an authoring error, never match-all).

**Enforcement is fail-closed, in two phases.** Each criterion is evaluated at the earliest point in a run where something actually *produces* the value it matches against:

| Criterion | Evaluated at | Against |
|---|---|---|
| `labels` | run admission — `POST /v0/runs` **and the webhook dispatch path** | the run's issue-context label snapshot (API) or the forge-supplied `issue.labels[]` (webhook) |
| `trigger` | run admission (both admission points) | the run's trigger form |
| `paths` | the **plan gate** | the plan's `scope.files`, **unioned** with every `decomposition.sub_plans[].scope.files` and `split_proposal.phases[].scope.files`. Declaring it on a workflow with no plan stage is **refused at validation** (E53.15 / #2377) — see below |

Both admission points evaluate the **same** `labels` / `trigger` predicate through one shared core. The webhook dispatch path (`issues.labeled`, `/fishhawk run`) creates runs directly, so without evaluating `applies_to` there an operator's routing declaration would silently not hold for webhook-triggered runs (E53.10 / #2361). The webhook seam evaluates against the **forge-authoritative** `issue.labels[]` on the event payload — the forge's own view of the issue, stronger provenance than the caller-attested snapshot the API path receives — and refuses fail-closed before creating any run row, surfacing the refusal as a comment on the triggering issue. `paths` needs no webhook-specific work: a webhook-created run reaches the same plan gate as any other.

The `paths` deferral is not a weakening. At admission a code-change run has no paths yet — nothing has proposed a diff — so evaluating `paths` there could only match against zero paths, which the AND-across-types rule turns into a blanket refusal. The plan gate is the first point where a path set exists, and the set it checks is `scope.files`, which is **binding rather than descriptive**: the existing scope gate confines the implement stage to it. A run admitted under a workflow declaring `paths: ["docs/**"]` is therefore *confined* to `docs/**`, not merely claimed to be. Both rejection points fire before any implement work, so a refusal costs a re-run and never half-applied work.

Three properties of the `paths` evaluation follow from that, and none of them is the predicate's default reading:

- **The quantifier is universal.** *Every* path in the set must be accepted by the declaration, not merely one of them. Under the predicate's own existential rule a plan scoping `[docs/x.md, backend/everything.go]` would satisfy `paths: ["docs/**"]` and the confinement guarantee above would be false. The rejection message names the offending paths (capped, with the elided count reported).
- **The set is the union, not the top level.** A decomposition fan-out child runs bounded to its *slice* scope and a `split_proposal` phase carries its own, so a top-level-only check would let a slice reach outside the declaration.
- **A plan committing to no files at all is refused**, not admitted on a vacuous "every one of zero paths matched". Nothing in an empty scope demonstrates the plan falls inside the declaration.

**A `paths` criterion on a workflow that declares no plan stage is refused** (E53.15 / #2377). Because `paths` is evaluated at the plan gate, a workflow with no `plan` stage produces no `scope.files` set for it to check, so the criterion could never be evaluated — no rejection, no audit entry, no signal that the declaration did nothing. Both the backend and `fishhawk validate` refuse it at parse time, in the same shape as the `change_kind` rejection below, with a message naming the workflow, the missing plan stage and the two alternatives. The refusal is **unconditional**: no other control admits the declaration, and in particular a stage's `constraints.allowed_paths` does not, because the *routing* criterion is still never evaluated.

The load-bearing reading for an author is unchanged by the refusal, only made explicit: a workflow that must be path-confined at routing time needs a plan stage, and `labels` / `trigger` are the criteria that hold on *every* workflow regardless of shape. The third option is the stage's own `constraints.allowed_paths`, a **post-hoc** envelope checked against the produced diff after the agent has run rather than at routing — a real control, with different timing, and the accurate description of it. In this repository only `feature_change` declares a plan stage, so `paths` is declarable on exactly one of the four workflows; `routine_change` states its `allowed_paths` envelope in those terms.

The rule reads the **`extends`-resolved** stage list: a plan stage inherited from a base satisfies it, because reuse resolution folds stages before validation and it is the resolved shape that decides whether a `scope.files` producer exists. It also **composes forward** — should a future stage type produce an authoritative pre-implementation path set, the refusal relaxes on its own by admitting that type as a producer, rather than needing the rule repealed.

**The trust boundary differs by admission path.** On `POST /v0/runs` issue labels are fetched by the caller and shipped inline on the create request, so a caller determined to route a change through the wrong workflow can attest whatever labels it likes — there, `applies_to` prevents **misrouting**, not a determined authorized caller, and server-side label fetching is the named hardening path. The **webhook** path is stronger: the labels are the forge's own `issue.labels[]`, not a caller attestation. The sanctioned exception is the audited override, and it exists **only on `POST /v0/runs`** (`applies_to_override` plus a required reason), which admits the run and records why — an escape hatch that leaves a trail. A **webhook trigger carries no `applies_to_override`**: the event carries no operator request, so there is nowhere to pass one, and the webhook refusal comment names amending the declaration or re-starting the run through `POST /v0/runs` / the CLI instead. An operator who needs the override re-starts the run through the API path where it is available.

**An issue-less run resolves to an empty label set and is refused.** A `cli` or `ui` run carries no issue context, so a workflow declaring `labels` cannot be satisfied by it. This is fail-closed by design; the refusal names the absent issue context specifically, rather than reporting a generic predicate mismatch that would send the operator looking for a label that was never going to be there.

**`change_kind` is rejected inside `applies_to`.** The shared predicate grammar carries the criterion for its other consumers, but nothing populates a change kind today, so a workflow declaring it would be selectable by no run at all — a state indistinguishable, from the operator's side, from the routing control being broken. Both the backend and `fishhawk validate` refuse it at parse time with a message naming the missing producer.

**The non-diff `trigger` forms have no producer either, but are accepted.** Unlike `change_kind` they are *partially* usable: `trigger: [diff, scheduled]` still matches real runs today, and the `scheduled` / `on_demand` arms route correctly the moment a scheduled groomer or an on-demand intake starts producing them. A predicate declaring **only** `scheduled` or **only** `on_demand` therefore matches nothing today — that is expected, not a bug.

**Overlap is benign by construction.** `applies_to` *filters* a workflow the operator named; it never *selects* one. Two workflows whose predicates both match a change is not a coin flip — the requested `workflow_id` is admitted if its own predicate is satisfied, and the other workflow is simply another one that would also have been legal.

`extends` folds **stages** only, so a deriving workflow does not inherit its base's `applies_to`; declare the routing predicate on each workflow that needs one. This matches every other non-stage member (`budgets`, `policy`, `decomposition`, `autonomy` are likewise not inherited).

> `applies_to` is **enforced on every path that can start a run**: the schema accepts it, the parser round-trips it, both validators check it, and every run consults it. A run whose labels or trigger do not satisfy the declaration is refused at admission — on `POST /v0/runs` **and** on the webhook dispatch path (`issues.labeled`, `/fishhawk run`), both through the same shared evaluation core — and a plan whose `scope.files` reaches outside a declared `paths` is refused at the plan gate. Every refusal names the workflows that *would* accept the change, evaluated at **both** phases, so a named alternative cannot turn out to reject the same run at admission. The audited `applies_to_override` (with its required reason) is available on `POST /v0/runs`; a webhook trigger carries no override, so its refusal is unconditional.

### Escalations

```yaml
escalations: # optional; each entry RAISES the requirements for a matching change
  - match: # a $defs/predicate — see "Path predicate" below
      paths: ["infra/**", "**/*.tf"]
    require:
      approvals:
        count: 2 # the workflow's approval gates declare count: 1
        min_permission: admin # they declare min_permission: write
        member_of: platform/security # no gate requires this group
      max_autonomy: low # the workflow declares autonomy: high
```

`escalations` declares **what gets stricter for a change matching a predicate**. Each entry pairs a `match` [path predicate](#path-predicate) — the same shared matcher `applies_to` consumes, not a second one — with a `require` block, and a change satisfying the predicate has the block's requirements *raised* — a higher approval `count`, an additional `member_of` group, a stricter `min_permission`, a lower `max_autonomy` ceiling. A workflow declaring no `escalations` is on the unchanged path: nothing is raised, and the enforcement seams short-circuit before any extra read.

**The `match:` wrapper is structurally forced, not stylistic.** `$defs/predicate` is `additionalProperties: false`, and JSON Schema Draft 2020-12 evaluates `additionalProperties` against the *whole instance object* rather than the referencing subschema's own scope. An escalation therefore **cannot** `$ref` the predicate and carry a sibling `require` key — the hoisted form

```yaml
escalations:
  - paths: ["infra/**"] # NOT valid: the predicate is a closed object
    require: { max_autonomy: low }
```

is rejected by the schema. Writing it that way would mean re-declaring the predicate's properties inline, which is the second-matcher drift the shared predicate exists to prevent now that it backs `applies_to` across three seams. The worked example at the top of this section is the implementable form.

**An escalation may only ever RAISE**, and that is a validation error rather than a convention. A declared value that does not exceed the workflow's baseline — or that composes to the baseline unchanged — is refused at parse time by the backend *and* by `fishhawk validate`, so a block an operator reads as a control can be trusted to actually do something. The four dimensions are not all checked the same way:

| Dimension | Check | Baseline it is measured against |
|---|---|---|
| `approvals.count` | **raise** — must exceed | the **highest** `count` any approval gate declares |
| `approvals.min_permission` | **raise** — must be stricter | the **strictest** tier any approval gate declares (absent = no constraint, so any tier raises) |
| `approvals.member_of` | **no-op** — refused only when every gate already names it | the group set every applicable approval gate already requires |
| `max_autonomy` | **no-op** — refused when the clamp changes nothing | the workflow's resolved action matrix, plus each gate declaring its own `autonomy` / `actions` block |

The baseline for the two raise-checks is the workflow's **least-restrictive** declaration, because a workflow-level escalation applies to *every* approval gate: one that raised gate A while leaving gate B untouched would not hold the invariant workflow-wide. The rejection names the baseline, so an author is not left guessing which gate they failed.

**Neither no-op check is a lowering check**, and conflating the two misreads the control. Membership composes as a *conjunction* over a de-duplicated set, so adding a group can only ever narrow the eligible approver set — no lowering is expressible. The autonomy clamp only ever downgrades `auto` to `gated`, so a ceiling is monotone-decreasing by construction — again no lowering is expressible. What *is* expressible on both dimensions is an escalation that raises **nothing**: a `member_of` every approval gate already requires de-duplicates away to the identical set, and a ceiling no higher than what the workflow already resolves to leaves every action class exactly as it was. Those are what the two no-op checks refuse. A group named by *some* but not all approval gates is accepted — it genuinely raises the gates that omit it.

A `require.approvals` block on a workflow declaring **no approval gate at all** is refused too: there is nothing anywhere for it to raise. Such a workflow may still declare `max_autonomy`, which acts through delegation rather than through a gate.

**Composition across several matching escalations is the strictest per dimension, and therefore order-independent.** `count` composes as the max, `member_of` as the sorted de-duplicated union (a **conjunction** — an approver must belong to *every* composed group), `min_permission` as the strictest tier, `max_autonomy` as the lowest tier. Max, min and set union are commutative, so shuffling the declaration order cannot change the result: there is no last-match-wins to reason about. One consequence follows from the conjunction and is intended: two escalations naming disjoint groups produce a requirement no single approver can satisfy. For a control that may only raise, that surfaces as a gate which cannot clear — the fail-closed reading — rather than one silently weakened.

**`max_autonomy` is a ceiling on AGENT autonomy** — equivalently a floor on human involvement. It is applied **last**, over the *fully resolved* action matrix: after the tier expansion and after every explicit `actions` override. Every class at `mode: auto` that the ceiling tier does not also hold at `auto` is downgraded to `gated`, its `when` condition dropped and its provenance recorded as `escalation`; `gated` and `report` classes are untouched (neither delegates), and an extension class the ceiling tier does not name clamps to `gated`, fail-closed. Applying the ceiling to the tier or to the declared entries instead would let an explicit `actions: {merge: {mode: auto}}` re-widen the class afterwards, which is exactly the failure the last-position rule exists to prevent. The clamp also re-derives the operator-agent knob block, because that — not the surfaced matrix — is what every enforcement site reads.

**Enforcement is at both seams, and a fired escalation is audited at every seam it fires at.** The approval gate raises the required count, the membership conjunction and the minimum permission (at the quorum count *and* at the pre-submit authorization check, so the two can never disagree); delegation resolution applies the ceiling. A `max_autonomy`-only escalation on a workflow with no approval gate changes behaviour purely through delegation, and is audited there rather than going unrecorded.

`paths` is evaluated against the **union** of the plan's `scope.files` and every `decomposition.sub_plans[].scope.files` and `split_proposal.phases[].scope.files`, the same union `applies_to` uses — a fan-out slice bounded to its own scope must not be able to reach an escalated path without escalating.

**`change_kind` is rejected inside `match`**, for the same reason `applies_to` rejects it: nothing produces a change kind today, so the escalation could never fire, which is indistinguishable from the control being broken.

**A `match.paths` criterion on a workflow that declares no plan stage is refused** (E53.16 / #2382), the escalation twin of the `applies_to.paths` rule above. Because `paths` is evaluated at the approval gate against the approved plan's `scope.files` union, a workflow with no `plan` stage produces no set for it to match against, so the escalation could never fire — worse than the routing case, because the control fails to *raise* rather than merely fails to route. Both the backend and `fishhawk validate` refuse it at parse time, reading the **extends-resolved** stage list (a plan stage inherited through `extends` satisfies it). The refusal is **unconditional**: no other control admits the declaration — in particular a stage's `constraints.allowed_paths` does not, because the escalation is still never evaluated — and it is reported last, after the `change_kind` and only-ever-raise diagnoses, so the sharper spelling error wins. It composes forward: a future stage type producing an authoritative pre-implementation path set relaxes the refusal by joining the producer set rather than repealing the rule. `labels` / `trigger` matches are unaffected, as are the `require` dimensions (`approvals`, `min_permission`, `max_autonomy` under a non-paths match) that do not depend on paths.

`extends` folds **stages** only, so a deriving workflow does not inherit its base's `escalations`; declare them on each workflow that needs them, exactly as with `applies_to`.

## Stages

```yaml
stages:
  - id: apply                 # required; snake_case, unique within the workflow
    type: implement           # required; plan | implement | review | deploy | acceptance
    executor: {...}           # required (or inherited from defaults / a base stage)
    inputs: [...]             # optional; what this stage consumes
    needs: [plan]             # optional; shorthand for artifact wiring
    produces: [...]           # optional; what this stage records
    constraints: {...}        # optional; object keyed by constraint kind
    budget: {...}             # optional; per-stage caps
    gates: [...]              # optional; approval / check gates
    reviewers: {...}          # optional; agent + human review policy
    egress: {...}             # optional; egress allowance (enforced on an acceptance stage)
    permissions: {...}        # optional; declared network/write/shell (declaration-only)
```

Stage `id` is unique within the workflow and is what `inputs[].from_stage` and `needs` reference. `type` is the closed five-token enum below. `executor` names who runs the stage and is required on the **resolved** document — a stage may omit it and inherit one from a `defaults` block or an `extends` base.

## Stage types: propose / apply / gate

The stage-type enum is five tokens: `plan`, `implement`, `review`, `deploy`, `acceptance`. The v0 names were coined for code-change workflows and read as if every workflow ships a diff. They do not. **The names are retained deliberately** (ADR-067 §2 — *do not rename*: renaming would churn every existing spec, the `stages.type` column, the API and the runner for a cosmetic gain), so each type carries two readings — the one a code-change workflow reads, and the general one every other workflow reads.

| Type | Code-change reading | General reading | Produces a diff? |
|---|---|---|---|
| `plan` | propose an implementation plan | **propose** — emit a proposal of any kind (a plan, a grooming report, a migration inventory) | no |
| `implement` | write the code | **apply** — apply the proposed change, whether that is source edits or tracker mutations | only if it says so |
| `review` | review the diff | **gate** — a decision point on the result | no |
| `deploy` | delegate a release (ADR-038) | delegate any externally-owned action | no (delegated) |
| `acceptance` | validate a running instance (ADR-049) | emit a verdict against declared criteria | no |

A workflow that grooms a backlog — propose a report, gate the proposal on an approval, apply the approved mutations, then confirm the result — is spelled with `plan` → `implement` → `review` and produces no code at any stage. Its first approval gate sits on the `plan` stage, because that is where the judgment is, and a second closes the `review` stage. Worked example: [`examples/workflow-v2-backlog-grooming.yaml`](examples/workflow-v2-backlog-grooming.yaml) (`groom` → `apply` → `confirm`).

### `plan` — propose

Code-change reading: an agent (or a human) reads the triggering issue and writes an implementation plan — the `standard_v1` plan artifact, with its scope file list, approach, and verification strategy.

General reading: a `plan` stage **proposes**. The artifact is a proposal of whatever kind the workflow is about: a grooming report naming the issues to retitle, an inventory of call sites a migration must touch, a release-notes draft. Nothing in the type requires the proposal to describe code.

- **Executor branches:** `agent` or `human`. Not `delegate` — that is deploy-only.
- **Produces:** the `plan` artifact, which must carry `schema: standard_v1`. A stage producing `plan` and nothing else produces no diff.
- **Reviewers:** the `reviewers` block has runtime effect here — agent and/or human plan review, with authority resolved per ADR-027.
- **Gates:** an approval gate on a plan stage is enforced by Fishhawk. The vote approves intent before any change is applied.

### `implement` — apply

Code-change reading: the agent writes the code against the approved plan, and the runner commits the scope-only tree and opens a pull request.

General reading: an `implement` stage **applies** the proposed change. Whether that means source edits, tracker mutations, or an inventory rewrite is the workflow's business. A stage produces a diff only if it **says so** by declaring the `pull_request` artifact — which is also what makes a post-hoc diff constraint valid on it (see [Constraints](#constraints)).

- **Executor branches:** `agent` or `human`.
- **Produces:** `pull_request` for a code change; nothing, for a stage whose effect is external.
- **Reviewers:** the `reviewers` block has runtime effect here — the implement-review loop reuses the same config and the same ADR-027 authority semantics as plan review.
- **Constraints:** the post-hoc diff kinds bind here, and `diff_coverage` is valid on this type only.

### `review` — gate

Code-change reading: a reviewer reads the diff and approves, requests changes, or rejects.

General reading: a `review` stage is a **gate** — a decision point on the result of the preceding stage. It produces no artifact of its own; its output is a verdict and the gate transition that follows.

- **Executor branches:** `agent` or `human`.
- **Produces:** nothing — a review stage emits no artifact in any major. That is why `needs:` rejects a `review` referent: there is no default artifact to derive, so a later stage that must consume something from it declares the wiring longhand.
- **Gates:** for a review stage, `approvals` is Fishhawk's own predicate; branch protection's required-reviewers remains the forge-side gate on the PR itself. A review stage carrying a `check` gate makes Fishhawk queue an auto-merge against the implement stage's PR and step out of the way.

### `deploy` — delegate

Code-change reading: hand a release to an external pipeline (ADR-038) and capture its outcome.

General reading: a `deploy` stage **delegates**. It is the one type Fishhawk deliberately holds no logic and no credentials for: it triggers something it does not own, gates the trigger, and records the result.

- **Executor branches:** `delegate` **only**. A deploy stage MUST use `executor.delegate` and MUST NOT use `agent` or `human`; conversely no non-deploy stage may use `delegate`.
- **Produces:** the `deployment` artifact, valid on this type only.
- **Constraints:** the three **pre-flight** kinds (`allowed_environments`, `change_freeze`, `required_upstream`) are valid here and nowhere else; the post-hoc diff kinds are **not** valid here, because a delegating deploy produces no reviewable diff.
- **Gates:** an approval gate on a deploy stage is a pre-execution gate — it blocks before the external pipeline is triggered.

### `acceptance` — verdict

Code-change reading: validate a running instance of the change against its acceptance criteria (ADR-049), on the same execution shape as `review`.

General reading: an `acceptance` stage emits a **verdict** with evidence. It adds no new stage states — it rides the ordinary agent-stage lifecycle — and it is advisory: the verdict is arbitrated, not self-executing.

- **Executor branches:** `agent` or `human`. Not `delegate`.
- **Produces:** the `acceptance` artifact, valid on this type only — the durable evidence record (`{verdict, per-criterion results, content-hash references to evidence blobs}`).
- **Constraints:** the pre-flight deploy kinds are not valid here.
- **`egress`:** the egress allowance below. On an acceptance stage it is **enforced** today by the runner's default-deny proxy; the same block is declarable on any other agent stage as a declaration only (see [Permissions](#permissions-network--write--shell)).

#### `egress.target_hosts`

```yaml
- id: acceptance
  type: acceptance
  executor:
    agent: claude-code
  egress:
    target_hosts:
      - staging.example.com
      - preview.internal.example.com:8443
  produces:
    - artifact: acceptance
```

The `egress` block (ADR-050; generalized E53.5 / #2228) declares the **target-instance host(s)** a stage's agent may reach.

- `egress.target_hosts` is required within the block and takes at least one entry. Each entry is `host` or `host:port` — never a URL; the schema pattern rejects a scheme, a path, and a wildcard. An entry without a port permits the default HTTP/HTTPS ports (80, 443) only; an entry with a port permits exactly that port.
- On an **acceptance** stage these entries are the **single customer-controlled slot** of the acceptance agent's default-deny egress allow-list, and they are **enforced** today: the runner adds the model API endpoint and the Fishhawk backend itself (neither declarable here), the acceptance invocation is forced through the runner-embedded egress proxy, destinations outside the composed allow-list are refused `403`, hostname resolutions are DNS-pinned against rebinding, and a public hostname resolving into loopback or private space is refused outright.
- On **any other** agent stage (v2 loosened the acceptance-only binding) an `egress` block — equivalently the `permissions.network` spelling below — is **declaration-only**: surfaced and audited but **not enforced until E51 (#2133)**. Below major 2 the block stays acceptance-stage-only.
- The first acceptance-stage `target_hosts` entry is also rendered into the acceptance prompt's target-instance section in full URL form — a schemeless `host:port` gains an `http://` prefix. That prefix applies to the **prompt seam only**; the allow-list itself keeps the verbatim `host:port` grammar. A spec with no `egress` block renders an explicit not-declared line.

## Permissions (`network` / `write` / `shell`)

The optional per-stage `permissions` block (E53.5 / #2228) is a **declaration-only** record of what a stage's agent is expected to do: the network destinations it reaches, the paths it writes, and its shell posture. It is **validated, audited and surfaced but not enforced until E51 (#2133)** — do not rely on it as containment; enforcement is a separate epic. It is valid on any **agent-executor** stage and rejected on a `human` or `delegate` executor, because a stage that runs no agent has no such declaration to make.

```yaml
- id: apply
  type: implement
  executor:
    agent: claude-code
  permissions:
    network:
      target_hosts:
        - api.internal.example.com:8443
    write:
      - "src/**/*.go"
      - "docs/**"
    shell: restricted
```

- **`permissions.network`** is a **spelling of `egress`**, not a parallel key: it carries the identical `target_hosts` grammar and is normalized into the stage's `egress` field at parse time, so an acceptance stage declaring `permissions.network` lands on the one enforced path exactly as `egress` does. Declaring **both** `egress` and `permissions.network` on one stage is a **validation error**, never a precedence rule — the quiet way to ship a silently-differing allow-list.
- **`permissions.write`** is a list of doublestar globs (`**` crosses `/`) naming the paths the agent is expected to write. They are validated and matched through the **same shared [path predicate](#path-predicate)** as `applies_to` and `escalations`, so the write dialect cannot drift.
- **`permissions.shell`** is a closed posture enum: `none`, `restricted`, or `unrestricted`.
- An empty `permissions: {}` is an authoring error (the schema requires at least one of `network`, `write`, `shell`).

The declaration is surfaced on the run-status read (`permissions[]`, each entry carrying an honest per-entry `enforced` flag — true only for an acceptance stage's network declaration) and recorded once per run as a `stage_permissions_declared` audit entry with an explicit `enforced: false` at the feature level.

## Executor

`executor` is a `oneOf` over three mutually exclusive branches. Exactly one of `agent`, `human`, `delegate` is declared, and each branch is closed — a key from another branch is a schema error, not a tolerated extra.

### The `agent` branch

```yaml
executor:
  agent: claude-code        # provider id; claude-code (default) or codex
  model: claude-opus-4-8    # optional; per-stage model override
  agent_version: ">=2.1 <2.2" # optional; semver comparator RANGE
  timeout: 20m              # optional; per-stage wall-clock cap
  agent_self_retry: false   # optional; default false (ADR-023)
  verify:                   # optional; in-band test gate
    command: 'scripts/test verify'
    timeout: 10m
    max_iterations: 0
```

- **`agent`** — a free-form string the runner resolves to a coding-agent adapter. Two providers ship: `claude-code` (the default — Anthropic's Claude Code CLI, reading `ANTHROPIC_API_KEY`) and `codex` (the OpenAI Codex CLI, reading `OPENAI_API_KEY`). An unknown id fails the stage category-A before the agent is invoked.
- **`model`** — a per-stage model override, one rung of the implement-model resolution ladder (lowest precedence first): deployment default < `executor.model` < the plan artifact's `model_recommendation` < the operator's gate decision. The highest non-empty rung wins; an empty resolved model spawns the agent on the deployment default. The resolved model is validated against the deployment's per-adapter allowed-model set at the approval gate, and an unknown model is rejected there, naming its source.
- **`timeout`** — a Go duration string; the highest rung of the [stage-timeout precedence](#policymax_stage_runtime).
- **`agent_self_retry`** — opt-in (default `false`, per ADR-023): the agent may perform one self-initiated retry when it detects a recoverable failure, before the workflow's `on_ci_failure` policy applies.
- **`agent_version`** — see [Agent version compatibility](#agent-version-compatibility) below.

### The `verify` gate

An in-band test gate that fires after the agent exits cleanly and before the bundle is committed. When `verify` is absent the gate is skipped.

| Field | Meaning |
|---|---|
| `command` | Executed via `sh -c`; a non-zero exit is a category-A failure. |
| `timeout` | Go duration string; defaults to 10 minutes when absent. |
| `max_iterations` | Verify-fix loop budget, integer `>= 0`, default `0`. |

`max_iterations: 0` is a single-shot demote-on-failure gate. A value `> 0` enables a bounded evaluator-optimizer fix loop against the committed scope-only tree, capping total verify-fix agent invocations across the stage at that value; worst-case wall clock is bounded by `(max_iterations+1) × executor.timeout` plus `(max_iterations+1) × verify.timeout`. The runner captures combined stdout and stderr into a `verify_run` bundle event (`command`, `exit_code`, `output`, `outcome`) so an operator can see why the gate failed without re-running it. The gate fires only when the agent itself succeeded — re-running the tests against a broken working tree would be misleading.

A workflow declaring the `verification_reported` required outcome **must** configure `verify` on that stage, or the outcome can never be satisfied.

### The `human` branch

```yaml
executor:
  human: true
```

The stage blocks on a person. The branch declares `human` and nothing else — `unevaluatedProperties: false`, so `model`, `timeout`, `verify`, `agent_self_retry` and `agent_version` are all schema errors here, and a `defaults.executor` fragment of any shape is dropped rather than grafted on (see [the executor branch rule](#the-executor-branch-rule)).

### The `delegate` branch

Deploy stages only. `executor.delegate` names the external pipeline via a `target` discriminator:

| `target` | Required | Optional | Meaning |
|---|---|---|---|
| `github_actions` | `workflow_ref` | `git_ref` | Dispatch a GitHub Actions workflow via `workflow_dispatch`. `workflow_ref` is the workflow file or id (e.g. `deploy.yml`); `git_ref` is the branch, tag or sha to dispatch against — absent means the provider default. |
| `webhook` | `url` | — | POST the deploy trigger to a generic webhook endpoint. |

### Agent version compatibility

`agent_version` declares the semver comparator **range** of agent CLI versions a workflow was validated against, on two surfaces: the executor's agent branch, and each entry of `reviewers.agents[]`. Absent on both surfaces means **no constraint**.

**It is a range, not an exact pin.** CLIs release near-daily; the binary pin stays a host concern via the `FISHHAWK_AGENT_BIN` / `FISHHAWK_CODEX_BIN` overrides. A value is a space-separated AND list of comparators, each an operator (`>=`, `>`, `<=`, `<`, `=`, `==`) immediately followed by a 1-to-3-part dotted version (`2`, `2.1`, `2.1.5`; partial bounds zero-pad, so `>=2.1` means `>=2.1.0`). Examples: `">=2.1 <2.2"`, `"=3.0.1"`. A malformed range is rejected by the semantic validator at parse time, not silently accepted.

The comparison extracts the first semver token from the probed version string, so `"2.1.5 (Claude Code)"` compares as `2.1.5`. Behaviour per outcome:

| Resolved CLI version | Result |
|---|---|
| in range | dispatch proceeds |
| out of range | the stage fails loudly **pre-spawn** (category C) with an `agent_version_mismatch` log naming the range and the resolved version; the agent never runs |
| unprobeable (no extractable token) | an `agent_version_uncomparable` warning, then proceed |

The reviewer-side range is **codex-only**: the backend probes the codex reviewer CLI before dispatch and fails the review dispatch on an out-of-range version. The anthropic (SDK) and claudecode adapters take no CLI version and ignore the field.

## Reviewers

The `reviewers` block declares who must weigh in before a stage advances (ADR-027). It is declarable on any stage type but has runtime effect on `plan` and `implement` stages — the implement-review loop reuses the same config and the same authority semantics.

```yaml
reviewers:
  authority: advisory             # optional; advisory | gating; absent -> count-derived default
  agents:
    - provider: anthropic         # anthropic | claudecode | codex
      model: claude-opus-4-8      # optional; empty -> the provider's deployment default
      reasoning_effort: high      # optional; codex-only
      agent_version: ">=0.30 <0.31" # optional; codex-only
      optional: false             # optional; default false
    - provider: codex
      optional: true
  human: 1                        # integer >= 0; default 0
  review_timeout: 10m             # optional; this stage's review-budget floor
```

- **`agents`** — one entry per agent reviewer, minimum one entry when the key is present. The effective agent count is `len(agents)`; there is no bare integer count at major 2.
- **`provider`** — required per entry, and must be configured in the deployment (`FISHHAWKD_ANTHROPIC_API_KEY` / `FISHHAWKD_ENABLE_LOCAL_CLAUDE_REVIEWER` / `FISHHAWKD_ENABLE_CODEX_REVIEWER`). Those env flags are **capability gates** — "is this provider available here" — not policy switches that override the spec.
- **`model`** — the reviewer's model; empty means the provider's deployment-configured default. The self-review guard runs per invocation against each reviewer's returned model: if it matches the plan's own generating model the server logs a warning and does not block.
- **`reasoning_effort`** — `low | medium | high | xhigh | max`, **codex-only** (the anthropic and claudecode adapters take no reasoning-effort parameter and ignore it). Resolved through a two-rung ladder: the deployment default (`FISHHAWKD_CODEX_REASONING_EFFORT`) < this value. A non-empty spec value is passed to the codex adapter as a CLI override; the schema enum is the sole guard before it reaches the CLI.
- **`optional`** — the per-reviewer degradation policy when the provider is **unavailable on this deployment**. `false` (default) surfaces the gap loudly (an `ERROR` log naming the env knob plus a capability audit) but does not block; `true` is a quiet advisory skip. Either way the gap is recorded as a `reviewer_capability_unavailable` audit at run-create time and as a capability-framed `*_review_skipped` audit when the loop runs — deliberately distinct from a genuine reviewer error, because the reviewer never ran. A deployment with **no** reviewer backend wired at all is still a hard run-create failure: that is a deployment-wide misconfiguration, and `optional` does not apply to it.
- **`authority`** — `advisory | gating`; optional. Declares **explicitly** whether this stage's agent reviewers can block, instead of leaving it inferred from the counts. See **[Authority](#authority)** below.
- **`human`** — how many human approvals the stage's review requires.
- **`review_timeout`** — a Go duration string setting the **floor** rung of the size-aware review-wait budget (`Floor + PerKB × ceil(promptKB)`, clamped to `[Floor, Cap]`) for **this stage's** agent reviews, so plan and implement stages can carry different review timeouts. Two-rung ladder: the deployment default (`FISHHAWKD_PLAN_REVIEW_TIMEOUT`) < this value. Only the floor is per-stage; the `PerKB` and `Cap` rungs stay deployment-level.

### Authority

**When `authority` is absent, authority is count-derived** (the ADR-027 default), so heterogeneity changes *who* reviews, not the gating semantics:

| Reviewers | Authority | Effect |
|---|---|---|
| `len(agents) > 0` and `human == 0` | **gating** | Agent rejections block advancement to `awaiting_approval`. Every agent review must approve (or approve-with-concerns) first. |
| `len(agents) > 0` and `human > 0` | **advisory** | Agent verdicts are surfaced and recorded as `plan_reviewed` / `implement_reviewed` audit entries, but cannot block human approval. |
| `len(agents) == 0` | **gateless** | No agent review. Human approval, if any, proceeds unchanged. |

**An explicit `authority` WINS over the counts.** Declare it when the count-derived reading is not the one you want: `authority: gating` alongside `human: 1` gates (agent rejections block, even though a human approver is present), and `authority: advisory` alongside `human: 0` stays advisory (agent verdicts surface but never block). Absent, the field reproduces the table above exactly, so every spec that omits it keeps byte-identical gating behavior.

```yaml
# A human approves the plan, but an agent reject still blocks it — the
# count-derived rule would have read this as advisory.
reviewers:
  authority: gating
  agents:
    - provider: anthropic
  human: 1
```

`gateless` is **not declarable** — it is the zero-agent outcome, not a policy. A declaration (either value) on a stage with **no agent reviewers** is rejected at validation in both the backend and `fishhawk validate`, e.g. `stage "plan": reviewers.authority: "gating" declares agent-reviewer authority but the stage configures no agent reviewers; declare at least one entry under reviewers.agents, or remove reviewers.authority to fall back to the count-derived ADR-027 default`.

Declaring `authority: gating` engages the **run-creation reviewer-availability check**: a gating stage naming a provider the deployment has not configured fails **run creation** up front with `plan_reviewer_unconfigured`, rather than dispatching and degrading later. So adding `authority: gating` to an existing spec whose reviewers name an unconfigured provider will make new runs fail to start until the provider is configured (or the declaration is removed). In **advisory** mode an unresolvable provider instead degrades per reviewer: a `*_review_failed` audit carries the resolve error and the loop continues with the remaining reviewers.

The resolved mode and its provenance (`declared` | `derived`) are surfaced per stage on `GET /v0/runs/{id}` as `review_authority[]`, so an operator reads the mode instead of re-deriving it.

**Absent `reviewers` block:** **no reviewers are configured**: `Stage.Reviewers` stays `nil`, the effective agent count is `0`, and the stage resolves **gateless** — consistent with `gateless` being the zero-agent outcome rather than a declarable policy. There is **no `{human: 1}` default**; nothing materializes one, and a caller reading a nil block interprets that nil directly rather than substituting a default. A human approval requirement is declared by a stage `gates:` entry of type `approval` (read by `hasApprovalGate`), **not** by `reviewers.human` (E52.12 / #2322).

## Inputs and `needs:`

Two `inputs` shapes:

```yaml
inputs:
  # external trigger
  - source: github_issue   # github_issue | pull_request
    required: true         # optional boolean

  # an artifact from an earlier stage in the same run
  - artifact: plan         # plan | pull_request
    from_stage: propose
```

`source` names the external trigger the run is opened against. `required` marks the trigger as mandatory for the stage. `artifact` + `from_stage` wire an earlier stage's output into this one; the input-artifact enum has exactly two members, `plan` and `pull_request`.

### `needs:` shorthand

```yaml
needs: [propose]
```

Each entry names an **earlier** stage whose default artifact this stage consumes, and expands to the equivalent `inputs` entry. The artifact is derived from the **referenced** stage's type:

| Referent type | Derived artifact |
|---|---|
| `plan` | `plan` |
| `implement` | `pull_request` |
| `review`, `deploy`, `acceptance` | **rejected** — no default input artifact; declare the wiring longhand with `inputs:` |

That mapping is the whole of the input-artifact enum: a review stage produces no artifact, and the `deployment` / `acceptance` artifacts are not declarable as inputs.

**`needs:` and longhand `inputs:` may be combined on the same stage.** Ordering and dedupe:

- declared `inputs` keep their positions; derived entries follow, in `needs` order;
- a derived entry whose `(artifact, from_stage)` pair already appears verbatim among the declared inputs is **dropped**.

So the resolved input set is identical however the author spelled it.

Referent errors reuse the existing `from_stage` graph-shape rules with their unchanged messages: a referent that does not exist reports `from_stage "…" does not match any stage id`, and a self or later reference reports the must-be-earlier error. One tradeoff follows: the report names the post-expansion `inputs` index rather than the `needs` entry the author wrote. That is deliberate — one canonical error beats two competing ones.

> **`fishhawk validate` does NOT check `needs` referents.** `cli/internal/spec` is a deliberately separate, **schema-only** module: it performs no typed decode and no graph-shape pass, so it also accepts a longhand `inputs[].from_stage` naming a nonexistent stage. Both `needs`-referent errors — a nonexistent stage, and a `review` / `deploy` / `acceptance` referent with no default input artifact — surface **only server-side, at run creation**. The CLI does validate the `needs` **shape** (an array of stage-id-patterned strings). Closing the general asymmetry needs its own decision about duplication versus coupling and is tracked on **#2323**.

## Produces

```yaml
produces:
  - artifact: plan          # plan | pull_request | deployment | acceptance | grooming_report
    schema: standard_v1     # plan / grooming_report; the artifact schema version
    persistence:
      - target: originating_issue   # originating_issue | fishhawk_audit_log
        mode: rendered_comment      # rendered_comment | canonical
        update_on_change: true      # optional; republish when regenerated
```

| `artifact` | Valid on | Records |
|---|---|---|
| `plan` | `plan` | The proposal, as `standard_v1`. `schema: standard_v1` is required alongside it. |
| `pull_request` | any non-deploy stage | A code change. This is the **diff signal** the post-hoc constraints bind to. |
| `deployment` | `deploy` only | The delegated release outcome (`{environment, ref/sha, external_run_url, outcome, rollback_handle}`). |
| `acceptance` | `acceptance` only | The durable acceptance-evidence record (`{verdict, per-criterion results, content-hash references}`). |
| `grooming_report` | `plan` only | **v2-only** (ADR-065 §3 / #2235). The backlog-grooming proposal a PROPOSE stage emits *instead of* a plan: a rubric-cited ordering plus duplicate candidates, hygiene defects, suggested `depends_on` edges, vision-drift flags and decomposition suggestions, as `grooming_report_v1`. `schema: grooming_report_v1` is **required** alongside it, and a stage may not declare **both** `plan` and `grooming_report` — a propose stage proposes one thing. See [`grooming-report-v1.md`](grooming-report-v1.md). |

The `grooming_report` binding keys on the **stage type**, not on a workflow name: `plan` reads as PROPOSE (ADR-067 §2), so any propose stage in any workflow may emit one. Declaring it does **not** make a stage produce a diff — `pull_request` remains the only diff signal, so a grooming workflow still cannot carry a post-hoc diff constraint. Rejections:

```
grooming_report artifact is valid only on a plan stage — the PROPOSE stage per ADR-067 §2 —
not a "implement" stage (ADR-065 §3)

grooming_report-producing stage must declare schema: grooming_report_v1, got ""
```

`persistence` says where a copy of the artifact lands. `target` is `fishhawk_audit_log` (the authoritative copy, `mode: canonical`) or `originating_issue` (the human-readable echo on the tracker, `mode: rendered_comment`).

`target: originating_issue` + `mode: rendered_comment` on a plan stage is the **canonical plan-review surface** (ADR-020): the backend posts the full plan as a markdown document on the triggering issue, and reviewers read and approve from the issue thread. `update_on_change: true` edits the existing comment in place on a re-upload; if the comment was deleted the backend falls back to a fresh one. Omitting the flag makes the post one-shot — the comment lands on the first upload and re-uploads are skipped. When a plan stage declares no `originating_issue` target at all, the backend posts a short summary comment linking to the plan document instead.

## Constraints

At major 2 `constraints` is **one object keyed by kind**, not a list:

```yaml
constraints:
  max_files_changed: 45
  forbidden_paths: ["infra/**", ".github/workflows/**"]
```

An object's keys are unique, so a v2 `constraints` block denotes exactly one constraint set and cannot express the duplicate-kind document that made the old list form ambiguous. `constraints: {}` is rejected as a no-op (`minProperties: 1`). A binding violation is reported at `/constraints/<kind>` — the kind the author actually wrote.

### Post-hoc diff constraints

Evaluated **after** the stage runs, against the produced diff. A hit is a category-B failure.

| Kind | Shape | Meaning |
|---|---|---|
| `max_files_changed` | integer | Ceiling on files touched by the stage's diff. |
| `forbidden_paths` | array of globs | Paths the diff must not touch. |
| `allowed_paths` | array of globs | Paths the diff may touch; mutually informative with `forbidden_paths`. |
| `required_outcomes` | array of enum members | Outcomes that must hold — see below. |
| `diff_coverage` | object | New-line coverage floor for the diff — see below. |

#### `required_outcomes`

| Member | Satisfied when |
|---|---|
| `tests_added_or_updated` | the diff contains a test-*named* file. Filename-shape-aware: it does not inspect the file's contents. Two vacuous-satisfaction carve-outs: a non-empty diff touching no unit-testable source at all (docs/scripts/config only, #610), and a **comment-only Go correction** (#2660) — see below. |
| `ci_green` | required checks pass. The one outcome whose missing signal **defers** to branch protection. |
| `verification_reported` | the stage's committed-tree verify gate reported `passed`. |

**Comment-only Go corrections (#2660).** `tests_added_or_updated` is also vacuously satisfied when the stage's emitted unified diff proves a narrow syntactic property: every changed testable-source file is a `.go` file with status `A`/`M`, every one has a patch section, every changed line in those sections is blank or an ordinary `//` line comment (never a Go directive such as `//go:build`), and no backtick appears anywhere in the section. This makes a doc-comment fix landable through `feature_change`, which previously required a test file for a change with no behavior to test. It is **fail-closed** on every other input (absent or truncated patch, binary or C-quoted section, non-`.go` source, a delete/rename, a directive line, any other changed line) and it is NOT a claim of behavioral emptiness: a changed `//`-shaped line inside a raw-string literal whose delimiters lie outside the emitted context is admitted, per occurrence. The plan-approval gate refuses the statically-knowable unlandable case up front (`plan_missing_required_tests`), with `--comment-only` as the escape for exactly this case. Contract: `backend/internal/policy/README.md`.

`verification_reported` is the substance-aware sibling of `tests_added_or_updated`: it gates on what the stage actually **ran**, not on a filename.

| Signal at evaluation time | Result |
|---|---|
| committed-tree verify reported `passed` | satisfied |
| committed-tree verify reported `failed` | violation, naming the outcome and the failing command |
| committed-tree verify reported `skipped` | violation — a skipped gate is not a passed gate |
| no verification evidence in the trace | violation (`no verification evidence in trace`) |

It is **fail-closed in every absent-or-negative mode** and, unlike `tests_added_or_updated`, has no filename inspection and no docs-only vacuous-satisfaction branch: a diff whose only change is `foo_test.go` does not satisfy it, and neither does a docs-only diff. It is also **not deferrable** — deferring it would reconstruct the vacuous pass it exists to remove. A workflow declaring it **must** configure `executor.verify` on the stage. Runner-side it is backend-authoritative: the runner's in-line check fires before either committed-tree verify gate runs, so it skips this outcome rather than asserting on it. It does not replace `tests_added_or_updated`; the two are independent and may be declared together.

#### `diff_coverage`

```yaml
constraints:
  diff_coverage:
    command: "coverage run -m pytest && coverage lcov -o coverage.lcov"
    report_path: coverage.lcov
    format: lcov              # optional; lcov is the only member
    min_new_line_coverage: 85
    base_ref: main            # optional; omitted = the run's base branch
  required_outcomes: [verification_reported]
produces:
  - artifact: pull_request
```

The customer declares the command; the runner **measures**; the backend is **authoritative** for the verdict.

| Field | Required | Meaning |
|---|---|---|
| `command` | yes | Shell command (`sh -c`) producing the coverage report, run from the repo root of a throwaway `git worktree` checkout of the stage's committed tree. Same containment as the verify gate: bounded timeout, process-group kill, default-deny gate-env allow-list (it never sees runner credentials), disposable checkout. |
| `report_path` | yes | Repo-relative path the command writes the report to. An absolute path or a `..` escape is rejected at parse time. |
| `format` | no (`lcov`) | Report format. `lcov` is the only member — an enum so a later change can add others. |
| `min_new_line_coverage` | yes | Integer 0–100, compared with `>=`, so a measurement exactly at the threshold passes. |
| `base_ref` | no | Git ref the diff is taken against (merge-base semantics). Omitted means the run's base branch, resolved by the runner at measurement time. An empty ref never reaches git. |

LCOV is the format because it is the one per-line format every major ecosystem can emit; the parsed grammar is the stable `SF:` / `DA:<line>,<hits>` / `end_of_record` subset and other record types are ignored, not rejected.

| Signal at evaluation time | Result |
|---|---|
| measured, at or above the threshold | satisfied |
| measured, **zero** new coverable lines | satisfied — a diff that added nothing cannot be under-covered |
| measured, below the threshold | violation naming covered/total, the percentage, the threshold, and the uncovered files |
| the command exited non-zero | violation naming the command, its exit code, and its output tail |
| the command succeeded but wrote no readable report | violation naming `report_path` |
| the report was not parseable | violation naming the path and the parse error |
| `base_ref` could not be resolved | violation naming the unresolved ref |
| **no diff-coverage evidence in the trace at all** | violation (`no diff-coverage evidence in trace`) |

**Absence of a measurement is a violation, not a pass.** The runner emits evidence whenever the constraint is configured, including the zero-new-lines case — an explicit zero is auditable, whereas silence is indistinguishable from a runner that failed to run. The added-line set is taken merge-base → the committed tree the command ran against, with the merge base pinned to a SHA before that tree is materialized so a `base_ref` naming the branch being committed onto cannot collapse the diff to empty. Two denominator exclusions are deliberate: an added line in a file the report never measured, and an added line the report measured no statement on. `diff_coverage` is valid on an **`implement` stage only** — the implement runner is the layer that measures, and an absent signal on a declared constraint is a violation, so declaring it elsewhere would be a guaranteed false failure. A coverage percentage credits execution, not assertion: a vacuous test still earns diff coverage.

### Post-hoc constraints bind to the produced artifact

Because the type names are general, **constraint validity keys on what a stage PRODUCES, not on its type name.** A stage carrying any post-hoc diff constraint must declare the `pull_request` artifact:

```yaml
# rejected — nothing to evaluate the constraint against
- id: apply
  type: implement
  executor: { agent: claude-code }
  constraints:
    max_files_changed: 5

# accepted — the stage says it produces a diff
- id: apply
  type: implement
  executor: { agent: claude-code }
  constraints:
    max_files_changed: 5
  produces:
    - artifact: pull_request
```

The message names the kind and both fixes:

```
post-hoc diff constraint "max_files_changed" is valid only on a stage that produces a diff;
stage "apply" declares no pull_request artifact (ADR-067).
Declare produces: [{artifact: pull_request}] on this stage, or remove the constraint.
```

`pull_request` is the diff signal because it is the only artifact in the closed set that **denotes a code change**: `deployment` is delegated to an external pipeline, `acceptance` is a verdict, and `plan` and `grooming_report` are proposals.

> **An absent or empty `produces` list reads as "produces no diff."** Omitting `produces` does not exempt a stage — that is what gives the rule teeth. The permissive alternative ("absent means unknown, so allow it") would let any stage keep a diff constraint simply by staying silent, which is exactly the case this rule exists to reject. The fix is one line: declare the artifact, or drop the constraint.

Two orderings are contracts rather than accidents:

- A **deploy** stage gets its own ADR-038 message (`post-hoc diff constraint is not valid on a deploy stage`), never this one.
- A stage that does produce `pull_request` but is typed other than `implement` still gets the `diff_coverage is valid only on an implement stage` message.

> **`fishhawk validate` does NOT report this binding.** `cli/internal/spec` is schema-only, so like the `needs`-referent errors this surfaces **only server-side, at run creation**. Same asymmetry, tracked on **#2323**.

### Pre-flight deploy constraints

Evaluated **before** a deploy stage executes, and valid on a deploy stage only:

| Kind | Shape | Meaning |
|---|---|---|
| `allowed_environments` | array of strings, min 1 | The stage may target only these environments. |
| `change_freeze` | boolean | When `true`, the stage is blocked while a change freeze is active. The freeze-signal source is out of scope for the spec. |
| `required_upstream` | array, unique, items `review_merged` \| `ci_green`, min 1 | Upstream conditions that must hold before the stage may run. |

A v2 `constraints` object may mix a pre-flight and a post-hoc kind, because it is one value. On a non-deploy stage the **pre-flight** diagnosis wins and is reported at `/constraints/<pre-flight kind>`: a pre-flight deploy constraint on a non-deploy stage is wrong regardless of what the stage produces, and fixing it is a prerequisite for the produced-artifact question even arising.

## Budget (per-stage)

```yaml
budget:
  limit_usd: 8.5        # primary cost ceiling, same unit as budgets[].limit_usd
  max_runtime: 15m      # Go duration string
  max_tokens: 200000    # optional secondary lever
  enforcement: advisory # advisory | blocking
```

A cap on one stage execution, distinct from the workflow-level [`budgets`](#periodic-budgets-budgets) which govern aggregate USD spend across runs.

- **`limit_usd`** is the **primary** cost lever and is **not required**. A budget declaring only `max_tokens`, only `limit_usd`, only `max_runtime`, any combination, or nothing at all is valid: ADR-067 ratified unifying the units, not mandating a cost model.
- **`max_runtime`** is the same duration form as every other v2 duration — the identical pattern (`^([0-9]+(ns|us|ms|s|m|h))+$`) as `policy.max_stage_runtime`, `executor.timeout` and `executor.verify.timeout`, parsed by the same code path. The pattern is a strict subset of what Go's duration parser accepts (which also takes fractional and signed forms), matching the convention of those three fields rather than the parser's full grammar. Sub-minute caps such as `90s` are expressible.
- **`max_tokens`** is an optional secondary lever.
- **`enforcement`** — `advisory` reports overruns; `blocking` is declarable but not yet enforced. **No runtime check reads a stage budget on any major**: this block is grammar today. Stage-budget enforcement — spending against the `limit_usd` ceiling and the cost accounting behind it — is tracked on **#2328**.

## Gates

A stage may declare gates. Two types:

```yaml
gates:
  # Approval gate — blocks until the predicate is satisfied.
  - type: approval
    approvals:
      count: 1
      not: [author, agent]
      min_permission: write
      member_of: my-org/reviewers
      members: [alice, bob]
    sla: 4_business_hours   # optional; D-category timeout
    autonomy: low           # optional; per-gate autonomy override
    actions: {...}          # optional; per-gate action matrix

  # Check gate — delegates to forge branch protection.
  - type: check
```

`sla` is the gate's D-category timeout. `autonomy` and `actions` on a gate override the workflow-level block **wholesale** — see [Placement, and the wholesale gate override](#placement-and-the-wholesale-gate-override).

A **check** gate carries no other spec-level field. When a review stage carries one, Fishhawk queues an auto-merge (squash) against the implement stage's pull request and transitions the review stage to `succeeded` immediately; the forge's own auto-merge machinery performs the merge once the required checks pass. Fishhawk's role is to queue and step out of the way. Required CI checks are not declared in the spec — they are derived from branch protection at run-create time and snapshotted onto the run.

### `approvals` — the sole approval predicate

At major 2 the forge-neutral `approvals` block is the **only** approval predicate. The gate approval branch lists `approvals` in its `required` set, so an approval gate **always** declares one and can never be a no-op. A gate declaring `type: approval` with no `approvals` block, or `approvals: {}`, is rejected.

| Field | Required | Shape | Meaning |
|---|---|---|---|
| `count` | **yes** | integer `>= 1` | Distinct approvals to collect before the gate clears. Always explicit (ADR-055), so an empty predicate fails validation. |
| `not` | no | array, unique, items `author` \| `agent` | Relationship classes barred from satisfying the gate — the change's own `author`, and any automated `agent` identity. Forge-neutral relationship classes, not handles. The two legs enforce **asymmetrically** — see below. |
| `min_permission` | no (`x-intended-required`) | enum `read` \| `triage` \| `write` \| `maintain` \| `admin` | Minimum forge-neutral repository permission tier an approver must hold. |
| `member_of` | no (`x-intended-required`) | string, min 1 | A forge-neutral group (an org, or `org/team`) an approver must belong to. |
| `members` | no | array of strings, min 1 | Explicit approver subjects as **plain** forge-neutral identifiers — not `@`-prefixed handles. |

`min_permission` and `member_of` are annotated [`x-intended-required`](../../AGENTS.md#schema-change-checklist): optional now, intended to become required in a future major. They are deliberately **not** promoted here; that promotion is a separate decision.

The three canonical presets (`docs/spec/workflow-preset-{low,medium,high}.yaml`) ship `approvals: {count: 1, not: [author, agent]}` — ADR-055's ratified preset default — so a freshly-scaffolded repo has no placeholder handle to replace.

#### What `not` means at runtime

The two members enforce asymmetrically, and the asymmetry is deliberate (#2358).

**`author` — conditional, and narrower than it reads.** The author is the identity that produced change **content**. Fishhawk resolves it from a closed allow-list of audit categories, today exactly one: `operator_commit_vouched`, the operator's audited declaration that a hand-pushed commit belongs to this run's lineage. Operator **governance** acts are explicitly *not* authorship and resolve no author — answering a clarification, driving a stage (`run_auto_driven`), approving with binding conditions, deciding a scope amendment, waiving or deferring a concern. A gate declaring `not: [author]` therefore refuses a self-approval only where such an authorship signal exists on the run; where none does, the leg is skipped and the remaining controls stand. The narrowing is the point: under the earlier rule — the earliest user-kind actor of *any* category — merely steering a run made the operator its "author" and locked them out of their own gate. An allow-list, rather than a deny-list of governance categories, is used so a future gate verb cannot silently re-open that wedge; its failure mode is "no author resolved", not a false refusal.

At a **plan** gate no authorship signal exists yet — the artifact under review is agent-authored and the operator is steering — so `not: [author]` does not bite there at all. Do not read a plan gate as author-protected. Its real controls are the agent floor below and the distinct-human `count`.

**`agent` — an unconditional floor.** An automated identity never satisfies a human quorum on *any* gate carrying an `approvals` block, whether or not `not` names `agent`. Its approval is recorded (audit `channel: delegated`) but never counted and never advances the gate. Declaring `agent` documents intent; omitting it does **not** permit an agent approver.

**Deployment caveat.** Until the backend change that reads `not` is deployed, the field is parsed, schema-validated and documented but read by no enforcement code — on such a deployment a declared `not:` is grammar rather than enforcement, and the author leg fires on *any* gate carrying an `approvals` block.

#### Forge resolution

When a gate sets `min_permission` and/or `member_of`, the backend resolves the predicate against the forge at **each** approval event, before recording the vote; no forge result is cached between requests. A human submitter whose resolved permission is below `min_permission`, or who is not a member of `member_of`, is refused `403 approver_predicate_unmet` and no approval row is inserted. A forge failure (error, rate-limit, timeout, or an ad-hoc run with no repository to resolve against) **fails the gate closed** with a retryable `503 forge_unavailable`. Agent and delegated submissions are recorded-but-never-counted and are not forge-gated.

At each approval event the backend records a `predicate_snapshot` — the submitter's auth method, channel, resolved permission and membership, and the resulting quorum state — into the `approval_submitted` audit entry, including on both rejection paths. See [`docs/ARCHITECTURE.md` §8.1](../ARCHITECTURE.md#81-approval-identity).

The permission vocabulary is forge-neutral and ordered `none` < `read` < `triage` < `write` < `maintain` < `admin`. GitHub maps its role names directly onto it; another forge maps its own tiers onto the same vocabulary — GitLab **Maintainer** → `maintain`, **Owner** → `admin` — with no schema change.

#### Where approval gates enforce

- **Plan stages** — enforced by Fishhawk. The gate accepts a decision from any convergent surface: a reply comment on the issue thread (`+1` / `👍` / `lgtm`), a `/fishhawk approve [reason]` slash command, or `POST /v0/stages/{id}/approvals` from the SPA or the CLI. Every surface converges on one approval row plus an `approval_submitted` audit entry, and the surface of origin is recorded so a later reader can attribute the decision.
- **Review stages** — branch protection's required-reviewers is the forge-side gate on the pull request; Fishhawk records reviewer activity from review events and transitions the review stage on the PR merging.
- **Deploy stages** — a pre-execution gate: it blocks before the external pipeline is triggered.

## Autonomy: tier shorthand and action matrix

Two surfaces declare how much the operator agent may do without paging a human (ADR-066):

```yaml
workflows:
  feature_change:
    autonomy: medium          # tier shorthand — expands to a full matrix
    actions:                  # per-class overrides, each winning for that class ONLY
      approve:
        mode: gated           # tighten one class the tier delegates
      fixup:
        mode: auto
        when: convergent_concerns
        min_severity: high
      page_human_on:          # RESERVED key
        - gating_reviewer_reject
```

### The three modes

`mode` says **who acts**:

| `mode` | Meaning |
|---|---|
| `gated` | The human acts. This is the **fail-closed default**: it is what an absent matrix, an absent class entry and an unrecognized class all resolve to. |
| `report` | The operator agent surfaces a **proposal** and does not act. It records a `run_auto_driven` attribution row with `act: report`; the run does **not** park on it and no gate action is dispatched. |
| `auto` | The operator agent may act without paging — the only mode that widens authority, and the only one that requires a condition. |

**When a `report` entry fires:** when the gate is **live**, and — only when the entry declares a `when` — additionally when that condition is met. A bare `report` entry with no `when` therefore surfaces whenever the gate is reachable; requiring a condition that need not exist would make the mode inert. A `when` declared on a `report` entry is subject to the **same per-class binding** as `mode: auto`: it must name that class's own condition. The row is emitted **at most once per gate occurrence per class**. A gate occurrence spans one opening of the gate: a gate a human takes an hour to reach produces one row, not one per poll cycle, while a gate that **closes and re-opens on a fresh review round** (a fix-up round trip) is a new occurrence and re-surfaces the proposal.

### `mode: auto` requires a condition

Each known action class has exactly **one** backend-evaluable condition — the backend has to be able to answer the predicate from run state:

| Class | `when` | Met when |
|---|---|---|
| `approve` | `clean_dual_approval` | every configured reviewer for the gated stage returned an approve verdict and zero concerns are open |
| `fixup` | `convergent_concerns` (+ `min_severity`) | all reviewer verdicts are in, at least one concern is open, and no gating-authority reviewer rejected |
| `waive` | `solo_low` | exactly one open concern, and its severity is low |
| `retry` | `infra_flake` | the latest stage failure is classified as an infrastructure flake |
| `merge` | `gates_resolved_ci_green` | no pending gate approvals, zero open concerns, PR open, required checks green |

`min_severity` is accepted on the `fixup` class only. It tunes when `convergent_concerns` auto-routes on an all-approve round: when every verdict is approve-class — no reject to arbitrate — the fix-up auto-routes only if at least one open concern ranks at or above this threshold (`low` < `medium` < `high`, default `medium`). A dual-approve round whose sole open concern is low-severity therefore parks for the operator rather than spending a full fix-up pass. A **reject** verdict bypasses the threshold entirely. An unrecognized severity ranks below `low` and parks (fail-closed).

Four documents are rejected with a message naming the class:

- `mode: auto` with **no** `when`,
- `mode: auto` whose `when` is another class's condition,
- `mode: auto` on an **extension** class (below), which has no backend-evaluable condition at all,
- `mode: report` that declares a `when` which is **not that class's own condition** (a foreign condition, or any `when` on an extension class). A bare `report` with no `when` is always accepted; a declared `when` must name the class's own condition, so a proposal the report arm cannot evaluate is refused at validation rather than silently dropped at fire time.

These rules are enforced by the **backend**, not by JSON Schema: the class-name set is open, so no schema keyword can bind a condition to a class the schema does not enumerate. A raw `check-jsonschema` run therefore accepts a document the backend rejects.

### Extension classes

The class-name set is deliberately **open** and per-workflow-type extensible (ADR-065): a workflow type may declare its own class, e.g. `promote` or `rollback`. An unknown class is safe **by construction** — accepted at `mode: gated` and `mode: report`, where it delegates nothing, and rejected at `mode: auto`, where it would need a condition that does not exist.

### The tiers

`autonomy` expands to a matrix. The three tiers are `docs/METHODOLOGY.md`'s autonomy tiers and reproduce the shipped `workflow-preset-<tier>.yaml` delegation blocks:

| Class | `low` | `medium` | `high` |
|---|---|---|---|
| `approve` | `gated` | `auto` / `clean_dual_approval` | `auto` / `clean_dual_approval` |
| `fixup` | `gated` | `auto` / `convergent_concerns` | `auto` / `convergent_concerns` |
| `retry` | `gated` | `auto` / `infra_flake` | `auto` / `infra_flake` |
| `waive` | `gated` | `gated` | `auto` / `solo_low` |
| `merge` | `gated` | `gated` | `auto` / `gates_resolved_ci_green` |
| `page_human_on` | *(none)* | the seven events below | the seven events below |

The medium/high page list is `gating_reviewer_reject`, `plan_rejection`, `scope_amendment`, `budget_override`, `policy_override`, `exception_request`, `requirement_arbitration`. A tier emits `gating_reviewer_reject`, never a bare `reviewer_reject` — a tier must not expand to a value undeclarable under the grammar it belongs to.

An explicit `actions` entry **overrides the tier for that class only**; unlisted classes keep the tier's value, and a class no tier names resolves to `gated`.

### `page_human_on` — the events that always page

`page_human_on` is a reserved key of `actions`, not an action class. It lists the events that page the human regardless of any class's `mode`. Closed set: `gating_reviewer_reject` (an agent reject on an agent-only implement review, where no human approver overrides it), `advisory_reviewer_reject` (an agent reject under agent + human authority — non-blocking and arbitrable, so it does not page on its own), `plan_rejection`, `scope_amendment`, `budget_override`, `policy_override`, `exception_request`, `requirement_arbitration`, and `clarification_request` (the planner parking the plan stage at `awaiting_input` because the issue is not yet plannable).

A human reviewer's reject never arrives as an agent review verdict at all — it surfaces as a gate rejection, which already pages.

### `model_policy` — operator-agent model selection

`model_policy` is the other **reserved key** of `actions`, so it keeps the wholesale gate-override semantics it needs. Neither reserved key is an action class, and a property named like one of them is never treated as one.

```yaml
actions:
  model_policy:
    strategy: explicit_defaults     # follow_plan_recommendation | explicit_defaults
    defaults:                       # applied under explicit_defaults; each stage optional
      plan: claude-opus-4-8
      implement: claude-sonnet-4-6
      review: gpt-5.5
    allowed:                        # composes with — never widens — the deployment allow-list
      - claude-opus-4-8
      - claude-sonnet-4-6
      - gpt-5.5
```

- **`strategy`** — `follow_plan_recommendation` (the operator agent follows the plan artifact's per-stage model recommendation) or `explicit_defaults` (it applies the `defaults` map, falling back to the deployment default for any unset stage).
- **`defaults`** — an optional model per stage kind: `plan`, `implement`, `review`, each optional. (This key is `model_policy`'s own; it is unrelated to the file- and workflow-level reuse `defaults` block.)
- **`allowed`** — the models the operator agent may select. It **composes with, never widens,** the deployment per-adapter allow-list.

`model_policy` is **declarative**: the operator agent reads the resolved policy from the run-status delegation block and applies it through the existing per-stage model override channels, still bounded by the deployment allow-list. An absent `model_policy` is omitted from the wire.

### Placement, and the wholesale gate override

Both surfaces are declarable at **workflow** level (the default for every gate) and on an **approval gate**. A gate declaring **either** `autonomy` or `actions` supplies the WHOLE block — nothing is inherited from the workflow level. So a gate declaring only `actions: {merge: {mode: auto, when: gates_resolved_ci_green}}` on an `autonomy: high` workflow has **no tier**: `merge` is auto and every other class falls to `gated`, not to high's value. `page_human_on` and `model_policy` are part of the block and are likewise never merged across levels.

**Fail-closed default.** A level declaring neither surface delegates nothing: every judgment pages the human. Nothing expressible in this grammar widens agent authority beyond what the run-state conditions above can answer.

### Provenance

Resolution records, per class, **which input decided it**: `explicit` (an `actions` entry), `tier` (the shorthand expansion), or `default` (the fail-closed fallback). The resolved tier and matrix are surfaced on the run-status `delegation` block alongside the evaluated-action list, so an operator can read `approve: gated (tier)` rather than infer it from a missing knob.

## Test conventions

An optional **top-level** `test_conventions` array making test-location rules per-repo data for the plan-gate test sweep. Each entry maps production files matching a glob to candidate test-file path templates; the sweep flags a candidate test that **exists on the base ref but is missing from the plan's scope file list**. Advisory-only and fail-open — it never blocks a plan.

```yaml
test_conventions:
  - match: "src/**/*.py"
    candidates:
      - "tests/test_{name}.py"
  - match: "lib/**/*.rb"
    candidates:
      - "spec/{relpath}_spec.rb"
```

- **`match`** — a doublestar glob (`**` crosses `/`) matched against the repo-relative production-file path. A scoped file whose basename matches a candidate's shape is treated as a test, not a production file.
- **`candidates`** — one or more test-file path templates (minimum one) for a matched production file. Template variables: `{dir}` (the production file's directory), `{name}` (basename without its final extension), `{ext}` (final extension without the dot), `{relpath}` (the repo-relative path without its final extension).
- **Built-in defaults are always on.** The sweep ships defaults reproducing the Go rule (`**/*.go` → `{dir}/{name}_test.go`) and colocated TypeScript. Declared conventions **append** to these — they never replace them — so a repo declaring only Python and Ruby keeps Go and TypeScript covered.

## Identifier namespaces

| Field | Pattern / values | Notes |
|---|---|---|
| `version` | `"2"` | Single token; no minor chain. A dotted `"2.0"` routes here by major and is then rejected. |
| Workflow / stage IDs | `^[a-z][a-z0-9_]*$` | snake_case; stage ids unique within a workflow |
| `extends` | a workflow key in the same document | same-document only; cycles and unknown bases rejected |
| `autonomy` | `low` \| `medium` \| `high` | tier shorthand; expands to the action matrix |
| `actions.<class>.mode` | `report` \| `gated` \| `auto` | `gated` is the fail-closed default |
| `actions.<class>.when` | one closed condition per class | required for `auto`; on `report` it must be that class's own condition |
| `actions.<class>.min_severity` | `low` \| `medium` \| `high` | `fixup` class only |
| `actions.page_human_on` | closed event set (see above) | reserved key, not an action class |
| `actions.model_policy.strategy` | `follow_plan_recommendation` \| `explicit_defaults` | reserved key, not an action class |
| Stage `type` | `plan` \| `implement` \| `review` \| `deploy` \| `acceptance` | closed set |
| Executor branch | `agent: <string>` xor `human: true` xor `delegate: {...}` | mutually exclusive; `delegate` is deploy-only |
| `executor.model` | non-empty string | agent branch only |
| `executor.agent_version` | space-separated comparator range | agent branch only; malformed ranges rejected by the validator |
| `executor.agent_self_retry` | `true` \| `false` (default `false`) | agent branch only |
| `executor.timeout`, `executor.verify.timeout`, `policy.max_stage_runtime`, `budget.max_runtime` | `^([0-9]+(ns\|us\|ms\|s\|m\|h))+$` | one duration grammar throughout v2 |
| `executor.verify.max_iterations` | integer `>= 0` (default `0`) | `0` = single-shot gate |
| `executor.delegate.target` | `github_actions` \| `webhook` | `workflow_ref` (+ optional `git_ref`), or `url` |
| `reviewers.agents[].provider` | `anthropic` \| `claudecode` \| `codex` | must be a configured capability on the deployment |
| `reviewers.agents[].reasoning_effort` | `low` \| `medium` \| `high` \| `xhigh` \| `max` | codex-only |
| `reviewers.agents[].optional` | `true` \| `false` (default `false`) | per-reviewer degradation policy |
| `reviewers.human` | integer `>= 0` (default `0`) | absent block → no reviewers configured; `Reviewers` nil; agent count `0`; resolves `gateless` (no `{human: 1}` default) |
| `reviewers.review_timeout` | duration string | this stage's review-budget floor |
| Input `source` | `github_issue` \| `pull_request` | external trigger |
| Input `artifact` | `plan` \| `pull_request` | what a later stage may consume |
| Produced `artifact` | `plan` \| `pull_request` \| `deployment` \| `acceptance` \| `grooming_report` | `deployment` deploy-only, `acceptance` acceptance-only, `grooming_report` plan-only (v2-only) |
| `produces[].schema` | `standard_v1` \| `grooming_report_v1` | required alongside the `plan` and `grooming_report` artifacts respectively |
| `persistence.target` | `originating_issue` \| `fishhawk_audit_log` | closed set |
| `persistence.mode` | `rendered_comment` \| `canonical` | closed set |
| `persistence.update_on_change` | `true` \| `false` | republish in place when the artifact is regenerated |
| Constraint kind | `max_files_changed`, `forbidden_paths`, `allowed_paths`, `required_outcomes`, `diff_coverage`, `allowed_environments`, `change_freeze`, `required_upstream` | object keys; at least one |
| `required_outcomes` items | `tests_added_or_updated`, `ci_green`, `verification_reported` | closed set |
| `required_upstream` items | `review_merged`, `ci_green` | closed set; deploy-only |
| `diff_coverage.format` | `lcov` | only member |
| `diff_coverage.min_new_line_coverage` | integer 0–100 | compared with `>=` |
| `budget.enforcement`, `budgets[].enforcement` | `advisory` \| `blocking` | advisory warns; blocking refuses a new run at admission |
| `budgets[].period` | `weekly` \| `monthly` | calendar reset cadence |
| `budgets[].warn_at` | fraction `[0,1]` | early advisory threshold |
| `decomposition.max_parallel` | integer `>= 0` | `0` = unlimited |
| `on_ci_failure.max_retries` | integer `0..5` (default `1`) | `0` disables auto-retry |
| Gate `type` | `approval` \| `check` | closed set |
| `approvals.count` | integer `>= 1` | required |
| `approvals.not` items | `author` \| `agent` | forge-neutral relationship classes |
| `approvals.min_permission` | `read` \| `triage` \| `write` \| `maintain` \| `admin` | `x-intended-required` |
| `approvals.member_of` | non-empty string | one forge-neutral group; `x-intended-required` |
| `approvals.members` | array of plain subject strings | not `@`-prefixed handles |
| `gates[].sla` | string | D-category gate timeout |
| `test_conventions[].match` | non-empty doublestar glob | `**` crosses `/` |
| `test_conventions[].candidates` | array of non-empty templates, min 1 | `{dir}` / `{name}` / `{ext}` / `{relpath}` |

## Validation rules beyond the schema

The schema enforces structure. Layers above it enforce what JSON Schema cannot express cleanly:

- Every `inputs[].from_stage` references an existing, **earlier** stage `id` in the same workflow — evaluated against the resolved document, so a base-only stage is a legal referent.
- Within a workflow, stage `id`s are unique. A duplicate is rejected in the reuse resolver, before the keyed `stages` merge could swallow it.
- A stage producing `plan` declares `schema: standard_v1`.
- A `deploy` stage uses `executor.delegate`; no other stage type may.
- The `deployment` artifact and the three pre-flight constraint kinds are deploy-only; the `acceptance` artifact and the `egress` block are acceptance-only.
- A post-hoc diff constraint requires the `pull_request` artifact; `diff_coverage` additionally requires stage type `implement`.
- `mode: auto` requires that class's own `when`; `min_severity` is `fixup`-only; an extension class may not be `auto`.
- An `agent_version` range parses as a comparator list.
- `extends` names a defined workflow and forms no cycle.
- Every `escalations` entry actually **raises** something (see [Escalations](#escalations)): `count` and `min_permission` must exceed the workflow's least-restrictive baseline, `member_of` may not name a group every approval gate already requires, `require.approvals` needs an approval gate to raise, and `max_autonomy` may not leave the resolved matrix identical. A `match.paths` criterion is additionally refused on a workflow that declares no plan stage (E53.16 / #2382), for the same reason `applies_to.paths` is — there is no `scope.files` producer for it to match against, so the escalation could never fire. `fishhawk validate` mirrors all of these except the `max_autonomy` no-op check, which needs the autonomy resolver the CLI deliberately does not carry.

`fishhawk validate` (the CLI) is **schema-only** — no typed decode, no graph-shape pass. It reports schema errors, the removed-form messages, and the reuse-resolution rejections, but not the graph-shape rules above, which surface server-side at run creation (**#2323**).

## Version routing

The backend (`backend/internal/spec`) and the CLI (`cli/internal/spec`) compile the workflow-v0, workflow-v1 and workflow-v2 schemas at init and dispatch a spec to one of them by its `version` **major** component:

- `version: "0.x"` → `workflow-v0.schema.json`
- `version: "1.x"` → `workflow-v1.schema.json`
- `version: "2"` → `workflow-v2.schema.json` (a dotted `"2.0"` routes here by major but the collapsed enum rejects it)
- a missing / non-string / unparseable `version` falls through to the v0 schema, which then emits the existing required-version error, so a malformed version never silently passes
- a well-formed but unrecognized major (`>= 3`) **fails closed** with an error naming the supported majors (`0, 1, 2`)

`/healthz` advertises the `workflow-v0`, `workflow-v1` and `workflow-v2` embedded-schema hashes so a component can detect drift in any of them.

## v0 and v1: deprecation posture

**v0 and v1 are frozen, not removed, and no removal is planned.**

- Both schemas stay embedded, compiled, routed and validated **forever**. `TestFrozenMajorsV0AndV1AreImmutable` pins each one by content digest, so an edit to a shipped major fails a test rather than only a convention.
- Every audit-log run and every already-committed spec keeps working with **no action**. Old runs stay readable — that is the whole point of carrying every major forever.
- A **new** spec on v0 or v1 is still **accepted**. Nothing rejects a `0.x` or `1.x` document.
- A new spec on v0 or v1 is nonetheless **discouraged**. `fishhawk init` emits v2 presets; the reuse, legibility and autonomy work exists only at v2; and `fishhawk migrate-spec` translates an existing v1 document, printing an approval-eligibility report rather than rewriting blind. See [`workflow-migration.md`](workflow-migration.md).
- No removal date is set, and none is planned.

## Path predicate

`$defs/predicate` is one declarative match rule over a change — the SINGLE matcher that a workflow's [`applies_to`](#workflow-routing-applies_to) routing (E53.3 / #2226), `escalations` (E53.4 / #2227) and the review-conventions of ADR-068 (#2211) each `$ref` rather than each growing a subtly different matcher. `applies_to` is its **first consumer**; the other two children wire the rest. The definition is deliberately left **unchanged by its consumers**: a consumer needing a narrower grammar refuses the criterion at its own declaration site — as `applies_to` does for `change_kind` — rather than editing the shared shape out from under the others.

A predicate carries four optional criteria, each a non-empty list:

- `paths` — doublestar globs matched against the change's paths, with `**` crossing `/`. Repo-relative and slash-separated, matched with the same `doublestar.Match` call the plan-gate test sweep already makes.
- `labels` — issue/PR label names.
- `change_kind` — change-kind tokens.
- `trigger` — the run shapes this predicate accepts: `diff` (a code-diff run), `scheduled` and `on_demand`. The last two are the **non-diff** forms: ADR-067 decoupled stage types from "produces a diff", so a scheduled backlog groomer (ADR-065) or an on-demand incident intake (ADR-053) must be routable by the same mechanism as a diff run. A change carrying no trigger normalizes to `diff`, so an existing diff-shaped caller need not set it.

**Match semantics are AND across criteria types, OR within a list.** Every *declared* criteria type must be satisfied (the AND); a type is satisfied when *any one* of its entries matches (the OR). An *undeclared* type does not constrain the match.

| A predicate declaring… | matches a change when… |
|---|---|
| `paths` only | any change path matches any glob |
| `paths` and `labels` | a path matches **and** a label matches (both types) |
| `labels: [a, b]` | the change carries `a` **or** `b` (either entry) |
| `trigger: [scheduled]` and nothing else | the change's trigger is `scheduled` (a no-path change matches) |
| `paths` (against a `scheduled`, no-path change) | never — the paths type is declared and unsatisfiable against zero paths |

**An empty predicate — no criterion at all — is a validation error, never match-all.** It is rejected both at the schema layer (`minProperties: 1`) and by the Go validator, so "match everything" can never be written by accident. Path handling is binary-safe and fail-closed: the matcher decodes git's C-quoted path form and reports an undecodable path by name rather than leaving it silently unmatched.

## Control surface: what is enforced, and where

The control-surface fields (`reviewers.authority`, `applies_to`, `escalations`, `permissions`) do **not** share one enforcement status, and reading them as if they did — "declared, therefore guaranteed", or the tidier and equally false "everything here is unenforced" — misstates what the product actually holds. This table is the consolidated per-control account. It states nothing the sections above do not; it keeps the honest split in one place so no reader has to reassemble it. A "declared" control is validated, audited and surfaced, but it is not a guarantee until a seam reads it. The same split, in the same words, governs this repository's own governance in [`docs/METHODOLOGY.md`](../METHODOLOGY.md); the two are written to be read against each other.

| Control | What it constrains | Enforcement status |
|---|---|---|
| `reviewers.authority` | Whether a stage's agent reviewers gate advancement or merely advise (ADR-027). | **Enforced.** A declared `authority` wins over the count-derived reading at the review gate; the resolved mode and its provenance surface as `review_authority[]`. See [Authority](#authority). |
| `applies_to.labels`, `applies_to.trigger` | Which changes a workflow may be used for, by issue label and run shape. | **Enforced at run admission** — `POST /v0/runs` **and** the webhook dispatch path — through one shared evaluation core, fail-closed. See [Workflow routing](#workflow-routing-applies_to). |
| `applies_to.paths` | Confines a workflow's change set to declared globs. | **Enforced at the plan gate** against the `scope.files` union, universally quantified. **Refused at validation on a workflow that declares no plan stage** — there is no `scope.files` producer for it to check, so it could never be evaluated (E53.15 / #2377). |
| `escalations` | Raises the approval count, membership conjunction, minimum permission, or autonomy ceiling for a change matching a predicate. | **Enforced where declared**, at the approval gate and in delegation resolution; a workflow declaring none short-circuits before any extra read. A `match.paths` criterion is **refused at validation on a workflow that declares no plan stage** — no `scope.files` producer, so it could never fire (E53.16 / #2382). The *mechanism* is shipped and tested; whether it holds on any given path depends on a declaration existing there. See [Escalations](#escalations). |
| `permissions.network` | The egress host(s) a stage's agent may reach. | **Enforced on an agent-executor `acceptance` stage**, where it normalizes into `egress` and the runner's default-deny proxy applies it — the pre-existing ADR-050 control. On **every other stage** it is a declaration only, until E51 (#2133). The run-status per-entry `enforced` flag encodes exactly this split. |
| `permissions.write` | The paths a stage's agent is expected to write. | Declared, audited (`stage_permissions_declared`) and surfaced (`permissions[]`), but **not enforced anywhere**, until E51 (#2133). |
| `permissions.shell` | The stage's shell posture (`none` / `restricted` / `unrestricted`). | Declared, audited and surfaced, but **not enforced anywhere**, until E51 (#2133). |

The `permissions.network` row is the one place the "everything under `permissions:` is unenforced" shorthand is false, and getting it wrong under-claims a real control as badly as the tidy version over-claims the others: on an acceptance stage the network declaration is enforced today, because `permissions.network` is a spelling of `egress` and lands on the one enforced path. `permissions.write` and `permissions.shell` have no reader yet, and `escalations` holds only where an author has declared it.

## Appendix: what changed from v0/v1

Historical record. This appendix documents how v2 came to differ from v1 and what each E52 child licensed; it is **not** the reference — every member v2 declares is documented above.

> **v2 *began* as a structural copy of v1 (ADR-067 / #2213; the ADR-046 major-copy precedent) and the E52 children diverged it.** At the copy, every `$defs` entry and every non-`version` property was **validation-identical** to [`workflow-v1.schema.json`](workflow-v1.schema.json) — same keys, types, enums, `required` lists, `additionalProperties` settings, and `oneOf`/`anyOf`/`allOf` branches — with only the description strings, `$id`, `title`, and the `version` property differing. The invariant was **structural/validation equivalence, not byte identity**. That copy-fidelity check is now **retired** (#2320, settled): it existed to catch an accidental drop *during the copy*, and with v2 deliberately edited by the E52 children it had become a changelog of intended divergences rather than an invariant. The per-child sections below are the record. v0 and v1, by contrast, are **frozen** majors and are pinned by content digest in `backend/internal/spec` (`TestFrozenMajorsV0AndV1AreImmutable`).

### Summary of the divergences

- **The version enum collapsed to the single token `"2"`.** v0 carried an additive `0.3…0.7` chain and v1 a `1.0…1.6` chain; v2 has no minor chain.
- **Field acceptance is by schema declaration (ADR-067 scope item 4).** The inherited minor-gating prose (`Requires version 0.N+`, `accepted at every advertised version`) was rewritten to field-presence language throughout the v2 descriptions.
- **The bare `reviewer_reject` page-event token was removed (E52.3 / #2215).**
- **The `reviewers.agent` integer count was removed (E52.3 / #2215).**
- **The legacy `approvers` allow-list and the top-level `roles` map were removed (E52.2 / #2214):** the forge-neutral `approvals` block became the sole approval predicate.
- **Three surfaces were reshaped for legibility (E52.6 / #2218):** `constraints` became an object, `drive` became `auto_advance`, and `needs:` was added as shorthand for artifact wiring.
- **Stage-budget units were unified (E52.5 / #2217):** the runtime cap `max_runtime_minutes` became the Go-duration `max_runtime`, and `limit_usd` was added as the primary cost lever with `max_tokens` demoted to optional secondary.
- **The `operator_agent` delegation block was replaced by the unified autonomy grammar (ADR-066 / E52.10 / #2222):** an `actions` matrix of action classes at `mode: report | gated | auto`, plus an `autonomy: low | medium | high` tier shorthand.
- **Post-hoc diff constraints were re-bound to the produced artifact (E52.7 / #2219)** rather than to the stage type name.
- **Same-document reuse was added (E52.4 / #2216):** file- and workflow-level `defaults`, and workflow `extends`.

### Removed from v1

v2 dropped five back-compat duplicate surfaces. Four had an explicit successor already shipped in v0/v1, so the removal deleted a second way to say the same thing rather than a capability; the fifth (`operator_agent`) was replaced by a grammar that says strictly more. **v0 and v1 are unchanged** — the old forms remain valid there, and the shared Go types still carry them, so an existing spec keeps working until it is migrated (migration codemod: `fishhawk migrate-spec`, E52.8 / #2220 — see [`workflow-migration.md`](workflow-migration.md)).

| Removed in v2 | Replacement | Why |
|---|---|---|
| `operator_agent.must_page_human: [reviewer_reject]` | `gating_reviewer_reject` (and its sibling `advisory_reviewer_reject`) | The bare token was the pre-#1378 form and always resolved to the *gating* sense. The two explicit classes state the review authority at the declaration site instead of leaving it to be resolved. |
| `reviewers.agent: <N>` | `reviewers.agents: [{provider, model?}, …]` | The heterogeneous list (#955) already superseded the bare count — the effective agent count is `len(agents)`. Keeping both left two inputs feeding one ADR-027 authority decision. |
| gate `approvers: {any_of \| all_of: [role, …]}` (E52.2 / #2214) | gate `approvals: {count, members \| member_of \| min_permission}` | The forge-neutral `approvals` block (ADR-055 / #1707) already superseded the GitHub-handle role allow-list. Keeping both left two mutually-exclusive predicates on one gate. The translation is **not mechanical** — `min_permission` / `member_of` have no source in the old form — so it belongs to the codemod, which emits a before/after approval-eligibility diff rather than rewriting blind. |
| top-level `roles: {name: {members: […]}}` (E52.2 / #2214) | `approvals.member_of` / `approvals.members` | The `roles` map existed only to be named by `approvers`; with `approvers` gone it has nothing left to reference. Forge-neutral membership moved onto the gate's `approvals` block. |
| `operator_agent: {may_*, route_fixup_min_severity, must_page_human, model_policy}` (E52.10 / #2222) | `actions: {<class>: {mode, when}}` + the `autonomy: low \| medium \| high` shorthand | Five boolean-ish `may_*` knobs could say only *delegated* or *not*, with no way to express "propose it and let me decide", no name for the thing being delegated, and no shorthand for the three tiers every workflow actually picks from. The matrix names each action class once and gives it a `mode`; the tier expands to a matrix. |

A v2 document using any of these forms is rejected with a message naming the replacement, not the generic enum / `additional properties` message: the backend (`backend/internal/spec/v2removed.go`) and the CLI (`cli/internal/spec/validate.go`) each sweep the raw document for a routed major `>= 2` *before* schema validation. The sweep matches by **key name at any depth** — deliberately over-triggering rather than risk missing a legacy form — so in an already-invalid document the legacy-form message may precede a structural error.

For the two-form v0/v1 approval grammar (the legacy `approvers` allow-list alongside `approvals`, mutually exclusive), see [`workflow-v1.md`](workflow-v1.md) — that page is unchanged, as v1 is frozen and still accepts both forms.

### Reshaped from v1

Three surfaces changed SHAPE in v2 (E52.6 / #2218). None changed what it MEANS: each is rewritten at parse time into the representation v0/v1 already use, so no consumer, no Go type, no DB column and no API field changed. **v0 and v1 are unchanged** — the old spellings remain valid there.

| Surface | v0 / v1 | v2 |
|---|---|---|
| `constraints` | a **list** of objects, each pinned to exactly one kind (`maxProperties: 1`) | **one object** keyed by kind (`maxProperties` dropped; `minProperties: 1` stays) |
| auto-advance | `drive: true` | `auto_advance: true` |
| artifact wiring | `inputs: [{artifact: plan, from_stage: plan}]` | `needs: [plan]` (longhand still valid) |

```yaml
# v0 / v1
constraints:
  - max_files_changed: 45
  - forbidden_paths: ["infra/**"]

# v2
constraints:
  max_files_changed: 45
  forbidden_paths: ["infra/**"]
```

A v2 document using the list form is rejected with a message showing the object form, not the generic array/object type error; a v2 document using `drive:` is rejected with a message naming `auto_advance`. The `drive` rename is a **pure rename of the spec surface** — the parser rewrites `auto_advance` to the `drive` key before the typed decode, so `spec.Workflow.Drive`, the `runs.drive` column, the per-run `POST /v0/runs` override and every read site see the identical value.

#### Why no constraint consumer changed

The obvious move — collapse the v0/v1 **list** into one canonical object with a merge rule — was **rejected**, and this is the design decision the slice turned on.

Five consumers read `[]spec.Constraint`, and they fold a duplicate-kind document **differently**: `mergeConstraints` concatenates slice kinds and takes min-wins on `max_files_changed` / max-wins on `diff_coverage`; `flattenPathConstraints` concatenates and takes min-wins; `resolveDiffCoverageConfig` takes max-wins; the deploy pre-flight gate in `approvals.go` **assigns** inside its loop, so **last wins**; and `deployEnvironmentForRun` returns the **first** non-empty entry. On one document — `[{allowed_environments: [staging]}, {allowed_environments: [prod]}]` — the two deploy sites already disagree with **each other** today: the gate permits only `prod`, the deploy record reports `staging`. A canonical merge rule cannot preserve three mutually inconsistent folds, and concatenation specifically would have **silently loosened** that governance gate by also permitting `staging`.

So the rewrite runs in the other direction, which is **exact** rather than approximate: an object's keys are unique, so it denotes exactly **one** `Constraint`, and the parser normalizes a v2 object into a **one-element list** — the single-entry case all five folds agree on. v0/v1 documents keep their list representation and are never re-resolved. The divergences are preserved by not being touched, and v2 cannot express the duplicate-kind document that makes them differ.

A consequence worth stating: because a v2 object is one `Constraint`, a binding violation is reported at `/constraints/<kind>` rather than at a list index that names nothing the author wrote. Below major 2 the `/constraints/<index>` form is unchanged.

### Units

The per-stage `budget` spells its units the way the rest of v2 does (E52.5 / #2217): one duration form throughout, and cost in USD.

| v0 / v1 | v2 |
|---|---|
| `max_runtime_minutes: 15` (bare integer minutes) | `max_runtime: 15m` (Go duration string) |
| `max_tokens` the primary lever; no stage-level USD ceiling | `limit_usd` the **primary** cost lever; `max_tokens` an **optional secondary** lever |

Because minutes was a lossless integer, `15` becomes `15m`; unlike the integer form, `max_runtime` can express sub-minute caps such as `90s`. **v0 and v1 are unchanged** — both keep the `max_runtime_minutes` spelling and reject the v2 forms (each major's `$defs/budget` is `additionalProperties: false`). A v2 document using `max_runtime_minutes` is rejected with a message naming `max_runtime` and showing the equivalence (`max_runtime_minutes: 15` → `max_runtime: 15m`).

`enforcement` semantics were unchanged by that slice: it unified the *grammar* only. Stage-budget enforcement is tracked on **#2328**.

### Post-hoc constraint binding (E52.7 / #2219)

**v0 and v1 are unchanged** — there the post-hoc binding stays type-keyed (a post-hoc constraint is valid on any non-deploy stage). This is not an oversight: v0/v1 documents legitimately declare these constraints on an `implement` stage with no `produces` list at all, and applying the artifact-keyed rule below major 2 would newly reject valid specs. The generalization is licensed only from v2 forward.

## See also

- [Control surface: what is enforced, and where](#control-surface-what-is-enforced-and-where) — the consolidated per-control enforcement account, read against `docs/METHODOLOGY.md`.
- [`README.md`](README.md) — the versioning + coexistence policy across the three majors.
- [`workflow-migration.md`](workflow-migration.md) — `fishhawk migrate-spec`, the v1 → v2 codemod and its approval-eligibility report.
- [`workflow-preset.md`](workflow-preset.md) — the three shipped presets `fishhawk init` writes.
- [`plan-standard-v1.md`](plan-standard-v1.md) — the plan artifact a `plan` stage produces.
- [`workflow-v1.md`](workflow-v1.md), [`workflow-v0.md`](workflow-v0.md) — the frozen earlier majors, for reading an unmigrated spec.
- `docs/ARCHITECTURE.md` §4 — workflow run lifecycle; §10 — where the grammar, reuse, shape and binding code lives.
- [`docs/ARCHITECTURE.md` §8.1](../ARCHITECTURE.md#81-approval-identity) — approval identity: the IdentityProvider seam and the `predicate_snapshot` lifecycle.
