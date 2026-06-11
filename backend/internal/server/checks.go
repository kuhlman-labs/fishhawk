package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// CategoryAuditCheckPublishDegraded is the audit-log category for the
// run-chain entry appended when the fishhawk_audit_complete Check Run
// publish has failed auditcheckpublisher.DefaultDegradedThreshold
// consecutive times for the run's head_sha (#993). Exactly one per
// failure episode; payload carries {head_sha, attempts, last_error}.
const CategoryAuditCheckPublishDegraded = "audit_check_publish_degraded"

// CategoryAuditCheckPublishRecovered is the paired entry appended on
// the successful publish that closes an open degraded episode (#993).
// Payload carries {head_sha, attempts}.
const CategoryAuditCheckPublishRecovered = "audit_check_publish_recovered"

// stageCheckResponse mirrors what the SPA's BlockingChecksPanel
// expects: a small `{name, state, …}` shape with the SPA's enum
// rather than raw GitHub fields. Detail fields (conclusion,
// head_sha, github_check_run_id) are forwarded for forensic /
// audit-export use. `missing` is populated only for self-derived
// checks like fishhawk_audit_complete (#229) where the failure
// reason is structured rather than a raw GitHub conclusion.
type stageCheckResponse struct {
	Name             string                      `json:"name"`
	State            string                      `json:"state"`
	Status           string                      `json:"status,omitempty"`
	Conclusion       *string                     `json:"conclusion,omitempty"`
	HeadSHA          string                      `json:"head_sha,omitempty"`
	GitHubCheckRunID *int64                      `json:"github_check_run_id,omitempty"`
	Timestamp        time.Time                   `json:"ts,omitempty"`
	Missing          []auditcomplete.MissingItem `json:"missing,omitempty"`
}

// stageChecksListResponse is the envelope for GET /v0/stages/{id}/checks.
// `declared` is the run's required-checks snapshot from branch
// protection (post-#251 / ADR-017); `sources` records which surfaces
// contributed (`branch_protection` and/or `ruleset:<id>`) so the SPA
// can render the right attribution sub-label (#256). `items` is the
// latest observed state per check name. Declared-but-not-observed
// checks render in the SPA as `not_tracked`; the response itself
// only carries observed rows since the SPA already knows the
// declared list.
type stageChecksListResponse struct {
	Declared []string             `json:"declared"`
	Sources  []string             `json:"sources"`
	Items    []stageCheckResponse `json:"items"`
}

// handleListStageChecks implements GET /v0/stages/{stage_id}/checks (#228).
//
// Returns the most-recent observed state per blocking check name on
// the stage, plus the gate's declared list so the SPA doesn't need
// to re-derive it from the Stage response. Declared-but-not-observed
// checks are reported in `declared` but absent from `items`; the
// SPA fills with `not_tracked`.
func (s *Server) handleListStageChecks(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil || s.cfg.StageCheckRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "stage_checks_unconfigured",
			"stage checks endpoint requires run and stage-check repos to be configured", nil)
		return
	}
	stageID, err := uuid.Parse(r.PathValue("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.PathValue("stage_id")})
		return
	}

	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "stage_not_found",
				"no stage with that id", map[string]any{"stage_id": stageID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get stage failed", map[string]any{"error": err.Error()})
		return
	}

	checks, err := s.cfg.StageCheckRepo.LatestForStage(r.Context(), stageID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list stage checks failed", map[string]any{"error": err.Error()})
		return
	}

	// Source the declared list from the run's required-checks
	// snapshot (#251 / ADR-017): the spec-level gate.blocking_checks
	// field was removed in v0.2 (#254). Best-effort — a run that
	// pre-dates the snapshot wiring or skipped protection lookup
	// (CLI / UI flow) renders an empty declared list and the SPA
	// falls back to "no checks declared yet".
	declared := []string{}
	sources := []string{}
	runRow, runErr := s.cfg.RunRepo.GetRun(r.Context(), stage.RunID)
	if runErr == nil && runRow.RequiredChecksSnapshot != nil {
		declared = runRow.RequiredChecksSnapshot.Contexts
		sources = runRow.RequiredChecksSnapshot.Sources
	}
	items := make([]stageCheckResponse, 0, len(checks))
	for _, c := range checks {
		items = append(items, toStageCheckResponse(c))
	}

	// Inject the self-derived fishhawk_audit_complete row for review
	// stages — the only stage type where the audit-complete signal
	// is meaningful (it gates the merge via the published Check Run
	// per #231). Computed live from the run's artifact + audit-log
	// presence; carries the structured `missing` list so the SPA
	// can show "fail because: plan missing, redacted trace missing
	// on stage X" without a secondary call.
	if stage.Type == run.StageTypeReview && s.cfg.ArtifactRepo != nil && s.cfg.AuditRepo != nil {
		state, missing, err := auditcomplete.Compute(r.Context(), stage.RunID, s.auditCompleteDeps())
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"derive audit-complete state failed",
				map[string]any{"stage_id": stageID.String(), "error": err.Error()})
			return
		}
		items = append(items, stageCheckResponse{
			Name:      AuditCompleteCheckName,
			State:     string(state),
			Timestamp: time.Now().UTC(),
			Missing:   missing,
		})

		// Publish the same state to GitHub as a Check Run (#231).
		// Best-effort: a failure logs but doesn't fail the read —
		// the in-Fishhawk gate enforcement still works without
		// the GitHub publish.
		s.publishAuditCheck(r.Context(), stage.RunID, state, missing)
	}

	s.writeJSON(w, r, http.StatusOK, stageChecksListResponse{
		Declared: declared,
		Sources:  sources,
		Items:    items,
	})
}

// publishAuditCheck is the small adapter between the server's
// compute paths and the auditcheckpublisher. Best-effort: a
// publish failure logs at WARN and returns; the in-Fishhawk gate
// enforcement still proceeds so a GitHub outage doesn't black-
// hole approvals. Nil-safe — the publisher is nil when
// ExternalURL or GitHub aren't wired (legacy / dev posture), and
// Publish returns immediately in that case. A PERSISTENT publish
// failure additionally surfaces on the run record itself: the
// publisher's episode callbacks (wired in New) append paired
// audit_check_publish_degraded / _recovered run-chain entries
// (#993), so a desynced merge gate is visible from
// fishhawk_get_run_status and the SPA without a daemon-log grep.
func (s *Server) publishAuditCheck(ctx context.Context, runID uuid.UUID, state stagecheck.State, missing []auditcomplete.MissingItem) {
	if s.auditCheckPublisher == nil {
		return
	}
	published, err := s.auditCheckPublisher.Publish(ctx, runID, state, missing)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"audit-complete check-run publish failed",
			slog.String("run_id", runID.String()),
			slog.String("state", string(state)),
			slog.String("error", err.Error()),
		)
		return
	}
	if published {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug,
			"audit-complete check-run published",
			slog.String("run_id", runID.String()),
			slog.String("state", string(state)),
		)
	}
}

// auditCheckPublishDegraded is the auditcheckpublisher's OnDegraded
// callback (#993): it surfaces a persistently failing
// fishhawk_audit_complete publish on the run record by appending a
// chained audit_check_publish_degraded entry. The publisher fires it
// at most once per in-process failure episode. Best-effort: an append
// failure logs at WARN and never unwinds the publish path.
func (s *Server) auditCheckPublishDegraded(ctx context.Context, runID uuid.UUID, headSHA string, attempts int, lastErr error) {
	if s.cfg.AuditRepo == nil {
		return
	}
	lastError := ""
	if lastErr != nil {
		lastError = lastErr.Error()
	}
	payload, _ := json.Marshal(map[string]any{
		"head_sha":   headSHA,
		"attempts":   attempts,
		"last_error": lastError,
	})
	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryAuditCheckPublishDegraded,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"audit-check publish degraded: audit append failed",
			slog.String("run_id", runID.String()),
			slog.String("head_sha", headSHA),
			slog.String("error", err.Error()))
	}
}

// auditCheckPublishRecovered is the auditcheckpublisher's OnRecovered
// callback (#993). The publisher invokes it on EVERY successful
// publish; whether a recovered entry is due derives from the run's
// audit chain — an audit_check_publish_degraded entry for this
// head_sha with no later audit_check_publish_recovered marks an open
// episode — NOT from the publisher's in-memory counter, so a daemon
// restart mid-episode can never orphan a degraded entry. Best-effort
// like the degraded side.
func (s *Server) auditCheckPublishRecovered(ctx context.Context, runID uuid.UUID, headSHA string, attempts int) {
	if s.cfg.AuditRepo == nil {
		return
	}
	open, err := s.openDegradedPublishEpisode(ctx, runID, headSHA)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"audit-check publish recovered: episode lookup failed",
			slog.String("run_id", runID.String()),
			slog.String("head_sha", headSHA),
			slog.String("error", err.Error()))
		return
	}
	if !open {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"head_sha": headSHA,
		"attempts": attempts,
	})
	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryAuditCheckPublishRecovered,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"audit-check publish recovered: audit append failed",
			slog.String("run_id", runID.String()),
			slog.String("head_sha", headSHA),
			slog.String("error", err.Error()))
	}
}

// openDegradedPublishEpisode reports whether the run's audit chain
// carries an audit_check_publish_degraded entry for headSHA with no
// later audit_check_publish_recovered entry for the same headSHA —
// the durable definition of an open episode (#993).
func (s *Server) openDegradedPublishEpisode(ctx context.Context, runID uuid.UUID, headSHA string) (bool, error) {
	degradedSeq, degraded, err := lastPublishEpisodeSeq(ctx, s.cfg.AuditRepo, runID, CategoryAuditCheckPublishDegraded, headSHA)
	if err != nil || !degraded {
		return false, err
	}
	recoveredSeq, recovered, err := lastPublishEpisodeSeq(ctx, s.cfg.AuditRepo, runID, CategoryAuditCheckPublishRecovered, headSHA)
	if err != nil {
		return false, err
	}
	return !recovered || recoveredSeq < degradedSeq, nil
}

// lastPublishEpisodeSeq returns the highest chain sequence among the
// run's entries of `category` whose payload head_sha matches.
// Entries with unparseable payloads are skipped — they can't be
// matched to an episode either way.
func lastPublishEpisodeSeq(ctx context.Context, repo audit.Repository, runID uuid.UUID, category, headSHA string) (int64, bool, error) {
	entries, err := repo.ListForRunByCategory(ctx, runID, category)
	if err != nil {
		return 0, false, err
	}
	var seq int64
	found := false
	for _, e := range entries {
		var p struct {
			HeadSHA string `json:"head_sha"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || p.HeadSHA != headSHA {
			continue
		}
		if !found || e.Sequence > seq {
			seq = e.Sequence
			found = true
		}
	}
	return seq, found, nil
}

func toStageCheckResponse(c *stagecheck.Check) stageCheckResponse {
	return stageCheckResponse{
		Name:             c.Name,
		State:            string(c.State),
		Status:           c.Status,
		Conclusion:       c.Conclusion,
		HeadSHA:          c.HeadSHA,
		GitHubCheckRunID: c.GitHubCheckRunID,
		Timestamp:        c.Timestamp,
	}
}

// auditCompleteDeps builds the auditcomplete.Deps with every closure the
// Compute rules need (#229 / #282 / #947). Centralized so the four
// closures — PRHead (foreign-commit rule 5), ImplementReviewers +
// ReviewBackstop + Now (review-pending rule 6) — cannot be forgotten at one
// of the multiple Compute call sites (the checks read endpoint, the
// synchronize republish, the post-review republish). Each closure is
// nil-tolerant inside Compute, so a dev/test Server without GitHub or a
// workflow spec degrades cleanly to the rules it can evaluate.
func (s *Server) auditCompleteDeps() auditcomplete.Deps {
	return auditcomplete.Deps{
		Runs:      s.cfg.RunRepo,
		Artifacts: s.cfg.ArtifactRepo,
		Audit:     s.cfg.AuditRepo,
		// Live HEAD lookup for the foreign-commit rule (#282).
		// Nil-safe: when GitHub isn't wired (dev / CLI runs), Compute
		// treats `nil PRHead` as "skip the drift rule" rather than
		// failing — the rest of the audit still evaluates.
		PRHead: s.prHeadFetcher(),
		// Implement-stage reviewers.agent resolution for the review-
		// pending presence gate (#947). Reuses resolveStageReviewers so
		// spec parsing stays single-sourced; nil result (no spec / no
		// implement stage / no agent reviewer) skips the rule cleanly.
		ImplementReviewers: func(runRow *run.Run) *spec.ReviewersConfig {
			return s.resolveStageReviewers(context.Background(), runRow, spec.StageTypeImplement)
		},
		// Same hard max-wait the ADR-036 merge-resolution hold uses, so
		// a stuck review can't wedge the audit gate any longer than it
		// wedges the merge.
		ReviewBackstop: s.planReviewBackstop,
	}
}

// recomputeAndPublishAuditComplete re-derives the audit-complete state for a
// run and republishes it as the fishhawk_audit_complete Check Run (#947).
// Extracted from republishOnSynchronize so the post-implement-review path
// (runImplementReviewLoop, trace.go) can flip the required check green the
// moment the advisory review lands — GitHub re-evaluates branch protection
// when the Check Run conclusion updates, so the merge gate clears with no
// operator action. Best-effort: a compute or publish failure logs and
// returns; the canonical state is recomputed again on the next SPA visit or
// PR webhook.
func (s *Server) recomputeAndPublishAuditComplete(ctx context.Context, runID uuid.UUID) {
	if s.cfg.RunRepo == nil || s.cfg.ArtifactRepo == nil || s.cfg.AuditRepo == nil {
		return
	}
	state, missing, err := auditcomplete.Compute(ctx, runID, s.auditCompleteDeps())
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"audit-complete recompute failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return
	}
	s.publishAuditCheck(ctx, runID, state, missing)
}

// RepublishAuditCheck re-derives a run's audit-complete state and republishes
// the fishhawk_audit_complete Check Run (#973). Exported for the merge
// reconciler's per-tick heal sweep — the same export-for-reconciler pattern as
// ResolveReviewFromPollState / ReverifyBranchLineage. The publisher's dedup
// cache records only on a SUCCESSFUL publish, so re-invoking this for every
// parked review stage retries exactly the dropped publishes (a transient
// GitHub failure heals within one tick after recovery) while an
// already-published state dedups to a no-op. Best-effort, like the
// recompute it delegates to. When the retried publish keeps failing,
// the publisher's episode tracking degrades to a run-chain
// audit_check_publish_degraded entry after
// auditcheckpublisher.DefaultDegradedThreshold consecutive attempts
// (#993) — see publishAuditCheck.
func (s *Server) RepublishAuditCheck(ctx context.Context, runID uuid.UUID) {
	s.recomputeAndPublishAuditComplete(ctx, runID)
}

// prHeadFetcher returns the closure auditcomplete.Compute calls
// for the foreign-commit rule (#282). Nil when no GitHub client is
// wired — Compute then skips the rule cleanly. Production wires
// `cfg.GitHub.GetPullRequest`.
func (s *Server) prHeadFetcher() auditcomplete.PRHeadFetcher {
	if s.cfg.GitHub == nil {
		return nil
	}
	return func(ctx context.Context, installationID int64, repo githubclient.RepoRef, prNumber int) (string, error) {
		pr, err := s.cfg.GitHub.GetPullRequest(ctx, installationID, repo, prNumber)
		if err != nil {
			return "", err
		}
		return pr.HeadSHA, nil
	}
}
