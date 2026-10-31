# Tests and cross-module contracts

Read before adding or changing tests, fixtures or duplicated wire types. These
rules condense the former AGENTS.md test traps; linked package docs own details.

## Database and environment fixtures

- Use `pgtest.NewPool(t)` / `NewURL(t)`: one shared Postgres and per-test template
  clones. Do not create a testcontainer per package. The raw migration-test
  exception must terminate each container in Cleanup, including start failures.
- Runner gate containers have no Docker daemon. With `FISHHAWK_GATE_CONTAINER=1`,
  use the runner-provided `FISHHAWK_TEST_PG_URL`; missing/broken service is a
  failure, never a skip. Raw migration fixtures use throwaway databases there.
  See [the shared-container contract](../../scripts/README.md).
- `t.Parallel()` cannot coexist with `t.Setenv` or `t.Chdir`. Parallel Postgres
  and integration/MCP test files need separate non-parallel files for such tests.
- `WithWaitStrategy` replaces defaults; use `WithAdditionalWaitStrategy` and
  retain a port-aware wait, so Docker Desktop's asynchronous mapping is ready
  before ConnectionString/Host/MappedPort (#2948).
- Fixture paths use init-captured `pkgSrcDir` / `mustGetwd()`, not runtime.Caller
  (module-relative under `-trimpath`). [testanchor](../../backend/internal/testanchor/README.md)
  enforces this (#4179).

## Timing and concurrency

- Every deadline-competing duration uses `timescale.D(base)`: context deadlines,
  kill grace, elapsed upper bounds, wedge/failure sleeps and spawn/reap liveness
  waits. Keep discrimination ratios constant. Ordinary 20ms polling intervals
  stay unscaled. CI scales 5×; the positive integer override is 1..1000.
  [timescale](../../backend/internal/timescale/README.md), #1984.
- Compare timestamps in one clock domain. For a Go seed compared against a
  DB-generated timestamp, read the database timestamp back and offset from it;
  scaling margins cannot bound host/container clock skew. Do not churn seeds
  that are never compared across clocks (#3048).
- A fake used by concurrent fan-out must protect its bookkeeping with a mutex;
  a race in the fake is not evidence that production synchronization is wrong
  (#3226/#2586).
- Wait on the sink you assert on. A published snapshot does not order a later
  log write; awaiting one then asserting the other races (#3004).
- A must-complete subprocess fixture races spawn latency under a tight deadline.
  Use synchronization/readiness and enough completion margin; timeout behavior
  tests must still discriminate the deliberately wedged case (#3587).

## Git fixtures

`scripts/test` pins GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM to /dev/null so
fixtures do not inherit host signing. Gate subprocesses pin them too. A direct
fixture helper must not accidentally reintroduce global signing (#912/#3102).

Bare-origin pushes can spawn detached maintenance that outlives a test and races
temp-directory cleanup. Pin maintenance.auto=false via a fixture-owned GLOBAL
config, since file transport scrubs repo-local config overrides. A new package
that pushes into bare temp repos needs the same TestMain guard as gitops/runner/
CLI. Per-test GIT_CONFIG_GLOBAL changes must retain that pin (#3503/#3507).

URL pushes can create remote-tracking refs under newer git. Use
`gitops.urlPushNoTrackingArgs` for fixtures simulating runner pushes; reproduce
with the documented container recipe. [runner/README.md](../../runner/README.md)
owns both contracts (#4767).

## Frontend fixtures

Use `frontend-check`. jsdom must use the configured HTTPS localhost URL for
`__Host-` cookie semantics. Do not undo `vite-fs-deny.ts`'s narrow `.git`-nested
worktree exception: stock Vite denies every file in those run checkouts and
misreports missing modules. [frontend/README.md](../../frontend/README.md), #3030.

## Cross-module wire types

A duplicated marshaled/decoded struct pair must carry `CROSS-MODULE WIRE CONTRACT`
markers on both ends and a `SeedManifest()` Pair in
`backend/internal/wirecontract/manifest.go`. Use ModeExact for round-trip leaves,
ModeSubset for a decoded subset; subset tolerates omitempty, not arbitrary tag
options. A marked swept struct needs a manifest entry or explicit unpaired
exemption. Share duplicated goldens through `testdata/wire/*.json` rather than
independent constants. [wirecontract](../../backend/internal/wirecontract/README.md), #2558.
