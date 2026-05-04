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

// runValidate implements `fishhawk validate [path]`. Reads the
// file (default `.fishhawk/workflows.yaml`), validates it against
// the workflow-v0 schema, and prints either "OK" or one error
// line per leaf failure.
//
// Exit code 0 on success, 1 on validation failure (per the issue
// body — exit 2 is reserved for usage errors).
func runValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: fishhawk validate [path]")
		_, _ = fmt.Fprintln(stderr, "")
		_, _ = fmt.Fprintln(stderr, "Validates a Fishhawk workflow spec against the v0 JSON Schema.")
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

	_, _ = fmt.Fprintf(stdout, "%s: OK\n", path)
	return exitOK
}
