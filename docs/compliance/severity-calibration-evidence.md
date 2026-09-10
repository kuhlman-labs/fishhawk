# Severity-calibration and adversarial-retention evidence (E50.22 / #3309)

Operator run-book for the two LIVE arms that decide whether #2119's severity
rubric did what it was added to do. Sibling of
[`prompt-injection-evidence.md`](prompt-injection-evidence.md), and it ships
with the same honest residual.

## THE MEASUREMENT HAS NOT BEEN TAKEN

The results table at the bottom of this document is **explicitly
unpopulated**. #3309 shipped the measurement APPARATUS — two corpora, a
pre-/post-#2119 two-arm A/B, and an omission-monotone comparison — and the
offline gates that prove the apparatus is correct. It did not ship the
measurement.

Concretely, and stated this plainly on purpose:

- The offline gates run in every `scripts/test verify` and make **no model
  call**. They prove ARM CORRECTNESS: the five #2119 treatment literals are
  byte-exact against the real shipped prompt, the strip is posture-aware in
  both directions, the comparison cannot be improved by omitting a labelled
  concern, and the retention verdict's third state is not a pass.
- They prove **nothing** about whether #2119 moved severities toward the
  operator's label, or whether it cost an adversarial finding.
- Therefore **#3309 done-means 2 and 3 remain UNANSWERED** until the arms
  below are run against a real model.

This is not a shortfall being papered over. The runner sanitizes
gate-subprocess environments through a default-deny allow-list that names
`ANTHROPIC_API_KEY` on `gateEnvDeny`
(`runner/cmd/fishhawk-runner/gateenv.go`), so **no in-loop test can obtain a
model credential by design**, and no such credential was present in the
environment that produced the change. The arms are operator-executed, and
the `live-comparative-rerun-measured` acceptance criterion carries
`requires_live_validation` precisely so this unanswered question stays
visible after the change merges.

## What each arm decides

| Arm | Test | Decides |
|---|---|---|
| severity calibration | `TestSeverityCalibrationLive` | done-means 2 — is the post-#2119 reviewer severity closer to the operator's label than the pre-#2119 one? |
| adversarial retention | `TestAdversarialRetentionLive` | done-means 2(b) — did #2119 LOSE a finding the pre-#2119 prompt produced? This feeds the done-means 3 revert-or-fix decision. |

Both are double-gated: they SKIP unless BOTH
`FISHHAWK_AGENTEVAL_CALIBRATION_LIVE` and `FISHHAWKD_ANTHROPIC_API_KEY` are
set, and the skip message names #3309 and this document.

## Step 1 — build the labelled corpus

The committed seed fixtures under
`backend/internal/agenteval/testdata/severity-calibration-corpus/` are
hand-authored and carry `synthetic: true`. They exercise the harness; they
are not epic #1824's operator dispositions, which live in the operator audit
database rather than the tree. Real cases arrive through the distiller:

```sh
# From the repo root, against a running backend.
export FISHHAWK_BACKEND_URL=http://localhost:8080
export FISHHAWK_TOKEN=fhk_...

go run ./backend/cmd/fishhawk-distill-corpus \
  --severity-calibration \
  --run-id <run-uuid> \
  --diff /tmp/implement.patch \
  --plan-summary /tmp/plan-summary.md \
  --case-name <slug> \
  --issue '#3309' \
  --dry-run
```

Drop `--dry-run` to write the candidate. Then, before committing:

1. **READ the free text.** The generated `case.md` opens with a
   `TODO(operator): REVIEW FREE TEXT BEFORE COMMITTING` block. Concern
   `note`s are reviewer prose and `disposition_reason`s are operator prose;
   both can carry tokens, hostnames, internal paths or customer detail, and
   nothing in this repository redacts them for you. This surface makes no
   redacted-by-construction claim.
2. **LABEL every concern.** `operator_severity` is written EMPTY. Set each to
   the severity you judged the concern to be worth when you dispositioned it
   (`high` / `medium` / `low`). The loader REFUSES an unlabelled case, so an
   unedited candidate cannot silently join the corpus.
3. **Add any unjoinable dispositions by hand.** A
   `concern_addressed_by_condition` audit payload carries neither `severity`
   nor `category`, so it has no key to join on; `case.md` lists such entries
   by `concern_id` rather than guessing them.

Contract and fail-loud modes: `backend/internal/corpusdistill/README.md`.

## Step 2 — run the arms

```sh
FISHHAWK_AGENTEVAL_CALIBRATION_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
  scripts/test single -run 'TestSeverityCalibrationLive|TestAdversarialRetentionLive' \
  ./backend/internal/agenteval/
```

Both arms use ONE generator config, because a model difference between arms
would confound the treatment effect. The arms differ ONLY in the five #2119
literals `StripCalibrationCriteria` removes.

## Step 3 — read the output honestly

**Severity calibration.** A POSITIVE overall delta means the post-#2119 arm
sits CLOSER to the operator's label. Read it beside the coverage columns:
the score penalizes a MISSED labelled concern at the maximum tier distance
(`MaxSeverityTierDistance`), which is what makes the comparison
omission-monotone — an arm can never improve its score by omitting a
labelled concern. The cost of that choice is that a delta computed mostly
from penalties measures **coverage, not calibration**, so check
`pre_missed` / `post_missed` per case before believing a number. The penalty
value and the improvement threshold are both JUDGEMENT CALLS, not measured
values; they are parameters, not hardcoded gates.

**Retention.** `indeterminate is NOT a pass` — the report says so in its
header and counts the three states separately. A case REGRESSED means the
finding was produced in the PRE arm and absent in the POST arm. That is
done-means 3's loss condition and the outcome that would argue for
reverting or narrowing #2119's wording.

The severity arm REPORTS rather than failing the build on a null result:
turning an unvalidated threshold into a red build would dress a judgement
call as a gate. The retention arm DOES fail on a regression, because a lost
finding is a decided loss, not a tuning parameter.

## Step 4 — record the result

Paste the rendered comparisons into #3309, fill the table below, and take
the revert-or-fix decision there.

## Results

**UNPOPULATED — the arms have not been run.** Do not read an empty row as a
null result; read it as an unasked question.

| Date | Model | Corpus size (labelled concerns) | Samples | Pre score | Post score | Overall delta | Scored / skipped cases | Retention: produced / absent / indeterminate (pre → post) | Regressed cases | Decision |
|---|---|---|---|---|---|---|---|---|---|---|
| _(not yet run)_ | | | | | | | | | | |

## Related

- `backend/internal/agenteval/README.md` — the corpora, the five-literal
  treatment set, and the omission-monotone scoring rule.
- `backend/internal/prompt/README.md` — the #2119 section, and the lockstep
  coupling an edit to any of the five literals costs.
- `backend/internal/corpusdistill/README.md` — the distiller, the join and
  its fail-loud modes.
- `docs/compliance/prompt-injection-evidence.md` — the #2291 sibling, which
  ships the same honest-residual posture.
