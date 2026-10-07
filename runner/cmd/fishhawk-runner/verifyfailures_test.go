package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// ---------------------------------------------------------------------------
// #4036 — pure helpers.
// ---------------------------------------------------------------------------

func TestIsFailureSignalLine(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"--- FAIL: TestX (0.00s)", true},
		{"    --- FAIL: TestX/sub (0.00s)", true},
		{"FAIL\texample.com/m/pkg\t0.12s", true},
		{"FAIL\texample.com/m/pkg [build failed]", true},
		{"FAIL\texample.com/m/pkg [setup failed]", true},
		{"FAIL", true},
		{"FAIL\r", true},
		{"panic: runtime error: index out of range", true},
		{"WARNING: DATA RACE", true},
		{"==================\tWARNING: DATA RACE", true},
		{"FAILED to connect", false},
		{"--- PASS: TestX (0.00s)", false},
		{"ok  \texample.com/m/pkg\t0.1s", false},
		{"some FAIL text", false},
		{"  panic: indented is a stack frame, not the banner", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isFailureSignalLine(c.line); got != c.want {
			t.Errorf("isFailureSignalLine(%q) = %t, want %t", c.line, got, c.want)
		}
	}
}

func TestParseVerifyFailures(t *testing.T) {
	t.Run("tests, subtests, packages, dedupe in order", func(t *testing.T) {
		out := strings.Join([]string{
			"=== RUN   TestA",
			"    --- FAIL: TestA/sub (0.00s)",
			"--- FAIL: TestA (0.00s)",
			"--- FAIL: TestA (0.00s)",
			"FAIL",
			"FAIL\texample.com/m/pkg/a\t0.10s",
			"FAIL\texample.com/m/pkg/a\t0.10s",
			"--- FAIL: TestB (0.00s)\r",
			"FAIL\texample.com/m/pkg/b\t0.20s\r",
		}, "\n")
		got := parseVerifyFailures(out)
		if want := []string{"TestA/sub", "TestA", "TestB"}; !reflect.DeepEqual(got.tests, want) {
			t.Errorf("tests = %q, want %q", got.tests, want)
		}
		if want := []string{"example.com/m/pkg/a", "example.com/m/pkg/b"}; !reflect.DeepEqual(got.pkgs, want) {
			t.Errorf("pkgs = %q, want %q", got.pkgs, want)
		}
		if len(got.buildFailed) != 0 {
			t.Errorf("buildFailed = %q, want none", got.buildFailed)
		}
	})
	t.Run("build and setup failures are recorded", func(t *testing.T) {
		out := "# example.com/m/pkg/b\npkg/b/b.go:3: undefined: a.Old\nFAIL\texample.com/m/pkg/b [build failed]\nFAIL\texample.com/m/pkg/c [setup failed]\n"
		got := parseVerifyFailures(out)
		if want := []string{"example.com/m/pkg/b", "example.com/m/pkg/c"}; !reflect.DeepEqual(got.pkgs, want) || !reflect.DeepEqual(got.buildFailed, want) {
			t.Errorf("pkgs = %q buildFailed = %q, want both %q", got.pkgs, got.buildFailed, want)
		}
	})
	t.Run("entry count and entry length are capped", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 15; i++ {
			fmt.Fprintf(&b, "--- FAIL: Test%02d (0.00s)\nFAIL\texample.com/m/p%02d\t0.1s\n", i, i)
		}
		long := strings.Repeat("L", 300)
		got := parseVerifyFailures(b.String())
		if len(got.tests) != verifyFailureParseMaxEntries || len(got.pkgs) != verifyFailureParseMaxEntries {
			t.Errorf("len(tests)=%d len(pkgs)=%d, want both %d", len(got.tests), len(got.pkgs), verifyFailureParseMaxEntries)
		}
		got = parseVerifyFailures("--- FAIL: " + long + " (0.00s)\n")
		if len(got.tests) != 1 || len(got.tests[0]) != verifyFailureParseMaxEntryBytes {
			t.Errorf("tests = %q, want one entry capped at %d bytes", got.tests, verifyFailureParseMaxEntryBytes)
		}
	})
	t.Run("a lossy package list is flagged", func(t *testing.T) {
		pkgLines := func(n int) string {
			var b strings.Builder
			for i := 0; i < n; i++ {
				fmt.Fprintf(&b, "FAIL\texample.com/m/p%02d\t0.1s\n", i)
			}
			return b.String()
		}
		testLines := func(n int) string {
			var b strings.Builder
			for i := 0; i < n; i++ {
				fmt.Fprintf(&b, "--- FAIL: T%02d (0.00s)\n", i)
			}
			return b.String()
		}
		cases := []struct {
			name string
			out  string
			want bool
		}{
			{"exactly the cap is complete", pkgLines(verifyFailureParseMaxEntries), false},
			{"a repeat past the cap drops nothing", pkgLines(verifyFailureParseMaxEntries) + "FAIL\texample.com/m/p00\t0.1s\n", false},
			{"a distinct package past the cap is dropped", pkgLines(verifyFailureParseMaxEntries + 1), true},
			{"a path cut at the byte cap", "FAIL\texample.com/" + strings.Repeat("x", verifyFailureParseMaxEntryBytes) + "/pkg/a\t0.1s\n", true},
			{"tests past the cap do not flag packages", testLines(verifyFailureParseMaxEntries+5) + "FAIL\texample.com/m/pkg/b\t0.1s\n", false},
		}
		for _, c := range cases {
			if got := parseVerifyFailures(c.out).pkgsLossy; got != c.want {
				t.Errorf("%s: pkgsLossy = %t, want %t", c.name, got, c.want)
			}
		}
	})
	t.Run("no failure lines", func(t *testing.T) {
		got := parseVerifyFailures("ok  \texample.com/m/pkg\t0.1s\nlint: 1 issue\n")
		if len(got.tests)+len(got.pkgs)+len(got.buildFailed) != 0 {
			t.Errorf("got %+v, want nothing parsed", got)
		}
	})
}

func TestVerifyFailureScopeRelation(t *testing.T) {
	scope := []string{"pkg/a"}
	cases := []struct {
		name  string
		f     verifyFailures
		scope []string
		want  string
	}{
		{"outside", verifyFailures{pkgs: []string{"example.com/m/pkg/b"}}, scope, verifyRelationOutside},
		{"every package outside", verifyFailures{pkgs: []string{"example.com/m/pkg/b", "example.com/m/pkg/c"}}, scope, verifyRelationOutside},
		{"inside by import-path suffix", verifyFailures{pkgs: []string{"example.com/m/pkg/a"}}, scope, verifyRelationInside},
		{"inside by equality", verifyFailures{pkgs: []string{"pkg/a"}}, scope, verifyRelationInside},
		{"one inside among outside", verifyFailures{pkgs: []string{"example.com/m/pkg/b", "example.com/m/pkg/a"}}, scope, verifyRelationInside},
		{"suffix collision errs inside", verifyFailures{pkgs: []string{"example.com/other/pkg/a"}}, scope, verifyRelationInside},
		{"partial segment is not a suffix match", verifyFailures{pkgs: []string{"example.com/m/xpkg/a"}}, scope, verifyRelationOutside},
		{"subpackage of a scope dir is outside", verifyFailures{pkgs: []string{"example.com/m/pkg/a/sub"}}, scope, verifyRelationOutside},
		{"unknown: no failing package", verifyFailures{tests: []string{"TestX"}}, scope, verifyRelationUnknown},
		{"unknown: empty scope", verifyFailures{pkgs: []string{"example.com/m/pkg/b"}}, nil, verifyRelationUnknown},
		{"unknown: root scope", verifyFailures{pkgs: []string{"example.com/m/pkg/b"}}, []string{".", "pkg/a"}, verifyRelationUnknown},
		// #4036 approval condition 1: a build/setup failure is never a flake.
		{"build failed outside is inside", verifyFailures{pkgs: []string{"example.com/m/pkg/b"}, buildFailed: []string{"example.com/m/pkg/b"}}, scope, verifyRelationInside},
		{"setup failed outside is inside", verifyFailures{pkgs: []string{"example.com/m/pkg/c"}, buildFailed: []string{"example.com/m/pkg/c"}}, scope, verifyRelationInside},
		{"build failed beats an empty scope", verifyFailures{pkgs: []string{"example.com/m/pkg/b"}, buildFailed: []string{"example.com/m/pkg/b"}}, nil, verifyRelationInside},
		// A lossy package list (an entry dropped past the cap, or a path cut
		// at the byte cap) can never answer outside.
		{"lossy all-outside is unknown", verifyFailures{pkgs: []string{"example.com/m/pkg/b"}, pkgsLossy: true}, scope, verifyRelationUnknown},
		{"lossy with a parsed inside package is inside", verifyFailures{pkgs: []string{"example.com/m/pkg/b", "example.com/m/pkg/a"}, pkgsLossy: true}, scope, verifyRelationInside},
	}
	for _, c := range cases {
		if got := verifyFailureScopeRelation(c.f, c.scope); got != c.want {
			t.Errorf("%s: verifyFailureScopeRelation(%+v, %q) = %q, want %q", c.name, c.f, c.scope, got, c.want)
		}
	}
	// End to end through the parser: a `[build failed]` line for an outside
	// package must classify inside.
	if got := verifyFailureScopeRelation(parseVerifyFailures("FAIL\texample.com/m/pkg/b [build failed]\n"), scope); got != verifyRelationInside {
		t.Errorf("parsed [build failed] outside package = %q, want inside", got)
	}
	// End to end at the parse caps: classification stays conservative when a
	// package entry is omitted or truncated.
	outsideLines := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "FAIL\texample.com/m/out%02d\t0.1s\n", i)
		}
		return b.String()
	}
	longScope := []string{"pkg/" + strings.Repeat("d", verifyFailureParseMaxEntryBytes)}
	ends := []struct {
		name  string
		out   string
		scope []string
		want  string
	}{
		{"exactly the cap, all outside", outsideLines(verifyFailureParseMaxEntries), scope, verifyRelationOutside},
		{"the cap of outside, then an in-scope package", outsideLines(verifyFailureParseMaxEntries) + "FAIL\texample.com/m/pkg/a\t0.1s\n", scope, verifyRelationUnknown},
		{"one past the cap, all outside", outsideLines(verifyFailureParseMaxEntries + 1), scope, verifyRelationUnknown},
		{"a path whose scope suffix is cut at the byte cap", "FAIL\texample.com/m/" + longScope[0] + "\t0.1s\n", longScope, verifyRelationUnknown},
	}
	for _, e := range ends {
		if got := verifyFailureScopeRelation(parseVerifyFailures(e.out), e.scope); got != e.want {
			t.Errorf("%s: relation = %q, want %q", e.name, got, e.want)
		}
	}
}

func TestIsNoopFixIteration(t *testing.T) {
	cases := []struct {
		name      string
		prev, cur string
		fixed     bool
		want      bool
	}{
		{"empty previous tree", "", "t1", true, false},
		{"empty current tree", "t1", "", true, false},
		{"no fix since the last verify", "t1", "t1", false, false},
		{"tree changed", "t1", "t2", true, false},
		{"identical tree after a fix", "t1", "t1", true, true},
	}
	for _, c := range cases {
		if got := isNoopFixIteration(c.prev, c.cur, c.fixed); got != c.want {
			t.Errorf("%s: isNoopFixIteration(%q, %q, %t) = %t, want %t", c.name, c.prev, c.cur, c.fixed, got, c.want)
		}
	}
}

func TestRetainFailureSignalLines(t *testing.T) {
	numbered := func(n int, signals map[int]string) []string {
		lines := make([]string, n)
		for i := range lines {
			if s, ok := signals[i]; ok {
				lines[i] = s
			} else {
				lines[i] = fmt.Sprintf("ctx-%02d", i)
			}
		}
		return lines
	}
	join := func(lines []string) string { return strings.Join(lines, "\n") + "\n" }

	t.Run("no signal line yields an empty block", func(t *testing.T) {
		block, retained, kept, omitted := retainFailureSignalLines(join(numbered(30, nil)), 1<<10)
		if block != "" || retained != 0 || kept != 0 || omitted != 0 {
			t.Errorf("got (%q, %d, %d, %d), want all zero", block, retained, kept, omitted)
		}
	})
	t.Run("overlapping windows merge", func(t *testing.T) {
		lines := numbered(12, map[int]string{3: "--- FAIL: TestA (0.00s)", 5: "FAIL\tex/pkg\t0.1s"})
		block, retained, kept, _ := retainFailureSignalLines(join(lines), 1<<10)
		want := join(lines[1:10]) // [3-2, 5+4]
		if block != want {
			t.Errorf("block =\n%s\nwant\n%s", block, want)
		}
		if retained != len(want) || kept != 2 {
			t.Errorf("retained=%d kept=%d, want %d, 2", retained, kept, len(want))
		}
	})
	t.Run("disjoint windows are separated", func(t *testing.T) {
		lines := numbered(30, map[int]string{2: "panic: boom", 20: "WARNING: DATA RACE"})
		block, retained, kept, _ := retainFailureSignalLines(join(lines), 1<<10)
		want := join(lines[0:7]) + verifyFixSignalSeparator + join(lines[18:25])
		if block != want {
			t.Errorf("block =\n%s\nwant\n%s", block, want)
		}
		if retained != len(want)-len(verifyFixSignalSeparator) || kept != 2 {
			t.Errorf("retained=%d kept=%d, want %d, 2", retained, kept, len(want)-len(verifyFixSignalSeparator))
		}
	})
	t.Run("long lines are capped with a truncation suffix", func(t *testing.T) {
		long := "--- FAIL: TestLong " + strings.Repeat("x", 3000)
		block, retained, kept, _ := retainFailureSignalLines(long+"\n", 8<<10)
		if kept != 1 || !strings.HasPrefix(block, long[:verifyFixSignalLineMaxBytes]) {
			t.Fatalf("kept=%d block=%.80q, want the capped signal line", kept, block)
		}
		if !strings.Contains(block, fmt.Sprintf("[... line truncated, %d more bytes]", len(long)-verifyFixSignalLineMaxBytes)) {
			t.Errorf("block lacks the truncation suffix: %.200q", block[len(block)-100:])
		}
		if retained != verifyFixSignalLineMaxBytes+1 {
			t.Errorf("retained = %d, want %d (capped prefix + newline)", retained, verifyFixSignalLineMaxBytes+1)
		}
	})
	t.Run("budget overflow counts omitted lines and stays bounded", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 100; i++ {
			fmt.Fprintf(&b, "FAIL\texample.com/m/pkg%03d\t0.01s\n", i)
		}
		const budget = 1000
		block, _, kept, omitted := retainFailureSignalLines(b.String(), budget)
		if len(block) > budget {
			t.Errorf("len(block) = %d, want <= budget %d", len(block), budget)
		}
		if kept == 0 || omitted == 0 || kept+omitted != 100 {
			t.Errorf("kept=%d omitted=%d, want both > 0 summing to 100", kept, omitted)
		}
	})
}

// ---------------------------------------------------------------------------
// #4036 — the no-op fix detection and the flake re-run, driven through the
// REAL runVerifyFixLoop.
//
// The fixture is a real repo whose base commit carries pkg/a/a.go; the
// agent's in-scope edit MODIFIES it, so StageScoped commits and the scoped
// package set is [pkg/a]. The verify command is scripted per CALL from an
// invocation counter outside the throwaway clone, against the real
// scoped-then-full sequence: an iteration whose scoped form passes makes two
// calls (FISHHAWK_VERIFY_PACKAGES set on the first only), one whose scoped
// form fails makes one. Every call is recorded and checked against the
// script, so a fixture that drifts from the loop's real call sequence fails.
// ---------------------------------------------------------------------------

type noopVerifyStep struct {
	scoped bool // the call must carry FISHHAWK_VERIFY_PACKAGES
	out    string
	rc     int
	hang   bool // print out, then sleep past the verify timeout
}

const (
	noopPassOut    = "ok  \texample.com/m/pkg/a\t0.01s\n"
	noopOutsideOut = "=== RUN   TestFlaky\n--- FAIL: TestFlaky (0.01s)\n    flaky_test.go:9: timing\nFAIL\nFAIL\texample.com/m/pkg/b\t0.02s\n"
	noopInsideOut  = "--- FAIL: TestInside (0.00s)\nFAIL\nFAIL\texample.com/m/pkg/a\t0.01s\n"
	noopBuildOut   = "# example.com/m/pkg/b\npkg/b/b.go:3:9: undefined: a.Old\nFAIL\texample.com/m/pkg/b [build failed]\n"
	// noopLintOut is a lint-leg failure: no `FAIL\t<pkg>` line, so the
	// relation is unknown.
	noopLintOut = "pkg/a/a.go:3:1: exported const X should have comment (revive)\n1 issues:\n* revive: 1\nscripts/test: lint failed in runner\n"
)

// noopOverflowOut is a multi-module aggregate failure whose in-scope package
// is reported AFTER verifyFailureParseMaxEntries outside packages.
var noopOverflowOut = func() string {
	var b strings.Builder
	for i := 0; i < verifyFailureParseMaxEntries; i++ {
		fmt.Fprintf(&b, "--- FAIL: TestOut%02d (0.00s)\nFAIL\texample.com/m/out%02d\t0.01s\n", i, i)
	}
	b.WriteString("--- FAIL: TestInside (0.00s)\nFAIL\texample.com/m/pkg/a\t0.01s\n")
	return b.String()
}()

func scopedPass() noopVerifyStep           { return noopVerifyStep{scoped: true, out: noopPassOut} }
func scopedFail(out string) noopVerifyStep { return noopVerifyStep{scoped: true, out: out, rc: 1} }
func fullPass() noopVerifyStep             { return noopVerifyStep{out: noopPassOut} }
func fullFail(out string) noopVerifyStep   { return noopVerifyStep{out: out, rc: 1} }

type noopLoopFixture struct {
	cfg   config
	repo  string
	dir   string
	steps []noopVerifyStep
}

func newNoopLoopFixture(t *testing.T, maxIter int, steps ...noopVerifyStep) *noopLoopFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, runGit := compileGateRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, "pkg", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, "pkg", "a", "a.go"), "package a\n")
	runGit("add", "-A")
	runGit("commit", "-m", "base", "--no-verify")
	// The agent's in-scope edit: MODIFY the tracked Go file.
	mustWrite(t, filepath.Join(repo, "pkg", "a", "a.go"), "package a\n\n// agent change\n")

	dir := t.TempDir()
	for i, s := range steps {
		mustWrite(t, filepath.Join(dir, fmt.Sprintf("out.%d", i+1)), s.out)
		mustWrite(t, filepath.Join(dir, fmt.Sprintf("rc.%d", i+1)), fmt.Sprintf("%d\n", s.rc))
		if s.hang {
			mustWrite(t, filepath.Join(dir, fmt.Sprintf("hang.%d", i+1)), "")
		}
	}
	script := filepath.Join(dir, "verify.sh")
	mustWrite(t, script, fmt.Sprintf(`#!/bin/sh
d=%q
n=$(cat "$d/count" 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > "$d/count"
printf '%%s pkgs=[%%s]\n' "$n" "$%s" >> "$d/calls.log"
if [ -f "$d/out.$n" ]; then cat "$d/out.$n"; if [ -f "$d/hang.$n" ]; then sleep 300; fi; exit "$(cat "$d/rc.$n")"; fi
echo "verify fixture: unscripted call $n"
exit 97
`, dir, verifyPackagesEnvVar))
	return &noopLoopFixture{
		cfg: config{
			runID:               verifiedTreeRunID,
			stageID:             verifiedTreeStageID,
			workingDir:          repo,
			verifyCmd:           "sh " + script,
			verifyMaxIterations: maxIter,
			scopeFiles:          []upload.ScopeFile{{Path: "pkg/a/a.go", Operation: "modify"}},
		},
		repo:  repo,
		dir:   dir,
		steps: steps,
	}
}

// assertCalls checks the recorded verify calls against the script: the same
// count, and FISHHAWK_VERIFY_PACKAGES set exactly on the scoped calls.
func (f *noopLoopFixture) assertCalls(t *testing.T) {
	t.Helper()
	lines := readVerifyFormLog(t, filepath.Join(f.dir, "calls.log"))
	if len(lines) != len(f.steps) {
		t.Errorf("verify calls = %d, want %d scripted: %q", len(lines), len(f.steps), lines)
	}
	for i, l := range lines {
		if i >= len(f.steps) {
			break
		}
		if scoped := !strings.Contains(l, "pkgs=[]"); scoped != f.steps[i].scoped {
			t.Errorf("verify call %d = %q, want scoped=%t", i+1, l, f.steps[i].scoped)
		}
	}
}

// cappedInvoker is a fix agent that changes nothing (unless edit is set) and
// FAILS the test past a hard call cap, so a missing refund bound reddens
// instead of looping.
func cappedInvoker(t *testing.T, limit int, edit func(callIdx int)) *fakeInvoker {
	return &fakeInvoker{
		canned: agent.Result{OK: true},
		onInvoke: func(i int, _ agent.Invocation) {
			if i >= limit {
				t.Fatalf("fix invoker called %d times, past the hard cap %d — the no-op refund is unbounded", i+1, limit)
			}
			if edit != nil {
				edit(i)
			}
		},
	}
}

func (f *noopLoopFixture) run(t *testing.T, inv *fakeInvoker) (agent.Result, string, string) {
	t.Helper()
	res := agent.Result{OK: true}
	var logSink strings.Builder
	_, tree, err := runVerifyFixLoop(context.Background(), &f.cfg, nil, "", inv, agent.Invocation{}, &res, &logSink)
	if err != nil {
		t.Fatalf("runVerifyFixLoop: %v\n%s", err, logSink.String())
	}
	f.assertCalls(t)
	return res, tree, logSink.String()
}

func eventPayloads(t *testing.T, res agent.Result, kind string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range res.Events {
		if ev.Kind != kind {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("%s payload: %v", kind, err)
		}
		out = append(out, p)
	}
	return out
}

func noopSummaryOf(t *testing.T, res agent.Result) map[string]any {
	t.Helper()
	s := eventPayloads(t, res, "verify_summary")
	if len(s) != 1 {
		t.Fatalf("verify_summary events = %d, want 1", len(s))
	}
	return s[0]
}

func countAdmitted(noops []map[string]any) (admitted int) {
	for _, p := range noops {
		if p["admitted"] == true {
			admitted++
		}
	}
	return admitted
}

// M1: a no-op fix of an outside-change failure, and the identical tree then
// passes — recorded as a flake re-run, not as a fix.
func TestRunVerifyFixLoop_NoopOutsideFailureRerunPasses(t *testing.T) {
	f := newNoopLoopFixture(t, 1, scopedPass(), fullFail(noopOutsideOut), scopedPass(), fullPass())
	inv := cappedInvoker(t, 3, nil)
	res, tree, log := f.run(t, inv)

	if !res.OK || tree == "" {
		t.Fatalf("the identical tree passed the full form, so the stage passes with a verified tree: OK=%t tree=%q\n%s", res.OK, tree, log)
	}
	if inv.callIdx != 1 {
		t.Errorf("fix invokes = %d, want exactly 1", inv.callIdx)
	}
	noops := eventPayloads(t, res, verifyFixNoopEvent)
	if len(noops) != 1 || noops[0]["admitted"] != true || noops[0]["relation"] != verifyRelationOutside {
		t.Fatalf("verify_fix_noop = %v, want one admitted=true relation=outside\n%s", noops, log)
	}
	if !reflect.DeepEqual(noops[0]["failing_tests"], []any{"TestFlaky"}) || noops[0]["tree_sha"] == "" {
		t.Errorf("verify_fix_noop payload = %v, want failing_tests [TestFlaky] and a tree_sha", noops[0])
	}
	reruns := eventPayloads(t, res, verifyFlakeRerunEvent)
	if len(reruns) != 1 || reruns[0]["outcome"] != "passed" {
		t.Errorf("verify_flake_rerun = %v, want one outcome=passed", reruns)
	}
	sum := noopSummaryOf(t, res)
	detail, _ := sum["detail"].(string)
	if sum["outcome"] != "passed" || !strings.HasPrefix(detail, verifyFlakeRerunLead) || !strings.Contains(detail, "TestFlaky") {
		t.Errorf("verify_summary = %v, want outcome=passed with a %s detail naming TestFlaky", sum, verifyFlakeRerunLead)
	}
	if !reflect.DeepEqual(sum["flaky_tests"], []any{"TestFlaky"}) {
		t.Errorf("verify_summary flaky_tests = %v, want [TestFlaky]", sum["flaky_tests"])
	}
}

// M2: the identical tree fails again entirely outside the change — category
// C with the outside-change lead, and no further fix re-invoke.
func TestRunVerifyFixLoop_NoopOutsideFailureReproducedIsCategoryC(t *testing.T) {
	f := newNoopLoopFixture(t, 2, scopedPass(), fullFail(noopOutsideOut), scopedPass(), fullFail(noopOutsideOut))
	inv := cappedInvoker(t, 3, nil)
	res, tree, log := f.run(t, inv)

	if res.OK || res.FailureCategory != "C" {
		t.Fatalf("OK=%t category=%q, want a category-C failure\n%s", res.OK, res.FailureCategory, log)
	}
	if !strings.HasPrefix(res.FailureReason, verifyFailureOutsideChangeLead) || !strings.Contains(res.FailureReason, "TestFlaky") {
		t.Errorf("FailureReason = %.300q, want the %s lead naming TestFlaky", res.FailureReason, verifyFailureOutsideChangeLead)
	}
	if tree != "" {
		t.Errorf("verified tree = %q, want empty", tree)
	}
	if inv.callIdx != 1 {
		t.Errorf("fix invokes = %d, want exactly 1 (a reproduced outside failure is not re-invoked)", inv.callIdx)
	}
	reruns := eventPayloads(t, res, verifyFlakeRerunEvent)
	if len(reruns) != 1 || reruns[0]["outcome"] != "failed" {
		t.Errorf("verify_flake_rerun = %v, want one outcome=failed", reruns)
	}
	sum := noopSummaryOf(t, res)
	if detail, _ := sum["detail"].(string); sum["outcome"] != "failed" || !strings.HasPrefix(detail, verifyFailureOutsideChangeLead) {
		t.Errorf("verify_summary = %v, want outcome=failed with the outside-change detail", sum)
	}
}

// M3: the flake re-run fails INSIDE the change — the no-op iteration is
// refunded, so max_iterations=1 still yields a second fix re-invoke.
func TestRunVerifyFixLoop_NoopFlakeRerunFailingInsideIsRefunded(t *testing.T) {
	f := newNoopLoopFixture(t, 1, scopedPass(), fullFail(noopOutsideOut), scopedFail(noopInsideOut), scopedPass(), fullPass())
	inv := cappedInvoker(t, 3, nil)
	res, tree, log := f.run(t, inv)

	if inv.callIdx != 2 {
		t.Fatalf("fix invokes = %d, want 2 (the refunded no-op iteration consumes no fix iteration)\n%s", inv.callIdx, log)
	}
	if !res.OK || tree == "" {
		t.Errorf("OK=%t tree=%q, want the final pass", res.OK, tree)
	}
	if !strings.Contains(log, `"event":"verify_flake_rerun_refunded"`) {
		t.Errorf("log lacks verify_flake_rerun_refunded:\n%s", log)
	}
	if n := len(eventPayloads(t, res, verifyFlakeRerunEvent)); n != 0 {
		t.Errorf("verify_flake_rerun events = %d, want 0 (the re-run failed inside the change)", n)
	}
	if noops := eventPayloads(t, res, verifyFixNoopEvent); countAdmitted(noops) != 1 {
		t.Errorf("admitted no-ops = %d, want 1: %v", countAdmitted(noops), noops)
	}
}

// M4: a no-op fix of a failure INSIDE the change is recorded but never
// admitted as a flake re-run; exhaustion demotes category A as before.
func TestRunVerifyFixLoop_NoopInsideFailureIsNotAdmitted(t *testing.T) {
	f := newNoopLoopFixture(t, 1, scopedFail(noopInsideOut), scopedFail(noopInsideOut))
	inv := cappedInvoker(t, 3, nil)
	res, _, log := f.run(t, inv)

	noops := eventPayloads(t, res, verifyFixNoopEvent)
	if len(noops) != 1 || noops[0]["admitted"] != false || noops[0]["relation"] != verifyRelationInside {
		t.Fatalf("verify_fix_noop = %v, want one admitted=false relation=inside\n%s", noops, log)
	}
	if n := len(eventPayloads(t, res, verifyFlakeRerunEvent)); n != 0 {
		t.Errorf("verify_flake_rerun events = %d, want 0", n)
	}
	if res.OK || res.FailureCategory != "A" {
		t.Errorf("OK=%t category=%q, want category A on exhaustion", res.OK, res.FailureCategory)
	}
	if inv.callIdx != 1 {
		t.Errorf("fix invokes = %d, want 1", inv.callIdx)
	}
}

// M5: the flake re-run is ONCE per stage. Alternating outside/inside failures
// would re-admit every outside no-op without the bound; with it, a second
// outside no-op is recorded admitted=false and the stage spends exactly one
// refund.
func TestRunVerifyFixLoop_FlakeRerunIsOncePerStage(t *testing.T) {
	f := newNoopLoopFixture(t, 2,
		scopedPass(), fullFail(noopOutsideOut), // iter 0: outside → fix 1 (no-op)
		scopedFail(noopInsideOut),              // iter 1: admitted re-run fails inside → refund, fix 2
		scopedPass(), fullFail(noopOutsideOut), // iter 1: no-op (inside given) → fix 3
		scopedFail(noopInsideOut), // iter 2: outside no-op, bound spent → exhausted
	)
	const maxIter = 2
	inv := cappedInvoker(t, maxIter+1, nil)
	res, _, log := f.run(t, inv)

	if inv.callIdx != maxIter+1 {
		t.Errorf("fix invokes = %d, want max_iterations+1 = %d (exactly one refund)\n%s", inv.callIdx, maxIter+1, log)
	}
	noops := eventPayloads(t, res, verifyFixNoopEvent)
	if countAdmitted(noops) != 1 {
		t.Errorf("admitted no-ops = %d, want 1: %v", countAdmitted(noops), noops)
	}
	spent := false
	for _, p := range noops {
		if p["admitted"] == false && p["relation"] == verifyRelationOutside {
			spent = true
		}
	}
	if !spent {
		t.Errorf("want a second outside no-op recorded admitted=false (bound spent): %v", noops)
	}
	if res.FailureCategory != "A" {
		t.Errorf("category = %q, want A", res.FailureCategory)
	}
}

// M6: a genuine fix (the agent edits the scope file) is never a no-op.
func TestRunVerifyFixLoop_GenuineFixIsNotANoop(t *testing.T) {
	f := newNoopLoopFixture(t, 1, scopedPass(), fullFail(noopOutsideOut), scopedPass(), fullPass())
	inv := cappedInvoker(t, 3, func(int) {
		mustWrite(t, filepath.Join(f.repo, "pkg", "a", "a.go"), "package a\n\n// agent change\n// the fix\n")
	})
	res, tree, log := f.run(t, inv)

	if !res.OK || tree == "" {
		t.Fatalf("OK=%t tree=%q, want a pass\n%s", res.OK, tree, log)
	}
	if n := len(eventPayloads(t, res, verifyFixNoopEvent)); n != 0 {
		t.Errorf("verify_fix_noop events = %d, want 0 for a genuine fix", n)
	}
	if n := len(eventPayloads(t, res, verifyFlakeRerunEvent)); n != 0 {
		t.Errorf("verify_flake_rerun events = %d, want 0", n)
	}
	if sum := noopSummaryOf(t, res); sum["detail"] != nil || sum["flaky_tests"] != nil {
		t.Errorf("verify_summary = %v, want no flake detail on a genuine fix", sum)
	}
}

// M7 (#4036 approval condition 2): after an admitted no-op, the re-run's
// output carries an infra signature → the infra absorb repeats the verify on
// the SAME tree without invoking the agent. That repeat must not read as a
// second no-op: exactly ONE verify_fix_noop, no second admission or refund.
func TestRunVerifyFixLoop_InfraAbsorbAfterNoopIsNotASecondNoop(t *testing.T) {
	f := newNoopLoopFixture(t, 1,
		scopedPass(), fullFail(noopOutsideOut), // iter 0 → fix 1 (no-op)
		scopedPass(), fullFail(lintLockOutput2645+"\n"+noopOutsideOut), // iter 1: admitted re-run → infra absorb
		scopedPass(), fullPass(), // absorb repeat on the identical tree passes
	)
	inv := cappedInvoker(t, 3, nil)
	res, tree, log := f.run(t, inv)

	if !res.OK || tree == "" {
		t.Fatalf("OK=%t tree=%q, want a pass\n%s", res.OK, tree, log)
	}
	if inv.callIdx != 1 {
		t.Errorf("fix invokes = %d, want 1", inv.callIdx)
	}
	if n := len(eventPayloads(t, res, "verify_infra_flake_retry")); n != 1 {
		t.Errorf("verify_infra_flake_retry events = %d, want 1", n)
	}
	noops := eventPayloads(t, res, verifyFixNoopEvent)
	if len(noops) != 1 || countAdmitted(noops) != 1 {
		t.Errorf("verify_fix_noop = %v, want exactly ONE (admitted) — an absorb repeat is not a no-op fix\n%s", noops, log)
	}
	if strings.Contains(log, "verify_flake_rerun_refunded") {
		t.Errorf("no refund may fire:\n%s", log)
	}
	if reruns := eventPayloads(t, res, verifyFlakeRerunEvent); len(reruns) != 1 || reruns[0]["outcome"] != "passed" {
		t.Errorf("verify_flake_rerun = %v, want one outcome=passed (the marker survives the absorb)", reruns)
	}
}

// #4036 approval condition 1: a no-op fix of an OUTSIDE `[build failed]`
// package is a deterministic artifact defect — no flake re-run, no category C.
func TestRunVerifyFixLoop_NoopOutsideBuildFailureIsNotAFlake(t *testing.T) {
	f := newNoopLoopFixture(t, 1, scopedPass(), fullFail(noopBuildOut), scopedPass(), fullFail(noopBuildOut))
	inv := cappedInvoker(t, 3, nil)
	res, _, log := f.run(t, inv)

	noops := eventPayloads(t, res, verifyFixNoopEvent)
	if len(noops) != 1 || noops[0]["admitted"] != false || noops[0]["relation"] != verifyRelationInside {
		t.Fatalf("verify_fix_noop = %v, want one admitted=false relation=inside\n%s", noops, log)
	}
	if n := len(eventPayloads(t, res, verifyFlakeRerunEvent)); n != 0 {
		t.Errorf("verify_flake_rerun events = %d, want 0", n)
	}
	if res.OK || res.FailureCategory != "A" {
		t.Errorf("OK=%t category=%q, want category A (never C for a build failure)", res.OK, res.FailureCategory)
	}
	if inv.callIdx != 1 {
		t.Errorf("fix invokes = %d, want 1", inv.callIdx)
	}
}

// The tree-resolution degrade: a scope-only tree that cannot be resolved
// leaves detection OFF (fail-open to the pre-#4036 loop). Driven through the
// pure seam — runVerifyFixLoop discards the gitRevParseTreeOf error and an
// empty curTree can never match.
func TestIsNoopFixIteration_UnresolvableTreeDisablesDetection(t *testing.T) {
	cur, err := gitRevParseTreeOf(context.Background(), t.TempDir(), "HEAD")
	if err == nil || cur != "" {
		t.Fatalf("gitRevParseTreeOf on a non-repo = (%q, %v), want an error and an empty tree", cur, err)
	}
	if isNoopFixIteration("", cur, true) || isNoopFixIteration("t1", cur, true) {
		t.Error("an unresolvable current tree must never detect a no-op")
	}
}

// A more specific cause wins: a reproduced outside-change failure on an
// OVERLOADED host is the host_overloaded category C, and verify_summary does
// not carry the outside-change detail the FailureReason contradicts.
func TestRunVerifyFixLoop_HostOverloadedBeatsOutsideChange(t *testing.T) {
	withHostLoad(t, overloadedSample(), nil)
	f := newNoopLoopFixture(t, 2, scopedPass(), fullFail(noopOutsideOut), scopedPass(), fullFail(noopOutsideOut))
	res, _, log := f.run(t, cappedInvoker(t, 3, nil))

	if res.FailureCategory != "C" || !strings.HasPrefix(res.FailureReason, "host_overloaded:") {
		t.Fatalf("category=%q reason=%.120q, want C with the host_overloaded: lead\n%s", res.FailureCategory, res.FailureReason, log)
	}
	if detail, _ := noopSummaryOf(t, res)["detail"].(string); strings.HasPrefix(detail, verifyFailureOutsideChangeLead) {
		t.Errorf("verify_summary detail = %q, want no outside-change detail when host_overloaded decides", detail)
	}
}

// A refund on an UNKNOWN relation: the admitted re-run fails in the lint leg
// (no `FAIL\t<pkg>` line — lint runs before the tests, so this is a realistic
// failure). Unknown is refunded exactly like inside: max_iterations=1 still
// yields a second fix re-invoke, and it is never category C.
func TestRunVerifyFixLoop_NoopFlakeRerunFailingUnknownIsRefunded(t *testing.T) {
	f := newNoopLoopFixture(t, 1,
		scopedPass(), fullFail(noopOutsideOut), // iter 0 → fix 1 (no-op)
		scopedFail(noopLintOut),  // iter 1: admitted re-run fails in lint → refund, fix 2
		scopedPass(), fullPass(), // iter 1 again → pass
	)
	inv := cappedInvoker(t, 3, nil)
	res, tree, log := f.run(t, inv)

	if inv.callIdx != 2 {
		t.Fatalf("fix invokes = %d, want 2 (an unknown re-run failure refunds the no-op iteration)\n%s", inv.callIdx, log)
	}
	if !res.OK || tree == "" {
		t.Errorf("OK=%t tree=%q, want the final pass", res.OK, tree)
	}
	if !strings.Contains(log, `"event":"verify_flake_rerun_refunded"`) {
		t.Errorf("log lacks verify_flake_rerun_refunded:\n%s", log)
	}
	if n := len(eventPayloads(t, res, verifyFlakeRerunEvent)); n != 0 {
		t.Errorf("verify_flake_rerun events = %d, want 0 (the re-run failed, undecidably)", n)
	}
	noops := eventPayloads(t, res, verifyFixNoopEvent)
	if countAdmitted(noops) != 1 {
		t.Errorf("admitted no-ops = %d, want 1: %v", countAdmitted(noops), noops)
	}
	if len(noops) != 2 || noops[1]["relation"] != verifyRelationUnknown {
		t.Errorf("verify_fix_noop = %v, want a second, unadmitted no-op with relation=unknown", noops)
	}
}

// The flakeRerun marker survives a verify-lock contention re-run at the
// admitted iteration: the contended verify never judged the tree, its in-place
// repeat is not a second no-op, and the repeat's pass settles the flake re-run.
func TestRunVerifyFixLoop_FlakeRerunSurvivesVerifyLockContention(t *testing.T) {
	f := newNoopLoopFixture(t, 1,
		scopedPass(), fullFail(noopOutsideOut), // iter 0 → fix 1 (no-op)
		scopedFail(lockRefusalFixtureOutput), // iter 1: admitted re-run is lock-contended → in-place repeat
		scopedPass(), fullPass(),             // the repeat on the identical tree passes
	)
	inv := cappedInvoker(t, 3, nil)
	res, tree, log := f.run(t, inv)

	if !res.OK || tree == "" {
		t.Fatalf("OK=%t tree=%q, want a pass\n%s", res.OK, tree, log)
	}
	if inv.callIdx != 1 {
		t.Errorf("fix invokes = %d, want 1", inv.callIdx)
	}
	if n := countEvents(res.Events, "verify_lock_contended_retry"); n != 1 {
		t.Errorf("verify_lock_contended_retry events = %d, want 1", n)
	}
	noops := eventPayloads(t, res, verifyFixNoopEvent)
	if len(noops) != 1 || countAdmitted(noops) != 1 {
		t.Errorf("verify_fix_noop = %v, want exactly ONE (admitted) — a lock re-run is not a no-op fix\n%s", noops, log)
	}
	if strings.Contains(log, "verify_flake_rerun_refunded") {
		t.Errorf("no refund may fire:\n%s", log)
	}
	if reruns := eventPayloads(t, res, verifyFlakeRerunEvent); len(reruns) != 1 || reruns[0]["outcome"] != "passed" {
		t.Errorf("verify_flake_rerun = %v, want one outcome=passed (the marker survives the lock re-run)", reruns)
	}
	if detail, _ := noopSummaryOf(t, res)["detail"].(string); !strings.HasPrefix(detail, verifyFlakeRerunLead) {
		t.Errorf("verify_summary detail = %q, want the %s lead", detail, verifyFlakeRerunLead)
	}
}

// A gateTimedOut verify at the admitted iteration breaks BEFORE the flake
// block: the stage ends with the timed-out category C, never the outside-change
// lead, and neither a flake-rerun verdict nor a refund is recorded.
func TestRunVerifyFixLoop_FlakeRerunTimedOutIsTimedOutNotOutsideChange(t *testing.T) {
	f := newNoopLoopFixture(t, 2,
		scopedPass(), fullFail(noopOutsideOut), // iter 0 → fix 1 (no-op)
		noopVerifyStep{scoped: true, out: noopOutsideOut, rc: 1, hang: true}, // iter 1: admitted re-run hangs
	)
	// Every call but the last must COMPLETE inside the deadline (#3587).
	f.cfg.verifyTimeout = fastExitSafeVerifyTimeout()
	inv := cappedInvoker(t, 3, nil)
	res, tree, log := f.run(t, inv)

	if res.OK || tree != "" || res.FailureCategory != "C" || !strings.HasPrefix(res.FailureReason, verifyGateTimedOutLead+": ") {
		t.Fatalf("OK=%t tree=%q category=%q reason=%.160q, want C with the %q lead\n%s", res.OK, tree, res.FailureCategory, res.FailureReason, verifyGateTimedOutLead, log)
	}
	if inv.callIdx != 1 {
		t.Errorf("fix invokes = %d, want 1 (a timed-out verify never reaches the fix agent)", inv.callIdx)
	}
	if noops := eventPayloads(t, res, verifyFixNoopEvent); len(noops) != 1 || countAdmitted(noops) != 1 {
		t.Errorf("verify_fix_noop = %v, want one admitted", noops)
	}
	if n := len(eventPayloads(t, res, verifyFlakeRerunEvent)); n != 0 {
		t.Errorf("verify_flake_rerun events = %d, want 0 (a timeout is no verdict)", n)
	}
	if strings.Contains(log, "verify_flake_rerun_refunded") {
		t.Errorf("no refund may fire on a timeout:\n%s", log)
	}
	if detail, _ := noopSummaryOf(t, res)["detail"].(string); strings.HasPrefix(detail, verifyFailureOutsideChangeLead) || strings.HasPrefix(detail, verifyFlakeRerunLead) {
		t.Errorf("verify_summary detail = %q, want neither flake-rerun lead", detail)
	}
}

// The parse cap must not admit a flake: a multi-module aggregate whose
// in-scope failure follows verifyFailureParseMaxEntries outside packages is
// unknown, so the no-op is NOT admitted and a reproduced failure stays
// category A, never verify_failure_outside_change.
func TestRunVerifyFixLoop_NoopOverflowingFailureListIsNotAdmitted(t *testing.T) {
	f := newNoopLoopFixture(t, 1, scopedPass(), fullFail(noopOverflowOut), scopedPass(), fullFail(noopOverflowOut))
	inv := cappedInvoker(t, 3, nil)
	res, _, log := f.run(t, inv)

	noops := eventPayloads(t, res, verifyFixNoopEvent)
	if len(noops) != 1 || noops[0]["admitted"] != false || noops[0]["relation"] != verifyRelationUnknown {
		t.Fatalf("verify_fix_noop = %v, want one admitted=false relation=unknown\n%s", noops, log)
	}
	if n := len(eventPayloads(t, res, verifyFlakeRerunEvent)); n != 0 {
		t.Errorf("verify_flake_rerun events = %d, want 0", n)
	}
	if res.OK || res.FailureCategory != "A" || strings.HasPrefix(res.FailureReason, verifyFailureOutsideChangeLead) {
		t.Errorf("OK=%t category=%q reason=%.120q, want category A on exhaustion, never outside-change", res.OK, res.FailureCategory, res.FailureReason)
	}
	if inv.callIdx != 1 {
		t.Errorf("fix invokes = %d, want 1", inv.callIdx)
	}
}
