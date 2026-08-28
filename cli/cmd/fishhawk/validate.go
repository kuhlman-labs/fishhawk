package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// defaultSpecPath is the canonical location of the workflow spec
// in a customer's repo. The validate subcommand defaults to it
// when no path argument is supplied so `fishhawk validate` from
// the repo root just works.
const defaultSpecPath = ".fishhawk/workflows.yaml"

// validateResidualLine names, on a successful validate, exactly what the CLI
// checked locally and what it deliberately leaves to the backend (E52.13 /
// #2323). It is a package-level constant so the test asserts the shipped string
// rather than a re-typed copy. Printed as a second stdout line after `<path>:
// OK`, whose bytes are kept unchanged so any consumer parsing the first line is
// unaffected.
const validateResidualLine = "checked locally: schema, reuse resolution, stage-reference resolution (duplicate ids, needs, inputs.from_stage), and — for a workflow producing a grooming report — the mandatory-charter rule against the sibling .fishhawk/work-management.yaml (its absence means the shipped default, which declares a charter). The remaining stage-binding rules (type/executor/constraint, produces-artifact, plan schema, max_autonomy) are checked server-side at run creation, which remains the load-bearing charter enforcement point."

// emitResolvedLossWarning names, on the --emit-resolved path, exactly what the
// re-marshalled document loses relative to the source (E52.22 / #2351). It is
// written to STDERR so stdout carries the resolved YAML and nothing else, and
// it is a package-level constant so the test asserts the shipped string rather
// than a re-typed copy.
const emitResolvedLossWarning = "fishhawk validate --emit-resolved: emitting the RESOLVED document; comments, key order and anchors are NOT preserved, so this is a machine-readable form only — never write it back over the source. This mode resolves reuse WITHOUT validating: exit 0 means 'resolvable', not 'valid'."

// runValidate implements `fishhawk validate [--emit-resolved] [path]`. Reads
// the file (default `.fishhawk/workflows.yaml`), validates it against
// the version-routed workflow schema and the local semantic sweeps
// (including stage-reference resolution, E52.13 / #2323), and prints
// either an "OK" line plus the residual-scope note or one error line
// per leaf failure.
//
// Under --emit-resolved it instead resolves workflow-v2 same-document reuse
// and writes the RESOLVED document to stdout and nothing else, so it pipes
// into a bare `check-jsonschema --schemafile docs/spec/workflow-v2.schema.json -`
// run (E52.22 / #2351). That mode deliberately runs NEITHER spec.ValidateBytes
// NOR the E54.11 static charter check — the point is to hand the document to a
// validator that judges validity itself. A failed or short stdout write on that
// path exits 2 (the I/O class) rather than reporting success, so a truncated
// document is never emitted under exit 0.
//
// Exit code 0 on success, 1 on validation failure (per the issue
// body — exit 2 is reserved for usage errors).
func runValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	emitResolved := fs.Bool("emit-resolved", false,
		"resolve workflow-v2 same-document reuse and write the resolved document to stdout (no validation)")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: fishhawk validate [--emit-resolved] [path]")
		_, _ = fmt.Fprintln(stderr, "")
		_, _ = fmt.Fprintln(stderr, "Validates a Fishhawk workflow spec against the version-routed JSON Schema (v0/v1/v2),")
		_, _ = fmt.Fprintln(stderr, "then resolves stage references locally (duplicate ids, needs, inputs.from_stage).")
		_, _ = fmt.Fprintln(stderr, "For a workflow producing a grooming report it also enforces the mandatory-charter rule")
		_, _ = fmt.Fprintln(stderr, "against the sibling work-management.yaml (absent means the shipped default, which declares a charter).")
		_, _ = fmt.Fprintln(stderr, "The remaining stage-binding rules are checked server-side at run creation.")
		_, _ = fmt.Fprintln(stderr, "Defaults to .fishhawk/workflows.yaml when no path is supplied.")
		_, _ = fmt.Fprintln(stderr, "")
		_, _ = fmt.Fprintln(stderr, "Flags:")
		// PrintDefaults renders the LIVE registered flag set. It is what
		// TestCLIFlagsMatchExecutableSurface harvests from `-h`, so a custom
		// Usage banner that hand-wrote its flag names instead would leave the
		// binary's real surface invisible to that coupling test.
		fs.PrintDefaults()
		_, _ = fmt.Fprintln(stderr, "        Resolves workflow-v2 same-document reuse (`defaults` / `extends`) and writes the")
		_, _ = fmt.Fprintln(stderr, "        RESOLVED document to stdout and nothing else, so it pipes into a schema validator:")
		_, _ = fmt.Fprintln(stderr, "          fishhawk validate --emit-resolved .fishhawk/workflows.yaml |")
		_, _ = fmt.Fprintln(stderr, "            check-jsonschema --schemafile docs/spec/workflow-v2.schema.json --default-filetype yaml -")
		_, _ = fmt.Fprintln(stderr, "        (--default-filetype yaml is required: check-jsonschema assumes JSON on stdin.)")
		_, _ = fmt.Fprintln(stderr, "        It does NOT validate: it resolves only, so exit 0 means 'resolvable', not 'valid'.")
		_, _ = fmt.Fprintln(stderr, "        Comments, key order and anchors are NOT preserved, so the output is a")
		_, _ = fmt.Fprintln(stderr, "        machine-readable form and must never be written back over the source.")
		_, _ = fmt.Fprintln(stderr, "")
		_, _ = fmt.Fprintln(stderr, "Exit codes:")
		_, _ = fmt.Fprintln(stderr, "  0  spec is valid")
		_, _ = fmt.Fprintln(stderr, "  1  spec has validation errors (printed to stderr)")
		_, _ = fmt.Fprintln(stderr, "  2  usage / I/O error")
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	path := defaultSpecPath
	switch fs.NArg() {
	case 0:
		// default
	case 1:
		path = fs.Arg(0)
	default:
		_, _ = fmt.Fprintln(stderr, "fishhawk validate: at most one path argument allowed")
		fs.Usage()
		return exitUsage
	}

	data, err := os.ReadFile(path) //nolint:gosec // user-supplied path is the point
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk validate: %s: %v\n", path, err)
		return exitUsage
	}

	if *emitResolved {
		// Emit-only mode. Placed BEFORE spec.ValidateBytes so the path never
		// reaches the validator or the charter check: the operator is piping
		// the document to a validator that judges validity itself.
		resolved, rerr := spec.ResolveReuse(data)
		if rerr != nil {
			printSpecError(stderr, path, rerr)
			return exitFailure
		}
		// ORDERING IS LOAD-BEARING: the loss warning goes to stderr first, and
		// the resolved bytes are the LAST write and the ONLY stdout write on
		// this path, so a failure can never leave a truncated half-document on
		// the pipe.
		_, _ = fmt.Fprintln(stderr, emitResolvedLossWarning)
		// A stdout write failure must NOT be reported as success: a partial
		// write (a redirect to a full disk, a downstream consumer that closed
		// early) would otherwise hand the consumer a truncated document under
		// exit 0. Both halves are checked because `n < len(p)` with a nil error
		// violates the io.Writer contract but costs one comparison to refuse.
		if n, werr := stdout.Write(resolved); werr != nil || n != len(resolved) {
			if werr == nil {
				werr = io.ErrShortWrite
			}
			_, _ = fmt.Fprintf(stderr, "fishhawk validate: %s: writing the resolved document to stdout: %v\n", path, werr)
			// exitUsage, not exitFailure: this is the I/O class the usage
			// banner's exit-code table already assigns 2 (the same code the
			// unreadable-input branch above returns), not a spec defect.
			return exitUsage
		}
		return exitOK
	}

	if err := spec.ValidateBytes(data); err != nil {
		printSpecError(stderr, path, err)
		return exitFailure
	}

	// The static charter check (E54.11 / #2801) runs only AFTER ValidateBytes
	// succeeds, so a schema-invalid spec reports its schema errors first and the
	// charter check never runs on a document the validator already rejected.
	// DELIBERATELY UNTESTED (#2996): ValidateBytes above already parsed these
	// same bytes through the shared decodeAndResolve prefix, so reaching the
	// branch below requires an input that fails WorkflowsRequiringCharter but
	// not ValidateBytes — evidence the two calls had DIVERGED, not evidence
	// the branch is exercised. A test manufactured by bypassing the first
	// call would prove nothing about the shipped path, so this stays a
	// surface-rather-than-swallow guard with no corresponding test. (Kept
	// ABOVE the `if`, not inside it: go tool cover attributes the whole
	// if-body's line span to its block's hit count, so a comment inside a
	// permanently-zero-coverage block gets flagged as an uncovered "new line"
	// by the patch-coverage gate on any future reword of this note.)
	requiring, cerr := spec.WorkflowsRequiringCharter(data)
	if cerr != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", path, cerr)
		return exitFailure
	}
	if len(requiring) == 0 {
		// AC3: a spec that declares no grooming workflow takes the OK path
		// completely unchanged — the sibling conventions file is never read.
		// STRUCTURAL early return: deleting it routes non-grooming specs through
		// the charter check below, which the non-grooming test then reddens.
		return printValidateOK(stdout, path)
	}

	// One or more workflows produce a grooming report: the mandatory-charter
	// rule applies. Read the sibling conventions ONCE and decide via the shared
	// reason core, then print one refusal line per requiring workflow.
	declared, charterPath, loadErr := loadCharterDeclaration(path)
	if reason := spec.CharterAdmissionReason(declared, charterPath, loadErr); reason != "" {
		for _, workflowID := range requiring {
			// The CLI has no repo identity, so the spec path is the message's
			// location subject (see cli/README.md and the plan's risks).
			_, _ = fmt.Fprintf(stderr, "%s: %s\n", path, spec.CharterRefusalMessage(path, workflowID, reason))
		}
		return exitFailure
	}

	return printValidateOK(stdout, path)
}

// printValidateOK prints the unchanged success output — the `<path>: OK` line
// (whose bytes any consumer parsing the first line depends on) plus the
// residual-scope note — and returns exitOK. Factored so the two success return
// sites emit byte-identical output.
func printValidateOK(stdout io.Writer, path string) int {
	_, _ = fmt.Fprintf(stdout, "%s: OK\n", path)
	_, _ = fmt.Fprintln(stdout, validateResidualLine)
	return exitOK
}

// printSpecError renders a spec error as one stderr line per leaf failure,
// prefixed by the validated path. Shared by the emit-resolved path and the
// ValidateBytes path so both emit byte-identical error lines.
func printSpecError(stderr io.Writer, path string, err error) {
	var pe *spec.ParseError
	var ve *spec.ValidationError
	switch {
	case errors.As(err, &pe):
		_, _ = fmt.Fprintf(stderr, "%s: %s\n", path, pe.Msg)
	case errors.As(err, &ve):
		for _, ent := range ve.Errors {
			_, _ = fmt.Fprintf(stderr, "%s%s: %s\n", path, ent.Path, ent.Message)
		}
	default:
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", path, err)
	}
}
