package agenteval

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
)

// calibrationSender answers the architect arm PER CASE (the rendered persona
// prompt identifies the case, committedPersonaScript): a catch case's first
// hits[case] calls get its first catching example, a clean case's first
// hits[case] calls get its first false-alarm example, and every later call a
// concern-free verdict. So each case's seeded count exists BY CONSTRUCTION.
type calibrationSender struct {
	script map[string]personaScriptCase
	hits   map[string]int
	calls  map[string]int
	err    error
}

func newCalibrationSender(t *testing.T, hits map[string]int) *calibrationSender {
	t.Helper()
	return &calibrationSender{script: committedPersonaScript(t), hits: hits, calls: map[string]int{}}
}

func (s *calibrationSender) Messages(_ context.Context, _, userText string) (string, string, int, int, int, int, error) {
	if s.err != nil {
		return "", "", 0, 0, 0, 0, s.err
	}
	sc, ok := s.script[userText]
	if !ok {
		return "", "", 0, 0, 0, 0, errors.New("calibration sender: a prompt matching no committed persona case")
	}
	n := s.calls[sc.name]
	s.calls[sc.name]++
	if n < s.hits[sc.name] {
		if sc.clean {
			return sc.falseAlarm, "fake-model", 0, 0, 0, 0, nil
		}
		return sc.catching, "fake-model", 0, 0, 0, 0, nil
	}
	return `{"verdict":"approve","concerns":[]}`, "fake-model", 0, 0, 0, 0, nil
}

func TestCalibrationBandFor_InclusiveBounds(t *testing.T) {
	for _, tc := range []struct {
		caught, trials int
		want           CalibrationBand
	}{
		{0, 10, CalibrationBelow},
		{2, 10, CalibrationBelow},
		{29, 100, CalibrationBelow},
		{3, 10, CalibrationInBand}, // exactly 30%: inclusive
		{30, 100, CalibrationInBand},
		{5, 10, CalibrationInBand},
		{8, 10, CalibrationInBand}, // exactly 80%: inclusive
		{80, 100, CalibrationInBand},
		{81, 100, CalibrationAbove},
		{9, 10, CalibrationAbove},
		{10, 10, CalibrationAbove},
	} {
		if got := calibrationBandFor(tc.caught, tc.trials); got != tc.want {
			t.Errorf("calibrationBandFor(%d, %d) = %q, want %q", tc.caught, tc.trials, got, tc.want)
		}
	}
}

// TestCalibrate_BandVerdicts drives the REAL architect arm (committed persona
// corpus, real docs, production persona builder) with a per-case scripted
// sender: below 30%, exactly 30%, exactly 80% and above 80% on the three catch
// cases across two runs, and a clean case's false-alarm rate with band n/a.
func TestCalibrate_BandVerdicts(t *testing.T) {
	const (
		admission  = "adr006-production-migrate-down"
		superseded = "adr091-merge-without-review-coverage"
		undeclared = "undeclared-cross-component-direction"
		cleanSQLC  = "clean-sqlc-query-cli"
	)
	for _, run := range []struct {
		name string
		hits map[string]int
		want map[string]CalibrationBand
	}{
		{
			name: "below, lower bound, above",
			hits: map[string]int{admission: 2, superseded: 3, undeclared: 9, cleanSQLC: 1},
			want: map[string]CalibrationBand{admission: CalibrationBelow, superseded: CalibrationInBand, undeclared: CalibrationAbove},
		},
		{
			name: "upper bound and mid band",
			hits: map[string]int{admission: 8, superseded: 5, undeclared: 3, cleanSQLC: 1},
			want: map[string]CalibrationBand{admission: CalibrationInBand, superseded: CalibrationInBand, undeclared: CalibrationInBand},
		},
	} {
		t.Run(run.name, func(t *testing.T) {
			cal, err := CalibrateCatchRateArm(context.Background(), newCalibrationSender(t, run.hits), committedCatchInputs(), ArmArchitect, 10)
			if err != nil {
				t.Fatalf("CalibrateCatchRateArm: %v", err)
			}
			if cal.Arm != ArmArchitect || cal.Kind != CatchRateArmKindPersona || cal.SamplesPerCase != 10 {
				t.Errorf("calibration header = %q/%q/%d, want architect/persona/10", cal.Arm, cal.Kind, cal.SamplesPerCase)
			}
			if len(cal.Cases) != 6 {
				t.Fatalf("calibrated %d cases, want the 6 committed architect cases", len(cal.Cases))
			}
			var wantOOB []string
			for _, cc := range cal.Cases {
				if cc.Trials != 10 {
					t.Errorf("case %s trials = %d, want 10", cc.Case, cc.Trials)
				}
				if cc.Expect == CatchExpectClean {
					if cc.Band != CalibrationNotApplicable {
						t.Errorf("clean case %s band = %q, want n/a", cc.Case, cc.Band)
					}
					if cc.Caught != 0 || cc.FalseAlarms != run.hits[cc.Case] {
						t.Errorf("clean case %s caught/false alarms = %d/%d, want 0/%d", cc.Case, cc.Caught, cc.FalseAlarms, run.hits[cc.Case])
					}
					if want := float64(run.hits[cc.Case]) / 10; cc.Rate != want {
						t.Errorf("clean case %s false-alarm rate = %v, want %v", cc.Case, cc.Rate, want)
					}
					continue
				}
				want, ok := run.want[cc.Case]
				if !ok {
					t.Errorf("unexpected catch case %s", cc.Case)
					continue
				}
				if cc.Caught != run.hits[cc.Case] || cc.FalseAlarms != 0 {
					t.Errorf("catch case %s caught/false alarms = %d/%d, want %d/0", cc.Case, cc.Caught, cc.FalseAlarms, run.hits[cc.Case])
				}
				if cc.Band != want {
					t.Errorf("catch case %s at %d/10 band = %q, want %q", cc.Case, cc.Caught, cc.Band, want)
				}
				if want != CalibrationInBand {
					wantOOB = append(wantOOB, cc.Case)
				}
			}
			if got := cal.OutOfBand(); strings.Join(got, ",") != strings.Join(wantOOB, ",") {
				t.Errorf("OutOfBand() = %v, want %v (clean cases never count)", got, wantOOB)
			}
			md := cal.Markdown()
			for _, want := range []string{"| Case | Expect |", "| " + cleanSQLC + " | clean | 10 | - | 1 | 0 | 10.0% false alarm | n/a |"} {
				if !strings.Contains(md, want) {
					t.Errorf("Markdown() lacks %q:\n%s", want, md)
				}
			}
			for name, band := range run.want {
				row := "| " + name + " | catch | 10 | "
				if !strings.Contains(md, row) || !strings.Contains(md, string(band)) {
					t.Errorf("Markdown() lacks the %s row with band %q:\n%s", name, band, md)
				}
			}
			if len(wantOOB) > 0 && !strings.Contains(md, "Out of band: "+strings.Join(wantOOB, ", ")) {
				t.Errorf("Markdown() does not name the out-of-band cases %v:\n%s", wantOOB, md)
			}
			if len(wantOOB) == 0 && !strings.Contains(md, "Every catch case is in band.") {
				t.Errorf("Markdown() does not state that every catch case is in band:\n%s", md)
			}
		})
	}
}

// TestCalibrate_CleanUndecodableCountsAsFalseAlarm pins the fail-safe
// direction on a clean case: an undecodable verdict raises its false-alarm
// rate.
func TestCalibrate_CleanUndecodableCountsAsFalseAlarm(t *testing.T) {
	row := catchRateArmRow{name: "x", kind: CatchRateArmKindPersona, cases: []catchRateArmCase{{name: "c", expect: CatchExpectClean}}}
	cal, err := calibrationFromReport(row, CatchRateArmReport{PerCase: map[string]CaseCatchCounts{
		"c": {Expect: CatchExpectClean, Trials: 10, FalseAlarms: 1, Undecodable: 2},
	}}, 10)
	if err != nil {
		t.Fatalf("calibrationFromReport: %v", err)
	}
	if got := cal.Cases[0].Rate; got != 0.3 {
		t.Errorf("clean false-alarm rate = %v, want 0.3 ((1 + 2) / 10)", got)
	}
}

// TestCalibrate_AnyArmInTheTable calibrates a CONVENTIONS arm the same way:
// every committed miss-corpus case is a catch case, all caught → ABOVE.
func TestCalibrate_AnyArmInTheTable(t *testing.T) {
	cal, err := CalibrateCatchRateArm(context.Background(), newScriptedCatchSender(t, math.MaxInt, 0), committedCatchInputs(), ArmWithoutConventions, 1)
	if err != nil {
		t.Fatalf("CalibrateCatchRateArm: %v", err)
	}
	cases, _ := loadCommittedCatch(t)
	if cal.Kind != CatchRateArmKindConventions || len(cal.Cases) != len(cases) {
		t.Fatalf("conventions calibration = %s with %d cases, want conventions with %d", cal.Kind, len(cal.Cases), len(cases))
	}
	for _, cc := range cal.Cases {
		if cc.Expect != CatchExpectCatch || cc.Band != CalibrationAbove {
			t.Errorf("case %s = %s/%q, want catch/ABOVE", cc.Case, cc.Expect, cc.Band)
		}
	}
}

// TestCalibrate_NeverWrites reads the committed evidence pair back after a
// calibration run: byte-identical, modification time unchanged.
func TestCalibrate_NeverWrites(t *testing.T) {
	paths := []string{planReviewCatchEvidencePath, CatchRateTrialsPath(planReviewCatchEvidencePath)}
	before := make(map[string][]byte, len(paths))
	stamps := make(map[string]int64, len(paths))
	for _, p := range paths {
		before[p] = mustReadCatch(t, p)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		stamps[p] = fi.ModTime().UnixNano()
	}
	if _, err := CalibrateCatchRateArm(context.Background(), newCalibrationSender(t, nil), committedCatchInputs(), ArmArchitect, 1); err != nil {
		t.Fatalf("CalibrateCatchRateArm: %v", err)
	}
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.ModTime().UnixNano() != stamps[p] || !bytes.Equal(mustReadCatch(t, p), before[p]) {
			t.Errorf("calibration modified %s", p)
		}
	}
}

// TestCalibrate_FailsClosed: each refusal returns a named error and the zero
// calibration. The samples and sender rows pair the bad argument with an
// UNAVAILABLE corpus, so the refusal must come from the argument check (run
// before any input is read), not from the corpus load.
func TestCalibrate_FailsClosed(t *testing.T) {
	badInputs := committedCatchInputs()
	badInputs.CorpusDir = t.TempDir() + "/absent"
	transport := &calibrationSender{err: errors.New("connection reset")}
	for _, tc := range []struct {
		name    string
		sender  MessageSender
		in      CatchRateInputs
		arm     string
		samples int
		want    string
	}{
		{"unknown arm", newCalibrationSender(t, nil), committedCatchInputs(), "security", 1, `no such arm in the arm table (known: architect, with_conventions, without_conventions)`},
		{"samples zero", newCalibrationSender(t, nil), badInputs, ArmArchitect, 0, "samples must be >= 1, got 0"},
		{"samples negative", newCalibrationSender(t, nil), badInputs, ArmArchitect, -1, "samples must be >= 1, got -1"},
		{"nil sender", nil, badInputs, ArmArchitect, 1, "sender is required"},
		{"unavailable input", newCalibrationSender(t, nil), badInputs, ArmArchitect, 1, "corpus unavailable"},
		{"transport error", transport, committedCatchInputs(), ArmArchitect, 1, "connection reset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cal, err := CalibrateCatchRateArm(context.Background(), tc.sender, tc.in, tc.arm, tc.samples)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "calibrating arm") {
				t.Errorf("err = %v, want it to name the calibration", err)
			}
			if cal.Arm != "" || cal.Cases != nil {
				t.Errorf("calibration = %+v, want the zero value", cal)
			}
		})
	}
}

// TestCalibrate_RefusesMissingCase: a report with no trials for a row case is
// refused, never scored.
func TestCalibrate_RefusesMissingCase(t *testing.T) {
	row := catchRateArmRow{name: "x", kind: CatchRateArmKindPersona, cases: []catchRateArmCase{{name: "a", expect: CatchExpectCatch}, {name: "b", expect: CatchExpectCatch}}}
	for name, perCase := range map[string]map[string]CaseCatchCounts{
		"absent":     {"a": {Trials: 10, Caught: 5}},
		"zero trial": {"a": {Trials: 10, Caught: 5}, "b": {}},
	} {
		t.Run(name, func(t *testing.T) {
			cal, err := calibrationFromReport(row, CatchRateArmReport{PerCase: perCase}, 10)
			if err == nil || !strings.Contains(err.Error(), `no trials for case "b"`) {
				t.Fatalf("err = %v, want the missing case named", err)
			}
			if cal.Cases != nil {
				t.Errorf("calibration = %+v, want the zero value", cal)
			}
		})
	}
}
