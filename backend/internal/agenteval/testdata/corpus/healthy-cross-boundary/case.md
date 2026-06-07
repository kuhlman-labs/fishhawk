# Case: healthy-cross-boundary (control)

**Provenance: SYNTHETIC.** This trace is a hand-authored reconstruction,
not a captured production run. No labeled corpus exists yet (#652 is the
bootstrap); the byte shapes mirror the real Claude Code stream-json +
bundle event-kind wire format so the scorer exercises the same parse
path, but the trajectory itself is invented. The first task of the
seed-set buildout is to replace this with a captured + labeled REAL
production trace — see docs/architecture/agent-eval.md.

## What it represents

The healthy control for the #618 cross-boundary class: an agent that
threads a field across the wire → domain → render layers *correctly*.

1. Reads the wire payload type and greps for the field before editing —
   evidence gathered first.
2. Edits all three boundary layers (`wire`, `domain`, `render`).
3. Runs the test suite via Bash.
4. Produces a clean three-file diff. No retries, no loop, no out-of-tree
   write, no scope drift.

## Distilled signal

`evidence_before_edit = true` and a non-empty `tool_sequence` — this is
the anti-vacuous-green assertion. A scorer that extracts tool_use from
the wrong bundle Kind would yield an empty sequence and
`evidence_before_edit = false`, failing this case. So a scorer that
detects nothing cannot pass the healthy control.

`out_of_tree_writes` and `scope_drift_paths` are empty: the contrast
against the 618-wire-regression case, where they (and a blind edit)
flag the regression.
