package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// ghIssueCommand is the subprocess `fishhawk run start` shells to
// when fetching the triggering issue (#415). Exposed as a var so
// tests can swap in a recording fake without actually calling gh.
// Production wires exec.Command.
var ghIssueCommand = exec.Command

// resolveIssueRef parses the operator's --issue argument into an
// issue number. Accepted forms:
//
//	1247
//	#1247
//	https://github.com/owner/repo/issues/1247
//	https://github.com/owner/repo/issues/1247#issuecomment-...
//
// Numbers must be > 0. Returns 0 + nil for an empty input so
// callers can branch on "user didn't pass anything."
func resolveIssueRef(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}
	// Strip a leading '#' on bare references — github's UI uses
	// that form, the CLI accepts the same shorthand.
	s = strings.TrimPrefix(s, "#")
	// URL form: pull the segment after `/issues/`. Drop any
	// fragment / trailing comment anchor.
	if i := strings.Index(s, "/issues/"); i >= 0 {
		s = s[i+len("/issues/"):]
		if h := strings.IndexAny(s, "#?"); h >= 0 {
			s = s[:h]
		}
		s = strings.TrimSuffix(s, "/")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("--issue %q: not a number, #N, or .../issues/N URL", raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("--issue %q: number must be > 0", raw)
	}
	return n, nil
}

// ghIssue is the subset of `gh issue view --json title,body,url,number`
// output the CLI consumes. gh emits camelCase; mirror it verbatim
// so the JSON decoder picks the right keys.
type ghIssue struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
	Number int    `json:"number"`
}

// fetchIssueViaGh shells to `gh issue view N --repo owner/name
// --json title,body,url,number` and returns the parsed result.
//
// Best-effort by design — when `gh` is missing, unauthed, or the
// repo blocks the operator, runStart warns to stderr and proceeds
// without the cached payload. The prompt then falls through to
// the "URL only" shape, which is the pre-#415 behavior for runs
// without an installation_id. This is preferable to failing the
// run-create: the operator still gets a working run, just with
// the older degraded prompt.
//
// Returns (nil, ErrGhNotInstalled) when the binary itself isn't
// on PATH so the caller can distinguish "operator hasn't set up
// gh" from "the issue genuinely isn't reachable."
func fetchIssueViaGh(repo string, issueNumber int) (*httpclient.IssueContext, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, ErrGhNotInstalled
	}
	cmd := ghIssueCommand("gh", "issue", "view", strconv.Itoa(issueNumber),
		"--repo", repo, "--json", "title,body,url,number")
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
	return &httpclient.IssueContext{
		Title:  iss.Title,
		Body:   iss.Body,
		URL:    iss.URL,
		Number: iss.Number,
	}, nil
}

// ErrGhNotInstalled signals the `gh` binary is missing on the
// operator's PATH. Callers warn and proceed without the cached
// payload (degraded prompt) rather than failing the verb.
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
