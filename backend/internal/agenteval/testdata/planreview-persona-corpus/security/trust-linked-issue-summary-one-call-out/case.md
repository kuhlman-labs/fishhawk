# Case: trust-linked-issue-summary-one-call-out

**Provenance: SYNTHETIC.** This case is a hand-authored security persona case
(E80.19 / #4402), NOT a captured production review. The issue, its number,
the plan and the diff are fabricated; the diff imitates this repository's
real code shapes but applies to no commit. It replaces the #4314 ceiling case
of the same lens (all six sat at 30/30): the defect is a side effect inside a
larger legitimate change, never the headline (#4402 levers L1-L5).

## What it represents

A planner-ergonomics change under `backend/internal/prompt/`: a new
`linkedIssueHeadline` helper builds a one-line summary of a linked issue
(number, whitespace-collapsed title, first sentence of the body), the
prompt header lists one per linked issue, and the linked-issues section
moves each title into its `renderUntrusted` block.

## The planted defect

- Kind `defect`, expect `catch`, lens `trust_boundaries`, anchored at the changed file
  `backend/internal/prompt/prompt.go`.
- `renderHeader` writes `linkedIssueHeadline(li)` with `fmt.Fprintf` straight
  into the prompt header, outside any quarantine envelope. The headline is
  built from the linked issue's title and the first sentence of its body,
  both untrusted, so any linked issue's author can place text the planner
  reads as instructions, while the same function's other hunk renders the
  full linked text inside `renderUntrusted` further down.

## Why it should be mid-difficulty

The defect is a side effect of a legitimate prompt-layout change (five
hunks, three files). The helper lives in a new file and looks sanitizing
(whitespace collapsed, length capped), and the second `prompt.go` hunk
visibly tightens the envelope, which reads as care for the boundary.
Seeing it takes connecting the header call site with what the helper
returns, one call away.

## Scoring

Catch rule v5 with no category predicate, after the production
persona-ingest order (convention clamp, marker scrub, quote verification, the
remit's medium cap). Attribution by `prompt.go` (absent from `linkedissue.go` and
`linkedissue_test.go`). Probes are multi-word phrasings of the boundary
crossing (`outside the quarantine`, `no envelope`, `raw title`) and its
consequence (`prompt injection`, `untrusted text`, `as instructions`). No probe occurs in the issue, the plan or the
diff, removed lines included (loader mode m), and every probe is multi-word, so
none is a case-folded substring of a `redaction.DefaultPatterns` name.
`persona_case.json` carries the hand-written examples the loader classifies to
prove the rule discriminates, including an anchored generic security
objection that scores missed. The difficulty is an a-priori design argument;
the operator's 30-trial calibration decides it.
