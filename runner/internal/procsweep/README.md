# procsweep — stage orphan reaper

Long-form contract for `runner/internal/procsweep`. Anchor issue: **#3663** (E51.11).

## The problem

An agent can background a process from a shell that then exits. The orphan is
re-parented to pid 1 and — if the shell ran with job control off — keeps the
**already-exited shell's process-group id**. Neither of the runner's two existing
teardown mechanisms reaches it:

- the ppid chain is broken (its parent is pid 1), so no descendant walk finds it;
- `kill(-pgid)` against the *agent's* group does not cover it, because the
  orphan's group is the dead shell's, not the agent's.

In the #3663 incident that left several CPU busy-loops running on the dogfood
host, and every *later* committed-tree verify on that host red-lined on tests
unrelated to its own diff — a category-A failure attributed to the wrong diff.

**Measured, not assumed:** a process backgrounded from a `zsh -c` on the dogfood
host showed `ppid=1` with `pgid=51854`, the pid of the `zsh` that had *already
exited*. POSIX `fork(2)`/`setpgid(2)`: a child inherits its parent's process
group, and re-parenting to init does not change it.

## Why not an env marker

The issue suggested tagging spawned processes with an environment marker and
identifying them by it. **That is not implementable on macOS**, which is the
primary dogfood host:

- `ps -E` prints another process's environment only for root.
- A `KERN_PROCARGS2` sysctl probe against another **same-uid** process was run
  during planning (`FISHHAWK_SWEEP_MARKER=… sh -c 'sleep 40 &'`) and returned a
  29-byte reply carrying `argc`, the exec path and `argv` **only** — no
  environment.

So identification uses the **process-group closure** instead.

## The mechanism

`Recorder` samples the host process table every **2 seconds**
(`orphanSweepInterval` in the runner command) during the stage and accumulates:

1. the transitive **ppid closure** of the runner process (`Descendants`), and
2. the set of **process-group ids** those descendants belong to.

At stage exit `Sweep` **re-reads** the table and `SIGKILL`s every survivor the
recorded state authorises. The pgid half is what reaches an orphan whose whole
ancestor chain has already exited: only the **shell** — which lives for the
duration of the agent's command — needs to have been sampled, not the
short-lived orphan itself.

## Identification: `lstart` only

`Proc.StartRaw` is the raw `ps lstart` text, **equality-compared** against the
recorded value and never re-parsed at compare time. A recorded pid whose
`StartRaw` changed was recycled by the kernel onto a different process and is
**not** a target.

`Proc.Start` is that same field **parsed into an absolute instant**, because it
must be *ordered* against the stage-start floor, not merely compared.

- The parse uses `time.ParseInLocation(…, time.Local)`, **never** `time.Parse`.
  `ps` prints local wall-clock time with **no zone field**, so a UTC parse would
  skew every `Start` by the host's UTC offset: on a UTC+N host a genuine
  descendant would read as started *before* the floor (spared), and on a UTC-N
  host a pre-existing process would read as started *after* it (killed). The
  location is an injectable package var so the parser tests can fix
  `time.FixedZone` at +5h and −7h and assert the resulting absolute instant.
- `etime` is **not used anywhere**: elapsed time shifts between samples and
  cannot serve as an identity discriminator.
- A line whose three leading integers **or** whose `lstart` cannot be parsed is
  **DROPPED** from the `Table` and counted in `Table.Unparsed`. A dropped line is
  invisible to `Descendants`, to `Targets` and to the continuity rule, so such a
  process can never be a target. Fail closed to *not* killing.

**Measured column shape** (Darwin 25.6, verified during implementation):

```
    1     0     1 Tue Aug 11 11:57:20 2026     /sbin/launchd
```

three leading integers, then `lstart` as exactly five whitespace-separated
tokens (`Mon Jan _2 15:04:05 2006`), then the command as the remainder.
procps-ng renders `lstart` in the same ctime-style five-token form.

## The six controls on a kill

Sweeping is a destructive action against processes the runner did not directly
spawn. Six controls bound it. Each has its own test and its own observed-RED
counterfactual.

| # | Control | Why |
|---|---|---|
| 1 | never the runner's own pid | a torn table read can report the runner as its own descendant |
| 2 | never `pid <= 1` | init / kernel task |
| 3 | **group-leader guard** — never a member of the runner's own process group unless the runner LEADS it (`selfPGID == selfPID`) | a hand-run `bin/fishhawk-runner` shares the operator's interactive shell's group; mirrors the shipped precedent in `runner/cmd/fishhawk-runner/lockholder_unix.go`'s `signalLockHolder` |
| 4 | **stage-start floor** (pgid-reuse rule a) | a process admitted ONLY by the pgid closure is a target only if it started at or after the recorder's start instant, so operator shells, the MCP server, `fishhawkd` and other runners are structurally unreachable |
| 5 | **continuity tombstone** (rule b) | a recorded pgid with ZERO live members at a sample is dropped PERMANENTLY, so an emptied group cannot authorise kills against a later group that reuses its id |
| 6 | **re-check at sweep** (rule c) | the table is re-read immediately before killing and selection runs against that FRESH read, never against a stale sample |

Two admission paths, deliberately **asymmetric**: a recorded descendant is
admitted on the `StartRaw` match alone (it *is* ours), while a pgid-closure
member must additionally clear the floor and the tombstone set.

`killPID` is deliberately **pid-addressed**, never `kill(-pgid)`: the recorded
pgid set is an *admission credential*, not a kill target — a group-addressed
signal would reach members `Targets` never admitted and never fenced.

## Named degrades (all fail-open: one printed reason, ZERO kills)

Owned by the runner command's `stageOrphanSweeper`:

| Reason | When |
|---|---|
| `disabled_in_test_binary` | the runner command's package test binary (`orphanSweepDefaultOn = false` in `runTestMain`) |
| `disabled_by_operator` | `FISHHAWK_ORPHAN_SWEEP=off` |
| `not_group_leader_no_recorded_pgids` | the runner does not lead its own group AND no descendant pgids were recorded — nothing this sweep could ever authorise, so it declines before reading the table |
| `proc_table_unsupported` | `procsweep.ErrUnsupported` (Windows: no `ps`, no portable group signal) |
| `proc_table_read_failed` | a `ps` read error at sweep time |

## Residuals — stated, not hidden

1. **PID read-to-kill window.** Between the fresh table read and the
   `kill(pid, SIGKILL)` a few microseconds later, a target pid can exit and be
   recycled onto an unrelated process, which then receives the SIGKILL. Inherent
   to pid-addressed signalling: Linux has `pidfd_open`/`pidfd_send_signal(2)`
   (Linux ≥ 5.1) for race-free targeting, Darwin has no equivalent, and the
   primary dogfood host is macOS. Narrowed by doing the re-read *immediately*
   before the kill pass rather than against a sample up to one interval old, and
   by Darwin allocating pids sequentially with wraparound rather than reusing a
   just-freed pid immediately. **Not eliminated.**
2. **PGID reuse within one sample interval.** The continuity rule tombstones a
   recorded pgid only *at a sample*, and the interval is 2s. A group that empties
   AND whose id is reused AND where the new process starts at or after the floor,
   all strictly *between* two samples, is not tombstoned in time. Bounded by:
   the reuse must land on a pgid this stage's own descendants used, the new
   process must have started after the stage began, and the whole sequence must
   complete inside 2s. Shortening the interval narrows it at linear `ps` cost;
   eliminating it needs a kernel-level group handle neither Darwin nor portable
   POSIX provides.
3. **`lstart` one-second resolution.** `ps lstart` has whole-second granularity,
   so the floor is compared at that same resolution
   (`floor.Truncate(time.Second)`, admit iff `start >= floor`). **This admits a
   pre-existing process started in the SAME wall-clock second as the stage
   start** — a sub-second window, and any process in it was started essentially
   concurrently with the runner. Making the floor strictly-after would instead
   risk sparing a genuine descendant spawned in that same second, a worse trade
   for a reaper.
4. **Sampling-based, so not a containment boundary.** A descendant that both
   starts AND escapes (new session or new process group) entirely between two
   samples, and whose whole ancestor chain also exits within that window, is
   never recorded and never reaped. The pgid closure widens coverage
   substantially — only the long-lived shell needs to be sampled — but the
   structural fix is a cgroup (Linux) or a job object (Windows), neither of which
   exists on macOS.
5. **The backstop sweep is log-only.** `run()` calls the sweep twice under one
   `sync.Once`: the normal post-invoke site, whose event rides in the signed
   bundle, and a `defer` that backstops the **cancel / timeout / early-return**
   paths. On those paths `run()` is already returning — there is no `res.Events`
   left to append to and no bundle to fold the event into — so the reap is
   visible only as a `stage_orphans_reaped` **log line**, with no bundle event.
   Accepted: the reap itself is the point, and the log line is enough to
   diagnose from.

## Test seams

`readTable` and `killPID` are unexported package vars, so no production code can
reach them. `SetTestHooks` exposes them to the runner command's wiring test,
which must drive the REAL `Recorder`/`Sweep` against a synthetic process table;
`FormatStart` lets such a test build a `StartRaw` the identity discriminator
accepts.

The integration test (`procsweep_integration_test.go`) drives a REAL escaping
grandchild through the real `ps` reader and the real `SIGKILL`. Two notes for
anyone editing it:

- The grandchild is spawned with `SysProcAttr{Setsid: true}` **alone**, never
  together with `Setpgid`: `setsid(2)` already makes the caller a session *and*
  process-group leader, so a simultaneous request to place it in a specified
  group fails **EPERM**.
- It passes the test process's **real** process group to `Sweep`, never a posed
  `selfPGID == selfPID`. Under `go test` the test binary does not lead its own
  group (it shares `go`'s, which shares the invoking shell's), so posing
  leadership disarms the group-leader guard and authorises kills against every
  live member of that shared group — **observed for real while writing the test:
  the suite killed its own `go test` parent.** `killPID` is additionally fenced
  to the grandchild for the duration, so a selection regression fails the test
  loudly instead of reaping the harness.
