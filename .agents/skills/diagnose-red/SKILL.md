---
name: diagnose-red
description: Triage a Fishhawk verify/test failure that looks unrelated to the change — red on clean main, flaky under -race, or failing only on this host. Walks the known environmental causes (host load and orphaned agent processes, Go toolchain drift, golangci-lint version/cache, Docker/testcontainers state, iCloud eviction, timing flakes, vanished images) before anyone edits code. Use when verify fails on tests the diff didn't touch or the user asks "why is main red locally".
---

# Diagnose an unexplained red

Rule: **prove it's the code before changing the code.** First establish whether `origin/main` itself is red on this host. If it is, the cause is environmental. Work through the list in order; cheap checks come first.

## 1. Host load and orphaned processes (check this first)

```sh
uptime; sysctl -n hw.ncpu
ps -axo pid=,ppid=,pcpu=,etime=,comm= | sort -k3 -nr | head -15
```

- **Signal:** a load average far above the core count, with the top consumers being `sh`/`zsh`/`go` processes whose **ppid is 1** and that no live run owns. These are orphans leaked by an agent stage (one past incident hit load ~140 on 10 cores).
- **What the runner does:** it reports `host_overloaded:` (category C) when a verify fails above 4× cores.
- **Action:** list the orphans to the user and kill them only with their OK. Then re-run.

## 2. Go toolchain drift

`go env GOVERSION` newer than the `go 1.25.0` pin can turn clean main red. For example, Go 1.27's jsonv2 breaks `TestJSONEncodedLen_MatchesEncoder_Differential`.

- **Signal:** green in CI, and green with `GOTOOLCHAIN=go1.25.6 scripts/test single -run <Test> ./<pkg>/`.
- **Fix:** propose `go env -w GOTOOLCHAIN=go1.25.6`; it's a global write, so ask first.

## 3. golangci-lint

- **Version:** local must be v2.x. CI pins v2.8.0, and a newer local binary formats differently. Check with `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.8.0 fmt --diff`.
- **Cross-worktree cache pollution:** a finding naming a path in a *different* worktree means a stale cache. Fix with `golangci-lint cache clean`, then re-run.
- **Concurrency:** two concurrent lint/verify runs contend on golangci-lint's global lock. Check for another `scripts/test` process.

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

The repo lives under `~/Documents`, which iCloud syncs.

- **`Operation timed out` reading a `.git/objects/pack/*` file:** the file is evicted (dataless). Rehydrate it with `brctl download <path>` (or read it once), then retry.
- **Stray `* 2.*` / `* 2` duplicates** (files or dirs) appearing as untracked or out-of-scope are iCloud conflict copies. List them with `git status --short | grep ' 2'` and show the user; don't bulk-delete.

## 6. Timing / ordering flakes

- **Reproduce:** `scripts/test single -run '^TestX$' -count=10 ./pkg/` (add `-race`).
- **Load sensitivity:** a pass when idle and a failure under load means a tight deadline. The repo's rule is `backend/internal/timescale` `D(base)` for every deadline-competing duration. `FISHHAWK_TEST_TIME_SCALE=5` simulates CI's factor.
- **Known flake shapes** (see `AGENTS.md` Traps):
  - a Go `time.Now()` compared against a DB `now()` (cross-clock)
  - waiting on one sink but asserting on another
  - a must-complete invocation racing a tight timeout
  - a racy test fake under fan-out

## 7. Verify lock

`refused`/lock-held messages mean another verify (often the runner's) is running. That isn't a test failure. Don't retry in a loop; use `scripts/test single` or wait.

## Report

Name the cause you **verified** (with the evidence line), the fix applied or proposed, and whether `origin/main` was red on this host. If nothing environmental explains it, say so plainly: the failure is likely real, so hand it back for a code fix.
