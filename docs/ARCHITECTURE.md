# Fishhawk — Architecture

> **Status:** Living document; tracks the current technical realization of the v0 spec.
> **Audience:** Agents and engineers working in this repo. Optimized for density, not narrative.
> **Authority:** `docs/MVP_SPEC.md` defines scope; this doc defines the *technical realization* of that scope. When they disagree, MVP_SPEC wins for "what" and this doc wins for "how".

## 1. System summary

Fishhawk is a **governance and workflow layer for agent-driven software changes**. A customer commits a YAML workflow spec to their repo, triggers it (issue label, CLI, UI), and the system runs the agent on their CI under typed constraints, captures a signed trace, and gates each stage on human approval. The product surface is the workflow execution and audit history; the agent itself is pluggable.

See `docs/MVP_SPEC.md` §1–§4 for product framing, primitives, and customer-side spec syntax.

## 2. Components

Five surfaces, deployed independently. Each maps to a directory in this monorepo and an epic in Project #7.

| Component | Path | Role | Epic |
|---|---|---|---|
| **Backend control plane** (`fishhawkd`) | `/backend` | Workflow state machine, policy evaluator, approval state, audit writer, REST API, GitHub App webhook receiver. | E3 (#3) |
| **Runner action** (`fishhawk/runner`) | `/runner` | GitHub Action published as `kuhlman-labs/fishhawk/runner@runner/vX.Y.Z`. Runs on the customer's CI: invokes the agent, captures trace, validates the produced plan, signs and ships the bundle. Self-execution in this repo uses `./runner` (the local path); external customers pin a release tag. Versioned, cosign-signed releases via `.github/workflows/runner-release.yml`. | E5 (#5) |
| **Web UI** | `/frontend` | Authenticated SPA — plan review, approval, audit search, run visualization. | E7 (#7) |
| **CLI** (`fishhawk`) | `/cli` | Validate workflow specs locally (`fishhawk validate`); trigger and inspect runs from the terminal. Plan review and approval explicitly stay in the UI. | E6 (#6) |
| **GitHub App** | `docs/github-app/manifest.template.json` (registered with GitHub from this template) | Per-installation tokens for repo access; OAuth provider for user sign-in; webhook source for triggers. Render via `scripts/render-github-app-manifest.sh <backend-url>`; setup docs in `docs/github-app/README.md`. | E4 (#4) |

Plus the canonical artifact, **`.fishhawk/workflows.yaml`**, which lives in the customer's repo (and in this repo for self-execution starting Day 21).

## 3. Tech stack

Consolidated from the resolved ADRs (#65–#73, #78). Each row links to the issue with full Decision and Consequences.

| Layer | Choice | ADR |
|---|---|---|
| Backend language | Go 1.22+ | MVP_SPEC §10 #1 |
| HTTP server / router | stdlib `net/http` + Go 1.22 method-aware `ServeMux` | session decision (CLAUDE.md) |
| Logger | `log/slog` (stdlib) with JSON handler | (E3.2 #42) |
| DB driver / access | `pgx/v5` + `sqlc` codegen; queries in `*.sql`, generated Go in `internal/<feature>/db/` | #66 |
| DB migrations | `golang-migrate/migrate` v4; `.up.sql/.down.sql` files; deploy-time application via `fishhawkd migrate` subcommand | #70 |
| Object storage | AWS S3 (prod) + MinIO via docker-compose (dev) | #67 |
| Trace bundle format | JSON Lines + gzip (`*.jsonl.gz`); one event per line with manifest + trailer | #71 |
| Trace signing | Ed25519 over `sha256(raw_bundle_bytes)`; per-run ephemeral keys, 30-min TTL | #72 |
| Cloud | AWS (RDS Postgres, S3, ECS Fargate, ALB, Secrets Manager, CloudWatch) | #65, #73 |
| Frontend framework | Vite 6 + React 19 + React Router 7 (SPA); pnpm 10 | session decision (CLAUDE.md) |
| Frontend test runner | Vitest 3 + @testing-library/react + jsdom | E7.1 (#37) |
| Frontend styling | Tailwind CSS v4 + shadcn/ui (copied components) + Radix primitives + lucide-react icons | #68 |
| Browser auth | HTTP-only `fishhawk_session` cookie (server-side state); CSRF token in `__Host-csrf` cookie for state-changing endpoints | #69 |
| CLI / API auth | Scoped opaque bearer tokens; revocable; audit-logged on issue/use/revoke | #69 (E4.5 #51) |
| Lint | golangci-lint v2 (curated preset: errcheck, govet, ineffassign, revive, staticcheck + gofmt, goimports) | #78 |
| CI | GitHub Actions; path-aware via `dorny/paths-filter`; loops `go.work` modules | #78 |

## 4. Workflow run lifecycle

Per `docs/MVP_SPEC.md` §5.2. Concrete realization in this codebase:

1. **Trigger** — GitHub issue label/assignment (webhook → backend), CLI `fishhawk run start`, or UI button. Backend validates the workflow spec at the issue's `.fishhawk/workflows.yaml` SHA, creates a `runs` row, and emits `workflow_dispatch` to the customer's repo invoking `fishhawk/runner@runner/vX.Y.Z`.
2. **Plan stage** — Runner checks out the repo, calls backend `POST /v0/runs/{id}/signing-key` (with GitHub OIDC token) and receives an Ed25519 private key + run metadata. Invokes Claude Code with the plan prompt. Captures full trace as JSON Lines events. Validates the plan artifact against `standard_v1` schema (E1.5 / #20). Signs `sha256(bundle)`. Ships `(bundle, sig)` to `POST /v0/runs/{id}/trace`.
3. **Backend ingest** — Verifies signature against the stored public key for `run_id`. Stores the bundle in S3 keyed by content sha256. Persists the plan artifact in `artifacts` and renders it as a comment on the originating GitHub issue (mode: `rendered_comment`, kept in sync). Transitions stage state.
4. **Plan approval** — Approver reads the plan in the Web UI (canonical surface) or the issue comment (read-only echo). Clicks Approve. Backend records `(approver_subject, surface, ts)` in the audit log and transitions to implement.
5. **Implement stage** — Runner re-invokes with the implement prompt + approved plan. Captures trace. Post-hoc evaluates constraints (`forbidden_paths`, `max_files_changed`, `required_outcomes`) against the produced diff. If a constraint hits, the stage fails as category B. Otherwise pushes branch, opens PR via the GitHub App's installation token, ships final trace.
6. **Review stage** — Backend awaits human approval on the PR plus blocking checks (`ci_pass`, `fishhawk_audit_complete`).
7. **Merge** — On merge, backend writes the final audit entry and closes the run.

Failure categories (per §6) are captured in the `audit_entries` table with `category` ∈ {A: agent, B: constraint, C: infra, D: approval timeout}. Re-execution is allowed for all four; idempotency keys prevent double-fire.

## 5. Storage model

### 5.1 Postgres (`fishhawkd` schema)

Designed under E2.1 (#22). Tables (immutable schema once frozen at Day 21):

- `runs` — one row per workflow execution. `(run_id, repo, trigger_source, workflow_sha, state, created_at, …)`
- `stages` — one row per stage execution. `(stage_id, run_id, kind, executor, state, started_at, ended_at)`
- `artifacts` — typed outputs. `(artifact_id, stage_id, kind, schema_version, data_jsonb, content_hash)`
- `signing_keys` — per-run ephemeral key chain. `(run_id, public_key_b64, issued_at, expires_at)`. Immutable.
- `audit_entries` — append-only event log. `(entry_id, run_id, ts, category, kind, actor_subject, payload_jsonb, prev_hash)`. Application-layer enforces no UPDATE/DELETE.
- `approvals` — `(approval_id, stage_id, approver_subject, surface, decision, ts)`.
- `sessions` — opaque session IDs for the Web UI cookie auth.
- `api_tokens` — scoped CLI/API tokens with revocation state.

Connection pool: `pgxpool.Pool` per service instance.

### 5.2 S3 (trace bundles)

- Bucket-per-environment: `fishhawk-traces-{env}`.
- Key layout: `{run_id}/redacted/{sha256}.jsonl.gz` and `{run_id}/raw/{sha256}.jsonl.gz`.
- Bucket policy denies `s3:DeleteObject` to all principals except a dedicated lifecycle-management role.
- Object Lock (Compliance Mode) on `raw/` is gated by retention SLA decisions (post-v0).
- Local dev: MinIO container at port 9000, same key layout.

### 5.3 Trace bundle format

`*.jsonl.gz`, one JSON event per line, UTF-8. Schema versioned via the manifest event's `bundle_schema` field (current: `"v1"`).

| Event kind | Position | Purpose |
|---|---|---|
| `manifest` | First line | Schema version, run/stage IDs, agent/model identity |
| `prompt` / `tool_call` / `tool_result` / `model_response` | Middle | The actual trace |
| `policy_event` / `gate_event` | Middle | Constraint hits, approval transitions |
| `error` | Middle | Errors during agent execution (category A) |
| `trailer` | Last line | `event_count`, content hash of preceding lines |

## 6. Invariants

These are load-bearing. Do not break them without explicit ADR.

1. **Customer source code never reaches Fishhawk's backend.** Code lives only in the customer's repo and on their CI runners. The backend sees traces, plans, and metadata — never source.
2. **Audit entries are append-only.** Enforced at the application layer (`audit.Append(...)`; no `Update` or `Delete`). Static-analysis test asserts no other code path mutates `audit_entries`.
3. **Trace bundles are signed by per-run ephemeral keys.** A tampered runner cannot forge a trace for any run other than its own; a leaked key expires within 30 minutes.
4. **Honest gap reporting beats fictional completeness.** A runner crash produces an audit entry that says `trace_lost`, not silent omission (MVP_SPEC §6).
5. **The workflow spec syntax is frozen at Day 21.** Old plans in the audit log remain readable forever; we never break old `standard_v1` artifacts.
6. **No founder bypass.** Methodology commitment from `docs/METHODOLOGY.md`. Emergency paths exist, are themselves audited, require post-hoc justification.

## 7. Module boundaries

The Go monorepo is a workspace, not a single module. Each top-level directory is independently taggable:

- `/backend` — `github.com/kuhlman-labs/fishhawk/backend`. Internal packages only (no exported API to other modules).
- `/runner` — `github.com/kuhlman-labs/fishhawk/runner`. The published GitHub Action artifact (composite action manifest at `runner/action.yml`). Customers pin a tag.
- `/cli` (planned) — `github.com/kuhlman-labs/fishhawk/cli`. Single binary.

Cross-module type sharing is intentionally avoided in v0. If two modules need the same struct (e.g., the `standard_v1` plan schema), the canonical source is a JSON Schema in `/docs/spec/` and each side parses independently. This keeps the runner's dependency graph small and the supply-chain surface minimal.

The frontend (`/frontend`, planned) is its own npm workspace; it talks to the backend over the REST API only.

## 8. Auth model

| Surface | Credential | Storage | Revocation |
|---|---|---|---|
| Web UI | `fishhawk_session` HTTP-only cookie | Server-side `sessions` row | Immediate (delete row) |
| Web UI (CSRF) | `__Host-csrf` cookie + `X-CSRF-Token` header | Tied to session row | With session |
| CLI / programmatic API | `Authorization: Bearer <opaque-token>` | `api_tokens` row with scopes | Immediate (mark revoked) |
| Runner → backend | GitHub OIDC token (verified per request) | Stateless | TTL ~10min from GitHub |
| Runner trace upload | Ed25519 signature with per-run key | `signing_keys` row | TTL 30min |

GitHub OAuth (E4.2 / #49) is the sign-in flow that mints the cookie session. Approvers in the workflow spec resolve to GitHub team members via the GitHub App's installation token (E4.4 / #50).

## 9. CI and release shape

- **CI workflow** at `.github/workflows/ci.yml`. Path-aware via `dorny/paths-filter`. The Go job iterates `go.work` `use` directives — adding a new module Just Works.
- **Lint config** at `/.golangci.yml` (v2 format). Shared across all Go modules in the workspace.
- **Coverage targets**, tiered to the autonomy levels in `docs/METHODOLOGY.md`:
  - **Low-autonomy code** (audit log integrity, signing/crypto, policy evaluator, run state machine, workflow spec parser): ≥ 85% statement coverage.
  - **Medium-autonomy code** (HTTP handlers, runner adapters, REST endpoints, UI logic): ≥ 75%.
  - **High-autonomy code** (docs, dep bumps, lint/format): no target.
  - **Generated code** (sqlc outputs, etc.): excluded from numerator and denominator.
  - **Aggregate floor (excluding generated): ≥ 80%.** Enforced by `scripts/check-coverage.py` in the Go CI job; PRs that drop below fail `CI Pass`. Run locally with `(cd backend && go test -race -coverprofile=coverage.out -covermode=atomic ./...) && python3 scripts/check-coverage.py --threshold 80 --exclude internal/run/db backend/coverage.out`.
- **Release**: each module is tagged independently. The runner is the customer-facing one — `kuhlman-labs/fishhawk/runner@v0.1` etc. — built with signed releases + SBOM (E5.7 / #54, E13.6 / #63).

## 10. Where to look

| Question | Look here |
|---|---|
| What does v0 ship? | `docs/MVP_SPEC.md` §9, §13 |
| Why a decision was made | The corresponding closed ADR issue (`gh issue list --label adr --state closed`) |
| Voice / naming for new surfaces | `docs/BRAND_FOUNDATIONS.md` §5–§7 |
| Autonomy tier of a change | `docs/METHODOLOGY.md` |
| Workflow spec grammar | `docs/spec/workflow-v0.md` + `docs/spec/workflow-v0.schema.json` |
| Plan artifact structure | `docs/spec/plan-standard-v1.md` + `docs/spec/plan-standard-v1.schema.json` |
| HTTP API contract (endpoints, auth, errors) | `docs/api/v0.openapi.yaml` (source of truth) + `docs/api/v0.md` (companion) |
| Trace bundle wire format (`*.jsonl.gz`) | `runner/internal/bundle/bundle.go` (pack + open) — implements ADR-007 (#71) |
| Runner → backend trace upload | `runner/internal/upload/` (HTTP client, retries, signing) — wired into runner main behind `--upload-trace` |
| CLI → backend HTTP client | `cli/internal/httpclient/` (typed wrappers); CLI subcommands in `cli/cmd/fishhawk/` |
| Constraint evaluation (forbidden_paths, max_files_changed, required_outcomes) | `runner/internal/constraint/constraint.go` (runner-side, immediate feedback to agent); `backend/internal/policy/` (backend-side, source of truth, emits chained `policy_evaluated` audit entry). Wired into trace ingest: runner emits a `git_diff` event in the bundle; trace handler calls `bundle.ExtractDiff` + `policy.EmitEvaluation`; violations transition the stage to `failed-B` instead of `awaiting_approval`. |
| Trace bundle reader (backend) | `backend/internal/bundle/`: `ReadEvents` parses gzipped JSONL bundle bytes; `ExtractDiff` returns the policy.Diff carried in the runner's `git_diff` event. Hand-rolled rather than importing `runner/internal/bundle` because the modules are separate; the read-side is small enough that duplication beats promoting bundle to a shared module. |
| HTTP middleware order / context keys | `backend/internal/server/middleware.go` |
| Run CRUD handlers (POST/GET/list/cancel) | `backend/internal/server/runs.go`; wired in `backend/cmd/fishhawkd/serve.go` from `FISHHAWKD_DATABASE_URL`. POST accepts `Idempotency-Key` (E8.2): same `(repo, key)` returns the existing run with 200 instead of creating a duplicate. Webhook-driven runs use the dedicated dedup path (E3.9) and don't carry a key. |
| Stage + audit read handlers (`/runs/{id}/stages`, `/runs/{id}/audit`) | `backend/internal/server/reads.go`; cursor pagination via `pageOffset`/`encodeOffsetCursor` |
| Signing-key issuance handler | `backend/internal/server/signing.go` wraps `signing.Repository.Issue`; OIDC auth via `backend/internal/githuboidc/` when `--oidc-audience` is set (canonical JWKS at `https://token.actions.githubusercontent.com/.well-known/jwks`, RS256 verify, claim binding to run's `repo` + `workflow_id`). Unauthenticated when audience unset (v0 self-execution fallback). |
| Trace upload handler | `backend/internal/server/trace.go`; verifies signature, calls `tracestore.Put` + `audit.AppendChained`. S3 wired in `serve.go` from `FISHHAWKD_S3_BUCKET`/`_REGION`/`_ENDPOINT`. |
| Per-stage prompt construction | `backend/internal/prompt/Build` (pure, by stage type); served at `GET /v0/stages/{id}/prompt` from `backend/internal/server/prompt.go`. Signed canonical message: `sha256("prompt:" + stage_id)`. Runner-side: `runner/internal/upload.FetchPrompt` + `--fetch-prompt` flag in `runner/cmd/fishhawk-runner/main.go` writes the prompt to a temp file before agent invocation. The signing-key endpoint is one-shot per run, so the runner reuses the key issued at fetch-prompt time for the trace upload. |
| GitHub webhook receiver | `backend/internal/webhook/` (HMAC + dedup) and `backend/internal/server/webhook.go`; secret from `FISHHAWKD_GITHUB_WEBHOOK_SECRET`. Dedup is `webhook.PostgresStore` (table `webhook_deliveries`) when a DB pool is wired; falls back to `webhook.MemoryStore` only when no DB is configured (NOT safe for multi-instance — logs a warning). 24h retention; 1h eviction tick on the Postgres path. |
| Webhook event dispatcher (events → runs + stages) | `backend/internal/webhook/dispatcher.go` (`MatchEvent` pure + `Dispatcher.Handle` orchestrator); wired via `cfg.WebhookDispatcher`. Creates one `Stage` row per spec-stage definition; first stage transitions to `dispatched` on workflow_dispatch. |
| Approval state management (`POST /v0/stages/{id}/approvals`) | `backend/internal/approval/` + `backend/internal/server/approvals.go`. approve → succeeded; reject → failed-D. Idempotent on (stage_id, approver_subject). SLA timeout via the ticker below. Role-based authorization via `RoleResolver` (E4.4): the subject must be in the gate's `approvers` after expanding `@org/team` refs from the spec's `roles:` map. Falls back to "any authenticated subject" when no resolver is wired. |
| Role resolution for approver checks | `backend/internal/role/`: `Resolver.ExpandRole` (role name → GitHub-login allowlist) and `CanApprove(any_of/all_of, subject)`. Per-team membership cached with a default 5-minute TTL; `Invalidate(org, slug)` bypasses TTL on explicit role-change events. `githubclient.ListTeamMembers` paginates the GitHub team-members endpoint. |
| Scoped API tokens for CLI / UI auth | `backend/internal/apitoken/` (Issue / Authenticate / Revoke / List + sha256-hashed storage). Bearer-aware middleware in `backend/internal/server/middleware.go` resolves `Authorization: Bearer fhk_…` to an `Identity` (subject + token id + scopes); absent / invalid bearer falls back to anonymous and per-handler logic decides. `/v0/tokens` endpoints in `backend/internal/server/tokens.go`; issue/revoke append to the global audit chain. Bootstrap via `fishhawkd token issue --subject <s>` — talks to the DB directly to break the chicken-and-egg of "you need a token to mint a token." |
| GitHub OAuth sign-in (E4.2) | `backend/internal/auth/`: `GitHubOAuth` wraps the authorize/token/user endpoints; `Repository` upserts the `users` row + creates a `sessions` row whose hash is stored. Cookies per ADR-005: `fishhawk_session` is HttpOnly + Secure + SameSite=Lax, sliding 24h / absolute 7d. The auth middleware resolves the cookie to an `Identity` carrying `Subject="github:<login>" + UserID + SessionID`; cookie auth is tried before bearer so a browser carrying both prefers the user-bound credential. Handlers: `/v0/auth/github/login`, `/v0/auth/github/callback`, `/v0/auth/me`, `/v0/auth/logout`. Configured via `--oauth-client-id` / `--oauth-client-secret` / `--oauth-callback-url`. |
| Global audit chain for non-run events | `audit_entries.run_id` is nullable (migration 0009). `audit.AppendGlobalChained` writes to the `WHERE run_id IS NULL` partition with its own prev_hash chain, independent of per-run chains. Used today for token issue/revoke; ready for OAuth events (E4.2) and GitHub App install/uninstall (E4.1). Verifier (`verifier/internal/audit`) handles nullable RunID in `HashInputs` so the canonical hash algorithm covers both partitions. |
| Approval SLA timeout ticker | `backend/internal/sla/`: `Parse` (`<n>_<unit>` → `time.Duration`; `business_hours` aliased to wall-clock hours in v0) + `Ticker` (background goroutine; lists `awaiting_approval` stages with non-null `gate_sla`, fails-D + chains an `approval_sla_elapsed` audit entry once the deadline passes). Off by default; enable with `--enable-sla-timer` (or `FISHHAWKD_ENABLE_SLA_TIMER=true`). Scan interval via `--sla-interval`, default 60s. Dispatcher persists the gate's SLA string to `stages.gate_sla` at create time. |
| Dispatch watchdog (category-C) | `backend/internal/dispatchwatchdog/`: `Ticker` walks `dispatched`-state stages whose `UpdatedAt` is past `--dispatch-watchdog-timeout` and fails them as category C ("infrastructure failure" — runner action timed out, GitHub-side dispatch failure, network partition). Mirrors the SLA ticker pattern: `FailStage(stageID, FailureC, …)` plus a chained `dispatch_watchdog_elapsed` audit entry. Off by default; enable with `--enable-dispatch-watchdog`. Default timeout 1h covers GitHub Actions dispatch + queue + first checkin. Closes the C-emitter half of [#158](https://github.com/kuhlman-labs/fishhawk/issues/158); the A-emitter (runner-side `agent_failed` flag in the trace bundle) is the remaining half. |
| Hosted infrastructure (Terraform) | `infra/terraform/` per [ADR-016 (#165)](https://github.com/kuhlman-labs/fishhawk/issues/165) — Terraform 1.5+ with the AWS provider 5.x. Foundation: VPC + 2-AZ subnets + IGW + single NAT, security groups (ALB → app → RDS chain), IAM (ECS task / execution roles + GitHub Actions OIDC role), Secrets Manager skeletons, CloudWatch log group. ECS slice (E13.7.2 / [#166](https://github.com/kuhlman-labs/fishhawk/issues/166)): Fargate cluster + task definition pointing at `ghcr.io/kuhlman-labs/fishhawkd:<image_tag>`, ECS service across both private subnets with rolling-deploy + circuit-breaker rollback, ALB + target group with `/healthz` health checks, HTTP listener (forward-only when no domain set, redirect-to-HTTPS otherwise), optional ACM cert + Route 53 alias + HTTPS listener gated on `var.domain_name`. State backend is S3 + DynamoDB lock table, bootstrapped out-of-band per `infra/terraform/README.md`. RDS + migration runner (slice 3 / [#167](https://github.com/kuhlman-labs/fishhawk/issues/167)) and the deploy workflow (slice 4 / [#168](https://github.com/kuhlman-labs/fishhawk/issues/168)) land in subsequent PRs. |
| Stage orchestrator (next-stage dispatch after approve) | `backend/internal/orchestrator/`; called from approval handler on approve. Dispatches the next pending stage (or transitions Run to terminal when all stages are done). Agent stages fire `workflow_dispatch`; human stages walk to `awaiting_approval` directly. |
| Stage detail + artifact reads | `backend/internal/server/reads.go`: `GET /v0/stages/{id}`, `GET /v0/stages/{id}/artifacts`, `GET /v0/artifacts/{id}` |
| GitHub App installation tokens | `backend/internal/githubapp/` (RS256 signer + client + TTL cache + telemetry); App ID + key file from `FISHHAWKD_GITHUB_APP_ID` / `FISHHAWKD_GITHUB_APP_PRIVATE_KEY_FILE` |
| GitHub REST operations (read workflow spec, fire workflow_dispatch) | `backend/internal/githubclient/`; consumes `githubapp.TokenProvider` |
| How a new Go module gets added | `CLAUDE.md` "Adding a Go module" |
| `fishhawkd` container image | `backend/Dockerfile` (multi-stage → distroless static, ~28 MB; `-X version.Version` stamped from `VERSION` build-arg). `.github/workflows/backend-build.yml` builds + pushes `ghcr.io/kuhlman-labs/fishhawkd:main` and `:sha-<commit>` on every push to `main`; `.github/workflows/backend-release.yml` fires on `backend/v*` tags, attaches an SPDX-JSON SBOM, cuts a GitHub Release. Both are signed keylessly via cosign + GHA OIDC; verify with the regex in `.github/release-notes/backend.md`. ECS task-definition + IAM scaffolding (per ADR-009) tracked separately in [#148](https://github.com/kuhlman-labs/fishhawk/issues/148). |
| Frontend scaffold + dev loop | `frontend/` is a pnpm package, decoupled from `go.work`. Entry `src/main.tsx` mounts `<App />` inside `<BrowserRouter>`; route table in `src/App.tsx`. Tailwind v4 via `@tailwindcss/vite` (config-as-CSS in `src/index.css`); shadcn/ui components live under `src/components/ui/` and are copied (not installed) per ADR-004. ESLint flat config + Prettier; Vitest + jsdom for tests. CI's TS lane (`.github/workflows/ci.yml`) runs `pnpm install --frozen-lockfile`, `format:check`, `lint`, `typecheck`, `test`, `build` against `frontend/`, path-filtered to `frontend/**`. Vite dev server proxies `/v0` → `localhost:8080` so the session cookie is same-origin during `pnpm dev`. |
| Frontend auth gate | `frontend/src/auth/`: `auth-context.ts` (the React context, isolated from any component file so Fast Refresh's one-export-per-file rule isn't tripped), `auth-provider.tsx` (fetches `GET /v0/auth/me` on mount with `credentials: 'include'`), `require-auth.tsx` (redirects to `/login` while unauthenticated; renders a "Checking session…" placeholder during load so deep-link refresh doesn't flash). Login uses `Button asChild` around `<a href="/v0/auth/github/login">` so the browser follows the 302; `useAuth().signOut()` POSTs `/v0/auth/logout` and drops local state even on network failure. CSRF protection on POSTs (per ADR-005) is deferred to [#152](https://github.com/kuhlman-labs/fishhawk/issues/152). |
| Frontend API client | `frontend/src/api/`: `types.ts` mirrors the OpenAPI Run/Stage/Artifact/PaginatedList shapes, `plan.ts` mirrors the `standard_v1` plan schema and exposes a narrow `isStandardV1Plan` guard, `client.ts` wraps `fetch` with `credentials: 'include'` + JSON parsing + `ApiClientError` on non-2xx, `use-async.ts` is a 30-line loader hook (no caching / retries — each route mount fetches fresh; reach for TanStack Query when shared cache or invalidation is needed). |
| Plan review surface | `frontend/src/plan/plan-document.tsx` renders a `standard_v1` plan as a structured document with side-nav anchors per Brand Foundations §6 ("plans are documents, not chat"). Section primitives in `sections.tsx` — Ticket, Generated by, Summary, Scope, Approach, Verification, Risks (optional). Routes: `/runs` (`routes/runs.tsx`) → `/runs/:id` (`routes/run-detail.tsx`) → `/runs/:id/stages/:sid` (`routes/stage-detail.tsx`); the last fetches the most-recent `kind=plan, schema_version=standard_v1` artifact and hands its `content` to `<PlanDocument>`. Older / unknown schema versions render a labelled warning rather than guessing. |
| Approval action | `frontend/src/plan/approval-panel.tsx` is a two-step idle → confirming → submitting state machine. Approve / Reject open an inline confirm panel with an optional comment textarea; the second click POSTs `/v0/stages/{id}/approvals`. Optimistic update applies immediately (approve → succeeded, reject → failed-D); on backend error the parent rolls back via the `onRollback` callback and the panel surfaces the error inline. Stage state lives in `routes/stage-detail.tsx` so the loader stays the source of truth. Regenerate button renders disabled until E8.3 (#146) lands re-execution. |
| Failure taxonomy | `backend/internal/run/run.go`'s `FailureCategory` type carries the four MVP_SPEC §6 categories (A=agent, B=constraint/policy, C=infra, D=approval timeout/rejection) with `Valid()` + `Description()` methods. `backend/internal/run/failure.go`'s `FailStage(ctx, repo, stageID, cat, reason)` helper is the single transition entry point — walks `dispatched → running → failed` when needed, idempotent on already-failed stages. Used by `server.failStageCategoryB` (trace path), `server.advanceStage` (approvals reject), `sla.handleStage` (SLA elapse). Frontend mirrors the descriptions in `frontend/src/api/types.ts` (`FAILURE_DESCRIPTIONS`, `describeFailure`); rendered by `<FailureBanner>` (`frontend/src/components/failure-banner.tsx`) above the stage detail and as a category badge next to failed stages on the run-detail list. Categories A and C have no emitter yet — tracked in [#158](https://github.com/kuhlman-labs/fishhawk/issues/158). |
| CSRF enforcement | `backend/internal/server/csrf.go` ships the double-submit pattern per ADR-005. The OAuth callback (`server.handleGitHubCallback`) mints a 32-byte hex token and sets it in the `__Host-csrf` cookie alongside `fishhawk_session`; logout clears both. The `csrf` middleware sits after `bearerAuth` in the chain (`recovery → requestID → logging → bearerAuth → csrf → mux`) and enforces `X-CSRF-Token` ≡ `__Host-csrf` on POST/PUT/PATCH/DELETE for session-cookie identities only — bearer-token clients (CLI, server-to-server) and GET-style methods bypass; safe-listed paths (`/v0/auth/github/*`, `/webhooks/github`) bypass too. Mismatch returns `403 csrf_required`. Frontend's `frontend/src/api/client.ts` reads the cookie via `getCookie()` (`frontend/src/lib/cookie.ts`) and auto-attaches the header on every state-changing call. Vitest runs jsdom under `https://localhost/` so `__Host-` cookies are accepted (jsdom rejects them under HTTP). |
| Post-sign-in redirect intent | `<RequireAuth>` (`frontend/src/auth/require-auth.tsx`) captures `location.pathname + location.search` and forwards it to `/login` as `?next=…` when an unauthenticated visitor hits a deep link. The Login route (`routes/login.tsx`) reads `next` and appends it to `/v0/auth/github/login?next=…`. Backend stashes the value in a short-lived `fishhawk_oauth_next` cookie at login (`server.handleGitHubLogin`) — only after `isSafeRelativeRedirect` passes, dropping anything that looks like an absolute URL, scheme-relative URL, or `/\…` Windows-path fragment. Callback (`server.handleGitHubCallback`) reads the cookie, re-validates (defense in depth), uses it as the redirect target, and clears the cookie. Tampered or malformed values fall back to the operator-configured default. Constants in `backend/internal/auth/auth.go`. |
| Per-run audit log on the run page | `frontend/src/routes/audit-list.tsx` (`<RunAuditList>`) renders entries returned by `GET /v0/runs/{id}/audit` as a dense table — sequence, category, actor, timestamp, truncated entry hash. Mounted at the `#audit` anchor inside `<RunDetail>`; the plan-document approval panel links to `/runs/:id#audit` so reviewers can verify the chain right next to the action they took. |
| Cursor pagination | `frontend/src/api/use-paginated.ts` (`usePaginated`) owns a current cursor + history stack so callers can step forward via `next_cursor` and back via the remembered prior cursors (the v0 cursor format is opaque + non-reversible — there's no `prev_cursor`). `frontend/src/components/pagination.tsx` (`<Pagination>`) is the Prev / Next + 1-based page indicator. Wired into `<Runs>` and `<RunAuditList>`; both call with `limit=50`. Page state lives in component state — not in the URL — so deep-linking lands on the first page; URL-state encoding is a future-when-shareability-matters change. |

## 11. Open work

What's not yet decided or implemented at the time of writing:

- ADR-010 (#74) marketplace billing — Day 45
- ADR-011 (#75) pricing model — Day 60
- ADR-012 (#76) design partner sourcing — Day 30
- ADR-013 (#77) Apache/BSL boundary — Day 60
- ADR-015 (#79) Slack notification approach — Day 21

The Day 21 self-execution milestone (E14 / #14) is the gating event: when Fishhawk first ships its own PR through Fishhawk, the workflow spec syntax and the `standard_v1` plan schema freeze.
