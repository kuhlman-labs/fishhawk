package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testExpectedSHA = "abc1234def5678901234567890123456789012ab"

// healthzServer serves a fixed /healthz body and counts hits.
func healthzServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts, &hits
}

// hostOf strips the scheme off an httptest server URL, yielding the
// scheme-less egress-host grammar the probe consumes.
func hostOf(ts *httptest.Server) string {
	return strings.TrimPrefix(strings.TrimPrefix(ts.URL, "http://"), "https://")
}

// closedPortHost returns a loopback host:port with nothing listening.
func closedPortHost(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := l.Addr().String()
	_ = l.Close()
	return host
}

// (a) matching git_sha → verified.
func TestProbeTargetIdentity_Verified(t *testing.T) {
	ts, _ := healthzServer(t, 200, `{"git_sha":"abc1234"}`)
	res := probeTargetIdentity(context.Background(), hostOf(ts), testExpectedSHA)
	if res.outcome != probeVerified {
		t.Fatalf("outcome = %s (%s), want verified", res.outcome, res.detail)
	}
	if res.gitSHA != "abc1234" {
		t.Errorf("gitSHA = %q, want abc1234", res.gitSHA)
	}
}

// (b) mismatched git_sha → stale, detail carries expected vs got + URL.
func TestProbeTargetIdentity_StaleMismatch(t *testing.T) {
	ts, _ := healthzServer(t, 200, `{"git_sha":"1111111"}`)
	res := probeTargetIdentity(context.Background(), hostOf(ts), testExpectedSHA)
	if res.outcome != probeStale {
		t.Fatalf("outcome = %s (%s), want stale", res.outcome, res.detail)
	}
	for _, want := range []string{testExpectedSHA, `"1111111"`, ts.URL} {
		if !strings.Contains(res.detail, want) {
			t.Errorf("detail %q missing %q", res.detail, want)
		}
	}
}

// (c) '-dirty'-suffixed git_sha → stale even when the prefix matches:
// a dirty build is not the committed merge candidate (fail closed).
func TestProbeTargetIdentity_StaleDirty(t *testing.T) {
	ts, _ := healthzServer(t, 200, `{"git_sha":"abc1234-dirty"}`)
	res := probeTargetIdentity(context.Background(), hostOf(ts), testExpectedSHA)
	if res.outcome != probeStale {
		t.Fatalf("outcome = %s (%s), want stale for a dirty build", res.outcome, res.detail)
	}
	if !strings.Contains(res.detail, "dirty") {
		t.Errorf("detail %q should name the dirty build", res.detail)
	}
}

// (d) connection refused on both schemes → unreachable.
func TestProbeTargetIdentity_Unreachable(t *testing.T) {
	res := probeTargetIdentity(context.Background(), closedPortHost(t), testExpectedSHA)
	if res.outcome != probeUnreachable {
		t.Fatalf("outcome = %s (%s), want unreachable", res.outcome, res.detail)
	}
}

// (e) reachable but no comparable build identity → unverifiable, per shape.
func TestProbeTargetIdentity_Unverifiable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"missing git_sha field", 200, `{"status":"ok"}`},
		{"unknown git_sha", 200, `{"git_sha":"unknown"}`},
		{"empty git_sha", 200, `{"git_sha":""}`},
		{"too-short git_sha", 200, `{"git_sha":"abc12"}`},
		{"non-JSON body", 200, `<html>ok</html>`},
		{"non-200 status", 503, `{"git_sha":"abc1234"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := healthzServer(t, tc.status, tc.body)
			res := probeTargetIdentity(context.Background(), hostOf(ts), testExpectedSHA)
			if res.outcome != probeUnverifiable {
				t.Fatalf("outcome = %s (%s), want unverifiable", res.outcome, res.detail)
			}
		})
	}
}

// (f) empty expected SHA (older backend) → unverifiable without dialing.
func TestProbeTargetIdentity_EmptyExpectedSHA(t *testing.T) {
	ts, hits := healthzServer(t, 200, `{"git_sha":"abc1234"}`)
	res := probeTargetIdentity(context.Background(), hostOf(ts), "")
	if res.outcome != probeUnverifiable {
		t.Fatalf("outcome = %s (%s), want unverifiable on empty expectation", res.outcome, res.detail)
	}
	if hits.Load() != 0 {
		t.Errorf("probe dialed the target %d times despite no expectation", hits.Load())
	}
}

// (g) scheme order: http first for loopback/IP-literal hosts, https first
// otherwise.
func TestProbeSchemeOrder(t *testing.T) {
	cases := []struct {
		host  string
		first string
	}{
		{"localhost:8090", "http"},
		{"127.0.0.1:8080", "http"},
		{"[::1]:8080", "http"},
		{"10.0.0.7", "http"},
		{"preview.internal:8443", "https"},
		{"app.fishhawk.example", "https"},
	}
	for _, tc := range cases {
		order := probeSchemeOrder(tc.host)
		if len(order) != 2 || order[0] == order[1] {
			t.Fatalf("%s: order = %v, want both schemes once", tc.host, order)
		}
		if order[0] != tc.first {
			t.Errorf("%s: first scheme = %s, want %s", tc.host, order[0], tc.first)
		}
	}
}

// (g) fallback behavior: an IP-literal host tries http first; when the
// listener is TLS the plain-HTTP attempt cannot verify, and the https
// fallback must win with the real answer.
func TestProbeTargetIdentity_HTTPSFallback(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"git_sha":"abc1234"}`)
	}))
	t.Cleanup(ts.Close)

	orig := probeHTTPClient
	probeHTTPClient = ts.Client() // trusts the httptest certificate
	t.Cleanup(func() { probeHTTPClient = orig })

	res := probeTargetIdentity(context.Background(), hostOf(ts), testExpectedSHA)
	if res.outcome != probeVerified {
		t.Fatalf("outcome = %s (%s), want verified via https fallback", res.outcome, res.detail)
	}
	if !strings.Contains(res.detail, "https://") {
		t.Errorf("detail %q should carry the https URL that verified", res.detail)
	}
}

// --- provision / teardown command execution ---------------------------------

// markerPath returns a temp path a hook command can write to prove it ran.
func markerPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "marker")
}

func TestRunPreviewCommand_EnvAndSuccess(t *testing.T) {
	marker := markerPath(t)
	cmd := `printf '%s %s' "$FISHHAWK_PREVIEW_SHA" "$FISHHAWK_PREVIEW_TARGET_HOST" > ` + marker
	if err := runPreviewCommand(context.Background(), cmd, testExpectedSHA, "localhost:8090", 30*time.Second); err != nil {
		t.Fatalf("runPreviewCommand: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if want := testExpectedSHA + " localhost:8090"; string(got) != want {
		t.Errorf("hook env = %q, want %q", got, want)
	}
}

// (g2) The hook — which builds and runs untrusted merge-candidate code — must
// NOT inherit the runner's secrets (#1569 security fixup): runPreviewCommand
// hands it the default-deny sanitizedGateEnv, so a runner secret (whether on
// the explicit denylist or dropped by omission) is absent while PATH survives
// for the Go build the hook performs.
func TestRunPreviewCommand_StripsRunnerSecrets(t *testing.T) {
	t.Setenv("FISHHAWK_API_TOKEN", "denylisted-secret") // explicit denylist
	t.Setenv("SOME_UNLISTED_SECRET", "omitted-secret")  // dropped by default-deny
	marker := markerPath(t)
	cmd := `printf '%s|%s|%s' "$FISHHAWK_API_TOKEN" "$SOME_UNLISTED_SECRET" "$PATH" > ` + marker
	if err := runPreviewCommand(context.Background(), cmd, testExpectedSHA, "h", 30*time.Second); err != nil {
		t.Fatalf("runPreviewCommand: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.SplitN(string(got), "|", 3)
	if len(fields) != 3 {
		t.Fatalf("marker = %q, want three |-separated fields", got)
	}
	if fields[0] != "" || fields[1] != "" {
		t.Errorf("hook saw runner secrets FISHHAWK_API_TOKEN=%q SOME_UNLISTED_SECRET=%q, want both empty", fields[0], fields[1])
	}
	if fields[2] == "" {
		t.Error("hook PATH is empty; the Go build the hook runs would break")
	}
}

// (h) non-zero exit → error carrying exit code and output tail.
func TestRunPreviewCommand_NonZeroExit(t *testing.T) {
	err := runPreviewCommand(context.Background(), "echo boom-tail; exit 3", testExpectedSHA, "h", 30*time.Second)
	if err == nil {
		t.Fatal("want error on exit 3")
	}
	if !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "boom-tail") {
		t.Errorf("error %q must carry exit code and output tail", err)
	}
}

func TestRunPreviewCommand_Timeout(t *testing.T) {
	err := runPreviewCommand(context.Background(), "sleep 5", testExpectedSHA, "h", 100*time.Millisecond)
	if err == nil {
		t.Fatal("want error on timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q should name the timeout", err)
	}
}

// --- acceptanceTargetGate ----------------------------------------------------

// fastGateConfig bounds every gate timing for tests.
func fastGateConfig() previewGateConfig {
	return previewGateConfig{
		provisionTimeout: 10 * time.Second,
		readyTimeout:     2 * time.Second,
		pollInterval:     50 * time.Millisecond,
		quickInterval:    20 * time.Millisecond,
		quickAttempts:    3,
	}
}

// No declared hosts → the gate skips entirely: no probe, no provision, no
// teardown, proceed.
func TestAcceptanceTargetGate_NoHostsSkips(t *testing.T) {
	gcfg := fastGateConfig()
	gcfg.provisionCmd = "echo should-not-run > " + markerPath(t)
	var log strings.Builder
	teardown, reason, _ := acceptanceTargetGate(context.Background(), gcfg, nil, testExpectedSHA, "run-1", &log)
	if reason != "" || teardown != nil {
		t.Fatalf("gate must skip on zero hosts, got reason %q teardown %v", reason, teardown != nil)
	}
	if log.Len() != 0 {
		t.Errorf("skip must be silent, got %q", log.String())
	}
}

// Empty expected SHA → warn acceptance_target_unverified, proceed, and do
// NOT provision (there is no candidate identity to build).
func TestAcceptanceTargetGate_EmptyExpectedSHA_WarnsProceedsNoProvision(t *testing.T) {
	marker := markerPath(t)
	gcfg := fastGateConfig()
	gcfg.provisionCmd = "echo ran > " + marker
	var log strings.Builder
	teardown, reason, _ := acceptanceTargetGate(context.Background(), gcfg, []string{"localhost:1"}, "", "run-1", &log)
	if reason != "" {
		t.Fatalf("empty expectation must proceed, got %q", reason)
	}
	if teardown != nil {
		t.Error("no provision happened; no teardown must be returned")
	}
	if !strings.Contains(log.String(), `"event":"acceptance_target_unverified"`) {
		t.Errorf("missing acceptance_target_unverified warn: %s", log.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("provision command must not run without an expected SHA")
	}
}

// Provision failure → acceptance_preview_provision_failed; the teardown
// closure is still returned so the caller's defer cleans up partial state.
func TestAcceptanceTargetGate_ProvisionFailure(t *testing.T) {
	marker := markerPath(t)
	gcfg := fastGateConfig()
	gcfg.provisionCmd = "echo provision-tail; exit 9"
	gcfg.teardownCmd = "echo torn > " + marker
	var log strings.Builder
	teardown, reason, detail := acceptanceTargetGate(context.Background(), gcfg, []string{"localhost:1"}, testExpectedSHA, "run-1", &log)
	if reason != acceptanceReasonProvisionFailed {
		t.Fatalf("reason = %q, want %q", reason, acceptanceReasonProvisionFailed)
	}
	if !strings.Contains(detail, "exit status 9") || !strings.Contains(detail, "provision-tail") {
		t.Errorf("detail %q must carry exit code and output tail", detail)
	}
	if teardown == nil {
		t.Fatal("teardown must be returned on provision failure for the caller's defer")
	}
	teardown()
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("teardown did not run: %v", err)
	}
}

// (i) provision ok, target never comes up → readiness timeout maps to
// acceptance_target_unreachable with a not-ready detail; teardown returned.
func TestAcceptanceTargetGate_ReadinessTimeout_Unreachable(t *testing.T) {
	gcfg := fastGateConfig()
	gcfg.readyTimeout = 300 * time.Millisecond
	gcfg.provisionCmd = "true"
	gcfg.teardownCmd = "true"
	var log strings.Builder
	teardown, reason, detail := acceptanceTargetGate(context.Background(), gcfg, []string{closedPortHost(t)}, testExpectedSHA, "run-1", &log)
	if reason != acceptanceReasonTargetUnreach {
		t.Fatalf("reason = %q (%s), want %q", reason, detail, acceptanceReasonTargetUnreach)
	}
	if !strings.Contains(detail, "not ready within") {
		t.Errorf("detail %q should name the readiness budget", detail)
	}
	if teardown == nil {
		t.Error("teardown must be returned after a successful provision")
	}
}

// Post-provision stale target → acceptance_target_stale; teardown returned.
func TestAcceptanceTargetGate_Stale(t *testing.T) {
	ts, _ := healthzServer(t, 200, `{"git_sha":"2222222"}`)
	gcfg := fastGateConfig()
	gcfg.readyTimeout = 200 * time.Millisecond
	gcfg.provisionCmd = "true"
	gcfg.teardownCmd = "true"
	var log strings.Builder
	teardown, reason, detail := acceptanceTargetGate(context.Background(), gcfg, []string{hostOf(ts)}, testExpectedSHA, "run-1", &log)
	if reason != acceptanceReasonTargetStale {
		t.Fatalf("reason = %q (%s), want %q", reason, detail, acceptanceReasonTargetStale)
	}
	if !strings.Contains(detail, testExpectedSHA) || !strings.Contains(detail, "2222222") {
		t.Errorf("detail %q must carry expected vs got", detail)
	}
	if teardown == nil {
		t.Error("teardown must be returned after a successful provision")
	}
}

// (j) provision ok, target becomes verified within the ready budget after
// transient not-ready answers → proceed with acceptance_target_verified.
func TestAcceptanceTargetGate_VerifiedWithinBudget(t *testing.T) {
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprint(w, `{"git_sha":"abc1234"}`)
	}))
	t.Cleanup(ts.Close)

	gcfg := fastGateConfig()
	gcfg.provisionCmd = "true"
	var log strings.Builder
	_, reason, detail := acceptanceTargetGate(context.Background(), gcfg, []string{hostOf(ts)}, testExpectedSHA, "run-1", &log)
	if reason != "" {
		t.Fatalf("reason = %q (%s), want proceed", reason, detail)
	}
	if !strings.Contains(log.String(), `"event":"acceptance_target_verified"`) ||
		!strings.Contains(log.String(), `"git_sha":"abc1234"`) {
		t.Errorf("missing acceptance_target_verified{git_sha}: %s", log.String())
	}
	if hits.Load() < 3 {
		t.Errorf("readiness poll gave up after %d probes", hits.Load())
	}
}

// Unverifiable target (reachable, no identifier) → warn and proceed.
func TestAcceptanceTargetGate_Unverifiable_WarnsAndProceeds(t *testing.T) {
	ts, _ := healthzServer(t, 200, `{"status":"ok"}`)
	gcfg := fastGateConfig()
	var log strings.Builder
	_, reason, detail := acceptanceTargetGate(context.Background(), gcfg, []string{hostOf(ts)}, testExpectedSHA, "run-1", &log)
	if reason != "" {
		t.Fatalf("reason = %q (%s), want proceed on unverifiable", reason, detail)
	}
	if !strings.Contains(log.String(), `"event":"acceptance_target_unverified"`) {
		t.Errorf("missing acceptance_target_unverified warn: %s", log.String())
	}
}

// No provision command → single-shot identity gate: a definitive stale
// answer gates immediately (no boot-budget polling).
func TestAcceptanceTargetGate_NoProvision_StaleImmediate(t *testing.T) {
	ts, hits := healthzServer(t, 200, `{"git_sha":"3333333"}`)
	gcfg := fastGateConfig()
	var log strings.Builder
	teardown, reason, _ := acceptanceTargetGate(context.Background(), gcfg, []string{hostOf(ts)}, testExpectedSHA, "run-1", &log)
	if reason != acceptanceReasonTargetStale {
		t.Fatalf("reason = %q, want stale", reason)
	}
	if hits.Load() != 1 {
		t.Errorf("definitive stale answer re-probed %d times, want 1", hits.Load())
	}
	if teardown != nil {
		t.Error("no teardown configured; none must be returned")
	}
}

// No provision command + unreachable → the quick-attempt ladder retries
// connection blips (quickAttempts probes) before failing unreachable.
func TestAcceptanceTargetGate_NoProvision_UnreachableAfterQuickAttempts(t *testing.T) {
	gcfg := fastGateConfig()
	var log strings.Builder
	_, reason, _ := acceptanceTargetGate(context.Background(), gcfg, []string{closedPortHost(t)}, testExpectedSHA, "run-1", &log)
	if reason != acceptanceReasonTargetUnreach {
		t.Fatalf("reason = %q, want unreachable", reason)
	}
}

// (l) a failing teardown logs acceptance_preview_teardown_failed and does
// not panic or alter anything else.
func TestAcceptanceTargetGate_TeardownFailureLoggedNonFatal(t *testing.T) {
	ts, _ := healthzServer(t, 200, `{"git_sha":"abc1234"}`)
	gcfg := fastGateConfig()
	gcfg.provisionCmd = "true"
	gcfg.teardownCmd = "echo teardown-went-wrong; exit 1"
	var log strings.Builder
	teardown, reason, detail := acceptanceTargetGate(context.Background(), gcfg, []string{hostOf(ts)}, testExpectedSHA, "run-1", &log)
	if reason != "" {
		t.Fatalf("reason = %q (%s), want proceed", reason, detail)
	}
	if teardown == nil {
		t.Fatal("teardown must be returned")
	}
	teardown()
	if !strings.Contains(log.String(), `"event":"acceptance_preview_teardown_failed"`) ||
		!strings.Contains(log.String(), "teardown-went-wrong") {
		t.Errorf("missing acceptance_preview_teardown_failed with output tail: %s", log.String())
	}
}

// A host that cannot form a request URL (defense-in-depth: the egress
// proxy's allow-list build rejects malformed hosts before the gate runs)
// classifies unreachable rather than panicking.
func TestProbeTargetIdentity_MalformedHost(t *testing.T) {
	res := probeTargetIdentity(context.Background(), "bad host{", testExpectedSHA)
	if res.outcome != probeUnreachable {
		t.Fatalf("outcome = %s (%s), want unreachable", res.outcome, res.detail)
	}
}

// A canceled runner context (SIGTERM chain) aborts the readiness sleep
// immediately instead of waiting out the poll interval.
func TestSleepCtx_CanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if sleepCtx(ctx, 10*time.Second) {
		t.Fatal("sleepCtx must report an aborted sleep on a done context")
	}
	if time.Since(start) > time.Second {
		t.Error("canceled context must abort the sleep immediately")
	}
}

// Config parsing: defaults apply on unset/garbage, explicit seconds win.
func TestPreviewGateConfigFromEnv(t *testing.T) {
	t.Setenv(previewCmdEnv, "scripts/dev preview")
	t.Setenv(previewTeardownCmdEnv, "scripts/dev preview-down")
	t.Setenv(previewProvisionTimeoutSecsEnv, "45")
	t.Setenv(previewReadyTimeoutSecsEnv, "garbage")
	gcfg := previewGateConfigFromEnv()
	if gcfg.provisionCmd != "scripts/dev preview" || gcfg.teardownCmd != "scripts/dev preview-down" {
		t.Errorf("commands = %q / %q", gcfg.provisionCmd, gcfg.teardownCmd)
	}
	if gcfg.provisionTimeout != 45*time.Second {
		t.Errorf("provisionTimeout = %s, want 45s", gcfg.provisionTimeout)
	}
	if gcfg.readyTimeout != defaultPreviewReadyTimeout {
		t.Errorf("readyTimeout = %s, want default on unparsable value", gcfg.readyTimeout)
	}
}
