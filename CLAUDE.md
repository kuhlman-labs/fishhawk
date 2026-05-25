# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repo state

Pre-alpha. Most code referenced in `docs/MVP_SPEC.md` doesn't exist yet — it's split into ~49 child issues across 15 epics in Project #7. Verify file tree before assuming.

## Canonical references

- `docs/MVP_SPEC.md` — v0 scope. Cite section numbers when scope is in question.
- `docs/ARCHITECTURE.md` — current technical realization (stack, lifecycle, storage, invariants). Read before designing anything cross-component.
- `docs/BRAND_FOUNDATIONS.md` — voice, naming, positioning.
- `docs/METHODOLOGY.md` — autonomy tiers (low/medium/high).
- `docs/spec/` — canonical JSON Schemas + reference docs for the workflow spec (`.fishhawk/workflows.yaml`) and the plan artifact (`standard_v1`). Validate with `check-jsonschema --schemafile <schema> <yaml-or-json>`.
- `docs/api/` — REST API surface: `v0.openapi.yaml` is source of truth, `v0.md` is the human companion. Lint with `npx -y @redocly/cli@latest lint --config docs/api/redocly.yaml docs/api/v0.openapi.yaml`.
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
scripts/test single -run TestName ./backend/internal/version/   # passthrough
```

Per-module without the wrapper (still useful for `go build` and `golangci-lint`):

```sh
go build ./backend/...
golangci-lint run ./backend/...
```

`.golangci.yml` is **v2 format** (`version: "2"` at top). Local install must be golangci-lint v2.x; v1 binaries reject this config.

**Coverage gate**: aggregate ≥ 80% across **all** registered modules, excluding sqlc-generated `*/db/` packages (CI uses `--exclude '/db/'` so new sqlc packages auto-skip). Tiered targets in `docs/ARCHITECTURE.md` §9. CI fails `CI Pass` if the threshold drops. `scripts/test coverage` runs the same check. The underlying loop (CI uses the same shape):

```sh
profiles=()
while IFS= read -r m; do
  (cd "$m" && go test -race -coverprofile=coverage.out -covermode=atomic ./...)
  profiles+=("$m/coverage.out")
done < <(go work edit -json | jq -r '.Use[].DiskPath')
python3 scripts/check-coverage.py --threshold 80 --exclude '/db/' "${profiles[@]}"
```

When editing a schema under `docs/spec/`, run `scripts/sync-schemas` to mirror the change into all embedded copies before committing. CI fails the schema-sync gate otherwise. Opt-in local check: `git config core.hooksPath .githooks`.

## Adding a Go module

1. `mkdir <name> && cd <name> && go mod init github.com/kuhlman-labs/fishhawk/<name>`
2. Append `use ./<name>` to `/go.work`.
3. Verify: `(cd <name> && go build ./... && go test ./... && golangci-lint run ./...)`.

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
   - Voice/naming → `BRAND_FOUNDATIONS.md`. New trap / build workflow → `CLAUDE.md`. Autonomy convention → `METHODOLOGY.md`.
5. **File issues for deferred work** before the PR opens. Any TODO, "follow-up PR", "deferred to E…", or obvious operability gap gets a tracking issue: title `[E<parent>.<n>]` (or `[ADR-NNN]`), same `area:*/autonomy:*/phase:*/type:*` labels as siblings, add to Project #7 with `Status=Backlog`, link from the parent epic's Children list, and reference from the PR body's `## Notes` so the deferral is reviewable.
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
- After **Day 21**, every change must flow through a Fishhawk workflow run (today: by convention; later: enforced by the product).
