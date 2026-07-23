package server

// refinement.go implements the E34.2 preview + approval gate over E34.1's
// refinement drafts (ADR-052 option A, #1593): a session-keyed HTTP surface to
// draft from a brief, preview the full filing (epic + children + wave DAG),
// edit (agent brief-amendment or direct field edit), and approve/reject with an
// audited decision. Approval is load-bearing: a decision pins the decided
// revision id + a content hash of the decoded EpicDraft, and session state is
// DERIVED (refinement.ResolveState) — an edit after approval lands a new
// revision that structurally invalidates the approval.
//
// AUDIT ORDERING (operator binding condition, gpt-5.5's HIGH concern): no gate
// action persists unaudited. Every edit / decision appends its audit entry via
// AppendGlobalChained BEFORE the refinement write (durable-before-state-change,
// the concern_waived pattern). An audit-append failure is a 500 and NOTHING is
// persisted — so an injected audit failure leaves session state unchanged and
// no approval usable by ApprovedDraft. The edit path pre-generates the new
// revision's id so the audit entry can name draft_id before the row exists.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/refinement"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// RefinementDrafter runs the E34.1 drafting agent for a brief. Its signature
// matches (*refinement.Drafter).Draft exactly, so the concrete Drafter
// satisfies it structurally and tests can fake it. Only the agent-backed arms
// (create-session, brief-amendment) call it.
type RefinementDrafter interface {
	Draft(ctx context.Context, sessionID uuid.UUID, brief string) (refinement.EpicDraft, string, error)
}

const (
	// scopeRefinementGate is the write scope every refinement-gate route
	// requires — the intake analogue of the plan-approval gate. Reusing the
	// existing write:approvals scope means no new token scope and an empty
	// auth-checklist impact inventory.
	scopeRefinementGate = "write:approvals"

	// defaultMaxBriefAmendments caps the number of agent brief-amendment
	// revisions per session, mirroring the revise_plan ceiling. Counted from
	// origin='amendment' draft rows; a further amendment returns 409.
	defaultMaxBriefAmendments = 3

	// refinementMaxBodyBytes bounds a refinement request body. Briefs and
	// direct-edit drafts are prose-and-JSON, comfortably under 1 MiB.
	refinementMaxBodyBytes = 1 << 20

	// refinementDraftBudget bounds the detached agent-backed drafting arms
	// (create-session, brief-amendment). The drafter's claudecode client has
	// Timeout=0, so this WithTimeout deadline is what invokeOnce honors. It
	// matches planreview's review-budget Cap (1200s /
	// backend/internal/planreview/budget.go). The MCP client's long timeout
	// (refinementDraftClientTimeout, 22m) sits above this so the server's own
	// bounded error surfaces before the client aborts.
	refinementDraftBudget = 20 * time.Minute
)

// ---- wire types -----------------------------------------------------------

type createRefinementSessionRequest struct {
	Brief string `json:"brief"`
}

type patchRefinementDraftRequest struct {
	// Exactly one of the two arms must be set. BriefAmendment re-runs the
	// Drafter over the composed brief; Draft is a direct strict-decoded edit.
	BriefAmendment string          `json:"brief_amendment,omitempty"`
	Draft          json.RawMessage `json:"draft,omitempty"`
}

type decideRefinementSessionRequest struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

type refinementDecisionView struct {
	Decision         string    `json:"decision"`
	Reason           string    `json:"reason"`
	DraftID          uuid.UUID `json:"draft_id"`
	DraftContentHash string    `json:"draft_content_hash"`
	DecidedBy        string    `json:"decided_by,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// refinementSessionView is the GET (and create/patch/decision) response: the
// derived approval state, the revision count, the latest structured draft, the
// full filing preview (epic + every child as it would file), the wave DAG, and
// the decision history.
type refinementSessionView struct {
	SessionID     uuid.UUID            `json:"session_id"`
	State         string               `json:"state"`
	Drifted       bool                 `json:"drifted,omitempty"`
	RevisionCount int                  `json:"revision_count"`
	LatestOrigin  string               `json:"latest_origin"`
	LatestDraft   refinement.EpicDraft `json:"latest_draft"`
	Preview       []workmgmt.WorkItem  `json:"preview"`
	Waves         [][]int              `json:"waves"`
	// CriteriaPrecheck is the E34.5 advisory acceptance-criteria pre-check over
	// the latest draft's children (#1596): per-child findings plus a
	// needs_attention marker. Always present (derived per response, nothing
	// persisted) so a clean draft is distinguishable (empty findings) from
	// never-checked. A no_blocking_criterion flag is advisory — the operator can
	// still approve.
	CriteriaPrecheck refinement.CriteriaPrecheck `json:"criteria_precheck"`
	Decisions        []refinementDecisionView    `json:"decisions"`
}

// ---- handlers -------------------------------------------------------------

// handleCreateRefinementSession implements POST /v0/refinement/sessions: mint a
// session, run the Drafter over the brief, persist the initial revision
// (origin=brief), and return the session view. Needs both the repository and
// the (agent-backed) drafter.
func (s *Server) handleCreateRefinementSession(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, scopeRefinementGate) {
		return
	}
	if !s.refinementRepoConfigured(w, r) {
		return
	}
	if s.cfg.RefinementDrafter == nil {
		s.writeRefinementDraftingUnavailable(w, r)
		return
	}

	var req createRefinementSessionRequest
	if !s.decodeRefinementBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Brief) == "" {
		s.writeError(w, r, http.StatusUnprocessableEntity, "validation_failed",
			"brief is required", map[string]any{"field": "brief"})
		return
	}

	// Detach drafting+persist from the request lifetime (#584 precedent,
	// plan.go:1094). The drafting agent runs for minutes; if the MCP client
	// disconnects, net/http cancels r.Context(), which would SIGKILL the drafter
	// mid-inference AND — because the open arm persists only after Draft returns
	// — strand a half-created session. context.WithoutCancel keeps the auth
	// identity values (IdentityFrom still resolves) but is not cancelled with the
	// parent; WithTimeout bounds the otherwise-unbounded drafter. Every
	// downstream call reads r.Context(), so this single rebind covers Draft,
	// CreateDraft, and the response render.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), refinementDraftBudget)
	defer cancel()
	r = r.WithContext(ctx)

	sessionID := uuid.New()
	draft, model, err := s.cfg.RefinementDrafter.Draft(r.Context(), sessionID, req.Brief)
	if err != nil {
		s.writeError(w, r, http.StatusBadGateway, "refinement_drafting_failed",
			"the drafting agent failed to produce a valid draft",
			map[string]any{"error": err.Error()})
		return
	}

	// The initial draft is not a gate action, so it carries no audit entry
	// (only edits and decisions do). Persist directly.
	if _, err := s.cfg.RefinementRepo.CreateDraft(r.Context(), refinement.CreateParams{
		SessionID: sessionID,
		Brief:     req.Brief,
		Draft:     draft,
		Model:     model,
		Origin:    refinement.OriginBrief,
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not persist the refinement draft", map[string]any{"error": err.Error()})
		return
	}

	s.respondRefinementSession(w, r, http.StatusCreated, sessionID)
}

// handleGetRefinementSession implements GET /v0/refinement/sessions/{id}.
func (s *Server) handleGetRefinementSession(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, scopeRefinementGate) {
		return
	}
	if !s.refinementRepoConfigured(w, r) {
		return
	}
	sessionID, ok := s.parseSessionID(w, r)
	if !ok {
		return
	}
	// A session with no revisions is unknown; respondRefinementSession re-loads
	// the current drafts + decisions to build the view.
	if _, ok := s.loadRefinementSession(w, r, sessionID); !ok {
		return
	}
	s.respondRefinementSession(w, r, http.StatusOK, sessionID)
}

// handlePatchRefinementDraft implements PATCH
// /v0/refinement/sessions/{id}/draft: exactly one of brief_amendment (re-run
// the Drafter) or draft (direct strict-decoded edit). Each successful edit
// appends a NEW revision — invalidating a prior approval — and writes a
// refinement_draft_edited audit entry BEFORE the persist.
func (s *Server) handlePatchRefinementDraft(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, scopeRefinementGate) {
		return
	}
	if !s.refinementRepoConfigured(w, r) {
		return
	}
	sessionID, ok := s.parseSessionID(w, r)
	if !ok {
		return
	}
	drafts, ok := s.loadRefinementSession(w, r, sessionID)
	if !ok {
		return
	}

	var req patchRefinementDraftRequest
	if !s.decodeRefinementBody(w, r, &req) {
		return
	}
	hasAmendment := strings.TrimSpace(req.BriefAmendment) != ""
	hasDraft := len(req.Draft) > 0
	if hasAmendment == hasDraft {
		s.writeError(w, r, http.StatusUnprocessableEntity, "validation_failed",
			"provide exactly one of brief_amendment or draft", nil)
		return
	}

	latest := drafts[len(drafts)-1]
	var newDraft refinement.EpicDraft
	var newBrief, model, origin string

	if hasAmendment {
		if countRefinementAmendments(drafts) >= defaultMaxBriefAmendments {
			s.writeError(w, r, http.StatusConflict, "amendment_budget_exhausted",
				"the per-session brief-amendment budget is exhausted",
				map[string]any{"max": defaultMaxBriefAmendments})
			return
		}
		if s.cfg.RefinementDrafter == nil {
			s.writeRefinementDraftingUnavailable(w, r)
			return
		}
		// Detach the agent re-draft+persist from the request lifetime, exactly as
		// the open arm does — same disconnect-survival rationale. Plain `r =`
		// assignment so the shared audit+persist tail after the if/else inherits
		// the detached, budgeted context. The direct-edit arm below keeps the
		// original request context (no agent call, nothing to survive).
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), refinementDraftBudget)
		defer cancel()
		r = r.WithContext(ctx)
		newBrief = strings.TrimRight(latest.Brief, "\n") + "\n\n" + strings.TrimSpace(req.BriefAmendment)
		d, m, err := s.cfg.RefinementDrafter.Draft(r.Context(), sessionID, newBrief)
		if err != nil {
			s.writeError(w, r, http.StatusBadGateway, "refinement_drafting_failed",
				"the drafting agent failed to produce a valid draft",
				map[string]any{"error": err.Error()})
			return
		}
		newDraft, model, origin = d, m, refinement.OriginAmendment
	} else {
		// Direct field edit: strict decode + Validate, no agent call. The stored
		// brief carries forward unchanged.
		d, err := refinement.DecodeDraft(req.Draft)
		if err != nil {
			s.writeError(w, r, http.StatusUnprocessableEntity, "validation_failed",
				"draft is not a valid EpicDraft", map[string]any{"error": err.Error()})
			return
		}
		if err := d.Validate(); err != nil {
			s.writeError(w, r, http.StatusUnprocessableEntity, "validation_failed",
				"draft failed validation", map[string]any{"error": err.Error()})
			return
		}
		newDraft, newBrief, origin = d, latest.Brief, refinement.OriginEdit
	}

	hash, err := refinement.ContentHash(newDraft)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not hash the edited draft", map[string]any{"error": err.Error()})
		return
	}

	// Pre-generate the new revision id so the durable-before-state-change audit
	// entry can name draft_id. revision is 1-based over the existing count.
	newID := uuid.New()
	revision := len(drafts) + 1
	if err := s.appendRefinementAudit(r, "refinement_draft_edited", map[string]any{
		"session_id":   sessionID.String(),
		"draft_id":     newID.String(),
		"revision":     revision,
		"origin":       origin,
		"content_hash": hash,
	}); err != nil {
		s.writeRefinementAuditFailure(w, r, err)
		return
	}

	if _, err := s.cfg.RefinementRepo.CreateDraft(r.Context(), refinement.CreateParams{
		ID:        newID,
		SessionID: sessionID,
		Brief:     newBrief,
		Draft:     newDraft,
		Model:     model,
		Origin:    origin,
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not persist the edited revision", map[string]any{"error": err.Error()})
		return
	}

	s.respondRefinementSession(w, r, http.StatusOK, sessionID)
}

// handleDecideRefinementSession implements POST
// /v0/refinement/sessions/{id}/decision: record an approve/reject pinning the
// latest revision id + content hash, writing the decision audit entry BEFORE
// the persist.
func (s *Server) handleDecideRefinementSession(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, scopeRefinementGate) {
		return
	}
	if !s.refinementRepoConfigured(w, r) {
		return
	}
	sessionID, ok := s.parseSessionID(w, r)
	if !ok {
		return
	}
	drafts, ok := s.loadRefinementSession(w, r, sessionID)
	if !ok {
		return
	}

	var req decideRefinementSessionRequest
	if !s.decodeRefinementBody(w, r, &req) {
		return
	}
	if req.Decision != refinement.DecisionApproved && req.Decision != refinement.DecisionRejected {
		s.writeError(w, r, http.StatusUnprocessableEntity, "validation_failed",
			"decision must be one of approved, rejected",
			map[string]any{"field": "decision", "got": req.Decision})
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		s.writeError(w, r, http.StatusUnprocessableEntity, "validation_failed",
			"a decision reason is required", map[string]any{"field": "reason"})
		return
	}

	decisions, err := s.cfg.RefinementRepo.ListDecisions(r.Context(), sessionID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load decisions", map[string]any{"error": err.Error()})
		return
	}
	res, err := refinement.ResolveState(drafts, decisions)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not resolve session state", map[string]any{"error": err.Error()})
		return
	}
	// The latest revision already carries a decision: re-gate by editing, not by
	// deciding twice.
	if res.Decision != nil {
		s.writeError(w, r, http.StatusConflict, "decision_already_recorded",
			"the latest revision already carries a decision", nil)
		return
	}

	latest := drafts[len(drafts)-1]
	hash, err := refinement.ContentHash(latest.Draft)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not hash the draft", map[string]any{"error": err.Error()})
		return
	}
	subject := IdentityFrom(r.Context()).Subject

	category := "refinement_draft_approved"
	if req.Decision == refinement.DecisionRejected {
		category = "refinement_draft_rejected"
	}
	if err := s.appendRefinementAudit(r, category, map[string]any{
		"session_id":   sessionID.String(),
		"draft_id":     latest.ID.String(),
		"revision":     len(drafts),
		"content_hash": hash,
		"reason":       req.Reason,
	}); err != nil {
		s.writeRefinementAuditFailure(w, r, err)
		return
	}

	if _, err := s.cfg.RefinementRepo.RecordDecision(r.Context(), refinement.DecisionParams{
		SessionID:        sessionID,
		DraftID:          latest.ID,
		Decision:         req.Decision,
		Reason:           req.Reason,
		DraftContentHash: hash,
		DecidedBy:        subject,
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not persist the decision", map[string]any{"error": err.Error()})
		return
	}

	s.respondRefinementSession(w, r, http.StatusOK, sessionID)
}

// ---- helpers --------------------------------------------------------------

func (s *Server) refinementRepoConfigured(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.RefinementRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "refinement_repo_unconfigured",
			"refinement endpoint requires a configured refinement repository", nil)
		return false
	}
	return true
}

func (s *Server) writeRefinementDraftingUnavailable(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, r, http.StatusServiceUnavailable, "refinement_drafting_unavailable",
		"agent-backed refinement drafting is not configured", nil)
}

func (s *Server) writeRefinementAuditFailure(w http.ResponseWriter, r *http.Request, err error) {
	// The audit entry IS the gate's record, so a failed append fails the request
	// — never a silent approval/edit. Nothing was persisted (audit runs first).
	s.writeError(w, r, http.StatusInternalServerError, "audit_append_failed",
		"could not durably record the gate action; nothing was persisted",
		map[string]any{"error": err.Error()})
}

// parseSessionID reads and validates the {session_id} path value.
func (s *Server) parseSessionID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := r.PathValue("session_id")
	id, err := uuid.Parse(raw)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"session_id must be a UUID", map[string]any{"got": raw})
		return uuid.Nil, false
	}
	return id, true
}

// loadRefinementSession returns the session's ordered draft revisions, or
// writes 404 when the session is unknown (no revisions).
func (s *Server) loadRefinementSession(w http.ResponseWriter, r *http.Request, sessionID uuid.UUID) ([]*refinement.StoredDraft, bool) {
	drafts, err := s.cfg.RefinementRepo.ListForSession(r.Context(), sessionID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load the refinement session", map[string]any{"error": err.Error()})
		return nil, false
	}
	if len(drafts) == 0 {
		s.writeError(w, r, http.StatusNotFound, "refinement_session_not_found",
			"no refinement session with that id", map[string]any{"session_id": sessionID.String()})
		return nil, false
	}
	// Repo-scoped gate (ADR-057 Amendment A2 / #2071). Applied HERE rather
	// than per handler because every refinement point read and write —
	// GET /sessions/{id}, PATCH .../draft, POST .../decision — routes through
	// this loader, so one check covers the surface and a new handler inherits
	// it by construction. Refinement is a POINT-read surface, so a non-visible
	// session 403s repo_forbidden rather than being filtered away.
	if !s.enforceRefinementRepoVisibility(w, r, drafts) {
		return nil, false
	}
	return drafts, true
}

// enforceRefinementRepoVisibility applies repo filtering to a refinement
// session. Returns true when the request may proceed.
//
// A refinement session's repo is the repo its FILING session pinned. That is
// the session's only durable repo: refinement.StoredDraft carries brief,
// draft, model and origin — no repo — because a session starts life as a
// brief, before any target repo has been chosen. The repo is pinned exactly
// once, at first filing invoke (refinement.FilingSession.Repo, E34.3 / #1594),
// and a re-invoke naming a different repo fails closed there.
//
// So the gate is: a session that HAS been filed authorizes against its pinned
// repo; a session that has NOT been filed has no repo to authorize against and
// stays reachable to any member holding the refinement gate scope, exactly as
// today. That is a deliberate, stated limit rather than a silent one — closing
// it means giving refinement sessions a repo at creation, which is a schema
// change to the refinement tables (outside this change's scope). The residual
// exposure is bounded: a pre-filing session contains a brief and a proposed
// epic draft, not run, campaign or audit data for a repo the caller lacks.
//
// The filing session is keyed by DRAFT id, and only the approved revision is
// filed, so this walks the session's revisions newest-first and gates on the
// first pinned repo it finds — at most one revision per session is ever filed.
func (s *Server) enforceRefinementRepoVisibility(w http.ResponseWriter, r *http.Request, drafts []*refinement.StoredDraft) bool {
	filter, ok := s.requestRepoFilter(w, r)
	if !ok {
		return false
	}
	if filter == nil {
		// No mirror wired, bearer / anonymous caller, or a workspace admin —
		// the admin bypass covers refinement just as it covers lists and
		// point reads.
		return true
	}
	repo, found, err := s.refinementSessionRepo(r.Context(), drafts)
	if err != nil {
		// A filing-session lookup fault is a STORE fault: the gate cannot
		// function, so 503 rather than guess in either direction.
		s.writeRepoFilterUnavailable(w, r)
		return false
	}
	if !found {
		return true
	}
	return s.repoVisibleOr403(w, r, filter, repo)
}

// refinementSessionRepo returns the repo pinned by the session's filing
// session, if one has been opened. found=false means the session has not been
// filed and therefore has no repo.
func (s *Server) refinementSessionRepo(ctx context.Context, drafts []*refinement.StoredDraft) (string, bool, error) {
	for i := len(drafts) - 1; i >= 0; i-- {
		fs, err := s.cfg.RefinementRepo.GetFilingSession(ctx, drafts[i].ID)
		if errors.Is(err, refinement.ErrNotFound) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if fs != nil && fs.Repo != "" {
			return fs.Repo, true, nil
		}
	}
	return "", false, nil
}

// decodeRefinementBody strict-decodes a bounded request body into dst, writing
// a 400 on malformed JSON / unknown fields.
func (s *Server) decodeRefinementBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, refinementMaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return false
	}
	return true
}

// countRefinementAmendments counts the revisions whose origin is 'amendment' —
// the brief-amendment budget denominator.
func countRefinementAmendments(drafts []*refinement.StoredDraft) int {
	n := 0
	for _, d := range drafts {
		if d.Origin == refinement.OriginAmendment {
			n++
		}
	}
	return n
}

// appendRefinementAudit writes one gate audit entry on the GLOBAL chain (a
// refinement session is not a run). ActorUser + the auth subject. Returns the
// append error so the caller can fail the request BEFORE persisting.
func (s *Server) appendRefinementAudit(r *http.Request, category string, payload map[string]any) error {
	if s.cfg.AuditRepo == nil {
		return errors.New("audit repository not configured")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	userKind := audit.ActorUser
	subject := IdentityFrom(r.Context()).Subject
	var subjectPtr *string
	if subject != "" {
		subjectPtr = &subject
	}
	_, err = s.cfg.AuditRepo.AppendGlobalChained(r.Context(), audit.GlobalChainAppendParams{
		Timestamp:    time.Now().UTC(),
		Category:     category,
		ActorKind:    &userKind,
		ActorSubject: subjectPtr,
		Payload:      body,
		AccountID:    identityAccountID(r.Context()),
	})
	return err
}

// respondRefinementSession loads the session's current drafts + decisions,
// derives the view (state, revision count, preview, wave DAG, decision
// history), and writes it at status.
func (s *Server) respondRefinementSession(w http.ResponseWriter, r *http.Request, status int, sessionID uuid.UUID) {
	drafts, err := s.cfg.RefinementRepo.ListForSession(r.Context(), sessionID)
	if err != nil || len(drafts) == 0 {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load the refinement session for the response", nil)
		return
	}
	decisions, err := s.cfg.RefinementRepo.ListDecisions(r.Context(), sessionID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load decisions for the response", map[string]any{"error": err.Error()})
		return
	}
	res, err := refinement.ResolveState(drafts, decisions)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not resolve session state", map[string]any{"error": err.Error()})
		return
	}

	latest := drafts[len(drafts)-1]
	preview, err := refinement.RenderDraft(latest.Draft, refinement.RenderOptions{}, workmgmt.Default())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not render the filing preview", map[string]any{"error": err.Error()})
		return
	}
	waves, err := latest.Draft.Waves()
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not compute the wave DAG", map[string]any{"error": err.Error()})
		return
	}

	view := refinementSessionView{
		SessionID:        sessionID,
		State:            string(res.State),
		Drifted:          res.Drifted,
		RevisionCount:    len(drafts),
		LatestOrigin:     latest.Origin,
		LatestDraft:      latest.Draft,
		Preview:          preview,
		Waves:            waves,
		CriteriaPrecheck: refinement.EvaluateDraftCriteria(latest.Draft),
		Decisions:        make([]refinementDecisionView, 0, len(decisions)),
	}
	for _, d := range decisions {
		view.Decisions = append(view.Decisions, refinementDecisionView{
			Decision:         d.Decision,
			Reason:           d.Reason,
			DraftID:          d.DraftID,
			DraftContentHash: d.DraftContentHash,
			DecidedBy:        d.DecidedBy,
			CreatedAt:        d.CreatedAt,
		})
	}
	s.writeJSON(w, r, status, view)
}
