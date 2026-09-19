package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/acceptenv"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// TestForgeWritesDenied_Table pins the pure gate predicate (E72.13 / #3500):
// only the exact value "deny" from either source denies, and the reported
// source names which one(s) fired.
func TestForgeWritesDenied_Table(t *testing.T) {
	cases := []struct {
		name    string
		environ []string
		backend string
		denied  bool
		source  string
	}{
		{"absent", []string{"PATH=/usr/bin"}, "", false, ""},
		{"env deny", []string{"PATH=/usr/bin", "FISHHAWK_FORGE_WRITES=deny"}, "", true, "env"},
		{"backend deny", []string{"PATH=/usr/bin"}, "deny", true, "backend"},
		{"both", []string{"FISHHAWK_FORGE_WRITES=deny"}, "deny", true, "env+backend"},
		{"env other value", []string{"FISHHAWK_FORGE_WRITES=allow"}, "", false, ""},
		{"env mixed case", []string{"FISHHAWK_FORGE_WRITES=Deny"}, "", false, ""},
		{"env empty", []string{"FISHHAWK_FORGE_WRITES="}, "", false, ""},
		{"backend other value", nil, "allow", false, ""},
		{"lower-case var name is not the var", []string{"fishhawk_forge_writes=deny"}, "", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			denied, source := forgeWritesDenied(c.environ, c.backend)
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
