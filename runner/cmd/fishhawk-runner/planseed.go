package main

import (
	"context"
	"fmt"
	"io"
)

// capturePlanSeed resolves the PLAN SEED (#4436): the commit the plan agent's
// working tree is checked out at when the agent starts. uploadPlan ships it in
// upload.PlanSeedHeader, the backend records it as plan_generated.seed_sha, and
// the plan review exports its read-only tree there instead of at the shared
// checkout's HEAD — which can sit behind the planner's tree after any unpulled
// merge.
//
// On a FRESH lineage provision the worktree is `git worktree add --detach
// <path> <seedSHA>` and no pre-agent step moves HEAD for a plan stage, so this
// equals lineage_worktree_seeded.seed_sha. On a reused worktree or the
// --prompt-file path it is still exactly the tree the planner reads.
//
// repoDir "" means "." (the agent's default CWD). It logs plan_seed_resolved,
// or on error plan_seed_unresolved and returns "" — it never fails the stage;
// the backend then fails CLOSED (no tree, diff-only plan review).
func capturePlanSeed(ctx context.Context, repoDir string, logSink io.Writer) string {
	if repoDir == "" {
		repoDir = "."
	}
	sha, err := resolveHead(ctx, repoDir)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, `{"event":"plan_seed_unresolved","detail":%q}`+"\n", err.Error())
		return ""
	}
	_, _ = fmt.Fprintf(logSink, `{"event":"plan_seed_resolved","seed_sha":%q}`+"\n", sha)
	return sha
}
