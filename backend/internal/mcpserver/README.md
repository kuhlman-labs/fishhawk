# mcpserver

The Fishhawk MCP tool library — the `fishhawk_*` tool registry, its onboarding
resources, and the API client they call. Extracted verbatim from the
`fishhawk-mcp` command (E66.7 / [#2408](https://github.com/kuhlman-labs/fishhawk/issues/2408), [ADR-076](https://github.com/kuhlman-labs/fishhawk/issues/2388)) so `fishhawkd` can serve the identical
registry over its own `/mcp` route ([#2390](https://github.com/kuhlman-labs/fishhawk/issues/2390)) without re-implementing every tool. That route has
since LANDED: `backend/cmd/fishhawkd` wires `NewServer` into the route's
`server.MCPServerFactory` seam, which calls it once per `/mcp` request bound to
that request's bearer — making fishhawkd the SECOND live consumer of this
package alongside the stdio binary. The seam exists because
`backend/internal/server` must NOT import this package: these in-package tests
drive a real `server.New` (`campaign_test.go`), so that edge would close an
import cycle in THIS package's test binary. The
[`fishhawk-mcp` binary](../../cmd/fishhawk-mcp/README.md) keeps its import path and build target; it now imports this
package and calls `NewServer` to construct the same fully-registered server it
built inline before. This is a behaviour-preserving relocation: no route, no
new dependency, and the handshake identity, tool surface, onboarding
instructions, and embedded runbook resource are byte-identical before and after.

## Entry points

Three exported identifiers are the intended surface (fishhawkd's `/mcp` route
consumes only the first two):

- **`Config{BackendURL, APIToken}`** — the construction input: the backend base
  URL every tool calls and the bearer token the API client authenticates with.
  Exactly those two exported fields. Kept separate from the package-private
  `config` (which the moved tool files and their test literals reference) via
  an unexported `Config.internal()` adapter, so the move left those files
  byte-identical apart from their package clause.
- **`NewServer(Config) *mcp.Server`** — builds a fully-registered server: the
  empty shell (`buildServer`), every `fishhawk_*` tool (`registerTools`), and
  the onboarding runbook resource (`registerOnboardingResources`). The single
  cross-package construction path both live consumers use: the stdio binary
  (once at startup) and fishhawkd's `/mcp` route (once per request, via the
  injected `server.MCPServerFactory`, #2390).
- **`Instructions`** — the server `instructions` string advertised on the MCP
  `initialize` handshake, the public alias of the package-private
  `onboardingInstructions`.

## Exported surface: why 234 identifiers, not 3

The package presents **234** exported top-level identifiers, but only the three
above are intended entry points. The other 231 are the tool I/O
request/response structs. The MCP SDK's jsonschema reflection requires each
tool's input/output type — and its exported fields — to build the tool's
schema, so **unexporting them would break tool registration**. In `package
main` their exportedness was cosmetic; the move to a library package makes it
real. They are deliberately NOT unexported (that refusal is correct — see
#2408); instead `export_surface_test.go` pins the full sorted surface against a
baseline generated from the tree, so a NEW export is caught in either direction
while the pre-existing 231 are grandfathered.

## Tool reference and internals

The sections below are the full per-tool reference, the onboarding contract,
the operator-surface playbooks, and the package internals — moved verbatim from
the binary README with the code.

## In-band onboarding (server `instructions` + `fishhawk://runbook`, [#1356](https://github.com/kuhlman-labs/fishhawk/issues/1356))

A connecting client whose agent holds no operator memory gets enough to drive a run without a CLI alt-tab, delivered over the protocol itself:

- **Non-empty server `instructions`** — returned on every MCP `initialize`. A concise happy-path verb sequence (`fishhawk_start_run` → `fishhawk_run_stage` plan → `fishhawk_approve_plan` → `fishhawk_dispatch_stage` implement → `fishhawk_await_review` → the acceptance stage when declared → approve PR → merge → post-merge) plus the gate semantics that decide when each verb is legal (don't approve before plan review clears, wait for all configured reviewers, operator-gated scope amendments, a failed acceptance verdict leaves the stage succeeded and routes through deterministic triage, `next_actions` is authoritative). Kept deliberately short; the long form lives in the runbook resource it points at.
- **`fishhawk://runbook` resource** — a listable/readable `text/markdown` resource carrying the full loop-driving procedure (the ADR-040 operator-role contract) and the edge-case playbook: `runner_kind:local` for the local dogfood loop, failed-run revive (`fishhawk_revive_run`, incl. the re-parked-acceptance pre-dispatch check), the decomposed-parent native path (`fishhawk_run_children` → `fishhawk_consolidate_slices`, never `fishhawk_dispatch_stage` on an `awaiting_children` parent), the `fishhawk_drive_run` loop shape (gate-ordered dispatch, delegated plan approval, `awaiting_host_dispatch` auto-dispatch, and its `decision_required`/`paged`/`dispatched_stale` stops), the acceptance stage (E31.9 — advisory runner-hosted validator against a preview you provision; verdict-vs-stage-state; deterministic triage table; the local-runner explicit-re-dispatch rule; paged arbitration), local-drive fixup requiring an explicit `fishhawk_dispatch_stage` to spawn the runner, the scope-amendment decide/naming flow, heterogeneous-review two-verdict waits, and post-failure clean-tree discipline.

Both register in the single shared `newServer` construction path (`onboarding.go`, content in `runbook.md`), so they are **transport-neutral** — identical over stdio and streamable-HTTP, and they carry into the #655 gateway unchanged.

Implementation: `onboardingInstructions` is wired into `buildServer`'s `mcp.ServerOptions{Instructions: …}` so it is
returned verbatim on every `initialize`; `registerOnboardingResources(srv)` adds the readable `fishhawk://runbook`
`text/markdown` resource. The in-memory round-trip in `onboarding_test.go` and the HTTP-session assertion in
`http_transport_test.go` pin both seams. This is the in-band counterpart to #996 Themes 2/3 (the thin operator agent +
onboarding-as-data).

## Onboarding tools (`fishhawk_doctor` / `fishhawk_init`, [#1506](https://github.com/kuhlman-labs/fishhawk/issues/1506))

Two thin tools (E29.6) wrap the E29 onboarding engine so a connecting Claude Code agent can drive a conversational "help me onboard a repo" flow — **one engine, another frontend** (the CLI `fishhawk doctor` / `fishhawk init` and the App-PR path are the other frontends). Both live in `onboard.go`.

- **`fishhawk_doctor`** (read-only) wraps `GET /v0/onboarding/readiness` (E29.4 / [#1511](https://github.com/kuhlman-labs/fishhawk/issues/1511)) and returns the `report` — the four server-side-only readiness checks a repo's first `feature_change` run needs, which the agent cannot introspect locally:
  - `app` — `{installed, installation_id?, reason?}`: is the GitHub App installed on the target repo.
  - `spec` — `{source, valid, error?, note?}`: the committed `.fishhawk/workflows.yaml` fetch + parse + validate state (`source` is `fetched` or `unavailable`; `valid` is only meaningful when fetched). Only checked once the app is installed.
  - `reviewers[]` — `{provider, model?, reasoning_effort?, available, missing_hint?}`: per spec-declared reviewer availability on **this** deployment, with the adapter's missing-env-var hint when a provider can't be resolved. Empty when the spec is unavailable or invalid.
  - `scopes` — `{adequate, required[], missing[], note?}`: whether the caller token holds the run-driving scope subset. A cookie-session caller bypasses scope enforcement and is adequate by construction.

  `repo` falls back to `GITHUB_REPOSITORY` when omitted (a fast local fail when neither is present, before the HTTP hop). The endpoint gates on **authentication only** (401 anonymous) — scope adequacy is itself a reported field, so a scope-gapped token still gets a report naming its gap rather than a 403. Backend 4xx map onto clean tool errors: `authentication_required` (401, with a `FISHHAWK_API_TOKEN` pointer) and `validation_failed` (400, malformed repo).

- **`fishhawk_init`** generates the starter spec **in-process** via `backend/internal/spec.PresetBytes(preset)` — there is **no HTTP generation endpoint** (spec generation is CLI-local `spec.Generate`), and the `fishhawk-mcp` binary is built from the backend module (ADR-021) so it may import `backend/internal/spec` directly (it already does for spec parsing). Returns `{preset, workflow_yaml, target_path}` where `preset` is `low` / `medium` / `high` (default `medium`), `workflow_yaml` is the canonical workflow-v2 preset bytes, and `target_path` is `.fishhawk/workflows.yaml`. An unknown preset fails closed with a clean error naming the valid tiers.

  **Preset-only scoping:** `fishhawk_init` returns the scaffold bytes for the conversational agent to **write** — it writes no file itself. The delta options (`--budget-usd` / `--single-reviewer` / `--human-gates`) and the AGENTS.md/CLAUDE.md bridge (E29.2) the CLI `fishhawk init` performs are a **follow-up**, because the delta-applying `Generate` lives only in `cli/internal/spec` and porting it into the backend module is beyond a thin tool.

Wiring: the `OnboardingReadinessReport` wire mirror (+ nested `OnboardingApp`/`OnboardingSpec`/`OnboardingReviewer`/`OnboardingScopes`)
lives in `client.go` — all scalar/string/slice fields, so it is #371-safe. Both tools register in `tools.go`, bumping the
house-style tool-count guard to 39; tests live in `onboard_test.go` (the low `client_test.go` stem-sibling needs no new
coverage). The planned `.claude/skills/onboarding/SKILL.md` conversational-entry seed is DEFERRED — `.claude/` is
gitignored repo-wide, so the skill file cannot be committed; the onboarding frontend ships in full via the two tools
regardless, and the skill is a follow-up if the repo later tracks `.claude/`.

## Stage-execution wait contract ([ADR-037](https://github.com/kuhlman-labs/fishhawk/issues/879), #880)

The durable `(run_id, stage_id)` handle is the unit of waiting on a stage's execution. `fishhawk_get_run_status` carries `plan_stage_wait_status` + `implement_stage_wait_status` — each a `StageWaitStatus` whose `status` is one of `pending`/`running`/`succeeded`/`failed`/`cancelled`, derived from the stage row (distinct from the `*_review_status` pair, which tracks a stage's **review** rather than its execution).

### Two stage vocabularies, one documented mapping ([#2494](https://github.com/kuhlman-labs/fishhawk/issues/2494))

The five-value `stage_wait_status.status` is a coarser **BUCKET** than the raw stage `state` the REST API reports — the thirteen `run.StageState` values (`GET /v0/runs/{run_id}` `stages[].state`, `docs/api/v0.openapi.yaml`). They are deliberately different — the bucket answers *"should I keep polling?"*, the raw state answers *"where exactly is this stage?"* — and neither is renamed, because renaming either is a breaking wire change for every existing consumer. The mapping is **total** and is written down once, in `stage_wait.go`'s `StageWaitStatus` doc comment:

| backend stage `state` | `stage_wait_status.status` |
| --- | --- |
| `pending` | `pending` |
| `awaiting_host_dispatch` | `pending` |
| `dispatched` | `pending` |
| `awaiting_approval` | `pending` |
| `awaiting_children` | `pending` |
| `awaiting_input` | `pending` |
| `awaiting_scope_decision` | `pending` |
| `awaiting_deploy_approval` | `pending` |
| `awaiting_deployment` | `pending` |
| `running` | `running` |
| `succeeded` | `succeeded` |
| `failed` | `failed` |
| `cancelled` | `cancelled` |

The table enumerates every state that exists today, but the mapping is a **rule**, not an enumeration: `running` maps to `running`, the three terminal states map to themselves, and every other state — including one added after this table was written — falls to `pending`, the conservative keep-polling default, so a new backend state never strands a caller on a bucket it does not recognize. `stage_wait_test.go`'s `TestClassifyStageWaitStatus_MappingTable` pins the table against the `run.StageState` constants themselves (a renamed state breaks the build, a re-bucketed one fails the test) and `tools_test.go`'s `TestToolDescriptions_CursorAndStageVocabulary` asserts every pair is on the wire description. `docs/api/v0.openapi.yaml` and `docs/api/v0.md` carry the reciprocal pointer back here.

### `ReviewStatus.reviews[]` has ONE shape ([#2494](https://github.com/kuhlman-labs/fishhawk/issues/2494))

`ReviewStatus.reviews` is **always present** and always a non-nil array: it renders as `[]` on `none`/`pending` rather than being absent. The prior `omitempty` made a consumer handle two shapes — a missing key and an array — for the same field, so a naive `len(reviews)` read against a `none`/`pending` status hit a missing key instead of an empty list. This is additive for any consumer that already tolerates an empty array; a consumer distinguishing *absent* from *empty* sees a behavior change. (`AwaitReviewOutput.reviews` is a separate response type and keeps its `omitempty`.)

- **Poll the handle (primary, authoritative).** Re-polling `fishhawk_get_run_status` is the blessed way to await a stage's terminal status. While the status is non-terminal (`pending`/`running`) the `StageWaitStatus` carries a server-suggested `poll_interval_seconds`, **derived** (E48.62 / [#2489](https://github.com/kuhlman-labs/fishhawk/issues/2489)) rather than flat: a quarter of the stage's **REMAINING** predicted runtime when the run carries an approved plan's `predicted_runtime_minutes`, otherwise a quarter of its **ELAPSED** runtime, clamped to a **30s floor** and a **900s ceiling**. Re-call on that cadence until the status goes terminal. Both branches land on the floor by construction for every degenerate input (no prediction and no elapsed, a never-started stage, clock skew, and an OVERRUNNING stage whose remaining has gone negative — the last deliberately, since a stage past its prediction is the one worth polling tightly), so a run with neither prediction nor elapsed advertises exactly the pre-#2489 30s. The plan stage always takes the elapsed branch (no plan exists while it runs), which is what stops a long plan stage sitting at 30s. The interval is dropped once the run itself is terminal (ADR-036 [#874](https://github.com/kuhlman-labs/fishhawk/issues/874) backstop), so a stage that can no longer progress never advertises an unbounded poll.
  - **Remaining-budget observability ([#2540](https://github.com/kuhlman-labs/fishhawk/issues/2540)).** A **non-terminal** `StageWaitStatus` also carries `elapsed_seconds`, `agent_timeout_seconds` (the spec-resolved agent wall clock the runner enforces, decoded from the stage read's `agent_timeout_seconds`) and `deadline_seconds_remaining` (`agent_timeout_seconds - elapsed_seconds`, clamped at 0 so an overrunning stage reports 0 rather than a negative), populated on exactly the statuses `poll_interval_seconds` is. `deadline_seconds_remaining` is a **pointer** (`nil` = the budget was unresolved; `0` = the deadline is reached), and all three are omitted when the budget is unknown (`agent_timeout_seconds <= 0` — a legacy row, an absent/unparseable spec, or an older backend that omits the field) or once the stage is terminal. They let an operator see how much runtime is left: a mid-stage scope amendment needs at least the **~15-minute poll window** of remaining budget to be decidable before the runner kills the stage — the same window the server surfaces OBSERVABILITY-ONLY on a request response (`amendment_poll_window_seconds`, `AmendmentPollWindowSeconds`). It is a display figure, never a refusal (#2540 approval condition 1). The window figure here (~15 minutes) must stay equal to `amendmentPollWindowMinutes` and the server's `AmendmentPollWindowSeconds`; the copies are pinned equal by `stage_wait_test.go`'s cross-package equality test.
  - **Mid-execution progress heartbeat ([#2541](https://github.com/kuhlman-labs/fishhawk/issues/2541)).** A **non-terminal** `StageWaitStatus` also carries `last_event`, `turns_this_attempt` and `tokens_this_attempt` — the runner's most recent `stage_progress` heartbeat, projected off the stage read's `progress` field (a `StageProgress` wire mirror; the json tags MUST byte-match the backend or the #371-class trap decodes them to nil and the operator sees one bit again). Populated ONLY while non-terminal and ONLY when the stage has reported a heartbeat, so a stage that has reported nothing keeps the byte-identical prior shape. The counters are cumulative **within the current agent attempt** and **reset on an in-driver re-spawn** (the observed 9, 9, 5 turn series) — the per-attempt meaning is encoded in the field names. `elapsed_seconds` is NOT taken from the heartbeat: it stays the server-side `started_at` derivation, which makes it cumulative and monotonic across re-spawns, and its population condition widens to `agent_timeout_seconds > 0 OR progress present` so a stage with progress but an unresolved budget still reports elapsed. The heartbeat POST (`POST /v0/runs/{run_id}/stages/{stage_id}/progress`) is fail-open on the runner side and can never fail a stage.
- **Synchronous-with-progress `fishhawk_run_stage` (negotiated fallback).** The synchronous call runs the stage to completion and returns the terminal outcome (also surfacing `stage_wait_status` on the handle — normally already terminal, so the interval is omitted). It is the fallback for clients that prefer to block or for short stages; it is not the primary mechanism. Its run-terminal result also carries the `next_actions` block (#1024) so the operator gets the legal next move directly — see [Server-suggested next actions](#server-suggested-next-actions-next_actions-1024).
- **Non-blocking `fishhawk_dispatch_stage` ([#1232](https://github.com/kuhlman-labs/fishhawk/issues/1232)).** The SDK-independent dispatch verb spawns the runner **detached** and returns the `(run_id, stage_id)` handle plus a non-terminal `stage_wait_status` **immediately**, so a **single** MCP session can poll `fishhawk_get_run_status` to terminal AND decide a mid-stage scope amendment in-band between polls (`fishhawk_decide_scope_amendment`) — the durable fix for the [#1189](https://github.com/kuhlman-labs/fishhawk/issues/1189) amendment timeout. It ships the poll-to-terminal UX today and **superseded the interim `fishhawk run auto-decide` second channel** ([#1233](https://github.com/kuhlman-labs/fishhawk/issues/1233)/[#1234](https://github.com/kuhlman-labs/fishhawk/issues/1234)) for in-band mid-stage amendment decisions, since removed ([#1554](https://github.com/kuhlman-labs/fishhawk/issues/1554)). See [Non-blocking dispatch](#non-blocking-dispatch-fishhawk_dispatch_stage-1232) below.
- **Terminal wait `fishhawk_await_stage` ([#2491](https://github.com/kuhlman-labs/fishhawk/issues/2491)).** The single terminal wait for the durable handle `fishhawk_dispatch_stage` returns — dispatch previously handed back a `log_path` and nothing to await, forcing a hand-rolled poll loop. It **reuses the [#1252](https://github.com/kuhlman-labs/fishhawk/issues/1252) `?wait` long-poll** (`GET /v0/runs/{run_id}/stages/{stage_id}?wait=<n>` → the `{state, terminal, failure_*}` settledness envelope) as a new api-client read (`apiClient.GetRunStageWait` decoding a purpose-built `RunStageWait` projection — NOT an embed of `Stage`, which would shadow-collide on the `state` json tag and hide the top-level `terminal`), so it adds **no** backend, orchestration, or runner surface. Contract:
  - **Settled, not just terminal.** The wait resolves the moment the stage reaches a state where `run.StageState.IsSettled()` is true — terminal (`succeeded`/`failed`/`cancelled`) OR parked-for-operator (`awaiting_approval` / `awaiting_children` / `awaiting_input` / `awaiting_scope_decision` / `awaiting_deploy_approval` / `awaiting_host_dispatch`). That is the settledness a detached watcher wants (release the moment the stage needs attention). The response carries the **RAW** stage state, never coerced, so a parked `awaiting_approval` is distinguishable from a `succeeded` stage; keying on terminality alone would silently miss the parked half.
  - **Shared timeout ladder.** It reuses `effectiveAwaitCap` / `clampAwaitTimeoutHeartbeat` (no new ladder): default 360s, cap 600s, raised to 7200s via `long_wait:true` or a client-supplied `progressToken` (the reachable knob is `long_wait`, per #2490); per-tick `notifications/progress` keep-alives fire only when a `progressToken` is present, and a failed `NotifyProgress` is swallowed.
  - **Run-terminal backstop (ADR-036).** Before the loop and on each still-unsettled tick it `GetRun`s; if the run is terminal it takes ONE final stage read (a settlement landing at/after the run transition still wins as `settled`) and otherwise resolves `run_terminal` rather than holding the session to the deadline. A `GetRun` failure is best-effort — the poll/timeout path stays in charge.
  - **Per-call `?wait` clamp (15s).** Each poll iteration issues `?wait=15`, clamped in `GetRunStageWait` to `awaitStagePerCallWaitSeconds`. That is below BOTH the backend's `maxRunStageWaitSeconds=30` cap AND — the binding constraint — the apiClient short client's **30s** timeout: a per-call `?wait` of 30 would let a held long-poll race the client deadline and surface as a transport error, so 15 leaves a full 15s of headroom.
  - **Pending scope amendment releases the wait ([#2588](https://github.com/kuhlman-labs/fishhawk/issues/2588)).** The wait has a **second release condition** alongside settledness. Filing a mid-stage scope amendment does **not** park the stage — it stays `running` — so a settledness-only wait never released, and the documented in-band amendment channel silently required hand-polling: the operator saw nothing until the agent's ~15-minute poll window had elapsed and the request had expired undecided. On the **fast path** (before the first tick — the shape an operator re-arming after a timeout hits) and on **every poll tick**, the verb probes the existing `GET /v0/runs/{run_id}/scope-amendments` read (`apiClient.ListScopeAmendments` — no new REST surface) and resolves `amendment_pending`, carrying the whole `ScopeAmendmentItem` (paths, reason, `requested_at`, #983 cap headroom) plus a pre-filled `fishhawk_decide_scope_amendment` `next_step`, so the operator decides in one hop with no second lookup. Details:
    - **Strict `(status == "pending", stage_id == awaited stage)` predicate.** A decided amendment is not actionable and must not release a re-armed wait; a sibling stage's request must not release this one. The backend's amendment row carries a non-pointer, non-`omitempty` `stage_id`, so equality is a complete predicate rather than one that silently drops rows.
    - **Settledness wins — enforced, not asserted.** The settled check runs first on both paths, AND — because the stage read and the amendment-list read are not atomic — after the probe finds a pending amendment the release **re-reads the stage** (`wait=0`) and prefers `settled` if it has since settled. Without that re-check a stage that settled while the list was in flight would surface as `amendment_pending`; the consequence is benign and self-correcting (the operator re-arms and gets `settled`), but a stated invariant the code does not hold is exactly the defect class this wave has been removing. The extra read is paid only on a tick that actually finds a pending amendment. If the re-read itself fails, the amendment release stands — a pending amendment is known to exist, settledness merely could not be confirmed.
    - **Best-effort probe.** Any `ListScopeAmendments` error returns nil and the normal poll/timeout path stays in charge; the wait NEVER fails on this probe. The deliberate cost: a persistently failing scope-amendments endpoint degrades this to pre-#2588 behaviour **silently** (the wait keeps working, the amendment signal is lost). An hours-long wait dying on a transient list error is the worse failure.
    - **`Terminal` stays honest.** On `amendment_pending` it is `false` and `state` carries the raw `running` state: the stage genuinely has not settled, and the flag is not borrowed to mean "the wait released".
    - **Cost:** one extra short GET per poll tick, alongside the tick's existing `?wait=15` stage read and ADR-036 `GetRun` — roughly one more request per 15s per armed wait.
    - **Re-arming without deciding spins.** A wait re-armed against an UNDECIDED amendment returns `amendment_pending` again immediately. Deliberate — the request is still waiting — and the message plus `next_step` name the exact decide call.
    - **Why the state machine was left alone.** Parking the stage at `awaiting_scope_decision` (the issue's proposal 1) was rejected: there is no `awaiting_scope_decision → succeeded` edge, and [#2548](https://github.com/kuhlman-labs/fishhawk/issues/2548) gave that state a decomposition-parent meaning (`orchestrator.go` reads implement + `awaiting_scope_decision` as "the agent has exited, an operator must decide"). The wait releases; the state does not change.
  - `fishhawk_dispatch_stage` returns a typed `next_step` `SuggestedAction` pointing at this verb with the resolved `(run_id, stage_id, stage)` pre-filled (nil on the `needs_target` pre-spawn refusal, since no runner was spawned). Its `reason` names both release conditions, so the verb no longer recommends a follow-up call blind to the amendment channel it exists to enable (#2588).
- **Native MCP Tasks — deferred.** A future mode that lets `fishhawk_run_stage` return a handle immediately and poll to terminal is **not built** here: Tasks now lives in the standalone, experimental [`io.modelcontextprotocol/tasks`](https://github.com/modelcontextprotocol/ext-tasks) extension (SEP-2663), outside the core spec, and is unimplemented in the pinned go-sdk ([go-sdk#626](https://github.com/modelcontextprotocol/go-sdk/issues/626)). It would layer onto the same `(run_id, stage_id)` handle that `fishhawk_dispatch_stage` already returns.

## `working_dir` resolution — transport-conditional ([#2479](https://github.com/kuhlman-labs/fishhawk/issues/2479))

The four runner-spawning verbs — `fishhawk_dispatch_stage`, `fishhawk_run_stage`, `fishhawk_run_children`, `fishhawk_drive_run` — resolve an omitted `working_dir` through one shared helper (`runResolver.resolveWorkingDir`) whose behaviour is keyed on the **transport**, not the binary:

- **HTTP transport (`Config.HTTPTransport: true`)** — fishhawkd's `/mcp` route (ADR-076 / [#2390](https://github.com/kuhlman-labs/fishhawk/issues/2390)) and `fishhawk-mcp --transport http`. `working_dir` is **required and must be absolute**. An omitted or relative value is **refused** with an actionable error naming the field, because the serving process's cwd is a long-lived daemon's checkout, not the caller's — silently defaulting to it would drive the run against the fishhawkd host's tree ([#1866](https://github.com/kuhlman-labs/fishhawk/issues/1866) is the discipline this closes by construction). A relative path is refused too: it resolves against the identical daemon cwd, so accepting it would leave the hole half-open.
- **stdio transport (`Config.HTTPTransport: false`)** — the client-spawned `fishhawk-mcp` process, whose cwd **is** the caller's own project directory. An omitted `working_dir` resolves to that directory as an **absolute** path (`os.Getwd`), and a relative value is made absolute (`filepath.Abs`) — the runner receives a concrete directory, never the literal `"."` it shipped before this change.

This divergence is a **deliberate, documented decision**: the same registry code refuses over HTTP and defaults over stdio because the meaning of the serving process's cwd differs between the two postures. Each of the four verbs echoes the resolved absolute directory back as `resolved_working_dir` on its output, so a wrong checkout is visible in the tool result rather than discovered from a contaminated diff. Callers already passing an absolute `working_dir` (the standing #1866 operator discipline) are behaviourally unaffected apart from the new echo field. The refusal is anchored in state, not just an error string: a refused call records no host-dispatch marker, spawns no runner, and (for `run_children`) provisions no per-child worktree.

### `working_dir` is bound once at start_run and inherited ([E66.42 / #2482](https://github.com/kuhlman-labs/fishhawk/issues/2482))

`fishhawk_start_run` binds `working_dir` onto the run row (a `runs.working_dir` column, migration 0065, echoed as `working_dir` on the Run) so the four runner-spawning verbs inherit it instead of re-typing the path on every call. Those verbs resolve through `runResolver.resolveWorkingDirForRun(ctx, runUUID, in)` — a run-aware wrapper over `resolveWorkingDir` (left unchanged, so the two #2479 refusals stay independently deletable for the counterfactual pass). The precedence ladder:

1. **Explicit non-empty input** — validated by `resolveWorkingDir` exactly as #2479 does, then **refused when it differs from the run's non-empty binding after `filepath.Clean` on both sides** (the #1866 contamination shape; the run branch lineage is anchored to the original tree). The refusal names both paths, both remedies, and — because comparison does **not** resolve symlinks — states that so the macOS `/tmp` → `/private/tmp` case is diagnosable from the error text alone.
2. **Empty input with a non-empty binding** — the binding is fed through the SAME `resolveWorkingDir` gate, so a relative or empty binding is refused identically to an explicit one; inheritance cannot bypass the gate.
3. **Empty input AND empty binding** — **refused when the run is a decomposition child (`decomposed_from != nil`) whose `runner_kind` is `local`** (E48.100 / #2547, below); otherwise falls through to `resolveWorkingDir("")`, which refuses over HTTP (#2479) and resolves the process cwd on stdio.

**Decomposition children inherit the binding at mint (E48.100 / [#2547](https://github.com/kuhlman-labs/fishhawk/issues/2547)).** The orchestrator's fan-out mint copies the parent's `working_dir` onto every child row (as it already copies `runner_kind` and the cached workflow spec), so a child's stage verbs resolve the checkout through rung 2 with no per-call repetition — a child executes against the same checkout its parent's branch lineage is anchored to. The CI-failure-retry mint (`webhook/dispatcher.go`) and the operator-recovery mint (`server/recover.go`) inherit it for the same reason.

**Rung 3's refusal is transport-INDEPENDENT, and narrow.** Unlike #2479's HTTP-only admission, the refusal for an unbound local decomposition child fires on stdio too: the stdio cwd fall-through — resolving a child against whatever directory the client process happens to sit in (#1866) — is precisely the hole being closed, so refusing only over HTTP would leave it open where it actually bites. It is scoped to decomposition children rather than every unbound local run because a top-level run started without a binding has no parent to inherit from and the cwd default is its documented behaviour; a child that reaches rung 3 is a row minted before this change, and its correct checkout is knowable (the parent's) rather than guessable. Ordering is load-bearing: the refusal sits AFTER the explicit-input rung, so an operator can still drive a legacy null-bound child by passing `working_dir` explicitly, and AFTER the inherit rung, so a bound child is never refused.

**Residual coverage gap, stated plainly:** the refusal keys on `runner_kind == "local"`, which a child inherits verbatim from its parent at mint. An empty `runner_kind` resolves to `github_actions`, which has no local checkout, so refusing there would be wrong. The residual is therefore a **legacy row that is genuinely local but carries an empty `runner_kind`**: that child still takes the old cwd fall-through and is NOT covered by this refusal.

**Fail-closed on an unreadable run over HTTP (Condition 1 / #2482):** every other guard in these handlers fails OPEN on a `GetRun` error; this one must not. Over the HTTP transport a run-read failure means the binding is UNKNOWN — the property the conflict/inheritance checks depend on — so the resolver **refuses regardless of whether an explicit value was supplied** (an explicit absolute path is self-validating, but its non-conflict is not, and non-conflict is the property the criterion demands). On stdio the same read failure degrades to today's behavior (explicit value used verbatim; omitted value resolves to the process cwd).

**start_run admission:** over the HTTP transport a `runner_kind: local` run REQUIRES an absolute `working_dir` — refused before any spec discovery or backend round-trip. `github_actions` / `gitlab_ci` runs spawn no local runner and have no checkout to bind, so they are untouched. `runner_kind` at start time is the caller-supplied hint (#1346), which is correct here: an agent that asks for a local run must supply its checkout.

### `working_dir` is bound once at start_campaign and inherited by every item run ([E48.87 / #2527](https://github.com/kuhlman-labs/fishhawk/issues/2527))

The run-level binding above removes the per-VERB repetition; this removes the per-ITEM repetition. `fishhawk_start_campaign` takes `working_dir` and binds it onto the CAMPAIGN row (`campaigns.working_dir`, migration `0071`, echoed as `working_dir` on the Campaign), and every item run minted by `fishhawk_start_campaign_item_run` inherits it. The per-item `working_dir` becomes an OVERRIDE, needed only when the campaign carries no binding.

The ladder is resolved SERVER-SIDE, in `handleStartCampaignItemRun`:

1. **An explicit per-item `working_dir` wins** — including when it DIFFERS from the campaign binding, which is **accepted as a deliberate override**, not refused. This diverges from `resolveWorkingDirForRun`, which REFUSES a conflict against a RUN binding, and the asymmetry is deliberate: a run has exactly one checkout and its branch lineage already lives there, so a conflict is incoherent; a campaign is a BATCH, and an item legitimately executing in a different checkout is conceivable. The divergence is logged, and the minted run row carries the value that actually applied, so the applied resolution is observable rather than inferred.
2. **Otherwise the campaign's binding is inherited** — re-validated through the same absolute-path gate an explicit value passes. Trusting the stored value would let a relative binding written straight to the campaign row bypass validation by arriving through a different door, which is the whole point of re-validating.
3. **Otherwise a `runner_kind: local` item is refused** `working_dir_required`.

**Rung 3 MOVED out of this package (a deliberate relocation).** #2498 fired the "a local item needs a resolvable checkout" refusal in `startCampaignItemRun` before any backend round-trip. That is no longer possible: an omitted `working_dir` is no longer necessarily missing — the campaign may bind it — and this layer cannot see the campaign row without a read. So the SEMANTIC refusal moved to the handler (which already loads the campaign) as a distinct `working_dir_required` 400, and the tool maps that code back to the same actionable operator message, naming BOTH remedies (bind it once at `start_campaign`, or pass it for this item). It stays transport-INDEPENDENT in #2498's sense — it fires for stdio and HTTP clients alike — and now also covers direct REST callers, which the MCP-only guard never did. The cost is one backend round-trip on that refusal; it mints nothing. The alternative (having the tool read the campaign first) needs a new client read, duplicates the ladder in two layers, and still leaves REST callers unguarded.

**What this package still refuses locally** is the purely SYNTACTIC non-absolute check, on BOTH verbs: `start_campaign`'s new `working_dir` and `start_campaign_item_run`'s existing one. Neither needs I/O, so both still dial nothing — and a relative CAMPAIGN binding is worth refusing early precisely because it would be inherited by every item run in the batch.

**MCP roots is not the alternative.** Passing the path as a tool parameter rather than asking the client is load-bearing: MCP roots is deprecated as of protocol revision 2026-07-28 (SEP-2577), whose migration guidance directs servers to take paths via tool parameters, and server-initiated requests are refused mid-tool-call on that revision (SEP-2322). This inference was rejected deliberately; a later proposal to reintroduce a roots-based path should re-read this note first (it is falsifiable by a future revision restoring roots).

`nextActionsFor` stamps the binding onto the params of every emitted action whose verb is one of the four (`foldWorkingDirParams`), and does nothing for an unbound run — so a driving loop propagates the binding without re-deriving it, and an unbound run never advertises an empty-string path.

## Progress notifications (`fishhawk_run_stage`)

`fishhawk_run_stage` spawns the runner and relays its stderr JSONL lines as MCP `notifications/progress` updates — but **only when a `progressToken` is present** on the call. A `progressToken` is client-supplied MCP request metadata, **not a tool input**: a tool-calling caller cannot set it, so whether live streaming is available is a property of your MCP client, not a knob you can reach (the MCP opt-in progress model). The durable signal is unaffected either way: the runner's events are still returned post-hoc in the final result's `events` list, and in the audit log and signed trace bundle.

While the agent runs, the runner emits a `stage_progress` heartbeat (~every 15s, see [runner/README.md](../../../runner/README.md#progress-heartbeats-580)). The relay renders it into the notification's message:

    stage_progress turns=7 tokens=13402 elapsed=42s last=assistant

Because the cadence is time-driven, a stalled stage keeps producing heartbeats with non-advancing `turns`/`tokens`, so a watching operator/client can tell a progressing stage from a stuck one. Note this is a signal for the **operator/client watching the run**, not a live early-cancel channel for the synchronously-blocked driving agent — that agent sees the heartbeats only after `fishhawk_run_stage` returns (and as groundwork for a future async run_stage).

### Compact-by-default result (#647)

The final tool result is **compact by default**: the routine `stage_progress` heartbeats are dropped from the `events` list, while every non-heartbeat event — `runner_completed`, `git_diff`, `runner_cancelled`, etc. — is retained in arrival order alongside `stage_state` and the best-effort enrichment fields. The heartbeats' signal is preserved in five scalar summary fields distilled from the stream:

| Field | Source |
|---|---|
| `outcome` | terminal `runner_completed` event (`ok` \| `failed`) |
| `tokens_used` | `runner_completed` when present, else the last heartbeat's `tokens_so_far` |
| `turns` / `elapsed_seconds` / `last_event_kind` | the last `stage_progress` heartbeat |

This roughly halves the driving agent's per-stage context cost without losing any durable signal — the audit log and signed trace bundle are unchanged. Pass `verbose: true` on the input to restore the full event list including every heartbeat (e.g. a driver that wants to inspect per-heartbeat progression).

## Non-blocking dispatch (`fishhawk_dispatch_stage`, [#1232](https://github.com/kuhlman-labs/fishhawk/issues/1232))

`fishhawk_dispatch_stage` is the **non-blocking sibling** of `fishhawk_run_stage`. Where `run_stage` blocks to terminal and returns the full event list, `dispatch_stage` spawns the same `fishhawk-runner` subprocess **detached** and returns the durable `(run_id, stage_id)` handle plus a (normally non-terminal) `stage_wait_status` **immediately**. It reuses `run_stage`'s input validation, stage-id resolution, runner-binary resolution, repo detection, and argv composition (the shared `composeRunnerArgv`, so the spawned argv is byte-identical) — the only difference is the spawn mode.

The flow it enables (the [#1189](https://github.com/kuhlman-labs/fishhawk/issues/1189) in-band amendment fix, ADR-037 poll-to-terminal half):

1. `fishhawk_dispatch_stage --stage implement …` — returns the handle now.
2. Poll `fishhawk_get_run_status` on the advertised `poll_interval_seconds` (derived — see the stage-execution wait contract above; the dispatch response itself always advertises the 30s floor, because the spawn path does not hold the run row and a freshly dispatched stage has ~0 elapsed) until the stage's `*_stage_wait_status` goes terminal.
3. **Between polls**, when a `scope_amendment_pending` surfaces, call `fishhawk_decide_scope_amendment` — so the runner's amendment `?wait` poll resolves **before its window elapses**, with no failed-stage retry.

This is what a **single** MCP session needs: a blocking `fishhawk_run_stage` call cannot decide an amendment the same agent's runner files mid-stage. `fishhawk_dispatch_stage` **superseded the interim `fishhawk run auto-decide` second channel** ([#1233](https://github.com/kuhlman-labs/fishhawk/issues/1233)/[#1234](https://github.com/kuhlman-labs/fishhawk/issues/1234)) for that decision, since removed ([#1554](https://github.com/kuhlman-labs/fishhawk/issues/1554)).

Detached-spawn properties (differ deliberately from the synchronous `spawnRunnerStage`):

- **Own process group** (`Setpgid`): a `SIGINT`/`SIGTERM` to the MCP server's foreground group is **not** forwarded to the runner — it is meant to outlive the tool call. There is **no** SIGTERM→grace→SIGKILL watcher.
- **Output → a per-invocation log file** under `os.TempDir()` (`fishhawk-runner-<run>-<stage>-<unixnano>.log`), **never a pipe**: an unread pipe fills its kernel buffer and blocks the writer once full (#446). The runner ships its trace via `--upload-trace` and its state to the backend, so the local log is a diagnostic only. `log_path` is returned for that diagnostic.
- **A reaper goroutine** (`reapDetachedRunner`) collects the child's exit so it never zombies while the tool returns, and converts two exit shapes into a backend report so the stage never sits stuck:
  - **Non-zero exit** (`*exec.ExitError`, #1747/#1763): parse the runner's `runner_failed` line from the log, report category C via the bounded-backoff `/reap-failure` POST (`reportDetachedFailureWithRetry`), and fall back to the stderr diagnostic + dispatch watchdog on exhaustion.
  - **Zero-exit STRAND** (#2630): a clean exit is NOT proof the stage settled — the #2630 runner exited 0 having re-entered its completed PR-open short-circuit, opened no PR, and left its implement stage `running` forever — which, before [#2689](https://github.com/kuhlman-labs/fishhawk/issues/2689), no verb short of `cancel_run` could clear (`fishhawk_reap_stage` is now that verb; see below). `reapZeroExitStrand` probes the stage state (`detachedStageStateProbe`, reading `ListRunStages`) and reports a category-C strand ONLY when the state is STILL in the POSITIVE allow-list `reapStrandAllowList` = {`dispatched`, `running`} after a bounded settle window (`reapProbeBackoff`, which absorbs the exit race — a healthy runner exits within ms of the backend committing its terminal transition). The allow-list, NOT `!stageStateIsTerminal`, is load-bearing: `awaiting_scope_decision` / `awaiting_input` / `awaiting_children` / `awaiting_approval` / `awaiting_host_dispatch` are all non-terminal states a runner legitimately exits 0 into (a scope-completeness park, a clarification park, a decomposed parent, a gate, a child awaiting host dispatch), and reaping any would destroy a live park — the exact class `reap_failure.go` refuses server-side for `awaiting_children`. Fail OPEN on every uncertainty (a nil probe — the pre-#2630 no-op, any probe error, or a state outside the allow-list): report nothing and leave the dispatch watchdog as the backstop. A confirmed strand lands the stage `failed`, so `retry_stage` can recover it — converting a permanent strand into an ordinary failed stage.

**Testing note — the reaper is a concurrent reader ([#2687](https://github.com/kuhlman-labs/fishhawk/issues/2687)).** A test that real-spawns a detached runner also fires `reapZeroExitStrand`, whose attempt-0 probe reads `ListRunStages` immediately (no pre-sleep) — concurrently with whatever the handler reads. So ORDINAL fault injection against `GET /v0/runs/{id}/stages` (the shared fake's `stagesFailOnCall`) is INVALID under a real spawn: the two readers race for the injected 500 and which call consumes it is nondeterministic (the `TestDispatchStage_PostFetchFailureWarnsNoError` flake). Keep such a test single-reader — stub the spawn seam (`withStubbedDispatchSpawn`), or use a no-spawn path. Recorded but NOT fixed here: when the probe finds an allow-list state (`dispatched` — which the fake's host-dispatch marker sets), the reaper goroutine OUTLIVES its test — it sleeps `reapProbeBackoff` and re-probes (up to ~3.5s) — while reading the package vars `reapProbeBackoff`/`reapReportBackoff` that `run_stage_test.go`'s `zeroReapProbeBackoff`/`zeroReapBackoff` helpers WRITE under `t.Cleanup`; a full-package `-race` run whose ordering places a reaper-leaking dispatch test just before one of those helpers is a latent cross-test race, diagnosable from here rather than mystifying.

Restarting the MCP server (`scripts/dev reload`) while a detached stage is in flight **orphans** the runner (reparented to init) but it continues to terminal and stays pollable via `fishhawk_get_run_status` — the intended durability of the `(run_id, stage_id)` handle (ADR-037), not a regression. Requires the `fishhawk-runner` binary to resolve on the MCP host, exactly like `fishhawk_run_stage`.

### Detached-runner registry and cancel reap ([#2545](https://github.com/kuhlman-labs/fishhawk/issues/2545))

`detached_registry.go` tracks the detached children **this MCP process** spawned so `fishhawk_cancel_run` can reap them. Without it, cancelling a run flipped the run state and returned while the runner kept running — still holding its lineage lockfile — and every later same-lineage dispatch failed category-C until an operator killed the process by hand.

**The invariant.** `startAndRegister` is the ONE chokepoint every detached spawn goes through, and its tombstone check, `cmd.Start()` and registration all happen under **one hold** of the registry mutex. That closes the register-after-Start race by construction:

- a cancel that wins the lock records a tombstone and the spawn is **REFUSED before any fork** (`errRunCancelledBeforeSpawn`, returned verbatim by `spawnRunnerStageDetached` so `dispatch_stage` / `drive_run` surface it);
- a spawn that wins the lock is **registered before it releases**, so a cancel enumerating afterwards always sees the child.

There is no interval in which a started child is unregistered. The reaper goroutine `deregister`s (deferred, so it also runs on the reaper's early returns), which closes the handle's `done` channel; `terminateRunners` waits on that channel rather than polling the pid, because `os/exec`'s `Cmd.Wait` releases the `Cmd`'s resources.

**The reap.** After `CancelRun` returns success (never for a cancel that failed — the tombstone must not outlive a no-op), `cancelRun` calls `terminateRunners`: TERM → `detachedTerminateGrace` → KILL → grace, per registered child. The verb reports `reaped_runners` and, for any child that never exited, a `reap_warnings` entry naming its stage id and pid. **A warning never fails the verb** — the cancel already landed, and turning a landed mutation into a tool error is exactly the failure [#2510](https://github.com/kuhlman-labs/fishhawk/issues/2510) removed from this type.

Immediately before **each** signal `terminateRunners` checks the handle's `done` channel **non-blockingly** (`handleExited`, [#2584](https://github.com/kuhlman-labs/fishhawk/issues/2584)). A handle whose reaper already closed `done` is a confirmed-dead child: it is **skipped — not signalled, not counted in `reaped_runners`, not warned about** (it was not reaped *by us*; we found it already gone). This mirrors the runner-side `holderAlreadyExited` ([#2582](https://github.com/kuhlman-labs/fishhawk/issues/2582)) which treats a confirmed-gone lock holder as eviction success rather than a survivor; the MCP side owns the handle and needs no signal to learn the child is gone. **Honest limitation:** this does NOT shorten the window in which `deregister` is still pending behind the reaper's `reapReportBackoff` report loop (`run_stage.go`) — throughout that loop the child is dead but `done` is still open, so the dead pid is signalled exactly as before. The guard makes the skip correct only where the close DOES land in time: a `deregister` completing after `terminateRunners` took its snapshot (most plausibly while blocked in the grace for an earlier handle of the same multi-handle run), which would otherwise draw a false kill-by-hand warning naming a dead pid.

**The tombstone must not become a new wedge.** It exists ONLY to catch a dispatch already in flight at the moment of cancel — the window between the caller's decision to spawn (host-dispatch marker POST, argv compose, log-file open) and `cmd.Start()`. So:

- its TTL is the **spawn-in-flight timescale, seconds not minutes** (`detachedCancelTombstoneTTL`, default **30s**), pruned on every registry mutation; and
- the primary release is not the clock at all: the refusal branch **consults the run's live state** via the probe the cancel recorded (`runResolver.runStateProbe` → `GET /v0/runs/{id}`) and **ADMITS immediately** when the run is no longer `cancelled`. An operator who cancels and then revives or retries inside the window gets a normal dispatch, not a fresh refusal.

The probe fails closed: absent, erroring, empty-state, or still-`cancelled` all refuse. A NEWER cancel landing while the probe was in flight also refuses (tombstone generations are compared), so the admit can never act on a stale answer.

A refusal also **removes the stage log file** it opened before the registry call. That file is created ahead of `startAndRegister` (the child needs the fd at fork), and the refusal returns an empty `logPath`, so a left-behind file is referenced by nothing — one zero-length orphan in `TempDir` per refused re-dispatch, unbounded over time. Removal is best-effort; a failure there does not change the refusal.

**Honest limitation.** The registry is **per MCP process**. A cancel issued from a different MCP server process than the one that dispatched reaps nothing — `reaped_runners` is 0 and nothing is wrong. The net for that case is runner-side: `runner/cmd/fishhawk-runner`'s lineage-lock holder rule reclaims the lock from a holder it can PROVE is an orphaned runner of a cancelled run (see that package's README).

### Sibling-in-flight dispatch refusal ([#1872](https://github.com/kuhlman-labs/fishhawk/issues/1872))

Both host-spawn verbs — `fishhawk_dispatch_stage` and `fishhawk_run_stage` — refuse to spawn a runner while **another stage of the same run is still executing**. Concretely, the dispatch is **blocked** (a non-nil tool error, **zero** runners spawned) when any stage OTHER than the target is `dispatched` or `running`, or when the **target stage itself is `running`** (a live runner already owns it — a second spawn would double-drive it). A sibling parked at **`awaiting_host_dispatch`** ([#1912](https://github.com/kuhlman-labs/fishhawk/issues/1912)) is **NOT** in-flight (no spawn attempt exists yet) and does **not** block — only `{dispatched, running}` siblings do. Dispatching a sibling stage while an implement runner is still in its ship phase (which spends its whole duration `running`) rotates the run's signing key out from under the in-flight runner; the block prevents that contention at admission time. The target stage's own **park** states — `awaiting_host_dispatch` (the plan-approved / retry / fixup local park, #1912) and the legacy/transitional `dispatched` (dead-runner re-dispatch) — are **allowed**, since blocking them would wedge every local dispatch. A stage-list read error **fails open** (a warning, the dispatch proceeds) — the backend's any-unexpired-key signature verify (#1872) is the correctness backstop. The refusal names the in-flight stage's type and state and tells you to wait for it to settle (the implement ship phase ends when its pull-request artifact upload lands).

### Acceptance-dispatch admission ([#1928](https://github.com/kuhlman-labs/fishhawk/issues/1928))

All three host-dispatch verbs — `fishhawk_dispatch_stage`, `fishhawk_run_stage`, and `fishhawk_drive_run` — call `POST /v0/stages/{stage_id}/acceptance-admission` for an **acceptance** stage BEFORE recording spawn evidence or spawning a runner. The backend evaluates the approved plan's three disjoint short-circuit predicates (out-of-scope skip, empty-criteria, all-skip-with-basis — the same arm `orchestrator.Advance` runs on the retry path); on a hit it settles the acceptance stage straight to `succeeded` (a `not_validated` verdict for the two basis-bearing predicates, [#2347](https://github.com/kuhlman-labs/fishhawk/issues/2347), or a skip marker for the out-of-scope predicate — **no runner dispatched** either way) so the verb returns/continues with the settled stage rather than spawning a runner that would only fail category-C `acceptance_target_unreachable`. `fishhawk_dispatch_stage` / `fishhawk_run_stage` return an output composed from the settled stage and record **no** spawn evidence; `fishhawk_drive_run` appends a short-circuit `DriveStep`, records no act, spawns nothing, and continues the loop (the next poll observes the terminal stage). No new tool is added. The call **fails OPEN on a TRANSPORT error only**: a `short_circuited:false` result (a non-admissible stage state — already settled, mixed criteria, an unconfigured orchestrator) is the normal no-op path and proceeds to record+spawn exactly as today with **no** warning; a network/5xx admission-call error appends a fail-open warning before proceeding to spawn. A **4xx admission REJECTION** (401 / 403 `cross_run_admission` / 422 / a **body-decoded 404** whose error envelope decoded to `stage_not_found`) is **NOT** fail-open — the verb **halts** with a tool error and spawns nothing, so a runner never executes after the run-subject authorization boundary rejected the request. The one carve-out (#1937) is a **route-absent bare 404** — a `404` with an *empty* decoded error code, i.e. a plain-text body that never decoded into the OpenAPI error envelope: an older fishhawkd that predates this endpoint (#1928) answers the unregistered route from its stdlib `http.ServeMux` (`http.NotFound` → `"404 page not found"`, no JSON envelope), while the registered handler always writes the envelope via `writeError` (so a genuine `stage_not_found` decodes to a non-empty code). That envelope-code discriminator reclassifies a bare route-absent 404 into the **transport-class fail-open** path with a **version-skew warning** — so a new fishhawk-mcp against an old fishhawkd does not wedge every acceptance dispatch — while a positively-evaluated `stage_not_found` 404 still fails closed. On the transport fail-open path (network / 5xx and the bare-404 skew) the verb also **re-checks the target stage** before spawning (a mid-walk 500 can leave the acceptance stage `running`); an observed non-dispatchable state halts rather than double-driving a partially-settled stage.

### Acceptance target-identity gate ([#1953](https://github.com/kuhlman-labs/fishhawk/issues/1953))

When admission returns `short_circuited:false` **and** the approved plan needs LIVE validation (no short-circuit predicate matched) **and** the acceptance stage's spec declares egress `target_hosts`, the response carries `needs_target:true` + `target_hosts` (verbatim spec hosts) + `expected_head_sha` (the resolved merge-candidate head SHA; may be empty). On such a result each host-dispatch verb runs a **verb-side target-identity gate** BEFORE any spawn evidence: it probes the first declared target host **from the dispatch host** (the same network position the local runner would probe from) — a direct-dial `GET <scheme>://<host>/healthz` (`Proxy:nil`, so an ambient operator proxy can't fake reachability; http-first for loopback/IP-literal hosts, https-first otherwise) whose `git_sha` is classified against `expected_head_sha` with the **runner's semantics** (`unreachable < unverifiable < stale < verified`; a `-dirty` suffix is stale/fail-closed, a `<7`-char sha is unverifiable, a `>=7`-char prefix match is verified). This intentionally **mirrors** the runner's `previewprobe.go` (a separate, non-importable Go module) — the classification table is pinned in `acceptance_target_test.go`.

Outcomes:

- **stale / unreachable** → **refuse to spawn**: `fishhawk_dispatch_stage` / `fishhawk_run_stage` return a structured `needs_target` (`{target_host, expected_head_sha, detail, remediation}`, `outcome:"needs_target"` on run_stage) recording **no** spawn evidence and stamping **no** host-dispatch marker; `fishhawk_drive_run` stops with `stopped_reason=acceptance_needs_target` + a `NextActions` pointer, records **no** act and spawns nothing. The stage stays `awaiting_host_dispatch`/`pending`, so re-dispatch is clean once the operator brings up the target at the named head SHA (e.g. `scripts/dev preview`).
- **verified** → **proceed to spawn** exactly as today.
- **unverifiable** (target answered but exposes no comparable build identity) → proceed with a warning.
- **`FISHHAWK_ACCEPTANCE_PREVIEW_CMD` set in the verb env** → proceed with an informational note **without probing**: the spawned local runner inherits `os.Environ()` and provisions the target itself ([#1569](https://github.com/kuhlman-labs/fishhawk/issues/1569)).
- **empty `expected_head_sha`** (older backend / ledger resolution failure) → proceed with a warning — the gate never hard-fails a stage on a missing expectation (runner parity).
- **no declared hosts** → proceed silently (the runner skips its own target gate then too).

All response fields are additive: a mixed old/new backend that omits them decodes to zero values and the verb spawns exactly as today.

### Campaign acceptance-slot visibility (`acceptance_slot`, [E48.71 / #2503](https://github.com/kuhlman-labs/fishhawk/issues/2503))

Acceptance validates against **ONE** fixed, shared preview slot, so a campaign driving N items **serializes** on it. Before this, the only signal of the contention was the per-dispatch `needs_target` refusal — which says nothing about the sibling item holding the slot, so an operator driving N items discovered the contention by hitting it. `fishhawk_get_campaign_status` now carries an additive, best-effort **`acceptance_slot`** block (`omitempty`) that makes the contention visible. It is **observability only**: no host mutation (ADR-038 keeps that off the MCP surface) and no per-run preview allocation.

**Host resolution ladder.** `target_host` is `FISHHAWK_PREVIEW_ADDR` when non-empty after `TrimSpace` (`target_host_source:"env"`), else the literal `localhost:8090` (`source:"default"`) — byte-matching `scripts/dev`'s `_preview_addr` default so the probe and the dev tooling agree. **Known limit** (like the target-identity gate above): the probe is issued **from the process serving the tool** — the operator host on the stdio transport, the fishhawkd host on the `/mcp` route (#2390). On the HTTP transport a default `localhost:8090` probes fishhawkd's own loopback, not the operator's preview; the `FISHHAWK_PREVIEW_ADDR` override is the escape hatch, and `detail` always names the URL actually probed so a misdirected probe is self-diagnosing.

**The probe** (`probeAcceptanceSlotHealth`) is a **single-shot, no-retry** `GET <scheme>://<host>/healthz` reusing `acceptanceProbeSchemeOrder` + the shared `acceptanceProbeHTTPClient` (direct-dial `Proxy:nil`, redirects refused), wrapped in a short `context.WithTimeout` (`acceptanceSlotProbeTimeout`, a package var tests shrink). It differs from the dispatch gate's probe **deliberately**: there is no expected head SHA on a campaign read, so it **reports** the served `git_sha` rather than classifying it, and it has **no** unreachable-retry loop — a status poll must not inherit the dispatch gate's multi-attempt boot budget, so a black-holed target adds **at most** that one timeout to the read.

**Three states, trustworthy by construction** (the operator conditions on #2503 — an observability signal that can be *confidently wrong* is worse than one that admits uncertainty):

| state | when |
|---|---|
| `held` | a 200 with a non-empty, non-`"unknown"` `git_sha` — a build is serving the slot; `serving_git_sha` is reported |
| `free` | **only** a connection **refused** from a reachable host (the one positive "nothing is bound" signal) **and** no stage-named holder |
| `unverifiable` | everything else: any **non-refused** dial failure (timeout, black-hole, no route, wrong host — a dial failure is **never** `free`); a reachable-but-unidentified answer (non-200, non-JSON, missing/`"unknown"` `git_sha`); **or** a probe/stage disagreement (probe says free while stage state names a live holder — resolved to `unverifiable`, never trusting either half, with `note` saying which signals disagreed) |

**Participant classification** (per-item, best-effort). Pass 1 (no network) selects candidate items — non-terminal item state carrying a parseable `run_id`. Pass 2 reads each candidate's stages and finds the `acceptance` stage: `dispatched`/`running` → **holds** the slot (`held_by`, first in item order wins); `pending`/`awaiting_host_dispatch` → **waits** behind it; a terminal stage or no acceptance stage → not a participant. Attribution is by **live acceptance stage**, not by matching the served `git_sha` back to an item (no read-only surface in this layer exposes a run's merge-candidate head SHA), so a settled or never-dispatched sibling that still binds the slot reports `state:held` + `serving_git_sha` with an **empty `held_by`** (the #2488 case); `note` covers that explicitly.

**Three bounding controls** (each pinned by a deletion counterfactual in `acceptance_slot_test.go`):

- **No-probe short-circuit** — when pass 2 yields no holder and no waiter **and the inspection was complete** (not truncated **and** no per-item read failed), return `nil` (block omitted) **without probing**: a campaign nowhere near acceptance issues **zero** network calls.
- **Per-item read cap** (`acceptanceSlotMaxItemReads` = 12) — more candidates than the cap sets `truncated:true` + `items_inspected`/`inspection_limit` rather than capping silently. **When the cap is hit before any participant is found**, the block is **still surfaced** (state `unverifiable`, no probe) rather than omitted — otherwise a candidate at the acceptance gate *beyond* the cap would silently drop both the block and the `truncated` signal (#2503). No probe runs on that path: without a known participant a refused probe could not distinguish `free` from a holder never inspected, so it could only mislead.
- **Best-effort degrade** — a per-item `ListRunStages` failure degrades that item (recorded in `read_failures` so the block is honestly partial) and never fails the campaign-status snapshot (the `children_status.go` contract). **When that degrade leaves NO classified participant** — a sole candidate, or all candidates, whose reads failed — the block is **still surfaced** (state `unverifiable`, no probe, `read_failures` populated) rather than omitted, for the same reason as the truncation case: those failed reads could have been an item AT the acceptance gate, and omitting the block would present the incomplete inspection as "no acceptance participation" and drop the `read_failures` signal (#2503 fix-up). The incomplete-inspection block names every reason (truncation, failed reads, or both).

`note` names the remediation (`scripts/dev preview <head>`) and that the slot is shared. The whole block is `omitempty`, so a campaign with no acceptance participant is byte-identical to the prior response.

## Local auto-driver (`fishhawk_drive_run`, [#1700](https://github.com/kuhlman-labs/fishhawk/issues/1700))

`fishhawk_drive_run` executes **every mechanical operator step between human gates** on a `runner_kind:local` run under ADR-040 delegation, and stops at the first genuine decision. It is the local sibling of the GHA campaign auto-driver (E25.6/E25.7's `AutoDriveRunGate`): a bounded, resumable loop that reuses this host's session, token, and detached-spawn machinery rather than a separate daemon (ADR-024 — the local runner can only be spawned by this MCP host).

Each loop iteration:

1. **Gate-ordered, record-before-dispatch.** The driver dispatches only the **earliest non-terminal stage**, and only once its **gate preconditions** hold: plan is always dispatchable; implement dispatches after the plan stage **succeeds**; acceptance dispatches after the implement stage **succeeds** and every review stage settles **OR** the run's `derived_status` reads `acceptance_pending` ([#1961](https://github.com/kuhlman-labs/fishhawk/issues/1961) — the backend's settled-evidence signal: review evidence is terminal and required checks are green while only the human review row still sits `awaiting_approval`). Under that signal a review stage parked at `awaiting_approval` is **non-blocking** for both the earliest-non-terminal walk and the acceptance precondition, so a parked human review row no longer strands a mechanical acceptance dispatch behind a mislabeled `decision_required:review_gate_parked` stop; a review stage still `pending`/`dispatched`/`running` (evidence genuinely in flight) still holds acceptance, and absent the signal (non-drive run, any other derived value) behavior is byte-identical. This makes the **`decision_required` contract** exact: a `decision_required:*` stop now always corresponds to a `next_actions` entry that names an operator judgment (a plan gate without `may_approve`, a split verdict / open concerns / undelegated merge, a pending scope amendment) — never a state whose authoritative next act is a bare dispatch or a poll. A fresh run creates every stage as a `pending` row, so this ordering is load-bearing (#1890): the earlier lowest-sequence-dispatchable rule dispatched implement + acceptance the instant plan was spawned, and both died category-C on the lineage lock the plan runner held. The **host-spawnable** states are `{pending, awaiting_host_dispatch}` ([#1912](https://github.com/kuhlman-labs/fishhawk/issues/1912)): a `runner_kind`-locked-local run parks its agent stage at `awaiting_host_dispatch` (the backend cannot spawn the host-local runner, ADR-024), so a **parked implement after a delegated plan approval is AUTO-DISPATCHED by the loop with no manual handoff**. For that one dispatchable stage it FIRST calls `POST /v0/runs/{id}/auto-drive/acts` to record the dispatch; then, immediately before spawning, it calls `POST /v0/runs/{id}/stages/{stage_id}/host-dispatch` to **mark the spawn** (CAS `{pending, awaiting_host_dispatch}` → `dispatched`), and only on a **successful record AND marker** host-spawns the runner via the same `spawnRunnerStageDetached`/`composeRunnerArgv` path `fishhawk_dispatch_stage` uses. A failed record stops the verb (`stopped_reason=unrecorded_act`) and a failed marker stops it (`stopped_reason=host_dispatch_failed`), both **without dispatching** — an unaudited or unmarked mechanical act is impossible by construction (an unmarked spawn would recreate the `dispatched` ambiguity #1912 removes).

   **Fix-up re-opens, honestly, in two parts.** A stage re-opened to `awaiting_host_dispatch`/`pending` after a delegated fix-up/retry is handled per invocation:
   - **Intra-invocation** (the same drive loop that saw the gate act): the re-open is re-dispatched and attributed `fixup_redispatch`.
   - **Cross-invocation** (a fresh drive invocation resuming a fix-up re-opened stage): the driver **never auto-spawns** a stage already in `dispatched` that it did not spawn itself — double-spawn stays impossible by construction. A re-opened `pending` implement stage carrying a `stage_fixup_triggered` audit row newer than its newest implement dispatch is re-dispatched as `fixup_redispatch`.

   **`dispatched` staleness — spawned but never reached its prompt fetch ([#1912](https://github.com/kuhlman-labs/fishhawk/issues/1912)).** Post-#1912 `dispatched` unambiguously means **a spawn attempt exists** (the host-dispatch marker stamped it, refreshing `updated_at`). A `dispatched` stage this invocation did not spawn has a runner in flight — from a prior driver invocation OR a manual `fishhawk_dispatch_stage` — so the driver **never re-spawns** it. It is classified purely on the **runner-liveness threshold**, anchored on the stage's own `max(updated_at, started_at)` (the marker/spawn timestamp; the older #1905 dispatch-row-timestamp anchor is removed — the `run_auto_driven act:dispatch` row is now **attribution only**, still matched source-agnostically for the retry/fixup discriminator). Two branches:
   - **Anchor past the liveness threshold** (default 10 min; a live local runner flips `dispatched`→`running` within seconds of its prompt fetch, #1924) — the driver **probes host liveness itself** ([#1955](https://github.com/kuhlman-labs/fishhawk/issues/1955)): it execs `pgrep -f` scoped to the stage's `--stage-id <uuid>` argv token (the MCP host is the host that spawned the runner, ADR-024, so the probe is precise) and classifies the result three ways. **DEAD** (pgrep exit 1, no matching process) — the spawned runner died at or just after spawn — is **auto-recovered in place**: the driver falls through its `record-act → host-dispatch marker → spawn` path with a stale-re-dispatch `steps_taken`/`run_auto_driven` note (`stale re-dispatch: liveness probe found no runner process`) and drives on, no operator action. **LIVE** (exit 0: a process carrying the stage id exists yet never flipped `running`) stops `dispatched_stale` and **never spawns** — a second runner into the same lineage lock stays impossible; the warning names the live process + the dispatch `log_path`. **UNKNOWN** (pgrep absent / exit ≥ 2 / exec error) degrades to today's manual verify-first stop `dispatched_stale`, `next_actions` pointing at a manual `fishhawk_dispatch_stage` after confirming no runner is live (`pgrep -f fishhawk-runner` + the dispatch's `log_path`). The `dispatched_stale` stop therefore survives only for the LIVE-or-unprobeable ambiguous cases.
   - **Anchor fresh** (or a zero-value anchor with no timestamped evidence) — poll. A hand re-dispatch spawns a fresh runner whose prompt fetch flips `dispatched`→`running`, so a subsequent `fishhawk_drive_run` reads the stage as in-flight and **polls to convergence** instead of re-reporting `dispatched_stale`.
2. **Poll** stages/reviews on the established 30s cadence while a stage or review is in flight. A pending **review** stage counts as in-flight only when it is REACHABLE (every lower-sequence stage terminal): a `feature_change` run creates its human `review` row `pending` at run creation, so an unconditional "pending review is in flight" rule would poll forever once the plan gate parks and never reach the gate/decision branch — the #1905 silent hang. When a **`progressToken`** is present, the driver emits an MCP `notifications/progress` heartbeat once per poll iteration (run state + earliest non-terminal stage + steps taken + elapsed) so a long drive is not aborted by the client's idle timeout. A `progressToken` is client-supplied MCP request metadata a tool-calling caller cannot set, so whether the heartbeat is emitted depends on your client; when it is not, the drive is still fully resumable (re-invoke with the same `run_id`), which is what makes a long drive safe without the heartbeat.
3. **Gate.** At every parked gate it FIRST waits for the parked stage's advisory agent reviews to settle — while `review_status` is `pending` (the #1127 count gate) it polls instead of calling the gate, so a delegated `may_approve` fires only on settled reviews rather than churning observe-only gate calls every interval; an unreadable review state at a parked gate **fails toward the operator** (a warning, then falls through to the gate/decision return — this path executes no code, unlike the dispatch-path fail-closed). Then it calls `POST /v0/runs/{id}/auto-drive` and **continues** on a delegated act (`approve`/`route_fixup`/`retry`/`merge`), **returns** immediately on a page (`paged:<event>`), on an observe-only outcome at a decision state (`decision_required:<state>` — a plan gate without `may_approve`, a split verdict), or on a **pending scope amendment** (no delegation knob covers amendments, so every one is a decision). A stall guard returns `stalled` rather than spinning. **Delegated-approval decision stops ([#2381](https://github.com/kuhlman-labs/fishhawk/issues/2381)):** the `may_approve` arm parks the operator with `decision_required:human_quorum_required` when a delegated approve could never advance the gate — a human-quorum `approvals` block (a delegated vote is recorded but never counted, so it cannot reach quorum), or a firing/unevaluable escalation, or an unreadable block — and `decision_required:delegated_approval_no_progress` when a delegated approve came back a duplicate; each has a `next_actions` entry pointing at `fishhawk_approve_plan` (the operator's own approval slot is free because the delegated vote is recorded under a distinct identity). **Driver-side no-progress bound:** a second consecutive delegated gate act (`gate.Acted`) that leaves the run/stage `driveSignature` AND action unchanged is a repeated no-op — the driver stops `decision_required:delegated_act_no_progress` after exactly `driveNoProgressThreshold` (2) such acts rather than looping to `max_minutes` (bounding the ~160-act livelock at 2 rows). The FIRST act is always allowed, so a legitimate `route_fixup`/`retry` that re-shapes the stage topology — changing the signature — is unaffected; the queued-merge path never reaches a second identical act. Two fail-closed guards: an **unreadable amendment state** (the amendment audit read errors) halts the driver (`stopped_reason=amendment_check_failed`) rather than falling through to dispatch, and a **queued merge** is remembered so later polls only await the webhook-settle — the gate is not re-called (no duplicate `run_auto_driven act:gate merge` rows, no re-enable of auto-merge). **Queued-merge memory persists across resumes**: the loop seeds it from an existing `run_auto_driven act:gate action:merge` audit row before starting, so a resume during merge latency polls for the webhook-settle instead of re-calling the gate. That seed read **fails OPEN** (a warning; the loop continues) — unlike the dispatch-path reads it opens no code-execution surface, so a false negative costs only a benign duplicate attribution row, and fail-closed halting would trade that for a wedge.

A clean run under fully delegated knobs goes `start_run` → `merged` with **no operator tool calls in between**, and its audit trail carries a delegated-context `run_auto_driven` row for **every** driver dispatch and **every** gate act (each gate row records the delegated rule as `delegated_rule` for provenance). **`run_auto_driven` is the supplementary driver-attribution record; each action's own audit row (`approval_submitted` with its delegated rule, `stage_fixup_triggered`, …) is the authoritative delegation record**, written transactionally by the action path. `merge` is **queued, not landed** (it enables GitHub auto-merge; the webhook settles the terminal run state), so `merged` is reported only after the run reaches `succeeded`.

Output: ordered `steps_taken[]` (each labeled mechanical vs delegated), the final run/stage state, `stopped_reason` (`merged` | `paged:<event>` | `decision_required:<state>` — where `<state>` includes `human_quorum_required` / `delegated_approval_no_progress` (server-side, #2381) and `delegated_act_no_progress` (driver-side no-progress bound) alongside `fixup_budget_exhausted` / `fixup_ceiling_reached` — | `timeout` | `stalled` | `stage_failed` | `unrecorded_act` | `host_dispatch_failed` | `run_failed` | `cancelled` | `gate_error` | `amendment_check_failed` | `dispatch_check_failed` | `dispatched_stale` | `context_cancelled`), and a `next_actions` pointer on a parked stop. **Every outcome is resumable** by re-invoking with the same `run_id`. Inputs: `{run_id, working_dir, github_repo, base_branch, runner_binary, max_minutes (clamped [1,240], default 60)}`. A **write** tool requiring `write:approvals`; local-only, requires the `fishhawk-runner` binary on the MCP host.

## Non-blocking decomposed fan-out (`fishhawk_run_children` + `fishhawk_await_children`, [#1144](https://github.com/kuhlman-labs/fishhawk/issues/1144), rewritten by [#2363](https://github.com/kuhlman-labs/fishhawk/issues/2363))

`fishhawk_run_children` **dispatches** a decomposed parent's currently-dispatchable children **detached** and returns
immediately. It does **not** await any child. `fishhawk_await_children` is the in-band wait that replaced the old
blocking await-all.

This is the whole point of #2363. The old verb held **one** MCP session and awaited every child, so a child that filed
a mid-stage scope amendment could not have it decided in-band: its `?wait` long-poll expired **UNDECIDED** (the request
stays pending server-side — an **expiry**, not a denial). The fix is structural, not a mitigation: the session is free
the moment the dispatch returns, so the decision is possible **by construction**.

Pass the decomposed **parent's** `run_id`; `run_children`:

- **Discovers** the children from the parent's `plan_decomposed` audit entry (`child_run_ids` +
  `effective_max_parallel`); a run with no such entry is a clean error (it is not a decomposed parent).
- **Partitions** by freshly-read implement-STAGE state — only children awaiting a host spawn (`pending` or
  `awaiting_host_dispatch`, #1912) are dispatched; in-flight (`dispatched`/`running`) and terminal children are
  reported as-is, so re-invocation is **idempotent**.
- **Marks then spawns**, per child, in a **plain sequential loop** with no goroutines, no `errgroup`, no mutex and no
  shared result map. That absence is load-bearing: the five rejected designs before this one all failed on the same
  shape — a handler returning while its goroutines kept writing into the value it returned. Detaching the spawn
  removes the shape rather than relocating it. `TestRunChildren_SourceHasNoConcurrencyShape` pins the deletion.
- **Uses the SAME detached machinery `fishhawk_dispatch_stage` uses** (`spawnRunnerStageDetached`, ADR-037 / #1232),
  which forks through the detached-runner registry — so `fishhawk_cancel_run`'s `terminateRunners` reaps these
  children. That **subsumes [#2679](https://github.com/kuhlman-labs/fishhawk/issues/2679)**, which reports exactly the
  missing capability; `TestRunChildren_RegistersChildrenForCancelReap` proves it with the real spawn rather than a
  faked seam.
- **Passes `--parallel-isolate`**, so each child provisions its **own isolated per-child git worktree**
  (`run-<child>`) — concurrent siblings never race a shared checkout and the operator's tracked tree stays untouched.

Returns `children[]` (`{run_id, stage_id, dispatched, stage_state, log_path, warnings}`, in `plan_decomposed` order),
`dispatched_count`, `effective_cap`, `resolved_working_dir`, and a `next_step` pointing at `fishhawk_await_children`.
There is **no** `exit_code` and **no** terminal `outcome`: a detached child is still running when the call returns.

### Server-side wave ordering, integration and re-base

Three things the client loop used to own now live server-side:

- **Wave ORDER** is enforced by the host-dispatch marker's wave-order guard (E48.99 / #2546), which 409s
  `dependency_not_satisfied`.
- **Between-wave INTEGRATION** moves into the child-completion sweeper (`orchestrator.IntegrateCompletedWave`), so a
  dependent wave's predecessors are merged with **no client alive**.
- **The per-wave RE-BASE** is answered by the marker itself: it returns the `base_branch` a dependent child must spawn
  against, and 409s `wave_not_integrated` when that child's predecessors have succeeded but are not yet merged.
  `run_children` uses the marker's `base_branch` when non-empty and the `base_branch` input only as a fallback — the
  client derives **nothing**.

So a dependent wave's children simply become dispatchable on a **later** invocation. The `wave_not_integrated` arm is
not a failure: it records the child `dispatched:false` with a warning naming the between-wave integration and pointing
at `fishhawk_await_children`.

### `effective_cap` is a per-invocation dispatch budget — and nothing more

`effective_cap` bounds how many children **this call** spawns, computed against a **point-in-time snapshot** of the
children whose implement stage reads `dispatched`/`running` during this invocation's partition (0 means unlimited).

It does **NOT** bound the number of live detached runners for the parent. Two concurrent invocations can each snapshot
the same in-flight count and each spend a full budget against it: the per-stage host-dispatch CAS prevents them
double-spawning the **same** child, but the loser simply proceeds to a different one. Bounding live runners would need
a server-side reservation the host-dispatch marker does not offer. Children left undispatched by the budget are named
in a warning; re-invoke once slots free.

### The spawn-error compensation, and its disclosed residual strand

The host-dispatch CAS commits **before** the spawn (the #1912 fail-closed ordering), so between the CAS and a
successful spawn the stage is `dispatched`. A failed spawn has **no reaper at all** — the reaper goroutine starts only
after the registry registration succeeds — so left uncompensated the stage sits `dispatched` forever, every later
invocation reads `transitioned:false` and treats it as already-dispatched, and the child can never be retried. That is
the permanent-strand class [#2630](https://github.com/kuhlman-labs/fishhawk/issues/2630) exists to remove.

- **A cancel-refused spawn is NOT compensated.** `errRunCancelledBeforeSpawn` means a cancel for this run already
  landed and the registry refused the fork; failing the stage would relabel a cancelled run as failed, and the cancel
  path owns the run's terminal state.
- **Any other spawn error is reported as a category-C stage failure** through the same `POST
  /v0/runs/{run_id}/stages/{stage_id}/reap-failure` closure the reaper would have used. This is authorized by
  construction: the reap handler's `reapProtectedParkStates` is a **closed allow-list of five PARK states** it must
  never collapse, and `dispatched` is deliberately **not** among them (the handler also refuses a terminal stage, so
  the endpoint's reap authority spans `{pending, dispatched, running}`) — and the stage is `dispatched` here, which is
  not a protected park. The report is called **FIRST and its OUTCOME decides the single warning**
  ([#2695](https://github.com/kuhlman-labs/fishhawk/issues/2695) item 4), so the disclosure never contradicts itself.
  On a **successful** report the stage lands `failed`/category-C — and only then is the one warning's "reported it as a
  category-C stage failure … recover with `fishhawk_retry_stage`" claim TRUE (for a local run the retry parks the stage
  back at `awaiting_host_dispatch`, which `run_children` admits, so a subsequent `run_children` re-spawns the child).
- **DISCLOSED RESIDUAL: if the report ITSELF fails, the child is STRANDED** in `dispatched` with no runner and no
  reaper. A single self-contained strand disclosure is emitted — with **no preceding success claim to retract** (the
  old ordering appended an unconditional success claim and then a "…that report ALSO failed" second warning, so a failed
  compensation returned two contradictory recovery instructions). `fishhawk_retry_stage` does **not** clear it —
  `run.RetryStage` admits only a stage already in state `failed`, and refuses anything else `ErrRetryNotApplicable`. The
  verb that actually clears it is the reap-failure REST endpoint the compensation just failed to reach: `POST
  /v0/runs/{run_id}/stages/{stage_id}/reap-failure` with `{"category":"C","reason":"runner_spawn_failed"}`, after which
  `fishhawk_retry_stage` becomes applicable. Since [#2689](https://github.com/kuhlman-labs/fishhawk/issues/2689) that
  endpoint HAS an MCP verb — **`fishhawk_reap_stage`** — so the strand warning prescribes
  `fishhawk_reap_stage` → `fishhawk_retry_stage` → re-invoke `fishhawk_run_children`, keeping the literal endpoint only
  as the fallback for a client that has not picked the verb up. This is still a disclosed residual, not a claimed
  recovery; `TestRunChildren_FailedCompensationDisclosesStrand` pins the honest behaviour (exactly one warning, and it
  does not claim a successful report) and the recovery the warning names.

**Pre-existing, unfixed, and stated plainly:** `fishhawk_dispatch_stage` has the SAME uncompensated shape today — it
marks the host dispatch, spawns, and returns a spawn error bare, leaving the single stage `dispatched` with no runner.
This change does not fix it (that would expand past the founder's decision for #2363); the operator files it
separately.

### The in-band wait (`fishhawk_await_children`)

`fishhawk_await_children` is **read-only**: it polls server state, spawns nothing, cancels nothing, holds no handle.
Because it owns no result, a return can never race a write. Every release condition is an **ABSOLUTE property of the
current snapshot**, evaluated on **every poll including the first** — there is no baseline and no transition
detection anywhere in the verb. A transition-keyed release could neither fire before the integration it waits for, nor
(when the interesting change already happened before the call) ever fire at all.

| Status | Releases when | `next_step` |
|---|---|---|
| `amendment_pending` | some child has a pending mid-stage scope amendment (the strict [#2588](https://github.com/kuhlman-labs/fishhawk/issues/2588) predicate, reused verbatim per child) | `fishhawk_decide_scope_amendment`, pre-filled |
| `children_dispatchable` | some child's **implement-stage** state is host-dispatchable (`{pending, awaiting_host_dispatch}`) **and** its dependency slices are COVERED per the shared `wavecoverage.Covered` predicate | `fishhawk_run_children` |
| `children_settled` | every child reached a terminal run state | `fishhawk_consolidate_slices` |
| `timeout` | none of the above within the window (resumable; the wait holds no server state) | `fishhawk_await_children` (re-arm) |

Every release — **including `timeout`** — carries the same `ChildrenStatus` snapshot and a `next_step`; a timeout's
`next_step` re-arms the wait, since a timeout is a resumable checkpoint, not a terminal state.

**Timeout default is 600s ([#2695](https://github.com/kuhlman-labs/fishhawk/issues/2695) item 2).** An omitted
`timeout_seconds` resolves to **600s**, not the shared 360s review default — reconciling with [#2363](https://github.com/kuhlman-labs/fishhawk/issues/2363)'s
approved 600s contract (`clampAwaitChildrenTimeout`, an await_children-specific default). The cap ladder is unchanged:
600s, raised to 7200s via `long_wait:true` or a client `progressToken`. So an omitted value with neither opt-in waits
the **full 600s cap** by construction, and no other await verb's 360s default is touched.

**The fan-in snapshot read is category-filtered and paginated ([#2695](https://github.com/kuhlman-labs/fishhawk/issues/2695)
item 1).** The snapshot's fan-in markers (`slices_integrated` / `slice_integration_conflict`) land **late** in a
decomposed parent's audit, so the old single unfiltered 500-entry window silently dropped the newest marker on any
parent with a longer history — every await then timed out. `latestFanInAudit` instead issues a **category-filtered**
read per fan-in kind and walks each to its **LAST page** (the endpoint returns entries ascending, so the max-Sequence
entry `childrenStatusFor` keeps is always on the last page). The walk **fails loud** rather than returning a stale page:
a history exceeding the per-category page cap, and a non-progressing (looping) cursor, each surface as their OWN wrapped
`read parent audit` error (distinct diagnoses), so `awaitChildrenEvaluate` sees a read FAILURE — never a confidently
wrong snapshot.

`children_dispatchable` keys on the child's **implement-stage** state, NOT its run-level state. A local decomposed
child parked by `RuleChildrenDispatch` has its RUN advanced to `running` while its implement stage sits at
`awaiting_host_dispatch` ([#1237](https://github.com/kuhlman-labs/fishhawk/issues/1237)), so a run-state predicate would
skip the entire primary locked-local parked population — the same `{pending, awaiting_host_dispatch}` partition
`fishhawk_run_children`'s own dispatch loop uses (`implementStageDispatchable`). It keys on **coverage**, not on
`blocked`: predecessor run state flips to `succeeded` **before** the between-wave integration runs, so a `blocked`-keyed
release would announce a dispatch the server then refuses 409 `wave_not_integrated`. The coverage predicate is the ONE
shared `backend/internal/wavecoverage` function the sweeper's short-circuit and the host-dispatch marker's admission
also use — a duplicated reconstruction is the drift class this repo already names as load-bearing.

### Concurrent child amendments are SERIAL BY CONTRACT

Several children can have open amendment windows at once, and `await_children` surfaces exactly **one** amendment per
release. The selection is **deterministic**, not implementation-defined:

- children are examined in **ascending slice index**, ties broken by run id so the order is total and stable;
- within a child, the **oldest** pending request wins — the endpoint returns items oldest-first, so that is the one
  closest to expiring.

The loop is: await releases on one amendment → decide it → re-invoke await, which releases on the next pending one →
repeat until it releases `children_dispatchable` or `children_settled`. Because every child's window runs concurrently
while the session is free, serial decisions still land inside their individual windows — which is exactly the property
this change exists to create. `TestAwaitChildren_TwoPendingAmendments_DeterministicSelectionAndReArm` pins it.

### What an expired amendment window actually means ([#2601](https://github.com/kuhlman-labs/fishhawk/issues/2601))

A child whose amendment window elapses with no decision is **UNDECIDED**, not denied.
This document previously claimed the child "proceeds as denied" and ships an inferior fallback; both are retired wording and were **wrong** (#2601).
What actually happens: the request **stays pending** server-side, the runner emits `scope_amendment_undecided`, the
verify-fix reinvoke is **withheld**, **nothing is reverted**, and the agent's only permitted branches are (a) adapt
within the ORIGINAL scope with disclosure, or (b) stop and fail loud. A late decision is still honored on a
`fishhawk_retry_stage` of the same stage.

The **post-hoc** amendment surfacing this document previously described (`pending_amendments[]`,
`pending_amendment_children`, and the three terminal-state-accurate recovery warnings) has been **removed** along with
its code: it existed to mitigate a timeout that no longer occurs on this path, and it scanned an event stream a
detached child does not return.


### Decomposed-parent observability (`children_status`, [#1147](https://github.com/kuhlman-labs/fishhawk/issues/1147))

For a **decomposed parent**, `fishhawk_get_run_status` carries a `children_status` block so the operator sees the fan-out's live progress instead of a bare `awaiting_children`:

- `children[]` — one entry per discovered child (`{run_id, slice_index, state, depends_on, blocked, blocked_by}`) in `plan_decomposed` (slice-index) order. `state` is the child run's lifecycle state (`pending`/`running`/`succeeded`/`failed`) or `unknown` when that child's read failed. Aggregate counts (`total`/`pending`/`running`/`succeeded`/`failed`) accompany it. **`depends_on`/`blocked`/`blocked_by` (E48.99 / #2546)** answer "what may I dispatch next" in one read: `depends_on` mirrors each child's `slice_depends_on` (from the parent plan's `decomposition`), and a second pass (keyed by SLICE INDEX, not slice position, so a non-dense `child_run_ids` never mis-associates a dependency) sets `blocked=true` with `blocked_by` naming the run ids of any dependency slice not yet `succeeded` — an `unknown`-state dependency counts as blocking, never as dispatchable. A dependency slice with NO minted sibling (absent from `child_run_ids`) ALSO counts as blocking — the read-side mirror of the host-dispatch guard's `not_minted` refusal, so the view never advertises a dispatch the backend would 409 `dependency_not_satisfied`; it has no run id, so `blocked_by` names it with a synthetic `slice N (not_minted)` marker. A wave-0 child (or a legacy backend that omits `slice_depends_on`) decodes to `depends_on=nil`, `blocked=false`, rendering exactly as before the field existed. This is the read-side companion to the host-dispatch wave-order guard (`backend/internal/server/decomposition_dispatch_guard.go`): the guard REFUSES an out-of-order individual dispatch `409 dependency_not_satisfied`, and the MCP api client annotates that refusal ONCE in `apiClient.HostDispatchStage` as a deliberate ordering refusal (so all three host-spawn verbs inherit it) pointing back here.
- `integration_phase` — the fan-in phase classified from the `slices_integrated` / `slice_integration_conflict` audit kinds (ADR-041 / #1142): `running_children` (a child is still in flight), `ready_to_integrate` (all children succeeded, no fan-in yet), `integrated` (a clean fan-in — `consolidated_branch` is surfaced), or `integration_conflict` (a slice branch failed to merge — `conflicting_child_run_id` is surfaced).
- **Best-effort:** a per-child read failure degrades that child to `state="unknown"` and never fails the snapshot.
- **Cost-gated:** the per-child fetch runs only for a top-level run (no `parent_run_id`) whose implement stage is `awaiting_children` **or** whose recent-audit window carries a decomposition marker (`plan_decomposed` / `slices_integrated` / `slice_integration_conflict`). An ordinary run makes **zero** extra calls (no `plan_decomposed` read), and the block is omitted for non-decomposed runs. The `next_actions` `implement_awaiting_children` arm points the operator at `fishhawk_run_children` plus this block.

## Server-suggested next actions (`next_actions`, #1024)

Every arm that advertises a `fishhawk_get_run_status` poll uses the SAME derived cadence the corresponding `*_stage_wait_status` carries (E48.62 / [#2489](https://github.com/kuhlman-labs/fishhawk/issues/2489)) — a flat 30s here against a derived 900s there would be a visible contradiction on one snapshot. The two arms that hold no stage of their own keep the bare floor deliberately: `stages_pending` (no stage row exists yet) and `awaiting_children` (the parent's stage is parked; the live progress is on the child runs, each carrying its own prediction and elapsed).

`fishhawk_get_run_status` and the run-terminal `fishhawk_run_stage` result both carry a `next_actions` block — the generalization of `review_action_hint` (#777/#860) across the whole run lifecycle. The classifier (`next_actions.go`) is a pure function over data the tools already fetch (run row, stage rows, review statuses, the computed hint, the drive read view): no extra backend round-trip, no new endpoint.

Shape: `{state, actions[]}`. `state` is the classified lifecycle state (`plan_gate_parked`, `implement_failed_category_b`, `implement_concerns_open`, `succeeded_pr_open`, `succeeded_merged`, …; terminal runs name the run state with no actions; an unmatched non-terminal state classifies `unclassified`). Each action entry carries:

- `action` — the tool to call (`fishhawk_resume_run`, `fishhawk_fixup_stage`, …) or a named ritual step outside the MCP surface (`approve_pr`, `merge_pr`, `post_merge`, `merge_and_file_follow_up`, `file_product_issue`);
- `params` — key parameters (`run_id`, `stage_id`, `parent_run_id`, the `concern_ids` source);
- `precondition` — when the action is legal;
- `consumes` — what taking it spends: `none` | `fixup_budget` | `retry_budget` | `approval_slot` | `new_run`;
- `reason` — one-line why-this-now.

Invariants:

- **Display-only, never gates** — like the periodic-budget block and the hint it generalizes, the block is advisory; the server-side applicability predicates stay authoritative.
- **A matched failure carries a recovery hint** (E22.X / [#1703](https://github.com/kuhlman-labs/fishhawk/issues/1703)). See [The failure-signature block](#the-failure-signature-block-next_actionssignature-1703) below.
- **A non-terminal run always carries ≥ 1 action.** Any state the table does not match falls back to `unclassified` (re-poll + file a product issue naming the state), structurally — never an empty list.
- **The concern arm derives from the hint computation** (`ReviewActionHint.suggestedActions`), so `review_action_hint` and `next_actions` cannot disagree on the remaining fix-up budget or override availability.
- **Drive folds first**: on drive-enabled runs the `drive_status.next_action` is prepended, so drive and `next_actions` never point different ways.
- **Decomposed parent at `awaiting_children`** (#1147) classifies `implement_awaiting_children` — a dedicated arm offering `fishhawk_run_children` (fan out the still-pending children) plus a poll pointing at the `children_status` block for each child's live state and the fan-in/integration phase.
- **A delegating deploy stage classifies per its state** (E23.13/E23.15 / [#1429](https://github.com/kuhlman-labs/fishhawk/issues/1429) / [#1432](https://github.com/kuhlman-labs/fishhawk/issues/1432)). A standalone release run's deploy stage at `awaiting_deploy_approval` classifies `deploy_gate_parked` and offers `fishhawk_approve_deploy` (carrying the required `environment` param and a precondition naming the `write:deploy` scope + the `--environment=<allowed_environments>` requirement) plus a `fishhawk_reject_deploy` counterpart — **not** the older `fishhawk_approve_plan` hint, which errors on a plan-less release run. Once approved, the backend triggers the external pipeline and the run classifies `deploy_in_flight` (poll until the deploy stage settles). See [Deploy-gate approval](#deploy-gate-approval-fishhawk_approve_deploy-fishhawk_reject_deploy-1432).
- **An acceptance stage gates the merge** (E31.9 / [ADR-049](https://github.com/kuhlman-labs/fishhawk/issues/1519)). When the workflow declares an acceptance stage, the settled-implement path branches to it **before** the merge ritual, reusing the existing verbs — no new tool (the registry stays at 39). Arms: a non-terminal acceptance stage classifies `acceptance_pending` (offering `fishhawk_dispatch_stage` first — non-blocking, since acceptance runs long against the customer-provisioned preview — with `fishhawk_run_stage` as the blocking opt-in; `github_actions` runs get a poll) or `acceptance_running` (poll); a succeeded stage with verdict `passed` classifies `acceptance_passed` and returns the merge ritual (ADR-049 decision #6: the merge is gated on the `acceptance_passed` evidence condition); a succeeded stage with the SERVER-INTERNAL verdict `not_validated` ([#2347](https://github.com/kuhlman-labs/fishhawk/issues/2347)) classifies **`acceptance_not_validated`** and returns the SAME merge ritual — the orchestrator short-circuited the stage pre-spawn, so it verified ZERO criteria with no runner and no preview, and the run is merge-eligible (a change with no live target must not be stranded) but is emphatically not a validated pass; the arm's reason tells the operator zero criteria were verified and asks them to acknowledge that in their merge verdict, which is a **prompt, not an enforcement** (text-matching operator prose to gate a merge would strand runs over wording, a worse failure than the dishonest word being removed) — the terminal-run twin arm is `succeeded_acceptance_not_validated`, and both the reason's load-bearing claims are pinned by `next_actions_test.go` so a refactor cannot silently drop the acknowledgement ask; a failed verdict whose deterministic-triage disposition is a **paged** variant (`paged` / `rerun_budget_exhausted` / `fixup_unavailable_paged` / `retry_unavailable_paged` / `unsettled_paged`) classifies `acceptance_triage_paged` (read the evidence, then arbitrate: manual `fishhawk_fixup_stage`, `merge_and_file_follow_up`, or `fishhawk_cancel_run`); an **auto-routed** disposition (`fixup_dispatched` / `retry_dispatched`) re-opens the implement/acceptance stage server-side, so the next snapshot's existing stage-state arm serves it (`acceptance_triage_rerouting` is the transient poll in between). A settled stage whose verdict is not in the recent-audit window classifies the **defensive** `acceptance_settled_outcome_unknown` (point at `fishhawk_list_audit`; **never** the merge ritual — fail toward read, not toward merge). That arm also offers the **`fishhawk_retry_stage` settled-outcome-unknown recovery** (E31.16 / [#1567](https://github.com/kuhlman-labs/fishhawk/issues/1567)): once `fishhawk_list_audit` confirms no `acceptance_outcome_recorded` verdict exists for the stage (the agent shipped a non-schema field and the verdict failed closed), retrying the acceptance stage re-opens it `succeeded → pending` for a re-run (operator token only; the server 422s if a verdict IS recorded) — the reopen lands the stage in pending so the `acceptance_pending` arm's `fishhawk_dispatch_stage` then serves the actual re-run. **Exception — the out-of-scope skip disposition** (E38.3 / [#1877](https://github.com/kuhlman-labs/fishhawk/issues/1877)): a succeeded verdict-less stage whose recent-audit window carries the `acceptance_skipped_out_of_scope` marker classifies the **pre-succeeded** `acceptance_skipped_out_of_scope` state and returns the merge ritual — the orchestrator auto-terminated a degenerate stage (`verification.out_of_scope`, zero `acceptance_criteria`), a legitimate terminal disposition equivalent to a recorded outcome, so the run is merge-eligible and no verdict was recorded **by design**. This is checked **before** the `acceptance_settled_outcome_unknown` arm, so the futile `fishhawk_retry_stage` reopen arm is **not** offered for the skip (the server also `422 retry_not_applicable`s a direct retry against a skip-marked stage — a reopen would re-fire the same skip). When the marker ages out of the recent-audit window the flag is false and the arm degrades gracefully to the read-first `acceptance_settled_outcome_unknown` arm (fail toward read, never toward merge). The verdict/disposition vocabulary is **mirrored, not imported** from `backend/internal/server/acceptance.go` (the #875 compile trap), pinned by a literal-table test. A FAILED verdict leaves the STAGE `succeeded`, so the classifier reads the `acceptance_outcome_recorded` / `acceptance_triage_decided` audit payloads, never the stage state.
- **An out-of-scope plan auto-terminates the acceptance stage** (E38.3 / [#1657](https://github.com/kuhlman-labs/fishhawk/issues/1657)). When the approved plan declares `verification.out_of_scope` with **zero** `acceptance_criteria`, the acceptance stage has no observable criterion to validate, so the orchestrator walks it straight to `succeeded` and emits an `acceptance_skipped_out_of_scope` audit marker (rather than waiting for an operator to dispatch a degenerate no-observable-change stage). A succeeded run with an open PR whose recent-audit window carries that marker classifies **`succeeded_acceptance_skipped_out_of_scope`** — the full `approve_pr` → `merge_pr` → `post_merge` merge ritual, still **merge-eligible**; only the state label changes so the operator knows why no acceptance verdict was recorded. If the marker ages out of the recent-audit window the arm degrades gracefully to plain `succeeded_pr_open` (itself merge-eligible). `fishhawk_audit_complete` exempts the marked stage from its trace-required rule (the auto-terminated stage runs no agent and ships no trace).
- **The run lifecycle owns its post-merge tail** ([#1370](https://github.com/kuhlman-labs/fishhawk/issues/1370)). A succeeded run with an open PR URL classifies `succeeded_pr_open` (the full `approve_pr` → `merge_pr` → `post_merge` merge ritual) **until** the backend observes the PR merge resolve: `resolveReviewStageOnMerge` emits a `post_merge_observed` audit row alongside the `pr_merged` / `run_merged` board move (from both the `pull_request.closed` webhook and the merge-reconciler poll, which share that path). `get_run_status` reads that entry off the recent-audit slice it already fetches and reclassifies the run `succeeded_merged` — dropping the now-completed `approve_pr` / `merge_pr` steps and surfacing **only** the operator `post_merge` dev-host step (the `scripts/dev post-merge` rebuild/reload stays an operator/deploy concern, [ADR-038](https://github.com/kuhlman-labs/fishhawk/issues/925)). So a merged run's tail state is owned and observable in `get_run_status` rather than implicit in whether the operator ran the script. (`fishhawk_run_stage`'s run-terminal `next_actions` never observes a post-merge — its PR is not open at stage exit — so it always passes `mergeObserved=false`.)

### The failure-signature block (`next_actions.signature`, #1703)

When the run carries a failed stage, `next_actions` gains an **additive** `signature` block: the [failure-signature registry](../failuresig/README.md)'s match on that stage's evidence, naming what the failure MEANS and the recommended recovery sequence. It converts operator memory — a failure string or counter shape, and the playbook that follows from it — into product behaviour, so an operator with no dogfood history gets the recovery the product already had the evidence to produce.

Shape: `{registry_version, id, title, means, playbook[]}`. The published catalog, one section per `id`, is [`docs/architecture/failure-signatures.md`](../../../docs/architecture/failure-signatures.md).

Invariants:

- **Fail-open.** No match ⇒ no block, and every other field is byte-identical to what it would be without the registry. `TestNextActions_UnmatchedFailureBehavesExactlyAsToday` pins the state, action names and retry reason against their pre-registry LITERALS (not against a second call of the same function, which would run the fold on both sides and hide a regression).
- **Never mutates actions.** `foldFailureSignature` ONLY assigns `na.Signature` — it never appends to, reorders, or rewrites `na.Actions`. `TestNextActions_SignatureNeverMutatesActions` drives every catalog entry through the fold against a pristine sentinel action list built in the test, so a fold that prepended a playbook step goes RED. This is an advisory surface; a hint that changed the operator's legal next moves would be a bug.
- **Constant-size.** `failuresig.Hint` echoes only registry-owned strings — never the failure reason — which is what licenses classifying `next_actions.signature` **`tierNever`** in the response byte ladder (`bound.go`). The diagnosis skeleton and the constant-size floor both rebuild `next_actions` without it, so neither is enlarged.
- **Folded on BOTH return paths** — the general classifier and the `ci_failed` early return — and AFTER the structural empty-actions guard, so that guard keeps measuring the classifier's own actions.
- **The diagnosis stage is the FIRST stage in state `failed`** (stages are sequence-ordered, so the earliest failure explains the run). A nil run, an empty stage slice, or no failed stage yields no block.

**Single source for the failure-reason literals.** `externalAPIReasonPhrase`, `quotaUnavailableReasonPhrase`, `sliceIntegrationConflictReasonPrefix` and `flakeTraceEvents` are now SOURCED FROM `failuresig.Anchor*` rather than declared here. Two literals for one contract would drift silently — the hint stops matching while the surrounding action still fires, presenting to the operator as "no signature matched", which is the worst failure shape this feature has. `TestFailureSignatureAnchorsMatchNextActionsPhrases` fails if a local re-declaration ever diverges.

## Deploy-gate approval (`fishhawk_approve_deploy` / `fishhawk_reject_deploy`, [#1432](https://github.com/kuhlman-labs/fishhawk/issues/1432))

`fishhawk_approve_deploy` and `fishhawk_reject_deploy` (E23.15) are the deploy-gate counterparts to `fishhawk_approve_plan` / `fishhawk_reject_plan`. After [#1429](https://github.com/kuhlman-labs/fishhawk/issues/1429) advances a release run's deploy stage to `awaiting_deploy_approval`, the operator loop needs a verb that targets the deploy gate: `fishhawk_approve_plan` resolves a `type=plan` stage first and errors `no plan stage on run …` on a plan-less release run before reaching the approval endpoint. Both new tools take a **run id** and resolve the `type=deploy` stage internally (`resolveDeployStage`, the deploy analogue of `resolvePlanStage`), then `POST` to the existing `/v0/stages/{id}/approvals` endpoint — no new backend, REST, or client-transport surface.

A deploy stage's gate is **pre-execution** ([ADR-038](https://github.com/kuhlman-labs/fishhawk/issues/925): a deploy stage's effect IS the side effect), so approving triggers the external pipeline — a production deploy pages the human regardless of runner kind.

- **`fishhawk_approve_deploy`** requires an operator token with **`write:deploy`** (ADR-038/#1390) and a **required `environment`** that must be one of the deploy stage's `allowed_environments`. The environment is conveyed to the backend **only through the approval comment** — the deploy pre-flight's `parseEnvironmentFlag` scans whitespace-delimited tokens for `--environment=<env>` (there is no structured environment field on the approval request body), so the tool composes `--environment=<environment>` into the comment. An optional `override_freeze` flag appends a standalone `--override-freeze` token (which the backend's `commentHasFlag` matches exactly) so a deploy during a spec-declared `change_freeze` is permitted. A trimmed `reason` is appended after the flags. An empty `environment` fails locally before the HTTP hop. Because the backend pre-flight parses flag tokens from the **whole** comment, the tool guards against flag smuggling (#1432): `environment` must be a single whitespace-free token (rejecting e.g. `production --override-freeze`), and `reason` must not carry a standalone `--override-freeze` token unless `override_freeze` is set — so `--override-freeze` appears in the comment **only** when the operator requested it. Both checks fail locally before the HTTP hop. Backend pre-flight refusals surface as typed tool errors: `422 deploy_environment_not_allowed` (absent / disallowed environment), `422 deploy_change_freeze_active` (freeze active, no override), `422 deploy_upstream_not_satisfied` (a `required_upstream` signal — `ci_green` / `review_merged` — not met), and a `403` when the token lacks `write:deploy`.
- **`fishhawk_reject_deploy`** mirrors `fishhawk_reject_plan`: a deploy reject routes through the backend `advanceStage` path (not the approve-only deploy pre-flight block), so it needs **neither `write:deploy` scope nor an environment**. The reason is recorded on the approval row as `comment`.

The `next_actions` `deploy_gate_parked` arm points at `fishhawk_approve_deploy` (with the `environment` param and a precondition naming the `write:deploy` + `--environment` requirement) plus a `fishhawk_reject_deploy` action. Duplicate-submission labeling (#986) applies to both, identical to the plan-gate tools. Deploy **rollback** is out of scope here — there is no rollback approval endpoint; the CLI `fishhawk deploy rollback` already exists and a rollback verb is a separate follow-up.

> **Operability note (stale local MCP token).** A local dev MCP server's `FISHHAWK_API_TOKEN` may have been issued before E23.10 added `write:deploy` to the operator default scope set, so `fishhawk_approve_deploy` returns `403 insufficient_scope` (`required_scope: write:deploy`) even for the operator. `operatorDefaultScopes` now includes `write:deploy`, so re-issuing the MCP token with the default scope set (`fishhawkd token issue --subject <s>`) fixes it.

**`fishhawk_start_run` — `upstream_run_id` input field (E23.11 / #1417 / #1490).** When minting a standalone deploy-only `release` run whose deploy stage carries a `required_upstream` pre-flight gate, pass `upstream_run_id` (a UUID string) to name the upstream `feature_change` run whose `ci_green` / `review_merged` the gate evaluates. Omit for all other run types — a non-deploy-gate run ignores the field. This is DISTINCT from `parent_run_id` (a follow-up/lineage link): `upstream_run_id` is a deploy-gate safety reference only, so it carries none of the `get_plan`-resolution / resume-retry / decomposition-provenance semantics `parent_run_id` does. The value is validated locally as a well-formed UUID before the HTTP hop; a malformed value surfaces a clean tool error without a backend round-trip. The echoed `upstream_run_id` on the returned Run confirms the value round-tripped correctly. The CLI mirror is `fishhawk run start --upstream-run-id <uuid>`; both surfaces validate locally.

## Implement-review fix-up (`fishhawk_fixup_stage`)

`fishhawk_fixup_stage` (E22.X / [#762](https://github.com/kuhlman-labs/fishhawk/issues/762)) routes one or more **advisory implement-review concerns** ([ADR-027](https://github.com/kuhlman-labs/fishhawk/issues/703) `approve_with_concerns`) back to the implement agent for a single fix-up pass, instead of the operator hand-editing the PR branch. It wraps `POST /v0/stages/{stage_id}/fixup`.

**Reviewer evidence on the concern surfaces ([#2353](https://github.com/kuhlman-labs/fishhawk/issues/2353)).** A reviewer concern may carry `new_evidence` (the substantiation behind `note`) and `settled_ref` (the re-raise lineage tag, #1913). Both are now decoded and surfaced, and the surfaces split by SOURCE, which is what decides how far back the fix reaches:

- `PlanReviewConcern` — ONE struct backing `fishhawk_await_review`, `fishhawk_get_plan`'s `reviews[]`, and `fishhawk_get_run_status`'s `implement_reviews[]`. It decodes the `plan_reviewed` / `implement_reviewed` audit payload directly, so these three carry the evidence **RETROACTIVELY**, including for runs that predate migration 0069.
- `GateViewConcern` / `GateViewSettledConcern` — the `fishhawk_get_gate_view` mirrors. These read PERSISTED concern rows, so they carry evidence only for concerns minted after 0069. Their json tags MUST byte-match the backend's `gateViewConcern` / `gateViewSettledConcern`: a mismatched tag is SILENT (an empty field, never a decode error), which is why the wire test drives raw server-shaped JSON rather than a marshalled client struct.

Both fields are `omitempty` on every surface — the common no-evidence concern leaves the payload byte-identical.

**Fix-up arguments (`FixupStageInput`).**

| Argument | Meaning |
|---|---|
| `concern_ids` | PRIMARY addressing: stable concern UUIDs from `fishhawk_get_run_status`'s `run.concerns.items[].id`. |
| `concerns` | DEPRECATED positional indices; valid only when `concern_ids` is absent. |
| `reason` | Operator rationale, recorded on `stage_fixup_triggered` and as the routed concerns' `state_reason`. |
| `allow_create` | Net-new paths this pass may CREATE ([#823](https://github.com/kuhlman-labs/fishhawk/issues/823)). |
| `force_additional_pass` | Bounded override: ONE pass past the budget, hard-capped at 3 total. |
| `implement_model` | Per-pass model override; empty inherits the run's resolved implement model. |
| `operator_concern` | Free-text binding instruction with NO pre-existing review concern ([#1311](https://github.com/kuhlman-labs/fishhawk/issues/1311)), minted as a durable tracked concern (#2623). |
| `operator_evidence` | Declares YOU executed a reproduction of the routed concern(s) ([#2551](https://github.com/kuhlman-labs/fishhawk/issues/2551)). **Authority, not prose** — the reproduction text still travels in `reason`/`operator_concern`. Every concern routed by the pass becomes EXEMPT from reviewer-confirmation auto-resolve: a later `confirmed` delta-verification verdict is vetoed (`operator_evidence_routed`) and the concern stays open until an operator waive/defer or a genuine fix. **PERMANENT** for those concerns, so it raises the waive burden — use it when a reviewer has already retired a defect you can still reproduce. NOT a selection input; whitespace-only or >4000 bytes → 400 `validation_failed`. |

**Gate-view disputes ([#2551](https://github.com/kuhlman-labs/fishhawk/issues/2551)).** `GateViewConcern` mirrors the backend's `disputed` (bool) and `disputes[]` (`gateViewDispute`: `{sequence, round, veto_reason, resolution, confirming_reviewer_model, raising_reviewer_model, note}`, `veto_reason` one of `raiser_rejected_same_round` | `operator_evidence_routed` | `fixup_pass_no_changes` | `evidence_lookup_failed`). `disputed` means a `confirmed` resolution was recorded on a concern that is STILL OPEN — the same-round raiser-reject / peer-confirm split. The backend derives it from the durable concern row, so `disputed` can be true with an EMPTY `disputes[]` when the best-effort veto record did not land; treat `disputes[]` as detail, never as the presence test. Same json-tag byte-match rule as above — the wire test drives raw server-shaped JSON. `gateViewDispute` is deliberately unexported (the exported surface is pinned by `exportBaseline`); the MCP schema reflection walks it through the exported `Disputes` field.

**Run-status concerns block `short_summary` ([#2488](https://github.com/kuhlman-labs/fishhawk/issues/2488)).** Each item in `fishhawk_get_run_status`'s `run.concerns.items[]` (mirror `RunConcernItem`) now carries a `short_summary` — a **bounded** note-derived recognition label (at most 100 bytes, whitespace-collapsed to one line, `...`-marked when the note is cut, equal to the whole collapsed note when it already fits, **absent** when the note is blank after collapsing). It lets ONE default `fishhawk_get_run_status` call map each concern `id` to a recognisable defect without a second `fishhawk_list_audit` and correlation by array ordering. It is present **regardless of `include_review_prose`** (like verdicts/severities/concern keys). It is **not a unique key** — two concerns whose notes share a long prefix may share a label — so route fix-ups by `id`; the full untruncated note stays on `fishhawk_get_gate_view` and the originating `*_reviewed` audit entry.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `stage_id` | **yes** | The implement stage parked at the review gate. |
| `concern_ids` | one of `concern_ids`/`concerns` | **Primary addressing ([#964](https://github.com/kuhlman-labs/fishhawk/issues/964))** — stable concern UUIDs to route back (at least one). Read them from `fishhawk_get_run_status`'s `run.concerns.items[].id`. Only this stage's **open** implement-stage concerns resolve; an unknown, foreign, plan-stage, or already-resolved ID is `validation_failed`. Routed concerns are marked `addressed_pending` (with `reason` as `state_reason`) in the durable concern store. |
| `concerns` | one of `concern_ids`/`concerns` | **Deprecated positional fallback** — indices into the stage's flattened `implement_reviewed` concern set. Ambiguous once multiple heterogeneous review entries exist per stage; prefer `concern_ids`. Only valid when `concern_ids` is absent — supplying both is `validation_failed`. |
| `reason` | no | Operator note, recorded on the `stage_fixup_triggered` audit entry (and as the routed concerns' `state_reason` on the ID path). |
| `allow_create` | no | Repo-relative paths this fix-up will **create** ([#823](https://github.com/kuhlman-labs/fishhawk/issues/823)). See below. |

**Declaring net-new files (`allow_create`)** — a concern that requires a *new* file needs `allow_create`. Each declared path is folded into the implement stage's **effective `scope.files` for THIS fix-up pass only** (never the persisted plan scope), reusing the same [#824](https://github.com/kuhlman-labs/fishhawk/issues/824) `foldScopePaths` machinery `add_scope_files` uses. Because the runner's created-out-of-scope gate ([#818](https://github.com/kuhlman-labs/fishhawk/issues/818)) keys off that effective union, folding the path in makes the runner stage the new file so the gate stops tripping for it. The pass is bounded and operator-authorized: a fix-up only happens when the operator calls this verb, and `allow_create` widens the legitimate set only by the paths the operator names. **Preserved contract:** any created file **NOT** declared here still fails category-B per #818 — declaring paths does not reopen the silent-strip hole. Entries must be repo-relative; an absolute path or one containing `..` is rejected (`validation_failed`, 400, `field: allow_create`). The OpenAPI/`v0.md` surface remains the authoritative parameter reference.

What a fix-up does — and how it differs from `fishhawk_retry_stage`:

- The selected concerns are delivered to the agent as **binding instructions** (the [#558](https://github.com/kuhlman-labs/fishhawk/issues/558) condition-delivery framing: MANDATORY, win on conflict).
- The agent commits onto the **same PR branch** and the existing PR is **updated** — a fix-up does **not** regenerate a fresh diff or open a new PR. (`retry` re-opens a *failed* stage and regenerates; fix-up re-opens a *healthy* review gate.)
- The implement review re-runs on the result.
- On success the stage flips `awaiting_approval → pending` (the orchestrator advances it to `dispatched`, re-firing `workflow_dispatch`); the tool returns the re-opened stage.

**Operator-gated and bounded — this is never an unbounded auto-loop:**

- The bound defaults to **one pass per stage**, enforced server-side by counting prior `stage_fixup_triggered` audit entries. A second attempt once the bound is spent returns a `fixup_budget_exhausted` tool error (its details carry `max_passes` + `used`). The remaining budget is `max − fix-ups already triggered`, surfaced on the audit entry's `remaining_budget` field (read it via `fishhawk_list_audit`); the success response itself carries only the re-opened stage.
- **No-change refund ([#967](https://github.com/kuhlman-labs/fishhawk/issues/967)):** a pass whose re-dispatch produced **no commit** (the `fishhawk_run_stage` result carries `fixup_no_changes: true`; a `fixup_no_changes` audit entry exists for the stage) is refunded against the **normal** budget, so the next trigger is admitted without `force_additional_pass`. The refund **never** extends the absolute 3-pass ceiling, which counts every triggered pass including refunded ones (`refunded_passes` on the `stage_fixup_triggered` audit entry records the refund).
- **Delivered-nothing infra refund ([#1957](https://github.com/kuhlman-labs/fishhawk/issues/1957)):** a fix-up pass that died **category-C (infrastructure)** without landing anything on the PR branch is likewise refunded against the **normal** budget under the delivered-nothing invariant — whether it died **before** the agent ran (a `dispatch_reaper_failed` audit entry with `failure_category` `C`, the #1747 spawn-phase reaper path) or **after** burning agent work that never pushed (a `stage_fixup_recovered` entry with `source_failure_category` `C`, the #788 recovery path), as long as the signal falls inside the pass's `stage_fixup_triggered` window. Category **A** (agent) and **B** (policy) failures still consume budget. Like the no-change refund, it **never** extends the absolute 3-pass raw ceiling, which keeps counting every triggered pass. The `review_action_hint` on `fishhawk_get_run_status` mirrors this refund so its `remaining_fixup_budget` / `override_available` agree with the backend's admit decision (it must not steer the operator at a needless `force_additional_pass`).
- **Operator owns the trigger and the merge.** A fix-up only ever happens when an operator calls this verb; the agent cannot self-trigger one, and the operator still approves the final merge.
- **Auth:** a write tool requiring `write:stages` (or the dedicated `write:fixups`) scope. A run-bound token may fix up only stages **within its own run** — a cross-run target returns `cross_run_fixup` (403).

Error surfaces propagated as tool errors: `validation_failed` (400, no concern selection / both `concern_ids` and `concerns` supplied / out-of-range index / unknown, foreign, plan-stage, or non-open `concern_id` — the empty/mixed selections are also caught locally before the HTTP hop), `cross_run_fixup` (403), `stage_not_found` (404), `fixup_not_applicable` (422, no recorded `approve_with_concerns` verdict to route back), `fixup_budget_exhausted` (422).

## Plan-gate revise (`fishhawk_revise_plan`)

`fishhawk_revise_plan` (E22.X / [#1099](https://github.com/kuhlman-labs/fishhawk/issues/1099)) is the **third plan-gate verdict** alongside `fishhawk_approve_plan` and `fishhawk_reject_plan`: it re-plans **in place** in the same run against a binding operator design constraint, instead of approving the plan as-is or rejecting it to a fresh-run replan. It wraps `POST /v0/stages/{stage_id}/revise`. Takes a **run id**; the tool resolves the plan stage internally (the `type=plan` stage, like the approve/reject tools).

Inputs:

| Field | Required | Notes |
|---|---|---|
| `run_id` | **yes** | The run whose plan stage to revise. |
| `constraint` | **yes** | The binding design constraint the planner must revise the prior plan to satisfy. Injected into the re-dispatched plan prompt as a dedicated, binding **"Revision constraint"** section (the [#558](https://github.com/kuhlman-labs/fishhawk/issues/558) condition-delivery framing: MANDATORY, wins on conflict), with the prior plan carried as the **revision base**. Empty constraints are rejected (`validation_failed`, also caught locally before the HTTP hop). |
| `force_additional_pass` | no | Bounded operator override — grant ONE revise pass **beyond** the normal budget when it is already spent (`revise_budget_exhausted`), hard-capped at 3 total passes per stage. The forced pass is audited. |

When to reach for revise vs the alternatives:

- **approve** — the plan is correct as written.
- **revise** — the plan's direction is sound but a design constraint must change first. Cheaper than a reject → fresh-run replan because the prior plan is the revision base and only the constrained parts change; the operator's design intent reaches the agent through the same binding channel as approval conditions.
- **reject** — the plan takes a wrong fork no constraint can amend.

What a revise does — and how it differs from `fishhawk_reject_plan`:

- The constraint is delivered to the planner as a **binding** instruction in a dedicated "Revision constraint" prompt section — never under the clarification-answers or approval-conditions heading — and the prior plan rides as the revision base so the planner **revises** rather than replanning blank-slate.
- On success the plan stage flips `awaiting_approval → pending` (the orchestrator advances it to `dispatched`, re-firing `workflow_dispatch`); the run re-enters the normal plan **review → approve** gate. (`reject` fails the gate as category D and the next step is a fresh run.)

**Operator-gated and bounded — this is never an unbounded auto-loop:**

- The bound defaults to **one pass per stage**, enforced server-side by counting prior `plan_revised` audit entries (no dedicated column — exactly as fix-up counts `stage_fixup_triggered`). A second attempt once the bound is spent returns a `revise_budget_exhausted` tool error (details carry `max_passes` + `used`); the operator may grant ONE more pass with `force_additional_pass=true`, hard-capped at 3 total passes. At the ceiling the tool returns the distinct `revise_ceiling_reached` error (a hard stop — reject and start a fresh run).
- **Auth:** a write tool requiring `write:approvals` scope (the #558 binding-conditions / gate-answer family). A run-bound token may revise only stages **within its own run** — a cross-run target returns `cross_run_revise` (403).

**Scope-regression refusal ([#2516](https://github.com/kuhlman-labs/fishhawk/issues/2516)).** A revise regenerates the WHOLE plan artifact, so a narrowly-scoped constraint can drop files the prior plan scoped. That drop is neither silent nor accepted:

- The plan gate diffs the new plan's scoped paths (top-level ∪ decomposition sub-plan ∪ `split_proposal` phase scopes) against the revision base and **REFUSES** an **undeclared** narrowing — the narrowed plan never reaches review or the approval gate. The plan stage is re-dispatched **once** with the enumerated carry-forward path set and the dropped-file list, so **ZERO** reviewer passes and **ZERO** revise passes are spent (the pain #2516 describes is that today the drop is caught only *after* the pass is spent, and the only remedy is another revise that can drop again).
- A narrowing **IS** admitted when the plan declares it in the top-level `scope_removals` array (`{path, reason}` — see `docs/spec/plan-standard-v1.md`). The reason rides in the plan artifact reviewers read, so a bogus justification is challengeable rather than silent; the refusal itself stays computed from the machine diff.
- **Residual operator-facing consequence:** the refusal budget is ONE pass per run. Once spent, a further undeclared narrowing degrades to the prior behaviour — the plan reaches the gate carrying the regression evidence, and the pass does NOT consume the normal revise budget, so the operator still gets a free recovery pass. The hard ceiling counts every pass either way.
- The tool description pins this contract (`TestRevisePlanDescription_DocumentsScopeRefusal`), so it cannot silently drift back to describing the old silent-drop behaviour.

Error surfaces propagated as tool errors: `validation_failed` (400, empty constraint), `cross_run_revise` (403), `stage_not_found` (404), `revise_not_applicable` (409, the stage is not a plan stage parked at `awaiting_approval`), `revise_budget_exhausted` (409), `revise_ceiling_reached` (409). The OpenAPI/`v0.md` surface remains the authoritative parameter reference.

## Concern waiver (`fishhawk_waive_concern`)

`fishhawk_waive_concern` (E22.X / [#984](https://github.com/kuhlman-labs/fishhawk/issues/984)) waives one **open** review concern (`raised`, `addressed_pending`, or `reopened`) with a **required, audited reason** — the operator judgment that the concern does not warrant a change (false positive, accepted trade-off, deliberate deferral), as distinct from `fishhawk_fixup_stage` (route the concern back to the agent). It wraps `POST /v0/concerns/{concern_id}/waive`.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `concern_id` | **yes** | The stable concern UUID, from `fishhawk_get_run_status`'s `run.concerns.items[].id`. |
| `reason` | **yes** | Audited rationale. Recorded on the `concern_waived` audit entry **before** the state change (append failure → `audit_append_failed`, no mutation), stored as the concern's `state_reason`, and rendered **verbatim** in later re-review prompts as the not-re-litigable waive context — make it self-contained. |

What a waive does:

- The concern transitions to the **terminal** `waived` state: it leaves `run.concerns` (the open block), can no longer be selected by `fishhawk_fixup_stage`'s `concern_ids`, and later re-reviews of the stage see it as context that must **not** be re-litigated absent new evidence.
- There is **no un-waive**. If the concern turns out to matter, a new concern from a later review is the path back.
- **Auth:** same write-scope pair as fix-up (`write:stages` or `write:fixups`); a run-bound token may waive only its own run's concerns (`cross_run_waive`, 403).

Error surfaces propagated as tool errors: `validation_failed` (400 — empty reason / bad UUID, both also caught locally before the HTTP hop), `cross_run_waive` (403), `concern_not_found` (404), `concern_waive_conflict` (422 — the concern is already `waived`/`superseded`/`addressed`; details carry the rejected `from`/`to` pair), `concern_store_unconfigured` (503).

## Concern defer (`fishhawk_defer_concern`)

`fishhawk_defer_concern` (E22.X / [#1202](https://github.com/kuhlman-labs/fishhawk/issues/1202)) converts one **open** review concern (`raised`, `addressed_pending`, or `reopened`) into a conventions-complete, boarded, epic-linked **follow-up work item** AND transitions the concern to the terminal `deferred` state — in a single call. It is the "not now, but track it" verb, sitting between `fishhawk_fixup_stage` (route the concern back to the agent now) and `fishhawk_waive_concern` (resolve with no follow-up). It consumes **no** fix-up budget. It wraps `POST /v0/concerns/{concern_id}/defer`.

The follow-up body is **auto-drafted** server-side from the concern — its note, severity, category, the reviewer model, the evidence run id, and the source PR link — so you do not hand-author it (the friction this replaces: ~7 hand-authored follow-ups via `fishhawk_file_issue` in one loop session). You supply only the title coordinates the concern cannot carry.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `concern_id` | **yes** | The stable concern UUID, from `fishhawk_get_run_status`'s `run.concerns.items[].id`. |
| `parent_epic` | **yes** | The epic the follow-up rolls up to (an issue reference like `#1196`); its leading `[E<n>]` title token is fetched to derive the `{epic}` placeholder. Operator judgment — not derivable from the concern. |
| `n` | no | The child number for the `[E<epic>.<n>]` title. Discovered server-side (#1958) from the parent epic's existing children (open and closed) and the next one allocated, so you no longer have to guess it — pass `n` only to override. |
| `type` | no | Override the auto-selected work-item type (`bug` for a defect category, else `chore`). |
| `labels` | no | Labels merged on top of the type's default labels. |
| `note` | no | Operator addendum folded into the follow-up body and the concern's `state_reason`. |

What a defer does:

- Files the follow-up work item, then transitions the concern to the **terminal** `deferred` state: it leaves `run.concerns` (the open block), can no longer be selected by `fishhawk_fixup_stage`'s `concern_ids`, and its `state_reason` names the filed issue.
- **Orphan-issue-safe.** An already-resolved concern is rejected **before** any issue is filed (`concern_defer_conflict`, 422). A filing failure leaves the concern **open** (no transition) so you can retry. The success `concern_deferred` audit entry is written only **after** the transition succeeds; a post-filing transition race emits only a corrective `concern_defer_failed` entry (naming the actual state + the orphaned issue url) and returns 422.
- **Auth:** byte-identical to waive — same write-scope pair (`write:stages` or `write:fixups`); a run-bound token may defer only its own run's concerns (`cross_run_defer`, 403).

Returns the filed follow-up issue (`{type, title, number, url, provider, applied_labels}`) and the updated concern row (state `deferred`, `state_reason` naming the issue).

Error surfaces propagated as tool errors: `validation_failed` (400 / bad UUID, caught locally before the HTTP hop), `cross_run_defer` (403), `concern_not_found` (404), `concern_defer_conflict` (422 — non-open concern or a post-filing race), `work_item_invalid` (422), `provider_unimplemented` (501), `work_item_filing_failed` (502 — the concern stays open), `concern_store_unconfigured` (503).

## Run-branch reset (`fishhawk_reset_run_branch`)

`fishhawk_reset_run_branch` ([ADR-035](https://github.com/kuhlman-labs/fishhawk/issues/857) / [#867](https://github.com/kuhlman-labs/fishhawk/issues/867)) is the **destructive, operator-gated** remediation for a foreign commit pushed **ON TOP** of a run's own commits on the open PR branch. It force-rewinds the run/PR branch back to its **last run-authored HEAD** (the newest commit attributable to the run's reported-head ledger), dropping the on-top foreign commit, then re-parks the review gate so CI + the merge reconciler re-evaluate the rewound head. It wraps `POST /v0/runs/{run_id}/reset-branch`.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `run_id` | **yes** | The run whose branch to reset. |
| `confirm` | **yes** | MUST be `true` — the reset is destructive, so it is never silent/auto. A missing/false value is refused (`confirmation_required`, 400; the tool also catches it locally before the HTTP hop). |
| `reason` | no | Operator note, recorded on the `branch_reset` audit entry. |

Safety (all server-enforced):

- **On-top only.** Refused with `reset_out_of_scope` (422) when the foreign commit is an ancestor/interleaved — a reset can't drop it; prevention ([#861](https://github.com/kuhlman-labs/fishhawk/issues/861)/[#865](https://github.com/kuhlman-labs/fishhawk/issues/865)) owns that.
- **Fail-closed.** Any classification uncertainty (unresolvable base ref, incomplete ledger, compare error, no identifiable run-authored HEAD, or a lease re-check that finds a concurrent push) returns `reset_not_determinable` (422) — the destructive action never force-updates on doubt. A clean tip returns `reset_not_applicable` (422).
- **Operator-gated + audited.** Requires `write:runs`; a run-bound token may reset only **its own** run's branch (`cross_run_reset`, 403). Every rewind writes a `branch_reset` audit entry; the dropped commit stays recoverable from the remote reflog / the foreign pusher's own branch (recorded in `recovery_note`).

Returns the rewind summary (`dropped_offending_sha`, `reset_to_sha`, `prior_head_sha`, `recovery_note`) on success.

## Local decomposition fan-in (`fishhawk_consolidate_slices`)

`fishhawk_consolidate_slices` ([E24.2 / ADR-041](https://github.com/kuhlman-labs/fishhawk/issues/857) / [#1238](https://github.com/kuhlman-labs/fishhawk/issues/1238)) runs the **decomposed-parent fan-in** on demand. After a decomposition's children all reach terminal-`succeeded`, the parent implement stage is parked in `awaiting_children` until the fan-in merges every slice branch onto the consolidated branch and opens the consolidated PR. That fan-in normally runs from the 60s **child-completion sweeper** — but the sweeper is **off by default in the local dev `fishhawkd`** ("dev-loop posture"), so on the local runner a settled parent stays parked with no consolidated branch/PR. This verb runs the same fan-in on demand, and (unlike the silent event-driven path) **surfaces** a non-conflict integration error so you can diagnose a stuck local fan-in. It wraps `POST /v0/runs/{run_id}/consolidate`.

> The local dev stack (`scripts/dev up`) now also passes `--enable-child-completion-sweeper`, so the sweeper backstop runs locally too; this verb is the explicit, error-surfacing operator path when you want to drive (or diagnose) the fan-in directly.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `run_id` | **yes** | The decomposed **parent** run whose children's slices should be fanned in. |

Preconditions (each a clean tool error): the run is a decomposed parent (not a child, and it has children — `not_a_decomposed_parent`, 400); its implement stage is parked in `awaiting_children` (`not_awaiting_children`, 409); every child is terminal (`children_in_flight`, 409) and every one succeeded (`children_failed`, 409). Auth is operator `write:runs`; a run-bound token is refused (`agent_token_forbidden`, 403).

Outcomes (200):

- `integrated` — every slice merged cleanly; the parent implement stage resolved `succeeded` and the consolidated PR opened. Carries `consolidated_branch` + `pull_request_url`.
- `slice_conflict` — a slice branch failed to merge; the parent implement stage failed recoverable (category `B`), preserving the E24.2 contract. Carries `conflicting_slice_index` + `conflicting_child_run_id`.

A non-conflict failure returns `slice_integration_error` (502); as a 5xx the cause is redacted from the body and retained in the operator log keyed by `error_ref` (E67.15 / #2587), which the tool surfaces so the operator can correlate — the diagnosability the event-driven fan-in path lacks.

Implementation: `backend/internal/server/consolidate.go::handleConsolidateRun`, registered as the MCP verb in `tools.go`.
It composes the **exported** orchestrator primitives `IntegrateSlices` → `TransitionStage` → `Advance`, mirroring
`childcompletion.resolveParent`'s all-succeeded arm, WITHOUT touching the hot event-driven/sweeper paths. The
`children_settled` and `slice_integration_conflict` audit payloads are byte-identical to the sweeper's, so the
`children_status` integration-phase classifier reports correctly whichever path settled the parent. Where the
event-driven `maybeAdvanceDecomposedParent` WARN-swallows a non-conflict `IntegrateSlices` error (leaving a silent
stuck parent), this endpoint returns it — the 502 `slice_integration_error` above, with the stage left
`awaiting_children` for retry.

## Run-branch vouch (`fishhawk_vouch_commit`)

`fishhawk_vouch_commit` ([ADR-035](https://github.com/kuhlman-labs/fishhawk/issues/857) / [#1044](https://github.com/kuhlman-labs/fishhawk/issues/1044)) is the **operator-gated, audited** provenance path for a foreign commit on a run branch that no loop-native remediation can route — an operator's mechanical remediation commit (e.g. a `scripts/sync-schemas` output pushed onto a decomposition fan-out branch whose children are all terminal with zero open concerns). Unlike `fishhawk_reset_run_branch` (which **drops** an on-top foreign commit), vouch **keeps** the operator commit and **declares it run-authored lineage**: the vouched SHA is unioned into the run's reported-head ledger (on the run's own chain and its decomposition children), so the merge reconciler's ADR-035 re-check attributes it cleanly and the run it fixed is no longer wedged. It wraps `POST /v0/runs/{run_id}/vouch-commit`.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `run_id` | **yes** | The run whose branch carries the commit. |
| `sha` | **yes** | The commit SHA to declare as run-authored lineage. Empty is refused (`validation_failed`, 400; caught locally before the HTTP hop). |
| `reason` | **yes** | Operator rationale, recorded verbatim on the `operator_commit_vouched` audit entry. Empty is refused (`validation_failed`, 400). |

Safety (server-enforced):

- **Fail-closed preserved.** The handler records the declaration verbatim — it does **not** verify the SHA exists on the branch. Vouching a wrong/non-existent SHA un-wedges nothing; an UN-vouched foreign commit still fails category-B at the report boundary and still blocks merge resolution.
- **Operator-token-only.** Requires `write:stages`. A run-bound token (subject `mcp:run:<uuid>`) is **rejected outright** (`run_token_forbidden`, 403) — even for its own run — because an agent self-declaring lineage for a commit on its own branch would defeat the ADR-035 sole-writer invariant. Mirrors the `fishhawk_decide_scope_amendment` run-bound rejection.

Returns the recorded declaration (`run_id`, `vouched_sha`, `reason`) on success.

## One-verb operator merge (`fishhawk_merge_run`)

`fishhawk_merge_run` ([E48.7 / #1954](https://github.com/kuhlman-labs/fishhawk/issues/1954)) takes a **gate-approved run from verdict to merged+terminal in one verb**, replacing the four-step hand ceremony (approve the PR → merge it → run post-merge). Per the 2026-07-15 design decision (option a), the **PR-approval review itself stays a `gh` step under the operator's own GitHub identity** (`gh pr review --approve`); App-identity approval is deferred to E39. It wraps `POST /v0/runs/{run_id}/merge`.

The endpoint records the operator merge verdict as a chained `merge_verdict_recorded` audit entry (modeled on the `operator_commit_vouched` vouch handler) and queues the squash merge through the **same `GitHubMerger` seam** the delegated `may_merge` arm of `AutoDriveRunGate` uses — so `drive_run`'s merge act (which routes through `POST /v0/runs/{run_id}/auto-drive`) converges on the same path by construction. Because GitHub's `enablePullRequestAutoMerge` mutation errors on an already-merge-ready PR (clean status — the common flow after a `gh` approval with checks green), the backend `githubAutoMerger` gains a REST squash-merge fallback.

The tool then **awaits the webhook-settled terminal run state** using the `fishhawk_await_audit` polling idiom: it polls the `pr_merged` / `post_merge_observed` audit categories anchored past the verdict row's sequence, with the ADR-036 run-terminal backstop each tick. There is **no persisted `merged` run state** — terminal-on-merge is `succeeded` and `awaiting_merge` is a presentation-only drive surface — so the await keys on those categories plus the run-terminal backstop, never on a state string.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `run_id` | **yes** | The gate-approved run to merge. It must carry a PR URL and must not be failed/cancelled (fast local refusal before the POST; the backend re-validates authoritatively, including the acceptance gate). |
| `verdict` | **yes** | Your operator merge verdict, recorded verbatim on the `merge_verdict_recorded` audit entry. Empty is refused locally before the HTTP hop. |
| `timeout_seconds` | no | Bounds the terminal await (default 360, cap 600 — same clamp as `await_audit`). |

Idempotence (**endpoint-side, #1954**):

- The tool **always re-POSTs on resume with NO client-side skip.** The ENDPOINT is idempotent: a repeated POST that finds an existing `merge_verdict_recorded` row appends no duplicate, responds `already_recorded:true`, and **still dispatches the merge helper**. So a timed-out re-invoke or a 502 (`merge_dispatch_failed`, the verdict row durable) re-queues the merge with no duplicate verdict row — closing the 502-retry hole a client-side skip would re-open.

Statuses: `merged` (a `pr_merged` / `post_merge_observed` entry landed past the anchor; `next_action` carries the operator post-merge dev-host step, **surfaced not invoked** — ADR-038), `timeout` (resumable — re-invoke; the endpoint's idempotence makes the re-POST safe), `run_terminal` (the run reached failed/cancelled while waiting — the merge will most likely never settle; check `fishhawk_get_run_status`), `checks_pending` (E67.56 / #2717 — the PR's required checks have NOT all passed, so GitHub reports it in unstable status and will not queue the merge within the wall-clock budget).

**In-tool wait on `checks_pending` (E67.56 / #2717).** GitHub's `enablePullRequestAutoMerge` refuses a PR in UNSTABLE `mergeStateStatus`, and the endpoint classifies that distinctly as `409 merge_checks_pending`. The tool does **not** surface it as an error — an immediate retry would reproduce the identical refusal. Instead it re-POSTs on the poll interval within its existing clamped timeout budget (the checks-pending retry loop and the terminal await share ONE deadline, so the tool's wall-clock contract is unchanged) until the endpoint accepts the merge (then falls through to the terminal await with the remaining budget) or the deadline expires. On expiry it returns status `checks_pending` with a message that names the honest precondition: the required checks have not all passed, an immediate retry cannot succeed, and — because UNSTABLE covers a check that has already **FAILED**, not only a pending one — if a required check has failed the merge will never queue, so inspect the PR instead of waiting. The verdict row is recorded and durable throughout (the message carries that claim), but `merge_queued`, `verdict_recorded` and `already_recorded` are all **false** on `checks_pending` — provenance the response cannot establish is not inferred (binding condition 4); `verdict_sequence` is surfaced when the server returned it.

A **write** tool needing `write:approvals`; a run-bound agent token is rejected (`run_token_forbidden`, 403). `next_actions`' merge-ritual states (`succeeded_pr_open`, the acceptance-skipped/passed states, and the drive-folded `awaiting_merge`) now emit `approve_pr` then `fishhawk_merge_run`, replacing the bare `merge_pr` + `post_merge` steps. For the drive-folded `awaiting_merge` action, `driveAction` passes the backend's `next_action.detail` through verbatim as the folded merge action's `reason`, so the operator sees the backend's advisory qualification (any outstanding reviewer rejects / open concerns — [#2487](https://github.com/kuhlman-labs/fishhawk/issues/2487)); the action's precondition no longer makes an unconditional "every gate resolved and required checks green" all-clear claim and instead points the reader at that reason/detail.

## Stranded-stage reap (`fishhawk_reap_stage`, [E67.47 / #2689](https://github.com/kuhlman-labs/fishhawk/issues/2689))

`fishhawk_reap_stage` is a **thin MCP verb over the EXISTING** `POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure`
endpoint, through the existing `apiClient.ReportStageFailure`. No new REST route, no schema change, no server-side
change: the **server keeps authority** over what the ENDPOINT may reap. The handler refuses a terminal stage and the
five protected PARK states (`reapProtectedParkStates` is that closed allow-list), so the **endpoint's** reap authority
spans `{pending, dispatched, running}` — `TestReapStageFailure_PendingAnchorSingleCAS` (server-side) pins that `pending`
is a first-class reapable anchor. A protected-park refusal, a `404 stage_not_found` and a `403 insufficient_scope` all
surface verbatim. **The DETACHED reaper only ever reports from `{dispatched, running}`** because its own runner-side
allow-list (`reapStrandAllowList`, `run_stage.go`) is that narrower set; that is the distinction an older "the reaper's
authority is exactly `{dispatched, running}`" phrasing conflated with the endpoint's own reach.

It exists because a stage stranded in `dispatched`/`running` with no live runner was recoverable **only** by
hand-POSTing that endpoint: `fishhawk_retry_stage` admits only a `failed` stage, `fishhawk_revive_run` only a
terminal-FAILED run, and `fishhawk_fixup_stage` / `fishhawk_dispatch_stage` both refuse. `fishhawk_cancel_run` was a
**trap** on a decomposition child (see below). Two residual strand paths produce that state even after
[#2363](https://github.com/kuhlman-labs/fishhawk/issues/2363)'s option 4(a): `run_children`'s spawn-error compensation
when the category-C report ITSELF fails, and the reaper's `reportDetachedFailureWithRetry` exhausting its bounded
backoff. Recovery sequence: **`fishhawk_reap_stage` → `fishhawk_retry_stage` → re-dispatch**.

### The verb narrows to `{dispatched, running}`: a stable `pending` stage is refused ([E67.52 / #2700](https://github.com/kuhlman-labs/fishhawk/issues/2700))

The endpoint accepts `pending`, but the **verb refuses it** — the endpoint and the verb are deliberately split. A stage
observed in `pending` **has no dispatch in flight for its current attempt**, so there is no live runner to reap and
nothing is stranded — this recovery verb, whose whole purpose is un-wedging a stage whose runner died, has no business
failing a stage that is sitting in the queue. Note the claim is about the CURRENT ATTEMPT, not the stage's lifetime: an
observed `pending` can carry a whole execution history behind it, because `fishhawk_retry_stage` re-parks a
previously-attempted stage back into `pending`. The refusal therefore does **not** say the stage "never started"; it
says its current attempt has no runner to clear.

- **What fires it:** the observed pre-classification stage state is `pending` (`before == "pending"`). The guard runs
  IMMEDIATELY after that state read and **before** the liveness probe and the HTTP hop, so it short-circuits ahead of
  both (no probe call, zero reap-failure POSTs).
- **Fail-open:** an unreadable or absent stage row leaves the observation unarmed, so the guard does **not** fire and
  the reap proceeds — the same best-effort posture `confirmStrandBeforeReap` documents (refusing a recovery verb on a
  transient read error would re-strand the stage it exists to clear). It is a narrowing layer over the server's
  protected-park allow-list, not an invariant.
- **Where to go instead:** the refusal points at `fishhawk_dispatch_stage` / `fishhawk_run_stage` (or
  `fishhawk_run_children` for a decomposition child) to MOVE the pending stage, or `fishhawk_cancel_run` to abandon the
  whole run.
- **The escape hatch is unchanged:** the direct `POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure` still accepts a
  `pending` stage, so an operator who genuinely means to fail one POSTs the endpoint directly. Keeping the endpoint
  unrestricted while the verb carries the opinion is the intended split — **do not "fix" the apparent inconsistency by
  loosening the verb.** Option (a) (refuse in the verb) was taken over option (b) (refuse at the endpoint) precisely so
  the endpoint stays the escape hatch for an operator who knows what they are doing.

`TestReapStage_StablePendingRefused` pins the refusal (message names `pending` and `fishhawk_dispatch_stage`, the probe
ran ZERO times, zero POSTs), `TestReapStage_DispatchedAndRunningStillReaped` is the paired control that the guard is
`pending`-specific (both still reap in one POST), `TestReapStage_UnreadableStageDoesNotTriggerPendingRefusal` pins the
fail-open posture, and `TestReapStage_DescriptionStatesTrueReapAuthority` pins that the tool description names the
pending refusal and no longer carries the old absolute authority claim.

Inputs: `run_id` + `stage_id` (required), `category` (B or C, default C — enforced LOCALLY with the verb's own message,
never a copy of the server's 400 text), `reason` (default `operator_reap_stranded_stage`), `detail`, `exit_code`.
Output: `transitioned`, `stage_state`, `runner_liveness`, `next_step`, `warnings`.

**`next_step` is per-CATEGORY, because the two accepted categories have different recovery paths.** `run.RetryableFailure`
classifies **C** (infrastructure) as retryable and **B** (constraint/policy) as NOT, so one constant hint would send a
category-B caller at a verb that refuses the stage the call just produced:

| reported `category` | `next_step` says |
|---|---|
| `C` (default) | the stage is `failed`/category-C, `fishhawk_retry_stage` admits it — retry, then re-dispatch |
| `B` | `fishhawk_retry_stage` does **NOT** admit it (`422 retry_not_applicable`); re-opening needs the operator-only category-B override ([#698](https://github.com/kuhlman-labs/fishhawk/issues/698)), `POST /v0/stages/{stage_id}/retry` with `{"override":true,"reason":"…"}` — agent tokens are refused there |

Prefer the default: `B` is for recording a genuine constraint/policy failure, not for clearing an ordinary strand.
`TestReapStage_CategoryBAcceptedWithItsOwnNextStep` pins both halves — that `B` reaches the wire verbatim, and that its
`next_step` DIFFERS from the `C` one (the two are driven through the same handler and compared, so collapsing the
selector back to one constant reddens it whatever the wording).

**The liveness probe is GATED on `runner_kind`, and its invariant is narrow — stated as such rather than overclaimed.**
`classifyReapLiveness` never returns `dead` outside the local arm:

| `runner_kind` | probe runs? | verdict | note |
|---|---|---|---|
| `local` | yes (`probeRunnerLiveness`, the shared `driveProbeRunnerLiveness` seam) | the probe's verbatim `live`/`dead`/`unknown` | `live` **REFUSES** the reap outright, before any HTTP hop |
| `github_actions`, `gitlab_ci`, any other non-empty kind | **no** | `unknown` | a host-local `pgrep` is INAPPLICABLE; warning says liveness is unverifiable from this host |
| absent (`""`, an older backend's `omitempty`) | **no** | `unknown` | an absent kind cannot be confirmed local; "never DEAD" is the safe direction |
| unreadable (GetRun error) | **no** | `unknown` | warning names the run and the read error |

So the guarantee is: **it prevents reaping a live stage THIS HOST SPAWNED**. For a remote runner it rests on the
operator's explicit invocation plus the server-side protected-park allow-list, and says so in `warnings`.

**The classification and the POST are not atomic — so the verb NARROWS client-side and then PINS server-side
([E67.51 / #2699](https://github.com/kuhlman-labs/fishhawk/issues/2699)).** The endpoint's reap authority spans BOTH
`dispatched` and `running`, so a stage probed `dead` while merely `dispatched` can be dispatched concurrently — spawning
a runner and advancing to `running` — and an unpinned POST would then fail a genuinely live stage.
`confirmStrandBeforeReap` re-observes IMMEDIATELY before the POST and refuses on either of two independent detectors:

| detector | fires when | misses |
|---|---|---|
| (1) pre/post stage-state comparison | the stage's state CHANGED across the classification (the `dispatched` → `running` advance is exactly this signature) | a spawn whose state advance has not committed yet |
| (2) re-probe | the host probe, re-run, now finds a LIVE runner (inherits the `runner_kind` gate — a non-local run is never probed on either pass) | a spawn whose process is not yet visible to `pgrep` |

Both **fail OPEN** on an unreadable or absent stage row: this is a narrowing layer over the server's protected-park
allow-list, and refusing a recovery verb on a transient read error would re-strand the stage it exists to clear. Neither
detector CLOSES the window on its own — nothing on the client side can, because the observation and the POST are two
round-trips. `TestReapStage_RaceStageAdvancedUnderProbeRefused` and `TestReapStage_RaceRunnerAppearedBeforePostRefused`
drive one interleaving each (each with the OTHER detector unarmed, so the refusal can only come from the one under
test), `TestReapStage_UnracedStrandStillReaped` is the paired control that an unraced strand is still reaped, and
`TestReapStage_ReconfirmNeverProbesANonLocalRun` pins the re-probe's gate.

**The window is now CLOSED at the server, which is the only place it can be closed (#2699).** The verb sends the state
it LAST observed (detector (1)'s `after` read) as the reap-failure endpoint's `expected_state` precondition, and the
server compares it under its row lock with the CAS walk's **re-anchor disabled** — so a concurrent dispatch that
advances the stage makes the compare-and-swap LOSE and the reap is refused `409 stage_state_precondition_failed`,
atomically, rather than absorbed. The named mechanism ("probe says dead while `dispatched` → a concurrent dispatch
advances the stage to `running` → the POST fails a live stage") is therefore structurally impossible, not merely
narrowed.

| surface | behaviour |
|---|---|
| observation succeeded | `expected_state` = that state; a mismatch is a tool error naming BOTH the pinned and the actual state and pointing at `fishhawk_get_run_status` + re-invoke |
| final re-read failed, EARLIER read succeeded | the STALER `before` observation is pinned rather than dropped — a pin is evaluated against LIVE state under the server's row lock, so a stale pin can only refuse MORE, never fail a stage that moved; dropping it would let one transient read error downgrade a conditional reap to an unpinned one |
| observation outside `{pending, dispatched, running}` (a protected park, an already-terminal stage) | **REFUSED LOCALLY, no HTTP hop.** Such a state can be neither PINNED (the endpoint `400`s a precondition it could never honour, which the verb would then have to tell apart from the version skew) nor sent UNPINNED — an unpinned POST takes the server's absorbing path, so a park RESOLVED at its own gate and dispatched between the observation and the POST would be reaped LIVE: the same check/use race #2699 closes for the anchors, relocated to the parks. The refusal names the observed state and the gate that owns it. `reapStageConditionalAnchors` mirrors the server's `reapConditionalAnchors` (deliberate duplication — the server package is not importable here) |
| stage row unreadable / absent on BOTH reads | NO `expected_state` sent — the fail-open posture verbatim, since refusing a recovery verb on a transient read error would re-strand the stage |
| `409 stage_state_precondition_failed` | surfaced to the operator; **never retried unpinned** (the client's `#1791` aggressive 4xx retry is skipped for this response on a conditional call) |
| `400` whose DETAILS carry `field=expected_state` + `accepted` | a SAME-VERSION out-of-set refusal, classified AHEAD of the skew arm and rendered as such (it names the accepted set and says it is **not** a version skew). Only the validating handler can produce that details map — an OLD `fishhawkd` rejects the field at its DECODER and never reaches validation — so the details, never message text, are the discriminator. Telling that operator to rebuild `fishhawkd` would send them at a repair that fixes nothing |
| any other `400` naming `expected_state` | a **VERSION SKEW** message (this `fishhawk-mcp` is newer than the `fishhawkd` it talks to; the endpoint decodes with `DisallowUnknownFields`, so it 400s the field rather than ignoring it) — also never retried unpinned, because a silent unconditional retry would hand back the exact guarantee the pin buys |

**The remaining residual, stated rather than papered over:** a runner that has been spawned but whose state advance has
not COMMITTED yet is still reapable, because the stage state is the only thing the server can compare against —
detector (2), the re-probe, remains the (best-effort) client-side detector for that case. And a 409 means the stage was
never driven to `failed`; for a `dispatched`-pinned reap the server's walk is `dispatched → running → failed`, so a
refusal on the SECOND leg leaves that intermediate hop committed. Pinned by
`TestReapStage_SendsObservedStateAsPrecondition` (both anchors), `TestReapStage_UnreadableStageSendsNoPrecondition`
(fail-open, BOTH reads failing), `TestReapStage_StaleBeforeObservationPinsAnyway` (the asymmetric arm — the earlier
observation is pinned, not dropped), `TestReapStage_NonAnchorObservationRefusedBeforePost` (one subtest per protected park plus a terminal state: refused
with ZERO POSTs) and `TestReapStage_NonAnchorParkResolvedBeforePostNotReaped` (the raced half — the park is resolved to
`running` the instant the confirming read is answered, and the reap still never goes out), `TestReapStage_PreconditionLostSurfacesToOperator` and
`TestReapStage_UnknownFieldSurfacesVersionSkew` (each asserting EXACTLY ONE POST),
`TestReapStage_UnrelatedBadRequestIsNotMisreadAsSkew` and `TestReapStage_OutOfSetPreconditionIsNotMisreadAsSkew` (the
two paired controls that a 400 is not mislabelled a skew — one not naming the field, one naming it for a
same-version reason), `TestReportStageFailureFrom_413RetryKeepsPrecondition`, and the server-side
`TestReapStageFailure_Conditional*` set.

`guardSiblingStageInFlight`'s target-`running` arm reuses the same classification, **message-only**: the refusal stays
UNCONDITIONAL across `live`/`dead`/`unknown` and only the wording changes (`dead`/`unknown` now name
`fishhawk_reap_stage` → `fishhawk_retry_stage` → re-dispatch instead of asserting "a live runner owns it" in exactly the
case where none does). Keeping the refusal unconditional is what makes a probe misclassification able to change prose
and never a permission — `TestGuardSiblingStageInFlight_TargetRunningRefusesAcrossAllVerdicts` pins it.

**`fishhawk_cancel_run` gained a FAIL-CLOSED decomposition-child guard in the same change.** Cancelling a child
permanently wedges its parent — `fishhawk_consolidate_slices` requires every child to have SUCCEEDED, which a cancelled
child never can, and no verb un-cancels a run. So `cancelRun` reads the run row first and REFUSES a child; an
**unreadable** row also refuses, deliberately the opposite posture from `guardHostDispatch` /
`guardSiblingStageInFlight`, which fail OPEN because they guard a non-destructive dispatch decision where failing open
costs at most a retry. `orphan_parent_ok:true` is the single documented override for both refusals and surfaces a
warning naming the orphaned parent.

## Restart-orphaned review recovery (`fishhawk_reconcile_reviews` + the await strand probe, [E67.55 / #2712](https://github.com/kuhlman-labs/fishhawk/issues/2712))

When `fishhawkd` restarts while a plan or implement review is in flight, the detached reviewing goroutine dies with the
process and no terminal `*_review_(reviewed|skipped|failed)` entry ever lands. `review_status` is derived from the audit
trail (`reviewStatusFor`: `pending` while `landed_terminal < configured_agents`), so the round stays `pending` forever —
the gate silently DEGRADES from N reviewers to the M that landed, and `fishhawk_await_review` used to burn its entire
window and then assert the dead reviewer was "genuinely still running". Two surfaces close it.

**The strand probe (`review.go`).** `reviewRoundStrandFrom` reports `Stranded` only when ALL of: a `*_review_started`
entry exists; its `configured_agents > 0`; landed terminals for THIS round (same re-open floor and floored decoders
`reviewStatusFor` uses) `< configured_agents`; `/healthz` resolved a PARSEABLE `process_start`; and the started entry's
timestamp is BEFORE that boot instant. Every other outcome is not-stranded, and an unreadable boundary is a distinct
`Undecidable` verdict carrying a reason.

- **Fail-open is uniform.** `/healthz` unreachable, non-200, non-JSON, absent `process_start`, or an unparseable one all
  degrade to `Undecidable` — the wait then behaves exactly as it did before this diagnostic existed. A zero `time.Time`
  is NEVER presented as a boundary: it compares as before every audit entry and would convert every pending review into
  a false strand, so `HealthInfo.ProcessStartOK` (present AND parseable) is the gate, not zero-ness.
- **The boundary is re-probed DURING the wait on a bounded TTL** (`strandProbeCacheTTL`, 30s; per-call cache state, not a
  per-call SAMPLE). This is load-bearing rather than a cost tweak: the restart that strands a review is an event that
  happens DURING the wait — an operator lands a sibling campaign item with `scripts/dev post-merge` while this review is
  in flight — so a boundary pinned at call start could never see it and the wait would still burn its window against a
  pre-restart boundary. `TestAwaitReview_BoundaryChangesMidWait_SameCallDetectsStrand` begins polling under boundary A,
  changes it to B mid-wait, and asserts the SAME in-flight call resolves.
- **No extra audit reads.** The status and the strand verdict are both derived from ONE `loadReviewRound` per tick, so
  the two can never disagree about which entries belong to the round and the diagnostic costs only the TTL'd `/healthz`.
- **The report names the SHORTFALL, not just the stall**: `landed_terminal` of `configured_agents`, WHICH reviewers
  reported, and how many never will — because the silent two-reviewers-to-one degradation is half of what makes this
  failure dangerous. `ReviewStatus.Status` is deliberately UNCHANGED at `pending` (it feeds the approval/merge gates,
  `drive_run` and `await_audit`); `stranded` is an await-surface diagnostic, not a new gate state.
- The pending-after-timeout message no longer asserts liveness unconditionally: it says "verified: dispatched by the
  daemon currently serving" only on a not-stranded verdict THAT ACTUALLY COMPARED THE BOUNDARY, and on `Undecidable` says
  so and names the recovery. The claim is gated on a non-zero `DaemonProcessStart` — set only after `boundary.OK`, so it
  IS the evidence the comparison ran — because `reviewRoundStrandFrom` returns the same not-stranded/not-undecidable
  shape on three early returns that never consult `/healthz`: no started entry, `configured_agents <= 0` (a pre-#1127
  round, still reachable at the timeout path via `reviewStatusFallback`), and every-reviewer-landed. Those fall back to
  the neutral pre-#2712 wording; claiming verification there would move the same confident-wrong-signal to the
  legacy-round edge.

**The recovery verb.** `fishhawk_reconcile_reviews` wraps `POST /v0/runs/{run_id}/reviews/reconcile` (contract:
`backend/internal/server/README.md`). It appends ONLY the missing terminal entries for the current round, so
**already-landed verdicts are preserved verbatim** — a 1-of-2 round gets exactly one synthesized entry and the real
verdict still reads back with its concerns at the gate — and a second call is a no-op (`round_already_settled`). It
**refuses (no-op with a reason) a round this daemon still has in flight**: the server compares the round's dispatch
timestamp against its own boot marker and answers `skip_reason: review_dispatched_by_this_process`, so invoking it
against a healthy running review changes nothing. Requires `write:runs`; a 404/403 surfaces verbatim.

## `push_and_open_pr=false` is per-case, not global (`guardNoPRImplement`)

`push_and_open_pr=false` (the runner's `--no-pr`) is supported on a **standalone** implement stage and refused on the
two paths where it cannot settle ([#2691](https://github.com/kuhlman-labs/fishhawk/issues/2691)):

| Stage kind | `push_and_open_pr=false` | Why |
|---|---|---|
| standalone implement | **supported** (E22.8 / [#406](https://github.com/kuhlman-labs/fishhawk/issues/406)) | stamps none of the three forward-gate manifest flags, so the trace upload settles the stage; the agent's work is left in the working tree for the operator to commit |
| decomposition child | **refused** at dispatch (L2) | the runner stamps `push_to_shared_branch` regardless of `--no-pr` ([#771](https://github.com/kuhlman-labs/fishhawk/issues/771)), so the backend defers the terminal transition onto a `/pull-request` report `--no-pr` never sends |
| fix-up pass | **refused** in the runner (L1) | same shape via `push_fixup` ([#794](https://github.com/kuhlman-labs/fishhawk/issues/794)) |

Those two forward gates deliberately do NOT test `--no-pr`: they exist to stop a succeeded-but-unlanded implement stage
(a child consolidated into a parent PR missing its slice; a fix-up re-review approving an unlanded diff). Making them
settle under `--no-pr` would trade a loud strand for a silent wrong merge, so the refusal is the fix and `trace.go` is
untouched.

`guardNoPRImplement` is the **pre-spawn** half, wired into BOTH `fishhawk_dispatch_stage` and `fishhawk_run_stage`
before every state-committing step (the auto-drive attribution row, the host-dispatch marker, the spawn), so a refusal
commits no state, spawns nothing, and leaves the stage parked for a clean re-dispatch. It returns immediately —
**no round-trip** — unless the stage is `implement` AND the flag is explicitly false; on that path it costs **one
additional `GetRun`** beyond the reads the callers already make. It **fails OPEN** on a `GetRun` error, matching
`guardHostDispatch`'s posture: on an unreadable run row this layer cannot know the stage is a child, and refusing would
break legitimate dispatches during a backend hiccup. That fail-open branch is the ONE case where a runner process may
start — the runner-side refusal then fires immediately after the prompt fetch, before any worktree or agent work, so
**no agent pass is ever burned**.

Its scope is deliberately the **decomposed-child signal only**. Fix-up status is not authoritatively knowable pre-spawn:
`fixup` is derived server-side inside the `/prompt` handler from `resolveFixupConcerns(runID, stageID)` and served only
on the prompt response — neither the run nor the stage response carries it, and the fix-up / retry / plan-approved stage
parks are indistinguishable by state. The gate view's pending fix-up rows are a PREDICTION, not authority: the backend
re-derives `fixup` at prompt-fetch time, so a concern waived, deferred or resolved in between flips the answer and an
L2 refusal built on it would reject a legitimate dispatch. `base_branch` is likewise not a refusal criterion — on a
standalone stage a base branch triggers neither a working-tree restore nor a forward-gate flag.

## Paged-acceptance discharge (`fishhawk_arbitrate_acceptance`)

`fishhawk_arbitrate_acceptance` ([E66.37 / #2474](https://github.com/kuhlman-labs/fishhawk/issues/2474)) is the **operator-only discharge** for a run parked at `acceptance_triage_paged` — a FAILED acceptance verdict whose deterministic triage paged the human. Before it, `fishhawk_merge_run` refused such a run with `409 acceptance_gate_not_passed` and `fishhawk_retry_stage`'s acceptance-reopen arm refused it too (it requires NO recorded verdict), so the only way out was to leave the loop and hand-merge — losing the `merge_verdict_recorded` audit entry the merge verb exists to write. It wraps `POST /v0/runs/{run_id}/acceptance-arbitration`.

Inputs: `run_id`, a required `reason` (recorded verbatim), and `acknowledge_failed_criteria` — the deliberate, separately-stated decision the backend requires (`409 acceptance_arbitration_requires_acknowledgement`) when the verdict carries genuinely FAILED criteria. A class-5 all-skip verdict, where nothing failed, needs the reason alone. The tool validates `run_id` and a non-empty `reason` locally before the HTTP hop and delegates the auth ladder, every guard and the audit append to the backend — the authoritative surface, so the tool can never admit something the gate refuses.

What it is NOT: not a way past an AUTO-ROUTED disposition (a class-1/2 verdict that routed to `fixup_dispatched`/`retry_dispatched` keeps its automatic route and is refused), not a re-run, and not a pass — the gate state is `acceptance_arbitrated`, deliberately distinct from `acceptance_passed`.

A **write** tool needing `write:approvals`; a run-bound agent token is rejected (`run_token_forbidden`, 403).

**One correlation rule, not two.** `next_actions` surfaces the discharge via `acceptanceArbitratedIn`, which matches an `acceptance_triage_arbitrated` entry whose payload `outcome_sequence` EQUALS the newest `acceptance_outcome_recorded` entry's SEQUENCE. That is byte-for-byte the rule the authoritative server gate applies — deliberately NOT the ordering idiom `latestAcceptanceTriageDisposition` uses. The two rules disagree on exactly one shape: an arbitration appended AFTER a newer verdict but NAMING an older outcome. Under an ordering rule this surface would offer a merge that `fishhawk_merge_run` then 409s — the self-contradiction #2474 documents as its second-worst symptom. The classifier's `acceptance_arbitrated` arm is checked BEFORE the paged branch, so a discharged run surfaces the merge ritual instead of being re-offered the arbitration it already has; the paged arm itself now lists `fishhawk_arbitrate_acceptance` ahead of `merge_and_file_follow_up`, whose precondition states the arbitration comes first.

## Scope amendment at approval (`fishhawk_approve_plan` → `add_scope_files` / `remove_scope_files`)

`fishhawk_approve_plan` (E22.4 / [#393](https://github.com/kuhlman-labs/fishhawk/issues/393)) takes an optional `add_scope_files` array ([#824](https://github.com/kuhlman-labs/fishhawk/issues/824)) — the **structured, authoritative** way to add files to the implement stage's `scope.files` at approval time. On approve the named paths are recorded on the approval audit payload and folded into the implement stage's effective scope by the prompt builder, so a reviewer-authorized edit ships as a declared path rather than surfacing as benign `scope_drift`.

Prefer it over naming paths in the free-text `reason`. The `reason` fold ([#730](https://github.com/kuhlman-labs/fishhawk/issues/730)) is a best-effort regex scrape kept only as a fallback; it silently misses:

- **directories** — pass a trailing slash (e.g. `pkg/testdata/corpus/`); every created file under that prefix stages.
- **extensionless and repo-root files** — e.g. `go.work`, `Makefile`.
- **described-but-not-spelled paths** — anything the prose names in words rather than as a literal path token.
- **absolute / non-repo-relative tokens** — the fold now silently skips any token that is absolute (leading `/`) or contains a `..` traversal segment ([#1155](https://github.com/kuhlman-labs/fishhawk/issues/1155)), so naming a `/tmp` path or an exclusion in prose no longer injects a phantom scope entry. Only clean repo-relative paths fold; use `add_scope_files` for an authoritative add.

`reason` and `add_scope_files` compose: the structured paths fold first (authoritative), then the prose fold runs as a fallback, both deduping by path. Both no-op when the plan declares an empty scope, preserving the runner's `git add -A` fallback. `add_scope_files` does **not** weaken the policy gate — a folded path that matches `forbidden_paths` still fails category-B against the produced diff.

**Removing and replacing scope paths ([#1726](https://github.com/kuhlman-labs/fishhawk/issues/1726)).** `fishhawk_approve_plan` also takes an optional `remove_scope_files` array — the **inverse** of `add_scope_files`. On approve the named paths are **subtracted** from the implement stage's effective `scope.files` by the prompt builder, so every runner gate (created-out-of-scope, commit-in-scope, category-B) and the scope-cap gate honor the removal, and it applies to per-slice scope on decomposed plans via the same parent fan-out fallback `add_scope_files` uses. It is recorded on the `approval_submitted` audit payload alongside `remove_scope_files` plus `scope_files_before` / `scope_files_after` (the deduped effective-scope file lists). The removed path is also surfaced in the implement prompt text (an "Operator-removed scope files" section) telling the agent it is no longer in scope, since `writeApprovedPlan` still renders the immutable plan artifact's `scope.files`.

- **Replace = remove + add in one call.** There is no separate replace field: pass `remove_scope_files` AND `add_scope_files` in the same approve to swap paths (remove old + add new) at the plan gate with zero planner invocations — composable and consistent with the additive path. This is how an over-cap plan is reconciled entirely at the gate.
- **Validation / skip rules (fail-closed).** Each `remove_scope_files` path is refused `400 validation_failed` (`field: remove_scope_files`), before any approval row is inserted (a corrected retry flows normally), when it is: **not repo-relative** (a leading `/` or a `..` traversal segment — same containment contract `add_scope_files` skips on); **absent from the current effective scope** (plan `scope.files` ∪ prior folds ∪ approved amendments ∪ this call's `add_scope_files`) — this catches operator typos rather than silently no-op'ing; or a removal that **would empty a non-empty effective scope** — an empty scope re-enables the runner's `git add -A` fallback and disables scope enforcement, so keep at least one path or re-plan. Omitting the field is byte-identical to today. Honored only on `approve`; ignored on `reject`.

**Duplicate-submission labeling ([#986](https://github.com/kuhlman-labs/fishhawk/issues/986)).** A re-submission by the same subject — `fishhawk_approve_plan` or `fishhawk_reject_plan` against a stage that subject already decided — is a no-op the tools label explicitly instead of rendering as a normal result: the output carries `duplicate_submission: true` plus `prior_decision` (the existing row's), and the result text leads with a banner stating the prior decision stands, the stage state is unchanged, and the budget/scope gates were NOT re-run. The override markers (`--override-budget` / `--override-scope-cap`) are honored because both gates now run **pre-insert**: a 422 refusal records no approval row, leaving the submission slot free for the override retry.

**Scope-cap gate ([#983](https://github.com/kuhlman-labs/fishhawk/issues/983); override posture [#2415](https://github.com/kuhlman-labs/fishhawk/issues/2415)).** A plan-stage approve is refused `422 plan_violates_scope_cap` when the effective scope — plan `scope.files` ∪ `add_scope_files` ∪ approved amendments, **minus `remove_scope_files`** ([#1726](https://github.com/kuhlman-labs/fishhawk/issues/1726)), deduped by exact path — exceeds the implement stage's `max_files_changed`. Because removals are subtracted first, an over-cap plan can be reconciled at the gate by removing or replacing paths. The refusal inserts no approval row, so a retry after re-scoping flows normally.

The `--override-scope-cap` marker in the comment forces an over-**declared**-cap approve through — but it is **conditional** ([#2415](https://github.com/kuhlman-labs/fishhawk/issues/2415)), no longer an unconditional escape hatch. The override clears **only** the declared-scope pre-check; it never reached the implement stage's `max_files_changed` re-evaluation, which counts the **real physical diff**. So when the scope's **minimum physical changed-file count** (`min_changed_files`) already exceeds the cap, the override is **refused** `422 plan_scope_cap_override_unavailable` (pre-insert, no approval row, plus a `plan_scope_cap_override_refused` audit entry) — approving it would only defer the same over-cap failure to a category-B implement stage after a full run. The refusal is **categorical within a run** because the cap is immutable there: the plan gate and the post-implement gate both read the same `runs.workflow_spec` snapshot, so an override that cannot fit today can never fit later in this run. When `min_changed_files` fits the cap the override still succeeds, recording a `plan_scope_cap_override_acknowledged` entry whose payload now notes it covers the declared-scope pre-check **only**. `min_changed_files` is the minimum PHYSICAL count the hard gate approximates: it collapses each declared delete+create **pair** git could detect as a rename into one physical file (the plan schema has no rename operation), so a rename-shaped plan whose declared count is over cap but whose physical count fits keeps the override.

Read headroom before approving against **`min_changed_files`, not `scanned_files`**: `fishhawk_get_plan`'s `scope_precheck` carries `min_changed_files` and `max_files_changed` alongside `scanned_files`, and its `plan_warnings` surfaces the [#2492](https://github.com/kuhlman-labs/fishhawk/issues/2492) **near-cap advisory** — when the plan's scope lands within a few files of the cap it names the remaining headroom and warns that a mid-stage scope amendment will be refused once that headroom is spent (stated more emphatically for a decomposed plan, whose slices all draw against the one whole-plan budget) — plus the [#2415](https://github.com/kuhlman-labs/fishhawk/issues/2415) **unlandable advisory**, which fires when `min_changed_files` exceeds the cap and states plainly the plan cannot land in this run even with `--override-scope-cap`.

**Decomposed-plan `add_scope_files` gate ([#2103](https://github.com/kuhlman-labs/fishhawk/issues/2103)).** A plan-stage approve that supplies `add_scope_files` on a **decomposed** plan (one carrying `decomposition.sub_plans`) is refused `422 plan_add_scope_files_fans_into_slices`. Unlike the override-able scope-cap gate, there is **no override** for the flat form: an operator-added path is folded into the effective scope across the fan-out boundary and returns the *same* paths to *every* child slice, so the added file lands in every slice's scope, violating single-owner-file and guaranteeing an add/add fan-in conflict (duplicate, incompatible implementations). The `details` name every inheriting slice — `add_scope_files`, `slice_count`, and `slices` (each `{index, title}`). The refusal inserts no approval row, so a corrected retry after **re-planning the decomposition** so each added file is declared in exactly one slice's `scope.files` flows normally. The gate also **fails closed** — same 422 — when `add_scope_files` is non-empty and the plan cannot be positively confirmed non-decomposed (a load failure or an indeterminate/nil plan), so an add is never recorded without positive confirmation the plan is flat.

**Per-slice scope add on a decomposed plan (`add_scope_files_to_slice`, [#2515](https://github.com/kuhlman-labs/fishhawk/issues/2515)).** The channel that DOES work where the flat add above is refused. `fishhawk_approve_plan` takes an optional `add_scope_files_to_slice` map — `{"<sub-plan title or 0-based index>": ["path", …]}` — folding each path into **exactly one** slice's effective `scope.files`, so restoring one dropped file into one slice costs an approve instead of a full revise pass.

- **Keying.** An exact sub-plan **title** match wins; otherwise the key must be a 0-based decimal index in range. Explicit intent beats positional coincidence (a plan whose title is literally `"0"` resolves the key `"0"` by title). The recorded form is always canonical: index-keyed, with each path list trimmed, deduped and **sorted** — so a title-keyed approve and the equivalent index-keyed approve record byte-identical audit payloads.
- **One owner per path — containment, not string equality.** A path here may be a **directory** (trailing slash). The approve is refused `400 validation_failed` (`field: add_scope_files_to_slice`) when a path overlaps in ownership — identical, or an ancestor/descendant directory containment compared on path segment boundaries (so `pkg/foo` never conflicts with the sibling `pkg/foobar`) — either with another key in the SAME request or with a **different** slice's declared `scope.files`. The same 400 covers an unresolvable or ambiguous slice key, two keys naming one slice, a non-repo-relative path, and an empty path list; `details` carry the offending key/path plus the ordered `{index, title}` slice list so a corrected retry takes one shot.
- **It ADDS; it does NOT MOVE.** The case that motivated #2515 — a file already declared in ANOTHER slice's scope that you want alongside a different slice's files (e.g. `README.md` owned by slice 1, wanted with `tools.go` in slice 2) — is still **refused**, on purpose: moving is a different operation with different semantics and is tracked separately. The refusal names the owning slice (`owning_slice` / `owning_title` / `owning_path`), states the add-not-move semantic (`channel_semantic: add_not_move`), and names the remedy: **re-plan the decomposition** so the path is declared in the slice that needs it.
- **Wrong-plan refusals.** `422 plan_slice_add_scope_files_requires_decomposed_plan` — the one new error code — when the plan is positively **flat** (`details.reason: plan_not_decomposed`; use plain `add_scope_files` there) or cannot be confirmed decomposed (`plan_indeterminate`, **fail-closed**, matching the categorical posture of the #2103 gate). No override.
- **Effects.** Per-slice paths consume `max_files_changed` headroom exactly like a flat add. At prompt-build time only the entry keyed to the requesting child's own `slice_index` is folded, so the path reaches that slice and no other; a run with no slice index (a plan-stage-less recovery child) folds nothing. Every refusal is pre-Submit: no approval row, no audit entry, corrected retry flows normally.

**Per-slice scope MOVE on a decomposed plan (`move_scope_files_to_slice`, [#2596](https://github.com/kuhlman-labs/fishhawk/issues/2596)).** The complement of the add above — the slice-boundary MOVE the add channel deliberately **refuses**. `fishhawk_approve_plan` takes an optional `move_scope_files_to_slice` map — `{"<destination sub-plan title or 0-based index>": ["path", …]}` — that relocates an **already-declared** file from the slice that owns it to the destination slice. The key names the **destination**; the **source is derived from ownership** (you do not name it), because single-owner-file guarantees exactly one owner.

- **Add vs move.** Use `add_scope_files_to_slice` to fold a **net-new** path into a slice; use `move_scope_files_to_slice` to relocate a path that is **already in the plan's decomposition scope** but sits in the wrong slice — the exact case the add channel refuses `path_owned_by_another_slice`. The move keeps the plan's total file count unchanged, so unlike the add it consumes **no `max_files_changed` headroom** (an at-cap plan can still take a move). The two compose in **one approve** but must name **disjoint** paths (refused `path_in_both_scope_channels` otherwise).
- **Exact owned path — no directory splitting.** A move names an **exact** declared scope path. A path that only overlaps a directory-valued declared entry by containment is refused `move_requires_exact_owned_path` — a directory entry cannot be split by a move; re-plan the decomposition instead. Recording is the same canonical index-keyed, sorted, deduped form as the add.
- **Refusals** (all pre-Submit — no approval row, no audit entry). `400 validation_failed` (`field: move_scope_files_to_slice`) for a path under two destination keys, a path also in `add_scope_files_to_slice`, a path declared in NO slice (`path_not_in_declared_scope` — the message names `add_scope_files_to_slice` as the channel that adds a net-new path), a containment-only overlap, a no-op move to the owning slice (`path_already_owned_by_destination`), a move that would empty the source slice (`move_would_empty_source_slice`), plus the shared key/path shape refusals. `422 plan_slice_move_scope_files_requires_decomposed_plan` when the plan is flat (`plan_not_decomposed`) or cannot be confirmed decomposed (`plan_indeterminate`, fail-closed). `409 plan_slice_move_after_dispatch` when a source or destination fan-out child has already left run state `pending` (`slice_already_started`) or the sibling listing failed (`dispatch_state_indeterminate`, fail-closed) — re-scoping a slice whose work has begun is refused.
- **Effects.** At prompt-build time the destination child gains the moved-in path in its enforced `scope_files` (folded via the same sole source as the add), and the source child loses it from BOTH its `scope_files` and its `scope_constraint.scope_files`. The audit records `move_scope_files_to_slice` (the canonical map) and `move_scope_files_resolved` (`[{path, from_slice, to_slice}]`) — the latter is the only place the true move provenance survives, since a trace labels a moved-in path identically to an added one.

**Approve-reason byte cap ([#2583](https://github.com/kuhlman-labs/fishhawk/issues/2583)).** The `reason` on `fishhawk_approve_plan` is injected **verbatim** to the implement agent as a binding approval condition (#558), so it must not be silently cut. It is capped at **`prompt.MaxApprovalConditionBytes` (12000 bytes)** — measured in **bytes, not characters** (the prompt builder truncates on `len`, and a byte cap cannot be expressed as a JSON-Schema `maxLength`, which counts characters). An over-cap approve `reason` is **refused `400 validation_failed`** at the approval gate (`handleSubmitApproval`, and the `/fishhawk approve` slash-command channel `HandleApprovalCommand`), naming the actual byte count, the cap, and the overflow — **no** approval row is recorded, so a shortened retry flows normally. The cap was raised from the historical 4000 so a realistic multi-condition approval is accepted; split or tighten an over-cap reason, or move a machine-checkable clause into `binding_assertions` (below). For the residual case of a comment stored **before** this gate existed, the prompt builder still truncates at build time but now appends an `approval_conditions_truncated` audit entry so the drop is visible in the run record.

**Reject-reason over-budget warning ([#2680](https://github.com/kuhlman-labs/fishhawk/issues/2680)).** A **reject** `reason` feeds the advisory `PriorRejectionFeedback` channel (the replanning agent's prior-rejection feedback), NOT binding conditions — so unlike the approve path it is **never refused**: a reject is cheap to re-issue, and refusing it would leave the operator unable to record a gate verdict at all. That is the deliberate **warn-on-reject / refuse-on-approve asymmetry** — a silently dropped over-cap *approve* condition would corrupt the implement stage's binding instructions, whereas an over-cap reject is delivered whole up to its own **`prompt.MaxRejectionFeedbackBytes` (12000 bytes)** cap and, past it, rendered with an explicit ADR-077 elision marker (naming the dropped bytes and a retrieval pointer) rather than a bare mid-sentence cut. When the reason exceeds that budget, `fishhawk_reject_plan` also returns a **non-blocking warning on the tool result** naming the reason's byte count, the budget, the overflow, that the delivered prompt will carry a truncation marker (not a silent cut), and the remedy (shorten, or move the surviving steering into the next approve's binding conditions / `binding_assertions`) — so a reason that will not arrive intact is visible **before** the replan cycle is spent, rather than discovered only when the next agent's steering turns out to be missing its tail. The warning **coexists** with the approver-login warning on one result (neither clobbers the other), and the reject **still submits** regardless. The threshold is `prompt.MaxRejectionFeedbackBytes` itself (not a re-typed literal), so the tool warning and the render-side cap cannot drift.

**Binding assertions ([#1171](https://github.com/kuhlman-labs/fishhawk/issues/1171)).** `fishhawk_approve_plan` also takes an optional `binding_assertions` array — the **machine-checkable** half of an approval condition, the deterministic complement to the free-text `reason` fold. Where `reason` is restated to the implement agent as binding conditions (#558) and `add_scope_files` widens the scope, `binding_assertions` declares checks the runner enforces: each entry is `{type, path, literal}` where `type` is `file_contains` or `test_asserts` (open enum), `path` is repo-relative (and must end in `_test.go` for `test_asserts`), and `literal` is a non-empty substring that must appear in the committed file. On approve they are recorded on the approval audit payload alongside `add_scope_files` and echoed on the implement prompt-response; the runner evaluates each as a deterministic substring check against the committed scope-only tree post-implement, and any unsatisfied assertion fails the implement stage category-B. Substring matching only — never parses prose, so a literal chosen too loosely is an operator-declaration concern, not a gate defect. A malformed declaration (unknown `type`, empty `literal`, a `test_asserts` path not ending in `_test.go`) is rejected `400 validation_failed` before any approval row is recorded. Omitting the field is byte-identical to today.

**Condition-claim resolution ([#1956](https://github.com/kuhlman-labs/fishhawk/issues/1956)).** `fishhawk_approve_plan` also takes an optional `claims_concern_ids` array — the operator-declared lineage link from a binding approval condition to the plan-stage concern(s) it answers (no NLP/heuristic matching, the operator-confirmed design). Where `binding_assertions` makes a condition machine-checkable, `claims_concern_ids` records *which plan-stage concerns* the condition resolves: read the ids from `fishhawk_get_gate_view` or the run-status concerns block, then name them at approve. Each claimed concern auto-resolves to the new terminal state `addressed_by_condition` once **ONE** implement review returns a confirming (non-reject) verdict — the operator's condition is the authority, the reviewer the witness, so a heterogeneous co-reviewer's reject in the same round does not block the resolution (the reject still pages the operator). The resolution appends a `concern_addressed_by_condition` audit entry (carrying the concern id, prior state, claiming approval sequence + approver, and confirming review sequence + reviewer model) **before** transitioning the row, so the concern → condition → confirming-review lineage is durable in the chain, and the resolved concern lands in the `fishhawk_get_gate_view` settled ledger instead of demanding a hand waive at the merge gate. Validated pre-Submit — approve-only, plan-stage-only, each id an **open** plan-stage concern of the **same run** — so a malformed claim is rejected `400 validation_failed` (a non-plan stage or `nil` concern store yields `400`/`503 concern_store_unconfigured`) before any approval row is inserted. Omitting the field is byte-identical to today.

**Implement-model override ([#1013](https://github.com/kuhlman-labs/fishhawk/issues/1013)).** `fishhawk_approve_plan` also takes an optional `implement_model` string — the operator's override for the implement-stage model, the top rung of the resolution ladder `deployment default < spec executor.model < plan model_recommendation < this override`. On a plan-stage approve the backend resolves the full ladder with this as the operator rung, validates the **resolved** value against the deployment's per-adapter allow-list, and records the choice as a `model_resolved` audit entry (`{model, model_source}`) that the runner spawn routes to the agent's `--model`. An unknown resolved model — from any rung, not just this override — is rejected `422 plan_invalid_model` (details carry `model`, `model_source`, `adapter`), pre-insert so a retry with an allowed `implement_model` flows normally. An empty/unconfigured allow-list fails **open** (any model accepted, byte-identical to today). Omit the field to ratify the plan's `model_recommendation` or fall through to the spec/deployment default; an empty resolution still records `model_resolved` and spawns with no `--model`, exactly as today.

The OpenAPI surface (`docs/api/v0.openapi.yaml`) and its companion `docs/api/v0.md` remain the authoritative parameter reference.

## Mid-stage scope amendments (`fishhawk_list_scope_amendments`, `fishhawk_decide_scope_amendment`)

E22.X / [#961](https://github.com/kuhlman-labs/fishhawk/issues/961) adds the **mid-stage** complement to approval-time `add_scope_files`: while the implement stage is RUNNING, the agent can request that specific paths be folded into the effective `scope.files` instead of silently dropping a coupled edit (the runner omits undeclared edits from the commit; an undeclared created file fails category-B, #818/#825).

**Agent protocol (poll-based, no push channel in v0).** The implement prompt instructs the agent to `POST /v0/runs/{run_id}/scope-amendments` with its run-bound `FISHHAWK_API_TOKEN` (`{paths: [{path, operation: modify|create}], reason}`), then poll the GET (same bearer, `mcp:read`) every 15–30s until the request leaves `pending`, working on in-scope files meanwhile and giving up after ~15 minutes, at which point the request EXPIRES UNDECIDED — it stays pending server-side, is not a denial, and a later decision is honored on a `fishhawk_retry_stage` of the same stage. Cap: **2 requests per stage**, counted server-side on rows — a denied request still consumes budget. The agent must never edit/create a requested file before the approval lands. The canonical contract for "expiry is not denial" is [`backend/internal/scopeamendment/README.md`](../scopeamendment/README.md); the figure the agent actually follows is rendered by `writeScopeAmendments` in [`backend/internal/prompt/prompt.go`](../prompt/prompt.go). This README states the number in prose and cannot render from a Go constant, so it is one of the surfaces the mcpserver `amendmentPollWindowMinutes` constant does **not** unify — a correction to the number touches this line, the prompt, and the constant. The Go-rendered surfaces the constant DOES unify are discrimination-tested end to end — the runtime `amendment_pending` Message/Reason (`TestAwaitStageAmendmentMessageRendersWindowConstant`) and the three tool descriptions that state the figure (`TestToolDescriptionsRenderWindowConstant`), so on those a hardcoded literal goes red; this prose figure is not, so it stays part of the three-edit correction.

**Operator loop:**

1. Await the request: `fishhawk_await_audit` anchored on category `scope_amendment_requested` (#977). The entry payload carries `{amendment_id, paths, reason, remaining_budget}`.
2. Inspect: `fishhawk_list_scope_amendments {run_id}` — paths, per-path operation, the agent's reason, status.
3. Decide: `fishhawk_decide_scope_amendment {run_id, amendment_id, decision: approve|deny, reason}`. Decide promptly — the agent's poll is bounded.

**Scope-cap headroom ([#983](https://github.com/kuhlman-labs/fishhawk/issues/983)).** When the implement stage has a `max_files_changed` cap, pending items in the list (and the request/decision responses) carry `effective_scope_files_after_approval` + `max_files_changed`, and both tools print an explicit `WARNING` line when approving would put the effective scope over the cap. Warn-only by design: an over-cap approve still succeeds — mid-stage amendments are often forced, and the post-implement gate plus the now-informed operator own the verdict. Fields are absent on older backends or when no cap is configured.

**Split-filing outcome on `fishhawk_get_plan` ([#2057](https://github.com/kuhlman-labs/fishhawk/issues/2057), refusal [#2412](https://github.com/kuhlman-labs/fishhawk/issues/2412)).** `get_plan`'s `split_filing` surfaces the on-approval split-child filing outcome, decoded from the newest completion (`split_children_filed`) OR refusal (`split_filing_refused`) audit marker. A completed filing carries `contract_classification`, the filed `children[]`, `contract_child_number`, the `#2062` deferral, and (governed-exception only) the in-memory `cap_exception`. A **refusal** — the hook DECLINED to file any children because a `split_proposal` phase's own declared file count exceeds the resolved implement `max_files_changed` cap (an over-cap phase would itself fail the implement cap, [#2410](https://github.com/kuhlman-labs/fishhawk/issues/2410)) — populates the `refused` sub-object (`reason`, `cap`, and each offending `phases[]` entry: index, title, declared_count); **no children were filed**, so re-slice the phases under the cap or declare the plan `irreducible` and re-approve. The two outcomes are mutually exclusive; `split_filing` is absent when the plan carries no `split_proposal`, the hook has not yet completed filing or refusing, or on older runs.

**Auth.** The decision is operator-only (`write:stages`); the backend rejects run-bound agent tokens outright (`self_decision`), so the requesting agent can never approve its own request. The agent-side POST requires the implement-stage token's `write:scope-amendments` scope (granted unconditionally at token issue for implement stages); the GET admits the run-bound token (`mcp:read`, own run only — cross-run is 403) or any operator bearer/session.

**Activation.** Approved paths fold into the effective scope at BOTH ends: the backend prompt fetch (`source "scope-amendment"`, so a stage restart or fix-up carries the amended scope) and the runner's pre-commit refresh, which re-reads the GET with the same run-bound token and folds approved paths BEFORE the committed-tree verify gates and `StageScoped` — preserving the #960 invariant that the gates verify the same folded tree that is pushed. Anything NOT requested still fails loud. Both `scope_amendment_requested` and `scope_amendment_decided` are internal audit kinds, not issue-comment surfaces.

## Scope-completeness park (`fishhawk_decide_scope_completeness`)

E22.X / [#1231](https://github.com/kuhlman-labs/fishhawk/issues/1231), generalized to a second shortfall class by [#2501](https://github.com/kuhlman-labs/fishhawk/issues/2501), is a **zero-re-run** recovery for the case the [#1229](https://github.com/kuhlman-labs/fishhawk/issues/1229) one-re-run exempt lever otherwise served: an implement stage whose **only** committed-tree gate shortfall is one of

- the [#1151](https://github.com/kuhlman-labs/fishhawk/issues/1151) scope-completeness "missing declared scope file(s)" check, or
- an [#1171](https://github.com/kuhlman-labs/fishhawk/issues/1171) unsatisfied operator-declared **binding assertion**,

while the committed tree otherwise passed verify (created-out-of-scope, compile/test, and verified-tree gates all green).

**Park, not category-B.** Instead of fail-and-restore, the runner pushes the **gate-verified commit** to the run branch (no PR opened — ADR-035 sole-writer preserved: the run writes its own branch) and PARKS the implement stage in `awaiting_scope_decision`, carrying the held commit SHA, run branch, verified tree SHA, and the shortfall itself (`missing_paths` **or** `unsatisfied_assertions` — exactly one).

**The sole-failure guarantee is structural, not a promise.** Both gates RECORD their shortfall and keep running; the runner's terminal resolver emits the typed park error only at a pass point where every other gate is green. A COMPOUND failure — either shortfall plus another gate, or *both* shortfalls together — returns an UNTYPED sentinel wrap that `CommitAndPush`'s `errors.As` cannot match, so the push aborts category-B **before origin is touched**, byte-identically to today.

**Operator loop:**

1. Observe the park: `fishhawk_get_run_status` surfaces the `implement_awaiting_scope_decision` next action; `fishhawk_list_audit` on category `scope_completeness_parked` carries the shortfall (`missing_paths` / `unsatisfied_assertions`) + held SHA.
2. Decide: `fishhawk_decide_scope_completeness {run_id, decision: exempt|amend|fail, reason, paths?, acknowledge_owner_unresolved?}`. Both optional fields are **`amend`-only**: supplied with `exempt` or `fail` they are refused locally before the HTTP hop (and 400 `validation_failed` at the handler), because only the amend arm folds paths or runs the owner-slice guard — forwarding either elsewhere would leave the operator believing a widening was recorded, or a risk audited, that landed nowhere.
   - `exempt` — the stage returns to **dispatch** (`pending`, then the orchestrator advances it; on a local run it parks at `awaiting_host_dispatch` for your `fishhawk_dispatch_stage`). The re-dispatched runner opens the PR from the **exact held commit** with **NO agent re-invocation**: the prompt response carries `open_pr_from_held_commit` / `held_commit_sha` / `held_commit_branch` / `held_commit_base_sha` (the fourth, #2563, is the base commit the runner ships as the PR artifact's required `base_sha` — resolved from the park or, for a legacy park, its `scope_completeness_parked` audit payload), and the runner short-circuits to its held-commit PR-open before the agent invoker is even selected. Appends `scope_completeness_exempted`.
   - `amend` ([#2591](https://github.com/kuhlman-labs/fishhawk/issues/2591)) — the `paths` you name are folded into **this stage's** effective scope as a pre-approved #961 amendment row and the stage resumes to `pending`, so the agent **RE-RUNS** against the wider scope. The held commit is left untouched on the run branch (ADR-035). It is the **only non-fail resolution for a BUILD-REQUIRED park** ([#2548](https://github.com/kuhlman-labs/fishhawk/issues/2548)), whose held tree is red by construction and whose `exempt` is refused `exempt_refused_build_required`. Appends `scope_completeness_amended`, which ALSO invalidates any earlier `exempted` on the same stage (an amend means "do not ship this tree"). Refusals, each leaving the stage parked with no row and no audit entry: `amend_incomplete_coverage` (a `build_required_path` was omitted — a re-run would re-park identically), `amend_refused_owner_slice_active` (the OWNING sibling slice already started its implement pass, so two branches could carry divergent edits to one file), `amend_owner_attribution_unresolved` (ownership unprovable — **fails closed**; re-submit with `acknowledge_owner_unresolved: true`, which is recorded on the response and the audit entry as an audited operator risk, and never relaxes the started-owner refusal) and `amend_budget_exhausted`. **The cost:** an operator amend consumes one of the stage's two mid-stage #961 amendment slots; `agent_amendment_budget_remaining` on the response reports what is left. A 500 `amend_unrecorded` / `amend_resume_failed` leaves the stage parked — re-POST it, the amendment row is REUSED rather than duplicated, so a retry cannot drain that budget.
   - `fail` — the stage falls through to today's category-B fail-and-restore. Appends `scope_completeness_failed`.
3. Spawn the runner (local runs): `fishhawk_dispatch_stage` on the implement stage. After `exempt` the PR opens from the held commit with zero agent re-run; after `amend` the agent re-runs against the widened scope.

**Emission is fail-closed.** The four exempt prompt fields are served ONLY when the newest entry for the stage across the decision-plus-invalidator set is `exempted` (`server/prompt.go::resolveHeldCommitExemption`). A **fix-up dispatch** (#2630 — a fix-up carries a prompt the runner MUST run, so emitting the fields would send it to the pre-agent short-circuit and discard the fix), a park still undecided, a `fail`-decided park, a stage RE-parked after an earlier exempt, a stage whose exempt was **superseded** by a newer `pull_request_opened` / `fixup_pushed` / `child_pushed` (#2630 — the invalidator categories are in the walk so a completed exempt can never be re-emitted on a later dispatch), an unreadable audit chain, a park missing its held-commit coordinates, AND a park for which no base SHA resolves (park field or the `scope_completeness_parked` audit fallback) all emit nothing — so a stage moved into `pending` by any other caller can never open a PR from a commit no operator accepted, and the runner is never handed an exempt resume it cannot ship (#2563).

**Auth.** Operator-only (`write:stages`); the backend rejects run-bound agent tokens (`run_token_forbidden`), so the agent whose stage parked can never decide its own park (mirrors `fishhawk_decide_scope_amendment`). `reason` is required and non-empty; an invalid `decision` (anything but `exempt`/`amend`/`fail`), an empty `reason`, an `amend` with no `paths`, and `paths` supplied with any other decision are all caught before the HTTP hop. The endpoint returns 409 (`scope_completeness_not_parked`) when the stage is not parked in `awaiting_scope_decision`. It wraps `POST /v0/runs/{run_id}/scope-completeness/decision`.

`scope_completeness_parked`, `scope_completeness_exempted`, `scope_completeness_amended`, and `scope_completeness_failed` are internal audit kinds, not issue-comment surfaces.

## Category-B recovery (`fishhawk_resume_run`)

E22.X / [#978](https://github.com/kuhlman-labs/fishhawk/issues/978) adds operator-initiated recovery for a run whose implement stage failed **category-B** (scope/constraint violation) after its plan was approved — the gap between `fishhawk_retry_stage` (refuses B) and `fishhawk_start_run` (replans from scratch). The tool wraps `POST /v0/runs/{run_id}/recover` and mints a **new plan-stage-less child run** that re-executes against the parent's approved plan.

Pointed at a failed decomposition **child** run instead, it re-drives that child **in place** on the shared parent
branch (#1081) — not a new run. Both arms fold operator-named `add_scope_files` as a pre-approved #961 amendment.

Inputs: `parent_run_id` (the failed run), optional `add_scope_files` (`[{path, operation: modify|create}]`, operation defaults to `modify`), optional `reason`, `budget_override`, and `idempotency_key` (same replay semantics as `fishhawk_start_run`).

- **Eligibility**: parent's plan stage `succeeded` AND implement stage `failed` category-B; anything else returns `recovery_not_eligible` naming which leg failed. Parents without a cached workflow spec return `recovery_unsupported` — start a fresh run.
- **Plan reuse**: the child carries `parent_run_id`; `fishhawk_get_plan` and the prompt builder resolve the parent's plan via the existing parent walk. The parent's binding approval conditions and approval-time `add_scope_files` are inherited too.
- **Scope amendments**: operator-named `add_scope_files` land as a **pre-approved** #961 amendment row on the child's implement stage — visible via `fishhawk_list_scope_amendments`, folded by the prompt fetch and the runner's pre-commit refresh; `operation: create` entries flow into the #818/#825 net-new-file gates.
- **Budget**: `retry_attempt` is carried UNCHANGED — recovery never consumes the `on_ci_failure` auto-retry cap. Provenance lands as a `plan_reused_from` audit entry on the child (internal audit kind, not an issue-comment surface).

Drive the child like any local run: `fishhawk_run_stage` executes the implement stage directly — no plan stage exists, no plan approval is needed.

## Failed-run revive (`fishhawk_revive_run`)

`fishhawk_revive_run` (E22.X / [#1915](https://github.com/kuhlman-labs/fishhawk/issues/1915)) is the **single operator verb** that re-admits a terminal-**FAILED** run for another turn, replacing the old retry-without-dispatch dance (retry each failed stage, then hand-park the rest). It wraps `POST /v0/runs/{run_id}/revive`.

The backend **pre-validates** that **every** failed stage is retryable, then re-parks each in its correct gate-ordered pre-dispatch state (A/C → `pending`, D SLA-timeout → `awaiting_approval`, decomposed-parent implement → `awaiting_children`) and flips the run **failed → running**. A single non-retryable failed stage (category-B, D-rejected, or a stage with no recorded category) refuses the **whole** revive with `422 revive_not_applicable` naming the blocking stage — the pre-validation admits **no partial mutation**.

**Resumable partial state ([#1942](https://github.com/kuhlman-labs/fishhawk/issues/1942)).** The re-park batch plus the run reopen are **NOT** one transaction (each re-park and the closing reopen run their own row-locked transaction), so a *post-validation* failure — an infra error or a concurrent transition — can leave the run partially re-parked and surface a `500`. This is deliberate; every intermediate state is a valid state-machine state and **a second revive is the compensation**: just retry the verb. It re-parks the remaining failed stages, and when every failed stage was already re-parked it completes the interrupted reopen — succeeding with an **empty** `restored_stages` and `resumed:true`, consuming **no** stage's retry budget a second time. Consequently the `revive_not_applicable` refusal for "no failed stage" now applies **only** when no stage sits in a re-parked pre-dispatch state; the interrupted-revive shape succeeds instead.

The load-bearing distinction from `fishhawk_retry_stage`: revive **re-parks only** — it performs **NO** orchestrator handoff and **never dispatches**. A re-parked stage sits in its pre-dispatch state until you dispatch it at its proper gate turn via the existing verbs (`fishhawk_dispatch_stage` / `fishhawk_run_stage` on the local runner), so the [#1700](https://github.com/kuhlman-labs/fishhawk/issues/1700) wrong-order re-dispatch corruption is structurally impossible. `fishhawk_retry_stage`, by contrast, re-opens **one** stage and auto-dispatches it. Reach for revive when a sibling stage's failure flipped the run terminal while a healthy stage's review is still settling and you want a safe batch re-park; reach for retry when you want one stage re-run immediately. Each re-park consumes that stage's per-stage retry budget exactly like a retry — revive is a batch retry-shaped re-open, not a budget bypass.

- **Input**: `run_id` (the terminal-FAILED run).
- **Auth**: operator-only. The backend requires `write:stages` **or** `write:retries` and rejects any run-bound agent (`mcp:run:*`) token outright (`403 agent_token_forbidden`).
- **Returns**: the re-opened run (now `running`), the per-stage re-park summary (`restored_stages` — each carrying id / type / prior failure category+reason / restored state; **empty** on a resumed revive), a `next_step` hint that dispatch happens at each stage's proper gate turn, and — only when the revive was committed but the `run_revived` chained audit append then failed — an `audit_warning` string ([#1943](https://github.com/kuhlman-labs/fishhawk/issues/1943)). `audit_warning` means the revive **succeeded** (the run is reopened and the stages re-parked) but the provenance record is missing; investigate the audit store. It is **omitted** on a clean revive.
- **Errors** propagated as tool errors: invalid UUID (caught before the HTTP hop), `agent_token_forbidden` (403), `insufficient_scope` (403), `run_not_found` (404), `revive_not_applicable` (422), `internal_error` (a post-validation partial re-park; **retry the verb to resume**, 500), `revive_unconfigured` (503).

The `next_actions` failed-run arms (`implement_failed_category_a` and the default `implement_failed`) surface `fishhawk_revive_run` alongside `fishhawk_retry_stage`.

## Clarification answer-and-resume (`fishhawk_answer_clarification`)

`fishhawk_answer_clarification` (E22.X / [#1088](https://github.com/kuhlman-labs/fishhawk/issues/1088), the [#1057](https://github.com/kuhlman-labs/fishhawk/issues/1057) answer-and-resume seam) answers the questions a planner parked at `awaiting_input` so its plan stage can resume. When an issue is not yet plannable the planner parks the plan stage at `awaiting_input` with a `clarification_request` ([#1080](https://github.com/kuhlman-labs/fishhawk/issues/1080)) instead of producing a plan; the run is stranded until the operator answers. This tool wraps `POST /v0/stages/{stage_id}/clarification`.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `run_id` | **yes** | The run whose plan stage parked at `awaiting_input`. The tool resolves the plan stage internally — no stage id needed. |
| `answers` | **yes** | One `{id, answer}` per parked question, keyed by the question id from the `clarification_requested` audit entry (read it via `fishhawk_get_run_status` / `fishhawk_list_audit`). At least one; every parked question needs exactly one answer, and an unknown/missing/duplicate id is rejected. |
| `comment` | no | Free-text note appended after the answers in the binding conditions delivered to the resumed plan agent. |

What it does:

- The answers are persisted as a **dedicated `clarification_answered` audit entry** — **not** an approval (the plan is not yet approved), so the `approval_submitted`/`decision=approve` channel `loadApprovalConditions` reads stays isolated. The plan-stage prompt loads them into the resumed agent's binding conditions.
- The **same** plan stage re-opens (`awaiting_input → pending`) in the **same** run — no new run, no duplicate reviews (distinct from `fishhawk_resume_run`, which mints a child run). On a `github_actions`/drive run the backend re-dispatches the plan stage; on a local run, re-run it with `fishhawk_run_stage plan` after this returns.
- **Auth:** a write tool requiring `write:approvals` (the [#558](https://github.com/kuhlman-labs/fishhawk/issues/558) gate-answer family).

Error surfaces propagated as tool errors: `validation_failed` (400 — empty answers / unknown fields; the empty case is also caught locally before the HTTP hop), `clarification_answer_invalid` (400 — an answer id is unknown, missing, or duplicated relative to the parked questions), `stage_not_found` (404), `invalid_state_transition` (409 — the resolved stage is not a plan stage parked at `awaiting_input`). The `next_actions` `plan_awaiting_input` arm points here.

## Work-item filing (`fishhawk_file_issue`, [#1005](https://github.com/kuhlman-labs/fishhawk/issues/1005))

`fishhawk_file_issue` files a work item — issue, bug, chore, ADR — through the repo's **work-management conventions** rather than calling the tracker's API directly. It is both the consistent cross-repo/cross-platform filing surface (the conventions are the value: one call shape works against a GitHub-Projects-configured repo or a Jira-configured one — only the per-repo conventions differ) and the operator-agent follow-up-filing path ([ADR-040](https://github.com/kuhlman-labs/fishhawk/issues/1004)): the operator agent files deferred-work tickets through it instead of by hand. It wraps `POST /v0/work-items`.

The backend loads the repo's conventions, renders the title from the type's `title_format`, assembles the body from the type's skeleton + caller `sections` (or takes `body` verbatim), merges `default_labels` with explicit `labels`, resolves board placement / complexity / ADR numbering, links the relations, and dispatches to the registered provider (GitHub Projects in v0).

Inputs:

| Field | Required | Notes |
|---|---|---|
| `type` | **yes** | Work-item type; a key in the repo's conventions (e.g. `feature`, `bug`, `chore`, `adr`). |
| `summary` | **yes** | The mandatory one-liner: fills the `{summary}` title placeholder and is the required Summary field. |
| `repo` | falls back to env | Target repo as `owner/name`; defaults to `GITHUB_REPOSITORY` when omitted (the in-runner case). |
| `body` | no | Verbatim body; when omitted the body is assembled from the type's skeleton + `sections`. |
| `sections` / `title_vars` | no | Per-skeleton-section content and extra title placeholders (e.g. `epic`, `n`). An unresolved title placeholder fails the filing. For a child type whose `title_format` is `[E{epic}.{n}]`, BOTH `{epic}` AND the `{n}` child number are auto-derived from the `parent_epic` relation server-side (#1958), so `title_vars` can be omitted entirely — supply `n` only to override the auto-derived child number. |
| `labels` / `complexity` / `status` | no | Merged on / overriding the type's defaults; `complexity` must be a declared level. |
| `relations` | no | `{parent_epic, supersedes[], companion_to[], evidence_runs[], depends_on[]}` — resolved into the provider's link operations. `depends_on` is the issue-level dependency edge (issue refs among the epic's children) a campaign reads to assemble its wave DAG (ADR-047); it is persisted as a `Depends on: #X, #Y` body marker and validated format-only at file time (cycle/existence checks deferred to campaign-assembly time). |
| `existing_numbers` | no | Numbers already in use for a numbered type (e.g. `adr`), so the next sequential number can be allocated. |
| `run_id` | falls back to env | Optional in-flight run UUID; defaults to `FISHHAWK_RUN_ID`. When set and non-terminal a best-effort `work_item_filed` audit entry is appended to it. |

Audit-on-active-run is **best-effort**: filing still succeeds with no run in flight, and the response's `audited` flag reports whether an entry was written. Returns the created item — `type`, `title`, `number`, `url`, `provider`, the resolved `applied_labels` / `complexity` / `status` / `board_column`, and `audited`.

**Auth:** a write tool — the backend requires an authenticated caller (anonymous requests are rejected). Error surfaces propagated as tool errors: `validation_failed` (400 — repo not `owner/name`, missing `type`/`summary`, unknown fields; the empty `type`/`summary`/`repo` cases are also caught locally before the HTTP hop), `authentication_required` (401), `work_item_invalid` (422 — the request violates the type's conventions), `provider_unimplemented` (501 — the configured provider id is not registered, e.g. the interface-only `jira`; details name it), `work_item_filing_failed` (502 — the provider rejected the filing). On the 502 the raw provider cause is REDACTED from the body (it can carry storage / third-party-endpoint internals); the tool surfaces the `error_ref` and points at the operator log where the full cause is retained (E67.15 / #2587), instead of appending `details.error` verbatim as it did before. The CLI mirror is `fishhawk file-issue`.

## Product feedback (`fishhawk_report_product_issue`, [#1006](https://github.com/kuhlman-labs/fishhawk/issues/1006))

`fishhawk_report_product_issue` files an upstream **Fishhawk product** bug or feature request — when you hit friction with Fishhawk itself, not the repo you're working in — carrying an auto-collected diagnostic bundle. It wraps `POST /v0/runs/{run_id}/product-reports`. The destination is the **fixed** upstream product repo; it is not caller-controlled. The backend collects the run's product-facts bundle, fingerprints the failure `(error code, failing surface, failure detail class, version family)` — the detail class is a closed-enum normalization of the failure reason (`auth-401` | `bad-object-ref` | `target-unreachable`) that splits distinct root causes sharing a surface (#1962), included only when classified — searches the product repo for an open report already carrying that fingerprint marker, and either appends an occurrence comment (dedup hit — nothing new is created) or files a new fingerprint-marked report (dedup miss). A source-side `product_report_filed` audit entry records what left the boundary.

**The redaction boundary is the hard contract.** By default the report carries **product-level facts only** — no diffs, paths, prompts, or free text. Operator free text (`description`) crosses the boundary **only** when `include_free_text=true`, and even then it is run through the backend's secret-redaction machinery first. Treat `include_free_text` as the operator's explicit consent; it defaults off.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `run_id` | falls back to env | The run whose product-facts bundle to attach; defaults to `FISHHAWK_RUN_ID` (the in-runner case). |
| `kind` | no | `bug` (default — attaches the diagnostic bundle) or `feature` (an enhancement request; lighter workflow context). |
| `description` | no | Operator free text. Crosses the boundary **only** with `include_free_text=true`, redacted server-side first; otherwise ignored. |
| `include_free_text` | no | Explicit consent: when true, `description` crosses **after** server-side redaction. Default false. |

Returns the egress outcome (`report.action` `created`\|`occurrence`, `fingerprint`, upstream `number`/`url`, `destination`), a transparency preview of the product facts that were attached (`diagnostics`), and `free_text_included`.

**Auth:** the first **write** tool that drives an egress on the run's chain — the backend requires the run's **own** run-bound agent token (an operator token or a foreign run's token is rejected with `run_not_entitled`). Error surfaces propagated as tool errors: `validation_failed` (400), `authentication_required` (401), `run_not_entitled` (403 — only the run's own run-bound token may file), `product_feedback_disabled` (403 — the per-repo kill-switch), `run_not_found` (404), `provider_unimplemented` (501), `product_report_failed` (502). The CLI mirror is `fishhawk report-issue`.

## Response byte budget (the bounded surfaces, ADR-077 / [#2508](https://github.com/kuhlman-labs/fishhawk/issues/2508), [#2510](https://github.com/kuhlman-labs/fishhawk/issues/2510))

Three surface families are bounded to **32,768 marshalled bytes BY CONSTRUCTION**: `fishhawk_get_run_status` (`bound.go`), `fishhawk_list_audit`, and **every verb that returns a run row** (`bound_surfaces.go`) — `fishhawk_get_active_run`, `fishhawk_start_run`, `fishhawk_cancel_run`, `fishhawk_list_runs`, `fishhawk_start_campaign_item_run`, `fishhawk_revive_run`, `fishhawk_resume_run`. `fishhawk_drive_run` needs no bound and the reason is structural, not an oversight: `DriveRunOutput` carries `run_id` / `run_state` only and never embeds a `Run`, so the criterion is "does the response type embed `Run`", not an enumerated list a future verb can fall off.

`fishhawk_get_run_status` is bounded to **32,768 marshalled bytes BY CONSTRUCTION** (`bound.go`). This is a **bound**, not another opt-in projection: the `#1727`/`#1749` `include_*` levers still apply first, and the ladder runs after them regardless of which flags were passed.

**Why 32 KiB, and why it is a proxy.** The client rejects on **tokens** (`result (N characters) exceeds maximum allowed tokens`); fishhawkd cannot tokenize, so a byte budget is necessarily a proxy. The threshold was bracketed by driving `fishhawk_list_audit` through the real MCP client: **42,280 chars succeeded**; **54,262**, **81,530** and an independent **75,963** failed — measured threshold `42,280 < T < 54,262`. 32 KiB is the largest confirmed success less ~20%. It bounds the **inner** response; the server never sees the transport envelope, so the headroom absorbs it. A client with a materially different tokenizer is a **re-measurement**, not a design change.

**Override and the below-floor CLAMP.** `FISHHAWKD_MCP_RESPONSE_BUDGET_BYTES` (the GENERAL var, #2510) overrides the default on every bounded surface; `FISHHAWKD_MCP_RUN_STATUS_BUDGET_BYTES` is the LEGACY spelling #2508 shipped and stays honoured, so no operator override breaks. The **first present** var decides — general, then legacy — and its value routes through the same named branches, so a present-but-invalid general var takes the **default** rather than silently falling through to a stale legacy value. Both share one default (32 KiB) and one convergence floor (`mcpConvergenceFloorBytes`, 4 KiB). Absent / unparseable / non-positive each fall back to the default with its own logged reason. A value **below** `minimalRunStatusMaxBytes` (the floor) is **CLAMPED UP**, never accepted-and-silently-not-honoured — convergence can only ever guarantee `max(budget, floor)`. Clamping rather than rejecting is deliberate: the override exists to serve a client with a *lower* limit, and bouncing it back to 32 KiB would leave that client worse off. The clamp is announced on **two** surfaces — a one-line log naming requested/floor/effective, and the elisions block's own `budget` field, which reports the **effective** (post-clamp) value so the wire never claims a bound the ladder did not honour.

**The discovery ladder — advertised > configured > default (`client_limit.go`, E48.76 / [#2509](https://github.com/kuhlman-labs/fishhawk/issues/2509)).** Rather than assuming the client's tool-result limit, every bounded handler resolves its budget through `resolveResponseBudget(caps, getenv)` and reports which rung won on the wire as `elisions.budget_source`:

| Source | Rung | Meaning |
|---|---|---|
| `advertised` | 1 | the client advertised a plausible tool-result byte limit on the initialize handshake, and it was honoured as-is |
| `advertised_below_floor` | 1 | the client advertised a VALID limit **below** the 4 KiB convergence floor — the floor is emitted (convergence cannot promise less), and the source **says so** rather than presenting the floor as satisfying the smaller advertisement |
| `configured` | 2 | an operator env override (the two vars above) decided the budget |
| `default` | 3 | the ADR-077 measured 32 KiB constant |

The precedence is the issue's, verbatim: **when a client advertises a limit, Fishhawk uses it INSTEAD OF the configured default.** An operator env override is the *configured* rung and sits BELOW a truthful advertisement; the escape-hatch worry (a lying client) is covered by the plausibility ceiling below, not by inverting the precedence. The advertised limit is read off `ClientCapabilities` — the vendor-prefixed `Extensions` bucket keyed `fishhawk/tool-result-limit` FIRST (a bare number, or a settings object carrying the count under `bytes`), then the looser `Experimental` bucket — a fixed precedence so a client populating both gets a deterministic answer. Every implausible value is treated as **ABSENT** (never coerced) with a logged reason: a non-number, a NaN/infinite/non-integral float, a non-positive value, or a value **above** `maxPlausibleAdvertisedBytes` (1 MiB — 32x the default; a value beyond it is a bug or a lie whose failure mode is the silent client-side rejection this epic exists to prevent, so it fails SAFE down to the conservative default). Resolution is nil-safe at every hop (nil request, nil `Session`, nil `InitializeParams`, nil capabilities all resolve to the env-or-default answer), which is what keeps the ~90 handler tests passing a literal `nil` request green and unedited. When nothing is advertised — the common case today — the resolved budget is **byte-identical** to what #2508/#2510 ship. Filing the upstream MCP protocol issue that would standardise this capability is an external-forge action a sandboxed stage cannot perform; it is carried as a deferred live-validation criterion, not attempted here.

**No `/healthz` surface, deliberately ([#2509](https://github.com/kuhlman-labs/fishhawk/issues/2509)).** The issue lists a `/healthz` field only as "consider", and it was **dropped** rather than built. `backend/internal/server` reaches this package through the injected `mcproute.go` seam, not a direct import, so populating a `/healthz` field from the package-private resolver would mean exporting across an intentional boundary or duplicating the resolution — neither worth it. And `/healthz` is not session-scoped: it could only ever report the server-level env-or-default budget, never a per-session *advertised* value, so a "resolved limit + source" field there would invite an operator to read a session-specific answer off a surface that structurally cannot know it. The done-means ("the resolved limit plus its source is visible to the operator") is satisfied by `elisions.budget_source` on the trimmed response — the surface that actually explains a given trim. Do not re-propose a `/healthz` field without a session-scoped mechanism that does not lie about scope.

**Three elision classes (the honesty contract).**

| Class | Meaning | Pointer |
|---|---|---|
| `stored` | the content exists in durable storage | required — a `fishhawk_list_audit(...)` / `fishhawk_get_gate_view(...)` / `GET /v0/...` surface |
| `oversized_capable` | stored, but the value can itself be arbitrarily large | required, and **must** be an unbounded **REST** path — pointing an oversized `failure_reason` back at `fishhawk_get_run_status` is circular, because that call caps the same value again |
| `computed` | derived at read time, never stored | **none** — recomputation is not retrieval |

**The ONE pointer promise:** a pointer returns **AT LEAST** the omitted content. There is deliberately no second exact/superset promise anywhere in code, comments, docs or tests — that distinction was tried and immediately produced a defect. Every audit pointer is **anchored** by `since_sequence` so it returns the *dropped* set rather than a bare newest-N. `since_sequence=0` is the anchor because the client omits the parameter at zero and the REST filter is strictly-greater-than, so zero yields the whole chain — a superset the at-least contract permits. `fishhawk_list_audit` gained a `since_sequence` input in the same change so the pointer is genuinely callable.

**Tier ladder** (fixed, least-actionable-first; a tier whose target is absent records nothing). After **each** tier the in-progress elisions block is attached and the **whole** output is re-marshalled — a **measured** re-check, never arithmetic, so the block's own bytes are inside every measurement:

| Tier | Target | Class | Surface |
|---|---|---|---|
| T1 | `cost` / `cache_efficiency` / `latency` / `budget` | computed | — |
| T2 | `children_status` per-child detail (phase + counts retained) | computed | — |
| T3 | `security_findings` | stored | `fishhawk_list_audit(category=implement_security_findings)` |
| T4 | the whole `implement_reviews` slice | stored (**two** entries, one per originating category) | `fishhawk_list_audit(category=implement_reviewed)` + `(implement_review_skipped)` |
| T5 | `recent_audit` capped to the newest N | stored | anchored `fishhawk_list_audit` |
| T6 | `recent_audit` dropped entirely | stored | anchored `fishhawk_list_audit` |
| T7 | each stage's `failure_reason` capped | oversized_capable | `GET /v0/runs/<id>/stages` |
| T8 | `run.issue_context` | oversized_capable | `GET /v0/runs/<id>` |
| T9 | `run.concerns.items`, `run.review_authority`, `run.live_validation`, `drive_status.auto_advanced` (stored) + `review_action_hint`, `implement_review_merge_hint`, `next_actions.actions` cap (computed) | mixed | gate view / REST / audit |

T1–T9 touch **none** of `run.id`, `run.state`, any stage's `state` or `failure_category`, or `next_actions` presence.

**Skeleton, then the constant-size floor.** Below T9 the response is projected to a fixed retained set (run id/state/workflow_id; per stage type/state/failure_category + a capped failure_reason; the three `*_stage_wait_status` blocks; `next_actions` presence) and **every** omission is itemised **per field** — including omissions **nested inside a retained composite** (`run.issue_context`, `run.concerns.items`, `run.review_authority`, `run.live_validation`, `stages[].executor`, `next_actions.actions`). Below the skeleton, `minimalRunStatus` returns a constant-size output pinned under `minimalRunStatusMaxBytes` with **exactly two aggregate entries** — one `stored` whose pointer names the **union** of every surface its members would have named, one `computed` with no pointer. **Diagnosis outranks itemisation** at and below the skeleton: an operator reaches these tiers precisely when something went badly wrong.

**The path-keyed classification table is the single source.** `runStatusPathTable` is keyed by dotted **path** (`cost`, `run.concerns.items`, `stages[].failure_reason`, `next_actions.actions`, …) and carries each path's tier, class and retrieval surface(s). The tiers read it, the skeleton's nested itemisation reads it, the floor's union is **computed by folding it** (so the aggregate cannot drift from its members — it necessarily includes the `fishhawk_get_gate_view` surface T9 uses, and absorbs any surface a future tier introduces), and the reflection pin diffs against it. `TestEveryResponsePathIsClassified` walks `GetRunStatusOutput` **plus** the retained composites `Run`, `Stage`, `NextActions` and `RunConcerns`, so a new **nested** field (the commonest real case — `Run` gaining a mirrored backend field, as `LiveValidation` and `ReviewAuthority` both did) cannot silently bypass the budget. **The walk is only as deep as `retainedCompositeTypes()`**: a new nested composite *promoted into the retained set* needs adding there; a new field on a type a tier already elides wholesale correctly needs no entry.

**Two types, not one — and why you must not "simplify" them.** The internal accumulator (`elidedField`, `elisionLedger`) keeps **all** fields unexported with **constructor-only** access, and the constructors make the invalid states unrepresentable *by signature*: `newStoredElision` takes the pointer as a **required** parameter; `newOversizedCapableElision` takes a distinct `unboundedPointer` produced only by `pointerREST`, so handing it a bounded `fishhawk_*` pointer is a **compile** error; `newComputedElision` takes **no** pointer parameter at all. The **exported** `Elisions` / `ElidedField` DTO is a pure **projection** produced at exactly one call site, `(elisionLedger).wire()`.

That split is forced by the SDK, not by taste. `modelcontextprotocol/go-sdk` v1.7.0 validates the **marshalled** tool output against the reflected output schema **inside** the handler (`mcp/server.go:422` `applySchema(outJSON, outputResolved, true)`; `:424` wraps a failure as `validating tool output`), and `google/jsonschema-go` v0.4.3 both sets `AdditionalProperties = falseSchema()` on every reflected struct (`jsonschema/infer.go:246`) and **skips unexported fields** (`jsonschema/util.go:434`). An all-unexported wire type with a custom `MarshalJSON` therefore emits keys the schema declares no properties for and forbids as additional — breaking the tool **at runtime**. `TestGetRunStatus_PopulatedElisions_PassesSDKOutputSchemaValidation` drives a real `mcp.NewClient` `CallTool` over `mcp.NewInMemoryTransports` (including a floor-budget row, so the two-aggregate shape crosses too) and is the regression catch.

Because a composite literal can still build a malformed DTO no constructor vetted, **`validateWireElisions` runs over the projected DTO immediately before `wire()` returns** and fails **loudly** — naming the offending field and the invariant — on a computed entry with a pointer, a stored entry without one, an oversized-capable entry pointing at a `fishhawk_*` tool, any pointer containing `include_`, an unrecognised class, a negative `omitted_count`, or an aggregate above the floor tier. It never silently normalises; the handler surfaces the error rather than shipping a DTO that lies about its own classification. `TestProjectionIsSoleProducerOfWireDTO` parses the package and fails a second construction path, deriving each literal's enclosing scope from the declaration it walks rather than carrying it across the walk — so a construction is caught at any source position relative to any function, including the package-level-after-`wire()` spot that previously inherited the allowed producer's name and escaped (#2514). The match (via the `wireDTOLiterals` helper, pinned directly by `TestWireDTOLiteralMatch`) covers `Ident`-typed literals (`Elisions{...}`, `ElidedField{...}`, `&Elisions{...}`) **and** compound-typed constructions that denote the DTO — a slice/array `[]ElidedField{...}` / `[2]ElidedField{...}` / `[...]ElidedField{...}` (all via `ArrayType.Elt`), a pointer element `[]*Elisions{...}`, and a map whose key or value is the DTO — **plus** each literal's implicitly-typed inner elements (`{{Field: "f"}}`), resolved from the parent literal's element/key/value type so `map[ElidedField]otherType{...}` attributes the key and not the value (#2652). It still does **not** resolve a named container type (`type Fields []ElidedField; Fields{{...}}`), a type-aliased or cross-package spelling, or a reflection-built literal, so the pin backstops `validateWireElisions` rather than replacing it.

**No `include_*` flag may ever appear in a pointer.** Repeating a flag is circular: the same deterministic ladder elides the restored content again on the re-read. This is pinned as an explicit negative assertion.

**Capping is on MARSHALLED cost, not raw bytes.** `jsonEncodedLen` returns the exact encoded length (two-byte short escapes; six-byte `\u00XX` for other control bytes; six-byte forms for `<` `>` `&` and U+2028/U+2029; and `\ufffd` for each **invalid UTF-8** byte — an inflation factor reaching 6x), verified **differentially against the real encoder**. `capJSONString(s, budget)` guarantees `len(json.Marshal(result)) <= max(budget, 2)`; the `max(budget, 2)` floor makes a sub-minimum or negative budget **defined** (no JSON string encodes smaller than `""`). It stops at an invalid byte, so the result is always valid UTF-8 and always a rune-boundary prefix. `compact.go`'s raw-byte `truncateOversizedString` / `auditPayloadStringCap` are **deliberately untouched** (#1749) — a raw-byte cap is not a size bound under escaping.

**Determinism.** Anywhere the ladder retains a bounded subset of a **map**, keys are sorted first: Go map iteration order is unspecified and would otherwise make the same input produce two different responses and two different sizes.

**The ladder is NOT size-monotonic**, and no test asserts otherwise: attaching the growing elisions block can make a later measurement larger than an earlier one. Convergence comes from the constant-size floor, not from monotonicity. Tier tests therefore measure the **domain payload** with the elisions block excluded.

**There is no `fields_list_capped` signal, deliberately.** The ladder never caps the fields **list**: list bloat (a per-stage T7 entry explosion on a 400-stage run, say) falls through to the skeleton — which discards the tier ledger and builds a fresh fixed-size one — and then to the floor's exactly-two aggregates. A flag no code path can set is a schema surface that lies to its reader, so the convergence behaviour is documented rather than advertised as a signal that can never fire.

**Inert below budget.** The `elisions` field is `omitempty` and an under-budget response is returned **unchanged**, so its wire bytes are byte-identical to the pre-#2508 shape and `TestGetRunStatus_CompactDefault_UnderSizeBudget` stays green unmodified. The byte-identity claim is proved by `TestBound_UnderBudget_ReturnsTheInputBytesUnchanged`, whose baseline is marshalled from the **input** before the ladder runs — re-running the ladder over its own already-bounded output would assert only **idempotence**, which a first-pass mutation emitting no elisions satisfies. The handler-level twin asserts what it actually can: no `elisions` key on the wire, and bytes that do not vary with the budget.

### `fishhawk_list_audit` — the verifier surface (#2510)

`limit` advertises up to **200**, and `limit=55` already returned **54,262 chars** and hard-failed the client. The cap is **deliberately kept at 200** — the issue states the preference outright ("prefer bounding — a hard cap loses the operator's ability to ask for more and get a partial answer") — and the honesty that owes the operator is paid by the ladder:

| Tier | Target | Class | Surface |
|---|---|---|---|
| A1 | every oversized payload **string value**, capped escape-aware via `capJSONString` | oversized_capable | `GET /v0/runs/<id>/audit` |
| A2 | items dropped from the **TAIL** (rows are sequence-ASCENDING, so the kept prefix makes the dropped set exactly `sequence > last kept`) | stored | anchored `fishhawk_list_audit(..., since_sequence=<last kept>)` |
| floor | exactly ONE entry, payload dropped, **hash chain intact** | stored (aggregate) | anchored walk + `GET /v0/runs/<id>/audit` |

A1 points at the **REST** walk, never back at `fishhawk_list_audit`: re-calling the now-bounded tool would cap the same value again — the circularity `validateWireElisions` exists to catch. `entry_hash` / `prev_hash` are **retained at every tier including the floor**: this is the hash-chain **verifier** surface (see `AuditEntry.EntryHash`'s comment), and dropping the chain to save bytes would break verification rather than shrink it.

**THE CURSOR RULE.** The backend's `next_cursor` is positioned after the **FULL** page it fetched. Returning it alongside a **truncated** item list would make an operator paging by cursor **silently SKIP** every dropped entry — a data-loss bug strictly worse than the hard failure this bound fixes. So the moment any item is dropped, `next_cursor` is **BLANKED** and the `since_sequence` anchor becomes the sole continuation path. **Do not "restore" the cursor** on a later refactor: it is cleared because it is WRONG, not because it is redundant. `TestBoundListAudit_BlanksCursorOnTruncation` asserts the resulting STATE (no cursor after truncation), because an error-identity assertion cannot distinguish a cursor that was cleared from one that was never set.

### The shared run-row ladder (#2510)

Any run row embedding `issue_context` can exceed the limit alone (run `143aea12` = **79,131 bytes**), and several of these verbs are **MUTATING**: a real `fishhawk_cancel_run` failed at **110,397 chars** *after* the server-side cancel had already succeeded, and `fishhawk_start_campaign_item_run` broke at **75,963 chars** with the run already minted. The bound is what decouples a mutating verb's success signal from whether its body fits.

| Tier | Target | Class | Surface |
|---|---|---|---|
| R1 | `issue_context.comments` (title / url / number / labels retained — they are what makes the pointer actionable) | oversized_capable | the issue URL, falling back to `GET /v0/runs/<id>` |
| R2 | `issue_context.body` capped to 512 encoded bytes | oversized_capable | same |
| R3 | `issue_context` dropped entirely | oversized_capable | same |
| R4 | `concerns.items` (`open` + `by_state` retained) | stored | `fishhawk_get_gate_view(run_id=…)` |
| floor | the **diagnosis core**: id, repo, workflow_id, state, runner_kind, pull_request_url, parent_run_id — **each capped**, so the floor is constant-size for a row with pathologically long *retained* strings, not merely a large issue body — **plus every response field the schema forbids omitting** (below), carried in reduced form and **itemised individually** | stored (aggregate, for the omitted omittable fields) + one classified entry per carried-and-reduced field | `GET /v0/runs/<id>` + gate view (aggregate); per-field per the carry table below |

The floor starts from the **ZERO value** of the response type and installs that core, so it is constant-size for **any** response type rather than only for the fields this ladder enumerates.

**A zero value is an honest omission only for an `omitempty` field.** A field *without* `omitempty` is emitted whatever the floor does with it, so zeroing it does not omit it — it publishes a wrong value that reads as real, while the aggregate elision claims it was omitted. `ResumeRunOutput.Idempotent` would render `"idempotent": false` on a call that genuinely **replayed** (an operator acting on that could mint a duplicate recovery run) and `StartCampaignItemRunOutput.Item` an all-empty but populated-**looking** `CampaignItem`. So `carryNonOmitemptyFields` re-installs those fields from the real response, driven by the **json tag** rather than an enumerated field list — a field a future verb adds without `omitempty` is carried automatically instead of silently joining the falsified set. Carrying is a **bounded projection**, not a verbatim copy (`floorCarryValue`: every string capped at `floorFieldCap`, every collection truncated to `floorCarryCollectionCap`, string-keyed maps truncated in sorted-key order so the floor stays deterministic), so the constant-size guarantee survives. Fields the ladder installs itself — the `Run` row(s) and the `*Elisions` block — are skipped. The two **page** floors (`boundListRunsOutput`, `listAuditFloor`) build their output by hand and need no carry pass, because `items` is their only non-`omitempty` field and they install it themselves; `TestFloorNeverFabricatesNonOmitemptyField` fails the day either type gains another.

**The carry REPORTS what it reduced, and each reduction is ITEMISED (#2576).** A bounded projection still *loses* data — a truncated collection, a capped string — and the floor's aggregate `stored` elision cannot deliver it: its pointers are the run read + gate view, and neither returns `ReviveRunOutput.restored_stages`, `ReviveRunOutput.next_step`, or `StartCampaignItemRunOutput.item.depends_on`. So `carryNonOmitemptyFields` returns a `[]floorCarryReduction` (dropped-element / elided-byte counts, keyed to the nearest named **two-level** path — `item.depends_on` is nameable, anything deeper rolls up), and the floor emits **one classified entry per reduction** *plus* a re-worded aggregate whose at-least promise is scoped to the **omitted omittable** fields only. The per-field class comes from `floorCarryTable`, keyed by `reflect.Type`, read through the single **exact-match-first, then longest-ancestor-prefix** rule (`floorCarryRowFor`) that the itemiser, the pointer override, and the reflection pin all share:

| Field | Class | Pointer |
|---|---|---|
| `ReviveRunOutput.restored_stages` (append succeeded) | `oversized_capable` | `GET /v0/runs/<id>/audit` — the `run_revived` payload carries the whole restored list verbatim |
| `ReviveRunOutput.restored_stages` (append **failed**) | `computed` | **none** — `writeReviveAudit` errored, so no `run_revived` row exists; the class is conditioned on the response's own `audit_warning` |
| `ReviveRunOutput.next_step` | `computed` | **none** — a constant guidance string the MCP layer renders, never stored |
| `StartCampaignItemRunOutput.item` | `stored` | `GET /v0/campaigns/<campaign_id>/items` — threaded from the call site via `withFloorCarryPointer` (the campaign id is not on the `CampaignItem`) |

A reducible field with **no table row FAILS SAFE** to a pointer-less `computed` entry — never a `stored` claim naming a surface that cannot deliver it — and `TestEveryWiredFloorCarryFieldIsClassified` (a reflection pin over every wired run-row type's reducible fields) fails the day a future verb adds an unclassified one. The itemised block is bounded at `floorCarryEntryCap` (4) per-field entries with any remainder folded into one aggregate `computed` spillover, so the floor stays constant-size; the honesty wording is **not** trimmed to fit `mcpConvergenceFloorBytes` — the entry cap is the byte lever. NOTE — this SUPERSEDES #2576's issue text, which claimed `RestoredStages` is "not retrievable from any read surface": `writeReviveAudit` marshals it verbatim into the `run_revived` audit payload, which `GET /v0/runs/{id}/audit` returns.

**`pointerIssueURL`'s guard.** The issue URL is the most actionable unbounded surface for an elided `issue_context`, so it is preferred — but it falls back to the REST run read for any URL that is empty, unparseable, whitespace-bearing, containing `fishhawk_` (which `validateWireElisions` reads as a bounded MCP tool), or **not an `http(s)` URL with a non-empty host**. The absoluteness test **parses** rather than prefix-matching: `strings.HasPrefix(u, "https://")` accepts `https://` and `https:///issues/1`, neither of which names a retrievable surface, so the prefix form did not establish the invariant the guard documents. An `oversized_capable` elision must never be spelled without a retrievable unbounded surface. This is an **honesty** guard on rendered text, **not an egress control** — the server never fetches the URL and the same value already ships verbatim in `issue_context.url`, so a loopback or link-local host is deliberately left alone rather than degraded to the REST fallback for no gain.

### `fishhawk_list_runs` — the multi-row path

It sheds **within** rows first (R1..R4) and only then drops whole trailing rows. `list_runs` has **no `since_sequence` analogue**, so the `list_audit` remedy does not transfer; this surface takes the other admissible option: **drop trailing rows, BLANK the cursor, and point the stored elision at the UNBOUNDED REST enumeration** (`GET /v0/runs`) — never at the now-bounded `fishhawk_list_runs`. The same cursor rule and the same state-asserting counterfactual (`TestBoundListRuns_BlanksCursorOnTruncation`) apply. The bound runs **after** the `include_issue_context` strip and **regardless** of the flag: the flag is an opt-in projection, this is a bound by construction, so an `include_issue_context=true` enumeration is bounded too. A page can therefore come back with fewer runs than `limit` requested — called out because `list_runs` drives fan-out work, where a silently short page could be misread as "no more runs"; the elisions block always names the drop.

## Auth split (runner `fhm_*` vs operator `fhk_*` tokens)

Runner-side `fhm_*` tokens carry the `mcp:read` scope only: a write tool called from the runner side hits the bearer
middleware, resolves a read-only identity, and the handler-side role check returns 403 — the SDK surfaces it as a tool
error, no code change needed. Operator-side `fhk_*` apitokens carry the `write:runs` / `write:approvals` /
`write:stages` scopes.

## Schema-reflection trap (#371)

The `github.com/modelcontextprotocol/go-sdk/mcp` SDK (v1.6.1) auto-generates output schemas via Go reflection over the
tool's return type. `uuid.UUID` (which is `[16]byte`) renders as `type: array`, and `json.RawMessage` (which is
`[]byte`) does the same — but on the wire UUIDs are strings and per-category audit payloads are JSON objects, so schema
validation rejects every response. `client.go` therefore types UUIDs as `string` and `Artifact.Content` /
`AuditEntry.Payload` as `any`; callers `uuid.Parse` at the API-client boundary, and the plan decoder re-marshals +
unmarshals into the typed `PlanContent`.

## Cross-component E2E test (#371)

`backend/internal/integration/mcp/e2e_test.go` spins up Postgres + the real backend HTTP server, then builds + spawns
the actual `fishhawk-mcp` binary as a subprocess, fetches an `fhm_` token via the runner's HTTP shape, and exercises a
tool call end-to-end. The revocation + malformed-token assertions assert at the `mcptoken.Repository.Authenticate`
layer rather than at the tool call, because v0 reads don't enforce identity — the bearer middleware falls through to
anonymous; per-handler enforcement is out of scope for v0.

## `fishhawk_start_run` field parity (#426)

The start_run tool accepts the same local-runner convenience inputs the CLI's `fishhawk run start` does:

- `working_dir` walks for `.fishhawk/workflows.yaml` and ships the bytes inline (#411).
- `issue` shells to `gh issue view` and caches the payload (#415), **including the issue's `labels`** (E53.3 / #2226 — see below).
- `runner_kind=local` tags the run for the local-runner backend (ADR-022 / #388).
- `applies_to_override` + `applies_to_override_reason` force a run past the workflow's `applies_to` routing declaration (E53.3 / #2226 — see below).

Spec discovery / gh fetch live in `spec_discover.go` / `issue_fetch.go` — **local copies** of the CLI's helpers rather
than a shared package (the cli → backend import direction precludes the inverse). Auto-flip rule: when `trigger_source`
is defaulted (empty) and an issue resolves via `issue` or `trigger_ref=issue:N`, the MCP server flips it to
`github_issue`; an explicit `trigger_source` is preserved.

### `applies_to` routing and its audited override (E53.3 / [#2226](https://github.com/kuhlman-labs/fishhawk/issues/2226))

A workflow may declare `applies_to:` — a predicate saying which changes may be routed through it. `fishhawk_start_run`
is **fail-closed** against it: a `workflow_id` whose declaration the change does not satisfy is refused with
`422 workflow_not_applicable` **before any run row is created**, and the message names the workflow, the criterion that
failed, the value observed, and which workflows the change *would* satisfy.

Two inputs feed this from the MCP surface:

- **`issue` / `issue_context.labels`.** `fetchIssueViaGh` now requests `--json …,labels` and projects gh's label
  OBJECTS to names. Labels are the only producer for the predicate's `labels` criterion, so an omission here is
  indistinguishable from a mismatch: an **issue-less run resolves to an EMPTY label set and FAILS CLOSED**, with a
  message naming the absent issue context specifically rather than a generic predicate mismatch. Only the `labels` and
  `trigger` criteria are evaluated here; `paths` is deferred to the plan gate, where the approved plan's `scope.files`
  is the first authoritative (and *binding*) path set.
- **`applies_to_override` + `applies_to_override_reason`.** The sanctioned exception. The reason is **required** — a
  reasonless override is a `400`, and admits nothing. An accepted override records a `run_admitted_applies_to_override`
  audit entry carrying the reason verbatim; that **entry, not the request, is the override's source of truth**, and the
  plan gate looks it up to suppress its own deferred rejection. Set it whenever you intend to force the run through,
  **including when the labels/trigger criteria already pass**: `paths` is enforced later at the plan gate, so an
  override recorded only on an admission refusal would be unreachable in the common case of satisfied labels plus an
  out-of-declaration plan scope. Auditing the bypass is a **precondition** of granting it — if the entry cannot be
  written the run is refused with `503 audit_unavailable` and no run row is created, rather than admitted unaudited.
  That holds for **both** writes: the pre-insert grant creates no run row, and a failure of the post-insert
  **run-scoped** entry (the one the plan gate reads) CANCELS the just-created run and returns the same `503`. A run
  that outlived its own entry would carry a bypass no run-scoped audit read returns, and an admission-only
  (`labels`/`trigger`) violation has no second evaluation point to catch it. Either way: retry once the audit store is
  reachable.

Prefer amending the workflow's declaration (a reviewable change) over the override. The control prevents **misrouting**,
not a determined authorized caller: labels are fetched by the caller's `gh` and attested inline, so a server-side fetch
is the named hardening follow-up.

## Review-status internals (#600, count-gated #1127)

Each `ReviewStatus{Stage, Status, Reviews[], PollIntervalSeconds}` (`Status` one of `none` | `pending` | `complete` |
`skipped` | `failed`) is derived **entirely from the audit trail**.

**Completeness is count-gated (#1127).** The round is terminal only once `landed_terminal >= configured_agents` (the
latest `*_review_started` entry's `ConfiguredAgents`; ANY terminal kind — `reviewed`/`review_failed`/`review_skipped` —
counts). That is the SAME rule `checkPlanReviewSettled`/`checkImplementReviewSettled` use for the approval/merge gates,
so a poll catching the heterogeneous partial-landing window (reviewers run sequentially, each minutes long) reports
`pending` rather than `complete` with only the first reviewer's verdict. Once the count threshold is met, precedence
resolves the status (`reviewed → complete`, else `review_skipped → skipped`, else `review_failed → failed`) and
`Reviews[]` is the UNION of every decoded terminal row — one per configured reviewer. Below the threshold (or with no
terminal entry) the status is `review_started → pending`, else `none`. An absent/non-positive `ConfiguredAgents`
(old/malformed started payload) degrades to the prior complete-on-first-verdict predicate so the surface never strands
on `pending`. The `pending` state is the gap the existing `Reviews[]`/`ImplementReviews[]` slices couldn't express — it
subsumes a still-running review, a silently-failed/timed-out one, and the partial-landing window; those fields stay
populated unchanged (additive, no driver regression).

**Poll-authoritative contract (#879).** The 15s `poll_interval_seconds` hint is populated ONLY on `pending` (the one
state worth re-polling) and rides in via the shared `ReviewStatus` to every poll surface (`fishhawk_get_run_status`,
`fishhawk_get_plan`) from one edit.

**`fishhawk_await_review` internals.** It polls the existing `GET /v0/runs/{id}/audit` endpoint server-side on an
injectable interval (no new backend long-poll, no wall-clock sleeps in tests) until a terminal entry lands, the run
itself reaches a terminal state (the ADR-036 #874 non-stranding backstop — an inline `succeeded`/`failed`/`cancelled`
comparison against the fishhawk-mcp-local `Run.State`, so a verdict that will never land never holds the session open),
or the timeout fires. A `pending`-on-timeout result carries the `poll_interval_seconds` hint plus an actionable message
framing the re-call (or a switch to `get_run_status` polling) as the documented next step and naming
`FISHHAWKD_PLAN_REVIEW_TIMEOUT`. A 360s synchronous call may still hit a client/transport per-call timeout — acceptable
precisely because poll-the-handle is the blessed primary path and a cut-short await is a no-op the caller can re-issue.

**Raising the cap — `long_wait` (#2490).** The default cap is 600s; it rises to 7200s (2h) when EITHER a `progressToken`
is present OR the caller sets **`long_wait: true`**. A `progressToken` is client-supplied MCP request metadata a
tool-calling agent cannot set, so `long_wait` is the reachable knob that unlocks the same cap from a tool call (its
tradeoff — no keep-alive, so the client's idle timeout may still cut the call short — is a safe no-op because the wait
holds no state and is resumable). Both awaits also return **`heartbeat`** (true only when the client actually supplied a
`progressToken` and a per-tick keep-alive was emitted — a false value is the operator's evidence their client does not)
and **`timeout_cap_seconds`** (the cap actually applied), on every return path — turning an invisible
client-implementation fact into one the operator observes on every call. `long_wait` deliberately does NOT enable
emission: a progress notification MUST reference a token from an active request, so a token-less call emits nothing. The
shared clamp is `clampAwaitTimeoutHeartbeat` / `effectiveAwaitCap` in `review.go`; `fishhawk_await_audit` carries the same
`long_wait` input and `heartbeat`/`timeout_cap_seconds` output.
Native MCP Tasks is a deferred follow-up — the experimental `io.modelcontextprotocol/tasks` extension (SEP-2663) is
unimplemented in the pinned go-sdk ([go-sdk#626](https://github.com/modelcontextprotocol/go-sdk/issues/626)); this ships
the sync/poll fallback only. The #894 fix-up-boundary floor lives in `reviewStatusFor`. Implementation: `review.go`
(`reviewStatusFor`, the shared `decodeReviewVerdicts`/`decodeSkippedReviews`, the `awaitReview` handler).

## Await-audit internals (#962)

`fishhawk_await_audit`'s `run_terminal` status is distinct from `timeout` because some categories land at/after the
terminal transition, so the backstop resolves the wait only after ONE final anchored read that must win. Beneath it,
`GET /v0/runs/{id}/audit` gained an additive `since_sequence` query param — a strictly-greater filter applied before
pagination, in-memory like the #215 `stage_id` filter (`handleListRunAudit` in `backend/internal/server/reads.go`).
Implementation: `await_audit.go` (`awaitAudit`, `nextAuditEntry`, `awaitAuditRunTerminalBackstop`), reusing
`clampAwaitTimeout` / `reviewPollInterval` / `runStateIsTerminal`; the cross-boundary seam test is
`backend/internal/integration/mcp/await_audit_test.go`.

## Stage-wait internals (ADR-037 / #880; local dispatch default #1247)

Implementation: `stage_wait.go` (`stageStateIsTerminal`, `classifyStageWaitStatus`, `stageWaitStatusFor`, plus the E48.62 derivation core `stageWaitPollIntervalSeconds` / `stageElapsed` / `derivedStageWaitPollInterval`) — a LOCAL
terminal classifier that does NOT import `backend/internal/run`, mirroring `review.go`'s `runStateIsTerminal`. Callers
pass the already-fetched stage slice, so no extra `ListRunStages` round-trip is issued. `fishhawk_run_stage` adds
`stage_wait_status` on its post-run output.

**`next_actions` defaults a parked LOCAL implement stage to `fishhawk_dispatch_stage`** (with `fishhawk_run_stage`
demoted to an explicit blocking opt-in second entry), because the implement stage is the one stage type that can file a
mid-stage amendment a blocking call cannot decide in-band; the plan-local branch (no amendments) keeps the single
`run_stage` action and the `github_actions` poll branch is unchanged (#1247).

## `fishhawk_run_stage` internals and cancellation (ADR-024 / #434, runner half #435, compact #647)

The tool mirrors the CLI's `fishhawk runner start` argv composition; stdout is parsed line-by-line as JSONL and either
streamed as `notifications/progress` (when the client supplied a progress token) or accumulated for the final tool
result. The audit log carries the durable record.

`summarizeRunStageEvents` (the #647 compaction) walks the accumulated events once. `outcome`/`tokens_used` come from
the terminal `{"event":"runner_completed","outcome":…,"tokens_used":N}` event — the only runner-level terminal event
relayed on the JSONL **stderr** stream the relay reads; the bundle-only `kind=="invocation_end"` is deliberately NOT
keyed on, as it never reaches this stream. All summary scalars are `omitempty` so they reflect as plain JSON scalars
(no #371 array-reflection regression).

**Cancellation.** Tool-context cancellation sends `SIGTERM`, waits `runStageGracePeriod` (default 30s), then escalates
to `SIGKILL`. The runner-side half (#435) lives in `runner/cmd/fishhawk-runner/main.go::newRunnerContext` —
`signal.NotifyContext` registers SIGINT + SIGTERM, and the deferred cancel-emit at the top of `run()` overrides the
exit code to 130 (`exitCancelled`) and writes a `runner_cancelled` JSONL line so the MCP tool's progress stream sees a
clean terminator. The plumbed `ctx` reaches the long-running calls (`Invoke`, `IssueKey`, `FetchPrompt`, `ShipTrace`,
`ShipPlan`, `FetchMCPToken`, `openPRAndShipArtifact`) so cooperative cancellation works upstream and partial-trace
bytes ship best-effort when the cancel lands during agent invocation.

**Binary resolution:** `runner_binary` input > `FISHHAWK_RUNNER_BIN` env > `exec.LookPath("fishhawk-runner")` — matches
the CLI's resolver. The tool is registered unconditionally on every `fishhawk-mcp` deployment and returns a clean error
when the binary can't resolve; ADR-024 Q5 defers splitting into a `fishhawk-mcp-local` binary until hosted MCP becomes
a real concern.

**Transport implementation map:** `main.go` (`parseFlags`, transport dispatch in `run`) +
`http_transport.go` (`validateLoopbackAddr`, `bearerAuthMiddleware`, `serveHTTP`). `next_actions.go` holds
`nextActionsFor`, the pure classifier generalizing `review_action_hint`.

## Intake refinement internals (`fishhawk_draft_epic`, ADR-052, E34.4 / #1595)

`draft_epic.go` — the SINGLE operator MCP verb over the E34 refinement loop (reuse-first per E31.9: every existing
decision verb is stage-gated and resolves a run/stage, but a refinement session is neither a run nor a stage, so one
tool with arms keeps the registry at +1). The arms' 1:1 endpoint mapping:

| Arm | Trigger fields | Endpoint |
|---|---|---|
| open | `brief` alone | `POST /v0/refinement/sessions` |
| preview | `session_id` alone | `GET /v0/refinement/sessions/{id}` |
| edit | `session_id` + exactly one of `brief_amendment` \| `draft` | `PATCH /v0/refinement/sessions/{id}/draft` |
| decide | `session_id` + `decision` (`approved` \| `rejected`) + required `reason` | `POST /v0/refinement/sessions/{id}/decision` |
| file | `session_id` + `repo` | `POST /v0/refinement/sessions/{id}/file` |

`brief_amendment` is the agent re-draft, bounded by a per-session budget of **3**; `draft` is a direct strict-decoded
`EpicDraft` field edit, unbudgeted. Arm classification lives in `draftEpic`: it fails closed with NO HTTP call
(`armError` + the `legalArmsHelp` enumeration) when zero arms or an illegal combination is populated (e.g. `brief` with
any other field, both edit sub-arms, or >1 session sub-arm).

Every session-view result (open/preview/edit/decide) carries the `RefinementSession` mirror; the file arm carries
`RefinementFilingResult`; exactly one is set. Every result also carries a `session_guidance` block
(`guidanceForSession`/`guidanceForFiling`) — a next_actions-STYLE, tool-LOCAL block (deliberately NOT the run-scoped
`next_actions.go` machinery, since a refinement session has no run UUID) naming the exact next arm + arguments for the
derived state; the `awaiting_approval` guidance names any criteria-flagged child ordinals via
`flaggedCriteriaOrdinals`/`formatOrdinals`.

Backend error codes surface verbatim through typed `apiError` unwraps: `amendment_budget_exhausted`,
`decision_already_recorded`, `refinement_not_approved`, `refinement_draft_drifted`, `refinement_filing_repo_mismatch`,
and `refinement_filing_failed` — the last carrying the filed-so-far ordinals for a resumable re-invoke via
`filedSoFarDetail`. Wire mirrors (`RefinementSession`/`RefinementFilingResult`/`CriteriaPrecheck`) live in `client.go`
as #371-safe shapes (UUIDs typed `string`, no reflect-array pitfalls). Registered via `registerDraftEpic` in
`tools.go`, bumping the tool-count guard to **40** (`tools_test.go` `wantToolCount`).

**Auth:** `write:approvals` — NO new scope (the E34.2 precedent), so the operator token already driving
`fishhawk_approve_plan` works unchanged; a runner-side `fhm_` token (`mcp:read` only, per the auth split above) is
refused 403 — this is why the live draft→file walk is operator work, not implement-agent work.

**Agent-backed drafting is decoupled from the request lifetime (E37.4 / #1637).** The open + `brief_amendment` arms are
minutes-long drafting-agent calls, so `client.go` routes ONLY those two arms through a second `httpLong` client
(`refinementDraftClientTimeout` = 22m) while read/decide/file/direct-edit stay on the 30s short client, and
`backend/internal/server/refinement.go` runs the drafter+persist under `context.WithoutCancel` +
`WithTimeout(refinementDraftBudget = 20m)` (the #584 detached-review pattern) — so a mid-draft client disconnect
neither SIGKILLs the drafter nor strands a half-created session. The 22m client timeout sits above the 20m server
budget so the server's bounded error surfaces first.

## Campaign tool internals (ADR-047 / #1437, E25.8 / #1447, #1461; `operator_agent` override E25.12 / #1451)

`campaign.go` holds the operator-agent's campaign verbs over the E25.4 REST API — the campaign counterparts to the
single-run `fishhawk_start_run`/`fishhawk_get_run_status`/`fishhawk_resume_run` (see the Status list above for the
per-tool contract). Internals not covered there:

- **Epic ref, subset, and no-epic variants (#2003 / #2051).** `fishhawk_start_campaign` requires `repo` plus ONE of
  `epic_ref` / `items` — the tool rejects LOCALLY (no HTTP call) when both are empty; the backend owns the branch
  decision (the client forwards both fields, `epic_ref` WITHOUT `omitempty` so an empty ref reaches the server, which
  treats `""` as absent and routes to the no-epic branch). WITH `epic_ref`, `items` is the #2003 subset filter (each
  item must be a child of the epic). WITHOUT `epic_ref`, `items` is the #2051 no-epic issue list the campaign assembles
  over directly. The no-epic path fails-dangling for EVERY out-of-set `depends_on` target (it does NOT apply the #2120
  completion-satisfied refinement), and `issue_set_resolution_unsupported` (501) maps when the provider cannot resolve
  an arbitrary issue set.
- **`operator_agent` override (E25.12 / #1451).** `fishhawk_start_campaign`'s optional `operator_agent` — the
  campaign-level delegation override — is typed `map[string]any` on `StartCampaignInput` so the SDK's reflection-built
  input schema sees an unconstrained object; it is marshalled to opaque JSON the backend validates against
  `spec.OperatorAgent`. It wins WHOLESALE over every issue-run's per-workflow `operator_agent`. The `Campaign` wire
  mirror (`client.go`) carries `operator_agent` as `map[string]any` for the same unconstrained-object reason, so the
  create + status surfaces round-trip the override back.
- **Gate-code mapping.** Each verb maps the backend gate codes onto operator-actionable tool errors;
  `fishhawk_start_campaign_item_run` (E26.2 / #1481; `campaign_id` + `issue_ref` + `workflow_id` required, optional
  `workflow_ref` + `runner_kind` — pass `local` for the local loop — and `working_dir` (E48.69 / #2498): the absolute
  checkout path, REQUIRED for a `local` item, bound onto the minted run so `run_stage` / `dispatch_stage` /
  `run_children` / `drive_run` all inherit it. The tool refuses a `local` item with no `working_dir` and a
  non-absolute `working_dir` for any kind on BOTH transports — a strictly stronger rule than `start_run`'s HTTP-only
  guard) maps `item_not_eligible` / `item_human_led`
  (#1697) / `campaign_item_not_found` / `campaign_not_startable` / `campaign_run_start_failed`. As of E67.43 / #2681
  the `campaign_not_startable` message names the two REMAINING causes truthfully — a PAUSED campaign must be resumed
  (`fishhawk_resume_campaign`); a CANCELLED or SUCCEEDED campaign is closed, so drive the issue standalone with
  `fishhawk_start_run` — because a terminal-FAILED campaign is no longer refused: this verb REOPENS it (the campaign
  flips back to `running` and the item is reset in ONE atomic backend step), which is what makes the last-failed-item
  recovery reachable inside the campaign at all.
- **`next_actions` mapping.** `fishhawk_get_campaign_status` maps `next_action` onto a legal operator move via
  `campaignNextActionsFor`, so the agent never reads an unclassified state. `fishhawk_resume_campaign` is legal only
  when `next_action` is `resume`. The `attend_human_led` classification names the relabel-and-re-poll remedy: the
  `human_led` verdict is re-read from the issue's `autonomy:*` label on every `fishhawk_get_campaign_status` poll
  (E25.20 / #2355), so relabelling a child (`autonomy:low` → `autonomy:medium`) and re-polling moves it into `eligible`.
  The `closed` action (E67.43 / #2681) classifies as `campaign_closed` with exactly ONE suggested action — a STANDALONE
  `fishhawk_start_run` on the stranded `issue_ref`, never `fishhawk_start_campaign_item_run`, which the campaign gate
  would refuse — and its reason states the campaign will NOT track that run's outcome. Unlike `complete` (terminal, nil
  actions) this arm carries an action: there IS work left, just not campaign-tracked work. `closed` is in the
  classifier's enumerated closed set, so it no longer falls through to the `campaign_unclassified` fallback.
- **`fishhawk_cancel_campaign` (E25.20 / #2355).** `campaign_id` only. Marks the campaign AND every unfinished
  (non-terminal) item `cancelled` so an abandoned/rebuilt campaign stops showing as live work in the campaign list;
  it does NOT cancel the linked RUNS (`fishhawk_cancel_run` owns that). Idempotent + convergent — re-invoking after a
  partial failure completes the cancellation. Maps `campaign_not_cancellable` (already terminal) / `campaign_not_found`
  onto actionable tool errors.
- The campaign-scoped operator procedure lives in the operator role spec's `conventions.campaign`
  (`docs/spec/operator-role.md`), not hard-coded here.
- **Batch-as-campaign local drive.** The step-by-step operator procedure for driving a multi-issue batch
  locally as one campaign — `fishhawk_start_campaign` → a `fishhawk_get_campaign_status` drive-tick loop
  (the single status surface) → `fishhawk_start_campaign_item_run` with `runner_kind:local` AND an absolute
  `working_dir` (bound onto the minted run, so every later verb for that item inherits it) per eligible
  item → a per-item `fishhawk_drive_run` handoff, one item at a time with a per-item `scripts/dev post-merge`
  before the next item ([#1918](https://github.com/kuhlman-labs/fishhawk/issues/1918) governs the serialize
  rule) — is the `fishhawk://runbook` resource's **Batch-as-campaign (local campaign drive)** section
  (E48.12 / [#1959](https://github.com/kuhlman-labs/fishhawk/issues/1959)), validated over the epic-#1940
  campaign walk.

## Acceptance operator surface (E31.9 / #1537, ADR-049)

**No new MCP tools or endpoints** — the acceptance stage is exposed to the operator loop by REUSING the existing verbs
(the tool registry did not grow for it). Dispatch rides the ordinary agent path: `fishhawk_dispatch_stage` /
`fishhawk_run_stage` accept `stage=acceptance` (their jsonschema enums + `composeRunnerArgv` already pass `--stage`
through generically; acceptance takes neither `--plan-out` nor `--check-base-ref`, and its egress hosts + criteria ids
arrive via `--fetch-prompt`, not argv). Awaiting the verdict rides the category-generic `fishhawk_await_audit` on
`acceptance_outcome_recorded` / `acceptance_triage_decided` (`fishhawk_await_review` does NOT fit — it is
ReviewStatus-shaped and acceptance has no reviewers). There is NO operator approve/reject acceptance gate (ADR-049
decision #2 makes failure routing deterministic server-side triage, E31.8; decision #6 gates the MERGE via the
`acceptance_passed` condition — joined since #2347 by the equally merge-eligible `acceptance_not_validated`, which
records that the short-circuited stage verified ZERO criteria rather than certifying a pass).

**The real surface is `next_actions`**: `implementStageNextActions`' settled path branches to
`acceptanceStageNextActions` before the merge ritual when the run declares an acceptance stage — the per-state arms are
enumerated in the [Server-suggested next actions](#server-suggested-next-actions-next_actions-1024) section above. The
verdict + disposition are read from the `acceptance_outcome_recorded` / `acceptance_triage_decided` audit payloads (a
FAILED verdict leaves the stage `succeeded`, so stage state is never inferred from) via `latestAcceptanceVerdict` /
`latestAcceptanceTriageDisposition` over the recent-audit slice; the verdict/disposition vocabulary is **mirrored, not
imported** from `backend/internal/server/acceptance.go` (the #875 compile trap), pinned by
`TestAcceptanceVocabularyMatchesBackend`. `fishhawk_get_run_status` adds `acceptance_stage_wait_status`
(`stageWaitStatusFor(…, "acceptance", …)`, omitted for non-acceptance runs). The acceptance playbook
(verdict-vs-stage-state, the deterministic triage table, the LOCAL-runner explicit-re-dispatch rule, paged arbitration)
lives in the `fishhawk://runbook` resource + the server `instructions`.


## Amending acceptance criteria at approval (`fishhawk_approve_plan` → `amend_acceptance_criteria`)

Plan-approval conditions reshape the design but never rewrite the approved plan's
acceptance criteria, so the acceptance stage can validate the shipped behaviour
against a superseded contract and fail a correct implementation (#2581).
`fishhawk_approve_plan` therefore accepts `amend_acceptance_criteria` alongside
`reason`: a list of `{id, action, reason, statement?}` recorded on the SAME
approval row as the conditions that motivated it.

- `action: "retire"` — the criterion is no longer validated. An acceptance
  verdict whose failures name ONLY retired criteria is recorded `passed`
  (`verdict_reported` / `downgrade_basis` / `retired_criterion_ids` ride the
  `acceptance_outcome_recorded` payload).
- `action: "restate"` — replaces the statement and keeps the criterion LIVE. A
  restated criterion still fails if it genuinely fails; restatement is not a
  silencing channel.
- `reason` is REQUIRED per criterion — it is the reconstructable why, and it is
  rendered into the acceptance prompt's retired block.

**Anti-silencing refusals** (all PRE-insert, so a corrected retry flows
normally): `400 validation_failed` with `details.field=amend_acceptance_criteria`
and `details.rule` naming the specific refusal (`unknown_criterion_id`,
`unknown_action`, `reason_required`, `statement_required`, `duplicate_id`,
`already_retired`, `amendment_not_approve_plan_stage`); `422
acceptance_criteria_all_retired` when the amendment would retire EVERY criterion
— evaluated on the union of prior recorded retirements and this request's, so it
fires cumulatively too, and there is no override (re-plan instead of emptying the
contract); `422 acceptance_criteria_unavailable` when the plan carries no
acceptance criteria or its prior amendments cannot be read (fail-closed).

**Derived retirement is deliberately NOT provided.** Retiring a criterion because
its subject is a file dropped by `remove_scope_files` is a natural-language
judgement no token rule decides, and a silent retirement converts a loud
acceptance failure into silence. Instead the dropped paths are rendered into the
acceptance prompt as contested context with a skip-not-fail instruction, so the
validator judges subjecthood where it can read both the criterion and the
observed behaviour. Use this channel for the unambiguous cases; you do not need
it merely because a path was dropped.
