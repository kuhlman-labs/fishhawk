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
	"regexp"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// #4229 — outside-only verify flake absorption at exhaustion.

const goldenAttestNonce = "0123456789abcdef0123456789abcdef"

func readSharedAttestGolden(t *testing.T) string {
	t.Helper()
	root := requireWorkspaceRoot(t, pkgSrcDir)
	data, err := os.ReadFile(filepath.Join(root, "testdata", "wire", "verify_absorb_attest.txt")) //nolint:gosec // fixed workspace-relative path
	if err != nil {
		t.Fatalf("reading the shared attest golden: %v", err)
	}
	return string(data)
}

func TestParseAbsorbAttest_SharedGolden(t *testing.T) {
	a, ok := parseAbsorbAttest(readSharedAttestGolden(t), goldenAttestNonce)
	if !ok {
		t.Fatal("the shared golden attest line did not parse")
	}
	want := absorbAttest{
		patchCoverage: "pass",
		failedModules: []absorbModuleClass{{module: "moda", class: "tests"}},
		changedDirs:   []string{"moda/a"},
	}
	if !reflect.DeepEqual(a, want) {
		t.Errorf("parsed = %+v, want %+v", a, want)
	}
}

// TestVerifyAbsorbNonceEnvNameMatchesScriptsTest pins the env var name and the
// attest prefix against the real scripts/test.
func TestVerifyAbsorbNonceEnvNameMatchesScriptsTest(t *testing.T) {
	root := requireWorkspaceRoot(t, pkgSrcDir)
	data, err := os.ReadFile(filepath.Join(root, "scripts", "test")) //nolint:gosec // fixed workspace-relative path
	if err != nil {
		t.Fatalf("reading scripts/test: %v", err)
	}
	src := string(data)
	for _, want := range []string{
		`VERIFY_ABSORB_NONCE="${` + verifyAbsorbNonceEnvVar + `:-}"`,
		"unset " + verifyAbsorbNonceEnvVar,
		"printf '" + verifyAbsorbAttestPrefix + " %s patch_coverage=%s failed_modules=%s changed_dirs=%s\\n'",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("scripts/test lacks %q — the runner's nonce/attest contract drifted from the script", want)
		}
	}
}

func TestNewVerifyAbsorbNonce(t *testing.T) {
	a, b := newVerifyAbsorbNonce(), newVerifyAbsorbNonce()
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(a) {
		t.Errorf("nonce %q is not 32 lowercase hex characters", a)
	}
	if a == b {
		t.Errorf("two nonces are equal: %q", a)
	}
}

func TestParseAbsorbAttest(t *testing.T) {
	const n = goldenAttestNonce
	line := func(rest string) string { return verifyAbsorbAttestPrefix + " " + n + " " + rest }
	good := line("patch_coverage=pass failed_modules=moda:tests,modb:tests changed_dirs=moda/a,moda/b")
	cases := []struct {
		name  string
		out   string
		nonce string
		ok    bool
		check func(t *testing.T, a absorbAttest)
	}{
		{name: "good amid noise", out: "ok\n" + good + "\nscripts/test: FAILED\n", nonce: n, ok: true, check: func(t *testing.T, a absorbAttest) {
			if len(a.failedModules) != 2 || len(a.changedDirs) != 2 || a.patchCoverage != "pass" {
				t.Errorf("parsed %+v", a)
			}
		}},
		{name: "CRLF", out: good + "\r\n", nonce: n, ok: true},
		{name: "no line", out: "FAIL\texample.com/x\n", nonce: n},
		{name: "empty nonce never matches", out: verifyAbsorbAttestPrefix + "  patch_coverage=pass failed_modules=a:tests changed_dirs=a", nonce: ""},
		{name: "wrong nonce only", out: good, nonce: "ffffffffffffffffffffffffffffffff"},
		{name: "other nonce ignored beside the right one", out: strings.Replace(good, n, "ffffffffffffffffffffffffffffffff", 1) + "\n" + good, nonce: n, ok: true},
		{name: "duplicate", out: good + "\n" + good, nonce: n},
		{name: "missing field", out: line("patch_coverage=pass failed_modules=moda:tests"), nonce: n},
		{name: "fields out of order", out: line("failed_modules=moda:tests patch_coverage=pass changed_dirs=moda/a"), nonce: n},
		{name: "unknown verdict", out: line("patch_coverage=maybe failed_modules=moda:tests changed_dirs=moda/a"), nonce: n},
		{name: "unknown class", out: line("patch_coverage=pass failed_modules=moda:flaky changed_dirs=moda/a"), nonce: n},
		{name: "empty module", out: line("patch_coverage=pass failed_modules=:tests changed_dirs=moda/a"), nonce: n},
		{name: "empty dir", out: line("patch_coverage=pass failed_modules=moda:tests changed_dirs=moda/a,"), nonce: n},
		{name: "undecodable modules and dirs", out: line("patch_coverage=unavailable failed_modules=undecodable changed_dirs=undecodable"), nonce: n, ok: true, check: func(t *testing.T, a absorbAttest) {
			if !a.modulesUndecodable || !a.dirsUndecodable || a.failedModules != nil || a.changedDirs != nil || a.patchCoverage != "unavailable" {
				t.Errorf("parsed %+v, want both undecodable", a)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, ok := parseAbsorbAttest(tc.out, tc.nonce)
			if ok != tc.ok {
				t.Fatalf("ok = %t, want %t (%+v)", ok, tc.ok, a)
			}
			if tc.check != nil {
				tc.check(t, a)
			}
		})
	}
}

// absorbRepo builds a git repo with a two-module go.work workspace (moda at
// example.com/moda, modb at example.com/modb) committed as the base.
func absorbRepo(t *testing.T) (string, func(args ...string)) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, runGit := compileGateRepo(t)
	for _, d := range []string{"moda/a", "moda/c", "modb/b", "modb/moda/a"} {
		if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(repo, "go.work"), "go 1.25\n\nuse (\n\t./moda\n\t./modb\n)\n")
	mustWrite(t, filepath.Join(repo, "moda", "go.mod"), "module example.com/moda\n\ngo 1.25\n")
	mustWrite(t, filepath.Join(repo, "modb", "go.mod"), "module example.com/modb\n\ngo 1.25\n")
	mustWrite(t, filepath.Join(repo, "moda", "a", "a.go"), "package a\n")
	mustWrite(t, filepath.Join(repo, "moda", "c", "c.go"), "package c\n")
	mustWrite(t, filepath.Join(repo, "modb", "b", "b.go"), "package b\n")
	mustWrite(t, filepath.Join(repo, "modb", "moda", "a", "a.go"), "package a\n")
	runGit("add", "-A")
	runGit("commit", "-m", "base", "--no-verify")
	return repo, runGit
}

func TestWorkspaceModuleMap(t *testing.T) {
	repo, runGit := absorbRepo(t)
	sha := gitHead(t, repo)
	ctx := context.Background()

	mods, err := workspaceModuleMap(ctx, repo, sha)
	if err != nil {
		t.Fatalf("workspaceModuleMap: %v", err)
	}
	want := []wsModule{{path: "example.com/moda", dir: "moda"}, {path: "example.com/modb", dir: "modb"}}
	if !reflect.DeepEqual(mods, want) {
		t.Fatalf("modules = %+v, want %+v", mods, want)
	}
	for _, tc := range []struct{ imp, dir, mod string }{
		{"example.com/moda", "moda", "moda"},
		{"example.com/moda/a", "moda/a", "moda"},
		{"example.com/modb/moda/a", "modb/moda/a", "modb"},
	} {
		dir, mod, ok := mapImportPath(mods, tc.imp)
		if !ok || dir != tc.dir || mod.dir != tc.mod {
			t.Errorf("mapImportPath(%q) = %q,%q,%t want %q,%q", tc.imp, dir, mod.dir, ok, tc.dir, tc.mod)
		}
	}
	if _, _, ok := mapImportPath(mods, "example.com/modabc/x"); ok {
		t.Error("a module-path prefix without a / boundary must not map")
	}
	if _, _, ok := mapImportPath(mods, "other.org/x"); ok {
		t.Error("an import path outside every module must not map")
	}

	// The map is read from the COMMIT: a working-tree go.work edit is invisible.
	mustWrite(t, filepath.Join(repo, "go.work"), "go 1.25\n\nuse ./moda\n")
	if again, err := workspaceModuleMap(ctx, repo, sha); err != nil || len(again) != 2 {
		t.Errorf("working-tree edit leaked into the commit read: %+v %v", again, err)
	}

	// Unreadable: a used module whose go.mod is absent from the commit.
	mustWrite(t, filepath.Join(repo, "go.work"), "go 1.25\n\nuse (\n\t./moda\n\t./missing\n)\n")
	runGit("add", "go.work")
	runGit("commit", "-m", "broken", "--no-verify")
	if _, err := workspaceModuleMap(ctx, repo, gitHead(t, repo)); err == nil {
		t.Error("a go.work naming a module with no go.mod must error")
	}
	if _, err := workspaceModuleMap(ctx, repo, "0000000000000000000000000000000000000000"); err == nil {
		t.Error("an unknown commit must error")
	}

	// Root go.mod only.
	root, rootGit := compileGateRepo(t)
	mustWrite(t, filepath.Join(root, "go.mod"), "module example.com/m\n\ngo 1.25\n")
	rootGit("add", "go.mod")
	rootGit("commit", "-m", "base", "--no-verify")
	rmods, err := workspaceModuleMap(ctx, root, gitHead(t, root))
	if err != nil || !reflect.DeepEqual(rmods, []wsModule{{path: "example.com/m", dir: "."}}) {
		t.Fatalf("root go.mod = %+v %v", rmods, err)
	}
	if dir, _, ok := mapImportPath(rmods, "example.com/m/pkg/b"); !ok || dir != "pkg/b" {
		t.Errorf("root-module map = %q %t, want pkg/b", dir, ok)
	}

	// Neither file.
	none, noneGit := compileGateRepo(t)
	mustWrite(t, filepath.Join(none, "x.txt"), "x\n")
	noneGit("add", "x.txt")
	noneGit("commit", "-m", "base", "--no-verify")
	if _, err := workspaceModuleMap(ctx, none, gitHead(t, none)); err == nil {
		t.Error("a commit with neither go.work nor go.mod must error")
	}
}

const (
	absorbOutsideFail = "--- FAIL: TestFlaky (0.01s)\nFAIL\nFAIL\texample.com/modb/b\t0.02s\n"
	absorbInsideFail  = "--- FAIL: TestInside (0.00s)\nFAIL\nFAIL\texample.com/moda/a\t0.01s\n"
	absorbPassOut     = "ok  \texample.com/moda/a\t0.01s\n"
)

func absorbAttestLine(nonce, pc, fm, cd string) string {
	return fmt.Sprintf("%s %s patch_coverage=%s failed_modules=%s changed_dirs=%s\n", verifyAbsorbAttestPrefix, nonce, pc, fm, cd)
}

var absorbModules = []wsModule{{path: "example.com/moda", dir: "moda"}, {path: "example.com/modb", dir: "modb"}}

func TestDecideFlakeAbsorb(t *testing.T) {
	const n = goldenAttestNonce
	goodOut := absorbOutsideFail + absorbAttestLine(n, "pass", "modb:tests", "moda/a")
	base := func() flakeAbsorbInput {
		return flakeAbsorbInput{
			output: goodOut, form: verifyFormFull, disp: gateExecuted, nonce: n,
			scopePkgs: []string{"moda/a"}, modules: absorbModules, tree: "t1",
		}
	}
	lossyOut := func() string {
		var b strings.Builder
		for i := 0; i <= verifyFailureParseMaxEntries; i++ {
			fmt.Fprintf(&b, "--- FAIL: TestX%02d (0.00s)\n", i)
		}
		b.WriteString("FAIL\texample.com/modb/b\t0.02s\n")
		b.WriteString(absorbAttestLine(n, "pass", "modb:tests", "moda/a"))
		return b.String()
	}()
	cases := []struct {
		name      string
		mut       func(in *flakeAbsorbInput)
		candidate bool
		reason    string
	}{
		{name: "admitted", candidate: true},
		{name: "inside is not a candidate", mut: func(in *flakeAbsorbInput) { in.output = absorbInsideFail }},
		{name: "unknown (no FAIL line) is not a candidate", mut: func(in *flakeAbsorbInput) { in.output = "lint failed\n" }},
		{name: "build failure is not a candidate", mut: func(in *flakeAbsorbInput) {
			in.output = "FAIL\texample.com/modb/b [build failed]\n"
		}},
		{name: "empty scope is not a candidate", mut: func(in *flakeAbsorbInput) { in.scopePkgs = nil }},
		{name: "already spent", mut: func(in *flakeAbsorbInput) { in.spent = true }, candidate: true, reason: absorbRefusedAlreadySpent},
		{name: "scoped form", mut: func(in *flakeAbsorbInput) { in.form = verifyFormScoped }, candidate: true, reason: absorbRefusedNotFullForm},
		{name: "disposition not executed", mut: func(in *flakeAbsorbInput) { in.disp = gateCheckoutRefused }, candidate: true, reason: absorbRefusedNotFullForm},
		{name: "parse lossy (11 tests, one package)", mut: func(in *flakeAbsorbInput) { in.output = lossyOut }, candidate: true, reason: absorbRefusedParseLossy},
		{name: "attest none", mut: func(in *flakeAbsorbInput) { in.output = absorbOutsideFail }, candidate: true, reason: absorbRefusedAttestMissing},
		{name: "attest wrong nonce", mut: func(in *flakeAbsorbInput) { in.nonce = "ffffffffffffffffffffffffffffffff" }, candidate: true, reason: absorbRefusedAttestMissing},
		{name: "attest no nonce injected", mut: func(in *flakeAbsorbInput) { in.nonce = "" }, candidate: true, reason: absorbRefusedAttestMissing},
		{name: "attest duplicate", mut: func(in *flakeAbsorbInput) { in.output += absorbAttestLine(n, "pass", "modb:tests", "moda/a") }, candidate: true, reason: absorbRefusedAttestMissing},
		{name: "patch coverage fail", mut: func(in *flakeAbsorbInput) {
			in.output = absorbOutsideFail + absorbAttestLine(n, "fail", "modb:tests", "moda/a")
		}, candidate: true, reason: absorbRefusedPatchCoverageFail},
		{name: "patch coverage unavailable", mut: func(in *flakeAbsorbInput) {
			in.output = absorbOutsideFail + absorbAttestLine(n, "unavailable", "modb:tests", "moda/a")
		}, candidate: true, reason: absorbRefusedPatchCoverageUnavailable},
		{name: "module build", mut: func(in *flakeAbsorbInput) {
			in.output = absorbOutsideFail + absorbAttestLine(n, "pass", "modb:build", "moda/a")
		}, candidate: true, reason: absorbRefusedModuleNotTestFailure},
		{name: "module other", mut: func(in *flakeAbsorbInput) {
			in.output = absorbOutsideFail + absorbAttestLine(n, "pass", "modb:other", "moda/a")
		}, candidate: true, reason: absorbRefusedModuleNotTestFailure},
		{name: "module unclassified", mut: func(in *flakeAbsorbInput) {
			in.output = absorbOutsideFail + absorbAttestLine(n, "pass", "modb:unclassified", "moda/a")
		}, candidate: true, reason: absorbRefusedModuleNotTestFailure},
		{name: "modules undecodable", mut: func(in *flakeAbsorbInput) {
			in.output = absorbOutsideFail + absorbAttestLine(n, "pass", "undecodable", "moda/a")
		}, candidate: true, reason: absorbRefusedModuleNotTestFailure},
		{name: "module literally named undecodable", mut: func(in *flakeAbsorbInput) {
			in.output = absorbOutsideFail + absorbAttestLine(n, "pass", "undecodable:tests", "moda/a")
		}, candidate: true, reason: absorbRefusedModuleNotTestFailure},
		{name: "module map unreadable", mut: func(in *flakeAbsorbInput) { in.modules, in.modErr = nil, errors.New("boom") }, candidate: true, reason: absorbRefusedPackageUnmapped},
		// A map error refuses even beside a usable partial map, so the error
		// check is the only control in the path.
		{name: "module map error with a partial map", mut: func(in *flakeAbsorbInput) { in.modErr = errors.New("boom") }, candidate: true, reason: absorbRefusedPackageUnmapped},
		{name: "package unmapped", mut: func(in *flakeAbsorbInput) {
			in.output = "--- FAIL: TestFlaky (0.01s)\nFAIL\tother.org/x\t0.02s\n" + absorbAttestLine(n, "pass", "modb:tests", "moda/a")
		}, candidate: true, reason: absorbRefusedPackageUnmapped},
		{name: "mapped dir equals a scope dir (module path differs from dir)", mut: func(in *flakeAbsorbInput) {
			in.modules = []wsModule{{path: "example.com/other", dir: "moda"}, {path: "example.com/modb", dir: "modb"}}
			in.output = "--- FAIL: TestFlaky (0.01s)\nFAIL\texample.com/other/a\t0.02s\n" + absorbAttestLine(n, "pass", "moda:tests", "moda/zz")
		}, candidate: true, reason: absorbRefusedChangedPackageFailed},
		{name: "changed dirs undecodable", mut: func(in *flakeAbsorbInput) {
			in.output = absorbOutsideFail + absorbAttestLine(n, "pass", "modb:tests", "undecodable")
		}, candidate: true, reason: absorbRefusedChangedDirsUndecodable},
		{name: "failing package in the diff but not the plan scope", mut: func(in *flakeAbsorbInput) {
			in.output = absorbOutsideFail + absorbAttestLine(n, "pass", "modb:tests", "moda/a,modb/b")
		}, candidate: true, reason: absorbRefusedChangedPackageFailed},
		{name: "module set mismatch (extra attested module)", mut: func(in *flakeAbsorbInput) {
			in.output = absorbOutsideFail + absorbAttestLine(n, "pass", "moda:tests,modb:tests", "moda/a")
		}, candidate: true, reason: absorbRefusedModuleSetMismatch},
		{name: "recurred test", mut: func(in *flakeAbsorbInput) {
			in.earlier = []verifyFailures{{tests: []string{"TestFlaky"}, pkgs: []string{"example.com/modb/zz"}}}
		}, candidate: true, reason: absorbRefusedRecurred},
		{name: "recurred package", mut: func(in *flakeAbsorbInput) {
			in.earlier = []verifyFailures{{tests: []string{"TestOther"}, pkgs: []string{"example.com/modb/b"}}}
		}, candidate: true, reason: absorbRefusedRecurred},
		{name: "unrelated earlier failure", mut: func(in *flakeAbsorbInput) {
			in.earlier = []verifyFailures{{tests: []string{"TestInside"}, pkgs: []string{"example.com/moda/a"}}}
		}, candidate: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			if tc.mut != nil {
				tc.mut(&in)
			}
			plan, reason, candidate := decideFlakeAbsorb(in)
			if candidate != tc.candidate || reason != tc.reason {
				t.Fatalf("candidate=%t reason=%q, want candidate=%t reason=%q", candidate, reason, tc.candidate, tc.reason)
			}
			if candidate && reason == "" {
				want := flakeAbsorbPlan{
					dirs: []string{"modb/b"}, pkgs: []string{"example.com/modb/b"}, tests: []string{"TestFlaky"},
					failedModules: []string{"modb"}, patchCoverage: "pass", tree: "t1", output: in.output,
				}
				if !reflect.DeepEqual(plan, want) {
					t.Errorf("plan = %+v, want %+v", plan, want)
				}
			}
		})
	}
}

func TestDecideFlakeAbsorb_SortsAndDeduplicatesDirs(t *testing.T) {
	out := "--- FAIL: TestB (0s)\nFAIL\texample.com/modb/b\t0.1s\n--- FAIL: TestA (0s)\nFAIL\texample.com/modb/moda/a\t0.1s\n" +
		absorbAttestLine(goldenAttestNonce, "pass", "modb:tests", "moda/c")
	plan, reason, candidate := decideFlakeAbsorb(flakeAbsorbInput{
		output: out, form: verifyFormFull, disp: gateExecuted, nonce: goldenAttestNonce,
		scopePkgs: []string{"moda/c"}, modules: absorbModules,
	})
	if !candidate || reason != "" || !reflect.DeepEqual(plan.dirs, []string{"modb/b", "modb/moda/a"}) {
		t.Fatalf("candidate=%t reason=%q dirs=%v", candidate, reason, plan.dirs)
	}
}

func TestAbsorbRerunRefusal(t *testing.T) {
	// scripts/test's refusal TEXT on an executed gate that did not exit 73:
	// the #3948 text classifier is deleted (#4022), so it is rerun_failed.
	lock := "scripts/test: another RUNNER verify still holds the lock at /l after 600s\n"
	for _, tc := range []struct {
		outcome string
		disp    gateDisposition
		out     string
		want    string
	}{
		{"failed", gateExecuted, absorbOutsideFail, absorbRefusedRerunFailed},
		{"failed", gateExecuted, golangciLintLockSignature + "\n", absorbRefusedRerunFailed},
		{"failed", gateExecuted, lock, absorbRefusedRerunFailed},
		{"skipped", gateExecuted, "", absorbRefusedRerunNotExecuted},
		{"failed", gateTimedOut, "", absorbRefusedRerunNotExecuted},
		{"failed", gateRefused, "", absorbRefusedRerunNotExecuted},
		{"passed", gateCheckoutRefused, "", absorbRefusedRerunNotExecuted},
		// The exit-code dispositions (#4022), with SIGNATURE-FREE output: only
		// the disposition keeps them out of rerun_failed.
		{"failed", gateTempFail, absorbOutsideFail, absorbRefusedRerunNotExecuted},
		{"failed", gateLockRefused, absorbOutsideFail, absorbRefusedRerunNotExecuted},
	} {
		if got := absorbRerunRefusal(tc.outcome, tc.disp, tc.out); got != tc.want {
			t.Errorf("absorbRerunRefusal(%q, %v, %q) = %q, want %q", tc.outcome, tc.disp, tc.out, got, tc.want)
		}
	}
}

func TestGateEnvDeniesAbsorbNonce(t *testing.T) {
	if !gateEnvDenied(verifyAbsorbNonceEnvVar) {
		t.Errorf("gateEnvDenied(%q) = false — the absorb-nonce deny entry is missing", verifyAbsorbNonceEnvVar)
	}
	for _, kv := range sanitizeEnv([]string{verifyAbsorbNonceEnvVar + "=" + goldenAttestNonce, "PATH=/usr/bin"}) {
		if strings.HasPrefix(kv, verifyAbsorbNonceEnvVar+"=") {
			t.Errorf("sanitizeEnv admitted an ambient absorb nonce: %q", kv)
		}
	}
}

// ---------------------------------------------------------------------------
// Loop level, through the REAL runVerifyFixLoop. The fixture repo is the
// two-module workspace above; the agent's in-scope edit modifies
// moda/a/a.go, so the plan scope's package set is [moda/a]. The verify
// command is scripted per call; `@NONCE@` in a scripted output is replaced
// with the FISHHAWK_VERIFY_ABSORB_NONCE the call actually saw, and every
// call's package set and nonce are recorded.
// ---------------------------------------------------------------------------

type absorbStep struct {
	pkgs string // the expected FISHHAWK_VERIFY_PACKAGES ("" = full form)
	out  string
	rc   int
}

type absorbFixture struct {
	cfg       config
	repo      string
	dir       string
	steps     []absorbStep
	wantCalls int // recorded calls expected; 0 = len(steps)
}

func newAbsorbFixture(t *testing.T, maxIter int, steps ...absorbStep) *absorbFixture {
	t.Helper()
	repo, _ := absorbRepo(t)
	mustWrite(t, filepath.Join(repo, "moda", "a", "a.go"), "package a\n\n// agent change\n")
	dir := t.TempDir()
	for i, s := range steps {
		mustWrite(t, filepath.Join(dir, fmt.Sprintf("out.%d", i+1)), s.out)
		mustWrite(t, filepath.Join(dir, fmt.Sprintf("rc.%d", i+1)), fmt.Sprintf("%d\n", s.rc))
	}
	script := filepath.Join(dir, "verify.sh")
	mustWrite(t, script, fmt.Sprintf(`#!/bin/sh
d=%q
n=$(cat "$d/count" 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > "$d/count"
printf '%%s pkgs=[%%s] nonce=[%%s]\n' "$n" "$%s" "$%s" >> "$d/calls.log"
if [ -f "$d/out.$n" ]; then sed "s/@NONCE@/$%s/g" "$d/out.$n"; exit "$(cat "$d/rc.$n")"; fi
echo "verify fixture: unscripted call $n"
exit 97
`, dir, verifyPackagesEnvVar, verifyAbsorbNonceEnvVar, verifyAbsorbNonceEnvVar))
	return &absorbFixture{
		cfg: config{
			runID:               verifiedTreeRunID,
			stageID:             verifiedTreeStageID,
			workingDir:          repo,
			verifyCmd:           "sh " + script,
			verifyMaxIterations: maxIter,
			scopeFiles:          []upload.ScopeFile{{Path: "moda/a/a.go", Operation: "modify"}},
		},
		repo: repo, dir: dir, steps: steps,
	}
}

type absorbCall struct{ pkgs, nonce string }

func (f *absorbFixture) calls(t *testing.T) []absorbCall {
	t.Helper()
	re := regexp.MustCompile(`^\d+ pkgs=\[(.*)\] nonce=\[(.*)\]$`)
	var out []absorbCall
	for _, l := range readVerifyFormLog(t, filepath.Join(f.dir, "calls.log")) {
		m := re.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("unparseable call record %q", l)
		}
		out = append(out, absorbCall{pkgs: m[1], nonce: m[2]})
	}
	return out
}

// assertCalls checks the recorded call count and each call's package set.
func (f *absorbFixture) assertCalls(t *testing.T) []absorbCall {
	t.Helper()
	calls := f.calls(t)
	want := f.wantCalls
	if want == 0 {
		want = len(f.steps)
	}
	if len(calls) != want {
		t.Errorf("verify calls = %d, want %d: %+v", len(calls), want, calls)
	}
	for i, c := range calls {
		if i < len(f.steps) && c.pkgs != f.steps[i].pkgs {
			t.Errorf("verify call %d pkgs = %q, want %q", i+1, c.pkgs, f.steps[i].pkgs)
		}
	}
	return calls
}

// scriptedOut renders step i's output as call i saw it.
func (f *absorbFixture) scriptedOut(t *testing.T, i int) string {
	t.Helper()
	return strings.ReplaceAll(f.steps[i].out, "@NONCE@", f.calls(t)[i].nonce)
}

func (f *absorbFixture) run(t *testing.T, inv *fakeInvoker) (agent.Result, string, string) {
	t.Helper()
	res := agent.Result{OK: true}
	var logSink strings.Builder
	_, tree, err := runVerifyFixLoop(context.Background(), &f.cfg, nil, "", inv, agent.Invocation{}, &res, &logSink)
	if err != nil {
		t.Fatalf("runVerifyFixLoop: %v\n%s", err, logSink.String())
	}
	return res, tree, logSink.String()
}

// todaysCategoryA is the byte-exact category-A FailureReason the loop has
// always produced (C7: a refused absorb never changes it).
func todaysCategoryA(cfg config, attempts int, out string) string {
	return fmt.Sprintf("verify command %q still failing after %d iteration(s):\n%s", cfg.verifyCmd, attempts, out)
}

func absorbOutsideStep(attest string) absorbStep {
	return absorbStep{out: absorbOutsideFail + attest + "scripts/test: FAILED modules: modb\n", rc: 1}
}

func goodAbsorbAttest() string { return absorbAttestLine("@NONCE@", "pass", "modb:tests", "moda/a") }

func editModaA(repo string) func(int) {
	return func(i int) {
		_ = os.WriteFile(filepath.Join(repo, "moda", "a", "a.go"), []byte(fmt.Sprintf("package a\n\n// fix %d\n", i)), 0o644) //nolint:gosec // test-owned temp path
	}
}

func verifyRunTrees(t *testing.T, res agent.Result) []string {
	t.Helper()
	var trees []string
	for _, p := range eventPayloads(t, res, "verify_run") {
		s, _ := p["tree_sha"].(string)
		trees = append(trees, s)
	}
	return trees
}

func TestRunVerifyFixLoop_OutsideFlakeAbsorbedAtExhaustion(t *testing.T) {
	f := newAbsorbFixture(t, 1,
		absorbStep{pkgs: "moda/a", out: absorbInsideFail, rc: 1},             // iter 0: scoped fail inside
		absorbStep{pkgs: "moda/a", out: absorbPassOut},                       // iter 1: scoped pass
		absorbOutsideStep(goodAbsorbAttest()),                                //         full fail outside
		absorbStep{pkgs: "modb/b", out: "ok  \texample.com/modb/b\t0.02s\n"}, // absorb re-run
	)
	inv := cappedInvoker(t, 2, editModaA(f.repo))
	res, tree, log := f.run(t, inv)
	calls := f.assertCalls(t)

	if !res.OK || tree == "" {
		t.Fatalf("OK=%t tree=%q, want an absorbed pass\n%s", res.OK, tree, log)
	}
	if inv.callIdx != 1 {
		t.Errorf("fix invokes = %d, want 1", inv.callIdx)
	}
	trees := verifyRunTrees(t, res)
	if len(trees) != 4 || trees[2] == "" || tree != trees[2] || trees[3] != trees[2] {
		t.Errorf("verified tree %q must equal the failed full verify's tree; verify_run trees = %q", tree, trees)
	}
	if len(calls) == 4 {
		if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(calls[2].nonce) {
			t.Errorf("the deciding full verify carried nonce %q, want 32 hex", calls[2].nonce)
		}
		for _, i := range []int{0, 1, 3} {
			if calls[i].nonce != "" {
				t.Errorf("verify call %d carried nonce %q; only the (c2) full form may", i+1, calls[i].nonce)
			}
		}
	}
	ab := eventPayloads(t, res, verifyFlakeAbsorbedEvent)
	if len(ab) != 1 {
		t.Fatalf("verify_flake_absorbed events = %d, want 1\n%s", len(ab), log)
	}
	if !reflect.DeepEqual(ab[0]["package_dirs"], []any{"modb/b"}) || !reflect.DeepEqual(ab[0]["packages"], []any{"example.com/modb/b"}) ||
		!reflect.DeepEqual(ab[0]["tests"], []any{"TestFlaky"}) || ab[0]["patch_coverage"] != "pass" ||
		!reflect.DeepEqual(ab[0]["failed_modules"], []any{"modb"}) || ab[0]["tree_sha"] != tree {
		t.Errorf("verify_flake_absorbed payload = %v", ab[0])
	}
	sum := noopSummaryOf(t, res)
	detail, _ := sum["detail"].(string)
	if sum["outcome"] != "passed" || !strings.HasPrefix(detail, verifyFlakeAbsorbedLead) || !strings.Contains(detail, "did NOT pass the full test loop") {
		t.Errorf("verify_summary = %v, want outcome=passed with a %s detail", sum, verifyFlakeAbsorbedLead)
	}
	if !reflect.DeepEqual(sum["absorbed_tests"], []any{"TestFlaky"}) || !reflect.DeepEqual(sum["absorbed_packages"], []any{"modb/b"}) || sum["patch_coverage"] != "pass" {
		t.Errorf("verify_summary absorbed fields = %v", sum)
	}
	if _, ok := sum["absorb_refused"]; ok {
		t.Errorf("an absorbed pass carries absorb_refused: %v", sum)
	}
	if n := len(eventPayloads(t, res, verifyFlakeAbsorbRefusedEvent)); n != 0 {
		t.Errorf("refused events = %d, want 0", n)
	}
	// gate_evidence folds the event.
	ev := composeGateEvidence(res.Events, 1)
	if ev == nil {
		t.Fatal("composeGateEvidence returned no event")
	}
	var ge gateEvidencePayload
	if err := json.Unmarshal(ev.Payload, &ge); err != nil || ge.FlakeAbsorbed == nil || ge.FlakeAbsorbed.TreeSHA != tree {
		t.Errorf("gate_evidence.flake_absorbed = %+v (%v)", ge.FlakeAbsorbed, err)
	}
}

// assertRefused checks a refused absorb: category A with today's exact
// FailureReason over the failed full verify (call fullIdx), the refusal reason
// on the event and the summary, and no absorbed event.
func assertRefused(t *testing.T, f *absorbFixture, res agent.Result, tree, log string, attempts, fullIdx int, reason string) {
	t.Helper()
	if res.OK || res.FailureCategory != "A" || tree != "" {
		t.Fatalf("OK=%t category=%q tree=%q, want today's category A\n%s", res.OK, res.FailureCategory, tree, log)
	}
	if want := todaysCategoryA(f.cfg, attempts, f.scriptedOut(t, fullIdx)); res.FailureReason != want {
		t.Errorf("FailureReason changed:\n got %q\nwant %q", res.FailureReason, want)
	}
	refused := eventPayloads(t, res, verifyFlakeAbsorbRefusedEvent)
	if len(refused) != 1 || refused[0]["reason"] != reason {
		t.Errorf("refused events = %v, want one reason=%s\n%s", refused, reason, log)
	}
	if n := len(eventPayloads(t, res, verifyFlakeAbsorbedEvent)); n != 0 {
		t.Errorf("absorbed events = %d, want 0", n)
	}
	if sum := noopSummaryOf(t, res); sum["outcome"] != "failed" || sum["absorb_refused"] != reason {
		t.Errorf("verify_summary = %v, want outcome=failed absorb_refused=%s", sum, reason)
	}
}

// The failing package's import path ENDS with the scope dir (moda/a), so the
// relation's suffix rule calls it inside — while its mapped dir (modb/moda/a)
// is outside both the plan scope and changed_dirs, and the attest is fully
// valid. The relation is therefore the only control in the path.
func TestRunVerifyFixLoop_InsideFailureNeverAbsorbed(t *testing.T) {
	out := "--- FAIL: TestCollide (0.01s)\nFAIL\texample.com/modb/moda/a\t0.02s\n"
	f := newAbsorbFixture(t, 0,
		absorbStep{pkgs: "moda/a", out: absorbPassOut},
		absorbStep{out: out + goodAbsorbAttest(), rc: 1},
		absorbStep{pkgs: "modb/moda/a", out: "ok\n"}, // would pass if re-run
	)
	f.wantCalls = 2
	res, tree, log := f.run(t, cappedInvoker(t, 1, nil))
	f.assertCalls(t)
	if res.OK || res.FailureCategory != "A" || tree != "" {
		t.Fatalf("OK=%t category=%q, want category A\n%s", res.OK, res.FailureCategory, log)
	}
	if want := todaysCategoryA(f.cfg, 1, f.scriptedOut(t, 1)); res.FailureReason != want {
		t.Errorf("FailureReason changed:\n got %q\nwant %q", res.FailureReason, want)
	}
	if n := len(eventPayloads(t, res, verifyFlakeAbsorbRefusedEvent)) + len(eventPayloads(t, res, verifyFlakeAbsorbedEvent)); n != 0 {
		t.Errorf("an inside failure is not a candidate, yet %d absorb events were emitted", n)
	}
}

func refusedAtExhaustion(t *testing.T, attest string, reason string) {
	t.Helper()
	f := newAbsorbFixture(t, 0,
		absorbStep{pkgs: "moda/a", out: absorbPassOut},
		absorbOutsideStep(attest),
		absorbStep{pkgs: "modb/b", out: "ok\n"}, // would pass if re-run
	)
	f.wantCalls = 2
	res, tree, log := f.run(t, cappedInvoker(t, 1, nil))
	f.assertCalls(t)
	assertRefused(t, f, res, tree, log, 1, 1, reason)
}

func TestRunVerifyFixLoop_AbsorbRefusedUnderPatchCoverage(t *testing.T) {
	refusedAtExhaustion(t, absorbAttestLine("@NONCE@", "fail", "modb:tests", "moda/a"), absorbRefusedPatchCoverageFail)
}

func TestRunVerifyFixLoop_AbsorbRefusedOnForeignNonce(t *testing.T) {
	refusedAtExhaustion(t, absorbAttestLine(goldenAttestNonce, "pass", "modb:tests", "moda/a"), absorbRefusedAttestMissing)
}

// The parsed FAIL lines carry no [build failed], so only the attest's class
// can refuse it.
func TestRunVerifyFixLoop_AbsorbRefusedOnBuildClassModule(t *testing.T) {
	refusedAtExhaustion(t, absorbAttestLine("@NONCE@", "pass", "modb:build", "moda/a"), absorbRefusedModuleNotTestFailure)
}

// C1: the failing package is in the DIFF (changed_dirs) but not in the plan
// scope.
func TestRunVerifyFixLoop_AbsorbRefusedWhenChangedPackageFailed(t *testing.T) {
	refusedAtExhaustion(t, absorbAttestLine("@NONCE@", "pass", "modb:tests", "moda/a,modb/b"), absorbRefusedChangedPackageFailed)
}

// C7: a failing re-run keeps today's FailureReason byte-for-byte — the
// re-run is not counted in the attempt number.
func TestRunVerifyFixLoop_AbsorbRefusedWhenRerunFails(t *testing.T) {
	f := newAbsorbFixture(t, 0,
		absorbStep{pkgs: "moda/a", out: absorbPassOut},
		absorbOutsideStep(goodAbsorbAttest()),
		absorbStep{pkgs: "modb/b", out: absorbOutsideFail, rc: 1},
	)
	res, tree, log := f.run(t, cappedInvoker(t, 1, nil))
	f.assertCalls(t)
	assertRefused(t, f, res, tree, log, 1, 1, absorbRefusedRerunFailed)
	if want := fmt.Sprintf("verify command %q still failing after 1 iteration(s):\n", f.cfg.verifyCmd); !strings.HasPrefix(res.FailureReason, want) {
		t.Errorf("FailureReason lead = %.120q, want %q", res.FailureReason, want)
	}
}

// C8: a re-run whose output carries an infra signature or a lock refusal is
// settled by the absorb branch alone — no infra re-run, no lock re-run.
func TestRunVerifyFixLoop_AbsorbRerunInfraOrLockIsNotReabsorbed(t *testing.T) {
	for _, tc := range []struct {
		name, out, reason string
		rc                int
	}{
		{"infra signature", golangciLintLockSignature + "\n", absorbRefusedRerunFailed, 1},
		// Refusal TEXT without exit 73 is an ordinary failure since the #3948
		// text classifier was deleted (#4022); exit 73 alone is the refusal.
		{"lock refusal text without exit 73", "scripts/test: another RUNNER verify still holds the lock at /l after 600s\n", absorbRefusedRerunFailed, 1},
		{"lock refusal exit 73", "", absorbRefusedRerunNotExecuted, verifyExitLockRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAbsorbFixture(t, 0,
				absorbStep{pkgs: "moda/a", out: absorbPassOut},
				absorbOutsideStep(goodAbsorbAttest()),
				absorbStep{pkgs: "modb/b", out: tc.out, rc: tc.rc},
				absorbStep{pkgs: "modb/b", out: "ok\n"}, // a second re-run would pass
			)
			f.wantCalls = 3
			res, tree, log := f.run(t, cappedInvoker(t, 1, nil))
			f.assertCalls(t)
			assertRefused(t, f, res, tree, log, 1, 1, tc.reason)
			if strings.Contains(log, `"event":"verify_infra_flake_retry"`) || strings.Contains(log, "verify_lock_contended") {
				t.Errorf("the absorb re-run reached the infra / lock branches:\n%s", log)
			}
		})
	}
}

func TestRunVerifyFixLoop_AbsorbRefusedWhenRerunSkipped(t *testing.T) {
	orig := materializeGateCheckoutFn
	t.Cleanup(func() { materializeGateCheckoutFn = orig })
	n := 0
	materializeGateCheckoutFn = func(ctx context.Context, repoDir, headSHA, parent string) (string, error) {
		n++
		if n == 3 {
			return "", errors.New("clone refused for the test")
		}
		return orig(ctx, repoDir, headSHA, parent)
	}
	f := newAbsorbFixture(t, 0,
		absorbStep{pkgs: "moda/a", out: absorbPassOut},
		absorbOutsideStep(goodAbsorbAttest()),
	)
	res, tree, log := f.run(t, cappedInvoker(t, 1, nil))
	f.assertCalls(t)
	assertRefused(t, f, res, tree, log, 1, 1, absorbRefusedRerunNotExecuted)
}

func TestRunVerifyFixLoop_AbsorbRefusedOnTreeChange(t *testing.T) {
	f := newAbsorbFixture(t, 0,
		absorbStep{pkgs: "moda/a", out: absorbPassOut},
		absorbOutsideStep(goodAbsorbAttest()),
		absorbStep{pkgs: "modb/b", out: "ok\n"}, // would pass if re-run
	)
	f.wantCalls = 2
	t.Cleanup(func() { flakeAbsorbBeforeRerunFn = nil })
	flakeAbsorbBeforeRerunFn = func() {
		mustWrite(t, filepath.Join(f.repo, "moda", "a", "a.go"), "package a\n\n// changed under the absorb\n")
	}
	res, tree, log := f.run(t, cappedInvoker(t, 1, nil))
	f.assertCalls(t)
	assertRefused(t, f, res, tree, log, 1, 1, absorbRefusedTreeChanged)
}

// C3: TestFlaky in modb/b already failed in an earlier verify of this pass.
func TestRunVerifyFixLoop_AbsorbRefusedWhenRecurred(t *testing.T) {
	f := newAbsorbFixture(t, 1,
		absorbStep{pkgs: "moda/a", out: absorbPassOut},
		absorbOutsideStep(goodAbsorbAttest()), // iter 0: outside, budget left → fix
		absorbStep{pkgs: "moda/a", out: absorbPassOut},
		absorbOutsideStep(goodAbsorbAttest()),   // iter 1: the same failure again
		absorbStep{pkgs: "modb/b", out: "ok\n"}, // would pass if re-run
	)
	f.wantCalls = 4
	res, tree, log := f.run(t, cappedInvoker(t, 2, editModaA(f.repo)))
	f.assertCalls(t)
	assertRefused(t, f, res, tree, log, 2, 3, absorbRefusedRecurred)
}

func TestRunVerifyFixLoop_NoAbsorbBeforeExhaustion(t *testing.T) {
	f := newAbsorbFixture(t, 1,
		absorbStep{pkgs: "moda/a", out: absorbPassOut},
		absorbOutsideStep(goodAbsorbAttest()),
		absorbStep{pkgs: "moda/a", out: absorbPassOut},
		absorbStep{out: absorbPassOut},
	)
	inv := cappedInvoker(t, 2, editModaA(f.repo))
	res, tree, log := f.run(t, inv)
	f.assertCalls(t)
	if !res.OK || tree == "" || inv.callIdx != 1 {
		t.Fatalf("OK=%t tree=%q fix invokes=%d, want the fix agent re-invoked once then a full pass\n%s", res.OK, tree, inv.callIdx, log)
	}
	if n := len(eventPayloads(t, res, verifyFlakeAbsorbedEvent)) + len(eventPayloads(t, res, verifyFlakeAbsorbRefusedEvent)); n != 0 {
		t.Errorf("absorb events = %d before exhaustion, want 0", n)
	}
}

// The #4036 no-op flake re-run that reproduces an outside failure stays
// category C; the absorb never sees it.
func TestRunVerifyFixLoop_ReproducedFlakeRerunStaysCategoryC(t *testing.T) {
	f := newAbsorbFixture(t, 2,
		absorbStep{pkgs: "moda/a", out: absorbPassOut},
		absorbOutsideStep(goodAbsorbAttest()),
		absorbStep{pkgs: "moda/a", out: absorbPassOut},
		absorbOutsideStep(goodAbsorbAttest()),
	)
	res, _, log := f.run(t, cappedInvoker(t, 2, nil))
	f.assertCalls(t)
	if res.OK || res.FailureCategory != "C" || !strings.HasPrefix(res.FailureReason, verifyFailureOutsideChangeLead) {
		t.Fatalf("OK=%t category=%q, want the #4036 category C\n%s", res.OK, res.FailureCategory, log)
	}
	if n := len(eventPayloads(t, res, verifyFlakeAbsorbedEvent)) + len(eventPayloads(t, res, verifyFlakeAbsorbRefusedEvent)); n != 0 {
		t.Errorf("absorb events = %d, want 0", n)
	}
}

// C4: an ambient FISHHAWK_VERIFY_ABSORB_NONCE never reaches a scoped verify or
// the absorb re-run, and never replaces the runner's own nonce.
func TestRunVerifyFixLoop_AmbientAbsorbNonceNeverReachesAVerify(t *testing.T) {
	const ambient = "ffffffffffffffffffffffffffffffff"
	t.Setenv(verifyAbsorbNonceEnvVar, ambient)
	f := newAbsorbFixture(t, 0,
		absorbStep{pkgs: "moda/a", out: absorbPassOut},
		absorbOutsideStep(goodAbsorbAttest()),
		absorbStep{pkgs: "modb/b", out: "ok\n"},
	)
	res, _, log := f.run(t, cappedInvoker(t, 1, nil))
	calls := f.assertCalls(t)
	if !res.OK {
		t.Fatalf("want the absorbed pass\n%s", log)
	}
	if len(calls) != 3 {
		return
	}
	if calls[0].nonce != "" || calls[2].nonce != "" {
		t.Errorf("an ambient nonce reached a scoped verify: %+v", calls)
	}
	if calls[1].nonce == ambient || calls[1].nonce == "" {
		t.Errorf("the deciding full verify carried %q, want the runner's own fresh nonce", calls[1].nonce)
	}
}
