package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// --- #3434 mid-loop scope-amendment RE-FOLD in the verify-fix loop ---

// TestRun_ScopeAmendmentApprovedMidFixLoop_FoldedIntoRecommit is the #3434
// DONE-MEANS test, driven end to end through run(): NO amendment row exists
// at agent exit (so the pre-loop settle wait, fold and #2601 detection are
// all no-ops and the reinvoke is NOT withheld); the operator's approval lands
// DURING the fix re-invocation (the fix agent's onInvoke sets fu.amendments,
// same goroutine — the #1035 watcher is pinned and already stopped before the
// loop), and the fix agent edits the GRANTED file with the seed-init fix.
// Iteration 2 must re-commit WITH mod/other.go and pass.
//
// COUNTERFACTUAL (delete the refoldScopeAmendmentsMidLoop call in the loop):
// iteration 2 stages only mod/reg.go, verify fails identically, the run exits
// exitFailure category-A — RED.
func TestRun_ScopeAmendmentApprovedMidFixLoop_FoldedIntoRecommit(t *testing.T) {
	pinAmendmentWatchInterval(t)
	repo := verifyFixBaseRepo(t)
	baseSHA := gitHead(t, repo)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetBuggy)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	fu := newFakeUploader(t)
	fu.promptResp = undecidedVerifyFixPrompt() // VerifyMaxIterations 1
	// EMPTY at agent exit: nothing pending, nothing approved.
	fu.amendments = nil

	var fixPrompt string
	invoker := &fakeInvoker{
		mirrorWorkingTreeFrom: repo,
		canned:                agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
		onInvoke: func(idx int, inv agent.Invocation) {
			if idx == 1 {
				fixPrompt = inv.Prompt
				// The operator approves the amendment WHILE the fix agent runs...
				fu.amendments = []upload.ScopeAmendment{approvedGrantRow()}
				// ...and the fix agent edits the granted file.
				mustWrite(t, filepath.Join(repo, "mod", "other.go"),
					"package mod\n\nfunc init() { registry[\"x\"] = 42 }\n")
			}
		},
	}
	withFakeInvoker(t, invoker)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
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
	log := stderr.String()
	// The iteration-1 fix prompt carried NO grant (nothing was approved yet).
	if fixPrompt == "" || strings.Contains(fixPrompt, verifyFixGrantedMarker) {
		t.Errorf("iteration-1 fix prompt must exist and carry no GRANTED block (approval landed later):\n%s", fixPrompt)
	}
	// Ordering: reinvoke (iteration 1) → refold (iteration 1, added
	// [mod/other.go]) → iteration-2 verify_form_outcome.
	reinvoke := strings.Index(log, `"event":"verify_fix_reinvoke"`)
	refold := strings.Index(log, `"event":"verify_fix_scope_refolded"`)
	if reinvoke < 0 || refold < 0 || refold < reinvoke {
		t.Fatalf("expected verify_fix_reinvoke before verify_fix_scope_refolded (%d, %d):\n%s", reinvoke, refold, log)
	}
	refoldLine := log[refold-len(`{"`)+1:]
	refoldLine = refoldLine[strings.Index(refoldLine, "{"):]
	refoldLine = refoldLine[:strings.Index(refoldLine, "\n")]
	var refoldEv struct {
		Event     string   `json:"event"`
		RunID     string   `json:"run_id"`
		StageID   string   `json:"stage_id"`
		Iteration int      `json:"iteration"`
		Added     []string `json:"added"`
	}
	if err := json.Unmarshal([]byte(refoldLine), &refoldEv); err != nil {
		t.Fatalf("verify_fix_scope_refolded line is not JSON: %v (%q)", err, refoldLine)
	}
	if refoldEv.RunID != verifyFixRunID || refoldEv.StageID != verifyFixStageID || refoldEv.Iteration != 1 ||
		len(refoldEv.Added) != 1 || refoldEv.Added[0] != "mod/other.go" {
		t.Errorf("verify_fix_scope_refolded = %+v, want iteration 1 added [mod/other.go]", refoldEv)
	}
	iter2 := strings.Index(log[refold:], `"event":"verify_form_outcome","run_id":"`+verifyFixRunID+`","stage_id":"`+verifyFixStageID+`","iteration":2,`)
	if iter2 < 0 {
		t.Errorf("no iteration-2 verify_form_outcome after the refold — iteration 2 never ran:\n%s", log)
	}
	// The pushed scope carries the refolded path — the pointer write-back.
	if fp.gotArgs == nil {
		t.Fatal("CommitAndPush must run: the fix converged")
	}
	pushed := false
	for _, f := range fp.gotArgs.ScopeFiles {
		if f == "mod/other.go" {
			pushed = true
		}
	}
	if !pushed {
		t.Errorf("CommitAndPush.ScopeFiles = %v, want mod/other.go folded (pointer write-back)", fp.gotArgs.ScopeFiles)
	}
	// The bundle carries the fold record and NO unused-grant signal.
	events := readBundleEvents(t, bundlePath)
	if !hasFoldedEvent(events, "mod/other.go") {
		t.Error("bundle missing a scope_amendments_folded policy_event with added [mod/other.go]")
	}
	if hasPolicyEvent(events, "scope_amendment_grant_unused") || strings.Contains(log, "scope_amendment_grant_unused") {
		t.Errorf("a folded-and-used grant must emit no scope_amendment_grant_unused signal:\n%s", log)
	}
	assertVerifySummary(t, events, "passed", 2, 1)
	if invoker.callIdx != 2 {
		t.Errorf("Invoke call count = %d, want 2 (initial + 1 fix that converged)", invoker.callIdx)
	}
	if fpr.gotArgs == nil {
		t.Error("OpenPR must run after a passing loop")
	}
}

// TestRun_ScopeAmendmentsApprovedAcrossTwoFixIterations_BothFolded is the
// two-fold producer for approval condition 1: with VerifyMaxIterations 2,
// the initial agent leaves TWO out-of-scope files behind (so the pre-fold
// scope_drift names both), fix agent 1's invocation sees the FIRST approval
// land (mod/other.go — whose init calls a helper only mod/third.go defines,
// so iteration 2 still fails on `undefined: seed`) and fix agent 2's sees the
// SECOND (mod/third.go), after which iteration 3 passes. The bundle carries
// TWO scope_amendments_folded policy_events across two iterations.
//
// When FISHHAWK_REFOLD_FIXTURE_OUT names a path the REAL emitted bundle is
// copied there: that is how backend/internal/bundle's
// TestExtractScopeAmendmentsFolded_UnionsRealRunnerBundle fixture (a base64
// const) was produced — regenerate it with
//
//	FISHHAWK_REFOLD_FIXTURE_OUT=/tmp/refold.jsonl.gz go test ./runner/cmd/fishhawk-runner/ \
//	  -run TestRun_ScopeAmendmentsApprovedAcrossTwoFixIterations_BothFolded
//	base64 < /tmp/refold.jsonl.gz
func TestRun_ScopeAmendmentsApprovedAcrossTwoFixIterations_BothFolded(t *testing.T) {
	pinAmendmentWatchInterval(t)
	repo := verifyFixBaseRepo(t)
	baseSHA := gitHead(t, repo)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetBuggy)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)
	// Out-of-scope at agent exit → pre-fold scope_drift names both.
	mustWrite(t, filepath.Join(repo, "mod", "other.go"), "package mod\n\nfunc init() { registry[\"x\"] = seed() }\n")
	mustWrite(t, filepath.Join(repo, "mod", "third.go"), "package mod\n\nfunc seed() int { return 42 }\n")

	fu := newFakeUploader(t)
	fu.promptResp = undecidedVerifyFixPrompt()
	fu.promptResp.VerifyMaxIterations = 2
	fu.amendments = nil
	first := approvedGrantRow() // amd-e2e → mod/other.go
	second := upload.ScopeAmendment{
		ID: "amd-third", RunID: verifyFixRunID, StageID: verifyFixStageID, Status: "approved",
		Paths: []upload.ScopeAmendmentPath{{Path: "mod/third.go", Operation: "modify"}},
	}
	invoker := &fakeInvoker{
		mirrorWorkingTreeFrom: repo,
		canned:                agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
		onInvoke: func(idx int, _ agent.Invocation) {
			switch idx {
			case 1:
				fu.amendments = []upload.ScopeAmendment{first}
			case 2:
				fu.amendments = []upload.ScopeAmendment{first, second}
			}
		},
	}
	withFakeInvoker(t, invoker)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
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
	log := stderr.String()
	if n := strings.Count(log, `"event":"verify_fix_scope_refolded"`); n != 2 {
		t.Errorf("verify_fix_scope_refolded lines = %d, want 2:\n%s", n, log)
	}
	events := readBundleEvents(t, bundlePath)
	if !hasFoldedEvent(events, "mod/other.go") || !hasFoldedEvent(events, "mod/third.go") {
		t.Error("bundle must carry a scope_amendments_folded policy_event for EACH fold")
	}
	if n := countPolicyEvents(events, "scope_amendments_folded"); n != 2 {
		t.Errorf("scope_amendments_folded policy_events = %d, want 2", n)
	}
	// The pre-fold drift snapshot names both paths — the input the backend
	// subtracts the union from.
	drift := false
	for _, ev := range events {
		if ev.Kind == "policy_event" && strings.Contains(string(ev.Data), `"scope_drift"`) &&
			strings.Contains(string(ev.Data), `"mod/other.go"`) && strings.Contains(string(ev.Data), `"mod/third.go"`) {
			drift = true
		}
	}
	if !drift {
		t.Error("bundle's scope_drift policy_event must name both out-of-scope paths")
	}
	assertVerifySummary(t, events, "passed", 3, 2)
	if invoker.callIdx != 3 {
		t.Errorf("Invoke call count = %d, want 3 (initial + 2 fixes)", invoker.callIdx)
	}
	if fp.gotArgs == nil {
		t.Fatal("CommitAndPush must run: the fix converged")
	}
	got := strings.Join(fp.gotArgs.ScopeFiles, ",")
	if !strings.Contains(got, "mod/other.go") || !strings.Contains(got, "mod/third.go") {
		t.Errorf("CommitAndPush.ScopeFiles = %v, want both folded paths", fp.gotArgs.ScopeFiles)
	}
	if strings.Contains(log, "scope_amendment_grant_unused") {
		t.Errorf("both grants were folded and carried; no unused signal expected:\n%s", log)
	}
	if out := os.Getenv("FISHHAWK_REFOLD_FIXTURE_OUT"); out != "" {
		data, err := os.ReadFile(bundlePath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, data, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("real emitted bundle written to %s (%d bytes)", out, len(data))
	}
}

// hasFoldedEvent reports a scope_amendments_folded policy_event whose `added`
// carries path.
func hasFoldedEvent(events []bundle.Line, path string) bool {
	for _, ev := range events {
		if ev.Kind != "policy_event" {
			continue
		}
		var p struct {
			Check string   `json:"check"`
			Added []string `json:"added"`
		}
		if json.Unmarshal(ev.Data, &p) != nil || p.Check != "scope_amendments_folded" {
			continue
		}
		for _, a := range p.Added {
			if a == path {
				return true
			}
		}
	}
	return false
}

func countPolicyEvents(events []bundle.Line, check string) int {
	n := 0
	for _, ev := range events {
		if ev.Kind == "policy_event" && strings.Contains(string(ev.Data), `"check":"`+check+`"`) {
			n++
		}
	}
	return n
}

// TestRefoldScopeAmendmentsMidLoop_NoOpGuards: nil client / empty token /
// empty scope each return (false, nil) with an EMPTY log and ZERO fetches —
// which is exactly why every by-value runVerifyFixLoop test passing
// `&cfg, nil, ""` is unchanged. A fetch error logs
// scope_amendment_refresh_failed and returns false with cfg.scopeFiles
// unchanged. A fold that adds nothing (path already in scope) returns false
// with NO verify_fix_scope_refolded line. A fold that adds a path returns
// true, the fold event, and the one refold line.
func TestRefoldScopeAmendmentsMidLoop_NoOpGuards(t *testing.T) {
	base := config{runID: verifyFixRunID, stageID: verifyFixStageID, scopeFiles: grantScope()}

	t.Run("nil client", func(t *testing.T) {
		cfg := base
		var log bytes.Buffer
		added, evs := refoldScopeAmendmentsMidLoop(context.Background(), nil, &cfg, "tok", "implement", 1, &log)
		if added || evs != nil || log.Len() != 0 || len(cfg.scopeFiles) != len(base.scopeFiles) {
			t.Errorf("nil client: added=%v evs=%v log=%q", added, evs, log.String())
		}
	})
	t.Run("empty token", func(t *testing.T) {
		cfg := base
		fu := newFakeUploader(t)
		fu.amendments = []upload.ScopeAmendment{approvedGrantRow()}
		var log bytes.Buffer
		added, evs := refoldScopeAmendmentsMidLoop(context.Background(), fu, &cfg, "", "implement", 1, &log)
		if added || evs != nil || log.Len() != 0 || fu.amendmentCalls != 0 || len(cfg.scopeFiles) != len(base.scopeFiles) {
			t.Errorf("empty token: added=%v evs=%v calls=%d log=%q", added, evs, fu.amendmentCalls, log.String())
		}
	})
	t.Run("empty scope", func(t *testing.T) {
		cfg := base
		cfg.scopeFiles = nil
		fu := newFakeUploader(t)
		fu.amendments = []upload.ScopeAmendment{approvedGrantRow()}
		var log bytes.Buffer
		added, evs := refoldScopeAmendmentsMidLoop(context.Background(), fu, &cfg, "tok", "implement", 1, &log)
		if added || evs != nil || log.Len() != 0 || fu.amendmentCalls != 0 || cfg.scopeFiles != nil {
			t.Errorf("empty scope: added=%v evs=%v calls=%d log=%q scope=%v", added, evs, fu.amendmentCalls, log.String(), cfg.scopeFiles)
		}
	})
	t.Run("fetch error", func(t *testing.T) {
		cfg := base
		fu := newFakeUploader(t)
		fu.amendmentsErr = errors.New("backend 503")
		var log bytes.Buffer
		added, evs := refoldScopeAmendmentsMidLoop(context.Background(), fu, &cfg, "tok", "implement", 1, &log)
		if added || len(evs) != 0 {
			t.Errorf("fetch error: added=%v evs=%v", added, evs)
		}
		if !strings.Contains(log.String(), `"event":"scope_amendment_refresh_failed"`) {
			t.Errorf("missing scope_amendment_refresh_failed:\n%s", log.String())
		}
		if strings.Contains(log.String(), "verify_fix_scope_refolded") {
			t.Errorf("a failed fetch must not log a refold:\n%s", log.String())
		}
		if len(cfg.scopeFiles) != len(base.scopeFiles) {
			t.Errorf("scope changed on a failed fetch: %v", cfg.scopeFiles)
		}
	})
	t.Run("approved path already in scope -> no growth", func(t *testing.T) {
		cfg := base
		cfg.scopeFiles = append(cfg.scopeFiles, upload.ScopeFile{Path: "mod/other.go", Operation: "modify"})
		n := len(cfg.scopeFiles)
		fu := newFakeUploader(t)
		fu.amendments = []upload.ScopeAmendment{approvedGrantRow()}
		var log bytes.Buffer
		added, _ := refoldScopeAmendmentsMidLoop(context.Background(), fu, &cfg, "tok", "implement", 2, &log)
		if added || len(cfg.scopeFiles) != n || strings.Contains(log.String(), "verify_fix_scope_refolded") {
			t.Errorf("no-growth fold: added=%v scope=%v log=%q", added, cfg.scopeFiles, log.String())
		}
		// The grant is still recorded for the prompt / unused-grant check.
		if len(cfg.approvedAmendments) != 1 {
			t.Errorf("approvedAmendments = %+v, want the one approved row", cfg.approvedAmendments)
		}
	})
	t.Run("approved new path -> added, event, one refold line", func(t *testing.T) {
		cfg := base
		fu := newFakeUploader(t)
		fu.amendments = []upload.ScopeAmendment{approvedGrantRow()}
		var log bytes.Buffer
		added, evs := refoldScopeAmendmentsMidLoop(context.Background(), fu, &cfg, "tok", "implement", 2, &log)
		if !added {
			t.Fatalf("added = false, want true:\n%s", log.String())
		}
		if len(evs) != 1 || !strings.Contains(string(evs[0].Payload), `"scope_amendments_folded"`) {
			t.Errorf("evs = %+v, want the one scope_amendments_folded policy_event", evs)
		}
		if n := strings.Count(log.String(), `"event":"verify_fix_scope_refolded"`); n != 1 {
			t.Errorf("refold lines = %d, want 1:\n%s", n, log.String())
		}
		if !strings.Contains(log.String(), `"iteration":2,"added":["mod/other.go"]`) {
			t.Errorf("refold line must name the iteration and the added path:\n%s", log.String())
		}
		if last := cfg.scopeFiles[len(cfg.scopeFiles)-1]; last.Path != "mod/other.go" {
			t.Errorf("scope tail = %+v, want mod/other.go", last)
		}
	})
}
