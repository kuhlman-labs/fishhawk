// Command fishhawk is the user-facing CLI for the Fishhawk
// control plane. Subcommands wrap docs/api/v0.openapi.yaml.
//
// Subcommands:
//
//	fishhawk run start    --repo R --workflow W --workflow-sha S [--trigger-ref REF]
//	fishhawk run status   <run-id> [--output text|json]
//	fishhawk run list     [--repo R] [--workflow W] [--state S] [--limit N]
//	fishhawk run cancel   <run-id>
//	fishhawk run open     <run-id>
//	fishhawk run retry    <stage-id> [--output text|json]
//	fishhawk plan approve <run-id> [--reason ...] [--output text|json]
//	fishhawk plan reject  <run-id> [--reason ...] [--output text|json]
//	fishhawk audit list   <run-id> [--category C] [--stage UUID] [--limit N] [--cursor X] [--output text|json]
//	fishhawk audit tail   <run-id> [--interval D] [--output text|json] [--max-polls N]
//
// Auth is the same `bearerToken` scheme defined in the OpenAPI:
// CLI sends `Authorization: Bearer <token>` from --token /
// FISHHAWK_TOKEN. Tokens are minted via the (forthcoming)
// /v0/tokens endpoint; until that lands the CLI works against a
// dev backend with auth stubbed (current state).
//
// `fishhawk validate` (E6.2 / #33) is intentionally absent from
// this PR: it requires a local copy of the workflow-spec parser,
// which currently lives under backend/internal/spec and can't be
// imported across modules. Tracked separately.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kuhlman-labs/fishhawk/cli/internal/version"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches to the appropriate subcommand. Split out of main
// so tests can drive it without exiting the test process.
func run(args []string, stdout, stderr io.Writer) int {
	cmd, rest := splitCommand(args)
	switch cmd {
	case "":
		printUsage(stderr)
		return exitUsage
	case "run":
		return runRun(rest, stdout, stderr)
	case "plan":
		return runPlan(rest, stdout, stderr)
	case "audit":
		return runAudit(rest, stdout, stderr)
	case "validate":
		return runValidate(rest, stdout, stderr)
	case "runner":
		return runRunner(rest, stdout, stderr)
	case "version", "--version":
		_, _ = fmt.Fprintln(stdout, version.Version)
		return exitOK
	case "-h", "--help", "help":
		printUsage(stdout)
		return exitOK
	default:
		_, _ = fmt.Fprintf(stderr, "fishhawk: unknown subcommand %q\n\n", cmd)
		printUsage(stderr)
		return exitUsage
	}
}

// splitCommand pulls the first positional arg as the subcommand.
// Anything starting with "-" is preserved as a flag for the
// implicit (currently empty) top-level command — no leading flags
// are accepted today, so that path returns usage.
func splitCommand(args []string) (cmd string, rest []string) {
	if len(args) == 0 {
		return "", nil
	}
	if strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return args[0], args[1:]
}

func printUsage(w io.Writer) {
	for _, line := range []string{
		"Usage: fishhawk <command> [args]",
		"",
		"Commands:",
		"  run start    Trigger a workflow run.",
		"  run status   Show a run's current state.",
		"  run list     List runs with optional filters.",
		"  run cancel   Cancel an in-flight run.",
		"  run open     Open a run's detail page in the browser.",
		"  run retry    Retry a failed stage (takes a stage id, not a run id).",
		"  plan approve Approve the plan stage on a run.",
		"  plan reject  Reject the plan stage on a run (category-D failure).",
		"  audit list   List audit entries for a run.",
		"  audit tail   Follow the audit log of a run in real time.",
		"  validate     Validate a workflow spec file locally.",
		"  runner start Spawn the fishhawk-runner locally against an already-minted run (Phase C of E22 / #389).",
		"  version      Print the CLI version and exit.",
		"  help         Show this help.",
		"",
		"Global flags (apply to every subcommand):",
		"  --backend-url URL   Fishhawk backend URL (default $FISHHAWK_BACKEND_URL or http://localhost:8080)",
		"  --token TOKEN       Bearer token (default $FISHHAWK_TOKEN, may be empty for dev backends)",
		"",
		"For per-command flags: fishhawk run start --help",
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
