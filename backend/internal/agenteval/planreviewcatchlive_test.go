package agenteval

import (
	"context"
	"os"
	"path/filepath"
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
// Every function it calls is offline-tested with fake senders. The arms run on
// DefaultCatchRateGeneratorModel, and this file also carries one OFFLINE wiring
// test (TestPlanReviewCatchRateLive_WiresCatchRateGeneratorModel) that runs in
// every verify: it pins that the generator client and the recorded
// generator_model come from that one constant.

const planReviewCatchEvidencePath = "testdata/planreview-catchrate/evidence.json"

func planReviewCatchLiveGate(t *testing.T) liveCredential {
	t.Helper()
	if os.Getenv("FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE") == "" {
		t.Skip("set FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE=1 to run the live plan-review catch-rate arms on " + DefaultCatchRateGeneratorModel + ". Until they run and an operator records the baseline, #2245's two-arm evidence is UNMEASURED — see docs/compliance/planreview-catchrate-evidence.md.")
	}
	return requireLiveCredential(t, "Skipping the live plan-review catch-rate arms. #2245's two-arm evidence remains UNMEASURED — see docs/compliance/planreview-catchrate-evidence.md. The runner denies ANTHROPIC_API_KEY to gate subprocesses by design (runner gateenv.go), so this arm is operator-executed, never in-loop.")
}

// planReviewCatchGeneratorConfig is the generator's client config, minus the
// credential. Its Model is the ONE value recordPlanReviewCatchLive hands to
// RecordCatchRateEvidence as the recorded generator_model.
func planReviewCatchGeneratorConfig() anthropic.Config {
	return anthropic.Config{
		Model:     DefaultCatchRateGeneratorModel,
		MaxTokens: 4096,
		Timeout:   120 * time.Second,
	}
}

// recordPlanReviewCatchLive builds the generator config once and uses that same
// cfg.Model for the client AND the recorded generator_model, so the model the
// arms ran on and the model the evidence names cannot disagree.
func recordPlanReviewCatchLive(ctx context.Context, newGenerator func(anthropic.Config) MessageSender, evidencePath string, now time.Time, opts RecordCatchRateOptions) (string, error) {
	cfg := planReviewCatchGeneratorConfig()
	return RecordCatchRateEvidence(ctx, newGenerator(cfg), cfg.Model,
		committedCatchCorpus, committedCatchConventions, evidencePath, now, opts)
}

// TestPlanReviewCatchRateLive runs both arms against
// DefaultCatchRateGeneratorModel (ONE generator config for both arms, so a
// model difference cannot confound the conventions effect) and judges the
// measurement with the SAME rule the offline gate applies. It FAILS — it does
// not warn — on a within-run regression, a regression against the pinned
// baseline, or an under-powered measurement: the tolerance is a judgement
// call, but it is the standing gate's bar, and RecordCatchRateEvidence refuses
// to write a failing measurement anyway.
func TestPlanReviewCatchRateLive(t *testing.T) {
	cred := planReviewCatchLiveGate(t)
	reason := os.Getenv("FISHHAWK_AGENTEVAL_PLANREVIEW_PIN_REASON")
	opts := RecordCatchRateOptions{
		PinBaseline: reason != "",
		Reason:      reason,
		DryRun:      os.Getenv("FISHHAWK_AGENTEVAL_PLANREVIEW_RECORD") != "1",
	}
	report, err := recordPlanReviewCatchLive(context.Background(),
		func(c anthropic.Config) MessageSender { return anthropic.NewClient(cred.config(c)) },
		planReviewCatchEvidencePath, time.Now().UTC(), opts)
	if report != "" {
		t.Logf("\n%s", report)
	}
	if err != nil {
		t.Errorf("plan-review catch-rate measurement FAILED: %v", err)
	}
}

// TestPlanReviewCatchRateLive_WiresCatchRateGeneratorModel is the OFFLINE wiring
// pin for #4235: it drives the same seam the live test uses with a
// config-capturing fake sender and a real write to a temp dir. The scripted
// sender ignores the model, so the only thing that can fail it is the model
// value it observes. Expectations are string LITERALS, never the constant
// compared against itself.
func TestPlanReviewCatchRateLive_WiresCatchRateGeneratorModel(t *testing.T) {
	all := committedCatchTrials(t)
	path := filepath.Join(t.TempDir(), "evidence.json")

	var got []anthropic.Config
	newGenerator := func(c anthropic.Config) MessageSender {
		got = append(got, c)
		return newScriptedCatchSender(t, all, all)
	}
	if _, err := recordPlanReviewCatchLive(context.Background(), newGenerator, path, catchT0,
		RecordCatchRateOptions{PinBaseline: true, Reason: "wiring pin"}); err != nil {
		t.Fatalf("recordPlanReviewCatchLive: %v", err)
	}

	// (i) the generator client is built exactly once, on the catch-rate model.
	if len(got) != 1 {
		t.Fatalf("generator factory called %d times, want 1", len(got))
	}
	if got[0].Model != "claude-sonnet-5-5" {
		t.Errorf("generator client Model = %q, want %q", got[0].Model, "claude-sonnet-5-5")
	}
	if got[0].MaxTokens != 4096 {
		t.Errorf("generator client MaxTokens = %d, want 4096", got[0].MaxTokens)
	}
	if got[0].Timeout != 120*time.Second {
		t.Errorf("generator client Timeout = %v, want 120s", got[0].Timeout)
	}

	// (ii) the WRITTEN record, and its pinned baseline, name the same model.
	rec := readCatchEvidence(t, path)
	if rec.GeneratorModel != "claude-sonnet-5-5" {
		t.Errorf("record generator_model = %q, want %q", rec.GeneratorModel, "claude-sonnet-5-5")
	}
	if rec.Baseline == nil {
		t.Fatal("the pinned record has no baseline")
	}
	if rec.Baseline.GeneratorModel != "claude-sonnet-5-5" {
		t.Errorf("baseline generator_model = %q, want %q", rec.Baseline.GeneratorModel, "claude-sonnet-5-5")
	}

	// (iii) the record passes the offline gate keyed to the catch-rate constant.
	if _, err := CheckCatchRateEvidence(path, committedCatchCorpus, committedCatchConventions, DefaultCatchRateGeneratorModel); err != nil {
		t.Errorf("CheckCatchRateEvidence on the recorded evidence: %v", err)
	}

	// (iv) the constant, and the defaults it must NOT have moved.
	if DefaultCatchRateGeneratorModel != "claude-sonnet-5-5" {
		t.Errorf("DefaultCatchRateGeneratorModel = %q, want %q", DefaultCatchRateGeneratorModel, "claude-sonnet-5-5")
	}
	if DefaultJudgeModel != "claude-sonnet-4-6" {
		t.Errorf("DefaultJudgeModel = %q, want %q (the catch-rate model must not move it)", DefaultJudgeModel, "claude-sonnet-4-6")
	}
	if DefaultQualityGeneratorModel != "claude-sonnet-4-6" {
		t.Errorf("DefaultQualityGeneratorModel = %q, want %q (the catch-rate model must not move it)", DefaultQualityGeneratorModel, "claude-sonnet-4-6")
	}
}
