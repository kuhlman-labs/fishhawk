package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
	"github.com/kuhlman-labs/fishhawk/credstore"
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

// doctorCredLoad is the credstore seam for doctor's credential ladder.
// Tests swap it; production delegates to credstore.Load. Matches the
// doctorHTTPDo / doctorLookPath / doctorRunOutput seam pattern.
var doctorCredLoad = credstore.Load

// doctorExecutable is the os.Executable seam for the runner sibling rung.
// Tests swap it to point at a directory whose fishhawk-runner sibling they
// control; production delegates to os.Executable.
var doctorExecutable = os.Executable

// doctorCredential is a bearer credential resolved for the doctor probes,
// with display metadata. Classification is for DISPLAY ONLY — the backend,
// not a prefix table, decides whether a credential is valid.
type doctorCredential struct {
	token  string
	source string // where it came from, for the rung detail
	class  string // display class by prefix
}

// classifyCredential names a credential class by its prefix, for display.
// Never used for admission — an unrecognized prefix is still probed.
func classifyCredential(token string) string {
	switch {
	case strings.HasPrefix(token, "fhk_"):
		return "operator token (fhk_)"
	case strings.HasPrefix(token, "fhm_"):
		return "machine token (fhm_)"
	case strings.HasPrefix(token, "fho_"):
		return "OAuth access token (fho_)"
	default:
		return "unrecognized prefix"
	}
}

// resolveDoctorCredential mirrors newClient's ladder (run.go): an explicit
// --token / $FISHHAWK_TOKEN (already folded into flagToken by
// bindCommonFlags) wins; when empty, fall back to the stored credential
// minted by `fishhawk token login` and keyed by the backend URL. Returns a
// zero doctorCredential (empty token) when no tier resolves one — the token
// rung degrades that to a warn, never a hard fail.
func resolveDoctorCredential(backendURL, flagToken string) doctorCredential {
	if flagToken != "" {
		return doctorCredential{
			token:  flagToken,
			source: "--token / $FISHHAWK_TOKEN",
			class:  classifyCredential(flagToken),
		}
	}
	if cred, err := doctorCredLoad(backendURL); err == nil && cred.Token != "" {
		return doctorCredential{
			token:  cred.Token,
			source: "stored credential (fishhawk token login)",
			class:  classifyCredential(cred.Token),
		}
	}
	return doctorCredential{}
}

// readinessOutcome carries the authoritative server-side readiness verdict
// into the local token rung so a 200 from GET /v0/onboarding/readiness can
// never be suppressed by the local /v0/runs heuristic. answered is true ONLY
// when the endpoint returned HTTP 200 AND its body decoded into a readiness
// payload — a 200 whose body does not decode is NOT an answer, so the token
// rung must still be able to fail (binding condition 1).
type readinessOutcome struct {
	answered       bool
	scopesAdequate bool
}

// runDoctor implements `fishhawk doctor`.
func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	runnerBinary := fs.String("runner-binary", envOr("FISHHAWK_RUNNER_BIN", ""),
		"path to fishhawk-runner binary; defaults to PATH lookup")
	workingDir := fs.String("working-dir", ".", "repo checkout to inspect")
	repo := fs.String("repo", "", "target repo owner/name for onboarding checks; auto-detected from git origin when empty")
	specOnly := fs.Bool("spec-only", false,
		"run only the environment-free spec rungs (schema validity + execution-path coverage); "+
			"skips all docker/backend/token/MCP/git/gh/onboarding checks. Use to validate a freshly "+
			"scaffolded repo with no local Fishhawk environment.")
	runVerify := fs.Bool("run-verify-command", false,
		"execute the `verify command` rung, which runs the spec's configured "+
			"executor.verify.command in a throwaway detached git worktree at HEAD. OFF by "+
			"default: that command string comes from the inspected repository's committed "+
			"spec, so doctor never executes code from a checkout you have not reviewed unless "+
			"you ask for it by name")
	skipVerify := fs.Bool("skip-verify-command", false,
		"force the `verify command` rung to be skipped; wins over --run-verify-command")
	verifyTimeout := fs.Duration("verify-timeout", defaultVerifyTimeout,
		"cap on how long the `verify command` rung lets each configured command run; "+
			"the spec's own executor.verify.timeout wins when it is shorter")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	// --spec-only: the two rungs that read only local bytes (no docker,
	// network, backend, token, MCP, or git state). This is the fresh-repo
	// quick-validate path — a repo whose sole workflows.yaml is
	// schema-valid and declares an executor for every stage exits 0
	// without any local Fishhawk environment; a missing/invalid spec
	// still fails closed via checkSpec.
	//
	// checkVerifyCommand is deliberately EXCLUDED here: it provisions a
	// git worktree and executes a subprocess, so it is neither
	// environment-free nor byte-only.
	var checks []checkResult
	if *specOnly {
		checks = []checkResult{
			checkSpec(*workingDir),
			checkExecutionPath(*workingDir),
		}
	} else {
		// Resolve the onboarding target repo: explicit --repo wins, else
		// auto-detect from the working dir's git origin. An unresolved repo
		// is not fatal — checkOnboardingReadiness degrades it to a single WARN.
		resolvedRepo := *repo
		if resolvedRepo == "" {
			if detected, err := detectGitHubRepo(*workingDir); err == nil {
				resolvedRepo = detected
			}
		}

		// Resolve the bearer credential ONCE (the same ladder newClient
		// uses) and probe onboarding readiness FIRST, so the authoritative
		// server-side verdict is in hand before the local token rung decides
		// and the two can never disagree about which credential is in play.
		cred := resolveDoctorCredential(*cf.backendURL, *cf.token)
		readinessChecks, readiness := checkOnboardingReadiness(*cf.backendURL, cred.token, resolvedRepo)

		checks = []checkResult{
			checkDockerDaemon(),
			checkPostgresContainer(),
			checkMinioContainer(),
			checkBackend(*cf.backendURL),
			checkToken(*cf.backendURL, cred, readiness),
			checkSpec(*workingDir),
			checkExecutionPath(*workingDir),
			checkVerifyCommandGated(*workingDir, *runVerify, *skipVerify, *verifyTimeout),
			checkRunnerBinary(*runnerBinary, *workingDir),
			checkMCPRegistration(*cf.backendURL),
			checkGitOrigin(*workingDir),
			checkGitWorkingTree(*workingDir),
			checkGhCLI(),
			checkBackendSHADrift(*cf.backendURL, *workingDir),
			checkRunnerSchemaDrift(*cf.backendURL, *runnerBinary, *workingDir),
			checkCLIVersion(*cf.backendURL),
		}
		// Append the readiness rungs LAST so the printed rung ORDER is
		// unchanged (they still render after the local rungs).
		checks = append(checks, readinessChecks...)
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

// checkDockerDaemon verifies the Docker daemon is reachable via `docker info`.
func checkDockerDaemon() checkResult {
	label := "docker daemon running"
	_, err := doctorRunOutput("docker", "info")
	if err != nil {
		return checkResult{label: label, detail: "daemon not reachable", status: "fail",
			remediate: "run: open -a Docker, wait ~20s, then retry"}
	}
	return checkResult{label: label, detail: "ok", status: "ok"}
}

// checkPostgresContainer verifies the fishhawk-postgres container is running
// and accepting connections.
func checkPostgresContainer() checkResult {
	label := "postgres container"
	out, err := doctorRunOutput("docker", "ps", "--filter", "name=^/fishhawk-postgres$", "--format", "{{.Names}}")
	if err != nil || strings.TrimSpace(out) == "" {
		return checkResult{label: label, detail: "not running", status: "fail",
			remediate: "run: docker compose up -d"}
	}
	_, pgErr := doctorRunOutput("pg_isready", "-h", "localhost", "-p", "5432")
	if pgErr != nil {
		return checkResult{label: label, detail: "container up, postgres initialising", status: "warn",
			remediate: "postgres container is up but not yet accepting connections; wait and retry"}
	}
	return checkResult{label: label, detail: "accepting connections", status: "ok"}
}

// checkMinioContainer verifies the fishhawk-minio container is running and
// its health endpoint returns 200.
func checkMinioContainer() checkResult {
	label := "minio container"
	out, err := doctorRunOutput("docker", "ps", "--filter", "name=^/fishhawk-minio$", "--format", "{{.Names}}")
	if err != nil || strings.TrimSpace(out) == "" {
		return checkResult{label: label, detail: "not running", status: "fail",
			remediate: "run: docker compose up -d"}
	}
	// Port 9000 is hardcoded; if .env overrides MINIO_PORT this rung will not read it (v1 limitation).
	req, reqErr := http.NewRequest(http.MethodGet, "http://localhost:9000/minio/health/live", nil)
	if reqErr != nil {
		return checkResult{label: label, detail: reqErr.Error(), status: "warn",
			remediate: "minio container is up but health probe failed; check logs with docker logs fishhawk-minio"}
	}
	resp, doErr := doctorHTTPDo(req)
	if doErr != nil {
		return checkResult{label: label, detail: "container up, health probe failed", status: "warn",
			remediate: "minio container is up but health probe failed; check logs with docker logs fishhawk-minio"}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return checkResult{label: label, detail: fmt.Sprintf("container up, health probe returned HTTP %d", resp.StatusCode), status: "warn",
			remediate: "minio container is up but health probe failed; check logs with docker logs fishhawk-minio"}
	}
	return checkResult{label: label, detail: "healthy", status: "ok"}
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

// checkToken probes the backend with whatever credential resolveDoctorCredential
// found, whatever its class. The fhk_ prefix no longer gates admission — the
// backend decides what a valid credential is, so an fho_ OAuth token passes and
// an unrecognized prefix is still probed. Branches:
//
//	(a) no credential resolved from any tier -> WARN (an MCP/OAuth-driven loop
//	    needs no CLI credential);
//	(b) a resolved credential the backend accepts (200) -> ok, naming its class
//	    and source;
//	(c) a non-200 AND readiness did NOT answer 200 -> fail (the token rung is
//	    the authority here);
//	(d) a non-200 BUT readiness answered 200 -> WARN, never fail: the backend
//	    authenticated this credential on the authoritative readiness endpoint,
//	    and `token scope adequate` (server-side) is the authority on scope. An
//	    OAuth/cookie-class identity carries no explicit scope list and can
//	    legitimately 403 on /v0/runs while being fully adequate per readiness.
func checkToken(backendURL string, cred doctorCredential, readiness readinessOutcome) checkResult {
	label := "token valid"
	if cred.token == "" {
		return checkResult{label: label, detail: "no CLI credential configured", status: "warn",
			remediate: "run `fishhawk token login`, or set --token / $FISHHAWK_TOKEN; " +
				"an MCP/OAuth-driven loop does not need a CLI credential"}
	}
	req, err := http.NewRequest(http.MethodGet, backendURL+"/v0/runs?limit=0", nil)
	if err != nil {
		return checkResult{label: label, detail: err.Error(), status: "fail",
			remediate: "check --backend-url"}
	}
	req.Header.Set("Authorization", "Bearer "+cred.token)
	resp, err := doctorHTTPDo(req)
	if err != nil {
		return checkResult{label: label, detail: err.Error(), status: "fail",
			remediate: "backend unreachable; run the backend check first"}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return checkResult{label: label,
			detail: fmt.Sprintf("accepted — %s via %s", cred.class, cred.source), status: "ok"}
	}
	// (d) The cascade break: a non-200 is downgraded to warn ONLY when the
	// authoritative readiness endpoint returned a decodable 200 for the SAME
	// credential. answered is false on a malformed 200, so a broken backend
	// response can never suppress a genuine token failure (binding condition 1).
	if readiness.answered {
		return checkResult{label: label,
			detail: fmt.Sprintf("HTTP %d on /v0/runs, but the readiness endpoint authenticated this credential", resp.StatusCode),
			status: "warn",
			remediate: "the backend authenticated this credential on the authoritative readiness endpoint; " +
				"`token scope adequate` (server-side) is the authority on scope"}
	}
	// (c) Readiness did not answer — the token rung is the authority; fail.
	switch resp.StatusCode {
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

// resolveRunnerBinary mirrors the resolution order the MCP dispatch path uses
// (backend/internal/mcpserver/run_stage.go): explicit input > $FISHHAWK_RUNNER_BIN
// > fishhawk-runner SIBLING of the running executable > PATH, then the CLI-only
// <workingDir>/bin/fishhawk-runner tier as the final rung. It returns the
// resolved path plus a source tag (for the rung detail), or an error when every
// tier misses.
//
// NOTE the sibling tier resolves next to THIS CLI binary, which is a PROXY for
// the serving binary's sibling that the dispatch path actually resolves against
// — the doctor CLI process is not the serving process, so it cannot observe the
// serving binary's directory. A green from that tier proves a runner sits beside
// the CLI, which is not the same claim.
func resolveRunnerBinary(flagVal, workingDir string) (path, source string, err error) {
	if flagVal != "" {
		return flagVal, "flag", nil
	}
	if v := os.Getenv("FISHHAWK_RUNNER_BIN"); v != "" {
		return v, "env", nil
	}
	if exe, exeErr := doctorExecutable(); exeErr == nil {
		sibling := filepath.Join(filepath.Dir(exe), "fishhawk-runner")
		if fi, statErr := os.Stat(sibling); statErr == nil && !fi.IsDir() {
			return sibling, "sibling", nil
		}
	}
	if p, lookErr := doctorLookPath("fishhawk-runner"); lookErr == nil {
		return p, "path", nil
	}
	for _, candidate := range []string{
		filepath.Join(workingDir, "bin", "fishhawk-runner"),
		filepath.Join(workingDir, "bin", "fishhawk-runner.exe"),
	} {
		fi, statErr := os.Stat(candidate)
		if statErr == nil && !fi.IsDir() {
			return candidate, "repo-bin", nil
		}
	}
	return "", "", errors.New("fishhawk-runner not found in flag, env, CLI-sibling, PATH, or repo bin/")
}

// checkRunnerBinary resolves the fishhawk-runner binary via the dispatch-mirroring
// order. When every tier misses it WARNS rather than fails: the CLI process is
// not the serving binary, so it cannot observe the sibling directory an
// MCP-driven dispatch resolves against — a hard fail would be a false negative
// for an operator whose runner sits beside the serving binary.
func checkRunnerBinary(flagVal, workingDir string) checkResult {
	label := "runner binary found"
	resolved, source, err := resolveRunnerBinary(flagVal, workingDir)
	if err != nil {
		return checkResult{label: label,
			detail: "not resolvable from this CLI process", status: "warn",
			remediate: "MCP-driven dispatch also resolves a fishhawk-runner sibling of the SERVING binary, " +
				"which this CLI process cannot observe; for the local CLI loop, install fishhawk-runner to PATH " +
				"or set $FISHHAWK_RUNNER_BIN"}
	}
	var detail string
	switch source {
	case "repo-bin":
		detail = resolved + " (via repo bin/)"
	case "sibling":
		detail = resolved + " (sibling of this CLI binary, as a proxy for the serving binary's sibling)"
	default:
		detail = resolved
	}
	return checkResult{label: label, detail: detail, status: "ok"}
}

// checkMCPRegistration parses `claude mcp list` and recognises a Fishhawk MCP
// registration in EITHER transport — the stdio shim (`fishhawk: /path/...`) or
// the HTTP registration (`fishhawk-http: https://host/mcp (HTTP)`) this issue
// exists to recognise. A registration qualifies when its name is `fishhawk` /
// `fishhawk-*` OR its target is an http(s) URL whose host:port equals the
// backend's and whose path ends in `/mcp` (a name-agnostic "points at this
// backend" test). Both matchers are load-bearing: an HTTP registration on a
// different port than --backend-url matches only by name, and a non-fishhawk
// name matches only by URL.
//
// Degradation:
//   - `claude mcp list` succeeds but no registration matches -> a `claude mcp
//     get` fallback probes BOTH the stdio and HTTP registration names before
//     declaring the registration absent -> fail with a transport-aware hint.
//   - `claude mcp list` errors but the `get` fallback resolves -> ok.
//   - the `claude` CLI is absent or erroring entirely -> WARN, never fail: the
//     operator may drive from another MCP client (binding condition 4).
func checkMCPRegistration(backendURL string) checkResult {
	label := "MCP registered"
	out, listErr := doctorRunOutput("claude", "mcp", "list")
	if listErr == nil {
		if detail, ok := matchMCPRegistration(out, backendURL); ok {
			return checkResult{label: label, detail: detail, status: "ok"}
		}
		// list answered but no match — try the get fallback before declaring
		// the registration genuinely absent.
		if detail, ok := mcpGetFallback(); ok {
			return checkResult{label: label, detail: detail, status: "ok"}
		}
		return checkResult{label: label, detail: "no fishhawk registration found", status: "fail",
			remediate: mcpRemediation(backendURL)}
	}
	// list errored. The get fallback covers the corner where list is broken but
	// get works — for BOTH transports (binding condition 2).
	if detail, ok := mcpGetFallback(); ok {
		return checkResult{label: label, detail: detail, status: "ok"}
	}
	// claude CLI absent or erroring on both list and get -> warn, not fail.
	return checkResult{label: label, detail: "cannot determine (claude CLI unavailable)", status: "warn",
		remediate: mcpRemediation(backendURL)}
}

// matchMCPRegistration scans `claude mcp list` output for a qualifying Fishhawk
// registration and returns a human detail (naming the matched registration and
// its transport) plus whether a match was found. The parse is tolerant: split
// each line on the FIRST ": " (names may contain spaces), strip a trailing
// " - <status>" and any " (HTTP)" / " (SSE)" transport marker.
func matchMCPRegistration(listOutput, backendURL string) (string, bool) {
	backendHostPort := ""
	if u, err := url.Parse(backendURL); err == nil {
		backendHostPort = u.Host
	}
	for _, line := range strings.Split(listOutput, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, target, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		target = strings.TrimSpace(target)
		// Strip a trailing " - <status>" (e.g. " - ✔ Connected").
		if idx := strings.LastIndex(target, " - "); idx >= 0 {
			target = strings.TrimSpace(target[:idx])
		}
		// Strip and record a transport marker; a stdio path carries none.
		transport := "stdio"
		for _, m := range []struct{ tag, name string }{{" (HTTP)", "HTTP"}, {" (SSE)", "SSE"}} {
			if strings.HasSuffix(target, m.tag) {
				transport = m.name
				target = strings.TrimSpace(strings.TrimSuffix(target, m.tag))
				break
			}
		}
		// (i) name match — covers both the stdio `fishhawk` and the HTTP
		// `fishhawk-http` registrations.
		if name == "fishhawk" || strings.HasPrefix(name, "fishhawk-") {
			return fmt.Sprintf("%s (%s)", name, transport), true
		}
		// (ii) backend-URL match — a name-agnostic "points at this backend".
		if backendHostPort != "" {
			if u, uErr := url.Parse(target); uErr == nil && (u.Scheme == "http" || u.Scheme == "https") &&
				u.Host == backendHostPort && strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/mcp") {
				return fmt.Sprintf("%s (%s, matches backend URL)", name, transport), true
			}
		}
	}
	return "", false
}

// mcpGetFallback probes `claude mcp get <name>` for BOTH the stdio and HTTP
// registration names, so a `list`-broken/`get`-working corner still recognises
// an HTTP-transport registration (binding condition 2). Returns a detail and
// whether either name resolved.
func mcpGetFallback() (string, bool) {
	for _, name := range []string{"fishhawk", "fishhawk-http"} {
		if _, err := doctorRunOutput("claude", "mcp", "get", name); err == nil {
			return "registered (" + name + ", via `claude mcp get`)", true
		}
	}
	return "", false
}

// mcpRemediation names the remedy for the transport in use: the HTTP
// registration first (the primary onboarding path), the stdio shim second.
func mcpRemediation(backendURL string) string {
	return fmt.Sprintf("register the backend's MCP surface — HTTP: "+
		"`claude mcp add --transport http fishhawk-http %s/mcp`; "+
		"or the stdio shim: `claude mcp add fishhawk -- /path/to/fishhawk-mcp` (see docs/mcp/install.md)",
		strings.TrimRight(backendURL, "/"))
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

// isBackendSourceCheckout reports whether workingDir is a checkout whose HEAD
// could plausibly BE the commit fishhawkd was built from: it carries the
// fishhawkd source (a backend/cmd/fishhawkd directory) AND a go.work at the
// root. It uses a structural marker rather than matching the git origin against
// a hardcoded slug, which keeps a fork or rename of this repo inside the drift
// check and makes the rung answerable offline. A repo that vendors a directory
// of that name would be misclassified and get the (spurious) drift warn back —
// strictly the status quo for such a repo.
func isBackendSourceCheckout(workingDir string) bool {
	if workingDir == "" {
		workingDir = "."
	}
	if fi, err := os.Stat(filepath.Join(workingDir, "backend", "cmd", "fishhawkd")); err != nil || !fi.IsDir() {
		return false
	}
	if fi, err := os.Stat(filepath.Join(workingDir, "go.work")); err != nil || fi.IsDir() {
		return false
	}
	return true
}

// checkBackendSHADrift compares the running backend's git_sha (from /healthz)
// against the local HEAD commit. A mismatch warns the operator that they may
// be running a backend built from a different commit.
//
// The comparison is prefix-wise after stripping a "-dirty" suffix: scripts/dev
// stamps `git rev-parse --short HEAD` (plus "-dirty" on a dirty tree) into dev
// builds, while the local side is the full HEAD SHA — exact equality would
// false-warn on every stamped dev build.
func checkBackendSHADrift(backendURL, workingDir string) checkResult {
	label := "backend SHA drift"
	// Skip outright when the working dir is not the backend's own source
	// checkout: comparing fishhawkd's build SHA to an unrelated repo's HEAD is
	// a category error that warns forever and can never be satisfied. This runs
	// BEFORE any HTTP call.
	if !isBackendSourceCheckout(workingDir) {
		return checkResult{label: label,
			detail: "skipped — working dir is not the backend's source checkout", status: "ok"}
	}
	req, err := http.NewRequest(http.MethodGet, backendURL+"/healthz", nil)
	if err != nil {
		return checkResult{label: label, detail: err.Error(), status: "warn",
			remediate: "check --backend-url"}
	}
	resp, err := doctorHTTPDo(req)
	if err != nil {
		return checkResult{label: label, detail: "healthz unreachable", status: "warn",
			remediate: "backend must be reachable for SHA comparison"}
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		GitSHA string `json:"git_sha"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	backendSHA := body.GitSHA
	if backendSHA == "" || backendSHA == "unknown" {
		return checkResult{label: label, detail: "backend SHA unknown (dev build)", status: "ok"}
	}

	cmd := exec.Command("git", "rev-parse", "HEAD") //nolint:gosec
	if workingDir != "" && workingDir != "." {
		cmd.Dir = workingDir
	}
	out, err := cmd.Output()
	if err != nil {
		return checkResult{label: label, detail: "git rev-parse failed", status: "warn",
			remediate: "ensure --working-dir is inside a git repository"}
	}
	localSHA := strings.TrimSpace(string(out))
	normalizedBackend := strings.TrimSuffix(backendSHA, "-dirty")
	builtDirty := normalizedBackend != backendSHA
	if strings.HasPrefix(localSHA, normalizedBackend) || strings.HasPrefix(normalizedBackend, localSHA) {
		detail := "in sync (" + shortSHA(normalizedBackend) + ")"
		if builtDirty {
			detail = "in sync (" + shortSHA(normalizedBackend) + ", dirty tree at build)"
		}
		return checkResult{label: label, detail: detail, status: "ok"}
	}
	return checkResult{
		label:     label,
		detail:    fmt.Sprintf("backend %s, local %s", shortSHA(backendSHA), shortSHA(localSHA)),
		status:    "warn",
		remediate: "backend is running a different commit; rebuild fishhawkd from HEAD or use the matching commit",
	}
}

// checkRunnerSchemaDrift compares the backend's embedded plan-standard-v1
// schema hash (from /healthz) against the runner binary's embedded hash
// (from 'fishhawk-runner version'). A mismatch warns that the plan schema
// has drifted between the two binaries.
func checkRunnerSchemaDrift(backendURL, runnerBinary, workingDir string) checkResult {
	label := "runner schema drift"

	runnerBin, _, err := resolveRunnerBinary(runnerBinary, workingDir)
	if err != nil {
		return checkResult{label: label, detail: "runner binary not found, schema drift not checked", status: "warn",
			remediate: "install fishhawk-runner or set $FISHHAWK_RUNNER_BIN"}
	}

	req, err := http.NewRequest(http.MethodGet, backendURL+"/healthz", nil)
	if err != nil {
		return checkResult{label: label, detail: err.Error(), status: "warn",
			remediate: "check --backend-url"}
	}
	resp, err := doctorHTTPDo(req)
	if err != nil {
		return checkResult{label: label, detail: "healthz unreachable", status: "warn"}
	}
	defer func() { _ = resp.Body.Close() }()
	var healthBody struct {
		Schemas map[string]string `json:"schemas"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&healthBody)
	backendHash := healthBody.Schemas["plan-standard-v1"]
	if backendHash == "" {
		return checkResult{label: label, detail: "backend does not report schema hashes (old build)", status: "warn",
			remediate: "upgrade fishhawkd to enable schema drift detection"}
	}

	runnerOut, err := doctorRunOutput(runnerBin, "version")
	if err != nil {
		return checkResult{label: label, detail: "runner does not support version output; rebuild to enable schema drift detection", status: "warn",
			remediate: "rebuild fishhawk-runner from HEAD to enable schema drift detection"}
	}
	var versionInfo struct {
		PlanSchemaHash string `json:"plan_schema_hash"`
	}
	if jsonErr := json.Unmarshal([]byte(runnerOut), &versionInfo); jsonErr != nil {
		return checkResult{label: label, detail: "runner version output not parseable", status: "warn",
			remediate: "ensure fishhawk-runner supports the version subcommand"}
	}
	if versionInfo.PlanSchemaHash == "" || versionInfo.PlanSchemaHash == backendHash {
		return checkResult{label: label, detail: "schemas in sync", status: "ok"}
	}
	return checkResult{
		label:     label,
		detail:    fmt.Sprintf("runner %s, backend %s", shortHash(versionInfo.PlanSchemaHash), shortHash(backendHash)),
		status:    "warn",
		remediate: "plan schema differs between runner and backend; rebuild both from the same commit",
	}
}

// checkCLIVersion compares the CLI's version against the backend's
// min_runner_version requirement. Both sides must be release builds
// (non-dev) for the comparison to run.
func checkCLIVersion(backendURL string) checkResult {
	label := "CLI version"
	req, err := http.NewRequest(http.MethodGet, backendURL+"/healthz", nil)
	if err != nil {
		return checkResult{label: label, detail: err.Error(), status: "warn",
			remediate: "check --backend-url"}
	}
	resp, err := doctorHTTPDo(req)
	if err != nil {
		return checkResult{label: label, detail: "healthz unreachable", status: "warn"}
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		MinRunnerVersion string `json:"min_runner_version"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	minVersion := body.MinRunnerVersion
	if minVersion == "" || minVersion == "dev" {
		return checkResult{label: label, detail: "no minimum version required", status: "ok"}
	}

	cliVersionStr, err := doctorRunOutput("fishhawk", "version")
	if err != nil {
		return checkResult{label: label, detail: "cannot determine CLI version", status: "warn",
			remediate: "ensure fishhawk is in PATH"}
	}
	cliVersionStr = strings.TrimSpace(cliVersionStr)
	if cliVersionStr == "" || cliVersionStr == "dev" {
		return checkResult{label: label, detail: "dev build (min version check skipped)", status: "ok"}
	}
	if semverLT(cliVersionStr, minVersion) {
		return checkResult{
			label:     label,
			detail:    fmt.Sprintf("%s < min %s", cliVersionStr, minVersion),
			status:    "warn",
			remediate: fmt.Sprintf("upgrade fishhawk CLI to at least %s", minVersion),
		}
	}
	return checkResult{label: label,
		detail: fmt.Sprintf("%s >= %s", cliVersionStr, minVersion), status: "ok"}
}

// shortSHA returns the first 8 characters of a git SHA for display.
func shortSHA(sha string) string {
	if len(sha) < 8 {
		return sha
	}
	return sha[:8]
}

// shortHash returns the first 12 characters of a hex hash for display.
func shortHash(h string) string {
	if len(h) < 12 {
		return h
	}
	return h[:12]
}

// semverLT reports whether semver string a is strictly less than b.
// Both strings may have an optional "v" prefix. Returns false whenever
// either value is "dev" or cannot be parsed — degrades gracefully so
// local dev builds never trip the version check.
func semverLT(a, b string) bool {
	ap := parseSemverParts(a)
	bp := parseSemverParts(b)
	if ap == nil || bp == nil {
		return false
	}
	for i := 0; i < 3; i++ {
		if ap[i] < bp[i] {
			return true
		}
		if ap[i] > bp[i] {
			return false
		}
	}
	return false
}

func parseSemverParts(v string) []int {
	v = strings.TrimPrefix(v, "v")
	if v == "dev" || v == "" {
		return nil
	}
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return nil
	}
	result := make([]int, 3)
	for i, p := range parts {
		if idx := strings.IndexByte(p, '-'); idx >= 0 {
			p = p[:idx]
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		result[i] = n
	}
	return result
}
