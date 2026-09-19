package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
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

// Fixture run ids for the cases below. `runner start` now reads the
// run row before spawning whenever --forge or --github-repo is
// omitted (E45.46 / #3463), so every such case needs a real UUID and
// a fake backend serving GET /v0/runs/{id} — the pre-#3463 "1"/"2"
// placeholders would be refused as unreadable, and the refusal must
// NOT be weakened to green them.
const (
	fixtureRunID   = "11111111-2222-3333-4444-555555555555"
	fixtureStageID = "22222222-3333-4444-5555-666666666666"
)

// runRowBackend is an httptest backend serving ONE run row on the
// single-run route only. getCalls counts GET /v0/runs/{id} hits; the
// list route deliberately 404s so a producer that read the list
// instead of the single-run GET (which omits forge_base_url on the
// real backend) cannot resolve a gitlab target from it.
type runRowBackend struct {
	srv      *httptest.Server
	getCalls atomic.Int64
	status   int
}

func newRunRowBackend(t *testing.T, row httpclient.Run) *runRowBackend {
	t.Helper()
	fb := &runRowBackend{status: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}", func(w http.ResponseWriter, _ *http.Request) {
		fb.getCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if fb.status != http.StatusOK {
			w.WriteHeader(fb.status)
			_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"boom"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(row)
	})
	fb.srv = httptest.NewServer(mux)
	t.Cleanup(fb.srv.Close)
	return fb
}

// withGitHubRunRow wires a plain github run row (forge github, repo
// x/y) behind the runnerNewClient seam so the pre-existing github
// cases keep their original shape — the row supplies the forge, and
// the repo still comes from the flag or the origin auto-detect.
func withGitHubRunRow(t *testing.T) *runRowBackend {
	t.Helper()
	fb := newRunRowBackend(t, httpclient.Run{
		ID: uuid.MustParse(fixtureRunID), Repo: "x/y", WorkflowID: "w",
		State: "running", RunnerKind: "local", Forge: forgeGitHub,
	})
	withFakeHTTPClient(t, fb.srv)
	return fb
}

// withFakeGitRemoteNeverCalled fails the test if the origin
// auto-detect seam is consulted at all.
func withFakeGitRemoteNeverCalled(t *testing.T) {
	t.Helper()
	orig := gitRemoteOriginURL
	gitRemoteOriginURL = func(_ string) (string, error) {
		t.Error("gitRemoteOriginURL must not be called: the repo comes from the flag or the run row")
		return "", errors.New("must not be called")
	}
	t.Cleanup(func() { gitRemoteOriginURL = orig })
}

func TestRunnerStart_HappyPath_BuildsExpectedArgv(t *testing.T) {
	cap := withFakeRunnerSpawn(t)
	withFakeGitRemote(t, "https://github.com/kuhlman-labs/fishhawk.git", nil)
	withNoopAutoPR(t)
	withGitHubRunRow(t)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", fixtureRunID,
		"--stage-id", fixtureStageID,
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
		"--run-id", fixtureRunID,
		"--stage-id", fixtureStageID,
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
	// A github run emits NO forge flags — the github argv is
	// byte-compatible with its pre-#3463 self.
	for _, banned := range []string{"--forge", "--gitlab-base-url"} {
		if contains(cap.args, banned) {
			t.Errorf("github argv must not carry %s: %v", banned, cap.args)
		}
	}
	// Implement stages carry --check-base-ref <baseBranch> so the runner
	// emits the git_diff event (backend policy_evaluated +
	// implement-review). Assert both the flag and its value follow.
	if i := indexOf(cap.args, "--check-base-ref"); i < 0 || i+1 >= len(cap.args) || cap.args[i+1] != "main" {
		t.Errorf("implement argv missing --check-base-ref main: %v", cap.args)
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
	withGitHubRunRow(t)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", fixtureRunID,
		"--stage-id", fixtureStageID,
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
	// Plan stages produce no diff, so --check-base-ref is omitted.
	if contains(cap.args, "--check-base-ref") {
		t.Errorf("plan-stage argv should NOT include --check-base-ref: %v", cap.args)
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
			withGitHubRunRow(t)
			if stage == "implement" {
				withNoopAutoPR(t)
			}
			got := run([]string{
				"runner", "start",
				"--run-id", fixtureRunID, "--stage-id", fixtureStageID,
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
	withGitHubRunRow(t)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", fixtureRunID,
		"--stage-id", fixtureStageID,
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
	withGitHubRunRow(t)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", fixtureRunID, "--stage-id", fixtureStageID,
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
	withGitHubRunRow(t)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", fixtureRunID, "--stage-id", fixtureStageID,
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
	withGitHubRunRow(t)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", fixtureRunID, "--stage-id", fixtureStageID,
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

	// Binary resolution precedes the pre-spawn run read, so no
	// backend is needed here and the placeholder ids are fine.
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

// withFakeHTTPClient swaps runnerNewClient so all HTTP calls inside
// runRunnerStart go to srv instead of the real backend.
func withFakeHTTPClient(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := runnerNewClient
	runnerNewClient = func(_ commonFlags) *httpclient.Client {
		return httpclient.New(srv.URL, "")
	}
	t.Cleanup(func() { runnerNewClient = orig })
}

// withCapturedPostOrEdit swaps postOrEditStatusComment to record calls
// without shelling to gh or reaching the backend status-comment endpoints.
type postOrEditCall struct {
	backendURL  string
	runID       string
	repo        string
	issueNumber int
}

func withCapturedPostOrEdit(t *testing.T) *[]postOrEditCall {
	t.Helper()
	calls := new([]postOrEditCall)
	orig := postOrEditStatusComment
	postOrEditStatusComment = func(backendURL, runID, repo string, issueNumber int) error {
		*calls = append(*calls, postOrEditCall{backendURL, runID, repo, issueNumber})
		return nil
	}
	t.Cleanup(func() { postOrEditStatusComment = orig })
	return calls
}

// TestRunnerStart_StageCompleteComment verifies that after a plan-stage
// runner exits cleanly for a local-runner issue-triggered run, the CLI
// calls postOrEditStatusComment with the right coordinates. Rendering is
// server-side (#428); this test only checks that the seam is invoked.
func TestRunnerStart_StageCompleteComment(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()

	runRow := httpclient.Run{
		ID:         runID,
		Repo:       "x/y",
		WorkflowID: "w",
		State:      "running",
		RunnerKind: "local",
		IssueContext: &httpclient.IssueContext{
			Title:  "test issue",
			Body:   "body",
			URL:    "https://github.com/x/y/issues/1",
			Number: 1,
		},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(runRow)
	}))
	t.Cleanup(srv.Close)

	withFakeRunnerSpawn(t)
	withFakeGitRemote(t, "", errors.New("no git"))
	withFakeHTTPClient(t, srv)
	calls := withCapturedPostOrEdit(t)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", runID.String(),
		"--stage-id", stageID.String(),
		"--workflow", "w",
		"--stage", "plan",
		"--backend-url", srv.URL,
	}, &stdout, &stderr)

	if got != exitOK {
		t.Fatalf("exit = %d, want exitOK:\n%s", got, stderr.String())
	}
	if len(*calls) == 0 {
		t.Fatal("postOrEditStatusComment was not called")
	}
	c := (*calls)[0]
	if c.runID != runID.String() {
		t.Errorf("runID = %q, want %q", c.runID, runID.String())
	}
	if c.repo != "x/y" {
		t.Errorf("repo = %q, want x/y", c.repo)
	}
	if c.issueNumber != 1 {
		t.Errorf("issueNumber = %d, want 1", c.issueNumber)
	}
}

// contains reports whether s appears anywhere in xs.
func contains(xs []string, s string) bool {
	return indexOf(xs, s) >= 0
}

func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}

// --- Forge target resolution (E45.46 / #3463) --------------------------------
//
// These cases drive the real runRunnerStart against a fake backend
// that serves the run row on the single-run GET only, and assert on
// the captured argv plus the seams (`gitRemoteOriginURL`,
// `runnerStartCommand`) — state, not error identity — so each
// fail-closed branch is pinned by what it did NOT do (no spawn, no
// origin auto-detect) as well as by its stderr.

// gitlabRunRow is a gitlab run row on a nested-group project path.
func gitlabRunRow() httpclient.Run {
	return httpclient.Run{
		ID: uuid.MustParse(fixtureRunID), Repo: "group/sub/proj", WorkflowID: "w",
		State: "running", RunnerKind: "local", Forge: forgeGitLab,
		ForgeBaseURL: "https://gitlab.example.com",
	}
}

// assertForgeFlags asserts the exact `--github-repo <repo> --forge
// gitlab --gitlab-base-url <url>` run in order right after
// --github-repo.
func assertForgeFlags(t *testing.T, args []string, repo, baseURL string) {
	t.Helper()
	i := indexOf(args, "--github-repo")
	if i < 0 || i+5 >= len(args) {
		t.Fatalf("argv lacks --github-repo with room for the forge flags: %v", args)
	}
	got := args[i : i+6]
	want := []string{"--github-repo", repo, "--forge", forgeGitLab, "--gitlab-base-url", baseURL}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("forge argv = %v, want %v (full: %v)", got, want, args)
	}
}

// TestRunnerStart_GitLabFromRunRow_EmitsForgeFlags: every forge input
// omitted, the run row supplies forge, base URL AND repo; the
// github.com-only origin auto-detect is never consulted.
func TestRunnerStart_GitLabFromRunRow_EmitsForgeFlags(t *testing.T) {
	cap := withFakeRunnerSpawn(t)
	withFakeGitRemoteNeverCalled(t)
	fb := newRunRowBackend(t, gitlabRunRow())
	withFakeHTTPClient(t, fb.srv)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", fixtureRunID, "--stage-id", fixtureStageID,
		"--workflow", "w", "--stage", "implement",
	}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	assertForgeFlags(t, cap.args, "group/sub/proj", "https://gitlab.example.com")
	if n := fb.getCalls.Load(); n != 1 {
		t.Errorf("GET /v0/runs/{id} called %d times, want exactly 1 (one pre-spawn read; a gitlab run posts no GitHub status comment)", n)
	}
}

// TestRunnerStart_ExplicitForgeAndBaseURL_RepoFromRunRow (constraint
// 3): `--forge gitlab --gitlab-base-url <url>` explicit with
// --github-repo omitted still reads the row, because the row's
// path_with_namespace is the only sane repo default on gitlab.
func TestRunnerStart_ExplicitForgeAndBaseURL_RepoFromRunRow(t *testing.T) {
	cap := withFakeRunnerSpawn(t)
	withFakeGitRemoteNeverCalled(t)
	row := gitlabRunRow()
	row.ForgeBaseURL = "https://row.example.invalid" // must NOT win over the flag
	fb := newRunRowBackend(t, row)
	withFakeHTTPClient(t, fb.srv)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", fixtureRunID, "--stage-id", fixtureStageID,
		"--workflow", "w", "--stage", "implement",
		"--forge", "gitlab", "--gitlab-base-url", "https://gitlab.example.com",
	}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if n := fb.getCalls.Load(); n != 1 {
		t.Errorf("GET /v0/runs/{id} called %d times, want 1 (the repo default needs the row)", n)
	}
	assertForgeFlags(t, cap.args, "group/sub/proj", "https://gitlab.example.com")
}

// TestRunnerStart_ExplicitForgeFlagWinsOverRunRow: the row says
// gitlab, the operator says github — the flag wins and the github
// argv carries no forge flags; --github-repo explicit too, so ZERO
// row reads.
func TestRunnerStart_ExplicitForgeFlagWinsOverRunRow(t *testing.T) {
	cap := withFakeRunnerSpawn(t)
	withFakeGitRemoteNeverCalled(t)
	withNoopAutoPR(t)
	fb := newRunRowBackend(t, gitlabRunRow())
	withFakeHTTPClient(t, fb.srv)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", fixtureRunID, "--stage-id", fixtureStageID,
		"--workflow", "w", "--stage", "implement",
		"--forge", "github", "--github-repo", "explicit/wins",
	}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	for _, banned := range []string{"--forge", "--gitlab-base-url"} {
		if contains(cap.args, banned) {
			t.Errorf("explicit --forge github must emit no forge flags: %v", cap.args)
		}
	}
	if !contains(cap.args, "explicit/wins") {
		t.Errorf("argv missing explicit/wins: %v", cap.args)
	}
}

// TestRunnerStart_GitLabNoBaseURL_ExitsBeforeSpawn: a gitlab row with
// no forge_base_url and no --gitlab-base-url refuses BEFORE the spawn,
// naming the flag and both server-side remedies.
func TestRunnerStart_GitLabNoBaseURL_ExitsBeforeSpawn(t *testing.T) {
	cap := withFakeRunnerSpawn(t)
	withFakeGitRemoteNeverCalled(t)
	row := gitlabRunRow()
	row.ForgeBaseURL = ""
	fb := newRunRowBackend(t, row)
	withFakeHTTPClient(t, fb.srv)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", fixtureRunID, "--stage-id", fixtureStageID,
		"--workflow", "w", "--stage", "implement",
	}, &stdout, &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if cap.binary != "" || cap.args != nil {
		t.Errorf("runner must NOT be spawned without a base URL; spawned %q %v", cap.binary, cap.args)
	}
	for _, want := range []string{"--gitlab-base-url", "FISHHAWKD_GITLAB_BASE_URL", "--forge-base-url"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr should name %s: %s", want, stderr.String())
		}
	}
}

// TestRunnerStart_RunReadFails_RefusesSpawn (constraint 2): --forge
// omitted and the row unreadable → exitFailure naming --forge and
// backend reachability, and NO spawn. Never a github default with a
// warning. Two subcases: an HTTP 500 from a reachable backend, and a
// closed listener (connection refused).
func TestRunnerStart_RunReadFails_RefusesSpawn(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire func(t *testing.T)
	}{
		{"http 500", func(t *testing.T) {
			fb := newRunRowBackend(t, gitlabRunRow())
			fb.status = http.StatusInternalServerError
			withFakeHTTPClient(t, fb.srv)
		}},
		{"connection refused", func(t *testing.T) {
			dead := httptest.NewServer(http.NotFoundHandler())
			dead.Close()
			withFakeHTTPClient(t, dead)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := withFakeRunnerSpawn(t)
			withFakeGitRemoteNeverCalled(t)
			tc.wire(t)

			var stdout, stderr strings.Builder
			got := run([]string{
				"runner", "start",
				"--run-id", fixtureRunID, "--stage-id", fixtureStageID,
				"--workflow", "w", "--stage", "implement",
			}, &stdout, &stderr)
			if got != exitFailure {
				t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
			}
			if cap.binary != "" || cap.args != nil {
				t.Errorf("runner must NOT be spawned when the forge is unresolvable; spawned %q %v", cap.binary, cap.args)
			}
			for _, want := range []string{"could not read run " + fixtureRunID, "--forge", "--backend-url", "FISHHAWK_BACKEND_URL", "not spawning"} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr should contain %q: %s", want, stderr.String())
				}
			}
		})
	}
}

// TestRunnerStart_ExplicitGitLab_RunReadFails_NamesMissingFlags: the
// forge is known (gitlab) but the row is unreadable and the base URL
// / repo it would have supplied are missing → refuse naming each.
func TestRunnerStart_ExplicitGitLab_RunReadFails_NamesMissingFlags(t *testing.T) {
	for _, tc := range []struct {
		name    string
		extra   []string
		wantAll []string
		wantNot []string
	}{
		{"both omitted", nil, []string{"--gitlab-base-url", "--github-repo"}, nil},
		{"only repo omitted", []string{"--gitlab-base-url", "https://gitlab.example.com"}, []string{"--github-repo"}, []string{"--gitlab-base-url and"}},
		{"only base url omitted", []string{"--github-repo", "g/p"}, []string{"--gitlab-base-url"}, []string{"and --github-repo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := withFakeRunnerSpawn(t)
			withFakeGitRemoteNeverCalled(t)
			fb := newRunRowBackend(t, gitlabRunRow())
			fb.status = http.StatusInternalServerError
			withFakeHTTPClient(t, fb.srv)

			var stdout, stderr strings.Builder
			got := run(append([]string{
				"runner", "start",
				"--run-id", fixtureRunID, "--stage-id", fixtureStageID,
				"--workflow", "w", "--stage", "implement",
				"--forge", "gitlab",
			}, tc.extra...), &stdout, &stderr)
			if got != exitFailure {
				t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
			}
			if cap.binary != "" || cap.args != nil {
				t.Errorf("runner must NOT be spawned; spawned %q %v", cap.binary, cap.args)
			}
			for _, want := range tc.wantAll {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr should name %s: %s", want, stderr.String())
				}
			}
			for _, not := range tc.wantNot {
				if strings.Contains(stderr.String(), not) {
					t.Errorf("stderr should not mention %q: %s", not, stderr.String())
				}
			}
		})
	}
}

// TestRunnerStart_ExplicitGitHub_RunReadFails_FallsThroughToAutoDetect:
// with `--forge github` explicit the forge is known, so an unreadable
// row degrades to today's origin auto-detect — the run-row repo
// default is a convenience the github path never had.
func TestRunnerStart_ExplicitGitHub_RunReadFails_FallsThroughToAutoDetect(t *testing.T) {
	cap := withFakeRunnerSpawn(t)
	withFakeGitRemote(t, "https://github.com/operator/scratch.git", nil)
	withNoopAutoPR(t)
	fb := newRunRowBackend(t, gitlabRunRow())
	fb.status = http.StatusInternalServerError
	withFakeHTTPClient(t, fb.srv)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", fixtureRunID, "--stage-id", fixtureStageID,
		"--workflow", "w", "--stage", "implement",
		"--forge", "github",
	}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if !contains(cap.args, "operator/scratch") {
		t.Errorf("argv missing auto-detected operator/scratch: %v", cap.args)
	}
	if contains(cap.args, "--forge") {
		t.Errorf("github argv must carry no --forge: %v", cap.args)
	}
}

// TestRunnerStart_GitHub_ArgvUnchanged is the github golden with the
// fake row: the run row says github, the argv is exactly the
// pre-#3463 shape (no forge flags anywhere), repo from origin.
func TestRunnerStart_GitHub_ArgvUnchanged(t *testing.T) {
	cap := withFakeRunnerSpawn(t)
	withFakeGitRemote(t, "https://github.com/kuhlman-labs/fishhawk.git", nil)
	withGitHubRunRow(t)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", fixtureRunID, "--stage-id", fixtureStageID,
		"--workflow", "w", "--stage", "implement",
		"--backend-url", "http://localhost:8080",
		"--no-pr",
	}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	want := []string{
		"--run-id", fixtureRunID,
		"--backend-url", "http://localhost:8080",
		"--workflow", "w",
		"--stage", "implement",
		"--stage-id", fixtureStageID,
		"--working-dir", ".",
		"--fetch-prompt",
		"--upload-trace",
		"--github-repo", "kuhlman-labs/fishhawk",
		"--base-branch", "main",
		"--check-base-ref", "main",
		"--no-pr",
	}
	if strings.Join(cap.args, " ") != strings.Join(want, " ") {
		t.Errorf("github argv drifted:\n got %v\nwant %v", cap.args, want)
	}
}

// TestRunnerStart_OlderBackendNoForgeField_DefaultsGitHub: a run row
// from a backend predating the forge field decodes to Forge "" and
// resolves to github.
func TestRunnerStart_OlderBackendNoForgeField_DefaultsGitHub(t *testing.T) {
	cap := withFakeRunnerSpawn(t)
	withFakeGitRemote(t, "https://github.com/kuhlman-labs/fishhawk.git", nil)
	fb := newRunRowBackend(t, httpclient.Run{
		ID: uuid.MustParse(fixtureRunID), Repo: "x/y", WorkflowID: "w",
		State: "running", RunnerKind: "local",
	})
	withFakeHTTPClient(t, fb.srv)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", fixtureRunID, "--stage-id", fixtureStageID,
		"--workflow", "w", "--stage", "plan",
	}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if contains(cap.args, "--forge") || contains(cap.args, "--gitlab-base-url") {
		t.Errorf("older-backend row must resolve to github (no forge flags): %v", cap.args)
	}
}

// TestRunnerStart_AllFlagsExplicit_NoRunRead: --forge github +
// --github-repo explicit → zero network calls before the spawn, as
// before #3463 (the seam points at a backend that counts).
func TestRunnerStart_AllFlagsExplicit_NoRunRead(t *testing.T) {
	cap := withFakeRunnerSpawn(t)
	withFakeGitRemoteNeverCalled(t)
	fb := newRunRowBackend(t, gitlabRunRow())
	withFakeHTTPClient(t, fb.srv)
	// The post-run status-comment read is a SEPARATE, post-spawn
	// GET, so the ordering witness is the GET count AT SPAWN TIME:
	// wrap the spawn seam to snapshot it.
	getsAtSpawn := int64(-1)
	inner := runnerStartCommand
	runnerStartCommand = func(name string, arg ...string) *exec.Cmd {
		getsAtSpawn = fb.getCalls.Load()
		return inner(name, arg...)
	}
	t.Cleanup(func() { runnerStartCommand = inner })

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", fixtureRunID, "--stage-id", fixtureStageID,
		"--workflow", "w", "--stage", "plan",
		"--forge", "github", "--github-repo", "x/y",
	}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if getsAtSpawn != 0 {
		t.Errorf("GET /v0/runs/{id} called %d times before the spawn, want 0 with every forge input explicit", getsAtSpawn)
	}
	if !contains(cap.args, "x/y") {
		t.Errorf("argv missing explicit x/y: %v", cap.args)
	}
}

// TestRunnerStart_InvalidForge_Usage: a --forge outside github|gitlab
// is a usage error with no spawn and no backend read.
func TestRunnerStart_InvalidForge_Usage(t *testing.T) {
	cap := withFakeRunnerSpawn(t)
	fb := newRunRowBackend(t, gitlabRunRow())
	withFakeHTTPClient(t, fb.srv)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", fixtureRunID, "--stage-id", fixtureStageID,
		"--workflow", "w", "--stage", "plan",
		"--forge", "bitbucket",
	}, &stdout, &stderr)
	if got != exitUsage {
		t.Fatalf("run = %d, want exitUsage:\n%s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--forge") {
		t.Errorf("stderr should name --forge: %s", stderr.String())
	}
	if cap.args != nil {
		t.Errorf("runner must NOT be spawned: %v", cap.args)
	}
	if n := fb.getCalls.Load(); n != 0 {
		t.Errorf("backend read %d times, want 0", n)
	}
}

// TestRunnerStart_GitLabRun_SkipsGitHubStatusComment: the sticky
// status comment goes through `gh` against github.com, so a gitlab
// run posts none even when the row carries an issue_context.
func TestRunnerStart_GitLabRun_SkipsGitHubStatusComment(t *testing.T) {
	withFakeRunnerSpawn(t)
	withFakeGitRemoteNeverCalled(t)
	row := gitlabRunRow()
	row.IssueContext = &httpclient.IssueContext{Title: "t", Number: 7, URL: "https://gitlab.example.com/group/sub/proj/-/issues/7"}
	fb := newRunRowBackend(t, row)
	withFakeHTTPClient(t, fb.srv)
	calls := withCapturedPostOrEdit(t)

	var stdout, stderr strings.Builder
	got := run([]string{
		"runner", "start",
		"--run-id", fixtureRunID, "--stage-id", fixtureStageID,
		"--workflow", "w", "--stage", "plan",
	}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if len(*calls) != 0 {
		t.Errorf("postOrEditStatusComment called %d times on a gitlab run, want 0: %+v", len(*calls), *calls)
	}
}
