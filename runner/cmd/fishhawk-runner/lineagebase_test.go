package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- E68.67 / #3454: standalone implement advances the lineage worktree ---

// advanceBaseFixture builds the real-git shape the base advance operates on: a
// bare origin carrying `main` at a PLAN-TIME tip, plus a clone whose HEAD is
// DETACHED at that tip (exactly what provisionLineageWorktree leaves behind for
// a lineage worktree) with a STALE refs/remotes/origin/main.
//
// The returned advance() pushes one further commit carrying advanced.txt to the
// origin's main and returns its SHA, leaving the clone's tracking ref stale —
// so the production fetchDiffBaseTip is what discovers the moved tip, as in
// production. Tests that want the already-current fast path simply never call it.
func advanceBaseFixture(t *testing.T) (repoDir, planSHA string, advance func() string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	must := func(dir string, args ...string) {
		t.Helper()
		if err := runGitErr(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	seed := t.TempDir()
	must(seed, "init", "-q")
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	must(seed, "add", "-A")
	must(seed, "commit", "-q", "-m", "base on main")
	must(seed, "branch", "-M", "main")

	bare := filepath.Join(t.TempDir(), "origin.git")
	must(seed, "init", "--bare", "-q", bare)
	// `git init --bare` seeds HEAD from init.defaultBranch, still `master` on
	// some git builds; point it at main so the clone's HEAD resolves.
	must(bare, "symbolic-ref", "HEAD", "refs/heads/main")
	must(seed, "remote", "add", "origin", bare)
	must(seed, "push", "-q", "origin", "main")

	repoDir = filepath.Join(t.TempDir(), "lineage-worktree")
	must(seed, "clone", "-q", "-b", "main", bare, repoDir)
	var err error
	if planSHA, err = runGitOut(repoDir, "rev-parse", "HEAD"); err != nil {
		t.Fatal(err)
	}
	// Detach, mirroring the lineage worktree's detached HEAD.
	must(repoDir, "checkout", "-q", "--detach", planSHA)

	advance = func() string {
		t.Helper()
		if werr := os.WriteFile(filepath.Join(seed, "advanced.txt"),
			[]byte("merged between plan and implement\n"), 0o644); werr != nil {
			t.Fatal(werr)
		}
		must(seed, "add", "-A")
		must(seed, "commit", "-q", "-m", "fix merged to main between plan and implement")
		must(seed, "push", "-q", "origin", "main")
		tip, gerr := runGitOut(seed, "rev-parse", "HEAD")
		if gerr != nil {
			t.Fatal(gerr)
		}
		return tip
	}
	return repoDir, planSHA, advance
}

// headOf reads a repo's current HEAD SHA. Every refusal case calls it AFTER the
// advance returns: the control's effect is COMMITTED STATE (HEAD position), so
// an error-identity assertion alone would pass even if the guard fired and HEAD
// moved anyway.
func headOf(t *testing.T, dir string) string {
	t.Helper()
	sha, err := runGitOut(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD in %s: %v", dir, err)
	}
	return sha
}

func withStubbedDirtyPaths(t *testing.T, paths []string, err error) {
	t.Helper()
	orig := dirtyPaths
	dirtyPaths = func(_ context.Context, _ string) ([]string, error) { return paths, err }
	t.Cleanup(func() { dirtyPaths = orig })
}

func withStubbedAncestryProbe(t *testing.T, err error) {
	t.Helper()
	orig := ancestryProbe
	ancestryProbe = func(_ context.Context, _, _, _ string) error { return err }
	t.Cleanup(func() { ancestryProbe = orig })
}

func withStubbedFetchDiffBaseTip(t *testing.T, tip string, err error) {
	t.Helper()
	orig := fetchDiffBaseTip
	fetchDiffBaseTip = func(_ context.Context, _, _, _, _ string) (string, error) { return tip, err }
	t.Cleanup(func() { fetchDiffBaseTip = orig })
}

func withStubbedCheckoutChildBase(t *testing.T, tip string, err error) {
	t.Helper()
	orig := checkoutChildBase
	checkoutChildBase = func(_ context.Context, _, _, _, _ string) (string, error) { return tip, err }
	t.Cleanup(func() { checkoutChildBase = orig })
}

// TestStandaloneBaseAdvanceEligible_Table pins the eligibility predicate: only a
// STANDALONE implement dispatch is eligible. A fix-up must stay on its PR
// branch; a decomposed child already establishes its own wave base (and the two
// blocks are mutually exclusive by construction).
func TestStandaloneBaseAdvanceEligible_Table(t *testing.T) {
	const parent = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	for _, tc := range []struct {
		name      string
		stageType string
		cfg       config
		want      bool
	}{
		{"standalone implement", "implement", config{}, true},
		{"fix-up pass", "implement", config{fixup: true}, false},
		{"decomposed child", "implement", config{decomposedFromRunID: parent}, false},
		{"decomposed child that is also a fix-up", "implement",
			config{decomposedFromRunID: parent, fixup: true}, false},
		{"plan stage", "plan", config{}, false},
		{"plan_review stage", "plan_review", config{}, false},
		{"implement_review stage", "implement_review", config{}, false},
		{"acceptance stage", "acceptance", config{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := standaloneBaseAdvanceEligible(tc.stageType, tc.cfg); got != tc.want {
				t.Errorf("standaloneBaseAdvanceEligible(%q, %+v) = %v, want %v",
					tc.stageType, tc.cfg, got, tc.want)
			}
		})
	}
}

// TestLineageBaseAdvanceFailureReason_Table pins the reason mapping: the typed
// pre-agent refusal gets its own self-diagnosing token, everything else the
// generic one.
func TestLineageBaseAdvanceFailureReason_Table(t *testing.T) {
	refusal := &lineageBaseAdvanceRefusal{kind: "dirty_worktree", fromSHA: "aaa", toSHA: "bbb"}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"typed refusal", refusal, "lineage_worktree_advance_refused"},
		{"wrapped typed refusal", errors.New("x: " + refusal.Error()), "lineage_base_advance"},
		{"errors.Join-wrapped refusal", errors.Join(errors.New("outer"), refusal), "lineage_worktree_advance_refused"},
		{"generic error", errors.New("fetch failed"), "lineage_base_advance"},
		{"nil", nil, "lineage_base_advance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lineageBaseAdvanceFailureReason(tc.err); got != tc.want {
				t.Errorf("lineageBaseAdvanceFailureReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestAdvanceLineageWorktreeToBase_EmptyBaseRef_NoOp: nothing declared to
// advance to → a silent no-op, HEAD untouched.
func TestAdvanceLineageWorktreeToBase_EmptyBaseRef_NoOp(t *testing.T) {
	repoDir, planSHA, _ := advanceBaseFixture(t)
	var log strings.Builder
	advanced, err := advanceLineageWorktreeToBase(context.Background(), repoDir, "", "", &log)
	if err != nil {
		t.Fatalf("empty baseRef must be a no-op, got %v", err)
	}
	if advanced {
		t.Error("empty baseRef reported an advance")
	}
	if log.String() != "" {
		t.Errorf("empty baseRef logged %q, want silence", log.String())
	}
	if got := headOf(t, repoDir); got != planSHA {
		t.Errorf("HEAD = %q, want the untouched plan-time SHA %q", got, planSHA)
	}
}

// TestAdvanceLineageWorktreeToBase_BaseRefAbsent_Skips: a base genuinely absent
// on the remote is the #1302 degrade contract (a never-pushed base) — skip and
// run exactly as today. Production remoteHasBranch, real ls-remote.
func TestAdvanceLineageWorktreeToBase_BaseRefAbsent_Skips(t *testing.T) {
	repoDir, planSHA, _ := advanceBaseFixture(t)
	var log strings.Builder
	advanced, err := advanceLineageWorktreeToBase(
		context.Background(), repoDir, "never-pushed-branch", "", &log)
	if err != nil {
		t.Fatalf("an absent base must skip, got %v", err)
	}
	if advanced {
		t.Error("an absent base reported an advance")
	}
	if !strings.Contains(log.String(), `"reason":"base_ref_absent"`) {
		t.Errorf("missing base_ref_absent skip:\n%s", log.String())
	}
	if got := headOf(t, repoDir); got != planSHA {
		t.Errorf("HEAD = %q, want the untouched plan-time SHA %q", got, planSHA)
	}
}

// TestAdvanceLineageWorktreeToBase_RemoteQueryError_RemoteConfigured_FailsLoud:
// a transient ls-remote failure against a CONFIGURED remote must NOT silently
// degrade to running on a base of unknown staleness — that IS the defect this
// closes. Production remoteConfigured (the fixture clone has a real origin).
func TestAdvanceLineageWorktreeToBase_RemoteQueryError_RemoteConfigured_FailsLoud(t *testing.T) {
	repoDir, planSHA, _ := advanceBaseFixture(t)
	withFakeRemoteHasBranch(t, false, errors.New("ls-remote: transient network failure"))

	var log strings.Builder
	advanced, err := advanceLineageWorktreeToBase(context.Background(), repoDir, "main", "", &log)
	if err == nil {
		t.Fatal("a remote-query failure against a configured remote must fail loud")
	}
	if advanced {
		t.Error("a failed advance reported an advance")
	}
	if got := lineageBaseAdvanceFailureReason(err); got != "lineage_base_advance" {
		t.Errorf("reason = %q, want lineage_base_advance", got)
	}
	if got := headOf(t, repoDir); got != planSHA {
		t.Errorf("HEAD = %q, want the untouched plan-time SHA %q", got, planSHA)
	}
}

// TestAdvanceLineageWorktreeToBase_RemoteQueryError_RemoteUnconfigured_Skips:
// the same query failure against a remote that is NOT configured is the
// GitHub-not-wired state (a bare local checkout with no origin) and skips like
// an absence — this is what keeps a bare local checkout running as it does today.
func TestAdvanceLineageWorktreeToBase_RemoteQueryError_RemoteUnconfigured_Skips(t *testing.T) {
	repoDir := initRepo(t) // no origin configured at all
	planSHA := headOf(t, repoDir)
	withFakeRemoteHasBranch(t, false, errors.New("ls-remote: no such remote"))

	var log strings.Builder
	advanced, err := advanceLineageWorktreeToBase(context.Background(), repoDir, "main", "", &log)
	if err != nil {
		t.Fatalf("an unconfigured remote must skip, got %v", err)
	}
	if advanced {
		t.Error("an unconfigured remote reported an advance")
	}
	if !strings.Contains(log.String(), `"reason":"remote_unconfigured"`) {
		t.Errorf("missing remote_unconfigured skip:\n%s", log.String())
	}
	if got := headOf(t, repoDir); got != planSHA {
		t.Errorf("HEAD = %q, want the untouched SHA %q", got, planSHA)
	}
}

// TestAdvanceLineageWorktreeToBase_AlreadyAtBaseTip_NoOp: the base did NOT move,
// so the fast path keeps the dispatch byte-identical to today — and, because it
// is reached BEFORE the dirty and ancestry guards, it does so regardless of tree
// state (a worktree an operator legitimately left dirty draws no refusal here).
func TestAdvanceLineageWorktreeToBase_AlreadyAtBaseTip_NoOp(t *testing.T) {
	repoDir, planSHA, _ := advanceBaseFixture(t) // advance() deliberately not called
	// A dirty tree proves the ordering: the fast path precedes guard (f).
	if err := os.WriteFile(filepath.Join(repoDir, "operator-scratch.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var log strings.Builder
	advanced, err := advanceLineageWorktreeToBase(context.Background(), repoDir, "main", "", &log)
	if err != nil {
		t.Fatalf("an already-current base must be a no-op, got %v", err)
	}
	if advanced {
		t.Error("an already-current base reported an advance")
	}
	if !strings.Contains(log.String(), `"event":"lineage_worktree_base_current"`) {
		t.Errorf("missing lineage_worktree_base_current:\n%s", log.String())
	}
	if got := headOf(t, repoDir); got != planSHA {
		t.Errorf("HEAD = %q, want the untouched plan-time SHA %q", got, planSHA)
	}
}

// TestAdvanceLineageWorktreeToBase_DirtyWorktree_Refuses: guard (f). A worktree
// carrying uncommitted changes is refused PRE-AGENT rather than force-advanced,
// in both dirty shapes. The untracked arm is the load-bearing counterfactual
// vehicle: `git checkout --detach` would happily move HEAD past a
// non-conflicting untracked file, so with the guard deleted HEAD genuinely
// MOVES — the refusal here is the only thing that stops it.
func TestAdvanceLineageWorktreeToBase_DirtyWorktree_Refuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dirty func(t *testing.T, repoDir string)
	}{
		{"untracked file", func(t *testing.T, repoDir string) {
			if err := os.WriteFile(filepath.Join(repoDir, "agent-scratch.txt"), []byte("wip\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"modified tracked file", func(t *testing.T, repoDir string) {
			if err := os.WriteFile(filepath.Join(repoDir, "base.txt"), []byte("locally edited\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoDir, planSHA, advance := advanceBaseFixture(t)
			advance()
			tc.dirty(t, repoDir)

			var log strings.Builder
			advanced, err := advanceLineageWorktreeToBase(context.Background(), repoDir, "main", "", &log)
			if advanced {
				t.Error("a dirty worktree reported an advance")
			}
			var refusal *lineageBaseAdvanceRefusal
			if !errors.As(err, &refusal) {
				t.Fatalf("error = %T (%v), want *lineageBaseAdvanceRefusal", err, err)
			}
			if refusal.kind != "dirty_worktree" {
				t.Errorf("refusal kind = %q, want dirty_worktree", refusal.kind)
			}
			if got := lineageBaseAdvanceFailureReason(err); got != "lineage_worktree_advance_refused" {
				t.Errorf("reason = %q, want lineage_worktree_advance_refused", got)
			}
			// COMMITTED STATE, not error identity: the refusal must have moved nothing.
			if got := headOf(t, repoDir); got != planSHA {
				t.Errorf("HEAD = %q, want the untouched plan-time SHA %q — the refusal moved HEAD", got, planSHA)
			}
			// The message names both SHAs and the remedy.
			for _, want := range []string{planSHA, "git worktree remove"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal message missing %q:\n%s", want, err.Error())
				}
			}
		})
	}
}

// TestAdvanceLineageWorktreeToBase_DirtyProbeError_Refuses: guard (f)'s
// fail-closed half — an unreadable dirty set cannot PROVE the tree is safe, so
// it refuses rather than moving HEAD blind.
func TestAdvanceLineageWorktreeToBase_DirtyProbeError_Refuses(t *testing.T) {
	repoDir, planSHA, advance := advanceBaseFixture(t)
	advance()
	withStubbedDirtyPaths(t, nil, errors.New("status: git unavailable"))

	advanced, err := advanceLineageWorktreeToBase(context.Background(), repoDir, "main", "", io.Discard)
	if advanced {
		t.Error("an unreadable dirty set reported an advance")
	}
	var refusal *lineageBaseAdvanceRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error = %T (%v), want *lineageBaseAdvanceRefusal", err, err)
	}
	if refusal.kind != "dirty_probe_failed" {
		t.Errorf("refusal kind = %q, want dirty_probe_failed", refusal.kind)
	}
	if got := headOf(t, repoDir); got != planSHA {
		t.Errorf("HEAD = %q, want the untouched plan-time SHA %q", got, planSHA)
	}
}

// TestAdvanceLineageWorktreeToBase_HeadNotAncestorOfBase_Refuses: guard (g).
// The worktree carries its OWN commit (seeded BY CONSTRUCTION — a commit made
// only in the worktree is definitionally not an ancestor of origin/main), so a
// fast-forward would discard it. Refuse loud; discard nothing.
func TestAdvanceLineageWorktreeToBase_HeadNotAncestorOfBase_Refuses(t *testing.T) {
	repoDir, _, advance := advanceBaseFixture(t)
	advance()
	// A run commit that exists ONLY in this worktree. Committed, so the tree is
	// CLEAN and guard (f) passes — the RED lands on the ancestry guard.
	if err := os.WriteFile(filepath.Join(repoDir, "run-work.txt"), []byte("run output\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runGitErr(repoDir, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if err := runGitErr(repoDir, "commit", "-q", "-m", "run commit local to the worktree"); err != nil {
		t.Fatal(err)
	}
	localSHA := headOf(t, repoDir)

	advanced, err := advanceLineageWorktreeToBase(context.Background(), repoDir, "main", "", io.Discard)
	if advanced {
		t.Error("a non-ancestor HEAD reported an advance")
	}
	var refusal *lineageBaseAdvanceRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error = %T (%v), want *lineageBaseAdvanceRefusal", err, err)
	}
	if refusal.kind != "head_not_ancestor" {
		t.Errorf("refusal kind = %q, want head_not_ancestor", refusal.kind)
	}
	if got := lineageBaseAdvanceFailureReason(err); got != "lineage_worktree_advance_refused" {
		t.Errorf("reason = %q, want lineage_worktree_advance_refused", got)
	}
	// COMMITTED STATE: the worktree's own commit is still HEAD, not discarded.
	if got := headOf(t, repoDir); got != localSHA {
		t.Errorf("HEAD = %q, want the preserved local run commit %q — the refusal discarded work", got, localSHA)
	}
	if _, serr := os.Stat(filepath.Join(repoDir, "run-work.txt")); serr != nil {
		t.Errorf("the worktree's own run output was discarded: %v", serr)
	}
}

// TestAdvanceLineageWorktreeToBase_AncestryProbeError_Refuses: guard (g)'s
// fail-closed half. An UNPROVABLE ancestry is not a licence to move HEAD — this
// deliberately differs from verifySeedAncestry, where a probe error degrades to
// a skip because a skip there falls through to today's behaviour rather than to
// mutating the tree.
func TestAdvanceLineageWorktreeToBase_AncestryProbeError_Refuses(t *testing.T) {
	repoDir, planSHA, advance := advanceBaseFixture(t)
	advance()
	// A plain error, NOT an *exec.ExitError with code 1 — the probe itself failed.
	withStubbedAncestryProbe(t, errors.New("merge-base: git unavailable"))

	advanced, err := advanceLineageWorktreeToBase(context.Background(), repoDir, "main", "", io.Discard)
	if advanced {
		t.Error("a failed ancestry probe reported an advance")
	}
	var refusal *lineageBaseAdvanceRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error = %T (%v), want *lineageBaseAdvanceRefusal", err, err)
	}
	if refusal.kind != "ancestry_probe_failed" {
		t.Errorf("refusal kind = %q, want ancestry_probe_failed", refusal.kind)
	}
	if got := headOf(t, repoDir); got != planSHA {
		t.Errorf("HEAD = %q, want the untouched plan-time SHA %q", got, planSHA)
	}
}

// TestAdvanceLineageWorktreeToBase_FetchTipError_FailsLoud: step (d). The base
// tip is unknowable, so the staleness is unknowable — fail loud pre-agent
// rather than run on it.
func TestAdvanceLineageWorktreeToBase_FetchTipError_FailsLoud(t *testing.T) {
	repoDir, planSHA, advance := advanceBaseFixture(t)
	advance()
	withStubbedFetchDiffBaseTip(t, "", errors.New("fetch: transient failure"))

	advanced, err := advanceLineageWorktreeToBase(context.Background(), repoDir, "main", "", io.Discard)
	if err == nil {
		t.Fatal("a fetch-tip failure must fail loud")
	}
	if advanced {
		t.Error("a failed advance reported an advance")
	}
	if got := lineageBaseAdvanceFailureReason(err); got != "lineage_base_advance" {
		t.Errorf("reason = %q, want lineage_base_advance", got)
	}
	if got := headOf(t, repoDir); got != planSHA {
		t.Errorf("HEAD = %q, want the untouched plan-time SHA %q", got, planSHA)
	}
}

// TestAdvanceLineageWorktreeToBase_CheckoutError_FailsLoud: step (h). The
// checkout is the last layer; its failure is loud, and because it is NON-force
// git itself refuses rather than overwriting.
func TestAdvanceLineageWorktreeToBase_CheckoutError_FailsLoud(t *testing.T) {
	repoDir, planSHA, advance := advanceBaseFixture(t)
	advance()
	withStubbedCheckoutChildBase(t, "", errors.New("checkout: would overwrite local modifications"))

	advanced, err := advanceLineageWorktreeToBase(context.Background(), repoDir, "main", "", io.Discard)
	if err == nil {
		t.Fatal("a checkout failure must fail loud")
	}
	if advanced {
		t.Error("a failed advance reported an advance")
	}
	if got := lineageBaseAdvanceFailureReason(err); got != "lineage_base_advance" {
		t.Errorf("reason = %q, want lineage_base_advance", got)
	}
	if got := headOf(t, repoDir); got != planSHA {
		t.Errorf("HEAD = %q, want the untouched plan-time SHA %q", got, planSHA)
	}
}

// TestAdvanceLineageWorktreeToBase_MovedBase_Advances is the happy path with
// every seam PRODUCTION: a real bare origin whose main moved between the
// worktree's provisioning and this call. HEAD must land on the advanced tip,
// the new file must be on disk, and the record must name BOTH SHAs.
func TestAdvanceLineageWorktreeToBase_MovedBase_Advances(t *testing.T) {
	repoDir, planSHA, advance := advanceBaseFixture(t)
	advancedSHA := advance()

	var log strings.Builder
	advanced, err := advanceLineageWorktreeToBase(context.Background(), repoDir, "main", "", &log)
	if err != nil {
		t.Fatalf("advance failed: %v\n%s", err, log.String())
	}
	if !advanced {
		t.Fatalf("advance reported no move on a moved base:\n%s", log.String())
	}
	if got := headOf(t, repoDir); got != advancedSHA {
		t.Errorf("HEAD = %q, want the advanced base tip %q (plan-time was %q)", got, advancedSHA, planSHA)
	}
	if _, serr := os.Stat(filepath.Join(repoDir, "advanced.txt")); serr != nil {
		t.Errorf("the advanced base's file is absent from the worktree: %v", serr)
	}
	for _, want := range []string{
		`"event":"lineage_worktree_advanced"`,
		`"base_ref":"main"`,
		`"from":"` + planSHA + `"`,
		`"to":"` + advancedSHA + `"`,
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("advance record missing %s:\n%s", want, log.String())
		}
	}
}
