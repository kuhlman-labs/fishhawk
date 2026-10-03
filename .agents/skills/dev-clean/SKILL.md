---
name: dev-clean
description: Reclaim disk and clear accumulated local litter in a Fishhawk checkout without stopping the stack — merged operator worktrees, aged run worktrees and /tmp sidecars, merged branches, leaked testcontainers and dangling volumes, stale MCP shims, Go/lint caches, logs. Use when asked to "clean up", "free disk space", "prune worktrees/branches", or when the machine is accumulating leftovers. To STOP services, use teardown-local instead.
disable-model-invocation: true
---

# Clean up local dev litter

Everything here is **list first, confirm, then remove**. Show the user the candidate list and sizes before deleting anything. Never touch `.env`, `.claude/settings.local.json`, the main checkout, or a dirty worktree.

## Operator-only: stop if you are a run agent

This skill stops services or deletes state, and its confirmation steps need a human. Before anything else:

```sh
case "$(git rev-parse --show-toplevel 2>/dev/null)" in
  */fishhawk-worktrees/run-*|*/fishhawk-acceptance-tree-*) echo RUN-AGENT ;;
esac
if [ -n "${FISHHAWK_RUN_ID:-}" ]; then echo RUN-AGENT; fi
```

If it prints `RUN-AGENT`, you are an agent inside a Fishhawk run: do nothing, and report that this skill is operator-only. The path match is the primary signal, since local runs run in those trees. `FISHHAWK_RUN_ID` is set only by CI-hosted runners (GitLab CI, deploy triggers), not by the local runner.

## 0. Is anything live?

```sh
pgrep -fl '[f]ishhawk-runner .*--run-id' || echo no-live-runner
pgrep -fl '[s]cripts/test' || echo no-scripts-test
```

With a live runner, skip run-worktree pruning and container removal. Those belong to the run.

## 1. Inventory (read-only)

```sh
du -sh .claude/worktrees .git/fishhawk-worktrees logs .fishhawk/cache 2>/dev/null
git worktree list | wc -l
docker ps -a --filter label=org.testcontainers=true --format '{{.Names}} {{.Status}}'
docker volume ls -q --filter dangling=true --filter label=com.docker.volume.anonymous | wc -l
bin/fishhawk-mcp-shim --status --stale-only
```

## 2. Worktrees, run litter, merged branches

```sh
scripts/dev sweep --dry-run          # lists candidates, removes nothing
scripts/dev sweep --yes              # only after the user approves the list
```

What `sweep` removes:

- merged-clean operator worktrees under `.claude/worktrees/` (never dirty ones, never the current one)
- run worktrees under `.git/fishhawk-worktrees/run-*` older than `--days N` (default 7), only when `git status` proves them clean
- keyed `/tmp/fishhawk-*` sidecars

It then runs `scripts/cleanup-merged` (deletes local branches already merged into `origin/main`). Follow with `git worktree prune` for entries whose directories are already gone.

## 3. Docker litter

- **Leaked testcontainers.** The `org.testcontainers=true` label is shared by every testcontainers user on this Docker daemon (other repositories, IDE test runs), not just Fishhawk.
  - Remove only `fishhawk-test-postgres`, and only when no `scripts/test` is running: `docker rm -f -v fishhawk-test-postgres`. `-v` takes only that container's anonymous volumes.
  - For any other labelled container, list it with its image and age, and remove it only after the user confirms that specific container.
- **Dangling anonymous volumes.** `scripts/test` warns above 100. The cleanup command is `docker volume prune -f`, but it is **daemon-wide**: it deletes every unused anonymous volume on the machine, not just Fishhawk's. Say so explicitly and get a yes before running it.
- **Compose named volumes** (`fishhawk_fishhawk-postgres-data`, `fishhawk_fishhawk-rustfs-data` from the main checkout; compose prefixes the project name) hold the dev database and traces. Leave them alone; destroying them is the `teardown-local` skill's destroy-data step (`make nuke`), and only on explicit request.

## 4. Caches and logs

| Item | Command | Note |
|---|---|---|
| Go test cache | `go clean -testcache` | Safe; next run is slower |
| golangci-lint cache | `golangci-lint cache clean` | Also fixes cross-worktree cache pollution |
| Go build cache | `go clean -cache` | Large; next build is slow. Ask first |
| `logs/` | truncate or remove old `*.log` | Not while fishhawkd is running (it holds `logs/fishhawkd.log`) |
| Preview worktree in `.fishhawk/cache/` | `scripts/dev preview-down` | Removes the acceptance preview build |

## 5. Stale MCP shim

If `--status --stale-only` printed anything, the shim is serving an old child binary. Report the recorded reason (`deferred_in_flight`, `deferred_no_initialize_recorded`, …). The fix is the user reconnecting their MCP client; don't kill the shim, because the harness owns it.

## 6. iCloud duplicates

Conflict copies named `* 2*` show as untracked files or directories. List them (`git status --short | grep ' 2'`) for the user to confirm before removing them.

## Report

What was removed and how much space it freed (`du` before/after). Also list what was skipped and why: live run, dirty worktree, or user declined.
