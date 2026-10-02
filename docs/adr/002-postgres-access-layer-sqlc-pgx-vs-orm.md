---
id: ADR-002
title: "Postgres access layer (sqlc + pgx vs. ORM)"
status: accepted
date: 2026-04-30
issue: https://github.com/kuhlman-labs/fishhawk/issues/66
supersedes: []
superseded_by: []
applies_to: ["backend/internal/*/queries.sql", "backend/internal/*/db/**", "backend/sqlc.yaml"]
---

# ADR-002: Postgres access layer (sqlc + pgx vs. ORM)

## Context

The Go backend needs a way to talk to Postgres. The audit log is the central artifact; queries on it must be fast, schema must be auditable, and there must be no surprising abstraction layer between the code and the data.

## Options

- **`sqlc` + `pgx`** — schema-first; queries written as raw SQL, generator produces typed Go. No runtime ORM. Fits the "auditable, restrained" voice and avoids ORM-induced surprises.
- **`ent`** — code-first ORM, strong type system, schema migrations bundled. Good for rapid CRUD; opinionated. Heavier abstraction than ideal for an audit-of-record service.
- **`gorm`** — most popular Go ORM. Reflective, magical. Not recommended given the audit-integrity stakes.
- **Hand-rolled `database/sql` with `pgx` driver** — no codegen, no ORM. Maximum control, more boilerplate.

## Recommendation

`sqlc` + `pgx`. Codegen + raw SQL is the sweet spot for an audit-grade service: queries are visible in the repo, types are checked, no runtime magic.

## Decision

**Recorded 2026-04-30: sqlc + pgx (jackc/pgx/v5).**

- Queries live in `backend/internal/<feature>/queries.sql`. `sqlc generate` writes typed Go into `backend/internal/<feature>/db/`.
- Connection pooling via `pgxpool`.
- No ORM at runtime.
- `sqlc.yaml` lives at `/backend/sqlc.yaml` (single config covering all features).
- The generated code is committed (so consumers can read the diff in PRs without running codegen locally).

## Consequences

**Easier**
- Every database query is visible in `*.sql` files; reviewers can read what hits the database without expanding ORM call chains.
- Type-checked at compile time: a schema migration that breaks a query produces a build failure.
- pgx's audit-friendly defaults (statement timeouts, prepared statement cache) align with audit-grade requirements.

**Harder**
- Cross-cutting query helpers (e.g., generic pagination) are slightly more verbose. Acceptable.
- Schema changes need both a migration and re-running `sqlc generate`. Add a CI check that the generated code is in sync (run `sqlc diff`).

**Other decisions this constrains**
- ADR-006 (migration tool) was decided independently; the migration runner doesn't share state with sqlc.
- Read-only replicas (if needed in v1+) drop in by adding a second `pgxpool.Pool`.

## Target deadline

Day 3 — **met**.

---
Parent epic: #15
