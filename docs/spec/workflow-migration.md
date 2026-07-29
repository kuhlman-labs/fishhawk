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
| 2 | **No-op**, exit 0. Nothing to migrate is not an error; the report is still printed. |
| absent / ≥ 3 | Refused as R0 — the source-validation gate reports the schema's own error. |

## Translation table

| v0/v1 form | workflow-v2 form | Notes |
|---|---|---|
| `version: "1.x"` | `version: "2"` | v2 has no minor chain; its enum rejects the dotted forms. |
| workflow `drive: true` | `auto_advance: true` | Renamed in place — the key's leading comment and position survive. |
| stage `budget.max_runtime_minutes: 15` | `budget.max_runtime: 15m` | Same value, spelled as the Go duration string the other three v2 duration fields use. |
| stage `budget.max_tokens: N` | carried forward unchanged | v2 keeps it as an optional secondary lever. |
| — | `budget.limit_usd` | **Never fabricated.** No token-to-USD rate exists anywhere in this repo. The report notes per stage that a USD ceiling was not derivable and should be added by hand. |
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

The translated matrix is compared against the three `autonomy` tier expansions. `autonomy: <tier>` is emitted **only on an exact match** across all five action classes AND the page list (compared as a set). Any partial match emits the explicit `actions` matrix and no `autonomy` key: rounding a `may_approve`-only block up to `autonomy: medium` would hand the operator agent fixup and retry authority the source never granted.

- An **absent knob** is omitted entirely — absence already resolves to the fail-closed `mode: gated`.
- An **absent `operator_agent` block** emits **neither** key. It is deliberately not rounded to `autonomy: low`, which would additionally assert an empty `page_human_on` list the source never declared.
- A `route_fixup_min_severity` or a `model_policy` blocks the tier collapse — no tier expands to either.

## What is never fabricated

`limit_usd`, `min_permission`, `member_of` and `not:` are never invented. The legacy grammar carries no source for any of them.

The `not:` case is the one with teeth. The shipped presets carry `approvals: {count: 1, not: [author, agent]}`; a gate migrated out of `approvers` emits no exclusion, so relative to that default the migration **widens** eligibility — the change's own author can satisfy the migrated gate. The codemod reports this per gate rather than adding the exclusion silently, because adding it would be exactly the guess the refusal taxonomy exists to avoid.

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

`cli/internal/spec` performs no typed decode and no graph-shape pass, so the codemod's `ValidateBytes` gate catches schema, removed-form and reuse-resolution errors but **not** `needs` / `from_stage` referent errors, which surface server-side at run creation. The codemod never rewrites input wiring, so it introduces no new referent — but server-side validation remains the authority, and the report says so.

## Golden fixture

`cli/internal/spec/testdata/migrate/fishhawk-own-spec.{v1,v2}.yaml` is a **frozen** snapshot of this repo's governing spec and its expected v2 output. It proves the codemod's behaviour on a realistic 360-line commented document; it is deliberately **not** tied byte-for-byte to the live `.fishhawk/workflows.yaml`, which the out-of-band operator migration will rewrite to v2. Regenerate it deliberately:

```sh
(cd cli && go test ./internal/spec -run TestMigrate_OwnSpecGolden -update-golden)
```

## See also

- [`workflow-v2.md`](workflow-v2.md) — the v2 grammar, per-E52-child.
- [`workflow-v1.md`](workflow-v1.md) — the source grammar.
- `cli/README.md` — the CLI surface.
