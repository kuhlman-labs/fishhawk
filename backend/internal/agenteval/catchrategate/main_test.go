package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/agenteval"
)

const (
	testCorpus        = "../testdata/planreview-miss-corpus"
	testConventions   = "../testdata/planreview-catchrate/representative-conventions.md"
	testPersonaCorpus = "../testdata/planreview-persona-corpus/architect"
	testRepoRoot      = "../../../.."
)

// testInputs is the committed arm-table inputs with the given conventions
// fixture.
func testInputs(conventions string) agenteval.CatchRateInputs {
	return agenteval.CatchRateInputs{CorpusDir: testCorpus, ConventionsPath: conventions, PersonaCorpusDir: testPersonaCorpus, RepoRoot: testRepoRoot}
}

// allCaughtSender answers every call with a verdict whose ONE concern names
// every committed case's planted criterion id(s) and first probe, so it
// satisfies both halves of every conventions case's catch rule. The architect
// arm gets the same answer: an acceptance_criteria concern, so its catch
// cases are missed (the decision_record category predicate) and its clean
// cases raise no false alarm — a passing first pin either way.
type allCaughtSender struct{ response string }

func newAllCaughtSender(t *testing.T) *allCaughtSender {
	t.Helper()
	cases, err := agenteval.LoadPlanReviewCatchCorpus(testCorpus)
	if err != nil {
		t.Fatal(err)
	}
	var terms []string
	for _, c := range cases {
		terms = append(terms, c.CatchRule().CriterionIDs...)
		terms = append(terms, c.Input.CatchProbes[0])
	}
	b, err := json.Marshal(map[string]any{"verdict": "approve_with_concerns", "concerns": []any{
		map[string]any{"severity": "medium", "category": "acceptance_criteria", "note": strings.Join(terms, "; ")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return &allCaughtSender{response: string(b)}
}

func (s *allCaughtSender) Messages(context.Context, string, string) (string, string, int, int, int, int, error) {
	return s.response, "fake-model", 0, 0, 0, 0, nil
}

// freshRecord records a pinned, passing record for the gate's own model
// against the committed corpus and the given conventions file.
func freshRecord(t *testing.T, conventions string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evidence.json")
	_, err := agenteval.RecordCatchRateEvidence(context.Background(), newAllCaughtSender(t), agenteval.DefaultCatchRateGeneratorModel,
		testInputs(conventions), path, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC),
		agenteval.RecordCatchRateOptions{PinBaseline: true, Reason: "test baseline"})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	return path
}

func runGate(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRun_PassesOnFreshRecord(t *testing.T) {
	path := freshRecord(t, testConventions)
	code, stdout, stderr := runGate("--evidence", path, "--corpus", testCorpus, "--conventions", testConventions)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"catchrategate: PASS", "MORE than 0.10 below", "at least 136 trials", "Pinned baseline",
		"catchrategate: trial sidecar (per-trial concern notes, near-miss tags): " + agenteval.CatchRateTrialsPath(path)} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	assertRuleLine(t, stdout)
}

// TestRun_FailsWithoutTheTrialSidecar is the cross-boundary pin: a pair
// recorded through the real RecordCatchRateEvidence passes the real CLI, and
// the same record with its sidecar removed exits 1 naming the sidecar.
func TestRun_FailsWithoutTheTrialSidecar(t *testing.T) {
	path := freshRecord(t, testConventions)
	sidecar := agenteval.CatchRateTrialsPath(path)
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("the recording wrote no sidecar at %s: %v", sidecar, err)
	}
	if code, _, stderr := runGate("--evidence", path, "--corpus", testCorpus, "--conventions", testConventions); code != 0 {
		t.Fatalf("premise: the fresh pair passes; exit %d, stderr:\n%s", code, stderr)
	}
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runGate("--evidence", path, "--corpus", testCorpus, "--conventions", testConventions)
	if code != 1 || !strings.Contains(stderr, "trial sidecar") || !strings.Contains(stderr, sidecar) || !strings.Contains(stderr, "absent") {
		t.Fatalf("exit %d, stderr:\n%s\nwant exit 1 naming the absent sidecar %s", code, stderr, sidecar)
	}
	if strings.Contains(stdout, "catchrategate: PASS") {
		t.Fatalf("stdout reports a PASS:\n%s", stdout)
	}
	assertRuleLine(t, stderr)
}

func TestRun_FailsOnStaleRecord(t *testing.T) {
	conv, err := os.ReadFile(testConventions)
	if err != nil {
		t.Fatal(err)
	}
	conv[len(conv)-2] ^= 0x01
	staleConv := filepath.Join(t.TempDir(), "conventions.md")
	if err := os.WriteFile(staleConv, conv, 0o644); err != nil {
		t.Fatal(err)
	}
	path := freshRecord(t, staleConv)
	code, _, stderr := runGate("--evidence", path, "--corpus", testCorpus, "--conventions", testConventions)
	if code != 1 || !strings.Contains(stderr, "STALE") {
		t.Fatalf("exit %d, stderr:\n%s\nwant exit 1 naming STALE", code, stderr)
	}
	assertRuleLine(t, stderr)
}

func TestRun_FailsOnRegressedRecordWithReport(t *testing.T) {
	path := freshRecord(t, testConventions)
	var rec agenteval.CatchRateEvidence
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	for name, c := range rec.Arms[agenteval.ArmWithConventions].PerCase {
		c.Caught = 0
		rec.Arms[agenteval.ArmWithConventions].PerCase[name] = c
	}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runGate("--evidence", path, "--corpus", testCorpus, "--conventions", testConventions)
	if code != 1 || !strings.Contains(stderr, "Verdict: REGRESSED") || !strings.Contains(stderr, "FAIL") {
		t.Fatalf("exit %d, stderr:\n%s\nwant exit 1 with the rendered report", code, stderr)
	}
}

func TestRun_FailsOnAbsentRecord(t *testing.T) {
	code, _, stderr := runGate("--evidence", filepath.Join(t.TempDir(), "evidence.json"), "--corpus", testCorpus, "--conventions", testConventions)
	if code != 1 || !strings.Contains(stderr, "absent") {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	assertRuleLine(t, stderr)
}

// assertRuleLine requires the gate's rule line — not merely a report that
// happens to carry the numbers — with the 0.10 tolerance and the 136-trial
// minimum in it.
func assertRuleLine(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "catchrategate: rule:") {
			if !strings.Contains(line, "0.10") || !strings.Contains(line, "136") {
				t.Errorf("rule line lacks the 0.10 tolerance or the 136-trial minimum: %q", line)
			}
			if !strings.Contains(out, "catchrategate: per-arm rule:") || !strings.Contains(out, "false-alarm rate") || !strings.Contains(out, "architect") {
				t.Errorf("output carries no per-arm rule line naming the false-alarm rate and the architect arm:\n%s", out)
			}
			return
		}
	}
	t.Errorf("output carries no 'catchrategate: rule:' line naming 0.10 and 136:\n%s", out)
}

// TestRun_PrintsTheRuleOnEveryOutcome: the rule line (0.10 tolerance, 136
// trials per arm, the derivation reference) is printed whatever the outcome —
// absent, stale, malformed and regressed evidence, infrastructure failures,
// usage errors, --print-fingerprint and a pass — on stdout for a pass and on
// stderr otherwise.
func TestRun_PrintsTheRuleOnEveryOutcome(t *testing.T) {
	dir := t.TempDir()
	staleConv := filepath.Join(dir, "conventions.md")
	conv, err := os.ReadFile(testConventions)
	if err != nil {
		t.Fatal(err)
	}
	conv[len(conv)-2] ^= 0x01
	if err := os.WriteFile(staleConv, conv, 0o644); err != nil {
		t.Fatal(err)
	}
	stale := freshRecord(t, staleConv)
	fresh := freshRecord(t, testConventions)
	trailing := filepath.Join(dir, "trailing.json")
	raw, err := os.ReadFile(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trailing, append(raw, []byte("{}\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		args     []string
		wantCode int
		onStdout bool
	}{
		{"absent evidence", []string{"--evidence", filepath.Join(dir, "none.json"), "--corpus", testCorpus, "--conventions", testConventions}, 1, false},
		{"stale evidence", []string{"--evidence", stale, "--corpus", testCorpus, "--conventions", testConventions}, 1, false},
		{"evidence with trailing content", []string{"--evidence", trailing, "--corpus", testCorpus, "--conventions", testConventions}, 1, false},
		{"infrastructure: corpus unavailable", []string{"--evidence", fresh, "--corpus", filepath.Join(dir, "no-corpus"), "--conventions", testConventions}, 1, false},
		{"infrastructure: conventions unavailable", []string{"--evidence", fresh, "--corpus", testCorpus, "--conventions", filepath.Join(dir, "none.md")}, 1, false},
		{"infrastructure: print-fingerprint corpus unavailable", []string{"--print-fingerprint", "--evidence", "x", "--corpus", filepath.Join(dir, "no-corpus"), "--conventions", testConventions}, 1, false},
		{"usage error", []string{"--no-such-flag"}, 2, false},
		{"usage error: rescore with print-fingerprint", []string{"--rescore", "--print-fingerprint", "--evidence", fresh, "--corpus", testCorpus}, 2, false},
		{"print-fingerprint", []string{"--print-fingerprint", "--evidence", "x", "--corpus", testCorpus, "--conventions", testConventions}, 0, false},
		{"rescore", []string{"--rescore", "--evidence", fresh, "--corpus", testCorpus}, 0, false},
		{"rescore: absent sidecar", []string{"--rescore", "--evidence", filepath.Join(dir, "none.json"), "--corpus", testCorpus}, 1, false},
		{"pass", []string{"--evidence", fresh, "--corpus", testCorpus, "--conventions", testConventions}, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runGate(tc.args...)
			if code != tc.wantCode {
				t.Fatalf("exit %d, want %d; stderr:\n%s", code, tc.wantCode, stderr)
			}
			if tc.onStdout {
				assertRuleLine(t, stdout)
			} else {
				assertRuleLine(t, stderr)
			}
		})
	}
	t.Run("infrastructure: outside the checkout", func(t *testing.T) {
		t.Chdir(t.TempDir())
		code, _, stderr := runGate()
		if code != 1 {
			t.Fatalf("exit %d, want 1; stderr:\n%s", code, stderr)
		}
		assertRuleLine(t, stderr)
	})
}

// TestRun_EvidenceWithTrailingContentFails: an otherwise passing record with a
// second object or garbage appended fails closed rather than passing on its
// first value.
func TestRun_EvidenceWithTrailingContentFails(t *testing.T) {
	fresh := freshRecord(t, testConventions)
	raw, err := os.ReadFile(fresh)
	if err != nil {
		t.Fatal(err)
	}
	for name, suffix := range map[string]string{"second object": "{\"schema\":\"x\"}\n", "garbage": "garbage\n"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "evidence.json")
			if err := os.WriteFile(path, append(append([]byte{}, raw...), suffix...), 0o644); err != nil {
				t.Fatal(err)
			}
			code, _, stderr := runGate("--evidence", path, "--corpus", testCorpus, "--conventions", testConventions)
			if code != 1 || !strings.Contains(stderr, "trailing content") {
				t.Fatalf("exit %d, stderr:\n%s\nwant exit 1 naming the trailing content", code, stderr)
			}
		})
	}
}

func TestRun_UsageErrors(t *testing.T) {
	for _, args := range [][]string{{"--no-such-flag"}, {"positional"}, {"--rescore", "--print-fingerprint"}} {
		if code, _, stderr := runGate(args...); code != 2 {
			t.Errorf("args %v: exit %d, want 2 (stderr %s)", args, code, stderr)
		}
	}
}

// TestRun_PrintFingerprintDefaults resolves the committed default paths from
// the package directory and prints the same digest the library computes.
func TestRun_PrintFingerprintDefaults(t *testing.T) {
	want, err := agenteval.CatchRatePromptFingerprint(testInputs(testConventions), agenteval.DefaultCatchRateGeneratorModel)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runGate("--print-fingerprint")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if got := strings.TrimSpace(stdout); got != want {
		t.Fatalf("--print-fingerprint = %q, want %q", got, want)
	}
}

func TestRun_PrintFingerprintFailsClosed(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"--print-fingerprint", "--evidence", "x", "--corpus", filepath.Join(dir, "none"), "--conventions", testConventions},
		{"--print-fingerprint", "--evidence", "x", "--corpus", testCorpus, "--conventions", filepath.Join(dir, "none.md")},
	} {
		if code, _, stderr := runGate(args...); code != 1 || !strings.Contains(stderr, "absent") {
			t.Errorf("args %v: exit %d, stderr %s; want exit 1 naming the absent input", args, code, stderr)
		}
	}
}

func TestRun_DefaultsOutsideTheCheckoutFailClosed(t *testing.T) {
	t.Chdir(t.TempDir())
	code, _, stderr := runGate()
	if code != 1 || !strings.Contains(stderr, "cannot locate the backend module root") {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
}

func TestFindBackendRoot(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := findBackendRoot(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(root) != "backend" {
		t.Fatalf("root = %q, want the backend module dir", root)
	}
	fromRepoRoot, err := findBackendRoot(filepath.Dir(root))
	if err != nil || fromRepoRoot != root {
		t.Fatalf("from the repo root: %q, %v; want %q", fromRepoRoot, err, root)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "go.mod"), []byte("module example.com/other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isBackendModule(filepath.Join(other, "go.mod")) {
		t.Fatal("a go.mod for another module was taken as the backend's")
	}
}

// TestRun_RescorePrintsReportAndNeverWrites drives --rescore end to end over a
// pair recorded through the real RecordCatchRateEvidence: exit 0 with the
// re-score report on stdout, never a PASS (the rule line goes to stderr), and
// the evidence record, its sidecar and their directory left byte-identical.
func TestRun_RescorePrintsReportAndNeverWrites(t *testing.T) {
	path := freshRecord(t, testConventions)
	sidecar := agenteval.CatchRateTrialsPath(path)
	snapshot := func() map[string]string {
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		files := map[string]string{}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(filepath.Dir(path), e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			files[e.Name()] = string(b)
		}
		return files
	}
	before := snapshot()
	if _, ok := before[filepath.Base(sidecar)]; !ok || len(before) != 2 {
		t.Fatalf("premise: the recording wrote exactly the record and its sidecar, got %d files", len(before))
	}
	code, stdout, stderr := runGate("--rescore", "--evidence", path, "--corpus", testCorpus)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"OFFLINE RE-SCORE", "lower bound", "attribution-only", "Sidecar: " + sidecar, "without_conventions: recorded ",
		"architect: recorded ", "architect clean cases: recorded "} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "catchrategate: PASS") || strings.Contains(stdout, "catchrategate: rule:") {
		t.Errorf("stdout carries a PASS or the rule line:\n%s", stdout)
	}
	assertRuleLine(t, stderr)
	after := snapshot()
	if len(after) != len(before) {
		t.Fatalf("the re-score changed the evidence directory: %d files before, %d after", len(before), len(after))
	}
	for name, b := range before {
		if after[name] != b {
			t.Errorf("the re-score modified %s", name)
		}
	}
}

// TestRun_RescoreDefaults resolves the committed sidecar and corpus from the
// package directory.
func TestRun_RescoreDefaults(t *testing.T) {
	code, stdout, stderr := runGate("--rescore")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "OFFLINE RE-SCORE") || !strings.Contains(stdout, filepath.Join("testdata", "planreview-catchrate", "evidence.trials.json")) {
		t.Errorf("stdout does not re-score the committed sidecar:\n%s", stdout)
	}
	assertRuleLine(t, stderr)
}

func TestRun_RescoreFailsOnAbsentSidecar(t *testing.T) {
	evidence := filepath.Join(t.TempDir(), "evidence.json")
	code, stdout, stderr := runGate("--rescore", "--evidence", evidence, "--corpus", testCorpus)
	if code != 1 || !strings.Contains(stderr, agenteval.CatchRateTrialsPath(evidence)) || !strings.Contains(stderr, "absent") {
		t.Fatalf("exit %d, stderr:\n%s\nwant exit 1 naming the absent sidecar %s", code, stderr, agenteval.CatchRateTrialsPath(evidence))
	}
	if stdout != "" {
		t.Errorf("a failed re-score printed to stdout:\n%s", stdout)
	}
	assertRuleLine(t, stderr)
}

// TestRun_RescoreWithPrintFingerprintIsUsage: the two modes are exclusive;
// neither runs.
func TestRun_RescoreWithPrintFingerprintIsUsage(t *testing.T) {
	code, stdout, stderr := runGate("--rescore", "--print-fingerprint", "--evidence", "x", "--corpus", testCorpus, "--conventions", testConventions)
	if code != 2 || !strings.Contains(stderr, "--rescore and --print-fingerprint are mutually exclusive") {
		t.Fatalf("exit %d, stderr:\n%s\nwant exit 2 naming the exclusivity", code, stderr)
	}
	if stdout != "" {
		t.Errorf("a usage error printed to stdout:\n%s", stdout)
	}
}

// TestRun_FailsOnLegacyV1Record: a legacy planreview-catchrate-evidence-v1
// record (the committed pair until the shared re-pin) exits 1 naming the
// shared #4313/#4314 re-recording, with the rule line on stderr.
func TestRun_FailsOnLegacyV1Record(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.json")
	if err := os.WriteFile(path, []byte(`{"schema":"planreview-catchrate-evidence-v1","arms":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runGate("--evidence", path)
	if code != 1 || !strings.Contains(stderr, "legacy planreview-catchrate-evidence-v1") || !strings.Contains(stderr, "#4313/#4314 re-pin recording") {
		t.Fatalf("exit %d, stderr:\n%s\nwant exit 1 naming the legacy record and the shared re-recording", code, stderr)
	}
	if strings.Contains(stdout, "catchrategate: PASS") {
		t.Fatalf("stdout reports a PASS:\n%s", stdout)
	}
	assertRuleLine(t, stderr)
}

// TestRun_PersonaFlags: --persona-corpus and --repo-root reach the arm table —
// an absent persona corpus or a checkout root without the architect documents
// fails the gate closed, naming the persona corpus.
func TestRun_PersonaFlags(t *testing.T) {
	fresh := freshRecord(t, testConventions)
	if code, _, stderr := runGate("--evidence", fresh, "--persona-corpus", testPersonaCorpus, "--repo-root", testRepoRoot); code != 0 {
		t.Fatalf("premise: explicit persona flags pass; exit %d, stderr:\n%s", code, stderr)
	}
	for name, args := range map[string][]string{
		"absent persona corpus":      {"--persona-corpus", filepath.Join(t.TempDir(), "none")},
		"repo root without the ADRs": {"--repo-root", t.TempDir()},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := runGate(append([]string{"--evidence", fresh}, args...)...)
			if code != 1 || !strings.Contains(stderr, "persona corpus unavailable") {
				t.Fatalf("exit %d, stderr:\n%s\nwant exit 1 naming the persona corpus", code, stderr)
			}
			assertRuleLine(t, stderr)
		})
	}
}
