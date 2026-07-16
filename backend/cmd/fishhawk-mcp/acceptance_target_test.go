package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
