---
name: sync-main
description: Safely bring the local Fishhawk checkout and stack up to date after a PR merges — pre-pull safety checks for live runs and decompositions, `scripts/dev post-merge`, and confirming the rebuilt binaries are live. Use when the user says a PR merged, asks to "pull main", "sync", "update local", or "run post-merge".
disable-model-invocation: true
---

# Sync main after a merge

`scripts/dev post-merge [<issue>] [--start-deps]` does the whole walk:

1. `git pull --ff-only origin main`
2. `scripts/cleanup-merged`
3. an optional issue-closed check
4. `reload`, which rebuilds all five binaries, restarts fishhawkd and gates on `/healthz`

The command is the easy part. The judgment is in **whether it is safe to pull and restart right now**.

## Operator-only: stop if you are a run agent

This skill stops services or deletes state, and its confirmation steps need a human. Before anything else:

```sh
"$(git rev-parse --show-toplevel)/scripts/is-run-agent"
```

If it exits non-zero (it prints `run-agent: <reason>`), you are an agent inside a Fishhawk run: do nothing, and report that this skill is operator-only. `scripts/is-run-agent` is the one shared definition of a run agent: it matches the runner's lineage, acceptance and conflict-resolution trees, plus `FISHHAWK_RUN_ID` (CI runners).

## 1. Is anything live? (stop and ask if yes)

```sh
pgrep -fl '[f]ishhawk-runner .*--run-id' || echo no-live-runner
git branch --show-current; git status --short
```

If the fishhawk MCP tools are available, also run `fishhawk_list_runs` and look for runs in a non-terminal state whose `working_dir` is this checkout. Each of these makes a pull or restart harmful:

| Live state | What a pull / restart breaks |
|---|---|
| A runner mid-stage | Restarting fishhawkd can strand its stage in `running` (recoverable only via the REST `reap-failure` endpoint). `post-merge` refuses on its own; `--force` needs the user's explicit OK |
| A decomposed parent with children not yet dispatched | Advancing `main` makes each later child fail `working_dir_diverged_from_base` |
| An implement/plan review round in flight | `post-merge` orphans it, and there is no re-dispatch verb |
| The user is awaiting (`fishhawk_await_*`) on another run | The restart kills the await with `connection refused` |

If any apply, report what's live and **hold**. Offer to do the safe parts now (delete stale local copies, prune branches) and pull later.

## 2. Clear files the pull would refuse

A pull can refuse when a local, previously-ignored or untracked file sits at a path the merge now tracks (e.g. a hand-copied `.claude/settings.json`). For each such path, compare it with the merged version and delete it only if identical:

```sh
git fetch -q origin main
git show origin/main:<path> | cmp -s - <path> && echo identical
```

If the file differs, show the diff and ask. Never discard local edits silently.

## 3. Pull and reload

Requires: on `main`, with a clean tree.

```sh
scripts/dev post-merge <issue-number-if-known> --start-deps
```

- A diverged local `main` fails loudly by design. Don't resolve it with a merge commit; show `git log --oneline origin/main..main` and ask.
- `post-merge` runs the **old** `scripts/dev`. If the merge changed `scripts/dev` itself, the change applies from the next invocation.

## 4. Confirm the new code is live

- **fishhawkd:** `/healthz` `git_sha` equals `git rev-parse --short HEAD`.
- **fishhawk-runner:** nothing to do; it is spawned fresh from `bin/` per stage.
- **fishhawk-mcp:** relay the closing banner verbatim.
  - `auto-swap` / `schema_major_shim`: this is an expectation, not a confirmed swap. Confirm with `fishhawk_doctor` or a version-returning MCP call showing the new GitSHA. If it is stale, run `bin/fishhawk-mcp-shim --status`, then the user reconnects the MCP client.
  - `ACTION REQUIRED` / shim rebuilt: the user must reconnect their MCP client (`/mcp` in Claude Code).

Report: the new HEAD, which branches were pruned, the healthz SHA, and the banner. Flag anything you held back.
