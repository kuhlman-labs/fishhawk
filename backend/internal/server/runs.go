package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// runResponse is the JSON shape POST /v0/runs and GET /v0/runs/{id}
// return. Field names + types match docs/api/v0.openapi.yaml's
// `Run` schema exactly so there's never a translation step between
// the OpenAPI doc and the wire format.
type runResponse struct {
	ID                 uuid.UUID  `json:"id"`
	Repo               string     `json:"repo"`
	WorkflowID         string     `json:"workflow_id"`
	WorkflowSHA        string     `json:"workflow_sha"`
	TriggerSource      string     `json:"trigger_source"`
	TriggerRef         *string    `json:"trigger_ref"`
	State              string     `json:"state"`
	ParentRunID        *uuid.UUID `json:"parent_run_id,omitempty"`
	DecomposedFrom     *uuid.UUID `json:"decomposed_from,omitempty"`
	PullRequestURL     *string    `json:"pull_request_url,omitempty"`
	RetryAttempt       int        `json:"retry_attempt"`
	MaxRetriesSnapshot int        `json:"max_retries_snapshot"`
	RunnerKind         string     `json:"runner_kind"`
	// Drive echoes the run's persisted drive-mode flag (#1023).
	// Always emitted; false for legacy rows (migration 0031).
	Drive        bool                 `json:"drive"`
	IssueContext *issueContextPayload `json:"issue_context,omitempty"`
	// CostUSDTotal is the rolled estimated USD cost of the run's model
	// usage from signed manifest token counts (#649). Always emitted (0
	// for legacy rows that predate the rollup).
	CostUSDTotal float64 `json:"cost_usd_total"`
	// ResolvedModel pins the agent model id the run executed under
	// (#649). Always emitted (empty string for legacy/unstamped runs).
	ResolvedModel string    `json:"resolved_model"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	// Concerns is the run's OPEN review-concern summary (#964): count,
	// per-state breakdown, and the stable IDs fishhawk_fixup_stage's
	// concern_ids addressing needs. Populated by handleGetRun ONLY —
	// toRunResponse never sets it, so the list endpoint stays free of a
	// per-row concern query (N+1). Omitted when the run has no open
	// concerns (or no concern store is configured).
	Concerns *runConcernsPayload `json:"concerns,omitempty"`
	// DerivedStatus is the presentation-only drive-mode status (#1023):
	// "awaiting_merge" while a non-terminal drive run with an open PR has
	// a checks_green_awaiting_merge auto-advance as its latest recorded
	// transition, or "ci_failed" (#1045) when its negative mirror — a
	// ci_failed stamp — is the latest. Never a persisted run.State — the state machine in
	// run/transition.go is untouched. Populated by handleGetRun ONLY
	// (distilled from run_auto_advanced audit entries, same single-read
	// posture as Concerns); omitted everywhere else.
	DerivedStatus string `json:"derived_status,omitempty"`
	// NextAction is the distilled operator next step from the most
	// recent run_auto_advanced audit entry (#1023): what (if anything)
	// the drive run is waiting on the operator for. Populated by
	// handleGetRun ONLY; omitted on non-drive runs, terminal runs, and
	// when the latest auto-advance carries no next action.
	NextAction *runNextActionPayload `json:"next_action,omitempty"`
	// AutoAdvanced lists the run's auto-advanced (or parked-with-next-
	// action) transitions distilled from run_auto_advanced audit entries
	// (#1023), oldest first. Populated by handleGetRun ONLY; omitted
	// when no drive transitions were recorded.
	AutoAdvanced []runAutoAdvancePayload `json:"auto_advanced,omitempty"`
	// Delegation is the evaluated operator_agent delegation surface
	// (ADR-040 / #1026): per-knob condition evaluations plus the
	// must_page_human list, so the operator agent never re-derives a
	// condition client-side. Populated by handleGetRun ONLY (same
	// single-read posture as Concerns); omitted when the run's cached
	// workflow spec declares no effective operator_agent block
	// (fail-closed — today's responses byte-identical), on terminal
	// runs, and best-effort on any evaluation failure.
	Delegation *runDelegationPayload `json:"delegation,omitempty"`
	// LineageComplete is the run's lineage-completion signal (E22.X /
	// #1137): true when the lineage-root run (a decomposed child's
	// parent, else the run itself) is terminal AND every decomposed
	// child of that root is terminal. The local-loop runner's worktree
	// sweep reads it to reclaim a terminal lineage's shared worktree at
	// the next provision. Populated by handleGetRun ONLY (a per-run
	// child-graph query, same single-read posture as Concerns); a
	// pointer so omitempty omits it on the list endpoint and whenever
	// the graph can't be read (no run repo / read failure), while still
	// distinguishing a computed false from absent.
	LineageComplete *bool `json:"lineage_complete,omitempty"`
}

// runNextActionPayload mirrors drive.NextAction on the wire. Kept
// distinct from the domain type so a payload-shape change in the audit
// trail can't silently leak through the API surface.
type runNextActionPayload struct {
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
	PRURL  string `json:"pr_url,omitempty"`
}

// runAutoAdvancePayload is one distilled drive transition: the rule
// that fired, the from/to edge it stamps, whether the mechanical rule
// parked instead of executing (runner_kind local dispatch), and when
// the audit entry landed.
type runAutoAdvancePayload struct {
	Rule      string    `json:"rule"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Parked    bool      `json:"parked,omitempty"`
	Timestamp time.Time `json:"ts"`
}

// runConcernsPayload summarizes a run's OPEN review concerns (#964).
// Items carries id/stage_kind/severity/category/state only — the note
// text is intentionally elided (bounded payload); read the originating
// *_reviewed audit entry for the full note.
type runConcernsPayload struct {
	Open    int                 `json:"open"`
	ByState map[string]int      `json:"by_state"`
	Items   []runConcernPayload `json:"items"`
}

// runConcernPayload is one open concern on the wire.
type runConcernPayload struct {
	ID        uuid.UUID `json:"id"`
	StageKind string    `json:"stage_kind"`
	Severity  string    `json:"severity"`
	Category  string    `json:"category"`
	State     string    `json:"state"`
}

// runDelegationPayload is the operator_agent delegation surface on the
// wire (ADR-040 / #1026): one entry per configured may_* knob, each
// carrying the named condition and whether current run state satisfies
// it, plus the effective block's must_page_human event list. Kept
// distinct from delegation.Result per the domain/wire separation the
// drive payloads follow.
type runDelegationPayload struct {
	Actions       []runDelegationActionPayload `json:"actions"`
	MustPageHuman []string                     `json:"must_page_human,omitempty"`
}

// runDelegationActionPayload is one knob's evaluation on the wire.
// unmet_reason names the exact failed predicate when met is false.
type runDelegationActionPayload struct {
	Action      string `json:"action"`
	Condition   string `json:"condition"`
	Met         bool   `json:"met"`
	UnmetReason string `json:"unmet_reason,omitempty"`
}

// issueContextPayload mirrors run.IssueContext on the wire. Kept
// distinct from the domain type so a future field change in the
// store doesn't accidentally leak through the API surface.
type issueContextPayload struct {
	Title    string                `json:"title"`
	Body     string                `json:"body"`
	URL      string                `json:"url"`
	Number   int                   `json:"number"`
	Comments []issueCommentPayload `json:"comments,omitempty"`
}

// issueCommentPayload mirrors run.IssueComment on the wire. Field
// names match the OpenAPI issue-comment schema (required [author,
// body, created_at]) that #618 published; because the POST /v0/runs
// decoder uses DisallowUnknownFields, these tags must stay exactly
// comments/author/body/created_at or a commented issue 400s.
type issueCommentPayload struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

func toRunResponse(r *run.Run) runResponse {
	resp := runResponse{
		ID:                 r.ID,
		Repo:               r.Repo,
		WorkflowID:         r.WorkflowID,
		WorkflowSHA:        r.WorkflowSHA,
		TriggerSource:      string(r.TriggerSource),
		TriggerRef:         r.TriggerRef,
		State:              string(r.State),
		ParentRunID:        r.ParentRunID,
		DecomposedFrom:     r.DecomposedFrom,
		PullRequestURL:     r.PullRequestURL,
		RetryAttempt:       r.RetryAttempt,
		MaxRetriesSnapshot: r.MaxRetriesSnapshot,
		RunnerKind:         r.RunnerKind,
		Drive:              r.Drive,
		CostUSDTotal:       r.CostUSDTotal,
		ResolvedModel:      r.ResolvedModel,
		CreatedAt:          r.CreatedAt,
		UpdatedAt:          r.UpdatedAt,
	}
	if r.IssueContext != nil {
		resp.IssueContext = &issueContextPayload{
			Title:  r.IssueContext.Title,
			Body:   r.IssueContext.Body,
			URL:    r.IssueContext.URL,
			Number: r.IssueContext.Number,
		}
		if len(r.IssueContext.Comments) > 0 {
			comments := make([]issueCommentPayload, len(r.IssueContext.Comments))
			for i, c := range r.IssueContext.Comments {
				comments[i] = issueCommentPayload{
					Author:    c.Author,
					Body:      c.Body,
					CreatedAt: c.CreatedAt,
				}
			}
			resp.IssueContext.Comments = comments
		}
	}
	return resp
}

// createRunRequest mirrors POST /v0/runs's request body in
// v0.openapi.yaml. All four required fields must be present and
// non-empty; trigger_ref and runner_kind are optional.
type createRunRequest struct {
	Repo          string  `json:"repo"`
	WorkflowID    string  `json:"workflow_id"`
	WorkflowSHA   string  `json:"workflow_sha"`
	TriggerSource string  `json:"trigger_source"`
	TriggerRef    *string `json:"trigger_ref,omitempty"`
	// RunnerKind tags the execution backend per ADR-022 / #388.
	// Optional; defaults to github_actions when omitted (the v0
	// dominant case). The local-runner CLI (Phase C of E22 / #389)
	// passes `local`. Validated against `run.ValidRunnerKinds` at
	// the handler.
	RunnerKind string `json:"runner_kind,omitempty"`
	// WorkflowSpec is the YAML bytes of the workflow spec at the
	// requested workflow_sha, transported as a string. When
	// supplied (#411), the handler parses it, validates that
	// workflow_id is defined, persists the bytes on the run row
	// (matching the dispatcher's caching path), and creates one
	// Stage row per stage definition. When absent, the run row is
	// created with no stages — the legacy behaviour, retained so
	// API callers that just want to register a run record without
	// driving a lifecycle (e.g. integration test seeding) keep
	// working. The CLI's `fishhawk run start` flow always sends
	// the bytes via auto-discovery of `.fishhawk/workflows.yaml`
	// or the explicit --spec-file flag.
	WorkflowSpec string `json:"workflow_spec,omitempty"`
	// IssueContext is the cached payload from the operator's
	// `gh issue view` for issue-triggered runs minted outside the
	// webhook flow (#415). The CLI fetches the issue locally and
	// ships title/body/url/number inline so the prompt builder
	// has the full context — webhook-dispatched runs leave this
	// nil and fall through to the existing GitHub-fetch path
	// inside prompt.fillIssueContext. Only honored when
	// trigger_source=github_issue; ignored otherwise so the
	// shape can't be abused to attach prose to non-issue runs.
	IssueContext *issueContextPayload `json:"issue_context,omitempty"`
	// Drive is the per-run drive-mode override (#1023 / #996 theme
	// 1). A pointer so absence is distinguishable from an explicit
	// false: nil inherits the workflow spec's `drive` default (false
	// when the spec doesn't set one or no spec is resolved); set, it
	// wins over the spec value. The resolved flag is snapshotted on
	// the run row at create time.
	Drive *bool `json:"drive,omitempty"`
	// BudgetOverride lets an operator force a run past a blocking
	// periodic budget that is over its limit for the current period
	// (#688 / ADR-030). When true and a blocking budget would
	// otherwise refuse the run with 402 budget_exhausted, the run is
	// admitted and a run_admitted_budget_override audit entry is
	// recorded. Ignored when no blocking budget is over — the field
	// only matters at the moment a budget would block.
	BudgetOverride bool `json:"budget_override,omitempty"`
}

// validTriggerSources is the closed set per the workflow-spec and
// OpenAPI surface. New sources land in v0.x and require an explicit
// schema bump (see MVP_SPEC §7.1).
var validTriggerSources = map[string]struct{}{
	string(run.TriggerGitHubIssue): {},
	string(run.TriggerCLI):         {},
	string(run.TriggerUI):          {},
}

// handleCreateRun implements POST /v0/runs. Validates the request
// body, calls into the run repository, and returns the canonical
// Run JSON. The state machine starts every new run in
// run.StatePending.
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, "write:runs") {
		return
	}
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"runs endpoint requires a configured run repository", nil)
		return
	}

	var req createRunRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return
	}

	if req.Repo == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo is required", map[string]any{"field": "repo"})
		return
	}
	if req.WorkflowID == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"workflow_id is required", map[string]any{"field": "workflow_id"})
		return
	}
	if req.WorkflowSHA == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"workflow_sha is required", map[string]any{"field": "workflow_sha"})
		return
	}
	if _, ok := validTriggerSources[req.TriggerSource]; !ok {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"trigger_source must be one of github_issue, cli, ui",
			map[string]any{"field": "trigger_source", "got": req.TriggerSource})
		return
	}
	if req.RunnerKind != "" {
		if _, ok := run.ValidRunnerKinds[req.RunnerKind]; !ok {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"runner_kind must be one of github_actions, local",
				map[string]any{"field": "runner_kind", "got": req.RunnerKind})
			return
		}
	}

	// IssueContext is only meaningful for issue-triggered runs
	// (#415). Reject the field on non-issue triggers up front so
	// the shape stays narrow — better a clear 400 now than a
	// prompt-time surprise when the cached payload turns out to
	// refer to nothing the prompt template will reference.
	if req.IssueContext != nil && req.TriggerSource != string(run.TriggerGitHubIssue) {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"issue_context is only valid with trigger_source=github_issue",
			map[string]any{"field": "issue_context", "trigger_source": req.TriggerSource})
		return
	}

	// Parse + validate the optional workflow_spec up front (#411).
	// Failing before idempotency / repository writes keeps a bad
	// spec from creating a half-formed run row. parsed is nil when
	// the caller didn't ship a spec — handled at the create-stages
	// step below.
	//
	// specBytes holds the raw YAML that gets cached on the run row.
	// Set by the inline-spec path or the GitHub-fetch fallback (#413).
	var (
		parsed         *spec.Spec
		workflowDef    spec.Workflow
		haveStageDefs  bool
		maxRetriesSnap int
		specBytes      []byte
	)

	// Resolve the repo's GitHub App installation best-effort (#713).
	// This runs for BOTH the inline-spec path (the one MCP/local runs
	// take — they ship the spec inline, so they'd otherwise skip the
	// GitHub-fetch branch below and never resolve an installation) and
	// the GitHub-fetch path. The resolved id is stamped onto the run row
	// so the runner's push_and_open_pr can mint an installation token and
	// the merge reconciler can poll the PR. When no installation is
	// attributable (non-App / local setup), the id stays nil and the
	// runner falls back to the operator's `gh` CLI token — so we do NOT
	// hard-fail here. The GitHub-fetch branch below DOES hard-fail on
	// ErrNotInstalled (it cannot read the spec without an installation);
	// it reuses installResolveErr instead of calling GetRepoInstallation
	// a second time.
	var (
		installationID    *int64
		installResolveErr error
	)
	if s.cfg.GitHub != nil {
		if owner, name, ok := strings.Cut(req.Repo, "/"); ok && owner != "" && name != "" {
			id, err := s.cfg.GitHub.GetRepoInstallation(r.Context(), githubclient.RepoRef{Owner: owner, Name: name})
			switch {
			case err == nil:
				installationID = &id
			case errors.Is(err, githubclient.ErrNotInstalled):
				installResolveErr = err
			default:
				installResolveErr = err
				s.cfg.Logger.Warn("resolve repo installation failed; run created without an attributed installation",
					"repo", req.Repo, "error", err.Error())
			}
		}
	}

	if req.WorkflowSpec != "" {
		specBytes = []byte(req.WorkflowSpec)
		p, err := spec.ParseBytes(specBytes)
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"workflow_spec failed to parse",
				map[string]any{"field": "workflow_spec", "error": err.Error()})
			return
		}
		wf, ok := p.Workflows[req.WorkflowID]
		if !ok {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"workflow_id not defined in workflow_spec",
				map[string]any{"field": "workflow_id", "got": req.WorkflowID})
			return
		}
		if len(wf.Stages) == 0 {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"workflow has no stages",
				map[string]any{"field": "workflow_id", "got": req.WorkflowID})
			return
		}
		parsed = p
		workflowDef = wf
		haveStageDefs = true
		maxRetriesSnap = webhook.WorkflowMaxRetries(wf)
	} else if s.cfg.GitHub != nil {
		// workflow_spec omitted but GitHub client is configured (#413).
		// Resolve the App's installation for the repo, then fetch the
		// spec at workflow_sha. Used by MCP-driven runs and cross-repo
		// CLI invocations that can't easily ship the spec inline.
		owner, name, ok := strings.Cut(req.Repo, "/")
		if !ok || owner == "" || name == "" {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"repo must be in owner/name format",
				map[string]any{"field": "repo", "got": req.Repo})
			return
		}
		// Reuse the installation resolved above. Unlike the inline path,
		// the fetch path genuinely needs the installation to read the
		// spec, so it hard-fails: ErrNotInstalled → 422, any other
		// resolve error → 500.
		if installResolveErr != nil {
			if errors.Is(installResolveErr, githubclient.ErrNotInstalled) {
				s.writeError(w, r, http.StatusUnprocessableEntity, "repo_not_installed",
					"GitHub App is not installed on the target repository",
					map[string]any{"repo": req.Repo})
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"could not resolve installation for repo",
				map[string]any{"error": installResolveErr.Error()})
			return
		}
		repoRef := githubclient.RepoRef{Owner: owner, Name: name}
		fc, err := s.cfg.GitHub.GetWorkflowSpec(r.Context(), *installationID, repoRef, req.WorkflowSHA)
		if err != nil {
			if errors.Is(err, githubclient.ErrNotFound) {
				s.writeError(w, r, http.StatusUnprocessableEntity, "spec_not_found",
					"workflow spec not found at the given sha",
					map[string]any{"repo": req.Repo, "workflow_sha": req.WorkflowSHA})
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"could not fetch workflow spec from repo",
				map[string]any{"error": err.Error()})
			return
		}
		specBytes = fc.Content
		p, err := spec.ParseBytes(specBytes)
		if err != nil {
			s.writeError(w, r, http.StatusUnprocessableEntity, "spec_not_found",
				"fetched workflow spec failed to parse",
				map[string]any{"error": err.Error()})
			return
		}
		wf, ok := p.Workflows[req.WorkflowID]
		if !ok {
			s.writeError(w, r, http.StatusUnprocessableEntity, "spec_not_found",
				"workflow_id not defined in fetched workflow spec",
				map[string]any{"field": "workflow_id", "got": req.WorkflowID})
			return
		}
		if len(wf.Stages) == 0 {
			s.writeError(w, r, http.StatusUnprocessableEntity, "spec_not_found",
				"workflow in fetched spec has no stages",
				map[string]any{"field": "workflow_id", "got": req.WorkflowID})
			return
		}
		parsed = p
		workflowDef = wf
		haveStageDefs = true
		maxRetriesSnap = webhook.WorkflowMaxRetries(wf)
	}
	_ = parsed // parsed is reserved for future spec-driven checks

	// Plan-review wiring guard (#574 / ADR-027 / #955). When the resolved
	// spec declares an agent-gated plan review (effective agent count > 0,
	// human == 0) that the wired reviewer set cannot satisfy — no default
	// adapter for the bare count form, or an agents-list entry naming an
	// unconfigured provider — agent review would be silently degraded at
	// plan-upload time, minting a run that can never satisfy its own gate.
	// Reject at create time with a run_rejected_misconfigured audit trail
	// rather than letting the run proceed past a gate that does not exist.
	// Advisory mode (human > 0) is allowed through: the human gate remains
	// authoritative and the review loops emit plan_review_skipped /
	// *_review_failed audit entries for the missing agent layer.
	if haveStageDefs {
		for _, st := range workflowDef.Stages {
			if st.Type != spec.StageTypePlan || st.Reviewers == nil {
				continue
			}
			if planreview.ResolveAuthority(*st.Reviewers) != planreview.AuthorityGating {
				continue
			}
			problem := s.gatingReviewerProblem(st.Reviewers)
			if problem == "" {
				continue
			}
			if s.cfg.AuditRepo != nil {
				payload, _ := json.Marshal(map[string]any{
					"reason":            "plan_reviewer_unconfigured",
					"stage":             st.ID,
					"workflow_id":       req.WorkflowID,
					"repo":              req.Repo,
					"configured_agents": st.Reviewers.AgentCount(),
				})
				systemKind := audit.ActorKind("system")
				if _, aerr := s.cfg.AuditRepo.AppendGlobalChained(r.Context(), audit.GlobalChainAppendParams{
					Timestamp: time.Now().UTC(),
					Category:  "run_rejected_misconfigured",
					ActorKind: &systemKind,
					Payload:   payload,
				}); aerr != nil {
					s.cfg.Logger.Warn("append run_rejected_misconfigured audit entry failed",
						"repo", req.Repo, "workflow_id", req.WorkflowID, "error", aerr.Error())
				}
			}
			s.writeError(w, r, http.StatusBadRequest, "plan_reviewer_unconfigured", problem, nil)
			return
		}
	}

	// Blocking periodic-budget admission gate (#688 / ADR-030). When
	// the resolved workflow declares an enforcement:blocking budget
	// whose current-period spend has reached limit_usd, refuse a NEW
	// run with 402 budget_exhausted unless the operator passed
	// budget_override. No-stage / no-spec requests have empty Budgets
	// and pass through. checkBlockingBudget writes the error response
	// (and the audit) on refusal.
	if haveStageDefs && !s.checkBlockingBudget(w, r, req.Repo, req.WorkflowID, workflowDef.Budgets, req.BudgetOverride) {
		return
	}

	// Idempotency-Key (E8.2 / #40). When set, a previously-created
	// run with the same (repo, key) is returned 200 instead of
	// fresh-creating + dispatching a duplicate. Empty header is
	// equivalent to "not idempotent" — every call mints a new run.
	idempKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempKey != "" {
		existing, err := s.cfg.RunRepo.GetRunByIdempotencyKey(r.Context(), req.Repo, idempKey)
		switch {
		case err == nil:
			// Replay: return the prior run with 200 (not 201).
			// 200 is the idempotency convention — clients that
			// react to "201 Created" by, e.g., posting a Slack
			// notification get a chance to no-op on the replay.
			s.writeJSON(w, r, http.StatusOK, toRunResponse(existing))
			return
		case errors.Is(err, run.ErrNotFound):
			// First call with this key — fall through to create.
		default:
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"idempotency lookup failed", map[string]any{"error": err.Error()})
			return
		}
	}

	createParams := run.CreateRunParams{
		Repo:          req.Repo,
		WorkflowID:    req.WorkflowID,
		WorkflowSHA:   req.WorkflowSHA,
		TriggerSource: run.TriggerSource(req.TriggerSource),
		TriggerRef:    req.TriggerRef,
		// Empty req.RunnerKind → repo layer applies the default
		// (RunnerKindGitHubActions). Explicit values are validated
		// above; only known-good kinds reach the repo.
		RunnerKind: req.RunnerKind,
		// Best-effort App installation resolved above (#713). Nil when
		// no App is installed on the repo (local / non-App setup); the
		// runner then falls back to the operator's `gh` CLI token.
		InstallationID: installationID,
	}
	if haveStageDefs {
		// Cache the validated spec bytes on the row so the trace
		// handler's policy re-evaluation reads constraints from
		// storage instead of refetching (mirrors the dispatcher
		// path; see dispatcher.createRun). specBytes is set by
		// either the inline-spec or the GitHub-fetch path above.
		createParams.WorkflowSpec = specBytes
		createParams.MaxRetriesSnapshot = maxRetriesSnap
		// Drive default from the workflow spec (#1023); the
		// per-run override below wins when present.
		createParams.Drive = workflowDef.Drive
	}
	if req.Drive != nil {
		createParams.Drive = *req.Drive
	}
	if req.IssueContext != nil {
		createParams.IssueContext = &run.IssueContext{
			Title:  req.IssueContext.Title,
			Body:   req.IssueContext.Body,
			URL:    req.IssueContext.URL,
			Number: req.IssueContext.Number,
		}
		if len(req.IssueContext.Comments) > 0 {
			comments := make([]run.IssueComment, len(req.IssueContext.Comments))
			for i, c := range req.IssueContext.Comments {
				comments[i] = run.IssueComment{
					Author:    c.Author,
					Body:      c.Body,
					CreatedAt: c.CreatedAt,
				}
			}
			createParams.IssueContext.Comments = comments
		}
	}
	if idempKey != "" {
		k := idempKey
		createParams.IdempotencyKey = &k
	}

	created, err := s.cfg.RunRepo.CreateRun(r.Context(), createParams)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"create run failed", map[string]any{"error": err.Error()})
		return
	}

	// Create one Stage row per stage definition in the spec.
	// Required-checks-snapshot capture is deliberately skipped for
	// API-created runs — the snapshot lives behind GitHub's
	// branch-protection API and the API path doesn't carry an
	// installation token to query it. Local-runner runs don't need
	// the snapshot (no PR merge gate enforcement); github_actions
	// runs minted via this path can be backfilled by the webhook
	// trace handler if needed.
	if haveStageDefs {
		if _, err := webhook.CreateStagesFromSpec(r.Context(), s.cfg.RunRepo, created.ID, workflowDef.Stages); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"create stages failed", map[string]any{"error": err.Error()})
			return
		}
	}

	// Emit the run_started board transition for the just-created run
	// (#1123). This is the local-runner / API run-creation emit point
	// that parallels the webhook dispatcher's run_started emit
	// (dispatcher.go) — without it a local-runner/MCP run's card never
	// leaves Backlog, and every later board edge then SKIPS on the
	// never-fight-the-human expected-source gate. We call
	// boardTransitionForRun directly with the already-fetched run (it
	// carries TriggerRef/Repo/InstallationID) rather than
	// notifyBoardTransition, avoiding a redundant GetRun. It is
	// best-effort: the hook no-ops silently for a non-issue TriggerRef
	// (ad-hoc CLI runs) and for unconfigured conventions, and board
	// failures only WARN-log — they can never unwind the create-run
	// response.
	s.boardTransitionForRun(r.Context(), created, lifecycleRunStarted)

	s.writeJSON(w, r, http.StatusCreated, toRunResponse(created))
}

// handleGetRun implements GET /v0/runs/{run_id}. Returns 404 with
// the run_not_found code if the ID doesn't resolve, and 400 if the
// path parameter isn't a valid UUID.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"runs endpoint requires a configured run repository", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	got, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
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

	resp := toRunResponse(got)
	// Attach the open-concern summary (#964) on the single-run read
	// ONLY — the list endpoint deliberately omits it (no N+1 concern
	// query per row). Best-effort: a concern-store failure warn-logs
	// and the field is omitted rather than failing the run read.
	if s.cfg.ConcernRepo != nil {
		open, cerr := s.cfg.ConcernRepo.ListOpenByRun(r.Context(), runID)
		if cerr != nil {
			s.cfg.Logger.Warn("list open concerns failed; omitting concerns block",
				"run_id", runID.String(), "error", cerr.Error())
		} else {
			resp.Concerns = buildRunConcernsPayload(open)
		}
	}
	// Drive read surfaces (#1023): auto_advanced + next_action +
	// derived_status, distilled from the run's run_auto_advanced audit
	// entries. Single-run read ONLY (same posture as Concerns — no
	// per-row audit query on the list endpoint), gated on the run's
	// Drive flag so non-drive runs never pay the audit read. Best-
	// effort: a read failure warn-logs and the fields are omitted
	// rather than failing the run read.
	if got.Drive && s.cfg.AuditRepo != nil {
		entries, derr := s.cfg.AuditRepo.ListForRunByCategory(r.Context(), runID, drive.Category)
		if derr != nil {
			s.cfg.Logger.Warn("list run_auto_advanced failed; omitting drive surfaces",
				"run_id", runID.String(), "error", derr.Error())
		} else {
			applyDriveSurfaces(&resp, got, entries)
		}
	}
	// Delegation surface (ADR-040 / #1026): evaluated on the single-run
	// read ONLY, same posture as Concerns. Omitted (nil) when the run's
	// spec declares no operator_agent block, on terminal runs, and
	// best-effort on any evaluation failure.
	resp.Delegation = s.buildDelegationPayload(r.Context(), got)
	// Lineage-completion signal (E22.X / #1137): single-run read ONLY,
	// same posture as Concerns. Omitted (nil) when no run repo is wired
	// or the child-graph read fails (best-effort).
	resp.LineageComplete = s.lineageComplete(r.Context(), got)
	s.writeJSON(w, r, http.StatusOK, resp)
}

// buildDelegationPayload evaluates the run's operator_agent delegation
// conditions (ADR-040 / #1026) for the single-run read. Returns nil —
// the field is omitted — when the run is terminal, carries no cached
// workflow spec (legacy rows), resolves no effective operator_agent
// block (fail-closed: nothing is delegated), the evaluator's
// repositories aren't wired, or the evaluation fails (best-effort,
// mirroring the Concerns / drive-surfaces degradation: warn-log and
// omit, never fail the read).
func (s *Server) buildDelegationPayload(ctx context.Context, runRow *run.Run) *runDelegationPayload {
	if runRow.State.IsTerminal() || len(runRow.WorkflowSpec) == 0 {
		return nil
	}
	if s.cfg.RunRepo == nil || s.cfg.ConcernRepo == nil || s.cfg.AuditRepo == nil {
		return nil
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.Warn("delegation: parse workflow spec failed; omitting delegation block",
			"run_id", runRow.ID.String(), "error", err.Error())
		return nil
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		s.cfg.Logger.Warn("delegation: workflow not in cached spec; omitting delegation block",
			"run_id", runRow.ID.String(), "workflow_id", runRow.WorkflowID)
		return nil
	}
	ev := &delegation.Evaluator{
		Stages:   s.cfg.RunRepo,
		Concerns: s.cfg.ConcernRepo,
		Audit:    s.cfg.AuditRepo,
	}
	res, err := ev.Evaluate(ctx, runRow, &wf)
	if err != nil {
		s.cfg.Logger.Warn("delegation: evaluate failed; omitting delegation block",
			"run_id", runRow.ID.String(), "error", err.Error())
		return nil
	}
	if res == nil {
		return nil
	}
	out := &runDelegationPayload{
		Actions:       make([]runDelegationActionPayload, 0, len(res.Actions)),
		MustPageHuman: res.MustPageHuman,
	}
	for _, d := range res.Actions {
		out.Actions = append(out.Actions, runDelegationActionPayload{
			Action:      d.Action,
			Condition:   string(d.Condition),
			Met:         d.Met,
			UnmetReason: d.UnmetReason,
		})
	}
	return out
}

// applyDriveSurfaces distills run_auto_advanced audit entries into the
// single-run read's drive fields (#1023): the full auto_advanced
// transition list (oldest first), the most recent entry's next_action,
// and the derived awaiting_merge presentation status. next_action and
// derived_status are suppressed on terminal runs — once the run
// completes, the recorded next step is history, not an instruction.
// awaiting_merge additionally requires an open PR on the row, mirroring
// the {gates resolved, checks green, PR open} derivation the
// checks_green_awaiting_merge stamp encodes at emission time. Corrupt
// payloads are skipped: the surface degrades to the readable entries.
func applyDriveSurfaces(resp *runResponse, runRow *run.Run, entries []*audit.Entry) {
	if len(entries) == 0 {
		return
	}
	// Defensive sort; the postgres repository already returns the
	// per-run chain sequence-ascending. Stable so fakes that surface
	// unsequenced entries keep their append order.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Sequence < entries[j].Sequence
	})
	var latest *drive.Advance
	for _, e := range entries {
		var adv drive.Advance
		if json.Unmarshal(e.Payload, &adv) != nil {
			continue
		}
		resp.AutoAdvanced = append(resp.AutoAdvanced, runAutoAdvancePayload{
			Rule:      string(adv.Rule),
			From:      adv.From,
			To:        adv.To,
			Parked:    adv.Parked,
			Timestamp: e.Timestamp,
		})
		latest = &adv
	}
	if latest == nil || runRow.State.IsTerminal() {
		return
	}
	if latest.NextAction != nil {
		resp.NextAction = &runNextActionPayload{
			Action: latest.NextAction.Action,
			Detail: latest.NextAction.Detail,
			PRURL:  latest.NextAction.PRURL,
		}
	}
	if latest.Rule == drive.RuleChecksGreenAwaitingMerge && runRow.PullRequestURL != nil {
		resp.DerivedStatus = "awaiting_merge"
	}
	// Negative mirror (#1045): the latest stamp is ci_failed and the PR
	// is open → the derived ci_failed presentation status. Keying off the
	// LATEST entry means a later checks_green/fixup_rereview_repark stamp
	// naturally supersedes it (and vice versa) with no separate
	// supersession code.
	if latest.Rule == drive.RuleCIFailed && runRow.PullRequestURL != nil {
		resp.DerivedStatus = "ci_failed"
	}
}

// buildRunConcernsPayload renders the open-concern summary for the
// single-run read. Returns nil (field omitted) when there is nothing
// open.
func buildRunConcernsPayload(open []*concern.Concern) *runConcernsPayload {
	if len(open) == 0 {
		return nil
	}
	out := &runConcernsPayload{
		Open:    len(open),
		ByState: make(map[string]int, 3),
		Items:   make([]runConcernPayload, 0, len(open)),
	}
	for _, c := range open {
		out.ByState[string(c.State)]++
		out.Items = append(out.Items, runConcernPayload{
			ID:        c.ID,
			StageKind: c.StageKind,
			Severity:  c.Severity,
			Category:  c.Category,
			State:     string(c.State),
		})
	}
	return out
}

const (
	runsDefaultLimit = 50
	runsMaxLimit     = 200
)

// handleListRuns implements GET /v0/runs. Cursor-paginated by
// created_at DESC; filter params (repo, workflow_id, state) are
// additive — multiple filters AND together. Cursor encoding is
// shared with the audit endpoint via pageOffset / encodeOffsetCursor.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"runs endpoint requires a configured run repository", nil)
		return
	}
	q := r.URL.Query()
	limit, err := parseLimit(q.Get("limit"), runsDefaultLimit, runsMaxLimit)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			err.Error(), map[string]any{"field": "limit"})
		return
	}
	offset, err := decodeOffsetCursor(q.Get("cursor"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "cursor_invalid",
			err.Error(), nil)
		return
	}
	stateFilter := q.Get("state")
	if stateFilter != "" {
		if _, ok := validRunStates[stateFilter]; !ok {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"state must be one of pending, running, succeeded, failed, cancelled",
				map[string]any{"field": "state", "got": stateFilter})
			return
		}
	}

	// pull_request_url and trigger_ref are optional equality
	// filters introduced in #216 for the threaded-runs view.
	// Empty value = no constraint (matches the SQL convention);
	// any non-empty value is passed verbatim.
	var prURLFilter *string
	if v := q.Get("pull_request_url"); v != "" {
		prURLFilter = &v
	}
	var triggerRefFilter *string
	if v := q.Get("trigger_ref"); v != "" {
		triggerRefFilter = &v
	}
	// runner_kind is the ADR-022 / #388 filter — compliance
	// consumers project to `github_actions` only to reproduce the
	// pre-pluggable-backends view. Validated against the closed
	// set so bad values surface as a clean 400 (not a silent
	// no-results page).
	var runnerKindFilter *string
	if v := q.Get("runner_kind"); v != "" {
		if _, ok := run.ValidRunnerKinds[v]; !ok {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"runner_kind must be one of github_actions, local",
				map[string]any{"field": "runner_kind", "got": v})
			return
		}
		runnerKindFilter = &v
	}

	// Fetch one extra row so we can tell whether there's a next
	// page without a separate COUNT query. The trick: ask for
	// limit+1, drop the extra in the response if present.
	rows, err := s.cfg.RunRepo.ListRuns(r.Context(), run.ListRunsFilter{
		Repo:           q.Get("repo"),
		WorkflowID:     q.Get("workflow_id"),
		State:          stateFilter,
		PullRequestURL: prURLFilter,
		TriggerRef:     triggerRefFilter,
		RunnerKind:     runnerKindFilter,
		Limit:          limit + 1,
		Offset:         offset,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list runs failed", map[string]any{"error": err.Error()})
		return
	}

	var nextCursor string
	if len(rows) > limit {
		nextCursor = encodeOffsetCursor(offset + limit)
		rows = rows[:limit]
	}
	items := make([]runResponse, 0, len(rows))
	for _, ru := range rows {
		items = append(items, toRunResponse(ru))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"items":       items,
		"next_cursor": nextCursor,
	})
}

// validRunStates pins the closed set per docs/api/v0.openapi.yaml.
// Schema constraint mirrors backend/internal/postgres/migrations/0001
// CHECK; defense-in-depth at the handler keeps a typo from
// reaching the DB layer.
var validRunStates = map[string]struct{}{
	string(run.StatePending):   {},
	string(run.StateRunning):   {},
	string(run.StateSucceeded): {},
	string(run.StateFailed):    {},
	string(run.StateCancelled): {},
}

// handleCancelRun implements POST /v0/runs/{run_id}/cancel.
// Idempotent: cancelling an already-cancelled run returns 200 with
// the same body as a fresh cancel. Cancelling a terminally-completed
// run (succeeded / failed) returns 409 because the state machine
// rejects the transition.
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, "write:runs") {
		return
	}
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"runs endpoint requires a configured run repository", nil)
		return
	}
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	got, err := s.cfg.RunRepo.TransitionRun(r.Context(), runID, run.StateCancelled)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		var inv run.InvalidTransitionError
		if errors.As(err, &inv) {
			s.writeError(w, r, http.StatusConflict, "invalid_state_transition",
				err.Error(),
				map[string]any{"run_id": runID.String(), "from": inv.From, "to": inv.To})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"cancel run failed", map[string]any{"error": err.Error()})
		return
	}

	s.writeJSON(w, r, http.StatusOK, toRunResponse(got))
}

// postStatusCommentRequest is the JSON body for POST /v0/runs/{run_id}/status-comment.
type postStatusCommentRequest struct {
	GithubCommentID int64 `json:"github_comment_id"`
}

// handlePostStatusComment implements POST /v0/runs/{run_id}/status-comment.
// Records the CLI-posted GitHub comment id into a status_comment_posted audit
// entry using the same payload shape as the GHA sticky-comment path, so that
// findStatusCommentID in the Notifier picks up local-path entries naturally
// on subsequent GHA-side transitions (#428).
func (s *Server) handlePostStatusComment(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"status-comment endpoint requires a configured run repository", nil)
		return
	}
	if s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "audit_repo_unconfigured",
			"status-comment endpoint requires a configured audit repository", nil)
		return
	}
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	var req postStatusCommentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON",
			map[string]any{"error": err.Error()})
		return
	}
	if req.GithubCommentID <= 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"github_comment_id must be a positive integer",
			map[string]any{"field": "github_comment_id"})
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

	var issueNumber int
	if runRow.IssueContext != nil {
		issueNumber = runRow.IssueContext.Number
	}
	payload, _ := json.Marshal(map[string]any{
		"kind":              string(issuecomment.KindStatusUpdate),
		"issue_number":      issueNumber,
		"repo":              runRow.Repo,
		"github_comment_id": req.GithubCommentID,
	})
	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  issuecomment.CategoryStatusCommentPosted,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"audit append failed", map[string]any{"error": err.Error()})
		return
	}

	w.WriteHeader(http.StatusCreated)
}

// gatingReviewerProblem reports why a gating reviewers config cannot be
// satisfied by the wired reviewer set (#574 / #955): the bare count form
// with no default adapter, or an agents-list entry naming a provider that
// does not resolve. Empty string means every declared reviewer is
// dispatchable. Used by handleCreateRun's run-create fail-fast; advisory
// stages keep the skip-with-audit degradation path instead.
func (s *Server) gatingReviewerProblem(reviewers *spec.ReviewersConfig) string {
	if len(reviewers.Agents) > 0 {
		if s.cfg.PlanReviewers == nil {
			return "workflow declares agent-gated review with a reviewers.agents list but fishhawkd has no reviewer set wired; set FISHHAWKD_ANTHROPIC_API_KEY, FISHHAWKD_ENABLE_LOCAL_CLAUDE_REVIEWER, or FISHHAWKD_ENABLE_CODEX_REVIEWER, or remove reviewers.agents"
		}
		for _, a := range reviewers.Agents {
			if _, err := s.cfg.PlanReviewers.For(a.Provider, a.Model); err != nil {
				return fmt.Sprintf("workflow declares agent-gated review naming reviewer provider %q but it does not resolve (%s); enable it (%s) or remove the reviewers.agents entry",
					a.Provider, err.Error(), reviewerProviderEnvKnob(a.Provider))
			}
		}
		return ""
	}
	if s.defaultPlanReviewer() == nil {
		return "workflow declares agent-gated plan review (reviewers.agent > 0, human == 0) but fishhawkd has no PlanReviewer wired; set FISHHAWKD_ANTHROPIC_API_KEY or remove reviewers.agent"
	}
	return ""
}

// reviewerProviderEnvKnob names the deployment env knob that enables a
// given reviewer provider, for fail-fast error messages.
func reviewerProviderEnvKnob(provider string) string {
	switch provider {
	case "anthropic":
		return "FISHHAWKD_ANTHROPIC_API_KEY"
	case "claudecode":
		return "FISHHAWKD_ENABLE_LOCAL_CLAUDE_REVIEWER"
	case "codex":
		return "FISHHAWKD_ENABLE_CODEX_REVIEWER"
	default:
		return "FISHHAWKD_ANTHROPIC_API_KEY / FISHHAWKD_ENABLE_LOCAL_CLAUDE_REVIEWER / FISHHAWKD_ENABLE_CODEX_REVIEWER"
	}
}
