package mergequeue

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pglock"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
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
	// locks is the pool-wide pglock holder budget WithMergeIntentLock shares
	// with every other pglock helper on the pool, so lock holders alone can
	// never occupy every connection their fns need (README.md § "Merge-intent
	// lock").
	locks *pglock.Budget

	// afterLiveCheck is a test hook, nil in production: it runs inside
	// AdmitNext after the live-entry check found none and before the oldest
	// held entry is selected — the seam the lock-serialization test parks an
	// admitter on.
	afterLiveCheck func(ctx context.Context)
	// afterPeek is a test hook, nil in production: it runs inside Enqueue
	// after the active-entry peek and before the base locks — the seam the
	// settled-between-peek-and-lock test settles the peeked entry on.
	afterPeek func(ctx context.Context)
	// afterPromotePeek is a test hook, nil in production: it runs inside
	// Promote after the unlocked active-entry peek and before the locks.
	afterPromotePeek func(ctx context.Context)
	// afterPromotePosition is a test hook, nil in production: it runs inside
	// Promote after the old-position read and before the guarded write.
	afterPromotePosition func(ctx context.Context)
}

var (
	_ Store          = (*PostgresStore)(nil)
	_ Promoter       = (*PostgresStore)(nil)
	_ EjectionReader = (*PostgresStore)(nil)
	_ ReentryReader  = (*PostgresStore)(nil)
)

// NewPostgresStore returns the Postgres queue store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool, locks: pglock.BudgetFor(pool)}
}

// admissionStaleSQL is built from the Go constant (an integer, never input).
var admissionStaleSQL = fmt.Sprintf("interval '%d seconds'", int(AdmissionStaleAfter/time.Second))

const entryCols = `e.id, e.run_id, COALESCE(r.account_id::text, ''), e.repo, e.base_ref, e.state,
  COALESCE(e.phase, ''), COALESCE(e.eject_reason, ''), e.enqueued_at, e.anchored_head_sha,
  COALESCE(e.anchored_base_sha, ''), e.anchored_at, e.admitted_at, e.settled_at, e.updated_at,
  e.merge_intent_at, COALESCE(e.merge_intent_sequence, 0), e.admission_epoch,
  COALESCE(e.yield_reason, ''), COALESCE(e.yield_detail, ''), e.yielded_at, e.approval_wait_started_at,
  COALESCE(GREATEST(0, (EXTRACT(EPOCH FROM clock_timestamp() - e.yielded_at) * 1000000)::bigint), 0),
  e.order_key, COALESCE(GREATEST(0, (EXTRACT(EPOCH FROM clock_timestamp() - e.settled_at) * 1000000)::bigint), 0)`

// effectiveKeySQL is an entry's queue-order key on alias e: the promote's
// order_key when set (0111), else enqueued_at. Every row without a promotion
// is therefore strict FIFO by eligibility time.
const effectiveKeySQL = `COALESCE(e.order_key, e.enqueued_at)`

const entryFrom = `
FROM merge_candidate_queue_entries e
JOIN runs r ON r.id = e.run_id`

// basePredicate scopes a per-base read to one account: $1 repo, $2 base ref,
// $3 account.
const basePredicate = `e.repo = $1 AND e.base_ref = $2 AND r.account_id IS NOT DISTINCT FROM $3`

func scanEntry(row pgx.Row, extra ...any) (Entry, error) {
	var e Entry
	var state, phase string
	var yieldAgeUS, settledAgeUS int64
	dest := append([]any{&e.ID, &e.RunID, &e.AccountID, &e.Repo, &e.BaseRef, &state,
		&phase, &e.EjectReason, &e.EnqueuedAt, &e.AnchoredHeadSHA,
		&e.AnchoredBaseSHA, &e.AnchoredAt, &e.AdmittedAt, &e.SettledAt, &e.UpdatedAt,
		&e.MergeIntentAt, &e.MergeIntentSequence, &e.AdmissionEpoch,
		&e.YieldReason, &e.YieldDetail, &e.YieldedAt, &e.ApprovalWaitStartedAt, &yieldAgeUS,
		&e.OrderKey, &settledAgeUS}, extra...)
	if err := row.Scan(dest...); err != nil {
		return Entry{}, err
	}
	e.State, e.Phase = State(state), Phase(phase)
	e.YieldAge = time.Duration(yieldAgeUS) * time.Microsecond
	e.SettledAge = time.Duration(settledAgeUS) * time.Microsecond
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
	if req.Yield != nil {
		if err := req.Yield.validate(); err != nil {
			return EnqueueResult{}, err
		}
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
			if req.MergeIntent {
				e, recorded, err := markIntentTx(ctx, tx, e, req.IntentSequence)
				if err != nil {
					return err
				}
				out.Entry, out.IntentRecorded = e, recorded
			}
			// A live entry is never marked: merge_run's yield applies only to
			// an entry that is not holding the line.
			if req.Yield != nil && out.Entry.State == StateHeld { // Enqueue marker live-row guard
				e, err := markYieldTx(ctx, tx, out.Entry, *req.Yield)
				if err != nil {
					return err
				}
				out.Entry, out.Marked = e, true
			}
		case cur != nil:
			dropped, err := settleTx(ctx, tx, cur.ID, StateDropped, ReasonBaseChanged)
			if err != nil {
				return err
			}
			e, err := insertTx(ctx, tx, req, repo)
			if err != nil {
				return err
			}
			out = EnqueueResult{Entry: e, Outcome: EnqueueBaseChanged, Dropped: &dropped, IntentRecorded: req.MergeIntent, Marked: req.Yield != nil}
		default:
			e, err := insertTx(ctx, tx, req, repo)
			if err != nil {
				return err
			}
			out = EnqueueResult{Entry: e, Outcome: EnqueueInserted, IntentRecorded: req.MergeIntent, Marked: req.Yield != nil}
		}
		if out.IntentRecorded {
			return recordIntentTx(ctx, tx, out.Entry.RunID, req.RecordIntent)
		}
		return nil
	})
	if err != nil {
		return EnqueueResult{}, err
	}
	return out, nil
}

// insertTx inserts a held row at the base's tail with no order_key (a fresh
// row is never promoted, so a re-entry joins behind every held entry):
// enqueued_at (the FIFO key),
// anchored_at, (with MergeIntent) merge_intent_at and (with Yield) yielded_at
// are one database-clock instant, and merge_intent_sequence and the yield
// reason and detail are stamped in the same statement.
func insertTx(ctx context.Context, tx pgx.Tx, req EnqueueRequest, repo string) (Entry, error) {
	var id uuid.UUID
	var yieldReason, yieldDetail string
	if req.Yield != nil {
		yieldReason, yieldDetail = req.Yield.Reason, req.Yield.Detail
	}
	if err := tx.QueryRow(ctx, `INSERT INTO merge_candidate_queue_entries
  (run_id, repo, base_ref, state, enqueued_at, anchored_head_sha, anchored_base_sha, anchored_at, merge_intent_at, merge_intent_sequence,
   yield_reason, yield_detail, yielded_at)
SELECT $1, $2, $3, 'held', c.ts, $4, NULLIF($5, ''), c.ts, CASE WHEN $6::bool THEN c.ts END,
  CASE WHEN $6::bool THEN NULLIF($7::bigint, 0) END,
  NULLIF($8::text, ''), CASE WHEN $8::text <> '' THEN NULLIF($9::text, '') END, CASE WHEN $8::text <> '' THEN c.ts END
FROM (SELECT clock_timestamp() AS ts) c
RETURNING id`, req.RunID, repo, req.BaseRef, req.HeadSHA, req.BaseSHA, req.MergeIntent, req.IntentSequence,
		yieldReason, yieldDetail).Scan(&id); err != nil {
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

// markYieldTx stamps a HELD entry's yield marker: reason and detail are the
// request's, and yielded_at keeps an existing value (COALESCE), so a re-marked
// entry stays in the same yield episode and its approval-wait anchor is kept;
// an unmarked entry continues m.Continue's cleared episode when its anchor
// matches (yieldContinueSQL), else stamps the database clock. The caller
// guarantees the entry is held (a live row is never marked; the 0109 CHECK
// refuses one).
func markYieldTx(ctx context.Context, tx pgx.Tx, cur Entry, m YieldMarker) (Entry, error) {
	contAt, contHead, contBase := m.continueArgs()
	if _, err := tx.Exec(ctx, `UPDATE merge_candidate_queue_entries
SET yield_reason = $2, yield_detail = NULLIF($3, ''), yielded_at = COALESCE(yielded_at, `+yieldContinueSQL+`, clock_timestamp())
WHERE id = $1`, cur.ID, m.Reason, m.Detail, contAt, contHead, contBase); err != nil {
		return Entry{}, fmt.Errorf("mergequeue: mark yield: %w", err)
	}
	return getEntryTx(ctx, tx, cur.ID)
}

// markIntentTx stamps an active entry's merge_intent_at, and seq as its
// merge_intent_sequence in the SAME statement, when merge_intent_at is NULL,
// and reports whether it did; an already-set intent writes nothing, so a
// recorded sequence is never overwritten.
func markIntentTx(ctx context.Context, tx pgx.Tx, cur Entry, seq int64) (Entry, bool, error) {
	if cur.MergeIntentAt != nil {
		return cur, false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE merge_candidate_queue_entries
SET merge_intent_at = clock_timestamp(), merge_intent_sequence = NULLIF($2::bigint, 0)
WHERE id = $1 AND merge_intent_at IS NULL`, cur.ID, seq); err != nil {
		return Entry{}, false, fmt.Errorf("mergequeue: mark merge intent: %w", err)
	}
	e, err := getEntryTx(ctx, tx, cur.ID)
	return e, true, err
}

// recordIntentTx runs a merge-intent call's record-before-commit callback
// on the run's Status read inside tx; its error rolls the call back.
func recordIntentTx(ctx context.Context, tx pgx.Tx, runID uuid.UUID, record RecordIntentFunc) error {
	if record == nil {
		return nil
	}
	st, found, err := activeForRun(ctx, tx, runID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("mergequeue: run %s has no active entry to record intent on", runID)
	}
	if err := record(ctx, st); err != nil {
		return fmt.Errorf("%w: %w", ErrIntentUnrecorded, err)
	}
	return nil
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

// AdmitNext admits the base's first UNMARKED held entry by effective key
// (effectiveKeySQL, id) when the base has no live one. See README.md §
// "AdmitNext".
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
			if _, err := tx.Exec(ctx, `UPDATE merge_candidate_queue_entries
SET admitted_at = clock_timestamp(), admission_epoch = admission_epoch + 1
WHERE id = $1`, live.ID); err != nil {
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
		// A yielded (marked) held entry is skipped entirely: clearing its
		// marker is the only way back in (ADR-112 R3).
		next, err := scanEntry(tx.QueryRow(ctx, `SELECT `+entryCols+entryFrom+`
WHERE `+basePredicate+` AND e.state = 'held' AND e.yielded_at IS NULL
ORDER BY `+effectiveKeySQL+`, e.id
LIMIT 1
FOR UPDATE OF e`, base.Repo, base.BaseRef, acct))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("mergequeue: read next held entry: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE merge_candidate_queue_entries
SET state = 'live', phase = $2, admitted_at = clock_timestamp(), admission_epoch = admission_epoch + 1
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

// epochConflict is a CAS refusal: the entry is no longer live at the epoch
// the caller's decision was made against.
func epochConflict(cur Entry, epoch int64) error {
	return fmt.Errorf("%w: entry %s is %s at admission epoch %d, want live at epoch %d",
		ErrStateConflict, cur.ID, cur.State, cur.AdmissionEpoch, epoch)
}

// Unadmit returns a live entry to held at its ORIGINAL place (enqueued_at is
// kept), clearing its phase and admitted_at — for a transient admission
// failure or a merge-on-pass readvance. It is a compare-and-swap on the
// admission epoch: an entry that is not live, or was re-admitted since epoch,
// is ErrStateConflict and is not touched.
func (s *PostgresStore) Unadmit(ctx context.Context, entryID uuid.UUID, epoch int64) (Entry, error) {
	var out Entry
	err := s.withEntry(ctx, entryID, func(tx pgx.Tx, cur Entry) error {
		if cur.State != StateLive || cur.AdmissionEpoch != epoch { // Unadmit admission-epoch CAS
			return epochConflict(cur, epoch)
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

// SetPhase records a LIVE entry's phase, a compare-and-swap on the admission
// epoch: an entry that is not live, or was re-admitted since epoch, is
// ErrStateConflict and is not touched; an unchanged phase writes nothing.
func (s *PostgresStore) SetPhase(ctx context.Context, entryID uuid.UUID, epoch int64, phase Phase) (Entry, error) {
	if phase == "" {
		return Entry{}, fmt.Errorf("%w: phase is required", ErrInvalidRequest)
	}
	var out Entry
	err := s.withEntry(ctx, entryID, func(tx pgx.Tx, cur Entry) error {
		if cur.State != StateLive || cur.AdmissionEpoch != epoch { // SetPhase admission-epoch CAS
			return epochConflict(cur, epoch)
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

// ReanchorAt is Reanchor for the admission owner: a compare-and-swap on the
// admission epoch. An entry that is not live, or was re-admitted since epoch,
// is ErrStateConflict and is not touched; an unchanged anchor writes nothing.
func (s *PostgresStore) ReanchorAt(ctx context.Context, entryID uuid.UUID, epoch int64, headSHA, baseSHA string) (Entry, error) {
	if headSHA == "" {
		return Entry{}, fmt.Errorf("%w: head sha is required", ErrInvalidRequest)
	}
	var out Entry
	err := s.withEntry(ctx, entryID, func(tx pgx.Tx, cur Entry) error {
		if cur.State != StateLive || cur.AdmissionEpoch != epoch { // ReanchorAt admission-epoch CAS
			return epochConflict(cur, epoch)
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

// validateSettle refuses a malformed settle before any SQL runs.
func validateSettle(state State, reason string) error {
	if !state.Terminal() {
		return fmt.Errorf("%w: settle state %q is not merged, ejected or dropped", ErrInvalidRequest, state)
	}
	if state != StateMerged && reason == "" {
		return fmt.Errorf("%w: a %s entry requires a reason", ErrInvalidRequest, state)
	}
	return nil
}

// SettleAt is Settle for the admission owner and merge-on-pass decisions: a
// compare-and-swap on the admission epoch. An already-terminal entry writes
// nothing and returns that entry (Settled=false), like Settle; a held entry,
// or a live one re-admitted since epoch, is ErrStateConflict and is not
// touched.
func (s *PostgresStore) SettleAt(ctx context.Context, entryID uuid.UUID, epoch int64, state State, reason string) (SettleResult, error) {
	if err := validateSettle(state, reason); err != nil {
		return SettleResult{}, err
	}
	var out SettleResult
	err := s.withEntry(ctx, entryID, func(tx pgx.Tx, cur Entry) error {
		if cur.State.Terminal() {
			out = SettleResult{Entry: cur}
			return nil
		}
		if cur.State != StateLive || cur.AdmissionEpoch != epoch { // SettleAt admission-epoch CAS
			return epochConflict(cur, epoch)
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

// Settle moves an active entry to merged, ejected or dropped. Ejected and
// dropped require a reason. On an already-terminal entry it writes nothing
// and returns that entry with its first state (Settled=false).
func (s *PostgresStore) Settle(ctx context.Context, entryID uuid.UUID, state State, reason string) (SettleResult, error) {
	if err := validateSettle(state, reason); err != nil {
		return SettleResult{}, err
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

// MarkMergeIntent records the operator's merge intent on an ACTIVE entry,
// stamping req.IntentSequence as merge_intent_sequence in the same statement.
// A terminal entry is ErrStateConflict; an already-set intent writes nothing
// (the recorded sequence is never overwritten) and calls no record
// (IntentRecorded=false). req.Record, when non-nil, runs inside the
// transaction after the write and before commit; its error rolls the write
// back (ErrIntentUnrecorded). A nil req records with neither. Lock order is
// SetPhase's.
func (s *PostgresStore) MarkMergeIntent(ctx context.Context, entryID uuid.UUID, req *MergeIntentRequest) (MergeIntentResult, error) {
	var seq int64
	var record RecordIntentFunc
	if req != nil {
		seq, record = req.IntentSequence, req.Record
	}
	var out MergeIntentResult
	err := s.withEntry(ctx, entryID, func(tx pgx.Tx, cur Entry) error {
		if !cur.State.Active() {
			return stateConflict(cur, "held or live")
		}
		e, recorded, err := markIntentTx(ctx, tx, cur, seq)
		if err != nil {
			return err
		}
		out = MergeIntentResult{Entry: e, IntentRecorded: recorded}
		if !recorded {
			return nil
		}
		return recordIntentTx(ctx, tx, e.RunID, record)
	})
	if err != nil {
		return MergeIntentResult{}, err
	}
	return out, nil
}

// ApprovalWait starts or continues the head-of-line approval wait of a LIVE
// entry (ADR-112 R3): a compare-and-swap on the admission epoch that stamps
// approval_wait_started_at once per yield episode (COALESCE: a readvance, a
// re-admission and a stale re-claim never reset it) and judges window's
// expiry against it on the database clock, in one transaction. A held or
// terminal entry, or one re-admitted since epoch, is ErrStateConflict and is
// not touched; window <= 0 is ErrInvalidRequest. See README.md § "Yield".
func (s *PostgresStore) ApprovalWait(ctx context.Context, entryID uuid.UUID, epoch int64, window time.Duration) (ApprovalWaitResult, error) {
	if window <= 0 {
		return ApprovalWaitResult{}, fmt.Errorf("%w: approval wait window %s, want a positive duration", ErrInvalidRequest, window)
	}
	var out ApprovalWaitResult
	err := s.withEntry(ctx, entryID, func(tx pgx.Tx, cur Entry) error {
		if cur.State != StateLive || cur.AdmissionEpoch != epoch { // ApprovalWait admission-epoch CAS
			return epochConflict(cur, epoch)
		}
		if _, err := tx.Exec(ctx, `UPDATE merge_candidate_queue_entries
SET approval_wait_started_at = COALESCE(approval_wait_started_at, clock_timestamp())
WHERE id = $1`, cur.ID); err != nil {
			return fmt.Errorf("mergequeue: start approval wait: %w", err)
		}
		e, err := getEntryTx(ctx, tx, cur.ID)
		if err != nil {
			return err
		}
		var remainingUS int64
		if err := tx.QueryRow(ctx, `SELECT GREATEST(0, (EXTRACT(EPOCH FROM
  (approval_wait_started_at + $2::bigint * interval '1 microsecond') - clock_timestamp()) * 1000000)::bigint)
FROM merge_candidate_queue_entries WHERE id = $1`, cur.ID, window.Microseconds()).Scan(&remainingUS); err != nil {
			return fmt.Errorf("mergequeue: judge approval wait: %w", err)
		}
		out = ApprovalWaitResult{Entry: e, Expired: remainingUS == 0, Remaining: time.Duration(remainingUS) * time.Microsecond}
		return nil
	})
	if err != nil {
		return ApprovalWaitResult{}, err
	}
	return out, nil
}

// Yield moves a LIVE entry at epoch to held and marks it yielded, in ONE
// statement (ADR-112 R3): state held, phase and admitted_at cleared,
// enqueued_at and order_key kept (a cleared row rejoins at its original
// effective key),
// yield_reason, yield_detail and yielded_at (database clock, or the
// marker's continued episode when the anchor matches it: yieldContinueSQL)
// stamped, and approval_wait_started_at kept. It is a compare-and-swap on the
// admission epoch: a held or terminal entry, or one re-admitted since epoch,
// is ErrStateConflict and is not touched. An empty reason is
// ErrInvalidRequest.
func (s *PostgresStore) Yield(ctx context.Context, entryID uuid.UUID, epoch int64, marker YieldMarker) (Entry, error) {
	if err := marker.validate(); err != nil {
		return Entry{}, err
	}
	var out Entry
	err := s.withEntry(ctx, entryID, func(tx pgx.Tx, cur Entry) error {
		if cur.State != StateLive || cur.AdmissionEpoch != epoch { // Yield admission-epoch CAS
			return epochConflict(cur, epoch)
		}
		contAt, contHead, contBase := marker.continueArgs()
		if _, err := tx.Exec(ctx, `UPDATE merge_candidate_queue_entries
SET state = 'held', phase = NULL, admitted_at = NULL,
    yield_reason = $2, yield_detail = NULLIF($3, ''), yielded_at = COALESCE(`+yieldContinueSQL+`, clock_timestamp())
WHERE id = $1`, cur.ID, marker.Reason, marker.Detail, contAt, contHead, contBase); err != nil {
			return fmt.Errorf("mergequeue: yield entry: %w", err)
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

// yieldContinueSQL is the episode continuation of a yield write ($4 the
// continued yielded_at, $5/$6 its head and base, YieldMarker.Continue): the
// continued yielded_at when the row's anchor equals it, else NULL (the caller
// COALESCEs to the database clock, a new episode). It is judged in the write's
// own statement, so a re-anchor earlier in the same transaction counts.
const yieldContinueSQL = `CASE WHEN anchored_head_sha = $5::text AND anchored_base_sha IS NOT DISTINCT FROM NULLIF($6::text, '')
  THEN $4::timestamptz END`

// yieldCASTx runs one compare-and-swap UPDATE on a yielded entry: set is the
// SET clause, args from $4. The predicate is the marker's identity: state
// held, the anchored head the caller judged ($2) and the episode's yielded_at
// ($3). A row matching none is ErrStateConflict and is not touched.
func (s *PostgresStore) yieldCASTx(ctx context.Context, op string, entryID uuid.UUID, headSHA string, yieldedAt time.Time, set string, args ...any) (Entry, error) {
	if headSHA == "" || yieldedAt.IsZero() {
		return Entry{}, fmt.Errorf("%w: %s requires the anchored head sha and yielded_at", ErrInvalidRequest, op)
	}
	var out Entry
	err := s.withEntry(ctx, entryID, func(tx pgx.Tx, cur Entry) error {
		tag, err := tx.Exec(ctx, `UPDATE merge_candidate_queue_entries SET `+set+`
WHERE id = $1 AND state = 'held' AND anchored_head_sha = $2 AND yielded_at = $3`, // yield marker CAS
			append([]any{cur.ID, headSHA, yieldedAt}, args...)...)
		if err != nil {
			return fmt.Errorf("mergequeue: %s: %w", op, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: %s: entry %s is %s at head %q yielded at %v, want held at head %q yielded at %v",
				ErrStateConflict, op, cur.ID, cur.State, cur.AnchoredHeadSHA, cur.YieldedAt, headSHA, yieldedAt)
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

// ClearYield clears a yielded entry's marker AND its approval-wait anchor (a
// later non-approval starts a fresh episode), a compare-and-swap on (held,
// anchored_head_sha, yielded_at) under the base lock: the row rejoins the
// line at its original effective key (enqueued_at, or a kept order_key). A
// row whose head moved, whose
// episode changed (another yielded_at) or that is no longer held is
// ErrStateConflict and keeps its marker. See README.md § "Yield".
func (s *PostgresStore) ClearYield(ctx context.Context, entryID uuid.UUID, headSHA string, yieldedAt time.Time) (Entry, error) {
	return s.yieldCASTx(ctx, "clear yield", entryID, headSHA, yieldedAt,
		`yield_reason = NULL, yield_detail = NULL, yielded_at = NULL, approval_wait_started_at = NULL`)
}

// UpdateYield rewrites a yielded entry's reason and detail only (a yield
// transition: yielded_at, the anchor and the wait anchor are kept), the same
// compare-and-swap as ClearYield. An empty reason is ErrInvalidRequest.
func (s *PostgresStore) UpdateYield(ctx context.Context, entryID uuid.UUID, headSHA string, yieldedAt time.Time, marker YieldMarker) (Entry, error) {
	if err := marker.validate(); err != nil {
		return Entry{}, err
	}
	return s.yieldCASTx(ctx, "update yield", entryID, headSHA, yieldedAt,
		`yield_reason = $4, yield_detail = NULLIF($5, '')`, marker.Reason, marker.Detail)
}

// ReanchorYielded moves a yielded entry's anchor to the PR's current head
// (newHeadSHA, newBaseSHA) KEEPING the marker, yielded_at and
// approval_wait_started_at (the same episode: a head move never clears a
// marker, so a push loop cannot buy a fresh wait window per push). It is the
// same compare-and-swap as ClearYield on the OLD head (headSHA): a stale
// probe is ErrStateConflict and is not touched. An unchanged anchor writes
// nothing beyond the CAS check.
func (s *PostgresStore) ReanchorYielded(ctx context.Context, entryID uuid.UUID, headSHA string, yieldedAt time.Time, newHeadSHA, newBaseSHA string) (Entry, error) {
	if newHeadSHA == "" {
		return Entry{}, fmt.Errorf("%w: reanchor yielded requires the new head sha", ErrInvalidRequest)
	}
	return s.yieldCASTx(ctx, "reanchor yielded", entryID, headSHA, yieldedAt,
		`anchored_head_sha = $4, anchored_base_sha = NULLIF($5, ''),
    anchored_at = CASE WHEN anchored_head_sha = $4 AND anchored_base_sha IS NOT DISTINCT FROM NULLIF($5, '') THEN anchored_at ELSE clock_timestamp() END`,
		newHeadSHA, newBaseSHA)
}

// intentFnTimeout bounds every fn run under the merge-intent lock: the
// merge_run intent decision's DB work, and (ADR-093 slice 2) merge-on-pass's,
// the inline merge_run tail's and the delegated arm's forge reads and merge
// call, each carrying its own per-call timeout well under this bound. It is
// applied even when the caller's context carries no deadline.
const intentFnTimeout = 90 * time.Second

// WithMergeIntentLock runs fn while holding the run's merge-intent lock: a
// SESSION-level advisory lock on intentLockKey, held on one dedicated pooled
// connection that runs no transaction while fn runs on others, through
// pglock.WithSessionLock on the pool-wide pglock budget. See README.md §
// "Merge-intent lock".
//
// The lock is TRIED, and a held lock re-tries every pglock.Poll holding
// neither a budget slot nor a connection between tries; the wait is bounded
// only by ctx, and a canceled ctx returns its error with fn never run. fn
// runs on a context bounded by intentFnTimeout and marked as inside a pglock
// lock (fn must not take another one: pglock.ErrNestedLock). A panic in fn
// still unlocks.
//
// fn's error is returned as is. When fn returned nil but the unlock failed or
// found no lock (the session died mid-fn, so the exclusion may have lapsed),
// the result wraps both ErrIntentLockLapsed and pglock.ErrLockLapsed.
func (s *PostgresStore) WithMergeIntentLock(ctx context.Context, runID uuid.UUID, fn func(context.Context) error) error {
	if runID == uuid.Nil {
		return fmt.Errorf("%w: run id is required", ErrInvalidRequest)
	}
	err := pglock.WithSessionLock(ctx, s.pool, s.locks, intentLockKey(runID),
		pglock.Options{Name: "mergequeue: intent lock", FnTimeout: intentFnTimeout}, fn)
	if errors.Is(err, pglock.ErrLockLapsed) {
		return fmt.Errorf("%w: %w", ErrIntentLockLapsed, err)
	}
	return err
}

// ClaimMergeDispatch claims the run's ONE merge dispatch (approval condition
// D1): a plain INSERT into merge_dispatch_claims, so the UNIQUE run_id
// constraint, not any lock, is what admits exactly one claim per run. A
// unique violation is ErrAlreadyDispatched, returned with the existing claim
// (read back on the pool; the zero claim when that read fails). An unknown
// run is ErrNotFound. See README.md § "Merge-dispatch ledger".
func (s *PostgresStore) ClaimMergeDispatch(ctx context.Context, runID uuid.UUID, path, headSHA string) (DispatchClaim, error) {
	if runID == uuid.Nil || headSHA == "" {
		return DispatchClaim{}, fmt.Errorf("%w: run id and head sha are required", ErrInvalidRequest)
	}
	switch path {
	case DispatchPathInline, DispatchPathDelegated, DispatchPathQueue:
	default:
		return DispatchClaim{}, fmt.Errorf("%w: dispatch path %q is not inline, delegated or queue", ErrInvalidRequest, path)
	}
	c, err := scanClaim(s.pool.QueryRow(ctx, `INSERT INTO merge_dispatch_claims (run_id, state, path, head_sha)
VALUES ($1, 'dispatched', $2, $3)
RETURNING `+claimCols, runID, path, headSHA))
	if err == nil {
		return c, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation: the D1 UNIQUE run_id
			existing, found, rerr := s.mergeDispatchFor(ctx, runID)
			if rerr != nil || !found {
				// The claim exists but could not be read back: still
				// ErrAlreadyDispatched (fail closed), with the run id and an
				// explicit unknown state so no caller renders empty fields.
				return DispatchClaim{RunID: runID, State: DispatchUnknown, Path: DispatchUnknownField, HeadSHA: DispatchUnknownField},
					fmt.Errorf("run %s: %w (the existing claim could not be read: %v)", runID, ErrAlreadyDispatched, rerr)
			}
			return existing, fmt.Errorf("run %s: %w", runID, ErrAlreadyDispatched)
		case "23503": // foreign_key_violation: no such run
			return DispatchClaim{}, fmt.Errorf("run %s: %w", runID, ErrNotFound)
		}
	}
	return DispatchClaim{}, fmt.Errorf("mergequeue: claim merge dispatch: %w", err)
}

// MarkMergeDispatchFailed moves the run's dispatch claim to dispatch_failed
// for an UNCERTAIN forge outcome, recording cause and the database-clock
// instant. The claim is UPDATED, never deleted, so a later dispatch is still
// refused until reconciliation or the operator re-arm (README.md §
// "Merge-dispatch ledger"). A repeat keeps the first failure. A
// run with no claim is ErrNotFound.
func (s *PostgresStore) MarkMergeDispatchFailed(ctx context.Context, runID uuid.UUID, cause string) error {
	if runID == uuid.Nil {
		return fmt.Errorf("%w: run id is required", ErrInvalidRequest)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE merge_dispatch_claims
SET state = 'dispatch_failed',
    failed_at = COALESCE(failed_at, clock_timestamp()),
    failure_cause = COALESCE(failure_cause, NULLIF($2, ''))
WHERE run_id = $1`, runID, cause)
	if err != nil {
		return fmt.Errorf("mergequeue: mark merge dispatch failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("run %s has no merge dispatch claim: %w", runID, ErrNotFound)
	}
	return nil
}

// ReleaseMergeDispatch DELETEs the run's dispatch claim, a compare-and-swap
// on the EXACT claim (run_id, state 'dispatched', path, head_sha and
// claimed_at), for a forge outcome that guarantees nothing was applied
// (README.md § "Merge-dispatch ledger": the release / uncertain split). A
// claim that no longer matches, a dispatch_failed claim, or another holder's
// claim is never deleted: ErrStateConflict. Call it under the run's
// merge-intent lock.
func (s *PostgresStore) ReleaseMergeDispatch(ctx context.Context, claim DispatchClaim) error {
	if claim.RunID == uuid.Nil || claim.HeadSHA == "" || claim.ClaimedAt.IsZero() {
		return fmt.Errorf("%w: run id, head sha and claimed_at are required", ErrInvalidRequest)
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM merge_dispatch_claims
WHERE run_id = $1 AND state = 'dispatched' AND path = $2 AND head_sha = $3 AND claimed_at = $4`,
		claim.RunID, claim.Path, claim.HeadSHA, claim.ClaimedAt)
	if err != nil {
		return fmt.Errorf("mergequeue: release merge dispatch: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("run %s: the dispatch claim no longer matches the one released: %w", claim.RunID, ErrStateConflict)
	}
	return nil
}

// MergeDispatch reads the run's dispatch claim; found=false when it has none.
func (s *PostgresStore) MergeDispatch(ctx context.Context, runID uuid.UUID) (DispatchClaim, bool, error) {
	return s.mergeDispatchFor(ctx, runID)
}

const claimCols = `run_id, state, path, head_sha, claimed_at, failed_at, COALESCE(failure_cause, '')`

func scanClaim(row pgx.Row) (DispatchClaim, error) {
	var c DispatchClaim
	var state string
	if err := row.Scan(&c.RunID, &state, &c.Path, &c.HeadSHA, &c.ClaimedAt, &c.FailedAt, &c.FailureCause); err != nil {
		return DispatchClaim{}, err
	}
	c.State = DispatchState(state)
	return c, nil
}

// mergeDispatchFor reads the run's dispatch claim.
func (s *PostgresStore) mergeDispatchFor(ctx context.Context, runID uuid.UUID) (DispatchClaim, bool, error) {
	c, err := scanClaim(s.pool.QueryRow(ctx, `SELECT `+claimCols+` FROM merge_dispatch_claims WHERE run_id = $1`, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return DispatchClaim{}, false, nil
	}
	if err != nil {
		return DispatchClaim{}, false, fmt.Errorf("mergequeue: read merge dispatch claim: %w", err)
	}
	return c, true, nil
}

// rowQuerier is the QueryRow half shared by the pool and a transaction.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// activeForRun is ActiveForRun on q: the pool for the read surface, the
// call's transaction for a RecordIntentFunc.
func activeForRun(ctx context.Context, q rowQuerier, runID uuid.UUID) (Status, bool, error) {
	var st Status
	var liveRun pgtype.UUID
	e, err := scanEntry(q.QueryRow(ctx, `SELECT `+entryCols+`,
  CASE WHEN e.state = 'held' THEN (
    SELECT count(*) FROM merge_candidate_queue_entries h JOIN runs hr ON hr.id = h.run_id
    WHERE h.repo = e.repo AND h.base_ref = e.base_ref AND h.state = 'held'
      AND hr.account_id IS NOT DISTINCT FROM r.account_id
      AND (h.yielded_at IS NOT NULL, COALESCE(h.order_key, h.enqueued_at), h.id) <= (e.yielded_at IS NOT NULL, `+effectiveKeySQL+`, e.id)
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

// ActiveForRun returns the run's active entry with its held position and the
// base's live run, counted within the run's own account. found is false when
// the run has no active entry.
func (s *PostgresStore) ActiveForRun(ctx context.Context, runID uuid.UUID) (Status, bool, error) {
	return activeForRun(ctx, s.pool, runID)
}

// ListForBase returns the base's active entries in queue order: the live
// entry first, then unmarked held entries by (effective key, id), then
// yielded held entries by (effective key, id); the effective key is
// effectiveKeySQL. Never nil.
func (s *PostgresStore) ListForBase(ctx context.Context, base Base) ([]Entry, error) {
	if err := base.validate(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+entryCols+entryFrom+`
WHERE `+basePredicate+` AND e.state IN ('held', 'live')
ORDER BY (e.state = 'live') DESC, (e.yielded_at IS NOT NULL), `+effectiveKeySQL+`, e.id`, base.Repo, base.BaseRef, acctArg(base.AccountID))
	if err != nil {
		return nil, fmt.Errorf("mergequeue: list base: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) { return scanEntry(row) })
	if err != nil {
		return nil, fmt.Errorf("mergequeue: scan base: %w", err)
	}
	return out, nil
}

// ActiveBases returns every base with a held or live entry, ordered by
// (account, repo, base ref). Never nil. It is the periodic pump's sweep list
// (backend/internal/server/README.md, "Periodic pump"): across every account,
// like the rest of the store.
func (s *PostgresStore) ActiveBases(ctx context.Context) ([]Base, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT COALESCE(r.account_id::text, ''), e.repo, e.base_ref`+entryFrom+`
WHERE e.state IN ('held', 'live')
ORDER BY 1, 2, 3`)
	if err != nil {
		return nil, fmt.Errorf("mergequeue: list active bases: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Base, error) {
		var b Base
		err := row.Scan(&b.AccountID, &b.Repo, &b.BaseRef)
		return b, err
	})
	if err != nil {
		return nil, fmt.Errorf("mergequeue: scan active bases: %w", err)
	}
	return out, nil
}

// RecentLineTurns returns the line-turn durations (settled_at − admitted_at,
// database clock) of the base's newest `limit` MERGED entries, newest settle
// first, scoped to the base's account like ListForBase. An entry merged
// without ever being admitted has no line turn and is skipped. Never nil;
// limit < 1 is ErrInvalidRequest. The ETA basis of ADR-093's queue read.
func (s *PostgresStore) RecentLineTurns(ctx context.Context, base Base, limit int) ([]time.Duration, error) {
	if err := base.validate(); err != nil {
		return nil, err
	}
	if limit < 1 {
		return nil, fmt.Errorf("%w: line-turn limit %d, want at least 1", ErrInvalidRequest, limit)
	}
	rows, err := s.pool.Query(ctx, `SELECT GREATEST(0, (EXTRACT(EPOCH FROM e.settled_at - e.admitted_at) * 1000000)::bigint)`+entryFrom+`
WHERE `+basePredicate+` AND e.state = 'merged' AND e.admitted_at IS NOT NULL
ORDER BY e.settled_at DESC, e.id DESC
LIMIT $4`, base.Repo, base.BaseRef, acctArg(base.AccountID), limit)
	if err != nil {
		return nil, fmt.Errorf("mergequeue: recent line turns: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (time.Duration, error) {
		var us int64
		err := row.Scan(&us)
		return time.Duration(us) * time.Microsecond, err
	})
	if err != nil {
		return nil, fmt.Errorf("mergequeue: scan line turns: %w", err)
	}
	return out, nil
}

// promoteKeySQL writes the promoted entry's order key: 1 ms ahead of the
// minimum effective key over every OTHER unmarked held entry on the same base
// and account ($1 repo, $2 base ref, $3 account), so the newest promote wins.
// enqueued_at and every other row are untouched. The write is GUARDED, in
// this same statement, on an entry still being strictly ahead of the
// caller's own (effective key $4, id $5): with none ahead, min() is not a
// key to write and bool_or over zero rows is NULL, so the guard matches no
// row (the concurrency store's #4312 shape).
const promoteKeySQL = `WITH others AS (
  SELECT min(` + effectiveKeySQL + `) AS k,
    bool_or((` + effectiveKeySQL + `, e.id) < ($4::timestamptz, $5::uuid)) AS ahead` + entryFrom + `
  WHERE ` + basePredicate + ` AND e.state = 'held' AND e.yielded_at IS NULL AND e.id <> $5::uuid
)
UPDATE merge_candidate_queue_entries SET order_key = others.k - interval '1 millisecond'
FROM others
WHERE id = $5::uuid AND state = 'held' AND yielded_at IS NULL AND others.ahead
RETURNING id`

// Promote moves a run's held entry to the head of its base's line among the
// unmarked held entries (ADR-093 D2). See README.md § "Promote".
//
// One transaction on one pooled connection: the run's account (an unknown
// run is ErrNotFound), the per-run lock (Enqueue holds it across a base
// change, so the peeked entry cannot be swapped for another base's row), an
// unlocked peek of the run's active entry, the base lock taken BLOCKING, the
// entry re-read FOR UPDATE, then classify: none → ErrNotQueued, live →
// ErrEntryLive, yielded → ErrEntryYielded. An entry already first answers
// Changed=false with nothing written and Record not called. Any error,
// including one returned by req.Record, rolls the whole promotion back.
func (s *PostgresStore) Promote(ctx context.Context, req PromoteRequest) (Promotion, error) {
	switch {
	case req.RunID == uuid.Nil:
		return Promotion{}, fmt.Errorf("%w: run id is required", ErrInvalidRequest)
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
	var acct string
	err := tx.QueryRow(ctx, `SELECT COALESCE(account_id::text, '') FROM runs WHERE id = $1`, req.RunID).Scan(&acct)
	if errors.Is(err, pgx.ErrNoRows) {
		return Promotion{}, fmt.Errorf("run %s: %w", req.RunID, ErrNotFound)
	}
	if err != nil {
		return Promotion{}, fmt.Errorf("mergequeue: promote: read run: %w", err)
	}
	if err := lockTx(ctx, tx, runLockKey(req.RunID)); err != nil {
		return Promotion{}, err
	}
	var peekID uuid.UUID
	var repo, baseRef string
	err = tx.QueryRow(ctx, `SELECT id, repo, base_ref FROM merge_candidate_queue_entries
WHERE run_id = $1 AND state IN ('held', 'live')`, req.RunID).Scan(&peekID, &repo, &baseRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return Promotion{}, fmt.Errorf("run %s: %w", req.RunID, ErrNotQueued)
	}
	if err != nil {
		return Promotion{}, fmt.Errorf("mergequeue: promote: read active entry: %w", err)
	}
	if s.afterPromotePeek != nil {
		s.afterPromotePeek(ctx)
	}
	if err := lockTx(ctx, tx, lockKey(acct, repo, baseRef)); err != nil {
		return Promotion{}, err
	}
	cur, err := getEntryTx(ctx, tx, peekID)
	if err != nil {
		return Promotion{}, err
	}
	switch {
	case cur.State == StateLive: // Promote live-entry refusal
		return Promotion{}, fmt.Errorf("run %s entry %s: %w", req.RunID, cur.ID, ErrEntryLive)
	case cur.State != StateHeld:
		// Settled by a by-entry mutation between the peek and the base lock.
		return Promotion{}, fmt.Errorf("run %s entry %s is %s: %w", req.RunID, cur.ID, cur.State, ErrNotQueued)
	case cur.Yielded(): // Promote yielded-entry refusal
		return Promotion{}, fmt.Errorf("run %s entry %s (%s): %w", req.RunID, cur.ID, cur.YieldReason, ErrEntryYielded)
	}

	// The held position of an unmarked entry counts only unmarked entries
	// ahead (yielded ones sort after it), so activeForRun's Position is the
	// place among the unmarked held entries.
	before, err := promoteStatus(ctx, tx, req.RunID)
	if err != nil {
		return Promotion{}, err
	}
	out := Promotion{Entry: before.Entry, OldPosition: before.Position, NewPosition: before.Position, LiveRunID: before.LiveRunID}
	if before.Position == 1 {
		return out, nil // already first: write nothing, record nothing
	}
	if s.afterPromotePosition != nil {
		s.afterPromotePosition(ctx)
	}
	key := cur.EnqueuedAt
	if cur.OrderKey != nil {
		key = *cur.OrderKey
	}
	var written uuid.UUID
	err = tx.QueryRow(ctx, promoteKeySQL, repo, baseRef, acctArg(acct), key, cur.ID).Scan(&written)
	if errors.Is(err, pgx.ErrNoRows) {
		// The guard failed: no entry is ahead at the write instant (the last
		// one left since the position read). The row is held, unmarked and
		// locked FOR UPDATE, so the UPDATE cannot miss for any other reason;
		// this is the already-first answer.
		out.OldPosition, out.NewPosition = 1, 1
		return out, nil
	}
	if err != nil {
		return Promotion{}, fmt.Errorf("mergequeue: promote: write order key: %w", err)
	}
	after, err := promoteStatus(ctx, tx, req.RunID)
	if err != nil {
		return Promotion{}, err
	}
	out.Entry, out.NewPosition, out.LiveRunID, out.Changed = after.Entry, after.Position, after.LiveRunID, true
	if err := req.Record(ctx, tx, out); err != nil { // Promote record-before-commit
		return Promotion{}, fmt.Errorf("mergequeue: promote: record: %w", err)
	}
	return out, nil
}

// promoteStatus is activeForRun inside Promote's transaction, under its
// locks.
func promoteStatus(ctx context.Context, tx pgx.Tx, runID uuid.UUID) (Status, error) {
	st, found, err := activeForRun(ctx, tx, runID)
	if err != nil {
		return Status{}, err
	}
	if !found {
		return Status{}, fmt.Errorf("mergequeue: promote: run %s lost its active entry under the lock", runID)
	}
	return st, nil
}

// newestRowSQL keeps only the run's NEWEST row (alias e): no row of the same
// run with a later (enqueued_at, id). A run's rows are inserted one episode
// at a time, each on the database clock after the previous one settled, so
// enqueued_at orders them; a promotion never rewrites it.
const newestRowSQL = `NOT EXISTS (SELECT 1 FROM merge_candidate_queue_entries n
    WHERE n.run_id = e.run_id AND (n.enqueued_at, n.id) > (e.enqueued_at, e.id))`

// LatestForRun returns the run's NEWEST row in any state (newestRowSQL's
// order); found=false when the run never had one.
func (s *PostgresStore) LatestForRun(ctx context.Context, runID uuid.UUID) (Entry, bool, error) {
	if runID == uuid.Nil {
		return Entry{}, false, fmt.Errorf("%w: run id is required", ErrInvalidRequest)
	}
	e, err := scanEntry(s.pool.QueryRow(ctx, `SELECT `+entryCols+entryFrom+`
WHERE e.run_id = $1
ORDER BY e.enqueued_at DESC, e.id DESC
LIMIT 1`, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("mergequeue: latest entry for run: %w", err)
	}
	return e, true, nil
}

// UnresolvedEjections returns the base's EJECTED rows that are still their
// run's newest row (no re-entry, no later episode), newest settle first, at
// most limit, scoped to the base's account like ListForBase. Never nil;
// limit < 1 is ErrInvalidRequest.
func (s *PostgresStore) UnresolvedEjections(ctx context.Context, base Base, limit int) ([]Entry, error) {
	if err := base.validate(); err != nil {
		return nil, err
	}
	if limit < 1 {
		return nil, fmt.Errorf("%w: ejection limit %d, want at least 1", ErrInvalidRequest, limit)
	}
	return s.collectEntries(ctx, "unresolved ejections", `SELECT `+entryCols+entryFrom+`
WHERE `+basePredicate+` AND e.state = 'ejected' AND `+newestRowSQL+`
ORDER BY e.settled_at DESC, e.id DESC
LIMIT $4`, base.Repo, base.BaseRef, acctArg(base.AccountID), limit)
}

// ReentryCandidates returns the rows ADR-093 D4's automatic re-entry may
// consider: EJECTED with ReentryReason, carrying merge intent, still their
// run's newest row, and belonging to a run that is not terminal (succeeded,
// failed, cancelled). Oldest settle first, at most limit, across every
// account (the sweep, like ActiveBases), strictly after the exclusive keyset
// cursor after when it is non-nil. Never nil; limit < 1 is ErrInvalidRequest.
// Eligibility, the fix-up and the forge are the caller's.
func (s *PostgresStore) ReentryCandidates(ctx context.Context, after *ReentryCursor, limit int) ([]Entry, error) {
	if limit < 1 {
		return nil, fmt.Errorf("%w: re-entry limit %d, want at least 1", ErrInvalidRequest, limit)
	}
	args := []any{ReentryReason, string(run.StateSucceeded), string(run.StateFailed), string(run.StateCancelled), limit}
	cursorSQL := ""
	if after != nil {
		cursorSQL = `
  AND (e.settled_at, e.id) > ($6, $7)`
		args = append(args, after.SettledAt, after.ID)
	}
	return s.collectEntries(ctx, "re-entry candidates", `SELECT `+entryCols+entryFrom+`
WHERE e.state = 'ejected' AND e.eject_reason = $1 AND e.merge_intent_at IS NOT NULL
  AND r.state NOT IN ($2, $3, $4) AND `+newestRowSQL+cursorSQL+`
ORDER BY e.settled_at, e.id
LIMIT $5`, args...)
}

// collectEntries runs a multi-row entry read on the pool. Never nil.
func (s *PostgresStore) collectEntries(ctx context.Context, what, sql string, args ...any) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("mergequeue: %s: %w", what, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) { return scanEntry(row) })
	if err != nil {
		return nil, fmt.Errorf("mergequeue: scan %s: %w", what, err)
	}
	return out, nil
}
