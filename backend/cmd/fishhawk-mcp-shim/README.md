# fishhawk-mcp-shim

Stdio **session-survival supervisor** for the [`fishhawk-mcp`](../fishhawk-mcp/) MCP server ([ADR-060 / #1921](https://github.com/kuhlman-labs/fishhawk/issues/1921)).

## Why

Claude Code owns the MCP server subprocess and does **not** reconnect a restarted stdio server. A `scripts/dev reload` that rebuilds `fishhawk-mcp` leaves the live session pointed at the old binary until the operator runs `/mcp` by hand (see [ADR-021 gotcha](../fishhawk-mcp/README.md) and the dev-loop reconnect banner). The shim closes that gap: it sits between the client and `fishhawk-mcp`, watches the child binary for a rebuild, and hot-swaps it under the live session so the tool set refreshes with no manual reconnect.

## Scope: the STDIO path only (ADR-076 slice 2 / #2390)

The shim supervises the **stdio** `fishhawk-mcp` child. It has nothing to do
with fishhawkd's `/mcp` route, which serves the same tool registry over
streamable HTTP on fishhawkd's own listener.

A client connected to `/mcp` needs no reconnect-after-rebuild dance: a
`scripts/dev reload` restarts `fishhawkd` itself, and the client reconnects to
the same URL, so there is no long-lived subprocess holding stale code the way
the harness-owned stdio child does. (Claude Code will not re-establish a dropped
HTTP MCP connection on its own either — that is a client-side gap, not a stale
binary, and is E66's concern rather than the shim's.)

The `childTransport` seam mentioned below is what would let a future phase point
the shim at that HTTP upstream instead of a stdio child; nothing in #2390
changes the shim.

## How it works

The shim spawns `fishhawk-mcp` as a child over pipes and passes newline-delimited JSON-RPC frames **byte-verbatim** in both directions. It parses only (a) the client's `initialize` request (recorded with the child's response) and (b) message ids, for in-flight request tracking.

- **Content poller** — a sha-256 poll (never mtime; a reload rebuild can be a byte-identical no-op) over the child binary path, with a settle debounce so a half-written `go build -o` output never triggers a swap.
- **Swap** — on a confirmed content change the shim quiesces (waits for zero in-flight client requests up to `--quiesce-timeout`; on timeout it defers the swap to the next idle moment — it never kills a child mid-request), SIGTERMs the old child (SIGKILL-escalated to its process group after a grace period), spawns the new binary, replays the recorded `initialize` with a synthetic collision-proof id (swallowing the response), sends `notifications/initialized`, and synthesizes `notifications/tools/list_changed` upstream so the client re-reads the tool set.
- **Crash recovery** — a crashed child is respawned with capped exponential backoff through the same replay path; any requests orphaned by the crash get synthesized JSON-RPC error responses so the client is never stranded.
- **Swap gate** — a swap needs a re-establishable session. The gate allows it when the handshake completed; when the handshake was never MATCHED but an `initialize` **is** recorded and the child has served at least one result-bearing response, it **presumes** the handshake and allows the swap (recording `handshake_presumed`); and it refuses — saying which — when no `initialize` was ever recorded, or when there is no served-result evidence yet.

The child connection sits behind a small `childTransport` seam so a later phase can substitute a streamable-HTTP upstream — the [#655](https://github.com/kuhlman-labs/fishhawk/issues/655) gateway phase-0 constraint.

## Diagnosing a shim that stopped swapping

The [#2831](https://github.com/kuhlman-labs/fishhawk/issues/2831) failure shape: the shim is alive, the child is alive, `bin/fishhawk-mcp` was rebuilt hours ago — and the session is still on the old binary, with nothing said about it. The tell is a tool call returning a `git_sha` that does not match `git rev-parse --short HEAD`.

```sh
bin/fishhawk-mcp-shim --status                      # every live shim, human-readable
bin/fishhawk-mcp-shim --status --stale-only         # machine mode: stale entries only, silent when clean
```

`--status` spawns no child and reads no stdin — it only reads the state dir, so running it against a live session is safe. It also prunes the leftover snapshots of shims that are no longer running (see below).

### The state file

Every running shim writes `<state-dir>/<shim-pid>.json` on each watcher tick and each swap-state transition. The write is atomic (temp file in the same dir, then `rename`), so a reader sees a whole snapshot or the previous one — never a partial write. The document is flat, schema `fishhawk-mcp-shim/state/v1`:

| Field | Meaning |
|---|---|
| `shim_pid`, `shim_git_sha` | which shim wrote this, built from which commit |
| `child_pid`, `child_path` | the running child — the same pid a `ps` table shows |
| `child_launch_hash` | sha-256 of the bytes the child was launched from |
| `pending_swap_hash`, `pending_since` | the confirmed content change waiting to be applied, and since when |
| `handshake_done`, `handshake_presumed` | whether the session can be replayed, and whether that was inferred rather than observed |
| `served_results` | result-bearing child responses — the evidence the presumption keys on |
| `in_flight`, `oldest_in_flight_id`, `oldest_in_flight_since` | what is holding a swap off, and for how long |
| `quiesce_expired` | the active quiesce timed out; the swap is waiting passively for idle |
| `last_swap_at`, `last_swap_outcome`, `updated_at` | the last attempt, its verdict, and snapshot freshness |

`last_swap_outcome` is one of `swapped`, `crash_respawn`, `deferred_no_initialize_recorded`, `deferred_handshake_not_observed`, `deferred_in_flight`. Each deferral is ALSO logged to stderr — on the transition, then at most once every 5 minutes while the same denial persists.

A snapshot is classified against the binary currently on disk: **current** (hashes equal), **pending** (hashes differ but the swap is younger than `--stale-grace`, or its age/path cannot be read — the fail-safe direction, so a just-rebuilt binary never draws a false alarm), or **stale** (hashes differ AND the swap has been pending at least the grace period).

The snapshot carries no request payloads, no environment and no credentials — only pids, paths, content hashes and JSON-RPC ids.

### Leftover state files are the steady state

A shim removes its own snapshot when it shuts down cleanly. A SIGKILLed or harness-terminated shim never does, so its file persists with a dead pid. The reader skips any snapshot whose `shim_pid` is not a live process (a `signal 0` probe — which cannot distinguish a *reused* pid, at worst one stale advisory line and never anything destructive), and `--status` **removes** those leftovers as it reports them. `--stale-only` never reports skips at all: it is consumed by `scripts/dev`, where leftover-file chatter on a machine with nothing actually stale would be pure noise.


## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--child` | sibling `fishhawk-mcp` next to the shim executable | path to the `fishhawk-mcp` child binary |
| `--poll-interval` | `2s` | how often to poll the child binary for a content change |
| `--quiesce-timeout` | `30s` | how long to wait for zero in-flight requests before deferring a swap |
| `--status` | off | report every live shim's swap state and exit — spawns no child, reads no stdin, writes no state file |
| `--stale-only` | off | with `--status`: print ONLY stale shims, and **nothing at all** (stdout or stderr) when none are stale |
| `--state-dir` | `${TMPDIR:-/tmp}/fishhawk-mcp-shim` | where per-shim snapshots live (env: `FISHHAWK_MCP_SHIM_STATE_DIR`) |
| `--stale-grace` | `60s` | how long a pending swap must have been outstanding before it counts as stale |

## Registration

**One-time operator re-registration (required).** To move a live session off the manual-reconnect treadmill you must re-point the harness's `fishhawk` MCP server entry at the shim **instead of** `fishhawk-mcp` — a one-time step per host. If a plain `fishhawk-mcp` entry already exists, remove it and re-add pointed at the shim binary:

```sh
fishhawk token login --backend-url http://localhost:8080   # mint a credential once
claude mcp remove fishhawk    # drop the existing plain fishhawk-mcp entry, if any
claude mcp add fishhawk -- /path/to/bin/fishhawk-mcp-shim
```

The registration mirrors the sibling [`fishhawk-mcp`](../fishhawk-mcp/README.md#install-operators) form — the binary path given positionally after the `--` separator — just pointed at the shim binary. With the standard `bin/` layout no `--child` flag is needed. Run `/mcp` once after re-registering so the session picks up the shim; from then on child rebuilds hot-swap with no further `/mcp`.

The shim passes env through unchanged: it adds no auth of its own and forwards `FISHHAWK_API_TOKEN` / `FISHHAWK_BACKEND_URL` straight to the child. With a **stored credential** (`fishhawk token login`) there is no `--env FISHHAWK_API_TOKEN` to pass — the child resolves the bearer from the shared [`credstore`](../../../credstore/README.md) at startup. If you would rather keep the token in the environment, wire it with `-e FISHHAWK_API_TOKEN=$FISHHAWK_API_TOKEN` on the `claude mcp add` line above (and `FISHHAWK_BACKEND_URL` too when it is not the default `http://localhost:8080`); an explicit env token wins over the store.

### `scripts/dev` integration

The shim is wired into the dev loop ([#1922](https://github.com/kuhlman-labs/fishhawk/issues/1922)) as the fifth rebuild-matrix binary with its **own** trigger glob (`backend/cmd/fishhawk-mcp-shim/` only — deliberately not the `backend/internal/plan|spec` shared-lib case, so it rebuilds rarely). `scripts/dev reload` (`--all`) rebuilds `bin/fishhawk-mcp-shim` alongside the others; a running shim keeps its open inode, so the on-disk rebuild is inert until a manual `/mcp` (see the accepted residual below).

`scripts/dev up`/`reload` select exactly one closing banner:

- **schema-major bump** — a `.fishhawk/workflows.yaml` `version:` MAJOR change; unconditional (#1422).
- **shim rebuilt** — `fishhawk-mcp-shim` source changed: a distinct banner telling you to run `/mcp` once (the shim swaps its child, not itself).
- **reconnect** — `fishhawk-mcp` changed and the shim is **not** registered: the legacy manual-reconnect banner.
- **auto-swap** — `fishhawk-mcp` changed and the shim **is** registered: a one-line note stating that the shim **should** hot-swap the rebuilt child within about two poll intervals, and that a version-returning tool call reflecting the new GitSHA is what confirms or refutes it. It deliberately does not assert the swap as an accomplished fact: `scripts/dev` cannot observe the harness-owned shim process, and a false "no `/mcp` needed" is exactly the silent-stale trap #2831 describes.

`scripts/dev up` (inherited by `reload`/`post-merge`) also runs a **non-fatal stale-shim advisory**, mirroring the stale-worktree one: it probes `bin/fishhawk-mcp-shim --status --stale-only` and, on non-empty output, prints a warning naming each stale shim and pointing at `/mcp`. Every degrade is silence — no shim binary, a non-executable path, a non-zero probe exit, empty output, and probe stderr (which is discarded outright, so a future diagnostic writing to stderr cannot turn the advisory into noise).

**State-dir coupling.** The advisory forwards `FISHHAWK_MCP_SHIM_STATE_DIR` to the probe as `--state-dir` only when that variable is set in `scripts/dev`'s OWN environment. A shim registered with a custom state dir that `scripts/dev` cannot see (for example `claude mcp add ... -e FISHHAWK_MCP_SHIM_STATE_DIR=...`) writes its snapshots elsewhere, the probe reads the default dir, finds nothing, and stays silent — a false negative. Treat the override as test-only unless you also export it where `scripts/dev` runs.

Registration is detected via the `FISHHAWK_MCP_SHIM_REGISTERED` env override (sourced from `.env`; `1`/`true` or `0`/`false`) winning over a best-effort `claude mcp get fishhawk` probe; an absent or errored `claude` CLI degrades to not-registered, which keeps the manual banner (the fail-safe direction).

## Layout

A separate binary built from the **backend module**, sibling to `fishhawk-mcp` per ADR-021.

- `watcher.go` — the sha-256 content poller (never mtime, settle-debounced).
- `supervisor.go` — quiesce, swap gate, handshake replay with a synthetic id, crash respawn with
  capped backoff, orphaned-request error synthesis, swap-state publication.
- `state.go` — the swap-state snapshot, its atomic file transport, the liveness/staleness
  classifier and the `--status` renderer (#2831).
- `transport.go` (+ `transport_unix.go` / `transport_other.go`) — the `childTransport` seam a
  streamable-HTTP upstream can later replace stdio through (#655 phase 0).

## Accepted residuals

- **A same-turn schema-new tool fails once.** Claude Code honours `notifications/tools/list_changed` across turns but not mid-turn ([anthropics/claude-code#31893](https://github.com/anthropics/claude-code/issues/31893), verified in the ADR-060 spike set). A tool whose schema is brand-new in the just-swapped binary fails once within the turn it was announced, then works.
- **A long-lived in-flight request defers a swap indefinitely — by design.** Legitimate stdio calls in this repo block for hours (7200s `await_stage` heartbeats), so force-orphaning them to unblock a swap would be a worse defect than the one being fixed. The deferral is now VISIBLE instead of silent: the snapshot carries `deferred_in_flight` with the oldest request's id and age, and stderr says so on a rate-limited line. Ending the call, or `/mcp`, is the remedy.
- **A pid-reuse false positive is possible.** Liveness is a `signal 0` probe, which cannot tell a reused pid from the original. The worst case is one spurious advisory line; nothing destructive keys off it.
- **A state-file write failure degrades the diagnostic, not the proxy.** A read-only or full `TMPDIR` logs once and the shim keeps serving frames.
- **Shim-binary changes still need one manual `/mcp`.** The shim swaps the *child*; a rebuild of the shim itself is still owned by the harness, so a change to `fishhawk-mcp-shim` needs a one-time `/mcp` reconnect like any MCP server change.
