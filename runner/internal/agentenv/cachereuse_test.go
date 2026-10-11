package agentenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These two tests drive the REAL go toolchain against an isolated GOCACHE to
// prove the SHIPPED behavior of the GOFLAGS overlay (#4180), not just its
// string shape: that a second worktree path reuses the first path's compile
// entries under the Env-composed env, and that the reuse never extends to a
// cached TEST RESULT. They are hermetic: the base env is built explicitly (not
// from os.Environ()) with GOPROXY=off, GOWORK=off, GOTOOLCHAIN=local and
// GOENV=off, and the fixture module has no imports to fetch, so they run
// unchanged inside the runner's --network=none container gate.

// goTool resolves the go binary. It t.Fatals rather than t.Skips when go is
// absent, which deviates from this module's convention of skipping on a
// missing tool: every gate that runs `go test` has go on PATH by construction,
// so a missing binary here is a broken gate environment, and a Skip would turn
// the only test of the overlay's shipped effect into a silent vacuous green.
func goTool(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go is not on PATH (%v): every gate that runs this test has the toolchain, so a missing binary is a broken environment, not a reason to skip the cache-reuse proof", err)
	}
	return p
}

// hermeticBase builds the base env slice a runner would hand to Env, with an
// isolated cache and module dirs under t.TempDir() and everything that could
// reach the network or a host-level go configuration pinned off.
func hermeticBase(t *testing.T, gocache string) []string {
	t.Helper()
	root := t.TempDir()
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(root, "home"),
		"TMPDIR=" + root,
		"GOPATH=" + filepath.Join(root, "gopath"),
		"GOMODCACHE=" + filepath.Join(root, "gopath", "pkg", "mod"),
		"GOCACHE=" + gocache,
		"GOENV=off",
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"GOPROXY=off",
	}
}

// writeModule writes a zero-import module into dir.
func writeModule(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files["go.mod"] = "module example.com/p\n\ngo 1.21\n"
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// runGo runs `go args...` in dir under env and returns its combined output.
func runGo(t *testing.T, goBin, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(goBin, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// countCompiles counts the compile-tool invocations for example.com/p in a
// `go build -x` transcript.
func countCompiles(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "compile") && strings.Contains(line, " -p example.com/p ") {
			n++
		}
	}
	return n
}

// buildTwice builds the fixture module at two sibling directories under ONE
// shared GOCACHE with env, returning the compile count at each path.
func buildTwice(t *testing.T, goBin string, env []string) (a, b int) {
	t.Helper()
	root := t.TempDir()
	dirA, dirB := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, d := range []string{dirA, dirB} {
		writeModule(t, d, map[string]string{"p.go": "package p\n\nfunc F() int { return 1 }\n"})
	}
	outA, err := runGo(t, goBin, dirA, env, "build", "-x", "./...")
	if err != nil {
		t.Fatalf("go build at A: %v\n%s", err, outA)
	}
	outB, err := runGo(t, goBin, dirB, env, "build", "-x", "./...")
	if err != nil {
		t.Fatalf("go build at B: %v\n%s", err, outB)
	}
	return countCompiles(outA), countCompiles(outB)
}

// TestEnv_TrimpathSharesCompileEntriesAcrossWorktreePaths is the issue's
// cache-reuse criterion measured on the real toolchain: a package built at one
// directory path is NOT recompiled at a second path when the build runs under
// the Env-composed env. Without -trimpath the go command hashes the build
// directory into the workspace package's compile action ID (go1.25.6
// src/cmd/go/internal/work/exec.go buildActionID), so every per-run lineage
// worktree adds fresh entries to the host cache.
//
// The first arm asserts A compiled once (a precondition, so a zero at B cannot
// be vacuous) and B compiled zero times. The second arm is the discrimination
// control: the same two-directory build under a hand-built env WITHOUT the
// overlay (bypassing Env) must recompile at B, which proves the path-hashing
// premise still holds and the first arm is measuring the flag, not a cache
// that happens to be shared anyway.
func TestEnv_TrimpathSharesCompileEntriesAcrossWorktreePaths(t *testing.T) {
	goBin := goTool(t)

	t.Run("with overlay", func(t *testing.T) {
		env, refused := Env(hermeticBase(t, filepath.Join(t.TempDir(), "gocache")))
		if len(refused) != 0 {
			t.Fatalf("refused = %v, want none", refused)
		}
		if got := goflagsEntries(env); len(got) != 1 || got[0] != "-trimpath" {
			t.Fatalf("composed GOFLAGS = %q, want exactly [-trimpath]", got)
		}
		a, b := buildTwice(t, goBin, env)
		if a != 1 {
			t.Fatalf("compiles at A = %d, want 1 (precondition: the fixture must compile once into the fresh cache)", a)
		}
		if b != 0 {
			t.Errorf("compiles at B = %d, want 0: -trimpath must let a second worktree path reuse the first path's compile entries", b)
		}
	})

	t.Run("without overlay recompiles", func(t *testing.T) {
		env := hermeticBase(t, filepath.Join(t.TempDir(), "gocache"))
		for _, kv := range env {
			if strings.HasPrefix(kv, "GOFLAGS=") {
				t.Fatalf("the discrimination env must carry no GOFLAGS, got %q", kv)
			}
		}
		a, b := buildTwice(t, goBin, env)
		if a != 1 {
			t.Fatalf("compiles at A = %d, want 1 (precondition)", a)
		}
		if b < 1 {
			t.Errorf("compiles at B = %d, want >= 1: without -trimpath a second path must recompile; the path-hashing premise no longer holds and the reuse arm is vacuous", b)
		}
	})
}

// TestEnv_TrimpathDoesNotShareTestResultsAcrossPaths is the safety pin that
// the overlay cannot serve a stale cached TEST RESULT across worktrees. Both
// directories hold a byte-identical _test.go that reads fixture.txt through an
// init-captured absolute os.Getwd() dir (the shape #4179's pkgSrcDir uses); A's
// fixture is good and B's is bad. Under the Env-composed env B must FAIL and
// must not be reported (cached).
//
// It runs `go test .` in PACKAGE-LIST mode on purpose: `go test` with no
// package argument is local-directory mode, which disables test-result caching
// altogether, so B "re-running" there would prove nothing. The positive
// control closes the same hole from the other side: a second run at A must be
// reported (cached), so B re-executing is a demonstrated cache MISS and not a
// toolchain with caching switched off.
func TestEnv_TrimpathDoesNotShareTestResultsAcrossPaths(t *testing.T) {
	goBin := goTool(t)
	env, refused := Env(hermeticBase(t, filepath.Join(t.TempDir(), "gocache")))
	if len(refused) != 0 {
		t.Fatalf("refused = %v, want none", refused)
	}
	if got := goflagsEntries(env); len(got) != 1 || got[0] != "-trimpath" {
		t.Fatalf("composed GOFLAGS = %q, want exactly [-trimpath]", got)
	}

	const testSrc = `package p

import (
	"os"
	"path/filepath"
	"testing"
)

var srcDir = mustGetwd()

func mustGetwd() string {
	d, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return d
}

func TestFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(srcDir, "fixture.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "good\n" {
		t.Fatalf("fixture = %q, want good", b)
	}
}
`
	root := t.TempDir()
	dirA, dirB := filepath.Join(root, "a"), filepath.Join(root, "b")
	writeModule(t, dirA, map[string]string{"p.go": "package p\n", "p_test.go": testSrc, "fixture.txt": "good\n"})
	writeModule(t, dirB, map[string]string{"p.go": "package p\n", "p_test.go": testSrc, "fixture.txt": "bad\n"})

	outA, err := runGo(t, goBin, dirA, env, "test", ".")
	if err != nil {
		t.Fatalf("go test at A (good fixture) failed: %v\n%s", err, outA)
	}
	if strings.Contains(outA, "(cached)") {
		t.Fatalf("first run at A was already cached, so the cache was not fresh:\n%s", outA)
	}

	// Positive control: caching is live in package-list mode.
	outA2, err := runGo(t, goBin, dirA, env, "test", ".")
	if err != nil {
		t.Fatalf("second go test at A failed: %v\n%s", err, outA2)
	}
	if !strings.Contains(outA2, "(cached)") {
		t.Fatalf("second run at A was not reported (cached); test-result caching is off, so B re-executing proves nothing:\n%s", outA2)
	}

	outB, err := runGo(t, goBin, dirB, env, "test", ".")
	if err == nil {
		t.Errorf("go test at B passed; want a FAIL (a differing fixture at a second worktree path must not reuse A's cached pass):\n%s", outB)
	}
	if strings.Contains(outB, "(cached)") {
		t.Errorf("go test at B was served from the cache; -trimpath must not share test results across worktree paths:\n%s", outB)
	}
}
