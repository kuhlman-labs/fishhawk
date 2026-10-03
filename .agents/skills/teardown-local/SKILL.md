---
name: teardown-local
description: Tear down the local Fishhawk dev environment — stop fishhawkd (and its TLS proxy / webhook relay), the acceptance preview, the Web UI dev server, the Docker Desktop k8s release, and the compose containers (Postgres, RustFS, Jaeger) — optionally destroying the dev data volumes. Use when asked to stop/shut down/tear down/turn off the local stack or free its ports. For disk cleanup without stopping services, use dev-clean.
disable-model-invocation: true
---

# Tear down the local stack

Teardown is ordered from the top of the stack to the bottom: clients, then the daemon, then the cluster, then the containers. The data volumes survive unless the user explicitly asks to destroy data.

## Operator-only: stop if you are a run agent

This skill stops services or deletes state, and its confirmation steps need a human. Before anything else:

```sh
case "$(git rev-parse --show-toplevel 2>/dev/null)" in
  */fishhawk-worktrees/run-*|*/fishhawk-acceptance-tree-*) echo RUN-AGENT ;;
esac
if [ -n "${FISHHAWK_RUN_ID:-}" ]; then echo RUN-AGENT; fi
```

If it prints `RUN-AGENT`, you are an agent inside a Fishhawk run: do nothing, and report that this skill is operator-only. The path match is the primary signal, since local runs run in those trees. `FISHHAWK_RUN_ID` is set only by CI-hosted runners (GitLab CI, deploy triggers), not by the local runner.

## 1. Check what's live, and stop if a run is mid-flight

```sh
pgrep -fl '[f]ishhawk-runner .*--run-id' || echo no-live-runner
pgrep -fl '[s]cripts/test' || echo no-scripts-test
lsof -nP -iTCP:8080,8090,8443,5173 -sTCP:LISTEN 2>/dev/null
docker ps --filter name=fishhawk --format '{{.Names}} {{.Status}}'
kubectl config current-context 2>/dev/null && helm status fishhawk 2>/dev/null | head -3
```

- **A live runner.** Stopping fishhawkd strands its stage in `running` (recoverable only via the REST `reap-failure` endpoint). Name the run and **ask** before continuing.
- **A running `scripts/test`.** Stopping Postgres or Docker under it reds that test run. Ask, or wait.

## 2. Stop, top-down

Skip any layer that isn't running.

1. **Web UI dev server** (`make dev-frontend`, vite on `:5173`): stop the background task that started it. Otherwise kill the listener PID from `lsof`, after confirming it's `node`/vite in this repo.
2. **Acceptance preview:** `scripts/dev preview-down`. It kills the preview fishhawkd on `:8090` and removes its worktree.
3. **fishhawkd + TLS proxy + webhook relay:** `scripts/dev down`.
   - It stops the caddy proxy and the smee relay if enabled, SIGTERMs fishhawkd (SIGKILL after 5s), and cleans pid/nonce files.
   - Then it checks the port. A fishhawkd squatting with a matching nonce is killed. A **foreign** process on the port is reported, never killed, and `down` exits 1. Relay that message; don't kill it yourself.
4. **Kubernetes** (only if a `fishhawk` helm release exists): `scripts/dev k8s-down`. It kills the port-forwards and runs `helm uninstall fishhawk` against **whatever cluster the current kubectl context points at**, with no check that it is local. So first confirm both of these:
   - `kubectl config current-context` prints `docker-desktop`.
   - `kubectl get nodes -o name` lists only `node/docker-desktop`, or only kind-style `node/<name>-control-plane` / `-worker` nodes. This is the same classification `scripts/dev k8s` uses.

   If either check fails, **stop**: report the context and the release, and ask. Never `helm uninstall` on a context that might be shared or remote.
5. **Compose containers:** `make down` (`docker compose down`). This stops Postgres, RustFS and Jaeger and **keeps** the named data volumes. Compose prefixes them with the project name, which defaults to the checkout directory's name: from the main checkout, `docker volume ls` shows `fishhawk_fishhawk-postgres-data` and `fishhawk_fishhawk-rustfs-data`. **Run every compose command from the main checkout.** From a differently named checkout (e.g. a `.claude/worktrees/<x>` worktree), compose targets a different project's containers and volumes. Jaeger sits behind the `otel` profile; if it's still up, run `docker compose --profile otel down`.
6. **Shared test Postgres** (`fishhawk-test-postgres`, usually already reaped by `scripts/test`): if it's still present and no `scripts/test` is running, run `docker rm -f -v fishhawk-test-postgres`.

## 3. Destroy data (only on explicit request)

Run this only if the user asked to wipe/reset/destroy data, and after restating that it permanently deletes the local dev database and stored traces:

```sh
make nuke        # docker compose down -v: removes fishhawk_fishhawk-postgres-data and fishhawk_fishhawk-rustfs-data
```

Run it from the main checkout only. From any other checkout directory, compose targets a different project's volumes.

The next `scripts/dev up --start-deps` recreates empty volumes and re-runs migrations.

Never run `docker volume prune`, `docker system prune`, or quit Docker Desktop as part of teardown. Those reach beyond Fishhawk.

## 4. Confirm it's down

```sh
lsof -nP -iTCP:8080,8090,8443,5173 -sTCP:LISTEN 2>/dev/null || echo ports-free
docker ps --filter name=fishhawk --format '{{.Names}}' | grep . || echo no-fishhawk-containers
```

## Report

List what was stopped, what was already down, anything left running and why (e.g. a foreign process on `:8080`), and whether the data volumes were kept or destroyed. Bring it back with the `deploy-local` skill.
