package server

import (
	"context"
	"net/http"
	"sort"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// GET /v0/restart-blockers (E83.30 / #3974) answers one question for
// `scripts/dev reload` / `post-merge`: would restarting THIS daemon orphan
// work right now? It is a read-only PROJECTION of already-recorded state — it
// writes nothing and mints no audit entry — and reuses the orphaned-review
// boot sweep's own round tally (latestReviewStarted /
// countLandedReviewTerminals in review_reconcile.go) rather than re-deriving
// the in-flight predicate. Long-form contract: backend/internal/server/README.md
// § "Restart blockers".

// restartBlockersRunScanLimit bounds the non-terminal runs scanned per
// request (pending + running combined). A bite sets truncated=true.
const restartBlockersRunScanLimit = 500

// Restart-blocker reasons — a closed three-member set mirrored by the OpenAPI
// RestartBlocker.reason enum and parsed by scripts/dev's
// _parse_restart_blockers.
const (
	// restartBlockerUndispatchedChild: a decomposition child whose implement
	// stage is still pending / awaiting_host_dispatch.
	restartBlockerUndispatchedChild = "undispatched_child"
	// restartBlockerReviewInFlight: a plan/implement review round dispatched
	// by THIS process with fewer landed verdicts than configured reviewers —
	// the in-process reviewing goroutine dies with the daemon.
	restartBlockerReviewInFlight = "review_in_flight"
	// restartBlockerCheckFailed: a per-run stage or audit read failed, so the
	// daemon could not decide that run's state. scripts/dev treats it as a
	// REFUSING blocker (fail-closed on a reachable-but-undecided daemon).
	restartBlockerCheckFailed = "check_failed"
)

// restartBlockerStageImplement is the stage an undispatched_child item names.
// Review items carry orphanedReviewStageKind.label ("plan" / "implement"), so
// the OpenAPI RestartBlocker.stage enum is exactly those two labels.
const restartBlockerStageImplement = "implement"

// restartBlockersResponse is the GET /v0/restart-blockers body.
type restartBlockersResponse struct {
	Items       []restartBlocker `json:"items"`
	ScannedRuns int              `json:"scanned_runs"`
	// Truncated is true when the candidate-run scan hit
	// restartBlockersRunScanLimit, so runs past the cap were not checked.
	Truncated bool `json:"truncated"`
}

// restartBlocker is one reason a restart would orphan work. Every value is an
// id, an enum slug or a count — no free text — so the shell parser in
// scripts/dev can extract it without a JSON decoder.
type restartBlocker struct {
	RunID  string `json:"run_id"`
	Reason string `json:"reason"`
	Stage  string `json:"stage"`
	// ParentRunID is set on undispatched_child: the decomposition parent.
	ParentRunID string `json:"parent_run_id,omitempty"`
	// StageState is set on undispatched_child: the implement stage's state.
	StageState string `json:"stage_state,omitempty"`
	// ConfiguredAgents / Landed are set on review_in_flight: the round's
	// configured reviewer count and the verdicts landed so far. Pointers so a
	// real zero (no verdict yet) is emitted rather than omitted.
	ConfiguredAgents *int `json:"configured_agents,omitempty"`
	Landed           *int `json:"landed,omitempty"`
}

// handleListRestartBlockers implements GET /v0/restart-blockers.
//
// Auth mirrors handleListRuns exactly: anonymous callers are accepted (no
// scope check — scripts/dev calls it with no token), the caller's
// Identity.AccountID bounds the run scan, and the repo-visibility filter drops
// runs the caller cannot read, failing the whole request CLOSED (503) on a
// filter fault. The items expose only run ids, enum slugs and counts — a
// subset of what anonymous GET /v0/runs and /v0/runs/{id}/stages serve.
func (s *Server) handleListRestartBlockers(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"restart-blockers endpoint requires a configured run repository", nil)
		return
	}
	if s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "audit_repo_unconfigured",
			"restart-blockers endpoint requires a configured audit repository", nil)
		return
	}
	ctx := r.Context()
	accountFilter := IdentityFrom(ctx).AccountID

	// The same non-terminal set the orphaned-review boot sweep pages
	// (reconcileOrphanedReviewStates). ListRunsFilter.State is a single
	// string, so each state is its own scan, each asking for one row past the
	// cap so a bite is observable without a COUNT.
	var candidates []*run.Run
	for _, st := range reconcileOrphanedReviewStates {
		rows, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
			State:     string(st),
			AccountID: accountFilter,
			Limit:     restartBlockersRunScanLimit + 1,
		})
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"list runs failed", map[string]any{"error": err.Error()})
			return
		}
		candidates = append(candidates, rows...)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
		}
		return candidates[i].ID.String() < candidates[j].ID.String()
	})
	resp := restartBlockersResponse{Items: []restartBlocker{}}
	if len(candidates) > restartBlockersRunScanLimit {
		candidates = candidates[:restartBlockersRunScanLimit]
		resp.Truncated = true
	}

	filter, ok := s.requestRepoFilter(w, r)
	if !ok {
		return
	}
	for _, ru := range candidates {
		allowed, ferr := filter.allows(ctx, ru.Repo)
		if ferr != nil {
			s.writeRepoFilterUnavailable(w, r)
			return
		}
		if !allowed {
			continue
		}
		resp.ScannedRuns++
		resp.Items = append(resp.Items, s.runRestartBlockers(ctx, ru)...)
	}

	sort.SliceStable(resp.Items, func(i, j int) bool {
		a, b := resp.Items[i], resp.Items[j]
		if a.RunID != b.RunID {
			return a.RunID < b.RunID
		}
		if a.Reason != b.Reason {
			return a.Reason < b.Reason
		}
		return a.Stage < b.Stage
	})
	s.writeJSON(w, r, http.StatusOK, resp)
}

// runRestartBlockers derives every blocker one non-terminal run contributes.
// Each check is independent: a read failure yields a check_failed item for
// that check only and the others still run.
func (s *Server) runRestartBlockers(ctx context.Context, ru *run.Run) []restartBlocker {
	var out []restartBlocker
	runID := ru.ID.String()

	// (a) Undispatched decomposition child: restarting mid-fan-out strands a
	// child the operator (or run_children) has not dispatched yet.
	if ru.DecomposedFrom != nil {
		stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, ru.ID)
		if err != nil {
			out = append(out, restartBlocker{RunID: runID, Reason: restartBlockerCheckFailed, Stage: restartBlockerStageImplement})
		} else {
			for _, st := range stages {
				if st.Type != run.StageTypeImplement {
					continue
				}
				if st.State == run.StageStatePending || st.State == run.StageStateAwaitingHostDispatch {
					out = append(out, restartBlocker{
						RunID:       runID,
						Reason:      restartBlockerUndispatchedChild,
						Stage:       restartBlockerStageImplement,
						ParentRunID: ru.DecomposedFrom.String(),
						StageState:  string(st.State),
					})
					break
				}
			}
		}
	}

	// (b) Review round in flight in THIS process. The round tally is the
	// orphaned-review reconcile's own (latest started round, verdicts landed
	// strictly after it). A round dispatched BEFORE this process booted is
	// not a blocker: its goroutine is already dead and the next boot sweep
	// (#1781 / #2712) closes it, so a restart loses nothing more.
	for _, stage := range orphanedReviewStages {
		latest, payload, found, err := s.latestReviewStarted(ctx, ru.ID, stage)
		if err != nil {
			out = append(out, restartBlocker{RunID: runID, Reason: restartBlockerCheckFailed, Stage: stage.label})
			continue
		}
		if !found {
			continue
		}
		landed, err := s.countLandedReviewTerminals(ctx, ru.ID, stage, latest.Sequence)
		if err != nil {
			out = append(out, restartBlocker{RunID: runID, Reason: restartBlockerCheckFailed, Stage: stage.label})
			continue
		}
		// Settled — or never pending: a round with no configured reviewer
		// (ConfiguredAgents <= 0) is settled at zero verdicts.
		if landed >= payload.ConfiguredAgents {
			continue
		}
		if latest.Timestamp.Before(s.processStart) {
			continue
		}
		configured := payload.ConfiguredAgents
		out = append(out, restartBlocker{
			RunID:            runID,
			Reason:           restartBlockerReviewInFlight,
			Stage:            stage.label,
			ConfiguredAgents: &configured,
			Landed:           &landed,
		})
	}
	return out
}
