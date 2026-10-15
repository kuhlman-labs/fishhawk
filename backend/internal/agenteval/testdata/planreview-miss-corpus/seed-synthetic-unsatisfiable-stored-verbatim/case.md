# Case: seed-synthetic-unsatisfiable-stored-verbatim

**Provenance: SYNTHETIC.** This case is a hand-authored plan-review miss
(E55.23 / #4361), NOT a captured production triage entry. It is a SYNTHETIC
reconstruction of a defect class this repository hit, not a captured run: it
plants exactly one defect the standard plan-review criteria should catch — an
inferred criterion requiring a byte-exact round-trip that the issue never
asks for and that the plan's own JSONB storage cannot satisfy — so the
plan-review catch-rate gate (`planreviewcatch.go`) has a committed,
discriminating fixture. Nothing here is a real run, issue or repository: the
`example-org/hooks-service` issue, the run ids and the triage sequence are
fabricated placeholders, and `miss.json` carries `"synthetic": true`.

## The class it reconstructs

Postgres JSONB stores a decomposed form: it does not keep the input's
whitespace, key order or duplicate keys, so a value read back is RE-ENCODED,
not byte-identical. This repository records that limit for its own comms
dispositions (`backend/internal/server/comms_dispositions.go`, the condition 3
note: "RE-ENCODED, not byte-identical: Postgres JSONB normalizes key order");
a criterion demanding byte-exactness through such a column can never pass.

## What it represents

The issue asks to store inbound webhook deliveries so support can inspect and
replay them; it never asks for a byte-exact copy. Approach step 1 stores the
payload in a `jsonb` column of `hook_deliveries`. The plan's INFERRED criterion
`ac-delivery-payload-verbatim` requires `GET /hooks/deliveries/{id}` to return
the payload byte-for-byte as received, original whitespace and key order
included, with a rationale about replaying the partner's exact payload. The
implementation stored JSONB as planned, inspection and replay worked,
acceptance failed the criterion on the re-encoded bytes, and triage
classified it class-3.

Mid-difficulty by design: the rationale sounds like a reasonable replay
property, and the infeasibility shows only when a reviewer connects the
criterion to the column type in approach step 1.

## The planted defect

- Criterion id: `ac-delivery-payload-verbatim` (present, byte-identical, in
  both `miss.json` and the plan inside `review_input.json` — the loader
  enforces both).
- Plan-review criterion it violates: 9 (Warrant of inferred criteria) —
  the issue never asks for a byte-exact round-trip, so nothing warrants the
  requirement; it is also unsatisfiable under the plan's own storage.

## Review input

`review_input.json` carries the issue text the reviewer sees, the complete
`standard_v1` plan under review, the `catch_probes` (case-folded substrings
matched against each emitted concern's note and category; they include the
warrant phrasing of `catchRuleV4InferredProbes` beside the infeasibility
phrasing), the `attribution_phrases` (substrings of the planted criterion's
statement and of no other criterion's), the `defect_phrases` the prompt-leak
self-check reads (`jsonb` and domain wording only), and the hand-written
`catching_examples` / `non_catching_examples` the loader uses to prove the
rule discriminates.
