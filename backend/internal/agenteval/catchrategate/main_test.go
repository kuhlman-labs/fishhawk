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
	testCorpus      = "../testdata/planreview-miss-corpus"
	testConventions = "../testdata/planreview-catchrate/representative-conventions.md"
)

// allCaughtSender answers every call with a verdict whose concern matches
// every committed case's first probe.
type allCaughtSender struct{ response string }

func newAllCaughtSender(t *testing.T) *allCaughtSender {
	t.Helper()
	cases, err := agenteval.LoadPlanReviewCatchCorpus(testCorpus)
	if err != nil {
		t.Fatal(err)
	}
	var probes []string
	for _, c := range cases {
		probes = append(probes, c.Input.CatchProbes[0])
	}
	b, err := json.Marshal(map[string]any{"verdict": "approve_with_concerns", "concerns": []any{
		map[string]any{"severity": "medium", "category": "acceptance_criteria", "note": strings.Join(probes, "; ")},
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
		testCorpus, conventions, path, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC),
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
	for _, want := range []string{"catchrategate: PASS", "MORE than 0.10 below", "at least 136 trials", "Pinned baseline"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	assertRuleLine(t, stdout)
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
	for name, c := range rec.Arms.WithConventions.PerCase {
		c.Caught = 0
		rec.Arms.WithConventions.PerCase[name] = c
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
		{"print-fingerprint", []string{"--print-fingerprint", "--evidence", "x", "--corpus", testCorpus, "--conventions", testConventions}, 0, false},
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
	for _, args := range [][]string{{"--no-such-flag"}, {"positional"}} {
		if code, _, stderr := runGate(args...); code != 2 {
			t.Errorf("args %v: exit %d, want 2 (stderr %s)", args, code, stderr)
		}
	}
}

// TestRun_PrintFingerprintDefaults resolves the committed default paths from
// the package directory and prints the same digest the library computes.
func TestRun_PrintFingerprintDefaults(t *testing.T) {
	cases, err := agenteval.LoadPlanReviewCatchCorpus(testCorpus)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := agenteval.LoadRepresentativeConventions(testConventions)
	if err != nil {
		t.Fatal(err)
	}
	want, err := agenteval.CatchRatePromptFingerprint(cases, conv, agenteval.DefaultCatchRateGeneratorModel)
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
