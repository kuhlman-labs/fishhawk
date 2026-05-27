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
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedStage, stageType)
	}
}

func buildImplement(t Trigger) string {
	var b strings.Builder
	b.WriteString("You are implementing a change in the repository ")
	b.WriteString(quoteRepo(t.Repo))
	b.WriteString(".\n\n")

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
		fmt.Fprintf(&b, "Your last %d implement-stage predictions on this workflow: actual runtime was %.2fx of predicted (actual / predicted).\n",
			t.CalibrationHint.Samples, t.CalibrationHint.CalibrationRatio)
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
	}
	return b.String()
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
	b.WriteString("\n")
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
