# backend/internal/escalation

Pure evaluation core for a workflow's per-path `escalations` declaration
(E53.4 / #2227). No I/O, no HTTP shapes, no persistence — the firing walk, the
composition hand-off to `spec`, the one operator-facing renderer, and the
fingerprint the audit de-duplication keys on.

It exists for the same reason `backend/internal/appliesto` does: two
enforcement seams in different packages consume the firing decision, and a seam
that grew its own copy of the walk would drift from the other. It imports only
`spec`; `spec` does not import it, so the dependency direction is verified by
compilation.

## Surface

| Symbol | Contract |
|---|---|
| `Evaluate(escalations, change)` | Walks declarations in order, matches each, composes the strictest requirement over the hits. A `Match` error is RETURNED with the zero `Result` — never swallowed. |
| `Result{Fired, Requirements}` | The zero value is "nothing fired, nothing raised" — what a workflow declaring none, and a change matching none, both evaluate to. |
| `RenderFired(res)` | THE operator-facing summary. Both the `escalation_fired` audit payload and the run read's `escalations.summary` render through it, so they cannot drift. |
| `Fingerprint(res)` | Stable hash over the rendered fired set + composed requirements; the audit de-duplication key. |
| `PersonaAttachments(res)` | The reviewer personas an evaluation attaches (ADR-084 D2(c) / E55.9 / #3754): the de-duplicated UNION of every FIRED escalation's `require.reviewers`, sorted by persona name, each `PersonaAttachment{Persona, Fired}` carrying the fired escalations naming it in declaration order. Reads `res.Fired` only, so a non-firing escalation contributes nothing ("cost only where attached"); sorted by name so it is a function of the fired SET. `TestPersonaAttachments_UnionSortedAttributed`, `_NonFiringContributesNothing`. |
| `RuleKey(e)` | Stable, content-derived key for ONE declaration (E75.1 / #3729) — the join key a decision index follows a rule by. Derived from that declaration's own match criteria (each list sorted on a COPY, because each is an unordered OR, and each element LENGTH-PREFIXED `<byte-len>:<value>` before the join, because the canonical rendering must be INJECTIVE over its own delimiters — a bare comma join renders `paths: ["a,b"]` and `paths: ["a", "b"]` identically and would give two different rules one key) plus its `require` clamp, and carrying NO positional index: reordering unrelated declarations, or editing another rule, leaves it unchanged; editing THIS rule's globs, labels, change kinds, triggers or clamp changes it. `require.reviewers` (E55.9 / #3754) is part of the clamp, rendered as a sorted list and ONLY when non-empty — so every rule declaring no reviewers keeps its pre-#3754 key (pinned literals in `TestRuleKey_ReviewersLessRuleKeyIsStable`) and permuting the list does not move it. NOT `Fingerprint` — that is the RESULT-level de-duplication key over a whole evaluation, this is the RULE-level content key for one declaration. |

## Seams

| Seam | Where | What it consumes |
|---|---|---|
| Approval gate | `backend/internal/server/quorum.go`, `approvals.go` | Raised `count`, the `member_of` CONJUNCTION and `min_permission`, at BOTH the quorum count and the pre-Submit 403 |
| Delegation resolution | `backend/internal/delegation` | The `max_autonomy` CEILING, applied LAST over the fully resolved matrix |
| Run read (legibility) | `backend/internal/server/runs.go` | The whole `Result`, projected onto the `escalations` response block |
| Plan / implement review loops | `backend/internal/server` (persona attachment) | `PersonaAttachments` over the FIRED set — never `Requirements` |

**`require.reviewers` is NOT in `spec.ComposedRequirements`, on purpose.** A
persona attachment raises the REVIEW, not the approval gate or the autonomy
clamp, and `Requirements.IsZero()` is what the approval gate reads to decide
whether anything was raised (its fetch-error fail-closed branch,
`snapshot.Escalated`, the 403 `escalated` flag). A reviewers-only escalation
therefore composes to the zero value and raises nothing at either seam
(`spec`'s `TestComposeEscalations_ReviewersOnlyIsZero`). It is still a FIRED
escalation, though: the server resolver writes `escalation_fired` whenever
`res.Any()` — not on `Requirements.IsZero()` — so a reviewers-only firing at the
approval / delegation seams DOES record an entry, rendered by `RenderFired` with
the fired rule and NO `Raised:` clause (`TestRenderFired_ReviewersOnlyFiring`).

The first three reach this package through the ONE server-side resolver
(`backend/internal/server/escalation_gate.go`), which is also the single
`escalation_fired` audit emit point; the review-loop persona attachment records
its own `escalation_persona_attached` entry (contract: the server README).

## Invariants

- **Fail-closed on a `Match` error.** `Evaluate` returns the error; every caller
  turns it into a refusal (a retryable 503 at the approval gate, an omitted
  delegation surface at the delegation seam). This is deliberately asymmetric to
  the host package's advisory sweeps (`runScopePrecheck`, `runSurfaceSweep`),
  which correctly fail OPEN — writing `if err != nil { return Result{}, nil }`
  here by analogy with them is the tempting bug.
- **Order-independence is structural.** `spec.ComposeEscalations` is max / min /
  set-union per dimension, each commutative and associative, so shuffling the
  declarations cannot change `Requirements`. Only the reported `Fired` slice
  keeps declaration order (for stable rendering).
- **Membership is a conjunction.** Two escalations naming disjoint groups
  produce a requirement no single approver can satisfy. That is the correct
  fail-closed reading for a control that may only raise: it surfaces as a gate
  that cannot clear, never one silently weakened.
- **Nothing is written here.** `Evaluate` knows no repository. The residual risk
  the design accepts is the inverse of the audit guarantee — a consumer that
  fires escalations WITHOUT going through the server resolver — bounded by that
  resolver being the only server-side entry point and by this package being
  pure.
