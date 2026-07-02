// Command fishhawk-distill-corpus scaffolds an agent-eval corpus case
// (trace.jsonl + expected.json + case.md) from a stored trace bundle,
// automating the mechanical half of the #819 corpus buildout.
//
// It is a standalone dev/operator tool, deliberately NOT a fishhawkd
// subcommand, so this scaffolding code stays out of the production server
// binary. The trace bundle source is, in precedence order: --stage-id
// (fetched from a running backend over GET /v0/stages/{id}/trace), --in
// <path>, or stdin.
//
// Run from the repo root (so the default --out-dir resolves) or pass
// --out-dir explicitly:
//
//	fishhawk-distill-corpus --stage-id <uuid> --case-name my-case --issue '#819'
//	fishhawk-distill-corpus --in bundle.jsonl.gz --case-name my-case --issue '#819'
//	gunzip -c bundle.jsonl.gz | fishhawk-distill-corpus --case-name my-case --issue '#819'
//
// The --plan-review-miss mode (E31.11 / #1539) scaffolds plan-review-miss
// corpus cases (miss.json + case.md) from a run's class-3
// acceptance_triage_decided audit entries instead:
//
//	fishhawk-distill-corpus --plan-review-miss --run-id <uuid> --case-name my-miss --issue '#1539'
//	fishhawk-distill-corpus --plan-review-miss --in items.json --case-name my-miss --issue '#1539'
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kuhlman-labs/fishhawk/backend/internal/corpusdistill"
)

// defaultCorpusRel is the corpus parent dir relative to the repo root.
// corpusParentRel is its parent, whose presence the default-OutDir
// resolution requires (fail-loud when run from the wrong cwd).
// defaultMissCorpusRel is the --plan-review-miss mode's default corpus dir,
// sharing the same parent check.
const (
	defaultCorpusRel     = "backend/internal/agenteval/testdata/corpus"
	corpusParentRel      = "backend/internal/agenteval/testdata"
	defaultMissCorpusRel = "backend/internal/agenteval/testdata/planreview-miss-corpus"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk-distill-corpus", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, `fishhawk-distill-corpus — scaffold an agent-eval corpus case from a trace bundle (#1290).

Reads a trace bundle (gzipped .jsonl.gz OR plain .jsonl), scores it with the
deterministic Tier-A scorer, and writes <out-dir>/<case-name>/{trace.jsonl,
expected.json,case.md}. Case selection + the distilled-signal narrative stay
operator curation (#819); this produces a replay-valid scaffold.

Bundle source precedence: --stage-id > --in > stdin.

Default --out-dir is %s, resolved relative to the current directory. If its
parent (%s) is absent from the cwd, the command fails loud — run from the repo
root or pass --out-dir <dir>.

Inline labeling (#1291): --signal and --narrative pre-fill the case.md
distilled-signal sections so the operator can add + label in one shot; omit
them to keep the #1290 TODO(operator) prompts. --dry-run scores the bundle and
prints the would-be case (dir, outcome, expected.json, case.md) WITHOUT writing
any files, so the operator can evaluate-before-keep.

Plan-review-miss mode (E31.11 / #1539): --plan-review-miss switches the tool to
scaffold plan-review-miss corpus cases (miss.json + case.md) from class-3
acceptance_triage_decided audit entries — one case per class-3 decision. The
item source is --run-id (fetched from --backend-url, all pages) or --in/stdin
(a JSON items array, or the {items:[...]} audit-endpoint envelope). Default
--out-dir for this mode is %s. Only structured verdict fields cross — evidence
blobs stay customer-side per ADR-049 #5. The tool scaffolds a CANDIDATE:
selection, labeling, and committing stay operator curation (#819 / ADR-040).

Flags:
`, defaultCorpusRel, corpusParentRel, defaultMissCorpusRel)
		fs.PrintDefaults()
	}

	var (
		in             = fs.String("in", "", "path to a trace bundle (.jsonl.gz or .jsonl), or audit items JSON with --plan-review-miss; '-' or empty reads stdin")
		stageID        = fs.String("stage-id", "", "fetch the bundle for this stage id from --backend-url (takes precedence over --in/stdin)")
		caseName       = fs.String("case-name", "", "corpus case slug (required); becomes the case directory name")
		issue          = fs.String("issue", "", "originating issue/run reference recorded in case.md (required), e.g. '#819'")
		outDir         = fs.String("out-dir", "", "corpus parent directory (default: "+defaultCorpusRel+", or "+defaultMissCorpusRel+" with --plan-review-miss, relative to cwd)")
		force          = fs.Bool("force", false, "overwrite an existing case directory")
		signal         = fs.String("signal", "", "optional scorecard signal/classification this case demonstrates (pre-fills case.md; else a TODO prompt)")
		narrative      = fs.String("narrative", "", "optional distilled-signal narrative for case.md (pre-fills the section; else a TODO prompt)")
		dryRun         = fs.Bool("dry-run", false, "print the would-be case(s) WITHOUT writing any files")
		backendURL     = fs.String("backend-url", envOr("FISHHAWK_BACKEND_URL", "http://localhost:8080"), "backend base URL for --stage-id/--run-id fetch (env FISHHAWK_BACKEND_URL)")
		token          = fs.String("token", os.Getenv("FISHHAWK_TOKEN"), "bearer API token for --stage-id/--run-id fetch (env FISHHAWK_TOKEN)")
		planReviewMiss = fs.Bool("plan-review-miss", false, "scaffold plan-review-miss corpus cases from class-3 acceptance_triage_decided audit entries (E31.11 / #1539)")
		runID          = fs.String("run-id", "", "with --plan-review-miss: fetch the run's acceptance_triage_decided audit entries from --backend-url (takes precedence over --in/stdin)")
	)

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *planReviewMiss && *stageID != "" {
		_, _ = fmt.Fprintln(stderr, "error: --stage-id is the trace-bundle source and does not apply to --plan-review-miss; use --run-id (or --in/stdin) instead")
		return 2
	}
	if !*planReviewMiss && *runID != "" {
		_, _ = fmt.Fprintln(stderr, "error: --run-id only applies to --plan-review-miss (the trace mode's fetch source is --stage-id)")
		return 2
	}
	if *caseName == "" {
		_, _ = fmt.Fprintln(stderr, "error: --case-name is required")
		return 2
	}
	if *issue == "" {
		_, _ = fmt.Fprintln(stderr, "error: --issue is required")
		return 2
	}

	defaultRel := defaultCorpusRel
	if *planReviewMiss {
		defaultRel = defaultMissCorpusRel
	}
	resolvedOut, err := resolveOutDir(*outDir, defaultRel)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	if *planReviewMiss {
		return runPlanReviewMiss(missRunParams{
			runID: *runID, in: *in, backendURL: *backendURL, token: *token,
			opts: corpusdistill.MissOptions{
				CaseName:  *caseName,
				Issue:     *issue,
				OutDir:    resolvedOut,
				Force:     *force,
				Fetched:   *runID != "",
				Narrative: *narrative,
			},
			dryRun: *dryRun,
		}, stdout, stderr)
	}

	src, err := openSource(context.Background(), *stageID, *in, *backendURL, *token, os.Stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	opts := corpusdistill.Options{
		CaseName: *caseName,
		Issue:    *issue,
		OutDir:   resolvedOut,
		Force:    *force,
		// Only the --stage-id fetch path GETs the redacted-only trace
		// endpoint, so only it can claim PRODUCTION+REDACTED provenance.
		Fetched:   *stageID != "",
		Signal:    *signal,
		Narrative: *narrative,
	}

	// --dry-run: score + render but write nothing, so the operator can
	// evaluate the would-be case before keeping it. Exit non-zero ONLY on a
	// genuine error (bad bundle, unsafe case name) — never on "previewed,
	// not written".
	if *dryRun {
		res, err := corpusdistill.Preview(src, opts)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "DRY RUN — no files written.\n")
		_, _ = fmt.Fprintf(stdout, "case dir: %s\n", res.CaseDir)
		_, _ = fmt.Fprintf(stdout, "derived outcome: %s\n", res.Card.Outcome)
		_, _ = fmt.Fprintf(stdout, "\n--- expected.json ---\n%s", res.ExpectedJSON)
		_, _ = fmt.Fprintf(stdout, "\n--- case.md ---\n%s", res.CaseMD)
		return 0
	}

	caseDir, err := corpusdistill.Distill(src, opts)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintln(stdout, caseDir)
	return 0
}

// openSource picks the bundle source by precedence: --stage-id (fetch) >
// --in file > stdin. It returns an io.Reader over the bundle bytes.
func openSource(ctx context.Context, stageID, in, backendURL, token string, stdin io.Reader) (io.Reader, error) {
	switch {
	case stageID != "":
		body, err := corpusdistill.FetchStageTrace(ctx, backendURL, stageID, token)
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(body), nil
	case in != "" && in != "-":
		f, err := os.Open(in)
		if err != nil {
			return nil, fmt.Errorf("open --in %q: %w", in, err)
		}
		// The caller (Distill) reads fully then returns; closing on
		// return of run() is unnecessary for a short-lived CLI, but read
		// the whole file now so we can close the handle deterministically.
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, fmt.Errorf("read --in %q: %w", in, err)
		}
		return bytes.NewReader(data), nil
	default:
		return stdin, nil
	}
}

// resolveOutDir returns the explicit --out-dir when set, otherwise the
// mode's default corpus dir (defaultRel) relative to the cwd. The default is
// fail-loud: if the corpus PARENT dir is absent from the cwd, it returns an
// actionable error rather than silently writing to a stray location.
func resolveOutDir(explicit, defaultRel string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if info, err := os.Stat(corpusParentRel); err != nil || !info.IsDir() {
		return "", fmt.Errorf(
			"default --out-dir requires %q to exist relative to the current directory; run from the repo root or pass --out-dir <dir>",
			corpusParentRel)
	}
	return filepath.FromSlash(defaultRel), nil
}

// missRunParams collects the --plan-review-miss mode inputs.
type missRunParams struct {
	runID, in, backendURL, token string
	opts                         corpusdistill.MissOptions
	dryRun                       bool
}

// runPlanReviewMiss drives the --plan-review-miss mode: source the audit
// items (--run-id fetch > --in file > stdin), then preview (--dry-run) or
// distill the class-3 cases.
func runPlanReviewMiss(p missRunParams, stdout, stderr io.Writer) int {
	items, err := loadAuditItems(context.Background(), p)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	if p.dryRun {
		results, err := corpusdistill.PreviewPlanReviewMiss(items, p.opts)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "DRY RUN — no files written.\n")
		for _, res := range results {
			_, _ = fmt.Fprintf(stdout, "case dir: %s\n", res.CaseDir)
			_, _ = fmt.Fprintf(stdout, "\n--- miss.json ---\n%s", res.MissJSON)
			_, _ = fmt.Fprintf(stdout, "\n--- case.md ---\n%s", res.CaseMD)
		}
		return 0
	}

	dirs, err := corpusdistill.DistillPlanReviewMiss(items, p.opts)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	for _, d := range dirs {
		_, _ = fmt.Fprintln(stdout, d)
	}
	return 0
}

// loadAuditItems sources the audit items for the --plan-review-miss mode:
// --run-id fetches every acceptance_triage_decided page from the backend;
// otherwise --in/stdin supplies JSON — either a bare items array or the
// {items:[...]} audit-endpoint envelope.
func loadAuditItems(ctx context.Context, p missRunParams) ([]corpusdistill.AuditItem, error) {
	if p.runID != "" {
		return corpusdistill.FetchRunTriageAudit(ctx, p.backendURL, p.runID, p.token)
	}
	src, err := openSource(ctx, "", p.in, p.backendURL, p.token, os.Stdin)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(src)
	if err != nil {
		return nil, fmt.Errorf("read audit items: %w", err)
	}
	var items []corpusdistill.AuditItem
	if err := json.Unmarshal(raw, &items); err == nil {
		return items, nil
	}
	var envelope struct {
		Items []corpusdistill.AuditItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("audit items input must be a JSON array of audit items or an {items:[...]} envelope: %w", err)
	}
	return envelope.Items, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
