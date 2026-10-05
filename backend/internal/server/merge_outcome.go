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

// pushObservers tracks every in-flight detached push observation so tests can
// wait for the background work to settle before reading the audit chain.
var pushObservers sync.WaitGroup

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
	detached, cancel := detachedPushContext(ctx)
	pushObservers.Add(1)
	go func() {
		defer pushObservers.Done()
		defer cancel()
		sum := obs.ObservePush(detached, push, ev.DeliveryID)
		if sum.Signals > 0 {
			s.cfg.Logger.LogAttrs(detached, slog.LevelInfo, "merge outcome: push observed",
				slog.String("delivery_id", ev.DeliveryID), slog.String("repo", push.FullName),
				slog.Int("signals", sum.Signals), slog.Int("resolved", sum.Resolved),
				slog.Bool("truncated", sum.Truncated), slog.Int("recorded", sum.Recorded),
				slog.Int("errors", sum.Errors))
		}
	}()
}
