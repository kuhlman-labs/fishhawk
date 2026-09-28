package procsweep

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// floorT is the stage-start instant every selection fixture is built around.
// Deliberately carries a sub-second component so the whole-second floor
// comparison (approval condition 2) is exercised rather than assumed.
var floorT = time.Date(2026, 9, 27, 12, 0, 0, 500_000_000, time.UTC)

// proc builds a table row whose StartRaw is the ps rendering of start, so the
// recorded-pid identity discriminator can be exercised without hand-writing it.
func proc(pid, ppid, pgid int, start time.Time, command string) Proc {
	return Proc{
		PID:      pid,
		PPID:     ppid,
		PGID:     pgid,
		Start:    start,
		StartRaw: start.Format(lstartLayout),
		Command:  command,
	}
}

func table(ps ...Proc) Table { return Table{Procs: ps} }

func recorded(descendants []Proc, pgids []int, tombstones []int) RecordedState {
	st := RecordedState{
		Descendants: map[int]Proc{},
		PGIDs:       map[int]struct{}{},
		Tombstones:  map[int]struct{}{},
	}
	for _, p := range descendants {
		st.Descendants[p.PID] = p
	}
	for _, pg := range pgids {
		st.PGIDs[pg] = struct{}{}
	}
	for _, pg := range tombstones {
		st.Tombstones[pg] = struct{}{}
	}
	return st
}

func pids(ps []Proc) []int {
	out := make([]int, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.PID)
	}
	return out
}

func wantPIDs(t *testing.T, got []Proc, want ...int) {
	t.Helper()
	g := pids(got)
	if len(g) != len(want) {
		t.Fatalf("targets = %v, want %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("targets = %v, want %v", g, want)
		}
	}
}

// --- Descendants ---------------------------------------------------------

func TestDescendants_TransitiveClosureExcludesAnchor(t *testing.T) {
	tb := table(
		proc(100, 1, 100, floorT, "runner"),
		proc(200, 100, 100, floorT, "agent"),
		proc(300, 200, 300, floorT, "sh"),
		proc(400, 300, 300, floorT, "busyloop"),
		proc(900, 1, 900, floorT, "unrelated"),
	)
	got := Descendants(tb, 100)
	for _, pid := range []int{200, 300, 400} {
		if _, ok := got[pid]; !ok {
			t.Errorf("pid %d missing from descendant closure", pid)
		}
	}
	if _, ok := got[100]; ok {
		t.Error("anchor 100 is in its own descendant closure")
	}
	if _, ok := got[900]; ok {
		t.Error("unrelated pid 900 is in the descendant closure")
	}
}

// A torn table read can report a ppid cycle. The closure must terminate.
func TestDescendants_TornReadPPIDCycleTerminates(t *testing.T) {
	tb := table(
		proc(100, 1, 100, floorT, "runner"),
		proc(200, 100, 100, floorT, "a"),
		proc(300, 200, 100, floorT, "b"),
		proc(201, 300, 100, floorT, "c"),
		// c's child claims 200 as its parent AND 200 claims 201 — a cycle.
		proc(202, 201, 100, floorT, "d"),
	)
	tb.Procs = append(tb.Procs, proc(203, 202, 100, floorT, "e"))
	tb.Procs[1].PPID = 203 // 200's parent is now its own descendant.
	done := make(chan map[int]Proc, 1)
	go func() { done <- Descendants(tb, 100) }()
	select {
	case got := <-done:
		if _, ok := got[200]; ok {
			t.Error("pid 200 is reachable from the anchor only through the cycle")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Descendants did not terminate on a ppid cycle")
	}
}

// The cycle above is DISCONNECTED from the anchor, so the traversal never
// reaches it and the visited set is not what terminates that test. A REACHABLE
// cycle needs a torn read to report the same pid TWICE with different ppids,
// which is exactly what a `ps` snapshot taken across a re-parent does: pid 200
// appears both as the anchor's child (the stale row) and as 300's child (the
// fresh row), so children[100] -> 200 -> children[200] -> 300 -> children[300]
// -> 200 walks into a cycle the traversal genuinely entered. Without the visited
// set this loops 200 -> 300 -> 200 forever. The duplicate ANCHOR row (100 as a
// child of 300) additionally proves the anchor pre-marking excludes the anchor
// from a closure that reaches it.
func TestDescendants_ReachableCycleTerminatesAndExcludesAnchor(t *testing.T) {
	tb := table(
		proc(100, 1, 100, floorT, "runner"),           // anchor
		proc(200, 100, 100, floorT, "sh (stale row)"), // reachable from the anchor
		proc(300, 200, 100, floorT, "sh -c loop"),     // reachable through 200
		proc(200, 300, 100, floorT, "sh (fresh row)"), // torn read: 200 <-> 300 cycle
		proc(100, 300, 100, floorT, "runner (torn row)"),
	)
	done := make(chan map[int]Proc, 1)
	go func() { done <- Descendants(tb, 100) }()
	select {
	case got := <-done:
		for _, pid := range []int{200, 300} {
			if _, ok := got[pid]; !ok {
				t.Errorf("pid %d must be in the closure — the traversal has to REACH the cycle for this test to pin its termination", pid)
			}
		}
		if _, ok := got[100]; ok {
			t.Error("the anchor must never appear in its own descendant closure, even when a torn row makes it a child of a descendant")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Descendants did not terminate on a ppid cycle it actually reached")
	}
}

// --- Targets: the three exclusions --------------------------------------

// c2's vehicle. The fixture RECORDS the runner's own pid (possible from a torn
// table read) and the live table reports it with a MATCHING StartRaw, so the
// recorded-descendant path would admit it. The runner is its own group leader
// here, so the group-leader guard cannot also drop it, and the recorded path
// bypasses the floor, so the floor cannot mask it either: the self exclusion is
// the only thing in the path.
func TestTargets_NeverTargetsSelf(t *testing.T) {
	self := proc(100, 1, 100, floorT, "fishhawk-runner")
	got := Targets(recorded([]Proc{self}, nil, nil), table(self), 100, 100, floorT)
	wantPIDs(t, got)
}

func TestTargets_NeverTargetsInitOrKernel(t *testing.T) {
	live := table(
		proc(1, 0, 1, floorT.Add(time.Hour), "launchd"),
		proc(0, 0, 0, floorT.Add(time.Hour), "kernel_task"),
	)
	// Both carry a RECORDED pgid and start after the floor, so only the pid<=1
	// exclusion can keep them out.
	got := Targets(recorded(nil, []int{1, 0}, nil), live, 100, 100, floorT)
	wantPIDs(t, got)
}

// c1's vehicle. The runner (pid 100) is in pgid 50 — an operator shell's group —
// and pid 60 is another member of that group with a start AFTER the floor and a
// RECORDED pgid, so the group-leader guard is the only thing in the path.
func TestTargets_NeverSweepsSharedProcessGroupWhenNotLeader(t *testing.T) {
	live := table(
		proc(100, 50, 50, floorT, "fishhawk-runner"),
		proc(60, 1, 50, floorT.Add(time.Minute), "zsh"),
	)
	got := Targets(recorded(nil, []int{50}, nil), live, 100, 50, floorT)
	wantPIDs(t, got)

	// Control: the SAME fixture with the runner LEADING its own group does admit
	// a same-group member, proving the guard — not some other rule — is what
	// spared pid 60 above.
	leader := table(
		proc(100, 1, 100, floorT, "fishhawk-runner"),
		proc(60, 1, 100, floorT.Add(time.Minute), "zsh"),
	)
	got = Targets(recorded(nil, []int{100}, nil), leader, 100, 100, floorT)
	wantPIDs(t, got, 60)
}

// --- Targets: identity + pgid-reuse rules -------------------------------

// The #3663 shape: an orphan re-parented to pid 1 whose pgid was recorded from a
// since-exited descendant shell, started after the floor.
func TestTargets_AdmitsOrphanViaRecordedPGID(t *testing.T) {
	live := table(
		proc(100, 1, 100, floorT, "fishhawk-runner"),
		proc(400, 1, 300, floorT.Add(time.Minute), "sh -c while :; do :; done"),
	)
	got := Targets(recorded(nil, []int{300}, nil), live, 100, 100, floorT)
	wantPIDs(t, got, 400)
}

// c3's vehicle (m5). The recorded pid is reported live with a DIFFERENT
// StartRaw: the kernel recycled it. Its pgid is deliberately NOT in the recorded
// set, so the pgid closure cannot re-admit it and the StartRaw discriminator is
// the only thing keeping it out.
func TestTargets_SkipsRecordedPIDWhoseStartChanged(t *testing.T) {
	rec := recorded([]Proc{proc(200, 100, 100, floorT, "agent")}, nil, nil)
	live := table(proc(200, 1, 777, floorT.Add(2*time.Minute), "someone-elses-process"))
	got := Targets(rec, live, 100, 100, floorT)
	wantPIDs(t, got)

	// Control: the same pid with the RECORDED StartRaw is admitted.
	same := table(proc(200, 1, 777, floorT, "agent"))
	got = Targets(rec, same, 100, 100, floorT)
	wantPIDs(t, got, 200)
}

func TestTargets_RecordedPIDAbsentFromLiveTableIsNotATarget(t *testing.T) {
	rec := recorded([]Proc{proc(200, 100, 100, floorT, "agent")}, nil, nil)
	got := Targets(rec, table(proc(100, 1, 100, floorT, "fishhawk-runner")), 100, 100, floorT)
	wantPIDs(t, got)
}

// p1 / c7: STAGE-START FLOOR. pid 400 is itself a live member of the recorded
// pgid P, so the continuity rule cannot drop P and therefore cannot mask this
// arm; the runner leads its own group, disarming the group-leader guard; pid 400
// is not self and has pid>1. Its start is an hour BEFORE the floor — a
// pre-existing operator shell that happens to hold a reused group id. The floor
// is the only thing in the path.
func TestTargets_StageStartFloorSparesPreexistingProcess(t *testing.T) {
	live := table(
		proc(100, 1, 100, floorT, "fishhawk-runner"),
		proc(400, 1, 300, floorT.Add(-time.Hour), "zsh"),
	)
	got := Targets(recorded(nil, []int{300}, nil), live, 100, 100, floorT)
	wantPIDs(t, got)
}

// Approval condition 2: the floor is compared at WHOLE-SECOND resolution,
// because ps lstart has no sub-second field. A pgid-only process that started in
// the same wall-clock second as the stage start — here 200ms BEFORE the unrounded
// floor, i.e. after the truncated one — IS admitted. That is the stated,
// accepted sub-second exposure.
func TestTargets_FloorTruncatesToSecondResolution(t *testing.T) {
	sameSecondBefore := floorT.Add(-300 * time.Millisecond) // same second, before floorT
	if sameSecondBefore.Truncate(time.Second) != floorT.Truncate(time.Second) {
		t.Fatalf("fixture bug: %v and %v are not in the same wall-clock second", sameSecondBefore, floorT)
	}
	live := table(
		proc(100, 1, 100, floorT, "fishhawk-runner"),
		proc(400, 1, 300, sameSecondBefore, "same-second-shell"),
	)
	got := Targets(recorded(nil, []int{300}, nil), live, 100, 100, floorT)
	wantPIDs(t, got, 400)

	// And one full second earlier is spared, so the truncation is a one-second
	// window and not an unbounded relaxation.
	earlier := table(
		proc(100, 1, 100, floorT, "fishhawk-runner"),
		proc(400, 1, 300, floorT.Truncate(time.Second).Add(-time.Second), "previous-second-shell"),
	)
	got = Targets(recorded(nil, []int{300}, nil), earlier, 100, 100, floorT)
	wantPIDs(t, got)
}

func TestTargets_TombstonedPGIDIsNeverAdmitted(t *testing.T) {
	live := table(
		proc(100, 1, 100, floorT, "fishhawk-runner"),
		proc(501, 1, 500, floorT.Add(time.Minute), "unrelated-reusing-500"),
	)
	got := Targets(recorded(nil, []int{500}, []int{500}), live, 100, 100, floorT)
	wantPIDs(t, got)
}

// --- parseTable ---------------------------------------------------------

// The captured real Darwin ps line (approval condition: parsed to the second),
// plus a Linux-shaped fixture and a single-digit space-padded day.
func TestParseTable_ParsesRealDarwinAndLinuxShapes(t *testing.T) {
	restore := parseLocation
	parseLocation = time.UTC
	t.Cleanup(func() { parseLocation = restore })

	raw := "    1     0     1 Tue Aug 11 11:57:20 2026     /sbin/launchd\n" +
		"  518     1   518 Tue Aug 11 11:58:28 2026     /usr/libexec/logd\n" +
		" 4242  4200  4200 Sat Aug  1 09:05:03 2026 /bin/sh -c while :; do :; done\n"
	got := parseTable(raw)
	if got.Unparsed != 0 {
		t.Errorf("Unparsed = %d, want 0", got.Unparsed)
	}
	if len(got.Procs) != 3 {
		t.Fatalf("Procs = %d rows, want 3: %+v", len(got.Procs), got.Procs)
	}
	if want := time.Date(2026, 8, 11, 11, 57, 20, 0, time.UTC); !got.Procs[0].Start.Equal(want) {
		t.Errorf("row 0 Start = %v, want %v", got.Procs[0].Start, want)
	}
	if got.Procs[0].Command != "/sbin/launchd" {
		t.Errorf("row 0 Command = %q", got.Procs[0].Command)
	}
	last := got.Procs[2]
	if last.PID != 4242 || last.PPID != 4200 || last.PGID != 4200 {
		t.Errorf("row 2 ids = %d/%d/%d, want 4242/4200/4200", last.PID, last.PPID, last.PGID)
	}
	if want := time.Date(2026, 8, 1, 9, 5, 3, 0, time.UTC); !last.Start.Equal(want) {
		t.Errorf("row 2 Start = %v, want %v (space-padded single-digit day)", last.Start, want)
	}
	if last.Command != "/bin/sh -c while :; do :; done" {
		t.Errorf("row 2 Command = %q — the command remainder must be kept whole", last.Command)
	}
}

// Approval condition 1: ps prints LOCAL wall-clock time with no zone field, so
// the parse must be anchored to the host's location. Two non-UTC locations,
// each asserting the resulting ABSOLUTE instant.
func TestParseTable_LstartIsParsedInTheHostLocation(t *testing.T) {
	restore := parseLocation
	t.Cleanup(func() { parseLocation = restore })

	const raw = "  100     1   100 Tue Aug 11 11:57:20 2026 /sbin/launchd\n"
	for _, tc := range []struct {
		name   string
		offset int // seconds east of UTC
	}{
		{"plus5h", 5 * 3600},
		{"minus7h", -7 * 3600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loc := time.FixedZone(tc.name, tc.offset)
			parseLocation = loc
			got := parseTable(raw)
			if len(got.Procs) != 1 {
				t.Fatalf("Procs = %d rows, want 1", len(got.Procs))
			}
			want := time.Date(2026, 8, 11, 11, 57, 20, 0, loc)
			if !got.Procs[0].Start.Equal(want) {
				t.Fatalf("Start = %v (unix %d), want %v (unix %d) — lstart must be read in the host location, not UTC",
					got.Procs[0].Start, got.Procs[0].Start.Unix(), want, want.Unix())
			}
			// And the parsed instant must differ from the UTC reading by exactly
			// the zone offset, which is the whole point of the control.
			utc := time.Date(2026, 8, 11, 11, 57, 20, 0, time.UTC)
			if delta := utc.Sub(got.Procs[0].Start); delta != time.Duration(tc.offset)*time.Second {
				t.Fatalf("parsed instant is %v from the UTC reading, want %v", delta, time.Duration(tc.offset)*time.Second)
			}
		})
	}
}

// m11 / c10: a line whose lstart cannot be parsed is DROPPED and counted, so
// that pid is structurally invisible to Descendants, to Targets and to the
// continuity rule — it can never be killed.
func TestParseTable_DropsUnparseableLstartLine(t *testing.T) {
	raw := "  100     1   100 Tue Aug 11 11:57:20 2026 /good\n" +
		"  200     1   200 Xxx Zzz 99 25:99:99 abcd /bad\n"
	got := parseTable(raw)
	if got.Unparsed != 1 {
		t.Errorf("Unparsed = %d, want 1", got.Unparsed)
	}
	if len(got.Procs) != 1 || got.Procs[0].PID != 100 {
		t.Fatalf("Procs = %+v, want exactly pid 100", got.Procs)
	}
	for _, p := range got.Procs {
		if p.PID == 200 {
			t.Fatal("the unparseable-lstart pid 200 survived into the table")
		}
	}
	// Proof of the consequence: pid 200 cannot be a target even with its pgid
	// recorded and no other rule in the way.
	if ts := Targets(recorded(nil, []int{200}, nil), got, 100, 100, floorT.Add(-time.Hour)); len(ts) != 0 {
		t.Fatalf("targets = %v, want none — a dropped line must be unreachable", pids(ts))
	}
}

func TestParseTable_DropsMalformedIDsAndShortLines(t *testing.T) {
	raw := "  abc     1   100 Tue Aug 11 11:57:20 2026 /bad-pid\n" +
		"  100   xyz   100 Tue Aug 11 11:57:20 2026 /bad-ppid\n" +
		"  100     1   zzz Tue Aug 11 11:57:20 2026 /bad-pgid\n" +
		"  100     1   100 Tue Aug 11\n" +
		"\n" +
		"  101     1   101 Tue Aug 11 11:57:20 2026 /good\n"
	got := parseTable(raw)
	if got.Unparsed != 4 {
		t.Errorf("Unparsed = %d, want 4", got.Unparsed)
	}
	if len(got.Procs) != 1 || got.Procs[0].PID != 101 {
		t.Fatalf("Procs = %+v, want exactly pid 101", got.Procs)
	}
}

// --- Recorder ----------------------------------------------------------

// withTables installs a readTable stub returning the given tables in order (the
// last one repeating), and reports how many reads happened.
func withTables(t *testing.T, tables ...Table) func() int {
	t.Helper()
	restore := readTable
	t.Cleanup(func() { readTable = restore })
	// The counter is guarded: Recorder.Run samples from its own goroutine while
	// the test reads the count, so an unguarded int here would make -race red on
	// the FAKE rather than on anything under test (#3226).
	var mu sync.Mutex
	calls := 0
	readTable = func(context.Context) (Table, error) {
		mu.Lock()
		i := calls
		calls++
		mu.Unlock()
		if i >= len(tables) {
			i = len(tables) - 1
		}
		return tables[i], nil
	}
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

// withKillRecorder installs a killPID stub that records every pid it was asked
// to kill and returns errs[pid] (nil when absent).
func withKillRecorder(t *testing.T, errs map[int]error) *[]int {
	t.Helper()
	restore := killPID
	t.Cleanup(func() { killPID = restore })
	var called []int
	killPID = func(pid int) error {
		called = append(called, pid)
		return errs[pid]
	}
	return &called
}

func TestRecorder_SampleRecordsDescendantsAndPGIDs(t *testing.T) {
	withTables(t, table(
		proc(100, 1, 100, floorT, "runner"),
		proc(200, 100, 100, floorT, "agent"),
		proc(300, 200, 300, floorT, "sh"),
	))
	r := NewRecorder(100, floorT)
	if err := r.Sample(context.Background()); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	st := r.State()
	if _, ok := st.Descendants[300]; !ok {
		t.Error("descendant pid 300 not recorded")
	}
	if _, ok := st.PGIDs[300]; !ok {
		t.Error("pgid 300 not recorded")
	}
	if r.RecordedPGIDs() != 2 {
		t.Errorf("RecordedPGIDs = %d, want 2 (100 and 300)", r.RecordedPGIDs())
	}
}

func TestRecorder_SampleReturnsReadError(t *testing.T) {
	restore := readTable
	t.Cleanup(func() { readTable = restore })
	sentinel := errors.New("ps exploded")
	readTable = func(context.Context) (Table, error) { return Table{}, sentinel }
	r := NewRecorder(100, floorT)
	if err := r.Sample(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("Sample error = %v, want %v", err, sentinel)
	}
}

// p2 / c8 (rule b, first half): a recorded pgid whose members have ALL exited is
// moved to the tombstone set at the next sample and never re-admitted.
func TestRecorder_ContinuityDropsEmptiedPGIDPermanently(t *testing.T) {
	sample1 := table(
		proc(100, 1, 100, floorT, "runner"),
		proc(500, 100, 500, floorT, "sh"),
	)
	sample2 := table(proc(100, 1, 100, floorT, "runner")) // pgid 500 empty
	sample3 := table(                                     // a fresh descendant lands in the SAME reused group id
		proc(100, 1, 100, floorT, "runner"),
		proc(505, 100, 500, floorT.Add(time.Minute), "sh-again"),
	)
	withTables(t, sample1, sample2, sample3)
	r := NewRecorder(100, floorT)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := r.Sample(ctx); err != nil {
			t.Fatalf("Sample %d: %v", i+1, err)
		}
	}
	st := r.State()
	if _, tombstoned := st.Tombstones[500]; !tombstoned {
		t.Fatal("pgid 500 was not tombstoned after its group emptied")
	}
	if _, recordedPG := st.PGIDs[500]; recordedPG {
		t.Fatal("tombstoned pgid 500 was re-admitted by a later sample")
	}
	// pid 505 IS still a target — it is a recorded DESCENDANT, and the tombstone
	// only withdraws the pgid CREDENTIAL, not descendant admission.
	if _, ok := st.Descendants[505]; !ok {
		t.Error("pid 505 should still be recorded as a descendant")
	}
}

// p4 / c8's vehicle: the operator's explicitly requested case. A pgid is
// recorded, ALL its members exit, and a NEW unrelated process carrying that pgid
// with a LATER start appears. It passes the stage-start floor by construction —
// so ONLY the continuity tombstone keeps it out — is not self, has pid>1, and is
// not in the runner's group.
func TestRecorder_EmptiedPGIDReusedByLaterProcessIsNotTargeted(t *testing.T) {
	sample1 := table(
		proc(100, 1, 100, floorT, "runner"),
		proc(500, 100, 500, floorT, "sh"),
	)
	sample2 := table(proc(100, 1, 100, floorT, "runner")) // pgid 500 empty → tombstoned
	sweepTable := table(
		proc(100, 1, 100, floorT, "runner"),
		proc(501, 1, 500, floorT.Add(time.Minute), "somebody-elses-daemon"),
	)
	withTables(t, sample1, sample2, sweepTable)
	killed := withKillRecorder(t, nil)

	r := NewRecorder(100, floorT)
	ctx := context.Background()
	if err := r.Sample(ctx); err != nil {
		t.Fatalf("Sample 1: %v", err)
	}
	if err := r.Sample(ctx); err != nil {
		t.Fatalf("Sample 2: %v", err)
	}

	// Sanity: pid 501 clears every other rule, so the tombstone is the only
	// thing that can spare it.
	if 501 <= 1 || floorT.Add(time.Minute).Before(floorT.Truncate(time.Second)) {
		t.Fatal("fixture bug: pid 501 must clear the pid and floor rules")
	}

	got := Targets(r.State(), sweepTable, 100, 100, r.StartedAt())
	wantPIDs(t, got)

	res, err := r.Sweep(ctx, 100, 100)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(*killed) != 0 {
		t.Fatalf("killPID called for %v, want no kills", *killed)
	}
	if len(res.Reaped) != 0 {
		t.Fatalf("Reaped = %v, want none", pids(res.Reaped))
	}
}

// --- Sweep -------------------------------------------------------------

// p3 / c9: rule (c). readTable returns T1 at sample time (pid 300 is a genuine
// target at that instant) and T2 at sweep time, in which pid 300 is GONE and pid
// 301 carries its pgid with a start BEFORE the floor. Against the fresh read
// nothing is killed; against the stale sample killPID(300) would fire against a
// pid that no longer belongs to the stage.
func TestSweep_RecomputesTargetsFromFreshTableNotLastSample(t *testing.T) {
	sampleTable := table(
		proc(100, 1, 100, floorT, "runner"),
		proc(300, 100, 300, floorT.Add(time.Minute), "sh"),
	)
	sweepTable := table(
		proc(100, 1, 100, floorT, "runner"),
		proc(301, 1, 300, floorT.Add(-time.Hour), "pre-existing-in-reused-group"),
	)
	withTables(t, sampleTable, sweepTable)
	killed := withKillRecorder(t, nil)

	r := NewRecorder(100, floorT)
	ctx := context.Background()
	if err := r.Sample(ctx); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	// Precondition: pid 300 WAS a target at sample time, so a stale-sample
	// selection would genuinely kill it.
	if ts := Targets(r.State(), sampleTable, 100, 100, r.StartedAt()); len(ts) != 1 || ts[0].PID != 300 {
		t.Fatalf("fixture bug: sample-time targets = %v, want [300]", pids(ts))
	}

	res, err := r.Sweep(ctx, 100, 100)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(*killed) != 0 {
		t.Fatalf("killPID called for %v, want no kills — selection must use the sweep-time read", *killed)
	}
	if len(res.Reaped) != 0 {
		t.Fatalf("Reaped = %v, want none", pids(res.Reaped))
	}
}

func TestSweep_KillsRecordedDescendantAndPGIDMember(t *testing.T) {
	live := table(
		proc(100, 1, 100, floorT, "runner"),
		proc(200, 100, 100, floorT, "agent"),
		proc(400, 1, 300, floorT.Add(time.Minute), "orphan-busyloop"),
	)
	withTables(t, live)
	killed := withKillRecorder(t, nil)

	r := NewRecorder(100, floorT)
	ctx := context.Background()
	// Record pgid 300 through a descendant that then exits.
	r.fold(table(
		proc(100, 1, 100, floorT, "runner"),
		proc(300, 100, 300, floorT, "sh"),
	))
	if err := r.Sample(ctx); err != nil { // folds `live`; pgid 300 still has pid 400 in it
		t.Fatalf("Sample: %v", err)
	}
	res, err := r.Sweep(ctx, 100, 100)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	wantPIDs(t, res.Reaped, 200, 400)
	if len(*killed) != 2 {
		t.Fatalf("killPID calls = %v, want 2", *killed)
	}
}

// m6 / c6: ESRCH is counted as already-gone, never an error, and the REMAINING
// targets are still killed. The assertion reads the recorded kill LIST after the
// call returns (committed state), not only the returned error.
func TestSweep_ESRCHCountedAsAlreadyGone(t *testing.T) {
	live := table(
		proc(100, 1, 100, floorT, "runner"),
		proc(200, 100, 100, floorT, "gone"),
		proc(201, 100, 100, floorT, "alive"),
	)
	withTables(t, live)
	killed := withKillRecorder(t, map[int]error{200: syscall.ESRCH})

	r := NewRecorder(100, floorT)
	ctx := context.Background()
	if err := r.Sample(ctx); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	res, err := r.Sweep(ctx, 100, 100)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("Errors = %v, want none — ESRCH is already-gone, not a failure", res.Errors)
	}
	if res.AlreadyGone != 1 {
		t.Errorf("AlreadyGone = %d, want 1", res.AlreadyGone)
	}
	wantPIDs(t, res.Reaped, 201)
	if len(*killed) != 2 {
		t.Fatalf("killPID calls = %v, want both targets attempted", *killed)
	}
}

// m7: a non-ESRCH kill error is collected into Result.Errors, the sweep
// CONTINUES to the remaining targets, and Sweep itself does not return an error.
func TestSweep_NonESRCHKillErrorIsCollectedAndSweepContinues(t *testing.T) {
	live := table(
		proc(100, 1, 100, floorT, "runner"),
		proc(200, 100, 100, floorT, "eperm"),
		proc(201, 100, 100, floorT, "alive"),
	)
	withTables(t, live)
	killed := withKillRecorder(t, map[int]error{200: syscall.EPERM})

	r := NewRecorder(100, floorT)
	ctx := context.Background()
	if err := r.Sample(ctx); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	res, err := r.Sweep(ctx, 100, 100)
	if err != nil {
		t.Fatalf("Sweep returned an error %v — a kill failure must not abort the stage", err)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "kill 200") {
		t.Fatalf("Errors = %v, want one naming pid 200", res.Errors)
	}
	wantPIDs(t, res.Reaped, 201)
	if len(*killed) != 2 {
		t.Fatalf("killPID calls = %v, want both targets attempted", *killed)
	}
}

// m3: a ps read error at sweep time returns the error with ZERO kills — the
// caller degrades fail-open.
func TestSweep_ReadErrorReturnsErrorWithZeroKills(t *testing.T) {
	restore := readTable
	t.Cleanup(func() { readTable = restore })
	readTable = func(context.Context) (Table, error) { return Table{}, errors.New("ps: no such file") }
	killed := withKillRecorder(t, nil)

	r := NewRecorder(100, floorT)
	r.fold(table(proc(100, 1, 100, floorT, "runner"), proc(200, 100, 100, floorT, "agent")))
	if _, err := r.Sweep(context.Background(), 100, 100); err == nil {
		t.Fatal("Sweep returned nil error on a read failure")
	}
	if len(*killed) != 0 {
		t.Fatalf("killPID called for %v on a read failure, want no kills", *killed)
	}
}

// m4: the platform stub's ErrUnsupported reaches the caller with zero kills.
func TestSweep_UnsupportedPlatformReturnsErrUnsupported(t *testing.T) {
	restore := readTable
	t.Cleanup(func() { readTable = restore })
	readTable = func(context.Context) (Table, error) { return Table{}, ErrUnsupported }
	killed := withKillRecorder(t, nil)

	r := NewRecorder(100, floorT)
	_, err := r.Sweep(context.Background(), 100, 100)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Sweep error = %v, want ErrUnsupported", err)
	}
	if len(*killed) != 0 {
		t.Fatalf("killPID called for %v, want no kills", *killed)
	}
}

func TestSweep_IsIdempotent(t *testing.T) {
	live := table(
		proc(100, 1, 100, floorT, "runner"),
		proc(200, 100, 100, floorT, "agent"),
	)
	withTables(t, live)
	killed := withKillRecorder(t, nil)

	r := NewRecorder(100, floorT)
	ctx := context.Background()
	if err := r.Sample(ctx); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	first, err := r.Sweep(ctx, 100, 100)
	if err != nil {
		t.Fatalf("Sweep 1: %v", err)
	}
	wantPIDs(t, first.Reaped, 200)
	second, err := r.Sweep(ctx, 100, 100)
	if err != nil {
		t.Fatalf("Sweep 2: %v", err)
	}
	if len(second.Reaped) != 0 {
		t.Fatalf("second Sweep reaped %v, want none", pids(second.Reaped))
	}
	if len(*killed) != 1 {
		t.Fatalf("killPID calls = %v, want exactly 1 across two sweeps", *killed)
	}
}

func TestRun_SamplesUntilContextCancelled(t *testing.T) {
	calls := withTables(t, table(proc(100, 1, 100, floorT, "runner"), proc(200, 100, 100, floorT, "agent")))
	r := NewRecorder(100, floorT)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx, time.Millisecond)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for calls() == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Run took no sample within 5s")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	if _, ok := r.State().Descendants[200]; !ok {
		t.Error("Run's samples did not reach the recorded state")
	}
}

// The fixture helpers above compare StartRaw built from lstartLayout, so a
// layout change would silently weaken every identity assertion. Pin the format.
func TestLstartLayoutRendersFiveTokens(t *testing.T) {
	rendered := floorT.Format(lstartLayout)
	if got := len(strings.Fields(rendered)); got != lstartTokens {
		t.Fatalf("lstartLayout renders %d tokens (%q), want %d", got, rendered, lstartTokens)
	}
	if _, err := time.ParseInLocation(lstartLayout, rendered, time.UTC); err != nil {
		t.Fatalf("lstartLayout does not round-trip: %v", err)
	}
	_ = fmt.Sprint(rendered)
}
