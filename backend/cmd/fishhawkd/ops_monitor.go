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
	"github.com/kuhlman-labs/fishhawk/backend/internal/opsmonitor"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stalesweep"
)

// Ops monitor wiring (E85.1 / #4142): the flags `serve --enable-ops-monitor`
// and the dry-run `ops-probe` subcommand share, their validation, and the
// probe/ticker construction. The probes, the fail-safe Sweep and the
// emit/dedup contract live in backend/internal/opsmonitor (README.md there).

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
			"host. Under kubernetes or remote the host-local probes (disk, gocache, load, docker, runners) report "+
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
	return f
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
	// NotifyIssue is 0 when no notify issue is configured (audit-only).
	NotifyRepo  forge.RepoRef
	NotifyIssue int
}

// resolveOpsMonitorConfig validates f. Disabled → (nil, nil) without
// validating anything, so a typo in a flag the operator never turned on
// cannot fail startup. Enabled → an unknown deployment, a malformed notify
// issue, a non-positive interval or quiet window, or a non-positive,
// non-finite or overflowing threshold is an error naming the flag.
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
	conf := &opsMonitorConfig{
		Deployment:       deployment,
		Interval:         f.Interval,
		QuietWindow:      f.QuietWindow,
		DiskPath:         f.DiskPath,
		DiskMinFreeBytes: diskMin,
		GoCacheDir:       f.GoCacheDir,
		GoCacheMaxBytes:  cacheMax,
		LoadFactor:       f.LoadFactor,
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
}

// opsMonitorDeps are the ops monitor's runtime dependencies. Every field is
// optional: a nil Ping or Runs reads unknown, a nil Audit is in-memory dedup
// with no row, a nil GitHub is audit-only.
type opsMonitorDeps struct {
	Audit  audit.Repository
	Runs   opsmonitor.RunReader
	Ping   func(ctx context.Context) error
	GitHub *githubclient.Client
	Logger *slog.Logger
	Seams  opsMonitorSeams
}

// newOpsProbes builds the six probes from conf + deps.
func newOpsProbes(conf opsMonitorConfig, deps opsMonitorDeps) []opsmonitor.Probe {
	ping := deps.Ping
	if deps.Seams.Ping != nil {
		ping = deps.Seams.Ping
	}
	return []opsmonitor.Probe{
		opsmonitor.DiskProbe{Path: conf.DiskPath, MinFreeBytes: conf.DiskMinFreeBytes, Statfs: deps.Seams.Statfs},
		opsmonitor.GoCacheProbe{Dir: conf.GoCacheDir, MaxBytes: conf.GoCacheMaxBytes, ResolveDir: deps.Seams.ResolveDir},
		opsmonitor.LoadProbe{Factor: conf.LoadFactor, ReadLoad: deps.Seams.ReadLoad, Cores: deps.Seams.Cores},
		opsmonitor.DockerProbe{Exec: deps.Seams.DockerExec},
		opsmonitor.PostgresProbe{Ping: ping},
		&opsmonitor.RunnerProbe{List: deps.Seams.ListRunners, Runs: deps.Runs},
	}
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

const opsProbeUsage = "Usage: fishhawkd ops-probe [--deployment auto|local|kubernetes|remote] [--db <url>] [--json] [--ops-monitor-* threshold flags]"

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
// notification. Without --db the postgres and runners probes read unknown.
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
		"postgres URL; without it the postgres and runners probes read unknown")
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
// `<status> <probe> condition=… subject=… observed=… threshold=… remedy=…|reason=…`,
// omitting empty fields and quoting values.
func formatOpsProbeLine(r opsmonitor.Result) string {
	parts := []string{string(r.Status), r.Probe}
	for _, kv := range []struct{ k, v string }{
		{"condition", r.Condition},
		{"subject", r.Subject},
		{"observed", r.Observed},
		{"threshold", r.Threshold},
		{"remedy", r.Remedy},
		{"reason", r.Reason},
	} {
		if kv.v != "" {
			parts = append(parts, kv.k+"="+strconv.Quote(kv.v))
		}
	}
	return strings.Join(parts, " ")
}
