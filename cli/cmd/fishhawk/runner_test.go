package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// withFakeRunnerSpawn patches the runner-spawn seam so tests can
// assert on the constructed argv without actually invoking a real
// fishhawk-runner binary. Cleanup restores the production hook.
type spawnCapture struct {
	binary string
	args   []string
	env    []string
}

func withFakeRunnerSpawn(t *testing.T) *spawnCapture {
	t.Helper()
	captured := &spawnCapture{}
	orig := runnerStartCommand
	runnerStartCommand = func(name string, arg ...string) *exec.Cmd {
		captured.binary = name
		captured.args = append([]string(nil), arg...)
		// /usr/bin/true exits 0 immediately on every platform we
		// support; lets cmd.Run() return cleanly without invoking
		// the real runner. cmd.Env capture happens in the handler
		// before Run() is called.
		c := exec.Command("/usr/bin/true")
		return c
	}
	// Also pin a binary path so tests don't hit the PATH lookup.
	origLook := runnerBinaryLookPath
	runnerBinaryLookPath = func(_ string) (string, error) {
		return "/usr/local/bin/fishhawk-runner", nil
	}
	t.Cleanup(func() {
		runnerStartCommand = orig
		runnerBinaryLookPath = origLook
	})
	// Capture env from the spawned cmd by overriding once more —
	// wrap the spawn to remember cmd.Env after the handler sets it.
	orig2 := runnerStartCommand
	runnerStartCommand = func(name string, arg ...string) *exec.Cmd {
		c := orig2(name, arg...)
		// After the caller sets cmd.Env, the runRunnerStart code path
		// has the captured arg list. Sniff env from os/exec.Cmd via
		// a wrapper that overrides Run() — simpler: leave Env capture
		// to a separate test that reads cmd.Env directly from the
		// returned exec.Cmd by walking the Cmd's process state.
		// For these tests, argv assertions are enough.
		return c
	}
	return captured
}

// withFakeGitRemote pins the auto-detect seam. Empty url + non-nil
// err simulates "not in a git repo" / "no origin remote."
func withFakeGitRemote(t *testing.T, url string, err error) {
	t.Helper()
	orig := gitRemoteOriginURL
	gitRemoteOriginURL = func(_ string) (string, error) {
		if err != nil {
			return "", err
		}
		return url, nil
	}
	t.Cleanup(func() { gitRemoteOriginURL = orig })
}

// withNoopAutoPR stubs autoOpenPR's test seams so implement-stage
// runner_test cases don't run real git/gh against the live checkout.
// The autopr_test.go suite covers autoOpenPR's behavior; here we
// just need the seams stubbed so the runner_test stays hermetic.
// Without this, TestRunnerStart_HappyPath_BuildsExpectedArgv created
// a real branch + commit in the dev checkout during sub-task 4 of
// #422 (caught in retro; fixed here as a follow-up).
func withNoopAutoPR(t *testing.T) {
	t.Helper()
	origGit := autoGitCommand
	origGh := autoGhCommand
	noop := func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("/usr/bin/false")
	}
	autoGitCommand = noop
	autoGhCommand = noop
	t.Cleanup(func() {
		autoGitCommand = origGit
		autoGhCommand = origGh
	})
}

func TestRunnerStart_HappyPath_BuildsExpectedArgv(t *testing.T) {
	cap := withFakeRunnerSpawn(t)
	withFakeGitRemote(t, "https://github.com/kuhlman-labs/fishhawk.git", nil)
	withNoopAutoPR(t)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--workflow", "feature_change",
		"--stage", "implement",
		"--backend-url", "http://localhost:8080",
		"--token", "fhk_dev",
	}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if cap.binary != "/usr/local/bin/fishhawk-runner" {
		t.Errorf("binary = %q, want /usr/local/bin/fishhawk-runner", cap.binary)
	}
	// Required flags surface in the constructed argv. Spot-check the
	// material ones; --no-pr is NOT included by default (default=false).
	for _, want := range []string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--workflow", "feature_change",
		"--stage", "implement",
		"--backend-url", "http://localhost:8080",
		"--working-dir", ".",
		"--fetch-prompt",
		"--upload-trace",
		"--github-repo", "kuhlman-labs/fishhawk",
		"--base-branch", "main",
	} {
		if !contains(cap.args, want) {
			t.Errorf("argv missing %q: %v", want, cap.args)
		}
	}
	if contains(cap.args, "--no-pr") {
		t.Errorf("argv should NOT include --no-pr when flag is not passed (default=false): %v", cap.args)
	}
}

// TestRunnerStart_PlanStage_PassesPlanOut exercises the
// local-runner plan-validation wiring: when --stage plan, the CLI
// auto-appends --plan-out /tmp/fishhawk-plan.json so the runner
// validates + uploads the plan artifact the agent produces. The
// GHA action.yml passes the same path; we mirror it here. Without
// this flag the plan stage uploads a trace but the artifact never
// lands and the stage is stuck.
func TestRunnerStart_PlanStage_PassesPlanOut(t *testing.T) {
	cap := withFakeRunnerSpawn(t)
	withFakeGitRemote(t, "https://github.com/kuhlman-labs/fishhawk.git", nil)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--workflow", "feature_change",
		"--stage", "plan",
	}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if !contains(cap.args, "--plan-out") {
		t.Errorf("plan-stage argv missing --plan-out: %v", cap.args)
	}
	if !contains(cap.args, "/tmp/fishhawk-plan.json") {
		t.Errorf("plan-stage argv missing /tmp/fishhawk-plan.json: %v", cap.args)
	}
}

// TestRunnerStart_NonPlanStage_OmitsPlanOut pins the negative: for
// implement / review the wrapper does NOT pass --plan-out. The
// runner only validates + uploads when --plan-out is set; passing
// it for stages that don't produce a plan would either silently
// no-op or warn.
func TestRunnerStart_NonPlanStage_OmitsPlanOut(t *testing.T) {
	for _, stage := range []string{"implement", "review"} {
		t.Run(stage, func(t *testing.T) {
			cap := withFakeRunnerSpawn(t)
			withFakeGitRemote(t, "https://github.com/x/y.git", nil)
			if stage == "implement" {
				withNoopAutoPR(t)
			}
			got := run([]string{
				"runner", "start",
				"--run-id", "1", "--stage-id", "2",
				"--workflow", "w", "--stage", stage,
			}, &strings.Builder{}, &strings.Builder{})
			if got != exitOK {
				t.Fatalf("run = %d", got)
			}
			if contains(cap.args, "--plan-out") {
				t.Errorf("%s-stage argv should NOT include --plan-out: %v", stage, cap.args)
			}
		})
	}
}

func TestRunnerStart_GithubRepoFlag_OverridesAutoDetect(t *testing.T) {
	cap := withFakeRunnerSpawn(t)
	// Auto-detect would return one repo; the explicit flag should win.
	withFakeGitRemote(t, "https://github.com/wrong/auto-detect.git", nil)
	withNoopAutoPR(t)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--workflow", "w", "--stage", "implement",
		"--github-repo", "explicit/wins",
	}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d:\n%s", got, stderr.String())
	}
	if !contains(cap.args, "explicit/wins") {
		t.Errorf("argv missing explicit/wins: %v", cap.args)
	}
	if contains(cap.args, "wrong/auto-detect") {
		t.Errorf("argv should NOT carry the auto-detect value when --github-repo is set: %v", cap.args)
	}
}

func TestRunnerStart_AutoDetect_PullsFromGitRemote(t *testing.T) {
	cap := withFakeRunnerSpawn(t)
	withFakeGitRemote(t, "git@github.com:operator/scratch.git", nil)
	withNoopAutoPR(t)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", "1", "--stage-id", "2",
		"--workflow", "w", "--stage", "implement",
	}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d:\n%s", got, stderr.String())
	}
	if !contains(cap.args, "operator/scratch") {
		t.Errorf("argv missing auto-detected operator/scratch: %v", cap.args)
	}
}

func TestRunnerStart_AutoDetectFailure_NoPRDefault_StillSucceeds(t *testing.T) {
	// `git remote get-url origin` fails (not in a git repo). With
	// --stage plan (not implement), the detection error is silently
	// skipped even with noPR=false — the guard is *noPR ||
	// *stage != "implement", so plan stages never need a repo for
	// PR purposes. The CLI should NOT fail.
	cap := withFakeRunnerSpawn(t)
	withFakeGitRemote(t, "", errors.New("not in a git repo"))

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", "1", "--stage-id", "2",
		"--workflow", "w", "--stage", "plan",
	}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d:\n%s\n%s", got, stderr.String(), stdout.String())
	}
	// argv should NOT carry --github-repo when neither flag nor
	// auto-detect succeeded.
	if contains(cap.args, "--github-repo") {
		t.Errorf("argv should omit --github-repo when not detectable: %v", cap.args)
	}
	// --no-pr is not in argv by default (default=false).
	if contains(cap.args, "--no-pr") {
		t.Errorf("argv should NOT include --no-pr when flag is not passed: %v", cap.args)
	}
}

func TestRunnerStart_ExplicitNoPR_FlagReachesSubprocess(t *testing.T) {
	cap := withFakeRunnerSpawn(t)
	withFakeGitRemote(t, "https://github.com/kuhlman-labs/fishhawk.git", nil)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", "1", "--stage-id", "2",
		"--workflow", "w", "--stage", "implement",
		"--no-pr",
	}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d:\n%s", got, stderr.String())
	}
	if !contains(cap.args, "--no-pr") {
		t.Errorf("argv missing --no-pr when passed explicitly: %v", cap.args)
	}
}

func TestRunnerStart_RequiredFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"missing run-id", []string{"runner", "start",
			"--stage-id", "x", "--workflow", "w", "--stage", "plan"}},
		{"missing stage-id", []string{"runner", "start",
			"--run-id", "x", "--workflow", "w", "--stage", "plan"}},
		{"missing workflow", []string{"runner", "start",
			"--run-id", "x", "--stage-id", "x", "--stage", "plan"}},
		{"missing stage", []string{"runner", "start",
			"--run-id", "x", "--stage-id", "x", "--workflow", "w"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			got := run(tc.args, &stdout, &stderr)
			if got != exitUsage {
				t.Errorf("run = %d, want exitUsage", got)
			}
			if !strings.Contains(stderr.String(), "required") {
				t.Errorf("stderr should mention required: %s", stderr.String())
			}
		})
	}
}

func TestRunnerStart_BinaryNotFound(t *testing.T) {
	orig := runnerBinaryLookPath
	runnerBinaryLookPath = func(_ string) (string, error) {
		return "", errors.New(`exec: "fishhawk-runner": executable file not found in $PATH`)
	}
	t.Cleanup(func() { runnerBinaryLookPath = orig })

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", "1", "--stage-id", "2",
		"--workflow", "w", "--stage", "plan",
	}, &stdout, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "fishhawk-runner not found on PATH") {
		t.Errorf("stderr should explain the lookup failure: %s", stderr.String())
	}
}

func TestRunnerStart_UnknownSubcommand(t *testing.T) {
	var stdout, stderr strings.Builder
	got := run([]string{"runner", "bogus"}, &stdout, &stderr)
	if got != exitUsage {
		t.Errorf("run = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), `unknown subcommand "bogus"`) {
		t.Errorf("stderr should name the bad subcommand: %s", stderr.String())
	}
}

func TestRunnerStart_NoSubcommand(t *testing.T) {
	var stdout, stderr strings.Builder
	got := run([]string{"runner"}, &stdout, &stderr)
	if got != exitUsage {
		t.Errorf("run = %d, want exitUsage", got)
	}
}

func TestParseGitHubRemote_Variants(t *testing.T) {
	cases := []struct {
		in      string
		wantOwn string
		wantRep string
		wantErr bool
	}{
		{"https://github.com/owner/name.git", "owner", "name", false},
		{"https://github.com/owner/name", "owner", "name", false},
		{"https://github.com/owner/name/", "owner", "name", false},
		{"git@github.com:owner/name.git", "owner", "name", false},
		{"git@github.com:owner/name", "owner", "name", false},
		{"ssh://git@github.com/owner/name.git", "owner", "name", false},
		{"  https://github.com/owner/name.git  ", "owner", "name", false},
		// Non-github hosts are rejected so customers get a clear error
		// rather than a malformed owner/name.
		{"https://gitlab.com/owner/name.git", "", "", true},
		{"https://gh.enterprise.example.com/owner/name.git", "", "", true},
		// Malformed inputs.
		{"https://github.com/no-second-segment", "", "", true},
		{"https://github.com/", "", "", true},
		{"not-a-url", "", "", true},
		{"", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			gotOwn, gotRep, err := parseGitHubRemote(tc.in)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr=%v", err, tc.wantErr)
				return
			}
			if gotOwn != tc.wantOwn || gotRep != tc.wantRep {
				t.Errorf("got (%q, %q), want (%q, %q)", gotOwn, gotRep, tc.wantOwn, tc.wantRep)
			}
		})
	}
}

// contains reports whether s appears anywhere in xs.
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
