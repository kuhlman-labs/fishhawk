package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mergeoutcome"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// pushObserveTimeout bounds one detached push observation (operator
// condition 2 of #3779): the observer outlives the webhook request but never
// runs unbounded.
const pushObserveTimeout = 60 * time.Second

// maxConcurrentPushObservations bounds the detached push observations in
// flight at once; a delivery arriving while every slot is held is skipped with
// a WARN rather than queued, so the webhook is never blocked.
const maxConcurrentPushObservations = 4

// pushObservers tracks every in-flight detached push observation so tests can
// wait for the background work to settle before reading the audit chain.
var pushObservers sync.WaitGroup

// pushObserveSlots is the process-wide semaphore for detached push
// observations.
var pushObserveSlots = make(chan struct{}, maxConcurrentPushObservations)

// detachedPushContext derives the push observer's context: detached from the
// webhook request's cancellation (context.WithoutCancel keeps its values) and
// bounded by its own pushObserveTimeout.
func detachedPushContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), pushObserveTimeout)
}

// observeDefaultBranchPush routes a GitHub `push` delivery to the
// default-branch revert observer (E82.2 / #3779). It is best-effort: it never
// influences the 202, and the observation itself runs DETACHED on its own
// goroutine so a slow forge never delays the delivery; ObservePush itself
// skips a non-default-branch push with zero forge calls. A same-GUID redelivery
// never reaches it — handleWebhook's delivery-dedup Mark rejects the GUID
// before any routing (see the README's push-route section).
//
// The forge client is checked as a CONCRETE nil before it is wrapped in the
// mergeoutcome.RevertForge interface: a nil *githubclient.Client stored in an
// interface would compare non-nil and panic on first use.
func (s *Server) observeDefaultBranchPush(ctx context.Context, ev webhook.Event) {
	if s.cfg.GitHub == nil || s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return
	}
	push, err := mergeoutcome.ParsePush(ev.RawBody)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "merge outcome: push payload undecodable; skipped",
			slog.String("delivery_id", ev.DeliveryID), slog.String("error", err.Error()))
		return
	}
	obs := &mergeoutcome.RevertObserver{
		Forge:  s.cfg.GitHub,
		Runs:   s.cfg.RunRepo,
		Audit:  s.cfg.AuditRepo,
		Logger: s.cfg.Logger,
	}
	runDetachedPushObservation(ctx, s.cfg.Logger, pushObserveSlots, ev.DeliveryID, func(detached context.Context) {
		sum := obs.ObservePush(detached, push, ev.DeliveryID)
		if sum.Signals > 0 {
			s.cfg.Logger.LogAttrs(detached, slog.LevelInfo, "merge outcome: push observed",
				slog.String("delivery_id", ev.DeliveryID), slog.String("repo", push.FullName),
				slog.Int("signals", sum.Signals), slog.Int("resolved", sum.Resolved),
				slog.Bool("truncated", sum.Truncated), slog.Int("recorded", sum.Recorded),
				slog.Int("errors", sum.Errors))
		}
	})
}

// runDetachedPushObservation runs observe on its own goroutine under
// detachedPushContext, holding one of slots for its lifetime. It never blocks:
// with every slot held it logs a WARN, skips the observation and returns
// false. A panic in observe is recovered, logged and the observation dropped —
// the goroutine sits outside net/http's per-request recovery, so an unrecovered
// panic would take fishhawkd down.
func runDetachedPushObservation(ctx context.Context, logger *slog.Logger, slots chan struct{}, deliveryID string, observe func(context.Context)) bool {
	select {
	case slots <- struct{}{}:
	default:
		logger.LogAttrs(ctx, slog.LevelWarn, "merge outcome: push observation skipped; concurrent observations saturated",
			slog.String("delivery_id", deliveryID), slog.Int("max_concurrent", cap(slots)))
		return false
	}
	detached, cancel := detachedPushContext(ctx)
	pushObservers.Add(1)
	go func() {
		defer pushObservers.Done()
		defer func() { <-slots }()
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				logger.LogAttrs(detached, slog.LevelError, "merge outcome: push observation panicked; dropped",
					slog.String("delivery_id", deliveryID), slog.Any("panic", r))
			}
		}()
		observe(detached)
	}()
	return true
}
