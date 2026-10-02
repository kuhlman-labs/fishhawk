---
id: ADR-028
title: "Agent filesystem confinement requires an OS-level sandbox"
status: unknown
date: 2026-06-01
issue: https://github.com/kuhlman-labs/fishhawk/issues/642
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-028: Agent filesystem confinement requires an OS-level sandbox

## Context

#611 found the implement agent could write outside the repo working tree (it edited the operator's `~/.claude/…/memory/` store during run `679b042c`). The runner spawns `claude` with `--dangerously-skip-permissions`, which disables Claude's built-in working-dir write confinement (not just the interactive prompts).

#611 (merged, PR #643) ships the **minimum**: trace-based detection that surfaces an `out_of_tree_write` event when a Write/Edit/MultiEdit/NotebookEdit tool-use targets a path outside the working tree (+ allowlisted dirs). It does **not** prevent the write, and does **not** catch **Bash-mediated** writes (`printf > $HOME/x`). Full confinement was deferred to this ADR.

## Empirical findings (claude 2.1.156, 2026-06-01)

No claude-native permission mode achieves **both** non-interactive Bash **and** out-of-tree write confinement:

| mode | non-interactive Bash (go test / lint / scripts) | out-of-tree write confined |
|---|---|---|
| `--dangerously-skip-permissions` (today) | ✅ allowed | ❌ unconfined |
| `--permission-mode acceptEdits` | ❌ Bash denied ("requires approval") → regresses the loop | ✅ Write/Edit confined |
| `acceptEdits --allowedTools Bash` / `--permission-mode auto` | ✅ allowed | ❌ `printf > $HOME/x` succeeds |

Bash is an unconfinable escape hatch at the CLI-permission layer. Confining the agent's filesystem writes requires an **OS-level boundary**, not a `claude` flag.

## Options

1. **macOS `sandbox-exec` (Seatbelt) + Linux `bubblewrap`/namespaces** wrapping the `claude` subprocess. Platform-specific; `sandbox-exec` is deprecated-but-present on macOS; bubblewrap needs install on Linux.
2. **Container the runner stage** — the agent runs in a container whose only writable mounts are the checkout + `/tmp`. Strongest boundary; aligns with hosted/CI execution (ADR-022 pluggable runner backends).
3. **Detection-only locally** (#611 surfacing) + confinement only where the trust boundary actually matters (hosted/multi-tenant).

## Decision (2026-06-01)

**Split by execution context:**

- **Local runner (v0 dogfood / operator-self-hosted): option 3 — detection-only.** Keep #611's `out_of_tree_write` surfacing as the local posture; do NOT add a local OS sandbox. Rationale: it is the operator's own agent on the operator's own machine — the blast radius is their own home/config, the boundary crossing is now visible, and `sandbox-exec` (deprecated on macOS) / bubblewrap (Linux-only, needs install) is platform-fragile and not worth the cost for a single-operator-own-machine threat model. Accepted residual: a local agent can still write outside the tree (including via Bash); it is surfaced, not blocked.

- **Hosted / multi-tenant runner (future): option 2 — container mount-confinement.** Confinement is mandatory there (an unconfined agent on Fishhawk infra against a customer repo could reach other tenants' data/secrets/host). Hosted execution runs in containers regardless, so the container's writable-mount allowlist (checkout + `/tmp`) **is** the sandbox — no `sandbox-exec`/bubblewrap layer needed. This requirement is inherited by the hosted-runner work under ADR-022 (#388).

- **Rejected: option 1** (sandbox-exec/bubblewrap) — platform-fragile, deprecated tooling on macOS, and redundant once hosted runs in containers.

## Consequences

- **Local dogfood:** unchanged behavior + #611 visibility. The agent retains broad FS write access on the operator's machine; out-of-tree *tool*-writes are surfaced via the trace event, Bash-mediated out-of-tree writes remain neither prevented nor surfaced. Accepted for v0.
- **Hosted:** the container writable-mount boundary is a hard requirement on any hosted runner backend (tracked against ADR-022 / #388); it closes both the tool-write and Bash-write holes in one mechanism. No standalone implementation issue is filed now because the hosted runner backend does not yet exist; the requirement is recorded here and cross-referenced from #388 so it is picked up when hosted execution is built.
- **`--dangerously-skip-permissions` stays** on the local runner (the empirical matrix shows the alternatives regress the loop); it becomes moot under the hosted container boundary.

## Related
- #611 / PR #643 (the surfacing minimum + the empirical matrix), ADR-022 / #388 (pluggable runner backends — owns the hosted container-confinement requirement), ADR-024 (agent execution), `runner/internal/agent/claudecode/claudecode.go`.

Parent epic: #389
