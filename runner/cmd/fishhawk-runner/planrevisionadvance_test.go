package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// --- #4867: a revise_plan re-dispatch advances the reused lineage worktree ---
//
// A plan-gate revise re-dispatches the plan stage onto the lineage worktree
// its first pass provisioned. provisionLineageWorktree's reuse path never moves
// HEAD, so before #4867 the revised planner — and plan_generated.seed_sha, and
// the plan review's exported tree — sat on the admission commit even after the
// declared base moved. The backend now serves plan_revision on a plan dispatch
// of a run holding a durable plan_revised constraint, and run() fast-forwards
// the worktree with advanceLineageWorktreeToBase BEFORE capturePlanSeed.

// planRevisionSyntheticToken is the synthetic forge token every fixture below
// mints (approval condition C3). It is NOT a real credential; the tests assert
// it reaches the advance's fetch auth seam and is never echoed into a log line,
// an error detail or the agent subprocess environment.
const planRevisionSyntheticToken = "ghs_SYNTHETIC_plan_revision_4867_not_a_real_token"

// planRevisionGoldenPrompt decodes the SHARED wire golden
// testdata/wire/plan_revision_prompt.json into the runner's FetchedPrompt, so a
// json-tag drift on upload.FetchedPrompt.PlanRevision decodes false and the
// cross-boundary test goes RED end to end (counterfactual (g)).
func planRevisionGoldenPrompt(t *testing.T) *upload.FetchedPrompt {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "plan_revision_prompt.json"))
	if err != nil {
		t.Fatalf("read shared golden: %v", err)
	}
	var p upload.FetchedPrompt
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode shared golden: %v", err)
	}
	if p.StageType != "plan" {
		t.Fatalf("golden stage_type = %q, want plan", p.StageType)
	}
	p.StageID = planOutTestStageID
	p.Prompt = "Write your plan JSON to " + planArtifactPath(planOutTestRunID, planOutTestStageID) + " and stop."
	p.PromptHash = "h"
	return &p
}

// planFirstPassPrompt is the same plan dispatch with NO plan_revision key — the
// first-pass shape the backend serves before any revise.
func planFirstPassPrompt() *upload.FetchedPrompt {
	return &upload.FetchedPrompt{
		StageID:    planOutTestStageID,
		StageType:  "plan",
		Prompt:     "Write your plan JSON to " + planArtifactPath(planOutTestRunID, planOutTestStageID) + " and stop.",
		PromptHash: "h",
	}
}

// provisionPlanLineage performs the FIRST plan pass's exact provisioning call
// against the operator clone and returns the lineage worktree path.
func provisionPlanLineage(t *testing.T, operator, wantSHA string) string {
	t.Helper()
	wt, err := provisionLineageWorktree(context.Background(), operator,
		lineageRoot(planOutTestRunID, "", false), "main", io.Discard)
	if err != nil {
		t.Fatalf("first-pass plan provision: %v", err)
	}
	if got, herr := runGitOut(wt, "rev-parse", "HEAD"); herr != nil {
		t.Fatal(herr)
	} else if got != wantSHA {
		t.Fatalf("lineage worktree seeded at %q, want %q", got, wantSHA)
	}
	return wt
}

// shippedSeed returns the plan upload's SeedSHA, or "<ShipPlan not called>".
func shippedSeed(fu *fakeUploader) string {
	if fu.gotPlanArgs == nil {
		return "<ShipPlan not called>"
	}
	return fu.gotPlanArgs.SeedSHA
}

// planRevisionInvocation records what the plan agent saw at invoke.
type planRevisionInvocation struct {
	calls       int
	head        string
	sawAdvanced bool
	inv         *agent.Invocation
}

// recordRemoteHasBranchToken wraps the CURRENT remoteHasBranch seam (production
// unless a test stubbed it first) and records the auth token the advance hands
// its ls-remote guard, so custody is asserted positively, not only by absence.
func recordRemoteHasBranchToken(t *testing.T) *string {
	t.Helper()
	orig := remoteHasBranch
	var got string
	remoteHasBranch = func(ctx context.Context, repoDir, remote, branch, token string) (bool, error) {
		got = token
		return orig(ctx, repoDir, remote, branch, token)
	}
	t.Cleanup(func() { remoteHasBranch = orig })
	return &got
}

// runPlanRevisionStage drives run() for a plan stage over --fetch-prompt with
// the given served prompt against the operator clone, with the synthetic token
// as the only mintable credential.
func runPlanRevisionStage(t *testing.T, operator string, prompt *upload.FetchedPrompt) (int, string, *fakeUploader, *planRevisionInvocation) {
	t.Helper()
	_, legacy := withPlanArtifactDirs(t)
	rec := &planRevisionInvocation{}
	withFakeInvoker(t, &fakeInvoker{
		canned: agent.Result{OK: true, StructuredOutput: []byte(planWithTicket(t, 4867))},
		onInvoke: func(_ int, inv agent.Invocation) {
			rec.calls++
			i := inv
			rec.inv = &i
			rec.head, _ = runGitOut(inv.WorkingDir, "rev-parse", "HEAD")
			if _, serr := os.Stat(filepath.Join(inv.WorkingDir, "advanced.txt")); serr == nil {
				rec.sawAdvanced = true
			}
		},
	})
	fu := newFakeUploader(t)
	fu.promptResp = prompt
	fu.instTokenSeq = []string{planRevisionSyntheticToken}
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", planOutTestRunID,
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "plan",
		"--stage-id", planOutTestStageID,
		"--working-dir", operator,
		"--fetch-prompt", "--upload-trace",
		"--plan-out", legacy,
	}, &stderr)
	return got, stderr.String(), fu, rec
}

// assertSyntheticTokenNotEchoed is the C3 non-echo assertion: the synthetic
// token appears in no runner log line or error detail, and never reaches the
// agent subprocess environment (its Env overlay, its BaseEnv seed, or the
// runner's own process env that a nil BaseEnv inherits).
func assertSyntheticTokenNotEchoed(t *testing.T, out string, rec *planRevisionInvocation) {
	t.Helper()
	if strings.Contains(out, planRevisionSyntheticToken) {
		t.Errorf("the synthetic forge token was echoed into the runner output:\n%s", out)
	}
	for _, kv := range os.Environ() {
		if strings.Contains(kv, planRevisionSyntheticToken) {
			t.Errorf("the synthetic forge token leaked into the runner process env: %q", kv)
		}
	}
	if rec == nil || rec.inv == nil {
		return
	}
	for k, v := range rec.inv.Env {
		if strings.Contains(v, planRevisionSyntheticToken) {
			t.Errorf("the synthetic forge token reached the agent env overlay %s", k)
		}
	}
	for _, kv := range rec.inv.BaseEnv {
		if strings.Contains(kv, planRevisionSyntheticToken) {
			t.Errorf("the synthetic forge token reached the agent BaseEnv: %q", kv)
		}
	}
}

// TestRun_PlanRevision_AdvancesLineageWorktreeToMovedBase_CrossBoundary is the
// LOAD-BEARING done-means test for #4867, with REAL git throughout: a real bare
// origin, a real operator clone, a real first-pass provisionLineageWorktree at
// the plan-time SHA, a real commit merged into origin/main between the first
// pass and the revise, and production remoteHasBranch / fetchDiffBaseTip /
// dirtyPaths / ancestryProbe / checkoutChildBase seams. The served prompt is
// decoded from the shared golden, so the backend's tag, the runner's tag and
// the predicate are crossed in one path.
func TestRun_PlanRevision_AdvancesLineageWorktreeToMovedBase_CrossBoundary(t *testing.T) {
	operator, planSHA, advanceOrigin := standaloneMovedBaseRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	provisionPlanLineage(t, operator, planSHA)
	advancedSHA := advanceOrigin()
	gotToken := recordRemoteHasBranchToken(t)

	got, out, fu, rec := runPlanRevisionStage(t, operator, planRevisionGoldenPrompt(t))
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, out)
	}

	// The agent read the advanced tree.
	if rec.calls != 1 {
		t.Fatalf("agent invoked %d times, want 1:\n%s", rec.calls, out)
	}
	if rec.head != advancedSHA {
		t.Errorf("worktree HEAD at invoke = %q, want the advanced base tip %q (plan-time sha %q)", rec.head, advancedSHA, planSHA)
	}
	if !rec.sawAdvanced {
		t.Errorf("advanced.txt absent from the worktree at invoke — the revised planner read the admission commit:\n%s", out)
	}

	// The shipped seed is the advanced tip, so plan_generated.seed_sha and the
	// plan review's exported tree follow.
	if fu.gotPlanArgs == nil {
		t.Fatalf("ShipPlan not called:\n%s", out)
	}
	if fu.gotPlanArgs.SeedSHA != advancedSHA {
		t.Errorf("ShipPlanArgs.SeedSHA = %q, want the advanced tip %q (stale plan sha %q)", fu.gotPlanArgs.SeedSHA, advancedSHA, planSHA)
	}

	// The structured record: reused, then advanced {from: plan sha, to: tip}.
	reusedAt := strings.Index(out, `"event":"lineage_worktree_reused"`)
	advancedAt := strings.Index(out, `"event":"lineage_worktree_advanced"`)
	if reusedAt < 0 || advancedAt < 0 || reusedAt > advancedAt {
		t.Errorf("want lineage_worktree_reused then lineage_worktree_advanced (reused@%d advanced@%d):\n%s", reusedAt, advancedAt, out)
	}
	for _, want := range []string{`"from":"` + planSHA + `"`, `"to":"` + advancedSHA + `"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in run output:\n%s", want, out)
		}
	}
	// No pin: a plan stage cuts no commit.
	if strings.Contains(out, `"event":"base_pinned"`) {
		t.Errorf("a plan stage logged base_pinned — the plan advance must not pin:\n%s", out)
	}

	// The operator checkout never moved and stays clean.
	if head := gitPorcelainHead(t, operator); head != planSHA {
		t.Errorf("operator HEAD moved: %q, want %q", head, planSHA)
	}
	if status := gitPorcelain(t, operator); status != "" {
		t.Errorf("operator git status not clean after the run:\n%s", status)
	}

	// C3 token custody: the minted token reached the fetch auth seam and was
	// echoed nowhere.
	if *gotToken != planRevisionSyntheticToken {
		t.Errorf("remoteHasBranch token = %q, want the minted synthetic token", *gotToken)
	}
	assertSyntheticTokenNotEchoed(t, out, rec)
}

// TestRun_PlanFirstPass_ReusedWorktree_DoesNotAdvance pins that a first-pass
// plan dispatch (no plan_revision key) on the SAME moved origin keeps today's
// behaviour: HEAD and SeedSHA stay at the plan SHA and nothing advances.
func TestRun_PlanFirstPass_ReusedWorktree_DoesNotAdvance(t *testing.T) {
	operator, planSHA, advanceOrigin := standaloneMovedBaseRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	wt := provisionPlanLineage(t, operator, planSHA)
	advanceOrigin()

	got, out, fu, rec := runPlanRevisionStage(t, operator, planFirstPassPrompt())
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, out)
	}
	if rec.head != planSHA {
		t.Errorf("first-pass worktree HEAD at invoke = %q, want the unmoved plan sha %q", rec.head, planSHA)
	}
	if seed := shippedSeed(fu); seed != planSHA {
		t.Errorf("first-pass SeedSHA = %q, want %q", seed, planSHA)
	}
	for _, ev := range []string{"lineage_worktree_advanced", "lineage_worktree_base_current", "lineage_worktree_advance_skipped"} {
		if strings.Contains(out, `"event":"`+ev+`"`) {
			t.Errorf("a first-pass plan reached the advance (%s):\n%s", ev, out)
		}
	}
	if head, _ := runGitOut(wt, "rev-parse", "HEAD"); head != planSHA {
		t.Errorf("worktree HEAD after run = %q, want %q", head, planSHA)
	}
}

// TestRun_PlanRevision_DirtyWorktree_RefusesPreAgent: a scratch file the first
// plan agent left in the reused worktree draws a pre-agent dirty_worktree
// refusal once the base moved; the agent is never invoked, ShipPlan never
// runs and HEAD stays put.
func TestRun_PlanRevision_DirtyWorktree_RefusesPreAgent(t *testing.T) {
	operator, planSHA, advanceOrigin := standaloneMovedBaseRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	wt := provisionPlanLineage(t, operator, planSHA)
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("left by the first plan agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	advanceOrigin()

	got, out, fu, rec := runPlanRevisionStage(t, operator, planRevisionGoldenPrompt(t))
	// Zero invocations FIRST, so a deleted `return exitFailure` (counterfactual
	// (d)) reddens on the agent running, not only on the exit code.
	if rec.calls != 0 {
		t.Errorf("agent invoked %d times on a refused advance, want 0:\n%s", rec.calls, out)
	}
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure:\n%s", got, out)
	}
	if fu.gotPlanArgs != nil {
		t.Errorf("ShipPlan called on a refused advance:\n%s", out)
	}
	for _, want := range []string{`"reason":"lineage_worktree_advance_refused"`, "(dirty_worktree)", "scratch.txt"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in run output:\n%s", want, out)
		}
	}
	if head, _ := runGitOut(wt, "rev-parse", "HEAD"); head != planSHA {
		t.Errorf("worktree HEAD after refusal = %q, want the untouched plan sha %q", head, planSHA)
	}
	assertSyntheticTokenNotEchoed(t, out, rec)
}

// TestRun_PlanRevision_HeadNotAncestor_RefusesPreAgent: a reused worktree that
// carries its own commit is not an ancestor of the moved base tip, so the
// advance refuses head_not_ancestor and HEAD keeps that commit.
func TestRun_PlanRevision_HeadNotAncestor_RefusesPreAgent(t *testing.T) {
	operator, planSHA, advanceOrigin := standaloneMovedBaseRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	wt := provisionPlanLineage(t, operator, planSHA)
	if err := os.WriteFile(filepath.Join(wt, "own.txt"), []byte("the worktree's own commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-q", "-m", "own commit"},
	} {
		if err := runGitErr(wt, args...); err != nil {
			t.Fatal(err)
		}
	}
	ownSHA, err := runGitOut(wt, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	advanceOrigin()

	got, out, fu, rec := runPlanRevisionStage(t, operator, planRevisionGoldenPrompt(t))
	// Zero invocations FIRST, so a deleted `return exitFailure` (counterfactual
	// (d)) reddens on the agent running, not only on the exit code.
	if rec.calls != 0 {
		t.Errorf("agent invoked %d times on a refused advance, want 0:\n%s", rec.calls, out)
	}
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure:\n%s", got, out)
	}
	if fu.gotPlanArgs != nil {
		t.Errorf("ShipPlan called on a refused advance:\n%s", out)
	}
	for _, want := range []string{`"reason":"lineage_worktree_advance_refused"`, "(head_not_ancestor)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in run output:\n%s", want, out)
		}
	}
	if head, _ := runGitOut(wt, "rev-parse", "HEAD"); head != ownSHA {
		t.Errorf("worktree HEAD after refusal = %q, want its own commit %q", head, ownSHA)
	}
	assertSyntheticTokenNotEchoed(t, out, rec)
}

// TestRun_PlanRevision_RemoteQueryError_FailsLoudPreAgent: an ls-remote error
// against a CONFIGURED origin fails loud with lineage_base_advance before the
// agent runs (silently planning on a base of unknown staleness is the defect).
func TestRun_PlanRevision_RemoteQueryError_FailsLoudPreAgent(t *testing.T) {
	operator, planSHA, _ := standaloneMovedBaseRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	wt := provisionPlanLineage(t, operator, planSHA)
	withFakeRemoteHasBranch(t, false, errors.New("ls-remote: connection reset"))

	got, out, fu, rec := runPlanRevisionStage(t, operator, planRevisionGoldenPrompt(t))
	if rec.calls != 0 {
		t.Errorf("agent invoked %d times on a remote-query failure, want 0:\n%s", rec.calls, out)
	}
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure:\n%s", got, out)
	}
	if fu.gotPlanArgs != nil {
		t.Errorf("ShipPlan called on a remote-query failure:\n%s", out)
	}
	if !strings.Contains(out, `"reason":"lineage_base_advance"`) || !strings.Contains(out, "connection reset") {
		t.Errorf("want runner_failed lineage_base_advance naming the query error:\n%s", out)
	}
	if head, _ := runGitOut(wt, "rev-parse", "HEAD"); head != planSHA {
		t.Errorf("worktree HEAD after failure = %q, want %q", head, planSHA)
	}
	assertSyntheticTokenNotEchoed(t, out, rec)
}

// TestRun_PlanRevision_BaseAbsent_Skips: a declared base absent on the remote
// takes the graceful skip; the agent runs and the seed is the unmoved HEAD.
func TestRun_PlanRevision_BaseAbsent_Skips(t *testing.T) {
	operator, planSHA, _ := standaloneMovedBaseRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	provisionPlanLineage(t, operator, planSHA)
	withFakeRemoteHasBranch(t, false, nil)

	got, out, fu, rec := runPlanRevisionStage(t, operator, planRevisionGoldenPrompt(t))
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, out)
	}
	if !strings.Contains(out, `"event":"lineage_worktree_advance_skipped","reason":"base_ref_absent"`) {
		t.Errorf("missing the base_ref_absent skip:\n%s", out)
	}
	if rec.calls != 1 || rec.head != planSHA {
		t.Errorf("agent calls=%d head=%q, want 1 at %q", rec.calls, rec.head, planSHA)
	}
	if seed := shippedSeed(fu); seed != planSHA {
		t.Errorf("SeedSHA = %q, want the unmoved HEAD %q", seed, planSHA)
	}
	assertSyntheticTokenNotEchoed(t, out, rec)
}

// TestRun_PlanRevision_BaseAlreadyCurrent_NoMove: a reused worktree already at
// the base tip takes the base_current fast path — no refusal even with an
// untracked file, because the dirty guard is reached only when the base moved.
func TestRun_PlanRevision_BaseAlreadyCurrent_NoMove(t *testing.T) {
	operator, planSHA, _ := standaloneMovedBaseRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	wt := provisionPlanLineage(t, operator, planSHA)
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, out, fu, rec := runPlanRevisionStage(t, operator, planRevisionGoldenPrompt(t))
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, out)
	}
	if !strings.Contains(out, `"event":"lineage_worktree_base_current","base_ref":"main","head_sha":"`+planSHA+`"`) {
		t.Errorf("missing lineage_worktree_base_current at the plan sha:\n%s", out)
	}
	if strings.Contains(out, `"event":"lineage_worktree_advanced"`) || strings.Contains(out, "lineage_worktree_advance_refused") {
		t.Errorf("an already-current base moved or refused:\n%s", out)
	}
	if seed := shippedSeed(fu); rec.calls != 1 || seed != planSHA {
		t.Errorf("agent calls=%d seed=%q, want 1 call seeded at %q", rec.calls, seed, planSHA)
	}
	assertSyntheticTokenNotEchoed(t, out, rec)
}

// TestRun_PlanRevision_FreshProvision_SeedsAtTip: when the lineage worktree is
// absent at the revise (swept, or removed per the refusal remedy) the fresh
// provision seeds at the current tip and the advance finds it current.
func TestRun_PlanRevision_FreshProvision_SeedsAtTip(t *testing.T) {
	operator, planSHA, advanceOrigin := standaloneMovedBaseRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	advancedSHA := advanceOrigin()

	got, out, fu, rec := runPlanRevisionStage(t, operator, planRevisionGoldenPrompt(t))
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, out)
	}
	seededAt := strings.Index(out, `"event":"lineage_worktree_seeded"`)
	currentAt := strings.Index(out, `"event":"lineage_worktree_base_current"`)
	if seededAt < 0 || currentAt < 0 || seededAt > currentAt {
		t.Errorf("want lineage_worktree_seeded then lineage_worktree_base_current (seeded@%d current@%d):\n%s", seededAt, currentAt, out)
	}
	if seed := shippedSeed(fu); rec.head != advancedSHA || seed != advancedSHA {
		t.Errorf("head=%q seed=%q, want both at the tip %q (plan sha %q)", rec.head, seed, advancedSHA, planSHA)
	}
	assertSyntheticTokenNotEchoed(t, out, rec)
}

// TestRun_PlanRevision_DecomposedChild_DoesNotAdvance isolates the
// decomposedFromRunID arm: a child plan prompt carrying plan_revision with
// seams that report a moved base. The wave-base block is implement-only, so
// with the arm absent checkoutChildBase would run once and the advanced event
// would be logged.
func TestRun_PlanRevision_DecomposedChild_DoesNotAdvance(t *testing.T) {
	repo := initRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
	checkouts := withMovedBaseSeams(t, "moved-tip-sha")

	prompt := planRevisionGoldenPrompt(t)
	prompt.DecomposedFromRunID = "99999999-8888-7777-6666-555555555555"
	got, out, _, rec := runPlanRevisionStage(t, repo, prompt)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, out)
	}
	if *checkouts != 0 {
		t.Errorf("checkoutChildBase called %d times for a decomposed child plan, want 0:\n%s", *checkouts, out)
	}
	if strings.Contains(out, `"event":"lineage_worktree_advanced"`) {
		t.Errorf("a decomposed child plan advanced its shared lineage worktree:\n%s", out)
	}
	if rec.calls != 1 {
		t.Errorf("agent invoked %d times, want 1", rec.calls)
	}
}
