// Command fishhawk-directory is the Fishhawk global directory: the tiny,
// customer-data-free control-plane service that maps an account to its
// home region and 302s login / App-install callbacks into that region's
// cell (ADR-062, E44.7 / #1831).
//
// Subcommands:
//
//	fishhawk-directory serve          start the HTTP server (default)
//	fishhawk-directory migrate up     apply pending directory migrations
//	fishhawk-directory migrate down   roll back one migration (dev only)
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr, os.Getenv))
}

// run dispatches to a subcommand. Split out of main so tests can drive
// it without exiting the test process, and parameterized on getenv so
// tests never mutate process state.
func run(args []string, logSink io.Writer, getenv func(string) string) int {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			printUsage(logSink)
			return exitOK
		}
	}
	cmd, rest := splitCommand(args)
	switch cmd {
	case "", "serve":
		return runServe(rest, logSink, getenv)
	case "migrate":
		return runMigrate(rest, logSink, getenv)
	default:
		_, _ = fmt.Fprintf(logSink, "fishhawk-directory: unknown subcommand %q\n\n", cmd)
		printUsage(logSink)
		return exitUsage
	}
}

// splitCommand pulls the first positional arg as the subcommand name.
// Anything starting with "-" is a flag for the implicit `serve`.
func splitCommand(args []string) (cmd string, rest []string) {
	if len(args) == 0 {
		return "", nil
	}
	if strings.HasPrefix(args[0], "-") {
		return "", args
	}
	return args[0], args[1:]
}

func printUsage(w io.Writer) {
	for _, line := range []string{
		"Usage: fishhawk-directory [serve|migrate] [flags]",
		"",
		"Subcommands:",
		"  serve         Run the routing HTTP server (default).",
		"  migrate up    Apply pending directory migrations.",
		"  migrate down  Roll back the most recent migration (dev only).",
		"",
		"Environment:",
		"  FISHHAWK_DIRECTORY_DATABASE_URL      Postgres URL for the directory database (required).",
		"  FISHHAWK_DIRECTORY_SUPPORTED_REGIONS Comma-separated region list, e.g. \"us,eu,au\" (required).",
		"  FISHHAWK_DIRECTORY_CELL_BASE_URLS    Comma-separated region=url pairs (required).",
		"  FISHHAWK_DIRECTORY_HANDOFF_SECRET    HMAC secret shared with every cell (required).",
		"  FISHHAWK_DIRECTORY_HANDOFF_TTL       Region-pin lifetime (default 2m).",
		"  FISHHAWK_DIRECTORY_ADDR              Listen address (default :8090).",
	} {
		_, _ = fmt.Fprintln(w, line)
	}
}
