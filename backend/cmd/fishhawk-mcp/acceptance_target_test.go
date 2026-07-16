package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// hostOf strips the scheme from an httptest server URL, yielding the host:port
// the probe (which builds scheme://host/healthz itself) expects.
func hostOf(serverURL string) string {
	return strings.TrimPrefix(serverURL, "http://")
}

// healthzServer stands up an httptest server whose /healthz returns the given
// status + body, so a probe leg can drive one classification branch.
func healthzServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

const probeExpectedSHA = "abc1234def567890abc1234def567890abc12345"

// TestProbeAcceptanceTargetIdentity_Classification pins the verb-side probe's
// four-way classification against a live /healthz per branch — the mirror of the
// runner's previewprobe.go table (#1953). A semantic divergence from the runner
// fails here.
func TestProbeAcceptanceTargetIdentity_Classification(t *testing.T) {
	t.Run("verified: git_sha is a >=7-char prefix of expected", func(t *testing.T) {
		ts := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def"}`)
		res := probeAcceptanceTargetIdentity(context.Background(), hostOf(ts.URL), probeExpectedSHA)
		if res.outcome != acceptanceProbeVerified {
			t.Errorf("outcome = %s, want verified (%s)", res.outcome, res.detail)
		}
	})
	t.Run("stale: git_sha is a different build", func(t *testing.T) {
		ts := healthzServer(t, http.StatusOK, `{"git_sha":"deadbeefcafe"}`)
		res := probeAcceptanceTargetIdentity(context.Background(), hostOf(ts.URL), probeExpectedSHA)
		if res.outcome != acceptanceProbeStale {
			t.Errorf("outcome = %s, want stale", res.outcome)
		}
	})
	t.Run("stale: dirty-suffixed git_sha fails closed even on a prefix match", func(t *testing.T) {
		ts := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def-dirty"}`)
		res := probeAcceptanceTargetIdentity(context.Background(), hostOf(ts.URL), probeExpectedSHA)
		if res.outcome != acceptanceProbeStale {
			t.Errorf("outcome = %s, want stale (dirty build is not the committed merge candidate)", res.outcome)
		}
	})
	t.Run("unverifiable: non-200 status", func(t *testing.T) {
		ts := healthzServer(t, http.StatusServiceUnavailable, `{"git_sha":"abc1234def"}`)
		res := probeAcceptanceTargetIdentity(context.Background(), hostOf(ts.URL), probeExpectedSHA)
		if res.outcome != acceptanceProbeUnverifiable {
			t.Errorf("outcome = %s, want unverifiable (non-200)", res.outcome)
		}
	})
	t.Run("unverifiable: non-JSON body", func(t *testing.T) {
		ts := healthzServer(t, http.StatusOK, `not json`)
		res := probeAcceptanceTargetIdentity(context.Background(), hostOf(ts.URL), probeExpectedSHA)
		if res.outcome != acceptanceProbeUnverifiable {
			t.Errorf("outcome = %s, want unverifiable (non-JSON)", res.outcome)
		}
	})
	t.Run("unverifiable: missing git_sha", func(t *testing.T) {
		ts := healthzServer(t, http.StatusOK, `{"status":"ok"}`)
		res := probeAcceptanceTargetIdentity(context.Background(), hostOf(ts.URL), probeExpectedSHA)
		if res.outcome != acceptanceProbeUnverifiable {
			t.Errorf("outcome = %s, want unverifiable (no git_sha)", res.outcome)
		}
	})
	t.Run("unverifiable: git_sha unknown", func(t *testing.T) {
		ts := healthzServer(t, http.StatusOK, `{"git_sha":"unknown"}`)
		res := probeAcceptanceTargetIdentity(context.Background(), hostOf(ts.URL), probeExpectedSHA)
		if res.outcome != acceptanceProbeUnverifiable {
			t.Errorf("outcome = %s, want unverifiable (unknown git_sha)", res.outcome)
		}
	})
	t.Run("unverifiable: git_sha too short (<7)", func(t *testing.T) {
		ts := healthzServer(t, http.StatusOK, `{"git_sha":"abc12"}`)
		res := probeAcceptanceTargetIdentity(context.Background(), hostOf(ts.URL), probeExpectedSHA)
		if res.outcome != acceptanceProbeUnverifiable {
			t.Errorf("outcome = %s, want unverifiable (too short)", res.outcome)
		}
	})
	t.Run("unverifiable: empty expected SHA (older backend)", func(t *testing.T) {
		res := probeAcceptanceTargetIdentity(context.Background(), "localhost:65535", "")
		if res.outcome != acceptanceProbeUnverifiable {
			t.Errorf("outcome = %s, want unverifiable (no expected SHA short-circuits before dialing)", res.outcome)
		}
	})
	t.Run("unverifiable: a /healthz redirect is refused, not followed", func(t *testing.T) {
		// A target whose /healthz 3xx-redirects to another path must NOT be
		// followed — a compromised or spec-declared target could otherwise point
		// the probe at an arbitrary internal/external URL. The 3xx is a non-200
		// so it classifies unverifiable, and the redirect destination is never hit.
		var redirectFollowed bool
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				http.Redirect(w, r, "/exfil", http.StatusFound)
				return
			}
			redirectFollowed = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"git_sha":"abc1234def"}`))
		}))
		t.Cleanup(ts.Close)
		res := probeAcceptanceTargetIdentity(context.Background(), hostOf(ts.URL), probeExpectedSHA)
		if res.outcome != acceptanceProbeUnverifiable {
			t.Errorf("outcome = %s, want unverifiable (redirect refused, non-200)", res.outcome)
		}
		if redirectFollowed {
			t.Error("probe followed the /healthz redirect to /exfil; redirects must be refused")
		}
	})
	t.Run("unreachable: no listener", func(t *testing.T) {
		// A server we immediately close: nothing listens, both schemes fail.
		ts := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def"}`)
		host := hostOf(ts.URL)
		ts.Close()
		res := probeAcceptanceTargetIdentity(context.Background(), host, probeExpectedSHA)
		if res.outcome != acceptanceProbeUnreachable {
			t.Errorf("outcome = %s, want unreachable (no listener)", res.outcome)
		}
	})
}

// TestAcceptanceProbeSchemeOrder pins the scheme-attempt order — the
// security-relevant half of the probe: it decides whether a NON-loopback target
// is tried over https before http. It mirrors the runner's probeSchemeOrder
// (runner/cmd/fishhawk-runner/previewprobe.go — a separate Go module, not
// importable), so a divergence from the runner's byte-for-byte logic must be
// caught here. The loopback/IP-literal cases (http-first, the dev loop) are the
// only order the httptest-driven probe tests exercise; this covers the https-first
// branch and the bracketed-IPv6 trim that those never reach.
func TestAcceptanceProbeSchemeOrder(t *testing.T) {
	cases := []struct {
		host string
		want string // comma-joined scheme order
	}{
		{"localhost:8080", "http,https"},           // loopback name + port
		{"localhost", "http,https"},                // loopback name, no port
		{"127.0.0.1:9000", "http,https"},           // IPv4 literal + port
		{"127.0.0.1", "http,https"},                // IPv4 literal, no port
		{"[::1]:8080", "http,https"},               // bracketed IPv6 + port (SplitHostPort strips)
		{"::1", "http,https"},                      // bare IPv6 literal -> ParseIP
		{"[2001:db8::1]", "http,https"},            // bracketed IPv6, NO port -> bracket trim
		{"preview.example.com:8080", "https,http"}, // non-loopback host -> https first
		{"preview.example.com", "https,http"},      // non-loopback host, no port
		{"example.com", "https,http"},              // non-loopback, https first
	}
	for _, c := range cases {
		if got := strings.Join(acceptanceProbeSchemeOrder(c.host), ","); got != c.want {
			t.Errorf("acceptanceProbeSchemeOrder(%q) = %s, want %s", c.host, got, c.want)
		}
	}
}

// TestAcceptanceSleepCtx pins acceptanceSleepCtx's two outcomes: an early return
// on a cancelled context (the retry-loop cancellation path) and a full elapse.
func TestAcceptanceSleepCtx(t *testing.T) {
	t.Run("cancelled ctx -> false without sleeping", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if acceptanceSleepCtx(ctx, time.Hour) {
			t.Error("acceptanceSleepCtx = true on a cancelled ctx, want false (early return)")
		}
	})
	t.Run("full sleep -> true", func(t *testing.T) {
		if !acceptanceSleepCtx(context.Background(), time.Millisecond) {
			t.Error("acceptanceSleepCtx = false, want true when the sleep elapses")
		}
	})
}

// TestAwaitAcceptanceTargetReady_RetryAndCancel exercises the unreachable-retry
// loop (attempt > 1) and the ctx-cancellation break the gate-branch tests skip
// by shrinking attempts to 1. Both use a closed listener so every probe leg is
// unreachable.
func TestAwaitAcceptanceTargetReady_RetryAndCancel(t *testing.T) {
	origAttempts := acceptanceQuickProbeAttempts
	origInterval := acceptanceQuickPollInterval
	acceptanceQuickProbeAttempts = 3
	acceptanceQuickPollInterval = time.Millisecond
	t.Cleanup(func() {
		acceptanceQuickProbeAttempts = origAttempts
		acceptanceQuickPollInterval = origInterval
	})

	ts := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def"}`)
	host := hostOf(ts.URL)
	ts.Close() // nothing listens: every probe is unreachable

	t.Run("retries the unreachable outcome to exhaustion", func(t *testing.T) {
		res := awaitAcceptanceTargetReady(context.Background(), host, probeExpectedSHA)
		if res.outcome != acceptanceProbeUnreachable {
			t.Errorf("outcome = %s, want unreachable after retries", res.outcome)
		}
	})
	t.Run("cancelled ctx breaks the retry loop early", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res := awaitAcceptanceTargetReady(ctx, host, probeExpectedSHA)
		if res.outcome != acceptanceProbeUnreachable {
			t.Errorf("outcome = %s, want unreachable (cancellation returns the last probe)", res.outcome)
		}
	})
}

// TestCheckAcceptanceTarget_GateBranches pins every proceed/refuse branch of the
// verb-side gate (#1953). The quick-retry knobs are shrunk so the unreachable
// leg does not sleep.
func TestCheckAcceptanceTarget_GateBranches(t *testing.T) {
	// Shrink the retry budget so the unreachable refusal returns immediately.
	origAttempts := acceptanceQuickProbeAttempts
	acceptanceQuickProbeAttempts = 1
	t.Cleanup(func() { acceptanceQuickProbeAttempts = origAttempts })

	newResolver := func(env map[string]string) *runResolver {
		return &runResolver{getenv: envFuncFromMap(env)}
	}

	t.Run("nil admission -> proceed", func(t *testing.T) {
		refusal, warn := newResolver(nil).checkAcceptanceTarget(context.Background(), nil)
		if refusal != nil || warn != "" {
			t.Errorf("got (%+v, %q), want proceed silently", refusal, warn)
		}
	})
	t.Run("!NeedsTarget -> proceed", func(t *testing.T) {
		adm := &AcceptanceAdmissionResult{NeedsTarget: false, TargetHosts: []string{"localhost:8090"}}
		refusal, warn := newResolver(nil).checkAcceptanceTarget(context.Background(), adm)
		if refusal != nil || warn != "" {
			t.Errorf("got (%+v, %q), want proceed silently", refusal, warn)
		}
	})
	t.Run("no declared hosts -> proceed", func(t *testing.T) {
		adm := &AcceptanceAdmissionResult{NeedsTarget: true, TargetHosts: nil, ExpectedHeadSHA: probeExpectedSHA}
		refusal, warn := newResolver(nil).checkAcceptanceTarget(context.Background(), adm)
		if refusal != nil || warn != "" {
			t.Errorf("got (%+v, %q), want proceed silently", refusal, warn)
		}
	})
	t.Run("FISHHAWK_ACCEPTANCE_PREVIEW_CMD set -> proceed with note", func(t *testing.T) {
		adm := &AcceptanceAdmissionResult{NeedsTarget: true, TargetHosts: []string{"localhost:8090"}, ExpectedHeadSHA: probeExpectedSHA}
		env := map[string]string{acceptancePreviewCmdEnv: "scripts/dev preview"}
		refusal, warn := newResolver(env).checkAcceptanceTarget(context.Background(), adm)
		if refusal != nil {
			t.Fatalf("refusal = %+v, want proceed (the spawned runner provisions)", refusal)
		}
		if !strings.Contains(warn, acceptancePreviewCmdEnv) {
			t.Errorf("warning = %q, want a note naming %s", warn, acceptancePreviewCmdEnv)
		}
	})
	t.Run("empty expected head SHA -> proceed with warning", func(t *testing.T) {
		adm := &AcceptanceAdmissionResult{NeedsTarget: true, TargetHosts: []string{"localhost:8090"}, ExpectedHeadSHA: ""}
		refusal, warn := newResolver(nil).checkAcceptanceTarget(context.Background(), adm)
		if refusal != nil {
			t.Fatalf("refusal = %+v, want proceed (never hard-fail a missing expectation)", refusal)
		}
		if warn == "" {
			t.Error("warning = empty, want a proceed-unverified warning")
		}
	})
	t.Run("probe verified -> proceed", func(t *testing.T) {
		ts := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def"}`)
		adm := &AcceptanceAdmissionResult{NeedsTarget: true, TargetHosts: []string{hostOf(ts.URL)}, ExpectedHeadSHA: probeExpectedSHA}
		refusal, warn := newResolver(nil).checkAcceptanceTarget(context.Background(), adm)
		if refusal != nil || warn != "" {
			t.Errorf("got (%+v, %q), want proceed silently on verified", refusal, warn)
		}
	})
	t.Run("probe unverifiable -> proceed with warning", func(t *testing.T) {
		ts := healthzServer(t, http.StatusOK, `{"status":"ok"}`) // no git_sha
		adm := &AcceptanceAdmissionResult{NeedsTarget: true, TargetHosts: []string{hostOf(ts.URL)}, ExpectedHeadSHA: probeExpectedSHA}
		refusal, warn := newResolver(nil).checkAcceptanceTarget(context.Background(), adm)
		if refusal != nil {
			t.Fatalf("refusal = %+v, want proceed with a warning on unverifiable", refusal)
		}
		if warn == "" {
			t.Error("warning = empty, want the probe detail as a warning")
		}
	})
	t.Run("probe stale -> refuse", func(t *testing.T) {
		ts := healthzServer(t, http.StatusOK, `{"git_sha":"deadbeefcafe"}`)
		adm := &AcceptanceAdmissionResult{NeedsTarget: true, TargetHosts: []string{hostOf(ts.URL)}, ExpectedHeadSHA: probeExpectedSHA}
		refusal, warn := newResolver(nil).checkAcceptanceTarget(context.Background(), adm)
		if refusal == nil {
			t.Fatalf("refusal = nil, want a stale refusal (warn=%q)", warn)
		}
		if refusal.TargetHost != hostOf(ts.URL) || refusal.ExpectedHeadSHA != probeExpectedSHA {
			t.Errorf("refusal = %+v, want host+sha populated", refusal)
		}
		if !strings.Contains(refusal.Remediation, probeExpectedSHA) {
			t.Errorf("remediation = %q, want it to name the head SHA", refusal.Remediation)
		}
	})
	t.Run("probe unreachable -> refuse", func(t *testing.T) {
		ts := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def"}`)
		host := hostOf(ts.URL)
		ts.Close()
		adm := &AcceptanceAdmissionResult{NeedsTarget: true, TargetHosts: []string{host}, ExpectedHeadSHA: probeExpectedSHA}
		refusal, _ := newResolver(nil).checkAcceptanceTarget(context.Background(), adm)
		if refusal == nil {
			t.Fatal("refusal = nil, want an unreachable refusal")
		}
		if refusal.TargetHost != host {
			t.Errorf("refusal.TargetHost = %q, want %q", refusal.TargetHost, host)
		}
	})
}
