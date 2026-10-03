---
name: deploy-local
description: Deploy (bring up, rebuild, restart, or tear down) the Fishhawk stack on this machine — Postgres + RustFS containers, the five Go binaries, migrations, and fishhawkd on :8080 — or the Helm chart on Docker Desktop Kubernetes. Use when asked to deploy/run/start/restart the stack locally, "bring fishhawkd up", "reload the backend", "deploy to local k8s", or tear the local stack down.
---

# Deploy Fishhawk locally

`scripts/dev` owns the whole bring-up. Do not hand-roll `docker compose` + `go build` + `fishhawkd serve` — `scripts/dev` adds the port preflight, GitSHA stamping, migrations, the `/healthz` nonce identity gate, and the MCP-shim banner. Run everything from the repo root (main checkout, not a `.claude/worktrees/*` checkout unless the user asks).

## 1. Pick the target

| User intent | Command |
|---|---|
| Default — "deploy locally", "start the stack" | `scripts/dev up --start-deps` |
| Rebuild everything + restart (after a pull / code change) | `scripts/dev reload --start-deps` |
| Full post-merge walk (pull main, prune branches, reload) | `scripts/dev post-merge [<issue>] --start-deps` |
| Kubernetes (Docker Desktop) — "deploy to k8s", "helm" | `scripts/dev k8s` |
| Web UI dev server too | additionally `make dev-frontend` (`:5173`, proxies `/v0` → `:8080`), run in background |
| Tear down | Use the `teardown-local` skill (ordered stop of every layer; `make nuke` only on explicit request) |

If the intent is ambiguous between process and k8s, use the process mode (`up`) — it is the daily dev loop. Never run `make nuke` (drops volumes) unless the user explicitly asks to destroy data.

Plain `up` is a **no-op when fishhawkd is already running** — it prints `fishhawkd already running` and does not rebuild. To pick up code changes, use `reload`.

## 2. Preflight (run before `up`/`reload`/`post-merge`/`k8s`)

```sh
docker info >/dev/null 2>&1 && echo docker-ok || echo docker-DOWN
test -f .env && echo env-ok || echo env-MISSING
pgrep -fl 'fishhawk-runner .*--run-id' || echo no-live-runner
git status --short | head
uptime
```

- **Docker down** → `open -a Docker`, then poll until `docker info` succeeds (a bounded until-loop, not one long `sleep`).
- **`.env` missing** → `cp .env.example .env`; `FISHHAWKD_DATABASE_URL` already matches `docker-compose.yml`. Tell the user which optional blocks (GitHub App, OAuth) are unset; fishhawkd starts without them but logs warnings. Never print secret values from `.env`.
- **Live runner** → `reload`/`post-merge`/`down` restart fishhawkd and can strand that run's stage in `running`. `reload`/`post-merge` refuse on their own; STOP and ask the user before passing `--force`. Same if the user has an in-flight `fishhawk_await_*` on another run.
- **Dirty tree** → fine for `up`/`reload` (binaries get stamped `-dirty`). `post-merge` does `git pull --ff-only` on main — confirm the user is on a clean `main` first.
- **Load average far above core count** → warn the user; a starved host makes the readiness gate flaky (orphaned agent busy-loops, see AGENTS.md Traps).

## 3. Deploy

Run the chosen command with a generous timeout: the first build of five binaries can take a few minutes, and `k8s` builds an image. If your shell tool caps command duration below ~10 minutes, run it in the background and wait for it to exit.

Success markers in the output:
- `postgres: ready|running` and `rustfs: ready|running`
- `rebuilt N of 5 binaries: …`
- `listener identity nonce-verified`
- `fishhawkd started (pid N) — logs: logs/fishhawkd.log`
- k8s: `/healthz` gate + image-identity gate pass

Then verify independently:

```sh
curl -fsS "http://${FISHHAWKD_ADDR:-localhost:8080}/healthz" | python3 -m json.tool
```

(Read `FISHHAWKD_ADDR` from `.env` if set — e.g. `127.0.0.1:8080`.) Confirm `git_sha` matches `git rev-parse --short HEAD` (with `-dirty` on a dirty tree).

## 4. Report

One short block: mode, URL, pid, `git_sha`, which binaries rebuilt, and **the MCP banner verbatim if one printed** — it is the only thing the user must act on:
- `ACTION REQUIRED` / shim-rebuilt banner → the user must reconnect their MCP client (`/mcp` in Claude Code).
- auto-swap / `schema_major_shim` → expectation only; verify with `fishhawk_doctor` (`spec.valid: true`) or a version-returning tool reflecting the new GitSHA. If stale: `bin/fishhawk-mcp-shim --status`, then reconnect the MCP client.
- `fishhawk-runner` needs nothing — it is spawned fresh from `bin/` per stage.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `port … in use by pid N` | `scripts/dev down`; if a foreign process holds it, report it — don't kill non-fishhawkd processes without asking |
| `did not become healthy within 10s` | Read the printed log tail / `tail -50 logs/fishhawkd.log`; usually a migration or config error |
| `postgres did not become ready` | `docker logs fishhawk-postgres` |
| `Operation not permitted` on repo read | Grant the app hosting the agent (terminal, Claude Code, Codex) Full Disk Access in System Settings → Privacy & Security, then restart it |
| Build fails only under a newer local Go | `go env -w GOTOOLCHAIN=go1.25.6` (AGENTS.md Traps, #3237) |
| k8s: `STALE fishhawkd image` / identity mismatch | See `docs/deploy/kubernetes.md` § "Image identity"; `FISHHAWK_K8S_SKIP_IDENTITY=1` only with the user's OK |
| k8s: `x509: certificate signed by unknown authority` in `docker build` | TLS-inspecting proxy — `docs/deploy/kubernetes.md` |
| `.git` pack `Operation timed out` | Repo is in iCloud `~/Documents`; rehydrate the evicted file and retry |

## References

- `scripts/dev` (`_usage` for every subcommand), `scripts/README.md`
- `AGENTS.md` § Rebuild matrix (rebuild + activation tables, the short rules) and § Traps
- `scripts/README.md` § "`scripts/dev` lifecycle" (readiness nonce gate, MCP banner, schema-major banner, `sweep`, ZERR trap), § "Live-run guard for reload / post-merge", § "Local k8s ergonomics"
- `docs/deploy/kubernetes.md`, `docs/local-tls.md` (`FISHHAWK_DEV_TLS=1`), `docs/local-webhook-relay.md` (`FISHHAWK_DEV_WEBHOOK_RELAY=1`)
