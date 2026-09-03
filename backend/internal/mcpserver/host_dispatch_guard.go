package mcpserver

import (
	"context"
	"fmt"
	"path"
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

// guardNoPRImplement is the pre-spawn half of the two-layer
// push_and_open_pr=false refusal (#2691). An implement stage dispatched with
// push_and_open_pr=false on a DECOMPOSITION CHILD cannot settle: the runner
// stamps push_to_shared_branch in the bundle manifest regardless of --no-pr
// (willPushChild deliberately does not test it — the #771 forward gate exists
// to stop a succeeded-but-unlanded child), so the backend defers the child
// stage's terminal transition onto a /pull-request report the --no-pr runner
// never sends and the stage sits in `running` until the #2630 reaper sweeps it
// category C. Refusing here means no runner process is spawned at all.
//
// Decision table:
//
//   - stage != "implement", or push_and_open_pr is true: return immediately,
//     NO round-trip. The common path costs nothing.
//   - GetRun error: FAIL OPEN — a warning and a nil error, matching
//     guardHostDispatch's #1355 posture. On an unreadable run row this layer
//     cannot know the stage is a child, and refusing would break legitimate
//     dispatches during a backend hiccup. This is the ONE branch where a runner
//     process may start: the runner-side L1 refusal (main.go, immediately after
//     the prompt fetch and before any worktree or agent work) is the backstop,
//     so no AGENT PASS is burned even here.
//   - DecomposedFrom == nil: allow. The STANDALONE E22.8/#406 commit-yourself
//     flow is supported and must keep working — its trace upload settles the
//     stage because it stamps none of the three forward-gate flags.
//   - a decomposition child: BLOCK, naming fishhawk_run_children (which spawns
//     the identical child WITHOUT the flag).
//
// COST: on the implement + push_and_open_pr=false path this guard performs ONE
// ADDITIONAL GetRun beyond the run reads the callers already do. That read is
// deliberately not shared with guardHostDispatch: threading a run row through
// would couple the two guards' fail-open postures, and the extra read is paid
// only on the rare explicit-false dispatch.
//
// SCOPE: deliberately the decomposed-child signal ONLY. Fix-up status is NOT
// authoritatively knowable pre-spawn — cfg.fixup is derived server-side inside
// the /prompt handler from resolveFixupConcerns(runID, stageID) and served only
// on the prompt response; neither the run response nor the stage response
// carries it, and the fix-up / retry / plan-approved stage parks are
// indistinguishable by state (see guardSiblingStageInFlight below). The gate
// view's pending fix-up rows are a PREDICTION, not authority: the backend
// re-derives fixup at prompt-fetch time, so a concern waived, deferred or
// resolved between dispatch and fetch flips the answer and an L2 refusal built
// on it would reject a legitimate dispatch. That is why the fix-up arm lives in
// the runner, at the first point the answer is authoritative and still strictly
// before the agent invoke. base_branch is likewise NOT a refusal criterion: a
// base branch on a STANDALONE stage triggers neither a working-tree restore nor
// a forward-gate flag.
//
// Returns (warnings, err): a non-nil err is the pre-spawn block (the caller
// must NOT spawn); warnings merge into the caller's warnings slice.
func (r *runResolver) guardNoPRImplement(ctx context.Context, runUUID uuid.UUID, stage string, pushAndOpenPR bool) ([]string, error) {
	if stage != "implement" || pushAndOpenPR {
		return nil, nil
	}
	got, err := r.api.GetRun(ctx, runUUID)
	if err != nil {
		return []string{fmt.Sprintf(
			"push_and_open_pr=false decomposition-child guard skipped (could not read run %s: %v); proceeding — the runner-side refusal remains the backstop and fires before the agent is invoked",
			runUUID, err)}, nil
	}
	if got.DecomposedFrom == nil {
		return nil, nil
	}
	return nil, fmt.Errorf(
		"refusing to dispatch the implement stage of run %s with push_and_open_pr=false: it is a decomposition CHILD of parent run %s, and a child's implement stage settles on its push onto the shared parent branch (push_to_shared_branch, #771) — push_and_open_pr=false suppresses that push, so the backend defers the stage's terminal transition onto a /pull-request report that is never sent and the stage sits in 'running' until the reaper sweeps it. Use fishhawk_run_children to dispatch this child: it spawns the identical child WITHOUT the flag",
		runUUID, *got.DecomposedFrom)
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

// runnerBinaryAffectingScopePath reports whether a repo-relative plan scope
// path can change the built fishhawk-runner binary (E64.5 / #3086). It is the
// pure predicate behind the guardRunnerSelfHost advisory.
//
// It fires for any path UNDER runner/ — a directory-boundary prefix match, so
// runnerx/foo.go does NOT match — EXCEPT the deny-list: a basename of README.md,
// or a name ending in _test.go. Neither can change the compiled binary: Go
// excludes _test.go files from the non-test package build, and no package under
// runner/ embeds a README.md.
//
// The deny-list is deliberately a DENY-list defaulting to FIRE, not a *.go
// allow-list — the same asymmetry AGENTS.md records for scripts/dev's
// _path_affects_build. The fail-safe direction is over-warning: a spurious
// advisory line is cosmetic, while a suppressed one leaves the operator in the
// four-identical-failures loop #3086 exists to end. So embedded assets under
// runner/ (e.g. runner/internal/plan/schemas/*.json, which are //go:embed inputs
// that genuinely change the binary) keep firing ON PURPOSE.
func runnerBinaryAffectingScopePath(p string) bool {
	clean := path.Clean(strings.TrimSpace(p))
	if clean != "runner" && !strings.HasPrefix(clean, "runner/") {
		return false
	}
	base := path.Base(clean)
	if base == "README.md" || strings.HasSuffix(base, "_test.go") {
		return false
	}
	return true
}

// resolvePlanScopePathsForRun resolves the DECLARED scope.files paths of a run's
// terminal standard_v1 plan artifact, walking ParentRunID up to
// retryPlanChainDepth so a CI-retry chain or decomposition child whose plan
// lives on the parent is still covered — the same shape getPlan uses. It is a
// bounded, fail-OPEN resolver: EVERY error path (a ListRunStages /
// ListStageArtifacts / decode failure inside tryGetPlanForRun, a GetRun failure
// during the walk, an unparseable parent id) returns nil with NO error, because
// this backs an ADVISORY that must never strand a dispatch.
//
// COST is per VISITED RUN, not a flat two reads (#3086 cost correction): each
// tryGetPlanForRun performs up to two reads (list stages, list stage artifacts),
// and a miss then adds one GetRun to find the parent. The honest worst-case
// bound is therefore up to (2 plan-resolution reads + 1 GetRun) per visited run,
// capped at retryPlanChainDepth (8) levels — i.e. up to 3*retryPlanChainDepth
// backend reads. A plan resolved on the run itself costs the two reads and stops.
func (r *runResolver) resolvePlanScopePathsForRun(ctx context.Context, runUUID uuid.UUID) []string {
	current := runUUID
	for depth := 0; depth < retryPlanChainDepth; depth++ {
		p, found, err := r.tryGetPlanForRun(ctx, current)
		if err != nil {
			return nil
		}
		if found {
			paths := make([]string, 0, len(p.Scope.Files))
			for _, f := range p.Scope.Files {
				paths = append(paths, f.Path)
			}
			return paths
		}
		runRow, err := r.api.GetRun(ctx, current)
		if err != nil {
			return nil
		}
		if runRow.ParentRunID == nil || *runRow.ParentRunID == "" {
			return nil
		}
		parent, perr := uuid.Parse(*runRow.ParentRunID)
		if perr != nil {
			return nil
		}
		current = parent
	}
	return nil
}

// guardRunnerSelfHost is the runner self-host bootstrap advisory (E64.5 /
// #3086). A run whose approved plan changes fishhawk-runner ITSELF executes
// under bin/fishhawk-runner built from main — which still carries the very
// defect the run is fixing — so a stage that triggers that defect fails
// category-A repeatedly and burns fix-up budget with nothing pushed, and the
// operator sees four identical failures with no signpost. This guard resolves
// the run's declared plan scope and, when it names a runner/ path that can
// change the built binary, appends ONE advisory line naming the consequence and
// the documented escape in runner/README.md.
//
// It is purely ADVISORY: unlike its three siblings it returns only []string and
// has NO error return, so it structurally CANNOT block a dispatch — giving it no
// error return makes that structural rather than conventional. It fails OPEN and
// SILENT on every read error (resolvePlanScopePathsForRun swallows them),
// matching guardHostDispatch's #1355 posture.
//
// A "plan"-stage dispatch returns immediately WITHOUT reading: no plan artifact
// exists at plan-stage dispatch, so the plan-resolution reads would always be
// wasted.
//
// Two known residuals, documented in backend/internal/mcpserver/README.md: the
// advisory fires on the DECLARED plan scope, so a runner/ path added later by an
// approved scope amendment does NOT trigger it; and the parent walk is capped at
// retryPlanChainDepth, so a chain deeper than that resolves no plan and the
// guard stays silent.
func (r *runResolver) guardRunnerSelfHost(ctx context.Context, runUUID uuid.UUID, stage string) []string {
	if stage == "plan" {
		return nil
	}
	var offending string
	for _, p := range r.resolvePlanScopePathsForRun(ctx, runUUID) {
		if runnerBinaryAffectingScopePath(p) {
			offending = p
			break
		}
	}
	if offending == "" {
		return nil
	}
	return []string{fmt.Sprintf(
		"this run's declared plan scope changes fishhawk-runner itself (e.g. %s), but the dispatched %q stage executes bin/fishhawk-runner built from main — which does NOT contain this run's fix. A stage that triggers the defect this run is fixing will fail category-A repeatedly and burn fix-up budget with nothing pushed. To run the stage under the fixed runner, rebuild bin/fishhawk-runner from the run branch into a scratch worktree before dispatching — see the 'Self-hosting bootstrap deadlock' section in runner/README.md (#3086)",
		offending, stage)}
}
