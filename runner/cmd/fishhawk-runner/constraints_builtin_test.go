package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/constraint"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// writeConstraintsFile writes body as a constraints file in a TempDir.
func writeConstraintsFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "constraints.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// agentViolationFiles decodes one policy_event payload and returns the files
// of an agent_instruction_paths violation, or ok=false for any other event.
// An undecodable payload fails the test rather than being skipped.
func agentViolationFiles(t *testing.T, payload []byte) (files []string, ok bool) {
	t.Helper()
	var p struct {
		Outcome    string   `json:"outcome"`
		Constraint string   `json:"constraint"`
		Files      []string `json:"files"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("decode policy_event payload %s: %v", payload, err)
	}
	if p.Outcome == "violation" && p.Constraint == "agent_instruction_paths" {
		return p.Files, true
	}
	return nil, false
}

// agentPolicyViolations decodes the policy_event violations enforceConstraints
// emitted for the agent_instruction_paths constraint.
func agentPolicyViolations(t *testing.T, evs []agent.Event) [][]string {
	t.Helper()
	var out [][]string
	for _, ev := range evs {
		if ev.Kind != "policy_event" {
			continue
		}
		if files, ok := agentViolationFiles(t, ev.Payload); ok {
			out = append(out, files)
		}
	}
	return out
}

// TestEnforceConstraints_BuiltinAgentInstructionPaths pins the runner's
// in-line default (#3942) end to end, from a constraints file through
// enforceConstraints to the policy_event it emits. The constraints file
// declares NOTHING ({}) in every arm but the opt-out, so the built-in is the
// only thing that can produce a violation, and the default is keyed on the
// RESOLVED stageType argument, never cfg.stage. It calls enforceConstraints
// directly with a stageType it chooses, so it cannot see what run() passes:
// TestRun_BuiltinAgentInstructionPaths_ResolvedStageTypeAtCallSite pins that
// call site.
func TestEnforceConstraints_BuiltinAgentInstructionPaths(t *testing.T) {
	const nested = "backend/.Claude/skills/x/SKILL.md"
	d := constraint.Diff{ChangedFiles: []constraint.ChangedFile{
		{Path: "main.go", Status: constraint.StatusModified},
		{Path: nested, Status: constraint.StatusAdded},
	}}
	empty := writeConstraintsFile(t, `{}`)
	optOut := writeConstraintsFile(t, `{"agent_instruction_paths":{"enforce":false}}`)

	for _, tc := range []struct {
		name      string
		file      string
		cfgStage  string
		stageType string
		wantHit   bool
	}{
		// (a) a resolved implement stage gets the default.
		{"resolved implement stage is enforced", empty, "implement", "implement", true},
		// (b) cfg.stage alone never applies it: an unresolved stageType (a
		// local replay without --fetch-prompt) gets no default.
		{"cfg.stage implement without a resolved type is not", empty, "implement", "", false},
		// (c) the resolved type wins over a mislabelled --stage flag.
		{"resolved implement wins over cfg.stage build", empty, "build", "implement", true},
		// (d) an explicit opt-out in the constraints file is honored.
		{"enforce false opts out", optOut, "implement", "implement", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config{constraintsFile: tc.file, stage: tc.cfgStage}
			evs, err := enforceConstraints(cfg, tc.stageType, d)
			hits := agentPolicyViolations(t, evs)
			if !tc.wantHit {
				if err != nil || len(hits) != 0 {
					t.Fatalf("err = %v, agent_instruction_paths violations = %v; want neither", err, hits)
				}
				return
			}
			if err == nil {
				t.Fatal("err = nil, want a constraint-violation error (the caller demotes to category B)")
			}
			if len(hits) != 1 || !slices.Equal(hits[0], []string{nested}) {
				t.Fatalf("agent_instruction_paths violations = %v, want exactly one naming %s", hits, nested)
			}
		})
	}
}

// initMainRepo creates a throwaway git repo on branch main with one seed
// commit. The branch name is pinned (not left to host init.defaultBranch)
// because both the lineage-worktree seed and --check-base-ref main need it.
// Skips the test when git is unavailable.
func initMainRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-q", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "seed")
	return dir
}

// TestRun_BuiltinAgentInstructionPaths_ResolvedStageTypeAtCallSite (#4310)
// pins the stageType argument run() passes to enforceConstraints. The unit
// test above chooses its own stageType, so replacing the call-site argument
// with "" or cfg.stage stays green there and silently disables the #3942
// built-in on the real path.
//
// This drives run() end to end: --fetch-prompt resolves stage_type
// "implement" from the backend while --stage is deliberately "build", so the
// resolved type is the ONLY source of "implement" on the path (a cfg.stage
// argument therefore also reddens arm a). The fake agent writes into
// inv.WorkingDir, the relocated lineage worktree (#1137), and the prompt
// serves no ScopeFiles so computeAndEmitDiff takes its `git add -A` fallback
// and the nested file reaches the diff instead of being drift-excluded.
//
//   - a: {} constraints + nested backend/CLAUDE.md: category B, one violation.
//   - b: {} constraints + only main.go: exits OK (positive control, so arm a's
//     failure is attributable to the built-in alone).
//   - c: enforce:false + nested backend/CLAUDE.md: exits OK (opt-out control,
//     so the violation comes from the ApplyImplementDefaults default).
//
// Only push/PR egress is swapped (as basepin_test.go does); every git seam
// stays production. No t.Parallel: the helpers swap package globals.
func TestRun_BuiltinAgentInstructionPaths_ResolvedStageTypeAtCallSite(t *testing.T) {
	const (
		runID   = "11111111-2222-3333-4444-555555555555"
		stageID = "22222222-3333-4444-5555-666666666666"
		nested  = "backend/CLAUDE.md"
	)
	for _, tc := range []struct {
		name        string
		constraints string
		files       []string
		wantOK      bool
	}{
		{"a: default enforces nested CLAUDE.md", `{}`, []string{nested, "main.go"}, false},
		{"b: ordinary file only exits OK", `{}`, []string{"main.go"}, true},
		{"c: enforce false opts out", `{"agent_instruction_paths":{"enforce":false}}`, []string{nested, "main.go"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initMainRepo(t)
			constraintsFile := writeConstraintsFile(t, tc.constraints)
			bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")

			withFakeInvoker(t, &fakeInvoker{
				canned: agent.Result{OK: true},
				onInvoke: func(_ int, inv agent.Invocation) {
					for _, f := range tc.files {
						p := filepath.Join(inv.WorkingDir, filepath.FromSlash(f))
						if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
							t.Error(err)
							return
						}
						if err := os.WriteFile(p, []byte("agent edit\n"), 0o644); err != nil {
							t.Error(err)
							return
						}
					}
				},
			})
			fu := newFakeUploader(t)
			fu.promptResp = &upload.FetchedPrompt{
				StageID:    stageID,
				StageType:  "implement",
				Prompt:     "implement",
				PromptHash: "h",
			}
			withFakeUploader(t, fu)

			// Only the push/PR egress is swapped; every git seam stays production.
			origPusher, origOpener := newPusher, newPROpener
			newPusher = func() pusher { return &fakePusher{} }
			newPROpener = func(string) prOpener { return &fakePROpener{} }
			t.Cleanup(func() { newPusher = origPusher; newPROpener = origOpener })

			var stderr strings.Builder
			got := run([]string{
				"--run-id", runID,
				"--backend-url", "https://api.fishhawk.test",
				"--workflow", "feature_change",
				"--stage", "build",
				"--stage-id", stageID,
				"--working-dir", repo,
				"--fetch-prompt", "--upload-trace",
				"--bundle-out", bundlePath,
				"--check-base-ref", "main",
				"--constraints-file", constraintsFile,
				"--no-pr",
			}, &stderr)

			var hits [][]string
			for _, ev := range bundleEventsForTest(t, bundlePath) {
				if ev.Kind != "policy_event" {
					continue
				}
				if files, ok := agentViolationFiles(t, ev.Data); ok {
					hits = append(hits, files)
				}
			}
			if tc.wantOK {
				if len(hits) != 0 {
					t.Errorf("agent_instruction_paths violations = %v, want none\nstderr:\n%s", hits, stderr.String())
				}
			} else {
				if len(hits) != 1 || !slices.Equal(hits[0], []string{nested}) {
					t.Errorf("agent_instruction_paths violations = %v, want exactly one naming only %s\nstderr:\n%s", hits, nested, stderr.String())
				}
				if !strings.Contains(stderr.String(), `"category":"B"`) {
					t.Errorf("result line missing category B\nstderr:\n%s", stderr.String())
				}
			}
			wantExit := exitFailure
			if tc.wantOK {
				wantExit = exitOK
			}
			if got != wantExit {
				t.Fatalf("run = %d, want %d\nstderr:\n%s", got, wantExit, stderr.String())
			}
		})
	}
}
