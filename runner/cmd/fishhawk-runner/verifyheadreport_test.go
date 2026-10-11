package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/bundle"
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
