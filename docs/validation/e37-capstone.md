# E37 capstone validation

Durable evidence record for the [E37 friction-fix sprint](https://github.com/kuhlman-labs/fishhawk/issues/1645) capstone: one `feature_change` run driven start-to-merge and one refinement session driven end-to-end through `fishhawk_draft_epic`, both with zero operator workarounds. This is the committable deliverable for [#1647](https://github.com/kuhlman-labs/fishhawk/issues/1647) — the run that produced this file exercises the same loop it documents.

## Context

E9 and E34 (both closed) surfaced two recurring loop-friction classes: plans reaching the acceptance gate without acceptance criteria (forcing a revise pass), and acceptance/refinement machinery producing operator-unreadable or operator-blocking results. E37 is the sprint that closed the remaining instances of both classes. This record validates five merged fixes:

| Fix | Issue | Landed via |
|---|---|---|
| Plan prompt authors acceptance criteria on the first attempt | [#1543](https://github.com/kuhlman-labs/fishhawk/issues/1543) | [#1649](https://github.com/kuhlman-labs/fishhawk/pull/1649) |
| Acceptance verdict shapes machine-decode losslessly (the `#1574` class) | [#1574](https://github.com/kuhlman-labs/fishhawk/issues/1574) | [#1646](https://github.com/kuhlman-labs/fishhawk/issues/1646), [#1648](https://github.com/kuhlman-labs/fishhawk/pull/1648) |
| Refinement open arm survives a real, multi-minute drafting-agent call through the MCP chain | [#1637](https://github.com/kuhlman-labs/fishhawk/issues/1637) | [#1651](https://github.com/kuhlman-labs/fishhawk/pull/1651) |
| Filed refinement children carry `[E<discovered-ordinal>.<n>]` titles, never hand-retitled | [#1644](https://github.com/kuhlman-labs/fishhawk/issues/1644) | [#1652](https://github.com/kuhlman-labs/fishhawk/pull/1652) |
| Acceptance sanctions a skipped-with-basis verdict when the running target cannot exhibit a criterion | [#1612](https://github.com/kuhlman-labs/fishhawk/issues/1612) | [#1650](https://github.com/kuhlman-labs/fishhawk/pull/1650) |

## Flow 1 — feature_change run

- **Run id:** `3d98ca3c-5c28-4e97-9c05-aa9d4899bc91`
- **Triggering issue:** [#1637](https://github.com/kuhlman-labs/fishhawk/issues/1637)
- **Delivered via:** [PR #1651](https://github.com/kuhlman-labs/fishhawk/pull/1651) (merged)

The plan carried acceptance criteria on the first attempt — no revise pass was needed, validating [#1543](https://github.com/kuhlman-labs/fishhawk/issues/1543). Both advisory plan reviewers approved first-shot with acceptance criteria authored, itself a live validation of the same fix.

The run's `acceptance_outcome_recorded` audit entry decoded on the first read, with zero decode failures:

| Field | Value |
|---|---|
| `verdict` | `passed` |
| `outcome` | `accepted` |
| `criteria_total` | 5 (3 passed, 2 skipped-with-basis, 0 failed) |
| `target_url` | `http://localhost:8090` |
| `evidence_hashes` | flat array (no nested-shape decode required) |

The flat, cleanly-typed `evidence_hashes` array and the single unambiguous `verdict`/`outcome` pair validate the `#1574` verdict-shape class fixed by [#1646](https://github.com/kuhlman-labs/fishhawk/issues/1646) / [#1648](https://github.com/kuhlman-labs/fishhawk/pull/1648). The 2 skipped-with-basis criteria (out of 5, 0 failed) validate the [#1612](https://github.com/kuhlman-labs/fishhawk/issues/1612) contract for criteria the running target could not exhibit. No operator arbitration was required at any gate in this run.

## Flow 2 — refinement session

- **Session id:** `538856f2-9860-4290-82e2-8b24b969b348`
- **Entry point:** `fishhawk_draft_epic`, driven end-to-end

The open arm survived the real, multi-minute drafting-agent call through the full MCP chain with no 30-second client timeout and no direct-API workaround, validating [#1637](https://github.com/kuhlman-labs/fishhawk/issues/1637)/[#1651](https://github.com/kuhlman-labs/fishhawk/pull/1651).

Filing produced one epic and three children, all carrying the discovered ordinal (38 — distinct from the parent issue number) rather than a hand-assigned one:

| Item | Issue | Title |
|---|---|---|
| Epic | [#1654](https://github.com/kuhlman-labs/fishhawk/issues/1654) | `[E38]` |
| Child | [#1655](https://github.com/kuhlman-labs/fishhawk/issues/1655) | `[E38.1]` |
| Child | [#1656](https://github.com/kuhlman-labs/fishhawk/issues/1656) | `[E38.2]` |
| Child | [#1657](https://github.com/kuhlman-labs/fishhawk/issues/1657) | `[E38.3]` |

None of the four titles were hand-retitled after filing — each carried the discovered `[E<ordinal>.<n>]` form at file time, and filing reported `verified=true`. This validates [#1644](https://github.com/kuhlman-labs/fishhawk/issues/1644)/[#1652](https://github.com/kuhlman-labs/fishhawk/pull/1652).

## Zero-workarounds attestation

All four done-means conditions for the E37 capstone are met:

| Done-means | Status | Evidence |
|---|---|---|
| Zero operator workarounds | Met | Flow 2's open arm completed through the MCP chain; no direct-API call substituted for it |
| Zero settled-outcome-unknown acceptance stages | Met | Flow 1's `acceptance_outcome_recorded` verdict decoded cleanly on first read; no operator arbitration of an undecodable verdict occurred |
| Zero hand-retitles | Met | Flow 2's epic and three children were correct — `[E38]` / `[E38.1]` / `[E38.2]` / `[E38.3]` — at file time |
| Zero plan-revise passes attributable to missing acceptance criteria | Met | Flow 1's plan authored acceptance criteria on the first attempt; no revise pass occurred |

The closing evidence comment summarizing this record is posted on [#1645](https://github.com/kuhlman-labs/fishhawk/issues/1645).
