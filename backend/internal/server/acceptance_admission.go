package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// acceptanceAdmissionWalkTimeout bounds the context-cancellable part of the
// pre-mutation phase of the short-circuit walk — the orchestrator's
// admissibility reads (GetRun / ListStagesForRun), which run under this deadline
// (#1936, binding condition 1). It does NOT bound the per-stage lock acquisition
// that precedes those reads: LockStageAdmission blocks on a plain, non-context-
// aware sync.Mutex.Lock(), so a goroutine parked behind a long-held lock waits
// past this deadline. The design still degrades safely — once the lock is
// acquired the first admissibility read fails fast on the by-then-expired
// context, so nothing mutates, and the lock hold is itself bounded by the
// holder's own DB/statement timeouts (the honest liveness backstop). It is a
// var, not a const, purely so the detached-completion test can shrink it to
// prove that a transition slower than this bound still fully settles: once
// TryShortCircuitAcceptance reaches its first state transition it re-detaches
// onto context.WithoutCancel with NO deadline, so this timeout can never cancel a
// walk mid-mutation.
var acceptanceAdmissionWalkTimeout = 30 * time.Second

// acceptanceAdmissionResponse is the 200 body of POST
// /v0/stages/{stage_id}/acceptance-admission (#1928). ShortCircuited is the
// only always-present field: true means the orchestrator settled the acceptance
// stage server-side (no runner needed), false means the caller should proceed to
// spawn as today. Kind/Basis/CriteriaTotal and the refreshed Stage are populated
// only on a short-circuit hit.
//
// NeedsTarget / TargetHosts / ExpectedHeadSHA (E48.6 / #1953) are additive fields
// on the short_circuited:false path: NeedsTarget is true when the approved plan
// requires LIVE validation (no short-circuit predicate matched) AND the spec
// declares egress target hosts, in which case TargetHosts carries the verbatim
// spec-declared hosts and ExpectedHeadSHA the resolved merge-candidate head SHA
// (possibly empty when ledger resolution fails). A dispatch verb reads this to
// probe the target FROM THE DISPATCH HOST and refuse a runner that would fail
// category-C acceptance_target_unreachable. The endpoint only REPORTS what is
// needed — it never probes (its network position differs from the operator host
// under a k8s deployment). All three are omitempty so a mixed old/new version
// degrades to current behavior.
type acceptanceAdmissionResponse struct {
	ShortCircuited  bool           `json:"short_circuited"`
	Kind            string         `json:"kind,omitempty"`
	Basis           string         `json:"basis,omitempty"`
	CriteriaTotal   int            `json:"criteria_total,omitempty"`
	Stage           *stageResponse `json:"stage,omitempty"`
	NeedsTarget     bool           `json:"needs_target,omitempty"`
	TargetHosts     []string       `json:"target_hosts,omitempty"`
	ExpectedHeadSHA string         `json:"expected_head_sha,omitempty"`
}

// handleAcceptanceAdmission implements POST
// /v0/stages/{stage_id}/acceptance-admission (#1928): the pre-spawn admission
// step a local host dispatch (fishhawk_dispatch_stage / fishhawk_run_stage /
// fishhawk_drive_run) calls BEFORE spawning a runner for an acceptance stage. It
// evaluates the approved plan's three disjoint short-circuit predicates via
// orchestrator.TryShortCircuitAcceptance; on a hit the acceptance stage settles
// straight to succeeded (a passed verdict / skip marker recorded, NO runner
// dispatched) and the response carries short_circuited:true + the refreshed
// stage.
//
// Auth mirrors handleRetryStage: an authenticated identity is required
// (401 anonymous), the write:stages scope gates a token identity (403), and an
// mcp:run:<uuid> subject may only admit stages within its own run (403
// cross-run). The endpoint reuses write:stages and adds no new scope, so the
// auth-change impact inventory is empty (AGENTS.md Auth-change-checklist).
//
// Fail-open by design (the reconciliation binding condition): a non-admissible
// stage state (already settled, mixed criteria, an un-wired orchestrator)
// returns short_circuited:false with NO warning — it is the normal no-op path,
// and the caller's own spawn path + guards handle everything else. Only a
// transport error (which the caller sees as a non-200) is treated as an
// admission-call error the caller fails open on with a warning.
func (s *Server) handleAcceptanceAdmission(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:stages") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages",
			map[string]any{"required_scope": "write:stages"})
		return
	}

	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "acceptance_admission_unconfigured",
			"acceptance-admission endpoint requires a configured run repository", nil)
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

	// Subject-binding guard: an mcp:run:<uuid> token may only admit stages
	// within its own run — mirrors handleRetryStage so an agent token can drive
	// its own dispatch admission but not a sibling run's.
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		subjectRunID, parseErr := uuid.Parse(strings.TrimPrefix(id.Subject, "mcp:run:"))
		if parseErr != nil {
			s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
				"mcp token subject is malformed", nil)
			return
		}
		if subjectRunID != stage.RunID {
			s.writeError(w, r, http.StatusForbidden, "cross_run_admission",
				"mcp token may only admit stages within its own run",
				map[string]any{
					"token_run_id": subjectRunID.String(),
					"stage_run_id": stage.RunID.String(),
				})
			return
		}
	}

	// A non-acceptance stage is a caller bug (the dispatch verbs only call this
	// for acceptance stages) — 422 rather than a silent false, so a misrouted
	// call is diagnosable.
	if stage.Type != run.StageTypeAcceptance {
		s.writeError(w, r, http.StatusUnprocessableEntity, "validation_failed",
			"acceptance-admission applies only to acceptance stages",
			map[string]any{"stage_id": stage.ID.String(), "stage_type": string(stage.Type)})
		return
	}

	// Fail-open: an un-wired orchestrator can never block a legitimate dispatch.
	// Admission is an evaluate-and-maybe-settle pre-step; the caller's own spawn
	// path handles everything else.
	if s.cfg.Orchestrator == nil {
		s.writeJSON(w, r, http.StatusOK, acceptanceAdmissionResponse{ShortCircuited: false})
		return
	}

	// Run the short-circuit under a bounded DETACHED context (binding condition 1,
	// #1936). net/http cancels r.Context() when the client disconnects, which could
	// otherwise abort the walk between its sequential transitions and wedge the
	// stage at an intermediate state. context.WithoutCancel preserves the request
	// values but drops cancellation; the WithTimeout adds a liveness bound on the
	// PRE-mutation phase only — TryShortCircuitAcceptance re-detaches onto a
	// no-deadline context at its point of no return, so an admission that begins its
	// state walk always runs to completion (settle + audit + Advance). Everything
	// before this (auth, stage load) and the post-walk refreshed-stage read keep
	// r.Context().
	walkCtx, cancelWalk := context.WithTimeout(context.WithoutCancel(r.Context()), acceptanceAdmissionWalkTimeout)
	defer cancelWalk()
	sc, liveValidationRequired, err := s.cfg.Orchestrator.TryShortCircuitAcceptance(walkCtx, stage.RunID, stage.ID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"acceptance admission failed", map[string]any{"error": err.Error()})
		return
	}
	if sc == nil {
		// No predicate matched, or the stage is in a non-admissible state — the
		// normal no-op path (short_circuited:false). When the approved plan
		// requires LIVE validation (liveValidationRequired) AND the spec declares
		// egress target hosts, augment the response with needs_target + the
		// verbatim hosts and the resolved merge-candidate head SHA (E48.6 / #1953)
		// so the dispatch verb probes the target before spawning a doomed runner.
		// The endpoint only REPORTS what is needed; it never probes (its network
		// position differs from the operator host under a k8s deployment). All
		// three fields stay omitted on a non-admissible / non-live no-op or a spec
		// with no declared hosts (the runner skips its target gate then anyway).
		resp := acceptanceAdmissionResponse{ShortCircuited: false}
		if liveValidationRequired {
			if runRow, gerr := s.cfg.RunRepo.GetRun(r.Context(), stage.RunID); gerr == nil {
				if hosts := s.resolveAcceptanceEgressTargetHosts(r.Context(), runRow); len(hosts) > 0 {
					resp.NeedsTarget = true
					resp.TargetHosts = hosts
					// Resolve the merge-candidate head, walking ParentRunID as a
					// fallback (#2028) so a plan-stageless recovery child — whose
					// implement lineage was recorded under the PARENT runID — carries
					// its ancestor's non-empty head rather than an empty one that
					// degrades the #1953 verb from a hard-block to proceed-with-warning.
					// May STILL be empty when the walk exhausts — still emit
					// needs_target; the verb degrades to a proceed-with-warning.
					resp.ExpectedHeadSHA = s.resolveAcceptanceExpectedHeadSHAWalkingParents(r.Context(), runRow, stage.ID)
				}
			}
		}
		s.writeJSON(w, r, http.StatusOK, resp)
		return
	}

	// Short-circuited: return the fired predicate + the refreshed (now terminal)
	// stage so the caller can render a settled-stage output without a runner.
	resp := acceptanceAdmissionResponse{
		ShortCircuited: true,
		Kind:           sc.Kind,
		Basis:          sc.Basis,
		CriteriaTotal:  sc.CriteriaTotal,
	}
	if refreshed, gerr := s.cfg.RunRepo.GetStage(r.Context(), stage.ID); gerr == nil {
		sr := toStageResponse(refreshed)
		resp.Stage = &sr
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}
