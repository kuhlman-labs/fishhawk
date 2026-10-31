---
id: ADR-102
title: "Verify-command exit-code contract: 0 pass, 75 temp-fail, 73 lock-refused, other = change failed, as the primary verify classifier in place of tool-text scraping"
status: accepted
date: 2026-10-31
issue: https://github.com/kuhlman-labs/fishhawk/issues/4514
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-102: Verify-command exit-code contract: 0 pass, 75 temp-fail, 73 lock-refused, other = change failed, as the primary verify classifier in place of tool-text scraping

## Context

#4022's plan (run b1070b0c, approved 2026-10-16) declared `new_architectural_decision`: a fixed, documented exit-code contract between Fishhawk and every project's verify command (0 pass / 75 EX_TEMPFAIL = infrastructure, bounded in-place re-run then category C / 73 EX_CANTCREAT = verify lock refused, bounded re-run then category C / any other non-zero = the change failed) replaces tool-text scraping as the primary verify classifier. Fix-up routed instructions also gain a router-set `target` (code | pr_body | docs) that alone decides satisfiability. The change extends the ADR-063 / #3448 out-of-band gate-disposition channel, and changes which failures ADR-023 category C absorbs.

## Options

1. Record the contract as an ADR (this item).
2. Leave it documented only in docs/spec/workflow-v2.md § "Verify exit-code contract" and scripts/README.md.

## Recommendation

Option 1: it is a project-facing protocol that other repositories' verify commands will implement.

## Decision

Accepted: Option 1. Ratified by the Captain on 2026-10-31. The verify exit-code contract is recorded as an ADR: 0 = pass; 75 (EX_TEMPFAIL) = infrastructure, bounded in-place re-run, then category C; 73 (EX_CANTCREAT) = verify lock refused, bounded re-run, then category C; any other non-zero = the change failed. It is the primary verify classifier, and the router-set fix-up `target` (code | pr_body | docs) alone decides satisfiability. The contract shipped with #4022 and is specified in docs/spec/workflow-v2.md § "Verify exit-code contract".

## Consequences

Projects whose verify command cannot set exit codes fall back to text signatures until project-declared signatures land (#4022 Proposal 2 follow-up).

### Intake signals (advisory)

Derived automatically when this item was filed. Everything below is a candidate for a human: nothing was closed, relabelled or transitioned.

**Possible duplicates**
- none found

**Derives from**
Declared as this item's sources; excluded from the duplicate candidates.
- #4022 (outside the scanned window)

**Parent epic suggestion**
- none

**Provisional score**
- 2.0, citing S4, U4
  - **S4** — Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.
    (no parent epic linked)
  - **U4** — Blocks nothing, and nothing blocks it. Schedule on value alone.
    (no depends_on edge declared)

Scanned 300 existing item(s); the scan window was truncated, so an older duplicate may be missed.

<!-- fishhawk-intake:v1 {"derives_from":[{"number":4022,"closed":false,"in_window":false}],"score":{"value":2,"citations":[{"rubric_id":"S4","quote":"Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.","note":"no parent epic linked"},{"rubric_id":"U4","quote":"Blocks nothing, and nothing blocks it. Schedule on value alone.","note":"no depends_on edge declared"}],"unscored":false},"degraded":false,"scanned_items":300,"window_truncated":true,"duration_ms":1618} -->
