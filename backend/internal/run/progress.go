package run

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	rundb "github.com/kuhlman-labs/fishhawk/backend/internal/run/db"
)

// StageProgress is the runner's mid-execution stage_progress heartbeat,
// projected onto the stage row so an operator poll returns last_event / turns
// / tokens instead of a single 'running' bit (E48.96 / #2541).
//
// PER-ATTEMPT vs CUMULATIVE. TurnsThisAttempt and TokensThisAttempt are
// cumulative WITHIN THE CURRENT AGENT ATTEMPT ONLY: they reset when the driver
// re-spawns the agent mid-stage (the observed 9, 9, 5 turn series). The
// per-attempt semantics live in the field NAMES on the wire, not only in this
// comment. Elapsed is deliberately NOT carried here — the operator-facing
// elapsed_seconds is derived server-side from the stage row's started_at, which
// makes it cumulative and monotonic across in-driver re-spawns by construction.
//
// ReportedAt is stamped by the DATABASE at ingest — Postgres now(), inside
// RecordStageProgress's UPDATE — not by the backend process and not by the
// runner. A caller-supplied ReportedAt is DISCARDED on write: the statement's
// jsonb_set OVERWRITES the key, so the value read back is always the DB's
// instant. The reason is clock domains (#3084): it shares one with
// stages.dispatched_at (migration 0072's trigger, also now()), which makes
// ListDispatchedStageLiveness's attempt-relative comparison single-domain and
// correct by construction rather than dependent on two hosts agreeing.
//
// The JSONB column is nullable (migration 0070): nil for every stage that has
// not reported and every stage on a run predating the column.
type StageProgress struct {
	// LastEvent is the agent's last event kind from the most recent heartbeat
	// (e.g. "assistant"). Clamped to a fixed rune length at ingest.
	LastEvent string `json:"last_event"`
	// TurnsThisAttempt is the parsed-event count so far in the CURRENT agent
	// attempt; it resets on an in-driver re-spawn.
	TurnsThisAttempt int `json:"turns_this_attempt"`
	// TokensThisAttempt is the cumulative token count so far in the CURRENT
	// agent attempt; it resets on an in-driver re-spawn.
	TokensThisAttempt int `json:"tokens_this_attempt"`
	// ReportedAt is the DATABASE-stamped ingest time of this heartbeat. A value
	// set here by a caller is discarded on write (see the type doc).
	ReportedAt time.Time `json:"reported_at"`
}

// StageProgressStore is the OPTIONAL capability the server's progress ingest
// and stage-read handlers use to record and read a stage's heartbeat. It is
// deliberately NOT part of the Repository interface — adding it would fan out
// across the 23 Repository implementations (only the postgres one and the
// server consumer are in scope here), exactly the ScopeCompletenessPark /
// runCostRecorder / runnerKindResolver precedent. The postgres repo implements
// it; a RunRepo that does not satisfy it answers 503 progress_unsupported.
//
// RecordStageProgress returns applied=false (no error) when the stage was
// terminal and the UPDATE matched zero rows — the terminal refusal IS the
// query's own WHERE-state predicate, so there is no read-then-write window
// (#2536).
type StageProgressStore interface {
	RecordStageProgress(ctx context.Context, stageID uuid.UUID, p StageProgress) (bool, error)
	StageProgressByID(ctx context.Context, stageID uuid.UUID) (*StageProgress, error)
	StageProgressForRun(ctx context.Context, runID uuid.UUID) (map[uuid.UUID]StageProgress, error)
}

// RecordStageProgress projects the heartbeat onto the stage row, last-writer
// wins. applied is false (nil error) when the stage was terminal and the
// UPDATE matched zero rows — the caller maps that to 409 stage_terminal.
//
// p.ReportedAt is marshalled into the payload but does NOT survive the write:
// the statement's jsonb_set replaces the reported_at key with Postgres now()
// (#3084), so the stamp comes from the DATABASE clock. p.ReportedAt is
// deliberately NOT zeroed here first — that would be a second control with no
// observable effect, i.e. one that can never be made to go red. The SQL
// expression is the single control.
func (r *postgresRepo) RecordStageProgress(ctx context.Context, stageID uuid.UUID, p StageProgress) (bool, error) {
	payload, err := json.Marshal(p)
	if err != nil {
		return false, err
	}
	q := rundb.New(r.pool)
	n, err := q.RecordStageProgress(ctx, rundb.RecordStageProgressParams{
		ID:       stageID,
		Progress: payload,
	})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// StageProgressByID reads one stage's heartbeat. Returns nil (no error) when
// the stage has no recorded progress (NULL column). An undecodable stored
// payload degrades to nil rather than erroring the read — fail-open on a READ
// only, mirroring rowToStage's ScopeCompletenessPark unmarshal.
func (r *postgresRepo) StageProgressByID(ctx context.Context, stageID uuid.UUID) (*StageProgress, error) {
	q := rundb.New(r.pool)
	raw, err := q.GetStageProgress(ctx, stageID)
	if err != nil {
		return nil, err
	}
	return decodeStageProgress(raw), nil
}

// StageProgressForRun reads every stage's heartbeat for a run in one query.
// Stages with no recorded progress (or an undecodable payload) are omitted from
// the map rather than erroring the whole read.
func (r *postgresRepo) StageProgressForRun(ctx context.Context, runID uuid.UUID) (map[uuid.UUID]StageProgress, error) {
	q := rundb.New(r.pool)
	rows, err := q.ListStageProgressForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]StageProgress, len(rows))
	for _, row := range rows {
		if p := decodeStageProgress(row.Progress); p != nil {
			out[row.ID] = *p
		}
	}
	return out, nil
}

// decodeStageProgress unmarshals a stored JSONB payload, degrading to nil on an
// empty column or an undecodable payload (fail-open read). Shared by both read
// methods.
func decodeStageProgress(raw []byte) *StageProgress {
	if len(raw) == 0 {
		return nil
	}
	var p StageProgress
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil
	}
	return &p
}
