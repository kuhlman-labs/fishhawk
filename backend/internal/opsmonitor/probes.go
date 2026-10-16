package opsmonitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stalesweep"
)

// ExecFunc runs one command and returns its stdout. Probes take it as a test
// seam; nil selects DefaultExec.
type ExecFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// execWaitDelay bounds Wait after the child exits or ctx is done: a
// grandchild that inherited stdout cannot keep the pipe (and Wait) open.
const execWaitDelay = 2 * time.Second

// DefaultExec is the production ExecFunc: exec.CommandContext with
// cmd.WaitDelay, so a cancelled probe cannot wedge on a grandchild holding
// its pipe.
func DefaultExec(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = execWaitDelay
	return cmd.Output()
}

func execOrDefault(fn ExecFunc) ExecFunc {
	if fn != nil {
		return fn
	}
	return DefaultExec
}

const gib = 1 << 30

func gibString(b uint64) string { return fmt.Sprintf("%.1f GiB", float64(b)/gib) }

// --- disk -------------------------------------------------------------------

// DiskProbe reports data-volume free space (statfs f_bavail * f_bsize).
type DiskProbe struct {
	Path         string
	MinFreeBytes uint64
	// Statfs is a test seam; nil uses statfs(2).
	Statfs func(path string) (avail, total uint64, err error)
}

// Name implements Probe.
func (DiskProbe) Name() string { return "disk" }

// HostLocal implements Probe.
func (DiskProbe) HostLocal() bool { return true }

// Check implements Probe.
func (p DiskProbe) Check(context.Context) []Result {
	if p.Path == "" {
		return []Result{unknown(p.Name(), "no data-volume path configured")}
	}
	statfs := p.Statfs
	if statfs == nil {
		statfs = statfsBytes
	}
	avail, total, err := statfs(p.Path)
	if err != nil {
		return []Result{unknown(p.Name(), "statfs failed: "+err.Error())}
	}
	r := Result{
		Probe:     p.Name(),
		Status:    StatusOK,
		Observed:  fmt.Sprintf("%s free of %s", gibString(avail), gibString(total)),
		Threshold: fmt.Sprintf(">= %s free", gibString(p.MinFreeBytes)),
		Detail:    map[string]any{"path": p.Path, "avail_bytes": avail, "total_bytes": total},
	}
	if avail < p.MinFreeBytes {
		r.Status = StatusFailing
		r.Condition = ConditionDiskFreeLow
	}
	return []Result{r}
}

// --- gocache ----------------------------------------------------------------

// goCacheReadmeMarker is the text cmd/go writes into GOCACHE/README (the same
// marker scripts/dev's _GOCACHE_README_MARKER checks).
const goCacheReadmeMarker = "cached build artifacts from the Go build system"

// goCacheSampleShards are the 16 of 256 two-hex-digit shards scripts/dev's
// _gocache_estimate_kb samples; entries spread uniformly by hash first byte.
var goCacheSampleShards = []string{"00", "10", "20", "30", "40", "50", "60", "70", "80", "90", "a0", "b0", "c0", "d0", "e0", "f0"}

// GoCacheProbe reports the sampled GOCACHE size estimate.
type GoCacheProbe struct {
	// Dir is the cache root; empty resolves it at Check time via ResolveDir.
	Dir      string
	MaxBytes uint64
	// ResolveDir is a test seam; nil runs ResolveGoCacheDir.
	ResolveDir func(ctx context.Context) (string, error)
}

// Name implements Probe.
func (GoCacheProbe) Name() string { return "gocache" }

// HostLocal implements Probe.
func (GoCacheProbe) HostLocal() bool { return true }

// ResolveGoCacheDir runs a bounded `go env GOCACHE` (honoring both the env
// var and the go env file). Called lazily at Check time, never at flag
// registration.
func ResolveGoCacheDir(ctx context.Context) (string, error) {
	return resolveGoCacheDirWith(ctx, DefaultExec)
}

func resolveGoCacheDirWith(ctx context.Context, execFn ExecFunc) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := execFn(ctx, "go", "env", "GOCACHE")
	if err != nil {
		return "", fmt.Errorf("go env GOCACHE: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Check implements Probe.
func (p GoCacheProbe) Check(ctx context.Context) []Result {
	dir := p.Dir
	if dir == "" {
		resolve := p.ResolveDir
		if resolve == nil {
			resolve = ResolveGoCacheDir
		}
		d, err := resolve(ctx)
		if err != nil {
			return []Result{unknown(p.Name(), "could not resolve GOCACHE: "+err.Error())}
		}
		dir = d
	}
	if dir == "" || dir == "off" {
		return []Result{unknown(p.Name(), fmt.Sprintf("GOCACHE is %q: no cache to measure", dir))}
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return []Result{unknown(p.Name(), "GOCACHE root is missing or not a directory")}
	}
	readme, err := os.ReadFile(filepath.Join(dir, "README"))
	if err != nil || !strings.Contains(string(readme), goCacheReadmeMarker) {
		return []Result{unknown(p.Name(), "not a Go build cache (README marker absent)")}
	}
	var sum uint64
	for _, shard := range goCacheSampleShards {
		entries, err := os.ReadDir(filepath.Join(dir, shard))
		if err != nil {
			continue // an absent shard samples as zero, as scripts/dev does
		}
		for _, e := range entries {
			if ctx.Err() != nil {
				return []Result{unknown(p.Name(), "cache walk cancelled")}
			}
			if !e.Type().IsRegular() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue // evicted mid-walk
			}
			sum += uint64(info.Size()) //nolint:gosec // a regular file's size is non-negative
		}
	}
	estimate := sum * uint64(256/len(goCacheSampleShards))
	r := Result{
		Probe:     p.Name(),
		Status:    StatusOK,
		Observed:  fmt.Sprintf("~%s (sampled estimate)", gibString(estimate)),
		Threshold: fmt.Sprintf("<= %s", gibString(p.MaxBytes)),
		Detail:    map[string]any{"dir": dir, "estimate_bytes": estimate},
	}
	if estimate > p.MaxBytes {
		r.Status = StatusFailing
		r.Condition = ConditionGoCacheOverThreshold
	}
	return []Result{r}
}

// --- load -------------------------------------------------------------------

// DefaultLoadFactor matches the runner's hostload.DefaultFactor.
const DefaultLoadFactor = 4.0

// LoadProbe reports the 1-minute load average against Factor x cores.
type LoadProbe struct {
	Factor float64
	// ReadLoad and Cores are test seams; nil reads the host
	// (/proc/loadavg, `sysctl -n vm.loadavg`, `uptime`) and runtime.NumCPU.
	ReadLoad func(ctx context.Context) (float64, error)
	Cores    func() int
}

// Name implements Probe.
func (LoadProbe) Name() string { return "load" }

// HostLocal implements Probe.
func (LoadProbe) HostLocal() bool { return true }

// Check implements Probe.
func (p LoadProbe) Check(ctx context.Context) []Result {
	read := p.ReadLoad
	if read == nil {
		read = func(ctx context.Context) (float64, error) {
			return readLoad1(ctx, os.ReadFile, DefaultExec)
		}
	}
	coresFn := p.Cores
	if coresFn == nil {
		coresFn = runtime.NumCPU
	}
	load, err := read(ctx)
	if err != nil {
		return []Result{unknown(p.Name(), "load average unreadable: "+err.Error())}
	}
	cores := coresFn()
	if cores <= 0 {
		return []Result{unknown(p.Name(), fmt.Sprintf("core count %d is not positive", cores))}
	}
	factor := p.Factor
	if factor <= 0 {
		factor = DefaultLoadFactor
	}
	limit := factor * float64(cores)
	r := Result{
		Probe:     p.Name(),
		Status:    StatusOK,
		Observed:  fmt.Sprintf("1-minute load %.2f on %d cores", load, cores),
		Threshold: fmt.Sprintf("<= %.1f (%.1f x %d cores)", limit, factor, cores),
		Detail:    map[string]any{"load1": load, "cores": cores, "factor": factor},
	}
	if load > limit {
		r.Status = StatusFailing
		r.Condition = ConditionHostLoadHigh
	}
	return []Result{r}
}

// uptimeLoadRe matches both `load average:` (procps) and `load averages:`
// (BSD), with a comma decimal separator tolerated.
var uptimeLoadRe = regexp.MustCompile(`load averages?:\s*([0-9]+(?:[.,][0-9]+)?)`)

// readLoad1 ports runner/internal/hostload's source order (the runner is a
// separate module): /proc/loadavg, then `sysctl -n vm.loadavg`, then uptime.
func readLoad1(ctx context.Context, readFile func(string) ([]byte, error), execFn ExecFunc) (float64, error) {
	if b, err := readFile("/proc/loadavg"); err == nil {
		if v, perr := parseProcLoadavg(string(b)); perr == nil {
			return v, nil
		}
	}
	if out, err := execFn(ctx, "sysctl", "-n", "vm.loadavg"); err == nil {
		if v, perr := parseSysctlLoadavg(string(out)); perr == nil {
			return v, nil
		}
	}
	if out, err := execFn(ctx, "uptime"); err == nil {
		if v, perr := parseUptimeLoad(string(out)); perr == nil {
			return v, nil
		}
	}
	return 0, errors.New("no readable 1-minute load average (tried /proc/loadavg, sysctl vm.loadavg, uptime)")
}

func parseProcLoadavg(s string) (float64, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, errors.New("empty /proc/loadavg")
	}
	return strconv.ParseFloat(fields[0], 64)
}

// parseSysctlLoadavg parses Darwin's `{ 7.19 6.83 5.76 }`.
func parseSysctlLoadavg(s string) (float64, error) {
	fields := strings.Fields(strings.Trim(strings.TrimSpace(s), "{}"))
	if len(fields) == 0 {
		return 0, errors.New("empty vm.loadavg")
	}
	return strconv.ParseFloat(fields[0], 64)
}

func parseUptimeLoad(s string) (float64, error) {
	m := uptimeLoadRe.FindStringSubmatch(s)
	if len(m) != 2 {
		return 0, errors.New("no load average in uptime output")
	}
	return strconv.ParseFloat(strings.Replace(m[1], ",", ".", 1), 64)
}

// --- EIO indicator ------------------------------------------------------------

// eioRe is the storage-I/O-error indicator: a case-insensitive
// `input/output error` or a word-bounded `EIO`.
var eioRe = regexp.MustCompile(`(?i:input/output error)|\bEIO\b`)

func indicatesEIO(texts ...string) bool {
	for _, t := range texts {
		if eioRe.MatchString(t) {
			return true
		}
	}
	return false
}

// --- docker -----------------------------------------------------------------

// DefaultDockerTimeout bounds `docker info`.
const DefaultDockerTimeout = 20 * time.Second

// DockerProbe reports Docker VM reachability via `docker info`.
type DockerProbe struct {
	Exec    ExecFunc
	Timeout time.Duration
}

// Name implements Probe.
func (DockerProbe) Name() string { return "docker" }

// HostLocal implements Probe.
func (DockerProbe) HostLocal() bool { return true }

// Check implements Probe. Observed and Reason are fixed templates: raw docker
// error text (socket paths, hosts) never reaches a result.
func (p DockerProbe) Check(ctx context.Context) []Result {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultDockerTimeout
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := execOrDefault(p.Exec)(tctx, "docker", "info", "--format", "{{.ServerVersion}}")
	threshold := fmt.Sprintf("docker info answers within %s", timeout)
	if err == nil {
		return []Result{{Probe: p.Name(), Status: StatusOK, Observed: "docker info answered", Threshold: threshold}}
	}
	if errors.Is(err, exec.ErrNotFound) {
		return []Result{unknown(p.Name(), "docker CLI not on PATH")}
	}
	var stderr string
	exitCode := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		stderr = string(ee.Stderr)
		exitCode = ee.ExitCode()
	}
	if indicatesEIO(string(out), stderr, err.Error()) {
		return []Result{{
			Probe:     p.Name(),
			Condition: ConditionStorageEIO,
			Subject:   "docker",
			Status:    StatusFailing,
			Observed:  "docker info reported a storage I/O error (EIO)",
			Threshold: "no storage I/O errors",
		}}
	}
	observed := "docker info failed to run"
	switch {
	case errors.Is(tctx.Err(), context.DeadlineExceeded):
		observed = fmt.Sprintf("docker info did not answer within %s", timeout)
	case exitCode >= 0:
		observed = fmt.Sprintf("docker info failed (exit %d)", exitCode)
	}
	return []Result{{
		Probe:     p.Name(),
		Condition: ConditionDockerUnreachable,
		Status:    StatusFailing,
		Observed:  observed,
		Threshold: threshold,
		Detail:    map[string]any{"exit_code": exitCode},
	}}
}

// --- postgres ---------------------------------------------------------------

// PostgresProbe pings fishhawkd's own pool. It is NOT host-local: the pool is
// observable in every deployment.
type PostgresProbe struct {
	// Ping is nil when no database is configured.
	Ping func(ctx context.Context) error
}

// Name implements Probe.
func (PostgresProbe) Name() string { return "postgres" }

// HostLocal implements Probe.
func (PostgresProbe) HostLocal() bool { return false }

// Check implements Probe. Observed and Reason are fixed templates: raw pgx
// error text (DSN hosts, socket paths) never reaches a result.
func (p PostgresProbe) Check(ctx context.Context) []Result {
	if p.Ping == nil {
		return []Result{unknown(p.Name(), "no database configured")}
	}
	err := p.Ping(ctx)
	if err == nil {
		return []Result{{Probe: p.Name(), Status: StatusOK, Observed: "postgres ping succeeded", Threshold: "postgres ping succeeds"}}
	}
	if indicatesEIO(err.Error()) {
		return []Result{{
			Probe:     p.Name(),
			Condition: ConditionStorageEIO,
			Subject:   "postgres",
			Status:    StatusFailing,
			Observed:  "postgres ping reported a storage I/O error (EIO)",
			Threshold: "no storage I/O errors",
		}}
	}
	observed := "postgres ping failed"
	if errors.Is(err, context.DeadlineExceeded) {
		observed = "postgres ping timed out"
	}
	return []Result{{
		Probe:     p.Name(),
		Condition: ConditionPostgresUnreachable,
		Status:    StatusFailing,
		Observed:  observed,
		Threshold: "postgres ping succeeds",
	}}
}

// --- runners ----------------------------------------------------------------

// RunReader is the narrow tracker read the runner probe needs;
// run.Repository satisfies it.
type RunReader interface {
	GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error)
	GetStage(ctx context.Context, id uuid.UUID) (*run.Stage, error)
}

// DefaultConfirmSweeps is how many CONSECUTIVE Checks must see the same
// pid/run/stage terminal before it is reported: it absorbs the runner's own
// post-upload teardown (the trace upload makes the stage terminal before the
// process exits). Worst-case detection latency: ConfirmSweeps x interval.
const DefaultConfirmSweeps = 2

// RunnerProbe reports fishhawk-runner processes whose stage (or, with no
// --stage-id, run) is terminal in the tracker. It is stateful (the candidate
// set): use it by pointer and share one instance across sweeps.
type RunnerProbe struct {
	// List is a test seam; nil runs stalesweep.PSProbe with DefaultExec.
	List func(ctx context.Context) ([]stalesweep.RunnerProcess, error)
	// Runs reads the tracker; nil reads unknown (no database configured).
	Runs          RunReader
	ConfirmSweeps int

	mu         sync.Mutex
	candidates map[string]int
}

// Name implements Probe.
func (*RunnerProbe) Name() string { return "runners" }

// HostLocal implements Probe.
func (*RunnerProbe) HostLocal() bool { return true }

type orphan struct {
	key, pid, runID, stageID, state string
}

// Check implements Probe.
func (p *RunnerProbe) Check(ctx context.Context) []Result {
	if p.Runs == nil {
		return []Result{unknown(p.Name(), "no database configured")}
	}
	list := p.List
	if list == nil {
		list = stalesweep.PSProbe{Exec: DefaultExec}.Processes
	}
	procs, err := list(ctx)
	if err != nil {
		return []Result{unknown(p.Name(), "process scan failed: "+err.Error())}
	}
	confirm := p.ConfirmSweeps
	if confirm <= 0 {
		confirm = DefaultConfirmSweeps
	}

	var unattributed, unobservable, foreign, attributed int
	var terminal []orphan
	for _, proc := range procs {
		if proc.RunID == nil || proc.StageIDMalformed {
			unattributed++
			continue
		}
		state, isTerminal, lerr := p.lookup(ctx, proc)
		switch {
		case errors.Is(lerr, run.ErrNotFound):
			foreign++ // a runner pointed at another backend (e.g. the acceptance preview)
			continue
		case lerr != nil:
			unobservable++
			continue
		}
		attributed++
		if !isTerminal {
			continue
		}
		stageID := "-"
		if proc.StageID != nil {
			stageID = proc.StageID.String()
		}
		terminal = append(terminal, orphan{
			key: proc.PID + "/" + proc.RunID.String() + "/" + stageID,
			pid: proc.PID, runID: proc.RunID.String(), stageID: stageID, state: state,
		})
	}

	p.mu.Lock()
	next := make(map[string]int, len(terminal))
	for _, o := range terminal {
		next[o.key] = p.candidates[o.key] + 1
	}
	p.candidates = next
	p.mu.Unlock()

	var failing []Result
	pending := 0
	for _, o := range terminal {
		if next[o.key] < confirm {
			pending++
			continue
		}
		failing = append(failing, Result{
			Probe:     p.Name(),
			Condition: ConditionOrphanedRunner,
			Subject:   o.key,
			Status:    StatusFailing,
			Observed:  fmt.Sprintf("fishhawk-runner pid %s still running for run %s stage %s in state %s", o.pid, o.runID, o.stageID, o.state),
			Threshold: fmt.Sprintf("no fishhawk-runner outlives its terminal stage (confirmed over %d sweeps)", confirm),
			Detail:    map[string]any{"pid": o.pid, "run_id": o.runID, "stage_id": o.stageID, "state": o.state},
		})
	}
	if len(failing) > 0 {
		return failing
	}
	var reasons []string
	if unobservable > 0 {
		reasons = append(reasons, fmt.Sprintf("%d runner lookup(s) failed", unobservable))
	}
	if unattributed > 0 {
		reasons = append(reasons, fmt.Sprintf("%d fishhawk-runner process(es) carry no readable --run-id/--stage-id", unattributed))
	}
	if len(reasons) > 0 {
		return []Result{unknown(p.Name(), strings.Join(reasons, "; "))}
	}
	return []Result{{
		Probe:     p.Name(),
		Status:    StatusOK,
		Observed:  fmt.Sprintf("%d fishhawk-runner process(es) attributed to this tracker, none confirmed orphaned", attributed),
		Threshold: fmt.Sprintf("no fishhawk-runner outlives its terminal stage (confirmed over %d sweeps)", confirm),
		Detail:    map[string]any{"attributed": attributed, "foreign": foreign, "pending_confirmation": pending},
	}}
}

// lookup reads the process's stage (or run, with no stage id) state.
func (p *RunnerProbe) lookup(ctx context.Context, proc stalesweep.RunnerProcess) (state string, terminal bool, err error) {
	if proc.StageID != nil {
		st, err := p.Runs.GetStage(ctx, *proc.StageID)
		if err != nil {
			return "", false, err
		}
		if st == nil {
			return "", false, run.ErrNotFound
		}
		return string(st.State), st.State.IsTerminal(), nil
	}
	r, err := p.Runs.GetRun(ctx, *proc.RunID)
	if err != nil {
		return "", false, err
	}
	if r == nil {
		return "", false, run.ErrNotFound
	}
	return string(r.State), r.State.IsTerminal(), nil
}
