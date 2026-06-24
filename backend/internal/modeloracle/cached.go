package modeloracle

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// providerSnapshot is the per-provider cached state guarded by Cached.mu.
//
//   - models      — the most recent SUCCESSFULLY fetched id set.
//   - lastSuccess — when that set was fetched; drives freshness.
//   - lastAttempt — when a fetch was last tried (success OR failure); freshness
//     decays from lastSuccess, not lastAttempt, so a failing refresh ages the
//     snapshot toward stale rather than masking the outage.
//   - ok          — a successful fetch has landed at least once. False until the
//     first success → Snapshot reports no data → caller fails open.
type providerSnapshot struct {
	models      []string
	lastSuccess time.Time
	lastAttempt time.Time
	ok          bool
}

// Cached is the live, in-memory, per-process ModelOracle (#1341): it holds a
// per-provider snapshot of GET /v1/models, refreshed by a background goroutine
// (Run), and answers the ModelOracle.Snapshot seam the #1339 validation queries.
//
// Fail-open is the invariant at every edge: a provider with no registered
// Fetcher, or one whose first fetch has not yet succeeded, reports ok=false; a
// snapshot older than the staleness threshold reports fresh=false. In both cases
// the validation layer accepts the model with a warning rather than rejecting —
// the oracle can never turn a fetch outage into a false rejection or a boot
// blocker.
type Cached struct {
	providers map[string]Fetcher
	threshold time.Duration
	now       func() time.Time
	logger    *slog.Logger

	mu        sync.RWMutex
	snapshots map[string]providerSnapshot
}

// CachedOption customizes a Cached at construction. Today only WithClock; kept
// as an option so the constructor signature in serve.go stays stable.
type CachedOption func(*Cached)

// WithClock injects the time source (tests advance it across the staleness
// threshold to exercise freshness decay deterministically). Defaults to
// time.Now().UTC.
func WithClock(now func() time.Time) CachedOption {
	return func(c *Cached) { c.now = now }
}

// NewCached builds a Cached over the given provider→Fetcher map with the given
// staleness threshold. A nil logger defaults to slog.Default(); the clock
// defaults to time.Now().UTC unless WithClock overrides it. The returned oracle
// has NO snapshot until Refresh/Run lands a first success, so Snapshot reports
// ok=false (fail-open) until then.
func NewCached(providers map[string]Fetcher, threshold time.Duration, logger *slog.Logger, opts ...CachedOption) *Cached {
	if logger == nil {
		logger = slog.Default()
	}
	c := &Cached{
		providers: providers,
		threshold: threshold,
		logger:    logger,
		snapshots: make(map[string]providerSnapshot),
	}
	for _, o := range opts {
		o(c)
	}
	if c.now == nil {
		c.now = func() time.Time { return time.Now().UTC() }
	}
	return c
}

// Snapshot implements ModelOracle. It returns (nil, false, false) for a provider
// that has never had a successful fetch (unregistered or pre-first-success) —
// fail-open. Otherwise it returns a copy of the cached id set with fresh =
// (age < threshold) and ok = true.
func (c *Cached) Snapshot(_ context.Context, provider string) (models []string, fresh bool, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	snap, present := c.snapshots[provider]
	if !present || !snap.ok {
		return nil, false, false
	}
	fresh = c.now().Sub(snap.lastSuccess) < c.threshold
	out := append([]string(nil), snap.models...)
	return out, fresh, true
}

// Refresh fetches every registered provider once, updating its snapshot. A
// provider whose fetch errors keeps its prior models and bumps lastAttempt only
// (NOT lastSuccess), so freshness decays toward stale during an outage. A
// successful fetch replaces the models, stamps lastSuccess, and logs any
// membership change (a disappeared id is a high-signal warn — specs naming it
// will now hard-reject).
func (c *Cached) Refresh(ctx context.Context) {
	for name, fetcher := range c.providers {
		ids, err := fetcher.Fetch(ctx)
		now := c.now()

		c.mu.Lock()
		snap := c.snapshots[name]
		snap.lastAttempt = now
		if err != nil {
			c.logger.Warn("model snapshot refresh failed; keeping prior snapshot",
				slog.String("provider", name),
				slog.Bool("had_prior", snap.ok),
				slog.String("error", err.Error()))
			c.snapshots[name] = snap
			c.mu.Unlock()
			continue
		}
		priorOK, prior := snap.ok, snap.models
		snap.models = ids
		snap.lastSuccess = now
		snap.ok = true
		c.snapshots[name] = snap
		c.mu.Unlock()

		c.logChanges(name, priorOK, prior, ids)
	}
}

// Run fires one Refresh immediately (best-effort startup fetch — a failure
// leaves the oracle ok=false/fail-open, never blocking boot) then refreshes on
// the ticker until ctx is cancelled. Mirrors runWebhookEvictor: a single ticker,
// select on ctx.Done()/ticker.C, with each refresh bounded by its own timeout so
// one hung fetch cannot wedge the loop.
func (c *Cached) Run(ctx context.Context, interval time.Duration) {
	c.refreshBounded(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refreshBounded(ctx)
		}
	}
}

// refreshTimeout bounds a single full Refresh pass (across all providers).
// Generous relative to a Fetcher's own per-request timeout but finite so a
// pathological vendor can't hold the goroutine past one cycle.
const refreshTimeout = 2 * time.Minute

func (c *Cached) refreshBounded(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	c.Refresh(rctx)
}

// logChanges diffs the new id set against the prior one and logs membership
// drift. The first successful fetch (no prior) logs an info populate line.
// Subsequent fetches info-log added/removed ids when the set changed, plus a
// high-signal warn naming any id that DISAPPEARED — a previously-valid model the
// validation layer will now hard-reject.
func (c *Cached) logChanges(name string, priorOK bool, prior, current []string) {
	if !priorOK {
		c.logger.Info("model snapshot populated",
			slog.String("provider", name),
			slog.Int("count", len(current)))
		return
	}
	added := setDiff(current, prior)
	removed := setDiff(prior, current)
	if len(added) == 0 && len(removed) == 0 {
		return
	}
	c.logger.Info("model snapshot changed",
		slog.String("provider", name),
		slog.Any("added", added),
		slog.Any("removed", removed))
	if len(removed) > 0 {
		c.logger.Warn("model id(s) disappeared from provider snapshot; specs naming them will now hard-reject",
			slog.String("provider", name),
			slog.Any("removed", removed))
	}
}

// setDiff returns the sorted elements present in a but not in b.
func setDiff(a, b []string) []string {
	inB := make(map[string]struct{}, len(b))
	for _, x := range b {
		inB[x] = struct{}{}
	}
	var out []string
	for _, x := range a {
		if _, ok := inB[x]; !ok {
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}
