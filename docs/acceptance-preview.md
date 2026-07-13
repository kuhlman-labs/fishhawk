# Acceptance preview provisioning — hook contract

Build-system-agnostic reference for the acceptance-stage preview provisioning hooks (E31.18 / #1569, E36.2 / #1640). The dogfood loop wires these to `scripts/dev preview`; this document specifies the contract independent of any build system, so an operator running a non-fishhawk stack can provision a verifiable preview instance.

Scope: this is runner-deployment configuration, deliberately **not** part of the workflow spec surface (`.fishhawk/workflows.yaml`). The hooks are read from the runner-process environment. The runner-internal narrative lives in `runner/README.md` ("Acceptance target-identity gate + preview provisioning") and `docs/ARCHITECTURE.md` §7/§10; this file is the external-operator contract.

## Why the gate exists

The acceptance stage validates a **running instance** at the first spec-declared `egress.target_hosts` entry. Without provisioning, that is whatever build happens to answer there — typically the orchestrating deployment on current `main`, not the run's merge candidate. Before the acceptance agent spawns, the runner provisions the preview (optional), waits for it to become ready, and verifies its build identity against the merge-candidate head SHA. A wrong or unverifiable build fails the stage **pre-spawn** rather than validating the wrong code.

## The two hooks

Both hooks run via `sh -c` in the operator's dispatch `working_dir` — the checkout the run was dispatched from (e.g. the one carrying the untracked `.env`) — falling back to the runner's current working directory when no `working_dir` was dispatched. Anchoring to the dispatch checkout means a **relative** provision command like `scripts/dev preview` resolves the operator's checkout even when the driving session launched from a git worktree, rather than the runner-inherited process cwd (#1746).

| Env var | Role | When it runs |
|---|---|---|
| `FISHHAWK_ACCEPTANCE_PREVIEW_CMD` | **provision** — build and serve the merge candidate | once, before the identity gate |
| `FISHHAWK_ACCEPTANCE_PREVIEW_TEARDOWN_CMD` | **teardown** — stop and remove the preview instance | deferred; on **every** post-provision return |

The teardown hook is deferred the moment it is configured, so it runs on the happy path (after the verdict ships) **and** on every pre-spawn failure that occurs after provisioning began (readiness timeout, stale/unreachable target, any gate failure). It is best-effort: a non-zero teardown exit logs `acceptance_preview_teardown_failed` and never changes the stage outcome.

### Injected environment

At call time the runner adds two variables to each hook's environment:

| Injected var | Value |
|---|---|
| `FISHHAWK_PREVIEW_SHA` | the expected merge-candidate head SHA (the identity the preview must serve) |
| `FISHHAWK_PREVIEW_TARGET_HOST` | the first declared `egress.target_hosts` entry (host or `host:port`, scheme-less) |

The hook runs under the credential-stripped `sanitizedGateEnv` allow-list (ADR-029 item 4, shared with the compile/test/verify gates): `PATH`, `HOME`, and `GO*` survive so a Go build works, but the runner's secrets do **not** reach the hook. `FISHHAWK_GITHUB_TOKEN` / `GITHUB_TOKEN` / `GH_TOKEN`, `ANTHROPIC_API_KEY` / `OPENAI_API_KEY`, and `FISHHAWK_API_TOKEN` are all absent. The provision hook builds and runs untrusted, committed, agent-authored merge-candidate code before the ADR-050 acceptance egress proxy contains anything, so it must never inherit the runner's credentials.

## Timeouts

| Env var | Default | Governs |
|---|---|---|
| `FISHHAWK_ACCEPTANCE_PREVIEW_TIMEOUT_SECS` | 300 | per-command budget for **both** the provision and teardown hooks (the provision command typically includes a build) |
| `FISHHAWK_ACCEPTANCE_PREVIEW_READY_TIMEOUT_SECS` | 60 | post-provision readiness-poll budget |

Each accepts a positive integer number of seconds; an unset, unparsable, or non-positive value falls back to the default.

## Readiness contract

After a successful provision, the runner polls `<scheme>://<host>/healthz` every 2 seconds until the served build identity is **verified** or the ready budget expires. Scheme order is http-first for loopback and IP-literal hosts, https-first otherwise, always falling back to the other scheme. The `/healthz` response must be `200` with a JSON body carrying a `git_sha` field:

- **verified** — `git_sha` is a ≥7-character prefix of `FISHHAWK_PREVIEW_SHA`. The gate proceeds and the agent spawns.
- A `-dirty`-suffixed `git_sha` is treated as **stale** (fail-closed): a dirty build is not the committed merge candidate, even when the prefix matches.

**The target MUST expose `git_sha` on `/healthz` to be verifiable.** A target that answers but exposes no build identifier (missing/`unknown`/too-short `git_sha`, non-200, or non-JSON) is classified **unverifiable** → the runner warns and proceeds (mixed-version compatibility posture — a missing identifier is never a hard fail). If you want acceptance to actually gate on identity, serve `git_sha`.

Without a provision command the gate is single-shot against a fixed instance: only connection failures are retried (3 quick attempts absorb a blip), and a definitive stale/unverifiable answer gates immediately.

## Exit-code semantics

| Path | Outcome |
|---|---|
| provision exits non-zero **or** times out | stage fails **pre-spawn**, category C, reason `acceptance_preview_provision_failed` (exit state + a bounded output tail in the detail) |
| provision succeeds, target verified | agent spawns |
| provision succeeds, target stale | stage fails pre-spawn, category C, reason `acceptance_target_stale` (expected-vs-got in the detail); teardown runs |
| provision succeeds, target never ready | stage fails pre-spawn, category C, reason `acceptance_target_unreachable` (`not ready within <budget>` in the detail); teardown runs |
| provision succeeds, target unverifiable | warn `acceptance_target_unverified`, agent spawns |
| teardown exits non-zero | logged `acceptance_preview_teardown_failed`; **stage outcome unchanged** |

A category-C failure is a pre-spawn infrastructure failure: the acceptance agent never runs, and no verdict ships.

## Event vocabulary

The runner logs these JSON events (one per line) to its log sink:

| Event | Meaning |
|---|---|
| `acceptance_preview_provisioned` | provision command succeeded; carries `expected_sha` |
| `acceptance_target_verified` | target serves the expected build; carries the observed `git_sha` |
| `acceptance_target_unverified` | target reachable but identity not comparable (proceeds); carries a `reason` |
| `acceptance_preview_provision_failed` | provision hook non-zero exit / timeout (pre-spawn category-C fail) |
| `acceptance_target_stale` | target serves a different (or `-dirty`) build (pre-spawn category-C fail) |
| `acceptance_target_unreachable` | no scheme reached the target, or it never became ready (pre-spawn category-C fail) |
| `acceptance_preview_teardown_failed` | teardown hook non-zero exit (advisory; outcome unchanged) |
| `acceptance_preview_teardown_missing` | **advisory**: a provision command is configured but no teardown command is — the provisioned instance will not be torn down |
| `acceptance_tree_provisioned` | the merge-candidate checkout was created; carries `path` + `head_sha` (see below) |
| `acceptance_tree_skipped` | merge-candidate checkout skipped — empty expectation, no dispatch dir, or a non-git dispatch dir (warn-and-proceed) |
| `acceptance_tree_stale_swept` | a leftover checkout dir from a crashed prior run was removed before provisioning |
| `acceptance_tree_fetch_failed` | the head SHA was absent locally and the bare-SHA fetch failed (may still `worktree add` from a reachable object) |
| `acceptance_tree_failed` | `git worktree add` failed (unfetchable/invalid SHA); the agent spawns unprovisioned (warn-and-proceed) |
| `acceptance_tree_removed` | teardown removed the checkout (a `"fallback":"rm_prune"` field marks the `os.RemoveAll` + prune path) |
| `acceptance_tree_teardown_failed` | teardown could not remove the checkout even via the fallback (advisory; outcome unchanged) |

`acceptance_preview_teardown_missing` is a misconfiguration warning, not a failure: it fires only on the path where provisioning actually runs (past the no-hosts and no-expectation skips), and it does **not** block provisioning — an operator whose provision command tears itself down is not affected.

## No-acceptance-stage path

These hooks are invoked **only** by the acceptance stage's pre-spawn gate. A `.fishhawk/workflows.yaml` that declares no acceptance stage never reaches the gate, so `FISHHAWK_ACCEPTANCE_PREVIEW_*` env — set or unset — is inert: no provision, no teardown, no dangling preview instance, no gate events. Setting these variables in an environment that also runs non-acceptance stages (plan, implement, review) is harmless; those stages structurally skip the gate.

## Merge-candidate tree for repository-content criteria

The acceptance agent spawns in a fresh **empty** temp dir (diff-withholding, ADR-049 #4): it has no repository checkout. Some acceptance criteria are nonetheless repository-content criteria — a `verify_hint` naming an in-repository check (Posture B in the acceptance prompt). To evaluate those correctly the runner provisions, after the identity gate passes and **before** the agent spawns, a disposable read-only checkout of the merge candidate:

- **What.** A `git worktree add --detach` of the same `acceptance_expected_head_sha` the identity gate verified, at the run/stage-keyed path `/tmp/fishhawk-acceptance-tree-<run>-<stage>`. The path format is mirrored byte-for-byte between the backend prompt (`prompt.AcceptanceTreePath`) and the runner (`acceptanceTreePath`), so the tree the prompt names is the tree the runner creates.
- **Source.** The checkout is taken against the operator's dispatch `working_dir` — the repo the run's lineage worktrees already hang off — so no network clone is needed on the common path. If the head object is not present locally the runner attempts a bare-SHA `git fetch origin <sha>` first.
- **Why.** Without this tree a Posture B check greps whatever checkout it finds on the host — the dispatch checkout or a lineage worktree — either of which `working_tree_restored` may have detached back to `main`. A reference the PR deletes then appears to remain and a criterion the PR head satisfies false-fails `assertion_fail` (`#1881`). The prompt names this checkout as the ONLY sanctioned tree, forbids evaluating a repository-content criterion against any other checkout, and instructs the agent to mark the criterion `skipped` when the sanctioned tree is absent.
- **Warn-and-proceed.** Provisioning **never** fails the stage. An empty expectation (a pre-#1569 backend), a non-git dispatch dir (e.g. a CI runner with no local checkout), or an unfetchable SHA emits `acceptance_tree_skipped` / `acceptance_tree_failed` and the agent spawns unprovisioned — an honest skipped criterion beats a false `assertion_fail`, and the preview-target criteria are unaffected.
- **Teardown.** The checkout is removed on **every** post-provision return (`git worktree remove --force`, with an `os.RemoveAll` + `git worktree prune` fallback that survives the macOS `/tmp`→`/private/tmp` symlink registration mismatch), emitting `acceptance_tree_removed` or `acceptance_tree_teardown_failed`; teardown is best-effort and never changes the stage outcome.

The agent's `WorkingDir` is unchanged — it remains the fresh empty temp dir. The merge-candidate checkout is a separate, sanctioned read-only tree the agent is pointed at only for repository-content criteria.

## Least-privilege guidance

The provisioned preview runs untrusted merge-candidate code. When the preview shares infrastructure with anything you care about — most commonly a shared Postgres — scope the preview binary's credentials to a throwaway resource, never an admin credential:

- Give the preview a database role that owns **only** a throwaway `<db>_preview` database and is denied `CONNECT` to your real database.
- Reserve the admin credential for the privileged provisioning step; never hand it to the branch binary.

The dogfood `scripts/dev preview` implements exactly this (a normalized non-superuser `fishhawk_preview` role, E31.19 / #1577); mirror the posture in a custom provision command.

## Worked example: docker-compose

A self-contained provision/teardown pair using Docker Compose. It builds and serves the merge candidate at `FISHHAWK_PREVIEW_SHA` and exposes `/healthz` with a matching `git_sha`.

Wire the hooks (runner-process env):

```sh
export FISHHAWK_ACCEPTANCE_PREVIEW_CMD="docker compose -p fishhawk-preview up -d --build"
export FISHHAWK_ACCEPTANCE_PREVIEW_TEARDOWN_CMD="docker compose -p fishhawk-preview down -v"
# optional: widen the budgets if the image build is slow
export FISHHAWK_ACCEPTANCE_PREVIEW_TIMEOUT_SECS=600
export FISHHAWK_ACCEPTANCE_PREVIEW_READY_TIMEOUT_SECS=120
```

`docker-compose.yml` — the build consumes `FISHHAWK_PREVIEW_SHA` (injected by the runner) as a build arg, and the served `/healthz` echoes it back as `git_sha`:

```yaml
services:
  preview:
    build:
      context: .
      dockerfile: Dockerfile
      args:
        # FISHHAWK_PREVIEW_SHA is injected into the provision hook's env by
        # the runner; stamp it into the binary so /healthz can serve it.
        GIT_SHA: ${FISHHAWK_PREVIEW_SHA:?provision hook must receive FISHHAWK_PREVIEW_SHA}
    ports:
      # FISHHAWK_PREVIEW_TARGET_HOST is <host>:<port>; publish that port.
      - "8090:8090"
    environment:
      APP_ADDR: ":8090"
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:8090/healthz"]
      interval: 2s
      timeout: 3s
      retries: 30
```

Contract the image must satisfy for the runner's identity probe to reach `verified`:

- `GET /healthz` returns `200` with a JSON body containing `git_sha`.
- That `git_sha` is the built commit — a ≥7-character prefix of `FISHHAWK_PREVIEW_SHA` — and is **not** `-dirty`. Build from a clean checkout of the exact SHA so the stamp is the committed identity.
- The service listens on the port named in `FISHHAWK_PREVIEW_TARGET_HOST` (here `8090`, matching the first declared `egress.target_hosts` entry).

Flow at run time: the runner runs the provision command (`docker compose … up -d --build`) with `FISHHAWK_PREVIEW_SHA` in its env → polls `http://<host>:8090/healthz` every 2s → reads `git_sha`, matches it against `FISHHAWK_PREVIEW_SHA` → `verified` → spawns the acceptance agent → after the verdict ships, the deferred teardown (`docker compose … down -v`) removes the containers and volumes.

`down -v` removes the named project's volumes so no preview state persists between runs; `-p fishhawk-preview` isolates the project name so teardown targets exactly what provision created.
