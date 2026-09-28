// Package procsweep is the runner's best-effort reaper for agent-spawned
// processes that OUTLIVE a stage (#3663).
//
// The problem it solves: an agent can background a process from a shell that
// then exits. The orphan is re-parented to pid 1 and, if the shell had job
// control off, keeps the EXITED shell's process-group id — so neither the
// runner's own ppid chain nor a `kill(-pgid)` against the agent's group reaches
// it at stage exit. The #3663 incident left a handful of CPU busy-loops running
// on the dogfood host, and every later committed-tree verify on that host
// red-lined on tests unrelated to its own diff.
//
// The mechanism: the runner SAMPLES the host process table during the stage,
// accumulating (a) the transitive ppid closure of the runner process and (b)
// the set of process-group ids those descendants belong to. At stage exit it
// RE-READS the table and SIGKILLs every survivor admitted by that recorded
// state. The pgid half is what reaches an orphan whose whole ancestor chain has
// already exited: only the SHELL — which lives for the duration of the agent's
// command — needs to have been sampled, not the short-lived orphan itself.
//
// Because a pgid is a recyclable integer with no lifetime guarantee once its
// group empties, the pgid half is fenced by three rules, each independently
// tested (see README.md):
//
//	(a) STAGE-START FLOOR   — a process admitted ONLY by the pgid closure is a
//	                          target only if it started at or after the
//	                          recorder's start instant, so nothing predating the
//	                          stage can ever be reached.
//	(b) CONTINUITY          — a recorded pgid with zero live members at a sample
//	                          is TOMBSTONED (dropped permanently, never
//	                          re-admitted), so an emptied group cannot authorise
//	                          kills against a later group that reuses its id.
//	(c) RE-CHECK AT SWEEP   — the table is re-read immediately before killing
//	                          and selection runs against that FRESH read, never
//	                          against a stale sample.
//
// This is a best-effort reaper, NOT a containment boundary: a descendant that
// both starts and escapes entirely between two samples, and whose whole
// ancestor chain also exits inside that window, is never recorded and never
// reaped. The structural fix is a cgroup (Linux) or a job object (Windows),
// neither of which exists on macOS — the primary dogfood host.
package procsweep

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrUnsupported is returned by the platform seams on a host where the sweep
// cannot be performed (Windows: no `ps`, no portable process-group signal).
// Callers degrade fail-open — one printed reason, zero kills.
var ErrUnsupported = errors.New("procsweep: unsupported on this platform")

// SetTestHooks replaces the process-table reader and the kill seam, returning a
// restore func. It exists because the runner command's wiring test must drive
// the REAL Recorder/Sweep against a synthetic process table — the seams
// themselves stay unexported so no production code can reach them. Pass nil for
// either hook to leave it unchanged.
//
// NOT safe for concurrent use: call it from one test at a time and defer the
// restore.
func SetTestHooks(read func(context.Context) (Table, error), kill func(int) error) (restore func()) {
	prevRead, prevKill := readTable, killPID
	if read != nil {
		readTable = read
	}
	if kill != nil {
		killPID = kill
	}
	return func() { readTable, killPID = prevRead, prevKill }
}

// FormatStart renders t the way ps lstart does, so a synthetic Proc built by a
// consumer package's test carries a StartRaw the identity discriminator accepts.
func FormatStart(t time.Time) string { return t.Format(lstartLayout) }

// Proc is one row of the host process table.
//
// Start is the ABSOLUTE start instant parsed from `ps lstart`; it must be
// ORDERED against the stage-start floor, which is why the raw text alone is not
// enough. StartRaw retains that raw lstart text as the recorded-pid identity
// discriminator: it is EQUALITY-compared against the recorded value and never
// re-parsed at compare time, so a pid the kernel recycled onto a different
// process is not mistaken for the one that was recorded.
//
// `etime` is deliberately absent: elapsed time shifts between samples and
// cannot serve as an identity discriminator.
type Proc struct {
	PID      int
	PPID     int
	PGID     int
	Start    time.Time
	StartRaw string
	Command  string
}

// Table is a parsed process-table reading. Unparsed counts the lines DROPPED
// because their three leading integers or their lstart field could not be
// parsed: a dropped line is invisible to Descendants, to Targets and to the
// continuity rule, so such a process can never be a target. That is the
// fail-closed direction — fewer kills, never a kill on an unidentified pid.
type Table struct {
	Procs    []Proc
	Unparsed int
}

// lstartLayout is ps(1)'s ctime-style lstart rendering, on both Darwin and
// procps-ng: five whitespace-separated tokens, e.g. "Tue Aug 11 11:57:20 2026".
const lstartLayout = "Mon Jan _2 15:04:05 2006"

// lstartTokens is how many whitespace-separated tokens lstart occupies.
const lstartTokens = 5

// parseLocation is the location lstart timestamps are interpreted in. ps prints
// LOCAL wall-clock time with NO zone field, so parsing must be anchored to the
// host's location: time.Parse would silently read those digits as UTC and skew
// every Start by the host's UTC offset, which on a UTC+N host makes a genuine
// descendant read as started BEFORE the stage-start floor (sparing it) and on a
// UTC-N host makes a pre-existing process read as started after it (a kill
// against a process that predates the stage). Injectable so the parser tests
// can fix a non-UTC location and assert the resulting absolute instant.
var parseLocation = time.Local

// parseTable parses raw `ps -axo pid=,ppid=,pgid=,lstart=,command=` output.
//
// Each line is three integers, then lstart as exactly five whitespace-separated
// tokens, then the command as the remainder. A line that does not have that
// shape — or whose lstart does not parse — is DROPPED and counted in
// Table.Unparsed (fail closed: an unidentifiable process is never a target).
func parseTable(raw string) Table {
	var t Table
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3+lstartTokens+1 {
			t.Unparsed++
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		pgid, err3 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || err3 != nil {
			t.Unparsed++
			continue
		}
		startRaw := strings.Join(fields[3:3+lstartTokens], " ")
		start, err := time.ParseInLocation(lstartLayout, startRaw, parseLocation)
		if err != nil {
			t.Unparsed++
			continue
		}
		t.Procs = append(t.Procs, Proc{
			PID:      pid,
			PPID:     ppid,
			PGID:     pgid,
			Start:    start,
			StartRaw: startRaw,
			Command:  strings.Join(fields[3+lstartTokens:], " "),
		})
	}
	return t
}

// Descendants returns the transitive ppid closure of anchor, EXCLUDING anchor
// itself. Iterative with a visited set (anchor pre-marked) so a ppid cycle from
// a torn table read cannot loop forever.
func Descendants(t Table, anchor int) map[int]Proc {
	children := make(map[int][]Proc, len(t.Procs))
	for _, p := range t.Procs {
		children[p.PPID] = append(children[p.PPID], p)
	}
	out := make(map[int]Proc)
	visited := map[int]bool{anchor: true}
	queue := []int{anchor}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range children[cur] {
			if visited[c.PID] {
				continue
			}
			visited[c.PID] = true
			out[c.PID] = c
			queue = append(queue, c.PID)
		}
	}
	return out
}

// RecordedState is the sweep-authorising state a Recorder accumulated: the
// descendants it ever observed (keyed by pid, each holding the StartRaw it was
// observed with), the live pgid set those descendants belonged to, and the
// tombstone set of pgids the continuity rule dropped permanently.
type RecordedState struct {
	Descendants map[int]Proc
	PGIDs       map[int]struct{}
	Tombstones  map[int]struct{}
}

// Targets selects the processes in live that the recorded state authorises a
// SIGKILL against, sorted by pid.
//
// Two admission paths, DELIBERATELY asymmetric:
//
//	(i)  RECORDED DESCENDANT   — a recorded pid present in live whose StartRaw
//	                             still equals the recorded StartRaw. A changed
//	                             StartRaw means the kernel recycled the pid onto
//	                             a different process, which is NOT a target.
//	(ii) PGID-CLOSURE MEMBER   — a live process whose pgid is in the recorded
//	                             set, NOT tombstoned, and whose Start is at or
//	                             after floor.
//
// Then three exclusions apply to BOTH paths: never the runner's own pid; never
// pid <= 1; and the GROUP-LEADER GUARD — never any member of the runner's own
// process group unless the runner LEADS it (selfPGID == selfPID), which stops a
// hand-run `bin/fishhawk-runner` (whose pgid is the operator's interactive
// shell's) from sweeping that shell. The guard mirrors the shipped precedent in
// the runner command's signalLockHolder.
//
// floor is compared at WHOLE-SECOND resolution (floor.Truncate(time.Second)),
// because that is the resolution ps lstart carries: comparing a second-granular
// Start against a sub-second floor would spare a genuine descendant spawned in
// the same wall-clock second as the stage start, which is the worse trade for a
// reaper. The cost is stated in README.md: a pre-existing process started in
// the SAME wall-clock second as the stage start is admitted.
func Targets(rec RecordedState, live Table, selfPID, selfPGID int, floor time.Time) []Proc {
	secFloor := floor.Truncate(time.Second)
	leadsOwnGroup := selfPGID == selfPID
	var out []Proc
	for _, p := range live.Procs {
		if p.PID == selfPID || p.PID <= 1 {
			continue
		}
		if !leadsOwnGroup && p.PGID == selfPGID {
			continue
		}
		admitted := false
		if r, ok := rec.Descendants[p.PID]; ok && r.StartRaw == p.StartRaw {
			admitted = true
		}
		if !admitted {
			if _, tombstoned := rec.Tombstones[p.PGID]; !tombstoned {
				if _, recorded := rec.PGIDs[p.PGID]; recorded && !p.Start.Before(secFloor) {
					admitted = true
				}
			}
		}
		if admitted {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// Result is what one Sweep did. AlreadyGone counts targets whose kill returned
// ESRCH (the process exited between the fresh read and the signal — expected,
// never an error). Errors collects every non-ESRCH kill failure WITHOUT
// aborting the remaining kills.
type Result struct {
	Reaped      []Proc
	AlreadyGone int
	Errors      []string
	Unparsed    int
}

// Recorder accumulates sweep-authorising state across samples taken during a
// stage. The sampler goroutine writes while the stage goroutine reads at exit,
// so every field is guarded by mu (#3226: a struct reachable from two
// goroutines guards its own bookkeeping).
type Recorder struct {
	mu          sync.Mutex
	anchor      int
	startedAt   time.Time
	descendants map[int]Proc
	pgids       map[int]struct{}
	tombstones  map[int]struct{}
	samples     int
	unparsed    int
}

// NewRecorder returns a Recorder anchored at the given pid whose stage-start
// floor is now. Every pgid-only admission is gated on that floor, so `now` must
// be captured at stage start, before the agent runs.
func NewRecorder(anchor int, now time.Time) *Recorder {
	return &Recorder{
		anchor:      anchor,
		startedAt:   now,
		descendants: make(map[int]Proc),
		pgids:       make(map[int]struct{}),
		tombstones:  make(map[int]struct{}),
	}
}

// StartedAt is the stage-start floor.
func (r *Recorder) StartedAt() time.Time { return r.startedAt }

// Sample takes ONE process-table reading and folds it in synchronously. The
// tests drive this directly, so no test depends on a timer.
func (r *Recorder) Sample(ctx context.Context) error {
	t, err := readTable(ctx)
	if err != nil {
		return err
	}
	r.fold(t)
	return nil
}

// fold is Sample's pure-ish half: record newly-seen descendants and their
// pgids, THEN apply the continuity rule. The ordering is load-bearing — a
// freshly recorded pgid has at least one live member in this very table, so it
// cannot be tombstoned by the same fold that recorded it.
func (r *Recorder) fold(t Table) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples++
	r.unparsed += t.Unparsed

	for pid, p := range Descendants(t, r.anchor) {
		if _, seen := r.descendants[pid]; !seen {
			r.descendants[pid] = p
		}
		if p.PGID <= 0 {
			continue
		}
		if _, tombstoned := r.tombstones[p.PGID]; tombstoned {
			// Permanently dropped by the continuity rule — never re-admitted,
			// even for a fresh descendant that happens to land in that group.
			continue
		}
		r.pgids[p.PGID] = struct{}{}
	}

	// CONTINUITY (rule b): a recorded pgid with ZERO live members in this
	// reading is tombstoned, so a later group reusing that id can never
	// authorise a kill.
	liveMembers := make(map[int]int, len(t.Procs))
	for _, p := range t.Procs {
		liveMembers[p.PGID]++
	}
	for pg := range r.pgids {
		if liveMembers[pg] == 0 {
			delete(r.pgids, pg)
			r.tombstones[pg] = struct{}{}
		}
	}
}

// State returns a snapshot copy of the recorded state.
func (r *Recorder) State() RecordedState {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := RecordedState{
		Descendants: make(map[int]Proc, len(r.descendants)),
		PGIDs:       make(map[int]struct{}, len(r.pgids)),
		Tombstones:  make(map[int]struct{}, len(r.tombstones)),
	}
	for k, v := range r.descendants {
		st.Descendants[k] = v
	}
	for k := range r.pgids {
		st.PGIDs[k] = struct{}{}
	}
	for k := range r.tombstones {
		st.Tombstones[k] = struct{}{}
	}
	return st
}

// RecordedPGIDs reports how many pgids are currently recorded (not
// tombstoned). The wiring consults it for the "not a group leader and nothing
// recorded" degrade.
func (r *Recorder) RecordedPGIDs() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pgids)
}

// Run samples at interval until ctx is cancelled. Sample errors are returned to
// nobody on purpose: a transient ps failure mid-stage must not abort the stage,
// and the sweep's own fresh read reports a read failure at the point it matters.
func (r *Recorder) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = r.Sample(ctx)
		}
	}
}

// Sweep RE-READS the process table and SIGKILLs every target that fresh read
// authorises (rule c): selection NEVER runs against the last sample, because a
// pid that belonged to the stage one interval ago may not belong to it now.
//
// A read failure returns the error with ZERO kills — the caller degrades
// fail-open. After the kill pass the recorded state is CLEARED (every recorded
// pgid moved to the tombstone set), so a second Sweep finds nothing: the sweep
// is terminal, and forgetting is the fail-safe direction.
func (r *Recorder) Sweep(ctx context.Context, selfPID, selfPGID int) (Result, error) {
	live, err := readTable(ctx)
	if err != nil {
		return Result{}, err
	}
	targets := Targets(r.State(), live, selfPID, selfPGID, r.StartedAt())

	var res Result
	res.Unparsed = live.Unparsed
	for _, p := range targets {
		if kerr := killPID(p.PID); kerr != nil {
			if errors.Is(kerr, syscall.ESRCH) {
				res.AlreadyGone++
				continue
			}
			res.Errors = append(res.Errors, fmt.Sprintf("kill %d (%s): %v", p.PID, p.Command, kerr))
			continue
		}
		res.Reaped = append(res.Reaped, p)
	}

	r.mu.Lock()
	r.descendants = make(map[int]Proc)
	for pg := range r.pgids {
		r.tombstones[pg] = struct{}{}
	}
	r.pgids = make(map[int]struct{})
	r.mu.Unlock()

	return res, nil
}
