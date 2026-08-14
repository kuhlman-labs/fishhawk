package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fishhawk_reap_stage (E67.47 / #2689) is the operator-reachable wrapper over
// the EXISTING `POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure`
// endpoint. It adds no REST route, no schema and no server-side behaviour: the
// SERVER stays the sole authority on what may be reaped
// (backend/internal/server/reap_failure.go's reapProtectedParkStates is a
// CLOSED allow-list of five PARK states the reaper must never collapse, so the
// reaper's authority is exactly {dispatched, running}).
//
// WHY A VERB IS NEEDED AT ALL. #2363's option 4(a) landed — run_children.go
// spawns children through spawnRunnerStageDetachedFn, so every child IS
// registered in the detached registry and both reaper arms (#1747 non-zero
// exit, #2630 zero-exit strand) cover them. TWO residual strand paths survive
// that, and in BOTH the stage sits in {dispatched, running} with no runner and
// no reaper left to clear it:
//
//   - run_children's spawn-error compensation (run_children.go, BRANCH 2) when
//     the category-C report ITSELF fails: its own doc comment states the case —
//     the stage stays 'dispatched' with no runner and no reaper.
//   - the reaper's report exhausting its bounded backoff
//     (reportDetachedFailureWithRetry, run_stage.go): after the last attempt it
//     falls back to a stderr diagnostic plus the off-by-default dispatch
//     watchdog, leaving the stage in {dispatched, running}.
//
// In both, fishhawk_retry_stage refuses (run.RetryStage admits ONLY a 'failed'
// stage), fishhawk_revive_run refuses (it admits only a terminal-FAILED run),
// fishhawk_fixup_stage and fishhawk_dispatch_stage refuse, and the ONLY
// recovery was a hand-rolled POST to the reap-failure endpoint. This verb is
// that POST.
const (
	// reapStageDefaultCategory is the retryable infrastructure class a reap
	// reports. The endpoint accepts only B or C; C is what makes
	// fishhawk_retry_stage applicable afterwards.
	reapStageDefaultCategory = "C"
	// reapStageCategoryB is the other category the endpoint accepts: the
	// constraint/policy class. Named so the allow-list and the next-step
	// selector share one literal.
	reapStageCategoryB = "B"
	// reapStageDefaultReason is the recorded classification when the operator
	// supplies none.
	reapStageDefaultReason = "operator_reap_stranded_stage"
	// reapStageNextStepHintC is the post-reap guidance for the DEFAULT category:
	// the reap only lands the stage in 'failed'; re-opening it is a separate
	// operator verb, and at category C that verb is fishhawk_retry_stage.
	reapStageNextStepHintC = "the stage is now 'failed' (category C, the retryable infrastructure class), so fishhawk_retry_stage admits it: call fishhawk_retry_stage next, then re-dispatch (fishhawk_dispatch_stage / fishhawk_run_stage, or fishhawk_run_children for a decomposition child)."
	// reapStageNextStepHintB is the guidance for the OTHER accepted category.
	// It is a DIFFERENT next step, not a wording variant: run.RetryableFailure
	// classifies B (constraint/policy) as NOT retryable, so fishhawk_retry_stage
	// refuses a B-reaped stage with 422 retry_not_applicable and the recovery
	// sequence needs the operator-only override escape hatch (#698) instead.
	reapStageNextStepHintB = "the stage is now 'failed' (category B, the constraint/policy class), which fishhawk_retry_stage does NOT admit — it refuses with 422 retry_not_applicable, because category B means a spec/workflow change rather than a re-run. Re-opening it needs the operator-only category-B override: POST /v0/stages/{stage_id}/retry with {\"override\":true,\"reason\":\"...\"} (an operator token; agent tokens are refused), then re-dispatch. To recover an ordinary strand, reap at the default category C instead — B is for recording a genuine constraint/policy failure."
)

// reapStageNextStep selects the post-reap guidance for the category actually
// reported. The two accepted categories have DIFFERENT recovery paths, so a
// single constant would tell a category-B caller to invoke a verb that refuses
// the stage it just produced. The argument is the DEFAULTED category, so the
// only inputs are the two the allow-list admits; anything else is treated as C
// (unreachable — reapStage refuses it before this is called).
func reapStageNextStep(category string) string {
	if category == reapStageCategoryB {
		return reapStageNextStepHintB
	}
	return reapStageNextStepHintC
}

// runnerLivenessLabel renders a runnerLivenessVerdict as the operator-facing
// string carried on ReapStageOutput.runner_liveness. Kept separate from the
// verdict type (drive_run.go) so the wire vocabulary is owned by the verb that
// publishes it.
func runnerLivenessLabel(v runnerLivenessVerdict) string {
	switch v {
	case runnerLive:
		return "live"
	case runnerDead:
		return "dead"
	default:
		return "unknown"
	}
}

// ReapStageInput is the fishhawk_reap_stage tool's input schema. Mirrors the
// reap-failure endpoint's body; every field but the two ids is optional and
// defaulted locally.
type ReapStageInput struct {
	RunID    string `json:"run_id" jsonschema:"the Fishhawk run UUID owning the stranded stage"`
	StageID  string `json:"stage_id" jsonschema:"the UUID of the stage stranded in dispatched or running with no live runner"`
	Category string `json:"category,omitempty" jsonschema:"failure category to record; only B or C are accepted and the default C is the retryable infrastructure class that makes fishhawk_retry_stage applicable"`
	Reason   string `json:"reason,omitempty" jsonschema:"short classification recorded on the failure; defaults to operator_reap_stranded_stage"`
	Detail   string `json:"detail,omitempty" jsonschema:"optional longer diagnostic recorded alongside the reason (the runner log tail, the observed strand, the incident id)"`
	ExitCode int    `json:"exit_code,omitempty" jsonschema:"optional exit code of the dead runner process when known; omit when unknown"`
}

// ReapStageOutput mirrors the endpoint's 200 body plus the host-liveness
// verdict this verb classified before making the call, the post-reap next step,
// and any warnings raised while classifying.
type ReapStageOutput struct {
	Transitioned bool   `json:"transitioned" jsonschema:"whether this report drove the stage to failed; false on the idempotent no-op against an already-terminal stage"`
	StageState   string `json:"stage_state" jsonschema:"the stage state after the report; failed on a successful reap"`
	// RunnerLiveness is deliberately NARROW: see classifyReapLiveness. It is
	// "dead" ONLY for a runner_kind=local run this host could probe.
	RunnerLiveness string   `json:"runner_liveness" jsonschema:"the host runner-liveness verdict classified before the reap: live (refused), dead (a local run this host probed and found no runner for), or unknown (a non-local runner_kind, an unreadable run row, or an inconclusive probe)"`
	NextStep       string   `json:"next_step" jsonschema:"the post-reap guidance, which DIFFERS by the category reported: at C the stage is failed/category-C so fishhawk_retry_stage admits it, then re-dispatch; at B fishhawk_retry_stage refuses (422 retry_not_applicable) and re-opening needs the operator-only category-B override on POST /v0/stages/{stage_id}/retry"`
	Warnings       []string `json:"warnings,omitempty" jsonschema:"non-fatal notes raised while classifying runner liveness, including the unverifiable-from-this-host note for a non-local runner_kind"`
}

// registerReapStage wires the fishhawk_reap_stage tool (E67.47 / #2689).
//
// Auth: operator write tool. The endpoint requires write:runs; a token without
// it surfaces 403 insufficient_scope verbatim.
func registerReapStage(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_reap_stage",
		Description: strings.TrimSpace(`
Use this when a stage is STRANDED in 'dispatched' or 'running' with no live
runner and every other recovery verb refuses it — fishhawk_retry_stage admits
only a 'failed' stage, fishhawk_revive_run only a terminal-FAILED run, and
fishhawk_fixup_stage / fishhawk_dispatch_stage both refuse a stage the
sibling-in-flight guard reads as owned by a live runner. This is the verb that
clears it.

It wraps POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure — the same
endpoint the detached reaper uses — reporting the stage failed at category C,
the retryable infrastructure class. After it lands the stage is 'failed' and
fishhawk_retry_stage becomes applicable; the recovery sequence is
fishhawk_reap_stage -> fishhawk_retry_stage -> re-dispatch.

This can never destroy a live park. The SERVER stays the authority on what may
be reaped: it refuses the five protected PARK states (awaiting_children,
awaiting_approval, awaiting_input, awaiting_scope_decision,
awaiting_host_dispatch) and its reap authority is exactly {dispatched, running}.
A protected-park refusal surfaces here verbatim.

This is NOT fishhawk_cancel_run. On a decomposition CHILD a cancel permanently
wedges the parent, because fishhawk_consolidate_slices requires every child to
have SUCCEEDED — a cancelled child can never satisfy it. Reap the stranded
stage instead of cancelling the run.

Before the call the verb classifies host runner liveness, GATED on the run's
runner_kind, and the invariant it delivers is narrow by design: for a
runner_kind=local run it probes THIS host's process table for the stage id and
REFUSES outright when a runner is still live. For github_actions / gitlab_ci /
an unreadable or absent runner_kind a host-local process probe is INAPPLICABLE —
the verdict is 'unknown', never 'dead', and the call proceeds on your explicit
invocation plus the server-side protected-park allow-list, with a warning saying
liveness is unverifiable from this host.

That classification and the POST are two round-trips, not one atomic operation,
and the endpoint's authority spans BOTH 'dispatched' and 'running' — so a
concurrent dispatch could spawn a runner for a stage just classified 'dead' and
the POST would fail a LIVE stage. The verb NARROWS that window: immediately
before the POST it re-reads the stage state and re-probes, and refuses if the
state moved or a runner appeared. It does not CLOSE it — only a server-side
compare-and-set on the observed state could — so the honest guarantee is
"reaping a live stage this host spawned is refused on every observation this
verb can take", not "never".

Input:
  - run_id, stage_id : the stranded stage (both required).
  - category         : B or C; defaults to C. Anything else is refused locally.
                       Prefer the default: only C is retryable, so a category-B
                       reap lands a stage fishhawk_retry_stage REFUSES (422
                       retry_not_applicable) and re-opening it then needs the
                       operator-only category-B override on
                       POST /v0/stages/{stage_id}/retry. next_step says which
                       of the two you got.
  - reason           : defaults to operator_reap_stranded_stage.
  - detail, exit_code: optional diagnostics recorded with the failure.

Tool errors:
  - invalid UUID, or a category outside {B, C} (both caught before the HTTP hop)
  - a LIVE runner for a runner_kind=local run (refused; no HTTP hop)
  - stage_not_found (404)
  - the protected-park refusal (the stage is in one of the five park states)
  - insufficient_scope (403 — the token lacks write:runs)
`),
	}, resolver.reapStage)
}

// reapStage is the tool handler. Every refusal short-circuits BEFORE the HTTP
// hop: id parse, then the category allow-list, then the liveness classification.
func (r *runResolver) reapStage(ctx context.Context, _ *mcp.CallToolRequest, in ReapStageInput) (*mcp.CallToolResult, ReapStageOutput, error) {
	runUUID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, ReapStageOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	stageUUID, err := uuid.Parse(in.StageID)
	if err != nil {
		return nil, ReapStageOutput{}, fmt.Errorf("stage_id %q is not a valid UUID: %w", in.StageID, err)
	}

	// The category allow-list is enforced LOCALLY with its own message rather
	// than by relaying the server's 400 text: the two strings are independent by
	// design, so neither is pinned to the other.
	category := in.Category
	if category == "" {
		category = reapStageDefaultCategory
	}
	if category != reapStageCategoryB && category != reapStageDefaultCategory {
		return nil, ReapStageOutput{}, fmt.Errorf(
			"category must be \"B\" or \"C\" (C is the retryable infrastructure class a reap reports); got %q", in.Category)
	}
	reason := in.Reason
	if reason == "" {
		reason = reapStageDefaultReason
	}

	// The stage's state BEFORE the classification: half of the check/use race
	// detector in confirmStrandBeforeReap. An unreadable or absent row leaves
	// that comparison unarmed (fail OPEN — see there).
	before, beforeKnown := r.observeStageState(ctx, runUUID, in.StageID)

	verdict, probed, warnings := r.classifyReapLivenessGated(ctx, runUUID, in.StageID)
	if verdict == runnerLive {
		// The one case the probe can prove: a runner THIS host spawned still
		// carries this stage id. Reaping it would fail a stage that is genuinely
		// executing, so refuse before any HTTP hop.
		return nil, ReapStageOutput{}, fmt.Errorf(
			"refusing to reap stage %s of run %s: a process on this host still carries this stage id, so the runner is LIVE and the stage is executing, not stranded. Wait for it to settle, or use fishhawk_cancel_run to abandon the whole run (never on a decomposition child — a cancelled child can never satisfy fishhawk_consolidate_slices)",
			in.StageID, in.RunID)
	}

	// LAST observation before the hop: the classification above is already stale
	// by the time the POST is written.
	if err := r.confirmStrandBeforeReap(ctx, runUUID, in.StageID, before, beforeKnown, probed); err != nil {
		return nil, ReapStageOutput{}, err
	}

	res, err := r.api.ReportStageFailure(ctx, runUUID, stageUUID, category, reason, in.Detail, in.ExitCode)
	if err != nil {
		// The backend's refusals — 404 stage_not_found, the protected-park
		// refusal, 403 insufficient_scope — reach the operator verbatim.
		return nil, ReapStageOutput{}, fmt.Errorf("reap stage: %w", err)
	}
	return nil, ReapStageOutput{
		Transitioned:   res.Transitioned,
		StageState:     res.StageState,
		RunnerLiveness: runnerLivenessLabel(verdict),
		NextStep:       reapStageNextStep(category),
		Warnings:       warnings,
	}, nil
}

// classifyReapLiveness decides whether a runner for stageID is alive on THIS
// host, GATED on the run's runner_kind, and NEVER returns runnerDead outside the
// local arm. A pgrep of this host's process table says something about a run
// this host spawned and nothing at all about a run executing on GitHub Actions
// or GitLab CI, so treating a non-match as death would be a fabricated verdict.
// The decision table:
//
//	(i)   GetRun error          -> runnerUnknown + a warning naming the run and
//	                               the read error (runner_kind is unreadable).
//	(ii)  runner_kind == local  -> the host probe's verdict VERBATIM (live, dead
//	                               or unknown), warning only on unknown.
//	(iii) any other non-empty   -> runnerUnknown WITHOUT probing, plus the
//	      runner_kind              inapplicable-probe warning.
//	(iv)  runner_kind == ""     -> the same as (iii): an absent kind (an older
//	                               backend omits the omitempty field) cannot be
//	                               CONFIRMED local, and 'never DEAD' is the safe
//	                               direction.
//
// The probe itself is the drive_run.go seam, unchanged and shared:
// r.driveProbeRunnerLiveness when injected, else probeRunnerLiveness.
func (r *runResolver) classifyReapLiveness(ctx context.Context, runUUID uuid.UUID, stageID string) (runnerLivenessVerdict, []string) {
	verdict, _, warnings := r.classifyReapLivenessGated(ctx, runUUID, stageID)
	return verdict, warnings
}

// classifyReapLivenessGated is classifyReapLiveness plus the gate's own
// decision: probed reports whether the host probe was APPLICABLE (the local
// arm), which is what lets the pre-POST re-confirmation re-probe without a
// second GetRun and without probing a run whose runner_kind the gate excluded.
func (r *runResolver) classifyReapLivenessGated(ctx context.Context, runUUID uuid.UUID, stageID string) (runnerLivenessVerdict, bool, []string) {
	got, err := r.api.GetRun(ctx, runUUID)
	if err != nil {
		return runnerUnknown, false, []string{fmt.Sprintf(
			"could not verify runner liveness for run %s because its runner_kind is unreadable (%v); proceeding on your explicit invocation plus the server-side protected-park allow-list",
			runUUID, err)}
	}
	if got.RunnerKind != driveRunnerKindLocal {
		kind := got.RunnerKind
		if kind == "" {
			kind = "(absent)"
		}
		return runnerUnknown, false, []string{fmt.Sprintf(
			"a host-local process probe is INAPPLICABLE to a runner_kind=%s run: liveness is unverifiable from this host, so proceeding rests on your explicit invocation plus the server-side protected-park allow-list (the five protected park states the server refuses to reap)",
			kind)}
	}
	verdict := r.livenessProbe()(ctx, stageID)
	if verdict == runnerUnknown {
		return verdict, true, []string{fmt.Sprintf(
			"the host runner-liveness probe for stage %s was inconclusive (pgrep absent from PATH, a syntax/fatal exit, or a timeout); liveness could not be confirmed either way",
			stageID)}
	}
	return verdict, true, nil
}

// livenessProbe resolves the shared drive_run.go probe seam:
// r.driveProbeRunnerLiveness when injected, else the real probeRunnerLiveness.
func (r *runResolver) livenessProbe() func(context.Context, string) runnerLivenessVerdict {
	if r.driveProbeRunnerLiveness != nil {
		return r.driveProbeRunnerLiveness
	}
	return probeRunnerLiveness
}

// observeStageState reads the target stage's CURRENT state. The second return
// is false when the state could not be observed at all — the stage list read
// failed, or the run carries no such stage — which leaves the caller's
// comparison unarmed rather than asserting a change.
func (r *runResolver) observeStageState(ctx context.Context, runUUID uuid.UUID, stageID string) (string, bool) {
	stages, err := r.api.ListRunStages(ctx, runUUID)
	if err != nil {
		return "", false
	}
	for _, s := range stages {
		if s.ID == stageID {
			return s.State, true
		}
	}
	return "", false
}

// confirmStrandBeforeReap re-observes the target IMMEDIATELY before the POST and
// refuses on any evidence the stage moved under the liveness classification.
//
// WHY (the #2689 fix-up concern). classifyReapLivenessGated and
// ReportStageFailure are two round-trips, not one atomic operation, and the
// endpoint's reap authority spans BOTH 'dispatched' and 'running'. So this
// interleaving fails a genuinely LIVE stage:
//
//	probe(stage 'dispatched') -> dead   |
//	                                    | a concurrent dispatch spawns a runner
//	                                    | the stage advances dispatched -> running
//	POST /reap-failure ---------------->| the live runner's stage is failed
//
// Two INDEPENDENT detectors, both taken as late as possible:
//
//	(1) the stage's state CHANGED since the pre-classification read — something
//	    else is driving it, and the dispatched -> running advance above is
//	    exactly that signature;
//	(2) the host probe, RE-RUN, now finds a LIVE runner — the spawn landed but
//	    the state has not advanced yet, which (1) alone would miss.
//
// This NARROWS the window; it does not close it. Nothing on this side of the
// wire can: a dispatch landing between this last observation and the server
// committing the failure is still reaped. Closing it needs a server-side
// compare-and-set (reap only from the state the caller observed) on an endpoint
// this verb deliberately does not change. The tool description and the package
// README state the guarantee at that narrowed strength rather than as an
// absolute invariant.
//
// Both detectors fail OPEN on an unreadable or absent stage row: this is a
// narrowing layer on top of the server's protected-park allow-list, and
// refusing a recovery verb on a transient read error would re-strand the very
// stage it exists to clear.
func (r *runResolver) confirmStrandBeforeReap(ctx context.Context, runUUID uuid.UUID, stageID, before string, beforeKnown, probed bool) error {
	if after, afterKnown := r.observeStageState(ctx, runUUID, stageID); beforeKnown && afterKnown && after != before {
		return fmt.Errorf(
			"refusing to reap stage %s of run %s: it moved from %q to %q while its runner liveness was being classified, so something else is driving it — a concurrent dispatch spawns a runner and advances the stage into 'running', and reaping now would fail a LIVE runner's stage. Re-read the stage (fishhawk_get_run_status) and re-invoke only if it is still stranded",
			stageID, runUUID, before, after)
	}
	if !probed {
		// A non-local runner_kind was never probed in the first place; re-probing
		// here would fabricate the host verdict the gate exists to withhold.
		return nil
	}
	if r.livenessProbe()(ctx, stageID) == runnerLive {
		return fmt.Errorf(
			"refusing to reap stage %s of run %s: a process on this host picked up this stage id between the liveness classification and the reap — a concurrent dispatch spawned a runner, so the stage is executing, not stranded. Wait for it to settle before reaping",
			stageID, runUUID)
	}
	return nil
}
