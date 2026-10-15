package agenteval

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/anthropic"
)

// The LIVE catch-rate calibration dry run (E78.11 / #4313; the security arms,
// E80.17 / #4314): runs ONE arm of the evidence arm table (default: the
// architect persona arm; FISHHAWK_AGENTEVAL_CALIBRATE_ARM=security or
// security_portability for the security arms) against
// DefaultCatchRateGeneratorModel and logs the per-case band table
// (CalibrateCatchRateArm) for the PR. It NEVER writes evidence, and it FAILS
// when any catch case is out of the 30–80% band, so an out-of-band case is
// reworked before the re-pin recording, not discovered by it — and when a
// security_portability concern names a repository-specific marker, the
// finding the recording refuses.
//
// IT SKIPS IN EVERY IN-LOOP RUN (the runner's gate env denies every model
// credential). DOUBLE-GATED on FISHHAWK_AGENTEVAL_CALIBRATE_LIVE plus one
// credential through requireLiveCredential (livecredential_test.go). Knobs:
//
//	FISHHAWK_AGENTEVAL_CALIBRATE_ARM=<arm>      the arm-table row to calibrate (default architect)
//	FISHHAWK_AGENTEVAL_CALIBRATE_SAMPLES=<n>    samples per case (default 30, >= 1; 10 is too noisy to place a case in the band)
//
// The operator walk is docs/compliance/planreview-catchrate-evidence.md
// § "Calibration dry run".

const (
	calibrateDefaultArm     = ArmArchitect
	calibrateDefaultSamples = 30
)

// calibrationLiveKnobs reads the arm and the per-case sample count, refusing
// a sample count that is not a positive integer.
func calibrationLiveKnobs(getenv func(string) string) (arm string, samples int, err error) {
	arm, samples = calibrateDefaultArm, calibrateDefaultSamples
	if v := getenv("FISHHAWK_AGENTEVAL_CALIBRATE_ARM"); v != "" {
		arm = v
	}
	if v := getenv("FISHHAWK_AGENTEVAL_CALIBRATE_SAMPLES"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 {
			return "", 0, fmt.Errorf("FISHHAWK_AGENTEVAL_CALIBRATE_SAMPLES=%q: want a positive integer", v)
		}
		samples = n
	}
	return arm, samples, nil
}

// calibrateLive builds the generator on the catch-rate model (the SAME config
// the recording uses, planReviewCatchGeneratorConfig) and calibrates arm over
// the committed inputs.
func calibrateLive(ctx context.Context, newGenerator func(anthropic.Config) MessageSender, arm string, samples int) (CatchRateCalibration, string, error) {
	cfg := planReviewCatchGeneratorConfig()
	cal, err := CalibrateCatchRateArm(ctx, newGenerator(cfg), committedCatchInputs(), arm, samples)
	return cal, cfg.Model, err
}

func TestCatchRateCalibrationLive(t *testing.T) {
	if os.Getenv("FISHHAWK_AGENTEVAL_CALIBRATE_LIVE") == "" {
		t.Skip("set FISHHAWK_AGENTEVAL_CALIBRATE_LIVE=1 to run the catch-rate calibration dry run on " + DefaultCatchRateGeneratorModel + ". Until it runs, the #4313 architect cases' and the #4314 security and security_portability cases' 30-80% band is UNMEASURED — see docs/compliance/planreview-catchrate-evidence.md.")
	}
	cred := requireLiveCredential(t, "Skipping the catch-rate calibration dry run; the #4313 architect cases' and the #4314 security and security_portability cases' band is UNMEASURED — see docs/compliance/planreview-catchrate-evidence.md.")
	arm, samples, err := calibrationLiveKnobs(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	cal, model, err := calibrateLive(context.Background(),
		func(c anthropic.Config) MessageSender { return anthropic.NewClient(cred.config(c)) }, arm, samples)
	if err != nil {
		t.Fatalf("calibration dry run FAILED: %v", err)
	}
	t.Logf("\nGenerator model: %s\n\n%s", model, cal.Markdown())
	if oob := cal.OutOfBand(); len(oob) > 0 {
		t.Errorf("catch cases out of the 30-80%% band: %v — rework them (a routed fix-up) before the shared re-pin recording", oob)
	}
	if err := calibrationPortabilityRefusal(cal); err != nil {
		t.Error(err)
	}
}

// calibrationPortabilityRefusal is the live dry run's portability check: an
// error naming every security_portability concern that names a
// repository-specific marker, nil otherwise.
func calibrationPortabilityRefusal(cal CatchRateCalibration) error {
	if len(cal.PortabilityFindings) == 0 {
		return nil
	}
	return fmt.Errorf("security_portability raised %d repository-specific finding(s): %s — the recording refuses these (ErrPortabilityRepoSpecificFinding); read the concerns and rework the remit or the case before the #4314 re-pin recording", len(cal.PortabilityFindings), renderPortabilityFindings(cal.PortabilityFindings))
}

// TestCalibrationLiveKnobs is OFFLINE: the knob defaults, an override, and the
// refusal of a non-positive or non-numeric sample count.
func TestCalibrationLiveKnobs(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	arm, samples, err := calibrationLiveKnobs(env(nil))
	if err != nil || arm != "architect" || samples != 30 {
		t.Errorf("defaults = %q, %d, %v; want architect, 30, nil", arm, samples, err)
	}
	arm, samples, err = calibrationLiveKnobs(env(map[string]string{"FISHHAWK_AGENTEVAL_CALIBRATE_ARM": "with_conventions", "FISHHAWK_AGENTEVAL_CALIBRATE_SAMPLES": "25"}))
	if err != nil || arm != "with_conventions" || samples != 25 {
		t.Errorf("override = %q, %d, %v; want with_conventions, 25, nil", arm, samples, err)
	}
	for _, bad := range []string{"0", "-3", "ten"} {
		if _, _, err := calibrationLiveKnobs(env(map[string]string{"FISHHAWK_AGENTEVAL_CALIBRATE_SAMPLES": bad})); err == nil {
			t.Errorf("samples %q accepted, want a refusal", bad)
		}
	}
}

// TestCatchRateCalibrationLive_WiresCatchRateGeneratorModel is OFFLINE: the
// live seam builds ONE generator on the catch-rate model and calibrates the
// committed architect arm end to end (a scripted sender catching every catch
// case is ABOVE band, so the live test would fail on it).
func TestCatchRateCalibrationLive_WiresCatchRateGeneratorModel(t *testing.T) {
	var got []anthropic.Config
	cal, model, err := calibrateLive(context.Background(), func(c anthropic.Config) MessageSender {
		got = append(got, c)
		return newScriptedCatchSender(t, 0, 0)
	}, ArmArchitect, 2)
	if err != nil {
		t.Fatalf("calibrateLive: %v", err)
	}
	if len(got) != 1 || got[0].Model != "claude-sonnet-5-5" || model != "claude-sonnet-5-5" {
		t.Errorf("generator configs = %+v, model %q; want one claude-sonnet-5-5 client", got, model)
	}
	if oob := cal.OutOfBand(); len(oob) != 3 {
		t.Errorf("OutOfBand() = %v, want the 3 always-caught architect catch cases", oob)
	}
}

// TestCatchRateCalibrationLive_PortabilityFindingFails is OFFLINE: the live
// dry run's portability check fails on a calibration carrying a
// repository-specific finding (naming it) and passes on one without.
func TestCatchRateCalibrationLive_PortabilityFindingFails(t *testing.T) {
	if err := calibrationPortabilityRefusal(CatchRateCalibration{Arm: ArmSecurityPortability}); err != nil {
		t.Errorf("a calibration with no finding failed: %v", err)
	}
	err := calibrationPortabilityRefusal(CatchRateCalibration{Arm: ArmSecurityPortability,
		PortabilityFindings: []CatchRatePortabilityFinding{{Case: "clean-x", Trial: 2, Marker: "gateenv.go"}}})
	if err == nil || !strings.Contains(err.Error(), `case "clean-x" trial 2 names "gateenv.go"`) {
		t.Errorf("err = %v, want a failure naming the finding", err)
	}
}
