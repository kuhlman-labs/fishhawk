package agenteval

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/anthropic"
)

// The LIVE two-arm plan-review catch-rate measurement (E55.4 / #2245).
//
// IT SKIPS IN EVERY IN-LOOP RUN, AND THAT IS THE HONEST STATE OF THE
// MEASUREMENT. This change ships the measurement APPARATUS and the offline
// gate (planreviewcatch_test.go, planreviewevidence_test.go, catchrategate),
// not a measured baseline: the runner sanitizes gate-subprocess environments
// through a default-deny allow-list that names ANTHROPIC_API_KEY on
// gateEnvDeny (runner/cmd/fishhawk-runner/gateenv.go), so no in-loop test can
// obtain a model credential. Until an operator records the first baseline,
// testdata/planreview-catchrate/evidence.json does not exist and the standing
// check fails closed on every review-prompt change, by design. The operator
// walk is docs/compliance/planreview-catchrate-evidence.md.
//
// DOUBLE-GATED on FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE plus
// one credential — FISHHAWKD_ANTHROPIC_API_KEY or FISHHAWKD_ANTHROPIC_AUTH_TOKEN
// (exactly one; see livecredential_test.go) — the posture injectionlive_test.go
// and severitycalibrationlive_test.go take. Two further knobs:
//
//	FISHHAWK_AGENTEVAL_PLANREVIEW_RECORD=1          write the evidence record (otherwise a dry run)
//	FISHHAWK_AGENTEVAL_PLANREVIEW_PIN_REASON=<why>  pin this measurement as the baseline (the explicit operator action)
//
// Every function it calls is offline-tested with fake senders.

const planReviewCatchEvidencePath = "testdata/planreview-catchrate/evidence.json"

func planReviewCatchLiveGate(t *testing.T) liveCredential {
	t.Helper()
	if os.Getenv("FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE") == "" {
		t.Skip("set FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE=1 to run the live plan-review catch-rate arms. Until they run and an operator records the baseline, #2245's two-arm evidence is UNMEASURED — see docs/compliance/planreview-catchrate-evidence.md.")
	}
	return requireLiveCredential(t, "Skipping the live plan-review catch-rate arms. #2245's two-arm evidence remains UNMEASURED — see docs/compliance/planreview-catchrate-evidence.md. The runner denies ANTHROPIC_API_KEY to gate subprocesses by design (runner gateenv.go), so this arm is operator-executed, never in-loop.")
}

// TestPlanReviewCatchRateLive runs both arms against
// DefaultQualityGeneratorModel (ONE generator config for both arms, so a
// model difference cannot confound the conventions effect) and judges the
// measurement with the SAME rule the offline gate applies. It FAILS — it does
// not warn — on a within-run regression, a regression against the pinned
// baseline, or an under-powered measurement: the tolerance is a judgement
// call, but it is the standing gate's bar, and RecordCatchRateEvidence refuses
// to write a failing measurement anyway.
func TestPlanReviewCatchRateLive(t *testing.T) {
	cred := planReviewCatchLiveGate(t)
	generator := anthropic.NewClient(cred.config(anthropic.Config{
		Model:     DefaultQualityGeneratorModel,
		MaxTokens: 4096,
		Timeout:   120 * time.Second,
	}))
	reason := os.Getenv("FISHHAWK_AGENTEVAL_PLANREVIEW_PIN_REASON")
	opts := RecordCatchRateOptions{
		PinBaseline: reason != "",
		Reason:      reason,
		DryRun:      os.Getenv("FISHHAWK_AGENTEVAL_PLANREVIEW_RECORD") != "1",
	}
	report, err := RecordCatchRateEvidence(context.Background(), generator, DefaultQualityGeneratorModel,
		committedCatchCorpus, committedCatchConventions, planReviewCatchEvidencePath, time.Now().UTC(), opts)
	if report != "" {
		t.Logf("\n%s", report)
	}
	if err != nil {
		t.Errorf("plan-review catch-rate measurement FAILED: %v", err)
	}
}
