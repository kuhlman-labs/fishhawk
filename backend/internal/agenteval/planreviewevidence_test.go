package agenteval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/backend/internal/personaprompt"
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

// scriptedCatchSender answers the first caught[arm] calls of each
// conventions arm with a verdict whose ONE concern names every committed
// case's planted criterion id(s) and first probe — so it satisfies both
// halves of every case's catch rule — and every later call with a
// concern-free verdict. The architect arm is routed on the persona remit
// heading (catchArmOf) and answered PER CASE (the rendered persona prompt
// identifies the case, committedPersonaScript): its first caught[architect]
// catch-case calls get the case's first catching example, every later one a
// concern-free verdict; its first falseAlarms clean-case calls get the case's
// first false-alarm example, every later one a concern-free verdict. The two
// security arms (#4314) are routed on the security remit heading — the
// conventions section splits security from security_portability — and are
// answered PER CASE the same way (committedSecurityScript), with their own
// counters: the first caught[security] catch-case calls get the case's first
// catching example, and the first secFalseAlarms[arm] clean-case calls of
// each arm its first false-alarm example; secOverride, when set, answers a
// security call first. So every seeded count exists BY CONSTRUCTION.
type scriptedCatchSender struct {
	caught      map[string]int
	falseAlarms int
	note        string
	err         error
	calls       map[string]int
	cleanCalls  int
	persona     map[string]personaScriptCase
	// The security arms.
	security       map[string]personaScriptCase
	secFalseAlarms map[string]int
	secCleanCalls  map[string]int
	secOverride    func(arm string, sc personaScriptCase, call int) (string, bool)
}

func newScriptedCatchSender(t *testing.T, withoutCaught, withCaught int) *scriptedCatchSender {
	t.Helper()
	return &scriptedCatchSender{
		caught:         map[string]int{ArmWithoutConventions: withoutCaught, ArmWithConventions: withCaught, ArmArchitect: math.MaxInt, ArmSecurity: math.MaxInt},
		note:           allCaseCatchNote(t),
		calls:          map[string]int{},
		persona:        committedPersonaScript(t),
		security:       committedSecurityScript(t),
		secFalseAlarms: map[string]int{},
		secCleanCalls:  map[string]int{},
	}
}

var (
	securityScriptOnce sync.Once
	securityScript     map[string]personaScriptCase
	securityScriptErr  error
)

// committedSecurityScript maps each committed security case's RENDERED
// persona prompt, for EACH security arm that runs it, to its scripted
// answers. Computed once per test process.
func committedSecurityScript(t *testing.T) map[string]personaScriptCase {
	t.Helper()
	securityScriptOnce.Do(func() {
		cases, _, err := LoadSecurityCatchCorpus(committedSecurityCorpus, personaTestRepoRoot)
		if err != nil {
			securityScriptErr = err
			return
		}
		securityScript = map[string]personaScriptCase{}
		for _, arm := range []string{ArmSecurity, ArmSecurityPortability} {
			for _, c := range SecurityArmCases(cases, arm) {
				r, err := SecurityArmPrompt(c, personaTestRepoRoot, arm)
				if err != nil {
					securityScriptErr = err
					return
				}
				sc := personaScriptCase{name: c.Name, clean: c.Expect() == CatchExpectClean}
				if sc.clean {
					ex := c.Input.FalseAlarmExamples[0]
					sc.falseAlarm = personaVerdictJSON(ex.Category, ex.Note)
				} else {
					ex := c.Input.CatchingExamples[0]
					sc.catching = personaVerdictJSON(ex.Category, ex.Note)
				}
				securityScript[r.Prompt] = sc
			}
		}
	})
	if securityScriptErr != nil {
		t.Fatalf("committed security script: %v", securityScriptErr)
	}
	return securityScript
}

// personaScriptCase is what the scripted sender answers for one persona case.
type personaScriptCase struct {
	name       string
	clean      bool
	catching   string // a verdict carrying the case's first catching example
	falseAlarm string // a verdict carrying the case's first false-alarm example
}

var (
	personaScriptOnce sync.Once
	personaScript     map[string]personaScriptCase
	personaScriptErr  error
)

// committedPersonaScript maps each committed persona case's RENDERED persona
// prompt to its scripted answers. Computed once per test process.
func committedPersonaScript(t *testing.T) map[string]personaScriptCase {
	t.Helper()
	personaScriptOnce.Do(func() {
		cases, err := LoadPersonaCatchCorpus(committedPersonaCorpus, personaTestRepoRoot, ArmArchitect)
		if err != nil {
			personaScriptErr = err
			return
		}
		personaScript = make(map[string]personaScriptCase, len(cases))
		for _, c := range cases {
			r, err := PersonaArmPrompt(c, personaTestRepoRoot)
			if err != nil {
				personaScriptErr = err
				return
			}
			sc := personaScriptCase{name: c.Name, clean: c.Expect() == CatchExpectClean}
			if sc.clean {
				ex := c.Input.FalseAlarmExamples[0]
				sc.falseAlarm = personaVerdictJSON(ex.Category, ex.Note)
			} else {
				ex := c.Input.CatchingExamples[0]
				sc.catching = personaVerdictJSON(ex.Category, ex.Note)
			}
			personaScript[r.Prompt] = sc
		}
	})
	if personaScriptErr != nil {
		t.Fatalf("committed persona script: %v", personaScriptErr)
	}
	return personaScript
}

// personaVerdictJSON is a persona verdict with ONE medium concern.
func personaVerdictJSON(category, note string) string {
	b, _ := json.Marshal(map[string]any{"verdict": "approve_with_concerns", "concerns": []any{
		map[string]any{"severity": "medium", "category": category, "note": note},
	}})
	return string(b)
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

// catchArmOf is the arm a fake sender's user prompt belongs to: the persona
// remit heading routes to the architect arm; the security remit heading —
// checked BEFORE the conventions heading, because a security prompt carries
// both — to the security arm when the conventions section is present and the
// portability arm otherwise; the conventions heading to the with-conventions
// arm.
func catchArmOf(userText string) string {
	if strings.Contains(userText, personaprompt.Framing(ArmArchitect).Heading) {
		return ArmArchitect
	}
	if strings.Contains(userText, personaprompt.Framing(personaSecurityName).Heading) {
		if strings.Contains(userText, "### "+prompt.ReviewConventionsHeading) {
			return ArmSecurity
		}
		return ArmSecurityPortability
	}
	if strings.Contains(userText, "### "+prompt.ReviewConventionsHeading) {
		return ArmWithConventions
	}
	return ArmWithoutConventions
}

func (s *scriptedCatchSender) Messages(_ context.Context, _, userText string) (string, string, int, int, int, int, error) {
	if s.err != nil {
		return "", "", 0, 0, 0, 0, s.err
	}
	arm := catchArmOf(userText)
	if arm == ArmSecurity || arm == ArmSecurityPortability {
		sc, ok := s.security[userText]
		if !ok {
			return "", "", 0, 0, 0, 0, errors.New("scripted sender: a security prompt matching no committed security case")
		}
		n := s.calls[arm]
		s.calls[arm]++
		if s.secOverride != nil {
			if ans, ok := s.secOverride(arm, sc, n); ok {
				return ans, "fake-model", 0, 0, 0, 0, nil
			}
		}
		if sc.clean {
			cn := s.secCleanCalls[arm]
			s.secCleanCalls[arm]++
			if cn < s.secFalseAlarms[arm] {
				return sc.falseAlarm, "fake-model", 0, 0, 0, 0, nil
			}
			return `{"verdict":"approve","concerns":[]}`, "fake-model", 0, 0, 0, 0, nil
		}
		if n-s.secCleanCalls[arm] < s.caught[arm] {
			return sc.catching, "fake-model", 0, 0, 0, 0, nil
		}
		return `{"verdict":"approve","concerns":[]}`, "fake-model", 0, 0, 0, 0, nil
	}
	if arm == ArmArchitect {
		sc, ok := s.persona[userText]
		if !ok {
			return "", "", 0, 0, 0, 0, errors.New("scripted sender: an architect prompt matching no committed persona case")
		}
		if sc.clean {
			n := s.cleanCalls
			s.cleanCalls++
			if n < s.falseAlarms {
				return sc.falseAlarm, "fake-model", 0, 0, 0, 0, nil
			}
			return `{"verdict":"approve","concerns":[]}`, "fake-model", 0, 0, 0, 0, nil
		}
		n := s.calls[arm]
		s.calls[arm]++
		if n < s.caught[arm] {
			return sc.catching, "fake-model", 0, 0, 0, 0, nil
		}
		return `{"verdict":"approve","concerns":[]}`, "fake-model", 0, 0, 0, 0, nil
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
	return s.calls[ArmWithoutConventions] + s.calls[ArmWithConventions] + s.calls[ArmArchitect] + s.cleanCalls +
		s.calls[ArmSecurity] + s.calls[ArmSecurityPortability]
}

// catchInputs is the committed arm-table inputs with the conventions arms'
// corpus and fixture replaced.
func catchInputs(corpus, conventions string) CatchRateInputs {
	in := committedCatchInputs()
	in.CorpusDir, in.ConventionsPath = corpus, conventions
	return in
}

// setArmSamples sets one arm's samples_per_case.
func setArmSamples(arms CatchRateEvidenceArms, name string, n int) {
	a := arms[name]
	a.SamplesPerCase = n
	arms[name] = a
}

// committedCatchInputs is the committed arm-table inputs.
func committedCatchInputs() CatchRateInputs {
	return CatchRateInputs{
		CorpusDir:         committedCatchCorpus,
		ConventionsPath:   committedCatchConventions,
		PersonaCorpusDir:  committedPersonaCorpus,
		SecurityCorpusDir: committedSecurityCorpus,
		RepoRoot:          personaTestRepoRoot,
	}
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
	return RecordCatchRateEvidence(context.Background(), sender, testCatchModel, committedCatchInputs(), path, now, opts)
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
	report, err := CheckCatchRateEvidence(path, committedCatchInputs(), testCatchModel)
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
		{name: "(1) absent evidence file", path: func(dir string) string { return filepath.Join(dir, "nope.json") }, want: "absent: no arm-keyed measurement"},
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
		{name: "(6) samples_per_case below 1", mutate: func(r *CatchRateEvidence) { setArmSamples(r.Arms, ArmWithoutConventions, 0) }, want: "must be >= 1"},
		{name: "(6) negative count", mutate: func(r *CatchRateEvidence) {
			m := r.Arms[ArmWithConventions].PerCase
			c := m[firstCase(m)]
			c.Undecodable = -1
			m[firstCase(m)] = c
		}, want: "inconsistent counts in arm"},
		{name: "(6) caught+undecodable > trials, every count in bounds, totals >= 136", mutate: func(r *CatchRateEvidence) {
			m := r.Arms[ArmWithoutConventions].PerCase
			c := m[firstCase(m)]
			c.Caught, c.Undecodable = c.Trials-3, 5 // each <= trials; the sum is not
			m[firstCase(m)] = c
		}, want: "inconsistent counts in arm"},
		{name: "(6) per-case trials != samples_per_case, arms agree, totals >= 136", mutate: func(r *CatchRateEvidence) {
			for _, m := range []map[string]CaseCatchCounts{r.Arms[ArmWithoutConventions].PerCase, r.Arms[ArmWithConventions].PerCase} {
				names := sortedCaseNames(m)
				a, b := m[names[0]], m[names[1]]
				a.Trials, a.Caught = samples-1, samples-1
				b.Trials, b.Caught = samples+1, samples+1
				m[names[0]], m[names[1]] = a, b
			}
		}, want: "not samples_per_case"},
		{name: "(6) arms disagree on the case set: with arm lacks a case", mutate: func(r *CatchRateEvidence) {
			m := r.Arms[ArmWithConventions].PerCase
			delete(m, firstCase(m))
		}, want: "disagree on the case set"},
		{name: "(6) arms disagree on the case set: with arm carries an extra case, totals >= 136", mutate: func(r *CatchRateEvidence) {
			r.Arms[ArmWithConventions].PerCase["zz-not-in-without-arm"] = CaseCatchCounts{Trials: samples, Caught: samples}
		}, want: "disagree on the case set"},
		{name: "(6) arms disagree on per-case trial weights, totals >= 136", mutate: func(r *CatchRateEvidence) {
			m := r.Arms[ArmWithoutConventions].PerCase
			names := sortedCaseNames(m)
			a, b := m[names[0]], m[names[1]]
			a.Trials, a.Caught = samples-1, samples-1
			b.Trials, b.Caught = samples+1, samples+1
			m[names[0]], m[names[1]] = a, b
		}, want: "trial weight"},
		{name: "(7) case set differs from the corpus (baseline agrees)", mutate: func(r *CatchRateEvidence) {
			for _, m := range []map[string]CaseCatchCounts{
				r.Arms[ArmWithoutConventions].PerCase, r.Arms[ArmWithConventions].PerCase,
				r.Baseline.Arms[ArmWithoutConventions].PerCase, r.Baseline.Arms[ArmWithConventions].PerCase,
			} {
				renameCase(m, firstCase(m), "zz-retired-case")
			}
		}, want: "differs from the current corpus"},
		{name: "(8) under-powered arms", mutate: func(r *CatchRateEvidence) {
			setArmSamples(r.Arms, ArmWithoutConventions, 1)
			setArmSamples(r.Arms, ArmWithConventions, 1)
			for _, m := range []map[string]CaseCatchCounts{r.Arms[ArmWithoutConventions].PerCase, r.Arms[ArmWithConventions].PerCase} {
				for name := range m {
					m[name] = CaseCatchCounts{Trials: 1, Caught: 1}
				}
			}
		}, want: "under-powered"},
		{name: "(9) within-run regression, recomputed from counts (baseline agrees)", mutate: func(r *CatchRateEvidence) {
			// 0.30 below the without arm; the baseline's with arm is moved to
			// match, so only the within-run rule can refuse it.
			setCaught(r.Arms[ArmWithConventions].PerCase, int(math.Round(0.7*float64(samples))))
			setCaught(r.Baseline.Arms[ArmWithConventions].PerCase, int(math.Round(0.7*float64(samples))))
		}, want: "below the without-conventions arm", regressed: true},
		{name: "(10) missing baseline", mutate: func(r *CatchRateEvidence) { r.Baseline = nil }, want: "no pinned baseline"},
		{name: "(10) baseline without a reason", mutate: func(r *CatchRateEvidence) { r.Baseline.Reason = " " }, want: "no reason"},
		{name: "(10) baseline without a timestamp", mutate: func(r *CatchRateEvidence) { r.Baseline.PinnedAt = "yesterday" }, want: "RFC 3339"},
		{name: "(10) baseline with inconsistent counts", mutate: func(r *CatchRateEvidence) {
			m := r.Baseline.Arms[ArmWithoutConventions].PerCase
			c := m[firstCase(m)]
			c.Caught = c.Trials + 1
			m[firstCase(m)] = c
		}, want: "pinned baseline: inconsistent counts"},
		{name: "(10) baseline from another generator model", mutate: func(r *CatchRateEvidence) { r.Baseline.GeneratorModel = "another-model" }, want: "not comparable"},
		{name: "(10) baseline from another corpus", mutate: func(r *CatchRateEvidence) {
			for _, m := range []map[string]CaseCatchCounts{r.Baseline.Arms[ArmWithoutConventions].PerCase, r.Baseline.Arms[ArmWithConventions].PerCase} {
				renameCase(m, firstCase(m), "zz-retired-case")
			}
		}, want: "corpus changed"},
		{name: "(10) baseline with another samples_per_case", mutate: func(r *CatchRateEvidence) {
			setArmSamples(r.Baseline.Arms, ArmWithoutConventions, samples+1)
			setArmSamples(r.Baseline.Arms, ArmWithConventions, samples+1)
			for _, m := range []map[string]CaseCatchCounts{r.Baseline.Arms[ArmWithoutConventions].PerCase, r.Baseline.Arms[ArmWithConventions].PerCase} {
				for name := range m {
					m[name] = CaseCatchCounts{Trials: samples + 1, Caught: samples + 1}
				}
			}
		}, want: "samples_per_case changed"},
		{name: "(10) both arms equal but 0.17 below the pinned baseline", mutate: func(r *CatchRateEvidence) {
			setCaught(r.Arms[ArmWithoutConventions].PerCase, int(math.Round(0.83*float64(samples))))
			setCaught(r.Arms[ArmWithConventions].PerCase, int(math.Round(0.83*float64(samples))))
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
			m := tr.Arms[ArmWithoutConventions].PerCase
			name := lastTrialCase(m)
			list := m[name]
			if list[len(list)-1].Outcome != CatchMissed {
				t.Fatalf("fixture premise: the last trial of %q must be missed, got %q", name, list[len(list)-1].Outcome)
			}
			m[name] = list[:len(list)-1]
		}, want: "trials, the record"},
		{name: "(16) trial indices: a duplicated index", trials: func(_ *testing.T, tr *CatchRateTrials) {
			list := tr.Arms[ArmWithConventions].PerCase[sortedTrialCaseNames(tr.Arms[ArmWithConventions].PerCase)[0]]
			list[1].Trial = list[0].Trial
		}, want: "trial index 1, want 2"},
		{name: "(16) outcome tallies: a caught trial flipped to missed", trials: func(t *testing.T, tr *CatchRateTrials) {
			list := tr.Arms[ArmWithoutConventions].PerCase[sortedTrialCaseNames(tr.Arms[ArmWithoutConventions].PerCase)[0]]
			if list[0].Outcome != CatchCaught {
				t.Fatalf("fixture premise: the first trial must be caught, got %q", list[0].Outcome)
			}
			list[0].Outcome = CatchMissed
		}, want: "outcome tallies"},
		{name: "(16) outcome tallies: an unknown outcome", trials: func(t *testing.T, tr *CatchRateTrials) {
			m := tr.Arms[ArmWithConventions].PerCase
			list := m[lastTrialCase(m)]
			if list[len(list)-1].Outcome != CatchMissed {
				t.Fatalf("fixture premise: the last trial must be missed, got %q", list[len(list)-1].Outcome)
			}
			list[len(list)-1].Outcome = "maybe"
		}, want: "outcome tallies"},
		{name: "(16) case set: an extra case in one arm", trials: func(_ *testing.T, tr *CatchRateTrials) {
			tr.Arms[ArmWithoutConventions].PerCase["zz-not-recorded"] = []CatchRateTrialRecord{{Trial: 1, Outcome: CatchMissed, Concerns: []CatchRateTrialConcern{}}}
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
				if _, err := CheckCatchRateEvidence(path, committedCatchInputs(), testCatchModel); err != nil {
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
			_, err := CheckCatchRateEvidence(path, catchInputs(corpus, conv), model)
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
	if _, err := RecordCatchRateEvidence(context.Background(), newScriptedCatchSender(t, all, all), testCatchModel, catchInputs(committedCatchCorpus, convPath), path, catchT0, pin("baseline")); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckCatchRateEvidence(path, catchInputs(committedCatchCorpus, convPath), testCatchModel); err != nil {
		t.Fatalf("premise: the record is fresh against its own conventions: %v", err)
	}
	_, err := CheckCatchRateEvidence(path, committedCatchInputs(), testCatchModel)
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
	if _, caught := pooledCounts(rec.Arms[ArmWithConventions].PerCase); caught != k {
		t.Errorf("with-arm caught = %d, want %d", caught, k)
	}
	if _, err := CheckCatchRateEvidence(path, committedCatchInputs(), testCatchModel); err != nil {
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
	if _, caught := pooledCounts(b.Arms[ArmWithoutConventions].PerCase); caught != k {
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
		_, err := RecordCatchRateEvidence(context.Background(), newScriptedCatchSender(t, all, all), " ", catchInputs(committedCatchCorpus, committedCatchConventions), filepath.Join(t.TempDir(), "e.json"), catchT0, pin("x"))
		if err == nil || !strings.Contains(err.Error(), "generator model") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("corpus unavailable", func(t *testing.T) {
		_, err := RecordCatchRateEvidence(context.Background(), newScriptedCatchSender(t, all, all), testCatchModel, catchInputs("testdata/no-such-corpus", committedCatchConventions), filepath.Join(t.TempDir(), "e.json"), catchT0, pin("x"))
		if err == nil || !strings.Contains(err.Error(), "absent") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("conventions unavailable", func(t *testing.T) {
		_, err := RecordCatchRateEvidence(context.Background(), newScriptedCatchSender(t, all, all), testCatchModel, catchInputs(committedCatchCorpus, "testdata/no-such.md"), filepath.Join(t.TempDir(), "e.json"), catchT0, pin("x"))
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
	_, err = RecordCatchRateEvidence(context.Background(), newScriptedCatchSender(t, all, all), testCatchModel, catchInputs(corpus, committedCatchConventions), path, catchT0, RecordCatchRateOptions{})
	if err == nil || !strings.Contains(err.Error(), "not comparable") || !strings.Contains(err.Error(), "corpus changed") {
		t.Fatalf("err = %v, want a non-comparable-baseline refusal", err)
	}
	if !bytes.Equal(mustRead(t, path), before) {
		t.Fatal("the refusal changed the evidence file")
	}
	report, err := RecordCatchRateEvidence(context.Background(), newScriptedCatchSender(t, all, all), testCatchModel, catchInputs(corpus, committedCatchConventions), path, catchT0, pin("corpus retired a case"))
	if err != nil {
		t.Fatalf("re-pin after a corpus change: %v", err)
	}
	if !strings.Contains(report, "not comparable") || !strings.Contains(report, "the pin replaces it") {
		t.Errorf("report does not explain the replaced baseline:\n%s", report)
	}
	if got := readCatchEvidence(t, path).Baseline.Arms[ArmWithoutConventions].SamplesPerCase; got != SamplesPerCaseForPower(n) {
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
		{ArmWithoutConventions, rec.Arms[ArmWithoutConventions].PerCase, tr.Arms[ArmWithoutConventions].PerCase},
		{ArmWithConventions, rec.Arms[ArmWithConventions].PerCase, tr.Arms[ArmWithConventions].PerCase},
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
	report, err := CheckCatchRateEvidence(path, committedCatchInputs(), testCatchModel)
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
		ArmWithoutConventions: tr.Arms[ArmWithoutConventions].PerCase,
		ArmWithConventions:    tr.Arms[ArmWithConventions].PerCase,
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
	for _, list := range tr.Arms[ArmWithoutConventions].PerCase {
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
	for _, list := range tr.Arms[ArmWithConventions].PerCase {
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
	for _, list := range tr.Arms[ArmWithoutConventions].PerCase {
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
	a, err := CatchRatePromptFingerprint(committedCatchInputs(), testCatchModel)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CatchRatePromptFingerprint(committedCatchInputs(), testCatchModel)
	if err != nil {
		t.Fatal(err)
	}
	if a != b || !strings.HasPrefix(a, "sha256:") || len(a) != len("sha256:")+64 {
		t.Fatalf("fingerprints %q / %q: want equal sha256 digests", a, b)
	}
	// The per-arm fingerprint is case-order independent.
	cases, conv := loadCommittedCatch(t)
	reversed := make([]PlanReviewCatchCase, len(cases))
	for i, c := range cases {
		reversed[len(cases)-1-i] = c
	}
	x, err := conventionsArmFingerprint(cases, conv, ArmWithConventions)
	if err != nil {
		t.Fatal(err)
	}
	y, err := conventionsArmFingerprint(reversed, conv, ArmWithConventions)
	if err != nil {
		t.Fatal(err)
	}
	if x != y {
		t.Fatalf("arm fingerprint depends on case order: %q / %q", x, y)
	}
}

func TestCatchRatePromptFingerprint_Sensitive(t *testing.T) {
	base, err := CatchRatePromptFingerprint(committedCatchInputs(), testCatchModel)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cases, _ := loadCommittedCatch(t)

	convBytes := mustRead(t, committedCatchConventions)
	convBytes[len(convBytes)-2] ^= 0x01
	convPath := filepath.Join(dir, "conv.md")
	if err := os.WriteFile(convPath, convBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	corpus := copyCatchCorpus(t)
	inputPath := filepath.Join(corpus, cases[0].Name, "review_input.json")
	if err := os.WriteFile(inputPath, append(mustRead(t, inputPath), '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	fewer := copyCatchCorpus(t)
	if err := os.RemoveAll(filepath.Join(fewer, committedSyntheticCatchCases(t)[0])); err != nil {
		t.Fatal(err)
	}
	persona := copyPersonaRepo(t)
	remit := filepath.Join(persona.RepoRoot, filepath.FromSlash(personaArchitectRemitPath))
	rb := mustRead(t, remit)
	rb[len(rb)-2] ^= 0x01
	if err := os.WriteFile(remit, rb, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		in    CatchRateInputs
		model string
	}{
		{"conventions (one byte)", catchInputs(committedCatchCorpus, convPath), testCatchModel},
		{"corpus review input (one byte)", catchInputs(corpus, committedCatchConventions), testCatchModel},
		{"generator model", committedCatchInputs(), testCatchModel + "x"},
		{"case set", catchInputs(fewer, committedCatchConventions), testCatchModel},
		{"architect remit (one byte)", persona, testCatchModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CatchRatePromptFingerprint(tc.in, tc.model)
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
	if _, err := conventionsArmFingerprint(cases, prompt.ReviewConvention{}, ArmWithConventions); err == nil {
		t.Fatal("fingerprint over an empty convention succeeded; want the with-arm render error")
	}
	in := committedCatchInputs()
	in.PersonaCorpusDir = "testdata/no-such-persona-corpus"
	if _, err := CatchRatePromptFingerprint(in, testCatchModel); err == nil || !strings.Contains(err.Error(), "persona corpus unavailable") {
		t.Fatalf("err = %v, want the persona corpus refusal", err)
	}
	cases2 := loadCommittedPersona(t)
	broken := func(PersonaCatchCase, string) (PersonaArmRender, error) {
		return PersonaArmRender{}, errors.New("render down")
	}
	if _, err := personaArmFingerprint(cases2, personaTestRepoRoot, broken); err == nil || !strings.Contains(err.Error(), "render down") {
		t.Fatalf("err = %v, want the persona render error", err)
	}
	if _, err := catchRateArmTableWith(committedCatchInputs(), broken, SecurityArmPrompt); err == nil || !strings.Contains(err.Error(), "render down") {
		t.Fatalf("err = %v, want the arm table to propagate the persona render error", err)
	}
	secBroken := func(SecurityCatchCase, string, string) (SecurityArmRender, error) {
		return SecurityArmRender{}, errors.New("security render down")
	}
	if _, err := catchRateArmTableWith(committedCatchInputs(), PersonaArmPrompt, secBroken); err == nil || !strings.Contains(err.Error(), "security render down") || !strings.Contains(err.Error(), `arm "security"`) {
		t.Fatalf("err = %v, want the arm table to propagate the security render error naming the arm", err)
	}
	in = committedCatchInputs()
	in.SecurityCorpusDir = "testdata/no-such-security-corpus"
	if _, err := CatchRatePromptFingerprint(in, testCatchModel); err == nil || !strings.Contains(err.Error(), "security corpus unavailable") {
		t.Fatalf("err = %v, want the security corpus refusal", err)
	}
}

// copyPersonaRepo copies the documents the architect arm reads (docs/review
// and docs/adr) into a temp checkout root and returns the committed inputs
// pointed at it.
func copyPersonaRepo(t *testing.T) CatchRateInputs {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{"docs/review", "docs/adr"} {
		src := filepath.Join(personaTestRepoRoot, filepath.FromSlash(rel))
		err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			r, _ := filepath.Rel(src, p)
			target := filepath.Join(root, filepath.FromSlash(rel), r)
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
	}
	in := committedCatchInputs()
	in.RepoRoot = root
	return in
}

// ---------------------------------------------------------------------------
// README minimal record (approval condition 7).
// ---------------------------------------------------------------------------

// TestREADMEMinimalCatchRateEvidenceIsValid pins the README's documented
// minimal record to the code. A v2 block must decode strictly, carry the
// schema, the current tolerance and floor, every arm of the arm table with a
// per_case entry for EVERY committed case of that arm and a pinned baseline,
// and pass every count invariant and every rule — so the documented shape
// cannot drift from what CheckCatchRateEvidence accepts (only the
// fingerprints are placeholders). A LEGACY v1 block (the README before the
// #4313 docs slice rewrites it) must be recognised by the version-dispatching
// reader as legacy — never accepted, never mistaken for a malformed v2 record.
// Adding a corpus case obliges a README update.
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
	if errors.Is(err, errLegacyCatchRateEvidence) {
		t.Logf("README minimal record is the LEGACY %s shape: recognised as legacy; the #4313 docs slice rewrites it as a v2 record", catchRateEvidenceSchemaV1)
		return
	}
	if err != nil {
		t.Fatalf("README minimal record: %v", err)
	}
	if rec.Tolerance != DefaultCatchRateRegressionTolerance || rec.MinTrialsPerArm != MinCatchRateTrialsPerArm {
		t.Fatalf("README minimal record carries tolerance %v / min trials %d, not the current rule", rec.Tolerance, rec.MinTrialsPerArm)
	}
	if err := validateEvidenceArms(rec.Arms); err != nil {
		t.Fatalf("README minimal record: %v", err)
	}
	rows, err := catchRateArmTable(committedCatchInputs())
	if err != nil {
		t.Fatal(err)
	}
	if err := checkArmSet(rec.Arms, rows); err != nil {
		t.Fatalf("README minimal record: %v", err)
	}
	for _, row := range rows {
		if err := checkArmCases(rec.Arms[row.name].PerCase, row); err != nil {
			t.Fatalf("README minimal record: %v", err)
		}
		if err := checkArmPartition(rec.Arms[row.name], row); err != nil {
			t.Fatalf("README minimal record: %v", err)
		}
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

// ---------------------------------------------------------------------------
// Arm-keyed v2 (E78.11 / #4313): the per-arm modes.
// ---------------------------------------------------------------------------

// personaCaseNames returns the first committed catch and clean persona case.
func personaCaseNames(t *testing.T) (catchCase, cleanCase string) {
	t.Helper()
	for _, c := range loadCommittedPersona(t) {
		if c.Expect() == CatchExpectClean && cleanCase == "" {
			cleanCase = c.Name
		}
		if c.Expect() == CatchExpectCatch && catchCase == "" {
			catchCase = c.Name
		}
	}
	if catchCase == "" || cleanCase == "" {
		t.Fatal("the committed persona corpus lacks a catch or a clean case")
	}
	return catchCase, cleanCase
}

func setCounts(arms CatchRateEvidenceArms, arm, name string, f func(*CaseCatchCounts)) {
	c := arms[arm].PerCase[name]
	f(&c)
	arms[arm].PerCase[name] = c
}

// TestCheckCatchRateEvidence_PerArmModes is one row per new v2 mode, each on
// a fresh all-arm record with one perturbation.
func TestCheckCatchRateEvidence_PerArmModes(t *testing.T) {
	catchCase, cleanCase := personaCaseNames(t)
	for _, tc := range []struct {
		name      string
		mutate    func(r *CatchRateEvidence)
		raw       func([]byte) []byte
		trials    func(t *testing.T, tr *CatchRateTrials)
		want      string
		regressed bool
	}{
		{name: "(3b) a legacy v1 record names the shared re-recording", raw: func([]byte) []byte {
			return []byte(`{"schema":"planreview-catchrate-evidence-v1","samples_per_case":12,"arms":{}}`)
		}, want: "#4313/#4314 re-pin recording"},
		{name: "(6b) false_alarms on a catch case", mutate: func(r *CatchRateEvidence) {
			setCounts(r.Arms, ArmArchitect, catchCase, func(c *CaseCatchCounts) { c.Caught, c.FalseAlarms = c.Trials-1, 1 })
		}, want: "on a catch case"},
		{name: "(6b) caught on a clean case", mutate: func(r *CatchRateEvidence) {
			setCounts(r.Arms, ArmArchitect, cleanCase, func(c *CaseCatchCounts) { c.Caught = 1 })
		}, want: "on a clean case"},
		{name: "(6b) an expect differing from the corpus", mutate: func(r *CatchRateEvidence) {
			setCounts(r.Arms, ArmArchitect, cleanCase, func(c *CaseCatchCounts) { c.Expect = CatchExpectCatch })
		}, want: `records expect "catch", the corpus "clean"`},
		{name: "(7b) a missing architect arm", mutate: func(r *CatchRateEvidence) { delete(r.Arms, ArmArchitect) }, want: `arm "architect" is missing`},
		{name: "(7b) an unknown arm", mutate: func(r *CatchRateEvidence) { r.Arms["zz_retired_arm"] = r.Arms[ArmArchitect] }, want: `unknown arm "zz_retired_arm"`},
		{name: "(7b) a missing security arm", mutate: func(r *CatchRateEvidence) { delete(r.Arms, ArmSecurity) }, want: `arm "security" is missing`},
		{name: "(7b) a missing security_portability arm", mutate: func(r *CatchRateEvidence) { delete(r.Arms, ArmSecurityPortability) }, want: `arm "security_portability" is missing`},
		{name: "(7b) an arm kind differing from the arm table", mutate: func(r *CatchRateEvidence) {
			a := r.Arms[ArmArchitect]
			a.Kind = CatchRateArmKindConventions
			r.Arms[ArmArchitect] = a
		}, want: `kind "conventions" is not the arm table's "persona"`},
		{name: "(7) an architect case set differing from the corpus", mutate: func(r *CatchRateEvidence) {
			for _, m := range []map[string]CaseCatchCounts{r.Arms[ArmArchitect].PerCase, r.Baseline.Arms[ArmArchitect].PerCase} {
				renameCase(m, cleanCase, "zz-retired-persona-case")
			}
		}, want: `arm "architect" case set differs from the current corpus`},
		{name: "(10d) an arm set differing from the baseline's", mutate: func(r *CatchRateEvidence) {
			delete(r.Baseline.Arms, ArmArchitect)
		}, want: "arm set changed"},
		{name: "(10) an architect samples_per_case differing from the baseline's", mutate: func(r *CatchRateEvidence) {
			b := r.Baseline.Arms[ArmArchitect]
			b.SamplesPerCase++
			for name, c := range b.PerCase {
				c.Trials++
				if c.IsClean() {
					c.FalseAlarms = 0
				} else {
					c.Caught = c.Trials
				}
				b.PerCase[name] = c
			}
			r.Baseline.Arms[ArmArchitect] = b
		}, want: `arm "architect" samples_per_case changed`},
		{name: "(10b) an architect catch-rate drop", mutate: func(r *CatchRateEvidence) {
			for name, c := range r.Arms[ArmArchitect].PerCase {
				if !c.IsClean() {
					c.Caught = c.Trials - 6 // 18 of 138 fewer: 0.130 below the pinned 1.000
					r.Arms[ArmArchitect].PerCase[name] = c
				}
			}
		}, want: "architect catch rate", regressed: true},
		{name: "(16b) a clean case's sidecar tally mismatch", trials: func(t *testing.T, tr *CatchRateTrials) {
			list := tr.Arms[ArmArchitect].PerCase[cleanCase]
			if list[0].Outcome != CatchNoFalseAlarm {
				t.Fatalf("fixture premise: the first clean trial is no_false_alarm, got %q", list[0].Outcome)
			}
			list[0].Outcome = CatchFalseAlarm
		}, want: "outcome tallies"},
		// EVERY trial of the clean case relabelled missed: the catch-class sum
		// (caught+missed+undecodable) then equals the list length, so only the
		// clean-class tally refuses it.
		{name: "(16b) a catch-class outcome on a clean case's sidecar", trials: func(_ *testing.T, tr *CatchRateTrials) {
			for i := range tr.Arms[ArmArchitect].PerCase[cleanCase] {
				tr.Arms[ArmArchitect].PerCase[cleanCase][i].Outcome = CatchMissed
			}
		}, want: "outcome tallies"},
		{name: "(16) a sidecar arm set differing from the record's", trials: func(_ *testing.T, tr *CatchRateTrials) {
			delete(tr.Arms, ArmArchitect)
		}, want: "arm set"},
	} {
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
			if tc.trials != nil {
				tr := readCatchTrials(t, path)
				tc.trials(t, &tr)
				writeCatchTrials(t, path, tr)
			}
			_, err := CheckCatchRateEvidence(path, committedCatchInputs(), testCatchModel)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a refusal naming %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), catchRateRunbook) {
				t.Errorf("err = %v, want it to name the run-book", err)
			}
			if got := errors.Is(err, ErrCatchRateRegressed); got != tc.regressed {
				t.Errorf("errors.Is(err, ErrCatchRateRegressed) = %v, want %v (err %v)", got, tc.regressed, err)
			}
		})
	}
}

// TestCheckCatchRateEvidence_PerArmStaleness is mode (4b): a record taken
// against a checkout copy, then ONE byte of the copied architect remit edited.
// The gate fails STALE naming the architect arm only — the conventions arms
// are fresh.
func TestCheckCatchRateEvidence_PerArmStaleness(t *testing.T) {
	in := copyPersonaRepo(t)
	path := filepath.Join(t.TempDir(), "evidence.json")
	all := committedCatchTrials(t)
	sender := newScriptedCatchSender(t, all, all)
	sender.persona = personaScriptFor(t, in.RepoRoot)
	if _, err := RecordCatchRateEvidence(context.Background(), sender, testCatchModel, in, path, catchT0, pin("baseline")); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckCatchRateEvidence(path, in, testCatchModel); err != nil {
		t.Fatalf("premise: the record is fresh against its own checkout: %v", err)
	}
	remit := filepath.Join(in.RepoRoot, filepath.FromSlash(personaArchitectRemitPath))
	b := mustRead(t, remit)
	b[len(b)-2] ^= 0x01
	if err := os.WriteFile(remit, b, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := CheckCatchRateEvidence(path, in, testCatchModel)
	if err == nil || !strings.Contains(err.Error(), "STALE arm(s) architect:") {
		t.Fatalf("err = %v, want a STALE refusal naming ONLY the architect arm", err)
	}
}

// personaScriptFor is committedPersonaScript over another checkout root (the
// rendered prompts differ only by that checkout's documents).
func personaScriptFor(t *testing.T, repoRoot string) map[string]personaScriptCase {
	t.Helper()
	out := map[string]personaScriptCase{}
	for _, c := range loadCommittedPersona(t) {
		r, err := PersonaArmPrompt(c, repoRoot)
		if err != nil {
			t.Fatal(err)
		}
		sc := personaScriptCase{name: c.Name, clean: c.Expect() == CatchExpectClean}
		if sc.clean {
			sc.falseAlarm = personaVerdictJSON(c.Input.FalseAlarmExamples[0].Category, c.Input.FalseAlarmExamples[0].Note)
		} else {
			sc.catching = personaVerdictJSON(c.Input.CatchingExamples[0].Category, c.Input.CatchingExamples[0].Note)
		}
		out[r.Prompt] = sc
	}
	return out
}

// TestRecordCatchRateEvidence_FalseAlarmRiseRegresses is mode (10c): pin with
// zero false alarms, then re-record with 16 of the 138 clean trials
// false-alarming (0.116 above the pinned 0.000). The recording is REFUSED for
// the regression reason and the files stay byte-identical.
func TestRecordCatchRateEvidence_FalseAlarmRiseRegresses(t *testing.T) {
	path := freshCatchEvidence(t)
	before, beforeTrials := mustRead(t, path), mustRead(t, CatchRateTrialsPath(path))
	all := committedCatchTrials(t)
	sender := newScriptedCatchSender(t, all, all)
	sender.falseAlarms = 16
	report, err := record(t, sender, path, catchT0.Add(time.Hour), RecordCatchRateOptions{})
	if err == nil || !errors.Is(err, ErrCatchRateRegressed) || !strings.Contains(err.Error(), "architect false-alarm rate") {
		t.Fatalf("err = %v, want a false-alarm-rate regression of the architect arm", err)
	}
	if !strings.Contains(report, "false alarm 16/138") {
		t.Errorf("report does not carry the measured false alarms:\n%s", report)
	}
	if !bytes.Equal(mustRead(t, path), before) || !bytes.Equal(mustRead(t, CatchRateTrialsPath(path)), beforeTrials) {
		t.Fatal("a refused recording changed the evidence pair")
	}
	t.Run("a rise of at most the tolerance passes", func(t *testing.T) {
		sender := newScriptedCatchSender(t, all, all)
		sender.falseAlarms = 13 // 13/138 = 0.094
		if _, err := record(t, sender, path, catchT0.Add(2*time.Hour), RecordCatchRateOptions{}); err != nil {
			t.Fatalf("a 0.094 false-alarm rise was refused: %v", err)
		}
	})
}

// judgeFixture is a record with a powered conventions pair (12 x 12, all
// caught) and an architect arm of nCatch catch cases and nClean clean cases
// at samples each, with the given pooled caught / false alarms spread over
// the cases.
func judgeFixture(samples, nCatch, nClean, caught, alarms int) CatchRateEvidence {
	conv := func() CatchRateEvidenceArm {
		pc := map[string]CaseCatchCounts{}
		for i := 0; i < 12; i++ {
			pc[fmt.Sprintf("c%02d", i)] = CaseCatchCounts{Trials: 12, Caught: 12}
		}
		return CatchRateEvidenceArm{Kind: CatchRateArmKindConventions, SamplesPerCase: 12, PerCase: pc}
	}
	arch := CatchRateEvidenceArm{Kind: CatchRateArmKindPersona, SamplesPerCase: samples, PerCase: map[string]CaseCatchCounts{}}
	for i := 0; i < nCatch; i++ {
		n := min(caught, samples)
		caught -= n
		arch.PerCase[fmt.Sprintf("k%02d", i)] = CaseCatchCounts{Expect: CatchExpectCatch, Trials: samples, Caught: n}
	}
	for i := 0; i < nClean; i++ {
		n := min(alarms, samples)
		alarms -= n
		arch.PerCase[fmt.Sprintf("z%02d", i)] = CaseCatchCounts{Expect: CatchExpectClean, Trials: samples, FalseAlarms: n}
	}
	return CatchRateEvidence{
		GeneratorModel: testCatchModel,
		Arms:           CatchRateEvidenceArms{ArmWithoutConventions: conv(), ArmWithConventions: conv(), ArmArchitect: arch},
	}
}

func baselineOf(rec CatchRateEvidence) *CatchRateBaseline {
	return &CatchRateBaseline{PinnedAt: "2026-10-07T00:00:00Z", Reason: "fixture", GeneratorModel: rec.GeneratorModel, Arms: cloneEvidenceArms(rec.Arms)}
}

// TestJudgeCatchRate_PerArmBaselineBoundary is mode (10b) at the boundary:
// an architect catch-rate drop of EXACTLY 0.10 passes, one trial more fails;
// the same for a false-alarm rise. Exact rationals over 3 x 50 trials.
func TestJudgeCatchRate_PerArmBaselineBoundary(t *testing.T) {
	ref := baselineOf(judgeFixture(50, 3, 3, 150, 0))
	for _, tc := range []struct {
		name           string
		caught, alarms int
		want           string
	}{
		{"catch drop exactly 0.10", 135, 0, ""},
		{"catch drop above 0.10", 134, 0, "architect catch rate"},
		{"false-alarm rise exactly 0.10", 150, 15, ""},
		{"false-alarm rise above 0.10", 150, 16, "architect false-alarm rate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := judgeCatchRate(judgeFixture(50, 3, 3, tc.caught, tc.alarms), ref, false)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("judgeCatchRate: %v", err)
				}
				return
			}
			if err == nil || !errors.Is(err, ErrCatchRateRegressed) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a regression naming %q", err, tc.want)
			}
		})
	}
}

// TestJudgeCatchRate_UnderPoweredClass is mode (8b): the catch class clears
// the floor (3 x 50 = 150) but the clean class does not (1 x 50); the
// measurement is refused naming the clean class.
func TestJudgeCatchRate_UnderPoweredClass(t *testing.T) {
	_, err := judgeCatchRate(judgeFixture(50, 3, 1, 150, 0), nil, false)
	if err == nil || !strings.Contains(err.Error(), `catch arm "architect" clean class is under-powered: 50 trials < 136`) {
		t.Fatalf("err = %v, want the clean class refused as under-powered", err)
	}
	if _, err := judgeCatchRate(judgeFixture(50, 3, 3, 150, 0), nil, false); err != nil {
		t.Fatalf("premise: both classes powered passes: %v", err)
	}
}

// TestRecordCatchRateEvidence_V1ExistingRequiresPin: over a legacy v1 record a
// non-pinning recording is refused before any model call with both files
// byte-identical (read back after the call); a dry run proceeds and writes
// nothing; a pin replaces the pair with a v2 one.
func TestRecordCatchRateEvidence_V1ExistingRequiresPin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evidence.json")
	legacy := []byte(`{"schema":"planreview-catchrate-evidence-v1","samples_per_case":12,"arms":{"without_conventions":{"per_case":{}},"with_conventions":{"per_case":{}}}}` + "\n")
	legacyTrials := []byte(`{"schema":"planreview-catchrate-trials-v1"}` + "\n")
	if err := os.WriteFile(path, legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(CatchRateTrialsPath(path), legacyTrials, 0o644); err != nil {
		t.Fatal(err)
	}
	all := committedCatchTrials(t)
	sender := newScriptedCatchSender(t, all, all)
	_, err := record(t, sender, path, catchT0, RecordCatchRateOptions{})
	if err == nil || !strings.Contains(err.Error(), "legacy planreview-catchrate-evidence-v1") || !strings.Contains(err.Error(), "must pin") {
		t.Fatalf("err = %v, want the legacy record to require a pin", err)
	}
	if sender.total() != 0 {
		t.Errorf("%d model calls were made before the refusal", sender.total())
	}
	if !bytes.Equal(mustRead(t, path), legacy) || !bytes.Equal(mustRead(t, CatchRateTrialsPath(path)), legacyTrials) {
		t.Fatal("the refusal changed the legacy pair")
	}
	report, err := record(t, newScriptedCatchSender(t, all, all), path, catchT0, RecordCatchRateOptions{DryRun: true})
	if err != nil || !strings.Contains(report, "legacy") || !strings.Contains(report, "Dry run") {
		t.Fatalf("dry run over a legacy record: err %v, report:\n%s", err, report)
	}
	if !bytes.Equal(mustRead(t, path), legacy) {
		t.Fatal("a dry run changed the legacy record")
	}
	report, err = record(t, newScriptedCatchSender(t, all, all), path, catchT0, pin("shared #4313/#4314 re-pin"))
	if err != nil {
		t.Fatalf("pin over a legacy record: %v", err)
	}
	if !strings.Contains(report, "the pin replaces it") {
		t.Errorf("report does not explain the replaced legacy record:\n%s", report)
	}
	if rec := readCatchEvidence(t, path); rec.Schema != CatchRateEvidenceSchema || rec.Baseline == nil || len(rec.Arms) != 5 {
		t.Fatalf("pinned record = schema %q, baseline %v, %d arms; want a v2 record with five arms and a baseline", rec.Schema, rec.Baseline != nil, len(rec.Arms))
	}
	if _, err := CheckCatchRateEvidence(path, committedCatchInputs(), testCatchModel); err != nil {
		t.Fatalf("the pinned v2 pair does not pass the gate: %v", err)
	}
}

// TestCatchRuleV5_CommittedSidecarRescoresUnchanged: catch rule v5 is inert on
// the conventions arms' v4 rules — re-scoring the committed sidecar gains and
// loses nothing in either conventions arm. Holds for the v1 pair and for the
// v2 pair the shared recording writes.
func TestCatchRuleV5_CommittedSidecarRescoresUnchanged(t *testing.T) {
	r, err := RescoreCatchRateTrials(CatchRateTrialsPath(committedCatchEvidence), committedCatchInputs())
	if err != nil {
		t.Fatal(err)
	}
	for _, arm := range []string{ArmWithoutConventions, ArmWithConventions} {
		a := rescoreArmOf(t, r, arm)
		p := a.Pooled()
		if p.Trials == 0 {
			t.Fatalf("arm %q re-scored no trials", arm)
		}
		if p.Gained != 0 || p.Lost != 0 || p.RescoredCaught != p.RecordedCaught {
			t.Errorf("arm %q under catch rule v5: +%d gained / -%d lost (recorded %d, re-scored %d); want identical", arm, p.Gained, p.Lost, p.RecordedCaught, p.RescoredCaught)
		}
	}
}

// pooledCounts sums trials and caught.
func pooledCounts(perCase map[string]CaseCatchCounts) (trials, caught int) {
	for _, c := range perCase {
		trials += c.Trials
		caught += c.Caught
	}
	return trials, caught
}

// ---------------------------------------------------------------------------
// The security arms (E80.17 / #4314).
// ---------------------------------------------------------------------------

// securityCaseSets is the committed security corpus's catch and clean case
// names.
func securityCaseSets(t *testing.T) (catch, clean []string) {
	t.Helper()
	cases, _ := loadCommittedSecurity(t)
	for _, c := range cases {
		if c.Expect() == CatchExpectClean {
			clean = append(clean, c.Name)
		} else {
			catch = append(catch, c.Name)
		}
	}
	sort.Strings(catch)
	sort.Strings(clean)
	return catch, clean
}

// TestRecordCatchRateEvidence_RecordsFiveArms (CF8) is the end-to-end
// security-arm wiring: one recording through the scripted sender writes all
// five arms — the security arm over every security case, the portability arm
// over the clean cases only, both persona-kind and partitioned — the record
// passes the gate, and the report carries the persona diagnostics.
func TestRecordCatchRateEvidence_RecordsFiveArms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.json")
	all := committedCatchTrials(t)
	report, err := record(t, newScriptedCatchSender(t, all, all), path, catchT0, pin("five arms"))
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	rec := readCatchEvidence(t, path)
	want := map[string]string{ArmWithoutConventions: CatchRateArmKindConventions, ArmWithConventions: CatchRateArmKindConventions,
		ArmArchitect: CatchRateArmKindPersona, ArmSecurity: CatchRateArmKindPersona, ArmSecurityPortability: CatchRateArmKindPersona}
	if len(rec.Arms) != len(want) {
		t.Fatalf("the record carries %d arms %v, want the five %v", len(rec.Arms), sortedArmNames(rec.Arms), want)
	}
	for arm, kind := range want {
		if a, ok := rec.Arms[arm]; !ok || a.Kind != kind {
			t.Errorf("arm %q = %+v (present %v), want kind %q", arm, a.Kind, ok, kind)
		}
	}
	catch, clean := securityCaseSets(t)
	if got := sortedCaseNames(rec.Arms[ArmSecurity].PerCase); len(got) != len(catch)+len(clean) {
		t.Errorf("security arm cases = %v, want every security case", got)
	}
	if got := sortedCaseNames(rec.Arms[ArmSecurityPortability].PerCase); strings.Join(got, ",") != strings.Join(clean, ",") {
		t.Errorf("security_portability arm cases = %v, want the clean cases %v", got, clean)
	}
	sec := pooledClassCounts(rec.Arms[ArmSecurity].PerCase)
	if sec.catchTrials < MinCatchRateTrialsPerArm || sec.caught != sec.catchTrials || sec.cleanTrials < MinCatchRateTrialsPerArm || sec.alarms != 0 {
		t.Errorf("security arm counts = %+v, want powered classes, every catch trial caught, no alarm", sec)
	}
	for _, arm := range []string{ArmSecurity, ArmSecurityPortability} {
		if p := rec.Arms[arm].FalseAlarmPartition; len(p) != len(clean) {
			t.Errorf("arm %q false_alarm_partition = %v, want one entry per clean case %v", arm, p, clean)
		}
	}
	if rec.Arms[ArmArchitect].FalseAlarmPartition != nil {
		t.Errorf("the architect arm records a false_alarm_partition %v", rec.Arms[ArmArchitect].FalseAlarmPartition)
	}
	if _, err := CheckCatchRateEvidence(path, committedCatchInputs(), testCatchModel); err != nil {
		t.Fatalf("CheckCatchRateEvidence on the five-arm record: %v", err)
	}
	for _, s := range []string{"Persona diagnostics", "security catch rate per remit lens:", SecurityLensAuthnAuthz + ": ", "security absent-control concerns, per case:",
		"security_portability repository-specific findings: none", "architect false alarms by category, per clean case:", "standard-criteria concerns included"} {
		if !strings.Contains(report, s) {
			t.Errorf("report lacks %q:\n%s", s, report)
		}
	}
}

// TestRecordCatchRateEvidence_PortabilityRepoSpecificFindingRefused (CF7):
// ONE security_portability clean-case call answered with a concern naming a
// repository-specific marker refuses the recording — pinning into a fresh
// path (no baseline to mask it), and on a dry run — with
// ErrPortabilityRepoSpecificFinding, and NEITHER file exists afterwards. The
// same answer on the security arm (shown the convention) records.
func TestRecordCatchRateEvidence_PortabilityRepoSpecificFindingRefused(t *testing.T) {
	all := committedCatchTrials(t)
	sender := func(arm string) *scriptedCatchSender {
		s := newScriptedCatchSender(t, all, all)
		done := false
		s.secOverride = func(a string, sc personaScriptCase, _ int) (string, bool) {
			if a == arm && sc.clean && !done {
				done = true
				return portabilityMarkerVerdict, true
			}
			return "", false
		}
		return s
	}
	for _, tc := range []struct {
		name string
		opts RecordCatchRateOptions
	}{{"pin", pin("portability")}, {"dry run", RecordCatchRateOptions{DryRun: true}}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "evidence.json")
			report, err := record(t, sender(ArmSecurityPortability), path, catchT0, tc.opts)
			if err == nil || !errors.Is(err, ErrPortabilityRepoSpecificFinding) || errors.Is(err, ErrCatchRateRegressed) {
				t.Fatalf("err = %v, want ErrPortabilityRepoSpecificFinding (and not a regression)", err)
			}
			if !strings.Contains(err.Error(), `names "gateenv.go"`) || !strings.Contains(report, "repository-specific findings (REFUSED)") {
				t.Errorf("err %v / report do not name the finding:\n%s", err, report)
			}
			for _, p := range []string{path, CatchRateTrialsPath(path)} {
				if _, statErr := os.Stat(p); !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("a refused recording left %s (stat err %v)", p, statErr)
				}
			}
		})
	}
	t.Run("the security arm may name a repository control", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "evidence.json")
		if _, err := record(t, sender(ArmSecurity), path, catchT0, pin("security names a control")); err != nil {
			t.Fatalf("a security-arm concern naming a repository control was refused: %v", err)
		}
	})
}

// TestRecordCatchRateEvidence_SidecarCarriesAbsenceAnnotations (CF10): a
// security-arm clean-case concern annotated in memory (repo_specific_marker
// gateenv.go, absent_control) is stored with both annotations in the WRITTEN
// sidecar, and the other arms' concerns carry none.
func TestRecordCatchRateEvidence_SidecarCarriesAbsenceAnnotations(t *testing.T) {
	all := committedCatchTrials(t)
	s := newScriptedCatchSender(t, all, all)
	var hit string
	s.secOverride = func(a string, sc personaScriptCase, _ int) (string, bool) {
		if a == ArmSecurity && sc.clean && hit == "" {
			hit = sc.name
			return portabilityMarkerVerdict, true
		}
		return "", false
	}
	path := filepath.Join(t.TempDir(), "evidence.json")
	if _, err := record(t, s, path, catchT0, pin("annotations")); err != nil {
		t.Fatalf("record: %v", err)
	}
	tr := readCatchTrials(t, path)
	first := tr.Arms[ArmSecurity].PerCase[hit][0]
	if first.Outcome != CatchFalseAlarm || len(first.Concerns) != 1 {
		t.Fatalf("case %q trial 1 = %+v, want a false alarm with one concern", hit, first)
	}
	if c := first.Concerns[0]; c.RepoSpecificMarker != "gateenv.go" || !c.AbsentControl {
		t.Errorf("stored concern = %+v, want repo_specific_marker gateenv.go and absent_control true", c)
	}
	raw := string(mustRead(t, CatchRateTrialsPath(path)))
	if !strings.Contains(raw, `"repo_specific_marker": "gateenv.go"`) || !strings.Contains(raw, `"absent_control": true`) {
		t.Error("the written sidecar JSON lacks the annotation fields")
	}
	for _, arm := range []string{ArmWithoutConventions, ArmArchitect} {
		for _, list := range tr.Arms[arm].PerCase {
			for _, rec := range list {
				for _, c := range rec.Concerns {
					if c.RepoSpecificMarker != "" || c.AbsentControl {
						t.Fatalf("arm %q stores an annotated concern %+v", arm, c)
					}
				}
			}
		}
	}
}

// partitionSender answers each security arm's FIRST call of every clean case
// with a concern of the given category: the partition fixture.
func partitionSender(t *testing.T, cats map[string]string) *scriptedCatchSender {
	t.Helper()
	all := committedCatchTrials(t)
	s := newScriptedCatchSender(t, all, all)
	seen := map[string]bool{}
	s.secOverride = func(a string, sc personaScriptCase, _ int) (string, bool) {
		cat, ok := cats[a+"/"+sc.name]
		if !ok || seen[a+"/"+sc.name] {
			return "", false
		}
		seen[a+"/"+sc.name] = true
		return personaVerdictJSON(cat, "a reviewer remark on the clean diff"), true
	}
	return s
}

// TestCatchRateFalseAlarmPartition_RoundTrips is approval condition C1(b):
// the false-alarm partition (security_lens vs other_category trials, per
// clean case of each security arm) is RECORDED in the evidence and, per
// trial, in the sidecar, survives Record → Check, and every inconsistency
// between the two is refused.
func TestCatchRateFalseAlarmPartition_RoundTrips(t *testing.T) {
	_, clean := securityCaseSets(t)
	if len(clean) < 2 {
		t.Fatal("fixture premise: at least two clean security cases")
	}
	a, b := clean[0], clean[1]
	cats := map[string]string{
		ArmSecurity + "/" + a:            "authz",       // security_lens
		ArmSecurity + "/" + b:            "correctness", // other_category
		ArmSecurityPortability + "/" + a: "regression",  // other_category
	}
	recordPartitioned := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "evidence.json")
		if _, err := record(t, partitionSender(t, cats), path, catchT0, pin("partition")); err != nil {
			t.Fatalf("record: %v", err)
		}
		return path
	}
	path := recordPartitioned(t)
	rec := readCatchEvidence(t, path)
	for _, w := range []struct {
		arm, name string
		want      CatchRateFalseAlarmPartition
	}{
		{ArmSecurity, a, CatchRateFalseAlarmPartition{SecurityLens: 1}},
		{ArmSecurity, b, CatchRateFalseAlarmPartition{OtherCategory: 1}},
		{ArmSecurityPortability, a, CatchRateFalseAlarmPartition{OtherCategory: 1}},
		{ArmSecurityPortability, b, CatchRateFalseAlarmPartition{}},
	} {
		if got := rec.Arms[w.arm].FalseAlarmPartition[w.name]; got != w.want {
			t.Errorf("arm %q case %q false_alarm_partition = %+v, want %+v", w.arm, w.name, got, w.want)
		}
		if got := rec.Baseline.Arms[w.arm].FalseAlarmPartition[w.name]; got != w.want {
			t.Errorf("pinned baseline arm %q case %q false_alarm_partition = %+v, want %+v", w.arm, w.name, got, w.want)
		}
	}
	tr := readCatchTrials(t, path)
	for _, w := range []struct{ arm, name, want string }{
		{ArmSecurity, a, AlarmPartitionSecurityLens}, {ArmSecurity, b, AlarmPartitionOther}, {ArmSecurityPortability, a, AlarmPartitionOther},
	} {
		if got := tr.Arms[w.arm].PerCase[w.name][0].AlarmPartition; got != w.want {
			t.Errorf("sidecar arm %q case %q trial 1 alarm_partition = %q, want %q", w.arm, w.name, got, w.want)
		}
	}
	if got := tr.Arms[ArmSecurity].PerCase[a][1].AlarmPartition; got != "" {
		t.Errorf("a no_false_alarm trial carries alarm_partition %q", got)
	}
	if _, err := CheckCatchRateEvidence(path, committedCatchInputs(), testCatchModel); err != nil {
		t.Fatalf("CheckCatchRateEvidence on the partitioned record: %v", err)
	}

	setPartition := func(r *CatchRateEvidence, arm, name string, p CatchRateFalseAlarmPartition) {
		x := r.Arms[arm]
		if x.FalseAlarmPartition == nil {
			x.FalseAlarmPartition = map[string]CatchRateFalseAlarmPartition{}
		}
		x.FalseAlarmPartition[name] = p
		r.Arms[arm] = x
	}
	archCatch, archClean := personaCaseNames(t)
	for _, tc := range []struct {
		name   string
		mutate func(r *CatchRateEvidence)
		trials func(tr *CatchRateTrials)
		want   string
	}{
		{name: "(6c) a partition not summing to false_alarms", mutate: func(r *CatchRateEvidence) {
			setPartition(r, ArmSecurity, a, CatchRateFalseAlarmPartition{SecurityLens: 2})
		}, want: "does not sum to its false_alarms 1"},
		{name: "(6c) a negative partition count", mutate: func(r *CatchRateEvidence) {
			setPartition(r, ArmSecurity, a, CatchRateFalseAlarmPartition{SecurityLens: 2, OtherCategory: -1})
		}, want: "negative count"},
		{name: "(6c) a partition entry on a catch case", mutate: func(r *CatchRateEvidence) {
			catch, _ := securityCaseSets(t)
			setPartition(r, ArmSecurity, catch[0], CatchRateFalseAlarmPartition{})
		}, want: "which is not a clean case of the arm"},
		{name: "(6c) a clean case missing from a security arm's partition", mutate: func(r *CatchRateEvidence) {
			delete(r.Arms[ArmSecurityPortability].FalseAlarmPartition, b)
		}, want: `arm "security_portability" clean case "` + b + `" has no false_alarm_partition entry`},
		{name: "(6c) a partition on the architect arm", mutate: func(r *CatchRateEvidence) {
			setPartition(r, ArmArchitect, archClean, CatchRateFalseAlarmPartition{})
		}, want: `arm "architect" records a false_alarm_partition`},
		{name: "(16) a partition flipped in the record only", mutate: func(r *CatchRateEvidence) {
			setPartition(r, ArmSecurity, a, CatchRateFalseAlarmPartition{OtherCategory: 1})
		}, want: "alarm_partition tallies (security_lens 1, other_category 0) differ"},
		{name: "(16) a false_alarm trial without a tag", trials: func(tr *CatchRateTrials) {
			tr.Arms[ArmSecurity].PerCase[a][0].AlarmPartition = ""
		}, want: "is a false_alarm without an alarm_partition"},
		{name: "(16) a tag on a no_false_alarm trial", trials: func(tr *CatchRateTrials) {
			tr.Arms[ArmSecurity].PerCase[b][1].AlarmPartition = AlarmPartitionOther
		}, want: "only a false_alarm trial is partitioned"},
		{name: "(16) an unknown tag", trials: func(tr *CatchRateTrials) {
			tr.Arms[ArmSecurity].PerCase[a][0].AlarmPartition = "security"
		}, want: `alarm_partition "security" is not`},
		{name: "(16) a tag on an unpartitioned arm", trials: func(tr *CatchRateTrials) {
			tr.Arms[ArmArchitect].PerCase[archCatch][0].AlarmPartition = AlarmPartitionSecurityLens
		}, want: "but the arm is not partitioned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := recordPartitioned(t)
			if tc.mutate != nil {
				rec := readCatchEvidence(t, path)
				tc.mutate(&rec)
				writeCatchEvidence(t, path, rec)
			}
			if tc.trials != nil {
				tr := readCatchTrials(t, path)
				tc.trials(&tr)
				writeCatchTrials(t, path, tr)
			}
			_, err := CheckCatchRateEvidence(path, committedCatchInputs(), testCatchModel)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}
