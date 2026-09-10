package agenteval

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/anthropic"
)

// The two LIVE arms of E50.22 / #3309.
//
// BOTH SKIP IN THIS RUN, AND THAT IS THE HONEST STATE OF THE MEASUREMENT.
// This change ships the measurement APPARATUS, not the measurement: the
// offline gates in severitycalibration_test.go and
// adversarialretention_test.go prove the arms are CORRECT (the five #2119
// treatment literals are byte-exact against the real shipped prompt, the
// strip is posture-aware, the comparison is omission-monotone, the
// retention verdict's third state is not a pass). They prove NOTHING about
// whether #2119 calibrated anything, because nothing here calls a model.
//
// So #3309 done-means 2 (the severity delta is measured) and done-means 3
// (the revert-or-fix decision that delta and the retention comparison
// support) remain UNANSWERED until an operator runs these arms against a
// real model. That residual is by DESIGN, not an oversight: the runner
// sanitizes gate-subprocess environments through a default-deny allow-list
// that names ANTHROPIC_API_KEY on gateEnvDeny
// (runner/cmd/fishhawk-runner/gateenv.go), so no in-loop test can obtain a
// model credential, and no such credential is present in this environment
// at all. The operator walk is docs/compliance/severity-calibration-evidence.md.
//
// Both are DOUBLE-GATED on FISHHAWK_AGENTEVAL_CALIBRATION_LIVE plus
// FISHHAWKD_ANTHROPIC_API_KEY — the same posture injectionlive_test.go and
// envelopequalitylive_test.go take.

// calibrationLiveGate skips unless BOTH env gates are set, with a message
// naming #3309 and the done-means clauses the skip leaves undecided.
func calibrationLiveGate(t *testing.T, undecided string) string {
	t.Helper()
	if os.Getenv("FISHHAWK_AGENTEVAL_CALIBRATION_LIVE") == "" {
		t.Skip("set FISHHAWK_AGENTEVAL_CALIBRATION_LIVE=1 to run the live severity-calibration arms. Until they run, #3309 " + undecided + " remains UNMEASURED — see docs/compliance/severity-calibration-evidence.md.")
	}
	apiKey := os.Getenv("FISHHAWKD_ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("FISHHAWKD_ANTHROPIC_API_KEY unset; skipping the live severity-calibration arms. #3309 " + undecided + " remains UNMEASURED — see docs/compliance/severity-calibration-evidence.md. The runner denies ANTHROPIC_API_KEY to gate subprocesses by design (runner gateenv.go), so this arm is operator-executed, never in-loop.")
	}
	return apiKey
}

// TestSeverityCalibrationLive is #3309 done-means 2: does the #2119
// treatment move reviewer severities CLOSER to the severity the operator
// assigned when dispositioning the concern?
//
// ONE generator config drives BOTH arms — same model, same limits — because
// a model difference between arms would confound the treatment effect. The
// arms differ ONLY in the five #2119 literals StripCalibrationCriteria
// removes, which the offline TestCalibrationArms_DifferOnlyInCalibrationWording
// pins.
//
// A POSITIVE overall delta means the post-#2119 arm sits closer to the
// label. Read it with the coverage columns beside it: the score penalizes a
// MISSED labelled concern at the maximum tier distance, so a delta computed
// mostly from penalties measures COVERAGE, not calibration.
func TestSeverityCalibrationLive(t *testing.T) {
	apiKey := calibrationLiveGate(t, "acceptance criterion 2 (the severity-calibration delta is measured)")

	cases, err := LoadSeverityCalibrationCorpus(severityCalibrationCorpusDir)
	if err != nil {
		t.Fatalf("load severity-calibration corpus: %v", err)
	}
	ctx := context.Background()
	generator := anthropic.NewClient(anthropic.Config{
		APIKey:    apiKey,
		Model:     DefaultQualityGeneratorModel,
		MaxTokens: 4096,
		Timeout:   120 * time.Second,
	})

	pre, err := RunSeverityArm(ctx, generator, cases, ArmPreCalibration, DefaultCalibrationSamples)
	if err != nil {
		t.Fatalf("pre-#2119 arm: %v", err)
	}
	post, err := RunSeverityArm(ctx, generator, cases, ArmPostCalibration, DefaultCalibrationSamples)
	if err != nil {
		t.Fatalf("post-#2119 arm: %v", err)
	}

	cmp, err := CompareSeverityArms(pre, post, DefaultCalibrationImprovementThreshold)
	if err != nil {
		t.Fatalf("compare arms: %v", err)
	}
	t.Logf("pre-#2119 arm coverage: %+v", pre.PerCase)
	t.Logf("post-#2119 arm coverage: %+v", post.PerCase)
	t.Logf("\n%s", cmp.Render())

	// This arm REPORTS; it does not fail the build on a null result. The
	// threshold is a JUDGEMENT CALL (DefaultCalibrationImprovementThreshold
	// says so), so turning it into a red build would dress an unvalidated
	// number as a gate. The operator records the rendered comparison on
	// #3309 and takes the revert-or-fix decision there.
	if !cmp.Improved {
		t.Logf("the post-#2119 arm did NOT clear the %+.2f improvement threshold (overall delta %+.2f over %d scored cases, %d skipped) — record this on #3309 before treating #2119's severity rubric as settled",
			DefaultCalibrationImprovementThreshold, cmp.Overall, cmp.ScoredCases, cmp.SkippedCases)
	}
}

// TestAdversarialRetentionLive is #3309 done-means 2(b): did the #2119
// treatment COST any of the high-value adversarial findings the pre-#2119
// prompt produced? A finding produced in the PRE arm and absent in the POST
// arm is the loss condition that would argue for reverting.
//
// The judge is consulted only when no finding probe matched, so the
// expensive path is the ambiguous case rather than the default. It is
// constructed WITHOUT a response schema because the rubric dimensions are
// per-fixture; JudgeRubric decodes strictly and an undecodable card
// resolves to INDETERMINATE, which is explicitly not a pass.
func TestAdversarialRetentionLive(t *testing.T) {
	apiKey := calibrationLiveGate(t, "acceptance criterion 2(b) (adversarial-finding retention) and the criterion 3 revert-or-fix decision it feeds")

	cases, err := LoadAdversarialRetentionCorpus(adversarialRetentionCorpusDir)
	if err != nil {
		t.Fatalf("load adversarial-retention corpus: %v", err)
	}
	ctx := context.Background()
	generator := anthropic.NewClient(anthropic.Config{
		APIKey:    apiKey,
		Model:     DefaultQualityGeneratorModel,
		MaxTokens: 4096,
		Timeout:   120 * time.Second,
	})
	judge := NewRubricJudge(anthropic.NewClient(anthropic.Config{
		APIKey:    apiKey,
		Model:     DefaultJudgeModel,
		MaxTokens: 1024,
		Timeout:   60 * time.Second,
	}), DefaultJudgeModel, 2)

	pre, err := RunRetentionArm(ctx, generator, judge, cases, ArmPreCalibration)
	if err != nil {
		t.Fatalf("pre-#2119 arm: %v", err)
	}
	post, err := RunRetentionArm(ctx, generator, judge, cases, ArmPostCalibration)
	if err != nil {
		t.Fatalf("post-#2119 arm: %v", err)
	}
	t.Logf("\n%s", pre.Render())
	t.Logf("\n%s", post.Render())

	cmp, err := CompareRetentionArms(pre, post)
	if err != nil {
		t.Fatalf("compare arms: %v", err)
	}
	t.Logf("\n%s", cmp.Render())

	if len(cmp.Regressed) > 0 {
		t.Errorf("#2119 LOST %d adversarial finding(s) the pre-#2119 prompt produced: %v — this is done-means 3's loss condition; record it on #3309 and take the revert-or-fix decision there",
			len(cmp.Regressed), cmp.Regressed)
	}
	if len(cmp.Undecidable) > 0 {
		t.Logf("%d case(s) were INDETERMINATE in at least one arm and decide nothing either way: %v", len(cmp.Undecidable), cmp.Undecidable)
	}
}

// TestTreatmentLiteralsAreEnumeratedInREADME is the documentation half of
// the operator's count/enumeration-agreement fix, and it runs OFFLINE
// despite living beside the live arms — a prose claim that is only prose
// goes stale silently, which is exactly the #3013 class.
//
// The README states in words that the #2119 treatment set is exactly these
// five literals and that adding a SIXTH #2119 surface without registering
// it here silently weakens the pre arm. This asserts the enumeration in
// that sentence actually MATCHES the table, in both directions: every
// literal name appears in the README, and the README's stated count agrees
// with the table's length. Neither this nor
// TestTreatmentLiteralCount_IsFiveAndEnumerated can detect a new prompt
// surface nobody registers; that residual is stated in the README rather
// than papered over.
func TestTreatmentLiteralsAreEnumeratedInREADME(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	readme := string(raw)
	for _, name := range TreatmentLiteralNames() {
		if !strings.Contains(readme, "`"+name+"`") {
			t.Errorf("treatment literal %q is not enumerated in backend/internal/agenteval/README.md; the stated treatment set and the table disagree", name)
		}
	}
	if !strings.Contains(readme, "EXACTLY THESE FIVE LITERALS") {
		t.Error("README must state the treatment-set COUNT in words alongside the enumeration")
	}
	if len(treatmentLiterals) != 5 {
		t.Fatalf("the README says FIVE but the table has %d entries", len(treatmentLiterals))
	}
	if !strings.Contains(readme, "SILENTLY WEAKENS THE PRE ARM") {
		t.Error("README must state that a SIXTH unregistered #2119 surface weakens the pre arm")
	}
}
