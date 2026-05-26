// Command fishhawkd is the Fishhawk backend control plane.
//
// Subcommands:
//
//	fishhawkd serve          start the HTTP server (default if no subcommand)
//	fishhawkd migrate up     apply pending DB migrations
//	fishhawkd migrate down   roll back the most recent migration (dev only)
//
// E3.2 (#42) wired the HTTP serve path. E3.3 (#43) added the run state
// machine, the Postgres pool, and the migrate subcommand.
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

// run dispatches to the appropriate subcommand. Split out of main so
// tests can drive it without exiting the test process.
func run(args []string, logSink io.Writer) int {
	cmd, rest := splitCommand(args)
	switch cmd {
	case "", "serve":
		return runServe(rest, logSink)
	case "migrate":
		return runMigrate(rest, logSink)
	case "audit-rehash":
		return runAuditRehash(rest, logSink)
	case "token":
		return runToken(rest, logSink)
	case "-h", "--help", "help":
		printUsage(logSink)
		return exitOK
	default:
		_, _ = fmt.Fprintf(logSink, "fishhawkd: unknown subcommand %q\n\n", cmd)
		printUsage(logSink)
		return exitUsage
	}
}

// splitCommand pulls the first positional arg as the subcommand name.
// Anything starting with "-" is treated as a flag for the implicit
// `serve` subcommand, preserving the bare "fishhawkd --addr=…" form.
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
		"Usage: fishhawkd [serve|migrate|token] [flags]",
		"",
		"Subcommands:",
		"  serve         Run the HTTP server (default).",
		"  migrate up    Apply pending DB migrations.",
		"  migrate down  Roll back the most recent migration (dev only).",
		"  audit-rehash  Rewrite audit_entries.entry_hash with the canonical algorithm (#302).",
		"  token issue   Mint a bootstrap API token for an identity.",
		"  token migrate Promote pre-#526 operator tokens to the current default scope set.",
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
