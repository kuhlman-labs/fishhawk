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
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agentenv"
	"github.com/kuhlman-labs/fishhawk/runner/internal/conflictresolve"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// Runner-side refusal reasons for the bounded conflict-resolution pass (#3202).
// They sit alongside the conflictresolve package's gate reasons — that package
// owns every decision about the agent's EDITS, this file owns the reasons the
// pass could not reach a decision at all. Every one is stable wire text: it is
// what the backend records on the stage_conflict_resolution_failed entry and
// what the operator reads before deciding to resolve-push-vouch by hand.
const (
	// reasonHeadReadFailed — the checked-out tip could not be read, so the
	// pass cannot prove it is operating on the branch the trigger anchored to.
	// Refused BEFORE any mutation.
	reasonHeadReadFailed = "conflict_resolution_head_read_failed"
	// reasonUnexpectedHead — the checked-out tip is not the trigger's anchor.
	// Refused BEFORE any mutation: merging into an unexpected tree would push
	// a commit the operator never authorized.
	reasonUnexpectedHead = "conflict_resolution_unexpected_head"
	// reasonNoConflict — the merge completed CLEANLY. The base advanced past
	// the conflict between the rebase probe and this pass, so there is nothing
	// to resolve; pushing an unauthorized clean merge is worse than refusing.
	reasonNoConflict = "conflict_resolution_no_conflict"
	// reasonMergeFailed — the merge failed and left NO unmerged entries, so it
	// is not the conflict this pass exists to resolve.
	reasonMergeFailed = "conflict_resolution_merge_failed"
	// reasonBaselineCaptureFailed — the merge state git produced could not be
	// captured, so nothing can be verified against it.
	reasonBaselineCaptureFailed = "conflict_resolution_baseline_capture_failed"
	// reasonAgentFailed — the agent invocation itself failed.
	reasonAgentFailed = "conflict_resolution_agent_failed"
	// reasonObserveFailed — the post-agent state could not be read back.
	reasonObserveFailed = "conflict_resolution_observe_failed"
	// reasonCommitFailed — the scoped `git add` or the single `git commit
	// --no-edit` failed AFTER a passing gate.
	reasonCommitFailed = "conflict_resolution_commit_failed"
	// reasonStagedContentChanged — the index `git add` wrote is not the artifact
	// the gate approved. `git add` applies the eol/text/ident normalization and
	// any `filter.<driver>.clean` the path's attributes name, and a driver is
	// named by config, so it cannot be neutralized by a deny-list the way a hook
	// can (gitops.HardeningArgs). The check compares the staged OID against the
	// OID `git add` WOULD produce from the gate-observed bytes (`git hash-object
	// --path`), so an HONEST committed-attribute transformation is accepted while
	// content the gate never approved is refused; it ALSO refuses a config or
	// attribute change slipped between the gate and the add. Refused AFTER the
	// gate, BEFORE the commit.
	reasonStagedContentChanged = "conflict_resolution_staged_content_changed"
	// reasonCommitTreeChanged — the commit git produced is not the artifact the
	// gate authorized: its tree differs from the index written immediately
	// after the scoped add, or its parents are not (pre-merge tip, MERGE_HEAD).
	// This is the mechanism-INDEPENDENT check: whatever staged the extra
	// content — a hook, a filter, a config key nobody thought of — the
	// published commit is compared against the authorized result rather than
	// trusted because the inputs were neutralized.
	reasonCommitTreeChanged = "conflict_resolution_commit_tree_changed"
	// reasonCommittedMessageChanged — the committed message is not the merge
	// message the gate compared. Distinct from the gate's
	// conflict_resolution_merge_message_changed, which reads MERGE_MSG BEFORE
	// the commit: this one catches a rewrite that happens DURING it.
	reasonCommittedMessageChanged = "conflict_resolution_committed_message_changed"

	// --- tree-establishment refusals (#3340) ---
	//
	// The stage ESTABLISHES its own detached throwaway tree at the run-branch
	// tip rather than assuming the dispatch checkout sits there (it does not on
	// the local loop: main.go routes to the pass BEFORE lineage-worktree
	// provisioning, and the lineage worktree is detached back to its pre-agent
	// ref by working_tree_restored). Each refusal below fires BEFORE any
	// WORKING-TREE, BRANCH or CHECKOUT mutation — the dispatch checkout's HEAD,
	// index and files are untouched. The one side effect a later refusal may
	// follow is a REMOTE-TRACKING-REF refresh: the run-branch fetch at step (c)
	// refreshes refs/remotes/origin/<branch> and the base fetch at step (f)
	// refreshes refs/remotes/<baseRemote>/<baseBranch> before a subsequent step
	// can refuse. Those are ref-only updates — never HEAD, index, working files
	// or local branches — so they are benign, but they are NOT claimed away.

	// reasonTreeUnavailable — the dispatch checkout is empty or not a git work
	// tree, so there is nothing to fetch the run-branch tip against and no clone
	// to hang a throwaway worktree off. Refused before the run-branch fetch, so
	// no tracking ref has been refreshed. Category C: an environment problem, not
	// a judgment about the change.
	reasonTreeUnavailable = "conflict_resolution_tree_unavailable"
	// reasonBranchFetchFailed — the run-branch tip could not be fetched from the
	// remote. Category C: transport, not judgment.
	reasonBranchFetchFailed = "conflict_resolution_branch_fetch_failed"
	// reasonBaseRefUnfetchable — the qualified base ref has no fetchable
	// <remote>/<branch> shape (e.g. refs/tags/…), so the pass cannot fetch the
	// base tip the merge will read. Refused AFTER the run-branch fetch refreshed
	// refs/remotes/origin/<branch> but BEFORE any base fetch. Category B: an
	// input the pass cannot honor, and a retry against the same trigger refuses
	// identically.
	reasonBaseRefUnfetchable = "conflict_resolution_base_ref_unfetchable"
	// reasonBaseFetchFailed — the base branch could not be fetched from the
	// remote the qualified ref names. Category C: transport, not judgment.
	reasonBaseFetchFailed = "conflict_resolution_base_fetch_failed"
	// reasonTreeProvisionFailed — the throwaway parent directory or the
	// `git worktree add --detach` could not be created. Refused after both
	// tracking refs were refreshed but before any working-tree/branch/checkout
	// mutation. Category C: an environment problem.
	reasonTreeProvisionFailed = "conflict_resolution_tree_provision_failed"
)

// conflictResolutionRequest is the runner's view of the backend's
// conflict-resolution instruction. It exists only in the fully-populated form:
// conflictResolutionFromPrompt returns nil for anything less.
type conflictResolutionRequest struct {
	Branch          string
	BaseRef         string
	ExpectedHeadSHA string
}

// conflictResolutionFromPrompt reads the instruction off a fetched prompt,
// returning nil when there is no pass to run.
//
// A HALF-populated instruction (the flag set but a missing branch, base ref or
// anchor) is treated as NO pass rather than served: a runner handed an empty
// base ref would merge nothing and then refuse naming the wrong cause, burning
// the ceiling-of-one budget on a serve bug and telling the operator the merge
// was clean when it never ran. Refusing to start is recoverable — the
// operator's next rebase invocation takes the fail-closed 422 — while a
// mis-attributed refusal is not.
func conflictResolutionFromPrompt(got *upload.FetchedPrompt) *conflictResolutionRequest {
	if got == nil || !got.ConflictResolution {
		return nil
	}
	if got.ConflictResolutionBranch == "" ||
		got.ConflictResolutionBaseRef == "" ||
		got.ConflictResolutionExpectedHeadSHA == "" {
		return nil
	}
	return &conflictResolutionRequest{
		Branch:          got.ConflictResolutionBranch,
		BaseRef:         got.ConflictResolutionBaseRef,
		ExpectedHeadSHA: got.ConflictResolutionExpectedHeadSHA,
	}
}

// conflictResolutionResult is the pass's verdict. Reason is empty on success,
// in which case HeadSHA is the merge commit and BaseSHA the pre-merge tip.
type conflictResolutionResult struct {
	Reason     string
	Detail     string
	Violations []conflictresolve.Violation
	HeadSHA    string
	BaseSHA    string
	// Recovered reports whether the abort/reset path VERIFIED the repository
	// back at the pre-merge tip with a clean worktree. It is an observation,
	// not an assumption: a refusal that could not restore is a materially
	// different operator situation from one that could.
	Recovered bool
	// RecoveryDetail names what recovery could not restore, when it could not.
	RecoveryDetail string
}

// refused reports whether the pass refused.
func (r conflictResolutionResult) refused() bool { return r.Reason != "" }

// conflictRecoveryTimeout bounds the detached recovery context. A var so tests
// can shrink it.
var conflictRecoveryTimeout = 2 * time.Minute

// conflictCaptureHook and conflictCommitHook are TEST SEAMS, nil in production
// and never set outside tests. They exist because two of the pass's fail-closed
// branches are reachable only from a repository state that arises BETWEEN the
// pass's own steps — a baseline capture that cannot read the merge state git
// produced, and an add/commit that fails AFTER a passing gate. Without a seam
// at exactly those two points those branches would be enumerated in prose and
// untested, which is the class of gap this change exists to close.
var (
	// conflictCaptureHook runs immediately after the merge stops on conflicts
	// and immediately before the baseline capture.
	conflictCaptureHook func(repoDir string)
	// conflictCommitHook runs immediately after a PASSING gate and immediately
	// before the scoped `git add`.
	conflictCommitHook func(repoDir string)
	// conflictPostAddHook runs after the authorized tree is written and
	// immediately BEFORE the single commit. It stands in for whatever git
	// mechanism the neutralization missed — the production vector is an
	// agent-installed pre-commit hook — so the post-commit authorized-artifact
	// check has a vehicle that does not depend on gitops.HardeningArgs being
	// incomplete. Without it that check could only be exercised by disabling
	// the neutralization, i.e. by shipping a bypass.
	conflictPostAddHook func(repoDir string)
)

// fetchConflictBranchTip fetches a branch's live tip into
// refs/remotes/<remote>/<branch> WITHOUT moving any working tree, returning the
// fetched tip SHA. Production wires it to gitops.FetchBaseTip; it is a
// package-level var for the same reason fetchDiffBaseTip is (main.go): the
// fake-pusher tests must never fetch against the runner's own source repo, and
// the establishment tests wrap it to record the (remote, branch) each fetch
// targeted. The run-branch fetch AND the base fetch both flow through it, so the
// tracking ref it refreshes is exactly the one the merge later resolves.
var fetchConflictBranchTip = gitops.FetchBaseTip

// conflictTreeTempDir is the parent directory os.MkdirTemp creates the
// throwaway worktree under. Empty means the OS default temp dir (os.TempDir).
// A var so tests redirect it to a t.TempDir (avoiding /tmp pollution and
// parallel races) and can point it at a regular FILE to induce the MkdirTemp
// failure branch.
var conflictTreeTempDir = ""

// gitOut runs git in repoDir and returns trimmed stdout.
func gitOut(ctx context.Context, repoDir string, args ...string) (string, error) {
	out, err := gitOutRaw(ctx, repoDir, args...)
	return strings.TrimSpace(string(out)), err
}

// gitOutRaw runs git in repoDir and returns raw stdout. Raw is required for
// every `-z` enumeration: trimming would eat a trailing NUL and, on a path
// carrying whitespace, silently reshape it.
func gitOutRaw(ctx context.Context, repoDir string, args ...string) ([]byte, error) {
	// EVERY git invocation this pass makes runs with repository-local hooks and
	// command-executing config neutralized (#3202). The agent is confined to
	// working-tree edits by CONTRACT, but `.git/hooks/*` and `.git/config` sit
	// OUTSIDE that tree: an agent-written pre-commit hook would `git add`
	// content after the gate passed, and an agent-set `core.fsmonitor` would
	// run a command during the observation reads the gate DECIDES on. Neither
	// is captured by the baseline, so neutralization is applied here — at the
	// single choke point every capture, observe, add, commit and recovery
	// command flows through — rather than at each call site, where the next
	// added command would silently miss it. gitops.HardeningArgs is the ONE
	// definition; the push path applies the same set.
	cmd := exec.CommandContext(ctx, "git", append(gitops.HardeningArgs(), args...)...)
	cmd.Dir = repoDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// gitRun runs git in repoDir, discarding stdout.
func gitRun(ctx context.Context, repoDir string, args ...string) error {
	_, err := gitOutRaw(ctx, repoDir, args...)
	return err
}

// gitOutStdin runs git in repoDir feeding stdin, returning trimmed stdout. It
// applies the same HardeningArgs choke point every other invocation uses. It is
// the vehicle for `git hash-object --path=<p> --stdin`, whose stdin is the
// gate-observed bytes — never the mutable working tree.
func gitOutStdin(ctx context.Context, repoDir string, stdin []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append(gitops.HardeningArgs(), args...)...)
	cmd.Dir = repoDir
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// readGitConfigFn is a TEST SEAM, never reassigned outside tests. Both config
// reads' failure branches are fail-closed returns that a repository state
// cannot reach selectively: a malformed `.git/config` fails `git rev-parse` and
// `git ls-files` too (verified), so every capture and observe read would fail
// FIRST and the test would pass whether or not the config read returned. The
// seam makes each branch's own return the thing under test — the same reason
// conflictCaptureHook and conflictCommitHook exist.
var readGitConfigFn = readGitConfig

// readGitConfig captures the EFFECTIVE git configuration as a raw
// `git config --list -z` stream, for the gate input that refuses ANY config
// change during the pass (conflictresolve.ReasonRepoConfigChanged).
//
// `--list` resolves `include.path` directives and merges the system, global,
// local and worktree scopes, so this covers an agent that writes
// `$HOME/.gitconfig` or an included file rather than `.git/config`. It is read
// through gitOutRaw so it inherits the same HardeningArgs choke point every
// other read uses; the `-c` overrides are CONSTANT across the baseline and
// observe reads, so they cannot manufacture a difference between them.
//
// RAW, never trimmed: the stream is compared byte-for-byte and a trailing NUL
// is part of it.
func readGitConfig(ctx context.Context, repoDir string) (string, error) {
	out, err := gitOutRaw(ctx, repoDir, "config", "--list", "-z")
	if err != nil {
		return "", fmt.Errorf("read git config: %w", err)
	}
	return string(out), nil
}

// readConflictedAttributesFn is a TEST SEAM, never reassigned outside tests. It
// exists for the same reason readGitConfigFn does: both attribute reads'
// fail-closed returns are reachable only from a repository state that fails the
// other capture/observe reads FIRST, so the test would pass whether or not the
// attribute read returned. The seam makes each branch's own return the thing
// under test.
var readConflictedAttributesFn = readConflictedAttributes

// readConflictedAttributes captures the EFFECTIVE gitattributes for the
// conflicted set as a raw `git check-attr -z --all -- <paths>` stream, for the
// gate input that refuses ANY attribute change during the pass
// (conflictresolve.ReasonAttributesChanged) and for step 7a's post-clean
// staged-blob comparison.
//
// `check-attr` resolves the working-tree `.gitattributes`, `.git/info/attributes`
// and the global `$HOME/.config/git/attributes`, so this covers every place an
// agent could bind an eol/text/ident/filter driver to a conflicted path. Paths
// are SORTED so the stream is deterministic across the baseline and observe
// reads. Read through gitOutRaw so it inherits the same HardeningArgs choke
// point every other read uses.
//
// RAW, never trimmed, and returned UNPARSED: the conflictresolve package owns
// the framing parse (parseAttrStream), so this stream has exactly one parser.
func readConflictedAttributes(ctx context.Context, repoDir string, paths []string) (string, error) {
	if len(paths) == 0 {
		return "", nil
	}
	args := append([]string{"check-attr", "-z", "--all", "--"}, paths...)
	out, err := gitOutRaw(ctx, repoDir, args...)
	if err != nil {
		return "", fmt.Errorf("read git attributes: %w", err)
	}
	return string(out), nil
}

// splitNUL splits a `-z` git stream on NUL, dropping the empty trailing field.
// Splitting on NUL rather than newline is not optional: git only refrains from
// C-quoting a path (a quote, a backslash, a control character, a non-ASCII
// byte) under -z, and a newline INSIDE a filename splits one path into two
// under any line-oriented split — which would drop the real path from the gate
// and admit an edit outside the conflicted set.
func splitNUL(b []byte) []string {
	var out []string
	for _, f := range bytes.Split(b, []byte{0}) {
		if len(f) > 0 {
			out = append(out, string(f))
		}
	}
	return out
}

// qualifyMergeRef resolves the base ref the pass merges into a ref git can
// actually name from repoDir.
//
// The rule is deliberately NOT "contains a slash → already qualified", which is
// what round 1 shipped: an ordinary base branch like `release/1.2` then never
// got the remote prefix, the merge failed to resolve it, and the pass burned
// its ceiling-of-one budget on a naming bug rather than on a real conflict.
//
// An explicit `refs/...` path is passed through. Otherwise the ref is prefixed
// with the runner's remote UNLESS its FIRST path segment names a CONFIGURED
// remote — the only case in which a slash-bearing ref is already
// remote-qualified.
func qualifyMergeRef(ctx context.Context, repoDir, remote, ref string) string {
	if remote == "" {
		remote = gitops.DefaultRemote
	}
	if ref == "" {
		return ref
	}
	if strings.HasPrefix(ref, "refs/") {
		return ref
	}
	if first, _, ok := strings.Cut(ref, "/"); ok {
		for _, r := range configuredRemotes(ctx, repoDir) {
			if r == first {
				return ref
			}
		}
	}
	return remote + "/" + ref
}

// configuredRemotes lists the repository's configured remote names. A git
// failure yields none, which makes qualifyMergeRef PREFIX the ref — the
// fail-safe direction: an over-qualified ref fails the merge loudly and
// recoverably, while an under-qualified one resolves to whatever local branch
// happens to share the name.
func configuredRemotes(ctx context.Context, repoDir string) []string {
	out, err := gitOut(ctx, repoDir, "remote")
	if err != nil {
		return nil
	}
	var names []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	return names
}

// conflictBaseFetchTarget derives the (remote, branch) the base fetch must
// target from the OUTPUT of qualifyMergeRef — never from the raw BaseRef, so
// the fetch and the merge cannot disagree about which remote or branch is meant.
// The stage then forces the merge to read refs/remotes/<remote>/<branch>, the
// exact tracking ref this fetch refreshes, so for EVERY accepted shape the
// object the fetch refreshed is the object the merge consumes (#3340).
//
// Shapes, all as qualifyMergeRef produces them:
//
//	refs/heads/<b>              → (DefaultRemote, b): the operator named a LOCAL
//	                              branch, but a stale local ref is the defect this
//	                              change closes, so the fresh remote tip is fetched
//	                              and merged instead.
//	refs/remotes/<r>/<b>        → (r, b): b may itself contain slashes.
//	<r>/<b>, r a known remote   → (r, b): the already-remote-qualified case;
//	                              b may contain slashes. DefaultRemote is always
//	                              accepted (it is the runner's own remote and
//	                              qualifyMergeRef's prefixing fallback), even when
//	                              `git remote` degraded to an empty list.
//	anything else (refs/tags/…, // → ok=false: no fetchable branch, so the caller
//	an unknown remote prefix)      refuses reasonBaseRefUnfetchable rather than
//	                               fetching from a guessed remote.
func conflictBaseFetchTarget(qualified string, remotes []string) (fetchRemote, branch string, ok bool) {
	if qualified == "" {
		return "", "", false
	}
	if b, found := strings.CutPrefix(qualified, "refs/heads/"); found {
		if b == "" {
			return "", "", false
		}
		return gitops.DefaultRemote, b, true
	}
	if rest, found := strings.CutPrefix(qualified, "refs/remotes/"); found {
		r, b, cut := strings.Cut(rest, "/")
		if !cut || r == "" || b == "" {
			return "", "", false
		}
		return r, b, true
	}
	if strings.HasPrefix(qualified, "refs/") {
		// refs/tags/… or any other non-branch ref path: not fetchable as a branch.
		return "", "", false
	}
	// A non-refs value is <remote>/<branch>: qualifyMergeRef leaves it unprefixed
	// only when the first segment names a configured remote, and otherwise
	// prefixes DefaultRemote — so the first segment is a remote name.
	first, rest, cut := strings.Cut(qualified, "/")
	if !cut || first == "" || rest == "" {
		return "", "", false
	}
	if first == gitops.DefaultRemote {
		return first, rest, true
	}
	for _, r := range remotes {
		if r == first {
			return first, rest, true
		}
	}
	return "", "", false
}

// runConflictResolutionPass performs the whole local pass: verify, merge,
// capture, invoke, observe, gate, and — only on a passing gate — the scoped
// `git add`, ONE `git commit --no-edit --no-verify`, and the verification that
// the commit it is about to hand back IS the artifact the gate authorized. It
// performs NO network I/O; the caller owns the push and the terminal report.
//
// Every git command below runs with repository-local hooks and
// command-executing config neutralized (see gitOutRaw): `.git` is not the
// working tree the agent's contract confines it to.
//
// Every refusal path runs the detached recovery before returning, so the
// repository is left at the pre-merge tip with a clean worktree whatever
// happened — and the result reports whether that was VERIFIED rather than
// assumed.
func runConflictResolutionPass(ctx context.Context, repoDir, remote string,
	req conflictResolutionRequest, invoke func(context.Context) error, logSink io.Writer) conflictResolutionResult {

	logEvent(logSink, "conflict_resolution_started", map[string]string{
		"branch": req.Branch, "base_ref": req.BaseRef, "expected_head_sha": req.ExpectedHeadSHA,
	})

	// (1) Verify the checked-out tip BEFORE mutating anything. Ordering is
	// load-bearing: a refusal here has nothing to recover from.
	head, err := gitOut(ctx, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return conflictResolutionResult{Reason: reasonHeadReadFailed, Detail: err.Error(), Recovered: true}
	}
	if head != req.ExpectedHeadSHA {
		return conflictResolutionResult{
			Reason:    reasonUnexpectedHead,
			Detail:    fmt.Sprintf("checked-out tip %s, trigger anchored to %s", head, req.ExpectedHeadSHA),
			Recovered: true,
		}
	}

	refuse := func(reason, detail string, violations []conflictresolve.Violation) conflictResolutionResult {
		res := conflictResolutionResult{Reason: reason, Detail: detail, Violations: violations, BaseSHA: head}
		res.Recovered, res.RecoveryDetail = recoverConflictResolution(ctx, repoDir, head)
		logEvent(logSink, "conflict_resolution_refused", map[string]string{
			"reason": reason, "detail": detail,
			"recovered": fmt.Sprintf("%t", res.Recovered), "recovery_detail": res.RecoveryDetail,
		})
		return res
	}

	// (2) Merge the qualified base ref, stopping before the commit.
	mergeRef := qualifyMergeRef(ctx, repoDir, remote, req.BaseRef)
	mergeErr := gitRun(ctx, repoDir, "merge", "--no-commit", "--no-ff", mergeRef)

	unmergedRaw, lsErr := gitOutRaw(ctx, repoDir, "ls-files", "--unmerged", "-z")
	if lsErr != nil {
		return refuse(reasonMergeFailed, "read unmerged entries: "+lsErr.Error(), nil)
	}
	conflictedPaths := unmergedPathSet(unmergedRaw)

	if mergeErr == nil {
		// The merge COMPLETED. The base advanced past the conflict the rebase
		// probe saw, so this pass has nothing to resolve — and the operator
		// authorized a conflict RESOLUTION, not an unreviewed clean merge.
		return refuse(reasonNoConflict, "merge of "+mergeRef+" completed with no conflict", nil)
	}
	if len(conflictedPaths) == 0 {
		return refuse(reasonMergeFailed,
			fmt.Sprintf("merge of %s failed with no conflicted paths: %v", mergeRef, mergeErr), nil)
	}

	// (3) Capture the baseline in ONE read, before the agent sees anything.
	if conflictCaptureHook != nil {
		conflictCaptureHook(repoDir)
	}
	base, err := captureConflictBaseline(ctx, repoDir, head, conflictedPaths)
	if err != nil {
		return refuse(reasonBaselineCaptureFailed, err.Error(), nil)
	}

	// (4) Invoke the agent under a working-tree-edits-only contract.
	if err := invoke(ctx); err != nil {
		return refuse(reasonAgentFailed, err.Error(), nil)
	}

	// (5) Read the state back. Uses the DETACHED context for the same reason
	// recovery does: a cancellation during the agent must not make the
	// observation fail instantly and be reported as an observe failure when the
	// real event was the cancellation.
	obsCtx, obsCancel := context.WithTimeout(context.WithoutCancel(ctx), conflictRecoveryTimeout)
	defer obsCancel()
	obs, err := observeConflictState(obsCtx, repoDir, base)
	if err != nil {
		return refuse(reasonObserveFailed, err.Error(), nil)
	}

	// (6) The gate. conflictresolve owns the decision; this file only acts on it.
	if violations := conflictresolve.Verify(base, obs); len(violations) > 0 {
		return refuse(string(violations[0].Reason), violationDetail(violations), violations)
	}

	// (7) Only now: the SCOPED add (never `git add -A`) and ONE commit.
	if conflictCommitHook != nil {
		conflictCommitHook(repoDir)
	}
	addArgs := append([]string{"add", "--"}, sortedStringKeys(base.Conflicted)...)
	if err := gitRun(obsCtx, repoDir, addArgs...); err != nil {
		return refuse(reasonCommitFailed, "stage conflicted paths: "+err.Error(), nil)
	}

	// (7a) The gate decided on WORKING-TREE bytes; `git add` is what turns them
	// into the artifact, applying the eol/text/ident/filter transformation the
	// path's attributes name. Verify the staged blob is the EXPECTED POST-CLEAN
	// form of the gate-observed bytes (not their raw bytes), and that neither the
	// config nor the attributes moved between the gate and the add — both survive
	// the hook/config neutralization above because a filter/eol driver is named
	// by config or attributes, not by a fixed key.
	if reason, detail := verifyStagedMatchesObserved(obsCtx, repoDir, base, obs); reason != "" {
		return refuse(reason, detail, nil)
	}

	// (7b) Snapshot the AUTHORIZED tree from the index the gate just approved,
	// then commit. Anything that mutates the index between here and the commit
	// (a hook the neutralization missed, a config key nobody enumerated) moves
	// the committed tree off this SHA and is refused below.
	authorizedTree, err := gitOut(obsCtx, repoDir, "write-tree")
	if err != nil {
		return refuse(reasonCommitFailed, "write authorized tree: "+err.Error(), nil)
	}
	if conflictPostAddHook != nil {
		conflictPostAddHook(repoDir)
	}
	// --no-verify is belt to HardeningArgs' braces: it bypasses the pre-commit
	// and commit-msg hooks by flag as well as by path.
	if err := gitRun(obsCtx, repoDir, "commit", "--no-edit", "--no-verify"); err != nil {
		return refuse(reasonCommitFailed, "commit merge: "+err.Error(), nil)
	}
	newHead, err := gitOut(obsCtx, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return refuse(reasonCommitFailed, "read merge commit: "+err.Error(), nil)
	}

	// (7c) The commit that will be PUBLISHED is compared against the authorized
	// result — tree, parents and message — rather than trusted. A refusal here
	// resets to the pre-merge tip, so the unauthorized commit is discarded and
	// never pushed.
	if reason, detail := verifyAuthorizedCommit(obsCtx, repoDir, base, head, authorizedTree); reason != "" {
		return refuse(reason, detail, nil)
	}

	logEvent(logSink, "conflict_resolution_committed", map[string]string{
		"head_sha": newHead, "base_sha": head, "merge_ref": mergeRef,
	})
	return conflictResolutionResult{HeadSHA: newHead, BaseSHA: head, Recovered: true}
}

// verifyStagedMatchesObserved confirms the index `git add` just wrote holds
// exactly what the gate approved, comparing against the EXPECTED POST-CLEAN form
// rather than the raw observed bytes.
//
// It is the filter/eol answer. `git add` applies whatever eol/text/ident
// normalization AND whatever `filter.<driver>.clean` command the path's
// attributes name, and a driver is named by config — there is no wildcard to
// neutralize it the way gitops.HardeningArgs neutralizes a fixed key. A raw
// byte comparison therefore refuses EVERY honest resolution in a repository
// whose committed `.gitattributes` legitimately transforms bytes (e.g.
// `text eol=crlf`: checkout writes CRLF, add cleans to LF). So the check has two
// parts:
//
//	(i)  Re-read the config AND the conflicted set's attributes and refuse any
//	     drift from the baseline the gate decided on — a change between the gate
//	     and the add moves the transformation, so a post-clean comparison could
//	     otherwise AGREE with a maliciously-transformed blob.
//	(ii) Compare each conflicted path's staged stage-0 OID against the OID
//	     `git add` WOULD produce from the gate-observed bytes — computed by
//	     hashing those bytes with the path's own attributes applied
//	     (`git hash-object --path=<p> --stdin`), fed the GATE-OBSERVED bytes and
//	     never the mutable working tree. A path the gate observed ABSENT must be
//	     absent from the index too.
//
// Returns ("", "") when the index matches, or a named reason and a detail.
func verifyStagedMatchesObserved(ctx context.Context, repoDir string,
	base conflictresolve.Baseline, obs conflictresolve.Observed) (string, string) {

	// Part (i): config/attribute drift between the gate and the add.
	cfg, err := readGitConfigFn(ctx, repoDir)
	if err != nil {
		return reasonStagedContentChanged, "re-read git config after add: " + err.Error()
	}
	if cfg != base.Config {
		return reasonStagedContentChanged,
			"the effective git configuration changed between the gate and the add — " +
				conflictresolve.ConfigChangeDetail(base.Config, cfg)
	}
	attrs, err := readConflictedAttributesFn(ctx, repoDir, sortedStringKeys(base.Conflicted))
	if err != nil {
		return reasonStagedContentChanged, "re-read git attributes after add: " + err.Error()
	}
	if attrs != base.Attributes {
		return reasonStagedContentChanged,
			"the effective git attributes changed between the gate and the add — " +
				conflictresolve.AttributeChangeDetail(base.Attributes, attrs)
	}

	// Part (ii): staged OID vs the expected post-clean OID of the observed bytes.
	for _, path := range sortedStringKeys(base.Conflicted) {
		want := obs.Working[path]
		staged, err := gitOut(ctx, repoDir, "rev-parse", ":"+path)
		if err != nil {
			// No stage-0 entry. Correct only when the gate observed the path
			// gone; otherwise the add did not stage what the gate approved.
			if want.Present {
				return reasonStagedContentChanged,
					fmt.Sprintf("%s: no staged entry for a path the gate observed present", path)
			}
			continue
		}
		if !want.Present {
			return reasonStagedContentChanged,
				fmt.Sprintf("%s: staged entry present for a path the gate observed deleted", path)
		}
		expected, err := gitOutStdin(ctx, repoDir, want.Bytes, "hash-object", "--path="+path, "--stdin")
		if err != nil {
			return reasonStagedContentChanged,
				fmt.Sprintf("%s: hash gate-observed bytes: %s", path, err.Error())
		}
		if staged != expected {
			return reasonStagedContentChanged, fmt.Sprintf(
				"%s: staged blob %s is not the expected post-clean blob %s of the %d gate-approved bytes (attributes: %s)",
				path, staged, expected, len(want.Bytes),
				conflictresolve.AttributeNamesFor(base.Attributes, path))
		}
	}
	return "", ""
}

// verifyAuthorizedCommit compares the commit that is about to be PUBLISHED
// against the artifact the gate authorized: the tree written from the approved
// index, the two merge parents, and the merge message.
//
// This is the check that makes the confinement claim hold WITHOUT depending on
// having enumerated every way an agent can make git run a command. Whatever the
// mechanism, a commit whose tree is not `authorizedTree` is refused, and the
// refusal's recovery resets to the pre-merge tip, so the unauthorized commit is
// discarded rather than pushed.
func verifyAuthorizedCommit(ctx context.Context, repoDir string,
	base conflictresolve.Baseline, preTip, authorizedTree string) (string, string) {

	tree, err := gitOut(ctx, repoDir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return reasonCommitTreeChanged, "read committed tree: " + err.Error()
	}
	if tree != authorizedTree {
		return reasonCommitTreeChanged,
			fmt.Sprintf("committed tree %s is not the authorized tree %s", tree, authorizedTree)
	}

	parentLine, err := gitOut(ctx, repoDir, "rev-list", "--parents", "-n", "1", "HEAD")
	if err != nil {
		return reasonCommitTreeChanged, "read commit parents: " + err.Error()
	}
	parents := strings.Fields(parentLine)
	if len(parents) != 3 || parents[1] != preTip || parents[2] != base.MergeHeadSHA {
		return reasonCommitTreeChanged,
			fmt.Sprintf("commit parents %v, want [%s %s]", parents[1:], preTip, base.MergeHeadSHA)
	}

	msgRaw, err := gitOutRaw(ctx, repoDir, "log", "-1", "--format=%B", "HEAD")
	if err != nil {
		return reasonCommittedMessageChanged, "read commit message: " + err.Error()
	}
	if normalizeCommitMessage(string(msgRaw)) != normalizeCommitMessage(base.MergeMessage) {
		return reasonCommittedMessageChanged,
			"the committed message is not the merge message the gate compared"
	}
	return "", ""
}

// normalizeCommitMessage renders a message in the form the comparison above can
// make: comment lines dropped, trailing whitespace trimmed per line, and the
// whole trimmed. Both sides go through it, so the comparison stays symmetric.
//
// A comment line is one whose FIRST BYTE is the comment character — that is
// git's own rule (git-commit(1) --cleanup), and matching it exactly is the
// point. The earlier form trimmed leading whitespace before the test, which was
// WIDER than git: an INDENTED comment-like line (`  # x`) is ordinary message
// content to git and survives into the recorded message, but was deleted from
// BOTH sides here — so an agent could add or remove one and the comparison
// would not see it. gitops.HardeningArgs pins `core.commentChar=#` on every
// invocation this pass makes, so "the comment character" is exactly defined
// rather than assumed from the repository's configuration.
//
// The stripping that remains is tolerance for a `commit.cleanup=strip`
// configuration, NOT a mirror of the default path: verified empirically that
// `git commit --no-edit` under the default cleanup mode strips NO comment lines
// at all — the recorded message retains git's own `# Conflicts:` block verbatim.
// Narrowing to a first-byte test therefore strictly TIGHTENS the comparison and
// cannot break the ordinary case.
func normalizeCommitMessage(s string) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		kept = append(kept, strings.TrimRight(line, " \t"))
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// violationDetail renders every violation into one operator-readable line, so a
// refusal naming ONE reason still shows the full set the gate found.
func violationDetail(vs []conflictresolve.Violation) string {
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		p := string(v.Reason)
		if v.Path != "" {
			p += " " + v.Path
		}
		if v.Detail != "" {
			p += " (" + v.Detail + ")"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ")
}

// recoverConflictResolution restores the repository to preTip with a clean
// worktree and VERIFIES it, returning whether the postcondition holds.
//
// It deliberately does NOT run under the pass's own context. exec.CommandContext
// kills the child the instant the context is done, so a cancellation while the
// agent ran would make `git merge --abort`, the verification, the reset and the
// clean ALL fail instantly and leave the repository mid-merge — the round-1
// defect. The recovery context is derived with context.WithoutCancel plus its
// OWN bounded timeout, so it outlives cancellation without becoming unbounded.
func recoverConflictResolution(parent context.Context, repoDir, preTip string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), conflictRecoveryTimeout)
	defer cancel()

	// `git merge --abort` is the clean path; ignore its error and let the
	// postcondition decide, because a repository that is no longer mid-merge
	// makes it fail while being exactly what we want.
	_ = gitRun(ctx, repoDir, "merge", "--abort")
	if ok, _ := conflictRepoRestored(ctx, repoDir, preTip); ok {
		return true, ""
	}

	// Escalate: hard reset + clean, then RE-VERIFY. Reporting success without
	// re-verifying would be the same assumption the abort path just violated.
	_ = gitRun(ctx, repoDir, "reset", "--hard", preTip)
	_ = gitRun(ctx, repoDir, "clean", "-fd")
	ok, detail := conflictRepoRestored(ctx, repoDir, preTip)
	return ok, detail
}

// conflictRepoRestored verifies the recovery postcondition: HEAD is preTip AND
// `git status --porcelain` is empty.
func conflictRepoRestored(ctx context.Context, repoDir, preTip string) (bool, string) {
	head, err := gitOut(ctx, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return false, "read HEAD: " + err.Error()
	}
	if head != preTip {
		return false, fmt.Sprintf("HEAD is %s, want pre-merge tip %s", head, preTip)
	}
	status, err := gitOutRaw(ctx, repoDir, "status", "--porcelain")
	if err != nil {
		return false, "read status: " + err.Error()
	}
	if len(bytes.TrimSpace(status)) > 0 {
		return false, "worktree not clean: " + strings.TrimSpace(string(status))
	}
	return true, ""
}

// unmergedPathSet reads `git ls-files --unmerged -z` output into the
// de-duplicated conflicted-path set. Each record is
// "<mode> <oid> <stage>\t<path>" and one path appears once per stage present.
func unmergedPathSet(raw []byte) map[string]bool {
	set := map[string]bool{}
	for _, rec := range splitNUL(raw) {
		if _, path, ok := strings.Cut(rec, "\t"); ok && path != "" {
			set[path] = true
		}
	}
	return set
}

// unmergedStages maps each conflicted path to the stage numbers git recorded
// (1 = base, 2 = ours, 3 = theirs). A path missing stage 2 or 3 is a
// delete/modify conflict — one side removed it.
func unmergedStages(raw []byte) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	for _, rec := range splitNUL(raw) {
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok || path == "" {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) < 3 {
			continue
		}
		var stage int
		if _, err := fmt.Sscanf(fields[2], "%d", &stage); err != nil {
			continue
		}
		if out[path] == nil {
			out[path] = map[int]bool{}
		}
		out[path][stage] = true
	}
	return out
}

// captureConflictBaseline snapshots the merge state git produced: HEAD,
// MERGE_HEAD, the merge message, every NON-conflicted stage-0 index entry, and
// each conflicted path's kind, working bytes, sides and MODE.
func captureConflictBaseline(ctx context.Context, repoDir, head string, conflicted map[string]bool) (conflictresolve.Baseline, error) {
	base := conflictresolve.Baseline{
		HeadSHA:    head,
		Index:      map[string]conflictresolve.IndexEntry{},
		Conflicted: map[string]conflictresolve.ConflictedFile{},
	}

	mergeHead, err := gitOut(ctx, repoDir, "rev-parse", "MERGE_HEAD")
	if err != nil {
		return base, fmt.Errorf("read MERGE_HEAD: %w", err)
	}
	base.MergeHeadSHA = mergeHead

	msg, err := readMergeMessage(ctx, repoDir)
	if err != nil {
		return base, err
	}
	base.MergeMessage = msg

	index, err := readStageZeroIndex(ctx, repoDir)
	if err != nil {
		return base, err
	}
	for path, entry := range index {
		if conflicted[path] {
			continue
		}
		base.Index[path] = entry
	}

	// A read failure RETURNS rather than defaulting to the empty string: an
	// empty baseline config would compare unequal to every observation, or —
	// worse, if the observe side also degraded — make a LATER change
	// undetectable. Surfacing it as conflict_resolution_baseline_capture_failed
	// is the fail-closed direction.
	cfg, err := readGitConfigFn(ctx, repoDir)
	if err != nil {
		return base, err
	}
	base.Config = cfg

	// Same fail-closed discipline as the config read: an empty attribute stream
	// would compare unequal to every observation (or, worse, mask a later
	// change), so a read failure RETURNS as conflict_resolution_baseline_capture_failed.
	attrs, err := readConflictedAttributesFn(ctx, repoDir, sortedStringKeys(conflicted))
	if err != nil {
		return base, err
	}
	base.Attributes = attrs

	unmergedRaw, err := gitOutRaw(ctx, repoDir, "ls-files", "--unmerged", "-z")
	if err != nil {
		return base, fmt.Errorf("read unmerged entries: %w", err)
	}
	stages := unmergedStages(unmergedRaw)

	for path := range conflicted {
		f, err := captureConflictedFile(ctx, repoDir, path, stages[path])
		if err != nil {
			return base, err
		}
		base.Conflicted[path] = f
	}
	return base, nil
}

// captureConflictedFile classifies ONE conflicted path and reads it back.
func captureConflictedFile(ctx context.Context, repoDir, path string, stages map[int]bool) (conflictresolve.ConflictedFile, error) {
	f := conflictresolve.ConflictedFile{Kind: conflictresolve.ConflictContent}

	state, err := readWorkingFile(repoDir, path)
	if err != nil {
		return f, fmt.Errorf("read conflicted path %q: %w", path, err)
	}
	f.MarkerBytes = state.Bytes
	f.Mode = state.Mode

	switch {
	case !stages[2] || !stages[3]:
		// One side removed the path: a delete/modify conflict. Capture BOTH
		// sides' full content so the gate can accept either one (or the
		// deletion) and refuse anything else.
		f.Kind = conflictresolve.ConflictDeleteModify
		if stages[2] {
			b, err := gitOutRaw(ctx, repoDir, "cat-file", "blob", ":2:"+path)
			if err != nil {
				return f, fmt.Errorf("read ours side of %q: %w", path, err)
			}
			f.Sides.Ours, f.Sides.OursPresent = b, true
		}
		if stages[3] {
			b, err := gitOutRaw(ctx, repoDir, "cat-file", "blob", ":3:"+path)
			if err != nil {
				return f, fmt.Errorf("read theirs side of %q: %w", path, err)
			}
			f.Sides.Theirs, f.Sides.TheirsPresent = b, true
		}
	case state.Present && !conflictresolve.ContainsMarkerLine(state.Bytes):
		// Both sides present but git wrote NO markers: it could not present the
		// conflict with hunks, so there is no boundary to confine anything to.
		f.Kind = conflictresolve.ConflictBinary
	}
	return f, nil
}

// readMergeMessage reads .git/MERGE_MSG. An ABSENT file is not an error — it is
// the empty message — but an unreadable one is, because the message is a gate
// input and silently defaulting it to empty would make a REWRITE undetectable.
func readMergeMessage(ctx context.Context, repoDir string) (string, error) {
	gitDir, err := gitOut(ctx, repoDir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", fmt.Errorf("resolve git dir: %w", err)
	}
	b, err := os.ReadFile(filepath.Join(gitDir, "MERGE_MSG"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read MERGE_MSG: %w", err)
	}
	return string(b), nil
}

// readStageZeroIndex reads every stage-0 index entry via `git ls-files -s -z`.
func readStageZeroIndex(ctx context.Context, repoDir string) (map[string]conflictresolve.IndexEntry, error) {
	raw, err := gitOutRaw(ctx, repoDir, "ls-files", "-s", "-z")
	if err != nil {
		return nil, fmt.Errorf("read index: %w", err)
	}
	out := map[string]conflictresolve.IndexEntry{}
	for _, rec := range splitNUL(raw) {
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok || path == "" {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) < 3 || fields[2] != "0" {
			continue
		}
		out[path] = conflictresolve.IndexEntry{Mode: fields[0], OID: fields[1]}
	}
	return out, nil
}

// readWorkingFile reads one working-tree path into a FileState, deriving the
// git mode from the filesystem: a symlink is 120000, an owner-executable
// regular file is 100755, everything else is 100644 — the same octal strings
// `git ls-files -s` reports, so the gate's mode comparison is like-for-like.
func readWorkingFile(repoDir, path string) (conflictresolve.FileState, error) {
	full := filepath.Join(repoDir, filepath.FromSlash(path))
	info, err := os.Lstat(full)
	if errors.Is(err, os.ErrNotExist) {
		return conflictresolve.FileState{}, nil
	}
	if err != nil {
		return conflictresolve.FileState{}, err
	}
	st := conflictresolve.FileState{Present: true}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		st.Mode = "120000"
		target, rerr := os.Readlink(full)
		if rerr != nil {
			return st, rerr
		}
		st.Bytes = []byte(target)
		return st, nil
	case info.Mode()&0o100 != 0:
		st.Mode = "100755"
	default:
		st.Mode = "100644"
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return st, err
	}
	st.Bytes = b
	return st, nil
}

// observeConflictState reads the post-agent state the gate compares against the
// baseline.
func observeConflictState(ctx context.Context, repoDir string, base conflictresolve.Baseline) (conflictresolve.Observed, error) {
	var obs conflictresolve.Observed

	head, err := gitOut(ctx, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return obs, fmt.Errorf("read HEAD: %w", err)
	}
	obs.HeadSHA = head

	// MERGE_HEAD is deliberately tolerant of ABSENCE (the agent committed, or
	// removed the file) — that is a violation the gate names, not a read error
	// that would mask it behind conflict_resolution_observe_failed.
	if mh, err := gitOut(ctx, repoDir, "rev-parse", "MERGE_HEAD"); err == nil {
		obs.MergeHeadSHA = mh
	}
	msg, err := readMergeMessage(ctx, repoDir)
	if err != nil {
		return obs, err
	}
	obs.MergeMessage = msg

	index, err := readStageZeroIndex(ctx, repoDir)
	if err != nil {
		return obs, err
	}
	obs.Index = index

	unmergedRaw, err := gitOutRaw(ctx, repoDir, "ls-files", "--unmerged", "-z")
	if err != nil {
		return obs, fmt.Errorf("read unmerged entries: %w", err)
	}
	obs.Unmerged = sortedStringKeys(unmergedPathSet(unmergedRaw))

	unstagedRaw, err := gitOutRaw(ctx, repoDir, "diff", "--name-only", "-z")
	if err != nil {
		return obs, fmt.Errorf("read unstaged changes: %w", err)
	}
	obs.Unstaged = splitNUL(unstagedRaw)

	untrackedRaw, err := gitOutRaw(ctx, repoDir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return obs, fmt.Errorf("read untracked paths: %w", err)
	}
	obs.Untracked = splitNUL(untrackedRaw)

	// Like the baseline side, a read failure RETURNS (surfacing as
	// conflict_resolution_observe_failed) rather than defaulting to the empty
	// string, which would silently compare unequal and misname the cause.
	cfg, err := readGitConfigFn(ctx, repoDir)
	if err != nil {
		return obs, err
	}
	obs.Config = cfg

	// Re-read the conflicted set's effective attributes over the SAME sorted
	// path list the baseline captured, so the gate compares like for like. A
	// read failure RETURNS (surfacing as conflict_resolution_observe_failed).
	attrs, err := readConflictedAttributesFn(ctx, repoDir, sortedStringKeys(base.Conflicted))
	if err != nil {
		return obs, err
	}
	obs.Attributes = attrs

	obs.Working = map[string]conflictresolve.FileState{}
	for path := range base.Conflicted {
		st, err := readWorkingFile(repoDir, path)
		if err != nil {
			return obs, fmt.Errorf("read conflicted path %q: %w", path, err)
		}
		obs.Working[path] = st
	}
	return obs, nil
}

// sortedKeys returns a map's keys in deterministic order.
func sortedStringKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// logEvent writes one JSON runner-log line. Field values are quoted with %q so
// a path or a git error carrying a quote cannot break the record.
func logEvent(logSink io.Writer, event string, fields map[string]string) {
	if logSink == nil {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, `{"event":%q`, event)
	for _, k := range sortedStringKeys(fields) {
		if fields[k] == "" {
			continue
		}
		fmt.Fprintf(&b, `,%q:%q`, k, fields[k])
	}
	b.WriteString("}\n")
	_, _ = io.WriteString(logSink, b.String())
}

// conflictResolutionAgentInvoker is the agent seam for the pass. Production
// selects the configured coding agent and invokes it on the fetched prompt;
// tests substitute a stub that edits the working tree (or fails, or cancels).
var conflictResolutionAgentInvoker = func(ctx context.Context, cfg config, logSink io.Writer) error {
	promptText, err := os.ReadFile(cfg.promptFile)
	if err != nil {
		return fmt.Errorf("read prompt: %w", err)
	}
	invoker, err := selectInvoker(cfg.agent, apiKeyForAgent(cfg.agent), agentBinaryOverride(cfg.agent, os.Getenv))
	if err != nil {
		return fmt.Errorf("select agent: %w", err)
	}
	agentBaseEnv, _ := agentenv.Env(os.Environ())
	res, err := invoker.Invoke(ctx, agent.Invocation{
		RunID:      cfg.runID,
		Stage:      cfg.stage,
		Prompt:     string(promptText),
		WorkingDir: cfg.workingDir,
		Budget:     agent.Budget{MaxTokens: cfg.maxTokens, Timeout: cfg.timeout},
		// Default-deny the inherited environment, exactly as the ordinary
		// implement invocation does (#2894) — a conflict-resolution pass has no
		// more claim on the runner's ambient bearer token or installation token
		// than any other agent spawn.
		BaseEnv:      agentBaseEnv,
		Env:          map[string]string{},
		ProgressSink: logSink,
	})
	if err != nil {
		return err
	}
	if !res.OK {
		return errors.New("agent reported failure")
	}
	return nil
}

// conflictTreeRefusal is the establishment step's failure: a named reason, an
// operator-readable detail, and the terminal-report category.
type conflictTreeRefusal struct {
	Reason   string
	Detail   string
	Category string
}

// establishConflictResolutionTree provisions the throwaway detached worktree the
// pass runs in, at the run-branch tip fetched fresh from the remote — instead of
// assuming the dispatch checkout sits at that tip (#3340). It returns the tree
// directory, the fully-qualified base ref the pass must merge (the exact
// remote-tracking ref the base fetch refreshed, so fetch and merge name the same
// object for every accepted BaseRef shape), a teardown func that is ALWAYS
// non-nil and best-effort, and a refusal (nil on success).
//
// Every refusal fires BEFORE any WORKING-TREE, BRANCH or CHECKOUT mutation of
// the dispatch checkout — its HEAD, index and files are untouched. The only side
// effects a later refusal can follow are REMOTE-TRACKING-REF refreshes: step (c)
// refreshes refs/remotes/origin/<branch> and step (f) refreshes
// refs/remotes/<baseRemote>/<baseBranch>. Those are ref-only and benign, and are
// not claimed away.
func establishConflictResolutionTree(ctx context.Context, cfg config, client uploadClient,
	issued *upload.IssuedKey, req conflictResolutionRequest, logSink io.Writer) (treeDir, mergeRef string, teardown func(), refusal *conflictTreeRefusal) {

	noop := func() {}

	// (a) The dispatch checkout must be a git work tree to fetch against and to
	// hang the throwaway worktree off. Refused before any tracking ref moves.
	dispatchDir := cfg.workingDir
	if dispatchDir == "" {
		dispatchDir = "."
	}
	if !isGitWorkTree(ctx, dispatchDir) {
		return "", "", noop, &conflictTreeRefusal{
			Reason:   reasonTreeUnavailable,
			Detail:   "dispatch checkout " + dispatchDir + " is empty or not a git work tree",
			Category: "C",
		}
	}

	// (b) Mint a fresh base-auth token for the fetches. NEVER fatal: a mint
	// failure degrades to ambient auth (#1951), it is not a refusal.
	token := mintBaseAuthToken(ctx, cfg, client, issued, logSink)

	// (c) Fetch the run-branch tip and compare it to the trigger's anchor. This
	// refreshes refs/remotes/origin/<branch> — the one ref-only side effect a
	// later refusal may follow.
	tip, err := fetchConflictBranchTip(ctx, dispatchDir, gitops.DefaultRemote, req.Branch, token)
	if err != nil {
		return "", "", noop, &conflictTreeRefusal{
			Reason:   reasonBranchFetchFailed,
			Detail:   "fetch run-branch tip of " + req.Branch + ": " + err.Error(),
			Category: "C",
		}
	}
	// (d) The remote branch must still be at the anchor the trigger authorized.
	// A moved tip means the remote branch advanced past the anchor, so merging
	// into it would push a commit the operator never anchored to. Category B: a
	// decision, retry against the same trigger refuses identically.
	if tip != req.ExpectedHeadSHA {
		return "", "", noop, &conflictTreeRefusal{
			Reason:   reasonUnexpectedHead,
			Detail:   fmt.Sprintf("remote tip of %s is %s, trigger anchored to %s", req.Branch, tip, req.ExpectedHeadSHA),
			Category: "B",
		}
	}

	// (e) Resolve the base ref and derive the (remote, branch) the fetch must
	// target from the QUALIFIED ref, so the fetch and the merge cannot disagree.
	qualified := qualifyMergeRef(ctx, dispatchDir, gitops.DefaultRemote, req.BaseRef)
	baseRemote, baseBranch, ok := conflictBaseFetchTarget(qualified, configuredRemotes(ctx, dispatchDir))
	if !ok {
		return "", "", noop, &conflictTreeRefusal{
			Reason:   reasonBaseRefUnfetchable,
			Detail:   "base ref " + qualified + " has no fetchable <remote>/<branch> shape",
			Category: "B",
		}
	}

	// (f) Fetch the base tip from THAT remote, refreshing
	// refs/remotes/<baseRemote>/<baseBranch> — the exact ref the merge below
	// reads.
	if _, err := fetchConflictBranchTip(ctx, dispatchDir, baseRemote, baseBranch, token); err != nil {
		return "", "", noop, &conflictTreeRefusal{
			Reason:   reasonBaseFetchFailed,
			Detail:   fmt.Sprintf("fetch base %s from %s: %s", baseBranch, baseRemote, err.Error()),
			Category: "C",
		}
	}
	// The pass must merge the ref the fetch just refreshed. Handing it the
	// remote-tracking ref (which begins with refs/, so qualifyMergeRef passes it
	// through unchanged) makes fetch and merge name the SAME object for the bare,
	// remote-prefixed AND fully-qualified refs/heads/* shapes alike (#3340).
	mergeRef = fmt.Sprintf("refs/remotes/%s/%s", baseRemote, baseBranch)

	// (g) Provision the throwaway detached worktree at the anchor. The parent
	// dir is created OUTSIDE the repo (os.MkdirTemp), the tree a child of it, so
	// teardown is a single RemoveAll of the parent plus the worktree unregister.
	parent, err := os.MkdirTemp(conflictTreeTempDir, "fishhawk-conflict-*")
	if err != nil {
		return "", "", noop, &conflictTreeRefusal{
			Reason:   reasonTreeProvisionFailed,
			Detail:   "create throwaway parent dir: " + err.Error(),
			Category: "C",
		}
	}
	tree := filepath.Join(parent, "tree")
	if _, err := gitOutRaw(ctx, dispatchDir, "worktree", "add", "--detach", tree, tip); err != nil {
		// Leave no half-registered worktree or leftover dir behind.
		_ = os.RemoveAll(parent)
		_ = gitRun(ctx, dispatchDir, "worktree", "prune")
		return "", "", noop, &conflictTreeRefusal{
			Reason:   reasonTreeProvisionFailed,
			Detail:   "git worktree add: " + err.Error(),
			Category: "C",
		}
	}
	logEvent(logSink, "conflict_resolution_tree_established", map[string]string{
		"path": tree, "head_sha": tip, "base_remote": baseRemote,
		"base_branch": baseBranch, "merge_ref": mergeRef,
	})

	teardown = func() {
		// Detached + bounded, mirroring reportCtx: teardown must outlive a
		// cancellation of the pass so the throwaway tree is never stranded.
		tctx, tcancel := context.WithTimeout(context.WithoutCancel(ctx), conflictRecoveryTimeout)
		defer tcancel()
		if err := gitRun(tctx, dispatchDir, "worktree", "remove", "--force", tree); err != nil {
			// A locked worktree refuses `remove --force` and is skipped by a plain
			// `prune`, so unlock first, then RemoveAll, then prune the now-missing
			// registration (the provisionAcceptanceTree ladder).
			_ = gitRun(tctx, dispatchDir, "worktree", "unlock", tree)
			_ = os.RemoveAll(tree)
			_ = gitRun(tctx, dispatchDir, "worktree", "prune")
		}
		_ = os.RemoveAll(parent)
		logEvent(logSink, "conflict_resolution_tree_removed", map[string]string{"path": tree})
	}
	return tree, mergeRef, teardown, nil
}

// runConflictResolutionStage is the runner's whole conflict-resolution stage:
// it ESTABLISHES its own detached throwaway tree at the run-branch tip (#3340),
// performs the local pass there, and then — this is the half round 1 omitted —
// PUBLISHES a passing result and REPORTS the terminal outcome either way.
// Neither arm may leave the stage in `running`.
//
// It returns the process exit code.
func runConflictResolutionStage(ctx context.Context, cfg config, req conflictResolutionRequest,
	client uploadClient, issued *upload.IssuedKey, logSink io.Writer) int {

	// Establish the tree the pass runs in BEFORE anything else. On a refusal,
	// report the terminal failure with the refusal's category so the stage never
	// strands in `running`, exactly as the refused-pass arm below does.
	treeDir, mergeRef, teardown, refusal := establishConflictResolutionTree(ctx, cfg, client, issued, req, logSink)
	if refusal != nil {
		reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), conflictRecoveryTimeout)
		defer cancel()
		reason := refusal.Reason
		if refusal.Detail != "" {
			reason += ": " + refusal.Detail
		}
		if _, err := client.ShipPullRequest(reportCtx, upload.ShipPullRequestArgs{
			RunID:      cfg.runID,
			StageID:    cfg.stageID,
			PrivateKey: issued.PrivateKey,
			Outcome:    "failed",
			Category:   refusal.Category,
			Reason:     reason,
		}); err != nil {
			logEvent(logSink, "conflict_resolution_report_failed", map[string]string{"error": err.Error()})
			return exitFailure
		}
		logEvent(logSink, "runner_failed", map[string]string{"reason": refusal.Reason, "detail": refusal.Detail})
		return exitFailure
	}
	// Registered BEFORE the pass so every return path tears the tree down.
	defer teardown()

	// The pass, the agent and the push all run in the throwaway tree. Redirect
	// cfg.workingDir so the agent invoker spawns there, and merge the ref the
	// fetch refreshed. No other cfg.workingDir reader remains below the redirect:
	// the pass and the push both take repoDir as an explicit argument (treeDir),
	// and the terminal reports key off cfg.runID/cfg.stageID, never workingDir.
	cfg.workingDir = treeDir
	req.BaseRef = mergeRef
	repoDir := treeDir

	invoke := func(ictx context.Context) error {
		return conflictResolutionAgentInvoker(ictx, cfg, logSink)
	}
	res := runConflictResolutionPass(ctx, repoDir, gitops.DefaultRemote, req, invoke, logSink)

	// The report itself must outlive a cancellation of the pass: a stage whose
	// refusal is never reported strands in `running` exactly as a stage that
	// never reported success would, and the cancellation is precisely when the
	// operator most needs the named reason.
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), conflictRecoveryTimeout)
	defer cancel()

	if res.refused() {
		reason := res.Reason
		if res.Detail != "" {
			reason += ": " + res.Detail
		}
		if !res.Recovered {
			reason += " [RECOVERY INCOMPLETE: " + res.RecoveryDetail + "]"
		}
		if _, err := client.ShipPullRequest(reportCtx, upload.ShipPullRequestArgs{
			RunID:      cfg.runID,
			StageID:    cfg.stageID,
			PrivateKey: issued.PrivateKey,
			Outcome:    "failed",
			// Category B: the pass reached a decision and the decision was no.
			// A retry against the same repository state would refuse
			// identically, so this is not the retryable class.
			Category: "B",
			Reason:   reason,
		}); err != nil {
			logEvent(logSink, "conflict_resolution_report_failed", map[string]string{"error": err.Error()})
			return exitFailure
		}
		logEvent(logSink, "runner_failed", map[string]string{"reason": res.Reason, "detail": res.Detail})
		return exitFailure
	}

	// Publish through the runner's existing authorized write path.
	pushed, err := pushConflictResolutionCommit(reportCtx, cfg, client, issued, repoDir, req.Branch, res.HeadSHA, logSink)
	if err != nil {
		// The commit exists locally but never reached the remote. Report the
		// failure so the stage settles; the pass's own commit is left in place
		// for the operator, whose next rebase invocation takes the fail-closed
		// 422 naming this failed pass.
		if _, serr := client.ShipPullRequest(reportCtx, upload.ShipPullRequestArgs{
			RunID:      cfg.runID,
			StageID:    cfg.stageID,
			PrivateKey: issued.PrivateKey,
			Outcome:    "failed",
			// Category C: a push failure is transport, not judgment.
			Category: "C",
			Reason:   "conflict_resolution_push_failed: " + err.Error(),
		}); serr != nil {
			logEvent(logSink, "conflict_resolution_report_failed", map[string]string{"error": serr.Error()})
		}
		logEvent(logSink, "runner_failed", map[string]string{"reason": "conflict_resolution_push_failed", "detail": err.Error()})
		return exitFailure
	}

	if _, err := client.ShipPullRequest(reportCtx, upload.ShipPullRequestArgs{
		RunID:      cfg.runID,
		StageID:    cfg.stageID,
		PrivateKey: issued.PrivateKey,
		Outcome:    "conflict_resolution_pushed",
		Branch:     req.Branch,
		HeadSHA:    pushed,
		BaseSHA:    res.BaseSHA,
	}); err != nil {
		logEvent(logSink, "conflict_resolution_report_failed", map[string]string{"error": err.Error()})
		return exitFailure
	}
	logEvent(logSink, "conflict_resolution_pushed", map[string]string{
		"branch": req.Branch, "head_sha": pushed, "base_sha": res.BaseSHA,
	})
	return exitOK
}

// pushConflictResolutionCommit mints a fresh installation token and pushes the
// merge commit through gitops.PushCommittedBranch, which PINS the push to the
// gate-authorized res.HeadSHA (so a local HEAD moved after the gate ran cannot
// be what gets published) and then CONFIRMS the remote tip advanced to it. It
// returns the confirmed remote head.
func pushConflictResolutionCommit(ctx context.Context, cfg config, client uploadClient,
	issued *upload.IssuedKey, repoDir, branch, headSHA string, logSink io.Writer) (string, error) {

	repoSlug := cfg.githubRepo
	if repoSlug == "" {
		repoSlug = os.Getenv("GITHUB_REPOSITORY")
	}
	owner, repoName, ok := strings.Cut(repoSlug, "/")
	if !ok || owner == "" || repoName == "" {
		return "", fmt.Errorf("github repo %q is not owner/name", repoSlug)
	}
	token, err := mintImplementToken(ctx, cfg, client, issued, logSink)
	if err != nil {
		return "", fmt.Errorf("mint push token: %w", err)
	}
	pushRes, err := newPusher().PushCommittedBranch(ctx, gitops.PushCommittedBranchArgs{
		RepoDir:   repoDir,
		Branch:    branch,
		RemoteURL: fmt.Sprintf("https://github.com/%s/%s", owner, repoName),
		PushToken: token,
		HeadSHA:   headSHA,
	})
	if err != nil {
		return "", err
	}
	return pushRes.RemoteHeadSHA, nil
}
