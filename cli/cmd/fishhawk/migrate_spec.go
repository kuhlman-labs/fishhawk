package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// runMigrateSpec implements `fishhawk migrate-spec [path] [--out PATH |
// --in-place | --report-only]` (E52.8 / #2220): the workflow-v1 ->
// workflow-v2 codemod.
//
// THE OUTPUT MATRIX, six cells, all settled here rather than by a silent
// precedence rule:
//
//	(1) no output flag        report to stdout, write NOTHING
//	(2) --report-only         the explicit spelling of (1)
//	(3) --out PATH            write there; REFUSE to clobber an existing PATH
//	(4) --in-place            rewrite the source file
//	(5) --out AND --in-place  usage error (exit 2), never a precedence rule
//	(6) --report-only with either output flag   usage error (exit 2)
//
// Cell (1) is the safe default for a command that rewrites a GOVERNANCE
// file: the operator reads the approval-eligibility diff first and opts
// into the write second. stdout is reserved for the report in every cell —
// the migrated YAML is never printed there, so piping the report
// somewhere can never accidentally produce a spec.
func runMigrateSpec(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk migrate-spec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "write the migrated spec to this path (refuses to overwrite an existing file)")
	inPlace := fs.Bool("in-place", false, "rewrite the source file with the migrated spec")
	reportOnly := fs.Bool("report-only", false, "print the report and write nothing (the default)")
	fs.Usage = func() { printMigrateSpecUsage(stderr) }
	// Go's flag package stops parsing at the first non-flag argument, so a
	// path-first invocation (`migrate-spec path --in-place`) would leave
	// the flags unparsed and silently take the report-only cell. Parse,
	// consume the positional, re-parse the tail.
	path := ""
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	for fs.NArg() > 0 {
		if path != "" {
			_, _ = fmt.Fprintln(stderr, "fishhawk migrate-spec: at most one path argument allowed")
			fs.Usage()
			return exitUsage
		}
		path = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return exitUsage
		}
	}
	if path == "" {
		path = defaultSpecPath
	}

	// Cells (5) and (6): contradictory flag combinations are usage
	// errors, not a precedence rule the operator has to remember.
	if *out != "" && *inPlace {
		_, _ = fmt.Fprintln(stderr, "fishhawk migrate-spec: --out and --in-place are mutually exclusive; pick where the migrated spec goes")
		return exitUsage
	}
	if *reportOnly && (*out != "" || *inPlace) {
		_, _ = fmt.Fprintln(stderr, "fishhawk migrate-spec: --report-only contradicts --out / --in-place; --report-only writes nothing")
		return exitUsage
	}

	src, err := os.ReadFile(path) //nolint:gosec // user-supplied path is the point
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk migrate-spec: %s: %v\n", path, err)
		return exitUsage
	}

	res, err := spec.MigrateBytes(src)
	if err != nil {
		var pe *spec.ParseError
		if errors.As(err, &pe) {
			_, _ = fmt.Fprintf(stderr, "%s: %s\n", path, pe.Msg)
		} else {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", path, err)
		}
		return exitFailure
	}

	// The report is the product. It goes to stdout on every path,
	// including the refusal and no-op ones.
	_, _ = fmt.Fprint(stdout, res.Report.Render())

	if res.Refused() {
		_, _ = fmt.Fprintf(stderr, "\nfishhawk migrate-spec: %s: refusing to migrate; nothing was written.\n", path)
		for _, r := range res.Refusals {
			_, _ = fmt.Fprintf(stderr, "  %s\n", r.String())
		}
		return exitFailure
	}
	if res.NoOp {
		_, _ = fmt.Fprintf(stdout, "\n%s: already at workflow-v2; nothing to migrate.\n", path)
		return exitOK
	}

	switch {
	case *out != "":
		// Cell (3): --in-place is NOT overloaded as permission to
		// overwrite, so a typo'd --out can never destroy a file.
		if _, statErr := os.Stat(*out); statErr == nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk migrate-spec: %s already exists; refusing to overwrite (it is byte-unchanged)\n", *out)
			return exitFailure
		} else if !errors.Is(statErr, os.ErrNotExist) {
			_, _ = fmt.Fprintf(stderr, "fishhawk migrate-spec: %s: %v\n", *out, statErr)
			return exitUsage
		}
		if err := os.WriteFile(*out, res.Migrated, 0o600); err != nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk migrate-spec: %s: %v\n", *out, err)
			return exitUsage
		}
		_, _ = fmt.Fprintf(stdout, "\n%s: migrated to workflow-v2, written to %s\n", path, *out)
	case *inPlace:
		// Cell (4).
		if err := os.WriteFile(path, res.Migrated, 0o600); err != nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk migrate-spec: %s: %v\n", path, err)
			return exitUsage
		}
		_, _ = fmt.Fprintf(stdout, "\n%s: migrated to workflow-v2 in place\n", path)
	default:
		// Cells (1) and (2): report only.
		_, _ = fmt.Fprintf(stdout, "\n%s: migrates cleanly to workflow-v2. Nothing was written — re-run with --in-place or --out PATH.\n", path)
	}
	return exitOK
}

func printMigrateSpecUsage(w io.Writer) {
	for _, line := range []string{
		"Usage: fishhawk migrate-spec [path] [--out PATH | --in-place | --report-only]",
		"",
		"Migrates a workflow-v1 spec to workflow-v2 and prints an approval-eligibility report.",
		"Defaults to .fishhawk/workflows.yaml when no path is supplied.",
		"Accepts version major 1 only: a major-0 source is refused, and an already-v2 spec is a no-op.",
		"",
		"Output matrix (stdout is reserved for the report in every cell):",
		"  (no flag)              print the report, write NOTHING (same as --report-only)",
		"  --report-only          print the report, write nothing",
		"  --out PATH             write the migrated spec to PATH; refuses to overwrite an existing PATH",
		"  --in-place             rewrite the source file",
		"  --out with --in-place  usage error",
		"  --report-only with --out or --in-place  usage error",
		"",
		"The codemod REFUSES rather than guessing: an approval predicate with no faithful",
		"workflow-v2 equivalent aborts the whole migration and writes nothing. limit_usd,",
		"min_permission, member_of and not: are never fabricated.",
		"",
		"Exit codes:",
		"  0  migrated, or already at workflow-v2",
		"  1  refusal, output-validation failure, or a refused overwrite",
		"  2  usage / I/O error",
	} {
		_, _ = fmt.Fprintln(w, line)
	}
}
