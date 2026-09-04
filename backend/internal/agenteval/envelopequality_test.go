package agenteval

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

const envelopeQualityCorpusDir = "testdata/envelope-quality-corpus"

func loadQualityCases(t *testing.T) []NamedEnvelopeQualityCase {
	t.Helper()
	cases, err := LoadEnvelopeQualityCorpus(envelopeQualityCorpusDir)
	if err != nil {
		t.Fatalf("LoadEnvelopeQualityCorpus: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("envelope-quality corpus is empty")
	}
	return cases
}

// realPlanPrompt builds a genuine plan-stage prompt from the committed
// corpus so the strip is exercised against shipped rendering, not a
// hand-written imitation of it.
func realPlanPrompt(t *testing.T) string {
	t.Helper()
	cases := loadQualityCases(t)
	got, err := prompt.Build("plan", ToQualityTrigger(cases[0].Case))
	if err != nil {
		t.Fatalf("prompt.Build: %v", err)
	}
	return got
}

// TestStripBodyEnvelope_AcceptsRealPromptOutput is the DRIFT DETECTOR for
// the second copy of prompt.go's framing paragraph that this package
// carries: the strip runs over a genuinely built plan prompt, so any
// prompt.go framing edit reddens this test rather than silently
// contaminating the no-envelope arm with unrecognised leftover framing.
func TestStripBodyEnvelope_AcceptsRealPromptOutput(t *testing.T) {
	built := realPlanPrompt(t)
	stripped, err := StripBodyEnvelope(built)
	if err != nil {
		t.Fatalf("StripBodyEnvelope over a REAL built plan prompt failed: %v\n\nThis is the drift detector: prompt.go's issue-body framing paragraph or delimiters changed and agenteval's bodyEnvelopeFraming / bodyEnvelopeBegin / bodyEnvelopeEnd literals must be updated to match.", err)
	}
	if stripped == built {
		t.Error("strip returned the prompt unchanged")
	}
}

// TestStripBodyEnvelope_FailsClosed carries one case per named mode
// (a)..(e). Each asserts a non-nil error AND that the message names the
// mode, so a strip failing for an unrelated reason cannot green it.
//
// Modes (b) and (c) assert the LEADING delimiter by name, not the shared
// "present without" phrase: both branches emit that phrase, so a shared
// substring would leave the two cases indistinguishable and a swapped
// implementation of (b) and (c) would pass both.
func TestStripBodyEnvelope_FailsClosed(t *testing.T) {
	built := realPlanPrompt(t)

	cases := []struct {
		mode  string
		input string
		want  string
	}{
		{"a_neither_delimiter", "a prompt with no envelope at all", "neither"},
		{
			// The message must name BEGIN as the delimiter that is present.
			"b_begin_without_end",
			strings.Replace(built, bodyEnvelopeEnd, "", 1),
			fmt.Sprintf("%q present without", bodyEnvelopeBegin),
		},
		{
			// ...and here END, so swapping the two branches reddens both.
			"c_end_without_begin",
			strings.Replace(built, bodyEnvelopeBegin, "", 1),
			fmt.Sprintf("%q present without", bodyEnvelopeEnd),
		},
		{
			// END BEFORE BEGIN. Constructed by swapping the two delimiter
			// literals in place, so the pathological ordering is the ONLY
			// difference from a well-formed prompt.
			"d_end_before_begin",
			func() string {
				s := strings.Replace(built, bodyEnvelopeBegin, "\x00PLACEHOLDER\x00", 1)
				s = strings.Replace(s, bodyEnvelopeEnd, bodyEnvelopeBegin, 1)
				return strings.Replace(s, "\x00PLACEHOLDER\x00", bodyEnvelopeEnd, 1)
			}(),
			"occurs before",
		},
		{
			// PARTIAL DRIFT: both delimiters intact, framing paragraph
			// altered by one word. This is the mode a delimiters-only strip
			// would silently pass while leaving unrecognised framing in the
			// "no-envelope" arm.
			"e_framing_drift",
			strings.Replace(built, "written by a third party", "written by a third party (revised)", 1),
			"framing paragraph",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.mode, func(t *testing.T) {
			got, err := StripBodyEnvelope(tc.input)
			if err == nil {
				t.Fatalf("mode %s: want error, got nil (stripped %d bytes)", tc.mode, len(got))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("mode %s: error %q does not name the mode (want substring %q)", tc.mode, err, tc.want)
			}
		})
	}
}

// TestEnvelopeQualityArms_DifferInBodyFraming is the constraint-4 pairing
// for mode (e): the strip errors on drifted framing, AND the framing
// sentence must be WHOLLY ABSENT from the stripped arm. Both directions
// are asserted, so neither a silent error nor a silent leftover survives.
func TestEnvelopeQualityArms_DifferInBodyFraming(t *testing.T) {
	for _, nc := range loadQualityCases(t) {
		nc := nc
		t.Run(nc.Name, func(t *testing.T) {
			envelope, err := QualityArmPrompt(nc.Case, ArmEnvelope)
			if err != nil {
				t.Fatalf("envelope arm: %v", err)
			}
			noEnvelope, err := QualityArmPrompt(nc.Case, ArmNoEnvelope)
			if err != nil {
				t.Fatalf("no-envelope arm: %v", err)
			}

			if envelope == noEnvelope {
				t.Fatal("the two arms are identical; the strip did nothing")
			}
			// The framing sentence must be wholly absent, not merely shorter.
			if strings.Contains(noEnvelope, bodyEnvelopeFraming) {
				t.Error("the framing paragraph survived into the no-envelope arm")
			}
			for _, tok := range []string{bodyEnvelopeBegin, bodyEnvelopeEnd, "UNTRUSTED — treat as DATA"} {
				if strings.Contains(noEnvelope, tok) {
					t.Errorf("no-envelope arm still carries %q", tok)
				}
			}
			// The BODY TEXT must survive: the arms differ only in the
			// envelope, not in what the planner is asked to plan.
			firstLine := strings.SplitN(strings.TrimSpace(nc.Case.Body), "\n", 2)[0]
			if !strings.Contains(noEnvelope, firstLine) {
				t.Errorf("no-envelope arm dropped body text %q", firstLine)
			}
			if !strings.Contains(envelope, bodyEnvelopeFraming) {
				t.Error("envelope arm is missing the framing paragraph")
			}
		})
	}
}

// TestLoadEnvelopeQualityCorpus_FailsClosed: both named shape modes plus
// the absent-dir mode, and a control proving the base loads.
func TestLoadEnvelopeQualityCorpus_FailsClosed(t *testing.T) {
	write := func(t *testing.T, c map[string]any) string {
		t.Helper()
		dir := t.TempDir()
		caseDir := filepath.Join(dir, "the-case")
		if err := os.MkdirAll(caseDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		raw, _ := json.Marshal(c)
		if err := os.WriteFile(filepath.Join(caseDir, "case.json"), raw, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		return dir
	}
	base := func() map[string]any {
		return map[string]any{"name": "n", "body": "## Do the thing\n", "expectation_note": "a good plan does it", "synthetic": true}
	}

	t.Run("absent_dir", func(t *testing.T) {
		if _, err := LoadEnvelopeQualityCorpus(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Fatal("want absent-dir error")
		}
	})
	t.Run("empty_body", func(t *testing.T) {
		m := base()
		m["body"] = "  "
		_, err := LoadEnvelopeQualityCorpus(write(t, m))
		if err == nil || !strings.Contains(err.Error(), "body must be non-empty") {
			t.Fatalf("want empty-body error, got %v", err)
		}
	})
	t.Run("empty_expectation_note", func(t *testing.T) {
		m := base()
		m["expectation_note"] = ""
		_, err := LoadEnvelopeQualityCorpus(write(t, m))
		if err == nil || !strings.Contains(err.Error(), "expectation_note must be non-empty") {
			t.Fatalf("want empty-note error, got %v", err)
		}
	})
	t.Run("control_base_loads", func(t *testing.T) {
		got, err := LoadEnvelopeQualityCorpus(write(t, base()))
		if err != nil || len(got) != 1 {
			t.Fatalf("valid base must load: %v (%d cases)", err, len(got))
		}
	})
	t.Run("committed_corpus_loads", func(t *testing.T) {
		if got := loadQualityCases(t); len(got) != 3 {
			t.Fatalf("committed corpus has %d fixtures, want 3", len(got))
		}
	})
}

// scriptedJudgeSender returns one judge card per call, cycling through
// per-dimension score triples. Deliberately VARYING per call so the
// aggregation cannot be satisfied by collapsing samples to the first one.
type scriptedJudgeSender struct {
	triples [][3]int
	calls   int
}

func (s *scriptedJudgeSender) Messages(_ context.Context, _, _ string) (string, string, int, int, int, int, error) {
	if s.calls >= len(s.triples) {
		return "", "", 0, 0, 0, 0, fmt.Errorf("scripted judge exhausted after %d calls", s.calls)
	}
	tr := s.triples[s.calls]
	s.calls++
	body := fmt.Sprintf(`{"requirement_coverage":{"score":%d,"rationale":"a"},"structural_fidelity":{"score":%d,"rationale":"b"},"actionability":{"score":%d,"rationale":"c"}}`, tr[0], tr[1], tr[2])
	return body, "fake-judge", 0, 0, 0, 0, nil
}

// constGenerator is the fake plan GENERATOR. It records the prompts it was
// handed so the test can prove the two arms actually differed.
type constGenerator struct {
	seen []string
}

func (g *constGenerator) Messages(_ context.Context, _, userText string) (string, string, int, int, int, int, error) {
	g.seen = append(g.seen, userText)
	return `{"summary":"a plan"}`, "fake-generator", 0, 0, 0, 0, nil
}

func nearly(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

// TestQualityArm_AggregatesEndToEnd drives the FULL
// generate-judge-aggregate path — real QualityArmPrompt, real
// StripBodyEnvelope, real RubricJudge decode — through deterministic fakes
// returning known VARYING scores, and asserts the shipped arithmetic: the
// per-sample three-dimension mean, the per-fixture sample mean, the
// UNWEIGHTED overall fixture mean, the signed delta, and the Regressed
// verdict on BOTH sides of the threshold.
//
// The fixtures are declared inline rather than loaded from the committed
// corpus so that adding a corpus fixture cannot silently change the
// expected arithmetic; TestEnvelopeQualityArms_DifferInBodyFraming and
// TestLoadEnvelopeQualityCorpus_FailsClosed cover the committed corpus.
func TestQualityArm_AggregatesEndToEnd(t *testing.T) {
	cases := []NamedEnvelopeQualityCase{
		{Name: "f1", Case: EnvelopeQualityCase{Name: "f1", Body: "## One\n\nDo the first thing.\n", ExpectationNote: "n"}},
		{Name: "f2", Case: EnvelopeQualityCase{Name: "f2", Body: "## Two\n\nDo the second thing.\n", ExpectationNote: "n"}},
		{Name: "f3", Case: EnvelopeQualityCase{Name: "f3", Body: "## Three\n\nDo the third thing.\n", ExpectationNote: "n"}},
	}
	const samples = 2

	// Envelope arm, in fixture-then-sample order:
	//   f1: (5,4,3)->4, (4,4,4)->4   => 4
	//   f2: (5,5,5)->5, (3,3,3)->3   => 4
	//   f3: (5,5,5)->5, (5,5,5)->5   => 5
	//   overall (unweighted over fixtures) = (4+4+5)/3 = 13/3
	envJudge := &scriptedJudgeSender{triples: [][3]int{{5, 4, 3}, {4, 4, 4}, {5, 5, 5}, {3, 3, 3}, {5, 5, 5}, {5, 5, 5}}}
	envGen := &constGenerator{}
	env, err := RunQualityArm(context.Background(), envGen, NewRubricJudge(envJudge, "", 0), cases, ArmEnvelope, samples)
	if err != nil {
		t.Fatalf("envelope arm: %v", err)
	}

	// No-envelope arm:
	//   f1: (3,3,3)->3, (3,3,3)->3   => 3
	//   f2: (4,4,4)->4, (2,2,2)->2   => 3
	//   f3: (3,2,4)->3, (3,3,3)->3   => 3
	//   overall = 3
	noEnvJudge := &scriptedJudgeSender{triples: [][3]int{{3, 3, 3}, {3, 3, 3}, {4, 4, 4}, {2, 2, 2}, {3, 2, 4}, {3, 3, 3}}}
	noEnvGen := &constGenerator{}
	noEnv, err := RunQualityArm(context.Background(), noEnvGen, NewRubricJudge(noEnvJudge, "", 0), cases, ArmNoEnvelope, samples)
	if err != nil {
		t.Fatalf("no-envelope arm: %v", err)
	}

	// Every scripted card was consumed: the loop really ran
	// len(cases)*samples times per arm rather than short-circuiting.
	if envJudge.calls != len(cases)*samples || noEnvJudge.calls != len(cases)*samples {
		t.Fatalf("judge calls = %d / %d, want %d each", envJudge.calls, noEnvJudge.calls, len(cases)*samples)
	}

	// The two arms really sent DIFFERENT prompts (the treatment).
	if len(envGen.seen) != len(noEnvGen.seen) || len(envGen.seen) != len(cases)*samples {
		t.Fatalf("generator calls = %d / %d, want %d each", len(envGen.seen), len(noEnvGen.seen), len(cases)*samples)
	}
	if envGen.seen[0] == noEnvGen.seen[0] {
		t.Fatal("both arms sent the same prompt; the treatment was not applied")
	}
	if strings.Contains(noEnvGen.seen[0], bodyEnvelopeFraming) {
		t.Error("the no-envelope arm's generator prompt still carries the framing paragraph")
	}

	for name, want := range map[string]float64{"f1": 4, "f2": 4, "f3": 5} {
		if !nearly(env.PerFixture[name], want) {
			t.Errorf("envelope PerFixture[%s] = %v, want %v", name, env.PerFixture[name], want)
		}
	}
	for name, want := range map[string]float64{"f1": 3, "f2": 3, "f3": 3} {
		if !nearly(noEnv.PerFixture[name], want) {
			t.Errorf("no-envelope PerFixture[%s] = %v, want %v", name, noEnv.PerFixture[name], want)
		}
	}
	if !nearly(env.Overall, 13.0/3.0) {
		t.Errorf("envelope Overall = %v, want %v (unweighted mean over fixtures)", env.Overall, 13.0/3.0)
	}
	if !nearly(noEnv.Overall, 3) {
		t.Errorf("no-envelope Overall = %v, want 3", noEnv.Overall)
	}
	if env.Samples != samples || noEnv.Samples != samples {
		t.Errorf("Samples = %d / %d, want %d", env.Samples, noEnv.Samples, samples)
	}

	// Signed delta: envelope minus no-envelope. Positive here, so NOT a
	// regression at the default threshold.
	d, err := CompareQualityArms(env, noEnv, DefaultQualityRegressionThreshold)
	if err != nil {
		t.Fatalf("compare arms: %v", err)
	}
	if !nearly(d.Overall, 13.0/3.0-3.0) {
		t.Errorf("delta Overall = %v, want %v", d.Overall, 13.0/3.0-3.0)
	}
	if !nearly(d.PerFixture["f3"], 2) {
		t.Errorf("delta PerFixture[f3] = %v, want 2", d.PerFixture["f3"])
	}
	if d.Regressed {
		t.Error("a positive delta must not be reported as a regression")
	}

	// Swap the arms: the same magnitude with the opposite sign IS a
	// regression at the default threshold.
	rev, err := CompareQualityArms(noEnv, env, DefaultQualityRegressionThreshold)
	if err != nil {
		t.Fatalf("compare arms (swapped): %v", err)
	}
	if !rev.Regressed {
		t.Errorf("delta %v must be reported as a regression at threshold %v", rev.Overall, DefaultQualityRegressionThreshold)
	}
}

// TestCompareQualityArms_ThresholdBoundary pins the shipped -0.25 default
// on BOTH sides plus the boundary itself (Regressed is a strict <).
func TestCompareQualityArms_ThresholdBoundary(t *testing.T) {
	if DefaultQualityRegressionThreshold != -0.25 {
		t.Fatalf("DefaultQualityRegressionThreshold = %v, want -0.25", DefaultQualityRegressionThreshold)
	}
	if DefaultQualitySamples != 5 {
		t.Fatalf("DefaultQualitySamples = %d, want 5", DefaultQualitySamples)
	}
	arm := func(v float64) QualityArmReport {
		return QualityArmReport{PerFixture: map[string]float64{"f": v}, Overall: v}
	}
	for _, tc := range []struct {
		name          string
		env, noEnv    float64
		wantRegressed bool
	}{
		{"above threshold", 3.8, 4.0, false},       // delta -0.20
		{"exactly at threshold", 3.75, 4.0, false}, // delta -0.25, strict <
		{"below threshold", 3.7, 4.0, true},        // delta -0.30
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d, err := CompareQualityArms(arm(tc.env), arm(tc.noEnv), DefaultQualityRegressionThreshold)
			if err != nil {
				t.Fatalf("compare arms: %v", err)
			}
			if d.Regressed != tc.wantRegressed {
				t.Fatalf("delta %v: Regressed = %v, want %v", d.Overall, d.Regressed, tc.wantRegressed)
			}
		})
	}
}

// TestCompareQualityArms_FixtureMismatchFailsClosed: a fixture name present
// in one arm and absent from the other is an ERROR naming that fixture, in
// BOTH directions. Without the guard the absent side indexes to 0.0 and the
// delta is computed against a score no judge produced — which through the
// overall mean can manufacture a regression or mask a real one.
//
// The mismatched pair is deliberately self-consistent in every OTHER
// respect (same threshold, same Overall arithmetic), so the RED under a
// deleted guard lands on the mismatch itself and not on an unrelated
// difference.
func TestCompareQualityArms_FixtureMismatchFailsClosed(t *testing.T) {
	both := QualityArmReport{PerFixture: map[string]float64{"f1": 4, "f2": 4}, Overall: 4}
	one := QualityArmReport{PerFixture: map[string]float64{"f1": 4}, Overall: 4}

	for _, tc := range []struct {
		name             string
		env, noEnv       QualityArmReport
		wantErrSubstring string
	}{
		{"missing from the no-envelope arm", both, one, `fixture "f2" is present in the envelope arm but absent from the no-envelope arm`},
		{"missing from the envelope arm", one, both, `fixture "f2" is present in the no-envelope arm but absent from the envelope arm`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d, err := CompareQualityArms(tc.env, tc.noEnv, DefaultQualityRegressionThreshold)
			if err == nil {
				t.Fatalf("a fixture-name mismatch must fail closed; got delta %+v and no error", d)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstring) {
				t.Fatalf("error %q must contain %q", err.Error(), tc.wantErrSubstring)
			}
			// Fail-closed means NO usable delta escapes alongside the error.
			if len(d.PerFixture) != 0 {
				t.Fatalf("a failed comparison must return the zero QualityDelta; got %+v", d)
			}
		})
	}

	// Control: the SAME fixture set on both sides still compares cleanly, so
	// the guard rejects the mismatch and not comparison in general.
	if _, err := CompareQualityArms(both, both, DefaultQualityRegressionThreshold); err != nil {
		t.Fatalf("aligned arms must compare without error; got %v", err)
	}
}

// TestRunQualityArm_FailsClosed: each guard in the arm driver returns an
// error rather than a partial (silently biased) mean.
func TestRunQualityArm_FailsClosed(t *testing.T) {
	cases := []NamedEnvelopeQualityCase{{Name: "f1", Case: EnvelopeQualityCase{Name: "f1", Body: "b", ExpectationNote: "n"}}}
	gen := &constGenerator{}
	judge := NewRubricJudge(&scriptedJudgeSender{triples: [][3]int{{4, 4, 4}}}, "", 0)

	t.Run("no_fixtures", func(t *testing.T) {
		if _, err := RunQualityArm(context.Background(), gen, judge, nil, ArmEnvelope, 1); err == nil {
			t.Fatal("want error for an empty fixture set")
		}
	})
	t.Run("zero_samples", func(t *testing.T) {
		if _, err := RunQualityArm(context.Background(), gen, judge, cases, ArmEnvelope, 0); err == nil {
			t.Fatal("want error for samples < 1")
		}
	})
	t.Run("nil_seams", func(t *testing.T) {
		if _, err := RunQualityArm(context.Background(), nil, judge, cases, ArmEnvelope, 1); err == nil {
			t.Fatal("want error for a nil sender")
		}
		if _, err := RunQualityArm(context.Background(), gen, nil, cases, ArmEnvelope, 1); err == nil {
			t.Fatal("want error for a nil judge")
		}
	})
	t.Run("unknown_arm", func(t *testing.T) {
		if _, err := RunQualityArm(context.Background(), gen, judge, cases, "sideways", 1); err == nil {
			t.Fatal("want error for an unknown arm")
		}
	})
	t.Run("judge_error_aborts", func(t *testing.T) {
		exhausted := NewRubricJudge(&scriptedJudgeSender{}, "", 0)
		if _, err := RunQualityArm(context.Background(), gen, exhausted, cases, ArmEnvelope, 1); err == nil {
			t.Fatal("want error when the judge fails; a partial arm is a biased mean")
		}
	})
}

// TestMeanRubricScore_MissingDimensionIsAnError: a card missing a scored
// dimension must ERROR, never contribute a silent zero that would drag the
// arm mean down and manufacture a regression.
func TestMeanRubricScore_MissingDimensionIsAnError(t *testing.T) {
	card := RubricCard{Scores: map[string]DimensionScore{"requirement_coverage": {Score: 4}}}
	if _, err := meanRubricScore(card, qualityDimensions); err == nil {
		t.Fatal("want error for a card missing a dimension")
	}
	full := RubricCard{Scores: map[string]DimensionScore{
		"requirement_coverage": {Score: 4}, "structural_fidelity": {Score: 5}, "actionability": {Score: 3},
	}}
	got, err := meanRubricScore(full, qualityDimensions)
	if err != nil {
		t.Fatalf("meanRubricScore: %v", err)
	}
	if !nearly(got, 4) {
		t.Errorf("mean = %v, want 4", got)
	}
}
