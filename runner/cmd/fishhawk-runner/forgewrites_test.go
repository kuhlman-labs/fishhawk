package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/acceptenv"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// TestForgeWritesDenied_Table pins the pure gate predicate (E72.13 / #3500,
// E72.16 / #3510): only the exact value "deny" from the env or backend
// source denies, a true healthz dev_mode denies, and the reported source
// names which one(s) fired in the fixed order env, backend, healthz.
func TestForgeWritesDenied_Table(t *testing.T) {
	cases := []struct {
		name    string
		environ []string
		backend string
		healthz bool
		denied  bool
		source  string
	}{
		{"absent", []string{"PATH=/usr/bin"}, "", false, false, ""},
		{"env deny", []string{"PATH=/usr/bin", "FISHHAWK_FORGE_WRITES=deny"}, "", false, true, "env"},
		{"backend deny", []string{"PATH=/usr/bin"}, "deny", false, true, "backend"},
		{"both", []string{"FISHHAWK_FORGE_WRITES=deny"}, "deny", false, true, "env+backend"},
		{"env other value", []string{"FISHHAWK_FORGE_WRITES=allow"}, "", false, false, ""},
		{"env mixed case", []string{"FISHHAWK_FORGE_WRITES=Deny"}, "", false, false, ""},
		{"env empty", []string{"FISHHAWK_FORGE_WRITES="}, "", false, false, ""},
		{"backend other value", nil, "allow", false, false, ""},
		{"lower-case var name is not the var", []string{"fishhawk_forge_writes=deny"}, "", false, false, ""},
		// #3510: the /healthz dev_mode source.
		{"healthz alone", []string{"PATH=/usr/bin"}, "", true, true, "healthz"},
		{"env+healthz", []string{"FISHHAWK_FORGE_WRITES=deny"}, "", true, true, "env+healthz"},
		{"backend+healthz", nil, "deny", true, true, "backend+healthz"},
		{"all three", []string{"FISHHAWK_FORGE_WRITES=deny"}, "deny", true, true, "env+backend+healthz"},
		{"healthz false with nothing else", []string{"PATH=/usr/bin"}, "", false, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			denied, source := forgeWritesDenied(c.environ, c.backend, c.healthz)
			if denied != c.denied || source != c.source {
				t.Fatalf("forgeWritesDenied = (%v, %q), want (%v, %q)", denied, source, c.denied, c.source)
			}
		})
	}
}

// forgeWriteEvents are the runner-side forge-write log events the gate must
// make unreachable: none may appear in a refused run's stderr.
var forgeWriteEvents = []string{
	`"event":"pull_request_opened"`,
	`"event":"implement_child_pushed"`,
	`"event":"implement_fixup_pushed"`,
	`"event":"acceptance_scenarios_pushed"`,
}

// assertForgeWritesRefused asserts the shared refusal shape: exitFailure, the
// agent NEVER invoked, the category-C runner_failed line naming the source,
// and none of the forge-write events logged.
func assertForgeWritesRefused(t *testing.T, got int, invoker *fakeInvoker, stderr string, wantSource string) {
	t.Helper()
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr)
	}
	if invoker.gotInv != nil {
		t.Fatalf("agent was invoked; the forge-writes gate must refuse pre-spawn:\n%s", stderr)
	}
	for _, want := range []string{`"event":"runner_failed"`, `"reason":"forge_writes_denied"`, `"category":"C"`, `"source":"` + wantSource + `"`} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %s:\n%s", want, stderr)
		}
	}
	for _, ev := range forgeWriteEvents {
		if strings.Contains(stderr, ev) {
			t.Errorf("stderr carries forge-write event %s after a forge-writes refusal:\n%s", ev, stderr)
		}
	}
}

// unsetForgeWrites clears FISHHAWK_FORGE_WRITES for the test (t.Setenv
// registers the restore; Unsetenv makes it genuinely absent, not empty).
func unsetForgeWrites(t *testing.T) {
	t.Helper()
	t.Setenv(acceptenv.ForgeWritesVar, "")
	_ = os.Unsetenv(acceptenv.ForgeWritesVar)
}

// TestRun_ForgeWritesDenied_EnvSource_FailsPreSpawn: the fetched prompt
// carries NO forge_writes field, but the process env carries the deny — the
// posture an acceptance-spawned descendant runner inherits from acceptenv.
func TestRun_ForgeWritesDenied_EnvSource_FailsPreSpawn(t *testing.T) {
	t.Setenv(acceptenv.ForgeWritesVar, acceptenv.ForgeWritesDeny)
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "Hello agent, do the thing.",
		PromptHash: "deadbeef",
	}
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
	}, &stderr)
	assertForgeWritesRefused(t, got, invoker, stderr.String(), "env")
	if fu.gotPromptArgs == nil {
		t.Error("FetchPrompt not called: the gate must fire AFTER the fetch, so the backend signal is honored too")
	}
}

// TestRun_ForgeWritesDenied_BackendSource_FailsPreSpawn: env unset, the
// fetched prompt response carries forge_writes:"deny" (a dev-mode backend).
func TestRun_ForgeWritesDenied_BackendSource_FailsPreSpawn(t *testing.T) {
	unsetForgeWrites(t)
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:     "22222222-3333-4444-5555-666666666666",
		StageType:   "implement",
		Prompt:      "Hello agent, do the thing.",
		PromptHash:  "deadbeef",
		ForgeWrites: "deny",
	}
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
	}, &stderr)
	assertForgeWritesRefused(t, got, invoker, stderr.String(), "backend")
}

// TestRun_ForgeWritesDenied_PromptFilePath_EnvSource: the --prompt-file path
// fetches nothing (no backend signal), so the env deny is the only source —
// and it still refuses pre-spawn on the default push-and-open-PR posture
// (no --no-pr).
func TestRun_ForgeWritesDenied_PromptFilePath_EnvSource(t *testing.T) {
	t.Setenv(acceptenv.ForgeWritesVar, acceptenv.ForgeWritesDeny)
	promptPath := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("local prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--prompt-file", promptPath,
	}, &stderr)
	assertForgeWritesRefused(t, got, invoker, stderr.String(), "env")
	if fu.gotPromptArgs != nil {
		t.Error("FetchPrompt called on the --prompt-file path")
	}
}

// TestRun_ForgeWritesAbsent_Proceeds is the positive control: with neither
// deny source present the same dispatch spawns the agent.
func TestRun_ForgeWritesAbsent_Proceeds(t *testing.T) {
	unsetForgeWrites(t)
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "Hello agent, do the thing.",
		PromptHash: "deadbeef",
	}
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if invoker.gotInv == nil {
		t.Fatal("agent not invoked with no deny source present")
	}
	if strings.Contains(stderr.String(), `"reason":"forge_writes_denied"`) {
		t.Errorf("forge_writes_denied logged with no deny source:\n%s", stderr.String())
	}
}

// TestRun_ForgeWritesDenied_RealFetchPath_BackendDeny (approval condition 2,
// E72.13 / #3500) drives the REAL fetch path end to end: the production
// upload.Client against an httptest backend whose /v0/stages/{id}/prompt
// answers a prompt response carrying forge_writes:"deny" (the bytes a
// dev-mode fishhawkd emits). run() must decode the field through
// upload.FetchedPrompt.ForgeWrites, thread it out of fetchPromptToFile and
// refuse pre-spawn — agent never invoked, runner_failed forge_writes_denied,
// source backend. This closes the gap a fakeUploader-only test leaves: the
// JSON tag, the decode and the return-tuple threading are all on the path.
func TestRun_ForgeWritesDenied_RealFetchPath_BackendDeny(t *testing.T) {
	unsetForgeWrites(t)
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)

	runID := "11111111-2222-3333-4444-555555555555"
	stageID := "22222222-3333-4444-5555-666666666666"
	pub, priv, kerr := ed25519.GenerateKey(rand.Reader)
	if kerr != nil {
		t.Fatal(kerr)
	}
	var promptServed bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/signing-key", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"run_id":      runID,
			"private_key": base64.StdEncoding.EncodeToString(priv),
			"public_key":  base64.StdEncoding.EncodeToString(pub),
			"issued_at":   time.Now().UTC(),
			"expires_at":  time.Now().UTC().Add(time.Hour),
		})
	})
	mux.HandleFunc("GET /v0/stages/{stage_id}/prompt", func(w http.ResponseWriter, _ *http.Request) {
		promptServed = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"stage_id":     stageID,
			"stage_type":   "implement",
			"prompt":       "Hello agent, do the thing.",
			"prompt_hash":  "deadbeef",
			"forge_writes": "deny",
		})
	})
	// Any other route (mcp-token, trace, pull-request …) is a forge/backend
	// write the refused runner must never need; answer 404 and record it.
	var unexpected []string
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		unexpected = append(unexpected, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	origClient := newUploadClient
	newUploadClient = func(baseURL string) uploadClient { return upload.New(baseURL) }
	t.Cleanup(func() { newUploadClient = origClient })

	var stderr strings.Builder
	got := run([]string{
		"--run-id", runID,
		"--backend-url", srv.URL,
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", stageID,
		"--fetch-prompt",
	}, &stderr)
	assertForgeWritesRefused(t, got, invoker, stderr.String(), "backend")
	if !promptServed {
		t.Fatal("the real prompt endpoint was never hit; this test must drive the production fetch path")
	}
	for _, u := range unexpected {
		if strings.Contains(u, "/pull-request") || strings.Contains(u, "/trace") {
			t.Errorf("refused runner reached backend write route %s", u)
		}
	}
}

// closedLoopbackAddr listens on an ephemeral loopback port, closes it, and
// returns the http:// URL of the now-closed port: a deterministic
// connection-refused backend for the unreachable branch.
func closedLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

// healthzBackend is an httptest backend whose GET /healthz writes body with
// status and counts hits; every other route answers 404 and is recorded.
func healthzBackend(t *testing.T, status int, body string) (srv *httptest.Server, hits *atomic.Int32, paths *[]string) {
	t.Helper()
	hits = new(atomic.Int32)
	paths = new([]string)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		*paths = append(*paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, hits, paths
}

// TestProbeBackendDevModeHTTP_Table pins the /healthz classifier (E72.16 /
// #3510, approval condition 1) against real httptest servers: only a 200
// whose COMPLETE body is a single JSON object with a boolean-true dev_mode
// member denies. Everything else is devMode:false with the named fail-open
// outcome — a production daemon omits the key (omitempty), so absence,
// non-200, non-JSON, non-object (null / array / scalar), trailing garbage,
// a non-boolean dev_mode, an over-cap body and an unreachable host all
// proceed.
func TestProbeBackendDevModeHTTP_Table(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		devMode bool
		outcome string
	}{
		{"dev_mode true", 200, `{"status":"ok","dev_mode":true}`, true, probeOutcomeDevMode},
		{"key absent", 200, `{"status":"ok"}`, false, probeOutcomeProduction},
		{"dev_mode false", 200, `{"status":"ok","dev_mode":false}`, false, probeOutcomeProduction},
		{"503", 503, `{"status":"degraded","dev_mode":true}`, false, probeOutcomeUnverifiable},
		{"non-JSON", 200, `<html>ok</html>`, false, probeOutcomeUnverifiable},
		{"null", 200, `null`, false, probeOutcomeUnverifiable},
		{"array", 200, `[]`, false, probeOutcomeUnverifiable},
		{"scalar", 200, `"ok"`, false, probeOutcomeUnverifiable},
		{"trailing garbage", 200, `{"dev_mode":true} trailing`, false, probeOutcomeUnverifiable},
		{"string not bool", 200, `{"dev_mode":"true"}`, false, probeOutcomeProduction},
		{"over-cap body", 200, `{"dev_mode":true,"pad":"` + strings.Repeat("x", forgeWritesHealthzBodyCap) + `"}`, false, probeOutcomeUnverifiable},
		{"empty body", 200, ``, false, probeOutcomeUnverifiable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, hits, _ := healthzBackend(t, c.status, c.body)
			got := probeBackendDevModeHTTP(context.Background(), srv.URL)
			if got.devMode != c.devMode || got.outcome != c.outcome {
				t.Fatalf("probe = (%v, %q, %q), want (%v, %q)", got.devMode, got.outcome, got.detail, c.devMode, c.outcome)
			}
			if got.detail == "" {
				t.Error("probe detail is empty; every outcome must name why")
			}
			if n := hits.Load(); n != 1 {
				t.Errorf("/healthz hit %d times, want 1", n)
			}
		})
	}

	t.Run("closed loopback port", func(t *testing.T) {
		got := probeBackendDevModeHTTP(context.Background(), closedLoopbackAddr(t))
		if got.devMode || got.outcome != probeOutcomeUnreachable {
			t.Fatalf("probe = (%v, %q, %q), want (false, %q)", got.devMode, got.outcome, got.detail, probeOutcomeUnreachable)
		}
	})

	t.Run("trailing-slash backend URL hits exactly /healthz", func(t *testing.T) {
		srv, _, paths := healthzBackend(t, 200, `{"status":"ok","dev_mode":true}`)
		got := probeBackendDevModeHTTP(context.Background(), srv.URL+"/")
		if !got.devMode {
			t.Fatalf("probe = (%v, %q, %q), want dev_mode", got.devMode, got.outcome, got.detail)
		}
		if len(*paths) != 1 || (*paths)[0] != "/healthz" {
			t.Fatalf("paths hit = %q, want exactly [/healthz]", *paths)
		}
	})
}

// promptFileRunArgs builds the --prompt-file launch used by the #3510 e2e
// tests: a local prompt, the default push-and-open-PR posture (no --no-pr).
func promptFileRunArgs(t *testing.T, backendURL string) []string {
	t.Helper()
	promptPath := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("local prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", backendURL,
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--prompt-file", promptPath,
	}
}

// TestRun_ForgeWritesDenied_PromptFilePath_HealthzDevMode_FailsPreSpawn is
// #3510's done-means 3 end to end: a --prompt-file launch (no prompt fetch,
// so no backend forge_writes signal) from an env that SCRUBBED
// FISHHAWK_FORGE_WRITES, against a backend whose /healthz advertises
// dev_mode:true, through the REAL HTTP probe. Refused pre-spawn, source
// healthz, FetchPrompt never called, /healthz hit exactly once, and the
// probe event logged with outcome dev_mode.
func TestRun_ForgeWritesDenied_PromptFilePath_HealthzDevMode_FailsPreSpawn(t *testing.T) {
	unsetForgeWrites(t)
	withRealDevModeProbe(t)
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	withFakeUploader(t, fu)
	srv, hits, _ := healthzBackend(t, 200, `{"status":"ok","dev_mode":true}`)

	var stderr strings.Builder
	got := run(promptFileRunArgs(t, srv.URL), &stderr)
	assertForgeWritesRefused(t, got, invoker, stderr.String(), "healthz")
	if fu.gotPromptArgs != nil {
		t.Error("FetchPrompt called on the --prompt-file path")
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("/healthz hit %d times, want exactly 1 (one probe feeds both gate sites)", n)
	}
	for _, want := range []string{`"event":"forge_writes_healthz_probe"`, `"outcome":"dev_mode"`} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %s:\n%s", want, stderr.String())
		}
	}
}

// TestRun_ForgeWritesAbsent_PromptFilePath_HealthzProduction_Proceeds: the
// same launch against a backend whose /healthz omits dev_mode (a production
// daemon) proceeds — agent invoked, no forge_writes_denied line.
func TestRun_ForgeWritesAbsent_PromptFilePath_HealthzProduction_Proceeds(t *testing.T) {
	unsetForgeWrites(t)
	withRealDevModeProbe(t)
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	withFakeUploader(t, fu)
	srv, _, _ := healthzBackend(t, 200, `{"status":"ok"}`)

	var stderr strings.Builder
	got := run(promptFileRunArgs(t, srv.URL), &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if invoker.gotInv == nil {
		t.Fatal("agent not invoked against a production /healthz")
	}
	if strings.Contains(stderr.String(), `"reason":"forge_writes_denied"`) {
		t.Errorf("forge_writes_denied logged against a production /healthz:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"outcome":"production"`) {
		t.Errorf("stderr missing the probe event with outcome production:\n%s", stderr.String())
	}
}

// TestRun_ForgeWritesAbsent_PromptFilePath_HealthzUnreachable_Proceeds pins
// the DELIBERATE fail-open: a backend that refuses the connection is not a
// dev-mode backend as far as the runner can tell, so the launch proceeds and
// the probe event names outcome unreachable so the fail-open is visible.
func TestRun_ForgeWritesAbsent_PromptFilePath_HealthzUnreachable_Proceeds(t *testing.T) {
	unsetForgeWrites(t)
	withRealDevModeProbe(t)
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run(promptFileRunArgs(t, closedLoopbackAddr(t)), &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if invoker.gotInv == nil {
		t.Fatal("agent not invoked with an unreachable /healthz; the probe must fail OPEN")
	}
	if strings.Contains(stderr.String(), `"reason":"forge_writes_denied"`) {
		t.Errorf("forge_writes_denied logged with an unreachable /healthz:\n%s", stderr.String())
	}
	for _, want := range []string{`"event":"forge_writes_healthz_probe"`, `"outcome":"unreachable"`} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %s:\n%s", want, stderr.String())
		}
	}
}

// TestRun_ForgeWritesDenied_RealFetchPath_HealthzDevMode (approval condition
// 3) is the fetch-path pin: the production upload.Client against an httptest
// backend serving the signing key and a prompt WITHOUT forge_writes, whose
// /healthz advertises dev_mode:true. The probe fires on the fetch path too
// and the fetch-block site honours it: refused, source healthz, with the
// prompt actually served.
func TestRun_ForgeWritesDenied_RealFetchPath_HealthzDevMode(t *testing.T) {
	unsetForgeWrites(t)
	withRealDevModeProbe(t)
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)

	runID := "11111111-2222-3333-4444-555555555555"
	stageID := "22222222-3333-4444-5555-666666666666"
	pub, priv, kerr := ed25519.GenerateKey(rand.Reader)
	if kerr != nil {
		t.Fatal(kerr)
	}
	var promptServed bool
	var healthzHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		healthzHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","dev_mode":true}`))
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/signing-key", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"run_id":      runID,
			"private_key": base64.StdEncoding.EncodeToString(priv),
			"public_key":  base64.StdEncoding.EncodeToString(pub),
			"issued_at":   time.Now().UTC(),
			"expires_at":  time.Now().UTC().Add(time.Hour),
		})
	})
	mux.HandleFunc("GET /v0/stages/{stage_id}/prompt", func(w http.ResponseWriter, _ *http.Request) {
		promptServed = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"stage_id":    stageID,
			"stage_type":  "implement",
			"prompt":      "Hello agent, do the thing.",
			"prompt_hash": "deadbeef",
		})
	})
	var unexpected []string
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		unexpected = append(unexpected, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	origClient := newUploadClient
	newUploadClient = func(baseURL string) uploadClient { return upload.New(baseURL) }
	t.Cleanup(func() { newUploadClient = origClient })

	var stderr strings.Builder
	got := run([]string{
		"--run-id", runID,
		"--backend-url", srv.URL,
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", stageID,
		"--fetch-prompt",
	}, &stderr)
	assertForgeWritesRefused(t, got, invoker, stderr.String(), "healthz")
	if !promptServed {
		t.Fatal("the real prompt endpoint was never hit; this test must drive the production fetch path")
	}
	if n := healthzHits.Load(); n != 1 {
		t.Errorf("/healthz hit %d times, want exactly 1 on the fetch path", n)
	}
	for _, u := range unexpected {
		if strings.Contains(u, "/pull-request") || strings.Contains(u, "/trace") {
			t.Errorf("refused runner reached backend write route %s", u)
		}
	}
}

// TestRun_ForgeWritesDenied_PromptFilePath_EnvAndHealthz pins the combined
// source string end to end: env deny plus a dev-mode /healthz reports
// source env+healthz (fixed order).
func TestRun_ForgeWritesDenied_PromptFilePath_EnvAndHealthz(t *testing.T) {
	t.Setenv(acceptenv.ForgeWritesVar, acceptenv.ForgeWritesDeny)
	withRealDevModeProbe(t)
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	withFakeUploader(t, fu)
	srv, _, _ := healthzBackend(t, 200, `{"status":"ok","dev_mode":true}`)

	var stderr strings.Builder
	got := run(promptFileRunArgs(t, srv.URL), &stderr)
	assertForgeWritesRefused(t, got, invoker, stderr.String(), "env+healthz")
}
