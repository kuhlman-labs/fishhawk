package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
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
	DecomposedFromRunID string `json:"decomposed_from_run_id,omitempty"`
	// SliceIndex is the decomposed child's 0-based sub_plan position
	// (E24.1 / #1141 / ADR-041), consumed by the runner to route the
	// child onto its own sole-writer slice branch
	// fishhawk/run-<parent>/slice-<n>. omitempty drops a 0 value: only
	// decomposed children carry it, and the runner reads it only when
	// decomposed_from_run_id is set, defaulting to 0 — the correct
	// value for slice 0.
	SliceIndex           int    `json:"slice_index,omitempty"`
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
	// AgentVersionRange is the stage executor's spec-declared agent CLI
	// compatibility range (executor.agent_version, E32.13 / #1743): a semver
	// comparator range (e.g. ">=2.1 <2.2") the workflow was validated
	// against. Non-empty only when the stage's executor declares it. The
	// runner compares its resolved (#1769-probed) coding-agent CLI version
	// against this range BEFORE spawning the agent and fails the stage loudly
	// pre-spawn (category C) on an out-of-range version, degrading-and-
	// proceeding on an unprobeable one. Empty/omitted (the common case) means
	// no constraint — byte-identical to today.
	//
	// CROSS-MODULE WIRE CONTRACT: the json tag (`agent_version_range`) MUST
	// stay byte-identical to the runner's upload.FetchedPrompt.AgentVersionRange
	// decoder (runner/internal/upload/upload.go) — the same independent-struct-
	// by-tag convention as ImplementModel/MinRunnerVersion. A tag drift here
	// silently disables the runner's pre-spawn compatibility check.
	AgentVersionRange string `json:"agent_version_range,omitempty"`
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
	// BindingAssertions is the operator-declared binding-assertion list
	// (#1171) echoed on implement stages so the runner can decode and
	// evaluate each deterministic substring check against the committed
	// scope-only tree post-implement (slice 2). Empty/omitted when no
	// assertions were declared at approval time — byte-identical to today.
	// Resolved across the decomposition fan-out boundary so children inherit
	// the parent's declared assertions. The wire tags (type/path/literal)
	// match the runner's upload.BindingAssertion decoder.
	BindingAssertions []bindingAssertion `json:"binding_assertions,omitempty"`
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
	// FixupApplyPatches is the near-deterministic apply-list (#1165): one
	// reviewer-emitted unified diff per routed concern, in routing order.
	// Populated ONLY when Fixup is true AND every routed concern carries a
	// non-empty suggested_patch (the all-or-nothing eligibility gate). The
	// runner then attempts `git apply --3way` of each patch against the
	// already-checked-out PR branch and, on a clean apply that passes the
	// committed-tree verify gate, commits/pushes via the existing fixup_pushed
	// path WITHOUT spawning the agent. ANY apply failure, verify-gate failure,
	// or an absent field (a routed concern lacked a patch) sends the runner
	// down the unchanged agent fix-up path. Omitted on a normal implement
	// dispatch and on a non-eligible fix-up — byte-identical to today.
	FixupApplyPatches []fixupApplyPatch `json:"fixup_apply_patches,omitempty"`
	// ScopeExemptions is the operator's exempt_scope_files list (#1229) echoed
	// on a recovery run's implement stage so the runner's #1151 MissingScopeFiles
	// shortfall gate subtracts each operator-justified-unchanged declared path —
	// the inverse of add_scope_files (it subtracts from the gate, it does not
	// widen scope). Resolved from the run's OWN plan_reused_from audit entry, so
	// it is empty/omitted on every non-recovery run — byte-identical to today.
	//
	// CROSS-MODULE WIRE CONTRACT: the json tags (scope_exemptions/path/reason)
	// MUST stay byte-identical to the runner's upload.FetchedPrompt.ScopeExemptions
	// decoder (runner/internal/upload/upload.go), the counterpart of this field —
	// the same independent-struct-by-tag convention as BindingAssertions (#1171)
	// and add_scope_files (#824). A tag drift here breaks the runner gate silently.
	ScopeExemptions []scopeExemption `json:"scope_exemptions,omitempty"`
	// ImplementModel is the backend-resolved implement model id (#1013),
	// carried on an implement-stage prompt so the runner pins the agent spawn
	// to it (`--model <ImplementModel>`). Resolved through the
	// implement-model ladder (resolveImplementModelForRun): operator gate
	// decision > plan model_recommendation.implement_model > spec
	// executor.model > deployment default. EMPTY/omitted (the common case)
	// means no rung supplied a model, and the runner spawns the agent on the
	// adapter's built-in default exactly as today, byte-for-byte.
	//
	// CROSS-MODULE WIRE CONTRACT: the json tag (`implement_model`) MUST stay
	// byte-identical to the runner's upload.FetchedPrompt.ImplementModel
	// decoder (runner/internal/upload/upload.go) — the same independent-struct-
	// by-tag convention as ScopeExemptions/BindingAssertions. A tag drift here
	// silently drops the model and the runner falls back to today's spawn.
	ImplementModel string `json:"implement_model,omitempty"`
	// PlanModel is the backend-resolved plan model id (#1416), carried on a
	// plan-stage prompt so the runner pins the plan agent spawn to it
	// (`--model <PlanModel>`), reusing the same FetchedPrompt -> inv.Model seam
	// as ImplementModel. Resolved through the plan-model ladder
	// (resolvePlanModelForRun): spec executor.model (plan stage) > deployment
	// default, with the operator gate rung added by a sibling slice. EMPTY/
	// omitted (the common case) means no rung supplied a model, and the runner
	// spawns the plan agent on the adapter's built-in default exactly as today,
	// byte-for-byte.
	//
	// CROSS-MODULE WIRE CONTRACT: the json tag (`plan_model`) MUST stay
	// byte-identical to the runner's upload.FetchedPrompt.PlanModel decoder
	// (runner/internal/upload/upload.go) — the same independent-struct-by-tag
	// convention as ImplementModel. A tag drift here silently drops the model and
	// the runner falls back to today's spawn.
	PlanModel string `json:"plan_model,omitempty"`
	// EgressTargetHosts is the acceptance stage's FULL spec-declared egress
	// target-host list (the E31.4/#1532 grammar), served ONLY on acceptance
	// stages (E31.7 / #1535). The runner feeds it into the ADR-050 egress
	// proxy's allow-list before spawning the acceptance agent. Distinct from
	// the prompt TEXT's target-instance line, which renders only the first
	// host. Absent on every other stage type, and on an acceptance stage whose
	// spec declares no egress block — the proxy then admits only model + backend
	// hosts (fail-closed).
	//
	// CROSS-MODULE WIRE CONTRACT: the json tag (`egress_target_hosts`) MUST
	// stay byte-identical to the runner's upload.FetchedPrompt.EgressTargetHosts
	// decoder (runner/internal/upload/upload.go) — the same independent-struct-
	// by-tag convention as ImplementModel/ScopeExemptions. A tag drift here
	// silently empties the proxy allow-list and the agent cannot reach the
	// target instance.
	EgressTargetHosts []string `json:"egress_target_hosts,omitempty"`
	// AcceptanceCriteriaIDs is the approved plan's
	// verification.acceptance_criteria id list (the E31.1 join keys), served
	// ONLY on acceptance stages (E31.7 / #1535). The runner validates the
	// acceptance verdict's criteria[].id join keys against this set before
	// shipping, failing closed on an unknown id. Absent on every other stage
	// type, and when the approved plan carries no criteria.
	//
	// CROSS-MODULE WIRE CONTRACT: the json tag (`acceptance_criteria_ids`)
	// MUST stay byte-identical to the runner's
	// upload.FetchedPrompt.AcceptanceCriteriaIDs decoder
	// (runner/internal/upload/upload.go). A tag drift here silently disables
	// the runner's verdict join-key validation.
	AcceptanceCriteriaIDs []string `json:"acceptance_criteria_ids,omitempty"`
	// AcceptanceExpectedHeadSHA is the run's merge-candidate identity — the
	// newest head_sha across the run's reported-head ledger entries
	// (pull_request_opened / child_pushed / fixup_pushed, the same ADR-035
	// lineage source FixupExpectedHeadSHA resolves from) — served ONLY on
	// acceptance stages (E31.18 / #1569). The runner's pre-spawn
	// target-identity gate compares the declared target's /healthz git_sha
	// against it before spawning the acceptance agent, so acceptance
	// validates the merge candidate rather than whatever build answers at
	// the declared host. Empty/omitted when resolution fails (empty ledger
	// or read error, WARN-and-omit) — the runner then treats the target as
	// unverifiable and warns-and-proceeds rather than blocking the stage.
	//
	// CROSS-MODULE WIRE CONTRACT: the json tag
	// (`acceptance_expected_head_sha`) MUST stay byte-identical to the
	// runner's upload.FetchedPrompt.AcceptanceExpectedHeadSHA decoder
	// (runner/internal/upload/upload.go) — the same independent-struct-by-tag
	// convention as EgressTargetHosts above. A tag drift here silently drops
	// the expectation and the runner's identity gate degrades to
	// unverifiable-warn on every dispatch.
	AcceptanceExpectedHeadSHA string `json:"acceptance_expected_head_sha,omitempty"`
}

// scopeExemption is one operator scope exemption: a DECLARED scope.files path
// marked operator-justified-unchanged so the runner's #1151 shortfall gate
// subtracts it (#1229). {path, reason}, both required at the recover endpoint.
//
// CROSS-MODULE WIRE CONTRACT: the json tags (path/reason) MUST stay
// byte-identical to the runner's upload.ScopeExemption decoder
// (runner/internal/upload/upload.go), the counterpart of this struct. Same
// independent-struct-by-tag convention as bindingAssertion (#1171) and the
// add_scope_files (#824) cross-module pair. This single server-package type
// is shared by recoverRunRequest.ExemptScopeFiles (the request + validation),
// the plan_reused_from exempted_paths persistence, and this response field.
type scopeExemption struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// fixupApplyPatch is one entry in promptResponse.FixupApplyPatches: a single
// routed concern's reviewer-emitted unified diff (#1165). The patch carries its
// own file paths in the diff headers, so no separate path field is needed; the
// runner extracts the touched paths for provenance. The wire tag (patch) is
// byte-identical to the runner's upload.FixupApplyPatch decoder.
type fixupApplyPatch struct {
	Patch string `json:"patch"`
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
// branch is that child's per-slice sole-writer branch
// `fishhawk/run-<shortID(parentRunID)>/slice-<n>` (ADR-041 / #1141): after
// ADR-041 a child's work lives on its slice branch, NOT the pre-ADR-041 shared
// `fishhawk/run-<parent>` bare prefix — which is orphaned from both the slice
// work and the #1243 consolidated PR head, and path-NESTS with the slice refs
// (the directory/file ref-conflict #1243 eliminated for fan-in, #1246). The
// slice index comes from the child's SliceIndex, defaulting to 0 when nil to
// match the runner's slice-0 default (runSliceIndex, runner main.go). We call
// the exported orchestrator.SliceBranch rather than re-hardcoding the literal
// — the single-source-of-truth discipline #1245 introduced for the
// consolidated name. Otherwise the branch is the per-stage branch
// `fishhawk/run-<shortID(runID)>/stage-<shortID(stageID)>`.
func fixupBranchFor(runRow *run.Run, stage *run.Stage) string {
	if runRow.DecomposedFrom != nil {
		idx := 0
		if runRow.SliceIndex != nil {
			idx = *runRow.SliceIndex
		}
		return orchestrator.SliceBranch(*runRow.DecomposedFrom, idx)
	}
	return fmt.Sprintf("fishhawk/run-%s/stage-%s", shortID(runRow.ID), shortID(stage.ID))
}

// fixupBranchForRun resolves the fix-up branch for a run, extending
// fixupBranchFor to handle the decomposed-PARENT case (#1063). A decomposed
// parent has DecomposedFrom == nil, so fixupBranchFor would return a per-stage
// branch — but a parent fix-up's commit must land on the consolidated PR head,
// the shared branch `fishhawk/run-<shortID(runID)>-consolidated`,
// byte-matching orchestrator.ConsolidatedBranch. We delegate to that exported
// helper rather than re-hardcoding the literal: a second, duplicated
// reconstruction here returned the pre-#1243 `fishhawk/run-<short>` form and
// diverged from the renamed consolidated branch, orphaning the parent fix-up
// commit (#1245). When the run is a parent (DecomposedFrom == nil) WITH minted
// children, return that consolidated head; otherwise delegate to
// fixupBranchFor, which lands a decomposed child on its own per-slice branch
// (#1246) and an ordinary run on its per-stage branch. On a probe error, fall
// back to fixupBranchFor — never widen an ordinary run onto a shared branch.
func (s *Server) fixupBranchForRun(ctx context.Context, runRow *run.Run, stage *run.Stage) string {
	if runRow.DecomposedFrom == nil && s.hasDecomposedChildren(ctx, runRow.ID) {
		return orchestrator.ConsolidatedBranch(runRow.ID)
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

// isRepoRelativePath reports whether p is a clean repo-relative path: not
// absolute (no leading '/') and free of any '..' parent-traversal segment. It
// mirrors validateAllowCreate's repo-relative contract (backend/internal/server/
// fixup.go) so the binding assertion validation (binding_assertions.go) and the
// recover-stage shortfall handling (recover.go) agree with the #1151 shortfall
// gate (runner MissingScopeFiles) on what token can name a real scope.files
// entry — a git diff-tree file set is always repo-relative, so an absolute or
// traversal token can never match a committed path (#1155). The #730 approve-
// reason prose fold that originally shared this helper was removed in #1225.
func isRepoRelativePath(p string) bool {
	return !strings.HasPrefix(p, "/") && !strings.Contains(p, "..")
}

// mergeStructuredScopeFiles folds the authoritative add_scope_files paths a
// reviewer named at approval time (#824) into the implement stage's effective
// scope set. It is the SOLE source of approve-time scope.files additions
// (the #730 approve-reason prose fold was removed in #1225). Because it takes
// the structured slice directly (no regex scrape), it stages directories
// (trailing slash), extensionless/repo-root files, and described-not-spelled
// paths a prose scrape could never reach. It shares the same empty-scope guard:
// an otherwise-empty scope stays empty so the runner's `git add -A` fallback
// isn't narrowed.
func (s *Server) mergeStructuredScopeFiles(ctx context.Context, scopeFiles []scopeFile, paths []string) []scopeFile {
	if len(scopeFiles) == 0 || len(paths) == 0 {
		return scopeFiles
	}
	return s.foldScopePaths(ctx, scopeFiles, paths, "approval-add-scope-files")
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
	// Guard the unconfigured repo here (not only in mergeApprovedScopeAmendments)
	// so effectiveFixupScope can call this directly on a server with no
	// ScopeAmendmentRepo wired without a nil deref (#1162/#1314).
	if s.cfg.ScopeAmendmentRepo == nil {
		return nil
	}
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
// the shared body behind mergeStructuredScopeFiles (#824 structured fold),
// mergeApprovedScopeAmendments (#961), and effectiveFixupScope (#1162/#1314).
// Callers own the empty-scope and empty-paths guards. (The #730 approve-reason
// prose fold that also shared this body was removed in #1225.)
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

// subtractScopePaths is the inverse of foldScopePaths (#1726): it removes every
// scope entry whose .Path is in paths (compare-by-Path, the same semantics the
// fold uses) and info-logs the removals. It is the prompt-side half of a
// gate-time remove_scope_files edit — the runner reads cfg.scopeFiles solely
// from the prompt-response ScopeFiles, so subtracting here makes every runner
// gate (created-out-of-scope, commit-in-scope, category-B) honor the removal
// with no runner change. Empty paths is a no-op.
//
// A subtraction that would empty a non-empty scope is refused at the plan gate
// (checkRemoveScopeFiles). This helper reasserts that non-empty invariant as
// defense-in-depth (#1726): when the gate's presence/would-empty checks were
// skipped in a fail-open window (effectiveScopePathSet returned ok=false), an
// operator removal could otherwise empty the scope here and silently re-enable
// the runner's `git add -A` fallback, disabling enforcement. If the subtraction
// would drop every entry of a non-empty scope, the removal is refused (the
// original scope is returned unchanged, WARN-logged) — preserving enforcement
// is strictly safer than an unguarded empty. In normal operation the gate
// already prevents this, so the guard never fires.
func (s *Server) subtractScopePaths(ctx context.Context, scopeFiles []scopeFile, paths []string, source string) []scopeFile {
	if len(paths) == 0 || len(scopeFiles) == 0 {
		return scopeFiles
	}
	remove := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		remove[p] = struct{}{}
	}
	out := make([]scopeFile, 0, len(scopeFiles))
	removed := make([]string, 0, len(paths))
	for _, f := range scopeFiles {
		if _, drop := remove[f.Path]; drop {
			removed = append(removed, f.Path)
			continue
		}
		out = append(out, f)
	}
	// Defense-in-depth: refuse a subtraction that would empty a non-empty
	// scope (the gate's would-empty check may have been skipped in a fail-open
	// window). An empty scope disables enforcement; keeping the full scope is
	// the safer failure mode.
	if len(out) == 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"prompt: refusing scope subtraction that would empty a non-empty scope; retaining full scope",
			slog.String("source", source),
			slog.Int("scope_count", len(scopeFiles)),
			slog.String("paths", strings.Join(removed, ",")),
		)
		return scopeFiles
	}
	if len(removed) > 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"prompt: subtracted files from implement scope",
			slog.String("source", source),
			slog.Int("count", len(removed)),
			slog.String("paths", strings.Join(removed, ",")),
		)
	}
	return out
}

// effectiveFixupScope computes the effective scope for an implement-review
// fix-up dispatch. It RETAINS the FULL inherited plan scope (#1314), reversing
// the #1162 concern-surface narrowing that rebuilt the fix-up scope from an
// empty slice using only concern-scraped paths. That narrowing silently
// collapsed the committed blast radius to ~1 file whenever a routed concern
// carried only prose (planreview.Concern has no structured file field, so the
// scrape was usually empty) AND a mid-stage scope amendment was present: the
// narrowed set was non-empty (= just the amendment target), so the empty-narrow
// fail-safe never fired, the entire approved plan scope was dropped, and the
// agent's legitimate in-plan edits were drift-excluded from the commit — a
// silent no-op fix-up that left PR HEAD unchanged and re-opened every routed
// concern (#1314).
//
// Accepted tradeoff (#1314): because the full plan scope is always retained, a
// fix-up may now ship an edit to ANY approved-plan-scope file even if the
// reviewer did not name that file. That is fine — the plan scope was approved,
// so edits within it are authorized; it is strictly better than the silent
// no-op + burned pass + wedged run the #1162 narrowing produced.
//
// The retained surface is the inherited planScope folded with:
//   - the operator-declared net-new files (allow_create, #823), so the runner's
//     #818 created-out-of-scope gate stages them rather than failing category-B,
//   - the stage's operator-approved mid-pass scope amendments (#961 — already
//     merged into planScope upstream by mergeApprovedScopeAmendments at the call
//     sites, so this fold is defensive/idempotent),
//   - the coupled *_test.go stem-sibling of each resulting source file (#1214),
//     mirroring resolveDecomposedScopeFiles (#1083): a fix-up whose fix touches
//     only a production file (e.g. main.go) still lands the agent's sibling test
//     (main_test.go) in the same commit instead of having it stripped as
//     scope_drift and pushed to a follow-up.
//
// A created file NOT declared via allow_create (or named by an approved
// amendment) is still absent from the effective scope, so the #818
// silent-strip hole stays closed — an undeclared create still fails category-B
// at the runner's created-out-of-scope gate (#818/#825).
//
// An empty plan scope STAYS empty (a plan_missing_for_implement fix-up keeps the
// runner's git add -A fallback; we do not synthesize a scope).
func (s *Server) effectiveFixupScope(ctx context.Context, planScope []scopeFile, allowCreate, amendmentPaths []string) []scopeFile {
	if len(planScope) == 0 {
		// Empty plan scope stays empty (keeps the runner's git add -A fallback).
		return planScope
	}
	// Retain the FULL inherited plan scope and fold in the operator-declared
	// creates and approved amendments. The amendment fold is idempotent — the
	// call sites already merged approved amendments into planScope via
	// mergeApprovedScopeAmendments — but is kept defensively so this helper is
	// correct regardless of caller order.
	scoped := s.foldScopePaths(ctx, planScope, allowCreate, "fixup-allow-create")
	scoped = s.foldScopePaths(ctx, scoped, amendmentPaths, "scope-amendment")
	// Fold the coupled stem-sibling *_test.go for each source file into the scope
	// so a fix-up's "fix + test" pass lands the sibling test in the same commit
	// instead of having it stripped as scope_drift (#1214). Mirrors
	// resolveDecomposedScopeFiles (#1083) for the identical "fix and test must
	// land together" reason. foldScopePaths dedups an already-scoped sibling and
	// defaults net-new ones to operation=modify.
	scoped = s.foldScopePaths(ctx, scoped, coupledTestSiblings(scoped), "fixup-coupled-test-sibling")
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
		"prompt: retained full plan scope for fix-up pass",
		slog.Int("scoped_count", len(scoped)),
		slog.Int("plan_scope_count", len(planScope)),
	)
	return scoped
}

// issueGetter is the slice of githubclient.Client the prompt
// handler consumes. Defining the interface in the server package
// lets tests substitute a stub without spinning up an httptest
// fake of api.github.com — *githubclient.Client satisfies it
// in production.
type issueGetter interface {
	GetIssue(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, number int) (*githubclient.Issue, error)
	ListIssueComments(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, number int) ([]githubclient.FetchedIssueComment, error)
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

	// Liveness flip (#1924): a valid signature proves a runner holding
	// THIS run's signing key is fetching THIS stage's prompt, and that
	// fetch lands within seconds of spawn — so it is the earliest
	// authenticated proof a runner is alive. Flip a 'dispatched' stage to
	// 'running' now, before prompt construction, giving runner_kind:local
	// the real-time dispatched→running signal it otherwise lacks until
	// trace upload at settle. Best-effort: a flip failure logs and never
	// blocks the prompt response.
	s.markStageRunningOnPromptFetch(r.Context(), stage)

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
	var bindingAssertions []bindingAssertion
	var scopeExemptions []scopeExemption
	var fixup bool
	var fixupBranch string
	var fixupExpectedHeadSHA string
	var fixupApplyPatches []fixupApplyPatch
	if stage.Type == run.StageTypeImplement {
		// Run/stage ids for the implement prompt's scope self-exempt sidecar
		// path (#1153). Populated only on the implement path; plan/review
		// triggers leave them empty so buildImplement omits the section.
		trigger.ImplementRunID = runRow.ID.String()
		trigger.ImplementStageID = stage.ID.String()
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
		// Decomposition fan-out (#676, fail-loud #1721): a genuine fan-out slice
		// child carries DecomposedFrom and narrows its scope to its own sub-plan
		// slice, linked by the SliceIndex the orchestrator stamps on every
		// fan-out child. A fan-out child whose slice scope cannot be resolved —
		// no linked sub-plan (INCLUDING a loaded plan whose Decomposition is nil,
		// via matchDecomposedSubPlan's nil-plan degrade), a NIL SliceIndex against
		// a decomposed plan, an unmatched/out-of-range index, or a matched slice
		// with empty scope — FAILS CLOSED (409 decomposed_scope_unresolved) rather
		// than silently inheriting the parent's full scope — the #1669 regression
		// that reopened when campaign-minted parents fanned out children with a
		// nil IssueContext.
		//
		// The guard fires for a decomposed child that either claims a slice
		// (SliceIndex != nil) OR resolves to a plan that actually carries a
		// Decomposition to narrow to. It deliberately does NOT require
		// approvedPlan.Decomposition to be non-nil once a SliceIndex is present
		// (a slice child whose loaded plan carries no decomposition has nothing
		// legitimate to narrow to → still fails closed). But a decomposed child
		// with a NIL SliceIndex AND a resolved plan with no Decomposition is not a
		// fan-out slice at all — it is a standalone child with its own top-level
		// plan — so it keeps that plan's scope rather than failing closed. A
		// missing plan is handled above (emitPlanMissingForImplement).
		// Non-decomposed runs are untouched.
		if runRow.DecomposedFrom != nil && approvedPlan != nil &&
			(runRow.SliceIndex != nil || approvedPlan.Decomposition != nil) {
			childScope, childConstraint, scopeErr := s.requireDecomposedScope(r.Context(), runRow, approvedPlan)
			if scopeErr != nil {
				s.writeDecomposedScopeUnresolved(w, r, scopeErr)
				return
			}
			scopeFiles = childScope
			trigger.ScopeConstraint = childConstraint
		}
		trigger.ApprovalConditions = s.resolveApprovalConditions(r.Context(), runRow)
		// Part D (#1229): a recovery run's resume_run reason rides the existing
		// #558 binding-conditions channel so the operator's steer at recovery
		// reaches the agent. No-op (byte-identical conditions) on every
		// non-recovery run — loadRecoveryResumeReason returns nil without a
		// plan_reused_from entry.
		trigger.ApprovalConditions = appendRecoveryResumeReason(
			trigger.ApprovalConditions, s.loadRecoveryResumeReason(r.Context(), runRow.ID))
		// Binding-assertion declaration (#1171): echo the operator's declared
		// assertions on the implement prompt-response so the runner can decode
		// and evaluate them post-implement (slice 2). Only when an approved
		// plan exists — a plan_missing_for_implement fallback carries no
		// declaration. Resolved across the decomposition fan-out boundary so
		// children inherit the parent's assertions exactly as they inherit
		// add_scope_files / ApprovalConditions.
		if approvedPlan != nil {
			bindingAssertions = s.resolveApprovalBindingAssertions(r.Context(), runRow)
			// Operator scope exemptions (#1229): echo the recovery run's
			// exempt_scope_files so the runner's #1151 shortfall gate subtracts
			// each operator-justified-unchanged declared path. Read from the
			// run's OWN plan_reused_from entry — nil on every non-recovery run.
			scopeExemptions = s.resolveRecoveryScopeExemptions(r.Context(), runRow.ID)
		}
		// Fold the authoritative add_scope_files paths a reviewer named at
		// approval time into the effective scope set (#824). This structured
		// param is the SOLE source of approve-time scope.files additions: it
		// stages directories, extensionless/repo-root files, and
		// described-not-spelled paths a regex scrape cannot reach. The #730
		// approve-reason PROSE fold was removed (#1225) — repo-relative tokens
		// scraped out of the operator's free-text reason/comment no longer
		// mutate scope, so an explanatory path in the reason can never be
		// folded as an unsatisfiable required-to-touch entry (the E24.4/#1144
		// category-B burn). ApprovalConditions are still injected as #558
		// binding instructions; only the scope-mutation consumption is gone.
		// No-op on an empty scope (keeps the runner's git add -A fallback).
		scopeFiles = s.mergeStructuredScopeFiles(r.Context(), scopeFiles, s.resolveApprovalAddScopeFiles(r.Context(), runRow))
		// Surface the operator's approval-time add_scope_files additions in the
		// implement prompt TEXT (#1406). The fold above already puts these paths
		// in the ENFORCED scope, but writeApprovedPlan renders only the immutable
		// plan artifact's scope.files, so a defensive agent reads the shown scope,
		// concludes the added paths are out of scope, and files a redundant
		// mid-stage amendment for paths already folded (run 6434aae9). Derive the
		// agent-shown set from the SAME single fold source the enforced scope and
		// the review prompt (trace.go:2680) already use — amendedScopeFilesForReview
		// — so the three stay in lockstep. It returns nil for a nil/empty-scope
		// plan and excludes paths already in raw scope.files, so a run with no
		// additions leaves trigger.AmendedScopeFiles nil and the prompt
		// byte-identical (audit prompt-hash replay stability).
		trigger.AmendedScopeFiles = s.amendedScopeFilesForReview(r.Context(), runRow, approvedPlan)
		// Fold the operator-approved mid-stage scope amendment paths (#961)
		// into the effective scope so a stage restart or fix-up prompt
		// carries the amended scope. No-op on an empty scope (keeps the
		// runner's git add -A fallback).
		scopeFiles = s.mergeApprovedScopeAmendments(r.Context(), scopeFiles, runRow.ID, stage.ID)
		// Subtract the operator's gate-time remove_scope_files (#1726) AFTER
		// the add fold, the approved-amendment fold, and (for decomposed
		// children) the per-slice narrowing above — so the removal covers
		// top-level, decomposed per-slice, and fix-up scopes uniformly. The
		// runner reads cfg.scopeFiles solely from this ScopeFiles, so a removed
		// path is honored by every runner gate with no runner change. Resolved
		// across the decomposition fan-out boundary so removals reach
		// implement-only decomposed and recovery children. No-op on an empty
		// removal set (audit prompt-hash replay stability).
		removedScopeFiles := s.resolveApprovalRemoveScopeFiles(r.Context(), runRow)
		scopeFiles = s.subtractScopePaths(r.Context(), scopeFiles, removedScopeFiles, "approval-remove-scope-files")
		trigger.RemovedScopeFiles = removedScopeFiles
		// Fix-up pass (#762): when the operator routed implement-review
		// concerns back to this stage, deliver them as binding instructions
		// (reusing #558's framing) and RETAIN the FULL approved plan scope as the
		// effective fix-up scope (#1314) — the inherited plan scope.files, plus
		// the operator-declared net-new files (#823), the stage's approved
		// mid-pass scope amendments (#961), and coupled test siblings (#1214).
		// This reverses the #1162 concern-surface narrowing, which silently
		// collapsed the scope to ~1 file and drift-excluded the agent's in-plan
		// edits whenever a concern carried only prose and an amendment was
		// present. An undeclared create still fails category-B at the runner's
		// #818 gate. No-op for a normal (non-fix-up) implement dispatch.
		if rendered := s.resolveFixupConcerns(r.Context(), runRow.ID, stage.ID); len(rendered) > 0 {
			trigger.FixupConcerns = rendered
			scopeFiles = s.effectiveFixupScope(r.Context(), scopeFiles,
				s.resolveFixupAllowCreate(r.Context(), runRow.ID, stage.ID),
				s.resolveApprovedScopeAmendments(r.Context(), runRow.ID, stage.ID))
			// Inject the prior implement commit's diff (#1163) so the slim
			// fix-up prompt shows the agent the change it is amending. Sourced
			// from the stage's newest redacted trace bundle (repo code only).
			trigger.FixupPriorDiff, trigger.FixupPriorDiffFiles = s.resolveFixupPriorDiff(r.Context(), runRow.ID, stage.ID)
			// Emit the fix-up routing flag (#784): point the runner at the
			// stage's existing PR branch so it takes the RebaseFromRemote
			// same-branch path instead of `checkout -b <existing branch>`.
			fixup = true
			fixupBranch = s.fixupBranchForRun(r.Context(), runRow, stage)
			// Advertise the run's recorded head so the runner can verify the
			// fetched PR-branch tip before invoking the agent (#967).
			fixupExpectedHeadSHA = s.resolveFixupExpectedHeadSHA(r.Context(), runRow.ID, stage.ID)
			// Near-deterministic apply-list (#1165): serve the routed concerns'
			// suggested_patches ONLY when every routed concern carries one, so the
			// runner can git-apply them instead of spawning the agent. nil (the
			// agent path) when any concern lacks a patch.
			fixupApplyPatches = s.resolveFixupApplyPatches(r.Context(), runRow.ID, stage.ID)
		}
	}

	// Acceptance stage (E31.6 / #1534): the independent validator judges the
	// running instance against the approved plan's acceptance criteria. Feed the
	// approved plan (the criteria source) and the target-instance URL seam.
	// Unlike the implement branch, do NOT attach ScopeFiles / BindingAssertions
	// / diff-adjacent fields — the diff is deliberately withheld for
	// independence (ADR-049 decision #4). resolveAcceptanceTargetURL returns the
	// empty string until E31.4/#1532's egress-allowance grammar lands, and
	// buildAcceptance then renders an explicit not-declared line.
	if stage.Type == run.StageTypeAcceptance {
		// Run/stage ids for the acceptance prompt's run/stage-keyed verdict
		// file-fallback path (#1780). Populated only on the acceptance path so
		// acceptanceVerdictPathForTrigger names the SAME keyed path the runner
		// reads first; other prompts leave them empty and fall back to the
		// legacy fixed path, staying byte-identical.
		trigger.AcceptanceRunID = runRow.ID.String()
		trigger.AcceptanceStageID = stage.ID.String()
		approvedPlan, err := s.loadApprovedPlanForRun(r.Context(), runRow.ID)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"load approved plan failed", map[string]any{"error": err.Error()})
			return
		}
		trigger.ApprovedPlan = approvedPlan
		trigger.TargetInstanceURL = s.resolveAcceptanceTargetURL(r.Context(), runRow)
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
		// Surface-coupling sibling map (#763/#1797): thread the static
		// surface-sweep registry into the plan prompt's Coupling-discovery
		// checklist so the planner scopes (or justifies) lockstep siblings at
		// first emission instead of the plan gate burning a review round on a
		// deterministic miss. surfacePatterns is the single source of truth; the
		// accessor is a pure projection. Set on BOTH prompt handlers so the
		// signed prompt and the render preview stay byte-identical.
		trigger.SurfaceCouplingPatterns = surfaceCouplingPatternsForPrompt()
		if runRow.TriggerRef != nil {
			trigger.PriorRejectionFeedback = s.loadPriorRejectionFeedback(r.Context(), runRow.Repo, *runRow.TriggerRef, runRow.ID)
		}
		trigger.PriorSchemaValidationError = s.loadPriorSchemaValidationError(r.Context(), runRow.ID)
		// Clarification answers (#1088): on resume after an awaiting_input
		// park, the operator's answers to the parked clarification_request
		// ride the binding-conditions channel (t.ApprovalConditions) so the
		// existing "Clarification answers (binding ...)" section renders. A
		// dedicated clarification_answered channel, NOT the implement-stage
		// approval_submitted one — a parked plan stage is not yet approved.
		// nil on a normal first-pass plan (no answers recorded).
		trigger.ApprovalConditions = s.loadClarificationAnswers(r.Context(), runRow.ID)
		// Revision constraint (#1099): on a plan-gate `revise` re-open, the
		// operator's binding design constraint rides a DEDICATED plan_revised
		// channel (not the approval/clarification one) and the prior plan is
		// carried as the revision base. First-pass plan dispatch records no
		// plan_revised entry, so RevisionConstraint stays nil and the base is
		// not loaded — normal plans are byte-unchanged.
		trigger.RevisionConstraint = s.loadRevisionConstraint(r.Context(), runRow.ID)
		if trigger.RevisionConstraint != nil {
			trigger.RevisionBasePlan = s.loadRevisionBasePlan(r.Context(), runRow.ID)
		}
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
		AgentVersionRange:    s.resolveExecutorAgentVersionRange(r.Context(), runRow, stage.Type),
		AgentSelfRetry:       s.resolveAgentSelfRetryForStage(r.Context(), runRow, stage.Type),
		MaxRetriesSnapshot:   runRow.MaxRetriesSnapshot,
		RetryAttempt:         runRow.RetryAttempt,
		ScopeFiles:           scopeFiles,
		BindingAssertions:    bindingAssertions,
		ScopeExemptions:      scopeExemptions,
		CommitAuthorName:     commitAuthorName,
		CommitAuthorEmail:    commitAuthorEmail,
		Fixup:                fixup,
		FixupBranch:          fixupBranch,
		FixupExpectedHeadSHA: fixupExpectedHeadSHA,
		FixupApplyPatches:    fixupApplyPatches,
	}
	if runRow.DecomposedFrom != nil {
		resp.DecomposedFromRunID = runRow.DecomposedFrom.String()
		if runRow.SliceIndex != nil {
			resp.SliceIndex = *runRow.SliceIndex
		}
	}
	if stage.Type == run.StageTypeImplement {
		rm := s.resolveImplementDispatchModel(r.Context(), runRow, stage, fixup)
		resp.ImplementModel = rm.Value
		s.logModelResolution(r.Context(), runRow.ID, rm)
	}
	// Plan-stage model routing (#1416): resolve the plan-model ladder and carry
	// the resolved value to the runner, which pins it onto the plan agent spawn
	// (parallel to ImplementModel). An empty resolution (no rung supplied a
	// model) leaves PlanModel empty so the spawn is byte-identical to today.
	if stage.Type == run.StageTypePlan {
		rm := s.resolvePlanModelForRun(r.Context(), runRow)
		resp.PlanModel = rm.Value
		s.logModelResolution(r.Context(), runRow.ID, rm)
	}
	// Acceptance-stage containment inputs (E31.7 / #1535): the FULL egress
	// target-host list (the runner's ADR-050 proxy allow-list) and the approved
	// plan's criterion ids (the runner's verdict join-key validation set).
	// trigger.ApprovedPlan was loaded by the acceptance branch above. Plus the
	// merge-candidate identity (E31.18 / #1569) the runner's pre-spawn
	// target-identity gate verifies the declared target against. All
	// omitempty, so every non-acceptance response is byte-identical to today.
	if stage.Type == run.StageTypeAcceptance {
		resp.EgressTargetHosts = s.resolveAcceptanceEgressTargetHosts(r.Context(), runRow)
		resp.AcceptanceCriteriaIDs = acceptanceCriteriaIDsFromPlan(trigger.ApprovedPlan)
		resp.AcceptanceExpectedHeadSHA = s.resolveAcceptanceExpectedHeadSHA(r.Context(), runRow.ID, stage.ID)
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// markStageRunningOnPromptFetch gives runner_kind:local a real-time
// dispatched→running liveness signal (#1924). Today nothing flips a local
// stage into 'running' until trace upload at settle (trace.go
// advanceStageAfterTrace), so every local implement stage sits in
// 'dispatched' for its entire runtime and fishhawk_drive_run's
// dispatched_stale detector deterministically false-stales any healthy
// implement past its liveness threshold. The signed prompt fetch that
// drives this call is authenticated by the per-run signing key with a
// stage-bound message and lands within seconds of spawn, so it is proof a
// runner for THIS stage is alive.
//
// The flip is best-effort and must never unwind the load-bearing prompt
// response: every failure path logs at warn and returns. It fires ONLY on
// an observed 'dispatched' OR 'awaiting_host_dispatch' state:
//
//   - 'dispatched' walks the single dispatched → running edge (the common
//     case: the MCP host-dispatch marker already flipped the park).
//   - 'awaiting_host_dispatch' (#1912) is the version-skew / raced-marker
//     defensive walk: a runner that spawned against a still-parked stage —
//     because an older MCP binary never called the host-dispatch marker, or
//     the marker call lost a race — presents this state. The authenticated
//     prompt fetch is proof a runner for THIS stage is alive, so converge
//     the state by walking awaiting_host_dispatch → dispatched → running.
//   - 'pending' is left untouched to preserve the #1030 local first-stage
//     semantics (advanceStageAfterTrace owns the pending→dispatched walk).
//   - a 'running' replay re-fetch is a no-op by the state guard.
//   - every other runnable state the prompt endpoint admits
//     (awaiting_input, awaiting_scope_decision) is left untouched, so a
//     legitimately-parked stage is never advanced.
//
// It anchors each step via the run.StageCASTransitioner capability
// (mirroring run.failStageCAS) so a concurrent advance — a scope park, a
// reap, any other writer — refuses atomically with StageStateChangedError
// instead of being stomped; the park's legal →running edge can never be
// collapsed. Repos without the capability (in-memory fakes) fall back to
// plain TransitionStage, still guarded on the observed state. As
// a side effect the repo sets started_at at real start time on this first
// →running transition (postgres.go), improving duration accuracy.
//
// RESIDUAL DETECTION GAP (#1924, accepted tradeoff): the flip is plan-mandated
// to fire after verifyPromptSignature succeeds and BEFORE prompt construction,
// so a handler failure AFTER a successful flip — the 501 unsupported-stage-type
// path, or a load-approved-plan / decomposed-scope error — leaves the stage
// persisted as 'running' even though the runner never executes. Such a stage is
// outside the dispatched_stale detector's coverage (it only examines
// 'dispatched'), converting what was previously a correctly-detected stale into
// an undetected wedged-'running' stage. The blast radius is bounded by the
// runner's own failure/reap paths and operator visibility, and is strictly
// narrower than the deterministic false-stale class this flip removes, so the
// pre-construction ordering stands. The
// TestGetStagePrompt_LivenessFlip_FlipsThenBuildFails test pins the
// flip-succeeded-then-construction-failed ordering.
func (s *Server) markStageRunningOnPromptFetch(ctx context.Context, stage *run.Stage) {
	if stage.State != run.StageStateDispatched && stage.State != run.StageStateAwaitingHostDispatch {
		return
	}
	cas, hasCAS := s.cfg.RunRepo.(run.StageCASTransitioner)

	// #1912 defensive first step: converge a still-parked stage to dispatched
	// before the dispatched → running flip. Anchored on awaiting_host_dispatch
	// so a concurrent cancel/reap refuses atomically rather than being stomped.
	if stage.State == run.StageStateAwaitingHostDispatch {
		var err error
		if hasCAS {
			_, err = cas.TransitionStageFrom(ctx, stage.ID,
				run.StageStateAwaitingHostDispatch, run.StageStateDispatched, nil)
		} else {
			_, err = s.cfg.RunRepo.TransitionStage(ctx, stage.ID, run.StageStateDispatched, nil)
		}
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"prompt-fetch awaiting_host_dispatch→dispatched liveness flip failed (advisory; prompt still served)",
				slog.String("run_id", stage.RunID.String()),
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", err.Error()))
			return
		}
	}

	var err error
	if hasCAS {
		_, err = cas.TransitionStageFrom(ctx, stage.ID,
			run.StageStateDispatched, run.StageStateRunning, nil)
	} else {
		_, err = s.cfg.RunRepo.TransitionStage(ctx, stage.ID, run.StageStateRunning, nil)
	}
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"prompt-fetch dispatched→running liveness flip failed (advisory; prompt still served)",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
	}
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
	var bindingAssertions []bindingAssertion
	var scopeExemptions []scopeExemption
	var fixup bool
	var fixupBranch string
	var fixupExpectedHeadSHA string
	var fixupApplyPatches []fixupApplyPatch
	if stage.Type == run.StageTypeImplement {
		// Run/stage ids for the implement prompt's scope self-exempt sidecar
		// path (#1153). Populated only on the implement path; plan/review
		// triggers leave them empty so buildImplement omits the section.
		trigger.ImplementRunID = runRow.ID.String()
		trigger.ImplementStageID = stage.ID.String()
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
		// Decomposition fan-out (#676, fail-loud #1721): a genuine fan-out slice
		// child carries DecomposedFrom and narrows its scope to its own sub-plan
		// slice, linked by the SliceIndex the orchestrator stamps on every
		// fan-out child. A fan-out child whose slice scope cannot be resolved —
		// no linked sub-plan (INCLUDING a loaded plan whose Decomposition is nil,
		// via matchDecomposedSubPlan's nil-plan degrade), a NIL SliceIndex against
		// a decomposed plan, an unmatched/out-of-range index, or a matched slice
		// with empty scope — FAILS CLOSED (409 decomposed_scope_unresolved) rather
		// than silently inheriting the parent's full scope — the #1669 regression
		// that reopened when campaign-minted parents fanned out children with a
		// nil IssueContext.
		//
		// The guard fires for a decomposed child that either claims a slice
		// (SliceIndex != nil) OR resolves to a plan that actually carries a
		// Decomposition to narrow to. It deliberately does NOT require
		// approvedPlan.Decomposition to be non-nil once a SliceIndex is present
		// (a slice child whose loaded plan carries no decomposition has nothing
		// legitimate to narrow to → still fails closed). But a decomposed child
		// with a NIL SliceIndex AND a resolved plan with no Decomposition is not a
		// fan-out slice at all — it is a standalone child with its own top-level
		// plan — so it keeps that plan's scope rather than failing closed. A
		// missing plan is handled above (emitPlanMissingForImplement).
		// Non-decomposed runs are untouched.
		if runRow.DecomposedFrom != nil && approvedPlan != nil &&
			(runRow.SliceIndex != nil || approvedPlan.Decomposition != nil) {
			childScope, childConstraint, scopeErr := s.requireDecomposedScope(r.Context(), runRow, approvedPlan)
			if scopeErr != nil {
				s.writeDecomposedScopeUnresolved(w, r, scopeErr)
				return
			}
			scopeFiles = childScope
			trigger.ScopeConstraint = childConstraint
		}
		trigger.ApprovalConditions = s.resolveApprovalConditions(r.Context(), runRow)
		// Part D (#1229): a recovery run's resume_run reason rides the existing
		// #558 binding-conditions channel so the operator's steer at recovery
		// reaches the agent. No-op (byte-identical conditions) on every
		// non-recovery run — loadRecoveryResumeReason returns nil without a
		// plan_reused_from entry.
		trigger.ApprovalConditions = appendRecoveryResumeReason(
			trigger.ApprovalConditions, s.loadRecoveryResumeReason(r.Context(), runRow.ID))
		// Binding-assertion declaration (#1171): echo the operator's declared
		// assertions on the implement prompt-response so the runner can decode
		// and evaluate them post-implement (slice 2). Only when an approved
		// plan exists — a plan_missing_for_implement fallback carries no
		// declaration. Resolved across the decomposition fan-out boundary so
		// children inherit the parent's assertions exactly as they inherit
		// add_scope_files / ApprovalConditions.
		if approvedPlan != nil {
			bindingAssertions = s.resolveApprovalBindingAssertions(r.Context(), runRow)
			// Operator scope exemptions (#1229): echo the recovery run's
			// exempt_scope_files so the runner's #1151 shortfall gate subtracts
			// each operator-justified-unchanged declared path. Read from the
			// run's OWN plan_reused_from entry — nil on every non-recovery run.
			scopeExemptions = s.resolveRecoveryScopeExemptions(r.Context(), runRow.ID)
		}
		// Fold the authoritative add_scope_files paths a reviewer named at
		// approval time into the effective scope set (#824). This structured
		// param is the SOLE source of approve-time scope.files additions, the
		// same derivation as the dispatch path so the rendered view matches.
		// The #730 approve-reason PROSE fold was removed (#1225) — repo-relative
		// tokens scraped from the operator's free-text reason/comment no longer
		// mutate scope. No-op on an empty scope (keeps the runner's git add -A
		// fallback).
		scopeFiles = s.mergeStructuredScopeFiles(r.Context(), scopeFiles, s.resolveApprovalAddScopeFiles(r.Context(), runRow))
		// Surface the operator's approval-time add_scope_files additions in the
		// rendered (SPA-readable) implement prompt TEXT (#1406), the SAME
		// derivation as the dispatch path above so the displayed prompt stays
		// byte-for-byte consistent with the runner-facing prompt.
		trigger.AmendedScopeFiles = s.amendedScopeFilesForReview(r.Context(), runRow, approvedPlan)
		// Fold the operator-approved mid-stage scope amendment paths (#961),
		// same derivation as the dispatch path so the rendered view matches.
		scopeFiles = s.mergeApprovedScopeAmendments(r.Context(), scopeFiles, runRow.ID, stage.ID)
		// Subtract the operator's gate-time remove_scope_files (#1726), the same
		// derivation as the dispatch path so the rendered (SPA-readable) view
		// stays byte-for-byte consistent with the runner-facing prompt.
		removedScopeFiles := s.resolveApprovalRemoveScopeFiles(r.Context(), runRow)
		scopeFiles = s.subtractScopePaths(r.Context(), scopeFiles, removedScopeFiles, "approval-remove-scope-files")
		trigger.RemovedScopeFiles = removedScopeFiles
		// Fix-up pass (#762): when the operator routed implement-review
		// concerns back to this stage, deliver them as binding instructions
		// (reusing #558's framing) and RETAIN the FULL approved plan scope
		// (#1314), identical derivation to the dispatch path so the rendered view
		// matches byte-for-byte — the inherited plan scope.files, allow_create
		// (#823), and the stage's approved mid-pass scope amendments (#961). No-op
		// for a normal (non-fix-up) implement dispatch.
		if rendered := s.resolveFixupConcerns(r.Context(), runRow.ID, stage.ID); len(rendered) > 0 {
			trigger.FixupConcerns = rendered
			scopeFiles = s.effectiveFixupScope(r.Context(), scopeFiles,
				s.resolveFixupAllowCreate(r.Context(), runRow.ID, stage.ID),
				s.resolveApprovedScopeAmendments(r.Context(), runRow.ID, stage.ID))
			// Inject the prior implement commit's diff (#1163), identical
			// derivation to the dispatch path so the rendered view matches the
			// runner-facing prompt byte-for-byte.
			trigger.FixupPriorDiff, trigger.FixupPriorDiffFiles = s.resolveFixupPriorDiff(r.Context(), runRow.ID, stage.ID)
			// Emit the fix-up routing flag (#784) so the rendered prompt view
			// and the runner-facing response stay byte-consistent. The SPA path
			// is read-only and never drives a commit; the same derivation keeps
			// the displayed and dispatched responses identical.
			fixup = true
			fixupBranch = s.fixupBranchForRun(r.Context(), runRow, stage)
			fixupExpectedHeadSHA = s.resolveFixupExpectedHeadSHA(r.Context(), runRow.ID, stage.ID)
			// Near-deterministic apply-list (#1165): serve the routed concerns'
			// suggested_patches ONLY when every routed concern carries one, so the
			// runner can git-apply them instead of spawning the agent. nil (the
			// agent path) when any concern lacks a patch.
			fixupApplyPatches = s.resolveFixupApplyPatches(r.Context(), runRow.ID, stage.ID)
		}
	}

	// Acceptance stage (E31.6 / #1534): the independent validator judges the
	// running instance against the approved plan's acceptance criteria. Feed the
	// approved plan (the criteria source) and the target-instance URL seam.
	// Unlike the implement branch, do NOT attach ScopeFiles / BindingAssertions
	// / diff-adjacent fields — the diff is deliberately withheld for
	// independence (ADR-049 decision #4). resolveAcceptanceTargetURL returns the
	// empty string until E31.4/#1532's egress-allowance grammar lands, and
	// buildAcceptance then renders an explicit not-declared line.
	if stage.Type == run.StageTypeAcceptance {
		// Run/stage ids for the acceptance prompt's run/stage-keyed verdict
		// file-fallback path (#1780). Populated only on the acceptance path so
		// acceptanceVerdictPathForTrigger names the SAME keyed path the runner
		// reads first; other prompts leave them empty and fall back to the
		// legacy fixed path, staying byte-identical.
		trigger.AcceptanceRunID = runRow.ID.String()
		trigger.AcceptanceStageID = stage.ID.String()
		approvedPlan, err := s.loadApprovedPlanForRun(r.Context(), runRow.ID)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"load approved plan failed", map[string]any{"error": err.Error()})
			return
		}
		trigger.ApprovedPlan = approvedPlan
		trigger.TargetInstanceURL = s.resolveAcceptanceTargetURL(r.Context(), runRow)
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
		// Surface-coupling sibling map (#763/#1797): thread the static
		// surface-sweep registry into the plan prompt's Coupling-discovery
		// checklist so the planner scopes (or justifies) lockstep siblings at
		// first emission instead of the plan gate burning a review round on a
		// deterministic miss. surfacePatterns is the single source of truth; the
		// accessor is a pure projection. Set on BOTH prompt handlers so the
		// signed prompt and the render preview stay byte-identical.
		trigger.SurfaceCouplingPatterns = surfaceCouplingPatternsForPrompt()
		if runRow.TriggerRef != nil {
			trigger.PriorRejectionFeedback = s.loadPriorRejectionFeedback(r.Context(), runRow.Repo, *runRow.TriggerRef, runRow.ID)
		}
		trigger.PriorSchemaValidationError = s.loadPriorSchemaValidationError(r.Context(), runRow.ID)
		// Clarification answers (#1088): on resume after an awaiting_input
		// park, the operator's answers to the parked clarification_request
		// ride the binding-conditions channel (t.ApprovalConditions) so the
		// existing "Clarification answers (binding ...)" section renders. A
		// dedicated clarification_answered channel, NOT the implement-stage
		// approval_submitted one — a parked plan stage is not yet approved.
		// nil on a normal first-pass plan (no answers recorded).
		trigger.ApprovalConditions = s.loadClarificationAnswers(r.Context(), runRow.ID)
		// Revision constraint (#1099): on a plan-gate `revise` re-open, the
		// operator's binding design constraint rides a DEDICATED plan_revised
		// channel (not the approval/clarification one) and the prior plan is
		// carried as the revision base. First-pass plan dispatch records no
		// plan_revised entry, so RevisionConstraint stays nil and the base is
		// not loaded — normal plans are byte-unchanged.
		trigger.RevisionConstraint = s.loadRevisionConstraint(r.Context(), runRow.ID)
		if trigger.RevisionConstraint != nil {
			trigger.RevisionBasePlan = s.loadRevisionBasePlan(r.Context(), runRow.ID)
		}
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
		AgentVersionRange:    s.resolveExecutorAgentVersionRange(r.Context(), runRow, stage.Type),
		AgentSelfRetry:       s.resolveAgentSelfRetryForStage(r.Context(), runRow, stage.Type),
		MaxRetriesSnapshot:   runRow.MaxRetriesSnapshot,
		RetryAttempt:         runRow.RetryAttempt,
		ScopeFiles:           scopeFiles,
		BindingAssertions:    bindingAssertions,
		ScopeExemptions:      scopeExemptions,
		CommitAuthorName:     commitAuthorName,
		CommitAuthorEmail:    commitAuthorEmail,
		Fixup:                fixup,
		FixupBranch:          fixupBranch,
		FixupExpectedHeadSHA: fixupExpectedHeadSHA,
		FixupApplyPatches:    fixupApplyPatches,
	}
	if runRow.DecomposedFrom != nil {
		resp.DecomposedFromRunID = runRow.DecomposedFrom.String()
		if runRow.SliceIndex != nil {
			resp.SliceIndex = *runRow.SliceIndex
		}
	}
	if stage.Type == run.StageTypeImplement {
		rm := s.resolveImplementDispatchModel(r.Context(), runRow, stage, fixup)
		resp.ImplementModel = rm.Value
	}
	// Plan-stage model routing (#1416), same derivation as the dispatch path so
	// the rendered (SPA-readable) prompt response stays byte-consistent with the
	// runner-facing one.
	if stage.Type == run.StageTypePlan {
		rm := s.resolvePlanModelForRun(r.Context(), runRow)
		resp.PlanModel = rm.Value
	}
	// Acceptance-stage containment inputs (E31.7 / #1535) and the E31.18
	// merge-candidate identity, same derivation as the dispatch path so the
	// rendered response stays byte-consistent.
	if stage.Type == run.StageTypeAcceptance {
		resp.EgressTargetHosts = s.resolveAcceptanceEgressTargetHosts(r.Context(), runRow)
		resp.AcceptanceCriteriaIDs = acceptanceCriteriaIDsFromPlan(trigger.ApprovedPlan)
		resp.AcceptanceExpectedHeadSHA = s.resolveAcceptanceExpectedHeadSHA(r.Context(), runRow.ID, stage.ID)
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// resolveImplementDispatchModel returns the implement model an implement-stage
// prompt fetch should carry to the runner's --model (#1164). For a fix-up
// dispatch it honors the model PINNED on the stage_fixup_triggered audit entry
// at trigger time (fixupResolvedModelFromAudit): when that read returns ok the
// pin wins EVEN IF its value is empty (a deliberately-pinned empty-ladder spawn
// stays empty regardless of any later config change) — the whole point of
// pinning at trigger time. The function falls through to live resolution
// (resolveImplementModelForRun) ONLY when fixup is false (a normal implement
// dispatch) or the read returns ok=false (no pin written: a pre-#1164 fix-up),
// keeping those paths byte-identical to today.
func (s *Server) resolveImplementDispatchModel(ctx context.Context, runRow *run.Run, stage *run.Stage, fixup bool) ResolvedModel {
	if fixup {
		if pinned, ok := s.fixupResolvedModelFromAudit(ctx, runRow.ID, stage.ID); ok {
			return pinned
		}
	}
	return s.resolveImplementModelForRun(ctx, runRow)
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
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)
	issue, err := github.GetIssue(ctx, scope, repo, issueNumber)
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
	comments, err := github.ListIssueComments(ctx, scope, repo, issueNumber)
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

// resolveExecutorAgentVersionRange returns the stage executor's spec-declared
// agent CLI compatibility range (executor.agent_version, #1743) for the given
// stage type, threaded to the runner as promptResponse.agent_version_range.
// Mirrors resolveVerifyConfig's parse + stage-lookup pattern. Returns the
// empty string on any error (nil spec, missing workflow, parse failure) or an
// absent field so the runner performs no compatibility check — byte-identical
// to today.
func (s *Server) resolveExecutorAgentVersionRange(ctx context.Context, runRow *run.Run, stageType run.StageType) string {
	if runRow.WorkflowSpec == nil {
		return ""
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: parse workflow spec for agent_version range",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()),
		)
		return ""
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return ""
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
	return specStage.Executor.AgentVersion
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
// "### Fix-up concerns" section.
//
// It reads the stage_fixup_triggered audit entries the fix-up handler writes
// (server/fixup.go), filters to the current stage, and uses the NEWEST entry
// — a fix-up re-opens the stage to pending and the renderer must reflect the
// most recent trigger. Each entry's `concerns` field is the resolved
// []planreview.Concern set the operator selected; they are formatted as
// "[severity/category] note" so the agent sees the full reviewer context.
//
// Each returned prompt.FixupConcern carries AcceptanceDerived = (the persisted
// concern's Provenance == planreview.ConcernProvenanceAcceptance), so the
// prompt renderer routes acceptance-synthesized concerns (ADR-050 / E31.8 /
// #1613) through the untrusted-DATA quarantine envelope while operator/reviewer
// concerns (empty Provenance) render on the unchanged trusted path.
//
// Returns nil when the AuditRepo is unconfigured, the stage carries no fix-up
// trigger (the common, non-fix-up case), or on any error — best-effort, same
// WARN-and-proceed posture as the other prompt resolvers. The concern-scrape
// that the prior #1162 narrowing fed off was removed in #1314: a fix-up now
// retains the full approved plan scope, so the joined concern text no longer
// feeds scope computation.
func (s *Server) resolveFixupConcerns(ctx context.Context, runID, stageID uuid.UUID) (rendered []prompt.FixupConcern) {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageFixupTriggered)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list stage_fixup_triggered audit failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return nil
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
		rendered = make([]prompt.FixupConcern, 0, len(payload.Concerns))
		for _, c := range payload.Concerns {
			rendered = append(rendered, prompt.FixupConcern{
				Text:              fmt.Sprintf("[%s/%s] %s", c.Severity, c.Category, c.Note),
				AcceptanceDerived: c.Provenance == planreview.ConcernProvenanceAcceptance,
			})
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"prompt: loaded fix-up concerns into implement prompt",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.Int("concern_count", len(rendered)),
		)
		return rendered
	}
	return nil
}

// resolveFixupApplyPatches returns the near-deterministic apply-list (#1165) for
// a fix-up dispatch: one reviewer-emitted unified diff per routed concern, in
// routing order — but ONLY when EVERY routed concern carries a non-empty
// suggested_patch. A single patch-less concern (non-mechanical, or a reviewer
// that declined a diff) makes the whole pass ineligible and returns nil, so the
// runner takes the unchanged agent fix-up path; a partial apply is never
// served. This is the all-or-nothing gate that mirrors fixupApplyEligible on
// the trigger-audit side.
//
// It scans the SAME newest stage_fixup_triggered entry resolveFixupConcerns
// selects (newest-first, first stage-bound entry with a non-empty concern set)
// so the served patches correspond exactly to the rendered fix-up concerns.
// Returns nil when the AuditRepo is unconfigured, the stage carries no fix-up
// trigger, any routed concern lacks a patch, or on any error — best-effort,
// WARN-and-proceed like the other fix-up resolvers; a nil return only means the
// runner re-derives the change with the agent, never a wrong fix.
func (s *Server) resolveFixupApplyPatches(ctx context.Context, runID, stageID uuid.UUID) []fixupApplyPatch {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageFixupTriggered)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list stage_fixup_triggered audit for apply patches failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var payload struct {
			Concerns []planreview.Concern `json:"concerns"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			// Same tolerance as resolveFixupConcerns: skip a malformed entry.
			continue
		}
		if len(payload.Concerns) == 0 {
			continue
		}
		patches := make([]fixupApplyPatch, 0, len(payload.Concerns))
		for _, c := range payload.Concerns {
			if strings.TrimSpace(c.SuggestedPatch) == "" {
				// Not every routed concern carries a patch → ineligible. Bail to
				// the agent path rather than serve a partial apply-list.
				s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
					"prompt: fix-up not apply-eligible (a routed concern lacks a suggested_patch); agent path",
					slog.String("run_id", runID.String()),
					slog.String("stage_id", stageID.String()),
				)
				return nil
			}
			patches = append(patches, fixupApplyPatch{Patch: c.SuggestedPatch})
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"prompt: serving near-deterministic fix-up apply-list",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.Int("patch_count", len(patches)),
		)
		return patches
	}
	return nil
}

// resolveFixupPriorDiff returns the prior implement commit's diff for a fix-up
// pass (#1163): the full unified-diff patch and the rendered changed-file list,
// sourced from the stage's newest REDACTED trace bundle via the same
// pickRedactedTraceHash + TraceStore.Get + bundle.ExtractDiff seam the
// implement-review prompt consumes (server/trace.go). Injected into the slim
// fix-up prompt so the fresh fix-up agent sees the change it is amending without
// cold-re-exploring the repo.
//
// Both return values are populated together on the success path — the changed-
// file list (the second value, trigger.FixupPriorDiffFiles) is ALWAYS returned
// alongside the patch whenever the bundle carries a diff, NOT only when the patch
// is oversize/absent. writeFixupPriorDiff renders that list as an explicit
// concern-relevant focus block on every fix-up dispatch (#1724), IN ADDITION to
// the inline patch, so the slim prompt always carries the concern-relevant files
// without narrowing scope.files (#1314 keeps the effective fix-up scope whole).
//
// The redacted bundle is pre-redacted by the runner and carries only repo-code
// diff — never IssueBody / IssueComments — so feeding it to the network-and-
// state-capable implement agent upholds the never-re-ingest invariant (ADR-029).
// The Variant is pinned to VariantRedacted (never the raw variant) for exactly
// this reason.
//
// Returns ("", "") when AuditRepo or TraceStore is unconfigured, no redacted
// trace exists for the stage, the bundle has no git_diff event (ErrNoDiffEvent),
// or on any list / get / read / extract error — best-effort WARN-and-proceed,
// the same posture as the other fix-up resolvers; it never blocks the dispatch.
func (s *Server) resolveFixupPriorDiff(ctx context.Context, runID, stageID uuid.UUID) (patch, fileList string) {
	if s.cfg.AuditRepo == nil || s.cfg.TraceStore == nil {
		return "", ""
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "trace_uploaded")
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list trace_uploaded audit for fixup prior diff failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return "", ""
	}
	hash, ok := pickRedactedTraceHash(entries, stageID)
	if !ok {
		// No redacted trace for this stage yet — nothing to amend against.
		return "", ""
	}
	body, err := s.cfg.TraceStore.Get(ctx, tracestore.BundleRef{
		RunID:       runID,
		Variant:     tracestore.VariantRedacted,
		ContentHash: hash,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: get redacted trace bundle for fixup prior diff failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return "", ""
	}
	defer func() { _ = body.Close() }()
	bundleBytes, err := io.ReadAll(body)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: read redacted trace bundle for fixup prior diff failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return "", ""
	}
	diff, err := bundle.ExtractDiff(bundleBytes)
	if err != nil {
		// ErrNoDiffEvent (older bundle / no diff) and any parse error degrade to
		// no injection — the slim prompt simply omits the section.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: extract diff from redacted trace for fixup prior diff failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return "", ""
	}
	return diff.Patch, renderDiffForReview(diff)
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
	return s.resolveNewestReportedHeadSHA(ctx, runID, stageID, "fixup_expected_head_sha")
}

// resolveAcceptanceExpectedHeadSHA returns the run's merge-candidate
// identity — the same newest-reported-head walk resolveFixupExpectedHeadSHA
// performs — advertised on acceptance dispatches as
// `acceptance_expected_head_sha` (E31.18 / #1569) so the runner's pre-spawn
// target-identity gate can verify the declared acceptance target serves the
// merge candidate (its /healthz git_sha) rather than a stale build.
//
// Same "" posture: unconfigured AuditRepo, empty ledger, or read error all
// WARN-and-omit, and the runner degrades to unverifiable-warn.
func (s *Server) resolveAcceptanceExpectedHeadSHA(ctx context.Context, runID, stageID uuid.UUID) string {
	return s.resolveNewestReportedHeadSHA(ctx, runID, stageID, "acceptance_expected_head_sha")
}

// resolveNewestReportedHeadSHA is the shared reported-head ledger walk behind
// resolveFixupExpectedHeadSHA and resolveAcceptanceExpectedHeadSHA: the newest
// head_sha across lineageLedgerCategories, "" (WARN-and-omit, logging forField
// as the omitted wire field) when the AuditRepo is unconfigured, no entry
// carries a head_sha, or on any read error.
func (s *Server) resolveNewestReportedHeadSHA(ctx context.Context, runID, stageID uuid.UUID, forField string) string {
	if s.cfg.AuditRepo == nil {
		return ""
	}
	var newest *audit.Entry
	var newestSHA string
	for _, cat := range lineageLedgerCategories {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"prompt: list reported-head audit entries failed; omitting field",
				slog.String("field", forField),
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
			"prompt: no reported-head audit entry found; omitting field",
			slog.String("field", forField),
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

// matchDecomposedSubPlan returns the sub-plan a decomposed child run owns,
// plus its index within the decomposition's sub_plans. Linkage is by the
// child run row's SliceIndex — the 0-based sub_plan position the orchestrator
// persists on every fan-out child (orchestrator.fanoutIfDecomposed sets
// CreateRunParams.SliceIndex, round-tripped through the runs.slice_index
// column). This durable index replaces the earlier string-prefix match
// against IssueContext.Body, which returned nil for a campaign-minted parent:
// its nil IssueContext yielded a headerless child body, so every prefix match
// failed and each child silently inherited the parent's full scope (#1721,
// reopening #1669).
//
// Returns (nil, -1) when:
//   - the run is not decomposed (DecomposedFrom == nil)
//   - the run carries no SliceIndex (not a fan-out child)
//   - parentPlan is nil or carries no decomposition (degrade gracefully)
//   - *SliceIndex is out of range for the decomposition's sub_plans
//     (defensive — an out-of-band plan whose sub_plans no longer cover the
//     persisted index)
//
// parentPlan is the already-loaded approved plan for the child run; for a
// decomposed child loadApprovedPlanForRun walks ParentRunID up to the
// parent's decomposed plan, so the caller's single load is reused here
// instead of re-reading the artifact.
func matchDecomposedSubPlan(runRow *run.Run, parentPlan *plan.Plan) (*plan.SubPlanSummary, int) {
	if runRow.DecomposedFrom == nil || runRow.SliceIndex == nil {
		return nil, -1
	}
	if parentPlan == nil || parentPlan.Decomposition == nil {
		return nil, -1
	}
	idx := *runRow.SliceIndex
	if idx < 0 || idx >= len(parentPlan.Decomposition.SubPlans) {
		return nil, -1
	}
	return &parentPlan.Decomposition.SubPlans[idx], idx
}

// decomposedScopeError is returned by requireDecomposedScope when a decomposed
// child's slice scope cannot be resolved. It is the fail-loud (#1721)
// replacement for the pre-fix silent full-parent-scope fallback: the prompt
// handlers render it as a 409 decomposed_scope_unresolved naming the child,
// its slice index, and the parent, rather than serving the parent's full
// scope to a child that would then conflict permanently at fan-in.
type decomposedScopeError struct {
	childRunID  string
	sliceIndex  int // -1 when the run carries no SliceIndex
	parentRunID string
	reason      string
}

func (e *decomposedScopeError) Error() string {
	return fmt.Sprintf("decomposed child %s (slice %d, parent %s): %s",
		e.childRunID, e.sliceIndex, e.parentRunID, e.reason)
}

// requireDecomposedScope resolves the narrowed scope for a decomposed child
// run, failing closed rather than degrading to the parent's full scope
// (#1721). It requires matchDecomposedSubPlan to link the child to a sub-plan
// (by the persisted SliceIndex) whose Scope declares >=1 file; any miss —
// no linkage, out-of-range index, or a matched slice with nil/empty scope —
// returns a *decomposedScopeError naming the child, its slice index, and the
// parent. On success it returns the slice's own scope.files (with coupled
// *_test.go siblings folded, matching the narrowing) plus the per-slice
// ScopeConstraint.
//
// Callers MUST invoke this only for a decomposed child (runRow.DecomposedFrom
// != nil); a non-decomposed run keeps the parent plan's full scope and a nil
// constraint at the call site.
func (s *Server) requireDecomposedScope(ctx context.Context, runRow *run.Run, parentPlan *plan.Plan) ([]scopeFile, *prompt.ScopeConstraint, error) {
	sliceIdx := -1
	if runRow.SliceIndex != nil {
		sliceIdx = *runRow.SliceIndex
	}
	parentRunID := ""
	if runRow.DecomposedFrom != nil {
		parentRunID = runRow.DecomposedFrom.String()
	}
	matched, matchIdx := matchDecomposedSubPlan(runRow, parentPlan)
	if matched == nil {
		return nil, nil, &decomposedScopeError{
			childRunID:  runRow.ID.String(),
			sliceIndex:  sliceIdx,
			parentRunID: parentRunID,
			reason:      "no sub-plan linked to the child's slice index",
		}
	}
	files := s.resolveDecomposedScopeFiles(ctx, runRow, parentPlan)
	if len(files) == 0 {
		return nil, nil, &decomposedScopeError{
			childRunID:  runRow.ID.String(),
			sliceIndex:  matchIdx,
			parentRunID: parentRunID,
			reason:      "linked sub-plan declares no scope files",
		}
	}
	return files, s.resolveDecomposedScopeConstraint(ctx, runRow, parentPlan), nil
}

// writeDecomposedScopeUnresolved renders a requireDecomposedScope failure as a
// 409 decomposed_scope_unresolved with run_id / slice_index / parent_run_id
// details. Shared by both prompt endpoints (handleGetStagePrompt and
// handleGetStagePromptRender) so the dispatch and preview/render surfaces fail
// closed identically (#1721).
func (s *Server) writeDecomposedScopeUnresolved(w http.ResponseWriter, r *http.Request, err error) {
	var scopeErr *decomposedScopeError
	if errors.As(err, &scopeErr) {
		s.writeError(w, r, http.StatusConflict, "decomposed_scope_unresolved", scopeErr.Error(),
			map[string]any{
				"run_id":        scopeErr.childRunID,
				"slice_index":   scopeErr.sliceIndex,
				"parent_run_id": scopeErr.parentRunID,
			})
		return
	}
	// Defensive: any non-typed error from the require path still fails closed.
	s.writeError(w, r, http.StatusConflict, "decomposed_scope_unresolved", err.Error(), nil)
}

// resolveDecomposedScopeConstraint builds a *prompt.ScopeConstraint for
// child runs of a decomposed plan. Returns nil when the child doesn't
// match a sub-plan (see matchDecomposedSubPlan) — for a decomposed child
// requireDecomposedScope now turns that nil into a fail-loud 409 rather than
// letting the caller degrade to the parent's full scope (#1721). parentPlan
// is the caller's already-loaded approved plan — for a decomposed child this
// is the parent's decomposed plan — so no additional artifact read happens.
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
	// Narrow the child to its slice via the matched sub-plan's own scope.files
	// (#1669). The plan gate now requires per-slice scope, so a matched child
	// always carries these paths — the nil guard is defensive for an
	// out-of-band plan constructed without the gate.
	var scopeFiles []string
	if matched.Scope != nil {
		for _, f := range matched.Scope.Files {
			scopeFiles = append(scopeFiles, f.Path)
		}
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
		"prompt: injected scope constraint for decomposed child",
		slog.String("child_run_id", runRow.ID.String()),
		slog.String("parent_run_id", runRow.DecomposedFrom.String()),
		slog.Int("sibling_count", len(siblingHints)),
		slog.Int("scope_file_count", len(scopeFiles)),
	)
	return &prompt.ScopeConstraint{
		ScopeHint:    matched.ScopeHint,
		ParentRunID:  runRow.DecomposedFrom.String(),
		SiblingHints: siblingHints,
		ScopeFiles:   scopeFiles,
	}
}

// resolveDecomposedScopeFiles returns the matched sub-plan's own
// scope.files for a decomposed child, converted to the prompt-response
// wire shape. Returns nil when the child doesn't match a sub-plan or the
// matched sub-plan omits scope. For a decomposed child this nil is no
// longer a silent full-scope fallback: requireDecomposedScope treats it as
// a fail-closed condition and the caller returns 409
// decomposed_scope_unresolved (#1721). parentPlan is the caller's already-
// loaded approved plan, reused here rather than re-read.
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

// loadClarificationAnswers scans the run's clarification_answered audit
// entries (newest-first) for the first entry carrying a non-empty rendered
// `conditions` blob — the operator's answers to a parked clarification_request
// (#1088). Returns the blob (capped at 4000 bytes) or nil when none is found.
// Best-effort: WARN-logs and returns nil on any error.
//
// This is a DEDICATED channel, isolated from loadApprovalConditions'
// approval_submitted entries: a plan stage parked at awaiting_input is NOT
// approved, so its answers must never ride a decision=approve entry. The
// plan-stage prompt branch feeds this into trigger.ApprovalConditions so the
// existing "Clarification answers (binding ...)" renderer fires on resume.
func (s *Server) loadClarificationAnswers(ctx context.Context, runID uuid.UUID) *string {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "clarification_answered")
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list clarification_answered for resume failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		var payload struct {
			Conditions string `json:"conditions"`
		}
		if err := json.Unmarshal(entries[i].Payload, &payload); err != nil {
			continue
		}
		if payload.Conditions != "" {
			c := payload.Conditions
			const maxConditionBytes = 4000
			if len(c) > maxConditionBytes {
				c = c[:maxConditionBytes] + "...[truncated]"
			}
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"prompt: loaded clarification answers into resumed plan prompt",
				slog.String("run_id", runID.String()),
				slog.Int("conditions_bytes", len(payload.Conditions)),
			)
			return &c
		}
	}
	return nil
}

// loadRevisionConstraint scans the run's plan_revised audit entries
// (newest-first) for the first entry carrying a non-empty rendered
// `conditions` blob — the operator's binding design constraint for a
// plan-gate `revise` re-open (#1099). Returns the blob (capped at 4000
// bytes) or nil when none is found. Best-effort: WARN-logs and returns
// nil on any error.
//
// This is a DEDICATED channel, isolated from loadApprovalConditions
// (approval_submitted) and loadClarificationAnswers (clarification_
// answered): the revise constraint must never ride a decision=approve or
// a clarification answer entry. The plan-stage prompt branch feeds this
// into trigger.RevisionConstraint so the dedicated "Revision constraint
// (binding ...)" renderer fires on the re-dispatch.
func (s *Server) loadRevisionConstraint(ctx context.Context, runID uuid.UUID) *string {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryPlanRevised)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list plan_revised for revise resume failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		var payload struct {
			Conditions string `json:"conditions"`
		}
		if err := json.Unmarshal(entries[i].Payload, &payload); err != nil {
			continue
		}
		if payload.Conditions != "" {
			c := payload.Conditions
			const maxConditionBytes = 4000
			if len(c) > maxConditionBytes {
				c = c[:maxConditionBytes] + "...[truncated]"
			}
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"prompt: loaded revision constraint into re-dispatched plan prompt",
				slog.String("run_id", runID.String()),
				slog.Int("constraint_bytes", len(payload.Conditions)),
			)
			return &c
		}
	}
	return nil
}

// loadRevisionBasePlan returns the run's most-recent plan artifact
// serialized to indented JSON — the revision base a plan-gate `revise`
// re-open carries so the planner revises the existing plan rather than
// replanning blank-slate (#1099). Returns nil when no plan artifact
// exists yet (impossible on a real revise, which only fires after a plan
// landed) or on any load/marshal error — best-effort, the prompt still
// binds the constraint without the base block.
func (s *Server) loadRevisionBasePlan(ctx context.Context, runID uuid.UUID) *string {
	p, err := s.loadApprovedPlanForRun(ctx, runID)
	if err != nil || p == nil {
		return nil
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil
	}
	base := string(raw)
	return &base
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

// resolveRecoveryScopeExemptions returns the operator scope exemptions for an
// implement-stage prompt, read from the run's OWN plan_reused_from audit entry
// (#1229). A recovery run — the new-child mint OR the in-place re-driven
// decomposition child (same id) — records exempted_paths on its own
// plan_reused_from entry, so NO fan-out parent fallback is needed (unlike
// resolveApprovalConditions): the exemptions belong to the run executing the
// implement stage. Returns nil on every non-recovery run (no plan_reused_from),
// keeping the response byte-identical to today. Best-effort: WARN-logs and
// returns nil on any error.
func (s *Server) resolveRecoveryScopeExemptions(ctx context.Context, runID uuid.UUID) []scopeExemption {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryPlanReusedFrom)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list plan_reused_from for scope exemptions failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		var payload struct {
			ExemptedPaths []scopeExemption `json:"exempted_paths"`
		}
		if err := json.Unmarshal(entries[i].Payload, &payload); err != nil {
			continue
		}
		if len(payload.ExemptedPaths) > 0 {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"prompt: loaded operator scope exemptions into implement prompt-response",
				slog.String("run_id", runID.String()),
				slog.Int("count", len(payload.ExemptedPaths)),
			)
			return payload.ExemptedPaths
		}
	}
	return nil
}

// loadRecoveryResumeReason returns the operator's resume_run reason recorded on
// the run's OWN plan_reused_from audit entry (#1229, Part D), capped at 4000
// bytes. nil when no plan_reused_from entry exists (every non-recovery run) or
// its reason is empty/whitespace — so normal prompts are byte-identical.
// Best-effort: WARN-logs and returns nil on any error. The caller appends this
// to trigger.ApprovalConditions so the recovery directive rides the existing
// #558 binding-conditions channel, mirroring approve_plan note delivery.
func (s *Server) loadRecoveryResumeReason(ctx context.Context, runID uuid.UUID) *string {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryPlanReusedFrom)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list plan_reused_from for resume reason failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		var payload struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(entries[i].Payload, &payload); err != nil {
			continue
		}
		if strings.TrimSpace(payload.Reason) != "" {
			c := payload.Reason
			const maxConditionBytes = 4000
			if len(c) > maxConditionBytes {
				c = c[:maxConditionBytes] + "...[truncated]"
			}
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"prompt: loaded recovery resume reason into implement prompt conditions",
				slog.String("run_id", runID.String()),
				slog.Int("reason_bytes", len(payload.Reason)),
			)
			return &c
		}
	}
	return nil
}

// appendRecoveryResumeReason folds the recovery run's resume_run reason
// (#1229, Part D) into the implement prompt's binding conditions so the
// operator's steer at recovery reaches the agent through the existing #558
// "Approval conditions" channel. The reason is appended under a labeled
// marker so the agent can tell it apart from inherited approve-with-conditions
// text. When recoveryReason is nil the conditions are returned unchanged
// (every non-recovery run), keeping normal prompts byte-identical.
func appendRecoveryResumeReason(conditions, recoveryReason *string) *string {
	if recoveryReason == nil {
		return conditions
	}
	labeled := "Recovery directive (resume_run reason): " + *recoveryReason
	if conditions == nil {
		return &labeled
	}
	combined := *conditions + "\n\n" + labeled
	return &combined
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

// loadApprovalRemoveScopeFiles scans the run's approval_submitted audit
// entries (newest-first) for the first approve carrying remove_scope_files and
// returns its paths (#1726). The inverse of loadApprovalAddScopeFiles: the
// prompt builder subtracts these from the effective scope. Best-effort:
// WARN-logs and returns nil on any error.
func (s *Server) loadApprovalRemoveScopeFiles(ctx context.Context, runID uuid.UUID) []string {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "approval_submitted")
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list approval_submitted for remove_scope_files failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		var payload struct {
			Decision         string   `json:"decision"`
			RemoveScopeFiles []string `json:"remove_scope_files"`
		}
		if err := json.Unmarshal(entries[i].Payload, &payload); err != nil {
			continue
		}
		if payload.Decision == "approve" && len(payload.RemoveScopeFiles) > 0 {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"prompt: loaded structured remove_scope_files for implement scope",
				slog.String("run_id", runID.String()),
				slog.Int("count", len(payload.RemoveScopeFiles)),
			)
			return payload.RemoveScopeFiles
		}
	}
	return nil
}

// resolveApprovalRemoveScopeFiles returns the structured remove_scope_files
// paths for an implement-stage prompt, resolving across the decomposition
// fan-out boundary (#1726, mirroring resolveApprovalAddScopeFiles / #824). It
// reads the run's own approval_submitted entries first; for a decomposed child
// with no gate of its own that yields nil, so it falls back to the PARENT
// run's paths so removals reach implement-only decomposed children. CI-retry /
// category-B recovery children carry ParentRunID and get the same single-level
// fallback.
func (s *Server) resolveApprovalRemoveScopeFiles(ctx context.Context, runRow *run.Run) []string {
	if paths := s.loadApprovalRemoveScopeFiles(ctx, runRow.ID); len(paths) > 0 {
		return paths
	}
	if runRow.DecomposedFrom != nil {
		paths := s.loadApprovalRemoveScopeFiles(ctx, *runRow.DecomposedFrom)
		if len(paths) > 0 {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"prompt: inherited remove_scope_files from decomposition parent",
				slog.String("child_run_id", runRow.ID.String()),
				slog.String("parent_run_id", runRow.DecomposedFrom.String()),
			)
		}
		return paths
	}
	if runRow.ParentRunID == nil {
		return nil
	}
	paths := s.loadApprovalRemoveScopeFiles(ctx, *runRow.ParentRunID)
	if len(paths) > 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"prompt: inherited remove_scope_files from retry/recovery parent",
			slog.String("child_run_id", runRow.ID.String()),
			slog.String("parent_run_id", runRow.ParentRunID.String()),
		)
	}
	return paths
}

// loadApprovalBindingAssertions scans the run's approval_submitted audit
// entries (newest-first) for the first entry where decision=="approve" and
// returns its structured binding_assertions slice (#1171). Returns nil when
// none is found. Best-effort: WARN-logs and returns nil on any error.
func (s *Server) loadApprovalBindingAssertions(ctx context.Context, runID uuid.UUID) []bindingAssertion {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "approval_submitted")
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list approval_submitted for binding_assertions failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		var payload struct {
			Decision          string             `json:"decision"`
			BindingAssertions []bindingAssertion `json:"binding_assertions"`
		}
		if err := json.Unmarshal(entries[i].Payload, &payload); err != nil {
			continue
		}
		if payload.Decision == "approve" && len(payload.BindingAssertions) > 0 {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"prompt: loaded binding_assertions for implement prompt-response",
				slog.String("run_id", runID.String()),
				slog.Int("count", len(payload.BindingAssertions)),
			)
			return payload.BindingAssertions
		}
	}
	return nil
}

// resolveApprovalBindingAssertions returns the operator-declared binding
// assertions for an implement-stage prompt, resolving across the decomposition
// fan-out boundary (#1171, mirroring resolveApprovalAddScopeFiles / #824). It
// reads the run's own approval_submitted entries first; for a decomposed child
// with no gate of its own that yields nil, so it falls back to the PARENT
// run's declared assertions so they reach implement-only decomposed children.
//
// CI-retry / category-B recovery children (#978) carry ParentRunID instead of
// DecomposedFrom and get the same single-level fallback: the parent's declared
// assertions were part of its effective approval contract and must reach the
// recovery implement stage too.
func (s *Server) resolveApprovalBindingAssertions(ctx context.Context, runRow *run.Run) []bindingAssertion {
	if a := s.loadApprovalBindingAssertions(ctx, runRow.ID); len(a) > 0 {
		return a
	}
	if runRow.DecomposedFrom != nil {
		a := s.loadApprovalBindingAssertions(ctx, *runRow.DecomposedFrom)
		if len(a) > 0 {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"prompt: inherited binding_assertions from decomposition parent",
				slog.String("child_run_id", runRow.ID.String()),
				slog.String("parent_run_id", runRow.DecomposedFrom.String()),
			)
		}
		return a
	}
	if runRow.ParentRunID == nil {
		return nil
	}
	a := s.loadApprovalBindingAssertions(ctx, *runRow.ParentRunID)
	if len(a) > 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"prompt: inherited binding_assertions from retry/recovery parent",
			slog.String("child_run_id", runRow.ID.String()),
			slog.String("parent_run_id", runRow.ParentRunID.String()),
		)
	}
	return a
}
