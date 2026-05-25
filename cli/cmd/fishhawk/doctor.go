package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// checkResult holds the outcome of a single doctor rung.
type checkResult struct {
	label     string
	detail    string
	status    string // "ok", "warn", or "fail"
	remediate string // non-empty on warn or fail
}

// doctorHTTPDo is the HTTP seam for doctor checks that probe the backend.
// Tests swap it for a stub; production uses http.DefaultClient.Do.
var doctorHTTPDo = func(req *http.Request) (*http.Response, error) {
	return http.DefaultClient.Do(req)
}

// doctorLookPath resolves a binary name to its absolute path.
// Test seam matching the runnerBinaryLookPath pattern in runner.go.
var doctorLookPath = exec.LookPath

// doctorRunOutput runs an external command and returns its trimmed stdout.
// Returns ("", non-nil err) on any failure including non-zero exit code.
// Test seam — production delegates to exec.Command().Output().
var doctorRunOutput = func(name string, arg ...string) (string, error) {
	out, err := exec.Command(name, arg...).Output() //nolint:gosec
	return strings.TrimSpace(string(out)), err
}

// runDoctor implements `fishhawk doctor`.
func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	runnerBinary := fs.String("runner-binary", envOr("FISHHAWK_RUNNER_BIN", ""),
		"path to fishhawk-runner binary; defaults to PATH lookup")
	workingDir := fs.String("working-dir", ".", "repo checkout to inspect")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	checks := []checkResult{
		checkBackend(*cf.backendURL),
		checkToken(*cf.backendURL, *cf.token),
		checkSpec(*workingDir),
		checkRunnerBinary(*runnerBinary, *workingDir),
		checkMCPRegistration(),
		checkGitOrigin(*workingDir),
		checkGitWorkingTree(*workingDir),
		checkGhCLI(),
	}

	useColor := isTerminal(stdout) && os.Getenv("NO_COLOR") == ""

	failures := 0
	warnings := 0
	for _, r := range checks {
		statusStr := r.status
		if useColor {
			switch r.status {
			case "fail":
				statusStr = "\033[31mfail\033[0m"
			case "warn":
				statusStr = "\033[33mwarn\033[0m"
			}
		}
		_, _ = fmt.Fprintf(stdout, "%-25s %-40s %s\n", r.label, r.detail, statusStr)
		if r.remediate != "" {
			_, _ = fmt.Fprintf(stdout, "  hint: %s\n", r.remediate)
		}
		switch r.status {
		case "fail":
			failures++
		case "warn":
			warnings++
		}
	}

	_, _ = fmt.Fprintln(stdout, "")
	if failures == 0 {
		if warnings == 0 {
			_, _ = fmt.Fprintln(stdout, "ready for local loop")
		} else {
			_, _ = fmt.Fprintf(stdout, "ready for local loop (%d warning(s))\n", warnings)
		}
		return exitOK
	}
	if warnings == 0 {
		_, _ = fmt.Fprintf(stdout, "%d check(s) failed — fix the above before running the loop\n", failures)
	} else {
		_, _ = fmt.Fprintf(stdout, "%d check(s) failed, %d warning(s) — fix the above before running the loop\n", failures, warnings)
	}
	return exitFailure
}

// isTerminal reports whether w is a character device (i.e. a terminal).
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// checkBackend probes GET {backendURL}/healthz and shows the version on success.
func checkBackend(backendURL string) checkResult {
	label := "backend reachable"
	req, err := http.NewRequest(http.MethodGet, backendURL+"/healthz", nil)
	if err != nil {
		return checkResult{label: label, detail: err.Error(), status: "fail",
			remediate: "check --backend-url or $FISHHAWK_BACKEND_URL; ensure fishhawkd is running"}
	}
	resp, err := doctorHTTPDo(req)
	if err != nil {
		return checkResult{label: label, detail: err.Error(), status: "fail",
			remediate: "check --backend-url or $FISHHAWK_BACKEND_URL; ensure fishhawkd is running"}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return checkResult{label: label, detail: fmt.Sprintf("HTTP %d", resp.StatusCode), status: "fail",
			remediate: "backend returned non-200; check fishhawkd logs"}
	}
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	detail := "ok"
	if body.Version != "" {
		detail = body.Version
	}
	return checkResult{label: label, detail: detail, status: "ok"}
}

// checkToken verifies the token is set, has the fhk_ prefix, and is
// accepted by the backend's /v0/runs endpoint.
func checkToken(backendURL, token string) checkResult {
	label := "token valid"
	if token == "" {
		token = os.Getenv("FISHHAWK_TOKEN")
	}
	if token == "" || !strings.HasPrefix(token, "fhk_") {
		return checkResult{label: label, detail: "token missing or malformed", status: "fail",
			remediate: "set --token or $FISHHAWK_TOKEN to a value starting with fhk_"}
	}
	req, err := http.NewRequest(http.MethodGet, backendURL+"/v0/runs?limit=0", nil)
	if err != nil {
		return checkResult{label: label, detail: err.Error(), status: "fail",
			remediate: "check --backend-url"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := doctorHTTPDo(req)
	if err != nil {
		return checkResult{label: label, detail: err.Error(), status: "fail",
			remediate: "backend unreachable; run the backend check first"}
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return checkResult{label: label, detail: "accepted", status: "ok"}
	case http.StatusUnauthorized:
		return checkResult{label: label, detail: "HTTP 401 — invalid token", status: "fail",
			remediate: "reissue via `fishhawkd token issue --subject <login> --scopes read:runs,...`"}
	case http.StatusForbidden:
		return checkResult{label: label, detail: "HTTP 403 — missing scope", status: "fail",
			remediate: "reissue with --scopes read:runs,write:runs,write:approvals,write:stages"}
	default:
		return checkResult{label: label, detail: fmt.Sprintf("HTTP %d", resp.StatusCode), status: "fail",
			remediate: "unexpected status; check fishhawkd logs"}
	}
}

// checkSpec discovers and validates .fishhawk/workflows.yaml under workingDir.
func checkSpec(workingDir string) checkResult {
	label := "workflow spec present"
	ds, err := discoverSpec(workingDir, "")
	if err != nil {
		return checkResult{label: label, detail: err.Error(), status: "fail",
			remediate: "fix the read error on .fishhawk/workflows.yaml"}
	}
	if ds == nil {
		return checkResult{label: label, detail: "not found", status: "fail",
			remediate: "create .fishhawk/workflows.yaml (see docs/spec/workflows-v0.md)"}
	}
	if err := spec.ValidateBytes(ds.Contents); err != nil {
		return checkResult{label: label, detail: "schema invalid", status: "fail",
			remediate: "run `fishhawk validate` for details"}
	}
	detail := fmt.Sprintf("%s (%d B)", ds.Path, len(ds.Contents))
	return checkResult{label: label, detail: detail, status: "ok"}
}

// checkRunnerBinary resolves the fishhawk-runner binary via flag > env > PATH > repo bin/.
func checkRunnerBinary(flagVal, workingDir string) checkResult {
	label := "runner binary found"
	binary := flagVal
	if binary == "" {
		binary = os.Getenv("FISHHAWK_RUNNER_BIN")
	}
	if binary != "" {
		return checkResult{label: label, detail: binary, status: "ok"}
	}
	resolved, err := doctorLookPath("fishhawk-runner")
	if err == nil {
		return checkResult{label: label, detail: resolved, status: "ok"}
	}
	for _, candidate := range []string{
		filepath.Join(workingDir, "bin", "fishhawk-runner"),
		filepath.Join(workingDir, "bin", "fishhawk-runner.exe"),
	} {
		fi, statErr := os.Stat(candidate)
		if statErr == nil && !fi.IsDir() {
			return checkResult{label: label, detail: candidate + " (via repo bin/)", status: "ok"}
		}
	}
	return checkResult{label: label, detail: "not found", status: "fail",
		remediate: "install fishhawk-runner to PATH or set $FISHHAWK_RUNNER_BIN"}
}

// checkMCPRegistration verifies `claude mcp get fishhawk` exits 0.
func checkMCPRegistration() checkResult {
	label := "MCP registered"
	_, err := doctorRunOutput("claude", "mcp", "get", "fishhawk")
	if err != nil {
		return checkResult{label: label, detail: "not registered", status: "fail",
			remediate: "run: claude mcp add fishhawk -- /path/to/fishhawk-mcp (see docs/mcp/install.md)"}
	}
	return checkResult{label: label, detail: "fishhawk", status: "ok"}
}

// checkGitOrigin verifies the working directory has a git remote named origin.
// Reuses gitRemoteOriginURL seam defined in runner.go.
func checkGitOrigin(workingDir string) checkResult {
	label := "git remote origin"
	url, err := gitRemoteOriginURL(workingDir)
	if err != nil {
		return checkResult{label: label, detail: "no origin remote", status: "fail",
			remediate: "git remote add origin <url>"}
	}
	return checkResult{label: label, detail: url, status: "ok"}
}

// checkGitWorkingTree reports whether the working tree is clean.
// Uses exec.Command directly (needs Dir support that doctorRunOutput lacks).
func checkGitWorkingTree(workingDir string) checkResult {
	label := "git working tree clean"
	cmd := exec.Command("git", "status", "--porcelain") //nolint:gosec
	if workingDir != "" && workingDir != "." {
		cmd.Dir = workingDir
	}
	out, err := cmd.Output()
	if err != nil {
		return checkResult{label: label, detail: "git error", status: "fail",
			remediate: "ensure you are inside a git repository"}
	}
	if strings.TrimSpace(string(out)) != "" {
		return checkResult{label: label, detail: "uncommitted changes", status: "warn",
			remediate: "in-flight changes are expected mid-loop; commit or stash before starting a new run"}
	}
	return checkResult{label: label, detail: "clean", status: "ok"}
}

// checkGhCLI verifies `gh auth status` exits 0.
func checkGhCLI() checkResult {
	label := "gh CLI authenticated"
	out, err := doctorRunOutput("gh", "auth", "status")
	if err != nil {
		return checkResult{label: label, detail: "not authenticated", status: "fail",
			remediate: "run: gh auth login"}
	}
	detail := "authenticated"
	if first, _, found := strings.Cut(out, "\n"); found && strings.TrimSpace(first) != "" {
		detail = strings.TrimSpace(first)
	}
	return checkResult{label: label, detail: detail, status: "ok"}
}
