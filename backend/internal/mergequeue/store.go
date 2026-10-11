package mergequeue

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the pgx-backed Store over merge_candidate_queue_entries
// (0101).
//
// Connection and lock contract (README.md § "Atomicity and lock order"): one
// mutation is ONE transaction on ONE pooled connection, under the per-base
// pg_advisory_xact_lock (a blocking lock: a mutation waits its turn rather
// than answering without deciding). Enqueue first takes a per-run lock, then
// every base lock it needs in ascending key order; the by-entry mutations
// take the entry's one base lock, then the entry row (FOR UPDATE). No path
// takes a row lock before an advisory lock, so the order is acyclic.
//
// Tenancy: every read and write runs on the raw pool with NO tenant GUC set,
// under the runtime role (a superuser today, which bypasses RLS). Account
// scoping is therefore EXPLICIT: every per-base read joins runs.account_id
// and matches the base's account (IS NOT DISTINCT FROM, so untenanted runs
// share one scope), and the lock key is per account.
type PostgresStore struct {
	pool *pgxpool.Pool

	// afterLiveCheck is a test hook, nil in production: it runs inside
	// AdmitNext after the live-entry check found none and before the oldest
	// held entry is selected — the seam the lock-serialization test parks an
	// admitter on.
	afterLiveCheck func(ctx context.Context)
	// afterPeek is a test hook, nil in production: it runs inside Enqueue
	// after the active-entry peek and before the base locks — the seam the
	// settled-between-peek-and-lock test settles the peeked entry on.
	afterPeek func(ctx context.Context)
}

var _ Store = (*PostgresStore)(nil)

// NewPostgresStore returns the Postgres queue store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// admissionStaleSQL is built from the Go constant (an integer, never input).
var admissionStaleSQL = fmt.Sprintf("interval '%d seconds'", int(AdmissionStaleAfter/time.Second))

const entryCols = `e.id, e.run_id, COALESCE(r.account_id::text, ''), e.repo, e.base_ref, e.state,
  COALESCE(e.phase, ''), COALESCE(e.eject_reason, ''), e.enqueued_at, e.anchored_head_sha,
  COALESCE(e.anchored_base_sha, ''), e.anchored_at, e.admitted_at, e.settled_at, e.updated_at`

const entryFrom = `
FROM merge_candidate_queue_entries e
JOIN runs r ON r.id = e.run_id`

// basePredicate scopes a per-base read to one account: $1 repo, $2 base ref,
// $3 account.
const basePredicate = `e.repo = $1 AND e.base_ref = $2 AND r.account_id IS NOT DISTINCT FROM $3`

func scanEntry(row pgx.Row, extra ...any) (Entry, error) {
	var e Entry
	var state, phase string
	dest := append([]any{&e.ID, &e.RunID, &e.AccountID, &e.Repo, &e.BaseRef, &state,
		&phase, &e.EjectReason, &e.EnqueuedAt, &e.AnchoredHeadSHA,
		&e.AnchoredBaseSHA, &e.AnchoredAt, &e.AdmittedAt, &e.SettledAt, &e.UpdatedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		return Entry{}, err
	}
	e.State, e.Phase = State(state), Phase(phase)
	return e, nil
}

// acctArg encodes a Base/Entry account string for IS NOT DISTINCT FROM ("" is
// NULL, the untenanted scope). Callers validate the string first.
func acctArg(account string) pgtype.UUID {
	id, err := uuid.Parse(account)
	if err != nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}

func getEntryTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Entry, error) {
	e, err := scanEntry(tx.QueryRow(ctx, `SELECT `+entryCols+entryFrom+` WHERE e.id = $1 FOR UPDATE OF e`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, fmt.Errorf("entry %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Entry{}, fmt.Errorf("mergequeue: read entry: %w", err)
	}
	return e, nil
}

func lockTx(ctx context.Context, tx pgx.Tx, keys ...int64) error {
	slices.Sort(keys)
	keys = slices.Compact(keys)
	for _, k := range keys {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, k); err != nil {
			return fmt.Errorf("mergequeue: advisory lock: %w", err)
		}
	}
	return nil
}

// Enqueue puts a run's pass in its base's queue. See README.md § "Enqueue".
func (s *PostgresStore) Enqueue(ctx context.Context, req EnqueueRequest) (EnqueueResult, error) {
	if req.RunID == uuid.Nil || req.BaseRef == "" || req.HeadSHA == "" {
		return EnqueueResult{}, fmt.Errorf("%w: run id, base ref and head sha are required", ErrInvalidRequest)
	}
	var out EnqueueResult
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var repo, acct string
		err := tx.QueryRow(ctx, `SELECT repo, COALESCE(account_id::text, '') FROM runs WHERE id = $1`, req.RunID).Scan(&repo, &acct)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("run %s: %w", req.RunID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("mergequeue: read run: %w", err)
		}
		// The per-run lock serializes every Enqueue of this run, so the
		// active entry peeked below can only stay put or be settled by a
		// by-entry mutation before the base locks are held.
		if err := lockTx(ctx, tx, runLockKey(req.RunID)); err != nil {
			return err
		}
		var peekID uuid.UUID
		var peekRepo, peekBase string
		err = tx.QueryRow(ctx, `SELECT id, repo, base_ref FROM merge_candidate_queue_entries
WHERE run_id = $1 AND state IN ('held', 'live')`, req.RunID).Scan(&peekID, &peekRepo, &peekBase)
		peeked := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("mergequeue: read active entry: %w", err)
		}
		if s.afterPeek != nil {
			s.afterPeek(ctx)
		}
		keys := []int64{lockKey(acct, repo, req.BaseRef)}
		if peeked {
			keys = append(keys, lockKey(acct, peekRepo, peekBase))
		}
		if err := lockTx(ctx, tx, keys...); err != nil {
			return err
		}

		// Re-read the peeked entry under its base lock: a by-entry mutation
		// may have settled it after the peek, and a terminal row is never
		// re-anchored (the run re-enters with a fresh row).
		var cur *Entry
		if peeked {
			e, err := getEntryTx(ctx, tx, peekID)
			if err != nil {
				return err
			}
			if e.State.Active() {
				cur = &e
			}
		}
		switch {
		case cur != nil && cur.Repo == repo && cur.BaseRef == req.BaseRef:
			e, changed, err := reanchorTx(ctx, tx, *cur, req.HeadSHA, req.BaseSHA)
			if err != nil {
				return err
			}
			out = EnqueueResult{Entry: e, Outcome: EnqueueReanchored, AnchorChanged: changed}
			return nil
		case cur != nil:
			dropped, err := settleTx(ctx, tx, cur.ID, StateDropped, ReasonBaseChanged)
			if err != nil {
				return err
			}
			e, err := insertTx(ctx, tx, req, repo)
			if err != nil {
				return err
			}
			out = EnqueueResult{Entry: e, Outcome: EnqueueBaseChanged, Dropped: &dropped}
			return nil
		default:
			e, err := insertTx(ctx, tx, req, repo)
			if err != nil {
				return err
			}
			out = EnqueueResult{Entry: e, Outcome: EnqueueInserted}
			return nil
		}
	})
	if err != nil {
		return EnqueueResult{}, err
	}
	return out, nil
}

// insertTx inserts a held row at the base's tail: enqueued_at (the FIFO key)
// and anchored_at are one database-clock instant.
func insertTx(ctx context.Context, tx pgx.Tx, req EnqueueRequest, repo string) (Entry, error) {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO merge_candidate_queue_entries
  (run_id, repo, base_ref, state, enqueued_at, anchored_head_sha, anchored_base_sha, anchored_at)
SELECT $1, $2, $3, 'held', c.ts, $4, NULLIF($5, ''), c.ts
FROM (SELECT clock_timestamp() AS ts) c
RETURNING id`, req.RunID, repo, req.BaseRef, req.HeadSHA, req.BaseSHA).Scan(&id); err != nil {
		return Entry{}, fmt.Errorf("mergequeue: insert entry: %w", err)
	}
	return getEntryTx(ctx, tx, id)
}

// reanchorTx moves an active entry's anchor in place, keeping its state,
// phase and enqueued_at. An unchanged anchor writes nothing.
func reanchorTx(ctx context.Context, tx pgx.Tx, cur Entry, headSHA, baseSHA string) (Entry, bool, error) {
	if cur.AnchoredHeadSHA == headSHA && cur.AnchoredBaseSHA == baseSHA {
		return cur, false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE merge_candidate_queue_entries
SET anchored_head_sha = $2, anchored_base_sha = NULLIF($3, ''), anchored_at = clock_timestamp()
WHERE id = $1`, cur.ID, headSHA, baseSHA); err != nil {
		return Entry{}, false, fmt.Errorf("mergequeue: reanchor entry: %w", err)
	}
	e, err := getEntryTx(ctx, tx, cur.ID)
	return e, true, err
}

// settleTx moves an ACTIVE entry to a terminal state.
func settleTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, state State, reason string) (Entry, error) {
	if _, err := tx.Exec(ctx, `UPDATE merge_candidate_queue_entries
SET state = $2, eject_reason = NULLIF($3, ''), settled_at = clock_timestamp()
WHERE id = $1 AND state IN ('held', 'live')`, id, string(state), reason); err != nil {
		return Entry{}, fmt.Errorf("mergequeue: settle entry: %w", err)
	}
	return getEntryTx(ctx, tx, id)
}

// AdmitNext admits the base's oldest held entry when the base has no live
// one. See README.md § "AdmitNext".
func (s *PostgresStore) AdmitNext(ctx context.Context, base Base) (Admission, error) {
	if err := base.validate(); err != nil {
		return Admission{}, err
	}
	acct := acctArg(base.AccountID)
	var out Admission
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockTx(ctx, tx, lockKey(base.AccountID, base.Repo, base.BaseRef)); err != nil {
			return err
		}
		var stale bool
		live, err := scanEntry(tx.QueryRow(ctx, `SELECT `+entryCols+`,
  (e.phase = '`+string(PhaseAdmitted)+`' AND e.admitted_at <= clock_timestamp() - `+admissionStaleSQL+`)`+entryFrom+`
WHERE `+basePredicate+` AND e.state = 'live'
ORDER BY e.admitted_at, e.id
LIMIT 1
FOR UPDATE OF e`, base.Repo, base.BaseRef, acct), &stale)
		switch {
		case err == nil && stale:
			// A crash mid-admission: re-claim it so exactly one pump re-runs
			// the (idempotent) admission work.
			if _, err := tx.Exec(ctx, `UPDATE merge_candidate_queue_entries SET admitted_at = clock_timestamp() WHERE id = $1`, live.ID); err != nil {
				return fmt.Errorf("mergequeue: reclaim admission: %w", err)
			}
			e, err := getEntryTx(ctx, tx, live.ID)
			if err != nil {
				return err
			}
			out = Admission{Admitted: true, Readmitted: true, Entry: &e}
			return nil
		case err == nil:
			out = Admission{Entry: &live}
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("mergequeue: read live entry: %w", err)
		}
		if s.afterLiveCheck != nil {
			s.afterLiveCheck(ctx)
		}
		next, err := scanEntry(tx.QueryRow(ctx, `SELECT `+entryCols+entryFrom+`
WHERE `+basePredicate+` AND e.state = 'held'
ORDER BY e.enqueued_at, e.id
LIMIT 1
FOR UPDATE OF e`, base.Repo, base.BaseRef, acct))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("mergequeue: read next held entry: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE merge_candidate_queue_entries
SET state = 'live', phase = $2, admitted_at = clock_timestamp()
WHERE id = $1 AND state = 'held'`, next.ID, string(PhaseAdmitted)); err != nil {
			return fmt.Errorf("mergequeue: admit entry: %w", err)
		}
		e, err := getEntryTx(ctx, tx, next.ID)
		if err != nil {
			return err
		}
		out = Admission{Admitted: true, Entry: &e}
		return nil
	})
	if err != nil {
		return Admission{}, err
	}
	return out, nil
}

// withEntry runs fn in one transaction holding the entry's base lock and its
// row lock. The base lock is derived from a plain read first; repo and
// base_ref never change on a row, so the key stays correct.
func (s *PostgresStore) withEntry(ctx context.Context, id uuid.UUID, fn func(tx pgx.Tx, cur Entry) error) error {
	if id == uuid.Nil {
		return fmt.Errorf("%w: entry id is required", ErrInvalidRequest)
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var repo, baseRef, acct string
		err := tx.QueryRow(ctx, `SELECT e.repo, e.base_ref, COALESCE(r.account_id::text, '')`+entryFrom+` WHERE e.id = $1`, id).
			Scan(&repo, &baseRef, &acct)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("entry %s: %w", id, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("mergequeue: read entry base: %w", err)
		}
		if err := lockTx(ctx, tx, lockKey(acct, repo, baseRef)); err != nil {
			return err
		}
		cur, err := getEntryTx(ctx, tx, id)
		if err != nil {
			return err
		}
		return fn(tx, cur)
	})
}

func stateConflict(cur Entry, want string) error {
	return fmt.Errorf("%w: entry %s is %s, want %s", ErrStateConflict, cur.ID, cur.State, want)
}

// Unadmit returns a live entry to held at its ORIGINAL place (enqueued_at is
// kept), clearing its phase and admitted_at — for a transient admission
// failure. A non-live entry is ErrStateConflict and is not touched.
func (s *PostgresStore) Unadmit(ctx context.Context, entryID uuid.UUID) (Entry, error) {
	var out Entry
	err := s.withEntry(ctx, entryID, func(tx pgx.Tx, cur Entry) error {
		if cur.State != StateLive {
			return stateConflict(cur, string(StateLive))
		}
		if _, err := tx.Exec(ctx, `UPDATE merge_candidate_queue_entries
SET state = 'held', phase = NULL, admitted_at = NULL
WHERE id = $1`, cur.ID); err != nil {
			return fmt.Errorf("mergequeue: unadmit entry: %w", err)
		}
		e, err := getEntryTx(ctx, tx, cur.ID)
		out = e
		return err
	})
	if err != nil {
		return Entry{}, err
	}
	return out, nil
}

// SetPhase records an active entry's phase. A terminal entry is
// ErrStateConflict; an unchanged phase writes nothing.
func (s *PostgresStore) SetPhase(ctx context.Context, entryID uuid.UUID, phase Phase) (Entry, error) {
	if phase == "" {
		return Entry{}, fmt.Errorf("%w: phase is required", ErrInvalidRequest)
	}
	var out Entry
	err := s.withEntry(ctx, entryID, func(tx pgx.Tx, cur Entry) error {
		if !cur.State.Active() {
			return stateConflict(cur, "held or live")
		}
		if cur.Phase == phase {
			out = cur
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE merge_candidate_queue_entries SET phase = $2 WHERE id = $1`, cur.ID, string(phase)); err != nil {
			return fmt.Errorf("mergequeue: set phase: %w", err)
		}
		e, err := getEntryTx(ctx, tx, cur.ID)
		out = e
		return err
	})
	if err != nil {
		return Entry{}, err
	}
	return out, nil
}

// Reanchor moves an active entry's anchored head (and base tip) in place,
// keeping state, phase and enqueued_at. A terminal entry is
// ErrStateConflict; an unchanged anchor writes nothing.
func (s *PostgresStore) Reanchor(ctx context.Context, entryID uuid.UUID, headSHA, baseSHA string) (Entry, error) {
	if headSHA == "" {
		return Entry{}, fmt.Errorf("%w: head sha is required", ErrInvalidRequest)
	}
	var out Entry
	err := s.withEntry(ctx, entryID, func(tx pgx.Tx, cur Entry) error {
		if !cur.State.Active() {
			return stateConflict(cur, "held or live")
		}
		e, _, err := reanchorTx(ctx, tx, cur, headSHA, baseSHA)
		out = e
		return err
	})
	if err != nil {
		return Entry{}, err
	}
	return out, nil
}

// Settle moves an active entry to merged, ejected or dropped. Ejected and
// dropped require a reason. On an already-terminal entry it writes nothing
// and returns that entry with its first state (Settled=false).
func (s *PostgresStore) Settle(ctx context.Context, entryID uuid.UUID, state State, reason string) (SettleResult, error) {
	if !state.Terminal() {
		return SettleResult{}, fmt.Errorf("%w: settle state %q is not merged, ejected or dropped", ErrInvalidRequest, state)
	}
	if state != StateMerged && reason == "" {
		return SettleResult{}, fmt.Errorf("%w: a %s entry requires a reason", ErrInvalidRequest, state)
	}
	var out SettleResult
	err := s.withEntry(ctx, entryID, func(tx pgx.Tx, cur Entry) error {
		if !cur.State.Active() {
			out = SettleResult{Entry: cur}
			return nil
		}
		e, err := settleTx(ctx, tx, cur.ID, state, reason)
		out = SettleResult{Entry: e, Settled: true}
		return err
	})
	if err != nil {
		return SettleResult{}, err
	}
	return out, nil
}

// ActiveForRun returns the run's active entry with its held position and the
// base's live run, counted within the run's own account. found is false when
// the run has no active entry.
func (s *PostgresStore) ActiveForRun(ctx context.Context, runID uuid.UUID) (Status, bool, error) {
	var st Status
	var liveRun pgtype.UUID
	e, err := scanEntry(s.pool.QueryRow(ctx, `SELECT `+entryCols+`,
  CASE WHEN e.state = 'held' THEN (
    SELECT count(*) FROM merge_candidate_queue_entries h JOIN runs hr ON hr.id = h.run_id
    WHERE h.repo = e.repo AND h.base_ref = e.base_ref AND h.state = 'held'
      AND hr.account_id IS NOT DISTINCT FROM r.account_id
      AND (h.enqueued_at, h.id) <= (e.enqueued_at, e.id)
  ) ELSE 0 END,
  (SELECT l.run_id FROM merge_candidate_queue_entries l JOIN runs lr ON lr.id = l.run_id
    WHERE l.repo = e.repo AND l.base_ref = e.base_ref AND l.state = 'live'
      AND lr.account_id IS NOT DISTINCT FROM r.account_id
    ORDER BY l.admitted_at, l.id LIMIT 1)`+entryFrom+`
WHERE e.run_id = $1 AND e.state IN ('held', 'live')`, runID), &st.Position, &liveRun)
	if errors.Is(err, pgx.ErrNoRows) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, fmt.Errorf("mergequeue: active entry for run: %w", err)
	}
	st.Entry = e
	if liveRun.Valid {
		id := uuid.UUID(liveRun.Bytes)
		st.LiveRunID = &id
	}
	return st, true, nil
}

// ListForBase returns the base's active entries in queue order: the live
// entry first, then held entries by (enqueued_at, id). Never nil.
func (s *PostgresStore) ListForBase(ctx context.Context, base Base) ([]Entry, error) {
	if err := base.validate(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+entryCols+entryFrom+`
WHERE `+basePredicate+` AND e.state IN ('held', 'live')
ORDER BY (e.state = 'live') DESC, e.enqueued_at, e.id`, base.Repo, base.BaseRef, acctArg(base.AccountID))
	if err != nil {
		return nil, fmt.Errorf("mergequeue: list base: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) { return scanEntry(row) })
	if err != nil {
		return nil, fmt.Errorf("mergequeue: scan base: %w", err)
	}
	return out, nil
}
