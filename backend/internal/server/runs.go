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
	"github.com/kuhlman-labs/fishhawk/backend/internal/securityscan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// runResponse is the JSON shape POST /v0/runs and GET /v0/runs/{id}
// return. Field names + types match docs/api/v0.openapi.yaml's
// `Run` schema exactly so there's never a translation step between
// the OpenAPI doc and the wire format.
type runResponse struct {
	ID             uuid.UUID  `json:"id"`
	Repo           string     `json:"repo"`
	WorkflowID     string     `json:"workflow_id"`
	WorkflowSHA    string     `json:"workflow_sha"`
	TriggerSource  string     `json:"trigger_source"`
	TriggerRef     *string    `json:"trigger_ref"`
	State          string     `json:"state"`
	ParentRunID    *uuid.UUID `json:"parent_run_id,omitempty"`
	DecomposedFrom *uuid.UUID `json:"decomposed_from,omitempty"`
	// UpstreamRunID echoes the run's deploy-gate cross-run reference
	// (E23.11 / #1417): the upstream feature_change run whose ci_green /
	// review_merged a standalone deploy-only release run's required_upstream
	// gate evaluates. Omitted when nil (appended-deploy / non-deploy runs).
	// DISTINCT from parent_run_id (#216).
	UpstreamRunID      *uuid.UUID `json:"upstream_run_id,omitempty"`
	PullRequestURL     *string    `json:"pull_request_url,omitempty"`
	RetryAttempt       int        `json:"retry_attempt"`
	MaxRetriesSnapshot int        `json:"max_retries_snapshot"`
	RunnerKind         string     `json:"runner_kind"`
	// RunnerKindResolved echoes whether RunnerKind has been LOCKED by the
	// run's first signed runner self-report (#1346/#1348). Always emitted
	// (false for legacy / un-resolved rows), matching drive / cost_usd_total
	// / resolved_model. The host-dispatch guardrail (#1355) reads this lock
	// flag to engage only against an already-resolved conflicting kind.
	RunnerKindResolved bool `json:"runner_kind_resolved"`
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
	// FixupModel is the model the run's most-recent fix-up pass ran under
	// (#1164), distilled from the newest stage_fixup_triggered audit entry's
	// pinned fixup_model / fixup_model_source / pass_ordinal. Populated by
	// handleGetRun ONLY (a single audit read, same posture as Concerns) —
	// toRunResponse never sets it, so the list endpoint stays free of the
	// extra read. Omitted when the run has had no fix-up (or the pin predates
	// #1164).
	FixupModel *runFixupModelPayload `json:"fixup_model,omitempty"`
	// SecurityFindings is the run's unresolved high-severity code-scanning
	// (CodeQL/SAST) findings on the implement diff (#1096), distilled from
	// the newest implement_security_findings audit entry (the webhook ingest
	// records ONE idempotent entry per scan, floored on the latest fix-up, so
	// the newest entry reflects the current scan state). Populated by
	// handleGetRun ONLY (a single audit read, same posture as Concerns) — so
	// the list endpoint stays free of the extra read. A SEPARATE signal from
	// Concerns: a finding here is held by its own merge gate and routed to
	// its own fix-up pass. Omitted when the run has no findings (no scan yet,
	// a clean scan, or a clean re-scan after a fix-up cleared them).
	SecurityFindings []securityFindingPayload `json:"security_findings,omitempty"`
}

// securityFindingPayload is one high-severity code-scanning finding on the
// wire (#1096). Kept distinct from securityscan.Finding so the audit payload
// shape (recorded by the webhook ingest) cannot silently leak fields through
// the API surface; the surfaced subset is what a reviewer/operator needs to
// locate and open the alert.
type securityFindingPayload struct {
	Number      int    `json:"number"`
	RuleID      string `json:"rule_id"`
	Description string `json:"description,omitempty"`
	Severity    string `json:"severity"`
	State       string `json:"state,omitempty"`
	Path        string `json:"path"`
	StartLine   int    `json:"start_line,omitempty"`
	HTMLURL     string `json:"html_url,omitempty"`
}

// securityFindingsAuditPayload is the cross-slice shape of the
// implement_security_findings audit entry the webhook ingest records
// (#1096) and the surfaces + merge gate read. The webhook marshals the
// high-severity findings intersecting the diff under "findings"; consumers
// decode only that key and tolerate any additional metadata fields the
// ingest may carry (head SHA, fix-up floor), so an additive payload change
// never breaks the surface.
type securityFindingsAuditPayload struct {
	Findings []securityscan.Finding `json:"findings"`
}

// runFixupModelPayload is the model a fix-up pass ran under on the wire
// (#1164): the source-tagged model plus the 1-based pass ordinal it was
// pinned on. Distinct from the audit payload shape so an audit-trail change
// can't silently leak through the API surface.
type runFixupModelPayload struct {
	Model       string `json:"model"`
	Source      string `json:"source"`
	PassOrdinal int    `json:"pass_ordinal"`
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
	// HasSuggestedPatch reports whether the reviewer attached a mechanical
	// suggested_patch to this concern (#1165). Only the boolean is
	// surfaced — the diff text stays elided like the note, keeping the
	// payload bounded; the fix-up apply path reads the patch server-side.
	HasSuggestedPatch bool `json:"has_suggested_patch"`
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
	// ReviewerRejectClass names the reviewer-reject page-event class the
	// run currently resolves to (#1378): gating_reviewer_reject (a reject
	// pages the human) or advisory_reviewer_reject (arbitrable /
	// auto-routed). Omitted when the implement stage is gateless — no
	// agent-reviewer authority — preserving byte-identical responses for
	// runs that resolve gateless.
	ReviewerRejectClass string `json:"reviewer_reject_class,omitempty"`
	// ModelPolicy is the effective operator_agent block's scenario-A
	// model-selection contract (#1421), passed through as static config.
	// Omitted when the block declares no model_policy, keeping
	// unconfigured responses byte-identical to today.
	ModelPolicy *runDelegationModelPolicyPayload `json:"model_policy,omitempty"`
}

// runDelegationActionPayload is one knob's evaluation on the wire.
// unmet_reason names the exact failed predicate when met is false.
type runDelegationActionPayload struct {
	Action      string `json:"action"`
	Condition   string `json:"condition"`
	Met         bool   `json:"met"`
	UnmetReason string `json:"unmet_reason,omitempty"`
}

// runDelegationModelPolicyPayload mirrors spec.ModelPolicy on the wire
// (#1421): the scenario-A operator-agent model-selection contract,
// surfaced as static config for the operator agent to read and apply via
// #1416's per-stage override channels. Every field omitempty so an
// unconfigured (or partially configured) policy stays minimal.
type runDelegationModelPolicyPayload struct {
	Strategy string                             `json:"strategy,omitempty"`
	Defaults *runDelegationModelDefaultsPayload `json:"defaults,omitempty"`
	Allowed  []string                           `json:"allowed,omitempty"`
}

// runDelegationModelDefaultsPayload mirrors spec.ModelPolicyDefaults on
// the wire: the per-stage model the operator agent applies under the
// explicit_defaults strategy.
type runDelegationModelDefaultsPayload struct {
	Plan      string `json:"plan,omitempty"`
	Implement string `json:"implement,omitempty"`
	Review    string `json:"review,omitempty"`
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
		UpstreamRunID:      r.UpstreamRunID,
		PullRequestURL:     r.PullRequestURL,
		RetryAttempt:       r.RetryAttempt,
		MaxRetriesSnapshot: r.MaxRetriesSnapshot,
		RunnerKind:         r.RunnerKind,
		RunnerKindResolved: r.RunnerKindResolved,
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
	// UpstreamRunID, when set, names the upstream feature_change run whose
	// ci_green / review_merged a standalone deploy-only release run's
	// required_upstream pre-flight gate evaluates (E23.11 / #1417). Optional;
	// omitted (nil) leaves the deploy gate evaluating the CURRENT run (the
	// appended-deploy path). DISTINCT from parent_run_id (#216) — a deploy-
	// gate safety pointer, not a follow-up/lineage link. Persisted on the run
	// row and echoed on the response.
	UpstreamRunID *uuid.UUID `json:"upstream_run_id,omitempty"`
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
	// Model-id validity layer (#1339). Validate every model named in the
	// resolved spec (executor.model + reviewers.agents[].model) against the
	// snapshot oracle BEFORE the idempotency / repository writes, so a
	// definitively-invalid model fails fast with a 422 and inserts no run row.
	// Fail-OPEN: a nil oracle, no snapshot (ok=false), or a stale one
	// (fresh=false) accepts with a warning — so the wired no-data oracle can
	// never hard-fail production today. Warnings are logged only this slice
	// (no HTTP response field; the user-facing surface lands with #1335).
	if parsed != nil {
		warnings, verr := spec.ValidateModels(parsed, s.cfg.ModelOracle)
		for _, wn := range warnings {
			s.cfg.Logger.Warn("workflow model unverifiable",
				"repo", req.Repo,
				"workflow_id", req.WorkflowID,
				"path", wn.Path,
				"code", wn.Code,
				"message", wn.Message)
		}
		if verr != nil {
			s.writeError(w, r, http.StatusUnprocessableEntity, "model_invalid",
				verr.Error(),
				map[string]any{"field": "workflow_spec", "workflow_id": req.WorkflowID})
			return
		}
	}

	// Plan-review capability gate (#574 / ADR-027 / #955 / #1495). The spec is
	// authoritative for WHICH reviewers a gating plan stage (effective agent
	// count > 0, human == 0) runs; the FISHHAWKD_ENABLE_* / FISHHAWKD_ANTHROPIC_API_KEY
	// flags are deployment CAPABILITY gates, not policy switches. Two cases:
	//
	//   - COARSE no-backend gate (gatingReviewerProblem): NO reviewer backend
	//     is wired at all on this deployment. This is a deployment-wide
	//     misconfiguration — zero review infrastructure — so it still HARD-FAILS
	//     run creation with a run_rejected_misconfigured audit + 400,
	//     irrespective of any per-reviewer optional flag. Symmetric with the
	//     webhook dispatcher's coarse !PlanReviewerConfigured hard-fail so both
	//     run-create paths behave identically (#1495 binding condition, opt. b).
	//
	//   - PER-REVIEWER capability gap (unavailableSpecReviewers): a backend IS
	//     wired but a specific spec-declared reviewer's provider is unavailable.
	//     Run creation NO LONGER hard-fails — the spec is valid, only the
	//     deployment capability is missing. Emit a reviewer_capability_unavailable
	//     audit per unavailable reviewer honoring its optional flag (loud for
	//     optional:false, quiet for optional:true) and PROCEED; the reviewer
	//     degrades again at the runtime review loop with a *_review_skipped entry.
	//
	// Advisory mode (human > 0) is allowed through entirely: the human gate
	// remains authoritative and the runtime review loops record the degradation.
	if haveStageDefs {
		for _, st := range workflowDef.Stages {
			if st.Type != spec.StageTypePlan || st.Reviewers == nil {
				continue
			}
			if planreview.ResolveAuthority(*st.Reviewers) != planreview.AuthorityGating {
				continue
			}
			if problem := s.gatingReviewerProblem(st.Reviewers); problem != "" {
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
			// Capability-gate graceful degradation (#1495): a wired backend
			// missing this specific reviewer's provider does not reject the run.
			for _, u := range s.unavailableSpecReviewers(st.Reviewers) {
				s.emitReviewerCapabilityUnavailable(r.Context(), req.Repo, req.WorkflowID, st.ID, st.Reviewers.AgentCount(), u)
			}
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

	// Map the request's issue context (#415) to the domain value, then
	// hand the resolved inputs to CreateRunForTrigger — the single
	// integrating seam for run + stage creation, reused by the
	// campaign-driver ticker (E25.5 / #1444) so the create path lives in
	// one place rather than being duplicated per trigger source.
	var issueCtx *run.IssueContext
	if req.IssueContext != nil {
		issueCtx = &run.IssueContext{
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
			issueCtx.Comments = comments
		}
	}
	var idemp *string
	if idempKey != "" {
		k := idempKey
		idemp = &k
	}

	created, err := s.CreateRunForTrigger(r.Context(), CreateRunForTriggerParams{
		Repo:               req.Repo,
		WorkflowID:         req.WorkflowID,
		WorkflowSHA:        req.WorkflowSHA,
		TriggerSource:      run.TriggerSource(req.TriggerSource),
		TriggerRef:         req.TriggerRef,
		RunnerKind:         req.RunnerKind,
		InstallationID:     installationID,
		IssueContext:       issueCtx,
		Drive:              req.Drive,
		IdempotencyKey:     idemp,
		UpstreamRunID:      req.UpstreamRunID,
		HaveStageDefs:      haveStageDefs,
		WorkflowDef:        workflowDef,
		WorkflowSpec:       specBytes,
		MaxRetriesSnapshot: maxRetriesSnap,
	})
	if err != nil {
		// CreateRunForTrigger wraps the stage-creation failure with the
		// "create stages failed" prefix and the run-row failure with
		// "create run failed"; surface that verbatim in details so the
		// existing diagnostic contract is preserved.
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"create run failed", map[string]any{"error": err.Error()})
		return
	}

	s.writeJSON(w, r, http.StatusCreated, toRunResponse(created))
}

// StartRunForCampaignIssue resolves the workflow context for a campaign's
// issue and starts a run for it through CreateRunForTrigger (E25.5 / #1444).
// A campaign carries only its repo + the issue refs — no workflow context —
// so this resolves the GitHub App installation, fetches + parses the workflow
// spec at workflowRef (empty = the repo's default branch), and uses the
// fetched blob SHA as the run's workflow_sha. The run is created with
// trigger_source=github_issue and trigger_ref=issueRef so the runner's
// prompt builder fetches the issue context.
//
// It also best-effort hydrates the run row's IssueContext (title/body/url/
// number + comments) from GitHub at mint time (#1721). This matters for a
// decomposed parent: its fan-out children inherit the parent's IssueContext to
// build their headers and degraded prompt context, and a nil parent context
// previously left every child headerless. Correctness of child scope narrowing
// now rests on the persisted SliceIndex linkage (see matchDecomposedSubPlan),
// not on this context; hydration is purely additive. A parse or fetch failure
// logs a warn and proceeds with IssueContext=nil so hydration NEVER blocks a
// run start.
//
// Requires a configured GitHub client (the campaign repo carries no inline
// spec). Returns an error rather than starting a run when the installation or
// spec can't be resolved — the driver leaves the item un-started and retries
// next tick.
//
// runnerKind selects the execution backend (ADR-022): empty applies the
// repo-layer default (github_actions) — the GHA auto-driver path — while the
// operator-driven campaign start (E26.2 / #1481) passes "local" so the
// campaign's runs execute through the local dogfood loop. runner_kind affects
// only execution dispatch, not spec resolution, so the GitHub fetch below is
// unaffected by the choice.
func (s *Server) StartRunForCampaignIssue(ctx context.Context, repo, issueRef, workflowID, workflowRef, runnerKind string) (*run.Run, error) {
	if s.cfg.GitHub == nil {
		return nil, errors.New("campaign run start requires a configured GitHub client")
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("campaign repo %q is not in owner/name form", repo)
	}
	repoRef := githubclient.RepoRef{Owner: owner, Name: name}

	instID, err := s.cfg.GitHub.GetRepoInstallation(ctx, repoRef)
	if err != nil {
		return nil, fmt.Errorf("resolve installation for %s: %w", repo, err)
	}

	fc, err := s.cfg.GitHub.GetWorkflowSpec(ctx, instID, repoRef, workflowRef)
	if err != nil {
		return nil, fmt.Errorf("fetch workflow spec for %s: %w", repo, err)
	}
	parsed, err := spec.ParseBytes(fc.Content)
	if err != nil {
		return nil, fmt.Errorf("parse workflow spec for %s: %w", repo, err)
	}
	wf, ok := parsed.Workflows[workflowID]
	if !ok {
		return nil, fmt.Errorf("workflow %q not defined in %s spec", workflowID, repo)
	}
	if len(wf.Stages) == 0 {
		return nil, fmt.Errorf("workflow %q in %s spec has no stages", workflowID, repo)
	}

	triggerRef := issueRef
	// Best-effort IssueContext hydration (#1721): parse the issue number and
	// fetch title/body/url/number + comments so campaign-minted parents carry
	// the same cached context as CLI/webhook runs. A decomposed parent's
	// children inherit this to build their headers + degraded prompt context.
	// Degrades to nil on a parse or fetch failure — hydration never blocks the
	// start; child scope correctness rests on the SliceIndex linkage instead.
	issueCtx := s.hydrateCampaignIssueContext(ctx, instID, repoRef, issueRef)
	return s.CreateRunForTrigger(ctx, CreateRunForTriggerParams{
		Repo:               repo,
		WorkflowID:         workflowID,
		WorkflowSHA:        fc.SHA,
		TriggerSource:      run.TriggerGitHubIssue,
		TriggerRef:         &triggerRef,
		RunnerKind:         runnerKind,
		InstallationID:     &instID,
		IssueContext:       issueCtx,
		HaveStageDefs:      true,
		WorkflowDef:        wf,
		WorkflowSpec:       fc.Content,
		MaxRetriesSnapshot: webhook.WorkflowMaxRetries(wf),
	})
}

// hydrateCampaignIssueContext best-effort fetches a campaign item's issue into
// a *run.IssueContext for caching on the run row (#1721). Returns nil (and
// warn-logs) when the ref can't be parsed as issue:N or the GitHub fetch
// fails, so hydration never blocks a campaign run start. A comment-list fetch
// failure degrades to title+body (the comments slice stays nil) rather than
// discarding the whole context.
func (s *Server) hydrateCampaignIssueContext(ctx context.Context, instID int64, repoRef githubclient.RepoRef, issueRef string) *run.IssueContext {
	number, ok := parseIssueRef(issueRef)
	if !ok {
		s.cfg.Logger.Warn("campaign run start: issue ref not parseable; proceeding without issue context",
			"repo", repoRef.Owner+"/"+repoRef.Name, "issue_ref", issueRef)
		return nil
	}
	issue, err := s.cfg.GitHub.GetIssue(ctx, instID, repoRef, number)
	if err != nil {
		s.cfg.Logger.Warn("campaign run start: hydrate issue context failed; proceeding without",
			"repo", repoRef.Owner+"/"+repoRef.Name, "issue", number, "error", err.Error())
		return nil
	}
	ic := &run.IssueContext{
		Title:  issue.Title,
		Body:   issue.Body,
		URL:    fmt.Sprintf("https://github.com/%s/%s/issues/%d", repoRef.Owner, repoRef.Name, number),
		Number: number,
	}
	comments, err := s.cfg.GitHub.ListIssueComments(ctx, instID, repoRef, number)
	if err != nil {
		s.cfg.Logger.Warn("campaign run start: list issue comments failed; proceeding with title+body",
			"repo", repoRef.Owner+"/"+repoRef.Name, "issue", number, "error", err.Error())
		return ic
	}
	for _, c := range comments {
		ic.Comments = append(ic.Comments, run.IssueComment{
			Author:    c.Author,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		})
	}
	return ic
}

// CreateRunForTriggerParams carries the already-resolved inputs
// CreateRunForTrigger needs to mint a run + its stages. The caller is
// responsible for all validation, installation resolution, and workflow-spec
// parsing — handleCreateRun does this from the HTTP request; the
// campaign-driver adapter (E25.5 / #1444) does it from the campaign's repo +
// issue ref. Splitting validation (caller) from creation (this struct) keeps
// the create path in one place without coupling the driver to HTTP decoding.
type CreateRunForTriggerParams struct {
	Repo          string
	WorkflowID    string
	WorkflowSHA   string
	TriggerSource run.TriggerSource
	TriggerRef    *string
	// RunnerKind is empty for "use the repo-layer default"
	// (RunnerKindGitHubActions); callers pass a validated non-default
	// explicitly (the API handler validates; the campaign adapter passes
	// the operator-selected kind).
	RunnerKind string
	// InstallationID is the best-effort resolved GitHub App installation
	// (#713); nil when no App is attributable.
	InstallationID *int64
	// IssueContext caches the triggering issue's title/body/url/number on
	// the run row (#415); nil for non-issue triggers.
	IssueContext *run.IssueContext
	// Drive is the per-run advancement override (#1023). Nil → use the
	// workflow spec's drive default; non-nil wins.
	Drive *bool
	// IdempotencyKey makes the create idempotent against (Repo, key) when
	// non-nil.
	IdempotencyKey *string
	// UpstreamRunID names the upstream feature_change run whose ci_green /
	// review_merged a standalone deploy-only release run's required_upstream
	// pre-flight gate evaluates (E23.11 / #1417). Nil → the gate evaluates
	// the current run (the appended-deploy path). NOT parent_run_id (#216).
	UpstreamRunID *uuid.UUID

	// HaveStageDefs reports whether WorkflowDef carries resolved stage
	// definitions. When true the run row caches WorkflowSpec +
	// MaxRetriesSnapshot and one Stage row is created per WorkflowDef.Stages.
	HaveStageDefs bool
	// WorkflowDef is the resolved workflow definition; consulted only when
	// HaveStageDefs.
	WorkflowDef spec.Workflow
	// WorkflowSpec is the validated raw spec bytes cached on the run row so
	// the trace handler's policy re-evaluation reads from storage.
	WorkflowSpec []byte
	// MaxRetriesSnapshot is the workflow's on_ci_failure.max_retries cap at
	// create time (#280).
	MaxRetriesSnapshot int
}

// CreateRunForTrigger mints a run and its stages from already-resolved
// inputs, then runs the post-create hooks (deploy-first park, board
// transition). It is the single integrating seam for run creation: the HTTP
// handler (handleCreateRun) and the campaign-driver ticker both route through
// it so stage seeding, the deploy-first Advance, and the run_started board
// edge are never duplicated. Returns the created run; on failure the error is
// prefixed "create run failed" or "create stages failed" so callers can
// surface the existing diagnostic.
func (s *Server) CreateRunForTrigger(ctx context.Context, p CreateRunForTriggerParams) (*run.Run, error) {
	createParams := run.CreateRunParams{
		Repo:          p.Repo,
		WorkflowID:    p.WorkflowID,
		WorkflowSHA:   p.WorkflowSHA,
		TriggerSource: p.TriggerSource,
		TriggerRef:    p.TriggerRef,
		// Empty RunnerKind → repo layer applies the default
		// (RunnerKindGitHubActions).
		RunnerKind: p.RunnerKind,
		// Best-effort App installation (#713). Nil when no App is
		// installed; the runner then falls back to the operator's `gh` token.
		InstallationID: p.InstallationID,
		IssueContext:   p.IssueContext,
		IdempotencyKey: p.IdempotencyKey,
		UpstreamRunID:  p.UpstreamRunID,
	}
	if p.HaveStageDefs {
		// Cache the validated spec bytes on the row so the trace handler's
		// policy re-evaluation reads constraints from storage instead of
		// refetching (mirrors dispatcher.createRun).
		createParams.WorkflowSpec = p.WorkflowSpec
		createParams.MaxRetriesSnapshot = p.MaxRetriesSnapshot
		// Drive default from the workflow spec (#1023); the per-run override
		// below wins when present.
		createParams.Drive = p.WorkflowDef.Drive
	}
	if p.Drive != nil {
		createParams.Drive = *p.Drive
	}

	created, err := s.cfg.RunRepo.CreateRun(ctx, createParams)
	if err != nil {
		return nil, fmt.Errorf("create run failed: %w", err)
	}

	// Create one Stage row per stage definition in the spec.
	// Required-checks-snapshot capture is deliberately skipped for
	// non-webhook creates — the snapshot lives behind GitHub's
	// branch-protection API and these paths don't carry a token to query
	// it; local-runner runs don't need it (no PR merge gate enforcement).
	var createdStages []*run.Stage
	if p.HaveStageDefs {
		createdStages, err = webhook.CreateStagesFromSpec(ctx, s.cfg.RunRepo, created.ID, p.WorkflowDef.Stages)
		if err != nil {
			return nil, fmt.Errorf("create stages failed: %w", err)
		}
	}

	// Deploy-first creation park (E23.13 / #1429). A standalone delegating
	// release run's only stage is a deploy stage, which has no agent or runner —
	// so unlike a plan/implement-first run there is NO operator-driven
	// fishhawk_run_stage entry to trigger orchestrator.Advance, and the deploy
	// stage would otherwise sit at pending forever. When the FIRST stage is a
	// deploy stage, kick Advance at creation: it transitions the run
	// pending → running and parks the deploy stage pending → awaiting_deploy_approval
	// (orchestrator.go's deploy guard), making the pre-execution approval gate
	// reachable. Gated STRICTLY on the first stage being a deploy stage so
	// agent-first runs keep their operator-driven entry. Best-effort: an Advance
	// error WARN-logs and never unwinds the created run.
	if len(createdStages) > 0 && createdStages[0].Type == run.StageTypeDeploy && s.cfg.Orchestrator != nil {
		if _, aerr := s.cfg.Orchestrator.Advance(ctx, created.ID); aerr != nil {
			s.cfg.Logger.Warn("deploy-first creation Advance failed; deploy stage left pending (re-poll will re-reach the gate)",
				"run_id", created.ID.String(), "error", aerr.Error())
		} else {
			s.recordDriveDeployInitialization(ctx, created, createdStages[0])
		}
	}

	// Emit the run_started board transition for the just-created run
	// (#1123) — the local-runner / API / campaign-driver run-creation emit
	// point that parallels the webhook dispatcher's run_started emit. Without
	// it a non-webhook run's card never leaves Backlog, and every later board
	// edge then SKIPS on the never-fight-the-human expected-source gate.
	// Best-effort: the hook no-ops for a non-issue TriggerRef and for
	// unconfigured conventions, and board failures only WARN-log.
	s.boardTransitionForRun(ctx, created, lifecycleRunStarted)

	return created, nil
}

// recordDriveDeployInitialization stamps the drive engine's
// deploy_initialization rule (E23.13 / #1429) after a deploy-first run's
// creation-time orchestrator.Advance parks the deploy stage at its
// pre-execution approval gate. No-ops for non-drive runs, when no engine is
// wired, or when no run repo is wired (best-effort: the park already landed; a
// missing stamp degrades attribution, never the run). Modeled on
// recordDrivePlanApproved / recordDriveFixupRepark. The entry is keyed to the
// parked deploy stage and carries the operator's approve-the-deploy-intent next
// action — the run is now parked awaiting that judgment.
func (s *Server) recordDriveDeployInitialization(ctx context.Context, runRow *run.Run, deployStage *run.Stage) {
	if s.drive == nil || s.cfg.RunRepo == nil || !runRow.Drive {
		return
	}
	out := drive.EvaluateDeployInitialization()
	s.drive.Record(ctx, runRow.ID, &deployStage.ID, drive.Advance{
		Rule:       drive.RuleDeployInitialization,
		From:       "deploy:pending",
		To:         "deploy:awaiting_deploy_approval",
		Event:      "deploy-first run created; orchestrator parked the deploy stage at its pre-execution approval gate",
		Parked:     true,
		NextAction: out.NextAction,
	})
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
	// Fix-up model surface (#1164): single-run read ONLY (same posture as
	// Concerns — no per-row audit query on the list endpoint). Distilled from
	// the run's newest stage_fixup_triggered entry's pinned model; nil (field
	// omitted) when the run has had no fix-up or the pin predates #1164.
	resp.FixupModel = s.fixupModelForRun(r.Context(), runID)
	// Security-findings surface (#1096): single-run read ONLY (same posture
	// as Concerns / FixupModel — no per-row audit query on the list
	// endpoint). Distilled from the run's newest implement_security_findings
	// entry; nil (field omitted) when the run has no findings, the read
	// fails (best-effort — warn and omit, never fail the read), or no audit
	// repo is wired.
	resp.SecurityFindings = s.securityFindingsForRun(r.Context(), runID)
	s.writeJSON(w, r, http.StatusOK, resp)
}

// securityFindingsForRun distills the run's unresolved high-severity
// code-scanning findings (#1096) from the newest implement_security_findings
// audit entry recorded ABOVE the latest stage_fixup_triggered floor — the same
// floor the merge gate (auditcomplete.securityFindingsRule) applies. The floor
// is load-bearing: the webhook writer records no clean marker entry when a
// post-fixup re-scan comes back clean, so the floor is what lets a clean
// re-scan clear the surface (the newest in-window entry then carries no
// findings, or there is none). Returns nil — the security_findings field is
// omitted — when the AuditRepo is unconfigured, either lookup fails
// (warn-logged, best-effort), the run has had no scan, every scan predates the
// latest fix-up, or the newest in-window entry carries no findings. A read
// failure degrades to omitted rather than failing the run read, matching the
// Concerns / drive-surface posture.
func (s *Server) securityFindingsForRun(ctx context.Context, runID uuid.UUID) []securityFindingPayload {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, securityscan.AuditCategorySecurityFindings)
	if err != nil {
		s.cfg.Logger.Warn("list security findings failed; omitting security_findings block",
			"run_id", runID.String(), "error", err.Error())
		return nil
	}
	if len(entries) == 0 {
		return nil
	}
	// Floor on the latest fix-up, exactly as the merge gate
	// (auditcomplete.securityFindingsRule) does: a securityscan entry
	// recorded before the most recent stage_fixup_triggered is stale (the
	// fix-up may have resolved it), so the current scan is the newest entry
	// recorded ABOVE that floor. This matters because the webhook writer
	// (codescanning.go recordSecurityScan) deliberately records NO clean
	// marker entry in the post-fixup-clean path — so "newest overall" would
	// still be the pre-fixup dirty entry even after a clean re-scan cleared
	// the gate, surfacing a resolved finding (#1096). A floor-read failure
	// degrades to an omitted field (best-effort), matching the posture above.
	floorSeq, ferr := s.latestFixupSequenceForRun(ctx, runID)
	if ferr != nil {
		s.cfg.Logger.Warn("list fixup markers failed; omitting security_findings block",
			"run_id", runID.String(), "error", ferr.Error())
		return nil
	}
	newestIdx := -1
	for i := range entries {
		if entries[i].Sequence > floorSeq {
			newestIdx = i
		}
	}
	if newestIdx == -1 {
		// Every recorded scan predates the latest fix-up: the clean re-scan
		// either cleared the findings (no marker written) or has not landed
		// yet. Omit — the gate floors identically.
		return nil
	}
	newest := entries[newestIdx]
	var payload securityFindingsAuditPayload
	if err := json.Unmarshal(newest.Payload, &payload); err != nil {
		// A corrupt/undecodable payload degrades to omitted rather than
		// surfacing a half-decoded list — the gate (slice 3) is the
		// authoritative fail-open signal; the surface stays quiet.
		s.cfg.Logger.Warn("decode security findings payload failed; omitting security_findings block",
			"run_id", runID.String(), "error", err.Error())
		return nil
	}
	if len(payload.Findings) == 0 {
		return nil
	}
	out := make([]securityFindingPayload, 0, len(payload.Findings))
	for _, f := range payload.Findings {
		out = append(out, securityFindingPayload{
			Number:      f.Number,
			RuleID:      f.RuleID,
			Description: f.Description,
			Severity:    f.Severity,
			State:       f.State,
			Path:        f.Path,
			StartLine:   f.StartLine,
			HTMLURL:     f.HTMLURL,
		})
	}
	return out
}

// latestFixupSequenceForRun returns the audit sequence of the most-recent
// stage_fixup_triggered entry for the run, or 0 when none has been recorded
// (no fix-up yet → floor 0, so every securityscan entry is in-window). Run-
// scoped (any stage's fix-up), mirroring auditcomplete.latestFixupSequence so
// the run-status surface and the merge gate floor identically (#1096).
func (s *Server) latestFixupSequenceForRun(ctx context.Context, runID uuid.UUID) (int64, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageFixupTriggered)
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, e := range entries {
		if e.Sequence > latest {
			latest = e.Sequence
		}
	}
	return latest, nil
}

// fixupModelForRun distills the run's most-recent fix-up model pin (#1164)
// from the newest stage_fixup_triggered audit entry. Returns nil — the
// fixup_model field is omitted — when the AuditRepo is unconfigured, the
// lookup fails, the run has had no fix-up, the payload is undecodable, or the
// entry predates #1164 (no fixup_model key written). Distinguishes a
// present-but-empty pin (surfaced verbatim) from an absent key (nil) by key
// presence, matching fixupResolvedModelFromAudit.
func (s *Server) fixupModelForRun(ctx context.Context, runID uuid.UUID) *runFixupModelPayload {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageFixupTriggered)
	if err != nil || len(entries) == 0 {
		return nil
	}
	// Newest wins: ListForRunByCategory returns sequence-ascending order, so
	// the last entry is the most-recent fix-up pass across the run.
	newest := entries[len(entries)-1]
	var probe struct {
		// Pointer so a present-but-empty pin is distinguishable from a
		// pre-#1164 entry that carried no fixup_model key.
		FixupModel       *string `json:"fixup_model"`
		FixupModelSource string  `json:"fixup_model_source"`
		PassOrdinal      int     `json:"pass_ordinal"`
	}
	if err := json.Unmarshal(newest.Payload, &probe); err != nil {
		return nil
	}
	if probe.FixupModel == nil {
		return nil
	}
	return &runFixupModelPayload{
		Model:       *probe.FixupModel,
		Source:      probe.FixupModelSource,
		PassOrdinal: probe.PassOrdinal,
	}
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
	// No campaign context on the single-run read: pass a nil campaign
	// override so resolution falls through to the workflow contract.
	res, err := ev.Evaluate(ctx, runRow, &wf, nil)
	if err != nil {
		s.cfg.Logger.Warn("delegation: evaluate failed; omitting delegation block",
			"run_id", runRow.ID.String(), "error", err.Error())
		return nil
	}
	if res == nil {
		return nil
	}
	out := &runDelegationPayload{
		Actions:             make([]runDelegationActionPayload, 0, len(res.Actions)),
		MustPageHuman:       res.MustPageHuman,
		ReviewerRejectClass: res.ReviewerRejectClass,
	}
	if res.ModelPolicy != nil {
		mp := &runDelegationModelPolicyPayload{
			Strategy: string(res.ModelPolicy.Strategy),
			Allowed:  res.ModelPolicy.Allowed,
		}
		if res.ModelPolicy.Defaults != nil {
			mp.Defaults = &runDelegationModelDefaultsPayload{
				Plan:      res.ModelPolicy.Defaults.Plan,
				Implement: res.ModelPolicy.Defaults.Implement,
				Review:    res.ModelPolicy.Defaults.Review,
			}
		}
		out.ModelPolicy = mp
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
	// Acceptance-gate presentation statuses (E31.17 / #1568): when the latest
	// stamp is an acceptance-gate rule and the PR is open, the derived status
	// is the rule name itself — acceptance_pending / acceptance_settled_outcome_unknown
	// / acceptance_triage. next_action already carried through generically
	// above (await_acceptance / read_acceptance_audit / read_acceptance_triage).
	// Keying off the LATEST entry means a later checks_green_awaiting_merge
	// stamp (acceptance passed) supersedes these to awaiting_merge.
	if runRow.PullRequestURL != nil {
		switch latest.Rule {
		case drive.RuleAcceptancePending,
			drive.RuleAcceptanceOutcomeUnknown,
			drive.RuleAcceptanceTriage:
			resp.DerivedStatus = string(latest.Rule)
		}
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
			ID:                c.ID,
			StageKind:         c.StageKind,
			Severity:          c.Severity,
			Category:          c.Category,
			State:             string(c.State),
			HasSuggestedPatch: c.SuggestedPatch != "",
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

// gatingReviewerProblem reports the COARSE deployment-capability problem
// that still HARD-FAILS run creation: NO reviewer backend is wired at all on
// this deployment (defaultPlanReviewer() == nil) while a gating plan stage
// (agent > 0, human == 0) needs one. This is a deployment-wide
// misconfiguration — there is zero review infrastructure — distinct from the
// finer-grained #1495 case of a SPECIFIC spec-declared reviewer's provider
// being unavailable while OTHER backends are wired (collected by
// unavailableSpecReviewers and degraded at the runtime review loop, not
// rejected). It is symmetric with the webhook dispatcher's coarse
// !PlanReviewerConfigured hard-fail (dispatcher.go), so both run-create paths
// hard-fail the no-backend case identically and irrespective of the
// per-reviewer optional flag. Empty string means a backend IS wired — any
// residual per-reviewer capability gap degrades rather than rejecting.
func (s *Server) gatingReviewerProblem(reviewers *spec.ReviewersConfig) string {
	if s.defaultPlanReviewer() != nil {
		return ""
	}
	if len(reviewers.Agents) > 0 {
		return "workflow declares agent-gated review (reviewers.agents, human == 0) but fishhawkd has no reviewer backend wired at all; set FISHHAWKD_ANTHROPIC_API_KEY, FISHHAWKD_ENABLE_LOCAL_CLAUDE_REVIEWER, or FISHHAWKD_ENABLE_CODEX_REVIEWER, or remove the reviewers.agents gate"
	}
	return "workflow declares agent-gated plan review (reviewers.agent > 0, human == 0) but fishhawkd has no PlanReviewer wired; set FISHHAWKD_ANTHROPIC_API_KEY or remove reviewers.agent"
}

// unavailableReviewer is one spec-declared reviewer whose provider cannot be
// resolved on this deployment (#1495) — a per-reviewer capability gap that
// degrades run creation rather than rejecting it.
type unavailableReviewer struct {
	provider string
	optional bool
	err      error
}

// unavailableSpecReviewers returns the agents-list reviewers whose provider
// cannot be resolved on this deployment (#1495), for the capability-gate
// graceful-degradation path. It is only meaningful once gatingReviewerProblem
// has returned "" (a reviewer backend IS wired): each entry is a per-reviewer
// capability gap that proceeds with a reviewer_capability_unavailable audit
// honoring its optional flag. The bare count form names no provider and
// resolves via the default adapter, so it yields no per-reviewer gaps here —
// its only failure mode is the coarse no-backend gate above.
func (s *Server) unavailableSpecReviewers(reviewers *spec.ReviewersConfig) []unavailableReviewer {
	if s.cfg.PlanReviewers == nil || len(reviewers.Agents) == 0 {
		return nil
	}
	var out []unavailableReviewer
	for _, a := range reviewers.Agents {
		if _, err := s.cfg.PlanReviewers.For(a.Provider, a.Model, a.ReasoningEffort); err != nil {
			out = append(out, unavailableReviewer{provider: a.Provider, optional: a.Optional, err: err})
		}
	}
	return out
}

// emitReviewerCapabilityUnavailable appends a global-chain
// reviewer_capability_unavailable audit entry at run-create time when a
// spec-declared reviewer's provider is unavailable on this deployment
// (#1495). No run row exists yet at the create gate, so it uses
// AppendGlobalChained (mirroring the run_rejected_misconfigured emission).
// The spec is authoritative for WHICH reviewers run and the FISHHAWKD_ENABLE_*
// flags are deployment capability gates, so this gap DEGRADES the run rather
// than rejecting it: the entry records the gap and the caller proceeds (the
// reviewer degrades again at the runtime review loop). The optional flag picks
// the log surface honoring the spec's policy — ERROR for optional:false (the
// deployment SHOULD have run it), INFO for optional:true (a quiet graceful
// skip). Best-effort: a nil AuditRepo is a no-op, an append error WARN-logs.
func (s *Server) emitReviewerCapabilityUnavailable(ctx context.Context, repo, workflowID, stage string, configuredAgents int, u unavailableReviewer) {
	knob := reviewerProviderEnvKnob(u.provider)
	if u.optional {
		s.cfg.Logger.Info("run-create: spec-declared optional reviewer unavailable on this deployment — proceeding (graceful skip)",
			"repo", repo, "workflow_id", workflowID, "stage", stage,
			"provider", u.provider, "enable_knob", knob, "error", u.err.Error())
	} else {
		s.cfg.Logger.Error("run-create: spec-declared reviewer unavailable on this deployment (optional:false) — proceeding; enable its capability to run it",
			"repo", repo, "workflow_id", workflowID, "stage", stage,
			"provider", u.provider, "enable_knob", knob, "error", u.err.Error())
	}
	if s.cfg.AuditRepo == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"reason":            planreview.ReasonReviewerUnavailable,
		"provider":          u.provider,
		"optional":          u.optional,
		"stage":             stage,
		"workflow_id":       workflowID,
		"repo":              repo,
		"configured_agents": configuredAgents,
	})
	systemKind := audit.ActorKind("system")
	if _, aerr := s.cfg.AuditRepo.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(),
		Category:  "reviewer_capability_unavailable",
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		s.cfg.Logger.Warn("append reviewer_capability_unavailable audit entry failed",
			"repo", repo, "workflow_id", workflowID, "error", aerr.Error())
	}
}

// reviewerProviderEnvKnob names the deployment env knob whose capability gate
// enables a given reviewer provider, for the #1495 capability-framed audit and
// log messaging.
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
