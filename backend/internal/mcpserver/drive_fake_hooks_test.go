package mcpserver

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// driveFakeBackend's four hook fields (onGate/onSpawn/onAudit/onStages) are READ
// and INVOKED by the httptest handler (and, for onSpawn, by the resolver's spawn
// closure) WHILE HOLDING f.mu — see drive_run_test.go handler(): the /auto-drive,
// /stages, and /audit arms Lock f.mu then call the hook, and newDriveResolver's
// driveSpawn Locks f.mu then calls onSpawn. A test goroutine that assigns one of
// those fields WITHOUT the lock therefore has no happens-before edge against a
// handler goroutine already in flight, so the field word is written and read
// concurrently — the #2694 data race that red-lined main under -race. (The race
// is not just on the field word: the assigned closure captures test-local state,
// and those captured variables race too, which is why the original report named
// two distinct addresses.)
//
// The setOnX setters below close the class BY CONSTRUCTION: each assigns its hook
// under f.mu, giving the handler's locked read a happens-before edge against the
// test goroutine's write. Every hook-assignment site in drive_run_test.go goes
// through these; the raw `f.onX = ` form is banned and enforced by
// TestDriveFakeHooks_NoRawHookAssignments.
//
// CONSTRAINT — these setters MUST NOT be called from inside a hook body.
// sync.Mutex is not reentrant, and the handler already holds f.mu when it invokes
// a hook, so a setOnX call from within onGate/onSpawn/onAudit/onStages would
// self-deadlock. To chain hook behaviour, COMPOSE the closures at assignment time
// (call the previous func from the new one) rather than reassigning mid-hook.
func (f *driveFakeBackend) setOnGate(fn func(f *driveFakeBackend) AutoDriveOutcome) {
	f.mu.Lock()
	f.onGate = fn
	f.mu.Unlock()
}

func (f *driveFakeBackend) setOnSpawn(fn func(f *driveFakeBackend, stageType string)) {
	f.mu.Lock()
	f.onSpawn = fn
	f.mu.Unlock()
}

func (f *driveFakeBackend) setOnAudit(fn func(f *driveFakeBackend, category string)) {
	f.mu.Lock()
	f.onAudit = fn
	f.mu.Unlock()
}

func (f *driveFakeBackend) setOnStages(fn func(f *driveFakeBackend)) {
	f.mu.Lock()
	f.onStages = fn
	f.mu.Unlock()
}

// TestDriveFakeHooks_ConcurrentAssignmentIsRaceFree is the deterministic
// counterfactual vehicle for the setOnX synchronization control. The original
// failing test (TestDriveRun_StaleRecoveryConvergence_EndToEnd) is NOT a usable
// vehicle: its race is timing-dependent and does not reproduce locally (per the
// #2694 investigation). This test removes the timing dependence by keeping
// several reader goroutines continuously mid-read on each hook word while the
// test goroutine assigns that word hundreds of times through the setter — so
// under -race a genuine concurrent reader/writer pair exists for EVERY hook, and
// the detector fires the moment any setOnX stops locking.
//
// Empirical race coverage (condition 2, option (a)): all four setters have a
// GENUINE concurrent reader here, not merely by construction. onStages, onAudit,
// and onGate are read by the httptest HANDLER (the /stages, /audit, and
// /auto-drive arms, each under f.mu) driven by real HTTP traffic below. onSpawn
// has no HTTP arm — the resolver's driveSpawn closure is its only reader — so it
// is exercised through that exact read path (an f.mu-guarded `f.onSpawn(f, …)`)
// in dedicated goroutines. The step-8(i) counterfactual deletes setOnStages'
// lock specifically, and the /stages readers make that go red.
func TestDriveFakeHooks_ConcurrentAssignmentIsRaceFree(t *testing.T) {
	f := newDriveFake("running", []Stage{stg(drivePlanID, "plan", "pending", 0)})
	// onGate is invoked UNCONDITIONALLY by the /auto-drive handler (no nil
	// guard), so seed every hook non-nil before any reader starts. Hook bodies
	// are trivial — no setState, no re-entry into a setter — so a reader holding
	// f.mu cannot deadlock.
	f.setOnGate(func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "seed"} })
	f.setOnStages(func(f *driveFakeBackend) {})
	f.setOnAudit(func(f *driveFakeBackend, category string) {})
	f.setOnSpawn(func(f *driveFakeBackend, stageType string) {})

	rec := &spawnRecorder{}
	_, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	id := f.runID.String()
	client := srv.Client()
	base := srv.URL + "/v0/runs/" + id

	const (
		stagesReaders = 4
		auditReaders  = 2
		gateReaders   = 2
		spawnReaders  = 2
		writes        = 2000
	)
	totalReaders := stagesReaders + auditReaders + gateReaders + spawnReaders

	stop := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(totalReaders)
	var wg sync.WaitGroup

	// launch runs read() in a tight loop until stop, signalling ready exactly
	// once (after its first read) so the writer can guarantee every reader is
	// in-flight before it begins assigning — that overlap is what makes the
	// counterfactual redden deterministically rather than by luck.
	launch := func(read func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var once sync.Once
			for {
				select {
				case <-stop:
					return
				default:
				}
				read()
				once.Do(ready.Done)
			}
		}()
	}

	drain := func(url string) {
		resp, err := client.Get(url)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	for i := 0; i < stagesReaders; i++ {
		launch(func() { drain(base + "/stages") }) // handler reads onStages under f.mu
	}
	for i := 0; i < auditReaders; i++ {
		launch(func() { drain(base + "/audit") }) // handler reads onAudit under f.mu
	}
	for i := 0; i < gateReaders; i++ {
		launch(func() { // handler reads onGate under f.mu
			resp, err := client.Post(base+"/auto-drive", "application/json", nil)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		})
	}
	for i := 0; i < spawnReaders; i++ {
		launch(func() {
			// onSpawn's only reader is the resolver's driveSpawn closure, which
			// reads it under f.mu; mirror that exact access here.
			f.mu.Lock()
			if f.onSpawn != nil {
				f.onSpawn(f, "plan")
			}
			f.mu.Unlock()
		})
	}

	// Every reader is now looping mid-read; assign each hook many times with a
	// DISTINCT closure so both the field word and its captured state are written
	// concurrently with the readers above.
	ready.Wait()
	for i := 0; i < writes; i++ {
		i := i
		f.setOnStages(func(f *driveFakeBackend) { _ = i })
		f.setOnAudit(func(f *driveFakeBackend, category string) { _ = i })
		f.setOnGate(func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: fmt.Sprintf("w%d", i)} })
		f.setOnSpawn(func(f *driveFakeBackend, stageType string) { _ = i })
	}

	close(stop)
	wg.Wait()
}

// TestDriveFakeHooks_NoRawHookAssignments is the done-means test for the CLASS
// rather than the one reported line: it fails if any *_test.go in the package
// (except this file) reintroduces a raw driveFakeBackend hook assignment. It is
// RED on the pre-fix tree (91 hits) and RED again on any future raw assignment,
// where a presence-only "the setters exist" check would pass.
//
// The pattern anchors on the FIELD NAME preceded by a `.` (any receiver) and
// followed by ` = ` — NOT the literal receiver `f` — so a future test that binds
// the fake to a differently-named variable (`g := f; g.onStages = …`) cannot
// evade the guard while reintroducing the exact race class. The struct-field
// declarations in the type block (`onGate func(...)`) carry no `.` and no `=`, so
// they are excluded by construction; the setter definitions in THIS file are
// excluded by skipping this file.
func TestDriveFakeHooks_NoRawHookAssignments(t *testing.T) {
	// `.onX =` with the char after `=` not being another `=`, so comparisons
	// (`onStages != nil`, `==`) never match, only assignments do.
	rawAssign := regexp.MustCompile(`\.on(Gate|Spawn|Audit|Stages)\b[ \t]*=([^=]|$)`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, "_test.go") || name == "drive_fake_hooks_test.go" {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if rawAssign.MatchString(line) {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", name, i+1, strings.TrimSpace(line)))
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("raw driveFakeBackend hook assignment(s) found — assign hooks through the setOnX setters (drive_fake_hooks_test.go), which lock f.mu, instead of `f.onX = …`:\n%s",
			strings.Join(offenders, "\n"))
	}
}
