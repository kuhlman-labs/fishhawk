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
// verdict whose concern matches every committed case's first probe, and every
// later call with a probe-free verdict.
type scriptedCatchSender struct {
	caught map[string]int
	note   string
	err    error
	calls  map[string]int
}

func newScriptedCatchSender(t *testing.T, withoutCaught, withCaught int) *scriptedCatchSender {
	t.Helper()
	cases, _ := loadCommittedCatch(t)
	probes := make([]string, 0, len(cases))
	for _, c := range cases {
		probes = append(probes, c.Input.CatchProbes[0])
	}
	return &scriptedCatchSender{
		caught: map[string]int{ArmWithoutConventions: withoutCaught, ArmWithConventions: withCaught},
		note:   "the plan's criteria are defective: " + strings.Join(probes, "; "),
		calls:  map[string]int{},
	}
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

// freshCatchEvidence records a pinned, all-caught measurement in a temp dir.
func freshCatchEvidence(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evidence.json")
	all := committedCatchTrials(t)
	if _, err := record(t, newScriptedCatchSender(t, all, all), path, catchT0, pin("initial baseline")); err != nil {
		t.Fatalf("record fresh evidence: %v", err)
	}
	return path
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
	for _, want := range []string{"without conventions:", "with conventions:", "Rule: FAIL", "Pinned baseline", "does not roll forward", "Verdict: PASS"} {
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
	}{
		{name: "(1) absent evidence file", path: func(dir string) string { return filepath.Join(dir, "nope.json") }, want: "absent: no two-arm measurement"},
		{name: "(2) malformed JSON", raw: func([]byte) []byte { return []byte("{") }, want: "malformed JSON"},
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
	}
	if trials < MinCatchRateTrialsPerArm {
		t.Fatalf("fixture premise: the committed corpus records %d trials per arm, below the floor", trials)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := freshCatchEvidence(t)
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
	if err := os.RemoveAll(filepath.Join(corpus, committedSyntheticCatchCases[0])); err != nil {
		t.Fatal(err)
	}
	n := len(committedSyntheticCatchCases) - 1
	all := SamplesPerCaseForPower(n) * n
	_, err := RecordCatchRateEvidence(context.Background(), newScriptedCatchSender(t, all, all), testCatchModel, corpus, committedCatchConventions, path, catchT0, RecordCatchRateOptions{})
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
