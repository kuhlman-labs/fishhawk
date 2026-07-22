package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func validEnv() map[string]string {
	return map[string]string{
		envDatabaseURL:                         "postgres://fishhawk@localhost:5432/fishhawk_directory?sslmode=disable",
		"FISHHAWK_DIRECTORY_SUPPORTED_REGIONS": "us,eu,au",
		"FISHHAWK_DIRECTORY_CELL_BASE_URLS":    "us=https://us.app.fishhawk.test,eu=https://eu.app.fishhawk.test,au=https://au.app.fishhawk.test",
		"FISHHAWK_DIRECTORY_HANDOFF_SECRET":    "shared-secret",
	}
}

func TestLoadServeConfigDefaults(t *testing.T) {
	var log bytes.Buffer
	cfg, err := loadServeConfig(nil, &log, env(validEnv()))
	if err != nil {
		t.Fatalf("loadServeConfig: %v", err)
	}
	if cfg.addr != defaultAddr {
		t.Fatalf("addr: got %q want %q", cfg.addr, defaultAddr)
	}
	if len(cfg.routing.SupportedRegions) != 3 {
		t.Fatalf("regions: %v", cfg.routing.SupportedRegions)
	}
	if cfg.routing.HandoffTTL <= 0 {
		t.Fatalf("HandoffTTL: %s", cfg.routing.HandoffTTL)
	}
}

func TestLoadServeConfigAddrPrecedence(t *testing.T) {
	kv := validEnv()
	kv[envAddr] = ":9999"

	cfg, err := loadServeConfig(nil, &bytes.Buffer{}, env(kv))
	if err != nil {
		t.Fatalf("loadServeConfig: %v", err)
	}
	if cfg.addr != ":9999" {
		t.Fatalf("env addr: got %q want :9999", cfg.addr)
	}

	// An explicit flag beats the environment.
	cfg, err = loadServeConfig([]string{"--addr", ":7777"}, &bytes.Buffer{}, env(kv))
	if err != nil {
		t.Fatalf("loadServeConfig: %v", err)
	}
	if cfg.addr != ":7777" {
		t.Fatalf("flag addr: got %q want :7777", cfg.addr)
	}
}

func TestLoadServeConfigCustomTTL(t *testing.T) {
	kv := validEnv()
	kv["FISHHAWK_DIRECTORY_HANDOFF_TTL"] = "30s"
	cfg, err := loadServeConfig(nil, &bytes.Buffer{}, env(kv))
	if err != nil {
		t.Fatalf("loadServeConfig: %v", err)
	}
	if cfg.routing.HandoffTTL != 30*time.Second {
		t.Fatalf("HandoffTTL: got %s want 30s", cfg.routing.HandoffTTL)
	}
}

// Startup fails closed on every incomplete configuration rather than
// booting a directory that cannot route.
func TestLoadServeConfigFailsClosed(t *testing.T) {
	cases := map[string]struct {
		args    []string
		mutate  func(map[string]string)
		wantSub string
	}{
		"no database url": {
			nil,
			func(kv map[string]string) { kv[envDatabaseURL] = "  " },
			envDatabaseURL,
		},
		"no supported regions": {
			nil,
			func(kv map[string]string) { kv["FISHHAWK_DIRECTORY_SUPPORTED_REGIONS"] = "" },
			"FISHHAWK_DIRECTORY_SUPPORTED_REGIONS",
		},
		"region without a cell": {
			nil,
			func(kv map[string]string) {
				kv["FISHHAWK_DIRECTORY_CELL_BASE_URLS"] = "us=https://us.app.fishhawk.test"
			},
			"have no cell base URL",
		},
		"no handoff secret": {
			nil,
			func(kv map[string]string) { kv["FISHHAWK_DIRECTORY_HANDOFF_SECRET"] = "" },
			"FISHHAWK_DIRECTORY_HANDOFF_SECRET",
		},
		"bad flag": {
			[]string{"--nope"},
			func(map[string]string) {},
			"parse flags",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			kv := validEnv()
			tc.mutate(kv)
			cfg, err := loadServeConfig(tc.args, &bytes.Buffer{}, env(kv))
			if err == nil {
				t.Fatalf("expected failure, got %+v", cfg)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

// runServe must exit non-zero (and never listen) on an invalid config.
func TestRunServeExitsOnInvalidConfig(t *testing.T) {
	kv := validEnv()
	kv[envDatabaseURL] = ""
	var log bytes.Buffer
	if code := runServe(nil, &log, env(kv)); code != exitFailure {
		t.Fatalf("exit code: got %d want %d", code, exitFailure)
	}
	if !strings.Contains(log.String(), envDatabaseURL) {
		t.Fatalf("log does not name the missing variable: %q", log.String())
	}
}

// unreachableDB refuses connections immediately (port 1), so the
// migrate-failure branches resolve fast and deterministically.
const unreachableDB = "postgres://fishhawk:fishhawk@127.0.0.1:1/fishhawk_directory?sslmode=disable&connect_timeout=1"

// A config that parses but whose database is unreachable must still exit
// non-zero at the migrate step, never fall through to listening.
func TestRunServeExitsWhenMigrateFails(t *testing.T) {
	kv := validEnv()
	kv[envDatabaseURL] = unreachableDB
	var log bytes.Buffer
	if code := runServe(nil, &log, env(kv)); code != exitFailure {
		t.Fatalf("exit code: got %d want %d", code, exitFailure)
	}
	if !strings.Contains(log.String(), "migrate") {
		t.Fatalf("log does not name the failing step: %q", log.String())
	}
}

func TestRunMigrateReportsFailure(t *testing.T) {
	kv := validEnv()
	kv[envDatabaseURL] = unreachableDB
	for _, direction := range []string{"up", "down"} {
		t.Run(direction, func(t *testing.T) {
			var log bytes.Buffer
			if code := runMigrate([]string{direction}, &log, env(kv)); code != exitFailure {
				t.Fatalf("exit code: got %d want %d", code, exitFailure)
			}
			if !strings.Contains(log.String(), "migrate "+direction) {
				t.Fatalf("log: %q", log.String())
			}
		})
	}
}

func TestRunMigrateRequiresDatabaseURL(t *testing.T) {
	var log bytes.Buffer
	if code := runMigrate([]string{"up"}, &log, env(map[string]string{})); code != exitFailure {
		t.Fatalf("exit code: got %d want %d", code, exitFailure)
	}
	if !strings.Contains(log.String(), envDatabaseURL) {
		t.Fatalf("log: %q", log.String())
	}
}

func TestRunMigrateRejectsUnknownDirection(t *testing.T) {
	var log bytes.Buffer
	if code := runMigrate([]string{"sideways"}, &log, env(validEnv())); code != exitUsage {
		t.Fatalf("exit code: got %d want %d", code, exitUsage)
	}
	if !strings.Contains(log.String(), "sideways") {
		t.Fatalf("log: %q", log.String())
	}
}

func TestRunDispatch(t *testing.T) {
	cases := map[string]struct {
		args     []string
		wantCode int
		wantSub  string
	}{
		"help":            {[]string{"--help"}, exitOK, "Usage: fishhawk-directory"},
		"unknown command": {[]string{"frobnicate"}, exitUsage, "unknown subcommand"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var log bytes.Buffer
			if code := run(tc.args, &log, env(validEnv())); code != tc.wantCode {
				t.Fatalf("exit code: got %d want %d", code, tc.wantCode)
			}
			if !strings.Contains(log.String(), tc.wantSub) {
				t.Fatalf("log %q does not contain %q", log.String(), tc.wantSub)
			}
		})
	}
}

func TestSplitCommand(t *testing.T) {
	cases := map[string]struct {
		args     []string
		wantCmd  string
		wantRest int
	}{
		"empty":        {nil, "", 0},
		"leading flag": {[]string{"--addr", ":1"}, "", 2},
		"subcommand":   {[]string{"migrate", "up"}, "migrate", 1},
		"bare serve":   {[]string{"serve"}, "serve", 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cmd, rest := splitCommand(tc.args)
			if cmd != tc.wantCmd || len(rest) != tc.wantRest {
				t.Fatalf("got (%q, %v) want (%q, %d args)", cmd, rest, tc.wantCmd, tc.wantRest)
			}
		})
	}
}

func TestEnvOrFallsBack(t *testing.T) {
	getenv := env(map[string]string{"SET": "value", "BLANK": "   "})
	if got := envOr(getenv, "SET", "def"); got != "value" {
		t.Fatalf("got %q want value", got)
	}
	if got := envOr(getenv, "BLANK", "def"); got != "def" {
		t.Fatalf("blank should fall back: got %q", got)
	}
	if got := envOr(getenv, "MISSING", "def"); got != "def" {
		t.Fatalf("got %q want def", got)
	}
}
