# backend/internal/timescale

Test-support timing multiplier for wall-clock boundary-timeout tests (#1984,
guarding the #1805 pipe-leak group-kill family).

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

## What to scale (and what not to)

Scale via `D(base)` **every duration that competes with the boundary being
tested**: ctx deadlines, kill graces (short and long), elapsed upper bounds,
helper wedge/failure sleeps, and spawn/reap liveness waits. Do **not** scale the
20ms poll intervals — they are sub-boundary sampling granularity, not a boundary,
and keeping them fixed keeps polling responsive at any factor. Do not touch
non-boundary helper modes that already carry a large fixed margin and assert no
wall-clock upper bound (e.g. claudecode's `slow`/`slow_brief`), nor any
production duration.

## Env inheritance invariant

The three test-helper re-exec builders (procgroup, claudecode, codex) all append
their helper env to `os.Environ()`, so `FISHHAWK_TEST_TIME_SCALE` and `CI`
propagate to the fake-CLI and grandchild processes — a driving test and its
helpers therefore compute the SAME factor. That is what keeps a scaled wedge
sleep above the scaled elapsed bound; a factor mismatch would shrink the
wedge-above-bound margin and could spuriously fail the test.
