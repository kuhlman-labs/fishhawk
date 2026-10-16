package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/opsmonitor"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stalesweep"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// None of these tests is parallel: they swap package-level seams
// (serveContext, newServer, serveOpsMonitorSeams, opsProbeOut, opsProbeSeams)
// and set env vars.

// opsMonitorEnvVars are every env fallback registerOpsMonitorFlags and the
// serve enable flag read; clearOpsMonitorEnv blanks them so a developer's
// shell cannot leak into a test.
var opsMonitorEnvVars = []string{
	"FISHHAWKD_ENABLE_OPS_MONITOR",
	"FISHHAWKD_OPS_MONITOR_INTERVAL",
	"FISHHAWKD_OPS_MONITOR_DEPLOYMENT",
	"FISHHAWKD_OPS_MONITOR_QUIET_WINDOW",
	"FISHHAWKD_OPS_MONITOR_DISK_PATH",
	"FISHHAWKD_OPS_MONITOR_DISK_MIN_FREE_GB",
	"FISHHAWKD_OPS_MONITOR_GOCACHE_DIR",
	"FISHHAWKD_OPS_MONITOR_GOCACHE_MAX_GB",
	"FISHHAWKD_OPS_MONITOR_LOAD_FACTOR",
	"FISHHAWKD_OPS_MONITOR_NOTIFY_ISSUE",
	"FISHHAWKD_OPS_MONITOR_AMENDMENT_WINDOW",
	"FISHHAWKD_OPS_MONITOR_AMENDMENT_WARN_LEAD",
	"FISHHAWKD_OPS_MONITOR_AMENDMENT_LOOKBACK",
	"FISHHAWKD_DATABASE_URL",
}

func clearOpsMonitorEnv(t *testing.T) {
	t.Helper()
	for _, k := range opsMonitorEnvVars {
		t.Setenv(k, "")
	}
}

// parseOpsMonitorFlags registers the shared flags on a fresh set and parses args.
func parseOpsMonitorFlags(t *testing.T, args ...string) *opsMonitorFlags {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := registerOpsMonitorFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return f
}

func noEnv(string) string { return "" }

// TestResolveOpsMonitorConfig is the config resolution table: disabled skips
// validation; each invalid mode errors naming its flag; a valid config
// resolves the defaults.
func TestResolveOpsMonitorConfig(t *testing.T) {
	clearOpsMonitorEnv(t)

	t.Run("disabled skips validation", func(t *testing.T) {
		f := parseOpsMonitorFlags(t, "-ops-monitor-deployment", "mars", "-ops-monitor-notify-issue", "bogus",
			"-ops-monitor-interval", "0s", "-ops-monitor-load-factor", "-1")
		conf, err := resolveOpsMonitorConfig(false, f, noEnv)
		if err != nil || conf != nil {
			t.Fatalf("disabled resolve = (%+v, %v), want (nil, nil)", conf, err)
		}
	})

	t.Run("nil flags error", func(t *testing.T) {
		if _, err := resolveOpsMonitorConfig(true, nil, noEnv); err == nil {
			t.Fatal("enabled resolve with nil flags returned no error")
		}
	})

	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"unknown deployment", []string{"-ops-monitor-deployment", "mars"}, "--ops-monitor-deployment"},
		{"malformed notify issue", []string{"-ops-monitor-notify-issue", "bogus"}, "--ops-monitor-notify-issue"},
		{"notify issue zero", []string{"-ops-monitor-notify-issue", "acme/ops#0"}, "--ops-monitor-notify-issue"},
		{"zero interval", []string{"-ops-monitor-interval", "0s"}, "--ops-monitor-interval"},
		{"negative interval", []string{"-ops-monitor-interval", "-1m"}, "--ops-monitor-interval"},
		{"zero quiet window", []string{"-ops-monitor-quiet-window", "0s"}, "--ops-monitor-quiet-window"},
		{"negative quiet window", []string{"-ops-monitor-quiet-window", "-1h"}, "--ops-monitor-quiet-window"},
		{"zero disk min", []string{"-ops-monitor-disk-min-free-gb", "0"}, "--ops-monitor-disk-min-free-gb"},
		{"negative disk min", []string{"-ops-monitor-disk-min-free-gb", "-5"}, "--ops-monitor-disk-min-free-gb"},
		{"NaN disk min", []string{"-ops-monitor-disk-min-free-gb", "NaN"}, "--ops-monitor-disk-min-free-gb"},
		{"Inf disk min", []string{"-ops-monitor-disk-min-free-gb", "+Inf"}, "--ops-monitor-disk-min-free-gb"},
		{"overflowing disk min", []string{"-ops-monitor-disk-min-free-gb", "1e12"}, "--ops-monitor-disk-min-free-gb"},
		{"zero gocache max", []string{"-ops-monitor-gocache-max-gb", "0"}, "--ops-monitor-gocache-max-gb"},
		{"negative gocache max", []string{"-ops-monitor-gocache-max-gb", "-1"}, "--ops-monitor-gocache-max-gb"},
		{"zero load factor", []string{"-ops-monitor-load-factor", "0"}, "--ops-monitor-load-factor"},
		{"negative load factor", []string{"-ops-monitor-load-factor", "-4"}, "--ops-monitor-load-factor"},
		{"NaN load factor", []string{"-ops-monitor-load-factor", "NaN"}, "--ops-monitor-load-factor"},
		{"Inf load factor", []string{"-ops-monitor-load-factor", "+Inf"}, "--ops-monitor-load-factor"},
		{"zero amendment window", []string{"-ops-monitor-amendment-window", "0s"}, "--ops-monitor-amendment-window"},
		{"negative amendment window", []string{"-ops-monitor-amendment-window", "-1m"}, "--ops-monitor-amendment-window"},
		{"negative warn lead", []string{"-ops-monitor-amendment-warn-lead", "-1s"}, "--ops-monitor-amendment-warn-lead"},
		{"warn lead equal to window", []string{"-ops-monitor-amendment-warn-lead", "15m"}, "--ops-monitor-amendment-warn-lead"},
		{"warn lead above window", []string{"-ops-monitor-amendment-warn-lead", "20m"}, "--ops-monitor-amendment-warn-lead"},
		{"lookback equal to window", []string{"-ops-monitor-amendment-lookback", "15m"}, "--ops-monitor-amendment-lookback"},
		{"lookback below window", []string{"-ops-monitor-amendment-lookback", "1m"}, "--ops-monitor-amendment-lookback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf, err := resolveOpsMonitorConfig(true, parseOpsMonitorFlags(t, tc.args...), noEnv)
			if err == nil {
				t.Fatalf("resolve %v = %+v, want an error naming %s", tc.args, conf, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("resolve %v error = %q, want it to name %s", tc.args, err, tc.wantErr)
			}
		})
	}

	t.Run("valid defaults", func(t *testing.T) {
		conf, err := resolveOpsMonitorConfig(true, parseOpsMonitorFlags(t), noEnv)
		if err != nil {
			t.Fatalf("resolve defaults: %v", err)
		}
		want := opsMonitorConfig{
			Deployment:       opsmonitor.DeploymentLocal,
			Interval:         opsmonitor.DefaultInterval,
			QuietWindow:      opsmonitor.DefaultQuietWindow,
			DiskPath:         defaultOpsMonitorDiskPath(),
			DiskMinFreeBytes: 20 << 30,
			GoCacheDir:       "",
			GoCacheMaxBytes:  50 << 30,
			LoadFactor:       opsmonitor.DefaultLoadFactor,

			AmendmentWindow:   opsmonitor.DefaultAmendmentWindow,
			AmendmentWarnLead: opsmonitor.DefaultAmendmentWarnLead,
			AmendmentLookback: opsmonitor.DefaultAmendmentLookback,
		}
		if *conf != want {
			t.Errorf("resolved defaults = %+v, want %+v", *conf, want)
		}
	})

	t.Run("zero amendment warn lead is accepted verbatim", func(t *testing.T) {
		conf, err := resolveOpsMonitorConfig(true, parseOpsMonitorFlags(t, "-ops-monitor-amendment-warn-lead", "0"), noEnv)
		if err != nil {
			t.Fatalf("resolve warn lead 0: %v", err)
		}
		if conf.AmendmentWarnLead != 0 {
			t.Errorf("AmendmentWarnLead = %s, want 0 (no lead, never the default)", conf.AmendmentWarnLead)
		}
	})

	t.Run("auto resolves kubernetes from KUBERNETES_SERVICE_HOST", func(t *testing.T) {
		getenv := func(k string) string {
			if k == "KUBERNETES_SERVICE_HOST" {
				return "10.0.0.1"
			}
			return ""
		}
		conf, err := resolveOpsMonitorConfig(true, parseOpsMonitorFlags(t), getenv)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if conf.Deployment != opsmonitor.DeploymentKubernetes {
			t.Errorf("deployment = %q, want kubernetes", conf.Deployment)
		}
	})

	t.Run("valid explicit values", func(t *testing.T) {
		f := parseOpsMonitorFlags(t, "-ops-monitor-deployment", "remote", "-ops-monitor-interval", "90s",
			"-ops-monitor-quiet-window", "2h", "-ops-monitor-disk-path", "/data", "-ops-monitor-disk-min-free-gb", "1.5",
			"-ops-monitor-gocache-dir", "/cache", "-ops-monitor-gocache-max-gb", "10", "-ops-monitor-load-factor", "2.5",
			"-ops-monitor-notify-issue", "acme/ops#42", "-ops-monitor-amendment-window", "20m",
			"-ops-monitor-amendment-warn-lead", "5m", "-ops-monitor-amendment-lookback", "48h")
		conf, err := resolveOpsMonitorConfig(true, f, noEnv)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		want := opsMonitorConfig{
			Deployment:       opsmonitor.DeploymentRemote,
			Interval:         90 * time.Second,
			QuietWindow:      2 * time.Hour,
			DiskPath:         "/data",
			DiskMinFreeBytes: 3 << 29,
			GoCacheDir:       "/cache",
			GoCacheMaxBytes:  10 << 30,
			LoadFactor:       2.5,
			NotifyRepo:       forge.RepoRef{Owner: "acme", Name: "ops"},
			NotifyIssue:      42,

			AmendmentWindow:   20 * time.Minute,
			AmendmentWarnLead: 5 * time.Minute,
			AmendmentLookback: 48 * time.Hour,
		}
		if *conf != want {
			t.Errorf("resolved = %+v, want %+v", *conf, want)
		}
	})
}

// TestRegisterOpsMonitorFlags_DefaultsAndEnv pins condition 6 (the GOCACHE
// flag default is the empty string — nothing is resolved at registration —
// and the help says the resolution is lazy), the condition 8 help text, and
// the env fallbacks.
func TestRegisterOpsMonitorFlags_DefaultsAndEnv(t *testing.T) {
	clearOpsMonitorEnv(t)
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	f := registerOpsMonitorFlags(fs)
	if f.GoCacheDir != "" {
		t.Errorf("--ops-monitor-gocache-dir default = %q, want empty (resolved lazily at probe time)", f.GoCacheDir)
	}
	if usage := fs.Lookup("ops-monitor-gocache-dir").Usage; !strings.Contains(usage, "lazily") || !strings.Contains(usage, "go env GOCACHE") {
		t.Errorf("--ops-monitor-gocache-dir help does not state the lazy `go env GOCACHE` resolution: %q", usage)
	}
	if usage := fs.Lookup("ops-monitor-notify-issue").Usage; !strings.Contains(usage, "access-controlled") {
		t.Errorf("--ops-monitor-notify-issue help does not state the access-controlled requirement: %q", usage)
	}
	if f.Deployment != "auto" || f.Interval != opsmonitor.DefaultInterval || f.QuietWindow != opsmonitor.DefaultQuietWindow ||
		f.DiskMinFreeGB != 20 || f.GoCacheMaxGB != 50 || f.LoadFactor != opsmonitor.DefaultLoadFactor || f.NotifyIssue != "" ||
		f.AmendmentWindow != opsmonitor.DefaultAmendmentWindow || f.AmendmentWarnLead != opsmonitor.DefaultAmendmentWarnLead ||
		f.AmendmentLookback != opsmonitor.DefaultAmendmentLookback {
		t.Errorf("defaults = %+v", *f)
	}

	t.Setenv("FISHHAWKD_OPS_MONITOR_INTERVAL", "7m")
	t.Setenv("FISHHAWKD_OPS_MONITOR_DEPLOYMENT", "remote")
	t.Setenv("FISHHAWKD_OPS_MONITOR_QUIET_WINDOW", "3h")
	t.Setenv("FISHHAWKD_OPS_MONITOR_DISK_PATH", "/env/data")
	t.Setenv("FISHHAWKD_OPS_MONITOR_DISK_MIN_FREE_GB", "11")
	t.Setenv("FISHHAWKD_OPS_MONITOR_GOCACHE_DIR", "/env/cache")
	t.Setenv("FISHHAWKD_OPS_MONITOR_GOCACHE_MAX_GB", "12")
	t.Setenv("FISHHAWKD_OPS_MONITOR_LOAD_FACTOR", "3")
	t.Setenv("FISHHAWKD_OPS_MONITOR_NOTIFY_ISSUE", "acme/ops#7")
	t.Setenv("FISHHAWKD_OPS_MONITOR_AMENDMENT_WINDOW", "30m")
	t.Setenv("FISHHAWKD_OPS_MONITOR_AMENDMENT_WARN_LEAD", "0s")
	t.Setenv("FISHHAWKD_OPS_MONITOR_AMENDMENT_LOOKBACK", "24h")
	g := registerOpsMonitorFlags(flag.NewFlagSet("env", flag.ContinueOnError))
	want := opsMonitorFlags{
		Interval: 7 * time.Minute, Deployment: "remote", QuietWindow: 3 * time.Hour, DiskPath: "/env/data",
		DiskMinFreeGB: 11, GoCacheDir: "/env/cache", GoCacheMaxGB: 12, LoadFactor: 3, NotifyIssue: "acme/ops#7",
		AmendmentWindow: 30 * time.Minute, AmendmentWarnLead: 0, AmendmentLookback: 24 * time.Hour,
	}
	if *g != want {
		t.Errorf("env fallbacks = %+v, want %+v", *g, want)
	}
}

// opsLogRecorder is a slog handler capturing every record.
type opsLogRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *opsLogRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *opsLogRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *opsLogRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *opsLogRecorder) WithGroup(string) slog.Handler      { return h }

func (h *opsLogRecorder) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

func (h *opsLogRecorder) atOrAbove(level slog.Level) []string {
	var out []string
	for _, r := range h.snapshot() {
		if r.Level >= level {
			out = append(out, r.Message)
		}
	}
	return out
}

func (h *opsLogRecorder) has(level slog.Level, substr string) bool {
	for _, r := range h.snapshot() {
		if r.Level == level && strings.Contains(r.Message, substr) {
			return true
		}
	}
	return false
}

func validOpsConf() opsMonitorConfig {
	return opsMonitorConfig{
		Deployment:       opsmonitor.DeploymentLocal,
		Interval:         time.Minute,
		QuietWindow:      time.Hour,
		DiskPath:         "/data",
		DiskMinFreeBytes: 1 << 30,
		GoCacheMaxBytes:  1 << 30,
		LoadFactor:       4,
	}
}

// TestNewOpsMonitorTicker_Wiring pins the probe set, the ticker fields, the
// Ping override seam and the three notifier branches.
func TestNewOpsMonitorTicker_Wiring(t *testing.T) {
	rec := &opsLogRecorder{}
	logger := slog.New(rec)
	conf := validOpsConf()
	conf.Deployment = opsmonitor.DeploymentRemote
	conf.Interval = 3 * time.Minute
	conf.QuietWindow = 2 * time.Hour

	t.Run("probe set and fields, audit-only without a notify issue", func(t *testing.T) {
		tk := newOpsMonitorTicker(conf, opsMonitorDeps{Logger: logger})
		var names []string
		for _, p := range tk.Probes {
			names = append(names, p.Name())
		}
		if got, want := strings.Join(names, ","), "disk,gocache,load,docker,postgres,runners,amendment_window,amendment_terminal"; got != want {
			t.Errorf("probes = %s, want %s", got, want)
		}
		// The amendment durations are passed verbatim: a zero warn lead stays
		// zero (no lead), never the default.
		c := conf
		c.AmendmentWindow, c.AmendmentWarnLead, c.AmendmentLookback = 20*time.Minute, 0, 48*time.Hour
		probes := newOpsProbes(c, opsMonitorDeps{})
		wp, ok := probes[6].(opsmonitor.AmendmentWindowProbe)
		if !ok || wp.Window != 20*time.Minute || wp.WarnLead != 0 || wp.Lookback != 48*time.Hour {
			t.Errorf("amendment_window probe = %#v, want window 20m, warn lead 0, lookback 48h", probes[6])
		}
		if tp, ok := probes[7].(opsmonitor.AmendmentTerminalProbe); !ok || tp.Lookback != 48*time.Hour {
			t.Errorf("amendment_terminal probe = %#v, want lookback 48h", probes[7])
		}
		if tk.Deployment != opsmonitor.DeploymentRemote || tk.Interval != 3*time.Minute || tk.QuietWindow != 2*time.Hour {
			t.Errorf("ticker = {deployment %s interval %s quiet %s}", tk.Deployment, tk.Interval, tk.QuietWindow)
		}
		if tk.Notifier != nil {
			t.Errorf("Notifier = %T, want nil (no notify issue)", tk.Notifier)
		}
		if tk.Logger != logger {
			t.Error("ticker logger is not the deps logger")
		}
	})

	t.Run("notify issue without a GitHub client warns and runs audit-only", func(t *testing.T) {
		c := conf
		c.NotifyRepo, c.NotifyIssue = forge.RepoRef{Owner: "acme", Name: "ops"}, 9
		tk := newOpsMonitorTicker(c, opsMonitorDeps{Logger: logger})
		if tk.Notifier != nil {
			t.Errorf("Notifier = %T, want nil without a GitHub client", tk.Notifier)
		}
		if !rec.has(slog.LevelWarn, "no GitHub App client") {
			t.Errorf("no WARN naming the missing GitHub client; records: %v", rec.atOrAbove(slog.LevelDebug))
		}
	})

	t.Run("notify issue with a GitHub client posts to that issue", func(t *testing.T) {
		c := conf
		c.NotifyRepo, c.NotifyIssue = forge.RepoRef{Owner: "acme", Name: "ops"}, 9
		gh := &githubclient.Client{}
		tk := newOpsMonitorTicker(c, opsMonitorDeps{Logger: logger, GitHub: gh})
		n, ok := tk.Notifier.(opsmonitor.IssueCommentNotifier)
		if !ok {
			t.Fatalf("Notifier = %T, want opsmonitor.IssueCommentNotifier", tk.Notifier)
		}
		if n.Repo != c.NotifyRepo || n.Issue != 9 || n.QuietWindow != 2*time.Hour || n.Client != opsmonitor.IssueCommentClient(gh) {
			t.Errorf("notifier = %+v", n)
		}
	})

	t.Run("seam Ping overrides the deps Ping", func(t *testing.T) {
		depsErr := errors.New("deps ping")
		seamErr := errors.New("seam ping")
		var seamCalls atomic.Int32
		probes := newOpsProbes(validOpsConf(), opsMonitorDeps{
			Ping: func(context.Context) error { return depsErr },
			Seams: opsMonitorSeams{Ping: func(context.Context) error {
				seamCalls.Add(1)
				return seamErr
			}},
		})
		pg := probes[4].(opsmonitor.PostgresProbe)
		if err := pg.Ping(context.Background()); !errors.Is(err, seamErr) || seamCalls.Load() != 1 {
			t.Errorf("postgres Ping = %v (seam calls %d), want the seam ping", err, seamCalls.Load())
		}
		probes = newOpsProbes(validOpsConf(), opsMonitorDeps{Ping: func(context.Context) error { return depsErr }})
		if err := probes[4].(opsmonitor.PostgresProbe).Ping(context.Background()); !errors.Is(err, depsErr) {
			t.Errorf("postgres Ping without a seam = %v, want the deps ping", err)
		}
	})
}

// failingOpsSeams returns seams that all error, plus a counter of disk probe
// calls (one per sweep).
func failingOpsSeams() (opsMonitorSeams, *atomic.Int32) {
	var sweeps atomic.Int32
	boom := errors.New("seam failed")
	return opsMonitorSeams{
		Statfs: func(string) (uint64, uint64, error) {
			sweeps.Add(1)
			return 0, 0, boom
		},
		ResolveDir:  func(context.Context) (string, error) { return "", boom },
		ReadLoad:    func(context.Context) (float64, error) { return 0, boom },
		Cores:       func() int { return 0 },
		DockerExec:  func(context.Context, string, ...string) ([]byte, error) { return nil, boom },
		ListRunners: func(context.Context) ([]stalesweep.RunnerProcess, error) { return nil, boom },
		Ping:        func(context.Context) error { return boom },
	}, &sweeps
}

// waitFor polls cond every 20ms until it holds or the scaled deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timescale.D(20 * time.Second))
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestNewOpsMonitorTicker_AllSeamsFailing builds the ticker through the
// wiring helper with every seam erroring, runs several 10ms ticks through
// startOpsMonitor, cancels cleanly, and asserts no record at ERROR or above
// and no ok result.
func TestNewOpsMonitorTicker_AllSeamsFailing(t *testing.T) {
	seams, sweeps := failingOpsSeams()
	rec := &opsLogRecorder{}
	conf := validOpsConf()
	conf.Interval = 10 * time.Millisecond
	tk := newOpsMonitorTicker(conf, opsMonitorDeps{
		Runs:   stubRunReader{},
		Logger: slog.New(rec),
		Seams:  seams,
	})

	for _, r := range tk.Tick(context.Background()) {
		if r.Status == opsmonitor.StatusOK {
			t.Errorf("probe %s read ok with every seam failing: %+v", r.Probe, r)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := startOpsMonitor(ctx, tk, slog.New(rec))
	waitFor(t, "three ticker sweeps", func() bool { return sweeps.Load() >= 4 })
	cancel()
	select {
	case <-done:
	case <-time.After(timescale.D(10 * time.Second)):
		t.Fatal("ops monitor goroutine did not exit after cancel")
	}
	if errs := rec.atOrAbove(slog.LevelError); len(errs) > 0 {
		t.Errorf("ops monitor logged at ERROR or above: %v", errs)
	}
}

type stubRunReader struct{}

func (stubRunReader) GetRun(context.Context, uuid.UUID) (*runpkg.Run, error) {
	return nil, errors.New("stub")
}
func (stubRunReader) GetStage(context.Context, uuid.UUID) (*runpkg.Stage, error) {
	return nil, errors.New("stub")
}

type panickingRunner struct{}

func (panickingRunner) Run(context.Context) error { panic("secret /Users/someone/path") }

type erroringRunner struct{}

func (erroringRunner) Run(context.Context) error { return errors.New("no probes") }

// TestStartOpsMonitor_RecoversPanic: a panicking Run is a WARN that does not
// carry the panic value, never a crash and never an ERROR.
func TestStartOpsMonitor_RecoversPanic(t *testing.T) {
	rec := &opsLogRecorder{}
	done := startOpsMonitor(context.Background(), panickingRunner{}, slog.New(rec))
	select {
	case <-done:
	case <-time.After(timescale.D(10 * time.Second)):
		t.Fatal("goroutine did not exit")
	}
	if !rec.has(slog.LevelWarn, "panicked") {
		t.Errorf("no panic WARN; records: %v", rec.atOrAbove(slog.LevelDebug))
	}
	for _, r := range rec.snapshot() {
		if strings.Contains(r.Message, "/Users/someone") {
			t.Errorf("panic value leaked into the log: %q", r.Message)
		}
	}
	if errs := rec.atOrAbove(slog.LevelError); len(errs) > 0 {
		t.Errorf("logged at ERROR: %v", errs)
	}
}

// TestStartOpsMonitor_RunErrorIsWarn: a Run error is a WARN, never an ERROR.
func TestStartOpsMonitor_RunErrorIsWarn(t *testing.T) {
	rec := &opsLogRecorder{}
	<-startOpsMonitor(context.Background(), erroringRunner{}, slog.New(rec))
	if !rec.has(slog.LevelWarn, "exited with error") {
		t.Errorf("no WARN for the Run error; records: %v", rec.atOrAbove(slog.LevelDebug))
	}
	if errs := rec.atOrAbove(slog.LevelError); len(errs) > 0 {
		t.Errorf("logged at ERROR: %v", errs)
	}
}

// TestPoolPing: nil pool → nil Ping (the postgres probe reads unknown); a
// pool at an unreachable address → a Ping that errors within its bound.
func TestPoolPing(t *testing.T) {
	if poolPing(nil) != nil {
		t.Error("poolPing(nil) != nil")
	}
	pool, err := pgxpool.New(context.Background(), "postgres://fishhawk@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()
	ping := poolPing(pool)
	if ping == nil {
		t.Fatal("poolPing(pool) = nil")
	}
	if err := ping(context.Background()); err == nil {
		t.Error("ping of an unreachable pool returned nil")
	}
}

// syncBuffer is a mutex-guarded log sink for a runServe running on another
// goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// errorRecords returns every JSON log line at level ERROR.
func errorRecords(log string) []string {
	var out []string
	for _, line := range strings.Split(log, "\n") {
		var rec struct {
			Level string `json:"level"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Level == "ERROR" {
			out = append(out, line)
		}
	}
	return out
}

// serveUntil runs runServe on a goroutine with a test-owned shutdown context
// (the serveContext seam), waits until the log shows a live listener and
// ready(log) holds, then cancels and returns the exit code and log. It also
// reports whether newServer was reached.
func serveUntil(t *testing.T, ready func(log string) bool, args ...string) (code int, log string, constructed bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	origCtx := serveContext
	serveContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	t.Cleanup(func() { serveContext = origCtx })
	var reached atomic.Bool
	origNew := newServer
	newServer = func(cfg server.Config) *server.Server {
		reached.Store(true)
		return origNew(cfg)
	}
	t.Cleanup(func() { newServer = origNew })

	sink := &syncBuffer{}
	codeCh := make(chan int, 1)
	go func() { codeCh <- runServe(append([]string{"-addr", "127.0.0.1:0"}, args...), sink) }()

	deadline := time.After(timescale.D(60 * time.Second))
	for {
		select {
		case c := <-codeCh:
			return c, sink.String(), reached.Load()
		case <-deadline:
			cancel()
			t.Fatalf("runServe did not reach a live listener with the ready condition holding; log:\n%s", sink.String())
		case <-time.After(20 * time.Millisecond):
		}
		if l := sink.String(); strings.Contains(l, "fishhawkd listening") && ready(l) {
			break
		}
	}
	cancel()
	select {
	case c := <-codeCh:
		return c, sink.String(), reached.Load()
	case <-time.After(timescale.D(60 * time.Second)):
		t.Fatalf("runServe did not shut down after cancel; log:\n%s", sink.String())
	}
	return 0, "", false
}

// TestServe_OpsMonitorInvalidConfigFailsStartup (condition 4):
// --enable-ops-monitor with a malformed notify issue fails startup BEFORE the
// server is constructed. The shutdown context is already cancelled, so with
// the refusal deleted runServe would construct the server and exit 0.
func TestServe_OpsMonitorInvalidConfigFailsStartup(t *testing.T) {
	clearOpsMonitorEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	origCtx := serveContext
	serveContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	t.Cleanup(func() { serveContext = origCtx })
	var reached atomic.Bool
	origNew := newServer
	newServer = func(cfg server.Config) *server.Server {
		reached.Store(true)
		return origNew(cfg)
	}
	t.Cleanup(func() { newServer = origNew })

	code, log := serveWithProfile(t, "-addr", "127.0.0.1:0", "--enable-ops-monitor", "--ops-monitor-notify-issue", "bogus")
	if code != exitFailure {
		t.Fatalf("runServe exit = %d, want %d; log:\n%s", code, exitFailure, log)
	}
	if reached.Load() {
		t.Error("newServer was reached; an invalid ops monitor config must fail startup before the server is constructed")
	}
	if !strings.Contains(log, "invalid ops monitor configuration") || !strings.Contains(log, "--ops-monitor-notify-issue") {
		t.Errorf("log does not name the invalid flag:\n%s", log)
	}
}

// TestServe_OpsMonitorStartsWithFailingSeams (condition 4): a valid config
// with every probe seam erroring → the server is constructed, the listener
// serves, the monitor sweeps several times, and nothing logs at ERROR.
func TestServe_OpsMonitorStartsWithFailingSeams(t *testing.T) {
	clearOpsMonitorEnv(t)
	seams, sweeps := failingOpsSeams()
	orig := serveOpsMonitorSeams
	serveOpsMonitorSeams = seams
	t.Cleanup(func() { serveOpsMonitorSeams = orig })

	code, log, constructed := serveUntil(t, func(l string) bool {
		return strings.Contains(l, "ops monitor started") && sweeps.Load() >= 3
	}, "--enable-ops-monitor", "--ops-monitor-interval", "10ms", "--ops-monitor-deployment", "local",
		"--ops-monitor-disk-path", t.TempDir())
	if code != exitOK {
		t.Fatalf("runServe exit = %d, want %d; log:\n%s", code, exitOK, log)
	}
	if !constructed || !strings.Contains(log, "fishhawkd listening") {
		t.Errorf("server not constructed (%v) or never listened; log:\n%s", constructed, log)
	}
	if !strings.Contains(log, `"msg":"ops monitor started"`) || !strings.Contains(log, `"deployment":"local"`) {
		t.Errorf("no 'ops monitor started' line carrying the deployment:\n%s", log)
	}
	if errs := errorRecords(log); len(errs) > 0 {
		t.Errorf("serve logged at ERROR with the ops monitor on and every probe seam failing:\n%s", strings.Join(errs, "\n"))
	}
}

// TestServe_OpsMonitorDisabledByDefault (condition 4): the default boot
// serves without starting the monitor — no 'ops monitor started' line and no
// probe ever runs.
func TestServe_OpsMonitorDisabledByDefault(t *testing.T) {
	clearOpsMonitorEnv(t)
	seams, sweeps := failingOpsSeams()
	orig := serveOpsMonitorSeams
	serveOpsMonitorSeams = seams
	t.Cleanup(func() { serveOpsMonitorSeams = orig })

	code, log, constructed := serveUntil(t, func(string) bool { return true },
		"--ops-monitor-interval", "10ms", "--ops-monitor-disk-path", t.TempDir())
	if code != exitOK {
		t.Fatalf("runServe exit = %d, want %d; log:\n%s", code, exitOK, log)
	}
	if !constructed || !strings.Contains(log, "fishhawkd listening") {
		t.Errorf("server not constructed (%v) or never listened; log:\n%s", constructed, log)
	}
	if strings.Contains(log, "ops monitor started") {
		t.Errorf("disabled default logged 'ops monitor started':\n%s", log)
	}
	if n := sweeps.Load(); n != 0 {
		t.Errorf("disabled default ran the disk probe %d times, want 0", n)
	}
}

// runOpsProbeCmd drives run([]string{"ops-probe", ...}) with the stdout and
// seam package vars swapped.
func runOpsProbeCmd(t *testing.T, seams opsMonitorSeams, args ...string) (code int, stdout, logs string) {
	t.Helper()
	var out, logSink strings.Builder
	origOut, origSeams := opsProbeOut, opsProbeSeams
	opsProbeOut, opsProbeSeams = &out, seams
	t.Cleanup(func() { opsProbeOut, opsProbeSeams = origOut, origSeams })
	code = run(append([]string{"ops-probe"}, args...), &logSink)
	return code, out.String(), logSink.String()
}

func decodeOpsResults(t *testing.T, stdout string) map[string]opsmonitor.Result {
	t.Helper()
	var results []opsmonitor.Result
	if err := json.Unmarshal([]byte(stdout), &results); err != nil {
		t.Fatalf("decode --json output: %v\n%s", err, stdout)
	}
	byProbe := map[string]opsmonitor.Result{}
	for _, r := range results {
		byProbe[r.Probe] = r
	}
	return byProbe
}

// countingFailingSeams are seams that would make every host probe fail, with
// a shared call counter.
func countingFailingSeams() (opsMonitorSeams, *atomic.Int32) {
	var calls atomic.Int32
	return opsMonitorSeams{
		Statfs: func(string) (uint64, uint64, error) { calls.Add(1); return 0, 1 << 40, nil },
		ResolveDir: func(context.Context) (string, error) {
			calls.Add(1)
			return "", errors.New("x")
		},
		ReadLoad:   func(context.Context) (float64, error) { calls.Add(1); return 1000, nil },
		Cores:      func() int { return 1 },
		DockerExec: func(context.Context, string, ...string) ([]byte, error) { calls.Add(1); return nil, errors.New("x") },
		ListRunners: func(context.Context) ([]stalesweep.RunnerProcess, error) {
			calls.Add(1)
			return nil, errors.New("x")
		},
	}, &calls
}

// TestOpsProbe_NonLocalAllHostProbesNotApplicable: under kubernetes or remote,
// via BOTH --deployment and --ops-monitor-deployment (condition 5), every
// host-local probe is not_applicable without running, postgres reads unknown
// without --db, and the exit is 0.
func TestOpsProbe_NonLocalAllHostProbesNotApplicable(t *testing.T) {
	clearOpsMonitorEnv(t)
	for _, flagName := range []string{"--deployment", "--ops-monitor-deployment"} {
		for _, dep := range []string{"kubernetes", "remote"} {
			t.Run(flagName+"="+dep, func(t *testing.T) {
				seams, calls := countingFailingSeams()
				code, stdout, logs := runOpsProbeCmd(t, seams, flagName, dep, "--json")
				if code != exitOK {
					t.Fatalf("exit = %d, want %d; stdout:\n%s\nlogs:\n%s", code, exitOK, stdout, logs)
				}
				byProbe := decodeOpsResults(t, stdout)
				for _, name := range []string{"disk", "gocache", "load", "docker", "runners"} {
					r, ok := byProbe[name]
					if !ok {
						t.Errorf("no %s result:\n%s", name, stdout)
						continue
					}
					if r.Status != opsmonitor.StatusNotApplicable || !strings.Contains(r.Reason, dep) {
						t.Errorf("%s = %+v, want not_applicable naming %s", name, r, dep)
					}
				}
				for _, name := range []string{"postgres", "amendment_window", "amendment_terminal"} {
					if r := byProbe[name]; r.Status != opsmonitor.StatusUnknown || r.Reason != "no database configured" {
						t.Errorf("%s = %+v, want unknown (no database configured), never not_applicable", name, r)
					}
				}
				if n := calls.Load(); n != 0 {
					t.Errorf("host probe seams called %d times under %s, want 0", n, dep)
				}
			})
		}
	}
}

// okOpsSeams make every host probe read ok (gocache via a real marked dir).
func okOpsSeams(t *testing.T) opsMonitorSeams {
	t.Helper()
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(cache, "README"),
		[]byte("This directory holds cached build artifacts from the Go build system.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return opsMonitorSeams{
		ResolveDir:  func(context.Context) (string, error) { return cache, nil },
		ReadLoad:    func(context.Context) (float64, error) { return 0.5, nil },
		Cores:       func() int { return 4 },
		DockerExec:  func(context.Context, string, ...string) ([]byte, error) { return []byte("27.0.0\n"), nil },
		ListRunners: func(context.Context) ([]stalesweep.RunnerProcess, error) { return nil, nil },
	}
}

// TestOpsProbe_DiskFailingExitsOne: a real statfs on t.TempDir() against an
// impossible floor reads failing disk_free_low with a remedy, and the exit is
// 1; the same probe with a tiny floor exits 0.
func TestOpsProbe_DiskFailingExitsOne(t *testing.T) {
	clearOpsMonitorEnv(t)
	dir := t.TempDir()

	code, stdout, logs := runOpsProbeCmd(t, okOpsSeams(t), "--deployment", "local",
		"--ops-monitor-disk-path", dir, "--ops-monitor-disk-min-free-gb", "1000000000")
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d; stdout:\n%s\nlogs:\n%s", code, exitFailure, stdout, logs)
	}
	var diskLine string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "failing disk ") {
			diskLine = line
		}
	}
	if diskLine == "" {
		t.Fatalf("no `failing disk` line:\n%s", stdout)
	}
	for _, want := range []string{`condition="disk_free_low"`, "observed=", "threshold=", `remedy="Free space`} {
		if !strings.Contains(diskLine, want) {
			t.Errorf("disk line missing %s: %s", want, diskLine)
		}
	}
	for _, want := range []string{"ok gocache ", "ok load ", "ok docker ", `unknown postgres reason="no database configured"`, "unknown runners ",
		`unknown amendment_window reason="no database configured"`, `unknown amendment_terminal reason="no database configured"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, logs = runOpsProbeCmd(t, okOpsSeams(t), "--deployment", "local",
		"--ops-monitor-disk-path", dir, "--ops-monitor-disk-min-free-gb", "0.000001")
	if code != exitOK {
		t.Fatalf("tiny floor exit = %d, want %d; stdout:\n%s\nlogs:\n%s", code, exitOK, stdout, logs)
	}
	if !strings.Contains(stdout, "ok disk ") {
		t.Errorf("no `ok disk` line:\n%s", stdout)
	}
}

// TestOpsProbe_WithDB drives --db against a real Postgres: postgres pings ok
// and the runners probe reads the real run repository (an attributed runner
// whose stage this tracker has never seen — another backend, or a tenanted
// run hidden by row-level security — is unobserved → unknown, never ok).
func TestOpsProbe_WithDB(t *testing.T) {
	clearOpsMonitorEnv(t)
	seams := okOpsSeams(t)
	runID, stageID := uuid.New(), uuid.New()
	seams.ListRunners = func(context.Context) ([]stalesweep.RunnerProcess, error) {
		return []stalesweep.RunnerProcess{{PID: "4242", RunID: &runID, StageID: &stageID}}, nil
	}
	code, stdout, logs := runOpsProbeCmd(t, seams, "--deployment", "local", "--db", pgtest.NewURL(t),
		"--ops-monitor-disk-path", t.TempDir(), "--ops-monitor-disk-min-free-gb", "0.000001", "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stdout:\n%s\nlogs:\n%s", code, exitOK, stdout, logs)
	}
	byProbe := decodeOpsResults(t, stdout)
	if pg := byProbe["postgres"]; pg.Status != opsmonitor.StatusOK {
		t.Errorf("postgres = %+v, want ok", pg)
	}
	if rn := byProbe["runners"]; rn.Status != opsmonitor.StatusUnknown || !strings.Contains(rn.Reason, "1 fishhawk-runner process(es) not found in this tracker") {
		t.Errorf("runners = %+v, want unknown naming 1 runner not found", rn)
	}
	// The amendment probes cross CLI -> lister -> SQL: an empty table reads ok.
	for _, name := range []string{"amendment_window", "amendment_terminal"} {
		if r := byProbe[name]; r.Status != opsmonitor.StatusOK || !strings.Contains(r.Observed, "0 pending amendment(s) scanned") {
			t.Errorf("%s = %+v, want ok over the real scan", name, r)
		}
	}
}

// TestOpsMonitorAmendmentWindowDefaultMatchesPollWindow pins the amendment
// window default to the implement agent's decision wait-poll.
func TestOpsMonitorAmendmentWindowDefaultMatchesPollWindow(t *testing.T) {
	if want := time.Duration(server.AmendmentPollWindowSeconds) * time.Second; opsmonitor.DefaultAmendmentWindow != want {
		t.Errorf("opsmonitor.DefaultAmendmentWindow = %s, server.AmendmentPollWindowSeconds = %s: update "+
			"DefaultAmendmentWindow, backend/internal/opsmonitor/README.md, backend/cmd/fishhawkd/README.md, "+
			".env.example and the --ops-monitor-amendment-window help together with the poll window",
			opsmonitor.DefaultAmendmentWindow, want)
	}
}

// TestOpsAmendmentReader_NilPoolIsNilInterface: a nil pool is a nil
// INTERFACE (the probes then read unknown), never a typed nil.
func TestOpsAmendmentReader_NilPoolIsNilInterface(t *testing.T) {
	if r := opsAmendmentReader(nil); r != nil {
		t.Errorf("opsAmendmentReader(nil) = %#v, want a nil interface", r)
	}
	pool, err := pgxpool.New(context.Background(), "postgres://fishhawk@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()
	if opsAmendmentReader(pool) == nil {
		t.Error("opsAmendmentReader(pool) = nil, want the postgres lister")
	}
}

// TestOpsProbe_DBConnectFailureExitsOne: an unreachable --db exits 1.
func TestOpsProbe_DBConnectFailureExitsOne(t *testing.T) {
	clearOpsMonitorEnv(t)
	code, stdout, logs := runOpsProbeCmd(t, okOpsSeams(t), "--db", "postgres://fishhawk@127.0.0.1:1/none?connect_timeout=1")
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d; stdout:\n%s\nlogs:\n%s", code, exitFailure, stdout, logs)
	}
	if !strings.Contains(logs, "connect") {
		t.Errorf("logs do not name the connect failure:\n%s", logs)
	}
	if stdout != "" {
		t.Errorf("a connect failure printed results:\n%s", stdout)
	}
}

// TestOpsProbe_UsageAndBadFlags: every usage error exits 2 without sweeping.
func TestOpsProbe_UsageAndBadFlags(t *testing.T) {
	clearOpsMonitorEnv(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"--no-such-flag"}},
		{"positional argument", []string{"extra"}},
		{"unknown deployment", []string{"--deployment", "mars"}},
		{"malformed notify issue", []string{"--ops-monitor-notify-issue", "bogus"}},
		{"non-positive threshold", []string{"--ops-monitor-disk-min-free-gb", "0"}},
		{"non-positive interval", []string{"--ops-monitor-interval", "0s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seams, calls := countingFailingSeams()
			code, stdout, logs := runOpsProbeCmd(t, seams, tc.args...)
			if code != exitUsage {
				t.Fatalf("exit = %d, want %d; stdout:\n%s\nlogs:\n%s", code, exitUsage, stdout, logs)
			}
			if stdout != "" || calls.Load() != 0 {
				t.Errorf("a usage error swept (stdout %q, seam calls %d)", stdout, calls.Load())
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

// TestOpsProbe_JSONEncodeErrorExitsOne: an unwritable stdout under --json
// exits 1 rather than reporting a clean sweep.
func TestOpsProbe_JSONEncodeErrorExitsOne(t *testing.T) {
	clearOpsMonitorEnv(t)
	var logs strings.Builder
	if code := runOpsProbeWith([]string{"--deployment", "remote", "--json"}, failingWriter{}, &logs, opsMonitorSeams{}); code != exitFailure {
		t.Fatalf("exit = %d, want %d; logs:\n%s", code, exitFailure, logs.String())
	}
	if !strings.Contains(logs.String(), "encode") {
		t.Errorf("logs do not name the encode failure:\n%s", logs.String())
	}
}

// TestFormatOpsProbeLine pins the text line shape: empty fields omitted,
// values quoted.
func TestFormatOpsProbeLine(t *testing.T) {
	got := formatOpsProbeLine(opsmonitor.Result{
		Probe: "disk", Status: opsmonitor.StatusFailing, Condition: "disk_free_low",
		Observed: "1.0 GiB free", Threshold: ">= 20.0 GiB free", Remedy: "free space",
	})
	want := `failing disk condition="disk_free_low" observed="1.0 GiB free" threshold=">= 20.0 GiB free" remedy="free space"`
	if got != want {
		t.Errorf("line = %s\nwant   %s", got, want)
	}
	got = formatOpsProbeLine(opsmonitor.Result{Probe: "docker", Status: opsmonitor.StatusNotApplicable, Reason: "skipped"})
	if want := `not_applicable docker reason="skipped"`; got != want {
		t.Errorf("line = %s, want %s", got, want)
	}
}

// TestGibToBytes pins the conversion and its overflow bound.
func TestGibToBytes(t *testing.T) {
	if b, err := gibToBytes("x", 2); err != nil || b != 2<<30 {
		t.Errorf("gibToBytes(2) = %d, %v", b, err)
	}
	if b, err := gibToBytes("x", maxOpsMonitorGB); err != nil || b == 0 {
		t.Errorf("gibToBytes(max) = %d, %v; want a non-overflowing value", b, err)
	}
	if _, err := gibToBytes("x", math.Nextafter(maxOpsMonitorGB, math.Inf(1))); err == nil {
		t.Error("gibToBytes above the bound returned no error")
	}
}

// TestDefaultOpsMonitorDiskPath: the default is the home directory, and an
// unresolvable home yields "" (the disk probe then reads unknown).
func TestDefaultOpsMonitorDiskPath(t *testing.T) {
	t.Setenv("HOME", "/tmp/ops-home")
	if got := defaultOpsMonitorDiskPath(); got != "/tmp/ops-home" {
		t.Errorf("defaultOpsMonitorDiskPath() = %q, want the home directory", got)
	}
	t.Setenv("HOME", "")
	if got := defaultOpsMonitorDiskPath(); got != "" {
		t.Errorf("defaultOpsMonitorDiskPath() with no home = %q, want empty", got)
	}
}

// TestNewOpsMonitorTicker_NilLoggerUsesDefault: a nil deps logger falls back
// to slog.Default() rather than leaving the ticker logger-less.
func TestNewOpsMonitorTicker_NilLoggerUsesDefault(t *testing.T) {
	if tk := newOpsMonitorTicker(validOpsConf(), opsMonitorDeps{}); tk.Logger != slog.Default() {
		t.Errorf("ticker logger = %p, want slog.Default()", tk.Logger)
	}
}

// TestServeContext_StopCancels: the production shutdown context is
// cancelled by its stop func (the signal half is the os/signal contract).
func TestServeContext_StopCancels(t *testing.T) {
	ctx, stop := serveContext()
	if ctx.Err() != nil {
		t.Fatalf("fresh serve context already done: %v", ctx.Err())
	}
	stop()
	select {
	case <-ctx.Done():
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("serve context not cancelled by stop")
	}
}
