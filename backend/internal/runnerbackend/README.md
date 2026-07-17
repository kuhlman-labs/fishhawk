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

`TriggerParams` is the forge-neutral carrier: `RunID`, `StageID`, `WorkflowID`, `StageExecutorRef`, `Repo` (`"owner/name"`), `Scope` (a `forge.CredentialScope`; `Scope.IsZero()` = unwired), `Ref` (the dispatch ref — see below), and decomposition provenance (`DecomposedFrom`, `SliceIndex`).

**`Scope` (was `InstallationID int64`, #1861).** When the `gitlab_ci` backend became the field's second consumer, the cross-forge `installationID int64` seam flipped to a `forge.CredentialScope` — the same field carries a GitHub installation id (a decimal `"<id>"` ref) or a `"gitlab:<project_id>"` ref. `Scope.IsZero()` is the direct analogue of the pre-flip `InstallationID == 0` sentinel, so each backend warn+skips on a zero scope exactly as it did on the zero id. This removed the last sanctioned `installationID int64` entry from `forge/credential_scope_gate_test.go`.

**`Ref` (the run-branch dispatch ref, #1861).** GitLab's pipelines API requires a `ref` that selects BOTH which `.gitlab-ci.yml` is evaluated AND which commit the pipeline (and its status) runs against. The orchestrator resolves it ONCE for a `gitlab_ci` run from the **run-branch derivation**:

- a **top-level** run's branch is `fishhawk/run-<short>` (`runBranchPrefix`);
- a **decomposed child**'s per-slice branch nests under the parent's namespace: `fishhawk/run-<short(parent)>/slice-<n>` — byte-identical to the runner's sole-writer slice branch.

The `github_actions`/`local` paths leave `Ref` empty; `GitHubActions.TriggerStage` falls back to `DefaultRef` then `main`, so the field is forge-neutral and the legacy "dispatch against main" behavior is preserved exactly. The **first** GitLab dispatch (webhook run creation) targets the default branch — where `.gitlab-ci.yml` lives — because the run branch does not exist yet; the runner creates it, and subsequent-stage (orchestrator) dispatches target the run branch itself.

## Implementations

- **`GitHubActions`** (`githubactions.go`) — fires a GitHub Actions `workflow_dispatch` via a narrow `DispatchClient` (just `DispatchWorkflow`, sliced from `*githubclient.Client`). `HostDispatched()` is false. `TriggerStage` reproduces the former `orchestrator.fireDispatch` byte-for-byte: warn+nil skip when the client is nil or `Scope.IsZero()`, `parseRepo`, ref = `p.Ref` falling back to `DefaultRef` then `main`, actions-file default `fishhawk.yml`, the decomposed-child `slice_index`/`decomposed_from` info log, and the `run_id`/`stage_id`/`workflow_id`/`stage` inputs plus `parent_run_id` iff the run is a decomposed child (#1227).
- **`GitLabCI`** (`gitlabci.go`, #1861) — the second dispatch backend. Creates a GitLab pipeline via a narrow `PipelineTriggerClient` (`CreatePipeline`, sliced from `*gitlabclient.Client`) against `p.Ref`, passing `run_id`/`stage_id`/`workflow_id`/`stage` (+ `parent_run_id` for a decomposed child) as CI/CD variables. `HostDispatched()` is **false** — fishhawkd fires the pipeline itself, so a `gitlab_ci` stage dispatches rather than parks. `TriggerStage` is **DISPATCH ONLY**: it writes NO commit status — the fishhawk-gate status is published EXCLUSIVELY by `auditcheckpublisher` as the run progresses, mirroring the GitHub division (dispatch fires the CI, the publisher posts the gate). Warn+skips on a nil client or `Scope.IsZero()`; hard-errors on a bad `gitlab:<project_id>` scope or an empty `Ref`.
- **`Local`** (`local.go`) — `HostDispatched()` is true (drives the `awaiting_host_dispatch` park). `TriggerStage` is the warn+no-op preserving `fireDispatch`'s defensive locked-local skip; in normal flow the park branch returns before it is reached.

## Registry + KindHostDispatched

`Registry` maps `runner_kind → Backend`. Dispatch sites (orchestrator, webhook) build a default registry from their own GitHub client / ref / actions-file / logger fields and route through it.

`KindHostDispatched(kind) (hostDispatched, known bool)` is the package-level predicate over the KNOWN kinds, for guard sites that hold only a `runner_kind` string (no client wiring): `local → (true, true)`, `github_actions → (false, true)`, anything else (including `gitlab_ci`, not yet recognized here) → `(false, false)`. The two-value shape lets the two guard sites keep their **opposite** unknown-kind postures explicit:

- `server/host_dispatch.go` admits a host spawn only when the resolved kind is `known && hostDispatched` → **rejects** unknown resolved kinds (409 `dispatch_not_admissible`).
- `cmd/fishhawk-mcp/host_dispatch_guard.go` blocks only when the locked kind is `known && !hostDispatched` → **allows** unknown locked kinds.

`gitlab_ci` is intentionally left UNKNOWN to this predicate in this slice: the two guard sites decide their own gitlab_ci posture, so promoting it to a known kind is owned by the slice that updates those guards (and their tests). This seam only supplies the `gitlab_ci` `Backend` + `Registry` entry. A future registry addition therefore cannot silently flip either site; one test per site pins the unknown-kind branch.

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

## gitlab_ci (child .8 / #1861 — landed)

The `gitlab_ci` backend (`GitLabCI`, above) is the second implementation this seam was built to admit. It is registered in each dispatch site's default `Registry` (orchestrator + webhook). `KindHostDispatched` does NOT yet recognize `gitlab_ci` (see above) — that flip rides with the guard-site slice. Its landing is what shaped the `TriggerParams.InstallationID → Scope` flip and the forge-neutral `Ref`.

**Known wiring gap (follow-up).** A `gitlab_ci` run does not yet persist its credential scope (`"gitlab:<project_id>"`) on the run row — the run carries only the GitHub `installation_id int64` (the ADR-057 `installation_ref` column is a follow-up). So **orchestrator-driven subsequent-stage** `gitlab_ci` dispatch warn+skips (zero scope) until that lands; the **first-stage** dispatch works because the webhook dispatcher has the scope directly from the trigger event's `CredentialRef`.

## Companion surfaces (now owned by the sibling slices)

The E45.7 issue's scope prose said the github_actions impl "wraps DispatchWorkflow **+ check-run status + workflow_run/check_run ingest**". This seam scopes the interface to the **dispatch** concern only. The companion surfaces are owned elsewhere:

- **status publishing** (`auditcheckpublisher`) — #1861 slice 2 adds its first `runner_kind` guard: a `gitlab_ci` run publishes a GitLab commit status instead of a GitHub check run. Dispatch (this package) writes NO status — the division mirrors GitHub's (dispatch fires the CI, the publisher posts the gate).
- **inbound CI ingest** — the GitHub `workflow_run`/`check_run` path and the GitLab `pipeline`/`build` path each classify a failed CI run into a CI-failure retry; the GitLab side is consumed server-side (`server/webhook_gitlab.go`) and routes through `Dispatcher.HandleGitLabCIFailure`.
