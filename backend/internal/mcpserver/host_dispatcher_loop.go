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
	"time"
	"unicode"
	"unicode/utf8"

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
//   - the caps (max in-flight, per tick, and an opt-in per-UTC-day cap) and the
//     pause file, checked by the hostDispatchGate the core calls last before any
//     POST;
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
// documented choices. There is no daily-cap default: ADR-104 amendment A1
// removed it, and a DailyCap of 0 means no daily cap.
const (
	hostDispatchDefaultInterval    = 30 * time.Second
	hostDispatchMaxInterval        = 45 * time.Second
	hostDispatchDefaultMaxInFlight = 2
	hostDispatchDefaultPerTick     = 4
	// hostDispatchStatusStaleTicks: HostDispatcherStatus.State (--status and
	// the stdio status overlay) reports a dispatcher stale when its last tick,
	// or a known last completed pass, is older than this many intervals (a
	// chosen default, not an ADR value).
	hostDispatchStatusStaleTicks = 3
	// hostDispatchStatusFutureSkew: a last_tick_at or last_complete_tick_at
	// more than this far ahead of the reader's clock is future-dated and reads
	// stale, so a planted or clock-skewed file cannot read live forever.
	hostDispatchStatusFutureSkew = 60 * time.Second
	// The reader clamps interval_seconds to [min, max] before computing the
	// staleness window, so a huge value cannot keep a dead file live. The
	// dispatcher's own interval is capped at hostDispatchMaxInterval.
	hostDispatchStatusMinIntervalSeconds = 1
	hostDispatchStatusMaxIntervalSeconds = 300
	hostDispatchListPageLimit            = 100
	hostDispatchListMaxPages             = 10
)

// State-dir entries.
const (
	hostDispatchLedgerFile = "ledger.json"
	hostDispatchStatusFile = "status.json"
	hostDispatchPauseFile  = "pause"
	hostDispatchLockFile   = "lock"
)

// hostDispatchStateDirEnv names a dispatcher state dir outside the default
// (#5402): `fishhawk-mcp host-dispatch` reads it as its --state-dir default,
// and the stdio server's status overlay reads it as its exclusive rung. The
// cmd package declares the same literal, and both sides have a test that sets
// exactly this name, so a rename on either side reddens one.
const hostDispatchStateDirEnv = "FISHHAWK_HOST_DISPATCH_STATE_DIR"

// hostDispatchTextMax bounds every file-sourced string the dispatcher writes
// into status.json's run_refusals and the overlay copies into tool output.
const hostDispatchTextMax = 300

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
	// Zero values take the defaults (30s, 2, 4, drive_run's liveness
	// threshold) except DailyCap, where 0 means no daily cap (ADR-104 A1).
	// Interval above 45s is refused.
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

// hostDispatchRunRefusal is one run's latest non-benign refusal in
// status.json's run_refusals (#5402). FirstAt is when this code (for this
// stage) was first seen in an unbroken run of refusals; LastAt is the latest
// tick that refused it. A spawn or a benign outcome clears the entry, and a run
// that is no longer a candidate is pruned at the end of a completed pass.
type hostDispatchRunRefusal struct {
	RunID   string    `json:"run_id"`
	StageID string    `json:"stage_id,omitempty"`
	Code    string    `json:"code"`
	Detail  string    `json:"detail,omitempty"`
	FirstAt time.Time `json:"first_at"`
	LastAt  time.Time `json:"last_at"`
}

// hostDispatchTickFailure is the failing early return of the latest tick
// (state_dir_lock_lost, ledger_read_failed, ledger_write_failed or
// list_runs_failed). Every tick start resets it, so it names the CURRENT tick.
type hostDispatchTickFailure struct {
	Code string    `json:"code"`
	At   time.Time `json:"at"`
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
	// DailyCap is the opt-in daily spawn limit (--daily-cap); absent when none
	// is set (ADR-104 A1).
	DailyCap int `json:"daily_cap,omitempty"`
	InFlight int `json:"in_flight"`
	// LastCompleteTickAt is the last tick that finished its candidate pass
	// (#5402). Zero (absent) is UNKNOWN: a pre-#5402 dispatcher never writes
	// it, so State then judges on last_tick_at alone.
	LastCompleteTickAt time.Time `json:"last_complete_tick_at,omitzero"`
	// LastTickFailure is the latest tick's failing early return; absent when
	// the latest tick did not fail (a paused tick records none).
	LastTickFailure *hostDispatchTickFailure `json:"last_tick_failure,omitempty"`
	// RunRefusals holds each candidate run's latest non-benign refusal, keyed
	// by run id. cap_reached:per_tick is global-only (last_refusal_code) and
	// never an entry here.
	RunRefusals map[string]hostDispatchRunRefusal `json:"run_refusals,omitempty"`
}

// interval is the staleness unit: interval_seconds clamped to
// [hostDispatchStatusMinIntervalSeconds, hostDispatchStatusMaxIntervalSeconds],
// or hostDispatchDefaultInterval when it is absent or not positive. The clamp
// runs on the float, before the Duration conversion.
func (s HostDispatcherStatus) interval() time.Duration {
	secs := s.IntervalSeconds
	if !(secs > 0) {
		return hostDispatchDefaultInterval
	}
	secs = min(max(secs, hostDispatchStatusMinIntervalSeconds), hostDispatchStatusMaxIntervalSeconds)
	return time.Duration(secs * float64(time.Second))
}

// FutureDated reports whether last_tick_at or last_complete_tick_at is more
// than hostDispatchStatusFutureSkew ahead of now. No live writer stamps one,
// and such a file would otherwise never age out.
func (s HostDispatcherStatus) FutureDated(now time.Time) bool {
	limit := now.Add(hostDispatchStatusFutureSkew)
	return s.LastTickAt.After(limit) || s.LastCompleteTickAt.After(limit)
}

// Stale reports whether the status is older than hostDispatchStatusStaleTicks
// intervals at now (the process stopped ticking: liveness), or future-dated.
func (s HostDispatcherStatus) Stale(now time.Time) bool {
	return s.LastTickAt.IsZero() || s.FutureDated(now) || now.Sub(s.LastTickAt) > hostDispatchStatusStaleTicks*s.interval()
}

// TickFailed reports whether the latest tick failed: a recorded failure not
// older than the last completed pass. A failure with no completed pass at all
// is current, so a dispatcher whose every tick fails reads failing.
func (s HostDispatcherStatus) TickFailed() bool {
	return s.LastTickFailure != nil && !s.LastTickFailure.At.Before(s.LastCompleteTickAt)
}

// CompletedStale reports whether a KNOWN last completed pass is older than
// hostDispatchStatusStaleTicks intervals at now (health, as opposed to Stale's
// liveness). A zero LastCompleteTickAt is unknown, not stale (S3).
func (s HostDispatcherStatus) CompletedStale(now time.Time) bool {
	if s.LastCompleteTickAt.IsZero() {
		return false // unknown: a pre-#5402 writer; liveness alone decides (S3)
	}
	return now.Sub(s.LastCompleteTickAt) > hostDispatchStatusStaleTicks*s.interval()
}

// State is the dispatcher's health at now, shared by `--status` and the stdio
// status overlay: "stale" (not ticking, judged on last_tick_at, or a
// future-dated last_tick_at or last_complete_tick_at), "paused" (a
// deliberate pause skips the pass, so it is exempt from the completed-pass
// test), "failing" (the latest tick failed), "stale" (ticking, but a known
// last completed pass is too old), else "live". A status file written before
// #5402 carries none of the new keys and reads exactly as it did before.
func (s HostDispatcherStatus) State(now time.Time) string {
	if s.Stale(now) {
		return "stale"
	}
	if s.Paused {
		return "paused"
	}
	if s.TickFailed() {
		return "failing"
	}
	if s.CompletedStale(now) {
		return "stale"
	}
	return "live"
}

// boundHostDispatchText is the one bound for file-sourced text that reaches
// status.json or tool output: invalid UTF-8 and every control rune (C0, DEL,
// C1) are dropped, and the result is cut at the last rune boundary at or under
// hostDispatchTextMax bytes. The writer applies it to run_refusals detail and
// the overlay re-applies it on read, so both share one limit.
func boundHostDispatchText(s string) string {
	return boundHostDispatchTextTo(s, hostDispatchTextMax)
}

// boundHostDispatchTextTo is boundHostDispatchText cut at max bytes.
func boundHostDispatchTextTo(s string, max int) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "") {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) <= max {
		return out
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return out[:cut]
}

// BudgetRefusals returns the run_refusals entries refused budget_blocked
// (ADR-030 Amendment A1 / #5473), sorted by run id: the runs a blocking
// periodic budget is holding. A method, so the export surface is unchanged.
func (s HostDispatcherStatus) BudgetRefusals() []hostDispatchRunRefusal {
	var out []hostDispatchRunRefusal
	for _, ref := range s.RunRefusals {
		if ref.Code == hdRefuseBudgetBlocked {
			out = append(out, ref)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out
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
// before returning. It returns an error when the configuration, an invalid
// FISHHAWK_HOST_CAPACITY (#4241) or the state dir is refused at start, when
// another dispatcher holds the state-dir lock, or when the lock file is
// replaced underneath it. The capacity check runs before the state dir is
// touched, so a refused start creates and locks nothing.
func RunHostDispatcher(ctx context.Context, cfg HostDispatcherConfig) error {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return err
	}
	if err := ValidateHostCapacityEnv(os.LookupEnv); err != nil {
		return fmt.Errorf("host-dispatch refused to start: %w", err)
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
		api:          newAPIClient(config{backendURL: cfg.BackendURL, apiToken: cfg.APIToken, hostLabel: processHostLabel, hostCapacity: processHostCapacity}),
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

// hostDispatchStateDirProblem is the state-dir trust check, shared by the
// dispatcher (prepareHostDispatchStateDir) and the stdio status overlay: it
// returns hdStateDirSymlink, hdStateDirNotDir or hdStateDirNotOwned, in that
// order, for an Lstat result that is not a real directory owned by
// os.Getuid(), or "" when the entry is trusted. An owner that cannot be read
// fails closed.
func hostDispatchStateDirProblem(fi os.FileInfo, owner func(os.FileInfo) (int, bool)) string {
	if fi.Mode()&os.ModeSymlink != 0 {
		return hdStateDirSymlink
	}
	if !fi.IsDir() {
		return hdStateDirNotDir
	}
	if uid, ok := owner(fi); !ok || uid != os.Getuid() {
		return hdStateDirNotOwned
	}
	return ""
}

// prepareHostDispatchStateDir creates dir 0700 when absent and refuses a
// symlink, a non-directory, or an entry not owned by os.Getuid()
// (hostDispatchStateDirProblem). An existing owned dir is tightened to 0700.
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
	switch hostDispatchStateDirProblem(fi, owner) {
	case hdStateDirSymlink:
		return fmt.Errorf("%s: state dir %s is a symlink; point --state-dir at a real directory", hdStateDirSymlink, dir)
	case hdStateDirNotDir:
		return fmt.Errorf("%s: state dir %s is not a directory", hdStateDirNotDir, dir)
	case hdStateDirNotOwned:
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

// acquireHostDispatchLock lives in host_dispatcher_lock_unix.go (flock(2)); the
// Windows stub in host_dispatcher_lock_windows.go fails closed.

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

// finishHostDispatchLock runs once the flock is held on f (shared by the
// platform acquire files). It refuses, closing f, when path no longer names
// the locked inode, then pins mode 0600 and records the pid.
func finishHostDispatchLock(f *os.File, path string) (*hostDispatchLock, error) {
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
	// moved last until a fresh evaluation stops refusing. Rank-2 runs rotate
	// least-recently-evaluated first, so neither older held runs nor same-rank
	// peers can spend every tick's budget. Of N deprioritised runs, each is
	// re-evaluated within ceil(N / PerTickCap) ticks that COMPLETE the candidate
	// pass without a host_memory_pressure stop and while ranks 0 and 1 leave
	// budget.
	deprioritised map[string]bool
	// evalTick counts the ticks that reached the candidate pass; lastEvaluated
	// maps run id -> the evalTick of the last tick that evaluated it. It is
	// pruned to the deprioritised set at the end of each completed pass.
	evalTick      uint64
	lastEvaluated map[string]uint64
	// ownRuns holds the runs this process spawned a runner for.
	ownRuns map[string]bool
	// gateCalls counts Admit calls (the per-tick budget reads it).
	gateCalls int
	// outcomes holds the current tick's per-candidate outcomes.
	outcomes []hostDispatchOutcome
	// runRefusals is each candidate run's latest non-benign refusal, keyed by
	// run id; writeStatus copies it into status.json's run_refusals.
	runRefusals map[string]hostDispatchRunRefusal
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
		lastEvaluated: map[string]uint64{},
		ownRuns:       map[string]bool{},
		runRefusals:   map[string]hostDispatchRunRefusal{},
	}
	l.lockHeld = func() bool { return false }
	l.core = newHostDispatcher(r, binary, l, l)
	l.core.staleAfter = cfg.StaleAfter
	l.core.now = func() time.Time { return l.now() }
	l.status = HostDispatcherStatus{
		PID:             os.Getpid(),
		Host:            r.api.hostLabelResolution().Label,
		IntervalSeconds: cfg.Interval.Seconds(),
		DailyCap:        cfg.DailyCap,
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

// noteTickFailure records a failing early return of this tick: the status
// file's last_tick_failure and its global last refusal.
func (l *hostDispatchLoop) noteTickFailure(code string) {
	l.status.LastTickFailure = &hostDispatchTickFailure{Code: code, At: l.now().UTC()}
	l.noteRefusal(code, "", "")
}

// hostDispatchRefusalRepeated reports whether o repeats the run's recorded
// refusal (same code, same stage): a repeat keeps first_at and is not logged
// again.
func hostDispatchRefusalRepeated(prev hostDispatchRunRefusal, seen bool, o hostDispatchOutcome) bool {
	return seen && prev.Code == o.Refusal && prev.StageID == o.StageID
}

// pruneRunRefusals drops the entries of runs that are not in this tick's
// candidate list (terminal, no longer local, gone).
func (l *hostDispatchLoop) pruneRunRefusals(candidates []Run) {
	live := make(map[string]bool, len(candidates))
	for _, r := range candidates {
		live[r.ID] = true
	}
	for id := range l.runRefusals {
		if !live[id] {
			delete(l.runRefusals, id)
		}
	}
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
	l.status.RunRefusals = nil
	if len(l.runRefusals) > 0 {
		l.status.RunRefusals = make(map[string]hostDispatchRunRefusal, len(l.runRefusals))
		for id, ref := range l.runRefusals {
			l.status.RunRefusals[id] = ref
		}
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
// a lost state-dir lock is returned (the loop must stop). Every non-paused
// early return records last_tick_failure; only a tick that finishes its
// candidate pass stamps last_complete_tick_at (#5402).
func (l *hostDispatchLoop) tick(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.status.LastTickAt = l.now().UTC()
	l.status.LastTickFailure = nil
	defer l.writeStatus()

	if !l.lockHeld() {
		l.noteTickFailure(hdRefuseLockLost)
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
		l.noteTickFailure(hdRefuseLedgerRead)
		l.logf("ledger read failed; no spawns this tick: %v", err)
		return nil
	}
	l.ledger = ledger
	l.rollDay()

	l.reap(ctx)
	l.refreshInFlight(ctx)
	if err := l.persistLedger(); err != nil {
		l.noteTickFailure(hdRefuseLedgerWrite)
		l.logf("ledger write failed; no spawns this tick: %v", err)
		return nil
	}

	candidates, err := l.listCandidates(ctx)
	if err != nil {
		l.noteTickFailure(hdRefuseListFailed)
		l.logf("list local runs failed; no spawns this tick: %v", err)
		return nil
	}
	l.evalTick++
	l.order(candidates)

	// A campaign-state or held refusal deprioritises the run until a fresh
	// evaluation of it stops refusing; a run this tick does not reach keeps
	// its rank AND its older lastEvaluated, so it sorts ahead of the runs that
	// spent the budget next tick.
	refused := make(map[string]bool, len(l.deprioritised))
	for id := range l.deprioritised {
		refused[id] = true
	}
	attempts := 0
	l.outcomes = l.outcomes[:0]
	for i, run := range candidates {
		if attempts >= l.cfg.PerTickCap {
			// Global only: cap_reached:per_tick is never a run_refusals entry,
			// and the runs it leaves keep their entries for a later tick.
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
		l.lastEvaluated[run.ID] = l.evalTick
		if o.CampaignRefused || l.gateCalls != before {
			attempts++
		}
		l.record(o, refused)
		if o.StopMarkers {
			l.logf("host memory pressure: no more markers this tick")
			break
		}
	}
	l.pruneRunRefusals(candidates)
	// A per-tick-cap or memory-pressure break still counts as a completed pass.
	l.status.LastCompleteTickAt = l.now().UTC()
	// Like runRefusals, the deprioritised set keeps only runs still in this
	// tick's candidate list: a run that went terminal or left the list is never
	// evaluated again, so it would otherwise hold its entry until a restart.
	live := make(map[string]bool, len(candidates))
	for _, r := range candidates {
		live[r.ID] = true
	}
	for id := range refused {
		if !live[id] {
			delete(refused, id)
		}
	}
	l.deprioritised = refused
	// The rotation state is bounded by the deprioritised set, and so by the live
	// candidates: a run that left rank 2 is re-stamped by the evaluation that
	// puts it back.
	for id := range l.lastEvaluated {
		if !refused[id] {
			delete(l.lastEvaluated, id)
		}
	}
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
		delete(l.runRefusals, o.RunID)
		l.ownRuns[o.RunID] = true
		l.logf("spawned run=%s stage=%s (%s) respawn=%v log=%s", o.RunID, o.StageID, o.StageType, o.Respawn, o.LogPath)
		return
	}
	if o.Refusal == "" || hostDispatchBenignRefusals[o.Refusal] {
		delete(l.runRefusals, o.RunID)
		return
	}
	l.noteRefusal(o.Refusal, o.RunID, o.StageID)
	prev, seen := l.runRefusals[o.RunID]
	repeated := hostDispatchRefusalRepeated(prev, seen, o)
	now := l.now().UTC()
	first := now
	if repeated {
		first = prev.FirstAt
	}
	l.runRefusals[o.RunID] = hostDispatchRunRefusal{
		RunID:   o.RunID,
		StageID: o.StageID,
		Code:    o.Refusal,
		FirstAt: first,
		LastAt:  now,

		// The writer bound (S4): control characters dropped, 300 bytes.
		Detail: boundHostDispatchText(o.Detail),
	}
	if !repeated {
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
// never skipped without a fresh evaluation). The deprioritised rank is ordered
// least-recently-evaluated first, then created_at and id, so a run the per-tick
// cap left unreached sorts ahead of the runs that spent the budget.
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
		if ra == 2 {
			la, lb := l.lastEvaluated[a.ID], l.lastEvaluated[b.ID]
			if la != lb {
				return la < lb
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
	if l.cfg.DailyCap > 0 && l.ledger.SpawnsToday >= l.cfg.DailyCap {
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
