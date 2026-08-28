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

// runValidate implements `fishhawk validate [path]`. Reads the
// file (default `.fishhawk/workflows.yaml`), validates it against
// the version-routed workflow schema and the local semantic sweeps
// (including stage-reference resolution, E52.13 / #2323), and prints
// either an "OK" line plus the residual-scope note or one error line
// per leaf failure.
//
// Exit code 0 on success, 1 on validation failure (per the issue
// body — exit 2 is reserved for usage errors).
func runValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: fishhawk validate [path]")
		_, _ = fmt.Fprintln(stderr, "")
		_, _ = fmt.Fprintln(stderr, "Validates a Fishhawk workflow spec against the version-routed JSON Schema (v0/v1/v2),")
		_, _ = fmt.Fprintln(stderr, "then resolves stage references locally (duplicate ids, needs, inputs.from_stage).")
		_, _ = fmt.Fprintln(stderr, "For a workflow producing a grooming report it also enforces the mandatory-charter rule")
		_, _ = fmt.Fprintln(stderr, "against the sibling work-management.yaml (absent means the shipped default, which declares a charter).")
		_, _ = fmt.Fprintln(stderr, "The remaining stage-binding rules are checked server-side at run creation.")
		_, _ = fmt.Fprintln(stderr, "Defaults to .fishhawk/workflows.yaml when no path is supplied.")
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

	if err := spec.ValidateBytes(data); err != nil {
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
		return exitFailure
	}

	// The static charter check (E54.11 / #2801) runs only AFTER ValidateBytes
	// succeeds, so a schema-invalid spec reports its schema errors first and the
	// charter check never runs on a document the validator already rejected.
	requiring, cerr := spec.WorkflowsRequiringCharter(data)
	if cerr != nil {
		// Unreachable in practice — ValidateBytes already parsed the same bytes
		// through the shared decodeAndResolve prefix — but surface rather than
		// swallow.
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
