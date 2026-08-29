// Command fishhawkd is the Fishhawk backend control plane.
//
// Subcommands:
//
//	fishhawkd serve                 start the HTTP server (default if no subcommand)
//	fishhawkd migrate up            apply pending DB migrations
//	fishhawkd migrate down          roll back the most recent migration (dev only)
//	fishhawkd account create|list   register / inventory tenancy accounts (GitLab authz gate)
//	fishhawkd installation register|list  register / inventory installations (GitLab authz gate)
//	fishhawkd member invite|list    invite / inventory account membership grants (first-user bootstrap)
//
// E3.2 (#42) wired the HTTP serve path. E3.3 (#43) added the run state
// machine, the Postgres pool, and the migrate subcommand.
package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
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
	case "account":
		return runAccount(rest, logSink)
	case "installation":
		return runInstallation(rest, logSink)
	case "member":
		return runMember(rest, logSink)
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
		"Usage: fishhawkd [serve|migrate|token|account|installation|member] [flags]",
		"",
		"Subcommands:",
		"  serve                  Run the HTTP server (default).",
		"  migrate up             Apply pending DB migrations.",
		"  migrate down           Roll back the most recent migration (dev only).",
		"  audit-rehash           Rewrite audit_entries.entry_hash with the canonical algorithm (#302).",
		"  token issue            Mint a bootstrap API token for an identity.",
		"  token migrate          Promote pre-#526 operator tokens to the current default scope set.",
		"  account create         Register a tenancy account (the GitLab run-creation authorization gate).",
		"  account list           Inventory registered tenancy accounts.",
		"  installation register  Register an installation under an account (the GitLab authorization gate).",
		"  installation list      Inventory registered installations with their owning account_key.",
		"  member invite          Invite a forge member into an account (the first-user bootstrap; writes an origin='invited' grant).",
		"  member list            Inventory membership grants with their origin and owning account.",
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

func envOrInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envOrDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envOrFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
