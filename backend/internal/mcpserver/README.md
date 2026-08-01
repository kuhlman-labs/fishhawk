# `mcpserver` — Fishhawk MCP tool surface

This package holds the Fishhawk MCP server: the tool registrations
(`tools.go`), their handlers, and the operator-facing drive/gate machinery
that `backend/cmd/fishhawk-mcp` wires into an MCP transport. The command
binary is a thin main; the behavior lives here so it can be unit-tested and
reused. `tools.go` is the human-facing tool listing's source of truth (its
count is pinned by `tools_test.go`); keep this README in lockstep when the
tool set changes.

## `fishhawk_drive_run`

`fishhawk_drive_run` executes **every mechanical operator step between human
gates** on a `runner_kind:local` run under ADR-040 delegation, and stops at
the first genuine decision. It is the local sibling of the GHA campaign
auto-driver: a bounded, resumable loop that reuses this host's session,
token, and detached-spawn machinery (ADR-024 — the local runner can only be
spawned by this MCP host).

### `dispatched` staleness — spawned but never reached its prompt fetch

A `dispatched` stage this invocation did not spawn has a runner in flight —
from a prior driver invocation OR a manual `fishhawk_dispatch_stage` — so the
driver **never re-spawns** it. It is classified purely on the
**runner-liveness threshold**, anchored on the stage's own
`max(updated_at, started_at)`:

- **Anchor past the liveness threshold** (default 10 min; a live local runner
  flips `dispatched`→`running` within seconds of its prompt fetch,
  [#1924](https://github.com/kuhlman-labs/fishhawk/issues/1924)) — the driver
  **probes host liveness itself**
  ([#1955](https://github.com/kuhlman-labs/fishhawk/issues/1955)): it execs
  `pgrep -f` scoped to the stage's `--stage-id <uuid>` argv token (the MCP
  host is the host that spawned the runner, ADR-024, so the probe is precise)
  and classifies the result three ways. **DEAD** (pgrep exit 1, no matching
  process) — the spawned runner died at or just after spawn — is
  **auto-recovered in place**: the driver falls through its
  `record-act → host-dispatch marker → spawn` path with a stale-re-dispatch
  note (`stale re-dispatch: liveness probe found no runner process`) and
  drives on, no operator action. **LIVE** (exit 0: a process carrying the
  stage id exists yet never flipped `running`) stops `dispatched_stale` and
  **never spawns** — a second runner into the same lineage lock stays
  impossible; the warning names the live process + the dispatch `log_path`.
  **UNKNOWN** (pgrep absent / exit ≥ 2 / exec error) degrades to the manual
  verify-first stop `dispatched_stale`, with `next_actions` pointing at a
  manual `fishhawk_dispatch_stage` after confirming no runner is live
  (`pgrep -f fishhawk-runner` + the dispatch's `log_path`). The
  `dispatched_stale` stop therefore survives only for the LIVE-or-unprobeable
  ambiguous cases.
- **Anchor fresh** (or a zero-value anchor with no timestamped evidence) —
  poll. A hand re-dispatch spawns a fresh runner whose prompt fetch flips
  `dispatched`→`running`, so a subsequent `fishhawk_drive_run` reads the stage
  as in-flight and **polls to convergence** instead of re-reporting
  `dispatched_stale`.
