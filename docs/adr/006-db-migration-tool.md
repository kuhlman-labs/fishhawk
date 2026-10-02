---
id: ADR-006
title: "DB migration tool"
status: accepted
date: 2026-04-30
issue: https://github.com/kuhlman-labs/fishhawk/issues/70
supersedes: []
superseded_by: []
applies_to: ["backend/internal/db/migrations/**"]
---

# ADR-006: DB migration tool

## Context

Postgres schema needs versioned migrations. Tool choice affects developer ergonomics, CI integration, and how easy it is to inspect migration history.

## Options

- **`golang-migrate/migrate`** — long-standing, file-based up/down SQL migrations, simple CI integration. Most common Go default.
- **`atlas`** — declarative schema management; can apply diffs. Newer but well-engineered. Higher learning curve.
- **`goose`** — file-based, supports Go migrations as well as SQL. Smaller community than `migrate`.
- **`pgx` + sqlc + custom runner** — bare-metal, no migration framework.

## Recommendation

`golang-migrate/migrate`. Boring, widely understood, easy to inspect.

## Decision

**Recorded 2026-04-30: `golang-migrate/migrate` v4.**

- Migrations live in `backend/internal/db/migrations/NNNN_<description>.{up,down}.sql`.
- Numeric prefix is sequential (`0001_…`, `0002_…`, etc.) — easier to read and grep than timestamps.
- The migration tool runs at deploy time via the backend container (`fishhawkd migrate` subcommand) — not via a separate CI step. This guarantees the running binary's expected schema matches the actual schema.
- Down migrations are written but not exercised in production; they exist for local dev tearing-down. Production rollback is via forward-only migrations that revert behavior.

## Consequences

**Easier**
- Migrations are plain SQL files in version control: any contributor can read them without learning a DSL.
- The deploy-time application means a deploy that requires a migration is atomic with the schema change. No half-migrated states.
- Tooling integrations are excellent: `migrate` CLI, library mode, IDE syntax highlighting on `.sql` files.

**Harder**
- Rolling back a migration in production requires a forward-only revert migration (no `migrate down`). That's a discipline cost, not a tooling cost.
- `migrate` doesn't support diffing the schema against a target state — useful for catching drift, but not a v0 need.

**Other decisions this constrains**
- E2.1 (#22) Postgres schema design uses this layout.
- E13.2 (#59) provisioning includes the deploy-time migration step.
- The backend exposes a `migrate` subcommand (post-E3.2 since it shares config with the server).

## Target deadline

Day 3 — **met**.

---
Parent epic: #15
