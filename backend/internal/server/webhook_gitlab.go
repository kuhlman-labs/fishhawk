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
	// redelivers.
	if s.cfg.WebhookDispatcher != nil {
		if err := s.cfg.WebhookDispatcher.Handle(r.Context(), ev); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"webhook dispatch failed", map[string]any{"error": err.Error()})
			return
		}
	}

	// The MR lifecycle is consumed server-side (the dispatcher skips
	// object_kind merge_request): a merge/close drives the review-stage
	// state machine via the ADR-018 resolver shared with the GitHub
	// pull_request.closed path. Best-effort — never influences the 202.
	if ev.Type == "merge_request" {
		s.handleGitLabMergeRequest(r.Context(), ev.RawBody)
	}

	w.WriteHeader(http.StatusAccepted)
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
// Best-effort throughout: a parse failure, a missing run, or a
// non-Fishhawk-managed MR all log and return without surfacing as a
// 5xx. Idempotent on redeliveries: TransitionStage is a no-op on an
// already-terminal stage.
func (s *Server) handleGitLabMergeRequest(ctx context.Context, raw []byte) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return
	}
	var p gitLabMergeRequestPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"gitlab merge_request: parse failed",
			slog.String("error", err.Error()))
		return
	}
	action := p.ObjectAttributes.Action
	if action != "merge" && action != "close" {
		return
	}
	target := s.findRunByGitLabMR(ctx, p.Project.PathWithNamespace, p.ObjectAttributes.IID, p.ObjectAttributes.URL)
	if target == nil {
		return
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
func (s *Server) findRunByGitLabMR(ctx context.Context, projectPath string, iid int, mrURL string) *run.Run {
	if projectPath == "" || iid <= 0 {
		return nil
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
		return nil
	}
	// Primary: match on the iid parsed out of the stored MR URL.
	for _, r := range runs {
		if r.PullRequestURL == nil || *r.PullRequestURL == "" {
			continue
		}
		if storedIID, ok := parseGitLabMRIID(*r.PullRequestURL); ok && storedIID == iid {
			return r
		}
	}
	// Fallback: normalized-URL equality, for a run whose stored URL
	// wasn't iid-parseable. Supplements the primary key; never the sole
	// resolution path.
	if mrURL != "" {
		want := normalizeGitLabMRURL(mrURL)
		for _, r := range runs {
			if r.PullRequestURL != nil && normalizeGitLabMRURL(*r.PullRequestURL) == want {
				return r
			}
		}
	}
	return nil
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
