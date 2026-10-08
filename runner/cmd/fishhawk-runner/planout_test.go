package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// Fixed ids the #4067 keyed-path literal pins share (runner, backend prompt and
// CLI tests assert the same literal for the same ids, the #1777 pattern).
const (
	planOutTestRunID   = "11111111-2222-3333-4444-555555555555"
	planOutTestStageID = "22222222-3333-4444-5555-666666666666"
)

// withPlanArtifactDirs redirects the keyed plan dir and the legacy fixed path
// into fresh temp dirs so no test touches the real /tmp. Returns (keyedDir,
// legacyPath).
func withPlanArtifactDirs(t *testing.T) (string, string) {
	t.Helper()
	keyedDir := t.TempDir()
	legacyPath := filepath.Join(t.TempDir(), "fishhawk-plan.json")
	origDir, origLegacy := planArtifactDir, legacyPlanArtifactPath
	planArtifactDir, legacyPlanArtifactPath = keyedDir, legacyPath
	t.Cleanup(func() { planArtifactDir, legacyPlanArtifactPath = origDir, origLegacy })
	return keyedDir, legacyPath
}

// planWithTicket returns a schema-valid standard_v1 plan whose
// ticket_reference names kuhlman-labs/fishhawk#n, so two stages' plans are
// distinguishable by their ticket.
func planWithTicket(t *testing.T, n int) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(validPlanJSON()), &doc); err != nil {
		t.Fatal(err)
	}
	doc["ticket_reference"] = map[string]any{
		"type": "github_issue",
		"url":  fmt.Sprintf("https://github.com/kuhlman-labs/fishhawk/issues/%d", n),
		"id":   fmt.Sprintf("kuhlman-labs/fishhawk#%d", n),
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFileOrEmpty(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// TestPlanArtifactPath_KeyedFormat pins the keyed plan handoff literal (#4067):
// distinct (run, stage) pairs yield distinct paths, and the literal is the one
// the backend prompt.PlanArtifactPath and the CLI mirror also pin.
func TestPlanArtifactPath_KeyedFormat(t *testing.T) {
	origDir := planArtifactDir
	planArtifactDir = "/tmp"
	t.Cleanup(func() { planArtifactDir = origDir })

	got := planArtifactPath(planOutTestRunID, planOutTestStageID)
	want := "/tmp/fishhawk-plan-11111111-2222-3333-4444-555555555555-22222222-3333-4444-5555-666666666666.json"
	if got != want {
		t.Errorf("keyed plan path = %q, want %q (must match backend + CLI literal)", got, want)
	}
	if other := planArtifactPath(planOutTestRunID, "33333333-4444-5555-6666-777777777777"); other == got {
		t.Errorf("distinct stage ids must yield distinct paths: %q == %q", other, got)
	}
	if got == legacyPlanArtifactPath {
		t.Errorf("keyed path must differ from the legacy fixed path %q", legacyPlanArtifactPath)
	}
}

// TestResolvePlanOut pins every resolvePlanOut arm. The missing-id rows pass the
// LEGACY path so they discriminate: without the id guard they would be keyed.
func TestResolvePlanOut(t *testing.T) {
	keyedDir, legacy := withPlanArtifactDirs(t)
	keyed := filepath.Join(keyedDir, "fishhawk-plan-"+planOutTestRunID+"-"+planOutTestStageID+".json")
	custom := filepath.Join(t.TempDir(), "plan.json")

	cases := []struct {
		name       string
		runID      string
		stageID    string
		planOut    string
		wantOut    string
		wantLegacy string
		wantLog    bool
	}{
		{"legacy rewritten to keyed and armed", planOutTestRunID, planOutTestStageID, legacy, keyed, legacy, true},
		{"keyed kept and armed", planOutTestRunID, planOutTestStageID, keyed, keyed, legacy, false},
		{"custom path unchanged and unarmed", planOutTestRunID, planOutTestStageID, custom, custom, "", false},
		{"missing stage id leaves legacy", planOutTestRunID, "", legacy, legacy, "", false},
		{"missing run id leaves legacy", "", planOutTestStageID, legacy, legacy, "", false},
		{"empty plan-out stays empty", planOutTestRunID, planOutTestStageID, "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config{runID: tc.runID, stageID: tc.stageID, planOut: tc.planOut}
			var log strings.Builder
			resolvePlanOut(&cfg, &log)
			if cfg.planOut != tc.wantOut {
				t.Errorf("planOut = %q, want %q", cfg.planOut, tc.wantOut)
			}
			if cfg.planOutLegacy != tc.wantLegacy {
				t.Errorf("planOutLegacy = %q, want %q", cfg.planOutLegacy, tc.wantLegacy)
			}
			gotLog := strings.Contains(log.String(), `"event":"plan_out_keyed"`)
			if gotLog != tc.wantLog {
				t.Errorf("plan_out_keyed logged = %t, want %t:\n%s", gotLog, tc.wantLog, log.String())
			}
			if tc.wantLog && !strings.Contains(log.String(), `"to":"`+keyed+`"`) {
				t.Errorf("plan_out_keyed must name the keyed path:\n%s", log.String())
			}
		})
	}
}

// twoLegacyPlanStages builds two configs that were both handed the legacy fixed
// --plan-out (the GHA/GitLab shape) with distinct run/stage ids, and resolves
// each.
func twoLegacyPlanStages(t *testing.T) (config, config) {
	t.Helper()
	_, legacy := withPlanArtifactDirs(t)
	a := config{runID: planOutTestRunID, stageID: planOutTestStageID, planOut: legacy, backendURL: "https://api.fishhawk.test"}
	b := config{runID: "aaaaaaaa-2222-3333-4444-555555555555", stageID: "bbbbbbbb-3333-4444-5555-666666666666", planOut: legacy, backendURL: "https://api.fishhawk.test"}
	var log strings.Builder
	resolvePlanOut(&a, &log)
	resolvePlanOut(&b, &log)
	return a, b
}

// TestPlanOut_ConcurrentStagesUploadOwnBytes replays, deterministically, the
// interleaving the shared fixed path loses (#4067): A's agent writes, B's agent
// writes, A adopts, B adopts, then each uploads. With the paths keyed each stage
// ships its own ticket; on one shared path B's adoption overwrites A's file
// before A's uploadPlan re-reads it, so A ships #B's bytes.
func TestPlanOut_ConcurrentStagesUploadOwnBytes(t *testing.T) {
	a, b := twoLegacyPlanStages(t)
	if a.planOut == b.planOut {
		t.Errorf("two plan stages share one plan-out path %q", a.planOut)
	}
	planA, planB := planWithTicket(t, 4012), planWithTicket(t, 4013)

	mustWriteFile(t, a.planOut, planA) // A's agent writes
	mustWriteFile(t, b.planOut, planB) // B's agent writes
	_ = adoptStructuredOutput(a.planOut, []byte(planA))
	_ = adoptStructuredOutput(b.planOut, []byte(planB))

	fuA, fuB := newFakeUploader(t), newFakeUploader(t)
	var log strings.Builder
	if err := uploadPlan(context.Background(), a, &log, fuA, &upload.IssuedKey{RunID: a.runID, PrivateKey: fuA.priv}); err != nil {
		t.Fatalf("uploadPlan(A): %v", err)
	}
	if err := uploadPlan(context.Background(), b, &log, fuB, &upload.IssuedKey{RunID: b.runID, PrivateKey: fuB.priv}); err != nil {
		t.Fatalf("uploadPlan(B): %v", err)
	}
	if fuA.gotPlanArgs == nil || fuB.gotPlanArgs == nil {
		t.Fatal("ShipPlan not called for both stages")
	}
	if got := string(fuA.gotPlanArgs.Plan); got != planA {
		t.Errorf("stage A shipped a foreign plan (want its own #4012):\n%s", got)
	}
	if got := string(fuB.gotPlanArgs.Plan); got != planB {
		t.Errorf("stage B shipped a foreign plan (want its own #4013):\n%s", got)
	}
}

// TestPlanOut_ForeignSiblingNotAdopted: a concurrent stage parking with a
// clarification_request must not make ANOTHER stage's plan look like a sibling
// (the precedence lets any recognized sibling win, #4067 mechanism 1).
func TestPlanOut_ForeignSiblingNotAdopted(t *testing.T) {
	a, b := twoLegacyPlanStages(t)
	mustWriteFile(t, a.planOut, planWithTicket(t, 4012))
	mustWriteFile(t, b.planOut, validClarificationJSON())

	if kind, isSibling, _ := detectPlanSibling(a.planOut); isSibling {
		t.Errorf("stage A's plan-out reports a foreign sibling %q", kind)
	}
	if kind, isSibling, _ := detectPlanSibling(b.planOut); !isSibling || kind != "clarification_request" {
		t.Errorf("stage B's own clarification not detected: kind=%q sibling=%t", kind, isSibling)
	}
}

// armedLegacyClaim returns a config armed for the legacy fallback, its legacy
// path and an invokedAt a minute in the past.
func armedLegacyClaim(t *testing.T) (config, string, time.Time) {
	t.Helper()
	_, legacy := withPlanArtifactDirs(t)
	cfg := config{runID: planOutTestRunID, stageID: planOutTestStageID, planOut: legacy}
	resolvePlanOut(&cfg, &strings.Builder{})
	if cfg.planOutLegacy == "" {
		t.Fatal("fixture: legacy fallback not armed")
	}
	return cfg, legacy, time.Now().Add(-time.Minute)
}

// oldPrompt is a prompt rendered by a pre-#4067 backend: it names only the
// legacy fixed path.
func oldPrompt(legacy string) string {
	return "Write your plan JSON to " + legacy + " and stop."
}

// (a) armed, keyed absent, fresh legacy: claimed by rename.
func TestClaimLegacyPlanArtifact_ClaimsFreshLegacy(t *testing.T) {
	cfg, legacy, invokedAt := armedLegacyClaim(t)
	plan := planWithTicket(t, 4012)
	mustWriteFile(t, legacy, plan)

	var log strings.Builder
	claimLegacyPlanArtifact(cfg, oldPrompt(legacy), invokedAt, &log)

	if got := readFileOrEmpty(cfg.planOut); got != plan {
		t.Errorf("keyed file = %q, want the claimed legacy plan", got)
	}
	if pathExists(legacy) {
		t.Error("legacy file still present: the claim must CONSUME it (rename), not copy it")
	}
	if !strings.Contains(log.String(), `"event":"plan_artifact_legacy_path"`) {
		t.Errorf("missing plan_artifact_legacy_path:\n%s", log.String())
	}
}

// (b) the prompt names the keyed path (up-to-date backend): the fixed path is
// never read, even when a fresh file sits there.
func TestClaimLegacyPlanArtifact_KeyedPromptNotArmed(t *testing.T) {
	cfg, legacy, invokedAt := armedLegacyClaim(t)
	foreign := planWithTicket(t, 4013)
	mustWriteFile(t, legacy, foreign)

	var log strings.Builder
	claimLegacyPlanArtifact(cfg, "Write your plan JSON to "+cfg.planOut+" and stop.", invokedAt, &log)

	if pathExists(cfg.planOut) {
		t.Errorf("keyed file created from the legacy path although the prompt names the keyed path")
	}
	if got := readFileOrEmpty(legacy); got != foreign {
		t.Error("legacy file was consumed although the prompt names the keyed path")
	}
}

// (c) a legacy file older than the invocation is a stale leftover: not claimed.
func TestClaimLegacyPlanArtifact_StaleNotClaimed(t *testing.T) {
	cfg, legacy, _ := armedLegacyClaim(t)
	mustWriteFile(t, legacy, planWithTicket(t, 4013))
	invokedAt := time.Now()
	old := invokedAt.Add(-time.Hour)
	if err := os.Chtimes(legacy, old, old); err != nil {
		t.Fatal(err)
	}

	var log strings.Builder
	claimLegacyPlanArtifact(cfg, oldPrompt(legacy), invokedAt, &log)

	if pathExists(cfg.planOut) {
		t.Error("stale legacy file was claimed onto the keyed path")
	}
	if !pathExists(legacy) {
		t.Error("stale legacy file was consumed")
	}
	if !strings.Contains(log.String(), `"event":"plan_artifact_legacy_stale"`) {
		t.Errorf("missing plan_artifact_legacy_stale:\n%s", log.String())
	}
}

// (d) the agent already wrote the keyed file: the legacy file is left alone and
// the keyed bytes survive.
func TestClaimLegacyPlanArtifact_KeyedPresentWins(t *testing.T) {
	cfg, legacy, invokedAt := armedLegacyClaim(t)
	own, foreign := planWithTicket(t, 4012), planWithTicket(t, 4013)
	mustWriteFile(t, cfg.planOut, own)
	mustWriteFile(t, legacy, foreign)

	claimLegacyPlanArtifact(cfg, oldPrompt(legacy), invokedAt, &strings.Builder{})

	if got := readFileOrEmpty(cfg.planOut); got != own {
		t.Errorf("keyed file overwritten by the legacy file:\n%s", got)
	}
	if got := readFileOrEmpty(legacy); got != foreign {
		t.Error("legacy file consumed although the keyed file already existed")
	}
}

// (e1) two claimants for one legacy file, in sequence: exactly one ends with
// the bytes.
func TestClaimLegacyPlanArtifact_SingleClaimant(t *testing.T) {
	cfg, legacy, invokedAt := armedLegacyClaim(t)
	other := cfg
	other.runID, other.stageID = "aaaaaaaa-2222-3333-4444-555555555555", "bbbbbbbb-3333-4444-5555-666666666666"
	other.planOut = planArtifactPath(other.runID, other.stageID)
	plan := planWithTicket(t, 4012)
	mustWriteFile(t, legacy, plan)

	claimLegacyPlanArtifact(cfg, oldPrompt(legacy), invokedAt, &strings.Builder{})
	claimLegacyPlanArtifact(other, oldPrompt(legacy), invokedAt, &strings.Builder{})

	if got := readFileOrEmpty(cfg.planOut); got != plan {
		t.Errorf("first claimant keyed file = %q, want the plan", got)
	}
	if pathExists(other.planOut) {
		t.Error("second claimant ALSO received the legacy plan: the claim must be consuming")
	}
}

// (e2) many claimants racing for one legacy file: exactly one wins, and a lost
// race (rename ENOENT) is silent rather than logged as unclaimable.
func TestClaimLegacyPlanArtifact_ConcurrentClaimantsOneWinner(t *testing.T) {
	cfg, legacy, invokedAt := armedLegacyClaim(t)
	const rounds, claimants = 25, 8
	for r := 0; r < rounds; r++ {
		mustWriteFile(t, legacy, fmt.Sprintf(`{"round":%d}`, r))
		cfgs := make([]config, claimants)
		for i := range cfgs {
			c := cfg
			c.stageID = fmt.Sprintf("%08d-0000-0000-0000-%012d", r, i)
			c.planOut = planArtifactPath(c.runID, c.stageID)
			cfgs[i] = c
		}
		var sb strings.Builder
		log := newSyncWriter(&sb)
		var start, done sync.WaitGroup
		start.Add(1)
		for _, c := range cfgs {
			done.Add(1)
			go func(c config) {
				defer done.Done()
				start.Wait()
				claimLegacyPlanArtifact(c, oldPrompt(legacy), invokedAt, log)
			}(c)
		}
		start.Done()
		done.Wait()

		winners := 0
		for _, c := range cfgs {
			if pathExists(c.planOut) {
				winners++
			}
		}
		if winners != 1 {
			t.Fatalf("round %d: %d claimants hold the legacy plan, want exactly 1", r, winners)
		}
		if strings.Contains(sb.String(), "plan_artifact_legacy_unclaimable") {
			t.Fatalf("round %d: a lost claim race was logged as unclaimable:\n%s", r, sb.String())
		}
	}
}

// (f) a symlink at the legacy path is refused, never followed or moved.
func TestClaimLegacyPlanArtifact_SymlinkRefused(t *testing.T) {
	cfg, legacy, invokedAt := armedLegacyClaim(t)
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	mustWriteFile(t, target, planWithTicket(t, 4013))
	if err := os.Symlink(target, legacy); err != nil {
		t.Fatal(err)
	}

	var log strings.Builder
	claimLegacyPlanArtifact(cfg, oldPrompt(legacy), invokedAt, &log)

	if pathExists(cfg.planOut) {
		t.Error("a symlinked legacy path was claimed onto the keyed path")
	}
	if fi, err := os.Lstat(legacy); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("legacy symlink not left in place: %v", err)
	}
	if !strings.Contains(log.String(), `"reason":"not_regular_file"`) {
		t.Errorf("missing plan_artifact_legacy_unclaimable not_regular_file:\n%s", log.String())
	}
}

// (g) a rename failure other than ENOENT (here EACCES: the keyed dir is
// read-only) is logged as unclaimable, the legacy file is left and no success
// is claimed — the plan is then missing (category B), never silently copied.
func TestClaimLegacyPlanArtifact_RenameErrorUnclaimable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permission")
	}
	cfg, legacy, invokedAt := armedLegacyClaim(t)
	mustWriteFile(t, legacy, planWithTicket(t, 4012))
	if err := os.Chmod(planArtifactDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(planArtifactDir, 0o700) })

	var log strings.Builder
	claimLegacyPlanArtifact(cfg, oldPrompt(legacy), invokedAt, &log)

	if !strings.Contains(log.String(), `"event":"plan_artifact_legacy_unclaimable"`) {
		t.Errorf("missing plan_artifact_legacy_unclaimable:\n%s", log.String())
	}
	if strings.Contains(log.String(), `"event":"plan_artifact_legacy_path"`) {
		t.Errorf("a failed rename was reported as a successful claim:\n%s", log.String())
	}
	if !pathExists(legacy) {
		t.Error("legacy file lost on a failed claim")
	}
}

// (h) armed but no legacy file at all: a silent no-op.
func TestClaimLegacyPlanArtifact_LegacyAbsentNoop(t *testing.T) {
	cfg, legacy, invokedAt := armedLegacyClaim(t)
	var log strings.Builder
	claimLegacyPlanArtifact(cfg, oldPrompt(legacy), invokedAt, &log)
	if pathExists(cfg.planOut) || log.Len() != 0 {
		t.Errorf("no-op expected: keyed exists=%t log=%q", pathExists(cfg.planOut), log.String())
	}
}

// (i) a custom --plan-out never arms the fallback.
func TestClaimLegacyPlanArtifact_UnarmedNoop(t *testing.T) {
	_, legacy := withPlanArtifactDirs(t)
	custom := filepath.Join(t.TempDir(), "plan.json")
	cfg := config{runID: planOutTestRunID, stageID: planOutTestStageID, planOut: custom}
	resolvePlanOut(&cfg, &strings.Builder{})
	mustWriteFile(t, legacy, planWithTicket(t, 4013))

	claimLegacyPlanArtifact(cfg, oldPrompt(legacy), time.Now().Add(-time.Minute), &strings.Builder{})

	if pathExists(custom) || !pathExists(legacy) {
		t.Errorf("unarmed config claimed the legacy file: custom exists=%t legacy exists=%t",
			pathExists(custom), pathExists(legacy))
	}
}

// planStageRunArgs is the run() argv for a plan stage handed the legacy fixed
// --plan-out with run/stage ids and --upload-trace (the GHA/GitLab shape).
func planStageRunArgs(promptPath, legacy string) []string {
	return []string{
		"--run-id", planOutTestRunID, "--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "plan",
		"--prompt-file", promptPath,
		"--plan-out", legacy,
		"--upload-trace",
		"--stage-id", planOutTestStageID,
	}
}

// TestRun_PlanStage_LegacyPlanOutResolvesToKeyed drives run() end to end with
// the legacy --plan-out while the shared fixed path holds a FOREIGN
// clarification_request (another run parking). The prompt names the keyed path
// (an up-to-date backend). The stage must ship its OWN structured-output plan,
// the keyed file must hold it, and the foreign legacy file must be untouched.
func TestRun_PlanStage_LegacyPlanOutResolvesToKeyed(t *testing.T) {
	_, legacy := withPlanArtifactDirs(t)
	keyed := planArtifactPath(planOutTestRunID, planOutTestStageID)
	foreign := validClarificationJSON()
	mustWriteFile(t, legacy, foreign)
	promptPath := filepath.Join(t.TempDir(), "prompt.txt")
	mustWriteFile(t, promptPath, "Write your plan JSON to "+keyed+" and stop.")

	own := planWithTicket(t, 4012)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true, StructuredOutput: []byte(own)}})
	fu := newFakeUploader(t)
	withFakeUploader(t, fu)

	var stderr strings.Builder
	if got := run(planStageRunArgs(promptPath, legacy), &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fu.gotPlanArgs == nil {
		t.Fatalf("ShipPlan not called:\n%s", stderr.String())
	}
	if got := string(fu.gotPlanArgs.Plan); got != own {
		t.Errorf("shipped a foreign artifact instead of the run's own plan:\n%s", got)
	}
	if got := readFileOrEmpty(keyed); got != own {
		t.Errorf("keyed file = %q, want the run's own plan", got)
	}
	if got := readFileOrEmpty(legacy); got != foreign {
		t.Errorf("foreign legacy file was touched: %q", got)
	}
	if !strings.Contains(stderr.String(), `"event":"plan_out_keyed"`) {
		t.Errorf("missing plan_out_keyed:\n%s", stderr.String())
	}
}

// TestRun_PlanStage_OldPromptClaimsFreshLegacyArtifact (binding condition 1):
// backend/runner version skew. The prompt names ONLY the legacy path, the agent
// returns no structured output and writes its plan to the LEGACY path during the
// invocation. The runner must claim it onto the keyed path and ship it.
func TestRun_PlanStage_OldPromptClaimsFreshLegacyArtifact(t *testing.T) {
	_, legacy := withPlanArtifactDirs(t)
	keyed := planArtifactPath(planOutTestRunID, planOutTestStageID)
	promptPath := filepath.Join(t.TempDir(), "prompt.txt")
	mustWriteFile(t, promptPath, oldPrompt(legacy))

	own := planWithTicket(t, 4012)
	withFakeInvoker(t, &fakeInvoker{
		canned: agent.Result{OK: true},
		onInvoke: func(int, agent.Invocation) {
			mustWriteFile(t, legacy, own)
		},
	})
	fu := newFakeUploader(t)
	withFakeUploader(t, fu)

	var stderr strings.Builder
	if got := run(planStageRunArgs(promptPath, legacy), &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fu.gotPlanArgs == nil {
		t.Fatalf("ShipPlan not called — the legacy plan was not claimed:\n%s", stderr.String())
	}
	if got := string(fu.gotPlanArgs.Plan); got != own {
		t.Errorf("shipped bytes = %q, want the claimed legacy plan", got)
	}
	if got := readFileOrEmpty(keyed); got != own {
		t.Errorf("keyed file = %q, want the claimed legacy plan", got)
	}
	if !strings.Contains(stderr.String(), `"event":"plan_artifact_legacy_path"`) {
		t.Errorf("missing plan_artifact_legacy_path:\n%s", stderr.String())
	}
}
