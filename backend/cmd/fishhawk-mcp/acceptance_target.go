package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

/*
 * Verb-side acceptance target-identity gate (E48.6 / #1953).
 *
 * When the acceptance-admission endpoint reports needs_target (the approved
 * plan needs LIVE validation and the spec declares egress target hosts), a
 * dispatch verb must not spawn a runner that would immediately fail
 * category-C acceptance_target_unreachable (evidence run 27da3ecc). Instead
 * the verb probes the first declared target host FROM THE DISPATCH HOST — the
 * same network position the local runner would probe from — using the
 * runner's identity-probe semantics, and refuses to spawn when the target is
 * unreachable or stale, leaving the stage parked at awaiting_host_dispatch for
 * a clean re-dispatch after the operator provisions the target.
 *
 * This intentionally DUPLICATES the runner's classification
 * (runner/cmd/fishhawk-runner/previewprobe.go — a separate Go module,
 * package main, not importable). The classification table is mirrored in
 * acceptance_target_test.go; a semantic divergence fails that table.
 */

// acceptancePreviewCmdEnv is the operator hook that, when set in the verb's
// environment, means the SPAWNED runner will provision the target itself
// (#1569) — the runner inherits os.Environ() from the verb (run_stage.go /
// dispatch_stage.go / drive_run.go each spawn with append(os.Environ(), …)), so
// a value visible here is guaranteed visible to the runner. The gate then
// proceeds to spawn WITHOUT probing.
const acceptancePreviewCmdEnv = "FISHHAWK_ACCEPTANCE_PREVIEW_CMD"

// acceptanceProbeHTTPClient dials the /healthz probe DIRECT (Proxy explicitly
// nil: the verb must never route its own probe through an ambient operator
// proxy, matching the runner's direct-dial probe so a proxy cannot fake
// reachability). Package var so tests can substitute a client that trusts an
// httptest server.
//
// Redirects are REFUSED (CheckRedirect returns http.ErrUseLastResponse): a
// spec-declared or compromised target must not be able to redirect the dispatch
// host's probe to an arbitrary internal/external URL, widening egress from a
// host that runs repository-controlled workflows. Leaving the 3xx response
// unfollowed means a redirect at the declared target classifies as unverifiable
// (non-200) rather than chasing the Location header off-target.
var acceptanceProbeHTTPClient = &http.Client{
	Timeout:   5 * time.Second,
	Transport: &http.Transport{Proxy: nil},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// acceptanceQuickProbeAttempts / acceptanceQuickPollInterval bound the probe:
// a fixed target either answers or it doesn't, so the gate only absorbs
// connection blips on the UNREACHABLE outcome rather than waiting a boot
// budget. Package vars so tests can shrink them to run without wall-clock waits.
var (
	acceptanceQuickProbeAttempts = 3
	acceptanceQuickPollInterval  = 500 * time.Millisecond
)

// acceptanceProbeOutcome is the four-way classification of a target-identity
// probe, mirroring the runner's probeOutcome. Ordered unreachable <
// unverifiable < stale < verified so the most-informative scheme attempt wins.
type acceptanceProbeOutcome int

const (
	acceptanceProbeUnreachable acceptanceProbeOutcome = iota
	acceptanceProbeUnverifiable
	acceptanceProbeStale
	acceptanceProbeVerified
)

func (o acceptanceProbeOutcome) String() string {
	switch o {
	case acceptanceProbeVerified:
		return "verified"
	case acceptanceProbeStale:
		return "stale"
	case acceptanceProbeUnverifiable:
		return "unverifiable"
	default:
		return "unreachable"
	}
}

// acceptanceProbeResult carries the classified outcome plus a precise detail
// string (expected vs got, URL probed) and the observed git_sha when present.
type acceptanceProbeResult struct {
	outcome acceptanceProbeOutcome
	detail  string
	gitSHA  string
}

// AcceptanceNeedsTarget is the structured pre-spawn refusal a dispatch verb
// returns when the acceptance target is unreachable or stale (E48.6 / #1953).
// It names the host to bring up and the head SHA it must serve so the operator
// can provision and re-dispatch cleanly — the stage stays parked at
// awaiting_host_dispatch (the refusal fires BEFORE any spawn evidence).
type AcceptanceNeedsTarget struct {
	TargetHost      string `json:"target_host" jsonschema:"the spec-declared acceptance egress target host that must serve the merge candidate before the runner can spawn"`
	ExpectedHeadSHA string `json:"expected_head_sha" jsonschema:"the merge-candidate head SHA the target's /healthz git_sha must match; empty when the backend could not resolve it"`
	Detail          string `json:"detail" jsonschema:"the probe classification detail (unreachable or stale, with the URL probed and the observed git_sha)"`
	Remediation     string `json:"remediation" jsonschema:"the operator action that unblocks the dispatch: bring up the target at the expected head SHA, then re-dispatch"`
}

// acceptanceProbeSchemeOrder returns the scheme attempt order for a declared
// egress host (the grammar carries no scheme): http first for loopback and
// IP-literal hosts (the dev loop), https first otherwise. The probe always
// falls back to the other scheme. Byte-mirrors the runner's probeSchemeOrder.
func acceptanceProbeSchemeOrder(host string) []string {
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

// probeAcceptanceTargetIdentity GETs <scheme>://<host>/healthz, decodes the
// JSON body's git_sha build identifier, and classifies the target against
// expectedSHA. Both schemes are attempted in acceptanceProbeSchemeOrder and the
// most informative outcome wins (verified > stale > unverifiable >
// unreachable). Mirrors the runner's probeTargetIdentity.
func probeAcceptanceTargetIdentity(ctx context.Context, host, expectedSHA string) acceptanceProbeResult {
	if expectedSHA == "" {
		return acceptanceProbeResult{
			outcome: acceptanceProbeUnverifiable,
			detail:  "backend sent no expected head SHA (older backend or ledger resolution failure); target identity not verifiable",
		}
	}
	best := acceptanceProbeResult{
		outcome: acceptanceProbeUnreachable,
		detail:  fmt.Sprintf("no scheme reached host %q", host),
	}
	for _, scheme := range acceptanceProbeSchemeOrder(host) {
		res := acceptanceProbeOnce(ctx, scheme+"://"+host+"/healthz", expectedSHA)
		if res.outcome == acceptanceProbeVerified {
			return res
		}
		if res.outcome > best.outcome {
			best = res
		}
	}
	return best
}

// acceptanceProbeOnce performs a single-scheme /healthz probe and classifies
// it, mirroring the runner's probeOnce: a non-200 or non-JSON body is
// unverifiable; a missing/"unknown" or <7-char git_sha is unverifiable; a
// '-dirty'-suffixed git_sha is stale (fail closed); a >=7-char prefix of the
// expected head is verified; any other git_sha is stale.
func acceptanceProbeOnce(ctx context.Context, url, expectedSHA string) acceptanceProbeResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return acceptanceProbeResult{outcome: acceptanceProbeUnreachable,
			detail: fmt.Sprintf("build probe request for %s: %v", url, err)}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := acceptanceProbeHTTPClient.Do(req)
	if err != nil {
		return acceptanceProbeResult{outcome: acceptanceProbeUnreachable,
			detail: fmt.Sprintf("probe %s: %v", url, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return acceptanceProbeResult{outcome: acceptanceProbeUnverifiable,
			detail: fmt.Sprintf("probe %s: status %d, build identity not verifiable", url, resp.StatusCode)}
	}
	var body struct {
		GitSHA string `json:"git_sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return acceptanceProbeResult{outcome: acceptanceProbeUnverifiable,
			detail: fmt.Sprintf("probe %s: non-JSON healthz body (%v), build identity not verifiable", url, err)}
	}
	got := strings.TrimSpace(body.GitSHA)
	switch {
	case got == "" || got == "unknown":
		return acceptanceProbeResult{outcome: acceptanceProbeUnverifiable, gitSHA: got,
			detail: fmt.Sprintf("probe %s: healthz exposes no git_sha build identifier", url)}
	case strings.HasSuffix(got, "-dirty"):
		// A dirty build is NOT the committed merge candidate even when its
		// prefix matches — fail closed.
		return acceptanceProbeResult{outcome: acceptanceProbeStale, gitSHA: got,
			detail: fmt.Sprintf("probe %s: git_sha %q is a dirty build, not the committed merge candidate (expected %s)", url, got, expectedSHA)}
	case len(got) < 7:
		return acceptanceProbeResult{outcome: acceptanceProbeUnverifiable, gitSHA: got,
			detail: fmt.Sprintf("probe %s: git_sha %q too short to verify (need >=7 chars)", url, got)}
	case strings.HasPrefix(expectedSHA, got):
		return acceptanceProbeResult{outcome: acceptanceProbeVerified, gitSHA: got,
			detail: fmt.Sprintf("probe %s: git_sha %q matches expected head %s", url, got, expectedSHA)}
	default:
		return acceptanceProbeResult{outcome: acceptanceProbeStale, gitSHA: got,
			detail: fmt.Sprintf("probe %s: expected head %s, got git_sha %q — target serves a different build", url, expectedSHA, got)}
	}
}

// awaitAcceptanceTargetReady probes the target once, retrying only the
// UNREACHABLE outcome up to acceptanceQuickProbeAttempts (connection blips), so
// a definitive stale/unverifiable/verified answer gates immediately. Mirrors
// the runner's no-provision-command awaitTargetReady branch.
func awaitAcceptanceTargetReady(ctx context.Context, host, expectedSHA string) acceptanceProbeResult {
	res := probeAcceptanceTargetIdentity(ctx, host, expectedSHA)
	for attempt := 1; res.outcome == acceptanceProbeUnreachable && attempt < acceptanceQuickProbeAttempts; attempt++ {
		if !acceptanceSleepCtx(ctx, acceptanceQuickPollInterval) {
			return res
		}
		res = probeAcceptanceTargetIdentity(ctx, host, expectedSHA)
	}
	return res
}

// acceptanceSleepCtx sleeps for d unless ctx is done first; reports whether the
// full sleep elapsed.
func acceptanceSleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// checkAcceptanceTarget is the verb-side pre-spawn gate (E48.6 / #1953). Given a
// needs_target admission result, it decides whether to PROCEED to spawn (nil
// refusal) — optionally with a warning — or to REFUSE (non-nil
// *AcceptanceNeedsTarget) so the caller returns a needs_target signal without
// recording spawn evidence or spawning.
//
// Proceed paths (nil refusal):
//   - admission nil / !NeedsTarget / no declared hosts: not a needs_target
//     result, proceed silently.
//   - FISHHAWK_ACCEPTANCE_PREVIEW_CMD set: the spawned runner inherits it and
//     provisions the target itself (#1569) — proceed with an informational note.
//   - empty ExpectedHeadSHA: the backend could not resolve the expectation;
//     mirror the runner gate's never-hard-fail-on-missing-expectation early
//     return — proceed with a warning.
//   - probe VERIFIED: the target serves the merge candidate — proceed.
//   - probe UNVERIFIABLE: the target answered but exposes no comparable build
//     identity — proceed with the probe detail as a warning.
//
// Refuse paths (non-nil refusal): probe STALE or UNREACHABLE — the target is up
// on the wrong build or not up at all; refuse so no doomed runner spawns.
func (r *runResolver) checkAcceptanceTarget(ctx context.Context, admission *AcceptanceAdmissionResult) (refusal *AcceptanceNeedsTarget, warning string) {
	if admission == nil || !admission.NeedsTarget || len(admission.TargetHosts) == 0 {
		return nil, ""
	}
	host := admission.TargetHosts[0]

	if r.getenv(acceptancePreviewCmdEnv) != "" {
		return nil, fmt.Sprintf(
			"acceptance target %q needs the merge candidate (expected head %s); %s is set so the spawned runner will provision it (#1569).",
			host, admission.ExpectedHeadSHA, acceptancePreviewCmdEnv)
	}

	if admission.ExpectedHeadSHA == "" {
		return nil, fmt.Sprintf(
			"acceptance target %q is declared but the backend sent no expected head SHA (older backend or ledger resolution failure); proceeding to spawn unverified.",
			host)
	}

	res := awaitAcceptanceTargetReady(ctx, host, admission.ExpectedHeadSHA)
	switch res.outcome {
	case acceptanceProbeVerified:
		return nil, ""
	case acceptanceProbeUnverifiable:
		return nil, fmt.Sprintf(
			"acceptance target %q identity not verifiable (%s); proceeding to spawn.", host, res.detail)
	default: // stale or unreachable
		return &AcceptanceNeedsTarget{
			TargetHost:      host,
			ExpectedHeadSHA: admission.ExpectedHeadSHA,
			Detail:          res.detail,
			Remediation: fmt.Sprintf(
				"bring up the acceptance target at head %s (e.g. scripts/dev preview), then re-dispatch", admission.ExpectedHeadSHA),
		}, ""
	}
}
