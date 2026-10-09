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
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
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

// gitOriginOnlyFixture builds a local clone whose bare origin holds a commit the
// clone does NOT have — the decomposed parent's consolidated-review shape, where
// the server pushed the head to origin and the run's working dir never fetched
// it (#4066). Built WITHOUT a `git push` into the bare origin (AGENTS.md #3503):
// the origin FETCHES the commit from a scratch source repo instead. It returns
// the local clone and the origin-only commit, precondition-asserted absent
// locally.
func gitOriginOnlyFixture(t *testing.T) (local, originOnlySHA string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	git := func(dir string, args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(dir string, args ...string) string {
		out, err := git(dir, args...)
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	src, origin := filepath.Join(root, "src"), filepath.Join(root, "origin.git")
	local = filepath.Join(root, "local")
	must(root, "init", "-q", src)
	if err := os.WriteFile(filepath.Join(src, "foo.go"), []byte("package foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	must(src, "add", "foo.go")
	must(src, "commit", "-q", "-m", "c1")
	must(root, "clone", "-q", "--bare", src, origin)
	must(root, "clone", "-q", origin, local)
	if err := os.WriteFile(filepath.Join(src, "consolidated.go"), []byte("package foo\n\nconst Consolidated = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	must(src, "add", "consolidated.go")
	must(src, "commit", "-q", "-m", "c2")
	originOnlySHA = must(src, "rev-parse", "HEAD")
	must(origin, "fetch", "-q", "--no-auto-maintenance", src, "HEAD:refs/heads/fishhawk/run-x-consolidated")
	if _, err := git(local, "cat-file", "-e", originOnlySHA); err == nil {
		t.Fatalf("fixture precondition: %s must be absent from the local clone", originOnlySHA)
	}
	return local, originOnlySHA
}

// TestExportReviewTree_Degrades pins every degrade AND the prompt reason it
// returns (#4066), so the review prompt distinguishes a switched-off
// deployment from an enabled one whose tree could not be provided.
func TestExportReviewTree_Degrades(t *testing.T) {
	repo, _ := gitFixtureRepo(t)

	cases := []struct {
		name       string
		disabled   bool
		workingDir string
		ref        string
		want       string
	}{
		{"kill switch", true, repo, "HEAD", prompt.ReviewUngroundedDisabled},
		{"empty working dir", false, "", "HEAD", prompt.ReviewUngroundedNoWorkingDir},
		{"empty ref", false, repo, "", prompt.ReviewUngroundedNoRef},
		{"non-SHA bad ref", false, repo, "no-such-ref", prompt.ReviewUngroundedRefUnavailable},
		// The fixture repo has no origin remote, so fetch-on-miss fails.
		{"SHA not on origin", false, repo, "0123456789abcdef0123456789abcdef01234567", prompt.ReviewUngroundedRefUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{Addr: "127.0.0.1:0", ReviewGroundingDisabled: tc.disabled})
			rr := &run.Run{ID: uuid.New(), WorkingDir: tc.workingDir}
			dir, commit, _, cleanup, reason := s.exportReviewTree(context.Background(), rr, tc.ref)
			defer cleanup()
			if dir != "" || commit != "" {
				t.Errorf("must degrade to empty; got dir=%q commit=%q", dir, commit)
			}
			if reason != tc.want {
				t.Errorf("reason = %q, want %q", reason, tc.want)
			}
		})
	}

	// Not parallel: t.Setenv. git absent is a start failure, not a ref miss.
	t.Run("git absent", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		s := New(Config{Addr: "127.0.0.1:0"})
		rr := &run.Run{ID: uuid.New(), WorkingDir: repo}
		dir, _, _, cleanup, reason := s.exportReviewTree(context.Background(), rr, "HEAD")
		defer cleanup()
		if dir != "" || reason != prompt.ReviewUngroundedExportFailed {
			t.Errorf("git absent: dir=%q reason=%q, want empty dir and %q", dir, reason, prompt.ReviewUngroundedExportFailed)
		}
	})

	t.Run("happy resolves and cleans up", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		rr := &run.Run{ID: uuid.New(), WorkingDir: repo}
		dir, commit, _, cleanup, reason := s.exportReviewTree(context.Background(), rr, "HEAD")
		if dir == "" || len(commit) < 40 || reason != "" {
			t.Fatalf("happy export failed: dir=%q commit=%q reason=%q", dir, commit, reason)
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

// TestGroundReview_StampsReason pins groundReview's decision: a grounded round
// stamps the tree commit and no reason; a panel that cannot ground stamps
// reviewer_cannot_ground — or disabled when the kill switch is on, preserving
// the shipping default's switch-off render even for a mixed panel — and
// attempts no export.
func TestGroundReview_StampsReason(t *testing.T) {
	repo, headSHA := gitFixtureRepo(t)
	grounded := reviewerInvocation{reviewer: &groundingFakeReviewer{}}
	plain := reviewerInvocation{reviewer: &plainFakeReviewer{}}
	rr := &run.Run{ID: uuid.New(), WorkingDir: repo}

	for _, tc := range []struct {
		name       string
		disabled   bool
		invs       []reviewerInvocation
		wantTree   bool
		wantReason string
	}{
		{"grounded", false, []reviewerInvocation{grounded}, true, ""},
		{"mixed panel", false, []reviewerInvocation{grounded, plain}, false, prompt.ReviewUngroundedReviewerCannotGround},
		{"mixed panel, kill switch", true, []reviewerInvocation{grounded, plain}, false, prompt.ReviewUngroundedDisabled},
		{"grounded panel, kill switch", true, []reviewerInvocation{grounded}, false, prompt.ReviewUngroundedDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{Addr: "127.0.0.1:0", ReviewGroundingDisabled: tc.disabled})
			var trig prompt.Trigger
			dir, cleanup := s.groundReview(context.Background(), rr, headSHA, tc.invs, &trig)
			defer cleanup()
			if (dir != "") != tc.wantTree {
				t.Errorf("treeDir = %q, wantTree %v", dir, tc.wantTree)
			}
			if trig.ReviewUngroundedReason != tc.wantReason {
				t.Errorf("ReviewUngroundedReason = %q, want %q", trig.ReviewUngroundedReason, tc.wantReason)
			}
			if tc.wantTree && trig.ReviewTreeCommit != headSHA {
				t.Errorf("ReviewTreeCommit = %q, want %q", trig.ReviewTreeCommit, headSHA)
			}
			if !tc.wantTree && trig.ReviewTreeCommit != "" {
				t.Errorf("ungrounded round stamped ReviewTreeCommit %q", trig.ReviewTreeCommit)
			}
		})
	}
}

// TestImplementReviewGrounded_FetchesOriginOnlyHead is the #4066 cross-boundary
// pin (reviewsandbox fetch-on-miss → server grounding decision → prompt render
// → reviewer): an implement review whose headSHA exists only in origin — the
// consolidated-review shape — runs GROUNDED, and the prompt names that commit.
// Deleting the fetch reddens it end to end.
func TestImplementReviewGrounded_FetchesOriginOnlyHead(t *testing.T) {
	local, headSHA := gitOriginOnlyFixture(t)

	reviewer := &groundingFakeReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	s, _, _, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	runRow.WorkingDir = local

	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: "consolidated.go", Status: policy.StatusAdded}}}
	s.runImplementReviews(context.Background(), runRow.ID, implStage.ID, diff, nil, headSHA, nil)
	s.waitBackgroundReviews()

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if !reviewer.reviewGroundedHit || reviewer.treeDir == "" || !reviewer.dirExistedAtReview {
		t.Fatalf("origin-only head not grounded: hit=%v treeDir=%q existed=%v\n%s",
			reviewer.reviewGroundedHit, reviewer.treeDir, reviewer.dirExistedAtReview, reviewer.prompt)
	}
	if !strings.Contains(reviewer.prompt, "TRACKED files exported at commit "+headSHA[:12]) {
		t.Errorf("grounded prompt does not name the origin-only commit %s:\n%s", headSHA[:12], reviewer.prompt)
	}
}

// TestImplementReviewGrounded_UnavailableHeadNamesCause: a headSHA absent from
// origin too degrades to DIFF-ONLY with the ref_unavailable cause, and the
// prompt does NOT tell the operator to set FISHHAWKD_REVIEW_GROUNDING anywhere
// (repo access AND criterion 10) — grounding is enabled here. With the reason
// unset, the zero value renders the switch text naming the env var.
func TestImplementReviewGrounded_UnavailableHeadNamesCause(t *testing.T) {
	local, _ := gitOriginOnlyFixture(t)
	const nowhere = "0123456789abcdef0123456789abcdef01234567"

	reviewer := &groundingFakeReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	s, _, _, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	runRow.WorkingDir = local

	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: "foo.go", Status: policy.StatusModified}}}
	s.runImplementReviews(context.Background(), runRow.ID, implStage.ID, diff, nil, nowhere, nil)
	s.waitBackgroundReviews()

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if reviewer.reviewGroundedHit || reviewer.treeDir != "" {
		t.Fatalf("unavailable head must degrade: grounded=%v treeDir=%q", reviewer.reviewGroundedHit, reviewer.treeDir)
	}
	for _, w := range []string{"DIFF-ONLY", "IS ENABLED", "could not be fetched from origin"} {
		if !strings.Contains(reviewer.prompt, w) {
			t.Errorf("degraded prompt missing %q:\n%s", w, reviewer.prompt)
		}
	}
	if strings.Contains(reviewer.prompt, "FISHHAWKD_REVIEW_GROUNDING") {
		t.Errorf("enabled-but-unavailable prompt must NOT name FISHHAWKD_REVIEW_GROUNDING:\n%s", reviewer.prompt)
	}
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
	// The switched-off posture still renders the switch text (#4066).
	if !strings.Contains(reviewer.prompt, "This is a DEPLOYMENT setting") || !strings.Contains(reviewer.prompt, "FISHHAWKD_REVIEW_GROUNDING=true") {
		t.Errorf("kill-switch review prompt must name the switch:\n%s", reviewer.prompt)
	}
	if strings.Contains(reviewer.prompt, "IS ENABLED") {
		t.Errorf("kill-switch review prompt must not claim grounding is enabled:\n%s", reviewer.prompt)
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
	// Grounding is enabled here; the panel is what cannot ground (#4066).
	for name, p := range map[string]string{"capable": capablePrompt, "incapable": incapable.prompt} {
		if !strings.Contains(p, "cannot read an exported tree") {
			t.Errorf("%s reviewer's mixed-panel prompt must name reviewer_cannot_ground:\n%s", name, p)
		}
		if strings.Contains(p, "FISHHAWKD_REVIEW_GROUNDING") {
			t.Errorf("%s reviewer's mixed-panel prompt must NOT name FISHHAWKD_REVIEW_GROUNDING:\n%s", name, p)
		}
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
