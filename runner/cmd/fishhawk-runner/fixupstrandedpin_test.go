package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

const (
	strandedRunID   = "11111111-2222-3333-4444-555555555555"
	strandedStageID = "22222222-3333-4444-5555-666666666666"
)

// strandedGit runs git in dir and returns its trimmed stdout, failing the test
// on error.
func strandedGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir,
		"-c", "user.name=t", "-c", "user.email=t@e", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// strandedCommit writes name into dir and commits it, returning the new HEAD.
func strandedCommit(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	strandedGit(t, dir, "add", name)
	strandedGit(t, dir, "commit", "-q", "-m", name)
	return strandedGit(t, dir, "rev-parse", "HEAD")
}

// strandedRepo builds a repository with a base commit B and returns (dir, B).
func strandedRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	strandedGit(t, dir, "init", "-q")
	return dir, strandedCommit(t, dir, "base.txt")
}

// strandedCheckpointRefs lists every ref under refs/fishhawk/checkpoints/.
func strandedCheckpointRefs(t *testing.T, dir string) string {
	t.Helper()
	return strandedGit(t, dir, "for-each-ref", "--format=%(refname)", checkpointRefPrefix)
}

func strandedCfg() config {
	return config{runID: strandedRunID, stageID: strandedStageID}
}

// TestPinStrandedFixupCommit_PinsLocalCommit: HEAD is a commit C on top of the
// base B, so C is pinned at the -fixup- ref, read back through git (the
// committed ref, not the return value), and the clause names both. MECHANISM:
// HEAD != base by construction, so only the `if head == baseTipSHA {` guard
// separates this from the silent no-commit arm; mutating it to `if head != "" {`
// returns before the pin and the ref is ABSENT.
func TestPinStrandedFixupCommit_PinsLocalCommit(t *testing.T) {
	repo, base := strandedRepo(t)
	head := strandedCommit(t, repo, "fixup.txt")

	var log strings.Builder
	clause := pinStrandedFixupCommit(context.Background(), strandedCfg(), &log, repo, base)

	ref := checkpointRef(strandedRunID, strandedStageID) + "-fixup-" + head[:12]
	if got := strandedGit(t, repo, "rev-parse", ref); got != head {
		t.Fatalf("pinned ref %s = %q, want HEAD %q\n%s", ref, got, head, log.String())
	}
	want := fmt.Sprintf("; fix-up commit %s never reached the PR branch; its work is pinned at %s (inspect with `git show %s`, recover with `git cherry-pick %s`)", head, ref, head, head)
	if clause != want {
		t.Errorf("clause = %q\nwant %q", clause, want)
	}
	for _, w := range []string{`"event":"fixup_stranded_commit_pinned"`, `"ref":"` + ref + `"`, `"base_sha":"` + base + `"`} {
		if !strings.Contains(log.String(), w) {
			t.Errorf("log missing %s:\n%s", w, log.String())
		}
	}
}

// TestPinStrandedFixupCommit_NoLocalCommit: HEAD == base is the common
// no-commit shape — nothing pinned, empty clause, and SILENT (no not-pinned
// line).
func TestPinStrandedFixupCommit_NoLocalCommit(t *testing.T) {
	repo, base := strandedRepo(t)
	var log strings.Builder
	if clause := pinStrandedFixupCommit(context.Background(), strandedCfg(), &log, repo, base); clause != "" {
		t.Errorf("clause = %q, want empty", clause)
	}
	if refs := strandedCheckpointRefs(t, repo); refs != "" {
		t.Errorf("checkpoint refs = %q, want none", refs)
	}
	if log.Len() != 0 {
		t.Errorf("no-commit arm must be silent, logged:\n%s", log.String())
	}
}

// TestPinStrandedFixupCommit_BaseUnresolved: no base tip, no anchor — nothing
// is pinned even though HEAD carries a commit, and base_unresolved is named.
func TestPinStrandedFixupCommit_BaseUnresolved(t *testing.T) {
	repo, _ := strandedRepo(t)
	strandedCommit(t, repo, "fixup.txt")
	var log strings.Builder
	if clause := pinStrandedFixupCommit(context.Background(), strandedCfg(), &log, repo, ""); clause != "" {
		t.Errorf("clause = %q, want empty", clause)
	}
	if refs := strandedCheckpointRefs(t, repo); refs != "" {
		t.Errorf("checkpoint refs = %q, want none", refs)
	}
	assertNotPinnedReason(t, log.String(), strandedNotPinnedBaseUnresolved)
}

// TestPinStrandedFixupCommit_NotDescendant: HEAD is an orphan root commit, a
// history foreign to the base. MECHANISM: HEAD != base by construction, so
// deleting the ancestry check pins the foreign commit and the absent-ref
// assertion goes RED.
func TestPinStrandedFixupCommit_NotDescendant(t *testing.T) {
	repo, base := strandedRepo(t)
	strandedGit(t, repo, "checkout", "-q", "--orphan", "foreign")
	strandedCommit(t, repo, "foreign.txt")
	var log strings.Builder
	if clause := pinStrandedFixupCommit(context.Background(), strandedCfg(), &log, repo, base); clause != "" {
		t.Errorf("clause = %q, want empty", clause)
	}
	if refs := strandedCheckpointRefs(t, repo); refs != "" {
		t.Errorf("a foreign HEAD was pinned: %q", refs)
	}
	assertNotPinnedReason(t, log.String(), strandedNotPinnedNotDescendant)
}

// TestPinStrandedFixupCommit_AncestryProbeError: a base the object store does
// not hold makes `git merge-base --is-ancestor` fail (not exit 1); that is
// not_descendant too, never a pin.
func TestPinStrandedFixupCommit_AncestryProbeError(t *testing.T) {
	repo, _ := strandedRepo(t)
	strandedCommit(t, repo, "fixup.txt")
	var log strings.Builder
	absent := strings.Repeat("ab", 20)
	if clause := pinStrandedFixupCommit(context.Background(), strandedCfg(), &log, repo, absent); clause != "" {
		t.Errorf("clause = %q, want empty", clause)
	}
	if refs := strandedCheckpointRefs(t, repo); refs != "" {
		t.Errorf("checkpoint refs = %q, want none", refs)
	}
	assertNotPinnedReason(t, log.String(), strandedNotPinnedNotDescendant)
	if !strings.Contains(log.String(), "merge-base --is-ancestor") {
		t.Errorf("probe error detail missing:\n%s", log.String())
	}
}

// TestGitIsAncestor covers the probe's three exits directly.
func TestGitIsAncestor(t *testing.T) {
	repo, base := strandedRepo(t)
	head := strandedCommit(t, repo, "fixup.txt")
	ctx := context.Background()
	if ok, err := gitIsAncestor(ctx, repo, base, head); !ok || err != nil {
		t.Errorf("base->head = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := gitIsAncestor(ctx, repo, head, base); ok || err != nil {
		t.Errorf("head->base = (%v, %v), want (false, nil)", ok, err)
	}
	if _, err := gitIsAncestor(ctx, repo, strings.Repeat("cd", 20), head); err == nil {
		t.Error("an absent object must be an error, not a verdict")
	}
}

// TestPinStrandedFixupCommit_HeadUnresolvable: a HEAD read error, AND an empty
// HEAD with a nil error (the shape main_test.go's default fixupLocalHead seam
// installs, approval condition C2), both refuse as head_unresolvable BEFORE the
// ancestry probe — so a default-seamed run() test never reaches git merge-base
// or git update-ref.
func TestPinStrandedFixupCommit_HeadUnresolvable(t *testing.T) {
	for _, tc := range []struct {
		name string
		head string
		err  error
	}{
		{"read error", "", errors.New("rev-parse HEAD: not a git repository")},
		{"empty head", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFakeFixupLocalHead(t, tc.head, tc.err)
			withStrandedAncestry(t, func(context.Context, string, string, string) (bool, error) {
				t.Error("the ancestry probe ran on an unresolvable HEAD")
				return true, nil
			})
			withStrandedPin(t, func(context.Context, string, string, string) error {
				t.Error("the pin ran on an unresolvable HEAD")
				return nil
			})
			var log strings.Builder
			if clause := pinStrandedFixupCommit(context.Background(), strandedCfg(), &log, "unused", "base"); clause != "" {
				t.Errorf("clause = %q, want empty", clause)
			}
			assertNotPinnedReason(t, log.String(), strandedNotPinnedHeadUnresolvable)
		})
	}
}

// TestPinStrandedFixupCommit_PinFailure: the pin itself fails — empty clause
// and the existing checkpoint_ref_pin_failed line, never a clause naming a ref
// that does not exist.
func TestPinStrandedFixupCommit_PinFailure(t *testing.T) {
	withFakeFixupLocalHead(t, "c0ffee0000000000000000000000000000000000", nil)
	withStrandedAncestry(t, func(context.Context, string, string, string) (bool, error) { return true, nil })
	withStrandedPin(t, func(context.Context, string, string, string) error {
		return errors.New("git update-ref: cannot lock ref")
	})
	var log strings.Builder
	if clause := pinStrandedFixupCommit(context.Background(), strandedCfg(), &log, "unused", "base"); clause != "" {
		t.Errorf("clause = %q, want empty on a failed pin", clause)
	}
	for _, w := range []string{`"event":"checkpoint_ref_pin_failed"`, "cannot lock ref"} {
		if !strings.Contains(log.String(), w) {
			t.Errorf("log missing %s:\n%s", w, log.String())
		}
	}
	if strings.Contains(log.String(), "fixup_stranded_commit_pinned") {
		t.Errorf("a failed pin logged success:\n%s", log.String())
	}
}

// TestCheckpointFixupRef: the ref is a -fixup- leaf sibling of the stage's
// checkpoint ref keyed by the SHA's 12-character prefix; a shorter SHA is used
// whole rather than panicking.
func TestCheckpointFixupRef(t *testing.T) {
	base := checkpointRef("r", "s")
	if got := checkpointFixupRef("r", "s", "0123456789abcdef0123"); got != base+"-fixup-0123456789ab" {
		t.Errorf("ref = %q", got)
	}
	if got := checkpointFixupRef("r", "s", "abc"); got != base+"-fixup-abc" {
		t.Errorf("short-sha ref = %q", got)
	}
}

func assertNotPinnedReason(t *testing.T, log, reason string) {
	t.Helper()
	if !strings.Contains(log, `"event":"fixup_stranded_commit_not_pinned"`) || !strings.Contains(log, `"reason":"`+reason+`"`) {
		t.Errorf("want fixup_stranded_commit_not_pinned reason %s:\n%s", reason, log)
	}
}

func withStrandedAncestry(t *testing.T, fn func(context.Context, string, string, string) (bool, error)) {
	t.Helper()
	orig := fixupIsAncestor
	fixupIsAncestor = fn
	t.Cleanup(func() { fixupIsAncestor = orig })
}

func withStrandedPin(t *testing.T, fn func(context.Context, string, string, string) error) {
	t.Helper()
	orig := fixupPinRef
	fixupPinRef = fn
	t.Cleanup(func() { fixupPinRef = orig })
}

// withStrandedBaseTip seams the fix-up base-tip read (approval condition C3),
// so a run()-level test fixes the base without depending on the process cwd
// being a git checkout.
func withStrandedBaseTip(t *testing.T, sha string) {
	t.Helper()
	orig := fixupBaseTipHead
	fixupBaseTipHead = func(context.Context, string) (string, error) { return sha, nil }
	t.Cleanup(func() { fixupBaseTipHead = orig })
}

const (
	strandedBase = "ba5e000000000000000000000000000000000000"
	strandedHead = "5ccdf30ee8d97d41c34129b133d83f77672d0c91"
)

// runStrandedFixup drives run() through a fix-up whose CommitAndPush fails
// with pushErr after the local HEAD advanced (or not, per head), recording
// every pin. It returns the failure report and the pins.
func runStrandedFixup(t *testing.T, pushErr error, head string) (*upload.ShipPullRequestArgs, []string, string) {
	t.Helper()
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = fixupLandingPrompt()
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{err: pushErr}, &fakePROpener{})
	withStrandedBaseTip(t, strandedBase)
	withFakeFixupLocalHead(t, head, nil)
	withStrandedAncestry(t, func(_ context.Context, _, anc, desc string) (bool, error) {
		return anc == strandedBase && desc == strandedHead, nil
	})
	var pins []string
	withStrandedPin(t, func(_ context.Context, _, ref, sha string) error {
		pins = append(pins, ref+"="+sha)
		return nil
	})
	var stderr strings.Builder
	if got := runFixupLanding(t, &stderr); got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if fu.gotPRArgs == nil || fu.gotPRArgs.Outcome != "failed" || fu.gotPRArgs.Category != "B" {
		t.Fatalf("report = %+v, want {failed, B}\n%s", fu.gotPRArgs, stderr.String())
	}
	return fu.gotPRArgs, pins, stderr.String()
}

// strandedClauseFor is the clause pinStrandedFixupCommit renders for head.
func strandedClauseFor(head string) string {
	ref := checkpointFixupRef(strandedRunID, strandedStageID, head)
	return fmt.Sprintf("; fix-up commit %s never reached the PR branch; its work is pinned at %s (inspect with `git show %s`, recover with `git cherry-pick %s`)", head, ref, head, head)
}

// TestRun_Fixup_CommitFailure_PinsStrandedCommit is the #4337 incident shape at
// the run() seam: a fix-up's CommitAndPush made its local commit and then the
// pushed-tree re-verify failed category B. The report's reason ENDS with the
// clause and the pin was called with the -fixup- ref. MECHANISM: the head seam
// reports an advanced SHA by construction, and only the main.go call site
// `strandedClause = pinStrandedFixupCommit(ctx, cfg, logSink, repoDir, fixupBaseTipSHA)`
// turns it into a clause — replacing it with `strandedClause = ""` drops the
// clause AND the pin call.
func TestRun_Fixup_CommitFailure_PinsStrandedCommit(t *testing.T) {
	report, pins, log := runStrandedFixup(t, fmt.Errorf("verify tree: %w", gitops.ErrPushedTreeNotVerified), strandedHead)
	clause := strandedClauseFor(strandedHead)
	if !strings.HasSuffix(report.Reason, clause) {
		t.Errorf("failure reason does not END with the pin clause:\n got %q\nwant suffix %q", report.Reason, clause)
	}
	wantPin := checkpointFixupRef(strandedRunID, strandedStageID, strandedHead) + "=" + strandedHead
	if len(pins) != 1 || pins[0] != wantPin {
		t.Errorf("pins = %v, want [%s]", pins, wantPin)
	}
	if !strings.Contains(log, `"event":"fixup_stranded_commit_pinned"`) {
		t.Errorf("missing fixup_stranded_commit_pinned:\n%s", log)
	}
}

// TestRun_Fixup_CommitFailure_NoAdvance_ReasonUnchanged: the head seam equals
// the base, so the pass made no commit — no pin, and the reason is
// byte-identical to the pre-#4337 form.
func TestRun_Fixup_CommitFailure_NoAdvance_ReasonUnchanged(t *testing.T) {
	pushErr := fmt.Errorf("verify tree: %w", gitops.ErrPushedTreeNotVerified)
	report, pins, log := runStrandedFixup(t, pushErr, strandedBase)
	if want := "commit+push: " + pushErr.Error(); report.Reason != want {
		t.Errorf("reason = %q, want byte-identical %q", report.Reason, want)
	}
	if len(pins) != 0 {
		t.Errorf("pins = %v, want none", pins)
	}
	if strings.Contains(log, "fixup_stranded_commit") {
		t.Errorf("the no-commit arm must be silent:\n%s", log)
	}
}

// TestRun_Fixup_CommitFailure_LongReasonKeepsClause: a pusher error far over
// upload.MaxFailureReportReasonBytes still ends with the clause after the wire
// shrink (upload.TruncateReason keeps head+tail). MECHANISM: the clause is the
// LAST thing appended; moving it before checkpointFailureSuffix's insertion
// point or prepending it puts it in the elided middle.
func TestRun_Fixup_CommitFailure_LongReasonKeepsClause(t *testing.T) {
	long := "verify tree: " + strings.Repeat("go test output line\n", 4*upload.MaxFailureReportReasonBytes/20)
	report, _, _ := runStrandedFixup(t, fmt.Errorf("%s: %w", long, gitops.ErrPushedTreeNotVerified), strandedHead)
	if len(report.Reason) <= upload.MaxFailureReportReasonBytes {
		t.Fatalf("fixture reason is %d bytes, want over the %d cap", len(report.Reason), upload.MaxFailureReportReasonBytes)
	}
	wire := upload.TruncateReason(report.Reason, upload.MaxFailureReportReasonBytes)
	if !strings.Contains(wire, "truncated") {
		t.Fatalf("the wire reason was not shrunk:\n%.200q", wire)
	}
	if clause := strandedClauseFor(strandedHead); !strings.HasSuffix(wire, clause) {
		t.Errorf("the shrunk wire reason lost the trailing pin clause:\n%q", wire[len(wire)-300:])
	}
}

// TestRun_NonFixup_CommitFailure_NoStrandedPin: the pin is fix-up-only. A
// standalone implement push failure, even with an advanced HEAD seam, never
// reaches pinStrandedFixupCommit. MECHANISM: off the fix-up path the base tip
// is empty, so dropping the `isFixup &&` guard surfaces as a
// fixup_stranded_commit_not_pinned {base_unresolved} line that is otherwise
// absent.
func TestRun_NonFixup_CommitFailure_NoStrandedPin(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{StageID: strandedStageID, StageType: "implement", Prompt: "implement", PromptHash: "h"}
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{err: fmt.Errorf("verify tree: %w", gitops.ErrPushedTreeNotVerified)}, &fakePROpener{})
	withFakeFixupLocalHead(t, strandedHead, nil)
	withStrandedPin(t, func(context.Context, string, string, string) error {
		t.Error("a non-fix-up failure pinned a stranded commit")
		return nil
	})
	var stderr strings.Builder
	if got := runFixupLanding(t, &stderr); got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if strings.Contains(stderr.String(), "fixup_stranded_commit") {
		t.Errorf("a non-fix-up failure reached the stranded-commit pin:\n%s", stderr.String())
	}
	if fu.gotPRArgs == nil || strings.Contains(fu.gotPRArgs.Reason, "never reached the PR branch") {
		t.Errorf("report = %+v, want a failure report without the pin clause", fu.gotPRArgs)
	}
}
