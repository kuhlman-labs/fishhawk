package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// maxWorkItemRequestBytes caps the filing request body. Bodies carry a
// summary plus optional per-section markdown, so the cap is generous
// but bounded.
const maxWorkItemRequestBytes = 64 * 1024

// categoryWorkItemFiled is the audit category written when a work item
// is filed while a run is in flight (#1005). Documented in
// docs/issue-comment-surfaces.md.
const categoryWorkItemFiled = "work_item_filed"

// conventionsLoader resolves the work-management conventions for a repo.
// v0 returns the shipped default (the conventions are the value, and the
// default seeds the kuhlman-labs/fishhawk Project #7 conventions); a
// per-repo override loader that fetches `.fishhawk/work-management.yaml`
// from the repo is a follow-up. Declared as a package var so tests can
// inject conventions (e.g. an unimplemented-provider config) without a
// GitHub round-trip.
var conventionsLoader = func(_ string) (workmgmt.Conventions, error) {
	return workmgmt.Default(), nil
}

// workItemRequest is the POST /v0/work-items body: the provider-neutral
// caller input the conventions layer turns into a filed item. `summary`
// is the mandatory one-liner (it both fills the title_format {summary}
// placeholder and is the required Summary field); everything else is
// optional and conventions-resolved. `run_id` is optional and drives the
// best-effort work_item_filed audit when the named run is in flight.
type workItemRequest struct {
	Repo            string             `json:"repo"`
	Type            string             `json:"type"`
	Summary         string             `json:"summary"`
	Body            string             `json:"body,omitempty"`
	Sections        map[string]string  `json:"sections,omitempty"`
	TitleVars       map[string]string  `json:"title_vars,omitempty"`
	Labels          []string           `json:"labels,omitempty"`
	Complexity      string             `json:"complexity,omitempty"`
	Status          string             `json:"status,omitempty"`
	Relations       *workItemRelations `json:"relations,omitempty"`
	ExistingNumbers []int              `json:"existing_numbers,omitempty"`
	RunID           string             `json:"run_id,omitempty"`
}

// workItemRelations mirrors workmgmt.Relations over the wire.
type workItemRelations struct {
	ParentEpic   string   `json:"parent_epic,omitempty"`
	Supersedes   []string `json:"supersedes,omitempty"`
	CompanionTo  []string `json:"companion_to,omitempty"`
	EvidenceRuns []string `json:"evidence_runs,omitempty"`
}

// workItemResponse echoes exactly what landed: the created item's
// number/URL plus the conventions-resolved placement and labels, so the
// caller (MCP tool, CLI verb) can render the result without a second
// fetch. `audited` reports whether a work_item_filed audit entry was
// written (true only when a run was in flight).
type workItemResponse struct {
	Type          string   `json:"type"`
	Title         string   `json:"title"`
	Number        int      `json:"number"`
	URL           string   `json:"url"`
	Provider      string   `json:"provider"`
	AppliedLabels []string `json:"applied_labels,omitempty"`
	Complexity    string   `json:"complexity,omitempty"`
	Status        string   `json:"status,omitempty"`
	BoardColumn   string   `json:"board_column,omitempty"`
	Audited       bool     `json:"audited"`
}

// handleFileWorkItem implements POST /v0/work-items.
//
// It loads the repo's work-management conventions, applies them to the
// caller's filing request (rendering the title, assembling the body,
// merging labels, resolving board placement and ADR numbering, and
// validating relations), dispatches the resolved item to the registered
// provider, and returns the created item. When `run_id` names a run that
// is in flight, it also writes a best-effort work_item_filed audit entry
// onto that run (#1005). An unimplemented/unregistered provider fails
// closed with a typed error naming the missing provider — never a nil
// dispatch.
func (s *Server) handleFileWorkItem(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"filing a work item requires an authenticated caller", nil)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxWorkItemRequestBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return
	}
	if len(raw) > maxWorkItemRequestBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"request body exceeds size cap", map[string]any{"limit_bytes": maxWorkItemRequestBytes})
		return
	}

	var req workItemRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON for a work-item filing",
			map[string]any{"error": err.Error()})
		return
	}

	owner, name, ok := splitRepoFullName(req.Repo)
	if !ok {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo must be in owner/name form",
			map[string]any{"field": "repo", "got": req.Repo})
		return
	}
	if strings.TrimSpace(req.Type) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"type is required", map[string]any{"field": "type"})
		return
	}
	if strings.TrimSpace(req.Summary) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"summary is required", map[string]any{"field": "summary"})
		return
	}

	conv, err := conventionsLoader(req.Repo)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load work-management conventions", map[string]any{"error": err.Error()})
		return
	}

	filing := workmgmt.FilingRequest{
		Type:            req.Type,
		Summary:         req.Summary,
		Body:            req.Body,
		Sections:        req.Sections,
		TitleVars:       req.TitleVars,
		Labels:          req.Labels,
		Complexity:      req.Complexity,
		Status:          req.Status,
		ExistingNumbers: req.ExistingNumbers,
	}
	if req.Relations != nil {
		filing.Relations = workmgmt.Relations{
			ParentEpic:   req.Relations.ParentEpic,
			Supersedes:   req.Relations.Supersedes,
			CompanionTo:  req.Relations.CompanionTo,
			EvidenceRuns: req.Relations.EvidenceRuns,
		}
	}

	item, number, err := workmgmt.Apply(filing, conv)
	if err != nil {
		var sem *workmgmt.SemanticError
		if errors.As(err, &sem) {
			s.writeError(w, r, http.StatusUnprocessableEntity, "work_item_invalid",
				sem.Error(), map[string]any{"type": req.Type})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not apply work-management conventions", map[string]any{"error": err.Error()})
		return
	}

	provider, err := workmgmt.Get(conv.Provider)
	if err != nil {
		var unk *workmgmt.UnknownProviderError
		if errors.As(err, &unk) {
			// Fail closed: an unimplemented provider (jira is interface-only
			// in v0) or a config typo names the missing id rather than
			// panicking on a nil dispatch.
			s.writeError(w, r, http.StatusNotImplemented, "provider_unimplemented",
				unk.Error(),
				map[string]any{"provider": unk.ID, "registered": unk.Known})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not resolve work-item provider", map[string]any{"error": err.Error()})
		return
	}

	// Resolve the optional active run up front: it supplies the
	// installation id the provider needs to act on the repo and is the
	// target of the work_item_filed audit.
	var activeRun *run.Run
	if strings.TrimSpace(req.RunID) != "" {
		if s.cfg.RunRepo == nil {
			s.writeError(w, r, http.StatusServiceUnavailable, "run_lookup_unconfigured",
				"run_id supplied but no run repository is configured", nil)
			return
		}
		runID, perr := uuid.Parse(req.RunID)
		if perr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"run_id must be a valid UUID",
				map[string]any{"field": "run_id", "got": req.RunID})
			return
		}
		rn, gerr := s.cfg.RunRepo.GetRun(r.Context(), runID)
		if gerr != nil {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"run does not exist", map[string]any{"run_id": runID.String()})
			return
		}
		// Repo-consistency authorization gate (#1005 fix-up). A run_id is
		// only honoured when the run's repo matches the filing target.
		// Without this, any authenticated subject that knows a run UUID
		// could (a) borrow that run's installation context to file against
		// a caller-chosen repo and (b) inject a work_item_filed audit entry
		// onto an unrelated run's hash chain (actor_subject = their token).
		// v0's authorization posture is authenticated-caller + run/repo
		// consistency: the conventions loader is hard-wired to the default
		// repo and the installation id is sourced from the named run, so
		// binding the run to the requested repo closes the cross-run /
		// cross-repo write surface. A per-caller entitlement check (does
		// this subject own this run?) is a follow-up for once runs carry an
		// owning-subject ACL.
		if !strings.EqualFold(rn.Repo, owner+"/"+name) {
			s.writeError(w, r, http.StatusForbidden, "run_repo_mismatch",
				"run_id belongs to a different repository than the filing target",
				map[string]any{"run_repo": rn.Repo, "requested_repo": owner + "/" + name})
			return
		}
		activeRun = rn
	}

	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
	}
	// InstallationID is sourced only from a consistency-checked active run.
	// On the run-absent path (the ADR-040 operator-agent follow-up filing
	// path) it stays 0: the real GitHub Projects provider cannot mint an
	// installation token without it, so GitHub filing is run-scoped by
	// design in v0. Supplying an installation source for run-absent GitHub
	// filing is a follow-up; non-GitHub providers that don't need an
	// installation token are unaffected.
	if activeRun != nil && activeRun.InstallationID != nil {
		target.InstallationID = *activeRun.InstallationID
	}

	created, err := provider.File(r.Context(), workmgmt.ProviderRequest{
		Item:   item,
		Number: number,
		Target: target,
	})
	if err != nil {
		s.writeError(w, r, http.StatusBadGateway, "work_item_filing_failed",
			"provider could not file the work item", map[string]any{"error": err.Error()})
		return
	}

	audited := s.auditWorkItemFiling(r, activeRun, item, created, id.Subject)

	s.writeJSON(w, r, http.StatusCreated, workItemResponse{
		Type:          item.Type,
		Title:         item.Title,
		Number:        created.Number,
		URL:           created.URL,
		Provider:      created.Provider,
		AppliedLabels: created.AppliedLabels,
		Complexity:    item.Classification.Complexity,
		Status:        created.Status,
		BoardColumn:   created.BoardColumn,
		Audited:       audited,
	})
}

// auditWorkItemFiling writes a work_item_filed entry onto activeRun when
// one is in flight. It is best-effort: the item is already filed, so a
// missing audit repo, a terminal/absent run, or an append error never
// fails the response — the function logs and returns false. Returns true
// only when an entry was written.
func (s *Server) auditWorkItemFiling(r *http.Request, activeRun *run.Run, item workmgmt.WorkItem, created *workmgmt.CreatedItem, subject string) bool {
	if s.cfg.AuditRepo == nil || activeRun == nil || activeRun.State.IsTerminal() {
		return false
	}
	payload, _ := json.Marshal(map[string]any{
		"type":           item.Type,
		"title":          item.Title,
		"provider":       created.Provider,
		"created_url":    created.URL,
		"created_number": created.Number,
		"applied_labels": created.AppliedLabels,
		"board_column":   created.BoardColumn,
		"status":         created.Status,
	})
	kind := actorKindForSubject(subject)
	subj := subject
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        activeRun.ID,
		Timestamp:    time.Now().UTC(),
		Category:     categoryWorkItemFiled,
		ActorKind:    &kind,
		ActorSubject: &subj,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "append work_item_filed audit",
			slog.String("error", err.Error()),
			slog.String("run_id", activeRun.ID.String()),
		)
		return false
	}
	return true
}

// splitRepoFullName splits an "owner/name" coordinate into its parts,
// reporting ok=false when either side is empty or the string isn't a
// single owner/name pair.
func splitRepoFullName(s string) (owner, name string, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(s), "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	owner = strings.TrimSpace(parts[0])
	name = strings.TrimSpace(parts[1])
	if owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return owner, name, true
}
