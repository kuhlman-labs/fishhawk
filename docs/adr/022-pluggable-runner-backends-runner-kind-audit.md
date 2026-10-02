---
id: ADR-022
title: "Pluggable runner backends + runner_kind audit dimension"
status: unknown
issue: https://github.com/kuhlman-labs/fishhawk/issues/388
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-022: Pluggable runner backends + runner_kind audit dimension

## Context

Today Fishhawk's runner is a single binary delivered as a GitHub Action (`fishhawk/runner@v1`). The audit log implicitly assumes `runner = GHA` — there's no field that distinguishes one execution backend from another. The roadmap calls for pluggable backends:

- **GitHub Actions (today).** Production-grade. Customer pins `fishhawk/runner@v1`, signed binary, SBOM, OIDC auth.
- **Local runner (near-term).** Operator-on-workstation dev loop. Same binary, different env. Critical for fast iteration and design-partner onboarding (no GitHub App install required).
- **Kubernetes (future).** Customer-hosted runner pods. Same binary, K8s-orchestrated.

The audit log needs to distinguish these so compliance consumers can filter or weigh runs differently. Without this dimension, a "local" run is indistinguishable from a "GHA" run in the chain, which weakens the compliance story.

## Options considered

1. **Trust the runner's self-declared kind.** Runner stamps `runner_kind` in the trace bundle payload, backend records it verbatim. Rejected: the runner is operator-controlled in local mode; self-declared provenance is unfalsifiable.
2. **Backend infers from auth method.** OIDC bearer → `github_actions`; Ed25519 with dispatcher-recorded kind → whatever the dispatcher said. Backend assigns at dispatch time; runner never declares.
3. **Strict-mode enforcement.** Backend refuses uploads from disallowed runner kinds per a deployment-wide allowlist.

## Recommendation

**Option 2 for v0** (backend assigns at dispatch, consumer-side filter). **Option 3 deferred to v1** when a compliance customer requests it.

## Decision

`runs.runner_kind` is a new column, `NOT NULL DEFAULT 'github_actions'`, with a CHECK constraint enumerating the closed set (`github_actions`, `local`). The dispatcher assigns it at run creation. The trace handler stamps it onto the `trace_uploaded` audit payload by reading the run row. The runner has no input on this field — it never appears in a trace bundle.

Consumers (SPA, CLI `audit list`, exports, MCP `list_audit`) can filter on `runner_kind` to scope reports. v0 doesn't enforce a strict allowlist server-side; that's a v1 graduation.

## Consequences

- **Compliance posture preserved.** Today's `runner_kind = 'github_actions'` reports look unchanged; legacy rows default to `github_actions` via the migration.
- **Local-runner work unblocked.** Once this lands, an operator can run the binary locally tagged `runner_kind = 'local'` and the audit chain stays coherent.
- **K8s extension is cheap.** Add `'k8s'` to the CHECK constraint, the dispatcher picks the right kind for K8s-routed runs.
- **Verifier (`verifier/internal/audit/`) unchanged.** Provenance is metadata atop the tamper-evidence chain, not part of the hash inputs.
- **Strict-mode kept as a v1 lever.** A `--allowed-runner-kinds` server flag could land later without breaking the v0 consumer-filter contract.

## Implementation tracking

- Migration `0024_runner_kind.up.sql`: add the column, default `github_actions`, CHECK constraint.
- `backend/internal/run/`: thread `RunnerKind` through `Run`, `CreateRunParams`, `ListRunsFilter`.
- `backend/internal/webhook/dispatcher.go`: set `runner_kind: "github_actions"` on dispatcher-created runs.
- `backend/internal/server/runs.go` `handleCreateRun`: accept an optional `runner_kind` in the body (defaults to `github_actions`).
- `backend/internal/server/trace.go` `handleShipTrace`: stamp `runner_kind` into the `trace_uploaded` audit payload from `runRow.RunnerKind`.
- `backend/internal/server/reads.go` `handleListRuns`: accept `?runner_kind=` query filter.

Tracked under the [E22] umbrella (see issue listing this ADR as a dependency).

## Related

- ADR-007 (audit log + signing posture) — provenance is additive to the trust model.
- ADR-019 (Fishhawk as coordination layer) — the operator-driven local runner is a downstream expression of this.

## Addendum: token posture for runner identity

This addendum captures a related decision that surfaced during E22's Phase A retro: **runner identity is read-only across all backends, regardless of where the runner runs.**

### Decision

1. **Runner-side `fhm_*` mcptokens stay scoped `mcp:read` only**, across `github_actions`, `local`, and future `k8s` runner kinds. The agent in the runner never writes — this is the ADR-021 principle generalized.

2. **Local-mode doesn't merge identities.** When the runner runs on the operator's workstation, the agent (Claude Code subprocess) holds an `fhm_*` mcptoken with `mcp:read` scope, and the operator's *separate* terminal Claude Code holds an `fhk_*` apitoken with `write:*` scopes. Two distinct processes, two distinct identities, even on the same physical machine. The operator's terminal-side session remains the canonical write surface — same as GHA-mode, just colocated.

### Why this matters now

Phase C's local-runner mode invites a tempting shortcut: "the operator trusts their own machine, let the agent self-approve." We considered this explicitly (Option B in the retro discussion) and rejected it because:

- The audit log can't distinguish "operator approved" from "operator's agent approved" once the identities merge — both surface as `actor_subject = mcp:run:<id>`. Compliance attribution gets muddier.
- The plan-approval gate becomes performative once the agent can clear it. ADR-021's "agent doesn't approve its own work" rule is location-independent; trust-of-machine isn't the right axis.
- Splitting the runner identity model by backend creates two security domains to maintain. Same posture across backends is simpler and forward-compatible with K8s.

### What this does NOT preclude

The agent-driven **self-retry** case (agent retries its own failed stage on a transient category-A failure) is a legitimate use case that doesn't undermine the human-gate principle — retries don't change "did a human approve this?" Tracked as a separate follow-up ADR; out of scope here.

### Consequences

- The operator-driven write path (`fhk_*` apitoken, `write:*` scopes, operator's own MCP session) is the only path to mutating state across all runner kinds.
- Local-runner work (Phase C) requires no changes to the auth model — the existing prefix-routed bearer middleware does the right thing.
- A future ADR can carve narrow exceptions (e.g. `write:retries`) without revisiting the location-independence principle.

## Related (continued)

- ADR-021 (Claude Code MCP server v0 read-only) — this addendum generalizes that principle across runner backends.
