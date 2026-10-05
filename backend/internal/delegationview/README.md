# delegationview

The per-workflow **delegation read** (E76.1 / #3747): for each workflow a
repository's spec declares, what may the operator agent decide on its own, and
what does an escalation take back?

This package is PURE. It takes a `*spec.Spec` and returns a projection. It
performs no I/O, reads no run, mints no audit entry and grants no authority.
The HTTP half is `backend/internal/server/delegation_view.go`
(`GET /v0/repos/{owner}/{name}/delegation`); the MCP half is
`fishhawk_delegation` in `backend/internal/mcpserver/delegation_view.go`.

## Why it exists

The run-side delegation block (`GET /v0/runs/{id}` → `delegation`) answers the
same question for ONE live run, by EVALUATING each class's condition against
that run's state. It is therefore unavailable exactly when the question is most
useful: before any run exists, when a reviewer is deciding whether this
repository's spec delegates more than it should.

So this read is the DECLARATION side of the same contract, projected from the
spec at a ref.

## What it projects

Per workflow, in **sorted workflow-id order**:

| Field | Source |
|---|---|
| `autonomy` | the declared tier shorthand, empty when only `actions` was declared |
| `matrix` | `spec.ResolveAutonomy(&wf, nil)` — every class with its `mode` and the PROVENANCE of that mode (`tier` / `explicit` / `default` / `escalation`) |
| `must_page_human` | `ResolvedMatrix.PageHumanOn` — the same list a run's delegation block surfaces |
| `model_policy` | the resolved block's `model_policy` |
| `escalations[]` | each declared escalation's match criteria, its raised approvals, its `max_autonomy` ceiling, and the `ceiling_matrix` that ceiling produces |
| `content_hash` | see below |

## Two contracts a reader must not misread

**The matrix is the WORKFLOW-level one.** `ResolveAutonomy` is called with a
nil gate. That is deliberate: the per-workflow read answers what the WORKFLOW
delegates, and a gate-level `autonomy`/`actions` block overrides wholesale only
at that gate. A workflow whose gates declare their own blocks therefore shows
the workflow-level matrix here — the same block `delegation.Evaluate` starts
from before a gate override wins. If a consumer later needs the per-gate
matrices, that is an additive field, not a change to this one.

**`ceiling_matrix` is what a ceiling WOULD produce, not what is in force.** The
escalation projection is declarative: it echoes each escalation's match globs
and shows the clamped matrix, but it does NOT evaluate whether the escalation
fires. Firing depends on an approved plan's `scope.files`, which does not exist
before a run. The field is named `ceiling_matrix` rather than anything
suggesting it is active for that reason. A workflow's own `matrix` is never
clamped here.

The clamp itself is `spec.ClampResolvedMatrix` — the SAME function the run-side
delegation seam applies, deliberately not a second implementation, so a change
to how a ceiling composes cannot leave this read reporting a laxer matrix than
the one that will actually be enforced.

## The content hash

Every workflow entry and the whole view carry a `content_hash`: `sha256` over
the projected delegation content, hex-encoded. A later re-confirmation
(ADR-083) can bind to exactly what was shown.

What the hash covers and — load-bearing — what it does NOT:

- `hashWorkflow` marshals the entry with its own `ContentHash` **zeroed**, so
  the field the hash lands in is never an input to it.
- `HashWorkflows` marshals the ordered slice of already-hashed entries.
- Neither hash function ever sees `repo`, `source`, `ref`, `workflow_sha`,
  `spec_version` or `schema_major`. Those live on `View` only, OUTSIDE the
  hashed structure. Reading the SAME delegation at two different refs — or
  through the two different sources — therefore yields the SAME hash.
  `TestContentHash_StableAcrossRefAndSource` pins that direction and
  `TestContentHash_ChangesOnTierAndEscalation` pins the other, so a
  constant-return hash fails the pair.

**Determinism rests on two properties**, both asserted rather than assumed:

1. `Project` walks the spec's workflow map in SORTED id order. A Go map range
   is deliberately randomized, so ranging it would make the view and its hash
   non-deterministic. `TestProject_IsSortedByWorkflowID` pins the order and
   re-projects the same spec for byte-identity.
2. Every view type is a plain struct with **no map field**. `encoding/json`
   marshals struct fields in declaration order and slice elements in order
   (<https://pkg.go.dev/encoding/json#Marshal>), so the marshalled bytes are a
   function of the content alone.

The per-workflow hash is stamped BEFORE the handler's optional `?workflow=`
filter, so a filtered read returns the same per-workflow hash an unfiltered one
does; only the view-level hash reflects the retained set. A consumer binding a
confirmation to one workflow should bind to the per-workflow hash.

### `HashMatrix`: one resolved matrix

`HashMatrix(actions []spec.ResolvedAction)` is `sha256` over `actionsFrom(actions)`. It uses the same `Action` wire mirror and the same hashing as the read above, so a resolved matrix hashes to the digest its projected `matrix` would carry here (`TestHashMatrix_DeterministicAndSensitive` pins that byte for byte). Its consumer is the delegation shadow stamp (E82.1 / #3778), which hashes the run's escalation-CLAMPED matrix as the stamp's matrix stratum.

A nil or empty matrix hashes the empty `Action` slice: a stable, non-empty digest. "No matrix governs the run" is therefore its own comparable stratum rather than an empty string that a hashing failure could also produce. Like the other hashes, it never sees a volatile field.

## Fail-closed readings

- A workflow declaring NO autonomy block resolves to nil and projects an
  **empty matrix** with an empty tier: nothing delegated. Never a nil
  dereference, never an inferred tier.
- An explicit `actions` entry wins for THAT CLASS ONLY; every other known class
  falls to `mode: gated`, `source: default`. That is `ResolveAutonomy`'s
  wholesale-override semantics, surfaced with its provenance rather than
  flattened.
- `Project(nil)` returns an empty slice, so the JSON surface is `[]` and never
  `null`.

## Anchor

#3747 (E76.1). The escalation grammar it projects is ADR-066 / #2222
(`autonomy` + `actions`) and E53.4 / #2227 (per-path escalations and the
`max_autonomy` clamp).
