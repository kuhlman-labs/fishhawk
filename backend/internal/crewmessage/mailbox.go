package crewmessage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// The three audit categories the mailbox writes (E77.2 / #3736, ADR-081 D2).
// Every send and every disposition is one of these chain entries FIRST; the
// crew_messages row is a derived index over them (Rebuild).
const (
	// CategorySent records one sent message. Its sequence IS the message's
	// identity (crew_messages.sent_sequence).
	CategorySent = "crew_message_sent"
	// CategoryDisposed records the one disposition that closed a message.
	CategoryDisposed = "crew_message_disposed"
	// CategoryEscalated records that a thread's reject-and-reply exchange hit
	// its round bound: the refused rejection is NOT recorded, this is.
	CategoryEscalated = "crew_message_escalated"
)

// DefaultRoundBound is the number of rejections one thread may record before
// the next is refused and escalated (ADR-081 rule 4).
const DefaultRoundBound = 3

// Thread-membership sentinels (Send with a ThreadRootSequence). Each is
// returned BEFORE anything is appended to the chain.
var (
	// ErrThreadRootNotFound: the named sequence is not a crew_message_sent
	// entry on the chain (it exists nowhere, or it is some other category).
	ErrThreadRootNotFound = errors.New("crewmessage: thread root not found")
	// ErrThreadRootNotRoot: the named sent entry is itself a reply, so it
	// cannot root a thread — a reply names its thread's ROOT, never a sibling.
	ErrThreadRootNotRoot = errors.New("crewmessage: thread root reference names a reply, not a root")
	// ErrThreadCrossAccount: the root belongs to a different account than the
	// reply would be recorded under.
	ErrThreadCrossAccount = errors.New("crewmessage: thread root belongs to another account")
	// ErrThreadAnchorMismatch: the reply's anchor differs from the root's.
	ErrThreadAnchorMismatch = errors.New("crewmessage: reply anchor does not match the thread root's anchor")
)

// ProjectionError reports that a chain entry was COMMITTED but projecting it
// into crew_messages failed. The chain is authoritative, so the operation
// happened; the index gap is closed by Rebuild. Callers must not retry the
// operation on this error — that would record it twice.
type ProjectionError struct {
	Sequence int64
	Err      error
}

func (e *ProjectionError) Error() string {
	return fmt.Sprintf("crewmessage: chain entry %d committed but not projected (Rebuild closes the gap): %v", e.Sequence, e.Err)
}

func (e *ProjectionError) Unwrap() error { return e.Err }

// Actor is who performed a send or a disposition, recorded on the chain entry.
type Actor struct {
	Kind    audit.ActorKind
	Subject string
}

func (a Actor) kindPtr() *audit.ActorKind {
	if a.Kind == "" {
		return nil
	}
	k := a.Kind
	return &k
}

func (a Actor) subjectPtr() *string {
	if a.Subject == "" {
		return nil
	}
	s := a.Subject
	return &s
}

// sentPayload is the crew_message_sent entry's payload. ThreadRootSequence is
// ABSENT for a thread root (its own sequence is not known until the insert)
// and names the root for a reply.
type sentPayload struct {
	Message            json.RawMessage `json:"message"`
	ThreadRootSequence *int64          `json:"thread_root_sequence,omitempty"`
}

// disposedPayload is the crew_message_disposed entry's payload. The reason
// prose lives HERE and only here; the row points at this entry.
type disposedPayload struct {
	SentSequence       int64       `json:"sent_sequence"`
	ThreadRootSequence int64       `json:"thread_root_sequence"`
	Disposition        Disposition `json:"disposition"`
	Round              int         `json:"round"`
	Reason             string      `json:"reason,omitempty"`
}

// escalatedPayload is the crew_message_escalated entry's payload.
type escalatedPayload struct {
	SentSequence       int64 `json:"sent_sequence"`
	ThreadRootSequence int64 `json:"thread_root_sequence"`
	RoundBound         int   `json:"round_bound"`
	Rejections         int   `json:"rejections"`
}

// Mailbox is the chain-authoritative crew-message domain layer: it writes the
// chain entry first and projects the derived row second.
type Mailbox struct {
	db         DBTX
	store      *Store
	roundBound int
	now        func() time.Time

	// appendRun / appendGlobal are the chain writers: audit.AppendChainedTx
	// for a run anchor and audit.AppendGlobalChainedTx for the two run-less
	// anchors. Fields only so a test can inject an append failure.
	appendRun    func(context.Context, pgx.Tx, audit.ChainAppendParams) (*audit.Entry, error)
	appendGlobal func(context.Context, pgx.Tx, audit.GlobalChainAppendParams) (*audit.Entry, error)

	// afterChainAppend is a TEST-ONLY seam, nil in production: Send calls it
	// after its chain entry commits and BEFORE projecting it, so a test can
	// drive a full Dispose into that window (the delayed-send-projection
	// interleaving the monotonic guard exists for).
	afterChainAppend func(ctx context.Context, sent *audit.Entry)
}

// NewMailbox returns a Mailbox over db with the given per-thread round bound;
// a bound <= 0 selects DefaultRoundBound.
func NewMailbox(db DBTX, roundBound int) *Mailbox {
	if roundBound <= 0 {
		roundBound = DefaultRoundBound
	}
	return &Mailbox{
		db:           db,
		store:        NewStore(db),
		roundBound:   roundBound,
		now:          func() time.Time { return time.Now().UTC() },
		appendRun:    audit.AppendChainedTx,
		appendGlobal: audit.AppendGlobalChainedTx,
	}
}

// Store returns the mailbox's derived-table store (reads).
func (m *Mailbox) Store() *Store { return m.store }

// SendParams is one Send.
type SendParams struct {
	// RawMessage is the crew-message-v1 document. It is validated through
	// Parse, so every Go-layer rule (invariant #8 included) gates it.
	RawMessage []byte
	Actor      Actor
	// AccountID is the chain partition for a RUN-LESS anchor (issue_ref or
	// decision_record_id). It is ignored for a run anchor, whose entry is
	// stamped with the run's own account.
	AccountID *uuid.UUID
	// ThreadRootSequence makes this message a reply in the thread rooted at
	// that crew_message_sent entry; nil makes it a thread root.
	ThreadRootSequence *int64
}

// Send validates, records one crew_message_sent chain entry, and projects it.
// On a projection failure after the entry committed it returns the built row
// alongside a *ProjectionError.
func (m *Mailbox) Send(ctx context.Context, p SendParams) (*Row, error) {
	msg, err := Parse(p.RawMessage)
	if err != nil {
		return nil, err
	}
	// Every field the projection derives from the message is checked BEFORE
	// the append, so a committed entry can always be projected.
	if _, err := projectSentRow(&audit.Entry{}, msg, nil); err != nil {
		return nil, fmt.Errorf("crewmessage: send: %w", err)
	}
	if p.ThreadRootSequence != nil {
		if err := m.checkThreadRoot(ctx, *p.ThreadRootSequence, msg, p.AccountID); err != nil {
			return nil, err
		}
	}
	payload, err := json.Marshal(sentPayload{Message: p.RawMessage, ThreadRootSequence: p.ThreadRootSequence})
	if err != nil {
		return nil, fmt.Errorf("crewmessage: marshal sent payload: %w", err)
	}

	var entry *audit.Entry
	err = pgx.BeginFunc(ctx, m.db, func(tx pgx.Tx) error {
		var aerr error
		entry, aerr = m.appendEntry(ctx, tx, msg.Anchor, p.AccountID, CategorySent, p.Actor, payload)
		return aerr
	})
	if err != nil {
		return nil, fmt.Errorf("crewmessage: send: %w", err)
	}

	if m.afterChainAppend != nil {
		m.afterChainAppend(ctx, entry)
	}

	// Cannot fail: the same message was projected before the append.
	row, _ := projectSentRow(entry, msg, p.ThreadRootSequence)
	if err := m.store.Upsert(ctx, row); err != nil {
		return &row, &ProjectionError{Sequence: entry.Sequence, Err: err}
	}
	return &row, nil
}

// appendEntry routes one chain append by anchor: a run anchor chains on the
// run, a run-less anchor on the account's global partition.
func (m *Mailbox) appendEntry(ctx context.Context, tx pgx.Tx, anchor Anchor, account *uuid.UUID, category string, actor Actor, payload json.RawMessage) (*audit.Entry, error) {
	ts := m.now()
	if anchor.RunID != "" {
		runID, err := uuid.Parse(anchor.RunID)
		if err != nil {
			return nil, fmt.Errorf("crewmessage: anchor run_id: %w", err)
		}
		return m.appendRun(ctx, tx, audit.ChainAppendParams{
			RunID: runID, Timestamp: ts, Category: category,
			ActorKind: actor.kindPtr(), ActorSubject: actor.subjectPtr(), Payload: payload,
		})
	}
	return m.appendGlobal(ctx, tx, audit.GlobalChainAppendParams{
		Timestamp: ts, Category: category,
		ActorKind: actor.kindPtr(), ActorSubject: actor.subjectPtr(), Payload: payload,
		AccountID: account,
	})
}

// checkThreadRoot is the thread-membership gate: rootSeq must be a
// crew_message_sent entry that is itself a root, in the reply's account, on
// the reply's anchor. For a run-anchored reply the account check is implied
// by the anchor check — one run belongs to one account.
func (m *Mailbox) checkThreadRoot(ctx context.Context, rootSeq int64, reply *Message, account *uuid.UUID) error {
	e, err := getChainEntry(ctx, m.db, rootSeq)
	if errors.Is(err, ErrMessageNotFound) {
		return fmt.Errorf("%w: sequence %d", ErrThreadRootNotFound, rootSeq)
	}
	if err != nil {
		return err
	}
	if e.Category != CategorySent {
		return fmt.Errorf("%w: sequence %d is a %q entry", ErrThreadRootNotFound, rootSeq, e.Category)
	}
	sp, rootMsg, err := decodeSent(e)
	if err != nil {
		return fmt.Errorf("%w: sequence %d: %v", ErrThreadRootNotFound, rootSeq, err)
	}
	if reply.Anchor.RunID == "" && !sameAccount(e.AccountID, account) {
		return fmt.Errorf("%w: sequence %d", ErrThreadCrossAccount, rootSeq)
	}
	if sp.ThreadRootSequence != nil {
		return fmt.Errorf("%w: sequence %d replies to %d", ErrThreadRootNotRoot, rootSeq, *sp.ThreadRootSequence)
	}
	if rootMsg.Anchor != reply.Anchor {
		return fmt.Errorf("%w: sequence %d", ErrThreadAnchorMismatch, rootSeq)
	}
	return nil
}

func sameAccount(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// DisposeParams is one Dispose.
type DisposeParams struct {
	SentSequence int64
	Disposition  Disposition
	// Reason is recorded on the disposition chain entry only; the row points
	// at that entry (reason_sequence) and never copies the prose.
	Reason string
	Actor  Actor
}

// Dispose closes an open message. Order is load-bearing and every refusal
// appends NOTHING:
//
//  1. ValidDisposition            -> ErrInvalidDisposition
//  2. resolve the sent entry from the CHAIN -> ErrMessageNotFound
//  3. in ONE READ COMMITTED transaction: pg_advisory_xact_lock(thread root)
//     FIRST, then
//     a. the message already has a disposition on the chain -> ErrAlreadyDisposed
//     b. a rejection when the thread already holds roundBound rejections ->
//     one crew_message_escalated entry (only the thread's first) and
//     ErrRoundBoundExhausted
//     c. otherwise append crew_message_disposed.
//
// Because every check runs after the thread lock and READ COMMITTED takes a
// fresh snapshot per statement, a check observes every racing disposition
// that committed before the lock was granted (audit/budget.go's argument).
// The thread lock is always taken BEFORE the chain helper's own run-row or
// partition lock, so the two can never invert.
func (m *Mailbox) Dispose(ctx context.Context, p DisposeParams) (*Row, error) {
	if !ValidDisposition(p.Disposition) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidDisposition, p.Disposition)
	}
	sentEntry, err := getChainEntry(ctx, m.db, p.SentSequence)
	if err != nil {
		return nil, err
	}
	if sentEntry.Category != CategorySent {
		return nil, fmt.Errorf("crewmessage: sequence %d is a %q entry: %w", p.SentSequence, sentEntry.Category, ErrMessageNotFound)
	}
	sp, msg, err := decodeSent(sentEntry)
	if err != nil {
		return nil, fmt.Errorf("crewmessage: sequence %d: %v: %w", p.SentSequence, err, ErrMessageNotFound)
	}
	sent, err := projectSentRow(sentEntry, msg, sp.ThreadRootSequence)
	if err != nil {
		return nil, fmt.Errorf("crewmessage: sequence %d: %v: %w", p.SentSequence, err, ErrMessageNotFound)
	}
	root := sent.ThreadRootSequence

	var (
		dispEntry *audit.Entry
		dp        disposedPayload
		exhausted bool
	)
	err = pgx.BeginFunc(ctx, m.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, root); err != nil {
			return fmt.Errorf("crewmessage: lock thread %d: %w", root, err)
		}
		disposed, err := chainHasDisposition(ctx, tx, p.SentSequence)
		if err != nil {
			return err
		}
		if disposed {
			return fmt.Errorf("crewmessage: sequence %d: %w", p.SentSequence, ErrAlreadyDisposed)
		}
		round := 0
		if p.Disposition == DispositionRejected {
			n, err := countThreadRejections(ctx, tx, root)
			if err != nil {
				return err
			}
			if n >= m.roundBound {
				exhausted = true
				return m.escalate(ctx, tx, sent, msg.Anchor, n, p.Actor)
			}
			round = n + 1
		}
		dp = disposedPayload{
			SentSequence: p.SentSequence, ThreadRootSequence: root,
			Disposition: p.Disposition, Round: round, Reason: p.Reason,
		}
		payload, err := json.Marshal(dp)
		if err != nil {
			return fmt.Errorf("crewmessage: marshal disposed payload: %w", err)
		}
		dispEntry, err = m.appendEntry(ctx, tx, msg.Anchor, sent.AccountID, CategoryDisposed, p.Actor, payload)
		return err
	})
	if err != nil {
		return nil, err
	}
	if exhausted {
		return nil, fmt.Errorf("crewmessage: thread %d holds %d rejections: %w", root, m.roundBound, ErrRoundBoundExhausted)
	}

	terminal := applyDisposed(sent, dispEntry, dp)
	if _, err := projectDisposition(ctx, m.store, terminal); err != nil {
		return &terminal, &ProjectionError{Sequence: dispEntry.Sequence, Err: err}
	}
	return &terminal, nil
}

// escalate records the thread's crew_message_escalated entry, once: a thread
// already escalated is not escalated again by a later refused rejection.
func (m *Mailbox) escalate(ctx context.Context, tx pgx.Tx, sent Row, anchor Anchor, rejections int, actor Actor) error {
	var already bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_entries
		WHERE category = $1 AND payload->>'thread_root_sequence' = $2)`,
		CategoryEscalated, strconv.FormatInt(sent.ThreadRootSequence, 10),
	).Scan(&already); err != nil {
		return fmt.Errorf("crewmessage: read thread %d escalation: %w", sent.ThreadRootSequence, err)
	}
	if already {
		return nil
	}
	payload, err := json.Marshal(escalatedPayload{
		SentSequence: sent.SentSequence, ThreadRootSequence: sent.ThreadRootSequence,
		RoundBound: m.roundBound, Rejections: rejections,
	})
	if err != nil {
		return fmt.Errorf("crewmessage: marshal escalated payload: %w", err)
	}
	_, err = m.appendEntry(ctx, tx, anchor, sent.AccountID, CategoryEscalated, actor, payload)
	return err
}

// chainHasDisposition reports whether the chain already records a
// disposition for sentSeq. Payload keys are compared as TEXT so no other
// category's payload can fail a cast.
func chainHasDisposition(ctx context.Context, q DBTX, sentSeq int64) (bool, error) {
	var ok bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_entries
		WHERE category = $1 AND payload->>'sent_sequence' = $2)`,
		CategoryDisposed, strconv.FormatInt(sentSeq, 10),
	).Scan(&ok); err != nil {
		return false, fmt.Errorf("crewmessage: read disposition of %d: %w", sentSeq, err)
	}
	return ok, nil
}

// countThreadRejections counts the thread's rejections FROM THE CHAIN, never
// from the derived table, so truncating crew_messages cannot reset the bound.
func countThreadRejections(ctx context.Context, q DBTX, root int64) (int, error) {
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM audit_entries
		WHERE category = $1 AND payload->>'thread_root_sequence' = $2 AND payload->>'disposition' = $3`,
		CategoryDisposed, strconv.FormatInt(root, 10), string(DispositionRejected),
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("crewmessage: count thread %d rejections: %w", root, err)
	}
	return n, nil
}

// chainEntryColumns is the audit_entries projection the mailbox and Rebuild
// read; scanChainEntry scans it.
const chainEntryColumns = `sequence, run_id, account_id, category, payload, entry_hash, ts`

func scanChainEntry(row pgx.Row) (*audit.Entry, error) {
	var e audit.Entry
	var payload []byte
	if err := row.Scan(&e.Sequence, &e.RunID, &e.AccountID, &e.Category, &payload, &e.EntryHash, &e.Timestamp); err != nil {
		return nil, err
	}
	e.Payload = payload
	e.Timestamp = e.Timestamp.UTC()
	return &e, nil
}

// getChainEntry reads the chain entry at seq, or ErrMessageNotFound.
func getChainEntry(ctx context.Context, q DBTX, seq int64) (*audit.Entry, error) {
	e, err := scanChainEntry(q.QueryRow(ctx,
		`SELECT `+chainEntryColumns+` FROM audit_entries WHERE sequence = $1`, seq))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("crewmessage: sequence %d: %w", seq, ErrMessageNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("crewmessage: read chain entry %d: %w", seq, err)
	}
	return e, nil
}

// decodeSent decodes a crew_message_sent entry, re-validating the carried
// message through Parse.
func decodeSent(e *audit.Entry) (sentPayload, *Message, error) {
	var sp sentPayload
	if err := json.Unmarshal(e.Payload, &sp); err != nil {
		return sentPayload{}, nil, fmt.Errorf("decode sent payload: %w", err)
	}
	msg, err := Parse(sp.Message)
	if err != nil {
		return sentPayload{}, nil, fmt.Errorf("decode sent message: %w", err)
	}
	return sp, msg, nil
}

// projectSentRow is the SHARED send-projection builder: Send and Rebuild both
// build an open row through it, which is what makes rebuild identity
// structural rather than two code paths hoping to agree.
func projectSentRow(e *audit.Entry, msg *Message, threadRoot *int64) (Row, error) {
	if e == nil {
		return Row{}, ErrNilEntry
	}
	r := Row{
		SentSequence:        e.Sequence,
		SentEntryHash:       e.EntryHash,
		AccountID:           e.AccountID,
		IssueRef:            msg.Anchor.IssueRef,
		DecisionRecordID:    msg.Anchor.DecisionRecordID,
		MessageType:         msg.Type,
		SenderRole:          msg.SenderRole,
		RecipientRole:       msg.RecipientRole,
		ResponseRequired:    msg.ResponseRequired,
		ThreadRootSequence:  e.Sequence,
		State:               StateOpen,
		SentAt:              e.Timestamp.UTC(),
		LastAppliedSequence: e.Sequence,
	}
	if threadRoot != nil {
		r.ThreadRootSequence = *threadRoot
	}
	if msg.Anchor.RunID != "" {
		id, err := uuid.Parse(msg.Anchor.RunID)
		if err != nil {
			return Row{}, fmt.Errorf("anchor run_id: %w", err)
		}
		r.RunID = &id
	}
	if msg.Deadline != "" {
		// RFC 3339 §5.6 permits a lowercase "t" / "z", which the schema's
		// date-time format accepts and time.RFC3339 does not.
		d, err := time.Parse(time.RFC3339, strings.ToUpper(msg.Deadline))
		if err != nil {
			return Row{}, fmt.Errorf("deadline: %w", err)
		}
		d = d.UTC().Truncate(time.Microsecond)
		r.Deadline = &d
	}
	return r, nil
}

// applyDisposed is the SHARED disposition builder: the terminal row a
// disposition entry turns sent into. Live Dispose and Rebuild both use it.
func applyDisposed(sent Row, e *audit.Entry, dp disposedPayload) Row {
	r := sent
	seq := e.Sequence
	r.State = StateFor(dp.Disposition)
	r.DispositionSequence = &seq
	if dp.Reason != "" {
		rs := seq
		r.ReasonSequence = &rs
	}
	r.Round = dp.Round
	at := e.Timestamp.UTC()
	r.DisposedAt = &at
	r.LastAppliedSequence = seq
	return r
}

// projectDisposition is the SHARED disposition projection. It applies the
// pending-only Transition; when the send projection has not landed yet it
// upserts the whole terminal row through the monotonic guard instead; and
// when the row is ALREADY terminal it leaves it and reports applied=false —
// the FIRST disposition in chain order wins, in live processing and in
// Rebuild alike (the only first-wins control; Rebuild keeps no second one).
func projectDisposition(ctx context.Context, s *Store, terminal Row) (applied bool, err error) {
	err = s.Transition(ctx, Transition{
		SentSequence:        terminal.SentSequence,
		State:               terminal.State,
		DispositionSequence: *terminal.DispositionSequence,
		ReasonSequence:      terminal.ReasonSequence,
		Round:               terminal.Round,
		DisposedAt:          *terminal.DisposedAt,
	})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrAlreadyDisposed):
		return false, nil
	case errors.Is(err, ErrMessageNotFound):
		if err := s.Upsert(ctx, terminal); err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, err
	}
}
