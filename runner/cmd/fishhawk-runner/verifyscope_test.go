package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// D. verifyScopePackages — the plan-scope → package-dir mapping and every
// fail-safe WIDENING branch.
// ---------------------------------------------------------------------------

func TestVerifyScopePackages(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope []string
		want  []string
	}{
		{
			name:  "go files map to deduplicated sorted package dirs",
			scope: []string{"runner/cmd/fishhawk-runner/main.go", "backend/internal/run/a.go", "runner/cmd/fishhawk-runner/gateenv.go"},
			want:  []string{"backend/internal/run", "runner/cmd/fishhawk-runner"},
		},
		{
			name:  "a root-level go file maps to dot",
			scope: []string{"doc.go"},
			want:  []string{"."},
		},
		{
			name:  "non-go scope files contribute nothing",
			scope: []string{"AGENTS.md", "docs/ARCHITECTURE.md", "scripts/test", ".fishhawk/workflows.yaml"},
			want:  nil,
		},
		{
			name:  "a go-named directory is not mistaken for a file",
			scope: []string{"backend/internal/run/x.go", "docs/notes.go.md"},
			want:  []string{"backend/internal/run"},
		},
		{
			name:  "empty entries are skipped, not mapped to dot",
			scope: []string{"", "backend/internal/run/a.go", ""},
			want:  []string{"backend/internal/run"},
		},
		{
			name:  "an empty scope yields the empty set",
			scope: nil,
			want:  nil,
		},
		// --- the WIDENING fail-safes, one case per named branch -------------
		{
			name:  "a comma in a GO path widens to the empty set",
			scope: []string{"backend/internal/run/a.go", "backend/internal/we,ird/b.go"},
			want:  nil,
		},
		{
			name:  "a comma in a NON-GO path widens too",
			scope: []string{"backend/internal/run/a.go", "docs/a,b.md"},
			want:  nil,
		},
		{
			name:  "a tab in a path widens to the empty set",
			scope: []string{"backend/internal/run/a.go", "backend/internal/ta\tb/c.go"},
			want:  nil,
		},
		{
			name:  "a newline in a path widens to the empty set",
			scope: []string{"backend/internal/run/a.go", "backend/internal/nl\nx/c.go"},
			want:  nil,
		},
		{
			name:  "an absolute path widens to the empty set",
			scope: []string{"/etc/passwd.go"},
			want:  nil,
		},
		{
			name:  "a path escaping the repo root widens to the empty set",
			scope: []string{"backend/internal/run/a.go", "../outside/b.go"},
			want:  nil,
		},
		{
			name:  "a bare parent reference widens to the empty set",
			scope: []string{".."},
			want:  nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := verifyScopePackages(tc.scope)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("verifyScopePackages(%q) = %q, want %q", tc.scope, got, tc.want)
			}
		})
	}
}

// TestVerifyScopePackagesEmptySetRunsTheFullForm pins the fail-safe direction
// BEHAVIOURALLY rather than by return value alone (test_strategy D): an
// undecodable scope must not merely return nil, it must make the caller run the
// FULL form — the variable ABSENT, not present-and-empty.
func TestVerifyScopePackagesEmptySetRunsTheFullForm(t *testing.T) {
	undecodable := []string{"backend/internal/run/a.go", "backend/internal/we,ird/b.go"}
	pkgs := verifyScopePackages(undecodable)

	if verifyFormName(pkgs) != verifyFormFull {
		t.Fatalf("an undecodable scope selected form %q, want %q", verifyFormName(pkgs), verifyFormFull)
	}
	env := verifyScopeEnv(verifyLockOwnerEnv(nil), pkgs)
	for _, kv := range env {
		if strings.HasPrefix(kv, verifyPackagesEnvVar+"=") {
			t.Fatalf("an undecodable scope still injected %q — the full form must carry the variable ABSENT", kv)
		}
	}
	// The owner marker is orthogonal and must survive.
	if !envHas(env, verifyLockOwnerEnvVar+"="+verifyLockOwnerRunner) {
		t.Fatalf("owner marker missing from the full-form env: %q", env)
	}
}

// TestVerifyFormName pins the form selector both ways.
func TestVerifyFormName(t *testing.T) {
	if got := verifyFormName(nil); got != verifyFormFull {
		t.Errorf("verifyFormName(nil) = %q, want %q", got, verifyFormFull)
	}
	if got := verifyFormName([]string{}); got != verifyFormFull {
		t.Errorf("verifyFormName(empty) = %q, want %q", got, verifyFormFull)
	}
	if got := verifyFormName([]string{"backend/internal/run"}); got != verifyFormScoped {
		t.Errorf("verifyFormName(one pkg) = %q, want %q", got, verifyFormScoped)
	}
}

// ---------------------------------------------------------------------------
// D (cont). The two env builders.
// ---------------------------------------------------------------------------

func TestVerifyScopeEnvAndLockOwnerEnv(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/home/x"}

	t.Run("scope env appends exactly one comma-joined entry", func(t *testing.T) {
		got := verifyScopeEnv(base, []string{"backend/internal/run", "cli/internal/spec"})
		if len(got) != len(base)+1 {
			t.Fatalf("verifyScopeEnv appended %d entries, want 1: %q", len(got)-len(base), got)
		}
		want := verifyPackagesEnvVar + "=backend/internal/run,cli/internal/spec"
		if got[len(got)-1] != want {
			t.Fatalf("last entry = %q, want %q", got[len(got)-1], want)
		}
		// The sanitized entries are undisturbed.
		for _, kv := range base {
			if !envHas(got, kv) {
				t.Fatalf("verifyScopeEnv dropped a sanitized entry %q from %q", kv, got)
			}
		}
	})

	t.Run("scope env returns env unchanged for an empty set", func(t *testing.T) {
		for _, pkgs := range [][]string{nil, {}} {
			got := verifyScopeEnv(base, pkgs)
			if !reflect.DeepEqual(got, base) {
				t.Fatalf("verifyScopeEnv(base, %q) = %q, want it unchanged (%q)", pkgs, got, base)
			}
		}
	})

	t.Run("scope env replaces rather than duplicates an inherited entry", func(t *testing.T) {
		polluted := append(append([]string{}, base...), verifyPackagesEnvVar+"=attacker/pkg")
		got := verifyScopeEnv(polluted, []string{"backend/internal/run"})
		n := 0
		for _, kv := range got {
			if strings.HasPrefix(kv, verifyPackagesEnvVar+"=") {
				n++
				if kv != verifyPackagesEnvVar+"=backend/internal/run" {
					t.Fatalf("surviving entry = %q, want the runner's own value", kv)
				}
			}
		}
		if n != 1 {
			t.Fatalf("found %d %s entries, want exactly 1: %q", n, verifyPackagesEnvVar, got)
		}
	})

	t.Run("lock owner env appends exactly one runner marker", func(t *testing.T) {
		got := verifyLockOwnerEnv(base)
		if len(got) != len(base)+1 {
			t.Fatalf("verifyLockOwnerEnv appended %d entries, want 1: %q", len(got)-len(base), got)
		}
		want := verifyLockOwnerEnvVar + "=" + verifyLockOwnerRunner
		if got[len(got)-1] != want {
			t.Fatalf("last entry = %q, want %q", got[len(got)-1], want)
		}
	})

	t.Run("lock owner env replaces rather than duplicates an inherited entry", func(t *testing.T) {
		polluted := append(append([]string{}, base...), verifyLockOwnerEnvVar+"=shell")
		got := verifyLockOwnerEnv(polluted)
		n := 0
		for _, kv := range got {
			if strings.HasPrefix(kv, verifyLockOwnerEnvVar+"=") {
				n++
				if kv != verifyLockOwnerEnvVar+"="+verifyLockOwnerRunner {
					t.Fatalf("surviving entry = %q, want the runner marker", kv)
				}
			}
		}
		if n != 1 {
			t.Fatalf("found %d %s entries, want exactly 1: %q", n, verifyLockOwnerEnvVar, got)
		}
	})

	t.Run("the builders do not mutate the caller's slice", func(t *testing.T) {
		callers := []string{"PATH=/usr/bin", "HOME=/home/x"}
		snapshot := append([]string{}, callers...)
		_ = verifyScopeEnv(verifyLockOwnerEnv(callers), []string{"backend/internal/run"})
		if !reflect.DeepEqual(callers, snapshot) {
			t.Fatalf("caller slice mutated: %q, want %q", callers, snapshot)
		}
	})
}

func envHas(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// D2. verifyLockPathEnv — the ADR-063 / #2134 lock-location injection.
//
// The runner's throwaway verify checkout is a CLONE with its own common dir, so
// the lock `scripts/test` would derive there contends with nothing. The
// resolver must yield the PRIMARY's lock from either the primary or one of its
// linked worktrees, and must leave env UNCHANGED when there is no repository
// to resolve.
// ---------------------------------------------------------------------------

// primaryLockPath is the expected injected value for repo: git's own absolute
// common dir plus the lock basename. Computed through git (not filepath.Join on
// the temp dir) so a symlinked TMPDIR (macOS /var -> /private/var) cannot fail
// the byte comparison.
func primaryLockPath(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse --git-common-dir in %s: %v\n%s", repo, err, out)
	}
	return strings.TrimRight(strings.TrimSpace(string(out)), "/") + "/" + verifyLockFileName
}

func TestVerifyLockPathEnv(t *testing.T) {
	primary := initRepo(t)
	want := primaryLockPath(t, primary)
	base := []string{"PATH=/usr/bin", "HOME=/home/x"}

	t.Run("primary checkout resolves its own common dir", func(t *testing.T) {
		got := verifyLockPathEnv(base, primary)
		if len(got) != len(base)+1 || got[len(got)-1] != verifyLockPathEnvVar+"="+want {
			t.Fatalf("verifyLockPathEnv = %q, want %q appended to base", got, verifyLockPathEnvVar+"="+want)
		}
	})

	t.Run("a LINKED worktree resolves the PRIMARY lock, not its own gitdir", func(t *testing.T) {
		linked := filepath.Join(t.TempDir(), "linked")
		cmd := exec.Command("git", "-C", primary, "worktree", "add", "-q", "--detach", linked)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git worktree add: %v\n%s", err, out)
		}
		t.Cleanup(func() {
			_ = exec.Command("git", "-C", primary, "worktree", "remove", "--force", linked).Run()
		})
		got := verifyLockPathEnv(base, linked)
		if !envHas(got, verifyLockPathEnvVar+"="+want) {
			t.Fatalf("from a linked worktree verifyLockPathEnv = %q, want the PRIMARY lock %q", got, want)
		}
		for _, kv := range got {
			if strings.HasPrefix(kv, verifyLockPathEnvVar+"=") && strings.Contains(kv, "/worktrees/") {
				t.Fatalf("the injected lock is keyed under the linked worktree's private gitdir: %q", kv)
			}
		}
	})

	t.Run("a CLONE resolves the CLONE's common dir — why the runner passes the primary", func(t *testing.T) {
		// Documents the hazard the injection closes: asked about the clone
		// itself, the resolver (like scripts/test's derivation) yields a lock
		// nothing else contends on. The runner therefore passes the PRIMARY.
		clone := filepath.Join(t.TempDir(), "clone")
		cmd := exec.Command("git", "clone", "-q", "--no-hardlinks", primary, clone)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git clone: %v\n%s", err, out)
		}
		got := verifyLockPathEnv(base, clone)
		if envHas(got, verifyLockPathEnvVar+"="+want) {
			t.Fatalf("a clone resolved the primary's lock %q — the fixture is not a clone with its own common dir", want)
		}
		if !envHas(got, verifyLockPathEnvVar+"="+primaryLockPath(t, clone)) {
			t.Fatalf("a clone did not resolve its own common dir: %q", got)
		}
	})

	t.Run("a non-git directory leaves env UNCHANGED", func(t *testing.T) {
		got := verifyLockPathEnv(base, t.TempDir())
		if !reflect.DeepEqual(got, base) {
			t.Fatalf("verifyLockPathEnv on a non-git dir = %q, want base unchanged %q", got, base)
		}
	})

	t.Run("a nonexistent directory leaves env UNCHANGED", func(t *testing.T) {
		got := verifyLockPathEnv(base, filepath.Join(t.TempDir(), "missing"))
		if !reflect.DeepEqual(got, base) {
			t.Fatalf("verifyLockPathEnv on a missing dir = %q, want base unchanged %q", got, base)
		}
	})

	t.Run("an inherited entry is dropped, never duplicated", func(t *testing.T) {
		withAmbient := append([]string{verifyLockPathEnvVar + "=/nowhere/attacker.lock"}, base...)
		got := verifyLockPathEnv(withAmbient, primary)
		n := 0
		for _, kv := range got {
			if strings.HasPrefix(kv, verifyLockPathEnvVar+"=") {
				n++
				if kv != verifyLockPathEnvVar+"="+want {
					t.Fatalf("the ambient lock path survived: %q", kv)
				}
			}
		}
		if n != 1 {
			t.Fatalf("verifyLockPathEnv left %d %s entries, want exactly 1: %q", n, verifyLockPathEnvVar, got)
		}
		// The input slice is never aliased or mutated.
		if withAmbient[0] != verifyLockPathEnvVar+"=/nowhere/attacker.lock" {
			t.Fatalf("verifyLockPathEnv mutated its input: %q", withAmbient)
		}
	})

	t.Run("the injected name is not admitted by sanitization — only injection delivers it", func(t *testing.T) {
		if gateEnvAllowed(verifyLockPathEnvVar) || !gateEnvDenied(verifyLockPathEnvVar) {
			t.Fatalf("%s must be denied by both layers so only the runner's own post-sanitization injection can set it", verifyLockPathEnvVar)
		}
	})
}

// ---------------------------------------------------------------------------
// F. NAME PIN — the cheap complement to the executable test below.
//
// The runner's consts and `scripts/test` are a bare string contract across a
// Go module and a bash script that cannot import each other. This is the same
// body-grep construction scripts/test-dev uses to bind _build_ldflags to
// version.go: it binds the NAMES. It does NOT bind the VALUE ENCODING — that is
// TestRunnerEnvDrivesRealScriptsTestVerify's job.
// ---------------------------------------------------------------------------

func TestVerifyPackagesEnvNameMatchesScriptsTest(t *testing.T) {
	root := repoRootForVerifyScopeTest(t)
	scriptPath := filepath.Join(root, "scripts", "test")
	data, err := os.ReadFile(scriptPath) //nolint:gosec // fixed workspace-relative path
	if err != nil {
		t.Fatalf("go.work found at %s but scripts/test could not be read: %v", root, err)
	}
	src := string(data)
	for _, name := range []string{verifyPackagesEnvVar, verifyLockOwnerEnvVar, verifyLockPathEnvVar} {
		if !strings.Contains(src, name) {
			t.Errorf("scripts/test does not reference %q — the runner injects a variable the shell never reads", name)
		}
	}
	// The lock BASENAME is load-bearing too: the runner appends it to the
	// primary's common dir, and the shell's own derivation appends the same
	// literal — a rename on one side keys the two lock kinds to different files.
	if !strings.Contains(src, verifyLockFileName) {
		t.Errorf("scripts/test does not reference %q — the runner's injected lock path and the shell's derived one would name different files", verifyLockFileName)
	}
	// The owner VALUE is load-bearing too: scripts/test compares the variable
	// against this exact string to select runner-kind semantics.
	if !strings.Contains(src, `"`+verifyLockOwnerRunner+`"`) && !strings.Contains(src, "= "+verifyLockOwnerRunner) {
		t.Errorf("scripts/test does not compare %s against %q — the runner's owner marker would read as a shell caller",
			verifyLockOwnerEnvVar, verifyLockOwnerRunner)
	}
}

// ---------------------------------------------------------------------------
// E. EXECUTABLE CROSS-BOUNDARY TEST.
//
// The body-grep above proves the two sides spell the same NAMES. It cannot
// prove they agree on the ENCODING — a separator disagreement (comma vs space
// vs colon) passes the grep and breaks in production. This runs the REAL
// `scripts/test verify` against the standard fixture built by
// `scripts/test-verify-scope --fixture`, with an environment produced by
// CALLING verifyScopeEnv(verifyLockOwnerEnv(base), pkgs) — never by
// hand-writing the strings — and asserts on the recorded `go test` argv.
//
// It asserts BOTH arms: the SCOPED one (the loop restricted to exactly the
// named packages, the other module skipped) and the UNSCOPED one (the final
// authoritative invocation's environment, built the same way with an EMPTY set,
// producing the FULL loop).
// ---------------------------------------------------------------------------

func TestRunnerEnvDrivesRealScriptsTestVerify(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not on PATH (%v); skipping the cross-boundary script test", err)
	}
	root := repoRootForVerifyScopeTest(t)
	harness := filepath.Join(root, "scripts", "test-verify-scope")
	if _, err := os.Stat(harness); err != nil {
		t.Fatalf("scripts/test-verify-scope is missing (%v) — the scoped-verify shell half must ship with this contract", err)
	}

	fixture := t.TempDir()
	build := exec.Command("bash", harness, "--fixture", fixture) //nolint:gosec // fixed workspace-relative path
	build.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the standard fixture failed: %v\n%s", err, out)
	}
	goLog := filepath.Join(fixture, "logs", "go.log")

	// The base is what a sanitized gate env looks like at the point the runner
	// appends its injections: PATH pointed at the fixture's recording stubs.
	base := []string{
		"PATH=" + filepath.Join(fixture, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"FISHHAWK_TEST_P=4",
		"FISHHAWK_TEST_LEASE_DIR=" + filepath.Join(fixture, "leases"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		base = append(base, "TMPDIR="+tmp)
	}

	runVerify := func(t *testing.T, env []string) []string {
		t.Helper()
		if err := os.Truncate(goLog, 0); err != nil {
			t.Fatalf("truncating the go stub log: %v", err)
		}
		cmd := exec.Command("bash", filepath.Join(fixture, "scripts", "test"), "verify") //nolint:gosec // fixture path
		cmd.Dir = fixture
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the real scripts/test verify failed under the runner-built env: %v\n%s", err, out)
		}
		return readGoLog(t, goLog)
	}

	// --- SCOPED arm --------------------------------------------------------
	// The package set is derived from Go SCOPE FILES through the production
	// mapper, so a mapper/encoder disagreement fails here too.
	pkgs := verifyScopePackages([]string{
		"backend/internal/alpha/a.go",
		"backend/internal/alpha/b.go",
		"AGENTS.md",
	})
	if want := []string{"backend/internal/alpha"}; !reflect.DeepEqual(pkgs, want) {
		t.Fatalf("verifyScopePackages = %q, want %q", pkgs, want)
	}
	scopedEnv := verifyScopeEnv(verifyLockOwnerEnv(base), pkgs)
	if !envHas(scopedEnv, verifyPackagesEnvVar+"=backend/internal/alpha") {
		t.Fatalf("the scoped env is missing the packages entry: %q", scopedEnv)
	}

	scoped := runVerify(t, scopedEnv)
	if len(scoped) != 1 {
		t.Fatalf("scoped verify recorded %d `go test` invocations, want exactly 1 (the saving IS the skipped modules): %q",
			len(scoped), scoped)
	}
	if mod, _, _ := strings.Cut(scoped[0], "\t"); mod != "backend" {
		t.Errorf("scoped verify ran `go test` in module %q, want backend: %q", mod, scoped[0])
	}
	if !strings.Contains(scoped[0], "./internal/alpha") {
		t.Errorf("scoped verify did not name ./internal/alpha: %q", scoped[0])
	}
	if strings.Contains(scoped[0], "./...") {
		t.Errorf("scoped verify still ran the whole-module pattern — it is not scoped at all: %q", scoped[0])
	}
	if strings.Contains(scoped[0], "internal/beta") || strings.Contains(scoped[0], "internal/gamma") {
		t.Errorf("scoped verify named a package outside the derived set: %q", scoped[0])
	}

	// --- MULTI-PACKAGE SCOPED arm ------------------------------------------
	// The singleton arm above cannot exercise verifyPackagesSeparator at all: a
	// one-element join emits the same bytes under any separator, so changing the
	// production joiner would leave that arm's input unchanged and green. This
	// arm derives TWO distinct packages through the production mapper and asserts
	// on the package arguments the SHELL actually ran, which is the only place a
	// separator disagreement can show up. A joiner that is not the comma
	// `_verify_scope_buckets` splits on presents the pair as ONE mangled entry,
	// and the recorded argv then names neither package.
	multiPkgs := verifyScopePackages([]string{
		"backend/internal/alpha/a.go",
		"backend/internal/beta/b.go",
		"docs/ARCHITECTURE.md",
	})
	wantMulti := []string{"backend/internal/alpha", "backend/internal/beta"}
	if !reflect.DeepEqual(multiPkgs, wantMulti) {
		t.Fatalf("verifyScopePackages = %q, want %q", multiPkgs, wantMulti)
	}
	multiEnv := verifyScopeEnv(verifyLockOwnerEnv(base), multiPkgs)
	multiValue := ""
	for _, kv := range multiEnv {
		if v, ok := strings.CutPrefix(kv, verifyPackagesEnvVar+"="); ok {
			multiValue = v
		}
	}
	// Asserted so a joiner change is visible HERE as well as in the argv below,
	// and so the two assertions cannot both be satisfied by an encoding the shell
	// does not split.
	if !strings.Contains(multiValue, verifyPackagesSeparator) {
		t.Fatalf("the two-package env value %q does not carry the separator at all", multiValue)
	}

	multi := runVerify(t, multiEnv)
	if len(multi) != 1 {
		t.Fatalf("two packages in ONE module recorded %d `go test` invocations, want exactly 1: %q", len(multi), multi)
	}
	mod, argv, _ := strings.Cut(multi[0], "\t")
	if mod != "backend" {
		t.Errorf("multi-package verify ran `go test` in module %q, want backend: %q", mod, multi[0])
	}
	// Field-exact, not substring: a mangled single entry such as
	// "./internal/alpha:backend/internal/beta" CONTAINS both package names, so a
	// substring check would stay green under the very separator change this arm
	// exists to catch.
	gotPkgs := map[string]bool{}
	for _, f := range strings.Fields(argv) {
		if strings.HasPrefix(f, "./") {
			gotPkgs[f] = true
		}
	}
	for _, want := range []string{"./internal/alpha", "./internal/beta"} {
		if !gotPkgs[want] {
			t.Errorf("multi-package verify did not pass %q as its own package argument (env %q, argv %q)",
				want, multiValue, argv)
		}
	}
	if len(gotPkgs) != 2 {
		t.Errorf("multi-package verify passed %d package arguments, want exactly 2: %q", len(gotPkgs), argv)
	}
	if gotPkgs["./..."] {
		t.Errorf("multi-package verify widened to the whole-module pattern: %q", argv)
	}

	// --- UNSCOPED arm ------------------------------------------------------
	// The FINAL authoritative invocation's environment, built by the SAME
	// production builders with an EMPTY set. Asserting the ABSENCE is the point:
	// the authoritative gate must be unnarrowable.
	fullEnv := verifyScopeEnv(verifyLockOwnerEnv(base), nil)
	for _, kv := range fullEnv {
		if strings.HasPrefix(kv, verifyPackagesEnvVar+"=") {
			t.Fatalf("the full-form env carries %q — the authoritative gate must be unnarrowable", kv)
		}
	}

	full := runVerify(t, fullEnv)
	if len(full) < 2 {
		t.Fatalf("unscoped verify recorded %d `go test` invocations, want one per module (>=2): %q", len(full), full)
	}
	sawBackend, sawCLI := false, false
	for _, line := range full {
		mod, argv, _ := strings.Cut(line, "\t")
		if !strings.Contains(argv, "./...") {
			t.Errorf("unscoped verify did not run the whole-module pattern in %q: %q", mod, line)
		}
		switch mod {
		case "backend":
			sawBackend = true
		case "cli":
			sawCLI = true
		}
	}
	if !sawBackend || !sawCLI {
		t.Errorf("unscoped verify skipped a module (backend=%v cli=%v): %q", sawBackend, sawCLI, full)
	}

	// --- LOCK-PATH OVERRIDE arm (ADR-063 / #2134) --------------------------
	// The runner's throwaway verify checkout is a CLONE with its own common dir.
	// This arm runs the REAL scripts/test verify FROM such a clone of the
	// fixture, with FISHHAWK_VERIFY_LOCK_PATH produced by CALLING
	// verifyLockPathEnv against the PRIMARY fixture, against a live
	// `<pid>:shell` holder pre-seeded at that exact path. Runner-kind at a
	// 1-second budget (the fixture's `sleep` is a recording stub, so the wait
	// never really elapses) the run must DISPLACE that holder and say so on
	// stderr NAMING the override path — proof the two sides agree on the
	// variable's NAME and that its VALUE is consumed verbatim. The control arm
	// runs the same clone verify WITHOUT the variable: the clone keys its lock
	// to its own common dir, never meets the holder, prints no warning, and the
	// holder is left standing — the reopened-#2645 shape the override closes.
	clone := filepath.Join(t.TempDir(), "clone")
	cloneCmd := exec.Command("git", "clone", "-q", "--no-hardlinks", fixture, clone)
	cloneCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("cloning the fixture: %v\n%s", err, out)
	}
	// The fixture's logs/ is created after its base commit, so the clone lacks
	// it; scripts/test itself writes nothing there, but the recording stubs
	// resolve their log dir from the ORIGINAL fixture, so only mkdir is needed.
	if err := os.MkdirAll(filepath.Join(clone, "logs"), 0o750); err != nil {
		t.Fatal(err)
	}
	lockEnv := verifyLockPathEnv(verifyScopeEnv(verifyLockOwnerEnv(base), pkgs), fixture)
	lockPath := ""
	for _, kv := range lockEnv {
		if v, ok := strings.CutPrefix(kv, verifyLockPathEnvVar+"="); ok {
			lockPath = v
		}
	}
	if lockPath == "" {
		t.Fatalf("verifyLockPathEnv against the primary fixture injected nothing: %q", lockEnv)
	}
	if got := primaryLockPath(t, fixture); lockPath != got {
		t.Fatalf("injected lock path %q != the primary's common-dir lock %q", lockPath, got)
	}
	if primaryLockPath(t, clone) == lockPath {
		t.Fatalf("the clone shares the primary's common dir (%q) — the arm cannot discriminate", lockPath)
	}
	seedHolder := func(t *testing.T) {
		t.Helper()
		_ = os.Remove(lockPath)
		// Our own pid is live by construction; the kind is shell so a
		// runner-kind verify may displace it after the budget.
		if err := os.Symlink(fmt.Sprintf("%d:shell", os.Getpid()), lockPath); err != nil {
			t.Fatalf("seeding the shell holder at %s: %v", lockPath, err)
		}
	}
	runCloneVerify := func(t *testing.T, env []string) (string, error) {
		t.Helper()
		cmd := exec.Command("bash", filepath.Join(clone, "scripts", "test"), "verify") //nolint:gosec // fixture path
		cmd.Dir = clone
		cmd.Env = append(append([]string{}, env...), "FISHHAWK_VERIFY_LOCK_WAIT_SECONDS=1")
		var stderr strings.Builder
		cmd.Stdout = io.Discard
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stderr.String(), err
	}
	t.Cleanup(func() { _ = os.Remove(lockPath) })

	seedHolder(t)
	stderr, err := runCloneVerify(t, lockEnv)
	if err != nil {
		t.Fatalf("the clone verify under the lock-path override failed: %v\n%s", err, stderr)
	}
	wantWarn := "displacing a wedged shell verify holding " + lockPath
	if !strings.Contains(stderr, wantWarn) {
		t.Errorf("the override arm did not displace the holder at the injected path (want %q on stderr):\n%s", wantWarn, stderr)
	}
	if _, serr := os.Lstat(lockPath); serr == nil {
		t.Errorf("the runner-kind verify did not release the override lock at %s on exit", lockPath)
	}

	// Control arm: identical env MINUS the variable.
	seedHolder(t)
	stderrCtl, err := runCloneVerify(t, dropEnvKey(lockEnv, verifyLockPathEnvVar))
	if err != nil {
		t.Fatalf("the control clone verify failed: %v\n%s", err, stderrCtl)
	}
	if strings.Contains(stderrCtl, "displacing") {
		t.Errorf("without %s the clone verify still met the primary's holder — the arm does not discriminate:\n%s", verifyLockPathEnvVar, stderrCtl)
	}
	if target, rerr := os.Readlink(lockPath); rerr != nil || target != fmt.Sprintf("%d:shell", os.Getpid()) {
		t.Errorf("without the override the primary's holder should be untouched; readlink = %q, %v", target, rerr)
	}
}

func readGoLog(t *testing.T, path string) []string {
	t.Helper()
	fh, err := os.Open(path) //nolint:gosec // fixture path
	if err != nil {
		t.Fatalf("reading the go stub log: %v", err)
	}
	defer func() { _ = fh.Close() }()
	var lines []string
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanning the go stub log: %v", err)
	}
	return lines
}

// repoRootForVerifyScopeTest resolves the workspace root, reusing the same
// go.work walk the gate-env CLI cross-check uses. It SKIPS only when there is
// genuinely no workspace (a vendored or module-cache build); a present-but-
// unreadable peer is a detector malfunction and FAILS at the call site.
func repoRootForVerifyScopeTest(t *testing.T) string {
	t.Helper()
	root, err := findWorkspaceRoot()
	if err != nil {
		t.Skipf("go.work not found from the test source directory (%v); skipping the shell cross-check", err)
	}
	return root
}
