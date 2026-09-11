# backend/internal/devfixtures

Named fixture scenarios a seeded acceptance preview materializes into its
target database (E72.2 / #3326, folding in #1874). A scenario is an
embedded YAML document declaring runs, stages, artifacts, approvals and
audit rows by scenario-local HANDLE; `Apply` writes them through the
product's own repositories so every invariant the product enforces
(transition tables, the per-run audit hash chain, foreign keys) holds
for seeded rows exactly as it does for live ones. Consumed by the
dev-only `GET/POST /v0/dev/fixtures` routes (registered only under
`FISHHAWKD_DEV_FIXTURES=1` with a database, loopback-only) and by
`scripts/dev preview <sha> --seed <name>`.

## Layout

| Path | Owns |
|---|---|
| `catalog/catalog.go` | The NAME SET: `Names()`, `Known(name)`, `Description(name)`. Stdlib-only leaf so `backend/internal/plan` (a pure classifier) can ask "is this token a known scenario?" without importing the DB-bearing package. |
| `devfixtures.go` | The FORMAT: `Scenario` types, `//go:embed scenarios/*.yaml`, `Load` / `Parse` / `Validate`, `EmbeddedNames`. |
| `scenarios/*.yaml` | The shipped scenarios. Adding a file here is what adds a scenario; the two-way test refuses one the catalog does not name (and vice versa). |
| `apply.go` | MATERIALIZATION: the four narrow store interfaces, `Deps`, `Apply`, `Result`, and the name-keyed `Applier`. |

## Catalog binding

`catalog.Names()` and the embedded file set are pinned to each other in
BOTH directions by `TestNames_MatchesEmbeddedScenarioSet`: a name in the
catalog without a `scenarios/<name>.yaml` fails, and a YAML without a
catalog entry fails. `Load(name)` consults `catalog.Known` first and
wraps `ErrUnknownScenario` naming the known set for anything else — the
404 body the dev route renders comes from that message. The catalog is a
map so a name cannot be added without its one-line description.

Shipped scenarios:

| Name | Shape |
|---|---|
| `grooming-confirm-gate` | `backlog_grooming` run, running. `groom` (plan, agent) succeeded with a `grooming_report` artifact and one approval; `confirm` (review, human, requires_approval) parked at `awaiting_approval`. NO implement stage — this is the exact stage set the `human_review_gate_parked` classifier reads, so a criterion can assert it from `GET /v0/runs/{id}/stages`. |
| `plan-gate-parked` | `feature_change` run, running. `plan` (plan, agent, requires_approval) at `awaiting_approval` carrying a valid `standard_v1` plan artifact; no approvals. |
| `trace-upload-target` | `feature_change` run, running. `plan` (plan, agent) at `dispatched` — ready for `POST /v0/runs/{id}/trace` — plus FOUR `cost_recorded` audit rows aged `1h5m` / `2h` / `3h` / `4h` (≈$0.001 each, model `claude-opus-4-8`) as a spend baseline. |

## Format

Top-level keys: `runs`, `stages`, `artifacts`, `approvals`, `audit`.
Unknown YAML keys are refused (`KnownFields`). Handles (`key`) are
scenario-local; nothing in a document is an id.

- `runs[]`: `key`, `repo`, `workflow_id`, `workflow_spec` (INLINE
  workflow document text, parsed with the `spec` package at validation;
  must declare `workflow_id`), `trigger_ref`, `state`
  (`pending|running|succeeded|failed|cancelled`).
- `stages[]`: `key`, `run` (run handle), `sequence`, `type`
  (`plan|implement|review|deploy|acceptance`), `executor_kind`
  (`agent|human`), `executor_ref`, `requires_approval`, `state`, and for
  `failed` a `failure_category` (A–D) plus optional `failure_reason`.
  `state` must be one the canonical walk can reach:
  `pending → dispatched → running → {succeeded | awaiting_approval | failed}`.
  Any other stage state (`awaiting_input`, `cancelled`, …) is refused
  because `Apply` could not materialize it.
- `artifacts[]`: `key`, `stage` (stage handle), `kind`
  (`plan|pull_request|deployment|acceptance|release_notes|grooming_report`),
  `schema_version`, `content` (inline JSON text; must parse).
- `approvals[]`: `stage`, `approver_subject`, `decision`
  (`approve|reject`), `comment`, `surface` (`api|ui|cli|github_comment|github_reply_comment`).
- `audit[]`: `run`, optional `stage`, `category` (must pass
  `audit.IsKnownCategory`), `actor_kind` (`agent|user|system`),
  `actor_subject`, `payload` (inline JSON text), optional `age`.

`Validate` fails closed on every shape above and names the offending
handle (or index); `TestValidate_Refusals` carries one row per branch.

### The `age` field

`age` is an OPTIONAL Go duration string (`"1h5m"`). `Apply` subtracts it
from its clock, so the row's `Timestamp` is `Now − age`; an absent `age`
dates the row at `Now`. This is what lets `trace-upload-target` seed a
spend baseline in PRIOR hour buckets: `spendalert.Evaluate` averages the
populated prior buckets inside its 24h window, so a ~$5 upload in the
seed hour OR the hour after trips `spend_alert` against the ≈$0.001
baseline. The `1h5m` row is what keeps the youngest populated prior
bucket at seed-hour minus one (`TestScenario_TraceUploadTarget_BaselineSurvivesHourBoundary`
asserts exactly that at pinned clocks); without it the youngest is
seed-hour minus two. Seed and upload contiguously — the baseline ages
with wall-clock time. Backdated timestamps do not disturb the audit
chain: `AppendChained` links by sequence, not by timestamp, and
`TestApply_TraceUploadTarget_StageDispatchedWithBackdatedSpendBaseline`
recomputes every stored hash to prove it.

## Apply contract

```go
deps := devfixtures.Deps{Runs: runRepo, Artifacts: artifactRepo, Audit: auditRepo, Approvals: approvalRepo, Now: time.Now}
res, err := devfixtures.NewApplier(deps).Apply(ctx, "trace-upload-target")
// res.Runs["target-run"].ID, res.Runs["target-run"].Stages["plan"]
```

- **Narrow stores.** `RunStore` (`CreateRun`, `TransitionRun`,
  `CreateStage`, `TransitionStage`), `ArtifactStore` (`Create`),
  `AuditStore` (`AppendChained`) and `ApprovalStore` (`Submit`) name only
  the methods `Apply` calls. The real repositories satisfy them unchanged
  (`apply_pg_test.go`); a fake-driven test arms each call to fail and
  asserts the error names the handle (`TestApply_ErrorBranches`), which
  is what keeps every error branch covered without Postgres.
- **Order.** Runs first (`CreateRun` → the run walk: `pending` writes
  nothing, every other declared state is reached THROUGH `running`), then
  stages in `sequence` order (stable across declaration order) —
  `CreateStage` then the canonical `TransitionStage` walk, with a
  `StageCompletion` carrying the declared category/reason on the `failed`
  step only — then artifacts (`ContentHash` = sha256 hex over the
  declared content bytes, the product's own convention; the JSONB column
  re-serializes on read), approvals, and audit rows (stage handle
  resolved to the minted id, `Timestamp = Now − age`).
- **Untenanted.** Runs are created with no account, `TriggerSource`
  `github_issue`, the inline spec bytes on the row and `WorkflowSHA` =
  sha256 of the spec; `enforceAccount` admits an anonymous caller, which
  is what the credential-less acceptance agent (ADR-050) needs.
- **Fresh rows every call.** Nothing is looked up or deduplicated;
  `Result{Scenario, Runs map[handle]RunResult{ID, Stages map[handle]uuid}}`
  maps every handle to the ids THIS call minted (`TestApply_FreshRowsPerCall`).
- **Fail closed, no rollback.** A nil store, a nil scenario or a
  scenario `Validate` refuses is rejected before any write. A store error
  mid-way returns an error naming the handle and leaves the rows written
  so far in place — the preview database is disposable, so partial state
  is reported, not unwound.
- **Clock.** `Deps.Now` is the ONE clock; nil means `time.Now`. Tests
  inject a pinned clock and assert exact backdates
  (`TestApply_BackdatesAuditRowsByAge`).

## Tests

| File | Needs | Pins |
|---|---|---|
| `devfixtures_test.go` | nothing | every `Validate` refusal, the two-way catalog binding, unknown-name `Load`, and the hour-boundary baseline property at pinned clocks. |
| `apply_test.go` | nothing (fakes embedding `run.BaseFake` / `audit.BaseFake`) | exact `TransitionRun` / `TransitionStage` sequences per declared state, the failed-step completion, sequence-order create, run/artifact/approval/audit params, `Now − age` backdating, every error branch, the `Applier` wrapper. |
| `apply_pg_test.go` | `pgtest.NewPool` | committed-state read-backs of all three scenarios through the real repositories (chain links + recomputed hashes for the baseline rows) and fresh rows per call. |
