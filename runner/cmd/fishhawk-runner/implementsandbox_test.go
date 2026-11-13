package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agentenv"
	"github.com/kuhlman-labs/fishhawk/runner/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/netsandbox"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// runTestMainImplementSignalStubReason is the reason runTestMain's
// package-wide probe stub reports (#4858, R6).
const runTestMainImplementSignalStubReason = "runTestMain stub: no sandbox-exec spawn (#4858)"

// signalWrapper is the ExecWrapper an applied configuration must carry.
func signalWrapper() []string { return netsandbox.Wrap(nil, netsandbox.SignalProfile()) }

// withImplementSignalProbe stubs the probe seam for one test and returns a
// pointer to its call count.
func withImplementSignalProbe(t *testing.T, available bool, reason string) *int {
	t.Helper()
	calls := 0
	orig := probeImplementSignalSandbox
	probeImplementSignalSandbox = func(context.Context) (bool, string) {
		calls++
		return available, reason
	}
	t.Cleanup(func() { probeImplementSignalSandbox = orig })
	return &calls
}

// withRealImplementSignalProbe opts one test into the REAL host probe.
func withRealImplementSignalProbe(t *testing.T) {
	t.Helper()
	orig := probeImplementSignalSandbox
	probeImplementSignalSandbox = netsandbox.ProbeSignalConfinement
	t.Cleanup(func() { probeImplementSignalSandbox = orig })
}

// eventLines returns every stderr JSON line whose "event" is name.
func eventLines(t *testing.T, log, name string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(log, "\n") {
		if !strings.HasPrefix(l, "{") {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil {
			continue
		}
		if m["event"] == name {
			out = append(out, m)
		}
	}
	return out
}

// signalTraceEvents returns every implement_signal_sandbox payload in lines.
func signalTraceEvents(t *testing.T, lines []bundle.Line) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range lines {
		if l.Kind != "implement_signal_sandbox" {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(l.Data, &p); err != nil {
			t.Fatalf("decode implement_signal_sandbox payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// (a) Every policy branch: exactly one event, its fields, the wrapper, the
// trace payload and the fail reason.
func TestConfigureImplementSignalSandbox_Branches(t *testing.T) {
	cases := []struct {
		name, mode     string
		available      bool
		wantEvent      string
		wantFields     map[string]any
		wantWrapper    []string
		wantFail       string
		wantProbeCalls int
		wantTrace      map[string]any // nil = not asserted (failing branch)
	}{
		{name: "invalid", mode: "sometimes", wantEvent: "implement_signal_sandbox_config",
			wantFields: map[string]any{"var": implementSignalSandboxEnvVar}, wantFail: "implement_signal_sandbox_config"},
		{name: "off", mode: "off", wantEvent: "implement_signal_sandbox_disabled",
			wantFields: map[string]any{"mode": "off", "signals_confined": false},
			wantTrace:  map[string]any{"outcome": "disabled", "mode": "off", "signals_confined": false}},
		{name: "auto unavailable", mode: "", wantEvent: "implement_signal_sandbox_unavailable", wantProbeCalls: 1,
			wantFields: map[string]any{"mode": "auto", "reason": "no seatbelt here", "signals_confined": false},
			wantTrace:  map[string]any{"outcome": "unavailable", "mode": "auto", "reason": "no seatbelt here", "signals_confined": false}},
		{name: "require unavailable", mode: "require", wantEvent: "implement_signal_sandbox_required", wantProbeCalls: 1,
			wantFields: map[string]any{"mode": "require", "reason": "no seatbelt here"}, wantFail: "implement_signal_sandbox_required"},
		{name: "auto available", mode: "auto", available: true, wantEvent: "implement_signal_sandbox_applied", wantProbeCalls: 1,
			wantFields:  map[string]any{"mode": "auto", "mechanism": "seatbelt", "signals_confined": true},
			wantWrapper: signalWrapper(),
			wantTrace:   map[string]any{"outcome": "applied", "mode": "auto", "signals_confined": true}},
		{name: "require available", mode: "require", available: true, wantEvent: "implement_signal_sandbox_applied", wantProbeCalls: 1,
			wantFields:  map[string]any{"mode": "require", "mechanism": "seatbelt", "signals_confined": true},
			wantWrapper: signalWrapper(),
			wantTrace:   map[string]any{"outcome": "applied", "mode": "require", "signals_confined": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			probe := func(context.Context) (bool, string) {
				calls++
				if tc.available {
					return true, ""
				}
				return false, "no seatbelt here"
			}
			getenv := func(k string) string {
				if k == implementSignalSandboxEnvVar {
					return tc.mode
				}
				return ""
			}
			var log bytes.Buffer
			out, fail, detail := configureImplementSignalSandbox(context.Background(), getenv, probe, &log)
			lines := strings.Split(strings.TrimSpace(log.String()), "\n")
			if len(lines) != 1 {
				t.Fatalf("logged %d lines, want exactly one:\n%s", len(lines), log.String())
			}
			var ev map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
				t.Fatalf("event %q is not JSON: %v", lines[0], err)
			}
			if ev["event"] != tc.wantEvent {
				t.Errorf("event = %v, want %s", ev["event"], tc.wantEvent)
			}
			for k, v := range tc.wantFields {
				if ev[k] != v {
					t.Errorf("event field %s = %v, want %v (%s)", k, ev[k], v, lines[0])
				}
			}
			if fail != tc.wantFail {
				t.Errorf("failReason = %q, want %q", fail, tc.wantFail)
			}
			if tc.wantFail != "" && detail == "" {
				t.Error("a failing branch must carry a detail")
			}
			if !reflect.DeepEqual(out.wrapper, tc.wantWrapper) {
				t.Errorf("wrapper = %q, want %q", out.wrapper, tc.wantWrapper)
			}
			if calls != tc.wantProbeCalls {
				t.Errorf("probe calls = %d, want %d", calls, tc.wantProbeCalls)
			}
			if tc.wantTrace != nil {
				ev := out.traceEvent()
				if ev.Kind != "implement_signal_sandbox" {
					t.Errorf("trace kind = %q", ev.Kind)
				}
				var p map[string]any
				_ = json.Unmarshal(ev.Payload, &p)
				if !reflect.DeepEqual(p, tc.wantTrace) {
					t.Errorf("trace payload = %v, want %v", p, tc.wantTrace)
				}
			}
		})
	}
}

// recordingInvoker returns a fakeInvoker that appends every Invocation it
// receives to *got.
func recordingInvoker(f *fakeInvoker, got *[]agent.Invocation) *fakeInvoker {
	prev := f.onInvoke
	f.onInvoke = func(i int, inv agent.Invocation) {
		*got = append(*got, inv)
		if prev != nil {
			prev(i, inv)
		}
	}
	return f
}

func assertAllWrapped(t *testing.T, invs []agent.Invocation, want int) {
	t.Helper()
	if len(invs) != want {
		t.Fatalf("recorded %d Invocations, want %d", len(invs), want)
	}
	for i, inv := range invs {
		if !reflect.DeepEqual(inv.ExecWrapper, signalWrapper()) {
			t.Errorf("Invocation %d ExecWrapper = %q, want the signal-sandbox wrapper", i, inv.ExecWrapper)
		}
	}
}

// assertOneTrace asserts the shipped raw bundle carries exactly ONE
// implement_signal_sandbox event equal to want (R8).
func assertOneTrace(t *testing.T, fu *fakeUploader, want map[string]any) {
	t.Helper()
	_, lines := shippedRaw(t, fu)
	evs := signalTraceEvents(t, lines)
	if len(evs) != 1 || !reflect.DeepEqual(evs[0], want) {
		t.Errorf("bundle implement_signal_sandbox events = %v, want exactly one %v", evs, want)
	}
}

var appliedTrace = map[string]any{"outcome": "applied", "mode": "auto", "signals_confined": true}

// (b) run() wires the wrapper onto EVERY derived spawn path, asserted on the
// Invocation each spawn actually received.
func TestRun_ImplementSpawnCarriesSignalSandboxWrapper(t *testing.T) {
	implementPrompt := func() *upload.FetchedPrompt {
		return &upload.FetchedPrompt{StageID: fixupResumeStageID, StageType: "implement", Prompt: "IMPLEMENT", PromptHash: "h"}
	}

	t.Run("primary implement", func(t *testing.T) {
		implementEnv(t, "kuhlman-labs/fishhawk", "main")
		withImplementSignalProbe(t, true, "")
		const runID = "48580001-0000-4000-8000-000000000001"
		cwdLineage(t, runID)
		var invs []agent.Invocation
		withFakeInvoker(t, recordingInvoker(&fakeInvoker{canned: agent.Result{OK: true}}, &invs))
		fu := newFakeUploader(t)
		fu.promptResp = implementPrompt()
		withFakeUploader(t, fu)
		withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
		var stderr strings.Builder
		if got := run(fixupResumeArgs(runID), &stderr); got != exitOK {
			t.Fatalf("run = %d:\n%s", got, stderr.String())
		}
		assertAllWrapped(t, invs, 1)
		assertOneTrace(t, fu, appliedTrace)
		if n := len(eventLines(t, stderr.String(), "implement_signal_sandbox_applied")); n != 1 {
			t.Errorf("implement_signal_sandbox_applied lines = %d, want 1", n)
		}
	})

	t.Run("fix-up dispatch", func(t *testing.T) {
		implementEnv(t, "kuhlman-labs/fishhawk", "main")
		withImplementSignalProbe(t, true, "")
		const runID = "48580002-0000-4000-8000-000000000001"
		cwdLineage(t, runID)
		var invs []agent.Invocation
		withFakeInvoker(t, recordingInvoker(&fakeInvoker{canned: agent.Result{OK: true}}, &invs))
		fu := newFakeUploader(t)
		fu.promptResp = fixupRespFor("FIXUP")
		withFakeUploader(t, fu)
		withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
		var stderr strings.Builder
		if got := run(fixupResumeArgs(runID), &stderr); got != exitOK {
			t.Fatalf("run = %d:\n%s", got, stderr.String())
		}
		assertAllWrapped(t, invs, 1)
		assertOneTrace(t, fu, appliedTrace)
	})

	t.Run("fix-up resume", func(t *testing.T) {
		implementEnv(t, "kuhlman-labs/fishhawk", "main")
		withImplementSignalProbe(t, true, "")
		const runID = "48580003-0000-4000-8000-000000000001"
		wt, ledger := cwdLineage(t, runID)
		seedLedger(t, ledger, runID, wt, "S1")
		var invs []agent.Invocation
		withFakeInvoker(t, recordingInvoker(&fakeInvoker{canned: agent.Result{OK: true, SessionID: "S1"}}, &invs))
		fu := newFakeUploader(t)
		fu.promptResp = fixupRespFor("FIXUP")
		withFakeUploader(t, fu)
		withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
		var stderr strings.Builder
		if got := run(fixupResumeArgs(runID), &stderr); got != exitOK {
			t.Fatalf("run = %d:\n%s", got, stderr.String())
		}
		assertAllWrapped(t, invs, 1)
		if invs[0].ResumeSessionID != "S1" {
			t.Errorf("ResumeSessionID = %q, want S1 (the resume path is the one under test)", invs[0].ResumeSessionID)
		}
	})

	t.Run("resume-failed fallback", func(t *testing.T) {
		implementEnv(t, "kuhlman-labs/fishhawk", "main")
		withImplementSignalProbe(t, true, "")
		const runID = "48580004-0000-4000-8000-000000000001"
		wt, ledger := cwdLineage(t, runID)
		seedLedger(t, ledger, runID, wt, "S1")
		var invs []agent.Invocation
		withFakeInvoker(t, recordingInvoker(&fakeInvoker{
			cannedSeq: []agent.Result{{OK: false, FailureCategory: "A"}, {OK: true, SessionID: "S2"}},
			errSeq:    []error{fmt.Errorf("%w: no such session", agent.ErrSessionResumeFailed), nil},
		}, &invs))
		fu := newFakeUploader(t)
		fu.promptResp = fixupRespFor("FIXUP")
		withFakeUploader(t, fu)
		withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
		var stderr strings.Builder
		if got := run(fixupResumeArgs(runID), &stderr); got != exitOK {
			t.Fatalf("run = %d:\n%s", got, stderr.String())
		}
		assertAllWrapped(t, invs, 2)
		if invs[0].ResumeSessionID != "S1" || invs[1].ResumeSessionID != "" {
			t.Errorf("resume ids = %q, %q; want S1 then fresh", invs[0].ResumeSessionID, invs[1].ResumeSessionID)
		}
	})

	t.Run("verify-fix re-invoke", func(t *testing.T) {
		withImplementSignalProbe(t, true, "")
		repo := verifyFixBaseRepo(t)
		implementEnv(t, "kuhlman-labs/fishhawk", "main")
		regPath := filepath.Join(repo, "mod", "reg.go")
		var invs []agent.Invocation
		withFakeInvoker(t, recordingInvoker(&fakeInvoker{
			mirrorWorkingTreeFrom: repo,
			canned:                agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
			onInvoke: func(idx int, _ agent.Invocation) {
				if idx == 0 {
					mustWrite(t, regPath, regGetBuggy)
					mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)
				} else {
					mustWrite(t, regPath, regGetFixed)
				}
			},
		}, &invs))
		fu := newFakeUploader(t)
		resp := fixupVerifyLandingPrompt()
		resp.VerifyCommand = "cd mod && go test ./..."
		resp.VerifyMaxIterations = 2
		resp.ScopeFiles = []upload.ScopeFile{
			{Path: "mod/reg.go", Operation: "create"},
			{Path: "mod/reg_test.go", Operation: "create"},
		}
		fu.promptResp = resp
		withFakeUploader(t, fu)
		withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
		var stderr strings.Builder
		if got := run(verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")), &stderr); got != exitOK {
			t.Fatalf("run = %d:\n%s", got, stderr.String())
		}
		assertAllWrapped(t, invs, 2)
	})

	t.Run("base-rebase re-invoke", func(t *testing.T) {
		implementEnv(t, "kuhlman-labs/fishhawk", "main")
		withImplementSignalProbe(t, true, "")
		var invs []agent.Invocation
		fi := recordingInvoker(&fakeInvoker{canned: agent.Result{OK: true}}, &invs)
		withFakeInvoker(t, fi)
		fu := newFakeUploader(t)
		fu.promptResp = &upload.FetchedPrompt{StageID: "22222222-3333-4444-5555-666666666666", StageType: "implement", Prompt: "implement", PromptHash: "h"}
		withFakeUploader(t, fu)
		conflict := &gitops.BaseRebaseConflictError{ConflictPaths: []string{"registry.txt"}, ConflictHunks: "++<<<<<<<", StashPatch: "+x"}
		withFakeGitOps(t, &fakePusher{errSeq: []error{conflict, nil}}, &fakePROpener{})
		var stderr strings.Builder
		if got := runFixupStage(t, &stderr); got != exitOK {
			t.Fatalf("run = %d:\n%s", got, stderr.String())
		}
		if fi.callIdx != 2 {
			t.Fatalf("invoker calls = %d, want 2 (base invoke + base-rebase re-invoke)", fi.callIdx)
		}
		assertAllWrapped(t, invs, 2)
	})

	t.Run("applied fix-up spawns nothing and never probes", func(t *testing.T) {
		implementEnv(t, "kuhlman-labs/fishhawk", "main")
		calls := withImplementSignalProbe(t, true, "")
		var invs []agent.Invocation
		withFakeInvoker(t, recordingInvoker(&fakeInvoker{canned: agent.Result{OK: true}}, &invs))
		fu := newFakeUploader(t)
		fu.promptResp = &upload.FetchedPrompt{
			StageID: "22222222-3333-4444-5555-666666666666", StageType: "implement", Prompt: "implement", PromptHash: "h",
			Fixup: true, FixupBranch: "fishhawk/run-11111111/stage-22222222", FixupExpectedHeadSHA: "fixup-branch-tip-sha",
			VerifyCommand: "true", FixupApplyPatches: []upload.FixupApplyPatch{{Patch: "diff1"}},
		}
		withFakeUploader(t, fu)
		withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
		swapFixupApplySeams(t, nil, "tree-applied", nil, nil)
		var stderr strings.Builder
		if got := runFixupStage(t, &stderr); got != exitOK {
			t.Fatalf("run = %d:\n%s", got, stderr.String())
		}
		if len(invs) != 0 {
			t.Fatalf("agent invoked %d times on an applied fix-up", len(invs))
		}
		if *calls != 0 {
			t.Errorf("probe ran %d times on a no-spawn stage, want 0 (D4)", *calls)
		}
		if strings.Contains(stderr.String(), `"event":"implement_signal_sandbox_`) {
			t.Errorf("a no-spawn stage logged a sandbox configuration event:\n%s", stderr.String())
		}
		assertOneTrace(t, fu, map[string]any{"outcome": "not_spawned"})
	})

	t.Run("plan stage stays unwrapped", func(t *testing.T) {
		calls := withImplementSignalProbe(t, true, "")
		var invs []agent.Invocation
		withFakeInvoker(t, recordingInvoker(&fakeInvoker{canned: agent.Result{OK: true}}, &invs))
		withFakeUploader(t, newFakeUploader(t)) // default prompt is a plan stage
		withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
		var stderr strings.Builder
		_ = runFixupStage(t, &stderr)
		if len(invs) == 0 {
			t.Fatalf("plan stage never invoked the agent:\n%s", stderr.String())
		}
		for i, inv := range invs {
			if inv.ExecWrapper != nil {
				t.Errorf("plan Invocation %d ExecWrapper = %q, want nil", i, inv.ExecWrapper)
			}
		}
		if *calls != 0 || strings.Contains(stderr.String(), "implement_signal_sandbox") {
			t.Errorf("plan stage probed (%d) or logged a signal-sandbox event:\n%s", *calls, stderr.String())
		}
	})

	t.Run("require unavailable fails category C with zero spawns", func(t *testing.T) {
		implementEnv(t, "kuhlman-labs/fishhawk", "main")
		withImplementSignalProbe(t, false, "host cannot confine")
		t.Setenv(implementSignalSandboxEnvVar, "require")
		fi := &fakeInvoker{canned: agent.Result{OK: true}}
		withFakeInvoker(t, fi)
		fu := newFakeUploader(t)
		fu.promptResp = implementPrompt()
		withFakeUploader(t, fu)
		withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
		var stderr strings.Builder
		if got := runFixupStage(t, &stderr); got != exitFailure {
			t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
		}
		if fi.callIdx != 0 {
			t.Errorf("agent spawned %d times under require+unavailable, want 0", fi.callIdx)
		}
		fails := eventLines(t, stderr.String(), "runner_failed")
		if len(fails) != 1 || fails[0]["reason"] != "implement_signal_sandbox_required" || fails[0]["category"] != "C" ||
			fails[0]["detail"] != "host cannot confine" {
			t.Errorf("runner_failed = %v, want one implement_signal_sandbox_required/C with the probe reason", fails)
		}
	})

	t.Run("auto unavailable spawns unwrapped and traces it", func(t *testing.T) {
		implementEnv(t, "kuhlman-labs/fishhawk", "main")
		const runID = "48580005-0000-4000-8000-000000000001"
		cwdLineage(t, runID)
		// The runTestMain stub (no withImplementSignalProbe): the default seam.
		var invs []agent.Invocation
		withFakeInvoker(t, recordingInvoker(&fakeInvoker{canned: agent.Result{OK: true}}, &invs))
		fu := newFakeUploader(t)
		fu.promptResp = implementPrompt()
		withFakeUploader(t, fu)
		withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
		var stderr strings.Builder
		if got := run(fixupResumeArgs(runID), &stderr); got != exitOK {
			t.Fatalf("run = %d:\n%s", got, stderr.String())
		}
		if len(invs) != 1 || invs[0].ExecWrapper != nil {
			t.Fatalf("Invocations = %d, wrapper = %q; want one unwrapped spawn", len(invs), invs[0].ExecWrapper)
		}
		assertOneTrace(t, fu, map[string]any{"outcome": "unavailable", "mode": "auto",
			"reason": runTestMainImplementSignalStubReason, "signals_confined": false})
		if n := len(eventLines(t, stderr.String(), "implement_signal_sandbox_unavailable")); n != 1 {
			t.Errorf("implement_signal_sandbox_unavailable lines = %d, want 1", n)
		}
	})
}

// (c) The cross-boundary e2e: the REAL probe, every REAL adapter, sandbox-exec,
// the agent's /bin/sh and the kernel. The fake agent tries to signal
// throwaway victims the test started UNSANDBOXED; under the wrapper every
// outside signal is refused with EPERM and every victim survives, while the
// agent's own child stays killable (143). Under off the same kills land.
func TestImplementAgentSpawn_SignalConfined_E2E(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt signal confinement is macOS-only: " + netsandbox.UnavailableSignalNonDarwin)
	}
	withRealImplementSignalProbe(t)
	if ok, reason := probeImplementSignalSandbox(context.Background()); !ok {
		t.Skipf("host cannot enforce the signal profile: %s", reason)
	}
	ids, err := selectInvokerCaseLabels("selectInvoker")
	if err != nil || len(ids) == 0 {
		t.Fatalf("enumerating selectInvoker's adapters: %v (%d)", err, len(ids))
	}
	for _, id := range ids {
		for _, mode := range []string{"auto", "off"} {
			t.Run(id+"/"+mode, func(t *testing.T) {
				t.Setenv(implementSignalSandboxEnvVar, mode)
				sbx, fail, _ := configureImplementSignalSandbox(context.Background(), os.Getenv, probeImplementSignalSandbox, io.Discard)
				if fail != "" {
					t.Fatalf("configure failed: %s", fail)
				}
				confined := mode == "auto"
				if confined != (sbx.wrapper != nil) {
					t.Fatalf("mode %s wrapper = %q", mode, sbx.wrapper)
				}
				runSignalE2E(t, id, sbx.wrapper, confined)
			})
		}
	}
}

type e2eVictim struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func startVictim(t *testing.T, path string, args ...string) *e2eVictim {
	t.Helper()
	cmd := exec.Command(path, args...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start victim %s: %v", path, err)
	}
	v := &e2eVictim{cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(v.done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-v.done })
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("victim %s not alive at start: %v", path, err)
	}
	return v
}

func (v *e2eVictim) diedWithin(d time.Duration) bool {
	select {
	case <-v.done:
		return true
	case <-time.After(d):
		return false
	}
}

func runSignalE2E(t *testing.T, id string, wrapper []string, confined bool) {
	dir := t.TempDir()
	unique := strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 10)
	vTerm := startVictim(t, "/bin/sleep", "3600")
	vZero := startVictim(t, "/bin/sleep", "3600")
	vPkill := startVictim(t, "/bin/sleep", "3600."+unique)
	link := filepath.Join(dir, "fhv"+unique)
	if err := os.Symlink("/bin/sleep", link); err != nil {
		t.Fatal(err)
	}
	vKillall := startVictim(t, link, "3600")

	out := filepath.Join(dir, "out.txt")
	script := fmt.Sprintf(`#!/bin/sh
exec >'%[1]s' 2>&1
/bin/kill -TERM %[2]d; echo "term=$?"
/bin/kill -0 %[3]d; echo "zero=$?"
/usr/bin/pkill -f '^/bin/sleep 3600\.%[4]s$'; echo "pkill=$?"
/usr/bin/killall 'fhv%[4]s'; echo "killall=$?"
/bin/sleep 30 & c=$!
/bin/kill -TERM $c; wait $c; echo "own=$?"
exit 0
`, out, vTerm.cmd.Process.Pid, vZero.cmd.Process.Pid, unique)
	bin := filepath.Join(dir, "fake-agent")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // executable fixture
		t.Fatal(err)
	}
	invoker, err := selectInvoker(id, "", bin)
	if err != nil {
		t.Fatalf("selectInvoker(%q): %v", id, err)
	}
	base, _ := agentenv.Env(os.Environ())
	_, _ = invoker.Invoke(context.Background(), agent.Invocation{
		RunID: "run-e2e-" + id, Stage: "implement", Prompt: "signal test",
		WorkingDir: t.TempDir(), BaseEnv: base, ExecWrapper: wrapper,
		Budget: agent.Budget{Timeout: 60 * time.Second},
	})
	data, err := os.ReadFile(out) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("the fake agent never wrote its results (%v); the spawn under test did not happen", err)
	}
	got := string(data)
	if !strings.Contains(got, "own=143") {
		t.Errorf("own child status not 143 — the agent must keep signalling its own tree:\n%s", got)
	}
	victims := map[string]*e2eVictim{"kill -TERM": vTerm, "pkill": vPkill, "killall": vKillall}
	if confined {
		if n := strings.Count(got, "Operation not permitted"); n < 2 {
			t.Errorf("want EPERM text for the kill -TERM and kill -0 attempts, got %d:\n%s", n, got)
		}
		for _, k := range []string{"term=0", "zero=0", "pkill=0", "killall=0"} {
			if strings.Contains(got, k) {
				t.Errorf("an outside signal succeeded under the wrapper (%s):\n%s", k, got)
			}
		}
		for name, v := range victims {
			if v.diedWithin(300 * time.Millisecond) {
				t.Errorf("victim of %s died under the signal sandbox:\n%s", name, got)
			}
		}
		return
	}
	// Counterfactual (off): the same kills land.
	for name, v := range victims {
		if !v.diedWithin(5 * time.Second) {
			t.Errorf("victim of %s survived WITHOUT the wrapper — the e2e is not discriminating:\n%s", name, got)
		}
	}
	if !strings.Contains(got, "zero=0") {
		t.Errorf("kill -0 failed without the wrapper:\n%s", got)
	}
}

// (d) The production conflict-resolution invoker carries the wrapper.
func TestConflictResolutionInvokerCarriesSignalWrapper(t *testing.T) {
	promptFile := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(promptFile, []byte("resolve"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config{agent: "claude-code", runID: "r", stage: "implement", promptFile: promptFile, workingDir: t.TempDir(), timeout: time.Minute}
	for _, tc := range []struct {
		name      string
		available bool
		want      []string
	}{{"available", true, signalWrapper()}, {"auto unavailable", false, nil}} {
		t.Run(tc.name, func(t *testing.T) {
			withImplementSignalProbe(t, tc.available, "nope")
			fi := &fakeInvoker{canned: agent.Result{OK: true}}
			withFakeInvoker(t, fi)
			if err := productionConflictResolutionAgentInvoker(context.Background(), cfg, io.Discard); err != nil {
				t.Fatalf("invoker: %v", err)
			}
			if fi.gotInv == nil {
				t.Fatal("the production invoker never reached the fake")
			}
			if !reflect.DeepEqual(fi.gotInv.ExecWrapper, tc.want) {
				t.Fatalf("recorded ExecWrapper = %q, want %q", fi.gotInv.ExecWrapper, tc.want)
			}
		})
	}
}

// (e) R1: require on a host that cannot confine fails the conflict-resolution
// stage category C with zero agent spawns, the merge aborted, and a
// category-carrying runner_failed.
func TestConflictResolutionStage_SignalSandboxRequiredFailsCategoryC(t *testing.T) {
	dispatch, _, featureTip, _ := crDispatchRepo(t)
	fp := &fakePusher{}
	crStageHarness(t, fp, crResolveInWorkdir)
	conflictResolutionAgentInvoker = productionConflictResolutionAgentInvoker // restored by crStageHarness's cleanup
	fi := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, fi)
	t.Setenv(implementSignalSandboxEnvVar, "require")
	withImplementSignalProbe(t, false, "host cannot confine")

	cfg := crStageCfg(dispatch)
	cfg.agent = "claude-code"
	// A readable prompt, so that without the refusal the invoker reaches the
	// fake (callIdx 1) instead of failing on the prompt read — the refusal is
	// then the ONLY thing between the pass and a spawn.
	cfg.promptFile = filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(cfg.promptFile, []byte("resolve"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &crFakeUpload{}
	var log bytes.Buffer
	code := runConflictResolutionStage(context.Background(), cfg, crDispatchRequest(featureTip),
		client, &upload.IssuedKey{PrivateKey: make([]byte, 64)}, &log)
	if code != exitFailure {
		t.Fatalf("exit = %d, want exitFailure:\n%s", code, log.String())
	}
	if fi.callIdx != 0 {
		t.Errorf("agent spawned %d times, want 0", fi.callIdx)
	}
	if fp.pushCommittedArgs != nil {
		t.Error("a refused pass pushed")
	}
	if len(client.ships) != 1 {
		t.Fatalf("ships = %d, want one terminal report", len(client.ships))
	}
	got := client.ships[0]
	if got.Outcome != "failed" || got.Category != "C" || !strings.HasPrefix(got.Reason, "implement_signal_sandbox_required") {
		t.Errorf("report = %q/%q/%q, want failed/C/implement_signal_sandbox_required…", got.Outcome, got.Category, got.Reason)
	}
	if strings.Contains(got.Reason, "[RECOVERY INCOMPLETE") {
		t.Errorf("the merge was not aborted: %q", got.Reason)
	}
	fails := eventLines(t, log.String(), "runner_failed")
	if len(fails) != 1 || fails[0]["category"] != "C" || fails[0]["reason"] != "implement_signal_sandbox_required" {
		t.Errorf("runner_failed = %v, want one category C implement_signal_sandbox_required", fails)
	}
}

// (f) The policy variable is runner config: it never reaches the agent env.
func TestImplementSignalSandboxVar_NotInAgentEnv(t *testing.T) {
	env, _ := agentenv.Env([]string{implementSignalSandboxEnvVar + "=require", "PATH=/usr/bin"})
	for _, kv := range env {
		if strings.HasPrefix(kv, implementSignalSandboxEnvVar+"=") {
			t.Fatalf("agent env carries %s", kv)
		}
	}
	if len(env) == 0 {
		t.Fatal("agentenv dropped everything; the assertion would be vacuous")
	}
}

// (g) runTestMain stubs the package-default seam (R6).
func TestMainStubsImplementSignalProbe(t *testing.T) {
	ok, reason := probeImplementSignalSandbox(context.Background())
	if ok || reason != runTestMainImplementSignalStubReason {
		t.Fatalf("default probe = %v/%q, want the runTestMain stub %q", ok, reason, runTestMainImplementSignalStubReason)
	}
	if v, set := os.LookupEnv(implementSignalSandboxEnvVar); set {
		t.Fatalf("%s=%q leaked into the test binary; runTestMain must unset it", implementSignalSandboxEnvVar, v)
	}
}

// R1/D5: both conflict-resolution runner_failed events carry the category —
// the tree-refusal one (here tree_unavailable / C) and the refused-pass one,
// whose historical B is now explicit.
func TestConflictResolutionRunnerFailedCarriesCategory(t *testing.T) {
	t.Run("tree refusal", func(t *testing.T) {
		crFetchRecorder(t)
		crStageHarness(t, &fakePusher{}, crResolveInWorkdir)
		var log bytes.Buffer
		code := runConflictResolutionStage(context.Background(), crStageCfg(t.TempDir()), crDispatchRequest(strings.Repeat("d", 40)),
			&crFakeUpload{}, &upload.IssuedKey{PrivateKey: make([]byte, 64)}, &log)
		fails := eventLines(t, log.String(), "runner_failed")
		if code != exitFailure || len(fails) != 1 || fails[0]["reason"] != reasonTreeUnavailable || fails[0]["category"] != "C" {
			t.Fatalf("exit %d, runner_failed = %v; want one %s with category C", code, fails, reasonTreeUnavailable)
		}
	})
	t.Run("refused pass", func(t *testing.T) {
		dispatch, _, featureTip, _ := crDispatchRepo(t)
		crStageHarness(t, &fakePusher{}, func(config) error { return fmt.Errorf("agent exploded") })
		var log bytes.Buffer
		code := runConflictResolutionStage(context.Background(), crStageCfg(dispatch), crDispatchRequest(featureTip),
			&crFakeUpload{}, &upload.IssuedKey{PrivateKey: make([]byte, 64)}, &log)
		fails := eventLines(t, log.String(), "runner_failed")
		if code != exitFailure || len(fails) != 1 || fails[0]["reason"] != reasonAgentFailed || fails[0]["category"] != "B" {
			t.Fatalf("exit %d, runner_failed = %v; want one %s with category B", code, fails, reasonAgentFailed)
		}
	})
}
