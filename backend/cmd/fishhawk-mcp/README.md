# fishhawk-mcp

Model Context Protocol server that exposes Fishhawk run / plan / audit state to Claude Code (and any other MCP-compatible client) per [ADR-021 / #322](https://github.com/kuhlman-labs/fishhawk/issues/322).

Two audiences share one surface:

- The **in-runner Claude Code agent** reads its own run's state mid-execution — what's the active plan, what audit entries fired for the current retry, what constraints apply. Closes the agent-is-blind-to-Fishhawk-state gap that motivated ADR-019.
- The **interactive Claude Code session** — an engineer asking "what's the status of my current run" — gets the answer through natural language without a CLI alt-tab.

The v0 surface began read-only; action verbs (approve, reject, retry, cancel, start, run_stage, the implement-review fix-up below, and the run-branch reset below) have since landed as scoped write tools so the loop can be driven end-to-end from the agent session. Write tools require an operator-side token with the matching `write:*` scope; a run-bound runner token is restricted to its own run.

## Status

E19.2 / #342 shipped scaffolding + handshake. E19.3–E19.6 landed the v0 tool surface (all read-only per ADR-021):

- `fishhawk_get_active_run` (E19.3 / #343) — the "which run" resolver: use it when you hold a `pr_number`, `trigger_ref`, or `FISHHAWK_RUN_ID` env but need the run UUID the other tools take.
- `fishhawk_get_plan` (E19.4 / #344) — read the approved standard_v1 plan artifact after the plan stage and before approve/reject; walks `parent_run_id` up to 8 levels for CI-retry chains.
- `fishhawk_get_run_status` (E19.5 / #345) — the agent's "where are we" query: bundles Run + ordered stages + recent audit (time-descending) into one call. Also carries `plan_review_status` + `implement_review_status` (`none`/`pending`/`complete`/`skipped`/`failed`). **Re-polling this tool is the authoritative way to reach a terminal review status (#879)**: on `pending` each status carries a server-suggested `poll_interval_seconds` (15s) — re-call on that cadence until the status goes terminal.
- `fishhawk_await_review` (#600) — OPTIONAL convenience block over that poll: blocks until a stage's review reaches a terminal state. Default timeout **360s** (recalibrated from 120s to exceed the measured 3.5–4.5min review latency and the 300s reviewer budget, #878), cap 600s. Never strands — it also resolves when the run itself goes terminal (ADR-036 #874). Idempotent/resumable: a timeout returns `pending` + the `poll_interval_seconds` hint; re-call to resume, or switch to `fishhawk_get_run_status` polling (the primary path).
- `fishhawk_list_audit` (E19.6 / #346) — use when you need the filtered or paginated audit trail (category, stage_id) rather than the recent slice — e.g. to read `implement_reviewed` concern indices for `fishhawk_fixup_stage`. Mirrors the CLI's `fishhawk audit list`.

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

## Implement-review fix-up (`fishhawk_fixup_stage`)

`fishhawk_fixup_stage` (E22.X / [#762](https://github.com/kuhlman-labs/fishhawk/issues/762)) routes one or more **advisory implement-review concerns** ([ADR-027](https://github.com/kuhlman-labs/fishhawk/issues/703) `approve_with_concerns`) back to the implement agent for a single fix-up pass, instead of the operator hand-editing the PR branch. It wraps `POST /v0/stages/{stage_id}/fixup`.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `stage_id` | **yes** | The implement stage parked at the review gate. |
| `concerns` | **yes** | Indices of the recorded implement-review concerns to route back (at least one). The indices address the concern set on the stage's `implement_reviewed` audit entry — inspect it via `fishhawk_list_audit`. |
| `reason` | no | Operator note, recorded on the `stage_fixup_triggered` audit entry. |
| `allow_create` | no | Repo-relative paths this fix-up will **create** ([#823](https://github.com/kuhlman-labs/fishhawk/issues/823)). See below. |

**Declaring net-new files (`allow_create`)** — a concern that requires a *new* file needs `allow_create`. Each declared path is folded into the implement stage's **effective `scope.files` for THIS fix-up pass only** (never the persisted plan scope), reusing the same [#824](https://github.com/kuhlman-labs/fishhawk/issues/824) `foldScopePaths` machinery `add_scope_files` uses. Because the runner's created-out-of-scope gate ([#818](https://github.com/kuhlman-labs/fishhawk/issues/818)) keys off that effective union, folding the path in makes the runner stage the new file so the gate stops tripping for it. The pass is bounded and operator-authorized: a fix-up only happens when the operator calls this verb, and `allow_create` widens the legitimate set only by the paths the operator names. **Preserved contract:** any created file **NOT** declared here still fails category-B per #818 — declaring paths does not reopen the silent-strip hole. Entries must be repo-relative; an absolute path or one containing `..` is rejected (`validation_failed`, 400, `field: allow_create`). The OpenAPI/`v0.md` surface remains the authoritative parameter reference.

What a fix-up does — and how it differs from `fishhawk_retry_stage`:

- The selected concerns are delivered to the agent as **binding instructions** (the [#558](https://github.com/kuhlman-labs/fishhawk/issues/558) condition-delivery framing: MANDATORY, win on conflict).
- The agent commits onto the **same PR branch** and the existing PR is **updated** — a fix-up does **not** regenerate a fresh diff or open a new PR. (`retry` re-opens a *failed* stage and regenerates; fix-up re-opens a *healthy* review gate.)
- The implement review re-runs on the result.
- On success the stage flips `awaiting_approval → pending` (the orchestrator advances it to `dispatched`, re-firing `workflow_dispatch`); the tool returns the re-opened stage.

**Operator-gated and bounded — this is never an unbounded auto-loop:**

- The bound defaults to **one pass per stage**, enforced server-side by counting prior `stage_fixup_triggered` audit entries. A second attempt once the bound is spent returns a `fixup_budget_exhausted` tool error (its details carry `max_passes` + `used`). The remaining budget is `max − fix-ups already triggered`, surfaced on the audit entry's `remaining_budget` field (read it via `fishhawk_list_audit`); the success response itself carries only the re-opened stage.
- **Operator owns the trigger and the merge.** A fix-up only ever happens when an operator calls this verb; the agent cannot self-trigger one, and the operator still approves the final merge.
- **Auth:** a write tool requiring `write:stages` (or the dedicated `write:fixups`) scope. A run-bound token may fix up only stages **within its own run** — a cross-run target returns `cross_run_fixup` (403).

Error surfaces propagated as tool errors: `validation_failed` (400, empty `concerns` / out-of-range index — empty selection is also caught locally before the HTTP hop), `cross_run_fixup` (403), `stage_not_found` (404), `fixup_not_applicable` (422, no recorded `approve_with_concerns` verdict to route back), `fixup_budget_exhausted` (422).

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

## Scope amendment at approval (`fishhawk_approve_plan` → `add_scope_files`)

`fishhawk_approve_plan` (E22.4 / [#393](https://github.com/kuhlman-labs/fishhawk/issues/393)) takes an optional `add_scope_files` array ([#824](https://github.com/kuhlman-labs/fishhawk/issues/824)) — the **structured, authoritative** way to add files to the implement stage's `scope.files` at approval time. On approve the named paths are recorded on the approval audit payload and folded into the implement stage's effective scope by the prompt builder, so a reviewer-authorized edit ships as a declared path rather than surfacing as benign `scope_drift`.

Prefer it over naming paths in the free-text `reason`. The `reason` fold ([#730](https://github.com/kuhlman-labs/fishhawk/issues/730)) is a best-effort regex scrape kept only as a fallback; it silently misses:

- **directories** — pass a trailing slash (e.g. `pkg/testdata/corpus/`); every created file under that prefix stages.
- **extensionless and repo-root files** — e.g. `go.work`, `Makefile`.
- **described-but-not-spelled paths** — anything the prose names in words rather than as a literal path token.

`reason` and `add_scope_files` compose: the structured paths fold first (authoritative), then the prose fold runs as a fallback, both deduping by path. Both no-op when the plan declares an empty scope, preserving the runner's `git add -A` fallback. `add_scope_files` does **not** weaken the policy gate — a folded path that matches `forbidden_paths` still fails category-B against the produced diff.

The OpenAPI surface (`docs/api/v0.openapi.yaml`) and its companion `docs/api/v0.md` remain the authoritative parameter reference.

## Runner integration

E19.8 / future wires `fishhawk-mcp` into the runner's container image. Until then the MCP surface is interactive-Claude-Code-only.

## See also

- `docs/api/v0.openapi.yaml` — every tool wraps a `/v0/*` endpoint from this surface.
- `cli/internal/httpclient` — typed wrappers the MCP server reuses (or a thin local copy if cross-module reuse becomes awkward — final call inside individual tool PRs).
- [ADR-021](https://github.com/kuhlman-labs/fishhawk/issues/322) — the model-decision ADR.
