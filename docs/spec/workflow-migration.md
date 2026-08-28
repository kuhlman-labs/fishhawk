# workflow-v1 → workflow-v2 migration (`fishhawk migrate-spec`)

E52.8 / #2220. The codemod that translates a `.fishhawk/workflows.yaml` at version major 1 into the workflow-v2 grammar E52.2–E52.10 landed, and prints an approval-eligibility report saying how the migration moves who can approve what.

Implementation: `cli/internal/spec/migrate.go` (translation) + `cli/internal/spec/migrate_report.go` (report), driven by `cli/cmd/fishhawk/migrate_spec.go`.

```sh
fishhawk migrate-spec                      # report only; writes NOTHING
fishhawk migrate-spec --in-place           # rewrite .fishhawk/workflows.yaml
fishhawk migrate-spec path --out new.yaml  # write elsewhere
```

The codemod edits **yaml.v3 nodes**, never round-tripping through a typed struct, so comments and key ordering survive. A governance file is read by humans; a codemod that strips its rationale comments has destroyed the thing being migrated.

## Accepted input

| Source version major | Behaviour |
|---|---|
| 0 | **REFUSED** (branch R9). What parses and what is faithfully translatable are different questions: the v0 and v1 constraint folds diverge, and no v0 golden exists, so a v0 collapse to `"2"` would be a mistranslation shipped as a success. Migrate to v1 first, or ask for a v0 path. |
| 1 | Migrated. |
| 2 | **No-op**, exit 0 — but only for a document that VALIDATES as workflow-v2. An invalid v2 source (including `version: "2.0"`, which the v2 enum rejects) is refused as R0 rather than falsely certified as already-migrated. The report is still printed. |
| absent / ≥ 3 | Refused as R0 — the source-validation gate reports the schema's own error. |

## Translation table

| v0/v1 form | workflow-v2 form | Notes |
|---|---|---|
| `version: "1.x"` | `version: "2"` | v2 has no minor chain; its enum rejects the dotted forms. |
| workflow `drive: true` | `auto_advance: true` | Renamed in place — the key's leading comment and position survive. |
| stage `budget.max_runtime_minutes: 15` | `budget.max_runtime: 15m` | Same value, spelled as the Go duration string the other three v2 duration fields use. |
| stage `budget.max_tokens: N` | carried forward unchanged | v2 keeps it as an optional secondary lever. |
| — | `budget.limit_usd` | **Never fabricated.** No token-to-USD rate exists anywhere in this repo. The report notes per budget-bearing stage that a USD ceiling was not derivable and should be added by hand. |
| stage `constraints:` LIST of single-key maps | one `constraints:` OBJECT | Declaration order and each entry's comments are preserved; a comment written above a `- kind:` entry moves onto the key it documents. |
| `operator_agent.must_page_human: [reviewer_reject, …]` | `actions.page_human_on: [gating_reviewer_reject, …]` | The bare `reviewer_reject` token is rewritten to `gating_reviewer_reject` — the sense v0/v1 always resolved it to. |
| `operator_agent.may_approve` | `actions.approve: {mode: auto, when: clean_dual_approval}` | |
| `operator_agent.may_route_fixup` (+ `route_fixup_min_severity`) | `actions.fixup: {mode: auto, when: convergent_concerns, min_severity}` | |
| `operator_agent.may_waive` | `actions.waive: {mode: auto, when: solo_low}` | |
| `operator_agent.may_retry` | `actions.retry: {mode: auto, when: infra_flake}` | |
| `operator_agent.may_merge` | `actions.merge: {mode: auto, when: gates_resolved_ci_green}` | |
| `operator_agent.model_policy` | `actions.model_policy` | Carried verbatim; the two blocks are structurally identical. |
| gate `approvers: {any_of: [r1, …]}` | `approvals: {count: 1, members: <union>}` | One approval from anyone in the union — exactly what `any_of` means. |
| gate `approvers: {all_of: [r1, …]}` | `approvals: {count: <distinct members>, members: <union>}` | Only when **every** named role resolves to exactly one member; see below. |
| top-level `roles:` | deleted | v2's root is `additionalProperties: false` over `[version, workflows, defaults, test_conventions]`. |

Stage `egress`, `inputs`, `produces`, `needs`, workflow `policy`, `on_ci_failure` and `budgets` are carried through untouched.

### `all_of` cardinality

`count` is the number of **distinct members in the deduplicated union**, not the number of named roles.

Two singleton roles naming the same person are one human. Emitting `count: 2` over a one-member list yields a gate that can never clear — and nothing downstream would catch it: v2's `approvals` predicate requires only that `count` be an integer ≥ 1 and never cross-checks it against `members`, so the output-validation gate would pass it and ship a deadlocked gate. Counting distinct members is faithful in both sub-cases: disjoint singletons still yield `count = N`, and overlapping ones collapse correctly.

### Delegation: tier shorthand vs. explicit matrix

The translated matrix is compared against the three `autonomy` tier expansions. `autonomy: <tier>` is emitted **only on an exact match** across all five action classes AND the page list (compared as a set), and **only when the block carries no rationale comment on a `may_*` knob key or on `must_page_human`** — that comment gate is evaluated **before** the class/page-list comparison, so a comment-bearing block never reaches it. Any partial match emits the explicit `actions` matrix and no `autonomy` key: rounding a `may_approve`-only block up to `autonomy: medium` would hand the operator agent fixup and retry authority the source never granted.

- An **absent knob** is omitted entirely — absence already resolves to the fail-closed `mode: gated`.
- An **absent `operator_agent` block** emits **neither** key. It is deliberately not rounded to `autonomy: low`, which would additionally assert an empty `page_human_on` list the source never declared.
- A `route_fixup_min_severity` or a `model_policy` blocks the tier collapse — no tier expands to either.
- A **rationale comment on a `may_*` knob key or on `must_page_human`** blocks the tier collapse and deliberately KEEPS the block in explicit `actions` matrix form. Only the matrix has a per-class key and a `page_human_on` key to carry that comment onto; collapsing would silently erase the operator's written rationale for a delegation choice. The emitted matrix is the **same delegation** — the tier shorthand and the matrix expand to an identical matrix — so nothing widens or narrows, only the output shape differs. Presence is the whole test: a stray `#` line blocks the collapse exactly as a paragraph of rationale does, because a heuristic about which comments are "substantive" would be the kind of guess this refusal taxonomy exists to avoid.
- A comment on the **`operator_agent:` key itself** does **not** block the collapse. The key is renamed in place, so the comment rides onto the emitted `autonomy` key with nothing lost.

Two attachment details follow from yaml.v3 and are worth knowing when you read a migrated diff. A trailing comment on a scalar knob (`may_approve: clean_dual_approval  # why`) attaches to the *value* node, and the codemod reads it from there — unlike `must_page_human:  # why`, whose block-sequence value starts on the next line, so that comment lands on the key. But a comment written *below* the last `must_page_human` list item attaches to that **item**, reaching neither the key nor the gate: it does not block the collapse, and it is dropped in both output shapes, because the matrix path rebuilds the page list as bare scalars.

## What is never fabricated

`limit_usd`, `min_permission`, `member_of` and `not:` are never invented. The legacy grammar carries no source for any of them.

The `not:` case is the one with teeth. The shipped presets carry `approvals: {count: 1, not: [author, agent]}`; a gate migrated out of `approvers` emits no exclusion, so relative to that default the migration **widens** eligibility. Be precise about which half widens (#2358):

- **`author` widens for real.** Omitting it permits the run's resolved change author to approve *and* to count toward quorum. An author is resolved wherever the run carries a human-authorship audit signal — today an operator-vouched commit; operator governance acts (driving a stage, answering a clarification, approving, deciding an amendment) resolve none. So the widening bites on a run that carries such a signal, and is inert on one that does not.
- **`agent` does not widen.** The agent exclusion is an unconditional floor on any gate carrying an `approvals` block: an automated identity never satisfies a human quorum whether or not `not` names it. Declaring `agent` documents intent; omitting it grants nothing.

Historical note: before the backend read `not:` at all, a declared exclusion was grammar rather than enforcement, so dropping it changed nothing observable either way. That is no longer true of the `author` half.

The codemod reports the widening per gate rather than adding the exclusion silently, because adding it would be exactly the guess the refusal taxonomy exists to avoid.

## Refusal taxonomy

Ten named branches. Each aborts the **whole** migration: a non-empty refusal list means no bytes are produced under any flag, and the eligibility report is still printed.

| Branch | Fires when | Why there is no faithful equivalent |
|---|---|---|
| `R0_source_invalid` | the source does not pass `ValidateBytes` against its own major's schema | Migrating an already-invalid document produces garbage. This also subsumes the schema-level impossibilities (a gate declaring both `approvers` and `approvals`, a role with an empty members list). |
| `R1_all_of_multi_member_role` | `all_of` names a role resolving to more than one member | v2's `approvals` block has no per-role quorum — its only cardinality field is one document-wide `count` over a flat `members` list. Both candidate encodings change who must approve. |
| `R2_undefined_role` | an `approvers` form names a role the `roles` map does not define | Nothing to resolve the approver set from, and v2 removed the roles map. |
| `R3_team_member_ref` | a role member is an `@org/team` ref, or a role mixes users and teams | v2 `members` is a list of per-subject identifiers and `member_of` is a **single** group string; neither encodes a team-valued role. |
| `R4_reviewers_agent_count` | a stage declares the bare `reviewers.agent: N` | Each v2 `agents[]` entry **requires** a `provider` and a bare integer carries none. |
| `R5_duplicate_constraint_kind` | a `constraints` list declares the same kind twice | An object cannot hold a duplicate key, and dropping one would silently discard a declared constraint. |
| `R6_unknown_page_event` | a `must_page_human` token outside v2's enum after the `reviewer_reject` rewrite | No target token. |
| `R7_output_invalid` | the emitted document fails `ValidateBytes` | Fail closed: the schema errors are printed and nothing is written. |
| `R8_unknown_delegation_knob` | an `operator_agent` knob, condition value, or `model_policy` field v2 does not carry | A `when` the backend cannot answer makes the entry unreachable. |
| `R9_source_version_major_0` | the source declares version major 0 | See *Accepted input*. |

R6 and R8 are unreachable from a schema-valid v1 source (v1's enums are subsets of v2's, and its `operator_agent` is `additionalProperties: false`), and R7 is unreachable from a correct codemod. They are asserted at the function they guard rather than end to end — the guards have to hold if the schemas ever diverge.

## Output matrix

`stdout` is reserved for the **report** in every cell; the migrated YAML is never printed there, so piping the report somewhere can never accidentally produce a spec.

| Invocation | Effect |
|---|---|
| (no output flag) | Report to stdout, write **nothing**. The safe default for a command that rewrites a governance file. |
| `--report-only` | The explicit spelling of the above. |
| `--out PATH` | Write there; **refuse to clobber** an existing PATH (exit 1, path left byte-unchanged). |
| `--in-place` | Rewrite the source file. |
| `--out` **and** `--in-place` | Usage error (exit 2) — never a silent precedence rule. |
| `--report-only` with `--out` or `--in-place` | Usage error (exit 2) — contradictory. |

Exit codes: `0` migrated or already-v2 no-op; `1` refusal / output-validation failure / refused overwrite; `2` usage or I/O.

## Eligibility report

Every approval gate the document carries is reported — translated, already-`approvals` (reported unchanged), or blocked by a refusal. The eligible-principal set is split into two explicitly labelled parts, and a predicate this document cannot resolve is rendered **symbolically as itself**, never as an empty or fabricated set. An empty set for a gate governed only by `member_of` reads as "nobody can approve this" and invites exactly the wrong edit.

```
Approval-eligibility report
===========================

gate /workflows/feature_change/stages/0/gates/0 (stage: plan)
  before: approvers: {any_of: [founder]}
  after:  approvals: {count: 1, members: [kuhlman-labs]}
  eligible principals
    enumerable from this document:
      - kuhlman-labs
    not resolvable from this document:
      (none)
  change: WIDENED — the migrated gate declares no `not:` exclusion, because the legacy
  `approvers` form carries no source for one. …

gate /workflows/feature_change/stages/2/gates/0 (stage: review)
  before: approvals: {count: 2, min_permission: write, member_of: acme/reviewers}
  after:  approvals: {count: 2, min_permission: write, member_of: acme/reviewers}
  eligible principals
    enumerable from this document:
      (none)
    not resolvable from this document:
      - min_permission >= write   (forge-resolved at run time)
      - member_of = acme/reviewers   (forge-resolved at run time)
  change: none — the gate already declares an `approvals` block; it is carried through verbatim.
```

A gate with neither enumerable members nor unresolved predicates prints the explicit `(no approver constraint beyond count/not)` line.

## Limits

Since E52.13 / #2323 the codemod's `ValidateBytes` gate (R0 source, R7 output) resolves stage references — duplicate ids, the `needs:` shorthand, and `inputs[].from_stage` referents/ordering — in addition to schema, removed-form and reuse-resolution errors. So a **source** carrying a dangling `from_stage` is now refused at R0 rather than migrated (the correct answer — the backend would reject it at run creation anyway). What still surfaces server-side only is the stage-BINDING class (type/executor/constraint, produces-artifact, plan schema, `max_autonomy`). The codemod never rewrites input wiring, so it introduces no new referent — but server-side validation remains the authority for the binding rules, and the report says so.

## Golden fixture

`cli/internal/spec/testdata/migrate/fishhawk-own-spec.{v1,v2}.yaml` is a **frozen** snapshot of this repo's governing spec and its expected v2 output. It proves the codemod's behaviour on a realistic 360-line commented document; it is deliberately **not** tied byte-for-byte to the live `.fishhawk/workflows.yaml`, which the out-of-band operator migration will rewrite to v2. Regenerate it deliberately:

```sh
(cd cli && go test ./internal/spec -run TestMigrate_OwnSpecGolden -update-golden)
```

## Migrating this repo's own spec (operator walk)

Fishhawk's own `.fishhawk/workflows.yaml` still declares `version: "1.3"`. **No agent stage can migrate it**: the `feature_change` implement stage declares `forbidden_paths` including `".fishhawk/**"` (`.fishhawk/workflows.yaml:177-181`), so an implement commit touching that path fails the constraint — for any run on this workflow, not just this one. The migration is therefore an **operator base commit**, walked in this order:

1. **Dry-run the codemod.** `fishhawk migrate-spec .fishhawk/workflows.yaml --report-only` writes nothing (verify with `git status --porcelain`) and prints the eligibility report. Against the live v1.3 spec it reports that the document *migrates cleanly to workflow-v2*.
2. **Read the report's four WIDENED gates.** All four approval gates — `feature_change` plan, `feature_change` review, `human_led_change` review, and `release` deploy — are flagged **WIDENED**, because the legacy `approvers: {any_of: [founder]}` form carries no source for a `not:` exclusion (see [What is never fabricated](#what-is-never-fabricated)). The migrated `approvals: {count: 1, members: [kuhlman-labs]}` would let the run's resolved change author satisfy the gate and count toward its quorum — an author is resolved wherever the run carries a human-authorship signal (today an operator-vouched commit), so the widening is real on those runs and inert on the rest. The `agent` half widens nothing: that floor is unconditional (#2358). **Restore `not: [author, agent]` by hand on all four gates**, matching the shipped presets, before committing — which is why the report, not the migrated bytes, is the product.
3. **Leave `limit_usd` alone unless you want it.** No USD ceiling is fabricated for the three budget-bearing stages (plan, implement, acceptance), and no runtime check reads a stage budget on any major (**#2328** tracks stage-budget enforcement), so adding one is optional and advisory today.
4. **Hoisting `executor` is supported; hoisting `reviewers` is the other clean candidate.** A v2 stage may omit `executor` and inherit one from a `defaults` block or an `extends` base — the resolved document is what `$defs/stage`'s `required` list is enforced on, and `fishhawk doctor`'s *execution path configured* rung now **resolves reuse before checking** (#2340), so a hoisted executor no longer false-fails. `reviewers` is the other real de-duplication target: the `feature_change` **plan** and **implement** stages declare byte-identical `reviewers` blocks (the two heterogeneous-reviewer declarations at `.fishhawk/workflows.yaml:92` and `:158`), so a `defaults.reviewers` hoist is a safe use of [`defaults`](workflow-v2.md#reuse-defaults-and-extends). The three shipped presets nonetheless keep a **per-stage `executor`** on purpose — a bare `check-jsonschema` run resolves no reuse, and a governance template ships to be readable by that non-resolving reader too.
5. **Commit the migrated file to the BASE branch**, never through a run. Then expect a schema-major MCP reconnect on the first `scripts/dev up` / `reload` afterwards: a `version:` major bump fires the louder schema-major banner (**#1422**) because the live stdio `fishhawk-mcp` validates the spec on its own embedded schema.

## See also

- [`workflow-v2.md`](workflow-v2.md) — the complete standalone reference for the target grammar.
- [`workflow-v1.md`](workflow-v1.md) — the source grammar.
- `cli/README.md` — the CLI surface.
