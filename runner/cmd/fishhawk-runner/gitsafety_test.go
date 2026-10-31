package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// Tests for the runner's concurrency-safe git wiring (ADR-115 D2, #4648): the
// process-wide maintenance pins run() installs, and the stage-keyed snapshot
// CommitAndPush is handed. Every test here that touches the process env is
// NON-parallel (t.Setenv), and every one that lets the REAL install run
// restores the GIT_CONFIG_COUNT/KEY_n/VALUE_n set afterwards
// (isolateGitConfigEnv), so no later test inherits pins it did not ask for.

// gitConfigEnvIndexSpan is how many GIT_CONFIG_KEY_n/VALUE_n indices
// isolateGitConfigEnv pre-registers for restore beyond any already present.
const gitConfigEnvIndexSpan = 16

// isolateGitConfigEnv clears every command-scope GIT_CONFIG_* entry from the
// test process env and restores the ORIGINAL set (present values, and absence)
// when t ends. It goes through t.Setenv for each name, so it also forbids
// t.Parallel on the calling test. Indices the install may ADD are registered
// too, so they are unset again afterwards.
func isolateGitConfigEnv(t *testing.T) {
	t.Helper()
	names := map[string]bool{"GIT_CONFIG_COUNT": true}
	for i := 0; i < gitConfigEnvIndexSpan; i++ {
		names["GIT_CONFIG_KEY_"+strconv.Itoa(i)] = true
		names["GIT_CONFIG_VALUE_"+strconv.Itoa(i)] = true
	}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			names[name] = true
		}
	}
	for name := range names {
		v, _ := os.LookupEnv(name)
		t.Setenv(name, v) // registers the restore of the original state
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

// swapInstallProcessGitConfig replaces run()'s install seam for t.
func swapInstallProcessGitConfig(t *testing.T, fn func() error) {
	t.Helper()
	orig := installProcessGitConfig
	installProcessGitConfig = fn
	t.Cleanup(func() { installProcessGitConfig = orig })
}

// maskedMaintenanceRepo is a repository whose LOCAL config turns auto
// maintenance ON (maintenance.auto=true, gc.auto=6700). The package TestMain
// pins maintenance.auto=false through GIT_CONFIG_GLOBAL, which repo-local
// config beats, so a child reads true/6700 here unless it carries the
// command-scope pins — the masking guard every maintenance assertion needs.
func maskedMaintenanceRepo(t *testing.T) string {
	t.Helper()
	repo := initRepo(t)
	for _, kv := range [][2]string{{"maintenance.auto", "true"}, {"gc.auto", "6700"}} {
		if out, err := exec.Command("git", "-C", repo, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v\n%s", kv[0], err, out)
		}
	}
	return repo
}

// inheritedGitConfig reads key in repo through a child exec'd with a nil
// cmd.Env — exactly the inherited-env shape of the ~50 git execs in this
// package that do not route through gitops.
func inheritedGitConfig(t *testing.T, repo, key string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "config", "--get", key).Output()
	if err != nil {
		t.Fatalf("git config --get %s: %v", key, err)
	}
	return strings.TrimSpace(string(out))
}

// TestRun_InstallsProcessGitConfig: run() calls the install seam exactly once
// on both the stage path and the specialist-job subcommand. Each case FAILS
// (no flags) right after the point the install must precede — parseFlags'
// usage error, or runSpecialistJob's — so a count of 1 proves the install
// runs BEFORE it: an install placed after parseFlags or after the dispatch is
// never reached here and leaves the count at 0.
func TestRun_InstallsProcessGitConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"stage", nil},
		{"specialist-job", []string{"specialist-job"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			swapInstallProcessGitConfig(t, func() error { calls++; return nil })
			var log bytes.Buffer
			if code := run(tc.args, &log); code == exitOK {
				t.Fatalf("run(%q) = exitOK, want a usage failure past the install:\n%s", tc.args, log.String())
			}
			if calls != 1 {
				t.Fatalf("installProcessGitConfig calls = %d, want exactly 1", calls)
			}
		})
	}
	t.Run("version subcommand skips the install", func(t *testing.T) {
		calls := 0
		swapInstallProcessGitConfig(t, func() error { calls++; return nil })
		if code := run([]string{"version"}, &bytes.Buffer{}); code != exitOK {
			t.Fatalf("run(version) = %d, want exitOK", code)
		}
		if calls != 0 {
			t.Errorf("install calls = %d, want 0 for the version subcommand", calls)
		}
	})
}

// TestRun_RefusesMalformedGitConfigCount: an install failure is a config
// error — run() exits exitUsage with a runner_failed reason=config line naming
// the cause, and never reaches parseFlags. Driven through both an injected
// failure and the REAL install under a malformed inherited GIT_CONFIG_COUNT
// (git itself refuses every command under it).
func TestRun_RefusesMalformedGitConfigCount(t *testing.T) {
	t.Run("seam error", func(t *testing.T) {
		swapInstallProcessGitConfig(t, func() error { return errors.New("gitops: inherited GIT_CONFIG_COUNT=\"x\" is malformed") })
		var log bytes.Buffer
		if code := run(nil, &log); code != exitUsage {
			t.Fatalf("run = %d, want exitUsage (%d):\n%s", code, exitUsage, log.String())
		}
		got := log.String()
		if !strings.Contains(got, `"event":"runner_failed","reason":"config"`) || !strings.Contains(got, "GIT_CONFIG_COUNT") {
			t.Errorf("log = %q, want a runner_failed reason=config line naming GIT_CONFIG_COUNT", got)
		}
		if strings.Count(got, "\n") != 1 {
			t.Errorf("log = %q, want only the runner_failed line (parseFlags must not run)", got)
		}
	})
	t.Run("real install", func(t *testing.T) {
		isolateGitConfigEnv(t)
		swapInstallProcessGitConfig(t, gitops.InstallProcessGitConfig)
		t.Setenv("GIT_CONFIG_COUNT", "x")
		var log bytes.Buffer
		if code := run(nil, &log); code != exitUsage {
			t.Fatalf("run = %d, want exitUsage:\n%s", code, log.String())
		}
		got := log.String()
		if !strings.Contains(got, `"event":"runner_failed","reason":"config"`) || !strings.Contains(got, "GIT_CONFIG_COUNT") {
			t.Errorf("log = %q, want a runner_failed reason=config line naming GIT_CONFIG_COUNT", got)
		}
		if v := os.Getenv("GIT_CONFIG_COUNT"); v != "x" {
			t.Errorf("GIT_CONFIG_COUNT = %q after a refused install, want it unchanged", v)
		}
	})
}

// TestRun_RealInstallPinsInheritedGitChildren is the end-to-end proof of the
// process install: in the masked fixture an inherited-env git child reads the
// repo-local true/6700 BEFORE run() (precondition: the fixture is not masked
// by a pin some earlier test left behind), and false/0 after run() — even
// though run() itself then fails in parseFlags.
func TestRun_RealInstallPinsInheritedGitChildren(t *testing.T) {
	isolateGitConfigEnv(t)
	swapInstallProcessGitConfig(t, gitops.InstallProcessGitConfig)
	repo := maskedMaintenanceRepo(t)
	if got := inheritedGitConfig(t, repo, "maintenance.auto"); got != "true" {
		t.Fatalf("precondition: maintenance.auto = %q before run(), want the repo-local true", got)
	}
	_ = run(nil, &bytes.Buffer{})
	if got := inheritedGitConfig(t, repo, "maintenance.auto"); got != "false" {
		t.Errorf("maintenance.auto = %q in an inherited-env child after run(), want false", got)
	}
	if got := inheritedGitConfig(t, repo, "gc.auto"); got != "0" {
		t.Errorf("gc.auto = %q in an inherited-env child after run(), want 0", got)
	}
	// Presence plus GIT_CONFIG_COUNT consistency, not fixed indices.
	n, err := strconv.Atoi(os.Getenv("GIT_CONFIG_COUNT"))
	if err != nil || n < 2 {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want a count covering the pins", os.Getenv("GIT_CONFIG_COUNT"))
	}
	for i := 0; i < n; i++ {
		if _, ok := os.LookupEnv("GIT_CONFIG_KEY_" + strconv.Itoa(i)); !ok {
			t.Errorf("GIT_CONFIG_KEY_%d absent under GIT_CONFIG_COUNT=%d", i, n)
		}
	}
}

// TestOpenPR_PassesStageSnapshotKey: the implement push hands CommitAndPush
// the stage id as its SnapshotKey, so the pre-commit snapshot lives at
// refs/fishhawk/stash/<stage> rather than on the shared refs/stash stack.
// Crosses the runner -> gitops seam through run().
func TestOpenPR_PassesStageSnapshotKey(t *testing.T) {
	swapInstallProcessGitConfig(t, func() error { return nil })
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	stageID := "22222222-3333-4444-5555-777777777777"
	fu.promptResp = &upload.FetchedPrompt{StageID: stageID, StageType: "implement", Prompt: "implement", PromptHash: "h"}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	withFakeGitOps(t, fp, &fakePROpener{})

	var stderr strings.Builder
	if got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", stageID,
		"--fetch-prompt",
		"--upload-trace",
	}, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fp.gotArgs == nil {
		t.Fatal("CommitAndPush not called")
	}
	if fp.gotArgs.SnapshotKey != stageID {
		t.Errorf("CommitAndPushArgs.SnapshotKey = %q, want the stage id %q", fp.gotArgs.SnapshotKey, stageID)
	}
}

// gitChildCall is one recorded git invocation: its argv and its effective env.
type gitChildCall struct {
	argv string
	env  map[string]string
}

// recordGitChildren puts a `git` wrapper first on PATH that appends each
// invocation's argv and FULL environment to a dump file and then execs the
// real git, so a test can read the env a production git child actually ran
// under. It returns a reader over the recorded calls. PATH is set with
// t.Setenv, so the calling test cannot be parallel.
func recordGitChildren(t *testing.T) func() []gitChildCall {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	dump := filepath.Join(dir, "calls")
	script := "#!/bin/sh\n" +
		"{ printf 'ARGV %s\\n' \"$*\"; env; printf '\\036\\n'; } >> '" + dump + "'\n" +
		"exec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []gitChildCall {
		raw, err := os.ReadFile(dump)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		var calls []gitChildCall
		for _, rec := range strings.Split(string(raw), "\x1e\n") {
			lines := strings.Split(rec, "\n")
			argv, ok := strings.CutPrefix(lines[0], "ARGV ")
			if !ok {
				continue
			}
			c := gitChildCall{argv: argv, env: map[string]string{}}
			for _, l := range lines[1:] {
				if name, val, ok := strings.Cut(l, "="); ok {
					c.env[name] = val
				}
			}
			calls = append(calls, c)
		}
		return calls
	}
}

// assertLockRetryLocale asserts every recorded call whose argv contains match
// ran under LC_ALL=C and LANG=C, and returns how many matched.
func assertLockRetryLocale(t *testing.T, calls []gitChildCall, match string) int {
	t.Helper()
	n := 0
	for _, c := range calls {
		if !strings.Contains(c.argv, match) {
			continue
		}
		n++
		if c.env["LC_ALL"] != "C" || c.env["LANG"] != "C" {
			t.Errorf("git %s ran with LC_ALL=%q LANG=%q, want C/C (the lock classifier matches only the untranslated message)",
				c.argv, c.env["LC_ALL"], c.env["LANG"])
		}
	}
	return n
}

// pinHostLocale sets a non-C locale in the test process, so a git child that
// does NOT carry the lock-retry locale pin is observably different.
func pinHostLocale(t *testing.T) {
	t.Helper()
	t.Setenv("LC_ALL", "en_US.UTF-8")
	t.Setenv("LANG", "en_US.UTF-8")
}
