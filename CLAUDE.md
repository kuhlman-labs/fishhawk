# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repo state

Pre-alpha. Most code referenced in `docs/MVP_SPEC.md` doesn't exist yet — it's split into ~49 child issues across 15 epics in Project #7. Verify file tree before assuming.

## Canonical references

- `docs/MVP_SPEC.md` — v0 scope. Cite section numbers when scope is in question.
- `docs/BRAND_FOUNDATIONS.md` — voice, naming, positioning.
- `docs/METHODOLOGY.md` — autonomy tiers (low/medium/high).
- `.fishhawk/workflows.yaml` — placeholder; executed by the product itself starting Day 21 (~2026-05-20).

## Documentation surfaces

- `README.md` (root) is the only human-facing doc. Keep narrative.
- Everything in `docs/` is agent-consumed. Write structured, dense, no fluff.
- Public-facing docs deploy via GitHub Pages (source path TBD).

## Build, test, lint

Multi-module Go workspace; **no root `go.mod`**, so `go build ./...` from root fails. Use:

```sh
go build ./backend/...
go test -race ./backend/...
golangci-lint run ./backend/...
go test -race -run TestName ./backend/internal/version/   # single test
```

CI loops over registered modules — adding `use ./<dir>` to `go.work` auto-includes it:

```sh
for m in $(go work edit -json | jq -r '.Use[].DiskPath'); do
  (cd "$m" && go build ./... && go test -race ./...)
done
```

`.golangci.yml` is **v2 format** (`version: "2"` at top). Local install must be golangci-lint v2.x; v1 binaries reject this config.

## Adding a Go module

1. `mkdir <name> && cd <name> && go mod init github.com/kuhlman-labs/fishhawk/<name>`
2. Append `use ./<name>` to `/go.work`.
3. Verify: `(cd <name> && go build ./... && go test ./... && golangci-lint run ./...)`.

## Git flow

**Issue → feature branch → PR → close issue → update relevant issues.**

1. Pick a child issue from Project #7. If a blocking ADR is open, resolve it first.
2. Branch from `origin/main`: `<issue-slug>-<desc>`, e.g. `e3.1-backend-skeleton`.
3. Commit with `git commit -s` (DCO is mandatory; PRs without sign-off are rejected). Imperative-mood title, no conventional-commits prefix. Use HEREDOC for multi-line messages.
4. Open PR — body uses `## Summary` / `## Test plan` / optional `## Notes` / `Closes #<issue>`. Match #80, #81.
5. After merge, walk the dependents:
   - Verify parent epic's task list checked off.
   - Update sibling issues if scope shifted (e.g., a CI fix bundled here means another sibling no longer needs it).
   - If an ADR was resolved, edit its body's Decision section and close.

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
- After **Day 21**, every change must flow through a Fishhawk workflow run (today: by convention; later: enforced by the product).
