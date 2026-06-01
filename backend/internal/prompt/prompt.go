// Package prompt builds the constructed prompt the agent sees for
// a given stage. Construction is server-side and deterministic: the
// runner fetches the prompt over HTTP rather than constructing it
// itself, so two replays of the same stage produce byte-identical
// prompts and the audit log records exactly what the agent was
// asked to do.
//
// v0 supports a closed set of stage types; the schema-validated
// workflow spec means StageType arrives at Build with a known
// value. Unsupported types return ErrUnsupportedStage and the
// caller (the prompt HTTP handler) surfaces a 501 — the v0 surface
// is intentionally narrow and we'd rather refuse than guess.
package prompt

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// defaultStageTimeoutMinutes mirrors spec.DefaultStageTimeout (15 minutes,
// per ADR-025 D1). If that default changes in the spec package, update here.
const defaultStageTimeoutMinutes = 15

// PlanReviewSplitMarker is the substring that separates the stable
// role-constraint preamble (cached) from the variable plan + issue
// content in the plan-review prompt. Adapters split here for
// Anthropic prompt caching; tests assert the constant appears in
// buildPlanReview's output.
const PlanReviewSplitMarker = "\n### Plan artifact\n\n"

// ImplementReviewSplitMarker is the substring that separates the stable
// role-constraint preamble (cached) from the variable diff + plan + issue
// content in the implement-review prompt. Adapters split here for
// Anthropic prompt caching; tests assert the constant appears in
// buildImplementReview's output (ADR-027 impl 2/2).
const ImplementReviewSplitMarker = "\n### Diff under review\n\n"

// ErrUnsupportedStage signals the requested stage type isn't yet
// wired for prompt construction. The handler maps this to HTTP 501.
var ErrUnsupportedStage = errors.New("prompt: unsupported stage type")

// PlanArtifactPath is the absolute path the runner expects to find
// the agent's plan artifact at after a plan-stage invocation. It's
// embedded in the prompt template (so the agent knows where to
// write) and matched by the runner's --plan-out flag (so it knows
// where to read). Hardcoded for v0; v0.x can lift this into a
// per-stage variable if multi-tenancy demands isolation.
const PlanArtifactPath = "/tmp/fishhawk-plan.json"

// PullRequestDescriptionPath is the absolute path the runner expects
// to find the agent-authored PR description at after an
// implement-stage invocation. Format: first line = title (≤72 chars),
// blank line, then markdown body. The runner reads this and forwards
// the title + body to GitHub's pulls API; if missing or malformed
// the runner falls back to a generic Fishhawk template, so v0 stays
// robust against agents that ignore the instruction.
//
// Hardcoded for v0 — same rationale as PlanArtifactPath. (#206.)
const PullRequestDescriptionPath = "/tmp/fishhawk-pr.md"

// CalibrationBand holds accuracy statistics for a single confidence level
// (high / medium / low) within a calibration window.
type CalibrationBand struct {
	Samples     int
	WithinScale int
}

// CalibrationHint carries aggregated calibration statistics for the plan-
// stage prompt. When non-nil, buildPlan appends a "### Calibration hint"
// section so the agent can self-correct its predicted_runtime_minutes.
// Nil when the workflow has fewer than calibrationHintMinSamples recorded
// implement-stage executions.
type CalibrationHint struct {
	Samples          int
	CalibrationRatio float64
	ActualP50Minutes float64
	ActualP95Minutes float64
	ConfidenceBands  map[string]CalibrationBand
}

// PredictionContext carries the runtime prediction context for the
// implement-stage prompt's Budget context section. Populated by the
// server-side prompt handler from the approved plan's
// predicted_runtime_minutes / predicted_runtime_confidence and the
// spec-resolved implement-stage timeout. Nil when no plan is available
// or the stage is not implement-type.
type PredictionContext struct {
	PredictedMinutes    int
	PredictedConfidence string
	StageBudgetMinutes  int
}

// ScopeConstraint narrows a decomposed child run's implement stage to
// a specific sub-plan scope. When non-nil, buildImplement prepends a
// binding SCOPE CONSTRAINT block so the agent doesn't drift into
// sibling sub-plans. Nil for standalone (non-decomposed) runs.
type ScopeConstraint struct {
	// ScopeHint is this child's scope_hint verbatim from the parent plan.
	ScopeHint string
	// ParentRunID is the UUID string of the parent decomposed run.
	ParentRunID string
	// SiblingHints are the scope_hints of all other sub-plans in the
	// parent decomposition (all sub-plans except this child's).
	SiblingHints []string
}

// IssueComment is one issue comment in Trigger.IssueComments, a
// snapshot taken at trigger time (#618). Author is the commenter's
// GitHub login; CreatedAt is the RFC3339 timestamp from the GitHub
// API. Rendered into the plan-stage prompt by writeIssueContext.
type IssueComment struct {
	Author    string
	Body      string
	CreatedAt string
}

// Trigger captures the bits of the originating event needed to
// construct an issue-driven prompt. Empty IssueTitle / IssueBody
// for non-issue triggers; for v0, those triggers all come from
// issues so this is always populated in practice.
type Trigger struct {
	// Source mirrors run.TriggerSource. Empty when the run was
	// created from a manual CLI trigger with no upstream issue.
	Source string
	// IssueNumber is the issue number when Source identifies an
	// issue or PR. Zero otherwise.
	IssueNumber int
	// IssueTitle is the issue title at trigger time. May be empty
	// for non-issue triggers.
	IssueTitle string
	// IssueBody is the issue body at trigger time. May be empty
	// even for issue triggers (issue created with no body). Only
	// the plan-stage prompt renders the body verbatim; the
	// implement stage links to the issue and lets the agent fetch
	// (#244).
	IssueBody string
	// IssueComments are the issue's comments at trigger time, a
	// snapshot captured alongside IssueBody (#618). Only the
	// plan-stage prompt renders them — after the body — so
	// comment-borne refinements/decisions reach the plan agent on
	// the first attempt. Empty/nil for non-issue triggers, issues
	// with no comments, or runs whose issue_context predates #618.
	IssueComments []IssueComment
	// IssueURL is the canonical github.com URL for the triggering
	// issue. Set by the server-side prompt handler from
	// repo + IssueNumber. Used by the implement-stage prompt's
	// link-only rendering (#244) so the agent can fetch fresh
	// content via its GitHub tooling rather than reasoning from
	// the snapshot taken at plan-stage trigger time.
	IssueURL string
	// Repo is the "owner/name" the run is operating on, surfaced in
	// the prompt so the agent's reasoning can reference it.
	Repo string
	// ApprovedPlan is the standard_v1 plan that the human approved
	// during the plan stage. When set on an implement-stage prompt,
	// the plan becomes the binding instruction the agent must
	// adhere to and the issue context drops to background.
	// Nil means "no approved plan available" — implement falls back
	// to the issue-only prompt and a `plan_missing_for_implement`
	// audit entry surfaces the gap (#223).
	ApprovedPlan *plan.Plan
	// PlanStageTimeout is the max runtime budget for the plan stage.
	// Zero resolves to defaultStageTimeoutMinutes in buildPlan.
	PlanStageTimeout time.Duration
	// ImplementStageTimeout is the max runtime budget for the implement stage.
	// Zero resolves to defaultStageTimeoutMinutes in buildPlan.
	ImplementStageTimeout time.Duration
	// DecomposeRequired signals that the previous plan for this run
	// was rejected because its predicted runtime exceeded the
	// implement-stage budget without a decomposition block. When true,
	// buildPlan injects a binding instruction to populate
	// decomposition.sub_plans.
	DecomposeRequired bool
	// CalibrationHint carries aggregated runtime-calibration statistics
	// for the workflow. When non-nil, buildPlan appends a Calibration
	// hint section so the agent can self-correct its
	// predicted_runtime_minutes. Nil when fewer than the minimum sample
	// threshold of implement-stage executions have been recorded (mirrors
	// the DecomposeRequired pattern for plan-stage hint injection).
	CalibrationHint *CalibrationHint
	// PriorRejectionFeedback is the operator's rationale from the most
	// recent rejection of a plan for the same trigger_ref. When non-nil
	// and non-empty, buildPlan injects a binding "you MUST address this"
	// section so the agent knows why the previous attempt was rejected.
	// Nil when no prior rejection exists or the comment was empty.
	PriorRejectionFeedback *string
	// PredictionContext carries runtime prediction data for the implement-
	// stage prompt. When non-nil, buildImplement appends a Budget context
	// section surfacing predicted_runtime_minutes, predicted_runtime_confidence,
	// and the spec-resolved stage budget. Nil → section omitted (#503).
	PredictionContext *PredictionContext
	// ScopeConstraint narrows the implement stage to a specific sub-plan
	// when this run is a decomposed child. When non-nil, buildImplement
	// prepends a binding SCOPE CONSTRAINT block before the plan and
	// issue sections. Nil for standalone runs (#541).
	ScopeConstraint *ScopeConstraint
	// ApprovalConditions carries the operator's approve-with-notes text for
	// the current run's plan stage. When non-nil, buildImplement injects a
	// binding section after the SCOPE CONSTRAINT block (if any) and before
	// the approved-plan section. Nil means no conditions were given (section
	// omitted).
	ApprovalConditions *string
	// Diff is the rendered changed-files summary for the implement-review
	// prompt (ADR-027 impl 2/2). Populated by the trace handler from
	// bundle.ExtractDiff (path + git status per file). Empty for any
	// non-implement-review build.
	Diff string
	// DiffPatch is the full unified-diff hunk text for the implement-review
	// prompt (#585). Populated by the trace handler from the bundle's
	// git_diff event patch field. When non-empty, buildImplementReview
	// renders the real hunks under the "### Diff under review" section so
	// the reviewer can inspect added/removed lines directly; when empty
	// (older bundles, patch-compute failure, or size-cap) it falls back to
	// the Diff file-list rendering with the original #561 read-the-files
	// caveat. Empty for any non-implement-review build.
	DiffPatch string
}

// Build returns the constructed prompt for the given stage type
// and trigger context. Stage types are the literal strings from
// the workflow-spec schema (`plan`, `implement`, `review`,
// `deploy`, …). Only types this function explicitly supports
// produce a prompt; everything else returns ErrUnsupportedStage.
//
// The prompt is plain text, not JSON or markdown-front-matter — the
// runner writes it to a temp file and passes that path to the
// agent's `--prompt-file` flag.
func Build(stageType string, t Trigger) (string, error) {
	switch stageType {
	case "implement":
		return buildImplement(t), nil
	case "plan":
		return buildPlan(t), nil
	case "plan_review":
		return buildPlanReview(t), nil
	case "implement_review":
		return buildImplementReview(t), nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedStage, stageType)
	}
}

func buildImplement(t Trigger) string {
	var b strings.Builder
	b.WriteString("You are implementing a change in the repository ")
	b.WriteString(quoteRepo(t.Repo))
	b.WriteString(".\n\n")

	// Scope constraint (#541): for decomposed child runs, inject a
	// binding SCOPE CONSTRAINT block before the plan and issue sections.
	// The constraint names this child's scope_hint and lists sibling
	// hints so the agent knows where NOT to touch code.
	if t.ScopeConstraint != nil {
		sc := t.ScopeConstraint
		b.WriteString("SCOPE CONSTRAINT (binding — read before writing any code)\n")
		b.WriteString("=========================================================\n\n")
		fmt.Fprintf(&b, "This run is a decomposed child of parent run %s.\n\n", sc.ParentRunID)
		b.WriteString("Your scope for this child run:\n\n")
		b.WriteString(sc.ScopeHint)
		b.WriteString("\n\n")
		if len(sc.SiblingHints) > 0 {
			b.WriteString("do NOT modify code in sibling scope. Sibling scopes are owned by other child runs:\n\n")
			for _, hint := range sc.SiblingHints {
				b.WriteString("- ")
				b.WriteString(hint)
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
		b.WriteString("Step zero — before writing any code: list the files you intend to modify. " +
			"If any file falls outside your scope above, STOP and surface that the boundary is wrong " +
			"rather than expanding scope. If you find yourself wanting to work in a sibling's area, " +
			"STOP and signal completion instead.\n\n")
	}

	// Approval conditions (#557): when the operator approved the plan with
	// notes, inject a binding section so the agent sees them before reading
	// the plan. Conditions AMEND the plan, are MANDATORY, and win on conflict.
	if t.ApprovalConditions != nil {
		ac := *t.ApprovalConditions
		const maxConditionBytes = 4000
		if len(ac) > maxConditionBytes {
			ac = ac[:maxConditionBytes] + "...[truncated]"
		}
		b.WriteString("### Approval conditions\n\n")
		b.WriteString("The operator approved this plan with the following conditions. These conditions AMEND the plan, are MANDATORY, and win on conflict with plan steps:\n\n")
		b.WriteString(ac)
		b.WriteString("\n\n")
	}

	// Plan-as-contract (#223): when the plan stage produced a
	// standard_v1 artifact and a human approved it, that plan is
	// what the agent commits to. The issue is included afterwards
	// as background context — useful for grounding when the plan
	// is ambiguous, but the plan is the binding instruction the
	// audit log records.
	//
	// When ApprovedPlan is nil we fall back to the historic
	// issue-only prompt so manual / non-issue-triggered runs and
	// edge cases (race between plan upload and implement dispatch)
	// don't 500 the prompt fetch — the gap shows up as a
	// `plan_missing_for_implement` audit entry on the handler side.
	if t.ApprovedPlan != nil {
		writeApprovedPlan(&b, t.ApprovedPlan)
		b.WriteString("Originating issue (link only — fetch if you need detail):\n\n")
		writeIssueLink(&b, t)

		b.WriteString("Your task: implement the approved plan above. The plan is the binding instruction; the issue is linked for grounding when the plan is ambiguous — fetch it via your GitHub tooling if you need the body. Make the smallest set of changes that satisfies the plan.\n")
		b.WriteString("\n")
		b.WriteString("If you discover the plan is wrong or infeasible — a file it names doesn't exist, an approach step is incompatible with the current code, the verification can't be implemented as specified — stop and surface that in your final response rather than diverging silently. The right path in that case is a follow-up run that re-plans, not an off-plan implementation.\n")
		b.WriteString("\n")
		b.WriteString("If the repository has materially changed since the plan was approved (files in the plan's scope have been heavily refactored, an approach step references code that no longer exists), surface that and pause.\n")
		b.WriteString("\n")
	} else {
		// No approved plan (race / non-issue-triggered run / missing
		// plan-stage). Falls back to "implement against the issue"
		// — but still link rather than copy the body, so the agent
		// reasons against current data rather than the snapshot
		// captured at plan-stage trigger time. The agent fetches
		// the body via its GitHub tooling.
		writeIssueLink(&b, t)
		b.WriteString("Your task: implement the change described in the issue above. Fetch the issue body via your GitHub tooling — the URL resolves with the run's installation token. Make the smallest set of changes that satisfies the issue.\n")
		b.WriteString("\n")
	}

	// Budget context (#503): surface the agent's own runtime prediction
	// alongside the spec-resolved stage budget so it can self-regulate
	// scope. Only present when an approved plan exists (PredictionContext
	// is populated by the handler from the plan + resolveAgentTimeout).
	if t.PredictionContext != nil {
		pc := t.PredictionContext
		budgetMins := pc.StageBudgetMinutes
		b.WriteString("### Budget context\n\n")
		fmt.Fprintf(&b, "You predicted **%d minutes** (%s confidence) for this work in your plan. ",
			pc.PredictedMinutes, pc.PredictedConfidence)
		if budgetMins == 0 {
			fmt.Fprintf(&b, "No spec budget was resolved for this stage; the backend default is **%d minutes**.",
				defaultStageTimeoutMinutes)
		} else {
			fmt.Fprintf(&b, "The spec-resolved stage budget is **%d minutes**.", budgetMins)
		}
		b.WriteString(" Allocate carefully and prefer incremental verification.\n\n")
	}

	// PR description: write to a known path so the runner can lift
	// it into the GitHub PR's title + body. Format is documented
	// here in the prompt itself (rather than a separate spec doc)
	// because the agent reads the prompt and nothing else.
	b.WriteString("When you're done, write a pull-request description to `")
	b.WriteString(PullRequestDescriptionPath)
	b.WriteString("`. Format:\n")
	b.WriteString("\n")
	b.WriteString("- The first line is the PR title. Write a task-specific summary of what you changed (e.g. `Add make minio-init target`). Keep it ≤72 characters and do not prefix it with `Fishhawk:` — the runner adds attribution separately.\n")
	b.WriteString("- Leave one blank line.\n")
	b.WriteString("- The rest is the PR body in markdown. Use these sections (and only these), in this order:\n")
	b.WriteString("  - `## Summary` — 1–3 bullets covering the motivation and the load-bearing changes. Don't restate the diff line-by-line.\n")
	b.WriteString("  - `## Test plan` — a checklist of how a reviewer should verify the change, written as `- [ ] …` items (unchecked; the reviewer ticks them).\n")
	b.WriteString("  - `## Notes` — optional. Use only when there's something deferred, surprising, or non-obvious worth flagging.\n")
	b.WriteString("- Don't add other top-level sections. Don't open the body with prose that floats above the first heading; everything narrative goes under `## Summary`.\n")

	if t.IssueNumber > 0 {
		fmt.Fprintf(&b,
			"- End the body with `Closes #%d` on its own line so merging the PR auto-closes the originating issue.\n",
			t.IssueNumber,
		)
	}

	b.WriteString("\n")
	b.WriteString("When the runner finishes, it will collect the diff, ship the trace bundle to Fishhawk, push your changes to a branch, and open the PR using the title + body you wrote.\n")
	return b.String()
}

func buildPlan(t Trigger) string {
	var b strings.Builder
	b.WriteString("You are drafting an implementation plan for a change in the repository ")
	b.WriteString(quoteRepo(t.Repo))
	b.WriteString(".\n\n")

	if t.DecomposeRequired {
		b.WriteString("IMPORTANT: Your previous plan was rejected because predicted_runtime_minutes " +
			"exceeded the implement-stage budget without a decomposition block. " +
			"You MUST populate decomposition.sub_plans in this plan — omitting it will block approval again.\n\n")
	}

	if t.PriorRejectionFeedback != nil && *t.PriorRejectionFeedback != "" {
		feedback := *t.PriorRejectionFeedback
		const maxFeedbackBytes = 4000
		if len(feedback) > maxFeedbackBytes {
			feedback = feedback[:maxFeedbackBytes] + "...[truncated]"
		}
		b.WriteString("### Prior plan-stage rejection feedback\n\n")
		b.WriteString("The operator rejected the most recent plan for this issue with the following rationale. You MUST address this feedback in your new plan:\n\n")
		b.WriteString(feedback)
		b.WriteString("\n\n")
	}

	writeIssueContext(&b, t)

	planMins := resolveMins(t.PlanStageTimeout)
	implMins := resolveMins(t.ImplementStageTimeout)
	fmt.Fprintf(&b,
		"Stage budget (ADR-025): plan stage %d minutes, implement stage %d minutes. "+
			"Treat overrunning the budget as a scope problem, not a runtime problem — "+
			"if your work estimate exceeds the implement-stage budget, populate decomposition.sub_plans "+
			"so the reviewer can split the work into multiple runs.\n\n",
		planMins, implMins,
	)

	b.WriteString("Your task: produce a `standard_v1` plan artifact describing the change. ")
	b.WriteString("Write the plan as a single JSON object to `")
	b.WriteString(PlanArtifactPath)
	b.WriteString("`. The schema is documented at docs/spec/plan-standard-v1.md and required fields are: plan_version (\"standard_v1\"), ticket_reference, generated_by, summary, scope, approach, verification, predicted_runtime_minutes, predicted_runtime_confidence. ")
	b.WriteString("predicted_runtime_minutes and predicted_runtime_confidence are MUST-populate fields — every plan artifact must carry your runtime estimate and confidence level. ")
	fmt.Fprintf(&b,
		"Populate decomposition.sub_plans if and only if your predicted_runtime_minutes estimate exceeds the implement-stage budget (%d minutes). ",
		implMins,
	)
	b.WriteString("Do not echo the plan in your final response — only write it to the file. ")
	b.WriteString("Do not modify source files in this stage — the implement stage that follows will execute the plan.\n")
	b.WriteString("\n")
	b.WriteString("scope.files shape: each entry MUST be an object with \"path\" and \"operation\" — not a bare string.\n\n")
	b.WriteString("WRONG:\n")
	b.WriteString("  \"files\": [\"backend/internal/foo/foo.go\", \"backend/internal/foo/foo_test.go\"]\n\n")
	b.WriteString("RIGHT:\n")
	b.WriteString("  \"files\": [\n")
	b.WriteString("    {\"path\": \"backend/internal/foo/foo.go\", \"operation\": \"create\"},\n")
	b.WriteString("    {\"path\": \"backend/internal/foo/foo_test.go\", \"operation\": \"modify\"}\n")
	b.WriteString("  ]\n\n")
	b.WriteString("Valid operations: create | modify | delete | rename.\n")
	b.WriteString("\n")
	b.WriteString("Compound-field shape rule: the following fields must be the structured shape shown in the schema — never a bare string or prose summary:\n")
	b.WriteString("- approach: array of {\"step\": N, \"description\": \"...\"} objects\n")
	b.WriteString("- verification: {\"test_strategy\": \"...\", \"rollback_plan\": \"...\"} object\n")
	b.WriteString("- scope: {\"files\": [...]} object\n")
	b.WriteString("- scope.files[i]: {\"path\": \"...\", \"operation\": \"...\"} object\n")
	b.WriteString("- ticket_reference: {\"type\": \"...\", \"url\": \"...\", \"id\": \"...\"} object\n")
	b.WriteString("- generated_by: {\"agent\": \"...\", \"model\": \"...\", \"timestamp\": \"...\"} object\n")
	b.WriteString("- decomposition (when present): {\"rationale\": \"...\", \"sub_plans\": [...]} object — when you are NOT decomposing, OMIT this field entirely; do NOT set it to null\n")
	b.WriteString("- decomposition.sub_plans[i]: {\"title\": \"...\", \"scope_hint\": \"...\", \"predicted_runtime_minutes\": N, \"predicted_runtime_confidence\": \"low|medium|high\"} object — use the FULL canonical field names; \"confidence\" / \"minutes\" shorthand will be rejected\n")
	b.WriteString("The validator rejects any plan where these fields contain bare strings instead of their required structured shapes.\n")
	b.WriteString("\n")
	b.WriteString("Cross-boundary test rule: when scope.files spans multiple architectural layers (request/response " +
		"payload, domain type, persistence, render/consumer), verification.test_strategy MUST name an " +
		"integration/end-to-end test that crosses those layers, not only per-layer unit tests. Per-layer units " +
		"pass while the seam between them breaks (cf. #618).\n")
	b.WriteString("\n")
	b.WriteString("If your plan scope includes any file under docs/spec/, the verification steps must include: " +
		"run scripts/sync-schemas after editing the canonical copy; CI schema-sync gate will catch embedded copies that drift.\n")
	b.WriteString("\n")
	b.WriteString("Citation-or-test rule: for every non-obvious OS, runtime, network, or third-party-API semantic claim in risks_and_assumptions, " +
		"the entry MUST include either a citation (URL, man page, RFC number) or a concrete test that would fail if the assumption is wrong. " +
		"An unsupported claim that looks well-reasoned disarms the reviewer's check reflex — if it sounds plausible, reviewers stop verifying.\n")
	b.WriteString("\n")
	b.WriteString("Counter-examples from production bugs:\n")
	b.WriteString("- SIGKILL and orphan file descriptors: SIGKILL kills only the direct child process; grandchildren that inherited stdout " +
		"via fork keep the pipe writer alive, preventing EOF on the reader. Fix: set syscall.SysProcAttr Setpgid:true and send " +
		"kill(-pgid, SIGKILL). Cited: syscall.SysProcAttr docs (https://pkg.go.dev/syscall#SysProcAttr).\n")
	b.WriteString("- cmd.Wait and pipe-read race: cmd.Wait closes the parent-side pipe file descriptors while a goroutine is still reading " +
		"from them. Fix: drain the pipe before calling Wait, or use io.Pipe indirection. Cited: os/exec.Cmd.Wait docs (https://pkg.go.dev/os/exec#Cmd.Wait).\n")
	b.WriteString("\n")
	b.WriteString("Incremental verification discipline: run relevant tests once after each batch of related changes rather than exhaustively at the end. " +
		"Run golangci-lint on touched packages only (e.g. `golangci-lint run ./internal/foo/...`), not the whole repo. " +
		"Reserve expensive gates (e.g. -count >= 50, full-repo -race) for the final iteration once you are confident the implementation is correct. " +
		"If your plan commits to an expensive step, allocate explicit minutes for it in predicted_runtime_minutes.\n")
	if t.CalibrationHint != nil {
		b.WriteString("\n### Calibration hint\n\n")
		fmt.Fprintf(&b, "Your last %d implement-stage predictions on this workflow: actual p50 = %.1f min, p95 = %.1f min, ratio = %.2f.\n",
			t.CalibrationHint.Samples, t.CalibrationHint.ActualP50Minutes, t.CalibrationHint.ActualP95Minutes, t.CalibrationHint.CalibrationRatio)
		b.WriteString("Confidence-band accuracy:\n")
		for _, level := range []string{"high", "medium", "low"} {
			band, ok := t.CalibrationHint.ConfidenceBands[level]
			if !ok || band.Samples == 0 {
				continue
			}
			fmt.Fprintf(&b, "- %s: %d samples, %d within 1.5x of prediction\n",
				level, band.Samples, band.WithinScale)
		}
		fmt.Fprintf(&b, "Multiply your raw estimate by %.2f to get a calibrated value.\n", t.CalibrationHint.CalibrationRatio)
		if highBand, ok := t.CalibrationHint.ConfidenceBands["high"]; ok && highBand.Samples >= 5 {
			if float64(highBand.WithinScale)/float64(highBand.Samples) <= 0.25 {
				fmt.Fprintf(&b, "→ \"high\" has been the LEAST accurate band historically (%d/%d within 1.5x). "+
					"Reserve \"high\" for genuinely mechanical changes (rename, doc edit). "+
					"Default to \"medium\" when there's any logic, multi-file, or new-code uncertainty.\n",
					highBand.WithinScale, highBand.Samples)
			}
		}
	}
	return b.String()
}

// buildPlanReview constructs the constrained prompt for a plan-review agent.
// The review agent's sole task is to emit a structured verdict JSON — it is
// explicitly forbidden from re-planning, proposing edits, or producing any
// output other than the verdict object.
//
// The prompt surfaces the full plan artifact and the originating issue body
// so the reviewer has the same context as the plan author. Docs references
// (plan schema, spec) are included so the reviewer can validate structure
// without fetching external resources.
func buildPlanReview(t Trigger) string {
	var b strings.Builder
	b.WriteString("You are a plan-review agent for the repository ")
	b.WriteString(quoteRepo(t.Repo))
	b.WriteString(".\n\n")

	b.WriteString("ROLE CONSTRAINT (binding — read before writing any output)\n")
	b.WriteString("===========================================================\n\n")
	b.WriteString("Your ONLY task is to review the plan artifact below and emit a single JSON verdict object.\n")
	b.WriteString("You MUST NOT:\n")
	b.WriteString("- Re-plan, propose alternative plans, or suggest edits to the plan.\n")
	b.WriteString("- Produce any prose output outside the JSON verdict object.\n")
	b.WriteString("- Modify any source files.\n")
	b.WriteString("- Invoke any tools beyond reading repository files for context.\n\n")
	b.WriteString("Your entire response MUST be a single JSON object conforming to the verdict schema below. " +
		"Do not wrap it in markdown code fences, do not add prose before or after it. " +
		"A response that contains anything other than the JSON object will be rejected.\n\n")

	// Plan artifact section — the primary input to the review.
	if t.ApprovedPlan != nil {
		writePlanForReview(&b, t.ApprovedPlan)
	} else {
		b.WriteString("### Plan artifact\n\n")
		b.WriteString("(no plan artifact provided — emit verdict: reject with concern: missing plan artifact)\n\n")
	}

	// Issue context: give the reviewer the originating motivation so
	// they can assess whether the plan actually addresses the issue.
	if t.IssueNumber > 0 || t.IssueTitle != "" || t.IssueBody != "" {
		b.WriteString("### Originating issue\n\n")
		if t.IssueNumber > 0 {
			fmt.Fprintf(&b, "Issue: #%d", t.IssueNumber)
			if t.IssueTitle != "" {
				b.WriteString(" · ")
				b.WriteString(t.IssueTitle)
			}
			b.WriteString("\n")
		} else if t.IssueTitle != "" {
			b.WriteString("Title: ")
			b.WriteString(t.IssueTitle)
			b.WriteString("\n")
		}
		if t.IssueBody != "" {
			b.WriteString("\n")
			b.WriteString(t.IssueBody)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// Verdict schema — inline so the reviewer doesn't need to fetch it.
	// The JSON shape below is load-bearing and kept verbatim; surrounding
	// prose is trimmed to keep the per-call token cost down (#606).
	b.WriteString("### Verdict schema\n\n")
	b.WriteString("Emit exactly this JSON shape; omit `concerns` and `free_form` when empty:\n\n")
	b.WriteString("{\n")
	b.WriteString("  \"verdict\": \"approve\" | \"approve_with_concerns\" | \"reject\",\n")
	b.WriteString("  \"concerns\": [\n")
	b.WriteString("    {\n")
	b.WriteString("      \"severity\": \"high\" | \"medium\" | \"low\",\n")
	b.WriteString("      \"category\": \"<short classifier, e.g. scope | security | correctness | coverage>\",\n")
	b.WriteString("      \"note\": \"<free-form explanation of the concern>\"\n")
	b.WriteString("    }\n")
	b.WriteString("  ],\n")
	b.WriteString("  \"free_form\": \"<optional overall commentary>\"\n")
	b.WriteString("}\n\n")

	// Review criteria — what the agent should assess. Record a concern per gap.
	b.WriteString("### Review criteria\n\n")
	b.WriteString("Record a concern for each gap found:\n\n")
	b.WriteString("1. **Scope completeness**: file list covers all changes implied by the approach; " +
		"create/modify/delete operations accurate.\n")
	b.WriteString("2. **Approach feasibility**: steps actionable, internally consistent, address the issue.\n")
	b.WriteString("3. **Verification adequacy**: test strategy catches load-bearing behaviour; rollback realistic.\n")
	b.WriteString("4. **Risk coverage**: meaningful risks identified; assumptions cited or testable.\n")
	b.WriteString("5. **Schema compliance**: plan conforms to standard_v1 (docs/spec/plan-standard-v1.md).\n")
	b.WriteString("6. **Grounded citations**: any rule you cite — from CLAUDE.md, a style guide, or a project " +
		"convention — MUST be one you can quote verbatim from the context in this prompt or a repository file you " +
		"actually read during this review. Do NOT assert rules from memory; if you cannot verify the rule exists, " +
		"do NOT raise the concern.\n")
	b.WriteString("7. **Cross-boundary integration test**: when the plan adds or changes a field/value that flows " +
		"across layers (request/response payload -> domain type -> persistence -> render/consumer), verify " +
		"verification.test_strategy includes a test that exercises the full path end-to-end, not only per-layer " +
		"unit tests. Flag (concern, category coverage) if any serialization boundary in that data flow is absent " +
		"from scope.files, or if the test strategy is unit-only for a cross-boundary change. Scope this to genuinely " +
		"cross-boundary changes; do not raise it for incidental multi-file changes.\n\n")

	// Verdict decision rule.
	b.WriteString("### Verdict decision rule\n\n")
	b.WriteString("- `approve`: all criteria met or concerns cosmetic.\n")
	b.WriteString("- `approve_with_concerns`: implementable with non-blocking gaps; record each as a concern.\n")
	b.WriteString("- `reject`: one or more blocking problems; record each as a `high`-severity concern.\n\n")

	b.WriteString("Emit your verdict now. JSON only, no surrounding prose.\n")
	return b.String()
}

// buildImplementReview constructs the constrained prompt for an
// implement-review agent (ADR-027 impl 2/2). The agent reviews the diff
// produced by the implement stage against the approved plan and emits a
// single structured verdict JSON — it is forbidden from re-planning,
// proposing edits, or producing any output other than the verdict object.
//
// The prompt surfaces the changed-files diff summary, the approved plan's
// scope.files / approach / verification, and the originating issue body so
// the reviewer can assess whether the diff implements the plan. The
// scope-drift rule is flag-only: files touched outside scope.files yield a
// {category:"scope"} concern, never an auto-reject (ADR-027 Decision Q6).
func buildImplementReview(t Trigger) string {
	var b strings.Builder
	b.WriteString("You are an implement-review agent for the repository ")
	b.WriteString(quoteRepo(t.Repo))
	b.WriteString(".\n\n")

	b.WriteString("ROLE CONSTRAINT (binding — read before writing any output)\n")
	b.WriteString("===========================================================\n\n")
	b.WriteString("Your ONLY task is to review the diff below against the approved plan and emit a single JSON verdict object.\n")
	b.WriteString("You MUST NOT:\n")
	b.WriteString("- Re-plan, propose alternative implementations, or suggest edits to the diff.\n")
	b.WriteString("- Produce any prose output outside the JSON verdict object.\n")
	b.WriteString("- Modify any source files.\n")
	b.WriteString("- Invoke any tools beyond reading repository files for context.\n\n")
	b.WriteString("Your entire response MUST be a single JSON object conforming to the verdict schema below. " +
		"Do not wrap it in markdown code fences, do not add prose before or after it. " +
		"A response that contains anything other than the JSON object will be rejected.\n\n")

	// Diff under review — the primary input. The split marker leads this
	// section so caching adapters can split the stable preamble from the
	// variable diff/plan/issue content.
	b.WriteString("### Diff under review\n\n")
	switch {
	case t.DiffPatch != "":
		// Patch-present path (#585): the full unified-diff hunks are
		// available, so the reviewer CAN inspect added and removed lines
		// directly. Keep the compact changed-files list as an index, then
		// render the real hunks. The honesty caveat is revised for this
		// path — added/removed lines ARE visible, so the reviewer should
		// assess them rather than deferring to a file read.
		if t.Diff != "" {
			b.WriteString("Files changed by the implement stage (path + git status — index for the hunks below):\n\n")
			b.WriteString(t.Diff)
			b.WriteString("\n")
		}
		b.WriteString("Unified diff (the actual hunks the implement stage produced — added lines prefixed `+`, removed lines prefixed `-`):\n\n")
		b.WriteString("```diff\n")
		b.WriteString(t.DiffPatch)
		if !strings.HasSuffix(t.DiffPatch, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("```\n\n")
		b.WriteString("Assess plan adherence, verification, and regressions against these hunks directly: both added and removed lines are visible above. " +
			"READ the surrounding repository files when you need more context than a hunk shows.\n\n")
	case t.Diff != "":
		// Fallback path (older bundles, patch-compute failure, or a
		// size-capped patch the runner dropped): only the changed-files
		// list is available. Keep the original #561 caveat verbatim — the
		// reviewer cannot see line-level content and must read the files.
		b.WriteString("Files changed by the implement stage (path + git status — this is a changed-files list, NOT a line-level diff):\n\n")
		b.WriteString(t.Diff)
		b.WriteString("\nTo assess content (plan adherence, verification, regressions below), READ each listed file from the repository to see its current state. " +
			"Pre-change content and deleted lines are NOT visible from this list, so base any regression concern only on what you can confirm by reading the current files — " +
			"do not assert the absence of regressions you could not actually inspect.\n\n")
	default:
		b.WriteString("(no diff present in the trace bundle — emit verdict: approve with a concern noting the empty diff)\n\n")
	}

	// Approved plan section — what the diff is being measured against.
	if t.ApprovedPlan != nil {
		writePlanForReview(&b, t.ApprovedPlan)
	} else {
		b.WriteString("### Plan artifact\n\n")
		b.WriteString("(no approved plan available — review the diff for obvious regressions only)\n\n")
	}

	// Issue context: the originating motivation.
	if t.IssueNumber > 0 || t.IssueTitle != "" || t.IssueBody != "" {
		b.WriteString("### Originating issue\n\n")
		if t.IssueNumber > 0 {
			fmt.Fprintf(&b, "Issue: #%d", t.IssueNumber)
			if t.IssueTitle != "" {
				b.WriteString(" · ")
				b.WriteString(t.IssueTitle)
			}
			b.WriteString("\n")
		} else if t.IssueTitle != "" {
			b.WriteString("Title: ")
			b.WriteString(t.IssueTitle)
			b.WriteString("\n")
		}
		if t.IssueBody != "" {
			b.WriteString("\n")
			b.WriteString(t.IssueBody)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// Verdict schema — inline so the reviewer doesn't need to fetch it.
	b.WriteString("### Verdict schema\n\n")
	b.WriteString("Emit exactly this JSON shape. All fields shown; omit `concerns` and `free_form` when empty:\n\n")
	b.WriteString("{\n")
	b.WriteString("  \"verdict\": \"approve\" | \"approve_with_concerns\" | \"reject\",\n")
	b.WriteString("  \"concerns\": [\n")
	b.WriteString("    {\n")
	b.WriteString("      \"severity\": \"high\" | \"medium\" | \"low\",\n")
	b.WriteString("      \"category\": \"<short classifier, e.g. scope | correctness | regression | verification>\",\n")
	b.WriteString("      \"note\": \"<free-form explanation of the concern>\"\n")
	b.WriteString("    }\n")
	b.WriteString("  ],\n")
	b.WriteString("  \"free_form\": \"<optional overall commentary>\"\n")
	b.WriteString("}\n\n")

	// Review criteria — what the agent should assess.
	b.WriteString("### Review criteria\n\n")
	b.WriteString("Assess the diff against the following criteria. Record a concern for each gap found:\n\n")
	b.WriteString("1. **Plan adherence**: Does the diff implement the approved plan's approach steps? " +
		"Are the changes the plan called for actually present?\n")
	b.WriteString("2. **Scope adherence (flag-only)**: Does the diff touch files outside the plan's scope.files? " +
		"If so, record a `{category: \"scope\"}` concern naming the out-of-scope files. " +
		"Do NOT reject solely for scope drift — drift is a flag, not a blocker.\n")
	b.WriteString("3. **Verification satisfiability**: Are the plan's verification steps (test strategy, rollback) " +
		"satisfiable against this diff?\n")
	b.WriteString("4. **Obvious regressions**: Does the diff introduce obvious correctness regressions, " +
		"broken references, or removed behaviour the issue didn't ask for?\n")
	b.WriteString("5. **Grounded citations**: Any rule you cite — from CLAUDE.md, a style guide, or a project " +
		"convention — MUST be one you can quote verbatim from the context provided in this prompt or from a " +
		"repository file you actually read during this review. Do NOT assert rules from memory. If you cannot " +
		"verify the rule exists, do NOT raise the concern. Ground every concern in the plan, issue, and diff " +
		"actually provided.\n")
	b.WriteString("6. **Style is out of scope**: Subjective style judgments (comment length, naming aesthetics, " +
		"formatting) are out of scope for review — that is lint's job. Focus on plan adherence, scope " +
		"(flag-only), verification satisfiability, and obvious regressions.\n\n")

	// Verdict decision rule.
	b.WriteString("### Verdict decision rule\n\n")
	b.WriteString("- `approve`: diff implements the plan; all criteria met or concerns are cosmetic.\n")
	b.WriteString("- `approve_with_concerns`: diff is acceptable but has non-blocking gaps (including any scope drift); " +
		"record each gap as a concern with appropriate severity.\n")
	b.WriteString("- `reject`: diff has one or more blocking problems — it does not implement the plan, or it introduces " +
		"a correctness regression — that must be resolved; record each blocker as a `high`-severity concern. " +
		"Scope drift ALONE is never grounds for reject; emit approve_with_concerns instead.\n\n")

	b.WriteString("Emit your verdict now. Remember: JSON only, no surrounding prose.\n")
	return b.String()
}

// writePlanForReview renders a standard_v1 plan for the review-agent prompt.
// It mirrors writeApprovedPlan but uses a neutral "Plan artifact" header
// (the plan is under review, not yet approved).
func writePlanForReview(b *strings.Builder, p *plan.Plan) {
	b.WriteString("### Plan artifact\n\n")

	if p.Summary != "" {
		b.WriteString("Summary:\n")
		b.WriteString(p.Summary)
		b.WriteString("\n\n")
	}

	if len(p.Scope.Files) > 0 {
		b.WriteString("Files in scope:\n")
		for _, f := range p.Scope.Files {
			fmt.Fprintf(b, "- %s (%s)\n", f.Path, f.Operation)
		}
		b.WriteString("\n")
	}

	if len(p.Approach) > 0 {
		b.WriteString("Approach:\n")
		for _, step := range p.Approach {
			fmt.Fprintf(b, "%d. %s\n", step.Step, step.Description)
		}
		b.WriteString("\n")
	}

	if p.Verification.TestStrategy != "" || p.Verification.RollbackPlan != "" {
		b.WriteString("Verification:\n")
		if p.Verification.TestStrategy != "" {
			b.WriteString("- Test strategy: ")
			b.WriteString(p.Verification.TestStrategy)
			b.WriteString("\n")
		}
		if p.Verification.RollbackPlan != "" {
			b.WriteString("- Rollback plan: ")
			b.WriteString(p.Verification.RollbackPlan)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if len(p.RisksAndAssumptions) > 0 {
		b.WriteString("Risks & assumptions:\n")
		for _, r := range p.RisksAndAssumptions {
			fmt.Fprintf(b, "- %s\n", r)
		}
		b.WriteString("\n")
	}

	if p.PredictedRuntimeMinutes > 0 {
		fmt.Fprintf(b, "Runtime prediction: %d minutes (%s confidence)\n\n",
			p.PredictedRuntimeMinutes, p.PredictedRuntimeConfidence)
	}
}

// resolveMins converts a duration to whole minutes, returning
// defaultStageTimeoutMinutes for zero durations.
func resolveMins(d time.Duration) int {
	if d <= 0 {
		return defaultStageTimeoutMinutes
	}
	return int(d.Minutes())
}

// writeApprovedPlan renders a standard_v1 plan as readable prose so
// the agent reads it as instructions rather than as JSON. Sections
// mirror the schema: Summary, Files in scope, Approach, Verification,
// Risks (when present). The rendering is deterministic (slices
// preserve their input order) so two replays of the same stage
// produce byte-identical prompts and the audit log records exactly
// what the agent was asked to do.
func writeApprovedPlan(b *strings.Builder, p *plan.Plan) {
	b.WriteString("Approved plan (binding instruction)\n")
	b.WriteString("===================================\n\n")

	if p.Summary != "" {
		b.WriteString("Summary:\n")
		b.WriteString(p.Summary)
		b.WriteString("\n\n")
	}

	if len(p.Scope.Files) > 0 {
		b.WriteString("Files in scope:\n")
		for _, f := range p.Scope.Files {
			fmt.Fprintf(b, "- %s (%s)\n", f.Path, f.Operation)
		}
		b.WriteString("\n")
	}

	if len(p.Approach) > 0 {
		b.WriteString("Approach:\n")
		for _, step := range p.Approach {
			fmt.Fprintf(b, "%d. %s\n", step.Step, step.Description)
		}
		b.WriteString("\n")
	}

	// Verification fields are required by the schema but render
	// defensively so a future schema relaxation can't crash the
	// builder.
	if p.Verification.TestStrategy != "" || p.Verification.RollbackPlan != "" {
		b.WriteString("Verification:\n")
		if p.Verification.TestStrategy != "" {
			b.WriteString("- Test strategy: ")
			b.WriteString(p.Verification.TestStrategy)
			b.WriteString("\n")
		}
		if p.Verification.RollbackPlan != "" {
			b.WriteString("- Rollback plan: ")
			b.WriteString(p.Verification.RollbackPlan)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if len(p.RisksAndAssumptions) > 0 {
		b.WriteString("Risks & assumptions:\n")
		for _, r := range p.RisksAndAssumptions {
			fmt.Fprintf(b, "- %s\n", r)
		}
		b.WriteString("\n")
	}

	if p.PredictedRuntimeMinutes > 0 {
		fmt.Fprintf(b, "Runtime prediction: %d minutes (%s confidence)\n\n",
			p.PredictedRuntimeMinutes, p.PredictedRuntimeConfidence)
	}
}

// writeIssueLink renders the issue's number, title, and URL — but
// not its body (#244). Used by the implement-stage prompt where
// the plan is the binding instruction and the issue is reference
// material the agent should fetch on demand if it needs detail.
//
// The URL gives the agent a stable handle to the issue's *current*
// state via GitHub's API; the body in Trigger.IssueBody is a
// snapshot from plan-stage trigger time and may be stale by the
// time the implement stage runs. Linking rather than copying also
// keeps the implement-stage prompt smaller — meaningful when the
// plan artifact is the load-bearing instruction.
//
// Empty IssueNumber produces "(no issue context provided)" — same
// fallback as writeIssueContext for non-issue-triggered runs.
func writeIssueLink(b *strings.Builder, t Trigger) {
	if t.IssueNumber == 0 && t.IssueTitle == "" && t.IssueURL == "" {
		b.WriteString("(no issue context provided)\n\n")
		return
	}
	if t.IssueNumber > 0 {
		fmt.Fprintf(b, "Triggering issue: #%d", t.IssueNumber)
		if t.IssueTitle != "" {
			b.WriteString(" · ")
			b.WriteString(t.IssueTitle)
		}
		b.WriteString("\n")
	} else if t.IssueTitle != "" {
		b.WriteString("Title: ")
		b.WriteString(t.IssueTitle)
		b.WriteString("\n")
	}
	if t.IssueURL != "" {
		b.WriteString("URL: ")
		b.WriteString(t.IssueURL)
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

// writeIssueContext renders the issue title and body into the
// prompt. Empty title/body produces "(no issue context provided)";
// the agent then has to ask clarifying questions, which is the
// right behavior for v0 (better than fabricating intent).
//
// Used by the plan-stage prompt where the agent reads the issue
// directly to construct a plan. The implement stage uses
// writeIssueLink instead — the plan is the binding instruction
// there and the issue body is redundant (#244).
func writeIssueContext(b *strings.Builder, t Trigger) {
	if t.IssueNumber == 0 && t.IssueTitle == "" && t.IssueBody == "" {
		b.WriteString("(no issue context provided)\n\n")
		return
	}
	if t.IssueNumber > 0 {
		fmt.Fprintf(b, "Triggering issue: #%d\n", t.IssueNumber)
	}
	if t.IssueTitle != "" {
		b.WriteString("Title: ")
		b.WriteString(t.IssueTitle)
		b.WriteString("\n")
	}
	if t.IssueBody != "" {
		b.WriteString("\n")
		b.WriteString(t.IssueBody)
		b.WriteString("\n")
	}
	writeIssueComments(b, t.IssueComments)
	b.WriteString("\n")
}

// writeIssueComments renders the issue's comments into the plan-stage
// prompt after the body (#618). Comment-borne refinements/decisions
// would otherwise never reach the plan agent, which only saw
// title+body — the #616 case where a refinement posted as a comment
// needed a reject->re-plan to land.
//
// Rules:
//   - Drop comments whose author login ends in `[bot]` (CI bots and
//     Fishhawk's own #377 footer) so the agent doesn't read its own
//     output back as new guidance.
//   - Render survivors chronologically (oldest->newest), each prefixed
//     with the author login and timestamp.
//   - Cap each comment body (maxCommentBytes) and the total rendered
//     size (maxTotalCommentBytes). When over the total budget, drop the
//     OLDEST comments first — recency is load-bearing because a later
//     comment may supersede the body — and prepend an omission marker.
//   - Lead with a one-line preface that later comments may supersede
//     the body.
//
// Renders nothing when no comments survive the bot filter (the
// body-only fallback is unchanged).
func writeIssueComments(b *strings.Builder, comments []IssueComment) {
	surviving := make([]IssueComment, 0, len(comments))
	for _, c := range comments {
		if strings.HasSuffix(c.Author, "[bot]") {
			continue
		}
		surviving = append(surviving, c)
	}
	if len(surviving) == 0 {
		return
	}

	// Per-comment cap so a single long comment can't dominate the
	// budget. Same capped-injection style as PriorRejectionFeedback's
	// maxFeedbackBytes.
	const maxCommentBytes = 2000
	rendered := make([]string, len(surviving))
	for i, c := range surviving {
		body := c.Body
		if len(body) > maxCommentBytes {
			body = body[:maxCommentBytes] + "...[truncated]"
		}
		author := c.Author
		if author == "" {
			author = "unknown"
		}
		rendered[i] = fmt.Sprintf("**@%s** (%s):\n%s", author, c.CreatedAt, body)
	}

	// Total budget: walk newest->oldest accumulating bytes; once over
	// budget, drop everything older (recency is load-bearing). The
	// newest comment always survives — the per-comment cap is below the
	// total budget.
	const maxTotalCommentBytes = 12000
	start := 0
	total := 0
	for i := len(rendered) - 1; i >= 0; i-- {
		total += len(rendered[i])
		if total > maxTotalCommentBytes {
			start = i + 1
			break
		}
	}

	b.WriteString("\n### Issue comments\n\n")
	b.WriteString("The issue has the following comments (oldest first). A later comment may refine or supersede the issue body above — weigh the most recent guidance accordingly.\n\n")
	if start > 0 {
		fmt.Fprintf(b, "[%d older comment(s) omitted to fit budget]\n\n", start)
	}
	for i := start; i < len(rendered); i++ {
		b.WriteString(rendered[i])
		b.WriteString("\n\n")
	}
}

// quoteRepo backticks an "owner/name" string for inline display.
// Empty input returns "this repository" so prompts read cleanly
// even when the run is missing a repo (e.g., synthetic test runs).
func quoteRepo(repo string) string {
	if repo == "" {
		return "this repository"
	}
	return "`" + repo + "`"
}
