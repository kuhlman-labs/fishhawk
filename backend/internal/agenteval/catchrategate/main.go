// Command catchrategate is the OFFLINE plan-review catch-rate gate (E55.4 /
// #2245; arm-keyed v2, E78.11 / #4313). It decides from the committed
// arm-keyed evidence record alone — no
// model call, no network — via agenteval.CheckCatchRateEvidence, which
// recomputes the verdict from the recorded counts and fails closed on an
// absent, malformed, stale, under-powered or regressed record, and on a
// record whose per-trial sidecar (evidence.trials.json beside it, derived by
// agenteval.CatchRateTrialsPath — no flag of its own) is absent, malformed,
// from another recording or count-inconsistent with it. On PASS it prints the
// sidecar path: a missed trial's concern notes, with their near-miss tags,
// are read there.
//
// The arm table (agenteval) reads --corpus and --conventions (the conventions
// arms), --persona-corpus (the architect persona arm, whose remit and
// decision-record index are read from the checkout at --repo-root) and
// --security-corpus (the security and security_portability persona arms,
// E80.17 / #4314, whose remit docs/review/security-remit.md and convention
// docs/review/security-conventions.md are read from --repo-root). A legacy v1
// record is refused naming the shared #4313/#4314 re-pin recording.
//
// --rescore is a DIAGNOSTIC, not a gate verdict (E55.22 / #4355): it
// re-classifies the sidecar derived from --evidence under the CURRENT arm
// table and catch rule via agenteval.RescoreCatchRateTrials, with no model
// call, and prints the report — per arm and case, recorded vs re-scored
// catches beside the attribution-only rate, a clean case's false alarms, and
// an arm the sidecar lacks as NOT RECORDED. The re-score is a per-arm LOWER BOUND (stored
// notes are truncated, concerns capped, and a caught trial keeps only its
// matching concern), it never writes evidence, and it never prints PASS, so a
// probe change can be evaluated with it before paying for a recording.
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
//	(cd backend && go run ./internal/agenteval/catchrategate --rescore)
//
// Exit codes: 0 the evidence passes (report on stdout), or --print-fingerprint
// / --rescore succeeded; 1 the gate fails (reason on stderr), or the re-score
// or fingerprint cannot be computed; 2 usage error (including --rescore with
// --print-fingerprint). Every outcome ends with the rule line
// (agenteval.CatchRateRule: the 0.10 tolerance, the 136-trial floor and its
// derivation) — on stdout for a pass, on stderr otherwise (so
// --print-fingerprint's stdout stays the bare digest and --rescore's stdout is
// never mistaken for a pass). The rule is per arm: the within-run rule for the
// conventions pair, then every arm's catch-rate drop and false-alarm-rate rise
// against the same arm of the pinned baseline.
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
	defaultPersonaRel     = "internal/agenteval/testdata/planreview-persona-corpus/architect"
	defaultSecurityRel    = "internal/agenteval/testdata/planreview-persona-corpus/security"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the gate and then prints the rule line, whatever the outcome:
// an absent, stale or malformed record and an infrastructure failure name the
// bar the record must clear just as a pass or a regression does.
func run(args []string, stdout, stderr io.Writer) int {
	code, passed := gate(args, stdout, stderr)
	ruleOut := stderr
	if passed {
		ruleOut = stdout
	}
	_, _ = fmt.Fprintln(ruleOut, "catchrategate: "+agenteval.CatchRateRule())
	_, _ = fmt.Fprintln(ruleOut, "catchrategate: "+agenteval.CatchRateArmRule())
	return code
}

// gate is the gate proper. passed is true only for an evidence PASS.
func gate(args []string, stdout, stderr io.Writer) (code int, passed bool) {
	fs := flag.NewFlagSet("catchrategate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	evidence := fs.String("evidence", "", "evidence record (default: the committed "+defaultEvidenceRel+" under the backend module root)")
	corpus := fs.String("corpus", "", "plan-review-miss corpus dir (default: the committed "+defaultCorpusRel+")")
	conventions := fs.String("conventions", "", "representative conventions fixture (default: the committed "+defaultConventionsRel+")")
	personaCorpus := fs.String("persona-corpus", "", "architect persona corpus dir (default: the committed "+defaultPersonaRel+")")
	securityCorpus := fs.String("security-corpus", "", "security persona corpus dir, markers.json at its root (default: the committed "+defaultSecurityRel+")")
	repoRoot := fs.String("repo-root", "", "checkout root the persona arms read docs/review/architect-remit.md, docs/adr/, docs/review/security-remit.md and docs/review/security-conventions.md from (default: the parent of the backend module root)")
	printFingerprint := fs.Bool("print-fingerprint", false, "print the CURRENT prompt fingerprint and exit 0 (compare with the record's prompt_fingerprint to confirm staleness)")
	rescore := fs.Bool("rescore", false, "re-score the trial sidecar derived from --evidence under the CURRENT --corpus and catch rule and print the report (a diagnostic per-arm LOWER BOUND, never a PASS; never writes evidence); exits 0")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, `catchrategate — the offline plan-review catch-rate gate (E55.4 / #2245).

Decides from the committed arm-keyed evidence record (no model call, no network)
and its per-trial sidecar beside it (<evidence stem>.trials.json, e.g.
evidence.trials.json; derived from --evidence, never a separate flag).
Exit 0: the evidence passes. Exit 1: the gate fails (reason on stderr). Exit 2: usage.

The arms: without_conventions and with_conventions (--corpus, --conventions),
architect (--persona-corpus), and security and security_portability
(--security-corpus; the security remit and convention), the persona documents
read under --repo-root. A legacy v1 record is refused: re-record every arm in
the shared #4313/#4314 re-pin.

--rescore is a diagnostic, not a gate verdict: it re-classifies the recorded
trial sidecar under the CURRENT arm table and catch rule with no model call and
prints recorded vs re-scored catches (a per-arm LOWER BOUND) beside the
attribution-only rate. It never writes evidence and never prints PASS: use it
to evaluate a probe change before paying for a recording. It cannot be
combined with --print-fingerprint.
Operator run-book: docs/compliance/planreview-catchrate-evidence.md

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2, false
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "catchrategate: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2, false
	}
	if *rescore && *printFingerprint {
		_, _ = fmt.Fprintln(stderr, "catchrategate: --rescore and --print-fingerprint are mutually exclusive")
		fs.Usage()
		return 2, false
	}
	if *evidence == "" || *corpus == "" || *conventions == "" || *personaCorpus == "" || *securityCorpus == "" || *repoRoot == "" {
		cwd, err := os.Getwd()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "catchrategate: %v\n", err)
			return 1, false
		}
		root, err := findBackendRoot(cwd)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "catchrategate: %v\n", err)
			return 1, false
		}
		defaultTo(evidence, filepath.Join(root, defaultEvidenceRel))
		defaultTo(corpus, filepath.Join(root, defaultCorpusRel))
		defaultTo(conventions, filepath.Join(root, defaultConventionsRel))
		defaultTo(personaCorpus, filepath.Join(root, defaultPersonaRel))
		defaultTo(securityCorpus, filepath.Join(root, defaultSecurityRel))
		defaultTo(repoRoot, filepath.Dir(root))
	}
	inputs := agenteval.CatchRateInputs{CorpusDir: *corpus, ConventionsPath: *conventions, PersonaCorpusDir: *personaCorpus,
		SecurityCorpusDir: *securityCorpus, RepoRoot: *repoRoot}
	model := agenteval.DefaultCatchRateGeneratorModel

	if *printFingerprint {
		fp, err := agenteval.CatchRatePromptFingerprint(inputs, model)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "catchrategate: %v\n", err)
			return 1, false
		}
		_, _ = fmt.Fprintln(stdout, fp)
		return 0, false
	}

	if *rescore {
		res, err := agenteval.RescoreCatchRateTrials(agenteval.CatchRateTrialsPath(*evidence), inputs)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "catchrategate: rescore: %v\n", err)
			return 1, false
		}
		// Never passed: the report is a diagnostic, so the rule line goes to
		// stderr and no PASS line is printed.
		_, _ = fmt.Fprint(stdout, res.Render())
		return 0, false
	}

	report, err := agenteval.CheckCatchRateEvidence(*evidence, inputs, model)
	if err != nil {
		if report != "" {
			_, _ = fmt.Fprintln(stderr, report)
		}
		_, _ = fmt.Fprintf(stderr, "catchrategate: FAIL: %v\n", err)
		return 1, false
	}
	_, _ = fmt.Fprintln(stdout, report)
	_, _ = fmt.Fprintln(stdout, "catchrategate: trial sidecar (per-trial concern notes, near-miss tags): "+agenteval.CatchRateTrialsPath(*evidence))
	_, _ = fmt.Fprintln(stdout, "catchrategate: PASS")
	return 0, true
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
			return "", fmt.Errorf("cannot locate the backend module root (a go.mod declaring %s) from %s: run inside the checkout or pass --evidence, --corpus, --conventions, --persona-corpus, --security-corpus and --repo-root", backendModulePath, dir)
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
