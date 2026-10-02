# backend/internal/scheduler

The in-process producer of the `scheduled` trigger form (E79.1 / #3725). A workflow that declares a workflow-level `schedule` (`spec.Schedule`: five-field numeric cron, IANA `timezone` defaulting to UTC, optional anchor `issue`) AND lists `scheduled` in `applies_to.trigger` gets exactly one run per due window. The grammar, its validation and the evaluator (`CronSchedule.Next` / `Prev`) live in `backend/internal/spec/schedule.go`; this package only decides WHEN to start and records WHAT happened.

## Shape

`Ticker` reuses the `campaigndriver.Ticker` background-worker shape: an exported `Tick(ctx)` one-pass method driven by a `Run(ctx)` interval loop (fires once at startup, then every `Interval`, default `DefaultInterval` = 60s). It is **off by default**: fishhawkd starts it only behind `--enable-scheduler`, and serve.go's start decision fails closed (logged reason, no ticker) when a dependency is unwired or no repositories are configured.

Seams, all defined HERE so the package never imports `backend/internal/server`:

| Field | Interface | Production binding (serve.go) |
|---|---|---|
| `Specs` | `SpecSource.FetchSpec(ctx, repo) (content, sha, err)` | GitHub App `GetRepoInstallation` + `GetWorkflowSpec` at the default branch; `sha` is the blob SHA, used as `workflow_sha` |
| `Starter` | `RunStarter.StartScheduledRun(ctx, StartRequest) (StartOutcome, error)` | `server.Server.StartScheduledRun`, which drives the EXISTING `handleCreateRun` in-process |
| `Audit` | `AuditAppender.AppendGlobalChained` | `audit.Repository` |

`Repos` (owner/name), `RunnerKind` (`github_actions` / `local`), `Now` and `Logger` complete the struct. `Specs`, `Starter`, `Audit` and a non-empty `Repos` are required: `Run` returns an error without them and `Tick` is a logged no-op (it fetches nothing and starts nothing).

## Per-tick contract

For each configured repository:

1. `FetchSpec` then `spec.ParseBytes`. A fetch or parse failure WARN-logs, is recorded on the snapshot as `SpecError`, and **audits nothing** — there is no window decision to record. The next successful tick clears it.
2. Workflows WITHOUT a `schedule` are never touched (a workflow listing `scheduled` in `applies_to.trigger` but declaring no schedule is not started). A schedule removed from the spec drops out of the snapshot.
3. For each scheduled workflow: `window = Prev(now)` (the latest fire at or before now, in the schedule's zone) and `next = Next(now)`. If this process already recorded a definitive outcome for `window` (or a later one) it does nothing. Otherwise it calls the starter with `IdempotencyKey(workflow, window)` = `scheduled:<workflow_id>:<window start UTC RFC3339>`, the raw spec bytes, the blob SHA, the anchor issue and `RunnerKind`.
4. The outcome is appended to the **global** audit chain (a refusal has no run to chain on), actor `system` / `fishhawkd/scheduler`:

| Starter result | Audit category | Extra payload | Window marked attempted |
|---|---|---|---|
| `OutcomeStarted` (POST /v0/runs 201) | `scheduled_run_started` | `run_id` | yes |
| `OutcomeAlreadyStarted` (200 Idempotency-Key replay) | `scheduled_run_skipped` | `run_id`, `reason: already_started` | yes |
| `OutcomeRefused` (any other 4xx) | `scheduled_run_refused` | `code`, `message`, `status` verbatim (e.g. 402 `budget_exhausted`, 422 applies_to / charter) | yes |
| error return (5xx, transport) | — (log-only) | — | **no**: retried next tick |
| unknown outcome kind | — (log-only) | — | no: treated as transient |

Every entry carries `repo`, `workflow_id`, `window_start`, `idempotency_key` and `runner_kind`; with `runner_kind: local` it also carries `dispatch_note` (`DispatchNoteLocal`). The three categories are registered in `backend/internal/audit/categories.go`. They are not issue-comment activity categories.

So one window yields at most ONE started-or-refused entry per process, plus one `scheduled_run_skipped` per restart that ticks inside it — never a per-tick flood. The audit append is best-effort (the campaign driver's posture): an append failure WARN-logs and does not re-open the window, since re-attempting would only replay the same run.

## Exactly-once

The in-memory attempted mark is NOT what makes a window exactly-once; it only suppresses the per-tick audit flood, and a restarted process starts with it empty. The run row is exactly-once per (repo, workflow, window) because the Idempotency-Key reaches `handleCreateRun`'s replay lookup, backed by the `(idempotency_key, repo)` unique index (migration 0011). Two ticks that race past the lookup collide on that index; the loser surfaces as a 5xx, which this package treats as transient, and its next tick replays as `already_started`. `TestTick_TwoWindowsAndRestart_ExactlyTwoRuns` pins this at unit level (a fresh `Ticker` over the same starter is the restart) and its counterfactual — `IdempotencyKey` returning `""` mints a third run.

**Replay residual (honest).** `handleCreateRun`'s replay lookup sits AFTER request and spec validation (#2366), and the scheduler fetches and parses the spec BEFORE it calls the starter at all. So a window whose run already exists is only recorded as `already_started` when both succeed on that tick. When the scheduler-side spec fetch or parse fails, the tick records `SpecError` and nothing else; when the handler's validation refuses the request (4xx) the window is recorded as `scheduled_run_refused`; when it fails with a 5xx it is a transient retry. In none of these cases is a second run minted — the residual is the RECORD, not the run.

## Catch-up policy

Only the LATEST window is ever attempted — never a backfill of the windows missed while fishhawkd was down. A newly declared schedule, or a fishhawkd restarted after missing a fire, starts ONE run as soon as it ticks, even late in that window. Deliberate simplicity; a max-lateness knob is a possible follow-up. Daylight-saving behaviour is the evaluator's: a fall-back repeated local hour is two distinct UTC instants and so two windows (two runs); a spring-forward skipped time never fires.

## Local runners

The scheduler does NOT dispatch. With `RunnerKind == local` a started run parks at `awaiting_host_dispatch` until a host dispatches it (`fishhawk_dispatch_stage` / `fishhawk_run_stage`); the audit payload and the snapshot both say so. A scheduled run otherwise stops at every gate exactly like a hand-started one: the starter drives `handleCreateRun`, so the plan-reviewer capability gate, the blocking periodic budget, `applies_to`, the charter and the Idempotency-Key replay all apply unchanged.

## Visibility

`Snapshot()` (every configured repo, in `Repos` order) and `SnapshotFor(repo)` (`ok == false` for an unconfigured repo) expose `Scanned`, `LastTickAt`, `SpecError`, `RunnerKind`, `DispatchNote`, and per scheduled workflow its `Cron`, `Timezone`, `Issue`, `CurrentWindowStart`, `NextDueAt`, `ScheduleError` and `LastOutcome` (including `transient_error`, which is snapshot-only). serve.go adapts it to the `server.ScheduleSource` behind `GET /v0/schedules` and `fishhawk_list_schedules`.

## Deployment target

v0 targets a SINGLE fishhawkd instance. Under multiple instances the run row stays exactly-once (the unique index above is database-wide), but each instance keeps its own attempted mark, so the instances would each record an outcome for the same window — one `started` plus a `skipped` per other instance, or one `refused` per instance. Avoiding those duplicate admission AUDITS would need a Postgres advisory lock around the per-window decision, the same in-process-mutex-vs-advisory-lock boundary `backend/internal/workmgmt`'s child-number discovery (#1958) records.
