package reviewsandbox_test

// This external test package is the ONLY place prompt, codex, and claudecode all
// meet (importing all three from a non-test package would cycle). It is the
// anti-divergence pin the #2486 done-means requires: whenever the review prompt
// CLAIMS repository reads, each adapter's REAL argv builder must actually set the
// working directory to the exported tree and grant read+search — so deleting
// --add-dir, the codex sandbox flag, or the cwd fails a test here rather than
// silently restoring the aspirational-prompt state the issue exists to close.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/claudecode"
	"github.com/kuhlman-labs/fishhawk/backend/internal/codex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/reviewsandbox"
)

// contractHostPaths builds the injectable host seam BOTH adapters take for the
// #2522 read bounds. It is mandatory here, not a convenience: without it these
// contract tests would drive DefaultHostPaths(), synthesize a confined CODEX_HOME
// inside the developer's or CI runner's REAL ~/.codex (MkdirAll-creating it if
// absent), copy their live auth.json into it, and write back to it on cleanup.
// No unit or contract test may touch a real credential.
func contractHostPaths(t *testing.T) *reviewsandbox.HostPaths {
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

const contractCommit = "abcdef0123456789abcdef0123456789abcdef01"

// capture returns a Cmd builder that records the adapter's intended argv and the
// built *exec.Cmd (so cmd.Dir, set by the adapter after the builder returns, is
// readable), pointing at a non-existent binary so the adapter fails fast without
// running anything real. The argv and cwd are captured regardless.
func capture(argv *[]string, cmd **exec.Cmd) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		*argv = args
		c := exec.CommandContext(ctx, "/nonexistent-reviewsandbox-contract-probe")
		*cmd = c
		return c
	}
}

func contractPlan() *plan.Plan {
	return &plan.Plan{
		PlanVersion: "standard_v1",
		TicketReference: plan.TicketReference{
			Type: plan.TicketTypeGitHubIssue,
			URL:  "https://github.com/kuhlman-labs/example/issues/42",
			ID:   "kuhlman-labs/example#42",
		},
		GeneratedBy: plan.GeneratedBy{
			Agent:     "test",
			Model:     "m",
			Timestamp: time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
		},
		Summary: "s",
		Scope:   plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/a.go", Operation: plan.FileOpModify}}},
		Approach: []plan.ApproachStep{
			{Step: 1, Description: "d"},
		},
		Verification: plan.Verification{TestStrategy: "t", RollbackPlan: "r"},
	}
}

// TestContract_GroundedPromptMatchesAdapterGrant pins that a grounded prompt's
// repo-read CLAIM is backed by an actual grant in BOTH adapters' real argv.
func TestContract_GroundedPromptMatchesAdapterGrant(t *testing.T) {
	hp := contractHostPaths(t)
	treeDir := t.TempDir()
	trig := prompt.Trigger{
		Repo:             "kuhlman-labs/example",
		ApprovedPlan:     contractPlan(),
		Diff:             "- A pkg/a.go\n",
		ReviewTreeCommit: contractCommit,
	}
	p, err := prompt.Build("implement_review", trig)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(p, "REPOSITORY ACCESS") || !strings.Contains(p, "reading and searching files within the provided working directory") {
		t.Fatalf("grounded prompt does not claim repo reads:\n%s", p)
	}

	// codex: cmd.Dir == tree, --sandbox read-only present.
	var cxArgv []string
	var cxCmd *exec.Cmd
	cx := codex.NewClient(codex.Config{Binary: "codex"})
	cx.Cmd = capture(&cxArgv, &cxCmd)
	cx.HostPaths = hp
	cx.GOOS = "linux"
	_, _, _, _ = cx.InferenceInTree(context.Background(), p, treeDir)
	if cxCmd.Dir != treeDir {
		t.Errorf("codex grounded cmd.Dir = %q, want the tree %q", cxCmd.Dir, treeDir)
	}
	if i := slices.Index(cxArgv, "--sandbox"); i < 0 || i+1 >= len(cxArgv) || cxArgv[i+1] != "read-only" {
		t.Errorf("codex grounded argv missing --sandbox read-only: %v", cxArgv)
	}
	// #2522: --sandbox read-only means "write nowhere, read ANYWHERE" — the
	// read bound is the `confined` permission profile, selected by --profile.
	// Without this flag the synthesized CODEX_HOME is inert and the reviewer is
	// unconfined with no error, so the pairing is pinned here at the boundary.
	if i := slices.Index(cxArgv, "--profile"); i < 0 || i+1 >= len(cxArgv) || cxArgv[i+1] != reviewsandbox.ConfinedProfileName {
		t.Errorf("codex grounded argv missing --profile %s: %v", reviewsandbox.ConfinedProfileName, cxArgv)
	}
	if !slices.Contains(cxArgv, "--ignore-rules") {
		t.Errorf("codex grounded argv missing --ignore-rules: %v", cxArgv)
	}

	// claudecode: cmd.Dir == tree, --add-dir <tree> present.
	var clArgv []string
	var clCmd *exec.Cmd
	cl := claudecode.NewClient(claudecode.Config{Binary: "claude", Model: "m"})
	cl.Cmd = capture(&clArgv, &clCmd)
	cl.HostPaths = hp
	cl.GOOS = "linux"
	_, _, _, _ = cl.InferenceInTree(context.Background(), p, treeDir)
	if clCmd.Dir != treeDir {
		t.Errorf("claude grounded cmd.Dir = %q, want the tree %q", clCmd.Dir, treeDir)
	}
	if i := slices.Index(clArgv, "--add-dir"); i < 0 || i+1 >= len(clArgv) || clArgv[i+1] != treeDir {
		t.Errorf("claude grounded argv missing --add-dir <tree>: %v", clArgv)
	}
	// The read+search grant is the substance of the grounded contract, not just
	// the directory: --add-dir alone lets a reviewer SEE the tree, but the
	// restricted toolset (--tools) plus the pre-approval (--allowed-tools) is what
	// actually lets print mode read/search it without stalling on an unanswerable
	// permission prompt. Assert BOTH so deleting the tool grant in the adapter
	// fails this cross-boundary contract rather than leaving the prompt's
	// read-and-search claim silently unbacked (#2486 test_vacuity fix-up).
	if i := slices.Index(clArgv, "--tools"); i < 0 || i+1 >= len(clArgv) || clArgv[i+1] != "Read,Grep,Glob" {
		t.Errorf("claude grounded argv missing --tools Read,Grep,Glob: %v", clArgv)
	}
	if i := slices.Index(clArgv, "--allowed-tools"); i < 0 || i+3 >= len(clArgv) ||
		clArgv[i+1] != "Read" || clArgv[i+2] != "Grep" || clArgv[i+3] != "Glob" {
		t.Errorf("claude grounded argv missing --allowed-tools Read Grep Glob: %v", clArgv)
	}
	// The built-in --tools restriction does NOT bound MCP tools: those load from
	// the operator's config and survive it (verified live — a grounded child
	// enumerated browser/Gmail/GitHub MCP tools). So the grounded argv must also
	// pin an EMPTY MCP server set — --strict-mcp-config (make --mcp-config the
	// sole source) plus --mcp-config {"mcpServers":{}} (zero servers) — or the
	// never-network property this whole change rests on is defeated. Assert BOTH
	// next to the --tools/--allowed-tools grant so deleting either fails this
	// cross-boundary contract (#2486 MCP fix-up).
	if !slices.Contains(clArgv, "--strict-mcp-config") {
		t.Errorf("claude grounded argv missing --strict-mcp-config: %v", clArgv)
	}
	if i := slices.Index(clArgv, "--mcp-config"); i < 0 || i+1 >= len(clArgv) || clArgv[i+1] != `{"mcpServers":{}}` {
		t.Errorf("claude grounded argv missing --mcp-config {\"mcpServers\":{}}: %v", clArgv)
	}
	// #2522: --add-dir plus the tool grant bounds nothing — an operator A/B
	// confirmed a grounded child reads an arbitrary out-of-tree file straight
	// through the argv above. The read bound is --disallowed-tools, and BOTH
	// rule forms must be present: `/**` covers everything beneath the home root,
	// the bare form covers a plain FILE sitting AT it.
	di := slices.Index(clArgv, "--disallowed-tools")
	if di < 0 {
		t.Fatalf("claude grounded argv missing --disallowed-tools: %v", clArgv)
	}
	homeRule := strings.TrimPrefix(hp.HomeDir, "/")
	for _, want := range []string{"Read(//" + homeRule + ")", "Read(//" + homeRule + "/**)"} {
		if !slices.Contains(clArgv[di:], want) {
			t.Errorf("claude grounded deny rules missing %q: %v", want, clArgv[di:])
		}
	}
	// Non-vacuity at the boundary: the deny rules must not blind the reviewer
	// against the very tree --add-dir just granted.
	for _, r := range clArgv[di:] {
		if strings.Contains(r, treeDir) {
			t.Errorf("a deny rule names the exported tree: %q", r)
		}
	}
}

// TestContract_UngroundedPromptMatchesAdapterPosture pins the degrade side: an
// ungrounded prompt is diff-only and NEITHER adapter grounds against the tree
// (no grounding flags; cwd is not the tree).
func TestContract_UngroundedPromptMatchesAdapterPosture(t *testing.T) {
	hp := contractHostPaths(t)
	treeDir := t.TempDir()
	trig := prompt.Trigger{
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: contractPlan(),
		Diff:         "- A pkg/a.go\n",
	}
	p, err := prompt.Build("implement_review", trig)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(p, "DIFF-ONLY") {
		t.Fatalf("ungrounded prompt is not diff-only:\n%s", p)
	}

	var cxArgv []string
	var cxCmd *exec.Cmd
	cx := codex.NewClient(codex.Config{Binary: "codex"})
	cx.Cmd = capture(&cxArgv, &cxCmd)
	cx.HostPaths = hp
	cx.GOOS = "linux"
	_, _, _, _ = cx.InferenceInTree(context.Background(), p, "")
	if slices.Contains(cxArgv, "--sandbox") {
		t.Errorf("ungrounded codex argv must not carry --sandbox: %v", cxArgv)
	}
	// The #2522 grounded-only read bounds must not leak into the diff-only
	// posture: its argv and env stay byte-for-byte as before this change.
	if slices.Contains(cxArgv, "--profile") {
		t.Errorf("ungrounded codex argv must not carry --profile: %v", cxArgv)
	}
	if cxCmd.Dir == treeDir {
		t.Errorf("ungrounded codex cmd.Dir must not be the tree")
	}

	var clArgv []string
	var clCmd *exec.Cmd
	cl := claudecode.NewClient(claudecode.Config{Binary: "claude", Model: "m"})
	cl.Cmd = capture(&clArgv, &clCmd)
	cl.HostPaths = hp
	cl.GOOS = "linux"
	_, _, _, _ = cl.InferenceInTree(context.Background(), p, "")
	if slices.Contains(clArgv, "--add-dir") {
		t.Errorf("ungrounded claude argv must not carry --add-dir: %v", clArgv)
	}
	if slices.Contains(clArgv, "--disallowed-tools") {
		t.Errorf("ungrounded claude argv must not carry --disallowed-tools: %v", clArgv)
	}
	// The empty-MCP pin is NOT grounded-only — it is posture-independent (#2524),
	// so the diff-only argv must carry both flags, mirroring the grounded side.
	// --add-dir (asserted absent above) remains the grounded-only flag.
	if !slices.Contains(clArgv, "--strict-mcp-config") {
		t.Errorf("ungrounded claude argv missing --strict-mcp-config: %v", clArgv)
	}
	if i := slices.Index(clArgv, "--mcp-config"); i < 0 || i+1 >= len(clArgv) || clArgv[i+1] != `{"mcpServers":{}}` {
		t.Errorf("ungrounded claude argv missing --mcp-config {\"mcpServers\":{}}: %v", clArgv)
	}
	// C2: the ungrounded claude cwd is a fresh empty scratch dir, not empty string
	// and not the tree.
	if clCmd.Dir == "" || clCmd.Dir == treeDir {
		t.Errorf("ungrounded claude cmd.Dir = %q, want a non-tree scratch dir", clCmd.Dir)
	}
}
