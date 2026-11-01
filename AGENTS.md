# AGENTS.md

Fishhawk is a multi-module Go workspace with a Web UI (`frontend/`) and public
documentation site (`site/`). `.fishhawk/workflows.yaml` is the live development
workflow. Inspect the checkout before assuming a feature is absent or complete.

## Working rules

- Follow the issue → branch → PR workflow. Resolve blocking ADRs first; use a
  Fishhawk workflow run for repository changes. Operator-authored instruction,
  skill and CI changes cannot be delegated to an implement stage whose paths
  forbid them. Preserve the human approval gate on the human-led route.
- Branch from `origin/main`; use an issue/description branch name. Preserve other
  work. Commit with `git commit -s` (DCO), an imperative title, and no
  conventional-commits prefix. PR bodies use `## Summary`, `## Test plan`, optional
  `## Notes`, and `Closes #<issue>`.
- Update affected documentation and generated surfaces in the same PR. File
  deferred work before opening the PR and link it in Notes. After merge, update
  affected parent/sibling issues. Details: [contributing](docs/agents/contributing.md).
- Never force-push `main`. Use `--force-with-lease` on a feature branch; ask first
  if it has review comments.
- Do not restart the shared stack or pull its checkout during live runs or
  blocked decompositions. Use the local-operation skills and their run-agent /
  restart-blocker guards. Never bypass a refusal as a routine retry.
- Never clear the Go build cache beside a live build. Use guarded
  `scripts/dev gocache --trim`; set `GOFLAGS=-trimpath` in operator scratch
  worktrees so they share build-cache entries.

## Build, test, lint

There is **no root `go.mod`**. Use the workspace wrapper or a module-qualified
package, such as `go build ./backend/...`; root `go build ./...` fails.

```sh
scripts/test single -run TestName ./backend/internal/version/  # focused test
scripts/verify-change                   # frontend when relevant, then full verify
scripts/test verify --packages backend/internal/foo           # iteration only
scripts/test verify --no-tests          # excludes tests and patch coverage
scripts/test coverage                   # aggregate coverage
```

Use the `verify-gate` skill for preflights and results. **Never start a concurrent
verify or retry/poll one that refuses** (exit 73): use a focused `scripts/test
single` instead. Scoped verify skips patch coverage; final verification must be
full. A host-toolchain mismatch is an environment failure: use the pinned tools
or the gate image, not code changes. Diagnose unexplained failures before
changing unrelated code; report all failing modules and any skipped checks.
The [verification guide](docs/agents/verification.md) routes the gate contracts.

For `frontend/`, use `frontend-check` before push. For `site/`, read
[site/README.md](site/README.md). Tool versions come from the checked-in pins,
not remembered versions. CI-fetched executable tooling must use immutable
versions/tags; GitHub Action floating major refs are the documented exception.

## Read for the task

Read the owning package README (and the topic it indexes) when changing its
behavior. Follow the relevant route below; do not load every reference up front.
For cross-component design, read the relevant architecture sections and settled
ADRs before choosing boundaries.

| Task | Reference / skill |
|---|---|
| Scope and architecture | [MVP scope](docs/MVP_SPEC.md) (cite section numbers), [architecture](docs/ARCHITECTURE.md) §§2–9; §10 locates owning packages |
| Decisions and autonomy | [ADR format/index](docs/adr/README.md), [methodology](docs/METHODOLOGY.md); an unrecorded ADR is read from the tracker |
| Drive runs, reviews or campaigns | [operating model](docs/OPERATING_MODEL.md), [grooming runbook](docs/GROOMING_RUNBOOK.md), Fishhawk MCP runbook |
| Documentation, public site, generated reference | [documentation rules](docs/agents/documentation.md) |
| Workflow/plan schema or REST API | `spec-change`; [change checklists](docs/agents/change-checklists.md), [workflow reference](docs/spec/workflow-v2.md), [API companion](docs/api/v0.md) |
| Handler authorization or audit category | [change checklists](docs/agents/change-checklists.md): token-impact inventory, migration, category registry and comment surfaces |
| New Go module | `new-go-module`; [change checklists](docs/agents/change-checklists.md) |
| Tests, fixtures, cross-module wire types | [testing rules](docs/agents/testing.md) |
| GitHub Actions, CI or publishing | [verification guide](docs/agents/verification.md) § Required GitHub Actions validation; [scripts contract](scripts/README.md) |
| Verify / unexplained red | `verify-gate` / `diagnose-red`; [verification guide](docs/agents/verification.md) |
| Hosted Azure pilot | [Azure bootstrap, costs, promotion and recovery](infra/terraform/README.md); operator commissioning #4903 |
| Deploy, sync, stop, clean | `deploy-local`, `sync-main`, `teardown-local`, `dev-clean`; [operations](docs/agents/operations.md) |
| Ratified ADR | `record-adr`; update record, README table, index and supersession/amendment links |
| Review prompt, persona, repodoc or eval change | [change checklists](docs/agents/change-checklists.md): catch-rate evidence and injection/quality evaluation obligations |
| Instructions, skills or prompt-loading upkeep | [context maintenance](docs/agents/README.md) |
| Naming and public voice | [brand foundations](docs/BRAND_FOUNDATIONS.md) §5: direct, technical, restrained; precise errors with remedies |

## Documentation surfaces

Root `README.md` and `site/` are human-facing narrative. `docs/` is structured,
agent-consumed material and is never a site publish source; no site imports,
globs or symlinks into `docs/`. GitHub prose links are fine.

`docs/ARCHITECTURE.md` §10 holds pointers: path + at most one sentence + one
anchor issue/ADR. Put longer contracts beside code in package READMEs or indexed
`docs/*.md` topics. Architecture lines must stay within 1,000 characters.

Canonical schemas are in `docs/spec/`; run `scripts/sync-schemas` after edits.
Canonical REST is `docs/api/v0.openapi.yaml`. The four site reference regions
(workflow-spec, plan-schema, cli, api) are generated: use
`scripts/gen-site-reference`; hand-edit only outside their generated markers.
The public site tracks main, with workflow majors documented inline on one page.

## Instruction upkeep

Keep this file within **8 KiB**. Put new lessons in the owning package contract,
focused guide, skill or executable check; add root rules only when they apply
across tasks. Do not append incident histories or duplicate executable pins.
Update skill references when their source moves; use `rg` to find affected
skills when changing a cited rule, tool pin, or `scripts/dev` / `scripts/test`
behavior. A run unable to edit skills files an operator follow-up.

Skills live in `.agents/skills/<name>/SKILL.md`, with per-skill symlinks from
`.claude/skills/`. Preserve invocation policies and operator-only guards.
`.agents/`, `.claude/`, `.codex/` and instruction-file protections are enforced by
workflow constraints; the exact root `AGENTS.md` exception is workflow-specific.
Keep `.claude/settings.json` permissions-only. `CLAUDE.md` imports this file;
do not add unconditional imports of the task guides.

Run `scripts/check-agent-context` after editing instruction entrypoints, their
linked guides, or skills. Its budgets, link scope, migration map and validation
scenarios are in [context maintenance](docs/agents/README.md).
