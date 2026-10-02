---
id: ADR-053
title: "Post-deploy verification + incident intake (closing the ops-to-dev loop)"
status: accepted
date: 2026-07-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/1581
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-053: Post-deploy verification + incident intake (closing the ops-to-dev loop)

## Context

The deploy stage (ADR-038, E23) dispatches an external pipeline and polls it to terminal — then goes blind: outcome derives purely from the GH Actions conclusion, with no verification that the deployed system actually works. The deployment artifact records a rollback_handle that is persisted and displayed but never actioned (deployment.go:77; the reconciler's rollback path uses its own correlation, and nothing feeds the stored handle back into a revert). Separately, the ops→dev feedback loop is manual: production signals become issues only when the founder types them. MVP_SPEC §9 explicitly excludes 'incident response and standalone rollback/monitoring tooling' — this ADR proposes a DELIBERATE, narrow v0.x scope amendment: two governance-shaped slices that stay inside the envelope posture, not a monitoring or incident-management product.

## Options

**Post-deploy verification — Option A: acceptance stage after deploy in the `release` workflow (RECOMMENDED).** The E31 acceptance machinery is target-agnostic by construction: an acceptance stage following the deploy stage, with the deployed environment's host in egress.target_hosts and the expected SHA resolved from the deployment artifact's ref, reuses the egress containment, identity probe (previewprobe verbatim — only the expected-SHA source is new), verdict/evidence pipeline, and deterministic triage wholesale. A failed post-deploy verdict routes through triage to a rollback offer wiring the stored rollback_handle into the existing rollback dispatch. **Option B: bespoke smoke-check runner.** Duplicates E31 for no gain; rejected.

**Incident intake — Option A: alert webhook as a TRIGGER SOURCE (RECOMMENDED).** An authenticated (HMAC) POST /v0/triggers/alert files a conventions-complete incident issue via the work-items pipeline and optionally auto-starts a `hotfix_change` run — auto-start behind a config default-OFF, operator-gated. Fishhawk stays the governance layer: the alerting system detects; Fishhawk turns the signal into a gated, audited change. **Option B: full incident management (paging, on-call, timelines).** Rejected — explicitly out of product scope; Fishhawk does not compete with PagerDuty.

## Recommendation

Adopt both Option As, and amend MVP_SPEC §9's exclusion language to distinguish what stays excluded (monitoring/paging/incident tooling) from what this admits (post-deploy verification EVIDENCE via the existing acceptance machinery, and alerts as one more run trigger source). Also wire rollback_handle actioning — the stored-but-dead handle is a completeness bug independent of the rest. Implementation epic: E35 (filed alongside): release-workflow acceptance stage, deployed-target identity, rollback actioning, alert trigger source (autonomy:low on the auth surface), hotfix_change preset, e2e dogfood.

## Decision

**Accepted (2026-07-26).** Adopt both recommended Option As, the MVP_SPEC §9 scope amendment, and `rollback_handle` actioning. This unblocks the **E35 #1585** campaign. Three forks were settled at ratification; where they refine the Recommendation, **these govern**.

### 1. Both slices are in scope

Post-deploy verification (an acceptance stage following the deploy stage in the `release` workflow, reusing the E31 egress containment, `previewprobe` identity probe, verdict/evidence pipeline and deterministic triage) **and** incident intake (an HMAC-authenticated `POST /v0/triggers/alert` filing a conventions-complete incident issue through the work-items pipeline). E35 stands as scoped.

`rollback_handle` actioning is included and is **not** contingent on the rest: the handle is persisted and displayed today but never fed back into a revert (`deployment.go:77`), which is a completeness bug on its own.

### 2. Auto-start-on-alert IS built, behind a config flag that ships OFF

The capability lands in this epic rather than being deferred. **The OFF default is the safety property, so it must be enforced as one:** a test asserting the shipped default, and enabling it must be an operator configuration decision recorded in config — never a code change, and never a default that drifts on in a later refactor. This is the sharpest autonomy edge in the product to date and should be labelled `autonomy:low` / human-led along with the HMAC ingress itself.

### 3. Acceptance targets are STAGING-ONLY in this epic

Production-facing acceptance targets are explicitly **out of scope** here. ADR-050's egress and credential posture was designed for a local preview; a production target is a materially different threat model and requires its own gated re-review (least-privilege synthetic-check accounts, read-only, never admin credentials) before any prod host is permitted. The egress allow-list must **reject** production hosts in this slice rather than merely not listing them.

### 4. MVP_SPEC §9 amendment

Amend the exclusion language to distinguish what remains excluded — monitoring, paging, on-call, incident-management tooling, and standalone rollback/monitoring products — from what this decision admits: **post-deploy verification evidence via the existing acceptance machinery**, and **alerts as one more run trigger source**. Fishhawk does not detect; it turns a detected signal into a gated, audited change.

### Known limitation created by forks 2 + 3 together

With auto-start available and targets restricted to staging, an alert originating in **production** can auto-start a `hotfix_change` run whose post-deploy verification runs against **staging** — not the environment that alerted. This is acceptable for v0.x because the hotfix still passes through the normal plan/implement/review gates and human merge, but it means post-deploy verification does not close the loop on the alerting environment until production targets are permitted under fork 3's follow-up. Record it as a documented limitation rather than letting it be discovered during the dogfood.

Named approver: repository maintainer (human).

## Consequences

Positive: closes deploy→verify→(rollback|proceed) with ~90% machinery reuse; the ops→dev loop becomes a trigger source rather than founder typing; deploy outcomes gain behavioral evidence rather than pipeline-conclusion proxies. Negative/risks: production egress targets put real credentials in FISHHAWK_ACCEPTANCE_ENV_* scope — the ADR-050 posture must be re-reviewed for prod-facing targets (least-privilege synthetic-check accounts, never admin creds); alert webhook is a new authenticated ingress (HMAC verification is autonomy:low human-led); auto-start-on-alert is the sharpest autonomy edge in the product so far — shipping it default-OFF is load-bearing.
