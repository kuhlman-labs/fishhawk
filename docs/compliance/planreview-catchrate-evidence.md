# Plan-review catch-rate evidence (E55.4 / #2245)

Operator run-book for the LIVE two-arm measurement that decides whether the
E55.3 / #2244 repository review-conventions section dilutes the plan
reviewer's catch rate. Sibling of
[`severity-calibration-evidence.md`](severity-calibration-evidence.md), and it
ships with the same honest residual.

## Baseline status: first pinned 2026-10-10; re-pin required by #4253

The first baseline was recorded and pinned on 2026-10-10 on
`claude-sonnet-5-5` (row in the results table at the bottom;
`backend/internal/agenteval/testdata/planreview-catchrate/evidence.json`),
over six cases under the probe-only `planreview-catch-v2` rule. E55.18 /
#4253 changed the catch rule (`planreview-catch-v3`: one concern must name
the planted criterion AND match a probe), grew the corpus to twelve cases and
added the trial sidecar, so that record is stale and its baseline is not
comparable: the next recording must RE-PIN (Step 2) and commit the record
with its sidecar (Step 3). The current pinned baseline is whatever
`evidence.json` carries; the results table lists every recording.
#2245 shipped the measurement APPARATUS and the offline gate; the measurement
is the operator's recording:

- The offline tests run in every `scripts/test verify` and make **no model
  call**. They prove the arms differ only by the conventions section, that
  the catch rule discriminates, and that every evidence- and sidecar-check
  mode fails closed.
- They prove **nothing** about whether conventions dilute the reviewer; only
  the recorded evidence speaks to that, and only for the committed
  planted-defect shapes.
- The gate command (`catchrategate`) passes on the committed record until the
  record goes stale (a prompt, corpus, fixture, model or catch-rule change),
  after which `scripts/check-review-prompt-eval` **fails closed on the next
  review-prompt change** until an operator re-measures.

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
difference is then at most the 0.10 tolerance). The corpus — the committed
synthetic planted-defect shapes, two or more per class — not the trial count,
bounds what the gate can detect. A dilution that spares every committed shape
is invisible to it. The 0.10 tolerance and the 95% level are judgement
calls.

## Step 1 — check the corpus loads

```sh
scripts/test single -run 'TestLoadPlanReviewCatch|TestCatchRateArms' ./backend/internal/agenteval/
```

Every corpus case needs a hand-curated `review_input.json` (shape, the
attribution-AND-probe catch rule and the loader's fail-closed modes:
`backend/internal/agenteval/README.md` § "Corpus authoring"). The corpus
holds twelve cases as of #4253 (`seed-synthetic-*`, two or more per weak
class).

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

**The #4253 recording is such a re-pin.** The case set changed (six cases to
twelve), so `samples_per_case` changed too, and the 2026-10-10 baseline is not
comparable; record with a reason that says so:

```sh
FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
  FISHHAWK_AGENTEVAL_PLANREVIEW_RECORD=1 \
  FISHHAWK_AGENTEVAL_PLANREVIEW_PIN_REASON='#4253: catch rule v3 and the twelve-case corpus; the 2026-10-10 baseline is not comparable' \
  scripts/test single -run TestPlanReviewCatchRateLive ./backend/internal/agenteval/
```

Land that recording commit on the #4253 run branch before merge.

Cost: each measurement is two arms of `ceil(136 / cases) x cases` plan-review
calls — 2 x 144 with the twelve committed cases (12 samples per case) —
against
`DefaultCatchRateGeneratorModel` (`claude-sonnet-5-5`, decided 2026-10-10,
#4235). Per `pricing/pricing.go` it bills $2 input / $10 output per Mtok, against
$3 / $15 for `claude-sonnet-4-6`; the maintainer's estimate (not re-derived) is
roughly $10-20 per recording. Sampling cannot be pinned (`anthropic.Config`
has no temperature knob); the power floor is the answer to run-to-run
variance.

## Step 3 — commit the evidence AND its sidecar, and confirm the gate

**Binding: the gate's model and the evidence's model must agree.** The first
recording (2026-10-10) was made on `claude-sonnet-5-5`, and the same commit
switched `catchrategate/main.go`'s `model :=` line (and `main_test.go`'s
matching references) from `agenteval.DefaultQualityGeneratorModel`
(`claude-sonnet-4-6`) to `agenteval.DefaultCatchRateGeneratorModel`, because
`CheckCatchRateEvidence` mode (4) refuses any record from another model. If
`catchrategate` ever FAILS with `recorded generator model "<x>" is not the
gate's model "<y>": re-measure` right after a deliberate generator-model
change, do NOT re-measure on that message: the gate's model line is what is
stale, not the measurement. Switch it in the same PR as the new evidence.

Every recording writes TWO files: `evidence.json` and its trial sidecar
`evidence.trials.json` beside it. Committing both together is the ONLY
recording path. Committing `evidence.json` alone fails `catchrategate` mode
(13), absent sidecar; a sidecar from another recording fails mode (15); a
hand-edited one fails mode (16).

```sh
(cd backend && go run ./internal/agenteval/catchrategate)
cd backend/internal/agenteval/testdata/planreview-catchrate
git add evidence.json evidence.trials.json
```

`catchrategate` exits 0 with the rendered report when the record passes, and
prints the sidecar path just before `catchrategate: PASS`.
Every outcome, pass or fail (absent, stale or malformed evidence and an
unavailable corpus included), ends with the rule line naming the 0.10
tolerance, the pinned-baseline rule and the 136-trials-per-arm floor. It
recomputes the verdict from the counts and refuses an absent, malformed,
stale, under-powered or regressed record, or an absent, malformed, foreign or
count-inconsistent sidecar (the sixteen modes:
`backend/internal/agenteval/README.md` §
"`CheckCatchRateEvidence` fail-closed modes"). A record goes STALE when the
corpus, the conventions fixture, the rendered plan-review prompt, the
generator model or the catch rule changes; `--print-fingerprint` prints the
current fingerprint to compare with the record's `prompt_fingerprint`.
Re-measure on staleness — never hand-edit the record or the sidecar.

## Step 4 — read the missed trials

The counts say how often each planted defect was caught; the sidecar says
what the reviewer wrote when it was not (schema, redaction and caps:
`backend/internal/agenteval/README.md` § "The trial sidecar"). From
`backend/internal/agenteval/testdata/planreview-catchrate/`:

```sh
# Per arm and case: caught/trials (the distribution check below reads this).
jq -r '.arms | to_entries[] | .key as $arm | .value.per_case | to_entries[] | "\($arm) \(.key): \(.value.caught)/\(.value.trials)"' evidence.json

# Per arm: how many catches named the planted id (the rest were attributed by a phrase).
jq -r '.arms | to_entries[] | .key as $arm | [.value.per_case[][] | select(.outcome == "caught")] | "\($arm): \(map(select(.attribution == "id")) | length)/\(length) catches id-attributed"' evidence.trials.json

# Per arm: near-miss tags on the missed trials' concerns.
jq -r '.arms | to_entries[] | .key as $arm | [.value.per_case[][] | select(.outcome == "missed") | .concerns[] | .near_miss // empty] | group_by(.) | map("\(.[0]) \(length)") | "\($arm): \(join(", "))"' evidence.trials.json
```

- Many `probe_without_attribution` concerns mean the reviewer described the
  defect without naming the criterion by id or a declared phrase: widen the
  case's `attribution_phrases` (within the uniqueness rule) or, if it
  persists, consider an LLM-judge classifier.
- Many `attribution_without_probe` concerns mean the reviewer named the
  planted criterion in words no probe covers: read the notes and widen the
  probes if they flag the planted defect.
- Either change edits a `review_input.json`, so the record goes stale and
  must be re-measured. The case set is unchanged, so the baseline stays
  comparable; re-pinning is an explicit choice there, not forced.

**Distribution check (done-means of #4253).** A case at an extreme in BOTH
arms — caught in every trial, or in at most one (with 12 samples per case,
`12/12` or `<= 1/12`, the per-case form of the issue's `>= 22/23` and
`<= 2/23` thresholds) — no longer discriminates. Explain each such case in
the recording PR, or file a follow-up for a harder variant.

## Results

| Recorded at | Generator model | Without conventions | With conventions | Delta | Pinned baseline | Verdict |
|---|---|---|---|---|---|---|
| 2026-10-10T18:33:25Z | `claude-sonnet-5-5` | 80/138 (0.580) | 83/138 (0.601) | -0.022 | pinned (first measurement, #4235) | PASS |
