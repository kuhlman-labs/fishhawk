package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/hostdispatcher"
)

// host_dispatcher.go is the per-run core of the ADR-104 host dispatcher (E88.2
// / #4526). It decides, for ONE candidate run at a time, whether to host-spawn
// the run's parked stage, and walks the single pre-marker sequence when it
// does:
//
//	campaign-link read (fail closed) -> gate (caps + pause, injected)
//	-> record run_auto_driven (fail closed) -> host-dispatch marker -> spawn
//
// The core has NO goroutines and never acts on its own: the tick loop
// (host_dispatcher_loop.go, slice 3) is its only caller, so every marker POST
// and spawn passes the same campaign, cap and pause checks. It is
// dispatch-only: it holds no reference to AutoDriveRunGate, approval, merge or
// scope-amendment-decision client methods, so it never acts at a gate. A 409
// concurrency_slot_queued leaves the stage parked at awaiting_host_dispatch for
// a later tick (the queue record is ADR-094, amended by ADR-100 and ADR-115);
// there is no asynchronous slot waiter.

// Host-dispatcher refusal codes. Every refusal commits nothing the core owns:
// the gate reservation is released, and no marker or spawn follows.
const (
	hdRefuseNotLocal              = "runner_kind_not_local"
	hdRefuseRunTerminal           = "run_terminal"
	hdRefusePathOutside           = pathOutsideAllowedRootsCode
	hdRefuseNotEligible           = "not_eligible"
	hdRefuseParentUnreadable      = "parent_unreadable"
	hdRefuseRunReadFailed         = "run_read_failed"
	hdRefuseStagesReadFailed      = "stages_read_failed"
	hdRefuseNothingDispatchable   = "nothing_dispatchable"
	hdRefuseInFlight              = "in_flight"
	hdRefuseStaleRunnerLive       = "stale_runner_live"
	hdRefuseStaleRunnerUnknown    = "stale_runner_unknown"
	hdRefuseStaleRespawnCap       = "stale_respawn_cap_reached"
	hdRefuseRespawnLedgerFailed   = "respawn_ledger_failed"
	hdRefuseForgeUnresolved       = "forge_unresolved"
	hdRefuseBaseBranchUnresolved  = "base_branch_unresolved"
	hdRefuseExecutorUnsupported   = "executor_agent_unsupported"
	hdRefuseCampaignUnavailable   = "campaign_link_unavailable"
	hdRefuseCampaignUnresolved    = "campaign_link_unresolved"
	hdRefuseCampaignNotRunning    = "campaign_not_running"
	hdRefuseGateUnconfigured      = "gate_unconfigured"
	hdRefuseAcceptanceAdmission   = "acceptance_admission_failed"
	hdRefuseAcceptanceShortCircut = "acceptance_short_circuited"
	hdRefuseAcceptanceNeedsTarget = "acceptance_needs_target"
	hdRefuseRecordFailed          = "record_act_failed"
	hdRefuseSlotQueued            = concurrencySlotQueuedCode
	hdRefuseMemoryPressure        = hostMemoryPressureCode
	hdRefuseMarkerFailed          = "host_dispatch_failed"
	hdRefuseMarkerNotTransitioned = "marker_not_transitioned"
	hdRefuseSpawnCancelled        = "spawn_refused_run_cancelled"
	hdRefuseSpawnFailed           = "spawn_failed"
)

// defaultHostDispatchStaleAfter is how long a 'dispatched' stage this process
// is not running may sit before the core probes its runner's liveness. It
// matches drive_run's runner-liveness threshold.
const defaultHostDispatchStaleAfter = defaultDriveDispatchedStaleAfter

// hostDispatchCandidate is what the gate sees for one admission attempt.
type hostDispatchCandidate struct {
	RunID     string
	StageID   string
	StageType string
	// Respawn marks the once-per-marker-epoch stale re-spawn path.
	Respawn bool
}

// hostDispatchGate is the last local predicate before any POST (caps, the
// ledger reservation and the pause check; slice 3 implements it). Admit returns
// a non-empty refusal code to refuse, or a release func the core calls when the
// attempt does not end in a spawn. An error refuses (fail closed).
type hostDispatchGate interface {
	Admit(ctx context.Context, c hostDispatchCandidate) (release func(), refusal string, err error)
}

// hostDispatchRespawnCounter bounds the stale re-spawn to ONE per (stage id,
// marker epoch) (ADR-104 Q2). Allowed is the read-only early check; Reserve
// consumes the epoch's single re-spawn BEFORE the spawn and reports whether it
// was still available. Slice 3 persists it in the ledger so a restart cannot
// grant a second re-spawn.
type hostDispatchRespawnCounter interface {
	Allowed(stageID string, epoch time.Time) bool
	Reserve(stageID string, epoch time.Time) (bool, error)
}

// hostDispatchOutcome is one evaluation's result.
type hostDispatchOutcome struct {
	RunID     string
	StageID   string
	StageType string
	// Spawned is true only when a runner was started.
	Spawned bool
	// Respawn marks a spawn on the stale re-spawn path.
	Respawn bool
	// Refusal names why nothing was spawned ("" on a spawn).
	Refusal string
	Detail  string
	// QueueEpisode is true when the marker queued the stage: it stays at
	// awaiting_host_dispatch and the next tick should re-attempt it first.
	QueueEpisode bool
	// CampaignRefused marks a refusal for campaign state (the tick moves the
	// run last next time; it never skips it without a fresh read).
	CampaignRefused bool
	// StopMarkers is true on 409 host_memory_pressure: the tick stops POSTing
	// markers for the rest of the round.
	StopMarkers bool
	LogPath     string
}

// hostDispatcher is the unexported per-run core.
type hostDispatcher struct {
	// r carries the api client and the allowed roots. It is built with
	// httpTransport false; the core confines working_dir itself.
	r *runResolver
	// binary is the resolved fishhawk-runner path.
	binary string
	gate   hostDispatchGate
	// respawns is the per-epoch stale re-spawn counter.
	respawns hostDispatchRespawnCounter
	// spawn is the detached-spawn seam (default spawnRunnerStageDetached).
	spawn driveSpawnFunc
	// probe is the runner liveness seam (default probeRunnerLiveness).
	probe func(ctx context.Context, stageID string) runnerLivenessVerdict
	// now is the clock seam.
	now func() time.Time
	// environ is the process-environment seam (default os.Environ).
	environ    func() []string
	staleAfter time.Duration

	// running holds the stage ids THIS process spawned. A 'dispatched' stage in
	// it is in flight: this process's own detached reaper reports its runner's
	// exit (the stage goes failed, which selection never re-picks), so the core
	// never re-spawns a stage it is running itself.
	running map[string]bool
	// recorded holds the stage ids whose run_auto_driven row this process wrote
	// for a still-live queue episode: a re-attempt re-POSTs the marker without
	// a second row.
	recorded map[string]bool
}

func newHostDispatcher(r *runResolver, binary string, gate hostDispatchGate, respawns hostDispatchRespawnCounter) *hostDispatcher {
	return &hostDispatcher{
		r:          r,
		binary:     binary,
		gate:       gate,
		respawns:   respawns,
		spawn:      spawnRunnerStageDetached,
		probe:      probeRunnerLiveness,
		now:        time.Now,
		environ:    os.Environ,
		staleAfter: defaultHostDispatchStaleAfter,
		running:    map[string]bool{},
		recorded:   map[string]bool{},
	}
}

// hostDispatchSpawnable is the dispatched-vs-fresh split for a stage
// driveDispatchableStage selected: a 'dispatched' stage takes the stale path,
// every other selected state (pending, awaiting_host_dispatch) is fresh.
func hostDispatchSpawnable(st Stage) (fresh bool) {
	return st.State != "dispatched"
}

// hostDispatchConfined reports whether workingDir is non-empty, absolute and
// inside one of roots. It is independent of the resolver's httpTransport: an
// empty working_dir would otherwise fall back to the process cwd.
func hostDispatchConfined(workingDir string, roots []string) bool {
	if workingDir == "" || !filepath.IsAbs(workingDir) {
		return false
	}
	candidate, err := resolveForConfinement(workingDir)
	if err != nil {
		return false
	}
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			continue
		}
		resolvedRoot, rerr := resolveForConfinement(root)
		if rerr != nil {
			continue
		}
		if pathWithinRoot(candidate, resolvedRoot) {
			return true
		}
	}
	return false
}

// hostDispatchRunnerEnv is the spawned runner's environment: env with every
// FISHHAWK_API_TOKEN entry removed, and any entry whose value equals the
// dispatcher's own bearer. NOTHING is appended — unlike the operator verbs,
// which forward FISHHAWK_API_TOKEN. The runner needs no bearer: its signing
// key, prompt fetch and uploads are Ed25519-signed, and the agent's bearer is
// the run-bound token the runner mints itself. Not forwarding the bearer fits
// ADR-104 Q1's permissive "may be the forwarded token" and is narrower than
// ADR-104 Q4; proposed amendment for Captain ratification.
func hostDispatchRunnerEnv(env []string, bearer string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		if name == "FISHHAWK_API_TOKEN" {
			continue
		}
		if bearer != "" && value == bearer {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (*hostDispatcher) refuse(o hostDispatchOutcome, code, detail string) hostDispatchOutcome {
	o.Refusal = code
	o.Detail = detail
	return o
}

// eligible reports whether run carries the mark, or is a decomposition child
// whose parent does. A parent read error refuses (fail closed). A verify-only
// re-open is eligible only through the same mark: an unmarked run is never
// spawned, whatever re-opened it.
func (h *hostDispatcher) eligible(ctx context.Context, run *Run) (bool, string, error) {
	if run.AutoDispatch {
		return true, "", nil
	}
	if run.SliceIndex == nil {
		return false, "run carries no auto_dispatch mark", nil
	}
	parentID := ""
	switch {
	case run.DecomposedFrom != nil && *run.DecomposedFrom != "":
		parentID = *run.DecomposedFrom
	case run.ParentRunID != nil && *run.ParentRunID != "":
		parentID = *run.ParentRunID
	default:
		return false, "child carries no parent reference", nil
	}
	parentUUID, err := uuid.Parse(parentID)
	if err != nil {
		return false, "", fmt.Errorf("parent run id %q is not a UUID", parentID)
	}
	parent, err := h.r.api.GetRun(ctx, parentUUID)
	if err != nil {
		return false, "", fmt.Errorf("read parent run %s: %w", parentID, err)
	}
	if !parent.AutoDispatch {
		return false, "child of an unmarked parent", nil
	}
	return true, "", nil
}

// campaignPermits applies the campaign-link gate: proceed only when the run is
// unlinked, or linked with EVERY linked campaign running.
func campaignPermits(link *runCampaignLink) (string, string) {
	switch link.Status {
	case campaignLinkStatusUnlinked:
		return "", ""
	case campaignLinkStatusLinked:
		if len(link.Campaigns) == 0 {
			return hdRefuseCampaignUnresolved, "linked with no visible campaign"
		}
		for _, c := range link.Campaigns {
			if c.State != "running" {
				return hdRefuseCampaignNotRunning, fmt.Sprintf("campaign %s is %s", c.CampaignID, c.State)
			}
		}
		return "", ""
	case campaignLinkStatusUnresolved:
		return hdRefuseCampaignUnresolved, link.Reason
	default:
		return hdRefuseCampaignUnresolved, fmt.Sprintf("unknown campaign_link status %q", link.Status)
	}
}

// evaluate decides one candidate run and walks the pre-marker sequence when it
// is admissible. It is synchronous and commits nothing on a refusal.
func (h *hostDispatcher) evaluate(ctx context.Context, runID uuid.UUID) hostDispatchOutcome {
	o := hostDispatchOutcome{RunID: runID.String()}
	view, err := h.r.fetchRunDriveView(ctx, runID)
	if err != nil {
		return h.refuse(o, hdRefuseRunReadFailed, err.Error())
	}
	run := &view.Run
	if run.RunnerKind != driveRunnerKindLocal {
		return h.refuse(o, hdRefuseNotLocal, "runner_kind "+run.RunnerKind)
	}
	if runStateIsTerminal(run.State) {
		return h.refuse(o, hdRefuseRunTerminal, "run is "+run.State)
	}
	if !hostDispatchConfined(run.WorkingDir, h.r.allowedRoots) {
		return h.refuse(o, hdRefusePathOutside, fmt.Sprintf("working_dir %q is empty, relative or outside every allowed root", run.WorkingDir))
	}
	ok, why, eerr := h.eligible(ctx, run)
	if eerr != nil {
		return h.refuse(o, hdRefuseParentUnreadable, eerr.Error())
	}
	if !ok {
		return h.refuse(o, hdRefuseNotEligible, why)
	}

	stages, serr := h.r.api.ListRunStages(ctx, runID)
	if serr != nil {
		return h.refuse(o, hdRefuseStagesReadFailed, serr.Error())
	}
	disp := driveDispatchableStage(stages, nil, view.DerivedStatus == derivedAcceptancePending)
	if disp == nil {
		return h.refuse(o, hdRefuseNothingDispatchable, "")
	}
	o.StageID, o.StageType = disp.ID, disp.Type
	stageUUID, perr := uuid.Parse(disp.ID)
	if perr != nil {
		return h.refuse(o, hdRefuseStagesReadFailed, fmt.Sprintf("stage id %q is not a UUID", disp.ID))
	}

	// The stale path: a 'dispatched' stage this process is not running.
	respawn := false
	var epoch time.Time
	if !hostDispatchSpawnable(*disp) {
		if h.running[disp.ID] {
			return h.refuse(o, hdRefuseInFlight, "this process spawned the runner")
		}
		epoch = disp.UpdatedAt
		if disp.StartedAt != nil && disp.StartedAt.After(epoch) {
			epoch = *disp.StartedAt
		}
		if epoch.IsZero() || h.now().Sub(epoch) <= h.staleAfter {
			return h.refuse(o, hdRefuseInFlight, "dispatched within the liveness threshold")
		}
		switch h.probe(ctx, disp.ID) {
		case runnerDead:
		case runnerLive:
			return h.refuse(o, hdRefuseStaleRunnerLive, "a runner process for the stage is live but never flipped it running")
		default:
			return h.refuse(o, hdRefuseStaleRunnerUnknown, "runner liveness could not be probed")
		}
		if h.respawns == nil || !h.respawns.Allowed(disp.ID, epoch) {
			return h.refuse(o, hdRefuseStaleRespawnCap, "the stale re-spawn for this marker epoch was already used")
		}
		respawn = true
		o.Respawn = true
	}

	// Compose the argv before any POST: it reads no state the act or marker
	// changes, so an unsupported executor refuses with nothing committed.
	forgeTarget, ferr := h.r.resolveRunForgeTarget(ctx, runID)
	if ferr != nil {
		return h.refuse(o, hdRefuseForgeUnresolved, ferr.Error())
	}
	baseBranch, berr := forgeTarget.resolveSpawnBaseBranch(runID, "")
	if berr != nil {
		return h.refuse(o, hdRefuseBaseBranchUnresolved, berr.Error())
	}
	repo := forgeTarget.Repo
	if repo == "" {
		repo = run.Repo
	}
	isChild := run.SliceIndex != nil && disp.Type == "implement"
	var argv []string
	var agent string
	if isChild {
		a, aerr := stageRunnerAgent(run.ID, disp.ID, disp.Executor)
		if aerr != nil {
			return h.refuse(o, hdRefuseExecutorUnsupported, aerr.Error())
		}
		agent = a
		argv = childRunnerArgv(run.ID, disp.ID, h.r.api.baseURL, run.WorkflowID, run.WorkingDir, repo, forgeTarget, agent, baseBranch)
	} else {
		a, aerr := h.r.composeStageRunnerArgv(RunStageInput{
			RunID:      run.ID,
			Workflow:   run.WorkflowID,
			Stage:      disp.Type,
			WorkingDir: run.WorkingDir,
			GitHubRepo: repo,
			BaseBranch: baseBranch,
		}, *disp, repo, baseBranch, true, forgeTarget)
		if aerr != nil {
			return h.refuse(o, hdRefuseExecutorUnsupported, aerr.Error())
		}
		argv = a
	}

	// (1) Campaign-link read, fail closed.
	link, lerr := h.r.api.GetRunCampaignLink(ctx, runID)
	if lerr != nil {
		o.CampaignRefused = true
		return h.refuse(o, hdRefuseCampaignUnavailable, lerr.Error())
	}
	if code, detail := campaignPermits(link); code != "" {
		o.CampaignRefused = true
		return h.refuse(o, code, detail)
	}

	// (2) The gate: caps, ledger reservation and pause — the last local
	// predicate before any POST.
	if h.gate == nil {
		return h.refuse(o, hdRefuseGateUnconfigured, "no admission gate configured")
	}
	release, gcode, gerr := h.gate.Admit(ctx, hostDispatchCandidate{RunID: run.ID, StageID: disp.ID, StageType: disp.Type, Respawn: respawn})
	if gerr != nil {
		return h.refuse(o, "gate_failed", gerr.Error())
	}
	if gcode != "" {
		return h.refuse(o, gcode, "")
	}
	if release == nil {
		release = func() {}
	}
	fail := func(code, detail string) hostDispatchOutcome {
		release()
		delete(h.recorded, disp.ID)
		return h.refuse(o, code, detail)
	}

	if respawn {
		got, rerr := h.respawns.Reserve(disp.ID, epoch)
		if rerr != nil {
			return fail(hdRefuseRespawnLedgerFailed, rerr.Error())
		}
		if !got {
			return fail(hdRefuseStaleRespawnCap, "the stale re-spawn for this marker epoch was already used")
		}
	}

	// Acceptance admission and target check run after the campaign and pause
	// checks so a held campaign's acceptance stage is never settled here.
	var previewEnv []string
	if disp.Type == "acceptance" {
		admission, _, admitErr := h.r.maybeShortCircuitAcceptance(ctx, runID, stageUUID)
		if admitErr != nil {
			return fail(hdRefuseAcceptanceAdmission, admitErr.Error())
		}
		if admission != nil && admission.ShortCircuited {
			return fail(hdRefuseAcceptanceShortCircut, "the server settled the acceptance stage without a runner")
		}
		previewCmd, previewSource := resolveAcceptancePreviewCmd(h.r.getenv, true)
		if refusal, _ := h.r.checkAcceptanceTarget(ctx, admission, acceptanceTargetOpts{
			workingDir: run.WorkingDir, previewCmd: previewCmd, previewSource: previewSource,
		}); refusal != nil {
			return fail(hdRefuseAcceptanceNeedsTarget, refusal.Detail)
		}
		if previewSource == "auto_preview" {
			previewEnv = append(previewEnv, acceptancePreviewCmdEnv+"="+previewCmd)
			if teardownCmd, teardownSource := resolveAcceptancePreviewTeardownCmd(h.r.getenv, previewSource); teardownSource == "auto_preview" {
				previewEnv = append(previewEnv, acceptancePreviewTeardownCmdEnv+"="+teardownCmd)
			}
		}
	}

	// (3) Record run_auto_driven, fail closed — once per queue episode.
	if !h.recorded[disp.ID] {
		note := ""
		if respawn {
			note = staleRedispatchNote
		}
		if _, rerr := h.r.api.RecordAutoDriveAct(ctx, runID, RecordAutoDriveAct{
			Action: autoDriveDispatchActionName,
			Stage:  disp.Type,
			Source: hostdispatcher.SourceTag,
			Note:   note,
		}); rerr != nil {
			return fail(hdRefuseRecordFailed, rerr.Error())
		}
		h.recorded[disp.ID] = true
	}

	// (4) The host-dispatch marker.
	hdr, hderr := h.r.api.HostDispatchStage(ctx, runID, stageUUID)
	if hderr != nil {
		if _, queued := asConcurrencySlotQueued(hderr); queued {
			release()
			o.QueueEpisode = true
			return h.refuse(o, hdRefuseSlotQueued, hderr.Error())
		}
		if asHostMemoryPressure(hderr) {
			o.StopMarkers = true
			return fail(hdRefuseMemoryPressure, hderr.Error())
		}
		code := hdRefuseMarkerFailed
		var ae *apiError
		if errors.As(hderr, &ae) && ae.Code != "" {
			code = ae.Code
		}
		return fail(code, hderr.Error())
	}
	delete(h.recorded, disp.ID)
	if !hdr.Transitioned && !respawn {
		release()
		return h.refuse(o, hdRefuseMarkerNotTransitioned, "another dispatcher won the marker")
	}
	if isChild && hdr.BaseBranch != "" {
		argv = childRunnerArgv(run.ID, disp.ID, h.r.api.baseURL, run.WorkflowID, run.WorkingDir, repo, forgeTarget, agent, hdr.BaseBranch)
	}

	env := append(hostDispatchRunnerEnv(h.environ(), h.r.api.token), previewEnv...)
	report := func(ctx context.Context, category, reason, detail string, exitCode int) error {
		_, e := h.r.api.ReportStageFailure(ctx, runID, stageUUID, category, reason, detail, exitCode)
		return e
	}
	probe := h.r.stageStateProbe(runID, disp.ID)
	logPath, spawnErr := h.spawn(h.binary, argv, env, run.ID, disp.ID, report, probe)
	if spawnErr != nil {
		release()
		if errors.Is(spawnErr, errRunCancelledBeforeSpawn) {
			return h.refuse(o, hdRefuseSpawnCancelled, spawnErr.Error())
		}
		// The marker committed 'dispatched' before the spawn, and a failed
		// spawn has no reaper: report the category-C failure so the stage is
		// retryable rather than stranded (the run_children compensation).
		detail := spawnErr.Error()
		if rerr := report(ctx, "C", "runner_spawn_failed", detail, 0); rerr != nil {
			detail += "; the category-C failure report also failed: " + rerr.Error()
		}
		return h.refuse(o, hdRefuseSpawnFailed, detail)
	}
	h.running[disp.ID] = true
	o.Spawned = true
	o.LogPath = logPath
	return o
}
