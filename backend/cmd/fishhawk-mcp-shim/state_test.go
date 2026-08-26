package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeStateFile drops a snapshot at an explicit filename so a fixture dir can
// hold several snapshots (and malformed files) at once.
func writeStateFile(t *testing.T, dir, name string, st swapState) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// hashOf is the hex launch hash of a fixture file.
func hashOf(t *testing.T, path string) string {
	t.Helper()
	h, err := sha256File(path)
	if err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(h)
}

// deadPid returns the pid of a process that has already exited AND been reaped,
// obtained BY CONSTRUCTION from an exec'd child rather than by asking the
// liveness filter under test — so a counterfactual RED lands on the behavioral
// assertion, not on fixture setup.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn throwaway child: %v", err)
	}
	return cmd.Process.Pid
}

// --- classifyState: one case per verdict, both sides of the grace boundary ---

func TestClassifyStateVerdicts(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	grace := time.Minute
	baseline := []byte{0x01, 0x02, 0x03}
	other := []byte{0x09, 0x09, 0x09}

	cases := []struct {
		name       string
		launchHash string
		onDisk     []byte
		pendingAgo time.Duration
		noPending  bool
		want       stateVerdict
	}{
		{name: "hashes equal", launchHash: hex.EncodeToString(baseline), onDisk: baseline, want: verdictCurrent},
		{name: "differ, no pending_since", launchHash: hex.EncodeToString(baseline), onDisk: other, noPending: true, want: verdictPending},
		{name: "differ, pending younger than grace", launchHash: hex.EncodeToString(baseline), onDisk: other, pendingAgo: 30 * time.Second, want: verdictPending},
		{name: "differ, pending exactly grace", launchHash: hex.EncodeToString(baseline), onDisk: other, pendingAgo: grace, want: verdictStale},
		{name: "differ, pending older than grace", launchHash: hex.EncodeToString(baseline), onDisk: other, pendingAgo: 10 * time.Minute, want: verdictStale},
		{name: "unhashable child_path", launchHash: hex.EncodeToString(baseline), onDisk: nil, pendingAgo: 10 * time.Minute, want: verdictPending},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := swapState{ChildLaunchHash: c.launchHash}
			if !c.noPending {
				st.PendingSince = now.Add(-c.pendingAgo).Format(time.RFC3339)
			}
			if got := classifyState(st, c.onDisk, now, grace); got != c.want {
				t.Fatalf("classifyState = %q, want %q", got, c.want)
			}
		})
	}

	// An unparseable pending_since is the same fail-safe direction as an
	// unhashable path: pending, never stale.
	st := swapState{ChildLaunchHash: hex.EncodeToString(baseline), PendingSince: "not-a-timestamp"}
	if got := classifyState(st, other, now, grace); got != verdictPending {
		t.Fatalf("unparseable pending_since = %q, want %q", got, verdictPending)
	}
}

// --- loadStates: live only, every skip carries a reason, absent dir is empty ---

func TestLoadStatesSkipsDeadAndMalformed(t *testing.T) {
	dir := t.TempDir()
	live := swapState{Schema: stateSchema, ShimPid: os.Getpid(), ChildPath: "/live/child"}
	writeStateFile(t, dir, strconv.Itoa(live.ShimPid)+".json", live)

	dead := swapState{Schema: stateSchema, ShimPid: deadPid(t), ChildPath: "/dead/child"}
	deadPath := writeStateFile(t, dir, strconv.Itoa(dead.ShimPid)+".json", dead)

	badPath := filepath.Join(dir, "garbage.json")
	if err := os.WriteFile(badPath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatalf("write non-json: %v", err)
	}

	states, skips := loadStates(dir)
	if len(states) != 1 || states[0].ChildPath != "/live/child" {
		t.Fatalf("states = %+v, want exactly the live snapshot", states)
	}
	var sawDead, sawBad bool
	for _, s := range skips {
		if s.Reason == "" {
			t.Errorf("skip %s carries no reason", s.Path)
		}
		if s.Path == deadPath {
			sawDead = true
			if !s.DeadPid {
				t.Errorf("dead-pid skip not flagged DeadPid: %+v", s)
			}
			if !strings.Contains(s.Reason, "not running") {
				t.Errorf("dead-pid reason = %q, want it to name the dead pid", s.Reason)
			}
		}
		if s.Path == badPath {
			sawBad = true
			if !strings.Contains(s.Reason, "unparseable") {
				t.Errorf("malformed reason = %q, want 'unparseable'", s.Reason)
			}
		}
	}
	if !sawDead || !sawBad {
		t.Fatalf("skips = %+v, want a reason for both the dead-pid and the malformed file", skips)
	}

	// An ABSENT dir is empty, not an error: the diagnostic must be inert on a
	// machine that has never run a shim.
	gone, goneSkips := loadStates(filepath.Join(dir, "nope"))
	if len(gone) != 0 || len(goneSkips) != 0 {
		t.Fatalf("absent dir = (%v, %v), want empty", gone, goneSkips)
	}
}

// --- writeState: atomic, complete, no temp litter, replaces in place ---

func TestWriteStateAtomicAndComplete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	st := swapState{Schema: stateSchema, ShimPid: 4242, ChildPath: "/bin/child", LastSwapOutcome: outcomeSwapped}
	if err := writeState(dir, st); err != nil {
		t.Fatalf("writeState: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "4242.json" {
		t.Fatalf("dir contents = %v, want exactly 4242.json (no CreateTemp leftovers)", entries)
	}

	b, err := os.ReadFile(filepath.Join(dir, "4242.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got swapState
	if err := json.Unmarshal(bytes.TrimSpace(b), &got); err != nil {
		t.Fatalf("target does not parse as complete JSON: %v (%q)", err, b)
	}
	if got.ShimPid != 4242 || got.LastSwapOutcome != outcomeSwapped || got.Schema != stateSchema {
		t.Fatalf("round-trip lost fields: %+v", got)
	}

	// A second write replaces the target in place — still one file, new content.
	st.LastSwapOutcome = outcomeDeferredInFlight
	if err := writeState(dir, st); err != nil {
		t.Fatalf("second writeState: %v", err)
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("dir contents after second write = %v, want exactly one file", entries)
	}
	b, err = os.ReadFile(filepath.Join(dir, "4242.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := json.Unmarshal(bytes.TrimSpace(b), &got); err != nil {
		t.Fatalf("replaced target does not parse: %v", err)
	}
	if got.LastSwapOutcome != outcomeDeferredInFlight {
		t.Fatalf("second write not observed: %+v", got)
	}
}

// staleFixture builds a stale-classifying snapshot whose child_path exists on
// disk with content that does NOT match the recorded launch hash.
func staleFixture(t *testing.T, dir, name string) swapState {
	t.Helper()
	childPath := filepath.Join(dir, name+".bin")
	if err := os.WriteFile(childPath, []byte("on-disk-"+name), 0o755); err != nil { //nolint:gosec // fixture binary
		t.Fatalf("write child fixture: %v", err)
	}
	return swapState{
		Schema:              stateSchema,
		ShimPid:             os.Getpid(),
		ShimGitSHA:          "abc1234",
		ChildPid:            918273,
		ChildPath:           childPath,
		ChildLaunchHash:     hex.EncodeToString([]byte("stale-baseline-hash-bytes")),
		PendingSwapHash:     hex.EncodeToString([]byte("pending-hash-bytes")),
		PendingSince:        time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		HandshakeDone:       false,
		ServedResults:       7,
		InFlight:            3,
		OldestInFlightID:    "4242",
		OldestInFlightSince: time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339),
		LastSwapAt:          time.Now().Add(-9 * time.Hour).UTC().Format(time.RFC3339),
		LastSwapOutcome:     outcomeDeferredInFlight,
		UpdatedAt:           time.Now().UTC().Format(time.RFC3339),
	}
}

// --- renderStatus: every AC2 field is named ---

func TestRenderStatusNamesEveryDiagnosticField(t *testing.T) {
	dir := t.TempDir()
	st := staleFixture(t, dir, "child")
	stateDir := filepath.Join(dir, "state")
	writeStateFile(t, stateDir, "1.json", st)

	var out, errOut bytes.Buffer
	if rc := renderStatus(&out, &errOut, stateDir, false, time.Minute, time.Now); rc != exitOK {
		t.Fatalf("renderStatus rc = %d, want %d (a diagnostic never fails its caller)", rc, exitOK)
	}
	got := out.String()
	for _, want := range []string{
		"STALE",
		strconv.Itoa(st.ShimPid),
		strconv.Itoa(st.ChildPid),
		st.ChildPath,
		shortHash(st.ChildLaunchHash),
		shortHash(st.PendingSwapHash),
		"NEVER OBSERVED",
		"served results 7",
		"in-flight 3",
		"4242",
		outcomeDeferredInFlight,
		"/mcp",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q\n---\n%s", want, got)
		}
	}
}

// TestRenderStatusPresumedHandshakeIsLabelled pins that a presumed handshake
// reads as presumed, not as a normal completed one — the operator must be able
// to tell the fallback fired.
func TestRenderStatusPresumedHandshakeIsLabelled(t *testing.T) {
	dir := t.TempDir()
	st := staleFixture(t, dir, "child")
	st.HandshakeDone = true
	st.HandshakePresumed = true
	stateDir := filepath.Join(dir, "state")
	writeStateFile(t, stateDir, "1.json", st)

	var out, errOut bytes.Buffer
	renderStatus(&out, &errOut, stateDir, false, time.Minute, time.Now)
	if !strings.Contains(out.String(), "presumed") {
		t.Fatalf("presumed handshake not labelled:\n%s", out.String())
	}
}

// --- renderStatus --stale-only: filters, and is SILENT on BOTH streams clean ---

func TestRenderStatusStaleOnlyFiltersAndIsEmptyWhenClean(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")

	stale := staleFixture(t, dir, "stalechild")
	writeStateFile(t, stateDir, "stale.json", stale)

	// A CURRENT snapshot: same live pid, different child path, launch hash equal
	// to the bytes on disk.
	currentPath := filepath.Join(dir, "currentchild.bin")
	if err := os.WriteFile(currentPath, []byte("fresh"), 0o755); err != nil { //nolint:gosec // fixture binary
		t.Fatalf("write: %v", err)
	}
	current := swapState{
		Schema:          stateSchema,
		ShimPid:         os.Getpid(),
		ChildPath:       currentPath,
		ChildLaunchHash: hashOf(t, currentPath),
	}
	writeStateFile(t, stateDir, "current.json", current)

	var out, errOut bytes.Buffer
	renderStatus(&out, &errOut, stateDir, true, time.Minute, time.Now)
	got := out.String()
	if !strings.Contains(got, stale.ChildPath) {
		t.Fatalf("--stale-only dropped the stale entry:\n%s", got)
	}
	if strings.Contains(got, currentPath) {
		t.Fatalf("--stale-only printed a non-stale entry:\n%s", got)
	}

	// Nothing stale ⇒ NOTHING on stdout AND nothing on stderr. This is the
	// machine-consumable contract scripts/dev keys its advisory off; a dead-pid
	// leftover (the steady state on any dev box that has ever run a shim) must
	// not turn a clean machine into a noisy one.
	cleanDir := filepath.Join(dir, "clean")
	writeStateFile(t, cleanDir, "current.json", current)
	writeStateFile(t, cleanDir, "leftover.json", swapState{Schema: stateSchema, ShimPid: deadPid(t), ChildPath: "/gone"})
	if err := os.WriteFile(filepath.Join(cleanDir, "garbage.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	var cleanOut, cleanErr bytes.Buffer
	if rc := renderStatus(&cleanOut, &cleanErr, cleanDir, true, time.Minute, time.Now); rc != exitOK {
		t.Fatalf("rc = %d, want %d", rc, exitOK)
	}
	if cleanOut.Len() != 0 {
		t.Fatalf("--stale-only must print NOTHING to stdout when nothing is stale, got %q", cleanOut.String())
	}
	if cleanErr.Len() != 0 {
		t.Fatalf("--stale-only must print NOTHING to stderr when nothing is stale, got %q", cleanErr.String())
	}
}

// TestRenderStatusReportsAndPrunesLeftoverFiles pins the human-readable mode's
// half of the same story: skip reasons go to stderr (never stdout, which the
// shell advisory reads), and a dead-pid leftover is removed so the noise source
// does not accumulate.
func TestRenderStatusReportsAndPrunesLeftoverFiles(t *testing.T) {
	dir := t.TempDir()
	leftover := writeStateFile(t, dir, "leftover.json", swapState{Schema: stateSchema, ShimPid: deadPid(t), ChildPath: "/gone"})

	var out, errOut bytes.Buffer
	renderStatus(&out, &errOut, dir, false, time.Minute, time.Now)
	if !strings.Contains(errOut.String(), "leftover.json") {
		t.Fatalf("skip reason not reported on stderr: %q", errOut.String())
	}
	if strings.Contains(out.String(), "leftover.json") {
		t.Fatalf("skip reason leaked to stdout: %q", out.String())
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("dead-pid leftover was not pruned (stat err = %v)", err)
	}
	if !strings.Contains(out.String(), "no live fishhawk-mcp-shim state files") {
		t.Fatalf("--status should say so when nothing live is found: %q", out.String())
	}
}

// TestWriteStateFailsWhenDirUnusable pins the degrade path main.go keys its
// log-once warning off: an unusable state dir returns an error (and leaves no
// litter) rather than panicking or half-writing.
func TestWriteStateFailsWhenDirUnusable(t *testing.T) {
	base := t.TempDir()
	// A regular FILE where the state dir should be: MkdirAll cannot proceed.
	blocked := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := writeState(blocked, swapState{ShimPid: 7}); err == nil {
		t.Fatal("writeState should fail when the state dir cannot be created")
	}

	// removeState is best-effort: an absent file is not an error and must not panic.
	removeState(filepath.Join(base, "nope"), 7)
}
