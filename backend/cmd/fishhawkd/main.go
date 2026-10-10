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
//	fishhawkd oauth client register|list|remove  pre-register / inventory / remove OAuth clients (#2438)
//	fishhawkd decision-index backfill|check  rebuild / gap-check the derived decision index (#3730)
//	fishhawkd precedent-tuning --repo R      replay the divergence threshold over recorded decisions (#3733)
//	fishhawkd approver-members --spec F      dry-run approvals.members against recorded approvers (#4116)
//	fishhawkd sweep-stale-runs [--apply]     dry-run / reconcile stale non-terminal top-level runs (#4185)
//	fishhawkd version | --version    print "<Version> (<GitSHA>)" and exit (#4117)
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

	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// versionOut is where `version` / `--version` print. A package-level seam so
// tests can capture it without threading a stdout through every run call.
var versionOut io.Writer = os.Stdout

// run dispatches to the appropriate subcommand. Split out of main so
// tests can drive it without exiting the test process.
func run(args []string, logSink io.Writer) int {
	// `version` / `--version` are handled BEFORE splitCommand, which routes
	// any "-"-prefixed first arg to the implicit serve (#4117). The output
	// shape matches `fishhawk version`: "<Version> (<GitSHA>)".
	if len(args) > 0 && (args[0] == "version" || args[0] == "--version") {
		_, _ = fmt.Fprintln(versionOut, version.String())
		return exitOK
	}
	cmd, rest := splitCommand(args)
	switch cmd {
	case "", "serve":
		return runServe(rest, logSink)
	case "migrate":
		return runMigrate(rest, logSink)
	case "audit-rehash":
		return runAuditRehash(rest, logSink)
	case "decision-index":
		return runDecisionIndex(rest, logSink)
	case "precedent-tuning":
		return runPrecedentTuning(rest, logSink)
	case "approver-members":
		return runApproverMembers(rest, logSink)
	case "reconcile-orphan-children":
		return runReconcileOrphanChildren(rest, logSink)
	case "sweep-stale-runs":
		return runSweepStaleRuns(rest, logSink)
	case "token":
		return runToken(rest, logSink)
	case "account":
		return runAccount(rest, logSink)
	case "installation":
		return runInstallation(rest, logSink)
	case "member":
		return runMember(rest, logSink)
	case "oauth":
		return runOAuth(rest, logSink)
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
		"Usage: fishhawkd [serve|migrate|token|account|installation|member|oauth|decision-index|precedent-tuning|approver-members|reconcile-orphan-children|sweep-stale-runs|version] [flags]",
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
		"  oauth client register  Pre-register an OAuth client (the operator write path for oauth_clients; #2438).",
		"  oauth client list      Inventory pre-registered OAuth clients.",
		"  oauth client remove    Remove a pre-registered OAuth client by client_id.",
		"  decision-index backfill  Reconstruct the derived decision index from the audit chain (--rebuild, --dry-run; #3730).",
		"  decision-index check     Report decision-bearing entries with no index row; exits 1 on a gap.",
		"  precedent-tuning         Replay the divergence threshold over a repo's decision history for a grid of (N, X, window) candidates (#3733).",
		"  approver-members         Dry-run approvals.members: evaluate recorded approvers against a spec's members gates; exits 1 if any human would be refused (#4116).",
		"  reconcile-orphan-children  Dry-run (default) or --apply: cancel non-terminal decomposition children of cancelled/succeeded parents (#4186).",
		"  sweep-stale-runs           Dry-run (default) or --apply: reconcile stale (--days, default 14) non-terminal top-level runs (#4185).",
		"  version | --version      Print the build version and git SHA, \"<Version> (<GitSHA>)\", and exit.",
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
