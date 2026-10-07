// Command catchrategate is the OFFLINE plan-review catch-rate gate (E55.4 /
// #2245). It decides from the committed two-arm evidence record alone — no
// model call, no network — via agenteval.CheckCatchRateEvidence, which
// recomputes the verdict from the recorded counts and fails closed on an
// absent, malformed, stale, under-powered or regressed record.
//
// It lives under backend/internal/agenteval (not backend/cmd) on purpose: it
// is a gate over agenteval's own testdata, never a shipped binary, and
// scripts/check-review-prompt-eval runs it with `go run` from the backend
// module. Precedent for a standalone operator tool: backend/cmd/fishhawk-distill-corpus.
//
// Run from anywhere inside the checkout:
//
//	(cd backend && go run ./internal/agenteval/catchrategate)
//	(cd backend && go run ./internal/agenteval/catchrategate --print-fingerprint)
//
// Exit codes: 0 the evidence passes (report on stdout), 1 the gate fails
// (reason on stderr), 2 usage error.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/agenteval"
)

// backendModulePath identifies the backend module's go.mod.
const backendModulePath = "github.com/kuhlman-labs/fishhawk/backend"

// The committed paths, relative to the backend module root.
const (
	defaultEvidenceRel    = "internal/agenteval/testdata/planreview-catchrate/evidence.json"
	defaultCorpusRel      = "internal/agenteval/testdata/planreview-miss-corpus"
	defaultConventionsRel = "internal/agenteval/testdata/planreview-catchrate/representative-conventions.md"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("catchrategate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	evidence := fs.String("evidence", "", "evidence record (default: the committed "+defaultEvidenceRel+" under the backend module root)")
	corpus := fs.String("corpus", "", "plan-review-miss corpus dir (default: the committed "+defaultCorpusRel+")")
	conventions := fs.String("conventions", "", "representative conventions fixture (default: the committed "+defaultConventionsRel+")")
	printFingerprint := fs.Bool("print-fingerprint", false, "print the CURRENT prompt fingerprint and exit 0 (compare with the record's prompt_fingerprint to confirm staleness)")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, `catchrategate — the offline plan-review catch-rate gate (E55.4 / #2245).

Decides from the committed two-arm evidence record (no model call, no network).
Exit 0: the evidence passes. Exit 1: the gate fails (reason on stderr). Exit 2: usage.
Operator run-book: docs/compliance/planreview-catchrate-evidence.md

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "catchrategate: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	if *evidence == "" || *corpus == "" || *conventions == "" {
		cwd, err := os.Getwd()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "catchrategate: %v\n", err)
			return 1
		}
		root, err := findBackendRoot(cwd)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "catchrategate: %v\n", err)
			return 1
		}
		defaultTo(evidence, filepath.Join(root, defaultEvidenceRel))
		defaultTo(corpus, filepath.Join(root, defaultCorpusRel))
		defaultTo(conventions, filepath.Join(root, defaultConventionsRel))
	}
	model := agenteval.DefaultQualityGeneratorModel

	if *printFingerprint {
		cases, err := agenteval.LoadPlanReviewCatchCorpus(*corpus)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "catchrategate: %v\n", err)
			return 1
		}
		conv, err := agenteval.LoadRepresentativeConventions(*conventions)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "catchrategate: %v\n", err)
			return 1
		}
		fp, err := agenteval.CatchRatePromptFingerprint(cases, conv, model)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "catchrategate: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintln(stdout, fp)
		return 0
	}

	report, err := agenteval.CheckCatchRateEvidence(*evidence, *corpus, *conventions, model)
	if err != nil {
		if report != "" {
			_, _ = fmt.Fprintln(stderr, report)
		}
		_, _ = fmt.Fprintf(stderr, "catchrategate: FAIL: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, report)
	_, _ = fmt.Fprintln(stdout, "catchrategate: PASS")
	return 0
}

func defaultTo(p *string, v string) {
	if *p == "" {
		*p = v
	}
}

// findBackendRoot walks up from dir to the backend module root: a directory
// whose go.mod declares backendModulePath, or one with such a backend/
// child (the repo root).
func findBackendRoot(dir string) (string, error) {
	for d := dir; ; d = filepath.Dir(d) {
		for _, candidate := range []string{d, filepath.Join(d, "backend")} {
			if isBackendModule(filepath.Join(candidate, "go.mod")) {
				return candidate, nil
			}
		}
		if parent := filepath.Dir(d); parent == d {
			return "", fmt.Errorf("cannot locate the backend module root (a go.mod declaring %s) from %s: run inside the checkout or pass --evidence, --corpus and --conventions", backendModulePath, dir)
		}
	}
}

func isBackendModule(goMod string) bool {
	f, err := os.Open(goMod)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")) == backendModulePath
		}
	}
	return false
}
