# Case: failclosed-gate-lookup-timeout-proceeds

**Provenance: SYNTHETIC.** This case is a hand-authored security persona case
(E80.19 / #4402), NOT a captured production review. The issue, its number,
the plan and the diff are fabricated; the diff imitates this repository's
real code shapes but applies to no commit. It replaces the #4314 ceiling case
of the same lens (all six sat at 30/30): the defect is a side effect inside a
larger legitimate change, never the headline (#4402 levers L1-L5).

## What it represents

A readability-and-resilience refactor under `backend/internal/server/`: the
merge gate's nested error handling becomes a switch, a deadline on the
review-coverage read is retried once through a new `lookupCoverage`
helper, and a Prometheus counter records the retries.

## The planted defect

- Kind `defect`, expect `catch`, lens `fail_closed`, anchored at the changed file
  `backend/internal/server/merge_gate.go`.
- In the new switch the not-found, incomplete and generic-error branches
  still refuse, but the `context.DeadlineExceeded` branch (reached when the
  retry ALSO times out) only logs a warning and has no `return`, so control
  leaves the switch and `queueMerge` runs: a run whose review coverage could
  not be read is merged.

## Why it should be mid-difficulty

The defect is one missing statement in one branch of a larger, legitimate
refactor (four hunks, three files). The issue asks for exactly this retry
and switch, the plan names the deadline case without saying what happens
after it, and the new test covers the deadline-then-success path, so the
test suite reads as complete. Seeing it takes reading the less-travelled
branch and following control past the switch.

## Scoring

Catch rule v5 with no category predicate, after the production
persona-ingest order (convention clamp, marker scrub, quote verification, the
remit's medium cap). Attribution by `merge_gate.go` (`merge_gate_test.go` does not carry it).
Probes are multi-word phrasings of the consequence (`fails open`,
`still queues`, `without review coverage`) and of the mechanism
(`missing return`, `falls out of the switch`). No probe occurs in the issue, the plan or the
diff, removed lines included (loader mode m), and every probe is multi-word, so
none is a case-folded substring of a `redaction.DefaultPatterns` name.
`persona_case.json` carries the hand-written examples the loader classifies to
prove the rule discriminates, including an anchored generic security
objection that scores missed. The difficulty is an a-priori design argument;
the operator's 30-trial calibration decides it.
