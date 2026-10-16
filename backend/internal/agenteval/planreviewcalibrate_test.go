package agenteval

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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

// treeSnapshot records every directory (as "<dir>/") and regular file (its
// bytes) under root, keyed by root-relative slash path.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			snap[rel+"/"] = ""
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		snap[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// diffTreeSnapshots names every path created, removed or modified between two
// snapshots of the same root.
func diffTreeSnapshots(before, after map[string]string) []string {
	var changed []string
	for p, b := range before {
		a, ok := after[p]
		switch {
		case !ok:
			changed = append(changed, "removed "+p)
		case a != b:
			changed = append(changed, "modified "+p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			changed = append(changed, "created "+p)
		}
	}
	sort.Strings(changed)
	return changed
}

// TestCalibrate_NeverWrites reads the committed evidence pair back after a
// calibration run (byte-identical, modification time unchanged), then
// calibrates EVERY arm against temp copies of the corpus directory and the
// checkout root and asserts neither tree gained, lost or changed a file or
// directory — so a write into an input the committed-evidence check cannot see
// still fails.
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

	in := copyPersonaRepo(t)
	in.CorpusDir = copyCatchCorpus(t)
	roots := map[string]string{"corpus dir": in.CorpusDir, "repo root": in.RepoRoot}
	snaps := make(map[string]map[string]string, len(roots))
	for name, root := range roots {
		snaps[name] = treeSnapshot(t, root)
	}
	rows, err := catchRateArmTable(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		sender := newScriptedCatchSender(t, 0, 0)
		sender.persona = personaScriptFor(t, in.RepoRoot)
		if _, err := CalibrateCatchRateArm(context.Background(), sender, in, row.name, 1); err != nil {
			t.Fatalf("CalibrateCatchRateArm(%s): %v", row.name, err)
		}
	}
	for name, root := range roots {
		if changed := diffTreeSnapshots(snaps[name], treeSnapshot(t, root)); len(changed) > 0 {
			t.Errorf("calibrating every arm changed the %s %s: %s", name, root, strings.Join(changed, ", "))
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
		{"unknown arm", newCalibrationSender(t, nil), committedCatchInputs(), "zz_no_such_arm", 1, `no such arm in the arm table (known: architect, security, security_portability, with_conventions, without_conventions)`},
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

// portabilityMarkerVerdict is a persona verdict whose ONE concern names the
// repository-specific marker gateenv.go and asserts it is absent.
const portabilityMarkerVerdict = `{"verdict":"approve_with_concerns","concerns":[{"severity":"medium","category":"security","note":"The change does not add the new variable to gateenv.go's allow-list."}]}`

// TestCalibrateCatchRateArm_PortabilityFindings (#4314, CF9): the scripted
// sender answers ONE security_portability clean-case call with a concern
// naming the repository-specific marker gateenv.go; the calibration lists
// exactly that finding and the Markdown its block. The same answer on the
// security arm (which IS shown the convention) is not a portability finding.
func TestCalibrateCatchRateArm_PortabilityFindings(t *testing.T) {
	for _, arm := range []string{ArmSecurityPortability, ArmSecurity} {
		t.Run(arm, func(t *testing.T) {
			sender := newScriptedCatchSender(t, 0, 0)
			var hit string
			sender.secOverride = func(a string, sc personaScriptCase, call int) (string, bool) {
				if a == arm && sc.clean && hit == "" {
					hit = sc.name
					return portabilityMarkerVerdict, true
				}
				return "", false
			}
			cal, err := CalibrateCatchRateArm(context.Background(), sender, committedCatchInputs(), arm, 1)
			if err != nil {
				t.Fatalf("CalibrateCatchRateArm: %v", err)
			}
			if arm == ArmSecurity {
				if len(cal.PortabilityFindings) != 0 || strings.Contains(cal.Markdown(), "Portability findings") {
					t.Fatalf("security arm reported portability findings %+v", cal.PortabilityFindings)
				}
				return
			}
			want := []CatchRatePortabilityFinding{{Case: hit, Trial: 1, Marker: "gateenv.go"}}
			if !reflect.DeepEqual(cal.PortabilityFindings, want) {
				t.Fatalf("PortabilityFindings = %+v, want %+v", cal.PortabilityFindings, want)
			}
			md := cal.Markdown()
			if !strings.Contains(md, "Portability findings") || !strings.Contains(md, `"gateenv.go"`) {
				t.Errorf("Markdown lacks the portability block:\n%s", md)
			}
		})
	}
}

// TestCalibrateCatchRateArm_SecurityLensAndAlarmCategories (#4314): the
// security calibration carries each defect case's lens (and a Lens column)
// and each false-alarming clean case's concerns by normalized category (and
// the block); the architect table carries neither.
func TestCalibrateCatchRateArm_SecurityLensAndAlarmCategories(t *testing.T) {
	sender := newScriptedCatchSender(t, 0, 0)
	sender.secOverride = func(a string, sc personaScriptCase, call int) (string, bool) {
		if sc.clean {
			return `{"verdict":"approve_with_concerns","concerns":[{"severity":"low","category":"Correctness","note":"a standard-criteria nit"},{"severity":"medium","category":"secret custody","note":"a security nit"}]}`, true
		}
		return "", false
	}
	cal, err := CalibrateCatchRateArm(context.Background(), sender, committedCatchInputs(), ArmSecurity, 1)
	if err != nil {
		t.Fatalf("CalibrateCatchRateArm: %v", err)
	}
	secCases, _ := loadCommittedSecurity(t)
	lens := map[string]string{}
	for _, c := range secCases {
		lens[c.Name] = c.Lens()
	}
	for _, cc := range cal.Cases {
		if cc.Lens != lens[cc.Case] {
			t.Errorf("case %s lens = %q, want %q", cc.Case, cc.Lens, lens[cc.Case])
		}
		if cc.Expect == CatchExpectClean {
			if want := map[string]int{"correctness": 1, "secret_custody": 1}; !reflect.DeepEqual(cc.AlarmCategories, want) {
				t.Errorf("clean case %s AlarmCategories = %v, want %v", cc.Case, cc.AlarmCategories, want)
			}
		} else if cc.AlarmCategories != nil {
			t.Errorf("catch case %s AlarmCategories = %v, want nil", cc.Case, cc.AlarmCategories)
		}
	}
	md := cal.Markdown()
	for _, want := range []string{"| Case | Expect | Lens | Trials |", "| " + SecurityLensAuthnAuthz + " |", "False alarms by category", "correctness 1, secret_custody 1"} {
		if !strings.Contains(md, want) {
			t.Errorf("security Markdown lacks %q:\n%s", want, md)
		}
	}
	arch, err := CalibrateCatchRateArm(context.Background(), newCalibrationSender(t, nil), committedCatchInputs(), ArmArchitect, 1)
	if err != nil {
		t.Fatal(err)
	}
	if md := arch.Markdown(); strings.Contains(md, "Lens") || strings.Contains(md, "False alarms by category") || strings.Contains(md, "Portability findings") {
		t.Errorf("the architect table carries a security block:\n%s", md)
	}
}
