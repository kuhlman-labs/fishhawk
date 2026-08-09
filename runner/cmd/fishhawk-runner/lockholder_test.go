package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// --- #2545 lineage-lock holder rule ---

// lockTestScale mirrors backend/internal/timescale's contract for this module
// (which has no timescale package of its own): every deadline-competing
// duration in these real-process tests derives from ONE factor, so the
// discrimination ratios hold at any scale and a loaded CI runner cannot slip
// past a tight raw bound. CI auto-scales 5x; FISHHAWK_TEST_TIME_SCALE
// overrides. Poll intervals stay unscaled.
func lockTestScale() time.Duration {
	factor := 1
	if v := os.Getenv("FISHHAWK_TEST_TIME_SCALE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			factor = n
		}
	} else if os.Getenv("CI") != "" {
		factor = 5
	}
	return time.Duration(factor)
}

// scaledD scales a base duration by the module's test time factor.
func scaledD(base time.Duration) time.Duration { return base * lockTestScale() }

// startSleeper starts a REAL long-lived child in its OWN process group and
// returns its pid. Real processes, not stubs: the refusal tests must prove an
// unprovable holder is left ALIVE, which only a live process can show.
func startSleeper(t *testing.T) int {
	t.Helper()
	return startSleeperInGroup(t, 0)
}

// startSleeperInGroup starts a REAL long-lived child and returns its pid. pgid
// 0 puts the child in its OWN new group (so it LEADS that group, pgid == pid,
// the shape every spawned runner has); a non-zero pgid JOINS the child to that
// existing group, which makes it a NON-leader (pgid != pid) — the only way to
// exercise signalLockHolder's single-pid fallback without signalling a group
// the test itself belongs to.
func startSleeperInGroup(t *testing.T, pgid int) int {
	t.Helper()
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	cmd := exec.Command("sleep", "120")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child process: %v", err)
	}
	// Reap in the background. An UNwaited-for child of THIS process becomes a
	// zombie when it dies, and signal 0 to a zombie still succeeds — so without
	// this the liveness probe would report a killed holder as alive forever.
	// A real orphaned runner is not our child, so this is a test artifact only.
	waited := make(chan struct{})
	go func() { defer close(waited); _ = cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waited
	})
	return cmd.Process.Pid
}

// plantLock writes a lineage lockfile for root recording pid + holderRunID,
// exactly as acquireLineageLock's own writer does, and returns its path.
func plantLock(t *testing.T, repo, root string, pid int, holderRunID string) string {
	t.Helper()
	wtDir, err := worktreesDir(context.Background(), repo)
	if err != nil {
		t.Fatalf("worktreesDir: %v", err)
	}
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(wtDir, "run-"+root+".lock")
	body := fmt.Sprintf("%d\n", pid)
	if holderRunID != "" {
		body += holderRunID + "\n"
	}
	if err := os.WriteFile(lockPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return lockPath
}

// fakeRunStateClient is the injectable backend read for the holder rule.
type fakeRunStateClient struct {
	state string
	err   error
	calls int
}

func (f *fakeRunStateClient) RunState(_ context.Context, _ string) (string, error) {
	f.calls++
	return f.state, f.err
}

// stubProcessArgv replaces the ps-backed argv reader for one test.
func stubProcessArgv(t *testing.T, argv string, err error) {
	t.Helper()
	orig := processArgv
	processArgv = func(int) (string, error) { return argv, err }
	t.Cleanup(func() { processArgv = orig })
}

// runnerArgvFor renders the argv a REAL fishhawk-runner carries for runID,
// derived from the same runIDArgFlag the identity matcher requires.
func runnerArgvFor(runID string) string {
	return "/opt/fishhawk/bin/" + runnerBinaryName + " " + runIDArgFlag + " " + runID +
		" --backend-url https://api.test --stage implement"
}

// TestRunnerIdentityMatches pins the argv matcher to TOKEN BOUNDARIES. The
// accept case is byte-for-byte the two-adjacent-token shape composeRunnerArgv
// emits (`--run-id <id>`), and every reject case is a shape that a naive
// substring containment check would have ACCEPTED.
func TestRunnerIdentityMatches(t *testing.T) {
	const runID = "3b790e10-73a4-4513-a52f-34fb4e3fb1d3"
	cases := []struct {
		name string
		argv string
		want bool
	}{
		{
			name: "real runner argv accepts",
			argv: runnerArgvFor(runID),
			want: true,
		},
		{
			name: "bare binary name (no path) accepts",
			argv: runnerBinaryName + " " + runIDArgFlag + " " + runID,
			want: true,
		},
		{
			name: "package test binary basename accepts",
			argv: "/tmp/go-build1/b001/" + runnerBinaryName + ".test -test.run=X " + runIDArgFlag + " " + runID,
			want: true,
		},
		{
			// The single-token form composeRunnerArgv never emits.
			name: "--run-id=<id> single token rejects",
			argv: "/opt/bin/" + runnerBinaryName + " " + runIDArgFlag + "=" + runID,
			want: false,
		},
		{
			// One token embedding BOTH the runner name and the exact run id.
			// Naked substring containment would authorize a signal here.
			name: "single token embedding runner name and run id rejects",
			argv: "/tmp/payload/x" + runnerBinaryName + "--run-id-" + runID + "-payload",
			want: false,
		},
		{
			name: "longer token merely containing the flag text rejects",
			argv: "/opt/bin/" + runnerBinaryName + " --run-idx " + runID,
			want: false,
		},
		{
			name: "flag present but the FOLLOWING token is a longer id rejects",
			argv: "/opt/bin/" + runnerBinaryName + " " + runIDArgFlag + " " + runID + "-suffix",
			want: false,
		},
		{
			name: "flag is the last token rejects",
			argv: "/opt/bin/" + runnerBinaryName + " " + runIDArgFlag,
			want: false,
		},
		{
			name: "different run id rejects",
			argv: runnerArgvFor("00000000-0000-0000-0000-000000000000"),
			want: false,
		},
		{
			name: "non-runner binary with the right flag pair rejects",
			argv: "/bin/sleep " + runIDArgFlag + " " + runID,
			want: false,
		},
		{
			// The binary is proven from argv[0] ALONE. An unrelated executable
			// that merely mentions the runner name as an ARGUMENT is not a
			// runner, and must never authorize TERM/KILL against its pid.
			name: "runner name in a later token (not argv[0]) rejects",
			argv: "/bin/sleep " + runnerBinaryName + " " + runIDArgFlag + " " + runID,
			want: false,
		},
		{
			name: "runner name only in a trailing argument rejects",
			argv: "/usr/bin/tail -f /var/log/" + runnerBinaryName + " " + runIDArgFlag + " " + runID,
			want: false,
		},
		{
			name: "binary name as a substring of a longer basename rejects",
			argv: "/opt/bin/evil-" + runnerBinaryName + " " + runIDArgFlag + " " + runID,
			want: false,
		},
		{
			name: "empty argv rejects",
			argv: "",
			want: false,
		},
		{
			name: "empty run id rejects",
			argv: runnerArgvFor(runID),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := runID
			if tc.name == "empty run id rejects" {
				id = ""
			}
			if got := runnerIdentityMatches(tc.argv, id); got != tc.want {
				t.Errorf("runnerIdentityMatches(%q, %q) = %v, want %v", tc.argv, id, got, tc.want)
			}
		})
	}
}

// TestRunnerIdentityMatches_AcceptsFullSpawnArgvShape asserts the matcher
// accepts a full runner command line of the shape the MCP-side spawn produces:
// the binary in argv[0], `--run-id` and the id as two adjacent tokens, and the
// remaining flags around them.
//
// Scope, stated honestly: this is a RUNNER-SIDE test only. It renders the argv
// from the same runIDArgFlag/runnerBinaryName constants the matcher consumes,
// so it can NOT detect a reshape of composeRunnerArgv — a reshape on the MCP
// side would leave it green. The actual cross-module pin is
// TestComposeRunnerArgv_RunIDFlagAndValueAreAdjacentTokens in
// backend/internal/mcpserver/run_stage_test.go, which exercises the real
// composeRunnerArgv (the two modules have no dependency edge either way, so the
// pin cannot be a single test).
func TestRunnerIdentityMatches_AcceptsFullSpawnArgvShape(t *testing.T) {
	const runID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	argv := strings.Join([]string{
		"/usr/local/bin/" + runnerBinaryName,
		runIDArgFlag, runID,
		"--backend-url", "http://127.0.0.1:8080",
		"--workflow", "feature_change",
		"--stage", "implement",
	}, " ")
	if !runnerIdentityMatches(argv, runID) {
		t.Fatalf("matcher rejected the real spawn argv shape: %q", argv)
	}
}

// TestProcessArgv_ReadsOwnArgv exercises the PRODUCTION ps-backed reader (no
// stub) against this process, so the seam's real body is covered.
func TestProcessArgv_ReadsOwnArgv(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps not available")
	}
	argv, err := processArgv(os.Getpid())
	if err != nil {
		t.Fatalf("processArgv(self): %v", err)
	}
	self := filepath.Base(os.Args[0])
	if !strings.Contains(argv, self) {
		t.Errorf("processArgv(self) = %q, want it to contain this test binary's name %q", argv, self)
	}
}

// TestProcessArgv_NonexistentPid asserts the reader FAILS for a pid that
// cannot exist — the failure that routes to the unprovable-identity refusal.
func TestProcessArgv_NonexistentPid(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps not available")
	}
	if argv, err := processArgv(0x7FFFFFFF); err == nil {
		t.Errorf("processArgv(impossible pid) = %q, nil; want an error", argv)
	}
}

// holderRefusalCase is one named refusal branch of the holder rule.
type holderRefusalCase struct {
	name string
	// holderRunID recorded on line 2 of the lockfile ("" omits the line).
	holderRunID string
	// client is built per-case (nil means "no client threaded in").
	client func(holderRunID string) runStateClient
	// argv, when non-empty, stubs processArgv; argvErr stubs its error.
	argv    string
	argvErr error
	// stubArgv selects whether processArgv is stubbed at all. When false the
	// REAL ps reader runs against the real `sleep` child.
	stubArgv bool
	wantErr  error
}

// TestAcquireLineageLock_RefusesUnprovableHolder drives EVERY named refusal
// branch of the holder rule through the REAL acquireLineageLock against a REAL
// live child holding a REAL lockfile, and asserts all three observable
// consequences of a refusal: the acquire fails loud, the holder is STILL
// ALIVE, and its lockfile is INTACT.
//
// The identity cases are the counterfactual vehicle for the authorizing
// control: delete the verifyLockHolderIdentity call in evictTerminalLockHolder
// and the non-runner `sleep` child gets signalled, the lock is reclaimed and
// the acquire SUCCEEDS — which this test asserts must not happen.
func TestAcquireLineageLock_RefusesUnprovableHolder(t *testing.T) {
	const holderRun = "11111111-2222-3333-4444-555555555555"
	cases := []holderRefusalCase{
		{
			name:        "nil client",
			holderRunID: holderRun,
			client:      func(string) runStateClient { return nil },
			wantErr:     errHolderClientUnavailable,
		},
		{
			name:        "holder run id absent from the lockfile",
			holderRunID: "",
			client: func(string) runStateClient {
				return &fakeRunStateClient{state: runStateCancelled}
			},
			wantErr: errHolderRunIDUnknown,
		},
		{
			name:        "run-state read fails",
			holderRunID: holderRun,
			client: func(string) runStateClient {
				return &fakeRunStateClient{err: errors.New("dial tcp: connection refused")}
			},
			stubArgv: true,
			argv:     runnerArgvFor(holderRun),
			wantErr:  errHolderStateUnknown,
		},
		{
			name:        "run-state read 404s",
			holderRunID: holderRun,
			client: func(string) runStateClient {
				return &fakeRunStateClient{err: upload.ErrNotFound}
			},
			stubArgv: true,
			argv:     runnerArgvFor(holderRun),
			wantErr:  errHolderStateUnknown,
		},
		{
			name:        "run-state read returns an empty state",
			holderRunID: holderRun,
			client: func(string) runStateClient {
				return &fakeRunStateClient{state: ""}
			},
			stubArgv: true,
			argv:     runnerArgvFor(holderRun),
			wantErr:  errHolderStateUnknown,
		},
		{
			name:        "holder run is running",
			holderRunID: holderRun,
			client: func(string) runStateClient {
				return &fakeRunStateClient{state: "running"}
			},
			stubArgv: true,
			argv:     runnerArgvFor(holderRun),
			wantErr:  errHolderNotCancelled,
		},
		{
			name:        "holder run is failed",
			holderRunID: holderRun,
			client: func(string) runStateClient {
				return &fakeRunStateClient{state: "failed"}
			},
			stubArgv: true,
			argv:     runnerArgvFor(holderRun),
			wantErr:  errHolderNotCancelled,
		},
		{
			name:        "holder run is succeeded",
			holderRunID: holderRun,
			client: func(string) runStateClient {
				return &fakeRunStateClient{state: "succeeded"}
			},
			stubArgv: true,
			argv:     runnerArgvFor(holderRun),
			wantErr:  errHolderNotCancelled,
		},
		{
			// REAL ps against a REAL non-runner child: cancelled run, live pid,
			// argv that does not identify a fishhawk-runner.
			name:        "identity unprovable: holder is not a runner",
			holderRunID: holderRun,
			client: func(string) runStateClient {
				return &fakeRunStateClient{state: runStateCancelled}
			},
			wantErr: errHolderIdentityUnprovable,
		},
		{
			name:        "identity unprovable: runner argv carries a different run id",
			holderRunID: holderRun,
			client: func(string) runStateClient {
				return &fakeRunStateClient{state: runStateCancelled}
			},
			stubArgv: true,
			argv:     runnerArgvFor("99999999-9999-9999-9999-999999999999"),
			wantErr:  errHolderIdentityUnprovable,
		},
		{
			name:        "identity unprovable: ps unavailable",
			holderRunID: holderRun,
			client: func(string) runStateClient {
				return &fakeRunStateClient{state: runStateCancelled}
			},
			stubArgv: true,
			argvErr:  errors.New("exec: \"ps\": executable file not found in $PATH"),
			wantErr:  errHolderIdentityUnprovable,
		},
		{
			name:        "identity unprovable: empty argv",
			holderRunID: holderRun,
			client: func(string) runStateClient {
				return &fakeRunStateClient{state: runStateCancelled}
			},
			stubArgv: true,
			argv:     "",
			wantErr:  errHolderIdentityUnprovable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t)
			pid := startSleeper(t)
			const root = "aaaa1111"
			lockPath := plantLock(t, repo, root, pid, tc.holderRunID)
			if tc.stubArgv {
				stubProcessArgv(t, tc.argv, tc.argvErr)
			}

			release, err := acquireLineageLock(
				context.Background(), repo, root, "new-run", tc.client(tc.holderRunID), io.Discard)
			if err == nil {
				release()
				t.Fatalf("acquire ADMITTED over an unprovable live holder; want a loud refusal")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want it to wrap %v", err, tc.wantErr)
			}
			// The error names the holder pid so an operator can act without ps.
			if !strings.Contains(err.Error(), fmt.Sprintf("live pid %d", pid)) {
				t.Errorf("error does not name the holder pid %d: %v", pid, err)
			}
			// The holder was NOT signalled.
			if !processAlive(pid) {
				t.Errorf("unprovable holder pid %d was killed; it must be left alone", pid)
			}
			// The lock was NOT broken.
			if _, statErr := os.Stat(lockPath); statErr != nil {
				t.Errorf("lockfile removed under an unprovable holder: %v", statErr)
			}
		})
	}
}

// TestAcquireLineageLock_HolderSurvivesTermAndKill asserts that when the
// holder is fully provable but does NOT die, the eviction gives up: the
// recorded signal sequence is [TERM, KILL], the acquire fails loud, and the
// lockfile is left INTACT — never break a lock a live holder still owns.
//
// The signalLockHolder seam RECORDS but does not DELIVER, so "the signals were
// sent" and "the holder is still alive" are both literally true; the assertion
// never contradicts a signal that actually landed.
func TestAcquireLineageLock_HolderSurvivesTermAndKill(t *testing.T) {
	repo := initRepo(t)
	pid := startSleeper(t)
	const (
		root      = "bbbb2222"
		holderRun = "22222222-3333-4444-5555-666666666666"
	)
	lockPath := plantLock(t, repo, root, pid, holderRun)
	stubProcessArgv(t, runnerArgvFor(holderRun), nil)

	var (
		mu   sync.Mutex
		sent []syscall.Signal
	)
	origSignal := signalLockHolder
	signalLockHolder = func(_ int, sig syscall.Signal) error {
		mu.Lock()
		sent = append(sent, sig)
		mu.Unlock()
		return nil // recorded, deliberately NOT delivered
	}
	t.Cleanup(func() { signalLockHolder = origSignal })

	origTerm, origKill := lockHolderTermGrace, lockHolderKillGrace
	lockHolderTermGrace = scaledD(60 * time.Millisecond)
	lockHolderKillGrace = scaledD(60 * time.Millisecond)
	t.Cleanup(func() { lockHolderTermGrace, lockHolderKillGrace = origTerm, origKill })

	release, err := acquireLineageLock(context.Background(), repo, root, "new-run",
		&fakeRunStateClient{state: runStateCancelled}, io.Discard)
	if err == nil {
		release()
		t.Fatal("acquire ADMITTED while the holder was still alive; want a loud refusal")
	}
	if !errors.Is(err, errHolderSurvivedKill) {
		t.Errorf("error = %v, want it to wrap %v", err, errHolderSurvivedKill)
	}
	mu.Lock()
	got := append([]syscall.Signal(nil), sent...)
	mu.Unlock()
	if len(got) != 2 || got[0] != syscall.SIGTERM || got[1] != syscall.SIGKILL {
		t.Errorf("signal sequence = %v, want [SIGTERM SIGKILL]", got)
	}
	if !processAlive(pid) {
		t.Errorf("holder pid %d died; the seam was supposed to record without delivering", pid)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Errorf("lockfile broken while the holder was still alive: %v", statErr)
	}
}

// TestAcquireLineageLock_EvictsCancelledRunnerHolder is the positive unit: a
// REAL live child in its own process group, a lockfile naming a run the
// backend reports CANCELLED, and a provable runner argv. The REAL signal path
// runs (no seam), the holder dies, the lock is reclaimed and the next acquire
// is ADMITTED, recording THIS process's pid and the new run id.
func TestAcquireLineageLock_EvictsCancelledRunnerHolder(t *testing.T) {
	repo := initRepo(t)
	pid := startSleeper(t)
	const (
		root      = "cccc3333"
		holderRun = "33333333-4444-5555-6666-777777777777"
	)
	lockPath := plantLock(t, repo, root, pid, holderRun)
	// The holder is a real `sleep`, so stub the argv reader to make its
	// IDENTITY provable — the identity check itself is pinned by the refusal
	// table above and by the ps-real integration test.
	stubProcessArgv(t, runnerArgvFor(holderRun), nil)

	origTerm, origKill := lockHolderTermGrace, lockHolderKillGrace
	lockHolderTermGrace = scaledD(2 * time.Second)
	lockHolderKillGrace = scaledD(2 * time.Second)
	t.Cleanup(func() { lockHolderTermGrace, lockHolderKillGrace = origTerm, origKill })

	var log strings.Builder
	release, err := acquireLineageLock(context.Background(), repo, root, "run-next",
		&fakeRunStateClient{state: runStateCancelled}, &log)
	if err != nil {
		t.Fatalf("acquire over a cancelled runner holder refused: %v", err)
	}
	defer release()

	if processAlive(pid) {
		t.Errorf("cancelled runner holder pid %d still alive after eviction", pid)
	}
	if got := readLockPID(lockPath); got != os.Getpid() {
		t.Errorf("lock pid after eviction = %d, want this process %d", got, os.Getpid())
	}
	if got := readLockRunID(lockPath); got != "run-next" {
		t.Errorf("lock run id after eviction = %q, want run-next", got)
	}
	for _, want := range []string{
		`"event":"lineage_lock_evicted"`,
		`"holder_run_id":"` + holderRun + `"`,
		`"holder_state":"` + runStateCancelled + `"`,
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("eviction log missing %s:\n%s", want, log.String())
		}
	}
}

// killHolderNow SIGKILLs pid for real and blocks until it is genuinely gone
// (startSleeperInGroup's goroutine reaps it, so it does not linger as a zombie
// that signal 0 would still report alive). Bounded: a stuck kill fails the test
// rather than hanging it.
func killHolderNow(t *testing.T, pid int) {
	t.Helper()
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if !waitProcessGone(pid, scaledD(2*time.Second)) {
		t.Fatalf("holder pid %d did not exit after SIGKILL", pid)
	}
}

// TestAcquireLineageLock_HolderExitsBeforeSignalLands covers the race the
// eviction flow introduces: a fully provable holder can exit NATURALLY in the
// window between the identity check and either signal (its stage simply
// finished). The delivery then fails ESRCH, and treating that as a failure
// would preserve the lockfile over an ALREADY-DEAD pid — the #2545 wedge
// re-created by its own fix, with the first post-cancel same-lineage dispatch
// still refused.
//
// The seam makes the interleaving deterministic rather than timing-dependent:
// for the signal under test it kills the holder for real, waits for the process
// to be genuinely gone, and only THEN returns ESRCH — exactly what the kernel
// reports for a holder that beat us to the exit. Every other signal is recorded
// and NOT delivered, so the holder really does survive it.
//
// The refusal cases pin the two halves of holderAlreadyExited independently:
// an ESRCH reported for a holder that is still ALIVE keeps the fail-closed
// refusal (the liveness re-probe), and a NON-ESRCH delivery failure keeps it
// even over a dead holder (the errno test — an EPERM, or the Windows
// errSignalUnsupported stub, is a signal we could not deliver, and on Windows
// processAlive cannot answer; that dead holder is reclaimed by the ordinary
// stale-lock path on the next acquire, so the conservative refusal costs one
// dispatch, not a wedge).
func TestAcquireLineageLock_HolderExitsBeforeSignalLands(t *testing.T) {
	const holderRun = "44444444-5555-6666-7777-888888888888"
	cases := []struct {
		name string
		// failOn is the signal whose delivery fails, with failErr.
		failOn  syscall.Signal
		failErr error
		// reallyExit makes the holder genuinely dead before that failure.
		reallyExit bool
		// deliverOn is the signal the seam actually DELIVERS (killing the
		// holder) and reports success for. Every other signal is recorded and
		// not delivered, so the holder survives it.
		deliverOn    syscall.Signal
		wantAdmitted bool
	}{
		{
			name:         "holder exits before TERM lands",
			failOn:       syscall.SIGTERM,
			failErr:      syscall.ESRCH,
			reallyExit:   true,
			wantAdmitted: true,
		},
		{
			name:         "holder exits before the escalated KILL lands",
			failOn:       syscall.SIGKILL,
			failErr:      syscall.ESRCH,
			reallyExit:   true,
			wantAdmitted: true,
		},
		{
			name:         "ESRCH from a holder that is STILL ALIVE refuses",
			failOn:       syscall.SIGTERM,
			failErr:      syscall.ESRCH,
			reallyExit:   false,
			wantAdmitted: false,
		},
		{
			name:         "non-ESRCH delivery failure refuses even over a dead holder",
			failOn:       syscall.SIGTERM,
			failErr:      syscall.EPERM,
			reallyExit:   true,
			wantAdmitted: false,
		},
		{
			name:         "non-ESRCH failure of the escalated KILL refuses",
			failOn:       syscall.SIGKILL,
			failErr:      syscall.EPERM,
			reallyExit:   false,
			wantAdmitted: false,
		},
		{
			// The ordinary escalation: the holder ignores TERM and dies on KILL.
			name:         "holder survives TERM and dies on the escalated KILL",
			deliverOn:    syscall.SIGKILL,
			wantAdmitted: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t)
			pid := startSleeper(t)
			const root = "dddd4444"
			lockPath := plantLock(t, repo, root, pid, holderRun)
			stubProcessArgv(t, runnerArgvFor(holderRun), nil)

			origSignal := signalLockHolder
			signalLockHolder = func(p int, sig syscall.Signal) error {
				switch sig {
				case tc.failOn:
					if tc.reallyExit {
						killHolderNow(t, p)
					}
					return tc.failErr
				case tc.deliverOn:
					killHolderNow(t, p)
					return nil
				default:
					return nil // recorded, deliberately NOT delivered
				}
			}
			t.Cleanup(func() { signalLockHolder = origSignal })

			origTerm, origKill := lockHolderTermGrace, lockHolderKillGrace
			lockHolderTermGrace = scaledD(60 * time.Millisecond)
			lockHolderKillGrace = scaledD(60 * time.Millisecond)
			t.Cleanup(func() { lockHolderTermGrace, lockHolderKillGrace = origTerm, origKill })

			var log strings.Builder
			release, err := acquireLineageLock(context.Background(), repo, root, "run-next",
				&fakeRunStateClient{state: runStateCancelled}, &log)

			if !tc.wantAdmitted {
				if err == nil {
					release()
					t.Fatal("acquire ADMITTED under a delivery failure that must refuse")
				}
				if !errors.Is(err, errHolderSurvivedKill) {
					t.Errorf("error = %v, want it to wrap %v", err, errHolderSurvivedKill)
				}
				if alive := processAlive(pid); alive == tc.reallyExit {
					t.Errorf("holder pid %d alive = %v, want %v — the seam did not stage the case it claims",
						pid, alive, !tc.reallyExit)
				}
				if _, statErr := os.Stat(lockPath); statErr != nil {
					t.Errorf("lockfile broken under a refused eviction: %v", statErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("acquire REFUSED over an already-dead holder — the #2545 wedge re-created: %v", err)
			}
			defer release()
			if processAlive(pid) {
				t.Errorf("holder pid %d still alive; the test's own kill did not land", pid)
			}
			if got := readLockPID(lockPath); got != os.Getpid() {
				t.Errorf("lock pid after eviction = %d, want this process %d", got, os.Getpid())
			}
			if got := readLockRunID(lockPath); got != "run-next" {
				t.Errorf("lock run id after eviction = %q, want run-next", got)
			}
			if !strings.Contains(log.String(), `"event":"lineage_lock_evicted"`) {
				t.Errorf("eviction log missing lineage_lock_evicted:\n%s", log.String())
			}
		})
	}
}

// TestSignalLockHolder_NonGroupLeaderSignalsOnlyThatPid exercises the REAL
// signalLockHolder (no seam) on its non-leader fallback: a pid that does not
// lead its group must be signalled ALONE, never as `-pgid`, which would reach
// a group the holder merely belongs to — up to and including our own.
//
// The two children are arranged so the failure is observable WITHOUT signalling
// the test's own group: the leader starts a fresh group, the member JOINS it
// (pgid == leader, so the member is a non-leader). Signalling the member must
// kill the member and leave the leader alive; a `-pgid` regression kills the
// leader instead of this process.
func TestSignalLockHolder_NonGroupLeaderSignalsOnlyThatPid(t *testing.T) {
	leader := startSleeper(t)
	member := startSleeperInGroup(t, leader)

	pgid, err := syscall.Getpgid(member)
	if err != nil {
		t.Skipf("cannot read pgid of pid %d: %v", member, err)
	}
	if pgid != leader || member == leader {
		t.Skipf("could not construct a non-leader child: member=%d pgid=%d leader=%d", member, pgid, leader)
	}

	if sigErr := signalLockHolder(member, syscall.SIGKILL); sigErr != nil {
		t.Fatalf("signalLockHolder(non-leader pid %d) = %v, want nil", member, sigErr)
	}
	if !waitProcessGone(member, scaledD(2*time.Second)) {
		t.Fatalf("non-leader pid %d survived the signal", member)
	}
	if !processAlive(leader) {
		t.Fatalf("signalling non-leader pid %d also killed pid %d: the whole GROUP was signalled", member, leader)
	}
}

// TestSignalLockHolder_RejectsNonPositivePid pins the guard that keeps a
// non-positive pid out of kill(2), where 0 means "this process's whole group"
// and -1 means "every process we may signal".
//
// Signal 0 deliberately: it is the no-delivery permission probe, so DELETING
// the guard makes both calls return nil and this test goes RED on the
// assertion — rather than the counterfactual signalling the test runner's own
// process group.
func TestSignalLockHolder_RejectsNonPositivePid(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if err := signalLockHolder(pid, syscall.Signal(0)); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("signalLockHolder(%d) = %v, want ESRCH", pid, err)
		}
	}
}

// TestReadLockRunID covers the lockfile line-2 reader's degrade paths, each of
// which routes to the errHolderRunIDUnknown refusal.
func TestReadLockRunID(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if got := readLockRunID(write("ok.lock", "1234\nrun-abc\n")); got != "run-abc" {
		t.Errorf("run id = %q, want run-abc", got)
	}
	if got := readLockRunID(write("pidonly.lock", "1234\n")); got != "" {
		t.Errorf("pid-only lock run id = %q, want empty", got)
	}
	if got := readLockRunID(write("noline.lock", "1234")); got != "" {
		t.Errorf("single-line lock run id = %q, want empty", got)
	}
	if got := readLockRunID(filepath.Join(dir, "missing.lock")); got != "" {
		t.Errorf("missing lock run id = %q, want empty", got)
	}
}
