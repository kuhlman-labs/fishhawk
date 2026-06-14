package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
)

// promptResponse is the 200 body for GET /v0/stages/{stage_id}/prompt.
// Wrapped in a JSON object so future fields (template version,
// hash, redaction notes) can be added without breaking the runner.
type promptResponse struct {
	StageID             string `json:"stage_id"`
	StageType           string `json:"stage_type"`
	Prompt              string `json:"prompt"`
	PromptHash          string `json:"prompt_hash"`
	AgentTimeoutSeconds int    `json:"agent_timeout_seconds"`
	// DecomposedFromRunID is the parent run's ID when this run is a
	// decomposed child. Absent for standalone runs. Runners use this to
	// route decomposed children onto a shared parent branch.
	DecomposedFromRunID  string `json:"decomposed_from_run_id,omitempty"`
	VerifyCommand        string `json:"verify_command,omitempty"`
	VerifyTimeoutSeconds int    `json:"verify_timeout_seconds,omitempty"`
	// VerifyMaxIterations is the verify-fix loop budget from
	// executor.verify.max_iterations. 0 (or absent) preserves the
	// single-shot demote-on-failure gate; >0 enables the bounded fix
	// loop. Wired through but not yet consumed by the runner.
	VerifyMaxIterations int `json:"verify_max_iterations,omitempty"`
	// MinRunnerVersion is the minimum runner version the backend requires.
	// Runners that are older than this should exit with a version-skew error
	// rather than proceeding to invoke the agent.
	MinRunnerVersion string `json:"min_runner_version,omitempty"`
	// AgentSelfRetry is true when the workflow spec opts the stage into
	// ADR-023 runner-side self-retry on category-A/C failures.
	AgentSelfRetry bool `json:"agent_self_retry,omitempty"`
	// MaxRetriesSnapshot is the run's max_retries_snapshot at prompt-fetch
	// time. Together with RetryAttempt it lets the runner compute the
	// remaining self-retry budget without a separate API call.
	MaxRetriesSnapshot int `json:"max_retries_snapshot,omitempty"`
	// RetryAttempt is the run's current retry_attempt counter. 0 for
	// original runs; incremented by the backend on each auto-retry.
	RetryAttempt int `json:"retry_attempt,omitempty"`
	// ScopeFiles is the approved plan's scope.files list, echoed on
	// implement stages so the runner can bound the commit to exactly
	// those declared paths instead of `git add -A` (#581). Empty/omitted
	// when no approved plan is available (plan_missing_for_implement) —
	// the runner falls back to staging every change.
	ScopeFiles []scopeFile `json:"scope_files,omitempty"`
	// CommitAuthorName / CommitAuthorEmail are the GitHub App bot account's
	// git commit identity, resolved from the App (slug + bot user-id) and
	// echoed so the runner attributes App-backed commits to the App's bot
	// account instead of its hardcoded fallback (#722). Empty/omitted when
	// the identity can't be resolved (no App JWT, dev/CLI) — the runner then
	// keeps gitops.DefaultAuthorName/DefaultAuthorEmail.
	CommitAuthorName  string `json:"commit_author_name,omitempty"`
	CommitAuthorEmail string `json:"commit_author_email,omitempty"`
	// Fixup is true when this implement stage is an operator-triggered
	// implement-review fix-up pass (#762/#784): an unconsumed
	// stage_fixup_triggered audit entry routed review concerns back to this
	// stage. The runner takes its RebaseFromRemote same-branch path (fetch +
	// checkout + pull --rebase on FixupBranch) and updates the open PR rather
	// than opening a new one. Both false/empty on a normal implement dispatch,
	// so the runner's branch routing falls through to the per-stage branch.
	// Keys (`fixup`/`fixup_branch`) are byte-identical to what the runner's
	// fetchPromptToFile reads (runner/internal/upload/upload.go).
	Fixup bool `json:"fixup,omitempty"`
	// FixupBranch is the existing PR branch a fix-up pass commits onto.
	// Non-empty only when Fixup is true. Derived to EXACTLY match the runner's
	// branch formula: non-decomposed
	// `fishhawk/run-<shortID(runID)>/stage-<shortID(stageID)>`, decomposed
	// `fishhawk/run-<shortID(decomposedFromRunID)>`. A divergence would
	// re-create the `checkout -b <existing branch>` already-exists failure.
	FixupBranch string `json:"fixup_branch,omitempty"`
	// FixupExpectedHeadSHA is the run's recorded head — the newest head_sha
	// across the run's reported-head ledger (pull_request_opened /
	// child_pushed / fixup_pushed audit entries, the same source the ADR-035
	// branch-lineage verifier treats as the run's legitimate tips). Set only
	// when Fixup is true so the runner can verify the fetched PR-branch tip
	// IS the stage's recorded head before invoking the agent (#967). Empty
	// when resolution fails (WARN-and-proceed) — the runner then skips the
	// SHA comparison rather than blocking the pass.
	FixupExpectedHeadSHA string `json:"fixup_expected_head_sha,omitempty"`
}

// shortID returns the first 8 characters of a UUID's string form, mirroring
// the runner's shortID (runner/cmd/fishhawk-runner/main.go) and
// auditcomplete.shortID. Used to derive the fix-up branch name so the
// backend-emitted FixupBranch matches the runner's branch-routing formula
// byte-for-byte.
func shortID(id uuid.UUID) string {
	return id.String()[:8]
}

// fixupBranchFor derives the existing PR branch a fix-up pass commits onto,
// matching the runner's branch-routing logic. For a decomposed child the
// branch is the shared parent branch `fishhawk/run-<shortID(parentRunID)>`;
// otherwise it is the per-stage branch
// `fishhawk/run-<shortID(runID)>/stage-<shortID(stageID)>`.
func fixupBranchFor(runRow *run.Run, stage *run.Stage) string {
	if runRow.DecomposedFrom != nil {
		return "fishhawk/run-" + shortID(*runRow.DecomposedFrom)
	}
	return fmt.Sprintf("fishhawk/run-%s/stage-%s", shortID(runRow.ID), shortID(stage.ID))
}

// fixupBranchForRun resolves the fix-up branch for a run, extending
// fixupBranchFor to handle the decomposed-PARENT case (#1063). A decomposed
// parent has DecomposedFrom == nil, so fixupBranchFor would return a per-stage
// branch — but a parent fix-up's commit must land on the consolidated PR head,
// the shared branch `fishhawk/run-<shortID(runID)>` (byte-matching
// orchestrator.consolidatedBranch). When the run is a parent (DecomposedFrom ==
// nil) WITH minted children, return that shared branch; otherwise delegate to
// fixupBranchFor. On a probe error, fall back to fixupBranchFor — never widen
// an ordinary run onto a shared branch.
func (s *Server) fixupBranchForRun(ctx context.Context, runRow *run.Run, stage *run.Stage) string {
	if runRow.DecomposedFrom == nil && s.hasDecomposedChildren(ctx, runRow.ID) {
		return "fishhawk/run-" + shortID(runRow.ID)
	}
	return fixupBranchFor(runRow, stage)
}

// hasDecomposedChildren reports whether the run has minted decomposition
// children (a bounded Limit:1 probe). A nil RunRepo or a probe error returns
// false so the caller falls back to the conservative per-stage branch.
func (s *Server) hasDecomposedChildren(ctx context.Context, runID uuid.UUID) bool {
	if s.cfg.RunRepo == nil {
		return false
	}
	children, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		DecomposedFrom: &runID,
		Limit:          1,
	})
	if err != nil {
		return false
	}
	return len(children) > 0
}

// scopeFile is one entry in promptResponse.ScopeFiles: the path the
// agent declared it would touch plus the per-file operation
// (create/modify/delete). Mirrors plan.ScopeFile but pins the wire
// shape to the prompt-response contract rather than re-exporting the
// plan type.
type scopeFile struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
}

// scopeFilesFromPlan converts an approved plan's scope.files into the
// prompt-response wire shape. Returns nil when the plan is nil or
// declares no files, so the field is omitted and the runner falls
// back to `git add -A`.
func scopeFilesFromPlan(p *plan.Plan) []scopeFile {
	if p == nil {
		return nil
	}
	return scopeFilesFromScope(&p.Scope)
}

// scopeFilesFromScope converts a plan.Scope's files into the
// prompt-response wire shape. Returns nil when the scope is nil or
// declares no files. Shared by scopeFilesFromPlan (top-level scope)
// and resolveDecomposedScopeFiles (per-sub-plan scope) so both produce
// identical wire shapes.
func scopeFilesFromScope(sc *plan.Scope) []scopeFile {
	if sc == nil || len(sc.Files) == 0 {
		return nil
	}
	out := make([]scopeFile, 0, len(sc.Files))
	for _, f := range sc.Files {
		out = append(out, scopeFile{Path: f.Path, Operation: string(f.Operation)})
	}
	return out
}

// conditionPathPattern matches repo-relative path-like tokens in free-text
// approval conditions: a run of path characters that contains at least one
// '/' and ends in a '.<ext>' suffix. Requiring BOTH a slash and an extension
// keeps prose tokens like "and/or" (no extension) and bare words like
// "README" (no slash) out of the extraction.
var conditionPathPattern = regexp.MustCompile(`[A-Za-z0-9_./-]+\.[A-Za-z0-9]+`)

// extractScopePathsFromConditions pulls repo-relative path tokens out of the
// free-text approve-with-conditions comment. Surrounding backticks, quotes,
// parentheses, and trailing punctuation are trimmed off each match. Returns
// de-duplicated paths in first-seen order; nil for empty input.
func extractScopePathsFromConditions(text string) []string {
	if text == "" {
		return nil
	}
	matches := conditionPathPattern.FindAllString(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		p := strings.Trim(m, "`'\"()[]{}<>,.;:!?")
		if p == "" {
			continue
		}
		// A token must still contain a slash and an extension after trimming
		// (trailing-dot stripping can erase the extension, e.g. "foo.").
		if !strings.Contains(p, "/") || !strings.Contains(p, ".") {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mergeConditionScopeFiles folds files named by an approval condition into the
// implement stage's effective scope set, so a condition-authorized edit ships
// as a declared path rather than surfacing as benign undeclared scope_drift
// (#730).
//
// It augments ONLY when a scope.files contract already exists (scopeFiles
// non-empty) AND conditions is non-nil. An empty scope must stay empty: the
// runner falls back to `git add -A` when no scope is declared, and folding
// condition files into an otherwise-empty set would silently narrow the run to
// just those files, dropping every other legitimately-changed file as drift.
//
// Paths already present (compared by .Path) are not duplicated. Operation is
// cosmetic for drift purposes — StageScoped keys off path only.
func (s *Server) mergeConditionScopeFiles(ctx context.Context, scopeFiles []scopeFile, conditions *string) []scopeFile {
	if len(scopeFiles) == 0 || conditions == nil {
		return scopeFiles
	}
	paths := extractScopePathsFromConditions(*conditions)
	return s.foldScopePaths(ctx, scopeFiles, paths, "approval-condition")
}

// mergeStructuredScopeFiles folds the authoritative add_scope_files paths a
// reviewer named at approval time (#824) into the implement stage's effective
// scope set. Unlike mergeConditionScopeFiles it takes the structured slice
// directly (no regex scrape), so it stages directories (trailing slash),
// extensionless/repo-root files, and described-not-spelled paths the prose
// fold cannot reach. It shares the same empty-scope guard: an otherwise-empty
// scope stays empty so the runner's `git add -A` fallback isn't narrowed.
func (s *Server) mergeStructuredScopeFiles(ctx context.Context, scopeFiles []scopeFile, paths []string) []scopeFile {
	if len(scopeFiles) == 0 || len(paths) == 0 {
		return scopeFiles
	}
	return s.foldScopePaths(ctx, scopeFiles, paths, "approval-add-scope-files")
}

// mergeFixupAllowCreate folds the net-new file paths an operator declared on a
// fix-up (#823) into the implement stage's effective scope set, so the runner
// stages them and the #818 created-out-of-scope gate no longer trips for them.
// Shares the same empty-scope guard as the other folders: an otherwise-empty
// scope stays empty so the runner's `git add -A` fallback isn't narrowed.
func (s *Server) mergeFixupAllowCreate(ctx context.Context, scopeFiles []scopeFile, paths []string) []scopeFile {
	if len(scopeFiles) == 0 || len(paths) == 0 {
		return scopeFiles
	}
	return s.foldScopePaths(ctx, scopeFiles, paths, "fixup-allow-create")
}

// mergeApprovedScopeAmendments folds the operator-approved mid-stage scope
// amendment paths (E22.X / #961) into the implement stage's effective scope
// set, so a stage restart or fix-up prompt fetch carries the amended scope —
// the prompt-side half of the activation path; the runner's pre-commit
// refresh is the other. Shares the same empty-scope guard as the other
// folders: an otherwise-empty scope stays empty so the runner's `git add -A`
// fallback isn't narrowed.
func (s *Server) mergeApprovedScopeAmendments(ctx context.Context, scopeFiles []scopeFile, runID, stageID uuid.UUID) []scopeFile {
	if len(scopeFiles) == 0 || s.cfg.ScopeAmendmentRepo == nil {
		return scopeFiles
	}
	paths := s.resolveApprovedScopeAmendments(ctx, runID, stageID)
	return s.foldScopePaths(ctx, scopeFiles, paths, "scope-amendment")
}

// resolveApprovedScopeAmendments returns the paths of every APPROVED scope
// amendment filed by the given stage. Pending/denied rows are excluded — a
// pending amendment confers nothing until the operator decides. Best-effort:
// a repository error logs and returns nil (the original scope remains
// authoritative).
func (s *Server) resolveApprovedScopeAmendments(ctx context.Context, runID, stageID uuid.UUID) []string {
	items, err := s.cfg.ScopeAmendmentRepo.ListByRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"prompt: list scope amendments failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return nil
	}
	var out []string
	for _, a := range items {
		if a.StageID != stageID || a.Status != scopeamendment.StatusApproved {
			continue
		}
		for _, p := range a.Paths {
			out = append(out, p.Path)
		}
	}
	return out
}

// coupledTestSiblings derives the stem-sibling test file for each owned
// source file in a scope slice: for every entry whose normalized path ends in
// `.go` but NOT `_test.go` and whose operation is create or modify (a deleted
// or renamed source file's test is handled separately), it returns the
// `<dir>/<stem>_test.go` sibling in the same directory. Returned paths are
// de-duped in first-seen order. The derivation is purely syntactic — no
// directory listing is consulted, so the sibling is folded whether it exists
// on the base ref (the agent will modify it) or not (the agent will create
// it). Mirrors evaluateTestSweep's testSweepRuleStemSibling rule
// (test_sweep.go) so the auto-fold and the advisory sweep agree on what a
// "coupled test" is.
func coupledTestSiblings(files []scopeFile) []string {
	seen := make(map[string]struct{}, len(files))
	var out []string
	for _, f := range files {
		switch f.Operation {
		case "create", "modify":
		default:
			continue
		}
		p := filepath.ToSlash(f.Path)
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			continue
		}
		base := path.Base(p)
		sibling := path.Join(path.Dir(p), strings.TrimSuffix(base, ".go")+"_test.go")
		if _, ok := seen[sibling]; ok {
			continue
		}
		seen[sibling] = struct{}{}
		out = append(out, sibling)
	}
	return out
}

// foldScopePaths appends paths not already present (compared by .Path) to the
// scope set with Operation=modify, dedups, and info-logs the additions. It is
// the shared body behind mergeConditionScopeFiles (#730 prose fold) and
// mergeStructuredScopeFiles (#824 structured fold). Callers own the
// empty-scope and empty-paths guards.
func (s *Server) foldScopePaths(ctx context.Context, scopeFiles []scopeFile, paths []string, source string) []scopeFile {
	if len(paths) == 0 {
		return scopeFiles
	}
	existing := make(map[string]struct{}, len(scopeFiles))
	for _, f := range scopeFiles {
		existing[f.Path] = struct{}{}
	}
	added := make([]string, 0, len(paths))
	for _, p := range paths {
		if _, ok := existing[p]; ok {
			continue
		}
		existing[p] = struct{}{}
		scopeFiles = append(scopeFiles, scopeFile{Path: p, Operation: "modify"})
		added = append(added, p)
	}
	if len(added) > 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"prompt: folded files into implement scope",
			slog.String("source", source),
			slog.Int("count", len(added)),
			slog.String("paths", strings.Join(added, ",")),
		)
	}
	return scopeFiles
}

// issueGetter is the slice of githubclient.Client the prompt
// handler consumes. Defining the interface in the server package
// lets tests substitute a stub without spinning up an httptest
// fake of api.github.com — *githubclient.Client satisfies it
// in production.
type issueGetter interface {
	GetIssue(ctx context.Context, installationID int64, repo githubclient.RepoRef, number int) (*githubclient.Issue, error)
	ListIssueComments(ctx context.Context, installationID int64, repo githubclient.RepoRef, number int) ([]githubclient.FetchedIssueComment, error)
}

// handleGetStagePrompt implements GET /v0/stages/{stage_id}/prompt.
//
// Auth is the same per-run signing-key signature used by the trace
// upload endpoint: the canonical message is sha256("prompt:" +
// stage_id), signed by the runner with the private half issued at
// signing-key time. Bound-to-stage scope keeps a leaked signature
// from being replayed against a different stage's prompt.
//
// Construction is server-side and pull-style (E3.12 design): the
// runner sees the constructed prompt rather than building it
// itself, so two replays of the same stage produce byte-identical
// prompts. Auditability of "what the agent was asked to do" is
// the load-bearing reason for that choice.
func (s *Server) handleGetStagePrompt(w http.ResponseWriter, r *http.Request) {
	github := s.issueGetter()
	if s.cfg.SigningRepo == nil || s.cfg.RunRepo == nil || github == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "prompt_unconfigured",
			"prompt construction requires signing, run, and GitHub repos to be configured", nil)
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

	switch stage.State {
	case run.StageStateAwaitingApproval, run.StageStateAwaitingChildren,
		run.StageStateSucceeded, run.StageStateFailed, run.StageStateCancelled:
		s.writeError(w, r, http.StatusConflict, "stage_not_runnable",
			"stage is not in a runnable state",
			map[string]any{"current_state": string(stage.State), "stage_id": stageID.String()})
		return
	}

	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), stage.RunID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run for stage failed", map[string]any{"error": err.Error()})
		return
	}

	if !s.verifyPromptSignature(w, r, runRow.ID, stageID) {
		return
	}

	// Build the trigger context. For issue-style triggers we fetch
	// the issue from GitHub at request time so the prompt reflects
	// the latest title/body — the cost is one API call per stage
	// dispatch, which is acceptable. If the issue can't be fetched
	// (e.g., deleted, App lacks access) we still return a prompt
	// rather than failing — the agent will work without it, just
	// with less context.
	trigger := prompt.Trigger{
		Source: string(runRow.TriggerSource),
		Repo:   runRow.Repo,
	}
	if runRow.TriggerRef != nil {
		if number, ok := parseIssueRef(*runRow.TriggerRef); ok {
			trigger.IssueNumber = number
			s.fillIssueContext(r.Context(), github, runRow, number, &trigger)
		}
	}
	// Plan-as-contract (#223): for implement stages, the approved
	// plan is the binding instruction. Look up the run's
	// plan-stage's most-recent standard_v1 artifact and feed it
	// into the prompt builder. Missing plan → fall back to the
	// issue-only template and emit `plan_missing_for_implement` so
	// the audit log captures the gap.
	var scopeFiles []scopeFile
	var fixup bool
	var fixupBranch string
	var fixupExpectedHeadSHA string
	if stage.Type == run.StageTypeImplement {
		approvedPlan, err := s.loadApprovedPlanForRun(r.Context(), runRow.ID)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"load approved plan failed", map[string]any{"error": err.Error()})
			return
		}
		if approvedPlan == nil {
			s.emitPlanMissingForImplement(r.Context(), runRow.ID, stage.ID)
		}
		trigger.ApprovedPlan = approvedPlan
		if approvedPlan != nil {
			budgetSecs := s.resolveAgentTimeout(r.Context(), runRow, run.StageTypeImplement)
			trigger.PredictionContext = &prompt.PredictionContext{
				PredictedMinutes:    approvedPlan.PredictedRuntimeMinutes,
				PredictedConfidence: string(approvedPlan.PredictedRuntimeConfidence),
				StageBudgetMinutes:  budgetSecs / 60,
			}
		}
		scopeFiles = scopeFilesFromPlan(approvedPlan)
		// Decomposition fan-out (#676): a child run narrows its scope to
		// the matched sub-plan's own scope.files when present, so the
		// runner's scope_handoff + scope-drift bound to this slice rather
		// than the parent's full scope. Falls back to the parent scope
		// when the sub-plan omits scope or the run isn't a decomposed child.
		if childScope := s.resolveDecomposedScopeFiles(r.Context(), runRow, approvedPlan); len(childScope) > 0 {
			scopeFiles = childScope
		}
		trigger.ScopeConstraint = s.resolveDecomposedScopeConstraint(r.Context(), runRow, approvedPlan)
		trigger.ApprovalConditions = s.resolveApprovalConditions(r.Context(), runRow)
		// Fold the authoritative add_scope_files paths a reviewer named at
		// approval time into the effective scope set (#824). This is the
		// structured, lossless replacement for the #730 prose scrape: it
		// stages directories, extensionless/repo-root files, and
		// described-not-spelled paths the regex misses. Applied first
		// (authoritative), then the #730 prose fold runs as a fallback —
		// both dedup by path and both no-op on an empty scope (keeps the
		// runner's git add -A fallback).
		scopeFiles = s.mergeStructuredScopeFiles(r.Context(), scopeFiles, s.resolveApprovalAddScopeFiles(r.Context(), runRow))
		// Fold files named by the approval conditions into the effective
		// scope set so a condition-authorized edit ships as a declared
		// path rather than benign scope_drift (#730). No-op when scope is
		// empty (keeps the runner's git add -A fallback).
		scopeFiles = s.mergeConditionScopeFiles(r.Context(), scopeFiles, trigger.ApprovalConditions)
		// Fold the operator-approved mid-stage scope amendment paths (#961)
		// into the effective scope so a stage restart or fix-up prompt
		// carries the amended scope. No-op on an empty scope (keeps the
		// runner's git add -A fallback).
		scopeFiles = s.mergeApprovedScopeAmendments(r.Context(), scopeFiles, runRow.ID, stage.ID)
		// Fix-up pass (#762): when the operator routed implement-review
		// concerns back to this stage, deliver them as binding instructions
		// (reusing #558's framing) and fold any file the concern names into
		// the effective scope so a concern-authorized edit ships as a
		// declared path rather than benign scope_drift. No-op for a normal
		// (non-fix-up) implement dispatch.
		if rendered, joined := s.resolveFixupConcerns(r.Context(), runRow.ID, stage.ID); len(rendered) > 0 {
			trigger.FixupConcerns = rendered
			scopeFiles = s.mergeConditionScopeFiles(r.Context(), scopeFiles, &joined)
			// Fold the operator-declared net-new files (#823) into the
			// effective scope so the runner's #818 created-out-of-scope gate
			// stages them rather than failing category-B. No-op on an empty
			// scope (keeps the runner's git add -A fallback). Any created file
			// NOT declared here still trips the gate.
			scopeFiles = s.mergeFixupAllowCreate(r.Context(), scopeFiles, s.resolveFixupAllowCreate(r.Context(), runRow.ID, stage.ID))
			// Emit the fix-up routing flag (#784): point the runner at the
			// stage's existing PR branch so it takes the RebaseFromRemote
			// same-branch path instead of `checkout -b <existing branch>`.
			fixup = true
			fixupBranch = s.fixupBranchForRun(r.Context(), runRow, stage)
			// Advertise the run's recorded head so the runner can verify the
			// fetched PR-branch tip before invoking the agent (#967).
			fixupExpectedHeadSHA = s.resolveFixupExpectedHeadSHA(r.Context(), runRow.ID, stage.ID)
		}
	}

	// Decompose-required hint: when the run's last plan approval was
	// rejected with --decompose, tell the agent it must populate
	// decomposition.sub_plans in the next plan attempt.
	if stage.Type == run.StageTypePlan {
		if s.loadLastDecomposeRejectionReason(r.Context(), runRow.ID) {
			trigger.DecomposeRequired = true
		}
		if hint, err := s.resolveCalibrationHint(r.Context(), runRow.WorkflowID); err != nil {
			slog.WarnContext(r.Context(), "calibration hint resolution failed", "error", err)
		} else {
			trigger.CalibrationHint = hint
		}
		if runRow.TriggerRef != nil {
			trigger.PriorRejectionFeedback = s.loadPriorRejectionFeedback(r.Context(), runRow.Repo, *runRow.TriggerRef, runRow.ID)
		}
		trigger.PriorSchemaValidationError = s.loadPriorSchemaValidationError(r.Context(), runRow.ID)
	}

	trigger.PlanStageTimeout = time.Duration(s.resolveAgentTimeout(r.Context(), runRow, run.StageTypePlan)) * time.Second
	trigger.ImplementStageTimeout = time.Duration(s.resolveAgentTimeout(r.Context(), runRow, run.StageTypeImplement)) * time.Second

	text, err := prompt.Build(string(stage.Type), trigger)
	if err != nil {
		if errors.Is(err, prompt.ErrUnsupportedStage) {
			s.writeError(w, r, http.StatusNotImplemented, "unsupported_stage_type",
				"prompt construction not yet implemented for this stage type",
				map[string]any{"stage_type": string(stage.Type)})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"build prompt failed", map[string]any{"error": err.Error()})
		return
	}

	hash := signing.ComputeMessage([]byte(text))
	verifyCmd, verifyTimeoutSecs, verifyMaxIterations := s.resolveVerifyConfig(r.Context(), runRow, stage.Type)
	commitAuthorName, commitAuthorEmail := s.resolveAppBotIdentity(r.Context())
	resp := promptResponse{
		StageID:              stageID.String(),
		StageType:            string(stage.Type),
		Prompt:               text,
		PromptHash:           hex.EncodeToString(hash),
		AgentTimeoutSeconds:  s.resolveAgentTimeout(r.Context(), runRow, stage.Type),
		VerifyCommand:        verifyCmd,
		VerifyTimeoutSeconds: verifyTimeoutSecs,
		VerifyMaxIterations:  verifyMaxIterations,
		MinRunnerVersion:     version.MinRunnerVersion,
		AgentSelfRetry:       s.resolveAgentSelfRetryForStage(r.Context(), runRow, stage.Type),
		MaxRetriesSnapshot:   runRow.MaxRetriesSnapshot,
		RetryAttempt:         runRow.RetryAttempt,
		ScopeFiles:           scopeFiles,
		CommitAuthorName:     commitAuthorName,
		CommitAuthorEmail:    commitAuthorEmail,
		Fixup:                fixup,
		FixupBranch:          fixupBranch,
		FixupExpectedHeadSHA: fixupExpectedHeadSHA,
	}
	if runRow.DecomposedFrom != nil {
		resp.DecomposedFromRunID = runRow.DecomposedFrom.String()
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// handleGetStagePromptRender implements GET /v0/stages/{stage_id}/prompt-render.
//
// SPA-readable counterpart of handleGetStagePrompt: same response
// shape, same construction, but no X-Fishhawk-Signature requirement.
// The runner contract on the signature-authed path stays untouched.
//
// Read access tracks the existing stage/audit read endpoints — no
// auth gate at the handler level today; the surrounding middleware
// handles cookie/bearer resolution. Used by the implement-stage
// session view (#215) to show the user the deterministic prompt
// the agent received.
func (s *Server) handleGetStagePromptRender(w http.ResponseWriter, r *http.Request) {
	github := s.issueGetter()
	if s.cfg.RunRepo == nil || github == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "prompt_unconfigured",
			"prompt construction requires run repo and GitHub access to be configured", nil)
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

	switch stage.State {
	case run.StageStateAwaitingApproval, run.StageStateAwaitingChildren,
		run.StageStateSucceeded, run.StageStateFailed, run.StageStateCancelled:
		s.writeError(w, r, http.StatusConflict, "stage_not_runnable",
			"stage is not in a runnable state",
			map[string]any{"current_state": string(stage.State), "stage_id": stageID.String()})
		return
	}

	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), stage.RunID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run for stage failed", map[string]any{"error": err.Error()})
		return
	}

	trigger := prompt.Trigger{
		Source: string(runRow.TriggerSource),
		Repo:   runRow.Repo,
	}
	if runRow.TriggerRef != nil {
		if number, ok := parseIssueRef(*runRow.TriggerRef); ok {
			trigger.IssueNumber = number
			s.fillIssueContext(r.Context(), github, runRow, number, &trigger)
		}
	}
	// Plan-as-contract (#223): for implement stages, the approved
	// plan is the binding instruction. Look up the run's
	// plan-stage's most-recent standard_v1 artifact and feed it
	// into the prompt builder. Missing plan → fall back to the
	// issue-only template and emit `plan_missing_for_implement` so
	// the audit log captures the gap.
	var scopeFiles []scopeFile
	var fixup bool
	var fixupBranch string
	var fixupExpectedHeadSHA string
	if stage.Type == run.StageTypeImplement {
		approvedPlan, err := s.loadApprovedPlanForRun(r.Context(), runRow.ID)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"load approved plan failed", map[string]any{"error": err.Error()})
			return
		}
		if approvedPlan == nil {
			s.emitPlanMissingForImplement(r.Context(), runRow.ID, stage.ID)
		}
		trigger.ApprovedPlan = approvedPlan
		if approvedPlan != nil {
			budgetSecs := s.resolveAgentTimeout(r.Context(), runRow, run.StageTypeImplement)
			trigger.PredictionContext = &prompt.PredictionContext{
				PredictedMinutes:    approvedPlan.PredictedRuntimeMinutes,
				PredictedConfidence: string(approvedPlan.PredictedRuntimeConfidence),
				StageBudgetMinutes:  budgetSecs / 60,
			}
		}
		scopeFiles = scopeFilesFromPlan(approvedPlan)
		// Decomposition fan-out (#676): a child run narrows its scope to
		// the matched sub-plan's own scope.files when present, so the
		// runner's scope_handoff + scope-drift bound to this slice rather
		// than the parent's full scope. Falls back to the parent scope
		// when the sub-plan omits scope or the run isn't a decomposed child.
		if childScope := s.resolveDecomposedScopeFiles(r.Context(), runRow, approvedPlan); len(childScope) > 0 {
			scopeFiles = childScope
		}
		trigger.ScopeConstraint = s.resolveDecomposedScopeConstraint(r.Context(), runRow, approvedPlan)
		trigger.ApprovalConditions = s.resolveApprovalConditions(r.Context(), runRow)
		// Fold the authoritative add_scope_files paths a reviewer named at
		// approval time into the effective scope set (#824). This is the
		// structured, lossless replacement for the #730 prose scrape: it
		// stages directories, extensionless/repo-root files, and
		// described-not-spelled paths the regex misses. Applied first
		// (authoritative), then the #730 prose fold runs as a fallback —
		// both dedup by path and both no-op on an empty scope (keeps the
		// runner's git add -A fallback).
		scopeFiles = s.mergeStructuredScopeFiles(r.Context(), scopeFiles, s.resolveApprovalAddScopeFiles(r.Context(), runRow))
		// Fold files named by the approval conditions into the effective
		// scope set so a condition-authorized edit ships as a declared
		// path rather than benign scope_drift (#730). No-op when scope is
		// empty (keeps the runner's git add -A fallback).
		scopeFiles = s.mergeConditionScopeFiles(r.Context(), scopeFiles, trigger.ApprovalConditions)
		// Fold the operator-approved mid-stage scope amendment paths (#961),
		// same derivation as the dispatch path so the rendered view matches.
		scopeFiles = s.mergeApprovedScopeAmendments(r.Context(), scopeFiles, runRow.ID, stage.ID)
		// Fix-up pass (#762): when the operator routed implement-review
		// concerns back to this stage, deliver them as binding instructions
		// (reusing #558's framing) and fold any file the concern names into
		// the effective scope so a concern-authorized edit ships as a
		// declared path rather than benign scope_drift. No-op for a normal
		// (non-fix-up) implement dispatch.
		if rendered, joined := s.resolveFixupConcerns(r.Context(), runRow.ID, stage.ID); len(rendered) > 0 {
			trigger.FixupConcerns = rendered
			scopeFiles = s.mergeConditionScopeFiles(r.Context(), scopeFiles, &joined)
			// Fold the operator-declared net-new files (#823) into the
			// effective scope so the runner's #818 created-out-of-scope gate
			// stages them rather than failing category-B. No-op on an empty
			// scope (keeps the runner's git add -A fallback). Any created file
			// NOT declared here still trips the gate.
			scopeFiles = s.mergeFixupAllowCreate(r.Context(), scopeFiles, s.resolveFixupAllowCreate(r.Context(), runRow.ID, stage.ID))
			// Emit the fix-up routing flag (#784) so the rendered prompt view
			// and the runner-facing response stay byte-consistent. The SPA path
			// is read-only and never drives a commit; the same derivation keeps
			// the displayed and dispatched responses identical.
			fixup = true
			fixupBranch = s.fixupBranchForRun(r.Context(), runRow, stage)
			fixupExpectedHeadSHA = s.resolveFixupExpectedHeadSHA(r.Context(), runRow.ID, stage.ID)
		}
	}

	// Decompose-required hint: when the run's last plan approval was
	// rejected with --decompose, tell the agent it must populate
	// decomposition.sub_plans in the next plan attempt.
	if stage.Type == run.StageTypePlan {
		if s.loadLastDecomposeRejectionReason(r.Context(), runRow.ID) {
			trigger.DecomposeRequired = true
		}
		if hint, err := s.resolveCalibrationHint(r.Context(), runRow.WorkflowID); err != nil {
			slog.WarnContext(r.Context(), "calibration hint resolution failed", "error", err)
		} else {
			trigger.CalibrationHint = hint
		}
		if runRow.TriggerRef != nil {
			trigger.PriorRejectionFeedback = s.loadPriorRejectionFeedback(r.Context(), runRow.Repo, *runRow.TriggerRef, runRow.ID)
		}
		trigger.PriorSchemaValidationError = s.loadPriorSchemaValidationError(r.Context(), runRow.ID)
	}

	trigger.PlanStageTimeout = time.Duration(s.resolveAgentTimeout(r.Context(), runRow, run.StageTypePlan)) * time.Second
	trigger.ImplementStageTimeout = time.Duration(s.resolveAgentTimeout(r.Context(), runRow, run.StageTypeImplement)) * time.Second

	text, err := prompt.Build(string(stage.Type), trigger)
	if err != nil {
		if errors.Is(err, prompt.ErrUnsupportedStage) {
			s.writeError(w, r, http.StatusNotImplemented, "unsupported_stage_type",
				"prompt construction not yet implemented for this stage type",
				map[string]any{"stage_type": string(stage.Type)})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"build prompt failed", map[string]any{"error": err.Error()})
		return
	}

	hash := signing.ComputeMessage([]byte(text))
	verifyCmd, verifyTimeoutSecs, verifyMaxIterations := s.resolveVerifyConfig(r.Context(), runRow, stage.Type)
	commitAuthorName, commitAuthorEmail := s.resolveAppBotIdentity(r.Context())
	resp := promptResponse{
		StageID:              stageID.String(),
		StageType:            string(stage.Type),
		Prompt:               text,
		PromptHash:           hex.EncodeToString(hash),
		AgentTimeoutSeconds:  s.resolveAgentTimeout(r.Context(), runRow, stage.Type),
		VerifyCommand:        verifyCmd,
		VerifyTimeoutSeconds: verifyTimeoutSecs,
		VerifyMaxIterations:  verifyMaxIterations,
		MinRunnerVersion:     version.MinRunnerVersion,
		AgentSelfRetry:       s.resolveAgentSelfRetryForStage(r.Context(), runRow, stage.Type),
		MaxRetriesSnapshot:   runRow.MaxRetriesSnapshot,
		RetryAttempt:         runRow.RetryAttempt,
		ScopeFiles:           scopeFiles,
		CommitAuthorName:     commitAuthorName,
		CommitAuthorEmail:    commitAuthorEmail,
		Fixup:                fixup,
		FixupBranch:          fixupBranch,
		FixupExpectedHeadSHA: fixupExpectedHeadSHA,
	}
	if runRow.DecomposedFrom != nil {
		resp.DecomposedFromRunID = runRow.DecomposedFrom.String()
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// verifyPromptSignature reads the X-Fishhawk-Signature header and
// validates it against sha256("prompt:" + stage_id) using the
// run's stored public key. Returns true on success; on failure
// writes the response and returns false so the caller short-circuits.
func (s *Server) verifyPromptSignature(w http.ResponseWriter, r *http.Request, runID, stageID uuid.UUID) bool {
	sigHeader := r.Header.Get("X-Fishhawk-Signature")
	if sigHeader == "" {
		s.writeError(w, r, http.StatusUnauthorized, "signature_missing",
			"X-Fishhawk-Signature header is required", nil)
		return false
	}
	signature, err := hex.DecodeString(sigHeader)
	if err != nil {
		s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
			"X-Fishhawk-Signature is not valid hex",
			map[string]any{"error": err.Error()})
		return false
	}

	message := promptCanonicalMessage(stageID)
	if err := s.cfg.SigningRepo.Verify(r.Context(), runID, message, signature); err != nil {
		switch {
		case errors.Is(err, signing.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "signing_key_not_found",
				"no signing key issued for this run", map[string]any{"run_id": runID.String()})
		case errors.Is(err, signing.ErrExpired):
			s.writeError(w, r, http.StatusUnauthorized, "signing_key_expired",
				"signing key TTL has passed", map[string]any{"run_id": runID.String()})
		case errors.Is(err, signing.ErrSignatureInvalid):
			s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
				"signature does not match the run's stored public key", nil)
		default:
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"signature verification failed", map[string]any{"error": err.Error()})
		}
		return false
	}
	return true
}

// PromptCanonicalMessage exposes the canonical message the prompt
// endpoint signs over so the runner can derive the same bytes
// without re-implementing the format.
func PromptCanonicalMessage(stageID uuid.UUID) []byte {
	return promptCanonicalMessage(stageID)
}

func promptCanonicalMessage(stageID uuid.UUID) []byte {
	return signing.ComputeMessage([]byte("prompt:" + stageID.String()))
}

// loadApprovedPlanForRun returns the plan stage's most-recent
// kind=plan, schema_version=standard_v1 artifact for the run, decoded
// into a *plan.Plan. Returns (nil, nil) when no such artifact exists
// (race between plan upload and implement dispatch, or a manual run
// with no plan stage). The implement-stage prompt builder treats nil
// as "no plan available" and falls back to the issue-only template.
//
// CI-failure retry runs (#279 / E16) intentionally skip the plan
// stage — their implement stage is meant to re-run against the
// parent's already-approved plan. When the current run has no plan
// stage of its own, we walk ParentRunID upward until we find a run
// that does (or until the chain ends). The walk is capped at
// retryPlanChainDepth so a corrupt parent_run_id cycle can't loop
// forever.
//
// Errors are returned to the caller only when the underlying repo
// IO fails — a missing or malformed plan logs and yields nil so the
// prompt fetch stays robust against the kinds of mid-flight states
// the runner sees during re-tries.
func (s *Server) loadApprovedPlanForRun(ctx context.Context, runID uuid.UUID) (*plan.Plan, error) {
	if s.cfg.ArtifactRepo == nil || s.cfg.RunRepo == nil {
		return nil, nil
	}
	current := runID
	for depth := 0; depth < retryPlanChainDepth; depth++ {
		p, found, err := s.tryLoadPlanForRun(ctx, current)
		if err != nil {
			return nil, err
		}
		if found {
			return p, nil
		}
		// No plan stage on this run; walk to the parent.
		runRow, err := s.cfg.RunRepo.GetRun(ctx, current)
		if err != nil {
			return nil, fmt.Errorf("get run for parent walk: %w", err)
		}
		if runRow.ParentRunID == nil {
			return nil, nil
		}
		current = *runRow.ParentRunID
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: parent-walk hit depth cap",
		slog.String("run_id", runID.String()),
		slog.Int("max_depth", retryPlanChainDepth))
	return nil, nil
}

// calibrationHintMinSamples is the minimum number of historical implement-
// stage samples required before the calibration hint is appended to the
// plan-stage prompt. Below this threshold the section is silently omitted.
const calibrationHintMinSamples = 5

// resolveCalibrationHint loads runtime_observed audit entries for the
// workflow, filters to implement-stage samples, and computes calibration
// statistics. Returns nil when the AuditRepo is unconfigured, when RunRepo
// is nil (can't resolve workflow_id per entry), or when the sample count
// is below calibrationHintMinSamples. Errors degrade gracefully — the
// caller logs at WARN and proceeds with a hint-free prompt.
func (s *Server) resolveCalibrationHint(ctx context.Context, workflowID string) (*prompt.CalibrationHint, error) {
	if s.cfg.AuditRepo == nil {
		return nil, nil
	}
	const runtimeObservedCategory = "runtime_observed"
	cat := runtimeObservedCategory
	entries, err := s.cfg.AuditRepo.ListAll(ctx, audit.ListAllParams{Category: &cat})
	if err != nil {
		return nil, fmt.Errorf("list runtime_observed entries: %w", err)
	}
	var samples []runtimeObservedPayload
	for _, e := range entries {
		var p runtimeObservedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if p.StageType != "implement" {
			continue
		}
		if e.RunID == nil || s.cfg.RunRepo == nil {
			continue
		}
		runRow, err := s.cfg.RunRepo.GetRun(ctx, *e.RunID)
		if err != nil || runRow.WorkflowID != workflowID {
			continue
		}
		samples = append(samples, p)
	}
	result := computeCalibration(workflowID, "implement", samples)
	if result.Samples < calibrationHintMinSamples {
		return nil, nil
	}
	bands := make(map[string]prompt.CalibrationBand, len(result.ConfidenceBandAccuracy))
	for level, b := range result.ConfidenceBandAccuracy {
		bands[level] = prompt.CalibrationBand{Samples: b.Samples, WithinScale: b.Within1p5x}
	}
	return &prompt.CalibrationHint{
		Samples:          result.Samples,
		CalibrationRatio: result.CalibrationRatio,
		ActualP50Minutes: result.ActualP50Minutes,
		ActualP95Minutes: result.ActualP95Minutes,
		ConfidenceBands:  bands,
	}, nil
}

// retryPlanChainDepth caps the parent-walk in loadApprovedPlanForRun.
// In practice an auto-retry chain is at most a handful of links
// (max_retries defaults to 1); 8 is generous and bounds a corrupt
// cycle without imposing on legitimate workflows.
const retryPlanChainDepth = 8

// tryLoadPlanForRun looks for a standard_v1 plan artifact on the
// single run identified by runID. Returns (plan, true, nil) on a
// hit; (nil, false, nil) when the run has no plan stage or its plan
// stage has no usable plan artifact (caller should walk to parent);
// (nil, false, err) on repo IO failure.
func (s *Server) tryLoadPlanForRun(ctx context.Context, runID uuid.UUID) (*plan.Plan, bool, error) {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return nil, false, fmt.Errorf("list stages for run: %w", err)
	}
	var planStageID uuid.UUID
	for _, st := range stages {
		if st.Type == run.StageTypePlan {
			planStageID = st.ID
			break
		}
	}
	if planStageID == uuid.Nil {
		return nil, false, nil
	}
	arts, err := s.cfg.ArtifactRepo.ListForStage(ctx, planStageID)
	if err != nil {
		return nil, false, fmt.Errorf("list plan stage artifacts: %w", err)
	}
	var picked *artifact.Artifact
	for _, a := range arts {
		if a.Kind != artifact.KindPlan {
			continue
		}
		if a.SchemaVersion == nil || *a.SchemaVersion != "standard_v1" {
			continue
		}
		if picked == nil || a.CreatedAt.After(picked.CreatedAt) {
			picked = a
		}
	}
	if picked == nil {
		return nil, false, nil
	}
	var p plan.Plan
	if err := json.Unmarshal(picked.Content, &p); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: plan unmarshal failed",
			slog.String("run_id", runID.String()),
			slog.String("artifact_id", picked.ID.String()),
			slog.String("error", err.Error()),
		)
		return nil, false, nil
	}
	return &p, true, nil
}

// emitPlanMissingForImplement records the case where an implement-
// stage prompt was served without an approved plan. It's not an
// error in the HTTP sense — the runner gets a usable issue-only
// prompt — but the audit log should capture the gap so reviewers can
// tell whether the agent was working off the plan they approved.
//
// Best-effort: a failure to append the audit entry doesn't unwind
// the prompt response. Logged at warn level for operator visibility.
func (s *Server) emitPlanMissingForImplement(ctx context.Context, runID, stageID uuid.UUID) {
	if s.cfg.AuditRepo == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"run_id":   runID.String(),
		"stage_id": stageID.String(),
		"reason":   "no standard_v1 plan artifact found for the run's plan stage at implement-prompt fetch time",
	})
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "plan_missing_for_implement",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: append plan_missing_for_implement failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
	}
}

// fillIssueContext populates the trigger's IssueTitle, IssueBody,
// and IssueURL.
//
// Resolution order (#415):
//  1. The run row's cached IssueContext — present when the CLI
//     ran `gh issue view` at run-create time and shipped the
//     payload inline. Used as-is; no GitHub call.
//  2. The webhook-dispatched path: when the run carries an
//     installation_id but no cached payload, fetch via GitHub
//     App token (unchanged behavior).
//  3. Otherwise leave the title + body empty; the prompt
//     template falls back to a "URL only" shape the agent can
//     navigate via its own tools.
//
// IssueURL is derived from `repo + IssueNumber` rather than the
// API response's html_url — the canonical github.com URL is fully
// determined by those two fields, and avoiding the response
// dependency means the field is set even on a partial fetch.
func (s *Server) fillIssueContext(ctx context.Context, github issueGetter, runRow *run.Run, issueNumber int, trigger *prompt.Trigger) {
	// Set the URL up front so any of the three branches below
	// leave the link-only renderer with a working fallback.
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: parse repo failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("repo", runRow.Repo),
			slog.String("error", err.Error()),
		)
		return
	}
	trigger.IssueURL = fmt.Sprintf("https://github.com/%s/%s/issues/%d",
		repo.Owner, repo.Name, issueNumber)

	// Branch 1: operator's `gh` fetch at run-create time
	// pre-populated the title + body on the row. Prefer this
	// over a fresh GitHub call so local-runner runs (which lack
	// an installation_id) get the full prompt context.
	if runRow.IssueContext != nil {
		trigger.IssueTitle = runRow.IssueContext.Title
		trigger.IssueBody = runRow.IssueContext.Body
		// Comments (#618): map the cached comment snapshot into the
		// trigger so the plan-stage prompt can render comment-borne
		// refinements. Branch 2 (webhook fetch) fetches comments via
		// ListIssueComments below to populate the same shape.
		for _, c := range runRow.IssueContext.Comments {
			trigger.IssueComments = append(trigger.IssueComments, prompt.IssueComment{
				Author:    c.Author,
				Body:      c.Body,
				CreatedAt: c.CreatedAt,
			})
		}
		return
	}

	// Branch 2: webhook-dispatched runs — fetch via the App's
	// installation token. Unchanged from the pre-#415 behavior.
	if runRow.InstallationID == nil {
		return
	}
	issue, err := github.GetIssue(ctx, *runRow.InstallationID, repo, issueNumber)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: get issue failed",
			slog.String("run_id", runRow.ID.String()),
			slog.Int("issue", issueNumber),
			slog.String("error", err.Error()),
		)
		return
	}
	trigger.IssueTitle = issue.Title
	trigger.IssueBody = issue.Body

	// Comments (#621): fetch the issue's comment thread so webhook-
	// triggered runs render comment-borne refinements identically to
	// branch 1. Best-effort: a fetch error degrades to title+body
	// rather than failing the prompt build (same WARN-and-proceed
	// posture as the GetIssue failure above).
	comments, err := github.ListIssueComments(ctx, *runRow.InstallationID, repo, issueNumber)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list issue comments failed",
			slog.String("run_id", runRow.ID.String()),
			slog.Int("issue", issueNumber),
			slog.String("error", err.Error()),
		)
		return
	}
	for _, c := range comments {
		trigger.IssueComments = append(trigger.IssueComments, prompt.IssueComment{
			Author:    c.Author,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		})
	}
}

// issueGetter returns the configured client cast to the small
// interface the handler needs. Returns nil when GitHub is unset.
// The promptIssueGetterOverride test seam takes precedence so
// handler tests don't need a real *githubclient.Client.
func (s *Server) issueGetter() issueGetter {
	if s.promptIssueGetterOverride != nil {
		return s.promptIssueGetterOverride
	}
	if s.cfg.GitHub == nil {
		return nil
	}
	return s.cfg.GitHub
}

// appIdentityGetter is the slice of githubclient.Client that
// resolveAppBotIdentity consumes. Defining the interface in the server
// package lets tests substitute a stub without an httptest fake of
// api.github.com — *githubclient.Client satisfies it in production.
type appIdentityGetter interface {
	GetApp(ctx context.Context) (*githubclient.App, error)
	GetUser(ctx context.Context, login string) (*githubclient.User, error)
}

// appIdentityResolver returns the configured client cast to the small
// interface resolveAppBotIdentity needs, or nil when GitHub is unset.
// The appIdentityGetterOverride test seam takes precedence.
func (s *Server) appIdentityResolver() appIdentityGetter {
	if s.appIdentityGetterOverride != nil {
		return s.appIdentityGetterOverride
	}
	if s.cfg.GitHub == nil {
		return nil
	}
	return s.cfg.GitHub
}

// resolveAppBotIdentity returns the GitHub App bot account's git commit
// identity (name, email) for echoing on the prompt response so App-backed
// commits attribute to the App's bot account rather than the runner's
// hardcoded fallback (#722). A SUCCESSFUL result is memoized for the
// process lifetime (the App slug and bot user-id are App-global and
// immutable), so GetApp + GetUser run at most once regardless of how many
// stages dispatch.
//
// Returns ("", "") when GitHub is unconfigured, the App JWT isn't wired
// (dev / CLI client built via New), or any API call fails — and a
// failed/empty resolution is NOT cached, so a transient first-call error
// can't permanently disable dynamic attribution; it is retried on the next
// fetch (the runner falls back to gitops.DefaultAuthorName/Email meanwhile).
// Failures log at WARN — never block the prompt response.
func (s *Server) resolveAppBotIdentity(ctx context.Context) (name, email string) {
	s.appBotIdentityMu.Lock()
	if s.appBotIdentityResolved {
		name, email = s.appBotIdentityName, s.appBotIdentityEmail
		s.appBotIdentityMu.Unlock()
		return name, email
	}
	s.appBotIdentityMu.Unlock()

	// Not yet resolved. Compute outside the lock so concurrent prompt fetches
	// don't serialize on the GetApp/GetUser round-trips; a rare double-compute
	// is harmless (slug/bot-id are stable, so it yields the same result).
	name, email = s.computeAppBotIdentity(ctx)
	if name == "" {
		// Transient/empty resolution is NOT cached — retry on the next fetch
		// so a first-call hiccup can't permanently disable dynamic attribution.
		return "", ""
	}
	s.appBotIdentityMu.Lock()
	s.appBotIdentityName, s.appBotIdentityEmail = name, email
	s.appBotIdentityResolved = true
	s.appBotIdentityMu.Unlock()
	return name, email
}

// computeAppBotIdentity performs the actual GetApp + GetUser resolution.
// Split out from resolveAppBotIdentity so the sync.Once wrapper stays
// trivial. Returns empty strings on any failure (logged at WARN).
func (s *Server) computeAppBotIdentity(ctx context.Context) (name, email string) {
	gh := s.appIdentityResolver()
	if gh == nil {
		return "", ""
	}
	app, err := gh.GetApp(ctx)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: resolve app bot identity: get app failed",
			slog.String("error", err.Error()))
		return "", ""
	}
	if app.Slug == "" {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: resolve app bot identity: app slug empty")
		return "", ""
	}
	login := app.Slug + "[bot]"
	user, err := gh.GetUser(ctx, login)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: resolve app bot identity: get user failed",
			slog.String("login", login), slog.String("error", err.Error()))
		return "", ""
	}
	if user.ID == 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: resolve app bot identity: user id zero",
			slog.String("login", login))
		return "", ""
	}
	name = login
	email = fmt.Sprintf("%d+%s@users.noreply.github.com", user.ID, login)
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "prompt: resolved app bot commit identity",
		slog.String("name", name), slog.String("email", email))
	return name, email
}

// resolveAgentTimeout returns the spec-governed timeout in seconds for the
// given run stage. Returns 0 when the workflow spec is absent or unparseable
// — the runner falls back to its own 15-minute constant in that case.
func (s *Server) resolveAgentTimeout(ctx context.Context, runRow *run.Run, stageType run.StageType) int {
	if runRow.WorkflowSpec == nil {
		return 0
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: parse workflow spec for timeout",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()),
		)
		return 0
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return 0
	}
	// Primary match: spec stage ID == string(stageType) (canonical workflow).
	// Fallback: spec stage Type == stageType string.
	var specStage spec.Stage
	for _, st := range wf.Stages {
		if st.ID == string(stageType) {
			specStage = st
			break
		}
	}
	if specStage.ID == "" {
		for _, st := range wf.Stages {
			if string(st.Type) == string(stageType) {
				specStage = st
				break
			}
		}
	}
	resolved := spec.ResolveStageTimeout(wf, specStage, spec.DefaultStageTimeout)
	// Implement stages get a dynamically-widened runtime kill cap (#523):
	// correctly-scoped work whose actual runtime lands in the deep
	// calibration tail should be allowed to finish rather than being
	// SIGKILLed mid-tail. Every other stage type keeps the spec-resolved
	// value unchanged. Both call sites that pass run.StageTypeImplement
	// (prompt.go:248/399 wire value and the :226/377 prompt-text hint)
	// flow through here, so the hint and the kill cap can't diverge.
	if stageType == run.StageTypeImplement {
		return int(s.resolveImplementTimeout(ctx, runRow, resolved).Seconds())
	}
	return int(resolved.Seconds())
}

// Dynamic implement-stage timeout terms (#523, #994). The shared
// plan-gate budget (resolvePlanGateBudget) is max(spec budget, p95×1.5)
// clamped to spec×2 — the same number checkPlanBudget enforces at
// approval and the plan-review prompt cites as gate evidence. The
// runtime kill cap (resolveImplementTimeout) builds on that base and
// additionally raises it to predicted×2, so the cap can widen past the
// gate budget but the two can never disagree about the shared terms.
const (
	implementPlanMultiplier       = 2   // plan.predicted_runtime_minutes × 2
	implementP95Multiplier        = 1.5 // calibration implement p95 × 1.5
	implementTimeoutCeilingFactor = 2   // hard ceiling = spec budget × 2
)

// resolvePlanGateBudget computes the shared implement-stage budget (#994):
// the spec-resolved budget raised to the calibration p95×implementP95Multiplier
// when data exists, clamped to spec×implementTimeoutCeilingFactor. Returns
// the resolved value and its source ("spec" | "p95" | "ceiling").
//
// It deliberately EXCLUDES the plan term (predicted×implementPlanMultiplier)
// resolveImplementTimeout adds: feeding the plan's own prediction into the
// budget would make the approval gate self-satisfying — any prediction X
// yields a budget ≥ 2X up to the ceiling, so nothing could ever fail.
//
// Three surfaces consume it so they all cite the same number and source:
// checkPlanBudget (approval gate + 422/audit payloads), resolveImplementTimeout
// (kill-cap base), and runPlanReviews (the plan-review prompt's Budget check
// gate evidence). Fail-open: calibration unavailability leaves the budget at
// the spec-resolved floor. Reuses the memoized implementCalibrationP95
// (60s TTL), so prompt build and approval gate read the same cached p95.
func (s *Server) resolvePlanGateBudget(ctx context.Context, workflowID string, specResolved time.Duration) (time.Duration, string) {
	candidate, source := specResolved, "spec"
	if p95, ok := s.implementCalibrationP95(ctx, workflowID); ok && p95 > 0 {
		if p95Term := time.Duration(p95 * implementP95Multiplier * float64(time.Minute)); p95Term > candidate {
			candidate, source = p95Term, "p95"
		}
	}
	if ceiling := specResolved * implementTimeoutCeilingFactor; candidate > ceiling {
		candidate, source = ceiling, "ceiling"
	}
	return candidate, source
}

// resolveImplementTimeout computes the dynamic wall-clock kill cap for an
// implement stage. It starts from resolvePlanGateBudget's shared base —
// max(spec budget, p95×1.5) clamped to spec×2, the floor, so the timeout
// can never be smaller than the approval-gate budget — raises the candidate
// to predicted_runtime_minutes×2 when an approved plan is loadable, then
// re-clamps the result to spec×implementTimeoutCeilingFactor.
//
// Best-effort throughout: a plan-load or calibration failure leaves the
// candidate at the spec floor. Crucially, at PLAN-stage prompt build there
// is no approved plan yet, so loadApprovedPlanForRun returns nil and the
// plan term falls back to the floor — the implement budget shown to the
// planner stays spec-resolved (no circularity).
func (s *Server) resolveImplementTimeout(ctx context.Context, runRow *run.Run, specResolved time.Duration) time.Duration {
	// Shared base (#994): max(spec, p95×1.5) clamped to spec×2 — the same
	// number checkPlanBudget gates on, so gate and kill cap cannot drift.
	candidate, winner := s.resolvePlanGateBudget(ctx, runRow.WorkflowID, specResolved)

	// Raw p95 term, re-read for the log line below (memoized — the same
	// cached scan resolvePlanGateBudget just consulted).
	var planTerm, p95Term time.Duration
	if p95, ok := s.implementCalibrationP95(ctx, runRow.WorkflowID); ok && p95 > 0 {
		p95Term = time.Duration(p95 * implementP95Multiplier * float64(time.Minute))
	}

	// Plan term: predicted_runtime_minutes × 2. Best-effort — any load
	// failure or absent/zero prediction leaves the candidate at the base.
	if p, err := s.loadApprovedPlanForRun(ctx, runRow.ID); err == nil && p != nil && p.PredictedRuntimeMinutes > 0 {
		planTerm = time.Duration(p.PredictedRuntimeMinutes*implementPlanMultiplier) * time.Minute
		if planTerm > candidate {
			candidate, winner = planTerm, "plan"
		}
	}

	// Re-clamp: a pathological plan estimate cannot produce an unbounded
	// timeout (the base is already clamped).
	if ceiling := specResolved * implementTimeoutCeilingFactor; candidate > ceiling {
		candidate, winner = ceiling, "ceiling"
	}

	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "prompt: resolved dynamic implement timeout",
		slog.String("run_id", runRow.ID.String()),
		slog.Int64("spec_budget_seconds", int64(specResolved.Seconds())),
		slog.Int64("plan_term_seconds", int64(planTerm.Seconds())),
		slog.Int64("p95_term_seconds", int64(p95Term.Seconds())),
		slog.Int64("timeout_seconds", int64(candidate.Seconds())),
		slog.String("winner", winner),
	)
	return candidate
}

// resolveVerifyConfig returns the verify command, timeout (in seconds),
// and max-iterations budget for the given stage from the run's workflow
// spec. Returns ("", 0, 0) when the spec is absent, the stage declares no
// executor.verify block, or the timeout is zero. Mirrors
// resolveAgentTimeout's parse + lookup pattern.
func (s *Server) resolveVerifyConfig(ctx context.Context, runRow *run.Run, stageType run.StageType) (command string, timeoutSecs int, maxIterations int) {
	if runRow.WorkflowSpec == nil {
		return "", 0, 0
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: parse workflow spec for verify config",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()),
		)
		return "", 0, 0
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return "", 0, 0
	}
	var specStage spec.Stage
	for _, st := range wf.Stages {
		if st.ID == string(stageType) {
			specStage = st
			break
		}
	}
	if specStage.ID == "" {
		for _, st := range wf.Stages {
			if string(st.Type) == string(stageType) {
				specStage = st
				break
			}
		}
	}
	if specStage.Executor.Verify == nil || specStage.Executor.Verify.Command == "" {
		return "", 0, 0
	}
	secs := int(specStage.Executor.Verify.Timeout.Seconds())
	return specStage.Executor.Verify.Command, secs, specStage.Executor.Verify.MaxIterations
}

// parseIssueRef extracts the issue number from a TriggerRef of the
// form "issue:<n>". Returns (n, true) on match; (0, false) otherwise.
func parseIssueRef(ref string) (int, bool) {
	const prefix = "issue:"
	if !strings.HasPrefix(ref, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(ref[len(prefix):])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// parseRepoOwnerName splits "owner/name" into a RepoRef. Returns
// an error if the input doesn't contain exactly one slash with
// non-empty segments.
func parseRepoOwnerName(s string) (githubclient.RepoRef, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return githubclient.RepoRef{}, fmt.Errorf("repo %q is not owner/name", s)
	}
	return githubclient.RepoRef{Owner: parts[0], Name: parts[1]}, nil
}

// resolveAgentSelfRetryForStage returns whether the workflow spec opts the
// given stage type into runner-side self-retry on category-A/C failures
// (ADR-023). Mirrors resolveVerifyConfig's parse + lookup pattern. Returns
// false on any error (nil spec, missing workflow, parse failure) so the
// runner degrades gracefully to the pre-ADR-023 behavior.
func (s *Server) resolveAgentSelfRetryForStage(ctx context.Context, runRow *run.Run, stageType run.StageType) bool {
	if runRow.WorkflowSpec == nil {
		return false
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: parse workflow spec for agent_self_retry",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return false
	}
	var specStage spec.Stage
	for _, st := range wf.Stages {
		if st.ID == string(stageType) {
			specStage = st
			break
		}
	}
	if specStage.ID == "" {
		for _, st := range wf.Stages {
			if string(st.Type) == string(stageType) {
				specStage = st
				break
			}
		}
	}
	return specStage.Executor.AgentSelfRetry
}

// loadPriorRejectionFeedback searches the most-recent prior runs for
// the same trigger_ref and returns the rejection_comment from the newest
// approval_submitted audit entry where decision=reject and
// rejection_comment is non-empty. Returns nil when there is no matching
// prior rejection, when RunRepo or AuditRepo is unconfigured, or on any
// error (best-effort, same posture as CalibrationHint). At most 3 prior
// runs are inspected to bound audit fan-out; at most 10 runs are fetched
// from the repo in total (Limit=10).
func (s *Server) loadPriorRejectionFeedback(ctx context.Context, repo, triggerRef string, currentRunID uuid.UUID) *string {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return nil
	}
	ref := triggerRef
	runs, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		Repo:       repo,
		TriggerRef: &ref,
		Limit:      10,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list runs for prior rejection failed",
			slog.String("trigger_ref", triggerRef),
			slog.String("error", err.Error()),
		)
		return nil
	}

	checked := 0
	for _, r := range runs {
		if r.ID == currentRunID {
			continue
		}
		if checked >= 3 {
			break
		}
		checked++

		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, r.ID, "approval_submitted")
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list approval_submitted for prior run failed",
				slog.String("run_id", r.ID.String()),
				slog.String("error", err.Error()),
			)
			continue
		}

		// Scan newest-first (ListForRunByCategory returns entries ordered ASC by ts).
		for i := len(entries) - 1; i >= 0; i-- {
			var payload struct {
				Decision         string `json:"decision"`
				RejectionComment string `json:"rejection_comment"`
			}
			if err := json.Unmarshal(entries[i].Payload, &payload); err != nil {
				continue
			}
			if payload.Decision == "reject" && payload.RejectionComment != "" {
				c := payload.RejectionComment
				s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
					"prompt: loaded prior rejection feedback into plan prompt",
					slog.String("prior_run_id", r.ID.String()),
					slog.Int("comment_bytes", len(c)),
				)
				return &c
			}
		}
	}
	return nil
}

// loadPriorSchemaValidationError scans the run's plan_schema_retry audit
// entries (newest-first) and returns the newest entry's validation_error
// (#646). Used by the plan-stage prompt builder to inject a binding "fix
// exactly this" section on a re-dispatched plan attempt after a transient
// schema-validation failure. The payload-key (validation_error) is the
// contract this reader shares with the trySchemaRetry writer in plan.go —
// the cross-boundary seam test guards it from drifting.
//
// Best-effort: returns nil when the AuditRepo is unconfigured, no
// plan_schema_retry entry exists, or on any error (WARN-logged), so the
// prompt fetch stays robust.
func (s *Server) loadPriorSchemaValidationError(ctx context.Context, runID uuid.UUID) *string {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "plan_schema_retry")
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list plan_schema_retry audit failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	// Scan newest-first (ListForRunByCategory returns entries ordered ASC by ts).
	for i := len(entries) - 1; i >= 0; i-- {
		var payload struct {
			ValidationError string `json:"validation_error"`
		}
		if err := json.Unmarshal(entries[i].Payload, &payload); err != nil {
			continue
		}
		if payload.ValidationError != "" {
			c := payload.ValidationError
			return &c
		}
	}
	return nil
}

// resolveFixupConcerns returns the operator-selected implement-review
// concerns for an implement stage that has been re-opened for a bounded
// fix-up pass (#762), rendered one-per-line for the prompt's binding
// "### Fix-up concerns" section, alongside the same concern text joined for
// scope-file extraction.
//
// It reads the stage_fixup_triggered audit entries the fix-up handler writes
// (server/fixup.go), filters to the current stage, and uses the NEWEST entry
// — a fix-up re-opens the stage to pending and the renderer must reflect the
// most recent trigger. Each entry's `concerns` field is the resolved
// []planreview.Concern set the operator selected; they are formatted as
// "[severity/category] note" so the agent sees the full reviewer context.
//
// Returns (nil, "") when the AuditRepo is unconfigured, the stage carries no
// fix-up trigger (the common, non-fix-up case), or on any error — best-effort,
// same WARN-and-proceed posture as the other prompt resolvers. The joined
// string is the input to mergeConditionScopeFiles so a concern that names a
// file folds that path into the effective scope set.
func (s *Server) resolveFixupConcerns(ctx context.Context, runID, stageID uuid.UUID) (rendered []string, joined string) {
	if s.cfg.AuditRepo == nil {
		return nil, ""
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageFixupTriggered)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list stage_fixup_triggered audit failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return nil, ""
	}
	// Scan newest-first (ListForRunByCategory returns entries ordered ASC by
	// ts) for the first entry bound to this stage with a non-empty concern set.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var payload struct {
			Concerns []planreview.Concern `json:"concerns"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: unmarshal stage_fixup_triggered payload failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", err.Error()),
			)
			continue
		}
		if len(payload.Concerns) == 0 {
			continue
		}
		var notes []string
		rendered = make([]string, 0, len(payload.Concerns))
		for _, c := range payload.Concerns {
			rendered = append(rendered, fmt.Sprintf("[%s/%s] %s", c.Severity, c.Category, c.Note))
			notes = append(notes, c.Note)
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"prompt: loaded fix-up concerns into implement prompt",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.Int("concern_count", len(rendered)),
		)
		return rendered, strings.Join(notes, "\n")
	}
	return nil, ""
}

// resolveFixupExpectedHeadSHA returns the run's recorded head SHA — the
// newest head_sha across the run's reported-head ledger entries
// (lineageLedgerCategories: pull_request_opened / child_pushed /
// fixup_pushed), the same audit source buildReportedHeadLedger feeds the
// ADR-035 branch-lineage verifier and resolveLastRunAuthoredHead. A
// fix-up dispatch advertises it as `fixup_expected_head_sha` so the
// runner can verify the PR-branch tip it fetched IS the stage's recorded
// head before invoking the agent (#967).
//
// Returns "" when the AuditRepo is unconfigured, no entry carries a
// head_sha, or on any read error — best-effort WARN-and-proceed, same
// posture as the other prompt resolvers. The runner then skips the SHA
// comparison (checkout only) rather than blocking the pass.
func (s *Server) resolveFixupExpectedHeadSHA(ctx context.Context, runID, stageID uuid.UUID) string {
	if s.cfg.AuditRepo == nil {
		return ""
	}
	var newest *audit.Entry
	var newestSHA string
	for _, cat := range lineageLedgerCategories {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"prompt: list reported-head audit entries for fixup_expected_head_sha failed; omitting field",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("category", cat),
				slog.String("error", err.Error()),
			)
			return ""
		}
		for _, e := range entries {
			var payload struct {
				HeadSHA string `json:"head_sha"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err != nil || payload.HeadSHA == "" {
				continue
			}
			if newest == nil || e.Timestamp.After(newest.Timestamp) ||
				(e.Timestamp.Equal(newest.Timestamp) && e.Sequence > newest.Sequence) {
				newest = e
				newestSHA = payload.HeadSHA
			}
		}
	}
	if newest == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"prompt: no reported-head audit entry found for fixup_expected_head_sha; omitting field",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
		)
		return ""
	}
	return newestSHA
}

// resolveFixupAllowCreate returns the net-new file paths the operator
// declared on the fix-up that re-opened this implement stage (#823),
// folded into the effective scope.files so the runner's #818
// created-out-of-scope gate stages them rather than failing category-B.
//
// It mirrors resolveFixupConcerns: it reads the stage_fixup_triggered
// audit entries (server/fixup.go), filters to the current stage, and
// uses the NEWEST entry (a fix-up re-opens the stage to pending and the
// renderer must reflect the most recent trigger). Each entry's
// `allow_create` field is the validated, repo-relative path slice the
// fix-up handler persisted.
//
// Returns nil when the AuditRepo is unconfigured, the stage carries no
// fix-up trigger (the common, non-fix-up case), the newest trigger
// declared no paths, or on any error — best-effort, same WARN-and-proceed
// posture as the other prompt resolvers.
func (s *Server) resolveFixupAllowCreate(ctx context.Context, runID, stageID uuid.UUID) []string {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageFixupTriggered)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list stage_fixup_triggered audit for allow_create failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	// Scan newest-first (ListForRunByCategory returns entries ordered ASC by
	// ts) for the first entry bound to this stage.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var payload struct {
			AllowCreate []string `json:"allow_create"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: unmarshal stage_fixup_triggered allow_create failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", err.Error()),
			)
			return nil
		}
		if len(payload.AllowCreate) == 0 {
			return nil
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"prompt: loaded fix-up allow_create paths into implement scope",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.Int("count", len(payload.AllowCreate)),
		)
		return payload.AllowCreate
	}
	return nil
}

// matchDecomposedSubPlan returns the sub-plan whose title prefixes the
// child run's IssueContext.Body, plus its index within the decomposition's
// sub_plans. Returns (nil, -1) when:
//   - the run is not decomposed (DecomposedFrom == nil)
//   - the run has no cached IssueContext (can't match a sub-plan without it)
//   - parentPlan is nil or carries no decomposition (degrade gracefully)
//   - no sub-plan title matches the child's IssueContext.Body prefix
//     (defensive — a wrong match is worse than none)
//
// parentPlan is the already-loaded approved plan for the child run; for a
// decomposed child loadApprovedPlanForRun walks ParentRunID up to the
// parent's decomposed plan, so the caller's single load is reused here
// instead of re-reading the artifact.
//
// Matching uses strings.HasPrefix(body, "## "+title+"\n\n"), which is the
// invariant enforced by childIssueContextFromSubPlan in orchestrator.go.
func matchDecomposedSubPlan(runRow *run.Run, parentPlan *plan.Plan) (*plan.SubPlanSummary, int) {
	if runRow.DecomposedFrom == nil || runRow.IssueContext == nil {
		return nil, -1
	}
	if parentPlan == nil || parentPlan.Decomposition == nil {
		return nil, -1
	}
	body := runRow.IssueContext.Body
	for i := range parentPlan.Decomposition.SubPlans {
		sub := &parentPlan.Decomposition.SubPlans[i]
		if strings.HasPrefix(body, "## "+sub.Title+"\n\n") {
			return sub, i
		}
	}
	return nil, -1
}

// resolveDecomposedScopeConstraint builds a *prompt.ScopeConstraint for
// child runs of a decomposed plan. Returns nil when the child doesn't
// match a sub-plan (see matchDecomposedSubPlan). parentPlan is the
// caller's already-loaded approved plan — for a decomposed child this is
// the parent's decomposed plan — so no additional artifact read happens.
func (s *Server) resolveDecomposedScopeConstraint(ctx context.Context, runRow *run.Run, parentPlan *plan.Plan) *prompt.ScopeConstraint {
	matched, matchIdx := matchDecomposedSubPlan(runRow, parentPlan)
	if matched == nil {
		return nil
	}

	var siblingHints []string
	for i, sub := range parentPlan.Decomposition.SubPlans {
		if i != matchIdx {
			siblingHints = append(siblingHints, sub.ScopeHint)
		}
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
		"prompt: injected scope constraint for decomposed child",
		slog.String("child_run_id", runRow.ID.String()),
		slog.String("parent_run_id", runRow.DecomposedFrom.String()),
		slog.Int("sibling_count", len(siblingHints)),
	)
	return &prompt.ScopeConstraint{
		ScopeHint:    matched.ScopeHint,
		ParentRunID:  runRow.DecomposedFrom.String(),
		SiblingHints: siblingHints,
	}
}

// resolveDecomposedScopeFiles returns the matched sub-plan's own
// scope.files for a decomposed child, converted to the prompt-response
// wire shape. Returns nil when the child doesn't match a sub-plan or the
// matched sub-plan omits scope — in which case the caller keeps the
// parent plan's full scope.files (backward-compatible fallback).
// parentPlan is the caller's already-loaded approved plan, reused here
// rather than re-read.
func (s *Server) resolveDecomposedScopeFiles(ctx context.Context, runRow *run.Run, parentPlan *plan.Plan) []scopeFile {
	matched, _ := matchDecomposedSubPlan(runRow, parentPlan)
	if matched == nil || matched.Scope == nil {
		return nil
	}
	files := scopeFilesFromScope(matched.Scope)
	if len(files) == 0 {
		return nil
	}
	// Fold the coupled stem-sibling *_test.go for each owned source file into
	// the narrowed slice so "write the coupled unit tests" is always in-scope
	// for the slice that owns the code (#1083). foldScopePaths dedups an
	// already-scoped sibling and defaults net-new ones to operation=modify.
	files = s.foldScopePaths(ctx, files, coupledTestSiblings(files), "coupled-test-sibling")
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
		"prompt: narrowed scope_files to sub-plan slice for decomposed child",
		slog.String("child_run_id", runRow.ID.String()),
		slog.String("parent_run_id", runRow.DecomposedFrom.String()),
		slog.Int("file_count", len(files)),
	)
	return files
}

// loadLastDecomposeRejectionReason scans the run's approval_submitted
// audit entries (newest-first) and returns true when it finds one with
// decision=reject and reject_reason=decompose_required. Used by the
// plan-stage prompt builder to inject a binding decompose hint on
// re-plan attempts after the approver requested decomposition.
func (s *Server) loadLastDecomposeRejectionReason(ctx context.Context, runID uuid.UUID) bool {
	if s.cfg.AuditRepo == nil {
		return false
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "approval_submitted")
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list approval_submitted audit failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}
	for i := len(entries) - 1; i >= 0; i-- {
		var payload struct {
			Decision     string `json:"decision"`
			RejectReason string `json:"reject_reason"`
		}
		if err := json.Unmarshal(entries[i].Payload, &payload); err != nil {
			continue
		}
		if payload.Decision == "reject" && payload.RejectReason == "decompose_required" {
			return true
		}
	}
	return false
}

// loadApprovalConditions scans the run's approval_submitted audit entries
// (newest-first) for the first entry where decision=="approve" and the
// comment payload key is non-empty. Returns the comment string (capped at
// 4000 bytes) or nil when none is found. Best-effort: WARN-logs and returns
// nil on any error.
func (s *Server) loadApprovalConditions(ctx context.Context, runID uuid.UUID) *string {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "approval_submitted")
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list approval_submitted for conditions failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		var payload struct {
			Decision string `json:"decision"`
			Comment  string `json:"comment"`
		}
		if err := json.Unmarshal(entries[i].Payload, &payload); err != nil {
			continue
		}
		if payload.Decision == "approve" && payload.Comment != "" {
			c := payload.Comment
			const maxConditionBytes = 4000
			if len(c) > maxConditionBytes {
				c = c[:maxConditionBytes] + "...[truncated]"
			}
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"prompt: loaded approval conditions into implement prompt",
				slog.String("run_id", runID.String()),
				slog.Int("comment_bytes", len(payload.Comment)),
			)
			return &c
		}
	}
	return nil
}

// resolveApprovalConditions returns the binding approve-with-conditions text
// for an implement-stage prompt, resolving across the decomposition fan-out
// boundary (#677). It first reads the run's own approval_submitted entries
// via loadApprovalConditions; for a decomposed child (DecomposedFrom != nil)
// that has no plan stage and no human approval gate of its own, this is
// always nil, so it falls back to the PARENT run's conditions — mirroring
// loadApprovedPlanForRun's parent walk (#558 approval-note delivery now
// reaches implement-only children).
//
// The child-first lookup keeps standalone runs unchanged (DecomposedFrom nil
// → exactly loadApprovalConditions(runRow.ID)) and future-proofs the case
// where a child ever gains its own gate: its own conditions win over the
// parent's.
//
// Runs minted by the CI-retry or category-B recovery paths (#978) carry
// ParentRunID instead of DecomposedFrom; they execute the parent's plan,
// so the parent's binding conditions fall through to them too —
// single-level only, mirroring the decomposition fallback (the deeper
// walk stays loadApprovedPlanForRun's plan-side mechanism).
func (s *Server) resolveApprovalConditions(ctx context.Context, runRow *run.Run) *string {
	if cond := s.loadApprovalConditions(ctx, runRow.ID); cond != nil {
		return cond
	}
	if runRow.DecomposedFrom != nil {
		cond := s.loadApprovalConditions(ctx, *runRow.DecomposedFrom)
		if cond != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"prompt: inherited approval conditions from decomposition parent",
				slog.String("child_run_id", runRow.ID.String()),
				slog.String("parent_run_id", runRow.DecomposedFrom.String()),
			)
		}
		return cond
	}
	if runRow.ParentRunID == nil {
		return nil
	}
	cond := s.loadApprovalConditions(ctx, *runRow.ParentRunID)
	if cond != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"prompt: inherited approval conditions from retry/recovery parent",
			slog.String("child_run_id", runRow.ID.String()),
			slog.String("parent_run_id", runRow.ParentRunID.String()),
		)
	}
	return cond
}

// loadApprovalAddScopeFiles scans the run's approval_submitted audit entries
// (newest-first) for the first entry where decision=="approve" and returns its
// structured add_scope_files slice (#824). Returns nil when none is found.
// Best-effort: WARN-logs and returns nil on any error.
func (s *Server) loadApprovalAddScopeFiles(ctx context.Context, runID uuid.UUID) []string {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "approval_submitted")
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list approval_submitted for add_scope_files failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		var payload struct {
			Decision      string   `json:"decision"`
			AddScopeFiles []string `json:"add_scope_files"`
		}
		if err := json.Unmarshal(entries[i].Payload, &payload); err != nil {
			continue
		}
		if payload.Decision == "approve" && len(payload.AddScopeFiles) > 0 {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"prompt: loaded structured add_scope_files for implement scope",
				slog.String("run_id", runID.String()),
				slog.Int("count", len(payload.AddScopeFiles)),
			)
			return payload.AddScopeFiles
		}
	}
	return nil
}

// resolveApprovalAddScopeFiles returns the structured add_scope_files paths
// for an implement-stage prompt, resolving across the decomposition fan-out
// boundary (#824, mirroring resolveApprovalConditions / #677). It reads the
// run's own approval_submitted entries first; for a decomposed child with no
// gate of its own that yields nil, so it falls back to the PARENT run's paths
// so folded paths reach implement-only decomposed children.
//
// CI-retry / category-B recovery children (#978) carry ParentRunID instead
// of DecomposedFrom and get the same single-level fallback: the parent's
// folded paths were part of its effective scope and must reach the
// recovery implement stage too.
func (s *Server) resolveApprovalAddScopeFiles(ctx context.Context, runRow *run.Run) []string {
	if paths := s.loadApprovalAddScopeFiles(ctx, runRow.ID); len(paths) > 0 {
		return paths
	}
	if runRow.DecomposedFrom != nil {
		paths := s.loadApprovalAddScopeFiles(ctx, *runRow.DecomposedFrom)
		if len(paths) > 0 {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"prompt: inherited add_scope_files from decomposition parent",
				slog.String("child_run_id", runRow.ID.String()),
				slog.String("parent_run_id", runRow.DecomposedFrom.String()),
			)
		}
		return paths
	}
	if runRow.ParentRunID == nil {
		return nil
	}
	paths := s.loadApprovalAddScopeFiles(ctx, *runRow.ParentRunID)
	if len(paths) > 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"prompt: inherited add_scope_files from retry/recovery parent",
			slog.String("child_run_id", runRow.ID.String()),
			slog.String("parent_run_id", runRow.ParentRunID.String()),
		)
	}
	return paths
}
