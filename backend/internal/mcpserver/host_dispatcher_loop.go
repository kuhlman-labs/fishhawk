package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// host_dispatcher_loop.go is the tick loop of the ADR-104 host dispatcher
// (E88.2 / #4526) and its exported entry points. The tick is the ONLY caller
// of the per-run core (host_dispatcher.go), so every marker POST and spawn
// passes the same campaign, cap and pause checks. The loop owns:
//
//   - the persisted 0600 ledger in a 0700, owner-checked, flock-locked state dir
//     (daily spawn count, in-flight set, the once-per-marker-epoch stale
//     re-spawn counter);
//   - the caps (max in-flight, per tick, per UTC day) and the pause file,
//     checked by the hostDispatchGate the core calls last before any POST;
//   - deterministic candidate order, cancel reaping and the status file.
//
// A 409 concurrency_slot_queued leaves a stage parked; the next tick re-POSTs
// it first. A re-POST within the server's QueueTTL keeps the queue row's
// enqueued_at (backend/internal/concurrency/README.md, the queue record
// ADR-094 as amended by ADR-100 and ADR-115), which is why the interval is
// capped at hostDispatchMaxInterval. A tick that cannot re-POST a queued stage
// lets its row go stale and forgoes its FIFO position: the stage could not be
// admitted at that moment anyway.

// Loop defaults. Max in-flight 2 is ADR-104 Q6; the rest are this delivery's
// documented choices.
const (
	hostDispatchDefaultInterval    = 30 * time.Second
	hostDispatchMaxInterval        = 45 * time.Second
	hostDispatchDefaultMaxInFlight = 2
	hostDispatchDefaultPerTick     = 4
	hostDispatchDefaultDaily       = 48
	// hostDispatchStatusStaleTicks: --status reports a dispatcher stale when its
	// last tick is older than this many intervals (a chosen default, not an
	// ADR value).
	hostDispatchStatusStaleTicks = 3
	hostDispatchListPageLimit    = 100
	hostDispatchListMaxPages     = 10
)

// State-dir entries.
const (
	hostDispatchLedgerFile = "ledger.json"
	hostDispatchStatusFile = "status.json"
	hostDispatchPauseFile  = "pause"
	hostDispatchLockFile   = "lock"
)

// Loop refusal codes (the core's are in host_dispatcher.go).
const (
	hdRefusePaused          = "paused"
	hdRefusePauseCheck      = "pause_check_failed"
	hdRefuseCapInFlight     = "cap_reached:max_in_flight"
	hdRefuseCapDaily        = "cap_reached:daily"
	hdRefuseCapPerTick      = "cap_reached:per_tick"
	hdRefuseLedgerWrite     = "ledger_write_failed"
	hdRefuseLedgerRead      = "ledger_read_failed"
	hdRefuseListFailed      = "list_runs_failed"
	hdRefuseLockLost        = "state_dir_lock_lost"
	hdStateDirSymlink       = "state_dir_symlink"
	hdStateDirNotOwned      = "state_dir_not_owned"
	hdStateDirNotDir        = "state_dir_not_directory"
	hdStateDirLocked        = "state_dir_locked"
	hdStateDirEntrySymlink  = "state_dir_entry_symlink"
	hdStateDirUnconfigured  = "state_dir_unconfigured"
	hdStateDirCreateFailure = "state_dir_create_failed"
)

// hostDispatchBenignRefusals are the per-candidate skips that are the normal
// shape of a tick (an unmarked run, a stage already running) and are not
// surfaced as the status file's last refusal.
var hostDispatchBenignRefusals = map[string]bool{
	hdRefuseNotLocal:            true,
	hdRefuseRunTerminal:         true,
	hdRefuseNotEligible:         true,
	hdRefuseNothingDispatchable: true,
	hdRefuseInFlight:            true,
}

// HostDispatcherConfig configures RunHostDispatcher (`fishhawk-mcp
// host-dispatch`). The bearer should be a host-dispatcher/<host> token minted
// with `fishhawkd token issue --subject host-dispatcher/<host>`; it is never
// forwarded to a spawned runner.
type HostDispatcherConfig struct {
	BackendURL string
	APIToken   string
	// AllowedRoots confines every spawned run's working_dir. Empty is refused.
	AllowedRoots []string
	// StateDir holds the ledger, status, pause and lock files (mode 0700,
	// owned by the current user, never a symlink).
	StateDir string
	// Zero values take the defaults (30s, 2, 4, 48, drive_run's liveness
	// threshold). Interval above 45s is refused.
	Interval    time.Duration
	MaxInFlight int
	PerTickCap  int
	DailyCap    int
	StaleAfter  time.Duration
	// RunnerBinary overrides the fishhawk-runner resolution (FISHHAWK_RUNNER_BIN,
	// then beside this binary, then PATH).
	RunnerBinary string
	// Log receives one line per spawn, refusal and tick error (nil: stderr).
	Log io.Writer
}

// HostDispatcherStatus is the status file a dispatcher rewrites every tick
// (<state-dir>/status.json, mode 0600).
type HostDispatcherStatus struct {
	PID                int       `json:"pid"`
	Host               string    `json:"host"`
	StartedAt          time.Time `json:"started_at"`
	LastTickAt         time.Time `json:"last_tick_at"`
	IntervalSeconds    float64   `json:"interval_seconds"`
	Paused             bool      `json:"paused"`
	LastRefusalCode    string    `json:"last_refusal_code,omitempty"`
	LastRefusalRunID   string    `json:"last_refusal_run_id,omitempty"`
	LastRefusalStageID string    `json:"last_refusal_stage_id,omitempty"`
	LastRefusalAt      time.Time `json:"last_refusal_at,omitzero"`
	SpawnsToday        int       `json:"spawns_today"`
	SpawnsDay          string    `json:"spawns_day"`
	InFlight           int       `json:"in_flight"`
}

// Stale reports whether the status is older than hostDispatchStatusStaleTicks
// intervals at now.
func (s HostDispatcherStatus) Stale(now time.Time) bool {
	interval := time.Duration(s.IntervalSeconds * float64(time.Second))
	if interval <= 0 {
		interval = hostDispatchDefaultInterval
	}
	return s.LastTickAt.IsZero() || now.Sub(s.LastTickAt) > hostDispatchStatusStaleTicks*interval
}

// ReadHostDispatcherStatus reads <stateDir>/status.json. A missing file
// answers an error wrapping os.ErrNotExist.
func ReadHostDispatcherStatus(stateDir string) (HostDispatcherStatus, error) {
	var st HostDispatcherStatus
	path := filepath.Join(stateDir, hostDispatchStatusFile)
	fi, err := os.Lstat(path)
	if err != nil {
		return st, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return st, fmt.Errorf("%s: %s is a symlink", hdStateDirEntrySymlink, path)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // path is the operator's own state dir
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("decode %s: %w", path, err)
	}
	return st, nil
}

// SetHostDispatcherPaused creates (paused) or removes (resumed) the
// <stateDir>/pause file. A running dispatcher checks it at the start of every
// tick and again immediately before every marker POST; ANY entry under that
// name, even a symlink, counts as paused.
func SetHostDispatcherPaused(stateDir string, paused bool) error {
	if err := prepareHostDispatchStateDir(stateDir, fileOwnerUID); err != nil {
		return err
	}
	path := filepath.Join(stateDir, hostDispatchPauseFile)
	if !paused {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // operator state dir
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	return f.Close()
}

// RunHostDispatcher runs the dispatcher until ctx is cancelled. It ticks once
// immediately and then every Interval; a cancel finishes the in-flight tick
// before returning. It returns an error when the configuration or state dir is
// refused at start, when another dispatcher holds the state-dir lock, or when
// the lock file is replaced underneath it.
func RunHostDispatcher(ctx context.Context, cfg HostDispatcherConfig) error {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return err
	}
	if err := prepareHostDispatchStateDir(cfg.StateDir, fileOwnerUID); err != nil {
		return err
	}
	lock, err := acquireHostDispatchLock(cfg.StateDir)
	if err != nil {
		return err
	}
	defer lock.unlock()

	r := &runResolver{
		api:          newAPIClient(config{backendURL: cfg.BackendURL, apiToken: cfg.APIToken, hostLabel: processHostLabel}),
		getenv:       os.Getenv,
		allowedRoots: cfg.AllowedRoots,
	}
	binary, err := r.resolveRunnerBinary(cfg.RunnerBinary)
	if err != nil {
		return err
	}
	l := newHostDispatchLoop(cfg, r, binary)
	l.lockHeld = lock.held
	return l.run(ctx)
}

func (cfg HostDispatcherConfig) withDefaults() (HostDispatcherConfig, error) {
	if len(cfg.AllowedRoots) == 0 {
		return cfg, errors.New("host-dispatch needs at least one allowed root (--allowed-roots or FISHHAWK_MCP_ALLOWED_ROOTS)")
	}
	if cfg.StateDir == "" {
		return cfg, fmt.Errorf("%s: no state dir configured", hdStateDirUnconfigured)
	}
	if cfg.Interval == 0 {
		cfg.Interval = hostDispatchDefaultInterval
	}
	if cfg.Interval < 0 || cfg.Interval > hostDispatchMaxInterval {
		return cfg, fmt.Errorf("interval %s is outside (0, %s]: a queued stage must be re-POSTed within the server's 60s queue TTL", cfg.Interval, hostDispatchMaxInterval)
	}
	if cfg.MaxInFlight == 0 {
		cfg.MaxInFlight = hostDispatchDefaultMaxInFlight
	}
	if cfg.PerTickCap == 0 {
		cfg.PerTickCap = hostDispatchDefaultPerTick
	}
	if cfg.DailyCap == 0 {
		cfg.DailyCap = hostDispatchDefaultDaily
	}
	if cfg.MaxInFlight < 0 || cfg.PerTickCap < 0 || cfg.DailyCap < 0 || cfg.StaleAfter < 0 {
		return cfg, errors.New("caps and --stale-after must not be negative")
	}
	if cfg.StaleAfter == 0 {
		cfg.StaleAfter = defaultHostDispatchStaleAfter
	}
	if cfg.Log == nil {
		cfg.Log = os.Stderr
	}
	return cfg, nil
}

// --- state dir -------------------------------------------------------------

// fileOwnerUID reads the Uid field of fi.Sys() by reflection, so the package
// still builds where FileInfo carries no uid; ok is false there and the state
// dir is refused (fail closed).
func fileOwnerUID(fi os.FileInfo) (int, bool) {
	v := reflect.ValueOf(fi.Sys())
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return 0, false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return 0, false
	}
	f := v.FieldByName("Uid")
	if !f.IsValid() {
		return 0, false
	}
	switch f.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int(f.Uint()), true //nolint:gosec // a uid fits an int
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return int(f.Int()), true
	default:
		return 0, false
	}
}

// prepareHostDispatchStateDir creates dir 0700 when absent and refuses a
// symlink, a non-directory, or an entry not owned by os.Getuid(). An existing
// owned dir is tightened to 0700.
func prepareHostDispatchStateDir(dir string, owner func(os.FileInfo) (int, bool)) error {
	if dir == "" {
		return fmt.Errorf("%s: no state dir configured", hdStateDirUnconfigured)
	}
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if merr := os.MkdirAll(dir, 0o700); merr != nil {
			return fmt.Errorf("%s: %w", hdStateDirCreateFailure, merr)
		}
		fi, err = os.Lstat(dir)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", hdStateDirCreateFailure, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s: state dir %s is a symlink; point --state-dir at a real directory", hdStateDirSymlink, dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s: state dir %s is not a directory", hdStateDirNotDir, dir)
	}
	uid, ok := owner(fi)
	if !ok || uid != os.Getuid() {
		return fmt.Errorf("%s: state dir %s is not owned by the current user (or its owner cannot be verified on this platform)", hdStateDirNotOwned, dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("%s: chmod 0700 %s: %w", hdStateDirCreateFailure, dir, err)
	}
	return nil
}

// writeHostDispatchFile writes data to dir/name with mode 0600 via a temp file
// and rename, so a reader never sees a torn file.
func writeHostDispatchFile(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, name))
}

// hostDispatchLock is the state dir's exclusive lock (F7e): an flock(2) on
// <dir>/lock, held for the dispatcher's whole lifetime, so every tick's ledger
// work runs under it. The kernel releases it when the holder's file closes,
// however the process ends, so a crashed dispatcher's lock is free at once and
// no contender ever reads, judges or removes another's lock (the pid-file
// takeover race). The file itself is never removed: removing it would let a
// contender lock a fresh inode while the holder still holds the old one.
type hostDispatchLock struct {
	f    *os.File
	path string
}

// acquireHostDispatchLock takes the lock without blocking. A held lock refuses
// state_dir_locked, naming the pid the holder wrote (diagnostics only: the
// flock, not the file content, is the lock).
func acquireHostDispatchLock(dir string) (*hostDispatchLock, error) {
	path := filepath.Join(dir, hostDispatchLockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // operator state dir
	if err != nil {
		return nil, fmt.Errorf("%s: open %s: %w", hdStateDirLocked, path, err)
	}
	if ferr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); ferr != nil { //nolint:gosec // an fd fits an int
		_ = f.Close()
		if errors.Is(ferr, syscall.EWOULDBLOCK) {
			holder := "unknown"
			if pid, rerr := readHostDispatchLock(path); rerr == nil {
				holder = strconv.Itoa(pid)
			}
			return nil, fmt.Errorf("%s: another dispatcher (pid %s) holds %s; stop it first", hdStateDirLocked, holder, path)
		}
		return nil, fmt.Errorf("%s: flock %s: %w", hdStateDirLocked, path, ferr)
	}
	l := &hostDispatchLock{f: f, path: path}
	if !l.held() {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %s was replaced while it was being locked; retry", hdStateDirLocked, path)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: chmod 0600 %s: %w", hdStateDirLocked, path, err)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
	}
	return l, nil
}

// held reports whether <dir>/lock still names the inode this process holds
// the flock on. Every tick checks it first, so a dispatcher whose lock file
// was removed or replaced (another dispatcher could then lock the new file)
// stops before any request.
func (l *hostDispatchLock) held() bool {
	fi, err := l.f.Stat()
	if err != nil {
		return false
	}
	pi, err := os.Lstat(l.path)
	return err == nil && os.SameFile(fi, pi)
}

// unlock releases the flock. It leaves the file in place.
func (l *hostDispatchLock) unlock() { _ = l.f.Close() }

func readHostDispatchLock(path string) (int, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator state dir
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(raw)))
}

// --- ledger ----------------------------------------------------------------

type hdLedgerEntry struct {
	RunID     string    `json:"run_id"`
	StageID   string    `json:"stage_id"`
	SpawnedAt time.Time `json:"spawned_at"`
}

// hdLedger is the persisted dispatcher state. Day is the UTC day the spawn
// count belongs to (it rolls over at 00:00:00Z). Respawns holds the stale
// re-spawn keys already used (stage id @ marker epoch).
type hdLedger struct {
	Day         string                   `json:"day"`
	SpawnsToday int                      `json:"spawns_today"`
	InFlight    map[string]hdLedgerEntry `json:"in_flight"`
	Respawns    map[string]bool          `json:"respawns"`
}

func respawnKey(stageID string, epoch time.Time) string {
	return stageID + "@" + epoch.UTC().Format(time.RFC3339Nano)
}

func loadHostDispatchLedger(dir string) (*hdLedger, error) {
	l := &hdLedger{InFlight: map[string]hdLedgerEntry{}, Respawns: map[string]bool{}}
	path := filepath.Join(dir, hostDispatchLedgerFile)
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s: %s is a symlink", hdStateDirEntrySymlink, path)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // operator state dir
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, l); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if l.InFlight == nil {
		l.InFlight = map[string]hdLedgerEntry{}
	}
	if l.Respawns == nil {
		l.Respawns = map[string]bool{}
	}
	return l, nil
}

// --- loop ------------------------------------------------------------------

type hostDispatchLoop struct {
	cfg  HostDispatcherConfig
	core *hostDispatcher
	api  *apiClient
	now  func() time.Time
	// terminate reaps this process's detached runners for a run (default
	// detachedRunners.terminateRunners).
	terminate func(runID string) int
	// lockHeld re-checks the state-dir lock each tick (hostDispatchLock.held;
	// false until RunHostDispatcher wires the lock it took: fail closed).
	lockHeld func() bool
	// writeFile is the ledger/status write seam (fault injection).
	writeFile func(dir, name string, data []byte) error

	mu     sync.Mutex
	ledger *hdLedger
	status HostDispatcherStatus

	// queued maps run id -> the start of its live queue episode.
	queued map[string]time.Time
	// deprioritised holds the runs whose latest evaluation was refused for
	// campaign state (CampaignRefused) or after the gate admitted it (Held):
	// moved last until a fresh evaluation stops refusing, so persistently held
	// older runs cannot spend every tick's budget.
	deprioritised map[string]bool
	// ownRuns holds the runs this process spawned a runner for.
	ownRuns map[string]bool
	// gateCalls counts Admit calls (the per-tick budget reads it).
	gateCalls int
	// outcomes holds the current tick's per-candidate outcomes.
	outcomes []hostDispatchOutcome
}

func newHostDispatchLoop(cfg HostDispatcherConfig, r *runResolver, binary string) *hostDispatchLoop {
	l := &hostDispatchLoop{
		cfg:           cfg,
		api:           r.api,
		now:           time.Now,
		terminate:     func(runID string) int { n, _ := detachedRunners.terminateRunners(runID, nil); return n },
		writeFile:     writeHostDispatchFile,
		queued:        map[string]time.Time{},
		deprioritised: map[string]bool{},
		ownRuns:       map[string]bool{},
	}
	l.lockHeld = func() bool { return false }
	l.core = newHostDispatcher(r, binary, l, l)
	l.core.staleAfter = cfg.StaleAfter
	l.core.now = func() time.Time { return l.now() }
	l.status = HostDispatcherStatus{
		PID:             os.Getpid(),
		Host:            r.api.hostLabelResolution().Label,
		IntervalSeconds: cfg.Interval.Seconds(),
	}
	return l
}

func (l *hostDispatchLoop) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(l.cfg.Log, "fishhawk-mcp host-dispatch: "+format+"\n", args...)
}

func (l *hostDispatchLoop) run(ctx context.Context) error {
	l.status.StartedAt = l.now().UTC()
	ticker := time.NewTicker(l.cfg.Interval)
	defer ticker.Stop()
	for {
		// The tick runs on a context detached from the cancel, so SIGINT /
		// SIGTERM finishes the in-flight tick before the loop exits.
		if err := l.tick(context.WithoutCancel(ctx)); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (l *hostDispatchLoop) paused() (bool, error) {
	_, err := os.Lstat(filepath.Join(l.cfg.StateDir, hostDispatchPauseFile))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return true, err
}

func (l *hostDispatchLoop) noteRefusal(code, runID, stageID string) {
	l.status.LastRefusalCode = code
	l.status.LastRefusalRunID = runID
	l.status.LastRefusalStageID = stageID
	l.status.LastRefusalAt = l.now().UTC()
}

func (l *hostDispatchLoop) persistLedger() error {
	raw, err := json.Marshal(l.ledger)
	if err != nil {
		return err
	}
	return l.writeFile(l.cfg.StateDir, hostDispatchLedgerFile, raw)
}

func (l *hostDispatchLoop) writeStatus() {
	if l.ledger != nil {
		l.status.SpawnsToday = l.ledger.SpawnsToday
		l.status.SpawnsDay = l.ledger.Day
		l.status.InFlight = len(l.ledger.InFlight)
	}
	raw, err := json.Marshal(l.status)
	if err == nil {
		err = l.writeFile(l.cfg.StateDir, hostDispatchStatusFile, raw)
	}
	if err != nil {
		l.logf("status write failed: %v", err)
	}
}

func (l *hostDispatchLoop) rollDay() {
	day := l.now().UTC().Format(time.DateOnly)
	if l.ledger.Day != day {
		l.ledger.Day = day
		l.ledger.SpawnsToday = 0
	}
}

// tick is one dispatcher round. A tick error is logged and never spawns; only
// a lost state-dir lock is returned (the loop must stop).
func (l *hostDispatchLoop) tick(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.status.LastTickAt = l.now().UTC()
	defer l.writeStatus()

	if !l.lockHeld() {
		l.noteRefusal(hdRefuseLockLost, "", "")
		return fmt.Errorf("%s: another dispatcher took %s", hdRefuseLockLost, filepath.Join(l.cfg.StateDir, hostDispatchLockFile))
	}
	if p, err := l.paused(); p {
		l.status.Paused = true
		if err != nil {
			l.noteRefusal(hdRefusePauseCheck, "", "")
			l.logf("pause check failed (treated as paused): %v", err)
		}
		return nil
	}
	l.status.Paused = false

	ledger, err := loadHostDispatchLedger(l.cfg.StateDir)
	if err != nil {
		l.noteRefusal(hdRefuseLedgerRead, "", "")
		l.logf("ledger read failed; no spawns this tick: %v", err)
		return nil
	}
	l.ledger = ledger
	l.rollDay()

	l.reap(ctx)
	l.refreshInFlight(ctx)
	if err := l.persistLedger(); err != nil {
		l.noteRefusal(hdRefuseLedgerWrite, "", "")
		l.logf("ledger write failed; no spawns this tick: %v", err)
		return nil
	}

	candidates, err := l.listCandidates(ctx)
	if err != nil {
		l.noteRefusal(hdRefuseListFailed, "", "")
		l.logf("list local runs failed; no spawns this tick: %v", err)
		return nil
	}
	l.order(candidates)

	// A campaign-state or held refusal deprioritises the run until a fresh
	// evaluation of it stops refusing; a run this tick does not reach keeps
	// its place.
	refused := make(map[string]bool, len(l.deprioritised))
	for id := range l.deprioritised {
		refused[id] = true
	}
	attempts := 0
	l.outcomes = l.outcomes[:0]
	for i, run := range candidates {
		if attempts >= l.cfg.PerTickCap {
			l.noteRefusal(hdRefuseCapPerTick, run.ID, "")
			l.logf("%s: %d candidate(s) left for a later tick", hdRefuseCapPerTick, len(candidates)-i)
			break
		}
		id, perr := uuid.Parse(run.ID)
		if perr != nil {
			continue
		}
		before := l.gateCalls
		o := l.core.evaluate(ctx, id)
		if o.CampaignRefused || l.gateCalls != before {
			attempts++
		}
		l.record(o, refused)
		if o.StopMarkers {
			l.logf("host memory pressure: no more markers this tick")
			break
		}
	}
	l.deprioritised = refused
	return nil
}

func (l *hostDispatchLoop) record(o hostDispatchOutcome, refused map[string]bool) {
	l.outcomes = append(l.outcomes, o)
	if o.QueueEpisode {
		if _, ok := l.queued[o.RunID]; !ok {
			l.queued[o.RunID] = l.now()
		}
	} else {
		delete(l.queued, o.RunID)
	}
	if o.CampaignRefused || o.Held {
		refused[o.RunID] = true
	} else {
		delete(refused, o.RunID)
	}
	if o.Spawned {
		l.ownRuns[o.RunID] = true
		l.logf("spawned run=%s stage=%s (%s) respawn=%v log=%s", o.RunID, o.StageID, o.StageType, o.Respawn, o.LogPath)
		return
	}
	if o.Refusal != "" && !hostDispatchBenignRefusals[o.Refusal] {
		l.noteRefusal(o.Refusal, o.RunID, o.StageID)
		l.logf("refused run=%s stage=%s: %s %s", o.RunID, o.StageID, o.Refusal, o.Detail)
	}
}

// reap terminates this process's own runners for a run that is now cancelled,
// or whose linked campaign is cancelled. A read error keeps the run (retried
// next tick).
func (l *hostDispatchLoop) reap(ctx context.Context) {
	ids := make([]string, 0, len(l.ownRuns))
	for id := range l.ownRuns {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		var run Run
		if err := l.api.do(ctx, http.MethodGet, "/v0/runs/"+id+"?include=campaign_link", nil, &run); err != nil {
			l.logf("reap read run=%s failed: %v", id, err)
			continue
		}
		campaignCancelled := false
		if run.CampaignLink != nil && run.CampaignLink.Status == campaignLinkStatusLinked {
			for _, c := range run.CampaignLink.Campaigns {
				if c.State == "cancelled" {
					campaignCancelled = true
				}
			}
		}
		switch {
		case run.State == "cancelled" || campaignCancelled:
			n := l.terminate(id)
			l.logf("reaped %d runner(s) for run=%s (run %s, campaign cancelled=%v)", n, id, run.State, campaignCancelled)
			delete(l.ownRuns, id)
			for sid, e := range l.ledger.InFlight {
				if e.RunID == id {
					delete(l.ledger.InFlight, sid)
				}
			}
		case runStateIsTerminal(run.State):
			delete(l.ownRuns, id)
		}
	}
}

// refreshInFlight keeps the ledger entries whose stage the server still
// reports dispatched or running (one stage list per run). A read error keeps
// the entry, so it still counts toward max in-flight (fail closed).
func (l *hostDispatchLoop) refreshInFlight(ctx context.Context) {
	byRun := map[string][]string{}
	for sid, e := range l.ledger.InFlight {
		byRun[e.RunID] = append(byRun[e.RunID], sid)
	}
	for runID, sids := range byRun {
		id, err := uuid.Parse(runID)
		if err != nil {
			continue
		}
		stages, err := l.api.ListRunStages(ctx, id)
		if err != nil {
			l.logf("in-flight read run=%s failed (kept): %v", runID, err)
			continue
		}
		live := map[string]bool{}
		for _, st := range stages {
			if st.State == "dispatched" || st.State == "running" {
				live[st.ID] = true
			}
		}
		for _, sid := range sids {
			if !live[sid] {
				delete(l.ledger.InFlight, sid)
			}
		}
	}
}

func (l *hostDispatchLoop) listCandidates(ctx context.Context) ([]Run, error) {
	seen := map[string]bool{}
	var out []Run
	for _, state := range []string{"pending", "running"} {
		cursor := ""
		for page := 0; page < hostDispatchListMaxPages; page++ {
			res, err := l.api.ListRuns(ctx, listRunsFilter{RunnerKind: driveRunnerKindLocal, State: state, Limit: hostDispatchListPageLimit, Cursor: cursor})
			if err != nil {
				return nil, err
			}
			for _, r := range res.Items {
				if !seen[r.ID] {
					seen[r.ID] = true
					out = append(out, r)
				}
			}
			if res.NextCursor == "" {
				break
			}
			cursor = res.NextCursor
		}
	}
	return out, nil
}

// order sorts candidates: live queue episodes first (oldest episode first),
// then created_at and id, with runs whose latest evaluation was refused for
// campaign state or after the gate admitted it moved last (deprioritised,
// never skipped without a fresh evaluation).
func (l *hostDispatchLoop) order(runs []Run) {
	rank := func(r Run) int {
		if _, ok := l.queued[r.ID]; ok {
			return 0
		}
		if l.deprioritised[r.ID] {
			return 2
		}
		return 1
	}
	sort.SliceStable(runs, func(i, j int) bool {
		a, b := runs[i], runs[j]
		ra, rb := rank(a), rank(b)
		if ra != rb {
			return ra < rb
		}
		if ra == 0 {
			ea, eb := l.queued[a.ID], l.queued[b.ID]
			if !ea.Equal(eb) {
				return ea.Before(eb)
			}
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
}

// Admit is the hostDispatchGate: the pause file, then the caps, then the
// ledger reservation (spawn count + in-flight entry), persisted BEFORE the
// core records the act or POSTs the marker. A ledger write failure refuses.
func (l *hostDispatchLoop) Admit(_ context.Context, c hostDispatchCandidate) (func(), string, error) {
	l.gateCalls++
	if p, err := l.paused(); p {
		if err != nil {
			return nil, hdRefusePauseCheck, nil
		}
		return nil, hdRefusePaused, nil
	}
	if l.ledger == nil {
		return nil, hdRefuseLedgerRead, nil
	}
	l.rollDay()
	_, already := l.ledger.InFlight[c.StageID]
	inFlight := len(l.ledger.InFlight)
	if already {
		inFlight--
	}
	if inFlight >= l.cfg.MaxInFlight {
		return nil, hdRefuseCapInFlight, nil
	}
	if l.ledger.SpawnsToday >= l.cfg.DailyCap {
		return nil, hdRefuseCapDaily, nil
	}
	prevEntry := l.ledger.InFlight[c.StageID]
	l.ledger.SpawnsToday++
	l.ledger.InFlight[c.StageID] = hdLedgerEntry{RunID: c.RunID, StageID: c.StageID, SpawnedAt: l.now().UTC()}
	undo := func() {
		l.ledger.SpawnsToday--
		if already {
			l.ledger.InFlight[c.StageID] = prevEntry
		} else {
			delete(l.ledger.InFlight, c.StageID)
		}
	}
	if err := l.persistLedger(); err != nil {
		undo()
		l.logf("ledger write failed for run=%s stage=%s: %v", c.RunID, c.StageID, err)
		return nil, hdRefuseLedgerWrite, nil
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		undo()
		if err := l.persistLedger(); err != nil {
			l.logf("ledger write failed releasing run=%s stage=%s: %v", c.RunID, c.StageID, err)
		}
	}, "", nil
}

// Allowed and Reserve are the persisted hostDispatchRespawnCounter: a used
// (stage, epoch) key survives a restart, so a restart never grants a second
// stale re-spawn for the same marker epoch (ADR-104 Q2).
func (l *hostDispatchLoop) Allowed(stageID string, epoch time.Time) bool {
	return l.ledger != nil && !l.ledger.Respawns[respawnKey(stageID, epoch)]
}

// Release returns a reservation whose attempt ended before a spawn. A persist
// failure keeps the key (the epoch stays consumed: fail safe).
func (l *hostDispatchLoop) Release(stageID string, epoch time.Time) error {
	if l.ledger == nil {
		return errors.New("no ledger loaded")
	}
	k := respawnKey(stageID, epoch)
	if !l.ledger.Respawns[k] {
		return nil
	}
	delete(l.ledger.Respawns, k)
	if err := l.persistLedger(); err != nil {
		l.ledger.Respawns[k] = true
		return err
	}
	return nil
}

func (l *hostDispatchLoop) Reserve(stageID string, epoch time.Time) (bool, error) {
	if l.ledger == nil {
		return false, errors.New("no ledger loaded")
	}
	k := respawnKey(stageID, epoch)
	if l.ledger.Respawns[k] {
		return false, nil
	}
	l.ledger.Respawns[k] = true
	if err := l.persistLedger(); err != nil {
		delete(l.ledger.Respawns, k)
		return false, err
	}
	return true, nil
}
