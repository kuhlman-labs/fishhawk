---
id: ADR-048
title: "Repository onboarding — `fishhawk init` + `doctor` (CLI engine), MCP/skill wrapper, and App-PR scaffolding, preset-driven from typed specs"
status: accepted
date: 2026-06-30
issue: https://github.com/kuhlman-labs/fishhawk/issues/1499
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-048: Repository onboarding — `fishhawk init` + `doctor` (CLI engine), MCP/skill wrapper, and App-PR scaffolding, preset-driven from typed specs

## Context

Fishhawk needs a low-friction way to onboard **other** repositories to run the workflow. Today onboarding is manual, undocumented, and split across three loci that fail independently:

- **Repo-side files** — `.fishhawk/workflows.yaml` (workflows, stages, reviewers, the `operator_agent` contract, budgets, gates) and an `AGENTS.md`/`CLAUDE.md` instruction telling the coding agent to route work through the loop.
- **Server-side provisioning** — an operator token with the right scopes (`fishhawkd token issue`), and the deployment's reviewer/adapter config.
- **Out-of-band** — the GitHub App installed on the repo (an OAuth flow), and an execution path (either the `fishhawk.yml` GitHub-Actions self-execution workflow + secrets + a reachable backend, or the local MCP loop).

A repo that has the YAML but is missing a prerequisite **looks onboarded but wedges on the first run**. This class of failure is not hypothetical — this session alone hit the `write:campaigns` scope gap (#1474, a token missing a needed scope), the operator-default-scopes omission, and App/token capability gaps. The YAML is rarely the hard part; the prerequisites are.

Timing is good: the workflow spec is now the **portable unit**. ADR-040 put the `operator_agent` contract in the spec; E28 (#1492) moved reviewer policy (`reasoning_effort`, `review_timeout`, reviewer enablement/`optional`) out of deployment env into the spec. So a generated `.fishhawk/workflows.yaml` is genuinely self-contained across repos.

Prompted by: "expose an interactive init skill in MCP/CLI that walks through creating the workflow + operator contract + AGENTS.md instructions, then validates the schema."

## Options

1. **Docs-only.** An onboarding guide; users hand-craft everything. Cheapest; highest friction; catches no prerequisite failures.

2. **Interactive `init` wizard (the original proposal, as-is).** A CLI/MCP tool with a Q&A flow that generates the spec + AGENTS.md and validates. Right instinct, but risks if taken literally: a long wizard, **string-templated YAML that drifts from the schema**, clobbering existing config, and *pretending to perform out-of-band steps it cannot* (App install, token issue) — reproducing the looks-onboarded-but-wedges failure.

3. **Layered, preset-driven onboarding (recommended).** One preset engine, three surfaces:
   - **`fishhawk init` (CLI engine)** — generates the spec by **marshaling typed `spec.Workflow` structs and validating against the embedded schema** (reuse `backend/internal/spec` / `cli/internal/spec` + the validator — never string YAML), from **autonomy-tier presets** (`low`/`medium`/`high` per `docs/METHODOLOGY.md`; the current `feature_change` spec is the `medium` preset) plus 3–4 deltas (heterogeneous vs single-agent reviewers, which human gates, a budget ceiling). Writes an **idempotent, managed `AGENTS.md`/`CLAUDE.md` block** between markers (never clobbers the user's own instructions; re-runnable). **Separates scaffold from provisioning**: prints a preflight checklist (App-install URL, the exact `token issue` command, the execution-path setup) for what it cannot do.
   - **`fishhawk doctor`** (or `init --check`) — verifies readiness: App installed? operator token carries the needed scopes? spec valid against the embedded schema? reviewers available on the target deployment? This is the piece that catches the real onboarding failures (the #1474 class).
   - **Thin MCP tool / Claude Code skill** wrapping the CLI for the conversational "help me onboard" UX — one engine, two frontends (no duplicated generation logic).
   - **App-PR scaffolding** — install the Fishhawk App on a repo → the App opens a PR adding `.fishhawk/workflows.yaml` (a chosen preset) + the AGENTS.md/CLAUDE.md block + `fishhawk.yml`, pre-filled. Zero local setup, reviewable as a PR, dogfoods our own loop. Same preset engine, different surface; ties to the hosted story / the parked #655 gateway.

4. **App-PR only (no CLI).** The App does all scaffolding via a PR. Lowest user friction at scale but no dev-local path and no help for the local dogfood loop / air-gapped users.

## Recommendation

**Option 3**, sequenced CLI-first with the App-PR path in the same epic (a later phase). Design commitments:

- **Preset model = the METHODOLOGY autonomy tiers** (`low`/`medium`/`high`); extract the current `feature_change` spec as the `medium` preset. `init` = pick a preset + a few deltas, not a 20-question wizard.
- **Generate from typed structs + validate against the embedded schema** — schema-valid by construction, evolves with the schema for free.
- **Idempotent + non-destructive** — managed instruction block between markers; never overwrite an existing `.fishhawk/workflows.yaml` (offer merge/update).
- **Instruction files — `AGENTS.md` canonical + a `CLAUDE.md` `@AGENTS.md` bridge (ratified fork A).** `AGENTS.md` holds the Fishhawk routing instructions (the cross-agent standard). **Claude Code reads ONLY `CLAUDE.md`/`CLAUDE.local.md` — it does NOT load `AGENTS.md` natively** (verified 2026-06-30 against code.claude.com/docs/en/memory; no setting toggles it). So `init` also writes/ensures a minimal `CLAUDE.md` containing `@AGENTS.md` (the documented import bridge — exactly how this repo is set up), idempotently adding the import line if a `CLAUDE.md` already exists. Result: one cross-agent instruction file, reliably loaded by Claude Code via the bridge, and picked up natively by AGENTS.md-aware agents.
- **The "operator contract" is the spec's `operator_agent` block**, not a separate artifact — a preset choice.
- **`init` ends by running the `doctor` preflight** so the user sees the remaining out-of-band steps immediately.
- **Sequencing (all in one epic):** CLI `init` + `doctor` engine first (immediately useful, incl. for local dogfooding) → MCP/skill wrapper → App-PR path (larger; hosted-story dependency; but in scope for this epic per ratified fork B).

## Decision

**Accepted (2026-06-30).** Adopt Option 3 — the layered, preset-driven onboarding (CLI `init` + `doctor` engine, MCP/skill wrapper, and App-PR scaffolding), preset-driven from typed specs, with the design commitments above.

Ratified forks:
- **A. Instruction file = `AGENTS.md` canonical + a `CLAUDE.md` `@AGENTS.md` bridge.** Because Claude Code loads only `CLAUDE.md` (not `AGENTS.md` natively — verified), `init` writes `AGENTS.md` and ensures a minimal `CLAUDE.md` that imports it. This matches how this repo bootstraps its own agent instructions.
- **B. The App-PR scaffolding path is IN SCOPE for this epic** (not deferred), sequenced as a later phase after the CLI engine + `doctor`.

Next: decompose into an epic with dependency-ordered children (preset library + typed-spec generation → `init` CLI → managed AGENTS.md/CLAUDE.md block → `doctor` → MCP/skill wrapper → App-PR onboarding) and drive as a campaign, same shape as ADR-047 → E25.

## Consequences

- **Positive:** a repo onboards in one guided command; prerequisite failures are caught by `doctor` before the first run instead of wedging it; spec generation is schema-safe by construction; the preset model encodes best-practice defaults; the App-PR path scales to zero-setup onboarding; reuses existing surfaces (spec types + embedded validator, METHODOLOGY tiers, `fishhawk.yml`, `token issue`).
- **Costs / negatives:** three surfaces to build — sequence them, don't build all at once; preset templates need maintenance as the spec evolves (mitigated by generating from typed structs, not literals); the App-PR path is a larger build with a hosted-story dependency (later phase, but committed). A managed AGENTS.md/CLAUDE.md block must be genuinely idempotent or it risks fighting the user's edits.
- **Non-goals for v0:** installing the App and issuing tokens (out-of-band / server-side — `init` guides, `doctor` verifies, neither performs them); a hosted onboarding web UI beyond the App-PR.
- **Follow-on:** the epic children below.

## Related

#655 (MCP gateway / hosted), ADR-040 (#997, operator_agent contract in the spec), E28 (#1492, reviewer policy in the spec), #1474 (the write:campaigns scope-gap — a canonical onboarding-prerequisite failure), `docs/METHODOLOGY.md` (autonomy tiers → presets), `.fishhawk/workflows.yaml` (the medium preset), `backend/internal/spec` / `cli/internal/spec` (spec types + embedded validator), `.github/workflows/fishhawk.yml` (execution-path template), code.claude.com/docs/en/memory (Claude Code loads CLAUDE.md not AGENTS.md — the bridge rationale).
