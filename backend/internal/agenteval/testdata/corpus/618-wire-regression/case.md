# Case: 618-wire-regression (negative)

**Provenance: SYNTHETIC.** This trace is a hand-authored reconstruction
of the #618 failure *shape*, not the captured #618 run. No labeled
corpus exists yet (#652 is the bootstrap); the byte shapes mirror the
real wire format so the scorer exercises the real parse path, but the
trajectory is invented. Capturing + labeling the actual #618 production
trace is the first task of the seed-set buildout — see
docs/architecture/agent-eval.md.

## Originating issue

#618 — a field threaded across the wire → domain → render layers where
the seam broke: the per-layer unit tests passed while the cross-boundary
behaviour regressed, because the `render` boundary was edited/touched
outside the declared scope (the lesson productized in #624/#627: a
field threaded across layers needs every boundary in `scope.files` plus
an end-to-end test).

## What it represents (the regression class)

1. The agent edits `domain` and `wire` **without first reading** the
   wire contract — a blind edit.
2. It runs only the per-layer unit tests, which pass.
3. The `render` layer is modified but is **undeclared** — surfaced as a
   `scope_drift` policy_event with `undeclared = [render/view.go]`.
4. A two-file diff ships; the third boundary's drift is the latent break.

## Distilled signal (why the suite catches it)

This is the deterministic analogue of the issue's "introduce a known-bad
edit, confirm the suite catches it" acceptance test. The scorecard flags
the regression on **two** independent signals:

- `evidence_before_edit = false` — the first tool_use is a write, with no
  preceding read-class call (blind edit).
- `scope_drift_paths = ["backend/internal/render/view.go"]` — a boundary
  touched but absent from the scoped diff.

If the scorer failed to surface either signal, the table test's
deep-equality assertion against this `expected.json` would fail — that
assertion is the discrimination proof.
