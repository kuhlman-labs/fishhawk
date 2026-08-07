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
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/claudecode"
	"github.com/kuhlman-labs/fishhawk/backend/internal/codex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

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
	_, _, _, _ = cx.InferenceInTree(context.Background(), p, treeDir)
	if cxCmd.Dir != treeDir {
		t.Errorf("codex grounded cmd.Dir = %q, want the tree %q", cxCmd.Dir, treeDir)
	}
	if i := slices.Index(cxArgv, "--sandbox"); i < 0 || i+1 >= len(cxArgv) || cxArgv[i+1] != "read-only" {
		t.Errorf("codex grounded argv missing --sandbox read-only: %v", cxArgv)
	}

	// claudecode: cmd.Dir == tree, --add-dir <tree> present.
	var clArgv []string
	var clCmd *exec.Cmd
	cl := claudecode.NewClient(claudecode.Config{Binary: "claude", Model: "m"})
	cl.Cmd = capture(&clArgv, &clCmd)
	_, _, _, _ = cl.InferenceInTree(context.Background(), p, treeDir)
	if clCmd.Dir != treeDir {
		t.Errorf("claude grounded cmd.Dir = %q, want the tree %q", clCmd.Dir, treeDir)
	}
	if i := slices.Index(clArgv, "--add-dir"); i < 0 || i+1 >= len(clArgv) || clArgv[i+1] != treeDir {
		t.Errorf("claude grounded argv missing --add-dir <tree>: %v", clArgv)
	}
}

// TestContract_UngroundedPromptMatchesAdapterPosture pins the degrade side: an
// ungrounded prompt is diff-only and NEITHER adapter grounds against the tree
// (no grounding flags; cwd is not the tree).
func TestContract_UngroundedPromptMatchesAdapterPosture(t *testing.T) {
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
	_, _, _, _ = cx.InferenceInTree(context.Background(), p, "")
	if slices.Contains(cxArgv, "--sandbox") {
		t.Errorf("ungrounded codex argv must not carry --sandbox: %v", cxArgv)
	}
	if cxCmd.Dir == treeDir {
		t.Errorf("ungrounded codex cmd.Dir must not be the tree")
	}

	var clArgv []string
	var clCmd *exec.Cmd
	cl := claudecode.NewClient(claudecode.Config{Binary: "claude", Model: "m"})
	cl.Cmd = capture(&clArgv, &clCmd)
	_, _, _, _ = cl.InferenceInTree(context.Background(), p, "")
	if slices.Contains(clArgv, "--add-dir") {
		t.Errorf("ungrounded claude argv must not carry --add-dir: %v", clArgv)
	}
	// C2: the ungrounded claude cwd is a fresh empty scratch dir, not empty string
	// and not the tree.
	if clCmd.Dir == "" || clCmd.Dir == treeDir {
		t.Errorf("ungrounded claude cmd.Dir = %q, want a non-tree scratch dir", clCmd.Dir)
	}
}
