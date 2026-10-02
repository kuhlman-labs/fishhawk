---
id: ADR-077
title: "Bounding the get_run_status response: what limit, which boundary, and what an elision pointer honestly promises"
status: accepted
date: 2026-08-07
issue: https://github.com/kuhlman-labs/fishhawk/issues/2507
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-077: Bounding the get_run_status response: what limit, which boundary, and what an elision pointer honestly promises

## Context

#2493 reports that `fishhawk_get_run_status` on a failed run returned **426,000 characters**, exceeding the tool-result limit outright and forcing the operator to spill the response to a file. The E41 compaction work (#1727, #1749, #647, #1098) holds on the happy path and does not hold on the failure path.

Seven plan passes across two runs (`c63dca4d`, `143aea12`) failed to reach an approvable plan. The reviews were substantively correct every time, and the design that emerged is good. What repeatedly failed was the SPECIFICATION: three times a decision supplied to fix one defect created the next, and the final pass surfaced that the budget is not anchored to any measured limit at all. This ADR settles the questions a plan gate cannot.

**The design that emerged — carry it forward, do not rediscover it:**

- A deterministic tiered elision ladder, applied at one call site immediately before the handler returns; an `omitempty` elisions block so a healthy response stays byte-identical.
- The bound established by MEASURED re-check with a converging absolute floor, not by arithmetic alone (an arithmetic premise proved false twice).
- Constructor-only `ElidedField` construction with an unexported violation counter asserted zero.
- Escape-aware capping: `encoding/json` inflates raw bytes up to 6x, invalid UTF-8 is a distinct cost class (ranging yields U+FFFD while the encoder emits a different escape), U+2028/U+2029 take six bytes, and the two surrounding quote bytes must be reserved — so the cap contract is `jsonEncodedLen(result) <= max(budget, 2)`, since no JSON string encodes smaller than `""`.
- Per-field elision accounting; a reflection test forcing every future top-level field into a tier or failing.
- Deterministic key selection before retaining bounded map entries (Go map order is unstable).
- The adversarial matrix driven through the REAL handler — many stages, long `next_actions`, long failure reasons, invalid UTF-8 — never through the ladder alone.
- Audit pointers anchored so they return the DROPPED entries, never a bare newest-N.

## Options

**Q1 — What is the limit, and which boundary does the budget apply to?**

The originating defect is a tool RESULT exceeding a platform limit. The plans bounded the handler's marshalled return value and used 65,536 bytes, which the last plan admitted is a judgment call rather than a measured constant. Bounding the inner response while the operator hits the envelope does not fix the reported defect.

- (a) Measure the actual limit and bound the serialized envelope, deriving the constant with its derivation recorded.
- (b) Bound the inner response at a conservative fraction of a measured envelope limit, documenting the headroom rationale.
- (c) Keep an assumed constant and accept that the fix is approximate — explicitly not recommended, since it leaves the reported failure unproven.

**Q2 — What does the constructor do with a refused elision?**

The three-way classification (stored / oversized-capable / computed) has no valid normalization at the refusal path: a stored elision with no pointer, and an oversized-capable elision whose bounded pointer is stripped, cannot remain in their class without violating the invariant, and reclassifying either as computed would falsely describe stored content.

- (a) Make the refusal a programming error — panic or compile-time impossibility — so no runtime normalization is needed.
- (b) Add a fourth class for "stored but unretrievable", honestly reported as such.
- (c) Require a pointer at construction so the invalid state is unrepresentable.

**Q3 — What coverage does a pointer promise?**

Exact-set equality proved unattainable where only a prefix is dropped (`since_sequence=0` returns a superset because the client omits the param at zero and the REST filter is strictly-greater-than), and T6's whole-slice test establishes containment plus a newest-N suffix, which against a longer chain is also a superset.

- (a) One uniform promise: a pointer returns AT LEAST the omitted content and never a bare newest-N that excludes it. Simple, honest, uniformly verifiable.
- (b) Per-tier promises (exact where the whole slice drops, superset where a prefix drops) — accurate but two contracts to verify and easy to blur, as the last pass showed.

**Q4 — Which surfaces must an aggregate elision name?**

The absolute floor drops stage failure reasons but its aggregate named only the run and audit endpoints, omitting the stages endpoint earlier tiers rely on for exactly that content.

- (a) An aggregate must name the union of every surface its members would have named individually.
- (b) The floor emits one aggregate per surface rather than one overall.

## Recommendation

**Q1: (a) or (b), with the measurement done first.** This is the load-bearing decision and it should be settled before any implementation resumes — everything else is refinement of a bound whose target is currently unknown. Measure what the operator actually hits, then decide whether to bound the envelope directly or the inner response with recorded headroom.

**Q2: (c), require a pointer at construction** where the class demands one, making the invalid state unrepresentable rather than normalized after the fact. (a) is an acceptable complement for genuine programming errors. Avoid (b) unless Q1's measurement shows genuinely unretrievable stored content exists in practice.

**Q3: (a), one uniform promise.** The exact-vs-superset split was introduced mid-revision to resolve a specific finding and immediately produced another where the same distinction was blurred. "At least the omitted content, never a bare newest-N" is honest, uniformly testable, and removes a whole class of false coverage claims.

**Q4: (a), the union of member surfaces.** Cheapest to state and to verify, and it keeps the floor's promise identical in kind to every other tier's.

## Decision


## Consequences

Settling these makes the remaining work a straightforward implementation of an already-designed ladder rather than an eighth planning round. #2493 stays open as the implementation ticket and should be re-scoped against this ADR's decisions once recorded.

If Q1's measurement shows the envelope limit is materially different from 65,536, the tier thresholds and the floor's arithmetic both need re-deriving — which is precisely why it must be answered first.

Cost of not doing this: seven plan passes, roughly $20, and no code. Every rejection was substantively correct; several caught defects that would have shipped a false green in a change whose entire purpose is honest reporting. The heterogeneous review panel disagreeing — one reviewer approving while the other rejected on specification grounds — is what surfaced the gap at all, and is worth noting as evidence for keeping it.

Evidence runs: `c63dca4d-ab92-4e98-b7fd-00d10d884ce7` (passes 1-3), `143aea12-9d10-4782-accc-26a73c5d1d23` (passes 4-7). The full design record and the earlier failure analysis are in the #2493 issue thread.


---

## Decision

Recorded 2026-08-07 after direct measurement.

### Q1 — the limit, MEASURED

The limit is **token-based, not byte-based**: the client rejects with *"result (N characters) exceeds maximum allowed **tokens**"*. fishhawkd cannot tokenize, so a byte budget is necessarily a **proxy** and must carry headroom for the worst chars-per-token ratio.

Rather than estimate that ratio, the character threshold was bracketed empirically by driving `fishhawk_list_audit` at increasing `limit` through the real MCP client:

| Probe | Result chars | Outcome |
|---|---|---|
| `limit=45` | 42,280 | **succeeded** |
| `limit=55` | 54,262 | failed |
| `limit=80` | 81,530 | failed |
| `start_campaign_item_run` (independent) | 75,963 | failed |

**Measured threshold: 42,280 < T < 54,262 characters.**

**DECISION: budget = 32,768 bytes (32 KiB)**, derived as the largest confirmed success (42,280) with a ~20% margin. That is 22% below a proven success and 40% below a proven failure. The derivation above must appear in the constant's comment — the number is evidence-backed, not chosen.

Two consequences:

- **Bound the inner response, not the envelope.** The server never sees the envelope, so this is the only implementable boundary. The 32 KiB proxy absorbs envelope framing within its margin.
- **Make it configurable.** This threshold was measured against one MCP client; another may differ. Ship the constant as the default, overridable.

### Q2 — refused elisions: **(c)**, require the pointer at construction

Constructors take the retrieval surface as a parameter so the invalid state is unrepresentable, rather than normalized after the fact. Fields unexported, construction-only. A genuine programming error may additionally panic.

### Q3 — pointer coverage: **(a)**, one uniform promise

A pointer returns **AT LEAST** the omitted content and is never a bare newest-N that excludes it. The per-tier exact/superset split was tried and immediately produced a defect where the distinction was blurred. Where exact equality happens to hold, asserting it is welcome; the *contract* is at-least.

### Q4 — aggregate surfaces: **(a)**, the union of member surfaces

An aggregate elision names every surface its members would have named individually. Cheapest to state and verify, and identical in kind to every other tier's promise.

### Additional decision — third elision class

Not in the original options, surfaced by the plan gate and adopted: elisions are **stored** (bounded surface, must retrieve), **oversized-capable** (may exceed any bounded surface — points at an UNBOUNDED surface: REST, artifact, or log), or **computed** (never stored — no pointer, says so plainly). The constructor refuses a bounded pointer for an oversized-capable field, and refuses any pointer on a computed one. Derived economics (`cost`, `cache_efficiency`, `latency`, `budget`) are **computed** — they are recomputed at read time, and recomputation is not retrieval.

**Status: decided.** #2493 is re-scoped against these decisions; implementation proceeds from here.
