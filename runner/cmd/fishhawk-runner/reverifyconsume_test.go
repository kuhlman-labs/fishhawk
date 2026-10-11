package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// #4190 run()-level reverify consume: a served `reverify` held commit is
// prechecked, gated, re-verified ONCE in full form and published as a
// DCO-signed commit of the same tree on the same parent — with no agent pass —
// or falls back to the ordinary agent pass in the same dispatch.

const reverifyHeldEdit = "package a\n\n// the held agent edit\n"

// reverifyRunFixture is a work repo whose bare origin carries main at base,
// and a held WIP commit on base that is reachable ONLY through the stage's
// -verify checkpoint ref (no branch, no other ref) — the state a category-C
// committed-gate failure leaves behind.
type reverifyRunFixture struct {
	repo, base, held, heldTree string
}

func newReverifyRunFixture(t *testing.T, heldFiles map[string]string) reverifyRunFixture {
	t.Helper()
	repo, runGit := compileGateRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, "pkg", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, "pkg", "a", "a.go"), "package a\n")
	runGit("add", "-A")
	runGit("commit", "-m", "base", "--no-verify")
	bare := filepath.Join(t.TempDir(), "origin.git")
	runGit("init", "--bare", bare)
	runGit("remote", "add", "origin", bare)
	runGit("push", "origin", "main")
	base := cprGit(t, repo, "rev-parse", "main")
	if heldFiles == nil {
		heldFiles = map[string]string{"pkg/a/a.go": reverifyHeldEdit}
	}
	held, heldTree := commitOnParent(t, repo, base, heldFiles)
	cprGit(t, repo, "update-ref", checkpointVerifyRef(verifyFixRunID, verifyFixStageID), held)
	return reverifyRunFixture{repo: repo, base: base, held: held, heldTree: heldTree}
}

// assertOnlyVerifyRefReaches pins approval condition 3: the held commit is on
// no branch and the -verify ref is the ONLY ref that reaches it, so a passing
// resume proves the gate clone (gateiso's local-transport `git clone
// --no-hardlinks`) carries an object reachable from refs/fishhawk/* alone.
func assertOnlyVerifyRefReaches(t *testing.T, repo, held string) {
	t.Helper()
	if got := cprGit(t, repo, "branch", "--all", "--contains", held); got != "" {
		t.Fatalf("fixture: held commit is on a branch: %q", got)
	}
	got := cprGit(t, repo, "for-each-ref", "--contains", held, "--format=%(refname)")
	if got != checkpointVerifyRef(verifyFixRunID, verifyFixStageID) {
		t.Fatalf("fixture: refs reaching the held commit = %q, want only the -verify ref", got)
	}
}

// reverifyVerifyCmd writes a verify script that prints out and exits rc on
// every call, appending `pkgs_set=<set|>` per call — `set` only when
// FISHHAWK_VERIFY_PACKAGES is PRESENT in the environment (even empty), so the
// full form's ABSENT variable is distinguishable from an empty one.
func reverifyVerifyCmd(t *testing.T, out string, rc int) (cmd, callsLog string) {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "out"), out)
	callsLog = filepath.Join(dir, "calls.log")
	script := filepath.Join(dir, "verify.sh")
	mustWrite(t, script, fmt.Sprintf(`#!/bin/sh
d=%q
printf 'pkgs_set=%%s\n' "${%s+set}" >> "$d/calls.log"
cat "$d/out"
exit %d
`, dir, verifyPackagesEnvVar, rc))
	return "sh " + script, callsLog
}

func verifyCalls(t *testing.T, callsLog string) []string {
	t.Helper()
	b, err := os.ReadFile(callsLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// reverifyPromptFor serves the fixture's held commit as a reverify checkpoint.
func reverifyPromptFor(f reverifyRunFixture, verifyCmd string) *upload.FetchedPrompt {
	return &upload.FetchedPrompt{
		StageID:              verifyFixStageID,
		StageType:            "implement",
		Prompt:               "implement",
		PromptHash:           "h",
		VerifyCommand:        verifyCmd,
		VerifyMaxIterations:  2,
		SupportsPushResume:   true,
		ScopeFiles:           []upload.ScopeFile{{Path: "pkg/a/a.go", Operation: "modify"}},
		OpenPRFromHeldCommit: true,
		HeldCommitSHA:        f.held,
		HeldCommitBranch:     reverifyRunBranch(),
		HeldCommitBaseSHA:    f.base,
		HeldCommitResumeKind: resumeKindReverify,
		HeldCommitPRTitle:    "fix(runner): the held change",
		HeldCommitPRBody:     "## Summary\n- the agent's own PR text\n",
	}
}

func reverifyRunBranch() string {
	return fmt.Sprintf("fishhawk/run-%s/stage-%s", shortID(verifyFixRunID), shortID(verifyFixStageID))
}

// reverifyRun drives run() over the fixture with fakes on every forge seam.
type reverifyRun struct {
	code    int
	log     string
	invoker *fakeInvoker
	fu      *fakeUploader
	fp      *fakePusher
	fpr     *fakePROpener
}

func runReverify(t *testing.T, f reverifyRunFixture, prompt *upload.FetchedPrompt, fp *fakePusher) reverifyRun {
	t.Helper()
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	inv := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, inv)
	fu := newFakeUploader(t)
	fu.promptResp = prompt
	withFakeUploader(t, fu)
	if fp == nil {
		fp = &fakePusher{}
	}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)
	var stderr strings.Builder
	code := run(verifyFixRunArgs(f.repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")), &stderr)
	return reverifyRun{code: code, log: stderr.String(), invoker: inv, fu: fu, fp: fp, fpr: fpr}
}

// TestRun_ReverifyResume_SkipsAgentAndPublishes is the zero-agent promise: the
// held commit passes the precheck, the pre-push gates and ONE full-form
// verify, and is published as a DCO-signed commit with the SAME tree and parent
// (same patch-id) through the push-resume tail, which opens the PR and
// releases every checkpoint ref.
func TestRun_ReverifyResume_SkipsAgentAndPublishes(t *testing.T) {
	f := newReverifyRunFixture(t, nil)
	assertOnlyVerifyRefReaches(t, f.repo, f.held)
	cmd, calls := reverifyVerifyCmd(t, noopPassOut, 0)

	r := runReverify(t, f, reverifyPromptFor(f, cmd), nil)
	if r.code != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", r.code, r.log)
	}
	if r.invoker.callIdx != 0 {
		t.Errorf("agent invocations = %d, want 0 (the reverify resume runs no agent)", r.invoker.callIdx)
	}
	if got := verifyCalls(t, calls); len(got) != 1 || got[0] != "pkgs_set=" {
		t.Errorf("verify calls = %q, want exactly one FULL-form call (FISHHAWK_VERIFY_PACKAGES absent)", got)
	}
	if r.fp.pushCommittedArgs == nil {
		t.Fatalf("nothing was published:\n%s", r.log)
	}
	synth := r.fp.pushCommittedArgs.HeadSHA
	if synth == f.held {
		t.Errorf("published the bot-identity WIP commit %s, want a synthesized DCO-signed commit", synth)
	}
	if r.fp.pushCommittedArgs.Branch != reverifyRunBranch() {
		t.Errorf("published branch = %q, want %q", r.fp.pushCommittedArgs.Branch, reverifyRunBranch())
	}
	if tree := cprGit(t, f.repo, "rev-parse", synth+"^{tree}"); tree != f.heldTree {
		t.Errorf("published tree = %s, want the held tree %s", tree, f.heldTree)
	}
	if parent := cprGit(t, f.repo, "rev-parse", synth+"^"); parent != f.base {
		t.Errorf("published parent = %s, want the served base %s", parent, f.base)
	}
	msg := cprGit(t, f.repo, "log", "-1", "--format=%B", synth)
	if !strings.Contains(msg, "Signed-off-by: ") || !strings.HasPrefix(msg, "fix(runner): the held change") {
		t.Errorf("published message lacks the served title or a Signed-off-by trailer:\n%s", msg)
	}
	ctx := context.Background()
	if got, want := gitPatchIDForCommit(ctx, f.repo, synth), gitPatchIDForCommit(ctx, f.repo, f.held); got == "" || got != want {
		t.Errorf("patch-id of published %q != held %q", got, want)
	}
	if r.fpr.gotArgs == nil || r.fpr.gotArgs.Head != reverifyRunBranch() {
		t.Fatalf("PR not opened from %s: %+v", reverifyRunBranch(), r.fpr.gotArgs)
	}
	var success, failed int
	for _, a := range r.fu.gotPRCalls {
		switch a.Outcome {
		case "":
			success++
		case "failed":
			failed++
		}
	}
	if success != 1 || failed != 0 {
		t.Errorf("ships = %d success, %d failed, want 1 and 0:\n%+v", success, failed, r.fu.gotPRCalls)
	}
	for _, ref := range []string{checkpointVerifyRef(verifyFixRunID, verifyFixStageID), checkpointRef(verifyFixRunID, verifyFixStageID)} {
		if _, ok := refTarget(f.repo, ref); ok {
			t.Errorf("checkpoint ref %s survived a successful reverify resume", ref)
		}
	}
	if !strings.Contains(r.log, `"event":"held_commit_resume_path","run_id":"`+verifyFixRunID+`","stage_id":"`+verifyFixStageID+`","path":"reverify"`) {
		t.Errorf("missing held_commit_resume_path path=reverify:\n%s", r.log)
	}
}

// TestRun_ReverifyResume_FallsBackToAgent: a precheck miss runs the ORDINARY
// agent pass in the same dispatch and logs the fallback with its token. Nothing
// is published from the held commit.
func TestRun_ReverifyResume_FallsBackToAgent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
		shape func(t *testing.T, f *reverifyRunFixture, p *upload.FetchedPrompt)
	}{
		{"HeldAbsent", reverifyFallbackHeldCommitAbsent, func(_ *testing.T, _ *reverifyRunFixture, p *upload.FetchedPrompt) {
			// Never written: absent BY CONSTRUCTION.
			p.HeldCommitSHA = strings.Repeat("4190", 10)
		}},
		{"BaseMismatch", reverifyFallbackBaseMismatch, func(t *testing.T, f *reverifyRunFixture, p *upload.FetchedPrompt) {
			other, _ := commitOnParent(t, f.repo, f.base, map[string]string{"pkg/a/other.go": "package a\n"})
			p.HeldCommitBaseSHA = other
		}},
		{"BaseNotOnBase", reverifyFallbackBaseNotOnBase, func(t *testing.T, f *reverifyRunFixture, p *upload.FetchedPrompt) {
			// A LOCAL-ONLY parent (never pushed), with a stale tracking ref
			// planted to claim it IS origin/main: only a FRESH fetch of the
			// declared base shows the held commit is not on it (ADR-035).
			foreign, _ := commitOnParent(t, f.repo, f.base, map[string]string{"FOREIGN": "x\n"})
			held, _ := commitOnParent(t, f.repo, foreign, map[string]string{"pkg/a/a.go": reverifyHeldEdit})
			cprGit(t, f.repo, "update-ref", "refs/remotes/origin/main", foreign)
			cprGit(t, f.repo, "update-ref", checkpointVerifyRef(verifyFixRunID, verifyFixStageID), held)
			p.HeldCommitSHA, p.HeldCommitBaseSHA = held, foreign
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReverifyRunFixture(t, nil)
			cmd, _ := reverifyVerifyCmd(t, noopPassOut, 0)
			p := reverifyPromptFor(f, cmd)
			tc.shape(t, &f, p)

			r := runReverify(t, f, p, nil)
			if r.invoker.callIdx != 1 {
				t.Errorf("agent invocations = %d, want 1 (the fallback runs the ordinary agent pass)", r.invoker.callIdx)
			}
			want := `"path":"fallback_agent_rerun","reason":"` + tc.token + `"`
			if !strings.Contains(r.log, want) {
				t.Errorf("missing %s:\n%s", want, r.log)
			}
			if strings.Contains(r.log, `"path":"reverify"`) {
				t.Errorf("a precheck miss must not take the reverify path:\n%s", r.log)
			}
			if r.fp.pushCommittedArgs != nil {
				t.Errorf("the held commit was published on a fallback: %+v", r.fp.pushCommittedArgs)
			}
		})
	}
}

// TestRun_ReverifyResume_PrePushGateRefusals (approval condition 1): a held tree
// an ordinary push would have refused — out-of-scope path (#980), missing
// declared scope file (#1151), unsatisfied binding assertion (#1171) — is a
// PERMANENT category-B outcome: nothing is verified, nothing is published, no
// agent runs and no checkpoint is re-armed. Each row's fixture violates ONLY
// its own gate and passes verify, so dropping the gate publishes it.
func TestRun_ReverifyResume_PrePushGateRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
		files map[string]string
		shape func(p *upload.FetchedPrompt)
	}{
		{"OutOfScope", reverifyGateOutOfScope,
			map[string]string{"pkg/a/a.go": reverifyHeldEdit, "pkg/z/stray.go": "package z\n"}, nil},
		{"ScopeFilesMissing", reverifyGateScopeFilesMissing,
			map[string]string{"pkg/a/a.go": reverifyHeldEdit}, func(p *upload.FetchedPrompt) {
				p.ScopeFiles = append(p.ScopeFiles, upload.ScopeFile{Path: "pkg/a/b.go", Operation: "create"})
			}},
		{"BindingAssertionUnsatisfied", reverifyGateBindingAssertionUnsatisfied,
			map[string]string{"pkg/a/a.go": reverifyHeldEdit}, func(p *upload.FetchedPrompt) {
				p.BindingAssertions = []upload.BindingAssertion{{Type: "file_contains", Path: "pkg/a/a.go", Literal: "MUST-SHIP-4190"}}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReverifyRunFixture(t, tc.files)
			cmd, calls := reverifyVerifyCmd(t, noopPassOut, 0)
			p := reverifyPromptFor(f, cmd)
			if tc.shape != nil {
				tc.shape(p)
			}

			r := runReverify(t, f, p, nil)
			if r.code != exitFailure {
				t.Fatalf("run = %d, want exitFailure:\n%s", r.code, r.log)
			}
			if r.fp.pushCommittedArgs != nil || r.fpr.gotArgs != nil {
				t.Fatalf("a held tree failing %s was published (push %+v, PR %+v)", tc.token, r.fp.pushCommittedArgs, r.fpr.gotArgs)
			}
			if r.invoker.callIdx != 0 {
				t.Errorf("agent invocations = %d, want 0 (a gate refusal never falls back)", r.invoker.callIdx)
			}
			if got := verifyCalls(t, calls); len(got) != 0 {
				t.Errorf("verify ran %d time(s), want 0 (the gates run first)", len(got))
			}
			reports := failedReports(r.fu)
			if len(reports) != 1 {
				t.Fatalf("failed reports = %d, want 1:\n%s", len(reports), r.log)
			}
			got := reports[0]
			if got.Category != "B" || !strings.HasPrefix(got.Reason, tc.token+":") {
				t.Errorf("report = category %q reason %.200q, want B with the %s token", got.Category, got.Reason, tc.token)
			}
			if got.ResumeKind != "" || got.HeadSHA != "" {
				t.Errorf("a permanent refusal re-armed a checkpoint: kind %q head %q", got.ResumeKind, got.HeadSHA)
			}
		})
	}
}

// TestRun_ReverifyResume_VerifyFailsOutsideChange_RearmsReverify: the re-verify
// fails only in a package outside the change — transient, so the report
// re-arms the SAME reverify checkpoint. Nothing is pushed, no agent runs.
func TestRun_ReverifyResume_VerifyFailsOutsideChange_RearmsReverify(t *testing.T) {
	f := newReverifyRunFixture(t, nil)
	cmd, calls := reverifyVerifyCmd(t, noopOutsideOut, 1)
	r := runReverify(t, f, reverifyPromptFor(f, cmd), nil)
	if r.code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", r.code, r.log)
	}
	if r.fp.pushCommittedArgs != nil || r.invoker.callIdx != 0 {
		t.Fatalf("pushed %+v / invoked %d times, want neither", r.fp.pushCommittedArgs, r.invoker.callIdx)
	}
	if got := verifyCalls(t, calls); len(got) != 1 {
		t.Errorf("verify calls = %d, want 1", len(got))
	}
	got := reportedFailureBody(t, r.fu)
	if got.Outcome != "failed" || got.Category != "C" || !strings.HasPrefix(got.Reason, reverifyFailedOutsideChange+":") {
		t.Errorf("report = %s/%s %.200q, want failed/C with the %s token", got.Outcome, got.Category, got.Reason, reverifyFailedOutsideChange)
	}
	if got.ResumeKind != resumeKindReverify || got.HeadSHA != f.held || got.BaseSHA != f.base || got.Branch != reverifyRunBranch() {
		t.Errorf("re-armed checkpoint = %q %s %s %s, want reverify %s %s %s",
			got.ResumeKind, got.HeadSHA, got.BaseSHA, got.Branch, f.held, f.base, reverifyRunBranch())
	}
	if got.PRTitle == "" {
		t.Error("the re-armed checkpoint dropped the served PR title")
	}
}

// TestRun_ReverifyResume_VerifyFailsInChange_NoCheckpoint: a failure inside
// the change is permanent — reported C with NO checkpoint, so the next retry is
// an ordinary agent pass.
func TestRun_ReverifyResume_VerifyFailsInChange_NoCheckpoint(t *testing.T) {
	f := newReverifyRunFixture(t, nil)
	cmd, _ := reverifyVerifyCmd(t, noopInsideOut, 1)
	r := runReverify(t, f, reverifyPromptFor(f, cmd), nil)
	if r.code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", r.code, r.log)
	}
	got := reportedFailureBody(t, r.fu)
	if got.Category != "C" || !strings.HasPrefix(got.Reason, reverifyFailedInChange+":") {
		t.Errorf("report = %s %.200q, want C with the %s token", got.Category, got.Reason, reverifyFailedInChange)
	}
	if got.ResumeKind != "" || got.HeadSHA != "" || got.BaseSHA != "" || got.Branch != "" {
		t.Errorf("a permanent in-change failure re-armed a checkpoint: %+v", got)
	}
	if r.fp.pushCommittedArgs != nil {
		t.Errorf("pushed %+v on a failed re-verify", r.fp.pushCommittedArgs)
	}
}

// TestRun_ReverifyResume_PushFailure_RearmsPushKind: the held commit passes
// and is synthesized, then the publish fails. The push-resume tail re-arms the
// CHEAPER push kind at the synthesized commit with the held tree as the
// verified tree, and checkpointRef pins the synthesized commit.
func TestRun_ReverifyResume_PushFailure_RearmsPushKind(t *testing.T) {
	f := newReverifyRunFixture(t, nil)
	cmd, _ := reverifyVerifyCmd(t, noopPassOut, 0)
	r := runReverify(t, f, reverifyPromptFor(f, cmd), &fakePusher{pushCommittedErr: errors.New("push transport down")})
	if r.code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", r.code, r.log)
	}
	if r.fp.pushCommittedArgs == nil {
		t.Fatalf("the publish was never attempted:\n%s", r.log)
	}
	synth := r.fp.pushCommittedArgs.HeadSHA
	got := reportedFailureBody(t, r.fu)
	if got.ResumeKind != resumeKindPush || got.HeadSHA != synth || got.TreeSHA != f.heldTree || got.BaseSHA != f.base {
		t.Errorf("re-armed = kind %q head %q tree %q base %q, want push %s %s %s", got.ResumeKind, got.HeadSHA, got.TreeSHA, got.BaseSHA, synth, f.heldTree, f.base)
	}
	if pinned, ok := refTarget(f.repo, checkpointRef(verifyFixRunID, verifyFixStageID)); !ok || pinned != synth {
		t.Errorf("checkpointRef = %q (present %t), want the synthesized commit %s", pinned, ok, synth)
	}
}

// TestReverifyFailureReason: the token leads, the output tail is redacted and
// bounded, and an empty output adds nothing.
func TestReverifyFailureReason(t *testing.T) {
	v := reverifyVerdict{token: reverifyFailedInChange, category: "C", detail: "d"}
	if got := reverifyFailureReason(v, ""); got != reverifyFailedInChange+": d" {
		t.Errorf("no-output reason = %q", got)
	}
	long := strings.Repeat("x", 3*gateEvidenceTailBytes) + "\nghp_" + strings.Repeat("a", 36) + "\nLAST LINE"
	got := reverifyFailureReason(v, long)
	if !strings.HasPrefix(got, reverifyFailedInChange+": d\n") || !strings.HasSuffix(got, "LAST LINE") {
		t.Errorf("reason does not lead with the token and end with the output tail: %.120q…", got)
	}
	if strings.Contains(got, "ghp_"+strings.Repeat("a", 36)) {
		t.Error("the output tail was not redacted")
	}
	if len(got) > len(reverifyFailedInChange)+4+gateEvidenceTailBytes {
		t.Errorf("reason length %d exceeds the bound", len(got))
	}
}

// TestRun_ReverifyResume_ChildServedReverifyIsRefused: a decomposition child is
// never intercepted. A (backend-anomalous) reverify served to a child reaches
// openHeldCommitPR, whose unknown-kind refusal (permanent, before any forge
// touch) rejects it — it never re-verifies, publishes, or reports pushed.
func TestRun_ReverifyResume_ChildServedReverifyIsRefused(t *testing.T) {
	f := newReverifyRunFixture(t, nil)
	cmd, calls := reverifyVerifyCmd(t, noopPassOut, 0)
	p := reverifyPromptFor(f, cmd)
	p.DecomposedFromRunID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	r := runReverify(t, f, p, nil)
	if r.code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", r.code, r.log)
	}
	if r.fp.pushCommittedArgs != nil || r.invoker.callIdx != 0 {
		t.Fatalf("a child served reverify was published (%+v) or ran the agent (%d)", r.fp.pushCommittedArgs, r.invoker.callIdx)
	}
	if got := verifyCalls(t, calls); len(got) != 0 {
		t.Errorf("verify ran %d time(s) for a child, want 0", len(got))
	}
	got := reportedFailureBody(t, r.fu)
	if got.Outcome != "failed" || !strings.HasPrefix(got.Reason, refusePushUnknownResumeKind+":") || got.ResumeKind != resumeKindPushDiscarded {
		t.Errorf("report = %s %.200q kind %q, want a permanent %s refusal", got.Outcome, got.Reason, got.ResumeKind, refusePushUnknownResumeKind)
	}
	if strings.Contains(r.log, `"path":"reverify"`) {
		t.Errorf("a child must not take the reverify path:\n%s", r.log)
	}
}
