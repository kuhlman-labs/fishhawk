# backend/internal/timescale

Test-support timing multiplier for wall-clock boundary-timeout tests (#1984,
guarding the #1805 pipe-leak group-kill family), plus `SpawnGate`, a
spawn-gated deadline for the same family (#4177). Everything here is test
support: only `_test.go` files import it.

## Why

A boundary-timeout test pits a context deadline against a subprocess spawn/reap
race: a fake CLI forks a grandchild that holds an inherited stdout pipe, and the
test asserts `cmd.Output()` returns *at the deadline* (via `procgroup.Harden`'s
whole-group SIGKILL / WaitDelay) rather than hanging. These tests carry raw
millisecond bounds — a 200–300ms ctx deadline, a 3s/5s elapsed upper bound, a
30s wedge sleep. On a loaded 2-core CI runner the fork/exec and signal delivery
can slip past a tight raw bound, red-lining an otherwise-correct test (the
observed `TestReviewer_PipeLeakGroupKillTimeout` failure at 30.31s — exactly the
30s grandchild-liveness wait — was the 300ms deadline firing the group kill
before the fake `claude` had fork/exec'd the grandchild and flushed its pidfile).

Deriving every deadline-competing duration through `D(base)` scales all of them
by one factor, so every discrimination ratio (`bound/deadline`, `wedge/bound`,
`long-grace/bound`) is preserved by construction while the family gains headroom
on CI-class hardware.

Scaling did **not** fix that 30.31s failure, though (#4177). It recurred in the
runner's gate container, where `CI` is unset and the factor is therefore `1`,
under a full `-race` module loop: the raw 300ms deadline still started at call
time and still raced fork/exec. Scaling widens a margin only where the factor is
above 1; it cannot separate a deadline whose expiry is the verdict from a spawn
that must complete first. That separation is `SpawnGate` (below).

## Contract

- `Factor() int` — the timing multiplier.
  - `FISHHAWK_TEST_TIME_SCALE` (explicit positive integer, `1..1000`) wins over
    everything.
  - Otherwise a **non-empty** `CI` env var (GitHub Actions sets `CI`
    unconditionally on every step) yields the CI default `5`.
  - Otherwise `1` (unchanged local behavior).
  - A set-but-invalid override (non-integer, zero, negative, or above `1000`)
    **panics** with a precise message — fail closed, never a silent `1`. The
    `1000` cap keeps `D`'s output far inside `time.Duration`'s int64 range so a
    huge value can never wrap to a misleadingly short or negative duration.
- `D(base time.Duration) time.Duration` — returns `base * Factor()`.

- `SpawnGate` — see the next section.
- `WritePidFile(path, pid)` (atomic: write `path+".tmp"`, then `os.Rename`),
  `ReadPidFile(path) (int, bool)` (`true` only for a positive integer) and
  `PidFileReady(path) func() bool` — the shared pid-file helpers the fakes and
  the gate use.

## When scaling is not enough: SpawnGate (#4177, #3587 class)

A boundary-timeout test has two deadline ROLES, and one raw
`context.WithTimeout` started at call time conflates them:

- the **cheap deadline**, whose expiry IS the verdict under test (the 300ms /
  200ms deadline that fires the group kill or the WaitDelay path);
- the **spawn budget**, a must-complete window for the precondition the verdict
  depends on (the fake CLI has fork/exec'd the grandchild that holds the pipe).

When the cheap deadline also has to cover spawn, a loaded host fires the group
kill before the grandchild exists, and the test fails 30s later on a symptom
("grandchild never wrote its pid") that reads as a defect in the control under
test. This is the #3587 class; `timescale` scaling does not fix it.

`NewSpawnGate(spawnBudget, deadline, ready)` returns a `context.Context` that
separates them:

- It polls `ready()` immediately and then every unscaled 20ms. Once `ready()`
  is true it records `ArmedAt()` and arms a **real stdlib**
  `context.WithTimeout(deadline)`, so the expiry is a genuine
  `context.DeadlineExceeded`.
- The spawn budget is generous (`D(10s)` in the fixtures, more than 30x the
  cheap deadline). It costs nothing on the happy path, because the gate arms
  the moment the precondition holds. Exhausting it ends the gate with
  `DeadlineExceeded` (so the code under test unblocks) and flags it.
- `RequireArmed(t)` returns the arm instant, or fails the test with a
  `PRECONDITION (#4177)` message naming the spawn budget and host load or a
  fork/exec failure, and saying it is NOT a defect in the control under test.
  Call it right after the call under test returns and **before** any
  behavioral assertion.
- `Err()` is nil until `Done()` is closed and non-nil once it is: every
  finishing path sets the error before `close(done)` inside one critical
  section. A stdlib context derived from a non-stdlib parent reads
  `parent.Err()` right after `parent.Done()` and panics on nil, so the
  ordering is load-bearing (`TestSpawnGate_ErrNilUntilDone` goes red under
  `-race` without it).
- `Deadline()` reports construction + spawn budget + deadline, a conservative
  upper bound rather than the instant `Done` fires. The review adapters read
  only its presence bit, so they add no `cfg.Timeout` overlay.
- `Stop()` (idempotent; register it with `t.Cleanup`) ends the gate with
  `context.Canceled` and waits for the watcher goroutine.

How the #1805 fixtures use it (claudecode, codex, procgroup):

- **The fake records the grandchild pid itself, right after
  `exec.Cmd.Start`, with `WritePidFile`.** `Start` returns only once the child
  has exec'd, so a recorded pid proves the grandchild exists and holds the
  inherited stdout (and, in the escape mode, has already left the group). This
  removes grandchild runtime-init latency from the precondition. The atomic
  rename is defense in depth against a partial read and has no deterministic
  counterfactual.
- **Elapsed bounds are measured from the arm instant**, not from the call, so
  a slow spawn cannot eat the `D(3s)` / `D(5s)` bound.
- **Escape arms need grace much larger than the deadline.** `os/exec` starts the
  `WaitDelay` timer when `Wait` observes the child's exit, and the escape fake
  exits right after it records the pid, i.e. near the arm instant. The gated
  deadline must still fire before exit + grace for the trigger to be the
  deadline. The margin: arm-to-exit latency (armedAt minus the fake's exit)
  must stay below `escapeGrace() - deadline`. The fixtures use
  `escapeGrace() = 10 * deadline` (1.8s of margin at factor 1); do not shrink it
  back toward the deadline. A failing escape case logs armedAt minus the
  fake's exit.
- **Margin pin.** Each package has a test (`TestReviewer_PipeLeakToleratesSlowSpawn`,
  `TestInference_PipeLeakToleratesSlowSpawn`, `TestHarden_ToleratesSlowSpawn`)
  that runs the same cases with the fake sleeping `D(1s)` before it forks,
  between the old cheap deadline and the spawn budget. It asserts both
  inequalities as preconditions and must pass under the gate. Its negative
  old-shape arm runs the same slow fixture under a plain
  `context.WithTimeout(cheap deadline)` and asserts the pid is never recorded:
  the old deadline kills the fake during its pre-spawn sleep.

## What to scale (and what not to)

Scale via `D(base)` **every duration that competes with the boundary being
tested**: ctx deadlines, kill graces (short and long), elapsed upper bounds,
helper wedge/failure sleeps, and spawn/reap liveness waits. Do **not** scale the
20ms poll intervals — they are sub-boundary sampling granularity, not a boundary,
and keeping them fixed keeps polling responsive at any factor. Do not touch
non-boundary helper modes that already carry a large fixed margin and assert no
wall-clock upper bound (e.g. claudecode's `slow`/`slow_brief`), nor any
production duration.

### When the boundary is a production constant

Sometimes the duration that competes with the test's bound is itself a
**production constant** — a graceful-shutdown timeout, a kill grace. The
production duration is still never scaled: the shipped default must stay
byte-identical. Instead, demote the constant to an unexported package `var`,
add a save/restore helper that swaps it for the duration of one test, and have
the test override it with `D(base)` (capturing `base` from the production
default first) so every competing bound — the inner timeout and the test's own
outer guard — derives from the same factor and their ratio holds at any scale.
Two in-tree instances: claudecode's `var killGrace` / `setKillGrace`, and
fishhawk-mcp's `var httpShutdownTimeout` / `setHTTPShutdownTimeout` (#2628).
Because the override is test-only, the shipped default is unchanged — this does
not contradict the "nor any production duration" line above.

## Env inheritance invariant

The three test-helper re-exec builders (procgroup, claudecode, codex) all append
their helper env to `os.Environ()`, so `FISHHAWK_TEST_TIME_SCALE` and `CI`
propagate to the fake-CLI and grandchild processes — a driving test and its
helpers therefore compute the SAME factor. That is what keeps a scaled wedge
sleep above the scaled elapsed bound; a factor mismatch would shrink the
wedge-above-bound margin and could spuriously fail the test.
