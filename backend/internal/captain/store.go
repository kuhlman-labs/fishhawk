package captain

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

// Store is the captain record's serialization domain. Apply runs
// read -> derive -> validate -> append inside ONE postgres.WithTenant
// transaction holding pg_advisory_xact_lock on a domain-separated
// (account, repo) key, so two vacant-seat claims cannot both succeed and an
// accept cannot commit after its offer was withdrawn. Read is the lock-free
// GET path.
type Store struct {
	pool *pgxpool.Pool
	// afterRead is a TEST-ONLY seam, called immediately after the state is
	// read and derived and before decide/append. The concurrency tests park
	// every caller on a barrier here to force overlapping reads when the read
	// is (wrongly) outside the lock. nil in production.
	afterRead func()
}

// NewStore returns a Store over pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

var (
	// ErrRepoRequired — Apply or Read with no repository.
	ErrRepoRequired = errors.New("captain: repo is required")
	// ErrDecideRequired — Apply with no transition.
	ErrDecideRequired = errors.New("captain: a transition (decide) is required")
	// ErrEventRepoMismatch — the transition produced an event for a different
	// repository than the one Apply locked and read.
	ErrEventRepoMismatch = errors.New("captain: transition event names a different repo than the one applied")
)

// ApplyParams scopes one Apply: the account partition and repository whose
// record is read and appended to, and the acting identity recorded as the
// entry's actor.
type ApplyParams struct {
	AccountID *uuid.UUID
	Repo      string
	Actor     string
	ActorKind audit.ActorKind
	Timestamp time.Time
}

// Applied is the outcome of a successful Apply: the event recorded, the
// persisted chain entry, and the State re-derived with that entry folded in.
type Applied struct {
	Event Event
	Entry *audit.Entry
	State State
}

// Snapshot is the lock-free read: the repository's captain entries in
// ascending sequence order and their fold.
type Snapshot struct {
	State   State
	Entries []ChainEntry
}

// captainLockKey is the pg_advisory_xact_lock key serializing one
// repository's captain record within one account partition: the first 8
// bytes of a sha256 over the domain prefix "fishhawk:captain:", the account
// UUID (or an untenanted marker), a separator, and the repo. The prefix keeps
// the key space disjoint from audit's globalChainLockKey and refinement's
// filing locks; a residual 1-in-2^64 collision only serializes two unrelated
// writers.
func captainLockKey(accountID *uuid.UUID, repo string) int64 {
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

// queryer is the read surface shared by the pool and a pgx.Tx.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// entriesSQL reads one repository's captain entries within one account
// partition. category = ANY is served by audit_entries_category_idx
// (migration 0002); payload->>'repo' is a JSON FILTER over those rows, not
// index-served (see README).
const entriesSQL = `SELECT sequence, entry_hash, category, ts, payload
FROM audit_entries
WHERE run_id IS NULL
  AND category = ANY($1)
  AND payload->>'repo' = $2
  AND account_id IS NOT DISTINCT FROM $3
ORDER BY sequence ASC`

func readEntries(ctx context.Context, q queryer, accountID *uuid.UUID, repo string) ([]ChainEntry, error) {
	rows, err := q.Query(ctx, entriesSQL, Categories(), repo, accountID)
	if err != nil {
		return nil, fmt.Errorf("captain: read entries: %w", err)
	}
	defer rows.Close()
	var out []ChainEntry
	for rows.Next() {
		var e ChainEntry
		var payload []byte
		if err := rows.Scan(&e.Sequence, &e.EntryHash, &e.Category, &e.Timestamp, &payload); err != nil {
			return nil, fmt.Errorf("captain: read entries: scan: %w", err)
		}
		e.Payload = payload
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("captain: read entries: %w", err)
	}
	return out, nil
}

// Entries returns repo's captain chain entries in ascending sequence order,
// narrowed to accountID's partition (nil = the untenanted partition).
func (s *Store) Entries(ctx context.Context, accountID *uuid.UUID, repo string) ([]ChainEntry, error) {
	if repo == "" {
		return nil, ErrRepoRequired
	}
	return readEntries(ctx, s.pool, accountID, repo)
}

// Read is the lock-free GET path: the entries and their fold. A read racing
// an Apply sees the record either before or after that Apply commits, never
// a partial state, because each Apply is one transaction.
func (s *Store) Read(ctx context.Context, accountID *uuid.UUID, repo string) (*Snapshot, error) {
	entries, err := s.Entries(ctx, accountID, repo)
	if err != nil {
		return nil, err
	}
	return &Snapshot{State: Derive(repo, entries), Entries: entries}, nil
}

// Apply runs one verb atomically: inside ONE WithTenant transaction it
// (a) takes the repository's captain advisory lock, (b) reads the
// repository's captain entries, (c) derives State, (d) calls decide — the
// transition — on state that cannot change before the append, (e) returns a
// decide error so the transaction ROLLS BACK with nothing appended, and
// (f) appends the event via audit.AppendGlobalChainedTx in the SAME
// transaction. Lock order is fixed — the captain key, then the global-chain
// partition key inside AppendGlobalChainedTx — and no other path takes the
// captain key, so no deadlock cycle exists.
func (s *Store) Apply(ctx context.Context, p ApplyParams, decide func(State) (Event, error)) (*Applied, error) {
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
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", captainLockKey(p.AccountID, p.Repo)); err != nil {
			return fmt.Errorf("captain: acquire record lock: %w", err)
		}
		entries, err := readEntries(ctx, tx, p.AccountID, p.Repo)
		if err != nil {
			return err
		}
		state := Derive(p.Repo, entries)
		if s.afterRead != nil {
			s.afterRead()
		}
		ev, err := decide(state)
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
			Timestamp: p.Timestamp,
			Category:  ev.Kind,
			Payload:   body,
			AccountID: p.AccountID,
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
		entries = append(entries, ChainEntry{
			Sequence:  entry.Sequence,
			EntryHash: entry.EntryHash,
			Category:  entry.Category,
			Timestamp: entry.Timestamp,
			Payload:   entry.Payload,
		})
		applied = &Applied{Event: ev, Entry: entry, State: Derive(p.Repo, entries)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return applied, nil
}
