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
//	fishhawk token login  [--provider github] [--client-id ID]
//	fishhawk token list

//	fishhawk deploy status   <run-id> [--output text|json]
//	fishhawk deploy approve  <run-id> [--reason ...] [--output text|json]
//	fishhawk deploy reject   <run-id> [--reason ...] [--output text|json]
//	fishhawk deploy rollback <run-id> [--output text|json]
//	fishhawk release preview --repo R --from REF --to REF [--output text|json]
//	fishhawk release prepare --repo R --from REF --to REF --stage-id UUID [--output text|json]
//	fishhawk release cut     --repo R --run-id UUID --artifact-id UUID --version V [--stage-id UUID] [--bump-level L] [--output text|json]
//	fishhawk release publish --repo R --tag T --run-id UUID --artifact-id UUID [--stage-id UUID] [--output text|json]
//	fishhawk campaign start  --repo R --epic E [--pause-policy P] [--output text|json]
//	fishhawk campaign status <campaign-id> [--output text|json]
//	fishhawk campaign list   [--repo R] [--state S] [--limit N] [--cursor X]
//	fishhawk campaign resume <campaign-id> [--output text|json]
//	fishhawk audit list   <run-id> [--category C] [--stage UUID] [--limit N] [--cursor X] [--output text|json]
//	fishhawk audit tail   <run-id> [--interval D] [--output text|json] [--max-polls N]
//	fishhawk init         [--preset low|medium|high] [--working-dir D] [--budget-usd N] [--single-reviewer] [--human-gates ids] [--force] [--repo R]
//	fishhawk file-issue   --repo R --type T --summary S [--body B] [--label L]... [--parent-epic E] [--run-id ID] [--output text|json]
//	fishhawk diagnose     <run-id> [--output text|json]
//	fishhawk report-issue <run-id> [--kind bug|feature] [--description T] [--include-free-text] [--output text|json]
//	fishhawk export       [--from RFC3339] [--to RFC3339] [--repo owner/name] [--run UUID]... [--limit N] [--csv] [--out PATH]
//	fishhawk migrate-spec [path] [--out PATH | --in-place | --report-only]
//
// Auth is the same `bearerToken` scheme defined in the OpenAPI:
// CLI sends `Authorization: Bearer <token>` from --token /
// FISHHAWK_TOKEN. A user-bound token can be minted with `fishhawk
// token login` (OAuth device flow), which stores it in the local
// credential store; subcommands then fall back to that stored token
// when --token / FISHHAWK_TOKEN is empty. An explicit flag/env token
// always wins over the stored credential.
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

	"github.com/kuhlman-labs/fishhawk/cli/internal/cmdinfo"
	"github.com/kuhlman-labs/fishhawk/cli/internal/version"
)

// dispatch maps a top-level command name to its runner. It is the SHARED
// production dispatch table: run() dispatches through it and
// TestInventoryCoversDispatch cross-checks its keys against the cmdinfo
// inventory, so a command reachable in the binary but absent from the
// docs (or vice versa) fails in test rather than silently. The
// non-dispatch cases (version, help, unknown, empty) stay in run().
var dispatch = map[string]func([]string, io.Writer, io.Writer) int{
	"run":          runRun,
	"plan":         runPlan,
	"token":        runToken,
	"deploy":       runDeploy,
	"release":      runRelease,
	"campaign":     runCampaign,
	"audit":        runAudit,
	"init":         runInit,
	"validate":     runValidate,
	"migrate-spec": runMigrateSpec,
	"runner":       runRunner,
	"doctor":       runDoctor,
	"file-issue":   runFileIssue,
	"diagnose":     runDiagnose,
	"report-issue": runReportIssue,
	"export":       runExport,
}

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
	if fn, ok := dispatch[cmd]; ok {
		return fn(rest, stdout, stderr)
	}
	switch cmd {
	case "":
		printUsage(stderr)
		return exitUsage
	case "version", "--version":
		if version.GitSHA != "unknown" {
			_, _ = fmt.Fprintf(stdout, "%s (%s)\n", version.Version, version.GitSHA)
		} else {
			_, _ = fmt.Fprintln(stdout, version.Version)
		}
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

// printUsage renders the top-level command listing from the shared
// cli/internal/cmdinfo inventory — the same inventory the generated CLI
// reference page renders from — so the binary's help and the docs cannot
// list a different set of commands.
func printUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Usage: fishhawk <command> [args]")
	_, _ = fmt.Fprintln(w, "")
	_, _ = fmt.Fprintln(w, "Commands:")
	width := 0
	for _, c := range cmdinfo.Commands() {
		if len(c.Key) > width {
			width = len(c.Key)
		}
	}
	for _, c := range cmdinfo.Commands() {
		_, _ = fmt.Fprintf(w, "  %-*s  %s\n", width, c.Key, c.Synopsis)
	}
	_, _ = fmt.Fprintf(w, "  %-*s  %s\n", width, "version", "Print the CLI version and exit.")
	_, _ = fmt.Fprintf(w, "  %-*s  %s\n", width, "help", "Show this help.")
	for _, line := range []string{
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
