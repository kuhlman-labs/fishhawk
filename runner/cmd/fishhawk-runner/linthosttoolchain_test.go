package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- #4245 lint-leg host-toolchain refusal ------------------------------
//
// CROSS-BOUNDARY: every test here drives the REAL `scripts/test lint`
// (cmd_lint -> _require_lint_host_toolchain -> scripts/check-gate-image --host
// -> the REAL deploy/gate-image/Dockerfile pins) with fake golangci-lint, gofmt
// and go first on PATH, so the seam between the lint leg, the checker and the
// pin file is exercised end to end rather than per layer.
//
// Each refusal fixture's fake golangci-lint `run` appends to FAKE_RUN_LOG,
// prints a gofmt-shaped finding and exits 1: with the host check deleted from
// cmd_lint nothing stands between the lint leg and that `run`, so the log fills
// and the finding is printed with no host_toolchain_mismatch — the false
// format finding #4245 describes. Each fixture changes exactly ONE tool's
// version away from the pins, so the comparison under test is the only branch
// that can refuse.
//
// OUTPUT HYGIENE. This repository's committed-tree gate runs these tests, and
// a failing test's output becomes that gate's verify output. Failure messages
// route captured output through lintHostHygiene so neither the fake format
// finding (which the runner's auto-format absorb parses, #3316) nor the
// refusal token is echoed verbatim.

const (
	lintHostFakeFinding = "fake.go:1:1: File is not properly formatted (gofmt)"
	lintHostRefusal     = "host_toolchain_mismatch"
	// Foreign values, asserted to differ from the Dockerfile pins so a future
	// pin can never coincide with one and silently turn a refusal into a pass.
	lintHostForeignGLVersion = "0.0.1"
	lintHostForeignGo        = "1.99.0"
)

func lintHostHygiene(s string) string {
	s = strings.ReplaceAll(s, lintHostFakeFinding, "<fake-format-finding>")
	return strings.ReplaceAll(s, lintHostRefusal, "<refusal-token>")
}

// gateImagePins is the pin pair the host check enforces, read from the REAL
// Dockerfile — never hard-coded, so a pin bump cannot strand this test.
type gateImagePins struct {
	goVersion string // e.g. 1.26.9
	goMM      string // e.g. 1.26
	glVersion string // e.g. 2.12.2 (no leading v)
}

// dockerfileArg returns the value of the first non-comment `ARG NAME=value`
// line, mirroring check-gate-image's arg_default.
func dockerfileArg(t *testing.T, dockerfile, name string) string {
	t.Helper()
	f, err := os.Open(dockerfile) //nolint:gosec // fixed workspace-relative path
	if err != nil {
		t.Fatalf("opening %s: %v", dockerfile, err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	prefix := "ARG " + name + "="
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		if v, ok := strings.CutPrefix(line, prefix); ok {
			if i := strings.IndexAny(v, " \t"); i >= 0 {
				v = v[:i]
			}
			v = strings.Trim(v, `"'`)
			if v != "" {
				return v
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading %s: %v", dockerfile, err)
	}
	t.Fatalf("%s has no `%s<version>` default — the host check's pin cannot be read", dockerfile, prefix)
	return ""
}

func readGateImagePins(t *testing.T, root string) gateImagePins {
	t.Helper()
	dockerfile := filepath.Join(root, "deploy", "gate-image", "Dockerfile")
	goVersion := dockerfileArg(t, dockerfile, "GO_VERSION")
	parts := strings.SplitN(goVersion, ".", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("GO_VERSION %q has no MAJOR.MINOR", goVersion)
	}
	p := gateImagePins{
		goVersion: goVersion,
		goMM:      parts[0] + "." + parts[1],
		glVersion: strings.TrimPrefix(dockerfileArg(t, dockerfile, "GOLANGCI_LINT_VERSION"), "v"),
	}
	if p.glVersion == lintHostForeignGLVersion {
		t.Fatalf("the foreign golangci-lint version %q equals the pin — pick another", lintHostForeignGLVersion)
	}
	if strings.HasPrefix(lintHostForeignGo, p.goMM+".") {
		t.Fatalf("the foreign Go %q shares the pinned MAJOR.MINOR %s — pick another", lintHostForeignGo, p.goMM)
	}
	return p
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// writeLintHostFakes writes FAKEBIN: golangci-lint (version line from env;
// `run` logs $PWD, prints the fake finding, exits $FAKE_RUN_RC), gofmt (never
// meant to run) and go (answers `version <FAKEBIN>/gofmt`, else execs the real
// go, so cmd_lint's `go work edit -json` module walk still works).
func writeLintHostFakes(t *testing.T, realGo string) string {
	t.Helper()
	bin := t.TempDir()
	gofmt := filepath.Join(bin, "gofmt")
	files := map[string]string{
		"golangci-lint": `#!/bin/sh
case "$1" in
  version)
    printf 'golangci-lint has version %s built with go%s from fake on 2026-01-01T00:00:00Z\n' "$FAKE_GL_VER" "$FAKE_GL_GO"
    exit 0 ;;
  run)
    printf '%s\n' "$PWD" >> "$FAKE_RUN_LOG"
    echo '` + lintHostFakeFinding + `'
    exit "${FAKE_RUN_RC:-1}" ;;
esac
echo "fake golangci-lint: unexpected argv: $*" >&2
exit 99
`,
		"gofmt": "#!/bin/sh\necho 'fake gofmt must never run' >&2\nexit 98\n",
		"go": `#!/bin/sh
if [ "$1" = version ] && [ "$2" = ` + shellQuote(gofmt) + ` ]; then
  printf '%s: go%s\n' "$2" "$FAKE_GOFMT_GO"
  exit 0
fi
exec ` + shellQuote(realGo) + ` "$@"
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil { //nolint:gosec // test fakes must be executable
			t.Fatal(err)
		}
	}
	return bin
}

type lintHostFixture struct {
	glVersion, glGo, gofmtGo string
	runRC                    string
}

type lintHostResult struct {
	out    string
	err    error
	runLog string
}

// runRealLintLeg runs `bash <root>/scripts/test lint` under the fixture.
func runRealLintLeg(t *testing.T, root string, fx lintHostFixture) lintHostResult {
	t.Helper()
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go not on PATH (%v): the fake go must exec the real one for cmd_lint's module walk", err)
	}
	bin := writeLintHostFakes(t, realGo)
	runLog := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(runLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "FISHHAWK_TEST_LIB_ONLY="),
			strings.HasPrefix(kv, "PATH="),
			strings.HasPrefix(kv, "GIT_CONFIG_GLOBAL="),
			strings.HasPrefix(kv, "GIT_CONFIG_SYSTEM="):
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"FAKE_GL_VER="+fx.glVersion,
		"FAKE_GL_GO="+fx.glGo,
		"FAKE_GOFMT_GO="+fx.gofmtGo,
		"FAKE_RUN_RC="+fx.runRC,
		"FAKE_RUN_LOG="+runLog,
	)
	cmd := exec.Command("bash", filepath.Join(root, "scripts", "test"), "lint") //nolint:gosec // fixed workspace-relative path
	cmd.Dir = root
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()
	logged, err := os.ReadFile(runLog) //nolint:gosec // test temp file
	if err != nil {
		t.Fatal(err)
	}
	return lintHostResult{out: string(out), err: runErr, runLog: string(logged)}
}

func requireBashForLintHost(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not on PATH (%v); skipping the cross-boundary lint-leg test", err)
	}
	return requireWorkspaceRoot(t, pkgSrcDir)
}

// assertLintRefused: the lint leg exited non-zero naming the refusal and the
// offending value, and golangci-lint `run` NEVER executed.
func assertLintRefused(t *testing.T, res lintHostResult, offending string) {
	t.Helper()
	clean := lintHostHygiene(res.out)
	if res.err == nil {
		t.Errorf("scripts/test lint exited 0 on a mismatched host toolchain; output:\n%s", clean)
	}
	if !strings.Contains(res.out, lintHostRefusal) {
		t.Errorf("output lacks the refusal token; output:\n%s", clean)
	}
	if !strings.Contains(res.out, offending) {
		t.Errorf("output does not name the offending value %q; output:\n%s", offending, clean)
	}
	if res.runLog != "" {
		t.Errorf("golangci-lint run executed despite the mismatch (run log: %q); output:\n%s", res.runLog, clean)
	}
	if strings.Contains(res.out, lintHostFakeFinding) {
		t.Errorf("the host toolchain's format finding reached the output; output:\n%s", clean)
	}
}

func TestLintLegRefusesForeignGolangciLintVersion(t *testing.T) {
	root := requireBashForLintHost(t)
	pins := readGateImagePins(t, root)
	res := runRealLintLeg(t, root, lintHostFixture{
		glVersion: lintHostForeignGLVersion, glGo: pins.goVersion, gofmtGo: pins.goVersion, runRC: "1",
	})
	assertLintRefused(t, res, "host golangci-lint "+lintHostForeignGLVersion+" differs")
}

func TestLintLegRefusesGolangciLintBuiltWithForeignGo(t *testing.T) {
	root := requireBashForLintHost(t)
	pins := readGateImagePins(t, root)
	res := runRealLintLeg(t, root, lintHostFixture{
		glVersion: pins.glVersion, glGo: lintHostForeignGo, gofmtGo: pins.goVersion, runRC: "1",
	})
	assertLintRefused(t, res, "host golangci-lint is built with go"+strings.Join(strings.SplitN(lintHostForeignGo, ".", 3)[:2], "."))
}

func TestLintLegRefusesForeignGofmt(t *testing.T) {
	root := requireBashForLintHost(t)
	pins := readGateImagePins(t, root)
	res := runRealLintLeg(t, root, lintHostFixture{
		glVersion: pins.glVersion, glGo: pins.goVersion, gofmtGo: lintHostForeignGo, runRC: "1",
	})
	assertLintRefused(t, res, "/gofmt is built with go"+strings.Join(strings.SplitN(lintHostForeignGo, ".", 3)[:2], "."))
}

// TestLintLegRefusesWithoutCheckGateImage: the host check is a CONTROL, so a
// checkout whose scripts/check-gate-image is missing refuses to lint rather
// than skipping the check (unlike verify's static pin report).
func TestLintLegRefusesWithoutCheckGateImage(t *testing.T) {
	root := requireBashForLintHost(t)
	pins := readGateImagePins(t, root)
	src, err := os.ReadFile(filepath.Join(root, "scripts", "test")) //nolint:gosec // fixed workspace-relative path
	if err != nil {
		t.Fatal(err)
	}
	bare := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bare, "scripts"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bare, "scripts", "test"), src, 0o755); err != nil { //nolint:gosec // the copied script must be executable
		t.Fatal(err)
	}
	res := runRealLintLeg(t, bare, lintHostFixture{
		glVersion: pins.glVersion, glGo: pins.goVersion, gofmtGo: pins.goVersion, runRC: "1",
	})
	clean := lintHostHygiene(res.out)
	if res.err == nil {
		t.Errorf("scripts/test lint exited 0 without scripts/check-gate-image; output:\n%s", clean)
	}
	if !strings.Contains(res.out, "scripts/check-gate-image missing or not executable") || !strings.Contains(res.out, "refusing to lint (fail closed)") {
		t.Errorf("output does not name the missing checker; output:\n%s", clean)
	}
	if res.runLog != "" {
		t.Errorf("golangci-lint run executed without the host check (run log: %q)", res.runLog)
	}
}

// TestLintLegRunsOnPinnedHostToolchain is the anti-vacuity arm: with every
// tool on the pins the lint leg runs golangci-lint in EVERY go.work module, so
// a host check that always refuses fails here.
func TestLintLegRunsOnPinnedHostToolchain(t *testing.T) {
	root := requireBashForLintHost(t)
	pins := readGateImagePins(t, root)
	res := runRealLintLeg(t, root, lintHostFixture{
		glVersion: pins.glVersion, glGo: pins.goVersion, gofmtGo: pins.goVersion, runRC: "0",
	})
	clean := lintHostHygiene(res.out)
	if res.err != nil {
		t.Fatalf("scripts/test lint failed on a pinned host toolchain: %v; output:\n%s", res.err, clean)
	}
	if strings.Contains(res.out, lintHostRefusal) {
		t.Errorf("a pinned host toolchain was refused; output:\n%s", clean)
	}
	want := goWorkModuleCount(t, root)
	got := 0
	for _, l := range strings.Split(res.runLog, "\n") {
		if strings.TrimSpace(l) != "" {
			got++
		}
	}
	if got != want {
		t.Errorf("golangci-lint run executed in %d module(s), want one per go.work module (%d); run log: %q", got, want, res.runLog)
	}
}

// goWorkModuleCount counts go.work `use` entries (single-line and block form).
func goWorkModuleCount(t *testing.T, root string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "go.work")) //nolint:gosec // fixed workspace-relative path
	if err != nil {
		t.Fatal(err)
	}
	n, inBlock := 0, false
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case line == "":
		case inBlock && line == ")":
			inBlock = false
		case inBlock:
			n++
		case line == "use (":
			inBlock = true
		case strings.HasPrefix(line, "use "):
			n++
		}
	}
	if n == 0 {
		t.Fatalf("go.work under %s declares no modules", root)
	}
	return n
}
