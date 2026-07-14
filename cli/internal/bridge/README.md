# cli/internal/bridge

Agent-docs bridge generator (ADR-048 / E29.2): the managed `AGENTS.md` + `CLAUDE.md` pair.

## EnsureAgentDocs

`EnsureAgentDocs(rootDir)` writes the canonical `AGENTS.md` carrying a marker-delimited managed block
(`<!-- fishhawk:begin -->` … `<!-- fishhawk:end -->`, regenerated in place; user content outside the
markers is preserved) and ensures `CLAUDE.md` imports it via an `@AGENTS.md` line — the bridge Claude
Code needs, because Claude Code reads only `CLAUDE.md`/`CLAUDE.local.md` and never `AGENTS.md`
natively (#1500).

Idempotent: files are rewritten only when content changes, so a re-run is a clean diff.

Pure library over a caller-supplied root — no repo-root detection lives here.

## Module-wall mirror

Mirrored byte-for-byte into `backend/internal/bridge/bridge.go` per the E29.1 / #1509 module wall
(this copy is `cli/internal/bridge/bridge.go`).

## Consumers

- E29.3 — `fishhawk init` (`cli/cmd/fishhawk/init.go`).
- E29.7 — the App-PR onboarding path (backend side, via the `backend/internal/bridge` mirror).
