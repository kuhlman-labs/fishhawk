package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mcpserver"
	"github.com/kuhlman-labs/fishhawk/credstore"
)

// host_dispatch.go is `fishhawk-mcp host-dispatch` (ADR-104, E88.2 / #4526):
// the long-lived host dispatcher that spawns the parked stages of
// auto_dispatch-marked local runs. It is dispatch-only and never a gate act;
// the contract lives in backend/internal/mcpserver/README.md. The backend URL
// and token come from the same loadConfig ladder as the server modes; mint the
// credential with `fishhawkd token issue --subject host-dispatcher/<host>`.

const (
	hostDispatchSubcommand = "host-dispatch"
	exitUsage              = 2
	// hostDispatchMaxInterval mirrors mcpserver's bound: a queued stage must be
	// re-POSTed within the server's 60s queue TTL.
	hostDispatchMaxInterval = 45 * time.Second
	// hostDispatchStaleTicks mirrors HostDispatcherStatus.Stale's threshold.
	hostDispatchStaleTicks = 3
)

type hostDispatchFlags struct {
	allowedRoots string
	interval     time.Duration
	maxInFlight  int
	perTickCap   int
	dailyCap     int
	stateDir     string
	staleAfter   time.Duration
	runnerBinary string
	pause        bool
	resume       bool
	status       bool
}

// hostDispatchDeps are the seams the subcommand's tests replace.
type hostDispatchDeps struct {
	getenv     func(string) string
	loadConfig func() (config, error)
	run        func(context.Context, mcpserver.HostDispatcherConfig) error
	now        func() time.Time
}

func defaultHostDispatchDeps() hostDispatchDeps {
	return hostDispatchDeps{
		getenv: os.Getenv,
		loadConfig: func() (config, error) {
			return loadConfig(os.Getenv, credstore.Load, refreshStoredCredential)
		},
		run: mcpserver.RunHostDispatcher,
		now: time.Now,
	}
}

func defaultHostDispatchStateDir() string {
	return filepath.Join(os.TempDir(), "fishhawk-host-dispatch")
}

// runHostDispatch is the subcommand entry point; args are the arguments AFTER
// "host-dispatch". Exit codes: 0 ok (and --status live or paused), 1 a runtime
// failure (and --status absent or stale), 2 a usage error.
func runHostDispatch(ctx context.Context, args []string, stdout, stderr io.Writer, deps hostDispatchDeps) int {
	fs := flag.NewFlagSet("fishhawk-mcp host-dispatch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f hostDispatchFlags
	fs.StringVar(&f.allowedRoots, "allowed-roots", "", "OS-path-list of absolute checkout roots a spawned run's working_dir must resolve inside; falls back to FISHHAWK_MCP_ALLOWED_ROOTS (both empty refuses)")
	fs.DurationVar(&f.interval, "interval", 30*time.Second, "tick interval (at most 45s, so a queued stage is re-POSTed inside the server's 60s queue TTL)")
	fs.IntVar(&f.maxInFlight, "max-in-flight", 2, "maximum stages this dispatcher keeps dispatched or running (ADR-104 Q6)")
	fs.IntVar(&f.perTickCap, "per-tick-cap", 4, "maximum candidates per tick that reach the campaign-link read (bounds reads and marker POSTs)")
	fs.IntVar(&f.dailyCap, "daily-cap", 48, "maximum spawns per UTC day (persisted; rolls over at 00:00:00Z)")
	fs.StringVar(&f.stateDir, "state-dir", defaultHostDispatchStateDir(), "dispatcher state dir (ledger, status, pause and lock files; 0700, owned by you, never a symlink)")
	fs.DurationVar(&f.staleAfter, "stale-after", 0, "how long a dispatched stage this process is not running may sit before its runner's liveness is probed (0: the drive_run default)")
	fs.StringVar(&f.runnerBinary, "runner-binary", "", "fishhawk-runner path (default: FISHHAWK_RUNNER_BIN, beside this binary, then PATH)")
	fs.BoolVar(&f.pause, "pause", false, "create <state-dir>/pause (a running dispatcher stops POSTing markers) and exit")
	fs.BoolVar(&f.resume, "resume", false, "remove <state-dir>/pause and exit")
	fs.BoolVar(&f.status, "status", false, "print the status file; exit 0 when live or paused, 1 when absent or stale (older than 3 intervals)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "fishhawk-mcp host-dispatch: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	modes := 0
	for _, m := range []bool{f.pause, f.resume, f.status} {
		if m {
			modes++
		}
	}
	if modes > 1 {
		_, _ = fmt.Fprintln(stderr, "fishhawk-mcp host-dispatch: --pause, --resume and --status are mutually exclusive")
		return exitUsage
	}
	switch {
	case f.pause, f.resume:
		if err := mcpserver.SetHostDispatcherPaused(f.stateDir, f.pause); err != nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk-mcp host-dispatch: %v\n", err)
			return exitFailure
		}
		verb := "resumed"
		if f.pause {
			verb = "paused"
		}
		_, _ = fmt.Fprintf(stdout, "host-dispatch %s (state dir %s)\n", verb, f.stateDir)
		return exitOK
	case f.status:
		return hostDispatchStatus(f.stateDir, stdout, stderr, deps.now())
	}

	if f.interval <= 0 || f.interval > hostDispatchMaxInterval {
		_, _ = fmt.Fprintf(stderr, "fishhawk-mcp host-dispatch: --interval %s is outside (0, %s]: a queued stage must be re-POSTed within the server's 60s queue TTL\n", f.interval, hostDispatchMaxInterval)
		return exitUsage
	}
	if f.maxInFlight <= 0 || f.perTickCap <= 0 || f.dailyCap <= 0 || f.staleAfter < 0 {
		_, _ = fmt.Fprintln(stderr, "fishhawk-mcp host-dispatch: --max-in-flight, --per-tick-cap and --daily-cap must be positive, --stale-after not negative")
		return exitUsage
	}
	roots := splitAllowedRoots(f.allowedRoots)
	if len(roots) == 0 {
		roots = splitAllowedRoots(deps.getenv("FISHHAWK_MCP_ALLOWED_ROOTS"))
	}
	if len(roots) == 0 {
		_, _ = fmt.Fprintln(stderr, "fishhawk-mcp host-dispatch: no allowed roots: pass --allowed-roots or set FISHHAWK_MCP_ALLOWED_ROOTS")
		return exitUsage
	}
	cfg, err := deps.loadConfig()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk-mcp host-dispatch: %v\n", err)
		return exitFailure
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = deps.run(ctx, mcpserver.HostDispatcherConfig{
		BackendURL:   cfg.backendURL,
		APIToken:     cfg.apiToken,
		AllowedRoots: roots,
		StateDir:     f.stateDir,
		Interval:     f.interval,
		MaxInFlight:  f.maxInFlight,
		PerTickCap:   f.perTickCap,
		DailyCap:     f.dailyCap,
		StaleAfter:   f.staleAfter,
		RunnerBinary: f.runnerBinary,
		Log:          stderr,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk-mcp host-dispatch: %v\n", err)
		return exitFailure
	}
	return exitOK
}

func hostDispatchStatus(stateDir string, stdout, stderr io.Writer, now time.Time) int {
	st, err := mcpserver.ReadHostDispatcherStatus(stateDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintf(stderr, "fishhawk-mcp host-dispatch: no dispatcher status in %s (not running?)\n", stateDir)
		} else {
			_, _ = fmt.Fprintf(stderr, "fishhawk-mcp host-dispatch: %v\n", err)
		}
		return exitFailure
	}
	state := "live"
	switch {
	case st.Stale(now):
		state = "stale"
	case st.Paused:
		state = "paused"
	}
	_, _ = fmt.Fprintf(stdout, "state=%s pid=%d host=%s last_tick_at=%s paused=%v in_flight=%d spawns_today=%d (%s)",
		state, st.PID, st.Host, st.LastTickAt.Format(time.RFC3339), st.Paused, st.InFlight, st.SpawnsToday, st.SpawnsDay)
	if st.LastRefusalCode != "" {
		_, _ = fmt.Fprintf(stdout, " last_refusal=%s run=%s stage=%s at=%s", st.LastRefusalCode, st.LastRefusalRunID, st.LastRefusalStageID, st.LastRefusalAt.Format(time.RFC3339))
	}
	_, _ = fmt.Fprintln(stdout)
	if state == "stale" {
		return exitFailure
	}
	return exitOK
}

// isHostDispatchInvocation reports whether args (os.Args) select the
// host-dispatch subcommand. It is routed BEFORE the transport flag parse, so
// every existing invocation sees unchanged input.
func isHostDispatchInvocation(args []string) bool {
	return len(args) > 1 && strings.TrimSpace(args[1]) == hostDispatchSubcommand
}
