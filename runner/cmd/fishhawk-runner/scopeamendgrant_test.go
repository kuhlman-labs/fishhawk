package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// --- #3390 approved-grant delivery into the verify-fix loop + unused-grant fail-loud ---

func grantScope() []upload.ScopeFile {
	return []upload.ScopeFile{
		{Path: "mod/reg.go", Operation: "modify"},
		{Path: "mod/reg_test.go", Operation: "create"},
		{Path: "", Operation: "modify"}, // empty path: skipped
	}
}

func grantRow(id, stageID, decisionReason string) upload.ScopeAmendment {
	return upload.ScopeAmendment{
		ID:             id,
		RunID:          verifyFixRunID,
		StageID:        stageID,
		Status:         "approved",
		Paths:          []upload.ScopeAmendmentPath{{Path: "mod/other.go", Operation: "modify"}},
		Reason:         "AGENT-AUTHORED-REASON-MUST-NOT-RENDER",
		DecisionReason: decisionReason,
	}
}

// TestVerifyFixPrompt_RendersEffectiveScope: every non-empty scope path + op
// appears under the effective-scope header in scope order; an empty scope
// (the `git add -A` fallback) omits the section and keeps the legacy
// "approved scope" wording; the Command/Output blocks are unchanged.
func TestVerifyFixPrompt_RendersEffectiveScope(t *testing.T) {
	prompt, elided := verifyFixPrompt("go test ./...", "--- FAIL: TestGet", grantScope(), nil)
	if elided != 0 {
		t.Fatalf("elided = %d, want 0", elided)
	}
	if !strings.Contains(prompt, "Command:\ngo test ./...\n\nOutput:\n--- FAIL: TestGet\n") {
		t.Errorf("Command/Output blocks changed:\n%s", prompt)
	}
	if !strings.Contains(prompt, verifyFixEffectiveScopeHeader) {
		t.Errorf("missing the effective-scope header:\n%s", prompt)
	}
	i := strings.Index(prompt, "- mod/reg.go (modify)\n")
	j := strings.Index(prompt, "- mod/reg_test.go (create)\n")
	if i < 0 || j < 0 || j < i {
		t.Errorf("scope lines missing or out of order (%d, %d):\n%s", i, j, prompt)
	}
	if strings.Contains(prompt, "- \n") || strings.Contains(prompt, "-  (modify)") {
		t.Errorf("empty scope path must be skipped:\n%s", prompt)
	}
	if !strings.Contains(prompt, `the files listed under
"Effective scope" above`) {
		t.Errorf("closing paragraph must point at the effective-scope list:\n%s", prompt)
	}
	if strings.Contains(prompt, verifyFixGrantedMarker) {
		t.Errorf("nil granted must render no GRANTED block:\n%s", prompt)
	}

	// Empty scope: section omitted, legacy wording kept.
	empty, _ := verifyFixPrompt("go test ./...", "out", nil, nil)
	if strings.Contains(empty, verifyFixEffectiveScopeHeader) {
		t.Errorf("empty scope must omit the effective-scope section:\n%s", empty)
	}
	if !strings.Contains(empty, "already allowed to change (the approved scope)") {
		t.Errorf("empty scope must keep the legacy wording:\n%s", empty)
	}
}

// TestVerifyFixPrompt_RendersGrantedMidStage: the GRANTED MID-STAGE block
// names the amendment id, `path (op)`, the operator decision reason and the
// in-force instruction; an empty DecisionReason omits the reason line; the
// agent-authored `reason` NEVER renders; nil granted omits the block.
func TestVerifyFixPrompt_RendersGrantedMidStage(t *testing.T) {
	granted := []upload.ScopeAmendment{
		grantRow("amd-1", verifyFixStageID, "edit only the init func"),
		{ID: "amd-2", Status: "approved", StageID: verifyFixStageID, Reason: "AGENT-AUTHORED-REASON-MUST-NOT-RENDER",
			Paths: []upload.ScopeAmendmentPath{{Path: "docs/x.md", Operation: "create"}, {Path: "", Operation: "modify"}, {Path: "pkg/noop.go"}}},
		{ID: "amd-empty", Status: "approved", StageID: verifyFixStageID},
	}
	prompt, _ := verifyFixPrompt("go test ./...", "out", grantScope(), granted)
	for _, want := range []string{
		verifyFixGrantedMarker,
		"- amendment amd-1 granted mod/other.go (modify)\n  Operator decision reason: edit only the init func\n",
		"- amendment amd-2 granted docs/x.md (create), pkg/noop.go\n",
		"- amendment amd-empty\n",
		"are in force for this fix",
		"do not treat the grant as\npending or denied",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "AGENT-AUTHORED-REASON-MUST-NOT-RENDER") {
		t.Errorf("agent-authored amendment reason must never render:\n%s", prompt)
	}
	// amd-2 has no DecisionReason: exactly ONE reason line in the whole prompt.
	if n := strings.Count(prompt, "Operator decision reason:"); n != 1 {
		t.Errorf("Operator decision reason lines = %d, want 1 (empty DecisionReason omits the line):\n%s", n, prompt)
	}
	// Order: Output → Effective scope → GRANTED → closing paragraph.
	o, s, g, c := strings.Index(prompt, "Output:"), strings.Index(prompt, verifyFixEffectiveScopeHeader),
		strings.Index(prompt, verifyFixGrantedMarker), strings.Index(prompt, "Edit the code so this command passes")
	if o >= s || s >= g || g >= c {
		t.Errorf("section order = output %d, scope %d, granted %d, closing %d:\n%s", o, s, g, c, prompt)
	}

	none, _ := verifyFixPrompt("go test ./...", "out", grantScope(), nil)
	if strings.Contains(none, verifyFixGrantedMarker) || strings.Contains(none, "amendment") {
		t.Errorf("nil granted must omit the GRANTED block entirely:\n%s", none)
	}
}

// TestUnusedScopeAmendmentGrants is the pure-predicate table: which granted
// paths count as unused against a dirty set, which rows are ignored, and which
// paths are skipped by the MissingScopeFiles-mirroring rules.
func TestUnusedScopeAmendmentGrants(t *testing.T) {
	const stage = verifyFixStageID
	row := func(id string, paths ...upload.ScopeAmendmentPath) upload.ScopeAmendment {
		return upload.ScopeAmendment{ID: id, Status: "approved", StageID: stage, Paths: paths}
	}
	mp := func(p, op string) upload.ScopeAmendmentPath { return upload.ScopeAmendmentPath{Path: p, Operation: op} }

	cases := []struct {
		name     string
		approved []upload.ScopeAmendment
		stageID  string
		dirty    []string
		want     []unusedScopeAmendmentGrant
	}{
		{"untouched modify -> unused", []upload.ScopeAmendment{row("a", mp("mod/other.go", "modify"))}, stage, []string{"mod/reg.go"},
			[]unusedScopeAmendmentGrant{{"a", "mod/other.go", "modify"}}},
		{"modified path in dirty set -> used", []upload.ScopeAmendment{row("a", mp("mod/other.go", "modify"))}, stage, []string{"mod/other.go"}, nil},
		{"created path in dirty set -> used", []upload.ScopeAmendment{row("a", mp("mod/new.go", "create"))}, stage, []string{"mod/new.go"}, nil},
		{"deleted path in dirty set -> used", []upload.ScopeAmendment{row("a", mp("mod/old.go", "delete"))}, stage, []string{"mod/old.go"}, nil},
		{"other stage's row ignored", []upload.ScopeAmendment{{ID: "a", Status: "approved", StageID: undecidedOtherStageID, Paths: []upload.ScopeAmendmentPath{mp("mod/other.go", "modify")}}}, stage, nil, nil},
		{"non-approved row ignored", []upload.ScopeAmendment{{ID: "a", Status: "pending", StageID: stage, Paths: []upload.ScopeAmendmentPath{mp("mod/other.go", "modify")}}}, stage, nil, nil},
		{"empty stageID -> nil", []upload.ScopeAmendment{row("a", mp("mod/other.go", "modify"))}, "", nil, nil},
		{"skip rules: trailing-slash, empty, absolute, dotdot", []upload.ScopeAmendment{row("a",
			mp("corpus/new/", "create"), mp("", "modify"), mp("/etc/passwd", "modify"), mp("../up.go", "modify"), mp("keep.go", "modify"))},
			stage, nil, []unusedScopeAmendmentGrant{{"a", "keep.go", "modify"}}},
		{"order preserved across rows and paths", []upload.ScopeAmendment{row("a", mp("z.go", "modify"), mp("y.go", "create")), row("b", mp("x.go", ""))},
			stage, nil, []unusedScopeAmendmentGrant{{"a", "z.go", "modify"}, {"a", "y.go", "create"}, {"b", "x.go", ""}}},
		{"no approved rows -> nil", nil, stage, []string{"a"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unusedScopeAmendmentGrants(tc.approved, tc.stageID, tc.dirty)
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] got %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestAnnotateUnusedAmendmentFailure_ExactLine pins the byte-exact attribution
// line, the preserved reason prefix, the empty-operation form, and the
// empty-list identity.
func TestAnnotateUnusedAmendmentFailure_ExactLine(t *testing.T) {
	const reason = "verify command \"go test\" still failing after 2 iteration(s):\n--- FAIL: TestGet"
	got := annotateUnusedAmendmentFailure(reason, []unusedScopeAmendmentGrant{
		{"amd-e2e", "mod/other.go", "modify"},
		{"amd-2", "pkg/noop.go", ""},
	})
	if !strings.HasPrefix(got, reason+"\n\n") {
		t.Errorf("reason prefix not preserved:\n%s", got)
	}
	for _, want := range []string{
		"amendment amd-e2e granted mod/other.go (modify); file not modified\n",
		"amendment amd-2 granted pkg/noop.go; file not modified\n",
		"fishhawk_retry_stage",
		"never edited",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("annotation missing %q:\n%s", want, got)
		}
	}
	if same := annotateUnusedAmendmentFailure(reason, nil); same != reason {
		t.Errorf("empty list must return reason unchanged, got:\n%s", same)
	}
}

// TestDetectUnusedScopeAmendmentGrants_GitErrorFailsOpen: a non-git workingDir
// makes `git status` fail — the check logs scope_amendment_grant_check_failed
// and returns nil, nil (no grants, no events) instead of failing the stage.
func TestDetectUnusedScopeAmendmentGrants_GitErrorFailsOpen(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cfg := config{
		runID:              verifyFixRunID,
		stageID:            verifyFixStageID,
		workingDir:         t.TempDir(), // not a git repository
		approvedAmendments: []upload.ScopeAmendment{grantRow("amd-e2e", verifyFixStageID, "")},
	}
	var log bytes.Buffer
	unused, events := detectUnusedScopeAmendmentGrants(context.Background(), cfg, &log)
	if unused != nil || events != nil {
		t.Fatalf("got unused=%+v events=%+v, want nil,nil (fail-open)", unused, events)
	}
	if !strings.Contains(log.String(), `"event":"scope_amendment_grant_check_failed"`) {
		t.Errorf("missing scope_amendment_grant_check_failed:\n%s", log.String())
	}
	if strings.Contains(log.String(), "scope_amendment_grant_unused") {
		t.Errorf("a failed check must not emit the unused signal:\n%s", log.String())
	}
}

// TestDetectUnusedScopeAmendmentGrants_OtherStageOnly_NoOp (approval condition
// 3): approved rows exist ONLY for a different stage id — the run() gate
// (len(cfg.approvedAmendments) > 0) would admit them, so the inner stage
// filter is what must return nil,nil with no git call and no line.
func TestDetectUnusedScopeAmendmentGrants_OtherStageOnly_NoOp(t *testing.T) {
	cfg := config{
		runID:      verifyFixRunID,
		stageID:    verifyFixStageID,
		workingDir: t.TempDir(), // NOT a git repo: a git call here would log check_failed
		approvedAmendments: []upload.ScopeAmendment{
			grantRow("amd-other", undecidedOtherStageID, ""),
		},
	}
	var log bytes.Buffer
	unused, events := detectUnusedScopeAmendmentGrants(context.Background(), cfg, &log)
	if unused != nil || events != nil {
		t.Fatalf("got unused=%+v events=%+v, want nil,nil", unused, events)
	}
	if log.Len() != 0 {
		t.Errorf("other-stage-only rows must emit nothing (no git call, no line), got:\n%s", log.String())
	}
	// Empty stageID: same no-op, same silence.
	cfg.stageID = ""
	cfg.approvedAmendments = []upload.ScopeAmendment{grantRow("amd-e2e", "", "")}
	if u, e := detectUnusedScopeAmendmentGrants(context.Background(), cfg, &log); u != nil || e != nil || log.Len() != 0 {
		t.Errorf("empty stageID must be a silent no-op, got unused=%+v events=%+v log=%q", u, e, log.String())
	}
}

// TestEmitScopeAmendmentGrantUnused_SeamContract pins the JSONL field set
// {event, run_id, stage_id, grants:[{amendment_id, path, operation}]} and the
// policy_event payload {check, grants} against a REAL git repo whose dirty set
// excludes the granted path.
func TestEmitScopeAmendmentGrantUnused_SeamContract(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetBuggy) // dirty, but not the grant
	cfg := config{
		runID:      "run-abc",
		stageID:    verifyFixStageID,
		workingDir: repo,
		approvedAmendments: []upload.ScopeAmendment{
			grantRow("amd-7", verifyFixStageID, "why"),
			// A used grant and another stage's grant must not reach the wire.
			{ID: "amd-8", Status: "approved", StageID: verifyFixStageID, Paths: []upload.ScopeAmendmentPath{{Path: "mod/reg.go", Operation: "modify"}}},
			grantRow("amd-9", undecidedOtherStageID, ""),
		},
	}
	var log bytes.Buffer
	unused, events := detectUnusedScopeAmendmentGrants(context.Background(), cfg, &log)
	if len(unused) != 1 || unused[0] != (unusedScopeAmendmentGrant{"amd-7", "mod/other.go", "modify"}) {
		t.Fatalf("unused = %+v, want exactly amd-7 mod/other.go", unused)
	}
	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("emitted %d lines, want exactly 1: %q", len(lines), log.String())
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("emitted line is not one JSON object: %v (%q)", err, lines[0])
	}
	if got["event"] != "scope_amendment_grant_unused" || got["run_id"] != "run-abc" || got["stage_id"] != verifyFixStageID {
		t.Errorf("event/run_id/stage_id = %v/%v/%v", got["event"], got["run_id"], got["stage_id"])
	}
	rows, ok := got["grants"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("grants = %v, want a 1-element array", got["grants"])
	}
	row, _ := rows[0].(map[string]any)
	if row["amendment_id"] != "amd-7" || row["path"] != "mod/other.go" || row["operation"] != "modify" {
		t.Errorf("grant row = %v", row)
	}
	if len(row) != 3 {
		t.Errorf("grant row has %d fields, want exactly 3 {amendment_id, path, operation}: %v", len(row), row)
	}
	if len(got) != 4 {
		t.Errorf("line has %d fields, want exactly 4 {event, run_id, stage_id, grants}: %v", len(got), got)
	}
	if len(events) != 1 || events[0].Kind != "policy_event" {
		t.Fatalf("events = %+v, want one policy_event", events)
	}
	var payload struct {
		Check  string           `json:"check"`
		Grants []map[string]any `json:"grants"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Check != "scope_amendment_grant_unused" || len(payload.Grants) != 1 || payload.Grants[0]["amendment_id"] != "amd-7" {
		t.Errorf("policy_event payload = %s", events[0].Payload)
	}
}

// approvedGrantRow is the e2e fixture row: the #2601 undecided row, APPROVED,
// with an operator decision reason — run faf4cf76's shape.
func approvedGrantRow() upload.ScopeAmendment {
	a := undecidedAmendmentRow("approved")
	a.DecisionReason = "seed the registry from an init in mod/other.go"
	return a
}

// grantFixPromptAssertions checks a captured iteration-1 fix prompt carries
// the grant: amendment id, `path (op)`, the operator decision reason, the
// GRANTED MID-STAGE marker, and the effective-scope list (which includes the
// folded path).
func grantFixPromptAssertions(t *testing.T, prompt string) {
	t.Helper()
	if prompt == "" {
		t.Fatal("iteration-1 fix prompt was never captured (no reinvoke?)")
	}
	for _, want := range []string{
		verifyFixGrantedMarker,
		"- amendment amd-e2e granted mod/other.go (modify)\n",
		"Operator decision reason: seed the registry from an init in mod/other.go",
		verifyFixEffectiveScopeHeader,
		"- mod/reg.go (modify)\n",
		"- mod/other.go (modify)\n",
		"in force for this fix",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("fix prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "the fix needs a file the plan did not declare") {
		t.Errorf("fix prompt must not render the agent-authored amendment reason:\n%s", prompt)
	}
}

const unusedGrantLine = "amendment amd-e2e granted mod/other.go (modify); file not modified"

// TestRun_ApprovedScopeAmendment_FixPromptCarriesGrant_UnusedGrantFailsLoud is
// the #3390 DONE-MEANS test, driven end to end through run(): an APPROVED
// amendment for THIS stage (folded before the verify-fix loop) whose path the
// fake agent never touches. The iteration-1 fix prompt must carry the grant,
// and the exhausted stage must fail category-A with the byte-exact
// `amendment <id> granted <path> (<op>); file not modified` line, the
// scope_amendment_grant_unused JSONL line, the policy_event, and no push.
//
// The bad state is seeded BY CONSTRUCTION (an approved row whose path the
// agent never writes), never by calling the control inside setup.
func TestRun_ApprovedScopeAmendment_FixPromptCarriesGrant_UnusedGrantFailsLoud(t *testing.T) {
	pinAmendmentWatchInterval(t)
	repo := verifyFixBaseRepo(t)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetBuggy)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	var fixPrompt string
	invoker := &fakeInvoker{
		mirrorWorkingTreeFrom: repo,
		canned:                agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
		// The fix agent fixes NOTHING: it only lets the test read its prompt.
		onInvoke: func(idx int, inv agent.Invocation) {
			if idx == 1 {
				fixPrompt = inv.Prompt
			}
		},
	}
	withFakeInvoker(t, invoker)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = undecidedVerifyFixPrompt() // VerifyMaxIterations 1
	fu.amendments = []upload.ScopeAmendment{approvedGrantRow()}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	if got := run(verifyFixRunArgs(repo, bundlePath), &stderr); got != exitFailure {
		t.Errorf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	// (i) the fix re-invocation prompt names the grant.
	grantFixPromptAssertions(t, fixPrompt)
	// (ii) category-A exhaustion with the byte-exact attribution line.
	if !strings.Contains(stderr.String(), `"category":"A"`) {
		t.Errorf("expected a category-A terminal demotion:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), unusedGrantLine) {
		t.Errorf("failure reason missing the exact line %q:\n%s", unusedGrantLine, stderr.String())
	}
	// (iii) the JSONL signal and the bundle policy_event.
	if !strings.Contains(stderr.String(), `"event":"scope_amendment_grant_unused"`) {
		t.Errorf("missing the scope_amendment_grant_unused JSONL signal:\n%s", stderr.String())
	}
	events := readBundleEvents(t, bundlePath)
	if !hasPolicyEvent(events, "scope_amendment_grant_unused") {
		t.Error("bundle missing the scope_amendment_grant_unused policy_event")
	}
	// (iv) no push, and the budget was spent (initial + 1 fix).
	if fp.gotArgs != nil || fpr.gotArgs != nil {
		t.Error("CommitAndPush/OpenPR must not run after a terminal verify-fix exhaustion")
	}
	if invoker.callIdx != 2 {
		t.Errorf("Invoke call count = %d, want 2 (initial + 1 fix)", invoker.callIdx)
	}
}

// TestRun_ApprovedScopeAmendment_GrantUsed_NoUnusedSignal is the
// discriminating twin over the IDENTICAL fixture: the fix agent EDITS the
// granted file (the seed-init shape), iteration 2 passes, and NO unused
// line/event/annotation appears — so the check reads the tree, not the grant
// list. It ALSO asserts positively that the iteration-1 fix prompt carried the
// grant (approval condition 2), so deleting the fold record reddens this twin
// too, not only the fail-loud test.
func TestRun_ApprovedScopeAmendment_GrantUsed_NoUnusedSignal(t *testing.T) {
	pinAmendmentWatchInterval(t)
	repo := verifyFixBaseRepo(t)
	baseSHA := gitHead(t, repo)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetBuggy)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	var fixPrompt string
	invoker := &fakeInvoker{
		mirrorWorkingTreeFrom: repo,
		canned:                agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
		onInvoke: func(idx int, inv agent.Invocation) {
			if idx == 1 {
				fixPrompt = inv.Prompt
				// The fix lands in the GRANTED file, exactly as the prompt asks.
				mustWrite(t, filepath.Join(repo, "mod", "other.go"),
					"package mod\n\nfunc init() { registry[\"x\"] = 42 }\n")
			}
		},
	}
	withFakeInvoker(t, invoker)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = undecidedVerifyFixPrompt()
	fu.amendments = []upload.ScopeAmendment{approvedGrantRow()}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	args := append(verifyFixRunArgs(repo, bundlePath), "--check-base-ref", baseSHA)
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	// POSITIVE grant delivery (condition 2).
	grantFixPromptAssertions(t, fixPrompt)
	if invoker.callIdx != 2 {
		t.Errorf("Invoke call count = %d, want 2 (initial + 1 fix that converged)", invoker.callIdx)
	}
	// NO unused signal anywhere: log line, policy_event, or failure reason.
	if strings.Contains(stderr.String(), "scope_amendment_grant_unused") {
		t.Errorf("a USED grant must emit no scope_amendment_grant_unused line:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "file not modified") {
		t.Errorf("a USED grant must add no failure-reason annotation:\n%s", stderr.String())
	}
	if hasPolicyEvent(readBundleEvents(t, bundlePath), "scope_amendment_grant_unused") {
		t.Error("bundle must carry no scope_amendment_grant_unused policy_event for a used grant")
	}
	if fp.gotArgs == nil {
		t.Error("CommitAndPush must run: the fix converged")
	}
}

// TestRun_NoAmendment_FixPromptScopeOnly is the no-grant control over the
// same harness: a stage with NO amendments produces a fix prompt with the
// effective-scope list but no GRANTED block, no unused signal, and no
// scope_amendment_grant_check_failed line (the check never ran).
func TestRun_NoAmendment_FixPromptScopeOnly(t *testing.T) {
	pinAmendmentWatchInterval(t)
	repo := verifyFixBaseRepo(t)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetBuggy)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	var fixPrompt string
	invoker := &fakeInvoker{
		mirrorWorkingTreeFrom: repo,
		canned:                agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
		onInvoke: func(idx int, inv agent.Invocation) {
			if idx == 1 {
				fixPrompt = inv.Prompt
			}
		},
	}
	withFakeInvoker(t, invoker)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = undecidedVerifyFixPrompt()
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	if got := run(verifyFixRunArgs(repo, bundlePath), &stderr); got != exitFailure {
		t.Errorf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if fixPrompt == "" {
		t.Fatal("iteration-1 fix prompt was never captured")
	}
	for _, want := range []string{verifyFixEffectiveScopeHeader, "- mod/reg.go (modify)\n", "- mod/reg_test.go (create)\n"} {
		if !strings.Contains(fixPrompt, want) {
			t.Errorf("fix prompt missing %q:\n%s", want, fixPrompt)
		}
	}
	if strings.Contains(fixPrompt, verifyFixGrantedMarker) {
		t.Errorf("no-amendment stage must render no GRANTED block:\n%s", fixPrompt)
	}
	for _, absent := range []string{"scope_amendment_grant_unused", "scope_amendment_grant_check_failed", "file not modified"} {
		if strings.Contains(stderr.String(), absent) {
			t.Errorf("no-amendment stage must not emit %q:\n%s", absent, stderr.String())
		}
	}
}

func hasPolicyEvent(events []bundle.Line, check string) bool {
	for _, ev := range events {
		if ev.Kind == "policy_event" && strings.Contains(string(ev.Data), `"`+check+`"`) {
			return true
		}
	}
	return false
}
