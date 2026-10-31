---
name: diagnose-red
description: Diagnose Fishhawk verify/test failures unrelated to the diff, flaky under race detection, or failing only on this host.
---

# Diagnose an unexplained red

Rule: **prove it's the code before changing the code.** First establish whether `origin/main` itself is red on this host. A failure there establishes that the diff did not introduce it; it can still be a real baseline defect. Work through the list in order; cheap checks come first.

## 1. Host load and orphaned processes (check this first)

```sh
uptime; sysctl -n hw.ncpu
ps -axo pid=,ppid=,pcpu=,etime=,comm= | sort -k3 -nr | head -15
```

- **Signal:** a load average far above the core count, with the top consumers being `sh`/`zsh`/`go` processes whose **ppid is 1** and that no live run owns. These are orphans leaked by an agent stage (one past incident hit load ~140 on 10 cores).
- **What the runner does:** it reports `host_overloaded:` (category C) when a verify fails above 4× cores.
- **Action:** list the orphans to the user and kill them only with their OK. Then re-run.

## 2. Go toolchain drift

Compare `go env GOVERSION` with the checked-in `deploy/gate-image/Dockerfile`
pin. A newer toolchain can change semantics (for example jsonv2). Reproduce the
failing test under the pinned toolchain before attributing the difference to
code. Prefer a per-command override; a persistent `go env -w` change requires
authorization. Host Go settings do not repair the baked-in container toolchain.

## 3. golangci-lint

- Run `scripts/check-gate-image --host`. The lint version, its build Go and standalone gofmt must match the checker's pins. Use pinned tools on PATH or the gate image; see [verification](../../../docs/agents/verification.md).
- A finding naming another worktree can be stale lint cache evidence. Check active lint users before clearing the shared cache, then reproduce.
- Lint invocations serialize on golangci-lint's global lock. A verify-lock refusal remains terminal: use a focused test, not a retry loop.

## 4. Docker / testcontainers

```sh
docker info >/dev/null && echo ok
docker ps -a --filter label=org.testcontainers=true --format '{{.ID}} {{.Names}} {{.Status}}'
pgrep -fl '[s]cripts/test' || echo no-scripts-test
```

- **"context deadline exceeded" / "No such container" on Postgres start:**
  - With no `scripts/test` running, a wedged `fishhawk-test-postgres` can be removed: `docker rm -f -v fishhawk-test-postgres`. `-v` removes only that container's own anonymous volumes.
  - Never `docker volume rm`/`prune` here.
- **Constrained daemon:** retry with `FISHHAWK_TEST_P=2 scripts/test`.
- **An image pull failing (`manifest unknown`, `repository does not exist`):** a pinned image may have vanished upstream. Check with `docker manifest inspect <image:tag>`.

## 5. iCloud-synced repo

Check the actual checkout path first. This section applies only when the checkout is in an iCloud-synced directory such as `~/Documents`.

- **`Operation timed out` reading a `.git/objects/pack/*` file:** the file is evicted (dataless). Rehydrate it with `brctl download <path>` (or read it once), then retry.
- **Stray `* 2.*` / `* 2` duplicates** (files or dirs) appearing as untracked or out-of-scope are iCloud conflict copies. List them with `git status --short | grep ' 2'` and show the user; don't bulk-delete.

## 6. Timing / ordering flakes

- **Reproduce:** `scripts/test single -run '^TestX$' -count=10 ./pkg/` (add `-race`).
- **Load sensitivity:** a pass when idle and failure under load suggests a timing or resource dependency; inspect the failed assertion. The repo's rule is `backend/internal/timescale` `D(base)` for every deadline-competing duration. `FISHHAWK_TEST_TIME_SCALE=5` simulates CI's factor.
- **Known flake shapes** (see [testing rules](../../../docs/agents/testing.md)):
  - a Go `time.Now()` compared against a DB `now()` (cross-clock)
  - waiting on one sink but asserting on another
  - a must-complete invocation racing a tight timeout
  - a racy test fake under fan-out

## 7. Verify lock

`refused`/lock-held messages mean another verify (often the runner's) is running. That isn't a test failure. Don't retry in a loop; use `scripts/test single` or wait.

## Report

Name the cause you **verified** (with the evidence line), the fix applied or proposed, and whether `origin/main` was red on this host. If nothing environmental explains it, say so plainly: the failure is likely real, so hand it back for a code fix.
