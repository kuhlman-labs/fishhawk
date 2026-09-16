package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// Mid-loop scope-amendment RE-FOLD (E64 / #3434) — the third sibling of the
// #3320 settle wait (scopeamendwait.go) and the #3390 unused-grant check
// (scopeamendgrant.go).
//
// The incident (run 49f642b8, stage 04e9f7c1, amendment 8ab9767e): the
// operator APPROVED a mid-stage amendment while the verify-fix loop's fix
// agent was running. The #3390 prompt block had told the agent the grant was
// in force, the agent edited AND staged the granted file — and the next
// iteration's re-commit excluded it anyway, because runVerifyFixLoop took cfg
// BY VALUE and snapshotted scopePaths(cfg.scopeFiles) ONCE before the loop,
// and StageScoped's leading `git reset -q` unstaged the agent's own `git add`.
// The identical verify failure repeated until the budget ran out and the stage
// died category-A with the granted file left ` M` (modified, unstaged).
//
// refoldScopeAmendmentsMidLoop closes that at the iteration boundary: after a
// SUCCESSFUL fix re-invocation and before the next iteration's StageScoped it
// re-runs the same settle-wait → fold pair run() uses pre-loop, so an approval
// that landed DURING the fix invocation is folded into cfg.scopeFiles and the
// next throwaway commit carries the granted path. refreshScopeAmendments stays
// the SINGLE writer of cfg.scopeFiles — this helper adds no parallel fold.

// refoldScopeAmendmentsMidLoop settles any still-pending amendment of this
// stage (awaitPendingScopeAmendmentSettle, #3320 — the fix agent's own
// wait-poll may not have outlived an in-flight operator decision) and then
// folds every APPROVED row into cfg.scopeFiles (refreshScopeAmendments, #961).
// It returns added=true when the fold GREW cfg.scopeFiles, together with the
// settle + fold events (the fold's scope_amendments_folded policy_event among
// them — one event PER FOLD, which is why the backend unions them, see
// bundle.ExtractScopeAmendmentsFolded). On added it also writes ONE
// verify_fix_scope_refolded JSONL line naming the iteration and the paths.
//
// No-op (false, nil, no log line, no fetch) when client == nil, mcpToken == ""
// or cfg.scopeFiles is empty — the same guards refreshScopeAmendments applies
// (an empty scope is the `git add -A` fallback, which already stages
// everything). Every degrade is fail-open: a fetch error is logged by the
// callee (scope_amendment_refresh_failed) and the scope stays unchanged. The
// helper never touches res.OK.
func refoldScopeAmendmentsMidLoop(ctx context.Context, client uploadClient, cfg *config, mcpToken, stageType string, iteration int, logSink io.Writer) (bool, []agent.Event) {
	if client == nil || mcpToken == "" || len(cfg.scopeFiles) == 0 {
		return false, nil
	}
	events := awaitPendingScopeAmendmentSettle(ctx, client, *cfg, mcpToken, stageType, logSink)
	before := len(cfg.scopeFiles)
	events = append(events, refreshScopeAmendments(ctx, client, cfg, mcpToken, logSink)...)
	if len(cfg.scopeFiles) <= before {
		return false, events
	}
	added := make([]string, 0, len(cfg.scopeFiles)-before)
	for _, f := range cfg.scopeFiles[before:] {
		added = append(added, f.Path)
	}
	addedJSON, _ := json.Marshal(added)
	_, _ = fmt.Fprintf(logSink,
		`{"event":"verify_fix_scope_refolded","run_id":%q,"stage_id":%q,"iteration":%d,"added":%s}`+"\n",
		cfg.runID, cfg.stageID, iteration, addedJSON)
	return true, events
}
