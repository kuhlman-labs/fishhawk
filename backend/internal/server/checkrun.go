package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// checkRunPayload is the slice of GitHub's check_run webhook payload
// the server reads. Used by both ingestCheckRun (writes to
// stage_checks) and reevaluateCIPolicy (#300). Defined in `server`
// because both consumers live here; if a third consumer lands
// elsewhere we'll lift it into a shared types package. Doc:
// https://docs.github.com/en/webhooks/webhook-events-and-payloads#check_run
type checkRunPayload struct {
	Action   string `json:"action"`
	CheckRun struct {
		ID           int64      `json:"id"`
		Name         string     `json:"name"`
		HeadSHA      string     `json:"head_sha"`
		Status       string     `json:"status"`
		Conclusion   *string    `json:"conclusion"`
		StartedAt    *time.Time `json:"started_at"`
		CompletedAt  *time.Time `json:"completed_at"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_run"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// ingestCheckRun handles a GitHub `check_run` event by walking the
// payload's pull_requests[] array and writing a stage_checks row per
// (matching stage, check name) pair (#228).
//
// Best-effort: per-row failures log but don't propagate. The
// webhook receiver acknowledges 202 either way — GitHub treats
// check_run delivery as best-effort too, and we don't want to ask
// for a redelivery for a single match's transient DB error.
//
// Skipped silently when:
//   - StageCheckRepo isn't wired (legacy deployments).
//   - The event's action isn't `created` / `completed` / `rerequested`
//     (other actions don't carry state-change semantics worth recording).
//   - The check_run has no pull_requests[] (org-level checks, etc.).
//   - No Fishhawk run matches the (pr_number, head_sha) pair.
func (s *Server) ingestCheckRun(ctx context.Context, raw []byte) {
	if s.cfg.StageCheckRepo == nil {
		return
	}
	var p checkRunPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "check_run: payload parse failed",
			slog.String("error", err.Error()),
		)
		return
	}
	if !checkRunActionIsStateBearing(p.Action) {
		return
	}
	if p.CheckRun.Name == "" {
		return
	}
	// Pick a usable timestamp. Completed events carry completed_at;
	// in-progress events carry started_at; anything else falls
	// back to "now" so the audit story has a value.
	ts := time.Now().UTC()
	if p.CheckRun.CompletedAt != nil {
		ts = p.CheckRun.CompletedAt.UTC()
	} else if p.CheckRun.StartedAt != nil {
		ts = p.CheckRun.StartedAt.UTC()
	}

	// Each pull_request × declared-blocking_check match writes one
	// row. v0 walks pull_requests[] linearly — most check_run
	// events list at most one PR (the head ref) so the inner
	// roundtrip count is small.
	for _, pr := range p.CheckRun.PullRequests {
		stages, err := s.cfg.StageCheckRepo.FindMatchingStages(ctx, pr.Number, p.CheckRun.HeadSHA, p.CheckRun.Name)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "check_run: find matching stages failed",
				slog.Int("pr_number", pr.Number),
				slog.String("head_sha", p.CheckRun.HeadSHA),
				slog.String("check_name", p.CheckRun.Name),
				slog.String("error", err.Error()),
			)
			continue
		}
		if len(stages) == 0 {
			continue
		}
		runID := int64(p.CheckRun.ID)
		for _, stageID := range stages {
			if _, err := s.cfg.StageCheckRepo.Append(ctx, stagecheck.AppendParams{
				StageID:          stageID,
				Name:             p.CheckRun.Name,
				Status:           p.CheckRun.Status,
				Conclusion:       p.CheckRun.Conclusion,
				HeadSHA:          p.CheckRun.HeadSHA,
				GitHubCheckRunID: &runID,
				Timestamp:        ts,
				Payload:          raw,
			}); err != nil {
				s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "check_run: append failed",
					slog.String("stage_id", stageID.String()),
					slog.String("check_name", p.CheckRun.Name),
					slog.String("error", err.Error()),
				)
			}
		}
	}
}

// checkRunActionIsStateBearing reports whether a check_run event's
// action describes a state we care about persisting. v0 records
// every state change — created (queued / in_progress) and
// completed. `rerequested` re-fires the original event, so we
// record it too. Anything else (`requested_action`, deprecated
// values) is a no-op for v0.
func checkRunActionIsStateBearing(action string) bool {
	switch action {
	case "created", "completed", "rerequested":
		return true
	default:
		return false
	}
}
