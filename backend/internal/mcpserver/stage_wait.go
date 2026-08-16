package mcpserver

import "time"

// StageWaitStatus is the lifecycle summary the MCP surface derives for one
// stage's EXECUTION (distinct from ReviewStatus, which summarizes a stage's
// review). It is the near-term, no-external-dependency half of ADR-037 (#879,
// #880): a crisp terminal-status contract on the durable (run_id, stage_id)
// handle plus a server-suggested poll cadence, mirroring the ReviewStatus /
// fishhawk_await_review pattern (#878).
//
// TWO VOCABULARIES, ONE MAPPING (#2494). The REST surface reports a stage's
// RAW backend `state` — the thirteen run.StageState values; this MCP surface
// reports a coarser wait-status BUCKET (five values). They are deliberately
// different — the bucket answers "should I keep polling?" — and neither is
// renamed, because renaming either is a breaking wire change for every
// existing consumer. The mapping is total and is the ONE place it is written
// down; the REST companion docs (docs/api/v0.openapi.yaml, docs/api/v0.md)
// point back here.
//
//	backend stage state       -> stage_wait_status.status
//	------------------------------------------------------
//	pending                   -> pending
//	awaiting_host_dispatch    -> pending
//	dispatched                -> pending
//	awaiting_approval         -> pending
//	awaiting_children         -> pending
//	awaiting_input            -> pending
//	awaiting_scope_decision   -> pending
//	awaiting_deploy_approval  -> pending
//	awaiting_deployment       -> pending
//	running                   -> running
//	succeeded                 -> succeeded
//	failed                    -> failed
//	cancelled                 -> cancelled
//
// The table enumerates every run.StageState that exists today, but the
// mapping is stated as a RULE so it stays total as the backend grows:
// `running` maps to "running", the three terminal states map to themselves,
// and EVERY other state — including one added after this comment was written
// — falls to "pending", the conservative keep-polling default, so a new
// backend state never strands a caller on a bucket it does not recognize.
// stage_wait_test.go's TestClassifyStageWaitStatus_MappingTable pins the table
// against the run.StageState constants themselves, so a renamed state breaks
// the build and a re-bucketed one fails the test.
//
// Status is one of:
//
//   - "pending"   — the stage exists but has not started running yet
//     (every non-`running`, non-terminal backend state: pending |
//     awaiting_host_dispatch | dispatched | awaiting_approval |
//     awaiting_children | awaiting_input | awaiting_scope_decision |
//     awaiting_deploy_approval | awaiting_deployment).
//     awaiting_host_dispatch (#1912) is
//     a runner_kind-locked-local agent stage parked for a host spawn — it is
//     actionable (a host dispatch will start it), so it maps to the pending
//     (keep-polling) bucket, not a terminal one. A polling agent should keep
//     calling fishhawk_get_run_status until a terminal status lands.
//   - "running"   — the runner is executing the stage (backend state
//     running). Keep polling.
//   - "succeeded" — terminal: the stage completed successfully.
//   - "failed"    — terminal: the stage failed.
//   - "cancelled" — terminal: the stage was cancelled.
//
// PollIntervalSeconds is a server-suggested poll cadence: it is populated
// ONLY while the status is non-terminal (pending/running) — the states where
// a polling agent should keep calling fishhawk_get_run_status — and omitted
// (zero) on every terminal status. Polling get_run_status on this cadence is
// the authoritative way to await a stage's terminal status. As an ADR-036
// (#874) backstop, the interval is also dropped when the parent run has
// already reached a terminal state while the stage row is still non-terminal:
// the stage can no longer progress, so advertising an unbounded poll would
// strand the caller.
//
// The value is DERIVED, not a constant (E48.62 / #2489): a quarter of the
// stage's REMAINING predicted runtime when the run carries an approved plan's
// predicted_runtime_minutes, otherwise a quarter of its ELAPSED runtime, in
// both cases clamped to [suggestedStageWaitPollIntervalSeconds,
// stageWaitPollCeilingSeconds]. See stageWaitPollIntervalSeconds.
type StageWaitStatus struct {
	Stage               string `json:"stage" jsonschema:"the stage type: 'plan', 'implement', 'review', or 'acceptance'"`
	Status              string `json:"status" jsonschema:"one of pending, running, succeeded, failed, cancelled. This is a coarser BUCKET than the raw stage 'state' the REST API reports; the mapping is total: pending -> pending, awaiting_host_dispatch -> pending, dispatched -> pending, awaiting_approval -> pending, awaiting_children -> pending, awaiting_input -> pending, awaiting_scope_decision -> pending, awaiting_deploy_approval -> pending, awaiting_deployment -> pending, running -> running, succeeded -> succeeded, failed -> failed, cancelled -> cancelled. Any state added later also buckets to pending (the conservative keep-polling default)"`
	PollIntervalSeconds int    `json:"poll_interval_seconds,omitempty" jsonschema:"server-suggested cadence (seconds) for re-polling fishhawk_get_run_status while status is non-terminal (pending/running); present only while non-terminal, omitted on terminal. Poll get_run_status on this cadence as the authoritative path to a terminal stage status"`
	// ElapsedSeconds / AgentTimeoutSeconds / DeadlineSecondsRemaining are the
	// stage's remaining-budget observability (#2540): how long the stage has been
	// running, the spec-resolved agent wall clock the runner enforces, and the
	// budget left before the runner SIGKILLs it. Populated ONLY while non-terminal
	// (a terminal stage has no remaining budget, mirroring PollIntervalSeconds) and
	// ONLY when the backend resolved a positive agent_timeout_seconds. They let an
	// operator see whether a mid-stage scope amendment can still be decided in time
	// — a filed amendment needs at least the ~15-minute poll window of remaining
	// budget to be decidable; less, and the race is already lost.
	//
	// DeadlineSecondsRemaining is a POINTER so a genuine 0 (the deadline-reached
	// case, clamped up from a negative) is distinguishable from "unknown" (nil,
	// when the budget was unresolved). ElapsedSeconds/AgentTimeoutSeconds are
	// omitempty scalars — absent when the budget is unknown or the stage terminal.
	ElapsedSeconds           int  `json:"elapsed_seconds,omitempty" jsonschema:"seconds the stage has been running (from its server-recorded started_at); present only while non-terminal and when the agent wall clock is known"`
	AgentTimeoutSeconds      int  `json:"agent_timeout_seconds,omitempty" jsonschema:"the spec-resolved agent wall clock (seconds) the runner enforces for this stage; present only while non-terminal and when resolved"`
	DeadlineSecondsRemaining *int `json:"deadline_seconds_remaining,omitempty" jsonschema:"seconds of agent budget left before the runner kills the stage (agent_timeout_seconds - elapsed_seconds, clamped at 0); present only while non-terminal and when the budget is known. A filed scope amendment needs at least the amendment poll window of remaining budget to be decidable"`
	// LastEvent / TurnsThisAttempt / TokensThisAttempt are the stage's
	// mid-execution progress heartbeat (#2541): the agent's last event kind and
	// the turn/token counters from the runner's most recent stage_progress
	// report, so a poll during a running stage shows real activity instead of a
	// single 'running' bit. Populated ONLY while non-terminal and ONLY when the
	// stage has reported a heartbeat; a stage with neither budget nor progress
	// keeps the byte-identical pre-#2540 shape. The counters are per-attempt and
	// RESET on an in-driver re-spawn — elapsed_seconds is the cumulative,
	// re-spawn-monotonic value (it is derived from started_at, not from the
	// heartbeat).
	//
	// TurnsThisAttempt / TokensThisAttempt are POINTERS so an explicit reported
	// ZERO (a valid heartbeat's first turn, or a fresh attempt after a re-spawn)
	// is preserved on the wire rather than dropped by omitempty (#2541 fix-up):
	// a plain omitempty int cannot distinguish "progress reported, count is 0"
	// from "no progress", and the contract is that a heartbeat POPULATES the
	// per-attempt counters. They are non-nil (and so serialized, even at 0) iff
	// progress is present on a non-terminal stage; nil — hence omitted — when
	// progress is absent or the stage is terminal.
	LastEvent         string `json:"last_event,omitempty" jsonschema:"the agent's last event kind from the most recent progress heartbeat (e.g. 'assistant'); present only while non-terminal and when the stage has reported progress"`
	TurnsThisAttempt  *int   `json:"turns_this_attempt,omitempty" jsonschema:"parsed-event count so far in the CURRENT agent attempt; resets on an in-driver re-spawn. present (including an explicit 0) only while non-terminal and when the stage has reported progress"`
	TokensThisAttempt *int   `json:"tokens_this_attempt,omitempty" jsonschema:"cumulative token count so far in the CURRENT agent attempt; resets on an in-driver re-spawn. present (including an explicit 0) only while non-terminal and when the stage has reported progress"`
}

// suggestedStageWaitPollIntervalSeconds is the FLOOR of the derived stage-wait
// poll cadence — the tightest interval the surface will ever advertise, and the
// value it advertises when there is nothing to derive from (#879/#880, E48.62 /
// #2489). Deliberately coarser than reviews' 15s cadence: stages run with ~15s
// heartbeats, so 30s balances freshness against round-trips.
//
// It is also the byte-identical-to-pre-#2489 case: a stage with neither a
// prediction nor any elapsed time (a freshly created, never-started stage)
// derives a raw 0 and clamps to exactly this constant, so nothing that polled
// at 30s before polls faster now.
const suggestedStageWaitPollIntervalSeconds = 30

// stageWaitPollCeilingSeconds is the CEILING of the derived cadence (15
// minutes, E48.62 / #2489). Two bounds fix it: an operator must learn a stage
// went terminal within a quarter-hour, and the runner's 3600s agent wall clock
// guarantees at least four observations per stage even at the widest interval.
const stageWaitPollCeilingSeconds = 900

// stageWaitPollDivisor splits the horizon the cadence is derived from (the
// REMAINING predicted runtime when a prediction exists, otherwise the ELAPSED
// runtime) into four polls. Four is the smallest count that still resolves a
// stage's terminal transition to within a quarter of its own runtime while
// cutting a 115-predicted-minute fan-out from ~190 polls to ~7.
const stageWaitPollDivisor = 4

// stageWaitPollIntervalSeconds is the pure derivation core: the cadence to
// advertise for a non-terminal stage, given the run's approved-plan prediction
// (0 when unstamped) and the stage's elapsed runtime.
//
// Two branches:
//
//   - predictedMinutes > 0 — a quarter of the REMAINING predicted runtime, so
//     the cadence tightens as the stage approaches its own prediction.
//   - otherwise — a quarter of the ELAPSED runtime. This is the plan stage's
//     branch by construction (the plan does not exist while it runs, so no
//     prediction can be stamped), and it is what stops a long plan stage
//     sitting at 30s: a stage 22 minutes in advertises ~5.5 minutes.
//
// Both branches clamp to [suggestedStageWaitPollIntervalSeconds,
// stageWaitPollCeilingSeconds]. Every degenerate input lands on the floor BY
// CONSTRUCTION rather than by a special case: no prediction and no elapsed
// (raw 0), a negative elapsed from clock skew, and an OVERRUNNING stage whose
// remaining has gone negative. The last is the behaviour you want — a stage
// past its own prediction is the one worth polling tightly.
func stageWaitPollIntervalSeconds(predictedMinutes int, elapsed time.Duration) int {
	elapsedSeconds := int(elapsed.Seconds())
	var raw int
	if predictedMinutes > 0 {
		raw = (predictedMinutes*60 - elapsedSeconds) / stageWaitPollDivisor
	} else {
		raw = elapsedSeconds / stageWaitPollDivisor
	}
	if raw < suggestedStageWaitPollIntervalSeconds {
		return suggestedStageWaitPollIntervalSeconds
	}
	if raw > stageWaitPollCeilingSeconds {
		return stageWaitPollCeilingSeconds
	}
	return raw
}

// stageElapsed is how long a stage has been running as of now, derived
// SERVER-SIDE from the stage's own started_at rather than from any
// runner-reported counter. Returns zero for a stage that has not started (nil
// StartedAt) and for a now that precedes started_at (clock skew), so the
// derivation's caller never sees a negative span.
//
// stages.started_at is written under COALESCE(started_at, $3), so it is set
// once on first start and never overwritten: elapsed is cumulative and
// monotonic across in-driver re-spawns. The consequence is deliberate — after
// a retry_stage, elapsed still counts from the ORIGINAL start, so a retried
// stage's advertised interval is wider than its fresh runtime alone would
// suggest. The cadence is a heuristic and the ceiling bounds the error at 900s.
func stageElapsed(startedAt *time.Time, now time.Time) time.Duration {
	if startedAt == nil || now.Before(*startedAt) {
		return 0
	}
	return now.Sub(*startedAt)
}

// derivedStageWaitPollInterval is the convenience wrapper the next-actions arms
// call: the derived cadence for one stage of one run, read against the current
// clock. A nil run or a nil stage yields the floor — the honest answer when
// there is nothing to derive from.
func derivedStageWaitPollInterval(run *Run, stage *Stage) int {
	if run == nil || stage == nil {
		return suggestedStageWaitPollIntervalSeconds
	}
	return stageWaitPollIntervalSeconds(
		run.PredictedRuntimeMinutes,
		stageElapsed(stage.StartedAt, time.Now().UTC()),
	)
}

// stageDeadlineRemaining derives the agent budget left for a stage (#2540):
// agentTimeoutSeconds minus elapsed, as a POINTER so the deadline-reached case
// (0) is distinguishable from unknown (nil). It returns nil — "unknown" — when
// agentTimeoutSeconds <= 0 (the backend could not resolve the spec budget, or an
// older backend omitted the field), and otherwise CLAMPS a negative remainder to
// 0 so an OVERRUNNING stage reports 0 rather than a nonsensical negative. Pure
// and table-testable; the caller supplies elapsed.
func stageDeadlineRemaining(agentTimeoutSeconds int, elapsed time.Duration) *int {
	if agentTimeoutSeconds <= 0 {
		return nil
	}
	remaining := agentTimeoutSeconds - int(elapsed.Seconds())
	if remaining < 0 {
		remaining = 0
	}
	return &remaining
}

// stageStateIsTerminal reports whether a backend stage state is one past which
// the stage can no longer make progress. The terminal set —
// succeeded / failed / cancelled — is compared INLINE here against the
// fishhawk-mcp-local Stage.State string (client.go); the backend's
// run.StageState type and its IsTerminal() method are deliberately NOT
// imported, mirroring review.go's runStateIsTerminal (which avoided #875's
// compile trap by not depending on backend/internal/run).
func stageStateIsTerminal(state string) bool {
	switch state {
	case "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// classifyStageWaitStatus maps a stage's backend state into a StageWaitStatus
// for the given stage type. runState carries the parent run's state for the
// ADR-036 (#874) backstop: when the run is already terminal but the stage row
// is still non-terminal, the suggested poll interval is dropped so the wait
// resolves rather than advertising an unbounded poll (pass "" when the run
// state is unknown — the backstop simply does not fire).
//
// Non-terminal states (pending | awaiting_host_dispatch | dispatched |
// awaiting_approval | awaiting_children) map to "pending"; running maps to
// "running"; the three terminal states map to themselves. A non-terminal status
// carries the DERIVED poll interval (see stageWaitPollIntervalSeconds); a
// terminal status omits it, as does the ADR-036 run-terminal backstop.
//
// startedAt / predictedMinutes / now are the derivation's three inputs (E48.62
// / #2489): the stage's own start (nil for a never-started stage), the run's
// stamped approved-plan prediction (0 when unstamped), and the clock to
// measure elapsed against — passed in rather than read here so every wait
// status on one snapshot agrees and the derivation stays purely testable.
func classifyStageWaitStatus(stageType, stageState, runState string, startedAt *time.Time, predictedMinutes int, now time.Time) *StageWaitStatus {
	status := "pending"
	switch stageState {
	case "running":
		status = "running"
	case "succeeded", "failed", "cancelled":
		status = stageState
	}

	st := &StageWaitStatus{Stage: stageType, Status: status}
	if !stageStateIsTerminal(stageState) && !runStateIsTerminal(runState) {
		st.PollIntervalSeconds = stageWaitPollIntervalSeconds(predictedMinutes, stageElapsed(startedAt, now))
	}
	return st
}

// stageWaitStatusFor resolves the StageWaitStatus for the requested stage type
// from an ALREADY-FETCHED stage list (no backend round-trip — callers pass the
// slice get_run_status / run_stage already hold, so the wait derivation never
// re-issues ListRunStages). Returns nil when no stage of that type exists
// (matching ReviewStatus's 'none' shape). runState is the parent run's state
// for the ADR-036 backstop; pass "" when unknown.
//
// predictedMinutes is the run's stamped approved-plan prediction (0 when
// unstamped) and now is the clock elapsed is measured against; the matched
// stage supplies its own started_at. Callers that hold several wait statuses
// from one snapshot should capture now ONCE and pass the same value to each,
// so the advertised cadences agree.
// It also folds the stage's remaining-budget observability (#2540) onto the
// resolved status: for a NON-terminal stage it reads the matched Stage's
// agent_timeout_seconds and derives elapsed + deadline_seconds_remaining. This
// is done HERE rather than inside classifyStageWaitStatus deliberately: the two
// DIRECT classifyStageWaitStatus callers (await_stage.go, run_stage.go) both
// classify SETTLED/terminal stages, whose deadline fields are omitted anyway, so
// threading the budget through classify's signature would break an out-of-scope
// caller for no observable gain. The non-terminal guard mirrors the one classify
// uses for PollIntervalSeconds, so the deadline fields appear on exactly the
// statuses the poll cadence does.
func stageWaitStatusFor(stages []Stage, stageType, runState string, predictedMinutes int, now time.Time) *StageWaitStatus {
	for _, s := range stages {
		if s.Type == stageType {
			st := classifyStageWaitStatus(stageType, s.State, runState, s.StartedAt, predictedMinutes, now)
			if !stageStateIsTerminal(s.State) && !runStateIsTerminal(runState) {
				elapsed := stageElapsed(s.StartedAt, now)
				st.AgentTimeoutSeconds = s.AgentTimeoutSeconds
				st.DeadlineSecondsRemaining = stageDeadlineRemaining(s.AgentTimeoutSeconds, elapsed)
				// Mid-execution progress heartbeat (#2541): project the per-attempt
				// counters + last_event onto the wait status. elapsed_seconds is NOT
				// taken from the heartbeat — it stays the started_at derivation, so it
				// is cumulative and monotonic across in-driver re-spawns.
				if s.Progress != nil {
					st.LastEvent = s.Progress.LastEvent
					// Copy into locals and take their addresses so an explicit 0
					// counter is preserved on the wire (pointer fields, above) —
					// a reported zero is "progress present, count 0", not absent.
					turns := s.Progress.TurnsThisAttempt
					tokens := s.Progress.TokensThisAttempt
					st.TurnsThisAttempt = &turns
					st.TokensThisAttempt = &tokens
				}
				// ElapsedSeconds is meaningful alongside a known budget OR a reported
				// heartbeat; its population condition widens from `budget > 0` to
				// `budget > 0 || progress present` (#2541) so a stage with progress but
				// an unresolved budget still reports elapsed, while a stage with NEITHER
				// keeps the byte-identical pre-#2540 shape.
				if s.AgentTimeoutSeconds > 0 || s.Progress != nil {
					st.ElapsedSeconds = int(elapsed.Seconds())
				}
			}
			return st
		}
	}
	return nil
}
