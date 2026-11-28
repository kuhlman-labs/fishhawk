package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// #4190 run()-level wiring of the verify-head pin and the committed-gate
// failure report: a committed-tree verify gate that turns a successful agent
// pass into a failure pins the last verify_run head, appends the clause naming
// it to the reason, and — for category B/C on a push-gated stage — sends the
// /pull-request failure report the stage's forward gate waits on.

// gateFailRepo is a real repo with one committed Go file (pkg/a/a.go) and the
// agent's in-scope edit to it left in the working tree. The fake invoker
// mirrors the repo's working tree into the lineage worktree, so the edit is
// what the committed-tree gate commits and judges.
func gateFailRepo(t *testing.T) string {
	t.Helper()
	repo, runGit := compileGateRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, "pkg", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, "pkg", "a", "a.go"), "package a\n")
	runGit("add", "-A")
	runGit("commit", "-m", "base", "--no-verify")
	mustWrite(t, filepath.Join(repo, "pkg", "a", "a.go"), "package a\n\n// agent change\n")
	return repo
}

// formVerifyCmd writes a verify script that answers by FORM: the scoped form
// (FISHHAWK_VERIFY_PACKAGES set) prints scopedOut and exits scopedRC, the full
// form prints fullOut and exits fullRC. Every call is appended to calls.log as
// `pkgs=[<set>]`, so a test can count calls and read each call's form.
func formVerifyCmd(t *testing.T, scopedOut string, scopedRC int, fullOut string, fullRC int) (cmd, callsLog string) {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "scoped.out"), scopedOut)
	mustWrite(t, filepath.Join(dir, "full.out"), fullOut)
	callsLog = filepath.Join(dir, "calls.log")
	script := filepath.Join(dir, "verify.sh")
	mustWrite(t, script, fmt.Sprintf(`#!/bin/sh
d=%q
printf 'pkgs=[%%s]\n' "$%s" >> "$d/calls.log"
if [ -n "$%s" ]; then cat "$d/scoped.out"; exit %d; fi
cat "$d/full.out"
exit %d
`, dir, verifyPackagesEnvVar, verifyPackagesEnvVar, scopedRC, fullRC))
	return "sh " + script, callsLog
}

// gateFailPrompt is the implement prompt every row here serves: one in-scope
// file, the given verify command and fix-loop budget, and a backend that
// advertised supports_push_resume (so a reverify checkpoint MAY be armed —
// each row then shows whether it is).
func gateFailPrompt(verifyCmd string, maxIter int) *upload.FetchedPrompt {
	return &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       verifyCmd,
		VerifyMaxIterations: maxIter,
		SupportsPushResume:  true,
		ScopeFiles:          []upload.ScopeFile{{Path: "pkg/a/a.go", Operation: "modify"}},
	}
}

// lastBundleVerifyRunHead is the test-side reading of the last verify_run head
// in the shipped bundle, independent of lastVerifyRunHead.
func lastBundleVerifyRunHead(t *testing.T, events []bundle.Line) string {
	t.Helper()
	head := ""
	for _, ev := range events {
		if ev.Kind != "verify_run" {
			continue
		}
		var p struct {
			HeadSHA string `json:"head_sha"`
		}
		if err := json.Unmarshal(ev.Data, &p); err == nil && p.HeadSHA != "" {
			head = p.HeadSHA
		}
	}
	return head
}

// failedReports returns every {outcome:"failed"} /pull-request report shipped.
func failedReports(fu *fakeUploader) []upload.ShipPullRequestArgs {
	var out []upload.ShipPullRequestArgs
	for _, a := range fu.gotPRCalls {
		if a.Outcome == "failed" {
			out = append(out, a)
		}
	}
	return out
}

// runGateFail drives run() over gateFailRepo with the given prompt and returns
// the exit code, the log, the bundle path and the repo.
func runGateFail(t *testing.T, repo string, fu *fakeUploader, args []string) (int, string) {
	t.Helper()
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
	var stderr strings.Builder
	code := run(args, &stderr)
	return code, stderr.String()
}

// TestRun_VerifyFailureC_PinsHeadAndReportsReverifyCheckpoint: the agent edits
// an in-scope file, the scoped verify passes and the full verify fails ONLY in a
// package outside the change, twice (the fix agent changes nothing), so the fix
// loop ends category C outside-change. The last verify_run head is pinned at
// the -verify ref, the reason ends with the clause naming it, and exactly one
// failure report ships: C, resume_kind reverify, head = the pin, base = its
// parent, the stage's run branch, NO verified tree — the shape of
// testdata/wire/reverify_failure_report.json.
func TestRun_VerifyFailureC_PinsHeadAndReportsReverifyCheckpoint(t *testing.T) {
	repo := gateFailRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true}})
	cmd, _ := formVerifyCmd(t, noopPassOut, 0, noopOutsideOut, 1)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 2)
	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")

	code, log := runGateFail(t, repo, fu, verifyFixRunArgs(repo, bundlePath))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}

	ref := checkpointVerifyRef(verifyFixRunID, verifyFixStageID)
	pinned, ok := refTarget(repo, ref)
	if !ok {
		t.Fatalf("verify-head ref %s absent after a category-C gate failure:\n%s", ref, log)
	}
	if want := lastBundleVerifyRunHead(t, readBundleEvents(t, bundlePath)); pinned != want || want == "" {
		t.Fatalf("pinned %s = %s, want the last verify_run head %q", ref, pinned, want)
	}
	base := cprGit(t, repo, "rev-parse", pinned+"^")

	reports := failedReports(fu)
	if len(reports) != 1 {
		t.Fatalf("failed /pull-request reports = %d, want exactly 1 (the committed-gate report):\n%+v\n%s", len(reports), reports, log)
	}
	got := reports[0]
	if got.Category != "C" {
		t.Errorf("category = %q, want C", got.Category)
	}
	suffix := verifyHeadFailureSuffix(pinned, ref)
	if !strings.HasSuffix(got.Reason, suffix) {
		t.Errorf("reason does not end with %q:\n%.600s", suffix, got.Reason)
	}
	if !strings.HasPrefix(got.Reason, verifyFailureOutsideChangeLead) {
		t.Errorf("reason = %.200q, want the %s lead (the suffix is appended, never a lead)", got.Reason, verifyFailureOutsideChangeLead)
	}
	wantBranch := fmt.Sprintf("fishhawk/run-%s/stage-%s", shortID(verifyFixRunID), shortID(verifyFixStageID))
	if got.ResumeKind != resumeKindReverify || got.HeadSHA != pinned || got.BaseSHA != base || got.Branch != wantBranch {
		t.Errorf("checkpoint = kind %q head %q base %q branch %q, want %q %s %s %s",
			got.ResumeKind, got.HeadSHA, got.BaseSHA, got.Branch, resumeKindReverify, pinned, base, wantBranch)
	}
	if got.TreeSHA != "" {
		t.Errorf("a reverify checkpoint must carry no verified tree, got %q", got.TreeSHA)
	}

	// Shape parity with the shared golden: every key it carries is set on the
	// report, and nothing beyond them (no tree, no PR text — the agent wrote
	// no PR handoff here).
	golden, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "reverify_failure_report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(golden, &keys); err != nil {
		t.Fatal(err)
	}
	sent := map[string]string{
		"outcome": got.Outcome, "category": got.Category, "reason": got.Reason, "branch": got.Branch,
		"head_sha": got.HeadSHA, "base_sha": got.BaseSHA, "resume_kind": got.ResumeKind,
		"verified_tree_sha": got.TreeSHA, "pr_title": got.PRTitle, "pr_body": got.PRBody,
	}
	for k, v := range sent {
		if _, inGolden := keys[k]; inGolden != (v != "") {
			t.Errorf("field %s set=%t, golden carries it=%t", k, v != "", inGolden)
		}
	}
	if !strings.Contains(log, `"event":"verify_head_pinned"`) || !strings.Contains(log, `"event":"reverify_checkpoint_armed"`) {
		t.Errorf("missing verify_head_pinned / reverify_checkpoint_armed:\n%s", log)
	}
}

// TestRun_SingleShotGateFailureB_ReportsWithoutCheckpoint: max_iterations 0
// runs the single-shot committed gate; a red tree is category B. One failure
// report ships with category B and NO checkpoint coordinates.
func TestRun_SingleShotGateFailureB_ReportsWithoutCheckpoint(t *testing.T) {
	repo := gateFailRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true}})
	cmd, _ := formVerifyCmd(t, noopInsideOut, 1, noopInsideOut, 1)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 0)

	code, log := runGateFail(t, repo, fu, verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}
	reports := failedReports(fu)
	if len(reports) != 1 {
		t.Fatalf("failed /pull-request reports = %d, want exactly 1:\n%s", len(reports), log)
	}
	got := reports[0]
	if got.Category != "B" {
		t.Errorf("category = %q, want B", got.Category)
	}
	if got.ResumeKind != "" || got.HeadSHA != "" || got.BaseSHA != "" || got.Branch != "" || got.TreeSHA != "" {
		t.Errorf("a category-B report carries no checkpoint, got %+v", got)
	}
	if _, ok := refTarget(repo, checkpointVerifyRef(verifyFixRunID, verifyFixStageID)); !ok {
		t.Errorf("the verify head must still be pinned on a category-B gate failure:\n%s", log)
	}
}

// TestRun_VerifyFailureA_NoPullRequestFailureReport: an in-change failure that
// exhausts the fix loop is category A. The trace manifest fails the stage, so
// no /pull-request failure report is sent — but the manifest's
// agent_failure_reason carries the pin clause.
func TestRun_VerifyFailureA_NoPullRequestFailureReport(t *testing.T) {
	repo := gateFailRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true}})
	cmd, _ := formVerifyCmd(t, noopInsideOut, 1, noopInsideOut, 1)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 1)
	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")

	code, log := runGateFail(t, repo, fu, verifyFixRunArgs(repo, bundlePath))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}
	if n := len(failedReports(fu)); n != 0 {
		t.Fatalf("failed /pull-request reports = %d, want 0 for a category-A failure:\n%s", n, log)
	}
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, _, err := openBundleForTest(data)
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.AgentFailed {
		t.Fatalf("manifest agent_failed = false, want a category-A failure:\n%s", log)
	}
	ref := checkpointVerifyRef(verifyFixRunID, verifyFixStageID)
	pinned, ok := refTarget(repo, ref)
	if !ok {
		t.Fatalf("verify head not pinned on a category-A failure:\n%s", log)
	}
	if suffix := verifyHeadFailureSuffix(pinned, ref); !strings.HasSuffix(manifest.AgentFailureReason, suffix) {
		t.Errorf("manifest agent_failure_reason does not end with %q:\n%.400s", suffix, manifest.AgentFailureReason)
	}
}

// TestRun_ChildAndFixupVerifyFailureC_ReportWithoutReverifyKind: a
// decomposition child and a fix-up pass that fail the committed gate category C
// each send ONE failure report, with no resume_kind — neither is ever served a
// reverify checkpoint, so arming one would strand it.
func TestRun_ChildAndFixupVerifyFailureC_ReportWithoutReverifyKind(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape func(p *upload.FetchedPrompt)
	}{
		{"decomposition_child", func(p *upload.FetchedPrompt) {
			p.DecomposedFromRunID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		}},
		{"fixup", func(p *upload.FetchedPrompt) {
			p.Fixup = true
			p.FixupBranch = "fishhawk/run-11111111/stage-22222222"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := gateFailRepo(t)
			implementEnv(t, "kuhlman-labs/fishhawk", "main")
			withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true}})
			cmd, _ := formVerifyCmd(t, noopPassOut, 0, noopOutsideOut, 1)
			fu := newFakeUploader(t)
			fu.promptResp = gateFailPrompt(cmd, 2)
			tc.shape(fu.promptResp)

			code, log := runGateFail(t, repo, fu, verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")))
			if code != exitFailure {
				t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
			}
			reports := failedReports(fu)
			if len(reports) != 1 {
				t.Fatalf("failed /pull-request reports = %d, want exactly 1:\n%s", len(reports), log)
			}
			if got := reports[0]; got.Category != "C" || got.ResumeKind != "" || got.HeadSHA != "" {
				t.Errorf("report = category %q resume_kind %q head %q, want C with no checkpoint", got.Category, got.ResumeKind, got.HeadSHA)
			}
		})
	}
}

// TestRun_VerifyHeadPinFailure_FailOpen: a run id containing ".." makes git
// refuse the -verify refname BY CONSTRUCTION. The pin fails open: the category
// and the exit are unchanged, the failure report is still sent, and it carries
// no resume_kind (there is no pinned head to resume).
func TestRun_VerifyHeadPinFailure_FailOpen(t *testing.T) {
	repo := gateFailRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true}})
	cmd, _ := formVerifyCmd(t, noopPassOut, 0, noopOutsideOut, 1)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 2)
	args := verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz"))
	const badRunID = "11111111-2222..3333-4444-555555555555"
	for i := range args {
		if args[i] == verifyFixRunID {
			args[i] = badRunID
		}
	}

	code, log := runGateFail(t, repo, fu, args)
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}
	if !strings.Contains(log, `"event":"verify_head_pin_failed"`) {
		t.Fatalf("missing verify_head_pin_failed (the fixture must refuse the refname):\n%s", log)
	}
	reports := failedReports(fu)
	if len(reports) != 1 {
		t.Fatalf("failed /pull-request reports = %d, want exactly 1:\n%s", len(reports), log)
	}
	got := reports[0]
	if got.Category != "C" || got.ResumeKind != "" || got.HeadSHA != "" {
		t.Errorf("report = category %q resume_kind %q head %q, want C with no checkpoint", got.Category, got.ResumeKind, got.HeadSHA)
	}
	if strings.Contains(got.Reason, "pinned at") {
		t.Errorf("an unpinned failure's reason must not name a pin:\n%.400s", got.Reason)
	}
}

// TestRun_SuccessfulPushReleasesVerifyHeadRef: an ordinary push that succeeds
// releases a -verify pin an earlier failed attempt left behind.
func TestRun_SuccessfulPushReleasesVerifyHeadRef(t *testing.T) {
	repo := gateFailRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true}})
	cmd, _ := formVerifyCmd(t, noopPassOut, 0, noopPassOut, 0)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 2)
	ref := checkpointVerifyRef(verifyFixRunID, verifyFixStageID)
	cprGit(t, repo, "update-ref", ref, "HEAD")

	code, log := runGateFail(t, repo, fu, verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")))
	if code != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", code, log)
	}
	if _, ok := refTarget(repo, ref); ok {
		t.Errorf("verify-head ref %s survived a successful push:\n%s", ref, log)
	}
	if n := len(failedReports(fu)); n != 0 {
		t.Errorf("a passing stage sent %d failure reports", n)
	}
}

// TestRun_VerifyFailureC_OldBackendNoReverifyKind: against a backend that did
// not advertise supports_push_resume (one that may not decode resume_kind at
// all), the category-C report is still sent but carries NO checkpoint, so its
// body is byte-identical to a legacy failure report.
func TestRun_VerifyFailureC_OldBackendNoReverifyKind(t *testing.T) {
	repo := gateFailRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true}})
	cmd, _ := formVerifyCmd(t, noopPassOut, 0, noopOutsideOut, 1)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 2)
	fu.promptResp.SupportsPushResume = false

	code, log := runGateFail(t, repo, fu, verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}
	reports := failedReports(fu)
	if len(reports) != 1 {
		t.Fatalf("failed /pull-request reports = %d, want exactly 1:\n%s", len(reports), log)
	}
	if got := reports[0]; got.Category != "C" || got.ResumeKind != "" || got.HeadSHA != "" || got.Branch != "" {
		t.Errorf("report = category %q resume_kind %q head %q branch %q, want C with no checkpoint", got.Category, got.ResumeKind, got.HeadSHA, got.Branch)
	}
	if _, ok := refTarget(repo, checkpointVerifyRef(verifyFixRunID, verifyFixStageID)); !ok {
		t.Error("the verify head is still pinned (work preserved) against an old backend")
	}
}

// TestRun_AgentFailureC_NoCommittedGateReport: the B/C report and the pin
// apply only when a committed GATE turned a successful agent pass into a
// failure. An agent pass that itself failed category C never reaches the gates,
// so it sends no /pull-request failure report and pins nothing, exactly as
// before #4190.
func TestRun_AgentFailureC_NoCommittedGateReport(t *testing.T) {
	repo := gateFailRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: false, FailureCategory: "C", FailureReason: "agent infra fault"}})
	cmd, calls := formVerifyCmd(t, noopPassOut, 0, noopOutsideOut, 1)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 2)

	code, log := runGateFail(t, repo, fu, verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}
	if b, _ := os.ReadFile(calls); len(b) != 0 {
		t.Fatalf("fixture: the committed gate ran (%q) — an agent failure must not reach it", b)
	}
	if n := len(failedReports(fu)); n != 0 {
		t.Errorf("failed /pull-request reports = %d, want 0 for an agent (not gate) failure:\n%s", n, log)
	}
	if _, ok := refTarget(repo, checkpointVerifyRef(verifyFixRunID, verifyFixStageID)); ok {
		t.Error("a verify head was pinned for an agent failure that never reached the gates")
	}
}

// bundleVerifyRunHeads lists every non-empty verify_run head_sha in the shipped
// bundle, in order.
func bundleVerifyRunHeads(t *testing.T, events []bundle.Line) []string {
	t.Helper()
	var heads []string
	for _, ev := range events {
		if ev.Kind != "verify_run" {
			continue
		}
		var p struct {
			HeadSHA string `json:"head_sha"`
		}
		if err := json.Unmarshal(ev.Data, &p); err == nil && p.HeadSHA != "" {
			heads = append(heads, p.HeadSHA)
		}
	}
	return heads
}

// assertNoReverifyArmed asserts the stage sent exactly one category-C failure
// report carrying NO checkpoint, left no -verify pin, and logged the pin skip
// with wantReason.
func assertNoReverifyArmed(t *testing.T, repo string, fu *fakeUploader, log, wantReason string) {
	t.Helper()
	reports := failedReports(fu)
	if len(reports) != 1 {
		t.Fatalf("failed /pull-request reports = %d, want exactly 1:\n%s", len(reports), log)
	}
	if got := reports[0]; got.Category != "C" || got.ResumeKind != "" || got.HeadSHA != "" || got.BaseSHA != "" || got.Branch != "" {
		t.Errorf("report = category %q resume_kind %q head %q base %q branch %q, want C with NO checkpoint",
			got.Category, got.ResumeKind, got.HeadSHA, got.BaseSHA, got.Branch)
	}
	if target, ok := refTarget(repo, checkpointVerifyRef(verifyFixRunID, verifyFixStageID)); ok {
		t.Errorf("verify head pinned at %s for a head that is not the agent pass's change", target)
	}
	if strings.Contains(log, `"event":"reverify_checkpoint_armed"`) {
		t.Errorf("reverify_checkpoint_armed logged:\n%s", log)
	}
	if want := `"event":"verify_head_pin_skipped","run_id":"` + verifyFixRunID + `","stage_id":"` + verifyFixStageID + `","reason":"` + wantReason + `"`; !strings.Contains(log, want) {
		t.Errorf("missing %s:\n%s", want, log)
	}
}

// TestRun_FixIterationEditThenVerifyFailureC_ArmsNoCheckpoint reproduces the
// #4190 review sequence: the first fix iteration EDITS a.go and CREATES fix.go,
// a later no-op fix iteration turns the reproduced outside failure into
// category C, so the last verify head carries both files while the trace's only
// git_diff (re-emitted only on a passing stage) lists a.go alone. The reverify
// consume ships no trace, so arming that head would publish fix.go with no
// backend policy evaluation. Nothing is pinned and the C report carries no
// checkpoint: the next retry is an ordinary agent pass.
func TestRun_FixIterationEditThenVerifyFailureC_ArmsNoCheckpoint(t *testing.T) {
	repo := gateFailRepo(t)
	baseSHA := gitHead(t, repo)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	invoker := &fakeInvoker{
		mirrorWorkingTreeFrom: repo,
		canned:                agent.Result{OK: true},
		onInvoke: func(idx int, _ agent.Invocation) {
			if idx == 1 { // the first fix iteration; idx 2 is a no-op
				mustWrite(t, filepath.Join(repo, "pkg", "a", "a.go"), "package a\n\n// agent change\n// fix edit\n")
				mustWrite(t, filepath.Join(repo, "pkg", "a", "fix.go"), "package a\n\n// created by the fix agent\n")
			}
		},
	}
	withFakeInvoker(t, invoker)
	cmd, _ := formVerifyCmd(t, noopPassOut, 0, noopOutsideOut, 1)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 2)
	fu.promptResp.ScopeFiles = append(fu.promptResp.ScopeFiles, upload.ScopeFile{Path: "pkg/a/fix.go", Operation: "create"})
	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")

	code, log := runGateFail(t, repo, fu, append(verifyFixRunArgs(repo, bundlePath), "--check-base-ref", baseSHA))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}

	// Fixture preconditions: a fix iteration really changed the committed tree
	// (the first head lacks fix.go, the last carries it) and the shipped git_diff
	// never names fix.go.
	events := readBundleEvents(t, bundlePath)
	heads := bundleVerifyRunHeads(t, events)
	if len(heads) < 2 {
		t.Fatalf("fixture: want several verify_run heads, got %v:\n%s", heads, log)
	}
	first, last := heads[0], heads[len(heads)-1]
	if err := exec.Command("git", "-C", repo, "cat-file", "-e", first+":pkg/a/fix.go").Run(); err == nil {
		t.Fatalf("fixture: the first verify head %s already carries pkg/a/fix.go", first)
	}
	if err := exec.Command("git", "-C", repo, "cat-file", "-e", last+":pkg/a/fix.go").Run(); err != nil {
		t.Fatalf("fixture: the last verify head %s lacks pkg/a/fix.go — no fix iteration edit landed:\n%s", last, log)
	}
	sawDiff := false
	for _, ev := range events {
		if ev.Kind == "git_diff" {
			sawDiff = true
			if strings.Contains(string(ev.Data), "fix.go") {
				t.Fatalf("fixture: a shipped git_diff names fix.go, so it is not the reproduced shape: %s", ev.Data)
			}
		}
	}
	if !sawDiff {
		t.Fatalf("fixture: no git_diff shipped:\n%s", log)
	}

	assertNoReverifyArmed(t, repo, fu, log, "tree_changed_after_agent_pass")
}

// singleShotInfraVerify is a verify command whose every run fails with the
// golangci-lint lock infra signature, so the single-shot committed gate
// (max_iterations 0) absorbs once and then fails category C
// (gitops.ErrVerifyInfraFailure) WITHOUT any fix iteration.
func singleShotInfraVerify(t *testing.T) string {
	t.Helper()
	out := "level=error msg=\"" + golangciLintLockSignature + "\"\n"
	cmd, _ := formVerifyCmd(t, out, 1, out, 1)
	return cmd
}

// TestRun_NoFixIterationVerifyFailureC_ArmsCheckpoint is the positive control
// for the two no-arm rows: with NO fix iteration and no scope fold, a
// category-C gate failure still pins the head and arms the reverify checkpoint.
func TestRun_NoFixIterationVerifyFailureC_ArmsCheckpoint(t *testing.T) {
	repo := gateFailRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	invoker := &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(singleShotInfraVerify(t), 0)

	code, log := runGateFail(t, repo, fu, verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}
	if invoker.callIdx != 1 {
		t.Fatalf("fixture: agent invocations = %d, want 1 (no fix iteration)", invoker.callIdx)
	}
	pinned, ok := refTarget(repo, checkpointVerifyRef(verifyFixRunID, verifyFixStageID))
	if !ok {
		t.Fatalf("verify head not pinned:\n%s", log)
	}
	reports := failedReports(fu)
	if len(reports) != 1 {
		t.Fatalf("failed /pull-request reports = %d, want exactly 1:\n%s", len(reports), log)
	}
	if got := reports[0]; got.Category != "C" || got.ResumeKind != resumeKindReverify || got.HeadSHA != pinned {
		t.Errorf("report = category %q resume_kind %q head %q, want C reverify at the pin %s:\n%s",
			got.Category, got.ResumeKind, got.HeadSHA, pinned, log)
	}
}

// TestRun_ScopeAmendmentFoldedVerifyFailureC_ArmsNoCheckpoint: the positive
// control's fixture plus an operator-approved scope amendment folded at agent
// exit. The fold adds pkg/a/extra.go to the committed tree AFTER the trace's
// git_diff was computed, so the head is not pinned and no checkpoint is armed.
// The tree never changes across the gate's runs here, so the fold check alone
// decides.
func TestRun_ScopeAmendmentFoldedVerifyFailureC_ArmsNoCheckpoint(t *testing.T) {
	pinAmendmentWatchInterval(t)
	repo := gateFailRepo(t)
	mustWrite(t, filepath.Join(repo, "pkg", "a", "extra.go"), "package a\n\n// granted file\n")
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(singleShotInfraVerify(t), 0)
	fu.amendments = []upload.ScopeAmendment{{
		ID: "amd-4190", RunID: verifyFixRunID, StageID: verifyFixStageID, Status: "approved",
		Paths:  []upload.ScopeAmendmentPath{{Path: "pkg/a/extra.go", Operation: "create"}},
		Reason: "the change needs a helper file",
	}}

	code, log := runGateFail(t, repo, fu, verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}
	if !strings.Contains(log, `"event":"scope_amendments_folded"`) {
		t.Fatalf("fixture: no scope amendment was folded:\n%s", log)
	}
	assertNoReverifyArmed(t, repo, fu, log, "scope_amendment_folded")
}

// --- #5535: the pre-trace verify pin and the trace 202's stage_state --------

// runGateWith is runGateFail with caller-owned git fakes, so a row can read
// the pusher's and PR opener's calls or make them fail.
func runGateWith(t *testing.T, repo string, fu *fakeUploader, fp *fakePusher, fpr *fakePROpener, args []string) (int, string) {
	t.Helper()
	withFakeUploader(t, fu)
	withFakeGitOps(t, fp, fpr)
	var stderr strings.Builder
	code := run(args, &stderr)
	return code, stderr.String()
}

// passingGate serves gateFailRepo with a verify that passes in both forms.
func passingGate(t *testing.T, repo string) *fakeUploader {
	t.Helper()
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true}})
	cmd, _ := formVerifyCmd(t, noopPassOut, 0, noopPassOut, 0)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 2)
	return fu
}

// rawManifest decodes the manifest of the RAW bundle shipped first.
func rawManifest(t *testing.T, fu *fakeUploader) bundle.ManifestData {
	t.Helper()
	if len(fu.gotShipCalls) == 0 {
		t.Fatal("no trace shipped")
	}
	m, _, _, err := openBundleForTest(fu.gotShipCalls[0].Bundle)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestRun_TraceRejected_KeepsPinNoPush: a passing verify pins its head before
// the trace and declares it in the manifest; the backend then answers failed +
// trace_rejected, so the runner stops without pushing, opening a PR or sending
// a /pull-request report, keeps the -verify ref and exits non-zero.
func TestRun_TraceRejected_KeepsPinNoPush(t *testing.T) {
	repo := gateFailRepo(t)
	fu := passingGate(t, repo)
	fu.traceStageState, fu.traceFailureKind = "failed", "trace_rejected"
	fp, fpr := &fakePusher{}, &fakePROpener{}
	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")

	code, log := runGateWith(t, repo, fu, fp, fpr, verifyFixRunArgs(repo, bundlePath))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}
	if fp.calls != 0 || fpr.gotArgs != nil || len(fu.gotPRCalls) != 0 {
		t.Fatalf("push=%d openPR=%v /pull-request=%d, want none after trace_rejected:\n%s", fp.calls, fpr.gotArgs != nil, len(fu.gotPRCalls), log)
	}
	ref := checkpointVerifyRef(verifyFixRunID, verifyFixStageID)
	pinned, ok := refTarget(repo, ref)
	if !ok {
		t.Fatalf("pre-trace pin %s absent after trace_rejected:\n%s", ref, log)
	}
	if want := lastBundleVerifyRunHead(t, readBundleEvents(t, bundlePath)); pinned != want || want == "" {
		t.Fatalf("pinned %s, want the last verify_run head %q", pinned, want)
	}
	if got := rawManifest(t, fu).VerifyPinHeadSHA; got != pinned {
		t.Errorf("raw manifest verify_pin_head_sha = %q, want %s", got, pinned)
	}
	m, _, _, err := openBundleForTest(mustRead(t, bundlePath))
	if err != nil || m.VerifyPinHeadSHA != pinned {
		t.Errorf("redacted manifest verify_pin_head_sha = %q (%v), want %s", m.VerifyPinHeadSHA, err, pinned)
	}
	if !strings.Contains(log, `"event":"trace_time_stage_failed"`) || !strings.Contains(log, `"verify_pin_ref":"`+ref+`"`) {
		t.Errorf("log lacks trace_time_stage_failed naming %s:\n%s", ref, log)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// successReports returns every non-failed /pull-request report.
func successReports(fu *fakeUploader) int {
	n := 0
	for _, a := range fu.gotPRCalls {
		if a.Outcome != "failed" {
			n++
		}
	}
	return n
}

// TestRun_TraceReviewReject_StillPushes (W1): the #877 gating reject is NOT a
// trace-time refusal: the runner pushes, opens the PR and ships /pull-request
// as today (the backend closes the PR), and the success path releases the pin.
func TestRun_TraceReviewReject_StillPushes(t *testing.T) {
	repo := gateFailRepo(t)
	fu := passingGate(t, repo)
	fu.traceStageState, fu.traceFailureKind = "failed", "implement_review_rejected"
	fp, fpr := &fakePusher{}, &fakePROpener{}

	code, log := runGateWith(t, repo, fu, fp, fpr, verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")))
	if code != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", code, log)
	}
	if fp.calls != 1 || fpr.gotArgs == nil || successReports(fu) != 1 || len(failedReports(fu)) != 0 {
		t.Fatalf("push=%d openPR=%v success=%d failed=%d, want 1/true/1/0:\n%s", fp.calls, fpr.gotArgs != nil, successReports(fu), len(failedReports(fu)), log)
	}
	if _, ok := refTarget(repo, checkpointVerifyRef(verifyFixRunID, verifyFixStageID)); ok {
		t.Error("the pre-trace pin survived a successful push; the success path must release it")
	}
}

// TestRun_CommittedGateFailC_TraceFailed_StillReports (W2): a FAILING committed
// verify keeps its #4190 report even when the 202 says failed + trace_rejected:
// exactly one failed C report ending with the #4190 pin suffix, no push.
func TestRun_CommittedGateFailC_TraceFailed_StillReports(t *testing.T) {
	repo := gateFailRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true}})
	cmd, _ := formVerifyCmd(t, noopPassOut, 0, noopOutsideOut, 1)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 2)
	fu.traceStageState, fu.traceFailureKind = "failed", "trace_rejected"
	fp := &fakePusher{}

	code, log := runGateWith(t, repo, fu, fp, &fakePROpener{}, verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}
	ref := checkpointVerifyRef(verifyFixRunID, verifyFixStageID)
	pinned, ok := refTarget(repo, ref)
	if !ok {
		t.Fatalf("#4190 pin %s absent:\n%s", ref, log)
	}
	reports := failedReports(fu)
	if len(reports) != 1 {
		t.Fatalf("failed reports = %d, want exactly 1 (the committed-gate report):\n%s", len(reports), log)
	}
	if reports[0].Category != "C" || !strings.HasSuffix(reports[0].Reason, verifyHeadFailureSuffix(pinned, ref)) {
		t.Errorf("report = %s %.300q, want C ending with the #4190 suffix", reports[0].Category, reports[0].Reason)
	}
	if reports[0].ResumeKind != resumeKindReverify {
		t.Errorf("resume_kind = %q, want today's reverify arming", reports[0].ResumeKind)
	}
	if fp.calls != 0 {
		t.Errorf("pushed %d time(s) after a failed committed gate", fp.calls)
	}
	if strings.Contains(log, `"event":"trace_time_stage_failed"`) {
		t.Errorf("a failed committed gate must not take the trace_rejected short-circuit:\n%s", log)
	}
}

// TestRun_HeldPush_ReleasesPreTracePin: a held (running) stage that pushes
// successfully releases the pre-trace pin; the manifest still declared it.
func TestRun_HeldPush_ReleasesPreTracePin(t *testing.T) {
	repo := gateFailRepo(t)
	fu := passingGate(t, repo)
	fu.traceStageState = "running"
	fp := &fakePusher{}
	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")

	code, log := runGateWith(t, repo, fu, fp, &fakePROpener{}, verifyFixRunArgs(repo, bundlePath))
	if code != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", code, log)
	}
	if fp.calls != 1 || successReports(fu) != 1 || len(failedReports(fu)) != 0 {
		t.Fatalf("push=%d success=%d failed=%d, want 1/1/0:\n%s", fp.calls, successReports(fu), len(failedReports(fu)), log)
	}
	want := lastBundleVerifyRunHead(t, readBundleEvents(t, bundlePath))
	if got := rawManifest(t, fu).VerifyPinHeadSHA; got != want || want == "" {
		t.Errorf("manifest verify_pin_head_sha = %q, want the verified head %q", got, want)
	}
	if _, ok := refTarget(repo, checkpointVerifyRef(verifyFixRunID, verifyFixStageID)); ok {
		t.Error("the pre-trace pin survived a successful push")
	}
}

// TestRun_HeldNoChanges_ReportsFailedC_KeepsPin (W4): a held stage whose push
// finds no changes sends ONE failed C report naming the pin, keeps the ref and
// exits non-zero. Unheld (StageState ""), TestRun_ImplementStage_NoChanges_SkipsPRAndShip
// pins today's silent success.
func TestRun_HeldNoChanges_ReportsFailedC_KeepsPin(t *testing.T) {
	repo := gateFailRepo(t)
	fu := passingGate(t, repo)
	fu.traceStageState = "running"
	fp := &fakePusher{result: &gitops.CommitAndPushResult{NoChanges: true, BaseSHA: "base-sha-def"}}

	code, log := runGateWith(t, repo, fu, fp, &fakePROpener{}, verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}
	ref := checkpointVerifyRef(verifyFixRunID, verifyFixStageID)
	pinned, ok := refTarget(repo, ref)
	if !ok {
		t.Fatalf("pre-trace pin %s released on a held no-changes stage:\n%s", ref, log)
	}
	reports := failedReports(fu)
	if len(reports) != 1 {
		t.Fatalf("failed reports = %d, want 1:\n%s", len(reports), log)
	}
	r := reports[0]
	if r.Category != "C" || !strings.Contains(r.Reason, errImplementNoChangesHeld.Error()) ||
		!strings.HasSuffix(r.Reason, verifyHeadFailureSuffix(pinned, ref)) {
		t.Errorf("report = %s %q, want C naming the held no-changes and ending with the pin clause", r.Category, r.Reason)
	}
}

// heldAgentFailure drives a held stage whose agent pass itself failed with
// the given category and reason (no committed gate runs).
func heldAgentFailure(t *testing.T, category, reason string) (*fakeUploader, int, string, string) {
	t.Helper()
	repo := gateFailRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: false, FailureCategory: category, FailureReason: reason}})
	cmd, _ := formVerifyCmd(t, noopPassOut, 0, noopPassOut, 0)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 2)
	fu.traceStageState = "running"
	code, log := runGateWith(t, repo, fu, &fakePusher{}, &fakePROpener{}, verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")))
	return fu, code, log, repo
}

// heldReportToken is a redactable GitHub PAT shape (Y4).
const heldReportToken = "ghp_QwRkZtLmNpVbXcJhGfDsYeUoIaPsKqWeRtYu"

// TestRun_HeldAgentFailureC_Reports (W4 backstop, Y4): a held stage whose
// agent pass failed C sends ONE failed C report, its reason redacted: the
// token in the agent's reason never reaches the /pull-request body.
func TestRun_HeldAgentFailureC_Reports(t *testing.T) {
	fu, code, log, _ := heldAgentFailure(t, "C", "agent infra fault token="+heldReportToken)
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}
	reports := failedReports(fu)
	if len(reports) != 1 || reports[0].Category != "C" {
		t.Fatalf("failed reports = %+v, want one C report:\n%s", reports, log)
	}
	if strings.Contains(reports[0].Reason, heldReportToken) || !strings.Contains(reports[0].Reason, "agent infra fault") {
		t.Errorf("reason = %q, want the agent reason with the token redacted", reports[0].Reason)
	}
}

// TestRun_HeldAgentFailureB_Reports: the B variant keeps category B.
func TestRun_HeldAgentFailureB_Reports(t *testing.T) {
	fu, code, log, _ := heldAgentFailure(t, "B", "agent output invalid")
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}
	if reports := failedReports(fu); len(reports) != 1 || reports[0].Category != "B" {
		t.Fatalf("failed reports = %+v, want one B report:\n%s", reports, log)
	}
}

// TestRun_HeldDiffCoverageDemotion_PinsAndReportsB (Y3): a passing verify
// followed by the post-gate diff-coverage demotion (res.OK=false, B) still
// pins before the trace and declares the pin; the held backstop reports B
// naming it, and the ref is kept.
func TestRun_HeldDiffCoverageDemotion_PinsAndReportsB(t *testing.T) {
	repo := gateFailRepo(t)
	fu := passingGate(t, repo)
	fu.promptResp.DiffCoverage = &upload.DiffCoverageConfig{Command: "true", ReportPath: "coverage.lcov", MinNewLineCoverage: 80, BaseRef: "main"}
	fu.traceStageState = "running"
	orig := diffCoverageGate
	t.Cleanup(func() { diffCoverageGate = orig })
	diffCoverageGate = func(context.Context, config, *upload.DiffCoverageConfig, string, string) (agent.Event, error) {
		return agent.Event{Kind: "diff_coverage", Payload: json.RawMessage(`{"outcome":"measured"}`)}, errors.New("diff coverage: reset of the throwaway commit failed")
	}
	fp := &fakePusher{}

	code, log := runGateWith(t, repo, fu, fp, &fakePROpener{}, verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}
	ref := checkpointVerifyRef(verifyFixRunID, verifyFixStageID)
	pinned, ok := refTarget(repo, ref)
	if !ok {
		t.Fatalf("no pre-trace pin after a passing verify demoted by diff coverage:\n%s", log)
	}
	if got := rawManifest(t, fu).VerifyPinHeadSHA; got != pinned {
		t.Errorf("manifest verify_pin_head_sha = %q, want %s", got, pinned)
	}
	reports := failedReports(fu)
	if len(reports) != 1 || reports[0].Category != "B" || !strings.HasSuffix(reports[0].Reason, verifyHeadFailureSuffix(pinned, ref)) {
		t.Fatalf("failed reports = %+v, want one B report ending with the pin clause:\n%s", reports, log)
	}
	if fp.calls != 0 {
		t.Errorf("pushed %d time(s) after a demotion", fp.calls)
	}
}

// TestRun_HeldPrePushFailure_ReportNamesPin: a held stage whose push fails
// before landing (no checkpoint armed) reports C with the pin clause.
func TestRun_HeldPrePushFailure_ReportNamesPin(t *testing.T) {
	repo := gateFailRepo(t)
	fu := passingGate(t, repo)
	fu.traceStageState = "running"
	fp := &fakePusher{err: errors.New("push rejected by remote")}

	code, log := runGateWith(t, repo, fu, fp, &fakePROpener{}, verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}
	ref := checkpointVerifyRef(verifyFixRunID, verifyFixStageID)
	pinned, ok := refTarget(repo, ref)
	if !ok {
		t.Fatalf("pre-trace pin released on a failed push:\n%s", log)
	}
	reports := failedReports(fu)
	if len(reports) != 1 || !strings.HasSuffix(reports[0].Reason, verifyHeadFailureSuffix(pinned, ref)) {
		t.Fatalf("failed reports = %+v, want one ending with the pin clause:\n%s", reports, log)
	}
}

// TestRun_PreTracePinFailure_FailOpen: a pin that cannot be written (a
// directory/file ref conflict) leaves no manifest claim, and the push proceeds.
func TestRun_PreTracePinFailure_FailOpen(t *testing.T) {
	repo := gateFailRepo(t)
	ref := checkpointVerifyRef(verifyFixRunID, verifyFixStageID)
	cprGit(t, repo, "update-ref", ref+"/blocker", "HEAD")
	fu := passingGate(t, repo)
	fu.traceStageState = "running"
	fp := &fakePusher{}

	code, log := runGateWith(t, repo, fu, fp, &fakePROpener{}, verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")))
	if code != exitOK {
		t.Fatalf("run = %d, want exitOK (the pin is fail-open):\n%s", code, log)
	}
	if !strings.Contains(log, `"event":"verify_head_pin_failed"`) {
		t.Fatalf("fixture: the pin did not fail:\n%s", log)
	}
	if got := rawManifest(t, fu).VerifyPinHeadSHA; got != "" {
		t.Errorf("manifest claims %q for a pin that failed", got)
	}
	if fp.calls != 1 {
		t.Errorf("push calls = %d, want 1", fp.calls)
	}
}

// TestRun_HeldRedactedUploadFailure_SendsHeldReport (Y6): the raw upload
// answered running and the redacted upload failed: the held /pull-request
// failure report is sent before exiting, and the reap self-report still runs.
func TestRun_HeldRedactedUploadFailure_SendsHeldReport(t *testing.T) {
	repo := gateFailRepo(t)
	fu := passingGate(t, repo)
	fu.promptResp.StageAttempt = selfReportAttemptToken
	fu.traceStageState = "running"
	fu.shipErrByVariant = map[string]error{"redacted": errors.New("backend 503 on redacted trace")}
	fp := &fakePusher{}

	code, log := runGateWith(t, repo, fu, fp, &fakePROpener{}, verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}
	ref := checkpointVerifyRef(verifyFixRunID, verifyFixStageID)
	pinned, ok := refTarget(repo, ref)
	if !ok {
		t.Fatalf("pre-trace pin absent:\n%s", log)
	}
	reports := failedReports(fu)
	if len(reports) != 1 || reports[0].Category != "C" || !strings.Contains(reports[0].Reason, "503 on redacted trace") ||
		!strings.HasSuffix(reports[0].Reason, verifyHeadFailureSuffix(pinned, ref)) {
		t.Fatalf("failed reports = %+v, want one C held report ending with the pin clause:\n%s", reports, log)
	}
	if fp.calls != 0 {
		t.Errorf("pushed after a failed trace upload")
	}
	if rf := fu.runnerFailures(); len(rf) != 1 || rf[0].Reason != "trace_upload" {
		t.Errorf("runner failure self-reports = %+v, want one trace_upload", rf)
	}
}

// TestRun_PackRefusal_SelfReports (W10): an over-bound policy_event makes Pack
// refuse; the runner self-reports bundle_pack, ships no trace and fails.
func TestRun_PackRefusal_SelfReports(t *testing.T) {
	repo := gateFailRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	big, _ := json.Marshal(map[string]string{"blob": strings.Repeat("p", bundle.MaxLineBytes+1)})
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "policy_event", Payload: big}}}})
	cmd, _ := formVerifyCmd(t, noopPassOut, 0, noopPassOut, 0)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 2)
	fu.promptResp.StageAttempt = selfReportAttemptToken

	code, log := runGateWith(t, repo, fu, &fakePusher{}, &fakePROpener{}, verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%.2000s", code, log)
	}
	if len(fu.gotShipCalls) != 0 {
		t.Errorf("shipped %d trace(s) after a pack refusal", len(fu.gotShipCalls))
	}
	rf := fu.runnerFailures()
	if len(rf) != 1 || rf[0].Reason != "bundle_pack" || !strings.Contains(rf[0].Detail, "MaxLineBytes") {
		t.Fatalf("self-reports = %+v, want one bundle_pack:\n%.2000s", rf, log)
	}
}

// TestRun_ManifestReasonBounded: a category-A verify failure whose output is
// 5 MiB produces a manifest line near MaxFailureReportReasonBytes, keeps the
// leading `verify command` token and ends with the #4190 suffix.
func TestRun_ManifestReasonBounded(t *testing.T) {
	repo := gateFailRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true}})
	huge := noopInsideOut + strings.Repeat("filler line of verify output\n", (5<<20)/29)
	cmd, _ := formVerifyCmd(t, huge, 1, huge, 1)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 1)
	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")

	code, log := runGateWith(t, repo, fu, &fakePusher{}, &fakePROpener{}, verifyFixRunArgs(repo, bundlePath))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%.3000s", code, log)
	}
	for _, data := range [][]byte{fu.gotShipCalls[0].Bundle, mustRead(t, bundlePath)} {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		plain, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		line0, _, _ := bytes.Cut(plain, []byte("\n"))
		if max := upload.MaxFailureReportReasonBytes + 64<<10; len(line0) > max {
			t.Errorf("manifest line = %d bytes, want <= %d", len(line0), max)
		}
		m, _, _, err := openBundleForTest(data)
		if err != nil {
			t.Fatal(err)
		}
		if !m.AgentFailed || !strings.HasPrefix(m.AgentFailureReason, "verify command") {
			t.Errorf("agent_failure_reason = %.120q, want a category-A `verify command` lead", m.AgentFailureReason)
		}
		ref := checkpointVerifyRef(verifyFixRunID, verifyFixStageID)
		pinned, ok := refTarget(repo, ref)
		if !ok || !strings.HasSuffix(m.AgentFailureReason, verifyHeadFailureSuffix(pinned, ref)) {
			t.Errorf("agent_failure_reason does not end with the #4190 suffix (pinned=%t): %.200q", ok, m.AgentFailureReason[max(0, len(m.AgentFailureReason)-200):])
		}
	}
}

// TestRun_ManifestReasonRedactedBeforeTruncate (W6): one redactable token
// straddles TruncateReason's head cut and another its tail cut. Redaction runs
// over the FULL reason first, so no 8-byte fragment of either token reaches
// the redacted bundle.
func TestRun_ManifestReasonRedactedBeforeTruncate(t *testing.T) {
	const tokA = "ghp_QwRkZtLmNpVbXcJhGfDsYeUoIaPsKqWeRtYu"
	const tokB = "ghp_ZxCvBnMaSdFgHjKlPoIuYtReWqLkJhGfDsAa"
	max := upload.MaxFailureReportReasonBytes
	total := 200_000
	upper := fmt.Sprintf("\n… [truncated %d bytes] …\n", total)
	keep := max - len(upper)
	head := keep / 2
	tailStart := total - (keep - head)
	b := []byte(strings.Repeat(".", total))
	copy(b, "agent failed: ")
	copy(b[head-len(tokA)/2:], tokA)
	copy(b[tailStart-len(tokB)/2:], tokB)
	reason := string(b)

	repo := gateFailRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: false, FailureCategory: "A", FailureReason: reason}})
	cmd, _ := formVerifyCmd(t, noopPassOut, 0, noopPassOut, 0)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 1)
	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	code, log := runGateWith(t, repo, fu, &fakePusher{}, &fakePROpener{}, verifyFixRunArgs(repo, bundlePath))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%.2000s", code, log)
	}
	if got := rawManifest(t, fu).AgentFailureReason; got != upload.TruncateReason(reason, max) {
		t.Fatalf("fixture: the raw manifest reason is not TruncateReason(reason), so the cut offsets are wrong (len %d)", len(got))
	}
	zr, err := gzip.NewReader(bytes.NewReader(mustRead(t, bundlePath)))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{tokA, tokB} {
		for i := 0; i+8 <= len(tok); i++ {
			if frag := tok[i : i+8]; bytes.Contains(plain, []byte(frag)) {
				t.Errorf("redacted bundle carries fragment %q of a token that straddled a cut", frag)
				break
			}
		}
	}
}

// TestManifestFailureReasons_KeepsInfraFlakeSignature (Y1): a 5 MiB verify
// output whose only infra-flake signature (a docker.sock dial failure) sits in
// the middle, the part the head+tail cut drops, still classifies as an infra
// flake after bounding: the marker the backend classifier accepts is inserted
// before the trailing #4190 pin clause, which stays last.
func TestManifestFailureReasons_KeepsInfraFlakeSignature(t *testing.T) {
	filler := strings.Repeat("ok  \texample.com/m/pkg/x\t0.01s\n", (5<<20)/64)
	clause := verifyHeadFailureSuffix(strings.Repeat("a", 40), checkpointVerifyRef(verifyFixRunID, verifyFixStageID))
	full := `verify command "scripts/verify-change" still failing after 2 iteration(s):` + "\n" + filler +
		"dial unix /var/run/docker.sock: connect: no such file or directory\n" + filler + clause
	if !backendInfraFlakeSignature(full) {
		t.Fatal("fixture: the full reason carries no backend-recognized signature")
	}
	if backendInfraFlakeSignature(upload.TruncateReason(full, upload.MaxFailureReportReasonBytes)) {
		t.Fatal("fixture: the bare cut keeps the signature, so the marker is not what preserves it")
	}
	raw, redacted, _ := manifestFailureReasons(full)
	for name, got := range map[string]string{"raw": raw, "redacted": redacted} {
		if len(got) > upload.MaxFailureReportReasonBytes {
			t.Errorf("%s: %d bytes, over the bound", name, len(got))
		}
		if !strings.Contains(got, "verify_infra_flake_retry") || !backendInfraFlakeSignature(got) {
			t.Errorf("%s: bounded reason lost the infra-flake classification", name)
		}
		if !strings.HasSuffix(got, clause) {
			t.Errorf("%s: the pin clause is no longer last: %.200q", name, got[len(got)-200:])
		}
		if !strings.HasPrefix(got, "verify command") {
			t.Errorf("%s: lost the leading classification token", name)
		}
	}
	// A reason that fits, or one with no signature, is untouched.
	if raw, _, _ := manifestFailureReasons("verify command failed: short"); raw != "verify command failed: short" {
		t.Errorf("a fitting reason changed: %q", raw)
	}
	if raw, _, _ := manifestFailureReasons("x" + filler); strings.Contains(raw, "verify_infra_flake_retry") {
		t.Error("a reason with no signature gained the marker")
	}
}

// TestRun_HeldCommittedGateFailC_SingleReport: a held stage whose committed
// gate failed C sends the #4190 report once; the held backstop sees
// pushReportSent and does not send a second.
func TestRun_HeldCommittedGateFailC_SingleReport(t *testing.T) {
	repo := gateFailRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true}})
	cmd, _ := formVerifyCmd(t, noopPassOut, 0, noopOutsideOut, 1)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 2)
	fu.traceStageState = "running"

	code, log := runGateWith(t, repo, fu, &fakePusher{}, &fakePROpener{}, verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")))
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}
	if reports := failedReports(fu); len(reports) != 1 || reports[0].ResumeKind != resumeKindReverify {
		t.Fatalf("failed reports = %d (%+v), want exactly the one #4190 reverify report:\n%s", len(reports), reports, log)
	}
}

// TestPreTracePinClause: the clause renders only for a pin, on a report that
// arms no checkpoint and whose reason names no checkpoint ref.
func TestPreTracePinClause(t *testing.T) {
	pin := verifyHeadPin{ref: checkpointVerifyRef(verifyFixRunID, verifyFixStageID), headSHA: strings.Repeat("b", 40)}
	want := verifyHeadFailureSuffix(pin.headSHA, pin.ref)
	for _, tc := range []struct {
		name          string
		reason        string
		pinned, armed bool
		want          string
	}{
		{"pinned plain reason", "push failed", true, false, want},
		{"not pinned", "push failed", false, false, ""},
		{"report arms a checkpoint", "push failed", true, true, ""},
		{"reason already names a ref", "x pinned at refs/fishhawk/checkpoints/r/s-verify", true, false, ""},
	} {
		if got := preTracePinClause(tc.reason, pin, tc.pinned, tc.armed); got != tc.want {
			t.Errorf("%s: clause = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestPinPassingVerifyHead_NoVerifyRunIsSilent: a push-gated stage with no
// committed verify pins nothing and logs nothing.
func TestPinPassingVerifyHead_NoVerifyRunIsSilent(t *testing.T) {
	var log strings.Builder
	if _, ok := pinPassingVerifyHead(context.Background(), config{runID: verifyFixRunID, stageID: verifyFixStageID},
		[]agent.Event{{Kind: "verify_run", Payload: json.RawMessage(`{"head_sha":""}`)}}, &log); ok {
		t.Fatal("pinned with no verify_run head")
	}
	if log.Len() != 0 {
		t.Errorf("log = %q, want silence", log.String())
	}
}
