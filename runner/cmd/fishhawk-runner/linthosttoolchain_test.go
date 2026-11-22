package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
// meant to run) and go (answers `version <FAKEBIN>/gofmt`, and `env GOVERSION`
// / `env GOTOOLCHAIN` from FAKE_GO_VERSION / FAKE_GO_TOOLCHAIN when those are
// set (#5453), else execs the real go, so cmd_lint's `go work edit -json`
// module walk still works).
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
if [ "$1" = env ] && [ "$2" = GOVERSION ] && [ $# -eq 2 ] && [ -n "${FAKE_GO_VERSION+x}" ]; then
  printf '%s\n' "$FAKE_GO_VERSION"
  exit 0
fi
if [ "$1" = env ] && [ "$2" = GOTOOLCHAIN ] && [ $# -eq 2 ] && [ -n "${FAKE_GO_TOOLCHAIN+x}" ]; then
  printf '%s\n' "$FAKE_GO_TOOLCHAIN"
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
	// goVersion / goToolchain, when non-empty, are what the fake go answers
	// to `go env GOVERSION` / `go env GOTOOLCHAIN` (check-gate-image --host
	// check (d), #5453). Empty leaves both to the REAL go.
	goVersion, goToolchain string
}

type lintHostResult struct {
	out    string
	err    error
	code   int // the script's exit code (-1 when it did not exit normally)
	runLog string
}

// onPinGo makes the fake go answer check (d) with the pinned Go under
// GOTOOLCHAIN=local, so a refusal fixture's ONE off-pin tool is the only
// branch that can refuse, whatever the host's own go and GOTOOLCHAIN are.
func (fx lintHostFixture) onPinGo(p gateImagePins) lintHostFixture {
	fx.goVersion, fx.goToolchain = "go"+p.goVersion, "local"
	return fx
}

// runRealLintLeg runs `bash <root>/scripts/test lint` under the fixture. It
// pins GOLANGCI_LINT_CACHE to a t.TempDir() so scripts/test's per-checkout lint
// cache (#5151) never creates a dir in the real checkout's git dir or the
// user's cache dir: an explicit value is left untouched.
func runRealLintLeg(t *testing.T, root string, fx lintHostFixture) lintHostResult {
	t.Helper()
	return runRealLintLegEnv(t, root, fx, nil)
}

// runRealLintLegEnv is runRealLintLeg plus extraEnv, appended last (so it
// wins: exec.Cmd keeps the last value of a duplicated key). The ambient
// FISHHAWK_GATE_CONTAINER / FISHHAWK_GATE_IMAGE_REF are dropped first: inside
// the runner's gate container this test binary inherits the container marker,
// and a host-path fixture must not.
func runRealLintLegEnv(t *testing.T, root string, fx lintHostFixture, extraEnv []string) lintHostResult {
	t.Helper()
	return runRealScriptsTest(t, root, fx, extraEnv, "lint")
}

// runRealScriptsTest is runRealLintLegEnv for any scripts/test subcommand
// (args), so a test can drive the lint leg through `verify` as well.
func runRealScriptsTest(t *testing.T, root string, fx lintHostFixture, extraEnv []string, args ...string) lintHostResult {
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
			strings.HasPrefix(kv, "GIT_CONFIG_SYSTEM="),
			strings.HasPrefix(kv, "GOLANGCI_LINT_CACHE="),
			strings.HasPrefix(kv, "FISHHAWK_GATE_CONTAINER="),
			strings.HasPrefix(kv, "FISHHAWK_GATE_IMAGE_REF="),
			strings.HasPrefix(kv, "FAKE_GO_VERSION="),
			strings.HasPrefix(kv, "FAKE_GO_TOOLCHAIN="):
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GOLANGCI_LINT_CACHE="+t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"FAKE_GL_VER="+fx.glVersion,
		"FAKE_GL_GO="+fx.glGo,
		"FAKE_GOFMT_GO="+fx.gofmtGo,
		"FAKE_RUN_RC="+fx.runRC,
		"FAKE_RUN_LOG="+runLog,
	)
	if fx.goVersion != "" {
		env = append(env, "FAKE_GO_VERSION="+fx.goVersion)
	}
	if fx.goToolchain != "" {
		env = append(env, "FAKE_GO_TOOLCHAIN="+fx.goToolchain)
	}
	env = append(env, extraEnv...)
	cmd := exec.Command("bash", append([]string{filepath.Join(root, "scripts", "test")}, args...)...) //nolint:gosec // fixed workspace-relative path, fixed args
	cmd.Dir = root
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()
	logged, err := os.ReadFile(runLog) //nolint:gosec // test temp file
	if err != nil {
		t.Fatal(err)
	}
	code := 0
	if runErr != nil {
		code = -1
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			code = ee.ExitCode()
		}
	}
	return lintHostResult{out: string(out), err: runErr, code: code, runLog: string(logged)}
}

func requireBashForLintHost(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not on PATH (%v); skipping the cross-boundary lint-leg test", err)
	}
	return requireWorkspaceRoot(t, pkgSrcDir)
}

// assertLintRefused: the lint leg exited VERIFY_EXIT_TEMPFAIL (75, #5453: a
// host toolchain mismatch against coherent pins means the change was NOT
// judged) naming the refusal and the offending value, and golangci-lint `run`
// NEVER executed.
func assertLintRefused(t *testing.T, res lintHostResult, offending string) {
	t.Helper()
	clean := lintHostHygiene(res.out)
	if res.code != verifyExitTempFail {
		t.Errorf("scripts/test lint exited %d on a mismatched host toolchain, want %d (EX_TEMPFAIL: not judged); output:\n%s", res.code, verifyExitTempFail, clean)
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
	}.onPinGo(pins))
	assertLintRefused(t, res, "host golangci-lint "+lintHostForeignGLVersion+" differs")
}

func TestLintLegRefusesGolangciLintBuiltWithForeignGo(t *testing.T) {
	root := requireBashForLintHost(t)
	pins := readGateImagePins(t, root)
	res := runRealLintLeg(t, root, lintHostFixture{
		glVersion: pins.glVersion, glGo: lintHostForeignGo, gofmtGo: pins.goVersion, runRC: "1",
	}.onPinGo(pins))
	assertLintRefused(t, res, "host golangci-lint is built with go"+strings.Join(strings.SplitN(lintHostForeignGo, ".", 3)[:2], "."))
}

func TestLintLegRefusesForeignGofmt(t *testing.T) {
	root := requireBashForLintHost(t)
	pins := readGateImagePins(t, root)
	res := runRealLintLeg(t, root, lintHostFixture{
		glVersion: pins.glVersion, glGo: pins.goVersion, gofmtGo: lintHostForeignGo, runRC: "1",
	}.onPinGo(pins))
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
	if res.code != 1 {
		t.Errorf("scripts/test lint exited %d without scripts/check-gate-image, want 1 (a missing control is the tree's fault); output:\n%s", res.code, clean)
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

// --- #5453 stale gate image / host Go: exit 75, not "the change failed" ---
//
// A Go pin bump leaves a stale gate image (or host Go) whose `go` is older than
// a go.work / go.mod directive; under GOTOOLCHAIN=local that `go` refuses the
// module, and before #5453 the lint leg's exit 1 read as the change's failure
// (merge-candidate failed / verify_failed, an ejection and a D6 fix-up).
// golangci-lint and gofmt stay ON the pins here, so check-gate-image --host's
// Go-directive check (d) is the only branch that can refuse: with it disabled
// the lint leg reaches the fake `run`, which prints a finding and exits 1.

const lintHostStaleImage = "fishhawk-gate:stale-test"

// goWorkGoDirective returns go.work's column-0 `go` directive value and line.
func goWorkGoDirective(t *testing.T, root string) (string, int) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "go.work")) //nolint:gosec // fixed workspace-relative path
	if err != nil {
		t.Fatal(err)
	}
	for i, raw := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(raw, "go "); ok {
			if j := strings.Index(v, "//"); j >= 0 {
				v = v[:j]
			}
			return strings.TrimSpace(v), i + 1
		}
	}
	t.Fatalf("go.work under %s has no `go` directive", root)
	return "", 0
}

// goOneMinorBelow returns a GOVERSION one minor below directive (1.27.0 ->
// go1.26.0), which no GOTOOLCHAIN=local `go` may run the workspace with.
func goOneMinorBelow(t *testing.T, directive string) string {
	t.Helper()
	parts := strings.SplitN(directive, ".", 3)
	if len(parts) < 2 {
		t.Fatalf("go.work directive %q has no MAJOR.MINOR", directive)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil || minor == 0 {
		t.Fatalf("go.work directive %q: cannot step its minor down (%v)", directive, err)
	}
	return fmt.Sprintf("go%s.%d.0", parts[0], minor-1)
}

// staleGoFixture: golangci-lint and gofmt on the pins, a GOTOOLCHAIN=local go
// one minor below go.work's directive, and a fake `run` that would exit 1.
func staleGoFixture(t *testing.T, root string) (lintHostFixture, gateImagePins, string, string) {
	t.Helper()
	pins := readGateImagePins(t, root)
	directive, line := goWorkGoDirective(t, root)
	older := goOneMinorBelow(t, directive)
	fx := lintHostFixture{
		glVersion: pins.glVersion, glGo: pins.goVersion, gofmtGo: pins.goVersion, runRC: "1",
		goVersion: older, goToolchain: "local",
	}
	return fx, pins, older, fmt.Sprintf("go.work:%d `go %s`", line, directive)
}

// classifyLintLeg composes the real (exit code, output) through the runner's
// merge-candidate mapping exactly as the committed-tree pass does.
func classifyLintLeg(res lintHostResult) (string, string, mergeCandidateOutputEvidence) {
	outcome := "passed"
	if res.code != 0 {
		outcome = "failed"
	}
	result, reason := classifyMergeCandidateVerify(outcome, verifyExitDisposition(res.code, gateExecuted), res.out)
	return result, reason, mergeCandidateEvidence(res.out, result)
}

func TestLintLegStaleGateImageGoIsTempFail(t *testing.T) {
	root := requireBashForLintHost(t)
	fx, pins, older, directiveAt := staleGoFixture(t, root)
	res := runRealLintLegEnv(t, root, fx, []string{
		"FISHHAWK_GATE_CONTAINER=1",
		"FISHHAWK_GATE_IMAGE_REF=" + lintHostStaleImage,
		"GOTOOLCHAIN=local",
	})
	clean := lintHostHygiene(res.out)
	if res.code != verifyExitTempFail {
		t.Fatalf("scripts/test lint exited %d on a stale gate-image go, want %d (EX_TEMPFAIL); output:\n%s", res.code, verifyExitTempFail, clean)
	}
	for _, want := range []string{
		lintHostRefusal,
		"gate_image_stale: the gate image " + lintHostStaleImage,
		"host go " + older + " (GOTOOLCHAIN=local",
		directiveAt,
		"GO_VERSION " + pins.goVersion,
		"docker buildx build --load --build-context frontend=./frontend -t fishhawk-gate:local deploy/gate-image",
	} {
		if !strings.Contains(res.out, want) {
			t.Errorf("output lacks %q; output:\n%s", lintHostHygiene(want), clean)
		}
	}
	if res.runLog != "" || strings.Contains(res.out, lintHostFakeFinding) {
		t.Errorf("golangci-lint run executed behind a stale-image go (run log: %q); output:\n%s", res.runLog, clean)
	}
	result, reason, ev := classifyLintLeg(res)
	if result != mergeCandidateNotExecuted || reason != mcReasonTempFail {
		t.Errorf("merge-candidate classification = %s / %s, want %s / %s (never failed / verify_failed: the tree was not judged)",
			result, reason, mergeCandidateNotExecuted, mcReasonTempFail)
	}
	if !strings.Contains(ev.tail, lintHostStaleImage) || !strings.Contains(ev.tail, "GO_VERSION "+pins.goVersion) {
		t.Errorf("the merge-candidate tail does not name the image and the pin; tail:\n%s", lintHostHygiene(ev.tail))
	}
}

// TestVerifyLegStaleGateImageGoIsTempFail: the declared gate runs `scripts/test
// verify` (through scripts/verify-change), not `scripts/test lint`, so the
// lint leg's 75 must survive cmd_verify, which calls cmd_lint as a plain
// statement. A later edit that wraps cmd_lint in a pipeline or `if !` turns 75
// back into 1 (the change's fault) and fails this test. The verify lock is a
// t.TempDir() path, never the checkout's shared one, and is released on exit.
func TestVerifyLegStaleGateImageGoIsTempFail(t *testing.T) {
	root := requireBashForLintHost(t)
	fx, pins, older, directiveAt := staleGoFixture(t, root)
	lock := filepath.Join(t.TempDir(), "verify.lock")
	res := runRealScriptsTest(t, root, fx, []string{
		"FISHHAWK_GATE_CONTAINER=1",
		"FISHHAWK_GATE_IMAGE_REF=" + lintHostStaleImage,
		"FISHHAWK_VERIFY_LOCK_PATH=" + lock,
		"GOTOOLCHAIN=local",
	}, "verify", "--no-tests")
	clean := lintHostHygiene(res.out)
	if res.code != verifyExitTempFail {
		t.Fatalf("scripts/test verify --no-tests exited %d on a stale gate-image go, want %d (EX_TEMPFAIL: the lint leg's 75 must survive cmd_verify); output:\n%s", res.code, verifyExitTempFail, clean)
	}
	for _, want := range []string{
		lintHostRefusal,
		"gate_image_stale: the gate image " + lintHostStaleImage,
		"host go " + older + " (GOTOOLCHAIN=local",
		directiveAt,
		"GO_VERSION " + pins.goVersion,
	} {
		if !strings.Contains(res.out, want) {
			t.Errorf("output lacks %q; output:\n%s", lintHostHygiene(want), clean)
		}
	}
	if res.runLog != "" || strings.Contains(res.out, lintHostFakeFinding) {
		t.Errorf("golangci-lint run executed behind a stale-image go (run log: %q); output:\n%s", res.runLog, clean)
	}
	if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the verify lock %s survived the tempfail exit (lstat err %v)", lock, err)
	}
	if result, reason, _ := classifyLintLeg(res); result != mergeCandidateNotExecuted || reason != mcReasonTempFail {
		t.Errorf("merge-candidate classification = %s / %s, want %s / %s", result, reason, mergeCandidateNotExecuted, mcReasonTempFail)
	}
}

func TestLintLegHostGoOlderIsTempFail(t *testing.T) {
	root := requireBashForLintHost(t)
	fx, _, older, directiveAt := staleGoFixture(t, root)
	res := runRealLintLegEnv(t, root, fx, []string{"GOTOOLCHAIN=local"})
	clean := lintHostHygiene(res.out)
	if res.code != verifyExitTempFail {
		t.Fatalf("scripts/test lint exited %d on an older host go, want %d (EX_TEMPFAIL); output:\n%s", res.code, verifyExitTempFail, clean)
	}
	for _, want := range []string{lintHostRefusal, "host go " + older, directiveAt, "scripts/test lint --in-gate-image"} {
		if !strings.Contains(res.out, want) {
			t.Errorf("output lacks %q; output:\n%s", lintHostHygiene(want), clean)
		}
	}
	if strings.Contains(res.out, "gate_image_stale") {
		t.Errorf("a host-path refusal printed the in-container gate_image_stale line; output:\n%s", clean)
	}
	if res.runLog != "" {
		t.Errorf("golangci-lint run executed behind an older host go (run log: %q)", res.runLog)
	}
}

// TestLintLegTreePinDefectExitsOne: a pin-file defect the diff caused (here
// no GO_VERSION ARG) is the change's fault, so it stays exit 1 and reaches the
// fix agent as failed / verify_failed.
func TestLintLegTreePinDefectExitsOne(t *testing.T) {
	root := requireBashForLintHost(t)
	pins := readGateImagePins(t, root)
	copied := t.TempDir()
	for _, rel := range []string{"scripts/test", "scripts/check-gate-image"} {
		b, err := os.ReadFile(filepath.Join(root, rel)) //nolint:gosec // fixed workspace-relative path
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(copied, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o755); err != nil { //nolint:gosec // the copied scripts must be executable
			t.Fatal(err)
		}
	}
	df, err := os.ReadFile(filepath.Join(root, "deploy", "gate-image", "Dockerfile")) //nolint:gosec // fixed workspace-relative path
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, l := range strings.Split(string(df), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "ARG GO_VERSION=") {
			kept = append(kept, l)
		}
	}
	if err := os.MkdirAll(filepath.Join(copied, "deploy", "gate-image"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copied, "deploy", "gate-image", "Dockerfile"), []byte(strings.Join(kept, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	res := runRealLintLeg(t, copied, lintHostFixture{
		glVersion: pins.glVersion, glGo: pins.goVersion, gofmtGo: pins.goVersion, runRC: "1",
	}.onPinGo(pins))
	clean := lintHostHygiene(res.out)
	if res.code != 1 {
		t.Fatalf("scripts/test lint exited %d on a Dockerfile with no GO_VERSION ARG, want 1 (the tree's fault); output:\n%s", res.code, clean)
	}
	for _, want := range []string{"no `ARG GO_VERSION=<version>` default", "gate-image pins unreadable"} {
		if !strings.Contains(res.out, want) {
			t.Errorf("output lacks %q; output:\n%s", want, clean)
		}
	}
	if strings.Contains(res.out, lintHostRefusal) {
		t.Errorf("a pin-file defect was reported as a host toolchain mismatch; output:\n%s", clean)
	}
	if res.runLog != "" {
		t.Errorf("golangci-lint run executed despite the pin defect (run log: %q)", res.runLog)
	}
	if result, reason, _ := classifyLintLeg(res); result != mergeCandidateFailed || reason != "verify_failed" {
		t.Errorf("merge-candidate classification = %s / %s, want %s / verify_failed", result, reason, mergeCandidateFailed)
	}
}
