# Plan-coercion golden corpus

Shared, committed test data for the plan-artifact coercion logic that is mirrored
in two hand-maintained packages: `backend/internal/plan` and `runner/internal/plan`.
Both packages expose an identical `TryCoerce(data []byte, now time.Time) ([]byte, []Coercion, error)`,
and both must stay in lockstep — a divergence between the mirrors was the root
cause of #832 (the runner failed to coerce object-form `ticket_reference` inputs
that the backend coerced), and this corpus is the cross-module drift guard
(#834).

Each module has a `TestCoercionCorpus` test that walks every `*.json` case here
(reached via `../../../testdata/coercion-corpus/` from each package's test dir,
which both resolve to repo root) and runs its own `TryCoerce` against it. If
either mirror drifts, that module's corpus test fails — exactly the seam that
per-module unit tests leave unguarded.

This is plain test data, **not** a schema. It is NOT under `docs/spec/` and is
NOT an embedded copy, so it is out of scope for `scripts/sync-schemas` and the
schema-sync CI gate.

## Case file format

Each `*.json` file is one self-describing case object:

| Field | Type | Meaning |
|---|---|---|
| `name` | string | Subtest name. |
| `input` | object | Raw pre-coercion plan-artifact JSON passed to `TryCoerce`. |
| `expected_output` | object (optional) | The plan JSON after coercion. Compared semantically (key order / whitespace insensitive). For zero-coercion cases this equals `input` (content-hash stability: `TryCoerce` returns the original bytes). Omit for `expect_error` cases. |
| `expected_coercion_count` | integer | EXACT number of `Coercion` records `TryCoerce` must return. `0` for the well-formed-object and already-valid cases — a scorer that silently dropped a coercion, or spuriously coerced a well-formed input, fails here. |
| `expected_coercions` | array (optional) | Subset of `Coercion` records that MUST be present, each pinning `field_path`, `original_type`, and optionally `coerced_to`. |
| `expect_error` | bool (optional) | When true, `TryCoerce` must return a non-nil error (non-coercible input that stays schema-invalid). |

`expected_coercion_count` is the exact-count assertion; `expected_coercions` pins
the shape of specific records on top of it.
