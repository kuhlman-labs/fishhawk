package agenteval

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/anthropic"
	"github.com/kuhlman-labs/fishhawk/redaction"
)

// The LIVE plan-review catch-rate measurement (E55.4 / #2245): EVERY arm of
// the evidence arm table — the two conventions arms, the architect persona
// arm (E78.11 / #4313) and the security and security_portability persona arms
// (E80.17 / #4314) — in ONE recording.
//
// IT SKIPS IN EVERY IN-LOOP RUN: the runner sanitizes gate-subprocess
// environments through a default-deny allow-list that names ANTHROPIC_API_KEY
// on gateEnvDeny (runner/cmd/fishhawk-runner/gateenv.go), so no in-loop test
// can obtain a model credential, and every recording is operator-executed.
// The committed testdata/planreview-catchrate/evidence.json and its per-trial
// sidecar evidence.trials.json (agenteval.CatchRateTrialsPath) are written
// together by a recording run; a change to the corpus, the conventions
// fixture, the persona corpora, the architect remit or decision record, the
// security remit, convention or markers.json, the rendered plan-review or
// persona prompt, or the catch rule makes the
// committed pair STALE, and the standing check fails closed on a review-prompt
// change until an operator re-records it. The operator walk is
// docs/compliance/planreview-catchrate-evidence.md.
//
// DOUBLE-GATED on FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE plus
// one credential — FISHHAWKD_ANTHROPIC_API_KEY or FISHHAWKD_ANTHROPIC_AUTH_TOKEN
// (exactly one; see livecredential_test.go) — the posture injectionlive_test.go
// and severitycalibrationlive_test.go take. Further knobs:
//
//	FISHHAWK_AGENTEVAL_PLANREVIEW_RECORD=1          write the evidence record and its trial sidecar (otherwise a dry run)
//	FISHHAWK_AGENTEVAL_PLANREVIEW_PIN_REASON=<why>  pin this measurement as the baseline (the explicit operator action)
//	FISHHAWK_AGENTEVAL_DIAG_DIR=<dir>               root of the per-run diagnostics file (default $TMPDIR/fishhawk-agenteval);
//	                                                every run writes <root>/<UTC 20060102T150405Z>/planreview-diag.jsonl
//	FISHHAWK_AGENTEVAL_PLANREVIEW_CASES=<id,...>    subset dry run: only these cases (never recordable)
//	FISHHAWK_AGENTEVAL_PLANREVIEW_TRIALS=<n>        subset dry run: n samples per case (never recordable)
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
		t.Skip("set FISHHAWK_AGENTEVAL_PLANREVIEW_LIVE=1 to run the live plan-review catch-rate arms on " + DefaultCatchRateGeneratorModel + ". Until they run and an operator records the baseline, #2245's arm-keyed evidence (both conventions arms, the #4313 architect arm and the #4314 security and security_portability arms) is UNMEASURED — see docs/compliance/planreview-catchrate-evidence.md.")
	}
	return requireLiveCredential(t, "Skipping the live plan-review catch-rate arms. #2245's arm-keyed evidence remains UNMEASURED — see docs/compliance/planreview-catchrate-evidence.md. The runner denies ANTHROPIC_API_KEY to gate subprocesses by design (runner gateenv.go), so this arm is operator-executed, never in-loop.")
}

// planReviewCatchGeneratorConfig is the generator's client config, minus the
// credential. Its Model is the ONE value recordPlanReviewCatchLive hands to
// RecordCatchRateEvidence as the recorded generator_model.
func planReviewCatchGeneratorConfig() anthropic.Config {
	return anthropic.Config{
		Model:     DefaultCatchRateGeneratorModel,
		MaxTokens: 8192,
		Timeout:   120 * time.Second,
	}
}

// recordPlanReviewCatchLive builds the generator config once and uses that same
// cfg.Model for the client AND the recorded generator_model, so the model the
// arms ran on and the model the evidence names cannot disagree. Every run
// writes its per-trial diagnostics file at diagPath.
func recordPlanReviewCatchLive(ctx context.Context, newGenerator func(anthropic.Config) MessageSender, evidencePath string, now time.Time, opts RecordCatchRateOptions, diagPath string, diagOpts CatchRateDiagOptions) (string, error) {
	cfg := planReviewCatchGeneratorConfig()
	return RecordCatchRateEvidenceWithDiagnostics(ctx, newGenerator(cfg), cfg.Model,
		committedCatchInputs(), evidencePath, now, opts, diagPath, diagOpts)
}

// TestPlanReviewCatchRateLive runs every arm against
// DefaultCatchRateGeneratorModel (ONE generator config for every arm, so a
// model difference cannot confound the conventions or persona effect) and judges the
// measurement with the SAME rule the offline gate applies. It FAILS — it does
// not warn — on a within-run regression, a regression against the pinned
// baseline, or an under-powered measurement: the tolerance is a judgement
// call, but it is the standing gate's bar, and RecordCatchRateEvidence refuses
// to write a failing measurement anyway.
func TestPlanReviewCatchRateLive(t *testing.T) {
	cred := planReviewCatchLiveGate(t)
	subset, err := ParseCatchRateSubset(os.Getenv)
	if err != nil {
		t.Fatalf("subset knobs: %v", err)
	}
	reason := os.Getenv("FISHHAWK_AGENTEVAL_PLANREVIEW_PIN_REASON")
	opts := RecordCatchRateOptions{
		PinBaseline: reason != "",
		Reason:      reason,
		DryRun:      os.Getenv("FISHHAWK_AGENTEVAL_PLANREVIEW_RECORD") != "1",
		Subset:      subset,
	}
	now := time.Now().UTC()
	diagPath, err := ResolveCatchRateDiagPath(os.Getenv, now)
	if err != nil {
		t.Fatalf("diagnostics path: %v", err)
	}
	creds := CatchRateDiagCredentials(os.Getenv)
	for name, v := range map[string]string{"resolved_api_key": cred.apiKey, "resolved_auth_token": cred.authToken} {
		if v != "" {
			creds = append(creds, redaction.KnownValue{Name: name, Value: v})
		}
	}
	t.Logf("per-trial diagnostics: %s", diagPath)
	report, err := recordPlanReviewCatchLive(context.Background(),
		func(c anthropic.Config) MessageSender { return anthropic.NewClient(cred.config(c)) },
		planReviewCatchEvidencePath, now, opts, diagPath, CatchRateDiagOptions{Progress: os.Stderr, Credentials: creds})
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
// compared against itself. It is also the end-to-end cross-layer check of
// #4313: ALL arms are recorded through the one seam — the scripted sender
// routes the architect arm on the persona remit heading — so corpus →
// personaprompt → repodoc/decisionrecord → prompt → classifier → evidence →
// gate runs in every verify.
func TestPlanReviewCatchRateLive_WiresCatchRateGeneratorModel(t *testing.T) {
	all := committedCatchTrials(t)
	path := filepath.Join(t.TempDir(), "evidence.json")

	var got []anthropic.Config
	newGenerator := func(c anthropic.Config) MessageSender {
		got = append(got, c)
		return newScriptedCatchSender(t, all, all)
	}
	diagPath := filepath.Join(t.TempDir(), "diag", CatchRateDiagFileName)
	if _, err := recordPlanReviewCatchLive(context.Background(), newGenerator, path, catchT0,
		RecordCatchRateOptions{PinBaseline: true, Reason: "wiring pin"}, diagPath, CatchRateDiagOptions{}); err != nil {
		t.Fatalf("recordPlanReviewCatchLive: %v", err)
	}

	// (i) the generator client is built exactly once, on the catch-rate model.
	if len(got) != 1 {
		t.Fatalf("generator factory called %d times, want 1", len(got))
	}
	if got[0].Model != "claude-sonnet-5-5" {
		t.Errorf("generator client Model = %q, want %q", got[0].Model, "claude-sonnet-5-5")
	}
	if got[0].MaxTokens != 8192 {
		t.Errorf("generator client MaxTokens = %d, want 8192", got[0].MaxTokens)
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

	// (iii) the record passes the offline gate keyed to the catch-rate
	// constant, and the seam wrote its trial sidecar beside it.
	if _, err := os.Stat(CatchRateTrialsPath(path)); err != nil {
		t.Errorf("the live seam wrote no trial sidecar at %s: %v", CatchRateTrialsPath(path), err)
	}
	if _, err := CheckCatchRateEvidence(path, committedCatchInputs(), DefaultCatchRateGeneratorModel); err != nil {
		t.Errorf("CheckCatchRateEvidence on the recorded evidence: %v", err)
	}
	// (iii-b) every arm of the arm table was recorded — five arms — the
	// architect and both security arms as persona arms whose trials the
	// scripted sender answered per case.
	if len(rec.Arms) != 5 {
		t.Errorf("the recording carries %d arms %v, want 5", len(rec.Arms), sortedArmNames(rec.Arms))
	}
	for arm, kind := range map[string]string{"without_conventions": "conventions", "with_conventions": "conventions", "architect": "persona",
		"security": "persona", "security_portability": "persona"} {
		a, ok := rec.Arms[arm]
		if !ok {
			t.Errorf("the recording carries no %q arm", arm)
			continue
		}
		if a.Kind != kind {
			t.Errorf("arm %q kind = %q, want %q", arm, a.Kind, kind)
		}
	}
	arch := pooledClassCounts(rec.Arms["architect"].PerCase)
	if arch.catchTrials == 0 || arch.caught != arch.catchTrials || arch.cleanTrials == 0 || arch.alarms != 0 {
		t.Errorf("architect arm counts = %+v, want every catch trial caught and no clean-case alarm", arch)
	}
	sec := pooledClassCounts(rec.Arms["security"].PerCase)
	if sec.catchTrials == 0 || sec.caught != sec.catchTrials || sec.cleanTrials == 0 || sec.alarms != 0 {
		t.Errorf("security arm counts = %+v, want every catch trial caught and no clean-case alarm", sec)
	}
	port := pooledClassCounts(rec.Arms["security_portability"].PerCase)
	if port.catchTrials != 0 || port.cleanTrials == 0 || port.alarms != 0 {
		t.Errorf("security_portability arm counts = %+v, want clean trials only and no alarm", port)
	}

	// (iii-c) the seam wrote the per-trial diagnostics: one trial line per
	// recorded trial in EVERY arm, then a passed summary line.
	lines := readCatchDiagLines(t, diagPath)
	perArm := map[string]int{}
	for _, l := range lines {
		if l["kind"] == "trial" {
			perArm[l["arm"].(string)]++
		}
	}
	for arm, a := range rec.Arms {
		want := 0
		for _, c := range a.PerCase {
			want += c.Trials
		}
		if perArm[arm] != want {
			t.Errorf("diagnostics carry %d trial lines for arm %q, want %d (one per recorded trial)", perArm[arm], arm, want)
		}
	}
	if last := lines[len(lines)-1]; last["kind"] != "summary" || last["outcome"] != "passed" {
		t.Errorf("last diagnostics line = kind %v outcome %v, want a passed summary", last["kind"], last["outcome"])
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
