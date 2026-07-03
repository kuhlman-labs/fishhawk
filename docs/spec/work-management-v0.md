# Work-management conventions `work-management-v0`

The contract for a repo's work-item filing conventions (#1005): the config that turns one `fishhawk_file_issue` call into a conventions-complete work item — title format, body skeleton, default labels, board placement, ADR numbering, epic linking. The conventions layer is the value; the provider API call is trivial.

This is a **new canonical artifact**, NOT a block inside `.fishhawk/workflows.yaml`. The workflow spec (`workflow-v0`) is frozen at Day 21 ("never break this schema in place; bump to a new spec version"), and the operator-role overlay (`.fishhawk/operator.yaml`) carries only an opaque `work_management` pointer at the config resolved here (ADR-040 D1).

- Canonical schema: [`work-management-v0.schema.json`](work-management-v0.schema.json) (JSON Schema Draft 2020-12, `$id` pins `work-management-v0`).
- Shipped default: [`work-management-default.yaml`](work-management-default.yaml) — a **product artifact**, versioned with the product, seeded from the `kuhlman-labs/fishhawk` Project #7 conventions.
- Go validation: `backend/internal/workmgmt` (embedded copies of the schema + default, mirrored by `scripts/sync-schemas`, locked by the schema-sync gate). `Default()` returns the shipped config, validated against its own schema at package init; `Parse` is the canonical enforcement point for a repo's config. The provider-agnostic canonical work-item model lives in the same package (`model.go`).

## Top-level fields

| Field | Required | Shape | Meaning |
|---|---|---|---|
| `spec_version` | yes | enum `work-management-v0` | Single-value enum per the versioning rules below. |
| `provider` | yes | enum `github_projects` \| `jira` | Work-management backend. `github_projects` is the only concrete provider in v0; `jira` is reserved at the interface level (no implementation) and an unimplemented provider must fail closed at filing time. |
| `project` | conditional | object | GitHub Projects connection (`owner`, `owner_type`, `number`). Required when `provider` is `github_projects` (semantic check). |
| `jira` | conditional | object | Jira connection (`project_key` + optional `issue_types` map). Required when `provider` is `jira` (semantic check). Selects only the target project — the instance base URL and credentials are server-side env (see below), **not** in this checked-in config. |
| `complexity_levels` | no | object: `low`/`medium`/`high` → prose | The complexity prior: concrete file/coupling definitions for each level. Optional in a repo config; shipped in the default. |
| `required_fields` | yes | non-empty unique string list | Fields every filed item must carry. Must include the mandatory trio Summary, Done-means, complexity (semantic check). |
| `field_hints` | no | object: field name → prose | Per-field authoring hints. The Done-means hint states the condition must be testable. |
| `types` | yes | object: type name → type config | Work-item types, keyed by snake_case name (bug, feature, chore, adr, epic, …). |
| `states` | no | object: canonical state → provider option | Canonical board-state map for run-lifecycle transitions (#1012). Keys from the closed set `backlog`/`in_progress`/`in_review`/`blocked`/`done`; values are provider option strings. |
| `transitions` | no | object: lifecycle event → canonical state | Run-lifecycle-event → canonical-state map (#1012). Keys from the closed set `run_started`/`pr_opened`/`run_failed`/`run_merged`; each value must be a key declared in `states` (semantic check). |
| `product_feedback` | no | object `{enabled: boolean}` | Per-repo kill-switch for upstream product-feedback egress (ADR-029, #1006). Absent means enabled (the default). `enabled: false` → `POST /v0/runs/{id}/product-reports` returns 403 `product_feedback_disabled` and files nothing. Set it as the object form (`product_feedback:` / `  enabled: false`), **not** a bare string. |

### Per-type fields (`types.<name>`)

| Field | Required | Shape | Meaning |
|---|---|---|---|
| `title_format` | no | template string | Title template with `{placeholder}` tokens (`{summary}`, `{epic}`, `{n}`, `{number}`). Rendering is the apply layer's concern. |
| `body_skeleton` | yes | non-empty string list | Ordered body section headings. Dual-audience: Feature = Summary/Proposal/Where to look/Done-means/Acceptance criteria/Notes/Relations; Bug = Summary/Observed/Proposal/Where to look/Done-means/Acceptance criteria/Notes/Relations; ADR = Context/Options/Recommendation/Decision/Consequences; Chore = Summary/Done-means. |
| `optional_sections` | no | unique string list | Subset of `body_skeleton` whose headings render only when the filing supplies content (see [Optional sections](#optional-sections)). Every entry must appear in `body_skeleton` (semantic check). |
| `default_labels` | no | unique label list | Labels applied before caller-supplied labels are merged. Each label is a bare token (`epic`, `adr`) or namespaced (`area:backend`, `type:feature`). |
| `default_fields` | no | object | `status` (single-select Status value), `board_column`, and `complexity` (low/medium/high). |
| `numbering` | conditional | object | `scheme` (`sequential`) + optional `prefix` + optional `pad` (zero-pad width for the rendered `{number}`, bounded 0..12; e.g. `3` → `041`, `0`/absent → no padding). Required when the type is `adr` (semantic check). The shipped default declares a second numbered type, `epic` (prefix `E`, `pad: 0` → the bare `[E29]`, not `[E029]`); its next number is discovered from existing `[E{number}]` titles, and the anchored discovery regexp skips child titles like `[E29.1]` because it demands `] ` immediately after the captured number. |
| `epic_link` | no | enum `required` \| `optional` \| `none` | Whether items of this type link to a parent epic. |

Every object level is `additionalProperties: false` — the surface is closed; new sections are additive schema changes within v0.

### Jira connection (`jira`)

The `jira` block is the connection for `provider: jira`. It carries **only non-secret project selection**:

| Field | Required | Shape | Meaning |
|---|---|---|---|
| `project_key` | yes | string | Jira project key (e.g. `FISH`) that filed issues are created under. |
| `issue_types` | no | object: canonical type → Jira issue-type name | Maps a canonical work-item type (`bug`, `feature`, …) to its Jira issue-type name (`Bug`, `Story`, …). An absent entry falls back to a title-cased default in the provider. |
| `parent_field` | no | string (default `parent`) | Field used to link a created issue to its parent epic, applied via a best-effort post-create edit. Team-managed (next-gen) projects use the default `parent` reference; company-managed (classic) projects set the instance's epic-link custom field id (e.g. `customfield_10014`) to the bare epic key. |

The Jira **instance base URL and credentials are server-side env**, never in this checked-in config: `FISHHAWKD_JIRA_BASE_URL`, `FISHHAWKD_JIRA_EMAIL`, `FISHHAWKD_JIRA_API_TOKEN`. This matches the `FISHHAWKD_PROJECTS_TOKEN` single-instance, secrets-never-in-repo precedent — the repo config selects only the project, the server holds the one instance and its creds. `provider: jira` still fails closed at filing time until the concrete provider and its server wiring land.

## Required-field discipline

The mandatory trio is **Summary**, **Done-means**, **complexity** (#1005, operator discussion 2026-06-11). The JSON Schema requires a non-empty `required_fields`; a semantic check (`workmgmt.Parse`) enforces that the trio is present, normalizing entries so `Done-means` and `Done means` both satisfy it. Everything else a type declares is optional.

- **Done-means** must be a *testable* condition — an observable outcome a reviewer can check, not a description of effort. The shipped default's `field_hints[Done-means]` states this.
- **complexity** is picked from `complexity_levels` by the files and coupling a change touches (low = a few tightly-scoped edits; medium = one module or cross-package seam; high = spans wire/domain/persistence or a migration, needs an integration test for the seam).

### Acceptance criteria vs Done-means

The `feature` and `bug` skeletons carry both a **Done-means** and an **Acceptance criteria** section (E34.7, #1614) — they answer different questions and are not interchangeable:

- **Done-means** is the change-complete checklist: what has to be true about the *change itself* (tests added, docs updated, mirrors synced) for the work to be considered finished. Example: "Filing a feature with an Acceptance criteria sections key succeeds; conventions docs updated; tests cover the old key set."
- **Acceptance criteria** is the behavioral contract: observable, falsifiable behaviors the *issue* is done when exhibiting, independent of how the change was implemented. Example: "A feature filed with sections keyed Acceptance criteria renders the section between Done-means and Notes."

`Acceptance criteria` is present on the `feature` and `bug` skeletons and optional on `chore` (the section is additive, not in `required_fields`, so it carries no retroactive requirement on existing issues filed under the old key set). It is the explicit/source_ref anchor the plan gate's `verification.acceptance_criteria` provenance expects; wiring a planner to read it automatically is out of scope here and tracked in #1543.

## Optional sections

A type's `optional_sections` is a subset of its `body_skeleton` whose headings render **only when the filing supplies content for them** (E34.8, #1615). This is the mechanism that lets a skeleton carry a section that not every filing fills, without polluting every item with an empty heading:

- **Absent** — the Sections map has no key for the section: the heading is skipped entirely (no `## Heading`, no trailing blank block). The assembled body is **byte-identical** to a skeleton that never listed the section, so adding an optional section to a type is a fully additive change.
- **Present-but-empty** — the Sections map has the key with an empty string value: the heading renders in position exactly as a mandatory section with no content does. A present key is content, even when empty.
- Sections **not** listed in `optional_sections` render unconditionally, as before.

The cross-reference is a semantic rule (`workmgmt.Parse`): **every `optional_sections` entry must name a section present in that type's `body_skeleton`**. An entry that names an off-skeleton heading is rejected fail-closed with a `*SemanticError` — the render skip would otherwise key on a heading `assembleBody` never emits. The schema declares the property (it is an additive-optional field within v0), but only this check ties an entry back to the skeleton.

### Where to look

The `feature` and `bug` skeletons carry an optional **Where to look** section (E34.8, #1615): non-binding starting pointers for the planner — files, symbols, precedent PRs/issues that ground where the change is likely to land. It sits immediately after `Proposal` (the issue's motivation is that such pointers currently blur into Proposal prose) and before `Done-means`.

**Where to look is explicitly NON-BINDING, and this is the load-bearing distinction from `scope.files`:**

- The **approved plan** owns the binding `scope.files` — the closed set of paths the implement stage may touch, enforced at commit time.
- A **Where to look** pointer neither folds into that scope nor obligates the plan to touch it. It grounds the planner; it does not constrain the plan. A path named here that the plan never touches is not a scope violation, and a plan may touch files no pointer named.

Give paths and names, never toolchain commands — the register is language-agnostic (a pointer is `dir/file.ext` or a symbol name, not `go test …`). Because it is optional, an item filed with no Where-to-look content renders byte-identically to the pre-section skeleton, so the section carries no retroactive requirement on existing issues.

## Board states and transitions

The `states` and `transitions` blocks (both optional, additive within v0) drive run-lifecycle board moves (#1012), the companion to the filing conventions above:

- **`states`** maps each canonical board state (the closed set `backlog`, `in_progress`, `in_review`, `blocked`, `done`) to the provider's column/option string — for GitHub Projects, the single-select Status option name (e.g. `in_progress → In Progress`). The small closed set is what keeps a future Jira provider tractable, since Jira transitions are workflow-gated.
- **`transitions`** maps each run-lifecycle event (the closed set `run_started`, `pr_opened`, `run_failed`, `run_merged`) to a canonical state. When a run reaches that lifecycle point, the board-sync hook moves the card to the mapped state — resolved to a provider option through `states`.

Both blocks are optional: absent (or empty) means no transitions are configured and the board-sync hook no-ops. A configured transition is best-effort and never blocks or fails the run, and the move fires only from the transition's expected source state — a card a human parked deliberately is never overridden.

The cross-reference is a semantic rule (`workmgmt.Parse`): **every `transitions` value must be a key present in `states`**. The schema constrains transition values to the canonical enum, but only the semantic check ties a value to a declared `states` key, so a transition can't target a state that has no provider option.

## Validation

`workmgmt.Parse` validates in two stages and returns a typed error:

- `*SchemaError` — a structural violation (unknown key, wrong enum, malformed label, empty `body_skeleton`). Carries a JSON Pointer path.
- `*SemanticError` — a cross-field rule the schema can't express: the mandatory trio is incomplete, `github_projects` is missing its `project` block, `jira` is missing its `jira` block, a type named `adr` has no `numbering` rule, a type's `optional_sections` names a heading absent from its `body_skeleton`, or a `transitions` value names a canonical state not declared in `states`.
- `*YAMLError` — unparseable, empty, or multi-document input (the config must be a single YAML document; a trailing document would bypass validation).

The shipped default is validated against the schema at backend package init, so the product artifact can never drift from its own schema.

## Versioning

- `spec_version` is a required, single-value enum (`work-management-v0`), matching the `version` / `plan_version` / `operator-role-v0` convention.
- The `$id` URL pins the version (`work-management-v0.schema.json`); the canonical filename matches.
- Additive optional fields are permitted within v0 and require validator-test updates. A breaking change bumps to `work-management-v1` in a new schema file; validators carry every version forever.

## See also

- `docs/spec/operator-role.md` — the `.fishhawk/operator.yaml` overlay carries the `work_management` pointer at this config.
- Parent epic #389; triggering issue #1005.
