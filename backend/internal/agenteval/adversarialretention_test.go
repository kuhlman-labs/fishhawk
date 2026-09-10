package agenteval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// adversarialRetentionCorpusDir is the committed corpus.
const adversarialRetentionCorpusDir = "testdata/adversarial-retention-corpus"

// ---------------------------------------------------------------------------
// This file's OWN fakes.
//
// The existing injection / envelope-quality / judge fakes live in test files
// this run does not own, so extending one would be a scope drift. These two
// are declared here and used only by this file.
// ---------------------------------------------------------------------------

// fakeRetentionSender replays a fixed queue of generator responses.
type fakeRetentionSender struct {
	responses []string
	err       error
	calls     int
}

func (f *fakeRetentionSender) Messages(_ context.Context, _, _ string) (string, string, int, int, int, int, error) {
	if f.err != nil {
		return "", "", 0, 0, 0, 0, f.err
	}
	if f.calls >= len(f.responses) {
		return "", "", 0, 0, 0, 0, fmt.Errorf("fakeRetentionSender: no response queued for call %d", f.calls+1)
	}
	r := f.responses[f.calls]
	f.calls++
	return r, "fake-model", 0, 0, 0, 0, nil
}

// fakeRetentionJudge returns a fixed card (or error) for every rubric.
type fakeRetentionJudge struct {
	card  RubricCard
	err   error
	calls int
}

func (f *fakeRetentionJudge) JudgeRubric(_ context.Context, _ Rubric, _ string) (RubricCard, error) {
	f.calls++
	if f.err != nil {
		return RubricCard{}, f.err
	}
	return f.card, nil
}

// ---------------------------------------------------------------------------
// Fixture helpers.
// ---------------------------------------------------------------------------

func validRetentionCase() RetentionCase {
	return RetentionCase{
		Name:          "synthetic",
		FindingClass:  "read-mirror-gates-mutations",
		Diff:          "--- a/x.go\n+++ b/x.go\n@@\n-old\n+new\n",
		PlanSummary:   "a summary",
		ScopeFiles:    []string{"x.go"},
		FindingProbes: []string{"projection"},
		BehavioralRubric: &BehavioralRubric{
			CompliantBehavior: "the review names the read/write source split",
			ResistantBehavior: "the review never mentions it",
			Dimensions:        []string{RetentionDeciderDimension, "named_the_mechanism"},
		},
		ExpectationNote: "the guard reads a mirror while the mutation writes the source",
		Synthetic:       true,
	}
}

func writeRetentionCase(t *testing.T, dir, name string, body []byte) {
	t.Helper()
	caseDir := filepath.Join(dir, name)
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(caseDir, "case.json"), body, 0o644); err != nil {
		t.Fatalf("write case.json: %v", err)
	}
}

func writeValidRetentionCase(t *testing.T, dir, name string, mutate func(*RetentionCase)) {
	t.Helper()
	c := validRetentionCase()
	if mutate != nil {
		mutate(&c)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	writeRetentionCase(t, dir, name, raw)
}

// ---------------------------------------------------------------------------
// Loader: one test per named fail-closed mode (a)-(h).
// ---------------------------------------------------------------------------

func TestLoadAdversarialRetentionCorpus_FailClosedModes(t *testing.T) {
	tests := []struct {
		mode  string
		want  string
		setup func(t *testing.T, dir string)
	}{
		{
			mode: "(a) unreadable case.json",
			want: "read case.json",
			setup: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "no-json"), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			},
		},
		{
			mode: "(b) malformed JSON",
			want: "parse case.json",
			setup: func(t *testing.T, dir string) {
				writeRetentionCase(t, dir, "bad-json", []byte("{not json"))
			},
		},
		{
			mode: "(c) empty diff",
			want: "diff must be non-empty",
			setup: func(t *testing.T, dir string) {
				writeValidRetentionCase(t, dir, "empty-diff", func(c *RetentionCase) { c.Diff = "   \n" })
			},
		},
		{
			mode: "(d) empty finding_probes",
			want: "finding_probes must be non-empty",
			setup: func(t *testing.T, dir string) {
				writeValidRetentionCase(t, dir, "no-probes", func(c *RetentionCase) { c.FindingProbes = nil })
			},
		},
		{
			mode: "(d) blank probe",
			want: "finding_probes[1] must be non-empty",
			setup: func(t *testing.T, dir string) {
				writeValidRetentionCase(t, dir, "blank-probe", func(c *RetentionCase) {
					c.FindingProbes = []string{"projection", "  "}
				})
			},
		},
		{
			mode: "(e) absent behavioral_rubric",
			want: "behavioral_rubric is required",
			setup: func(t *testing.T, dir string) {
				writeValidRetentionCase(t, dir, "no-rubric", func(c *RetentionCase) { c.BehavioralRubric = nil })
			},
		},
		{
			mode: "(e) rubric missing the decider dimension",
			want: "must declare the decider dimension \"produced_the_named_finding\"",
			setup: func(t *testing.T, dir string) {
				writeValidRetentionCase(t, dir, "no-decider", func(c *RetentionCase) {
					// Declares the INJECTION decider, not this corpus's.
					c.BehavioralRubric.Dimensions = []string{DeciderDimension, "named_the_mechanism"}
				})
			},
		},
		{
			mode: "(f) empty compliant_behavior",
			want: "compliant_behavior must be non-empty",
			setup: func(t *testing.T, dir string) {
				writeValidRetentionCase(t, dir, "no-compliant", func(c *RetentionCase) {
					c.BehavioralRubric.CompliantBehavior = " "
				})
			},
		},
		{
			mode: "(f) empty resistant_behavior",
			want: "resistant_behavior must be non-empty",
			setup: func(t *testing.T, dir string) {
				writeValidRetentionCase(t, dir, "no-resistant", func(c *RetentionCase) {
					c.BehavioralRubric.ResistantBehavior = ""
				})
			},
		},
		{
			mode: "(f) zero dimensions",
			want: "dimensions must be non-empty",
			setup: func(t *testing.T, dir string) {
				writeValidRetentionCase(t, dir, "no-dims", func(c *RetentionCase) {
					c.BehavioralRubric.Dimensions = nil
				})
			},
		},
		{
			mode: "(g) empty expectation_note",
			want: "expectation_note must be non-empty",
			setup: func(t *testing.T, dir string) {
				writeValidRetentionCase(t, dir, "no-note", func(c *RetentionCase) { c.ExpectationNote = "\t" })
			},
		},
		{
			mode: "(h) unknown finding_class",
			want: "finding_class \"time-travel\" is not one of",
			setup: func(t *testing.T, dir string) {
				writeValidRetentionCase(t, dir, "bad-class", func(c *RetentionCase) { c.FindingClass = "time-travel" })
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.mode, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			_, err := LoadAdversarialRetentionCorpus(dir)
			if err == nil {
				t.Fatalf("mode %s: expected an error, got nil", tc.mode)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("mode %s: error %q does not name the mode (want substring %q)", tc.mode, err, tc.want)
			}
		})
	}
}

// TestLoadAdversarialRetentionCorpus_AbsentDirIsAnError is mode (a) at the
// DIRECTORY level: an absent committed corpus means the gate is silently not
// running, so it must not degrade to an empty slice.
func TestLoadAdversarialRetentionCorpus_AbsentDirIsAnError(t *testing.T) {
	_, err := LoadAdversarialRetentionCorpus(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected an error for an absent corpus dir, got nil (a silently empty corpus is the fail-open this loader exists to close)")
	}
	if !strings.Contains(err.Error(), "read adversarial-retention corpus dir") {
		t.Fatalf("error %q does not name the absent corpus dir", err)
	}
}

// TestLoadAdversarialRetentionCorpus_RubricMissingDecider is the named
// counterfactual vehicle for the LOADER-side half of the decider guard.
// RetentionVerdict carries the verdict-side half
// (TestRetentionVerdict_MissingDeciderIsIndeterminate), so neither guard can
// be removed silently.
func TestLoadAdversarialRetentionCorpus_RubricMissingDecider(t *testing.T) {
	dir := t.TempDir()
	writeValidRetentionCase(t, dir, "no-decider", func(c *RetentionCase) {
		c.BehavioralRubric.Dimensions = []string{"named_the_mechanism", "assessed_reachability"}
	})
	_, err := LoadAdversarialRetentionCorpus(dir)
	if err == nil {
		t.Fatal("expected a rubric without the decider dimension to be refused at load, got nil")
	}
	if !strings.Contains(err.Error(), RetentionDeciderDimension) {
		t.Fatalf("error %q does not name the decider dimension", err)
	}
}

func TestLoadAdversarialRetentionCorpus_ValidCaseLoads(t *testing.T) {
	dir := t.TempDir()
	writeValidRetentionCase(t, dir, "good", nil)
	got, err := LoadAdversarialRetentionCorpus(dir)
	if err != nil {
		t.Fatalf("valid case did not load: %v", err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("got %+v, want one case named \"good\"", got)
	}
}

// ---------------------------------------------------------------------------
// The committed corpus.
// ---------------------------------------------------------------------------

// TestRetentionCorpus_CoversAllFourNamedClasses pins the enumeration against
// the committed fixtures in BOTH directions: deleting a fixture or renaming a
// class goes red.
func TestRetentionCorpus_CoversAllFourNamedClasses(t *testing.T) {
	cases, err := LoadAdversarialRetentionCorpus(adversarialRetentionCorpusDir)
	if err != nil {
		t.Fatalf("load committed corpus: %v", err)
	}
	seen := map[string]string{}
	for _, c := range cases {
		if prev, dup := seen[c.Case.FindingClass]; dup {
			t.Fatalf("finding class %q is covered by two fixtures (%s and %s); one fixture per class",
				c.Case.FindingClass, prev, c.Name)
		}
		seen[c.Case.FindingClass] = c.Name
		if !c.Case.Synthetic {
			t.Fatalf("fixture %q must set synthetic:true — every committed fixture is hand-authored, not distilled", c.Name)
		}
	}
	for _, want := range RetentionFindingClasses {
		if _, ok := seen[want]; !ok {
			t.Errorf("finding class %q has no committed fixture", want)
		}
	}
	if len(seen) != len(RetentionFindingClasses) {
		t.Errorf("corpus covers %d classes, RetentionFindingClasses declares %d", len(seen), len(RetentionFindingClasses))
	}
}

// TestRetentionCorpus_ExercisesPriorConcernsPosture asserts the COMMITTED
// corpus populates the `len(t.PriorConcerns) > 0` guard the guard-conditional
// fifth treatment literal renders under. Without a fixture that does, the
// posture-aware strip would only ever be exercised by hand-built triggers.
func TestRetentionCorpus_ExercisesPriorConcernsPosture(t *testing.T) {
	cases, err := LoadAdversarialRetentionCorpus(adversarialRetentionCorpusDir)
	if err != nil {
		t.Fatalf("load committed corpus: %v", err)
	}
	for _, c := range cases {
		if len(c.Case.PriorConcerns) > 0 {
			return
		}
	}
	t.Fatal("no committed retention fixture carries prior_concerns, so the guard-conditional fifth treatment literal is never exercised by the shipped corpus")
}

// TestRetentionArms_DifferOnlyInCalibrationWording drives the REAL prompt
// builder over every committed fixture and asserts the pre arm is the post arm
// minus the five #2119 treatment literals — nothing else.
func TestRetentionArms_DifferOnlyInCalibrationWording(t *testing.T) {
	cases, err := LoadAdversarialRetentionCorpus(adversarialRetentionCorpusDir)
	if err != nil {
		t.Fatalf("load committed corpus: %v", err)
	}
	for _, nc := range cases {
		t.Run(nc.Name, func(t *testing.T) {
			post, err := RetentionArmPrompt(nc.Case, ArmPostCalibration)
			if err != nil {
				t.Fatalf("post arm: %v", err)
			}
			pre, err := RetentionArmPrompt(nc.Case, ArmPreCalibration)
			if err != nil {
				t.Fatalf("pre arm: %v", err)
			}
			if pre == post {
				t.Fatal("the pre arm is byte-identical to the post arm; nothing was stripped")
			}
			if len(pre) >= len(post) {
				t.Fatalf("the pre arm (%d bytes) is not shorter than the post arm (%d bytes)", len(pre), len(post))
			}
			// The fixture's own diff must survive into BOTH arms: the strip
			// must remove treatment wording, not review material.
			if !strings.Contains(pre, "@@") || !strings.Contains(post, "@@") {
				t.Fatal("a fixture's diff did not survive into both arms")
			}
		})
	}
}

func TestRetentionArmPrompt_UnknownArm(t *testing.T) {
	_, err := RetentionArmPrompt(validRetentionCase(), "sideways")
	if err == nil {
		t.Fatal("expected an unknown arm to be refused, got nil")
	}
	if !strings.Contains(err.Error(), "unknown calibration arm") {
		t.Fatalf("error %q does not name the unknown arm", err)
	}
}

func TestRetentionRubric_StatesFixtureTermsVerbatim(t *testing.T) {
	c := validRetentionCase()
	r, err := RetentionRubric(c)
	if err != nil {
		t.Fatalf("build rubric: %v", err)
	}
	for _, want := range []string{
		c.BehavioralRubric.CompliantBehavior,
		c.BehavioralRubric.ResistantBehavior,
		c.ExpectationNote,
		RetentionDeciderDimension,
	} {
		if !strings.Contains(r.SystemPrompt, want) {
			t.Errorf("rubric system prompt omits %q", want)
		}
	}
	if len(r.Dimensions) != len(c.BehavioralRubric.Dimensions) {
		t.Errorf("rubric declares %d dimensions, fixture declares %d", len(r.Dimensions), len(c.BehavioralRubric.Dimensions))
	}
}

func TestRetentionRubric_RefusesDeciderlessCase(t *testing.T) {
	c := validRetentionCase()
	c.BehavioralRubric.Dimensions = []string{"named_the_mechanism"}
	if _, err := RetentionRubric(c); err == nil {
		t.Fatal("expected RetentionRubric to refuse a case whose rubric omits the decider dimension, got nil")
	}
}

// ---------------------------------------------------------------------------
// The three-state verdict.
// ---------------------------------------------------------------------------

// TestRetentionVerdict_IndeterminateIsNotAPass pins the whole point of the
// third state: an unmatched probe with no judged evidence must NOT resolve to
// either decided state.
func TestRetentionVerdict_IndeterminateIsNotAPass(t *testing.T) {
	c := validRetentionCase()
	state, basis := RetentionVerdict(false, RubricCard{}, c)
	if state != FindingIndeterminate {
		t.Fatalf("state = %q, want %q — an unmatched probe with no judged decider is not evidence either way", state, FindingIndeterminate)
	}
	if state == FindingProduced || state == FindingAbsent {
		t.Fatal("indeterminate must never be reported as a decided state")
	}
	if !strings.Contains(basis, "Not a pass") {
		t.Fatalf("basis %q does not state that indeterminate is not a pass", basis)
	}
}

// TestRetentionVerdict_MissingDeciderIsIndeterminate is the VERDICT-side half
// of the decider guard: a card lacking the decider must not index to score 0
// and report the finding ABSENT — that would fabricate a #2119 regression.
func TestRetentionVerdict_MissingDeciderIsIndeterminate(t *testing.T) {
	c := validRetentionCase()
	card := RubricCard{Scores: map[string]DimensionScore{
		"named_the_mechanism": {Score: 1, Rationale: "no"},
	}}
	state, basis := RetentionVerdict(false, card, c)
	if state != FindingAbsent && state != FindingProduced && state != FindingIndeterminate {
		t.Fatalf("unexpected state %q", state)
	}
	if state != FindingIndeterminate {
		t.Fatalf("state = %q, want %q — a card carrying scores but NOT the decider must not be read as a zero score", state, FindingIndeterminate)
	}
	if !strings.Contains(basis, RetentionDeciderDimension) {
		t.Fatalf("basis %q does not name the missing decider dimension", basis)
	}
}

func TestRetentionVerdict_DecidedStates(t *testing.T) {
	c := validRetentionCase()
	tests := []struct {
		name         string
		probeMatched bool
		score        int
		want         RetentionVerdictState
	}{
		{"probe match is sufficient", true, 0, FindingProduced},
		{"decider at the midpoint is produced", false, retentionProducedScoreThreshold, FindingProduced},
		{"decider above the midpoint is produced", false, scoreMax, FindingProduced},
		{"decider below the midpoint is absent", false, retentionProducedScoreThreshold - 1, FindingAbsent},
		{"decider at the floor is absent", false, scoreMin, FindingAbsent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			card := RubricCard{Scores: map[string]DimensionScore{
				RetentionDeciderDimension: {Score: tc.score, Rationale: "r"},
			}}
			got, _ := RetentionVerdict(tc.probeMatched, card, c)
			if got != tc.want {
				t.Fatalf("state = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRetentionVerdict_NilRubricIsIndeterminate(t *testing.T) {
	c := validRetentionCase()
	c.BehavioralRubric = nil

	t.Run("empty card", func(t *testing.T) {
		state, _ := RetentionVerdict(false, RubricCard{}, c)
		if state != FindingIndeterminate {
			t.Fatalf("state = %q, want %q for a rubricless case", state, FindingIndeterminate)
		}
	})

	// A card that DOES carry the decider is what makes the nil-rubric guard
	// load-bearing rather than redundant: both decided branches dereference
	// c.BehavioralRubric to build the basis string, so without the guard this
	// input reaches a nil dereference.
	t.Run("card carrying the decider", func(t *testing.T) {
		for _, score := range []int{scoreMax, scoreMin} {
			card := RubricCard{Scores: map[string]DimensionScore{
				RetentionDeciderDimension: {Score: score, Rationale: "r"},
			}}
			state, basis := RetentionVerdict(false, card, c)
			if state != FindingIndeterminate {
				t.Fatalf("decider score %d: state = %q, want %q — a case with no rubric has no declared behaviour to judge against", score, state, FindingIndeterminate)
			}
			if !strings.Contains(basis, "Not a pass") {
				t.Fatalf("decider score %d: basis %q does not state that indeterminate is not a pass", score, basis)
			}
		}
	})
}

func TestMatchFindingProbe(t *testing.T) {
	c := validRetentionCase()
	c.FindingProbes = []string{"Projection", "stale"}
	if p, ok := MatchFindingProbe(c, []string{"the guard reads a PROJECTION"}); !ok || p != "Projection" {
		t.Fatalf("probe = %q ok = %t, want a case-insensitive match on %q", p, ok, "Projection")
	}
	if _, ok := MatchFindingProbe(c, []string{"unrelated naming nit"}); ok {
		t.Fatal("expected no probe match on an unrelated note")
	}
	if _, ok := MatchFindingProbe(c, nil); ok {
		t.Fatal("expected no probe match against zero notes")
	}
}

// ---------------------------------------------------------------------------
// The report.
// ---------------------------------------------------------------------------

// TestRetentionReport_CountsThreeStatesSeparately pins that indeterminate has
// its OWN column in both the counts and the rendered header.
func TestRetentionReport_CountsThreeStatesSeparately(t *testing.T) {
	var r RetentionReport
	r.Arm = ArmPostCalibration
	r.Add(RetentionResult{Case: "a", State: FindingProduced})
	r.Add(RetentionResult{Case: "b", State: FindingAbsent})
	r.Add(RetentionResult{Case: "c", State: FindingIndeterminate})
	r.Add(RetentionResult{Case: "d", State: FindingIndeterminate})
	if r.Produced != 1 || r.Absent != 1 || r.Indeterminate != 2 {
		t.Fatalf("produced=%d absent=%d indeterminate=%d, want 1/1/2", r.Produced, r.Absent, r.Indeterminate)
	}
	out := r.Render()
	if !strings.Contains(out, "indeterminate is NOT a pass") {
		t.Fatalf("rendered header does not say indeterminate is not a pass:\n%s", out)
	}
	if !strings.Contains(out, "indeterminate=2") {
		t.Fatalf("rendered header does not carry a separate indeterminate count:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// The comparison.
// ---------------------------------------------------------------------------

func retentionArm(arm string, states map[string]RetentionVerdictState) RetentionReport {
	r := RetentionReport{Arm: arm}
	names := make([]string, 0, len(states))
	for n := range states {
		names = append(names, n)
	}
	// Deterministic order so Render output is stable.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, n := range names {
		r.Add(RetentionResult{Case: n, Class: "read-mirror-gates-mutations", State: states[n]})
	}
	return r
}

// TestCompareRetentionArms_RegressionDetected is done-means 3's loss
// condition: produced in the pre arm, absent in the post arm.
func TestCompareRetentionArms_RegressionDetected(t *testing.T) {
	pre := retentionArm(ArmPreCalibration, map[string]RetentionVerdictState{
		"lost":     FindingProduced,
		"kept":     FindingProduced,
		"gained":   FindingAbsent,
		"neither":  FindingAbsent,
		"unjudged": FindingProduced,
	})
	post := retentionArm(ArmPostCalibration, map[string]RetentionVerdictState{
		"lost":     FindingAbsent,
		"kept":     FindingProduced,
		"gained":   FindingProduced,
		"neither":  FindingAbsent,
		"unjudged": FindingIndeterminate,
	})
	got, err := CompareRetentionArms(pre, post)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(got.Regressed) != 1 || got.Regressed[0] != "lost" {
		t.Fatalf("regressed = %v, want exactly [lost]", got.Regressed)
	}
	if got.Retained != 1 {
		t.Errorf("retained = %d, want 1", got.Retained)
	}
	if got.Gained != 1 {
		t.Errorf("gained = %d, want 1", got.Gained)
	}
	if len(got.Undecidable) != 1 || got.Undecidable[0] != "unjudged" {
		t.Errorf("undecidable = %v, want exactly [unjudged]", got.Undecidable)
	}
	out := got.Render()
	if !strings.Contains(out, "regressed=1") || !strings.Contains(out, "lost") {
		t.Fatalf("rendered comparison does not lead with the regressed set:\n%s", out)
	}
}

// TestCompareRetentionArms_IndeterminatePreArmIsNotARegression: an
// indeterminate pre arm cannot establish that anything was there to lose, so
// the pair must land in the undecidable column, never in the regressed set.
func TestCompareRetentionArms_IndeterminatePreArmIsNotARegression(t *testing.T) {
	pre := retentionArm(ArmPreCalibration, map[string]RetentionVerdictState{"x": FindingIndeterminate})
	post := retentionArm(ArmPostCalibration, map[string]RetentionVerdictState{"x": FindingAbsent})
	got, err := CompareRetentionArms(pre, post)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(got.Regressed) != 0 {
		t.Fatalf("regressed = %v, want empty — an indeterminate pre arm establishes nothing to lose", got.Regressed)
	}
	if len(got.Undecidable) != 1 {
		t.Fatalf("undecidable = %v, want the pair counted apart", got.Undecidable)
	}
}

// TestCompareRetentionArms_FailsClosedOnCaseMismatch asserts BOTH directions:
// a post arm silently missing the one case that regressed would otherwise
// report a clean comparison.
func TestCompareRetentionArms_FailsClosedOnCaseMismatch(t *testing.T) {
	t.Run("case in pre only", func(t *testing.T) {
		pre := retentionArm(ArmPreCalibration, map[string]RetentionVerdictState{
			"shared": FindingProduced, "pre-only": FindingProduced,
		})
		post := retentionArm(ArmPostCalibration, map[string]RetentionVerdictState{"shared": FindingProduced})
		_, err := CompareRetentionArms(pre, post)
		if err == nil {
			t.Fatal("expected a case present only in the pre arm to fail closed, got nil")
		}
		if !strings.Contains(err.Error(), "pre-only") || !strings.Contains(err.Error(), "absent from the post-calibration arm") {
			t.Fatalf("error %q does not name the missing case and direction", err)
		}
	})
	t.Run("case in post only", func(t *testing.T) {
		pre := retentionArm(ArmPreCalibration, map[string]RetentionVerdictState{"shared": FindingProduced})
		post := retentionArm(ArmPostCalibration, map[string]RetentionVerdictState{
			"shared": FindingProduced, "post-only": FindingProduced,
		})
		_, err := CompareRetentionArms(pre, post)
		if err == nil {
			t.Fatal("expected a case present only in the post arm to fail closed, got nil")
		}
		if !strings.Contains(err.Error(), "post-only") || !strings.Contains(err.Error(), "absent from the pre-calibration arm") {
			t.Fatalf("error %q does not name the missing case and direction", err)
		}
	})
}

// TestCompareRetentionArms_DuplicateCaseInOneArmFailsClosed: a case reported
// twice would let the last write decide the comparison silently.
func TestCompareRetentionArms_DuplicateCaseInOneArmFailsClosed(t *testing.T) {
	pre := RetentionReport{Arm: ArmPreCalibration}
	pre.Add(RetentionResult{Case: "dup", State: FindingProduced})
	pre.Add(RetentionResult{Case: "dup", State: FindingAbsent})
	post := retentionArm(ArmPostCalibration, map[string]RetentionVerdictState{"dup": FindingAbsent})
	if _, err := CompareRetentionArms(pre, post); err == nil {
		t.Fatal("expected a duplicated case name in one arm to fail closed, got nil")
	}
}

// TestCompareRetentionArms_OmissionCannotHideARegression is the retention
// analogue of the omission-monotonicity property: the only ways a produced
// pre-arm finding leaves the regressed set are the post arm producing it or
// an indeterminate pair (counted apart) — dropping the case outright is
// REFUSED rather than silently favourable.
func TestCompareRetentionArms_OmissionCannotHideARegression(t *testing.T) {
	pre := retentionArm(ArmPreCalibration, map[string]RetentionVerdictState{
		"regressing": FindingProduced, "clean": FindingProduced,
	})
	full := retentionArm(ArmPostCalibration, map[string]RetentionVerdictState{
		"regressing": FindingAbsent, "clean": FindingProduced,
	})
	got, err := CompareRetentionArms(pre, full)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(got.Regressed) != 1 {
		t.Fatalf("regressed = %v, want the regression reported", got.Regressed)
	}
	// Now the post arm simply OMITS the case it regressed on. The comparison
	// must refuse rather than report a clean run.
	omitting := retentionArm(ArmPostCalibration, map[string]RetentionVerdictState{"clean": FindingProduced})
	got2, err := CompareRetentionArms(pre, omitting)
	if err == nil {
		t.Fatalf("omitting the regressed case reported regressed=%v with no error; an omission must never produce a cleaner comparison", got2.Regressed)
	}
	if !strings.Contains(err.Error(), "regressing") {
		t.Fatalf("error %q does not name the omitted case", err)
	}
}

// ---------------------------------------------------------------------------
// Running an arm.
// ---------------------------------------------------------------------------

func TestRunRetentionArm_ProbeMatchShortCircuitsTheJudge(t *testing.T) {
	cases := []NamedRetentionCase{{Name: "c1", Case: validRetentionCase()}}
	sender := &fakeRetentionSender{responses: []string{
		`{"verdict":"changes_requested","concerns":[{"severity":"high","category":"correctness","note":"the guard reads the projection while the mutation writes the store"}]}`,
	}}
	judge := &fakeRetentionJudge{}
	got, err := RunRetentionArm(context.Background(), sender, judge, cases, ArmPostCalibration)
	if err != nil {
		t.Fatalf("run arm: %v", err)
	}
	if got.Produced != 1 || got.Absent != 0 || got.Indeterminate != 0 {
		t.Fatalf("produced=%d absent=%d indeterminate=%d, want 1/0/0", got.Produced, got.Absent, got.Indeterminate)
	}
	if judge.calls != 0 {
		t.Fatalf("judge was called %d times; a matched probe is already sufficient evidence", judge.calls)
	}
}

func TestRunRetentionArm_UnmatchedProbeConsultsTheJudge(t *testing.T) {
	cases := []NamedRetentionCase{{Name: "c1", Case: validRetentionCase()}}
	sender := &fakeRetentionSender{responses: []string{
		`{"verdict":"approve","concerns":[{"severity":"low","category":"style","note":"naming nit only"}]}`,
	}}
	judge := &fakeRetentionJudge{card: RubricCard{Scores: map[string]DimensionScore{
		RetentionDeciderDimension: {Score: 1, Rationale: "the review never raised it"},
	}}}
	got, err := RunRetentionArm(context.Background(), sender, judge, cases, ArmPostCalibration)
	if err != nil {
		t.Fatalf("run arm: %v", err)
	}
	if judge.calls != 1 {
		t.Fatalf("judge calls = %d, want 1", judge.calls)
	}
	if got.Absent != 1 {
		t.Fatalf("absent = %d, want 1 (a judged decider below the midpoint)", got.Absent)
	}
}

// TestRunRetentionArm_NilJudgeIsIndeterminateNotAbsent: without a judge there
// is no substantive evidence, so an unmatched probe must not be promoted to a
// decided ABSENT — that would fabricate a #2119 regression.
func TestRunRetentionArm_NilJudgeIsIndeterminateNotAbsent(t *testing.T) {
	cases := []NamedRetentionCase{{Name: "c1", Case: validRetentionCase()}}
	sender := &fakeRetentionSender{responses: []string{
		`{"verdict":"approve","concerns":[{"severity":"low","category":"style","note":"naming nit only"}]}`,
	}}
	got, err := RunRetentionArm(context.Background(), sender, nil, cases, ArmPostCalibration)
	if err != nil {
		t.Fatalf("run arm: %v", err)
	}
	if got.Indeterminate != 1 || got.Absent != 0 {
		t.Fatalf("indeterminate=%d absent=%d, want 1/0", got.Indeterminate, got.Absent)
	}
}

// TestRunRetentionArm_FailsClosed covers every abort path: a partial arm is a
// silently-biased comparison, and an undecodable verdict read as
// zero-concerns would fabricate a retention failure.
func TestRunRetentionArm_FailsClosed(t *testing.T) {
	valid := []NamedRetentionCase{{Name: "c1", Case: validRetentionCase()}}
	tests := []struct {
		name   string
		cases  []NamedRetentionCase
		sender MessageSender
		judge  RubricJudge
		arm    string
		want   string
	}{
		{"no fixtures", nil, &fakeRetentionSender{}, nil, ArmPostCalibration, "no fixtures"},
		{"nil sender", valid, nil, nil, ArmPostCalibration, "sender is required"},
		{"unknown arm", valid, &fakeRetentionSender{}, nil, "sideways", "unknown calibration arm"},
		{"generate error", valid, &fakeRetentionSender{err: fmt.Errorf("boom")}, nil, ArmPostCalibration, "generate"},
		{"undecodable verdict", valid, &fakeRetentionSender{responses: []string{"not json at all"}}, nil, ArmPostCalibration, "decode verdict"},
		{
			"judge error",
			valid,
			&fakeRetentionSender{responses: []string{`{"verdict":"approve","concerns":[]}`}},
			&fakeRetentionJudge{err: fmt.Errorf("judge down")},
			ArmPostCalibration,
			"judge",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RunRetentionArm(context.Background(), tc.sender, tc.judge, tc.cases, tc.arm)
			if err == nil {
				t.Fatalf("expected an error, got nil (report %+v)", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the failure (want substring %q)", err, tc.want)
			}
			if len(got.Results) != 0 {
				t.Fatalf("a failed arm returned %d partial results; a partial arm is a silently-biased comparison", len(got.Results))
			}
		})
	}
}
