// Command fishhawk-directory is the Fishhawk directory plane (ADR-062,
// E44.7 / #1831): the globally shared control plane that answers "which
// region owns this account?" and routes the caller to that region's cell.
//
// Subcommands:
//
//	fishhawk-directory serve          start the HTTP server (default)
//	fishhawk-directory migrate up     apply pending directory migrations
//	fishhawk-directory migrate down   roll back the most recent migration (dev only)
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
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run dispatches to a subcommand. Split out of main so tests can drive it
// without exiting the test process.
func run(args []string, logSink io.Writer) int {
	cmd, rest := splitCommand(args)
	switch cmd {
	case "", "serve":
		return runServe(rest, logSink)
	case "migrate":
		return runMigrate(rest, logSink)
	case "-h", "--help", "help":
		printUsage(logSink)
		return exitOK
	default:
		_, _ = fmt.Fprintf(logSink, "fishhawk-directory: unknown subcommand %q\n\n", cmd)
		printUsage(logSink)
		return exitUsage
	}
}

// splitCommand pulls the first positional arg as the subcommand name.
// Anything starting with "-" is a flag for the implicit `serve`, preserving
// the bare `fishhawk-directory --addr=…` form.
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
		"  serve         Run the HTTP server (default).",
		"  migrate up    Apply pending directory migrations.",
		"  migrate down  Roll back the most recent migration (dev only).",
		"",
		"Environment:",
		"  FISHHAWK_DIRECTORY_DATABASE_URL  Postgres URL for the directory's own database (required).",
		"  FISHHAWK_DIRECTORY_REGIONS       region=url pairs, comma separated (required).",
		"  FISHHAWK_DIRECTORY_HANDOFF_SECRET  HMAC key shared with every cell (required).",
		"  FISHHAWK_DIRECTORY_ADMIN_TOKEN   Operator credential; unset refuses every request.",
		"  FISHHAWK_DIRECTORY_ROUTED_PATHS  Cell paths to route (default /v0/onboarding/start).",
		"  FISHHAWK_DIRECTORY_HANDOFF_TTL   Handoff validity window (default 5m).",
		"  FISHHAWK_DIRECTORY_ADDR          Listen address (default :8081).",
	} {
		_, _ = fmt.Fprintln(w, line)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
