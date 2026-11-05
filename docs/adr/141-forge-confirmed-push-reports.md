---
id: ADR-141
title: "Require forge-confirmed push reports and isolate their retries (amends ADR-035)"
status: accepted
date: 2026-11-04
issue: https://github.com/kuhlman-labs/fishhawk/issues/5053
supersedes: []
superseded_by: []
amends: ["ADR-035"]
applies_to: []
---

# ADR-141: Require forge-confirmed push reports and isolate their retries (amends ADR-035)

**Status: accepted (ratified 2026-11-04).**

## Context

#4870 hardens three push-report ingestion paths: fix-up, conflict-resolution and acceptance-scenario reports. Their audit rows feed lineage, merge-candidate verification and merge-queue re-entry. The corrected issue premise requires exact equality with the forge PR head or the server-derived run-branch tip. ADR-035 requires verified lineage but leaves report-time enforcement and remediation to implementation; the existing lineage guard allows forge-read failures. The Captain selected fail-closed report ingestion and requested an ADR amendment first.

This decision covers these three report kinds. Initial PR and child reports, broader authorship proof and fix-up base provenance remain separate work (#5050, #5051). It does not change the existing lineage guard or the downstream eligibility checks.

## Options

- A. Retain the current fail-open posture on forge-read errors. This preserves availability while leaving an outage window in provenance admission.
- B. Require exact forge confirmation before admitting a new push-report row; return a retryable refusal when confirmation is unavailable, with a separate bounded request-local retry budget.
- C. Admit an unverified row and make every consumer distinguish it. This needs a broader audit/consumer contract.

## Recommendation

Option B.

D1. On the currently wired, installation-bearing forge path, a new report is admitted only when its reported head exactly equals either the forge PR head or the tip of a branch derived from server/run state. A reported branch is not authority. Reachability alone is insufficient. Conflict-resolution and acceptance-scenario reports also require their reported base to equal the confirmed head's first parent. The fix-up base remains outside this decision. Existing unwired/no-installation skips, including the current GitLab path, remain explicit residuals; the implementation documents and tests them rather than claiming universal forge verification.

D2. A forge or required repository read error makes confirmation unknown. It returns retryable HTTP 503 `reported_head_unverifiable` and writes no qualifying push-report row. It must not turn a read failure into a proven mismatch or a category-B failure. Existing replay dedup runs before the forge check so a previously admitted signed report remains idempotent after the tip moves; a failed dedup read on a putative mismatch is unknown. Verification has a bounded deadline shared across its forge reads.

D3. A confirmed mismatch admits no qualifying row. A fix-up or conflict report on a running implement stage answers HTTP 200 with outcome failed. Without an active fix-up trigger, its failure is category C, so the same stage can be retried, as selected by the Captain. A live fix-up trigger retains its existing category-B recovery shape, and conflict-resolution recovery ordering remains unchanged. Historical or consumed trigger rows are not proof of an active trigger; an unreadable or indeterminate required trigger state returns the D2 retryable refusal rather than inventing a failure category. A mismatch on an acceptance report, a non-implement stage or a settled stage answers HTTP 409 `reported_head_mismatch` without advancing or transitioning that stage. This narrows ADR-035 rule 4 at this report-ingestion boundary: an acceptance-report mismatch refuses admission without itself halting the run or failing the already-settled acceptance stage; the remaining lineage and merge checks still apply. Mismatch evidence and status refresh are best-effort, with WARN logging on evidence failure; the refusal does not depend on those writes. Exact-head movement or a stale successful forge read can produce a benign refusal; neither is retried as an unknown read error, and this classification alone is never proof of misconduct.

D4. The runner retries only these three push-report kinds within a request-local budget, initially 45 seconds including bounded backoff and attempts. This budget never reads, arms, spends or resets the shared terminal-egress phase. An exhausted or failed push report must leave any later terminal-egress call on the same client its normal budget. Today's runner uploads its trace before acceptance-scenario persistence and the fix-up push report; this decision preserves that call order. Retryable classes remain network errors and 5xx; other 4xx stop. The caller's earlier deadline still applies.

D5. Availability is deliberately traded for provenance admission. A commit may already be pushed when its report exhausts retries, leaving no qualifying audit row. Existing fix-up recovery and acceptance continuation remain; the current conflict-report exhaustion may leave a stage running until watchdog/operator recovery (#5052). This amendment does not authorize automatic vouching, an unverified row, a new recovery bypass or retrying the whole terminal phase to conceal that state. The PR and operator documentation state these recovery costs.

## Decision

**Accepted 2026-11-04** by the Captain (manual ratification path; approver and hashes in the ratification comment). Adopt Option B and D1-D5. This adds a report-ingestion decision within ADR-035's area; it leaves ADR-035's other rules and the existing lineage guard unchanged.

## Consequences

Downstream consumers read newly admitted rows with the stated forge confirmation on wired runs, without a new unverified-row state. Forge availability becomes a dependency of report admission. Exact-head races can refuse a legitimate push; retry exhaustion can leave a pushed commit without recorded provenance. The separate 45-second report budget can increase total elapsed time; it shares no terminal-egress deadline and does not reorder today's uploads. Forge state still does not establish authorship. Existing skips and the deferred surfaces are not covered by this hardening.

### Validation

Tests and counterfactual mutations must cover each handler's refusal before row/advance/republish effects; exact PR-head and server-derived branch-tip success; ancestor-only refusal; first-parent checks on both success arms; repository and forge errors; dedup after movement and dedup-read uncertainty; evidence-write failure; every documented skip; bounded retries; and a client-level failed/exhausted push report followed by a successful terminal-egress call using its intact budget. Whole-run tests pin today's trace-before-report call order and each report's actual continuation; they must not claim that today's acceptance path calls ShipTrace after its push report. Public evidence describes the control and its class without private reproduction details.

- **Record metadata.** amends: ["ADR-035"]; supersedes: []; applies_to: []
- **Related:** ADR-031, ADR-093, ADR-112, ADR-140 (#5047), #4870, #5050, #5051, #5052.
