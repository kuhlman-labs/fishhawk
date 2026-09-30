package delegationconfirm

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

// Store is the delegation-confirmation write path. Append runs
// lock -> read -> derive -> decide -> append inside ONE postgres.WithTenant
// transaction holding the CAPTAIN RECORD's advisory lock for the
// (account, repo) — the same key captain.Store.Apply takes — so a captain
// check and its append cannot be split by a concurrent handover: an outgoing
// captain's confirm either commits BEFORE the captain_assigned (and is then
// voided by the fold's reset) or is refused after it. Read is the lock-free
// GET path.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store over pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

var (
	// ErrRepoRequired — Append or Read with no repository.
	ErrRepoRequired = errors.New("delegationconfirm: repo is required")
	// ErrDecideRequired — Append with no transition.
	ErrDecideRequired = errors.New("delegationconfirm: a transition (decide) is required")
	// ErrEventRepoMismatch — the transition produced an event for a
	// different repository than the one locked and read.
	ErrEventRepoMismatch = errors.New("delegationconfirm: transition event names a different repo than the one applied")
)

// AppendParams scopes one Append.
type AppendParams struct {
	AccountID *uuid.UUID
	Repo      string
	Actor     string
	ActorKind audit.ActorKind
	Timestamp time.Time
}

// Applied is a successful Append's outcome.
type Applied struct {
	Event Event
	Entry *audit.Entry
	State State
}

// Snapshot is the lock-free read: both entry sets in ascending order and
// their fold.
type Snapshot struct {
	State          State
	CaptainEntries []ChainEntry
	Entries        []ChainEntry
}

// lockKey is the CAPTAIN RECORD's pg_advisory_xact_lock key — byte-for-byte
// the derivation in backend/internal/captain/store.go (captainLockKey):
// sha256("fishhawk:captain:" || account-or-"untenanted" || 0x00 || repo),
// first 8 bytes. Sharing the key (rather than a disjoint one) is the point:
// a confirm serializes against every captain verb for the same repo.
// TestAppend_SharesCaptainLock proves the keys agree by blocking a real
// captain.Store.Apply on this lock.
func lockKey(accountID *uuid.UUID, repo string) int64 {
	h := sha256.New()
	h.Write([]byte("fishhawk:captain:"))
	if accountID != nil {
		h.Write(accountID[:])
	} else {
		h.Write([]byte("untenanted"))
	}
	h.Write([]byte{0})
	h.Write([]byte(repo))
	sum := h.Sum(nil)
	return int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // deliberate wrap: advisory-lock keys are opaque int64s
}

type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// entriesSQL reads one repository's captain AND delegation-confirmation
// entries within one account partition, ascending.
const entriesSQL = `SELECT sequence, entry_hash, category, ts, payload
FROM audit_entries
WHERE run_id IS NULL
  AND category = ANY($1)
  AND payload->>'repo' = $2
  AND account_id IS NOT DISTINCT FROM $3
ORDER BY sequence ASC`

func readEntries(ctx context.Context, q queryer, accountID *uuid.UUID, repo string) (captainEntries, confirmEntries []ChainEntry, err error) {
	cats := append(captain.Categories(), Categories()...)
	rows, err := q.Query(ctx, entriesSQL, cats, repo, accountID)
	if err != nil {
		return nil, nil, fmt.Errorf("delegationconfirm: read entries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e ChainEntry
		var raw []byte
		if err := rows.Scan(&e.Sequence, &e.EntryHash, &e.Category, &e.Timestamp, &raw); err != nil {
			return nil, nil, fmt.Errorf("delegationconfirm: read entries: scan: %w", err)
		}
		e.Payload = raw
		if isCategory(e.Category) {
			confirmEntries = append(confirmEntries, e)
		} else {
			captainEntries = append(captainEntries, e)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("delegationconfirm: read entries: %w", err)
	}
	return captainEntries, confirmEntries, nil
}

// Read is the lock-free GET path.
func (s *Store) Read(ctx context.Context, accountID *uuid.UUID, repo string) (*Snapshot, error) {
	if repo == "" {
		return nil, ErrRepoRequired
	}
	ce, de, err := readEntries(ctx, s.pool, accountID, repo)
	if err != nil {
		return nil, err
	}
	return &Snapshot{State: Derive(repo, ce, de), CaptainEntries: ce, Entries: de}, nil
}

// Append runs one write verb atomically: inside ONE WithTenant transaction it
// takes the captain record's advisory lock, reads both entry sets, derives
// State, calls decide on state that cannot change before the append, rolls
// back with nothing appended on a decide error, and appends via
// audit.AppendGlobalChainedTx in the SAME transaction. Lock order matches
// captain.Store.Apply (captain key, then the global-chain partition key), so
// no deadlock cycle exists.
func (s *Store) Append(ctx context.Context, p AppendParams, decide func(State) (Event, error)) (*Applied, error) {
	if p.Repo == "" {
		return nil, ErrRepoRequired
	}
	if decide == nil {
		return nil, ErrDecideRequired
	}
	acct := ""
	if p.AccountID != nil {
		acct = p.AccountID.String()
	}
	var applied *Applied
	err := postgres.WithTenant(ctx, s.pool, acct, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey(p.AccountID, p.Repo)); err != nil {
			return fmt.Errorf("delegationconfirm: acquire captain record lock: %w", err)
		}
		ce, de, err := readEntries(ctx, tx, p.AccountID, p.Repo)
		if err != nil {
			return err
		}
		ev, err := decide(Derive(p.Repo, ce, de))
		if err != nil {
			return err
		}
		if ev.Repo != p.Repo {
			return fmt.Errorf("%w: event %q, applied %q", ErrEventRepoMismatch, ev.Repo, p.Repo)
		}
		body, err := ev.Payload()
		if err != nil {
			return err
		}
		params := audit.GlobalChainAppendParams{
			Timestamp: p.Timestamp, Category: ev.Kind, Payload: body, AccountID: p.AccountID,
		}
		if p.ActorKind != "" {
			kind := p.ActorKind
			params.ActorKind = &kind
		}
		if p.Actor != "" {
			subj := p.Actor
			params.ActorSubject = &subj
		}
		entry, err := audit.AppendGlobalChainedTx(ctx, tx, params)
		if err != nil {
			return err
		}
		de = append(de, ChainEntry{
			Sequence: entry.Sequence, EntryHash: entry.EntryHash, Category: entry.Category,
			Timestamp: entry.Timestamp, Payload: entry.Payload,
		})
		applied = &Applied{Event: ev, Entry: entry, State: Derive(p.Repo, ce, de)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return applied, nil
}
