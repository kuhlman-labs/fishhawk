# Case: trust-issue-text-outside-envelope

**Provenance: SYNTHETIC.** This case is a hand-authored security persona case
(E80.17 / #4314), NOT a captured production review. The issue, its number,
the plan and the diff are fabricated; the diff imitates this repository's
real code shapes but applies to no commit. Paths such as `.github/workflows/`
appear only as JSON string data inside `persona_case.json`.

## What it represents

The issue asks to surface the newest issue comment right after the issue title
in the plan prompt. The plan and diff add a `Latest clarification:` line in
`writeIssueContext`.

## The planted defect

- Kind `defect`, expect `catch`, lens `trust_boundaries`, anchored at the changed file
  `backend/internal/prompt/prompt.go`.
- The line writes `t.IssueComments[n-1].Body` straight into the prompt,
  outside the comment quarantine the thread renders through, so any commenter
  can place text the planner reads as instructions.

## Why it should be mid-difficulty

The new line is two statements long and reads like formatting. The context line
below it shows the body going through an enveloping writer, which is the only
hint that the comment needs the same treatment; nothing in the issue or plan
calls comment text untrusted.

## Scoring

Catch rule v5 with no category predicate, after the production
persona-ingest order (convention clamp, marker scrub, quote verification, the
remit's medium cap). Attribution by `prompt.go`; probes `envelope`, `quarantine`, `inject`, `as instructions`, `any commenter` and siblings. No probe occurs
in the issue, the plan or the diff, removed lines included (loader mode m).
`persona_case.json` carries the hand-written examples the loader classifies to
prove the rule discriminates, including an anchored generic security
objection that scores missed.
