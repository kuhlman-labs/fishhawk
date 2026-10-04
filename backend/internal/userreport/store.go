package userreport

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

// Source is the closed set of activity sources a cursor row is keyed by. Each
// source gets its OWN row so a source that is unreachable (E81.2's
// discussions, say) never drags another source's cursor past unread items.
type Source string

// SourceIssues is the issues-and-issue-comments source (#3771).
const SourceIssues Source = "issues"

// SourceIssueNotes is NOT a scannable source: it is SourceIssues' NOTE FLOOR
// row, the workmgmt NoteSince a scan of SourceIssues passes and advances
// through the same monotonic Advance. It exists because an issue-listing
// truncation advances the issues cursor past issues the scan never read
// (GitLab finds notes only through their issue), so their earlier notes need
// a bound that truncation does not move. An absent row means "the issues
// cursor".
const SourceIssueNotes Source = "issue_notes"

func (s Source) valid() bool { return s == SourceIssues || s == SourceIssueNotes }

// ErrInvalidKey reports a cursor key or value the store refuses before any
// database call: an empty repo, a source outside the closed set, or a zero
// cursor value.
var ErrInvalidKey = errors.New("userreport: invalid cursor key")

// Key selects one cursor row: the account partition (nil = the untenanted
// partition), the repository string the scan is keyed by, and the source.
type Key struct {
	AccountID *uuid.UUID
	Repo      string
	Source    Source
}

func (k Key) validate() error {
	if k.Repo == "" {
		return fmt.Errorf("%w: repo is required", ErrInvalidKey)
	}
	if !k.Source.valid() {
		return fmt.Errorf("%w: source %q is not one of the closed set (%q, %q)", ErrInvalidKey, k.Source, SourceIssues, SourceIssueNotes)
	}
	return nil
}

// tenant is the WithTenant account string: empty for the untenanted partition,
// which leaves app.account_id unset so the 0096 policy fails closed to
// NULL-account rows only.
func (k Key) tenant() string {
	if k.AccountID == nil {
		return ""
	}
	return k.AccountID.String()
}

// Store persists user-report cursors in user_report_cursors (0096).
//
// TRANSACTION SCOPING (#3771 approval condition 6): every method runs in its
// OWN short postgres.WithTenant transaction and returns before the caller does
// any forge I/O. No transaction is held across a listing; Scan's
// Record-then-Advance ordering is the only atomicity guarantee between a
// recorded report and the cursor.
//
// Hand-written pgx, not sqlc: a local `sqlc generate` regenerates every
// package in backend/sqlc.yaml (the digest / decisionindex precedent).
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store over pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const getCursorSQL = `SELECT cursor_at FROM user_report_cursors
	WHERE repo = $1 AND source = $2 AND account_id IS NOT DISTINCT FROM $3`

// Get returns the stored cursor for key and whether one is recorded.
func (s *Store) Get(ctx context.Context, key Key) (time.Time, bool, error) {
	if err := key.validate(); err != nil {
		return time.Time{}, false, err
	}
	var (
		at    time.Time
		found bool
	)
	err := postgres.WithTenant(ctx, s.pool, key.tenant(), func(tx pgx.Tx) error {
		var err error
		at, found, err = getCursor(ctx, tx, key)
		return err
	})
	if err != nil {
		return time.Time{}, false, fmt.Errorf("userreport: get cursor: %w", err)
	}
	return at, found, nil
}

func getCursor(ctx context.Context, tx pgx.Tx, key Key) (time.Time, bool, error) {
	var at time.Time
	err := tx.QueryRow(ctx, getCursorSQL, key.Repo, string(key.Source), key.AccountID).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return at, true, nil
}

// initCursorSQL is INSERT-IF-ABSENT returning the STORED value: the `ins` CTE
// returns a row only when this statement created the row; otherwise the second
// arm reads the row that already existed. (A row committed by a concurrent
// Init after this statement's snapshot is caught by Init's re-read.)
const initCursorSQL = `WITH ins AS (
	INSERT INTO user_report_cursors (account_id, repo, source, cursor_at)
	VALUES ($1, $2, $3, $4)
	ON CONFLICT (repo, source, COALESCE(account_id, '00000000-0000-0000-0000-000000000000'::uuid))
	DO NOTHING
	RETURNING cursor_at
)
SELECT cursor_at FROM ins
UNION ALL
SELECT cursor_at FROM user_report_cursors
	WHERE repo = $2 AND source = $3 AND account_id IS NOT DISTINCT FROM $1
	  AND NOT EXISTS (SELECT 1 FROM ins)
LIMIT 1`

// Init stores initial as key's cursor ONLY when no row exists, and returns
// the STORED value either way (#3771 approval condition 2). A caller reads
// with the returned value, never with initial, so a first scan that fails and
// is retried with an advanced clock passes the identical since.
func (s *Store) Init(ctx context.Context, key Key, initial time.Time) (time.Time, error) {
	if err := key.validate(); err != nil {
		return time.Time{}, err
	}
	if initial.IsZero() {
		return time.Time{}, fmt.Errorf("%w: initial cursor must be non-zero", ErrInvalidKey)
	}
	var stored time.Time
	err := postgres.WithTenant(ctx, s.pool, key.tenant(), func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, initCursorSQL, key.AccountID, key.Repo, string(key.Source), initial).Scan(&stored)
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// A concurrent Init committed the row after this statement's
		// snapshot: the conflict arm saw it, the snapshot read did not. A
		// fresh statement sees it.
		at, found, err := getCursor(ctx, tx, key)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("insert-if-absent returned no row and none is visible")
		}
		stored = at
		return nil
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("userreport: init cursor: %w", err)
	}
	return stored, nil
}

// advanceCursorSQL upserts MONOTONICALLY and reports the COMMITTED cursor plus
// whether THIS statement moved it: the `up` CTE returns a row only when the
// INSERT or a strictly-later UPDATE fired; when the WHERE refuses a backwards
// (or equal) move, the second arm re-reads the row as it stands (the digest
// advanceWatermarkSQL shape).
const advanceCursorSQL = `WITH up AS (
	INSERT INTO user_report_cursors (account_id, repo, source, cursor_at)
	VALUES ($1, $2, $3, $4)
	ON CONFLICT (repo, source, COALESCE(account_id, '00000000-0000-0000-0000-000000000000'::uuid))
	DO UPDATE SET cursor_at = EXCLUDED.cursor_at, updated_at = now()
	WHERE user_report_cursors.cursor_at < EXCLUDED.cursor_at
	RETURNING cursor_at
)
SELECT cursor_at, true FROM up
UNION ALL
SELECT cursor_at, false FROM user_report_cursors
	WHERE repo = $2 AND source = $3 AND account_id IS NOT DISTINCT FROM $1
	  AND NOT EXISTS (SELECT 1 FROM up)
LIMIT 1`

// Advance moves key's cursor to `to` and NEVER backwards. It returns the
// cursor COMMITTED after the call (not necessarily `to`: a later value
// already stored wins) and whether this call moved it.
func (s *Store) Advance(ctx context.Context, key Key, to time.Time) (time.Time, bool, error) {
	if err := key.validate(); err != nil {
		return time.Time{}, false, err
	}
	if to.IsZero() {
		return time.Time{}, false, fmt.Errorf("%w: cursor must be non-zero", ErrInvalidKey)
	}
	var (
		committed time.Time
		advanced  bool
	)
	err := postgres.WithTenant(ctx, s.pool, key.tenant(), func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, advanceCursorSQL, key.AccountID, key.Repo, string(key.Source), to).Scan(&committed, &advanced)
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// The row was committed concurrently after this statement's
		// snapshot and the monotonic guard refused the move: re-read it.
		at, found, err := getCursor(ctx, tx, key)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("monotonic upsert returned no row and none is visible")
		}
		committed, advanced = at, false
		return nil
	})
	if err != nil {
		return time.Time{}, false, fmt.Errorf("userreport: advance cursor: %w", err)
	}
	return committed, advanced, nil
}
