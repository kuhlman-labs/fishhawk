package concurrency

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// PostgresStore is the pgx-backed Store over stage_concurrency_slots (0097).
//
// Connection and lock contract (README.md § "Atomicity and lock order"): one
// Admit is ONE transaction on ONE pooled connection. The group lock is a
// transaction-scoped pg_try_advisory_xact_lock, so a lock miss answers queued
// without waiting and the lock cannot outlive its transaction. The full lock
// order a marker call takes is: the server's per-stage admission mutex
// (orchestrator.LockStageAdmission — a process-local sync.Mutex that holds NO
// pooled connection or transaction) → the group advisory lock → the stage row
// (FOR UPDATE) → the run row (only on paths that already take stage-then-run,
// such as ResumeAwaitingInputAndAppend; Admit itself never locks a run row —
// on the admit path it READS the run's state with a plain SELECT, #4035).
//
// Tenancy: Admit and StatusForStages run on the raw pool with NO tenant GUC
// set, under the runtime role (a superuser today, which bypasses RLS). They
// therefore scope by account EXPLICITLY — every holder/queue read joins
// runs.account_id and matches the requesting stage's account (IS NOT DISTINCT
// FROM, so untenanted runs share one scope), and the lock key is per account
// — so another account's holder is neither counted nor disclosed regardless
// of RLS.
type PostgresStore struct {
	pool *pgxpool.Pool

	// Test hooks, nil in production.
	//
	// afterLock runs right after the try-lock statement with its result,
	// before the stage row lock and the queue upsert — the seam the
	// same-round pre-emption test parks a lock WINNER on while a contended
	// loser commits its queued row.
	afterLock func(ctx context.Context, hit bool)
	// afterDecision runs on the lock-hit path between the admit decision and
	// the CAS — the seam the try-lock serialization test parks an admitter on.
	afterDecision func(ctx context.Context, admit bool)
	// afterCAS runs after the dispatched CAS and before the held mark; a
	// non-nil error aborts (rolls back) the whole admission.
	afterCAS func(ctx context.Context) error
}

var _ Store = (*PostgresStore)(nil)

// NewPostgresStore returns the Postgres slot store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// Interval literals built from the Go constants (integers, never input).
var (
	queueTTLSQL          = fmt.Sprintf("interval '%d seconds'", int(QueueTTL/time.Second))
	runningStaleSQL      = fmt.Sprintf("interval '%d seconds'", int(RunningStaleAfter/time.Second))
	dispatchedStaleSQL   = fmt.Sprintf("interval '%d seconds'", int(DispatchedStaleAfter/time.Second))
	liveHolderPredicateQ = `st.dispatched_at = s.held_dispatched_at
	AND ((st.state = 'running' AND GREATEST(st.dispatched_at, fishhawk_stage_heartbeat_at(st.progress)) > clock_timestamp() - ` + runningStaleSQL + `)
	  OR (st.state = 'dispatched' AND GREATEST(st.dispatched_at, fishhawk_stage_heartbeat_at(st.progress)) > clock_timestamp() - ` + dispatchedStaleSQL + `))`
)

// holdersSQL lists the live holders of one group in one account: held rows
// whose stage is still dispatched/running on the SAME attempt the row
// admitted, and live by the heartbeat-keyed backstop.
var holdersSQL = `SELECT s.run_id, s.stage_id, s.acquired_at
FROM stage_concurrency_slots s
JOIN stages st ON st.id = s.stage_id
JOIN runs r ON r.id = s.run_id
WHERE s.group_key = $1 AND s.state = 'held'
  AND r.account_id IS NOT DISTINCT FROM $2
  AND ` + liveHolderPredicateQ + `
ORDER BY s.acquired_at, s.stage_id`

// aheadSQL counts the fresh queued rows ahead of (enqueued_at, stage_id) in
// one group and account. round_ahead counts only rows enqueued at or before
// $5 (the caller's round start): a row a contended loser committed during
// the caller's own round never blocks the lock winner.
var aheadSQL = `SELECT
  count(*) FILTER (WHERE s.enqueued_at <= $5) AS round_ahead,
  count(*) AS all_ahead
FROM stage_concurrency_slots s
JOIN stages st ON st.id = s.stage_id
JOIN runs r ON r.id = s.run_id
WHERE s.group_key = $1 AND s.state = 'queued' AND s.stage_id <> $2
  AND r.account_id IS NOT DISTINCT FROM $3
  AND (s.enqueued_at, s.stage_id) < ($4, $2)
  AND s.last_seen_at > clock_timestamp() - ` + queueTTLSQL + `
  AND st.state IN ('pending', 'awaiting_host_dispatch')`

// validate refuses a malformed Request before any SQL runs.
func (req Request) validate() error {
	switch {
	case req.StageID == uuid.Nil || req.RunID == uuid.Nil:
		return fmt.Errorf("%w: stage and run ids are required", ErrInvalidRequest)
	case req.GroupKey == "":
		return fmt.Errorf("%w: group key is required", ErrInvalidRequest)
	case req.Limit < 1 || req.Limit > MaxLimit:
		return fmt.Errorf("%w: limit %d outside 1..%d", ErrInvalidRequest, req.Limit, MaxLimit)
	case req.From != run.StageStatePending && req.From != run.StageStateAwaitingHostDispatch:
		return fmt.Errorf("%w: from state %q is not host-dispatchable (want pending or awaiting_host_dispatch)", ErrInvalidRequest, req.From)
	}
	return nil
}

// Admit decides one admission atomically. See README.md § "Admit".
//
// Any error rolls the whole transaction back: the stage keeps its state and
// its slot row keeps exactly the queue standing it had before the call.
func (s *PostgresStore) Admit(ctx context.Context, req Request) (Admission, error) {
	if err := req.validate(); err != nil {
		return Admission{}, err
	}
	var out Admission
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		out, err = s.admitTx(ctx, tx, req)
		return err
	})
	if err != nil {
		return Admission{}, err
	}
	return out, nil
}

func (s *PostgresStore) admitTx(ctx context.Context, tx pgx.Tx, req Request) (Admission, error) {
	// (1) The stage's account, and the round start read BEFORE the try-lock:
	// any row a loser commits because THIS call holds the lock is inserted
	// after this instant, so the round_ahead filter excludes it.
	var acct pgtype.UUID
	var roundStart time.Time
	err := tx.QueryRow(ctx, `SELECT r.account_id, clock_timestamp()
FROM stages st JOIN runs r ON r.id = st.run_id
WHERE st.id = $1 AND st.run_id = $2`, req.StageID, req.RunID).Scan(&acct, &roundStart)
	if errors.Is(err, pgx.ErrNoRows) {
		return Admission{}, fmt.Errorf("concurrency: stage %s in run %s: %w", req.StageID, req.RunID, run.ErrNotFound)
	}
	if err != nil {
		return Admission{}, fmt.Errorf("concurrency: read stage account: %w", err)
	}
	var acctPtr *uuid.UUID
	if acct.Valid {
		id := uuid.UUID(acct.Bytes)
		acctPtr = &id
	}

	// (2) Group lock: try, never wait.
	var hit bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, lockKey(acctPtr, req.GroupKey)).Scan(&hit); err != nil {
		return Admission{}, fmt.Errorf("concurrency: try group lock: %w", err)
	}
	if s.afterLock != nil {
		s.afterLock(ctx, hit)
	}

	// (3) Stage row lock + drift check, on BOTH paths. Every slot-row write
	// for a stage happens under this lock, so the restart decision below
	// reads a row no concurrent admission of the same stage can be changing.
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM stages WHERE id = $1 FOR UPDATE`, req.StageID).Scan(&state); err != nil {
		return Admission{}, fmt.Errorf("concurrency: lock stage: %w", err)
	}
	if run.StageState(state) != req.From {
		return Admission{}, run.StageStateChangedError{StageID: req.StageID, Expected: req.From, Actual: run.StageState(state)}
	}

	// (4) Queue row with the episode-restart rule, identical on both paths.
	enqueuedAt, newlyQueued, queuedBefore, err := upsertQueued(ctx, tx, req)
	if err != nil {
		return Admission{}, err
	}

	holders, err := listHolders(ctx, tx, req.GroupKey, acct)
	if err != nil {
		return Admission{}, err
	}
	var roundAhead, allAhead int
	if err := tx.QueryRow(ctx, aheadSQL, req.GroupKey, req.StageID, acct, enqueuedAt, roundStart).Scan(&roundAhead, &allAhead); err != nil {
		return Admission{}, fmt.Errorf("concurrency: count queue ahead: %w", err)
	}
	queued := Admission{
		Position:    allAhead + 1,
		Holders:     holders,
		EnqueuedAt:  enqueuedAt,
		NewlyQueued: newlyQueued,
		Contended:   !hit,
	}

	// (5) Lock miss: queued without deciding.
	if !hit {
		return queued, nil
	}

	// (6) Decide under the group lock.
	admit := len(holders)+roundAhead < req.Limit
	if s.afterDecision != nil {
		s.afterDecision(ctx, admit)
	}
	if !admit {
		return queued, nil
	}

	// (7) Admit: CAS in THIS transaction, then the held mark. The live-run CAS
	// re-reads the run's state after the CAS (#4035): it sits AFTER the holders
	// read, so a run that went terminal before the slot was seen free is
	// visible here (READ COMMITTED) and refused with run.RunTerminalError,
	// which rolls this whole admission back.
	stage, err := run.TransitionStageFromLiveRunTx(ctx, tx, req.StageID, req.From, run.StageStateDispatched)
	if err != nil {
		return Admission{}, err
	}
	if s.afterCAS != nil {
		if err := s.afterCAS(ctx); err != nil {
			return Admission{}, err
		}
	}
	var waited int
	if err := tx.QueryRow(ctx, `UPDATE stage_concurrency_slots s
SET state = 'held', acquired_at = clock_timestamp(), held_dispatched_at = st.dispatched_at,
  admission_nonce = NULLIF($2, '')
FROM stages st
WHERE st.id = s.stage_id AND s.stage_id = $1
RETURNING GREATEST(0, floor(extract(epoch FROM s.acquired_at - s.enqueued_at)))::int`, req.StageID, req.AdmissionNonce).Scan(&waited); err != nil {
		return Admission{}, fmt.Errorf("concurrency: mark held: %w", err)
	}
	if !queuedBefore {
		waited = 0
	}
	return Admission{
		Admitted:      true,
		Stage:         stage,
		Holders:       holders,
		EnqueuedAt:    enqueuedAt,
		QueuedBefore:  queuedBefore,
		WaitedSeconds: waited,
	}, nil
}

// upsertQueued records the stage as queued, applying the episode-restart
// rule: the caller has verified the stage is host-dispatchable under its row
// lock, so an existing 'held' row is from a PREVIOUS episode. The row
// restarts at the TAIL (enqueued_at = now) when it was held, queued in a
// different group, or queued but stale past QueueTTL; otherwise it keeps its
// enqueued_at. It reports whether this call inserted or restarted the row
// (newlyQueued) and whether the row was already queued in this episode
// (queuedBefore).
func upsertQueued(ctx context.Context, tx pgx.Tx, req Request) (enqueuedAt time.Time, newlyQueued, queuedBefore bool, err error) {
	var exists, restart bool
	err = tx.QueryRow(ctx, `SELECT true,
  state = 'held' OR group_key <> $2 OR last_seen_at <= clock_timestamp() - `+queueTTLSQL+`
FROM stage_concurrency_slots WHERE stage_id = $1`, req.StageID, req.GroupKey).Scan(&exists, &restart)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, false, fmt.Errorf("concurrency: read slot row: %w", err)
	}
	err = tx.QueryRow(ctx, `INSERT INTO stage_concurrency_slots AS s
  (stage_id, run_id, group_key, slot_limit, host, state, enqueued_at, last_seen_at)
VALUES ($1, $2, $3, $4, $5, 'queued', clock_timestamp(), clock_timestamp())
ON CONFLICT (stage_id) DO UPDATE SET
  run_id = EXCLUDED.run_id,
  group_key = EXCLUDED.group_key,
  slot_limit = EXCLUDED.slot_limit,
  host = EXCLUDED.host,
  state = 'queued',
  enqueued_at = CASE WHEN $6 THEN clock_timestamp() ELSE s.enqueued_at END,
  last_seen_at = clock_timestamp(),
  acquired_at = NULL,
  held_dispatched_at = NULL,
  admission_nonce = NULL
RETURNING enqueued_at`, req.StageID, req.RunID, req.GroupKey, req.Limit, req.Host, restart).Scan(&enqueuedAt)
	if err != nil {
		return time.Time{}, false, false, fmt.Errorf("concurrency: upsert slot row: %w", err)
	}
	return enqueuedAt, !exists || restart, exists && !restart, nil
}

func listHolders(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, group string, acct pgtype.UUID) ([]Holder, error) {
	rows, err := q.Query(ctx, holdersSQL, group, acct)
	if err != nil {
		return nil, fmt.Errorf("concurrency: list holders: %w", err)
	}
	holders, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Holder, error) {
		var h Holder
		err := row.Scan(&h.RunID, &h.StageID, &h.Since)
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("concurrency: scan holders: %w", err)
	}
	// pgx.CollectRows returns a non-nil empty slice on zero rows, so an empty
	// holders list renders as [] (TestAdmit_EmptyHoldersRenderAsList pins it).
	return holders, nil
}

// StatusForStages returns the ACTIVE slot state of each listed stage that has
// one: a queued row whose stage is still pending/awaiting_host_dispatch, or a
// held row that currently counts (same attempt, live). Settled, parked,
// bypassed and previous-episode rows are omitted. It runs on the raw pool
// under the same explicit account scoping as Admit.
func (s *PostgresStore) StatusForStages(ctx context.Context, stageIDs []uuid.UUID) (map[uuid.UUID]Status, error) {
	out := map[uuid.UUID]Status{}
	if len(stageIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT s.stage_id, s.group_key, s.slot_limit, s.host, s.state,
  s.enqueued_at, s.acquired_at, s.held_dispatched_at, COALESCE(s.admission_nonce, ''),
  s.last_seen_at > clock_timestamp() - `+queueTTLSQL+` AS waiter_live,
  r.account_id,
  (s.state = 'queued' AND st.state IN ('pending', 'awaiting_host_dispatch')) AS active_queued,
  (s.state = 'held' AND `+liveHolderPredicateQ+`) AS active_held
FROM stage_concurrency_slots s
JOIN stages st ON st.id = s.stage_id
JOIN runs r ON r.id = s.run_id
WHERE s.stage_id = ANY($1)`, stageIDs)
	if err != nil {
		return nil, fmt.Errorf("concurrency: status: %w", err)
	}
	type statusRow struct {
		stageID      uuid.UUID
		st           Status
		acct         pgtype.UUID
		activeQueued bool
		activeHeld   bool
	}
	var collected []statusRow
	for rows.Next() {
		var r statusRow
		var state string
		if err := rows.Scan(&r.stageID, &r.st.GroupKey, &r.st.Limit, &r.st.Host, &state,
			&r.st.EnqueuedAt, &r.st.AcquiredAt, &r.st.HeldDispatchedAt, &r.st.AdmissionNonce, &r.st.WaiterLive,
			&r.acct, &r.activeQueued, &r.activeHeld); err != nil {
			rows.Close()
			return nil, fmt.Errorf("concurrency: scan status: %w", err)
		}
		r.st.State = SlotState(state)
		collected = append(collected, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("concurrency: status rows: %w", err)
	}

	type scope struct {
		group string
		acct  pgtype.UUID
	}
	holdersBy := map[scope][]Holder{}
	for _, r := range collected {
		if !r.activeQueued && !r.activeHeld {
			continue
		}
		key := scope{r.st.GroupKey, r.acct}
		holders, ok := holdersBy[key]
		if !ok {
			holders, err = listHolders(ctx, s.pool, r.st.GroupKey, r.acct)
			if err != nil {
				return nil, err
			}
			holdersBy[key] = holders
		}
		st := r.st
		st.Holders = holders
		if r.activeQueued {
			var roundAhead, allAhead int
			if err := s.pool.QueryRow(ctx, aheadSQL, st.GroupKey, r.stageID, r.acct, st.EnqueuedAt, st.EnqueuedAt).Scan(&roundAhead, &allAhead); err != nil {
				return nil, fmt.Errorf("concurrency: status position: %w", err)
			}
			st.Position = allAhead + 1
			st.AcquiredAt, st.HeldDispatchedAt, st.AdmissionNonce = nil, nil, ""
		} else {
			st.WaiterLive = false
		}
		out[r.stageID] = st
	}
	return out, nil
}
