package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// groundingFakeReviewer implements BOTH Review and the optional groundedReviewer
// capability (#2486), recording the treeDir + prompt it received and whether the
// export dir existed at review time (the C6 survives-during-review probe). When
// started/release are non-nil it blocks so a test can inspect the export mid-review.
type groundingFakeReviewer struct {
	mu                 sync.Mutex
	treeDir            string
	prompt             string
	dirExistedAtReview bool
	reviewGroundedHit  bool
	started            chan struct{}
	release            chan struct{}
	verdict            *planreview.ReviewVerdict
	model              string
}

func (g *groundingFakeReviewer) Review(_ context.Context, promptText string) (*planreview.ReviewVerdict, string, error) {
	return g.record("", promptText, false)
}

func (g *groundingFakeReviewer) ReviewGrounded(_ context.Context, promptText, treeDir string) (*planreview.ReviewVerdict, string, error) {
	return g.record(treeDir, promptText, true)
}

func (g *groundingFakeReviewer) record(treeDir, promptText string, grounded bool) (*planreview.ReviewVerdict, string, error) {
	g.mu.Lock()
	g.treeDir = treeDir
	g.prompt = promptText
	g.reviewGroundedHit = grounded
	if treeDir != "" {
		_, err := os.Stat(treeDir)
		g.dirExistedAtReview = err == nil
	}
	g.mu.Unlock()
	if g.started != nil {
		close(g.started)
	}
	if g.release != nil {
		<-g.release
	}
	v := g.verdict
	if v == nil {
		v = &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}
	}
	return v, g.model, nil
}

// plainFakeReviewer implements ONLY Review (no grounding capability) — the
// anthropic-SDK analog that forces a mixed panel ungrounded.
type plainFakeReviewer struct {
	prompt string
}

func (p *plainFakeReviewer) Review(_ context.Context, promptText string) (*planreview.ReviewVerdict, string, error) {
	p.prompt = promptText
	return &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, "", nil
}

func TestAllInvocationsGrounded(t *testing.T) {
	grounded := reviewerInvocation{reviewer: &groundingFakeReviewer{}}
	plain := reviewerInvocation{reviewer: &plainFakeReviewer{}}
	failed := reviewerInvocation{reviewer: &groundingFakeReviewer{}, resolveErr: context.Canceled}

	if !allInvocationsGrounded([]reviewerInvocation{grounded, grounded}) {
		t.Error("all-capable panel must be grounded")
	}
	if allInvocationsGrounded([]reviewerInvocation{grounded, plain}) {
		t.Error("mixed panel must NOT be grounded")
	}
	if allInvocationsGrounded(nil) {
		t.Error("empty panel must NOT be grounded")
	}
	if allInvocationsGrounded([]reviewerInvocation{grounded, failed}) {
		t.Error("panel with an unresolved reviewer must NOT be grounded")
	}
}

func TestInvokeReview_Routing(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})

	gr := &groundingFakeReviewer{}
	if _, _, err := s.invokeReview(context.Background(), reviewerInvocation{reviewer: gr}, "p", "/tree"); err != nil {
		t.Fatalf("invokeReview: %v", err)
	}
	if !gr.reviewGroundedHit || gr.treeDir != "/tree" {
		t.Errorf("grounded route not taken: hit=%v treeDir=%q", gr.reviewGroundedHit, gr.treeDir)
	}

	gr2 := &groundingFakeReviewer{}
	if _, _, err := s.invokeReview(context.Background(), reviewerInvocation{reviewer: gr2}, "p", ""); err != nil {
		t.Fatalf("invokeReview: %v", err)
	}
	if gr2.reviewGroundedHit {
		t.Errorf("empty treeDir must route to diff-only Review, not ReviewGrounded")
	}

	// A non-capable reviewer with a treeDir still falls back to Review.
	pr := &plainFakeReviewer{}
	if _, _, err := s.invokeReview(context.Background(), reviewerInvocation{reviewer: pr}, "p", "/tree"); err != nil {
		t.Fatalf("invokeReview: %v", err)
	}
}

// gitFixtureRepo builds a one-commit git repo and returns its dir and HEAD SHA.
func gitFixtureRepo(t *testing.T) (dir, headSHA string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir = t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "foo.go"), []byte("package foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "foo.go")
	run("commit", "-q", "-m", "init")
	return dir, run("rev-parse", "HEAD")
}

func TestExportReviewTree_Degrades(t *testing.T) {
	repo, _ := gitFixtureRepo(t)

	t.Run("kill switch", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", ReviewGroundingDisabled: true})
		rr := &run.Run{ID: uuid.New(), WorkingDir: repo}
		dir, commit, _, cleanup := s.exportReviewTree(context.Background(), rr, "HEAD")
		defer cleanup()
		if dir != "" || commit != "" {
			t.Errorf("kill switch must degrade to empty; got dir=%q commit=%q", dir, commit)
		}
	})

	t.Run("empty working dir", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		rr := &run.Run{ID: uuid.New(), WorkingDir: ""}
		dir, _, _, cleanup := s.exportReviewTree(context.Background(), rr, "HEAD")
		defer cleanup()
		if dir != "" {
			t.Errorf("empty working dir must degrade; got dir=%q", dir)
		}
	})

	t.Run("bad ref", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		rr := &run.Run{ID: uuid.New(), WorkingDir: repo}
		dir, _, _, cleanup := s.exportReviewTree(context.Background(), rr, "no-such-ref")
		defer cleanup()
		if dir != "" {
			t.Errorf("unresolvable ref must degrade; got dir=%q", dir)
		}
	})

	t.Run("happy resolves and cleans up", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		rr := &run.Run{ID: uuid.New(), WorkingDir: repo}
		dir, commit, _, cleanup := s.exportReviewTree(context.Background(), rr, "HEAD")
		if dir == "" || len(commit) < 40 {
			t.Fatalf("happy export failed: dir=%q commit=%q", dir, commit)
		}
		if _, err := os.Stat(filepath.Join(dir, "foo.go")); err != nil {
			t.Errorf("exported tree missing foo.go: %v", err)
		}
		cleanup()
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("cleanup did not remove the export dir: %v", err)
		}
	})
}

// TestImplementReviewGrounded_AdvisoryDetachedLifecycle is the C6 pin plus the
// grounded happy path: an advisory (detached) implement review runs grounded,
// the reviewer receives the export tree and a prompt naming the commit, the
// export SURVIVES for the detached review's lifetime, and it is removed after the
// loop returns.
func TestImplementReviewGrounded_AdvisoryDetachedLifecycle(t *testing.T) {
	repo, headSHA := gitFixtureRepo(t)

	reviewer := &groundingFakeReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	s, _, _, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	runRow.WorkingDir = repo

	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: "foo.go", Status: policy.StatusModified}}}
	if s.runImplementReviews(context.Background(), runRow.ID, implStage.ID, diff, nil, headSHA, nil) {
		t.Fatal("advisory runImplementReviews returned true")
	}

	<-reviewer.started
	// Mid-review: the export MUST still exist (C6 survives the detached lifetime).
	reviewer.mu.Lock()
	treeDir := reviewer.treeDir
	existed := reviewer.dirExistedAtReview
	prompt := reviewer.prompt
	reviewer.mu.Unlock()
	if treeDir == "" {
		t.Fatal("reviewer received no tree — grounded path not taken")
	}
	if !existed {
		t.Error("export dir did not exist at review time")
	}
	if _, err := os.Stat(treeDir); err != nil {
		t.Errorf("export dir removed before the detached review released: %v", err)
	}
	if !strings.Contains(prompt, "REPOSITORY ACCESS") || !strings.Contains(prompt, headSHA[:12]) {
		t.Errorf("grounded prompt does not name the commit %s:\n%s", headSHA[:12], prompt)
	}

	close(reviewer.release)
	s.waitBackgroundReviews()

	// After the loop returns the export MUST be removed (C6 removed-afterward).
	if _, err := os.Stat(treeDir); !os.IsNotExist(err) {
		t.Errorf("export dir not removed after the detached review returned: %v", err)
	}
}

// TestImplementReviewGrounded_KillSwitchDegrades proves the kill switch forces an
// ungrounded, diff-only review even with a grounding-capable reviewer and a
// working dir.
func TestImplementReviewGrounded_KillSwitchDegrades(t *testing.T) {
	repo, headSHA := gitFixtureRepo(t)

	reviewer := &groundingFakeReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	s, _, _, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	s.cfg.ReviewGroundingDisabled = true
	runRow.WorkingDir = repo

	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: "foo.go", Status: policy.StatusModified}}}
	s.runImplementReviews(context.Background(), runRow.ID, implStage.ID, diff, nil, headSHA, nil)
	s.waitBackgroundReviews()

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if reviewer.reviewGroundedHit || reviewer.treeDir != "" {
		t.Errorf("kill switch must degrade to ungrounded; grounded=%v treeDir=%q", reviewer.reviewGroundedHit, reviewer.treeDir)
	}
	if !strings.Contains(reviewer.prompt, "DIFF-ONLY") {
		t.Errorf("kill-switch review prompt must be diff-only:\n%s", reviewer.prompt)
	}
}

// TestPlanReviewGrounded_AdvisoryExportsAndCleansUp covers the plan.go grounding
// path (#2486): an advisory plan review over a run with a working dir exports the
// resolved HEAD, the grounding-capable reviewer receives the tree and a prompt
// naming the commit, and the export is removed after the detached loop returns.
func TestPlanReviewGrounded_AdvisoryExportsAndCleansUp(t *testing.T) {
	repo, _ := gitFixtureRepo(t)
	runID, stageID := uuid.New(), uuid.New()
	reviewer := &groundingFakeReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	s, _, _, _, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, specAdvisoryReviewers)
	rr.getRuns[runID].WorkingDir = repo

	if s.runPlanReviews(context.Background(), runID, stageID, validPlanBytes(t), nil, nil, nil, nil, nil) {
		t.Fatal("advisory runPlanReviews returned true")
	}
	s.waitBackgroundReviews()

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if !reviewer.reviewGroundedHit || reviewer.treeDir == "" {
		t.Fatalf("plan review not grounded: hit=%v treeDir=%q", reviewer.reviewGroundedHit, reviewer.treeDir)
	}
	if !strings.Contains(reviewer.prompt, "REPOSITORY ACCESS") {
		t.Errorf("grounded plan-review prompt missing REPOSITORY ACCESS:\n%s", reviewer.prompt)
	}
	if _, err := os.Stat(reviewer.treeDir); !os.IsNotExist(err) {
		t.Errorf("plan-review export not removed after the detached loop returned: %v", err)
	}
}
