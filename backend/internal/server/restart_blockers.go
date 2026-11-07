package server

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// GET /v0/restart-blockers (E83.30 / #3974) answers one question for
// `scripts/dev reload` / `post-merge`: would restarting THIS daemon orphan
// work right now? It is a read-only PROJECTION of already-recorded state — it
// writes nothing and mints no audit entry — and reuses the orphaned-review
// boot sweep's own round tally (latestReviewStarted /
// countLandedReviewTerminals in review_reconcile.go) and its re-dispatch
// predicate (redispatchEligibility in review_redispatch.go, E72.59 / #4077)
// rather than re-deriving either. Long-form contract:
// backend/internal/server/README.md § "Restart blockers".
//
// Terminal-run discovery (ADR-140 D2-D5, #5047). A review started while its run
// was reporting a terminal state is on no state page, so the route also reads the
// newest started rows per review kind. Per-request read bound: two ListAll calls of
// at most terminalRoundScanLimit rows each, at most one memoized GetRun per distinct
// in-window candidate run, and for each visible terminal candidate its discovered
// stages' round reads; a round that reaches eligibility adds that predicate's own
// run, parent and stage reads. An expired row costs no GetRun. A row's account column
// is never evidence of visibility: a run is named only after candidateVisible
// accepts it, and an unloadable candidate answers one generic 503 carrying no
// identifier. Rounds whose budget outlives the 24-hour lookback, and rows past the
// cap, are stated residuals; truncated reports the cap.

// restartBlockersRunScanLimit bounds the non-terminal runs scanned per
// request (pending + running combined). A bite sets truncated=true.
const restartBlockersRunScanLimit = 500

// Restart-blocker reasons — a closed four-member set (merge_in_flight added
// by ADR-093 D8 / #4298) mirrored by the OpenAPI RestartBlocker.reason enum
// and parsed by scripts/dev's _parse_restart_blockers.
const (
	// restartBlockerUndispatchedChild: a decomposition child whose implement
	// stage is still pending / awaiting_host_dispatch AND whose decomposition
	// parent is not terminal. A child of a succeeded / failed / cancelled
	// parent can never be dispatched by that parent's fan-out, so a restart
	// strands nothing (#4184).
	restartBlockerUndispatchedChild = "undispatched_child"
	// restartBlockerReviewInFlight: an unsettled plan/implement review round a
	// restart would LOSE — one dispatched by THIS process that the next boot
	// sweep would NOT re-dispatch (redispatchEligibility says no), or an
	// orphaned round this process's boot sweep has handed to a re-dispatch
	// that has not settled yet (#4077). An eligible current-process round is
	// not a blocker: the next boot re-dispatches it. Nor is a proven no-consumer
	// round (run_terminal, ADR-140 D2): no verdict could be used.
	restartBlockerReviewInFlight = "review_in_flight"
	// restartBlockerCheckFailed: a per-run stage, parent-run or audit read
	// failed, so the daemon could not decide that run's state. scripts/dev treats it as a
	// REFUSING blocker (fail-closed on a reachable-but-undecided daemon).
	restartBlockerCheckFailed = "check_failed"
	// restartBlockerMergeInFlight (ADR-093 D8 / #4298): THIS process has the
	// run's head-of-line merge-queue merge in flight (s.mergeInFlight, marked
	// after the merging stamp and cleared after the pump applies the
	// outcome). A restart interrupts the forge merge call, so the next boot
	// must reconcile it and may hold it for the operator re-arm. A merging
	// entry a PRIOR process left is not a blocker: the next boot reconciles
	// it and a restart loses nothing more (the review-round rule's shape).
	restartBlockerMergeInFlight = "merge_in_flight"
)

// restartBlockerStageImplement is the stage an undispatched_child item names.
// Review items carry orphanedReviewStageKind.label ("plan" / "implement"), and
// a merge_in_flight item names restartBlockerStageMerge, so the OpenAPI
// RestartBlocker.stage enum is exactly those three labels.
const restartBlockerStageImplement = "implement"

// restartBlockerStageMerge is the stage a merge_in_flight item names: the
// queue's merge step, not a run stage row.
const restartBlockerStageMerge = "merge"

// restartBlockersResponse is the GET /v0/restart-blockers body.
type restartBlockersResponse struct {
	Items       []restartBlocker `json:"items"`
	ScannedRuns int              `json:"scanned_runs"`
	// Truncated is true when the candidate-run scan hit
	// restartBlockersRunScanLimit, so runs past the cap were not checked, or when
	// a terminal-run audit page reached its cap inside the discovery lookback, so
	// rounds past that cap were not checked.
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

// parentLookup is one memoized decomposition-parent read: the parent's state,
// or the error that read returned (including run.ErrNotFound). Caching the
// error keeps a failing parent from being re-read once per child.
type parentLookup struct {
	state run.State
	err   error
}

// handleListRestartBlockers implements GET /v0/restart-blockers.
//
// Auth mirrors handleListRuns exactly: anonymous callers are accepted (no
// scope check — scripts/dev calls it with no token), the caller's
// Identity.AccountID bounds the run scan, and the repo-visibility filter drops
// runs the caller cannot read, failing the whole request CLOSED (503) on a
// filter fault. The items expose only run ids, enum slugs and counts — a
// subset of what anonymous GET /v0/runs and /v0/runs/{id}/stages serve. Terminal
// discovery keeps that posture (ADR-140 D5): an anonymous caller with no account
// filter sees qualifying terminal rounds across accounts, with no added field.
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
	parents := map[uuid.UUID]parentLookup{}
	stateScanned := make(map[uuid.UUID]struct{}, len(candidates))
	for _, ru := range candidates {
		stateScanned[ru.ID] = struct{}{}
		allowed, ferr := filter.allows(ctx, ru.Repo)
		if ferr != nil {
			s.writeRepoFilterUnavailable(w, r)
			return
		}
		if !allowed {
			continue
		}
		resp.ScannedRuns++
		resp.Items = append(resp.Items, s.runRestartBlockers(ctx, ru, parents)...)
	}
	if !s.appendTerminalRunBlockers(w, r, &resp, filter, accountFilter, stateScanned) {
		return
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

// appendTerminalRunBlockers adds what terminal-run discovery finds (ADR-140 D3-D5).
// Audit discovery is candidate discovery only: a run that the state scan already
// listed is that scan's, a non-terminal run is too, and a terminal run contributes
// only after candidateVisible accepts it, so no parent is read for an invisible run.
// Checks (a) and (c) of runRestartBlockers stay non-terminal-only. false means an
// error was written: a list or candidate read failure is one generic 503 and the
// partial response is discarded.
func (s *Server) appendTerminalRunBlockers(w http.ResponseWriter, r *http.Request, resp *restartBlockersResponse, filter *repoFilter, accountFilter string, stateScanned map[uuid.UUID]struct{}) bool {
	ctx := r.Context()
	window := func(configured int) time.Duration {
		return min(s.planReviewBackstop(configured), terminalRoundDiscoveryBound)
	}
	scan, err := s.discoverTerminalRunRounds(ctx, accountFilter, window)
	if err != nil {
		s.writeRestartBlockersUnavailable(w, r, err)
		return false
	}
	if scan.truncated {
		resp.Truncated = true
	}
	// admitted memoizes one GetRun per distinct run; nil means skipped.
	admitted := map[uuid.UUID]*run.Run{}
	for _, candidate := range scan.candidates {
		if _, owned := stateScanned[candidate.runID]; owned {
			continue
		}
		ru, seen := admitted[candidate.runID]
		if !seen {
			loaded, err := s.cfg.RunRepo.GetRun(ctx, candidate.runID)
			if err != nil {
				s.writeRestartBlockersUnavailable(w, r, err)
				return false
			}
			if loaded.State.IsTerminal() {
				visible, verr := candidateVisible(ctx, filter, accountFilter, loaded)
				if verr != nil {
					s.writeRepoFilterUnavailable(w, r)
					return false
				}
				if visible {
					ru = loaded
					resp.ScannedRuns++
				}
			}
			admitted[candidate.runID] = ru
		}
		if ru == nil {
			continue
		}
		if blocker := s.reviewRoundBlocker(ctx, ru, candidate.stage, window); blocker != nil {
			resp.Items = append(resp.Items, *blocker)
		}
	}
	return true
}

// writeRestartBlockersUnavailable is the single generic 503 for a scan that could
// not decide (ADR-140 D5, D7). A candidate's identity and the raw error stay out of
// the body; the cause reaches only the server log. scripts/dev refuses on it.
func (s *Server) writeRestartBlockersUnavailable(w http.ResponseWriter, r *http.Request, cause error) {
	s.writeError(w, r, http.StatusServiceUnavailable, "service_unavailable",
		"could not complete the restart-blocker scan; retry shortly",
		map[string]any{internalCauseKey: cause.Error()})
}

// candidateVisible is the visibility a discovered run must pass before anything may
// name it (ADR-140 D5): the same account window and repository filter the state scan
// applies. The audit row's own account is not consulted, because a NULL-account row
// is never evidence of its run's visibility. An error is a filter fault.
func candidateVisible(ctx context.Context, filter *repoFilter, accountFilter string, ru *run.Run) (bool, error) {
	if !accountWindowAdmits(accountFilter, ru.AccountID) {
		return false, nil
	}
	return filter.allows(ctx, ru.Repo)
}

// accountWindowAdmits mirrors ListRuns' account clause, since a discovered run never
// passed through that query: an empty or malformed filter is no constraint, an equal
// account admits and a NULL (untenanted) run account admits. An unparseable run
// account matches nothing.
func accountWindowAdmits(accountFilter, runAccount string) bool {
	want, err := uuid.Parse(accountFilter)
	if err != nil {
		return true
	}
	if runAccount == "" {
		return true
	}
	got, err := uuid.Parse(runAccount)
	return err == nil && got == want
}

// runRestartBlockers derives every blocker one non-terminal run contributes.
// Each check is independent: a read failure yields a check_failed item for
// that check only and the others still run. parents memoizes the decomposition
// parent reads for one request, so N children of one parent cost one GetRun.
func (s *Server) runRestartBlockers(ctx context.Context, ru *run.Run, parents map[uuid.UUID]parentLookup) []restartBlocker {
	var out []restartBlocker
	runID := ru.ID.String()

	// (a) Undispatched decomposition child: restarting mid-fan-out strands a
	// child the operator (or run_children) has not dispatched yet. Only a child
	// of a NON-terminal parent counts: a child of a succeeded / failed /
	// cancelled parent can never be dispatched by that parent's fan-out, and
	// would otherwise wedge `scripts/dev post-merge` (#4184, defence in depth).
	// The #4186 cascade cancels a parent's non-terminal children only on the
	// CANCEL sinks, so this skip still covers (1) the undispatched children of a
	// FAILED or SUCCEEDED parent, which the cascade never touches, and (2)
	// children orphaned before `fishhawkd reconcile-orphan-children --apply`
	// runs. A parent read that fails — including run.ErrNotFound
	// across a concurrent delete, since decomposed_from is ON DELETE SET NULL —
	// is a check_failed item, never a silent drop.
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
					parent := s.restartBlockerParent(ctx, *ru.DecomposedFrom, parents)
					if parent.err != nil {
						out = append(out, restartBlocker{RunID: runID, Reason: restartBlockerCheckFailed, Stage: restartBlockerStageImplement})
						break
					}
					if parent.state.IsTerminal() {
						break
					}
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

	// (b) Review round a restart would lose. The round tally is the
	// orphaned-review reconcile's own (latest started round, verdicts landed
	// strictly after it), and the re-dispatch decision is the boot sweep's
	// own redispatchEligibility, so this surface and the boot sweep cannot
	// disagree about which rounds survive a restart (#4077).
	for _, stage := range orphanedReviewStages {
		if blocker := s.reviewRoundBlocker(ctx, ru, stage, nil); blocker != nil {
			out = append(out, *blocker)
		}
	}

	// (c) A head-of-line merge-queue merge THIS process has in flight
	// (ADR-093 D8 / #4298). The registry is process-local with no DB read, so
	// this check never yields check_failed.
	if s.mergeInFlight.has(ru.ID) {
		out = append(out, restartBlocker{RunID: runID, Reason: restartBlockerMergeInFlight, Stage: restartBlockerStageMerge})
	}
	return out
}

// reviewRoundBlocker is check (b) for one review stage: the blocker a restart would
// lose on that stage's ACTUAL latest round, or nil. A nil window is the state scan,
// which bounds nothing; terminal discovery passes the round's own window, so a settled
// or younger sibling that made the run a candidate cannot admit an expired round. A
// proven no-consumer round (run_terminal) is omitted, because no verdict could be used
// (ADR-140 D1, D2). Every other refusal keeps blocking.
func (s *Server) reviewRoundBlocker(ctx context.Context, ru *run.Run, stage orphanedReviewStageKind, window func(configured int) time.Duration) *restartBlocker {
	runID := ru.ID.String()
	checkFailed := &restartBlocker{RunID: runID, Reason: restartBlockerCheckFailed, Stage: stage.label}
	latest, payload, found, err := s.latestReviewStarted(ctx, ru.ID, stage)
	if err != nil {
		return checkFailed
	}
	if !found {
		return nil
	}
	landed, err := s.countLandedReviewTerminals(ctx, ru.ID, stage, latest.Sequence)
	if err != nil {
		return checkFailed
	}
	// Settled — or never pending: a round with no configured reviewer
	// (ConfiguredAgents <= 0) is settled at zero verdicts.
	if landed >= payload.ConfiguredAgents {
		return nil
	}
	configured := payload.ConfiguredAgents
	inFlight := &restartBlocker{
		RunID:            runID,
		Reason:           restartBlockerReviewInFlight,
		Stage:            stage.label,
		ConfiguredAgents: &configured,
		Landed:           &landed,
	}
	// A boot re-dispatch mid-handoff: the orphaned round predates this
	// process, so it is checked BEFORE the boot-marker skip. A restart
	// kills the re-dispatch goroutine, and the next boot finds the round
	// already named by its review_round_redispatched entry, so it closes
	// the round failed instead of re-dispatching it again.
	if reviewRedispatchPending(ru.ID, stage.label, latest.Sequence) {
		return inFlight
	}
	// A round dispatched BEFORE this process booted is not a blocker: its
	// goroutine is already dead and this process's boot sweep either
	// re-dispatched it (the pending check above) or closed it, so a
	// restart loses nothing more.
	if latest.Timestamp.Before(s.processStart) {
		return nil
	}
	if window != nil && !reviewRoundInWindow(time.Now(), ru.ID, stage, latest.Sequence, latest.Timestamp, window(payload.ConfiguredAgents)) {
		return nil
	}
	eligible, slug, err := s.redispatchEligibility(ctx, ru.ID, stage, latest, payload)
	if err != nil {
		return checkFailed
	}
	// The next boot sweep re-dispatches an eligible round against the
	// same plan or head, so a restart loses nothing.
	if eligible {
		return nil
	}
	if slug == redispatchSlugRunTerminal {
		return nil
	}
	return inFlight
}

// restartBlockerParent resolves a decomposition parent's state through the
// per-request memo, reading the run once per distinct parent and caching the
// error as well as the state.
func (s *Server) restartBlockerParent(ctx context.Context, parentID uuid.UUID, parents map[uuid.UUID]parentLookup) parentLookup {
	if p, ok := parents[parentID]; ok {
		return p
	}
	var p parentLookup
	if pr, err := s.cfg.RunRepo.GetRun(ctx, parentID); err != nil {
		p.err = err
	} else {
		p.state = pr.State
	}
	parents[parentID] = p
	return p
}
