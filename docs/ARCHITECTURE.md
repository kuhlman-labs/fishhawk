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
| **Runner action** (`fishhawk/runner`) | `/runner` | GitHub Action published as `kuhlman-labs/fishhawk/runner@vX.Y`. Runs on the customer's CI: invokes the agent, captures trace, validates the produced plan, signs and ships the bundle. Currently a scaffold (E5.1) — flag parsing only, no agent invocation yet. | E5 (#5) |
| **Web UI** | `/frontend` (planned) | Authenticated SPA — plan review, approval, audit search, run visualization. | E7 (#7) |
| **CLI** (`fishhawk`) | `/cli` (planned) | Validate workflow specs locally; trigger and inspect runs from the terminal. Plan review and approval explicitly stay in the UI. | E6 (#6) |
| **GitHub App** | (registered with GitHub; manifest in repo) | Per-installation tokens for repo access; OAuth provider for user sign-in; webhook source for triggers. | E4 (#4) |

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
| Frontend framework | Vite + React Router (SPA) | session decision (CLAUDE.md) |
| Frontend styling | Tailwind CSS v4 + shadcn/ui (copied components) + Radix primitives + lucide-react icons | #68 |
| Browser auth | HTTP-only `fishhawk_session` cookie (server-side state); CSRF token in `__Host-csrf` cookie for state-changing endpoints | #69 |
| CLI / API auth | Scoped opaque bearer tokens; revocable; audit-logged on issue/use/revoke | #69 (E4.5 #51) |
| Lint | golangci-lint v2 (curated preset: errcheck, govet, ineffassign, revive, staticcheck + gofmt, goimports) | #78 |
| CI | GitHub Actions; path-aware via `dorny/paths-filter`; loops `go.work` modules | #78 |

## 4. Workflow run lifecycle

Per `docs/MVP_SPEC.md` §5.2. Concrete realization in this codebase:

1. **Trigger** — GitHub issue label/assignment (webhook → backend), CLI `fishhawk run start`, or UI button. Backend validates the workflow spec at the issue's `.fishhawk/workflows.yaml` SHA, creates a `runs` row, and emits `workflow_dispatch` to the customer's repo invoking `fishhawk/runner@vX.Y`.
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
| Constraint evaluation (forbidden_paths, max_files_changed, required_outcomes) | `runner/internal/constraint/constraint.go` (post-hoc, runner-side) |
| HTTP middleware order / context keys | `backend/internal/server/middleware.go` |
| Run CRUD handlers (POST/GET/list/cancel) | `backend/internal/server/runs.go`; wired in `backend/cmd/fishhawkd/serve.go` from `FISHHAWKD_DATABASE_URL` |
| Stage + audit read handlers (`/runs/{id}/stages`, `/runs/{id}/audit`) | `backend/internal/server/reads.go`; cursor pagination via `pageOffset`/`encodeOffsetCursor` |
| Signing-key issuance handler | `backend/internal/server/signing.go` wraps `signing.Repository.Issue`; OIDC auth pending (#112) |
| Trace upload handler | `backend/internal/server/trace.go`; verifies signature, calls `tracestore.Put` + `audit.AppendChained`. S3 wired in `serve.go` from `FISHHAWKD_S3_BUCKET`/`_REGION`/`_ENDPOINT`. |
| GitHub webhook receiver | `backend/internal/webhook/` (HMAC + dedup) and `backend/internal/server/webhook.go`; secret from `FISHHAWKD_GITHUB_WEBHOOK_SECRET` |
| Webhook event dispatcher (events → runs) | `backend/internal/webhook/dispatcher.go` (`MatchEvent` pure + `Dispatcher.Handle` orchestrator); wired via `cfg.WebhookDispatcher` |
| GitHub App installation tokens | `backend/internal/githubapp/` (RS256 signer + client + TTL cache + telemetry); App ID + key file from `FISHHAWKD_GITHUB_APP_ID` / `FISHHAWKD_GITHUB_APP_PRIVATE_KEY_FILE` |
| GitHub REST operations (read workflow spec, fire workflow_dispatch) | `backend/internal/githubclient/`; consumes `githubapp.TokenProvider` |
| How a new Go module gets added | `CLAUDE.md` "Adding a Go module" |

## 11. Open work

What's not yet decided or implemented at the time of writing:

- ADR-010 (#74) marketplace billing — Day 45
- ADR-011 (#75) pricing model — Day 60
- ADR-012 (#76) design partner sourcing — Day 30
- ADR-013 (#77) Apache/BSL boundary — Day 60
- ADR-015 (#79) Slack notification approach — Day 21

The Day 21 self-execution milestone (E14 / #14) is the gating event: when Fishhawk first ships its own PR through Fishhawk, the workflow spec syntax and the `standard_v1` plan schema freeze.
