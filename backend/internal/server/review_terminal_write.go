package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
)

// Durable review terminal writes (E72.64 / #4106).
//
// A review round settles when its terminal audit entries (*_reviewed /
// *_review_skipped / *_review_failed) reach configured_agents. Those writes
// used to be a single best-effort AppendChained whose failure was a WARN, so a
// transient database failure turned a completed review into a round that
// stayed pending forever. appendReviewTerminal replaces that single attempt:
//
//   - Phase 1 retries the ORIGINAL row with bounded backoff. Every append and
//     every read-back probe runs under its own context.WithTimeout, so a hung
//     store counts as a failed attempt instead of wedging the caller.
//   - Each terminal carries a process-unique microsecond timestamp, and a
//     probe on (category, stage_id, timestamp) runs before every retry, so a
//     commit whose acknowledgement was lost is never appended twice.
//   - Exhaustion and parent-context cancellation share ONE terminal path. It
//     probes once more for the original; when that probe SUCCEEDS and finds
//     nothing it makes one bounded attempt at a NAMED failure (a
//     *_review_failed whose reason begins verdict_write_failed). When the
//     probe FAILS, or the named attempt fails, the round is recorded in a
//     process-local lost-terminal registry and a detached, bounded settle loop
//     appends the named failure once the store accepts writes. The named
//     failure only ever lands after a probe proved the original absent, so a
//     round never carries two terminals for one reviewer.
//
// Callers holding reconcileEmitLockFor(runID) use appendReviewTerminalSingleWith
// (one bounded append, one bounded probe, then the registry) so the stripe
// lock is held across at most two bounded operations per terminal.

// reviewVerdictWriteFailedPrefix starts the reason of every named failure the
// terminal path writes, so an operator (and the tests) can tell a lost-write
// failure from a reviewer failure.
const reviewVerdictWriteFailedPrefix = "verdict_write_failed"

// reviewTerminalPayloadLogCap bounds the payload JSON the ERROR log carries.
const reviewTerminalPayloadLogCap = 256 << 10

// reviewTerminalReasonCap bounds each free-text fragment folded into a named
// failure's reason (the last error, the lost original's reason).
const reviewTerminalReasonCap = 512

var (
	reviewTerminalTSMu   sync.Mutex
	reviewTerminalTSLast time.Time
)

// nextReviewTerminalTimestamp returns a UTC, microsecond-truncated timestamp
// strictly after every value it returned before in this process. The audit
// store truncates to microseconds (audit/chain.go), so the stored timestamp
// equals the returned one and (category, stage_id, timestamp) identifies the
// row a probe looks for.
func nextReviewTerminalTimestamp() time.Time {
	reviewTerminalTSMu.Lock()
	defer reviewTerminalTSMu.Unlock()
	ts := time.Now().UTC().Truncate(time.Microsecond)
	if !ts.After(reviewTerminalTSLast) {
		ts = reviewTerminalTSLast.Add(time.Microsecond)
	}
	reviewTerminalTSLast = ts
	return ts
}

// reviewTerminalWriteConfig is the retry schedule. Every internal variant takes
// one and snapshots it at entry, so a detached settle goroutine never reads
// package state a test could change.
type reviewTerminalWriteConfig struct {
	// backoff is the phase-1 wait before each retry of the original row.
	backoff []time.Duration
	// settle is the detached settle loop's wait before each flush.
	settle []time.Duration
	// opTimeout bounds every single append and probe.
	opTimeout time.Duration
}

// productionReviewTerminalWriteConfig rides out a database blip of minutes:
// 12 attempts over 330s in phase 1, then ~25 minutes of detached settle.
func productionReviewTerminalWriteConfig() reviewTerminalWriteConfig {
	settle := make([]time.Duration, 50)
	for i := range settle {
		settle[i] = 30 * time.Second
	}
	return reviewTerminalWriteConfig{
		backoff: []time.Duration{
			1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
			15 * time.Second, 30 * time.Second, 30 * time.Second,
			60 * time.Second, 60 * time.Second, 60 * time.Second, 60 * time.Second,
		},
		settle:    settle,
		opTimeout: 30 * time.Second,
	}
}

// defaultReviewTerminalWriteConfig is read-only after init. The package's
// tests replace it ONCE in an init() with millisecond steps.
var defaultReviewTerminalWriteConfig = productionReviewTerminalWriteConfig()

// boundedAppend is one AppendChained under its own per-operation timeout.
func (s *Server) boundedAppend(parent context.Context, p audit.ChainAppendParams, cfg reviewTerminalWriteConfig) (*audit.Entry, error) {
	ctx, cancel := context.WithTimeout(parent, cfg.opTimeout)
	defer cancel()
	return s.cfg.AuditRepo.AppendChained(ctx, p)
}

// probeReviewTerminal reads back the run's entries of one category under its
// own per-operation timeout and reports the row stamped (stageID, ts). A read
// failure, including a probe cut off by its timeout, returns err.
func (s *Server) probeReviewTerminal(parent context.Context, runID uuid.UUID, category string, stageID uuid.UUID, ts time.Time, cfg reviewTerminalWriteConfig) (*audit.Entry, bool, error) {
	ctx, cancel := context.WithTimeout(parent, cfg.opTimeout)
	defer cancel()
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, category)
	if err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID && e.Timestamp.Equal(ts) {
			return e, true, nil
		}
	}
	return nil, false, nil
}

// appendReviewTerminal durably appends one review-round terminal entry. It
// returns the stored entry on success (including a lost-ack commit a probe
// found). On failure it returns (nil, lastErr) after the terminal path has
// either landed a named verdict_write_failed or registered the round for the
// detached settle; the caller keeps its WARN and skips post-success work.
func (s *Server) appendReviewTerminal(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	return s.appendReviewTerminalWith(ctx, p, defaultReviewTerminalWriteConfig)
}

// appendReviewTerminalWith is appendReviewTerminal with an explicit schedule.
func (s *Server) appendReviewTerminalWith(ctx context.Context, p audit.ChainAppendParams, cfg reviewTerminalWriteConfig) (*audit.Entry, error) {
	p.Timestamp = nextReviewTerminalTimestamp()
	start := time.Now()
	attempts := 1
	entry, err := s.boundedAppend(ctx, p, cfg)
	if err == nil {
		return entry, nil
	}
	lastErr := err
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review terminal write failed — retrying",
		slog.String("run_id", p.RunID.String()),
		slog.String("category", p.Category),
		slog.String("error", err.Error()),
	)
phase1:
	for _, step := range cfg.backoff {
		timer := time.NewTimer(step)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			break phase1
		}
		attempts++
		found, ok, perr := s.probeReviewTerminal(ctx, p.RunID, p.Category, reviewTerminalStageID(p), p.Timestamp, cfg)
		if perr != nil {
			// The original may have committed; appending blind could
			// duplicate it. Count the attempt and wait for the next step.
			lastErr = perr
			s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug, "review terminal write: probe failed — skipping the retry append",
				slog.String("run_id", p.RunID.String()),
				slog.String("category", p.Category),
				slog.Int("attempt", attempts),
				slog.String("error", perr.Error()),
			)
			continue
		}
		if ok {
			return found, nil
		}
		entry, err := s.boundedAppend(ctx, p, cfg)
		if err == nil {
			return entry, nil
		}
		lastErr = err
		s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug, "review terminal write: retry failed",
			slog.String("run_id", p.RunID.String()),
			slog.String("category", p.Category),
			slog.Int("attempt", attempts),
			slog.String("error", err.Error()),
		)
	}
	return s.reviewTerminalWriteFailed(ctx, p, cfg, attempts, time.Since(start), lastErr, true)
}

// appendReviewTerminalSingleWith is the in-lock variant for callers holding
// reconcileEmitLockFor(runID): one bounded append, then one bounded probe for
// a lost-ack commit, then the registry. It never waits on a backoff step and
// never makes the immediate named attempt, so it holds the caller's lock for
// at most two operations of cfg.opTimeout each. Every operation derives from
// context.WithoutCancel(ctx), so a cancelled request still registers.
func (s *Server) appendReviewTerminalSingleWith(ctx context.Context, p audit.ChainAppendParams, cfg reviewTerminalWriteConfig) (*audit.Entry, error) {
	p.Timestamp = nextReviewTerminalTimestamp()
	start := time.Now()
	detached := context.WithoutCancel(ctx)
	entry, err := s.boundedAppend(detached, p, cfg)
	if err == nil {
		return entry, nil
	}
	return s.reviewTerminalWriteFailed(ctx, p, cfg, 1, time.Since(start), err, false)
}

// reviewTerminalWriteFailed is the terminal path shared by exhaustion and
// cancellation (and the single in-lock variant, with namedAttempt false). Every
// operation runs under context.WithoutCancel(ctx), each with its own timeout.
func (s *Server) reviewTerminalWriteFailed(ctx context.Context, p audit.ChainAppendParams, cfg reviewTerminalWriteConfig, attempts int, elapsed time.Duration, lastErr error, namedAttempt bool) (*audit.Entry, error) {
	detached := context.WithoutCancel(ctx)
	cancelled := ctx.Err() != nil
	stageID := reviewTerminalStageID(p)

	// (1) Final probe for the ORIGINAL row: the last attempt (or the one the
	// cancel interrupted) may have committed despite its error.
	found, ok, perr := s.probeReviewTerminal(detached, p.RunID, p.Category, stageID, p.Timestamp, cfg)
	if perr == nil && ok {
		return found, nil
	}

	// (2) The verdict is about to be lost from the audit trail: keep it in
	// the log verbatim.
	msg := "review terminal write exhausted its retry budget"
	if cancelled {
		msg = "review terminal write interrupted by cancellation"
	}
	logged, truncated := string(p.Payload), false
	if len(logged) > reviewTerminalPayloadLogCap {
		logged, truncated = logged[:reviewTerminalPayloadLogCap], true
	}
	attrs := []slog.Attr{
		slog.String("run_id", p.RunID.String()),
		slog.String("stage_id", stageID.String()),
		slog.String("category", p.Category),
		slog.Int("attempts", attempts),
		slog.Duration("elapsed", elapsed),
		slog.Bool("cancelled", cancelled),
		slog.String("error", errString(lastErr)),
		slog.String("payload", logged),
		slog.Bool("payload_truncated", truncated),
	}
	if perr != nil {
		attrs = append(attrs, slog.String("final_probe_error", perr.Error()))
	}
	s.cfg.Logger.LogAttrs(detached, slog.LevelError, msg, attrs...)

	// (3) The named failure that settles the reviewer's slot.
	named, stage, known := buildVerdictWriteFailedParams(p, attempts, elapsed, lastErr)
	if !known {
		return nil, lastErr
	}

	// (4) One immediate attempt, ONLY after a probe that succeeded and found
	// nothing: a failed probe cannot rule out a committed original, and the
	// flush re-probes before it appends.
	if namedAttempt && perr == nil {
		_, nerr := s.boundedAppend(detached, named, cfg)
		if nerr == nil {
			return nil, lastErr
		}
		s.cfg.Logger.LogAttrs(detached, slog.LevelWarn, "review terminal write: named verdict_write_failed append failed — handing off to the settle loop",
			slog.String("run_id", p.RunID.String()),
			slog.String("category", named.Category),
			slog.String("error", nerr.Error()),
		)
	}

	// (5) Register and settle in the background.
	s.registerLostReviewTerminal(detached, &lostReviewTerminal{
		stageID:           stageID,
		originalCategory:  p.Category,
		originalTimestamp: p.Timestamp,
		named:             named,
	}, p.RunID, stage, cfg)
	return nil, lastErr
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func reviewTerminalStageID(p audit.ChainAppendParams) uuid.UUID {
	if p.StageID == nil {
		return uuid.Nil
	}
	return *p.StageID
}

// reviewStageKindForTerminal maps a terminal category to its review stage.
func reviewStageKindForTerminal(category string) (orphanedReviewStageKind, bool) {
	for _, st := range orphanedReviewStages {
		for _, c := range st.terminals {
			if c == category {
				return st, true
			}
		}
	}
	return orphanedReviewStageKind{}, false
}

// reviewStageKindByLabel maps a stage label ("plan" / "implement") to its kind.
func reviewStageKindByLabel(label string) (orphanedReviewStageKind, bool) {
	for _, st := range orphanedReviewStages {
		if st.label == label {
			return st, true
		}
	}
	return orphanedReviewStageKind{}, false
}

func capReviewTerminalText(s string) string {
	if len(s) > reviewTerminalReasonCap {
		return s[:reviewTerminalReasonCap] + "…"
	}
	return s
}

// buildVerdictWriteFailedParams builds the named *_review_failed that stands in
// for a lost terminal. Its reason summarises the original: a lost verdict
// (*_reviewed) names its verdict and concern count; a lost skip or failure
// names its category and reason. known is false for a category outside the
// review terminal set.
func buildVerdictWriteFailedParams(original audit.ChainAppendParams, attempts int, elapsed time.Duration, lastErr error) (audit.ChainAppendParams, orphanedReviewStageKind, bool) {
	stage, ok := reviewStageKindForTerminal(original.Category)
	if !ok {
		return audit.ChainAppendParams{}, stage, false
	}
	var orig struct {
		ReviewerModel string                   `json:"reviewer_model"`
		Authority     planreview.AuthorityMode `json:"authority"`
		Verdict       string                   `json:"verdict"`
		Concerns      []json.RawMessage        `json:"concerns"`
		Reason        string                   `json:"reason"`
	}
	_ = json.Unmarshal(original.Payload, &orig)
	lead := fmt.Sprintf("%s: %s append failed after %d attempts over %s", reviewVerdictWriteFailedPrefix, original.Category, attempts, elapsed.Round(time.Millisecond))
	var reason string
	if original.Category == stage.terminals[0] {
		reason = fmt.Sprintf("%s (verdict %s, %d concerns; full payload in the fishhawkd ERROR log): %s",
			lead, orig.Verdict, len(orig.Concerns), capReviewTerminalText(errString(lastErr)))
	} else {
		reason = fmt.Sprintf("%s (original %s reason: %s; full payload in the fishhawkd ERROR log): %s",
			lead, original.Category, capReviewTerminalText(orig.Reason), capReviewTerminalText(errString(lastErr)))
	}
	payload, _ := json.Marshal(planreview.ReviewFailedPayload{
		Reason:        reason,
		ReviewerModel: orig.ReviewerModel,
		Authority:     orig.Authority,
	})
	systemKind := audit.ActorKind("system")
	return audit.ChainAppendParams{
		RunID:     original.RunID,
		StageID:   original.StageID,
		Timestamp: nextReviewTerminalTimestamp(),
		Category:  stage.failed,
		ActorKind: &systemKind,
		Payload:   payload,
	}, stage, true
}

// lostReviewTerminalKey groups lost terminals by run and stage label.
type lostReviewTerminalKey struct {
	runID uuid.UUID
	stage string
}

// lostReviewTerminal is one reviewer slot whose terminal never landed: the
// original's identity (for the lost-ack probe) and the named failure to land.
type lostReviewTerminal struct {
	id                uint64
	stageID           uuid.UUID
	originalCategory  string
	originalTimestamp time.Time
	named             audit.ChainAppendParams
}

// lostReviewTerminals is the process-local registry: a SET of entries per
// (run, stage label), each with its own identity, so two lost reviewers of one
// round settle independently (the pendingRedispatches precedent).
var (
	lostReviewTerminalMu  sync.Mutex
	lostReviewTerminals   = map[lostReviewTerminalKey]map[uint64]*lostReviewTerminal{}
	lostReviewTerminalSeq uint64
)

// registerLostReviewTerminal records the entry and starts its detached settle,
// tracked by bgReviews.
func (s *Server) registerLostReviewTerminal(detached context.Context, e *lostReviewTerminal, runID uuid.UUID, stage orphanedReviewStageKind, cfg reviewTerminalWriteConfig) {
	key := lostReviewTerminalKey{runID: runID, stage: stage.label}
	lostReviewTerminalMu.Lock()
	lostReviewTerminalSeq++
	e.id = lostReviewTerminalSeq
	set := lostReviewTerminals[key]
	if set == nil {
		set = map[uint64]*lostReviewTerminal{}
		lostReviewTerminals[key] = set
	}
	set[e.id] = e
	lostReviewTerminalMu.Unlock()
	s.bgReviews.Add(1)
	go s.settleLostReviewTerminal(detached, key, e.id, cfg)
}

// lostReviewTerminalRegistered reports whether entry id is still registered.
func lostReviewTerminalRegistered(key lostReviewTerminalKey, id uint64) bool {
	lostReviewTerminalMu.Lock()
	defer lostReviewTerminalMu.Unlock()
	_, ok := lostReviewTerminals[key][id]
	return ok
}

// lostReviewTerminalsFor returns a snapshot of the key's entries in
// registration order.
func lostReviewTerminalsFor(key lostReviewTerminalKey) []*lostReviewTerminal {
	lostReviewTerminalMu.Lock()
	defer lostReviewTerminalMu.Unlock()
	out := make([]*lostReviewTerminal, 0, len(lostReviewTerminals[key]))
	for _, e := range lostReviewTerminals[key] {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// hasLostReviewTerminals reports whether this process holds any lost terminal
// for the run's stage.
func hasLostReviewTerminals(runID uuid.UUID, label string) bool {
	lostReviewTerminalMu.Lock()
	defer lostReviewTerminalMu.Unlock()
	return len(lostReviewTerminals[lostReviewTerminalKey{runID: runID, stage: label}]) > 0
}

func removeLostReviewTerminal(key lostReviewTerminalKey, id uint64) {
	lostReviewTerminalMu.Lock()
	defer lostReviewTerminalMu.Unlock()
	delete(lostReviewTerminals[key], id)
	if len(lostReviewTerminals[key]) == 0 {
		delete(lostReviewTerminals, key)
	}
}

// flushLostReviewTerminalsLocked lands the registered named failures for one
// run's stage. The caller MUST hold reconcileEmitLockFor(runID) and passes the
// stage's current round (latest == nil when no round was ever started) and its
// landed-terminal count. Every store operation is a single bounded operation
// and the flush STOPS at the first failure, leaving that entry registered.
//
// Per entry: a STALE entry (another stage id, or an original older than the
// latest *_review_started) is dropped; an entry whose original or named
// failure is found by probe is dropped as confirmed landed; an entry with no
// room left in the round (ConfiguredAgents - landed - appended <= 0) is
// dropped; otherwise its named failure is appended.
//
// appended counts the named failures this call wrote. confirmed is true when
// ANY entry was removed because its round's terminal is now confirmed landed
// (its own append, or a probe that found the original or the named failure):
// an implement-stage caller republishes fishhawk_audit_complete on confirmed,
// OUTSIDE the lock, not only when appended > 0.
func (s *Server) flushLostReviewTerminalsLocked(ctx context.Context, runID uuid.UUID, stage orphanedReviewStageKind, latest *audit.Entry, payload planreview.ReviewStartedPayload, landed int, cfg reviewTerminalWriteConfig) (appended int, confirmed bool) {
	key := lostReviewTerminalKey{runID: runID, stage: stage.label}
	for _, e := range lostReviewTerminalsFor(key) {
		if latest != nil && (latest.StageID == nil || *latest.StageID != e.stageID || e.originalTimestamp.Before(latest.Timestamp)) {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review terminal flush: lost terminal belongs to a superseded round — dropped",
				slog.String("run_id", runID.String()),
				slog.String("stage", stage.label),
				slog.String("stage_id", e.stageID.String()),
				slog.String("category", e.originalCategory),
			)
			removeLostReviewTerminal(key, e.id)
			continue
		}
		_, found, err := s.probeReviewTerminal(ctx, runID, e.originalCategory, e.stageID, e.originalTimestamp, cfg)
		if err != nil {
			return appended, confirmed
		}
		if !found {
			_, found, err = s.probeReviewTerminal(ctx, runID, e.named.Category, e.stageID, e.named.Timestamp, cfg)
			if err != nil {
				return appended, confirmed
			}
		}
		if found {
			removeLostReviewTerminal(key, e.id)
			confirmed = true
			continue
		}
		if latest != nil && payload.ConfiguredAgents-landed-appended <= 0 {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "review terminal flush: round already settled — lost terminal dropped",
				slog.String("run_id", runID.String()),
				slog.String("stage", stage.label),
				slog.Int("configured_agents", payload.ConfiguredAgents),
				slog.Int("landed", landed+appended),
			)
			removeLostReviewTerminal(key, e.id)
			continue
		}
		if _, err := s.boundedAppend(ctx, e.named, cfg); err != nil {
			return appended, confirmed
		}
		removeLostReviewTerminal(key, e.id)
		appended++
		confirmed = true
	}
	return appended, confirmed
}

// flushLostReviewTerminals takes the run's stripe lock, reads the stage's
// current round under per-operation timeouts and delegates to
// flushLostReviewTerminalsLocked. A read failure returns without flushing.
func (s *Server) flushLostReviewTerminals(ctx context.Context, runID uuid.UUID, label string, cfg reviewTerminalWriteConfig) (appended int, confirmed bool, isImplement bool) {
	stage, ok := reviewStageKindByLabel(label)
	if !ok || !hasLostReviewTerminals(runID, label) {
		return 0, false, stage.isImplement
	}
	lock := reconcileEmitLockFor(runID)
	lock.Lock()
	defer lock.Unlock()

	rctx, cancel := context.WithTimeout(ctx, cfg.opTimeout)
	latest, payload, found, err := s.latestReviewStarted(rctx, runID, stage)
	cancel()
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug, "review terminal flush: read current round failed",
			slog.String("run_id", runID.String()),
			slog.String("stage", label),
			slog.String("error", err.Error()),
		)
		return 0, false, stage.isImplement
	}
	landed := 0
	if found {
		cctx, ccancel := context.WithTimeout(ctx, cfg.opTimeout)
		landed, err = s.countLandedReviewTerminals(cctx, runID, stage, latest.Sequence)
		ccancel()
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug, "review terminal flush: count landed terminals failed",
				slog.String("run_id", runID.String()),
				slog.String("stage", label),
				slog.String("error", err.Error()),
			)
			return 0, false, stage.isImplement
		}
	} else {
		latest = nil
	}
	appended, confirmed = s.flushLostReviewTerminalsLocked(ctx, runID, stage, latest, payload, landed, cfg)
	return appended, confirmed, stage.isImplement
}

// settleLostReviewTerminal is the detached settle loop for one registered
// entry. It flushes the entry's (run, stage) at each settle step and returns
// as soon as the entry is no longer registered. Server has no process-level
// stop signal, so the wait selects only on its timer; Shutdown's bounded
// bgReviews drain caps how long a stop waits for it. When the schedule ends
// with the entry still registered it logs an ERROR naming the recovery verb
// and leaves the entry for fishhawk_reconcile_reviews.
func (s *Server) settleLostReviewTerminal(detached context.Context, key lostReviewTerminalKey, id uint64, cfg reviewTerminalWriteConfig) {
	defer s.bgReviews.Done()
	for _, step := range cfg.settle {
		timer := time.NewTimer(step)
		<-timer.C
		if !lostReviewTerminalRegistered(key, id) {
			return
		}
		_, confirmed, isImplement := s.flushLostReviewTerminals(detached, key.runID, key.stage, cfg)
		if confirmed && isImplement {
			s.republishAuditCompleteAfterLostTerminal(detached, key.runID)
		}
		if !lostReviewTerminalRegistered(key, id) {
			return
		}
	}
	s.cfg.Logger.LogAttrs(detached, slog.LevelError,
		"review terminal settle exhausted — the round stays pending; run fishhawk_reconcile_reviews once the audit store accepts writes",
		slog.String("run_id", key.runID.String()),
		slog.String("stage", key.stage),
	)
}

// republishAuditCompleteAfterLostTerminal re-derives fishhawk_audit_complete
// once an implement round's lost terminal is confirmed landed, through the
// same seam the reconcile paths use (production wires nil).
func (s *Server) republishAuditCompleteAfterLostTerminal(ctx context.Context, runID uuid.UUID) {
	if s.reconcileRecomputeAuditComplete != nil {
		s.reconcileRecomputeAuditComplete(ctx, runID)
		return
	}
	s.recomputeAndPublishAuditComplete(ctx, runID)
}
