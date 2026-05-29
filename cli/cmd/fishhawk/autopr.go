package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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

// shortID strips hyphens from id.String() and returns the leading 8
// hex characters. Matches the runner/internal/gitops convention.
func shortID(id uuid.UUID) string {
	s := strings.ReplaceAll(id.String(), "-", "")
	return s[:8]
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
// Falls back to a generated title ("Fishhawk: implement stage
// <shortRunID>") and empty body when the file is missing or the first
// line is whitespace-only. The attribution footer is always appended.
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
	return "Fishhawk: implement stage " + s
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

	commitMsg := title + "\n\n" + body
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
