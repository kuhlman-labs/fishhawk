package crewmessage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DBTX is the query surface the store runs against. Both *pgxpool.Pool and
// pgx.Tx satisfy it, so a caller can run the store inside a
// postgres.WithTenant transaction (the RLS test does) or straight on the pool.
//
// The store is hand-written pgx rather than sqlc-generated: a local
// `sqlc generate` regenerates every package in backend/sqlc.yaml. The queries
// are covered by the pgtest-backed store tests instead of a compile check.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Row is one crew_messages row (migration 0090): the DERIVED projection of a
// message's crew_message_sent chain entry plus, once disposed, its
// disposition entry. Every field is reconstructable from the chain; there is
// deliberately no operational-metadata field (no indexed_at), because one
// would break full-row rebuild equality.
type Row struct {
	// SentSequence is the chain sequence of the crew_message_sent entry — the
	// primary key.
	SentSequence int64
	// SentEntryHash cites that entry's entry_hash.
	SentEntryHash string
	AccountID     *uuid.UUID
	// Exactly one of RunID / IssueRef / DecisionRecordID is set: the anchor.
	RunID            *uuid.UUID
	IssueRef         string
	DecisionRecordID string
	MessageType      MessageType
	SenderRole       Role
	RecipientRole    Role
	ResponseRequired bool
	Deadline         *time.Time
	// ThreadRootSequence is SentSequence for a thread root and the root's
	// SentSequence for a reply.
	ThreadRootSequence int64
	State              State
	// DispositionSequence is the chain sequence of the disposition entry that
	// closed the message; nil while open.
	DispositionSequence *int64
	// ReasonSequence POINTS AT the chain entry carrying the reason prose, which
	// is never copied into the row.
	ReasonSequence *int64
	// Round is the thread's rejection round this disposition closed (0 while
	// open or for a non-rejection disposition); see the package README.
	Round      int
	SentAt     time.Time
	DisposedAt *time.Time
	// LastAppliedSequence is the chain sequence of the newest entry projected
	// into the row: SentSequence for a send projection, DispositionSequence for
	// a disposition projection. Upsert only ever moves it FORWARD.
	LastAppliedSequence int64
}

// Store reads and writes crew_messages rows.
type Store struct {
	db DBTX
}

// NewStore returns a Store over db.
func NewStore(db DBTX) *Store {
	return &Store{db: db}
}

// rowColumns is the full column list, in Row field order. Every read and write
// names it so a column cannot be written but not read (or vice versa).
const rowColumns = `sent_sequence, sent_entry_hash, account_id, run_id, issue_ref,
	decision_record_id, message_type, sender_role, recipient_role, response_required,
	deadline, thread_root_sequence, state, disposition_sequence, reason_sequence,
	round, sent_at, disposed_at, last_applied_sequence`

// upsertSQL is the MONOTONIC guarded upsert. Its WHERE clause is the control:
// a conflicting row is overwritten ONLY by a projection of a strictly newer
// chain entry. A send projection carries last_applied_sequence = sent_sequence
// and a disposition projection the strictly larger disposition sequence, so a
// send projection delayed past a disposition's is a NO-OP instead of a
// regression of a terminal row back to open. A replay of the same entry is
// likewise a no-op, which is what makes a rebuild idempotent.
const upsertSQL = `INSERT INTO crew_messages (` + rowColumns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
ON CONFLICT (sent_sequence) DO UPDATE SET
	sent_entry_hash       = EXCLUDED.sent_entry_hash,
	account_id            = EXCLUDED.account_id,
	run_id                = EXCLUDED.run_id,
	issue_ref             = EXCLUDED.issue_ref,
	decision_record_id    = EXCLUDED.decision_record_id,
	message_type          = EXCLUDED.message_type,
	sender_role           = EXCLUDED.sender_role,
	recipient_role        = EXCLUDED.recipient_role,
	response_required     = EXCLUDED.response_required,
	deadline              = EXCLUDED.deadline,
	thread_root_sequence  = EXCLUDED.thread_root_sequence,
	state                 = EXCLUDED.state,
	disposition_sequence  = EXCLUDED.disposition_sequence,
	reason_sequence       = EXCLUDED.reason_sequence,
	round                 = EXCLUDED.round,
	sent_at               = EXCLUDED.sent_at,
	disposed_at           = EXCLUDED.disposed_at,
	last_applied_sequence = EXCLUDED.last_applied_sequence
WHERE crew_messages.last_applied_sequence < EXCLUDED.last_applied_sequence`

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func upsertArgs(r Row) []any {
	return []any{
		r.SentSequence, r.SentEntryHash, r.AccountID, r.RunID, r.IssueRef,
		r.DecisionRecordID, string(r.MessageType), string(r.SenderRole), string(r.RecipientRole), r.ResponseRequired,
		utcPtr(r.Deadline), r.ThreadRootSequence, string(r.State), r.DispositionSequence, r.ReasonSequence,
		r.Round, r.SentAt.UTC(), utcPtr(r.DisposedAt), r.LastAppliedSequence,
	}
}

// Upsert writes r keyed on its sent sequence, through the monotonic guard: an
// existing row whose LastAppliedSequence is already >= r's is left untouched,
// and that is NOT an error (a stale projection is expected under interleaving).
func (s *Store) Upsert(ctx context.Context, r Row) error {
	if _, err := s.db.Exec(ctx, upsertSQL, upsertArgs(r)...); err != nil {
		return fmt.Errorf("crewmessage: upsert sequence %d: %w", r.SentSequence, err)
	}
	return nil
}

// Transition is one disposition applied to an open row.
type Transition struct {
	SentSequence int64
	// State is the target state; it must be Terminal.
	State               State
	DispositionSequence int64
	ReasonSequence      *int64
	Round               int
	DisposedAt          time.Time
}

// transitionSQL is the PENDING-ONLY atomic transition (the scopeamendment
// Decide style): the UPDATE matches only an OPEN row, so of any number of
// concurrent dispositions exactly one can move it. The last_applied_sequence
// arm keeps the transition inside the same monotonic order as Upsert. The
// EXISTS arm reads the pre-statement snapshot, so zero updated rows can be told
// apart as "no such row" versus "row no longer open" in the same statement.
const transitionSQL = `WITH moved AS (
	UPDATE crew_messages SET
		state                 = $2,
		disposition_sequence  = $3,
		reason_sequence       = $4,
		round                 = $5,
		disposed_at           = $6,
		last_applied_sequence = $3
	WHERE sent_sequence = $1
	  AND state = 'open'
	  AND last_applied_sequence < $3
	RETURNING 1
)
SELECT (SELECT count(*) FROM moved),
       EXISTS (SELECT 1 FROM crew_messages WHERE sent_sequence = $1)`

// Transition moves an OPEN row to tr.State. It refuses a non-terminal target
// with ErrInvalidDisposition before touching the database, returns
// ErrMessageNotFound when no row exists at tr.SentSequence (the send
// projection has not landed yet — the caller then upserts a terminal row), and
// ErrAlreadyDisposed when the row exists but cannot take this transition:
// it is no longer open, or it already reflects a chain entry at or after
// tr.DispositionSequence (impossible on an append-only chain, where a
// disposition always follows its send, and refused rather than applied).
func (s *Store) Transition(ctx context.Context, tr Transition) error {
	if !tr.State.Terminal() {
		return fmt.Errorf("%w: target state %q is not terminal", ErrInvalidDisposition, tr.State)
	}
	var (
		moved  int64
		exists bool
	)
	if err := s.db.QueryRow(ctx, transitionSQL,
		tr.SentSequence, string(tr.State), tr.DispositionSequence, tr.ReasonSequence,
		tr.Round, tr.DisposedAt.UTC(),
	).Scan(&moved, &exists); err != nil {
		return fmt.Errorf("crewmessage: transition sequence %d: %w", tr.SentSequence, err)
	}
	switch {
	case moved == 1:
		return nil
	case !exists:
		return fmt.Errorf("crewmessage: transition sequence %d: %w", tr.SentSequence, ErrMessageNotFound)
	default:
		return fmt.Errorf("crewmessage: transition sequence %d: %w", tr.SentSequence, ErrAlreadyDisposed)
	}
}

// Get returns the row at sentSequence, or ErrMessageNotFound.
func (s *Store) Get(ctx context.Context, sentSequence int64) (Row, error) {
	rows, err := s.db.Query(ctx,
		`SELECT `+rowColumns+` FROM crew_messages WHERE sent_sequence = $1`, sentSequence)
	if err != nil {
		return Row{}, fmt.Errorf("crewmessage: get sequence %d: %w", sentSequence, err)
	}
	r, err := pgx.CollectExactlyOneRow(rows, scanRow)
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, fmt.Errorf("crewmessage: get sequence %d: %w", sentSequence, ErrMessageNotFound)
	}
	if err != nil {
		return Row{}, fmt.Errorf("crewmessage: get sequence %d: %w", sentSequence, err)
	}
	return r, nil
}

// ListFilter narrows ListByRecipient. An empty field matches any value;
// Limit <= 0 means unbounded.
type ListFilter struct {
	RecipientRole Role
	State         State
	Limit         int
}

// ListByRecipient returns the rows matching f, ordered by sent sequence.
func (s *Store) ListByRecipient(ctx context.Context, f ListFilter) ([]Row, error) {
	var limit *int
	if f.Limit > 0 {
		limit = &f.Limit
	}
	return s.list(ctx, "list by recipient", `SELECT `+rowColumns+` FROM crew_messages
		WHERE ($1 = '' OR recipient_role = $1)
		  AND ($2 = '' OR state = $2)
		ORDER BY sent_sequence
		LIMIT $3`, string(f.RecipientRole), string(f.State), limit)
}

// AnchorFilter names ONE anchor. The match is EXACT on all three anchor
// columns (the unset ones must be unset on the row too), so a filter naming
// nothing matches no valid message rather than every message.
type AnchorFilter struct {
	RunID            *uuid.UUID
	IssueRef         string
	DecisionRecordID string
}

// ListByAnchor returns the rows on the anchor f names, ordered by sent sequence.
func (s *Store) ListByAnchor(ctx context.Context, f AnchorFilter) ([]Row, error) {
	return s.list(ctx, "list by anchor", `SELECT `+rowColumns+` FROM crew_messages
		WHERE run_id IS NOT DISTINCT FROM $1
		  AND issue_ref = $2
		  AND decision_record_id = $3
		ORDER BY sent_sequence`, f.RunID, f.IssueRef, f.DecisionRecordID)
}

// CountThreadRejections counts the thread's messages the INDEX records as
// rejected. It reads the derived table, so it is a read convenience only: a
// bound that must survive a truncate counts from the chain instead.
func (s *Store) CountThreadRejections(ctx context.Context, threadRootSequence int64) (int, error) {
	var n int
	if err := s.db.QueryRow(ctx,
		`SELECT count(*) FROM crew_messages WHERE thread_root_sequence = $1 AND state = 'rejected'`,
		threadRootSequence,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("crewmessage: count thread %d rejections: %w", threadRootSequence, err)
	}
	return n, nil
}

// Truncate empties the index — the from-scratch rebuild path. Safe because the
// table is derived: the chain still holds every fact.
func (s *Store) Truncate(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, `TRUNCATE crew_messages`); err != nil {
		return fmt.Errorf("crewmessage: truncate: %w", err)
	}
	return nil
}

func (s *Store) list(ctx context.Context, op, sql string, args ...any) ([]Row, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("crewmessage: %s: %w", op, err)
	}
	out, err := pgx.CollectRows(rows, scanRow)
	if err != nil {
		return nil, fmt.Errorf("crewmessage: %s: %w", op, err)
	}
	return out, nil
}

func scanRow(row pgx.CollectableRow) (Row, error) {
	var (
		r                             Row
		msgType, sender, recip, state string
	)
	if err := row.Scan(
		&r.SentSequence, &r.SentEntryHash, &r.AccountID, &r.RunID, &r.IssueRef,
		&r.DecisionRecordID, &msgType, &sender, &recip, &r.ResponseRequired,
		&r.Deadline, &r.ThreadRootSequence, &state, &r.DispositionSequence, &r.ReasonSequence,
		&r.Round, &r.SentAt, &r.DisposedAt, &r.LastAppliedSequence,
	); err != nil {
		return Row{}, err
	}
	r.MessageType = MessageType(msgType)
	r.SenderRole = Role(sender)
	r.RecipientRole = Role(recip)
	r.State = State(state)
	r.Deadline = utcPtr(r.Deadline)
	r.SentAt = r.SentAt.UTC()
	r.DisposedAt = utcPtr(r.DisposedAt)
	return r, nil
}
