---
id: ADR-026
title: "Backward-compatibility discipline for schema-extension PRs"
status: unknown
issue: https://github.com/kuhlman-labs/fishhawk/issues/472
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-026: Backward-compatibility discipline for schema-extension PRs

## Context

ADR-025 D2 (PR #461) added `predicted_runtime_minutes` and `predicted_runtime_confidence` as **required** fields on `standard_v1`. This was a breaking change to an embedded schema: all pre-D2 plans now fail validation. Coordinated rollout worked (the plan-stage prompt update shipped in the same PR so the agent produces compliant plans immediately) — but the schema-cascade aftermath was substantial:

- 5 test fixtures broke (3 caught only post-push)
- Stale-backend rejection footgun (rebuilt runner + old running fishhawkd)
- One wasted loop run + restart cycle

In a hosted-deployment future where backend and runner versions diverge across organizational boundaries (Fishhawk's backend updates on our cadence; customer runners update on theirs), this kind of breaking change will not be possible without a multi-month deprecation period.

This ADR settles the policy.

## Options

### A. Break freely, coordinate rollout per-PR

What we did for D2. Works in a single-team monorepo; doesn't scale to customer-side runners.

### B. Add fields optional first, never break

Every new field defaults to optional. Once all callers comply (observed via audit metrics over N weeks), a separate PR promotes to required.

Cost: every required field becomes a two-PR sequence with a soak period. Slower, but boring.

### C. Versioned schemas with side-by-side support

`standard_v1.1` + `standard_v1.0` both supported by the backend simultaneously. Runner advertises which version it speaks; backend serves the matching shape. Deprecation removes old versions after some period.

Cost: backend complexity. But matches how real APIs evolve.

### D. Required fields only in NEW schema versions (v2, v3, etc.)

`standard_v1.x` stays additive forever. Required-field additions land in `standard_v2`. Backend supports both during transition. CLI/runner upgrades opt into v2 when ready.

Cost: bumps the schema version number frequently. But the version-bump becomes the explicit signal.

## Recommendation (subject to ADR review)

**B + D in combination**:
- Within a major version (`standard_v1.x`), all schema additions are optional. Required-field promotions require a soak period documented in the change PR.
- When required-field additions are unavoidable, bump to `standard_v2` (or `workflow_v0` → `workflow_v0.4`, etc.) and keep both schemas live during a deprecation period.
- The version-coordination mechanism from issue #466 (backend `/healthz` schema versions, runner version advertising) makes this enforceable.

ADR-025 D2's breaking-change-with-coordinated-rollout becomes the **last** time we do that. From here forward, the policy applies.

## Consequences

- **Schema bumps become rarer** but each one is a deliberate, documented event.
- **Optional-first discipline** for additive changes: most new fields are optional and the agent fills them when present.
- **Backend has to support multiple schema versions** during deprecation. Estimated cost: ~50 lines per schema per supported version (validators, decoders).
- **CLAUDE.md gets a "schema change checklist"**: (1) optional or major version? (2) what's the deprecation period? (3) which surfaces advertise the new version?
- **`scripts/sync-schemas` (#463) extends to version-aware**: copies the latest version of each schema; older versions live alongside.

## Open design questions

- Deprecation period: 30 days? 90? Until next major release? Customer-survey driven?
- How to declare a field's "intended-required" status without enforcing it during the soak period? A JSON Schema extension keyword (`x-intended-required: true`)?
- What about removing fields? Different rules — deprecation period probably longer.

## Out of scope

- Re-doing D2 retroactively as additive — the train has left, and D2's required fields are enforced as of merge. The policy applies forward.
- Database migration discipline (a related but distinct concern).
- Audit-chain compatibility across schema bumps (the audit chain stores raw payloads; old entries remain readable indefinitely — already handled).

## Related

- ADR-025 / #451 — the predecessor; this ADR is the policy that ADR-025's D2 surfaces the need for.
- #466 (cross-binary version coordination) — the mechanism this ADR's enforcement depends on.
- #463 / PR #463 — sync-schemas tooling (will extend to version-aware).
- [[feedback-dogfood-local-loop]] — the methodology that made the stale-backend rejection visible.
