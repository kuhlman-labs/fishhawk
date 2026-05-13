package server

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// maxWebhookBody is a defensive upper bound on the body GitHub will
// send. GitHub's documented max is 25 MiB; we cap at 10 MiB which
// comfortably handles every payload type we care about (issues,
// PRs, pushes, marketplace events) and rejects pathological inputs.
const maxWebhookBody = 10 * 1024 * 1024

// handleWebhook implements POST /webhooks/github per
// docs/api/v0.openapi.yaml. The endpoint is unversioned because
// GitHub controls the request shape; the backend adapts as
// GitHub's webhook schema evolves.
//
// Order of checks: signature → headers → dedup. We verify the
// signature first so a forged body can't influence the dedup
// store. Headers come next so we don't re-process events whose
// payload is malformed; dedup is last because it has a side
// effect (recording the delivery).
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if len(s.cfg.GitHubWebhookSecret) == 0 {
		s.writeError(w, r, http.StatusServiceUnavailable, "webhook_secret_unconfigured",
			"webhook receiver requires a configured secret", nil)
		return
	}
	if s.cfg.WebhookDeliveries == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "webhook_store_unconfigured",
			"webhook receiver requires a configured delivery store", nil)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return
	}
	if len(body) > maxWebhookBody {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"webhook body exceeds 10 MiB", map[string]any{"limit_bytes": maxWebhookBody})
		return
	}

	sig := r.Header.Get("X-Hub-Signature-256")
	if err := webhook.VerifySignature(s.cfg.GitHubWebhookSecret, body, sig); err != nil {
		s.writeError(w, r, http.StatusUnauthorized, "webhook_signature_invalid",
			"signature did not verify against the configured secret",
			map[string]any{"error": err.Error()})
		return
	}

	eventType := r.Header.Get("X-GitHub-Event")
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	ev, err := webhook.ParseEvent(eventType, deliveryID, body)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"webhook headers or body invalid", map[string]any{"error": err.Error()})
		return
	}

	if err := s.cfg.WebhookDeliveries.Mark(deliveryID); err != nil {
		if errors.Is(err, webhook.ErrDeliveryDuplicate) {
			// Acknowledge duplicates with 202 — GitHub retries on
			// non-2xx, and we don't want to ask for a retry of an
			// event we've already processed.
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "webhook duplicate delivery",
				slog.String("event", ev.Type),
				slog.String("delivery_id", deliveryID),
			)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"failed to record delivery", map[string]any{"error": err.Error()})
		return
	}

	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "webhook received",
		slog.String("event", ev.Type),
		slog.String("action", ev.Action),
		slog.String("delivery_id", ev.DeliveryID),
		slog.String("repo", ev.Repo),
		slog.String("sender", ev.Sender),
	)

	// Dispatch the event when a dispatcher is configured. Skips
	// (no installation id, unrecognized event, bot author, etc.)
	// are the dispatcher's responsibility — Handle returns nil
	// for the non-transient cases and we acknowledge with 202.
	// A non-nil error here is a transient infrastructure failure;
	// 5xx tells GitHub to retry.
	if s.cfg.WebhookDispatcher != nil {
		if err := s.cfg.WebhookDispatcher.Handle(r.Context(), ev); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"webhook dispatch failed", map[string]any{"error": err.Error()})
			return
		}
	}

	// `check_run` events update the gate's blocking-check states
	// (#228). Distinct from the dispatcher path: those events
	// don't trigger runs, they update existing ones. Best-effort
	// — per-row failures inside ingestCheckRun log but don't
	// surface as a 5xx, since the audit log keeps the canonical
	// trail and a missed delivery just leaves the SPA showing
	// stale state until the next event.
	if ev.Type == "check_run" {
		s.ingestCheckRun(r.Context(), ev.RawBody)
	}

	// `pull_request.synchronize` fires whenever the PR's head_sha
	// moves (push, force-push, rebase, "Update branch" merge from
	// base). Re-publish the fishhawk_audit_complete Check Run so
	// branch protection re-evaluates against the new HEAD — that's
	// where the foreign-commit drift becomes visible to the
	// reviewer + the merge gate (#282). Best-effort.
	if ev.Type == "pull_request" && ev.Action == "synchronize" {
		s.republishOnSynchronize(r.Context(), ev.RawBody)
	}

	// `pull_request.closed` with merged=true is the review-stage
	// success signal per ADR-018 (#311 / #312). Branch protection
	// already gated the merge — Fishhawk just records who merged
	// and transitions the review stage. Closed-without-merging
	// leaves the run in awaiting_approval; operator can manually
	// intervene. Best-effort.
	if ev.Type == "pull_request" && ev.Action == "closed" {
		s.handlePullRequestClosed(r.Context(), ev.RawBody)
	}

	// `pull_request_review.submitted` records approver / commenter
	// actions on the PR into the audit log per ADR-018. Audit-only
	// — the merge event is what advances the stage. Best-effort.
	if ev.Type == "pull_request_review" && ev.Action == "submitted" {
		s.handlePullRequestReviewSubmitted(r.Context(), ev.RawBody)
	}

	w.WriteHeader(http.StatusAccepted)
}
