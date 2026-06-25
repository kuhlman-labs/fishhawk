package main

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// guardHostDispatch is the pre-dispatch runner_kind mismatch guardrail (#1355,
// the ADR-045 guardrail variant #1346 deferred). The host-dispatch MCP verbs
// (fishhawk_dispatch_stage / fishhawk_run_stage) ALWAYS spawn a LOCAL runner —
// they are local-only by design (ADR-024 Q5). So a host dispatch against a run
// whose runner_kind has already been LOCKED to github_actions is a guaranteed
// channel mismatch: #1348 would FLAG it via the runner_kind_mismatch audit, but
// only AFTER the runner ran. This rejects the conflict BEFORE the runner spawns.
//
// It engages ONLY on the LOCKED state (RunnerKindResolved == true). The three
// invariants the operator's approval conditions make mandatory:
//
//   - (1) Un-resolved run (RunnerKindResolved == false): ALLOW. The run has not
//     yet been locked, and #1346's first-dispatch auto-resolve still needs to
//     fire — a premature block here would re-create the #1344 local-loop wedge.
//   - (2) GetRun error: FAIL OPEN — return a warning and a nil error, never
//     strand a legitimate local dispatch. This is defense-in-depth layered on
//     top of #1348's post-execution audit backstop, not a correctness gate.
//   - (3) Locked to github_actions: BLOCK with an actionable error naming the
//     locked kind and the corrective action (BRAND_FOUNDATIONS §5).
//
// Returns (warnings, err): a non-nil err is the pre-execution block (the caller
// must NOT spawn); warnings are merged into the caller's warnings slice.
func (r *runResolver) guardHostDispatch(ctx context.Context, runUUID uuid.UUID) ([]string, error) {
	got, err := r.api.GetRun(ctx, runUUID)
	if err != nil {
		// (2) Fail OPEN: a transient backend read must not strand a legitimate
		// local dispatch. The #1348 post-execution runner_kind_mismatch audit
		// remains the backstop.
		return []string{fmt.Sprintf(
			"host-dispatch runner_kind guard skipped (could not read run %s: %v); proceeding — the post-execution runner_kind_mismatch audit remains the backstop",
			runUUID, err)}, nil
	}
	// (1) Not yet locked: allow so the first dispatch auto-resolves runner_kind
	// to local (#1346 decision-1). Blocking here would re-create the #1344 wedge.
	if !got.RunnerKindResolved {
		return nil, nil
	}
	// (3) Locked to github_actions: a host dispatch (always local) conflicts.
	if got.RunnerKind == "github_actions" {
		return nil, fmt.Errorf(
			"run %s is locked to runner_kind=github_actions, but fishhawk_dispatch_stage / fishhawk_run_stage spawn a LOCAL runner — dispatching here would conflict with the run's resolved execution channel. To run this stage on GitHub Actions, dispatch it through the Actions workflow channel; to drive it locally, start a NEW run with runner_kind=local",
			runUUID)
	}
	// Locked to local (or any non-actions kind): allow — the host dispatch
	// matches the run's resolved local channel.
	return nil, nil
}
