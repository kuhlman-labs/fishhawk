# Operator role spec `operator-role-v0`

The contract for the operator role (ADR-040 / #997, D1): the agent that drives runs through their gates as the medium between human operators and implement/review agents. The role spec defines **behavior** (procedure, escalation posture, conventions, prohibitions); **authority** lives in the workflow spec's `operator_agent` delegation knobs (ADR-040 D2, #1026) and is not expressed here.

- Canonical schema: [`operator-role.schema.json`](operator-role.schema.json) (JSON Schema Draft 2020-12, `$id` pins `operator-role-v0`).
- Shipped default: [`operator-role-default.yaml`](operator-role-default.yaml) — a **product artifact**, versioned with the product, seeded from the 2026-06 dogfood playbook.
- Overlay schema: [`operator-role-overlay.schema.json`](operator-role-overlay.schema.json) — the contract for a repo's `.fishhawk/operator.yaml`.
- Go validation: `backend/internal/operatorrole` (embedded copies of all three, mirrored by `scripts/sync-schemas`, locked by the schema-sync gate). `Default()` returns the shipped spec, validated against its own schema at package init; `ValidateOverlay` is the canonical enforcement point for overlays.

## Artifact topology

The base role spec ships with the product. A repo does NOT write its own role spec; it may add a **thin overlay** at `.fishhawk/operator.yaml`. Procedure improvements discovered in any repo flow back into the product's default spec (file an issue), so every deployment benefits — this is the thinness rule, made structural by the overlay schema.

## Full role spec sections

| Field | Required | Shape | Meaning |
|---|---|---|---|
| `role` | yes | const `operator` | Role identity. v0 defines exactly one role. |
| `spec_version` | yes | enum `operator-role-v0` | Single-value enum per the versioning rules below. |
| `mission` | yes | non-empty string | Behavior posture in prose: distill don't relay; act only within delegation; page for everything else. |
| `gate_procedures` | yes | object, exactly the five keys | The playbook. Each key maps to a non-empty ordered list of prose steps. |
| `gate_procedures.pre_flight` | yes | string list | Checks before starting a run (clean main, daemon identity, committed spec changes). |
| `gate_procedures.plan_gate` | yes | string list | Verdict-awaiting, sweep reading, split-verdict arbitration, amendment discipline. |
| `gate_procedures.implement_review_gate` | yes | string list | Concern routing via fixup, waive discipline, false-positive handling. |
| `gate_procedures.merge_ritual` | yes | string list | Checks-green → approve → merge → post-merge walk sequence. |
| `gate_procedures.recovery` | yes | string list | Failure-category playbook (resume vs retry vs page). |
| `escalation.always_page` | yes | string or string list | Conditions that always page the human — either a passthrough reference to the workflow spec's `operator_agent.must_page_human` or an explicit condition list. Posture is fail-closed. |
| `escalation.page_format` | yes | non-empty string | What a page contains (distilled summary, never raw event streams). |
| `conventions` | no | object: snake_case key → prose value | Named working conventions. The section overlays merge into. |
| `forbidden` | yes | non-empty string list | Actions the role must never take, regardless of delegation. |

Every object level is `additionalProperties: false` — the surface is closed; new sections are additive schema changes within v0.

**Reviewer-reject classification is read from config, not judged.** In the `implement_review_gate` and `escalation` procedures, the advisory-vs-hard line on a reviewer reject is resolved from the workflow spec's `operator_agent` contract, not from per-run operator judgment. The mapping is the [decision-class taxonomy](./workflow-v0.md#operator-agent-delegation-v05): an **advisory** agent reject (agent + human reviewers) is non-blocking and routes back through `may_route_fixup` / `convergent_concerns` (legible token `advisory_reviewer_reject`, v0.7+); a **gating** agent reject (agent-only) fires `gating_reviewer_reject` and pages (the legacy bare `reviewer_reject` resolves to this gating sense for back-compat); a **human** reject arrives as `plan_rejection` and pages. The run's resolved class is surfaced on the `GET /v0/runs/{id}` delegation block as `reviewer_reject_class`, so the operator agent reads it rather than re-deriving the authority. Authority is the ADR-027 `planreview.ResolveAuthority` semantics, unchanged. The interactive operator prompt that asks a human to classify the reject is a fallback **only** when no `operator_agent` contract governs the run (the fail-closed default, where every judgment pages).

**Per-stage model selection is read from config, not judged (#1421).** The operator agent's authority to choose each stage's model is declared in the workflow spec's `operator_agent.model_policy` contract (scenario A: an operator agent pinned to a frontier model decides each stage's model), not from per-run operator judgment. It reads the resolved policy from the `GET /v0/runs/{id}` delegation block (`delegation.model_policy`: `strategy`, per-stage `defaults`, and the `allowed` set) and **applies** it through the existing per-stage model override channels (#1416) — bounded by, never widening, the deployment per-adapter allow-list. This is declarative only: the backend resolution ladders are unchanged, so an absent `model_policy` leaves model selection byte-identical to today. See the [`model_policy` reference](./workflow-v0.md#operator-agent-delegation-v05).

## Overlay contract (`.fishhawk/operator.yaml`)

The overlay may ONLY:

- **`knob_presets`** — named preset selection (snake_case key → preset name). The key shape is reserved here; preset values and their evaluation against `operator_agent` knobs are #1026's scope and may tighten additively within v0.
- **`conventions`** — local merge-ritual specifics and escalation contacts/channels, merged into the base spec's `conventions`.
- **`work_management`** — opaque pointer to the repo's work-management config (#1005/#1012).
- Identity fields: `spec_version` (required — names the base spec version the overlay targets) and optional `role`.

Example: [`examples/operator-role-overlay-example.yaml`](examples/operator-role-overlay-example.yaml).

### Thinness rule

Procedure fields (`mission`, `gate_procedures`, `escalation`, `forbidden`) are structurally excluded from the overlay: each is declared in the overlay schema with an always-failing `not` subschema whose `$comment` carries the rule text, so a violation's validator output names the rule — not just "additional properties not allowed". Draft 2020-12 treats `$comment` as an annotation (core spec §10.3, same basis as the repo's `x-intended-required` convention), so the embedded text is safe for all validators. The Go side (`operatorrole.ValidateOverlay`) returns a dedicated `*ThinnessError` naming the offending field: overlay may only set knob presets, local conventions, and the work-management pointer; procedure belongs in the product — file an issue.

Escalation **contacts and channels** are conventions and go under `conventions`; the `escalation` section itself (paging conditions and format) is procedure and stays in the product.

`.fishhawk/operator.yaml` must be a **single YAML document**: `ValidateOverlay` rejects a multi-document stream outright, since only the first document is schema-validated and a trailing document could otherwise carry procedure fields past the thinness rule.

## Identity and token issuance (D4)

A deployed operator-agent role instance acts under its own API token, never a human's. The token subject follows the convention:

```
operator-agent/<role-spec-version>      e.g. operator-agent/operator-role-v0
```

- **Issuance**: `fishhawkd token issue --subject operator-agent/operator-role-v0`. The default operator scope set (#526: `read:runs`, `read:audit`, `write:runs`, `write:approvals`, `write:stages`) applies unchanged — the subject carries no `mcp:` prefix and the convention introduces no new scopes. Authority remains entirely in the workflow spec's `operator_agent` delegation knobs.
- **Pattern validation**: issuance rejects a subject carrying the `operator-agent/` prefix (or the bare string `operator-agent`) whose suffix is not a recognized role-spec version (currently exactly `operator-role-v0`, the same single-value set the schema's `spec_version` enum pins). Non-prefixed subjects are unaffected. Validation is issuance-time only — no read path validates subjects, so a version bump strands no existing token. Go implementation: `operatorrole.ValidateTokenSubject` / `operatorrole.IsTokenSubject`.
- **Audit attribution**: every delegated-action audit entry (`approval_submitted`, `stage_fixup_triggered`, `concern_waived`, `stage_retried`, `stage_override_retried`, and the bearer-auth PR-report appends) records `actor_subject` = the acting token subject, and selects `actor_kind` from it: `agent` when the subject carries the `operator-agent/` prefix, `user` for every other subject (human tokens, GitHub logins). A role-instance action and a human action on the same run are therefore distinguishable on the chain. The `delegated: "<rule>"` payload field (#1026) is an independent signal — it names the condition that authorized a delegated action, while `actor_kind` names who acted; a non-delegated action by the role token still records `agent`.

## Versioning

- `spec_version` is a required, single-value enum (`operator-role-v0`), matching the `version` / `plan_version` convention.
- The `$id` URL pins the version (`operator-role-v0.schema.json`); the canonical filename stays `operator-role.schema.json`.
- Additive optional fields are permitted within v0 and require validator-test updates. A breaking behavior change bumps to `operator-role-v1` in a new schema file; validators carry every version forever.
- The shipped default is validated against the full schema at backend package init, so the product artifact can never drift from its own schema.

## See also

- ADR-040 (#997) — decision record: artifact topology (D1), delegation knobs (D2), parallel-track prerequisites (D3), identity + paging (D4).
- `docs/spec/workflow-v0.md` — where the `operator_agent` delegation knobs will live (#1026).
- `docs/METHODOLOGY.md` — autonomy tiers, which map to knob presets.
