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
refuses the v1 record (its refusal text names the ruling's sequence, #4313
recorded first and #4314 re-pinned separately after it; the #4313 re-pin is
what clears it),
and `scripts/check-review-prompt-eval` fails on every trigger-path change: by
design, and outside `scripts/test verify`.

**E55.23 / #4361 grows the conventions arms' corpus from twelve cases to
fifteen**, so their `samples_per_case` drops from 12 to 10
(`SamplesPerCaseForPower(15)`, 150 trials per arm, up from 144) and the 2026-10-15
record is not comparable for those arms: after the live calibration (Step 1c)
an operator re-pins (Step 2, "The #4361 recording"). #4361 itself leaves
`evidence.json` and `evidence.trials.json` untouched.

**E55.20 / #4326 records the INSTRUMENT in the evidence**: a
`catch_rule_version` (the full catch-rule text, on the record and its
baseline) and a per-arm `input_digest` (each case's name, expectation class,
exact input bytes and catch rule — not the rendered prompt, fixture, remit or
documents). A baseline lacking or differing in either is not comparable. The
committed 2026-10-15 record predates both terms, so `catchrategate` refuses
it — by mode (4c) while the record is otherwise fresh, otherwise by the
earlier STALE mode — and in either case the next recording must RE-PIN
(Step 2), because the committed baseline is not comparable. By the campaign's
batch rule that re-pin is the ONE combined operator re-recording made after
every batched catch-rate trigger change (#3930, #4402, #4453, #4326, #4400)
has merged; until then the refusal is by design. #4326 leaves `evidence.json`
and `evidence.trials.json` untouched.
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
  (not the legacy v1 pair, nor a record made before #4326: see the
  paragraphs above) until the record
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
| comparability | the arm set, the generator model, the catch rule (`catch_rule_version`), or any arm's case set, expectation classes, samples per case or case inputs (`input_digest`: a case's input bytes or catch rule, same case names) differs from the baseline — refused unless the recording re-pins. The treatment (rendered prompt, conventions fixture, remit, documents) is not an instrument term: changing it is what a recording measures |

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
holds fifteen cases as of #4361 (`seed-synthetic-*`, two or more per weak
class: #4253's twelve plus #4361's three subtler mid-difficulty cases that
calibrated in band; Step 1c's results record the three cut at the ceiling).

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
for rework, not a verdict. The default is now 30 samples per case (10 proved
too noisy, #4314).

**The security arms (E80.17 / #4314).** Calibrate every security catch case
at 30 samples to the 30–80% band BEFORE #4314's recording; any out-of-band
case is reworked through a routed fix-up first (the merge is held for both).
Captain ruling 2026-10-14: a catch case at ceiling (30/30) is accepted, since a
ceiling case still detects a recall drop; a clean case's false alarms are
reworked (see "Calibration dry runs"):

```sh
FISHHAWK_AGENTEVAL_CALIBRATE_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
  FISHHAWK_AGENTEVAL_CALIBRATE_ARM=security FISHHAWK_AGENTEVAL_CALIBRATE_SAMPLES=30 \
  scripts/test single -run 'TestCatchRateCalibrationLive$' ./backend/internal/agenteval/
FISHHAWK_AGENTEVAL_CALIBRATE_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
  FISHHAWK_AGENTEVAL_CALIBRATE_ARM=security_portability FISHHAWK_AGENTEVAL_CALIBRATE_SAMPLES=30 \
  scripts/test single -run 'TestCatchRateCalibrationLive$' ./backend/internal/agenteval/
```

Cost: 10 cases x 30 = 300 implement-review persona calls for `security`, 4 x
30 = 120 for `security_portability`. Read three things in the table beyond the
band:

- **The false-alarm rate counts EVERY concern on a clean diff.** A security
  clean case has no category predicate (the remit reserves no concern
  category), so the standard-criteria concerns the persona also raises
  (correctness, verification, scope) count as false alarms beside the
  security ones. Read the rate beside the "False alarms by category" block,
  never alone. The recording commits the same split per clean case as
  `false_alarm_partition` (`security_lens` / `other_category` trials) in the
  evidence and a per-trial `alarm_partition` tag in the sidecar, so a later
  decision to reserve a security category in the remit is made from
  committed data.
- **Absent-control concerns.** The sidecar marks each concern asserting a
  control is absent (`absent_control`, from `markers.json`); the recording
  report counts them per case. A generic one (no repository marker) is
  surfaced, not refused.
- **Portability findings.** A `security_portability` concern naming a
  repository-specific marker (`markers.json` `repo_specific_markers`: this
  repository's own controls, every one documented in
  `docs/review/security-conventions.md` and absent from the remit and from
  every clean case) is about a control the remit-only reviewer was never
  shown. The dry run FAILS on one and the recording REFUSES it
  (`ErrPortabilityRepoSpecificFinding`; both files untouched, dry run
  included). Read the concern in the report: rework the remit (if it leaked a
  repository assumption) or the case; never delete the marker to pass.

**#4402: the harder security catch cases.** #4402 replaced the six #4314
catch cases (all at the ceiling, 34/34) with six harder cases under new
names: `authz-account-check-behind-legacy-flag`,
`failclosed-gate-lookup-timeout-proceeds`,
`permission-instruction-path-case-fold-dropped`,
`secret-signed-url-in-debug-retry-log`,
`supplychain-installer-from-branch-version-pinned` and
`trust-linked-issue-summary-one-call-out`. The four clean cases and
`markers.json` are unchanged. Three steps:

1. **Calibrate at the PR head, before merge.** Run ONE subset dry run over
   the ten security case ids at 30 trials. Both security arms run (the
   portability arm runs the four clean ids), and the per-trial miss classes
   land in `planreview-diag.jsonl` (cost: 10 x 30 + 4 x 30 = 420
   implement-review persona calls):

   ```sh
   FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
     FISHHAWK_AGENTEVAL_PLANREVIEW_CASES=authz-account-check-behind-legacy-flag,failclosed-gate-lookup-timeout-proceeds,permission-instruction-path-case-fold-dropped,secret-signed-url-in-debug-retry-log,supplychain-installer-from-branch-version-pinned,trust-linked-issue-summary-one-call-out,clean-auth-helper-refactor,clean-ci-pinned-tool-bump,clean-policy-test-only,clean-redaction-helper-move \
     FISHHAWK_AGENTEVAL_PLANREVIEW_TRIALS=30 \
     scripts/test single -v -timeout 2h -run TestPlanReviewCatchRateLive ./backend/internal/agenteval/
   ```

   The band-table alternative is `TestCatchRateCalibrationLive` with
   `FISHHAWK_AGENTEVAL_CALIBRATE_ARM=security` and then
   `security_portability`, `FISHHAWK_AGENTEVAL_CALIBRATE_SAMPLES=30` (the
   commands above). It reports scored counts only, so read the miss classes
   from the subset dry run.
2. **Accept a rate on reviewer misses only (the Step 1c rule).** A case is in
   band (30%–80%) only when its misses are `not_mentioned`. Repair a
   `no_probe` or `unattributed` miss by widening the case's probes or
   attribution, never by loosening a probe the loader refuses. Rework an
   out-of-band case through a routed fix-up. A case still out of band after
   rework is recorded with its documented reason, which #4402's done-means
   allows.
3. **Re-pin in the next combined recording, not on the #4402 branch.** The
   campaign batches every review-prompt-eval trigger change (#3930, #4402,
   #4453, #4326, #4400) into ONE combined operator re-recording after all of
   them merge, so the #4402 merge is not held for a re-pin. The changed case
   set makes the `security` arm non-comparable for exactly that one pin
   (`baselineComparable` compares case-name sets): that recording does not
   check the arm for regression, and later pins compare normally. Until it
   lands, `catchrategate` and `scripts/check-review-prompt-eval` fail on
   `main`, naming the stale `security` arm, by design. The combined
   recording uses this `PIN_REASON` text for the arm, folded into its own:
   `#4402: six harder security catch cases replace the #4314 ceiling set; the security arm case set changed, so the baseline is not comparable`.

**Calibration at 58f8062e (security arm, 30 trials/case).** The step-1 subset
dry run at the PR head:

| Case | Caught | Misses |
|---|---|---|
| `authz-account-check-behind-legacy-flag` | 29/30 | 1 undecodable |
| `failclosed-gate-lookup-timeout-proceeds` | 30/30 | none |
| `permission-instruction-path-case-fold-dropped` | 28/30 | 2 unattributed |
| `secret-signed-url-in-debug-retry-log` | 30/30 | none |
| `supplychain-installer-from-branch-version-pinned` | 30/30 | none |
| `trust-linked-issue-summary-one-call-out` | 24/30 | 4 no_probe, 2 unattributed |

- **Clean cases.** 0/120 false alarms on `security`. 4/120 on
  `security_portability`, all on `clean-policy-test-only`, all in the
  `verification` category.
- **Generic-probe check.** No generic probe matched any trial: every catch
  matched a defect-specific phrase.
- **Miss classes.** 171/180 caught. All 8 decodable misses (the 4 `no_probe`
  and 4 `unattributed` above) were real catches mis-attributed (category or
  phrase), so the true rate is about 179/180. The
  `trust-linked-issue-summary-one-call-out` misattributed catches used the
  phrase `trusted prompt header`, which that case now carries as a probe.
- **Ruling (Captain, 2026-10-15).** The six cases are accepted as documented
  OUT-OF-BAND AT THE CEILING; #4402's done-means allows a documented reason.
  No rework chases the band. Cross-context security cases (a defect visible
  only against unchanged code, or outside the convention) are the follow-up
  #4474. The `security` arm is re-pinned in the campaign's combined
  re-recording (step 3).

## Step 1c — calibrate new conventions-arm cases (#4361)

Before the #4361 re-pin, check that each of its new catch cases is of mid
difficulty in the conventions arms. The implement agent cannot call the
model, so this runs on the PR branch as an operator subset dry run (never
judged, never recordable; see "Probe a few cases before paying for a
recording" below): both conventions arms, 1 case x 30 trials each, 60
plan-review calls.

```sh
FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
  FISHHAWK_AGENTEVAL_PLANREVIEW_CASES=seed-synthetic-restates-approach-observable \
  FISHHAWK_AGENTEVAL_PLANREVIEW_TRIALS=30 \
  scripts/test single -v -timeout 2h -run TestPlanReviewCatchRateLive ./backend/internal/agenteval/
```

**Band rule.** Each new case is caught in 30%–80% of its trials (both bounds
inclusive) in at least one conventions arm; the target is 40%–70%. At 30
samples a case near a bound is a rework candidate, not a verdict (an
unchanged architect case moved 23/30 → 29/30 between runs, "Calibration dry
runs" below).

Read the run's diagnostics file (`DIAG` = the path it logged; "Diagnosing a
refused or surprising measurement" below):

```sh
# Per arm and case: caught/trials, the rate, and the miss classes.
jq -rs '.[-1] | select(.kind == "summary") | .arms | to_entries[] | .key as $arm | .value.per_case | to_entries[] | "\($arm) \(.key): \(.value.caught)/\(.value.trials) (\(if .value.trials > 0 then (100 * .value.caught / .value.trials | floor) else 0 end)%), undecodable \(.value.undecodable), misses \(.value.miss_classes // {} | to_entries | map("\(.key) \(.value)") | join(", "))"' "$DIAG"

# Per arm and case: the scored rate beside the attribution-only rate (trials
# with ANY raw concern naming the planted criterion, probe or not).
jq -rs '[.[] | select(.kind == "trial" and .expect == "catch")] | group_by([.arm, .case]) | .[] | "\(.[0].arm) \(.[0].case): scored \(map(select(.caught)) | length)/\(length), attribution-only \(map(select(any((.raw_concerns // [])[]; .attributed))) | length)/\(length)"' "$DIAG"

# Per arm, corpus level: the same two rates over every measured case.
jq -rs '[.[] | select(.kind == "trial" and .expect == "catch")] | group_by(.arm) | .[] | "\(.[0].arm): scored \(map(select(.caught)) | length)/\(length), attribution-only \(map(select(any((.raw_concerns // [])[]; .attributed))) | length)/\(length)"' "$DIAG"
```

**A case is in band only on REVIEWER misses.** Read each missed trial's
`miss_class` and its `raw_concerns` before accepting a rate; the remedy
depends on the class:

- `not_mentioned` is genuine difficulty: no concern named the planted
  criterion or matched a probe. A case whose in-band rate comes from these is
  accepted.
- `unattributed` (a probe matched but no concern named the planted id or a
  declared attribution phrase) is a scoring miss, repaired by widening the
  case's ATTRIBUTION wording: add the planted statement's distinguishing
  wording the concern used to `attribution_phrases` (and to `defect_phrases`
  when it is new domain content, so the prompt-leak check covers it). Mode p
  bounds this: a phrase any sibling criterion's statement also carries is
  refused by the loader.
- `no_probe` (a concern named the planted criterion but matched no probe) is a
  scoring miss, repaired by widening `catch_probes` with the concern's
  polarity-bearing, multi-word phrasing: never a bare generic objection word,
  and never wording the issue or plan shows (mode m) or a non-catching example
  carries (mode j).
- `truncated` (the generator response stopped at max_tokens) is neither: the
  trial was never scored. The conventions arms record it as an undecodable
  miss and keep running instead of aborting. A case whose rate is depressed by
  `truncated` trials is re-measured, not accepted or reworked.

Widen only for a concern that really flags the planted defect; one about
another aspect of the criterion stays a miss and becomes a
`non_catching_example`. A case whose in-band rate comes from scoring misses
(`unattributed` / `no_probe`) instead of `not_mentioned` is REWORKED (widened
as above), never accepted as measured. `TestCatchCorpusWidening_RepairsScoringMissClasses`
(`backend/internal/agenteval/planreviewcatch_test.go`) is the offline example
of each remedy: a confirmed catch phrased as each scoring class is MISSED and
refused as a catching example, then loads and scores CAUGHT after its
widening, while a widening past mode p is refused. Check any widened set with
`catchrategate --rescore` before re-measuring.

**Results (2026-10-15, head `23a9dee4`).** Generator `claude-sonnet-5-5`,
`MaxTokens` 8192, 30 trials per arm, both conventions arms, 0 truncations,
0 undecodable. Raw scored catches and miss classes, then the honest rate
(the miss transcripts read by the operator, and the kept case re-scored
under its widened probes):

| case | arm | scored | misses | honest | outcome |
|---|---|---|---|---|---|
| restates-approach-observable | with | 11/30 (37%) | no_probe 15, not_mentioned 4 | 13/30 (43%) | KEPT |
| restates-approach-observable | without | 15/30 (50%) | no_probe 15 | 16/30 (53%) | KEPT |
| unsatisfiable-stored-verbatim | with / without | 14/30 / 16/30 | no_probe 16 / 14 | ~30/30 / ~30/30 | CUT |
| untestable-measurable-phrase | with / without | 20/30 / 26/30 | no_probe 10 / 4 | ~30/30 / ~30/30 | CUT |
| contradicts-buried-clause | with / without | 30/30 / 30/30 | none | ceiling | CUT |
| contradicts-absent-vs-null | with / without | 30/30 / 30/30 | none | ceiling | CUT |
| inferred-wrong-source | with / without | 30/30 / 30/30 | none | ceiling | CUT |

`unsatisfiable-stored-verbatim` and `untestable-measurable-phrase` were in
band only on SCORING misses: every `no_probe` transcript is a real catch
("jsonb normalizes ... the criterion cannot pass"; "cannot be decided
pass/fail, the budget is never given a number") phrased outside the probes.
Their honest rate is the ceiling, so by this step's rule they are not
accepted as measured, and widening would put them at the ceiling. The kept
case's remaining `no_probe` misses name its criterion id while flagging
another defect (scan durability, wiring, end-to-end tests), so they are
reviewer misses. Its three real catches outside the probes (the "marked
source explicit ... a plan choice presented as an explicit requirement"
fidelity route) were added as probes, and the re-score under the widened
rule over the run's diag gives 13/30 and 16/30, matching the hand count.

Cut by Captain ruling 2026-10-15: the three ceiling cases (option c), then
the two scoring-miss cases after this honest-rate check. This is a PARTIAL
delivery of #4361's ">= 6 cases" done-means: #4361 adds ONE in-band case.
The harder cases continue in #4453, which sources them from real
plan-review misses and requires each case's band to rest on `not_mentioned`
reviewer misses.

Post the per-case table (both arms, both rates, the miss classes) as a PR
comment: a fix-up pass cannot edit the PR body. Rework an out-of-band case,
or one in band on scoring misses, through a routed fix-up and re-run this
step for it. Then re-pin through Step 2 ("The #4361 recording") and, after
the recording lands, run `(cd backend && go run ./internal/agenteval/catchrategate --rescore)`
so the Results row reports the scored rate next to the attribution-only rate
(#4355 approval condition C3).

## Step 2 — run the arms

Authenticate with exactly one of `FISHHAWKD_ANTHROPIC_API_KEY` or
`FISHHAWKD_ANTHROPIC_AUTH_TOKEN` (an OAuth bearer; substitute it for the key in
any command below). Neither set skips the arm naming both variables; both set
fails it. Which bearer tokens Anthropic's terms permit for direct API use is the operator's responsibility.

Every command below passes `-v` (so the per-trial progress lines stream
instead of arriving when the package finishes) and an explicit `-timeout`
(`go test` defaults to 10 minutes; a full recording is about 1,000 model
calls). Every run, refused or not, writes a per-trial diagnostics file and
logs its path first (`per-trial diagnostics: <path>`) and last
(`Diagnostics: <path>`) — see "Diagnosing a refused or surprising
measurement" below.

Dry run (measures and judges against the committed baseline; writes no
evidence):

```sh
FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
  scripts/test single -v -timeout 4h -run TestPlanReviewCatchRateLive ./backend/internal/agenteval/
```

Record a measurement (`RECORD=1`). The FIRST recording must also pin the
baseline, with a reason:

```sh
FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
  FISHHAWK_AGENTEVAL_PLANREVIEW_RECORD=1 \
  FISHHAWK_AGENTEVAL_PLANREVIEW_PIN_REASON='first measurement after #2245' \
  scripts/test single -v -timeout 4h -run TestPlanReviewCatchRateLive ./backend/internal/agenteval/
```

Later recordings omit `PIN_REASON`; the pinned baseline is carried forward
unchanged. Moving it is the separate, explicit action of setting
`PIN_REASON` again: it still requires a passing measurement (including
against the current baseline when that baseline is comparable) and records
the reason and a timestamp in the evidence. After a corpus, catch-rule or
case-input change (#4326) the old baseline is not comparable, an ordinary
recording is refused, and you re-pin.

**The #4253 recording is such a re-pin.** The case set changed (six cases to
twelve), so `samples_per_case` changed too, and the 2026-10-10 baseline is not
comparable; record with a reason that says so:

```sh
FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
  FISHHAWK_AGENTEVAL_PLANREVIEW_RECORD=1 \
  FISHHAWK_AGENTEVAL_PLANREVIEW_PIN_REASON='#4253: catch rule v3 and the twelve-case corpus; the 2026-10-10 baseline is not comparable' \
  scripts/test single -v -timeout 4h -run TestPlanReviewCatchRateLive ./backend/internal/agenteval/
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
  scripts/test single -v -timeout 4h -run TestPlanReviewCatchRateLive ./backend/internal/agenteval/
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
  scripts/test single -v -timeout 4h -run TestPlanReviewCatchRateLive ./backend/internal/agenteval/
```

#4314 records separately, after #4313 has merged. Its security arms change
the arm table, so an ordinary recording over the #4313 baseline is refused as
not comparable: #4314's recording is a re-pin of its own, with its own
`PIN_REASON`, made on the #4314 branch after its calibration (Step 1b) and
before it merges (the merge is HELD for it; until it lands `catchrategate`
and `scripts/check-review-prompt-eval` fail on that branch naming the
missing `security` and `security_portability` arms, by design):

```sh
FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
  FISHHAWK_AGENTEVAL_PLANREVIEW_RECORD=1 \
  FISHHAWK_AGENTEVAL_PLANREVIEW_PIN_REASON='#4314: security and security_portability join the arm table; the #4313 baseline is not comparable' \
  scripts/test single -v -timeout 4h -run TestPlanReviewCatchRateLive ./backend/internal/agenteval/
```

**The #4361 recording.** The conventions arms' case set grew from twelve to
fifteen and their `samples_per_case` from 12 to 10, so the 2026-10-15
baseline is not comparable and the recording is a re-pin, made on the #4361
branch after its calibration (Step 1c) and before it merges. It measures
every arm in the table, the persona arms included:

```sh
FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
  FISHHAWK_AGENTEVAL_PLANREVIEW_RECORD=1 \
  FISHHAWK_AGENTEVAL_PLANREVIEW_PIN_REASON='#4361: fifteen-case conventions corpus (10 samples per case); the 2026-10-15 baseline is not comparable' \
  scripts/test single -v -timeout 4h -run TestPlanReviewCatchRateLive ./backend/internal/agenteval/
```

A recording in which any `security_portability` concern names a
repository-specific marker is refused (Step 1b, "Portability findings"). The
security arms' judged false-alarm rate counts every concern on a clean diff,
standard-criteria concerns included: read it beside the report's "Persona
diagnostics" partition and the committed `false_alarm_partition`.

Commit the v2 pair (Step 3); `catchrategate` then decodes it, and the stale
message on any later change names the arm whose fingerprint moved. An ADR
landing stales the architect arm (its prompt embeds `docs/adr/index.json`)
even though `docs/adr/` is not a trigger path.

Cost: each conventions arm is `ceil(136 / cases) x cases` plan-review
calls — 2 x 150 with the fifteen committed cases (10 samples per case; 2 x
144 at #4253's twelve) — and
the architect arm 6 x 46 = 276 persona calls (46 samples per case, so each
of its catch and clean classes clears the 136 floor with 138), about 564
calls for the #4313 recording (twelve cases); #4314's recording adds the security arm, 10 x
34 = 340 implement-review persona calls (34 samples per case, so its six catch
and four clean cases each clear the floor), and the portability arm, 4 x 34 =
136; the #4361 re-pin re-measures all of them, about 2 x 150 + 276 + 476 =
1,052 calls, against
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
  must be re-measured. Since #4326 it also changes that arm's `input_digest`
  (the case's input bytes and catch rule), so the baseline is NOT comparable
  even though the case set is unchanged: the re-measurement must re-pin.

**Distribution check (done-means of #4253).** A case at an extreme in BOTH
arms — caught in every trial, or in at most one (with 10 samples per case
since #4361, `10/10` or `<= 1/10`; at #4253's 12 samples, `12/12` or `<= 1/12`
was the per-case form of the issue's `>= 22/23` and `<= 2/23` thresholds,
and at 10 samples `<= 1/10` is slightly looser than `2/23`) — no longer
discriminates. Explain each such case in
the recording PR, or file a follow-up for a harder variant.

## Diagnosing a refused or surprising measurement

Every live run writes `planreview-diag.jsonl` (schema, redaction and the
miss-class rule: `backend/internal/agenteval/README.md` § "Live-run
diagnostics"), at
`$FISHHAWK_AGENTEVAL_DIAG_DIR/<UTC 20060102T150405Z>/planreview-diag.jsonl`,
or under `$TMPDIR/fishhawk-agenteval/<UTC 20060102T150405Z>/` when the knob
is unset. It is written trial by trial, so a refused, failed or killed run
still has every trial it finished; it is never evidence and never committed
(a path under `testdata/` is refused).

Follow a long run as it goes:

```sh
tail -f "$DIAG"     # DIAG = the path the run logged
```

Read it with `jq` (`-s` slurps the lines):

```sh
# Per arm and case: the miss-class breakdown of the missed catch trials.
jq -rs '.[] | select(.kind == "trial" and .miss_class != "") | "\(.arm) \(.case) \(.miss_class)"' "$DIAG" | sort | uniq -c

# Per arm: the concern-category histogram, verdicts and mean concerns.
jq -s '.[-1] | select(.kind == "summary") | .arms | map_values({concern_categories, verdicts, mean_concerns_per_decoded_trial})' "$DIAG"

# How the run ended (passed, not_judged, refused, error) and why.
jq -s '.[-1] | {outcome, error, trials_written}' "$DIAG"
```

Read the miss classes as two kinds of miss:

- `not_mentioned` is a **reviewer miss**: no concern touched the planted
  criterion or the defect. The reviewer did not see it, so look at the prompt
  (or accept that the case is hard).
- `unattributed`, `no_probe` and `category_mismatch` are **scoring misses**:
  the reviewer raised the defect, but the catch rule did not credit it (no
  planted id or phrase, no probe, or the wrong category label on the
  architect arm). Look at the case's `catch_probes` / `attribution_phrases`
  and the concern's `raw_concerns` entry, then check a widened set with
  `catchrategate --rescore` before re-recording.
- `undecodable` means no verdict decoded; read `response_excerpt`.
- `truncated` means the generator response stopped at max_tokens, so no
  verdict was scored. It is counted in `undecodable`, and the run continues
  instead of aborting.

**Probe a few cases before paying for a recording.** A subset dry run
measures only the named cases (any arm's case directory names) at a reduced
sample count, for a fraction of a recording's cost (a dollar or two at
most). It is never judged and never recordable: with `RECORD=1` or
`PIN_REASON` set it is refused before any model call.

```sh
FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
  FISHHAWK_AGENTEVAL_PLANREVIEW_CASES=seed-synthetic-untestable-adjective,<another-case> \
  FISHHAWK_AGENTEVAL_PLANREVIEW_TRIALS=3 \
  scripts/test single -v -timeout 30m -run TestPlanReviewCatchRateLive ./backend/internal/agenteval/
```

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

One row per recording,
naming every arm. The architect columns are catch (catch cases) and false
alarm (clean cases).

| Recorded at | Generator model | Without conventions | With conventions | Architect catch | Architect false alarm | Other arms | Pinned baseline | Verdict |
|---|---|---|---|---|---|---|---|---|
| 2026-10-14 | `claude-sonnet-5-5` | 133/144 (0.924) | 130/144 (0.903) | 106/138 (0.768) | 32/138 (0.232) | — | pinned (#4313: arm-keyed v2, catch rule v5; v1 not comparable) | PASS (delta +0.021) |
| 2026-10-15 | `claude-sonnet-5-5` | 137/144 (0.951) | 135/144 (0.938) | 105/138 (0.761) | 28/138 (0.203) | `security` catch 204/204 (1.000), false alarm 3/136 (0.022; trials: security_lens 0, other_category 2, undecodable 1); `security_portability` false alarm 3/136 (0.022; trials: security_lens 1, other_category 1, undecodable 1) | pinned (#4314: security + security_portability arms added; the #4313 arm set is not comparable) | PASS (delta +0.014) |
| 2026-10-15 | `claude-sonnet-5-5` | 128/143 (0.895) | 131/143 (0.916) | 118/138 (0.855) | 24/138 (0.174) | `security` catch 204/204 (1.000), false alarm 1/136 (0.007; trials: security_lens 0, other_category 0, undecodable 1); `security_portability` false alarm 7/136 (0.051; trials: security_lens 0, other_category 6, undecodable 1) | pinned (combined re-pin after #4337/#4398/#4399/#4361 and ADR-100/101: conventions corpus +restates-approach-observable, 13 seed-synthetic cases; MaxTokens 8192 with non-fatal truncation; architect index + #4399 record budget; 4 architect trials undecodable; prior pin not comparable) | PASS (delta -0.021) |

The #4314 row's "Other arms" column names `security` catch and false alarm
(with its security_lens / other_category trial split; the remainder of each
false-alarm count is undecodable trials) and `security_portability` false
alarm. All six #4314 security catch cases sat at ceiling (34/34 each), accepted by
Captain ruling 2026-10-14. #4402 replaced them with six harder catch cases;
their calibration and the `security` arm's re-pin follow Step 1b, "#4402: the
harder security catch cases".

### Calibration dry runs

| Run at | Arm | Samples per case | Out-of-band catch cases | Table |
|---|---|---|---|---|
| 2026-10-13 | `architect` | 10 | adr033 + adr094 above band (10/10); reworked, re-calibration pending operator | routed fix-up concern (head `84ce5e13`) |
| 2026-10-14 | `architect` | 10 | adr033 + adr094 10/10 after rewording; replaced by adr006-production-migrate-down and adr091-merge-without-review-coverage (body-only conflicts), re-calibration pending operator | routed fix-up concern (head `f7549511`) |
| 2026-10-14 | `architect` | 30 | adr006 76.7% and undeclared 43.3% in band; adr091 13.3% below (successor body never shown: superseding ADRs have empty applies_to); adr091 reworked to a content reference, re-calibration pending operator | routed fix-up concern (head `ea8bd7f1`) |
| 2026-10-14 | `architect` | 30 | adr006 29/30 and adr091 29/30 ABOVE band; undeclared 10/30 in band. Recorded as-is per Captain ruling (no fix-up budget left). **Variance flag:** adr006 was unchanged since the previous run yet moved 23/30 → 29/30, and clean-sqlc false alarms moved 1/30 → 6/30, so at 30 samples the band is not a stable gate for this arm. The recording (46 per case: adr006 41/46, adr091 45/46, undeclared 20/46; false alarms sqlc 3/46, workflow-v2 1/46, declared pair 28/46) is the measurement of record; #4398 tracks the empty-applies_to gap behind adr091's bimodality | PR #4396 comments (head `77c989e6`) |
| 2026-10-14 | `security` | 30 | all six catch cases 30/30 (ceiling, accepted by Captain: a ceiling case still detects a recall drop); clean-redaction 30/30 FA, clean-policy-test-only 0-1/30; clean cases reworked, re-calibration pending operator | routed fix-up concern (head `69549c71`) |
| 2026-10-14 | `security_portability` | 30 | clean-redaction 30/30 FA, clean-policy-test-only 21/30; reworked | routed fix-up concern (head `69549c71`) |
| 2026-10-14 | `security` | 30 | re-calibration after the clean-case rework: all four clean cases 0/30 FA; six catch cases 30/30 (ceiling, accepted) | operator dry run (head `f691917a`) |
| 2026-10-14 | `security_portability` | 30 | re-calibration after the clean-case rework: clean-auth 1/30 (undecodable), clean-policy-test-only 2/30, clean-ci and clean-redaction 0/30 | operator dry run (head `f691917a`) |
| 2026-10-15 | `without_conventions` + `with_conventions` (#4361, Step 1c) | 30 | three cases at the ceiling (100%/100%) and two in band only on scoring misses (honest rate ~100%) were cut by Captain ruling 2026-10-15; restates-approach-observable is in band on reviewer misses, honest 13/30 with and 16/30 without (Step 1c "Results"); harder cases continue in #4453 | Step 1c "Results" (head `23a9dee4`, re-scored at the operator commit) |
