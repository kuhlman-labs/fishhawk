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

	// Run dispatch from webhook events lands in a follow-up PR — it
	// needs a GitHub API client to resolve `.fishhawk/workflows.yaml`'s
	// SHA at the event's repo + ref. For now we acknowledge the
	// delivery and stop.
	w.WriteHeader(http.StatusAccepted)
}
