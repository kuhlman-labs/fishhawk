package digest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// MarkRead's ORDER is load-bearing and is the first thing to check:
//
//  1. Resolve the current watermark. If ToSequence is at or below it, return a
//     no-op result (Advanced=false) having appended NOTHING.
//  2. Refuse a ToSequence above the repository's chain head with a
//     *BeyondChainHeadError naming the head.
//  3. Append the digest_marked_read chain entry through the injected Appender.
//  4. ONLY after that append succeeds, advance the watermark (a monotonic
//     upsert). An append error returns with the row untouched.
//
// Idempotence is a property of the WATERMARK, not of the chain. A retry after
// an append succeeded but the advance failed appends a SECOND entry — an
// honest record of the second attempt, since the chain is append-only — and
// converges the watermark to ToSequence. The chain does not deduplicate.

// MarkedReadEvent is what the Appender records on the chain. The surface
// wiring (backend/internal/server) maps it to a global-chain
// digest_marked_read entry partitioned by AccountID: the mark is repo- and
// captain-scoped and belongs to no run.
type MarkedReadEvent struct {
	AccountID      *uuid.UUID
	CaptainSubject string
	Repo           string
	// PreviousSequence is the watermark being replaced (0 with HadPrevious
	// false for a first mark).
	PreviousSequence int64
	HadPrevious      bool
	ToSequence       int64
}

// Payload renders the event's chain-entry payload.
func (e MarkedReadEvent) Payload() (json.RawMessage, error) {
	b, err := json.Marshal(map[string]any{
		"repo":              e.Repo,
		"captain_subject":   e.CaptainSubject,
		"previous_sequence": e.PreviousSequence,
		"had_previous":      e.HadPrevious,
		"to_sequence":       e.ToSequence,
	})
	if err != nil {
		return nil, fmt.Errorf("digest: marked-read payload: %w", err)
	}
	return b, nil
}

// Appender writes the digest_marked_read chain entry.
type Appender interface {
	AppendMarkedRead(ctx context.Context, e MarkedReadEvent) error
}

// AppenderFunc adapts a function to Appender.
type AppenderFunc func(ctx context.Context, e MarkedReadEvent) error

// AppendMarkedRead calls f.
func (f AppenderFunc) AppendMarkedRead(ctx context.Context, e MarkedReadEvent) error {
	return f(ctx, e)
}

// MarkReadParams selects the watermark to advance and its new position.
type MarkReadParams struct {
	AccountID      *uuid.UUID
	CaptainSubject string
	Repo           string
	ToSequence     int64
}

// MarkReadResult reports what MarkRead did.
type MarkReadResult struct {
	PreviousSequence int64 `json:"previous_sequence"`
	HadPrevious      bool  `json:"had_previous"`
	// Sequence is the watermark after the call.
	Sequence int64 `json:"sequence"`
	// Advanced is false for the at-or-below-watermark no-op (nothing was
	// appended).
	Advanced bool `json:"advanced"`
}

// MarkRead advances the captain's watermark to p.ToSequence, appending the
// digest_marked_read entry FIRST. See the ordering contract above.
func (s *Store) MarkRead(ctx context.Context, appender Appender, p MarkReadParams) (MarkReadResult, error) {
	if p.Repo == "" || p.CaptainSubject == "" {
		return MarkReadResult{}, fmt.Errorf("%w: repo and captain subject are required", ErrInvalidRequest)
	}
	if p.ToSequence <= 0 {
		return MarkReadResult{}, fmt.Errorf("%w: to_sequence must be positive", ErrInvalidRequest)
	}
	cur, has, err := s.GetWatermark(ctx, p.AccountID, p.CaptainSubject, p.Repo)
	if err != nil {
		return MarkReadResult{}, err
	}
	if has && p.ToSequence <= cur {
		return MarkReadResult{PreviousSequence: cur, HadPrevious: true, Sequence: cur, Advanced: false}, nil
	}
	head, err := s.ChainHead(ctx, p.Repo)
	if err != nil {
		return MarkReadResult{}, err
	}
	if p.ToSequence > head {
		return MarkReadResult{}, &BeyondChainHeadError{Requested: p.ToSequence, Head: head}
	}
	if err := appender.AppendMarkedRead(ctx, MarkedReadEvent{
		AccountID: p.AccountID, CaptainSubject: p.CaptainSubject, Repo: p.Repo,
		PreviousSequence: cur, HadPrevious: has, ToSequence: p.ToSequence,
	}); err != nil {
		return MarkReadResult{}, fmt.Errorf("digest: mark read: append digest_marked_read (watermark unmoved): %w", err)
	}
	if err := s.UpsertWatermark(ctx, p.AccountID, p.CaptainSubject, p.Repo, p.ToSequence); err != nil {
		return MarkReadResult{}, fmt.Errorf("digest: mark read: entry appended but watermark not advanced (a retry converges it): %w", err)
	}
	return MarkReadResult{PreviousSequence: cur, HadPrevious: has, Sequence: p.ToSequence, Advanced: true}, nil
}

// jsonObject renders m as a JSON object string for a ::jsonb bind.
func jsonObject(m map[string]string) (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("digest: encode pairing: %w", err)
	}
	return string(b), nil
}
