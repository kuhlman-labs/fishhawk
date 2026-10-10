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
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/intakegroom"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgithub "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/github"
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
// The default stub returns the shipped Default() (which seeds the
// kuhlman-labs/fishhawk Project #7 conventions); serve.go replaces it at
// startup with RepoConventionsLoader.Load (conventions_loader.go, #2022),
// which fetches `.fishhawk/work-management.yaml` from the filing repo's OWN
// forge — resolved via the ADR-057/ADR-058 provider discriminator, with the
// FISHHAWKD_WORKMGMT_CONVENTIONS override retained as the break-glass
// fallback. Declared as a package var so tests can inject conventions (e.g.
// an unimplemented-provider config) without a forge round-trip.
var conventionsLoader = func(_ context.Context, _ string) (workmgmt.Conventions, error) {
	return workmgmt.Default(), nil
}

// SetConventionsLoader installs the process-wide work-management conventions
// resolver, replacing the Default()-only stub. It is the seam serve.go uses
// to install the per-repo RepoConventionsLoader (#2022) — carrying the
// deployment-level FISHHAWKD_WORKMGMT_CONVENTIONS file as its break-glass
// fallback. It is not concurrency-safe with in-flight filings and is
// intended to be called once at startup.
func SetConventionsLoader(loader func(ctx context.Context, repo string) (workmgmt.Conventions, error)) {
	conventionsLoader = loader
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
	// SourceRefs are same-repo refs ('#N' or 'N') to the existing items this
	// draft derives from (#3774). They map onto FilingRequest.SourceRefs: the
	// intake hook excludes those items from the duplicate candidates and
	// reports them as derives_from instead. A malformed ref is refused 400 by
	// the prelude before any forge round-trip.
	SourceRefs []string `json:"source_refs,omitempty"`
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
	// DefaultedLabels / MissingLabelNamespaces are the LOUD label-completeness
	// report (#1616, phase added #3179): every label the system added that the
	// caller did not supply (namespace defaults + handler-derived area and
	// phase), and any required
	// namespace still absent after merge/derivation/defaulting. A missing
	// namespace is reported, never a rejection (fail-open). Both omitempty.
	DefaultedLabels        []string `json:"defaulted_labels,omitempty"`
	MissingLabelNamespaces []string `json:"missing_label_namespaces,omitempty"`
	// Intake carries the ADVISORY intake-groom signals derived at filing time
	// (#2239): duplicate candidates, a parent-epic suggestion and a provisional
	// charter-anchored structural score. NOTHING was acted on — no item was
	// closed, merged, relabelled or transitioned — and a `degraded` object
	// carrying a `degrade_reason` is NORMAL, not an error: grooming is
	// best-effort and never blocks or fails a filing. Omitempty, so a
	// deployment whose hook produced nothing returns the pre-#2239 payload
	// verbatim.
	Intake *intakegroom.Signals `json:"intake,omitempty"`
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
	rq, ok := s.resolveWorkItemRequest(w, r, false)
	if !ok {
		return
	}

	item, created, werr, signals := s.applyAndFileWorkItemWithIntake(r.Context(), rq.filing, rq.conv, rq.target, rq.owner, rq.name)
	if werr != nil {
		s.writeError(w, r, werr.status, werr.code, werr.msg, werr.details)
		return
	}

	audited := s.auditWorkItemFiling(r, rq.activeRun, *item, created, rq.id.Subject, signals)

	s.writeJSON(w, r, http.StatusCreated, workItemResponse{
		Type:                   item.Type,
		Title:                  item.Title,
		Number:                 created.Number,
		URL:                    created.URL,
		Provider:               created.Provider,
		AppliedLabels:          created.AppliedLabels,
		Complexity:             item.Classification.Complexity,
		Status:                 created.Status,
		BoardColumn:            created.BoardColumn,
		Boarded:                created.Boarded,
		EpicLinked:             created.EpicLinked,
		BoardingError:          created.BoardingError,
		EpicLinkError:          created.EpicLinkError,
		Audited:                audited,
		DefaultedLabels:        item.Classification.DefaultedLabels,
		MissingLabelNamespaces: item.Classification.MissingLabelNamespaces,
		Intake:                 signals,
	})
}

// resolvedWorkItem is what the shared work-item request prelude hands its
// handler: the authenticated caller, the decoded FilingRequest, the repo's
// conventions, the resolved provider Target (installation scope included) and
// the entitlement-checked active run (nil on the run-absent path).
type resolvedWorkItem struct {
	id        Identity
	filing    workmgmt.FilingRequest
	conv      workmgmt.Conventions
	target    workmgmt.Target
	owner     string
	name      string
	activeRun *run.Run
}

// resolveWorkItemRequest is the request prelude POST /v0/work-items and POST
// /v0/work-items/preview share (#3774): auth, the body cap, the
// DisallowUnknownFields decode, repo/type/summary/source_refs validation, the
// conventions load, the run_id entitlement and run-to-repo checks, the
// run-bound run-absent gate and GitHub installation resolution — so a preview
// resolves EXACTLY the filing a POST would, with the same status codes in the
// same order. It writes the error envelope itself and returns ok=false on any
// refusal.
//
// preview=true adds three refusals, in this order: (a) a run-bound
// (mcp:run:<uuid>) identity is 403 preview_operator_only right after the
// anonymous check, before the body is read; (b) a non-empty run_id is 400
// validation_failed {field: run_id}, because a preview writes no audit; (c)
// the repo is held to the point-read visibility DENY (403 repo_forbidden) as
// soon as it parses. Everything else is shared verbatim.
func (s *Server) resolveWorkItemRequest(w http.ResponseWriter, r *http.Request, preview bool) (*resolvedWorkItem, bool) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		verb := "filing"
		if preview {
			verb = "previewing"
		}
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			verb+" a work item requires an authenticated caller", nil)
		return nil, false
	}
	// PREVIEW (a): operator-only, refused BEFORE the body is read, so a
	// run-bound agent token reaches no tracker read at all. A preview reads the
	// target repo's titles (the duplicate window) without filing anything, and
	// ADR-064 decision 3 exposes no board read to agents through the MCP
	// surface. This is the posture the MCP gate defers to: fishhawk_preview_issue
	// is mcpScopeAuthenticatedOnly and THIS is where a run-bound bearer stops.
	if preview {
		if _, runBound := runBoundTokenRunID(id); runBound {
			s.writeError(w, r, http.StatusForbidden, "preview_operator_only",
				"previewing a work item is operator-only; a run-bound agent token files through POST /v0/work-items with its own run_id",
				nil)
			return nil, false
		}
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxWorkItemRequestBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return nil, false
	}
	if len(raw) > maxWorkItemRequestBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"request body exceeds size cap", map[string]any{"limit_bytes": maxWorkItemRequestBytes})
		return nil, false
	}

	var req workItemRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON for a work-item filing",
			map[string]any{"error": err.Error()})
		return nil, false
	}
	// PREVIEW (b): a preview writes no audit and is not run-scoped, so a run_id
	// has nothing to drive. Refused rather than ignored, so a caller that
	// expected a run-scoped side effect learns it will not happen.
	if preview && strings.TrimSpace(req.RunID) != "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id is not accepted by a preview: a preview writes no audit and is not run-scoped",
			map[string]any{"field": "run_id", "got": req.RunID})
		return nil, false
	}

	owner, name, ok := splitRepoFullName(req.Repo)
	if !ok {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo must be in owner/name form",
			map[string]any{"field": "repo", "got": req.Repo})
		return nil, false
	}
	// PREVIEW (c): the point-read repo-visibility DENY. A preview returns the
	// repo's item titles inside its duplicate candidates, so it is a READ of the
	// repo and gets the #1829 point-read posture (403 repo_forbidden) for a
	// cookie session that cannot see it. Token callers and an unwired mirror
	// pass through (repoFilterFor's nil filter), exactly as on every other read.
	if preview && !s.enforceRepoVisibility(w, r, owner+"/"+name) {
		return nil, false
	}
	if strings.TrimSpace(req.Type) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"type is required", map[string]any{"field": "type"})
		return nil, false
	}
	if strings.TrimSpace(req.Summary) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"summary is required", map[string]any{"field": "summary"})
		return nil, false
	}
	// source_refs are validated HERE, before the conventions load, so a
	// malformed ref costs no forge round-trip. The filing core re-checks them
	// (422) for the server-internal callers that never pass this prelude.
	if _, err := intakegroom.ParseSourceRefs(req.SourceRefs); err != nil {
		got := ""
		var refErr *intakegroom.SourceRefError
		if errors.As(err, &refErr) {
			got = refErr.Ref
		}
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			err.Error(), map[string]any{"field": "source_refs", "got": got})
		return nil, false
	}

	conv, err := conventionsLoader(r.Context(), req.Repo)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load work-management conventions", map[string]any{"error": err.Error()})
		return nil, false
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
		SourceRefs:      req.SourceRefs,
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
			return nil, false
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
			return nil, false
		}
		if s.cfg.RunRepo == nil {
			s.writeError(w, r, http.StatusServiceUnavailable, "run_lookup_unconfigured",
				"run_id supplied but no run repository is configured", nil)
			return nil, false
		}
		rn, gerr := s.cfg.RunRepo.GetRun(r.Context(), runID)
		if gerr != nil {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"run does not exist", map[string]any{"run_id": runID.String()})
			return nil, false
		}
		// (2) Run-to-repo consistency (#1005 fix-up). Defense in depth: the
		// run the caller is entitled to must also be the run for the filing
		// target repo, so a run-bound token cannot file against — or borrow
		// the installation of — a different repository than its own run.
		if !strings.EqualFold(rn.Repo, owner+"/"+name) {
			s.writeError(w, r, http.StatusForbidden, "run_repo_mismatch",
				"run_id belongs to a different repository than the filing target",
				map[string]any{"run_repo": rn.Repo, "requested_repo": owner + "/" + name})
			return nil, false
		}
		activeRun = rn
	}

	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
		Jira:    conv.Jira,
		GitLab:  conv.GitLab,
	}
	// Scope is sourced first from a consistency-checked active run.
	// On the run-absent path (the ADR-040 operator-agent follow-up filing
	// path) the run does not supply one, so the handler resolves the App's
	// installation for the target repo directly (mirroring run-creation at
	// runs.go:384) — without this the real GitHub Projects provider cannot
	// mint an installation token and fails closed. Providers that need no
	// installation token are unaffected (the scope is GitHub-specific and
	// resolution only runs when a GitHub client is wired).
	if activeRun != nil && activeRun.InstallationID != nil {
		target.Scope = forge.FromGitHubInstallationID(*activeRun.InstallationID)
	}
	// BINDING authz gate (provider-independent, ADR-058 #1856): the run-absent
	// filing path is the operator-agent follow-up path ONLY. A run-bound agent
	// token (mcp:run:<uuid> subject) MUST file through the run-scoped path —
	// supply its own run_id, repo-consistency-checked above — so it can neither
	// resolve a GitHub App installation for an arbitrary App-installed repo (the
	// confused-deputy egress #1005 closed) NOR file gitlab/jira issues with the
	// deployment-wide server-side credentials; both widen a run-scoped token's
	// authority to make unscoped filings. This gate is provider-INDEPENDENT and
	// fires before any provider-specific resolution, so gitlab/jira get the same
	// run-scoped-only posture github does (a run-bound token filing a resolved
	// provider:gitlab no longer slips past this rejection). Keyed on the
	// run-ABSENT condition (activeRun nil), NOT on Target.Scope being zero, so a
	// legitimate run-scoped filing whose run carries no GitHub installation
	// (every gitlab run) is unaffected: it set activeRun by supplying a
	// repo-consistency-checked run_id. Non-run-bound operator/session callers
	// proceed (operators are trusted in v0). Reject before any
	// GetRepoInstallation call or provider dispatch.
	if activeRun == nil {
		if _, runBound := runBoundTokenRunID(id); runBound {
			s.writeError(w, r, http.StatusForbidden, "run_scoped_filing_required",
				"a run-bound agent token must file through the run-scoped path (supply its own run_id); the run-absent filing path is operator-only",
				nil)
			return nil, false
		}
	}
	// The GitHub installation-resolution branch is forge-optional: it runs ONLY
	// when the resolved provider is github_projects (ADR-058 #1856).
	// Installation resolution and its scope are GitHub-specific, so a gitlab (or
	// jira) filing must not attempt GitHub App resolution — it would otherwise
	// 502 on GitHub egress and cannot proceed with s.cfg.GitHub nil. Those
	// providers carry their own server-side credentials and leave Target.Scope
	// zero (the #1855 credential-scope seam is out of scope here). The
	// provider-independent run-bound rejection above has already run, so this
	// branch is reached only by operator/session callers (or a run-scoped
	// filing) — its github_projects security posture is unchanged.
	if target.Scope.IsZero() && s.cfg.GitHub != nil && conv.Provider == workmgmtgithub.ProviderName {
		scope, rerr := s.resolveRepoScope(r.Context(), owner, name)
		if rerr != nil {
			// Transient/network failure: surface it rather than masking it as
			// the misleading provider "no installation" message. An
			// ErrNotInstalled is not an error here — resolveRepoScope returns a
			// zero scope so the GitHub provider fails closed with its own
			// actionable typed error for the unresolvable case.
			s.writeError(w, r, http.StatusBadGateway, "work_item_filing_failed",
				"could not resolve the GitHub App installation for the target repo",
				map[string]any{"error": rerr.Error()})
			return nil, false
		}
		target.Scope = scope
	}

	return &resolvedWorkItem{
		id:        id,
		filing:    filing,
		conv:      conv,
		target:    target,
		owner:     owner,
		name:      name,
		activeRun: activeRun,
	}, true
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
	item, created, werr, _ := s.applyAndFileWorkItemWithIntake(ctx, filing, conv, target, owner, name)
	return item, created, werr
}

// applyAndFileWorkItemWithIntake is applyAndFileWorkItem plus the intake
// signals the hook derived (#2239). It exists as a separate entry point rather
// than as a fourth return value on the shared core because FIVE call sites
// share that core — handleFileWorkItem, defer-concern filing, live-validation
// filing, refinement filing and split filing — and only the HTTP handler has
// anywhere to put the signals. Widening the shared signature would have forced
// a `_` at four sites that gain nothing from it, one of which (split_filing.go)
// is outside this change's scope; the plan's inventory named only three.
//
// The grooming itself is NOT gated on which entry point was used: it happens
// inside this function, which every path reaches, so defer-concern, refinement,
// live-validation and split filings all carry the rendered advisory section on
// their created issue exactly as an HTTP filing does. They simply do not read
// the returned Signals back. That is the shared-core claim, and
// TestDeferConcern_IntakeSignalsRenderedThroughSharedCore pins it behaviourally
// through one of those secondary paths (binding approval condition L5).
//
// Since #3774 it is prepareWorkItem (every pre-File step) followed by the File
// tail below; previewWorkItem shares the first half and never runs the second.
func (s *Server) applyAndFileWorkItemWithIntake(ctx context.Context, filing workmgmt.FilingRequest, conv workmgmt.Conventions, target workmgmt.Target, owner, name string) (*workmgmt.WorkItem, *workmgmt.CreatedItem, *workItemError, *intakegroom.Signals) {
	// prepareWorkItem self-releases on its own error paths; on success the
	// locks it took are still held, and deferring release here keeps them held
	// across File exactly as the two inline defers did before the split.
	prep, release, werr := s.prepareWorkItem(ctx, filing, conv, target, owner, name)
	if werr != nil {
		return nil, nil, werr, nil
	}
	defer release()
	item, number, provider, signals := prep.item, prep.number, prep.provider, prep.signals

	created, err := provider.File(ctx, workmgmt.ProviderRequest{
		Item:   item,
		Number: number,
		Target: target,
		// The caller's write-ahead hook rides through verbatim (#4153): the
		// provider fires it right after its create and before board/link, so
		// the refinement executor records the issue before the interruptible
		// enrichment runs. prepareWorkItem works on its own copy of filing and
		// never touches the hook; every other entry point leaves it nil.
		OnCreated: filing.OnCreated,
	})
	if err != nil {
		return nil, nil, &workItemError{
			status: http.StatusBadGateway, code: "work_item_filing_failed",
			msg:     "provider could not file the work item",
			details: map[string]any{"error": err.Error()},
		}, nil
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

	return &item, created, nil, &signals
}

// preparedWorkItem is everything prepareWorkItem resolves for one filing: the
// rendered item exactly as provider.File would receive it (advisory section
// included), the allocated sequential number (0 when the type is not
// numbered), the resolved provider and the intake signals.
type preparedWorkItem struct {
	item     workmgmt.WorkItem
	number   int
	provider workmgmt.Provider
	signals  intakegroom.Signals
}

// prepareWorkItem runs every step of the filing core BEFORE provider.File
// (#3774): the source_refs check, {epic}/{n} derivation, the parent-epic
// capacity guard, area/phase derivation, sequential-number discovery,
// workmgmt.Apply, provider resolution, the intake hook and RenderBody. It is
// the half applyAndFileWorkItemWithIntake (which files) and previewWorkItem
// (which never does) share, so a preview renders byte-for-byte what a filing
// would send.
//
// LOCK-RELEASE CONTRACT. The child-number and sequential-number locks taken
// here are the ones that must span File, so on SUCCESS they are still held and
// handed back as release: non-nil, idempotent (sync.Once), runs the unlocks in
// reverse acquisition order (sequential, then child) and is safe to call when
// no lock was taken. On ERROR — and on a panic unwinding this frame — every
// lock it took is released internally before it returns (a deferred release
// keyed on a success flag), release is nil and the caller owes nothing.
//
// A malformed filing.SourceRefs is refused 422 work_item_invalid with
// details.source_refs_invalid before any lock or I/O.
func (s *Server) prepareWorkItem(ctx context.Context, filing workmgmt.FilingRequest, conv workmgmt.Conventions, target workmgmt.Target, owner, name string) (_ *preparedWorkItem, release func(), _ *workItemError) {
	// source_refs are refused HERE, before any lock or I/O, so EVERY entry
	// path — the server-internal callers that never pass the HTTP prelude
	// included — is refused rather than silently losing the duplicate
	// exclusion (intakeFilingFor drops a malformed list on the floor).
	if _, err := intakegroom.ParseSourceRefs(filing.SourceRefs); err != nil {
		return nil, nil, &workItemError{
			status: http.StatusUnprocessableEntity, code: "work_item_invalid",
			msg:     err.Error(),
			details: map[string]any{"type": filing.Type, "source_refs_invalid": err.Error()},
		}
	}

	// SELF-RELEASE. Every lock taken below is recorded in unlocks, in
	// acquisition order. Until prepared flips true — on any error return AND on
	// a panic unwinding this frame — the deferred func releases them all, in
	// reverse order, so a caller never owes a release after an error. On
	// success the same set is handed back as the idempotent release instead.
	var unlocks []func()
	releaseAll := func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
	prepared := false
	defer func() {
		if !prepared {
			releaseAll()
		}
	}()

	// Auto-derive the {epic} title placeholder from the parent_epic relation
	// (#1184) before Apply renders the title, so a child type need only supply
	// {n}. Fails closed (leaves epic unset) on every failure mode, so Apply's
	// renderTitle returns the structured missing-placeholder 422 rather than a
	// wrong title or a crash.
	s.deriveEpicTitleVar(ctx, &filing, conv, target.Scope, owner, name)

	// Derive the {n} child-number title placeholder from the parent epic's
	// existing children (#1958), immediately after {epic} is resolved above
	// (it consumes TitleVars["epic"]). When derivation runs it returns a
	// non-nil unlock that holds a per-parent-epic in-process lock across the
	// whole discover-{n} -> Apply -> File critical section (binding approval
	// condition 1), so two concurrent omitted-n filings against the same epic
	// serialize and allocate DISTINCT consecutive numbers. The lock is taken
	// ONLY when {n} is discovered — an explicit-n caller returns a nil unlock
	// and never contends. Recorded for release so it is held across File.
	unlockChildNumber, capacityChecked, werr := s.deriveChildNumberTitleVar(ctx, &filing, conv, target)
	if unlockChildNumber != nil {
		unlocks = append(unlocks, unlockChildNumber)
	}
	if werr != nil {
		return nil, nil, werr
	}

	// Refuse a filing whose parent epic is already at the provider's hard child
	// cap when derivation did NOT already evaluate it (#3714) — an explicit
	// title_vars.n, or a type whose title_format carries no {n}. The cap governs
	// the sub-issue LINK, not the number, so those filings would otherwise land
	// an UNLINKED orphan. This is the ONE chokepoint all five filing entry
	// points funnel through (HTTP work-items, defer-concern, live-validation,
	// refinement, split filing), so no per-call-site edit is needed, and it runs
	// BEFORE Apply and therefore before File, so nothing is created.
	if !capacityChecked {
		if werr := s.guardParentEpicCapacity(ctx, filing, conv, target); werr != nil {
			return nil, nil, werr
		}
	}

	// Derive the area:* label from the parent epic when the type wants an area
	// namespace and none was supplied (#1616), mutating filing.Labels in place
	// BEFORE Apply so the completeness pass sees area as present. Fails OPEN on
	// every mode, leaving Apply to report 'area' in missing_label_namespaces.
	// The returned labels are system-added, so they are appended to the item's
	// DefaultedLabels after Apply for the single LOUD reporting field.
	derivedArea := s.deriveAreaLabel(ctx, &filing, conv, target.Scope, owner, name)

	// Derive the phase:* label the same way and for the same reason (#3179),
	// immediately after area and likewise BEFORE Apply so the completeness pass
	// sees phase as present. The ladder is caller-supplied > parent epic >
	// originating run's triggering issue (same-repo only) > nothing; it fails
	// OPEN at every guard, leaving Apply to report 'phase' in
	// missing_label_namespaces. This is the ONE chokepoint every auto-file path
	// funnels through, so the defer-concern path that left 48 children
	// phase-unlabelled is closed here with no per-call-site edit.
	derivedPhase := s.derivePhaseLabel(ctx, &filing, conv, target.Scope, owner, name)

	// Discover the in-use sequential numbers server-side for a numbered type
	// that omitted existing_numbers (#1269), so the caller no longer has to
	// scan the tracker. Runs BEFORE the pure Apply (mirroring deriveEpicTitleVar)
	// and seeds filing.ExistingNumbers; a genuine discovery failure fails the
	// filing closed here, and a provider without the capability falls through to
	// Apply's existing #1265 fail-closed 422.
	// When discovery runs it returns a non-nil unlock holding a per-(repo,
	// numbering prefix) in-process lock across the whole discover -> Apply ->
	// File critical section (#3704), exactly as the child-number lock above
	// does, so two concurrent omitted-existing_numbers filings of the same
	// sequential type serialize and allocate DISTINCT consecutive numbers. The
	// lock is taken ONLY when discovery runs — an explicit-existing_numbers
	// caller, a non-sequential type, an unresolvable provider and a provider
	// without the capability all return a nil unlock and never contend.
	// Recorded BEFORE the error check so the discovery-success path holds it
	// across Apply + File and releases with the child lock. Both
	// locks are acquired in one fixed order (child then sequential) at this
	// single call site, so no lock-ordering cycle is reachable. Every filing
	// path (HTTP handler, defer-concern, live-validation, refinement, split)
	// funnels through this one function, so all of them inherit the
	// serialization.
	unlockSequentialNumber, werr := s.discoverExistingNumbers(ctx, &filing, conv, target)
	if unlockSequentialNumber != nil {
		unlocks = append(unlocks, unlockSequentialNumber)
	}
	if werr != nil {
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

	// The handler-derived area labels are system-added (the caller did not
	// supply them), so fold them into the ONE reporting field for everything
	// the caller didn't supply (#1616). Apply already recorded namespace
	// defaults there; derived area joins them.
	if len(derivedArea) > 0 {
		item.Classification.DefaultedLabels = append(item.Classification.DefaultedLabels, derivedArea...)
	}
	// Same for the derived phase (#3179): a system-added label the caller did
	// not supply, so it rides the SINGLE LOUD reporting field alongside area
	// and the namespace defaults. A wrong inherit is therefore visible and
	// challengeable at filing time rather than silent.
	if len(derivedPhase) > 0 {
		item.Classification.DefaultedLabels = append(item.Classification.DefaultedLabels, derivedPhase...)
	}
	// Fail-open visibility: a required namespace that could be neither merged,
	// derived, nor defaulted is WARN-logged (mirroring the #1107
	// enrichment-incomplete precedent), never a rejection.
	if len(item.Classification.MissingLabelNamespaces) > 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "work item filed with missing required label namespaces",
			slog.String("repo", owner+"/"+name),
			slog.String("type", filing.Type),
			slog.Any("missing_label_namespaces", item.Classification.MissingLabelNamespaces),
		)
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

	// INTAKE GROOM (#2239 / E54.7). Runs AFTER Apply — so it evaluates the
	// rendered title, labels and body a reader will actually see — and BEFORE
	// File, so the rendered advisory section lands on the created issue itself
	// rather than needing a second write.
	//
	// runIntakeGroom cannot fail: every error mode inside it becomes a typed
	// degrade reason and a WARN log. See intake_hook.go for WHY filing degrades
	// here while the rest of E54 fails closed on a missing charter.
	signals := s.runIntakeGroom(ctx, conv, target, intakeFilingFor(filing, item))
	// RenderBody is a no-op when the signals carry no findings, so a degraded
	// hook leaves the body byte-identical to what this core produced before
	// #2239.
	item.Body = intakegroom.RenderBody(item.Body, signals)

	prepared = true
	var once sync.Once
	return &preparedWorkItem{item: item, number: number, provider: provider, signals: signals},
		func() { once.Do(releaseAll) }, nil
}

// discoverExistingNumbers fills filing.ExistingNumbers for a numbered type
// (e.g. adr) by asking the resolved provider to enumerate the numbers already
// in use in the tracker (#1269), so existing_numbers is optional again for
// numbered filings. It runs BEFORE the pure workmgmt.Apply and mirrors
// deriveEpicTitleVar's pre-Apply provider-side I/O step.
//
// It is a no-op (returns a nil unlock, nil error) when: the type is unknown, the type is not
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
//
// When discovery WILL run — and only then — it acquires the per-(repo,
// numbering prefix) in-process lock (#3704) immediately before calling
// DiscoverNumbers and returns its unlock to the caller, which defers it so the
// lock spans discover -> Apply -> provider File exactly as the child-number
// lock does. Two concurrent filings of the same sequential type against the
// same repo would otherwise both observe the same max and both allocate max+1
// (the duplicate [E78] on #3699/#3700). Every no-op guard above returns a nil
// unlock and never contends, and the discovery-error branch releases before
// returning its 422. The lock is in-process only: a hosted MULTI-INSTANCE
// deployment sharing one tracker still needs a Postgres advisory lock, tracked
// with the child-number lock; see backend/internal/workmgmt/README.md.
//
// The receiver is unused (discovery resolves the provider through the global
// workmgmt registry, not server config) but the method form mirrors
// deriveEpicTitleVar's pre-Apply hook and keeps the call site uniform.
func (*Server) discoverExistingNumbers(ctx context.Context, filing *workmgmt.FilingRequest, conv workmgmt.Conventions, target workmgmt.Target) (func(), *workItemError) {
	itemType, ok := conv.Types[filing.Type]
	if !ok || itemType.Numbering == nil || itemType.Numbering.Scheme != "sequential" {
		return nil, nil
	}
	if len(filing.ExistingNumbers) > 0 {
		// Caller-supplied numbers are an explicit hint/override — skip discovery
		// AND the lock.
		return nil, nil
	}
	provider, err := workmgmt.Get(conv.Provider)
	if err != nil {
		// Provider resolution failure is surfaced by prepareWorkItem's own
		// workmgmt.Get below (typed 501 / 500); leave it to that single mapping.
		return nil, nil
	}
	discoverer, ok := provider.(workmgmt.NumberDiscoverer)
	if !ok {
		// No discovery capability: fall through to Apply's #1265 fail-closed 422.
		return nil, nil
	}

	// Discovery WILL run: serialize the allocate-then-file window per
	// (repo, numbering prefix).
	unlock := lockSequentialNumberKey(sequentialNumberLockKey(target, itemType.Numbering.Prefix))
	discovered, err := discoverer.DiscoverNumbers(ctx, workmgmt.DiscoverNumbersRequest{
		Target:      target,
		Prefix:      itemType.Numbering.Prefix,
		TitleFormat: itemType.TitleFormat,
		// Thread the type's default labels so the provider can narrow discovery
		// by a `label:` qualifier — its first label is the recency-proof
		// discovery key (#1522).
		DefaultLabels: itemType.DefaultLabels,
	})
	if err != nil {
		// Release before returning so a discovery failure never wedges every
		// later filing of this key — mirrors deriveChildNumberTitleVar's
		// unlock-before-return discipline. The 422 payload is unchanged.
		unlock()
		return nil, &workItemError{
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
	return unlock, nil
}

// childNumberLocks serializes the discover-{n} -> File critical section per
// parent-epic ref WITHIN THIS PROCESS (binding approval condition 1, #1958).
// Two concurrent omitted-n filings against the same epic would otherwise both
// read the same max child number and allocate a colliding {n}; the per-epic
// mutex makes them serialize so they file DISTINCT consecutive numbers. This
// is sufficient for the single-daemon v0 deployment. A hosted MULTI-INSTANCE
// deployment (multiple fishhawkd processes sharing one tracker) would need a
// Postgres advisory lock instead — the in-process map is invisible across
// processes; see backend/internal/workmgmt/README.md. The map is never pruned
// (one small mutex per distinct epic ref for the process lifetime), which is
// bounded by the number of epics filed against.
var childNumberLocks = &keyedLocks{}

// keyedLocks is the shared per-key in-process mutex map both number-allocation
// critical sections delegate to (#3704 collapsed the two ad-hoc copies into
// one). It is never pruned — one small mutex per distinct key for the process
// lifetime, bounded by the number of distinct keys filed against — which is
// the #1958 map's behaviour preserved verbatim.
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

// lock acquires (creating on first use) the mutex for key and returns its
// unlock func.
func (k *keyedLocks) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*sync.Mutex{}
	}
	m := k.m[key]
	if m == nil {
		m = &sync.Mutex{}
		k.m[key] = m
	}
	k.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// lockChildNumberKey acquires (creating on first use) the per-epic mutex for
// key and returns its unlock func. The caller holds it across EpicChildren ->
// Apply -> File so the whole allocate-then-file window is serialized.
func lockChildNumberKey(key string) func() {
	return childNumberLocks.lock(key)
}

// sequentialNumberLocks serializes the discover -> File critical section per
// (repo, numbering prefix) WITHIN THIS PROCESS (#3704), the sequential-number
// sibling of childNumberLocks above. Two concurrent filings of a
// `numbering: sequential` type (epic, adr) with existing_numbers omitted would
// otherwise both read the same max from DiscoverNumbers and both allocate
// max+1 — the duplicate [E78] observed on #3699/#3700. The same residual
// applies: a hosted MULTI-INSTANCE deployment needs a Postgres advisory lock,
// tracked with the child-number lock rather than solved here.
var sequentialNumberLocks = &keyedLocks{}

// lockSequentialNumberKey acquires (creating on first use) the per-(repo,
// prefix) mutex for key and returns its unlock func. The caller holds it
// across DiscoverNumbers -> Apply -> File.
func lockSequentialNumberKey(key string) func() {
	return sequentialNumberLocks.lock(key)
}

// sequentialNumberLockKey is the per-numbered-type serialization key: the
// target repo plus the type's numbering prefix, so `epic` and `adr` filings in
// one repo — and the same prefix in two repos — never contend on one lock. It
// reuses childNumberLockKey's owner/name derivation so the two keyspaces are
// shaped identically.
func sequentialNumberLockKey(target workmgmt.Target, prefix string) string {
	return target.Repo.Owner + "/" + target.Repo.Name + "#" + strings.TrimSpace(prefix)
}

// parentEpicAtChildCap reports whether the parent epic's observed child set has
// reached the provider's declared hard child cap (#3714), returning the counts
// so the refusal can name both numbers. It is pure so the boundary is
// unit-testable and shared by BOTH refusal sites (deriveChildNumberTitleVar's
// derived-{n} path and guardParentEpicCapacity's non-derivation path).
//
// A ChildCap of 0 means the provider declares NO cap, so the guard is inert.
// The comparison is >= , not > : at exactly cap children the parent is FULL and
// the NEXT link is the one that would be rejected.
func parentEpicAtChildCap(res *workmgmt.EpicChildrenResult) (full bool, count, childCap int) {
	if res == nil {
		return false, 0, 0
	}
	count, childCap = len(res.Children), res.ChildCap
	if childCap <= 0 {
		return false, count, childCap
	}
	return count >= childCap, count, childCap
}

// parentEpicFullError is the SINGLE constructor for the at-cap refusal, called
// from both refusal sites so the two 422s are byte-identical by construction
// (#3714). The parent's sub-issue cap governs the LINK, not the number, so an
// explicit title_vars.n does not bypass it — the message says so, because "pass
// n explicitly" is the remedy every OTHER 422 on this path names and an
// operator would otherwise reach for it here too.
func parentEpicFullError(filingType, epicRef string, count, childCap int) *workItemError {
	return &workItemError{
		status: http.StatusUnprocessableEntity, code: "work_item_invalid",
		msg: fmt.Sprintf(
			"parent epic %q already carries %d of the provider's maximum %d children, so this filing's parent link would be rejected and its [E<epic>.<n>] title would duplicate an existing one; file under a successor catch-all epic, or file with a different parent_epic (an explicit title_vars.n does not bypass the link cap)",
			epicRef, count, childCap),
		details: map[string]any{
			"type":                    filingType,
			"parent_epic_full":        true,
			"parent_epic":             epicRef,
			"parent_epic_child_count": count,
			"parent_epic_child_cap":   childCap,
		},
	}
}

// guardParentEpicCapacity refuses a filing whose parent epic is ALREADY at the
// provider's hard child cap, for every filing that does NOT run {n} derivation
// (#3714): an explicit title_vars.n, or a type whose title_format carries no
// {n} at all. The cap is a property of the sub-issue LINK, not of the number,
// so those filings would otherwise succeed-with-epic_link_error and leave an
// unlinked orphan — the defect's worse half.
//
// deriveChildNumberTitleVar owns the DERIVED-{n} path and evaluates the cap on
// the EpicChildren result it already holds; it reports that as capacityChecked,
// and the single call site runs THIS helper only when that is false — so the cap
// is evaluated exactly once per filing and this is never a second round-trip on
// the derived path.
//
// It is a no-op (returns nil) when Relations.ParentEpic is blank, when
// workmgmt.Get errors, or when the provider does not implement
// EpicChildrenQuerier — the same short-circuit ladder derivation uses.
//
// It FAILS OPEN on an EpicChildren probe error, logging at WARN: "pass n
// explicitly" is the documented escape hatch from exactly that error, and
// failing closed here would make the remedy unreachable. The residual is that a
// probe error against a genuinely-full parent still files an unlinked issue —
// narrower than the pre-#3714 unconditional freeze, but real.
func (*Server) guardParentEpicCapacity(ctx context.Context, filing workmgmt.FilingRequest, conv workmgmt.Conventions, target workmgmt.Target) *workItemError {
	epicRef := strings.TrimSpace(filing.Relations.ParentEpic)
	if epicRef == "" {
		return nil
	}
	provider, err := workmgmt.Get(conv.Provider)
	if err != nil {
		return nil
	}
	querier, ok := provider.(workmgmt.EpicChildrenQuerier)
	if !ok {
		return nil
	}
	res, err := querier.EpicChildren(ctx, workmgmt.EpicChildrenRequest{Target: target, Epic: epicRef})
	if err != nil {
		// FAIL OPEN: see the doc comment. The filing proceeds exactly as it did
		// before #3714.
		slog.WarnContext(ctx, "parent epic capacity probe failed; filing proceeds unguarded",
			"parent_epic", epicRef, "error", err)
		return nil
	}
	if full, count, childCap := parentEpicAtChildCap(res); full {
		return parentEpicFullError(filing.Type, epicRef, count, childCap)
	}
	return nil
}

// childNumberLockKey is the per-epic serialization key: the target repo plus
// the parent-epic ref, so two epics (or the same epic ref in different repos)
// never contend on one lock.
func childNumberLockKey(target workmgmt.Target, epicRef string) string {
	return target.Repo.Owner + "/" + target.Repo.Name + "#" + strings.TrimSpace(epicRef)
}

// deriveChildNumberTitleVar fills the {n} child-number title placeholder for a
// child type (e.g. [E{epic}.{n}]) by enumerating the parent epic's existing
// children server-side (#1958), so fishhawk_defer_concern and
// fishhawk_file_issue no longer make the operator guess it. It runs BEFORE the
// pure workmgmt.Apply, immediately after deriveEpicTitleVar resolved
// TitleVars["epic"] (which it consumes), mirroring discoverExistingNumbers'
// pre-Apply provider-side I/O step.
//
// It is a no-op (returns a nil unlock, nil error) when: the type is unknown or
// its title_format has no {n}; TitleVars["n"] is already set (a caller-supplied
// n is an explicit override that short-circuits discovery — and, per the
// binding concurrency condition, is the case where the per-epic lock is NOT
// taken); Relations.ParentEpic is blank; TitleVars["epic"] is unresolved (epic
// derivation already failed closed, so renderTitle's 422 covers both
// placeholders); workmgmt.Get errors (prepareWorkItem's own Get surfaces
// the typed 501/500); or the provider does not implement EpicChildrenQuerier
// (fall through to Apply's missing-placeholder 422 unchanged).
//
// Otherwise it acquires the per-parent-epic in-process lock (returned to the
// caller as unlock, released after File) and calls EpicChildren. A genuine
// query error releases the lock and returns a *workItemError 422
// work_item_invalid naming the failure and the fallback ("pass n explicitly"),
// with details {type, n_discovery_failed}.
//
// It ALSO refuses at the parent's CHILD CAP (#3714), on the result already in
// hand (no second round-trip): when the provider declares a hard cap and the
// epic already carries that many children, the {n} derivation would freeze at a
// number whose sub-issue link GitHub rejects, so every later filing renders the
// SAME [E<epic>.<n>] title (E68 #2885 produced seven [E68.67] issues). It
// unlocks FIRST — mirroring the EpicChildren-error and zero-match branches'
// unlock-before-return discipline — and returns the shared parentEpicFullError
// 422, BEFORE NextChildNumber and therefore before Apply and File, so nothing
// is created.
//
// It ALSO fails closed the same way
// (unlock, 422 work_item_invalid, details.n_discovery_failed) when the query
// succeeds but NextChildNumber cannot allocate — children exist yet none carry
// the numbered [E<epic>.<n>] form, so allocating 1 would collide (#2101). On
// success it sets filing.TitleVars["n"] = NextChildNumber(...) (with the
// mandatory nil-map guard, the #1184 precedent) and returns the still-held
// unlock so the caller serializes Apply + File under it.
//
// capacityChecked reports whether THIS call actually evaluated the parent-epic
// child cap — true only on the branch that called EpicChildren, false on every
// short-circuit above (unknown type, no {n} in title_format, explicit n, blank
// parent_epic, unresolved {epic}, unresolvable provider, no EpicChildrenQuerier).
// The caller runs guardParentEpicCapacity only when it is false, so the cap is
// evaluated exactly once per filing.
// The receiver is unused (discovery resolves the provider through the global
// workmgmt registry, not server config) but the method form mirrors
// deriveEpicTitleVar/discoverExistingNumbers and keeps the call site uniform.
func (*Server) deriveChildNumberTitleVar(ctx context.Context, filing *workmgmt.FilingRequest, conv workmgmt.Conventions, target workmgmt.Target) (unlockFn func(), capacityChecked bool, werr *workItemError) {
	itemType, ok := conv.Types[filing.Type]
	if !ok || !strings.Contains(itemType.TitleFormat, "{n}") {
		return nil, false, nil
	}
	if _, set := filing.TitleVars["n"]; set {
		// Explicit-n override: skip discovery AND the lock.
		return nil, false, nil
	}
	if strings.TrimSpace(filing.Relations.ParentEpic) == "" {
		return nil, false, nil
	}
	epic, ok := filing.TitleVars["epic"]
	if !ok || strings.TrimSpace(epic) == "" {
		// {epic} unresolved: leave {n} unset too so renderTitle's 422 reports
		// both missing placeholders.
		return nil, false, nil
	}
	provider, err := workmgmt.Get(conv.Provider)
	if err != nil {
		return nil, false, nil
	}
	querier, ok := provider.(workmgmt.EpicChildrenQuerier)
	if !ok {
		return nil, false, nil
	}

	// Discovery WILL run: serialize the allocate-then-file window per epic.
	unlock := lockChildNumberKey(childNumberLockKey(target, filing.Relations.ParentEpic))
	res, err := querier.EpicChildren(ctx, workmgmt.EpicChildrenRequest{
		Target: target,
		Epic:   filing.Relations.ParentEpic,
	})
	if err != nil {
		unlock()
		return nil, true, &workItemError{
			status: http.StatusUnprocessableEntity, code: "work_item_invalid",
			msg: fmt.Sprintf(
				"could not discover the child number for the parent epic %q: %s; pass n explicitly",
				filing.Relations.ParentEpic, err.Error()),
			details: map[string]any{
				"type":               filing.Type,
				"n_discovery_failed": err.Error(),
			},
		}
	}
	// The parent is already FULL: refuse here, before NextChildNumber can freeze
	// on a number whose link GitHub will reject (#3714). Unlock first, mirroring
	// the branches either side.
	if full, count, childCap := parentEpicAtChildCap(res); full {
		unlock()
		return nil, true, parentEpicFullError(filing.Type, strings.TrimSpace(filing.Relations.ParentEpic), count, childCap)
	}
	n, ok := workmgmt.NextChildNumber(itemType.TitleFormat, epic, res.Children)
	if !ok {
		// Children exist but none carry the numbered [E<epic>.<n>] form, so the
		// next number cannot be allocated without colliding with an existing
		// child (e.g. epic #389's placeholder [E22.X] corpus). Fail closed —
		// mirror the EpicChildren-error branch's unlock-before-return discipline.
		unlock()
		return nil, true, &workItemError{
			status: http.StatusUnprocessableEntity, code: "work_item_invalid",
			msg: fmt.Sprintf(
				"could not discover the child number for the parent epic %q: it has %d children but none carry the numbered [E%s.<n>] form; pass n explicitly",
				filing.Relations.ParentEpic, len(res.Children), epic),
			details: map[string]any{
				"type": filing.Type,
				"n_discovery_failed": fmt.Sprintf(
					"parent epic %q has %d children but none match the numbered [E%s.<n>] form",
					filing.Relations.ParentEpic, len(res.Children), epic),
			},
		}
	}
	// MANDATORY nil-map guard (#1184): allocate before assigning so a filing
	// that omits title_vars entirely does not panic.
	if filing.TitleVars == nil {
		filing.TitleVars = map[string]string{}
	}
	filing.TitleVars["n"] = strconv.Itoa(n)
	return unlock, true, nil
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
func (s *Server) deriveEpicTitleVar(ctx context.Context, filing *workmgmt.FilingRequest, conv workmgmt.Conventions, scope forge.CredentialScope, owner, name string) {
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
	if s.cfg.GitHub == nil || scope.IsZero() {
		return
	}
	number, err := parseEpicRef(filing.Relations.ParentEpic)
	if err != nil {
		return
	}
	issue, err := s.cfg.GitHub.GetIssue(ctx, scope, githubclient.RepoRef{Owner: owner, Name: name}, number)
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

// areaNamespace is the label namespace derived from the parent epic (#1616).
const areaNamespace = "area"

// phaseNamespace is the label namespace derived by derivePhaseLabel (#3179).
// Unlike area it has a SECOND rung below the parent epic — the originating
// run's triggering issue — because the epic_link:optional types (bug, chore)
// routinely file with no parent epic at all, which is exactly the defer-concern
// shape that left 48 children phase-unlabelled.
const phaseNamespace = "phase"

// deriveAreaLabel copies the parent epic's area:* label(s) onto the filing
// when the resolved type wants an area namespace (declares 'area' in
// required_label_namespaces or label_defaults), a parent epic is set, and no
// area:* label is already present in the caller's labels or the type's
// default_labels (#1616). It mutates filing.Labels in place (the caller passes
// a pointer) and returns the labels it derived so the handler can fold them
// into the item's DefaultedLabels for LOUD reporting.
//
// It fails OPEN on every failure mode — no GitHub client, no installation, an
// unparseable parent ref, a GetIssue error, or a parent epic with no area:*
// label — by deriving nothing, so Apply's completeness pass reports 'area' in
// missing_label_namespaces rather than the filing failing. It issues its own
// GetIssue (a second fetch alongside deriveEpicTitleVar) deliberately, to keep
// that derivation's fail-closed {epic} contract untouched.
//
// It is a thin wrapper over the namespace-parameterized core (#3179): the
// #1616 area behavior and every one of its fail-open modes is preserved
// byte-for-byte, which TestFileWorkItem_AreaDerivedFromParentEpic pins.
func (s *Server) deriveAreaLabel(ctx context.Context, filing *workmgmt.FilingRequest, conv workmgmt.Conventions, scope forge.CredentialScope, owner, name string) []string {
	itemType, ok := conv.Types[filing.Type]
	if !ok || !typeWantsNamespace(itemType, areaNamespace) {
		return nil
	}
	if strings.TrimSpace(filing.Relations.ParentEpic) == "" {
		return nil
	}
	if hasLabelInNamespace(filing.Labels, areaNamespace) || hasLabelInNamespace(itemType.DefaultLabels, areaNamespace) {
		return nil
	}
	if s.cfg.GitHub == nil || scope.IsZero() {
		return nil
	}
	number, err := parseEpicRef(filing.Relations.ParentEpic)
	if err != nil {
		return nil
	}
	return s.deriveLabelFromIssue(ctx, filing, scope, owner, name, areaNamespace, number)
}

// derivePhaseLabel copies a phase:* label onto the filing when the resolved
// type wants a phase namespace and the caller supplied none (#3179). Every
// auto-file path funnels through applyAndFileWorkItemWithIntake, so this is the
// single site that closes the gap the defer-concern path left: an item filed
// with area/autonomy/type but no phase is structurally unrankable by a
// phase-scoped grooming window.
//
// DERIVATION LADDER, in order, first hit wins:
//
//  1. A caller-supplied phase:* label (or one in the type's default_labels)
//     WINS outright — derivation never rewrites an explicit choice.
//  2. The PARENT EPIC's phase:* label(s). phase:* describes WHEN an item will
//     be worked, which follows its scheduling home — the epic it rolls up to —
//     not where it was discovered. So a concern deferred out of an alpha run
//     onto a beta epic correctly lands phase:beta.
//  3. The ORIGINATING RUN's triggering issue's phase:* label(s), resolved from
//     filing.Relations.EvidenceRuns[0]. This rung exists because the
//     epic_link:optional types (bug, chore — the defer-concern shape) routinely
//     carry no parent epic, and the run's own issue is a strictly better signal
//     than nothing.
//  4. Nothing — reported LOUDLY in missing_label_namespaces, never a rejection.
//
// SAME-REPO GUARD on rung 3: Relations.EvidenceRuns is CALLER-SUPPLIED on
// POST /v0/work-items and is NOT entitlement-checked (unlike the request's own
// run_id, which handleFileWorkItem gates). So the run row's Repo must equal the
// filing target owner/name or NOTHING is derived — reading a foreign run's
// triggering issue number and applying its phase against this target repo would
// derive a wrong label from an unowned row.
//
// It fails OPEN at every guard (returns nil): no GitHub client, no
// installation, no run repository, an unparseable run id or parent ref, a
// GetRun or GetIssue error, a cross-repo run, a run with no issue TriggerRef,
// or an issue carrying no phase:* label. Derived labels are system-added, so
// the caller folds them into DefaultedLabels — a wrong inherit is visible and
// challengeable at filing time rather than silent.
func (s *Server) derivePhaseLabel(ctx context.Context, filing *workmgmt.FilingRequest, conv workmgmt.Conventions, scope forge.CredentialScope, owner, name string) []string {
	itemType, ok := conv.Types[filing.Type]
	if !ok || !typeWantsNamespace(itemType, phaseNamespace) {
		return nil
	}
	// Rung 1: an explicit phase wins and is never rewritten.
	if hasLabelInNamespace(filing.Labels, phaseNamespace) || hasLabelInNamespace(itemType.DefaultLabels, phaseNamespace) {
		return nil
	}
	if s.cfg.GitHub == nil || scope.IsZero() {
		return nil
	}
	// Rung 2: the parent epic.
	if strings.TrimSpace(filing.Relations.ParentEpic) != "" {
		if number, err := parseEpicRef(filing.Relations.ParentEpic); err == nil {
			if derived := s.deriveLabelFromIssue(ctx, filing, scope, owner, name, phaseNamespace, number); len(derived) > 0 {
				return derived
			}
		}
	}
	// Rung 3: the originating run's triggering issue, same-repo only.
	number, ok := s.originatingRunIssue(ctx, filing, owner, name)
	if !ok {
		return nil
	}
	return s.deriveLabelFromIssue(ctx, filing, scope, owner, name, phaseNamespace, number)
}

// originatingRunIssue resolves the issue number that triggered the filing's
// first evidence run, for the phase-derivation fallback (#3179). It returns
// ok=false — deriving nothing — when there is no evidence run, no run
// repository, an unparseable id, a GetRun error, a run whose Repo is NOT the
// filing target repo (the same-repo guard on a caller-supplied, un-entitled
// id), or a run with no `issue:<n>` TriggerRef.
func (s *Server) originatingRunIssue(ctx context.Context, filing *workmgmt.FilingRequest, owner, name string) (int, bool) {
	if len(filing.Relations.EvidenceRuns) == 0 || s.cfg.RunRepo == nil {
		return 0, false
	}
	runID, err := uuid.Parse(strings.TrimSpace(filing.Relations.EvidenceRuns[0]))
	if err != nil {
		return 0, false
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil || runRow == nil {
		return 0, false
	}
	// SAME-REPO GUARD. See derivePhaseLabel's doc comment: the evidence-run id
	// is caller-supplied and un-entitled, so a run belonging to a different
	// repository derives NOTHING.
	if !strings.EqualFold(runRow.Repo, owner+"/"+name) {
		return 0, false
	}
	if runRow.TriggerRef == nil {
		return 0, false
	}
	number, ok := parseIssueRef(*runRow.TriggerRef)
	if !ok {
		return 0, false
	}
	return number, true
}

// deriveLabelFromIssue is the namespace-parameterized derivation core shared by
// deriveAreaLabel and derivePhaseLabel (#3179, generalizing #1616): it fetches
// the named issue in the filing target repo, collects every label carrying the
// `<ns>:` prefix, appends them to filing.Labels in place, and returns what it
// derived so the caller can fold them into DefaultedLabels for LOUD reporting.
//
// It fails OPEN — returns nil, deriving nothing — on a GetIssue error or an
// issue carrying no label in the namespace. It reads ONLY the label list of an
// issue in the repo the caller already has filing access to; no body, no
// private field.
func (s *Server) deriveLabelFromIssue(ctx context.Context, filing *workmgmt.FilingRequest, scope forge.CredentialScope, owner, name, ns string, issueNumber int) []string {
	issue, err := s.cfg.GitHub.GetIssue(ctx, scope, githubclient.RepoRef{Owner: owner, Name: name}, issueNumber)
	if err != nil {
		return nil
	}
	var derived []string
	for _, l := range issue.Labels {
		if strings.HasPrefix(l, ns+":") {
			derived = append(derived, l)
		}
	}
	if len(derived) == 0 {
		return nil
	}
	filing.Labels = append(filing.Labels, derived...)
	return derived
}

// typeWantsNamespace reports whether the type declares ns in its
// required_label_namespaces or label_defaults — the trigger for deriving that
// namespace from an issue. Generalized from typeWantsAreaNamespace (#3179).
func typeWantsNamespace(it workmgmt.ItemType, ns string) bool {
	if _, ok := it.LabelDefaults[ns]; ok {
		return true
	}
	for _, declared := range it.RequiredLabelNamespaces {
		if declared == ns {
			return true
		}
	}
	return false
}

// hasLabelInNamespace reports whether any label carries the "<ns>:" prefix. A
// server-local copy of the workmgmt predicate of the same name, deliberately
// NOT exported from workmgmt so the two packages stay decoupled.
func hasLabelInNamespace(labels []string, ns string) bool {
	for _, l := range labels {
		if strings.HasPrefix(l, ns+":") {
			return true
		}
	}
	return false
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
func (s *Server) auditWorkItemFiling(r *http.Request, activeRun *run.Run, item workmgmt.WorkItem, created *workmgmt.CreatedItem, subject string, signals *intakegroom.Signals) bool {
	if s.cfg.AuditRepo == nil || activeRun == nil || activeRun.State.IsTerminal() {
		return false
	}
	fields := map[string]any{
		"type":                     item.Type,
		"title":                    item.Title,
		"provider":                 created.Provider,
		"created_url":              created.URL,
		"created_number":           created.Number,
		"applied_labels":           created.AppliedLabels,
		"board_column":             created.BoardColumn,
		"status":                   created.Status,
		"defaulted_labels":         item.Classification.DefaultedLabels,
		"missing_label_namespaces": item.Classification.MissingLabelNamespaces,
	}
	// A COMPACT intake summary folded into the EXISTING work_item_filed
	// payload (#2239) — deliberately not a new audit category, so the
	// issue-comment surface inventory is untouched. It records what the
	// advisory hook saw, which is what makes a later "why was this filed as a
	// duplicate?" answerable from the chain.
	if signals != nil {
		fields["intake"] = intakeAuditSummary(*signals)
	}
	payload, _ := json.Marshal(fields)
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
