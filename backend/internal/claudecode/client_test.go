package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/reviewsandbox"
)

// The #2486 scrub-probe sentinels. scrubSentinelEnv is a secret that must be
// DROPPED by the allow-list scrub; scrubProbeEnv is a var admitted ONLY via the
// operator passthrough, which must SURVIVE. Both are set via t.Setenv in the
// scrub test and read by the env_scrub helper mode.
const (
	scrubSentinelEnv = "FISHHAWKD_DATABASE_URL"
	scrubProbeEnv    = "HELPER_ENV_PROBE"
	scrubProbeVal    = "present-via-passthrough"
)

// emitVerdictEnvelope prints a success CLI envelope whose result is a verdict
// JSON carrying the given verdict + free_form detail (the fake-claude reply the
// echo_cwd / env_scrub helper modes use).
func emitVerdictEnvelope(verdict, detail string) {
	body, _ := json.Marshal(map[string]string{"verdict": verdict, "free_form": detail})
	env, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": string(body),
	})
	fmt.Println(string(env))
}

// emitCwdEnvelope (echo_cwd mode) approves only when the child's cwd is an EMPTY
// directory distinct from the parent process cwd (HELPER_PARENT_CWD) — the C2
// proof that ungrounded claude runs from a fresh empty scratch dir, not the
// inherited process working directory.
func emitCwdEnvelope() {
	verdict, detail := "approve", ""
	wd, err := os.Getwd()
	switch {
	case err != nil:
		verdict, detail = "reject", "getwd failed: "+err.Error()
	case wd == os.Getenv("HELPER_PARENT_CWD"):
		verdict, detail = "reject", "cwd is the parent process cwd: "+wd
	default:
		if entries, rerr := os.ReadDir(wd); rerr != nil {
			verdict, detail = "reject", "readdir failed: "+rerr.Error()
		} else if len(entries) > 0 {
			verdict, detail = "reject", fmt.Sprintf("cwd %s not empty: %d entries", wd, len(entries))
		}
	}
	emitVerdictEnvelope(verdict, detail)
}

// emitScrubEnvelope (env_scrub mode) approves only when the sentinel secret was
// DROPPED from the child env and the passthrough-named probe var SURVIVED.
func emitScrubEnvelope() {
	verdict, detail := "approve", ""
	switch {
	case os.Getenv(scrubSentinelEnv) != "":
		verdict, detail = "reject", "sentinel secret reached child: "+scrubSentinelEnv
	case os.Getenv(scrubProbeEnv) != scrubProbeVal:
		verdict, detail = "reject", "passthrough probe var missing"
	}
	emitVerdictEnvelope(verdict, detail)
}

// capturingHelper re-execs the test binary as the `claude` stand-in in the given
// HELPER_MODE, capturing the ADAPTER's intended argv (binary name + flag args,
// which this builder otherwise discards for the re-exec) and the built *exec.Cmd
// so a test can read cmd.Dir after the adapter sets it. When passEnv is false the
// returned cmd's Env is left nil so the adapter's scrub path runs.
func capturingHelper(mode string, passEnv bool, argv *[]string, capturedCmd **exec.Cmd) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if argv != nil {
			*argv = append([]string{name}, args...)
		}
		c := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess")
		if passEnv {
			c.Env = append(os.Environ(), "GO_HELPER_PROCESS=1", "HELPER_MODE="+mode)
		}
		if capturedCmd != nil {
			*capturedCmd = c
		}
		return c
	}
}

// TestInvokeGrounded_ClaudeArgvAndCwd pins the GROUNDED claude posture (#2486):
// cmd.Dir is the exported tree and the argv carries --add-dir/--tools/
// --allowed-tools plus the empty-MCP-config pin (--strict-mcp-config and
// --mcp-config {"mcpServers":{}}) while NEVER carrying any --dangerously-* flag.
// The MCP pin is load-bearing: --tools bounds only the BUILT-IN toolset, so
// without these flags the operator's MCP tools (browser, Gmail, GitHub) load
// past the restriction and defeat the never-network property (#2486 fix-up).

// testHostPaths builds the injectable #2522 host seam over temp dirs. Every
// GROUNDED-path test MUST use it: without it the adapter falls through to
// reviewsandbox.DefaultHostPaths(), so the emitted deny rules would depend on
// the developer's real home layout instead of being deterministic.
func testHostPaths(t *testing.T) *reviewsandbox.HostPaths {
	t.Helper()
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatalf("mkdir codex home: %v", err)
	}
	return &reviewsandbox.HostPaths{
		HomeDir:   home,
		CodexHome: codexHome,
		TempDir:   t.TempDir(),
		Canonical: func(p string) (string, error) { return p, nil },
	}
}

func TestInvokeGrounded_ClaudeArgvAndCwd(t *testing.T) {
	treeDir := t.TempDir()
	hp := testHostPaths(t)
	var argv []string
	var cmd *exec.Cmd
	c := NewClient(testConfig())
	c.Cmd = capturingHelper("happy", true, &argv, &cmd)
	c.HostPaths = hp
	c.GOOS = "linux"

	if _, _, _, err := c.InferenceInTree(context.Background(), "review", treeDir); err != nil {
		t.Fatalf("InferenceInTree(grounded): %v", err)
	}
	if cmd.Dir != treeDir {
		t.Errorf("cmd.Dir = %q, want the exported tree %q", cmd.Dir, treeDir)
	}
	assertContainsPair(t, argv, "--add-dir", treeDir)
	assertContains(t, argv, "--tools", "Read,Grep,Glob")
	assertContains(t, argv, "--allowed-tools", "Read", "Grep", "Glob")
	assertContains(t, argv, "--strict-mcp-config")
	assertContainsPair(t, argv, "--mcp-config", `{"mcpServers":{}}`)
	assertNoDangerous(t, argv)
	// #2522: none of the flags above BOUNDS what the granted Read/Grep/Glob
	// tools may reach — an operator A/B confirmed a grounded child reads an
	// arbitrary out-of-tree file straight through this argv. The bound is
	// --disallowed-tools, and BOTH rule forms must be present: `/**` covers
	// everything beneath the denied root, the BARE form covers a plain FILE
	// sitting AT it.
	di := slices.Index(argv, "--disallowed-tools")
	if di < 0 {
		t.Fatalf("grounded argv missing --disallowed-tools: %v", argv)
	}
	homeRule := strings.TrimPrefix(hp.HomeDir, "/")
	for _, want := range []string{"Read(//" + homeRule + ")", "Read(//" + homeRule + "/**)"} {
		if !slices.Contains(argv[di:], want) {
			t.Errorf("grounded deny rules missing %q: %v", want, argv[di:])
		}
	}
	// Non-vacuity: the deny rules must not blind the reviewer against the very
	// tree --add-dir just granted.
	for _, r := range argv[di:] {
		if strings.Contains(r, treeDir) {
			t.Errorf("a deny rule names the exported tree: %q", r)
		}
	}
}

// TestInvokeUngrounded_ClaudeNoDenyRules pins the degrade side: the #2522
// grounded-only deny rules must not leak into the diff-only posture, whose argv
// stays byte-for-byte as before this change.
func TestInvokeUngrounded_ClaudeNoDenyRules(t *testing.T) {
	var argv []string
	c := NewClient(testConfig())
	c.Cmd = capturingHelper("happy", true, &argv, nil)
	c.HostPaths = testHostPaths(t)
	c.GOOS = "linux"

	if _, _, _, err := c.InferenceInTree(context.Background(), "review", ""); err != nil {
		t.Fatalf("InferenceInTree(ungrounded): %v", err)
	}
	if slices.Contains(argv, "--disallowed-tools") || slices.Contains(argv, "--add-dir") {
		t.Errorf("ungrounded argv %v must not carry the grounded-only read-bound flags", argv)
	}
}

// TestInvokeGrounded_ClaudeDenyRuleFailureFailsClosed: a deny-rule build error
// FAILS the invocation. Spawning a grounded reviewer with NO deny rules would be
// the unbounded posture this change exists to close — a degraded advisory review
// is strictly better than a silent unbounding.
func TestInvokeGrounded_ClaudeDenyRuleFailureFailsClosed(t *testing.T) {
	hp := testHostPaths(t)
	// A HOME spelling carrying a rule metacharacter: expressible as a directory,
	// NOT expressible as an unambiguous Tool(//path) rule.
	hp.HomeDir = "/Users/op (work)"
	var argv []string
	c := NewClient(testConfig())
	c.Cmd = capturingHelper("happy", true, &argv, nil)
	c.HostPaths = hp
	c.GOOS = "linux"

	_, _, _, err := c.InferenceInTree(context.Background(), "review", t.TempDir())
	if err == nil {
		t.Fatal("a deny-rule build failure must FAIL the invocation, not spawn an unbounded reviewer")
	}
	if !strings.Contains(err.Error(), "deny rules") {
		t.Errorf("err = %v, want it to name the deny-rule build", err)
	}
	if len(argv) != 0 {
		t.Errorf("no subprocess may be built on the fail-closed path; argv = %v", argv)
	}
}

// TestInvokeUngrounded_ClaudeEmptyScratchCwd pins the C2 fix: an UNGROUNDED
// claude review runs from a fresh EMPTY scratch dir, NOT the process working
// directory. The echo_cwd helper approves only if that holds.
func TestInvokeUngrounded_ClaudeEmptyScratchCwd(t *testing.T) {
	parentCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	c := NewClient(testConfig())
	c.Cmd = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess")
		cmd.Env = append(os.Environ(), "GO_HELPER_PROCESS=1", "HELPER_MODE=echo_cwd", "HELPER_PARENT_CWD="+parentCwd)
		return cmd
	}
	verdict, _, _, err := c.Inference(context.Background(), "review")
	if err != nil {
		t.Fatalf("Inference(ungrounded): %v", err)
	}
	if !strings.Contains(verdict, `"approve"`) {
		t.Errorf("ungrounded review verdict = %q, want approve (cwd must be an empty scratch dir, not the process cwd)", verdict)
	}
}

// TestInference_EnvScrubClaude proves the allow-list scrub AND the passthrough
// knob at once (#2486): a t.Setenv sentinel secret is DROPPED from the child env
// while a passthrough-named probe var SURVIVES. The env_scrub helper approves
// only when both hold. The builder leaves cmd.Env nil so the adapter's scrub
// runs; the helper control vars ride through via cfg.EnvPassthrough.
func TestInference_EnvScrubClaude(t *testing.T) {
	t.Setenv("GO_HELPER_PROCESS", "1")
	t.Setenv("HELPER_MODE", "env_scrub")
	t.Setenv(scrubProbeEnv, scrubProbeVal)
	t.Setenv(scrubSentinelEnv, "postgres://secret-should-not-leak")

	cfg := testConfig()
	cfg.EnvPassthrough = []string{"GO_HELPER_PROCESS", "HELPER_MODE", scrubProbeEnv}
	c := NewClient(cfg)
	c.Cmd = capturingHelper("env_scrub", false, nil, nil) // leave cmd.Env nil → adapter scrubs

	verdict, _, _, err := c.Inference(context.Background(), "review")
	if err != nil {
		t.Fatalf("Inference(env_scrub): %v", err)
	}
	if !strings.Contains(verdict, `"approve"`) {
		t.Errorf("env-scrub verdict = %q, want approve (sentinel must be dropped and the passthrough var kept)", verdict)
	}
}

func assertContains(t *testing.T, argv []string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !slices.Contains(argv, w) {
			t.Errorf("argv %q missing %q", argv, w)
		}
	}
}

func assertContainsPair(t *testing.T, argv []string, flag, value string) {
	t.Helper()
	i := slices.Index(argv, flag)
	if i < 0 || i+1 >= len(argv) || argv[i+1] != value {
		t.Errorf("argv %q missing %q %q pair", argv, flag, value)
	}
}

func assertNoDangerous(t *testing.T, argv []string) {
	t.Helper()
	for _, a := range argv {
		if strings.HasPrefix(a, "--dangerously") || a == "bypassPermissions" {
			t.Errorf("argv %q contains a dangerous flag %q", argv, a)
		}
	}
	_ = filepath.Separator
}
