package server

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	// err, when non-nil, is returned from the review call instead of a verdict —
	// the reviewer-transport-failure branch (#2486 C6 failure path). The record
	// still runs first, so treeDir is captured before the error returns.
	err error
	// blockOnCtx makes the review BLOCK until its per-invocation context is
	// cancelled (the deadline fires), then return ctx.Err() — the branch where the
	// subprocess still holds the export as its cwd when the deadline kills it, so
	// cleanup ordering (deadline kill vs RemoveAll of a live child's working dir)
	// is exercised (#2486 C6 deadline path). It takes precedence over release.
	blockOnCtx bool
}

func (g *groundingFakeReviewer) Review(ctx context.Context, promptText string) (*planreview.ReviewVerdict, string, error) {
	return g.record(ctx, "", promptText, false)
}

func (g *groundingFakeReviewer) ReviewGrounded(ctx context.Context, promptText, treeDir string) (*planreview.ReviewVerdict, string, error) {
	return g.record(ctx, treeDir, promptText, true)
}

func (g *groundingFakeReviewer) record(ctx context.Context, treeDir, promptText string, grounded bool) (*planreview.ReviewVerdict, string, error) {
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
	// Deadline branch: honour the per-invocation context so a tiny review budget
	// actually times the reviewer out while it still holds the export as its cwd.
	if g.blockOnCtx {
		<-ctx.Done()
		return nil, "", ctx.Err()
	}
	if g.release != nil {
		<-g.release
	}
	if g.err != nil {
		return nil, "", g.err
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

// TestImplementReviewGrounded_CleanupOnReviewerError is the C6 failure-branch pin
// the acceptance criterion names ("removed ... including when a reviewer errors"):
// a grounded reviewer that returns an error must still leave the export removed
// after the detached loop returns, not leaked (#2486 fix-up). The happy-path
// lifecycle test alone did not exercise this branch.
func TestImplementReviewGrounded_CleanupOnReviewerError(t *testing.T) {
	repo, headSHA := gitFixtureRepo(t)

	reviewer := &groundingFakeReviewer{
		model: "claude-sonnet-4-6",
		err:   errors.New("reviewer transport failed"),
	}
	s, _, _, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	runRow.WorkingDir = repo

	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: "foo.go", Status: policy.StatusModified}}}
	s.runImplementReviews(context.Background(), runRow.ID, implStage.ID, diff, nil, headSHA, nil)
	s.waitBackgroundReviews()

	reviewer.mu.Lock()
	treeDir := reviewer.treeDir
	reviewer.mu.Unlock()
	if treeDir == "" {
		t.Fatal("reviewer received no tree — grounded path not taken, so the error branch is untested")
	}
	if _, err := os.Stat(treeDir); !os.IsNotExist(err) {
		t.Errorf("export dir not removed after the grounded reviewer errored: %v", err)
	}
}

// TestImplementReviewGrounded_CleanupOnDeadline is the C6 failure-branch pin for
// the second required path ("...or its deadline fires"): the per-invocation
// deadline fires mid-review while the fake reviewer still holds the export as its
// cwd, and the export MUST still be removed after the detached loop returns — the
// one branch where cleanup ordering (deadline kill vs RemoveAll of a live child's
// working directory) could misbehave (#2486 fix-up). A tiny review-budget floor
// forces the deadline.
func TestImplementReviewGrounded_CleanupOnDeadline(t *testing.T) {
	repo, headSHA := gitFixtureRepo(t)

	reviewer := &groundingFakeReviewer{
		model:      "claude-sonnet-4-6",
		blockOnCtx: true,
	}
	s, _, _, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	runRow.WorkingDir = repo
	// Tiny per-invocation floor (PerKB/Cap zeroed) so the applied review budget is
	// ~20ms and the reviewer's blocked-on-ctx wait times out mid-review.
	s.cfg.ReviewBudget = planreview.ReviewBudget{Floor: 20 * time.Millisecond}

	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: "foo.go", Status: policy.StatusModified}}}
	s.runImplementReviews(context.Background(), runRow.ID, implStage.ID, diff, nil, headSHA, nil)
	s.waitBackgroundReviews()

	reviewer.mu.Lock()
	treeDir := reviewer.treeDir
	grounded := reviewer.reviewGroundedHit
	reviewer.mu.Unlock()
	if treeDir == "" || !grounded {
		t.Fatalf("reviewer was not grounded (treeDir=%q grounded=%v) — the deadline branch is untested", treeDir, grounded)
	}
	if _, err := os.Stat(treeDir); !os.IsNotExist(err) {
		t.Errorf("export dir not removed after the reviewer's deadline fired: %v", err)
	}
}

// specImplementMixedAdvisoryReviewers declares a heterogeneous implement-stage
// panel (one anthropic reviewer + one codex reviewer, advisory) so a MIXED
// capability panel can be driven through the real runImplementReviews loop.
var specImplementMixedAdvisoryReviewers = []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: anthropic
              model: claude-opus-4-8
            - provider: codex
              model: gpt-5.5
          human: 1
`)

// TestImplementReviewGrounded_MixedPanelUngroundedThroughLoop drives the
// mixed-capability rule through the REAL loop (the plan's verification section
// promised it, beyond the unit-level TestAllInvocationsGrounded): a panel with
// one grounding-capable reviewer and one that is not must run EVERYONE ungrounded
// — no tree is exported and both get the diff-only prompt — so the prompt never
// claims a tree half the panel cannot read (#2486 fix-up).
func TestImplementReviewGrounded_MixedPanelUngroundedThroughLoop(t *testing.T) {
	repo, headSHA := gitFixtureRepo(t)

	capable := &groundingFakeReviewer{model: "claude-opus-4-8"}
	incapable := &plainFakeReviewer{}
	set := fakeReviewerSet{
		def: capable,
		providers: map[string]PlanReviewer{
			"anthropic": capable,
			"codex":     incapable,
		},
	}
	s, _, _, _, runRow, implStage := newImplementReviewServerWithSet(t, set, specImplementMixedAdvisoryReviewers)
	runRow.WorkingDir = repo

	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: "foo.go", Status: policy.StatusModified}}}
	s.runImplementReviews(context.Background(), runRow.ID, implStage.ID, diff, nil, headSHA, nil)
	s.waitBackgroundReviews()

	capable.mu.Lock()
	grounded := capable.reviewGroundedHit
	treeDir := capable.treeDir
	capablePrompt := capable.prompt
	capable.mu.Unlock()
	if grounded || treeDir != "" {
		t.Errorf("mixed panel must run the capable reviewer ungrounded: grounded=%v treeDir=%q", grounded, treeDir)
	}
	if !strings.Contains(capablePrompt, "DIFF-ONLY") {
		t.Errorf("mixed panel prompt must be diff-only:\n%s", capablePrompt)
	}
	if !strings.Contains(incapable.prompt, "DIFF-ONLY") {
		t.Errorf("mixed panel prompt for the incapable reviewer must be diff-only:\n%s", incapable.prompt)
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
