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

	writeIssueContext(&b, t)

	b.WriteString("Your task: implement the change described above. Make the smallest set of changes that satisfies the issue.\n")
	b.WriteString("\n")

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
	b.WriteString("- The rest is the PR body in markdown. Lead with the motivation, then describe what changed, then list anything a reviewer should test or watch for. Avoid restating the diff line by line.\n")

	if t.IssueNumber > 0 {
		fmt.Fprintf(&b,
			"- Include the line `Closes #%d` somewhere in the body so merging the PR auto-closes the originating issue.\n",
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
