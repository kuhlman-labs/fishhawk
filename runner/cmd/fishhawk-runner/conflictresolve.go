package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/conflictresolve"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// Conflict-resolution pass (E64.62 / #3202).
//
// The backend triggers this pass instead of refusing a conflicting base merge
// with a 422, so the conflict is resolved ON the run branch and the operator
// never pushes to a branch ADR-035 declares runner-owned. The runner performs
// the merge LOCALLY, captures a mechanical BASELINE of the merge state git
// produced, invokes the agent under a working-tree-edits-only contract, and
// runs a confinement GATE against that baseline. Only a passing gate reaches
// the scoped `git add` + single `git commit --no-edit`.
//
// Every refusal is a NAMED reason and every refusal runs the recovery path,
// because the contract this pass owes the operator is that a merge which starts
// and cannot finish is worse than one that never starts: the branch tip and the
// working tree are left exactly as they were before the merge began.

// Refusal reasons owned by the git wiring. The gate's own reasons come from
// runner/internal/conflictresolve, which decides them over captured structs.
const (
	// reasonUnexpectedHead means the checked-out tip is not the head the
	// trigger authorized, so a base that advanced under the pass cannot be
	// merged unnoticed. Refused BEFORE the merge starts.
	reasonUnexpectedHead = "conflict_resolution_unexpected_head"
	// reasonNoConflict means the merge completed cleanly. The base advanced
	// past the conflict between the trigger and the pass; pushing this merge
	// would be an unauthorized merge, so it is aborted and reported. The
	// operator simply re-invokes the verb, which then takes the clean-advance
	// path.
	reasonNoConflict = "conflict_resolution_no_conflict"
	// reasonMergeFailed means the merge command failed for a reason that is
	// NOT a content conflict (a bad ref, an unresolvable remote ref, a dirty
	// tree). Distinct from reasonNoConflict because the remedy differs.
	reasonMergeFailed = "conflict_resolution_merge_failed"
	// reasonBaselineCaptureFailed means the merge stopped on conflicts but the
	// baseline could not be read. Fail closed: with no baseline there is
	// nothing to confine the agent against.
	reasonBaselineCaptureFailed = "conflict_resolution_baseline_capture_failed"
	// reasonAgentFailed means the agent pass errored or exited non-zero.
	reasonAgentFailed = "conflict_resolution_agent_failed"
	// reasonObserveFailed means the post-agent state could not be read back.
	// Fail closed for the same reason as a baseline-capture failure.
	reasonObserveFailed = "conflict_resolution_observe_failed"
	// reasonCommitFailed means the gate passed but the scoped add or the
	// commit itself failed.
	reasonCommitFailed = "conflict_resolution_commit_failed"
)

// conflictResolutionRequest is the prompt-served instruction for one pass.
type conflictResolutionRequest struct {
	Branch          string
	BaseRef         string
	ExpectedHeadSHA string
}

// conflictResolutionAgent is the agent-invocation seam. Production wires the
// real coding agent; tests wire a stub that edits the working tree directly,
// which is what makes each named refusal drivable against a REAL repository.
type conflictResolutionAgent func(ctx context.Context, repoDir string) error

// conflictResolveParams is everything one pass needs. mergeRef is the FULLY
// QUALIFIED ref the run branch merges from (production passes
// "<remote>/<base>"; tests pass a local branch), so the wiring itself is
// network-free and the tests exercise the shipped code path rather than a
// parallel one.
type conflictResolveParams struct {
	repoDir  string
	mergeRef string
	request  conflictResolutionRequest
	invoke   conflictResolutionAgent
	logSink  io.Writer
}

// conflictResolveResult is the outcome of one pass. Reason is empty exactly
// when the pass committed.
type conflictResolveResult struct {
	Reason    string
	Detail    string
	CommitSHA string
	// Recovered reports whether the pre-merge state was restored. It is true
	// on every refusal that ran the recovery path to completion; a false with
	// a non-empty Reason means recovery ITSELF failed, which is the one
	// outcome an operator must look at by hand.
	Recovered bool
}

// runConflictResolution performs one bounded conflict-resolution pass.
//
// The ordering is load-bearing and mirrors the approved design:
//
//	verify tip -> merge --no-commit --no-ff -> capture baseline (HEAD,
//	MERGE_HEAD, merge message, index, conflicted files) -> agent ->
//	observe -> Verify -> scoped add -> commit
//
// The baseline is captured IMMEDIATELY after the merge stops, in one read, and
// BEFORE the agent runs. MERGE_HEAD and the merge message are captured there
// alongside HEAD because they are inputs to the commit this gate authorizes: a
// merge commit's second parent comes from MERGE_HEAD and its message from the
// merge message source, both on-disk repository metadata an agent can write
// while leaving HEAD, the index and the working tree untouched.
func runConflictResolution(ctx context.Context, p conflictResolveParams) conflictResolveResult {
	preTip, err := gitCapture(ctx, p.repoDir, "rev-parse", "HEAD")
	if err != nil {
		return conflictResolveResult{Reason: reasonMergeFailed, Detail: fmt.Sprintf("read HEAD: %v", err)}
	}
	if want := p.request.ExpectedHeadSHA; want != "" && want != preTip {
		// Nothing has been mutated yet, so there is nothing to recover:
		// report Recovered so the caller's postcondition holds uniformly.
		return conflictResolveResult{
			Reason:    reasonUnexpectedHead,
			Detail:    fmt.Sprintf("HEAD %s != authorized %s", preTip, want),
			Recovered: true,
		}
	}

	mergeOut, mergeErr := gitRun(ctx, p.repoDir, "merge", "--no-commit", "--no-ff", p.mergeRef)
	if mergeErr == nil {
		// The base advanced past the conflict. Refuse rather than push an
		// unauthorized merge the operator never approved.
		return refuseConflictResolution(ctx, p, preTip, reasonNoConflict,
			"merge completed with no conflict; the base advanced past the conflict")
	}
	unmerged, lsErr := gitUnmergedPaths(ctx, p.repoDir)
	if lsErr != nil || len(unmerged) == 0 {
		return refuseConflictResolution(ctx, p, preTip, reasonMergeFailed,
			fmt.Sprintf("merge failed with no conflicted paths: %s", firstLine(mergeOut)))
	}

	base, err := captureBaseline(ctx, p.repoDir, unmerged)
	if err != nil {
		return refuseConflictResolution(ctx, p, preTip, reasonBaselineCaptureFailed, err.Error())
	}
	logConflictEvent(p.logSink, "conflict_resolution_merge_stopped",
		fmt.Sprintf(`"conflicted":%d,"head":%q,"merge_head":%q`, len(base.Conflicted), base.HeadSHA, base.MergeHeadSHA))

	if p.invoke != nil {
		if err := p.invoke(ctx, p.repoDir); err != nil {
			return refuseConflictResolution(ctx, p, preTip, reasonAgentFailed, err.Error())
		}
	}

	obs, err := observeState(ctx, p.repoDir, base)
	if err != nil {
		return refuseConflictResolution(ctx, p, preTip, reasonObserveFailed, err.Error())
	}
	if violations := conflictresolve.Verify(base, obs); len(violations) > 0 {
		v := violations[0]
		detail := v.Detail
		if v.Path != "" {
			detail = fmt.Sprintf("%s: %s", v.Path, v.Detail)
		}
		if len(violations) > 1 {
			detail = fmt.Sprintf("%s (+%d more)", detail, len(violations)-1)
		}
		return refuseConflictResolution(ctx, p, preTip, string(v.Reason), detail)
	}

	// Gate passed. Stage EXACTLY the conflicted paths — never `git add -A`,
	// which would sweep anything the gate did not inspect.
	addArgs := append([]string{"add", "--"}, sortedConflictedPaths(base)...)
	if out, err := gitRun(ctx, p.repoDir, addArgs...); err != nil {
		return refuseConflictResolution(ctx, p, preTip, reasonCommitFailed,
			fmt.Sprintf("scoped add: %v: %s", err, firstLine(out)))
	}
	if out, err := gitRun(ctx, p.repoDir, "commit", "--no-edit"); err != nil {
		return refuseConflictResolution(ctx, p, preTip, reasonCommitFailed,
			fmt.Sprintf("commit: %v: %s", err, firstLine(out)))
	}
	head, err := gitCapture(ctx, p.repoDir, "rev-parse", "HEAD")
	if err != nil {
		return conflictResolveResult{Reason: reasonCommitFailed, Detail: fmt.Sprintf("read committed HEAD: %v", err)}
	}
	logConflictEvent(p.logSink, "conflict_resolution_committed", fmt.Sprintf(`"commit":%q`, head))
	return conflictResolveResult{CommitSHA: head}
}

// refuseConflictResolution records a named refusal and restores the pre-merge
// state.
//
// The recovery has two layers ON PURPOSE and both are part of the control: the
// abort is the ordinary path, and the verified hard-reset + clean is the
// fallback for a repository the abort could not fully restore (an abort that
// fails, or one that leaves residue). Deleting only the abort therefore proves
// nothing about the control — the counterfactual for this path deletes the
// WHOLE recovery.
func refuseConflictResolution(ctx context.Context, p conflictResolveParams, preTip, reason, detail string) conflictResolveResult {
	recovered := recoverPreMergeState(ctx, p.repoDir, preTip)
	logConflictEvent(p.logSink, "conflict_resolution_refused",
		fmt.Sprintf(`"reason":%q,"recovered":%t`, reason, recovered))
	return conflictResolveResult{Reason: reason, Detail: detail, Recovered: recovered}
}

// recoverPreMergeState restores the branch tip and the working tree to what
// they were before the merge began, and REPORTS whether it succeeded rather
// than assuming it did.
func recoverPreMergeState(ctx context.Context, repoDir, preTip string) bool {
	_, _ = gitRun(ctx, repoDir, "merge", "--abort")
	if preMergeStateRestored(ctx, repoDir, preTip) {
		return true
	}
	if _, err := gitRun(ctx, repoDir, "reset", "--hard", preTip); err != nil {
		return false
	}
	if _, err := gitRun(ctx, repoDir, "clean", "-fd"); err != nil {
		return false
	}
	return preMergeStateRestored(ctx, repoDir, preTip)
}

// preMergeStateRestored is the recovery POSTCONDITION: HEAD is back at the
// pre-merge tip and `git status --porcelain` is empty.
func preMergeStateRestored(ctx context.Context, repoDir, preTip string) bool {
	head, err := gitCapture(ctx, repoDir, "rev-parse", "HEAD")
	if err != nil || head != preTip {
		return false
	}
	status, err := gitCapture(ctx, repoDir, "status", "--porcelain")
	return err == nil && status == ""
}

// captureBaseline reads the merge state git produced in one pass: HEAD,
// MERGE_HEAD, the merge message source, every NON-CONFLICTED stage-0 index
// entry, and every conflicted path with the bytes git wrote.
func captureBaseline(ctx context.Context, repoDir string, unmerged map[string][]int) (conflictresolve.Baseline, error) {
	var base conflictresolve.Baseline
	head, err := gitCapture(ctx, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return base, fmt.Errorf("read HEAD: %w", err)
	}
	mergeHead, err := gitCapture(ctx, repoDir, "rev-parse", "MERGE_HEAD")
	if err != nil {
		return base, fmt.Errorf("read MERGE_HEAD: %w", err)
	}
	msg, err := readMergeMessage(ctx, repoDir)
	if err != nil {
		return base, err
	}
	index, err := gitStage0Index(ctx, repoDir)
	if err != nil {
		return base, err
	}
	base.HeadSHA = head
	base.MergeHeadSHA = mergeHead
	base.MergeMessage = msg
	base.Index = make(map[string]conflictresolve.IndexEntry, len(index))
	for path, e := range index {
		if _, conflicted := unmerged[path]; conflicted {
			continue
		}
		base.Index[path] = e
	}
	base.Conflicted = make(map[string]conflictresolve.ConflictedFile, len(unmerged))
	for path, stages := range unmerged {
		cf, err := captureConflictedFile(ctx, repoDir, path, stages)
		if err != nil {
			return base, err
		}
		base.Conflicted[path] = cf
	}
	return base, nil
}

// captureConflictedFile classifies ONE conflicted path from what git actually
// produced, never from the file's name or extension.
//
// A path carrying both stage 2 and stage 3 is a two-sided conflict: it is
// textual when git wrote marker lines into the working tree, and BINARY when it
// did not — git leaves OURS on disk with no markers for a file it cannot merge
// textually, so there is no hunk boundary to confine an edit to. A path missing
// stage 2 or stage 3 is a delete/modify conflict, whose admissible resolutions
// are the surviving side's full content or the deletion itself.
func captureConflictedFile(ctx context.Context, repoDir, path string, stages []int) (conflictresolve.ConflictedFile, error) {
	var cf conflictresolve.ConflictedFile
	has := func(stage int) bool {
		for _, s := range stages {
			if s == stage {
				return true
			}
		}
		return false
	}
	content, present, err := readWorkingFile(repoDir, path)
	if err != nil {
		return cf, err
	}
	if has(2) && has(3) {
		if present && conflictresolve.ContainsMarkerLine(content) {
			cf.Kind = conflictresolve.KindContent
			cf.MarkerBytes = content
			return cf, nil
		}
		cf.Kind = conflictresolve.KindBinary
		return cf, nil
	}
	cf.Kind = conflictresolve.KindDeleteModify
	for _, stage := range []int{2, 3} {
		if !has(stage) {
			continue
		}
		blob, err := gitCaptureRaw(ctx, repoDir, "cat-file", "blob", fmt.Sprintf(":%d:%s", stage, path))
		if err != nil {
			return cf, fmt.Errorf("read stage %d of %s: %w", stage, path, err)
		}
		cf.Sides = append(cf.Sides, blob)
	}
	return cf, nil
}

// observeState reads the same state back after the agent pass, before any
// `git add` and before the commit.
func observeState(ctx context.Context, repoDir string, base conflictresolve.Baseline) (conflictresolve.Observed, error) {
	var obs conflictresolve.Observed
	head, err := gitCapture(ctx, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return obs, fmt.Errorf("read HEAD: %w", err)
	}
	// MERGE_HEAD may be absent if the agent removed it; an absent MERGE_HEAD
	// is an empty observation, which the gate reads as a change from the
	// baseline rather than as a read failure.
	mergeHead, _ := gitCapture(ctx, repoDir, "rev-parse", "MERGE_HEAD")
	msg, err := readMergeMessage(ctx, repoDir)
	if err != nil {
		return obs, err
	}
	index, err := gitStage0Index(ctx, repoDir)
	if err != nil {
		return obs, err
	}
	unmerged, err := gitUnmergedPaths(ctx, repoDir)
	if err != nil {
		return obs, err
	}
	unstaged, err := gitPathList(ctx, repoDir, "diff", "--name-only", "-z")
	if err != nil {
		return obs, err
	}
	untracked, err := gitPathList(ctx, repoDir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return obs, err
	}
	obs.HeadSHA = head
	obs.MergeHeadSHA = mergeHead
	obs.MergeMessage = msg
	obs.Index = index
	obs.Unstaged = unstaged
	obs.Untracked = untracked
	for path := range unmerged {
		obs.Unmerged = append(obs.Unmerged, path)
	}
	sort.Strings(obs.Unmerged)
	obs.Working = make(map[string][]byte, len(base.Conflicted))
	for path := range base.Conflicted {
		content, present, err := readWorkingFile(repoDir, path)
		if err != nil {
			return obs, err
		}
		if present {
			obs.Working[path] = content
		}
	}
	return obs, nil
}

// readMergeMessage reads the merge message source git will hand
// `git commit --no-edit`. An ABSENT file is an empty message, not an error:
// removing it is one of the tampering shapes the gate must catch, and an error
// here would misreport it as a read failure.
func readMergeMessage(ctx context.Context, repoDir string) (string, error) {
	gitDir, err := gitCapture(ctx, repoDir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", fmt.Errorf("resolve git dir: %w", err)
	}
	b, err := os.ReadFile(filepath.Join(gitDir, "MERGE_MSG"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read MERGE_MSG: %w", err)
	}
	return string(b), nil
}

// readWorkingFile reads one working-tree path, distinguishing ABSENT from
// unreadable — the deletion side of a delete/modify conflict is expressed by
// absence, so collapsing the two would accept a deletion of any conflicted
// path.
func readWorkingFile(repoDir, path string) ([]byte, bool, error) {
	b, err := os.ReadFile(filepath.Join(repoDir, filepath.FromSlash(path)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	return b, true, nil
}

// gitUnmergedPaths returns every unmerged path with the index stages git
// recorded for it, de-duplicated across those stages: `git ls-files --unmerged`
// reports one RECORD PER STAGE (1 = common ancestor, 2 = ours, 3 = theirs), so
// a conflicted path appears up to three times.
func gitUnmergedPaths(ctx context.Context, repoDir string) (map[string][]int, error) {
	out, err := gitCaptureRaw(ctx, repoDir, "ls-files", "--unmerged", "-z")
	if err != nil {
		return nil, fmt.Errorf("list unmerged: %w", err)
	}
	paths := map[string][]int{}
	for _, rec := range splitNUL(out) {
		// <mode> <object> <stage>\t<path>
		tab := bytes.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		fields := strings.Fields(string(rec[:tab]))
		if len(fields) != 3 {
			continue
		}
		stage := 0
		if _, err := fmt.Sscanf(fields[2], "%d", &stage); err != nil {
			continue
		}
		path := string(rec[tab+1:])
		paths[path] = append(paths[path], stage)
	}
	return paths, nil
}

// gitStage0Index returns every MERGED (stage-0) index entry keyed by path.
func gitStage0Index(ctx context.Context, repoDir string) (map[string]conflictresolve.IndexEntry, error) {
	out, err := gitCaptureRaw(ctx, repoDir, "ls-files", "-s", "-z")
	if err != nil {
		return nil, fmt.Errorf("list index: %w", err)
	}
	index := map[string]conflictresolve.IndexEntry{}
	for _, rec := range splitNUL(out) {
		tab := bytes.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		fields := strings.Fields(string(rec[:tab]))
		if len(fields) != 3 || fields[2] != "0" {
			continue
		}
		index[string(rec[tab+1:])] = conflictresolve.IndexEntry{Mode: fields[0], OID: fields[1]}
	}
	return index, nil
}

// gitPathList runs a NUL-delimited path-listing git command and splits on NUL.
//
// Every path enumeration on this path uses `-z` and splits on NUL: git
// C-quotes a path carrying a quote, a backslash, a control character or a
// non-ASCII byte in its non-`-z` output, and a NEWLINE inside a name splits one
// path into two — the same blind-gate class AGENTS.md documents for the
// patch-coverage gate. A gate a filename can evade is not a gate.
func gitPathList(ctx context.Context, repoDir string, args ...string) ([]string, error) {
	out, err := gitCaptureRaw(ctx, repoDir, args...)
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	var paths []string
	for _, rec := range splitNUL(out) {
		paths = append(paths, string(rec))
	}
	return paths, nil
}

// splitNUL splits a NUL-delimited stream, dropping the empty trailing record.
func splitNUL(b []byte) [][]byte {
	var out [][]byte
	for _, rec := range bytes.Split(b, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// sortedConflictedPaths returns the conflicted set in a deterministic order so
// the scoped `git add` argument list is stable across runs.
func sortedConflictedPaths(base conflictresolve.Baseline) []string {
	out := make([]string, 0, len(base.Conflicted))
	for path := range base.Conflicted {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// gitBinary is the git executable, swappable in tests.
var gitBinary = "git"

// gitRun runs a git command and returns its combined output.
func gitRun(ctx context.Context, repoDir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, gitBinary, args...)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// gitCapture runs a git command and returns its trimmed stdout.
func gitCapture(ctx context.Context, repoDir string, args ...string) (string, error) {
	out, err := gitCaptureRaw(ctx, repoDir, args...)
	return strings.TrimSpace(string(out)), err
}

// gitCaptureRaw runs a git command and returns its stdout VERBATIM — no
// trimming, because blob contents and NUL-delimited streams are read this way.
func gitCaptureRaw(ctx context.Context, repoDir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, gitBinary, args...)
	cmd.Dir = repoDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, firstLine(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// firstLine bounds a captured message to its first line for a log field.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// logConflictEvent emits one runner-log line. fields is pre-rendered JSON
// members, matching the runner's existing hand-rendered log style.
func logConflictEvent(w io.Writer, event, fields string) {
	if w == nil {
		return
	}
	if fields == "" {
		_, _ = fmt.Fprintf(w, `{"event":%q}`+"\n", event)
		return
	}
	_, _ = fmt.Fprintf(w, `{"event":%q,%s}`+"\n", event, fields)
}

// conflictResolutionFromPrompt converts the prompt response's four
// conflict_resolution wire fields into a request, or nil when the backend
// served no pass.
//
// Nil is the ordinary case on every implement dispatch, and it is what keeps
// main.go's routing branch inert: an absent or half-populated instruction (no
// branch, or no base ref to merge from) is NOT a pass, because a runner handed
// an empty base ref would merge nothing and the pass would look like a
// no-conflict abort — a refusal naming the wrong cause.
func conflictResolutionFromPrompt(got *upload.FetchedPrompt) *conflictResolutionRequest {
	if got == nil || !got.ConflictResolution {
		return nil
	}
	if got.ConflictResolutionBranch == "" || got.ConflictResolutionBaseRef == "" {
		return nil
	}
	return &conflictResolutionRequest{
		Branch:          got.ConflictResolutionBranch,
		BaseRef:         got.ConflictResolutionBaseRef,
		ExpectedHeadSHA: got.ConflictResolutionExpectedHeadSHA,
	}
}

// runConflictResolutionStage is main.go's routing target: it wires the real
// coding agent into runConflictResolution and maps the outcome onto an exit
// code.
//
// A refusal is exitFailure with the NAMED reason on the runner log, which is
// what the backend's failure arm records as stage_conflict_resolution_failed —
// the pass is an assist, so a refusal restores the pre-pass review gate rather
// than escalating.
func runConflictResolutionStage(ctx context.Context, cfg config, req conflictResolutionRequest, promptPath string, logSink io.Writer) int {
	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}
	promptText, err := os.ReadFile(promptPath)
	if err != nil {
		logConflictEvent(logSink, "runner_failed",
			fmt.Sprintf(`"reason":%q,"detail":%q`, "read_prompt", err.Error()))
		return exitUsage
	}
	invoker, selErr := selectInvoker(cfg.agent, apiKeyForAgent(cfg.agent), agentBinaryOverride(cfg.agent, os.Getenv))
	if selErr != nil {
		logConflictEvent(logSink, "runner_failed",
			fmt.Sprintf(`"reason":%q,"detail":%q`, "agent_select", selErr.Error()))
		return exitUsage
	}
	res := runConflictResolution(ctx, conflictResolveParams{
		repoDir: repoDir,
		// The base ref is qualified with the runner's remote here, not by the
		// backend: the backend does not know what the runner calls its remote.
		mergeRef: conflictMergeRef(req.BaseRef),
		request:  req,
		invoke: func(ctx context.Context, dir string) error {
			_, err := invoker.Invoke(ctx, agent.Invocation{
				RunID:      cfg.runID,
				Stage:      cfg.stage,
				Prompt:     string(promptText),
				WorkingDir: dir,
				Budget:     agent.Budget{MaxTokens: cfg.maxTokens, Timeout: cfg.timeout},
			})
			return err
		},
		logSink: logSink,
	})
	if res.Reason != "" {
		logConflictEvent(logSink, "runner_failed",
			fmt.Sprintf(`"reason":%q,"detail":%q,"recovered":%t`, res.Reason, res.Detail, res.Recovered))
		return exitFailure
	}
	return exitOK
}

// conflictMergeRef qualifies a base BRANCH name with the runner's remote. A ref
// that already names a remote (or any explicit ref path) is passed through
// unchanged, so an operator-supplied fully-qualified ref is never double-prefixed.
func conflictMergeRef(baseRef string) string {
	if strings.Contains(baseRef, "/") {
		return baseRef
	}
	return "origin/" + baseRef
}
