// Package mergeoutcome records the post-merge OUTCOME facts of ADR-085
// (#3777) rules 3 and 6 for a merged run (E82.2 / #3779): whether the run's
// merge was later reverted on the default branch (run_merge_reverted) and
// what the merge commit's CI concluded once it matured
// (run_merge_ci_observed).
//
// Both categories are chained, system-actor audit entries built ONLY from
// forge-attested facts — SHAs, PR numbers and URLs, file-level diff counts,
// check names and conclusions, forge timestamps. No commit message, PR body
// or other prose is ever recorded: a commit message is at most a POINTER to a
// candidate run, which is then confirmed against forge state.
package mergeoutcome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// Audit categories this package writes. Both are registered in
// backend/internal/audit/categories.go.
const (
	// CategoryRunMergeReverted is one row per (run, reverting commit): a
	// default-branch push carried a revert signal that resolved to the run's
	// forge-confirmed merge.
	CategoryRunMergeReverted = "run_merge_reverted"
	// CategoryRunMergeCIObserved is one row per (run, merge commit): the merge
	// commit's CI conclusion, fixed at maturity.
	CategoryRunMergeCIObserved = "run_merge_ci_observed"
)

// ActorSubject is the system actor every row this package writes carries.
const ActorSubject = "merge-outcome-observer"

// AuditStore is the slice of audit.Repository the recorders need. The
// production Postgres repository additionally implements
// audit.DedupedChainAppender, which appendDeduped prefers.
type AuditStore interface {
	AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error)
	ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error)
}

// appendDeduped appends p unless an entry of p.Category carrying the same
// spec.PayloadKey=spec.PayloadValue already exists on the run's chain.
//
// Capability path: a store implementing audit.DedupedChainAppender runs the
// scan and the append in ONE transaction under the run-row lock, so two
// concurrent observations of the same fact commit exactly one row. A
// *audit.DedupedDuplicateError is the already-recorded no-op: (false, nil).
//
// Fallback path (the in-memory fakes): list the category, scan, then
// AppendChained — NOT atomic. A list error fails closed (no append) so the
// caller retries on its next observation rather than risking a duplicate.
//
// Returns (true, nil) when a row landed.
func appendDeduped(ctx context.Context, store AuditStore, p audit.ChainAppendParams, spec audit.DedupeSpec) (bool, error) {
	if d, ok := store.(audit.DedupedChainAppender); ok {
		_, err := d.AppendChainedDeduped(ctx, p, spec)
		var dup *audit.DedupedDuplicateError
		if errors.As(err, &dup) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, nil
	}
	entries, err := store.ListForRunByCategory(ctx, p.RunID, p.Category)
	if err != nil {
		return false, fmt.Errorf("mergeoutcome: list %s for dedup: %w", p.Category, err)
	}
	if dedupMatch(entries, spec) {
		return false, nil
	}
	if _, err := store.AppendChained(ctx, p); err != nil {
		return false, err
	}
	return true, nil
}

// dedupMatch is the fallback leg's scan, mirroring the key
// audit.AppendChainedDedupedTx enforces: same stage scope (when spec names
// one) and a STRING payload value equal to spec.PayloadValue.
func dedupMatch(entries []*audit.Entry, spec audit.DedupeSpec) bool {
	for _, e := range entries {
		if spec.StageID != nil && (e.StageID == nil || *e.StageID != *spec.StageID) {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		if v, ok := payload[spec.PayloadKey].(string); ok && v == spec.PayloadValue {
			return true
		}
	}
	return false
}

// systemChainParams builds the chained, system-actor append for one fact row.
func systemChainParams(runID uuid.UUID, category string, at time.Time, payload json.RawMessage) audit.ChainAppendParams {
	kind := audit.ActorSystem
	subject := ActorSubject
	return audit.ChainAppendParams{
		RunID:        runID,
		Timestamp:    at.UTC(),
		Category:     category,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	}
}

// forgeAttempts bounds every forge call: one try plus two retries.
const forgeAttempts = 3

// defaultRetryBackoff is the first retry's wait; each later retry doubles it.
const defaultRetryBackoff = 500 * time.Millisecond

// githubStatus5xx matches the status-bearing error githubclient's
// classifyStatus produces for a status outside its typed arms
// ("githubclient: <op>: <code>: <body>"). Only a 5xx there is transient; a
// 409/429/400 lands on the same arm and is NOT retried.
var githubStatus5xx = regexp.MustCompile(`githubclient: [^:]*: 5[0-9]{2}: `)

// isTransient reports whether a forge error is worth retrying: a network
// error or a 5xx. Everything else — a typed forge refusal (not found,
// forbidden, validation, not installed), any other status, a decode failure —
// and any error raised after the caller's own context ended is terminal.
func isTransient(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	return githubStatus5xx.MatchString(err.Error())
}

// withRetry runs fn up to forgeAttempts times, retrying only a transient
// error, with exponential backoff starting at base. It returns the last
// result and error.
func withRetry[T any](ctx context.Context, base time.Duration, fn func(context.Context) (T, error)) (T, error) {
	if base <= 0 {
		base = defaultRetryBackoff
	}
	var (
		out T
		err error
	)
	wait := base
	for attempt := 1; attempt <= forgeAttempts; attempt++ {
		out, err = fn(ctx)
		if err == nil || !isTransient(ctx, err) || attempt == forgeAttempts {
			return out, err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, err
		case <-timer.C:
		}
		wait *= 2
	}
	return out, err
}
