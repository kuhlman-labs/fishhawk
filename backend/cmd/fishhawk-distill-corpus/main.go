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
package main

import (
	"bytes"
	"context"
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
const (
	defaultCorpusRel = "backend/internal/agenteval/testdata/corpus"
	corpusParentRel  = "backend/internal/agenteval/testdata"
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

Flags:
`, defaultCorpusRel, corpusParentRel)
		fs.PrintDefaults()
	}

	var (
		in         = fs.String("in", "", "path to a trace bundle (.jsonl.gz or .jsonl); '-' or empty reads stdin")
		stageID    = fs.String("stage-id", "", "fetch the bundle for this stage id from --backend-url (takes precedence over --in/stdin)")
		caseName   = fs.String("case-name", "", "corpus case slug (required); becomes the case directory name")
		issue      = fs.String("issue", "", "originating issue/run reference recorded in case.md (required), e.g. '#819'")
		outDir     = fs.String("out-dir", "", "corpus parent directory (default: "+defaultCorpusRel+" relative to cwd)")
		force      = fs.Bool("force", false, "overwrite an existing case directory")
		backendURL = fs.String("backend-url", envOr("FISHHAWK_BACKEND_URL", "http://localhost:8080"), "backend base URL for --stage-id fetch (env FISHHAWK_BACKEND_URL)")
		token      = fs.String("token", os.Getenv("FISHHAWK_TOKEN"), "bearer API token for --stage-id fetch (env FISHHAWK_TOKEN)")
	)

	if err := fs.Parse(args); err != nil {
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

	resolvedOut, err := resolveOutDir(*outDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	src, err := openSource(context.Background(), *stageID, *in, *backendURL, *token, os.Stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	caseDir, err := corpusdistill.Distill(src, corpusdistill.Options{
		CaseName: *caseName,
		Issue:    *issue,
		OutDir:   resolvedOut,
		Force:    *force,
		// Only the --stage-id fetch path GETs the redacted-only trace
		// endpoint, so only it can claim PRODUCTION+REDACTED provenance.
		Fetched: *stageID != "",
	})
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
// default corpus dir relative to the cwd. The default is fail-loud: if the
// corpus PARENT dir is absent from the cwd, it returns an actionable error
// rather than silently writing to a stray location.
func resolveOutDir(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if info, err := os.Stat(corpusParentRel); err != nil || !info.IsDir() {
		return "", fmt.Errorf(
			"default --out-dir requires %q to exist relative to the current directory; run from the repo root or pass --out-dir <dir>",
			corpusParentRel)
	}
	return filepath.FromSlash(defaultCorpusRel), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
