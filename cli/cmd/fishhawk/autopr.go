package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// prDescriptionPath is where the runner agent writes the PR title and
// body. The autoOpenPR function reads this file; override in tests.
var prDescriptionPath = "/tmp/fishhawk-pr.md"

// scopeFilePath is where the runner writes the implement stage's
// resolved plan scope.files (see writeScopeHandoff in
// runner/cmd/fishhawk-runner/main.go). autoOpenPR reads this to bound
// staging to the declared paths instead of `git add -A`; override in
// tests. Missing or empty falls back to `git add -A` (#581).
var scopeFilePath = "/tmp/fishhawk-scope.json"

// scopeHandoffFile is the JSON the runner writes to scopeFilePath. It is
// field-for-field compatible with the runner's scopeHandoff type — this
// is the runner↔CLI wire contract for #581, not a JSON Schema.
type scopeHandoffFile struct {
	Files []scopeFileEntry `json:"files"`
}

// scopeFileEntry mirrors one standard_v1 plan scope.files entry: a
// declared repo-relative path plus its per-file operation.
type scopeFileEntry struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
}

// autoGitCommand constructs git subprocesses for autoOpenPR.
// Exposed as a var so tests can inject stubs without spawning real git.
var autoGitCommand = exec.Command

// autoGhCommand constructs gh subprocesses for autoOpenPR.
// Exposed as a var so tests can inject stubs without spawning real gh.
var autoGhCommand = exec.Command

const autoPRFooter = "---\n*Opened automatically by Fishhawk.*"

// conventionalCommitHeaderRe matches a Conventional Commits v1.0.0 header
// (#1572): a lowercase type from the allowed set, an optional lowercase scope in
// parens, an optional breaking-change `!`, then `: ` and a non-empty
// description. Applied WARN-ONLY to the agent-authored PR title — a non-match
// emits pr_template_warning and the title is used verbatim; it never rewrites
// the title or fails. Mirrors the runner's conventionalCommitHeaderRe.
var conventionalCommitHeaderRe = regexp.MustCompile(`^(feat|fix|docs|refactor|test|chore|perf|build)(\([a-z0-9/._-]+\))?!?: .+$`)

// shortID strips hyphens from id.String() and returns the leading 8
// hex characters. Matches the runner/internal/gitops convention.
func shortID(id uuid.UUID) string {
	s := strings.ReplaceAll(id.String(), "-", "")
	return s[:8]
}

// implementCommitMessageDir is the directory the run/stage-keyed INITIAL-implement
// commit-message sidecar lives in (#1686). var (not const) so tests can redirect
// it to a t.TempDir, avoiding /tmp pollution / parallel-test races — the same seam
// pattern as prDescriptionPath.
var implementCommitMessageDir = "/tmp"

// implementCommitMessagePath mirrors prompt.ImplementCommitMessagePath in the
// backend and the runner's implementCommitMessagePath: the run/stage-keyed path
// the initial implement agent writes its clean Conventional-Commits commit message
// to and the CLI reads it from on the local --no-pr commit path (#1686). The
// format string is hardcoded in all three independent modules by design — a
// one-sided edit is caught by the prompt-render test plus the runner and CLI load
// tests, which each assert the byte-identical literal for the same ids.
func implementCommitMessagePath(runID, stageID string) string {
	return implementCommitMessageDir + "/" + fmt.Sprintf("fishhawk-implement-commitmsg-%s-%s.txt", runID, stageID)
}

// loadImplementCommitMessage reads the agent's initial-implement commit-message
// sidecar (#1686) and splits it into (subject, body). It deletes the file on
// EVERY return path (delete-after-read) so a stale sidecar can never bleed into a
// later run/stage. Returns ok=false when the sidecar is absent, unreadable, or
// empty/whitespace-only — the fallback cases the caller resolves to today's
// title + "\n\n" + body. On success the first line is the subject and the
// remainder after it is the body. The runner subprocess already swept a stale
// sidecar pre-invoke, so this path only READS.
func loadImplementCommitMessage(runID, stageID string) (subject, body string, ok bool) {
	path := implementCommitMessagePath(runID, stageID)
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		// Absent sidecar is the common no-op (an older agent wrote nothing); any
		// other read error is fail-closed too.
		return "", "", false
	}
	// A present sidecar is consumed regardless of outcome: remove it on every
	// return path so a stale sidecar is never reused by a later run/stage.
	defer func() { _ = os.Remove(path) }()

	text := strings.TrimSpace(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	if text == "" {
		_, _ = fmt.Fprintf(os.Stderr,
			`{"event":"implement_commitmsg_empty","path":%q}`+"\n", path)
		return "", "", false
	}
	lines := strings.SplitN(text, "\n", 2)
	subject = strings.TrimSpace(lines[0])
	if len(lines) == 2 {
		body = strings.TrimSpace(lines[1])
	}
	return subject, body, true
}

// implementCommitMessage resolves the commit message for the INITIAL implement
// commit on the local --no-pr path (#1686): the agent-authored clean sidecar
// (consumed + deleted by loadImplementCommitMessage), falling back to EXACTLY
// today's title + "\n\n" + body when the sidecar is absent/empty — no synthetic
// subject, so an older agent that writes no sidecar sees no behavior change.
// title/body are the PR title/body sourced unchanged from /tmp/fishhawk-pr.md.
func implementCommitMessage(runID, stageID, title, body string) string {
	if subject, sidecarBody, ok := loadImplementCommitMessage(runID, stageID); ok {
		// Warn-only conventional-commit header check on the sidecar subject,
		// matching the PR-title warn — advisory, never a rewrite or hard failure.
		if !conventionalCommitHeaderRe.MatchString(subject) {
			_, _ = fmt.Fprintf(os.Stderr,
				`{"event":"implement_commitmsg_warning","reason":%q}`+"\n",
				"sidecar subject is not a conventional-commit header")
		}
		if sidecarBody == "" {
			return subject
		}
		return subject + "\n\n" + sidecarBody
	}
	return title + "\n\n" + body
}

// autoOpenPRArgs is the input bag for autoOpenPR.
type autoOpenPRArgs struct {
	WorkingDir string
	RunID      uuid.UUID
	StageID    uuid.UUID
	GitHubRepo string
	BaseBranch string
	// DecomposedFrom, when non-nil, routes this child run onto the shared
	// parent branch fishhawk/run-<shortID> instead of the per-stage branch.
	DecomposedFrom *uuid.UUID
}

// autoOpenPRResult holds the outputs of a successful autoOpenPR call.
type autoOpenPRResult struct {
	Branch   string
	HeadSHA  string
	PRNumber int
	PRURL    string
}

// parsePRDescriptionFile reads the agent-authored PR description at
// path and returns the title and body. The first non-blank line is the
// title; everything after the first blank separator line is the body.
//
// Falls back to a generated title ("chore: fishhawk implement stage
// <shortRunID>") and empty body when the file is missing or the first
// line is whitespace-only. The attribution footer is always appended.
// A non-conventional agent title is used verbatim but flagged with a
// warn-only pr_template_warning (#1572).
func parsePRDescriptionFile(path, runID string) (title, body string, err error) {
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return prFallbackTitle(runID), autoPRFooter, nil
		}
		return "", "", fmt.Errorf("read pr description: %w", readErr)
	}

	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return prFallbackTitle(runID), autoPRFooter, nil
	}

	title = strings.TrimSpace(lines[0])

	// Warn-only conventional-commit header check (#1572): the title doubles as
	// the commit subject, so nudge agents toward the Conventional Commits v1.0.0
	// shape. A non-match is advisory — emit pr_template_warning and use the title
	// VERBATIM. Never a hard failure, never a rewrite. Mirrors the runner.
	if !conventionalCommitHeaderRe.MatchString(title) {
		_, _ = fmt.Fprintf(os.Stderr,
			`{"event":"pr_template_warning","reason":%q,"path":%q}`+"\n",
			"title is not a conventional-commit header", path)
	}

	// Skip the blank separator line following the title.
	start := 1
	if start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	var bodyLines []string
	if start < len(lines) {
		bodyLines = lines[start:]
	}
	rawBody := strings.TrimRight(strings.Join(bodyLines, "\n"), "\n")
	if rawBody != "" {
		body = rawBody + "\n\n" + autoPRFooter
	} else {
		body = autoPRFooter
	}
	return title, body, nil
}

func prFallbackTitle(runID string) string {
	s := strings.ReplaceAll(runID, "-", "")
	if len(s) > 8 {
		s = s[:8]
	}
	return "chore: fishhawk implement stage " + s
}

// autoOpenPR orchestrates the implement-stage PR flow:
//  1. Parse the agent's PR description file.
//  2. Derive a branch name from the run/stage IDs (or shared parent branch
//     for decomposed children).
//  3. Create the branch (or rebase onto existing shared branch), commit all
//     staged changes, and push.
//  4. Open a GitHub PR via gh pr create (skipped for subsequent children
//     when a PR already exists on the shared branch).
//  5. Ship the PR artifact to the backend via ShipLocalPullRequest (skipped
//     for subsequent children).
//
// A clean working tree (nothing to commit) is a warning, not a
// failure — autoOpenPR pushes the existing HEAD and continues.
func autoOpenPR(ctx context.Context, client *httpclient.Client, args autoOpenPRArgs) (*autoOpenPRResult, error) {
	shortRunID := shortID(args.RunID)
	shortStageID := shortID(args.StageID)

	title, body, err := parsePRDescriptionFile(prDescriptionPath, args.RunID.String())
	if err != nil {
		return nil, fmt.Errorf("autopr: %w", err)
	}

	// Branch routing: decomposed children share a single parent branch.
	var (
		branch       string
		isDecomposed bool
		isSubsequent bool
	)
	if args.DecomposedFrom != nil {
		isDecomposed = true
		branch = "fishhawk/run-" + shortID(*args.DecomposedFrom)
		// Detect first vs subsequent by probing the remote tracking ref.
		showRefOut, showRefErr := autoGitCommand("git", "-C", args.WorkingDir,
			"show-ref", "--verify", "refs/remotes/origin/"+branch).CombinedOutput()
		_ = showRefOut
		isSubsequent = showRefErr == nil
	} else {
		branch = "fishhawk/run-" + shortRunID + "/stage-" + shortStageID
	}

	if isSubsequent {
		// Shared branch already exists on the remote: stash agent edits,
		// fetch+rebase, restore, then commit on top.
		if out, stashErr := autoGitCommand("git", "-C", args.WorkingDir,
			"stash", "--include-untracked").CombinedOutput(); stashErr != nil {
			return nil, fmt.Errorf("autopr: git stash: %w\n%s", stashErr, out)
		}
		if out, fetchErr := autoGitCommand("git", "-C", args.WorkingDir,
			"fetch", "origin", branch).CombinedOutput(); fetchErr != nil {
			return nil, fmt.Errorf("autopr: git fetch: %w\n%s", fetchErr, out)
		}
		if out, coErr := autoGitCommand("git", "-C", args.WorkingDir,
			"checkout", branch).CombinedOutput(); coErr != nil {
			return nil, fmt.Errorf("autopr: git checkout %s: %w\n%s", branch, coErr, out)
		}
		if out, rebaseErr := autoGitCommand("git", "-C", args.WorkingDir,
			"pull", "--rebase", "origin", branch).CombinedOutput(); rebaseErr != nil {
			return nil, fmt.Errorf("autopr: git pull --rebase: %w\n%s", rebaseErr, out)
		}
		if out, popErr := autoGitCommand("git", "-C", args.WorkingDir,
			"stash", "pop").CombinedOutput(); popErr != nil {
			return nil, fmt.Errorf("autopr: git stash pop: %w\n%s", popErr, out)
		}
	} else {
		if out, chkErr := autoGitCommand("git", "-C", args.WorkingDir,
			"checkout", "-b", branch).CombinedOutput(); chkErr != nil {
			return nil, fmt.Errorf("autopr: git checkout -b %s: %w\n%s", branch, chkErr, out)
		}
	}

	// Bound staging to the approved plan's scope.files when the runner
	// handed them off; otherwise fall back to `git add -A`. Scope-bounded
	// staging excludes dirty-but-undeclared paths (stray dev .pid files,
	// editor scratch, unrelated local edits) from the Fishhawk-attributed
	// commit, warning about them as scope drift rather than blocking (#581).
	if scopeFiles := readScopeFiles(scopeFilePath); len(scopeFiles) > 0 {
		drift, scopeErr := stageScopedAuto(args.WorkingDir, scopeFiles)
		if scopeErr != nil {
			return nil, fmt.Errorf("autopr: %w", scopeErr)
		}
		if len(drift) > 0 {
			_, _ = fmt.Fprintf(os.Stderr,
				"autopr: scope_drift: %d undeclared dirty path(s) excluded from commit: %s\n",
				len(drift), strings.Join(drift, ", "))
		}
	} else if out, addErr := autoGitCommand("git", "-C", args.WorkingDir, "add", "-A").CombinedOutput(); addErr != nil {
		return nil, fmt.Errorf("autopr: git add -A: %w\n%s", addErr, out)
	}

	// Initial implement commit message (#1686): prefer the agent's clean
	// Conventional-Commits sidecar (delete-after-read), falling back to today's
	// title + "\n\n" + body when absent — so the local --no-pr commit no longer
	// stuffs the whole PR review body into its message. The PR title/body still
	// come from /tmp/fishhawk-pr.md unchanged.
	commitMsg := implementCommitMessage(args.RunID.String(), args.StageID.String(), title, body)
	commitOut, commitErr := autoGitCommand("git", "-C", args.WorkingDir,
		"commit", "--signoff", "-m", commitMsg).CombinedOutput()
	if commitErr != nil {
		if strings.Contains(string(commitOut), "nothing to commit") {
			_, _ = fmt.Fprintf(os.Stderr,
				"autopr: nothing to commit in %s; pushing existing HEAD\n", args.WorkingDir)
		} else {
			return nil, fmt.Errorf("autopr: git commit: %w\n%s", commitErr, commitOut)
		}
	}

	// Decomposed children always push with --force-with-lease so concurrent
	// pushes to the shared branch are rejected rather than silently overwritten.
	var pushArgs []string
	if isDecomposed {
		pushArgs = []string{"git", "-C", args.WorkingDir, "push", "--force-with-lease", "origin", branch}
	} else {
		pushArgs = []string{"git", "-C", args.WorkingDir, "push", "-u", "origin", branch}
	}
	if out, pushErr := autoGitCommand(pushArgs[0], pushArgs[1:]...).CombinedOutput(); pushErr != nil {
		return nil, fmt.Errorf("autopr: git push: %w\n%s", pushErr, out)
	}

	headOut, err := autoGitCommand("git", "-C", args.WorkingDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("autopr: git rev-parse HEAD: %w", err)
	}
	headSHA := strings.TrimSpace(string(headOut))

	// Subsequent decomposed children: PR was already opened by the first child.
	// Skip gh pr create and ShipLocalPullRequest — the shared branch and PR URL
	// are stable from the first child's artifact.
	if isSubsequent {
		_, _ = fmt.Fprintf(os.Stderr,
			"autopr: subsequent decomposed child: pushed to shared branch %s\n", branch)
		return &autoOpenPRResult{Branch: branch, HeadSHA: headSHA}, nil
	}

	var baseSHA string
	if baseOut, baseErr := autoGitCommand("git", "-C", args.WorkingDir,
		"rev-parse", "origin/"+args.BaseBranch).Output(); baseErr == nil {
		baseSHA = strings.TrimSpace(string(baseOut))
	}

	var filesChanged int
	diffRange := "origin/" + args.BaseBranch + "...HEAD"
	if diffOut, diffErr := autoGitCommand("git", "-C", args.WorkingDir,
		"diff", "--name-only", diffRange).Output(); diffErr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(diffOut)), "\n") {
			if strings.TrimSpace(line) != "" {
				filesChanged++
			}
		}
	}

	prOut, err := autoGhCommand("gh", "pr", "create",
		"--repo", args.GitHubRepo,
		"--title", title,
		"--body", body,
		"--base", args.BaseBranch).Output()
	if err != nil {
		return nil, fmt.Errorf("autopr: gh pr create: %w", err)
	}

	prURL, prNumber, err := parsePRCreateOutput(string(prOut))
	if err != nil {
		return nil, fmt.Errorf("autopr: %w", err)
	}

	if _, shipErr := client.ShipLocalPullRequest(ctx, args.RunID, args.StageID,
		httpclient.ShipLocalPullRequestInput{
			PRNumber:          prNumber,
			PRURL:             prURL,
			Branch:            branch,
			HeadSHA:           headSHA,
			BaseSHA:           baseSHA,
			Title:             title,
			Body:              body,
			FilesChangedCount: filesChanged,
		}); shipErr != nil {
		return nil, fmt.Errorf("autopr: ship pr: %w", shipErr)
	}

	return &autoOpenPRResult{
		Branch:   branch,
		HeadSHA:  headSHA,
		PRNumber: prNumber,
		PRURL:    prURL,
	}, nil
}

// parsePRCreateOutput extracts the PR URL from the output of
// `gh pr create`. gh emits the URL as a standalone line; the PR
// number is the trailing path segment.
func parsePRCreateOutput(out string) (prURL string, prNumber int, err error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "https://") && strings.Contains(line, "/pull/") {
			parts := strings.Split(line, "/")
			if len(parts) > 0 {
				n, parseErr := strconv.Atoi(parts[len(parts)-1])
				if parseErr == nil && n > 0 {
					return line, n, nil
				}
			}
		}
	}
	return "", 0, fmt.Errorf("no pull request URL found in gh output: %q", strings.TrimSpace(out))
}

// readScopeFiles reads the runner-written scope handoff at path and
// returns the declared repo-relative paths. Returns nil — triggering
// the `git add -A` fallback in autoOpenPR — when the file is missing,
// empty, unparseable, or declares no paths.
func readScopeFiles(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var h scopeHandoffFile
	if jsonErr := json.Unmarshal(data, &h); jsonErr != nil {
		return nil
	}
	var paths []string
	for _, f := range h.Files {
		if f.Path != "" {
			paths = append(paths, f.Path)
		}
	}
	return paths
}

// stageScopedAuto stages exactly the declared scope paths in workingDir
// and returns the set of dirty-but-undeclared paths (scope drift). It
// reads `git status --porcelain` to enumerate every dirty path, stages
// the ones that match a declared path via a single `git add -A -- <paths>`
// (per-path -A covers create, modify, AND delete), and returns the
// remainder as drift. Drift paths are never staged. Staging only paths
// git already reports dirty means `git add` never errors on a pathspec
// matching nothing. Mirrors gitops.Pusher.StageScoped so the CLI auto-PR
// path and the runner-internal push path bound the commit identically.
func stageScopedAuto(workingDir string, scopeFiles []string) (drift []string, err error) {
	statusOut, err := autoGitCommand("git", "-C", workingDir, "status", "--porcelain").Output()
	if err != nil {
		return nil, fmt.Errorf("git status --porcelain: %w", err)
	}
	declared := make(map[string]bool, len(scopeFiles))
	for _, f := range scopeFiles {
		declared[f] = true
	}
	var toStage []string
	for _, line := range strings.Split(string(statusOut), "\n") {
		path := porcelainPath(line)
		if path == "" {
			continue
		}
		if declared[path] {
			toStage = append(toStage, path)
		} else {
			drift = append(drift, path)
		}
	}
	if len(toStage) > 0 {
		addArgs := append([]string{"-C", workingDir, "add", "-A", "--"}, toStage...)
		if out, addErr := autoGitCommand("git", addArgs...).CombinedOutput(); addErr != nil {
			return nil, fmt.Errorf("git add scoped: %w\n%s", addErr, out)
		}
	}
	return drift, nil
}

// porcelainPath extracts the repo-relative path from one `git status
// --porcelain` line. Returns "" for blank/short lines. For a rename
// ("R  old -> new") it returns the destination path, which is what a
// plan declares for a rename. Surrounding quotes (core.quotePath, used
// for paths with special characters) are stripped. Mirrors the runner's
// gitops.porcelainPath.
func porcelainPath(line string) string {
	if len(line) < 4 {
		return ""
	}
	rest := line[3:]
	if idx := strings.Index(rest, " -> "); idx >= 0 {
		rest = rest[idx+len(" -> "):]
	}
	return strings.Trim(rest, `"`)
}
