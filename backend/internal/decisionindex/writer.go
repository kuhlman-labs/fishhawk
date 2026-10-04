package decisionindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// IndexTimeout bounds one best-effort index write. The write runs on a context
// DETACHED from the caller's cancellation (context.WithoutCancel), so a client
// that disconnects after its decision committed does not also lose the index
// row, but it can never hold the decision's request longer than this.
const IndexTimeout = 5 * time.Second

// FullRepository is the audit repository surface the decorator requires of the
// repository it wraps: audit.Repository plus EVERY optional capability the
// concrete Postgres repository carries. The capabilities are not tidiness —
// server/*.go reaches them by type-asserting its audit.Repository, and two of
// the nine decision classes reach the chain ONLY through them
// (acceptance_triage_arbitrated via AnchoredChainAppender,
// grooming_disposition_recorded via GroomingWindowAppender). A decorator that
// dropped one would silently disable a server feature (the assertion just
// returns ok=false) and silently stop indexing a decision class.
//
// capabilities_test.go scans package audit's source for every exported
// *Appender interface and fails when one is not satisfied by the decorator, so
// a capability added later cannot be forgotten here.
type FullRepository interface {
	audit.Repository
	audit.AnchoredChainAppender
	audit.DedupedChainAppender
	audit.GroomingWindowAppender
	audit.RetryBudgetAppender
	audit.UpkeepWindowAppender
}

// ErrMissingCapability is returned by NewIndexingRepository when the wrapped
// repository lacks an optional capability the decorator must forward.
var ErrMissingCapability = errors.New("decisionindex: wrapped audit repository lacks a required capability")

// IndexingRepository decorates an audit repository so every SUCCESSFUL append
// of a decision-bearing entry is projected into decision_index. Every audit
// method is delegated unchanged; the index write happens after the append has
// returned, and every index failure (resolve, extract, upsert) is logged at
// WARN and SWALLOWED — the decision's entry and nil error are returned exactly
// as the inner repository produced them. An index write can never fail a
// decision (ADR-082 rule 1); a missed row is a gap the `fishhawkd
// decision-index check` reports and `backfill` closes.
type IndexingRepository struct {
	FullRepository
	store    *Store
	resolver ContextResolver
	logger   *slog.Logger
}

// Compile-time proof the decorator itself carries every capability it claims.
var _ FullRepository = (*IndexingRepository)(nil)

// NewIndexingRepository wraps inner. It FAILS CLOSED when inner does not carry
// every capability FullRepository names: returning a decorator without one
// would drop a server feature and a decision class silently, and claiming one
// the inner cannot honour would defeat the server's non-atomic fallback. A nil
// logger logs to slog.Default().
func NewIndexingRepository(inner audit.Repository, store *Store, resolver ContextResolver, logger *slog.Logger) (*IndexingRepository, error) {
	full, ok := inner.(FullRepository)
	if !ok {
		return nil, fmt.Errorf("%w: %T must implement audit.AnchoredChainAppender, audit.DedupedChainAppender, audit.GroomingWindowAppender, audit.RetryBudgetAppender and audit.UpkeepWindowAppender", ErrMissingCapability, inner)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &IndexingRepository{FullRepository: full, store: store, resolver: resolver, logger: logger}, nil
}

// Append delegates, then indexes the entry best-effort.
func (r *IndexingRepository) Append(ctx context.Context, p audit.AppendParams) (*audit.Entry, error) {
	e, err := r.FullRepository.Append(ctx, p)
	if err == nil {
		r.index(ctx, e)
	}
	return e, err
}

// AppendChained delegates, then indexes the entry best-effort.
func (r *IndexingRepository) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	e, err := r.FullRepository.AppendChained(ctx, p)
	if err == nil {
		r.index(ctx, e)
	}
	return e, err
}

// AppendGlobalChained delegates, then indexes the entry best-effort. No
// decision-bearing category is written run-less today; one that ever is lands
// here, is refused by Extract (ErrNoRun / ErrRunMissing) and is LOGGED, never
// silently dropped.
func (r *IndexingRepository) AppendGlobalChained(ctx context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error) {
	e, err := r.FullRepository.AppendGlobalChained(ctx, p)
	if err == nil {
		r.index(ctx, e)
	}
	return e, err
}

// AppendChainedAnchored forwards the AnchoredChainAppender capability — the
// ONLY path acceptance_triage_arbitrated reaches the chain — then indexes the
// entry. A duplicate or moved-anchor error wrote nothing and is returned as-is.
func (r *IndexingRepository) AppendChainedAnchored(ctx context.Context, p audit.ChainAppendParams, spec audit.AnchorSpec) (*audit.Entry, error) {
	e, err := r.FullRepository.AppendChainedAnchored(ctx, p, spec)
	if err == nil {
		r.index(ctx, e)
	}
	return e, err
}

// AppendChainedDeduped forwards the DedupedChainAppender capability, then
// indexes the entry. A duplicate error wrote nothing (the surviving entry was
// indexed when IT was appended) and is returned as-is.
func (r *IndexingRepository) AppendChainedDeduped(ctx context.Context, p audit.ChainAppendParams, spec audit.DedupeSpec) (*audit.Entry, error) {
	e, err := r.FullRepository.AppendChainedDeduped(ctx, p, spec)
	if err == nil {
		r.index(ctx, e)
	}
	return e, err
}

// AppendChainedUnderBudget forwards the RetryBudgetAppender capability, then
// indexes the entry.
func (r *IndexingRepository) AppendChainedUnderBudget(ctx context.Context, p audit.ChainAppendParams, maxEntries int, stamp func(attempt int) (json.RawMessage, error)) (*audit.Entry, error) {
	e, err := r.FullRepository.AppendChainedUnderBudget(ctx, p, maxEntries, stamp)
	if err == nil {
		r.index(ctx, e)
	}
	return e, err
}

// AppendChainedGroomingDispositionBatch forwards the GroomingWindowAppender
// capability — the ONLY path grooming_disposition_recorded reaches the chain —
// then indexes every appended entry.
func (r *IndexingRepository) AppendChainedGroomingDispositionBatch(ctx context.Context, artifactID string, ps []audit.ChainAppendParams) ([]*audit.Entry, error) {
	es, err := r.FullRepository.AppendChainedGroomingDispositionBatch(ctx, artifactID, ps)
	if err == nil {
		for _, e := range es {
			r.index(ctx, e)
		}
	}
	return es, err
}

// AppendChainedGroomingWindowClose forwards the GroomingWindowAppender
// settlement. Only the watermark is (possibly) new; the consumed dispositions
// were indexed when they were appended, so they are not re-indexed.
func (r *IndexingRepository) AppendChainedGroomingWindowClose(ctx context.Context, p audit.ChainAppendParams, artifactID string) (*audit.Entry, []*audit.Entry, error) {
	w, consumed, err := r.FullRepository.AppendChainedGroomingWindowClose(ctx, p, artifactID)
	if err == nil {
		r.index(ctx, w)
	}
	return w, consumed, err
}

// AppendChainedUpkeepDispositionBatch forwards the UpkeepWindowAppender
// capability (#3923) — the ONLY atomic path upkeep_disposition_recorded
// reaches the chain — then indexes every appended entry (a no-op while the
// category is not decision-bearing; forwarded through index so classifying it
// later needs no change here).
func (r *IndexingRepository) AppendChainedUpkeepDispositionBatch(ctx context.Context, artifactID string, ps []audit.ChainAppendParams) ([]*audit.Entry, error) {
	es, err := r.FullRepository.AppendChainedUpkeepDispositionBatch(ctx, artifactID, ps)
	if err == nil {
		for _, e := range es {
			r.index(ctx, e)
		}
	}
	return es, err
}

// AppendChainedUpkeepWindowClose forwards the UpkeepWindowAppender settlement.
// As for grooming, only the watermark is (possibly) new.
func (r *IndexingRepository) AppendChainedUpkeepWindowClose(ctx context.Context, p audit.ChainAppendParams, artifactID string) (*audit.Entry, []*audit.Entry, error) {
	w, consumed, err := r.FullRepository.AppendChainedUpkeepWindowClose(ctx, p, artifactID)
	if err == nil {
		r.index(ctx, w)
	}
	return w, consumed, err
}

// index projects one appended entry. It returns nothing on purpose: every
// failure is logged and swallowed so the caller's decision is untouched.
func (r *IndexingRepository) index(ctx context.Context, e *audit.Entry) {
	if e == nil || !IsDecisionBearing(e.Category) {
		return
	}
	ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), IndexTimeout)
	defer cancel()
	if err := r.indexEntry(ictx, e); err != nil {
		r.logger.Warn("decision index write failed; the decision stands and the gap is closed by `fishhawkd decision-index backfill`",
			slog.Int64("sequence", e.Sequence),
			slog.String("category", e.Category),
			slog.String("error", err.Error()))
	}
}

func (r *IndexingRepository) indexEntry(ctx context.Context, e *audit.Entry) error {
	rc, err := r.resolver.Resolve(ctx, e)
	if err != nil {
		return fmt.Errorf("resolve context: %w", err)
	}
	row, err := Extract(e, rc)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	if err := r.store.Upsert(ctx, *row); err != nil {
		return fmt.Errorf("upsert: %w", err)
	}
	return nil
}
