package main

// StageWaitStatus is the lifecycle summary the MCP surface derives for one
// stage's EXECUTION (distinct from ReviewStatus, which summarizes a stage's
// review). It is the near-term, no-external-dependency half of ADR-037 (#879,
// #880): a crisp terminal-status contract on the durable (run_id, stage_id)
// handle plus a server-suggested poll cadence, mirroring the ReviewStatus /
// fishhawk_await_review pattern (#878).
//
// Status is one of:
//
//   - "pending"   — the stage exists but has not started running yet
//     (backend state pending | dispatched | awaiting_approval |
//     awaiting_children). A polling agent should keep calling
//     fishhawk_get_run_status until a terminal status lands.
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
type StageWaitStatus struct {
	Stage               string `json:"stage" jsonschema:"the stage type: 'plan', 'implement', or 'review'"`
	Status              string `json:"status" jsonschema:"one of pending, running, succeeded, failed, cancelled"`
	PollIntervalSeconds int    `json:"poll_interval_seconds,omitempty" jsonschema:"server-suggested cadence (seconds) for re-polling fishhawk_get_run_status while status is non-terminal (pending/running); present only while non-terminal, omitted on terminal. Poll get_run_status on this cadence as the authoritative path to a terminal stage status"`
}

// suggestedStageWaitPollIntervalSeconds is the server-suggested cadence a
// polling agent should use to re-poll fishhawk_get_run_status while a stage is
// still executing (#879/#880). Deliberately coarser than reviews' 15s cadence:
// stages run 6–13 min with ~15s heartbeats, so a 30s cadence balances freshness
// against round-trips. Advertised on StageWaitStatus.PollIntervalSeconds while
// the status is non-terminal.
const suggestedStageWaitPollIntervalSeconds = 30

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
// Non-terminal states (pending | dispatched | awaiting_approval |
// awaiting_children) map to "pending"; running maps to "running"; the three
// terminal states map to themselves. A non-terminal status carries the
// suggested poll interval; a terminal status omits it.
func classifyStageWaitStatus(stageType, stageState, runState string) *StageWaitStatus {
	status := "pending"
	switch stageState {
	case "running":
		status = "running"
	case "succeeded", "failed", "cancelled":
		status = stageState
	}

	st := &StageWaitStatus{Stage: stageType, Status: status}
	if !stageStateIsTerminal(stageState) && !runStateIsTerminal(runState) {
		st.PollIntervalSeconds = suggestedStageWaitPollIntervalSeconds
	}
	return st
}

// stageWaitStatusFor resolves the StageWaitStatus for the requested stage type
// from an ALREADY-FETCHED stage list (no backend round-trip — callers pass the
// slice get_run_status / run_stage already hold, so the wait derivation never
// re-issues ListRunStages). Returns nil when no stage of that type exists
// (matching ReviewStatus's 'none' shape). runState is the parent run's state
// for the ADR-036 backstop; pass "" when unknown.
func stageWaitStatusFor(stages []Stage, stageType, runState string) *StageWaitStatus {
	for _, s := range stages {
		if s.Type == stageType {
			return classifyStageWaitStatus(stageType, s.State, runState)
		}
	}
	return nil
}
