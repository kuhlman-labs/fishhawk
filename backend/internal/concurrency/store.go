package concurrency

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// PostgresStore is the pgx-backed Store over stage_concurrency_slots (0097;
// one row per (stage, group) since 0118, ADR-094 D2).
//
// Connection and lock contract (README.md § "Atomicity and lock order"): one
// Admit is ONE transaction on ONE pooled connection. Each group lock is a
// transaction-scoped pg_try_advisory_xact_lock, taken in canonical (byte)
// key order across the stage's set, so a lock miss answers queued without
// waiting and no lock can outlive its transaction. The full lock order a
// marker call takes is: the server's per-stage admission mutex
// (orchestrator.LockStageAdmission — a process-local sync.Mutex that holds NO
// pooled connection or transaction) → the group advisory locks → the stage row
// (FOR UPDATE) → the run row (only on paths that already take stage-then-run,
// such as ResumeAwaitingInputAndAppend; Admit itself never locks a run row —
// on the admit path it READS the run's state with a plain SELECT, #4035).
// Promote takes the same group locks BLOCKING (pg_advisory_xact_lock), in the
// same sorted order, and then the same stage row, slot rows and (through its
// Record callback's audit append) run row, so it follows the one documented
// order; Admit never waits on a lock, so no wait-for cycle can form.
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

// freshWaiterPredicate is what makes a queued row count for anyone: in the
// group(s) groupCond names, same account, fresh within QueueTTL, stage still
// host-dispatchable, and not the caller's own stage ($2).
func freshWaiterPredicate(groupCond string) string {
	return groupCond + ` AND s.state = 'queued' AND s.stage_id <> $2
  AND r.account_id IS NOT DISTINCT FROM $3
  AND s.last_seen_at > clock_timestamp() - ` + queueTTLSQL + `
  AND st.state IN ('pending', 'awaiting_host_dispatch')`
}

// freshWaiterPredicateQ is freshWaiterPredicate for one group ($1).
var freshWaiterPredicateQ = freshWaiterPredicate(`s.group_key = $1`)

// multiGroupStageQ reports that row s's stage holds more than one slot row,
// i.e. its admission set has more than one group (ADR-094 D2, #4240).
const multiGroupStageQ = `EXISTS (SELECT 1 FROM stage_concurrency_slots m
    WHERE m.stage_id = s.stage_id AND m.group_key <> s.group_key)`

// aheadSQL counts the fresh queued rows ahead of ($4, stage_id) in one group
// and account, where $4 is the caller's EFFECTIVE key (effectiveKeySQL).
// round_ahead counts only rows enqueued at or before $5 (the caller's round
// start): a row a contended loser committed during the caller's own round
// never blocks the lock winner. It filters on enqueued_at, not the effective
// key, because it is about when a row was COMMITTED, which a promotion never
// changes. multi_round_ahead is the subset of round_ahead whose stage has a
// multi-group set: Admit's condition (b) (README.md § "Group sets and scoped
// keys"); Status and Promote ignore it.
var aheadSQL = `SELECT
  count(*) FILTER (WHERE s.enqueued_at <= $5) AS round_ahead,
  count(*) AS all_ahead,
  count(*) FILTER (WHERE s.enqueued_at <= $5 AND ` + multiGroupStageQ + `) AS multi_round_ahead
FROM stage_concurrency_slots s
JOIN stages st ON st.id = s.stage_id
JOIN runs r ON r.id = s.run_id
WHERE ` + freshWaiterPredicateQ + `
  AND (` + effectiveKeySQL + `, s.stage_id) < ($4, $2)`

// promoteKeySQL writes the promoted stage's order key: 1 ms ahead of the
// minimum effective key over every OTHER fresh waiter in ANY group of the
// stage's set ($1, a text[]; the aheadSQL predicate), so the newest promote
// wins and the stage is first in every group. The SAME key is written to
// EVERY row of the stage (#4191 option a). enqueued_at, last_seen_at and
// every other stage's rows are untouched. It returns the written key and the
// number of rows written (0 when the guard fails).
//
// The write is GUARDED on a waiter still being strictly ahead of the caller's
// own effective key ($4) in some group of the set, evaluated in this same
// statement (#4312). The old-position read is an EARLIER statement, and the
// competitor set can shrink in between: a waiter goes stale (clock_timestamp
// freshness) or is cancelled (its stage and run rows are not under the group
// lock). Unguarded, an emptied set made min() NULL and wrote a NULL order_key
// that was still audited as a promotion; with only a waiter BEHIND left, it
// wrote a key later than the caller's own. min and the ahead flag come from
// ONE aggregate over ONE scan, so they agree on each row's clock_timestamp()
// freshness. bool_or over zero rows is NULL, which fails the guard too. A
// failed guard matches no row.
var promoteKeySQL = `WITH others AS (
  SELECT min(` + effectiveKeySQL + `) AS k,
    bool_or((` + effectiveKeySQL + `, s.stage_id) < ($4, $2)) AS ahead
  FROM stage_concurrency_slots s
  JOIN stages st ON st.id = s.stage_id
  JOIN runs r ON r.id = s.run_id
  WHERE ` + freshWaiterPredicate(`s.group_key = ANY($1::text[])`) + `
), written AS (
  UPDATE stage_concurrency_slots SET order_key = others.k - interval '1 millisecond'
  FROM others
  WHERE stage_id = $2 AND state = 'queued' AND others.ahead
  RETURNING COALESCE(order_key, enqueued_at) AS key
)
SELECT min(key), count(*) FROM written`

// validate refuses a malformed Request before any SQL runs.
func (req Request) validate() error {
	switch {
	case req.StageID == uuid.Nil || req.RunID == uuid.Nil:
		return fmt.Errorf("%w: stage and run ids are required", ErrInvalidRequest)
	case req.From != run.StageStatePending && req.From != run.StageStateAwaitingHostDispatch:
		return fmt.Errorf("%w: from state %q is not host-dispatchable (want pending or awaiting_host_dispatch)", ErrInvalidRequest, req.From)
	}
	if req.Groups == nil {
		// The single-group shorthand.
		switch {
		case req.GroupKey == "":
			return fmt.Errorf("%w: group key is required", ErrInvalidRequest)
		case req.Limit < 1 || req.Limit > MaxLimit:
			return fmt.Errorf("%w: limit %d outside 1..%d", ErrInvalidRequest, req.Limit, MaxLimit)
		}
		return nil
	}
	switch {
	case req.GroupKey != "" || req.Limit != 0:
		return fmt.Errorf("%w: the GroupKey/Limit shorthand and Groups are mutually exclusive", ErrInvalidRequest)
	case len(req.Groups) == 0:
		return fmt.Errorf("%w: the group set is empty", ErrInvalidRequest)
	case len(req.Groups) > MaxGroups:
		return fmt.Errorf("%w: %d groups exceeds the maximum of %d", ErrInvalidRequest, len(req.Groups), MaxGroups)
	}
	seen := make(map[string]bool, len(req.Groups))
	for i, g := range req.Groups {
		switch {
		case g.Key == "":
			return fmt.Errorf("%w: group %d: group key is required", ErrInvalidRequest, i)
		case seen[g.Key]:
			return fmt.Errorf("%w: group %q appears more than once", ErrInvalidRequest, g.Key)
		case g.Limit < 1 || g.Limit > MaxLimit:
			return fmt.Errorf("%w: group %q: limit %d outside 1..%d", ErrInvalidRequest, g.Key, g.Limit, MaxLimit)
		}
		seen[g.Key] = true
	}
	return nil
}

// normalised returns the request's group set in canonical (byte) key order,
// with the shorthand folded into a one-element set. Call it only on a
// validated request.
func (req Request) normalised() []GroupRequest {
	if req.Groups == nil {
		return []GroupRequest{{Key: req.GroupKey, Limit: req.Limit}}
	}
	groups := append([]GroupRequest(nil), req.Groups...)
	sort.Slice(groups, func(i, j int) bool { return groups[i].Key < groups[j].Key })
	return groups
}

// groupScope is a key's scope for read surfaces ("" when it does not parse).
func groupScope(key string) Scope {
	g, ok := ParseGroupKey(key)
	if !ok {
		return ""
	}
	return g.Scope
}

// bindingQueued picks the binding group of a queued set: the highest
// position, ties broken in canonical key order (positions is in that order).
func bindingQueued(positions []int) int {
	b := 0
	for i, p := range positions {
		if p > positions[b] {
			b = i
		}
	}
	return b
}

// Admit decides one admission atomically. See README.md § "Admit".
//
// Any error rolls the whole transaction back: the stage keeps its state and
// its slot rows keep exactly the queue standing they had before the call.
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

// groupView is one group's admission inputs, read in admitTx step (5).
type groupView struct {
	holders                     []Holder
	roundAhead, allAhead, multi int
	jobs                        int
}

func (s *PostgresStore) admitTx(ctx context.Context, tx pgx.Tx, req Request) (Admission, error) {
	groups := req.normalised()

	// (1) The stage's account, and the round start read BEFORE the try-locks:
	// any row a loser commits because THIS call holds a lock is inserted after
	// this instant, so the round_ahead filter excludes it.
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

	// (2) Group locks: try each in canonical key order, never wait, and stop
	// at the first miss (a miss queues without deciding, so the rest are
	// never needed).
	hit := true
	for _, g := range groups {
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, lockKey(acctPtr, g.Key)).Scan(&hit); err != nil {
			return Admission{}, fmt.Errorf("concurrency: try group lock: %w", err)
		}
		if !hit {
			break
		}
	}
	if s.afterLock != nil {
		s.afterLock(ctx, hit)
	}

	// (3) Stage row lock + drift check, on BOTH paths. Every slot-row write
	// for a stage happens under this lock, so the restart decision below
	// reads rows no concurrent admission of the same stage can be changing.
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM stages WHERE id = $1 FOR UPDATE`, req.StageID).Scan(&state); err != nil {
		return Admission{}, fmt.Errorf("concurrency: lock stage: %w", err)
	}
	if run.StageState(state) != req.From {
		return Admission{}, run.StageStateChangedError{StageID: req.StageID, Expected: req.From, Actual: run.StageState(state)}
	}

	// (4) One queue row per group with the episode-restart rule, identical on
	// both paths.
	enqueuedAt, effectiveKey, newlyQueued, queuedBefore, err := upsertQueuedSet(ctx, tx, req, groups)
	if err != nil {
		return Admission{}, err
	}

	views := make([]groupView, len(groups))
	positions := make([]int, len(groups))
	for i, g := range groups {
		holders, err := listHolders(ctx, tx, g.Key, acct)
		if err != nil {
			return Admission{}, err
		}
		v := groupView{holders: holders}
		if err := tx.QueryRow(ctx, aheadSQL, g.Key, req.StageID, acct, effectiveKey, roundStart).Scan(&v.roundAhead, &v.allAhead, &v.multi); err != nil {
			return Admission{}, fmt.Errorf("concurrency: count queue ahead: %w", err)
		}
		views[i] = v
		positions[i] = v.allAhead + 1
	}
	b := bindingQueued(positions)
	queued := Admission{
		Position:    positions[b],
		Holders:     views[b].holders,
		EnqueuedAt:  enqueuedAt,
		NewlyQueued: newlyQueued,
		Contended:   !hit,
		Groups:      make([]GroupState, len(groups)),
	}
	for i, g := range groups {
		queued.Groups[i] = GroupState{Key: g.Key, Scope: groupScope(g.Key), Limit: g.Limit, Position: positions[i], Holders: views[i].holders}
	}

	// (5) Lock miss: queued without deciding.
	if !hit {
		return queued, nil
	}

	// (6) Decide under every group lock, fully before any write. Live
	// specialist-job holders count against the same limit (ADR-097 D6): zero
	// whenever no job is running in the group, so stage-only groups are
	// unchanged. Per group G: (a) holders + job holders + earlier fresh
	// waiters < limit (ADR-087's reservation formula), AND (b) no earlier
	// fresh waiter on G has a multi-group set (ADR-094 D2 strict per-group
	// FIFO). (b) is vacuous when every earlier waiter is single-group.
	admit := true
	for i, g := range groups {
		jobs, err := countJobHolders(ctx, tx, g.Key, acct)
		if err != nil {
			return Admission{}, err
		}
		v := views[i]
		free := len(v.holders)+jobs+v.roundAhead < g.Limit // (a)
		if !free || v.multi > 0 {                          // (b)
			admit = false
		}
	}
	if s.afterDecision != nil {
		s.afterDecision(ctx, admit)
	}
	if !admit {
		return queued, nil
	}

	// (7) Admit: CAS in THIS transaction, then the held mark. The live-run CAS
	// re-reads the run's state after the CAS (#4035): it sits AFTER the holders
	// read, so a run that went terminal before the slots were seen free is
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
	// ONE statement marks EVERY row of the stage held, all reading ONE
	// acquired_at (the MATERIALIZED CTE is evaluated once).
	var waited int
	if err := tx.QueryRow(ctx, `WITH t AS MATERIALIZED (SELECT clock_timestamp() AS now),
marked AS (
  UPDATE stage_concurrency_slots s
  SET state = 'held', acquired_at = t.now, held_dispatched_at = st.dispatched_at,
    admission_nonce = NULLIF($2, ''), order_key = NULL
  FROM t, stages st
  WHERE st.id = s.stage_id AND s.stage_id = $1
  RETURNING GREATEST(0, floor(extract(epoch FROM s.acquired_at - s.enqueued_at)))::int AS waited
)
SELECT COALESCE(max(waited), 0) FROM marked`, req.StageID, req.AdmissionNonce).Scan(&waited); err != nil {
		return Admission{}, fmt.Errorf("concurrency: mark held: %w", err)
	}
	if !queuedBefore {
		waited = 0
	}
	out := Admission{
		Admitted:      true,
		Stage:         stage,
		Holders:       views[0].holders,
		EnqueuedAt:    enqueuedAt,
		QueuedBefore:  queuedBefore,
		WaitedSeconds: waited,
		Groups:        make([]GroupState, len(groups)),
	}
	for i, g := range groups {
		out.Groups[i] = GroupState{Key: g.Key, Scope: groupScope(g.Key), Limit: g.Limit, Holders: views[i].holders}
	}
	return out, nil
}

// upsertQueuedSet records the stage as queued in every group of its set,
// applying the episode-restart rule: the caller has verified the stage is
// host-dispatchable under its row lock, so an existing 'held' row is from a
// PREVIOUS episode. The whole set restarts at the TAIL (enqueued_at = now)
// when the stage had no rows, any row was held, its row set differs from the
// requested set, or any row is stale past QueueTTL; otherwise every row keeps
// its enqueued_at. Rows outside the set are deleted. A restart also clears
// order_key (a promotion lasts for its episode only); the keep arm leaves it
// untouched. Every row reads ONE timestamp (the MATERIALIZED CTE), so the
// stage's rows share their episode state. It returns the set's enqueued_at
// and effective ordering key, and reports whether this call inserted or
// restarted the set (newlyQueued) and whether it was already queued in this
// episode (queuedBefore).
func upsertQueuedSet(ctx context.Context, tx pgx.Tx, req Request, groups []GroupRequest) (enqueuedAt, effectiveKey time.Time, newlyQueued, queuedBefore bool, err error) {
	rows, err := tx.Query(ctx, `SELECT true, group_key,
  state = 'held' OR last_seen_at <= clock_timestamp() - `+queueTTLSQL+`
FROM stage_concurrency_slots WHERE stage_id = $1`, req.StageID)
	if err != nil {
		return time.Time{}, time.Time{}, false, false, fmt.Errorf("concurrency: read slot rows: %w", err)
	}
	want := make(map[string]bool, len(groups))
	keys := make([]string, len(groups))
	limits := make([]int32, len(groups))
	for i, g := range groups {
		want[g.Key] = true
		keys[i] = g.Key
		limits[i] = int32(g.Limit) //nolint:gosec // validated 1..MaxLimit
	}
	existing, restart, extra := 0, false, false
	for rows.Next() {
		var exists, rowRestart bool
		var key string
		if err := rows.Scan(&exists, &key, &rowRestart); err != nil {
			rows.Close()
			return time.Time{}, time.Time{}, false, false, fmt.Errorf("concurrency: scan slot row: %w", err)
		}
		existing++
		restart = restart || rowRestart
		if !want[key] {
			extra = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return time.Time{}, time.Time{}, false, false, fmt.Errorf("concurrency: read slot rows: %w", err)
	}
	exists := existing > 0
	// The set differs when a row lies outside it or a requested group has no
	// row (a full match has exactly len(groups) rows, none extra).
	if !exists || extra || existing != len(groups) {
		restart = true
	}
	if extra {
		if _, err := tx.Exec(ctx, `DELETE FROM stage_concurrency_slots WHERE stage_id = $1 AND NOT (group_key = ANY($2::text[]))`, req.StageID, keys); err != nil {
			return time.Time{}, time.Time{}, false, false, fmt.Errorf("concurrency: delete out-of-set slot rows: %w", err)
		}
	}
	err = tx.QueryRow(ctx, `WITH t AS MATERIALIZED (SELECT clock_timestamp() AS now),
upserted AS (
  INSERT INTO stage_concurrency_slots AS s
    (stage_id, run_id, group_key, slot_limit, host, state, enqueued_at, last_seen_at)
  SELECT $1, $2, g.key, g.lim, $5, 'queued', t.now, t.now
  FROM t, unnest($3::text[], $4::int[]) AS g(key, lim)
  ON CONFLICT (stage_id, group_key) DO UPDATE SET
    run_id = EXCLUDED.run_id,
    slot_limit = EXCLUDED.slot_limit,
    host = EXCLUDED.host,
    state = 'queued',
    enqueued_at = CASE WHEN $6 THEN EXCLUDED.enqueued_at ELSE s.enqueued_at END,
    order_key = CASE WHEN $6 THEN NULL ELSE s.order_key END,
    last_seen_at = EXCLUDED.last_seen_at,
    acquired_at = NULL,
    held_dispatched_at = NULL,
    admission_nonce = NULL
  RETURNING s.enqueued_at, COALESCE(s.order_key, s.enqueued_at) AS effective_key
)
SELECT min(enqueued_at), min(effective_key) FROM upserted`, req.StageID, req.RunID, keys, limits, req.Host, restart).Scan(&enqueuedAt, &effectiveKey)
	if err != nil {
		return time.Time{}, time.Time{}, false, false, fmt.Errorf("concurrency: upsert slot rows: %w", err)
	}
	return enqueuedAt, effectiveKey, restart, exists && !restart, nil
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
// one: queued rows whose stage is still pending/awaiting_host_dispatch, or
// held rows that currently count (same attempt, live). Settled, parked,
// bypassed and previous-episode rows are omitted. A stage's rows are read as
// one set: Status.Groups lists every group in canonical key order, and the
// top-level fields describe the binding group (the Admission rule), so a
// single-row stage reads exactly as before #4240. It runs on the raw pool
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
		st           Status
		effectiveKey time.Time
		promoted     bool
		acct         pgtype.UUID
		activeQueued bool
		activeHeld   bool
	}
	byStage := map[uuid.UUID][]statusRow{}
	var order []uuid.UUID
	for rows.Next() {
		var r statusRow
		var stageID uuid.UUID
		var state string
		if err := rows.Scan(&stageID, &r.st.GroupKey, &r.st.Limit, &r.st.Host, &state,
			&r.st.EnqueuedAt, &r.st.AcquiredAt, &r.st.HeldDispatchedAt, &r.st.AdmissionNonce, &r.st.WaiterLive,
			&r.effectiveKey, &r.promoted, &r.acct, &r.activeQueued, &r.activeHeld); err != nil {
			rows.Close()
			return nil, fmt.Errorf("concurrency: scan status: %w", err)
		}
		r.st.State = SlotState(state)
		if _, ok := byStage[stageID]; !ok {
			order = append(order, stageID)
		}
		byStage[stageID] = append(byStage[stageID], r)
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
	for _, stageID := range order {
		set := byStage[stageID]
		sort.Slice(set, func(i, j int) bool { return set[i].st.GroupKey < set[j].st.GroupKey })
		// The set is active only when EVERY row is: its rows share one
		// episode state, so a mixed set is not an active admission.
		queued, held := true, true
		for _, r := range set {
			queued = queued && r.activeQueued
			held = held && r.activeHeld
		}
		if !queued && !held {
			continue
		}
		groups := make([]GroupState, len(set))
		positions := make([]int, len(set))
		for i, r := range set {
			key := scope{r.st.GroupKey, r.acct}
			holders, ok := holdersBy[key]
			if !ok {
				holders, err = listHolders(ctx, s.pool, r.st.GroupKey, r.acct)
				if err != nil {
					return nil, err
				}
				holdersBy[key] = holders
			}
			g := GroupState{Key: r.st.GroupKey, Scope: groupScope(r.st.GroupKey), Limit: r.st.Limit, Holders: holders}
			if queued {
				var roundAhead, allAhead, multi int
				if err := s.pool.QueryRow(ctx, aheadSQL, r.st.GroupKey, stageID, r.acct, r.effectiveKey, r.st.EnqueuedAt).Scan(&roundAhead, &allAhead, &multi); err != nil {
					return nil, fmt.Errorf("concurrency: status position: %w", err)
				}
				g.Position = allAhead + 1
			}
			groups[i], positions[i] = g, g.Position
		}
		b := 0
		if queued {
			b = bindingQueued(positions)
		}
		st := set[b].st
		st.Limit, st.Holders, st.Groups = groups[b].Limit, groups[b].Holders, groups
		if queued {
			st.Position = groups[b].Position
			st.Promoted = set[b].promoted
			st.AcquiredAt, st.HeldDispatchedAt, st.AdmissionNonce = nil, nil, ""
		} else {
			st.WaiterLive = false
		}
		out[stageID] = st
	}
	return out, nil
}

// Promote moves one queued waiter ahead of every other fresh waiter in EVERY
// group of its set and account (#4191, option a for ADR-094 D2 sets). See
// README.md § "Promote".
//
// One transaction on one pooled connection: an unlocked peek of the stage's
// account and group set, then every group advisory lock taken BLOCKING in
// canonical key order (Admit only try-locks them, so this cannot deadlock
// with an admission; an admitter arriving meanwhile answers Contended), then
// the stage row and the slot rows FOR UPDATE. It never writes a held row and
// never touches enqueued_at. Any error, including one returned by req.Record,
// rolls the whole promotion back.
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

// promoteRow is one slot row of the stage, as Promote's locked re-read sees it.
type promoteRow struct {
	group, state    string
	limit           int
	enqueuedAt, key time.Time
	fresh           bool
}

func (s *PostgresStore) promoteTx(ctx context.Context, tx pgx.Tx, req PromoteRequest) (Promotion, error) {
	// (1) Unlocked peek: the stage's run and account, and its rows' group set.
	var runID uuid.UUID
	var acct pgtype.UUID
	var peekGroups []string
	err := tx.QueryRow(ctx, `SELECT st.run_id, r.account_id,
  COALESCE(array_agg(s.group_key) FILTER (WHERE s.group_key IS NOT NULL), '{}')
FROM stages st JOIN runs r ON r.id = st.run_id
LEFT JOIN stage_concurrency_slots s ON s.stage_id = st.id
WHERE st.id = $1
GROUP BY st.run_id, r.account_id`, req.StageID).Scan(&runID, &acct, &peekGroups)
	if errors.Is(err, pgx.ErrNoRows) {
		return Promotion{}, fmt.Errorf("concurrency: promote stage %s: %w", req.StageID, run.ErrNotFound)
	}
	if err != nil {
		return Promotion{}, fmt.Errorf("concurrency: promote: read stage: %w", err)
	}
	if len(peekGroups) == 0 {
		return Promotion{}, fmt.Errorf("concurrency: promote stage %s: %w", req.StageID, ErrNotQueued)
	}
	sort.Strings(peekGroups)
	var acctPtr *uuid.UUID
	if acct.Valid {
		id := uuid.UUID(acct.Bytes)
		acctPtr = &id
	}
	if s.afterPromotePeek != nil {
		s.afterPromotePeek(ctx)
	}

	// (2) Every group lock, BLOCKING, in canonical key order: waits out an
	// in-flight admission round in any group of the set.
	for _, g := range peekGroups {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey(acctPtr, g)); err != nil {
			return Promotion{}, fmt.Errorf("concurrency: promote: group lock: %w", err)
		}
	}

	// (3) Stage row lock: every slot-row write for a stage happens under it.
	var stageState string
	if err := tx.QueryRow(ctx, `SELECT state FROM stages WHERE id = $1 FOR UPDATE`, req.StageID).Scan(&stageState); err != nil {
		return Promotion{}, fmt.Errorf("concurrency: promote: lock stage: %w", err)
	}

	// (4) Locked re-read of every slot row, then classify.
	rows, err := tx.Query(ctx, `SELECT group_key, state, slot_limit, enqueued_at, COALESCE(order_key, enqueued_at),
  last_seen_at > clock_timestamp() - `+queueTTLSQL+`
FROM stage_concurrency_slots WHERE stage_id = $1 FOR UPDATE`, req.StageID)
	if err != nil {
		return Promotion{}, fmt.Errorf("concurrency: promote: lock slot row: %w", err)
	}
	set, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (promoteRow, error) {
		var r promoteRow
		err := row.Scan(&r.group, &r.state, &r.limit, &r.enqueuedAt, &r.key, &r.fresh)
		return r, err
	})
	if err != nil {
		return Promotion{}, fmt.Errorf("concurrency: promote: lock slot row: %w", err)
	}
	if len(set) == 0 {
		return Promotion{}, fmt.Errorf("concurrency: promote stage %s: %w", req.StageID, ErrNotQueued)
	}
	sort.Slice(set, func(i, j int) bool { return set[i].group < set[j].group })
	groups := make([]string, len(set))
	anyHeld, allFresh := false, true
	for i, r := range set {
		groups[i] = r.group
		anyHeld = anyHeld || SlotState(r.state) == SlotHeld
		allFresh = allFresh && r.fresh
	}
	st := run.StageState(stageState)
	switch {
	case !slices.Equal(groups, peekGroups):
		return Promotion{}, fmt.Errorf("concurrency: promote stage %s: group set %q became %q: %w", req.StageID, peekGroups, groups, ErrSlotChanged)
	case anyHeld && (st == run.StageStateDispatched || st == run.StageStateRunning):
		return Promotion{}, fmt.Errorf("concurrency: promote stage %s: %w", req.StageID, ErrSlotHeld)
	case anyHeld:
		// A previous episode's held rows the stage has not re-polled.
		return Promotion{}, fmt.Errorf("concurrency: promote stage %s: held row from a previous episode (stage %s): %w", req.StageID, st, ErrNotQueued)
	case st != run.StageStatePending && st != run.StageStateAwaitingHostDispatch:
		return Promotion{}, fmt.Errorf("concurrency: promote stage %s: stage is %s: %w", req.StageID, st, ErrNotQueued)
	case !allFresh:
		return Promotion{}, fmt.Errorf("concurrency: promote stage %s: %w", req.StageID, ErrWaiterNotLive)
	}
	// The rows share one episode state, so the first row's keys are the set's.
	enqueuedAt, key := set[0].enqueuedAt, set[0].key

	// (5) Holders and the old position, per group.
	position := func(group string, key time.Time) (int, error) {
		var roundAhead, allAhead, multi int
		if err := tx.QueryRow(ctx, aheadSQL, group, req.StageID, acct, key, enqueuedAt).Scan(&roundAhead, &allAhead, &multi); err != nil {
			return 0, fmt.Errorf("concurrency: promote: count queue ahead: %w", err)
		}
		return allAhead + 1, nil
	}
	pgs := make([]PromotionGroup, len(set))
	oldPositions := make([]int, len(set))
	first := true
	for i, r := range set {
		holders, err := listHolders(ctx, tx, r.group, acct)
		if err != nil {
			return Promotion{}, err
		}
		oldPos, err := position(r.group, key)
		if err != nil {
			return Promotion{}, err
		}
		pgs[i] = PromotionGroup{Key: r.group, Limit: r.limit, OldPosition: oldPos, NewPosition: oldPos, Holders: holders}
		oldPositions[i] = oldPos
		first = first && oldPos == 1
	}
	b := bindingQueued(oldPositions)
	result := func(changed bool) Promotion {
		bg := pgs[b]
		return Promotion{
			StageID: req.StageID, RunID: runID, GroupKey: bg.Key, Limit: bg.Limit, Changed: changed,
			OldPosition: bg.OldPosition, NewPosition: bg.NewPosition, Holders: bg.Holders, Groups: pgs,
		}
	}
	if first {
		return result(false), nil // already first in every group: write nothing, record nothing
	}

	if s.afterPromotePosition != nil {
		s.afterPromotePosition(ctx)
	}

	// (6) Write ONE order key to every row, just ahead of every other fresh
	// waiter in any group of the set, guarded in the statement on a waiter
	// still being ahead (promoteKeySQL, #4312). Zero rows written means the
	// guard failed: the last competitor ahead went stale or was cancelled
	// since step (5), so nobody is ahead at the write instant and this is the
	// same answer as the already-first path above — nothing written, Record
	// not called. That reading is sound ONLY because step (4) holds this
	// stage's slot rows FOR UPDATE and has already classified them as queued,
	// so the UPDATE cannot miss for any other reason. A change that drops or
	// weakens that lock would turn a vanished or held row into a false
	// Changed=false 1/1 answer.
	var written *time.Time
	var n int
	if err := tx.QueryRow(ctx, promoteKeySQL, groups, req.StageID, acct, key).Scan(&written, &n); err != nil {
		return Promotion{}, fmt.Errorf("concurrency: promote: write order key: %w", err)
	}
	if n == 0 || written == nil {
		for i := range pgs {
			pgs[i].OldPosition, pgs[i].NewPosition = 1, 1
		}
		return result(false), nil
	}

	// (7) The new positions, read back under the same locks.
	for i := range pgs {
		if pgs[i].NewPosition, err = position(pgs[i].Key, *written); err != nil {
			return Promotion{}, err
		}
	}
	out := result(true)

	// (8) The caller's record (the audit append) inside this transaction.
	if req.Record != nil {
		if err := req.Record(ctx, tx, out); err != nil {
			return Promotion{}, err
		}
	}
	return out, nil
}
