package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gateiso"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// Slice gate wiring (#5498): the hook, runVerifyFixLoop's slice-gate arm and
// the single-shot runVerifyGateCommitted, over a REAL materialized clone and a
// real `go list`. The base commit is a go.mod module with a, b (imports a) and
// c; the agent's change is a/a.go, so the reverse-import reach is [a, b].

// sliceGateLoopRepo commits the base module and applies the agent's edit.
func sliceGateLoopRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	repo, runGit := compileGateRepo(t)
	mustWrite(t, filepath.Join(repo, "go.mod"), "module example.com/sg\n\ngo 1.22\n")
	for rel, body := range map[string]string{
		"a/a.go": "package a\n",
		"b/b.go": "package b\n\nimport _ \"example.com/sg/a\"\n",
		"c/c.go": "package c\n",
	} {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, p, body)
	}
	runGit("add", "-A")
	runGit("commit", "-m", "base", "--no-verify")
	mustWrite(t, filepath.Join(repo, "a", "a.go"), "package a\n\n// agent change\n")
	return repo
}

// sliceGateSeqRecorder is a verify command that records each invocation's
// package set and absorb nonce, then exits codes[n-1] (the last code repeats).
func sliceGateSeqRecorder(t *testing.T, codes ...int) (cmd, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	var arms strings.Builder
	for i, c := range codes {
		fmt.Fprintf(&arms, "%d) exit %d;; ", i+1, c)
	}
	script := filepath.Join(dir, "verify.sh")
	mustWrite(t, script, fmt.Sprintf(`#!/bin/sh
printf 'pkgs=[%%s] nonce=[%%s]\n' "$%s" "$%s" >> %q
n=$(wc -l < %q | tr -d ' ')
case "$n" in %s*) exit %d;; esac
`, verifyPackagesEnvVar, verifyAbsorbNonceEnvVar, logPath, logPath, arms.String(), codes[len(codes)-1]))
	return "sh " + script, logPath
}

var sliceGateOffered = &upload.SliceGateConfig{FullGatePaths: []string{"scripts/**", "Makefile"}, InertPaths: []string{"docs/**"}}

func sliceGateChildCfg(repo, verifyCmd string) config {
	return config{
		runID: "run-sg", stageID: "stage-sg", workingDir: repo, verifyCmd: verifyCmd,
		verifyMaxIterations: 1, verifyTimeout: time.Minute,
		scopeFiles:          []upload.ScopeFile{{Path: "a/a.go", Operation: "modify"}},
		decomposedFromRunID: "parent-run", sliceGate: sliceGateOffered,
	}
}

// sliceGateCountLoads wraps sliceGateLoadFn and returns the call counter.
func sliceGateCountLoads(t *testing.T) *int {
	t.Helper()
	n := new(int)
	orig := sliceGateLoadFn
	sliceGateLoadFn = func(ctx context.Context, wt string) (map[string][]string, map[string]bool, string) {
		*n++
		return orig(ctx, wt)
	}
	t.Cleanup(func() { sliceGateLoadFn = orig })
	return n
}

func sliceGateWithSelection(t *testing.T, sel gateiso.Selection) {
	t.Helper()
	orig := sliceGateSelectionFn
	sliceGateSelectionFn = func(context.Context) gateiso.Selection { return sel }
	t.Cleanup(func() { sliceGateSelectionFn = orig })
}

func sliceGateRunLoop(t *testing.T, cfg *config, inv *fakeInvoker) (agent.Result, string, string) {
	t.Helper()
	if inv == nil {
		inv = &fakeInvoker{canned: agent.Result{OK: true}}
	}
	res := agent.Result{OK: true}
	var logSink strings.Builder
	_, tree, err := runVerifyFixLoop(context.Background(), cfg, nil, "", inv, agent.Invocation{}, &res, &logSink)
	if err != nil {
		t.Fatalf("runVerifyFixLoop: %v\n%s", err, logSink.String())
	}
	return res, tree, logSink.String()
}

func sliceGateEvents(events []agent.Event, kind string) []map[string]any {
	var out []map[string]any
	for _, ev := range events {
		if ev.Kind != kind {
			continue
		}
		var p map[string]any
		_ = json.Unmarshal(ev.Payload, &p)
		out = append(out, p)
	}
	return out
}

func sliceGateSummaryOf(t *testing.T, res agent.Result) map[string]any {
	t.Helper()
	s := sliceGateEvents(res.Events, "verify_summary")
	if len(s) != 1 {
		t.Fatalf("verify_summary events = %d, want 1", len(s))
	}
	return s[0]
}

func sliceGatePkgs(lines []string) []string {
	var out []string
	for _, l := range lines {
		i := strings.Index(l, "pkgs=[")
		j := strings.Index(l[i:], "]")
		out = append(out, l[i+len("pkgs="):i+j+1])
	}
	return out
}

// (21) TestRunVerifyFixLoop_SliceGateRunsOneScopedReachVerify: an eligible
// child runs exactly ONE verify, scoped to the reach [a, b], and the pass
// yields the #960 tree, a slice_scoped verify_summary and one event.
func TestRunVerifyFixLoop_SliceGateRunsOneScopedReachVerify(t *testing.T) {
	repo := sliceGateLoopRepo(t)
	cmd, logPath := sliceGateSeqRecorder(t, 0)
	cfg := sliceGateChildCfg(repo, cmd)
	loads := sliceGateCountLoads(t)

	res, tree, log := sliceGateRunLoop(t, &cfg, nil)
	got := sliceGatePkgs(readVerifyFormLog(t, logPath))
	if !reflect.DeepEqual(got, []string{"[a,b]"}) {
		t.Fatalf("verify invocations = %q, want exactly one [a,b]\n%s", got, log)
	}
	if tree == "" || !res.OK {
		t.Errorf("a scoped pass must yield a #960 tree and OK; tree=%q ok=%v", tree, res.OK)
	}
	s := sliceGateSummaryOf(t, res)
	if s["gate_shape"] != sliceGateShapeScoped || s["gate_packages_count"] != float64(2) {
		t.Errorf("verify_summary gate shape = %v / %v, want slice_scoped / 2", s["gate_shape"], s["gate_packages_count"])
	}
	if d, _ := s["detail"].(string); d != sliceGateScopedDetail {
		t.Errorf("detail = %q, want the scoped-shape sentence", d)
	}
	ev := sliceGateEvents(res.Events, sliceGateEventKind)
	if len(ev) != 1 || ev[0]["shape"] != sliceGateShapeScoped {
		t.Errorf("verify_slice_gate events = %v, want one slice_scoped", ev)
	}
	if *loads != 1 {
		t.Errorf("reach loads = %d, want 1", *loads)
	}
	if !strings.Contains(got[0], "a,b") || strings.Contains(readVerifyFormLog(t, logPath)[0], "nonce=[]") {
		// The nonce rides every hooked verify even when it is not kept.
		t.Errorf("the hooked verify must carry an absorb nonce: %q", readVerifyFormLog(t, logPath))
	}
}

// (22) a scope path on the declared full-gate surface keeps today's shape.
func TestRunVerifyFixLoop_SliceGateGlobalPathFallsBackToFull(t *testing.T) {
	repo := sliceGateLoopRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, "scripts", "test"), "#!/bin/sh\n")
	cmd, logPath := sliceGateSeqRecorder(t, 0)
	cfg := sliceGateChildCfg(repo, cmd)
	cfg.scopeFiles = append(cfg.scopeFiles, upload.ScopeFile{Path: "scripts/test", Operation: "create"})

	res, _, _ := sliceGateRunLoop(t, &cfg, nil)
	got := sliceGatePkgs(readVerifyFormLog(t, logPath))
	if !reflect.DeepEqual(got, []string{"[a]", "[]"}) {
		t.Fatalf("verify invocations = %q, want today's [a] then []", got)
	}
	s := sliceGateSummaryOf(t, res)
	if s["gate_shape"] != sliceGateShapeFull || s["gate_shape_reason"] != "global_surface:scripts/test" {
		t.Errorf("gate shape = %v / %v, want full / global_surface:scripts/test", s["gate_shape"], s["gate_shape_reason"])
	}
}

// (23) a red scoped gate fails the slice: category A, no verified tree, and
// never a full re-verify.
func TestRunVerifyFixLoop_SliceGateRedFailsSlice(t *testing.T) {
	repo := sliceGateLoopRepo(t)
	cmd, logPath := sliceGateSeqRecorder(t, 1)
	cfg := sliceGateChildCfg(repo, cmd)
	inv := &fakeInvoker{canned: agent.Result{OK: true}, onInvoke: func(i int, _ agent.Invocation) {
		mustWrite(t, filepath.Join(repo, "a", "a.go"), fmt.Sprintf("package a\n\n// fix %d\n", i))
	}}

	res, tree, _ := sliceGateRunLoop(t, &cfg, inv)
	if res.OK || res.FailureCategory != "A" || tree != "" {
		t.Errorf("ok=%v category=%q tree=%q, want a category-A failure with no tree", res.OK, res.FailureCategory, tree)
	}
	got := sliceGatePkgs(readVerifyFormLog(t, logPath))
	if !reflect.DeepEqual(got, []string{"[a,b]", "[a,b]"}) {
		t.Errorf("verify invocations = %q, want two scoped [a,b] and no full form", got)
	}
}

// (24) off a child, and on a child FIX-UP, the gate keeps today's two-verify
// shape and emits no slice-gate evidence.
func TestRunVerifyFixLoop_SliceGateIgnoredOffChild(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config)
	}{
		{"standalone", func(c *config) { c.decomposedFromRunID = "" }},
		{"child fix-up", func(c *config) { c.fixup = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := sliceGateLoopRepo(t)
			cmd, logPath := sliceGateSeqRecorder(t, 0)
			cfg := sliceGateChildCfg(repo, cmd)
			tc.mutate(&cfg)
			res, _, _ := sliceGateRunLoop(t, &cfg, nil)
			got := sliceGatePkgs(readVerifyFormLog(t, logPath))
			if !reflect.DeepEqual(got, []string{"[a]", "[]"}) {
				t.Errorf("verify invocations = %q, want today's [a] then []", got)
			}
			s := sliceGateSummaryOf(t, res)
			if _, ok := s["gate_shape"]; ok {
				t.Errorf("verify_summary carries gate_shape off the slice gate: %v", s)
			}
			if ev := sliceGateEvents(res.Events, sliceGateEventKind); len(ev) != 0 {
				t.Errorf("verify_slice_gate events = %v, want none", ev)
			}
		})
	}
}

// (25) a child with no served config keeps today's shape, reason named.
func TestRunVerifyFixLoop_SliceGateNotOffered(t *testing.T) {
	repo := sliceGateLoopRepo(t)
	cmd, logPath := sliceGateSeqRecorder(t, 0)
	cfg := sliceGateChildCfg(repo, cmd)
	cfg.sliceGate = nil
	res, _, _ := sliceGateRunLoop(t, &cfg, nil)
	if got := sliceGatePkgs(readVerifyFormLog(t, logPath)); !reflect.DeepEqual(got, []string{"[a]", "[]"}) {
		t.Errorf("verify invocations = %q, want today's [a] then []", got)
	}
	if s := sliceGateSummaryOf(t, res); s["gate_shape_reason"] != sliceGateReasonNotOffered || s["gate_shape"] != sliceGateShapeFull {
		t.Errorf("gate shape = %v / %v, want full / slice_gate_not_offered", s["gate_shape"], s["gate_shape_reason"])
	}
}

// (29) R7a + (43) T3: a hook-WIDENED deciding verify (unclassified_path, so
// the load ran) keeps today's outside-only absorb with the bound nonce, and
// the absorb re-run keeps its absorbing.dirs scope with no slice-gate event.
func TestRunVerifyFixLoop_SliceGateWidenedFullKeepsAbsorb(t *testing.T) {
	f := newAbsorbFixture(t, 0,
		absorbOutsideStep(goodAbsorbAttest()),
		absorbStep{pkgs: "modb/b", out: absorbPassOut, rc: 0},
	)
	if err := os.MkdirAll(filepath.Join(f.repo, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(f.repo, "tools", "gen.sh"), "#!/bin/sh\n")
	f.cfg.scopeFiles = append(f.cfg.scopeFiles, upload.ScopeFile{Path: "tools/gen.sh", Operation: "create"})
	f.cfg.decomposedFromRunID = "parent-run"
	f.cfg.sliceGate = sliceGateOffered
	loads := sliceGateCountLoads(t)

	res, tree, log := f.run(t, &fakeInvoker{canned: agent.Result{OK: true}})
	calls := f.assertCalls(t)
	if !res.OK || tree == "" {
		t.Fatalf("ok=%v tree=%q, want the absorbed pass\n%s", res.OK, tree, log)
	}
	if len(sliceGateEvents(res.Events, "verify_flake_absorbed")) != 1 {
		t.Errorf("want one verify_flake_absorbed\n%s", log)
	}
	if len(calls) == 2 && (calls[0].nonce == "" || calls[1].pkgs != "modb/b") {
		t.Errorf("calls = %+v, want a nonce-bound full verify then the modb/b absorb re-run", calls)
	}
	ev := sliceGateEvents(res.Events, sliceGateEventKind)
	if len(ev) != 1 || ev[0]["reason"] != "unclassified_path:tools/gen.sh" {
		t.Errorf("verify_slice_gate events = %v, want exactly one widened decision (none for the absorb re-run)", ev)
	}
	if *loads != 1 {
		t.Errorf("reach loads = %d, want 1 (the absorb re-run carries no hook)", *loads)
	}
}

// (30) R7b: a reverse-dependency failure on a SCOPED deciding verify is
// `outside` the declared scope: no absorb, no full form. MUTATING fix ends
// category A; a NO-OP fix ends category C verify_failure_outside_change.
func TestRunVerifyFixLoop_SliceGateReverseDepFailureIsOutside(t *testing.T) {
	const bFail = "--- FAIL: TestB (0.00s)\nFAIL\nFAIL\texample.com/sg/b\t0.01s\n"
	for _, tc := range []struct {
		name     string
		mutating bool
		wantCat  string
	}{
		{"mutating", true, "A"},
		{"no-op", false, "C"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := sliceGateLoopRepo(t)
			dir := t.TempDir()
			logPath := filepath.Join(dir, "calls.log")
			script := filepath.Join(dir, "v.sh")
			mustWrite(t, filepath.Join(dir, "out"), bFail)
			mustWrite(t, script, fmt.Sprintf("#!/bin/sh\nprintf 'pkgs=[%%s]\\n' \"$%s\" >> %q\ncat %q\nexit 1\n", verifyPackagesEnvVar, logPath, filepath.Join(dir, "out")))
			cfg := sliceGateChildCfg(repo, "sh "+script)
			inv := &fakeInvoker{canned: agent.Result{OK: true}}
			if tc.mutating {
				inv.onInvoke = func(i int, _ agent.Invocation) {
					mustWrite(t, filepath.Join(repo, "a", "a.go"), fmt.Sprintf("package a\n\n// fix %d\n", i))
				}
			}
			res, _, log := sliceGateRunLoop(t, &cfg, inv)
			if res.OK || res.FailureCategory != tc.wantCat {
				t.Fatalf("ok=%v category=%q, want %s\n%s", res.OK, res.FailureCategory, tc.wantCat, log)
			}
			if !tc.mutating && !strings.HasPrefix(res.FailureReason, verifyFailureOutsideChangeLead) {
				t.Errorf("no-op reason = %q, want the verify_failure_outside_change lead", res.FailureReason)
			}
			for _, p := range sliceGatePkgs(readVerifyFormLog(t, logPath)) {
				if p != "[a,b]" {
					t.Errorf("a verify ran %s, want only the scoped [a,b] form", p)
				}
			}
			if len(sliceGateEvents(res.Events, "verify_flake_absorbed")) != 0 {
				t.Errorf("a scoped deciding verify must never absorb")
			}
		})
	}
}

// sliceGateStep is one scripted verify invocation: its output and exit code.
type sliceGateStep struct {
	out  string
	code int
}

// sliceGateStepRecorder is sliceGateSeqRecorder with per-invocation output:
// invocation n prints steps[n-1].out and exits steps[n-1].code (the last step
// repeats). Each output is a file, so no shell quoting touches it.
func sliceGateStepRecorder(t *testing.T, steps ...sliceGateStep) (cmd, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	var arms strings.Builder
	for i, st := range steps {
		outPath := filepath.Join(dir, fmt.Sprintf("out%d", i+1))
		mustWrite(t, outPath, st.out)
		arm := fmt.Sprintf("%d)", i+1)
		if i == len(steps)-1 {
			arm = "*)"
		}
		fmt.Fprintf(&arms, "%s cat %q; exit %d;; ", arm, outPath, st.code)
	}
	script := filepath.Join(dir, "verify.sh")
	mustWrite(t, script, fmt.Sprintf(`#!/bin/sh
printf 'pkgs=[%%s] nonce=[%%s]\n' "$%s" "$%s" >> %q
n=$(wc -l < %q | tr -d ' ')
case "$n" in %s esac
`, verifyPackagesEnvVar, verifyAbsorbNonceEnvVar, logPath, logPath, arms.String()))
	return "sh " + script, logPath
}

// (31) S4: in-place re-runs on an identical committed tree reuse the reach; the
// #3316 auto-format absorb's reformatted tree reuses it once through the
// loop's carry() call; a fix iteration (new tree) reloads.
func TestRunVerifyFixLoop_SliceGateReachOncePerTree(t *testing.T) {
	ab := []string{"[a,b]", "[a,b]"}
	for _, tc := range []struct {
		name      string
		steps     []sliceGateStep
		fix       bool
		setup     func(t *testing.T, repo string)
		wantPkgs  []string
		wantLoads int
		wantEvent string // a trace event kind the re-run path must emit once
	}{
		{name: "temp-fail", steps: []sliceGateStep{{"", 75}, {"", 0}}, wantPkgs: ab, wantLoads: 1, wantEvent: "verify_infra_tempfail_retry"},
		{name: "lock-contention", steps: []sliceGateStep{{"", 73}, {"", 0}}, wantPkgs: ab, wantLoads: 1},
		{name: "infra flake", steps: []sliceGateStep{{golangciLintLockSignature + "\n", 3}, {"", 0}}, wantPkgs: ab, wantLoads: 1, wantEvent: "verify_infra_flake_retry"},
		{
			// The formatter rewrites the in-scope a/a.go, so the re-verify
			// gates a DIFFERENT committed tree; carry() reuses the reach.
			name: "auto-format", steps: []sliceGateStep{{gofmtOnlyLintOutput + "\n", 3}, {"", 0}},
			setup: func(t *testing.T, _ string) {
				stubFormatter(t, `printf 'package a\n\n// formatted\n' > "$2"; exit 0`)
			},
			wantPkgs: ab, wantLoads: 1, wantEvent: "verify_autoformatted",
		},
		{
			// The first gate's clone cannot be materialized, so neither the
			// load nor the verify runs; the one re-run loads once.
			name: "skipped gate", steps: []sliceGateStep{{"", 0}},
			setup: func(t *testing.T, _ string) {
				calls := 0
				swapMaterializer(t, func(ctx context.Context, repoDir, headSHA, parent string) (string, error) {
					calls++
					if calls == 1 {
						return "", errors.New("git clone --no-hardlinks: fatal: No space left on device")
					}
					return materializeGateCheckout(ctx, repoDir, headSHA, parent)
				})
			},
			wantPkgs: []string{"[a,b]"}, wantLoads: 1, wantEvent: "verify_gate_skipped_retry",
		},
		{name: "fix iteration", steps: []sliceGateStep{{"", 1}, {"", 0}}, fix: true, wantPkgs: ab, wantLoads: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := sliceGateLoopRepo(t)
			if tc.setup != nil {
				tc.setup(t, repo)
			}
			cmd, logPath := sliceGateStepRecorder(t, tc.steps...)
			cfg := sliceGateChildCfg(repo, cmd)
			loads := sliceGateCountLoads(t)
			inv := &fakeInvoker{canned: agent.Result{OK: true}}
			if tc.fix {
				inv.onInvoke = func(i int, _ agent.Invocation) {
					mustWrite(t, filepath.Join(repo, "a", "a.go"), fmt.Sprintf("package a\n\n// fix %d\n", i))
				}
			}
			res, _, log := sliceGateRunLoop(t, &cfg, inv)
			if !res.OK {
				t.Fatalf("want a pass: %s\n%s", res.FailureReason, log)
			}
			if got := sliceGatePkgs(readVerifyFormLog(t, logPath)); !reflect.DeepEqual(got, tc.wantPkgs) {
				t.Errorf("verify invocations = %q, want %q\n%s", got, tc.wantPkgs, log)
			}
			if *loads != tc.wantLoads {
				t.Errorf("reach loads = %d, want %d\n%s", *loads, tc.wantLoads, log)
			}
			if tc.wantEvent != "" {
				if n := countEvents(res.Events, tc.wantEvent); n != 1 {
					t.Errorf("%s events = %d, want 1 (the re-run path under test)\n%s", tc.wantEvent, n, log)
				}
			}
			if !tc.fix && inv.callIdx != 0 {
				t.Errorf("fix agent invocations = %d, want 0 for an in-place re-run", inv.callIdx)
			}
		})
	}
}

// (33) a PASSED #4036 flake re-run on a slice-scoped verify: the reverse-dep
// failure is outside the change, the fix agent changes nothing, and the
// identical tree passes. The loop's verify_summary detail carries BOTH the
// flake re-run text (main.go's verifyFlakeRerunDetail assignment) and the
// scoped-shape sentence sliceGateSummary appends after it.
func TestRunVerifyFixLoop_SliceGateDetailAppendsToExisting(t *testing.T) {
	const bFail = "--- FAIL: TestB (0.00s)\nFAIL\nFAIL\texample.com/sg/b\t0.01s\n"
	repo := sliceGateLoopRepo(t)
	cmd, logPath := sliceGateStepRecorder(t, sliceGateStep{bFail, 1}, sliceGateStep{"", 0})
	cfg := sliceGateChildCfg(repo, cmd)
	loads := sliceGateCountLoads(t)

	res, tree, log := sliceGateRunLoop(t, &cfg, &fakeInvoker{canned: agent.Result{OK: true}})
	if !res.OK || tree == "" {
		t.Fatalf("ok=%v tree=%q, want the passed flake re-run\n%s", res.OK, tree, log)
	}
	if got := sliceGatePkgs(readVerifyFormLog(t, logPath)); !reflect.DeepEqual(got, []string{"[a,b]", "[a,b]"}) {
		t.Errorf("verify invocations = %q, want two scoped [a,b]", got)
	}
	if *loads != 1 {
		t.Errorf("reach loads = %d, want 1 (the no-op fix left the tree identical)", *loads)
	}
	s := sliceGateSummaryOf(t, res)
	d, _ := s["detail"].(string)
	if !strings.HasPrefix(d, verifyFlakeRerunLead) || !strings.Contains(d, "TestB") {
		t.Errorf("detail = %q, want it to open with the flake re-run text naming TestB", d)
	}
	if !strings.HasSuffix(d, "; "+sliceGateScopedDetail) {
		t.Errorf("detail = %q, want the scoped-shape sentence appended after the flake text", d)
	}
	if s["gate_shape"] != sliceGateShapeScoped {
		t.Errorf("gate_shape = %v, want slice_scoped", s["gate_shape"])
	}
}

// (42) T2: an eligible child whose gate isolation selected the container path
// runs ONE full verify with the nonce bound, and never loads the reach.
func TestRunVerifyFixLoop_SliceGateContainerSelectionRunsFull(t *testing.T) {
	repo := sliceGateLoopRepo(t)
	cmd, logPath := sliceGateSeqRecorder(t, 0)
	cfg := sliceGateChildCfg(repo, cmd)
	sliceGateWithSelection(t, gateiso.Selection{Path: gateiso.PathContainer, Profile: gateiso.ProfileLocal})
	loads := sliceGateCountLoads(t)

	res, _, _ := sliceGateRunLoop(t, &cfg, nil)
	lines := readVerifyFormLog(t, logPath)
	if got := sliceGatePkgs(lines); !reflect.DeepEqual(got, []string{"[]"}) {
		t.Fatalf("verify invocations = %q, want exactly one full []", got)
	}
	if strings.Contains(lines[0], "nonce=[]") {
		t.Errorf("the widened deciding verify must carry the absorb nonce: %q", lines[0])
	}
	s := sliceGateSummaryOf(t, res)
	if s["gate_shape"] != sliceGateShapeFull || s["gate_shape_reason"] != "gate_isolation_not_host:container" {
		t.Errorf("gate shape = %v / %v", s["gate_shape"], s["gate_shape_reason"])
	}
	if *loads != 0 {
		t.Errorf("reach loads = %d, want 0 off a host path", *loads)
	}
}

// (26) S6 + Q2: the single-shot gate.
func TestRunVerifyGateCommitted_SliceGate(t *testing.T) {
	t.Run("scoped", func(t *testing.T) {
		repo := sliceGateLoopRepo(t)
		cmd, logPath := sliceGateSeqRecorder(t, 0)
		cfg := sliceGateChildCfg(repo, cmd)
		cfg.verifyMaxIterations = 0
		events, tree, err := runVerifyGateCommitted(context.Background(), cfg, &strings.Builder{})
		if err != nil || tree == "" {
			t.Fatalf("err=%v tree=%q", err, tree)
		}
		if got := sliceGatePkgs(readVerifyFormLog(t, logPath)); !reflect.DeepEqual(got, []string{"[a,b]"}) {
			t.Errorf("verify invocations = %q, want one [a,b]", got)
		}
		if ev := sliceGateEvents(events, sliceGateEventKind); len(ev) != 1 || ev[0]["shape"] != sliceGateShapeScoped {
			t.Errorf("verify_slice_gate events = %v, want one slice_scoped", ev)
		}
		if s := sliceGateEvents(events, "verify_summary"); len(s) != 0 {
			t.Errorf("the single-shot gate emitted a verify_summary: %v", s)
		}
	})
	t.Run("red demotes B", func(t *testing.T) {
		repo := sliceGateLoopRepo(t)
		cmd, _ := sliceGateSeqRecorder(t, 1)
		cfg := sliceGateChildCfg(repo, cmd)
		cfg.verifyMaxIterations = 0
		_, _, err := runVerifyGateCommitted(context.Background(), cfg, &strings.Builder{})
		if !errors.Is(err, gitops.ErrCommittedTestsFailed) || committedGateFailureCategory(err) != "B" {
			t.Errorf("err = %v (category %q), want ErrCommittedTestsFailed / B", err, committedGateFailureCategory(err))
		}
	})
	t.Run("temp-fail re-run reuses the reach", func(t *testing.T) {
		repo := sliceGateLoopRepo(t)
		cmd, logPath := sliceGateSeqRecorder(t, 75, 0)
		cfg := sliceGateChildCfg(repo, cmd)
		cfg.verifyMaxIterations = 0
		loads := sliceGateCountLoads(t)
		if _, _, err := runVerifyGateCommitted(context.Background(), cfg, &strings.Builder{}); err != nil {
			t.Fatalf("err = %v", err)
		}
		if got := sliceGatePkgs(readVerifyFormLog(t, logPath)); !reflect.DeepEqual(got, []string{"[a,b]", "[a,b]"}) {
			t.Errorf("verify invocations = %q, want two identical [a,b]", got)
		}
		if *loads != 1 {
			t.Errorf("reach loads = %d, want exactly 1", *loads)
		}
	})
	t.Run("child fix-up stays full", func(t *testing.T) {
		repo := sliceGateLoopRepo(t)
		cmd, logPath := sliceGateSeqRecorder(t, 0)
		cfg := sliceGateChildCfg(repo, cmd)
		cfg.verifyMaxIterations = 0
		cfg.fixup = true
		events, _, err := runVerifyGateCommitted(context.Background(), cfg, &strings.Builder{})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got := sliceGatePkgs(readVerifyFormLog(t, logPath)); !reflect.DeepEqual(got, []string{"[]"}) {
			t.Errorf("verify invocations = %q, want one full []", got)
		}
		if ev := sliceGateEvents(events, sliceGateEventKind); len(ev) != 0 {
			t.Errorf("a child fix-up emitted verify_slice_gate: %v", ev)
		}
	})
}

// (27) the shared golden cases, decoded into the runner's wire type, through
// the runner's matcher.
func TestSliceGateGoldenCases(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "slice_gate_prompt.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Config      upload.SliceGateConfig `json:"config"`
		Unavailable upload.SliceGateConfig `json:"unavailable"`
		Cases       []struct {
			Path string `json:"path"`
			Full bool   `json:"full"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Cases) == 0 {
		t.Fatal("golden carries no cases")
	}
	for _, c := range golden.Cases {
		d := planSliceGate(true, golden.Config.UnavailableReason, golden.Config.FullGatePaths, []string{c.Path})
		if gotFull := !d.eligible(); gotFull != c.Full {
			t.Errorf("%s: full = %v (%s), want %v", c.Path, gotFull, d.Reason, c.Full)
		}
	}
	if d := planSliceGate(true, golden.Unavailable.UnavailableReason, nil, []string{"m1/a/a.go"}); d.Reason != "surface_incomplete" {
		t.Errorf("unavailable block decided %+v, want full/surface_incomplete", d)
	}
}

// (32) the loader plus the mapping over the slice-3 base workspace.
func TestSliceGateReachHook_ComposesLoaderAndMapping(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	sliceReachQuietLog(t)
	root := sliceReachWriteFixture(t, sliceReachBaseFiles())
	sg := newSliceGateReach([]string{"m1/a/a.go"}, nil)
	got := sg.resolve(context.Background(), root, "tree-1")
	want := []string{"m1/a", "m1/b", "m1/d", "m1/e", "m2/f"}
	if !reflect.DeepEqual(got, want) {
		d, _ := sg.take()
		t.Fatalf("reach = %v (%+v), want %v", got, d, want)
	}

	orig := sliceGateLoadFn
	sliceGateLoadFn = func(context.Context, string) (map[string][]string, map[string]bool, string) {
		return nil, nil, "reach_load_failed:no_packages"
	}
	t.Cleanup(func() { sliceGateLoadFn = orig })
	sg = newSliceGateReach([]string{"m1/a/a.go"}, nil)
	if got := sg.resolve(context.Background(), root, "tree-2"); got != nil {
		t.Errorf("a refused load returned %v, want nil (full)", got)
	}
	if d, ok := sg.take(); !ok || d.Shape != sliceGateShapeFull || d.Reason != "reach_load_failed:no_packages" {
		t.Errorf("decision = %+v (%v), want full/reach_load_failed:no_packages", d, ok)
	}
}

// sliceGateStubGraph scopes m/a.go to [a, b] out of [a, b, c].
func sliceGateStubLoads(t *testing.T) *int {
	t.Helper()
	n := new(int)
	orig := sliceGateLoadFn
	sliceGateLoadFn = func(context.Context, string) (map[string][]string, map[string]bool, string) {
		*n++
		return map[string][]string{"a": nil, "b": {"a"}, "c": nil}, map[string]bool{"a": true, "b": true, "c": true}, ""
	}
	t.Cleanup(func() { sliceGateLoadFn = orig })
	return n
}

// (41) T2: only a host gate-isolation path under a non-hosted profile loads.
func TestSliceGateReachHook_IsolationNotHostWidens(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sel        gateiso.Selection
		wantReason string
	}{
		{"container", gateiso.Selection{Path: gateiso.PathContainer, Profile: gateiso.ProfileLocal}, "gate_isolation_not_host:container"},
		{"refused", gateiso.Selection{Path: gateiso.PathRefused}, "gate_isolation_not_host:refused"},
		{"hosted clone", gateiso.Selection{Path: gateiso.PathClone, Profile: gateiso.ProfileHosted}, "gate_isolation_not_host:hosted"},
		{"empty path", gateiso.Selection{Profile: gateiso.ProfileLocal}, "gate_isolation_not_host:unknown"},
		{"clone local", gateiso.Selection{Path: gateiso.PathClone, Profile: gateiso.ProfileLocal}, ""},
		{"clone-sandbox self-hosted", gateiso.Selection{Path: gateiso.PathCloneSandbox, Profile: gateiso.ProfileSelfHosted}, ""},
		{"nil-state host exec", hostExecSelection(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sliceGateWithSelection(t, tc.sel)
			loads := sliceGateStubLoads(t)
			sg := newSliceGateReach([]string{"a/a.go"}, nil)
			got := sg.resolve(context.Background(), t.TempDir(), "tree")
			d, _ := sg.take()
			if tc.wantReason != "" {
				if got != nil || d.Reason != tc.wantReason || *loads != 0 {
					t.Errorf("got %v reason %q loads %d, want nil / %q / 0", got, d.Reason, *loads, tc.wantReason)
				}
				return
			}
			if !reflect.DeepEqual(got, []string{"a", "b"}) || *loads != 1 {
				t.Errorf("got %v loads %d, want [a b] / 1", got, *loads)
			}
		})
	}
}

// The tree-keyed cache: a repeat on the same tree reuses; a new tree reloads;
// carry() reuses once for the next tree; a changed scope or an empty tree
// never reuses.
func TestSliceGateReachHook_TreeCacheAndCarry(t *testing.T) {
	loads := sliceGateStubLoads(t)
	ctx := context.Background()
	sg := newSliceGateReach([]string{"a/a.go"}, nil)
	sg.resolve(ctx, "", "t1")
	sg.resolve(ctx, "", "t1")
	if *loads != 1 {
		t.Fatalf("same tree loads = %d, want 1", *loads)
	}
	sg.carry()
	sg.resolve(ctx, "", "t2")
	if *loads != 1 {
		t.Errorf("carried tree loads = %d, want 1", *loads)
	}
	sg.resolve(ctx, "", "t3")
	if *loads != 2 {
		t.Errorf("new tree loads = %d, want 2 (carry is once)", *loads)
	}
	sg.setScope([]string{"c/c.go"})
	if got := sg.resolve(ctx, "", "t3"); !reflect.DeepEqual(got, []string{"c"}) || *loads != 3 {
		t.Errorf("changed scope got %v loads %d, want [c] / 3", got, *loads)
	}
	sg.resolve(ctx, "", "")
	sg.resolve(ctx, "", "")
	if *loads != 5 {
		t.Errorf("empty tree loads = %d, want 5 (never cached)", *loads)
	}
}

// (33) S9: the scoped-shape sentence is set on an empty detail and APPENDED to
// an existing passed detail; it never lands on a full or failed summary.
func TestSliceGateSummary_DetailAppendsToExisting(t *testing.T) {
	scoped := sliceGateDecision{Shape: sliceGateShapeScoped, Packages: []string{"a", "b"}}
	s := map[string]any{"outcome": "passed", "detail": "flake re-run passed"}
	sliceGateSummary(s, scoped)
	if d := s["detail"].(string); !strings.Contains(d, "flake re-run passed") || !strings.Contains(d, sliceGateScopedDetail) {
		t.Errorf("detail = %q, want both the existing text and the scoped sentence", d)
	}
	s = map[string]any{"outcome": "passed"}
	sliceGateSummary(s, sliceGateFull("global_surface:x"))
	if _, ok := s["detail"]; ok || s["gate_shape_reason"] != "global_surface:x" {
		t.Errorf("a full summary = %v, want no detail and the reason", s)
	}
	s = map[string]any{"outcome": "failed"}
	sliceGateSummary(s, scoped)
	if _, ok := s["detail"]; ok {
		t.Errorf("a failed scoped summary gained a detail: %v", s)
	}
}

// (20) S2: the served slice_gate JSON reaches the gate through the real
// decode, fetchPromptToFile's return and the ONE call-site assignment.
func TestRun_SliceGateHandoff_ServedJSONReachesGate(t *testing.T) {
	repo := sliceGateLoopRepo(t)
	cmd, logPath := sliceGateSeqRecorder(t, 0)
	// Revert the fixture's pre-applied edit: the agent makes it in run().
	if out, err := exec.Command("git", "-C", repo, "checkout", "--", "a/a.go").CombinedOutput(); err != nil {
		t.Fatalf("reset fixture: %v\n%s", err, out)
	}
	wire, err := json.Marshal(map[string]any{
		"stage_id": verifyFixStageID, "stage_type": "implement", "prompt": "implement",
		"verify_command": cmd, "verify_max_iterations": 1, "verify_timeout_seconds": 60,
		"decomposed_from_run_id": "parent-run",
		"scope_files":            []map[string]string{{"path": "a/a.go", "operation": "modify"}},
		"slice_gate":             json.RawMessage(sliceGateGoldenConfig(t)),
	})
	if err != nil {
		t.Fatal(err)
	}
	var fetched upload.FetchedPrompt
	if err := json.NewDecoder(strings.NewReader(string(wire))).Decode(&fetched); err != nil {
		t.Fatal(err)
	}
	if fetched.SliceGate == nil {
		t.Fatalf("decode dropped slice_gate: %s", wire)
	}

	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	inv := &fakeInvoker{canned: agent.Result{OK: true}, onInvoke: func(_ int, in agent.Invocation) {
		// A child runs in its lineage worktree, not the operator checkout.
		mustWrite(t, filepath.Join(in.WorkingDir, "a", "a.go"), "package a\n\n// agent change\n")
	}}
	withFakeInvoker(t, inv)
	fu := newFakeUploader(t)
	fu.promptResp = &fetched
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
	var stderr strings.Builder
	run(verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")), &stderr)

	got := sliceGatePkgs(readVerifyFormLog(t, logPath))
	if len(got) == 0 || got[0] != "[a,b]" {
		t.Fatalf("verify invocations = %q, want the hooked [a,b] first\n%s", got, stderr.String())
	}
	if n := strings.Count(stderr.String(), `"event":"verify_slice_gate_resolved"`); n != 1 {
		t.Errorf("verify_slice_gate_resolved lines = %d, want exactly 1 (the strict re-verify carries no hook)\n%s", n, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"shape":"slice_scoped"`) {
		t.Errorf("the gate decision is not slice_scoped:\n%s", stderr.String())
	}
}

func sliceGateGoldenConfig(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "slice_gate_prompt.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Config json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	return g.Config
}
