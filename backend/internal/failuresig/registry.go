package failuresig

// The failure-reason ANCHORS. Each is a substring of a literal some component
// of this repository provably emits; the emitting site is named per anchor.
//
// These are BEST-EFFORT STRING CONTRACTS, not typed plumbing. The runner is a
// separate go.work module and cannot share a constant with the backend (the
// #1548 limitation the pre-existing next_actions phrases already carry), so a
// runner-side wording change silently stops a matcher firing. The failure mode
// is fail-open — no match means today's behaviour, never a wrong hint — and
// this package is the SINGLE place the backend declares them, so a second
// diverging copy cannot appear inside the backend module.
const (
	// AnchorExternalAPIError is emitted by the runner's claudecode adapter as
	// "terminal external API error <N> (retries exhausted): …"
	// (runner/internal/agent/claudecode/claudecode.go).
	AnchorExternalAPIError = "terminal external API error "

	// AnchorQuotaUnavailable is emitted by the runner's claudecode adapter as
	// "could not obtain model quota (likely a usage/rate cap): …" (#2085,
	// runner/internal/agent/claudecode/claudecode.go).
	AnchorQuotaUnavailable = "could not obtain model quota"

	// AnchorVerifyInfraFlake is the runner trace-event name for a verify-gate
	// infra flake the runner absorbed by re-running the gate in place. When it
	// resurfaces in a failure detail, the absorbed flake recurred.
	AnchorVerifyInfraFlake = "verify_infra_flake_retry"

	// AnchorSliceIntegrationConflict is the stable prefix the decomposition
	// fan-in stamps on the PARENT implement stage's failure reason (ADR-041 /
	// #1142). MUST match
	// backend/internal/orchestrator.sliceIntegrationConflictReasonPrefix
	// (orchestrator.go:1855).
	AnchorSliceIntegrationConflict = "slice integration conflict"

	// AnchorLineageLock is the runner's runner_failed reason for a lineage-lock
	// refusal — `{"event":"runner_failed","reason":"lineage_lock",…}` at
	// runner/cmd/fishhawk-runner/main.go:779 — relayed verbatim into the
	// stage's failure_reason by the reap-failure endpoint
	// (backend/internal/server/reap_failure.go -> run.FailStage).
	AnchorLineageLock = "lineage_lock"

	// AnchorRunnerExitedBeforeReporting is the reaper's synthesized reason for
	// a runner that exited non-zero without settling its stage:
	// "runner exited %d before reporting a terminal state"
	// (backend/internal/mcpserver/run_stage.go:1297, reapDetachedRunner).
	AnchorRunnerExitedBeforeReporting = "before reporting a terminal state"

	// AnchorZeroExitStrand is the #2630 reaper's synthesized reason for a
	// runner that exited ZERO having done nothing:
	// "runner exited 0 without settling the stage (state=%s)"
	// (backend/internal/mcpserver/run_stage.go:1336, reapZeroExitStrand).
	AnchorZeroExitStrand = "runner exited 0 without settling the stage"
)

// Failure categories this catalog keys on.
const (
	categoryAgent   = "A"
	categoryRefusal = "C"
)

// Registry returns the catalog in PRECEDENCE order, most specific first.
// Match walks it first-match-wins, so the order is behaviour, not
// presentation: an entry placed after one whose evidence it overlaps can never
// fire on the overlapping shape.
//
// A fresh slice is returned per call so no caller can mutate the catalog. The
// catalog is small and the call sits on a display path, so the allocation is
// not worth trading for shared mutable state.
func Registry() []Signature {
	return []Signature{
		{
			ID:    "external_api_incident",
			Title: "Terminal external API error (upstream incident)",
			Means: "The agent's model provider returned a terminal error after exhausting its retries — most often a 529 overload. This is an upstream platform incident, not a defect in the task.",
			Playbook: []string{
				"check status.claude.com (or the provider's status page) for an active incident",
				"back off until the incident clears — an immediate retry re-hits the same incident and burns retry budget",
				"then fishhawk_retry_stage to retry the stage in place",
			},
			match: func(ev Evidence) bool { return cites(ev.FailureReason, AnchorExternalAPIError) },
		},
		{
			ID:    "model_quota_exhausted",
			Title: "Model quota exhausted",
			Means: "The agent could not obtain model quota — a usage or rate cap, not a transient crash. The stage will fail identically until the cap resets.",
			Playbook: []string{
				"confirm the cap from the failure reason — the agent made no model call (0 tokens)",
				"wait for the usage window to reset rather than burning retry budget against the wall",
				"then fishhawk_retry_stage",
			},
			match: func(ev Evidence) bool { return cites(ev.FailureReason, AnchorQuotaUnavailable) },
		},
		{
			ID:    "slice_integration_conflict",
			Title: "Slice integration conflict during fan-in",
			Means: "A decomposed parent's fan-in could not merge one slice branch onto the consolidated branch. The consolidated branch already holds the earlier slices, so the parent is not the thing to re-drive.",
			Playbook: []string{
				"read conflicting_child_run_id from the newest slice_integration_conflict audit entry's structured payload — never parse it out of the reason string",
				"fishhawk_resume_run pointed at THAT CHILD's run id to re-drive only the conflicting slice in place",
				"do NOT point resume at the parent: it replans from scratch and discards the succeeded sibling slices",
			},
			match: func(ev Evidence) bool { return cites(ev.FailureReason, AnchorSliceIntegrationConflict) },
		},
		{
			ID:    "lineage_lock_contention",
			Title: "Lineage lock contention",
			Means: "The runner refused to start because another runner already holds this run's lineage lock — two runners were pointed at the same lineage, or a previous runner's lock outlived it.",
			Playbook: []string{
				"confirm no live runner still holds the lock: pgrep -f fishhawk-runner",
				"if one IS live, wait for it — a second runner into the same lineage will refuse again",
				"dispatch decomposition children SEQUENTIALLY, not concurrently: a concurrent same-lineage dispatch is the usual cause",
				"then fishhawk_retry_stage once the lock is clear",
			},
			match: func(ev Evidence) bool {
				return ev.FailureCategory == categoryRefusal && cites(ev.FailureReason, AnchorLineageLock)
			},
		},
		{
			ID:    "zero_exit_strand",
			Title: "Runner exited 0 without settling the stage",
			Means: "The runner exited successfully having settled nothing — it re-entered a phase that had already completed, did no work, and left the stage stranded (#2630). A sticky scope-completeness exemption is the known cause.",
			Playbook: []string{
				"read the runner log for the dispatch: a strand looks like a very short (~seconds) run with no runner_completed event",
				"a retry usually re-runs the same no-op — do not spend more than one",
				"if it recurs, fishhawk_cancel_run and start a fresh run rather than retrying into the same strand",
			},
			match: func(ev Evidence) bool { return cites(ev.FailureReason, AnchorZeroExitStrand) },
		},
		{
			ID:    "runner_died_before_reporting",
			Title: "Runner died before reporting a terminal state",
			Means: "The spawned runner exited non-zero without ever reporting a terminal stage state, so the backend reaped the stage on its behalf. The real cause is in the runner log, not in the stage's failure reason.",
			Playbook: []string{
				"read the dispatch's log_path — the runner's own last lines carry the real cause",
				"check the host for a crash the runner could not report (out of memory, a killed process, a missing binary)",
				"then fishhawk_retry_stage to re-spawn in place",
			},
			match: func(ev Evidence) bool { return cites(ev.FailureReason, AnchorRunnerExitedBeforeReporting) },
		},
		{
			ID:    "infra_flake_recurred",
			Title: "Absorbed infra flake recurred",
			Means: "The stage's verify gate hit an infrastructure flake, absorbed one in-place re-run, and the flake recurred — so the failure is the environment, not the change.",
			Playbook: []string{
				"fishhawk_retry_stage — a recurring absorbed flake is the cheapest thing to retry",
				"if it recurs again, check the local Docker daemon / testcontainers state before spending a third retry",
			},
			match: func(ev Evidence) bool { return cites(ev.FailureReason, AnchorVerifyInfraFlake) },
		},
		{
			ID:    "agent_no_progress_repeat",
			Title: "Agent made no progress on a repeat attempt",
			Means: "A repeat attempt failed category-A while its stage_progress heartbeat reported zero turns and zero tokens — the agent never got going, so this is a harness or provider-side stall rather than a hard task.",
			Playbook: []string{
				"check the provider status page before spending another retry — a zero-token attempt rarely means a hard task",
				"read the stage trace for a harness error (a 400 on the first turn, a zero-token hang)",
				"then fishhawk_retry_stage; if a third attempt also reports zero turns, stop retrying and read the runner log",
			},
			// Counter-anchored, NOT string-anchored: this signature has no
			// failure-reason literal to key on, so it reads the runner's
			// stage_progress counters plus the run's retry attempt.
			// ProgressReported gates the counters so an ABSENT heartbeat is
			// never read as observed inactivity, and RetryAttempt > 0 is what
			// makes it a REPEAT — a first attempt that reported zero turns has
			// simply not got going yet.
			match: func(ev Evidence) bool {
				return ev.FailureCategory == categoryAgent &&
					ev.ProgressReported &&
					ev.TurnsThisAttempt == 0 &&
					ev.TokensThisAttempt == 0 &&
					ev.RetryAttempt > 0
			},
		},
	}
}
