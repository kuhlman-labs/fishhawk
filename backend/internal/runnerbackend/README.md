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

`TriggerParams` is the forge-neutral carrier: `RunID`, `StageID`, `WorkflowID`, `StageExecutorRef`, `Repo` (`"owner/name"`), `Scope` (a `forge.CredentialScope`; `Scope.IsZero()` = unwired, the direct analogue of the pre-#1861 `InstallationID == 0` sentinel — the last cross-forge `int64` seam, flipped in E45.8 / #2013), `Ref` (the run's ADR-035 sole-writer branch the trigger targets), and decomposition provenance (`DecomposedFrom`, `SliceIndex`).

### The run-branch ref (`Ref`)

`orchestrator.triggerParams` resolves `Ref` once from the run-branch lineage (`runBranchRef`): a decomposed child executes on its per-slice branch `fishhawk/run-<shortParent>/slice-<n>` (`sliceBranch`), every other run on its own `fishhawk/run-<short>` namespace (`runBranchPrefix`). The `github_actions` backend **ignores** `Ref` (its workflow file lives on the default branch, so it always dispatches on `DefaultRef`/`main`); the `gitlab_ci` backend **creates its pipeline against** `Ref`. This is the seam the whole dispatch design turns on — the ref derived here threads through `TriggerStage` to the `CreatePipeline` request unmodified.

## Implementations

- **`GitHubActions`** (`githubactions.go`) — fires a GitHub Actions `workflow_dispatch` via a narrow `DispatchClient` (just `DispatchWorkflow`, sliced from `*githubclient.Client`). `HostDispatched()` is false. `TriggerStage` reproduces the former `orchestrator.fireDispatch` byte-for-byte: warn+nil skip when the client is nil or `Scope.IsZero()`, `parseRepo`, ref default `main` / actions-file default `fishhawk.yml`, the decomposed-child `slice_index`/`decomposed_from` info log, and the `run_id`/`stage_id`/`workflow_id`/`stage` inputs plus `parent_run_id` iff the run is a decomposed child (#1227). Ignores `p.Ref`.
- **`Local`** (`local.go`) — `HostDispatched()` is true (drives the `awaiting_host_dispatch` park). `TriggerStage` is the warn+no-op preserving `fireDispatch`'s defensive locked-local skip; in normal flow the park branch returns before it is reached.
- **`GitLabCI`** (`gitlabci.go`, #1861) — creates a GitLab CI/CD pipeline against `p.Ref` via a narrow `PipelineTrigger` interface (just `TriggerPipeline`), satisfied by `*gitlab.Forge` and resolved through `forge.Get("gitlab")` by `GitLabPipelineTrigger()` (nil-safe when GitLab is unconfigured — so no authenticated client is wired into the dispatch path). `HostDispatched()` is false. `TriggerStage` is **dispatch-only**: it warn+skips (fires NO pipeline) when `Trigger == nil` or `Scope.IsZero()`, else creates the pipeline carrying `run_id`/`stage_id`/`workflow_id`/`stage` (+ `parent_run_id` for a decomposed child) as CI/CD variables — and **NEVER writes a commit status**. Status publishing for a `gitlab_ci` run is exclusively `auditcheckpublisher`'s (mirroring the GitHub check-run split), which is why `PipelineTrigger` carries no status method — the backend structurally cannot write one. **DORMANT**: no `gitlab_ci` run is created until go-live enablement (#2043), so this backend fires only under unit/wire tests today.

## Registry + KindHostDispatched

`Registry` maps `runner_kind → Backend`. Dispatch sites (orchestrator, webhook) build a default registry from their own GitHub client / ref / actions-file / logger fields and route through it.

`KindHostDispatched(kind) (hostDispatched, known bool)` is the package-level predicate over the KNOWN kinds, for guard sites that hold only a `runner_kind` string (no client wiring): `local → (true, true)`, `github_actions → (false, true)`, `gitlab_ci → (false, true)`, anything else → `(false, false)`. The two-value shape lets the two guard sites keep their **opposite** unknown-kind postures explicit:

- `server/host_dispatch.go` admits a host spawn only when the resolved kind is `known && hostDispatched` → **rejects** unknown resolved kinds AND known non-host kinds (`github_actions`, `gitlab_ci`) with 409 `dispatch_not_admissible`.
- `cmd/fishhawk-mcp/host_dispatch_guard.go` blocks only when the locked kind is `known && !hostDispatched` → **blocks** `github_actions`/`gitlab_ci` (a host/local dispatch conflicts with their CI channel) but **allows** unknown locked kinds.

`gitlab_ci` is a KNOWN non-host-dispatched kind (#1861): fishhawkd fires its pipeline trigger, so both guard sites treat a `gitlab_ci`-locked run the same way they treat `github_actions`. A future registry addition cannot silently flip either site; one test per site pins the branch.

## Resolver — run-lineage semantics (ported verbatim)

`Resolver.Resolve(ctx, r) Backend` ports the former `orchestrator.runLockedLocal` decision:

- **Resolved lock is authoritative**: `r.RunnerKind` → its registry backend (a resolved `gitlab_ci` run now routes to the `GitLabCI` backend). An **unknown** resolved kind — one not in the registry — falls to the trigger (github_actions) backend, today's fire-through.
- **Un-resolved top-level run** → github_actions (legacy first-dispatch auto-resolve, #1346 decision-1).
- **Un-resolved decomposed child** (`DecomposedFrom != nil`, minted `runner_kind`-UNRESOLVED with the parent's kind copied):
  - inherited kind **not** local → github_actions (fires unchanged);
  - inherited **local** kind → consult the parent via `GetRun`:
    - parent resolved local → **local** backend (park);
    - parent resolved non-local → github_actions (the inherited hint was superseded);
    - **parent read error OR parent unresolved → local** backend (park), keeping the two structured WARN logs byte-identical. This is the **#1980 fail-toward-recoverable rule**: `awaiting_host_dispatch` is CAS-recoverable with one host-dispatch verb, whereas a wrongly-fired `workflow_dispatch` is an unrecoverable external side effect (#1355).

`dispatchStage` resolves once and keys the park on `HostDispatched()`; the webhook CI-retry path does a plain registry lookup on the child's inherited kind (NOT the lineage resolver — a fresh local retry child must stay pending, not auto-resolve to github_actions).

## gitlab_ci: landed as dormant plumbing (E45.8 / #1861)

The `gitlab_ci` backend is the second non-host implementation this seam existed to admit. It landed as **additive, dormant** plumbing: the backend, the `Scope`/`Ref` `TriggerParams` flip, the `KindHostDispatched` classification, and the registry entries all ship, but **no `gitlab_ci` run is ever created** — go-live enablement is carved to #2043. Every new path is exercised only by unit/wire tests.

Companion surfaces (routed, not abstracted here):

- **check-run status publishing** (`auditcheckpublisher`) — E45.8 gave it its first `runner_kind` guard: a `gitlab_ci` run publishes a GitLab commit status (via the GitLab forge `CreateCheckRun`), `github_actions`/`local` keep the GitHub check-run path. This is the **dispatch-only division**: `GitLabCI.TriggerStage` NEVER writes a status; `auditcheckpublisher` owns it exclusively (mirroring how the GitHub backend never publishes its own check run).
- **`workflow_run`/`check_run` ingest matching** — Actions-specific by construction; the GitLab analogue is out of scope here.
