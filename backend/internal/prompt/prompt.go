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
	b.WriteString("When you're done, the runner will collect the diff and ship the trace bundle to Fishhawk for review.\n")
	return b.String()
}

func buildPlan(t Trigger) string {
	var b strings.Builder
	b.WriteString("You are drafting an implementation plan for a change in the repository ")
	b.WriteString(quoteRepo(t.Repo))
	b.WriteString(".\n\n")

	writeIssueContext(&b, t)

	b.WriteString("Your task: produce a `standard_v1` plan artifact describing the change. Do not modify source files in this stage — the implement stage that follows will execute the plan.\n")
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
