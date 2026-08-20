package run

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	rundb "github.com/kuhlman-labs/fishhawk/backend/internal/run/db"
)

// DispatchedStageLiveness is the per-dispatched-stage snapshot the dispatch
// watchdog measures its deadline from (#2744). It carries the DEDICATED
// dispatch clock (DispatchedAt, stamped by migration 0072's transition-keyed
// trigger — a progress heartbeat cannot advance it), the generic UpdatedAt (the
// legacy fallback for a row that somehow escaped the 0072 backfill), and the
// last heartbeat time decoded from the stage's progress JSONB.
//
// DispatchedAt is a *time.Time so a NULL column (a pre-0072 row that was never
// re-dispatched, or a backfill miss) is representable as nil; the watchdog then
// degrades to UpdatedAt for that one row. LastHeartbeatAt is nil when the stage
// has never reported, when its stored payload is undecodable, OR when the
// stored heartbeat PREDATES the current dispatch — see ListDispatchedStageLiveness.
type DispatchedStageLiveness struct {
	StageID         uuid.UUID
	RunID           uuid.UUID
	DispatchedAt    *time.Time
	UpdatedAt       time.Time
	LastHeartbeatAt *time.Time
}

// DispatchLivenessLister is the OPTIONAL capability the dispatch watchdog reads
// its liveness signal through. Like StageProgressStore / ScopeCompletenessPark
// it is deliberately NOT part of Repository — adding it would fan out across the
// ~23 Repository implementations for a signal only the postgres repo and the
// watchdog use. The concrete postgres repo implements it; the watchdog FAILS
// CLOSED (Ticker.Run returns an error, the ticker does not start) when the wired
// repo does not carry it, rather than silently degrading back to the
// heartbeat-defeatable Stage.UpdatedAt signal this change exists to remove.
type DispatchLivenessLister interface {
	ListDispatchedStageLiveness(ctx context.Context) ([]DispatchedStageLiveness, error)
}

// Compile-time assertion that the concrete postgres repo carries the
// DispatchLivenessLister capability (the StageCASTransitioner precedent).
// Removing or renaming the method fails the BUILD rather than degrading the
// watchdog's serve.go capability pre-check at runtime.
var _ DispatchLivenessLister = (*postgresRepo)(nil)

// ListDispatchedStageLiveness returns one DispatchedStageLiveness per stage in
// the 'dispatched' state, oldest dispatch first.
//
// LastHeartbeatAt is derived ATTEMPT-RELATIVE (#2744 approval condition 1): the
// stored progress payload may be left over from a PREVIOUS dispatch attempt on
// the supported dispatched → running → … → dispatched retry path, and treating
// it as the current attempt's check-in would misclassify a fresh, un-checked-in
// attempt as wedged_after_checkin — the exact two modes this change separates,
// swapped, on the recovery path an operator is most likely watching. So a
// decoded heartbeat whose ReportedAt is OLDER than the current DispatchedAt is
// treated as ABSENT (nil): it belongs to the prior attempt. A nil DispatchedAt
// (legacy row on the updated_at fallback) skips the comparison and trusts the
// decoded heartbeat as-is. A nil/undecodable/zero payload yields nil, the same
// fail-open-on-READ posture progress.go documents.
func (r *postgresRepo) ListDispatchedStageLiveness(ctx context.Context) ([]DispatchedStageLiveness, error) {
	q := rundb.New(r.pool)
	rows, err := q.ListDispatchedStageLiveness(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]DispatchedStageLiveness, 0, len(rows))
	for _, row := range rows {
		l := DispatchedStageLiveness{
			StageID:      row.ID,
			RunID:        row.RunID,
			DispatchedAt: timestamptzToPtr(row.DispatchedAt),
		}
		if row.UpdatedAt.Valid {
			l.UpdatedAt = row.UpdatedAt.Time.UTC()
		}
		if p := decodeStageProgress(row.Progress); p != nil && !p.ReportedAt.IsZero() {
			hb := p.ReportedAt.UTC()
			// Attempt-relative: ignore a heartbeat that predates the current
			// dispatch — it is a leftover from a previous attempt.
			if l.DispatchedAt == nil || !hb.Before(*l.DispatchedAt) {
				l.LastHeartbeatAt = &hb
			}
		}
		out = append(out, l)
	}
	return out, nil
}

// timestamptzToPtr maps a pgtype.Timestamptz to *time.Time (UTC), returning nil
// for an invalid (NULL) value.
func timestamptzToPtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time.UTC()
	return &t
}
