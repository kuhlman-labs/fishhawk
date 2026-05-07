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

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

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
	// even for issue triggers (issue created with no body).
	IssueBody string
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
		b.WriteString("Originating issue (background context only):\n\n")
		writeIssueContext(&b, t)

		b.WriteString("Your task: implement the approved plan above. The plan is the binding instruction; the issue is included for grounding when the plan is ambiguous. Make the smallest set of changes that satisfies the plan.\n")
		b.WriteString("\n")
		b.WriteString("If you discover the plan is wrong or infeasible — a file it names doesn't exist, an approach step is incompatible with the current code, the verification can't be implemented as specified — stop and surface that in your final response rather than diverging silently. The right path in that case is a follow-up run that re-plans, not an off-plan implementation.\n")
		b.WriteString("\n")
		b.WriteString("If the repository has materially changed since the plan was approved (files in the plan's scope have been heavily refactored, an approach step references code that no longer exists), surface that and pause.\n")
		b.WriteString("\n")
	} else {
		writeIssueContext(&b, t)
		b.WriteString("Your task: implement the change described above. Make the smallest set of changes that satisfies the issue.\n")
		b.WriteString("\n")
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

	writeIssueContext(&b, t)

	b.WriteString("Your task: produce a `standard_v1` plan artifact describing the change. ")
	b.WriteString("Write the plan as a single JSON object to `")
	b.WriteString(PlanArtifactPath)
	b.WriteString("`. The schema is documented at docs/spec/plan-standard-v1.md and required fields are: plan_version (\"standard_v1\"), ticket_reference, generated_by, summary, scope, approach, verification. ")
	b.WriteString("Do not echo the plan in your final response — only write it to the file. ")
	b.WriteString("Do not modify source files in this stage — the implement stage that follows will execute the plan.\n")
	return b.String()
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
}

// writeIssueContext renders the issue title and body into the
// prompt. Empty title/body produces "(no issue context provided)";
// the agent then has to ask clarifying questions, which is the
// right behavior for v0 (better than fabricating intent).
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
