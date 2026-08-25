package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
	fs.Usage = func() { printMigrateSpecUsage(fs, stderr) }
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
		// overwrite, so a typo'd --out can never destroy a file. The
		// no-clobber refusal and the create are ONE atomic syscall
		// (O_CREATE|O_EXCL): a stat-then-write pair leaves a
		// time-of-check/time-of-use window in which a file or symlink
		// planted after the check is overwritten by the write. O_EXCL
		// closes that window and additionally refuses to follow a symlink
		// landed at the path.
		f, openErr := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if openErr != nil {
			if errors.Is(openErr, os.ErrExist) {
				_, _ = fmt.Fprintf(stderr, "fishhawk migrate-spec: %s already exists; refusing to overwrite (it is byte-unchanged)\n", *out)
				return exitFailure
			}
			_, _ = fmt.Fprintf(stderr, "fishhawk migrate-spec: %s: %v\n", *out, openErr)
			return exitUsage
		}
		// A write or close failure AFTER the O_EXCL create leaves a
		// truncated fragment at PATH. Remove it: because the no-clobber
		// refusal above keys off the file's mere existence, a leftover
		// fragment would turn a transient I/O error (a full disk mid-write)
		// into a persistent "already exists; refusing to overwrite" the
		// operator has to resolve by hand. This mirrors the temp-file
		// cleanup writeFileAtomic gives the --in-place cell; here the
		// O_EXCL create is kept so the atomic no-clobber guarantee stands,
		// and only the destination the command itself created is removed.
		if _, err := f.Write(res.Migrated); err != nil {
			_ = f.Close()
			_ = os.Remove(*out)
			_, _ = fmt.Fprintf(stderr, "fishhawk migrate-spec: %s: %v\n", *out, err)
			return exitUsage
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(*out)
			_, _ = fmt.Fprintf(stderr, "fishhawk migrate-spec: %s: %v\n", *out, err)
			return exitUsage
		}
		_, _ = fmt.Fprintf(stdout, "\n%s: migrated to workflow-v2, written to %s\n", path, *out)
	case *inPlace:
		// Cell (4). Rewrite via a same-directory temp file + atomic
		// rename, never an in-place truncate-then-write: a short write or
		// a close failure part-way through os.WriteFile would leave the
		// validated GOVERNANCE source truncated or half-replaced. The
		// rename either fully lands the migrated bytes or leaves the
		// original intact — the fail-closed behavior a governance rewrite
		// demands.
		if err := writeFileAtomic(path, res.Migrated); err != nil {
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

// writeFileAtomic writes data to path by creating a temp file in the same
// directory, writing and closing it, then renaming it over path. Any
// failure before the rename leaves path untouched and removes the temp, so
// an interrupted in-place rewrite never truncates or partially replaces the
// source. The original file's permission bits are preserved when it exists.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".migrate-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Remove the temp on every failure path before the rename lands it.
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if fi, statErr := os.Stat(path); statErr == nil {
		_ = tmp.Chmod(fi.Mode().Perm())
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
}

// printMigrateSpecUsage renders the hand-written usage prose and THEN the
// flag defaults via fs.PrintDefaults(), so migrate-spec's three real flags
// (out, in-place, report-only) appear in `migrate-spec -h`. Without the
// PrintDefaults() call the flags were invisible to any VisitAll-based
// enumeration — the vacuous-empty-harvest hole TestCustomUsageCommandsArePinned
// closes.
func printMigrateSpecUsage(fs *flag.FlagSet, w io.Writer) {
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
		"",
		"Flags:",
	} {
		_, _ = fmt.Fprintln(w, line)
	}
	fs.SetOutput(w)
	fs.PrintDefaults()
}
