package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
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
	DependsOn    []string `json:"depends_on,omitempty"`
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
	// Boarded / EpicLinked report whether the best-effort post-create
	// enrichment landed (#1107). Board placement and epic linking no longer
	// fail the filing: the created issue is the durable result, so a
	// placement/link failure returns 201 with boarded/epic_linked false and
	// the cause in boarding_error / epic_link_error (also WARN-logged
	// server-side) rather than a 502 that orphans the issue. Boarded is
	// always set (required); boarding_error / epic_link_error are present
	// only when the respective step failed.
	Boarded       bool   `json:"boarded"`
	EpicLinked    bool   `json:"epic_linked"`
	BoardingError string `json:"boarding_error,omitempty"`
	EpicLinkError string `json:"epic_link_error,omitempty"`
	Audited       bool   `json:"audited"`
}

// handleFileWorkItem implements POST /v0/work-items.
//
// It loads the repo's work-management conventions, applies them to the
// caller's filing request (rendering the title, assembling the body,
// merging labels, resolving board placement and ADR numbering, and
// validating relations), dispatches the resolved item to the registered
// provider, and returns the created item. When `run_id` names a run that
// is in flight, it also writes a best-effort work_item_filed audit entry
// onto that run (#1005) — but only when the caller holds that run's own
// run-bound agent token (the entitlement gate below closes the cross-run
// audit-write surface). An unimplemented/unregistered provider fails
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
			DependsOn:    req.Relations.DependsOn,
		}
	}

	// Resolve the optional active run up front: it supplies the
	// installation id the provider needs to act on the repo and is the
	// target of the work_item_filed audit. run_id is an
	// authorization-sensitive input — it names whose hash chain a
	// work_item_filed entry is appended to — so it is gated by a
	// caller-to-run entitlement check AND a run-to-repo consistency
	// check before it is honoured.
	var activeRun *run.Run
	if strings.TrimSpace(req.RunID) != "" {
		runID, perr := uuid.Parse(req.RunID)
		if perr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"run_id must be a valid UUID",
				map[string]any{"field": "run_id", "got": req.RunID})
			return
		}
		// (1) Caller-to-run entitlement (#1005 fix-up). A work_item_filed
		// audit entry may only be written onto a run by that run's own
		// run-bound agent token — the same `mcp:run:<uuid>` binding the
		// scope-amendment endpoints enforce (runBoundTokenRunID). Without
		// this, any authenticated caller that learns an in-flight run UUID
		// could inject an entry onto that run's hash chain under their own
		// actor_subject (a cross-run audit-write surface; Fishhawk's threat
		// model assumes agent tokens run arbitrary commands). A token bound
		// to a *different* run, an operator token, or a cookie session is
		// rejected here too: the in-runner filing path (fishhawk_file_issue
		// with FISHHAWK_RUN_ID) carries the agent's own run-bound token, and
		// the ADR-040 operator follow-up path files run-absent (no run_id,
		// no audit).
		tokenRunID, runBound := runBoundTokenRunID(id)
		if !runBound || tokenRunID != runID {
			s.writeError(w, r, http.StatusForbidden, "run_not_entitled",
				"run_id may only be supplied by that run's own run-bound agent token",
				map[string]any{"run_id": runID.String()})
			return
		}
		if s.cfg.RunRepo == nil {
			s.writeError(w, r, http.StatusServiceUnavailable, "run_lookup_unconfigured",
				"run_id supplied but no run repository is configured", nil)
			return
		}
		rn, gerr := s.cfg.RunRepo.GetRun(r.Context(), runID)
		if gerr != nil {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"run does not exist", map[string]any{"run_id": runID.String()})
			return
		}
		// (2) Run-to-repo consistency (#1005 fix-up). Defense in depth: the
		// run the caller is entitled to must also be the run for the filing
		// target repo, so a run-bound token cannot file against — or borrow
		// the installation of — a different repository than its own run.
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
		Jira:    conv.Jira,
	}
	// InstallationID is sourced first from a consistency-checked active run.
	// On the run-absent path (the ADR-040 operator-agent follow-up filing
	// path) the run does not supply one, so the handler resolves the App's
	// installation for the target repo directly (mirroring run-creation at
	// runs.go:384) — without this the real GitHub Projects provider cannot
	// mint an installation token and fails closed. Providers that need no
	// installation token are unaffected (the field is GitHub-specific and
	// resolution only runs when a GitHub client is wired).
	if activeRun != nil && activeRun.InstallationID != nil {
		target.InstallationID = *activeRun.InstallationID
	}
	if target.InstallationID == 0 && s.cfg.GitHub != nil {
		// BINDING authz gate: the run-absent installation-resolution branch
		// is the operator-agent follow-up path ONLY. A run-bound agent token
		// (mcp:run:<uuid> subject) MUST file through the run-scoped path —
		// supply its own run_id, which is repo-consistency-checked above — so
		// it cannot use the run-absent door to resolve an installation for an
		// arbitrary App-installed repo (the confused-deputy egress #1005
		// closed). Reject before any GetRepoInstallation call or provider
		// dispatch. Non-run-bound operator/session callers proceed (operators
		// are trusted for App-installed repos in v0).
		if _, runBound := runBoundTokenRunID(id); runBound {
			s.writeError(w, r, http.StatusForbidden, "run_scoped_filing_required",
				"a run-bound agent token must file through the run-scoped path (supply its own run_id); the run-absent installation-resolution path is operator-only",
				nil)
			return
		}
		instID, rerr := s.cfg.GitHub.GetRepoInstallation(r.Context(), githubclient.RepoRef{Owner: owner, Name: name})
		switch {
		case rerr == nil:
			target.InstallationID = instID
		case errors.Is(rerr, githubclient.ErrNotInstalled):
			// App genuinely not installed on the repo: leave InstallationID 0
			// and proceed so the GitHub provider fails closed with its own
			// actionable typed error for the unresolvable case.
		default:
			// Transient/network failure: surface it rather than masking it as
			// the misleading provider "no installation" message.
			s.writeError(w, r, http.StatusBadGateway, "work_item_filing_failed",
				"could not resolve the GitHub App installation for the target repo",
				map[string]any{"error": rerr.Error()})
			return
		}
	}

	item, created, werr := s.applyAndFileWorkItem(r.Context(), filing, conv, target, owner, name)
	if werr != nil {
		s.writeError(w, r, werr.status, werr.code, werr.msg, werr.details)
		return
	}

	audited := s.auditWorkItemFiling(r, activeRun, *item, created, id.Subject)

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
		Boarded:       created.Boarded,
		EpicLinked:    created.EpicLinked,
		BoardingError: created.BoardingError,
		EpicLinkError: created.EpicLinkError,
		Audited:       audited,
	})
}

// workItemError carries one failure branch of the work-item filing
// pipeline as a structured value, so both handleFileWorkItem and
// handleDeferConcern map the same Apply/provider error modes onto the
// same HTTP status + code without duplicating the branch ladder.
type workItemError struct {
	status  int
	code    string
	msg     string
	details map[string]any
}

// applyAndFileWorkItem is the conventions-Apply -> provider-File core
// shared by the POST /v0/work-items handler and the defer-concern
// handler. It auto-derives the {epic} title var, applies the repo's
// conventions, resolves the registered provider, dispatches the filing,
// and WARN-logs an incomplete boarding/epic-link enrichment. It returns
// the canonical item + the created result, or a *workItemError naming
// the failure branch (work_item_invalid/422, provider_unimplemented/501,
// work_item_filing_failed/502, internal_error/500) — the SAME mapping
// the inline handler used before the extraction, so the behavior is
// preserved verbatim.
//
// It does NOT enforce any caller-to-run entitlement: those #1005
// confused-deputy egress gates (run-id entitlement, run-to-repo
// consistency, the run-bound run-absent-installation rejection) live in
// handleFileWorkItem BEFORE this is called and must stay there. The
// defer handler resolves its already-authorized run's installation into
// the target itself.
func (s *Server) applyAndFileWorkItem(ctx context.Context, filing workmgmt.FilingRequest, conv workmgmt.Conventions, target workmgmt.Target, owner, name string) (*workmgmt.WorkItem, *workmgmt.CreatedItem, *workItemError) {
	// Auto-derive the {epic} title placeholder from the parent_epic relation
	// (#1184) before Apply renders the title, so a child type need only supply
	// {n}. Fails closed (leaves epic unset) on every failure mode, so Apply's
	// renderTitle returns the structured missing-placeholder 422 rather than a
	// wrong title or a crash.
	s.deriveEpicTitleVar(ctx, &filing, conv, target.InstallationID, owner, name)

	// Discover the in-use sequential numbers server-side for a numbered type
	// that omitted existing_numbers (#1269), so the caller no longer has to
	// scan the tracker. Runs BEFORE the pure Apply (mirroring deriveEpicTitleVar)
	// and seeds filing.ExistingNumbers; a genuine discovery failure fails the
	// filing closed here, and a provider without the capability falls through to
	// Apply's existing #1265 fail-closed 422.
	if werr := s.discoverExistingNumbers(ctx, &filing, conv, target); werr != nil {
		return nil, nil, werr
	}

	item, number, err := workmgmt.Apply(filing, conv)
	if err != nil {
		var sem *workmgmt.SemanticError
		if errors.As(err, &sem) {
			// Surface the conventions layer's structured detail
			// (missing_placeholders / unknown_sections / expected_sections)
			// alongside type so the caller can act on it (#1184). Details
			// defaults nil, so a SemanticError without it is unchanged.
			details := map[string]any{"type": filing.Type}
			for k, v := range sem.Details {
				details[k] = v
			}
			return nil, nil, &workItemError{
				status: http.StatusUnprocessableEntity, code: "work_item_invalid",
				msg: sem.Error(), details: details,
			}
		}
		return nil, nil, &workItemError{
			status: http.StatusInternalServerError, code: "internal_error",
			msg:     "could not apply work-management conventions",
			details: map[string]any{"error": err.Error()},
		}
	}

	provider, err := workmgmt.Get(conv.Provider)
	if err != nil {
		var unk *workmgmt.UnknownProviderError
		if errors.As(err, &unk) {
			// Fail closed: an unimplemented provider (jira is interface-only
			// in v0) or a config typo names the missing id rather than
			// panicking on a nil dispatch.
			return nil, nil, &workItemError{
				status: http.StatusNotImplemented, code: "provider_unimplemented",
				msg:     unk.Error(),
				details: map[string]any{"provider": unk.ID, "registered": unk.Known},
			}
		}
		return nil, nil, &workItemError{
			status: http.StatusInternalServerError, code: "internal_error",
			msg:     "could not resolve work-item provider",
			details: map[string]any{"error": err.Error()},
		}
	}

	created, err := provider.File(ctx, workmgmt.ProviderRequest{
		Item:   item,
		Number: number,
		Target: target,
	})
	if err != nil {
		return nil, nil, &workItemError{
			status: http.StatusBadGateway, code: "work_item_filing_failed",
			msg:     "provider could not file the work item",
			details: map[string]any{"error": err.Error()},
		}
	}

	// A best-effort boarding/epic-link failure stays VISIBLE: WARN-log the
	// cause (repo + issue url/number + the wrapped placeOnBoard/linkEpic
	// error) so a genuine org-project misconfig (e.g. a typo'd Status
	// option) is diagnosable rather than silently swallowed (#1107). The
	// issue itself was created, so this is not a filing failure.
	if created.BoardingError != "" || created.EpicLinkError != "" {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "work item filed but enrichment incomplete",
			slog.String("repo", owner+"/"+name),
			slog.String("issue_url", created.URL),
			slog.Int("issue_number", created.Number),
			slog.String("boarding_error", created.BoardingError),
			slog.String("epic_link_error", created.EpicLinkError),
		)
	}

	return &item, created, nil
}

// discoverExistingNumbers fills filing.ExistingNumbers for a numbered type
// (e.g. adr) by asking the resolved provider to enumerate the numbers already
// in use in the tracker (#1269), so existing_numbers is optional again for
// numbered filings. It runs BEFORE the pure workmgmt.Apply and mirrors
// deriveEpicTitleVar's pre-Apply provider-side I/O step.
//
// It is a no-op (returns nil) when: the type is unknown, the type is not
// numbered (Numbering == nil) or its scheme is not "sequential", or the caller
// already supplied existing_numbers (an explicit hint/override short-circuits
// discovery). Otherwise it resolves the provider via workmgmt.Get; if the
// provider does NOT implement the optional workmgmt.NumberDiscoverer
// capability it returns nil (no-op) and lets Apply's existing #1265 fail-closed
// 422 fire unchanged — discovery never ran, so that 422 is NOT enriched with
// discovery_failed. Only a genuine discovery error (capability present,
// DiscoverNumbers returns an error) returns a *workItemError 422
// work_item_invalid carrying details.discovery_failed.
//
// On success it sets filing.ExistingNumbers = append(discovered, 0): an empty
// discovery seeds [0] (the documented seed-zero escape → number 1) and a
// populated discovery allocates max+1. allocateNumber is unchanged and stays
// the final fail-closed guard.
// The receiver is unused (discovery resolves the provider through the global
// workmgmt registry, not server config) but the method form mirrors
// deriveEpicTitleVar's pre-Apply hook and keeps the call site uniform.
func (*Server) discoverExistingNumbers(ctx context.Context, filing *workmgmt.FilingRequest, conv workmgmt.Conventions, target workmgmt.Target) *workItemError {
	itemType, ok := conv.Types[filing.Type]
	if !ok || itemType.Numbering == nil || itemType.Numbering.Scheme != "sequential" {
		return nil
	}
	if len(filing.ExistingNumbers) > 0 {
		// Caller-supplied numbers are an explicit hint/override — skip discovery.
		return nil
	}
	provider, err := workmgmt.Get(conv.Provider)
	if err != nil {
		// Provider resolution failure is surfaced by applyAndFileWorkItem's own
		// workmgmt.Get below (typed 501 / 500); leave it to that single mapping.
		return nil
	}
	discoverer, ok := provider.(workmgmt.NumberDiscoverer)
	if !ok {
		// No discovery capability: fall through to Apply's #1265 fail-closed 422.
		return nil
	}
	discovered, err := discoverer.DiscoverNumbers(ctx, workmgmt.DiscoverNumbersRequest{
		Target:      target,
		Prefix:      itemType.Numbering.Prefix,
		TitleFormat: itemType.TitleFormat,
	})
	if err != nil {
		return &workItemError{
			status: http.StatusUnprocessableEntity, code: "work_item_invalid",
			msg: fmt.Sprintf(
				"could not discover existing numbers for the numbered type %q: %s; pass existing_numbers explicitly (or seed existing_numbers:[0] for a genuinely-first item)",
				itemType.Numbering.Prefix, err.Error()),
			details: map[string]any{
				"type":                      filing.Type,
				"numbered_type":             itemType.Numbering.Prefix,
				"existing_numbers_required": true,
				"discovery_failed":          err.Error(),
			},
		}
	}
	// Seed 0 so an empty discovery yields 1 via allocateNumber's seed-zero path,
	// and a populated discovery allocates max+1.
	filing.ExistingNumbers = append(discovered, 0)
	return nil
}

// epicTitleRE extracts the epic number from a parent epic's leading
// `[E<digits>]` title token (the Project #7 `[EX] desc` epic title format).
// `\d+` stops at the first non-digit, so a `[E22.X]`-style title still
// yields "22".
var epicTitleRE = regexp.MustCompile(`^\s*\[E(\d+)`)

// deriveEpicTitleVar auto-derives the {epic} title placeholder from the
// parent_epic relation (#1184). When the type's title_format references
// {epic}, parent_epic is set, title_vars omits epic, a GitHub client is
// wired, and an installation id is available, it fetches the parent epic
// issue and parses its leading [E<n>] token into filing.TitleVars["epic"]
// so a child type need only supply {n}.
//
// It fails CLOSED on every failure mode — no client, no installation, an
// unparseable parent ref, a GetIssue error, or a parent title with no
// [E<n>] token — by leaving epic unset, so Apply's renderTitle returns the
// structured missing-placeholder 422 rather than a wrong title or a crash.
// It mutates filing in place; the caller passes a pointer.
func (s *Server) deriveEpicTitleVar(ctx context.Context, filing *workmgmt.FilingRequest, conv workmgmt.Conventions, installID int64, owner, name string) {
	itemType, ok := conv.Types[filing.Type]
	if !ok || !strings.Contains(itemType.TitleFormat, "{epic}") {
		return
	}
	if strings.TrimSpace(filing.Relations.ParentEpic) == "" {
		return
	}
	if _, set := filing.TitleVars["epic"]; set {
		return
	}
	if s.cfg.GitHub == nil || installID == 0 {
		return
	}
	number, err := parseEpicRef(filing.Relations.ParentEpic)
	if err != nil {
		return
	}
	issue, err := s.cfg.GitHub.GetIssue(ctx, installID, githubclient.RepoRef{Owner: owner, Name: name}, number)
	if err != nil {
		return
	}
	m := epicTitleRE.FindStringSubmatch(issue.Title)
	if m == nil {
		return
	}
	// MANDATORY nil-map guard (#1184): allocate before assigning so a filing
	// that omits title_vars entirely does not panic.
	if filing.TitleVars == nil {
		filing.TitleVars = map[string]string{}
	}
	filing.TitleVars["epic"] = m[1]
}

// parseEpicRef parses "#123" or "123" into the issue number, mirroring the
// github provider's parser so a parent_epic relation resolves consistently.
func parseEpicRef(ref string) (int, error) {
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ref), "#"))
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("not a numeric issue reference: %q", ref)
	}
	if n <= 0 {
		return 0, fmt.Errorf("issue number must be > 0: %q", ref)
	}
	return n, nil
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
