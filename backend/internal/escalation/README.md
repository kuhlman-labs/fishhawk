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

## Seams

| Seam | Where | What it consumes |
|---|---|---|
| Approval gate | `backend/internal/server/quorum.go`, `approvals.go` | Raised `count`, the `member_of` CONJUNCTION and `min_permission`, at BOTH the quorum count and the pre-Submit 403 |
| Delegation resolution | `backend/internal/delegation` | The `max_autonomy` CEILING, applied LAST over the fully resolved matrix |
| Run read (legibility) | `backend/internal/server/runs.go` | The whole `Result`, projected onto the `escalations` response block |

All three reach this package through the ONE server-side resolver
(`backend/internal/server/escalation_gate.go`), which is also the single
`escalation_fired` audit emit point.

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
