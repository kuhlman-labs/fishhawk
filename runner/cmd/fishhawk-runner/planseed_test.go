package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// (R2) TestCapturePlanSeed_FreshLineageWorktreeIsSeedNotOperatorHead is the
// runner-side #4436 proof: the operator checkout's HEAD is BEHIND the
// origin/main tip, a fresh lineage worktree is provisioned, and the plan seed
// captured in it is the tip — equal to lineage_worktree_seeded.seed_sha and
// different from the operator HEAD the plan review used to export.
func TestCapturePlanSeed_FreshLineageWorktreeIsSeedNotOperatorHead(t *testing.T) {
	ctx := context.Background()
	operator, headSHA, tipSHA := initRepoBehindOrigin(t)
	var provLog bytes.Buffer
	wt, err := provisionLineageWorktree(ctx, operator, "ps000000", "main", &provLog)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	var seededSHA string
	for _, line := range strings.Split(provLog.String(), "\n") {
		var ev struct {
			Event   string `json:"event"`
			SeedSHA string `json:"seed_sha"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Event == "lineage_worktree_seeded" {
			seededSHA = ev.SeedSHA
		}
	}
	if seededSHA == "" {
		t.Fatalf("fixture precondition: no lineage_worktree_seeded line:\n%s", provLog.String())
	}

	var log bytes.Buffer
	got := capturePlanSeed(ctx, wt, &log)
	if got != tipSHA {
		t.Errorf("capturePlanSeed = %q, want the origin tip %q", got, tipSHA)
	}
	if got != seededSHA {
		t.Errorf("capturePlanSeed = %q, want lineage_worktree_seeded.seed_sha %q", got, seededSHA)
	}
	if got == headSHA {
		t.Errorf("capturePlanSeed returned the stale operator HEAD %q", headSHA)
	}
	if want := `{"event":"plan_seed_resolved","seed_sha":"` + tipSHA + `"}`; !strings.Contains(log.String(), want) {
		t.Errorf("missing %s in:\n%s", want, log.String())
	}
}

// (R3) TestCapturePlanSeed_NotARepoReturnsEmpty: an unresolvable HEAD returns
// "" (the upload then sends no header and the backend fails closed) and logs
// plan_seed_unresolved — it never fails the stage.
func TestCapturePlanSeed_NotARepoReturnsEmpty(t *testing.T) {
	var log bytes.Buffer
	if got := capturePlanSeed(context.Background(), t.TempDir(), &log); got != "" {
		t.Errorf("capturePlanSeed on a non-repo = %q, want \"\"", got)
	}
	if !strings.Contains(log.String(), `"event":"plan_seed_unresolved"`) {
		t.Errorf("missing plan_seed_unresolved in:\n%s", log.String())
	}
	if strings.Contains(log.String(), `"event":"plan_seed_resolved"`) {
		t.Errorf("an unresolved seed must not log plan_seed_resolved:\n%s", log.String())
	}
}

// (R4) TestRun_UploadPlan_ShipsWorkingTreeHeadAsSeed drives run() end to end
// for a plan stage over --working-dir and asserts the plan upload carries the
// working tree's HEAD as SeedSHA. Discarding the capture leaves it "" → RED.
func TestRun_UploadPlan_ShipsWorkingTreeHeadAsSeed(t *testing.T) {
	_, legacy := withPlanArtifactDirs(t)
	keyed := planArtifactPath(planOutTestRunID, planOutTestStageID)
	promptPath := filepath.Join(t.TempDir(), "prompt.txt")
	mustWriteFile(t, promptPath, "Write your plan JSON to "+keyed+" and stop.")
	repo := initRepo(t)
	headSHA, err := runGitOut(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true, StructuredOutput: []byte(planWithTicket(t, 4436))}})
	fu := newFakeUploader(t)
	withFakeUploader(t, fu)

	var stderr strings.Builder
	args := append(planStageRunArgs(promptPath, legacy), "--working-dir", repo)
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fu.gotPlanArgs == nil {
		t.Fatalf("ShipPlan not called:\n%s", stderr.String())
	}
	if fu.gotPlanArgs.SeedSHA != headSHA {
		t.Errorf("ShipPlanArgs.SeedSHA = %q, want the working tree HEAD %q\n%s", fu.gotPlanArgs.SeedSHA, headSHA, stderr.String())
	}
}
