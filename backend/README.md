# fishhawkd

The Go service that orchestrates workflow runs in Fishhawk. Owns the
workflow run / stage state machine, the policy evaluator, approval state,
the audit log writer, the GitHub App webhook receiver, and the REST API
consumed by the CLI and the Web UI.

This directory is its own Go module. It is tied into the repo via
`go.work` at the root so it can be tagged and released independently of
the runner action and the CLI. See
[ADR-014](https://github.com/kuhlman-labs/fishhawk/issues/78) for the
multi-module rationale.

## Layout

- `cmd/fishhawkd/` — the binary entrypoint with `serve` and `migrate` subcommands.
- `internal/postgres/` — pgx pool wrapper and embedded `golang-migrate` runner. Migrations live under `internal/postgres/migrations/`.
- `internal/run/` — workflow run / stage state machine. Domain types in `run.go`, transition tables in `transition.go`, `Repository` interface in `repository.go`, Postgres adapter in `postgres.go`. sqlc-generated code under `internal/run/db/`.
- `internal/server/` — HTTP server, middleware, handlers.
- `internal/version/` — build version exposed via `-ldflags`.

## Build and test

From the repo root (workspace-aware):

    go build ./backend/...
    go test ./backend/...
    golangci-lint run ./backend/...

Integration tests under `internal/run/postgres_test.go` require Docker (testcontainers spins up Postgres 16). Devs without Docker get a `t.Skip`.

To regenerate sqlc code after editing `internal/run/queries.sql`:

    cd backend && sqlc generate

## Status

- **E3.1 (#41)** — module scaffold.
- **E3.2 (#42)** — HTTP server, middleware, `/healthz`. Middleware order: `recovery → requestID → logging → authStub → mux`.
- **E3.3 (#43)** — run/stage state machine on Postgres. Transitions are validated against an explicit table; persistence uses `SELECT … FOR UPDATE` inside a transaction so concurrent transitions can't both succeed. `fishhawkd migrate up|down` applies the embedded migrations.

Upcoming under epic E3 (#3):

- E3.4 (#44) — policy evaluator.
- E3.5 (#45) — approval state + SLA tracking.
- E3.6 (#46) — REST API surface for CLI + UI.
- E3.7 (#47) — GitHub App webhook receiver wiring.

## Run

Bring up Postgres locally:

    docker compose up -d postgres

Apply migrations and start the server:

    export FISHHAWKD_DATABASE_URL='postgres://fishhawk:fishhawk@localhost:5432/fishhawk?sslmode=disable'
    go run ./backend/cmd/fishhawkd migrate up
    go run ./backend/cmd/fishhawkd serve

    curl http://localhost:8080/healthz

Override the listen address with `--addr` or `FISHHAWKD_ADDR`.

Optional flags:

- `--start-nonce` (or `FISHHAWKD_START_NONCE`) — per-start opaque identity token echoed by `GET /healthz` as `start_nonce`; unset omits the field. `scripts/dev` sets one per spawn and requires the round-trip in its readiness gate (and consults it in `down`'s port fallback), so it can prove the listener on the port is the daemon it started even across OS pid reuse (#1018).
- `--projects-token` (or `FISHHAWKD_PROJECTS_TOKEN`) — optional user PAT/UAT carrying the **`project`** scope. It lets `fishhawk_file_issue` place items on a **USER-owned** Projects v2 board (e.g. Project #7, owner `kuhlman-labs`). A GitHub App installation token cannot reach a personal-account Projects v2 — there is no user-projects permission for Apps — so without this token, board placement on a user-owned project degrades to best-effort `boarded:false` (#1107). It is routed only through the user-owned board-placement GraphQL (issue creation and epic sub-issue linking stay on the installation token). **Secret:** never logged or traced; startup logs presence only (`projects_token_configured`). Unset leaves the #1107 behavior unchanged.
- `--jira-base-url` / `--jira-email` / `--jira-api-token` (or `FISHHAWKD_JIRA_BASE_URL` / `FISHHAWKD_JIRA_EMAIL` / `FISHHAWKD_JIRA_API_TOKEN`) — enable the **jira** work-item provider (#1094) so `fishhawk_file_issue` resolves `provider: jira` instead of returning 501. All three must be set; a partial config is warned and leaves the provider disabled. The base URL is the Jira Cloud instance address (e.g. `https://acme.atlassian.net`); the email + token are the HTTP Basic credentials. The **instance URL + credentials are server-side env** — secrets cannot live in a checked-in repo config — while the per-repo `.fishhawk` work-management `jira` block selects only the project (`project_key` + optional `issue_types`). **Secret:** the email + token are never logged or traced; startup logs presence only (`credentials_configured`) plus the non-secret base URL. Board placement is a best-effort workflow transition (#1107) and the board-state `Transitioner` capability is not implemented for jira in v0.
- `--enable-sla-timer` (or `FISHHAWKD_ENABLE_SLA_TIMER=true`) — start the background goroutine that times out `awaiting_approval` stages past their gate SLA, transitioning them to failed with category D. Off by default so dev runs aren't racing the timer.
- `--sla-interval` — scan interval; defaults to `60s`. Hour-grained SLAs need no finer cadence.
- `--enable-merge-reconciler` (or `FISHHAWKD_ENABLE_MERGE_RECONCILER=true`) — start the merge-status reconciler (ADR-031 Phase 1). It resolves a run's review gate on a verified PR merge state when the `pull_request.closed` webhook was missed — `merged → succeeded`, `closed-unmerged → cancelled`, through the same idempotent path the webhook uses. Each tick also heals dropped `fishhawk_audit_complete` Check Run publishes (#973): every review stage parked in `awaiting_approval` gets a recompute+republish, so a publish lost to a transient GitHub failure lands within one tick of recovery (already-published states dedup to a no-op). Without this flag the publish stays one-shot best-effort. Off by default; requires a GitHub App wired.
- `--merge-reconciler-interval` — reconciler scan interval; defaults to `60s`. **Each tick makes one GitHub `GetPullRequest` call per parked review stage with no per-stage cooldown, plus up to one more inside the audit-check republish recompute — up to 2 calls per parked stage.** Acceptable at v0 scale, but tune this upward at scale to stay within GitHub's 5,000/hour per-installation REST budget.
- `--review-resolution` (or `FISHHAWKD_REVIEW_RESOLUTION`) — deployment-level review-gate resolution provider (ADR-031 Phase 2); defaults to `github_merge`. Selects which `reviewresolver.Resolver` the merge-status reconciler routes through. The default `github_merge` provider resolves a run's review gate only on a verified GitHub merge — `succeeded` always means a verified merge, there is no force-succeed path. **An unknown value fails startup** (fail closed) rather than silently defaulting, so a misconfigured resolver cannot mask a deployment error.
- `--oidc-audience` (or `FISHHAWKD_OIDC_AUDIENCE`) — turn on GitHub Actions OIDC verification on the signing-key endpoint. Callers must present a `Bearer` token whose `aud` claim matches this value, and whose `repository` + `workflow` claims bind to the path's run. Unset = endpoint accepts any caller (v0 self-execution posture; not safe for production).
- `--oidc-jwks-url` — override the JWKS endpoint. Defaults to GitHub's published URL; useful for testing.
- `--oauth-client-id` / `--oauth-client-secret` / `--oauth-callback-url` (or `FISHHAWKD_OAUTH_CLIENT_ID` / `_CLIENT_SECRET` / `_CALLBACK_URL`) — enable the GitHub OAuth sign-in flow at `/v0/auth/github/*`. All three must be set; mismatched configuration fails fast. The callback URL is the public URL of `/v0/auth/github/callback` (the value the OAuth App is registered with).
- `--oauth-redirect-after-login` (default `/`) — relative path the callback handler redirects to on successful sign-in. Absolute URLs and scheme-relative paths are rejected.
- `--external-url` (or `FISHHAWKD_EXTERNAL_URL`) — operator-facing root URL of the SPA, e.g. `https://app.fishhawk.example.com`. Used to build links in surfaces that escape the backend (today: GitHub Check Runs, #231 — `details_url` on the published `fishhawk_audit_complete` check points back here so a reviewer who clicks the check on github.com lands in Fishhawk). Empty disables the publish-to-GitHub paths cleanly; the in-Fishhawk gate enforcement still works without it.
- `--spend-alert-multiple` (or `FISHHAWKD_SPEND_ALERT_MULTIPLE`, default `3`) — warn-only spend-anomaly threshold (#649). The trace upload handler writes a `spend_alert` audit entry when the current hour's estimated model spend exceeds this multiple of the rolling average of prior hours (24h window). It never gates or fails a run; the detector stays quiet until a baseline of prior hours with spend exists.

- **Per-stage allowed-model policies** (#1013, #1416) — the approval gate validates the RESOLVED model for each agent stage against an optional per-adapter allow-list. Format is `adapter=model1,model2;adapter2=model3` (e.g. `claudecode=claude-opus-4-8,claude-sonnet-4-6;codex=gpt-5.5`); an unset policy — or an adapter/provider with no configured set — **fails open** (any model accepted, byte-identical to today). A disallowed resolved model is rejected `422` at the gate, naming the resolved source.
  - `--implement-allowed-models` (or `FISHHAWKD_IMPLEMENT_ALLOWED_MODELS`) — validates the resolved **implement** model, keyed by the implement stage's `executor.agent` adapter (rejection code `plan_invalid_model`).
  - `--plan-allowed-models` (or `FISHHAWKD_PLAN_ALLOWED_MODELS`) — validates the resolved **plan** model (plan `executor.model` or the `plan_model` approval override), keyed by the plan stage's `executor.agent` adapter (rejection code `plan_model_not_allowed`).
  - `--review-allowed-models` (or `FISHHAWKD_REVIEW_ALLOWED_MODELS`) — validates the resolved **review** model (review `executor.model` or the `review_model` approval override), keyed by each implement-stage reviewer provider the override is applied to; a run with no agent reviewers has nothing to validate and fails open (rejection code `review_model_not_allowed`).

- **Size-aware review budget** (#747) — the advisory plan-/implement-review reviewer runs under a per-invocation deadline computed from the prompt size, so large diffs are no longer killed mid-inference with the verdict silently dropped. `--plan-review-timeout` (or `FISHHAWKD_PLAN_REVIEW_TIMEOUT`, default `300s`) is the **floor**; `--review-budget-per-kb` (or `FISHHAWKD_REVIEW_BUDGET_PER_KB`, default `10s`) is the per-KB allowance; `--review-budget-cap` (or `FISHHAWKD_REVIEW_BUDGET_CAP`, default `1200s`) is the ceiling. The effective deadline is `floor + per_kb*ceil(promptBytes/1024)`, clamped to `[floor, cap]`. Set `FISHHAWKD_REVIEW_BUDGET_PER_KB=0` to collapse the budget to a flat floor (the pre-#747 fixed-timeout behaviour) without a redeploy. A reviewer killed by this deadline is recorded as a `*_review_failed` audit entry with `timeout: true`, distinguishing it from a transport/decode failure.

- **Per-run budget tripwire** (`server.Config.MaxRunUSD` / `MaxRunTokens`, default `0` = disabled) — the whole-run safety rail of ADR-030 (#653): a global operator backstop that HALTS a single run once its cumulative estimated cost reaches the configured ceiling, independent of the per-workflow periodic budgets. `MaxRunUSD` is enforced against the run's rolled `cost_usd_total` (#649); `MaxRunTokens` against the run's cumulative input+output tokens. **Decomposition-family aggregation (E24.6 / #1146):** the tripwire sums cost + tokens across the whole decomposition **family** (the parent plus every child) before evaluating, so a wide fan-out can't blow the run budget even when no single child is over its own ceiling; a non-decomposed run's family is just itself, so its figure is unchanged. On breach the trace upload handler cancels the run (terminal state `cancelled`, non-retryable — a protective stop, not a work failure), writes a `run_budget_exceeded` audit entry naming the breached dimension + figures, and dispatches no further stage. A non-positive ceiling disables that dimension, so the default deployment is unaffected. **Note:** this slice ships the config + enforcement + cross-layer test; the CLI flag / `FISHHAWKD_MAX_RUN_USD` env wiring in `cmd/fishhawkd/serve.go` is a deferred follow-up (out of this child run's scope), so today the ceilings are set programmatically on `server.Config`.

- `--max-parallel-children` (or `FISHHAWKD_MAX_PARALLEL_CHILDREN`, default `0` = unlimited) — the global default cap on how many decomposed child runs may dispatch **concurrently** for a single run (E24.6 / #1146). A per-workflow `decomposition.max_parallel` knob (workflow-v0.6) overrides it when `> 0`; `0 = unlimited` on both. The orchestrator resolves the effective cap from the run's cached workflow spec and **surfaces** it (a log line plus `effective_max_parallel` in the `plan_decomposed` audit payload) — it does **not** yet throttle the fan-out (every child is still minted). The concurrency throttle that consumes the resolved cap lands in E24.3 (#1143).

### Inspecting OTel trace spans locally (#649 / #679)

The runner emits a per-run OpenTelemetry GenAI trace — a `stage <name>` span with a `chat <model>` child carrying token counts, estimated cost, and reproducibility attrs (span shape detailed in `docs/ARCHITECTURE.md` §10, "Local OTLP trace collector"). Emission is a no-op unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set, so the default loop is unaffected.

To view a run's trace tree end-to-end against a local collector:

```sh
# 1. Start the opt-in Jaeger all-in-one (the `otel` compose profile —
#    it does NOT start under the default `docker compose up -d`).
docker compose --profile otel up -d

# 2. Point the runner at it (no-op when unset).
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318

# 3. Run a stage, then open the Jaeger UI and select service
#    `fishhawk-runner` to see the per-run trace.
open http://localhost:16686
```

**Execution-locality caveat**: the collector must be reachable from wherever the runner *actually* executes. The standard dogfood loop dispatches the runner to a GitHub-hosted CI runner (`.github/workflows/fishhawk.yml`, `runs-on: ubuntu-latest`), where `localhost:4318` is the CI host's loopback — not this machine. End-to-end local viewing therefore requires the runner to run on a host that can reach the collector: invoke `fishhawk-runner` locally (see `runner/README.md` "Local invocation") with the endpoint set. Exporting from the GHA job is deferred human-led `.github/workflows/**` work.

### Bootstrapping API tokens

`/v0/tokens` requires an authenticated identity to mint a new token (a chicken-and-egg). For the first token, use the CLI:

```sh
fishhawkd token issue --subject github:42 --scopes runs:read,runs:write
```

The plaintext is printed to stdout exactly once (suitable for `... | head -n1`); only the sha256 hash is stored. Subsequent tokens can be minted via `POST /v0/tokens` once you have one bearer in hand.

A token for an operator-agent role instance (ADR-040 D4) uses the subject convention `operator-agent/<role-spec-version>` — e.g. `fishhawkd token issue --subject operator-agent/operator-role-v0` — and gets the same default operator scope set. Issuance rejects an `operator-agent/` subject naming an unrecognized role-spec version; delegated-action audit entries written under such a token record `actor_kind=agent` with the full subject. See `docs/spec/operator-role.md` "Identity and token issuance".

## Container image

`fishhawkd` ships as a distroless static-binary image at
`ghcr.io/kuhlman-labs/fishhawkd`. Two tag streams:

- `:main` and `:sha-<commit>` — pushed by `.github/workflows/backend-build.yml` on every merge to `main`.
- `:v<version>` and `:latest` — pushed by `.github/workflows/backend-release.yml` on `backend/v*` tags. Tagged releases also attach an SPDX-JSON SBOM to the GitHub Release.

Both streams are signed keylessly with [cosign](https://docs.sigstore.dev/cosign/overview/) via GitHub Actions OIDC. To verify before pulling:

```sh
cosign verify ghcr.io/kuhlman-labs/fishhawkd:<tag> \
  --certificate-identity-regexp '\.github/workflows/backend-(build|release)\.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

To build locally (matches the CI build, including version stamping):

```sh
docker build \
  --build-arg VERSION=$(git rev-parse --short HEAD) \
  -f backend/Dockerfile \
  -t fishhawkd:dev .
```

The image's entrypoint is `/fishhawkd serve`; override with the `migrate` subcommand to apply migrations: `docker run … fishhawkd:dev migrate up`. Hosted deploy (ECS Fargate task definition + IAM scaffolding per [ADR-009](https://github.com/kuhlman-labs/fishhawk/issues/73)) is tracked separately in [#148](https://github.com/kuhlman-labs/fishhawk/issues/148).

## See also

Larger context: `docs/MVP_SPEC.md` §5.1.1 (component) and §5.2 (execution flow); `docs/ARCHITECTURE.md` §4–§6 for the workflow lifecycle, storage model, and invariants.
