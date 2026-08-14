package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/runnerbackend"
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
	// (3) Locked to a KNOWN, non-host-dispatched kind (github_actions, gitlab_ci):
	// a host dispatch (always local) conflicts. This site blocks ONLY a known
	// non-host-dispatched kind — an unknown locked kind keeps ALLOWING (the
	// opposite unknown-kind posture from the host-dispatch endpoint), which the
	// KindHostDispatched (hostDispatched, known) two-value shape keeps explicit.
	// The error names the locked kind so it stays accurate as new non-host kinds
	// (gitlab_ci, #1861) join the known set.
	if hostDispatched, known := runnerbackend.KindHostDispatched(got.RunnerKind); known && !hostDispatched {
		return nil, fmt.Errorf(
			"run %s is locked to runner_kind=%s, but fishhawk_dispatch_stage / fishhawk_run_stage spawn a LOCAL runner — dispatching here would conflict with the run's resolved execution channel. To run this stage on its own CI channel, dispatch it there; to drive it locally, start a NEW run with runner_kind=local",
			runUUID, got.RunnerKind)
	}
	// Locked to local (or any unknown non-actions kind): allow — the host
	// dispatch matches the run's resolved local channel.
	return nil, nil
}

// guardSiblingStageInFlight is the sibling-in-flight host-dispatch admission
// guard (incident bdf94763 / #1872). Dispatching a stage while another stage of
// the same run is still executing rotates the run's signing key out from under
// the in-flight runner (a fresh IssueKey per runner start), which — before the
// companion multi-key Verify fix — invalidated the in-flight runner's still-open
// artifact-upload signature (401 signature_invalid, pull_request_url left null).
// The signing fix is the correctness backstop; this guard is the admission-side
// belt: it refuses to spawn a second local runner while a sibling stage is
// mid-flight so the two runners never contend in the first place.
//
// targetStageID is the resolved stage the caller is about to spawn. Decision:
//
//   - A stage OTHER than the target in "dispatched" or "running": BLOCK — a
//     sibling runner is (or is about to be) live. Its whole ship phase is spent
//     in "running"; the incident dispatched acceptance while implement was still
//     shipping.
//   - The TARGET stage itself in "running": BLOCK — a second spawn would
//     double-drive the stage. The refusal is UNCONDITIONAL; only its MESSAGE
//     branches on the host runner-liveness verdict (#2689). Before #2689 this
//     arm asserted "a live runner owns it" as a pure DB-state claim — precisely
//     wrong in the strand case, where no runner exists at all and the operator
//     was told to wait for a settle that can never come. Now classifyReapLiveness
//     decides the wording: LIVE keeps the wait-for-it text; DEAD and UNKNOWN name
//     fishhawk_reap_stage -> fishhawk_retry_stage -> re-dispatch as the recovery.
//     Keeping the REFUSAL unconditional is the safety property: a probe
//     misclassification can only change prose, never ADMIT a dispatch this guard
//     would otherwise block. (Why the host-local probe is sound at THIS call site
//     without the explicit runner_kind gate the reap verb needs: both callers
//     run guardHostDispatch FIRST, which blocks a run locked to a known
//     non-host-dispatched kind, and a stage reaches "running" only via a runner's
//     signed prompt fetch — which is what LOCKS runner_kind. So a "running"
//     target on a remote run is already refused upstream and this arm sees only
//     local/unlocked runs. classifyReapLiveness gates on runner_kind anyway, so
//     even that reasoning failing degrades to the UNKNOWN wording, never a false
//     DEAD claim.)
//   - The TARGET stage itself in "awaiting_children" (#1891): BLOCK — it is a
//     decomposed parent's implement stage parked on its child slices. Spawning
//     a runner here 409s (stage_not_runnable) and the reaper report would
//     destroy the park; the error names fishhawk_run_children /
//     fishhawk_consolidate_slices as the correct verbs.
//   - The target stage in "awaiting_host_dispatch" (#1912) with every sibling
//     settled: ALLOW — this is the local park-then-spawn state (plan-approved
//     dispatch, retry_stage, fixup_stage all park the agent stage here for a
//     host-side spawn); blocking it would wedge every local dispatch. A target
//     merely "dispatched" is likewise ALLOW as the legacy/transitional
//     re-dispatch park (a spawned runner died and the operator re-dispatches).
//     A SIBLING in "awaiting_host_dispatch" is NOT in-flight (no spawn attempt
//     exists yet), so it never blocks the target — only {dispatched, running}
//     siblings do.
//   - All stages settled (pending / awaiting_* / terminal): ALLOW.
//   - Stage-list read error: FAIL OPEN with a warning, mirroring
//     guardHostDispatch's #1355 posture — availability over strictness for the
//     local loop, with the multi-key Verify fix as the correctness backstop.
//
// Returns (warnings, err): a non-nil err is the pre-execution block (the caller
// must NOT spawn); warnings merge into the caller's warnings slice.
func (r *runResolver) guardSiblingStageInFlight(ctx context.Context, runUUID uuid.UUID, targetStageID string) ([]string, error) {
	stages, err := r.api.ListRunStages(ctx, runUUID)
	if err != nil {
		// Fail OPEN: a transient backend read must not strand a legitimate local
		// dispatch. The companion multi-key Verify fix is the correctness backstop.
		return []string{fmt.Sprintf(
			"sibling-in-flight dispatch guard skipped (could not list stages for run %s: %v); proceeding — the multi-key signature verify remains the backstop",
			runUUID, err)}, nil
	}

	for _, s := range stages {
		inFlight := s.State == "dispatched" || s.State == "running"
		if s.ID == targetStageID {
			// A target parked "awaiting_children" is a decomposed parent's
			// implement stage waiting on its child slices (#1891). Spawning a
			// runner here produces a doomed prompt-fetch (409 stage_not_runnable),
			// and the detached reaper's spawn-failure report would then destroy
			// the park (awaiting_children → failed is a legal sweeper edge). Refuse
			// synchronously and name the correct verbs. The server-side reap no-op
			// is the fail-closed backstop if this guard is ever skipped.
			if s.State == "awaiting_children" {
				return nil, fmt.Errorf(
					"stage %s (%s) is a decomposed parent's implement stage parked awaiting_children for run %s — its child slices own it. Dispatching a runner here produces a doomed prompt fetch (409 stage_not_runnable) whose failure report would destroy the park. Use fishhawk_run_children to dispatch the child slices, then fishhawk_consolidate_slices for the final fan-in",
					s.ID, s.Type, runUUID)
			}
			// The target's own park states — "awaiting_host_dispatch" (#1912, the
			// plan-approved / retry / fixup local park) and legacy "dispatched"
			// (transitional dead-runner re-dispatch) — are allowed. Only a live
			// "running" target blocks.
			if s.State == "running" {
				return nil, r.targetRunningRefusal(ctx, runUUID, s)
			}
			continue
		}
		if inFlight {
			return nil, fmt.Errorf(
				"cannot dispatch: sibling stage %s (%s) is %s for run %s — dispatching another stage now would rotate the run's signing key out from under the in-flight runner. Wait for it to settle before dispatching (the implement ship phase ends when its pull-request artifact upload lands)",
				s.ID, s.Type, s.State, runUUID)
		}
	}
	return nil, nil
}

// targetRunningRefusal renders the target-'running' refusal (#2689). The
// refusal itself is UNCONDITIONAL — this function ALWAYS returns a non-nil
// error, for every liveness verdict — and only the wording branches, so a
// misclassified probe can never turn a block into an admission. LIVE keeps the
// pre-#2689 wait-for-it-to-settle text; DEAD and UNKNOWN name the recovery
// sequence the operator previously had to know by hand.
func (r *runResolver) targetRunningRefusal(ctx context.Context, runUUID uuid.UUID, s Stage) error {
	verdict, warnings := r.classifyReapLiveness(ctx, runUUID, s.ID)
	switch verdict {
	case runnerLive:
		return fmt.Errorf(
			"stage %s (%s) is already running for run %s — a live runner owns it; dispatching again would double-drive the stage. Wait for it to settle before re-dispatching",
			s.ID, s.Type, runUUID)
	case runnerDead:
		return fmt.Errorf(
			"stage %s (%s) is 'running' for run %s but NO process on this host carries stage id %s: the runner is GONE and the stage is STRANDED. Re-dispatching would not clear it. Recover with fishhawk_reap_stage (category C), then fishhawk_retry_stage, then re-dispatch",
			s.ID, s.Type, runUUID, s.ID)
	default: // runnerUnknown
		return fmt.Errorf(
			"stage %s (%s) is 'running' for run %s but runner liveness could not be verified from this host (%s). If no runner is alive the stage is STRANDED — recover with fishhawk_reap_stage (category C) then fishhawk_retry_stage; otherwise wait for it to settle before re-dispatching",
			s.ID, s.Type, runUUID, strings.Join(warnings, "; "))
	}
}
