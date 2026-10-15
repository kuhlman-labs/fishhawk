package agenteval

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/redaction"
)

// No t.Parallel in this file: several tests use t.Setenv (AGENTS.md trap).

// readCatchDiagLines decodes every line of a diagnostics file.
func readCatchDiagLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw := mustRead(t, path)
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<26)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("diagnostics line %d is not JSON: %v (%q)", len(out)+1, err, sc.Text())
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	if len(out) == 0 {
		t.Fatalf("diagnostics file %s is empty", path)
	}
	return out
}

func catchDiagTrialLines(lines []map[string]any) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["kind"] == "trial" {
			out = append(out, l)
		}
	}
	return out
}

func catchDiagSummaryOf(t *testing.T, lines []map[string]any) map[string]any {
	t.Helper()
	last := lines[len(lines)-1]
	if last["kind"] != "summary" {
		t.Fatalf("last diagnostics line kind = %v, want summary (the run finished without one)", last["kind"])
	}
	return last
}

// expectedArmTrials is the per-arm trial count of a full recording: the sum
// over the row's cases of its samples.
func expectedArmTrials(t *testing.T) map[string]int {
	t.Helper()
	rows, err := catchRateArmTable(committedCatchInputs())
	if err != nil {
		t.Fatalf("catchRateArmTable: %v", err)
	}
	out := map[string]int{}
	for _, row := range rows {
		out[row.name] = len(row.cases) * row.samples
	}
	if len(out) != 5 {
		t.Fatalf("arm table has %d arms, want 5", len(out))
	}
	return out
}

func snapshotEvidencePair(t *testing.T, path string) (evidence, trials []byte) {
	t.Helper()
	return mustRead(t, path), mustRead(t, CatchRateTrialsPath(path))
}

func assertEvidencePairUnchanged(t *testing.T, path string, evidence, trials []byte) {
	t.Helper()
	if got := mustRead(t, path); !bytes.Equal(got, evidence) {
		t.Errorf("evidence.json changed (it must stay byte-identical)")
	}
	if got := mustRead(t, CatchRateTrialsPath(path)); !bytes.Equal(got, trials) {
		t.Errorf("evidence.trials.json changed (it must stay byte-identical)")
	}
}

// D1. The refusal happens AFTER every arm ran, so every trial reaches the
// diagnostics file. Fixture-state mechanism: with the unconditional Finish
// removed (CF2: Finish only on a nil error), the file still holds every trial
// line but NO summary line, so catchDiagSummaryOf fails; with the persona
// loop hook deleted (CF6), the architect and both security arms have ZERO
// trial lines, so the per-arm counts fail.
func TestRecordCatchRateEvidenceWithDiagnostics_RefusalKeepsEveryTrial(t *testing.T) {
	path := freshCatchEvidence(t)
	evidence, trials := snapshotEvidencePair(t, path)
	all := committedCatchTrials(t)
	lower := catchesAtRate(t, 0.80)
	diagPath := filepath.Join(t.TempDir(), "run", CatchRateDiagFileName)

	report, err := RecordCatchRateEvidenceWithDiagnostics(context.Background(), newScriptedCatchSender(t, all, lower), testCatchModel,
		committedCatchInputs(), path, catchT0.Add(time.Hour), RecordCatchRateOptions{}, diagPath, CatchRateDiagOptions{})
	if !errors.Is(err, ErrCatchRateRegressed) {
		t.Fatalf("err = %v, want ErrCatchRateRegressed", err)
	}
	assertEvidencePairUnchanged(t, path, evidence, trials)

	lines := readCatchDiagLines(t, diagPath)
	if lines[0]["kind"] != "header" || lines[0]["schema"] != CatchRateDiagSchema {
		t.Errorf("first line = %v, want the %s header", lines[0], CatchRateDiagSchema)
	}
	perArm := map[string]int{}
	for _, l := range catchDiagTrialLines(lines) {
		perArm[l["arm"].(string)]++
	}
	for arm, want := range expectedArmTrials(t) {
		if perArm[arm] != want {
			t.Errorf("arm %q: %d trial lines, want %d (one per trial, written before the refusal)", arm, perArm[arm], want)
		}
	}
	sum := catchDiagSummaryOf(t, lines)
	if sum["outcome"] != "refused" {
		t.Errorf("summary outcome = %v, want refused", sum["outcome"])
	}
	with := sum["arms"].(map[string]any)[ArmWithConventions].(map[string]any)
	misses := int(with["catch_trials"].(float64)) - int(with["caught"].(float64))
	if misses != all-lower {
		t.Errorf("with_conventions misses = %d, want %d", misses, all-lower)
	}
	classSum := 0
	for _, v := range with["miss_classes"].(map[string]any) {
		classSum += int(v.(float64))
	}
	if classSum != misses {
		t.Errorf("with_conventions miss_classes sum to %d, want its %d misses", classSum, misses)
	}
	for _, want := range []string{"Diagnostics summary", "  with_conventions:", "miss classes: not_mentioned", "Diagnostics: " + diagPath} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
}

// D2. Both the file and Finish's returned summary text are checked.
// Fixture-state mechanism: the fake credential matches no
// redaction.DefaultPatterns shape, so only the known-value pass removes it
// (CF4: with that line deleted the file carries the fake value); the
// sk-ant-oat01 token is short and not an api03 key, so only the sk-ant scrub
// removes it (CF7: with that line deleted the file carries "sk-ant"). The
// undecodable response's verdict carries both, and the verdict histogram is
// rendered into the returned text without passing the write-time scrub, so
// with Finish's text scrub deleted the returned text carries the fake value.
// The credential is MIXED-CASE and also sits in a concern category, which
// the histogram case-folds: with the scrub-before-fold in add deleted, the
// folded value no longer matches its known value, so the file's summary line
// and the returned text carry it lower-cased (the checks are case-folded).
func TestCatchRateDiag_NoCredentialInFile(t *testing.T) {
	const fake = "fhdiag-Fake-Credential-Value-0123456789"
	t.Setenv(liveAPIKeyEnv, fake)
	t.Setenv(liveAuthTokenEnv, "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	creds := CatchRateDiagCredentials(os.Getenv)
	if len(creds) != 1 || creds[0].Name != liveAPIKeyEnv {
		t.Fatalf("CatchRateDiagCredentials = %d bindings, want the one set variable", len(creds))
	}
	const oat = "sk-ant-oat01-shortfaketoken"
	api03 := "sk-ant-api03-" + strings.Repeat("A1b2C3d4", 6)
	if red, _ := redaction.RedactDefault([]byte(fake + " " + oat)); !bytes.Contains(red, []byte(fake)) || !bytes.Contains(red, []byte(oat)) {
		t.Fatal("precondition: RedactDefault already removes the fake value or the oat01 token; the fixture would not isolate the known-value pass and the sk-ant scrub")
	}

	b, _ := json.Marshal(map[string]any{"verdict": "approve_with_concerns", "concerns": []any{map[string]any{
		"severity": "medium", "category": "leak " + fake + " " + oat, "convention": fake + " " + oat,
		"note": "the key " + fake + " and " + oat + " and " + api03,
	}}})
	verdict := string(b)
	undecodable := `no verdict here: {"verdict":"` + fake + " " + oat + " " + api03 + `"}`
	call := 0
	sender := senderFunc(func(_ context.Context, _, _ string) (string, string, int, int, int, int, error) {
		call++
		if call%2 == 0 {
			return undecodable, "fake-model", 0, 0, 0, 0, nil
		}
		return verdict, "fake-model", 0, 0, 0, 0, nil
	})
	path := filepath.Join(t.TempDir(), CatchRateDiagFileName)
	d, err := OpenCatchRateDiag(path, CatchRateDiagOptions{Credentials: creds}, nil)
	if err != nil {
		t.Fatalf("OpenCatchRateDiag: %v", err)
	}
	cases, conv := loadCommittedCatch(t)
	if _, err := RunCatchRateArm(withCatchTrialObserver(context.Background(), d), sender, cases, conv, ArmWithConventions, 1); err != nil {
		t.Fatalf("RunCatchRateArm: %v", err)
	}
	text, err := d.Finish(errors.New("transport said " + fake + " " + oat))
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !strings.Contains(text, "verdicts: ") || !strings.Contains(text, "concern categories: leak ") {
		t.Fatalf("precondition: the returned summary renders no verdict or category histogram; the fixture did not reach it:\n%s", text)
	}
	raw := mustRead(t, path)
	if n := len(catchDiagTrialLines(readCatchDiagLines(t, path))); n != len(cases) {
		t.Fatalf("precondition: %d trial lines, want %d", n, len(cases))
	}
	if !bytes.Contains(raw, []byte("[REDACTED:")) || !bytes.Contains(raw, []byte("response_excerpt")) {
		t.Fatal("precondition: the file carries no redaction marker or no undecodable excerpt; the fixture did not reach the writer")
	}
	for what, b := range map[string][]byte{"file": raw, "returned summary text": []byte(text)} {
		if bytes.Contains(bytes.ToLower(b), bytes.ToLower([]byte(fake))) {
			t.Errorf("the diagnostics %s carries the credential value (in some letter case)", what)
		}
		if bytes.Contains(b, []byte("sk-ant")) {
			t.Errorf("the diagnostics %s carries an sk-ant token", what)
		}
	}
}

// senderFunc adapts a function to MessageSender.
type senderFunc func(ctx context.Context, system, user string) (string, string, int, int, int, int, error)

func (f senderFunc) Messages(ctx context.Context, system, user string) (string, string, int, int, int, int, error) {
	return f(ctx, system, user)
}

func concernVerdict(concerns ...[2]string) string {
	list := make([]any, 0, len(concerns))
	for _, c := range concerns {
		list = append(list, map[string]any{"severity": "medium", "category": c[0], "note": c[1]})
	}
	b, _ := json.Marshal(map[string]any{"verdict": "approve_with_concerns", "concerns": list})
	return string(b)
}

// D3. One constructed response+rule per class. Each row first asserts the
// classifier's own outcome, so classification never touches scoring.
// Fixture-state mechanism: collapsing any two classes (CF3: `return
// CatchMissNoProbe` returning CatchMissUnattributed) leaves the no_probe row
// reading unattributed.
func TestClassifyCatchMiss(t *testing.T) {
	base := CatchRule{Probes: []string{"untestable"}, CriterionIDs: []string{"AC-7"}, AttributionPhrases: []string{"retry budget"}}
	labelled := base
	labelled.Category = "architecture"
	clean := CatchRule{Clean: true}
	for _, tc := range []struct {
		name     string
		response string
		rule     CatchRule
		outcome  CatchOutcome
		want     CatchMissClass
	}{
		{"not_mentioned", concernVerdict([2]string{"acceptance_criteria", "the deploy order is unclear"}), base, CatchMissed, CatchMissNotMentioned},
		{"no concerns", `{"verdict":"approve","concerns":[]}`, base, CatchMissed, CatchMissNotMentioned},
		{"unattributed", concernVerdict([2]string{"acceptance_criteria", "this criterion is untestable"}), base, CatchMissed, CatchMissUnattributed},
		{"no_probe", concernVerdict([2]string{"acceptance_criteria", "AC-7 needs more detail"}), base, CatchMissed, CatchMissNoProbe},
		{"category_mismatch", concernVerdict([2]string{"acceptance_criteria", "AC-7 is untestable"}), labelled, CatchMissed, CatchMissWrongLabel},
		{"undecodable", "not json at all", base, CatchUndecodable, CatchMissUndecodable},
		{"precedence unattributed over no_probe", concernVerdict(
			[2]string{"acceptance_criteria", "AC-7 needs more detail"},
			[2]string{"acceptance_criteria", "something is untestable"}), base, CatchMissed, CatchMissUnattributed},
		{"precedence mismatch over unattributed", concernVerdict(
			[2]string{"acceptance_criteria", "something is untestable"},
			[2]string{"acceptance_criteria", "AC-7 is untestable"}), labelled, CatchMissed, CatchMissWrongLabel},
		{"caught", concernVerdict([2]string{"acceptance_criteria", "AC-7 is untestable"}), base, CatchCaught, ""},
		{"clean rule", concernVerdict([2]string{"acceptance_criteria", "AC-7 is untestable"}), clean, CatchFalseAlarm, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := ClassifyCatchTrial(tc.response, tc.rule)
			if tr.Outcome != tc.outcome {
				t.Fatalf("precondition: ClassifyCatchTrial outcome = %q, want %q", tr.Outcome, tc.outcome)
			}
			if got := classifyCatchMiss(tr, tc.rule); got != tc.want {
				t.Errorf("classifyCatchMiss = %q, want %q", got, tc.want)
			}
		})
	}
}

// D4. Fixture-state mechanism (isolation): the subset selects EVERY case at
// the default samples, so its measurement is identical to a passing,
// comparable full one, and the dry-run-only not-judged branch is skipped
// when DryRun is false. With the guard deleted (CF1) the run judges, passes
// and REWRITES the pair (recorded_at differs) after 5 arms of sender calls;
// on the pin row it returns the not-judged report with a nil error.
func TestRecordCatchRateEvidence_SubsetRefusesToRecord(t *testing.T) {
	rows, err := catchRateArmTable(committedCatchInputs())
	if err != nil {
		t.Fatalf("catchRateArmTable: %v", err)
	}
	var every []string
	for _, row := range rows {
		for _, c := range row.cases {
			every = append(every, c.name)
		}
	}
	for _, tc := range []struct {
		name string
		opts RecordCatchRateOptions
	}{
		{"record", RecordCatchRateOptions{Subset: CatchRateSubset{Cases: every}}},
		{"dry run that pins", RecordCatchRateOptions{DryRun: true, PinBaseline: true, Reason: "x", Subset: CatchRateSubset{Cases: every}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := freshCatchEvidence(t)
			evidence, trials := snapshotEvidencePair(t, path)
			all := committedCatchTrials(t)
			sender := newScriptedCatchSender(t, all, all)
			_, err := RecordCatchRateEvidence(context.Background(), sender, testCatchModel, committedCatchInputs(), path, catchT0.Add(time.Hour), tc.opts)
			if !errors.Is(err, ErrCatchRateSubsetNotRecordable) {
				t.Errorf("err = %v, want ErrCatchRateSubsetNotRecordable", err)
			}
			if n := sender.total(); n != 0 {
				t.Errorf("sender called %d times, want 0 (refused before any model call)", n)
			}
			assertEvidencePairUnchanged(t, path, evidence, trials)
		})
	}
}

// D5. Fixture-state mechanism: the sender reads the file on every call, so
// a trial line must be on disk before the NEXT trial starts. With the
// RunCatchRateArm hook deleted (CF5) every observed count is 0.
func TestCatchRateDiag_CheckpointsEveryTrial(t *testing.T) {
	cases, conv := loadCommittedCatch(t)
	const samples = 2
	n := samples*len(cases) - 1 // the last call fails: the run aborts there
	path := filepath.Join(t.TempDir(), CatchRateDiagFileName)
	d, err := OpenCatchRateDiag(path, CatchRateDiagOptions{}, nil)
	if err != nil {
		t.Fatalf("OpenCatchRateDiag: %v", err)
	}
	var seen []int
	call := 0
	sender := senderFunc(func(_ context.Context, _, _ string) (string, string, int, int, int, int, error) {
		call++
		lines := 0
		for _, l := range strings.Split(string(mustRead(t, path)), "\n") {
			if strings.Contains(l, `"kind":"trial"`) {
				lines++
			}
		}
		seen = append(seen, lines)
		if call > n {
			return "", "", 0, 0, 0, 0, errors.New("connection reset")
		}
		return `{"verdict":"approve","concerns":[]}`, "fake-model", 0, 0, 0, 0, nil
	})
	_, runErr := RunCatchRateArm(withCatchTrialObserver(context.Background(), d), sender, cases, conv, ArmWithoutConventions, samples)
	if runErr == nil {
		t.Fatal("precondition: the run did not abort on the transport error")
	}
	for k, got := range seen {
		if got != k {
			t.Errorf("call %d saw %d trial lines on disk, want %d", k+1, got, k)
		}
	}
	if _, err := d.Finish(runErr); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	lines := readCatchDiagLines(t, path)
	if got := len(catchDiagTrialLines(lines)); got != n {
		t.Errorf("%d trial lines after the abort, want %d", got, n)
	}
	if sum := catchDiagSummaryOf(t, lines); sum["outcome"] != "error" || !strings.Contains(sum["error"].(string), "connection reset") {
		t.Errorf("summary = outcome %v error %v, want an error outcome naming the transport error", sum["outcome"], sum["error"])
	}
}

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// The knob refusals are the counterfactual vehicles. Fixture-state
// mechanism: with the blank-list refusal (`if len(s.Cases) == 0`) deleted,
// " , " parses to the INACTIVE subset with a nil error, so the blank-list row
// fails; with the `n < 1` half deleted, "0" and "-1" parse to a non-positive
// count with a nil error, so those two rows fail; with the `err != nil` half
// deleted, the out-of-range row fails, because Atoi returns the clamped
// maximum (positive) with its range error — "abc" cannot isolate that half,
// since Atoi returns 0 for it and `n < 1` refuses it anyway.
func TestParseCatchRateSubset(t *testing.T) {
	s, err := ParseCatchRateSubset(envMap(nil))
	if err != nil || s.active() {
		t.Fatalf("unset knobs = %+v, %v; want the inactive subset", s, err)
	}
	s, err = ParseCatchRateSubset(envMap(map[string]string{CatchRateSubsetCasesEnv: " a, b ,a,, ", CatchRateSubsetTrialsEnv: " 3 "}))
	if err != nil {
		t.Fatalf("ParseCatchRateSubset: %v", err)
	}
	if strings.Join(s.Cases, "|") != "a|b" || s.TrialsPerCase != 3 || !s.active() {
		t.Errorf("subset = %+v, want cases [a b] (trimmed, de-duplicated) and 3 trials", s)
	}
	for _, tc := range []struct{ name, key, val string }{
		{"blank case list", CatchRateSubsetCasesEnv, " , "},
		{"zero trials", CatchRateSubsetTrialsEnv, "0"},
		{"negative trials", CatchRateSubsetTrialsEnv, "-1"},
		{"non-numeric trials", CatchRateSubsetTrialsEnv, "abc"},
		{"out-of-range trials", CatchRateSubsetTrialsEnv, "99999999999999999999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if s, err := ParseCatchRateSubset(envMap(map[string]string{tc.key: tc.val})); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Errorf("ParseCatchRateSubset(%s=%q) = %+v, %v; want a refusal naming %s", tc.key, tc.val, s, err, tc.key)
			}
		})
	}
}

func TestCatchRateSubsetForRow(t *testing.T) {
	row := catchRateArmRow{name: "arm", samples: 9, cases: []catchRateArmCase{{name: "a"}, {name: "b"}}}
	if n, keep, ok := (CatchRateSubset{}).forRow(row); n != 9 || keep != nil || !ok {
		t.Errorf("inactive forRow = %d, keep set %v, %v; want the row's own samples, no filter, runs", n, keep != nil, ok)
	}
	if n, keep, ok := (CatchRateSubset{TrialsPerCase: 2}).forRow(row); n != 2 || keep != nil || !ok {
		t.Errorf("trials-only forRow = %d, keep set %v, %v; want 2 samples over every case", n, keep != nil, ok)
	}
	n, keep, ok := (CatchRateSubset{Cases: []string{"b"}}).forRow(row)
	if n != 9 || keep == nil || !ok || keep("a") || !keep("b") {
		t.Errorf("cases forRow = %d, %v; want the row's samples and a filter keeping only b", n, ok)
	}
	if _, _, ok := (CatchRateSubset{Cases: []string{"z"}}).forRow(row); ok {
		t.Error("an arm none of whose cases is selected must not run")
	}
	if got := selectCatchCases([]string{"a", "b"}, keep, func(s string) string { return s }); strings.Join(got, "|") != "b" {
		t.Errorf("selectCatchCases = %v, want [b]", got)
	}
	if got := selectCatchCases([]string{"a", "b"}, nil, func(s string) string { return s }); len(got) != 2 {
		t.Errorf("selectCatchCases with nil keep = %v, want the input unchanged", got)
	}
}

// Fixture-state mechanism: with validate's refusal removed, the unknown id
// selects no arm, so every row is skipped and the run returns the not-judged
// dry-run report with a NIL error.
func TestRecordCatchRateEvidence_SubsetUnknownCaseRefused(t *testing.T) {
	all := committedCatchTrials(t)
	sender := newScriptedCatchSender(t, all, all)
	path := filepath.Join(t.TempDir(), "evidence.json")
	_, err := RecordCatchRateEvidence(context.Background(), sender, testCatchModel, committedCatchInputs(), path, catchT0,
		RecordCatchRateOptions{DryRun: true, Subset: CatchRateSubset{Cases: []string{"no-such-case"}}})
	if err == nil || !strings.Contains(err.Error(), "no-such-case") {
		t.Errorf("err = %v, want a refusal naming the unknown case id", err)
	}
	if n := sender.total(); n != 0 {
		t.Errorf("sender called %d times, want 0", n)
	}
}

// Fixture-state mechanism: with the not-judged branch deleted, the two-arm,
// 2-sample measurement goes to judgeCatchRate and is refused (under-powered),
// so the nil-error assertion fails.
func TestRecordCatchRateEvidence_SubsetDryRun(t *testing.T) {
	cases, _ := loadCommittedCatch(t)
	all := committedCatchTrials(t)
	path := filepath.Join(t.TempDir(), "evidence.json")
	sender := newScriptedCatchSender(t, all, all)
	report, err := RecordCatchRateEvidence(context.Background(), sender, testCatchModel, committedCatchInputs(), path, catchT0,
		RecordCatchRateOptions{DryRun: true, Subset: CatchRateSubset{Cases: []string{cases[0].Name}, TrialsPerCase: 2}})
	if err != nil {
		t.Fatalf("subset dry run: %v", err)
	}
	if sender.total() != 4 || sender.calls[ArmWithoutConventions] != 2 || sender.calls[ArmWithConventions] != 2 {
		t.Errorf("sender calls = %v (total %d), want 2 per conventions arm and nothing else", sender.calls, sender.total())
	}
	for _, want := range []string{"NOT judged", "never recordable", cases[0].Name + ": caught 2/2", "  with_conventions:"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, ArmArchitect+":") {
		t.Errorf("report names an arm the subset did not select:\n%s", report)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a subset dry run created the evidence file (stat err %v)", err)
	}

	// A clean persona case reports its false-alarm count.
	rows, err := catchRateArmTable(committedCatchInputs())
	if err != nil {
		t.Fatalf("catchRateArmTable: %v", err)
	}
	var cleanCase string
	for _, row := range rows {
		if row.name != ArmArchitect {
			continue
		}
		for _, c := range row.cases {
			if c.expect == CatchExpectClean {
				cleanCase = c.name
				break
			}
		}
	}
	if cleanCase == "" {
		t.Fatal("precondition: the architect arm has no clean case")
	}
	report, err = RecordCatchRateEvidence(context.Background(), newScriptedCatchSender(t, all, all), testCatchModel, committedCatchInputs(), path, catchT0,
		RecordCatchRateOptions{DryRun: true, Subset: CatchRateSubset{Cases: []string{cleanCase}, TrialsPerCase: 1}})
	if err != nil {
		t.Fatalf("clean subset dry run: %v", err)
	}
	if !strings.Contains(report, cleanCase+" (clean): false alarms 0/1") {
		t.Errorf("report lacks the clean case's false-alarm line:\n%s", report)
	}
}

// Fixture-state mechanism (CF8): with the `if catchDiagUnderTestdata(abs) {`
// guard mutated to `if false {`, both testdata rows return a path.
func TestResolveCatchRateDiagPath(t *testing.T) {
	now := time.Date(2006, 10, 14, 23, 25, 53, 0, time.FixedZone("x", -5*3600))
	const stamp = "20061015T042553Z"
	dir := t.TempDir()
	got, err := ResolveCatchRateDiagPath(envMap(map[string]string{CatchRateDiagDirEnv: dir}), now)
	if want := filepath.Join(dir, stamp, CatchRateDiagFileName); err != nil || got != want {
		t.Errorf("DIAG_DIR path = %q, %v; want %q", got, err, want)
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	got, err = ResolveCatchRateDiagPath(envMap(nil), now)
	if want := filepath.Join(tmp, "fishhawk-agenteval", stamp, CatchRateDiagFileName); err != nil || got != want {
		t.Errorf("TMPDIR path = %q, %v; want %q", got, err, want)
	}
	if _, err := ResolveCatchRateDiagPath(envMap(map[string]string{CatchRateDiagDirEnv: filepath.Join(dir, "testdata", "x")}), now); err == nil || !strings.Contains(err.Error(), "testdata") {
		t.Errorf("a DIAG_DIR under testdata = %v, want a refusal", err)
	}
	t.Setenv("TMPDIR", filepath.Join(tmp, "testdata"))
	if _, err := ResolveCatchRateDiagPath(envMap(nil), now); err == nil || !strings.Contains(err.Error(), "testdata") {
		t.Errorf("a TMPDIR under testdata = %v, want a refusal", err)
	}
}

// Fixture-state mechanism: without O_EXCL the open truncates the earlier
// file and writes a header over it.
func TestOpenCatchRateDiag_RefusesClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), CatchRateDiagFileName)
	earlier := []byte("an earlier run's diagnostics\n")
	if err := os.WriteFile(path, earlier, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCatchRateDiag(path, CatchRateDiagOptions{}, nil); err == nil {
		t.Error("OpenCatchRateDiag over an existing file succeeded, want a refusal")
	}
	if got := mustRead(t, path); !bytes.Equal(got, earlier) {
		t.Errorf("the earlier file changed: %q", got)
	}
	// The directory step fails first and names itself; without its check the
	// open would fail anyway, but naming "open", not "mkdir".
	if _, err := OpenCatchRateDiag(filepath.Join(path, "sub", CatchRateDiagFileName), CatchRateDiagOptions{}, nil); err == nil || !strings.Contains(err.Error(), "mkdir") {
		t.Errorf("OpenCatchRateDiag under a regular file = %v, want the mkdir error", err)
	}
}

// An open failure refuses the run before any model call.
func TestRecordCatchRateEvidenceWithDiagnostics_OpenFailureRefusesRun(t *testing.T) {
	diagPath := filepath.Join(t.TempDir(), CatchRateDiagFileName)
	if err := os.WriteFile(diagPath, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	all := committedCatchTrials(t)
	sender := newScriptedCatchSender(t, all, all)
	path := filepath.Join(t.TempDir(), "evidence.json")
	if _, err := RecordCatchRateEvidenceWithDiagnostics(context.Background(), sender, testCatchModel, committedCatchInputs(), path, catchT0,
		pin("x"), diagPath, CatchRateDiagOptions{}); err == nil {
		t.Error("a diagnostics open failure did not refuse the run")
	}
	if n := sender.total(); n != 0 {
		t.Errorf("sender called %d times, want 0", n)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused run wrote evidence (stat err %v)", err)
	}
}

// failingWriter accepts okWrites writes, then fails every one.
type failingWriter struct {
	okWrites, writes int
	closed           bool
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.okWrites {
		return 0, errors.New("disk full")
	}
	return len(p), nil
}

func (w *failingWriter) Close() error { w.closed = true; return nil }

// Fixture-state mechanism: without the sticky check the second observe
// writes again (3 writes, not 2); without Finish's error return the write
// failure is silent.
func TestCatchRateDiag_WriteFailureSurfacesAtFinish(t *testing.T) {
	w := &failingWriter{okWrites: 1}
	var progress bytes.Buffer
	d, err := newCatchRateDiag(w, "/diag/path.jsonl", CatchRateDiagOptions{Progress: &progress}, nil)
	if err != nil {
		t.Fatalf("newCatchRateDiag: %v", err)
	}
	tr := ClassifyCatchTrial(`{"verdict":"approve","concerns":[]}`, CatchRule{Probes: []string{"p"}})
	d.observe("arm", "case", CatchExpectCatch, 1, `{"verdict":"approve","concerns":[]}`, tr)
	d.observe("arm", "case", CatchExpectCatch, 2, `{"verdict":"approve","concerns":[]}`, tr)
	if w.writes != 2 {
		t.Errorf("writer saw %d writes, want 2 (the header, then the failing line; later lines skipped)", w.writes)
	}
	text, err := d.Finish(nil)
	if err == nil || !strings.Contains(err.Error(), "/diag/path.jsonl") || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("Finish err = %v, want the write failure naming the path", err)
	}
	if !w.closed {
		t.Error("Finish did not close the writer")
	}
	if !strings.Contains(text, "arm: 2 trials") {
		t.Errorf("the summary lost the trials the file could not hold:\n%s", text)
	}
	if !strings.Contains(progress.String(), "disk full") {
		t.Errorf("progress carries no write warning: %q", progress.String())
	}
	if _, err := d.Finish(nil); err == nil {
		t.Error("a second Finish succeeded, want an error")
	}
	d.observe("arm", "case", CatchExpectCatch, 3, "", tr) // after Finish: a no-op
	if w.writes != 2 {
		t.Errorf("observe after Finish wrote (writes %d)", w.writes)
	}
}

// nopWriteCloser is an in-memory diagnostics sink.
type nopWriteCloser struct{ bytes.Buffer }

func (*nopWriteCloser) Close() error { return nil }

func TestCatchRateDiag_ProgressLines(t *testing.T) {
	rule := CatchRule{Probes: []string{"untestable"}, CriterionIDs: []string{"AC-7"}}
	rules := map[string]map[string]catchRateArmCase{
		"arm_a": {"c1": {name: "c1", expect: CatchExpectCatch, rule: rule}, "k1": {name: "k1", expect: CatchExpectClean, rule: CatchRule{Clean: true}}},
	}
	var progress bytes.Buffer
	d, err := newCatchRateDiag(&nopWriteCloser{}, "p", CatchRateDiagOptions{Progress: &progress,
		Credentials: []redaction.KnownValue{{Name: "SHORT", Value: "abc"}}}, rules)
	if err != nil {
		t.Fatalf("newCatchRateDiag: %v", err)
	}
	if !strings.Contains(progress.String(), "SHORT is shorter than") {
		t.Errorf("no below-floor credential warning: %q", progress.String())
	}
	progress.Reset()
	steps := []struct {
		caseName, expect, response string
	}{
		{"c1", CatchExpectCatch, concernVerdict([2]string{"x", "AC-7 is untestable"})},
		{"c1", CatchExpectCatch, concernVerdict([2]string{"x", "AC-7 needs work"})},
		{"c1", CatchExpectCatch, "garbage"},
		{"k1", CatchExpectClean, concernVerdict([2]string{"x", "a note"})},
		{"k1", CatchExpectClean, `{"verdict":"approve","concerns":[]}`},
		{"k1", CatchExpectClean, "garbage"},
	}
	for i, s := range steps {
		r := rules["arm_a"][s.caseName].rule
		d.observe("arm_a", s.caseName, s.expect, i+1, s.response, ClassifyCatchTrial(s.response, r))
	}
	got := strings.Split(strings.TrimSuffix(progress.String(), "\n"), "\n")
	want := []string{"c1/arm_a/1 caught", "c1/arm_a/2 miss:no_probe", "c1/arm_a/3 miss:undecodable",
		"k1/arm_a/4 false_alarm", "k1/arm_a/5 no_false_alarm", "k1/arm_a/6 false_alarm"}
	re := regexp.MustCompile(`^[^/]+/[a-z_]+/[0-9]+ (caught|miss:(not_mentioned|unattributed|no_probe|category_mismatch|undecodable)|false_alarm|no_false_alarm)$`)
	if len(got) != len(want) {
		t.Fatalf("progress lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] || !re.MatchString(got[i]) {
			t.Errorf("progress line %d = %q, want %q", i+1, got[i], want[i])
		}
	}
	// Neither a nil context nor a nil writer observes anything.
	observeCatchTrial(nil, "arm_a", "c1", CatchExpectCatch, 1, "", CatchTrial{}) //nolint:staticcheck // the nil-context guard is the point
	var nilDiag *CatchRateDiag
	nilDiag.observe("arm_a", "c1", CatchExpectCatch, 1, "", CatchTrial{})
}

func TestCatchRateDiagSummary(t *testing.T) {
	rule := CatchRule{Probes: []string{"untestable"}, CriterionIDs: []string{"AC-7"}}
	rules := map[string]map[string]catchRateArmCase{
		"zeta":  {"c1": {name: "c1", expect: CatchExpectCatch, rule: rule}, "c2": {name: "c2", expect: CatchExpectCatch, rule: rule}},
		"alpha": {"k1": {name: "k1", expect: CatchExpectClean, rule: CatchRule{Clean: true}}},
	}
	sink := &nopWriteCloser{}
	d, err := newCatchRateDiag(sink, "p", CatchRateDiagOptions{}, rules)
	if err != nil {
		t.Fatalf("newCatchRateDiag: %v", err)
	}
	obs := func(arm, c, expect, response string) {
		d.observe(arm, c, expect, 1, response, ClassifyCatchTrial(response, rules[arm][c].rule))
	}
	obs("zeta", "c1", CatchExpectCatch, concernVerdict([2]string{"Scope", "AC-7 is untestable"}, [2]string{"testing", "n"}))
	obs("zeta", "c1", CatchExpectCatch, concernVerdict([2]string{"scope", "AC-7 needs work"}))
	obs("zeta", "c2", CatchExpectCatch, `{"verdict":"reject","concerns":[{"severity":"high","category":"testing","note":"other","criterion_id":7}]}`)
	obs("zeta", "c2", CatchExpectCatch, "garbage")
	obs("alpha", "k1", CatchExpectClean, concernVerdict([2]string{"scope", "a note"}))
	text, err := d.Finish(nil)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	var sum catchDiagSummaryLine
	lines := strings.Split(strings.TrimSuffix(sink.String(), "\n"), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &sum); err != nil {
		t.Fatalf("decode the summary line: %v", err)
	}
	if sum.Outcome != "passed" || sum.TrialsWritten != 5 {
		t.Errorf("summary outcome %q trials_written %d, want passed and 5", sum.Outcome, sum.TrialsWritten)
	}
	z := sum.Arms["zeta"]
	if z.Trials != 4 || z.Decoded != 3 || z.CatchTrials != 4 || z.Caught != 1 || z.Undecodable != 1 || z.CleanTrials != 0 {
		t.Errorf("zeta counts = %+v", z)
	}
	if z.MeanConcernsPerDecodedTrial != 4.0/3.0 {
		t.Errorf("zeta mean concerns per decoded trial = %v, want 4/3", z.MeanConcernsPerDecodedTrial)
	}
	if z.Labels["scope"] != 2 || z.Labels["testing"] != 2 || len(z.Labels) != 2 {
		t.Errorf("zeta labels = %v, want scope 2 and testing 2 (trimmed, case-folded)", z.Labels)
	}
	if z.Verdicts["approve_with_concerns"] != 2 || z.Verdicts["reject"] != 1 || z.Verdicts["undecodable"] != 1 {
		t.Errorf("zeta verdicts = %v", z.Verdicts)
	}
	if z.MissClasses[CatchMissNoProbe] != 1 || z.MissClasses[CatchMissNotMentioned] != 1 || z.MissClasses[CatchMissUndecodable] != 1 || len(z.MissClasses) != 3 {
		t.Errorf("zeta miss classes = %v", z.MissClasses)
	}
	if c1, c2 := z.PerCase["c1"], z.PerCase["c2"]; c1.Trials != 2 || c1.Caught != 1 || c1.MissClasses[CatchMissNoProbe] != 1 ||
		c2.Trials != 2 || c2.Undecodable != 1 || c2.MissClasses[CatchMissNotMentioned] != 1 {
		t.Errorf("zeta per case = %+v", z.PerCase)
	}
	if a := sum.Arms["alpha"]; a.CleanTrials != 1 || a.FalseAlarms != 1 || len(a.MissClasses) != 0 || a.PerCase["k1"].FalseAlarms != 1 {
		t.Errorf("alpha = %+v", a)
	}
	// The criterion_id typed as a number still decodes (lenient).
	if !strings.Contains(sink.String(), `"criterion_id":"7"`) {
		t.Errorf("the numeric criterion_id did not surface: %s", sink.String())
	}
	// Render order: arms by name; histograms by count descending then name.
	if ia, iz := strings.Index(text, "  alpha:"), strings.Index(text, "  zeta:"); ia < 0 || iz < 0 || ia > iz {
		t.Errorf("arms not in name order:\n%s", text)
	}
	for _, want := range []string{"concern categories: scope 2, testing 2", "miss classes: no_probe 1, not_mentioned 1, undecodable 1", "c1: caught 1/2; no_probe 1"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary text lacks %q:\n%s", want, text)
		}
	}
}

func TestCatchDiagOutcome(t *testing.T) {
	for _, tc := range []struct {
		err       error
		notJudged bool
		want      string
	}{
		{nil, false, "passed"},
		{nil, true, "not_judged"},
		{ErrCatchRateRegressed, false, "refused"},
		{ErrPortabilityRepoSpecificFinding, false, "refused"},
		{ErrCatchRateSubsetNotRecordable, true, "refused"},
		{errors.New("connection reset"), false, "error"},
	} {
		if got := catchDiagOutcome(tc.err, tc.notJudged); got != tc.want {
			t.Errorf("catchDiagOutcome(%v, %v) = %q, want %q", tc.err, tc.notJudged, got, tc.want)
		}
	}
}

// The cross-layer check: diagnostics never alter scoring or the record.
func TestRecordCatchRateEvidenceWithDiagnostics_EvidenceUnchanged(t *testing.T) {
	k := catchesAtRate(t, 0.95)
	plain := filepath.Join(t.TempDir(), "evidence.json")
	diagnosed := filepath.Join(t.TempDir(), "evidence.json")
	if _, err := record(t, newScriptedCatchSender(t, k, k), plain, catchT0, pin("same")); err != nil {
		t.Fatalf("plain recording: %v", err)
	}
	diagPath := filepath.Join(t.TempDir(), CatchRateDiagFileName)
	report, err := RecordCatchRateEvidenceWithDiagnostics(context.Background(), newScriptedCatchSender(t, k, k), testCatchModel, committedCatchInputs(),
		diagnosed, catchT0, pin("same"), diagPath, CatchRateDiagOptions{})
	if err != nil {
		t.Fatalf("diagnosed recording: %v", err)
	}
	if !bytes.Equal(mustRead(t, plain), mustRead(t, diagnosed)) {
		t.Error("evidence.json differs between the plain and the diagnosed recording")
	}
	if !bytes.Equal(mustRead(t, CatchRateTrialsPath(plain)), mustRead(t, CatchRateTrialsPath(diagnosed))) {
		t.Error("evidence.trials.json differs between the plain and the diagnosed recording")
	}
	if !strings.Contains(report, "Trial sidecar written") || !strings.Contains(report, "Diagnostics: "+diagPath) {
		t.Errorf("report lacks the recorder's or the diagnostics' tail:\n%s", report)
	}
	if sum := catchDiagSummaryOf(t, readCatchDiagLines(t, diagPath)); sum["outcome"] != "passed" {
		t.Errorf("summary outcome = %v, want passed", sum["outcome"])
	}
}

// C1: raw_concerns is the response's own lenient decode (pre-control) and
// scored_concerns is tr.Concerns (post-control). The trial comes from the
// production persona classifier, whose severity cap clamps the emitted high.
// Fixture-state mechanism: with scored_concerns filled from the raw decode,
// the scored concern reads the emitted "high" with no severity_clamped_from;
// with raw_concerns filled from tr.Concerns, the raw concern reads the
// clamped severity and loses the convention and criterion_id fields.
func TestCatchRateDiag_RawAndScoredConcernsDiverge(t *testing.T) {
	response := `{"verdict":"reject","concerns":[{"severity":"high","category":"security","note":"the token reaches the log","convention":"conv-1","criterion_id":"AC-3"}]}`
	rule := CatchRule{Probes: []string{"untestable"}, CriterionIDs: []string{"AC-9"}}
	tr := ClassifyPersonaTrial(response, rule, nil, "medium")
	if tr.Outcome != CatchMissed || len(tr.Concerns) != 1 || tr.Concerns[0].SeverityClampedFrom != "high" || tr.Concerns[0].Severity == "high" {
		t.Fatalf("precondition: the persona classifier did not clamp the emitted high: %+v", tr)
	}
	sink := &nopWriteCloser{}
	d, err := newCatchRateDiag(sink, "p", CatchRateDiagOptions{}, nil)
	if err != nil {
		t.Fatalf("newCatchRateDiag: %v", err)
	}
	d.observe(ArmSecurity, "c1", CatchExpectCatch, 1, response, tr)
	var line catchDiagTrialLine
	lines := strings.Split(strings.TrimSuffix(sink.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d diagnostics lines, want the header and one trial", len(lines))
	}
	if err := json.Unmarshal([]byte(lines[1]), &line); err != nil {
		t.Fatalf("decode the trial line: %v", err)
	}
	if len(line.RawConcerns) != 1 || len(line.ScoredConcerns) != 1 {
		t.Fatalf("raw %d / scored %d concerns, want 1 / 1", len(line.RawConcerns), len(line.ScoredConcerns))
	}
	if r := line.RawConcerns[0]; r.Severity != "high" || r.Convention != "conv-1" || r.CriterionID != "AC-3" {
		t.Errorf("raw concern = %+v, want the emitted severity high with its convention and criterion_id", r)
	}
	if sc := line.ScoredConcerns[0]; sc.Severity != tr.Concerns[0].Severity || sc.SeverityClampedFrom != "high" {
		t.Errorf("scored concern = %+v, want the clamped severity %q from high", sc, tr.Concerns[0].Severity)
	}
}

// A table error still leaves a diagnostics file: the header, no trial line
// and an error summary, while RecordCatchRateEvidence's own table error is
// what the run returns. Fixture-state mechanism: with the table error
// returned before the open (`if err != nil { return "", err }`), no file
// exists, so readCatchDiagLines fails.
func TestRecordCatchRateEvidenceWithDiagnostics_TableErrorStillSummarized(t *testing.T) {
	in := committedCatchInputs()
	in.CorpusDir = filepath.Join(t.TempDir(), "no-such-corpus")
	_, tableErr := catchRateArmTable(in)
	if tableErr == nil {
		t.Fatal("precondition: the arm table built from a missing corpus")
	}
	all := committedCatchTrials(t)
	sender := newScriptedCatchSender(t, all, all)
	path := filepath.Join(t.TempDir(), "evidence.json")
	diagPath := filepath.Join(t.TempDir(), CatchRateDiagFileName)
	_, err := RecordCatchRateEvidenceWithDiagnostics(context.Background(), sender, testCatchModel, in, path, catchT0, pin("x"), diagPath, CatchRateDiagOptions{})
	if err == nil || !strings.Contains(err.Error(), tableErr.Error()) {
		t.Errorf("err = %v, want the arm table's error %q", err, tableErr)
	}
	if n := sender.total(); n != 0 {
		t.Errorf("sender called %d times, want 0", n)
	}
	lines := readCatchDiagLines(t, diagPath)
	if lines[0]["kind"] != "header" {
		t.Errorf("first line = %v, want the header", lines[0])
	}
	if n := len(catchDiagTrialLines(lines)); n != 0 {
		t.Errorf("%d trial lines, want 0", n)
	}
	if sum := catchDiagSummaryOf(t, lines); sum["outcome"] != "error" || !strings.Contains(sum["error"].(string), "corpus unavailable") {
		t.Errorf("summary = outcome %v error %v, want an error outcome naming the table error", sum["outcome"], sum["error"])
	}
}

// A subset dry run's summary reads not_judged.
func TestRecordCatchRateEvidenceWithDiagnostics_SubsetNotJudged(t *testing.T) {
	cases, _ := loadCommittedCatch(t)
	all := committedCatchTrials(t)
	diagPath := filepath.Join(t.TempDir(), CatchRateDiagFileName)
	report, err := RecordCatchRateEvidenceWithDiagnostics(context.Background(), newScriptedCatchSender(t, all, all), testCatchModel, committedCatchInputs(),
		filepath.Join(t.TempDir(), "evidence.json"), catchT0, RecordCatchRateOptions{DryRun: true, Subset: CatchRateSubset{Cases: []string{cases[0].Name}, TrialsPerCase: 1}},
		diagPath, CatchRateDiagOptions{})
	if err != nil {
		t.Fatalf("subset dry run: %v", err)
	}
	lines := readCatchDiagLines(t, diagPath)
	if sum := catchDiagSummaryOf(t, lines); sum["outcome"] != "not_judged" {
		t.Errorf("summary outcome = %v, want not_judged", sum["outcome"])
	}
	if h := lines[0]; h["subset_trials_per_case"] != float64(1) || h["dry_run"] != true {
		t.Errorf("header = %v, want the subset and dry-run flags", h)
	}
	if n := len(catchDiagTrialLines(lines)); n != 2 {
		t.Errorf("%d trial lines, want 2", n)
	}
	if !strings.Contains(report, "NOT judged") {
		t.Errorf("report:\n%s", report)
	}
}
