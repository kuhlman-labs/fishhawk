# backend/internal/runnerbackend

Forge-neutral **dispatcher seam** (E45.7, ADR-058 / #1851, ADR-022 growth path). It replaces the four scattered `runner_kind` comparisons that decided "park for a host spawn vs fire a `github_actions` workflow_dispatch" with a single `Backend` interface plus a `Resolver` that owns the run-lineage semantics those sites shared. Purely internal refactor — zero behavior change, mirroring the E45.9 CredentialScope expand-phase (#2014).

## The Backend interface

```go
type Backend interface {
	Kind() string
	HostDispatched() bool
	TriggerStage(ctx context.Context, p TriggerParams) error
}
```

- **`Kind()`** — the `runner_kind` string the backend serves.
- **`HostDispatched()`** — whether the backend is spawned host-side rather than by fishhawkd. The local runner is host-spawned (ADR-024), so its agent stages **park** at `awaiting_host_dispatch` (#1912) instead of being fired here; the host-dispatch marker endpoint (or an MCP spawn verb calling it) flips `awaiting_host_dispatch → dispatched` at the moment of the spawn.
- **`TriggerStage`** — wakes the runner for the stage in `p`. For a host-dispatched backend this is a defensive warn+no-op (the host spawn is the real trigger).

`TriggerParams` is the forge-neutral carrier: `RunID`, `StageID`, `WorkflowID`, `StageExecutorRef`, `Repo` (`"owner/name"`), `InstallationID` (`0` = unwired), and decomposition provenance (`DecomposedFrom`, `SliceIndex`).

## Implementations

- **`GitHubActions`** (`githubactions.go`) — fires a GitHub Actions `workflow_dispatch` via a narrow `DispatchClient` (just `DispatchWorkflowScoped`, sliced from `*githubclient.Client`). `HostDispatched()` is false. `TriggerStage` reproduces the former `orchestrator.fireDispatch` byte-for-byte: warn+nil skip when the client is nil or `InstallationID == 0`, `parseRepo`, ref default `main` / actions-file default `fishhawk.yml`, the decomposed-child `slice_index`/`decomposed_from` info log, and the `run_id`/`stage_id`/`workflow_id`/`stage` inputs plus `parent_run_id` iff the run is a decomposed child (#1227).
- **`Local`** (`local.go`) — `HostDispatched()` is true (drives the `awaiting_host_dispatch` park). `TriggerStage` is the warn+no-op preserving `fireDispatch`'s defensive locked-local skip; in normal flow the park branch returns before it is reached.

## Registry + KindHostDispatched

`Registry` maps `runner_kind → Backend`. Dispatch sites (orchestrator, webhook) build a default registry from their own GitHub client / ref / actions-file / logger fields and route through it.

`KindHostDispatched(kind) (hostDispatched, known bool)` is the package-level predicate over the two KNOWN kinds, for guard sites that hold only a `runner_kind` string (no client wiring): `local → (true, true)`, `github_actions → (false, true)`, anything else → `(false, false)`. The two-value shape lets the two guard sites keep their **opposite** unknown-kind postures explicit:

- `server/host_dispatch.go` admits a host spawn only when the resolved kind is `known && hostDispatched` → **rejects** unknown resolved kinds (409 `dispatch_not_admissible`).
- `cmd/fishhawk-mcp/host_dispatch_guard.go` blocks only when the locked kind is `known && !hostDispatched` → **allows** unknown locked kinds.

A future registry addition therefore cannot silently flip either site; one test per site pins the unknown-kind branch.

## Resolver — run-lineage semantics (ported verbatim)

`Resolver.Resolve(ctx, r) Backend` ports the former `orchestrator.runLockedLocal` decision:

- **Resolved lock is authoritative**: `r.RunnerKind` → its registry backend. An **unknown** resolved kind falls to the trigger (github_actions) backend — today's fire-through.
- **Un-resolved top-level run** → github_actions (legacy first-dispatch auto-resolve, #1346 decision-1).
- **Un-resolved decomposed child** (`DecomposedFrom != nil`, minted `runner_kind`-UNRESOLVED with the parent's kind copied):
  - inherited kind **not** local → github_actions (fires unchanged);
  - inherited **local** kind → consult the parent via `GetRun`:
    - parent resolved local → **local** backend (park);
    - parent resolved non-local → github_actions (the inherited hint was superseded);
    - **parent read error OR parent unresolved → local** backend (park), keeping the two structured WARN logs byte-identical. This is the **#1980 fail-toward-recoverable rule**: `awaiting_host_dispatch` is CAS-recoverable with one host-dispatch verb, whereas a wrongly-fired `workflow_dispatch` is an unrecoverable external side effect (#1355).

`dispatchStage` resolves once and keys the park on `HostDispatched()`; the webhook CI-retry path does a plain registry lookup on the child's inherited kind (NOT the lineage resolver — a fresh local retry child must stay pending, not auto-resolve to github_actions).

## Extension point: gitlab_ci (child .8 / #1861)

The `gitlab_ci` backend is the second implementation this seam exists to admit. Adding it means: a new `Backend` impl, registering it in each site's default `Registry`, and extending `KindHostDispatched` if the new kind is host-dispatched.

## Deliberately-excluded companion surfaces (deferred to child .8)

The E45.7 issue's scope prose said the github_actions impl "wraps DispatchWorkflow **+ check-run status + workflow_run/check_run ingest**". This seam scopes the interface to the **dispatch** concern only — where the scattered `runner_kind` guards actually lived. Two companion surfaces stay in place as the github_actions backend's documented neighbors, deferred to child .8 (#1861, whose body was amended 2026-07-16 to own both):

- **check-run status publishing** (`auditcheckpublisher`) — gated only on GitHub wiring; **no `runner_kind` guard exists there today**.
- **`workflow_run`/`check_run` ingest matching** — Actions-specific by construction.

Neither carries a `runner_kind` guard to replace, so abstracting them now would be single-implementation speculation with behavior-change risk and nothing to replace. The `gitlab_ci` implementation is what should shape those interface members when a second implementation exists.
