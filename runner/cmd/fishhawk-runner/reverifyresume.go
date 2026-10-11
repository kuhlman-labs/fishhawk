package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// The reverify resume's building blocks (#4190).
//
// A standalone open-PR stage whose committed-tree verify failed category C
// after a SUCCESSFUL agent pass pins the last verify_run head
// (verifyheadpin.go) and reports a `reverify` held-commit checkpoint. The next
// dispatch is served that checkpoint and, instead of re-running the agent, runs
// the helpers below over the pinned commit, in this order:
//
//  1. reverifyPrecheck — can this held commit be resumed at all? A miss is a
//     FALLBACK token: the ordinary agent pass runs in the same dispatch.
//  2. reverifyPrePushGates — the reproducible pre-push gates
//     openPRAndShipArtifact's verifyCommit closure and CommitAndPush run on an
//     ordinary push (scope completeness #1151, binding assertions #1171, the
//     post-commit scope assertion #980). A failure is PERMANENT category B: no
//     agent fallback, no checkpoint, nothing published.
//  3. reverifyHeldCommit — ONE full-form committed-tree verify, plus one re-run
//     only on an infra signature. A failure is category C.
//  4. synthesizeCommitOnParent — a DCO-signed commit of the held tree on its
//     recorded parent, which the push-resume tail publishes.
//
// The one ordinary-path gate that is NOT reproduced is created-out-of-scope
// (#818): it reads `git ls-files --others` in the WORKING tree the agent left,
// and a held commit has no working tree. That is acceptable because the held
// commit is a committed snapshot whose COMPLETE changed-path set
// reverifyPrePushGates checks against the scope (OutOfScopePaths): a file the
// agent created outside scope is either in that set (and refused) or not in the
// commit at all (and so not published).

// resumeKindReverify is the held_commit_resume_kind discriminator for the
// reverify resume. WIRE VALUE: byte-identical to the backend's
// resumeKindReverify and to testdata/wire/reverify_failure_report.json.
const resumeKindReverify = "reverify"

// Bounded reverify tokens. Callers and tests assert token IDENTITY, never prose.
const (
	// FALLBACK (reverifyPrecheck): the ordinary agent pass runs instead.
	reverifyFallbackVerifyCommandAbsent = "verify_command_absent"
	reverifyFallbackScopeUnavailable    = "scope_unavailable"
	reverifyFallbackHeldCommitAbsent    = "held_commit_absent"
	reverifyFallbackBaseMismatch        = "held_commit_base_mismatch"
	// reverifyFallbackBaseNotOnBase: the held commit's parent is not the tip of,
	// nor an ancestor of the tip of, a FRESHLY fetched origin/<declared base>.
	// A standalone run cuts from a freshly fetched base (ADR-035 / #861), so a
	// parent off that base could launder a foreign commit into the PR.
	reverifyFallbackBaseNotOnBase = "held_commit_base_not_on_base"
	// reverifyFallbackBaseUnverifiable: the fresh fetch failed or ancestry
	// could not be decided — not evidence the base is good, so fall back.
	reverifyFallbackBaseUnverifiable = "held_commit_base_unverifiable"

	// PERMANENT category B (reverifyPrePushGates): the held tree fails a gate
	// an ordinary push would have refused it on.
	reverifyGateOutOfScope                  = "held_commit_out_of_scope"
	reverifyGateScopeFilesMissing           = "held_commit_scope_files_missing"
	reverifyGateBindingAssertionUnsatisfied = "held_commit_binding_assertion_unsatisfied"
	// reverifyGateUnevaluable: a gate could not be evaluated (git failed on a
	// present commit). Category C, permanent: nothing is published and the next
	// retry is an ordinary agent pass, which runs every gate itself.
	reverifyGateUnevaluable = "held_commit_gate_unevaluable"

	// reverifyHeldCommit, category C.
	reverifyGateNotExecuted     = "reverify_gate_not_executed"     // transient
	reverifyFailedOutsideChange = "reverify_failed_outside_change" // transient
	reverifyFailedInChange      = "reverify_failed_in_change"      // permanent
)

// reverifyMaxInfraReruns bounds reverifyHeldCommit's infra-signature re-run.
const reverifyMaxInfraReruns = 1

// reverifyHeld is everything the reverify helpers read about the served held
// commit and the stage.
type reverifyHeld struct {
	repoDir   string
	verifyCmd string
	heldSHA   string
	// baseSHA is the served held_commit_base_sha: the held commit's recorded
	// parent.
	baseSHA string
	// baseBranch is the declared base (resolveImplementBaseRef) the precheck
	// freshly fetches; pushToken authenticates that fetch ("" = ambient auth).
	baseBranch string
	pushToken  string
	// scopeFiles is the SERVED effective scope (approved amendments folded in
	// by the backend); bindingAssertions and scopeExemptions are the served
	// operator declarations the ordinary gates read.
	scopeFiles        []upload.ScopeFile
	bindingAssertions []upload.BindingAssertion
	scopeExemptions   []scopeExemption
}

// reverifyVerdict is the outcome of reverifyPrePushGates / reverifyHeldCommit.
// An empty token is a pass. permanent=true means report WITHOUT a checkpoint
// (the next retry is an ordinary agent pass); false means re-arm reverify.
type reverifyVerdict struct {
	token     string
	category  string
	permanent bool
	detail    string
}

func (v reverifyVerdict) ok() bool { return v.token == "" }

// reverifyFetchBaseTipFn is the precheck's fresh-fetch seam; production is
// gitops.FetchBaseTip against gitops.DefaultRemote.
var reverifyFetchBaseTipFn = gitops.FetchBaseTip

// reverifyPrecheck decides whether the held commit can be resumed, returning
// "" when it can or a FALLBACK token plus detail when the ordinary agent pass
// must run instead. Rows, in order:
//
//   - verify_command_absent: no verify command, so nothing could certify it;
//   - scope_unavailable: no served scope to gate it against;
//   - held_commit_absent: `cat-file -e <held>^{commit}` fails (an ephemeral
//     host, a gc'd object);
//   - held_commit_base_mismatch: held^ is not the served base;
//   - held_commit_base_unverifiable / held_commit_base_not_on_base: after a
//     FRESH fetch of origin/<declared base>, held^ must equal or be an ancestor
//     of that tip (ADR-035). The held^ == base row alone is circular — both
//     values came from the runner that pinned the commit.
//
// Nothing here publishes or mutates anything but the fetched tracking ref.
func reverifyPrecheck(ctx context.Context, h reverifyHeld) (token, detail string) {
	if strings.TrimSpace(h.verifyCmd) == "" {
		return reverifyFallbackVerifyCommandAbsent, "no executor.verify command to re-verify the held commit with"
	}
	if len(scopePaths(h.scopeFiles)) == 0 {
		return reverifyFallbackScopeUnavailable, "no served scope.files to gate the held commit against"
	}
	if err := exec.CommandContext(ctx, "git", "-C", h.repoDir, "cat-file", "-e", h.heldSHA+"^{commit}").Run(); err != nil {
		return reverifyFallbackHeldCommitAbsent, fmt.Sprintf("held commit %s is not present in %s: %v", h.heldSHA, h.repoDir, err)
	}
	parent, err := gitRevParseIn(ctx, h.repoDir, h.heldSHA+"^")
	if err != nil || parent == "" || parent != h.baseSHA {
		return reverifyFallbackBaseMismatch, fmt.Sprintf("held commit %s parent %q != served base %q (err %v)", h.heldSHA, parent, h.baseSHA, err)
	}
	if h.baseBranch == "" {
		return reverifyFallbackBaseUnverifiable, "no declared base branch to fetch"
	}
	tip, err := reverifyFetchBaseTipFn(ctx, h.repoDir, gitops.DefaultRemote, h.baseBranch, h.pushToken)
	if err != nil || tip == "" {
		return reverifyFallbackBaseUnverifiable, fmt.Sprintf("fresh fetch of %s/%s: tip %q (err %v)", gitops.DefaultRemote, h.baseBranch, tip, err)
	}
	// `git merge-base --is-ancestor A B` exits 0 when A is B or an ancestor of
	// it, 1 when it is not, anything else on error. ONLY exit 0 passes.
	if ancErr := exec.CommandContext(ctx, "git", "-C", h.repoDir, "merge-base", "--is-ancestor", parent, tip).Run(); ancErr != nil {
		var exitErr *exec.ExitError
		if errors.As(ancErr, &exitErr) && exitErr.ExitCode() == 1 {
			return reverifyFallbackBaseNotOnBase, fmt.Sprintf("held commit parent %s is not on freshly fetched %s/%s (tip %s)", parent, gitops.DefaultRemote, h.baseBranch, tip)
		}
		return reverifyFallbackBaseUnverifiable, fmt.Sprintf("cannot decide ancestry of %s vs %s/%s tip %s: %v", parent, gitops.DefaultRemote, h.baseBranch, tip, ancErr)
	}
	return "", ""
}

// reverifyPrePushGates re-runs, over the held commit, every pre-push gate an
// ordinary standalone open-PR push runs that a committed snapshot can
// reproduce, with the SAME helpers so the two paths cannot disagree:
//
//   - the post-commit scope assertion (#980): gitops.OutOfScopePaths, the
//     shared body of CommitAndPush's assertCommitInScope;
//   - the scope-completeness shortfall gate (#1151): gitops.MissingScopeFiles
//     over the served effective scope, minus the served operator exemptions
//     (partitionExemptedMissing), with the ordinary path's message;
//   - the binding-assertion gate (#1171): gitops.EvaluateBindingAssertions over
//     the served assertions.
//
// Any failure is PERMANENT category B — never a fallback to the agent, never a
// publish. Unlike the ordinary path, a sole scope or assertion shortfall does
// not park for an operator exempt decision: there is no fresh push to park.
// created-out-of-scope (#818) is the one gate not reproduced; see the file
// header.
func reverifyPrePushGates(ctx context.Context, h reverifyHeld) reverifyVerdict {
	gateScope := scopePaths(h.scopeFiles)
	unevaluable := func(gate string, err error) reverifyVerdict {
		return reverifyVerdict{token: reverifyGateUnevaluable, category: "C", permanent: true,
			detail: fmt.Sprintf("%s gate over held commit %s: %v", gate, h.heldSHA, err)}
	}
	outside, err := gitops.OutOfScopePaths(ctx, h.repoDir, h.heldSHA, gateScope)
	if err != nil {
		return unevaluable("scope assertion", err)
	}
	if len(outside) > 0 {
		return reverifyVerdict{token: reverifyGateOutOfScope, category: "B", permanent: true,
			detail: fmt.Sprintf("held commit %s contains %d path(s) outside the declared scope.files: %s",
				h.heldSHA, len(outside), strings.Join(outside, ", "))}
	}
	missing, committed, err := gitops.MissingScopeFiles(ctx, h.repoDir, h.heldSHA, gateScope)
	if err != nil {
		return unevaluable("scope completeness", err)
	}
	if exempted, remaining := partitionExemptedMissing(missing, h.scopeExemptions); len(remaining) > 0 {
		return reverifyVerdict{token: reverifyGateScopeFilesMissing, category: "B", permanent: true,
			detail: missingScopeFilesMessage(h.scopeFiles, remaining, exempted, len(gateScope), len(committed))}
	}
	if len(h.bindingAssertions) > 0 {
		results, err := gitops.EvaluateBindingAssertions(ctx, h.repoDir, h.heldSHA, toGitopsBindingAssertions(h.bindingAssertions))
		if err != nil {
			return unevaluable("binding assertion", err)
		}
		if unsatisfied := gitops.UnsatisfiedBindingAssertions(results); len(unsatisfied) > 0 {
			return reverifyVerdict{token: reverifyGateBindingAssertionUnsatisfied, category: "B", permanent: true,
				detail: fmt.Sprintf("%d of %d declared binding assertion(s) not satisfied by held commit %s: %s",
					len(unsatisfied), len(results), h.heldSHA, gitops.FormatUnsatisfied(unsatisfied))}
		}
	}
	return reverifyVerdict{}
}

// reverifyVerifyFn is reverifyHeldCommit's verify seam: production is the SAME
// runVerifyCommittedTree invocation the committed-tree gate's success decision
// uses.
var reverifyVerifyFn = runVerifyCommittedTree

// reverifyResult is reverifyHeldCommit's outcome: the verdict, every verify_run
// event (for the trace), the LAST run's raw output (the caller redacts and
// bounds it before it reaches a reason), and how many runs executed.
type reverifyResult struct {
	verdict reverifyVerdict
	events  []agent.Event
	output  string
	runs    int
}

// reverifyHeldCommit runs ONE full-form committed-tree verify over the held
// commit — a nil package set, so FISHHAWK_VERIFY_PACKAGES is absent and
// `scripts/test verify` runs its full loop plus the patch-coverage gate — and
// re-runs it once only when an EXECUTED failure carries an infra signature
// (isVerifyInfraFailure, the #2645 absorb's classifier). Every verdict is
// category C:
//
//   - passed → ok;
//   - never executed (refused / unavailable / skipped), timed out,
//     lock-contended, or an infra signature that persists → reverify_gate_not_executed,
//     transient (the tree was never judged);
//   - an executed failure whose failing packages all lie OUTSIDE the change
//     (verifyFailureScopeRelation over the served scope) → reverify_failed_outside_change,
//     transient;
//   - anything else, including undecidable output → reverify_failed_in_change,
//     permanent.
func reverifyHeldCommit(ctx context.Context, h reverifyHeld, timeout time.Duration) reverifyResult {
	var res reverifyResult
	run := func() (string, string, gateDisposition) {
		ev, out, outcome, disp := reverifyVerifyFn(ctx, h.verifyCmd, h.repoDir, h.heldSHA, timeout, nil)
		res.events = append(res.events, ev)
		res.output = out
		res.runs++
		return out, outcome, disp
	}
	out, outcome, disp := run()
	for i := 0; i < reverifyMaxInfraReruns && outcome == "failed" && disp == gateExecuted &&
		isVerifyInfraFailure(out) && !isVerifyLockContended(out); i++ {
		out, outcome, disp = run()
	}
	notExecuted := func(why string) reverifyResult {
		res.verdict = reverifyVerdict{token: reverifyGateNotExecuted, category: "C", detail: why}
		return res
	}
	switch {
	case outcome == "passed":
		return res
	case outcome == "skipped":
		return notExecuted("the verify gate never ran the command (skipped)")
	case disp.neverExecutedInfra():
		return notExecuted("the verify gate never executed (" + disp.String() + ")")
	case disp == gateTimedOut:
		return notExecuted("the verify gate timed out before a verdict")
	case isVerifyLockContended(out):
		return notExecuted("the verify lock was held by another runner verify")
	case isVerifyInfraFailure(out):
		return notExecuted("an infrastructure failure signature persisted across the re-run")
	}
	if verifyFailureScopeRelation(parseVerifyFailures(out), verifyScopePackages(scopePaths(h.scopeFiles))) == verifyRelationOutside {
		res.verdict = reverifyVerdict{token: reverifyFailedOutsideChange, category: "C",
			detail: "every failing package lies outside the change"}
		return res
	}
	res.verdict = reverifyVerdict{token: reverifyFailedInChange, category: "C", permanent: true,
		detail: "the held commit failed verify inside (or not provably outside) the change"}
	return res
}

// synthesizeCommitOnParent builds a commit whose tree is treeSHA and whose
// parent is the EXPLICIT parentSHA, without touching the working tree, the index
// or any branch: `git commit-tree <tree> -p <parent>`. The reverify resume uses
// it to replace the bot-identity WIP commit with a DCO-signed one carrying the
// same tree and parent (so `git patch-id --stable` is unchanged);
// synthesizeVerifiedCommit delegates to it with HEAD as the parent.
//
// DCO: `git commit-tree` has no --signoff, so the message gets an explicit
// `Signed-off-by: <author name> <author email>` trailer through
// `git interpret-trailers`. The identity defaults to gitops.DefaultAuthorName /
// DefaultAuthorEmail, as CommitAndPush does.
//
// The result is RE-PROVED before it is returned: its tree must equal treeSHA
// and its parent must equal parentSHA.
func synthesizeCommitOnParent(ctx context.Context, repoDir, treeSHA, parentSHA, commitMessage, authorName, authorEmail string) (string, error) {
	if treeSHA == "" {
		return "", errors.New("synthesize: no tree")
	}
	if parentSHA == "" {
		return "", errors.New("synthesize: no parent")
	}
	if strings.TrimSpace(commitMessage) == "" {
		return "", errors.New("synthesize: empty commit message")
	}
	if authorName == "" {
		authorName = gitops.DefaultAuthorName
	}
	if authorEmail == "" {
		authorEmail = gitops.DefaultAuthorEmail
	}

	trailer := exec.CommandContext(ctx, "git", "-C", repoDir, "interpret-trailers",
		"--if-exists", "addIfDifferent", "--trailer", "Signed-off-by: "+authorName+" <"+authorEmail+">")
	trailer.Stdin = strings.NewReader(strings.TrimRight(commitMessage, "\n") + "\n")
	msg, err := trailer.Output()
	if err != nil {
		return "", fmt.Errorf("synthesize: add Signed-off-by trailer: %w", err)
	}

	commit := exec.CommandContext(ctx, "git", "-C", repoDir, "commit-tree", treeSHA, "-p", parentSHA)
	commit.Stdin = strings.NewReader(string(msg))
	commit.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME="+authorName, "GIT_AUTHOR_EMAIL="+authorEmail,
		"GIT_COMMITTER_NAME="+authorName, "GIT_COMMITTER_EMAIL="+authorEmail)
	var stderr strings.Builder
	commit.Stderr = &stderr
	out, err := commit.Output()
	if err != nil {
		return "", fmt.Errorf("synthesize: git commit-tree %s -p %s: %w (%s)", treeSHA, parentSHA, err, strings.TrimSpace(stderr.String()))
	}
	commitSHA := strings.TrimSpace(string(out))

	if tree, terr := gitRevParseIn(ctx, repoDir, commitSHA+"^{tree}"); terr != nil || tree != treeSHA {
		return "", fmt.Errorf("synthesize: commit %s tree %q != tree %q (err %v)", commitSHA, tree, treeSHA, terr)
	}
	if parent, perr := gitRevParseIn(ctx, repoDir, commitSHA+"^"); perr != nil || parent != parentSHA {
		return "", fmt.Errorf("synthesize: commit %s parent %q != parent %q (err %v)", commitSHA, parent, parentSHA, perr)
	}
	return commitSHA, nil
}
