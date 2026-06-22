// Command fishhawk-distill-corpus scaffolds an agent-eval corpus case
// (trace.jsonl + expected.json + case.md) from a stored trace bundle,
// automating the mechanical half of #819. It is a dev/operator-only
// tool, kept OUT of the production fishhawkd server binary as a
// standalone command.
//
// Two input sources:
//
//   - --in <path> | '-' | (absent): read bundle bytes from a file, or
//     from stdin. gzip-vs-plain is auto-detected by corpusdistill.
//   - --stage-id <uuid>: fetch the REDACTED bundle over
//     GET {FISHHAWK_BACKEND_URL}/v0/stages/{stage_id}/trace and feed the
//     response body to the same offline core.
//
// The fetch path and the file/stdin path converge on
// corpusdistill.Distill — the pure, offline, unit-tested core — so the
// only network code is the thin fetchStageTrace helper.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/corpusdistill"
)

// defaultCorpusDir is the repo-relative corpus directory new cases are
// written under when --out-dir is not given. It is resolved against the
// current working directory and existence-checked (fail-loud) so the
// tool never silently scaffolds a case into a stray cwd-relative path
// (condition 2).
const defaultCorpusDir = "backend/internal/agenteval/testdata/corpus"

// maxTraceFetchBytes caps the HTTP fetch body. Mirrors the backend's
// trace-bundle ceiling (server.maxTraceBundleBytes) so a runaway
// response can't exhaust memory.
const maxTraceFetchBytes = 64 * 1024 * 1024

// flags holds the parsed command-line inputs.
type flags struct {
	in       string
	caseName string
	issue    string
	outDir   string
	stageID  string
	force    bool
}

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "fishhawk-distill-corpus:", err)
		os.Exit(1)
	}
}

// run is the testable entry point: argv + env + stdin in, error out.
func run(argv []string, getenv func(string) string, stdin io.Reader) error {
	fs := flag.NewFlagSet("fishhawk-distill-corpus", flag.ContinueOnError)
	var f flags
	fs.StringVar(&f.in, "in", "", "path to a trace bundle (gzip or plain JSONL); '-' or absent reads stdin")
	fs.StringVar(&f.caseName, "case-name", "", "corpus case directory name (required)")
	fs.StringVar(&f.issue, "issue", "", "originating issue reference recorded in case.md (e.g. #1290)")
	fs.StringVar(&f.outDir, "out-dir", "",
		"parent dir for the case; default "+defaultCorpusDir+
			" resolved from the cwd (run from the repo root, or pass this explicitly)")
	fs.StringVar(&f.stageID, "stage-id", "",
		"fetch the redacted bundle from GET {FISHHAWK_BACKEND_URL}/v0/stages/{stage_id}/trace instead of --in")
	fs.BoolVar(&f.force, "force", false, "overwrite an existing case directory")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	if f.caseName == "" {
		return errors.New("--case-name is required")
	}

	outDir, err := resolveOutDir(f.outDir)
	if err != nil {
		return err
	}

	input, err := readInput(&f, getenv, stdin)
	if err != nil {
		return err
	}

	return corpusdistill.Distill(input, corpusdistill.Options{
		CaseName: f.caseName,
		Issue:    f.issue,
		OutDir:   outDir,
		Force:    f.force,
	})
}

// resolveOutDir returns the case parent directory. An explicit
// --out-dir is used verbatim. Otherwise the repo-relative default is
// resolved against the cwd and FAILS LOUD with an actionable error when
// that directory does not exist — never silently scaffolds into a
// cwd-relative path that may not be the real corpus (condition 2).
func resolveOutDir(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	info, err := os.Stat(defaultCorpusDir)
	if err != nil || !info.IsDir() {
		wd, _ := os.Getwd()
		return "", fmt.Errorf(
			"default corpus dir %q not found from %q; run from the repo root or pass --out-dir explicitly",
			defaultCorpusDir, wd)
	}
	return defaultCorpusDir, nil
}

// readInput resolves the bundle bytes from whichever source the flags
// select: --stage-id fetches over HTTP; otherwise --in reads a file,
// '-' or absence reads stdin. --stage-id and --in are mutually
// exclusive.
func readInput(f *flags, getenv func(string) string, stdin io.Reader) ([]byte, error) {
	if f.stageID != "" {
		if f.in != "" {
			return nil, errors.New("--stage-id and --in are mutually exclusive")
		}
		backendURL, apiToken, err := loadConfig(getenv)
		if err != nil {
			return nil, err
		}
		return fetchStageTrace(http.DefaultClient, backendURL, apiToken, f.stageID)
	}

	if f.in == "" || f.in == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		return b, nil
	}
	b, err := os.ReadFile(filepath.Clean(f.in))
	if err != nil {
		return nil, fmt.Errorf("read --in %q: %w", f.in, err)
	}
	return b, nil
}

// loadConfig reads the backend URL + API token from the env, mirroring
// backend/cmd/fishhawk-mcp loadConfig: FISHHAWK_BACKEND_URL defaults to
// http://localhost:8080, FISHHAWK_API_TOKEN is required for the fetch
// path (the trace endpoint authenticates a bearer fhk_ token via the
// server's bearerAuth middleware — see condition 3).
func loadConfig(getenv func(string) string) (backendURL, apiToken string, err error) {
	backendURL = strings.TrimRight(getenv("FISHHAWK_BACKEND_URL"), "/")
	if backendURL == "" {
		backendURL = "http://localhost:8080"
	}
	apiToken = getenv("FISHHAWK_API_TOKEN")
	if apiToken == "" {
		return "", "", errors.New("FISHHAWK_API_TOKEN is required for the --stage-id fetch path")
	}
	return backendURL, apiToken, nil
}

// fetchStageTrace is the thin, isolated network helper (the only
// non-offline code). It GETs the redacted trace bundle for a stage and
// returns the response body bytes for the offline core. base is passed
// explicitly so an in-process httptest.Server can target it (condition
// 1). Auth is an Authorization: Bearer header — the scheme the trace
// endpoint's bearerAuth middleware accepts for an fhk_ operator token
// (condition 3).
func fetchStageTrace(client *http.Client, base, apiToken, stageID string) ([]byte, error) {
	url := fmt.Sprintf("%s/v0/stages/%s/trace", base, stageID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build trace request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch stage trace: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("fetch stage trace %s: unexpected status %d: %s",
			stageID, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxTraceFetchBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read trace body: %w", err)
	}
	if len(b) > maxTraceFetchBytes {
		return nil, fmt.Errorf("trace body exceeds %d bytes", maxTraceFetchBytes)
	}
	return b, nil
}
