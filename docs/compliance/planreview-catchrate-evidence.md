# Plan-review catch-rate evidence (E55.4 / #2245)

Operator run-book for the LIVE two-arm measurement that decides whether the
E55.3 / #2244 repository review-conventions section dilutes the plan
reviewer's catch rate. Sibling of
[`severity-calibration-evidence.md`](severity-calibration-evidence.md), and it
ships with the same honest residual.

## THE BASELINE HAS NOT BEEN RECORDED

The results table at the bottom of this document is **explicitly
unpopulated**, and no
`backend/internal/agenteval/testdata/planreview-catchrate/evidence.json` is
committed. #2245 shipped the measurement APPARATUS and the offline gate, not
the measurement:

- The offline tests run in every `scripts/test verify` and make **no model
  call**. They prove the arms differ only by the conventions section, that
  the catch probes discriminate, and that every evidence-check mode fails
  closed.
- They prove **nothing** about whether conventions dilute the reviewer.
- Until an operator records and pins the first baseline, the gate command
  (`catchrategate`) exits 1 on the committed tree, so the standing check
  `scripts/check-review-prompt-eval` **fails closed on every review-prompt
  change**. That is by design: conventions already shipped in #2244 without
  evidence.

This is not a shortfall being papered over. The runner sanitizes
gate-subprocess environments through a default-deny allow-list that names
`ANTHROPIC_API_KEY` on `gateEnvDeny`
(`runner/cmd/fishhawk-runner/gateenv.go`), so no in-loop test can obtain a
model credential. The measurement is operator-executed and operator-paid.

## What the gate decides

| Rule | Fails when |
|---|---|
| within-run | the with-conventions catch rate is MORE than 0.10 below the without-conventions rate of the SAME measurement (exactly 0.10 passes) |
| pinned baseline | either arm's catch rate is MORE than 0.10 below the SAME arm of the PINNED baseline |
| power floor | either arm has fewer than 136 trials — REFUSED, never passed |

The baseline is PINNED, not rolling: a recording is never judged only against
the previous one, so a series of in-tolerance drops cannot erode the
reference. A measurement that fails any rule is REFUSED and the evidence file
is left byte-identical, so a failed run can never become a baseline.

**Read the floor honestly.** The 136-trial floor bounds MODEL-SAMPLING NOISE
ONLY (the one-sided 95% worst-case bound `1.645*sqrt(0.5/n)` on the arm
difference is then at most the 0.10 tolerance). The corpus size — six
synthetic planted-defect shapes — not the trial count, bounds what the gate
can detect. A dilution that spares those six shapes is invisible to it. The
0.10 tolerance and the 95% level are judgement calls.

## Step 1 — check the corpus loads

```sh
scripts/test single -run 'TestLoadPlanReviewCatch|TestCatchRateArms' ./backend/internal/agenteval/
```

Every corpus case needs a hand-curated `review_input.json` (shape and probe
matcher: `backend/internal/agenteval/README.md` § "Corpus authoring").

## Step 2 — run the arms

Authenticate with exactly one of `FISHHAWKD_ANTHROPIC_API_KEY` or
`FISHHAWKD_ANTHROPIC_AUTH_TOKEN` (an OAuth bearer; substitute it for the key in
any command below). Neither set skips the arm naming both variables; both set
fails it. Which bearer tokens Anthropic's terms permit for direct API use is the operator's responsibility.

Dry run (measures and judges against the committed baseline, writes
nothing):

```sh
FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
  scripts/test single -run TestPlanReviewCatchRateLive ./backend/internal/agenteval/
```

Record a measurement (`RECORD=1`). The FIRST recording must also pin the
baseline, with a reason:

```sh
FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
  FISHHAWK_AGENTEVAL_PLANREVIEW_RECORD=1 \
  FISHHAWK_AGENTEVAL_PLANREVIEW_PIN_REASON='first measurement after #2245' \
  scripts/test single -run TestPlanReviewCatchRateLive ./backend/internal/agenteval/
```

Later recordings omit `PIN_REASON`; the pinned baseline is carried forward
unchanged. Moving it is the separate, explicit action of setting
`PIN_REASON` again: it still requires a passing measurement (including
against the current baseline when that baseline is comparable) and records
the reason and a timestamp in the evidence. After a corpus change the old
baseline is not comparable, an ordinary recording is refused, and you re-pin.

Cost: each measurement is two arms of `ceil(136 / cases) x cases` plan-review
calls — 2 x 138 with the six committed cases — against
`DefaultQualityGeneratorModel`. Sampling cannot be pinned (`anthropic.Config`
has no temperature knob); the power floor is the answer to run-to-run
variance.

## Step 3 — commit the evidence and confirm the gate

```sh
(cd backend && go run ./internal/agenteval/catchrategate)
git add backend/internal/agenteval/testdata/planreview-catchrate/evidence.json
```

`catchrategate` exits 0 with the rendered report when the record passes.
Every outcome, pass or fail (absent, stale or malformed evidence and an
unavailable corpus included), ends with the rule line naming the 0.10
tolerance, the pinned-baseline rule and the 136-trials-per-arm floor. It
recomputes the verdict from the counts and refuses an absent, malformed,
stale, under-powered or regressed record (the twelve modes:
`backend/internal/agenteval/README.md` §
"`CheckCatchRateEvidence` fail-closed modes"). A record goes STALE when the
corpus, the conventions fixture, the rendered plan-review prompt, the
generator model or the catch rule changes; `--print-fingerprint` prints the
current fingerprint to compare with the record's `prompt_fingerprint`.
Re-measure on staleness — never hand-edit the record.

## Results

| Recorded at | Generator model | Without conventions | With conventions | Delta | Pinned baseline | Verdict |
|---|---|---|---|---|---|---|
| — not yet measured — | | | | | | |
