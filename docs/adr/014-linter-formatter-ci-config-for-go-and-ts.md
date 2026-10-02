---
id: ADR-014
title: "Linter/formatter/CI config for Go and TS"
status: accepted
date: 2026-04-30
issue: https://github.com/kuhlman-labs/fishhawk/issues/78
supersedes: []
superseded_by: []
applies_to: [".golangci.yml", ".github/workflows/ci.yml", "go.work"]
---

# ADR-014: Linter/formatter/CI config for Go and TS

## Context

Lint, format, and CI need to be set up before the first Go file is written; otherwise the team accumulates inconsistencies that are painful to retrofit. Solo founder, but the project is public and contributors will appear.

## Options

- **Go**: `gofmt` + `golangci-lint` (with a vetted preset like `revive` + `staticcheck` + `errcheck`). Industry standard.
- **TypeScript**: `eslint` + `prettier`. Optional `typescript-eslint` for typed-rule extras.
- **CI**: GitHub Actions, single workflow with path-aware jobs (Go-only changes don't run TS jobs).

## Recommendation

Go: gofmt + golangci-lint with a curated preset. TS: eslint + prettier. CI: single GitHub Actions workflow with path filters per language.

## Decision

**Recorded 2026-04-30.**

- **Go module layout:** Multiple modules (`/backend`, `/cli`, `/runner`) tied together by `go.work` at the repo root. Each module is independently taggable, which matters for `kuhlman-labs/fishhawk/runner@vX.Y` per MVP_SPEC §5.1.2.
- **Go lint:** `golangci-lint` with a curated preset — `errcheck`, `govet`, `ineffassign`, `revive`, `staticcheck`, `gofmt`, `goimports`. Single config at the repo root (`.golangci.yml`); applies to every Go module via `go.work`.
- **Go format:** `gofmt` + `goimports`, enforced via the linter step.
- **Go version:** 1.22 (the version that landed method-aware `ServeMux`).
- **TypeScript:** specifics deferred until E7.1 (#37) scaffolds `/frontend`. ESLint flat config + Prettier is the intended direction.
- **CI:** single GitHub Actions workflow at `.github/workflows/ci.yml`. Path-aware via `dorny/paths-filter`: Go-only PRs don't run TS jobs, and vice versa. Until any Go module is registered in `go.work`, the Go job no-ops cleanly.
- **Pre-commit hooks:** deferred. CI is the floor; per-developer hooks are a nice-to-have once contributor volume warrants.

## Consequences

**Easier**
- E3.1 (#41), E5.1 (#52), E6.1 (#55) — each just adds `use ./<dir>` to `go.work` and a `go.mod` in its directory. Lint and CI immediately apply.
- E7.1 (#37) — frontend scaffold lands ESLint + Prettier under `/frontend`; the existing path filter routes its work to the (currently stub) TS job.
- The runner can be tagged independently as `kuhlman-labs/fishhawk/runner@v0.1` without dragging the backend or CLI into its module graph.

**Harder**
- Cross-module type sharing requires either a shared module (e.g., `pkg/spec`) or duplication. Acceptable; the v0 boundaries are clean enough that this won't bite often.
- `go.work` is committed (override of the default `.gitignore` template), so any developer-local workspace tweaks need to be done in a separate workspace file.

**Other decisions this constrains**
- Future linters get added to `.golangci.yml` here, not in per-module configs, unless there's a strong reason to diverge.
- Pre-commit hooks (if added later) should mirror the CI lint step exactly so behaviour matches.

## Spec reference

(no spec deferral — this is a foundational engineering choice picked at Day 1)

## Target deadline

Day 1 — **met**.

## Files landed

- `.editorconfig` — base editor consistency
- `.gitignore` — adjusted to commit `go.work`, ignore `node_modules` etc.
- `go.work` — empty workspace, ready for module `use` directives
- `.golangci.yml` — curated linter preset
- `.github/workflows/ci.yml` — path-aware CI workflow

---
Parent epic: #15
