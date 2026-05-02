// Command fishhawk-verify checks the integrity of a Fishhawk audit
// log export — chain-hash and Ed25519 signatures — without trusting
// the backend that produced the export. Per ADR-008 (#72) the
// (run_id, public_key) pair plus a copy of the canonical hash
// algorithm is sufficient to recompute the entry chain offline.
//
// Usage:
//
//	fishhawk-verify --export <path>
//
// Exit codes:
//
//	0  every chain in the export verified
//	1  one or more issues found
//	2  usage error (missing flag, bad file, malformed JSON)
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/kuhlman-labs/fishhawk/verifier/internal/audit"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is split out so tests can drive it without exiting the test
// process.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk-verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	exportPath := fs.String("export", "", "path to a Fishhawk audit log export JSON file")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *exportPath == "" {
		_, _ = fmt.Fprintln(stderr, "fishhawk-verify: --export <path> is required")
		fs.Usage()
		return exitUsage
	}

	f, err := os.Open(*exportPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk-verify: open export: %v\n", err)
		return exitUsage
	}
	defer func() { _ = f.Close() }()

	ex, err := audit.ParseExport(f)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk-verify: %v\n", err)
		return exitUsage
	}

	res := audit.VerifyExport(ex)
	printResult(stdout, res)
	if res.OK() {
		return exitOK
	}
	return exitFailure
}

// printResult writes a human-readable summary to w. The format is
// stable enough that scripts can parse it (one issue per line,
// fields tab-separated).
func printResult(w io.Writer, res audit.Result) {
	if res.OK() {
		_, _ = fmt.Fprintf(w, "PASS — verified %d run(s), %d audit entries; no issues found.\n",
			res.RunsVerified, res.EntriesChecked)
		return
	}
	_, _ = fmt.Fprintf(w, "FAIL — verified %d run(s), %d audit entries; %d issue(s):\n",
		res.RunsVerified, res.EntriesChecked, len(res.Issues))
	for _, iss := range res.Issues {
		_, _ = fmt.Fprintf(w, "  run=%s\tseq=%d\tkind=%s\t%s\n",
			iss.RunID, iss.Sequence, iss.Kind, iss.Detail)
	}
}
