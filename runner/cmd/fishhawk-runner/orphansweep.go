package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/procsweep"
)

// orphanSweepInterval is how often the recorder samples the host process table
// during the stage. Two seconds is the window the pgid-reuse residual in
// runner/internal/procsweep/README.md is measured against: shortening it narrows
// that window at linear `ps` cost.
const orphanSweepInterval = 2 * time.Second

// orphanSweepMaxSampled bounds the {pid, command} sample carried on the
// stage_orphans_reaped event, so a pathological stage cannot balloon the bundle.
const orphanSweepMaxSampled = 10

// orphanSweepDefaultOn is the package-test-binary KILL SWITCH. runTestMain sets
// it FALSE, so under this package's ~31k-line test binary the sweeper
// constructs into the named `disabled_in_test_binary` degrade and kills
// nothing: those tests run inside the `go test` harness's own process tree, and
// a future refactor that made the test binary its own process-group leader
// would otherwise have the sweeper SIGKILL the harness's children. A test that
// needs the real sweeper opts in via enableOrphanSweepForTest.
var orphanSweepDefaultOn = true

// stageOrphanSweeper records the runner's descendant + process-group closure
// during a stage and SIGKILLs the survivors at stage exit (#3663).
//
// Every degrade is FAIL-OPEN with one printed reason and ZERO kills: the
// package-test-binary kill switch, FISHHAWK_ORPHAN_SWEEP=off, a runner that is
// not its own process-group leader with no descendant pgids recorded,
// procsweep.ErrUnsupported (Windows), and a `ps` read error at sweep time.
type stageOrphanSweeper struct {
	cfg      config
	sink     io.Writer
	rec      *procsweep.Recorder
	selfPID  int
	selfPGID int
	disabled string // non-empty = a named pre-start degrade; sweep() is a no-op

	// sweepFn is the kill pass, injectable so the wiring tests can drive the
	// ps-read-error and ErrUnsupported degrades without a real process table.
	sweepFn func(context.Context, int, int) (procsweep.Result, error)

	stop context.CancelFunc
	wg   sync.WaitGroup
	once sync.Once
	ev   *agent.Event

	// degraded names the fail-open reason the sweep took, "" when it ran. Set by
	// degrade(); read by the wiring tests, which assert on the printed reason
	// rather than on a bundle event (a degrade is deliberately LOG-ONLY).
	degradedReason string
}

// selfProcessGroup is injectable so the wiring tests can pose the runner as a
// group leader or as a member of somebody else's group.
var selfProcessGroup = procsweep.SelfPGID

// newStageOrphanSweeper constructs the sweeper and resolves the pre-start
// degrades. getenv is injected so the FISHHAWK_ORPHAN_SWEEP=off escape is
// testable without mutating the process environment.
func newStageOrphanSweeper(sink io.Writer, cfg config, getenv func(string) string, now time.Time) *stageOrphanSweeper {
	s := &stageOrphanSweeper{cfg: cfg, sink: sink, selfPID: os.Getpid()}
	s.selfPGID = selfProcessGroup(s.selfPID)
	s.rec = procsweep.NewRecorder(s.selfPID, now)
	s.sweepFn = s.rec.Sweep

	switch {
	case !orphanSweepDefaultOn:
		s.disabled = "disabled_in_test_binary"
	case strings.EqualFold(strings.TrimSpace(getenv("FISHHAWK_ORPHAN_SWEEP")), "off"):
		s.disabled = "disabled_by_operator"
	}
	return s
}

// start launches the sampling goroutine. A disabled sweeper samples nothing.
func (s *stageOrphanSweeper) start(ctx context.Context) {
	if s == nil || s.disabled != "" {
		return
	}
	sampleCtx, cancel := context.WithCancel(ctx)
	s.stop = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// Take one reading immediately so a stage shorter than the interval
		// still records the agent's own shell.
		_ = s.rec.Sample(sampleCtx)
		s.rec.Run(sampleCtx, orphanSweepInterval)
	}()
}

// stopSampling cancels the sampler and joins it, so the sampling goroutine can
// never race the post-invoke logSink writes on the main goroutine (the #1035
// discipline the scope-amendment watcher already follows).
func (s *stageOrphanSweeper) stopSampling() {
	if s == nil || s.stop == nil {
		return
	}
	s.stop()
	s.wg.Wait()
	s.stop = nil
}

// sweep performs the kill pass exactly once, whichever call site reaches it
// first: the normal post-invoke path (whose returned event rides in the signed
// bundle) or the run() defer that backstops the cancel / timeout / early-return
// paths.
//
// It returns a stage_orphans_reaped event ONLY when the sweep actually did
// something — reaped a process, found one already gone, or collected a kill
// error. A named DEGRADE is LOG-ONLY and returns nil, as is the zero-orphan
// common case: a stage that reaped nothing (for whatever reason) leaves a trace
// byte-identical to before this change, so no existing bundle grows an event and
// the operator escape FISHHAWK_ORPHAN_SWEEP=off costs nothing in the bundle.
func (s *stageOrphanSweeper) sweep(ctx context.Context) *agent.Event {
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		s.stopSampling()
		s.ev = s.sweepOnce(ctx)
	})
	return s.ev
}

func (s *stageOrphanSweeper) sweepOnce(ctx context.Context) *agent.Event {
	if s.disabled != "" {
		return s.degrade(s.disabled, "")
	}
	// A runner that does not LEAD its own process group is almost always a
	// hand-run `bin/fishhawk-runner` sharing the operator's interactive shell's
	// group. Targets' group-leader guard already refuses to touch that group, so
	// with nothing recorded outside it there is nothing this sweep could ever
	// authorise — decline before reading the table at all.
	if s.selfPGID != s.selfPID && s.rec.RecordedPGIDs() == 0 {
		return s.degrade("not_group_leader_no_recorded_pgids", "")
	}

	res, err := s.sweepFn(ctx, s.selfPID, s.selfPGID)
	if err != nil {
		reason := "proc_table_read_failed"
		if errors.Is(err, procsweep.ErrUnsupported) {
			reason = "proc_table_unsupported"
		}
		return s.degrade(reason, err.Error())
	}
	if len(res.Reaped) == 0 && res.AlreadyGone == 0 && len(res.Errors) == 0 {
		return nil
	}

	sampled := make([]map[string]any, 0, orphanSweepMaxSampled)
	for i, p := range res.Reaped {
		if i >= orphanSweepMaxSampled {
			break
		}
		sampled = append(sampled, map[string]any{"pid": p.PID, "command": p.Command})
	}
	payload := map[string]any{
		"count":        len(res.Reaped),
		"already_gone": res.AlreadyGone,
		"sampled":      sampled,
	}
	if len(res.Errors) > 0 {
		payload["errors"] = res.Errors
	}
	_, _ = fmt.Fprintf(s.sink,
		`{"event":"stage_orphans_reaped","run_id":%q,"stage_id":%q,"count":%d,"already_gone":%d,"errors":%d}`+"\n",
		s.cfg.runID, s.cfg.stageID, len(res.Reaped), res.AlreadyGone, len(res.Errors))
	return &agent.Event{Kind: "stage_orphans_reaped", Payload: agent.MakePayload(payload)}
}

// degrade records and PRINTS the named fail-open reason, and returns nil so no
// bundle event is appended. ZERO kills happened. degradeReason() exposes the
// reason to the wiring tests.
func (s *stageOrphanSweeper) degrade(reason, detail string) *agent.Event {
	s.degradedReason = reason
	_, _ = fmt.Fprintf(s.sink,
		`{"event":"stage_orphan_sweep_degraded","run_id":%q,"stage_id":%q,"reason":%q,"detail":%q}`+"\n",
		s.cfg.runID, s.cfg.stageID, reason, detail)
	return nil
}

// degradeReason is the named fail-open reason the sweep took, "" when it ran.
func (s *stageOrphanSweeper) degradeReason() string {
	if s == nil {
		return ""
	}
	return s.degradedReason
}
