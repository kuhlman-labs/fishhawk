# Workflow spec v2

Reference for `.fishhawk/workflows.yaml` at major version 2. The canonical schema is [`workflow-v2.schema.json`](workflow-v2.schema.json) (JSON Schema Draft 2020-12).

> **v2 *began* as a structural copy of v1 (ADR-067 / #2213; the ADR-046 major-copy precedent) and the E52 children now diverge it.** At the copy, every `$defs` entry and every non-`version` property was **validation-identical** to [`workflow-v1.schema.json`](workflow-v1.schema.json) — same keys, types, enums, `required` lists, `additionalProperties` settings, and `oneOf`/`anyOf`/`allOf` branches — with only the description strings, `$id`, `title`, and the `version` property differing. The invariant was **structural/validation equivalence, not byte identity**. That copy-fidelity check is now **retired** (#2320, settled): it existed to catch an accidental drop *during the copy*, and with v2 deliberately edited by five E52 children it had become a changelog of intended divergences rather than an invariant. **Each deliberate divergence is recorded in the per-child sections below, which are now the record.** v0 and v1, by contrast, are **frozen** majors and are pinned by content digest in `backend/internal/spec` (`TestFrozenMajorsV0AndV1AreImmutable`).

## What changed from v1

- **The version enum collapses to the single token `"2"`.** Unlike v0's additive `0.3…0.7` chain and v1's `1.0…1.6` chain, **v2 has no minor chain**. Declare exactly `"2"`; the dotted minor forms (`"2.0"`, `"2.1"`, …) route to this schema by major but are **rejected** by the collapsed enum.
- **Field acceptance is by schema declaration (ADR-067 scope item 4).** A field is accepted because this schema declares it — not because the document declared a high enough minor. The inherited minor-gating prose (`Requires version 0.N+`, `accepted at every advertised version`) has been rewritten to field-presence language throughout the v2 descriptions.
- **The bare `reviewer_reject` page-event token is removed (E52.3 / #2215).** See *Removed from v1* below.
- **The `reviewers.agent` integer count is removed (E52.3 / #2215).** See *Removed from v1* below.
- **The legacy `approvers` allow-list and the top-level `roles` map are removed (E52.2 / #2214):** the forge-neutral `approvals` block becomes the sole approval predicate. See *Removed from v1* and *Approval gate predicate (v2)* below.
- **Three surfaces are reshaped for legibility (E52.6 / #2218):** `constraints` becomes an object, `drive` becomes `auto_advance`, and `needs:` is added as shorthand for artifact wiring. See *Reshaped from v1* below.
- **Stage-budget units are unified (E52.5 / #2217):** the runtime cap `max_runtime_minutes` becomes the Go-duration `max_runtime`, and `limit_usd` is added as the primary cost lever with `max_tokens` demoted to optional secondary. See *Units* below.
- **The `operator_agent` delegation block is replaced by the unified autonomy grammar (ADR-066 / E52.10 / #2222):** an `actions` matrix of action classes at `mode: report | gated | auto`, plus an `autonomy: low | medium | high` tier shorthand. See *Autonomy: tier shorthand and action matrix* below.

## Removed from v1

v2 drops five back-compat duplicate surfaces. Four had an explicit successor already shipped in v0/v1, so the removal deletes a second way to say the same thing rather than a capability; the fifth (`operator_agent`) is REPLACED by a new grammar that says strictly more. **v0 and v1 are unchanged** — the old forms remain valid there, and the shared Go types still carry them, so an existing spec keeps working until it is migrated to v2 (migration codemod: `fishhawk migrate-spec`, E52.8 / #2220 — see [`workflow-migration.md`](workflow-migration.md)).

| Removed in v2 | Replacement | Why |
|---|---|---|
| `operator_agent.must_page_human: [reviewer_reject]` | `gating_reviewer_reject` (and its sibling `advisory_reviewer_reject`) | The bare token was the pre-#1378 form and always resolved to the *gating* sense. The two explicit classes state the review authority at the declaration site instead of leaving it to be resolved. |
| `reviewers.agent: <N>` | `reviewers.agents: [{provider, model?}, …]` | The heterogeneous list (#955) already superseded the bare count — the effective agent count is `len(agents)`. Keeping both left two inputs feeding one ADR-027 authority decision. |
| gate `approvers: {any_of \| all_of: [role, …]}` (E52.2 / #2214) | gate `approvals: {count, members \| member_of \| min_permission}` | The forge-neutral `approvals` block (ADR-055 / #1707) already superseded the GitHub-handle role allow-list. Keeping both left two mutually-exclusive predicates on one gate. The translation is **not mechanical** — `min_permission` / `member_of` have no source in the old form — so it belongs to the `fishhawk migrate-spec` codemod (E52.8 / #2220; see [`workflow-migration.md`](workflow-migration.md)), which emits a before/after approval-eligibility diff rather than rewriting blind. See *Approval gate predicate (v2)*. |
| top-level `roles: {name: {members: […]}}` (E52.2 / #2214) | `approvals.member_of` / `approvals.members` | The `roles` map existed only to be named by `approvers`; with `approvers` gone it has nothing left to reference. Forge-neutral membership moves onto the gate's `approvals` block. |
| `operator_agent: {may_*, route_fixup_min_severity, must_page_human, model_policy}` (E52.10 / #2222) | `actions: {<class>: {mode, when}}` + the `autonomy: low \| medium \| high` shorthand | Five boolean-ish `may_*` knobs could say only *delegated* or *not*, with no way to express "propose it and let me decide", no name for the thing being delegated, and no shorthand for the three tiers every workflow actually picks from. The matrix names each action class once and gives it a `mode`; the tier expands to a matrix. See *Autonomy: tier shorthand and action matrix*. |

A v2 document using any of these forms is rejected with a message naming the replacement, not the generic enum / `additional properties` message: the backend (`backend/internal/spec/v2removed.go`) and the CLI (`cli/internal/spec/validate.go`) each sweep the raw document for a routed major `>= 2` *before* schema validation. The sweep matches by **key name at any depth** — deliberately over-triggering rather than risk missing a legacy form — so in an already-invalid document the legacy-form message may precede a structural error.

The ADR-027 authority table now reads on `len(agents)`:

| Reviewers | Authority |
|---|---|
| `len(agents) > 0` and `human == 0` | gating (agent rejections block advancement) |
| `len(agents) > 0` and `human > 0` | advisory (agent verdicts surfaced, cannot block) |
| `len(agents) == 0` | gateless |

An absent `reviewers` block is unchanged by this slice.

## Approval gate predicate (v2)

At v2 the forge-neutral `approvals` block is the **sole** approval predicate (E52.2 / #2214). The gate approval branch lists `approvals` in its `required` set, so an approval gate **always** declares it and can never be a no-op — this is the property the removed inner `oneOf` (which chose between `approvers` and `approvals`) used to guarantee. A gate declaring `type: approval` with no `approvals` block, or `approvals: {}` (missing the required `count`), is rejected.

```yaml
gates:
  - type: approval
    approvals:
      count: 1
      not: [author, agent]
```

- `count` is **required** (an integer `>= 1`, always explicit per ADR-055), so an empty predicate fails validation.
- `not` excludes relationship classes (`author`, `agent`) from satisfying the gate.
- `min_permission` (forge-neutral repository permission tier) and `member_of` (a forge-neutral org/team) are **optional** and are annotated `x-intended-required` — intended to become required in a future major. They were **deliberately NOT promoted to required here**; that promotion is a separate decision.

Because `min_permission` / `member_of` have no source in the legacy `approvers` role allow-list, translating an old gate is **not mechanical**: migration belongs to the `fishhawk migrate-spec` codemod (E52.8 / #2220; see [`workflow-migration.md`](workflow-migration.md)), which emits a before/after approval-eligibility diff rather than rewriting blind.

For the two-form v0/v1 grammar (the legacy `approvers` allow-list alongside `approvals`, mutually exclusive), see [`workflow-v1.md`](workflow-v1.md)'s *Approval gate predicate (v1)* — that page is unchanged, as v1 is frozen and still accepts both forms.

## Autonomy: tier shorthand and action matrix

v2 replaces the `operator_agent` block with two surfaces (ADR-066 / E52.10 / #2222):

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
      page_human_on:          # RESERVED key — v0/v1's must_page_human
        - gating_reviewer_reject
```

### The three modes

`mode` says **who acts**:

| `mode` | Meaning |
|---|---|
| `gated` | The human acts. This is the **fail-closed default**: it is what an absent matrix, an absent class entry and an unrecognized class all resolve to, and it is byte-identical to v0/v1 knob-absence. |
| `report` | The operator agent surfaces a **proposal** and does not act. It records a `run_auto_driven` attribution row with `act: report`; the run does **not** park on it and no gate action is dispatched. |
| `auto` | The operator agent may act without paging — the only mode that widens authority, and the only one that requires a condition. |

**When a `report` entry fires:** when the gate is **live**, and — only when the entry declares a `when` — additionally when that condition is met. A bare `report` entry with no `when` therefore surfaces whenever the gate is reachable; requiring a condition that need not exist would make the mode inert. A `when` declared on a `report` entry is subject to the **same per-class binding** as `mode: auto`: it must name that class's own condition (a foreign or extension-class `when` is rejected — see below). The row is emitted **at most once per gate occurrence per class**. A gate occurrence spans one opening of the gate: a gate a human takes an hour to reach produces one row, not one per poll cycle, while a gate that **closes and re-opens on a fresh review round** (a fix-up round trip) is a new occurrence and re-surfaces the proposal.

### `mode: auto` requires a condition

Each known action class has exactly **one** backend-evaluable condition — the backend has to be able to answer the predicate from run state:

| Class | `when` | v0/v1 knob it replaces |
|---|---|---|
| `approve` | `clean_dual_approval` | `may_approve` |
| `fixup` | `convergent_concerns` (+ `min_severity`) | `may_route_fixup` (+ `route_fixup_min_severity`) |
| `waive` | `solo_low` | `may_waive` |
| `retry` | `infra_flake` | `may_retry` |
| `merge` | `gates_resolved_ci_green` | `may_merge` |

Four documents are rejected with a message naming the class:

- `mode: auto` with **no** `when`,
- `mode: auto` whose `when` is another class's condition,
- `mode: auto` on an **extension** class (below), which has no backend-evaluable condition at all,
- `mode: report` that declares a `when` which is **not that class's own condition** (a foreign condition, or any `when` on an extension class). A bare `report` with no `when` is always accepted (it fires on gate-live); a declared `when` must name the class's own condition, so a proposal the report arm cannot evaluate is refused at validation rather than silently dropped at fire time.

These rules are enforced by the **backend**, not by JSON Schema: the class-name set is open, so no schema keyword can bind a condition to a class the schema does not enumerate. A raw `check-jsonschema` run therefore accepts a document the backend rejects. `min_severity` is likewise accepted on the `fixup` class only.

### Extension classes

The class-name set is deliberately **open** and per-workflow-type extensible (ADR-065): a workflow type may declare its own class, e.g. `promote` or `rollback`. An unknown class is safe **by construction** — accepted at `mode: gated` and `mode: report`, where it delegates nothing, and rejected at `mode: auto`, where it would need a condition that does not exist.

### The tiers

`autonomy` expands to a matrix. The three tiers are METHODOLOGY.md's autonomy tiers and reproduce the shipped `workflow-preset-<tier>.yaml` delegation blocks:

| Class | `low` | `medium` | `high` |
|---|---|---|---|
| `approve` | `gated` | `auto` / `clean_dual_approval` | `auto` / `clean_dual_approval` |
| `fixup` | `gated` | `auto` / `convergent_concerns` | `auto` / `convergent_concerns` |
| `retry` | `gated` | `auto` / `infra_flake` | `auto` / `infra_flake` |
| `waive` | `gated` | `gated` | `auto` / `solo_low` |
| `merge` | `gated` | `gated` | `auto` / `gates_resolved_ci_green` |
| `page_human_on` | *(none)* | the seven events below | the seven events below |

The medium/high page list is `gating_reviewer_reject`, `plan_rejection`, `scope_amendment`, `budget_override`, `policy_override`, `exception_request`, `requirement_arbitration`. Note the **v2 spelling**: a tier emits `gating_reviewer_reject`, never the bare `reviewer_reject` v2 removed — a tier must not expand to a value undeclarable under the grammar it belongs to.

An explicit `actions` entry **overrides the tier for that class only**; unlisted classes keep the tier's value, and a class no tier names resolves to `gated`.

### Placement, and the wholesale gate override

Both surfaces are declarable at **workflow** level (the default for every gate) and on an **approval gate**. A gate declaring **either** `autonomy` or `actions` supplies the WHOLE block — nothing is inherited from the workflow level, matching the wholesale-override semantics `operator_agent` had. So a gate declaring only `actions: {merge: {mode: auto, when: gates_resolved_ci_green}}` on an `autonomy: high` workflow has **no tier**: `merge` is auto and every other class falls to `gated`, not to high's value. `page_human_on` and `model_policy` are part of the block and are likewise never merged across levels.

### Provenance

Resolution records, per class, **which input decided it**: `explicit` (an `actions` entry), `tier` (the shorthand expansion), or `default` (the fail-closed fallback). The resolved tier and matrix are surfaced on the run-status `delegation` block alongside the existing evaluated-action list, so an operator can read `approve: gated (tier)` rather than infer it from a missing knob.

### `model_policy` moved onto the matrix

`model_policy` (#1421) is a **reserved key** of `actions` rather than a new top-level block, so it keeps the wholesale gate-override semantics it already had as part of `operator_agent`. `page_human_on` is the other reserved key — it is v0/v1's `must_page_human` under its new name. Neither is an action class, and a property named like one of them is never treated as one.

## Reshaped from v1

Three surfaces change SHAPE in v2 (E52.6 / #2218). None changes what it MEANS: each is rewritten at parse time into the representation v0/v1 already use, so no consumer, no Go type, no DB column and no API field changed. **v0 and v1 are unchanged** — the old spellings remain valid there (migration codemod: `fishhawk migrate-spec`, E52.8 / #2220 — see [`workflow-migration.md`](workflow-migration.md)).

### `constraints` is an object

| v0 / v1 | v2 |
|---|---|
| a **list** of objects, each pinned to exactly one kind (`maxProperties: 1`) | **one object** keyed by kind (`maxProperties` dropped; `minProperties: 1` stays) |

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

`constraints: {}` is still rejected as a no-op. A v2 document using the list form is rejected with a message showing the object form, not the generic array/object type error.

#### Why no constraint consumer changed

The obvious move — collapse the v0/v1 **list** into one canonical object with a merge rule — was **rejected**, and this is the design decision the slice turns on.

Five consumers read `[]spec.Constraint`, and they fold a duplicate-kind document **differently**: `mergeConstraints` concatenates slice kinds and takes min-wins on `max_files_changed` / max-wins on `diff_coverage`; `flattenPathConstraints` concatenates and takes min-wins; `resolveDiffCoverageConfig` takes max-wins; the deploy pre-flight gate in `approvals.go` **assigns** inside its loop, so **last wins**; and `deployEnvironmentForRun` returns the **first** non-empty entry. On one document — `[{allowed_environments: [staging]}, {allowed_environments: [prod]}]` — the two deploy sites already disagree with **each other** today: the gate permits only `prod`, the deploy record reports `staging`. A canonical merge rule cannot preserve three mutually inconsistent folds, and concatenation specifically would have **silently loosened** that governance gate by also permitting `staging`.

So the rewrite runs in the other direction, which is **exact** rather than approximate: an object's keys are unique, so it denotes exactly **one** `Constraint`, and the parser normalizes a v2 object into a **one-element list** — the single-entry case all five folds agree on. v0/v1 documents keep their list representation and are never re-resolved. The divergences are preserved by not being touched, and v2 cannot express the duplicate-kind document that makes them differ.

A consequence worth stating: because a v2 object is one `Constraint`, a binding violation is reported at `/constraints/<kind>` (e.g. `/constraints/change_freeze`) rather than at a list index that names nothing the author wrote. Below major 2 the `/constraints/<index>` form is unchanged.

### `drive` is `auto_advance`

```yaml
# v0 / v1              # v2
drive: true            auto_advance: true
```

A **pure rename of the spec surface**. The parser rewrites `auto_advance` to the `drive` key before the typed decode, so `spec.Workflow.Drive`, the `runs.drive` column, the per-run `POST /v0/runs` `drive` override and every auto-advance / `next_action` read site are untouched and see the identical value. A v2 document using `drive:` is rejected with a message naming `auto_advance`.

### `needs:` shorthand for artifact wiring

```yaml
# longhand (valid in v2 too)        # shorthand
inputs:                             needs: [plan]
  - artifact: plan
    from_stage: plan
```

Each entry names an **earlier** stage whose default artifact this stage consumes, and expands to the equivalent `inputs` entry. The artifact is derived from the **referenced** stage's type:

| Referent type | Derived artifact |
|---|---|
| `plan` | `plan` |
| `implement` | `pull_request` |
| `review`, `deploy`, `acceptance` | **rejected** — no default input artifact; declare the wiring longhand with `inputs:` |

The mapping is `$defs/input`'s artifact enum, whose only members are `plan` and `pull_request`: a review stage produces no artifact, and the `deployment` / `acceptance` artifacts are not declarable as inputs.

**`needs:` and longhand `inputs:` MAY be combined on the same stage** (the explicit decision for this slice). Ordering and dedupe:

- declared `inputs` keep their positions; derived entries follow, in `needs` order;
- a derived entry whose `(artifact, from_stage)` pair already appears verbatim among the declared inputs is **dropped**.

So the resolved input set is identical however the author spelled it.

Referent errors reuse the existing `from_stage` graph-shape rules with their unchanged messages: a referent that does not exist reports `from_stage "…" does not match any stage id`, and a self or later reference reports the must-be-earlier error. One tradeoff follows from that choice: the report names the post-expansion `inputs` index rather than the `needs` entry the author wrote. That is deliberate — one canonical error beats two competing ones — and it is recorded in `backend/internal/spec/README.md`.

> **`fishhawk validate` does NOT check `needs` referents.** `cli/internal/spec` is a deliberately separate, **schema-only** module: it performs no typed decode and no graph-shape pass at all, so it already accepts a longhand `inputs[].from_stage` naming a nonexistent stage. Both `needs`-referent errors — a nonexistent stage, and a `review` / `deploy` / `acceptance` referent with no default input artifact — therefore surface **only server-side, at run creation**, not from `fishhawk validate`. The CLI does validate the `needs` **shape** (array of stage-id-patterned strings) and rejects the two legacy spellings above with messages byte-identical to the backend's. Closing the general asymmetry needs its own decision about duplication versus coupling and is tracked on **#2323**.

### Units

The per-stage `budget` now spells its units the way the rest of v2 does (E52.5 / #2217): one duration form throughout, and cost in USD.

| v0 / v1 | v2 |
|---|---|
| `max_runtime_minutes: 15` (bare integer minutes) | `max_runtime: 15m` (Go duration string) |
| `max_tokens` the primary lever; no stage-level USD ceiling | `limit_usd` the **primary** cost lever; `max_tokens` an **optional secondary** lever |

```yaml
# v0 / v1
budget:
  max_tokens: 200000
  max_runtime_minutes: 15

# v2
budget:
  limit_usd: 8.5        # primary cost ceiling, same unit as budgets[].limit_usd
  max_runtime: 15m      # 15 becomes 15m; sub-minute values (e.g. 90s) are now expressible
  max_tokens: 200000    # optional secondary lever
```

- **`max_runtime` is the same form as every other v2 duration.** It uses the identical pattern (`^([0-9]+(ns|us|ms|s|m|h))+$`) as `policy.max_stage_runtime`, `executor.timeout` and `executor.verify.timeout`, and is parsed by the same `time.ParseDuration` code path. The pattern is a **strict subset** of what `time.ParseDuration` accepts (the parser also accepts fractional `1.5h`, signed, and micro-sign forms the pattern rejects), **matching the convention of the three existing v2 duration fields** byte-for-byte rather than the parser's full grammar. Because minutes was a lossless integer, `15` becomes `15m`; unlike the integer form, `max_runtime` can express sub-minute caps such as `90s`.
- **`limit_usd` is primary but NOT required (AC-4).** A budget declaring only `max_tokens`, only `limit_usd`, only `max_runtime`, any combination, or nothing at all is valid. ADR-067 ratified unifying the units, not mandating a cost model, so the choice is stated in the schema description rather than enforced by a required field.
- **`enforcement` semantics are UNCHANGED by this slice.** This change unifies the *grammar* only — no runtime check reads a stage budget on any major. Stage-budget enforcement (spending against the `limit_usd` ceiling, cost accounting) is tracked on **#2328**.

**v0 and v1 are unchanged** — both keep the `max_runtime_minutes` spelling and reject the v2 forms (each major's `$defs/budget` is `additionalProperties: false`), so an existing spec keeps working until it is migrated to v2 (migration codemod: `fishhawk migrate-spec`, E52.8 / #2220 — see [`workflow-migration.md`](workflow-migration.md)). A v2 document using `max_runtime_minutes` is rejected with a message naming `max_runtime` and showing the equivalence (`max_runtime_minutes: 15` → `max_runtime: 15m`).

## Grammar

Apart from the two removals and three reshapes above, the grammar is **identical to v1** — every stage type, executor branch, constraint kind, produces artifact, gate shape, delegation block, and reviewer shape carries over unchanged. For the full reference see [`workflow-v1.md`](workflow-v1.md) (the v1 deploy/acceptance/egress/agent-version/verification/diff-coverage surfaces) and [`workflow-v0.md`](workflow-v0.md) (the base grammar). A minimal v2 spec differs from a v1 spec only in its `version` value:

```yaml
version: "2" # required; routes to workflow-v2.schema.json (no minor form — "2.0" is rejected)
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
```

Sibling E52 children grow this page as they change the v2 grammar; for every member not listed under *Removed from v1*, *Reshaped from v1*, *Reuse: `defaults` and `extends`*, or *Stage types: propose / apply / gate*, the v1 reference is authoritative.

## Reuse: `defaults` and `extends`

v2 adds the two SAME-DOCUMENT reuse primitives earlier majors never had (E52.4 / #2216):

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

Consider file defaults `reviewers: {human: 1, agents: [a, b]}` and a stage declaring `reviewers: {agents: [c]}`. A key-wise merge resolves to `{human: 1, agents: [c]}` — the stage's agents correctly replace the default's, but `human: 1` is **supplemented from a block the author never wrote on that stage**. That is not cosmetic: the ADR-027 authority table reads `len(agents) > 0 && human == 0` as **gating** and `len(agents) > 0 && human > 0` as **advisory**, so the supplemented human silently converts a gating review into an arbitrable one, and nothing in the document says so.

So a stage (or workflow) that **declares** `reviewers` gets that block exactly as authored, with no merge from any defaults rung. Declared or inherited — never blended.

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

The cost is the same tradeoff `needs:` expansion accepted (#2218): a reported error **path** points at a position in the resolved document, which for a deriving workflow may not be a position its author wrote. Read the path against the resolved form, not the source.

The `defaults` and `extends` keys are stripped after validation, before the typed decode. No Go struct, database column, API field or consumer read site changed.

## Stage types: propose / apply / gate

The stage-type enum is **unchanged** in v2 — `plan`, `implement`, `review`, `deploy`, `acceptance`, same five tokens. What changes is what the validator reads them as (E52.7 / #2219).

The v0 names were coined for code-change workflows and read as if every workflow ships a diff. They do not. **The names are retained deliberately** (ADR-067 §2 — *do not rename*: renaming would churn every existing spec, the `stages.type` column, the API and the runner for a cosmetic gain), so each type carries both readings:

| Type | Code-change reading | General reading | Produces a diff? |
|---|---|---|---|
| `plan` | propose an implementation plan | **propose** — emit a proposal of any kind (a plan, a grooming report, a migration inventory) | no |
| `implement` | write the code | **apply** — apply the proposed change, whether that is source edits or tracker mutations | only if it says so |
| `review` | review the diff | **gate** — a decision point on the result | no |
| `deploy` | delegate a release (ADR-038) | unchanged — always delegating | no (delegated) |
| `acceptance` | validate a running instance (ADR-049) | unchanged — emits a verdict | no |

A workflow that grooms a backlog — propose a report, gate it on an approval, apply the approved mutations — is spelled with `plan` → `implement` → `review` and produces no code at any stage. Worked example: [`examples/workflow-v2-backlog-grooming.yaml`](examples/workflow-v2-backlog-grooming.yaml).

### Post-hoc diff constraints bind to the produced artifact

Because the type names are general, **constraint validity keys on what a stage PRODUCES, not on its type name.** At v2, a stage carrying any post-hoc diff constraint — `max_files_changed`, `forbidden_paths`, `allowed_paths`, `required_outcomes`, `diff_coverage` — must declare the `pull_request` artifact:

```yaml
# rejected at v2 — nothing to evaluate the constraint against
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

`pull_request` is the diff signal because it is the only artifact in the closed set that **denotes a code change**: `deployment` is delegated to an external pipeline, `acceptance` is a verdict, and `plan` is a proposal.

> **An absent or empty `produces` list reads as "produces no diff."** Omitting `produces` does not exempt a stage — that is what gives the rule teeth. The permissive alternative ("absent means unknown, so allow it") would let any stage keep a diff constraint simply by staying silent, which is exactly the case this rule exists to reject. This is a real behaviour change for a v2 author, and the fix is one line: declare the artifact, or drop the constraint.

**v0 and v1 are unchanged** — there the binding stays type-keyed (a post-hoc constraint is valid on any non-deploy stage). This is not an oversight: v0/v1 documents legitimately declare these constraints on an `implement` stage with no `produces` list at all, and applying the artifact-keyed rule below major 2 would newly reject valid specs. The generalization is licensed only from v2 forward (migration codemod: `fishhawk migrate-spec`, E52.8 / #2220 — see [`workflow-migration.md`](workflow-migration.md)).

Two orderings are worth knowing, both contracts rather than accidents:

- A **deploy** stage still gets its existing ADR-038 message (`post-hoc diff constraint is not valid on a deploy stage`), never this one.
- A stage that DOES produce `pull_request` but is typed other than `implement` still gets the existing `diff_coverage is valid only on an implement stage` message (#1888).

> **`fishhawk validate` does NOT report this binding.** `cli/internal/spec` is schema-only — no typed decode, no graph-shape pass — so like the `needs`-referent errors above, this surfaces **only server-side, at run creation**. The CLI accepts a document this rule rejects. Same asymmetry, tracked on **#2323**.

## Version routing

The backend (`backend/internal/spec`) and the CLI (`cli/internal/spec`) compile the workflow-v0, workflow-v1, **and** workflow-v2 schemas at init and dispatch a spec to one of them by its `version` **major** component:

- `version: "0.x"` → `workflow-v0.schema.json`
- `version: "1.x"` → `workflow-v1.schema.json`
- `version: "2"` → `workflow-v2.schema.json` (a dotted `"2.0"` routes here by major but the collapsed enum rejects it)
- a missing / non-string / unparseable `version` falls through to the v0 schema, which then emits the existing required-version error (so a malformed version never silently passes)
- a well-formed but unrecognized major (`>= 3`) **fails closed** with an error naming the supported majors (`0, 1, 2`)

`/healthz` advertises the `workflow-v0`, `workflow-v1`, and `workflow-v2` embedded-schema hashes so a component can detect drift in any of them.

## See also

- [`workflow-v1.md`](workflow-v1.md) — the grammar v2 inherited, authoritative for every member v2 has not diverged.
- [`workflow-v0.md`](workflow-v0.md) — the base grammar.
- [`README.md`](README.md) — the versioning + coexistence policy.
- `docs/ARCHITECTURE.md` §10 — workflow spec grammar and version routing.
