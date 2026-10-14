package agenteval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures: every sidecar below is RECORDED (recordCyclingSidecar /
// freshCatchEvidence: RecordCatchRateEvidence with a scripted sender against
// the committed corpus) and then perturbed, so a re-score result is about the
// perturbation, not the sidecar's shape.

const committedCatchEvidence = "testdata/planreview-catchrate/evidence.json"

func rescoreOf(t *testing.T, evidencePath string) CatchRateRescore {
	t.Helper()
	r, err := RescoreCatchRateTrials(CatchRateTrialsPath(evidencePath), committedCatchInputs())
	if err != nil {
		t.Fatalf("RescoreCatchRateTrials: %v", err)
	}
	return r
}

func rescoreArmOf(t *testing.T, r CatchRateRescore, arm string) CatchRateRescoreArm {
	t.Helper()
	for _, a := range r.Arms {
		if a.Arm == arm {
			return a
		}
	}
	t.Fatalf("re-score has no arm %q", arm)
	return CatchRateRescoreArm{}
}

func rescoreCaseOf(t *testing.T, r CatchRateRescore, arm, name string) CaseCatchRescore {
	t.Helper()
	for _, c := range rescoreArmOf(t, r, arm).PerCase {
		if c.Case == name {
			return c
		}
	}
	t.Fatalf("re-score arm %q has no case %q", arm, name)
	return CaseCatchRescore{}
}

// committedCatchCase is a committed case by name.
func committedCatchCase(t *testing.T, name string) PlanReviewCatchCase {
	t.Helper()
	cases, _ := loadCommittedCatch(t)
	for _, c := range cases {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no committed case %q", name)
	return PlanReviewCatchCase{}
}

// firstTrialWith is the index of the first trial of list with outcome o.
func firstTrialWith(t *testing.T, list []CatchRateTrialRecord, o CatchOutcome) int {
	t.Helper()
	for i, tr := range list {
		if tr.Outcome == o {
			return i
		}
	}
	t.Fatalf("no %s trial in the fixture case", o)
	return -1
}

// catchingStoredConcern is one stored concern naming the case's planted id and
// its first catch probe: a catch under the current rule.
func catchingStoredConcern(t *testing.T, name string) CatchRateTrialConcern {
	t.Helper()
	c := committedCatchCase(t, name)
	return CatchRateTrialConcern{Severity: "medium", Category: "acceptance_criteria",
		Note: c.CatchRule().CriterionIDs[0] + " is defective: " + c.Input.CatchProbes[0]}
}

// TestRescoreCatchRateTrials_IdempotentOnCurrentRule: a sidecar recorded
// under the current rule re-scores to exactly its recorded outcomes — per
// trial, per case and per arm — with the stored near-miss tags and the
// uncapped concerns_total reproduced, in arm-table order.
func TestRescoreCatchRateTrials_IdempotentOnCurrentRule(t *testing.T) {
	path, rec, _ := recordCyclingSidecar(t)
	stored := readCatchTrials(t, path)
	r := rescoreOf(t, path)
	if len(r.Arms) != 3 || r.Arms[0].Arm != ArmWithoutConventions || r.Arms[1].Arm != ArmWithConventions || r.Arms[2].Arm != ArmArchitect {
		t.Fatalf("arms = %+v, want the arm table order [%s %s %s]", r.Arms, ArmWithoutConventions, ArmWithConventions, ArmArchitect)
	}
	if r.RecordedAt != rec.RecordedAt || r.PromptFingerprint != rec.PromptFingerprint || !strings.HasPrefix(catchRuleVersion, r.CatchRule+":") {
		t.Errorf("re-score envelope = %q / %q / %q; want the sidecar's recorded_at and fingerprint and the catch rule label", r.RecordedAt, r.PromptFingerprint, r.CatchRule)
	}
	for _, a := range []struct {
		arm    string
		counts map[string]CaseCatchCounts
		trials map[string][]CatchRateTrialRecord
	}{
		{ArmWithoutConventions, rec.Arms[ArmWithoutConventions].PerCase, stored.Arms[ArmWithoutConventions].PerCase},
		{ArmWithConventions, rec.Arms[ArmWithConventions].PerCase, stored.Arms[ArmWithConventions].PerCase},
		{ArmArchitect, rec.Arms[ArmArchitect].PerCase, stored.Arms[ArmArchitect].PerCase},
	} {
		arm := rescoreArmOf(t, r, a.arm)
		if len(arm.Unscored) != 0 || len(arm.NotRecorded) != 0 || len(arm.PerCase) != len(a.counts) {
			t.Fatalf("arm %q: unscored %v, not recorded %v, %d scored cases; want every one of the %d cases scored", a.arm, arm.Unscored, arm.NotRecorded, len(arm.PerCase), len(a.counts))
		}
		for _, c := range arm.PerCase {
			want := a.counts[c.Case]
			if c.Trials != want.Trials || c.RecordedCaught != want.Caught || c.RecordedFalseAlarms != want.FalseAlarms || c.Undecodable != want.Undecodable {
				t.Errorf("arm %q case %q: recorded %+v, evidence %+v", a.arm, c.Case, c.CatchRescoreCounts, want)
			}
			if c.RescoredCaught != c.RecordedCaught || c.RescoredFalseAlarms != c.RecordedFalseAlarms || c.Gained != 0 || c.Lost != 0 {
				t.Errorf("arm %q case %q: re-scored %d vs recorded %d, +%d/-%d; want identical", a.arm, c.Case, c.RescoredCaught, c.RecordedCaught, c.Gained, c.Lost)
			}
			for i, rt := range c.TrialRecords {
				st := a.trials[c.Case][i]
				if rt.Trial != st.Trial || rt.Recorded != st.Outcome || rt.Rescored.Outcome != st.Outcome || rt.Rescored.ConcernsTotal != st.ConcernsTotal {
					t.Errorf("arm %q case %q trial %d: re-scored %+v, stored %+v", a.arm, c.Case, st.Trial, rt, st)
					continue
				}
				if st.Outcome == CatchUndecodable {
					continue
				}
				if rt.Rescored.MatchedProbe != st.MatchedProbe || rt.Rescored.Attribution != st.Attribution || len(rt.Rescored.Concerns) != len(st.Concerns) {
					t.Errorf("arm %q case %q trial %d: re-scored match %q/%q with %d concerns, stored %q/%q with %d",
						a.arm, c.Case, st.Trial, rt.Rescored.MatchedProbe, rt.Rescored.Attribution, len(rt.Rescored.Concerns), st.MatchedProbe, st.Attribution, len(st.Concerns))
					continue
				}
				for j, cc := range rt.Rescored.Concerns {
					if cc.NearMiss != st.Concerns[j].NearMiss {
						t.Errorf("arm %q case %q trial %d concern %d: near_miss %q, stored %q", a.arm, c.Case, st.Trial, j, cc.NearMiss, st.Concerns[j].NearMiss)
					}
				}
			}
		}
	}
}

// TestRescoreCatchRateTrials_GainsAMissUnderTheCurrentRule: a stored MISSED
// trial whose concern now satisfies both halves re-scores as caught. The
// recorded outcome is "missed", so a re-score that carried it would report
// gained 0.
func TestRescoreCatchRateTrials_GainsAMissUnderTheCurrentRule(t *testing.T) {
	path, _, _ := recordCyclingSidecar(t)
	tr := readCatchTrials(t, path)
	name := sortedTrialCaseNames(tr.Arms[ArmWithoutConventions].PerCase)[0]
	list := tr.Arms[ArmWithoutConventions].PerCase[name]
	i := firstTrialWith(t, list, CatchMissed)
	list[i].Concerns = []CatchRateTrialConcern{catchingStoredConcern(t, name)}
	list[i].ConcernsTotal = 1
	writeCatchTrials(t, path, tr)

	r := rescoreOf(t, path)
	c := rescoreCaseOf(t, r, ArmWithoutConventions, name)
	if c.Gained != 1 || c.Lost != 0 || c.RescoredCaught != c.RecordedCaught+1 {
		t.Fatalf("case %q: recorded %d, re-scored %d, +%d/-%d; want one gained catch", name, c.RecordedCaught, c.RescoredCaught, c.Gained, c.Lost)
	}
	got := c.TrialRecords[i]
	if got.Recorded != CatchMissed || got.Rescored.Outcome != CatchCaught || got.Rescored.Attribution != CatchAttributionID ||
		got.Rescored.MatchedProbe != committedCatchCase(t, name).Input.CatchProbes[0] {
		t.Errorf("gained trial = %+v", got)
	}
	if w := rescoreCaseOf(t, r, ArmWithConventions, name); w.Gained != 0 || w.Lost != 0 {
		t.Errorf("the untouched with arm changed: +%d/-%d", w.Gained, w.Lost)
	}
}

// TestRescoreCatchRateTrials_LosesACatchTheRuleNoLongerHolds: a stored CAUGHT
// trial whose concern now matches neither half re-scores as missed.
func TestRescoreCatchRateTrials_LosesACatchTheRuleNoLongerHolds(t *testing.T) {
	path, _, _ := recordCyclingSidecar(t)
	tr := readCatchTrials(t, path)
	name := sortedTrialCaseNames(tr.Arms[ArmWithConventions].PerCase)[0]
	list := tr.Arms[ArmWithConventions].PerCase[name]
	i := firstTrialWith(t, list, CatchCaught)
	list[i].Concerns = []CatchRateTrialConcern{{Severity: "low", Category: "observability", Note: "the handler logs nothing on success"}}
	writeCatchTrials(t, path, tr)

	c := rescoreCaseOf(t, rescoreOf(t, path), ArmWithConventions, name)
	if c.Lost != 1 || c.Gained != 0 || c.RescoredCaught != c.RecordedCaught-1 {
		t.Fatalf("case %q: recorded %d, re-scored %d, +%d/-%d; want one lost catch", name, c.RecordedCaught, c.RescoredCaught, c.Gained, c.Lost)
	}
	if got := c.TrialRecords[i]; got.Recorded != CatchCaught || got.Rescored.Outcome != CatchMissed || got.Rescored.MatchedProbe != "" {
		t.Errorf("lost trial = %+v", got)
	}
}

// TestRescoreCatchRateTrials_UndecodableStaysUndecodable: an undecodable
// trial's response_excerpt is never re-parsed, even when it holds a verdict
// whose concern would catch.
func TestRescoreCatchRateTrials_UndecodableStaysUndecodable(t *testing.T) {
	path, _, _ := recordCyclingSidecar(t)
	tr := readCatchTrials(t, path)
	name := sortedTrialCaseNames(tr.Arms[ArmWithoutConventions].PerCase)[0]
	list := tr.Arms[ArmWithoutConventions].PerCase[name]
	i := firstTrialWith(t, list, CatchUndecodable)
	concern := catchingStoredConcern(t, name)
	excerpt := catchVerdictJSON(t, CatchExampleConcern{Severity: concern.Severity, Category: concern.Category, Note: concern.Note})
	if got := ClassifyCatch(excerpt, committedCatchCase(t, name).CatchRule()); got != CatchCaught {
		t.Fatalf("premise: the planted excerpt classifies %s when parsed, want caught", got)
	}
	list[i].ResponseExcerpt = excerpt
	writeCatchTrials(t, path, tr)

	c := rescoreCaseOf(t, rescoreOf(t, path), ArmWithoutConventions, name)
	if got := c.TrialRecords[i]; got.Rescored.Outcome != CatchUndecodable || got.Rescored.Response != excerpt || got.Attributed {
		t.Fatalf("undecodable trial re-scored as %+v; want undecodable, unparsed, unattributed", got)
	}
	if c.RescoredCaught != c.RecordedCaught || c.Gained != 0 {
		t.Errorf("case %q: re-scored %d vs recorded %d, gained %d; the excerpt was scored", name, c.RescoredCaught, c.RecordedCaught, c.Gained)
	}
}

// TestRescoreCatchRateTrials_CountsLowerBoundTrials: a re-scored miss whose
// stored record is incomplete — concerns past the cap, or a note or category
// cut at note_max_runes — is counted; a complete miss and a catch are not.
func TestRescoreCatchRateTrials_CountsLowerBoundTrials(t *testing.T) {
	path, _, _ := recordCyclingSidecar(t)
	base := readCatchTrials(t, path)
	name := sortedTrialCaseNames(base.Arms[ArmWithoutConventions].PerCase)[0]
	// The recorded missed trials store 12 of 18 concerns, one of them a
	// truncated wide-rune note: make each one COMPLETE (its first two stored
	// concerns, neither truncated, concerns_total 2) so each row below is
	// incomplete in exactly one way.
	for _, list := range [][]CatchRateTrialRecord{base.Arms[ArmWithoutConventions].PerCase[name]} {
		for i := range list {
			if list[i].Outcome == CatchMissed {
				list[i].Concerns = append([]CatchRateTrialConcern(nil), list[i].Concerns[:2]...)
				list[i].ConcernsTotal = 2
			}
		}
	}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		outcome CatchOutcome
		mutate  func(*CatchRateTrialRecord)
		want    int
	}{
		{"complete miss", CatchMissed, func(*CatchRateTrialRecord) {}, 0},
		{"concerns_total exceeds the stored concerns", CatchMissed, func(r *CatchRateTrialRecord) { r.ConcernsTotal = len(r.Concerns) + 1 }, 1},
		{"note ends in the truncation marker", CatchMissed, func(r *CatchRateTrialRecord) { r.Concerns[0].Note += catchTrialTruncationMarker }, 1},
		{"category ends in the truncation marker", CatchMissed, func(r *CatchRateTrialRecord) { r.Concerns[0].Category += catchTrialTruncationMarker }, 1},
		{"a catch is never lower-bound", CatchCaught, func(r *CatchRateTrialRecord) {
			r.ConcernsTotal = 9
			r.Concerns[0].Category += catchTrialTruncationMarker
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tr CatchRateTrials
			if err := json.Unmarshal(raw, &tr); err != nil {
				t.Fatal(err)
			}
			list := tr.Arms[ArmWithoutConventions].PerCase[name]
			i := firstTrialWith(t, list, tc.outcome)
			tc.mutate(&list[i])
			writeCatchTrials(t, path, tr)
			c := rescoreCaseOf(t, rescoreOf(t, path), ArmWithoutConventions, name)
			if c.LowerBound != tc.want || c.TrialRecords[i].LowerBound != (tc.want == 1) {
				t.Fatalf("lower-bound trials = %d (trial %d flagged %v), want %d", c.LowerBound, list[i].Trial, c.TrialRecords[i].LowerBound, tc.want)
			}
			if c.RescoredCaught != c.RecordedCaught {
				t.Errorf("premise: the mutation changed an outcome (recorded %d, re-scored %d)", c.RecordedCaught, c.RescoredCaught)
			}
		})
	}
}

// TestRescoreCatchRateTrials_AttributionOnlyRate: a decodable trial counts
// toward the attribution-only rate when SOME stored concern names the planted
// criterion, whatever its probe; an undecodable trial never does.
func TestRescoreCatchRateTrials_AttributionOnlyRate(t *testing.T) {
	path, _, _ := recordCyclingSidecar(t)
	tr := readCatchTrials(t, path)
	name := sortedTrialCaseNames(tr.Arms[ArmWithoutConventions].PerCase)[0]
	list := tr.Arms[ArmWithoutConventions].PerCase[name]
	i := firstTrialWith(t, list, CatchMissed)
	list[i].Concerns = []CatchRateTrialConcern{{Severity: "low", Category: "scope", Note: "filler concern"}}
	writeCatchTrials(t, path, tr)

	r := rescoreOf(t, path)
	for _, arm := range []string{ArmWithoutConventions, ArmWithConventions} {
		for _, c := range rescoreArmOf(t, r, arm).PerCase {
			// Every recorded missed trial carries an attribution-without-probe
			// concern naming every planted id, so each decodable trial is
			// attributed — except the one rewritten above.
			want := c.Trials - c.Undecodable
			if arm == ArmWithoutConventions && c.Case == name {
				want--
			}
			if c.Attributed != want || c.Attributed <= c.RescoredCaught {
				t.Errorf("arm %q case %q: attributed %d, want %d (caught %d, undecodable %d of %d)", arm, c.Case, c.Attributed, want, c.RescoredCaught, c.Undecodable, c.Trials)
			}
		}
	}
	if rescoreCaseOf(t, r, ArmWithoutConventions, name).TrialRecords[i].Attributed {
		t.Error("the rewritten trial, whose only concern names no planted criterion, is attributed")
	}
}

// TestRescoreCatchRateTrials_ReportsUnscoredAndUnrecordedCases: a sidecar
// case absent from the current corpus and a corpus case absent from the
// sidecar are reported, never fatal.
func TestRescoreCatchRateTrials_ReportsUnscoredAndUnrecordedCases(t *testing.T) {
	const unknown = "seed-synthetic-not-in-the-corpus"
	t.Run("one case renamed", func(t *testing.T) {
		path, _, _ := recordCyclingSidecar(t)
		tr := readCatchTrials(t, path)
		orig := lastTrialCase(tr.Arms[ArmWithoutConventions].PerCase)
		for _, m := range []map[string][]CatchRateTrialRecord{tr.Arms[ArmWithoutConventions].PerCase, tr.Arms[ArmWithConventions].PerCase} {
			m[unknown] = m[orig]
			delete(m, orig)
		}
		writeCatchTrials(t, path, tr)
		r := rescoreOf(t, path)
		for _, a := range r.Arms {
			if a.Arm == ArmArchitect {
				continue // the architect arm is untouched by this fixture
			}
			if len(a.Unscored) != 1 || a.Unscored[0] != unknown || len(a.NotRecorded) != 1 || a.NotRecorded[0] != orig {
				t.Errorf("arm %q: unscored %v, not recorded %v; want [%s] and [%s]", a.Arm, a.Unscored, a.NotRecorded, unknown, orig)
			}
			for _, c := range a.PerCase {
				if c.Case == orig || c.Case == unknown {
					t.Errorf("arm %q scored case %q", a.Arm, c.Case)
				}
			}
		}
		out := r.Render()
		for _, want := range []string{"UNSCORED (in the sidecar, not in the current corpus): " + unknown, "NOT RECORDED (in the current corpus, not in the sidecar): " + orig} {
			if strings.Count(out, want) != 2 {
				t.Errorf("Render names %q %d times, want once per arm:\n%s", want, strings.Count(out, want), out)
			}
		}
	})
	t.Run("no case scored", func(t *testing.T) {
		path, _, _ := recordCyclingSidecar(t)
		tr := readCatchTrials(t, path)
		orig := lastTrialCase(tr.Arms[ArmWithoutConventions].PerCase)
		one := map[string][]CatchRateTrialRecord{unknown: tr.Arms[ArmWithoutConventions].PerCase[orig]}
		tr.Arms[ArmWithoutConventions] = CatchRateTrialsArm{PerCase: one}
		tr.Arms[ArmWithConventions] = CatchRateTrialsArm{PerCase: one}
		delete(tr.Arms, ArmArchitect)
		writeCatchTrials(t, path, tr)
		r := rescoreOf(t, path)
		cases, _ := loadCommittedCatch(t)
		if a := rescoreArmOf(t, r, ArmArchitect); a.Recorded || len(a.PerCase) != 0 || len(a.NotRecorded) != len(loadCommittedPersona(t)) {
			t.Errorf("architect arm absent from the sidecar: recorded %v, %d scored, not recorded %v; want NOT RECORDED", a.Recorded, len(a.PerCase), a.NotRecorded)
		}
		if out := r.Render(); !strings.Contains(out, ArmArchitect+": NOT RECORDED (the sidecar carries no such arm)") {
			t.Errorf("Render does not report the architect arm NOT RECORDED:\n%s", out)
		}
		for _, a := range r.Arms {
			if a.Arm == ArmArchitect {
				continue
			}
			if len(a.PerCase) != 0 || len(a.NotRecorded) != len(cases) || a.Pooled().Trials != 0 {
				t.Errorf("arm %q: %d scored, %d not recorded, %d trials; want 0, %d, 0", a.Arm, len(a.PerCase), len(a.NotRecorded), a.Pooled().Trials, len(cases))
			}
		}
		if out := r.Render(); !strings.Contains(out, "0/0 (n/a)") || !strings.Contains(out, "n/a (an arm has no scored trials)") || strings.Contains(out, "AT CEILING") {
			t.Errorf("Render of a sidecar with no scored case:\n%s", out)
		}
	})
}

// cleanPersonaCaseName is a committed clean persona case.
func cleanPersonaCaseName(t *testing.T) string {
	t.Helper()
	for _, c := range loadCommittedPersona(t) {
		if c.Expect() == CatchExpectClean {
			return c.Name
		}
	}
	t.Fatal("the committed persona corpus has no clean case")
	return ""
}

// TestRescoreCatchRateTrials_FailClosed: one row per fail-closed mode, each
// on an otherwise valid recorded sidecar.
func TestRescoreCatchRateTrials_FailClosed(t *testing.T) {
	path, _, raw := recordCyclingSidecar(t)
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mutated := func(name string, f func(*CatchRateTrials)) string {
		tr := readCatchTrials(t, path)
		f(&tr)
		b, err := json.Marshal(tr)
		if err != nil {
			t.Fatal(err)
		}
		return write(name, b)
	}
	var withExtra map[string]any
	if err := json.Unmarshal(raw, &withExtra); err != nil {
		t.Fatal(err)
	}
	withExtra["extra"] = 1
	extra, err := json.Marshal(withExtra)
	if err != nil {
		t.Fatal(err)
	}
	bogus := func(arm CatchRateTrialsArm, name string) {
		list := arm.PerCase[name]
		list[0].Outcome = "bogus"
	}
	for _, tc := range []struct {
		name, trials string
		in           CatchRateInputs
		want         string
	}{
		{"absent sidecar", filepath.Join(dir, "none.trials.json"), committedCatchInputs(), "absent: nothing to re-score"},
		{"unreadable sidecar (a directory)", t.TempDir(), committedCatchInputs(), "read:"},
		{"malformed JSON", write("malformed.json", []byte("{not json")), committedCatchInputs(), "malformed JSON"},
		{"unknown field", write("unknown.json", extra), committedCatchInputs(), `unknown field "extra"`},
		{"trailing content", write("trailing.json", append(append([]byte{}, raw...), "{}\n"...)), committedCatchInputs(), "trailing content"},
		{"wrong schema", mutated("schema.json", func(tr *CatchRateTrials) { tr.Schema = "planreview-catchrate-trials-v0" }), committedCatchInputs(),
			`schema "planreview-catchrate-trials-v0" is not "` + CatchRateTrialsSchema + `"`},
		{"a catch-class outcome on a clean case", mutated("clean-caught.json", func(tr *CatchRateTrials) {
			m := tr.Arms[ArmArchitect].PerCase
			name := cleanPersonaCaseName(t)
			m[name][0].Outcome = CatchCaught
		}), committedCatchInputs(), `outcome "caught" is not one of false_alarm|no_false_alarm|undecodable`},
		{"unknown outcome in a scored case", mutated("bogus.json", func(tr *CatchRateTrials) {
			bogus(tr.Arms[ArmWithConventions], lastTrialCase(tr.Arms[ArmWithConventions].PerCase))
		}), committedCatchInputs(), `outcome "bogus" is not one of caught|missed|undecodable`},
		{"unknown outcome in an unscored case", mutated("bogus-unscored.json", func(tr *CatchRateTrials) {
			m := tr.Arms[ArmWithoutConventions].PerCase
			orig := lastTrialCase(m)
			m["seed-synthetic-not-in-the-corpus"] = m[orig]
			delete(m, orig)
			bogus(tr.Arms[ArmWithoutConventions], "seed-synthetic-not-in-the-corpus")
		}), committedCatchInputs(), `(not in the current corpus): trial 1 outcome "bogus"`},
		{"corpus unavailable", CatchRateTrialsPath(path), catchInputs(filepath.Join(dir, "no-corpus"), committedCatchConventions), "corpus unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := RescoreCatchRateTrials(tc.trials, tc.in)
			if err == nil {
				t.Fatalf("re-scored %+v, want an error naming %q", r.Arms, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.trials) || !strings.Contains(err.Error(), catchRateRunbook) {
				t.Fatalf("err = %v\nwant it to name %q, the sidecar path and the run-book", err, tc.want)
			}
			if len(r.Arms) != 0 {
				t.Errorf("a refused re-score returned arms %+v", r.Arms)
			}
		})
	}
}

// TestRescoreCatchRateTrials_RenderStatesLowerBound: the report leads with
// the diagnostic banner, states the lower bound with the SIDECAR's own caps
// (not the current constants), reports the attribution-only rate and the
// unbounded arm delta, and names every case.
func TestRescoreCatchRateTrials_RenderStatesLowerBound(t *testing.T) {
	path, rec, _ := recordCyclingSidecar(t)
	tr := readCatchTrials(t, path)
	tr.NoteMaxRunes, tr.MaxConcernsPerTrial = 777, 9
	writeCatchTrials(t, path, tr)
	out := rescoreOf(t, path).Render()
	if !strings.HasPrefix(out, "OFFLINE RE-SCORE — a diagnostic, not a gate verdict; it never writes evidence\n") {
		t.Errorf("Render does not lead with the diagnostic banner:\n%s", out)
	}
	for _, want := range []string{
		"lower bound", "note_max_runes (777 runes)", "max_concerns_per_trial (9)", "never over-count",
		"recorded_at: " + rec.RecordedAt, "prompt_fingerprint: " + rec.PromptFingerprint, CatchRateTrialsPath(path),
		"attribution-only", "The arm DELTA is NOT a bound", "IN-SAMPLE", "arm delta (without_conventions - with_conventions)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Render lacks %q:\n%s", want, out)
		}
	}
	for _, name := range sortedTrialCaseNames(tr.Arms[ArmWithoutConventions].PerCase) {
		if !strings.Contains(out, "\n  "+name+": without_conventions ") {
			t.Errorf("Render has no case line for %q:\n%s", name, out)
		}
	}
	if strings.Contains(out, "PASS") || strings.Contains(out, "AT CEILING") {
		t.Errorf("Render of the cycling fixture (re-scored 1/3, attributed 2/3) claims a PASS or a ceiling:\n%s", out)
	}
}

// TestRescoreCatchRateTrials_RenderFlagsTheCeiling: an arm whose re-scored
// rate is within 0.03 of its attribution-only rate is flagged at ceiling
// (#4361); the margin is inclusive and exact.
func TestRescoreCatchRateTrials_RenderFlagsTheCeiling(t *testing.T) {
	out := rescoreOf(t, freshCatchEvidence(t)).Render()
	if n := strings.Count(out, "AT CEILING"); n != 3 {
		t.Errorf("an all-caught sidecar is flagged at ceiling %d times, want once per arm:\n%s", n, out)
	}
	for _, tc := range []struct {
		p    CatchRescoreCounts
		want bool
	}{
		{CatchRescoreCounts{Trials: 100, Attributed: 13, RescoredCaught: 10}, true},
		{CatchRescoreCounts{Trials: 100, Attributed: 14, RescoredCaught: 10}, false},
		{CatchRescoreCounts{Trials: 0}, false},
		{CatchRescoreCounts{Trials: 100, Attributed: 0, RescoredCaught: 0}, false},
		{CatchRescoreCounts{Trials: 200, CleanTrials: 100, Attributed: 13, RescoredCaught: 10}, true},
		{CatchRescoreCounts{Trials: 200, CleanTrials: 100, Attributed: 14, RescoredCaught: 10}, false},
	} {
		if got := atCatchCeiling(tc.p); got != tc.want {
			t.Errorf("atCatchCeiling(%+v) = %v, want %v", tc.p, got, tc.want)
		}
	}
}

// TestRescoreCatchRateTrials_CommittedSidecar is a smoke test over the
// committed pair: the sidecar re-scores against the committed corpus, and its
// recorded tallies equal the committed record's. It asserts no rate, so it
// stays true across a re-recording.
func TestRescoreCatchRateTrials_CommittedSidecar(t *testing.T) {
	r, err := RescoreCatchRateTrials(CatchRateTrialsPath(committedCatchEvidence), committedCatchInputs())
	if err != nil {
		t.Fatalf("the committed sidecar does not re-score: %v", err)
	}
	// A LOOSE decode: the committed pair is v1 until the shared #4313/#4314
	// re-pin recording, v2 after it; both carry arms.<name>.per_case.
	var rec CatchRateEvidence
	if err := json.Unmarshal(mustRead(t, committedCatchEvidence), &rec); err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct {
		arm    string
		counts map[string]CaseCatchCounts
	}{
		{ArmWithoutConventions, rec.Arms[ArmWithoutConventions].PerCase},
		{ArmWithConventions, rec.Arms[ArmWithConventions].PerCase},
	} {
		arm := rescoreArmOf(t, r, a.arm)
		if len(arm.PerCase) == 0 {
			t.Fatalf("arm %q scored no case", a.arm)
		}
		for _, c := range arm.PerCase {
			if want := a.counts[c.Case]; c.Trials != want.Trials || c.RecordedCaught != want.Caught || c.Undecodable != want.Undecodable {
				t.Errorf("arm %q case %q: sidecar tallies %+v, record %+v", a.arm, c.Case, c.CatchRescoreCounts, want)
			}
		}
	}
	if out := r.Render(); !strings.Contains(out, "recorded_at: "+rec.RecordedAt) {
		t.Errorf("Render does not name the committed recording:\n%s", out)
	}
}
