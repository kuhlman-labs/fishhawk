# backend/internal/opsmonitor

Opt-in host and process health monitor for fishhawkd (E85.1 / #4142; scope-amendment probes E85.2 / #4146). A `Ticker` sweeps eight probes on an interval; each FAILING `(condition, subject)` produces ONE run-less global-chain `ops_probe_failed` audit row and ONE operator issue comment per quiet window. Off by default; wired by `fishhawkd serve --enable-ops-monitor` and the dry-run `fishhawkd ops-probe` subcommand (`backend/cmd/fishhawkd/ops_monitor.go`, flags in `backend/cmd/fishhawkd/README.md`).

## Result statuses

Every probe result is exactly one of:

| Status | Meaning |
|---|---|
| `ok` | the probe observed the condition and it is within threshold |
| `failing` | the probe observed the condition and it is out of threshold; carries `condition`, `observed`, `threshold` and a non-empty `remedy` |
| `unknown` | the probe could not observe (error, timeout, panic, missing input); carries a `reason`; never emitted as a row or comment, WARN-logged once per probe per quiet window |
| `not_applicable` | a host-local probe under a non-local deployment; its `Check` is never called |

## Fail-safety (`Sweep`)

`Sweep(ctx, probes, deployment, probeTimeout)` is the single chokepoint. A host-local probe under a non-local deployment yields one `not_applicable` result without running. Every other probe runs on its own goroutine under `probeTimeout` (default 30s) and `Sweep` selects on the result vs the deadline, so a probe that ignores its context cannot stall a tick. Residual: such a probe's goroutine is abandoned, not killed, so a Check that ignores its context and never returns leaks ONE goroutine per tick for as long as it stays wedged. Every production probe is context-bound (`DefaultExec` is `CommandContext` + `WaitDelay`, the GOCACHE walk checks `ctx`, the postgres `Ping` carries its own timeout), so this needs a new probe that breaks that rule. Each of these reads `unknown`, never `ok`: a deadline (`probe timed out after <d>`), including a result that arrives after the deadline; a panic (`probe panicked`; the panic value is not logged); zero results; an empty or unrecognized status; a nil probe or one whose `Name`/`HostLocal` panics. A failing result with no remedy gets the per-condition table remedy, or a generic one for an unlisted condition, so a failing row never has an empty remedy.

## Deployment

`ResolveDeployment(raw, getenv)`: `""`/`auto` → `kubernetes` when `KUBERNETES_SERVICE_HOST` is set (the kubelet injects it into every pod), else `local`; `local|kubernetes|remote` are honored verbatim; anything else is an error. `auto` cannot detect a non-k8s remote host, so pass `remote` explicitly there. The five host-local probes (disk, gocache, load, docker, runners) are `not_applicable` unless the deployment is `local`; the postgres and the two amendment probes read the tracker, not the host, and run everywhere.

## Probes

| Probe | Host-local | ok | failing (condition) | unknown |
|---|---|---|---|---|
| `disk` | yes | `statfs` `f_bavail * f_bsize` ≥ min free | below min → `disk_free_low` | empty path, statfs error, non-darwin/linux build |
| `gocache` | yes | sampled estimate ≤ max | estimate > max → `gocache_over_threshold` | dir unresolvable (`go env GOCACHE` error, empty, `off`), root missing, README marker absent (`not a Go build cache`), walk cancelled |
| `load` | yes | 1-minute load ≤ factor × cores | load > factor × cores → `host_load_high` | no load source readable, cores ≤ 0 |
| `docker` | yes | `docker info --format {{.ServerVersion}}` exits 0 | EIO text → `storage_eio` (subject `docker`); any other non-zero exit or the 20s bound → `docker_unreachable` | `docker` not on PATH |
| `postgres` | no | `Ping` nil | EIO text → `storage_eio` (subject `postgres`); any other error → `postgres_unreachable` | no database configured (nil `Ping`) |
| `runners` | yes | every runner is attributed and found in this tracker, and its stage/run is live | a pid/run/stage seen terminal on `ConfirmSweeps` consecutive Checks → `orphaned_runner` (subject `pid/run/stage`) | no database, ps scan error, a tracker lookup error, any UNATTRIBUTED runner, or any runner NOT FOUND in this tracker, when nothing is failing |
| `amendment_window` | no | no pending amendment on a live stage is due | a pending amendment on a NON-terminal stage older than window − warn lead → `amendment_window_closing` (subject = amendment id) | no database, scan error, any pending amendment whose stage is hidden (RLS), or a truncated scan — see § "Scope amendments" |
| `amendment_terminal` | no | no pending amendment sits on a terminal stage | a pending amendment whose stage is terminal → `amendment_pending_on_terminal_stage` (subject = amendment id) | as `amendment_window` |

- **GOCACHE sizing** is `scripts/dev`'s sampled estimate: regular-file bytes in the 16 shards `00,10,…,f0` × 16 (cmd/go files entries under the hash's first byte). The directory resolves lazily at `Check` time from `go env GOCACHE` when no `Dir` is configured — never at flag registration. A directory without the cmd/go README marker (`cached build artifacts from the Go build system`) reads `unknown` instead of a misleading 0.
- **Load** ports `runner/internal/hostload`'s source order (`/proc/loadavg`, `sysctl -n vm.loadavg` `{ a b c }`, `uptime` with `load average(s):`) because the runner is a separate module; default factor 4.0 = the runner's `hostload.DefaultFactor`.
- **EIO indicator**: a case-insensitive `input/output error` or a word-bounded `EIO` in the command output, stderr or error text.
- **No raw error text.** The docker and postgres `observed`/`reason` are fixed templates (`docker info failed (exit N)`, `docker info did not answer within 20s`, `postgres ping failed`, `postgres ping timed out`, `… reported a storage I/O error (EIO)`). Raw docker/pgx error strings, which carry socket paths and DSN hosts, never reach a result, a row or a comment.
- **Exec** (`DefaultExec`) is `exec.CommandContext` + `cmd.WaitDelay = 2s`: a grandchild that inherited stdout cannot keep `Wait` blocked after the child exits or the context is done.

### Orphaned runners

Processes come from `stalesweep.PSProbe.Processes` (`ps -axww -o pid=,args=`, the SAME identity rule as `scripts/dev` and the stale-run sweep; see `backend/internal/stalesweep/README.md`). Per process: a `--stage-id` → `GetStage`, else `GetRun`; `run.ErrNotFound` (or a nil row) → counted not-found; terminal (`StageState.IsTerminal` / `State.IsTerminal`) → a candidate keyed `pid/run/stage`.

- **Confirmation (operator-ratified relaxation of #4142 AC5 by one sweep).** A candidate is reported only once the SAME key is terminal on `ConfirmSweeps` (default **2**) CONSECUTIVE Checks; a key absent from one Check is dropped. This absorbs the runner's own post-upload teardown: the trace upload makes the stage terminal before the process exits. **Worst-case detection latency is 2 × interval.** Residual: a runner that lingers past its terminal stage for exactly one sweep and then exits is never reported. The rule is clock-free on purpose — comparing host time against a DB-stamped `updated_at` is the cross-clock-domain trap (#3048). `ConfirmSweeps: 1` restores immediate reporting.
- **Not-found runners never read ok.** A not-found runner is either pointed at another backend (e.g. the acceptance preview) or a tenanted run hidden by row-level security: `runs`/`stages` are FORCE RLS (migration 0057) and fishhawkd sets no `app.account_id` here, so under a role without `BYPASSRLS` a tenanted run reads as not-found. The two are indistinguishable, so with no failing runner the probe reads `unknown` naming the not-found count. Cost: while another backend's runner is live on the host (an acceptance preview), the probe reads `unknown` — a WARN once per quiet window, never a row or a comment.
- **Unattributed runners never read ok.** A `fishhawk-runner` with no readable run id (missing, valueless, non-UUID, or the single-token `--run-id=` form) or an unreadable stage claim (`--stage-id` with a missing or non-UUID value, or `--stage-id=`) is counted unattributed; with no failing runner the probe reads `unknown` naming the count. A process with no `--stage-id` at all is attributed at run level.

### Scope amendments (E85.2 / #4146)

Both probes read ONE query, `scopeamendment.PendingLister.ListPendingWithStage` (`ListPendingScopeAmendmentsWithStage`): `status = 'pending'` rows requested within the lookback, LEFT JOINed to their stage's state, with the database's `now()` returned as `observed_at`, newest first, at most 200 rows. One shared classifier partitions the scan.

- **Derived deadline.** `scope_amendments` has no deadline column (pinned by `TestPostgres_ScopeAmendmentsHasNoDeadlineColumn`): the deadline is `requested_at + window`. The window defaults to 15m, the implement agent's decision wait-poll (`server.AmendmentPollWindowSeconds`, pinned equal by a fishhawkd test); past it the agent stops waiting and a later approval lands only through `fishhawk_retry_stage`.
- **One clock domain (#3048).** Age is `observed_at − requested_at`, both stamped by the database. No Go `time.Now()` is compared against a DB timestamp.
- **Window vs terminal partition.** A row on a NON-terminal stage (`run.StageState.IsTerminal` false) is the window probe's; it fails once its age STRICTLY exceeds `window − warn lead`. A row on a terminal stage (succeeded, failed, cancelled, superseded) is the terminal probe's and is never a window alert.
- **Zero warn lead is no lead.** `AmendmentWindowProbe.WarnLead` is used verbatim: `0` fires only once the age exceeds the window itself. Only a non-positive `Window` or `Lookback` falls back to its default. fishhawkd passes all three resolved flag values.
- **#4127 relation.** The scan returns pending rows only, and the classifier drops any non-pending row as a second guard, so a row the #4127 lapse step marked `lapsed` is never reported and the two never double-report. The lapse step deliberately leaves a FAILED stage's requests pending (a late approval plus `fishhawk_retry_stage` still folds), so the terminal probe reports those, plus any row the lapse step missed: decide each with `fishhawk_decide_scope_amendment` (deny to close it, or approve and retry the stage).
- **Hidden stages never read ok.** `stages` is FORCE row-level secured through its run (migration 0057) and `scope_amendments` is not; fishhawkd sets no `app.account_id` here, so under a non-`BYPASSRLS` role a tenanted run's stage is invisible. The LEFT JOIN keeps such a row with a NULL state; it counts as hidden and, with nothing failing, the probe reads `unknown` naming the count. A failing result wins over a hidden count.
- **Truncation.** A scan that returns 200 rows is truncated. Newest-first ordering keeps the freshest pending rows (the only ones whose window can still be met) inside the page; rows older than the newest 200 are unobserved until newer ones settle. A truncated scan ALWAYS adds an `unknown` result naming the truncation, alongside any failing results, so the Sweep WARN fires.
- **Lookback.** Only rows requested within the lookback (default 168h) are scanned. It bounds the pre-#4127 backlog of stale pending rows (53 on the 2026-10-08 snapshot) so enabling the monitor cannot emit a burst of rows and comments.
- **Recommended defaults.** The 10m warn lead and the 168h lookback are recommended defaults, not issue-stated numbers; each is a flag. The warn-lead rule (fire at window − lead, `0 ≤ lead < window`) is the planner's design choice; a degenerate config (a lead ≥ the window, a lookback ≤ the window) is refused at startup.
- **Latency.** The probe fires on the first sweep after age > window − lead. At the default 5m interval and 10m lead the first alert lands at age 5–10m, with 5–10m left. An interval longer than the lead can deliver the first alert after the window has closed.
- **Repeat.** Each amendment id is its own `(condition, subject)`: one row and one comment per id per quiet window, re-notified every quiet window while the row stays pending (the same repeat rule as every persistent condition).
- **`Fields` payload contract.** Both probes set `Result.Fields`, which `emit` copies verbatim onto the row as `fields`: `amendment_id`, `run_id`, `stage_id`, `stage_state`, `requested_at` (RFC 3339), `age_seconds`, plus for the window probe `window_seconds`, `warn_lead_seconds`, `time_remaining_seconds` (`max(0, window − age)`) and `window_elapsed`. The row is readable by every tenant, so `Fields` carries ids, counts and durations only: never amendment paths, the agent's reason text, host paths or argv.

## Ticker, emit and dedup

`Run(ctx)` errors only on an empty probe list, logs the `not_applicable` probes once at INFO, ticks immediately then every `Interval` (default 5m). `Tick` = `Sweep` + emit; each failing result's emit runs under its own `recover`, so a panicking audit store, lookup or notifier is a WARN and the next failing result is still emitted. A panicking dedup lookup is recovered on its own and degrades like a read error (step 2), so the notifier is still reached. A panicking notifier is recorded as `notification: failed` and the row still lands. Nothing in the package logs at ERROR.

Per failing result, key `condition|subject`:

1. Suppressed when the in-memory `lastEmitted[key]` is within `QuietWindow` (default 1h).
2. Else suppressed when `ListAll{Category: ops_probe_failed, Limit: 200}` holds a row with the same `condition` + `subject` whose timestamp is within the window (the timestamp is written from the Ticker's own clock, so the comparison is single-clock-domain; a hit is cached in memory). A read error or a panicking lookup WARN-logs and falls back to in-memory only — fail open toward one notification, never toward silence.
3. Otherwise `lastEmitted[key] = now` is set FIRST (so neither a failed notify nor a failed append can turn every tick into a notification), the notifier runs under `NotifyTimeout` (default 15s), and ONE `AppendGlobalChained` row is written: system actor `ops-monitor`, nil account, payload `{condition, probe, subject, status, observed, threshold, remedy, reason, deployment, quiet_window_seconds, notification: posted|failed|disabled}`, plus `fields` when the result carries `Fields` (§ "Scope amendments").

**The row is readable by every tenant.** It is written with a nil account, and `GET /v0/audit` shows a tenanted caller its own entries PLUS untenanted ones (`backend/internal/server/reads.go` `handleListGlobalAudit`). So the payload never carries `Detail` (the disk path, the GOCACHE dir): it is the comment's field set. An `orphaned_runner` row still names the pid, run id and stage id in `subject`/`observed`, whichever tenant the run belongs to — the same exposure as other untenanted global-chain rows that name run ids (e.g. `invariant_violation`). `unknown` reasons may carry raw error text (`statfs failed: …`, `process scan failed: …`), but an `unknown` result is never emitted: it reaches only the WARN log and `ops-probe` output.

A window comparison counts a clock step in either direction: an entry more than one window in the future does not suppress.

**Dedup residuals.** (a) The restart lookup reads only the newest 200 `ops_probe_failed` rows: with more than 200 newer rows inside one window, a restart can re-notify once. (b) A restart loses the in-memory `lastEmitted` map (the audit lookup covers it, residual (a) aside) AND the orphan candidate set, so an orphan seen once before a restart needs `ConfirmSweeps` fresh sightings after it. (c) Without an audit store the dedup is in-memory only.

## Comment surface

`IssueCommentNotifier` posts to ONE operator-configured `owner/name#N` issue (`ParseIssueRef`, strict) through the GitHub App client's direct `CreateIssueComment` (scope resolved as `server.resolveRepoScope` does: installation id → installation scope, `forge.ErrNotInstalled` → zero scope, other error → no post). It **deliberately bypasses `issuecomment.Channel`/`Router` (ADR-015)**: those are run-anchored and a host condition has no run. **Slack parity for ops conditions needs a follow-up.** `RenderComment` carries a hidden `<!-- fishhawk-ops-monitor:v1 condition=<c> -->` marker, the condition, probe, subject, observed vs threshold, deployment, remedy and the suppression window — and nothing from `Detail` (host paths, process argv). **The notify issue must be access-controlled**: the comment still names pids, run ids and host metrics. Unset → audit-only. Surface row: `docs/issue-comment-surfaces.md`.

## Tests

`opsmonitor_test.go` (Sweep gating/normalization/timeout/panic, Ticker emit/dedup/fail-safety including the `fields` payload, `ResolveDeployment`), `probes_test.go` (every probe branch through seams, real `statfs` and real `WaitDelay` exec), `amendments_test.go` (both amendment probes through a fake reader: the strict window boundary, zero warn lead, elapsed clamp, terminal partition, the lapsed/denied exclusion, every unknown mode, truncation alongside a failing result, per-amendment dedup through the Ticker), `notify_test.go` (`ParseIssueRef`, `RenderComment`, scope resolution, the no-raw-error-text guard), `opsmonitor_pg_test.go` (real Postgres audit repo: one row across two Ticks, a restarted Ticker stays suppressed; the amendment probes end to end over real SQL with DB-side backdating).
