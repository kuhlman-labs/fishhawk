# Case: no-diff-noop (benign no-op terminal)

**Provenance: SYNTHETIC.** This trace is a hand-authored reconstruction,
not a captured production run. No labeled corpus exists yet (#819 is the
buildout to 30–50 real cases); the byte shapes mirror the real Claude
Code stream-json + bundle event-kind wire format so the scorer exercises
the same parse path, but the trajectory itself is invented. Replacing it
with a captured + labeled REAL production trace stays open under #819 —
see docs/architecture/agent-eval.md.

## What it represents

The benign no-op terminal: an agent that runs, inspects the tree, and
concludes no change is warranted.

1. Reads the domain type and greps for the field — evidence gathered.
2. Produces no edits: no file-writing tool_use, no `git_diff` event.
3. The manifest's `agent_failed` flag is unset, so this is not a
   category-A failure either.

## Distilled signal

`outcome = no_diff` is the previously-uncovered `deriveOutcome` branch:
the bundle carries neither an `agent_failed` manifest flag (→
`agent_failed`) nor a `git_diff` event (→ `diff_produced`), so the
function falls through both checks to the benign `no_diff` terminal. This
case is the fixture-replay complement to `TestDeriveOutcome`, contrasting
the `diff_produced` healthy control (healthy-cross-boundary) and the
`agent_failed` branch.

`evidence_before_edit` is true: a read-class call (`Read`) is reached
before any write, and there is no write at all. `out_of_tree_writes` and
`scope_drift_paths` are empty — no boundary violation, no drift.
