package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryCIRetriggered is the audit category handleRetriggerCI appends after
// it re-ran at least one of a run PR's failed CI workflow runs at the PR's
// current head (E83.49 / #4082). Payload: pr_url, head_sha, rerun, skipped,
// failed. Internal audit kind — NOT an issue-comment surface.
const CategoryCIRetriggered = "ci_retriggered"

// retriggerCIEvents is the allow-list of workflow-run events the CI re-trigger
// re-runs: the events a pull request's own CI fires on. workflow_dispatch is
// deliberately absent, so a Fishhawk runner dispatch (the github_actions
// runner kind fires workflow_dispatch) is never re-executed by this verb; any
// other event (schedule, issue_comment, …) is not the PR's CI either.
var retriggerCIEvents = map[string]struct{}{
	"pull_request":        {},
	"pull_request_target": {},
	"push":                {},
}

// Skip reasons reported on retriggerCIResponse.Skipped.
const (
	retriggerSkipEventExcluded = "event_excluded"
	retriggerSkipNotCompleted  = "not_completed"
	retriggerSkipSucceeded     = "succeeded"
)

// retriggerCIRun is one workflow run as the CI re-trigger reports it. Reason is
// set on skipped entries only; Error on failed entries only.
type retriggerCIRun struct {
	ID         int64  `json:"id"`
	Event      string `json:"event"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	HTMLURL    string `json:"html_url,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Error      string `json:"error,omitempty"`
}

// retriggerCIResponse is the 200 body of POST /v0/runs/{run_id}/retrigger-ci.
// The three lists are always present (never null): rerun names the workflow
// runs GitHub accepted a re-run for, skipped the runs at the head that were
// not re-run (with a reason), and failed the re-runs GitHub refused.
type retriggerCIResponse struct {
	RunID   string           `json:"run_id"`
	PRURL   string           `json:"pr_url"`
	HeadSHA string           `json:"head_sha"`
	Rerun   []retriggerCIRun `json:"rerun"`
	Skipped []retriggerCIRun `json:"skipped"`
	Failed  []retriggerCIRun `json:"failed"`
}

// handleRetriggerCI implements POST /v0/runs/{run_id}/retrigger-ci
// (fishhawk_retrigger_ci, E83.49 / #4082).
//
// It is the SAFE CI re-trigger: closing and reopening a run's pull request to
// re-fire CI cancels the run, so this verb re-runs the PR's completed,
// non-successful CI workflow runs at the PR's CURRENT head through the GitHub
// Actions re-run API instead. A listed workflow run whose head_sha is not that
// head is dropped before selection, so a stale head's run is never re-run. It makes NO pull-request write, NO ref write and
// NO commit: the only writes are the re-run POSTs and one audit entry.
//
// Auth mirrors handleVouchCommit exactly: anonymous → 401; a run-bound agent
// token → 403 run_token_forbidden (even for its own run — re-triggering CI is
// an operator action); any identity without write:stages → 403
// insufficient_scope, enforced unconditionally (no cookie-session bypass).
//
// Then, in order: 400 validation_failed on a bad run_id; 503
// retrigger_unconfigured when the run/audit repositories or the GitHub client
// are not wired; 404 run_not_found; 409 run_has_no_pull_request; 422
// retrigger_unsupported_forge when the run has no GitHub installation (a
// GitLab run); 502 forge_error when the pull request or its workflow runs
// cannot be read; 409 pull_request_not_open; 409 no_ci_runs_at_head when no
// pull_request / pull_request_target / push workflow run exists at the head;
// 502 retrigger_failed when EVERY attempted re-run failed (no audit entry).
// Otherwise 200 with the rerun / skipped / failed lists, and — when at least
// one re-run was accepted — one ci_retriggered audit entry.
func (s *Server) handleRetriggerCI(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not re-trigger CI; re-running a pull request's CI is an operator action",
			nil)
		return
	}
	if !hasScope(id, "write:stages") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages",
			map[string]any{"required_scope": "write:stages"})
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil || s.cfg.GitHub == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "retrigger_unconfigured",
			"retrigger-ci endpoint requires run + audit repositories and a GitHub client", nil)
		return
	}

	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}

	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)
	if prNumber <= 0 {
		s.writeError(w, r, http.StatusConflict, "run_has_no_pull_request",
			"the run has no tracked pull request, so there is no CI to re-trigger",
			map[string]any{"run_id": runID.String()})
		return
	}
	prURL := *runRow.PullRequestURL

	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		s.writeError(w, r, http.StatusUnprocessableEntity, "retrigger_unsupported_forge",
			"CI re-trigger is supported only for GitHub runs (the run has no GitHub App installation); push a commit to the PR branch to re-fire CI instead",
			map[string]any{"run_id": runID.String(), "pr_url": prURL})
		return
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"run repo is unparseable", map[string]any{"repo": runRow.Repo, "error": err.Error()})
		return
	}

	pr, err := s.cfg.GitHub.GetPullRequest(r.Context(), scope, repo, prNumber)
	if err != nil {
		s.writeError(w, r, http.StatusBadGateway, "forge_error",
			"reading the run's pull request failed",
			map[string]any{"pr_url": prURL, "error": err.Error()})
		return
	}
	if pr.State != "open" {
		s.writeError(w, r, http.StatusConflict, "pull_request_not_open",
			"the run's pull request is not open, so its CI is not re-triggered. Never close a run's PR to re-trigger CI: the close cancels the run. A reopen within 10 minutes of the close, with the head unchanged, revives the run to its review gate; then call fishhawk_retrigger_ci",
			map[string]any{"pr_url": prURL, "state": pr.State, "merged": pr.Merged})
		return
	}
	headSHA := strings.TrimSpace(pr.HeadSHA)
	if headSHA == "" {
		s.writeError(w, r, http.StatusBadGateway, "forge_error",
			"the pull request returned an empty head sha",
			map[string]any{"pr_url": prURL})
		return
	}

	workflowRuns, err := s.cfg.GitHub.ListWorkflowRunsForHeadSHA(r.Context(), scope, repo, headSHA)
	if err != nil {
		s.writeError(w, r, http.StatusBadGateway, "forge_error",
			"listing the workflow runs at the pull request head failed",
			map[string]any{"pr_url": prURL, "head_sha": headSHA, "error": err.Error()})
		return
	}

	resp := retriggerCIResponse{
		RunID:   runID.String(),
		PRURL:   prURL,
		HeadSHA: headSHA,
		Rerun:   []retriggerCIRun{},
		Skipped: []retriggerCIRun{},
		Failed:  []retriggerCIRun{},
	}
	var candidates []retriggerCIRun
	ciRuns := 0
	for _, wr := range workflowRuns {
		if wr == nil {
			continue
		}
		// Bind to the PR's CURRENT head: a listed run GitHub recorded for any
		// other commit is not this head's CI, so it is neither re-run nor
		// counted nor reported.
		if wr.HeadSHA != headSHA {
			continue
		}
		entry := retriggerCIRun{
			ID: wr.ID, Event: wr.Event, Status: wr.Status, Conclusion: wr.Conclusion, HTMLURL: wr.HTMLURL,
		}
		if _, ok := retriggerCIEvents[wr.Event]; !ok {
			entry.Reason = retriggerSkipEventExcluded
			resp.Skipped = append(resp.Skipped, entry)
			continue
		}
		ciRuns++
		switch {
		case wr.Status != "completed":
			entry.Reason = retriggerSkipNotCompleted
			resp.Skipped = append(resp.Skipped, entry)
		case wr.Conclusion == "success":
			entry.Reason = retriggerSkipSucceeded
			resp.Skipped = append(resp.Skipped, entry)
		default:
			candidates = append(candidates, entry)
		}
	}
	if ciRuns == 0 {
		s.writeError(w, r, http.StatusConflict, "no_ci_runs_at_head",
			"no pull_request, pull_request_target or push workflow run exists at the pull request head, so there is nothing to re-run. Push a commit to the PR branch to fire CI and record it with fishhawk_vouch_commit; do not close and reopen the PR",
			map[string]any{"pr_url": prURL, "head_sha": headSHA, "skipped": resp.Skipped})
		return
	}

	for _, c := range candidates {
		if rerr := s.cfg.GitHub.RerunWorkflowRun(r.Context(), scope, repo, c.ID); rerr != nil {
			c.Error = rerr.Error()
			resp.Failed = append(resp.Failed, c)
			continue
		}
		resp.Rerun = append(resp.Rerun, c)
	}
	if len(candidates) > 0 && len(resp.Rerun) == 0 {
		// Every attempted re-run failed: nothing happened, so nothing is
		// recorded. The caller sees each run's error.
		s.writeError(w, r, http.StatusBadGateway, "retrigger_failed",
			"every workflow re-run request failed; no CI was re-triggered",
			map[string]any{"pr_url": prURL, "head_sha": headSHA, "failed": resp.Failed})
		return
	}
	if len(resp.Rerun) == 0 {
		// Nothing was eligible (every CI run at the head is in progress or
		// succeeded): no re-run happened, so no audit entry is written.
		s.writeJSON(w, r, http.StatusOK, resp)
		return
	}

	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := audit.ActorUser
	rerunIDs := make([]int64, 0, len(resp.Rerun))
	for _, c := range resp.Rerun {
		rerunIDs = append(rerunIDs, c.ID)
	}
	payload, _ := json.Marshal(map[string]any{
		"pr_url":   prURL,
		"head_sha": headSHA,
		"rerun":    rerunIDs,
		"skipped":  resp.Skipped,
		"failed":   resp.Failed,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryCIRetriggered,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		// The re-runs already happened; say so rather than implying nothing
		// was done, so the operator does not mistake this for a no-op.
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"retrigger-ci: append ci_retriggered audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"the workflow re-runs were requested but recording the ci_retriggered audit entry failed",
			map[string]any{"rerun": rerunIDs, "error": err.Error()})
		return
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}
