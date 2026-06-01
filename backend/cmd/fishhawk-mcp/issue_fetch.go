package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ghIssueCommand is the subprocess the MCP server shells to when
// fetching the triggering issue (#415). Exposed as a var so tests
// can swap in a recording fake without actually calling `gh`.
// Production wires exec.Command.
var ghIssueCommand = exec.Command

// ghLookPath is the test seam for `gh` PATH resolution. Tests can
// swap this to make the binary appear missing without manipulating
// the process-wide PATH env.
var ghLookPath = exec.LookPath

// resolveIssueRef parses the agent's `issue` argument into an
// issue number. Mirrors the CLI's accepted forms:
//
//	1247
//	#1247
//	https://github.com/owner/repo/issues/1247
//	https://github.com/owner/repo/issues/1247#issuecomment-...
//
// Numbers must be > 0. Returns 0 + nil for an empty input so
// callers can branch on "agent didn't pass anything."
func resolveIssueRef(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}
	s = strings.TrimPrefix(s, "#")
	if i := strings.Index(s, "/issues/"); i >= 0 {
		s = s[i+len("/issues/"):]
		if h := strings.IndexAny(s, "#?"); h >= 0 {
			s = s[:h]
		}
		s = strings.TrimSuffix(s, "/")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("issue %q: not a number, #N, or .../issues/N URL", raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("issue %q: number must be > 0", raw)
	}
	return n, nil
}

// ghIssue is the subset of `gh issue view --json
// title,body,url,number,comments` output the MCP server consumes. gh
// emits camelCase; mirror it verbatim so the JSON decoder picks the
// right keys. Each comment carries a nested `author` object (we read
// its `login`), plus `body` and `createdAt`.
type ghIssue struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	URL      string `json:"url"`
	Number   int    `json:"number"`
	Comments []struct {
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		Body      string `json:"body"`
		CreatedAt string `json:"createdAt"`
	} `json:"comments"`
}

// fetchIssueViaGh shells to `gh issue view N --repo owner/name
// --json title,body,url,number,comments` and returns the parsed
// result.
//
// Best-effort by design — when `gh` is missing, unauthed, or the
// repo blocks the operator, the startRun handler logs a warning to
// the tool's structured output and proceeds without the cached
// payload. The prompt then falls through to the "URL only" shape,
// which is the pre-#415 behavior for runs without an
// installation_id.
//
// Returns (nil, ErrGhNotInstalled) when the binary itself isn't
// on PATH so the caller can distinguish "operator hasn't set up
// gh" from "the issue genuinely isn't reachable."
func fetchIssueViaGh(repo string, issueNumber int) (*IssueContext, error) {
	if _, err := ghLookPath("gh"); err != nil {
		return nil, ErrGhNotInstalled
	}
	cmd := ghIssueCommand("gh", "issue", "view", strconv.Itoa(issueNumber),
		"--repo", repo, "--json", "title,body,url,number,comments")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("gh issue view: %s",
				strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("gh issue view: %w", err)
	}
	var iss ghIssue
	if err := json.Unmarshal(out, &iss); err != nil {
		return nil, fmt.Errorf("gh issue view: decode: %w", err)
	}
	ic := &IssueContext{
		Title:  iss.Title,
		Body:   iss.Body,
		URL:    iss.URL,
		Number: iss.Number,
	}
	for _, c := range iss.Comments {
		ic.Comments = append(ic.Comments, IssueComment{
			Author:    c.Author.Login,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		})
	}
	return ic, nil
}

// ErrGhNotInstalled signals the `gh` binary is missing on the
// operator's PATH. Callers warn and proceed without the cached
// payload (degraded prompt) rather than failing the tool call.
var ErrGhNotInstalled = errors.New("gh CLI not found on PATH; install https://cli.github.com to enable inline issue fetch")

// inferIssueNumberFromTriggerRef extracts N from a trigger_ref of
// the form `issue:N`. Returns 0 when the ref is empty or in any
// other shape — auto-derive is a best-effort fallback, not a
// validation surface; the caller decides whether to error.
func inferIssueNumberFromTriggerRef(triggerRef string) int {
	s := strings.TrimSpace(triggerRef)
	if !strings.HasPrefix(s, "issue:") {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(s, "issue:"))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
