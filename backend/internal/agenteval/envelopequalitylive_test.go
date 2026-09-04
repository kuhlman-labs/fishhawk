package agenteval

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/anthropic"
)

// TestEnvelopeQualityLive is the opt-in live arm of measurement 1: does
// the #2290 quarantine envelope DILUTE plan quality on legitimate issues?
//
// Double-gated on FISHHAWK_AGENTEVAL_QUALITY_LIVE +
// FISHHAWKD_ANTHROPIC_API_KEY, the same pattern TestCalibrateLive uses, so
// the committed-tree verify and CI never make a model call.
//
// IT SKIPS IN THIS RUN — no FISHHAWKD_ANTHROPIC_API_KEY is configured
// here, so #2291 acceptance criterion 4 (the quality delta) is NOT decided
// by this change. #3187 owns it. The offline
// TestQualityArm_AggregatesEndToEnd is the expectation_basis: it proves
// the arithmetic and the threshold verdict this arm would feed, without
// claiming the measurement was taken.
func TestEnvelopeQualityLive(t *testing.T) {
	if os.Getenv("FISHHAWK_AGENTEVAL_QUALITY_LIVE") == "" {
		t.Skip("set FISHHAWK_AGENTEVAL_QUALITY_LIVE=1 to run the live envelope-quality arms. Until they run, #2291 acceptance criterion 4 (no plan-quality regression) remains UNMEASURED — see #3187 and docs/compliance/prompt-injection-evidence.md.")
	}
	apiKey := os.Getenv("FISHHAWKD_ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("FISHHAWKD_ANTHROPIC_API_KEY unset; skipping the live envelope-quality arms. #2291 acceptance criterion 4 remains UNMEASURED — see #3187 and docs/compliance/prompt-injection-evidence.md.")
	}

	cases := loadQualityCases(t)
	ctx := context.Background()

	// ONE generator config for BOTH arms: same model, same limits. A model
	// difference between arms would confound the treatment effect.
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
		Schema:    RubricCardSchema(qualityDimensions),
	}), DefaultJudgeModel, 2)

	envelope, err := RunQualityArm(ctx, generator, judge, cases, ArmEnvelope, DefaultQualitySamples)
	if err != nil {
		t.Fatalf("envelope arm: %v", err)
	}
	noEnvelope, err := RunQualityArm(ctx, generator, judge, cases, ArmNoEnvelope, DefaultQualitySamples)
	if err != nil {
		t.Fatalf("no-envelope arm: %v", err)
	}

	delta := CompareQualityArms(envelope, noEnvelope, DefaultQualityRegressionThreshold)
	t.Logf("envelope arm: %+v", envelope)
	t.Logf("no-envelope arm: %+v", noEnvelope)
	t.Logf("delta (envelope - no_envelope): %+v", delta)
	if delta.Regressed {
		t.Errorf("the quarantine envelope regressed plan quality by %.3f, past the %.2f threshold — record this on #3187 before treating #2290's treatment as settled",
			delta.Overall, DefaultQualityRegressionThreshold)
	}
}
