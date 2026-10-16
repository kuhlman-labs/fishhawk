package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/hosthealth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/opsmonitor"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stalesweep"
)

// Ops monitor wiring (E85.1 / #4142): the flags `serve --enable-ops-monitor`
// and the dry-run `ops-probe` subcommand share, their validation, and the
// probe/ticker construction. The probes, the fail-safe Sweep and the
// emit/dedup contract live in backend/internal/opsmonitor (README.md there).
// E85.11 / #4330 adds the host-health thresholds, the memory and
// workload_containers probes, load attribution and the interim dispatch hold.

const (
	// defaultOpsMonitorDiskMinFreeGB is the data-volume free-space floor.
	defaultOpsMonitorDiskMinFreeGB = 20.0
	// defaultOpsMonitorGoCacheMaxGB matches scripts/dev's FISHHAWK_GOCACHE_MAX_GB.
	defaultOpsMonitorGoCacheMaxGB = 50.0
	// maxOpsMonitorGB bounds a GB flag so GB x 2^30 cannot overflow uint64.
	maxOpsMonitorGB = float64(1 << 33)
	// opsMonitorPingTimeout bounds the postgres probe's pool ping.
	opsMonitorPingTimeout = 5 * time.Second
	// opsProbeTimeout bounds the whole one-shot `ops-probe` sweep.
	opsProbeTimeout = 2 * time.Minute
	bytesPerGiB     = 1 << 30
)

// opsMonitorFlags carries the ops-monitor flags shared by `serve` and
// `ops-probe`. --enable-ops-monitor is serve-only and registered there.
type opsMonitorFlags struct {
	Interval      time.Duration
	Deployment    string
	QuietWindow   time.Duration
	DiskPath      string
	DiskMinFreeGB float64
	GoCacheDir    string
	GoCacheMaxGB  float64
	LoadFactor    float64
	NotifyIssue   string

	AmendmentWindow   time.Duration
	AmendmentWarnLead time.Duration
	AmendmentLookback time.Duration

	// Host-health thresholds (E85.11 / #4330), in PERCENT; 0 (the default)
	// keeps hosthealth.DefaultThresholds, resolved against the host's RAM
	// and cores at Check time.
	SwapEnterPct       float64
	SwapExitPct        float64
	MemAvailableMinPct float64
	CriticalSwapPct    float64
	HolderMinPct       float64
	ContainersMax      int
	ContainersMemPct   float64
	// DispatchHold is the serve-only --ops-monitor-dispatch-hold (the
	// interim admission hold); ops-probe never registers it.
	DispatchHold bool
}

// defaultOpsMonitorDiskPath is the operator's home directory: the volume the
// worktrees, Go build cache and Docker Desktop VM disk live on in the local
// deployment. Empty when unresolvable (os.UserHomeDir returns "" with its
// error), and the disk probe then reads unknown.
func defaultOpsMonitorDiskPath() string {
	home, _ := os.UserHomeDir()
	return home
}

// registerOpsMonitorFlags registers the shared ops-monitor flags, each with a
// FISHHAWKD_OPS_MONITOR_* env fallback. It runs no subprocess: the GOCACHE
// directory is resolved lazily at probe time, never here.
func registerOpsMonitorFlags(fs *flag.FlagSet) *opsMonitorFlags {
	f := &opsMonitorFlags{}
	fs.DurationVar(&f.Interval, "ops-monitor-interval",
		envOrDuration("FISHHAWKD_OPS_MONITOR_INTERVAL", opsmonitor.DefaultInterval),
		"ops monitor sweep interval (E85.1 / #4142); must be positive")
	fs.StringVar(&f.Deployment, "ops-monitor-deployment",
		envOr("FISHHAWKD_OPS_MONITOR_DEPLOYMENT", "auto"),
		"where fishhawkd runs relative to its runners: auto|local|kubernetes|remote. auto resolves kubernetes when "+
			"KUBERNETES_SERVICE_HOST is set, else local; pass remote on a non-k8s host that does not share the runners' "+
			"host. Under kubernetes or remote the host-local probes (disk, gocache, load, docker, runners, memory, "+
			"workload_containers) report "+
			"not_applicable without running")
	fs.DurationVar(&f.QuietWindow, "ops-monitor-quiet-window",
		envOrDuration("FISHHAWKD_OPS_MONITOR_QUIET_WINDOW", opsmonitor.DefaultQuietWindow),
		"suppress a repeat audit row and notification for the same failing condition+subject for this long; must be positive")
	fs.StringVar(&f.DiskPath, "ops-monitor-disk-path",
		envOr("FISHHAWKD_OPS_MONITOR_DISK_PATH", defaultOpsMonitorDiskPath()),
		"a path on the data volume whose free space the disk probe measures (default: the home directory); empty reads unknown")
	fs.Float64Var(&f.DiskMinFreeGB, "ops-monitor-disk-min-free-gb",
		envOrFloat("FISHHAWKD_OPS_MONITOR_DISK_MIN_FREE_GB", defaultOpsMonitorDiskMinFreeGB),
		"disk probe floor in GiB of free space; must be positive")
	fs.StringVar(&f.GoCacheDir, "ops-monitor-gocache-dir",
		envOr("FISHHAWKD_OPS_MONITOR_GOCACHE_DIR", ""),
		"Go build cache root the gocache probe samples. Empty (the default) resolves `go env GOCACHE` lazily at each "+
			"probe, never at startup; an empty, off or failing resolution reads unknown")
	fs.Float64Var(&f.GoCacheMaxGB, "ops-monitor-gocache-max-gb",
		envOrFloat("FISHHAWKD_OPS_MONITOR_GOCACHE_MAX_GB", defaultOpsMonitorGoCacheMaxGB),
		"gocache probe ceiling in GiB for the sampled cache estimate (matches FISHHAWK_GOCACHE_MAX_GB); must be positive")
	fs.Float64Var(&f.LoadFactor, "ops-monitor-load-factor",
		envOrFloat("FISHHAWKD_OPS_MONITOR_LOAD_FACTOR", opsmonitor.DefaultLoadFactor),
		"load probe fails when the 1-minute load exceeds this factor x the core count (matches the runner's hostload default); must be positive")
	fs.StringVar(&f.NotifyIssue, "ops-monitor-notify-issue",
		envOr("FISHHAWKD_OPS_MONITOR_NOTIFY_ISSUE", ""),
		"owner/name#N issue each failing condition is commented on through the GitHub App; empty is audit-only. "+
			"It MUST be an access-controlled issue: the comment carries pids, run ids and host metrics")
	fs.DurationVar(&f.AmendmentWindow, "ops-monitor-amendment-window",
		envOrDuration("FISHHAWKD_OPS_MONITOR_AMENDMENT_WINDOW", opsmonitor.DefaultAmendmentWindow),
		"scope-amendment decision window the amendment_window probe measures against (E85.2 / #4146); mirrors the "+
			"implement agent's ~15-minute decision wait-poll. Must be positive")
	fs.DurationVar(&f.AmendmentWarnLead, "ops-monitor-amendment-warn-lead",
		envOrDuration("FISHHAWKD_OPS_MONITOR_AMENDMENT_WARN_LEAD", opsmonitor.DefaultAmendmentWarnLead),
		"the amendment_window probe fires once a pending amendment's age exceeds the window minus this lead; 0 means "+
			"no lead (it fires only once the window has elapsed). Must be >= 0 and below the window")
	fs.DurationVar(&f.AmendmentLookback, "ops-monitor-amendment-lookback",
		envOrDuration("FISHHAWKD_OPS_MONITOR_AMENDMENT_LOOKBACK", opsmonitor.DefaultAmendmentLookback),
		"only pending scope amendments requested within this long are scanned, which bounds the backlog of stale "+
			"pre-#4127 pending rows; must exceed the window")
	fs.Float64Var(&f.SwapEnterPct, "ops-monitor-swap-enter-pct",
		envOrFloat("FISHHAWKD_OPS_MONITOR_SWAP_ENTER_PCT", 0),
		"memory probe (E85.11 / #4330): used swap, as a percent of swap total, at which host_memory_pressure trips; "+
			"0 keeps the default 90. Must lie in (0,100) and above --ops-monitor-swap-exit-pct")
	fs.Float64Var(&f.SwapExitPct, "ops-monitor-swap-exit-pct",
		envOrFloat("FISHHAWKD_OPS_MONITOR_SWAP_EXIT_PCT", 0),
		"memory probe: used swap percent below which a tripped swap signal clears (hysteresis); 0 keeps the default 70. "+
			"Must lie in (0,100) and below the enter percent")
	fs.Float64Var(&f.MemAvailableMinPct, "ops-monitor-mem-available-min-pct",
		envOrFloat("FISHHAWKD_OPS_MONITOR_MEM_AVAILABLE_MIN_PCT", 0),
		"memory probe: available memory, as a percent of RAM, at or below which host_memory_pressure trips; 0 keeps the "+
			"default 5. The band scales with it: it clears above 2x and is critical at 0.4x. Must lie in (0,50)")
	fs.Float64Var(&f.CriticalSwapPct, "ops-monitor-critical-swap-pct",
		envOrFloat("FISHHAWKD_OPS_MONITOR_CRITICAL_SWAP_PCT", 0),
		"memory probe: used swap percent at which memory pressure is CRITICAL (drives --ops-monitor-dispatch-hold); "+
			"0 keeps the default 97. Must lie in (0,100) and above the swap enter percent")
	fs.Float64Var(&f.HolderMinPct, "ops-monitor-holder-min-pct",
		envOrFloat("FISHHAWKD_OPS_MONITOR_HOLDER_MIN_PCT", 0),
		"memory probe: a process whose footprint is at least this percent of RAM is named as a contributor "+
			"(attribution only, never a trigger); 0 keeps the default max(1 GiB, 25%). Must lie in (0,100)")
	fs.IntVar(&f.ContainersMax, "ops-monitor-containers-max",
		envOrInt("FISHHAWKD_OPS_MONITOR_CONTAINERS_MAX", 0),
		"workload_containers probe: more running fishhawk-gate- containers than this trips workload_containers_high; "+
			"0 keeps the default max(4, cores/2). Must be >= 0")
	fs.Float64Var(&f.ContainersMemPct, "ops-monitor-containers-mem-pct",
		envOrFloat("FISHHAWKD_OPS_MONITOR_CONTAINERS_MEM_PCT", 0),
		"workload_containers probe: total gate-container memory, as a percent of RAM, at which "+
			"workload_containers_high trips; 0 keeps the default 50. It clears below 0.8x. Must lie in (0,100)")
	return f
}

// registerOpsMonitorDispatchHoldFlag registers the serve-only
// --ops-monitor-dispatch-hold onto f (ops-probe writes and holds nothing,
// so it never registers it).
func registerOpsMonitorDispatchHoldFlag(fs *flag.FlagSet, f *opsMonitorFlags) {
	fs.BoolVar(&f.DispatchHold, "ops-monitor-dispatch-hold",
		envOrBool("FISHHAWKD_OPS_MONITOR_DISPATCH_HOLD", false),
		"INTERIM dispatch hold (E85.11 / #4330; ADR-100's adaptive concurrency controller absorbs it): while the "+
			"memory probe reads CRITICAL pressure, refuse NEW host-dispatch admissions with 409 host_memory_pressure. "+
			"In-flight and already-dispatched stages are never touched. Needs --enable-ops-monitor and a local "+
			"deployment; ignored (with a WARN) otherwise. Off by default")
}

// opsMonitorConfig is the validated ops-monitor configuration.
type opsMonitorConfig struct {
	Deployment       opsmonitor.Deployment
	Interval         time.Duration
	QuietWindow      time.Duration
	DiskPath         string
	DiskMinFreeBytes uint64
	GoCacheDir       string
	GoCacheMaxBytes  uint64
	LoadFactor       float64
	// The amendment probes' durations (E85.2 / #4146), passed to the probes
	// verbatim: a zero warn lead means no lead.
	AmendmentWindow   time.Duration
	AmendmentWarnLead time.Duration
	AmendmentLookback time.Duration
	// NotifyIssue is 0 when no notify issue is configured (audit-only).
	NotifyRepo  forge.RepoRef
	NotifyIssue int
	// HostThresholds are the validated host-health overrides (E85.11).
	HostThresholds hostThresholdOverrides
	// DispatchHold requests the interim admission hold (opsAdmissionHold
	// decides whether it is built).
	DispatchHold bool
}

// hostThresholdOverrides are the operator's host-health threshold
// overrides as FRACTIONS; a zero field keeps the hosthealth default. It is
// applied at Check time against the host's RAM and cores (thresholds), so
// nothing reads the host at flag registration.
type hostThresholdOverrides struct {
	SwapEnter         float64
	SwapExit          float64
	AvailableEnter    float64
	CriticalSwap      float64
	HolderMinFraction float64
	ContainerMemEnter float64
	ContainerCountMax int
}

// thresholds is the probes' opsmonitor.ThresholdsFunc: the defaults for
// totalRAM/cores with every set override applied. A single-flag band
// (available memory, container memory) scales its exit (and, for available
// memory, its critical entry) by the same ratio as its enter, so the band
// keeps the default shape: available clears above 2x the floor and is
// critical at 0.4x; container memory clears below 0.8x.
func (o hostThresholdOverrides) thresholds(totalRAM uint64, cores int) hosthealth.Thresholds {
	th := hosthealth.DefaultThresholds(totalRAM, cores)
	if o.SwapEnter > 0 {
		th.SwapEnter = o.SwapEnter
	}
	if o.SwapExit > 0 {
		th.SwapExit = o.SwapExit
	}
	if o.CriticalSwap > 0 {
		th.CriticalSwap = o.CriticalSwap
	}
	if o.AvailableEnter > 0 {
		scale := o.AvailableEnter / th.AvailableEnter
		th.AvailableEnter = o.AvailableEnter
		th.AvailableExit *= scale
		th.CriticalAvailable *= scale
	}
	if o.HolderMinFraction > 0 {
		if b := uint64(o.HolderMinFraction * float64(totalRAM)); b > 0 {
			th.HolderMinBytes = b
		}
	}
	if o.ContainerMemEnter > 0 {
		scale := o.ContainerMemEnter / th.ContainerMemEnter
		th.ContainerMemEnter = o.ContainerMemEnter
		th.ContainerMemExit *= scale
	}
	if o.ContainerCountMax > 0 {
		th.ContainerCountMax = o.ContainerCountMax
	}
	return th
}

// validationRAM and validationCores are the representative host the
// startup check resolves the overrides against: every override is a
// fraction or a count, so the verdict does not depend on the real RAM.
const (
	validationRAM   = 16 << 30
	validationCores = 8
)

// resolveHostThresholds validates the host-health flags: each percent flag
// is 0 (default) or in (0,100) (available memory in (0,50), so its scaled
// exit stays a fraction), the container count is >= 0, and the composed
// bands pass hosthealth.Thresholds.Validate (high-water exit below enter,
// low-water exit above enter, critical beyond the failing entry).
func resolveHostThresholds(f *opsMonitorFlags) (hostThresholdOverrides, error) {
	var o hostThresholdOverrides
	for _, p := range []struct {
		name string
		pct  float64
		max  float64
		dst  *float64
	}{
		{"--ops-monitor-swap-enter-pct", f.SwapEnterPct, 100, &o.SwapEnter},
		{"--ops-monitor-swap-exit-pct", f.SwapExitPct, 100, &o.SwapExit},
		{"--ops-monitor-mem-available-min-pct", f.MemAvailableMinPct, 50, &o.AvailableEnter},
		{"--ops-monitor-critical-swap-pct", f.CriticalSwapPct, 100, &o.CriticalSwap},
		{"--ops-monitor-holder-min-pct", f.HolderMinPct, 100, &o.HolderMinFraction},
		{"--ops-monitor-containers-mem-pct", f.ContainersMemPct, 100, &o.ContainerMemEnter},
	} {
		if p.pct == 0 {
			continue
		}
		if math.IsNaN(p.pct) || p.pct <= 0 || p.pct >= p.max {
			return hostThresholdOverrides{}, fmt.Errorf("%s must be 0 (default) or a percent in (0,%.0f), got %v", p.name, p.max, p.pct)
		}
		*p.dst = p.pct / 100
	}
	if f.ContainersMax < 0 {
		return hostThresholdOverrides{}, fmt.Errorf("--ops-monitor-containers-max must be 0 (default) or >= 1, got %d", f.ContainersMax)
	}
	o.ContainerCountMax = f.ContainersMax
	if err := o.thresholds(validationRAM, validationCores).Validate(); err != nil {
		return hostThresholdOverrides{}, fmt.Errorf("--ops-monitor-*-pct host-health thresholds are inconsistent: %w", err)
	}
	return o, nil
}

// resolveOpsMonitorConfig validates f. Disabled → (nil, nil) without
// validating anything, so a typo in a flag the operator never turned on
// cannot fail startup. Enabled → an unknown deployment, a malformed notify
// issue, a non-positive interval or quiet window, a non-positive,
// non-finite or overflowing threshold, a degenerate amendment window,
// warn lead or lookback, or an out-of-range or inconsistent host-health
// threshold is an error naming the flag.
func resolveOpsMonitorConfig(enabled bool, f *opsMonitorFlags, getenv func(string) string) (*opsMonitorConfig, error) {
	if !enabled {
		return nil, nil
	}
	if f == nil {
		return nil, errors.New("ops monitor flags are not registered")
	}
	deployment, err := opsmonitor.ResolveDeployment(f.Deployment, getenv)
	if err != nil {
		return nil, fmt.Errorf("--ops-monitor-deployment: %w", err)
	}
	if f.Interval <= 0 {
		return nil, fmt.Errorf("--ops-monitor-interval must be positive, got %s", f.Interval)
	}
	if f.QuietWindow <= 0 {
		return nil, fmt.Errorf("--ops-monitor-quiet-window must be positive, got %s", f.QuietWindow)
	}
	diskMin, err := gibToBytes("--ops-monitor-disk-min-free-gb", f.DiskMinFreeGB)
	if err != nil {
		return nil, err
	}
	cacheMax, err := gibToBytes("--ops-monitor-gocache-max-gb", f.GoCacheMaxGB)
	if err != nil {
		return nil, err
	}
	if math.IsNaN(f.LoadFactor) || math.IsInf(f.LoadFactor, 0) || f.LoadFactor <= 0 {
		return nil, fmt.Errorf("--ops-monitor-load-factor must be a positive finite number, got %v", f.LoadFactor)
	}
	if f.AmendmentWindow <= 0 {
		return nil, fmt.Errorf("--ops-monitor-amendment-window must be positive, got %s", f.AmendmentWindow)
	}
	if f.AmendmentWarnLead < 0 || f.AmendmentWarnLead >= f.AmendmentWindow {
		return nil, fmt.Errorf("--ops-monitor-amendment-warn-lead must be >= 0 and below the amendment window (%s), got %s",
			f.AmendmentWindow, f.AmendmentWarnLead)
	}
	if f.AmendmentLookback <= f.AmendmentWindow {
		return nil, fmt.Errorf("--ops-monitor-amendment-lookback must exceed the amendment window (%s), got %s",
			f.AmendmentWindow, f.AmendmentLookback)
	}
	hostThresholds, err := resolveHostThresholds(f)
	if err != nil {
		return nil, err
	}
	conf := &opsMonitorConfig{
		Deployment:       deployment,
		Interval:         f.Interval,
		QuietWindow:      f.QuietWindow,
		DiskPath:         f.DiskPath,
		DiskMinFreeBytes: diskMin,
		GoCacheDir:       f.GoCacheDir,
		GoCacheMaxBytes:  cacheMax,
		LoadFactor:       f.LoadFactor,

		AmendmentWindow:   f.AmendmentWindow,
		AmendmentWarnLead: f.AmendmentWarnLead,
		AmendmentLookback: f.AmendmentLookback,

		HostThresholds: hostThresholds,
		DispatchHold:   f.DispatchHold,
	}
	if f.NotifyIssue != "" {
		repo, issue, err := opsmonitor.ParseIssueRef(f.NotifyIssue)
		if err != nil {
			return nil, fmt.Errorf("--ops-monitor-notify-issue: %w", err)
		}
		conf.NotifyRepo, conf.NotifyIssue = repo, issue
	}
	return conf, nil
}

// gibToBytes converts a positive, finite GiB flag value to bytes, refusing a
// value whose byte count would overflow uint64.
func gibToBytes(name string, gb float64) (uint64, error) {
	if math.IsNaN(gb) || math.IsInf(gb, 0) || gb <= 0 {
		return 0, fmt.Errorf("%s must be a positive finite number, got %v", name, gb)
	}
	if gb > maxOpsMonitorGB {
		return 0, fmt.Errorf("%s must be at most %.0f, got %v", name, maxOpsMonitorGB, gb)
	}
	return uint64(gb * bytesPerGiB), nil
}

// opsMonitorSeams are the probe test seams. The zero value selects every
// probe's production default.
type opsMonitorSeams struct {
	Statfs      func(path string) (avail, total uint64, err error)
	ResolveDir  func(ctx context.Context) (string, error)
	ReadLoad    func(ctx context.Context) (float64, error)
	Cores       func() int
	DockerExec  opsmonitor.ExecFunc
	ListRunners func(ctx context.Context) ([]stalesweep.RunnerProcess, error)
	// Ping, when set, replaces the deps' pool ping.
	Ping func(ctx context.Context) error
	// Host is the host-health reader the memory, workload_containers and
	// load-attribution reads go through (E85.11); the zero Reader reads the
	// running host.
	Host hosthealth.Reader
}

// opsMonitorDeps are the ops monitor's runtime dependencies. Every field is
// optional: a nil Ping, Runs or Amendments reads unknown, a nil Audit is
// in-memory dedup with no row, a nil GitHub is audit-only.
type opsMonitorDeps struct {
	Audit      audit.Repository
	Runs       opsmonitor.RunReader
	Amendments opsmonitor.AmendmentReader
	Ping       func(ctx context.Context) error
	GitHub     *githubclient.Client
	Logger     *slog.Logger
	Seams      opsMonitorSeams
	// Hold, when set, is the interim admission hold the ticker updates every
	// tick; serve shares the SAME pointer with server.Config.HostAdmissionHold.
	Hold *opsmonitor.AdmissionHold
}

// newOpsProbes builds the ten probes from conf + deps. The two host-health
// probes come last, so the E85.1/E85.2 probe order is unchanged.
func newOpsProbes(conf opsMonitorConfig, deps opsMonitorDeps) []opsmonitor.Probe {
	ping := deps.Ping
	if deps.Seams.Ping != nil {
		ping = deps.Seams.Ping
	}
	host := deps.Seams.Host
	thresholds := conf.HostThresholds.thresholds
	return []opsmonitor.Probe{
		opsmonitor.DiskProbe{Path: conf.DiskPath, MinFreeBytes: conf.DiskMinFreeBytes, Statfs: deps.Seams.Statfs},
		opsmonitor.GoCacheProbe{Dir: conf.GoCacheDir, MaxBytes: conf.GoCacheMaxBytes, ResolveDir: deps.Seams.ResolveDir},
		opsmonitor.LoadProbe{Factor: conf.LoadFactor, ReadLoad: deps.Seams.ReadLoad, Cores: deps.Seams.Cores, Attribution: host.Load},
		opsmonitor.DockerProbe{Exec: deps.Seams.DockerExec},
		opsmonitor.PostgresProbe{Ping: ping},
		&opsmonitor.RunnerProbe{List: deps.Seams.ListRunners, Runs: deps.Runs},
		opsmonitor.AmendmentWindowProbe{
			Amendments: deps.Amendments,
			Window:     conf.AmendmentWindow,
			WarnLead:   conf.AmendmentWarnLead,
			Lookback:   conf.AmendmentLookback,
		},
		opsmonitor.AmendmentTerminalProbe{Amendments: deps.Amendments, Lookback: conf.AmendmentLookback},
		&opsmonitor.MemoryProbe{Reader: host, Thresholds: thresholds, Cores: deps.Seams.Cores},
		&opsmonitor.WorkloadContainersProbe{Reader: host, Thresholds: thresholds, Cores: deps.Seams.Cores},
	}
}

// opsAdmissionHold builds the interim dispatch hold (E85.11 / #4330) when
// the monitor is enabled, --ops-monitor-dispatch-hold is set AND the
// resolved deployment is local: only there does the memory probe observe
// the host the runners spawn on. Otherwise it returns nil (a requested hold
// under a non-local deployment WARNs). serve builds it BEFORE newServer and
// hands the same pointer to the ticker. INTERIM: ADR-100's adaptive
// concurrency controller absorbs it.
func opsAdmissionHold(conf *opsMonitorConfig, logger *slog.Logger) *opsmonitor.AdmissionHold {
	if conf == nil || !conf.DispatchHold {
		return nil
	}
	if conf.Deployment != opsmonitor.DeploymentLocal {
		logger.Warn("--ops-monitor-dispatch-hold ignored: the memory probe observes the runners' host only under a local deployment",
			slog.String("deployment", string(conf.Deployment)))
		return nil
	}
	return &opsmonitor.AdmissionHold{}
}

// opsAmendmentReader adapts a pool to the amendment probes' reader. A nil
// pool returns a nil INTERFACE (never a typed nil), so the probes read
// unknown (no database configured) instead of panicking.
func opsAmendmentReader(pool *pgxpool.Pool) opsmonitor.AmendmentReader {
	if pool == nil {
		return nil
	}
	return scopeamendment.NewPendingLister(pool)
}

// newOpsMonitorTicker builds the serve-side ticker. A notify issue with no
// GitHub App client WARNs and runs audit-only rather than failing startup:
// the monitor is advisory.
func newOpsMonitorTicker(conf opsMonitorConfig, deps opsMonitorDeps) *opsmonitor.Ticker {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	t := &opsmonitor.Ticker{
		Probes:      newOpsProbes(conf, deps),
		Deployment:  conf.Deployment,
		Audit:       deps.Audit,
		Logger:      logger,
		Interval:    conf.Interval,
		QuietWindow: conf.QuietWindow,
		Hold:        deps.Hold,
	}
	if conf.NotifyIssue > 0 {
		if deps.GitHub == nil {
			logger.Warn("--ops-monitor-notify-issue set but no GitHub App client is configured; ops monitor runs audit-only",
				slog.String("repo", conf.NotifyRepo.Owner+"/"+conf.NotifyRepo.Name),
				slog.Int("issue", conf.NotifyIssue))
		} else {
			t.Notifier = opsmonitor.IssueCommentNotifier{
				Client:      deps.GitHub,
				Repo:        conf.NotifyRepo,
				Issue:       conf.NotifyIssue,
				QuietWindow: conf.QuietWindow,
			}
		}
	}
	return t
}

// opsMonitorRunner is the Run half of *opsmonitor.Ticker (a seam for the
// goroutine's recover).
type opsMonitorRunner interface {
	Run(ctx context.Context) error
}

// startOpsMonitor runs r on its own goroutine so it can never block startup,
// and recovers a panic into a WARN so it can never crash fishhawkd. Nothing
// here logs at ERROR: the monitor is advisory. The returned channel closes
// when the goroutine exits.
func startOpsMonitor(ctx context.Context, r opsMonitorRunner, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if recover() != nil {
				logger.Warn("ops monitor stopped: it panicked (the panic value is not logged)")
			}
		}()
		if err := r.Run(ctx); err != nil {
			logger.Warn("ops monitor exited with error", slog.String("error", err.Error()))
		}
	}()
	return done
}

// poolPing adapts a pool to the postgres probe's Ping under
// opsMonitorPingTimeout; a nil pool → nil (the probe reads unknown).
func poolPing(pool *pgxpool.Pool) func(ctx context.Context) error {
	if pool == nil {
		return nil
	}
	return func(ctx context.Context) error {
		pctx, cancel := context.WithTimeout(ctx, opsMonitorPingTimeout)
		defer cancel()
		return pool.Ping(pctx)
	}
}

const opsProbeUsage = "Usage: fishhawkd ops-probe [--deployment auto|local|kubernetes|remote] [--db <url>] [--json] [--ops-monitor-* threshold flags]\n" +
	"Probes: disk, gocache, load, docker, postgres, runners, amendment_window, amendment_terminal, memory, workload_containers"

// opsProbeOut is where `ops-probe` prints its results; opsProbeSeams are its
// probe seams. Package-level so tests can drive run([]string{"ops-probe", …}).
var (
	opsProbeOut   io.Writer = os.Stdout
	opsProbeSeams opsMonitorSeams
)

// runOpsProbe is the one-shot dry-run ops sweep (E85.1 / #4142):
//
//	fishhawkd ops-probe [--deployment auto] [--db <url>] [--json] [threshold flags]
//
// It runs ONE opsmonitor.Sweep with the same probes and thresholds `serve
// --enable-ops-monitor` uses, and writes NOTHING: no audit row, no
// notification. Without --db the postgres, runners and amendment probes read
// unknown.
// Exit 0 when nothing is failing, 1 when any result is failing or --db fails
// to connect, 2 on a usage error (including an invalid flag value).
func runOpsProbe(args []string, logSink io.Writer) int {
	return runOpsProbeWith(args, opsProbeOut, logSink, opsProbeSeams)
}

func runOpsProbeWith(args []string, out, logSink io.Writer, seams opsMonitorSeams) int {
	fs := flag.NewFlagSet("fishhawkd ops-probe", flag.ContinueOnError)
	fs.SetOutput(logSink)
	f := registerOpsMonitorFlags(fs)
	fs.StringVar(&f.Deployment, "deployment", f.Deployment, "alias of --ops-monitor-deployment")
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""),
		"postgres URL; without it the postgres, runners and amendment probes read unknown")
	asJSON := fs.Bool("json", false, "print the results as a JSON array instead of one line per result")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(logSink, "fishhawkd ops-probe: unexpected argument %q\n%s\n", fs.Arg(0), opsProbeUsage)
		return exitUsage
	}
	conf, err := resolveOpsMonitorConfig(true, f, os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd ops-probe: %v\n%s\n", err, opsProbeUsage)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), opsProbeTimeout)
	defer cancel()
	deps := opsMonitorDeps{Seams: seams}
	if *dbURL != "" {
		pool, err := postgres.Connect(ctx, *dbURL)
		if err != nil {
			_, _ = fmt.Fprintf(logSink, "fishhawkd ops-probe: connect: %v\n", err)
			return exitFailure
		}
		defer pool.Close()
		deps.Runs = runpkg.NewPostgresRepository(pool)
		deps.Amendments = opsAmendmentReader(pool)
		deps.Ping = poolPing(pool)
	}

	results := opsmonitor.Sweep(ctx, newOpsProbes(*conf, deps), conf.Deployment, opsmonitor.DefaultProbeTimeout)
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			_, _ = fmt.Fprintf(logSink, "fishhawkd ops-probe: encode: %v\n", err)
			return exitFailure
		}
	} else {
		for _, r := range results {
			_, _ = fmt.Fprintln(out, formatOpsProbeLine(r))
		}
	}
	for _, r := range results {
		if r.Status == opsmonitor.StatusFailing {
			return exitFailure
		}
	}
	return exitOK
}

// formatOpsProbeLine renders one result as
// `<status> <probe> condition=… subject=… observed=… threshold=… contributors=… remedy=…|reason=…`,
// omitting empty fields and quoting values. contributors (E85.11) is
// `name[pid] 41.0 GiB, …`; the names are already bounded and sanitized.
func formatOpsProbeLine(r opsmonitor.Result) string {
	parts := []string{string(r.Status), r.Probe}
	for _, kv := range []struct{ k, v string }{
		{"condition", r.Condition},
		{"subject", r.Subject},
		{"observed", r.Observed},
		{"threshold", r.Threshold},
		{"contributors", formatOpsContributors(r.Contributors)},
		{"remedy", r.Remedy},
		{"reason", r.Reason},
	} {
		if kv.v != "" {
			parts = append(parts, kv.k+"="+strconv.Quote(kv.v))
		}
	}
	return strings.Join(parts, " ")
}

// formatOpsContributors renders contributors for the ops-probe line; "" when
// there are none.
func formatOpsContributors(cs []opsmonitor.Contributor) string {
	descs := make([]string, 0, len(cs))
	for _, c := range cs {
		d := c.Name
		if c.PID > 0 {
			d += fmt.Sprintf("[%d]", c.PID)
		}
		descs = append(descs, fmt.Sprintf("%s %.1f GiB", d, float64(c.ResidentBytes)/bytesPerGiB))
	}
	return strings.Join(descs, ", ")
}
