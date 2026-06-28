# AGENTS.md

This file provides guidance to coding agents working with code in this repository.

## Repo state

Pre-alpha. Most code referenced in `docs/MVP_SPEC.md` doesn't exist yet — it's split into ~49 child issues across 15 epics in Project #7. Verify file tree before assuming.

## Canonical references

- `docs/MVP_SPEC.md` — v0 scope. Cite section numbers when scope is in question.
- `docs/ARCHITECTURE.md` — current technical realization (stack, lifecycle, storage, invariants). Read before designing anything cross-component.
- `docs/BRAND_FOUNDATIONS.md` — voice, naming, positioning.
- `docs/METHODOLOGY.md` — autonomy tiers (low/medium/high).
- `docs/spec/` — canonical JSON Schemas + reference docs for the workflow spec (`.fishhawk/workflows.yaml`) and the plan artifact (`standard_v1`). Validate with `check-jsonschema --schemafile <schema> <yaml-or-json>`.
- `docs/api/` — REST API surface: `v0.openapi.yaml` is source of truth, `v0.md` is the human companion. Lint with `npx -y @redocly/cli@2.31.5 lint --config docs/api/redocly.yaml docs/api/v0.openapi.yaml` (pinned version — see the pinning rule under "Build, test, lint").
- `.fishhawk/workflows.yaml` — placeholder; executed by the product itself starting Day 21 (~2026-05-20).

## Documentation surfaces

- `README.md` (root) is the only human-facing doc. Keep narrative.
- Everything in `docs/` is agent-consumed. Write structured, dense, no fluff.
- Public-facing docs deploy via GitHub Pages (source path TBD).

## Build, test, lint

Multi-module Go workspace; **no root `go.mod`**, so `go build ./...` from root fails. The common gates are wrapped by `scripts/test`:

```sh
scripts/test               # `go test -race ./...` in every registered module
scripts/test coverage      # the same, plus the aggregate coverage gate
scripts/test lint          # `golangci-lint run ./...` in every registered module
scripts/test verify        # lint THEN tests (no coverage) — the runner's verify gate
scripts/test single -run TestName ./backend/internal/version/   # passthrough
```

`scripts/test lint` runs `golangci-lint run ./...` per registered module — byte-for-byte CI's lint invocation (`ci.yml`). Because `.golangci.yml` enables the `gofmt`/`goimports` formatters, golangci-lint v2's `run` fails on unformatted files, so `lint` covers gofmt/goimports drift with no separate gofmt invocation. `scripts/test verify` runs `lint` first (a fast format/lint failure leads the captured output and aborts before the slow test loop) then the test loop, omitting coverage to bound runtime. Both **fail closed** if golangci-lint is absent from PATH — an actionable error naming the v2.x install pin, never a silent skip. The runner's committed-tree implement verify gate runs `scripts/test verify`, so gofmt/golangci-lint defects fail in-loop rather than red-lining the PR in CI after the agent is terminal (#1064).

Per-module without the wrapper (still useful for `go build` and `golangci-lint`):

```sh
go build ./backend/...
golangci-lint run ./backend/...
```

**Backend tests share ONE testcontainers Postgres (#1174, was #972):** the residual #972 start-contention flake (~GOMAXPROCS Postgres containers started at once → "context deadline exceeded after 9 retries") is eliminated at the source by `backend/internal/pgtest`: a single reused container named `fishhawk-test-postgres` (testcontainers `WithReuse`+`WithName`, attach-retry on the first-start name conflict AND on a stale reuse reference — docker `No such container` — re-creating a daemon-evicted container, #1402) shared across every package process, with each test handed its OWN ephemeral `CREATE DATABASE ... TEMPLATE` clone for isolation (cross-process-idempotent template bootstrap, advisory-locked, tolerating SQLSTATE 42P04). Consumers call `pgtest.NewPool(t)` / `pgtest.NewURL(t)` — do NOT hand-roll a per-package `tcpostgres.Run`. (`backend/internal/postgres/postgres_test.go` is the one exemption: it tests MigrateUp/MigrateDown and needs raw, un-migrated throwaway databases.) With the N-container start storm gone, `scripts/test` (default and `coverage`) restored `go test -p "${FISHHAWK_TEST_P:-4}"` (the 2→4 bump was gated on a -p 4 run proving a single shared container via `docker ps`); lower it on a constrained daemon with `FISHHAWK_TEST_P=2`; `scripts/test single` is unbounded. **`scripts/test` also exports `TESTCONTAINERS_RYUK_DISABLED=true`** so the named container PERSISTS across the package processes that reuse it (the ryuk reaper is itself a shared single-point-of-failure container that times out under daemon load, cascading one flake into a whole-suite red); because nothing else reaps it, `scripts/test` removes it via an `EXIT` trap (`docker rm -f fishhawk-test-postgres`, guarded to no-op when Docker is absent) after the module loop — `pgtest` deliberately does NOT Terminate it. The runner's committed-tree verify gates additionally absorb ONE infra flake per stage (re-run the verify in place, `verify_infra_flake_retry` trace event) before failing.

`.golangci.yml` is **v2 format** (`version: "2"` at top). Local install must be golangci-lint v2.x; v1 binaries reject this config.

**Pin executable tooling fetched inside CI/release `run:` steps.** Any tool a workflow downloads and executes at run time — install scripts piped to a shell (`curl … | sh`), `npx` packages — MUST be pinned to a specific immutable tag or version, never `master`/`main`/`latest`/a floating major. A third-party repo can change its `master` install script or publish a new `@latest` and red-line the entire pipeline (including `main`) with zero change on our side — exactly what happened when golangci-lint's `master/install.sh` broke CI (#607/#608). Current pins: golangci-lint `install.sh` → the `v2.8.0` tag (all four workflows: `ci.yml` + the three `*-release.yml`); `@redocly/cli` → `2.31.5` (in `ci.yml` and the `docs/api/` lint/preview commands — keep these in sync). GitHub Action refs (`actions/checkout@vN`, etc.) are the exception: they stay on floating major tags because `.github/dependabot.yml`'s `github-actions` entry bumps them deliberately and reviewably. npx-in-run-step pins are not yet Dependabot-tracked, so bump them by hand.

**Coverage gate**: aggregate ≥ 80% across **all** registered modules, excluding sqlc-generated `*/db/` packages (CI uses `--exclude '/db/'` so new sqlc packages auto-skip). Tiered targets in `docs/ARCHITECTURE.md` §9. CI fails `CI Pass` if the threshold drops. `scripts/test coverage` runs the same check. The underlying loop (CI uses the same shape):

```sh
profiles=()
while IFS= read -r m; do
  (cd "$m" && go test -race -coverprofile=coverage.out -covermode=atomic ./...)
  profiles+=("$m/coverage.out")
done < <(go work edit -json | jq -r '.Use[].DiskPath')
python3 scripts/check-coverage.py --threshold 80 --exclude '/db/' "${profiles[@]}"
```

When editing a schema under `docs/spec/`, run `scripts/sync-schemas` to mirror the change into all embedded copies before committing. CI fails the schema-sync gate otherwise. Opt-in local check: `git config core.hooksPath .githooks`. The `workflow-v*.schema.json` glob fans each workflow major out to two mirrors — `backend/internal/spec/schemas/` and `cli/internal/spec/schemas/` — so `workflow-v0` and `workflow-v1` (ADR-046 / #1381) each have a canonical + two mirror copies, and `/healthz` advertises both the `workflow-v0` and `workflow-v1` embedded-schema hashes (`spec.EmbeddedSchemaHash` / `EmbeddedSchemaHashV1`).

### Schema change checklist

Before opening a PR that adds or modifies a field in `docs/spec/`:

1. **Additive or breaking?** New optional fields within the current major version (e.g., `standard_v1.x`, `workflow-v0.x`) are additive — proceed. A new *required* field in an existing major version is a breaking change and must be avoided: bump the major version instead (`standard_v2`, `workflow-v1`). Use the `x-intended-required` annotation (see below) to signal a field whose required promotion is deferred through a soak period.

2. **Soak period.** When a field is introduced as optional but is intended to become required in a future version, declare the soak window in the PR body under `## Notes`. The soak period duration is determined per-PR; no minimum is set yet (TBD in a follow-up issue). During the soak, both the old and new schema versions are validated by the backend.

3. **Version advertising.** After the schema change merges, update every surface that advertises supported schema versions:
   - Plan validator (`backend/internal/plan/`) — add the new version string to the recognized set.
   - Runner `/healthz` schema-versions endpoint (once #466 lands) — add the new version to the advertised list.
   - **A NEW workflow MAJOR** (ADR-046 / #1381 stood up `workflow-v1` as the keystone): add the canonical `docs/spec/workflow-vN.schema.json`, run `scripts/sync-schemas` to mirror it into `backend/internal/spec/schemas/` + `cli/internal/spec/schemas/` (the `workflow-v*` glob routes it — no script edit), append a `{Major: N, Path: …}` entry to the `embeddedSchemas` routing table in BOTH `backend/internal/spec/parse.go` and `cli/internal/spec/spec.go` (this is what makes the version-routed validator dispatch to the new major instead of failing closed), add an `EmbeddedSchemaHashVN()` + the `workflow-vN` `/healthz` `schemas`-map entry in `backend/internal/server/handlers.go`, and register the new mirror set as its own self-referential surface-sweep pattern in `backend/internal/server/surface_sweep.go`.

4. **`x-intended-required` annotation.** When a field is optional now but will become required in the next major version, annotate it in the JSON Schema:
   ```json
   "some_field": {
     "type": "string",
     "x-intended-required": true
   }
   ```
   JSON Schema Draft 2020-12 §10.3 collects unknown keywords as annotations without affecting validation, so this annotation is safe to add to any schema without breaking existing validators.

### Auth change checklist

Before opening a PR that adds or tightens handler-side authorization (scope check, role check, audience check), the PR body must include:

1. **Impact inventory.** Which active tokens would lose access if the change shipped as-is? Run `fishhawkd token migrate --db $FISHHAWKD_DATABASE_URL` (dry-run, no `--apply`) to enumerate affected tokens. Paste the summary line into `## Notes`.

2. **Migration path.** State how affected tokens will be brought up to the new requirements. Options in order of preference:
   - `fishhawkd token migrate --apply` — promotes tokens whose scope set is a strict subset of the operator default (see #529).
   - Re-issue the token with `fishhawkd token issue --subject <s>`.
   - Manual DB update (last resort; document the exact SQL).

3. **Safe-to-ship determination.** The PR is safe to ship without operator-side action only when the impact inventory is empty (step 1 produced `scanned=N migrated=0`). If the inventory is non-empty, the migration path must be completed before or immediately after deploy, and this must be stated explicitly.

Cross-reference: this checklist codifies the rollout discipline introduced by #529; see #472 for the analogous schema-change discipline.

### Rebuild matrix

`scripts/dev up` auto-detects which binaries need rebuilding by diffing `origin/main...HEAD` (falling back to `main...HEAD` with a warning if origin is unreachable) against the table below. `fishhawkd` always rebuilds as the baseline; the others rebuild only when their source changed. `scripts/dev up --all` forces all four. Each rebuild prints a line naming the trigger (`baseline`, `--all`, or the path that matched).

| If PR touches | Rebuild |
|---|---|
| `backend/cmd/fishhawkd/` or `backend/internal/{server,prompt,plan,spec,runner,webhook,notifier,github,storage,db,...}` | `fishhawkd` |
| `backend/cmd/fishhawk-mcp/` | `fishhawk-mcp` |
| `runner/cmd/fishhawk-runner/` or `runner/internal/...` | `fishhawk-runner` |
| `cli/cmd/fishhawk/` or `cli/internal/...` | `fishhawk` CLI |
| `backend/internal/plan/` or `backend/internal/spec/` (shared libs) | all four binaries |

**Rebuilt-on-disk is not the same as live.** What it takes for a rebuilt binary to actually take effect differs per binary:

| Binary | Activation after a rebuild |
|---|---|
| `fishhawkd` | rebuilt **and restarted** by `scripts/dev up`/`reload` |
| `fishhawk-runner` | picked up automatically — spawned fresh from `bin/` on each `fishhawk_run_stage` |
| `fishhawk-mcp` | rebuilt on disk but **needs a manual `/mcp` reconnect** — the Claude Code harness owns the MCP server process, so `scripts/dev` cannot restart it; it only signals the operator |

**Dev builds are GitSHA-stamped (#1007).** `scripts/dev` stamps the short HEAD SHA (`-dirty` suffix on a dirty tree) into all four binaries via `-ldflags -X <module>/internal/version.GitSHA=…`, and `scripts/dev k8s` passes the same value as the image's `GIT_SHA` build arg — so `/healthz` `git_sha`, the runner's `runner_started`/`version` output, the MCP handshake version, and `fishhawk version` report the real build commit instead of `unknown`. `Version` intentionally stays `dev` — it carries the MinRunnerVersion no-enforcement semantics. A wrong `-X` package path is a **silent no-op**, not a build error: `scripts/test-dev` body-greps each `_build_ldflags` path against the `var GitSHA` declaration in the matching `version.go`, so keep them in sync when moving a version package. Release-workflow GitSHA stamping (`.github/workflows/**`, human-led) is a separate follow-up.

`scripts/dev post-merge [<issue>] [--start-deps]` is the one-step full per-PR post-merge walk: it `git pull --ff-only origin main` (a diverged local main fails loud rather than landing a merge commit), prunes the merged local branch via `scripts/cleanup-merged` (reused, not reimplemented — it deletes only ancestors of `origin/main`, never `main`), optionally confirms a given issue is `CLOSED` via `gh issue view` (warn-only, non-fatal), then runs `reload` LAST so reload's `/healthz` readiness gate (#628 — non-zero exit + log tail on failure) and MCP-reconnect verdict are inherited and the verdict is the command's final line.

`scripts/dev reload` (down-then-up) is the stack-only primitive `post-merge` composes: plain `scripts/dev up` no-ops when fishhawkd is already running and so never rebuilds. **`reload` always rebuilds all four binaries (it forces `--all`)** — after a merge `HEAD == origin/main`, so the `origin/main...HEAD` diff is empty and the rebuild matrix would match nothing, silently skipping runner/CLI/mcp. Forcing `--all` closes that gap.

Because `--all` always rebuilds `fishhawk-mcp`, the `/mcp` reconnect banner is **decoupled from the rebuild action**: it is keyed to whether the pull's merge-aware diff (`HEAD@{1}..HEAD`) actually touched fishhawk-mcp source — `backend/cmd/fishhawk-mcp/` or the shared libs `backend/internal/plan/` + `backend/internal/spec/` (the same matrix globs that route to mcp). So `reload` no longer false-nags a reconnect when the merge never touched the MCP server. Fresh-clone / no-reflog / detached-HEAD (where `HEAD@{1}` is unresolvable) falls back to firing the banner conservatively — never a false-negative silent-stale mcp. Plain `up` (branch-diff, not `--all`) still keys the banner off whether mcp was rebuilt, since there a rebuild genuinely means mcp source differs from main. This supersedes the older "MCP server stale after rebuild" gotcha by making the reconnect-vs-rebuild distinction first-class.

**Readiness gate verifies listener identity (`scripts/dev`, #965, nonce round-trip #1018).** A healthy `/healthz` plus a live spawned pid is not proof the spawn succeeded — a stale daemon squatting on the port answers the gate while the fresh fishhawkd dies on `bind: address already in use` (the reload false-success). `up`/`reload` (a) preflight the fishhawkd port (from `FISHHAWKD_ADDR`, post-.env) before spawning and fail loud naming any squatting pid + command, and (b) after the gate prove the listener IS the spawned daemon by nonce round-trip: `up` generates a per-spawn nonce (persisted to `.fishhawk/dev.nonce`), hands it to the child as `FISHHAWKD_START_NONCE`, and `_verify_healthz_nonce` requires `/healthz` to echo it back as `start_nonce` — surviving OS pid reuse, which the older #965 pid comparison cannot. A mismatch or unreachable `/healthz` fails per the #628 contract (expected-vs-actual nonce, log tail, exit 1); a body with no `start_nonce` field (pre-nonce binary) degrades with a warning to the #965 pid check (`_verify_listener_identity`). `down` no longer returns success on a missing/stale pid file alone: every path falls through to a port fallback (`_down_port_fallback`) where the nonce only UPGRADES confidence, never blocks the #965 path — an exact `/healthz` `start_nonce` match against `.fishhawk/dev.nonce` proves the listener is our stale fishhawkd (kill TERM→KILL, reported as nonce-verified); ANY other outcome (no nonce file, unreachable healthz, missing field, different nonce) degrades to the comm-basename heuristic unchanged, which kills fishhawkd-named listeners but only reports — never kills — a foreign process, with `down` exiting 1 rather than claiming a clean shutdown. Both commands sweep numbered stray pid files (`dev 2.pid` …), `down` sources `.env` for port parity with `up`, and every teardown path removes `.fishhawk/dev.nonce`. Hosts without `lsof` degrade to the old behavior with a warning. The contract is locked by `scripts/test-dev`'s real-listener (nc) squatter tests, one-shot fake-`/healthz` responder tests (mismatch, pre-nonce degrade, down-fallback ladder), and call-site body-greps.

**ZERR diagnostic trap (`scripts/dev`, #631).** `up`/`reload` print a `up: starting` / `reload: starting` entry line first and install a zsh `TRAPZERR` (ZERR) trap that, on any command aborting under `set -e` in a non-tested context, prints `scripts/dev: command failed (exit <code>) at line <file:line>` to stderr — using `${funcfiletrace[1]}` for the error-site line, not the trap's own. So a top-level abort can never again exit 1 with zero output. The trap is silent in normal operation (ZERR does not fire for if/while/&&/||/! tested contexts). It is the permanent diagnostic for the intermittent #631 abort, whose root cause it pinned on the first recurrence: **`(( i++ ))` returns exit 1 when `i==0`** (post-increment yields the old value `0`, and `(( 0 ))` is false), aborting the wait loop under `set -e` — fixed by pre-increment `(( ++i ))`. When writing zsh under `set -e`, prefer `(( ++i ))` / `i=$(( i + 1 ))` over `(( i++ ))`, and the `if [[ cond ]]; then x=1; fi` form over `[[ cond ]] && x=1` (an assignment-RHS `&&` is a tested-context hazard). `scripts/test-dev` guards both regressions.

**Local Kubernetes bring-up (`scripts/dev k8s` / `k8s-down`, #852).** `scripts/dev k8s` (alias `make k8s-up`) is the one-command operator path to run fishhawkd on Docker Desktop's Kubernetes: it `docker build`s the image into the host daemon as `ghcr.io/kuhlman-labs/fishhawkd:dev-local` (Docker-Desktop k8s shares the image store — no push / kind load), `helm upgrade --install`s the chart with `values-local.yaml` + `--set image.tag=dev-local --set image.pullPolicy=IfNotPresent`, waits for the rollout, then port-forwards `svc/fishhawk 8080:8080` and gates on `/healthz` (the authoritative readiness signal — the in-cluster migrate Job is a `post-install` hook, so rollout-status can go green before it finishes). `scripts/dev k8s-down` (`make k8s-down`) kills the tracked port-forward (`.fishhawk/k8s-pf.pid`) and `helm uninstall`s. The full image→install→healthz path is an operator smoke test against a Docker-Desktop cluster, NOT run in CI; `scripts/test-dev` covers the pure helpers + the readiness-gate body contract. Quickstart: `docs/deploy/kubernetes.md`.

## Adding a Go module

1. `mkdir <name> && cd <name> && go mod init github.com/kuhlman-labs/fishhawk/<name>`
2. Append `use ./<name>` to `/go.work`.
3. Verify: `(cd <name> && go build ./... && go test ./... && golangci-lint run ./...)`.
4. **Wire the module into `backend/Dockerfile`** if the image build must resolve it. The image runs `go mod download` against the whole `go.work` workspace inside the container, so EVERY module listed in `go.work` must have its `go.mod` copied before that step — add `COPY <name>/go.mod[ <name>/go.sum] ./<name>/` to the first COPY phase (omit `go.sum` if the module has none) and `COPY <name> ./<name>` to the source phase. A module added to `go.work` but not the Dockerfile fails `go mod download` with `cannot load module ../<name>` — and this was invisible at PR time until the PR-CI `docker-build` gate landed (#735), because the on-disk workspace the `go` job builds against always has the module present (the #733 / #672 break: red main for 5 merges).

## Git flow

**Issue → feature branch → PR → docs + follow-ups → close issue → update relevant issues.**

1. Pick a child issue from Project #7. If a blocking ADR is open, resolve it first.
2. Branch from `origin/main`: `<issue-slug>-<desc>`, e.g. `e3.1-backend-skeleton`.
3. Commit with `git commit -s` (DCO is mandatory; PRs without sign-off are rejected). Imperative-mood title, no conventional-commits prefix. Use HEREDOC for multi-line messages.
4. **Update docs in the same PR**, before opening:
   - New package / HTTP route / env var / flag → `docs/ARCHITECTURE.md` "Where to look" table; operator-facing inputs also → component `README.md`.
   - Spec or schema change → `docs/spec/<x>.md` + every embedded copy (CI's schema-sync diff fails otherwise).
   - HTTP API change → `docs/api/v0.openapi.yaml` (source of truth) + `docs/api/v0.md`.
   - Add / remove / rename an issue-comment surface (Notifier method or audit kind) → `docs/issue-comment-surfaces.md`.
   - Voice/naming → `BRAND_FOUNDATIONS.md`. New trap / build workflow → `AGENTS.md`. Autonomy convention → `METHODOLOGY.md`.
5. **File issues for deferred work** before the PR opens. Any TODO, "follow-up PR", "deferred to E…", or obvious operability gap gets a tracking issue. **Prefer `fishhawk_file_issue`** (the work-management tool, #1005) over the hand-rolled `gh issue create` + four labels + `gh project item-add` + GraphQL-status dance: it applies this repo's `work_management` conventions automatically — the `[E<parent>.<n>]` / `[ADR-NNN]` title format, the `type:*` default label, the per-type body skeleton, `Status=Backlog`, ADR numbering, and the parent-epic link — so you pass `type` + `summary` + the `area:*/autonomy:*/phase:*` labels + `relations.parent_epic` and it renders a conventions-complete item. For a numbered type (`adr`) ADR numbering is now discovered server-side from the tracker (#1269), so `existing_numbers` is OPTIONAL — the backend searches the existing numbered titles (open and closed) and allocates the next one. Pass `existing_numbers` only to override/hint that discovery, or seed `existing_numbers:[0]` (which yields 1) if discovery is unavailable for the target provider; a genuine discovery failure fails closed with `work_item_invalid` (`details.discovery_failed`) rather than allocating a wrong number (the #1265 empty-list guard remains the last line for a provider without discovery). Reference the filed issue from the PR body's `## Notes` so the deferral is reviewable.
   - **Board placement** is automatic (`boarded:true`) when `FISHHAWKD_PROJECTS_TOKEN` (a `project`-scoped PAT/UAT) is configured — required because Project #7 is a *user-owned* project, which App installation tokens cannot reach (Projects permission is org-only; #1114). If that token is absent, the tool still files the conventions-complete issue but degrades to `boarded:false` with a logged `boarding_error` (best-effort, #1107) — board it manually then: `gh project item-add 7 --owner kuhlman-labs --url <issue-url>`. Note: `gh project item-list` caps at 100 items so it can't see the newest entry on #7 (>100 items) — confirm board membership via the issue's `projectItems`, not the project's item list.
   - **For Fishhawk product friction** (a bug in the tooling itself hit during a run), prefer **`fishhawk_report_product_issue`** with the run's id — it attaches a redacted, fingerprint-deduped diagnostic bundle (#1006).
6. Open PR — body uses `## Summary` / `## Test plan` / optional `## Notes` / `Closes #<issue>`. Match #80, #81.
7. After merge, walk the dependents:
   - Verify parent epic's task list checked off.
   - Update sibling issues if scope shifted (e.g., a CI fix bundled here means another sibling no longer needs it).
   - If an ADR was resolved, edit its body's Decision section and close.

Run `scripts/cleanup-merged` to delete local branches that have been merged into `origin/main` (optional; can be wired into a post-merge git hook).

Force-pushing a feature branch is fine with `--force-with-lease`; ask before force-pushing if there are review comments. Never force-push `main`.

## Project tracker (#7)

| Range | Kind | Title format |
|---|---|---|
| #1–#15 | Epic | `[EX] desc` |
| #16–#64 | Child | `[EX.Y] desc`, body ends `Parent epic: #N` |
| #65–#79 | ADR | `[ADR-NNN] desc`, body uses Context / Options / Recommendation / Decision / Consequences |

- **Labels** (apply all four to every impl issue): `area:*`, `autonomy:*`, `phase:*`, `type:*`. Plus `epic` or `adr` on parents.
- **Custom fields**: Component, Phase, Autonomy, Estimate, Priority, Target day. Status **must** be set to `Backlog` on creation (default is null; null breaks the kanban view).
- **Day 1 = 2026-04-30.** Phase milestones derive from there.

## Voice (BRAND_FOUNDATIONS §5)

Direct, technical, restrained, honest about trade-offs. Banned:

- "revolutionary," "game-changing," "next-generation," "industry-leading," "world-class"
- "AI-powered" as a sole differentiator
- "frictionless," "seamless," "effortless"
- "empower" in any context
- "trust" as a marketing claim

Error messages: precise about what failed and how to fix. No generic apologies.

## Naming

- Product: **Fishhawk** in prose; **`fishhawk`** lowercase for CLI, `.fishhawk/`, `fishhawk/runner@v1`, `app.fishhawk.[tld]`.
- Go module paths: `github.com/kuhlman-labs/fishhawk/<module>`.
- Workflow primitives (workflow, stage, gate, constraint, approver, artifact, plan, audit log) — lowercase nouns. Never branded.

## Traps

- **macOS bash is 3.2** — no associative arrays. Use zsh, gawk, or awk lookups for scripts that need them.
- **`go.work` is committed**; `go.work.sum` is not. `go.sum` will appear on first external import.
- **Project #7 Status field** has 6 options (Backlog/Up Next/In Progress/In Review/Blocked/Done) set via GraphQL `updateProjectV2Field` with `singleSelectOptions` (undocumented input field). Repeat the same workaround if creating new projects.
- **Project #7 owner is `kuhlman-labs` as a `user`, not an `organization`** — the GraphQL queries that take an owner login must use `user(login:"kuhlman-labs")`, not `organization(...)`, or you get a NOT_FOUND. The repo lives under what looks like an org namespace but it's a user-owned account.
- **Project #7 has > 100 items.** GraphQL caps `items(first:N)` at 100. To find an item ID for a known issue, query the issue's `projectItems(first:10)` and filter by `project.id` instead of paginating the project's items list — same answer in one round-trip with no cursors.
- **jsdom rejects `__Host-` cookies under HTTP.** The default vitest jsdom URL is `http://localhost/`, but `__Host-` requires Secure, and tough-cookie won't honour it over HTTP. `frontend/vite.config.ts` sets `test.environmentOptions.jsdom.url = 'https://localhost/'` so cookie semantics in tests match production. Browsers treat `localhost` as a secure context anyway, so dev still works.
- **`scripts/test` exports `GIT_CONFIG_GLOBAL=/dev/null` + `GIT_CONFIG_SYSTEM=/dev/null`** so temp-repo test commits never inherit the operator's global `commit.gpgsign` (#912). Without this, a global `commit.gpgsign=true` red-lines the verify gate when the SSH/GPG signing agent (e.g. 1Password op-ssh-sign) is down. No-op on CI (no signing config); in-repo git ops are unaffected because repo-local `.git/config commit.gpgsign=false` takes precedence over the now-absent global.
- After **Day 21**, every change must flow through a Fishhawk workflow run (today: by convention; later: enforced by the product).
- **INTERIM (`fishhawk run auto-decide`, #1233) — REMOVE when #1232 ships.** While a `fishhawk_run_stage` call blocks the driving MCP session, a mid-stage scope amendment the implement agent files cannot be decided in-band (#1189). As a stopgap, launch `fishhawk run auto-decide <run-id>` **detached** (backgrounded) alongside the blocking `fishhawk_run_stage`, using the **operator/operator-agent token** (the decision endpoint rejects run-bound `fhm_` tokens). It polls the run's pending amendments over the existing `?wait` long-poll and **auto-approves ONLY** amendments whose every path is a coupled `*_test.go` sibling — a `<dir>/<stem>_test.go` whose production sibling `<dir>/<stem>.go` is already in the run's approved-plan `scope.files`. Anything else (a non-test path, a production file, or a test whose production sibling is out of scope) is left undecided → today's fail-and-retry. It is a second decision channel that changes **no** backend/runner/product-runtime code (it reuses endpoints that already exist). This entire subcommand is to be **removed when #1232 (durable non-blocking dispatch) makes the single-session in-band decision native.**
