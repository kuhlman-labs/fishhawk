package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// consolidatedFanInFiles are the three child files the fan-in committed onto
// the consolidated branch BETWEEN main and its tip. A diff measured from
// anything other than the tip counts them — that is the fixture state every
// #4391 counterfactual below relies on.
var consolidatedFanInFiles = []string{"pkg/a/a.go", "pkg/b/b.go", "pkg/c/c.go"}

// fixupIncrementFile is the ONE file the decomposed parent's fix-up adds.
const fixupIncrementFile = "pkg/fixup/fixup.go"

// initConsolidatedRepo builds a real repo whose `main` holds one seed commit
// and whose `consolidated` branch carries consolidatedFanInFiles in one
// further commit (the decomposed parent's fan-in). It leaves the checkout ON
// main and returns the repo path, main's commit and the consolidated tip.
func initConsolidatedRepo(t *testing.T) (repo, mainSHA, tipSHA string) {
	t.Helper()
	repo = initMainRepo(t)
	git := gitRunnerFor(t, repo)
	git("config", "user.name", "t")
	git("config", "user.email", "t@e")
	git("config", "commit.gpgsign", "false")
	mainSHA = revParse(t, repo, "HEAD")
	git("checkout", "-q", "-b", "consolidated")
	for _, f := range consolidatedFanInFiles {
		writeRepoFile(t, repo, f)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "fan-in")
	tipSHA = revParse(t, repo, "HEAD")
	git("checkout", "-q", "main")
	return repo, mainSHA, tipSHA
}

func revParse(t *testing.T, repo, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

func writeRepoFile(t *testing.T, dir, rel string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func changedPaths(d constraint.Diff) []string {
	out := make([]string, 0, len(d.ChangedFiles))
	for _, f := range d.ChangedFiles {
		out = append(out, f.Path)
	}
	slices.Sort(out)
	return out
}

// TestResolvePolicyBaseRef_DecomposedParentFixup (R1): a recorded increment
// base is returned FIRST as the git_diff base_ref label and logged; without
// it the standalone answer (checkBaseRef) stands with no such line.
func TestResolvePolicyBaseRef_DecomposedParentFixup(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	var log strings.Builder
	got := resolvePolicyBaseRef(context.Background(),
		config{checkBaseRef: "main", fixupIncrementBaseSHA: sha, stageID: "s"}, &log)
	if got != sha {
		t.Errorf("resolvePolicyBaseRef = %q, want the increment base %q", got, sha)
	}
	for _, want := range []string{`"event":"policy_base_decomposed_parent_fixup"`, `"base_ref":"` + sha + `"`, `"cumulative_base_ref":"main"`} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log missing %s:\n%s", want, log.String())
		}
	}

	log.Reset()
	if got := resolvePolicyBaseRef(context.Background(), config{checkBaseRef: "main", stageID: "s"}, &log); got != "main" {
		t.Errorf("without an increment base: resolvePolicyBaseRef = %q, want main", got)
	}
	if strings.Contains(log.String(), "policy_base_decomposed_parent_fixup") {
		t.Errorf("without an increment base the parent-fixup line fired:\n%s", log.String())
	}
}

// TestComputeAndEmitDiff_DecomposedParentFixup_MeasuresIncrement (R2): the
// checkout sits on the consolidated tip (3 committed fan-in files over main)
// with the fix-up's ONE file staged.
//
//   - A: fixupIncrementBaseSHA = the tip → exactly the one file, under a cap
//     of 2, labelled with the bare tip SHA, via diff_base_fixup_increment.
//   - B: unset → the cumulative chain (no remote → local merge-base(main,
//     HEAD) = main) → 4 files and a max_files_changed violation.
//
// MECHANISM: the 3 fan-in commits sit between main and the tip, so ONLY a
// diff measured from the tip excludes them; deleting resolveStageDiffBase's
// increment arm makes A measure from merge-base(main) and report 4 files.
func TestComputeAndEmitDiff_DecomposedParentFixup_MeasuresIncrement(t *testing.T) {
	repo, _, tipSHA := initConsolidatedRepo(t)
	gitRunnerFor(t, repo)("checkout", "-q", "consolidated")
	writeRepoFile(t, repo, fixupIncrementFile)
	maxFiles := constraint.Constraints{MaxFilesChanged: 2}

	t.Run("A increment base", func(t *testing.T) {
		var log strings.Builder
		cfg := config{workingDir: repo, checkBaseRef: "main", stageID: "s-parent", fixupIncrementBaseSHA: tipSHA}
		d, evs, err := computeAndEmitDiff(cfg, &log)
		if err != nil {
			t.Fatalf("computeAndEmitDiff: %v\n%s", err, log.String())
		}
		if got := changedPaths(d); !slices.Equal(got, []string{fixupIncrementFile}) {
			t.Fatalf("increment diff = %v, want only the fix-up's file %q\n%s", got, fixupIncrementFile, log.String())
		}
		if v := constraint.Evaluate(d, maxFiles); len(v) != 0 {
			t.Errorf("increment under max_files_changed 2 violated: %v", v)
		}
		if !strings.Contains(log.String(), `"event":"diff_base_fixup_increment"`) ||
			!strings.Contains(log.String(), `"merge_base":"`+tipSHA+`"`) {
			t.Errorf("missing diff_base_fixup_increment naming the tip:\n%s", log.String())
		}
		if got := gitDiffBaseRef(t, evs); got != tipSHA {
			t.Errorf("git_diff base_ref = %q, want the bare tip SHA %q", got, tipSHA)
		}
	})

	t.Run("B cumulative without the base", func(t *testing.T) {
		var log strings.Builder
		cfg := config{workingDir: repo, checkBaseRef: "main", stageID: "s-parent"}
		d, _, err := computeAndEmitDiff(cfg, &log)
		if err != nil {
			t.Fatalf("computeAndEmitDiff: %v\n%s", err, log.String())
		}
		want := append(slices.Clone(consolidatedFanInFiles), fixupIncrementFile)
		slices.Sort(want)
		if got := changedPaths(d); !slices.Equal(got, want) {
			t.Fatalf("cumulative diff = %v, want %v", got, want)
		}
		v := constraint.Evaluate(d, maxFiles)
		if len(v) != 1 || v[0].Constraint != "max_files_changed" {
			t.Errorf("cumulative 4 files under cap 2: violations = %v, want one max_files_changed", v)
		}
	})
}

// gitDiffBaseRef returns the base_ref label of the git_diff event in evs.
func gitDiffBaseRef(t *testing.T, evs []agent.Event) string {
	t.Helper()
	for _, ev := range evs {
		if ev.Kind != "git_diff" {
			continue
		}
		var p struct {
			BaseRef string `json:"base_ref"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("decode git_diff: %v", err)
		}
		return p.BaseRef
	}
	t.Fatal("no git_diff event emitted")
	return ""
}

// TestResolveStageDiffBase_FixupIncrementUnresolved_FallsBackCumulative (R3):
// an increment base that is not a commit in the repository (a freshly
// generated random SHA — definitionally absent) falls back to the CUMULATIVE
// chain against checkBaseRef — failing toward enforcement, never toward a
// smaller diff — and the unresolved line names BOTH the SHA label and the
// cumulative base actually measured (approval condition C5).
//
// MECHANISM: the returned value is main's commit only via the fallback;
// returning the unresolved SHA (deleting the fallback) reddens the value.
func TestResolveStageDiffBase_FixupIncrementUnresolved_FallsBackCumulative(t *testing.T) {
	repo, mainSHA, _ := initConsolidatedRepo(t)
	gitRunnerFor(t, repo)("checkout", "-q", "consolidated")
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	absent := hex.EncodeToString(buf)

	var log strings.Builder
	cfg := config{workingDir: repo, checkBaseRef: "main", stageID: "s-parent", fixupIncrementBaseSHA: absent}
	got := resolveStageDiffBase(context.Background(), cfg, absent, repo, &log)
	if got != mainSHA {
		t.Fatalf("resolveStageDiffBase = %q, want the cumulative merge-base (main %q)\n%s", got, mainSHA, log.String())
	}
	for _, want := range []string{
		`"event":"diff_base_fixup_increment_unresolved"`,
		`"base_sha":"` + absent + `"`,
		`"cumulative_base_ref":"main"`,
		`"measured_against":"` + mainSHA + `"`,
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("unresolved line missing %s:\n%s", want, log.String())
		}
	}
	if strings.Contains(log.String(), `"event":"diff_base_fixup_increment",`) {
		t.Errorf("the resolved-increment line fired for an absent SHA:\n%s", log.String())
	}
}

// TestRun_Fixup_DecomposedParent_PolicyBoundedToIncrement (R4) is the
// cross-boundary test for #4391: it crosses the wire struct
// (upload.FetchedPrompt.FixupPolicyIncrement) → fetchPromptToFile → config →
// run()'s fix-up block → both diff helpers → the in-band enforceConstraints →
// the shipped bundle whose LAST git_diff the backend's reEvaluatePolicy reads
// (the backend half is TestShipTrace_DecomposedParentFixup_PolicyBoundedToIncrement).
//
// The fake checkoutFixupBase performs a REAL detached checkout of the
// consolidated tip in the directory run() hands it (the lineage worktree), and
// the agent writes one file there; --constraints-file caps the stage at 2.
//
//   - flag true: exits OK with no category B, and the bundle's last git_diff
//     lists exactly the one file, labelled with the bare tip SHA.
//   - flag false: the same fixture fails category B (4 > 2).
//
// MECHANISM: the ONLY difference between the arms is the served flag; deleting
// run()'s `cfg.fixupIncrementBaseSHA = tipSHA` (or the
// cfg.fixupPolicyIncrement assignment from the fetched prompt) makes the true
// arm measure from merge-base(main) → 4 files → category B.
func TestRun_Fixup_DecomposedParent_PolicyBoundedToIncrement(t *testing.T) {
	const (
		runID   = "11111111-2222-3333-4444-555555555555"
		stageID = "22222222-3333-4444-5555-666666666666"
		branch  = "fishhawk/run-11111111-consolidated"
	)
	for _, tc := range []struct {
		name string
		flag bool
	}{
		{"flag served", true},
		{"flag absent", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			implementEnv(t, "kuhlman-labs/fishhawk", "main")
			repo, _, tipSHA := initConsolidatedRepo(t)
			constraintsFile := writeConstraintsFile(t, `{"MaxFilesChanged": 2}`)
			bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")

			withFakeInvoker(t, &fakeInvoker{
				canned: agent.Result{OK: true},
				onInvoke: func(_ int, inv agent.Invocation) {
					writeRepoFile(t, inv.WorkingDir, fixupIncrementFile)
				},
			})
			fu := newFakeUploader(t)
			fu.promptResp = &upload.FetchedPrompt{
				StageID:              stageID,
				StageType:            "implement",
				Prompt:               "implement",
				PromptHash:           "h",
				Fixup:                true,
				FixupBranch:          branch,
				FixupExpectedHeadSHA: tipSHA,
				FixupPolicyIncrement: tc.flag,
			}
			withFakeUploader(t, fu)
			fp := &fakePusher{}
			withFakeGitOps(t, fp, &fakePROpener{})
			var checkoutDir string
			checkoutFixupBase = func(ctx context.Context, dir, _, gotBranch, _ string, _ []string) (string, error) {
				if gotBranch != branch {
					t.Errorf("checkout branch = %q, want %q", gotBranch, branch)
				}
				checkoutDir = dir
				cmd := exec.CommandContext(ctx, "git", "checkout", "-q", "--detach", tipSHA)
				cmd.Dir = dir
				if out, err := cmd.CombinedOutput(); err != nil {
					return "", fmt.Errorf("checkout tip: %w: %s", err, out)
				}
				return tipSHA, nil
			}

			var stderr strings.Builder
			got := run([]string{
				"--run-id", runID,
				"--backend-url", "https://api.fishhawk.test",
				"--workflow", "feature_change", "--stage", "implement",
				"--stage-id", stageID,
				"--working-dir", repo,
				"--fetch-prompt", "--upload-trace",
				"--bundle-out", bundlePath,
				"--check-base-ref", "main",
				"--constraints-file", constraintsFile,
			}, &stderr)
			if checkoutDir == "" {
				t.Fatalf("checkoutFixupBase was never called:\n%s", stderr.String())
			}

			var lastDiff struct {
				BaseRef string `json:"base_ref"`
				Files   []struct {
					Path string `json:"path"`
				} `json:"files"`
			}
			var sawDiff bool
			for _, ev := range bundleEventsForTest(t, bundlePath) {
				if ev.Kind != "git_diff" {
					continue
				}
				sawDiff = true
				if err := json.Unmarshal(ev.Data, &lastDiff); err != nil {
					t.Fatalf("decode git_diff: %v", err)
				}
			}
			if !sawDiff {
				t.Fatalf("the shipped bundle carries no git_diff event:\n%s", stderr.String())
			}
			var paths []string
			for _, f := range lastDiff.Files {
				paths = append(paths, f.Path)
			}
			slices.Sort(paths)
			categoryB := strings.Contains(stderr.String(), `"category":"B"`)

			if tc.flag {
				if got != exitOK || categoryB {
					t.Fatalf("run = %d (category B: %v), want exitOK with no category B:\n%s", got, categoryB, stderr.String())
				}
				if !slices.Equal(paths, []string{fixupIncrementFile}) {
					t.Errorf("last git_diff files = %v, want only the fix-up's %q", paths, fixupIncrementFile)
				}
				if lastDiff.BaseRef != tipSHA {
					t.Errorf("last git_diff base_ref = %q, want the bare tip SHA %q", lastDiff.BaseRef, tipSHA)
				}
				for _, want := range []string{
					`"event":"fixup_policy_increment_base"`,
					`"event":"policy_base_decomposed_parent_fixup"`,
					`"event":"diff_base_fixup_increment"`,
				} {
					if !strings.Contains(stderr.String(), want) {
						t.Errorf("missing %s:\n%s", want, stderr.String())
					}
				}
				return
			}
			if got != exitFailure || !categoryB {
				t.Fatalf("run = %d (category B: %v), want exitFailure category B (4 files > 2):\n%s", got, categoryB, stderr.String())
			}
			if len(paths) != len(consolidatedFanInFiles)+1 {
				t.Errorf("cumulative last git_diff files = %v, want the 3 fan-in files + the fix-up's", paths)
			}
			if strings.Contains(stderr.String(), "fixup_policy_increment_base") {
				t.Errorf("an unflagged fix-up recorded an increment base:\n%s", stderr.String())
			}
		})
	}
}
