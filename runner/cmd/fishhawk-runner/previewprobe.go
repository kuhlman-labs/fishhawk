package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

/*
 * Acceptance target-identity gate + preview provisioning hook
 * (E31.18 / #1569). Acceptance validates the RUNNING INSTANCE at the
 * spec-declared egress.target_hosts — which, without this gate, is
 * whatever build happens to answer there (in the dogfood spec: the
 * orchestrating fishhawkd running current main). Before the acceptance
 * agent spawns, the runner:
 *
 *   provision — when FISHHAWK_ACCEPTANCE_PREVIEW_CMD is set, runs it via
 *               `sh -c` with FISHHAWK_PREVIEW_SHA / FISHHAWK_PREVIEW_TARGET_HOST
 *               in its env so the operator's hook can build+serve the
 *               merge candidate (e.g. `scripts/dev preview`).
 *   readiness — polls probeTargetIdentity until the target serves the
 *               expected head SHA or the ready budget expires.
 *   gate      — verified proceeds; stale/unreachable (and a provision
 *               failure) fail the stage category-C BEFORE any spawn;
 *               unverifiable (no build identifier exposed, or an older
 *               backend that sent no expectation) warns and proceeds,
 *               preserving mixed-version compat.
 *   teardown  — FISHHAWK_ACCEPTANCE_PREVIEW_TEARDOWN_CMD runs best-effort
 *               via defer at the call site, so it covers BOTH the
 *               after-the-verdict-ships happy path and every
 *               post-provision failure return. A teardown failure logs
 *               an event and never changes the stage outcome.
 *
 * The probe dials DIRECT from the runner process, never through the
 * ADR-050 egress proxy — the proxy contains the agent, not the runner.
 * The FISHHAWK_ACCEPTANCE_PREVIEW_* variables are runner-process config
 * and never reach the agent: acceptenv's default-deny allow-list drops
 * them by omission (asserted in acceptance_test.go).
 */

// Pre-spawn category-C failure reasons the gate maps probe/provision
// outcomes onto (main.go emits them as runner_failed events).
const (
	acceptanceReasonTargetStale      = "acceptance_target_stale"
	acceptanceReasonTargetUnreach    = "acceptance_target_unreachable"
	acceptanceReasonProvisionFailed  = "acceptance_preview_provision_failed"
	previewCmdEnv                    = "FISHHAWK_ACCEPTANCE_PREVIEW_CMD"
	previewTeardownCmdEnv            = "FISHHAWK_ACCEPTANCE_PREVIEW_TEARDOWN_CMD"
	previewProvisionTimeoutSecsEnv   = "FISHHAWK_ACCEPTANCE_PREVIEW_TIMEOUT_SECS"
	previewReadyTimeoutSecsEnv       = "FISHHAWK_ACCEPTANCE_PREVIEW_READY_TIMEOUT_SECS"
	defaultPreviewProvisionTimeout   = 300 * time.Second
	defaultPreviewReadyTimeout       = 60 * time.Second
	defaultPreviewPollInterval       = 2 * time.Second
	defaultPreviewQuickPollInterval  = 500 * time.Millisecond
	defaultPreviewQuickProbeAttempts = 3
)

// probeOutcome is the four-way classification of a target-identity probe.
type probeOutcome int

const (
	// probeUnreachable: no scheme produced an HTTP response at all.
	probeUnreachable probeOutcome = iota
	// probeUnverifiable: the target answered but its build identity
	// cannot be compared — non-200, non-JSON, missing/"unknown"/too-short
	// git_sha, or the backend sent no expected SHA (older backend).
	probeUnverifiable
	// probeStale: the target exposes a git_sha and it is NOT the expected
	// merge candidate — including any '-dirty'-suffixed value (a dirty
	// build is not the committed merge candidate; fail closed).
	probeStale
	// probeVerified: git_sha is a >=7-char prefix of the expected head SHA.
	probeVerified
)

func (o probeOutcome) String() string {
	switch o {
	case probeVerified:
		return "verified"
	case probeStale:
		return "stale"
	case probeUnverifiable:
		return "unverifiable"
	default:
		return "unreachable"
	}
}

// probeResult carries the classified outcome plus a precise detail string
// (expected vs got, URL probed) and the observed git_sha when present.
type probeResult struct {
	outcome probeOutcome
	detail  string
	gitSHA  string
}

// probeHTTPClient dials the /healthz probe DIRECT (Proxy explicitly nil:
// the runner process must never route its own probe through the agent's
// egress proxy, and must ignore any ambient proxy env). Package var so
// tests can substitute a client that trusts an httptest TLS certificate.
var probeHTTPClient = &http.Client{
	Timeout:   5 * time.Second,
	Transport: &http.Transport{Proxy: nil},
}

// probeSchemeOrder returns the scheme attempt order for a declared egress
// host (the grammar carries no scheme): http first for loopback and
// IP-literal hosts (the dev loop), https first otherwise. The probe always
// falls back to the other scheme.
func probeSchemeOrder(host string) []string {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.Trim(name, "[]")
	if strings.EqualFold(name, "localhost") || net.ParseIP(name) != nil {
		return []string{"http", "https"}
	}
	return []string{"https", "http"}
}

// probeTargetIdentity GETs <scheme>://<host>/healthz, decodes the JSON
// body's git_sha build identifier, and classifies the target against
// expectedSHA. Both schemes are attempted in probeSchemeOrder and the
// most informative outcome wins (verified > stale > unverifiable >
// unreachable), so a wrong-scheme artifact (a TLS listener answering a
// plain-HTTP probe with 400, or a handshake failure) never masks the
// other scheme's real answer.
func probeTargetIdentity(ctx context.Context, host, expectedSHA string) probeResult {
	if expectedSHA == "" {
		return probeResult{
			outcome: probeUnverifiable,
			detail:  "backend sent no expected head SHA (older backend or ledger resolution failure); target identity not verifiable",
		}
	}

	best := probeResult{
		outcome: probeUnreachable,
		detail:  fmt.Sprintf("no scheme reached host %q", host),
	}
	for _, scheme := range probeSchemeOrder(host) {
		res := probeOnce(ctx, scheme+"://"+host+"/healthz", expectedSHA)
		if res.outcome == probeVerified {
			return res
		}
		if res.outcome > best.outcome {
			best = res
		}
	}
	return best
}

// probeOnce performs a single-scheme /healthz probe and classifies it.
func probeOnce(ctx context.Context, url, expectedSHA string) probeResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return probeResult{outcome: probeUnreachable,
			detail: fmt.Sprintf("build probe request for %s: %v", url, err)}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := probeHTTPClient.Do(req)
	if err != nil {
		return probeResult{outcome: probeUnreachable,
			detail: fmt.Sprintf("probe %s: %v", url, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return probeResult{outcome: probeUnverifiable,
			detail: fmt.Sprintf("probe %s: status %d, build identity not verifiable", url, resp.StatusCode)}
	}
	var body struct {
		GitSHA string `json:"git_sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return probeResult{outcome: probeUnverifiable,
			detail: fmt.Sprintf("probe %s: non-JSON healthz body (%v), build identity not verifiable", url, err)}
	}
	got := strings.TrimSpace(body.GitSHA)
	switch {
	case got == "" || got == "unknown":
		return probeResult{outcome: probeUnverifiable, gitSHA: got,
			detail: fmt.Sprintf("probe %s: healthz exposes no git_sha build identifier", url)}
	case strings.HasSuffix(got, "-dirty"):
		// A dirty build is NOT the committed merge candidate even when its
		// prefix matches — fail closed.
		return probeResult{outcome: probeStale, gitSHA: got,
			detail: fmt.Sprintf("probe %s: git_sha %q is a dirty build, not the committed merge candidate (expected %s)", url, got, expectedSHA)}
	case len(got) < 7:
		return probeResult{outcome: probeUnverifiable, gitSHA: got,
			detail: fmt.Sprintf("probe %s: git_sha %q too short to verify (need >=7 chars)", url, got)}
	case strings.HasPrefix(expectedSHA, got):
		return probeResult{outcome: probeVerified, gitSHA: got,
			detail: fmt.Sprintf("probe %s: git_sha %q matches expected head %s", url, got, expectedSHA)}
	default:
		return probeResult{outcome: probeStale, gitSHA: got,
			detail: fmt.Sprintf("probe %s: expected head %s, got git_sha %q — target serves a different build", url, expectedSHA, got)}
	}
}

// previewGateConfig is the operator-configurable knob set for the gate.
// Read from the runner-process env in production (previewGateConfigFromEnv);
// tests inject the struct directly to bound timing.
type previewGateConfig struct {
	provisionCmd     string
	teardownCmd      string
	provisionTimeout time.Duration
	readyTimeout     time.Duration
	// pollInterval paces the readiness poll after a provision command.
	pollInterval time.Duration
	// quickInterval / quickAttempts bound the no-provision-command probe:
	// a fixed target either answers or it doesn't, so the gate only
	// absorbs connection blips rather than waiting a full boot budget.
	quickInterval time.Duration
	quickAttempts int
}

// previewGateConfigFromEnv reads the FISHHAWK_ACCEPTANCE_PREVIEW_* knobs
// off the runner-process env. These are runner-deployment config, never
// agent env — acceptenv's default-deny allow-list excludes them.
func previewGateConfigFromEnv() previewGateConfig {
	return previewGateConfig{
		provisionCmd:     os.Getenv(previewCmdEnv),
		teardownCmd:      os.Getenv(previewTeardownCmdEnv),
		provisionTimeout: envSeconds(previewProvisionTimeoutSecsEnv, defaultPreviewProvisionTimeout),
		readyTimeout:     envSeconds(previewReadyTimeoutSecsEnv, defaultPreviewReadyTimeout),
		pollInterval:     defaultPreviewPollInterval,
		quickInterval:    defaultPreviewQuickPollInterval,
		quickAttempts:    defaultPreviewQuickProbeAttempts,
	}
}

// envSeconds parses an integer-seconds env var, falling back to def on
// unset, unparsable, or non-positive values.
func envSeconds(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return time.Duration(n) * time.Second
}

// runPreviewCommand executes an operator hook via `sh -c` in the runner's
// cwd with a CREDENTIAL-STRIPPED env plus FISHHAWK_PREVIEW_SHA /
// FISHHAWK_PREVIEW_TARGET_HOST. On a non-zero exit or timeout it returns an
// error carrying the exit state and an output tail.
//
// The provisioning hook (in the dogfood spec, `scripts/dev preview`) builds
// and RUNS the merge candidate's fishhawkd (`migrate up` / `serve`) — i.e.
// untrusted, committed, agent-authored code — before the ADR-050 acceptance
// proxy contains anything. If that hook inherited os.Environ() it would run
// branch code while holding the runner's secrets (FISHHAWK_GITHUB_TOKEN /
// GITHUB_TOKEN / GH_TOKEN, ANTHROPIC_API_KEY / OPENAI_API_KEY,
// FISHHAWK_API_TOKEN) with ordinary network egress — the lethal-trifecta
// shape. sanitizedGateEnv() (ADR-029 item 4, shared with the compile/test/
// verify gates) applies the default-deny allow-list, so those secrets never
// reach the hook or the branch binary it spawns while PATH/HOME/GO* survive
// for the Go build.
func runPreviewCommand(ctx context.Context, command, expectedSHA, host string, timeout time.Duration) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	cmd.Env = append(sanitizedGateEnv(),
		"FISHHAWK_PREVIEW_SHA="+expectedSHA,
		"FISHHAWK_PREVIEW_TARGET_HOST="+host,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if cctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("timed out after %s: output tail: %s", timeout, outputTail(out))
	}
	return fmt.Errorf("%v: output tail: %s", err, outputTail(out))
}

// outputTail returns the last 2KiB of a hook's combined output so a
// failure event carries the actionable end of the log, bounded.
func outputTail(out []byte) string {
	const tail = 2048
	s := strings.TrimSpace(string(out))
	if len(s) > tail {
		s = "…" + s[len(s)-tail:]
	}
	return s
}

// acceptanceTargetGate runs provision → readiness → identity gate for the
// acceptance stage's first declared egress target host. Returns:
//
//	teardown   — non-nil when a teardown command must run; the CALLER
//	             defers it immediately, so it covers both the
//	             after-the-verdict-ships happy path and EVERY
//	             post-provision failure return (binding condition of the
//	             approved plan). Best-effort: failures are logged
//	             (acceptance_preview_teardown_failed), never propagated.
//	failReason — "" to proceed with the spawn; otherwise one of the
//	             acceptanceReason* constants, failed category-C pre-spawn
//	             by the caller with failDetail.
//
// No declared hosts skips the gate entirely (the existing not-declared
// prompt behavior is preserved). An empty expectedSHA (older backend, or
// backend-side ledger resolution failure) warns acceptance_target_unverified
// and proceeds without provisioning — the gate never hard-fails a stage on
// a missing expectation.
func acceptanceTargetGate(ctx context.Context, gcfg previewGateConfig, targetHosts []string, expectedSHA, runID string, logSink io.Writer) (teardown func(), failReason, failDetail string) {
	if len(targetHosts) == 0 {
		return nil, "", ""
	}
	host := targetHosts[0]

	if expectedSHA == "" {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"acceptance_target_unverified","run_id":%q,"host":%q,"reason":%q}`+"\n",
			runID, host, "backend sent no expected head SHA; proceeding unverified")
		return nil, "", ""
	}

	if gcfg.teardownCmd != "" {
		teardown = func() {
			if err := runPreviewCommand(ctx, gcfg.teardownCmd, expectedSHA, host, gcfg.provisionTimeout); err != nil {
				_, _ = fmt.Fprintf(logSink,
					`{"event":"acceptance_preview_teardown_failed","run_id":%q,"host":%q,"detail":%q}`+"\n",
					runID, host, err.Error())
			}
		}
	}

	if gcfg.provisionCmd != "" {
		if err := runPreviewCommand(ctx, gcfg.provisionCmd, expectedSHA, host, gcfg.provisionTimeout); err != nil {
			return teardown, acceptanceReasonProvisionFailed,
				fmt.Sprintf("preview provision command failed: %v", err)
		}
		_, _ = fmt.Fprintf(logSink,
			`{"event":"acceptance_preview_provisioned","run_id":%q,"host":%q,"expected_sha":%q}`+"\n",
			runID, host, expectedSHA)
	}

	res := awaitTargetReady(ctx, gcfg, host, expectedSHA)
	switch res.outcome {
	case probeVerified:
		_, _ = fmt.Fprintf(logSink,
			`{"event":"acceptance_target_verified","run_id":%q,"host":%q,"git_sha":%q}`+"\n",
			runID, host, res.gitSHA)
		return teardown, "", ""
	case probeUnverifiable:
		// The issue's probe-verified clause applies only where the target
		// exposes a build identifier — warn loudly and proceed.
		_, _ = fmt.Fprintf(logSink,
			`{"event":"acceptance_target_unverified","run_id":%q,"host":%q,"reason":%q}`+"\n",
			runID, host, res.detail)
		return teardown, "", ""
	case probeStale:
		return teardown, acceptanceReasonTargetStale, res.detail
	default:
		return teardown, acceptanceReasonTargetUnreach, res.detail
	}
}

// awaitTargetReady polls probeTargetIdentity until verified or the budget
// expires, returning the LAST probe result for outcome mapping. With a
// provision command the budget is readyTimeout (the preview instance is
// booting — stale/unreachable/unverifiable can all be transient startup
// states). Without one the target is a fixed instance: retry only
// connection failures, quickAttempts times, so definitive answers gate
// immediately.
func awaitTargetReady(ctx context.Context, gcfg previewGateConfig, host, expectedSHA string) probeResult {
	if gcfg.provisionCmd == "" {
		res := probeTargetIdentity(ctx, host, expectedSHA)
		for attempt := 1; res.outcome == probeUnreachable && attempt < gcfg.quickAttempts; attempt++ {
			if !sleepCtx(ctx, gcfg.quickInterval) {
				return res
			}
			res = probeTargetIdentity(ctx, host, expectedSHA)
		}
		return res
	}

	deadline := time.Now().Add(gcfg.readyTimeout)
	res := probeTargetIdentity(ctx, host, expectedSHA)
	for res.outcome != probeVerified && time.Now().Add(gcfg.pollInterval).Before(deadline) {
		if !sleepCtx(ctx, gcfg.pollInterval) {
			return res
		}
		res = probeTargetIdentity(ctx, host, expectedSHA)
	}
	if res.outcome != probeVerified {
		res.detail = fmt.Sprintf("preview not ready within %s: %s", gcfg.readyTimeout, res.detail)
	}
	return res
}

// sleepCtx sleeps for d unless ctx is done first; reports whether the
// full sleep elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
