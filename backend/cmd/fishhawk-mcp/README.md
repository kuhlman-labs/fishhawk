# fishhawk-mcp

Model Context Protocol server that exposes Fishhawk run / plan / audit state to Claude Code (and any other MCP-compatible client) per [ADR-021 / #322](https://github.com/kuhlman-labs/fishhawk/issues/322).

Two audiences share one surface:

- The **in-runner Claude Code agent** reads its own run's state mid-execution — what's the active plan, what audit entries fired for the current retry, what constraints apply. Closes the agent-is-blind-to-Fishhawk-state gap that motivated ADR-019.
- The **interactive Claude Code session** — an engineer asking "what's the status of my current run" — gets the answer through natural language without a CLI alt-tab.

The v0 surface began read-only; action verbs (approve, reject, retry, cancel, start, run_stage, the implement-review fix-up below, and the run-branch reset below) have since landed as scoped write tools so the loop can be driven end-to-end from the agent session. Write tools require an operator-side token with the matching `write:*` scope; a run-bound runner token is restricted to its own run.

## Status

E19.2 / #342 shipped scaffolding + handshake. E19.3–E19.6 landed the v0 tool surface (all read-only per ADR-021):

- `fishhawk_get_active_run` (E19.3 / #343) — the "which run" resolver: use it when you hold a `pr_number`, `trigger_ref`, or `FISHHAWK_RUN_ID` env but need the run UUID the other tools take.
- `fishhawk_get_plan` (E19.4 / #344) — read the approved standard_v1 plan artifact after the plan stage and before approve/reject; walks `parent_run_id` up to 8 levels for CI-retry chains. Carries the plan-gate results alongside the plan: `scope_precheck` (#658), `surface_sweep` (#763), and `test_sweep` (#942) — the last flags EXISTING test files the plan omitted (a stem-sibling test, existing tests in a package gaining a new test file, or a path-trigger rule's pinned test — `migration_walk`: a scoped `migrations/*.sql` requires `backend/internal/postgres/postgres_test.go`); judge before approving whether the changed behavior's tests live there, since the runner scope_drift-excludes edits to unscoped files. `surface_sweep` also carries `cross_slice_findings` (#1102): when a decomposition splits a lockstep pattern's member files across two or more distinct slices (e.g. a schema's canonical and mirror copies landing in different slices), each finding names the pattern and which slice owns which files — the inverse of the same-file-in-two-slices gate (#1062), where the fix is consolidating the seam into one slice rather than declaring the shared file twice, because completing a split seam otherwise needs a runtime scope amendment that can time out (#1035).
- `fishhawk_get_run_status` (E19.5 / #345) — the agent's "where are we" query: bundles Run + ordered stages + recent audit (time-descending) into one call. Also carries `plan_review_status` + `implement_review_status` (`none`/`pending`/`complete`/`skipped`/`failed`) and `plan_stage_wait_status` + `implement_stage_wait_status` (`pending`/`running`/`succeeded`/`failed`/`cancelled`). **Re-polling this tool is the authoritative way to reach a terminal review *or* stage-execution status (#879/#880)**: on a non-terminal status each carries a server-suggested `poll_interval_seconds` (15s for reviews, 30s for stage execution) — re-call on that cadence until the status goes terminal. See [Stage-execution wait contract](#stage-execution-wait-contract-adr-037-880). The run row also carries `run.concerns` when the run has **open** review concerns (#964): the open count, a `by_state` breakdown, and `items[]` with each concern's **stable id** — the primary addressing scheme for `fishhawk_fixup_stage`'s `concern_ids`. Drive-enabled runs (#1023) additionally get a top-level `drive_status` block: `auto_advanced` (`[{rule, from, to, parked?, ts}]`, oldest first — the transitions the backend advanced itself, distilled from `run_auto_advanced` audit entries; `parked` marks a runner_kind-`local` dispatch that recorded a ready-to-run next action instead), `next_action` (`{action, detail?, pr_url?}` — the distilled operator next step, e.g. `run_implement_stage` or `merge_pr`; omitted on terminal runs), and `derived_status` (`awaiting_merge` when every gate is resolved and required PR checks are green on an open PR — presentation-only, `run.state` stays `running`). The block is omitted entirely for non-drive runs. Decomposed parents (#1147) additionally get a `children_status` block (per-child live state + the fan-in `integration_phase`) — see [Decomposed-parent observability](#decomposed-parent-observability-children_status-1147). Every run additionally carries a `next_actions` block (#1024) — see [Server-suggested next actions](#server-suggested-next-actions-next_actions-1024). Runs with cost data additionally get a best-effort, display-only `cache_efficiency` block (ADR-044 slice 3 / #1352): the run's prompt-cache `cache_read_ratio` (share of input served from cache), `reuse_factor` (re-reads per cache-write token), and `gross_read_savings_usd` / `write_penalty_usd` / `net_savings_usd`, plus a per-stage (`plan_review` / `implement_review` / `agent`) breakdown — derived from the `cost_recorded` audit ledger via `GET /v0/runs/{run_id}/cache-efficiency`. Omitted when the run has no cost data; it never gates a run.
- `fishhawk_await_review` (#600) — OPTIONAL convenience block over that poll: blocks until a stage's review reaches a terminal state. Default timeout **360s** (recalibrated from 120s to exceed the measured 3.5–4.5min review latency and the 300s reviewer budget, #878), cap 600s. Never strands — it also resolves when the run itself goes terminal (ADR-036 #874). Idempotent/resumable: a timeout returns `pending` + the `poll_interval_seconds` hint; re-call to resume, or switch to `fishhawk_get_run_status` polling (the primary path).
- `fishhawk_await_audit` (#962) — the sequence-anchored await primitive: blocks until the next audit entry with the given `category` and sequence strictly greater than `since_sequence` lands, and returns that entry. The anchoring contract makes the wait race-free: an event that happens after another always has a strictly greater audit sequence, so "the review after the fix-up" is the `implement_reviewed` entry with sequence > the `fixup_pushed` entry's sequence — a stale pre-fix-up verdict can never satisfy the wait (the #894 class of stale-read race). Inputs `{run_id, category, since_sequence (default 0), timeout_seconds (default 360, cap 600 — same clamp as await_review)}`. Statuses: `found` (with `entry` + `latest_sequence`), `timeout` (gapless re-arm: re-call with `since_sequence` = the returned `latest_sequence`, == your anchor when nothing landed, and no entry can be skipped), `run_terminal` (the ADR-036 non-stranding backstop fired after one final anchored read — do not re-arm blindly). `fishhawk_await_review` stays unchanged as the review-specific convenience; re-polling `fishhawk_get_run_status` remains the authoritative fallback (ADR-037).
- `fishhawk_list_audit` (E19.6 / #346) — use when you need the filtered or paginated audit trail (category, stage_id) rather than the recent slice — e.g. to read an `implement_reviewed` concern's full note text. Mirrors the CLI's `fishhawk audit list`. (For fix-up addressing, prefer the stable concern IDs on `run.concerns` over audit-entry indices, #964.)
- `fishhawk_list_runs` (E22.5 / #394) — the "what runs do I have" enumeration: filter by `repo` / `workflow_id` / `state`, walk pages via the opaque `cursor`. Mirrors the CLI's `fishhawk run list`. **Compact by default (#1098):** each run's `issue_context` (issue body + every comment) is omitted from the list response so a single `list_runs` over issues with large bodies/comment threads stays within the tool-result token cap — the overflow that forced a `curl`+`jq` fallback when enumerating child run IDs during decomposition fan-out. Pass `include_issue_context: true` to re-include the full payload when it is actually needed. (`fishhawk_get_active_run` / `fishhawk_get_run_status` resolve a single run and are unaffected.)
- `fishhawk_file_issue` ([#1005](https://github.com/kuhlman-labs/fishhawk/issues/1005)) — file a work item (issue, bug, chore, ADR) through the repo's work-management conventions. The consistent cross-repo/cross-platform filing surface and the operator-agent follow-up-filing path ([ADR-040](https://github.com/kuhlman-labs/fishhawk/issues/1004)). See [Work-item filing](#work-item-filing-fishhawk_file_issue-1005).
- `fishhawk_report_product_issue` ([#1006](https://github.com/kuhlman-labs/fishhawk/issues/1006)) — file an upstream Fishhawk **product** bug/feature carrying an auto-collected, redacted, fingerprint-deduped diagnostic bundle. The first **write** tool that drives an egress on the run's chain. See [Product feedback](#product-feedback-fishhawk_report_product_issue-1006).
- `fishhawk_consolidate_slices` ([E24.2 / ADR-041 / #1238](https://github.com/kuhlman-labs/fishhawk/issues/1238)) — run the decomposed-parent fan-in on demand when a parent is stuck in `awaiting_children` after its children all succeeded on the **local** runner (the 60s sweeper backstop is off by default there). See [Local decomposition fan-in](#local-decomposition-fan-in-fishhawk_consolidate_slices-1238).
- `fishhawk_decide_scope_completeness` ([E22.X / #1231](https://github.com/kuhlman-labs/fishhawk/issues/1231)) — resolve an implement stage parked in `awaiting_scope_decision`: **exempt** the already-committed tree (open the PR from the held commit with **no agent re-run**) or **fail** it to category-B. The zero-re-run recovery for a missing-declared-scope-file-only gate failure. See [Scope-completeness park](#scope-completeness-park-fishhawk_decide_scope_completeness-1231).

E19.7 / #347 wires the binary into the release pipeline next.

## Build

From the repo root (workspace-aware):

```sh
go build ./backend/cmd/fishhawk-mcp/...
```

The binary lands at `./fishhawk-mcp` when built explicitly:

```sh
go build -o fishhawk-mcp ./backend/cmd/fishhawk-mcp
```

## Configuration

Two env vars; both honored from the OS environment when the binary launches.

| Variable                | Required | Default                 | Notes                                                                                                                                                                                                  |
| ----------------------- | -------- | ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `FISHHAWK_API_TOKEN`    | **yes**  | —                       | Bearer token. Generated via the backend's API-token surface. No "anonymous" mode — unlike the CLI's dev path, MCP tools always round-trip the API and running unauthenticated would be a silent permission bug. |
| `FISHHAWK_BACKEND_URL`  | no       | `http://localhost:8080` | Same fallback as the CLI. Trailing slash is stripped.                                                                                                                                                  |

The binary exits non-zero on startup if `FISHHAWK_API_TOKEN` is missing.

## Transport (`--transport` / `--addr`)

Two transports, selected by flag. **stdio is the default and unchanged** — every existing per-client subprocess consumer (Claude Code, Codex) keeps working with no flags.

| Flag | Default | Notes |
| --- | --- | --- |
| `--transport` | `stdio` | `stdio` \| `http`. `http` is the opt-in [ADR-033](https://github.com/kuhlman-labs/fishhawk/issues/843) option-b streamable-HTTP transport ([#927](https://github.com/kuhlman-labs/fishhawk/issues/927)). |
| `--addr` | `127.0.0.1:8765` | `host:port` for `--transport http`; ignored for stdio. **Loopback-only** — see below. A bind collision surfaces as an operator-visible error. |

```sh
fishhawk-mcp --transport http --addr 127.0.0.1:8765
```

The HTTP transport is a **single-operator local shared endpoint, NOT multi-tenant** — a hosted/remote MCP server is [#655](https://github.com/kuhlman-labs/fishhawk/issues/655), out of scope here. Two enforcements back that posture:

- **Loopback-only bind.** `--addr` is validated before any bind: a literal IP must be loopback (`127.0.0.0/8` or `::1`), an empty host clamps to `127.0.0.1`, and a hostname is rejected unless **every** resolved IP is loopback (so `localhost` aliased to a routable address can't slip through). `0.0.0.0` and any routable address fail fast with a precise error.
- **Per-request bearer.** Every request must carry `Authorization: Bearer <FISHHAWK_API_TOKEN>`, compared in constant time. A missing/malformed/mismatched header gets `401` with `WWW-Authenticate: Bearer`. Loopback is explicitly **not** a trust boundary — co-tenant local processes still need the token.

The go-sdk's own DNS-rebinding protection (rejecting a non-loopback `Host` header) stays enabled; the loopback bind + bearer gate are independent of it. Tool registration is identical across both transports.

## Install (operators)

Pre-built binaries ship with every `mcp/vX.Y.Z` GitHub Release: darwin-arm64, darwin-amd64, linux-amd64, linux-arm64. Full install path including cosign verification and `claude mcp add` registration lives at [`docs/mcp/install.md`](../../../docs/mcp/install.md).

Short version for operators on Apple Silicon Macs:

```sh
curl -fSL "https://github.com/kuhlman-labs/fishhawk/releases/download/mcp/vX.Y.Z/fishhawk-mcp-vX.Y.Z-darwin-arm64" \
  -o /usr/local/bin/fishhawk-mcp
chmod +x /usr/local/bin/fishhawk-mcp
export FISHHAWK_API_TOKEN="<token>"
claude mcp add fishhawk --command /usr/local/bin/fishhawk-mcp \
  --env FISHHAWK_API_TOKEN=$FISHHAWK_API_TOKEN
```

## Release pipeline

`.github/workflows/mcp-release.yml` (E19.7 / #347) — triggered by `mcp/v*` tags. Re-runs lint + tests at the tag commit, cross-builds the four-platform matrix with CGO disabled, generates an SPDX-JSON SBOM, signs `SHA256SUMS` with cosign keyless, publishes the GitHub Release.

Verification (after `cosign install`):

```sh
cosign verify-blob \
  --certificate-identity-regexp 'https://github.com/kuhlman-labs/fishhawk/\.github/workflows/mcp-release\.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --signature SHA256SUMS.sig \
  --certificate SHA256SUMS.pem \
  SHA256SUMS
```

## Stage-execution wait contract ([ADR-037](https://github.com/kuhlman-labs/fishhawk/issues/879), #880)

The durable `(run_id, stage_id)` handle is the unit of waiting on a stage's execution. `fishhawk_get_run_status` carries `plan_stage_wait_status` + `implement_stage_wait_status` — each a `StageWaitStatus` whose `status` is one of `pending`/`running`/`succeeded`/`failed`/`cancelled`, derived from the stage row (distinct from the `*_review_status` pair, which tracks a stage's **review** rather than its execution).

- **Poll the handle (primary, authoritative).** Re-polling `fishhawk_get_run_status` is the blessed way to await a stage's terminal status. While the status is non-terminal (`pending`/`running`) the `StageWaitStatus` carries a server-suggested `poll_interval_seconds` of **30s** — coarser than reviews' 15s because stages run minutes, not seconds. Re-call on that cadence until the status goes terminal. The interval is dropped once the run itself is terminal (ADR-036 [#874](https://github.com/kuhlman-labs/fishhawk/issues/874) backstop), so a stage that can no longer progress never advertises an unbounded poll.
- **Synchronous-with-progress `fishhawk_run_stage` (negotiated fallback).** The synchronous call runs the stage to completion and returns the terminal outcome (also surfacing `stage_wait_status` on the handle — normally already terminal, so the interval is omitted). It is the fallback for clients that prefer to block or for short stages; it is not the primary mechanism. Its run-terminal result also carries the `next_actions` block (#1024) so the operator gets the legal next move directly — see [Server-suggested next actions](#server-suggested-next-actions-next_actions-1024).
- **Non-blocking `fishhawk_dispatch_stage` ([#1232](https://github.com/kuhlman-labs/fishhawk/issues/1232)).** The SDK-independent dispatch verb spawns the runner **detached** and returns the `(run_id, stage_id)` handle plus a non-terminal `stage_wait_status` **immediately**, so a **single** MCP session can poll `fishhawk_get_run_status` to terminal AND decide a mid-stage scope amendment in-band between polls (`fishhawk_decide_scope_amendment`) — the durable fix for the [#1189](https://github.com/kuhlman-labs/fishhawk/issues/1189) amendment timeout. It ships the poll-to-terminal UX today and **supersedes the interim `fishhawk run auto-decide` second channel** ([#1233](https://github.com/kuhlman-labs/fishhawk/issues/1233)/[#1234](https://github.com/kuhlman-labs/fishhawk/issues/1234)) for in-band mid-stage amendment decisions. See [Non-blocking dispatch](#non-blocking-dispatch-fishhawk_dispatch_stage-1232) below.
- **Native MCP Tasks (`invocationMode: async`) — deferred.** A future mode that lets `fishhawk_run_stage` return a handle immediately and poll to terminal is **not built** here: it is gated on [ADR-033](https://github.com/kuhlman-labs/fishhawk/issues/843) transport plus MCP Tasks leaving experimental (ADR-037 two-phase delivery). It would be a later transport refinement layering onto the same `(run_id, stage_id)` handle that `fishhawk_dispatch_stage` already returns.

## Progress notifications (`fishhawk_run_stage`)

`fishhawk_run_stage` spawns the runner and relays its stderr JSONL lines as MCP `notifications/progress` updates — but **only when the client supplied a `progressToken`** on the call (the MCP opt-in progress model; without a token the runner's events are still returned post-hoc in the final result's `events` list).

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
2. Poll `fishhawk_get_run_status` on the advertised `poll_interval_seconds` (30s) until the stage's `*_stage_wait_status` goes terminal.
3. **Between polls**, when a `scope_amendment_pending` surfaces, call `fishhawk_decide_scope_amendment` — so the runner's amendment `?wait` poll resolves **before its window elapses**, with no failed-stage retry.

This is what a **single** MCP session needs: a blocking `fishhawk_run_stage` call cannot decide an amendment the same agent's runner files mid-stage. `fishhawk_dispatch_stage` **supersedes the interim `fishhawk run auto-decide` second channel** ([#1233](https://github.com/kuhlman-labs/fishhawk/issues/1233)/[#1234](https://github.com/kuhlman-labs/fishhawk/issues/1234)) for that decision.

Detached-spawn properties (differ deliberately from the synchronous `spawnRunnerStage`):

- **Own process group** (`Setpgid`): a `SIGINT`/`SIGTERM` to the MCP server's foreground group is **not** forwarded to the runner — it is meant to outlive the tool call. There is **no** SIGTERM→grace→SIGKILL watcher.
- **Output → a per-invocation log file** under `os.TempDir()` (`fishhawk-runner-<run>-<stage>-<unixnano>.log`), **never a pipe**: an unread pipe fills its kernel buffer and blocks the writer once full (#446). The runner ships its trace via `--upload-trace` and its state to the backend, so the local log is a diagnostic only. `log_path` is returned for that diagnostic.
- **A reaper goroutine** (`go func(){ _ = cmd.Wait() }()`) collects the child's exit so it never zombies while the tool returns.

Restarting the MCP server (`scripts/dev reload`) while a detached stage is in flight **orphans** the runner (reparented to init) but it continues to terminal and stays pollable via `fishhawk_get_run_status` — the intended durability of the `(run_id, stage_id)` handle (ADR-037), not a regression. Requires the `fishhawk-runner` binary to resolve on the MCP host, exactly like `fishhawk_run_stage`.

## Parallel decomposed children (`fishhawk_run_children`, [#1144](https://github.com/kuhlman-labs/fishhawk/issues/1144))

`fishhawk_run_children` is the fan-out sibling of `fishhawk_run_stage`: where `run_stage` drives **one** stage of **one** run, `run_children` drives **all** of a decomposed parent's pending children **concurrently**. Pass the decomposed **parent's** `run_id`; the tool:

- **Discovers** the children from the parent's `plan_decomposed` audit entry (`child_run_ids` + `effective_max_parallel`); a run with no such entry is a clean error (it is not a decomposed parent).
- **Partitions** by freshly-read state — only `pending` children are spawned; in-flight and terminal children are reported as-is, so a re-invocation is **idempotent**.
- **Spawns** each pending child's implement stage as a `fishhawk-runner` subprocess (the same `spawnRunnerStage` process-group/SIGKILL core `run_stage` uses) with `--parallel-isolate` appended, so each child provisions its **own isolated per-child git worktree** (`run-<child>`) — concurrent siblings, which already own distinct per-slice sole-writer branches (E24.1), never race a shared checkout, and the operator's tracked tree stays untouched.
- **Bounds** concurrency with an `errgroup` whose limit is the orchestrator-resolved effective cap, **clamp-DOWN-only** against an optional `max_parallel` override (it can lower an unlimited/looser cap, never raise it; `effective_max_parallel == 0` means unlimited and skips the limit).
- **Awaits ALL with no sibling-cancel.** A child failure is **data**, not a tool error: every child is awaited and surfaces in `children[]` with its `exit_code`, `outcome`, and `stage_state` regardless of success.

Returns `children[]` (one entry per discovered child, in `plan_decomposed` order), `dispatched_count` (how many were pending and spawned), and `effective_cap` (the cap used; 0 = unlimited). Requires the `fishhawk-runner` binary to resolve on the MCP host, exactly like `fishhawk_run_stage`.

### Decomposed-parent observability (`children_status`, [#1147](https://github.com/kuhlman-labs/fishhawk/issues/1147))

For a **decomposed parent**, `fishhawk_get_run_status` carries a `children_status` block so the operator sees the fan-out's live progress instead of a bare `awaiting_children`:

- `children[]` — one entry per discovered child (`{run_id, slice_index, state}`) in `plan_decomposed` (slice-index) order. `state` is the child run's lifecycle state (`pending`/`running`/`succeeded`/`failed`) or `unknown` when that child's read failed. Aggregate counts (`total`/`pending`/`running`/`succeeded`/`failed`) accompany it.
- `integration_phase` — the fan-in phase classified from the `slices_integrated` / `slice_integration_conflict` audit kinds (ADR-041 / #1142): `running_children` (a child is still in flight), `ready_to_integrate` (all children succeeded, no fan-in yet), `integrated` (a clean fan-in — `consolidated_branch` is surfaced), or `integration_conflict` (a slice branch failed to merge — `conflicting_child_run_id` is surfaced).
- **Best-effort:** a per-child read failure degrades that child to `state="unknown"` and never fails the snapshot.
- **Cost-gated:** the per-child fetch runs only for a top-level run (no `parent_run_id`) whose implement stage is `awaiting_children` **or** whose recent-audit window carries a decomposition marker (`plan_decomposed` / `slices_integrated` / `slice_integration_conflict`). An ordinary run makes **zero** extra calls (no `plan_decomposed` read), and the block is omitted for non-decomposed runs. The `next_actions` `implement_awaiting_children` arm points the operator at `fishhawk_run_children` plus this block.

## Server-suggested next actions (`next_actions`, #1024)

`fishhawk_get_run_status` and the run-terminal `fishhawk_run_stage` result both carry a `next_actions` block — the generalization of `review_action_hint` (#777/#860) across the whole run lifecycle. The classifier (`next_actions.go`) is a pure function over data the tools already fetch (run row, stage rows, review statuses, the computed hint, the drive read view): no extra backend round-trip, no new endpoint.

Shape: `{state, actions[]}`. `state` is the classified lifecycle state (`plan_gate_parked`, `implement_failed_category_b`, `implement_concerns_open`, `succeeded_pr_open`, …; terminal runs name the run state with no actions; an unmatched non-terminal state classifies `unclassified`). Each action entry carries:

- `action` — the tool to call (`fishhawk_resume_run`, `fishhawk_fixup_stage`, …) or a named ritual step outside the MCP surface (`approve_pr`, `merge_pr`, `post_merge`, `merge_and_file_follow_up`, `file_product_issue`);
- `params` — key parameters (`run_id`, `stage_id`, `parent_run_id`, the `concern_ids` source);
- `precondition` — when the action is legal;
- `consumes` — what taking it spends: `none` | `fixup_budget` | `retry_budget` | `approval_slot` | `new_run`;
- `reason` — one-line why-this-now.

Invariants:

- **Display-only, never gates** — like the periodic-budget block and the hint it generalizes, the block is advisory; the server-side applicability predicates stay authoritative.
- **A non-terminal run always carries ≥ 1 action.** Any state the table does not match falls back to `unclassified` (re-poll + file a product issue naming the state), structurally — never an empty list.
- **The concern arm derives from the hint computation** (`ReviewActionHint.suggestedActions`), so `review_action_hint` and `next_actions` cannot disagree on the remaining fix-up budget or override availability.
- **Drive folds first**: on drive-enabled runs the `drive_status.next_action` is prepended, so drive and `next_actions` never point different ways.
- **Decomposed parent at `awaiting_children`** (#1147) classifies `implement_awaiting_children` — a dedicated arm offering `fishhawk_run_children` (fan out the still-pending children) plus a poll pointing at the `children_status` block for each child's live state and the fan-in/integration phase.

## Implement-review fix-up (`fishhawk_fixup_stage`)

`fishhawk_fixup_stage` (E22.X / [#762](https://github.com/kuhlman-labs/fishhawk/issues/762)) routes one or more **advisory implement-review concerns** ([ADR-027](https://github.com/kuhlman-labs/fishhawk/issues/703) `approve_with_concerns`) back to the implement agent for a single fix-up pass, instead of the operator hand-editing the PR branch. It wraps `POST /v0/stages/{stage_id}/fixup`.

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
| `n` | **yes** | The child number for the `[E<epic>.<n>]` title. Operator judgment, mirroring how `fishhawk_file_issue` takes `{n}`. |
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

A non-conflict failure returns `slice_integration_error` (502) with the cause in `details.error` — the diagnosability the event-driven fan-in path lacks.

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

## Scope amendment at approval (`fishhawk_approve_plan` → `add_scope_files`)

`fishhawk_approve_plan` (E22.4 / [#393](https://github.com/kuhlman-labs/fishhawk/issues/393)) takes an optional `add_scope_files` array ([#824](https://github.com/kuhlman-labs/fishhawk/issues/824)) — the **structured, authoritative** way to add files to the implement stage's `scope.files` at approval time. On approve the named paths are recorded on the approval audit payload and folded into the implement stage's effective scope by the prompt builder, so a reviewer-authorized edit ships as a declared path rather than surfacing as benign `scope_drift`.

Prefer it over naming paths in the free-text `reason`. The `reason` fold ([#730](https://github.com/kuhlman-labs/fishhawk/issues/730)) is a best-effort regex scrape kept only as a fallback; it silently misses:

- **directories** — pass a trailing slash (e.g. `pkg/testdata/corpus/`); every created file under that prefix stages.
- **extensionless and repo-root files** — e.g. `go.work`, `Makefile`.
- **described-but-not-spelled paths** — anything the prose names in words rather than as a literal path token.
- **absolute / non-repo-relative tokens** — the fold now silently skips any token that is absolute (leading `/`) or contains a `..` traversal segment ([#1155](https://github.com/kuhlman-labs/fishhawk/issues/1155)), so naming a `/tmp` path or an exclusion in prose no longer injects a phantom scope entry. Only clean repo-relative paths fold; use `add_scope_files` for an authoritative add.

`reason` and `add_scope_files` compose: the structured paths fold first (authoritative), then the prose fold runs as a fallback, both deduping by path. Both no-op when the plan declares an empty scope, preserving the runner's `git add -A` fallback. `add_scope_files` does **not** weaken the policy gate — a folded path that matches `forbidden_paths` still fails category-B against the produced diff.

**Duplicate-submission labeling ([#986](https://github.com/kuhlman-labs/fishhawk/issues/986)).** A re-submission by the same subject — `fishhawk_approve_plan` or `fishhawk_reject_plan` against a stage that subject already decided — is a no-op the tools label explicitly instead of rendering as a normal result: the output carries `duplicate_submission: true` plus `prior_decision` (the existing row's), and the result text leads with a banner stating the prior decision stands, the stage state is unchanged, and the budget/scope gates were NOT re-run. The override markers (`--override-budget` / `--override-scope-cap`) are honored because both gates now run **pre-insert**: a 422 refusal records no approval row, leaving the submission slot free for the override retry.

**Scope-cap gate ([#983](https://github.com/kuhlman-labs/fishhawk/issues/983)).** A plan-stage approve is refused `422 plan_violates_scope_cap` when the effective scope — plan `scope.files` ∪ `add_scope_files` ∪ approved amendments, deduped by exact path — exceeds the implement stage's `max_files_changed`. The refusal inserts no approval row, so a retry after re-scoping flows normally; to force it through (declared scope is an upper bound, and the cap may legitimately be about to change), include the `--override-scope-cap` marker in the comment, which records a `plan_scope_cap_override_acknowledged` audit entry — the same posture as `--override-budget`. Read headroom before approving: `fishhawk_get_plan`'s `scope_precheck` now carries `max_files_changed` alongside `scanned_files`.

**Binding assertions ([#1171](https://github.com/kuhlman-labs/fishhawk/issues/1171)).** `fishhawk_approve_plan` also takes an optional `binding_assertions` array — the **machine-checkable** half of an approval condition, the deterministic complement to the free-text `reason` fold. Where `reason` is restated to the implement agent as binding conditions (#558) and `add_scope_files` widens the scope, `binding_assertions` declares checks the runner enforces: each entry is `{type, path, literal}` where `type` is `file_contains` or `test_asserts` (open enum), `path` is repo-relative (and must end in `_test.go` for `test_asserts`), and `literal` is a non-empty substring that must appear in the committed file. On approve they are recorded on the approval audit payload alongside `add_scope_files` and echoed on the implement prompt-response; the runner evaluates each as a deterministic substring check against the committed scope-only tree post-implement, and any unsatisfied assertion fails the implement stage category-B. Substring matching only — never parses prose, so a literal chosen too loosely is an operator-declaration concern, not a gate defect. A malformed declaration (unknown `type`, empty `literal`, a `test_asserts` path not ending in `_test.go`) is rejected `400 validation_failed` before any approval row is recorded. Omitting the field is byte-identical to today.

**Implement-model override ([#1013](https://github.com/kuhlman-labs/fishhawk/issues/1013)).** `fishhawk_approve_plan` also takes an optional `implement_model` string — the operator's override for the implement-stage model, the top rung of the resolution ladder `deployment default < spec executor.model < plan model_recommendation < this override`. On a plan-stage approve the backend resolves the full ladder with this as the operator rung, validates the **resolved** value against the deployment's per-adapter allow-list, and records the choice as a `model_resolved` audit entry (`{model, model_source}`) that the runner spawn routes to the agent's `--model`. An unknown resolved model — from any rung, not just this override — is rejected `422 plan_invalid_model` (details carry `model`, `model_source`, `adapter`), pre-insert so a retry with an allowed `implement_model` flows normally. An empty/unconfigured allow-list fails **open** (any model accepted, byte-identical to today). Omit the field to ratify the plan's `model_recommendation` or fall through to the spec/deployment default; an empty resolution still records `model_resolved` and spawns with no `--model`, exactly as today.

The OpenAPI surface (`docs/api/v0.openapi.yaml`) and its companion `docs/api/v0.md` remain the authoritative parameter reference.

## Mid-stage scope amendments (`fishhawk_list_scope_amendments`, `fishhawk_decide_scope_amendment`)

E22.X / [#961](https://github.com/kuhlman-labs/fishhawk/issues/961) adds the **mid-stage** complement to approval-time `add_scope_files`: while the implement stage is RUNNING, the agent can request that specific paths be folded into the effective `scope.files` instead of silently dropping a coupled edit (the runner omits undeclared edits from the commit; an undeclared created file fails category-B, #818/#825).

**Agent protocol (poll-based, no push channel in v0).** The implement prompt instructs the agent to `POST /v0/runs/{run_id}/scope-amendments` with its run-bound `FISHHAWK_API_TOKEN` (`{paths: [{path, operation: modify|create}], reason}`), then poll the GET (same bearer, `mcp:read`) every 15–30s until the request leaves `pending`, working on in-scope files meanwhile and giving up after ~5 minutes. Cap: **2 requests per stage**, counted server-side on rows — a denied request still consumes budget. The agent must never edit/create a requested file before the approval lands.

**Operator loop:**

1. Await the request: `fishhawk_await_audit` anchored on category `scope_amendment_requested` (#977). The entry payload carries `{amendment_id, paths, reason, remaining_budget}`.
2. Inspect: `fishhawk_list_scope_amendments {run_id}` — paths, per-path operation, the agent's reason, status.
3. Decide: `fishhawk_decide_scope_amendment {run_id, amendment_id, decision: approve|deny, reason}`. Decide promptly — the agent's poll is bounded.

**Scope-cap headroom ([#983](https://github.com/kuhlman-labs/fishhawk/issues/983)).** When the implement stage has a `max_files_changed` cap, pending items in the list (and the request/decision responses) carry `effective_scope_files_after_approval` + `max_files_changed`, and both tools print an explicit `WARNING` line when approving would put the effective scope over the cap. Warn-only by design: an over-cap approve still succeeds — mid-stage amendments are often forced, and the post-implement gate plus the now-informed operator own the verdict. Fields are absent on older backends or when no cap is configured.

**Auth.** The decision is operator-only (`write:stages`); the backend rejects run-bound agent tokens outright (`self_decision`), so the requesting agent can never approve its own request. The agent-side POST requires the implement-stage token's `write:scope-amendments` scope (granted unconditionally at token issue for implement stages); the GET admits the run-bound token (`mcp:read`, own run only — cross-run is 403) or any operator bearer/session.

**Activation.** Approved paths fold into the effective scope at BOTH ends: the backend prompt fetch (`source "scope-amendment"`, so a stage restart or fix-up carries the amended scope) and the runner's pre-commit refresh, which re-reads the GET with the same run-bound token and folds approved paths BEFORE the committed-tree verify gates and `StageScoped` — preserving the #960 invariant that the gates verify the same folded tree that is pushed. Anything NOT requested still fails loud. Both `scope_amendment_requested` and `scope_amendment_decided` are internal audit kinds, not issue-comment surfaces.

## Scope-completeness park (`fishhawk_decide_scope_completeness`)

E22.X / [#1231](https://github.com/kuhlman-labs/fishhawk/issues/1231) adds a **zero-re-run** recovery for the case the [#1229](https://github.com/kuhlman-labs/fishhawk/issues/1229) one-re-run exempt lever otherwise served: an implement stage whose **only** committed-tree gate failure is the [#1151](https://github.com/kuhlman-labs/fishhawk/issues/1151) scope-completeness "missing declared scope file(s)" check, while the committed tree otherwise passed verify (created-out-of-scope, binding-assertion, compile/test, and verified-tree gates all green).

**Park, not category-B.** Instead of fail-and-restore, the runner pushes the **gate-verified commit** to the run branch (no PR opened — ADR-035 sole-writer preserved: the run writes its own branch) and PARKS the implement stage in a new `awaiting_scope_decision` state, carrying the held commit SHA, run branch, verified tree SHA, and the missing declared paths. The park leaves the gate waiting for an in-band operator decision over the [#1232](https://github.com/kuhlman-labs/fishhawk/issues/1232)/[#1235](https://github.com/kuhlman-labs/fishhawk/issues/1235) non-blocking dispatch substrate. Any compound failure (missing **plus** another gate) keeps today's category-B unchanged.

**Operator loop:**

1. Observe the park: `fishhawk_get_run_status` surfaces the `implement_awaiting_scope_decision` next action; `fishhawk_list_audit` on category `scope_completeness_parked` carries the missing paths + held SHA.
2. Decide: `fishhawk_decide_scope_completeness {run_id, decision: exempt|fail, reason}`.
   - `exempt` — the backend opens the PR from the **exact held commit** with **NO agent re-invocation** (the already-committed tree is accepted as-is; the implement-review gate proceeds). Appends `scope_completeness_exempted`.
   - `fail` — the stage falls through to today's category-B fail-and-restore. Appends `scope_completeness_failed`.

**Auth.** Operator-only (`write:stages`); the backend rejects run-bound agent tokens (`run_token_forbidden`), so the agent whose stage parked can never decide its own park (mirrors `fishhawk_decide_scope_amendment`). `reason` is required and non-empty; an invalid `decision` (anything but `exempt`/`fail`) or empty `reason` is caught before the HTTP hop. The endpoint returns 409 (`scope_completeness_not_parked`) when the stage is not parked in `awaiting_scope_decision`. It wraps `POST /v0/runs/{run_id}/scope-completeness/decision`.

`scope_completeness_parked`, `scope_completeness_exempted`, and `scope_completeness_failed` are internal audit kinds, not issue-comment surfaces.

## Category-B recovery (`fishhawk_resume_run`)

E22.X / [#978](https://github.com/kuhlman-labs/fishhawk/issues/978) adds operator-initiated recovery for a run whose implement stage failed **category-B** (scope/constraint violation) after its plan was approved — the gap between `fishhawk_retry_stage` (refuses B) and `fishhawk_start_run` (replans from scratch). The tool wraps `POST /v0/runs/{run_id}/recover` and mints a **new plan-stage-less child run** that re-executes against the parent's approved plan.

Inputs: `parent_run_id` (the failed run), optional `add_scope_files` (`[{path, operation: modify|create}]`, operation defaults to `modify`), optional `reason`, `budget_override`, and `idempotency_key` (same replay semantics as `fishhawk_start_run`).

- **Eligibility**: parent's plan stage `succeeded` AND implement stage `failed` category-B; anything else returns `recovery_not_eligible` naming which leg failed. Parents without a cached workflow spec return `recovery_unsupported` — start a fresh run.
- **Plan reuse**: the child carries `parent_run_id`; `fishhawk_get_plan` and the prompt builder resolve the parent's plan via the existing parent walk. The parent's binding approval conditions and approval-time `add_scope_files` are inherited too.
- **Scope amendments**: operator-named `add_scope_files` land as a **pre-approved** #961 amendment row on the child's implement stage — visible via `fishhawk_list_scope_amendments`, folded by the prompt fetch and the runner's pre-commit refresh; `operation: create` entries flow into the #818/#825 net-new-file gates.
- **Budget**: `retry_attempt` is carried UNCHANGED — recovery never consumes the `on_ci_failure` auto-retry cap. Provenance lands as a `plan_reused_from` audit entry on the child (internal audit kind, not an issue-comment surface).

Drive the child like any local run: `fishhawk_run_stage` executes the implement stage directly — no plan stage exists, no plan approval is needed.

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
| `sections` / `title_vars` | no | Per-skeleton-section content and extra title placeholders (e.g. `epic`, `n`). An unresolved title placeholder fails the filing. |
| `labels` / `complexity` / `status` | no | Merged on / overriding the type's defaults; `complexity` must be a declared level. |
| `relations` | no | `{parent_epic, supersedes[], companion_to[], evidence_runs[]}` — resolved into the provider's link operations. |
| `existing_numbers` | no | Numbers already in use for a numbered type (e.g. `adr`), so the next sequential number can be allocated. |
| `run_id` | falls back to env | Optional in-flight run UUID; defaults to `FISHHAWK_RUN_ID`. When set and non-terminal a best-effort `work_item_filed` audit entry is appended to it. |

Audit-on-active-run is **best-effort**: filing still succeeds with no run in flight, and the response's `audited` flag reports whether an entry was written. Returns the created item — `type`, `title`, `number`, `url`, `provider`, the resolved `applied_labels` / `complexity` / `status` / `board_column`, and `audited`.

**Auth:** a write tool — the backend requires an authenticated caller (anonymous requests are rejected). Error surfaces propagated as tool errors: `validation_failed` (400 — repo not `owner/name`, missing `type`/`summary`, unknown fields; the empty `type`/`summary`/`repo` cases are also caught locally before the HTTP hop), `authentication_required` (401), `work_item_invalid` (422 — the request violates the type's conventions), `provider_unimplemented` (501 — the configured provider id is not registered, e.g. the interface-only `jira`; details name it), `work_item_filing_failed` (502 — the provider rejected the filing). The CLI mirror is `fishhawk file-issue`.

## Product feedback (`fishhawk_report_product_issue`, [#1006](https://github.com/kuhlman-labs/fishhawk/issues/1006))

`fishhawk_report_product_issue` files an upstream **Fishhawk product** bug or feature request — when you hit friction with Fishhawk itself, not the repo you're working in — carrying an auto-collected diagnostic bundle. It wraps `POST /v0/runs/{run_id}/product-reports`. The destination is the **fixed** upstream product repo; it is not caller-controlled. The backend collects the run's product-facts bundle, fingerprints the failure `(error code, failing surface, version family)`, searches the product repo for an open report already carrying that fingerprint marker, and either appends an occurrence comment (dedup hit — nothing new is created) or files a new fingerprint-marked report (dedup miss). A source-side `product_report_filed` audit entry records what left the boundary.

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

## Runner integration

E19.8 / future wires `fishhawk-mcp` into the runner's container image. Until then the MCP surface is interactive-Claude-Code-only.

## See also

- `docs/api/v0.openapi.yaml` — every tool wraps a `/v0/*` endpoint from this surface.
- `cli/internal/httpclient` — typed wrappers the MCP server reuses (or a thin local copy if cross-module reuse becomes awkward — final call inside individual tool PRs).
- [ADR-021](https://github.com/kuhlman-labs/fishhawk/issues/322) — the model-decision ADR.
