# Plan-review catch-rate evidence (E55.4 / #2245)

Operator run-book for the LIVE arm-keyed measurement that decides whether the
E55.3 / #2244 repository review-conventions section dilutes the plan
reviewer's catch rate (the two conventions arms) and, since E78.11 / #4313,
whether the architect reviewer persona catches plans that conflict with a
recorded decision and stays quiet on conforming ones (the `architect` arm). Sibling of
[`severity-calibration-evidence.md`](severity-calibration-evidence.md), and it
ships with the same honest residual.

## Baseline status: re-pinned 2026-10-12; re-pin required by #4355

The first baseline was recorded and pinned on 2026-10-10 on
`claude-sonnet-5-5` (row in the results table at the bottom;
`backend/internal/agenteval/testdata/planreview-catchrate/evidence.json`),
over six cases under the probe-only `planreview-catch-v2` rule. E55.18 /
#4253 changed the catch rule (`planreview-catch-v3`: one concern must name
the planted criterion AND match a probe), grew the corpus to twelve cases and
added the trial sidecar; the 2026-10-12 recording re-pinned for it and the
#4254 prompt fold-in. E55.22 / #4355 bumps the catch rule to
`planreview-catch-v4` (prompt-vocabulary probes, loader mode r) and replaces
the plan-review prompt's corpus-echoing examples, so that record is stale and
its baseline is not comparable: the next recording must RE-PIN (Step 2) and
commit the record with its sidecar (Step 3), on the #4355 branch before it
merges. The current pinned baseline is whatever `evidence.json` carries; the
results table lists every recording.

**E78.11 / #4313 makes the record arm-keyed** (`planreview-catchrate-evidence-v2`
plus `planreview-catchrate-trials-v2`, catch rule `planreview-catch-v5`). The
committed pair stays the legacy v1 pair until the #4313 re-pin recording.
Recording sequence (Captain ruling 2026-10-12): #4313 is recorded and merged
FIRST, on its own re-pin; #4314 (which registers its security arms in the
same arm table) records separately after it (Step 2, "The #4313 re-pin
recording, then #4314's"). Until the #4313 recording lands `catchrategate`
refuses the v1 record (its refusal text, written before the ruling, still
names a shared #4313/#4314 recording; the #4313 re-pin is what clears it),
and `scripts/check-review-prompt-eval` fails on every trigger-path change: by
design, and outside `scripts/test verify`.
#2245 shipped the measurement APPARATUS and the offline gate; the measurement
is the operator's recording:

- The offline tests run in every `scripts/test verify` and make **no model
  call**. They prove the arms differ only by the conventions section, that
  the catch rule discriminates, and that every evidence- and sidecar-check
  mode fails closed.
- They prove **nothing** about whether conventions dilute the reviewer; only
  the recorded evidence speaks to that, and only for the committed
  planted-defect shapes.
- The gate command (`catchrategate`) passes on a current committed record
  (not the legacy v1 pair: see the #4313 paragraph above) until the record
  goes stale (a prompt, corpus, fixture, model or catch-rule change),
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
| pinned baseline, catch | any arm's catch rate is MORE than 0.10 below the SAME arm of the PINNED baseline |
| pinned baseline, false alarm | any arm's false-alarm rate (`(false_alarms + undecodable) / clean trials`) is MORE than 0.10 above the SAME arm of the PINNED baseline |
| power floor | any arm has fewer than 136 trials in an expectation class it has (catch, clean) — REFUSED, never passed |
| comparability | the arm set, the generator model, or any arm's case set, expectation classes or samples per case differs from the baseline — refused unless the recording re-pins |

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

The architect arm's corpus (`testdata/planreview-persona-corpus/architect/`,
six SYNTHETIC cases: three catch, three clean) loads against the REAL
`docs/review/architect-remit.md` and `docs/adr/index.json` of the checkout;
its schema and fail-closed loader modes are in the agenteval README §
"The arms":

```sh
scripts/test single -run 'Persona' ./backend/internal/agenteval/
```

## Step 1b — calibrate a new arm's cases (dry run, never written)

Before the arm's re-pin recording, check that each catch case of a NEW arm is of
mid difficulty: caught in 30%–80% of trials (both bounds inclusive). A case
at 0% or 100% cannot show a regression in one direction. The dry run never
writes evidence; clean cases report their false-alarm rate with band `n/a`.

```sh
FISHHAWK_AGENTEVAL_CALIBRATE_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
  FISHHAWK_AGENTEVAL_CALIBRATE_ARM=architect FISHHAWK_AGENTEVAL_CALIBRATE_SAMPLES=10 \
  scripts/test single -run 'TestCatchRateCalibrationLive$' ./backend/internal/agenteval/
```

Run it on the PR branch (the implement agent cannot call the model) and post
the logged markdown table as a PR comment (a fix-up pass cannot edit the PR
body). The test FAILS when any catch case is out of band: rework that case
through a routed fix-up before merge rather than cutting it (the corpus
checks require every kind). Cost: 6 cases x 10 samples = 60 persona-prompt
calls (each prompt ~60 KB: index, remit and plan-review prompt). At 10 samples
a case's rate has a wide sampling interval; a case at a bound is a candidate
for rework, not a verdict.

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

**Evaluate a probe change for free first.** Before any recording, run the
offline re-score: it re-classifies the committed sidecar under the CURRENT
corpus and catch rule with no model call, so a probe change shows its effect
on the last recording before you pay for the next one (a per-arm lower
bound, in-sample, never a PASS, never written; long-form:
`backend/internal/agenteval/README.md` § "The offline re-score"):

```sh
(cd backend && go run ./internal/agenteval/catchrategate --rescore)
```

**The #4355 recording is a re-pin too.** Catch rule v4 changes the
fingerprint and the prompt examples changed, so the 2026-10-12 baseline is
not comparable:

```sh
FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
  FISHHAWK_AGENTEVAL_PLANREVIEW_RECORD=1 \
  FISHHAWK_AGENTEVAL_PLANREVIEW_PIN_REASON='#4355: catch rule v4 (prompt-vocabulary probes) and corpus-disjoint prompt examples; the v3 baseline is not comparable' \
  scripts/test single -run TestPlanReviewCatchRateLive ./backend/internal/agenteval/
```

That recording is the ONLY out-of-sample measurement of v4: the re-score
below tuned the v4 probes on the same sidecar it reports.

**The #4313 re-pin recording, then #4314's.** Captain ruling 2026-10-12:
#4313 is recorded and merged FIRST, on its own re-pin. That recording, made
on the #4313 branch before it merges, measures every arm the arm table
registers at that point (the two conventions arms and the architect arm) and
pins the first v2 baseline. The existing v1 record is a non-comparable
baseline: a non-pinning recording over it is refused (both files stay
byte-identical), a pin replaces it, and a dry run proceeds.

```sh
FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
  FISHHAWK_AGENTEVAL_PLANREVIEW_RECORD=1 \
  FISHHAWK_AGENTEVAL_PLANREVIEW_PIN_REASON='#4313: arm-keyed v2 record (catch rule v5, architect persona arm); the v1 baseline is not comparable' \
  scripts/test single -run TestPlanReviewCatchRateLive ./backend/internal/agenteval/
```

#4314 records separately, after #4313 has merged. Its security arms change
the arm table, so an ordinary recording over the #4313 baseline is refused as
not comparable: #4314's recording is a re-pin of its own, with its own
`PIN_REASON`.

Commit the v2 pair (Step 3); `catchrategate` then decodes it, and the stale
message on any later change names the arm whose fingerprint moved. An ADR
landing stales the architect arm (its prompt embeds `docs/adr/index.json`)
even though `docs/adr/` is not a trigger path.

Cost: each conventions arm is `ceil(136 / cases) x cases` plan-review
calls — 2 x 144 with the twelve committed cases (12 samples per case) — and
the architect arm 6 x 46 = 276 persona calls (46 samples per case, so each
of its catch and clean classes clears the 136 floor with 138), about 564
calls for the #4313 recording (#4314's security arms bill to its own
recording), against
`DefaultCatchRateGeneratorModel` (`claude-sonnet-5-5`, decided 2026-10-10,
#4235). Per `pricing/pricing.go` it bills $2 input / $10 output per Mtok, against
$3 / $15 for `claude-sonnet-4-6`; the maintainer's estimate (not re-derived) is
roughly $10-20 per recording for the two conventions arms; the architect
arm's ~60 KB prompts add materially to that (not estimated here). Sampling cannot be pinned (`anthropic.Config`
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
git commit -s -m "Record plan-review catch-rate evidence and trial sidecar"
```

`catchrategate` exits 0 with the rendered report when the record passes, and
prints the sidecar path just before `catchrategate: PASS`.
Every outcome, pass or fail (absent, stale or malformed evidence and an
unavailable corpus included), ends with the rule line naming the 0.10
tolerance, the pinned-baseline rule and the 136-trials-per-arm floor. It
recomputes the verdict from the counts and refuses an absent, malformed,
stale, under-powered or regressed record, a legacy v1 record, or an absent, malformed, foreign or
count-inconsistent sidecar (the sixteen v1 modes plus the arm-keyed ones:
`backend/internal/agenteval/README.md` §
"`CheckCatchRateEvidence` fail-closed modes"). A record goes STALE when the
corpus, the conventions fixture, the persona corpus, the architect remit or
decision record (`docs/adr/`), the rendered plan-review or persona prompt, the
generator model or the catch rule changes — the message names the stale arm; `--print-fingerprint` prints the
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
  probes if they flag the planted defect, then check the widened set with
  `catchrategate --rescore` before re-recording.
- Either change edits a `review_input.json`, so the record goes stale and
  must be re-measured. The case set is unchanged, so the baseline stays
  comparable; re-pinning is an explicit choice there, not forced.

**Distribution check (done-means of #4253).** A case at an extreme in BOTH
arms — caught in every trial, or in at most one (with 12 samples per case,
`12/12` or `<= 1/12`, the per-case form of the issue's `>= 22/23` and
`<= 2/23` thresholds) — no longer discriminates. Explain each such case in
the recording PR, or file a follow-up for a harder variant.

## Offline re-score: catch rule v3 → v4 (2026-10-12 sidecar, #4355)

Re-score of the committed sidecar
(`backend/internal/agenteval/testdata/planreview-catchrate/evidence.trials.json`,
`recorded_at` 2026-10-12T12:44:59Z, `prompt_fingerprint`
`sha256:344b9a192a0740939ded46f8b61ba4694e41656cf1b90c6a6fb13a099fca2995`,
`claude-sonnet-5-5`) under `planreview-catch-v4`, produced by
`catchrategate --rescore`. No model call; nothing recorded or pinned.

- **Each arm's figure is a LOWER BOUND** (stored notes are cut at 1000 runes,
  at most 12 concerns are stored per trial, and a caught trial stores only its
  matching concern; no stored concern was cut, so 0 lower-bound trials). **The
  arm delta is NOT a bound.**
- **The v4 rates are IN-SAMPLE**: the v4 probes were tuned on this sidecar.
  The operator's v4 recording (Step 2) is the only out-of-sample measurement.
- **The corpus is at ceiling (#4361).** v4 equals the attribution-only rate
  in both arms (within 0.03): once a concern names the planted criterion, the
  probe half rejects almost nothing. The gate still compares arms, but the
  probes no longer separate a catch from an attributed miss on this corpus.

| Arm | v3 recorded | v4 re-scored | Attribution-only | Gained / lost | Undecodable |
|---|---|---|---|---|---|
| without conventions | 87/144 (0.604) | 143/144 (0.993) | 143/144 (0.993) | +56 / -0 | 1 |
| with conventions | 82/144 (0.569) | 144/144 (1.000) | 144/144 (1.000) | +62 / -0 | 0 |
| delta (without - with) | +0.035 | -0.007 (not a bound) | | | |

| Case | Without: v3 → v4 (attribution-only) | With: v3 → v4 (attribution-only) |
|---|---|---|
| contradicts-issue | 4 → 12 (12) | 6 → 12 (12) |
| contradicts-numeric-limit | 10 → 12 (12) | 9 → 12 (12) |
| contradicts-status-code | 9 → 12 (12) | 6 → 12 (12) |
| inferred-criterion | 0 → 12 (12) | 1 → 12 (12) |
| inferred-sibling-retention | 4 → 12 (12) | 1 → 12 (12) |
| inferred-sibling-timeout | 0 → 11 (11; 1 undecodable) | 0 → 12 (12) |
| restates-approach | 11 → 12 (12) | 12 → 12 (12) |
| untestable-adjective | 12 → 12 (12) | 12 → 12 (12) |
| untestable-reasonable-time | 11 → 12 (12) | 11 → 12 (12) |
| untestable-user-friendly | 12 → 12 (12) | 11 → 12 (12) |
| unwarranted-rate-limit | 2 → 12 (12) | 1 → 12 (12) |
| vacuous-criterion | 12 → 12 (12) | 12 → 12 (12) |

**Every remaining attributed miss, judged.** Under v4 no decodable trial
carrying an attributed concern stays missed in either arm; the one uncaught
trial (without conventions, inferred-sibling-timeout) is undecodable. The
last attributed misses tuned in were each read and judged a REAL catch, so
each got a probe: contradicts-status-code without trial 6 ("would contradict
the stated requirement") and with trial 3 ("Nothing in the issue permits
400"); restates-approach without trial 1 ("is a restatement of approach step
2"); untestable-reasonable-time without trial 7 and with trial 12, and
untestable-user-friendly with trial 2 ("no executor can decide"). The
attributed concerns that are about the WRONG defect (a missing test, a
scope or implementability gap, an unnamed limit) were left unmatched, and one
per affected case is now a verbatim `non_catching_example`
(contradicts-issue, contradicts-numeric-limit, contradicts-status-code, the
three inferred-* cases, untestable-reasonable-time, unwarranted-rate-limit).
No difference between the arms comes from the matcher: both arms reach their
attribution-only ceiling, and the 1-trial gap is the undecodable response.
No bare class label (`issue fidelity`, `warrant check`) was needed: it was
never the sole matcher of a real catch. The with arm is re-primed with
"issue fidelity" by `ReviewConventionsFraming`, which is one more reason not
to use it as a probe.

## Results

| Recorded at | Generator model | Without conventions | With conventions | Delta | Pinned baseline | Verdict |
|---|---|---|---|---|---|---|
| 2026-10-10T18:33:25Z | `claude-sonnet-5-5` | 80/138 (0.580) | 83/138 (0.601) | -0.022 | pinned (first measurement, #4235) | PASS |
| 2026-10-12T12:44:59Z | `claude-sonnet-5-5` | 87/144 (0.604) | 82/144 (0.569) | +0.035 | re-pinned (#4253 + #4254 fold-in; the 2026-10-10 baseline is not comparable) | PASS |

### Arm-keyed results (E78.11 / #4313 on)

Pending the #4313 re-pin recording (#4314 records separately after it); one row per recording,
naming every arm. The architect columns are catch (catch cases) and false
alarm (clean cases).

| Recorded at | Generator model | Without conventions | With conventions | Architect catch | Architect false alarm | Other arms | Pinned baseline | Verdict |
|---|---|---|---|---|---|---|---|---|
| — | — | — | — | — | — | — | pending the #4313 re-pin | not yet recorded |

### Calibration dry runs

| Run at | Arm | Samples per case | Out-of-band catch cases | Table |
|---|---|---|---|---|
| 2026-10-13 | `architect` | 10 | adr033 + adr094 above band (10/10); reworked, re-calibration pending operator | routed fix-up concern (head `84ce5e13`) |
| 2026-10-14 | `architect` | 10 | adr033 + adr094 10/10 after rewording; replaced by adr006-production-migrate-down and adr091-merge-without-review-coverage (body-only conflicts), re-calibration pending operator | routed fix-up concern (head `f7549511`) |
| 2026-10-14 | `architect` | 30 | adr006 76.7% and undeclared 43.3% in band; adr091 13.3% below (successor body never shown: superseding ADRs have empty applies_to); adr091 reworked to a content reference, re-calibration pending operator | routed fix-up concern (head `ea8bd7f1`) |
