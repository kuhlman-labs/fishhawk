// Package dispatcherheartbeat persists each host dispatcher's latest
// heartbeat (#5489, ADR-104 Amendment A2) in host_dispatcher_heartbeats
// (migration 0121): one row per (account, host label), written by
// POST /v0/host-dispatchers/heartbeat and read by the campaign status awaiting
// block and, through Get, by the marker's host-capacity resolution (ADR-094
// Amendment A5). The wire type is hostdispatcher.Heartbeat.
//
// ACCOUNT PARTITION: a write lands in the token's account partition (empty =
// the untenanted partition). A List read for account X sees the untenanted
// rows and X's rows, never another account's; a List read for the untenanted
// caller sees the untenanted rows only. Get is EXACT instead: it returns only
// the row whose account equals the caller's (two empties match), so an
// untenanted declaration never governs a tenanted run (A5 item 7). Every
// method runs inside postgres.WithTenant AND carries an explicit account
// WHERE: the runtime superuser bypasses RLS
// (backend/internal/postgres/tenant.go), so the WHERE is the operative
// control and the 0121 policy is defense in depth.
//
// The payload column holds the heartbeat plus the server-only
// dropped_declaration (storedPayload), so the strict wire type gains no key a
// client could send.
//
// Hand-written pgx, not sqlc: a local `sqlc generate` regenerates every
// package in backend/sqlc.yaml (the userreport / digest precedent).
package dispatcherheartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/hostdispatcher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

// MaxListLimit bounds one List read; a limit outside [1, MaxListLimit] is
// clamped to it.
const MaxListLimit = 256

// Record is one dispatcher's latest heartbeat.
type Record struct {
	// AccountID is the token's tenant account; empty means untenanted.
	AccountID string
	// HostLabel is the <host> of the token's host-dispatcher/<host> subject.
	HostLabel string
	// TokenID is the authenticating token's id (empty when unknown).
	TokenID   string
	Heartbeat hostdispatcher.Heartbeat
	// DroppedDeclaration names the declaration field whose out-of-range value
	// dropped the whole declaration at ingest (hostdispatcher
	// DeclarationField*); empty when none dropped. Heartbeat's declaration
	// fields are then both nil, so no earlier declaration carries forward.
	DroppedDeclaration string
	// ReceivedAt is fishhawkd's clock at ingest, never the dispatcher's.
	ReceivedAt time.Time
}

// storedPayload is the payload column's shape: the wire heartbeat's fields
// promoted to the top level (an embedded struct), plus the server-only
// dropped_declaration. Keeping that key off hostdispatcher.Heartbeat keeps it
// an unknown-key 400 when a client sends it.
type storedPayload struct {
	hostdispatcher.Heartbeat
	DroppedDeclaration string `json:"dropped_declaration,omitempty"`
}

// Repository is the heartbeat store seam.
type Repository interface {
	// Upsert writes rec, replacing the row for the same (account, host label).
	Upsert(ctx context.Context, rec Record) error
	// List returns the rows visible to accountID (see the package doc), newest
	// received_at first, ties broken by the lower host label.
	List(ctx context.Context, accountID string, limit int) ([]Record, error)
	// Get returns the row whose account EXACTLY equals accountID (empty
	// matches only the untenanted row) and whose label equals hostLabel, and
	// whether one exists. Unlike List it never falls back to an untenanted
	// row for a tenanted caller.
	Get(ctx context.Context, accountID, hostLabel string) (Record, bool, error)
}

// PostgresRepository is the pgx-backed Repository.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository returns a Repository over pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

var _ Repository = (*PostgresRepository)(nil)

func accountParam(accountID string) (*uuid.UUID, error) {
	if accountID == "" {
		return nil, nil
	}
	id, err := uuid.Parse(accountID)
	if err != nil {
		return nil, fmt.Errorf("dispatcherheartbeat: account id %q is not a UUID: %w", accountID, err)
	}
	return &id, nil
}

func clampLimit(limit int) int {
	if limit < 1 || limit > MaxListLimit {
		return MaxListLimit
	}
	return limit
}

const upsertSQL = `INSERT INTO host_dispatcher_heartbeats (account_id, host_label, token_id, payload, received_at)
	VALUES ($1, $2, NULLIF($3, ''), $4, $5)
	ON CONFLICT (host_label, COALESCE(account_id, '00000000-0000-0000-0000-000000000000'::uuid))
	DO UPDATE SET token_id = EXCLUDED.token_id, payload = EXCLUDED.payload, received_at = EXCLUDED.received_at`

// Upsert implements Repository.
func (p *PostgresRepository) Upsert(ctx context.Context, rec Record) error {
	acct, err := accountParam(rec.AccountID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(storedPayload{Heartbeat: rec.Heartbeat, DroppedDeclaration: rec.DroppedDeclaration})
	if err != nil {
		return fmt.Errorf("dispatcherheartbeat: encode payload: %w", err)
	}
	err = postgres.WithTenant(ctx, p.pool, rec.AccountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, upsertSQL, acct, rec.HostLabel, rec.TokenID, payload, rec.ReceivedAt.UTC())
		return err
	})
	if err != nil {
		return fmt.Errorf("dispatcherheartbeat: upsert: %w", err)
	}
	return nil
}

// listSQL's account predicate is the operative partition control: with $1
// NULL (an untenanted caller) `account_id = $1` is never true, so only the
// untenanted rows match.
const listSQL = `SELECT COALESCE(account_id::text, ''), host_label, COALESCE(token_id, ''), payload, received_at
	FROM host_dispatcher_heartbeats
	WHERE (account_id IS NULL OR account_id = $1)
	ORDER BY received_at DESC, host_label ASC
	LIMIT $2`

// List implements Repository.
func (p *PostgresRepository) List(ctx context.Context, accountID string, limit int) ([]Record, error) {
	acct, err := accountParam(accountID)
	if err != nil {
		return nil, err
	}
	var out []Record
	err = postgres.WithTenant(ctx, p.pool, accountID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, listSQL, acct, clampLimit(limit))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			rec, err := scanRecord(rows)
			if err != nil {
				return err
			}
			out = append(out, rec)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("dispatcherheartbeat: list: %w", err)
	}
	return out, nil
}

// getSQL is Get's EXACT (account, label) read. The COALESCE pair is the 0121
// unique expression index's key, so two NULL accounts match and the read is an
// index point lookup. It deliberately does NOT use listSQL's visibility rule:
// an untenanted row is invisible to a tenanted caller here.
const getSQL = `SELECT COALESCE(account_id::text, ''), host_label, COALESCE(token_id, ''), payload, received_at
	FROM host_dispatcher_heartbeats
	WHERE host_label = $2
	  AND COALESCE(account_id, '00000000-0000-0000-0000-000000000000'::uuid) = COALESCE($1::uuid, '00000000-0000-0000-0000-000000000000'::uuid)`

// Get implements Repository.
func (p *PostgresRepository) Get(ctx context.Context, accountID, hostLabel string) (Record, bool, error) {
	acct, err := accountParam(accountID)
	if err != nil {
		return Record{}, false, err
	}
	var (
		rec   Record
		found bool
	)
	err = postgres.WithTenant(ctx, p.pool, accountID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, getSQL, acct, hostLabel)
		if err != nil {
			return err
		}
		defer rows.Close()
		if rows.Next() {
			if rec, err = scanRecord(rows); err != nil {
				return err
			}
			found = true
		}
		return rows.Err()
	})
	if err != nil {
		return Record{}, false, fmt.Errorf("dispatcherheartbeat: get: %w", err)
	}
	return rec, found, nil
}

// scanRecord reads one row of listSQL's / getSQL's column list.
func scanRecord(rows pgx.Rows) (Record, error) {
	var (
		rec     Record
		payload []byte
	)
	if err := rows.Scan(&rec.AccountID, &rec.HostLabel, &rec.TokenID, &payload, &rec.ReceivedAt); err != nil {
		return Record{}, err
	}
	var sp storedPayload
	if err := json.Unmarshal(payload, &sp); err != nil {
		return Record{}, fmt.Errorf("decode payload for host %q: %w", rec.HostLabel, err)
	}
	rec.Heartbeat = sp.Heartbeat
	rec.DroppedDeclaration = sp.DroppedDeclaration
	rec.ReceivedAt = rec.ReceivedAt.UTC()
	return rec, nil
}

// MemoryRepository is an in-memory Repository with the same account
// semantics, for tests. It has no host-label CHECK, so a caller that skips
// label validation is observable through Records(). Set UpsertErr, ListErr or
// GetErr to inject a failure.
type MemoryRepository struct {
	mu        sync.Mutex
	rows      map[memoryKey]Record
	listCalls int
	UpsertErr error
	ListErr   error
	GetErr    error
}

type memoryKey struct{ account, label string }

// NewMemoryRepository returns an empty MemoryRepository.
func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{rows: map[memoryKey]Record{}}
}

var _ Repository = (*MemoryRepository)(nil)

// Upsert implements Repository.
func (m *MemoryRepository) Upsert(_ context.Context, rec Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.UpsertErr != nil {
		return m.UpsertErr
	}
	if _, err := accountParam(rec.AccountID); err != nil {
		return err
	}
	if m.rows == nil {
		m.rows = map[memoryKey]Record{}
	}
	rec.ReceivedAt = rec.ReceivedAt.UTC()
	rec.Heartbeat.RunRefusals = append([]hostdispatcher.RunRefusal(nil), rec.Heartbeat.RunRefusals...)
	m.rows[memoryKey{rec.AccountID, rec.HostLabel}] = rec
	return nil
}

// List implements Repository and counts the call (ListCalls), error or not.
func (m *MemoryRepository) List(_ context.Context, accountID string, limit int) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listCalls++
	if m.ListErr != nil {
		return nil, m.ListErr
	}
	if _, err := accountParam(accountID); err != nil {
		return nil, err
	}
	var out []Record
	for k, rec := range m.rows {
		if k.account == "" || (accountID != "" && k.account == accountID) {
			out = append(out, rec)
		}
	}
	sortRecords(out)
	if n := clampLimit(limit); len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// Get implements Repository with the Postgres repository's EXACT account
// match: the key is (accountID, hostLabel), so an untenanted row never answers
// a tenanted caller.
func (m *MemoryRepository) Get(_ context.Context, accountID, hostLabel string) (Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.GetErr != nil {
		return Record{}, false, m.GetErr
	}
	if _, err := accountParam(accountID); err != nil {
		return Record{}, false, err
	}
	rec, ok := m.rows[memoryKey{accountID, hostLabel}]
	if !ok {
		return Record{}, false, nil
	}
	rec.Heartbeat.RunRefusals = append([]hostdispatcher.RunRefusal(nil), rec.Heartbeat.RunRefusals...)
	return rec, true, nil
}

// ListCalls reports how many times List was called.
func (m *MemoryRepository) ListCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listCalls
}

// Records returns every stored record (all accounts), in List order.
func (m *MemoryRepository) Records() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Record, 0, len(m.rows))
	for _, rec := range m.rows {
		out = append(out, rec)
	}
	sortRecords(out)
	return out
}

func sortRecords(recs []Record) {
	sort.Slice(recs, func(i, j int) bool {
		if !recs[i].ReceivedAt.Equal(recs[j].ReceivedAt) {
			return recs[i].ReceivedAt.After(recs[j].ReceivedAt)
		}
		if recs[i].HostLabel != recs[j].HostLabel {
			return recs[i].HostLabel < recs[j].HostLabel
		}
		return recs[i].AccountID < recs[j].AccountID
	})
}
