# Change checklists

Read the section matching the planned change before finalizing scope.

## Schema change checklist

Before opening a PR that adds or modifies a field in `docs/spec/`:

1. **Additive or breaking?** New optional fields within the current major version (e.g.,
   `standard_v1.x`, `workflow-v0.x`) are additive — proceed. A new *required* field in an existing
   major version is a breaking change and must be avoided: bump the major version instead
   (`standard_v2`, `workflow-v1`). Use the `x-intended-required` annotation (see below) to signal a
   field whose required promotion is deferred through a soak period.

2. **Soak period.** When a field is introduced as optional but is intended to become required in a
   future version, declare the soak window in the PR body under `## Notes`. The soak period duration
   is determined per-PR; no minimum is set yet (TBD in a follow-up issue). During the soak, both the
   old and new schema versions are validated by the backend.

3. **Version advertising.** After the schema change merges, update every surface that advertises supported schema versions:
   - Plan validator (`backend/internal/plan/`) — add the new version string to the recognized set.
   - Inspect the live schema-advertising surfaces; backend `/healthz` advertises workflow schema hashes. Do not create a speculative runner endpoint as part of a schema edit.
   - **A NEW workflow MAJOR** (ADR-046 / #1381 stood up `workflow-v1` as the keystone): add the
     canonical `docs/spec/workflow-vN.schema.json`, run `scripts/sync-schemas` to mirror it into
     `backend/internal/spec/schemas/` + `cli/internal/spec/schemas/` (the `workflow-v*` glob routes
     it — no script edit), append a `{Major: N, Path: …}` entry to the `embeddedSchemas` routing
     table in BOTH `backend/internal/spec/parse.go` and `cli/internal/spec/spec.go` (this is what
     makes the version-routed validator dispatch to the new major instead of failing closed), add an
     `EmbeddedSchemaHashVN()` + the `workflow-vN` `/healthz` `schemas`-map entry in
     `backend/internal/server/handlers.go`, and register the new mirror set as its own
     self-referential surface-sweep pattern in `backend/internal/server/surface_sweep.go`.

4. **`x-intended-required` annotation.** When a field is optional now but will become required in the next major version, annotate it in the JSON Schema:
   ```json
   "some_field": {
     "type": "string",
     "x-intended-required": true
   }
   ```
   JSON Schema Draft 2020-12 §10.3 collects unknown keywords as annotations without affecting
validation, so this annotation is safe to add to any schema without breaking existing validators.

## Auth change checklist

Before opening a PR that adds or tightens handler-side authorization (scope check, role check, audience check), the PR body must include:

1. **Impact inventory.** Which active tokens would lose access if the change shipped as-is? Run
   `fishhawkd token migrate --db $FISHHAWKD_DATABASE_URL` (dry-run, no `--apply`) to enumerate
   affected tokens. Paste the summary line into `## Notes`.

2. **Migration path.** State how affected tokens will be brought up to the new requirements. Options in order of preference:
   - `fishhawkd token migrate --apply` — promotes tokens whose scope set is a strict subset of the operator default (see #529).
   - Re-issue the token with `fishhawkd token issue --subject <s>`.
   - Manual DB update (last resort; document the exact SQL).

3. **Safe-to-ship determination.** The PR is safe to ship without operator-side action only when the
   impact inventory is empty (step 1 produced `scanned=N migrated=0`). If the inventory is
   non-empty, the migration path must be completed before or immediately after deploy, and this must
   be stated explicitly.

Cross-reference: this checklist codifies the rollout discipline introduced by #529; see #472 for the analogous schema-change discipline.

## Audit-category change checklist

A **new audit category** — any string literal reaching `audit.Entry` / `ChainAppendParams` /
`AppendChained` / an emit helper — obliges `backend/internal/audit/categories.go` in the SAME plan
scope. The committed-tree verify gate's `TestKnownCategoriesCoversEmittedCategories` fails on an
unregistered emitted category, and an implement agent whose scope omits the registry cannot reach
the file (run 49f642b8 / #3410 discovered the obligation only by failing that gate and burning a
scope amendment on it).

1. **Scope the registry.** Add `backend/internal/audit/categories.go` (modify) to `scope.files` alongside the emitter — a one-line registration.
2. **The plan gate flags the omission.** The plan-gate surface sweep's audit-category rule
   (`backend/internal/server/audit_category_sweep.go`) reads the plan's own prose for a snake_case
   token named as an audit category/entry/row/event, drops the ones `audit.IsKnownCategory` already
   registers, and — when the plan touches `backend/` without the registry — records a `new audit
   category requires registry` finding rendered to the plan reviewer as `NEW AUDIT CATEGORY`. It is
   exemptible via `surface_sweep_exemptions` `{pattern: "new audit category requires registry",
   sibling: "backend/internal/audit/categories.go"}` ONLY when the token is not in fact a category
   (a runner log event, a tool name); an exemption is recorded and reviewer-challengeable.
3. **Operator-visible categories also oblige `backend/internal/issuecomment`'s
   `activityCategories`** (#3392/#3406) — the activity-comment renderer's allow-list — so the entry
   surfaces on the issue thread; and any new issue-comment surface still obliges
   `docs/issue-comment-surfaces.md` (the path-triggered "audit kind requires surfaces doc" pattern).
4. **In a fix loop, do not retry.** When the verify output names the registry (`add them to
   backend/internal/audit/categories.go`), the fix prompt lists it under `OUT-OF-SCOPE PATHS` with
   the mid-stage scope-amendment recipe — file the amendment; an in-scope workaround is the #1170
   silent wrong-fix class.


## Adding a Go module

Use the `new-go-module` skill. Register the module in `go.work`, copy its go.mod
(and go.sum when present) into the backend Dockerfile before `go mod download`,
copy its sources in the build phase, update Dependabot and the architecture
pointer. Validate build/tests/lint for the module. `go.work` is committed;
`go.work.sum` is not. Module paths are `github.com/kuhlman-labs/fishhawk/<module>`.

## Review prompts and evaluation

Changes to review prompts, repodoc, personaprompt, review remits/conventions,
the catch-rate instrument or fixtures require `scripts/check-review-prompt-eval`.
It runs outside `scripts/test verify` and fails closed on absent, stale,
under-powered or regressed committed arm-keyed evidence. A changed instrument
needs an operator re-recording; runner agents cannot record model-key-dependent
evidence. An ADR record can stale the architect arm because its prompt consumes
the decision index. Read [the evidence procedure](../compliance/planreview-catchrate-evidence.md)
and [agenteval](../../backend/internal/agenteval/README.md) before changing these
surfaces; do not treat an offline green as a live behavioral measurement.

Changing untrusted-intake framing in `backend/internal/prompt/` also requires the
injection and envelope-quality corpora (#2291). Offline containment runs in
verify; live behavioral resistance and quality arms are opt-in, gated by the
live-eval flags and model key. The issue-body, verify-output, crew-message,
review-convention and user-report framing each have harness-side byte-exact
copies that must move together. Never add a production envelope off-switch to
make the no-envelope arm. See [injection evidence](../compliance/prompt-injection-evidence.md)
for measured/unmeasured status and [agenteval](../../backend/internal/agenteval/README.md)
for the real rendering matrix, flags and framing-copy locations.
