# backend/internal/opsmonitor

Opt-in host and process health monitor for fishhawkd (E85.1 / #4142). A `Ticker` sweeps six probes on an interval; each FAILING `(condition, subject)` produces ONE run-less global-chain `ops_probe_failed` audit row and ONE operator issue comment per quiet window. Off by default; wired by `fishhawkd serve --enable-ops-monitor` and the dry-run `fishhawkd ops-probe` subcommand (`backend/cmd/fishhawkd/ops_monitor.go`, flags in `backend/cmd/fishhawkd/README.md`).

## Result statuses

Every probe result is exactly one of:

| Status | Meaning |
|---|---|
| `ok` | the probe observed the condition and it is within threshold |
| `failing` | the probe observed the condition and it is out of threshold; carries `condition`, `observed`, `threshold` and a non-empty `remedy` |
| `unknown` | the probe could not observe (error, timeout, panic, missing input); carries a `reason`; never emitted as a row or comment, WARN-logged once per probe per quiet window |
| `not_applicable` | a host-local probe under a non-local deployment; its `Check` is never called |

## Fail-safety (`Sweep`)

`Sweep(ctx, probes, deployment, probeTimeout)` is the single chokepoint. A host-local probe under a non-local deployment yields one `not_applicable` result without running. Every other probe runs on its own goroutine under `probeTimeout` (default 30s) and `Sweep` selects on the result vs the deadline, so a probe that ignores its context cannot stall a tick. Each of these reads `unknown`, never `ok`: a deadline (`probe timed out after <d>`), including a result that arrives after the deadline; a panic (`probe panicked`; the panic value is not logged); zero results; an empty or unrecognized status; a nil probe or one whose `Name`/`HostLocal` panics. A failing result with no remedy gets the per-condition table remedy, or a generic one for an unlisted condition, so a failing row never has an empty remedy.

## Deployment

`ResolveDeployment(raw, getenv)`: `""`/`auto` → `kubernetes` when `KUBERNETES_SERVICE_HOST` is set (the kubelet injects it into every pod), else `local`; `local|kubernetes|remote` are honored verbatim; anything else is an error. `auto` cannot detect a non-k8s remote host, so pass `remote` explicitly there. The five host-local probes (disk, gocache, load, docker, runners) are `not_applicable` unless the deployment is `local`; the postgres probe runs everywhere.

## Probes

| Probe | Host-local | ok | failing (condition) | unknown |
|---|---|---|---|---|
| `disk` | yes | `statfs` `f_bavail * f_bsize` ≥ min free | below min → `disk_free_low` | empty path, statfs error, non-darwin/linux build |
| `gocache` | yes | sampled estimate ≤ max | estimate > max → `gocache_over_threshold` | dir unresolvable (`go env GOCACHE` error, empty, `off`), root missing, README marker absent (`not a Go build cache`), walk cancelled |
| `load` | yes | 1-minute load ≤ factor × cores | load > factor × cores → `host_load_high` | no load source readable, cores ≤ 0 |
| `docker` | yes | `docker info --format {{.ServerVersion}}` exits 0 | EIO text → `storage_eio` (subject `docker`); any other non-zero exit or the 20s bound → `docker_unreachable` | `docker` not on PATH |
| `postgres` | no | `Ping` nil | EIO text → `storage_eio` (subject `postgres`); any other error → `postgres_unreachable` | no database configured (nil `Ping`) |
| `runners` | yes | every attributed runner's stage/run is live (or unknown to this tracker) | a pid/run/stage seen terminal on `ConfirmSweeps` consecutive Checks → `orphaned_runner` (subject `pid/run/stage`) | no database, ps scan error, a tracker lookup error, or any UNATTRIBUTED runner, when nothing is failing |

- **GOCACHE sizing** is `scripts/dev`'s sampled estimate: regular-file bytes in the 16 shards `00,10,…,f0` × 16 (cmd/go files entries under the hash's first byte). The directory resolves lazily at `Check` time from `go env GOCACHE` when no `Dir` is configured — never at flag registration. A directory without the cmd/go README marker (`cached build artifacts from the Go build system`) reads `unknown` instead of a misleading 0.
- **Load** ports `runner/internal/hostload`'s source order (`/proc/loadavg`, `sysctl -n vm.loadavg` `{ a b c }`, `uptime` with `load average(s):`) because the runner is a separate module; default factor 4.0 = the runner's `hostload.DefaultFactor`.
- **EIO indicator**: a case-insensitive `input/output error` or a word-bounded `EIO` in the command output, stderr or error text.
- **No raw error text.** The docker and postgres `observed`/`reason` are fixed templates (`docker info failed (exit N)`, `docker info did not answer within 20s`, `postgres ping failed`, `postgres ping timed out`, `… reported a storage I/O error (EIO)`). Raw docker/pgx error strings, which carry socket paths and DSN hosts, never reach a result, a row or a comment.
- **Exec** (`DefaultExec`) is `exec.CommandContext` + `cmd.WaitDelay = 2s`: a grandchild that inherited stdout cannot keep `Wait` blocked after the child exits or the context is done.

### Orphaned runners

Processes come from `stalesweep.PSProbe.Processes` (`ps -axww -o pid=,args=`, the SAME identity rule as `scripts/dev` and the stale-run sweep; see `backend/internal/stalesweep/README.md`). Per process: a `--stage-id` → `GetStage`, else `GetRun`; `run.ErrNotFound` → skipped (a runner pointed at another backend, e.g. the acceptance preview); terminal (`StageState.IsTerminal` / `State.IsTerminal`) → a candidate keyed `pid/run/stage`.

- **Confirmation (operator-ratified relaxation of #4142 AC5 by one sweep).** A candidate is reported only once the SAME key is terminal on `ConfirmSweeps` (default **2**) CONSECUTIVE Checks; a key absent from one Check is dropped. This absorbs the runner's own post-upload teardown: the trace upload makes the stage terminal before the process exits. **Worst-case detection latency is 2 × interval.** Residual: a runner that lingers past its terminal stage for exactly one sweep and then exits is never reported. The rule is clock-free on purpose — comparing host time against a DB-stamped `updated_at` is the cross-clock-domain trap (#3048). `ConfirmSweeps: 1` restores immediate reporting.
- **Unattributed runners never read ok.** A `fishhawk-runner` with no readable run id (missing, valueless, non-UUID, or the single-token `--run-id=` form) or an unreadable stage claim (`--stage-id` with a missing or non-UUID value, or `--stage-id=`) is counted unattributed; with no failing runner the probe reads `unknown` naming the count. A process with no `--stage-id` at all is attributed at run level.

## Ticker, emit and dedup

`Run(ctx)` errors only on an empty probe list, logs the `not_applicable` probes once at INFO, ticks immediately then every `Interval` (default 5m). `Tick` = `Sweep` + emit; each failing result's emit runs under its own `recover`, so a panicking audit store, lookup or notifier is a WARN and the next failing result is still emitted. A panicking notifier is recorded as `notification: failed` and the row still lands. Nothing in the package logs at ERROR.

Per failing result, key `condition|subject`:

1. Suppressed when the in-memory `lastEmitted[key]` is within `QuietWindow` (default 1h).
2. Else suppressed when `ListAll{Category: ops_probe_failed, Limit: 200}` holds a row with the same `condition` + `subject` whose timestamp is within the window (the timestamp is written from the Ticker's own clock, so the comparison is single-clock-domain; a hit is cached in memory). A read error WARN-logs and falls back to in-memory only — fail open toward one notification, never toward silence.
3. Otherwise `lastEmitted[key] = now` is set FIRST (so neither a failed notify nor a failed append can turn every tick into a notification), the notifier runs under `NotifyTimeout` (default 15s), and ONE `AppendGlobalChained` row is written: system actor `ops-monitor`, nil account, payload `{condition, probe, subject, status, observed, threshold, remedy, reason, deployment, detail, quiet_window_seconds, notification: posted|failed|disabled}`.

A window comparison counts a clock step in either direction: an entry more than one window in the future does not suppress.

**Dedup residuals.** (a) The restart lookup reads only the newest 200 `ops_probe_failed` rows: with more than 200 newer rows inside one window, a restart can re-notify once. (b) A restart loses the in-memory `lastEmitted` map (the audit lookup covers it, residual (a) aside) AND the orphan candidate set, so an orphan seen once before a restart needs `ConfirmSweeps` fresh sightings after it. (c) Without an audit store the dedup is in-memory only.

## Comment surface

`IssueCommentNotifier` posts to ONE operator-configured `owner/name#N` issue (`ParseIssueRef`, strict) through the GitHub App client's direct `CreateIssueComment` (scope resolved as `server.resolveRepoScope` does: installation id → installation scope, `forge.ErrNotInstalled` → zero scope, other error → no post). It **deliberately bypasses `issuecomment.Channel`/`Router` (ADR-015)**: those are run-anchored and a host condition has no run. **Slack parity for ops conditions needs a follow-up.** `RenderComment` carries a hidden `<!-- fishhawk-ops-monitor:v1 condition=<c> -->` marker, the condition, probe, subject, observed vs threshold, deployment, remedy and the suppression window — and nothing from `Detail` (host paths, process argv). **The notify issue must be access-controlled**: the comment still names pids, run ids and host metrics. Unset → audit-only. Surface row: `docs/issue-comment-surfaces.md`.

## Tests

`opsmonitor_test.go` (Sweep gating/normalization/timeout/panic, Ticker emit/dedup/fail-safety, `ResolveDeployment`), `probes_test.go` (every probe branch through seams, real `statfs` and real `WaitDelay` exec), `notify_test.go` (`ParseIssueRef`, `RenderComment`, scope resolution, the no-raw-error-text guard), `opsmonitor_pg_test.go` (real Postgres audit repo: one row across two Ticks, a restarted Ticker stays suppressed).
