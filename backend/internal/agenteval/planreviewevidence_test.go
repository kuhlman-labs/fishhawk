package agenteval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

// ---------------------------------------------------------------------------
// Fixtures. Every record below is built BY CONSTRUCTION: RecordCatchRateEvidence
// with a scripted sender against the REAL committed corpus and conventions,
// then (for the check modes) one perturbation. So each record is complete —
// schema, tolerance, min_trials_per_arm, a per_case entry for every committed
// case, a pinned baseline — and a refusal is about the perturbed field, not
// the record's shape.
// ---------------------------------------------------------------------------

// scriptedCatchSender answers the first caught[arm] calls of each arm with a
// verdict whose ONE concern names every committed case's planted criterion
// id(s) and first probe — so it satisfies both halves of every case's catch
// rule — and every later call with a concern-free verdict.
type scriptedCatchSender struct {
	caught map[string]int
	note   string
	err    error
	calls  map[string]int
}

func newScriptedCatchSender(t *testing.T, withoutCaught, withCaught int) *scriptedCatchSender {
	t.Helper()
	return &scriptedCatchSender{
		caught: map[string]int{ArmWithoutConventions: withoutCaught, ArmWithConventions: withCaught},
		note:   allCaseCatchNote(t),
		calls:  map[string]int{},
	}
}

// allCaseCatchNote is one concern note naming every committed case's planted
// criterion id(s) and first catch probe.
func allCaseCatchNote(t *testing.T) string {
	t.Helper()
	cases, _ := loadCommittedCatch(t)
	terms := make([]string, 0, 2*len(cases))
	for _, c := range cases {
		terms = append(terms, c.CatchRule().CriterionIDs...)
		terms = append(terms, c.Input.CatchProbes[0])
	}
	return "the plan's criteria are defective: " + strings.Join(terms, "; ")
}

// catchArmOf is the arm a fake sender's user prompt belongs to.
func catchArmOf(userText string) string {
	if strings.Contains(userText, "### "+prompt.ReviewConventionsHeading) {
		return ArmWithConventions
	}
	return ArmWithoutConventions
}

func (s *scriptedCatchSender) Messages(_ context.Context, _, userText string) (string, string, int, int, int, int, error) {
	if s.err != nil {
		return "", "", 0, 0, 0, 0, s.err
	}
	arm := ArmWithoutConventions
	if strings.Contains(userText, "### "+prompt.ReviewConventionsHeading) {
		arm = ArmWithConventions
	}
	n := s.calls[arm]
	s.calls[arm]++
	if n < s.caught[arm] {
		b, _ := json.Marshal(map[string]any{"verdict": "approve_with_concerns", "concerns": []any{
			map[string]any{"severity": "medium", "category": "acceptance_criteria", "note": s.note},
		}})
		return string(b), "fake-model", 0, 0, 0, 0, nil
	}
	return `{"verdict":"approve","concerns":[]}`, "fake-model", 0, 0, 0, 0, nil
}

func (s *scriptedCatchSender) total() int {
	return s.calls[ArmWithoutConventions] + s.calls[ArmWithConventions]
}

const testCatchModel = "fixture-generator-model"

// committedCatchTrials is the per-arm trial count a recording against the
// committed corpus takes.
func committedCatchTrials(t *testing.T) int {
	t.Helper()
	cases, _ := loadCommittedCatch(t)
	return SamplesPerCaseForPower(len(cases)) * len(cases)
}

func catchesAtRate(t *testing.T, rate float64) int {
	t.Helper()
	return int(math.Round(rate * float64(committedCatchTrials(t))))
}

var catchT0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func record(t *testing.T, sender MessageSender, path string, now time.Time, opts RecordCatchRateOptions) (string, error) {
	t.Helper()
	return RecordCatchRateEvidence(context.Background(), sender, testCatchModel, committedCatchCorpus, committedCatchConventions, path, now, opts)
}

func pin(reason string) RecordCatchRateOptions {
	return RecordCatchRateOptions{PinBaseline: true, Reason: reason}
}

// freshCatchEvidence records a pinned, all-caught measurement (and its trial
// sidecar) in a temp dir.
func freshCatchEvidence(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evidence.json")
	all := committedCatchTrials(t)
	if _, err := record(t, newScriptedCatchSender(t, all, all), path, catchT0, pin("initial baseline")); err != nil {
		t.Fatalf("record fresh evidence: %v", err)
	}
	return path
}

// freshMixedCatchEvidence records a pinned, passing 0.95 measurement: the
// scripted sender catches each arm's first calls, so the LAST case's last
// trials are missed in both arms — a fixture carrying caught AND missed
// trials, which the sidecar's per-sub-check rows need.
func freshMixedCatchEvidence(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evidence.json")
	k := catchesAtRate(t, 0.95)
	if _, err := record(t, newScriptedCatchSender(t, k, k), path, catchT0, pin("initial baseline")); err != nil {
		t.Fatalf("record fresh mixed evidence: %v", err)
	}
	return path
}

func readCatchTrials(t *testing.T, evidencePath string) CatchRateTrials {
	t.Helper()
	var tr CatchRateTrials
	if err := decodeStrictJSON(mustRead(t, CatchRateTrialsPath(evidencePath)), &tr); err != nil {
		t.Fatalf("decode the trial sidecar: %v", err)
	}
	return tr
}

func writeCatchTrials(t *testing.T, evidencePath string, tr CatchRateTrials) {
	t.Helper()
	b, err := json.MarshalIndent(tr, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(CatchRateTrialsPath(evidencePath), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// lastTrialCase is the sorted-last case of a sidecar arm: in the mixed
// fixture its last trial is missed.
func lastTrialCase(m map[string][]CatchRateTrialRecord) string {
	names := sortedTrialCaseNames(m)
	return names[len(names)-1]
}

func readCatchEvidence(t *testing.T, path string) CatchRateEvidence {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec CatchRateEvidence
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func writeCatchEvidence(t *testing.T, path string, rec CatchRateEvidence) {
	t.Helper()
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// setCaught sets every case's caught count (undecodable 0).
func setCaught(m map[string]CaseCatchCounts, caught int) {
	for name, c := range m {
		c.Caught, c.Undecodable = caught, 0
		m[name] = c
	}
}

func firstCase(m map[string]CaseCatchCounts) string {
	return sortedCaseNames(m)[0]
}

func renameCase(m map[string]CaseCatchCounts, from, to string) {
	m[to] = m[from]
	delete(m, from)
}

// copyCatchCorpus copies the committed corpus into a temp dir.
func copyCatchCorpus(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(committedCatchCorpus, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(committedCatchCorpus, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// ---------------------------------------------------------------------------
// CheckCatchRateEvidence.
// ---------------------------------------------------------------------------

func TestCheckCatchRateEvidence_PassesOnFreshRecord(t *testing.T) {
	path := freshCatchEvidence(t)
	report, err := CheckCatchRateEvidence(path, committedCatchCorpus, committedCatchConventions, testCatchModel)
	if err != nil {
		t.Fatalf("CheckCatchRateEvidence on a fresh record: %v", err)
	}
	for _, want := range []string{"without conventions:", "with conventions:", "Rule: FAIL", "Pinned baseline", "does not roll forward", "Verdict: PASS",
		"Trial sidecar: " + CatchRateTrialsPath(path)} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
}

// TestCheckCatchRateEvidence_FailClosed is one row per fail-closed mode. Each
// row isolates its control: the perturbation leaves every OTHER check passing,
// so deleting the named control turns the row's refusal into a pass.
func TestCheckCatchRateEvidence_FailClosed(t *testing.T) {
	trials := committedCatchTrials(t)
	cases, _ := loadCommittedCatch(t)
	samples := SamplesPerCaseForPower(len(cases))
	tests := []struct {
		name        string
		mutate      func(rec *CatchRateEvidence)
		raw         func(raw []byte) []byte
		path        func(dir string) string
		corpus      string
		conventions string
		model       string
		want        string
		regressed   bool
		// The sidecar rows (13)-(16) start from the MIXED fixture and perturb
		// only the trial sidecar; the record itself passes (1)-(12).
		trials       func(t *testing.T, tr *CatchRateTrials)
		trialsRaw    func(raw []byte) []byte
		deleteTrials bool
	}{
		{name: "(1) absent evidence file", path: func(dir string) string { return filepath.Join(dir, "nope.json") }, want: "absent: no two-arm measurement"},
		{name: "(2) malformed JSON", raw: func([]byte) []byte { return []byte("{") }, want: "malformed JSON"},
		{name: "(2) a second object after a valid record", raw: func(raw []byte) []byte {
			return append(append([]byte{}, raw...), `{"schema":"planreview-catchrate-evidence-v1"}`...)
		}, want: "trailing content"},
		{name: "(2) trailing garbage after a valid record", raw: func(raw []byte) []byte {
			return append(append([]byte{}, raw...), "garbage\n"...)
		}, want: "trailing content"},
		{name: "(2) unknown field (a stored verdict)", raw: func(raw []byte) []byte {
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			m["verdict"] = "pass"
			b, _ := json.Marshal(m)
			return b
		}, want: "unknown field"},
		{name: "(3) wrong schema", mutate: func(r *CatchRateEvidence) { r.Schema = "planreview-catchrate-evidence-v0" }, want: "schema"},
		{name: "(4) generator model mismatch", model: "another-model", want: "generator model"},
		{name: "(5) tolerance differs", mutate: func(r *CatchRateEvidence) { r.Tolerance = 0.2 }, want: "tolerance"},
		{name: "(5) min trials differs", mutate: func(r *CatchRateEvidence) { r.MinTrialsPerArm = 100 }, want: "min_trials_per_arm"},
		{name: "(6) samples_per_case below 1", mutate: func(r *CatchRateEvidence) { r.SamplesPerCase = 0 }, want: "must be >= 1"},
		{name: "(6) negative count", mutate: func(r *CatchRateEvidence) {
			m := r.Arms.WithConventions.PerCase
			c := m[firstCase(m)]
			c.Undecodable = -1
			m[firstCase(m)] = c
		}, want: "inconsistent counts in arm"},
		{name: "(6) caught+undecodable > trials, every count in bounds, totals >= 136", mutate: func(r *CatchRateEvidence) {
			m := r.Arms.WithoutConventions.PerCase
			c := m[firstCase(m)]
			c.Caught, c.Undecodable = c.Trials-3, 5 // each <= trials; the sum is not
			m[firstCase(m)] = c
		}, want: "inconsistent counts in arm"},
		{name: "(6) per-case trials != samples_per_case, arms agree, totals >= 136", mutate: func(r *CatchRateEvidence) {
			for _, m := range []map[string]CaseCatchCounts{r.Arms.WithoutConventions.PerCase, r.Arms.WithConventions.PerCase} {
				names := sortedCaseNames(m)
				a, b := m[names[0]], m[names[1]]
				a.Trials, a.Caught = samples-1, samples-1
				b.Trials, b.Caught = samples+1, samples+1
				m[names[0]], m[names[1]] = a, b
			}
		}, want: "not samples_per_case"},
		{name: "(6) arms disagree on the case set: with arm lacks a case", mutate: func(r *CatchRateEvidence) {
			m := r.Arms.WithConventions.PerCase
			delete(m, firstCase(m))
		}, want: "disagree on the case set"},
		{name: "(6) arms disagree on the case set: with arm carries an extra case, totals >= 136", mutate: func(r *CatchRateEvidence) {
			r.Arms.WithConventions.PerCase["zz-not-in-without-arm"] = CaseCatchCounts{Trials: samples, Caught: samples}
		}, want: "disagree on the case set"},
		{name: "(6) arms disagree on per-case trial weights, totals >= 136", mutate: func(r *CatchRateEvidence) {
			m := r.Arms.WithoutConventions.PerCase
			names := sortedCaseNames(m)
			a, b := m[names[0]], m[names[1]]
			a.Trials, a.Caught = samples-1, samples-1
			b.Trials, b.Caught = samples+1, samples+1
			m[names[0]], m[names[1]] = a, b
		}, want: "trial weight"},
		{name: "(7) case set differs from the corpus (baseline agrees)", mutate: func(r *CatchRateEvidence) {
			for _, m := range []map[string]CaseCatchCounts{
				r.Arms.WithoutConventions.PerCase, r.Arms.WithConventions.PerCase,
				r.Baseline.Arms.WithoutConventions.PerCase, r.Baseline.Arms.WithConventions.PerCase,
			} {
				renameCase(m, firstCase(m), "zz-retired-case")
			}
		}, want: "differs from the current corpus"},
		{name: "(8) under-powered arms", mutate: func(r *CatchRateEvidence) {
			r.SamplesPerCase = 1
			for _, m := range []map[string]CaseCatchCounts{r.Arms.WithoutConventions.PerCase, r.Arms.WithConventions.PerCase} {
				for name := range m {
					m[name] = CaseCatchCounts{Trials: 1, Caught: 1}
				}
			}
		}, want: "under-powered"},
		{name: "(9) within-run regression, recomputed from counts (baseline agrees)", mutate: func(r *CatchRateEvidence) {
			// 0.30 below the without arm; the baseline's with arm is moved to
			// match, so only the within-run rule can refuse it.
			setCaught(r.Arms.WithConventions.PerCase, int(math.Round(0.7*float64(samples))))
			setCaught(r.Baseline.Arms.WithConventions.PerCase, int(math.Round(0.7*float64(samples))))
		}, want: "below the without-conventions arm", regressed: true},
		{name: "(10) missing baseline", mutate: func(r *CatchRateEvidence) { r.Baseline = nil }, want: "no pinned baseline"},
		{name: "(10) baseline without a reason", mutate: func(r *CatchRateEvidence) { r.Baseline.Reason = " " }, want: "no reason"},
		{name: "(10) baseline without a timestamp", mutate: func(r *CatchRateEvidence) { r.Baseline.PinnedAt = "yesterday" }, want: "RFC 3339"},
		{name: "(10) baseline with inconsistent counts", mutate: func(r *CatchRateEvidence) {
			m := r.Baseline.Arms.WithoutConventions.PerCase
			c := m[firstCase(m)]
			c.Caught = c.Trials + 1
			m[firstCase(m)] = c
		}, want: "pinned baseline: inconsistent counts"},
		{name: "(10) baseline from another generator model", mutate: func(r *CatchRateEvidence) { r.Baseline.GeneratorModel = "another-model" }, want: "not comparable"},
		{name: "(10) baseline from another corpus", mutate: func(r *CatchRateEvidence) {
			for _, m := range []map[string]CaseCatchCounts{r.Baseline.Arms.WithoutConventions.PerCase, r.Baseline.Arms.WithConventions.PerCase} {
				renameCase(m, firstCase(m), "zz-retired-case")
			}
		}, want: "corpus changed"},
		{name: "(10) baseline with another samples_per_case", mutate: func(r *CatchRateEvidence) {
			r.Baseline.SamplesPerCase = samples + 1
			for _, m := range []map[string]CaseCatchCounts{r.Baseline.Arms.WithoutConventions.PerCase, r.Baseline.Arms.WithConventions.PerCase} {
				for name := range m {
					m[name] = CaseCatchCounts{Trials: samples + 1, Caught: samples + 1}
				}
			}
		}, want: "samples_per_case changed"},
		{name: "(10) both arms equal but 0.17 below the pinned baseline", mutate: func(r *CatchRateEvidence) {
			setCaught(r.Arms.WithoutConventions.PerCase, int(math.Round(0.83*float64(samples))))
			setCaught(r.Arms.WithConventions.PerCase, int(math.Round(0.83*float64(samples))))
		}, want: "against the pinned baseline", regressed: true},
		{name: "(11) corpus unavailable", corpus: "testdata/no-such-corpus", want: "corpus unavailable"},
		{name: "(12) conventions fixture unavailable", conventions: "testdata/planreview-catchrate/no-such.md", want: "conventions fixture unavailable"},
		{name: "(13) absent trial sidecar", deleteTrials: true, want: "absent: every recording writes the trial sidecar"},
		{name: "(14) sidecar malformed JSON", trialsRaw: func([]byte) []byte { return []byte("{") }, want: "malformed JSON"},
		{name: "(14) sidecar unknown field", trialsRaw: func(raw []byte) []byte {
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			m["verdict"] = "pass"
			b, _ := json.Marshal(m)
			return b
		}, want: "unknown field"},
		{name: "(14) sidecar trailing content", trialsRaw: func(raw []byte) []byte {
			return append(append([]byte{}, raw...), "{}"...)
		}, want: "trailing content"},
		{name: "(14) sidecar wrong schema", trials: func(_ *testing.T, tr *CatchRateTrials) { tr.Schema = "planreview-catchrate-trials-v0" }, want: `schema "planreview-catchrate-trials-v0" is not`},
		{name: "(15) sidecar from another recording: prompt_fingerprint", trials: func(_ *testing.T, tr *CatchRateTrials) {
			tr.PromptFingerprint = "sha256:" + strings.Repeat("0", 64)
		}, want: "prompt_fingerprint sha256:"},
		{name: "(15) sidecar from another recording: recorded_at", trials: func(_ *testing.T, tr *CatchRateTrials) {
			tr.RecordedAt = catchT0.Add(-time.Hour).Format(time.RFC3339)
		}, want: "recorded_at"},
		// (16): each row trips ONE sub-check (approval condition C3).
		{name: "(16) list length: a missed trial dropped", trials: func(t *testing.T, tr *CatchRateTrials) {
			m := tr.Arms.WithoutConventions.PerCase
			name := lastTrialCase(m)
			list := m[name]
			if list[len(list)-1].Outcome != CatchMissed {
				t.Fatalf("fixture premise: the last trial of %q must be missed, got %q", name, list[len(list)-1].Outcome)
			}
			m[name] = list[:len(list)-1]
		}, want: "trials, the record"},
		{name: "(16) trial indices: a duplicated index", trials: func(_ *testing.T, tr *CatchRateTrials) {
			list := tr.Arms.WithConventions.PerCase[sortedTrialCaseNames(tr.Arms.WithConventions.PerCase)[0]]
			list[1].Trial = list[0].Trial
		}, want: "trial index 1, want 2"},
		{name: "(16) outcome tallies: a caught trial flipped to missed", trials: func(t *testing.T, tr *CatchRateTrials) {
			list := tr.Arms.WithoutConventions.PerCase[sortedTrialCaseNames(tr.Arms.WithoutConventions.PerCase)[0]]
			if list[0].Outcome != CatchCaught {
				t.Fatalf("fixture premise: the first trial must be caught, got %q", list[0].Outcome)
			}
			list[0].Outcome = CatchMissed
		}, want: "outcome tallies"},
		{name: "(16) outcome tallies: an unknown outcome", trials: func(t *testing.T, tr *CatchRateTrials) {
			m := tr.Arms.WithConventions.PerCase
			list := m[lastTrialCase(m)]
			if list[len(list)-1].Outcome != CatchMissed {
				t.Fatalf("fixture premise: the last trial must be missed, got %q", list[len(list)-1].Outcome)
			}
			list[len(list)-1].Outcome = "maybe"
		}, want: "outcome tallies"},
		{name: "(16) case set: an extra case in one arm", trials: func(_ *testing.T, tr *CatchRateTrials) {
			tr.Arms.WithoutConventions.PerCase["zz-not-recorded"] = []CatchRateTrialRecord{{Trial: 1, Outcome: CatchMissed, Concerns: []CatchRateTrialConcern{}}}
		}, want: "case set"},
	}
	if trials < MinCatchRateTrialsPerArm {
		t.Fatalf("fixture premise: the committed corpus records %d trials per arm, below the floor", trials)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sidecarRow := tc.trials != nil || tc.trialsRaw != nil || tc.deleteTrials
			var path string
			if sidecarRow {
				path = freshMixedCatchEvidence(t)
				if _, err := CheckCatchRateEvidence(path, committedCatchCorpus, committedCatchConventions, testCatchModel); err != nil {
					t.Fatalf("fixture premise: the unperturbed mixed pair passes: %v", err)
				}
			} else {
				path = freshCatchEvidence(t)
			}
			if tc.trials != nil {
				tr := readCatchTrials(t, path)
				tc.trials(t, &tr)
				writeCatchTrials(t, path, tr)
			}
			if tc.trialsRaw != nil {
				if err := os.WriteFile(CatchRateTrialsPath(path), tc.trialsRaw(mustRead(t, CatchRateTrialsPath(path))), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.deleteTrials {
				if err := os.Remove(CatchRateTrialsPath(path)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.mutate != nil {
				rec := readCatchEvidence(t, path)
				tc.mutate(&rec)
				writeCatchEvidence(t, path, rec)
			}
			if tc.raw != nil {
				if err := os.WriteFile(path, tc.raw(mustRead(t, path)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.path != nil {
				path = tc.path(filepath.Dir(path))
			}
			corpus, conv, model := committedCatchCorpus, committedCatchConventions, testCatchModel
			if tc.corpus != "" {
				corpus = tc.corpus
			}
			if tc.conventions != "" {
				conv = tc.conventions
			}
			if tc.model != "" {
				model = tc.model
			}
			_, err := CheckCatchRateEvidence(path, corpus, conv, model)
			if err == nil {
				t.Fatalf("CheckCatchRateEvidence passed; want a refusal naming %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %q", err, tc.want)
			}
			if sidecarRow && !strings.Contains(err.Error(), "trial sidecar "+`"`+CatchRateTrialsPath(path)+`"`) {
				t.Errorf("err = %v, want it to name the trial sidecar %s", err, CatchRateTrialsPath(path))
			}
			if !strings.Contains(err.Error(), catchRateRunbook) {
				t.Errorf("err = %v, want it to name the run-book %s", err, catchRateRunbook)
			}
			if got := errors.Is(err, ErrCatchRateRegressed); got != tc.regressed {
				t.Errorf("errors.Is(err, ErrCatchRateRegressed) = %v, want %v (err %v)", got, tc.regressed, err)
			}
		})
	}
}

// TestCheckCatchRateEvidence_StaleFingerprint is mode (4): a record taken
// against a conventions file one byte different from the committed one.
func TestCheckCatchRateEvidence_StaleFingerprint(t *testing.T) {
	dir := t.TempDir()
	conv := mustRead(t, committedCatchConventions)
	conv[len(conv)-2] ^= 0x01
	convPath := filepath.Join(dir, "conventions.md")
	if err := os.WriteFile(convPath, conv, 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "evidence.json")
	all := committedCatchTrials(t)
	if _, err := RecordCatchRateEvidence(context.Background(), newScriptedCatchSender(t, all, all), testCatchModel, committedCatchCorpus, convPath, path, catchT0, pin("baseline")); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckCatchRateEvidence(path, committedCatchCorpus, convPath, testCatchModel); err != nil {
		t.Fatalf("premise: the record is fresh against its own conventions: %v", err)
	}
	_, err := CheckCatchRateEvidence(path, committedCatchCorpus, committedCatchConventions, testCatchModel)
	if err == nil || !strings.Contains(err.Error(), "STALE") {
		t.Fatalf("err = %v, want a STALE fingerprint refusal", err)
	}
}

// ---------------------------------------------------------------------------
// RecordCatchRateEvidence.
// ---------------------------------------------------------------------------

// TestRecordCatchRateEvidence_PinnedBaselineSeries is approval condition 1:
// an equal-arm series 0.90 → 0.82 → 0.74 → 0.66, each step only 0.08 below
// the PREVIOUS recording (inside the 0.10 tolerance), must not erode the
// reference. The THIRD recording (0.74) is the failing step: it is 0.16 below
// the pinned 0.90 baseline. The fourth (0.66) fails too. Against the
// previous record only, every step would pass.
func TestRecordCatchRateEvidence_PinnedBaselineSeries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.json")
	var afterSecond []byte
	for i, rate := range []float64{0.90, 0.82, 0.74, 0.66} {
		k := catchesAtRate(t, rate)
		opts := RecordCatchRateOptions{}
		if i == 0 {
			opts = pin("first measurement")
		}
		_, err := record(t, newScriptedCatchSender(t, k, k), path, catchT0.Add(time.Duration(i)*time.Hour), opts)
		switch i {
		case 0, 1:
			if err != nil {
				t.Fatalf("recording %d (rate %.2f): %v", i+1, rate, err)
			}
			afterSecond = mustRead(t, path)
		default:
			if !errors.Is(err, ErrCatchRateRegressed) || !strings.Contains(err.Error(), "against the pinned baseline") {
				t.Fatalf("recording %d (rate %.2f) err = %v, want a pinned-baseline regression refusal", i+1, rate, err)
			}
			if !bytes.Equal(mustRead(t, path), afterSecond) {
				t.Fatalf("recording %d (rate %.2f) changed the evidence file despite the refusal", i+1, rate)
			}
		}
	}
	rec := readCatchEvidence(t, path)
	if rec.Baseline.Reason != "first measurement" || rec.Baseline.PinnedAt != catchT0.Format(time.RFC3339) {
		t.Fatalf("baseline moved: %+v", rec.Baseline)
	}
}

// TestRecordCatchRateEvidence_RefusesFailingMeasurement: a failing
// measurement is refused and the evidence file is byte-identical — with and
// without a pin (a failed run can never become a baseline).
func TestRecordCatchRateEvidence_RefusesFailingMeasurement(t *testing.T) {
	all := committedCatchTrials(t)
	for _, tc := range []struct {
		name    string
		without int
		with    int
		opts    RecordCatchRateOptions
		want    string
	}{
		{name: "within-run regression", without: all, with: 0, want: "below the without-conventions arm"},
		{name: "within-run regression, pinning", without: all, with: 0, opts: pin("try to pin a failure"), want: "below the without-conventions arm"},
		{name: "baseline regression, pinning", without: catchesAtRate(t, 0.74), with: catchesAtRate(t, 0.74), opts: pin("lower the bar"), want: "against the pinned baseline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := freshCatchEvidence(t)
			before := mustRead(t, path)
			beforeTrials := mustRead(t, CatchRateTrialsPath(path))
			_, err := record(t, newScriptedCatchSender(t, tc.without, tc.with), path, catchT0.Add(time.Hour), tc.opts)
			if !errors.Is(err, ErrCatchRateRegressed) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a regression refusal naming %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "evidence not written") {
				t.Errorf("err = %v, want it to say the evidence was not written", err)
			}
			if !bytes.Equal(mustRead(t, path), before) {
				t.Fatal("the refused recording changed the evidence file")
			}
			if !bytes.Equal(mustRead(t, CatchRateTrialsPath(path)), beforeTrials) {
				t.Fatal("the refused recording changed the trial sidecar")
			}
		})
	}
}

func TestRecordCatchRateEvidence_CarriesBaselineForward(t *testing.T) {
	path := freshCatchEvidence(t)
	pinned := readCatchEvidence(t, path).Baseline
	k := catchesAtRate(t, 0.95)
	if _, err := record(t, newScriptedCatchSender(t, k, k), path, catchT0.Add(time.Hour), RecordCatchRateOptions{}); err != nil {
		t.Fatal(err)
	}
	rec := readCatchEvidence(t, path)
	if !reflect.DeepEqual(rec.Baseline, pinned) {
		t.Fatalf("an ordinary recording moved the baseline:\n got %+v\nwant %+v", rec.Baseline, pinned)
	}
	if rec.RecordedAt != catchT0.Add(time.Hour).Format(time.RFC3339) {
		t.Errorf("recorded_at = %q", rec.RecordedAt)
	}
	if _, caught := pooledCounts(rec.Arms.WithConventions.PerCase); caught != k {
		t.Errorf("with-arm caught = %d, want %d", caught, k)
	}
	if _, err := CheckCatchRateEvidence(path, committedCatchCorpus, committedCatchConventions, testCatchModel); err != nil {
		t.Errorf("the carried-forward record does not pass the gate: %v", err)
	}
}

func TestRecordCatchRateEvidence_PinMovesBaselineWithReasonAndTimestamp(t *testing.T) {
	path := freshCatchEvidence(t)
	k := catchesAtRate(t, 0.95)
	later := catchT0.Add(24 * time.Hour)
	report, err := record(t, newScriptedCatchSender(t, k, k), path, later, pin("  model refresh  "))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "new baseline") {
		t.Errorf("report does not announce the pin:\n%s", report)
	}
	b := readCatchEvidence(t, path).Baseline
	if b.Reason != "model refresh" || b.PinnedAt != later.Format(time.RFC3339) {
		t.Fatalf("baseline = %+v, want reason %q pinned at %s", b, "model refresh", later.Format(time.RFC3339))
	}
	if _, caught := pooledCounts(b.Arms.WithoutConventions.PerCase); caught != k {
		t.Errorf("pinned without-arm caught = %d, want %d", caught, k)
	}
}

func TestRecordCatchRateEvidence_RefusesBeforeAnyModelCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evidence.json")
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		opts  RecordCatchRateOptions
		want  string
	}{
		{name: "first recording must pin", want: "first recording must pin"},
		{name: "pin requires a reason", opts: RecordCatchRateOptions{PinBaseline: true, Reason: "  "}, want: "requires a reason"},
		{name: "malformed existing record", setup: func(t *testing.T) {
			if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, opts: pin("x"), want: "malformed"},
		{name: "existing record with trailing content", setup: func(t *testing.T) {
			raw := mustRead(t, freshCatchEvidence(t))
			if err := os.WriteFile(path, append(raw, "{}"...), 0o644); err != nil {
				t.Fatal(err)
			}
		}, opts: pin("x"), want: "trailing content"},
		{name: "existing record with a wrong schema", setup: func(t *testing.T) {
			if err := os.WriteFile(path, []byte(`{"schema":"other"}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}, opts: pin("x"), want: "malformed"},
		{name: "existing baseline without a reason", setup: func(t *testing.T) {
			p := freshCatchEvidence(t)
			rec := readCatchEvidence(t, p)
			rec.Baseline.Reason = ""
			writeCatchEvidence(t, path, rec)
		}, opts: pin("x"), want: "no reason"},
		{name: "existing record has no baseline", setup: func(t *testing.T) {
			p := freshCatchEvidence(t)
			rec := readCatchEvidence(t, p)
			rec.Baseline = nil
			writeCatchEvidence(t, path, rec)
		}, want: "first recording must pin"},
		{name: "existing path unreadable", setup: func(t *testing.T) {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}, opts: pin("x"), want: "read existing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.RemoveAll(path)
			if tc.setup != nil {
				tc.setup(t)
			}
			before, _ := os.ReadFile(path)
			sender := newScriptedCatchSender(t, 0, 0)
			_, err := record(t, sender, path, catchT0, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %q", err, tc.want)
			}
			if sender.total() != 0 {
				t.Errorf("%d model calls were made before the refusal", sender.total())
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Error("the refusal changed the evidence file")
			}
		})
	}
}

func TestRecordCatchRateEvidence_FailClosed(t *testing.T) {
	all := committedCatchTrials(t)
	t.Run("transport error", func(t *testing.T) {
		path := freshCatchEvidence(t)
		before := mustRead(t, path)
		sender := newScriptedCatchSender(t, all, all)
		sender.err = errors.New("connection reset")
		if _, err := record(t, sender, path, catchT0, RecordCatchRateOptions{}); err == nil || !strings.Contains(err.Error(), "connection reset") {
			t.Fatalf("err = %v, want the transport error", err)
		}
		if !bytes.Equal(mustRead(t, path), before) {
			t.Fatal("a transport error changed the evidence file")
		}
	})
	t.Run("transport error on the with arm", func(t *testing.T) {
		path := freshCatchEvidence(t)
		before := mustRead(t, path)
		sender := &failingWithArmSender{inner: newScriptedCatchSender(t, all, all)}
		if _, err := record(t, sender, path, catchT0, RecordCatchRateOptions{}); err == nil || !strings.Contains(err.Error(), "with arm down") {
			t.Fatalf("err = %v, want the with-arm transport error", err)
		}
		if !bytes.Equal(mustRead(t, path), before) {
			t.Fatal("a transport error changed the evidence file")
		}
	})
	t.Run("empty generator model", func(t *testing.T) {
		_, err := RecordCatchRateEvidence(context.Background(), newScriptedCatchSender(t, all, all), " ", committedCatchCorpus, committedCatchConventions, filepath.Join(t.TempDir(), "e.json"), catchT0, pin("x"))
		if err == nil || !strings.Contains(err.Error(), "generator model") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("corpus unavailable", func(t *testing.T) {
		_, err := RecordCatchRateEvidence(context.Background(), newScriptedCatchSender(t, all, all), testCatchModel, "testdata/no-such-corpus", committedCatchConventions, filepath.Join(t.TempDir(), "e.json"), catchT0, pin("x"))
		if err == nil || !strings.Contains(err.Error(), "absent") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("conventions unavailable", func(t *testing.T) {
		_, err := RecordCatchRateEvidence(context.Background(), newScriptedCatchSender(t, all, all), testCatchModel, committedCatchCorpus, "testdata/no-such.md", filepath.Join(t.TempDir(), "e.json"), catchT0, pin("x"))
		if err == nil || !strings.Contains(err.Error(), "absent") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("evidence directory missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing-dir", "e.json")
		if _, err := record(t, newScriptedCatchSender(t, all, all), path, catchT0, pin("x")); err == nil || !strings.Contains(err.Error(), "write catch-rate evidence") {
			t.Fatalf("err = %v, want a write error", err)
		}
	})
}

// failingWithArmSender passes the without arm through and fails the with arm.
type failingWithArmSender struct{ inner *scriptedCatchSender }

func (f *failingWithArmSender) Messages(ctx context.Context, sys, userText string) (string, string, int, int, int, int, error) {
	if strings.Contains(userText, "### "+prompt.ReviewConventionsHeading) {
		return "", "", 0, 0, 0, 0, errors.New("with arm down")
	}
	return f.inner.Messages(ctx, sys, userText)
}

// TestRecordCatchRateEvidence_NonComparableBaselineNeedsRePin: after a corpus
// change the pinned baseline is a different instrument. An ordinary
// recording is refused; an explicit pin replaces it.
func TestRecordCatchRateEvidence_NonComparableBaselineNeedsRePin(t *testing.T) {
	path := freshCatchEvidence(t)
	before := mustRead(t, path)
	corpus := copyCatchCorpus(t)
	if err := os.RemoveAll(filepath.Join(corpus, committedSyntheticCatchCases(t)[0])); err != nil {
		t.Fatal(err)
	}
	// n is what the copied corpus actually holds after the removal, not a
	// count of a literal list.
	loaded, err := LoadPlanReviewCatchCorpus(corpus)
	if err != nil {
		t.Fatal(err)
	}
	n := len(loaded)
	all := SamplesPerCaseForPower(n) * n
	_, err = RecordCatchRateEvidence(context.Background(), newScriptedCatchSender(t, all, all), testCatchModel, corpus, committedCatchConventions, path, catchT0, RecordCatchRateOptions{})
	if err == nil || !strings.Contains(err.Error(), "not comparable") || !strings.Contains(err.Error(), "corpus changed") {
		t.Fatalf("err = %v, want a non-comparable-baseline refusal", err)
	}
	if !bytes.Equal(mustRead(t, path), before) {
		t.Fatal("the refusal changed the evidence file")
	}
	report, err := RecordCatchRateEvidence(context.Background(), newScriptedCatchSender(t, all, all), testCatchModel, corpus, committedCatchConventions, path, catchT0, pin("corpus retired a case"))
	if err != nil {
		t.Fatalf("re-pin after a corpus change: %v", err)
	}
	if !strings.Contains(report, "not comparable") || !strings.Contains(report, "the pin replaces it") {
		t.Errorf("report does not explain the replaced baseline:\n%s", report)
	}
	if got := readCatchEvidence(t, path).Baseline.SamplesPerCase; got != SamplesPerCaseForPower(n) {
		t.Errorf("re-pinned baseline samples_per_case = %d", got)
	}
}

func TestRecordCatchRateEvidence_DryRunNeverWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.json")
	all := committedCatchTrials(t)
	report, err := record(t, newScriptedCatchSender(t, all, all), path, catchT0, RecordCatchRateOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"No pinned baseline", "Dry run: evidence not written"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a dry run wrote the evidence file (stat err %v)", err)
	}
	if _, err := os.Stat(CatchRateTrialsPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a dry run wrote the trial sidecar (stat err %v)", err)
	}
	t.Run("over an existing pair", func(t *testing.T) {
		path := freshCatchEvidence(t)
		before, beforeTrials := mustRead(t, path), mustRead(t, CatchRateTrialsPath(path))
		k := catchesAtRate(t, 0.95)
		if _, err := record(t, newScriptedCatchSender(t, k, k), path, catchT0.Add(time.Hour), RecordCatchRateOptions{DryRun: true}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(mustRead(t, path), before) || !bytes.Equal(mustRead(t, CatchRateTrialsPath(path)), beforeTrials) {
			t.Fatal("a dry run changed the evidence file or its trial sidecar")
		}
	})
}

// ---------------------------------------------------------------------------
// The trial sidecar.
// ---------------------------------------------------------------------------

// fabricatedCatchToken matches redaction's github-pat-classic pattern. It is
// built at run time so no literal token sits in the source.
var fabricatedCatchToken = "ghp_" + strings.Repeat("Ab1x", 9)

// wideRuneNote is long enough to be truncated and made of 3-byte runes, so a
// BYTE cut at the cap lands mid-rune.
var wideRuneNote = strings.Repeat("日", 5000)

// sidecarFillerConcerns pads a missed verdict past catchTrialMaxConcerns.
const sidecarFillerConcerns = 15

// trialScriptSender answers each arm's n-th call (0-based, counted across
// cases) with script(n), cycling caught / missed / undecodable.
type trialScriptSender struct {
	script func(n int) string
	calls  map[string]int
}

func (s *trialScriptSender) Messages(_ context.Context, _, userText string) (string, string, int, int, int, int, error) {
	arm := catchArmOf(userText)
	n := s.calls[arm]
	s.calls[arm]++
	return s.script(n), "fake-model", 0, 0, 0, 0, nil
}

// newCyclingTrialSender: n%3==0 a caught verdict (one high-severity concern
// naming every case's id and first probe); n%3==1 a missed verdict whose
// concerns are, in order, a probe-without-attribution concern for the
// untestable-adjective case carrying a fabricated token in its severity,
// category and note, an
// attribution-without-probe concern naming every planted id, the wide-rune
// note, and filler past the concern cap; n%3==2 an undecodable response
// carrying the token.
func newCyclingTrialSender(t *testing.T) *trialScriptSender {
	t.Helper()
	cases, _ := loadCommittedCatch(t)
	var ids []string
	for _, c := range cases {
		ids = append(ids, c.CatchRule().CriterionIDs...)
	}
	verdict := func(concerns ...map[string]any) string {
		b, err := json.Marshal(map[string]any{"verdict": "approve_with_concerns", "concerns": concerns})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	caught := verdict(map[string]any{"severity": "high", "category": "acceptance_criteria", "note": allCaseCatchNote(t)})
	missedConcerns := []map[string]any{
		{"severity": "medium " + fabricatedCatchToken, "category": "testability " + fabricatedCatchToken, "note": "this criterion is vague; token " + fabricatedCatchToken},
		{"severity": "low", "category": "documentation", "note": strings.Join(ids, ", ") + " need a doc line"},
		{"severity": "low", "category": "coverage", "note": wideRuneNote},
	}
	for i := 0; i < sidecarFillerConcerns; i++ {
		missedConcerns = append(missedConcerns, map[string]any{"severity": "low", "category": "scope", "note": "filler concern"})
	}
	missed := verdict(missedConcerns...)
	undecodable := "not a verdict; token " + fabricatedCatchToken
	return &trialScriptSender{calls: map[string]int{}, script: func(n int) string {
		switch n % 3 {
		case 0:
			return caught
		case 1:
			return missed
		default:
			return undecodable
		}
	}}
}

// recordCyclingSidecar records a pinned measurement through the cycling sender
// and returns the evidence path, the record and the raw sidecar bytes.
func recordCyclingSidecar(t *testing.T) (string, CatchRateEvidence, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evidence.json")
	report, err := record(t, newCyclingTrialSender(t), path, catchT0, pin("sidecar fixture"))
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if !strings.Contains(report, "Trial sidecar written: "+CatchRateTrialsPath(path)) {
		t.Errorf("report does not name the written sidecar:\n%s", report)
	}
	return path, readCatchEvidence(t, path), mustRead(t, CatchRateTrialsPath(path))
}

func TestCatchRateTrialsPath(t *testing.T) {
	for in, want := range map[string]string{
		"testdata/planreview-catchrate/evidence.json": "testdata/planreview-catchrate/evidence.trials.json",
		"/x/e.json": "/x/e.trials.json",
		"/x/record": "/x/record.trials.json",
	} {
		if got := CatchRateTrialsPath(in); got != want {
			t.Errorf("CatchRateTrialsPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSameTrialCaseSet covers the case-set comparison mode (16) uses,
// including the equal-size, different-name branch the extra-case row cannot
// reach.
func TestSameTrialCaseSet(t *testing.T) {
	counts := map[string]CaseCatchCounts{"a": {}, "b": {}}
	for _, tc := range []struct {
		name   string
		trials map[string][]CatchRateTrialRecord
		want   bool
	}{
		{"equal", map[string][]CatchRateTrialRecord{"a": nil, "b": nil}, true},
		{"extra case", map[string][]CatchRateTrialRecord{"a": nil, "b": nil, "c": nil}, false},
		{"missing case", map[string][]CatchRateTrialRecord{"a": nil}, false},
		{"same size, renamed case", map[string][]CatchRateTrialRecord{"a": nil, "z": nil}, false},
	} {
		if got := sameTrialCaseSet(counts, tc.trials); got != tc.want {
			t.Errorf("%s: sameTrialCaseSet = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRecordCatchRateEvidence_WritesTrialSidecar: one record per trial, per
// arm and case in sample order, whose outcome tallies equal the evidence
// counts; a caught trial carries its matching concern (severity included),
// probe and attribution; a missed trial its near-miss tagged concerns; an
// undecodable trial a response excerpt; and the pair is bound by
// recorded_at and prompt_fingerprint — so the offline gate passes it.
func TestRecordCatchRateEvidence_WritesTrialSidecar(t *testing.T) {
	path, rec, raw := recordCyclingSidecar(t)
	var tr CatchRateTrials
	if err := decodeStrictJSON(raw, &tr); err != nil {
		t.Fatalf("the sidecar does not decode strictly: %v", err)
	}
	if tr.Schema != CatchRateTrialsSchema || tr.RecordedAt != rec.RecordedAt || tr.PromptFingerprint != rec.PromptFingerprint ||
		tr.NoteMaxRunes != catchTrialNoteMaxRunes || tr.MaxConcernsPerTrial != catchTrialMaxConcerns {
		t.Fatalf("sidecar envelope = %+v; want schema %s, recorded_at %s, fingerprint %s", tr, CatchRateTrialsSchema, rec.RecordedAt, rec.PromptFingerprint)
	}
	for _, a := range []struct {
		arm    string
		counts map[string]CaseCatchCounts
		trials map[string][]CatchRateTrialRecord
	}{
		{ArmWithoutConventions, rec.Arms.WithoutConventions.PerCase, tr.Arms.WithoutConventions.PerCase},
		{ArmWithConventions, rec.Arms.WithConventions.PerCase, tr.Arms.WithConventions.PerCase},
	} {
		if !reflect.DeepEqual(sortedTrialCaseNames(a.trials), sortedCaseNames(a.counts)) {
			t.Fatalf("arm %q sidecar cases %v, record cases %v", a.arm, sortedTrialCaseNames(a.trials), sortedCaseNames(a.counts))
		}
		for name, list := range a.trials {
			counts := a.counts[name]
			if len(list) != counts.Trials {
				t.Errorf("arm %q case %q: %d trial records, %d recorded trials", a.arm, name, len(list), counts.Trials)
			}
			tally := map[CatchOutcome]int{}
			for i, r := range list {
				tally[r.Outcome]++
				if r.Trial != i+1 {
					t.Errorf("arm %q case %q entry %d: trial %d", a.arm, name, i, r.Trial)
				}
				switch r.Outcome {
				case CatchCaught:
					if r.MatchedProbe == "" || r.Attribution != CatchAttributionID || len(r.Concerns) != 1 ||
						r.Concerns[0].Severity != "high" || r.Concerns[0].NearMiss != "" || r.ResponseExcerpt != "" {
						t.Errorf("arm %q case %q caught trial %d = %+v", a.arm, name, r.Trial, r)
					}
				case CatchMissed:
					if r.MatchedProbe != "" || r.Attribution != "" || r.Concerns[1].NearMiss != NearMissAttributionWithoutProbe ||
						r.Concerns[1].Severity != "low" || r.ResponseExcerpt != "" {
						t.Errorf("arm %q case %q missed trial %d = %+v", a.arm, name, r.Trial, r)
					}
					if name == "seed-synthetic-untestable-adjective" && r.Concerns[0].NearMiss != NearMissProbeWithoutAttribution {
						t.Errorf("arm %q case %q missed trial %d: concern 0 near_miss %q, want %q", a.arm, name, r.Trial, r.Concerns[0].NearMiss, NearMissProbeWithoutAttribution)
					}
				case CatchUndecodable:
					if r.ResponseExcerpt == "" || len(r.Concerns) != 0 || r.MatchedProbe != "" {
						t.Errorf("arm %q case %q undecodable trial %d = %+v", a.arm, name, r.Trial, r)
					}
				default:
					t.Errorf("arm %q case %q trial %d: outcome %q", a.arm, name, r.Trial, r.Outcome)
				}
			}
			if tally[CatchCaught] != counts.Caught || tally[CatchUndecodable] != counts.Undecodable {
				t.Errorf("arm %q case %q tallies %v, recorded caught %d undecodable %d", a.arm, name, tally, counts.Caught, counts.Undecodable)
			}
			if tally[CatchCaught] == 0 || tally[CatchMissed] == 0 || tally[CatchUndecodable] == 0 {
				t.Errorf("arm %q case %q tallies %v: the fixture must exercise all three outcomes", a.arm, name, tally)
			}
		}
	}
	report, err := CheckCatchRateEvidence(path, committedCatchCorpus, committedCatchConventions, testCatchModel)
	if err != nil {
		t.Fatalf("CheckCatchRateEvidence on the recorded pair: %v", err)
	}
	if !strings.Contains(report, CatchRateTrialsPath(path)) {
		t.Errorf("the gate report does not name the sidecar:\n%s", report)
	}
}

// TestRecordCatchRateEvidence_SidecarRecordsPhraseAttribution (#4326 b): a
// trial caught through an attribution PHRASE — its one concern names every
// case's first attribution phrase and first probe and NO planted id — reaches
// the committed sidecar as attribution "phrase", never rewritten to "id", so
// the id-attributed share the run-book's jq reads is not inflated.
func TestRecordCatchRateEvidence_SidecarRecordsPhraseAttribution(t *testing.T) {
	cases, _ := loadCommittedCatch(t)
	terms := make([]string, 0, 2*len(cases))
	for _, c := range cases {
		terms = append(terms, c.Input.AttributionPhrases[0], c.Input.CatchProbes[0])
	}
	note := "the plan's criteria are defective: " + strings.Join(terms, "; ")
	for _, c := range cases {
		if _, attribution, ok := matchCatchConcern("acceptance_criteria", note, c.CatchRule()); !ok || attribution != CatchAttributionPhrase {
			t.Fatalf("case %q: fixture premise: the note must catch via a phrase, got caught=%v attribution %q", c.Name, ok, attribution)
		}
	}
	b, err := json.Marshal(map[string]any{"verdict": "approve_with_concerns", "concerns": []any{
		map[string]any{"severity": "high", "category": "acceptance_criteria", "note": note},
	}})
	if err != nil {
		t.Fatal(err)
	}
	sender := &trialScriptSender{calls: map[string]int{}, script: func(int) string { return string(b) }}
	path := filepath.Join(t.TempDir(), "evidence.json")
	if _, err := record(t, sender, path, catchT0, pin("phrase-attribution fixture")); err != nil {
		t.Fatalf("record: %v", err)
	}
	tr := readCatchTrials(t, path)
	caught := 0
	for arm, perCase := range map[string]map[string][]CatchRateTrialRecord{
		ArmWithoutConventions: tr.Arms.WithoutConventions.PerCase,
		ArmWithConventions:    tr.Arms.WithConventions.PerCase,
	} {
		for name, list := range perCase {
			for _, r := range list {
				if r.Outcome != CatchCaught || r.Attribution != CatchAttributionPhrase {
					t.Fatalf("arm %q case %q trial %d: outcome %q attribution %q, want caught via %q", arm, name, r.Trial, r.Outcome, r.Attribution, CatchAttributionPhrase)
				}
				caught++
			}
		}
	}
	if caught == 0 {
		t.Fatal("the sidecar recorded no caught trial")
	}
	if !bytes.Contains(mustRead(t, CatchRateTrialsPath(path)), []byte(`"attribution": "phrase"`)) {
		t.Error(`the committed sidecar bytes carry no "attribution": "phrase"`)
	}
}

// TestRecordCatchRateEvidence_TrialSidecarRedacts: a fabricated GitHub token
// in a missed concern's note, category and severity and in an undecodable
// response is committed as the redaction marker, never verbatim — every
// model-derived field, so a refactor that bypasses sanitizeCatchTrialText for
// one of them fails here.
func TestRecordCatchRateEvidence_TrialSidecarRedacts(t *testing.T) {
	_, _, raw := recordCyclingSidecar(t)
	if bytes.Contains(raw, []byte(fabricatedCatchToken)) {
		t.Fatal("the trial sidecar carries the fabricated token verbatim")
	}
	var tr CatchRateTrials
	if err := decodeStrictJSON(raw, &tr); err != nil {
		t.Fatal(err)
	}
	const marker = "[REDACTED:github-pat-classic]"
	var notes, categories, severities, excerpts int
	for _, list := range tr.Arms.WithoutConventions.PerCase {
		for _, r := range list {
			if r.Outcome == CatchMissed {
				if strings.Contains(r.Concerns[0].Note, marker) {
					notes++
				}
				if strings.Contains(r.Concerns[0].Category, marker) {
					categories++
				}
				if strings.Contains(r.Concerns[0].Severity, marker) {
					severities++
				}
			}
			if r.Outcome == CatchUndecodable && strings.Contains(r.ResponseExcerpt, marker) {
				excerpts++
			}
		}
	}
	if notes == 0 || categories == 0 || severities == 0 || excerpts == 0 {
		t.Fatalf("redaction markers: %d in missed notes, %d in missed categories, %d in missed severities, %d in response excerpts; want all > 0", notes, categories, severities, excerpts)
	}
}

// TestRecordCatchRateEvidence_TrialSidecarTruncatesRuneSafely is approval
// condition C4: a note of 5000 three-byte runes is stored as valid UTF-8, at
// or under note_max_runes, ending in the marker, and is exactly the note's
// leading runes plus the marker.
func TestRecordCatchRateEvidence_TrialSidecarTruncatesRuneSafely(t *testing.T) {
	_, _, raw := recordCyclingSidecar(t)
	var tr CatchRateTrials
	if err := decodeStrictJSON(raw, &tr); err != nil {
		t.Fatal(err)
	}
	want := string([]rune(wideRuneNote)[:tr.NoteMaxRunes-utf8.RuneCountInString(catchTrialTruncationMarker)]) + catchTrialTruncationMarker
	checked := 0
	for _, list := range tr.Arms.WithConventions.PerCase {
		for _, r := range list {
			if r.Outcome != CatchMissed {
				continue
			}
			got := r.Concerns[2].Note
			if !utf8.ValidString(got) || utf8.RuneCountInString(got) > tr.NoteMaxRunes || !strings.HasSuffix(got, catchTrialTruncationMarker) {
				t.Fatalf("stored note: valid %v, %d runes (cap %d), marker %v", utf8.ValidString(got), utf8.RuneCountInString(got), tr.NoteMaxRunes, strings.HasSuffix(got, catchTrialTruncationMarker))
			}
			if got != want {
				t.Fatalf("stored note is not the note's leading runes plus the marker (%d runes stored)", utf8.RuneCountInString(got))
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no missed trial carried the wide-rune note")
	}
	// The helper itself, before any JSON round trip could mask a mid-rune cut.
	if got := truncateRunes(wideRuneNote, catchTrialNoteMaxRunes); !utf8.ValidString(got) || got != want {
		t.Fatalf("truncateRunes: valid UTF-8 %v, equal to the rune-prefix %v", utf8.ValidString(got), got == want)
	}
	if got := truncateRunes("short", catchTrialNoteMaxRunes); got != "short" {
		t.Errorf("truncateRunes on a short string = %q", got)
	}
	if got := truncateRunes("abc\xffdef", catchTrialNoteMaxRunes); !utf8.ValidString(got) {
		t.Errorf("truncateRunes kept invalid UTF-8: %q", got)
	}
	if got := truncateRunes(wideRuneNote, 3); got != catchTrialTruncationMarker {
		t.Errorf("truncateRunes below the marker length = %q, want the bare marker", got)
	}
}

// TestRecordCatchRateEvidence_TrialSidecarCapsConcerns: a missed trial keeps
// at most max_concerns_per_trial concerns, in emitted order, and
// concerns_total records the uncapped count.
func TestRecordCatchRateEvidence_TrialSidecarCapsConcerns(t *testing.T) {
	_, _, raw := recordCyclingSidecar(t)
	var tr CatchRateTrials
	if err := decodeStrictJSON(raw, &tr); err != nil {
		t.Fatal(err)
	}
	for _, list := range tr.Arms.WithoutConventions.PerCase {
		for _, r := range list {
			if r.Outcome != CatchMissed {
				continue
			}
			if r.ConcernsTotal != 3+sidecarFillerConcerns || len(r.Concerns) != tr.MaxConcernsPerTrial {
				t.Fatalf("missed trial %d: %d concerns stored of %d, want %d of %d", r.Trial, len(r.Concerns), r.ConcernsTotal, tr.MaxConcernsPerTrial, 3+sidecarFillerConcerns)
			}
			if r.Concerns[tr.MaxConcernsPerTrial-1].Note != "filler concern" {
				t.Fatalf("the cap did not keep emitted order: last kept %q", r.Concerns[tr.MaxConcernsPerTrial-1].Note)
			}
		}
	}
}

// TestRecordCatchRateEvidence_SidecarWriteFailureLeavesNoPair: when the
// sidecar cannot be renamed into place, neither file is written and no
// staged temp file is left behind.
func TestRecordCatchRateEvidence_SidecarWriteFailureLeavesNoPair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evidence.json")
	// A non-empty directory at the sidecar path makes its rename fail.
	if err := os.MkdirAll(filepath.Join(CatchRateTrialsPath(path), "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	all := committedCatchTrials(t)
	if _, err := record(t, newScriptedCatchSender(t, all, all), path, catchT0, pin("x")); err == nil || !strings.Contains(err.Error(), "write catch-rate evidence") {
		t.Fatalf("err = %v, want a write error", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the evidence was written although its sidecar was not (stat err %v)", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".catchrate-") {
			t.Errorf("staged temp file %q left behind", e.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// Fingerprint.
// ---------------------------------------------------------------------------

func TestCatchRatePromptFingerprint_Deterministic(t *testing.T) {
	cases, conv := loadCommittedCatch(t)
	a, err := CatchRatePromptFingerprint(cases, conv, testCatchModel)
	if err != nil {
		t.Fatal(err)
	}
	reversed := make([]PlanReviewCatchCase, len(cases))
	for i, c := range cases {
		reversed[len(cases)-1-i] = c
	}
	b, err := CatchRatePromptFingerprint(reversed, conv, testCatchModel)
	if err != nil {
		t.Fatal(err)
	}
	if a != b || !strings.HasPrefix(a, "sha256:") || len(a) != len("sha256:")+64 {
		t.Fatalf("fingerprints %q / %q: want equal sha256 digests", a, b)
	}
}

func TestCatchRatePromptFingerprint_Sensitive(t *testing.T) {
	cases, conv := loadCommittedCatch(t)
	base, err := CatchRatePromptFingerprint(cases, conv, testCatchModel)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	convBytes := mustRead(t, committedCatchConventions)
	convBytes[len(convBytes)-2] ^= 0x01
	convPath := filepath.Join(dir, "conv.md")
	if err := os.WriteFile(convPath, convBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	conv2, err := LoadRepresentativeConventions(convPath)
	if err != nil {
		t.Fatal(err)
	}

	corpus := copyCatchCorpus(t)
	inputPath := filepath.Join(corpus, cases[0].Name, "review_input.json")
	if err := os.WriteFile(inputPath, append(mustRead(t, inputPath), '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	cases2, err := LoadPlanReviewCatchCorpus(corpus)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		cases []PlanReviewCatchCase
		conv  prompt.ReviewConvention
		model string
	}{
		{"conventions (one byte)", cases, conv2, testCatchModel},
		{"corpus review input (one byte)", cases2, conv, testCatchModel},
		{"generator model", cases, conv, testCatchModel + "x"},
		{"case set", cases[1:], conv, testCatchModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CatchRatePromptFingerprint(tc.cases, tc.conv, tc.model)
			if err != nil {
				t.Fatal(err)
			}
			if got == base {
				t.Fatalf("fingerprint unchanged by a %s change", tc.name)
			}
		})
	}
}

func TestCatchRatePromptFingerprint_RenderErrorPropagates(t *testing.T) {
	cases, _ := loadCommittedCatch(t)
	if _, err := CatchRatePromptFingerprint(cases, prompt.ReviewConvention{}, testCatchModel); err == nil {
		t.Fatal("fingerprint over an empty convention succeeded; want the with-arm render error")
	}
}

// ---------------------------------------------------------------------------
// README minimal record (approval condition 7).
// ---------------------------------------------------------------------------

// TestREADMEMinimalCatchRateEvidenceIsValid pins the README's documented
// minimal record to the code: it decodes strictly, carries the schema, the
// current tolerance and floor, a per_case entry for EVERY committed case and
// a pinned baseline, and passes every count invariant and both rules — so the
// documented shape cannot drift from what CheckCatchRateEvidence accepts
// (only the fingerprint is a placeholder). Adding a corpus case obliges a
// README update.
func TestREADMEMinimalCatchRateEvidenceIsValid(t *testing.T) {
	readme := string(mustRead(t, "README.md"))
	const begin, end = "<!-- BEGIN minimal-catchrate-evidence -->", "<!-- END minimal-catchrate-evidence -->"
	i, j := strings.Index(readme, begin), strings.Index(readme, end)
	if i < 0 || j < i {
		t.Fatalf("README.md lacks the %q ... %q block", begin, end)
	}
	block := readme[i+len(begin) : j]
	block = strings.TrimSpace(block)
	block = strings.TrimPrefix(block, "```json")
	block = strings.TrimSuffix(block, "```")
	rec, err := decodeCatchRateEvidence([]byte(block))
	if err != nil {
		t.Fatalf("README minimal record: %v", err)
	}
	if rec.Tolerance != DefaultCatchRateRegressionTolerance || rec.MinTrialsPerArm != MinCatchRateTrialsPerArm {
		t.Fatalf("README minimal record carries tolerance %v / min trials %d, not the current rule", rec.Tolerance, rec.MinTrialsPerArm)
	}
	if err := validateEvidenceArms(rec.SamplesPerCase, rec.Arms); err != nil {
		t.Fatalf("README minimal record: %v", err)
	}
	cases, _ := loadCommittedCatch(t)
	if missing, extra := caseSetDiff(rec.Arms.WithoutConventions.PerCase, cases); len(missing)+len(extra) > 0 {
		t.Fatalf("README minimal record case set differs from the committed corpus (missing %v, unknown %v)", missing, extra)
	}
	if rec.Baseline == nil {
		t.Fatal("README minimal record carries no pinned baseline")
	}
	if err := validateCatchRateBaseline(rec.Baseline); err != nil {
		t.Fatalf("README minimal record: %v", err)
	}
	if _, err := judgeCatchRate(*rec, rec.Baseline, false); err != nil {
		t.Fatalf("README minimal record does not pass the rule: %v", err)
	}
}
