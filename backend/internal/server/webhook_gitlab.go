package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// handleWebhookGitLab implements POST /webhooks/gitlab, the GitLab
// parallel to handleWebhook. It mirrors the GitHub receiver's check
// order exactly — secret/store config → body cap → auth → parse →
// dedup → dispatch — so the two forges share one contract. Auth is the
// verbatim X-Gitlab-Token secret (GitLab sends the configured token,
// not an HMAC); dedup is the X-Gitlab-Event-UUID delivery id
// namespaced into the SAME delivery store the GitHub path uses.
//
// The endpoint is unversioned like /webhooks/github because GitLab
// controls the request shape; the backend adapts as GitLab's webhook
// schema evolves.
func (s *Server) handleWebhookGitLab(w http.ResponseWriter, r *http.Request) {
	if len(s.cfg.GitLabWebhookSecret) == 0 {
		s.writeError(w, r, http.StatusServiceUnavailable, "webhook_secret_unconfigured",
			"gitlab webhook receiver requires a configured secret", nil)
		return
	}
	if s.cfg.WebhookDeliveries == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "webhook_store_unconfigured",
			"gitlab webhook receiver requires a configured delivery store", nil)
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

	token := r.Header.Get("X-Gitlab-Token")
	if err := webhook.VerifyGitLabToken(s.cfg.GitLabWebhookSecret, token); err != nil {
		s.writeError(w, r, http.StatusUnauthorized, "webhook_token_invalid",
			"X-Gitlab-Token did not match the configured secret",
			map[string]any{"error": err.Error()})
		return
	}

	eventType := r.Header.Get("X-Gitlab-Event")
	eventUUID := r.Header.Get("X-Gitlab-Event-UUID")
	ev, err := webhook.ParseGitLabEvent(eventType, eventUUID, body)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"gitlab webhook headers or body invalid", map[string]any{"error": err.Error()})
		return
	}

	if err := s.cfg.WebhookDeliveries.Mark(ev.DeliveryID); err != nil {
		if errors.Is(err, webhook.ErrDeliveryDuplicate) {
			// Acknowledge duplicates with 202 — GitLab retries on
			// non-2xx, and we don't want a retry of an event we've
			// already processed.
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "gitlab webhook duplicate delivery",
				slog.String("event", ev.Type),
				slog.String("delivery_id", ev.DeliveryID),
			)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"failed to record delivery", map[string]any{"error": err.Error()})
		return
	}

	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "webhook received",
		slog.String("forge", "gitlab"),
		slog.String("event", ev.Type),
		slog.String("action", ev.Action),
		slog.String("delivery_id", ev.DeliveryID),
		slog.String("repo", ev.Repo),
		slog.String("sender", ev.Sender),
	)

	// Dispatch through the shared pipeline. MatchGitLabEvent handles
	// issue-label / note triggers and parks the deliberately-out-of-
	// scope kinds (MR lifecycle, pipeline, build); a non-nil error is a
	// transient infra failure the caller surfaces as 5xx so GitLab
	// redelivers. dispatchGitLabDelivery unmarks the delivery on that
	// error so the redelivery actually re-processes (see its doc).
	if s.cfg.WebhookDispatcher != nil {
		if err := s.dispatchGitLabDelivery(r.Context(), ev, s.cfg.WebhookDispatcher.Handle); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"webhook dispatch failed", map[string]any{"error": err.Error()})
			return
		}
	}

	// The MR lifecycle is consumed server-side (the dispatcher skips
	// object_kind merge_request): a merge/close drives the review-stage
	// state machine via the ADR-018 resolver shared with the GitHub
	// pull_request.closed path. A transient RunRepo lookup failure is the
	// GitLab receiver's ONLY review-gate signal (the GitHub path has the
	// merge-reconciler poll as a backstop; GitLab has none), so — unlike a
	// parse failure or a genuine no-match, which stay best-effort 202 — it
	// propagates as a non-nil error here. We unmark the already-recorded
	// delivery and surface a 5xx so GitLab redelivers and re-drives the
	// transition (E45.21; mirrors the dispatch-drop fix above).
	if ev.Type == "merge_request" {
		if err := s.handleGitLabMergeRequest(r.Context(), ev.RawBody); err != nil {
			s.unmarkGitLabDelivery(r.Context(), ev.DeliveryID)
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"gitlab merge_request lookup failed", map[string]any{"error": err.Error()})
			return
		}
	}

	w.WriteHeader(http.StatusAccepted)
}

// unmarkGitLabDelivery releases an ALREADY-RECORDED delivery so a GitLab
// redelivery is treated as a first delivery again and re-processes rather
// than deduping to a 202. It is best-effort: a failure to unmark is logged
// (the retry may then still be deduped) and does not abort the caller. Shared
// by dispatchGitLabDelivery and the MR review-gate consumer, both of which
// return a 5xx on a transient failure after Mark has already recorded the
// delivery.
func (s *Server) unmarkGitLabDelivery(ctx context.Context, deliveryID string) {
	if uerr := s.cfg.WebhookDeliveries.Unmark(deliveryID); uerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"gitlab webhook: failed to unmark delivery after error; retry may be deduped",
			slog.String("delivery_id", deliveryID),
			slog.String("error", uerr.Error()),
		)
	}
}

// dispatchGitLabDelivery runs dispatch for an ALREADY-RECORDED delivery
// and, on a dispatch error, UNMARKS ev.DeliveryID before returning so the
// caller's 5xx actually causes GitLab to re-process the event.
//
// The delivery is recorded (Mark) BEFORE dispatch so a concurrent
// redelivery dedups to a 202 rather than double-dispatching. The cost of
// recording first is that a dispatch failure would otherwise leave the
// delivery recorded: GitLab retries on the 5xx, the retry hits the recorded
// delivery, Mark returns ErrDeliveryDuplicate, and the receiver answers 202
// — permanently dropping an event whose processing actually failed (E45.6
// fix-up). Unmarking undoes the record so the retry is treated as a first
// delivery again and re-runs dispatch.
//
// A nil dispatch is a no-op success. The dispatch error is returned
// unchanged for the caller to map to a 500; the unmark is best-effort — a
// failure to unmark is logged (the retry may then still be deduped) but
// does not mask the original dispatch error.
func (s *Server) dispatchGitLabDelivery(ctx context.Context, ev webhook.Event, dispatch func(context.Context, webhook.Event) error) error {
	if dispatch == nil {
		return nil
	}
	err := dispatch(ctx, ev)
	if err == nil {
		return nil
	}
	s.unmarkGitLabDelivery(ctx, ev.DeliveryID)
	return err
}

// gitLabMergeRequestPayload is the subset of a GitLab Merge Request
// Hook payload the review-gate consumer reads
// (https://docs.gitlab.com/user/project/integrations/webhook_events/#merge-request-events).
// object_attributes.action is "merge" | "close" | "open" | "update" |
// "reopen" | "approved" | ...; only merge / close drive the review gate.
type gitLabMergeRequestPayload struct {
	Project struct {
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	User struct {
		Username string `json:"username"`
	} `json:"user"`
	ObjectAttributes struct {
		IID        int    `json:"iid"`
		Action     string `json:"action"`
		URL        string `json:"url"`
		LastCommit struct {
			ID string `json:"id"`
		} `json:"last_commit"`
	} `json:"object_attributes"`
}

// handleGitLabMergeRequest drives the review-stage state machine on a
// terminal MR event, reusing the source-neutral resolveReviewStageOnMerge
// (the ADR-018 resolver shared with the GitHub merge reconciler):
// action "merge" resolves the review stage to succeeded (merged=true),
// "close" to cancelled (merged=false, closed-without-merge). Every
// other MR action is ignored — they aren't review-gate signals.
//
// Best-effort EXCEPT for a transient run-lookup failure: a nil
// RunRepo/AuditRepo, a parse failure, a non-terminal MR action, or a
// genuine no-match all log and return nil (the receiver answers 202).
// A transient RunRepo error from findRunByGitLabMR, by contrast, is
// returned so the receiver unmarks the delivery and answers 5xx — the
// GitLab webhook is the only review-gate signal, so a dropped merge/close
// on a DB blip would strand the review stage permanently (E45.21).
// Idempotent on redeliveries: TransitionStage is a no-op on an
// already-terminal stage.
func (s *Server) handleGitLabMergeRequest(ctx context.Context, raw []byte) error {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return nil
	}
	var p gitLabMergeRequestPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"gitlab merge_request: parse failed",
			slog.String("error", err.Error()))
		return nil
	}
	action := p.ObjectAttributes.Action
	if action != "merge" && action != "close" {
		return nil
	}
	target, err := s.findRunByGitLabMR(ctx, p.Project.PathWithNamespace, p.ObjectAttributes.IID, p.ObjectAttributes.URL)
	if err != nil {
		return err
	}
	if target == nil {
		return nil
	}

	// prURL for the audit/log uses the Fishhawk-stored URL when present
	// (the canonical record) and falls back to the webhook's MR URL.
	prURL := p.ObjectAttributes.URL
	if target.PullRequestURL != nil && *target.PullRequestURL != "" {
		prURL = *target.PullRequestURL
	}
	meta := reviewMergeMeta{
		prURL:      prURL,
		headSHA:    p.ObjectAttributes.LastCommit.ID,
		actorLogin: p.User.Username,
		actorKind:  audit.ActorKind("user"),
	}
	s.resolveReviewStageOnMerge(ctx, target, action == "merge", meta)
	return nil
}

// findRunByGitLabMR resolves the Fishhawk run backing a GitLab MR by
// the durable identity BOTH the webhook and the stored run carry
// unambiguously: project path + MR iid (E45.6 binding condition 1).
// It deliberately does NOT rely on exact string equality between the
// webhook's object_attributes.url and the stored runs.pull_request_url
// — GitLab's webhook and API URL representations are not guaranteed
// byte-identical across versions (notably the /-/ infix), so a raw URL
// match would silently drop legitimate merges.
//
// The run is scoped to the project (Repo == path_with_namespace, which
// makes the per-project iid unambiguous) and matched on the iid parsed
// out of each candidate's stored MR URL. A URL-normalization pass
// SUPPLEMENTS — never replaces — the iid-keyed lookup: it catches a run
// whose stored URL differs from the webhook's only by the /-/ infix
// when the iid could not be parsed.
//
// Returns (nil, nil) on a genuine no-match (warn logged, best-effort) and
// (nil, err) on a transient RunRepo error from either ListRuns call — the
// caller propagates that error into a 5xx so GitLab redelivers rather than
// permanently dropping the merge/close transition (E45.21).
func (s *Server) findRunByGitLabMR(ctx context.Context, projectPath string, iid int, mrURL string) (*run.Run, error) {
	if projectPath == "" || iid <= 0 {
		return nil, nil
	}
	runs, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		Repo:  projectPath,
		Limit: 50,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"gitlab merge_request: run lookup failed",
			slog.String("project", projectPath),
			slog.Int("iid", iid),
			slog.String("error", err.Error()))
		return nil, err
	}
	// Primary: match on the iid parsed out of the stored MR URL.
	for _, r := range runs {
		if r.PullRequestURL == nil || *r.PullRequestURL == "" {
			continue
		}
		if storedIID, ok := parseGitLabMRIID(*r.PullRequestURL); ok && storedIID == iid {
			return r, nil
		}
	}
	// Fallback: normalized-URL equality, for a run whose stored URL
	// wasn't iid-parseable. Supplements the primary key; never the sole
	// resolution path.
	if mrURL != "" {
		want := normalizeGitLabMRURL(mrURL)
		for _, r := range runs {
			if r.PullRequestURL != nil && normalizeGitLabMRURL(*r.PullRequestURL) == want {
				return r, nil
			}
		}
	}
	// Supplement: an exact PullRequestURL DB filter, mirroring the GitHub
	// findRunByPullRequestURL lookup. Unlike the project-scoped scan above
	// this is NOT windowed by recency — the DB filters on the indexed
	// pull_request_url — so it resolves a run whose stored URL byte-matches
	// the webhook URL even when 50+ newer runs exist on the project (the
	// >window silent-miss the iid scan cannot see). Still a SUPPLEMENT, not
	// a replacement: it runs only after the durable iid key missed, and the
	// Go-side equality re-check keeps it exact-match-only (GitLab's /-/ infix
	// variance is handled by the iid + normalized paths above).
	if mrURL != "" {
		exact, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
			PullRequestURL: &mrURL,
			Limit:          5,
		})
		if err != nil {
			// Same transient-failure class as the primary lookup: propagate
			// so the caller redelivers rather than dropping the transition.
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"gitlab merge_request: exact-url run lookup failed",
				slog.String("project", projectPath),
				slog.Int("iid", iid),
				slog.String("error", err.Error()))
			return nil, err
		}
		for _, r := range exact {
			if r.PullRequestURL != nil && *r.PullRequestURL == mrURL {
				return r, nil
			}
		}
	}
	// No run matched. Unlike the GitHub path — which has the merge-
	// reconciler poll as a backstop — the GitLab webhook is the ONLY
	// review-gate signal, so a silent miss strands the review stage with
	// no trace. Warn so an unmatched merge/close is diagnosable.
	//
	// Known, deliberately-deferred residual (E45.20, tracked at #1861): the
	// exact-URL supplement above resolves an aged-out-of-window run ONLY
	// when its stored URL byte-matches the webhook URL. An OLDER MR whose
	// stored URL differs from the webhook's only by the /-/ infix variance
	// is resolvable solely via the iid/normalized paths, both windowed to
	// the 50 most recent project runs — so it still falls through to this
	// warn if it merges after 50+ newer runs. This is dormant until GitLab
	// run creation lands (#1861), which is the natural home for closing it:
	// either a durable project+IID persisted lookup column, or a
	// non-windowed normalized-URL DB filter alongside the exact-URL one.
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
		"gitlab merge_request: no run matched; review stage will not transition",
		slog.String("project", projectPath),
		slog.Int("iid", iid),
		slog.String("mr_url", mrURL))
	return nil, nil
}

// parseGitLabMRIID extracts the merge-request iid from a GitLab MR web
// URL. It tolerates both the current "/-/merge_requests/<iid>" form and
// the legacy "/merge_requests/<iid>" form (the /-/ infix the binding
// condition flags as version-variable), plus any trailing path / query
// / fragment. Returns ok=false when no positive iid is present.
func parseGitLabMRIID(u string) (int, bool) {
	const marker = "/merge_requests/"
	idx := strings.LastIndex(u, marker)
	if idx < 0 {
		return 0, false
	}
	rest := u[idx+len(marker):]
	if cut := strings.IndexAny(rest, "/?#"); cut >= 0 {
		rest = rest[:cut]
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// normalizeGitLabMRURL collapses the version-variable "/-/" infix and a
// trailing slash so two representations of the same MR compare equal.
// Used only by the fallback path in findRunByGitLabMR.
func normalizeGitLabMRURL(u string) string {
	u = strings.TrimSuffix(u, "/")
	return strings.ReplaceAll(u, "/-/", "/")
}
