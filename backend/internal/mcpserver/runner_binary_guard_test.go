package mcpserver

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// attackerRunnerBinary is the caller-supplied runner_binary every refusal row
// carries: a path the HTTP transport must never spawn.
const attackerRunnerBinary = "/opt/attacker/payload"

// spawningToolNames is the set of registered tools whose input schema exposes
// runner_binary — the five runner-spawning tools (E66.64 / #4476).
var spawningToolNames = []string{
	"fishhawk_dispatch_stage",
	"fishhawk_drive_run",
	"fishhawk_request_specialist_job",
	"fishhawk_run_children",
	"fishhawk_run_stage",
}

// countingDispatchSpawn swaps the detached dispatch spawn seam for a counter,
// so a dispatch row can prove no runner started.
func countingDispatchSpawn(t *testing.T) *int {
	t.Helper()
	n := new(int)
	saved := dispatchSpawnDetached
	dispatchSpawnDetached = func(string, []string, []string, string, string, detachedFailureReporter, detachedStageStateProbe) (string, error) {
		*n++
		return "/dev/null", nil
	}
	t.Cleanup(func() { dispatchSpawnDetached = saved })
	return n
}

// TestRunnerBinaryRefusedOverHTTP_EverySpawningTool drives the REAL handler of
// every runner-spawning tool over an HTTP-posture resolver with a supplied
// runner_binary and asserts, per row:
//
//   - the named runner_binary_not_allowed_over_http refusal, AND
//   - that NOTHING was committed: zero backend dials (which includes zero
//     specialist-job POSTs), zero runner spawns on every seam, no
//     host-dispatch marker — error identity alone cannot prove a refusal
//     committed nothing, because the resolveRunnerBinary chokepoint returns
//     the same error AFTER the backend reads.
//
// working_dir is the allowed root itself, so confinePath passes and only the
// runner_binary guard can refuse. The specialist row uses spawn=false so the
// chokepoint (reached only when spawning) cannot mask a deleted early guard:
// without the guard that row POSTs the job.
func TestRunnerBinaryRefusedOverHTTP_EverySpawningTool(t *testing.T) {
	root := t.TempDir()

	type seams struct {
		detached   *fakeDetachedSpawn
		dispatch   *int
		drive      *int
		specialist *specialistJobSpawnRecorder
	}
	rows := []struct {
		name string
		call func(fb *fakeBackend, r *runResolver) error
	}{
		{"fishhawk_run_stage", func(fb *fakeBackend, r *runResolver) error {
			runID, stageID := uuid.New(), uuid.New()
			seedStageOfType(fb, runID, stageID, "implement", "pending")
			_, _, err := r.runStage(context.Background(), nil, RunStageInput{
				RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
				WorkingDir: root, GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false), RunnerBinary: attackerRunnerBinary,
			})
			return err
		}},
		{"fishhawk_dispatch_stage", func(fb *fakeBackend, r *runResolver) error {
			runID, stageID := uuid.New(), uuid.New()
			seedStageOfType(fb, runID, stageID, "implement", "pending")
			_, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
				RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
				WorkingDir: root, GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false), RunnerBinary: attackerRunnerBinary,
			})
			return err
		}},
		{"fishhawk_run_children", func(fb *fakeBackend, r *runResolver) error {
			parent, child := uuid.New(), uuid.New()
			seedChildRun(fb, child, "pending")
			seedPlanDecomposed(fb, parent, []string{child.String()}, 0)
			_, _, err := r.runChildren(context.Background(), nil, RunChildrenInput{
				RunID: parent.String(), Workflow: "feature_change", WorkingDir: root, GitHubRepo: "x/y",
				RunnerBinary: attackerRunnerBinary,
			})
			return err
		}},
		{"fishhawk_drive_run", func(fb *fakeBackend, r *runResolver) error {
			runID := uuid.New()
			seedRunWorkingDir(fb, runID, root)
			_, _, err := r.driveRun(context.Background(), nil, DriveRunInput{
				RunID: runID.String(), WorkingDir: root, GitHubRepo: "x/y", RunnerBinary: attackerRunnerBinary,
			})
			return err
		}},
		{"fishhawk_request_specialist_job", func(_ *fakeBackend, r *runResolver) error {
			_, _, err := r.requestSpecialistJob(context.Background(), nil, RequestSpecialistJobInput{
				Kind: "overlap_scan", Subject: SpecialistJobSubject{HeadSHA: strings.Repeat("a", 40)}, Inputs: overlapInputs(),
				WorkingDir: root, RunnerBinary: attackerRunnerBinary, Spawn: boolPtr(false),
			})
			return err
		}},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			fb, r, rec, calls := newConfinementFixture(t, root)
			s := seams{detached: withFakeDetachedSpawn(t), dispatch: countingDispatchSpawn(t), drive: new(int), specialist: &specialistJobSpawnRecorder{}}
			r.specialistJobSpawn = s.specialist.spawn
			var driveMu sync.Mutex
			r.driveSpawn = func(string, []string, []string, string, string, detachedFailureReporter, detachedStageStateProbe) (string, error) {
				driveMu.Lock()
				*s.drive++
				driveMu.Unlock()
				return "/dev/null", nil
			}
			// Bound the drive loop so a deleted guard ends promptly instead of
			// polling the fake backend for the default wall clock.
			r.drivePollInterval = time.Millisecond
			r.driveMaxWallclock = 200 * time.Millisecond

			err := row.call(fb, r)
			if err == nil || !strings.Contains(err.Error(), runnerBinaryNotAllowedCode) {
				t.Errorf("err = %v, want one carrying %q", err, runnerBinaryNotAllowedCode)
			}
			if err != nil && strings.Contains(err.Error(), attackerRunnerBinary) {
				t.Errorf("the refusal echoes the supplied runner_binary: %v", err)
			}
			if dials := rec.recorded(); len(dials) != 0 {
				t.Errorf("the refusal dialed the backend %d time(s): %v; it must refuse before any backend read or POST", len(dials), dials)
			}
			driveMu.Lock()
			driveSpawns := *s.drive
			driveMu.Unlock()
			if n := len(*calls) + len(s.detached.snapshot()) + *s.dispatch + driveSpawns + s.specialist.calls; n != 0 {
				t.Errorf("the refusal spawned %d runner(s); it must spawn none", n)
			}
			fb.mu.Lock()
			hostDispatches := len(fb.hostDispatchCalledByID)
			fb.mu.Unlock()
			if hostDispatches != 0 {
				t.Errorf("the refusal recorded %d host-dispatch marker(s); it must record none", hostDispatches)
			}
		})
	}

	// Completeness: every registered tool exposing runner_binary must have a
	// row above, so a sixth spawning tool cannot ship without the guard's test.
	t.Run("completeness", func(t *testing.T) {
		got := toolsWithInputProperty(t, "runner_binary")
		var want []string
		for _, row := range rows {
			want = append(want, row.name)
		}
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("registered tools exposing runner_binary = %v, but the refusal table covers %v; add a row (and the early refuseHTTPRunnerBinary guard) for every spawning tool", got, want)
		}
	})
}

// toolsWithInputProperty lists, sorted, the registered tools whose wire input
// schema carries the named property.
func toolsWithInputProperty(t *testing.T, prop string) []string {
	t.Helper()
	list, err := connectInMemory(t).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range list.Tools {
		schema, _ := tool.InputSchema.(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if _, ok := props[prop]; ok {
			names = append(names, tool.Name)
		}
	}
	sort.Strings(names)
	return names
}

// TestRunnerBinaryHonoredOnStdio_EverySpawningTool is the accept control for
// the refusal table above: on the STDIO transport a supplied runner_binary is
// not refused and is the binary each tool's spawn seam receives. Without it
// the refusal rows could pass by refusing everything.
func TestRunnerBinaryHonoredOnStdio_EverySpawningTool(t *testing.T) {
	const supplied = "/opt/custom/fishhawk-runner"
	failRunnerResolution(t) // only the supplied rung can resolve

	t.Run("fishhawk_run_stage", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		var got string
		origCmd := runStageCommand
		runStageCommand = func(name string, _ ...string) *exec.Cmd {
			got = name
			return exec.Command("sh", "-c", "exit 0")
		}
		t.Cleanup(func() { runStageCommand = origCmd })
		runID, stageID := uuid.New(), uuid.New()
		seedStage(fb, runID, stageID, "succeeded")
		_, _, err := r.runStage(context.Background(), nil, RunStageInput{
			RunID: runID.String(), StageID: stageID.String(), Workflow: "w", Stage: "plan",
			GitHubRepo: "x/y", RunnerBinary: supplied,
		})
		assertStdioAccepted(t, err, got, supplied)
	})

	t.Run("fishhawk_dispatch_stage", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		var got string
		saved := dispatchSpawnDetached
		dispatchSpawnDetached = func(binary string, _, _ []string, _, _ string, _ detachedFailureReporter, _ detachedStageStateProbe) (string, error) {
			got = binary
			return "/dev/null", nil
		}
		t.Cleanup(func() { dispatchSpawnDetached = saved })
		runID, stageID := uuid.New(), uuid.New()
		seedStageOfType(fb, runID, stageID, "implement", "pending")
		_, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
			RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
			WorkingDir: t.TempDir(), GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false), RunnerBinary: supplied,
		})
		assertStdioAccepted(t, err, got, supplied)
	})

	t.Run("fishhawk_run_children", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		parent, child := uuid.New(), uuid.New()
		seedChildRun(fb, child, "pending")
		seedPlanDecomposed(fb, parent, []string{child.String()}, 0)
		spawn := withFakeDetachedSpawn(t)
		_, _, err := r.runChildren(context.Background(), nil, RunChildrenInput{
			RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", WorkingDir: t.TempDir(), RunnerBinary: supplied,
		})
		var got string
		if calls := spawn.snapshot(); len(calls) == 1 {
			got = calls[0].binary
		}
		assertStdioAccepted(t, err, got, supplied)
	})

	t.Run("fishhawk_drive_run", func(t *testing.T) {
		f := newDriveFake("running", []Stage{
			stg(drivePlanID, "plan", "succeeded", 0),
			stg(driveImplID, "implement", "pending", 1),
		})
		f.setOnSpawn(func(f *driveFakeBackend, typ string) {
			if typ == "implement" {
				f.setState("implement", "succeeded")
				f.runState = "succeeded"
			}
		})
		f.setOnGate(func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} })
		r, srv := newDriveResolver(t, f, &spawnRecorder{})
		defer srv.Close()
		var mu sync.Mutex
		var got string
		r.driveSpawn = func(binary string, _, _ []string, _, stageID string, _ detachedFailureReporter, _ detachedStageStateProbe) (string, error) {
			mu.Lock()
			got = binary
			mu.Unlock()
			typ := f.typeForStageID(stageID)
			f.mu.Lock()
			if f.onSpawn != nil {
				f.onSpawn(f, typ)
			}
			f.mu.Unlock()
			return "/tmp/log", nil
		}
		_, _, err := r.driveRun(context.Background(), nil, DriveRunInput{
			RunID: f.runID.String(), GitHubRepo: "x/y", WorkingDir: t.TempDir(), RunnerBinary: supplied,
		})
		mu.Lock()
		defer mu.Unlock()
		assertStdioAccepted(t, err, got, supplied)
	})

	t.Run("fishhawk_request_specialist_job", func(t *testing.T) {
		_, srv := newSpecialistJobFakeBackend(t, http.StatusCreated, specialistJobBody(uuid.NewString(), "overlap_scan", "queued", false))
		r := newResolver(srv, nil)
		rec := &specialistJobSpawnRecorder{}
		r.specialistJobSpawn = rec.spawn
		_, _, err := r.requestSpecialistJob(context.Background(), nil, RequestSpecialistJobInput{
			Kind: "overlap_scan", Subject: SpecialistJobSubject{HeadSHA: strings.Repeat("a", 40)}, Inputs: overlapInputs(),
			WorkingDir: t.TempDir(), RunnerBinary: supplied,
		})
		assertStdioAccepted(t, err, rec.binary, supplied)
	})
}

func assertStdioAccepted(t *testing.T, err error, gotBinary, supplied string) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), runnerBinaryNotAllowedCode) {
		t.Fatalf("stdio refused a supplied runner_binary: %v", err)
	}
	if err != nil {
		t.Fatalf("stdio call failed: %v", err)
	}
	if gotBinary != supplied {
		t.Errorf("spawned binary = %q, want the supplied %q (stdio resolves the input first)", gotBinary, supplied)
	}
}

// TestRefuseHTTPRunnerBinary pins one case per branch of the shared helper.
func TestRefuseHTTPRunnerBinary(t *testing.T) {
	stdio := &runResolver{}
	overHTTP := &runResolver{httpTransport: true}

	if err := stdio.refuseHTTPRunnerBinary("/opt/fishhawk-runner"); err != nil {
		t.Errorf("stdio + supplied: err = %v, want nil", err)
	}
	if err := overHTTP.refuseHTTPRunnerBinary(""); err != nil {
		t.Errorf("http + empty: err = %v, want nil", err)
	}
	if err := overHTTP.refuseHTTPRunnerBinary("/opt/fishhawk-runner"); err == nil || !strings.Contains(err.Error(), runnerBinaryNotAllowedCode) {
		t.Errorf("http + supplied: err = %v, want %q", err, runnerBinaryNotAllowedCode)
	}
	if err := overHTTP.refuseHTTPRunnerBinary("   "); err == nil || !strings.Contains(err.Error(), runnerBinaryNotAllowedCode) {
		t.Errorf("http + whitespace-only: err = %v, want %q (non-emptiness alone decides)", err, runnerBinaryNotAllowedCode)
	}

	// No existence oracle: an existing executable and a non-existent path draw
	// byte-identical refusals.
	existing := filepath.Join(t.TempDir(), "fishhawk-runner")
	if err := os.WriteFile(existing, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "absent", "fishhawk-runner")
	e1, e2 := overHTTP.refuseHTTPRunnerBinary(existing), overHTTP.refuseHTTPRunnerBinary(missing)
	if e1 == nil || e2 == nil || e1.Error() != e2.Error() {
		t.Errorf("existing vs missing refusals differ (an existence oracle): %v / %v", e1, e2)
	}
}

// TestResolveRunnerBinary_TransportConditional pins the chokepoint branches of
// resolveRunnerBinary, called directly so the early guards cannot mask them.
func TestResolveRunnerBinary_TransportConditional(t *testing.T) {
	t.Run("http_input_refused", func(t *testing.T) {
		r := &runResolver{httpTransport: true, getenv: envFuncFromMap(nil)}
		got, err := r.resolveRunnerBinary(attackerRunnerBinary)
		if err == nil || !strings.Contains(err.Error(), runnerBinaryNotAllowedCode) || got != "" {
			t.Errorf("resolveRunnerBinary over HTTP = (%q, %v), want (\"\", %q)", got, err, runnerBinaryNotAllowedCode)
		}
	})
	t.Run("stdio_input_first", func(t *testing.T) {
		r := &runResolver{getenv: envFuncFromMap(map[string]string{"FISHHAWK_RUNNER_BIN": "/from/env"})}
		got, err := r.resolveRunnerBinary("/explicit/fishhawk-runner")
		if err != nil || got != "/explicit/fishhawk-runner" {
			t.Errorf("resolveRunnerBinary on stdio = (%q, %v), want the input verbatim", got, err)
		}
	})
	t.Run("http_env_rung_still_resolves", func(t *testing.T) {
		failRunnerResolution(t)
		r := &runResolver{httpTransport: true, getenv: envFuncFromMap(map[string]string{"FISHHAWK_RUNNER_BIN": "/daemon/fishhawk-runner"})}
		got, err := r.resolveRunnerBinary("")
		if err != nil || got != "/daemon/fishhawk-runner" {
			t.Errorf("resolveRunnerBinary over HTTP with FISHHAWK_RUNNER_BIN = (%q, %v), want the daemon's env value", got, err)
		}
	})
	t.Run("http_not_found_remedy", func(t *testing.T) {
		failRunnerResolution(t)
		r := &runResolver{httpTransport: true, getenv: envFuncFromMap(nil)}
		_, err := r.resolveRunnerBinary("")
		if err == nil {
			t.Fatal("expected a not-found error")
		}
		msg := err.Error()
		if !strings.HasPrefix(msg, "fishhawk-runner not on PATH") {
			t.Errorf("HTTP not-found error must keep the prefix %q; got %q", "fishhawk-runner not on PATH", msg)
		}
		if !strings.Contains(msg, "FISHHAWK_RUNNER_BIN") {
			t.Errorf("HTTP not-found error must name FISHHAWK_RUNNER_BIN; got %q", msg)
		}
		if strings.Contains(msg, "pass runner_binary") {
			t.Errorf("HTTP not-found error advises a remedy the transport refuses: %q", msg)
		}
	})
	t.Run("stdio_not_found_remedy", func(t *testing.T) {
		failRunnerResolution(t)
		r := &runResolver{getenv: envFuncFromMap(nil)}
		_, err := r.resolveRunnerBinary("")
		if err == nil || !strings.HasPrefix(err.Error(), "fishhawk-runner not on PATH") || !strings.Contains(err.Error(), "pass runner_binary") {
			t.Errorf("stdio not-found error = %v, want the prefix and the pass runner_binary remedy", err)
		}
	})
}

// TestSpawningToolDescriptions_StateRunnerBinaryHTTPRefusal reads each spawning
// tool's WIRE-VISIBLE runner_binary description and requires the refusal code,
// so an agent learns the rule from the tool surface.
func TestSpawningToolDescriptions_StateRunnerBinaryHTTPRefusal(t *testing.T) {
	list, err := connectInMemory(t).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	seen := map[string]bool{}
	for _, tool := range list.Tools {
		schema, _ := tool.InputSchema.(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		rb, ok := props["runner_binary"].(map[string]any)
		if !ok {
			continue
		}
		seen[tool.Name] = true
		if desc, _ := rb["description"].(string); !strings.Contains(desc, runnerBinaryNotAllowedCode) {
			t.Errorf("%s runner_binary description does not name %q: %q", tool.Name, runnerBinaryNotAllowedCode, desc)
		}
	}
	for _, name := range spawningToolNames {
		if !seen[name] {
			t.Errorf("%s exposes no runner_binary property over ListTools", name)
		}
	}
}
