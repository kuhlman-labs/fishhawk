package server

// refinement_file.go implements the E34.3 filing executor endpoint (ADR-052
// filing half, #1594): POST /v0/refinement/sessions/{session_id}/file turns an
// approved, hash-pinned refinement draft into real tracker items (epic first,
// then children in wave order) over the EXISTING conventions/provider pipeline
// (applyAndFileWorkItem), then asserts the filed epic round-trips through the
// provider's EpicChildren + campaign.Assemble.
//
// It is gated behind the existing write:approvals scope (no new scope, empty
// auth-checklist impact inventory — E34.2's scopeRefinementGate precedent). The
// executor is idempotent: a per-draft filing session pins the repo and a
// durable row per filed item lets a re-invoke resume at the first unfiled
// ordinal. A per-draft advisory lock (refinement.ExecuteFiling -> WithFilingLock)
// serializes concurrent invocations so two POST /file calls cannot both file.
//
// AUDIT ORDERING (the E34.2 durable-before-state-change discipline): the
// refinement_filing_completed audit entry is appended on the GLOBAL chain
// BEFORE CompleteFilingSession flips completed_at, so an audit-append failure is
// a 500 with completed_at still NULL and the re-invoke retries the close.

import (
	"context"
	"errors"
	"net/http"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/refinement"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// fileRefinementSessionRequest is the POST body: the target repo (owner/name)
// the approved draft files into. Strict-decoded + bounded via decodeRefinementBody.
type fileRefinementSessionRequest struct {
	Repo string `json:"repo"`
}

// refinementFiledEpicView / refinementFiledChildView echo what landed.
type refinementFiledEpicView struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

type refinementFiledChildView struct {
	Ordinal int    `json:"ordinal"`
	Number  int    `json:"number"`
	URL     string `json:"url"`
}

// refinementFileResponse is the POST response: the filed epic + children, plus
// whether this invocation resumed a partial filing, replayed an already-completed
// session, and whether the EpicChildren round-trip verification passed.
type refinementFileResponse struct {
	SessionID        string                     `json:"session_id"`
	DraftID          string                     `json:"draft_id"`
	Repo             string                     `json:"repo"`
	Epic             refinementFiledEpicView    `json:"epic"`
	Children         []refinementFiledChildView `json:"children"`
	Resumed          bool                       `json:"resumed"`
	AlreadyCompleted bool                       `json:"already_completed"`
	Verified         bool                       `json:"verified"`
}

// handleFileRefinementSession implements POST
// /v0/refinement/sessions/{session_id}/file.
func (s *Server) handleFileRefinementSession(w http.ResponseWriter, r *http.Request) {
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

	var req fileRefinementSessionRequest
	if !s.decodeRefinementBody(w, r, &req) {
		return
	}

	drafts, ok := s.loadRefinementSession(w, r, sessionID)
	if !ok {
		return
	}
	decisions, err := s.cfg.RefinementRepo.ListDecisions(r.Context(), sessionID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load decisions", map[string]any{"error": err.Error()})
		return
	}

	// The E34.2 gate: a draft cannot be filed in any state except approved, and
	// the executor refuses a drifted draft (its content no longer matches the
	// pinned approval hash).
	approved, err := refinement.ApprovedDraft(drafts, decisions)
	if err != nil {
		switch {
		case errors.Is(err, refinement.ErrNotApproved):
			s.writeError(w, r, http.StatusConflict, "refinement_not_approved",
				"the session's latest revision is not approved", nil)
		case errors.Is(err, refinement.ErrDraftDrifted):
			s.writeError(w, r, http.StatusConflict, "refinement_draft_drifted",
				"the approved draft content has drifted from the decision; re-approve before filing", nil)
		default:
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"could not resolve the approved draft", map[string]any{"error": err.Error()})
		}
		return
	}

	owner, name, valid := splitRepoFullName(req.Repo)
	if !valid {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo must be in owner/name form", map[string]any{"field": "repo", "got": req.Repo})
		return
	}

	conv, err := conventionsLoader(req.Repo)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load work-management conventions", map[string]any{"error": err.Error()})
		return
	}

	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
		Jira:    conv.Jira,
	}
	// Resolve the App installation for the target repo, exactly like
	// handleFileWorkItem's run-absent operator path: an ErrNotInstalled leaves
	// InstallationID 0 so the provider fails closed with its own actionable
	// error; a transient resolution failure is surfaced as 502 here.
	if s.cfg.GitHub != nil {
		instID, rerr := s.cfg.GitHub.GetRepoInstallation(r.Context(), githubclient.RepoRef{Owner: owner, Name: name})
		switch {
		case rerr == nil:
			target.InstallationID = instID
		case errors.Is(rerr, githubclient.ErrNotInstalled):
			// Leave InstallationID 0; the provider fails closed on filing.
		default:
			s.writeError(w, r, http.StatusBadGateway, "refinement_filing_failed",
				"could not resolve the GitHub App installation for the target repo",
				map[string]any{"error": rerr.Error()})
			return
		}
	}

	// The FileItem seam: draft items ride exactly the hand-filed pipeline. The
	// executor passes TitleVars["epic"] explicitly (via FilingRequestForChild),
	// so deriveEpicTitleVar short-circuits with no per-child GetIssue.
	fileItem := func(ctx context.Context, filing workmgmt.FilingRequest) (int, string, error) {
		_, created, werr := s.applyAndFileWorkItem(ctx, filing, conv, target, owner, name)
		if werr != nil {
			return 0, "", errors.New(werr.msg)
		}
		return created.Number, created.URL, nil
	}

	outcome, err := refinement.ExecuteFiling(r.Context(), approved, req.Repo, s.cfg.RefinementRepo, fileItem)
	if err != nil {
		var partial *refinement.FilingPartialError
		switch {
		case errors.Is(err, refinement.ErrFilingRepoMismatch):
			s.writeError(w, r, http.StatusConflict, "refinement_filing_repo_mismatch",
				err.Error(), map[string]any{"requested_repo": req.Repo})
		case errors.As(err, &partial):
			s.writeError(w, r, http.StatusBadGateway, "refinement_filing_failed",
				"a work item could not be filed; the items filed so far are durable — re-invoke to resume",
				map[string]any{
					"failed_ordinal": partial.FailedOrdinal,
					"filed":          filedProgress(partial.Filed),
					"error":          partial.Err.Error(),
				})
		default:
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"could not execute filing", map[string]any{"error": err.Error()})
		}
		return
	}

	// On a fresh/resumed full fill (not a replay), verify the round-trip then
	// close the session audit-before-state-change. A replay of an
	// already-completed session performs no verification, audit, or complete —
	// it just replays the recorded result.
	verified := false
	if !outcome.AlreadyCompleted {
		var verr error
		verified, verr = s.verifyFiledEpic(r, conv, target, outcome)
		if verr != nil {
			s.writeError(w, r, http.StatusBadGateway, "refinement_filing_verification_failed",
				"the filed epic did not round-trip through provider verification; the items are durable — re-invoke to re-verify",
				map[string]any{"epic_number": outcome.Epic.IssueNumber, "error": verr.Error()})
			return
		}

		hash, herr := refinement.ContentHash(approved.Draft)
		if herr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"could not hash the approved draft", map[string]any{"error": herr.Error()})
			return
		}
		if err := s.appendRefinementAudit(r, "refinement_filing_completed", map[string]any{
			"session_id":    sessionID.String(),
			"draft_id":      approved.ID.String(),
			"content_hash":  hash,
			"repo":          req.Repo,
			"epic_number":   outcome.Epic.IssueNumber,
			"child_numbers": childNumbers(outcome),
			"verified":      verified,
		}); err != nil {
			s.writeRefinementAuditFailure(w, r, err)
			return
		}
		if err := s.cfg.RefinementRepo.CompleteFilingSession(r.Context(), approved.ID); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"could not close the filing session", map[string]any{"error": err.Error()})
			return
		}
	}

	s.writeJSON(w, r, http.StatusOK, refinementFileResponse{
		SessionID:        sessionID.String(),
		DraftID:          approved.ID.String(),
		Repo:             req.Repo,
		Epic:             refinementFiledEpicView{Number: outcome.Epic.IssueNumber, URL: outcome.Epic.IssueURL},
		Children:         childViews(outcome),
		Resumed:          outcome.Resumed,
		AlreadyCompleted: outcome.AlreadyCompleted,
		Verified:         verified,
	})
}

// verifyFiledEpic resolves the provider and, when it implements the optional
// EpicChildrenQuerier capability, runs the filed-epic round-trip assertion. A
// provider WITHOUT the capability skips verification (verified=false, fail-open
// on a pure read-back — never on filing). A verification error is returned so
// the handler surfaces it as 502 before the audit/complete close.
func (*Server) verifyFiledEpic(r *http.Request, conv workmgmt.Conventions, target workmgmt.Target, outcome *refinement.FilingOutcome) (bool, error) {
	provider, err := workmgmt.Get(conv.Provider)
	if err != nil {
		// The provider must exist (filing just succeeded through it), but if it
		// cannot be resolved, skip verification fail-open — the items are durable.
		return false, nil
	}
	querier, ok := provider.(workmgmt.EpicChildrenQuerier)
	if !ok {
		return false, nil
	}
	if err := refinement.VerifyFiledEpic(r.Context(), querier, target, outcome.Epic.IssueNumber, outcome.FiledMap()); err != nil {
		return false, err
	}
	return true, nil
}

// childViews renders the outcome children as response views (ordinal order).
func childViews(o *refinement.FilingOutcome) []refinementFiledChildView {
	out := make([]refinementFiledChildView, 0, len(o.Children))
	for _, c := range o.Children {
		out = append(out, refinementFiledChildView{Ordinal: c.Ordinal, Number: c.IssueNumber, URL: c.IssueURL})
	}
	return out
}

// childNumbers is the child issue numbers (ordinal order) for the audit payload.
func childNumbers(o *refinement.FilingOutcome) []int {
	out := make([]int, 0, len(o.Children))
	for _, c := range o.Children {
		out = append(out, c.IssueNumber)
	}
	return out
}

// filedProgress renders the filed-so-far items for a partial-failure error's
// details, so a re-invoke's caller can see what already landed.
func filedProgress(filed []refinement.FiledResult) []map[string]any {
	out := make([]map[string]any, 0, len(filed))
	for _, f := range filed {
		out = append(out, map[string]any{"ordinal": f.Ordinal, "number": f.IssueNumber, "url": f.IssueURL})
	}
	return out
}
