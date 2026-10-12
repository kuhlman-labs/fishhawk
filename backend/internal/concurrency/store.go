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
// Promote takes the same group lock BLOCKING (pg_advisory_xact_lock) and then
// the same stage row, slot row and (through its Record callback's audit
// append) run row, so it follows the one documented order.
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
	// afterPromotePeek runs between Promote's unlocked peek and its group
	// lock — the seam the slot-changed test re-keys the row on.
	afterPromotePeek func(ctx context.Context)
	// afterPromotePosition runs between Promote's old-position read and its
	// order-key write, only on the path that decided to write — the seam the
	// emptied-competitor test cancels or stales a competitor on (#4312).
	afterPromotePosition func(ctx context.Context)
}

var _ Store = (*PostgresStore)(nil)

// NewPostgresStore returns the Postgres slot store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// JobHeartbeatTTL bounds a RUNNING specialist job's heartbeat (ADR-097 D6,
// #4300): a job counts as a holder of its admission group only while its
// runner re-POSTs admission within this window (every 15s), so a crashed job
// stops blocking local-verify within it. README.md § "Specialist-job holders
// (ADR-097 D6)".
const JobHeartbeatTTL = 2 * time.Minute

// Interval literals built from the Go constants (integers, never input).
var (
	jobHeartbeatTTLSQL   = fmt.Sprintf("interval '%d seconds'", int(JobHeartbeatTTL/time.Second))
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

// jobHoldersSQL counts the live specialist-job holders of one group in one
// account (ADR-097 D6): running jobs admitted into the group whose heartbeat
// is fresh within JobHeartbeatTTL. A stale running job releases without a
// write here, like a stage holder past its backstop.
var jobHoldersSQL = `SELECT count(*) FROM specialist_jobs
WHERE admission_group = $1 AND state = 'running'
  AND account_id IS NOT DISTINCT FROM $2
  AND last_seen_at > clock_timestamp() - ` + jobHeartbeatTTLSQL

// freshStageWaitersSQL counts every fresh queued stage row of one group in one
// account (the freshWaiterPredicateQ rows; no stage is excluded, so $2 is the
// nil UUID).
var freshStageWaitersSQL = `SELECT count(*)
FROM stage_concurrency_slots s
JOIN stages st ON st.id = s.stage_id
JOIN runs r ON r.id = s.run_id
WHERE ` + freshWaiterPredicateQ

// effectiveKeySQL is a slot row's ordering key: the promote's order_key when
// set (0102, #4191), else enqueued_at. Every row without a promotion is
// therefore strict FIFO.
const effectiveKeySQL = `COALESCE(s.order_key, s.enqueued_at)`

// freshWaiterPredicateQ is what makes a queued row count for anyone: same
// group and account, fresh within QueueTTL, stage still host-dispatchable.
var freshWaiterPredicateQ = `s.group_key = $1 AND s.state = 'queued' AND s.stage_id <> $2
  AND r.account_id IS NOT DISTINCT FROM $3
  AND s.last_seen_at > clock_timestamp() - ` + queueTTLSQL + `
  AND st.state IN ('pending', 'awaiting_host_dispatch')`

// aheadSQL counts the fresh queued rows ahead of ($4, stage_id) in one group
// and account, where $4 is the caller's EFFECTIVE key (effectiveKeySQL).
// round_ahead counts only rows enqueued at or before $5 (the caller's round
// start): a row a contended loser committed during the caller's own round
// never blocks the lock winner. It filters on enqueued_at, not the effective
// key, because it is about when a row was COMMITTED, which a promotion never
// changes.
var aheadSQL = `SELECT
  count(*) FILTER (WHERE s.enqueued_at <= $5) AS round_ahead,
  count(*) AS all_ahead
FROM stage_concurrency_slots s
JOIN stages st ON st.id = s.stage_id
JOIN runs r ON r.id = s.run_id
WHERE ` + freshWaiterPredicateQ + `
  AND (` + effectiveKeySQL + `, s.stage_id) < ($4, $2)`

// promoteKeySQL writes the promoted stage's order key: 1 ms ahead of the
// minimum effective key over every OTHER fresh waiter (the aheadSQL
// predicate), so the newest promote wins. enqueued_at, last_seen_at and every
// other row are untouched.
//
// The write is GUARDED on a waiter still being strictly ahead of the caller's
// own effective key ($4), evaluated in this same statement (#4312). The
// old-position read is an EARLIER statement, and the competitor set can shrink
// in between: a waiter goes stale (clock_timestamp freshness) or is cancelled
// (its stage and run rows are not under the group lock). Unguarded, an emptied
// set made min() NULL and wrote a NULL order_key that was still audited as a
// promotion; with only a waiter BEHIND left, it wrote a key later than the
// caller's own. min and the ahead flag come from ONE aggregate over ONE scan,
// so they agree on each row's clock_timestamp() freshness. bool_or over zero
// rows is NULL, which fails the guard too. A failed guard matches no row.
var promoteKeySQL = `WITH others AS (
  SELECT min(` + effectiveKeySQL + `) AS k,
    bool_or((` + effectiveKeySQL + `, s.stage_id) < ($4, $2)) AS ahead
  FROM stage_concurrency_slots s
  JOIN stages st ON st.id = s.stage_id
  JOIN runs r ON r.id = s.run_id
  WHERE ` + freshWaiterPredicateQ + `
)
UPDATE stage_concurrency_slots SET order_key = others.k - interval '1 millisecond'
FROM others
WHERE stage_id = $2 AND state = 'queued' AND others.ahead
RETURNING COALESCE(order_key, enqueued_at)`

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
	enqueuedAt, effectiveKey, newlyQueued, queuedBefore, err := upsertQueued(ctx, tx, req)
	if err != nil {
		return Admission{}, err
	}

	holders, err := listHolders(ctx, tx, req.GroupKey, acct)
	if err != nil {
		return Admission{}, err
	}
	var roundAhead, allAhead int
	if err := tx.QueryRow(ctx, aheadSQL, req.GroupKey, req.StageID, acct, effectiveKey, roundStart).Scan(&roundAhead, &allAhead); err != nil {
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

	// Live specialist-job holders count against the same limit (ADR-097 D6):
	// a merge-candidate verify pass never runs beside a live job. Zero
	// whenever no job is running in the group, so stage-only groups are
	// unchanged.
	jobHolders, err := countJobHolders(ctx, tx, req.GroupKey, acct)
	if err != nil {
		return Admission{}, err
	}

	// (6) Decide under the group lock.
	admit := len(holders)+jobHolders+roundAhead < req.Limit
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
  admission_nonce = NULLIF($2, ''), order_key = NULL
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
// enqueued_at. A restart also clears order_key (a promotion lasts for its
// episode only); the keep arm leaves it untouched. It returns the row's
// enqueued_at and its effective ordering key, and reports whether this call
// inserted or restarted the row (newlyQueued) and whether the row was already
// queued in this episode (queuedBefore).
func upsertQueued(ctx context.Context, tx pgx.Tx, req Request) (enqueuedAt, effectiveKey time.Time, newlyQueued, queuedBefore bool, err error) {
	var exists, restart bool
	err = tx.QueryRow(ctx, `SELECT true,
  state = 'held' OR group_key <> $2 OR last_seen_at <= clock_timestamp() - `+queueTTLSQL+`
FROM stage_concurrency_slots WHERE stage_id = $1`, req.StageID, req.GroupKey).Scan(&exists, &restart)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, time.Time{}, false, false, fmt.Errorf("concurrency: read slot row: %w", err)
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
  order_key = CASE WHEN $6 THEN NULL ELSE s.order_key END,
  last_seen_at = clock_timestamp(),
  acquired_at = NULL,
  held_dispatched_at = NULL,
  admission_nonce = NULL
RETURNING enqueued_at, COALESCE(order_key, enqueued_at)`, req.StageID, req.RunID, req.GroupKey, req.Limit, req.Host, restart).Scan(&enqueuedAt, &effectiveKey)
	if err != nil {
		return time.Time{}, time.Time{}, false, false, fmt.Errorf("concurrency: upsert slot row: %w", err)
	}
	return enqueuedAt, effectiveKey, !exists || restart, exists && !restart, nil
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

func countJobHolders(ctx context.Context, tx pgx.Tx, group string, acct pgtype.UUID) (int, error) {
	var n int
	if err := tx.QueryRow(ctx, jobHoldersSQL, group, acct).Scan(&n); err != nil {
		return 0, fmt.Errorf("concurrency: count job holders: %w", err)
	}
	return n, nil
}

func pgAccount(acct *uuid.UUID) pgtype.UUID {
	if acct == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *acct, Valid: true}
}

// TryGroupLockTx try-locks one group's advisory lock in one account inside
// the caller's transaction (ADR-097 D6). It uses the SAME key Admit takes, so
// a specialist-job admission and a stage admission of one group contend on
// one lock. It never waits; the lock is released at the caller's COMMIT or
// ROLLBACK. A nil acct is the untenanted scope.
func TryGroupLockTx(ctx context.Context, tx pgx.Tx, acct *uuid.UUID, group string) (bool, error) {
	var hit bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, lockKey(acct, group)).Scan(&hit); err != nil {
		return false, fmt.Errorf("concurrency: try group lock: %w", err)
	}
	return hit, nil
}

// Occupancy is one group's occupancy in one account, as a specialist-job
// admission reads it under the group lock.
type Occupancy struct {
	// StageHolders counts the live stage holders (the Admit holders list).
	StageHolders int
	// FreshStageWaiters counts the queued stage rows fresh within QueueTTL
	// whose stage is still host-dispatchable. A job yields to them.
	FreshStageWaiters int
	// JobHolders counts the running specialist jobs fresh within
	// JobHeartbeatTTL.
	JobHolders int
}

// GroupOccupancyTx reads one group's occupancy in one account inside the
// caller's transaction (ADR-097 D6). It is meaningful as a decision input
// only while the caller holds TryGroupLockTx for the same group and account.
func GroupOccupancyTx(ctx context.Context, tx pgx.Tx, acct *uuid.UUID, group string) (Occupancy, error) {
	a := pgAccount(acct)
	holders, err := listHolders(ctx, tx, group, a)
	if err != nil {
		return Occupancy{}, err
	}
	var waiters int
	if err := tx.QueryRow(ctx, freshStageWaitersSQL, group, uuid.Nil, a).Scan(&waiters); err != nil {
		return Occupancy{}, fmt.Errorf("concurrency: count fresh stage waiters: %w", err)
	}
	jobs, err := countJobHolders(ctx, tx, group, a)
	if err != nil {
		return Occupancy{}, err
	}
	return Occupancy{StageHolders: len(holders), FreshStageWaiters: waiters, JobHolders: jobs}, nil
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
  `+effectiveKeySQL+` AS effective_key, s.order_key IS NOT NULL AS promoted,
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
		effectiveKey time.Time
		promoted     bool
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
			&r.effectiveKey, &r.promoted, &r.acct, &r.activeQueued, &r.activeHeld); err != nil {
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
			if err := s.pool.QueryRow(ctx, aheadSQL, st.GroupKey, r.stageID, r.acct, r.effectiveKey, st.EnqueuedAt).Scan(&roundAhead, &allAhead); err != nil {
				return nil, fmt.Errorf("concurrency: status position: %w", err)
			}
			st.Position = allAhead + 1
			st.Promoted = r.promoted
			st.AcquiredAt, st.HeldDispatchedAt, st.AdmissionNonce = nil, nil, ""
		} else {
			st.WaiterLive = false
		}
		out[r.stageID] = st
	}
	return out, nil
}

// Promote moves one queued waiter ahead of every other fresh waiter in its
// group and account (#4191). See README.md § "Promote".
//
// One transaction on one pooled connection: an unlocked peek of the stage's
// account and group, then the group advisory lock taken BLOCKING (Admit only
// try-locks it, so this cannot deadlock with an admission; an admitter
// arriving meanwhile answers Contended), then the stage row and the slot row
// FOR UPDATE. It never writes a held row and never touches enqueued_at. Any
// error, including one returned by req.Record, rolls the whole promotion
// back.
func (s *PostgresStore) Promote(ctx context.Context, req PromoteRequest) (Promotion, error) {
	switch {
	case req.StageID == uuid.Nil:
		return Promotion{}, fmt.Errorf("%w: stage id is required", ErrInvalidRequest)
	case req.Record == nil:
		return Promotion{}, fmt.Errorf("%w: a Record callback is required (a promotion never commits unaudited)", ErrInvalidRequest)
	}
	var out Promotion
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		out, err = s.promoteTx(ctx, tx, req)
		return err
	})
	if err != nil {
		return Promotion{}, err
	}
	return out, nil
}

func (s *PostgresStore) promoteTx(ctx context.Context, tx pgx.Tx, req PromoteRequest) (Promotion, error) {
	// (1) Unlocked peek: the stage's run and account, and its slot row's group.
	var runID uuid.UUID
	var acct pgtype.UUID
	var peekGroup *string
	err := tx.QueryRow(ctx, `SELECT st.run_id, r.account_id, s.group_key
FROM stages st JOIN runs r ON r.id = st.run_id
LEFT JOIN stage_concurrency_slots s ON s.stage_id = st.id
WHERE st.id = $1`, req.StageID).Scan(&runID, &acct, &peekGroup)
	if errors.Is(err, pgx.ErrNoRows) {
		return Promotion{}, fmt.Errorf("concurrency: promote stage %s: %w", req.StageID, run.ErrNotFound)
	}
	if err != nil {
		return Promotion{}, fmt.Errorf("concurrency: promote: read stage: %w", err)
	}
	if peekGroup == nil {
		return Promotion{}, fmt.Errorf("concurrency: promote stage %s: %w", req.StageID, ErrNotQueued)
	}
	var acctPtr *uuid.UUID
	if acct.Valid {
		id := uuid.UUID(acct.Bytes)
		acctPtr = &id
	}
	if s.afterPromotePeek != nil {
		s.afterPromotePeek(ctx)
	}

	// (2) Group lock, BLOCKING: waits out an in-flight admission round.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey(acctPtr, *peekGroup)); err != nil {
		return Promotion{}, fmt.Errorf("concurrency: promote: group lock: %w", err)
	}

	// (3) Stage row lock: every slot-row write for a stage happens under it.
	var stageState string
	if err := tx.QueryRow(ctx, `SELECT state FROM stages WHERE id = $1 FOR UPDATE`, req.StageID).Scan(&stageState); err != nil {
		return Promotion{}, fmt.Errorf("concurrency: promote: lock stage: %w", err)
	}

	// (4) Locked re-read of the slot row, then classify.
	var (
		group, slotState string
		limit            int
		enqueuedAt, key  time.Time
		fresh            bool
	)
	err = tx.QueryRow(ctx, `SELECT group_key, state, slot_limit, enqueued_at, COALESCE(order_key, enqueued_at),
  last_seen_at > clock_timestamp() - `+queueTTLSQL+`
FROM stage_concurrency_slots WHERE stage_id = $1 FOR UPDATE`, req.StageID).Scan(&group, &slotState, &limit, &enqueuedAt, &key, &fresh)
	if errors.Is(err, pgx.ErrNoRows) {
		return Promotion{}, fmt.Errorf("concurrency: promote stage %s: %w", req.StageID, ErrNotQueued)
	}
	if err != nil {
		return Promotion{}, fmt.Errorf("concurrency: promote: lock slot row: %w", err)
	}
	st := run.StageState(stageState)
	switch {
	case group != *peekGroup:
		return Promotion{}, fmt.Errorf("concurrency: promote stage %s: group %q became %q: %w", req.StageID, *peekGroup, group, ErrSlotChanged)
	case SlotState(slotState) == SlotHeld && (st == run.StageStateDispatched || st == run.StageStateRunning):
		return Promotion{}, fmt.Errorf("concurrency: promote stage %s: %w", req.StageID, ErrSlotHeld)
	case SlotState(slotState) == SlotHeld:
		// A previous episode's held row the stage has not re-polled.
		return Promotion{}, fmt.Errorf("concurrency: promote stage %s: held row from a previous episode (stage %s): %w", req.StageID, st, ErrNotQueued)
	case st != run.StageStatePending && st != run.StageStateAwaitingHostDispatch:
		return Promotion{}, fmt.Errorf("concurrency: promote stage %s: stage is %s: %w", req.StageID, st, ErrNotQueued)
	case !fresh:
		return Promotion{}, fmt.Errorf("concurrency: promote stage %s: %w", req.StageID, ErrWaiterNotLive)
	}

	// (5) Holders and the old position.
	holders, err := listHolders(ctx, tx, group, acct)
	if err != nil {
		return Promotion{}, err
	}
	position := func(key time.Time) (int, error) {
		var roundAhead, allAhead int
		if err := tx.QueryRow(ctx, aheadSQL, group, req.StageID, acct, key, enqueuedAt).Scan(&roundAhead, &allAhead); err != nil {
			return 0, fmt.Errorf("concurrency: promote: count queue ahead: %w", err)
		}
		return allAhead + 1, nil
	}
	oldPos, err := position(key)
	if err != nil {
		return Promotion{}, err
	}
	out := Promotion{
		StageID: req.StageID, RunID: runID, GroupKey: group, Limit: limit,
		OldPosition: oldPos, NewPosition: oldPos, Holders: holders,
	}
	if oldPos == 1 {
		return out, nil // already first: write nothing, record nothing
	}

	if s.afterPromotePosition != nil {
		s.afterPromotePosition(ctx)
	}

	// (6) Write the order key just ahead of every other fresh waiter, guarded
	// in the statement on a waiter still being ahead (promoteKeySQL, #4312).
	// pgx.ErrNoRows means the guard failed: the last competitor ahead went
	// stale or was cancelled since step (5), so nobody is ahead at the write
	// instant and this is the same answer as the oldPos == 1 path above —
	// nothing written, Record not called. That reading is sound ONLY because
	// step (4) holds this stage's slot row FOR UPDATE and has already
	// classified it as queued, so the UPDATE cannot miss for any other reason.
	// A change that drops or weakens that lock would turn a vanished or held
	// row into a false Changed=false 1/1 answer.
	if err := tx.QueryRow(ctx, promoteKeySQL, group, req.StageID, acct, key).Scan(&key); errors.Is(err, pgx.ErrNoRows) {
		out.OldPosition, out.NewPosition = 1, 1
		return out, nil
	} else if err != nil {
		return Promotion{}, fmt.Errorf("concurrency: promote: write order key: %w", err)
	}

	// (7) The new position, read back under the same lock.
	if out.NewPosition, err = position(key); err != nil {
		return Promotion{}, err
	}
	out.Changed = true

	// (8) The caller's record (the audit append) inside this transaction.
	if req.Record != nil {
		if err := req.Record(ctx, tx, out); err != nil {
			return Promotion{}, err
		}
	}
	return out, nil
}
